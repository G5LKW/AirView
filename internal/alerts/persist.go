package alerts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"airview/internal/store"
)

// MigrationName is the store migration owner name of this package.
const MigrationName = "alerts"

const migrationV1 = `CREATE TABLE IF NOT EXISTS alerts_rule(
  id TEXT PRIMARY KEY,
  json TEXT NOT NULL,
  updated INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS alerts_watch(
  list TEXT NOT NULL,
  icao INTEGER NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  added INTEGER NOT NULL,
  PRIMARY KEY(list, icao)
) WITHOUT ROWID;`

// kvSeeded marks that the default rules were written once (so deleting a
// default rule is permanent until ResetDefaults).
const kvSeeded = "alerts.seeded"

func init() {
	if err := store.RegisterMigration(MigrationName, 1, migrationV1); err != nil {
		panic(err)
	}
}

// Errors.
var (
	ErrNotFound = errors.New("alerts: not found")
	ErrTooMany  = errors.New("alerts: too many rules or lists")
)

// Load reads rules and watch-lists from the store (seeding the default rules
// on first use). Without a store the engine keeps the built-in defaults in memory.
func (e *Engine) Load(ctx context.Context) error {
	if e.st == nil {
		return nil
	}
	e.cfgMu.Lock()
	defer e.cfgMu.Unlock()
	db := e.st.DB()
	rows, err := db.QueryContext(ctx, `SELECT id, json FROM alerts_rule ORDER BY id`)
	if err != nil {
		return err
	}
	var rules []Rule
	for rows.Next() {
		var id, js string
		if err := rows.Scan(&id, &js); err != nil {
			rows.Close()
			return err
		}
		r, err := ParseRule([]byte(js))
		if err != nil || r.ID != id {
			e.log.Warn("alerts: skipping invalid stored rule", "id", id, "err", err)
			continue
		}
		rules = append(rules, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	_, seeded, err := e.st.KVGet(ctx, kvSeeded)
	if err != nil {
		return err
	}
	if !seeded && len(rules) == 0 {
		rules = DefaultRules()
		if err := e.saveRules(ctx, rules, false); err != nil {
			return err
		}
		if err := e.st.KVSet(ctx, kvSeeded, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	e.rules.Store(newRuleSet(rules))

	wrows, err := db.QueryContext(ctx, `SELECT list, icao, note, added FROM alerts_watch`)
	if err != nil {
		return err
	}
	defer wrows.Close()
	ws := &watchSet{lists: map[string]map[uint32]watchEntry{}}
	for wrows.Next() {
		var list, note string
		var icao, added int64
		if err := wrows.Scan(&list, &icao, &note, &added); err != nil {
			return err
		}
		l := ws.lists[list]
		if l == nil {
			l = map[uint32]watchEntry{}
			ws.lists[list] = l
		}
		l[uint32(icao)] = watchEntry{note: note, added: added}
	}
	if err := wrows.Err(); err != nil {
		return err
	}
	e.watch.Store(ws)
	return nil
}

func (e *Engine) saveRules(ctx context.Context, rules []Rule, replaceAll bool) error {
	if e.st == nil {
		return nil
	}
	return e.st.Write(ctx, func(tx *sql.Tx) error {
		if replaceAll {
			if _, err := tx.ExecContext(ctx, `DELETE FROM alerts_rule`); err != nil {
				return err
			}
		}
		for _, r := range rules {
			b, err := json.Marshal(r)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO alerts_rule(id, json, updated) VALUES(?,?,?)`,
				r.ID, string(b), time.Now().Unix()); err != nil {
				return err
			}
		}
		return nil
	})
}

// Rules returns all rules sorted by ID.
func (e *Engine) Rules() []Rule {
	rs := e.rules.Load()
	out := make([]Rule, len(rs.all))
	for i, c := range rs.all {
		out[i] = cloneRule(c.rule)
	}
	return out
}

// Rule returns one rule.
func (e *Engine) Rule(id string) (Rule, bool) {
	c, ok := e.rules.Load().byID[id]
	if !ok {
		return Rule{}, false
	}
	return cloneRule(c.rule), true
}

func cloneRule(r Rule) Rule {
	b, _ := json.Marshal(r)
	var out Rule
	_ = json.Unmarshal(b, &out)
	return out
}

func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "r-" + hex.EncodeToString(b[:])
}

func isBuiltinID(id string) bool {
	for _, d := range DefaultRules() {
		if d.ID == id {
			return true
		}
	}
	return false
}

// PutRule creates (empty ID) or replaces a rule after validation and returns
// the stored version.
func (e *Engine) PutRule(ctx context.Context, r Rule) (Rule, error) {
	if err := r.Normalize(); err != nil {
		return Rule{}, err
	}
	e.cfgMu.Lock()
	defer e.cfgMu.Unlock()
	rs := e.rules.Load()
	now := time.Now().UnixMilli()
	if r.ID == "" {
		r.ID = newID()
	}
	old, exists := rs.byID[r.ID]
	if !exists && len(rs.all) >= MaxRules {
		return Rule{}, fmt.Errorf("%w: at most %d rules", ErrTooMany, MaxRules)
	}
	r.Builtin = isBuiltinID(r.ID)
	r.Created = now
	if exists && old.rule.Created != 0 {
		r.Created = old.rule.Created
	}
	r.Updated = now
	if err := e.saveRules(ctx, []Rule{r}, false); err != nil {
		return Rule{}, err
	}
	rules := make([]Rule, 0, len(rs.all)+1)
	for _, c := range rs.all {
		if c.rule.ID != r.ID {
			rules = append(rules, c.rule)
		}
	}
	rules = append(rules, r)
	e.rules.Store(newRuleSet(rules))
	return cloneRule(r), nil
}

// DeleteRule removes a rule (built-in rules too; ResetDefaults restores them).
func (e *Engine) DeleteRule(ctx context.Context, id string) error {
	e.cfgMu.Lock()
	defer e.cfgMu.Unlock()
	rs := e.rules.Load()
	if _, ok := rs.byID[id]; !ok {
		return ErrNotFound
	}
	if e.st != nil {
		if err := e.st.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM alerts_rule WHERE id = ?`, id)
			return err
		}); err != nil {
			return err
		}
	}
	rules := make([]Rule, 0, len(rs.all))
	for _, c := range rs.all {
		if c.rule.ID != id {
			rules = append(rules, c.rule)
		}
	}
	e.rules.Store(newRuleSet(rules))
	return nil
}

// SetEnabled switches a rule on or off.
func (e *Engine) SetEnabled(ctx context.Context, id string, on bool) (Rule, error) {
	r, ok := e.Rule(id)
	if !ok {
		return Rule{}, ErrNotFound
	}
	r.Enabled = on
	return e.PutRule(ctx, r)
}

// ResetDefaults restores the built-in rules to their shipped state (user
// rules are kept).
func (e *Engine) ResetDefaults(ctx context.Context) error {
	e.cfgMu.Lock()
	defer e.cfgMu.Unlock()
	defs := DefaultRules()
	if err := e.saveRules(ctx, defs, false); err != nil {
		return err
	}
	rs := e.rules.Load()
	rules := defs
	for _, c := range rs.all {
		if !isBuiltinID(c.rule.ID) {
			rules = append(rules, c.rule)
		}
	}
	e.rules.Store(newRuleSet(rules))
	return nil
}

// ExportRules returns all rules as indented JSON (an array).
func (e *Engine) ExportRules() ([]byte, error) {
	return json.MarshalIndent(e.Rules(), "", "  ")
}

// ImportRules adds/replaces rules from a JSON array (all are validated
// first; nothing is changed when one is invalid).
func (e *Engine) ImportRules(ctx context.Context, data []byte) (int, error) {
	if len(data) > MaxRuleBytes*MaxRules/8 {
		return 0, fmt.Errorf("%w: import too large", ErrInvalid)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(raw) > MaxRules {
		return 0, fmt.Errorf("%w: at most %d rules", ErrTooMany, MaxRules)
	}
	rules := make([]Rule, 0, len(raw))
	for i, m := range raw {
		r, err := ParseRule(m)
		if err != nil {
			return 0, fmt.Errorf("rule %d: %w", i+1, err)
		}
		rules = append(rules, r)
	}
	for _, r := range rules {
		if _, err := e.PutRule(ctx, r); err != nil {
			return 0, err
		}
	}
	return len(rules), nil
}

// --- watch-lists ---

// Watchlists returns the lists with their sizes, sorted by name.
func (e *Engine) Watchlists() []WatchlistInfo {
	ws := e.watch.Load()
	out := make([]WatchlistInfo, 0, len(ws.lists))
	for n, l := range ws.lists {
		out = append(out, WatchlistInfo{Name: n, Entries: len(l)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Watchlist returns the entries of one list sorted by ICAO.
func (e *Engine) Watchlist(name string) ([]WatchEntry, bool) {
	l, ok := e.watch.Load().lists[name]
	if !ok {
		return nil, false
	}
	out := make([]WatchEntry, 0, len(l))
	for icao, w := range l {
		out = append(out, WatchEntry{ICAO: hex6(icao), Note: w.note, Added: w.added * 1000})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ICAO < out[j].ICAO })
	return out, true
}

// WatchNote returns (list, note, true) when icao is on any watch-list (for
// the aircraft details panel).
func (e *Engine) WatchNote(icao uint32) (string, string, bool) {
	return e.watch.Load().note(icao)
}

// ImportWatchlist reads a text/CSV list (see ParseWatchlist) into list name.
// replace=true replaces the list, otherwise entries are merged. Returns the
// number of entries imported and of rejected lines.
func (e *Engine) ImportWatchlist(ctx context.Context, name string, r io.Reader, replace bool) (int, int, error) {
	name = strings.TrimSpace(name)
	if !validListName(name) {
		return 0, 0, fmt.Errorf("%w: list name must be 1-40 characters of letters, digits, space . _ -", ErrInvalid)
	}
	entries, order, bad, err := ParseWatchlist(r)
	if err != nil {
		return 0, bad, err
	}
	err = e.modifyList(ctx, name, func(l map[uint32]watchEntry, tx *sql.Tx, now int64) error {
		if replace {
			for k := range l {
				delete(l, k)
			}
			if tx != nil {
				if _, err := tx.ExecContext(ctx, `DELETE FROM alerts_watch WHERE list = ?`, name); err != nil {
					return err
				}
			}
		}
		for _, icao := range order {
			l[icao] = watchEntry{note: entries[icao], added: now}
			if tx != nil {
				if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO alerts_watch(list, icao, note, added) VALUES(?,?,?,?)`,
					name, int64(icao), entries[icao], now); err != nil {
					return err
				}
			}
		}
		if len(l) > MaxListEntries {
			return fmt.Errorf("%w: list would exceed %d entries", ErrTooMany, MaxListEntries)
		}
		return nil
	})
	return len(order), bad, err
}

// AddWatch adds or updates one entry.
func (e *Engine) AddWatch(ctx context.Context, list string, icao uint32, note string) error {
	if !validListName(list) || icao == 0 || icao > 0xFFFFFF {
		return fmt.Errorf("%w: list name or icao", ErrInvalid)
	}
	note = cleanNote(note)
	return e.modifyList(ctx, list, func(l map[uint32]watchEntry, tx *sql.Tx, now int64) error {
		l[icao] = watchEntry{note: note, added: now}
		if len(l) > MaxListEntries {
			return ErrTooMany
		}
		if tx == nil {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO alerts_watch(list, icao, note, added) VALUES(?,?,?,?)`,
			list, int64(icao), note, now)
		return err
	})
}

// RemoveWatch removes one entry.
func (e *Engine) RemoveWatch(ctx context.Context, list string, icao uint32) error {
	if _, ok := e.watch.Load().lists[list][icao]; !ok {
		return ErrNotFound
	}
	return e.modifyList(ctx, list, func(l map[uint32]watchEntry, tx *sql.Tx, _ int64) error {
		delete(l, icao)
		if tx == nil {
			return nil
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM alerts_watch WHERE list = ? AND icao = ?`, list, int64(icao))
		return err
	})
}

// DeleteWatchlist removes a whole list.
func (e *Engine) DeleteWatchlist(ctx context.Context, list string) error {
	e.cfgMu.Lock()
	defer e.cfgMu.Unlock()
	ws := e.watch.Load()
	if _, ok := ws.lists[list]; !ok {
		return ErrNotFound
	}
	if e.st != nil {
		if err := e.st.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM alerts_watch WHERE list = ?`, list)
			return err
		}); err != nil {
			return err
		}
	}
	n := ws.clone()
	delete(n.lists, list)
	e.watch.Store(n)
	return nil
}

// ExportWatchlist writes a list as "txt" or "csv".
func (e *Engine) ExportWatchlist(w io.Writer, list, format string) error {
	entries, ok := e.Watchlist(list)
	if !ok {
		return ErrNotFound
	}
	return WriteWatchlist(w, list, entries, format)
}

// modifyList applies fn to a copy of the list (and inside a store
// transaction when a store exists), then publishes the new snapshot.
func (e *Engine) modifyList(ctx context.Context, name string, fn func(l map[uint32]watchEntry, tx *sql.Tx, now int64) error) error {
	e.cfgMu.Lock()
	defer e.cfgMu.Unlock()
	ws := e.watch.Load()
	old, exists := ws.lists[name]
	if !exists && len(ws.lists) >= MaxLists {
		return fmt.Errorf("%w: at most %d watch-lists", ErrTooMany, MaxLists)
	}
	l := make(map[uint32]watchEntry, len(old)+1)
	for k, v := range old {
		l[k] = v
	}
	now := time.Now().Unix()
	if e.st != nil {
		if err := e.st.Write(ctx, func(tx *sql.Tx) error { return fn(l, tx, now) }); err != nil {
			return err
		}
	} else if err := fn(l, nil, now); err != nil {
		return err
	}
	n := ws.clone()
	n.lists[name] = l
	e.watch.Store(n)
	return nil
}
