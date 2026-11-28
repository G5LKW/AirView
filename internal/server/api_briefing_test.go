package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"airview/internal/briefing"
)

// TestBriefingDaily: the daily briefing for today, yesterday and a date, and
// the parameter checks.
func TestBriefingDaily(t *testing.T) {
	r := newRig(t, rigOptions{seedHours: 3, aircraft: 8})
	for _, q := range []string{"?date=today", "", "?date=yesterday", "?date=2020-01-01"} {
		rec := r.get("/api/briefing/daily" + q)
		must(t, rec, 200, "briefing "+q)
		var b briefing.Briefing
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
			t.Fatalf("%s: %v: %s", q, err, rec.Body)
		}
		if b.Date == "" || b.To-b.From != 86400*1000 || b.Mood == "" || len(b.Plain) == 0 || len(b.Facts) != 6 {
			t.Fatalf("%s: incomplete briefing: %s", q, rec.Body)
		}
		if b.Partial != (q == "?date=today") {
			t.Fatalf("%s: partial = %v", q, b.Partial)
		}
		if q == "?date=2020-01-01" && (b.Date != "2020-01-01" || b.Mood != "nodata") {
			t.Fatalf("old date: %s", rec.Body)
		}
		if strings.Contains(strings.Join(b.Plain, " "), "{") {
			t.Fatalf("%s: tokens in plain text: %v", q, b.Plain)
		}
	}
	future := time.Now().AddDate(0, 0, 3).Format("2006-01-02")
	for _, q := range []string{"?date=nonsense", "?date=" + future} {
		must(t, r.get("/api/briefing/daily"+q), 400, "briefing "+q)
	}
}
