// Package ssrsim is the ground-truth simulator of the experimental bistatic
// SSR self-location (package airview/internal/mlat/ssr): rotating SSR
// interrogators on 1030 MHz, transponders answering on 1090 MHz with
// per-airframe reply-delay offsets and jitter, a receiver with its own
// free-running clock that hears the replies (with garbling and FRUIT) and
// the ADS-B positions of most aircraft. Everything is modelled from the
// published SSR/Mode S characteristics (ICAO Annex 10 Vol IV), not from
// any third-party implementation.
package ssrsim

import (
	"math"
	"math/rand"
	"sort"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/ssr"
)

// Mode is an interrogation mode.
type Mode uint8

const (
	ModeA Mode = iota // Mode A (identity) interrogation, P1-P3 8 µs
	ModeC             // Mode C (altitude) interrogation, P1-P3 21 µs
	// ModeSAllCall is a Mode A/C/S all-call (long P4): Mode S transponders
	// that are not locked out answer with DF11.
	ModeSAllCall
)

// Radar is a rotating SSR interrogator.
type Radar struct {
	Name         string
	Pos          geo.LLH
	PeriodS      float64   // antenna rotation period (4-12 s)
	Az0Deg       float64   // beam azimuth at t = 0
	BeamwidthDeg float64   // interrogation beam: aircraft within ±bw/2 answer
	PRFHz        float64   // mean interrogation rate
	Stagger      []float64 // PRI multipliers cycled (nil = constant PRF)
	Modes        []Mode    // interrogation mode cycle
	RangeM       float64   // instrumented range
	IID          uint8     // DF11 interrogator identifier
	LockoutFrac  float64   // fraction of Mode S aircraft locked out (no DF11)
}

// Scenario describes one simulation run.
type Scenario struct {
	Seed     int64
	Receiver geo.LLH
	RxRangeM float64 // receiver reception range (default 350 km)
	Radars   []Radar
	Aircraft int     // aircraft simultaneously in the region (default 150)
	RegionM  float64 // radius of the traffic region about the receiver (default 300 km)
	DurS     float64 // duration (default 600 s)

	ADSBFrac        float64 // aircraft with ADS-B positions (default 0.85)
	ConspicuityFrac float64 // aircraft squawking 7000 (default 0.1)
	PosSigmaM       float64 // ADS-B position noise 1σ horizontal (default 10 m)
	PosRateHz       float64 // ADS-B position rate (default 2)
	LatencyFrac     float64 // aircraft with an uncompensated position latency
	LatencyMaxS     float64 // its maximum (uniform 0..max)

	ACBiasUs    float64 // Mode A/C reply delay offset, uniform ±(default 0.4 µs; Annex 10 ±0.5)
	SBiasUs     float64 // Mode S reply delay offset, uniform ±(default 0.15 µs; Annex 10 ±0.25)
	ACJitterNs  float64 // Mode A/C reply jitter 1σ (default 50 ns)
	SJitterNs   float64 // Mode S reply jitter 1σ (default 25 ns)
	RxJitterACs float64 // receiver time-of-arrival 1σ for Mode A/C, ns (default 60)
	RxJitterSs  float64 // receiver time-of-arrival 1σ for Mode S, ns (default 40)
	ReplyProb   float64 // reply probability per interrogation (default 0.9)
	FruitRate   float64 // unrelated Mode A/C replies per second at the receiver (default 300)

	ClockOffsetS  float64 // receiver clock offset
	ClockDriftPPM float64 // receiver clock frequency error
	NoGarble      bool    // disable garbling (tests)
	Ideal         bool    // zero every noise and offset (tests)
}

func (s *Scenario) defaults() {
	d := func(p *float64, v float64) {
		if *p == 0 {
			*p = v
		}
	}
	d(&s.RxRangeM, 350e3)
	if s.Aircraft == 0 {
		s.Aircraft = 150
	}
	d(&s.RegionM, 300e3)
	d(&s.DurS, 600)
	d(&s.ADSBFrac, 0.85)
	d(&s.ConspicuityFrac, 0.1)
	d(&s.PosSigmaM, 10)
	d(&s.PosRateHz, 2)
	d(&s.ACBiasUs, 0.4)
	d(&s.SBiasUs, 0.15)
	d(&s.ACJitterNs, 50)
	d(&s.SJitterNs, 25)
	d(&s.RxJitterACs, 60)
	d(&s.RxJitterSs, 40)
	d(&s.ReplyProb, 0.9)
	d(&s.FruitRate, 300)
	if s.Ideal {
		s.PosSigmaM, s.LatencyFrac = 1e-9, 0
		s.ACBiasUs, s.SBiasUs = 1e-12, 1e-12
		s.ACJitterNs, s.SJitterNs, s.RxJitterACs, s.RxJitterSs = 1e-9, 1e-9, 1e-9, 1e-9
		s.FruitRate = 1e-9
		s.NoGarble = true
	}
	for i := range s.Radars {
		r := &s.Radars[i]
		d(&r.PeriodS, 5)
		d(&r.BeamwidthDeg, 2.4)
		d(&r.PRFHz, 250)
		d(&r.RangeM, 450e3)
		if len(r.Modes) == 0 {
			r.Modes = []Mode{ModeA, ModeC}
		}
	}
}

// Event is one receiver output: a position report or a reply.
type Event struct {
	T     float64 // local time
	IsPos bool
	ICAO  uint32
	Pos   ssr.Position
	Reply ssr.Reply
}

// Truth is the simulated ground truth.
type Truth struct {
	Receiver geo.Vec3
	Radars   []geo.Vec3
}

// Stats counts what the simulation produced.
type Stats struct {
	Interrogations int
	Replies        int // replies transmitted to interrogations
	Heard          int // heard by the receiver (before garbling)
	Garbled        int
	Fruit          int
	Emitted        int // replies emitted (after garbling)
	Positions      int
}

type aircraft struct {
	icao     uint32
	e0, n0   float64 // ENU at t0 (receiver frame)
	t0       float64
	ve, vn   float64
	alt0     float64 // m at t0
	vz       float64
	squawk   uint16
	adsb     bool
	latency  float64
	biasAC   float64 // s
	biasS    float64 // s
	lockout  uint64  // bit per radar
	posPhase float64
}

type sim struct {
	sc    Scenario
	rng   *rand.Rand
	f     geo.Frame
	rx    geo.Vec3
	atm   geo.Atmosphere
	ac    []*aircraft
	next  uint32
	used  map[uint16]bool
	st    Stats
	radF  []geo.Frame
	radP  []geo.Vec3
	nextI []float64 // next interrogation time per radar
	nI    []int
}

func (s *sim) pos(a *aircraft, t float64) (geo.Vec3, float64) {
	dt := t - a.t0
	e, n := a.e0+a.ve*dt, a.n0+a.vn*dt
	alt := a.alt0 + a.vz*dt
	l := geo.FromECEF(s.f.FromENU(geo.Vec3{X: e, Y: n}))
	return geo.ToECEF(l.Lat, l.Lon, alt), alt
}

func (s *sim) newSquawk() uint16 {
	for {
		c := uint16(s.rng.Intn(0o10000))
		if !s.used[c] && c != 0o7000 && c != 0o2000 && c != 0o1200 && c != 0 && c != 0o7777 && c != 0o1000 && c>>9 != 7 {
			s.used[c] = true
			return c
		}
	}
}

// spawn creates an aircraft; edge places it on the region boundary
// heading inwards.
func (s *sim) spawn(t float64, edge bool) *aircraft {
	sc := &s.sc
	s.next++
	a := &aircraft{icao: 0x400000 + s.next*7919%0x3fffff, t0: t}
	R := sc.RegionM
	var e, n float64
	if edge {
		th := s.rng.Float64() * 2 * math.Pi
		e, n = R*math.Sin(th), R*math.Cos(th)
	} else {
		r := R * math.Sqrt(s.rng.Float64())
		th := s.rng.Float64() * 2 * math.Pi
		e, n = r*math.Sin(th), r*math.Cos(th)
	}
	a.e0, a.n0 = e, n
	spd := 120 + 140*s.rng.Float64()
	// Head roughly across the region.
	hd := math.Atan2(-e, -n) + (s.rng.Float64()-0.5)*2.2
	if !edge && s.rng.Float64() < 0.5 {
		hd = s.rng.Float64() * 2 * math.Pi
	}
	a.ve, a.vn = spd*math.Sin(hd), spd*math.Cos(hd)
	if s.rng.Float64() < 0.2 {
		a.alt0 = (1500 + 6000*s.rng.Float64())
		a.vz = (s.rng.Float64() - 0.5) * 20
	} else {
		a.alt0 = float64(100+10*s.rng.Intn(31)) * 100 * 0.3048 // FL100-FL400
	}
	a.adsb = s.rng.Float64() < sc.ADSBFrac
	if s.rng.Float64() < sc.ConspicuityFrac {
		a.squawk = 0o7000
		a.alt0 = float64(15+5*s.rng.Intn(12)) * 100 * 0.3048
		a.vz = 0
	} else {
		a.squawk = s.newSquawk()
	}
	if s.rng.Float64() < sc.LatencyFrac {
		a.latency = s.rng.Float64() * sc.LatencyMaxS
	}
	a.biasAC = (2*s.rng.Float64() - 1) * sc.ACBiasUs * 1e-6
	a.biasS = (2*s.rng.Float64() - 1) * sc.SBiasUs * 1e-6
	for k := range sc.Radars {
		if s.rng.Float64() < sc.Radars[k].LockoutFrac {
			a.lockout |= 1 << uint(k)
		}
	}
	a.posPhase = s.rng.Float64() / sc.PosRateHz
	return a
}

func (s *sim) local(t float64) float64 {
	return t*(1+s.sc.ClockDriftPPM*1e-6) + s.sc.ClockOffsetS
}

type rec struct {
	t    float64
	dur  float64
	r    ssr.Reply
	garb bool
}

// Run simulates the scenario, calling emit for every event in (nearly)
// time order: events are sorted within 5 s chunks.
func Run(sc Scenario, emit func(Event)) (Truth, Stats) {
	sc.defaults()
	s := &sim{sc: sc, rng: rand.New(rand.NewSource(sc.Seed)), atm: geo.DefaultAtmosphere(), used: map[uint16]bool{}}
	s.f = geo.NewFrame(geo.LLH{Lat: sc.Receiver.Lat, Lon: sc.Receiver.Lon})
	s.rx = sc.Receiver.ECEF()
	tr := Truth{Receiver: s.rx}
	for _, r := range sc.Radars {
		p := r.Pos.ECEF()
		s.radP = append(s.radP, p)
		s.radF = append(s.radF, geo.NewFrame(r.Pos))
		tr.Radars = append(tr.Radars, p)
		s.nextI = append(s.nextI, s.rng.Float64()/r.PRFHz)
		s.nI = append(s.nI, s.rng.Intn(64))
	}
	for i := 0; i < sc.Aircraft; i++ {
		s.ac = append(s.ac, s.spawn(0, false))
	}
	const chunk = 5.0
	for t0 := 0.0; t0 < sc.DurS; t0 += chunk {
		t1 := math.Min(t0+chunk, sc.DurS)
		// Respawn aircraft that left the region.
		for i, a := range s.ac {
			p, _ := s.pos(a, t0)
			en := s.f.ToENU(p)
			if math.Hypot(en.X, en.Y) > sc.RegionM*1.05 {
				s.ac[i] = s.spawn(t0, true)
			}
		}
		var recs []rec
		var evs []Event
		for k := range sc.Radars {
			recs = s.interrogate(k, t0, t1, recs)
		}
		// FRUIT.
		nf := poisson(s.rng, sc.FruitRate*(t1-t0))
		for i := 0; i < nf; i++ {
			t := t0 + s.rng.Float64()*(t1-t0)
			recs = append(recs, rec{t: s.local(t), dur: 20.3e-6, r: ssr.Reply{T: s.local(t), Class: ssr.ClassModeAC, Code: uint16(s.rng.Intn(0o10000))}})
			s.st.Fruit++
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].t < recs[j].t })
		if !sc.NoGarble {
			end, last := math.Inf(-1), -1
			for i := range recs {
				if recs[i].t < end {
					recs[i].garb = true
					if last >= 0 {
						recs[last].garb = true
					}
				}
				if e := recs[i].t + recs[i].dur; e > end {
					end, last = e, i
				}
			}
		}
		for i := range recs {
			if recs[i].garb {
				s.st.Garbled++
				continue
			}
			s.st.Emitted++
			evs = append(evs, Event{T: recs[i].r.T, Reply: recs[i].r})
		}
		// ADS-B positions.
		dt := 1 / sc.PosRateHz
		for _, a := range s.ac {
			if !a.adsb {
				continue
			}
			for t := t0 + math.Mod(a.posPhase-t0+1e6*dt, dt); t < t1; t += dt {
				p, alt := s.pos(a, t-a.latency)
				fe := geo.NewFrameECEF(p)
				p = fe.FromENU(geo.Vec3{X: s.rng.NormFloat64() * sc.PosSigmaM, Y: s.rng.NormFloat64() * sc.PosSigmaM, Z: s.rng.NormFloat64() * sc.PosSigmaM / 2})
				evs = append(evs, Event{T: s.local(t), IsPos: true, ICAO: a.icao, Pos: ssr.Position{
					T: s.local(t), P: p, SigmaM: sc.PosSigmaM, Squawk: int32(a.squawk), AltBaroFt: alt / 0.3048,
				}})
				s.st.Positions++
			}
		}
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].T < evs[j].T })
		for _, e := range evs {
			emit(e)
		}
	}
	return tr, s.st
}

func poisson(r *rand.Rand, mu float64) int {
	if mu > 50 {
		return int(math.Max(0, math.Round(mu+math.Sqrt(mu)*r.NormFloat64())))
	}
	l, k, p := math.Exp(-mu), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func angDiff(a, b float64) float64 {
	d := math.Mod(a-b+540, 360) - 180
	return math.Abs(d)
}

// interrogate generates radar k's interrogations in [t0, t1) and the
// replies the receiver hears.
func (s *sim) interrogate(k int, t0, t1 float64, recs []rec) []rec {
	sc := &s.sc
	r := &sc.Radars[k]
	rf := &s.radF[k]
	rp := s.radP[k]
	// Azimuth buckets of the aircraft over the chunk.
	var bins [360][]*aircraft
	for _, a := range s.ac {
		pa, _ := s.pos(a, t0)
		pb, _ := s.pos(a, t1)
		ea, eb := rf.ToENU(pa), rf.ToENU(pb)
		if math.Hypot(ea.X, ea.Y) > r.RangeM+20e3 {
			continue
		}
		za := math.Atan2(ea.X, ea.Y) * 180 / math.Pi
		zb := math.Atan2(eb.X, eb.Y) * 180 / math.Pi
		d := math.Mod(zb-za+540, 360) - 180
		lo, hi := za, za+d
		if hi < lo {
			lo, hi = hi, lo
		}
		lo -= r.BeamwidthDeg
		hi += r.BeamwidthDeg
		for z := math.Floor(lo); z <= hi; z++ {
			b := int(math.Mod(z+720, 360))
			bins[b] = append(bins[b], a)
		}
	}
	pri := 1 / r.PRFHz
	for s.nextI[k] < t1 {
		t := s.nextI[k]
		n := s.nI[k]
		m := 1.0
		if len(r.Stagger) > 0 {
			m = r.Stagger[n%len(r.Stagger)]
		}
		s.nextI[k] += pri * m
		s.nI[k]++
		if t < t0 {
			continue
		}
		s.st.Interrogations++
		mode := r.Modes[n%len(r.Modes)]
		beam := math.Mod(r.Az0Deg+360*t/r.PeriodS, 360)
		for _, a := range bins[int(beam)%360] {
			p0, _ := s.pos(a, t)
			e := rf.ToENU(p0)
			if math.Hypot(e.X, e.Y) > r.RangeM {
				continue
			}
			if angDiff(math.Atan2(e.X, e.Y)*180/math.Pi, beam) > r.BeamwidthDeg/2 {
				continue
			}
			if mode == ModeSAllCall && a.lockout&(1<<uint(k)) != 0 {
				continue
			}
			if s.rng.Float64() > sc.ReplyProb {
				continue
			}
			if !geo.LineOfSight(rp, p0, 0) {
				continue
			}
			tin := t + s.atm.Range(rp, p0)/geo.C
			pa, alt := s.pos(a, tin)
			tin = t + s.atm.Range(rp, pa)/geo.C
			var rep ssr.Reply
			var tx, dur, rxj float64
			switch mode {
			case ModeSAllCall:
				tx = tin + 128e-6 + a.biasS + s.rng.NormFloat64()*sc.SJitterNs*1e-9
				dur, rxj = 64e-6, sc.RxJitterSs
				rep = ssr.Reply{Class: ssr.ClassModeS, ICAO: a.icao, IID: r.IID}
			default:
				tx = tin + 3e-6 + a.biasAC + s.rng.NormFloat64()*sc.ACJitterNs*1e-9
				dur, rxj = 20.3e-6, sc.RxJitterACs
				code := a.squawk
				if mode == ModeC {
					c, ok := ssr.ModeCCode(alt / 0.3048)
					if !ok {
						continue
					}
					code = c
				}
				rep = ssr.Reply{Class: ssr.ClassModeAC, Code: code}
			}
			s.st.Replies++
			// Reception.
			rng := pa.Dist(s.rx)
			if rng > sc.RxRangeM || !geo.LineOfSight(s.rx, pa, 0) {
				continue
			}
			pdet := 0.97
			if rng > 150e3 {
				pdet = 0.97 - 0.5*(rng-150e3)/(sc.RxRangeM-150e3)
			}
			if s.rng.Float64() > pdet {
				continue
			}
			s.st.Heard++
			ta := tx + s.atm.Range(s.rx, pa)/geo.C
			rep.T = s.local(ta) + s.rng.NormFloat64()*rxj*1e-9
			recs = append(recs, rec{t: rep.T, dur: dur, r: rep})
		}
	}
	return recs
}
