// Package briefing writes the daily briefing: a short plain-language account
// of one station-local day, produced on the station itself from its own
// statistics.
//
// It is deliberately not a language model. Every sentence comes from a
// template filled with measured numbers, and every judgement ("busier than a
// typical Tuesday", "range was normal, so the drop is most likely less
// traffic") is a robust statistical comparison (median and MAD) with the
// station's own recent days. That keeps it cheap enough for a Pi 4, fully
// explainable, and honest: it never states anything the numbers do not show.
//
// Text is returned twice: Paragraphs carry unit tokens ({dist:412.3},
// {int:1234}, {pct:12.3}, {hour:14}, {alt:FT}, {speed:KT}) that the web UI formats in the viewer's
// units; Plain has the same sentences in metric units for other consumers.
package briefing

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"airview/internal/analytics"
	"airview/internal/refdata"
	"airview/internal/store"
)

// Input is everything the briefing is written from.
type Input struct {
	// Day is the day being described (analytics.Daily, local day).
	Day analytics.DayTotals
	// History holds earlier days (any order, the described day excluded or
	// not), normally the 28 days before Day.
	History []analytics.DayTotals
	// Partial is true for the current day (still running): comparisons with
	// whole days are left out.
	Partial bool
	// UTCOffsetSec is the offset of station-local time (for hours).
	UTCOffsetSec int
	// Top lists for the day (may be nil).
	Top *analytics.Top
	// Events of the day (any kinds; emergency / military / interesting /
	// record / feed_down are used).
	Events []store.Event
	// Now is the generation time.
	Now time.Time
}

// Briefing is the result.
type Briefing struct {
	Date    string `json:"date"` // YYYY-MM-DD, station-local
	From    int64  `json:"from"` // Unix ms, local midnight
	To      int64  `json:"to"`   // Unix ms, next local midnight
	Partial bool   `json:"partial"`
	// UTCOffsetSec is the station-local offset used for the day and hours.
	UTCOffsetSec int       `json:"utcOffsetSec"`
	Generated    int64     `json:"generated"` // Unix ms
	Mood         string    `json:"mood"`      // busy | normal | quiet | outage | nodata | partial
	Headline     string    `json:"headline"`  // tokenised like Paragraphs
	Paragraphs   []string  `json:"paragraphs"`
	Plain        []string  `json:"plain"` // Headline first, then Paragraphs, metric units
	Facts        []Fact    `json:"facts"`
	Notable      []Notable `json:"notable"`
	Baseline     Baseline  `json:"baseline"`
	Method       string    `json:"method"`
}

// Fact is one measured figure with its comparison.
type Fact struct {
	Key     string        `json:"key"`   // aircraft | flights | peak | maxKm | newAirframes | uptime
	Label   string        `json:"label"` // e.g. "Aircraft"
	Unit    string        `json:"unit"`  // "" (count) | "km" | "min"
	Value   analytics.Num `json:"value"`
	Typical analytics.Num `json:"typical"` // baseline median, null without a baseline
	Low     analytics.Num `json:"low"`     // typical range (median -/+ 1.5 robust sigma)
	High    analytics.Num `json:"high"`
	DiffPct analytics.Num `json:"diffPct"` // (value - typical) / typical * 100
	Z       analytics.Num `json:"z"`       // robust z-score
	// Level: "" (no baseline), normal, high, low, veryHigh, veryLow.
	Level string `json:"level"`
	// Explained is set when a low value is explained by station downtime.
	Explained bool `json:"explained,omitempty"`
}

// Notable is one event worth naming.
type Notable struct {
	Kind     string `json:"kind"` // emergency | military | interesting | record | feed_down
	T        int64  `json:"t"`    // Unix ms
	ICAO     string `json:"icao,omitempty"`
	Name     string `json:"name"`  // callsign / registration / ICAO
	Text     string `json:"text"`  // one line, with unit tokens like Paragraphs
	Plain    string `json:"plain"` // Text in metric units
	FlightID int64  `json:"flightId,omitempty"`
}

// Baseline says what the day was compared with.
type Baseline struct {
	Days       int    `json:"days"`       // comparable days used
	Candidates int    `json:"candidates"` // earlier days with data
	Weekday    bool   `json:"weekday"`    // only the same weekday
	Text       string `json:"text"`
}

const (
	minFullUptimeMin = 20 * 60 // a comparable day had data at least this long
	minWeekdayDays   = 3       // same-weekday baseline needs this many days
	minAnyDays       = 5       // otherwise all comparable days, at least this many
	outageFrac       = 0.9     // uptime below this share of the day explains low counts
)

// Build writes the briefing.
func Build(in Input) *Briefing {
	d := in.Day
	off := int64(in.UTCOffsetSec) * 1000
	b := &Briefing{
		Date: d.Date, From: d.T, To: d.T + 86400*1000, Partial: in.Partial, UTCOffsetSec: in.UTCOffsetSec,
		Generated: in.Now.UnixMilli(), Paragraphs: []string{}, Facts: []Fact{}, Notable: []Notable{},
		Method: "Written on this station from its own statistics. Each figure is compared with the median of " +
			"comparable earlier days (at least 20 h of data); \"unusual\" means more than two robust standard " +
			"deviations (from the median absolute deviation) away. No data leaves the station and no language model is used.",
	}
	day, _ := time.Parse("2006-01-02", d.Date)
	wd := day.Weekday().String()
	dayName := wd + " " + day.Format("2 January 2006")

	base, bl := baseline(in.History, d, day.Weekday())
	b.Baseline = bl

	uptimeMin := d.UptimeMin
	uptimeFrac := math.Min(1, float64(uptimeMin)/1440)
	// A running day is not judged: the hourly roll-ups behind UptimeMin
	// lag the clock.
	outage := !in.Partial && uptimeMin > 0 && uptimeFrac < outageFrac
	compare := !in.Partial && len(base) > 0

	mk := func(key, label, unit string, v float64, get func(analytics.DayTotals) float64, minScale float64) Fact {
		f := Fact{Key: key, Label: label, Unit: unit, Value: analytics.Num(v),
			Typical: nan(), Low: nan(), High: nan(), DiffPct: nan(), Z: nan()}
		if !compare || !finite(v) {
			return f
		}
		xs := make([]float64, 0, len(base))
		for _, h := range base {
			if x := get(h); finite(x) {
				xs = append(xs, x)
			}
		}
		if len(xs) < 3 {
			return f
		}
		med, sig := robust(xs)
		sig = math.Max(sig, math.Max(minScale, 0.05*math.Abs(med)))
		z := (v - med) / sig
		f.Typical, f.Low, f.High, f.Z = analytics.Num(med), analytics.Num(math.Max(0, med-1.5*sig)), analytics.Num(med+1.5*sig), analytics.Num(z)
		if med > 0 {
			f.DiffPct = analytics.Num((v - med) / med * 100)
		}
		switch {
		case z >= 3:
			f.Level = "veryHigh"
		case z >= 2:
			f.Level = "high"
		case z <= -3:
			f.Level = "veryLow"
		case z <= -2:
			f.Level = "low"
		default:
			f.Level = "normal"
		}
		return f
	}
	maxKm := float64(d.MaxKm)
	fAircraft := mk("aircraft", "Aircraft", "", float64(d.UniqueAircraft), func(x analytics.DayTotals) float64 { return float64(x.UniqueAircraft) }, 3)
	fFlights := mk("flights", "Flights", "", float64(d.Flights), func(x analytics.DayTotals) float64 { return float64(x.Flights) }, 3)
	fPeak := mk("peak", "Most at once", "", float64(d.PeakAircraft), func(x analytics.DayTotals) float64 { return float64(x.PeakAircraft) }, 2)
	fRange := mk("maxKm", "Farthest", "km", maxKm, func(x analytics.DayTotals) float64 { return float64(x.MaxKm) }, 10)
	fNew := mk("newAirframes", "New airframes", "", float64(d.NewAirframes), func(x analytics.DayTotals) float64 { return float64(x.NewAirframes) }, 3)
	fUp := Fact{Key: "uptime", Label: "Data", Unit: "min", Value: analytics.Num(uptimeMin), Typical: nan(), Low: nan(), High: nan(), DiffPct: nan(), Z: nan()}
	if outage {
		fUp.Level = "low"
		for _, f := range []*Fact{&fAircraft, &fFlights, &fPeak, &fNew} {
			if f.Level == "low" || f.Level == "veryLow" {
				f.Explained = true
			}
		}
	} else if !in.Partial && uptimeMin > 0 {
		fUp.Level = "normal"
	}
	b.Facts = append(b.Facts, fAircraft, fFlights, fPeak, fRange, fNew, fUp)

	// ---------------------------------------------------------- headline
	hasData := d.UniqueAircraft > 0 || d.Flights > 0 || d.Frames > 0
	switch {
	case !hasData && in.Partial:
		b.Mood = "nodata"
		b.Headline = "No traffic recorded yet today (" + dayName + ")."
	case !hasData:
		b.Mood = "nodata"
		b.Headline = "No traffic was recorded on " + dayName + "."
	case in.Partial:
		b.Mood = "partial"
		b.Headline = fmt.Sprintf("%s so far: %s aircraft and %s flights.", wd, tInt(d.UniqueAircraft), tInt(d.Flights))
	case outage:
		b.Mood = "outage"
		b.Headline = fmt.Sprintf("A partial %s: the station had data for %s of 24 hours.", wd, durText(uptimeMin))
	case fAircraft.Level == "high" || fAircraft.Level == "veryHigh":
		b.Mood = "busy"
		b.Headline = fmt.Sprintf("Busier than a typical %s: %s aircraft (%s).", wdOrDay(bl.Weekday, wd), tInt(d.UniqueAircraft), signedPct(float64(fAircraft.DiffPct)))
	case fAircraft.Level == "low" || fAircraft.Level == "veryLow":
		b.Mood = "quiet"
		b.Headline = fmt.Sprintf("Quieter than a typical %s: %s aircraft (%s).", wdOrDay(bl.Weekday, wd), tInt(d.UniqueAircraft), signedPct(float64(fAircraft.DiffPct)))
	case fAircraft.Level == "normal":
		b.Mood = "normal"
		b.Headline = fmt.Sprintf("A normal %s: %s aircraft, %s flights.", wd, tInt(d.UniqueAircraft), tInt(d.Flights))
	default:
		b.Mood = "normal"
		b.Headline = fmt.Sprintf("%s: %s aircraft, %s flights.", wd, tInt(d.UniqueAircraft), tInt(d.Flights))
	}

	if hasData {
		// ------------------------------------------------------ traffic
		var p []string
		verb := "logged"
		if in.Partial {
			verb = "has logged"
		}
		p = append(p, fmt.Sprintf("On %s the station %s %s flights by %s different aircraft; at most %s were in view at once.",
			dayName, verb, tInt(d.Flights), tInt(d.UniqueAircraft), tInt(int64(d.PeakAircraft))))
		if s := compareSentence(fAircraft, bl, wd); s != "" {
			p = append(p, s)
		}
		if d.BusiestHour != nil && d.BusiestHour.Aircraft > 0 {
			hr := time.UnixMilli(d.BusiestHour.T + off).UTC().Hour()
			p = append(p, fmt.Sprintf("The busiest hour was %s with %s aircraft.", tHour(hr), tInt(int64(d.BusiestHour.Aircraft))))
		}
		if d.NewAirframes > 0 {
			s := fmt.Sprintf("%s aircraft had never been seen here before", tInt(d.NewAirframes))
			if fNew.Level == "high" || fNew.Level == "veryHigh" {
				s += fmt.Sprintf(", more than usual (typically %s)", tInt(int64(math.Round(float64(fNew.Typical)))))
			}
			p = append(p, s+".")
		}
		b.Paragraphs = append(b.Paragraphs, strings.Join(p, " "))

		// ------------------------------------------------------ range and health
		p = p[:0]
		if finite(maxKm) && maxKm > 0 {
			s := fmt.Sprintf("The farthest position was %s", tDist(maxKm))
			if f := d.Farthest; f != nil {
				name := f.Callsign
				if name == "" {
					name = f.ICAO
				}
				if name != "" {
					s += " (" + name + ")"
				}
			}
			switch fRange.Level {
			case "normal":
				s += fmt.Sprintf(", within the usual range (typically %s)", tDist(float64(fRange.Typical)))
			case "high", "veryHigh":
				s += fmt.Sprintf(", well beyond the usual %s", tDist(float64(fRange.Typical)))
			case "low", "veryLow":
				s += fmt.Sprintf(", short of the usual %s", tDist(float64(fRange.Typical)))
			}
			p = append(p, s+".")
		}
		if s := diagnose(fAircraft, fRange, outage, uptimeMin, in.Partial); s != "" {
			p = append(p, s)
		}
		if len(p) > 0 {
			b.Paragraphs = append(b.Paragraphs, strings.Join(p, " "))
		}

		// ------------------------------------------------------ who
		if s := topSentence(in.Top); s != "" {
			b.Paragraphs = append(b.Paragraphs, s)
		}
	}

	// ---------------------------------------------------------- events
	b.Notable = notables(in.Events, d.T, b.To)
	if s := eventSentence(b.Notable); s != "" {
		b.Paragraphs = append(b.Paragraphs, s)
	}
	if !in.Partial && bl.Days == 0 && hasData {
		b.Paragraphs = append(b.Paragraphs, bl.Text)
	}

	b.Plain = make([]string, 0, 1+len(b.Paragraphs))
	b.Plain = append(b.Plain, Plain(b.Headline))
	for _, s := range b.Paragraphs {
		b.Plain = append(b.Plain, Plain(s))
	}
	return b
}

// baseline picks the comparable earlier days: at least 20 h of data, before
// the described day; the same weekday when there are enough of them.
func baseline(hist []analytics.DayTotals, d analytics.DayTotals, wd time.Weekday) ([]analytics.DayTotals, Baseline) {
	var all, same []analytics.DayTotals
	cand := 0
	for _, h := range hist {
		if h.T >= d.T || !(h.UniqueAircraft > 0 || h.Frames > 0) {
			continue
		}
		cand++
		if h.UptimeMin < minFullUptimeMin {
			continue
		}
		all = append(all, h)
		if t, err := time.Parse("2006-01-02", h.Date); err == nil && t.Weekday() == wd {
			same = append(same, h)
		}
	}
	bl := Baseline{Candidates: cand}
	switch {
	case len(same) >= minWeekdayDays:
		bl.Days, bl.Weekday = len(same), true
		bl.Text = fmt.Sprintf("Compared with the last %d %ss with a full day of data.", len(same), wd)
		return same, bl
	case len(all) >= minAnyDays:
		bl.Days = len(all)
		bl.Text = fmt.Sprintf("Compared with the last %d days with a full day of data.", len(all))
		return all, bl
	}
	bl.Text = fmt.Sprintf("No comparison yet: the station has %d earlier days with a full day of data and needs %d.", len(all), minAnyDays)
	return nil, bl
}

// robust returns the median and the MAD scaled to a normal sigma.
func robust(xs []float64) (med, sigma float64) {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	med = quant(s)
	dev := make([]float64, len(s))
	for i, x := range s {
		dev[i] = math.Abs(x - med)
	}
	sort.Float64s(dev)
	return med, 1.4826 * quant(dev)
}

func quant(s []float64) float64 {
	n := len(s)
	if n == 0 {
		return math.NaN()
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func compareSentence(f Fact, bl Baseline, wd string) string {
	if f.Level == "" || !finite(float64(f.Typical)) {
		return ""
	}
	what := "a typical day"
	if bl.Weekday {
		what = "the typical " + wd
	}
	typ := tInt(int64(math.Round(float64(f.Typical))))
	switch f.Level {
	case "normal":
		return fmt.Sprintf("That is in line with %s (%s aircraft).", what, typ)
	case "high":
		return fmt.Sprintf("That is %s more than %s (%s aircraft).", tPct(float64(f.DiffPct)), what, typ)
	case "veryHigh":
		return fmt.Sprintf("That is %s more than %s (%s aircraft), an unusually busy day.", tPct(float64(f.DiffPct)), what, typ)
	case "low", "veryLow":
		s := fmt.Sprintf("That is %s fewer than %s (%s aircraft)", tPct(-float64(f.DiffPct)), what, typ)
		if f.Explained {
			s += ", explained by the time the station was without data"
		}
		return s + "."
	}
	return ""
}

// diagnose reasons about the cause of an unusual day from traffic, range and
// uptime together.
func diagnose(ac, rg Fact, outage bool, uptimeMin int, partial bool) string {
	low := func(f Fact) bool { return f.Level == "low" || f.Level == "veryLow" }
	high := func(f Fact) bool { return f.Level == "high" || f.Level == "veryHigh" }
	switch {
	case partial:
		return ""
	case outage:
		return fmt.Sprintf("The station had data for %s of the day, so all totals are lower than for a full day. Check power, the network and the receiver feed for the missing hours.", durText(uptimeMin))
	case low(ac) && low(rg):
		return "Traffic and range were both well below normal while the station was online all day, which is what a receiver problem looks like: check the antenna, cable, connectors and gain."
	case low(ac):
		return "Range was normal, so the drop is most likely less traffic (a holiday, weather or airspace changes) rather than a receiver problem."
	case rg.Level == "veryLow" && !low(ac):
		return "Range was unusually short while traffic was normal: a sign of an antenna or cable fault, new local interference or an obstruction."
	case rg.Level == "veryHigh":
		return "Positions this far out are unusual for this station; enhanced radio propagation (tropospheric ducting) is a common cause."
	case high(ac) && !high(rg):
		return ""
	}
	return ""
}

func topSentence(t *analytics.Top) string {
	if t == nil {
		return ""
	}
	var parts []string
	if len(t.Operators) > 0 && t.Operators[0].Key != "" {
		o := t.Operators[0]
		s := "The most frequent operator was " + o.Key
		if finite(float64(o.Pct)) && o.Pct > 0 {
			s += fmt.Sprintf(" (%s of flights)", tPct(float64(o.Pct)))
		}
		parts = append(parts, s)
	}
	if len(t.Types) > 0 && t.Types[0].Key != "" {
		ty := t.Types[0]
		s := "the most common type " + ty.Key
		if ty.Label != "" {
			s += " (" + ty.Label + ")"
		}
		parts = append(parts, s)
	}
	if len(t.Countries) > 0 && t.Countries[0].Key != "" {
		parts = append(parts, "and the most common country of registration "+t.Countries[0].Key)
	}
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, ", ")
	if len(parts) == 1 {
		s = strings.TrimPrefix(s, "and ")
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

type evDetail struct {
	Callsign  string   `json:"callsign"`
	Reg       string   `json:"reg"`
	Type      string   `json:"type"`
	Operator  string   `json:"operator"`
	Emergency string   `json:"emergency"`
	Squawk    string   `json:"squawk"`
	FlightID  int64    `json:"flightId"`
	Key       string   `json:"key"`
	Value     *float64 `json:"value"`
	Previous  *float64 `json:"previous"`
}

var recordText = map[string]struct{ label, unit string }{
	"max_dist_km":       {"farthest position", "km"},
	"max_alt_ft":        {"highest aircraft", "ft"},
	"max_gs_kt":         {"fastest aircraft", "kt"},
	"min_rssi_pos_dbfs": {"weakest decoded position", "dBFS"},
	"max_aircraft":      {"most aircraft at once", ""},
	"max_msg_rate":      {"peak message rate", "msg/s"},
	"max_flight_s":      {"longest tracked flight", "s"},
}

// notables keeps the day's events worth naming: every emergency, the first
// military / interesting catch per aircraft, the last record per key and
// feed outages.
func notables(evs []store.Event, fromMs, toMs int64) []Notable {
	out := []Notable{}
	seen := map[string]bool{}
	sorted := append([]store.Event(nil), evs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].T < sorted[j].T })
	recIdx := map[string]int{}
	for _, e := range sorted {
		t := e.T * 1000
		if t < fromMs || t >= toMs {
			continue
		}
		var det evDetail
		if e.Detail != "" {
			_ = json.Unmarshal([]byte(e.Detail), &det)
		}
		icao := ""
		if e.ICAO != 0 {
			icao = store.HexICAO(e.ICAO)
		}
		name := strings.TrimSpace(det.Callsign)
		if name == "" {
			name = det.Reg
		}
		if name == "" {
			name = icao
		}
		n := Notable{Kind: e.Kind, T: t, ICAO: icao, Name: name, FlightID: det.FlightID}
		desc := strings.TrimSpace(strings.Join(nonEmpty(det.Type, det.Operator), ", "))
		switch e.Kind {
		case "emergency":
			lbl := strings.ToLower(refdata.EmergencyLabel(det.Emergency))
			if lbl == "" || det.Emergency == "" {
				lbl = "emergency"
			}
			n.Text = name + ": " + lbl
			if det.Squawk != "" {
				n.Text += ", squawk " + det.Squawk
			}
		case "military", "interesting":
			k := e.Kind + "/" + icao
			if seen[k] || icao == "" {
				continue
			}
			seen[k] = true
			n.Text = name
			if desc != "" {
				n.Text += " (" + desc + ")"
			}
		case "record":
			r, ok := recordText[det.Key]
			if !ok || det.Value == nil {
				continue
			}
			n.Text = "New record, " + r.label + ": " + valueToken(*det.Value, r.unit)
			if name != "" {
				n.Text += " (" + name + ")"
			}
			if i, ok := recIdx[det.Key]; ok { // keep the day's final value only
				out[i] = n
				continue
			}
			recIdx[det.Key] = len(out)
		case "feed_down":
			n.Name, n.ICAO = "", ""
			n.Text = "A receiver feed went down"
		default:
			continue
		}
		out = append(out, n)
	}
	for i := range out {
		out[i].Plain = Plain(out[i].Text)
	}
	return out
}

// valueToken writes a record value: a unit token for the units the viewer
// can choose (distance, altitude, speed), else the number and its unit.
func valueToken(v float64, unit string) string {
	switch unit {
	case "km":
		return tDist(v)
	case "ft":
		return "{alt:" + strconv.FormatFloat(math.Round(v), 'f', 0, 64) + "}"
	case "kt":
		return "{speed:" + strconv.FormatFloat(math.Round(v), 'f', 0, 64) + "}"
	case "s":
		return durText(int(math.Round(v / 60)))
	case "":
		return groupInt(int64(math.Round(v)))
	}
	return strconv.FormatFloat(v, 'f', -1, 64) + " " + unit
}

func eventSentence(ns []Notable) string {
	cnt := map[string][]string{}
	for _, n := range ns {
		cnt[n.Kind] = append(cnt[n.Kind], n.Name)
	}
	list := func(names []string) string {
		const maxNames = 3
		shown := names
		if len(shown) > maxNames {
			shown = shown[:maxNames]
		}
		s := strings.Join(shown, ", ")
		if len(names) > maxNames {
			s += fmt.Sprintf(" and %d more", len(names)-maxNames)
		}
		return s
	}
	var p []string
	if v := cnt["emergency"]; len(v) > 0 {
		p = append(p, fmt.Sprintf("%s declared: %s.", plural(len(v), "emergency was", "emergencies were"), list(v)))
	}
	if v := cnt["military"]; len(v) > 0 {
		p = append(p, fmt.Sprintf("%s seen: %s.", plural(len(v), "military aircraft was", "military aircraft were"), list(v)))
	}
	if v := cnt["interesting"]; len(v) > 0 {
		p = append(p, fmt.Sprintf("%s on the watch list: %s.", plural(len(v), "aircraft was", "aircraft were"), list(v)))
	}
	if n := len(cnt["record"]); n > 0 {
		if n == 1 {
			p = append(p, "The station set a new all-time record.")
		} else {
			p = append(p, fmt.Sprintf("The station set %s new all-time records.", tInt(int64(n))))
		}
	}
	if n := len(cnt["feed_down"]); n > 0 {
		if n == 1 {
			p = append(p, "A receiver feed went down once.")
		} else {
			p = append(p, fmt.Sprintf("Receiver feeds went down %s times.", tInt(int64(n))))
		}
	}
	return strings.Join(p, " ")
}

// ------------------------------------------------------------------ text helpers

// Tokens: {int:N} {dist:KM} {pct:P} {hour:H} {alt:FT} {speed:KT}. Plain replaces them in metric
// units; the web UI formats them in the viewer's units.
func tInt(n int64) string { return "{int:" + strconv.FormatInt(n, 10) + "}" }
func tDist(km float64) string {
	return "{dist:" + strconv.FormatFloat(math.Round(km*10)/10, 'f', -1, 64) + "}"
}
func tPct(p float64) string {
	return "{pct:" + strconv.FormatFloat(math.Round(math.Abs(p)), 'f', 0, 64) + "}"
}
func tHour(h int) string { return "{hour:" + strconv.Itoa(h) + "}" }

func signedPct(p float64) string {
	if !finite(p) {
		return "no comparison"
	}
	if p >= 0 {
		return "+" + tPct(p)
	}
	return "-" + tPct(p)
}

var tokenRE = regexp.MustCompile(`\{(int|dist|pct|hour|alt|speed):(-?[0-9.]+)\}`)

// Plain replaces the unit tokens of a briefing sentence with metric text.
func Plain(s string) string {
	return tokenRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := tokenRE.FindStringSubmatch(m)
		v, err := strconv.ParseFloat(sub[2], 64)
		if err != nil {
			return sub[2]
		}
		switch sub[1] {
		case "int":
			return groupInt(int64(v))
		case "dist":
			return strconv.FormatFloat(math.Round(v), 'f', 0, 64) + " km"
		case "pct":
			return strconv.FormatFloat(v, 'f', 0, 64) + " %"
		case "hour":
			return fmt.Sprintf("%02d:00", int(v))
		case "alt":
			return groupInt(int64(v)) + " ft"
		case "speed":
			return strconv.FormatFloat(v, 'f', 0, 64) + " kt"
		}
		return sub[2]
	})
}

func groupInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func durText(min int) string {
	h, m := min/60, min%60
	switch {
	case h == 0:
		return fmt.Sprintf("%d min", m)
	case m == 0:
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "One " + one
	}
	return tInt(int64(n)) + " " + many
}

func wdOrDay(weekday bool, wd string) string {
	if weekday {
		return wd
	}
	return "day"
}

func nonEmpty(ss ...string) []string {
	out := ss[:0:0]
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func nan() analytics.Num    { return analytics.Num(math.NaN()) }
func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
