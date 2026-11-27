package ref

import (
	"math"
	"testing"
	"time"

	"airview/internal/weather"
)

// warmColumn returns the height (m) of the pressure pHPa in an atmosphere
// with sea-level pressure qnh and the ISA lapse rate, ΔT warmer than ISA.
func warmColumn(pHPa, qnh, dT float64) float64 {
	t0 := 288.15 + dT
	return t0 / 0.0065 * (1 - math.Pow(pHPa/qnh, 1/5.25588))
}

// testBundle is one location with three hours of surface data and a
// pressure-level profile of a warm (ISA+dT) column whose QNH rises by
// 2 hPa per hour.
func testBundle(lat, lon float64, t0 time.Time, dT float64, levels bool) *weather.Bundle {
	b := &weather.Bundle{Source: weather.Source, Lat: lat, Lon: lon, Fetched: t0.UnixMilli()}
	for h := 0; h < 3; h++ {
		qnh := 1020.0 + 2*float64(h)
		hp := weather.HourPoint{T: t0.Add(time.Duration(h) * time.Hour).UnixMilli()}
		hp.PressureMSL, hp.TempC = weather.Num(qnh), weather.Num(15+dT)
		if levels {
			for _, p := range []float64{1000, 925, 850, 700, 500, 300, 250, 200} {
				z := warmColumn(p, qnh, dT)
				gh := earthRgpm * z / (earthRgpm + z) // geopotential height of that geometric height
				hp.Levels = append(hp.Levels, weather.Level{HPa: p, TempC: weather.NaN, RH: weather.NaN,
					HeightM: weather.Num(gh), WindKmh: weather.NaN, WindDir: weather.NaN})
			}
		}
		b.Hourly = append(b.Hourly, hp)
	}
	return b
}

func TestBundleWeather(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	w := NewBundleWeather()
	w.Set(testBundle(50, 9, t0, 12, true))
	w.Set(testBundle(50.01, 9.01, t0, 12, true)) // same place: replaces
	if w.Len() != 1 {
		t.Fatalf("%d locations, want 1", w.Len())
	}
	at := t0.Add(90 * time.Minute).UnixNano()
	qnh, temp, ok := w.SurfaceAt(50.5, 9.5, at)
	if !ok || math.Abs(qnh-1023) > 1e-9 || math.Abs(temp-27) > 1e-9 {
		t.Fatalf("SurfaceAt = %v %v %v, want 1023 hPa (interpolated) and 27 °C", qnh, temp, ok)
	}
	if _, _, ok := w.SurfaceAt(55, 9, at); ok {
		t.Error("answered 550 km away")
	}
	if _, _, ok := w.SurfaceAt(50, 9, t0.Add(5*time.Hour).UnixNano()); ok {
		t.Error("answered outside the bundle's hours")
	}
	// profile: the 500 hPa surface at t0 + 1 h (QNH 1022)
	h, ok := w.MSLHeightOfPressure(50, 9, 500, t0.Add(time.Hour).UnixNano())
	if want := warmColumn(500, 1022, 12); !ok || math.Abs(h-want) > 0.5 {
		t.Fatalf("500 hPa at %.1f m, want %.1f (%v)", h, want, ok)
	}
	// between levels: log-pressure interpolation is within a metre or two
	h, _ = w.MSLHeightOfPressure(50, 9, 600, t0.Add(time.Hour).UnixNano())
	if want := warmColumn(600, 1022, 12); math.Abs(h-want) > 3 {
		t.Errorf("600 hPa at %.1f m, want %.1f", h, want)
	}
	if _, ok := w.MSLHeightOfPressure(50, 9, 100, at); ok {
		t.Error("answered above the top level")
	}
	if n := testing.AllocsPerRun(100, func() { w.SurfaceAt(50, 9, at); w.MSLHeightOfPressure(50, 9, 400, at) }); n != 0 {
		t.Errorf("%v allocations per query", n)
	}
}

// TestFieldWithWeatherBundles: the GNSS−baro fallback through the adapter.
// In a column 12 K warmer than ISA a pressure altitude of FL350 lies
// hundreds of metres above its ISA height; the geoid-only fallback misses
// that, the QNH/temperature fallback gets most of it and the profile all of
// it, each with a σ that covers its error.
func TestFieldWithWeatherBundles(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	at := t0.Add(time.Hour).UnixNano()
	const lat, lon, fl = 50.0, 9.0, 35000.0
	hp := fl * ftM
	none := NewField(FieldOptions{}, nil)
	n, _ := none.Fallback(lat, lon, 0, at) // geoid undulation
	truth := warmColumn(isaPressure(hp), 1022, 12) + n - hp
	cases := []struct {
		name string
		wx   Weather
	}{{"none", nil}}
	surf := NewBundleWeather()
	surf.Set(testBundle(lat, lon, t0, 12, false))
	cases = append(cases, struct {
		name string
		wx   Weather
	}{"surface", surf})
	prof := NewBundleWeather()
	prof.Set(testBundle(lat, lon, t0, 12, true))
	cases = append(cases, struct {
		name string
		wx   Weather
	}{"profile", prof})
	var errs []float64
	for _, c := range cases {
		f := NewField(FieldOptions{}, c.wx)
		d, sig := f.Fallback(lat, lon, fl, at)
		e := d - truth
		t.Logf("%-8s offset %7.1f m (truth %7.1f), error %6.1f m, σ %5.1f m", c.name, d, truth, e, sig)
		if math.Abs(e) > 3*sig {
			t.Errorf("%s: error %.1f m outside 3σ (%.1f m)", c.name, e, sig)
		}
		errs = append(errs, math.Abs(e))
	}
	if !(errs[2] < 10 && errs[1] < errs[0]) {
		t.Errorf("errors none/surface/profile %v: weather must help", errs)
	}
}
