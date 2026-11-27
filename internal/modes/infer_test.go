package modes

import (
	"math"
	"math/rand"
	"testing"
)

func candidates(m *Message) map[BDS]float32 {
	out := map[BDS]float32{}
	for _, c := range m.BDS[:m.NumBDS] {
		out[c.BDS] = c.Confidence
	}
	return out
}

func TestInferBDSKnownAnswers(t *testing.T) {
	for _, c := range []struct {
		frame string
		want  BDS
	}{
		// Self-generated replies of each register.
		{ownBDS10, BDS10},
		{ownBDS10v5, BDS10},
		{fhex(commBFrame(20, ac13For(41000), 0x4D2B3C, bds10MB(ownBDS10b))), BDS10},
		{ownBDS20, BDS20},
		{ownBDS20b, BDS20},
		{ownBDS20c, BDS20},
		{ownBDS40, BDS40},
		{ownBDS40b, BDS40},
		{ownBDS50, BDS50},
		{fhex(commBFrame(20, ac13For(36000), 0x3C5EF1, bds50MB(-0.3515625, 31.640625, 462, -0.03125, 470))), BDS50},
		{ownBDS60, BDS60},
		// The 1090 MHz Riddle: the inference chapter's "should be BDS60" and
		// the register examples of the EHS, ELS and meteorological chapters.
		{bookInference, BDS60},
		{bookBDS17, BDS17},
		{bookBDS20, BDS20},
		{bookBDS40, BDS40},
		{bookBDS50, BDS50},
		{bookBDS60, BDS60},
		{bookBDS44, BDS44},
	} {
		m := mustDecode(t, c.frame)
		if !m.Has(FCommB) || m.NumBDS == 0 {
			t.Errorf("%s: no candidates", c.frame)
			continue
		}
		b, conf, ok := m.BestBDS()
		if !ok || b != c.want || conf < 0.5 {
			t.Errorf("%s: BestBDS = %v (%.2f, ok=%v), want %v; candidates %v", c.frame, b, conf, ok, c.want, m.BDS[:m.NumBDS])
		}
		if m.BDS[0].BDS != b || m.BDS[0].Confidence != conf {
			t.Errorf("%s: BestBDS is not the first candidate", c.frame)
		}
		// Candidates are sorted best first and are all distinct.
		seen := map[BDS]bool{}
		for i, cand := range m.BDS[:m.NumBDS] {
			if i > 0 && cand.Confidence > m.BDS[i-1].Confidence {
				t.Errorf("%s: candidates not sorted: %v", c.frame, m.BDS[:m.NumBDS])
			}
			if seen[cand.BDS] || cand.Confidence <= 0 || cand.Confidence > 1 {
				t.Errorf("%s: bad candidate list %v", c.frame, m.BDS[:m.NumBDS])
			}
			seen[cand.BDS] = true
		}
		// The stand-alone function gives the same answer.
		alt := math.NaN()
		if m.Has(FAltBaro) {
			alt = float64(m.AltBaro)
		}
		list, n := InferBDS(m.MB(), alt)
		if n != int(m.NumBDS) || list != m.BDS {
			t.Errorf("%s: InferBDS disagrees with Decode", c.frame)
		}
	}
}

func TestInferBDSUnknownAndEmpty(t *testing.T) {
	// Self-generated payloads that are no supported register, or empty.
	for _, frame := range []string{
		fhex(commBFrame(21, EncodeID13(0x6246), 0xA95E01, 0x15EE315463718B)),
		fhex(commBFrame(20, ac13For(30000), 0x27ED41, 0x17826515BEBD01)),
		fhex(commBFrame(20, ac13For(37000), 0x4CA9F0, 0)),
		fhex(commBFrame(20, ac13For(32300), 0x4B8E21, 0)),
	} {
		m := mustDecode(t, frame)
		if b, conf, ok := m.BestBDS(); ok {
			t.Errorf("%s: BestBDS = %v (%.2f), want none; candidates %v", frame, b, conf, m.BDS[:m.NumBDS])
		}
		if b, _, ok := DisambiguateBDS(&m, 420, 90, 35000); ok {
			t.Errorf("%s: DisambiguateBDS = %v, want none", frame, b)
		}
	}
	if _, n := InferBDS(0, 35000); n != 0 {
		t.Error("an all-zero payload has no candidates")
	}
	// Not a Comm-B reply.
	m := mustDecode(t, bookPosEven)
	if b, _, ok := DisambiguateBDS(&m, 420, 90, 38000); ok || b != BDSNone {
		t.Errorf("DisambiguateBDS on DF17 = %v, %v", b, ok)
	}
	if _, n := RankBDS(&m, 420, 90, 38000); n != 0 {
		t.Error("RankBDS on DF17 returned candidates")
	}
	// NoCommB skips the inference but still flags the payload.
	var q Message
	if err := DecodeOpts(hexFrame(t, ownBDS50), &q, Options{NoCommB: true}); err != nil {
		t.Fatal(err)
	}
	if !q.Has(FCommB) || q.NumBDS != 0 || q.AltBaro != 33000 {
		t.Errorf("NoCommB: commb=%v n=%d alt=%d", q.Has(FCommB), q.NumBDS, q.AltBaro)
	}
	if b, _, ok := DisambiguateBDS(&q, math.NaN(), math.NaN(), math.NaN()); !ok || b != BDS50 {
		t.Errorf("DisambiguateBDS after NoCommB = %v, %v", b, ok)
	}
}

func TestDisambiguate50or60(t *testing.T) {
	// Self-generated payloads that satisfy the BDS 5,0 and the BDS 6,0
	// rules; with ADS-B state matching their 5,0 reading they are BDS 5,0.
	for _, s := range []struct {
		mb      uint64
		gs, trk float64
	}{{ownAmbig1, 336, 240.5}, {ownAmbig2, 350, 221}} {
		st := s
		frame := fhex(commBFrame(21, EncodeID13(0x5402), 0x3C7A90, st.mb))
		m := mustDecode(t, frame)
		c := candidates(&m)
		if c[BDS50] == 0 || c[BDS60] == 0 {
			t.Errorf("%s: want both 5,0 and 6,0 as candidates, got %v", frame, m.BDS[:m.NumBDS])
		}
		b, conf, ok := DisambiguateBDS(&m, st.gs, st.trk, 14000)
		if !ok || b != BDS50 || conf < 0.8 {
			t.Errorf("%s: DisambiguateBDS = %v (%.2f, ok=%v), want 5,0", frame, b, conf, ok)
		}
		ranked, n := RankBDS(&m, st.gs, st.trk, 14000)
		if n == 0 || ranked[0].BDS != BDS50 {
			t.Errorf("%s: RankBDS = %v", frame, ranked[:n])
		}
		for _, r := range ranked[:n] {
			if r.BDS == BDS60 && ranked[0].Confidence-r.Confidence < bdsAcceptMargin {
				t.Errorf("%s: 6,0 still too close: %v", frame, ranked[:n])
			}
		}
		// The message itself is never modified by the helpers.
		before := m
		DisambiguateBDS(&m, 100, 10, 40000)
		if m != before {
			t.Errorf("%s: DisambiguateBDS modified the message", frame)
		}
		// State that contradicts the 5,0 reading (aircraft flying the other
		// way, much slower) must not confirm it.
		if b, _, ok := DisambiguateBDS(&m, 150, st.trk-180, 14000); ok && b == BDS50 {
			t.Errorf("%s: 5,0 confirmed against contradicting state", frame)
		}
	}
	// A BDS 2,0 payload is neither 5,0 nor 6,0 and stays 2,0.
	m := mustDecode(t, ownBDS20c)
	b, _, ok := DisambiguateBDS(&m, 0, 0, 0)
	if !ok || b != BDS20 {
		t.Errorf("BDS 2,0 payload: %v, %v", b, ok)
	}
	// The ambiguous payload in a DF20 at 3000 ft.
	m = mustDecode(t, fhex(commBFrame(20, ac13For(3000), 0x4243D0, ownAmbig3)))
	v := DecodeBDS50(m.MB())
	if b, _, ok := DisambiguateBDS(&m, v.GS, v.Track, math.NaN()); !ok || b != BDS50 {
		t.Errorf("ownAmbig3 at 3000 ft: %v, %v", b, ok)
	}
}

func TestInferenceHardRules(t *testing.T) {
	score := func(mb uint64, alt float64) map[BDS]float32 {
		list, n := InferBDS(mb, alt)
		out := map[BDS]float32{}
		for _, c := range list[:n] {
			out[c.BDS] = c.Confidence
		}
		return out
	}
	nan := math.NaN()

	// BDS 1,0: register number and reserved bits 10-14.
	mb10 := bds10MB(ownBDS10b)
	if score(mb10, nan)[BDS10] < 0.9 {
		t.Error("1,0 baseline")
	}
	for bit := uint(10); bit <= 14; bit++ {
		if score(setBits(mb10, bit, bit, 1), nan)[BDS10] != 0 {
			t.Errorf("1,0 with reserved bit %d set accepted", bit)
		}
	}
	if score(mb10^1<<48, nan)[BDS10] != 0 {
		t.Error("1,0 with wrong register number accepted")
	}
	// Overlay capability needs subnetwork version >= 5.
	if got := score(setBits(mb10, 17, 23, 3), nan)[BDS10]; got == 0 || got >= 0.9 {
		t.Errorf("1,0 overlay/version conflict: confidence %v", got)
	}
	// Subnetwork versions: 0..5 assigned, a few more tolerated, the rest not.
	if got := score(setBits(mb10, 17, 23, 7), nan)[BDS10]; got == 0 || got >= 0.7 {
		t.Errorf("1,0 with subnetwork version 7: confidence %v", got)
	}
	if score(setBits(mb10, 17, 23, 100), nan)[BDS10] != 0 {
		t.Error("1,0 with subnetwork version 100 accepted")
	}

	// BDS 1,7: bit 7 (BDS 2,0) mandatory, bits 29-56 reserved.
	mb17 := mbOf(t, bookBDS17)
	if score(mb17, nan)[BDS17] < 0.8 {
		t.Error("1,7 baseline")
	}
	if score(setBits(mb17, 7, 7, 0), nan)[BDS17] != 0 {
		t.Error("1,7 without BDS 2,0 capability accepted")
	}
	for _, bit := range []uint{29, 40, 56} {
		if score(setBits(mb17, bit, bit, 1), nan)[BDS17] != 0 {
			t.Errorf("1,7 with reserved bit %d set accepted", bit)
		}
	}

	// BDS 2,0: only legal characters.
	mb20 := mbOf(t, bookBDS20)
	if score(mb20, nan)[BDS20] < 0.9 {
		t.Error("2,0 baseline")
	}
	if score(setBits(mb20, 9, 14, 27), nan)[BDS20] != 0 { // '['
		t.Error("2,0 with an illegal character accepted")
	}
	if got := score(0x20<<48|0x820820820820, nan)[BDS20]; got == 0 || got > 0.5 {
		t.Errorf("2,0 with an empty callsign: confidence %v", got)
	}
	if got := score(setBits(mb20, 21, 26, 32), nan)[BDS20]; got == 0 || got >= 0.9 { // embedded space
		t.Errorf("2,0 with an embedded space: confidence %v", got)
	}

	// BDS 3,0: threat type 3 and ACAS III bits.
	mb30 := uint64(0x30)<<48 | 0b11<<46
	if score(mb30, nan)[BDS30] < 0.8 {
		t.Error("3,0 baseline")
	}
	if score(setBits(mb30, 29, 30, 3), nan)[BDS30] != 0 {
		t.Error("3,0 with threat type 3 accepted")
	}
	if score(setBits(mb30, 16, 22, 48), nan)[BDS30] != 0 {
		t.Error("3,0 with ACAS III bits accepted")
	}
	// An idle report (no advisory) is the most common real BDS 3,0 content.
	if score(0x30<<48, nan)[BDS30] < 0.8 {
		t.Error("idle 3,0 report rejected")
	}
	// Threat identity data must fit the threat type.
	withAddr := setBits(setBits(mb30, 29, 30, 1), 31, 54, 0x4840D6)
	if score(withAddr, nan)[BDS30] < 0.8 {
		t.Error("3,0 with a threat address rejected")
	}
	if score(setBits(withAddr, 56, 56, 1), nan)[BDS30] != 0 {
		t.Error("3,0 threat address with unused bits set accepted")
	}
	if got := score(setBits(mb30, 31, 54, 0x4840D6), nan)[BDS30]; got >= 0.5 {
		t.Errorf("3,0 identity data without threat type: confidence %v", got)
	}
	withPos := setBits(setBits(setBits(mb30, 29, 30, 2), 31, 43, uint64(ac13For(9000))), 51, 56, 60)
	if score(withPos, nan)[BDS30] < 0.8 {
		t.Error("3,0 with threat altitude/bearing rejected")
	}
	if score(setBits(withPos, 51, 56, 61), nan)[BDS30] != 0 {
		t.Error("3,0 with an unassigned threat bearing accepted")
	}
	if score(setBits(withPos, 31, 43, 0x0040), nan)[BDS30] != 0 {
		t.Error("3,0 with a metric threat altitude accepted")
	}
	// ARA detail bits without any advisory, unless the RA just terminated.
	stray := uint64(0x30)<<48 | 0b0101<<44
	if got := score(stray, nan)[BDS30]; got >= 0.5 {
		t.Errorf("3,0 ARA detail without advisory: confidence %v", got)
	}
	if score(setBits(stray, 27, 27, 1), nan)[BDS30] < 0.8 {
		t.Error("3,0 terminated advisory rejected")
	}

	// BDS 4,0: status bits and reserved fields.
	mb40 := bds40MB(36000, 36000, 1013.2)
	if score(mb40, nan)[BDS40] < 0.8 {
		t.Errorf("4,0 baseline: %v", score(mb40, nan))
	}
	for _, c := range []struct {
		name string
		mb   uint64
	}{
		{"MCP data without status", setBits(mb40, 1, 1, 0)},
		{"FMS data without status", setBits(mb40, 14, 14, 0)},
		{"baro data without status", setBits(mb40, 27, 27, 0)},
		{"reserved 40-47", setBits(mb40, 43, 43, 1)},
		{"reserved 52-53", setBits(mb40, 52, 52, 1)},
		{"mode bits without status", setBits(mb40, 49, 49, 1)},
		{"source without status", setBits(mb40, 56, 56, 1)},
	} {
		if score(c.mb, nan)[BDS40] != 0 {
			t.Errorf("4,0 %s accepted", c.name)
		}
	}
	// Mode bits alone are not enough evidence; implausible values are weak.
	if score(setBits(0, 48, 51, 0b1100), nan)[BDS40] != 0 {
		t.Error("4,0 with mode bits only accepted")
	}
	if got := score(bds40MB(64000, -1, -1), nan)[BDS40]; got >= 0.5 {
		t.Errorf("4,0 with 64000 ft selected: confidence %v", got)
	}
	if got := score(bds40MB(-1, -1, 1190), nan)[BDS40]; got >= 0.5 {
		t.Errorf("4,0 with 1190 hPa only: confidence %v", got)
	}

	// BDS 5,0: status bits and value ranges.
	mb50 := bds50MB(2, 114, 438, 0.125, 424)
	if score(mb50, nan)[BDS50] < 0.8 {
		t.Errorf("5,0 baseline: %v", score(mb50, nan))
	}
	for _, c := range []struct {
		name string
		mb   uint64
	}{
		{"roll data without status", setBits(mb50, 1, 1, 0)},
		{"track data without status", setBits(mb50, 12, 12, 0)},
		{"GS data without status", setBits(mb50, 24, 24, 0)},
		{"rate data without status", setBits(mb50, 35, 35, 0)},
		{"TAS data without status", setBits(mb50, 46, 46, 0)},
		{"roll 60 deg", bds50MB(60, 114, 438, 0.125, 424)},
		{"roll -60 deg", bds50MB(-60, 114, 438, 0.125, 424)},
		{"GS 700 kt", bds50MB(2, 114, 700, 0.125, 600)},
		{"TAS 700 kt", bds50MB(2, 114, 560, 0.125, 700)},
		{"GS and TAS 250 kt apart", bds50MB(2, 114, 150, 0.125, 400)},
	} {
		if score(c.mb, nan)[BDS50] != 0 {
			t.Errorf("5,0 %s accepted", c.name)
		}
	}

	// BDS 6,0: status bits and value ranges.
	mb60 := bds60MB(110, 259, 0.70, -2144, -2016)
	if score(mb60, nan)[BDS60] < 0.8 {
		t.Errorf("6,0 baseline: %v", score(mb60, nan))
	}
	for _, c := range []struct {
		name string
		mb   uint64
	}{
		{"heading data without status", setBits(mb60, 1, 1, 0)},
		{"IAS data without status", setBits(mb60, 13, 13, 0)},
		{"Mach data without status", setBits(mb60, 24, 24, 0)},
		{"baro rate data without status", setBits(mb60, 35, 35, 0)},
		{"inertial rate data without status", setBits(mb60, 46, 46, 0)},
		{"IAS 520 kt", bds60MB(110, 520, 0.95, 0, 0)},
		{"Mach 1.2", bds60MB(110, 400, 1.2, 0, 0)},
		{"baro rate 8000", bds60MB(110, 259, 0.70, 8000, 100)},
		{"inertial rate -8000", bds60MB(110, 259, 0.70, 100, -8000)},
		{"IAS/Mach impossible at any altitude", bds60MB(110, 120, 0.95, 0, 0)},
	} {
		if score(c.mb, nan)[BDS60] != 0 {
			t.Errorf("6,0 %s accepted", c.name)
		}
	}
	// The altitude makes the Mach/IAS test sharp: Mach 0.70
	// with IAS 259 kt is right around 30000 ft and wrong at 10000 ft.
	if score(mb60, 30000)[BDS60] < score(mb60, nan)[BDS60] {
		t.Error("6,0 consistent altitude lowered the confidence")
	}
	if score(mb60, 10000)[BDS60] != 0 {
		t.Error("6,0 with IAS/Mach inconsistent at the known altitude accepted")
	}

	// BDS 4,4: figure of merit, status bits, value ranges.
	mb44 := mbOf(t, bookBDS44)
	if got := score(mb44, 35050)[BDS44]; got < 0.7 {
		t.Errorf("4,4 baseline at altitude: %v", got)
	}
	if got := score(mb44, nan)[BDS44]; got == 0 || got >= 0.5 {
		t.Errorf("4,4 without altitude must stay below the acceptance threshold: %v", got)
	}
	if got := score(mb44, 2000)[BDS44]; got >= 0.5 {
		t.Errorf("4,4 with -48.75 C at 2000 ft: confidence %v", got)
	}
	for _, c := range []struct {
		name string
		mb   uint64
	}{
		{"FOM 5", setBits(mb44, 1, 4, 5)},
		{"wind without status", setBits(mb44, 5, 5, 0)},
		{"wind 300 kt", setBits(mb44, 6, 14, 300)},
		{"temperature +70 C", setBits(mb44, 24, 34, 280)},
		{"temperature -90 C", setBits(mb44, 24, 34, 2048-360)},
		{"pressure data without status", setBits(mb44, 36, 46, 250)},
		{"turbulence data without status", setBits(mb44, 48, 49, 1)},
		{"humidity data without status", setBits(mb44, 51, 56, 10)},
	} {
		if score(c.mb, 35050)[BDS44] != 0 {
			t.Errorf("4,4 %s accepted", c.name)
		}
	}

	// BDS 4,5: status bits, reserved bits, temperature range.
	mb45 := mbOf(t, bookBDS45)
	if got := score(mb45, 35050)[BDS45]; got < 0.5 {
		t.Errorf("4,5 baseline at altitude: %v", got)
	}
	if got := score(mb45, nan)[BDS45]; got == 0 || got >= 0.5 {
		t.Errorf("4,5 without altitude must stay below the acceptance threshold: %v", got)
	}
	for _, c := range []struct {
		name string
		mb   uint64
	}{
		{"reserved 52-56", setBits(mb45, 56, 56, 1)},
		{"turbulence data without status", setBits(mb45, 2, 3, 1)},
		{"icing data without status", setBits(mb45, 11, 12, 2)},
		{"temperature without status", setBits(mb45, 16, 16, 0)},
		{"temperature +100 C", setBits(mb45, 17, 26, 400)},
		{"pressure data without status", setBits(mb45, 28, 38, 300)},
		{"radio height data without status", setBits(mb45, 40, 51, 100)},
	} {
		if score(c.mb, 35050)[BDS45] != 0 {
			t.Errorf("4,5 %s accepted", c.name)
		}
	}
}

// simAircraft is a random but physically consistent aircraft state.
type simAircraft struct {
	alt                       float64 // ft
	mach, ias, tas            float64
	heading, magHeading       float64
	gs, track                 float64
	roll, trackRate, vertRate float64
}

func randomAircraft(rng *rand.Rand) simAircraft {
	var a simAircraft
	a.alt = 3000 + rng.Float64()*38000
	// Typical speed schedule: 250 kt below 10000 ft, then up to Mach 0.82.
	if a.alt < 10000 {
		a.ias = 180 + rng.Float64()*70
		a.mach = casToMach(a.ias, a.alt)
	} else {
		a.mach = 0.55 + rng.Float64()*0.27
		a.ias = machToCAS(a.mach, a.alt)
		if a.ias > 340 {
			a.ias = 340
			a.mach = casToMach(a.ias, a.alt)
		}
	}
	// Non-standard temperature of up to +-15 K.
	a.tas = machToTAS(a.mach, a.alt) * math.Sqrt(1+(rng.Float64()*30-15)/250)
	a.heading = rng.Float64() * 360
	a.magHeading = math.Mod(a.heading+rng.Float64()*30-15+360, 360) // magnetic variation
	windSpeed := rng.Float64() * 90
	windTo := rng.Float64() * 360
	sh, ch := math.Sincos(a.heading * math.Pi / 180)
	sw, cw := math.Sincos(windTo * math.Pi / 180)
	vx, vy := a.tas*sh+windSpeed*sw, a.tas*ch+windSpeed*cw
	a.gs = math.Hypot(vx, vy)
	a.track = math.Mod(math.Atan2(vx, vy)*180/math.Pi+360, 360)
	if rng.Intn(4) == 0 { // turning
		a.roll = rng.Float64()*50 - 25
	} else {
		a.roll = rng.Float64()*2 - 1
	}
	a.trackRate = 1091 * math.Tan(a.roll*math.Pi/180) / a.tas
	if rng.Intn(3) == 0 {
		a.vertRate = rng.Float64()*5000 - 2500
	}
	return a
}

// TestInferenceSimulation feeds realistic BDS 4,0 / 5,0 / 6,0 replies through
// the decoder. A wrong register must never be accepted, and the right one
// must be found nearly always once the tracker state is supplied.
func TestInferenceSimulation(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	const n = 6000
	type tally struct{ right, undecided, wrong int }
	var alone, withState [3]tally
	names := [3]string{"4,0", "5,0", "6,0"}
	for i := 0; i < n; i++ {
		a := randomAircraft(rng)
		kind := i % 3
		var mb uint64
		var want BDS
		switch kind {
		case 0:
			sel := math.Round((a.alt+rng.Float64()*8000-4000)/100) * 100
			if sel < 1000 {
				sel = 1000
			}
			mb, want = bds40MB(sel, sel, 990+math.Round(rng.Float64()*400)/10), BDS40
			if rng.Intn(2) == 0 {
				mb = bds40MB(sel, -1, 1013.2)
			}
		case 1:
			mb, want = bds50MB(a.roll, a.track, a.gs, a.trackRate, a.tas), BDS50
		case 2:
			mb, want = bds60MB(a.magHeading, a.ias, a.mach, a.vertRate, a.vertRate+rng.Float64()*400-200), BDS60
		}
		// Alternate between altitude replies (DF20) and identity replies
		// (DF21, no altitude in the message).
		var frame []byte
		if i%2 == 0 {
			frame = commBFrame(20, ac13For(int32(a.alt/25)*25), 0x4840D6, mb)
		} else {
			frame = commBFrame(21, id13FromModeA(0x2345), 0x4840D6, mb)
		}
		var m Message
		if err := Decode(frame, &m); err != nil {
			t.Fatal(err)
		}
		count := func(tl *tally, b BDS, ok bool) {
			switch {
			case !ok:
				tl.undecided++
			case b == want:
				tl.right++
			default:
				tl.wrong++
				t.Errorf("%s payload %014X (state %+v) accepted as %v; candidates %v", names[kind], mb, a, b, m.BDS[:m.NumBDS])
			}
		}
		b, _, ok := m.BestBDS()
		count(&alone[kind], b, ok)
		// Tracker state as it would come from ADS-B: slightly stale.
		gs := a.gs + rng.Float64()*10 - 5
		trk := math.Mod(a.track+rng.Float64()*4-2+360, 360)
		b, _, ok = DisambiguateBDS(&m, gs, trk, a.alt)
		count(&withState[kind], b, ok)
		if got := candidates(&m)[want]; got == 0 {
			t.Errorf("%s payload %014X (state %+v): true register is not even a candidate: %v", names[kind], mb, a, m.BDS[:m.NumBDS])
		}
	}
	for k := range names {
		t.Logf("BDS %s: message alone right=%d undecided=%d wrong=%d; with state right=%d undecided=%d wrong=%d",
			names[k], alone[k].right, alone[k].undecided, alone[k].wrong, withState[k].right, withState[k].undecided, withState[k].wrong)
		if min := n / 3 * 97 / 100; withState[k].right < min {
			t.Errorf("BDS %s: only %d of %d recognised with tracker state", names[k], withState[k].right, n/3)
		}
		if min := n / 3 * 85 / 100; alone[k].right < min {
			t.Errorf("BDS %s: only %d of %d recognised from the message alone", names[k], alone[k].right, n/3)
		}
	}
}

// TestInferenceRandomPayloads: noise (or registers this package does not
// know) must almost never be mistaken for a supported register.
func TestInferenceRandomPayloads(t *testing.T) {
	rng := rand.New(rand.NewSource(10))
	const n = 200000
	accepted, acceptedWithState := 0, 0
	for i := 0; i < n; i++ {
		mb := rng.Uint64() & (1<<56 - 1)
		alt := math.NaN()
		if i%2 == 0 {
			alt = 1000 + rng.Float64()*40000
		}
		list, cnt := InferBDS(mb, alt)
		if _, _, ok := pickBDS(&list, cnt); ok {
			accepted++
		}
		var m Message
		m.Valid = FCommB
		m.Len = 14
		for j := 0; j < 7; j++ {
			m.Raw[4+j] = byte(mb >> (48 - 8*uint(j)))
		}
		if _, _, ok := DisambiguateBDS(&m, 150+rng.Float64()*350, rng.Float64()*360, 1000+rng.Float64()*40000); ok {
			acceptedWithState++
		}
	}
	t.Logf("random payloads accepted: %d of %d from the message alone, %d with tracker state", accepted, n, acceptedWithState)
	if accepted > n/250 {
		t.Errorf("%d of %d random payloads accepted (> 0.4%%)", accepted, n)
	}
	if acceptedWithState > n/500 {
		t.Errorf("%d of %d random payloads accepted with tracker state (> 0.2%%)", acceptedWithState, n)
	}
}

func TestStandardAtmosphereHelpers(t *testing.T) {
	// Published ISA values: 11000 m = 36089 ft: 216.65 K, 226.32 hPa;
	// FL350: 238.4 hPa, -54.3 C. Mach 0.78 at FL350 = 266 kt CAS, 450 kt TAS.
	if got := isaTempC(0); !near(got, 15, 0.01) {
		t.Errorf("ISA temperature at sea level = %v", got)
	}
	if got := isaTempC(35000); !near(got, -54.3, 0.1) {
		t.Errorf("ISA temperature at FL350 = %v", got)
	}
	if got := isaTempC(45000); !near(got, -56.5, 0.1) {
		t.Errorf("ISA temperature at FL450 = %v", got)
	}
	if got := 1013.25 * isaDelta(35000); !near(got, 238.4, 0.3) {
		t.Errorf("ISA pressure at FL350 = %v", got)
	}
	if got := 1013.25 * isaDelta(36089.24); !near(got, 226.32, 0.2) {
		t.Errorf("ISA pressure at the tropopause = %v", got)
	}
	if got := 1013.25 * isaDelta(45000); !near(got, 147.5, 0.5) {
		t.Errorf("ISA pressure at FL450 = %v", got)
	}
	if got := machToCAS(0.78, 35000); !near(got, 266, 2) {
		t.Errorf("Mach 0.78 at FL350 = %v kt CAS", got)
	}
	if got := machToTAS(0.78, 35000); !near(got, 450, 2) {
		t.Errorf("Mach 0.78 at FL350 = %v kt TAS", got)
	}
	// At sea level CAS equals Mach times the speed of sound.
	if got := machToCAS(0.3, 0); !near(got, 0.3*661.48, 0.05) {
		t.Errorf("Mach 0.3 at sea level = %v kt CAS", got)
	}
	// Mach 0.5 at 10000 ft: TAS 0.5 * 638.3 kt (a at -4.8 C) = 319 kt; with
	// the ISA density ratio 0.7385 that is about 275 kt EAS / 278 kt CAS.
	if got := machToCAS(0.5, 10000); !near(got, 278, 3) {
		t.Errorf("Mach 0.5 at 10000 ft = %v kt CAS", got)
	}
	for _, alt := range []float64{0, 10000, 25000, 36089.24, 41000} {
		for _, mach := range []float64{0.2, 0.5, 0.8, 0.95} {
			if got := casToMach(machToCAS(mach, alt), alt); !near(got, mach, 1e-9) {
				t.Errorf("casToMach(machToCAS(%v, %v)) = %v", mach, alt, got)
			}
		}
	}
	if !near(angleDiff(350, 10), 20, 1e-12) || !near(angleDiff(10, 350), 20, 1e-12) || !near(angleDiff(90, 270), 180, 1e-12) || angleDiff(45, 45) != 0 {
		t.Error("angleDiff")
	}
	if !near(velocityDiff(100, 0, 100, 180), 200, 1e-9) || !near(velocityDiff(300, 90, 400, 0), 500, 1e-9) || !near(velocityDiff(250, 123, 250, 123), 0, 1e-9) {
		t.Error("velocityDiff")
	}
}
