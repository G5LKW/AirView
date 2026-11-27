package met

import (
	"math"
	"testing"
	"time"
)

func TestWMMOfficialTestValues(t *testing.T) {
	if len(wmmTestValues) < 50 {
		t.Fatalf("test value table too short: %d", len(wmmTestValues))
	}
	var worstD, worstNT float64
	for _, v := range wmmTestValues {
		f, ok := MagField(v.lat, v.lon, v.altKm, v.year)
		if !ok {
			t.Fatalf("MagField(%v,%v) not ok", v.lat, v.lon)
		}
		dd := math.Abs(angDiff(f.D, v.decl))
		// The table prints 2 decimals; near the poles H is tiny, so allow
		// for the rounding of the published value only.
		if dd > 0.006 {
			t.Errorf("decl at %v/%v alt %v year %v: got %.4f want %.2f", v.lat, v.lon, v.altKm, v.year, f.D, v.decl)
		}
		if math.Abs(f.I-v.incl) > 0.006 {
			t.Errorf("incl at %v/%v: got %.4f want %.2f", v.lat, v.lon, f.I, v.incl)
		}
		for _, c := range [][2]float64{{f.X, v.x}, {f.Y, v.y}, {f.Z, v.z}, {f.H, v.h}, {f.F, v.f}} {
			e := math.Abs(c[0] - c[1])
			worstNT = math.Max(worstNT, e)
			if e > 0.05 {
				t.Errorf("component at %v/%v year %v: got %.3f want %.3f", v.lat, v.lon, v.year, c[0], c[1])
			}
		}
		worstD = math.Max(worstD, dd)
	}
	t.Logf("%d official test points: worst declination error %.4f deg, worst component error %.4f nT",
		len(wmmTestValues), worstD, worstNT)
}

func TestMagVar(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// Sanity of the sign convention: western Europe is slightly east by
	// 2026, the US west coast strongly east, the US east coast west.
	cases := []struct {
		name     string
		lat, lon float64
		lo, hi   float64
	}{
		{"London", 51.5, -0.1, 0.5, 2.5},
		{"Seattle", 47.6, -122.3, 13.5, 16.5},
		{"Boston", 42.4, -71.0, -15.5, -12.5},
		{"Sydney", -33.9, 151.2, 11.5, 14},
	}
	for _, c := range cases {
		d := MagVar(c.lat, c.lon, 10, t0)
		if !(d > c.lo && d < c.hi) {
			t.Errorf("%s: declination %.2f outside [%v, %v]", c.name, d, c.lo, c.hi)
		}
	}
	if !math.IsNaN(MagVar(math.NaN(), 0, 0, t0)) || !math.IsNaN(MagVar(95, 0, 0, t0)) {
		t.Error("invalid position must give NaN, not a value")
	}
	if d := MagVar(90, 0, 0, t0); !finite(d) {
		t.Errorf("pole: %v", d)
	}
	if y := DecimalYear(time.Date(2025, 7, 2, 12, 0, 0, 0, time.UTC)); math.Abs(y-2025.5) > 1e-9 {
		t.Errorf("DecimalYear = %v", y)
	}
}

func BenchmarkMagVar(b *testing.B) {
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		MagVar(48+float64(i%10)*0.1, 11, 10, t0)
	}
}
