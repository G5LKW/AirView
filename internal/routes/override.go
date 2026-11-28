package routes

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Operator overrides (docs/ROUTES-DESIGN.md §4). They live in
// routes_override (edited through the API, audited in routes_override_log)
// and in an optional file routes.overrides.csv in the data directory
// (callsign,airports,mode,note), whose rows are locked: the API shows them
// read-only and they win over database rows.

// Override modes.
const (
	ModeFixed  = "fixed"  // always this route
	ModeVerify = "verify" // a strong prior that live evidence can still contradict
	ModeBlock  = "block"  // never show a route for this callsign
)

// Errors of the override API.
var (
	ErrBadOverride = errors.New("routes: invalid override")
	ErrLocked      = errors.New("routes: this override comes from the overrides file and can only be changed there")
	ErrNotFound    = errors.New("routes: not found")
)

// Override is one operator correction.
type Override struct {
	Callsign string `json:"callsign"`
	Airports string `json:"airports"` // codes in flight order joined with "-" ("" for block)
	Mode     string `json:"mode"`
	Note     string `json:"note,omitempty"`
	Author   string `json:"author,omitempty"`
	Created  int64  `json:"created,omitempty"` // Unix s
	Expires  int64  `json:"expires,omitempty"` // Unix s, 0 = never
	Locked   bool   `json:"locked,omitempty"`  // from the overrides file
}

// OverrideLog is one audit entry.
type OverrideLog struct {
	ID       int64  `json:"id"`
	Callsign string `json:"callsign"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Author   string `json:"author"`
	T        int64  `json:"t"`
}

// normAirports validates and normalises "egll-kjfk" / "EGLL KJFK" to "EGLL-KJFK".
func normAirports(s string) (string, bool) {
	f := strings.FieldsFunc(strings.ToUpper(s), func(r rune) bool { return r == '-' || r == ' ' || r == ',' || r == '>' })
	if len(f) < 2 || len(f) > 8 {
		return "", false
	}
	for _, c := range f {
		if len(c) < 3 || len(c) > 4 || Normalize(c) != c {
			return "", false
		}
	}
	return strings.Join(f, "-"), true
}

// normOverride validates o in place.
func normOverride(o *Override) error {
	o.Callsign = Normalize(o.Callsign)
	if o.Callsign == "" {
		return fmt.Errorf("%w: callsign", ErrBadOverride)
	}
	o.Mode = strings.ToLower(strings.TrimSpace(o.Mode))
	if o.Mode == "" {
		o.Mode = ModeFixed
	}
	switch o.Mode {
	case ModeFixed, ModeVerify:
		a, ok := normAirports(o.Airports)
		if !ok {
			return fmt.Errorf("%w: airports must be 2 to 8 airport codes, e.g. EGLL-KJFK", ErrBadOverride)
		}
		o.Airports = a
	case ModeBlock:
		o.Airports = ""
	default:
		return fmt.Errorf("%w: mode must be fixed, verify or block", ErrBadOverride)
	}
	o.Note = strings.TrimSpace(o.Note)
	if len(o.Note) > 200 {
		o.Note = o.Note[:200]
	}
	if o.Expires < 0 {
		o.Expires = 0
	}
	return nil
}

func (o Override) summary() string {
	if o.Callsign == "" {
		return ""
	}
	if o.Mode == ModeBlock {
		return "block"
	}
	return o.Mode + " " + o.Airports
}

// loadOverrides reads the database rows.
func loadOverrides(ctx context.Context, db *sql.DB) (map[string]Override, error) {
	rows, err := db.QueryContext(ctx, `SELECT callsign, airports, mode, note, author, created, COALESCE(expires, 0) FROM routes_override`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Override{}
	for rows.Next() {
		var o Override
		if err := rows.Scan(&o.Callsign, &o.Airports, &o.Mode, &o.Note, &o.Author, &o.Created, &o.Expires); err != nil {
			return nil, err
		}
		out[o.Callsign] = o
	}
	return out, rows.Err()
}

// ParseOverridesCSV reads callsign,airports,mode,note rows (a header row
// and lines starting with '#' are skipped). Invalid rows are reported in
// bad and skipped.
func ParseOverridesCSV(r io.Reader) (out []Override, bad []string) {
	cr := csv.NewReader(bufio.NewReader(r))
	cr.Comment = '#'
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	for line := 1; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			bad = append(bad, err.Error())
			break
		}
		if len(rec) == 0 || (line == 1 && strings.EqualFold(strings.TrimSpace(rec[0]), "callsign")) {
			continue
		}
		o := Override{Callsign: rec[0], Author: "file", Locked: true}
		if len(rec) > 1 {
			o.Airports = rec[1]
		}
		if len(rec) > 2 {
			o.Mode = rec[2]
		}
		if len(rec) > 3 {
			o.Note = rec[3]
		}
		if err := normOverride(&o); err != nil {
			bad = append(bad, fmt.Sprintf("row %d: %v", line, err))
			continue
		}
		out = append(out, o)
	}
	return out, bad
}

// WriteOverridesCSV writes overrides in the file format (round-trips with
// ParseOverridesCSV).
func WriteOverridesCSV(w io.Writer, list []Override) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"callsign", "airports", "mode", "note"}); err != nil {
		return err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Callsign < list[j].Callsign })
	for _, o := range list {
		if err := cw.Write([]string{o.Callsign, o.Airports, o.Mode, o.Note}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// fileOverrides loads the overrides file if it changed since mod (zero mod
// = always). changed=false when the file's modification time is unchanged.
func fileOverrides(path string, mod time.Time) (list []Override, bad []string, newMod time.Time, changed bool) {
	if path == "" {
		return nil, nil, time.Time{}, !mod.IsZero()
	}
	fi, err := os.Stat(path)
	if err != nil {
		// gone (or never there): changed when it existed before
		return nil, nil, time.Time{}, !mod.IsZero()
	}
	if fi.ModTime().Equal(mod) {
		return nil, nil, mod, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, []string{err.Error()}, fi.ModTime(), true
	}
	defer f.Close()
	list, bad = ParseOverridesCSV(io.LimitReader(f, 8<<20))
	return list, bad, fi.ModTime(), true
}
