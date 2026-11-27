package ref

import (
	"context"
	"math"
	"testing"
	"time"

	"airview/internal/geoid"
	"airview/internal/sim/simnet"
)

func TestPressureToMSL(t *testing.T) {
	// standard atmosphere: identity
	for _, h := range []float64{0, 1000, 5000, 10000} {
		if d := PressureToMSL(h, 1013.25, 15) - h; math.Abs(d) > 0.01 {
			t.Fatalf("ISA identity off by %.3f m at %.0f m", d, h)
		}
	}
	// QNH +10 hPa: ≈ 8.3 m/hPa near the ground
	if d := PressureToMSL(0, 1023.25, 15); math.Abs(d-83) > 3 {
		t.Fatalf("QNH +10 hPa at the surface: %.1f m", d)
	}
	// +10 °C warmer: a 3000 m layer is ≈ 3000·10/278 ≈ 108 m thicker
	if d := PressureToMSL(3000, 1013.25, 25) - 3000; math.Abs(d-108) > 5 {
		t.Fatalf("+10 °C at 3000 m: %.1f m", d)
	}
}

type fixedWx struct{ qnh, t float64 }

func (w fixedWx) SurfaceAt(float64, float64, int64) (float64, float64, bool) { return w.qnh, w.t, true }

func TestFieldLearnsAndRelaxes(t *testing.T) {
	f := NewField(FieldOptions{}, nil)
	n, ok := geoid.Undulation(50.2, 8.3)
	if !ok {
		t.Skip("no geoid")
	}
	t0 := int64(1_700_000_000_000_000_000)
	// fallback without weather: the geoid undulation, σ 150 m at the
	// surface growing with height (≈ 487 m at FL350)
	fb := sigmaAt(150, 11, 35000*ftM)
	d, s, src := f.Offset(50.2, 8.3, 35000, t0)
	if math.Abs(d-n) > 0.5 || math.Abs(s-fb) > 5 || src != AltFallback {
		t.Fatalf("fallback %.1f ± %.1f (%v), geoid %.1f", d, s, src, n)
	}
	// 40 decimated observations of +120 m around one cell centre
	k := f.key(50.2, 8.3, 35000)
	clat, clon, cb := f.center(k)
	for i := 0; i < 40; i++ {
		f.Update(clat, clon, cb, 120, 12, t0+int64(i)*30e9)
	}
	tn := t0 + 40*30e9
	d, s, src = f.Offset(clat, clon, cb, tn)
	if math.Abs(d-120) > 3 || s > 5 || src != AltField {
		t.Fatalf("learned %.1f ± %.1f (%v), want 120", d, s, src)
	}
	h, _, _ := f.BaroToHAE(clat, clon, cb, tn)
	if math.Abs(h-(cb*ftM+120)) > 3 {
		t.Fatalf("BaroToHAE %.1f", h)
	}
	// 6 h later the value is kept with σ ≈ 20 m·√6 ≈ 49 m
	d, s, src = f.Offset(clat, clon, cb, tn+6*3600e9)
	if math.Abs(d-120) > 3 || math.Abs(s-49) > 3 || src != AltField {
		t.Fatalf("after 6 h %.1f ± %.1f (%v), want 120 ± 49", d, s, src)
	}
	// after (σ_fallback/20)² ≈ 590 h it is no better than the fallback
	d, s, src = f.Offset(clat, clon, cb, tn+600*3600e9)
	if math.Abs(d-n) > 0.5 || s < fb-5 || src != AltFallback {
		t.Fatalf("after 600 h %.1f ± %.1f (%v), want ≈ geoid %.1f ± %.0f", d, s, src, n, fb)
	}
	if st := f.Stats(); st.Cells != 1 || st.Observations != 40 {
		t.Fatalf("stats %+v", st)
	}
	if f.Prune(tn+60*3600e9) != 0 || f.Prune(tn+600*3600e9) != 1 {
		t.Fatal("prune")
	}
}

func TestFieldWeatherFallback(t *testing.T) {
	f := NewField(FieldOptions{}, fixedWx{1030, 5})
	d, s, src := f.Offset(50, 8, 10000, 0)
	n, _ := geoid.Undulation(50, 8)
	want := PressureToMSL(10000*ftM, 1030, 5) + n - 10000*ftM
	ws := sigmaAt(60, 6, 10000*ftM) // ≈ 89 m at FL100
	if math.Abs(d-want) > 0.5 || math.Abs(s-ws) > 2 || src != AltFallback {
		t.Fatalf("weather fallback %.1f ± %.1f, want %.1f ± %.1f", d, s, want, ws)
	}
}

// Finding M3: the fallback σ was a constant 150 m (60 m with weather) at
// every altitude, but a non-ISA column moves a pressure level by about
// h·ΔT/T_mean. ISA+15 K at FL390 puts the aircraft ≈ 715 m above the
// no-weather fallback, which must stay within about 2σ; near the ground
// the σ keeps its base value.
func TestFallbackSigmaGrowsWithHeight(t *testing.T) {
	f := NewField(FieldOptions{}, nil)
	hp := 39000 * ftM
	errM := PressureToMSL(hp, 1013.25, 30) - hp // ISA+15 K
	if errM < 650 || errM > 780 {
		t.Fatalf("ISA+15 K at FL390: %.0f m, want ≈ 715", errM)
	}
	_, s := f.Fallback(50, 8, 39000, 0)
	if errM/s > 2 {
		t.Fatalf("FL390 no-weather σ %.0f m: a %.0f m error is %.1f σ", s, errM, errM/s)
	}
	_, s0 := f.Fallback(50, 8, 0, 0)
	if math.Abs(s0-150) > 0.5 {
		t.Fatalf("surface σ %.1f, want 150", s0)
	}
	_, s1 := f.Fallback(50, 8, 10000, 0)
	_, s2 := f.Fallback(50, 8, 20000, 0)
	if !(s0 < s1 && s1 < s2 && s2 < s) {
		t.Fatalf("σ not increasing with height: %.0f %.0f %.0f %.0f", s0, s1, s2, s)
	}
	// with surface weather: upper-air deviations of 10 K from the surface
	// value (≈ 400 m at FL400) stay within about 2σ
	fw := NewField(FieldOptions{}, fixedWx{1013.25, 15})
	_, sw := fw.Fallback(50, 8, 40000, 0)
	up := 40000 * ftM * 10 / isaMeanTemp(40000*ftM)
	if up/sw > 2 {
		t.Fatalf("FL400 weather σ %.0f m: a 10 K upper-air deviation (%.0f m) is %.1f σ", sw, up, up/sw)
	}
	// cells are initialised and relaxed with the same σ
	k := fw.key(50, 8, 40000)
	clat, clon, cb := fw.center(k)
	fw.Update(clat, clon, cb, 0, 1e6, 0) // an uninformative observation
	if _, p, _ := fw.cellAt(k, 0); math.Abs(math.Sqrt(p)-sigmaAt(60, 6, cb*ftM)) > 1 {
		t.Fatalf("cell initialised with σ %.1f, want %.1f", math.Sqrt(p), sigmaAt(60, 6, cb*ftM))
	}
}

// TestSimnetField: after six minutes of simnet traffic the field converts
// the pressure altitudes of every airborne aircraft (as for a Mode S-only
// target) into heights above the ellipsoid; compared with the true GNSS
// height. The fallback (geoid only, no weather) is shown for contrast.
func TestSimnetField(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	cfg := simnet.Config{Seed: 21, Aircraft: 200, PosLatencyMaxMs: -1, PosErrorScale: -1,
		Receivers: simnet.RandomReceivers(21, 8, simnet.Placement{CenterLat: 50.5, CenterLon: 8, RadiusKm: 150})}
	n, err := simnet.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(n, New(Options{}, nil), 0.4)
	n.AddSink(h)
	if err := n.Advance(context.Background(), 6*time.Minute); err != nil {
		t.Fatal(err)
	}
	f := h.s.Field()
	var ef, efb []float64
	var inField int
	for _, a := range n.Aircraft() {
		if a.Sim.OnGround || a.Sim.AltBaro < 1000 {
			continue
		}
		tn := n.UnixNs(a.AtS)
		hae, sig, src := f.BaroToHAE(a.Lat, a.Lon, a.Sim.AltBaro, tn)
		fb, _ := f.Fallback(a.Lat, a.Lon, a.Sim.AltBaro, tn)
		e := hae - a.HAE
		if src == AltField {
			inField++
			ef = append(ef, math.Abs(e))
			if math.Abs(e) > 4*sig+10 {
				t.Errorf("aircraft %06x: field error %.1f m with σ %.1f", a.ICAO, e, sig)
			}
		}
		efb = append(efb, math.Abs(a.Sim.AltBaro*ftM+fb-a.HAE))
	}
	t.Logf("field: %d aircraft in learned cells: |error| P50 %.1f P95 %.1f max %.1f m; fallback (geoid only) P50 %.1f P95 %.1f m; %+v",
		inField, pct(ef, 50), pct(ef, 95), pct(ef, 100), pct(efb, 50), pct(efb, 95), f.Stats())
	if inField < 50 {
		t.Fatalf("only %d aircraft in learned cells", inField)
	}
	if pct(ef, 95) > 25 {
		t.Errorf("field P95 %.1f m > 25 m", pct(ef, 95))
	}
}
