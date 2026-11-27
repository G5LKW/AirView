package met

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"testing"
	"time"

	"airview/internal/radio"
	"airview/internal/store"
	"airview/internal/weather"
)

// ---- synthetic world ------------------------------------------------------

const testDT = 8.0 // the test atmosphere is ISA + 8 degC

// truthWind is the wind field: backing and strengthening with height.
func truthWind(altFt float64) (spd, dir float64) {
	return 15 + altFt/1000*2.2, 230 + altFt/1000*0.8
}

func truthTemp(altFt float64) float64 { return radio.ISAFt(altFt).TempC + testDT }

type simAC struct {
	icao     uint32
	lat, lon float64
	alt      float64
	hdg      float64 // true heading
	mach     float64
	bias     float64 // the airframe reports its heading this many degrees too low
	quantise bool
}

// input builds the Input an ideal (or quantised) transponder would give.
func (a *simAC) input(t time.Time) Input {
	nan := math.NaN()
	temp := truthTemp(a.alt)
	tas := radio.TASFromMachKt(a.mach, temp)
	ws, wd := truthWind(a.alt)
	sw, cw := math.Sincos(wd * math.Pi / 180)
	sh, ch := math.Sincos(a.hdg * math.Pi / 180)
	ge, gn := tas*sh-ws*sw, tas*ch-ws*cw
	in := Input{
		T: t, ICAO: a.icao, Lat: a.lat, Lon: a.lon, AltFt: a.alt,
		GS: math.Hypot(ge, gn), Track: norm360(math.Atan2(ge, gn) * 180 / math.Pi),
		TAS: tas, IAS: CASFromMach(a.mach, radio.ISAFt(a.alt).PressureHPa), Mach: a.mach,
		MagHeading: norm360(a.hdg - MagVar(a.lat, a.lon, a.alt*0.0003048, t) - a.bias),
		Roll:       0, MRARWindSpd: nan, MRARWindDir: nan, MRARTemp: nan,
	}
	if a.quantise {
		q := func(v, step float64) float64 { return math.Round(v/step) * step }
		in.GS, in.TAS = q(in.GS, 2), q(in.TAS, 2)
		in.Track, in.MagHeading = q(in.Track, 90.0/512), norm360(q(in.MagHeading, 90.0/512))
		in.Mach, in.IAS = q(in.Mach, 0.004), q(in.IAS, 1)
		in.AltFt = q(in.AltFt, 25)
	}
	return in
}

func windErr(spd, dir, altFt float64) float64 {
	ws, wd := truthWind(altFt)
	s1, c1 := math.Sincos(dir * math.Pi / 180)
	s2, c2 := math.Sincos(wd * math.Pi / 180)
	return math.Hypot(spd*s1-ws*s2, spd*c1-ws*c2)
}

var t0 = time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

// ---- Derive -----------------------------------------------------------------

func TestDeriveReproducesWindAndTemperature(t *testing.T) {
	for _, alt := range []float64{3000, 12000, 24000, 35000, 41000} {
		for hdg := 0.0; hdg < 360; hdg += 37 {
			a := simAC{icao: 0x400001, lat: 51.2, lon: 6.8, alt: alt, hdg: hdg, mach: 0.3 + alt/41000*0.5}
			d := Derive(a.input(t0))
			if !d.HasWind || !d.HasOAT {
				t.Fatalf("alt %v hdg %v: %+v", alt, hdg, d)
			}
			if e := windErr(d.WindSpd, d.WindDir, alt); e > 0.01 {
				t.Errorf("alt %v hdg %v: wind %.2f/%.1f, vector error %.3f kt", alt, hdg, d.WindSpd, d.WindDir, e)
			}
			if e := math.Abs(d.OAT - truthTemp(alt)); e > 0.01 {
				t.Errorf("alt %v: OAT %.3f want %.3f", alt, d.OAT, truthTemp(alt))
			}
			if d.Quality < 0.5 || d.Quality > 1 {
				t.Errorf("quality %v", d.Quality)
			}
		}
	}
}

func TestDeriveFallbacks(t *testing.T) {
	nan := math.NaN()
	a := simAC{icao: 1, lat: 48, lon: 11, alt: 34000, hdg: 75, mach: 0.79}

	// No Mach: computed from IAS and pressure altitude.
	in := a.input(t0)
	in.Mach = nan
	d := DeriveDetail(in)
	if !d.HasOAT || math.Abs(d.OAT-truthTemp(a.alt)) > 0.05 || math.Abs(d.Mach-a.mach) > 1e-4 {
		t.Errorf("IAS fallback: OAT %.3f (want %.3f) mach %.4f", d.OAT, truthTemp(a.alt), d.Mach)
	}
	if d.TempQuality >= DeriveDetail(a.input(t0)).TempQuality {
		t.Error("temperature from IAS-derived Mach must have lower quality")
	}

	// No TAS: Mach x speed of sound of the standard atmosphere; exact only
	// when the air is standard, and it yields no temperature.
	in = a.input(t0)
	in.TAS = nan
	d = DeriveDetail(in)
	if !d.HasWind || !d.TASDerived || d.HasOAT {
		t.Fatalf("no-TAS: %+v", d)
	}
	wantTAS := radio.TASFromMachKt(a.mach, radio.ISAFt(a.alt).TempC)
	if math.Abs(d.TAS-wantTAS) > 0.01 {
		t.Errorf("derived TAS %.2f want %.2f", d.TAS, wantTAS)
	}
	// With the right temperature hint (what the Collector supplies from its
	// profile) the wind is exact again.
	d = derive(&in, deriveOpts{magVar: nan, tempHintC: truthTemp(a.alt)})
	if e := windErr(d.WindSpd, d.WindDir, a.alt); e > 0.01 {
		t.Errorf("no-TAS wind with temperature hint: error %.3f kt", e)
	}

	// Neither Mach nor TAS, only IAS: wind from IAS-derived Mach.
	in = a.input(t0)
	in.TAS, in.Mach = nan, nan
	if d := DeriveDetail(in); !d.HasWind || !d.TASDerived || d.WindQuality > 0.5 {
		t.Errorf("IAS only: %+v", d)
	}

	// CAS <-> Mach round trip.
	for _, m := range []float64{0.2, 0.5, 0.82} {
		p := radio.ISAFt(30000).PressureHPa
		if got := MachFromCAS(CASFromMach(m, p), p); math.Abs(got-m) > 1e-9 {
			t.Errorf("Mach round trip %v -> %v", m, got)
		}
	}
	// 250 kt CAS at FL100 is Mach 0.452 (standard tables).
	if m := MachFromCAS(250, radio.ISAFt(10000).PressureHPa); math.Abs(m-0.452) > 0.002 {
		t.Errorf("MachFromCAS(250, FL100) = %.4f", m)
	}
}

func TestDeriveRejects(t *testing.T) {
	nan := math.NaN()
	base := simAC{icao: 1, lat: 50, lon: 8, alt: 30000, hdg: 200, mach: 0.76}

	in := base.input(t0)
	in.Roll = 12
	d := DeriveDetail(in)
	if d.HasWind || d.Reject&RejectTurning == 0 {
		t.Errorf("turning aircraft must give no wind: %+v", d)
	}
	if !d.HasOAT {
		t.Error("turning does not invalidate the temperature")
	}
	in.Roll = -5.5
	if Derive(in).HasWind {
		t.Error("roll -5.5 accepted")
	}
	in.Roll = 3
	if !Derive(in).HasWind {
		t.Error("roll 3 rejected")
	}

	in = base.input(t0)
	in.Lat, in.Lon = nan, nan
	if d := DeriveDetail(in); d.HasWind || d.Reject&RejectNoPosition == 0 || !d.HasOAT {
		t.Errorf("no position: %+v", d)
	}

	slow := simAC{icao: 1, lat: 50, lon: 8, alt: 2000, hdg: 90, mach: 0.15}
	if d := DeriveDetail(slow.input(t0)); d.HasWind || d.HasOAT || d.Reject&RejectLowSpeed == 0 || d.Reject&RejectLowMach == 0 {
		t.Errorf("slow aircraft: %+v", d)
	}

	// TAS and Mach that cannot belong together (stale / mis-decoded).
	in = base.input(t0)
	in.TAS *= 1.25
	if d := DeriveDetail(in); d.HasWind || d.HasOAT || d.Reject&RejectInconsistent == 0 {
		t.Errorf("inconsistent TAS/Mach: %+v", d)
	}
	// Mach and IAS that contradict each other.
	in = base.input(t0)
	in.IAS += 60
	if d := DeriveDetail(in); d.HasOAT || d.Reject&RejectInconsistent == 0 {
		t.Errorf("inconsistent IAS/Mach: %+v", d)
	}

	// Absurd wind: the ground vector points the other way.
	in = base.input(t0)
	in.Track = norm360(in.Track + 30)
	in.GS += 250
	if d := DeriveDetail(in); d.HasWind || d.Reject&RejectWindRange == 0 {
		t.Errorf("absurd wind: %+v", d)
	}
	in = base.input(t0)
	in.Track = norm360(in.Track + 90)
	if d := DeriveDetail(in); d.HasWind || d.Reject&RejectDrift == 0 {
		t.Errorf("drift: %+v", d)
	}

	// Nothing at all.
	d = DeriveDetail(Input{T: t0, Lat: nan, Lon: nan, AltFt: nan, GS: nan, Track: nan, TAS: nan, IAS: nan, Mach: nan,
		MagHeading: nan, Roll: nan, MRARWindSpd: nan, MRARWindDir: nan, MRARTemp: nan})
	if d.HasWind || d.HasOAT || d.Quality != 0 || len(d.Reject.Strings()) == 0 {
		t.Errorf("empty input: %+v", d)
	}
	if _, err := json.Marshal(d.Derived); err != nil {
		t.Errorf("Derived must marshal: %v", err)
	}
}

func TestMRARPrecedence(t *testing.T) {
	nan := math.NaN()
	a := simAC{icao: 1, lat: 50, lon: 8, alt: 36000, hdg: 100, mach: 0.8}
	ws, wd := truthWind(a.alt)
	tt := truthTemp(a.alt)

	// Plausible direct values win.
	in := a.input(t0)
	in.MRARWindSpd, in.MRARWindDir, in.MRARTemp = ws+3, wd+2, tt+0.5
	d := DeriveDetail(in)
	if d.WindSource != SourceMRAR || d.TempSource != SourceMRAR || math.Abs(d.WindSpd-(ws+3)) > 1e-6 ||
		math.Abs(d.OAT-(tt+0.5)) > 1e-6 || d.Quality != 1 {
		t.Errorf("MRAR precedence: %+v", d)
	}
	if math.Abs(d.EHSTemp-tt) > 0.01 {
		t.Errorf("EHSTemp %.2f", d.EHSTemp)
	}

	// Temperature scale ambiguity: the transponder used 0.125 degC/LSB, so
	// the 0.25-scale decode is twice the real value.
	in = a.input(t0)
	in.MRARTemp = 2 * (tt + 0.25)
	if d := DeriveDetail(in); d.TempSource != SourceMRAR || math.Abs(d.OAT-(tt+0.25)) > 1e-6 {
		t.Errorf("0.125 scale: OAT %.2f source %v", d.OAT, d.TempSource)
	}
	// Same without any EHS data: the reading closest to ISA is taken.
	only := Input{T: t0, Lat: nan, Lon: nan, AltFt: 36000, GS: nan, Track: nan, TAS: nan, IAS: nan, Mach: nan,
		MagHeading: nan, Roll: nan, MRARWindSpd: 80, MRARWindDir: 270, MRARTemp: -104}
	if d := DeriveDetail(only); !d.HasWind || !d.HasOAT || d.OAT != -52 || d.WindDir != 270 {
		t.Errorf("MRAR only: %+v", d)
	}

	// Implausible direct values are ignored in favour of EHS.
	in = a.input(t0)
	in.MRARWindSpd, in.MRARWindDir, in.MRARTemp = 20, norm360(wd+180), tt+25
	d = DeriveDetail(in)
	if d.WindSource != SourceEHS || d.TempSource != SourceEHS || d.Reject&RejectMRAR == 0 {
		t.Errorf("implausible MRAR: %+v", d)
	}
	in.MRARWindSpd, in.MRARWindDir = 400, 90
	if d := DeriveDetail(in); d.WindSource != SourceEHS {
		t.Errorf("out-of-range MRAR wind used: %+v", d)
	}
}

// ---- Collector ----------------------------------------------------------------

// fleet builds a traffic sample: aircraft at many levels on all headings.
func fleet(rng *rand.Rand, n int, quantise bool) []*simAC {
	acs := make([]*simAC, n)
	for i := range acs {
		alt := float64(4+rng.Intn(36)) * 1000
		if rng.Intn(3) == 0 {
			alt += float64(rng.Intn(1000)) // climbing / descending traffic
		}
		acs[i] = &simAC{icao: 0x3C0000 + uint32(i), lat: 49 + rng.Float64()*3, lon: 6 + rng.Float64()*5,
			alt: alt, hdg: rng.Float64() * 360, mach: 0.36 + alt/40000*0.44 + rng.Float64()*0.03, quantise: quantise}
	}
	return acs
}

func runFleet(c *Collector, acs []*simAC, start time.Time, dur, step time.Duration, rng *rand.Rand) {
	for dt := time.Duration(0); dt < dur; dt += step {
		for _, a := range acs {
			// Small speed changes dither the quantisation, as in real life.
			a.mach += (rng.Float64() - 0.5) * 0.002
			c.Observe(a.input(start.Add(dt)))
		}
	}
}

func TestCollectorProfile(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	c := New(nil, Options{})
	acs := fleet(rng, 150, true)
	runFleet(c, acs, t0, 60*time.Minute, 10*time.Second, rng)

	p := c.Current(time.Time{})
	if len(p.Levels) < 30 {
		t.Fatalf("only %d levels", len(p.Levels))
	}
	var worstW, worstT float64
	for _, l := range p.Levels {
		if l.WindKt == nil || l.TempC == nil || l.NWind < 100 {
			t.Fatalf("level %d incomplete: %+v", l.Level, l)
		}
		// Compare at the mean altitude of the samples in the layer.
		eW := windErr(*l.WindKt, *l.WindDir, l.AltFt)
		eT := math.Abs(*l.TempC - truthTemp(l.AltFt))
		worstW, worstT = math.Max(worstW, eW), math.Max(worstT, eT)
		if math.Abs(*l.ISADevC-testDT) > 0.5 {
			t.Errorf("level %d: ISA deviation %.2f", l.Level, *l.ISADevC)
		}
	}
	t.Logf("quantised fleet: worst level wind error %.2f kt, worst temperature error %.2f K", worstW, worstT)
	if worstW > 1.0 || worstT > 0.4 {
		t.Errorf("profile off: wind %.2f kt, temperature %.2f K", worstW, worstT)
	}
	if p.WindowMin < 60 || p.WindowMin > 90 {
		t.Errorf("window %v min", p.WindowMin)
	}

	// Analysis of the synthetic atmosphere.
	if p.FreezingLevelFt == nil {
		t.Fatal("no freezing level")
	}
	wantFrz := (15 + testDT) / 1.9812 * 1000
	if math.Abs(*p.FreezingLevelFt-wantFrz) > 250 {
		t.Errorf("freezing level %.0f want %.0f", *p.FreezingLevelFt, wantFrz)
	}
	if p.Tropopause == nil || math.Abs(p.Tropopause.AltFt-36089) > 1100 {
		t.Errorf("tropopause %+v", p.Tropopause)
	}
	if p.MaxWind == nil || !p.MaxWind.Jet || p.MaxWind.AltFt < 38000 {
		t.Errorf("max wind %+v", p.MaxWind)
	}
	if len(p.Inversions) != 0 {
		t.Errorf("spurious inversions: %+v", p.Inversions)
	}
	if len(p.WindsAloft) < 6 {
		t.Errorf("winds aloft: %+v", p.WindsAloft)
	}
	for _, w := range p.WindsAloft {
		if w.WindKt == nil || w.TempC == nil {
			continue
		}
		if e := windErr(*w.WindKt, *w.WindDir, w.AltFt); e > 1.5 {
			t.Errorf("FL%03d wind error %.2f kt", w.FL, e)
		}
		if e := math.Abs(*w.TempC - truthTemp(w.AltFt)); e > 0.5 {
			t.Errorf("FL%03d temp error %.2f", w.FL, e)
		}
	}

	// Rate limit: 150 aircraft x 360 steps, all 10 s apart -> all derived;
	// a second call within the interval is not.
	s := c.Stats()
	before := s.Derived
	in := acs[0].input(t0.Add(60*time.Minute + time.Second))
	d1 := c.Observe(in)
	in.T = in.T.Add(time.Second)
	d2 := c.Observe(in)
	if c.Stats().Derived != before+1 || d1 != d2 {
		t.Errorf("rate limit: derived %d -> %d", before, c.Stats().Derived)
	}

	// Map layer.
	obs := c.Latest(time.Time{}, 10*time.Minute, 0, true)
	if len(obs) != len(acs) {
		t.Errorf("latest per aircraft: %d", len(obs))
	}
	for _, v := range []any{p, obs, c.Stats(), c.HeadingBiases(0)} {
		if _, err := json.Marshal(v); err != nil {
			t.Fatalf("marshal %T: %v", v, err)
		}
	}
}

func TestCollectorLearnsHeadingBias(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	c := New(nil, Options{})
	acs := fleet(rng, 120, false)
	// Three airframes with a systematic heading error, flying at busy
	// cruise levels.
	planted := map[uint32]float64{}
	for i, b := range []float64{2.5, -1.8, 4.0} {
		a := acs[i]
		a.alt, a.mach, a.bias = float64(33+i)*1000, 0.78, b
		planted[a.icao] = b
	}
	// Uncorrected, a 2.5 degree error is about 20 kt of wind.
	raw := Derive(acs[0].input(t0))
	if e := windErr(raw.WindSpd, raw.WindDir, acs[0].alt); e < 15 {
		t.Fatalf("planted bias too small to matter: %.1f kt", e)
	}

	runFleet(c, acs, t0, 70*time.Minute, 10*time.Second, rng)

	for icao, b := range planted {
		got := c.HeadingCorrection(icao)
		if math.Abs(got-b) > 0.4 {
			t.Errorf("%06X: learned correction %.2f, planted %.2f", icao, got, b)
		}
	}
	// Unbiased aircraft must stay (almost) uncorrected.
	for _, a := range acs[3:] {
		if g := c.HeadingCorrection(a.icao); math.Abs(g) > 0.3 {
			t.Errorf("%06X: unbiased aircraft got correction %.2f", a.icao, g)
		}
	}
	// The corrected observation of the biased aircraft is good now.
	a := acs[0]
	d := c.Observe(a.input(t0.Add(71 * time.Minute)))
	if e := windErr(d.WindSpd, d.WindDir, a.alt); !d.HasWind || e > 3 {
		t.Errorf("biased aircraft after learning: wind error %.2f kt", e)
	}
	// And the profile at those levels is not polluted.
	for _, l := range c.Current(time.Time{}).Levels {
		if l.Level >= 33 && l.Level <= 35 {
			// The minutes before the correction was known stay in the
			// averages: a 4 degree error is 30 kt for those samples.
			if e := windErr(*l.WindKt, *l.WindDir, l.AltFt); e > 2 {
				t.Errorf("level %d wind error %.2f kt", l.Level, e)
			}
		}
	}
	hb := c.HeadingBiases(3)
	if len(hb) != 3 || math.Abs(hb[0].MeanDeg-4.0) > 0.3 {
		t.Errorf("HeadingBiases: %+v", hb)
	}

	// Learning switched off: no correction.
	c2 := New(nil, Options{NoBiasLearning: true})
	runFleet(c2, acs, t0, 20*time.Minute, 10*time.Second, rng)
	if c2.HeadingCorrection(acs[0].icao) != 0 || len(c2.HeadingBiases(0)) != 0 {
		t.Error("bias learned although disabled")
	}
}

func TestCollectorTurningAircraftIgnored(t *testing.T) {
	c := New(nil, Options{})
	a := simAC{icao: 0xABCDEF, lat: 50, lon: 8, alt: 20000, hdg: 10, mach: 0.6}
	for i := 0; i < 20; i++ {
		in := a.input(t0.Add(time.Duration(i) * 10 * time.Second))
		in.Roll = 25
		if d := c.Observe(in); d.HasWind || !d.HasOAT {
			t.Fatalf("turning: %+v", d)
		}
	}
	s := c.Stats()
	if s.Wind != 0 || s.Temp != 20 || s.Rejected["turning"] != 20 {
		t.Errorf("stats %+v", s)
	}
	p := c.Current(time.Time{})
	if len(p.Levels) != 1 || p.Levels[0].WindKt != nil || p.Levels[0].TempC == nil {
		t.Errorf("profile %+v", p.Levels)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	rng := rand.New(rand.NewSource(3))
	// Recent wall-clock time: Load rebuilds the current profile from the
	// last window of met_sample. Start on the hour and run 61 .. 119 minutes,
	// so that exactly two clock hours are filled whenever the test runs.
	start := time.Now().Add(-62 * time.Minute).Truncate(time.Hour)
	dur := min(time.Since(start)-time.Minute, 119*time.Minute)
	c := New(st, Options{StoreInterval: 30 * time.Second})
	if err := c.Load(ctx); err != nil {
		t.Fatal(err)
	}
	acs := fleet(rng, 60, false)
	acs[0].alt, acs[0].mach, acs[0].bias = 35000, 0.78, 3
	for i := 1; i < 12; i++ {
		acs[i].alt = float64(34+i%3) * 1000
	}
	runFleet(c, acs, start, dur, 10*time.Second, rng)
	c.Flush()
	if err := c.SaveBiases(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	wantBias := c.HeadingCorrection(acs[0].icao)
	if math.Abs(wantBias-3) > 0.5 {
		t.Fatalf("bias not learned: %.2f", wantBias)
	}

	// The table holds exactly what was accumulated.
	var nw, nt int64
	if err := st.DB().QueryRow(`SELECT sum(n_wind), sum(n_temp) FROM met_profile`).Scan(&nw, &nt); err != nil {
		t.Fatal(err)
	}
	s := c.Stats()
	if nw != s.Wind || nt != s.Temp || nw == 0 {
		t.Errorf("met_profile sums %d/%d, stats %d/%d", nw, nt, s.Wind, s.Temp)
	}
	var nObs int64
	if err := st.DB().QueryRow(`SELECT count(*) FROM met_sample`).Scan(&nObs); err != nil {
		t.Fatal(err)
	}
	if nObs < 1000 || nObs > s.Wind/2 {
		t.Errorf("met_sample rows %d (thinning) for %d observations", nObs, s.Wind)
	}

	from, to := start.Add(-time.Hour), start.Add(3*time.Hour)
	h1, err := c.History(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(h1.Hours) != 2 || len(h1.Cells) < 50 {
		t.Fatalf("history: %d hours, %d cells", len(h1.Hours), len(h1.Cells))
	}
	for _, cell := range h1.Cells {
		if cell.WindKt == nil || cell.TempC == nil {
			continue
		}
		// Hourly cells mix altitudes within the layer: compare against the
		// range of the truth over the layer.
		lo, hi := float64(cell.Level)*1000, float64(cell.Level)*1000+1000
		if *cell.TempC < truthTemp(hi)-0.1 || *cell.TempC > truthTemp(lo)+0.1 {
			t.Errorf("history cell level %d temp %.2f outside [%.2f, %.2f]", cell.Level, *cell.TempC, truthTemp(hi), truthTemp(lo))
		}
		if e := windErr(*cell.WindKt, *cell.WindDir, (lo+hi)/2); e > 3.5 {
			t.Errorf("history cell level %d wind error %.2f", cell.Level, e)
		}
	}

	// A fresh collector continues from the database.
	c2 := New(st, Options{})
	// Before Load the table alone answers.
	h0, err := c2.History(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	h2, err := c2.History(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	j0, _ := json.Marshal(h0)
	j1, _ := json.Marshal(h1)
	j2, _ := json.Marshal(h2)
	if string(j1) != string(j2) || string(j0) != string(j1) {
		t.Errorf("history differs after reload (%d / %d / %d bytes)", len(j0), len(j1), len(j2))
	}
	if got := c2.HeadingCorrection(acs[0].icao); math.Abs(got-wantBias) > 1e-9 {
		t.Errorf("bias after reload %.3f want %.3f", got, wantBias)
	}
	p := c2.Current(time.Now())
	if len(p.Levels) < 20 {
		t.Fatalf("current profile after reload has %d levels", len(p.Levels))
	}
	for _, l := range p.Levels {
		if l.TempC != nil && math.Abs(*l.TempC-truthTemp(l.AltFt)) > 0.3 {
			t.Errorf("reloaded level %d temp %.2f want %.2f", l.Level, *l.TempC, truthTemp(l.AltFt))
		}
		if l.WindKt != nil {
			if e := windErr(*l.WindKt, *l.WindDir, l.AltFt); e > 2.5 {
				t.Errorf("reloaded level %d wind error %.2f", l.Level, e)
			}
		}
	}
	hp, err := c2.HourProfile(ctx, start.Add(10*time.Minute))
	if err != nil || len(hp.Levels) < 20 || hp.FreezingLevelFt == nil {
		t.Errorf("hour profile: %v, %d levels", err, len(hp.Levels))
	}

	// New observations after the reload merge into the same hours.
	before := len(h2.Cells)
	runFleet(c2, acs[:5], time.Now().Add(-30*time.Second), 20*time.Second, 10*time.Second, rng)
	c2.Flush()
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	var nw2 int64
	if err := st.DB().QueryRow(`SELECT sum(n_wind) FROM met_profile`).Scan(&nw2); err != nil {
		t.Fatal(err)
	}
	if nw2 != nw+c2.Stats().Wind {
		t.Errorf("after reload: n_wind %d want %d", nw2, nw+c2.Stats().Wind)
	}
	h3, _ := c2.History(ctx, from, to)
	if len(h3.Cells) < before {
		t.Errorf("history shrank: %d -> %d", before, len(h3.Cells))
	}

	// Run stops cleanly and flushes.
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c2.Run(rctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// ---- analysis on crafted profiles ------------------------------------------------

func craftedLevels() []Level { return craftedProfile(5500) }

// craftedProfile is ISA+6 with a 4 K inversion over the 2000 ft above
// invBase, a tropopause near 36 000 ft and a 140 kt jet core at FL345.
func craftedProfile(invBase float64) []Level {
	var ls []Level
	for l := 1; l <= 42; l++ {
		alt := float64(l)*1000 + 500
		p := newPoint(l, alt)
		p.t = radio.ISAFt(alt).TempC + 6
		switch {
		case alt >= invBase+2000:
			p.t += 8
		case alt >= invBase+1000:
			p.t += 6
		}
		// Jet core at FL340.
		spd := 30 + 110*math.Exp(-math.Pow((alt-34500)/6000, 2))
		s, c := math.Sincos(280 * math.Pi / 180)
		p.u, p.v = -spd*s, -spd*c
		p.nw, p.nt, p.tsd, p.wsd = 40, 40, 1.2, 4
		ls = append(ls, p.level1())
	}
	return ls
}

func TestAnalyze(t *testing.T) {
	ls := craftedLevels()
	a := Analyze(ls)

	if len(a.Inversions) != 1 {
		t.Fatalf("inversions: %+v", a.Inversions)
	}
	inv := a.Inversions[0]
	if inv.BaseFt != 5500 || inv.TopFt != 7500 || math.Abs(inv.StrengthC-4.04) > 0.15 || inv.GradKPerKm < 5 {
		t.Errorf("inversion %+v", inv)
	}
	if a.Tropopause == nil || math.Abs(a.Tropopause.AltFt-36500) > 600 || !a.Tropopause.Confirmed {
		t.Errorf("tropopause %+v", a.Tropopause)
	}
	if a.MaxWind == nil || a.MaxWind.AltFt != 34500 || math.Abs(a.MaxWind.WindKt-140) > 0.2 || !a.MaxWind.Jet ||
		math.Abs(a.MaxWind.WindDir-280) > 0.1 {
		t.Errorf("max wind %+v", a.MaxWind)
	}
	if a.MaxShear == nil || a.MaxShear.KtPer1000Ft < 5 {
		t.Errorf("shear %+v", a.MaxShear)
	}
	// Surface ISA+6+8 = 29 degC with 1.98 K/1000 ft above the inversion.
	wantFrz := (15 + 14) / 1.9812 * 1000
	if a.FreezingLevelFt == nil || math.Abs(*a.FreezingLevelFt-wantFrz) > 60 {
		t.Errorf("freezing level %v want %.0f", pv(a.FreezingLevelFt), wantFrz)
	}
	var sawInv, sawStd bool
	for _, l := range a.Lapse {
		if l.Class == LapseInversion {
			sawInv = true
		}
		// The real layers are warmer, hence thicker, than standard ones.
		if l.BaseFt == 20500 && math.Abs(l.LapseKPerKm-6.5*(273.15-25.6)/(273.15-25.6+14)) < 0.05 {
			sawStd = true
		}
	}
	if !sawInv || !sawStd {
		t.Errorf("lapse layers: inversion %v, tropospheric %v: %+v", sawInv, sawStd, a.Lapse)
	}
	if len(a.WindsAloft) != len(StandardFlightLevels)-1 { // FL450 is above the data
		t.Errorf("winds aloft rows: %d", len(a.WindsAloft))
	}
	for _, w := range a.WindsAloft {
		if w.FL == 340 {
			// 280 deg, 138.5 kt between the two layers, -38 degC: "7838" + "38".
			if w.Coded != "783838" {
				t.Errorf("FL340 coded %q (%v kt, %v C)", w.Coded, pv(w.WindKt), pv(w.TempC))
			}
		}
		if w.FL == 30 && w.Coded != "2830+15" {
			t.Errorf("FL030 coded %q", w.Coded)
		}
	}
	if g := codeWindAloft(123, 2, 5, 60); g != "9900+05" {
		t.Errorf("calm coded %q", g)
	}

	// Noise must not create inversions: a level 0.6 K warm with few samples.
	noisy := craftedLevels()[10:20]
	warm := *noisy[4].TempC + 2.6
	noisy[4].TempC, noisy[4].NTemp = &warm, 3
	if inv := Analyze(noisy).Inversions; len(inv) != 0 {
		t.Errorf("noise taken for an inversion: %+v", inv)
	}

	// Empty input.
	e := Analyze(nil)
	if b, err := json.Marshal(e); err != nil || e.Tropopause != nil || e.Lapse == nil {
		t.Errorf("empty analysis: %s %v", b, err)
	}
}

func modelFromTruth(ls []Level, dT, dSpd float64, rh func(altFt float64) float64) []weather.Level {
	pts := pointsOf(ls)
	var out []weather.Level
	for _, hpa := range weather.DefaultLevels {
		alt := radio.PressureAltitudeM(float64(hpa)) / 0.3048
		m := weather.Level{HPa: float64(hpa), TempC: weather.NaN, RH: weather.NaN, HeightM: weather.NaN,
			WindKmh: weather.NaN, WindDir: weather.NaN}
		if t, ok := sampleTemp(pts, alt); ok {
			m.TempC = weather.Num(t - dT)
		}
		if u, v, ok := sampleWind(pts, alt); ok {
			spd := math.Hypot(u, v)
			m.WindKmh = weather.Num((spd - dSpd) * kmhPerKt)
			m.WindDir = weather.Num(norm360(math.Atan2(-u, -v)*180/math.Pi - 5))
		}
		if rh != nil {
			m.RH = weather.Num(rh(alt))
		}
		out = append(out, m)
	}
	return out
}

func TestCompareWithModel(t *testing.T) {
	ls := craftedLevels()
	model := modelFromTruth(ls, 1.5, -4, nil)
	c := Compare(ls, model)
	// 1000 hPa (364 ft) is below the observed range; the 13 levels from
	// 975 to 200 hPa are inside.
	if c.NTemp != 13 || c.NWind != 13 || len(c.Levels) != 13 {
		t.Fatalf("levels compared: %d temp, %d wind, %d rows", c.NTemp, c.NWind, len(c.Levels))
	}
	if math.Abs(*c.TempBiasC-1.5) > 0.01 || math.Abs(*c.TempRMSC-1.5) > 0.01 {
		t.Errorf("temp bias %.3f rms %.3f", *c.TempBiasC, *c.TempRMSC)
	}
	if math.Abs(*c.SpeedBiasKt+4) > 0.11 || math.Abs(*c.DirBiasDeg-5) > 0.11 {
		t.Errorf("speed bias %.2f dir bias %.2f", *c.SpeedBiasKt, *c.DirBiasDeg)
	}
	// RMSVD must be at least the speed difference and reflect the 5 degree turn.
	if *c.RMSVDKt < 4 || *c.RMSVDKt > 12 {
		t.Errorf("RMSVD %.2f", *c.RMSVDKt)
	}
	for i := 1; i < len(c.Levels); i++ {
		if c.Levels[i].AltFt <= c.Levels[i-1].AltFt {
			t.Error("levels not ascending")
		}
	}
	if _, err := json.Marshal(c); err != nil {
		t.Fatal(err)
	}
	// Missing model values and an empty model are fine.
	if e := Compare(ls, nil); e.NTemp != 0 || e.TempBiasC != nil || e.Levels == nil {
		t.Errorf("empty compare %+v", e)
	}
	if _, err := json.Marshal(Compare(nil, model)); err != nil {
		t.Fatal(err)
	}
}

func TestRadioLayers(t *testing.T) {
	// A low, warm inversion: moist below, very dry above.
	ls := craftedProfile(1500)
	dry := modelFromTruth(ls, 0, 0, func(alt float64) float64 {
		if alt < 2000 {
			return 90
		}
		return 10
	})
	r := RadioLayers(ls, dry)
	if len(r.Layers) != 1 || r.Strongest != 0 {
		t.Fatalf("layers %+v", r)
	}
	l := r.Layers[0]
	if l.Humidity != "model" || l.DNDH == nil || l.BaseFt != 1500 || l.TopFt != 3500 || l.K == nil {
		t.Fatalf("layer %+v", l)
	}
	if *l.DNDH > -79 || r.Class != radio.ClassSuperRefraction && r.Class != radio.ClassDucting {
		t.Errorf("moist-below/dry-above inversion: dN/dh %.1f class %q", *l.DNDH, r.Class)
	}
	if r.LowestBaseFt == nil || *r.LowestBaseFt != 1500 || r.Note == "" {
		t.Errorf("summary %+v", r)
	}

	// Without humidity: dry term only, close to the standard gradient.
	r2 := RadioLayers(ls, nil)
	if len(r2.Layers) != 1 || r2.Layers[0].Humidity != "dry" || r2.Layers[0].DNDH == nil {
		t.Fatalf("dry layers %+v", r2)
	}
	if g := *r2.Layers[0].DNDH; g > -20 || g < -60 {
		t.Errorf("dry gradient %.1f N/km", g)
	}
	if *r2.Layers[0].DNDH <= *l.DNDH {
		t.Error("humidity drop must steepen the gradient")
	}

	// No inversion at all.
	r3 := RadioLayers(ls[10:], dry)
	if len(r3.Layers) != 0 || r3.Strongest != -1 || r3.LowestBaseFt != nil {
		t.Errorf("no inversion: %+v", r3)
	}
	for _, v := range []any{r, r2, r3} {
		if _, err := json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
}

func BenchmarkObserve(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	c := New(nil, Options{})
	acs := fleet(rng, 200, true)
	ins := make([]Input, len(acs))
	for i, a := range acs {
		ins[i] = a.input(t0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		in := ins[i%len(ins)]
		in.T = t0.Add(time.Duration(i/len(ins)) * 6 * time.Second)
		c.Observe(in)
	}
}

func BenchmarkObserveRateLimited(b *testing.B) {
	c := New(nil, Options{})
	a := simAC{icao: 1, lat: 50, lon: 8, alt: 35000, hdg: 90, mach: 0.78}
	in := a.input(t0)
	c.Observe(in)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Observe(in)
	}
}
