package analytics

import (
	"context"
	"testing"

	"airview/internal/geo"
)

// Bearings without data are left out of the outline (not drawn at the
// receiver); fewer than three bearings give no feature.
func TestOutlineSkipsEmptyBearings(t *testing.T) {
	a, st := newTest(t, locatedConfig(50, 8))
	ctx := context.Background()
	// Band 4 covers only 90..180 degrees, band 2 only two bearing bins.
	plantCoverage(t, st, 3, func(band int, brg float64) float64 {
		switch {
		case band == 4 && brg >= 90 && brg < 180:
			return 300
		case band == 2 && brg >= 10 && brg < 14:
			return 100
		}
		return 0
	})
	fc, err := a.CoverageOutline(ctx, Range{}, "", OutlineOptions{Band: 4, Kind: "max"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.Features) != 1 {
		t.Fatalf("band 4 features: %d", len(fc.Features))
	}
	ring := fc.Features[0].Geometry.Coordinates[0]
	if len(ring) != 45+1 || ring[0] != ring[len(ring)-1] {
		t.Fatalf("ring has %d points, want 45 measured bearings + closing point", len(ring))
	}
	for _, p := range ring {
		if d := geo.HaversineKm(50, 8, p[1], p[0]); d < 250 {
			t.Fatalf("ring point %v at %.1f km: an empty bearing collapsed onto the receiver", p, d)
		}
	}
	pr := fc.Features[0].Properties
	if pr["band"] != 4 || pr["loFt"] != 20000.0 || pr["hiFt"] != 30000.0 || pr["kind"] != "max" {
		t.Errorf("properties %v", pr)
	}
	if fc, _ := a.CoverageOutline(ctx, Range{}, "", OutlineOptions{Band: 2}); len(fc.Features) != 0 {
		t.Errorf("band with 2 bearings drew %d features", len(fc.Features))
	}
	if fc, _ := a.CoverageOutline(ctx, Range{}, "", OutlineOptions{Band: 0}); len(fc.Features) != 0 {
		t.Errorf("band without data drew %d features", len(fc.Features))
	}
	// Legacy call: overall + band 4, both kinds; band 2 has no feature.
	all, _ := a.CoverageGeoJSON(ctx, Range{}, "")
	if len(all.Features) != 4 {
		t.Errorf("CoverageGeoJSON features: %d", len(all.Features))
	}
}

func TestOutlineAllBands(t *testing.T) {
	a, st := newTest(t, locatedConfig(50, 8))
	ctx := context.Background()
	plantCoverage(t, st, 3, func(band int, brg float64) float64 {
		if band == 6 {
			return 0
		}
		return 50 + 60*float64(band)
	})
	fc, err := a.CoverageOutline(ctx, Range{}, "", OutlineOptions{Band: OutlineAllBands, Kind: "robust"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.Features) != 6 {
		t.Fatalf("features %d, want one per band with data (6)", len(fc.Features))
	}
	for i, f := range fc.Features {
		pr := f.Properties
		lo, hi := geo.AltBandBoundsFt(i)
		wantLo := any(lo)
		if i == 0 {
			wantLo = nil
		}
		if pr["band"] != i || pr["kind"] != "robust" || len(f.Geometry.Coordinates[0]) != 181 {
			t.Errorf("feature %d: %v", i, pr)
		}
		if (i == 0 && pr["loFt"] != 0.0) || (i > 0 && pr["loFt"] != wantLo) || pr["hiFt"] != hi {
			t.Errorf("feature %d bounds lo %v hi %v", i, pr["loFt"], pr["hiFt"])
		}
	}
	ov, _ := a.CoverageOutline(ctx, Range{}, "", OutlineOptions{Band: OutlineOverall})
	if len(ov.Features) != 2 || ov.Features[0].Properties["band"] != -1 || ov.Features[0].Properties["hiFt"] != nil {
		t.Errorf("overall: %d features", len(ov.Features))
	}
	if _, err := a.CoverageOutline(ctx, Range{}, "", OutlineOptions{Band: 7}); err == nil {
		t.Error("band 7 accepted")
	}
	for s, want := range map[string]int{"all": OutlineAllBands, "-1": OutlineOverall, "0": 0, "6": 6} {
		if b, ok := ParseOutlineBand(s); !ok || b != want {
			t.Errorf("ParseOutlineBand(%q) = %d %v", s, b, ok)
		}
	}
	for _, s := range []string{"", "7", "-2", "x"} {
		if _, ok := ParseOutlineBand(s); ok {
			t.Errorf("ParseOutlineBand(%q) accepted", s)
		}
	}
}
