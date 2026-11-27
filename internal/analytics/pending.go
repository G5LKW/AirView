package analytics

import (
	"context"
	"database/sql"
	"encoding/json"

	"airview/internal/store"
)

// Options are the inputs of an Analyzer that the engine wires
// (WithOptions).
type Options struct {
	// Pending returns the increments the tracker holds in memory and has
	// not written yet (store.PendingIncs; DB-EFFICIENCY-DESIGN §3.3). Every
	// analysis that covers the open day or hour (signal, coverage, outline,
	// elevation, heat, range) merges them with the writer's rules, so a
	// page answers what it would answer if they had been written. nil =
	// nothing pending (the 1.0.0 behaviour).
	Pending func() store.PendingIncs
}

// WithOptions sets the Analyzer's Options.
func WithOptions(o Options) Option {
	return func(a *Analyzer) { a.opts = o }
}

// WithPending sets Options.Pending only.
func WithPending(fn func() store.PendingIncs) Option {
	return func(a *Analyzer) { a.opts.Pending = fn }
}

// pending returns the tracker's unwritten increments (empty without a
// source).
func (a *Analyzer) pending() store.PendingIncs {
	if a.opts.Pending == nil {
		return store.PendingIncs{}
	}
	return a.opts.Pending()
}

// RangeHours returns the hourly range rows of antenna ant with from <= t <=
// to (Unix s; to <= 0 open), the tracker's partial row of the open hour
// merged in (store.RangeHours plus the overlay).
func (a *Analyzer) RangeHours(ctx context.Context, ant string, from, to int64) ([]store.RangeHour, error) {
	key := a.antKey(ant)
	rows, err := a.st.RangeHours(ctx, key, from, to)
	if err != nil {
		return nil, err
	}
	if p := a.pending().Range; len(p) > 0 {
		rows = store.OverlayRangeHours(rows, key, from, to, p)
	}
	return rows, nil
}

// frameAttrs are the aircraft / acdb attributes of one airframe as the
// top lists show them.
type frameAttrs struct{ op, cty, typ, desc, reg string }

// airframeAttrs looks up the attributes of the airframes of list (one
// query for all of them).
func airframeAttrs[T any](ctx context.Context, a *Analyzer, list []T, icao func(T) int64) (map[int64]frameAttrs, error) {
	out := make(map[int64]frameAttrs, len(list))
	if len(list) == 0 {
		return out, nil
	}
	ids := make([]int64, len(list))
	for i, v := range list {
		ids[i] = icao(v)
	}
	js, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	err = queryEach(ctx, a.flightDB(), `SELECT j.value,
  COALESCE(NULLIF(a.operator, ''), c.operator, ''), COALESCE(a.country, ''),
  COALESCE(NULLIF(a.type, ''), c.type, ''), COALESCE(c."desc", ''), COALESCE(NULLIF(a.reg, ''), c.reg, '')
FROM json_each(?) j LEFT JOIN aircraft a ON a.icao = j.value LEFT JOIN acdb c ON c.icao = j.value`, []any{string(js)}, func(rs *sql.Rows) error {
		var id int64
		var f frameAttrs
		if err := rs.Scan(&id, &f.op, &f.cty, &f.typ, &f.desc, &f.reg); err != nil {
			return err
		}
		out[id] = f
		return nil
	})
	return out, err
}
