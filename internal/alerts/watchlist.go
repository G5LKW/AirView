package alerts

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// Watch-list limits.
const (
	MaxLists       = 32
	MaxListEntries = 100000
	MaxNoteLen     = 200
)

// WatchEntry is one airframe on a watch-list.
type WatchEntry struct {
	ICAO  string `json:"icao"` // 6 upper-case hex digits
	Note  string `json:"note,omitempty"`
	Added int64  `json:"added,omitempty"` // Unix ms
}

// WatchlistInfo summarises one list.
type WatchlistInfo struct {
	Name    string `json:"name"`
	Entries int    `json:"entries"`
}

type watchEntry struct {
	note  string
	added int64
}

// watchSet is an immutable snapshot of all lists (copy-on-write).
type watchSet struct {
	lists map[string]map[uint32]watchEntry
}

// contains reports whether icao is on one of the named lists (all lists when names is empty).
func (w *watchSet) contains(names []string, icao uint32) bool {
	if len(names) == 0 {
		for _, l := range w.lists {
			if _, ok := l[icao]; ok {
				return true
			}
		}
		return false
	}
	for _, n := range names {
		if _, ok := w.lists[n][icao]; ok {
			return true
		}
	}
	return false
}

// note returns the first note found for icao.
func (w *watchSet) note(icao uint32) (string, string, bool) {
	names := make([]string, 0, len(w.lists))
	for n := range w.lists {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if e, ok := w.lists[n][icao]; ok {
			return n, e.note, true
		}
	}
	return "", "", false
}

func (w *watchSet) clone() *watchSet {
	c := &watchSet{lists: make(map[string]map[uint32]watchEntry, len(w.lists))}
	for k, v := range w.lists {
		c.lists[k] = v
	}
	return c
}

// ParseWatchlist reads a text or CSV watch-list: one airframe per line, the
// hex address first, then an optional note, separated by a comma, semicolon,
// tab or whitespace. Lines starting with '#' or ';', blank lines and a header
// line ("icao,...", "hex,...") are ignored. Hex codes may carry a "~" prefix
// (ignored) and must be 1-6 hex digits. Returns the entries in file order
// (later duplicates replace earlier ones) and the number of rejected lines.
func ParseWatchlist(r io.Reader) (map[uint32]string, []uint32, int, error) {
	sc := bufio.NewScanner(io.LimitReader(r, 16<<20))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	out := map[uint32]string{}
	var order []uint32
	bad := 0
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if line == 1 {
			s = strings.TrimPrefix(s, "\xef\xbb\xbf")
		}
		if s == "" || s[0] == '#' || s[0] == ';' {
			continue
		}
		hex, note := splitFirst(s)
		low := strings.ToLower(strings.Trim(hex, `"`))
		if line == 1 && (low == "icao" || low == "hex" || low == "icao24" || low == "modes" || low == "mode_s") {
			continue
		}
		v, ok := parseHexICAO(strings.Trim(hex, `"`))
		if !ok || v == 0 {
			bad++
			continue
		}
		note = cleanNote(note)
		if _, dup := out[v]; !dup {
			order = append(order, v)
		}
		out[v] = note
		if len(out) > MaxListEntries {
			return nil, nil, bad, fmt.Errorf("%w: more than %d entries", ErrInvalid, MaxListEntries)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, bad, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return out, order, bad, nil
}

// splitFirst splits at the first separator; a quoted CSV note is unquoted.
func splitFirst(s string) (string, string) {
	i := strings.IndexAny(s, ",;\t ")
	if i < 0 {
		return s, ""
	}
	rest := strings.TrimLeft(s[i+1:], " \t")
	if strings.HasPrefix(rest, `"`) {
		if rec, err := csv.NewReader(strings.NewReader(rest)).Read(); err == nil && len(rec) > 0 {
			rest = rec[0]
		}
	}
	return s[:i], rest
}

func cleanNote(s string) string {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > MaxNoteLen {
		s = string([]rune(s)[:MaxNoteLen])
	}
	return s
}

// WriteWatchlist writes entries as "txt" ("HEX note" lines with a comment
// header) or "csv" ("icao,note" with a header row).
func WriteWatchlist(w io.Writer, name string, entries []WatchEntry, format string) error {
	switch format {
	case "csv":
		cw := csv.NewWriter(w)
		if err := cw.Write([]string{"icao", "note"}); err != nil {
			return err
		}
		for _, e := range entries {
			if err := cw.Write([]string{e.ICAO, e.Note}); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	case "", "txt", "text":
		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "# AirView watch-list %q (%d entries): hex address, then an optional note\n", name, len(entries))
		for _, e := range entries {
			if e.Note != "" {
				fmt.Fprintf(bw, "%s %s\n", e.ICAO, e.Note)
			} else {
				fmt.Fprintf(bw, "%s\n", e.ICAO)
			}
		}
		return bw.Flush()
	}
	return fmt.Errorf("%w: unknown format %q (txt or csv)", ErrInvalid, format)
}
