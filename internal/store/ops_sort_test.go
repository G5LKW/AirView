package store

import (
	"database/sql"
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
)

// Sorting a batch by key (DB-EFFICIENCY-DESIGN §3.7) must not change what
// is stored: the rows of a key are merged in submission order, so the
// result equals the unsorted 1.0.0 application. Repeated keys, the defensive
// "seconds instead of day number" form and NULL values are all included.
func TestSortedBatchesSameResult(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	day := DayOf(1_700_000_000)
	var cov []CoverageInc
	var sig []SignalHistInc
	var elev []ElevHistInc
	var heat []HeatInc
	for i := 0; i < 600; i++ {
		d := day + rng.IntN(2)
		if rng.IntN(5) == 0 {
			d = d*86400 + 100 // seconds: dayNumber maps it to the same day
		}
		ant := []string{"", "a1", "a2"}[rng.IntN(3)]
		cov = append(cov, CoverageInc{Day: d, Ant: ant, BrgBin: rng.IntN(20), AltBand: rng.IntN(3),
			MaxKm: rng.Float64() * 300, Count: 1 + rng.Int64N(5), RSSISum: -rng.Float64() * 30, RSSIN: 1})
		sig = append(sig, SignalHistInc{Day: d, Ant: ant, DistBin: rng.IntN(30), RSSIBin: -rng.IntN(40), Count: 1})
		elev = append(elev, ElevHistInc{Day: d, Ant: ant, ElevBin: rng.IntN(30), Count: 1, RSSISum: -3, ResidSum: 1.5})
		minAlt, maxAlt := math.NaN(), math.NaN()
		if rng.IntN(3) != 0 {
			minAlt, maxAlt = float64(rng.IntN(40000)), float64(rng.IntN(40000))
		}
		cell := int64(rng.IntN(64))<<16 | int64(rng.IntN(64))
		heat = append(heat, HeatInc{Cell: cell, Ant: ant, Count: 1, RSSISum: -2, RSSIN: 1, MinAlt: minAlt, MaxAlt: maxAlt,
			LastT: 1_700_000_000 + rng.Int64N(1000)})
	}
	var seen []AircraftSeen
	var flights []Flight
	for i := 0; i < 200; i++ {
		icao := uint32(1 + rng.IntN(40))
		last := 1_700_000_000 + rng.Int64N(5000)
		seen = append(seen, AircraftSeen{ICAO: icao, FirstSeen: last - rng.Int64N(500), LastSeen: last,
			Callsign: []string{"", "ABC", "XYZ"}[rng.IntN(3)], Msgs: 3, Positions: 1, MaxDistKm: rng.Float64() * 100,
			MaxAlt: float64(rng.IntN(30000)), SrcMask: 1 << rng.IntN(3)})
		f := NewFlight(int64(1+rng.IntN(30)), uint32(1+rng.IntN(3)), 1_700_000_000+rng.Int64N(100))
		f.End, f.Msgs, f.MaxGS = f.Start+rng.Int64N(1000), rng.Int64N(500), float64(rng.IntN(500))
		flights = append(flights, f)
	}

	dump := func(s *Store) string {
		t.Helper()
		var out string
		for _, q := range []string{
			`SELECT day, ant, brg_bin, alt_band, max_km, count, rssi_sum, rssi_n FROM coverage ORDER BY 1, 2, 3, 4`,
			`SELECT day, ant, dist_bin, rssi_bin, count FROM signal_hist ORDER BY 1, 2, 3, 4`,
			`SELECT day, ant, elev_bin, count, rssi_sum, resid_sum FROM elev_hist ORDER BY 1, 2, 3`,
			`SELECT cell, ant, count, rssi_sum, rssi_n, min_alt, max_alt, last_t FROM heat ORDER BY 1, 2`,
			`SELECT * FROM aircraft ORDER BY icao`,
			`SELECT * FROM flights ORDER BY id`,
		} {
			rs, err := s.DB().Query(q)
			if err != nil {
				t.Fatal(err)
			}
			cols, _ := rs.Columns()
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			for rs.Next() {
				if err := rs.Scan(ptrs...); err != nil {
					t.Fatal(err)
				}
				for _, v := range vals {
					out += sqlString(v) + ","
				}
				out += "\n"
			}
			rs.Close()
		}
		return out
	}
	// Reference: the rows applied one by one, in submission order (what the
	// 1.0.0 statements did with an unsorted batch).
	ref := openTest(t, manual)
	if err := ref.Write(bg, func(tx *sql.Tx) error {
		for _, r := range cov {
			if _, err := tx.Exec(sqlMergeCoverage, dayNumber(r.Day), r.Ant, r.BrgBin, r.AltBand, zf(r.MaxKm), r.Count, zf(r.RSSISum), r.RSSIN); err != nil {
				return err
			}
		}
		for _, r := range sig {
			if _, err := tx.Exec(sqlMergeSignalHist, dayNumber(r.Day), r.Ant, r.DistBin, r.RSSIBin, r.Count); err != nil {
				return err
			}
		}
		for _, r := range elev {
			if _, err := tx.Exec(sqlMergeElevHist, dayNumber(r.Day), r.Ant, r.ElevBin, r.Count, zf(r.RSSISum), zf(r.ResidSum)); err != nil {
				return err
			}
		}
		for _, r := range heat {
			if _, err := tx.Exec(heatMergeSQL(KindMain), r.Cell, r.Ant, r.Count, zf(r.RSSISum), r.RSSIN, ni(r.MinAlt), ni(r.MaxAlt), r.LastT); err != nil {
				return err
			}
		}
		for _, r := range seen {
			if _, err := tx.Exec(sqlUpsertAircraft, int64(r.ICAO), r.FirstSeen, r.LastSeen, r.Callsign, r.Reg, r.Type, r.Operator,
				r.Country, r.Category, r.Msgs, r.Flights, r.Positions, zf(r.MaxDistKm), ni(r.MaxAlt), r.SrcMask, r.Flags); err != nil {
				return err
			}
		}
		for _, f := range flights {
			if _, err := tx.Exec(sqlUpsertFlight, f.ID, int64(f.ICAO), f.Callsign, f.Squawk, f.Start, max(f.End, f.Start),
				f.Msgs, f.Positions, f.SrcMask, nf(f.MaxDistKm), nf(f.MaxDistBrg), nf(f.MinDistKm), ni(f.MinAlt), ni(f.MaxAlt),
				nf(f.MaxGS), nf(f.RSSIMin), nf(f.RSSIMax), nf(f.RSSIAvg), nf(f.FirstLat), nf(f.FirstLon), nf(f.LastLat),
				nf(f.LastLon), f.Emergency); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := openTest(t, manual)
	var b Batch
	b.MergeCoverage(cov)
	b.MergeSignalHist(sig)
	b.MergeElevHist(elev)
	b.MergeHeat(heat)
	b.UpsertAircraft(seen)
	b.UpsertFlights(flights)
	if err := got.Apply(bg, &b); err != nil {
		t.Fatal(err)
	}
	if a, w := dump(got), dump(ref); a != w {
		t.Fatalf("sorted batches store something else:\n%.600s\nwant\n%.600s", a, w)
	}
}

func sqlString(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return "?"
}

// The refresh path of flights and aircraft does not assign the indexed
// columns that do not change (no index page is written), yet stores exactly
// the row of the 1.0.0 upsert in every case: refresh, first insert, an icao
// / start that changed, a resolved flight (dep / arr reset), a sighting
// older than the stored one.
func TestRefreshPathsMatchUpsert(t *testing.T) {
	ref, got := openTest(t, manual), openTest(t, manual)
	apply := func(s *Store, useRef bool, fl []Flight, ac []AircraftSeen) {
		if useRef {
			if err := s.Write(bg, func(tx *sql.Tx) error {
				for _, f := range fl {
					if _, err := tx.Exec(sqlUpsertFlight, f.ID, int64(f.ICAO), f.Callsign, f.Squawk, f.Start, max(f.End, f.Start),
						f.Msgs, f.Positions, f.SrcMask, nf(f.MaxDistKm), nf(f.MaxDistBrg), nf(f.MinDistKm), ni(f.MinAlt), ni(f.MaxAlt),
						nf(f.MaxGS), nf(f.RSSIMin), nf(f.RSSIMax), nf(f.RSSIAvg), nf(f.FirstLat), nf(f.FirstLon), nf(f.LastLat),
						nf(f.LastLon), f.Emergency); err != nil {
						return err
					}
				}
				for _, r := range ac {
					first := r.FirstSeen
					if first <= 0 {
						first = r.LastSeen
					}
					if _, err := tx.Exec(sqlUpsertAircraft, int64(r.ICAO), first, r.LastSeen, r.Callsign, r.Reg, r.Type, r.Operator,
						r.Country, r.Category, r.Msgs, r.Flights, r.Positions, zf(r.MaxDistKm), ni(r.MaxAlt), r.SrcMask, r.Flags); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			return
		}
		var b Batch
		b.UpsertFlights(fl)
		b.UpsertAircraft(ac)
		if err := s.Apply(bg, &b); err != nil {
			t.Fatal(err)
		}
	}
	both := func(fl []Flight, ac []AircraftSeen) {
		apply(ref, true, fl, ac)
		apply(got, false, fl, ac)
	}
	f := NewFlight(1, 0xaa, 1000)
	f.End, f.Callsign = 1100, "AB1"
	both([]Flight{f}, []AircraftSeen{{ICAO: 0xaa, LastSeen: 1100, MaxAlt: math.NaN(), Msgs: 5}})
	f.End, f.Msgs, f.MaxGS = 1200, 40, 300
	both([]Flight{f}, []AircraftSeen{{ICAO: 0xaa, FirstSeen: 1150, LastSeen: 1200, Callsign: "AB1", MaxAlt: 30000, Msgs: 7}})
	// Resolved, then continued: dep / arr reset.
	for _, s := range []*Store{ref, got} {
		if err := s.SetFlightAirports(bg, []FlightAirports{{ID: 1, Dep: "EDDF", Arr: "EGLL"}}); err != nil {
			t.Fatal(err)
		}
	}
	f.End = 1300
	// Older sighting (last_seen does not move), earlier first_seen.
	both([]Flight{f}, []AircraftSeen{{ICAO: 0xaa, FirstSeen: 900, LastSeen: 1250, MaxAlt: math.NaN(), Msgs: 1}})
	// The same id with another start / icao (never by the tracker, but
	// the upsert semantic is kept).
	g := f
	g.Start, g.ICAO = 990, 0xab
	both([]Flight{g}, []AircraftSeen{{ICAO: 0xab, LastSeen: 1300, MaxAlt: math.NaN()}})
	dump := func(s *Store) string {
		var out string
		for _, q := range []string{`SELECT * FROM flights ORDER BY id`, `SELECT * FROM aircraft ORDER BY icao`} {
			rs, err := s.DB().Query(q)
			if err != nil {
				t.Fatal(err)
			}
			cols, _ := rs.Columns()
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			for rs.Next() {
				_ = rs.Scan(ptrs...)
				for _, v := range vals {
					out += sqlString(v) + ","
				}
				out += "\n"
			}
			rs.Close()
		}
		return out
	}
	if a, w := dump(got), dump(ref); a != w {
		t.Fatalf("refresh path differs:\n%s\nwant\n%s", a, w)
	}
}
