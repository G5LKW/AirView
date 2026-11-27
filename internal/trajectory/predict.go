package trajectory

import "math"

// turn plans used by the predictor
type plan uint8

const (
	planDecay   plan = iota // turn rate decays exponentially to straight flight
	planCapture             // turn continues until the selected track, then straight
	planHold                // racetrack holding pattern continues
	planGround              // surface movement: no flying off, taxi speed decays
)

func (p plan) String() string {
	switch p {
	case planCapture:
		return "capture"
	case planHold:
		return "hold"
	case planGround:
		return "ground"
	}
	return "decay"
}

// params are the derived numeric parameters shared by all tracks.
type params struct {
	imm         immParams
	qVert       float64
	qWPred      float64
	qMan        float64
	actTau      float64
	turnTau     float64
	turnHold    float64
	accelTau    float64
	levelTau    float64
	ceilFt      float64
	maxSpeed    float64
	maxCoast    float64
	coastAfter  float64
	reinitGap   float64
	oosMax      float64
	recenterM   float64
	sigADSB     float64
	sigMLAT     float64
	sigTISB     float64
	sigOther    float64
	sigVel      float64
	sigAlt      float64
	resetAfter  int
	snapEvery   float64
	scoreMaxSig float64
	manW        float64 // coast turn hypothesis, rad/s
	manAmax     float64 // its lateral acceleration limit (bank), m/s^2
	manAmin     float64 // its minimum lateral acceleration, m/s^2
}

func newParams(c Config) params {
	return params{
		imm: immParams{
			qCV: c.QCV, qCT: c.QCT, qW: c.QOmega,
			tauCV: c.TauCVs, tauCT: c.TauCTs, gate: c.GateChi2,
		},
		qVert:       c.QVert,
		qWPred:      c.QOmegaPred,
		qMan:        c.QManeuver,
		actTau:      c.ActivityTau,
		turnTau:     c.TurnTau,
		turnHold:    c.TurnHoldSec,
		accelTau:    c.AccelTau,
		levelTau:    c.LevelOffTau,
		ceilFt:      c.CeilingFt,
		maxSpeed:    c.MaxSpeedMS,
		maxCoast:    c.MaxCoast.Seconds(),
		coastAfter:  c.CoastAfter.Seconds(),
		reinitGap:   c.ReinitGap.Seconds(),
		oosMax:      c.OOSMaxLag.Seconds(),
		recenterM:   c.RecenterKm * 1000,
		sigADSB:     c.SigmaADSBM,
		sigMLAT:     c.SigmaMLATM,
		sigTISB:     c.SigmaTISBM,
		sigOther:    c.SigmaOtherM,
		sigVel:      c.SigmaVelMS,
		sigAlt:      c.SigmaAltFt,
		resetAfter:  c.ResetAfterRejects,
		snapEvery:   c.SnapshotEvery.Seconds(),
		scoreMaxSig: c.ScoreMaxSigmaM,
		manW:        c.CoastTurnDps * deg2rad,
		manAmax:     G0 * math.Tan(c.CoastBankDeg*deg2rad),
		manAmin:     c.CoastMinAccel,
	}
}

// pstate holds everything needed to predict forward from one instant. It is
// a plain value: copying it is how snapshots and predictions work.
type pstate struct {
	fr   frame
	t    float64 // time of x (Unix s)
	x    vec5
	P    mat5
	muCT float64

	accel  float64 // along-track air-speed acceleration, m/s^2 (decays with accelTau)
	wx, wy float64 // wind (m/s, blowing towards)
	act    float64 // manoeuvre activity 0..1 (scales prediction noise)
	since  float64 // seconds propagated since the last filter update (manoeuvre floor)
	velGap float64 // seconds between the last accepted position and the last filter update

	// vertical (closed form from the base state + elapsed time)
	vOK      bool
	h, hd    float64       // ft, ft/s at base
	pv       [2][2]float64 // covariance at base
	vElapsed float64       // seconds since the vertical base
	selAlt   float64
	selOK    bool
	altHold  bool
	floorFt  float64

	plan     plan
	tgtTrk   float64 // capture target track / hold leg course (deg true)
	holdW    float64 // hold turn rate (rad/s, signed)
	holdLeg  float64 // hold straight-leg seconds
	legLeft  float64 // remaining straight time in the current hold leg
	turnKeep float64 // remaining constant-turn time before decay starts
}

const subStep = 5.0 // seconds

// propagate advances ps by dt seconds (dt >= 0). withCov also propagates P.
func (ps *pstate) propagate(dt float64, p *params, withCov bool) {
	if !(dt > 0) {
		return
	}
	for dt > 1e-9 {
		h := math.Min(dt, subStep)
		ps.stepH(h, p, withCov)
		dt -= h
	}
}

// stepH advances one sub-step, splitting it where the turn plan changes.
func (ps *pstate) stepH(h float64, p *params, withCov bool) {
	ps.vElapsed += h
	ps.since += h
	ps.t += h
	for guard := 0; h > 1e-9 && guard < 6; guard++ {
		w := ps.x[iw]
		switch ps.plan {
		case planGround:
			dec := math.Exp(-h / 8)
			ps.move(h, w*math.Exp(-h/16), math.Exp(-h/16), math.Exp(-h/8), dec, p, withCov)
			h = 0
		case planCapture, planHold:
			aw := math.Abs(w)
			if aw < 0.2*deg2rad {
				if ps.plan == planHold && ps.holdW != 0 {
					// straight hold leg
					if ps.legLeft > h {
						ps.legLeft -= h
						ps.move(h, 0, 0, 1, 1, p, withCov)
						h = 0
					} else {
						t1 := math.Max(ps.legLeft, 0)
						ps.move(t1, 0, 0, 1, 1, p, withCov)
						h -= t1
						ps.x[iw] = ps.holdW
						ps.tgtTrk = normDeg(ps.tgtTrk + 180)
						ps.legLeft = 0
					}
					continue
				}
				ps.plan = planDecay
				continue
			}
			trk := trackOf(ps.x[ivx], ps.x[ivy])
			var rem float64
			if w > 0 {
				rem = normDeg(ps.tgtTrk - trk)
			} else {
				rem = normDeg(trk - ps.tgtTrk)
			}
			if ps.plan == planHold {
				// next leg course: the target or its reciprocal, whichever comes
				// first (ignoring the course the turn just left)
				if rem < 2 {
					rem += 360
				}
				r2 := math.Mod(rem+180, 360)
				if r2 < 2 {
					r2 += 360
				}
				if r2 < rem {
					rem = r2
					ps.tgtTrk = normDeg(ps.tgtTrk + 180)
				}
			}
			t1 := rem * deg2rad / aw
			if t1 >= h {
				ps.move(h, w, 1, 1, 1, p, withCov)
				h = 0
			} else {
				ps.move(t1, w, 1, 1, 1, p, withCov)
				h -= t1
				ps.x[iw] = 0
				if ps.plan == planHold {
					ps.legLeft = ps.holdLeg
				} else {
					ps.plan = planDecay
					ps.turnKeep = 0
				}
			}
		default: // planDecay
			if ps.turnKeep > 0 {
				t1 := math.Min(h, ps.turnKeep)
				ps.move(t1, w, 1, 1, 1, p, withCov)
				ps.turnKeep -= t1
				h -= t1
				continue
			}
			tau := p.turnTau
			ps.move(h, w*math.Exp(-h/(2*tau)), math.Exp(-h/(2*tau)), math.Exp(-h/tau), 1, p, withCov)
			h = 0
		}
	}
}

// move applies motion over h with turn rate w (dw = dw/dx[iw]), then the
// along-track speed change. speedMul < 1 decays speed (ground taxi).
func (ps *pstate) move(h, w, dw, wdec, speedMul float64, p *params, withCov bool) {
	if !(h > 0) {
		return
	}
	wx, wy := ps.wx, ps.wy
	if ps.plan == planGround {
		wx, wy = 0, 0
	}
	if withCov {
		var F mat5
		keepW := ps.x[iw]
		motion(&ps.x, h, w, wx, wy, dw, &F)
		F[iw][iw] = wdec
		ps.x[iw] = keepW * wdec
		ps.P = fpft(&F, &ps.P)
		q := (1-ps.muCT)*p.imm.qCV + ps.muCT*p.imm.qCT + ps.act*p.qMan
		addQ(&ps.P, h, q, p.qWPred+ps.act*p.imm.qW)
		// along-track acceleration uncertainty
		if ps.accel != 0 {
			v := math.Hypot(ps.x[ivx], ps.x[ivy])
			if v > 1 {
				ux, uy := ps.x[ivx]/v, ps.x[ivy]/v
				sa := 0.3 * math.Abs(ps.accel) * h
				ps.P[ivx][ivx] += sa * sa * ux * ux
				ps.P[ivy][ivy] += sa * sa * uy * uy
				ps.P[ivx][ivy] += sa * sa * ux * uy
				ps.P[ivy][ivx] += sa * sa * ux * uy
			}
		}
		sanitize(&ps.P)
	} else {
		keepW := ps.x[iw]
		motion(&ps.x, h, w, wx, wy, 0, nil)
		ps.x[iw] = keepW * wdec
	}
	// speed changes act on the air vector (ground vector minus wind)
	ax, ay := ps.x[ivx]-wx, ps.x[ivy]-wy
	v := math.Hypot(ax, ay)
	if v < 1e-6 {
		return
	}
	nv := v
	if speedMul != 1 {
		if v < 26 { // taxi (< ~50 kt): slow down to a stop
			nv = v * speedMul
		} else if ps.accel != 0 {
			nv = v + ps.accel*h
		}
	} else if ps.accel != 0 {
		tau := p.accelTau
		e := math.Exp(-h / tau)
		nv = v + ps.accel*tau*(1-e)
		ps.accel *= e
	}
	nv = clamp(nv, 0, p.maxSpeed)
	if nv != v {
		ux, uy := ax/v, ay/v
		dpos := 0.5 * (nv - v) * h
		ps.x[ix] += ux * dpos
		ps.x[iy] += uy * dpos
		ps.x[ivx] = wx + ax*nv/v
		ps.x[ivy] = wy + ay*nv/v
	}
}

// vertical returns altitude (ft), rate (ft/s) and 1-sigma (ft) at the current elapsed time.
func (ps *pstate) vertical(p *params) (alt, rate, sig float64) {
	if !ps.vOK {
		return 0, 0, 0
	}
	e := ps.vElapsed
	pv := ps.pv
	vv := pv[0][0] + 2*e*pv[0][1] + e*e*pv[1][1]
	if e > 0 {
		vv += p.qVert * e * e * e / 3
	}
	sig = math.Sqrt(math.Max(vv, 1))
	r := ps.hd
	if ps.plan == planGround || ps.altHold || math.Abs(r) < 1.5 {
		return ps.h, 0, sig
	}
	if e <= 0 {
		return ps.h + r*e, r, sig
	}
	var tgt float64
	sel := false
	if ps.selOK && (ps.selAlt-ps.h)*r > 0 && math.Abs(ps.selAlt-ps.h) > 50 {
		tgt, sel = ps.selAlt, true
	} else if r > 0 {
		tgt = math.Max(p.ceilFt, ps.h)
	} else {
		tgt = math.Min(ps.floorFt, ps.h)
	}
	tau := p.levelTau
	rem := math.Abs(tgt - ps.h)
	band := math.Abs(r) * tau
	tSw, remS := 0.0, rem
	if rem > band {
		tSw = (rem - band) / math.Abs(r)
		remS = band
	}
	if e <= tSw {
		return ps.h + r*e, r, sig
	}
	u := e - tSw
	ex := math.Exp(-u / tau)
	sg := 1.0
	if r < 0 {
		sg = -1
	}
	alt = tgt - sg*remS*ex
	rate = sg * remS / tau * ex
	if sel {
		// level-off at a known selected altitude is a strong constraint
		capSig := math.Sqrt(pv[0][0]+60*60) + remS*ex
		if capSig < sig {
			sig = capSig
		}
	}
	return alt, rate, sig
}

// ellipse returns the 1-sigma error ellipse of the 2x2 position covariance.
func ellipse(a, b, d float64) Ellipse {
	tr := (a + d) / 2
	df := math.Sqrt(math.Max(((a-d)/2)*((a-d)/2)+b*b, 0))
	l1 := math.Max(tr+df, 0)
	l2 := math.Max(tr-df, 0)
	th := 0.5 * math.Atan2(2*b, a-d) // from east, counter-clockwise
	or := math.Mod(90-th*rad2deg+360, 180)
	return Ellipse{SemiMajorM: fin(math.Sqrt(l1)), SemiMinorM: fin(math.Sqrt(l2)), OrientDeg: fin(or)}
}

// recenter moves the plane origin of a prediction state to its current position.
func (ps *pstate) recenter() {
	var f imm
	f.fr = ps.fr
	f.mu = [2]float64{1, 0}
	f.m[0] = modelState{ps.x, ps.P}
	f.m[1] = f.m[0]
	f.x, f.P = ps.x, ps.P
	f.recenter()
	ps.fr = f.fr
	ps.x, ps.P = f.m[0].x, f.m[0].P
	ps.x[iw] = f.m[0].x[iw]
}

// velIntegrationSigma is the 1-sigma velocity error (m/s per axis) that
// accumulates while only velocity reports keep a track alive.
const velIntegrationSigma = 1.5

// manoeuvreFloor returns the position covariance (xx, xy, yy; m^2) of the
// coast turn hypothesis after ps.since seconds without updates: the aircraft
// may have turned (either way) at the hypothesised rate from the moment the
// data stopped. Its displacement from the straight-line prediction is
// R*(theta - sin theta) along track and R*(1 - cos theta) across, with
// theta = w*t and R = v/w; this becomes the 1-sigma along/across spread. For
// small t it is a lateral acceleration a = v*w integrated twice (a*t^2/2).
// Intent-constrained plans (selected-heading capture, holding) get half.
func (ps *pstate) manoeuvreFloor(p *params) (float64, float64, float64) {
	t := ps.since
	vx, vy := ps.x[ivx], ps.x[ivy]
	v := math.Hypot(vx, vy)
	if !(t > 0) && !(ps.velGap > 0) || !(v > 5) || !(p.manW > 0) {
		return 0, 0, 0
	}
	t = math.Max(t, 0)
	acc := clamp(v*p.manW, p.manAmin, math.Max(p.manAmax, p.manAmin))
	w := acc / v
	th := w * t
	var along, across float64
	if th < 1e-3 {
		along = acc * t * t * th / 6
		across = acc * t * t / 2
	} else {
		r := v / w
		along = r * (th - math.Sin(th))
		across = r * (1 - math.Cos(th))
	}
	if ps.plan == planCapture || ps.plan == planHold {
		along, across = along/2, across/2
	}
	// Positions lost but velocities still received: the turn is observed,
	// but the position is dead-reckoned from velocity reports whose bias /
	// quantisation (~1.5 m/s, NACv 2) integrates up.
	if ps.velGap > 0 {
		dv := velIntegrationSigma * ps.velGap
		along, across = math.Hypot(along, dv), math.Hypot(across, dv)
	}
	ux, uy := vx/v, vy/v // along-track unit vector (x east, y north)
	la, lc := along*along, across*across
	// covariance with eigenvalues la (along u) and lc (across u)
	xx := la*ux*ux + lc*uy*uy
	yy := la*uy*uy + lc*ux*ux
	xy := (la - lc) * ux * uy
	return fin(xx), fin(xy), fin(yy)
}

// out is a prediction in output units.
type out struct {
	lat, lon         float64
	trk, gsKt, vrFpm float64
	turnDps          float64
	alt              float64
	sigH, sigV       float64
	sigF             float64 // horizontal 1-sigma of the filter alone (no turn hypothesis), m
	ell              Ellipse
}

func (ps *pstate) output(p *params) out {
	var o out
	o.lat, o.lon = ps.fr.toLatLon(ps.x[ix], ps.x[iy])
	o.trk = trackOf(ps.x[ivx], ps.x[ivy])
	o.gsKt = math.Hypot(ps.x[ivx], ps.x[ivy]) * msToKt
	o.turnDps = ps.x[iw] * rad2deg
	alt, rate, sv := ps.vertical(p)
	o.alt, o.vrFpm, o.sigV = alt, rate*60, sv
	a, b, d := ps.P[ix][ix], ps.P[ix][iy], ps.P[iy][iy]
	o.sigF = fin(math.Sqrt(math.Max(a+d, 0)))
	if ps.plan != planGround {
		fa, fb, fd := ps.manoeuvreFloor(p)
		a, b, d = a+fa, b+fb, d+fd
	}
	o.sigH = math.Sqrt(math.Max(a+d, 0))
	o.ell = ellipse(a, b, d)
	o.lat, o.lon = fin(o.lat), fin(o.lon)
	o.trk, o.gsKt, o.vrFpm, o.turnDps = fin(o.trk), fin(o.gsKt), fin(o.vrFpm), fin(o.turnDps)
	o.alt, o.sigH, o.sigV = fin(o.alt), fin(o.sigH), fin(o.sigV)
	return o
}
