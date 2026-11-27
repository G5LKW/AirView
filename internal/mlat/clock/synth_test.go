package clock

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// A small exact-truth world for unit tests: stations with analytically
// known clocks, aircraft on straight tracks, propagation by the same
// geo.Atmosphere the filter uses (so model mismatch is zero and the
// filter's error must match its own covariance).

const synthOrigin = int64(1_780_000_000) * 1e9

type synthClock struct {
	cls      model.ClockClass
	gps      bool
	hz       float64
	counter0 float64 // counter value at local time 0 (s since origin)
	theta0   float64 // s
	phi      float64 // fractional
	psi      float64 // 1/s
	jitter   float64 // s
	betaNs   float64 // short-frame error, ns
	stepAt   float64 // silent step time (s); 0 = none
	stepNs   float64
	fault    func(t float64) float64 // extra offset (s), e.g. anchor fault
	epoch    uint32
}

// theta returns the true offset (local − true) at true time t, seconds.
func (c *synthClock) theta(t float64) float64 {
	th := c.theta0 + c.phi*t + 0.5*c.psi*t*t
	if c.stepAt > 0 && t >= c.stepAt {
		th += c.stepNs * 1e-9
	}
	if c.fault != nil {
		th += c.fault(t)
	}
	return th
}

func (c *synthClock) ticks(local float64) uint64 {
	if c.gps {
		return uint64(synthOrigin + int64(math.Round(local*1e9)))
	}
	return uint64(math.Round(c.counter0 + local*c.hz))
}

type synthStation struct {
	idx model.StationIdx
	pos geo.Vec3
	clk synthClock
}

type synthAc struct {
	key uint32
	p0  geo.Vec3
	v   geo.Vec3
}

type synthWorld struct {
	rng  *rand.Rand
	sts  []*synthStation
	acs  []synthAc
	atm  geo.Atmosphere
	posS float64 // reference position sigma (m)
	tick float64
	// badKey reports a position offset by badOff (a bad-NACp aircraft)
	badKey uint32
	badOff geo.Vec3
}

func newSynth(seed uint64) *synthWorld {
	return &synthWorld{rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), atm: geo.DefaultAtmosphere(), posS: 10}
}

func (w *synthWorld) addStation(cls model.ClockClass, lat, lon float64) *synthStation {
	r := w.rng
	s := &synthStation{idx: model.StationIdx(len(w.sts) + 2), pos: geo.ToECEF(lat, lon, 100+r.Float64()*200)}
	c := synthClock{cls: cls, hz: cls.NominalHz(), jitter: cls.PriorJitterNs() * 1e-9, epoch: 1}
	switch {
	case cls.GPS():
		c.gps = true
		c.theta0 = (50 + r.Float64()*250) * 1e-9
	default:
		c.counter0 = math.Floor(1e9 + r.Float64()*1e12)
		c.theta0 = (r.Float64() - 0.5) * 4e-3
		c.phi = (r.Float64()*2 - 1) * cls.PriorFreqSigma()
		if cls == model.ClockSDR12 || cls == model.ClockSBS20 {
			c.psi = (r.Float64()*2 - 1) * 2e-9 / 60
		}
		c.betaNs = (r.Float64()*2 - 1) * 300
	}
	s.clk = c
	w.sts = append(w.sts, s)
	return s
}

func (w *synthWorld) addAircraft(n int, lat, lon, radiusKm float64) {
	for i := 0; i < n; i++ {
		la := lat + (w.rng.Float64()*2-1)*radiusKm/111
		lo := lon + (w.rng.Float64()*2-1)*radiusKm/111/math.Cos(lat*math.Pi/180)
		p := geo.ToECEF(la, lo, 3000+w.rng.Float64()*9000)
		f := geo.NewFrameECEF(p)
		hd := w.rng.Float64() * 2 * math.Pi
		v := f.Unrotate(geo.Vec3{X: 230 * math.Sin(hd), Y: 230 * math.Cos(hd)})
		w.acs = append(w.acs, synthAc{key: uint32(0x400000 + i), p0: p, v: v})
	}
}

func (w *synthWorld) register(f *Filter) {
	for _, s := range w.sts {
		f.SetStation(s.idx, StationInfo{Class: s.clk.cls, Pos: s.pos, Located: true, NoStatus: true})
	}
}

// emit generates one transmission of a random aircraft at true time tau and
// feeds it to the filter.
func (w *synthWorld) emit(f *Filter, tau float64, short bool) bool {
	a := w.acs[w.rng.IntN(len(w.acs))]
	p := a.p0.Add(a.v.Scale(tau))
	kind := model.FrameModeSLong
	if short {
		kind = model.FrameModeSShort
	}
	var ms []Member
	for _, s := range w.sts {
		d := s.pos.Dist(p)
		if d > 400e3 || !geo.LineOfSight(s.pos, p, 0) {
			continue
		}
		arr := tau + w.atm.PropagationNs(s.pos, p)*1e-9
		ref := DefaultFrameRefNs(s.clk.cls, kind) * 1e-9
		stamp := arr + ref
		local := stamp + s.clk.theta(stamp) + w.rng.NormFloat64()*s.clk.jitter
		if short {
			local += s.clk.betaNs * 1e-9
		}
		ms = append(ms, Member{Station: s.idx, Epoch: s.clk.epoch, Ticks: s.clk.ticks(local)})
	}
	if len(ms) < 2 {
		return false
	}
	// reference position error
	e := geo.Vec3{X: w.rng.NormFloat64() * w.posS, Y: w.rng.NormFloat64() * w.posS, Z: w.rng.NormFloat64() * w.posS}
	if a.key == w.badKey {
		e = e.Add(w.badOff)
	}
	v := w.posS * w.posS
	host := synthOrigin + int64(tau*1e9) + int64(w.rng.NormFloat64()*2e6)
	return f.AddSync(&SyncObs{Key: a.key, Kind: kind, HostNs: host, Pos: p.Add(e), PosCov: [6]float64{v, 0, 0, v, 0, v}, Members: ms})
}

// run advances the world from t0 to t1 with rate transmissions per second
// and steps the filter every epoch (lag 0.6 s); cb is called after each
// Step.
func (w *synthWorld) run(f *Filter, t0, t1, rate, shortFrac float64, cb func(t float64)) {
	ep := f.cfg.EpochS
	next := t0 + ep
	for t := t0; t < t1; {
		t += w.rng.ExpFloat64() / rate
		w.emit(f, t, w.rng.Float64() < shortFrac)
		for t-0.6 >= next {
			f.Step(next)
			if cb != nil {
				cb(next)
			}
			next += ep
		}
	}
}

// errNs returns the error of the filter's network time for station s at
// true time t, in ns (estimated network time − true time). ok is false
// when the station is not published.
func (w *synthWorld) errNs(f *Filter, s *synthStation, t float64) (e, sig float64, ok bool) {
	sc := f.Snapshot().Station(s.idx)
	if sc == nil || sc.Hz == 0 {
		return 0, 0, false
	}
	local := t + s.clk.theta(t)
	var y float64
	if s.clk.gps {
		y = sc.BaseS + (float64(synthOrigin-int64(sc.BaseTicks))+local*1e9)/1e9
	} else {
		y = (s.clk.counter0+local*s.clk.hz-float64(sc.BaseTicks))/sc.Hz + sc.BaseS
	}
	tn, v := sc.NetFromLocal(y, false)
	return (tn - t) * 1e9, math.Sqrt(v), true
}

func TestSynthGPSAndSDR(t *testing.T) {
	w := newSynth(1)
	for i := 0; i < 4; i++ {
		w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*1.6), 9+1.2*math.Sin(float64(i)*1.6))
	}
	for i := 0; i < 10; i++ {
		cls := []model.ClockClass{model.ClockSDR12, model.ClockHW12, model.ClockSDR12HiRes, model.ClockSBS20}[i%4]
		w.addStation(cls, 50+(w.rng.Float64()-0.5)*2, 9+(w.rng.Float64()-0.5)*3)
	}
	w.addAircraft(60, 50, 9, 200)
	f, err := New(Config{OriginUnixNs: synthOrigin, ProbationS: 60, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.register(f)
	var nees, neesN float64
	w.run(f, 0, 400, 150, 0.3, func(tE float64) {
		if tE < 300 {
			return
		}
		// common mode: mean over anchors
		var cm float64
		var na int
		for _, s := range w.sts[:4] {
			if e, _, ok := w.errNs(f, s, tE); ok {
				cm += e
				na++
			}
		}
		if na == 0 {
			return
		}
		cm /= float64(na)
		for _, s := range w.sts {
			e, sg, ok := w.errNs(f, s, tE)
			if !ok || sg == 0 {
				continue
			}
			nees += (e - cm) * (e - cm) / (sg * sg)
			neesN++
		}
	})
	snap := f.Snapshot()
	anch := 0
	for _, s := range w.sts {
		sc := snap.Station(s.idx)
		if sc == nil {
			t.Fatalf("station %d not published", s.idx)
		}
		e, sg, _ := w.errNs(f, s, snap.T)
		t.Logf("st %2d %-12s anchor=%-13s synced=%v err=%8.1f ns sigma=%6.1f phi=%10.2f ppb (true %10.2f) beta=%7.1f (true %7.1f) q=%.2f j=%.2f out=%.1f%%",
			s.idx, s.clk.cls, sc.AnchorState, sc.Synced, e, sg, sc.Phi, s.clk.phi*1e9+s.clk.psi*1e9*snap.T, sc.Beta, s.clk.betaNs, sc.QScale, sc.JitterScale, sc.OutlierPct)
		if sc.Anchor {
			anch++
		}
		if !sc.Synced {
			t.Errorf("station %d not synced", s.idx)
		}
	}
	if anch != 4 {
		t.Errorf("anchors = %d, want 4", anch)
	}
	dumpEvents(t, f, 6)
	st := f.Stats()
	t.Logf("stats %+v; NEES/sample %.2f over %v", st, nees/neesN, neesN)
	if r := nees / neesN; r > 3 || r < 0.2 {
		t.Errorf("normalised error %.2f: covariance inconsistent", r)
	}
}

func dumpEvents(t *testing.T, f *Filter, idx model.StationIdx) {
	sc := f.Snapshot().Station(idx)
	if sc == nil {
		return
	}
	for _, e := range sc.Events {
		t.Logf("  st %d event t=%.1f %s %s", idx, e.T, e.Reason, e.Detail)
	}
}
