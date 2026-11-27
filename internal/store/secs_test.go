package store

import (
	"math"
	"testing"
)

// TestStatsCoveredSecs: the covered seconds of a minute add up when the
// minute is written in parts (capped at 60), unknown (0 / NULL) means a
// full minute, and the hour rollup sums them.
func TestStatsCoveredSecs(t *testing.T) {
	s := openTest(t, manual)
	const h0 = int64(1_700_000_000) / 3600 * 3600
	nan := math.NaN()
	row := func(t0 int64, secs float64) StatsRow {
		return StatsRow{T: t0, Frames: 100, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan, Secs: secs}
	}
	s.AddMinuteStats([]StatsRow{
		row(h0, 20), row(h0, 15), // first minute after a start, flushed twice: 35 s
		row(h0+60, 40), row(h0+60, 40), // capped at 60
		row(h0+120, 0),                  // unknown: a full minute
		row(h0+180, 25), row(h0+180, 0), // unknown wins
	})
	mustFlush(t, s)
	ms, err := s.MinuteStats(bg, "", h0, h0+3599)
	if err != nil || len(ms) != 4 {
		t.Fatalf("minute rows: %v %+v", err, ms)
	}
	for i, want := range []float64{35, 60, 0, 0} {
		if ms[i].Secs != want {
			t.Errorf("minute %d: secs %v, want %v", i, ms[i].Secs, want)
		}
	}
	if got := ms[0].CoveredSec(false); got != 35 {
		t.Errorf("CoveredSec of a partial minute = %v", got)
	}
	if got := ms[2].CoveredSec(false); got != 60 {
		t.Errorf("CoveredSec of an unknown minute = %v", got)
	}
	if _, err := s.RollupHours(bg); err != nil {
		t.Fatal(err)
	}
	hs, err := s.HourStats(bg, "", h0, h0)
	if err != nil || len(hs) != 1 {
		t.Fatalf("hour rows: %v %+v", err, hs)
	}
	if h := hs[0]; h.Minutes != 4 || h.Secs != 35+60+60+60 || h.CoveredSec(true) != 215 {
		t.Errorf("hour: minutes %d secs %v", h.Minutes, h.Secs)
	}
}
