package routes

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strings"
	"time"
)

// Learning from observed legs (docs/ROUTES-DESIGN.md §3.6). Only AirView's
// own observations go in: never an online (restricted) answer.

// Event tiers as bits of routes_obs.events.
const (
	EvT1     = 1 << iota // take-off from the surface on a runway
	EvT2                 // first fix low, climbing, aligned with a runway
	EvT3                 // first fix low near the airport
	EvL1                 // on the surface after landing
	EvL2                 // last fix low, aligned with a runway
	EvL5                 // last fix low near the airport
	evStrong = EvT1 | EvT2 | EvL1 | EvL2
)

// Learning thresholds.
const (
	learnMinConf     = 0.7
	learnAlpha       = 2.0
	learnPiStanding  = 0.8
	promoteMinDays   = 2
	promoteMinP      = 0.8
	promoteMaxAge    = 14 * 24 * time.Hour
	staleMinDays     = 3
	staleMinP        = 0.85
	obsKeepDays      = 120
	decayDays        = 21.0
	seasonDownWeight = 0.3
)

// Leg is one observed flight leg handed to the learner when a flight closes.
type Leg struct {
	Flight   int64   `json:"flight"`
	Callsign string  `json:"callsign"`
	Start    int64   `json:"start"` // Unix s
	End      int64   `json:"end"`
	Dep      string  `json:"dep,omitempty"`
	DepConf  float64 `json:"depConf,omitempty"`
	DepTier  string  `json:"depTier,omitempty"` // T1 | T2 | T3
	Arr      string  `json:"arr,omitempty"`
	ArrConf  float64 `json:"arrConf,omitempty"`
	ArrTier  string  `json:"arrTier,omitempty"` // L1 | L2 | L5
	Route    string  `json:"route,omitempty"`   // route shown at close (open/own classes only)
	RouteSrc string  `json:"routeSrc,omitempty"`
}

func tierBit(t string) int {
	switch t {
	case "T1":
		return EvT1
	case "T2":
		return EvT2
	case "T3":
		return EvT3
	case "L1":
		return EvL1
	case "L2":
		return EvL2
	case "L5":
		return EvL5
	}
	return 0
}

// lastSeasonChange returns the start (00:00 UTC) of the last IATA season
// change (last Sunday of March or October) at or before t.
func lastSeasonChange(t time.Time) time.Time {
	t = t.UTC()
	lastSunday := func(y int, m time.Month) time.Time {
		d := time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
		for d.Weekday() != time.Sunday {
			d = d.AddDate(0, 0, -1)
		}
		return d
	}
	for y := t.Year(); y >= t.Year()-1; y-- {
		for _, m := range []time.Month{time.October, time.March} {
			if d := lastSunday(y, m); !d.After(t) {
				return d
			}
		}
	}
	return time.Time{}
}

// recordLeg writes the leg row and, when the leg is good enough, the
// observation. Returns whether an observation was added.
func recordLeg(ctx context.Context, tx *sql.Tx, l Leg) (bool, error) {
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO routes_leg(flight, seq, start, "end", callsign, dep, dep_conf, dep_tier, arr, arr_conf, arr_tier, route, route_src)
  VALUES(?,0,?,?,?,?,?,?,?,?,?,?,?)`, l.Flight, l.Start, l.End, l.Callsign, l.Dep, round(l.DepConf, 2), l.DepTier,
		l.Arr, round(l.ArrConf, 2), l.ArrTier, l.Route, l.RouteSrc); err != nil {
		return false, err
	}
	dep, arr := l.Dep, l.Arr
	if l.DepConf < learnMinConf {
		dep = ""
	}
	if l.ArrConf < learnMinConf {
		arr = ""
	}
	if l.Callsign == "" || (dep == "" && arr == "") || (dep != "" && dep == arr) {
		return false, nil
	}
	ev := 0
	if dep != "" {
		ev |= tierBit(l.DepTier)
	}
	if arr != "" {
		ev |= tierBit(l.ArrTier)
	}
	w := 1.0
	if dep != "" {
		w *= l.DepConf
	}
	if arr != "" {
		w *= l.ArrConf
	}
	// departure time of day as a unit vector (circular mean over rows)
	tod := float64(l.Start%86400) / 86400 * 2 * math.Pi
	_, err := tx.ExecContext(ctx, `INSERT INTO routes_obs(callsign, dep, arr, day, n, w, tod_sin, tod_cos, last_t, events)
  VALUES(?,?,?,?,1,?,?,?,?,?)
  ON CONFLICT(callsign, dep, arr, day) DO UPDATE SET n = n + 1, w = w + excluded.w,
    tod_sin = tod_sin + excluded.tod_sin, tod_cos = tod_cos + excluded.tod_cos,
    last_t = MAX(last_t, excluded.last_t), events = events | excluded.events`,
		l.Callsign, dep, arr, l.Start/86400, w, math.Sin(tod), math.Cos(tod), max(l.End, l.Start), ev)
	return err == nil, err
}

// Learned is a promoted route of a callsign.
type Learned struct {
	Callsign      string  `json:"callsign"`
	Rank          int     `json:"rank"`
	Airports      string  `json:"airports"`
	P             float64 `json:"p"`
	Days          int     `json:"days"`
	LastT         int64   `json:"lastT"`
	TodMin        int     `json:"todMin"` // typical departure time of day, minutes UTC
	StaleStanding bool    `json:"staleStanding,omitempty"`
}

type obsAgg struct {
	key    string
	n      float64 // weighted count
	days   map[int64]bool
	last   int64
	ev     int
	ts, tc float64
}

// rebuild recomputes the learned routes of one callsign from its
// observations; standing is the standing-data route ("" when none).
func rebuild(ctx context.Context, tx *sql.Tx, cs, standing string, now time.Time) ([]Learned, error) {
	rows, err := tx.QueryContext(ctx, `SELECT dep, arr, day, w, tod_sin, tod_cos, last_t, events FROM routes_obs
  WHERE callsign = ? AND day >= ? AND dep <> '' AND arr <> ''`, cs, now.Unix()/86400-obsKeepDays)
	if err != nil {
		return nil, err
	}
	season := lastSeasonChange(now).Unix() / 86400
	aggs := map[string]*obsAgg{}
	for rows.Next() {
		var dep, arr string
		var day, last int64
		var w, ts, tc float64
		var ev int
		if err := rows.Scan(&dep, &arr, &day, &w, &ts, &tc, &last, &ev); err != nil {
			rows.Close()
			return nil, err
		}
		k := dep + "-" + arr
		a := aggs[k]
		if a == nil {
			a = &obsAgg{key: k, days: map[int64]bool{}}
			aggs[k] = a
		}
		age := float64(now.Unix()/86400 - day)
		wt := w * math.Exp(-math.Max(0, age)/decayDays)
		if day < season {
			wt *= seasonDownWeight
		}
		a.n += wt
		a.days[day] = true
		a.last = max(a.last, last)
		a.ev |= ev
		a.ts += ts
		a.tc += tc
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Dirichlet posterior with the standing route as the prior.
	inStanding := func(k string) bool {
		if standing == "" {
			return false
		}
		cs := strings.Split(standing, "-")
		for i := 0; i+1 < len(cs); i++ {
			if cs[i]+"-"+cs[i+1] == k {
				return true
			}
		}
		return false
	}
	total, others := 0.0, 0
	for k, a := range aggs {
		total += a.n
		if !inStanding(k) {
			others++
		}
	}
	var out []Learned
	keys := make([]string, 0, len(aggs))
	for k := range aggs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := aggs[k]
		pi := 0.0
		switch {
		case standing == "":
			pi = 1 / float64(len(aggs))
		case inStanding(k):
			pi = learnPiStanding
		case others > 0:
			pi = (1 - learnPiStanding) / float64(others)
		}
		p := (a.n + learnAlpha*pi) / (total + learnAlpha)
		if len(a.days) < promoteMinDays || p < promoteMinP || now.Sub(time.Unix(a.last, 0)) > promoteMaxAge {
			continue
		}
		tod := math.Atan2(a.ts, a.tc)
		if tod < 0 {
			tod += 2 * math.Pi
		}
		l := Learned{Callsign: cs, Airports: k, P: round(p, 3), Days: len(a.days), LastT: a.last,
			TodMin: int(tod / (2 * math.Pi) * 1440)}
		l.StaleStanding = standing != "" && !inStanding(k) && len(a.days) >= staleMinDays && p >= staleMinP && a.ev&evStrong != 0
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].P > out[j].P })
	if len(out) > 3 {
		out = out[:3]
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM routes_learned WHERE callsign = ?`, cs); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Rank = i
		l := out[i]
		if _, err := tx.ExecContext(ctx, `INSERT INTO routes_learned(callsign, rank, airports, p, n_days, last_t, tod_min, stale_standing)
  VALUES(?,?,?,?,?,?,?,?)`, cs, l.Rank, l.Airports, l.P, l.Days, l.LastT, l.TodMin, b2i(l.StaleStanding)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pruneObs deletes observations older than obsKeepDays and learned routes
// not seen for that long.
func pruneObs(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM routes_obs WHERE day < ?`, now.Unix()/86400-obsKeepDays)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	_, err = tx.ExecContext(ctx, `DELETE FROM routes_learned WHERE last_t < ?`, now.Unix()-obsKeepDays*86400)
	return n, err
}

// learnedFor reads the promoted routes of a callsign.
func learnedFor(ctx context.Context, db *sql.DB, cs string) ([]Learned, error) {
	rows, err := db.QueryContext(ctx, `SELECT rank, airports, p, n_days, last_t, tod_min, stale_standing FROM routes_learned WHERE callsign = ? ORDER BY rank`, cs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Learned
	for rows.Next() {
		l := Learned{Callsign: cs}
		var stale int
		if err := rows.Scan(&l.Rank, &l.Airports, &l.P, &l.Days, &l.LastT, &l.TodMin, &stale); err != nil {
			return nil, err
		}
		l.StaleStanding = stale != 0
		out = append(out, l)
	}
	return out, rows.Err()
}
