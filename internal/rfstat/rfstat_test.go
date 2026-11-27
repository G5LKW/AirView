package rfstat

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want %.4f +- %.4f", name, got, want, tol)
	}
}

func at(sec float64) time.Time { return simBase.Add(time.Duration(sec * 1e9)) }

func logistic(d, mid, w float64) float64 { return 1 / (1 + math.Exp((d-mid)/w)) }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "NaN") || strings.Contains(string(b), "Inf") {
		t.Fatalf("JSON contains NaN/Inf")
	}
	return string(b)
}

// ------------------------------------------------------------ detection

func TestDetectionProbability(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var acs []simAC
	for i := 0; i < 400; i++ {
		d := 2 + rng.Float64()*395
		acs = append(acs, simAC{icao: 0x400000 + uint32(i), dist: d, brg: rng.Float64() * 360,
			elev: math.Atan2(10, d) * 180 / math.Pi, alt: 33000, track: rng.Float64() * 360,
			pd: logistic(d, 250, 20), rssi: -10 - 20*math.Log10(d/10)})
	}
	sm := newSim(7, acs, false)
	st := New(Config{Antennas: []string{"yagi"}})
	sm.run(st, 1800)
	rep := st.Report()
	checkHealthData(t, rep.Health)
	mustJSON(t, rep)

	// totals per type versus the generator's own counts (all aircraft
	// trackable: Pd 0.3..1; never-heard aircraft cannot be counted)
	var acs2 []simAC
	for i := 0; i < 100; i++ {
		acs2 = append(acs2, simAC{icao: 0x410000 + uint32(i), dist: 100, brg: 10, elev: 2, pd: 0.3 + 0.7*rng.Float64(), rssi: -30})
	}
	sm2 := newSim(8, acs2, false)
	st2 := New(Config{})
	sm2.run(st2, 1200)
	rep2 := st2.Report()
	checkHealthData(t, rep2.Health)
	for typ := 0; typ < 3; typ++ {
		truth := 1 - float64(sm2.lost[typ])/float64(sm2.sent[typ])
		near(t, "Pd "+pdTypeNames[typ], float64(rep2.Detection.Types[typ].Pd), truth, 0.015)
	}
	// per-distance bin versus mean truth of the aircraft in the bin
	var sumPd, n [nPdDist]float64
	for _, a := range acs {
		b := int(a.dist / pdDistKm)
		sumPd[b] += a.pd
		n[b]++
	}
	checked := 0
	for i, p := range rep.Detection.VsDistance.Pos {
		if !p.Valid() || n[i] == 0 {
			continue
		}
		truth := sumPd[i] / n[i]
		if truth < 0.1 {
			continue
		}
		near(t, "Pd bin "+ftoa(float64(i*10)), float64(p), truth, 0.04)
		checked++
	}
	if checked < 25 {
		t.Errorf("only %d distance bins checked", checked)
	}
	near(t, "Range50Km", float64(rep.Detection.Range50Km), 250, 8)
	near(t, "Range90Km", float64(rep.Detection.Range90Km), 250-20*math.Log(9), 10)

	// update gaps: band 0 (< 50 km) has Pd ~1 -> gaps uniform 0.4-0.6 s
	g := rep.Detection.Gaps[0]
	near(t, "gap p50 <50km", float64(g.P50), 0.5, 0.02)
	near(t, "P(age<=2s) <50km", float64(g.PUpdate[0]), 1, 0.001)
	if len(rep.Detection.Aircraft) == 0 {
		t.Error("no per-aircraft Pd")
	}
	if rep.Health.Items[2].Score < 0 {
		t.Errorf("detection health unknown: %+v", rep.Health.Items[2])
	}
}

// TestUpdateGapKnown feeds one aircraft with positions every 0.5 s and
// drops every second one: gaps are exactly 1.0 s.
func TestUpdateGapKnown(t *testing.T) {
	st := New(Config{})
	for i := 0; i < 2000; i++ {
		ts := float64(i) * 0.5
		st.Tick(at(ts))
		if i%2 == 1 {
			continue
		}
		f := NewFrame(at(ts))
		f.DF, f.CRC, f.ICAO, f.Kind, f.DistKm = 17, CRCOK, 0xABCDEF, KindPosEven, 120
		st.Observe(f)
	}
	rep := st.Report()
	checkHealthData(t, rep.Health)
	g := rep.Detection.Gaps[2] // 100-150 km
	if g.N != 999 {
		t.Fatalf("gap N = %d", g.N)
	}
	near(t, "gap mean", float64(g.Mean), 1, 1e-9)
	near(t, "gap p50", float64(g.P50), 1.05, 0.051)
	near(t, "P(age<=2s)", float64(g.PUpdate[0]), 1, 1e-9)
	// T=... all thresholds >= 1 s give 1
	near(t, "position Pd", float64(rep.Detection.Types[0].Pd), 0.5, 0.01)
}

// ------------------------------------------------------------ timing

func TestTimingIntervals(t *testing.T) {
	var acs []simAC
	for i := 0; i < 30; i++ {
		acs = append(acs, simAC{icao: 0x500000 + uint32(i), dist: 50, brg: 90, elev: 5, alt: 30000, pd: 0.9, rssi: -20})
	}
	acs = append(acs, simAC{icao: 0xBAD001, dist: 50, brg: 90, elev: 5, pd: 1, rssi: -20, fixed: 0.5})
	acs = append(acs, simAC{icao: 0xBAD002, dist: 50, brg: 90, elev: 5, pd: 1, rssi: -20, fixed: 0.7})
	sm := newSim(3, acs, true)
	st := New(Config{})
	sm.run(st, 900)
	rep := st.Report()
	checkHealthData(t, rep.Health)
	tm := rep.Timing
	if !tm.Available {
		t.Fatal("timing not available")
	}
	pos := tm.Types[0]
	near(t, "single mean", float64(pos.Mean), 0.5, 0.003)
	near(t, "single std (uniform 0.4-0.6 = 0.0577)", float64(pos.Std), 0.2/math.Sqrt(12), 0.004)
	// 0.7 s transmitter: every one of its intervals is impossible
	if !(pos.BadFrac > 0.01) {
		t.Errorf("bad frac %v", pos.BadFrac)
	}
	near(t, "alternation", float64(tm.AltRegularity), 1, 1e-9)
	near(t, "k=2 same parity", float64(tm.Alt2Same), 1, 1e-9)
	flag := map[string]string{}
	for _, nc := range tm.NonCompliant {
		flag[nc.ICAO] = nc.Reason
	}
	if !strings.Contains(flag["BAD001"], "jitter") {
		t.Errorf("fixed 0.5 s transmitter not flagged: %v", flag)
	}
	if !strings.Contains(flag["BAD002"], "outside") {
		t.Errorf("0.7 s transmitter not flagged: %v", flag)
	}
	if len(flag) != 2 {
		t.Errorf("unexpected flags: %v", flag)
	}

	// the uniform test must pass for compliant traffic only
	st2 := New(Config{})
	newSim(4, acs[:30], true).run(st2, 900)
	if p := st2.Report().Timing.Types[0]; !p.UniformOK {
		t.Errorf("uniform jitter rejected: chi2 %v", p.Chi2)
	}
}

func TestClassifyInterval(t *testing.T) {
	for _, c := range []struct {
		s float64
		k int
	}{{0.3, 0}, {0.395, 1}, {0.5, 1}, {0.61, 1}, {0.7, 0}, {0.8, 2}, {1.2, 2}, {1.5, 3}} {
		if k := classifyInterval(0, c.s); k != c.k {
			t.Errorf("classify(%v) = %d, want %d", c.s, k, c.k)
		}
	}
	if classifyInterval(2, 5.0) != 1 || classifyInterval(2, 4.0) != 0 {
		t.Error("ident classification")
	}
}

// ------------------------------------------------------------ channel

func TestChannelOccupancy(t *testing.T) {
	st := New(Config{})
	st.Tick(at(0))
	for sec := 0; sec < 200; sec++ {
		for i := 0; i < 1000; i++ {
			f := NewFrame(at(float64(sec) + float64(i)/1000))
			f.ICAO = 0x300000 + uint32(i%50)
			switch {
			case i < 50: // 5 % bad
				f.DF, f.CRC = 17, CRCBad
			case i < 600:
				f.DF, f.CRC, f.Kind = 4, CRCAddrAccepted, KindSurvAlt
			default:
				f.DF, f.CRC, f.Kind = 17, CRCOK, KindVelocity
			}
			st.Observe(f)
		}
	}
	st.Tick(at(200))
	cRep := st.Report()
	checkHealthData(t, cRep.Health)
	c := cRep.Channel
	near(t, "seconds", float64(c.Seconds), 200, 0)
	near(t, "fps", float64(c.FPS), 1000, 1e-9)
	// 550 short (64 us) + 450 long (120 us) per second
	occ := (550*64.0 + 450*120.0) / 1e6 * 100
	near(t, "occupancy", float64(c.OccupancyPct), occ, 1e-9)
	near(t, "bad", float64(c.BadFrac), 0.05, 1e-9)
	tau := (550*64.0 + 450*120) / 1000
	near(t, "overlap", float64(c.OverlapProb), 1-math.Exp(-2*1000*tau*1e-6), 1e-9)
	near(t, "implied rate", float64(c.ImpliedRate), -math.Log(0.95)/(2*tau*1e-6), 1e-6)
	near(t, "peak occ", float64(c.PeakOccPct), occ, 1e-3)
	if len(c.VsLoad) != 1 || c.VsLoad[0].Seconds != 200 {
		t.Errorf("vs load %+v", c.VsLoad)
	}
	var df17 int64
	for _, d := range c.ByDF {
		if d.DF == 17 {
			df17 = d.Count
		}
	}
	if df17 != 450*200 {
		t.Errorf("DF17 count %d", df17)
	}
	if c.BurstCount != 0 {
		t.Errorf("unexpected bursts %d", c.BurstCount)
	}
}

func TestBurstDetection(t *testing.T) {
	st := New(Config{})
	rng := rand.New(rand.NewPCG(5, 6))
	emit := func(sec, n int) {
		for i := 0; i < n; i++ {
			f := NewFrame(at(float64(sec) + rng.Float64()*0.999))
			f.DF, f.CRC = 11, CRCOK
			st.Observe(f)
		}
	}
	for sec := 0; sec < 300; sec++ {
		st.Tick(at(float64(sec)))
		n := 200 + rng.IntN(21) - 10
		if sec == 250 {
			n = 900
		}
		emit(sec, n)
	}
	st.Tick(at(300))
	cRep := st.Report()
	checkHealthData(t, cRep.Health)
	c := cRep.Channel
	if c.BurstCount != 1 || len(c.Bursts) != 1 || c.Bursts[0].T != at(250).UnixMilli() || c.Bursts[0].Frames != 900 {
		t.Fatalf("bursts %d %+v", c.BurstCount, c.Bursts)
	}
}

// ------------------------------------------------------------ interrogation

func TestInterrogatorCensus(t *testing.T) {
	st := New(Config{})
	const dur = 600
	for sec := 0; sec < dur; sec++ {
		st.Tick(at(float64(sec)))
		for a := 0; a < 20; a++ {
			icao := 0x600000 + uint32(a)
			base := float64(sec) + float64(a)/40
			f := NewFrame(at(base))
			f.DF, f.CRC, f.ICAO, f.Kind = 4, CRCAddrAccepted, icao, KindSurvAlt
			st.Observe(f)
			if sec%2 == 0 { // SI 12 from every aircraft every 2 s (10/s total)
				f = NewFrame(at(base + 0.01))
				f.DF, f.CRC, f.ICAO, f.Kind, f.IID = 11, CRCOK, icao, KindAllCall, 16+12
				st.Observe(f)
			}
			if a < 2 { // II 3: only two aircraft reply (others locked out), 2/s total
				f = NewFrame(at(base + 0.02))
				f.DF, f.CRC, f.ICAO, f.Kind, f.IID = 11, CRCOK, icao, KindAllCall, 3
				st.Observe(f)
			}
			if sec%10 == 0 {
				bds := []uint8{0x40, 0x40, 0x50, 0x60, 0x40}[a%5]
				f = NewFrame(at(base + 0.03))
				f.DF, f.CRC, f.ICAO, f.Kind, f.BDS = 20, CRCAddrAccepted, icao, KindCommBAlt, bds
				st.Observe(f)
			}
		}
	}
	st.Tick(at(dur))
	inRep := st.Report()
	checkHealthData(t, inRep.Health)
	in := inRep.Interrogation
	byCode := map[string]Interrogator{}
	for _, c := range in.Codes {
		byCode[c.Code] = c
	}
	si, ii := byCode["SI 12"], byCode["II 3"]
	if si.Replies != 20*dur/2 || ii.Replies != 2*dur {
		t.Fatalf("replies SI12 %d II3 %d", si.Replies, ii.Replies)
	}
	near(t, "SI12 rate", float64(si.Rate), 10, 1e-9)
	near(t, "II3 rate", float64(ii.Rate), 2, 1e-9)
	if si.Aircraft != 20 || ii.Aircraft != 2 {
		t.Errorf("aircraft SI12 %d II3 %d", si.Aircraft, ii.Aircraft)
	}
	if si.Lockout || !ii.Lockout {
		t.Errorf("lockout SI12 %v II3 %v", si.Lockout, ii.Lockout)
	}
	if in.ActiveRadars != 2 || len(in.Codes) != 2 {
		t.Errorf("active radars %d codes %d", in.ActiveRadars, len(in.Codes))
	}
	if si.Last10m != 20*270 { // minutes 1..9 closed, minute 10 just started
		t.Errorf("SI12 last10m %d", si.Last10m)
	}
	mix := map[string]float64{}
	for _, b := range in.CommB {
		mix[b.BDS] = float64(b.Frac)
	}
	near(t, "BDS4,0", mix["4,0"], 0.6, 1e-9)
	near(t, "BDS5,0", mix["5,0"], 0.2, 1e-9)
	near(t, "BDS6,0", mix["6,0"], 0.2, 1e-9)
	// per aircraft: 1 DF4/s + 0.1 DF20/s
	near(t, "surv rate p50", float64(in.SurvRateP50), 660.0/599.5, 0.01)
	if in.ADSBAndModeS != 0 || in.ModeSOnly != 20 {
		t.Errorf("surveillance split %+v", in)
	}
}

// ------------------------------------------------------------ fading / multipath / aspect

func riceSample(rng *rand.Rand, k float64) float64 {
	s := math.Sqrt(k / (k + 1))
	sg := math.Sqrt(1 / (2 * (k + 1)))
	i := s + sg*rng.NormFloat64()
	q := sg * rng.NormFloat64()
	return i*i + q*q // power, mean 1
}

func fadingRun(k float64, seed uint64) FadingStats {
	rng := rand.New(rand.NewPCG(seed, 1))
	st := New(Config{})
	for a := 0; a < 40; a++ {
		for i := 0; i < 2400; i++ { // 10 minutes at 4 frames/s
			ts := float64(i) * 0.25
			f := NewFrame(at(ts + float64(a)*0.001))
			f.DF, f.CRC, f.ICAO, f.Kind = 17, CRCOK, 0x700000+uint32(a), KindVelocity
			f.DistKm, f.ElevDeg = 100, 3
			f.Signal = -40 + 10*math.Log10(riceSample(rng, k))
			st.Observe(f)
		}
	}
	return st.Report().Signal.Fading
}

func TestRicianK(t *testing.T) {
	f := fadingRun(5, 11)
	near(t, "median K dB (K=5)", float64(f.KMedianDB), 10*math.Log10(5), 1.2)
	if f.Windows < 1000 {
		t.Errorf("windows %d", f.Windows)
	}
	f = fadingRun(20, 12)
	near(t, "median K dB (K=20)", float64(f.KMedianDB), 13, 1.5)
	f = fadingRun(0, 13) // Rayleigh
	if !(float64(f.KMedianDB) < 0) {
		t.Errorf("Rayleigh median K %v dB, want < 0", f.KMedianDB)
	}
	near(t, "Rayleigh dB std (5.57 dB)", float64(f.StdMedianDB), 5.57, 1.0)
	if len(f.Worst) == 0 {
		t.Error("no per-aircraft fading")
	}
}

func TestRiceKMoments(t *testing.T) {
	// exact moments: E[P]=1, Var[P]=(1+2K)/(1+K)^2
	for _, k := range []float64{0.5, 1, 5, 30} {
		v := (1 + 2*k) / ((1 + k) * (1 + k))
		n := 1000.0
		sp := n
		sp2 := v*(n-1) + n // (sp2 - n*1)/(n-1) = v
		near(t, "riceK", riceK(n, sp, sp2), k, 1e-6*k)
	}
}

func TestMultipathHeight(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	const h = 15.0
	st := New(Config{})
	for a := 0; a < 60; a++ {
		for i := 0; i < 3000; i++ {
			e := 0.3 + rng.Float64()*9.5
			ripple := 3 * math.Cos(2*math.Pi*2*h/lambdaM*math.Sin(e*math.Pi/180))
			f := NewFrame(at(float64(i)*0.3 + float64(a)*0.001))
			f.DF, f.CRC, f.ICAO, f.Kind = 17, CRCOK, 0x710000+uint32(a), KindVelocity
			f.DistKm, f.ElevDeg = 100, e
			f.Signal = -40 + ripple + rng.NormFloat64()*0.5
			st.Observe(f)
		}
	}
	mp := st.Report().Signal.Multipath
	if !mp.Detected {
		t.Fatalf("multipath not detected: %+v", mp.Strength)
	}
	near(t, "reflector height", float64(mp.ReflectorHeightM), h, 0.5)
	near(t, "ripple rms (3/sqrt2)", float64(mp.RippleDB), 3/math.Sqrt2, 0.5)

	// no ripple -> not detected
	st2 := New(Config{})
	for i := 0; i < 60000; i++ {
		f := NewFrame(at(float64(i) * 0.01))
		f.DF, f.CRC, f.ICAO, f.Kind = 17, CRCOK, 0x720000+uint32(i%50), KindVelocity
		f.DistKm, f.ElevDeg = 100, 0.3+rng.Float64()*9.5
		f.Signal = -40 + rng.NormFloat64()*0.5
		st2.Observe(f)
	}
	if mp := st2.Report().Signal.Multipath; mp.Detected {
		t.Errorf("false multipath detection: strength %v ripple %v", mp.Strength, mp.RippleDB)
	}
}

func TestAspect(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 32))
	st := New(Config{})
	for a := 0; a < 50; a++ {
		for i := 0; i < 1000; i++ {
			brg := rng.Float64() * 360
			asp := rng.Float64() * 180 // angle between track and direction to receiver
			track := math.Mod(brg+180+asp, 360)
			sig := -30.0
			if asp >= 150 {
				sig -= 3 // tail-on shadowing
			}
			f := NewFrame(at(float64(i)*0.5 + float64(a)*0.001))
			f.DF, f.CRC, f.ICAO, f.Kind = 17, CRCOK, 0x730000+uint32(a), KindVelocity
			f.DistKm, f.BearingDeg, f.TrackDeg = 100, brg, track
			f.Signal = sig
			st.Observe(f)
		}
	}
	as := st.Report().Signal.Aspect
	near(t, "nose-tail dB", float64(as.NoseTailDB), 3, 0.2)
	near(t, "nose-beam dB", float64(as.NoseDB)-float64(as.BeamDB), 0, 0.2)
}

// ------------------------------------------------------------ signal

func TestSignalHistogram(t *testing.T) {
	st := New(Config{Antennas: []string{"yagi", "vertical"}, DecodeSNRdB: 10})
	// uniform levels from -40 to 0 dBFS on antenna 0, -40..-20 on antenna 1
	for i := 0; i < 40000; i++ {
		f := NewFrame(at(float64(i) * 0.001))
		f.DF, f.CRC, f.Kind = 11, CRCOK, KindAllCall
		f.Antenna = int8(i % 2)
		if f.Antenna == 0 {
			f.Signal = -40 + 40*(float64(i/2)+0.5)/20000
		} else {
			f.Signal = -40 + 20*(float64(i/2)+0.5)/20000
		}
		f.DistKm, f.ElevDeg = float64(i%400), float64(i%30)
		st.Observe(f)
	}
	sRep := st.Report()
	checkHealthData(t, sRep.Health)
	s := sRep.Signal
	// mixture: density 3/80 per dB on -40..-20, 1/80 on -20..0
	near(t, "strong frac", float64(s.StrongFrac), 3.0/80, 0.002)
	near(t, "p50", float64(s.P50), -40+40.0/3, 0.1)
	near(t, "decode floor (p1)", float64(s.DecodeFloor), -40+0.01*80/3, 0.05)
	near(t, "noise est", float64(s.NoiseFloorEst), float64(s.DecodeFloor)-10, 1e-9)
	if len(s.PerAntenna) != 2 || s.PerAntenna[1].Label != "vertical" {
		t.Fatalf("per antenna %+v", s.PerAntenna)
	}
	near(t, "vertical p50", float64(s.PerAntenna[1].P50), -30, 0.1)
	if len(s.PerRing) != nRing || len(s.PerElev) == 0 {
		t.Errorf("rings %d elev %d", len(s.PerRing), len(s.PerElev))
	}
	hRep := st.Report()
	checkHealthData(t, hRep.Health)
	h := hRep.Health
	if h.Items[0].Key != "gain" || h.Items[0].Status != "warn" {
		t.Errorf("gain health %+v", h.Items[0])
	}
}

// ------------------------------------------------------------ census / GNSS

func TestCensusAndGNSS(t *testing.T) {
	st := New(Config{StationLat: 48, StationLon: 11})
	for sec := 0; sec < 180; sec++ {
		st.Tick(at(float64(sec)))
		for a := 0; a < 20; a++ {
			f := NewFrame(at(float64(sec) + float64(a)*0.01))
			f.DF, f.CRC, f.ICAO = 17, CRCOK, 0x800000+uint32(a)
			f.Kind = KindPosEven
			f.DistKm, f.BearingDeg = 120, 45
			f.Lat, f.Lon = 48.8+float64(sec)*0.002, 12.0
			f.Version = int8(a % 3)
			f.NIC, f.NACp, f.SIL = 8, 9, 3
			if a < 5 && sec >= 60 {
				f.NIC, f.NACp = 0, 0 // integrity drop
			}
			if a == 7 && sec == 100 {
				f.Lat += 1 // 111 km jump
			}
			f.Category = 0xA3
			if a == 0 {
				f.Category = 0xA5
			}
			f.GNSSDeltaFt = 300
			f.AltFt = 35000
			st.Observe(f)
			if sec%5 == 0 && a < 10 {
				g := f
				g.Kind = KindOpStatus
				st.Observe(g)
			}
		}
	}
	st.Tick(at(180))
	cRep := st.Report()
	checkHealthData(t, cRep.Health)
	c := cRep.Census
	if c.Aircraft != 20 || c.ADSB != 20 {
		t.Fatalf("aircraft %d adsb %d", c.Aircraft, c.ADSB)
	}
	vers := map[string]int{}
	for _, v := range c.Versions {
		vers[v.Label] = v.Count
	}
	if vers["v0"] != 7 || vers["v1"] != 7 || vers["v2"] != 6 {
		t.Errorf("versions %v", vers)
	}
	near(t, "TC31 share", float64(c.TC31Share), 0.5, 1e-9)
	near(t, "GNSS delta mean", float64(c.GNSSDeltaMean), 300, 1e-9)
	if c.Degraded != 5 {
		t.Errorf("degraded %d", c.Degraded)
	}
	near(t, "GNSS index now", float64(c.GNSSIndexNow), 25, 1e-9)
	near(t, "poor integrity", float64(c.PoorIntegrity), 0.25, 1e-9)
	if c.JumpAircraft != 1 {
		t.Errorf("jump aircraft %d", c.JumpAircraft)
	}
	if len(c.GNSSSeries) != 3 || c.GNSSSeries[0].Deg != 0 || c.GNSSSeries[2].Deg != 5 {
		t.Errorf("series %+v", c.GNSSSeries)
	}
	if len(c.GNSSCells) != 1 || c.GNSSCells[0].Sector != 4 || c.GNSSCells[0].Ring != 2 || !c.GNSSCells[0].CenterLat.Valid() {
		t.Errorf("cells %+v", c.GNSSCells)
	}
	if len(c.Categories) != 2 || c.Categories[0].Label != "A3" || c.Categories[0].Count != 19 {
		t.Errorf("categories %+v", c.Categories)
	}
}

// ------------------------------------------------------------ table, num, history

func TestTable(t *testing.T) {
	rng := rand.New(rand.NewPCG(41, 42))
	tb := newTable(500)
	ref := map[uint32]bool{}
	for i := 0; i < 200000; i++ {
		k := rng.Uint32N(1500) + 1
		if rng.IntN(2) == 0 {
			s, _ := tb.get(k)
			if s == nil {
				if len(ref) < 500 {
					t.Fatalf("insert failed with %d entries", len(ref))
				}
				continue
			}
			if s.icao != k {
				t.Fatal("wrong slot")
			}
			ref[k] = true
		} else if ref[k] {
			tb.remove(k)
			delete(ref, k)
		}
		if tb.n != len(ref) {
			t.Fatalf("n %d ref %d", tb.n, len(ref))
		}
	}
	for k := range ref {
		s, isNew := tb.get(k)
		if s == nil || isNew {
			t.Fatalf("lost key %x", k)
		}
	}
}

func TestNumJSON(t *testing.T) {
	b, _ := json.Marshal([]Num{Num(math.NaN()), Num(math.Inf(1)), 1.23456789, -2})
	if string(b) != "[null,null,1.2346,-2]" {
		t.Fatalf("got %s", b)
	}
	var n []Num
	if err := json.Unmarshal(b, &n); err != nil || n[0].Valid() || float64(n[2]) != 1.2346 {
		t.Fatalf("unmarshal %v %v", n, err)
	}
}

func TestHourRollAndHistory(t *testing.T) {
	st := New(Config{History: 3})
	for h := 0; h < 5; h++ {
		for sec := 0; sec < 3600; sec += 10 {
			ts := float64(h*3600 + sec)
			st.Tick(at(ts))
			f := NewFrame(at(ts))
			f.DF, f.CRC = 11, CRCOK
			st.Observe(f)
		}
	}
	st.Tick(at(5 * 3600))
	hist := st.History()
	if len(hist) != 3 {
		t.Fatalf("history %d", len(hist))
	}
	if hist[2].T != at(4*3600).UnixMilli() || hist[2].Frames != 360 {
		t.Errorf("last hour %+v", hist[2])
	}
	near(t, "hour seconds", float64(hist[2].Seconds), 3600, 0)
	mustJSON(t, hist)
	// sliding window right after the roll = previous hour
	rep := st.Report()
	checkHealthData(t, rep.Health)
	near(t, "window", float64(rep.WindowSec), 3600, 1)
}
