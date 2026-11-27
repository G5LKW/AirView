package sim

import (
	"context"
	"hash/fnv"
	"math"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
)

// funcSink adapts a function to model.FrameSink.
type funcSink func(model.RawFrame)

func (f funcSink) HandleFrame(fr model.RawFrame)  { f(fr) }
func (funcSink) HandleUpdate(model.DecodedUpdate) {}

// streamHash runs the simulation for d and returns a hash over everything
// that is delivered, plus the number of frames.
func streamHash(t *testing.T, s *Sim, d, chunk time.Duration) (uint64, int) {
	t.Helper()
	h := fnv.New64a()
	n := 0
	var b [8]byte
	put := func(v uint64) {
		for i := range b {
			b[i] = byte(v >> (8 * i))
		}
		h.Write(b[:])
	}
	sink := funcSink(func(f model.RawFrame) {
		n++
		h.Write(f.Data)
		put(uint64(f.Recv.UnixNano()))
		put(uint64(f.GPSNs))
		put(math.Float64bits(f.Signal))
		put(uint64(int64(f.Channel)))
		put(uint64(f.Kind))
		if f.MLAT {
			put(1)
		}
	})
	for done := time.Duration(0); done < d; done += chunk {
		if err := s.Advance(context.Background(), chunk, sink); err != nil {
			t.Fatal(err)
		}
	}
	return h.Sum64(), n
}

func TestDeterministic(t *testing.T) {
	cfg := Config{Aircraft: 80, Seed: 42, Start: testStart}
	h1, n1 := streamHash(t, New(cfg), 3*time.Minute, 3*time.Minute)
	if n1 == 0 {
		t.Fatal("no frames")
	}
	s3 := New(cfg)
	h3, n3 := streamHash(t, s3, 3*time.Minute, time.Second)
	if h1 != h3 || n1 != n3 {
		t.Errorf("same seed, different stream: %d frames %x vs %d frames %x", n1, h1, n3, h3)
	}
	if want := testStart.Add(3 * time.Minute); !s3.Now().Equal(want) {
		t.Errorf("virtual clock %v, want %v", s3.Now(), want)
	}
	// Advance rounds up to whole 100 ms steps
	s5 := New(cfg)
	if err := s5.Advance(context.Background(), 250*time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if want := testStart.Add(300 * time.Millisecond); !s5.Now().Equal(want) {
		t.Errorf("Advance(250 ms) moved the clock to %v, want %v", s5.Now(), want)
	}

	cfg.Seed = 43
	if h4, _ := streamHash(t, New(cfg), 3*time.Minute, time.Minute); h4 == h1 {
		t.Error("different seeds produce the same stream")
	}

	// Seed 0 draws a random seed, which is reported and reproduces the run.
	a := New(Config{Aircraft: 20, Start: testStart})
	if a.Seed() == 0 || a.Config().Seed != a.Seed() {
		t.Fatalf("random seed not reported: %d", a.Seed())
	}
	b := New(Config{Aircraft: 20, Start: testStart, Seed: a.Seed()})
	ha, _ := streamHash(t, a, 30*time.Second, 30*time.Second)
	hb, _ := streamHash(t, b, 30*time.Second, 30*time.Second)
	if ha != hb {
		t.Error("re-running with the reported seed gives a different stream")
	}
}

func TestDefaultsAndConfig(t *testing.T) {
	s := New(Config{})
	c := s.Config()
	if c.Aircraft != 60 || c.FeedID != "demo" || c.Speed != 1 || c.RangeKm != DefaultRangeKm || len(c.Antennas) != 2 {
		t.Errorf("defaults: %+v", c)
	}
	if c.EmergencyPerHour <= 0 || c.EmergencyPerHour > 0.001 {
		t.Errorf("default emergency rate %v should be very rare", c.EmergencyPerHour)
	}
	if st := s.Station(); !st.Located || st.Lat != DefaultLat || st.Lon != DefaultLon {
		t.Errorf("default station: %+v", st)
	}
	if d := time.Since(s.Now()); d < 0 || d > 5*time.Second {
		t.Errorf("default start is not now: %v ago", d)
	}

	mc := model.ExampleRadarcapeConfig()
	mc.Receiver.Located = true
	mc.Receiver.Lat, mc.Receiver.Lon, mc.Receiver.GroundElevM = 40.64, -73.78, 4
	mc.Demo.Aircraft = 25
	mc.Antennas[1].AzimuthDeg = 200
	fc := FromModel(mc, "sim1")
	fc.Seed, fc.Start, fc.NoDiurnal = 4, testStart, true
	s = New(fc)
	c = s.Config()
	if c.Aircraft != 25 || c.FeedID != "sim1" || c.Antennas[1].AzimuthDeg != 200 {
		t.Errorf("FromModel: %+v", c)
	}
	if st := s.Station(); st.Lat != 40.64 || st.Lon != -73.78 || st.GroundElevM != 4 {
		t.Errorf("station from the receiver: %+v", st)
	}
	// around a North American station most airframes are US registered
	us := 0
	tr := s.Truth()
	for _, a := range tr {
		if a.ICAO >= 0xA00000 && a.ICAO <= 0xAFFFFF {
			us++
		}
		if d := refDistM(40.64, -73.78, a.Lat, a.Lon); d > DefaultRangeKm*1100 {
			t.Errorf("%06X is %.0f km from the station", a.ICAO, d/1000)
		}
	}
	if us < len(tr)/2 {
		t.Errorf("only %d of %d aircraft are US registered", us, len(tr))
	}
	// an unlocated or absurd receiver falls back to the default station
	mc.Receiver.Lat = 89.5
	if st := New(FromModel(mc, "x")).Station(); st.Lat != DefaultLat {
		t.Errorf("polar receiver accepted: %+v", st)
	}
}

var (
	reAirline = regexp.MustCompile(`^[A-Z]{3}[0-9]{1,4}[A-Z]{0,2}$`)
	reReg     = regexp.MustCompile(`^(G[A-Z]{4}|(DE|DM|PH|FG|OO|HB|OE|CG)[A-Z]{3}|N[0-9]{2,4}[A-Z]{0,2})$`)
	reOther   = regexp.MustCompile(`^(HLE|NPAS|CHX|LIFE|RESQ|RRR|GAF|FAF|BAF|NAF|NATO|RCH|PAT)[0-9]{2,4}$`)
	reSquawk  = regexp.MustCompile(`^[0-7]{4}$`)
)

// stateBlocks are the ICAO 24-bit address allocations (Annex 10 Vol III) the
// simulator is allowed to draw from.
var stateBlocks = [][2]uint32{
	{0x400000, 0x43FFFF}, {0x4CA000, 0x4CAFFF}, {0x3C0000, 0x3FFFFF}, {0x480000, 0x487FFF},
	{0x380000, 0x3BFFFF}, {0x340000, 0x37FFFF}, {0x300000, 0x33FFFF}, {0x4B0000, 0x4B7FFF},
	{0x440000, 0x447FFF}, {0x470000, 0x477FFF}, {0x4A8000, 0x4AFFFF}, {0x478000, 0x47FFFF},
	{0x460000, 0x467FFF}, {0x488000, 0x48FFFF}, {0x490000, 0x497FFF}, {0x4B8000, 0x4BFFFF},
	{0x448000, 0x44FFFF}, {0x4CC000, 0x4CCFFF}, {0x4D0000, 0x4D03FF}, {0x896000, 0x896FFF},
	{0x06A000, 0x06A3FF}, {0x768000, 0x76FFFF}, {0x780000, 0x7BFFFF}, {0x840000, 0x87FFFF},
	{0xC00000, 0xC3FFFF}, {0xA00000, 0xAFFFFF},
}

// TestWorldBehaviour runs two hours of traffic and checks that the aircraft
// behave like the kinds of traffic they are supposed to be.
func TestWorldBehaviour(t *testing.T) {
	hours := 2.0
	if testing.Short() {
		hours = 0.5
	}
	s := New(Config{Aircraft: 80, Seed: 7, Start: testStart, NoDiurnal: true, EmergencyPerHour: -1})
	st := s.Station()
	groundFt := st.GroundElevM / 0.3048

	apts := s.Airports()
	if len(apts) < 3 {
		t.Fatalf("%d airports", len(apts))
	}
	for _, ap := range apts {
		if d := refDistM(st.Lat, st.Lon, ap.Lat, ap.Lon) / 1000; d > 150 {
			t.Errorf("airport %.0f km from the station", d)
		}
	}

	first := map[uint32]bool{}
	for _, tr := range s.Truth() {
		first[tr.ICAO] = true
	}
	kinds := map[string]int{}
	phases := map[string]int{}
	cats := map[string]int{}
	squawks := map[string]int{}
	var ovSamples, ovInBand int
	var ovMinGS, ovMaxGS = math.Inf(1), math.Inf(-1)
	var nADSB, nMLAT, nCivil, total int
	lowest := map[uint32]float64{} // arrivals: lowest altitude seen
	ctx := context.Background()
	for i := 0; i < int(hours*360); i++ {
		if err := s.Advance(ctx, 10*time.Second, nil); err != nil {
			t.Fatal(err)
		}
		tr := s.Truth()
		seen := map[uint32]bool{}
		for _, a := range tr {
			if seen[a.ICAO] {
				t.Fatalf("duplicate address %06X", a.ICAO)
			}
			seen[a.ICAO] = true
			for _, v := range []float64{a.Lat, a.Lon, a.AltBaro, a.AltGeom, a.GS, a.Track, a.VertRate, a.TAS, a.IAS,
				a.Mach, a.TrueHeading, a.MagHeading, a.Roll, a.TrackRate, a.SelAlt, a.WindDir, a.WindKt, a.OAT,
				a.DistKm, a.Bearing, a.Elevation, a.EIRPdBm} {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("non-finite truth value: %+v", a)
				}
			}
			kinds[a.Kind]++
			phases[a.Phase]++
			cats[a.Category]++
			squawks[a.Squawk]++
			total++
			if a.Kind != "military" {
				nCivil++
				if a.ADSB {
					nADSB++
				}
				if a.MLAT {
					nMLAT++
				}
			}

			// identity
			inBlock := false
			for _, b := range stateBlocks {
				if a.ICAO >= b[0] && a.ICAO <= b[1] {
					inBlock = true
				}
			}
			if !inBlock {
				t.Fatalf("address %06X is outside every state block", a.ICAO)
			}
			if !reAirline.MatchString(a.Callsign) && !reReg.MatchString(a.Callsign) && !reOther.MatchString(a.Callsign) {
				t.Fatalf("implausible callsign %q (%s)", a.Callsign, a.Kind)
			}
			if !reSquawk.MatchString(a.Squawk) {
				t.Fatalf("squawk %q", a.Squawk)
			}
			if a.ADSB && a.MLAT {
				t.Fatalf("%06X is both ADS-B and MLAT", a.ICAO)
			}

			// geometry
			if d := refDistM(st.Lat, st.Lon, a.Lat, a.Lon) / 1000; d > DefaultRangeKm*1.1 || math.Abs(d-a.DistKm) > 0.5 {
				t.Fatalf("%06X %s at %.1f km (truth says %.1f)", a.ICAO, a.Kind, d, a.DistKm)
			}
			if b := refBearing(st.Lat, st.Lon, a.Lat, a.Lon); a.DistKm > 1 && angDiff(b, a.Bearing) > 0.5 {
				t.Fatalf("bearing %.2f vs %.2f", b, a.Bearing)
			}
			if a.GS < 0 || a.GS > 620 || a.AltBaro < -500 || a.AltBaro > 45000 || math.Abs(a.Roll) > 46 ||
				a.Track < 0 || a.Track >= 360 || a.Mach > 0.9 {
				t.Fatalf("implausible state: %+v", a)
			}
			if a.OnGround && (a.VertRate != 0 || a.GS > 190 || math.Abs(a.AltBaro-groundFt) > 400) {
				t.Fatalf("implausible ground state: %+v", a)
			}

			switch a.Kind {
			case "overflight":
				if a.AltBaro < 28900 || a.AltBaro > 41100 {
					t.Fatalf("overflight at %.0f ft", a.AltBaro)
				}
				ovSamples++
				ovMinGS, ovMaxGS = math.Min(ovMinGS, a.GS), math.Max(ovMaxGS, a.GS)
				if a.GS >= 420 && a.GS <= 500 {
					ovInBand++
				}
				if a.GS < 385 || a.GS > 535 {
					t.Fatalf("overflight at %.0f kt", a.GS)
				}
			case "ga":
				if a.AltBaro-groundFt > 7000 || a.GS > 200 || a.Category[0] != 'A' && a.Category[0] != 'B' {
					t.Fatalf("GA aircraft: %+v", a)
				}
			case "helicopter":
				if a.AltBaro-groundFt > 3000 || a.GS > 165 || a.Category != "A7" {
					t.Fatalf("helicopter: %+v", a)
				}
			case "military":
				if a.ADSB || a.MLAT {
					t.Fatalf("military aircraft with a position source: %+v", a)
				}
			case "arrival":
				if l, ok := lowest[a.ICAO]; !ok || a.AltBaro < l {
					lowest[a.ICAO] = a.AltBaro
				}
			}
		}
	}

	stats := s.Stats()
	t.Logf("kinds %v", kinds)
	t.Logf("phases %v", phases)
	t.Logf("categories %v", cats)
	t.Logf("overflight GS %.0f..%.0f kt, %d of %d samples within 420-500 kt; spawned %d, now %d",
		ovMinGS, ovMaxGS, ovInBand, ovSamples, stats.Spawned, stats.Aircraft)

	for _, k := range kindNames {
		if kinds[k] == 0 {
			t.Errorf("no %s traffic", k)
		}
	}
	want := []string{"enroute", "climbout", "final", "taxi-out", "takeoff", "rollout", "taxi-in", "hover"}
	if !testing.Short() {
		want = append(want, "hold", "line-up", "parked")
	}
	for _, p := range want {
		if phases[p] == 0 {
			t.Errorf("flight phase %q never occurred", p)
		}
	}
	for _, c := range []string{"A1", "A3", "A5", "A7"} {
		if cats[c] == 0 {
			t.Errorf("no emitter category %s", c)
		}
	}
	if squawks["7000"] == 0 || squawks["1000"] == 0 || len(squawks) < 30 {
		t.Errorf("squawk variety: %d codes, 7000 x%d, 1000 x%d", len(squawks), squawks["7000"], squawks["1000"])
	}
	if squawks["7700"]+squawks["7600"]+squawks["7500"] != 0 {
		t.Error("emergency squawk although emergencies are disabled")
	}
	if float64(ovInBand) < 0.9*float64(ovSamples) {
		t.Errorf("only %d of %d overflight samples fly 420-500 kt", ovInBand, ovSamples)
	}
	// (the military share is checked on the spawn statistics in TestTrafficMix:
	// two hours contain too few individuals for a meaningful bound)
	if f := float64(kinds["military"]) / float64(total); f > 0.16 {
		t.Errorf("military share %.3f, want about 0.07", f)
	}
	if f := float64(nMLAT) / float64(nCivil); !testing.Short() && (f < 0.03 || f > 0.15) {
		t.Errorf("MLAT share %.3f, want about 0.08", f)
	}
	if nADSB+nMLAT != nCivil {
		t.Errorf("civil aircraft without ADS-B or MLAT: %d + %d != %d", nADSB, nMLAT, nCivil)
	}
	// traffic appears at the edge and leaves again
	gone, fresh := 0, 0
	now := map[uint32]bool{}
	for _, a := range s.Truth() {
		now[a.ICAO] = true
		if !first[a.ICAO] {
			fresh++
		}
	}
	for icao := range first {
		if !now[icao] {
			gone++
		}
	}
	if gone < len(first)/3 || fresh < len(now)/3 {
		t.Errorf("traffic does not turn over: %d of %d left, %d of %d new", gone, len(first), fresh, len(now))
	}
	if stats.Aircraft < 60 || stats.Aircraft > 104 {
		t.Errorf("%d aircraft, configured 80", stats.Aircraft)
	}
	// some arrivals made it all the way down to their airport
	landed := 0
	for _, l := range lowest {
		if l < groundFt+600 {
			landed++
		}
	}
	if landed == 0 {
		t.Error("no arrival reached the ground")
	}
}

func TestDiurnalTraffic(t *testing.T) {
	if d := trafficDensity(1.5); d < 0.15 || d > 0.25 {
		t.Errorf("night density %.2f", d)
	}
	for _, h := range []float64{8, 13, 18} {
		if d := trafficDensity(h); d < 0.85 || d > 1 {
			t.Errorf("density at %v h = %.2f", h, d)
		}
	}
	for h := 0.0; h < 24; h += 0.25 {
		if d := trafficDensity(h); d < 0.08 || d > 1 {
			t.Errorf("density(%v) = %v", h, d)
		}
	}
	if math.Abs(trafficDensity(0)-trafficDensity(23.999)) > 0.01 {
		t.Error("density curve is not continuous over midnight")
	}

	// 00:55 UTC is 01:29 local solar time at the default station
	night := New(Config{Aircraft: 100, Seed: 3, Start: time.Date(2026, 6, 1, 0, 55, 0, 0, time.UTC)})
	day := New(Config{Aircraft: 100, Seed: 3, Start: time.Date(2026, 6, 1, 7, 30, 0, 0, time.UTC)})
	flat := New(Config{Aircraft: 100, Seed: 3, Start: time.Date(2026, 6, 1, 0, 55, 0, 0, time.UTC), NoDiurnal: true})
	n, d, f := night.Stats(), day.Stats(), flat.Stats()
	if n.Aircraft < 12 || n.Aircraft > 26 || d.Aircraft < 90 || f.Aircraft != 100 {
		t.Errorf("aircraft at night %d, by day %d, without diurnal curve %d", n.Aircraft, d.Aircraft, f.Aircraft)
	}
	if n.TrafficDensity > 0.26 || d.TrafficDensity < 0.9 || f.TrafficDensity != 1 {
		t.Errorf("density night %.2f day %.2f flat %.2f", n.TrafficDensity, d.TrafficDensity, f.TrafficDensity)
	}

	// through the morning the traffic builds up
	s := New(Config{Aircraft: 60, Seed: 5, Start: time.Date(2026, 6, 1, 2, 0, 0, 0, time.UTC)})
	before := s.Stats().Aircraft
	if err := s.Advance(context.Background(), 5*time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	if after := s.Stats().Aircraft; after < 2*before || after < 45 {
		t.Errorf("traffic did not build up: %d -> %d", before, after)
	}
}

func TestPropagationVariesSlowly(t *testing.T) {
	s := New(Config{Aircraft: 1, Seed: 11, Start: testStart, NoStatusFrames: true})
	lo, hi := math.Inf(1), math.Inf(-1)
	prev := s.Stats().KFactor
	var maxStep float64
	for i := 0; i < 30*6; i++ { // 30 hours in 10 minute steps
		if err := s.Advance(context.Background(), 10*time.Minute, nil); err != nil {
			t.Fatal(err)
		}
		st := s.Stats()
		k := st.KFactor
		lo, hi = math.Min(lo, k), math.Max(hi, k)
		maxStep = math.Max(maxStep, math.Abs(k-prev))
		prev = k
		if st.QNH < 990 || st.QNH > 1040 || st.SurfaceWindKt < 0 || st.SurfaceWindKt > 40 {
			t.Fatalf("environment out of range: %+v", st)
		}
	}
	t.Logf("k-factor %.2f..%.2f over 30 hours, largest change in 10 min %.3f", lo, hi, maxStep)
	if lo < 0.9 || hi > 2.6 {
		t.Errorf("k-factor range %.2f..%.2f is not physical", lo, hi)
	}
	if hi-lo < 0.25 {
		t.Errorf("k-factor hardly varies: %.2f..%.2f", lo, hi)
	}
	if maxStep > 0.12 {
		t.Errorf("k-factor jumps by %.3f in ten minutes", maxStep)
	}
}

// TestLinkBudget checks the pieces of the reception model directly.
func TestLinkBudget(t *testing.T) {
	// free space loss at 1 km and 1090 MHz
	want := 20 * math.Log10(4*math.Pi*1000*1090e6/299792458)
	if math.Abs(fsplConst-want) > 0.01 {
		t.Errorf("FSPL constant %.3f, want %.3f", fsplConst, want)
	}

	yagi := newAntModel(model.Antenna{ID: "y", Type: model.AntYagi, Channel: 1, GainDBi: 12, HeightAGLM: 8,
		AzimuthDeg: 135, HBeamwidthDeg: 40, VBeamwidthDeg: 40, FrontToBackDB: 18, CableLossDB: 1.5}, 1)
	if yagi.omni || yagi.gain != 10.5 || yagi.channel != 1 || yagi.heightM != 8 {
		t.Errorf("yagi model: %+v", yagi)
	}
	if g := yagi.rel(135, 0); g < -0.1 || g > 0 {
		t.Errorf("boresight %.2f dB", g)
	}
	if g := yagi.rel(155, 0); math.Abs(g+3) > 0.25 {
		t.Errorf("half beamwidth off boresight %.2f dB, want -3", g)
	}
	if g := yagi.rel(135, 20); math.Abs(g+3) > 0.25 {
		t.Errorf("half vertical beamwidth %.2f dB, want -3", g)
	}
	if g := yagi.rel(315, 0); math.Abs(g+18) > 0.5 {
		t.Errorf("back lobe %.2f dB, want -18", g)
	}
	for b := 0.0; b < 360; b += 5 {
		if g := yagi.rel(b, 3); g > 0 || g < -18.5 {
			t.Errorf("pattern at %v deg = %.2f dB", b, g)
		}
	}
	omni := newAntModel(model.Antenna{ID: "v", Type: model.AntVertical, Channel: -1, GainDBi: 5, VBeamwidthDeg: 30}, 0)
	if !omni.omni || omni.channel != 0 || omni.heightM < 1 {
		t.Errorf("omni model: %+v", omni)
	}
	if omni.rel(10, 2) != omni.rel(250, 2) || math.Abs(omni.rel(0, 15)+3) > 0.25 || omni.rel(0, 80) > -15 {
		t.Errorf("omni pattern: %v %v %v", omni.rel(10, 2), omni.rel(0, 15), omni.rel(0, 80))
	}
	// a mast-head LNA lowers the noise floor
	lna := newAntModel(model.Antenna{Type: model.AntVertical, GainDBi: 5, LNAGainDB: 20, LNANoiseFigDB: 1}, 0)
	if lna.noise >= omni.noise {
		t.Errorf("LNA does not improve the noise floor: %v vs %v", lna.noise, omni.noise)
	}

	// diffraction: nothing inside, 3 dB at grazing, steep beyond
	if diffractionLoss(-60) != 0 || math.Abs(diffractionLoss(0)-3) > 1e-9 || diffractionLoss(40) < 30 {
		t.Errorf("diffraction loss %v %v %v", diffractionLoss(-60), diffractionLoss(0), diffractionLoss(40))
	}
	for x := -30.0; x < 80; x++ {
		if diffractionLoss(x+1) < diffractionLoss(x) {
			t.Errorf("diffraction loss not monotonic at %v km", x)
		}
	}

	// An airliner at FL350, 430 km out: beyond the horizon in a standard
	// atmosphere and below k = 1, comfortably inside under ducting conditions.
	s := New(Config{Aircraft: 1, Seed: 1, Start: testStart, NoDiurnal: true})
	a := &s.ac[0]
	a.lat, a.lon = destination(s.st.lat, s.st.lon, 0, 430)
	a.alt, a.onGround, a.shadow, a.roll, a.eirp = 35000, false, 0, 0, 54
	level := func(k float64) (float64, float64) {
		s.env.kFactor = k
		s.updateLink(a)
		return float64(a.link[0].mean), float64(a.link[1].mean)
	}
	v1, _ := level(1.0)
	v43, y43 := level(4.0 / 3)
	v2, y2 := level(2.0)
	if !(v1 < v43 && v43 < v2) || v2-v1 < 25 {
		t.Errorf("level at 430 km: k=1 %.1f, k=4/3 %.1f, k=2 %.1f dBFS", v1, v43, v2)
	}
	// with the default yagi pointing north the aircraft is on boresight:
	// 12 dBi against 5 dBi
	if d := y2 - v2; math.Abs(d-7) > 0.5 {
		t.Errorf("yagi advantage on boresight %.2f dB, want 7", d)
	}
	_ = y43
	// inside the horizon the level follows the free space law
	a.lat, a.lon = destination(s.st.lat, s.st.lon, 90, 100)
	n100, _ := level(4.0 / 3)
	a.lat, a.lon = destination(s.st.lat, s.st.lon, 90, 200)
	n200, _ := level(4.0 / 3)
	if d := n100 - n200; d < 5 || d > 9 {
		t.Errorf("doubling the distance costs %.2f dB, want about 6 (plus pattern)", d)
	}
	wantLevel := 54 - (fsplConst + 20*math.Log10(100.5)) + 5 - fullScaleDBm
	if math.Abs(n100-wantLevel) > 1.5 {
		t.Errorf("level at 100 km %.1f dBFS, link budget says %.1f", n100, wantLevel)
	}
	// an aircraft low over the horizon is cut off
	a.alt = 1500
	low, _ := level(4.0 / 3)
	if low > n200-30 {
		t.Errorf("1500 ft at 200 km is received at %.1f dBFS", low)
	}

	if softClip(5) > -0.4 || softClip(-3.2) != -3.2 || softClip(-1) > -1 {
		t.Errorf("soft clip: %v %v %v", softClip(5), softClip(-3.2), softClip(-1))
	}
}

// TestReceptionStatistics runs the full reception model and checks that the
// resulting frame statistics look physical.
func TestReceptionStatistics(t *testing.T) {
	minutes := 25
	if testing.Short() {
		minutes = 6
	}
	const yagiAz = 135.0
	ants := model.ExampleRadarcapeConfig().Antennas
	ants[1].AzimuthDeg = yagiAz
	s := New(Config{Aircraft: 110, Seed: 17, Start: testStart, NoDiurnal: true, Antennas: ants})
	st := s.Station()

	truth := map[uint32]Truth{}
	heard := map[uint32]time.Time{}
	var k float64
	var n [2]int
	var sigMin, sigMax = math.Inf(1), math.Inf(-1)
	var below38 int
	var nearSum, farSum float64
	var nearN, farN int
	var far [2]int      // frames from beyond 250 km
	var farFront [2]int // ... within 60 degrees of the yagi boresight
	var beam [2]int     // frames from beyond 250 km within 15 degrees of boresight
	var maxKm [2]float64
	var worstOver float64
	var mlat, mlatUnheard int
	hist := map[int]int{}
	sink := funcSink(func(f model.RawFrame) {
		if f.Kind == model.FrameStatus {
			return
		}
		var icao uint32
		switch df := refDF(f.Data); df {
		case 11, 17, 18:
			icao = refICAO(f.Data)
		default:
			icao = refRemainder(f.Data)
		}
		tr, ok := truth[icao]
		if f.MLAT {
			mlat++
			if last, was := heard[icao]; !was || f.Recv.Sub(last) > 16*time.Second {
				mlatUnheard++
			}
			return
		}
		heard[icao] = f.Recv
		if f.Channel < 0 || f.Channel > 1 || math.IsNaN(f.Signal) {
			t.Errorf("channel %d signal %v", f.Channel, f.Signal)
			return
		}
		ch := f.Channel
		n[ch]++
		sigMin, sigMax = math.Min(sigMin, f.Signal), math.Max(sigMax, f.Signal)
		hist[int(math.Floor(f.Signal/5))*5]++
		if f.Signal < -38 {
			below38++
		}
		if !ok {
			return // appeared within this second
		}
		d := tr.DistKm
		maxKm[ch] = math.Max(maxKm[ch], d)
		if ch == 0 {
			if d < 40 {
				nearSum += f.Signal
				nearN++
			} else if d > 300 {
				farSum += f.Signal
				farN++
			}
		}
		if d > 250 {
			far[ch]++
			off := angDiff(tr.Bearing, yagiAz)
			if off < 60 {
				farFront[ch]++
			}
			if off < 15 {
				beam[ch]++
			}
		}
		// radio horizon for the current k-factor
		ae := k * 6371.0
		ha := math.Max(tr.AltBaro*0.3048-st.GroundElevM, 2) / 1000
		horizon := math.Sqrt(2*ae*0.008) + math.Sqrt(2*ae*ha)
		worstOver = math.Max(worstOver, d-horizon)
	})

	ctx := context.Background()
	for i := 0; i < minutes*60; i++ {
		clear(truth)
		for _, tr := range s.Truth() {
			truth[tr.ICAO] = tr
		}
		k = s.Stats().KFactor
		if err := s.Advance(ctx, time.Second, sink); err != nil {
			t.Fatal(err)
		}
	}
	stats := s.Stats()
	t.Logf("%d transmissions, %d frames received: omni %d (max %.0f km), yagi %d (max %.0f km), %d MLAT results",
		stats.Transmitted, stats.Received, n[0], maxKm[0], n[1], maxKm[1], mlat)
	t.Logf("signal %.1f..%.1f dBFS, histogram (5 dB bins) %v", sigMin, sigMax, hist)
	t.Logf("omni mean level: %.1f dBFS inside 40 km, %.1f dBFS beyond 300 km", nearSum/float64(nearN), farSum/float64(farN))
	t.Logf("beyond 250 km: omni %d frames (%d within 60 deg of the yagi boresight), yagi %d (%d); in the beam: omni %d, yagi %d",
		far[0], farFront[0], far[1], farFront[1], beam[0], beam[1])
	t.Logf("farthest reception beyond the radio horizon: %.1f km", worstOver)

	if n[0] < 20000 || n[1] < 5000 {
		t.Fatalf("too few frames: %v", n)
	}
	// a plausible dBFS scale: strong signals near 0, the weakest just above the noise floor
	if sigMax > 0 || sigMax < -6 || sigMin > -33 || sigMin < -40 {
		t.Errorf("signal range %.1f..%.1f dBFS", sigMin, sigMax)
	}
	if below38 > (n[0]+n[1])/1000 {
		t.Errorf("%d frames reported below the noise floor", below38)
	}
	// losses: not everything is received, but most of what is in range is
	ratio := float64(n[0]+n[1]) / float64(2*stats.Transmitted)
	if ratio < 0.15 || ratio > 0.9 {
		t.Errorf("%.0f %% of all transmissions received per antenna", ratio*100)
	}
	// level falls with distance
	if nearN == 0 || farN == 0 || nearSum/float64(nearN)-farSum/float64(farN) < 15 {
		t.Errorf("near/far level difference too small (%d near, %d far frames)", nearN, farN)
	}
	// the radio horizon is a hard limit, softened only by diffraction
	if worstOver > 45 {
		t.Errorf("frame received %.0f km beyond the radio horizon", worstOver)
	}
	if maxKm[1] < 300 || maxKm[0] < 250 || maxKm[1] > DefaultRangeKm*1.1 {
		t.Errorf("maximum range omni %.0f km, yagi %.0f km", maxKm[0], maxKm[1])
	}
	// the yagi is directional, the vertical is not
	if far[1] == 0 || float64(farFront[1]) < 0.8*float64(far[1]) {
		t.Errorf("yagi: only %d of %d distant frames come from its front", farFront[1], far[1])
	}
	if f := float64(farFront[0]) / float64(far[0]); f < 0.12 || f > 0.6 {
		t.Errorf("omni: %.0f %% of distant frames come from the yagi's sector", f*100)
	}
	if beam[1] <= beam[0] {
		t.Errorf("inside its beam the yagi (%d frames) should out-receive the omni (%d)", beam[1], beam[0])
	}
	// MLAT results exist only for aircraft that are actually being received
	if mlat == 0 || mlatUnheard != 0 {
		t.Errorf("%d MLAT results, %d of them for aircraft not heard in the last 16 s", mlat, mlatUnheard)
	}
}

func TestEmergencies(t *testing.T) {
	// frequent emergencies when asked for
	s := New(Config{Aircraft: 60, Seed: 23, Start: testStart, NoDiurnal: true, EmergencyPerHour: 20})
	if err := s.Advance(context.Background(), 10*time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, tr := range s.Truth() {
		if tr.Emergency == "" {
			if tr.Squawk == "7700" || tr.Squawk == "7600" || tr.Squawk == "7500" {
				t.Errorf("%06X squawks %s without an emergency", tr.ICAO, tr.Squawk)
			}
			continue
		}
		n++
		want := map[string]string{"general": "7700", "medical": "7700", "minfuel": "7700", "nordo": "7600", "unlawful": "7500"}[tr.Emergency]
		if tr.Squawk != want {
			t.Errorf("%06X emergency %q squawk %s", tr.ICAO, tr.Emergency, tr.Squawk)
		}
	}
	if n < 5 {
		t.Errorf("only %d emergencies after 10 minutes at 20 per aircraft-hour", n)
	}

	// never when disabled, until one is triggered by hand
	s = New(Config{Aircraft: 60, Seed: 23, Start: testStart, NoDiurnal: true, EmergencyPerHour: -1})
	if err := s.Advance(context.Background(), 20*time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	for _, tr := range s.Truth() {
		if tr.Emergency != "" {
			t.Errorf("emergency %q although disabled", tr.Emergency)
		}
	}
	icao, ok := s.TriggerEmergency()
	if !ok {
		t.Fatal("TriggerEmergency failed")
	}
	tr, ok := s.TruthOf(icao, s.Now())
	if !ok || tr.Emergency != "general" || tr.Squawk != "7700" || tr.OnGround {
		t.Errorf("triggered emergency: %+v", tr)
	}
	icao2, ok := s.TriggerEmergency()
	if !ok || icao2 == icao {
		t.Errorf("second trigger picked %06X again (ok=%v)", icao2, ok)
	}
}

func TestAdvanceCancelAndCatchUp(t *testing.T) {
	s := New(Config{Aircraft: 20, Seed: 1, Start: testStart})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Advance(ctx, time.Hour, nil); err != context.Canceled {
		t.Errorf("Advance on a cancelled context: %v", err)
	}
	if !s.Now().Equal(testStart) {
		t.Errorf("cancelled Advance moved the clock to %v", s.Now())
	}

	s = New(Config{Aircraft: 30, Seed: 1, Start: time.Now().Add(-3 * time.Minute), NoDiurnal: true})
	frames := 0
	var last time.Time
	if err := s.CatchUp(context.Background(), funcSink(func(f model.RawFrame) { frames++; last = f.Recv })); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(s.Now()); d < -150*time.Millisecond || d > 2*time.Second {
		t.Errorf("after CatchUp the virtual clock is %v behind the wall clock", d)
	}
	if frames < 1000 || time.Since(last) > 3*time.Second {
		t.Errorf("CatchUp delivered %d frames, the last one %v ago", frames, time.Since(last))
	}
}

// TestRunPaced runs the pacing loop at 300x and checks delivery, ordering and
// shutdown.
func TestRunPaced(t *testing.T) {
	s := New(Config{Aircraft: 40, Seed: 5, Start: testStart, Speed: 300, NoDiurnal: true})
	var mu sync.Mutex
	var frames int
	var last time.Time
	ordered := true
	sink := funcSink(func(f model.RawFrame) {
		mu.Lock()
		defer mu.Unlock()
		frames++
		if f.Recv.Before(last) {
			ordered = false
		}
		last = f.Recv
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	go func() { done <- s.Run(ctx, sink) }()
	go func() { done <- s.Run(ctx, nil) }() // a second Run shares the same simulation
	time.Sleep(400 * time.Millisecond)
	cancel()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop after cancellation")
		}
	}
	mu.Lock()
	n, l := frames, last
	mu.Unlock()
	virtual := s.Now().Sub(testStart)
	t.Logf("%d frames, %v of virtual time in 0.4 s at 300x", n, virtual)
	if n == 0 || !ordered {
		t.Errorf("frames %d, ordered %v", n, ordered)
	}
	// 0.4 s at 300x is two virtual minutes; allow for slow machines
	if virtual < 20*time.Second || virtual > 4*time.Minute {
		t.Errorf("virtual time advanced %v", virtual)
	}
	if l.After(s.Now()) {
		t.Errorf("frame from the future: %v > %v", l, s.Now())
	}
	// stopped means stopped
	before := s.Now()
	time.Sleep(50 * time.Millisecond)
	if !s.Now().Equal(before) {
		t.Error("simulation keeps running after Run returned")
	}
	// and it can be started again
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- s.Run(ctx, sink) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	if !s.Now().After(before) {
		t.Error("simulation did not resume on the second Run")
	}
}

// TestRunLive: with the default configuration (no Start, speed 1) Recv is the
// current wall clock time, even when Run starts some time after New.
func TestRunLive(t *testing.T) {
	s := New(Config{Aircraft: 60, Seed: 9, NoDiurnal: true})
	time.Sleep(700 * time.Millisecond)
	var frames atomic.Int64
	var worstLag, worstLead atomic.Int64
	var badGPS atomic.Int64
	sink := funcSink(func(f model.RawFrame) {
		frames.Add(1)
		lag := int64(time.Since(f.Recv))
		if lag > worstLag.Load() {
			worstLag.Store(lag)
		}
		if -lag > worstLead.Load() {
			worstLead.Store(-lag)
		}
		if f.GPSNs >= 0 && !rxDelayOK(int64(f.Recv.Sub(f.Recv.Truncate(24*time.Hour)))-f.GPSNs) {
			badGPS.Add(1)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx, sink); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d frames in 0.6 s, Recv lags the wall clock by at most %v", frames.Load(), time.Duration(worstLag.Load()))
	if frames.Load() < 50 {
		t.Errorf("only %d frames in 0.6 s of real time", frames.Load())
	}
	if worstLead.Load() > int64(20*time.Millisecond) {
		t.Errorf("Recv is %v in the future", time.Duration(worstLead.Load()))
	}
	if worstLag.Load() > int64(500*time.Millisecond) {
		t.Errorf("Recv lags the wall clock by %v", time.Duration(worstLag.Load()))
	}
	if badGPS.Load() != 0 {
		t.Errorf("%d frames whose GPSNs is not shortly before the time of day of Recv", badGPS.Load())
	}
	if s.Stats().ClockSkipped < 500*time.Millisecond {
		t.Errorf("virtual clock was not moved onto the wall clock: %v", s.Stats().ClockSkipped)
	}
}

// TestStepDoesNotAllocate: generating a step must not allocate beyond the
// shared slab the frame data is carved from.
func TestStepDoesNotAllocate(t *testing.T) {
	s := New(Config{Aircraft: 100, Seed: 3, Start: testStart, NoDiurnal: true})
	ctx := context.Background()
	var n int
	sink := funcSink(func(f model.RawFrame) { n += len(f.Data) })
	if err := s.Advance(ctx, time.Minute, sink); err != nil { // warm up the buffers
		t.Fatal(err)
	}
	frames := s.Stats().Received
	const runs = 600
	allocs := testing.AllocsPerRun(runs, func() {
		s.Advance(ctx, 100*time.Millisecond, sink)
	})
	perStep := float64(s.Stats().Received-frames) / (runs + 1)
	t.Logf("%.2f allocations and %.0f frames per 100 ms step", allocs, perStep)
	if allocs > 2 {
		t.Errorf("%.1f allocations per step", allocs)
	}
	if perStep < 10 {
		t.Errorf("only %.1f frames per step", perStep)
	}
}

func BenchmarkStep100Aircraft(b *testing.B) {
	s := New(Config{Aircraft: 100, Seed: 3, Start: testStart, NoDiurnal: true})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Advance(ctx, 100*time.Millisecond, nil)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e8*100, "%cpu-realtime")
}

// TestRunStopsDelivering: once Run has returned its sink gets nothing more,
// even though the shared simulation keeps running for another output.
func TestRunStopsDelivering(t *testing.T) {
	s := New(Config{Aircraft: 40, Seed: 6, Start: testStart, Speed: 200, NoDiurnal: true})
	var a, b atomic.Int64
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	doneA := make(chan error, 1)
	doneB := make(chan error, 1)
	go func() { doneA <- s.Run(ctxA, funcSink(func(model.RawFrame) { a.Add(1) })) }()
	go func() { doneB <- s.Run(ctxB, funcSink(func(model.RawFrame) { b.Add(1) })) }()
	waitFor(t, "frames on both sinks", func() bool { return a.Load() > 100 && b.Load() > 100 })
	cancelA()
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
	na, nb := a.Load(), b.Load()
	waitFor(t, "the other sink to keep receiving", func() bool { return b.Load() > nb+100 })
	if got := a.Load(); got != na {
		t.Errorf("sink received %d frames after its Run returned", got-na)
	}
	cancelB()
	if err := <-doneB; err != nil {
		t.Fatal(err)
	}
}

// TestTrafficMix checks the spawn statistics by kind and equipage.
func TestTrafficMix(t *testing.T) {
	mix := func(cfg Config) (byKind [numKinds]float64, mlat, gnss, adsb float64) {
		cfg.Seed, cfg.Start, cfg.NoDiurnal, cfg.Aircraft = 13, testStart, true, 10
		s := New(cfg)
		const n = 40000
		civil := 0.0
		for range n {
			var a aircraft
			a.kind = s.randomKind()
			s.identify(&a)
			delete(s.used, a.icao)
			byKind[a.kind] += 1.0 / n
			if a.kind != kMil {
				civil++
				if a.mlat {
					mlat++
				}
				if a.gnssPos {
					gnss++
				}
				if a.adsb {
					adsb++
				}
			} else if a.adsb || a.mlat {
				t.Fatalf("military aircraft with ADS-B or MLAT")
			}
		}
		return byKind, mlat / civil, gnss / civil, adsb / civil
	}

	k, mlat, gnss, adsb := mix(Config{})
	t.Logf("spawn mix %.3f, MLAT %.3f, GNSS altitude %.3f, ADS-B %.3f", k, mlat, gnss, adsb)
	// Military aircraft stay about 1.5 times as long as civil ones, so a 7 %
	// share of the picture needs about 4.5 % of the spawns (the long-run
	// share was measured over several simulated days when this was tuned).
	if k[kMil] < 0.037 || k[kMil] > 0.053 {
		t.Errorf("military spawn share %.3f", k[kMil])
	}
	if k[kOverflight] < 0.4 || k[kArrival] < 0.1 || k[kDeparture] < 0.1 || k[kGA] < 0.05 || k[kHeli] < 0.02 {
		t.Errorf("spawn mix %.3f", k)
	}
	if math.Abs(mlat-0.08) > 0.01 || math.Abs(gnss-0.04*0.92) > 0.01 || math.Abs(adsb+mlat-1) > 1e-9 {
		t.Errorf("MLAT %.3f, GNSS altitude %.3f, ADS-B %.3f", mlat, gnss, adsb)
	}

	k, mlat, gnss, _ = mix(Config{MilitaryFraction: -1, MLATFraction: -1, GNSSAltFraction: -1})
	if k[kMil] != 0 || mlat != 0 || gnss != 0 {
		t.Errorf("disabled fractions: military %.3f MLAT %.3f GNSS %.3f", k[kMil], mlat, gnss)
	}
	k, mlat, _, _ = mix(Config{MilitaryFraction: 0.5, MLATFraction: 0.3})
	if k[kMil] < 0.3 || k[kMil] > 0.45 || math.Abs(mlat-0.3) > 0.02 {
		t.Errorf("raised fractions: military %.3f MLAT %.3f", k[kMil], mlat)
	}
}
