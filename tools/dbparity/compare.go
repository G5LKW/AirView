package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Comparison modes.
const (
	// modeStrict: both instances hold the same database and receive no
	// input (migration / historic parity). Everything must be equal except
	// the fields that depend on the wall clock or the process (tolerances
	// table, Strict = true rules).
	modeStrict = "strict"
	// modeLive: both instances received the same Beast bytes in real time.
	// Frames that arrive at a minute boundary can be counted into
	// neighbouring minutes, an aircraft can time out in one instance a
	// moment earlier, the two snapshots are taken a few ms apart, so
	// numbers get a relative tolerance and lists a length tolerance (see
	// liveNumTol / liveLenTol). Structure (keys, types, non-empty lists,
	// which records exist) must still match.
	modeLive = "live"
)

const (
	liveNumRel = 0.10 // live: relative tolerance of every number
	liveNumAbs = 3.0  // live: absolute tolerance of every number (small counts)
	liveLenRel = 0.10 // live: lists may differ by 10 % of their length...
	liveLenAbs = 3    // ...or 3 entries, whichever is larger
)

// tolerance is one documented exception to exact equality. Route and Path
// are globs ("*" = any characters) on the route key ("GET /api/status",
// variants included) and on the JSON path ("aircraft[].posAge"; "[]" for
// array elements). Exactly one of Ignore / Abs / Rel / OldNull applies.
type tolerance struct {
	Route  string
	Path   string
	Ignore bool
	Abs    float64 // |a-b| <= Abs (numbers)
	Rel    float64 // |a-b| <= Rel * max(|a|,|b|)
	Live   bool    // only in live mode
	// OldNull accepts any new value where the old build answered null (no
	// data yet); values present on both sides are still compared.
	OldNull bool
	Why     string

	rr, pr *regexp.Regexp
}

// tolerances is the complete list of fields that are not compared exactly.
// Every entry says why the field cannot be equal between two correct
// builds. Keep it short: a field added here is a field the harness no
// longer checks.
var tolerances = []tolerance{
	// Process / host facts.
	{Route: "*", Path: "*uptime*", Ignore: true, Why: "process uptime"},
	{Route: "*", Path: "*Uptime*", Ignore: true, Why: "process uptime"},
	{Route: "GET /api/system*", Path: "*", Ignore: true, Why: "process facts: PID, memory, goroutines, GC, file sizes, paths, build info"},
	// db-stats-shape: /api/db/stats keeps its 1.0.0 members; the table
	// counts of the 1.0.0 tables are compared, everything about the files
	// and the new storage machinery is not.
	{Route: "GET /api/db/stats*", Path: "stats*", Ignore: true, Why: "db-stats-shape: database file sizes, page counts, WAL and checkpoint state: exactly what the efficiency work changes"},
	{Route: "GET /api/db/stats*", Path: "profile*", Ignore: true, Why: "db-stats-shape: the storage profile object is new (1.1)"},
	{Route: "GET /api/db/stats*", Path: "migration*", Ignore: true, Why: "db-stats-shape: the data-migration object is new (1.1)"},
	{Route: "GET /api/db/stats*", Path: "tileCache*", Ignore: true, Why: "tile cache files of each process"},
	{Route: "GET /api/db/stats*", Path: "tables.kv", Ignore: true, Why: "db-stats-shape: maintenance watermarks (kv) differ by design"},
	{Route: "GET /api/db/stats*", Path: "tables.schema_*", Ignore: true, Why: "db-stats-shape: migration bookkeeping (checked by checkMigrations)"},
	{Route: "GET /api/db/stats*", Path: "tables.trail_*", Ignore: true, Why: "db-stats-shape: new physical tables behind the logical positions table (positions keeps its name and count)"},
	{Route: "GET /api/db/stats*", Path: "tables.met_sample", Ignore: true, Why: "db-stats-shape: new physical table behind the logical met_obs table"},
	{Route: "GET /api/db/stats*", Path: "tables.hist_day", Ignore: true, Why: "db-stats-shape: sealed day histograms behind the logical coverage / signal_hist / elev_hist tables"},
	{Route: "GET /api/db/stats*", Path: "tables.flights_hour", Ignore: true, Why: "db-stats-shape: new rollup table"},
	{Route: "GET /api/db/stats*", Path: "tables.receiver_status", Ignore: true, Why: "rows written by each running instance (one per feed and minute since its start)"},
	{Route: "GET /api/db/stats*", Path: "tables.stats_minute", Ignore: true, Why: "rows written by each running instance (idle minutes since its start)"},
	{Route: "GET /api/db/stats*", Path: "tables.met_profile", Ignore: true, Live: true, Why: "visible-after: the met collector merges a profile hour into met_profile when the hour has closed (one UPSERT per level instead of one every 15 s); the open hour is served from memory by the met pages and written before an export, but /api/db/stats counts only the rows in the file"},
	{Route: "GET /api/db/stats*", Path: "tables.met_bias", Ignore: true, Live: true, Why: "the met collector saves its heading corrections every 5 min after its own start; the live comparison falls on that moment, so one process may have saved them and the other not yet (the corrections themselves are served from memory)"},
	{Route: "*", Path: "*dbBytes*", Ignore: true, Why: "db-stats-shape: database file size (what the efficiency work changes; also differs with checkpoint timing)"},
	{Route: "*", Path: "*walBytes*", Ignore: true, Why: "db-stats-shape: WAL file size (checkpoint timing)"},
	{Route: "*", Path: "*started*", Ignore: true, Why: "process start time"},
	{Route: "*", Path: "*Started*", Ignore: true, Why: "process start time"},
	{Route: "*", Path: "*dataDir*", Ignore: true, Why: "each instance has its own scratch data directory"},
	{Route: "*", Path: "*port", Ignore: true, Why: "each instance has its own HTTP / feed ports"},
	{Route: "*", Path: "*Port", Ignore: true, Why: "each instance has its own HTTP / feed ports"},
	{Route: "*", Path: "*url", Ignore: true, Why: "contains the instance's port"},
	{Route: "*", Path: "*URL", Ignore: true, Why: "contains the instance's port"},
	{Route: "*", Path: "feeds[].host*", Ignore: true, Why: "feed endpoint (port differs)"},
	{Route: "*", Path: "*addr", Ignore: true, Why: "feed endpoint (port differs)"},
	{Route: "*", Path: "*version*", Ignore: true, Why: "the new build may carry a newer version string"},
	{Route: "*", Path: "*Version*", Ignore: true, Why: "the new build may carry a newer version string"},
	{Route: "GET /api/config*", Path: "http.port", Ignore: true, Why: "own port"},
	{Route: "GET /api/config*", Path: "feeds[].port", Ignore: true, Why: "own feed port"},
	{Route: "*", Path: "*dir", Ignore: true, Why: "a path inside the instance's own data directory"},
	{Route: "*", Path: "*Dir", Ignore: true, Why: "a path inside the instance's own data directory"},
	{Route: "GET /api/config*", Path: "locked[].origin", Ignore: true, Why: "path of the instance's own operator file"},
	{Route: "*", Path: "*.usage.*", Ignore: true, Why: "Open-Meteo request quota of the process (calls made by /api/geo/elevation; the server-reported remainder is shared by both)"},
	{Route: "*", Path: "*Nanos", Ignore: true, Why: "CPU time of the last evaluation"},
	{Route: "*", Path: "*Micros", Ignore: true, Why: "CPU time of the last evaluation"},
	{Route: "*", Path: "*sessionId*", Ignore: true, Why: "random per process"},
	{Route: "*", Path: "*csrf*", Ignore: true, Why: "random per process"},

	// Wall clock of the request / computation.
	{Route: "*", Path: "now", Ignore: true, Why: "server time of the response"},
	{Route: "*", Path: "*.now", Ignore: true, Why: "server time of the response"},
	{Route: "*", Path: "*generated*", Ignore: true, Why: "time the answer was computed"},
	{Route: "*", Path: "*computed*", Ignore: true, Why: "time the answer was computed"},
	{Route: "*", Path: "*serverTime*", Ignore: true, Why: "server time of the response"},
	{Route: "*", Path: "*cachedAt*", Ignore: true, Why: "cache time"},
	{Route: "*", Path: "*elapsed*", Ignore: true, Why: "computation time"},
	{Route: "*", Path: "*durationMs*", Ignore: true, Why: "computation time"},
	{Route: "*", Path: "*tookMs*", Ignore: true, Why: "computation time"},
	{Route: "*", Path: "*lastMs", Abs: 5000, Why: "time of the last update / check, a few s apart between two processes"},
	{Route: "*", Path: "*finishedMs", Abs: 5000, Why: "time a background job (analysis, import) finished in this process"},
	{Route: "*", Path: "*ago*", Abs: 30, Why: "age relative to now"},
	{Route: "*", Path: "*Ago*", Abs: 30, Why: "age relative to now"},
	{Route: "*", Path: "*age", Abs: 30, Why: "age relative to now (s)"},
	{Route: "*", Path: "*Age", Abs: 30, Why: "age relative to now (s)"},
	{Route: "*", Path: "*seen", Abs: 30, Why: "seconds since the last message"},
	{Route: "GET /api/rf/*", Path: "*seconds", Abs: 2, Why: "the RF statistics window is min(time since the process started, ~1 h); two processes started a moment apart are a second apart"},
	{Route: "GET /api/rf/*", Path: "*windowSec", Abs: 2, Why: "the RF statistics window is min(time since the process started, ~1 h); two processes started a moment apart are a second apart"},

	// Live mode only: values that move with arrival timing.
	{Route: "GET /api/timing/latency*", Path: "histograms*", Ignore: true, Live: true, Why: "histograms of the host's own receive / processing delay (scheduler timing of each process)"},
	{Route: "GET /api/timing/latency*", Path: "waterfall*", Ignore: true, Live: true, Why: "per-frame host processing stages (ns, scheduler timing)"},
	{Route: "GET /api/timing/latency*", Path: "*proc*", Ignore: true, Live: true, Why: "host processing delay (scheduler timing)"},
	{Route: "GET /api/timing/*", Path: "*hist[]*", Ignore: true, Live: true, Why: "per-feed histograms of the host arrival delay (scheduler timing of each process; differs on a loaded host)"},
	{Route: "GET /api/timing/*", Path: "*maxMs", Ignore: true, Live: true, Why: "worst host processing stage delay (scheduler timing)"},
	{Route: "GET /api/timing/*", Path: "*issues[]", Ignore: true, Live: true, Why: "findings text quoting the host-clock drift fitted from arrival times (see *drift*)"},
	{Route: "GET /api/timing/*", Path: "*findings[]*", Ignore: true, Live: true, Why: "findings text quoting the host-clock drift fitted from arrival times (see *drift*)"}, {Route: "GET /api/timing/*", Path: "*drift*", Ignore: true, Live: true, Why: "receiver-clock drift against the host clock, fitted from the host arrival times of a few minutes of frames"},
	{Route: "GET /api/timing/*", Path: "*ffset*", Abs: 5, Rel: 0.5, Live: true, Why: "receiver-clock offset against the host clock, measured from host arrival times"},
	{Route: "*", Path: "trail*", Abs: 10, Rel: 0.25, Live: true, Why: "live and stored trail points are taken every trailIntervalSec from the arrival timing, so the two builds can keep neighbouring samples (RSSI differs by several dB between them; positions by less than the live tolerance)"},
	{Route: "GET /api/history/flights/*", Path: "points*", Abs: 10, Rel: 0.25, Live: true, Why: "stored trail points of a live flight, as trail*"},
	{Route: "GET /api/rf/*", Path: "*.expected[]", Rel: 0.25, Live: true, Why: "expected squitter counts are derived from the receive-time spacing of each aircraft's squitters (host arrival timing)"},
	{Route: "*", Path: "*rate*", Rel: 0.25, Live: true, Why: "message rates over a sliding window ending at the request"},
	{Route: "*", Path: "*Rate*", Rel: 0.25, Live: true, Why: "message rates over a sliding window ending at the request"},
	{Route: "*", Path: "*fitted", Abs: 300000, Why: "time the forecast model was fitted (ms): a few s after each start, and kept in the database, so in the crash phase the new build reports the fit of an earlier, killed start (the kills span ~2 min)"},
	{Route: "GET /api/*daynight*", Path: "byHour[]*", OldNull: true, Live: true, Why: "visible-after (the open hour): 1.0.0 shows the open hour after its 5-min range_hour partial write, which the live comparison may come before; 1.1 merges the open hour from the tracker's overlay at once"},
	{Route: "GET /api/*daynight*", Path: "phases[]*", OldNull: true, Live: true, Why: "visible-after (the open hour), as byHour"},
	{Route: "GET /api/timing/*", Path: "*status", Ignore: true, Live: true, Why: "ok/warn verdict on the host arrival timing (scheduler load of each process)"},
	{Route: "GET /api/astro/terminator*", Path: "*coordinates*", Abs: 0.05, Why: "the night side is computed for the request time; the two requests are up to a few seconds apart (the sun moves 0.004 deg/s)"},

	// met-precision (design §3.9): met observations are stored as scaled
	// integers (lat/lon 1e-5 deg, wind direction 0.1 deg, wind 0.1 kt, OAT
	// 0.01 C), finer than the BDS 4,4 / 5,0 / 6,0 sources (1 kt, ~0.7 deg,
	// 0.25 C). Met answers and the met_obs export may differ by the
	// rounding of the scaled value.
	{Route: "GET /api/met/*", Path: "*", Abs: 0.06, Why: "met-precision: scaled-integer met observations (half of the 0.1 storage step, plus float noise)"},
	{Route: "GET /api/db/export/{file} [met_obs.csv]", Path: "*", Abs: 0.06, Why: "met-precision: met_obs is exported from the scaled-integer met_sample table"},
}

func init() {
	for i := range tolerances {
		t := &tolerances[i]
		t.rr = globRe(t.Route)
		t.pr = globRe(t.Path)
	}
}

func globRe(g string) *regexp.Regexp {
	return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(g), `\*`, ".*") + "$")
}

// ruleFor returns the first tolerance that applies to a leaf, or nil.
func ruleFor(mode, route, path string) *tolerance {
	for i := range tolerances {
		t := &tolerances[i]
		if t.Live && mode != modeLive {
			continue
		}
		if t.rr.MatchString(route) && t.pr.MatchString(path) {
			return t
		}
	}
	return nil
}

// differ collects the differences of one response pair.
type differ struct {
	mode  string
	route string
	diffs []string
	max   int
}

func (d *differ) add(path, f string, args ...any) {
	if len(d.diffs) < d.max {
		d.diffs = append(d.diffs, fmt.Sprintf("%s: ", orRoot(path))+fmt.Sprintf(f, args...))
	} else if len(d.diffs) == d.max {
		d.diffs = append(d.diffs, "... (more differences)")
	}
}

func orRoot(p string) string {
	if p == "" {
		return "$"
	}
	return p
}

// compareJSON compares two decoded JSON documents.
func (d *differ) compareJSON(path string, a, b any) {
	if r := ruleFor(d.mode, d.route, path); r != nil && r.Ignore {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			d.add(path, "old is an object, new is %s", typeName(b))
			return
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		ks := make([]string, 0, len(keys))
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			p := join(path, k)
			x, okA := av[k]
			y, okB := bv[k]
			switch {
			case okA && okB:
				d.compareJSON(p, x, y)
			case ruleIgnores(d, p):
			case okA:
				d.add(p, "missing in new (old: %s)", short(x))
			default:
				d.add(p, "only in new (%s)", short(y))
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			d.add(path, "old is an array, new is %s", typeName(b))
			return
		}
		d.compareArrays(path, av, bv)
	case float64:
		bv, ok := b.(float64)
		if !ok {
			d.add(path, "old %v, new %s", av, short(b))
			return
		}
		if !d.numEqual(path, av, bv) {
			d.add(path, "old %v, new %v", fmtNum(av), fmtNum(bv))
		}
	default:
		if a == nil {
			if r := ruleFor(d.mode, d.route, path); r != nil && r.OldNull {
				return
			}
		}
		if !jsonEqual(a, b) {
			// Strings that hold numbers (some IDs / CSV-ish fields).
			as, okA := a.(string)
			bs, okB := b.(string)
			if okA && okB {
				fa, e1 := strconv.ParseFloat(as, 64)
				fb, e2 := strconv.ParseFloat(bs, 64)
				if e1 == nil && e2 == nil && d.numEqual(path, fa, fb) {
					return
				}
				// Sentences with numbers ("measured boresight 2°, ..."):
				// same words, numbers within the mode's tolerance.
				if d.lineEqual(as, bs) {
					return
				}
			}
			d.add(path, "old %s, new %s", short(a), short(b))
		}
	}
}

func ruleIgnores(d *differ, p string) bool {
	r := ruleFor(d.mode, d.route, p)
	return r != nil && r.Ignore
}

func (d *differ) numEqual(path string, a, b float64) bool {
	if a == b || (math.IsNaN(a) && math.IsNaN(b)) {
		return true
	}
	diff := math.Abs(a - b)
	scale := math.Max(math.Abs(a), math.Abs(b))
	if diff <= 1e-9*scale {
		return true // float formatting
	}
	if r := ruleFor(d.mode, d.route, path); r != nil {
		if r.Abs > 0 && diff <= r.Abs {
			return true
		}
		if r.Rel > 0 && diff <= r.Rel*scale {
			return true
		}
	}
	if d.mode == modeLive {
		return diff <= liveNumAbs || diff <= liveNumRel*scale
	}
	return false
}

// idKeys are the fields that identify an element of a list of objects, in
// order of preference. Lists are matched by the first key that is present
// and unique in every element of both lists; else by position.
var idKeys = []string{"id", "icao", "hex", "flightId", "key", "table", "t", "time", "day", "hour", "ts", "name", "feed", "feedId", "ant", "code", "kind", "level", "bin", "band", "alt", "brg", "bearing", "az", "dist", "lead", "type"}

func (d *differ) compareArrays(path string, a, b []any) {
	ep := path + "[]"
	if key := commonKey(a, b); key != "" {
		ma, oa := index(a, key)
		mb, _ := index(b, key)
		var onlyA, onlyB []string
		for _, k := range oa {
			if _, ok := mb[k]; !ok {
				onlyA = append(onlyA, k)
			}
		}
		for k := range mb {
			if _, ok := ma[k]; !ok {
				onlyB = append(onlyB, k)
			}
		}
		sort.Strings(onlyB)
		allowed := 0
		if d.mode == modeLive {
			allowed = max(liveLenAbs, int(liveLenRel*float64(max(len(a), len(b)))))
		}
		if len(onlyA)+len(onlyB) > allowed && !ruleIgnores(d, ep) {
			d.add(path, "list keyed by %q: %d only in old %v, %d only in new %v", key, len(onlyA), head(onlyA), len(onlyB), head(onlyB))
		}
		for _, k := range oa {
			if y, ok := mb[k]; ok {
				d.compareJSON(ep, ma[k], y)
			}
		}
		return
	}
	n := min(len(a), len(b))
	if len(a) != len(b) {
		allowed := 0
		if d.mode == modeLive {
			allowed = max(liveLenAbs, int(liveLenRel*float64(max(len(a), len(b)))))
		}
		if absInt(len(a)-len(b)) > allowed && !ruleIgnores(d, ep) {
			d.add(path, "length old %d, new %d", len(a), len(b))
		}
		if d.mode == modeLive {
			// A list that grew at one end: compare the common prefix only
			// for small differences; positions shift otherwise.
			if absInt(len(a)-len(b)) > 0 {
				return
			}
		}
	}
	for i := 0; i < n; i++ {
		d.compareJSON(ep, a[i], b[i])
	}
}

func commonKey(a, b []any) string {
	if len(a) == 0 || len(b) == 0 {
		return ""
	}
	for _, k := range idKeys {
		if uniqueKey(a, k) && uniqueKey(b, k) {
			return k
		}
	}
	return ""
}

func uniqueKey(l []any, k string) bool {
	seen := map[string]bool{}
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			return false
		}
		v, ok := m[k]
		if !ok || v == nil {
			return false
		}
		s := short(v)
		if seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}

func index(l []any, k string) (map[string]any, []string) {
	m := map[string]any{}
	var order []string
	for _, e := range l {
		s := short(e.(map[string]any)[k])
		m[s] = e
		order = append(order, s)
	}
	return m, order
}

func head(l []string) []string {
	if len(l) > 5 {
		return append(l[:5:5], "...")
	}
	return l
}

func join(p, k string) string {
	if p == "" {
		return k
	}
	return p + "." + k
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case float64:
		return "a number"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	}
	return fmt.Sprintf("%T", v)
}

func short(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 80 {
		return string(b[:77]) + "..."
	}
	return string(b)
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// numToken splits a line into text and numbers for the text comparisons.
var numToken = regexp.MustCompile(`-?\d+(\.\d+)?([eE][-+]?\d+)?`)

// compareText compares CSV / KML / other text bodies line by line as a
// multiset (export order is not a feature; content is). In strict mode
// every line must have an equal partner (numbers equal within the float
// formatting epsilon, timestamp-like tokens excepted, see textNumEqual);
// in live mode up to liveLenRel of the lines may differ.
func (d *differ) compareText(a, b []byte) {
	la := splitLines(a)
	lb := splitLines(b)
	if len(la) > 0 && len(lb) > 0 && d.kindHeader() && la[0] != lb[0] {
		d.add("header", "old %q, new %q", trunc(la[0]), trunc(lb[0]))
	}
	// Exact matches first, then the rest by normalised form.
	cnt := map[string]int{}
	for _, l := range lb {
		cnt[l]++
	}
	var restA []string
	for _, l := range la {
		if cnt[l] > 0 {
			cnt[l]--
		} else {
			restA = append(restA, l)
		}
	}
	var restB []string
	for l, n := range cnt {
		for i := 0; i < n; i++ {
			restB = append(restB, l)
		}
	}
	sort.Strings(restB)
	used := make([]bool, len(restB))
	var unmatched []string
	for _, l := range restA {
		ok := false
		for j, m := range restB {
			if !used[j] && d.lineEqual(l, m) {
				used[j], ok = true, true
				break
			}
		}
		if !ok {
			unmatched = append(unmatched, l)
		}
	}
	var onlyB []string
	for j, m := range restB {
		if !used[j] {
			onlyB = append(onlyB, m)
		}
	}
	allowed := 0
	if d.mode == modeLive {
		allowed = max(liveLenAbs, int(liveLenRel*float64(max(len(la), len(lb)))))
	}
	if len(unmatched)+len(onlyB) > allowed {
		d.add("lines", "%d of %d lines only in old, %d of %d only in new; first old: %q; first new: %q",
			len(unmatched), len(la), len(onlyB), len(lb), trunc(first(unmatched)), trunc(first(onlyB)))
	}
}

func (d *differ) kindHeader() bool {
	return strings.Contains(d.route, ".csv") || strings.Contains(d.route, "csv")
}

func (d *differ) lineEqual(a, b string) bool {
	ta := numToken.Split(a, -1)
	tb := numToken.Split(b, -1)
	if len(ta) != len(tb) {
		return false
	}
	for i := range ta {
		if ta[i] != tb[i] {
			return false
		}
	}
	na := numToken.FindAllString(a, -1)
	nb := numToken.FindAllString(b, -1)
	if len(na) != len(nb) {
		return false
	}
	for i := range na {
		x, _ := strconv.ParseFloat(na[i], 64)
		y, _ := strconv.ParseFloat(nb[i], 64)
		if !d.textNumEqual(x, y) {
			return false
		}
	}
	return true
}

// textNumEqual: in text bodies there are no field names, so Unix-time
// tokens (s / ms in the plausible window) get the time tolerance of the
// JSON rules (5 s), everything else the mode's number tolerance.
func (d *differ) textNumEqual(x, y float64) bool {
	diff := math.Abs(x - y)
	scale := math.Max(math.Abs(x), math.Abs(y))
	if diff <= 1e-9*scale {
		return true
	}
	if (scale > 1e9 && scale < 2e9 && diff <= 5) || (scale > 1e12 && scale < 2e12 && diff <= 5000) {
		return true
	}
	// Route-wide numeric tolerances (Path "*") also cover text bodies.
	if r := ruleFor(d.mode, d.route, ""); r != nil && !r.Ignore {
		if (r.Abs > 0 && diff <= r.Abs) || (r.Rel > 0 && diff <= r.Rel*scale) {
			return true
		}
	}
	if d.mode == modeLive {
		return diff <= liveNumAbs || diff <= liveNumRel*scale
	}
	return false
}

func splitLines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func first(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

func trunc(s string) string {
	if len(s) > 160 {
		return s[:157] + "..."
	}
	return s
}

// normalize rewrites response shapes whose element order or cross
// references depend on processing order, before the comparison:
//
//   - /api/replay: frame rows reference aircraft[] by index; the index is
//     replaced by the flight ID and the rows of a frame sorted by it, so
//     two builds that list the same flights in a different order compare
//     equal (the order of aircraft[] is not a feature; the list itself is
//     compared keyed by flightId).
//   - live mode, the "longest tracked flight" record: every aircraft that
//     was in view since the feed started has the same tracked time, so
//     which of them holds the record is decided by processing order (a
//     tie). The aircraft's identity and the time the record was set (the
//     tracker re-checks the tie every few seconds, so each process sets it
//     at its own moment) are removed from that record, its event, notice
//     and alert; the value is still compared. Live events are then ordered
//     by time and content (see stripEventIDs).
func normalize(route, mode string, v any) any {
	if mode == modeLive {
		stripRecordTies(v)
		if route == "/api/notices" {
			stripNoticeTimes(v)
		}
		if route == "/api/stats/events" || route == "/api/stats/overview" {
			stripEventIDs(v)
		}
	}
	if route != "/api/replay" {
		return v
	}
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	ac, _ := m["aircraft"].([]any)
	frames, _ := m["frames"].([]any)
	for _, f := range frames {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		rows, _ := fm["p"].([]any)
		for _, r := range rows {
			row, ok := r.([]any)
			if !ok || len(row) == 0 {
				continue
			}
			if idx, ok := row[0].(float64); ok && int(idx) >= 0 && int(idx) < len(ac) {
				if a, ok := ac[int(idx)].(map[string]any); ok {
					row[0] = fmt.Sprintf("flight %v", a["flightId"])
				}
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return short(rows[i]) < short(rows[j]) })
	}
	return v
}

// tieIdentity are the fields that name the aircraft of a record, and the
// time ("t") the tie was decided.
var tieIdentity = []string{"t", "icao", "callsign", "reg", "type", "operator", "country", "countryCode", "category",
	"lat", "lon", "altFt", "brgDeg", "bearing", "distKm", "gsKt", "rssi", "squawk", "trackDeg", "vrFpm", "detail_json"}

func stripRecordTies(v any) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			stripRecordTies(e)
		}
	case map[string]any:
		if isLongestFlight(x) {
			strip(x)
			if d, ok := x["detail"].(map[string]any); ok {
				strip(d)
			}
			return
		}
		for _, e := range x {
			stripRecordTies(e)
		}
	}
}

func isLongestFlight(m map[string]any) bool {
	for _, mm := range []map[string]any{m, asMap(m["detail"])} {
		if mm == nil {
			continue
		}
		if k, _ := mm["key"].(string); k == "max_flight_s" {
			return true
		}
		if t, _ := mm["text"].(string); strings.Contains(t, "longest tracked flight") {
			return true
		}
	}
	return false
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func strip(m map[string]any) {
	for _, k := range tieIdentity {
		delete(m, k)
	}
	if t, ok := m["text"].(string); ok {
		if i := strings.LastIndex(t, " - "); i > 0 {
			m["text"] = t[:i]
		}
	}
}

// stripNoticeTimes removes the publication time (each process's own clock,
// ms apart) from live notices and orders them by content, so they are
// matched by what they say.
func stripNoticeTimes(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	l, ok := m["notices"].([]any)
	if !ok {
		return
	}
	for _, e := range l {
		if n, ok := e.(map[string]any); ok {
			delete(n, "t")
		}
	}
	sort.SliceStable(l, func(i, j int) bool { return short(l[i]) < short(l[j]) })
}

// stripEventIDs removes the row IDs of live events: an event that one
// process writes a moment earlier (feed status, timing warnings) shifts the
// IDs of all later ones; events are matched by time and content instead.
func stripEventIDs(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if l, ok := m["events"].([]any); ok {
		// The harness stops the simulation but keeps the connections
		// open, so each build's feed hits its 60-s "no data" timeout
		// while the comparison waits; whether a build has logged the
		// resulting feed_down / feed_up pair by then is timing (seen
		// both ways with two 1.0.0 builds). They are not compared.
		kept := l[:0]
		for _, e := range l {
			if n, ok := e.(map[string]any); ok {
				if k, _ := n["kind"].(string); k == "feed_down" || k == "feed_up" {
					continue
				}
			}
			kept = append(kept, e)
		}
		l = kept
		m["events"] = l
		for _, e := range l {
			if n, ok := e.(map[string]any); ok {
				delete(n, "id")
			}
		}
		// Events of the same second (a record of one aircraft sets several
		// records at once) are written in processing order, which differs
		// between two processes: order them by time, newest first, then
		// by content.
		sort.SliceStable(l, func(i, j int) bool {
			ti, tj := eventTime(l[i]), eventTime(l[j])
			if ti != tj {
				return ti > tj
			}
			return short(l[i]) < short(l[j])
		})
	}
}

func eventTime(v any) float64 {
	t, _ := asMap(v)["t"].(float64)
	return t
}
