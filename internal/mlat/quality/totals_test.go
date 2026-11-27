package quality

import (
	"testing"

	"airview/internal/model"
)

// TestStationTotals: cumulative used rows and outliers (excluded or
// |w| > 4) per station, kept across Forget.
func TestStationTotals(t *testing.T) {
	s := NewStations()
	if _, _, ok := s.Totals(1); ok {
		t.Fatal("unknown station reported")
	}
	now := int64(1_700_000_000e9)
	for i := 0; i < 10; i++ {
		s.Observe(now+int64(i)*1e9, []Residual{
			{Station: 1, W: 0.5},
			{Station: 2, W: 5},           // used outlier
			{Station: 3, Excluded: true}, // excluded
		}, nil)
	}
	for _, c := range []struct {
		st        int
		fix, outl uint64
	}{{1, 10, 0}, {2, 10, 10}, {3, 0, 10}} {
		f, o, ok := s.Totals(model.StationIdx(c.st))
		if !ok || f != c.fix || o != c.outl {
			t.Fatalf("station %d: fixes %d outliers %d ok %v, want %d %d", c.st, f, o, ok, c.fix, c.outl)
		}
	}
	s.Forget(now + 3600e9)
	if f, _, ok := s.Totals(1); !ok || f != 10 {
		t.Fatalf("totals lost by Forget: %d %v", f, ok)
	}
	s.Observe(now+3700e9, []Residual{{Station: 1, W: 0.1}}, nil)
	if f, _, _ := s.Totals(1); f != 11 {
		t.Fatalf("totals not monotonic after Forget: %d", f)
	}
}
