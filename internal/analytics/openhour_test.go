package analytics

import (
	"context"
	"testing"

	"airview/internal/model"
	"airview/internal/store"
)

// UX-02: the hourly rollup also writes the open hour; minutes received
// after that must still count in the hour totals ("today", "24 h").
func TestHourStatsOpenHour(t *testing.T) {
	a, st := newTest(t, model.DefaultConfig())
	ctx := context.Background()
	now := testNow.Unix()
	open := now / 3600 * 3600
	row := func(ts int64) store.StatsRow { return store.StatsRow{T: ts, Frames: 60, Aircraft: 1} }
	var first []store.StatsRow
	for ts := open - 3600; ts < open+600; ts += 60 { // previous hour + 10 min of the open hour
		first = append(first, row(ts))
	}
	apply(t, st, func(b *store.Batch) { b.AddMinuteStats(first) })
	if _, err := st.RollupHours(ctx); err != nil {
		t.Fatal(err)
	}
	var later []store.StatsRow
	for ts := open + 600; ts < open+1200; ts += 60 { // 10 more minutes, not rolled up
		later = append(later, row(ts))
	}
	apply(t, st, func(b *store.Batch) { b.AddMinuteStats(later) })
	rows, err := a.hourStats(ctx, "", open-3600, open+3600)
	if err != nil {
		t.Fatal(err)
	}
	var frames int64
	for _, r := range rows {
		frames += r.Frames
	}
	if want := int64(60 * (60 + 20)); frames != want {
		t.Fatalf("frames = %d, want %d (rows %+v)", frames, want, rows)
	}
}
