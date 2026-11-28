package briefing

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"airview/internal/analytics"
	"airview/internal/store"
)

// day builds a DayTotals for date (UTC midnight, offset 0).
func day(date string, aircraft, flights int64, maxKm float64, uptime int) analytics.DayTotals {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		panic(err)
	}
	return analytics.DayTotals{
		T: t.UnixMilli(), Date: date, Frames: aircraft * 1000, Positions: aircraft * 300,
		PeakAircraft: int(aircraft / 10), UniqueAircraft: aircraft, Flights: flights,
		NewAirframes: aircraft / 50, MaxKm: analytics.Num(maxKm), UptimeMin: uptime,
	}
}

// history makes n days before date with a small deterministic spread.
func history(date string, n int, aircraft int64, maxKm float64) []analytics.DayTotals {
	t, _ := time.Parse("2006-01-02", date)
	out := make([]analytics.DayTotals, 0, n)
	for i := 1; i <= n; i++ {
		d := t.AddDate(0, 0, -i)
		jit := int64((i*37)%11) - 5 // -5..5
		out = append(out, day(d.Format("2006-01-02"), aircraft+jit*aircraft/100, aircraft+jit*aircraft/100+aircraft/5, maxKm+float64(jit)*3, 1440))
	}
	return out
}

func fact(t *testing.T, b *Briefing, key string) Fact {
	t.Helper()
	for _, f := range b.Facts {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("no fact %q", key)
	return Fact{}
}

func all(b *Briefing) string { return strings.Join(b.Plain, "\n") }

var now = time.Date(2026, 11, 27, 8, 0, 0, 0, time.UTC)

func TestNormalDayWeekdayBaseline(t *testing.T) {
	// 2026-11-24 is a Tuesday; 28 days of history give 4 Tuesdays.
	b := Build(Input{Day: day("2026-11-24", 1000, 1200, 400, 1440), History: history("2026-11-24", 28, 1000, 400), Now: now})
	if b.Mood != "normal" {
		t.Fatalf("mood %q, want normal; %s", b.Mood, all(b))
	}
	if !b.Baseline.Weekday || b.Baseline.Days != 4 {
		t.Fatalf("baseline %+v, want 4 Tuesdays", b.Baseline)
	}
	f := fact(t, b, "aircraft")
	if f.Level != "normal" || math.Abs(float64(f.Typical)-1000) > 60 {
		t.Fatalf("aircraft fact %+v", f)
	}
	txt := all(b)
	for _, want := range []string{"A normal Tuesday: 1,000 aircraft", "Tuesday 24 November 2026", "in line with the typical Tuesday", "within the usual range"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "{") {
		t.Errorf("plain text still has tokens:\n%s", txt)
	}
	// tokenised paragraphs carry unit tokens for the UI
	if !strings.Contains(b.Headline, "{int:1000}") {
		t.Errorf("headline %q has no int token", b.Headline)
	}
}

func TestQuietDayRangeNormalBlamesTraffic(t *testing.T) {
	b := Build(Input{Day: day("2026-11-24", 600, 700, 400, 1440), History: history("2026-11-24", 28, 1000, 400), Now: now})
	if b.Mood != "quiet" {
		t.Fatalf("mood %q; %s", b.Mood, all(b))
	}
	if !strings.Contains(all(b), "most likely less traffic") {
		t.Errorf("no traffic diagnosis:\n%s", all(b))
	}
	if f := fact(t, b, "aircraft"); f.Level != "veryLow" || float64(f.DiffPct) > -35 {
		t.Errorf("aircraft fact %+v", f)
	}
}

func TestQuietDayShortRangeBlamesReceiver(t *testing.T) {
	b := Build(Input{Day: day("2026-11-24", 500, 600, 150, 1440), History: history("2026-11-24", 28, 1000, 400), Now: now})
	if !strings.Contains(all(b), "receiver problem looks like") {
		t.Errorf("no receiver diagnosis:\n%s", all(b))
	}
}

func TestShortRangeNormalTraffic(t *testing.T) {
	b := Build(Input{Day: day("2026-11-24", 1000, 1200, 200, 1440), History: history("2026-11-24", 28, 1000, 400), Now: now})
	if !strings.Contains(all(b), "Range was unusually short while traffic was normal") {
		t.Errorf("no range diagnosis:\n%s", all(b))
	}
}

func TestDuctingHint(t *testing.T) {
	b := Build(Input{Day: day("2026-11-24", 1000, 1200, 700, 1440), History: history("2026-11-24", 28, 1000, 400), Now: now})
	if fact(t, b, "maxKm").Level != "veryHigh" || !strings.Contains(all(b), "tropospheric ducting") {
		t.Errorf("no ducting hint:\n%s", all(b))
	}
}

func TestBusyDay(t *testing.T) {
	b := Build(Input{Day: day("2026-11-24", 1400, 1600, 400, 1440), History: history("2026-11-24", 28, 1000, 400), Now: now})
	if b.Mood != "busy" || !strings.Contains(b.Plain[0], "Busier than a typical Tuesday") || !strings.Contains(b.Plain[0], "+") {
		t.Fatalf("mood %q headline %q", b.Mood, b.Plain[0])
	}
}

func TestOutageExplainsLowCounts(t *testing.T) {
	b := Build(Input{Day: day("2026-11-24", 500, 600, 390, 700), History: history("2026-11-24", 28, 1000, 400), Now: now})
	if b.Mood != "outage" {
		t.Fatalf("mood %q", b.Mood)
	}
	if f := fact(t, b, "aircraft"); !f.Explained {
		t.Errorf("low aircraft not marked explained: %+v", f)
	}
	txt := all(b)
	if !strings.Contains(txt, "11 h 40 min") || strings.Contains(txt, "receiver problem") {
		t.Errorf("outage text wrong:\n%s", txt)
	}
}

func TestNoBaselineYet(t *testing.T) {
	b := Build(Input{Day: day("2026-11-24", 1000, 1200, 400, 1440), History: history("2026-11-24", 2, 1000, 400), Now: now})
	if b.Baseline.Days != 0 || fact(t, b, "aircraft").Level != "" {
		t.Fatalf("baseline %+v", b.Baseline)
	}
	if !strings.Contains(all(b), "No comparison yet") {
		t.Errorf("missing no-baseline note:\n%s", all(b))
	}
	// facts stay JSON-safe without a baseline (NaN -> null)
	if _, err := json.Marshal(b); err != nil {
		t.Fatal(err)
	}
}

func TestAllDaysBaselineWithoutWeekdays(t *testing.T) {
	// 6 days of history: only 0 Tuesdays, so all days are used.
	b := Build(Input{Day: day("2026-11-24", 1000, 1200, 400, 1440), History: history("2026-11-24", 6, 1000, 400), Now: now})
	if b.Baseline.Weekday || b.Baseline.Days != 6 {
		t.Fatalf("baseline %+v", b.Baseline)
	}
	if !strings.Contains(all(b), "a typical day") {
		t.Errorf("text:\n%s", all(b))
	}
}

func TestShortHistoryDaysAreNotComparable(t *testing.T) {
	h := history("2026-11-24", 28, 1000, 400)
	for i := range h {
		h[i].UptimeMin = 600
	}
	b := Build(Input{Day: day("2026-11-24", 1000, 1200, 400, 1440), History: h, Now: now})
	if b.Baseline.Days != 0 || b.Baseline.Candidates != 28 {
		t.Fatalf("baseline %+v", b.Baseline)
	}
}

func TestPartialDay(t *testing.T) {
	b := Build(Input{Day: day("2026-11-24", 300, 320, 380, 470), History: history("2026-11-24", 28, 1000, 400), Partial: true, Now: now})
	if b.Mood != "partial" || fact(t, b, "aircraft").Level != "" || fact(t, b, "uptime").Level != "" {
		t.Fatalf("partial: mood %q fact %+v", b.Mood, fact(t, b, "aircraft"))
	}
	txt := all(b)
	if !strings.Contains(txt, "Tuesday so far") || !strings.Contains(txt, "has logged") || strings.Contains(txt, "Quieter") {
		t.Errorf("partial text:\n%s", txt)
	}
}

func TestNoData(t *testing.T) {
	b := Build(Input{Day: analytics.DayTotals{T: 0, Date: "2026-11-24", MaxKm: analytics.Num(math.NaN())}, Now: now})
	if b.Mood != "nodata" || len(b.Paragraphs) != 0 {
		t.Fatalf("nodata: %+v", b)
	}
	if _, err := json.Marshal(b); err != nil {
		t.Fatal(err)
	}
}

func TestEventsAndTop(t *testing.T) {
	d := day("2026-11-24", 1000, 1200, 400, 1440)
	at := func(h int) int64 { return d.T/1000 + int64(h)*3600 }
	ev := []store.Event{
		{T: at(9), Kind: "emergency", ICAO: 0x4CA123, Detail: `{"callsign":"RYR12AB","emergency":"general","squawk":"7700","flightId":42}`},
		{T: at(10), Kind: "military", ICAO: 0x43C001, Detail: `{"callsign":"RRR1234","type":"A400"}`},
		{T: at(11), Kind: "military", ICAO: 0x43C001, Detail: `{"callsign":"RRR1234"}`}, // same airframe again
		{T: at(12), Kind: "record", ICAO: 0x400001, Detail: `{"callsign":"BAW1","key":"max_dist_km","value":450.5}`},
		{T: at(13), Kind: "record", ICAO: 0x400002, Detail: `{"callsign":"BAW2","key":"max_dist_km","value":460.25}`},
		{T: at(14), Kind: "feed_down"},
		{T: at(30), Kind: "emergency", ICAO: 1, Detail: `{"emergency":"general"}`}, // next day
		{T: at(15), Kind: "feed_up"},
	}
	top := &analytics.Top{
		Operators: []analytics.TopEntry{{Key: "Ryanair", Pct: 12.4}},
		Types:     []analytics.TopEntry{{Key: "B738", Label: "BOEING 737-800"}},
		Countries: []analytics.TopEntry{{Key: "Ireland"}, {Key: "United Kingdom"}},
	}
	b := Build(Input{Day: d, History: history("2026-11-24", 28, 1000, 400), Events: ev, Top: top, Now: now})
	if len(b.Notable) != 4 {
		t.Fatalf("notable %d, want 4 (emergency, military once, last record, feed down): %+v", len(b.Notable), b.Notable)
	}
	if b.Notable[0].FlightID != 42 || !strings.Contains(b.Notable[0].Text, "general emergency, squawk 7700") {
		t.Errorf("emergency notable %+v", b.Notable[0])
	}
	if b.Notable[2].Text != "New record, farthest position: {dist:460.3} (BAW2)" || b.Notable[2].Plain != "New record, farthest position: 460 km (BAW2)" {
		t.Errorf("record notable %+v", b.Notable[2])
	}
	txt := all(b)
	for _, want := range []string{"One emergency was declared: RYR12AB.", "One military aircraft was seen: RRR1234.",
		"a new all-time record", "went down once", "Ryanair (12 % of flights)", "B738 (BOEING 737-800)"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
}

func TestPlain(t *testing.T) {
	for in, want := range map[string]string{
		"{int:1234567} and {int:-1234}": "1,234,567 and -1,234",
		"{dist:412.6}":                  "413 km",
		"{pct:12}":                      "12 %",
		"{hour:7}":                      "07:00",
		"{alt:45000} {speed:612}":       "45,000 ft 612 kt",
		"no tokens {x:1}":               "no tokens {x:1}",
	} {
		if got := Plain(in); got != want {
			t.Errorf("Plain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRobust(t *testing.T) {
	med, sig := robust([]float64{1, 2, 3, 4, 100})
	if med != 3 || math.Abs(sig-1.4826) > 1e-9 {
		t.Fatalf("robust = %v, %v", med, sig)
	}
}

func BenchmarkBuild(b *testing.B) {
	d := day("2026-11-24", 1000, 1200, 400, 1440)
	h := history("2026-11-24", 28, 1000, 400)
	for b.Loop() {
		Build(Input{Day: d, History: h, Now: now})
	}
}
