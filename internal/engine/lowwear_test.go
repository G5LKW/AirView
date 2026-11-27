package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"airview/internal/store"
)

func sumPending(p store.PendingIncs) (heat, cov, sig, elev int64) {
	for _, h := range p.Heat {
		heat += h.Count
	}
	for _, c := range p.Coverage {
		cov += c.Count
	}
	for _, s := range p.Signal {
		sig += s.Count
	}
	for _, e := range p.Elev {
		elev += e.Count
	}
	return
}

func sumStored(t *testing.T, db *sql.DB) (heat, cov, sig, elev int64) {
	t.Helper()
	q := func(table string) int64 {
		var n sql.NullInt64
		if err := db.QueryRow(`SELECT SUM(count) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		return n.Int64
	}
	return q("heat"), q("coverage"), q("signal_hist"), q("elev_hist")
}

// TestLowWearStopLosesNothing: with the low-wear profile the heat and day
// statistics stay in memory for 5-15 minutes; a graceful stop (SIGTERM,
// service stop) writes all of them, and a restart starts from exactly
// what was counted.
func TestLowWearStopLosesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the demo for ~20 s")
	}
	dir := t.TempDir()
	opts := Options{Demo: true, Offline: true, DemoSeed: 7, Logger: quietLogger(),
		ConfigSets: []string{"storage.profile=low-wear"}}
	e := New(dir, opts)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := e.StorageProfile(); p.Name != store.ProfileLowWear || p.HeatFlush != 15*time.Minute {
		e.Stop()
		t.Fatalf("profile %+v", p.View())
	}
	waitFor(t, "aircraft with positions", 30*time.Second, func() bool { return withPositions(e) >= 3 })
	// Let a few hundred positions accumulate in the tracker.
	var pendHeat int64
	waitFor(t, "pending heat increments", 30*time.Second, func() bool {
		pendHeat, _, _, _ = sumPending(e.Tracker().PendingIncs())
		return pendHeat >= 200
	})
	h0, c0, s0, e0 := sumStored(t, e.Store().DB())
	ph, pc, ps, pe := sumPending(e.Tracker().PendingIncs())
	before := [4]int64{h0 + ph, c0 + pc, s0 + ps, e0 + pe}
	if h0 != 0 {
		t.Logf("heat already written before the stop: %d (expected none within 15 min)", h0)
	}

	e.Stop() // the same path as SIGTERM / service stop (cmd/airview a.shutdown)

	if ph, pc, ps, pe := sumPending(e.Tracker().PendingIncs()); ph+pc+ps+pe != 0 {
		t.Fatalf("still pending after stop: heat %d cov %d sig %d elev %d", ph, pc, ps, pe)
	}

	// Restart on the same directory: the database holds at least what was
	// counted before the stop (frames that arrived until the feeds closed
	// add to it), nothing is pending yet.
	e2 := New(dir, Options{Offline: true, Logger: quietLogger(), ConfigSets: []string{"storage.profile=low-wear"}})
	if err := e2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e2.Stop()
	h1, c1, s1, e1 := sumStored(t, e2.Store().DB())
	after := [4]int64{h1, c1, s1, e1}
	names := []string{"heat", "coverage", "signal_hist", "elev_hist"}
	for i := range after {
		if after[i] < before[i] || after[i] == 0 && i < 2 {
			t.Errorf("%s: %d stored after the restart, %d counted before the stop", names[i], after[i], before[i])
		}
	}
	// Trail points (60-s trail stream) and flights reached the database too.
	counts, err := e2.Store().TableCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts["positions"] == 0 || counts["flights"] == 0 {
		t.Errorf("after restart: %d trail points, %d flights", counts["positions"], counts["flights"])
	}
	t.Logf("before stop (stored+pending) %v, after restart %v", before, after)
}
