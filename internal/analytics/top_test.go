package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"airview/internal/store"
)

// legacyTop is the 1.0.0 Top: one joined row per flight.
func legacyTop(t *testing.T, a *Analyzer, from, to int64, limit int) *Top {
	t.Helper()
	type agg struct {
		e   TopEntry
		acs map[uint32]struct{}
	}
	ops, ctys, types, frames := map[string]*agg{}, map[string]*agg{}, map[string]*agg{}, map[uint32]*agg{}
	add := func(m map[string]*agg, key, label string, icao uint32, msgs int64) {
		if key == "" {
			return
		}
		g := m[key]
		if g == nil {
			g = &agg{e: TopEntry{Key: key, Label: label, MaxKm: Num(math.NaN())}, acs: map[uint32]struct{}{}}
			m[key] = g
		}
		if g.e.Label == "" {
			g.e.Label = label
		}
		g.e.Flights++
		g.e.Msgs += msgs
		g.acs[icao] = struct{}{}
	}
	res := &Top{From: from * 1000, To: to * 1000}
	err := queryEach(context.Background(), a.flightDB(), `SELECT f.icao, f.msgs, f.max_dist_km,
  COALESCE(NULLIF(a.operator, ''), c.operator, ''), COALESCE(a.country, ''),
  COALESCE(NULLIF(a.type, ''), c.type, ''), COALESCE(c."desc", ''), COALESCE(NULLIF(a.reg, ''), c.reg, '')
FROM flights f LEFT JOIN aircraft a ON a.icao = f.icao LEFT JOIN acdb c ON c.icao = f.icao
WHERE f.start >= ? AND f.start < ?`, []any{from, to}, func(rs *sql.Rows) error {
		var icao, msgs int64
		var dist sql.NullFloat64
		var op, cty, typ, desc, reg string
		if err := rs.Scan(&icao, &msgs, &dist, &op, &cty, &typ, &desc, &reg); err != nil {
			return err
		}
		ic := uint32(icao)
		res.Flights++
		add(ops, op, "", ic, msgs)
		add(ctys, cty, "", ic, msgs)
		add(types, typ, desc, ic, msgs)
		g := frames[ic]
		if g == nil {
			g = &agg{e: TopEntry{Key: store.HexICAO(ic), Label: reg, Type: typ, Operator: op, Country: cty, Aircraft: 1, MaxKm: Num(math.NaN())}}
			frames[ic] = g
		}
		g.e.Flights++
		g.e.Msgs += msgs
		if dist.Valid {
			g.e.MaxKm = Num(nanMax(float64(g.e.MaxKm), dist.Float64))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	res.Aircraft = int64(len(frames))
	list := func(m map[string]*agg) []TopEntry {
		out := make([]TopEntry, 0, len(m))
		for _, g := range m {
			g.e.Aircraft = int64(len(g.acs))
			out = append(out, g.e)
		}
		return topN(out, limit, res.Flights)
	}
	res.Operators, res.Countries, res.Types = list(ops), list(ctys), list(types)
	fl := make([]TopEntry, 0, len(frames))
	for _, g := range frames {
		fl = append(fl, g.e)
	}
	res.Airframes = topN(fl, limit, res.Flights)
	return res
}

// TestTopEqualsLegacy: the per-airframe Top equals the 1.0.0 per-flight one.
func TestTopEqualsLegacy(t *testing.T) {
	a, st := newTest(t, locatedConfig(50, 8))
	rng := rand.New(rand.NewSource(7))
	now := testNow.Unix()
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		for i := 0; i < 300; i++ {
			icao := 0x400000 + i
			if rng.Intn(3) > 0 {
				if _, err := tx.Exec(`INSERT INTO aircraft(icao, first_seen, last_seen, reg, type, operator, country) VALUES(?,?,?,?,?,?,?)`,
					icao, now-1000, now, []string{"", fmt.Sprintf("G-%03d", i)}[rng.Intn(2)], []string{"", "A320", "B738", "E190"}[rng.Intn(4)],
					[]string{"", "BAW", "EZY", "DLH"}[rng.Intn(4)], []string{"", "UK", "DE"}[rng.Intn(3)]); err != nil {
					return err
				}
			}
			if rng.Intn(2) > 0 {
				typ := []string{"", "A320", "B738", "E190"}[rng.Intn(4)]
				if _, err := tx.Exec(`INSERT INTO acdb(icao, reg, type, "desc", operator) VALUES(?,?,?,?,?)`, icao, fmt.Sprintf("R%d", i), typ,
					[]string{"", typ + " desc", typ + " other"}[rng.Intn(3)], []string{"", "RYR", "BAW"}[rng.Intn(3)]); err != nil {
					return err
				}
			}
		}
		for id := 1; id <= 4000; id++ {
			start := now - int64(rng.Intn(35*86400))
			var dist any
			if rng.Intn(5) > 0 {
				dist = rng.Float64() * 400
			}
			if _, err := tx.Exec(`INSERT INTO flights(id, icao, start, "end", msgs, max_dist_km) VALUES(?,?,?,?,?,?)`,
				id, 0x400000+rng.Intn(320), start, start+600, rng.Intn(5000), dist); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{5, 15, 200} {
		from, to := now-30*86400, now
		got, err := a.Top(context.Background(), Range{From: from, To: to}, limit)
		if err != nil {
			t.Fatal(err)
		}
		if got.Flights < 1000 || len(got.Types) == 0 {
			t.Fatalf("too few flights in range: %d", got.Flights)
		}
		if g, w := mustJSON(t, "top", got), mustJSON(t, "legacy", legacyTop(t, a, got.From/1000, got.To/1000, limit)); g != w {
			t.Fatalf("limit %d:\n got %s\nwant %s", limit, g, w)
		}
	}
}
