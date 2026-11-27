package trajectory

import (
	"math"
	"sync/atomic"
)

const (
	nSnap = 15 // prediction snapshots kept for self-verification (75 s at 5 s)
	nHist = 96 // track-history samples for intent detection (8 min at 5 s)

	histEvery   = 5.0
	selAltFresh = 120.0
	selHdgFresh = 60.0
	navFresh    = 60.0
	tasFresh    = 10.0
	windFresh   = 900.0
	windRefresh = 60.0
)

// nacpEPU is the 95 % horizontal position bound (m) per NACp (DO-260B; see WIRING.md).
var nacpEPU = [12]float64{0, 18520, 7408, 3704, 1852, 926, 555.6, 185.2, 92.6, 30, 10, 3}

// nacvBound is the 95 % horizontal velocity bound (m/s) per NACv.
var nacvBound = [5]float64{0, 10, 3, 1, 0.3}

type counters struct {
	obs, positions, rejected, velRejected, altRejected atomic.Uint64
	outOfOrder, oooDropped, duplicates, resets, inits  atomic.Uint64
	overflow                                           atomic.Uint64
}

// env is shared, read-mostly context handed to tracks.
type env struct {
	p    params
	cnt  *counters
	sc   *scorer
	wind atomic.Pointer[windHolder]
}

type windHolder struct{ w WindProvider }

type snap struct {
	ok    bool
	done  uint8 // horizons scored or expired
	phase uint8
	ps    pstate
	// naive dead reckoning inputs (raw data at snapshot time)
	drLat, drLon, drT float64
	drGS, drTrk       float64 // m/s, deg
	drAltOK           bool
	drAlt, drAltT     float64 // ft, s
	drVR              float64 // ft/s
}

type histS struct {
	t        float64
	lat, lon float32
	trk      float32
	w        float32 // rad/s
}

// track is the per-aircraft state. All access is under the owning shard's lock.
type track struct {
	icao uint32
	hex  string

	f      imm
	hInit  bool
	vert   kf2
	spd    kf2
	src    Source
	ground bool

	lastSeen    float64
	lastAccPosT float64 // last accepted position time
	posCount    uint32
	rejects     int
	velRejects  int

	// raw (unfiltered) last values, used for dead reckoning and scoring
	rawLat, rawLon, rawPosT float64
	rawGS, rawTrk, rawVelT  float64 // m/s, deg
	rawAlt, rawAltT         float64
	rawVR, rawVRT           float64 // ft/s
	haveRawVel, haveRawAlt  bool
	haveRawVR               bool

	pendOK         bool
	pendT          float64
	pendVx, pendVy float64
	pendVar        float64

	// auxiliary intent data
	selAlt, selAltT float64
	selHdg, selHdgT float64
	magOff, magOffT float64
	magOffOK        bool
	tas, tasT       float64 // m/s
	nav             NavMode
	navT            float64
	windX, windY    float64
	windT           float64
	windOK          bool
	windQueryT      float64

	snaps     [nSnap]snap
	snapHead  int
	lastSnapT float64

	hist      [nHist]histS
	histHead  int
	histN     int
	lastHistT float64
	hold      holdInfo
	holdT     float64

	act, actT float64 // manoeuvre activity (EWMA of |turn rate|, 0..1)
}

func (tr *track) reset(icao uint32) {
	*tr = track{icao: icao, hex: hexICAO(icao)}
}

func hexICAO(icao uint32) string {
	const digits = "0123456789ABCDEF"
	var b [7]byte
	n := 0
	if icao&(1<<24) != 0 {
		b[0] = '~'
		n = 1
	}
	for i := 5; i >= 0; i-- {
		b[n+5-i] = digits[(icao>>(uint(i)*4))&0xF]
	}
	return string(b[:n+6])
}

func posSigma(o *Obs, p *params) float64 {
	if o.Has&HasAccuracy != 0 && o.AccuracyM > 0 && finite(o.AccuracyM) {
		return math.Max(o.AccuracyM, 3)
	}
	switch o.Src {
	case SrcMLAT:
		return p.sigMLAT
	case SrcTISB:
		return p.sigTISB
	case SrcADSB:
		s := p.sigADSB
		if o.Has&HasNACp != 0 && o.NACp >= 0 && o.NACp <= 11 {
			if o.NACp == 0 {
				return math.Max(s, 2*p.sigOther)
			}
			s = math.Max(s, nacpEPU[o.NACp]/2.45)
		}
		return s
	}
	return p.sigOther
}

func velSigma(o *Obs, src Source, p *params) float64 {
	if o.Has&HasNACv != 0 && o.NACv >= 0 && o.NACv <= 4 {
		if o.NACv == 0 {
			return 5
		}
		return math.Max(nacvBound[o.NACv]/2, 0.3)
	}
	switch src {
	case SrcADSB, SrcUnknown:
		return p.sigVel
	}
	return 4
}

func (tr *track) velMeas(o *Obs) (vx, vy float64) {
	v := o.GSKt * ktToMS
	s, c := math.Sincos(o.TrackDeg * deg2rad)
	return v * s, v * c
}

// ready reports whether the horizontal estimate is good enough to predict.
func (tr *track) ready() bool {
	if !tr.hInit {
		return false
	}
	P := &tr.f.P
	return P[ivx][ivx]+P[ivy][ivy] < 2*20*20
}

// observe ingests one observation (time t in Unix seconds).
func (tr *track) observe(t float64, o *Obs, e *env) {
	p := &e.p
	if t > tr.lastSeen {
		tr.lastSeen = t
	}
	tr.aux(t, o, e)

	hasPos := o.Has&HasPos != 0 && finite(o.Lat) && finite(o.Lon) && math.Abs(o.Lat) <= 90 && math.Abs(o.Lon) <= 180
	hasVel := o.Has&HasVel != 0 && finite(o.GSKt) && finite(o.TrackDeg) && o.GSKt >= 0 && o.GSKt < 1500
	// duplicates (same frame via two feeds / two decoders)
	if hasPos && tr.posCount > 0 && math.Abs(t-tr.rawPosT) < 1e-3 && o.Lat == tr.rawLat && o.Lon == tr.rawLon {
		hasPos = false
		e.cnt.duplicates.Add(1)
	}
	if hasVel && tr.haveRawVel && math.Abs(t-tr.rawVelT) < 1e-3 && o.GSKt*ktToMS == tr.rawGS && o.TrackDeg == tr.rawTrk {
		hasVel = false
		if !hasPos {
			e.cnt.duplicates.Add(1)
		}
	}
	// out-of-order handling for the horizontal channel
	shift := 0.0
	if tr.hInit && t < tr.f.t-1e-3 {
		shift = tr.f.t - t
		if shift > p.oosMax {
			if hasPos || hasVel {
				e.cnt.oooDropped.Add(1)
			}
			hasPos, hasVel = false, false
		} else if hasPos || hasVel {
			e.cnt.outOfOrder.Add(1)
		}
	}

	var wMeas, wVar float64
	hasW := false
	if o.Has&HasTrackRate != 0 && finite(o.TrackRateDps) && math.Abs(o.TrackRateDps) < 16 {
		wMeas = o.TrackRateDps * deg2rad
		s := 0.1*deg2rad + 0.1*math.Abs(wMeas)
		wVar, hasW = s*s, true
	} else if o.Has&HasRoll != 0 && finite(o.RollDeg) && math.Abs(o.RollDeg) < 60 && !tr.ground {
		v := 0.0
		if o.Has&HasTAS != 0 && o.TASKt > 0 {
			v = o.TASKt * ktToMS
		} else if tr.tas > 0 && t-tr.tasT < tasFresh {
			v = tr.tas
		} else if hasVel {
			v = o.GSKt * ktToMS
		} else if tr.hInit {
			v = math.Hypot(tr.f.x[ivx], tr.f.x[ivy])
		}
		if v > 40 {
			wMeas = G0 * math.Tan(o.RollDeg*deg2rad) / v
			s := 0.15*deg2rad + 0.15*math.Abs(wMeas)
			wVar, hasW = s*s, true
		}
	}
	if shift > p.oosMax {
		hasW = false
	}

	posAccepted := false
	var posSig float64
	if hasPos {
		e.cnt.positions.Add(1)
		posSig = posSigma(o, p)
		if !tr.hInit || t-tr.lastAccPosT > p.reinitGap {
			tr.initH(t, o, hasVel, posSig, e)
			posAccepted = true
		} else {
			tr.prepare(t+shift, p, e)
			zx, zy := tr.f.fr.toXY(o.Lat, o.Lon)
			r := posSig * posSig
			if shift > 0 {
				zx += tr.f.x[ivx] * shift
				zy += tr.f.x[ivy] * shift
				sv := math.Sqrt((tr.f.P[ivx][ivx]+tr.f.P[ivy][ivy])/2) + 1
				r += (sv*shift)*(sv*shift) + 1
			}
			R := [2][2]float64{{r, 0}, {0, r}}
			_, ok := tr.f.update(2, [2]int{ix, iy}, [2]float64{zx, zy}, R, p.imm.gate)
			if ok {
				posAccepted = true
				tr.rejects = 0
			} else {
				e.cnt.rejected.Add(1)
				tr.rejects++
			}
			if hasVel {
				tr.velUpdate(o, shift, p, e)
			}
			if hasW {
				tr.f.update(1, [2]int{iw}, [2]float64{wMeas}, [2][2]float64{{wVar}}, 10.8)
			}
			tr.f.finish()
			if !ok && tr.rejects >= p.resetAfter {
				e.cnt.resets.Add(1)
				tr.initH(t, o, hasVel, posSig, e)
				posAccepted = true
			}
		}
	} else if (hasVel || hasW) && tr.hInit && t-tr.lastAccPosT <= p.reinitGap {
		tr.prepare(t+shift, p, e)
		if hasVel {
			tr.velUpdate(o, shift, p, e)
		}
		if hasW {
			tr.f.update(1, [2]int{iw}, [2]float64{wMeas}, [2][2]float64{{wVar}}, 10.8)
		}
		tr.f.finish()
	} else if hasVel {
		vx, vy := tr.velMeas(o)
		s := velSigma(o, o.Src, p)
		tr.pendOK, tr.pendT, tr.pendVx, tr.pendVy, tr.pendVar = true, t, vx, vy, s*s
	}

	// raw values for dead reckoning
	if hasVel && t >= tr.rawVelT {
		tr.rawGS, tr.rawTrk, tr.rawVelT, tr.haveRawVel = o.GSKt*ktToMS, o.TrackDeg, t, true
	}

	// air-speed / along-track acceleration channel (ground speed when no wind is known)
	if hasVel && shift == 0 {
		vx, vy := tr.velMeas(o)
		wx, wy := tr.wind(t)
		v := math.Hypot(vx-wx, vy-wy)
		sv := velSigma(o, o.Src, p)
		if !tr.spd.ok || t-tr.spd.t > 30 {
			tr.spd.init(t, v, sv*sv, 0, 0.25)
		} else {
			tr.spd.predict(t, 0.004)
			if _, ok := tr.spd.update(0, v, sv*sv, 25); !ok {
				tr.spd.rej++
				if tr.spd.rej >= 3 {
					tr.spd.init(t, v, sv*sv, 0, 0.25)
				}
			} else {
				tr.spd.rej = 0
			}
		}
	}

	tr.vertical(t, o, p, e)
	if tr.hInit && shift == 0 {
		tr.updActivity(t, p)
	}

	if posAccepted {
		if t >= tr.rawPosT {
			tr.rawLat, tr.rawLon, tr.rawPosT = o.Lat, o.Lon, t
		}
		if t > tr.lastAccPosT {
			tr.lastAccPosT = t
		}
		tr.src = o.Src
		tr.posCount++
		if tr.posCount > 1 && posSig <= p.scoreMaxSig && shift == 0 && !tr.ground {
			tr.score(t, o, e)
		}
		if shift == 0 && tr.ready() {
			if t-tr.lastSnapT >= p.snapEvery {
				tr.takeSnapshot(t, p)
			}
			if t-tr.lastHistT >= histEvery {
				tr.addHist(t)
			}
		}
		tr.queryWind(t, e)
	}
}

// prepare re-centres if needed and runs the IMM prediction to t.
func (tr *track) prepare(t float64, p *params, e *env) {
	if math.Hypot(tr.f.x[ix], tr.f.x[iy]) > p.recenterM {
		tr.f.recenter()
	}
	wx, wy := tr.wind(t)
	tr.f.predict(t, &p.imm, wx, wy)
}

func (tr *track) wind(t float64) (wx, wy float64) {
	if tr.ground || !tr.windOK || t-tr.windT > windFresh {
		return 0, 0
	}
	return tr.windX, tr.windY
}

func (tr *track) velUpdate(o *Obs, shift float64, p *params, e *env) {
	vx, vy := tr.velMeas(o)
	s := velSigma(o, o.Src, p)
	r := s*s + (2*shift)*(2*shift)
	R := [2][2]float64{{r, 0}, {0, r}}
	if _, ok := tr.f.update(2, [2]int{ivx, ivy}, [2]float64{vx, vy}, R, p.imm.gate); !ok {
		e.cnt.velRejected.Add(1)
		tr.velRejects++
		if tr.velRejects >= p.resetAfter {
			// persistent disagreement: trust the broadcast velocity
			for j := 0; j < 2; j++ {
				m := &tr.f.m[j]
				m.x[ivx], m.x[ivy] = vx, vy
				m.P[ivx][ivx], m.P[ivy][ivy] = r, r
				for k := 0; k < nx; k++ {
					if k != ivx {
						m.P[ivx][k], m.P[k][ivx] = 0, 0
					}
					if k != ivy {
						m.P[ivy][k], m.P[k][ivy] = 0, 0
					}
				}
				m.P[ivx][ivx], m.P[ivy][ivy] = r, r
			}
			tr.velRejects = 0
		}
	} else {
		tr.velRejects = 0
	}
}

func (tr *track) initH(t float64, o *Obs, hasVel bool, sig float64, e *env) {
	e.cnt.inits.Add(1)
	fr := newFrame(o.Lat, o.Lon)
	var x vec5
	var P mat5
	P[ix][ix], P[iy][iy] = sig*sig, sig*sig
	vVar := 250.0 * 250.0
	switch {
	case hasVel:
		x[ivx], x[ivy] = tr.velMeas(o)
		s := velSigma(o, o.Src, &e.p)
		vVar = s * s
	case tr.pendOK && math.Abs(t-tr.pendT) < 10:
		x[ivx], x[ivy] = tr.pendVx, tr.pendVy
		vVar = tr.pendVar + 4*(t-tr.pendT)*(t-tr.pendT)
	case tr.hInit && tr.haveRawVel && t-tr.rawVelT < 10:
		s, c := math.Sincos(tr.rawTrk * deg2rad)
		x[ivx], x[ivy] = tr.rawGS*s, tr.rawGS*c
		vVar = 25
	}
	P[ivx][ivx], P[ivy][ivy] = vVar, vVar
	P[iw][iw] = math.Pow(1.5*deg2rad, 2)
	tr.f.init(fr, t, x, P)
	tr.hInit = true
	tr.rejects, tr.velRejects = 0, 0
	tr.pendOK = false
	tr.snaps = [nSnap]snap{}
	tr.lastSnapT = 0
	tr.histN, tr.histHead = 0, 0
	tr.hold = holdInfo{}
}

// aux records intent-related fields (only when newer than what we have).
func (tr *track) aux(t float64, o *Obs, e *env) {
	h := o.Has
	if h&HasGround != 0 {
		tr.ground = o.OnGround
	}
	if h&HasSelAlt != 0 && finite(o.SelAltFt) && t >= tr.selAltT && o.SelAltFt > -2000 && o.SelAltFt < 70000 {
		tr.selAlt, tr.selAltT = o.SelAltFt, t
	}
	if h&HasSelHeading != 0 && finite(o.SelHeadingDeg) && t >= tr.selHdgT {
		tr.selHdg, tr.selHdgT = normDeg(o.SelHeadingDeg), t
	}
	if h&HasNavModes != 0 && t >= tr.navT {
		tr.nav, tr.navT = o.NavModes, t
	}
	if h&HasTAS != 0 && finite(o.TASKt) && o.TASKt > 0 && t >= tr.tasT {
		tr.tas, tr.tasT = o.TASKt*ktToMS, t
	}
	if h&HasWind != 0 && finite(o.WindDirDeg) && finite(o.WindKt) && o.WindKt >= 0 && o.WindKt < 300 && t >= tr.windT {
		s, c := math.Sincos(o.WindDirDeg * deg2rad)
		w := o.WindKt * ktToMS
		tr.windX, tr.windY, tr.windT, tr.windOK = -w*s, -w*c, t, true
	}
	if h&HasMagHeading != 0 && finite(o.MagHeadingDeg) && tr.hInit {
		v := math.Hypot(tr.f.x[ivx], tr.f.x[ivy])
		if v > 50 {
			off := angDiff(trackOf(tr.f.x[ivx], tr.f.x[ivy]), o.MagHeadingDeg)
			if !tr.magOffOK || t-tr.magOffT > 300 {
				tr.magOff, tr.magOffOK = off, true
			} else {
				tr.magOff += 0.1 * angDiff(off, tr.magOff)
			}
			tr.magOffT = t
		}
	}
}

func (tr *track) queryWind(t float64, e *env) {
	if tr.windOK && t-tr.windT < windRefresh {
		return
	}
	if t-tr.windQueryT < windRefresh {
		return
	}
	wh := e.wind.Load()
	if wh == nil || wh.w == nil || !tr.vert.ok {
		return
	}
	tr.windQueryT = t
	dir, kt, ok := wh.w.WindAt(tr.rawLat, tr.rawLon, tr.vert.x[0])
	if ok && finite(dir) && finite(kt) && kt >= 0 && kt < 300 {
		s, c := math.Sincos(dir * deg2rad)
		w := kt * ktToMS
		tr.windX, tr.windY, tr.windT, tr.windOK = -w*s, -w*c, t, true
	}
}

func (tr *track) vertical(t float64, o *Obs, p *params, e *env) {
	hasAlt := o.Has&HasAlt != 0 && finite(o.AltFt) && o.AltFt > -2000 && o.AltFt < 80000
	hasVR := o.Has&HasVRate != 0 && finite(o.VRateFpm) && math.Abs(o.VRateFpm) < 30000
	if !hasAlt && !hasVR {
		if tr.ground && tr.vert.ok {
			tr.vert.x[1] = 0
		}
		return
	}
	sa := p.sigAlt
	if o.AltQ100 {
		sa *= 3
	}
	const svr = 1.5 // ft/s
	if hasAlt && t >= tr.rawAltT {
		tr.rawAlt, tr.rawAltT, tr.haveRawAlt = o.AltFt, t, true
	}
	if hasVR && t >= tr.rawVRT {
		tr.rawVR, tr.rawVRT, tr.haveRawVR = o.VRateFpm/60, t, true
	}
	k := &tr.vert
	if !k.ok || (hasAlt && t-k.t > 60) {
		if !hasAlt {
			return
		}
		r, rv := 0.0, 30.0*30.0
		if hasVR {
			r, rv = o.VRateFpm/60, svr*svr
		}
		k.init(t, o.AltFt, sa*sa, r, rv)
		return
	}
	tv := t
	lag := 0.0
	if t < k.t {
		lag = k.t - t
		if lag > p.oosMax {
			return
		}
		tv = k.t
	}
	k.predict(tv, p.qVert)
	if hasAlt {
		z := o.AltFt + k.x[1]*lag
		r := sa*sa + (3*lag)*(3*lag)
		if _, ok := k.update(0, z, r, 16); !ok {
			e.cnt.altRejected.Add(1)
			k.rej++
			if k.rej >= 3 {
				r0, rv := k.x[1], k.P[1][1]
				if hasVR {
					r0, rv = o.VRateFpm/60, svr*svr
				}
				k.init(t, o.AltFt, sa*sa, r0, rv)
				return
			}
		} else {
			k.rej = 0
		}
	}
	if hasVR {
		k.update(1, o.VRateFpm/60, svr*svr+lag*lag, 50)
	}
	if tr.ground {
		k.x[1] = 0
	}
	if !k.ok {
		k.init(t, tr.rawAlt, sa*sa, 0, 30*30)
	}
}

// pstate builds a prediction state from the current estimate.
func (tr *track) pstate(p *params) pstate {
	var ps pstate
	ps.fr = tr.f.fr
	ps.t = tr.f.t
	ps.x = tr.f.x
	ps.P = tr.f.P
	ps.muCT = tr.f.mu[mCT]
	t := tr.f.t
	ps.wx, ps.wy = tr.wind(t)
	ps.act = tr.act
	ps.velGap = math.Max(tr.f.t-tr.lastAccPosT, 0)
	// Without a wind estimate the ground speed of a turning aircraft oscillates
	// with the wind geometry; do not extrapolate that as acceleration.
	turning := math.Abs(tr.f.x[iw]) > 1*deg2rad && !tr.windOK
	if tr.spd.ok && t-tr.spd.t < 15 && !tr.ground && !turning {
		a, sa := tr.spd.x[1], math.Sqrt(tr.spd.P[1][1])
		if math.Abs(a) > 0.05 && math.Abs(a) > 2*sa {
			ps.accel = clamp(a, -3, 3)
		}
	}
	if tr.vert.ok {
		ps.vOK = true
		ps.h, ps.hd = tr.vert.x[0], tr.vert.x[1]
		ps.pv = tr.vert.P
		ps.vElapsed = t - tr.vert.t
		if tr.selAltT > 0 && tr.lastSeen-tr.selAltT < selAltFresh {
			ps.selAlt, ps.selOK = tr.selAlt, true
		}
		if tr.nav&NavAltHold != 0 && tr.lastSeen-tr.navT < navFresh {
			ps.altHold = true
		}
	}
	w := ps.x[iw]
	ps.turnKeep = p.turnHold
	switch {
	case tr.ground:
		ps.plan = planGround
	case tr.hold.ok && tr.lastSeen-tr.holdT < 60:
		ps.plan = planHold
		ps.holdW = tr.hold.w
		ps.holdLeg = tr.hold.legSec
		if math.Abs(w) >= 0.2*deg2rad && w*tr.hold.w > 0 {
			ps.tgtTrk = tr.hold.legCourse
		} else {
			ps.x[iw] = 0
			ps.tgtTrk = trackOf(ps.x[ivx], ps.x[ivy])
			ps.legLeft = math.Max(tr.hold.legSec-tr.hold.straightFor, 0)
		}
	case tr.selHdgT > 0 && tr.lastSeen-tr.selHdgT < selHdgFresh && math.Abs(w) > 0.3*deg2rad:
		sel := tr.selHdg
		if tr.magOffOK {
			sel = normDeg(sel + tr.magOff)
		}
		d := angDiff(sel, trackOf(ps.x[ivx], ps.x[ivy]))
		if d*w > 0 && math.Abs(d) > 1 {
			ps.plan = planCapture
			ps.tgtTrk = sel
		}
	}
	return ps
}

// takeSnapshot stores the current prediction state for later scoring.
func (tr *track) takeSnapshot(t float64, p *params) {
	if !tr.haveRawVel || t-tr.rawVelT > 10 {
		return
	}
	s := &tr.snaps[tr.snapHead]
	tr.snapHead = (tr.snapHead + 1) % nSnap
	*s = snap{ok: true}
	s.ps = tr.pstate(p)
	s.drLat, s.drLon, s.drT = tr.rawLat, tr.rawLon, tr.rawPosT
	s.drGS, s.drTrk = tr.rawGS, tr.rawTrk
	if tr.haveRawAlt && t-tr.rawAltT < 10 {
		s.drAltOK, s.drAlt, s.drAltT = true, tr.rawAlt, tr.rawAltT
		if tr.haveRawVR && t-tr.rawVRT < 10 {
			s.drVR = tr.rawVR
		}
	}
	s.phase = phaseOf(&s.ps)
	tr.lastSnapT = t
}

// phase classes for statistics
const (
	phAll = iota
	phStraight
	phTurn
	phVertical
	nPhase
)

var phaseNames = [nPhase]string{"all", "straight", "turn", "vertical"}

func phaseOf(ps *pstate) uint8 {
	if math.Abs(ps.x[iw]) > 0.5*deg2rad && ps.muCT > 0.5 {
		return phTurn
	}
	if ps.vOK && math.Abs(ps.hd) > 5 {
		return phVertical
	}
	return phStraight
}

func (tr *track) addHist(t float64) {
	lat, lon := tr.f.fr.toLatLon(tr.f.x[ix], tr.f.x[iy])
	h := &tr.hist[tr.histHead]
	*h = histS{t: t, lat: float32(lat), lon: float32(lon), trk: float32(trackOf(tr.f.x[ivx], tr.f.x[ivy])), w: float32(tr.f.x[iw])}
	tr.histHead = (tr.histHead + 1) % nHist
	if tr.histN < nHist {
		tr.histN++
	}
	tr.lastHistT = t
	if t-tr.holdT >= 15 || !tr.hold.ok {
		tr.hold = detectHold(tr)
		tr.holdT = t
	}
}

// histAt returns the i-th newest history sample (0 = newest).
func (tr *track) histAt(i int) *histS {
	return &tr.hist[(tr.histHead-1-i+2*nHist)%nHist]
}

// updActivity updates the manoeuvre activity EWMA from the filtered turn rate.
func (tr *track) updActivity(t float64, p *params) {
	if tr.actT == 0 {
		tr.act, tr.actT = 0.2, t
		return
	}
	dt := t - tr.actT
	if !(dt > 0) {
		return
	}
	tr.actT = t
	ind := clamp(math.Abs(tr.f.x[iw])*rad2deg/1.0, 0, 1)
	a := 1 - math.Exp(-dt/p.actTau)
	tr.act = clamp(fin(tr.act+a*(ind-tr.act)), 0, 1)
}
