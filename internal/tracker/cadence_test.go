package tracker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"airview/internal/sim"
	"airview/internal/store"
)

// incTotals sums what the database holds plus an overlay, per table and
// key, the way readers merge them (counts and sums are additive).
type incTotals map[string]float64

func (m incTotals) add(key string, v float64) { m[key] += v }

func dbTotals(t *testing.T, st *store.Store) incTotals {
	t.Helper()
	out := incTotals{}
	q := func(table, query string) {
		rs, err := st.DB().Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			var k string
			var v float64
			if err := rs.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			out.add(table+"/"+k, v)
		}
	}
	q("cov", `SELECT day || '/' || ant || '/' || brg_bin || '/' || alt_band, count FROM coverage`)
	q("covn", `SELECT day || '/' || ant || '/' || brg_bin || '/' || alt_band, rssi_n FROM coverage`)
	q("sig", `SELECT day || '/' || ant || '/' || dist_bin || '/' || rssi_bin, count FROM signal_hist`)
	q("elev", `SELECT day || '/' || ant || '/' || elev_bin, count FROM elev_hist`)
	q("heat", `SELECT ant || '/' || cell, count FROM heat`)
	q("heatn", `SELECT ant || '/' || cell, rssi_n FROM heat`)
	q("range", `SELECT t || '/' || ant, positions FROM range_hour`)
	q("rangefar", `SELECT t || '/' || ant, far_count FROM range_hour`)
	return out
}

func (m incTotals) addPending(p store.PendingIncs) {
	for _, r := range p.Coverage {
		k := fmt.Sprintf("%d/%s/%d/%d", r.Day, r.Ant, r.BrgBin, r.AltBand)
		m.add("cov/"+k, float64(r.Count))
		m.add("covn/"+k, float64(r.RSSIN))
	}
	for _, r := range p.Signal {
		m.add(fmt.Sprintf("sig/%d/%s/%d/%d", r.Day, r.Ant, r.DistBin, r.RSSIBin), float64(r.Count))
	}
	for _, r := range p.Elev {
		m.add(fmt.Sprintf("elev/%d/%s/%d", r.Day, r.Ant, r.ElevBin), float64(r.Count))
	}
	for _, r := range p.Heat {
		m.add(fmt.Sprintf("heat/%s/%d", r.Ant, r.Cell), float64(r.Count))
		m.add(fmt.Sprintf("heatn/%s/%d", r.Ant, r.Cell), float64(r.RSSIN))
	}
	for _, r := range p.Range {
		m.add(fmt.Sprintf("range/%d/%s", r.T-r.T%3600, r.Ant), float64(r.Positions))
		m.add(fmt.Sprintf("rangefar/%d/%s", r.T-r.T%3600, r.Ant), float64(r.FarCount))
	}
}

func equalTotals(t *testing.T, label string, got, want incTotals) {
	t.Helper()
	n := 0
	for k, v := range want {
		if got[k] != v {
			if n++; n <= 5 {
				t.Errorf("%s: %s = %v, want %v", label, k, got[k], v)
			}
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok && v != 0 {
			if n++; n <= 5 {
				t.Errorf("%s: extra %s = %v", label, k, v)
			}
		}
	}
	if n > 0 {
		t.Fatalf("%s: %d keys differ", label, n)
	}
}

// lowWear is the cadence of the low-wear profile.
var lowWear = Options{TrailFlush: time.Minute, IncFlush: 5 * time.Minute, HeatFlush: 15 * time.Minute, RangePartial: 30 * time.Minute}

// The database plus PendingIncs is at every moment exactly what the
// database holds after a final flush: the overlay loses or doubles nothing.
func TestPendingIncsOverlayExact(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, cfg := newSim(t, 60, nil)
	o := lowWear
	o.Store, o.Config = st, &cfg
	tr := New(o)
	for _, d := range []time.Duration{90 * time.Second, 4 * time.Minute, 7 * time.Minute} {
		advance(t, s, d, tr)
		if err := st.Flush(); err != nil { // what the tracker submitted is committed
			t.Fatal(err)
		}
		pend := tr.PendingIncs()
		if pend.Empty() {
			t.Fatal("nothing pending")
		}
		got := dbTotals(t, st)
		got.addPending(pend)
		if err := tr.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if p := tr.PendingIncs(); !p.Empty() {
			t.Fatalf("pending after Flush: %d heat", len(p.Heat))
		}
		equalTotals(t, fmt.Sprint("after ", d), got, dbTotals(t, st))
	}
}

// With the low-wear cadence nothing of the day tables reaches the database
// before IncFlush, heat before HeatFlush, but the trail tail and the minute
// rows are written on their own clocks.
func TestCadenceStreams(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, cfg := newSim(t, 40, nil)
	o := lowWear
	o.Store, o.Config = st, &cfg
	tr := New(o)
	count := func(q string) int {
		var n int
		if err := st.DB().QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	advance(t, s, 150*time.Second, tr)
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM coverage`); n != 0 {
		t.Fatalf("coverage written before IncFlush: %d rows", n)
	}
	if n := count(`SELECT COUNT(*) FROM heat`); n != 0 {
		t.Fatalf("heat written before HeatFlush: %d rows", n)
	}
	if n := count(`SELECT COUNT(*) FROM stats_minute`); n == 0 {
		t.Fatal("no minute rows")
	}
	if n := count(`SELECT COUNT(*) FROM trail_tail`) + count(`SELECT COUNT(*) FROM trail_chunk`); n == 0 {
		t.Fatal("no trail points after two trail flushes")
	}
	if n := count(`SELECT COUNT(*) FROM flights`); n == 0 {
		t.Fatal("no flight rows after a minute")
	}
	advance(t, s, 4*time.Minute, tr)
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM coverage`); n == 0 {
		t.Fatal("coverage not written after IncFlush")
	}
	if n := count(`SELECT COUNT(*) FROM heat`); n != 0 {
		t.Fatalf("heat written before HeatFlush: %d rows", n)
	}
}

// A UTC day roll writes the old day's increments out first: whatever the
// flush interval, every increment lands on the day of its position (the
// same totals per day as with a one-second flush).
func TestDayRollIncrements(t *testing.T) {
	midnight := time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)
	run := func(inc time.Duration) incTotals {
		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		s, cfg := newSim(t, 40, func(c *sim.Config) { c.Start = midnight.Add(-3 * time.Minute) })
		tr := New(Options{Store: st, Config: &cfg, IncFlush: inc, HeatFlush: inc})
		advance(t, s, 6*time.Minute, tr)
		if err := tr.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		days := incTotals{}
		rs, err := st.DB().Query(`SELECT day, SUM(count) FROM coverage GROUP BY day`)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			var day int
			var n float64
			if err := rs.Scan(&day, &n); err != nil {
				t.Fatal(err)
			}
			days.add(fmt.Sprint(day), n)
		}
		return days
	}
	slow, fast := run(time.Hour), run(time.Second)
	if len(slow) != 2 {
		t.Fatalf("days: %v", slow)
	}
	equalTotals(t, "per day", slow, fast)
}

// The active flight / aircraft rows are refreshed on one tracker-wide tick:
// an airframe that appeared between two ticks is written once at the next
// row check and afterwards together with all the others, so a table page is
// rewritten once per refresh, not once per airframe phase.
func TestRowRefreshAligned(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, cfg := newSim(t, 80, nil)
	o := lowWear
	o.Store, o.Config = st, &cfg
	tr := New(o)
	advance(t, s, 7*time.Minute+20*time.Second, tr)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tick, acTick := tr.lastRefreshSec, tr.lastAcRefreshSec
	if acTick-tr.startMs/1000 < int64(DefaultAircraftRefresh/time.Second) {
		t.Fatalf("aircraft refresh tick %d s after the start, want >= %v", acTick-tr.startMs/1000, DefaultAircraftRefresh)
	}
	checked, phases := 0, map[int64]int{}
	for _, ac := range tr.ac {
		if ac.seenSavedSec == 0 || ac.firstSeen/1000+30 >= acTick || ac.lastSeen/1000 < tick {
			continue // new since the last tick, or not active across it
		}
		checked++
		phases[ac.seenSavedSec]++
		if ac.flightOpen && ac.flightSavedSec != tick {
			t.Errorf("flight row of %06X saved at %d, last refresh tick %d", ac.key, ac.flightSavedSec, tick)
		}
	}
	if checked < 10 {
		t.Fatalf("only %d aircraft active across the tick", checked)
	}
	if len(phases) != 1 || phases[acTick] != checked {
		t.Fatalf("aircraft rows refreshed at %v, want all %d at the aircraft tick %d", phases, checked, acTick)
	}
}

// The aircraft rows are refreshed every AircraftRefresh only, and
// FlushAircraftRows brings them up to date for a reader: afterwards the
// table holds every message and position the tracker counted.
func TestAircraftRowsOnDemand(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, cfg := newSim(t, 80, nil)
	o := lowWear
	o.Store, o.Config = st, &cfg
	o.AircraftRefresh = 15 * time.Minute
	tr := New(o)
	sums := func() (msgs, pos int64) {
		t.Helper()
		if err := st.DB().QueryRow(`SELECT COALESCE(SUM(msgs), 0), COALESCE(SUM(positions), 0) FROM aircraft`).Scan(&msgs, &pos); err != nil {
			t.Fatal(err)
		}
		return
	}
	counted := func() (msgs, pos int64) {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		for _, ac := range tr.ac {
			if ac.shown() {
				msgs += int64(ac.msgs)
				pos += int64(ac.nPos)
			}
		}
		return
	}
	advance(t, s, 6*time.Minute, tr)
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	m0, p0 := sums()
	cm, cp := counted()
	if m0 >= cm || p0 >= cp {
		t.Fatalf("aircraft rows refreshed before AircraftRefresh: stored %d/%d of %d/%d", m0, p0, cm, cp)
	}
	if m0 == 0 {
		t.Fatal("new airframes not written at the row check")
	}
	if err := tr.FlushAircraftRows(context.Background()); err != nil {
		t.Fatal(err)
	}
	m1, p1 := sums()
	if m1 != cm || p1 != cp {
		t.Fatalf("after FlushAircraftRows: stored %d msgs / %d positions, tracker counted %d / %d", m1, p1, cm, cp)
	}
	// A second reader within the minute costs nothing.
	tr.mu.Lock()
	last := tr.lastAcRefreshSec
	tr.mu.Unlock()
	if err := tr.FlushAircraftRows(context.Background()); err != nil {
		t.Fatal(err)
	}
	tr.mu.Lock()
	again := tr.lastAcRefreshSec
	tr.mu.Unlock()
	if again != last {
		t.Fatalf("second FlushAircraftRows within a minute refreshed again (%d -> %d)", last, again)
	}
}
