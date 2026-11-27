package analytics

import (
	"context"
	"os"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

// TestOverviewMeasure is an opt-in measurement of /api/stats/overview on a
// COPY of a real database (the hourly maintenance writes the flights_hour
// rollup into it):
//
//	AIRVIEW_DBEFF_C_OV=<dir holding airview.db> go test ./internal/analytics -run TestOverviewMeasure -v
func TestOverviewMeasure(t *testing.T) {
	dir := os.Getenv("AIRVIEW_DBEFF_C_OV")
	if dir == "" {
		t.Skip("set AIRVIEW_DBEFF_C_OV to a directory with a copy of airview.db")
	}
	ctx := context.Background()
	now := time.Now()
	run := func(label string) string {
		st, err := store.OpenWithOptions(dir, store.Options{MigrationDelay: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		cfg := model.DefaultConfig()
		for _, off := range []int{0, 3600, 19800, -28800} {
			a := New(st, func() model.Config { return cfg })
			a.SetClock(func() time.Time { return now })
			a.SetLocation(time.FixedZone("x", off))
			t0 := time.Now()
			ov, err := a.Overview(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			r1 := time.Since(t0)
			a.Invalidate()
			t0 = time.Now()
			if _, err := a.Overview(ctx, ""); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s offset %+d: overview r1 %v, recomputed %v", label, off, r1, time.Since(t0))
			if off == 0 {
				a.Invalidate()
				t0 = time.Now()
				_, _ = a.Traffic(ctx, LastHours(24), "")
				tt := time.Since(t0)
				t0 = time.Now()
				_, _ = a.Daily(ctx, LastDays(30), "")
				td := time.Since(t0)
				t0 = time.Now()
				_, _ = a.Top(ctx, LastDays(30), 5)
				tp := time.Since(t0)
				t.Logf("%s parts: traffic %v, daily %v, top %v", label, tt, td, tp)
			}
			if off == 19800 {
				label += ""
				_ = ov
			}
		}
		a := New(st, func() model.Config { return cfg })
		a.SetClock(func() time.Time { return now })
		a.SetLocation(time.FixedZone("x", 19800))
		d, err := a.Daily(ctx, LastDays(30), "")
		if err != nil {
			t.Fatal(err)
		}
		return mustJSON(t, "daily", d)
	}
	before := run("1.0.0 path")
	st, err := store.OpenWithOptions(dir, store.Options{MigrationDelay: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	rep, err := st.HourlyMaintenance(ctx)
	t.Logf("hourly maintenance %v: %+v", time.Since(t0), rep)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	after := run("rollup")
	if before != after {
		t.Fatalf("daily answers differ:\n%s\n%s", before, after)
	}
}
