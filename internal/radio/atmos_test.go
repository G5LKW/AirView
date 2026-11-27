package radio

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %.6g, want %.6g (±%g)", name, got, want, tol)
	}
}

func TestUnits(t *testing.T) {
	near(t, "wavelength", WavelengthM, 0.27504, 1e-5)
	near(t, "DB(2)", DB(2), 3.0103, 1e-4)
	near(t, "FromDB(10)", FromDB(10), 10, 1e-12)
	if !math.IsInf(DB(0), -1) {
		t.Error("DB(0) should be -Inf")
	}
	near(t, "1 W", WattsToDBm(1), 30, 1e-12)
	near(t, "250 W", WattsToDBm(250), 53.979, 1e-3)
	near(t, "round trip", DBmToWatts(WattsToDBm(125)), 125, 1e-9)
	// kT0 = −173.98 dBm/Hz (the "−174 dBm/Hz" rule).
	near(t, "kT0", NoisePowerDBm(290, 1), -173.975, 0.001)
	near(t, "kT0B 2 MHz", NoisePowerDBm(290, 2e6), -110.965, 0.001)
	// A 3 dB noise figure is ~289 K; 290 K is 3.01 dB.
	near(t, "Te(3 dB)", NoiseTempFromNF(3), 288.6, 0.1)
	near(t, "NF(290 K)", NFFromNoiseTemp(290), 3.0103, 1e-4)
	near(t, "NF round trip", NFFromNoiseTemp(NoiseTempFromNF(6.5)), 6.5, 1e-9)
}

func TestVapourPressure(t *testing.T) {
	// ITU-R P.453 / Buck (1981) saturation vapour pressure over water (no
	// enhancement factor): 6.112 hPa at 0 °C, 17.05 at 15 °C, 23.39 at 20 °C,
	// 2.863 at −10 °C; over ice at −10 °C: 2.599.
	near(t, "es(0)", SaturationVapourPressure(0, 0), 6.1121, 1e-9)
	near(t, "es(15)", SaturationVapourPressure(15, 0), 17.05, 0.01)
	near(t, "es(20)", SaturationVapourPressure(20, 0), 23.39, 0.01)
	near(t, "es(-10)", SaturationVapourPressure(-10, 0), 2.863, 0.005)
	near(t, "es ice(-10)", SaturationVapourPressureIce(-10, 0), 2.599, 0.005)
	near(t, "es ice(0)", SaturationVapourPressureIce(0, 0), 6.1115, 1e-9)
	// Enhancement factor at sea level is about +0.4 %.
	ef := SaturationVapourPressure(15, 1013.25) / SaturationVapourPressure(15, 0)
	near(t, "EF", ef, 1.0041, 0.0002)

	near(t, "e from RH", VapourPressureFromRH(15, 60, 0), 0.6*SaturationVapourPressure(15, 0), 1e-12)
	near(t, "e RH clamp", VapourPressureFromRH(15, 150, 0), SaturationVapourPressure(15, 0), 1e-12)
	near(t, "e RH clamp low", VapourPressureFromRH(15, -5, 0), 0, 0)
	near(t, "e from dew point", VapourPressureFromDewPoint(10, 0), SaturationVapourPressure(10, 0), 1e-12)

	// 20 °C / 50 % RH has a dew point of 9.3 °C.
	td := DewPointFromRH(20, 50)
	near(t, "dew point", td, 9.27, 0.05)
	near(t, "RH round trip", RHFromDewPoint(20, td), 50, 0.001)
	near(t, "saturated", DewPointFromRH(12, 100), 12, 0)
	for _, c := range [][2]float64{{-30, 70}, {-5, 20}, {5, 95}, {35, 10}, {40, 60}} {
		near(t, "RH round trip", RHFromDewPoint(c[0], DewPointFromRH(c[0], c[1])), c[1], 0.01)
	}
	near(t, "RH cap", RHFromDewPoint(10, 12), 100, 0)
	// 10 hPa of vapour at 15 °C is 7.52 g/m³.
	near(t, "vapour density", WaterVapourDensity(10, 15), 7.520, 0.001)
}

func TestRefractivity(t *testing.T) {
	// Dry air: N = 77.6·P/T.
	near(t, "dry", Refractivity(1000, 0, 0), 77.6*1000/273.15, 1e-9)
	// Hand computation for 1013.25 hPa, 15 °C, e = 10 hPa:
	// 77.6/288.15·(1013.25 + 4810·10/288.15) = 317.82.
	near(t, "N(e=10)", Refractivity(1013.25, 15, 10), 317.82, 0.01)
	// The same through the explicit two-term form N = 77.6·P/T + 3.73e5·e/T².
	T := 288.15
	near(t, "two-term form", Refractivity(1013.25, 15, 10), 77.6*1013.25/T+3.73e5*10/(T*T), 0.05)
	dry, wet := RefractivityTerms(1013.25, 15, 10)
	near(t, "dry+wet", dry+wet, Refractivity(1013.25, 15, 10), 1e-9)
	near(t, "dry term", dry, 272.87, 0.01)
	// Standard sea-level air at 60 % RH: N ≈ 319, i.e. the "N0 ≈ 315" region
	// of the ITU-R reference atmosphere.
	n := RefractivityRH(1013.25, 15, 60)
	near(t, "N standard", n, 319.0, 0.5)
	if math.Abs(n-315) > 8 {
		t.Errorf("N = %v not in the 315 region", n)
	}
	// Current P.453-13/-14 three-term form agrees closely.
	e := VapourPressureFromRH(15, 60, 1013.25)
	n3 := 77.6*(1013.25-e)/T + 72*e/T + 3.75e5*e/(T*T)
	near(t, "three-term form", n, n3, 0.05)

	near(t, "M", ModifiedRefractivity(300, 1000), 457, 1e-9)
	near(t, "gradient", GradientNPerKm(320, 0, 280, 1000), -40, 1e-9)
	near(t, "gradient 100 m", GradientNPerKm(320, 50, 310, 150), -100, 1e-9)
	near(t, "gradient same level", GradientNPerKm(320, 50, 310, 50), 0, 0)
}

func TestKFactor(t *testing.T) {
	// k = 157/(157 + dN/dh): the standard −39 N/km gives 4/3.
	near(t, "k(-39)", KFactor(-39), 4.0/3, 0.004)
	near(t, "k(-39.25)", KFactor(-39.25), 4.0/3, 1e-9)
	near(t, "k(0)", KFactor(0), 1, 1e-12)
	near(t, "k(-100)", KFactor(-100), 157.0/57, 1e-12)
	near(t, "k(+40)", KFactor(40), 157.0/197, 1e-12)
	near(t, "k(-157)", KFactor(-157), KMax, 0)
	near(t, "k(-300)", KFactor(-300), KMax, 0)
	if k := KFactor(-156.99); k <= 100 || k > KMax {
		t.Errorf("k(-156.99) = %v", k)
	}
	for _, g := range []float64{-150, -100, -39.25, 0, 60} {
		near(t, "gradient round trip", GradientForK(KFactor(g)), g, 1e-9)
	}
	near(t, "gradient for trapping", GradientForK(KMax), -157, 0)
	near(t, "gradient for bad k", GradientForK(-1), -157, 0)

	class := []struct {
		g    float64
		want string
	}{{60, ClassSubRefraction}, {0.1, ClassSubRefraction}, {0, ClassStandard}, {-40, ClassStandard}, {-78.9, ClassStandard},
		{-79, ClassSuperRefraction}, {-120, ClassSuperRefraction}, {-156.9, ClassSuperRefraction},
		{-157, ClassDucting}, {-400, ClassDucting}}
	for _, c := range class {
		if got := ClassifyGradient(c.g); got != c.want {
			t.Errorf("ClassifyGradient(%v) = %q, want %q", c.g, got, c.want)
		}
	}

	near(t, "horizon factor std", HorizonFactor(KStandard), 1, 1e-12)
	near(t, "horizon factor 2x", HorizonFactor(2*KStandard), math.Sqrt2, 1e-12)
	near(t, "horizon factor k=1", HorizonFactor(1), math.Sqrt(0.75), 1e-12)
	near(t, "horizon factor cap", HorizonFactor(KMax), 3, 0)
	near(t, "horizon factor bad", HorizonFactor(-2), 3, 0)
}

func TestDuctCutoff(t *testing.T) {
	// λmax = (2/3)·3.77e-3·100·sqrt(10) = 0.7948 m → 377 MHz.
	lam, f := DuctCutoff(100, 10, false)
	near(t, "λmax surface", lam, 0.7948, 0.0005)
	near(t, "cut-off surface", f, 377.2, 0.3)
	// Classical form 2.5e-3·D·sqrt(ΔM).
	near(t, "classical", lam, 2.5e-3*100*math.Sqrt(10), 0.01)
	lam2, f2 := DuctCutoff(100, 10, true)
	near(t, "elevated ratio", lam2/lam, 5.66/3.77, 1e-9)
	if f2 >= f {
		t.Error("an elevated duct of the same size traps lower frequencies")
	}
	// 1090 MHz (λ = 0.275 m) needs about 35 m at ΔM = 10.
	if _, f := DuctCutoff(30, 10, false); f <= FreqMHz {
		t.Errorf("30 m duct cut-off %v should be above 1090", f)
	}
	if _, f := DuctCutoff(40, 10, false); f >= FreqMHz {
		t.Errorf("40 m duct cut-off %v should be below 1090", f)
	}
	if lam, f := DuctCutoff(0, 10, false); lam != 0 || !math.IsInf(f, 1) {
		t.Error("degenerate duct")
	}
}

// refProfile samples the ITU-R P.453 reference atmosphere N = 315·exp(−h/7.35 km).
func refProfile(heights ...float64) []ProfilePoint {
	pts := make([]ProfilePoint, len(heights))
	for i, h := range heights {
		pts[i] = ProfilePoint{HeightM: h, N: 315 * math.Exp(-h/7350)}
	}
	return pts
}

func TestAnalyzeStandardProfile(t *testing.T) {
	p, ok := AnalyzePoints(refProfile(0, 200, 500, 1000, 1500, 3000, 5500))
	if !ok {
		t.Fatal("not ok")
	}
	// ΔN over the first kilometre of the reference atmosphere: 315·(e^(−1/7.35) − 1) = −40.07.
	near(t, "gradient", p.GradientNPerKm, -40.07, 0.01)
	near(t, "k", p.K, 157/(157-40.07), 1e-3)
	near(t, "surface N", p.SurfaceN, 315, 1e-9)
	if p.Class != ClassStandard || len(p.Ducts) != 0 {
		t.Errorf("class %q ducts %v", p.Class, p.Ducts)
	}
	if len(p.Layers) != 6 || len(p.Points) != 7 {
		t.Fatalf("layers %d points %d", len(p.Layers), len(p.Points))
	}
	for _, l := range p.Layers {
		if l.DMDH <= 0 || l.Class != ClassStandard {
			t.Errorf("layer %+v should be standard with dM/dh > 0", l)
		}
		near(t, "dM = dN + 157", l.DMDH, l.DNDH+157, 1e-9)
	}
	near(t, "M at 1 km", p.Points[3].M, p.Points[3].N+157, 1e-9)
	// The steepest layer of an exponential profile is the lowest one.
	near(t, "min gradient height", p.MinGradientHeightM, 0, 0)
	if p.MinGradientNPerKm > -40 || p.MinGradientNPerKm < -44 {
		t.Errorf("min gradient %v", p.MinGradientNPerKm)
	}

	// A shallow profile uses whatever it has.
	p, ok = AnalyzePoints([]ProfilePoint{{HeightM: 100, N: 320}, {HeightM: 600, N: 300}})
	near(t, "shallow gradient", p.GradientNPerKm, -40, 1e-9)
	if !ok {
		t.Error("two points are enough")
	}
	if _, ok := AnalyzePoints([]ProfilePoint{{HeightM: 0, N: 320}}); ok {
		t.Error("one point is not a profile")
	}
	// Input order does not matter and the input is not modified.
	in := []ProfilePoint{{HeightM: 1000, N: 280}, {HeightM: 0, N: 320}}
	p, _ = AnalyzePoints(in)
	if in[0].HeightM != 1000 || p.Points[0].HeightM != 0 {
		t.Error("sorting must not touch the caller's slice")
	}
}

func TestAnalyzeDucts(t *testing.T) {
	// Surface duct: M falls from the surface up to 100 m.
	// M: 340.0, 325.7, 432.0, 701.0
	p, _ := AnalyzePoints([]ProfilePoint{{HeightM: 0, N: 340}, {HeightM: 100, N: 310}, {HeightM: 1000, N: 275}, {HeightM: 3000, N: 230}})
	if len(p.Ducts) != 1 {
		t.Fatalf("ducts: %+v", p.Ducts)
	}
	d := p.Ducts[0]
	if d.Kind != DuctSurface || d.BaseM != 0 || d.TopM != 100 || d.TrapBaseM != 0 || d.ThicknessM != 100 {
		t.Errorf("surface duct: %+v", d)
	}
	near(t, "ΔM", d.StrengthM, 14.3, 1e-9)
	near(t, "λmax", d.MaxWavelengthM, 2.0/3*3.77e-3*100*math.Sqrt(14.3), 1e-9)
	if !d.Traps1090 || d.CutoffMHz > 400 {
		t.Errorf("100 m / 14 M duct must trap 1090 MHz: %+v", d)
	}
	near(t, "gradient lowest km", p.GradientNPerKm, -65, 1e-9)
	if p.Class != ClassDucting {
		t.Errorf("class %q", p.Class)
	}
	if p.Layers[0].Class != ClassDucting || p.Layers[0].DMDH >= 0 {
		t.Errorf("layer 0: %+v", p.Layers[0])
	}
	near(t, "min gradient", p.MinGradientNPerKm, -300, 1e-9)

	// Elevated duct: trapping layer 1000–1200 m.
	// M: 320.0, 378.5, 439.0, 428.4, 529.0, 661.0 → M(top) is met again at
	// 500 + 500·(428.4 − 378.5)/(439 − 378.5) = 912.4 m.
	p, _ = AnalyzePoints([]ProfilePoint{{HeightM: 0, N: 320}, {HeightM: 500, N: 300}, {HeightM: 1000, N: 282},
		{HeightM: 1200, N: 240}, {HeightM: 2000, N: 215}, {HeightM: 3000, N: 190}})
	if len(p.Ducts) != 1 {
		t.Fatalf("ducts: %+v", p.Ducts)
	}
	d = p.Ducts[0]
	if d.Kind != DuctElevated || d.TopM != 1200 || d.TrapBaseM != 1000 {
		t.Errorf("elevated duct: %+v", d)
	}
	near(t, "elevated base", d.BaseM, 912.4, 0.05)
	near(t, "elevated thickness", d.ThicknessM, 287.6, 0.05)
	near(t, "elevated ΔM", d.StrengthM, 10.6, 1e-9)
	near(t, "elevated λmax", d.MaxWavelengthM, 2.0/3*5.66e-3*d.ThicknessM*math.Sqrt(10.6), 1e-9)
	if !d.Traps1090 {
		t.Error("a 290 m elevated duct traps 1090 MHz")
	}

	// Surface-based duct: elevated trapping layer (200–400 m) strong enough
	// that M at its top is below the surface value. M: 335.0, 353.4, 332.8, 534.0
	p, _ = AnalyzePoints([]ProfilePoint{{HeightM: 0, N: 335}, {HeightM: 200, N: 322}, {HeightM: 400, N: 270}, {HeightM: 2000, N: 220}})
	if len(p.Ducts) != 1 || p.Ducts[0].Kind != DuctSurfaceBased || p.Ducts[0].BaseM != 0 || p.Ducts[0].TopM != 400 ||
		p.Ducts[0].TrapBaseM != 200 {
		t.Errorf("surface-based duct: %+v", p.Ducts)
	}

	// A thin, weak duct is found but cannot trap 1090 MHz. M: 320, 319.14, …
	p, _ = AnalyzePoints([]ProfilePoint{{HeightM: 0, N: 320}, {HeightM: 20, N: 316}, {HeightM: 1000, N: 280}, {HeightM: 2000, N: 250}})
	if len(p.Ducts) != 1 || p.Ducts[0].Traps1090 {
		t.Errorf("thin duct: %+v", p.Ducts)
	}

	// Two separate ducts.
	p, _ = AnalyzePoints([]ProfilePoint{{HeightM: 0, N: 340}, {HeightM: 100, N: 310}, {HeightM: 1000, N: 282},
		{HeightM: 1200, N: 240}, {HeightM: 3000, N: 190}})
	if len(p.Ducts) != 2 || p.Ducts[0].Kind != DuctSurface || p.Ducts[1].Kind != DuctElevated {
		t.Errorf("two ducts: %+v", p.Ducts)
	}
}

func TestAnalyzeProfileLevels(t *testing.T) {
	// Standard atmosphere at 60 % RH: a normal, duct-free profile with
	// dN/dh ≈ −40 N/km and k ≈ 4/3.
	var levels []Level
	for _, h := range []float64{3000, 0, 500, 1000, 1500, 2000} { // deliberately unsorted
		a := ISA(h)
		levels = append(levels, Level{HeightM: h, PressureHPa: a.PressureHPa, TempC: a.TempC, RH: 60})
	}
	levels = append(levels,
		Level{HeightM: 500.3, PressureHPa: 950, TempC: 11, RH: 60},       // duplicate height: dropped
		Level{HeightM: 800, PressureHPa: -1, TempC: 9, RH: 60},           // invalid: dropped
		Level{HeightM: 900, PressureHPa: 900, TempC: math.NaN(), RH: 60}, // invalid: dropped
		Level{HeightM: 1200, PressureHPa: 880, TempC: 7, RH: math.NaN()}) // invalid: dropped
	p, ok := AnalyzeProfile(levels)
	if !ok {
		t.Fatal("not ok")
	}
	if len(p.Points) != 6 {
		t.Fatalf("points: %d", len(p.Points))
	}
	near(t, "surface N", p.SurfaceN, 319.0, 0.5)
	near(t, "gradient", p.GradientNPerKm, -40, 3)
	near(t, "k", p.K, 4.0/3, 0.05)
	if p.Class != ClassStandard || len(p.Ducts) != 0 {
		t.Errorf("class %q ducts %+v", p.Class, p.Ducts)
	}
	near(t, "Level.N", levels[1].N(), p.SurfaceN, 1e-9)

	// A sharp temperature inversion with dry air above a moist surface layer
	// (classic anticyclonic / sea-fog situation) produces a duct.
	inv := []Level{
		{HeightM: 0, PressureHPa: 1020, TempC: 14, RH: 95},
		{HeightM: 150, PressureHPa: 1002, TempC: 13, RH: 95},
		{HeightM: 300, PressureHPa: 984, TempC: 20, RH: 25},
		{HeightM: 1000, PressureHPa: 905, TempC: 16, RH: 20},
		{HeightM: 3000, PressureHPa: 705, TempC: 3, RH: 20},
	}
	p, ok = AnalyzeProfile(inv)
	if !ok || len(p.Ducts) == 0 || p.Class != ClassDucting {
		t.Fatalf("inversion profile should duct: %+v", p)
	}
	if p.Ducts[0].TopM != 300 || !p.Ducts[0].Traps1090 {
		t.Errorf("inversion duct: %+v", p.Ducts[0])
	}

	if _, ok := AnalyzeProfile(nil); ok {
		t.Error("empty profile")
	}
	if _, ok := AnalyzeProfile(levels[:1]); ok {
		t.Error("single level")
	}
}

func TestAssess(t *testing.T) {
	a := AssessGradient(-40)
	if a.Index != 50 || a.Class != ClassStandard || a.Ducting1090 || len(a.Ducts) != 0 {
		t.Errorf("standard: %+v", a)
	}
	near(t, "std horizon factor", a.HorizonFactor, 1, 0.01)
	if !strings.Contains(a.Summary, "Standard refraction") {
		t.Errorf("summary: %s", a.Summary)
	}

	a = AssessGradient(50)
	if a.Index != 16 || a.Class != ClassSubRefraction || !strings.Contains(a.Summary, "Sub-refraction") || a.HorizonFactor >= 1 {
		t.Errorf("sub-refraction: %+v", a)
	}
	a = AssessGradient(-100)
	if a.Index != 70 || a.Class != ClassSuperRefraction || !strings.Contains(a.Summary, "Super-refraction") || a.HorizonFactor <= 1.2 {
		t.Errorf("super-refraction: %+v", a)
	}
	a = AssessGradient(-200)
	if a.Index < 85 || a.Class != ClassDucting || a.K != KMax || !strings.Contains(a.Summary, "Trapping") {
		t.Errorf("trapping gradient: %+v", a)
	}

	// The index never decreases as the gradient gets more negative, and
	// stays within 0..100.
	prev := -1
	for g := 200.0; g >= -600; g -= 5 {
		idx := AssessGradient(g).Index
		if idx < prev || idx < 0 || idx > 100 {
			t.Fatalf("index not monotonic at %v: %d after %d", g, idx, prev)
		}
		prev = idx
	}

	// Profile with a duct that traps 1090 MHz.
	p, _ := AnalyzePoints([]ProfilePoint{{HeightM: 0, N: 340}, {HeightM: 100, N: 310}, {HeightM: 1000, N: 275}, {HeightM: 3000, N: 230}})
	a = Assess(p)
	if !a.Ducting1090 || a.Index < 80 || a.Class != ClassDucting || len(a.Ducts) != 1 ||
		!strings.Contains(a.Summary, "Surface duct") || !strings.Contains(a.Summary, "can trap 1090 MHz") {
		t.Errorf("ducting assessment: %+v", a)
	}
	// A duct too thin for 1090 MHz adds only a little.
	p, _ = AnalyzePoints([]ProfilePoint{{HeightM: 0, N: 320}, {HeightM: 20, N: 316}, {HeightM: 1000, N: 280}, {HeightM: 2000, N: 250}})
	b := Assess(p)
	if b.Ducting1090 || b.Index >= a.Index || b.Index <= AssessGradient(p.GradientNPerKm).Index ||
		!strings.Contains(b.Summary, "too thin") {
		t.Errorf("thin duct assessment: %+v", b)
	}
	// Standard profile: same as the plain gradient.
	p, _ = AnalyzePoints(refProfile(0, 500, 1000, 3000))
	if Assess(p).Index != AssessGradient(p.GradientNPerKm).Index {
		t.Error("duct-free profile must score like its gradient")
	}

	// Everything must be JSON-encodable (no Inf/NaN), including trapping cases.
	for _, v := range []any{AssessGradient(-500), a, b, p} {
		if _, err := json.Marshal(v); err != nil {
			t.Errorf("json: %v", err)
		}
	}
}
