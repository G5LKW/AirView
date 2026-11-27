package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/store"
)

// overlayIncs returns increments around a receiver at (50, 8) for the days
// before testNow and the open day.
func overlayIncs(rng *rand.Rand, n int) store.PendingIncs {
	today := store.DayOf(testNow.Unix())
	var p store.PendingIncs
	for i := 0; i < n; i++ {
		ant := []string{"", "yagi"}[rng.Intn(2)]
		day := today - rng.Intn(5)
		brg := rng.Float64() * 360
		km := 20 + rng.Float64()*300
		lat, lon := geo.Destination(50, 8, brg, km)
		alt := 1000 + rng.Float64()*38000
		rssi := -3 - rng.Float64()*30
		cnt := int64(1 + rng.Intn(20))
		p.Coverage = append(p.Coverage, store.CoverageInc{Day: day, Ant: ant, BrgBin: store.BrgBin(brg), AltBand: store.AltBand(alt),
			MaxKm: km, Count: cnt, RSSISum: rssi * float64(cnt), RSSIN: cnt})
		p.Signal = append(p.Signal, store.SignalHistInc{Day: day, Ant: ant, DistBin: store.DistBin(km), RSSIBin: store.RSSIBin(rssi), Count: cnt})
		p.Elev = append(p.Elev, store.ElevHistInc{Day: day, Ant: ant, ElevBin: store.ElevBin(math.Atan2(alt*0.3048/1000, km) * 180 / math.Pi),
			Count: cnt, RSSISum: rssi * float64(cnt), ResidSum: (rssi + 20*math.Log10(km)) * float64(cnt)})
		p.Heat = append(p.Heat, store.HeatInc{Cell: store.HeatCell(lat, lon), Ant: ant, Count: cnt, RSSISum: rssi * float64(cnt), RSSIN: cnt,
			MinAlt: alt, MaxAlt: alt + 500, LastT: store.DayStart(day) + int64(rng.Intn(86400))})
	}
	return p
}

// TestPendingOverlayEqualsWritten: every analysis that covers the open day
// or hour answers the same with the increments pending in the tracker as
// with them written.
func TestPendingOverlayEqualsWritten(t *testing.T) {
	cfg := locatedConfig(50, 8)
	_, st := newTest(t, cfg)
	rng := rand.New(rand.NewSource(42))
	base := overlayIncs(rng, 20000)
	pend := overlayIncs(rng, 3000)
	hour := testNow.Unix() / 3600 * 3600
	nan := math.NaN()
	mkRange := func(t0 int64, km float64, pos int64) store.RangeHour {
		h := make([]uint32, store.DistHistBins)
		h[int(km/store.DistHistBinKm)] = uint32(pos)
		s := make([]float32, store.SectorBins)
		s[3] = float32(km)
		return store.RangeHour{T: t0, Ant: "", MaxKm: km, P99Km: nan, P95Km: nan, P90Km: nan, P50Km: nan, Positions: pos, Aircraft: 3,
			ResidDB: 40, ResidN: pos, DistHist: h, SectorMax: s}
	}
	base.Range = []store.RangeHour{mkRange(hour-3600, 150, 100), mkRange(hour, 120, 40)}
	pend.Range = []store.RangeHour{mkRange(hour, 210, 30)}
	write := func(p store.PendingIncs) {
		apply(t, st, func(b *store.Batch) {
			b.MergeCoverage(p.Coverage)
			b.MergeSignalHist(p.Signal)
			b.MergeElevHist(p.Elev)
			b.MergeHeat(p.Heat)
			b.AddRangeHours(p.Range)
		})
	}
	write(base)

	answers := func(a *Analyzer) map[string]any {
		ctx := context.Background()
		out := map[string]any{}
		add := func(name string, v any, err error) {
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			out[name] = v
		}
		v1, err := a.Coverage(ctx, Range{}, "")
		add("coverage", v1, err)
		v2, err := a.CoverageOutline(ctx, Range{}, "yagi", OutlineOptions{Band: 4})
		add("outline", v2, err)
		v3, err := a.HeatGrid(ctx, "", HeatOptions{})
		add("heat", v3, err)
		v4, err := a.Antenna(ctx, Range{}, "yagi")
		add("antenna", v4, err)
		v5, err := a.AltitudeDistribution(ctx, Range{}, "")
		add("altitudes", v5, err)
		v6, err := a.RangeHours(ctx, "", 0, 0)
		add("range", v6, err)
		return out
	}
	withPending := New(st, func() model.Config { return cfg }, WithPending(func() store.PendingIncs { return pend }))
	withPending.SetClock(func() time.Time { return testNow })
	withPending.SetLocation(time.UTC)
	over := answers(withPending)
	stale := New(st, func() model.Config { return cfg })
	stale.SetClock(func() time.Time { return testNow })
	stale.SetLocation(time.UTC)
	if jsonNear(mustJSON(t, "c", answers(stale)["heat"]), mustJSON(t, "c", over["heat"])) == nil {
		t.Fatal("the overlay changed nothing")
	}

	write(pend)
	plain := New(st, func() model.Config { return cfg })
	plain.SetClock(func() time.Time { return testNow })
	plain.SetLocation(time.UTC)
	written := answers(plain)
	for name := range over {
		if err := jsonNear(mustJSON(t, name, over[name]), mustJSON(t, name, written[name])); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// jsonNear compares two JSON documents; numbers may differ by 1e-9
// relative (REAL sums taken in another order).
func jsonNear(a, b string) error {
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		return err
	}
	return near(x, y, "$")
}

func near(x, y any, path string) error {
	switch xv := x.(type) {
	case map[string]any:
		yv, ok := y.(map[string]any)
		if !ok || len(xv) != len(yv) {
			return fmt.Errorf("%s: object differs", path)
		}
		for k := range xv {
			if err := near(xv[k], yv[k], path+"."+k); err != nil {
				return err
			}
		}
	case []any:
		yv, ok := y.([]any)
		if !ok || len(xv) != len(yv) {
			return fmt.Errorf("%s: array length differs", path)
		}
		for i := range xv {
			if err := near(xv[i], yv[i], fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case float64:
		yv, ok := y.(float64)
		if !ok || math.Abs(xv-yv) > 1e-9*math.Max(1, math.Abs(yv)) {
			return fmt.Errorf("%s: %v vs %v", path, x, y)
		}
	default:
		if x != y {
			return fmt.Errorf("%s: %v vs %v", path, x, y)
		}
	}
	return nil
}
