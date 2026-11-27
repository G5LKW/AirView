package trajectory

import (
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"

	"airview/internal/geo"
)

// ---- truth simulator (WGS84 geodesic integration, independent of the filter) ----

type truthPt struct {
	t, lat, lon, alt, gs, trk, vr, turn float64
}

type simAC struct {
	t, lat, lon, alt float64
	hdg, tas         float64 // deg true, m/s (air vector)
	wx, wy           float64 // wind, m/s blowing to
	vr               float64 // ft/s
	turn             float64 // deg/s heading rate
	gs, trk          float64 // ground speed m/s, ground track deg
	ground           bool
	hist             []truthPt
}

type control struct {
	turnDps float64 // heading rate (+ right)
	accel   float64 // m/s^2 on TAS
	vrFpm   float64
}

func (s *simAC) ground3() {
	sh, ch := math.Sincos(s.hdg * deg2rad)
	vx := s.tas*sh + s.wx
	vy := s.tas*ch + s.wy
	s.gs = math.Hypot(vx, vy)
	s.trk = trackOf(vx, vy)
}

func (s *simAC) step(dt float64, c control) {
	s.turn = c.turnDps
	s.hdg = normDeg(s.hdg + c.turnDps*dt/2)
	s.tas = math.Max(0, s.tas+c.accel*dt)
	s.ground3()
	if s.gs > 0 {
		lat2, lon2, fb := geo.VincentyDirect(s.lat, s.lon, s.trk, s.gs*dt)
		// geodesic convergence rotates heading with the track
		s.hdg = normDeg(s.hdg + angDiff(fb, s.trk))
		s.lat, s.lon = lat2, lon2
	}
	s.hdg = normDeg(s.hdg + c.turnDps*dt/2)
	s.vr = c.vrFpm / 60
	if !s.ground {
		s.alt += s.vr * dt
	}
	s.t += dt
	s.ground3()
	s.hist = append(s.hist, truthPt{s.t, s.lat, s.lon, s.alt, s.gs, s.trk, s.vr * 60, s.turn})
}

func (s *simAC) at(t float64) truthPt {
	i := sort.Search(len(s.hist), func(i int) bool { return s.hist[i].t >= t })
	if i >= len(s.hist) {
		return s.hist[len(s.hist)-1]
	}
	return s.hist[i]
}

type scenario struct {
	lat, lon, alt, hdg, tasKt float64
	windFrom, windKt          float64
	ground                    bool
	dur                       float64
	ctl                       func(t float64, s *simAC) control
	posEvery, velEvery        float64
	posSigma, velSigma        float64
	src                       Source
	noVel                     bool
	gap                       func(t float64) bool
	extra                     func(t float64, s *simAC, o *Obs)
	seed                      int64
	cfg                       Config
}

type evalSample struct {
	t                float64
	imm, dr, cv      float64 // horizontal errors, m
	immV, drV        float64 // vertical errors, ft (NaN when n/a)
	turning, climb   bool
	sigH             float64
	immAlong, immXtk float64
}

type runResult struct {
	m       *Manager
	ac      *simAC
	samples []evalSample
}

const testICAO = 0x3C6586

// runScenario simulates, feeds the manager and evaluates H-second predictions
// every evalEvery seconds after warm seconds.
func runScenario(t testing.TB, sc scenario, H, evalEvery, warm float64) runResult {
	if sc.posEvery == 0 {
		sc.posEvery = 0.5
	}
	if sc.velEvery == 0 {
		sc.velEvery = 0.5
	}
	if sc.src == 0 {
		sc.src = SrcADSB
	}
	rng := rand.New(rand.NewSource(sc.seed + 1))
	m := NewManager(sc.cfg)
	s := &simAC{lat: sc.lat, lon: sc.lon, alt: sc.alt, hdg: sc.hdg, tas: sc.tasKt * ktToMS, ground: sc.ground}
	if sc.windKt > 0 {
		sn, cs := math.Sincos(sc.windFrom * deg2rad)
		s.wx, s.wy = -sc.windKt*ktToMS*sn, -sc.windKt*ktToMS*cs
	}
	s.ground3()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tm := func(x float64) time.Time { return base.Add(time.Duration(x * float64(time.Second))) }
	const dt = 0.05
	nextPos, nextVel := 0.0, sc.velEvery/2
	nextEval := warm
	var lastLat, lastLon, lastPosT, lastGS, lastTrk, lastAlt, lastVR float64
	haveVel, haveAlt := false, false
	type pending struct {
		at                 float64
		lat, lon, alt, vr0 float64
		gsx, trk           float64
	}
	var res runResult
	type evalJob struct {
		te                  float64
		pr                  Prediction
		drLat, drLon, drAlt float64
		drAltOK             bool
		cvLat, cvLon        float64
		turning, climb      bool
	}
	var jobs []evalJob
	for s.t < sc.dur {
		c := control{}
		if sc.ctl != nil {
			c = sc.ctl(s.t, s)
		}
		s.step(dt, c)
		now := s.t
		gap := sc.gap != nil && sc.gap(now)
		if now >= nextPos {
			nextPos += sc.posEvery
			if !gap {
				var o Obs
				o.T = tm(now)
				dn, de := rng.NormFloat64()*sc.posSigma, rng.NormFloat64()*sc.posSigma
				lat := s.lat + dn/111200
				lon := s.lon + de/(111200*math.Cos(s.lat*deg2rad))
				o.SetPos(lat, lon, sc.src)
				if sc.src == SrcADSB {
					o.Has |= HasNACp
					o.NACp = 9
				}
				if !s.ground {
					q := math.Round(s.alt/25) * 25
					o.SetAlt(q)
					lastAlt, haveAlt = q, true
				}
				if s.ground {
					o.Has |= HasGround
					o.OnGround = true
				}
				if sc.noVel {
					// MLAT-style: velocity derived poorly; none given
				}
				if sc.extra != nil {
					sc.extra(now, s, &o)
				}
				m.Observe(testICAO, &o)
				lastLat, lastLon, lastPosT = lat, lon, now
			}
		}
		if !sc.noVel && now >= nextVel {
			nextVel += sc.velEvery
			if !gap {
				var o Obs
				o.T = tm(now)
				sn, cs := math.Sincos(s.trk * deg2rad)
				vx := s.gs*sn + rng.NormFloat64()*sc.velSigma
				vy := s.gs*cs + rng.NormFloat64()*sc.velSigma
				gs := math.Round(math.Hypot(vx, vy)*msToKt*10) / 10
				trk := trackOf(vx, vy)
				o.SetVel(gs, trk)
				vr := math.Round(s.vr*60/64) * 64
				if !s.ground {
					o.SetVRate(vr)
				}
				if sc.src == SrcADSB {
					o.Has |= HasNACv
					o.NACv = 2
				} else {
					o.Src = sc.src // velocity from the MLAT solver / feed: no NACv
				}
				if s.ground {
					o.Has |= HasGround
					o.OnGround = true
				}
				m.Observe(testICAO, &o)
				lastGS, lastTrk, haveVel = gs*ktToMS, trk, true
				lastVR = vr / 60
			}
		}
		if now >= nextEval && now+H < sc.dur {
			nextEval += evalEvery
			if pr, ok := m.Predict(testICAO, tm(now+H)); ok && haveVel {
				j := evalJob{te: now, pr: pr}
				d := lastGS * (now + H - lastPosT)
				j.drLat, j.drLon, _ = geo.VincentyDirect(lastLat, lastLon, lastTrk, d)
				j.drAlt, j.drAltOK = lastAlt+lastVR*(now+H-lastPosT), haveAlt
				// straight line from the filtered state
				p0, _ := m.Predict(testICAO, tm(now))
				j.cvLat, j.cvLon, _ = geo.VincentyDirect(p0.Lat, p0.Lon, p0.TrackDeg, p0.GSKt*ktToMS*H)
				j.turning = math.Abs(s.turn) > 0.5
				j.climb = math.Abs(s.vr*60) > 300
				jobs = append(jobs, j)
			}
		}
	}
	for _, j := range jobs {
		tr := s.at(j.te + H)
		e := evalSample{t: j.te, turning: j.turning, climb: j.climb, immV: math.NaN(), drV: math.NaN()}
		e.imm = geo.VincentyKm(tr.lat, tr.lon, j.pr.Lat, j.pr.Lon) * 1000
		e.dr = geo.VincentyKm(tr.lat, tr.lon, j.drLat, j.drLon) * 1000
		e.cv = geo.VincentyKm(tr.lat, tr.lon, j.cvLat, j.cvLon) * 1000
		e.sigH = j.pr.SigmaHM
		if j.pr.AltOK {
			e.immV = j.pr.AltFt - tr.alt
		}
		if j.drAltOK {
			e.drV = j.drAlt - tr.alt
		}
		res.samples = append(res.samples, e)
	}
	res.m, res.ac = m, s
	return res
}

func median(x []float64) float64 {
	if len(x) == 0 {
		return math.NaN()
	}
	y := append([]float64(nil), x...)
	sort.Float64s(y)
	return quantSorted(y, 0.5)
}

func pct(x []float64, p float64) float64 {
	if len(x) == 0 {
		return math.NaN()
	}
	y := append([]float64(nil), x...)
	sort.Float64s(y)
	return quantSorted(y, p)
}

func collect(ss []evalSample, sel func(e evalSample) bool, f func(e evalSample) float64) []float64 {
	var out []float64
	for _, e := range ss {
		if sel == nil || sel(e) {
			v := f(e)
			if !math.IsNaN(v) {
				out = append(out, v)
			}
		}
	}
	return out
}

func absf(v float64) float64 { return math.Abs(v) }
