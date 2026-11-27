package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShiftDays(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := openDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	day := now / 86400
	for _, s := range []string{
		`CREATE TABLE pos(flight_id INTEGER, t INTEGER, alt INTEGER, PRIMARY KEY(flight_id, t)) WITHOUT ROWID`,
		`CREATE TABLE cov(day INTEGER, bin INTEGER, count INTEGER, PRIMARY KEY(day, bin))`,
		`CREATE TABLE g(t INTEGER, n INTEGER)`,
		`CREATE TABLE kv(key TEXT PRIMARY KEY, value TEXT, updated INTEGER)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	// Two rows one day apart: an in-place UPDATE of the key would collide.
	mustExec(t, db, `INSERT INTO pos VALUES(1, ?, 30000), (1, ?, 31000)`, now, now-86400)
	mustExec(t, db, `INSERT INTO cov VALUES(?, 5, 7), (?, 5, 9)`, day, day-1)
	mustExec(t, db, `INSERT INTO g VALUES(?, 3)`, now*1000)
	mustExec(t, db, `INSERT INTO kv VALUES('w', ?, ?), ('name', 'x', ?)`, now, now, now)
	db.Close()

	if _, err := shiftDays(ctx, path, 1); err != nil {
		t.Fatal(err)
	}
	db, _ = openDB(path, true)
	defer db.Close()
	var lo, hi, alt int64
	_ = db.QueryRow(`SELECT min(t), max(t), max(alt) FROM pos`).Scan(&lo, &hi, &alt)
	if hi != now-86400 || lo != now-2*86400 || alt != 31000 {
		t.Errorf("pos: %d..%d alt %d", lo, hi, alt)
	}
	var d, cnt int64
	_ = db.QueryRow(`SELECT max(day), sum(count) FROM cov`).Scan(&d, &cnt)
	if d != day-1 || cnt != 16 {
		t.Errorf("cov: day %d (want %d), count %d", d, day-1, cnt)
	}
	var ms, n int64
	_ = db.QueryRow(`SELECT t, n FROM g`).Scan(&ms, &n)
	if ms != (now-86400)*1000 || n != 3 {
		t.Errorf("g: %d %d", ms, n)
	}
	var w, name string
	_ = db.QueryRow(`SELECT value FROM kv WHERE key='w'`).Scan(&w)
	_ = db.QueryRow(`SELECT value FROM kv WHERE key='name'`).Scan(&name)
	if w != itoa(now-86400) || name != "x" {
		t.Errorf("kv: %s %s", w, name)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }

func diffJSON(t *testing.T, mode, route, a, b string) []string {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		t.Fatal(err)
	}
	d := &differ{mode: mode, route: route, max: 20}
	path := strings.Fields(route)[1]
	d.compareJSON("", normalize(path, mode, va), normalize(path, mode, vb))
	return d.diffs
}

func TestCompareJSON(t *testing.T) {
	cases := []struct {
		name, mode, route, a, b string
		want                    int
	}{
		{"equal", modeStrict, "GET /x", `{"a":1,"b":[1,2]}`, `{"b":[1,2],"a":1}`, 0},
		{"missing key", modeStrict, "GET /x", `{"a":1,"b":2}`, `{"a":1}`, 1},
		{"value", modeStrict, "GET /x", `{"a":1}`, `{"a":2}`, 1},
		{"null vs number", modeStrict, "GET /x", `{"a":null}`, `{"a":2}`, 1},
		{"keyed reorder", modeStrict, "GET /x", `{"l":[{"icao":"A","n":1},{"icao":"B","n":2}]}`, `{"l":[{"icao":"B","n":2},{"icao":"A","n":1}]}`, 0},
		{"keyed missing", modeStrict, "GET /x", `{"l":[{"icao":"A"},{"icao":"B"}]}`, `{"l":[{"icao":"A"}]}`, 1},
		{"emptied list", modeStrict, "GET /x", `{"l":[1,2,3]}`, `{"l":[]}`, 1},
		{"ignored now", modeStrict, "GET /x", `{"now":1,"a":1}`, `{"now":2,"a":1}`, 0},
		{"db stats file sizes ignored", modeStrict, "GET /api/db/stats", `{"stats":{"dbBytes":100}}`, `{"stats":{"dbBytes":10}}`, 0},
		{"db stats new objects ignored", modeStrict, "GET /api/db/stats", `{"tables":{"flights":3}}`, `{"tables":{"flights":3,"trail_chunk":9},"profile":{"name":"standard"},"migration":{"state":"done"}}`, 0},
		{"db stats table counts compared", modeStrict, "GET /api/db/stats", `{"tables":{"positions":100}}`, `{"tables":{"positions":99}}`, 1},
		{"met precision", modeStrict, "GET /api/met/observations", `{"lat":50.123456}`, `{"lat":50.12346}`, 0},
		{"met beyond precision", modeStrict, "GET /api/met/observations", `{"lat":50.1}`, `{"lat":50.2}`, 1},
		{"sentence numbers strict", modeStrict, "GET /x", `{"s":"span 23 dB"}`, `{"s":"span 22 dB"}`, 1},
		{"sentence numbers live", modeLive, "GET /x", `{"s":"span 23 dB"}`, `{"s":"span 22 dB"}`, 0},
		{"sentence words", modeLive, "GET /x", `{"s":"terms: horizon"}`, `{"s":"terms: signal"}`, 1},
		{"live number tol", modeLive, "GET /x", `{"n":100}`, `{"n":105}`, 0},
		{"live number off", modeLive, "GET /x", `{"n":100}`, `{"n":150}`, 1},
		{"live structure", modeLive, "GET /x", `{"n":100,"k":1}`, `{"n":100}`, 1},
		{"record tie live", modeLive, "GET /api/notices",
			`{"notices":[{"id":"r1","icao":"3C5AAD","text":"New record: longest tracked flight 221 s - DLH73VC (3C5AAD)"}]}`,
			`{"notices":[{"id":"r1","icao":"400F99","text":"New record: longest tracked flight 221 s - EZY1506 (400F99)"}]}`, 0},
		{"record tie strict", modeStrict, "GET /api/notices",
			`{"notices":[{"id":"r1","icao":"3C5AAD","text":"New record: longest tracked flight 221 s - DLH73VC (3C5AAD)"}]}`,
			`{"notices":[{"id":"r1","icao":"400F99","text":"New record: longest tracked flight 221 s - EZY1506 (400F99)"}]}`, 2},
		{"replay index order", modeStrict, "GET /api/replay",
			`{"aircraft":[{"flightId":7},{"flightId":9}],"frames":[{"t":1,"p":[[0,50.1],[1,51.2]]}]}`,
			`{"aircraft":[{"flightId":9},{"flightId":7}],"frames":[{"t":1,"p":[[0,51.2],[1,50.1]]}]}`, 0},
	}
	for _, c := range cases {
		if got := diffJSON(t, c.mode, c.route, c.a, c.b); len(got) != c.want {
			t.Errorf("%s: %d differences %v, want %d", c.name, len(got), got, c.want)
		}
	}
}

func TestCompareText(t *testing.T) {
	d := &differ{mode: modeStrict, route: "GET /x.csv", max: 5}
	d.compareText([]byte("a,b\n1,2\n3,4\n"), []byte("a,b\n3,4\n1,2\n"))
	if len(d.diffs) != 0 {
		t.Errorf("reordered lines: %v", d.diffs)
	}
	d = &differ{mode: modeStrict, route: "GET /x.csv", max: 5}
	d.compareText([]byte("a,b\n1,2\n3,4\n"), []byte("a,b\n1,2\n"))
	if len(d.diffs) != 1 {
		t.Errorf("lost line: %v", d.diffs)
	}
	d = &differ{mode: modeStrict, route: "GET /x.kml", max: 5}
	d.compareText([]byte("<when>1794700000</when>"), []byte("<when>1794700003</when>"))
	if len(d.diffs) != 0 {
		t.Errorf("time token: %v", d.diffs)
	}
}

func TestRoutesCovered(t *testing.T) {
	ph := map[string]string{"live": "3C6444", "seen": "3C6444", "flight": "1", "histFlight": "1", "histICAO": "3C6444",
		"airport": "EDDF", "route": "DLH400?lat=50.2&lon=7.9", "watchlist": "", "exportFiles": "records.csv,flights.csv"}
	calls := buildCalls(ph, 1, 2)
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c.Route] = true
		if strings.Contains(c.URL, "{") {
			t.Errorf("unresolved placeholder in %s", c.URL)
		}
	}
	for _, r := range []string{"/api/aircraft/{icao}", "/api/history/flights/{id}/trace", "/api/export/flight/{file}",
		"/api/db/export/{file}", "/api/replay", "/api/stats/daily", "/api/export/live.kml", "/api/reports.csv"} {
		if !seen[r] {
			t.Errorf("route %s is not called", r)
		}
	}
}

func TestReplayUntruncated(t *testing.T) {
	old := map[string]any{"truncated": true, "frames": []any{map[string]any{"t": 1.0}, map[string]any{"t": 2.0}}}
	full := map[string]any{"truncated": false, "frames": []any{map[string]any{"t": 1.0}, map[string]any{"t": 2.0}, map[string]any{"t": 3.0}}}
	if !replayUntruncated("/api/replay", old, full) || replayUntruncated("/api/replay", full, full) || replayUntruncated("/api/x", old, full) {
		t.Fatal("replayUntruncated")
	}
	if msg := replayCovers(old, full); msg != "" {
		t.Fatal(msg)
	}
	if msg := replayCovers(full, old); msg == "" {
		t.Fatal("missing frames not reported")
	}
}

func TestSortRowsAndCutoff(t *testing.T) {
	rows := [][]any{{int64(2), "b"}, {nil, "z"}, {int64(1), "c"}, {int64(1), "a"}}
	sortRows(rows)
	got := fmt.Sprint(rows)
	if got != "[[<nil> z] [1 a] [1 c] [2 b]]" {
		t.Fatal(got)
	}
	cut := time.Unix(1_800_000_000, 0)
	if !afterCutoff(1_800_000_100, cut) || afterCutoff(1_799_999_000, cut) || afterCutoff(42, cut) {
		t.Fatal("afterCutoff seconds")
	}
	if !afterCutoff(1_800_000_100_000, cut) || afterCutoff(20833, cut) {
		t.Fatal("afterCutoff ms / day")
	}
}

func TestSameLines(t *testing.T) {
	if !sameLines([]byte("a\nb\n"), []byte("b\na\n")) || sameLines([]byte("a\nb"), []byte("a\nc")) {
		t.Fatal("sameLines")
	}
}

func TestSqlNear(t *testing.T) {
	cases := []struct {
		a, b any
		abs  float64
		want bool
	}{
		{49.396636962890625, 49.39664, 0.06, true},
		{49.396636962890625, 49.39664, 0, false},
		{int64(3), 3.04, 0.06, true},
		{1.0, 1.1, 0.06, false},
		{nil, 1.0, 0.06, false},
		{"a", "a", 0, true},
	}
	for _, c := range cases {
		if got := sqlNear(c.a, c.b, c.abs); got != c.want {
			t.Errorf("sqlNear(%v, %v, %v) = %v", c.a, c.b, c.abs, got)
		}
	}
	if rowAbs["met_obs.lat"] == 0 || rowAbs["met_obs.alt"] != 0 {
		t.Error("met-precision row tolerances")
	}
}

func TestOldNullTolerance(t *testing.T) {
	const route = "GET /api/stats/daynight"
	if d := diffJSON(t, modeLive, route, `{"byHour":[{"hour":1,"aircraft":null}]}`, `{"byHour":[{"hour":1,"aircraft":49}]}`); len(d) != 0 {
		t.Errorf("open hour (old null) in live mode: %v", d)
	}
	if d := diffJSON(t, modeStrict, route, `{"byHour":[{"hour":1,"aircraft":null}]}`, `{"byHour":[{"hour":1,"aircraft":49}]}`); len(d) != 1 {
		t.Errorf("strict mode accepts old null: %v", d)
	}
	if d := diffJSON(t, modeLive, route, `{"byHour":[{"hour":1,"aircraft":20}]}`, `{"byHour":[{"hour":1,"aircraft":null}]}`); len(d) != 1 {
		t.Errorf("new null accepted: %v", d)
	}
	if d := diffJSON(t, modeLive, route, `{"byHour":[{"hour":1,"aircraft":20}]}`, `{"byHour":[{"hour":1,"aircraft":49}]}`); len(d) != 1 {
		t.Errorf("values on both sides not compared: %v", d)
	}
}

func TestLiveEventsWithoutFeedTimeouts(t *testing.T) {
	const route = "GET /api/stats/events"
	old := `{"events":[{"id":9,"t":5000,"kind":"record","detail":{"key":"max_aircraft","value":58}},{"id":8,"t":4000,"kind":"military"}]}`
	new := `{"events":[{"id":11,"t":6000,"kind":"feed_up"},{"id":10,"t":5999,"kind":"feed_down"},{"id":9,"t":5000,"kind":"record","detail":{"key":"max_aircraft","value":58}},{"id":8,"t":4000,"kind":"military"}]}`
	if d := diffJSON(t, modeLive, route, old, new); len(d) != 0 {
		t.Errorf("live: %v", d)
	}
	if d := diffJSON(t, modeStrict, route, old, new); len(d) == 0 {
		t.Error("strict mode must still see the feed events")
	}
}
