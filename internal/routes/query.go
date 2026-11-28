package routes

// Read-side queries for the product surface (docs/ROUTES-DESIGN.md §6.5, §6.6,
// §7): airport boards built from the live picture, airport references for
// client-side drawing, leg search and leg statistics. Nothing here sends an
// online request; online (restricted) answers are never stored in routes_leg,
// so leg queries only ever see open / own classes.

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
)

// AirportRef is an airport for drawing and aliasing (IATA <-> ICAO).
type AirportRef struct {
	Code   string   `json:"code"` // ICAO (else ident)
	IATA   string   `json:"iata,omitempty"`
	Name   string   `json:"name,omitempty"`
	Lat    float64  `json:"lat"`
	Lon    float64  `json:"lon"`
	ElevFt *float64 `json:"elevFt,omitempty"`
	Size   int      `json:"size,omitempty"`
}

func refOf(st Stop) AirportRef {
	r := AirportRef{Code: st.Code, IATA: st.IATA, Name: st.Name, Lat: st.Lat, Lon: st.Lon, Size: st.Size}
	if !math.IsNaN(st.ElevFt) {
		e := st.ElevFt
		r.ElevFt = &e
	}
	return r
}

// MaxAirportCodes bounds one Airports request.
const MaxAirportCodes = 200

// Airports resolves airport codes (ICAO, IATA or ident; duplicates and
// unknown codes are dropped) for client-side drawing and the alias map.
func (s *Service) Airports(ctx context.Context, codes []string) ([]AirportRef, error) {
	seen := map[string]bool{}
	out := []AirportRef{}
	for _, c := range codes {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" || len(c) > 8 || seen[c] {
			continue
		}
		seen[c] = true
		if len(seen) > MaxAirportCodes {
			break
		}
		st, err := s.stops(ctx, []string{c})
		if err != nil {
			return nil, err
		}
		if len(st) == 1 && st[0].Known && !seen["="+st[0].Code] {
			seen["="+st[0].Code] = true
			out = append(out, refOf(st[0]))
		}
	}
	return out, nil
}

// BoardRow is one aircraft on an airport board.
type BoardRow struct {
	ICAO     string   `json:"icao"`
	Callsign string   `json:"callsign"`
	Type     string   `json:"type,omitempty"`
	Operator string   `json:"operator,omitempty"`
	From     string   `json:"from,omitempty"`
	FromIATA string   `json:"fromIata,omitempty"`
	FromName string   `json:"fromName,omitempty"`
	To       string   `json:"to,omitempty"`
	ToIATA   string   `json:"toIata,omitempty"`
	ToName   string   `json:"toName,omitempty"`
	State    string   `json:"state,omitempty"`
	Conf     float64  `json:"conf,omitempty"`
	Source   string   `json:"source,omitempty"`
	ETA      int64    `json:"eta,omitempty"` // Unix s
	ETASigma int      `json:"etaSigma,omitempty"`
	DistKm   float64  `json:"distKm,omitempty"` // to (arrivals) / from (departures) the airport
	AltFt    *float64 `json:"altFt,omitempty"`
	VertRate *float64 `json:"vertRate,omitempty"`
	T        int64    `json:"t,omitempty"` // landed: Unix s of the end of the leg
}

// Board is the arrivals / departures / recently-landed board of an airport,
// built from the live picture only (no schedules).
type Board struct {
	Airport    AirportRef `json:"airport"`
	Arrivals   []BoardRow `json:"arrivals"`
	Departures []BoardRow `json:"departures"`
	Landed     []BoardRow `json:"landed"`
	Generated  int64      `json:"generated"` // Unix ms
	Note       string     `json:"note"`
}

// Board limits (ROUTES-DESIGN §6.5).
const (
	BoardMinConf     = 0.5
	BoardDepRadiusKm = 300
	BoardWindow      = 60 * time.Minute
)

// BoardNote says what a board is (and is not).
const BoardNote = "Built from the aircraft this station receives now and the routes it resolved for them; not a schedule. Arrivals are aircraft whose route ends here (confidence >= 50 %), departures left here and are still climbing within 300 km, landed are flights this station saw end here in the last 60 minutes."

func matchCode(ap AirportRef, code, iata string) bool {
	return code != "" && (strings.EqualFold(code, ap.Code) || (ap.IATA != "" && strings.EqualFold(code, ap.IATA))) ||
		iata != "" && ap.IATA != "" && strings.EqualFold(iata, ap.IATA)
}

// BuildBoard builds a board from live aircraft (already projected for the
// caller's scope) and the legs that ended at the airport recently. Pure
// function: the caller supplies the airport and the legs.
func BuildBoard(ap AirportRef, aircraft []model.Aircraft, landed []Leg, now time.Time) Board {
	b := Board{Airport: ap, Arrivals: []BoardRow{}, Departures: []BoardRow{}, Landed: []BoardRow{}, Generated: now.UnixMilli(), Note: BoardNote}
	for i := range aircraft {
		a := &aircraft[i]
		r := a.Route
		if r == nil || r.Conf < BoardMinConf || r.State == "reversed" {
			continue
		}
		row := BoardRow{ICAO: a.ICAO, Callsign: a.Callsign, Type: a.TypeCode, Operator: a.Operator,
			From: r.From, FromIATA: r.FromIATA, FromName: r.FromName, To: r.To, ToIATA: r.ToIATA, ToName: r.ToName,
			State: r.State, Conf: r.Conf, Source: r.Source, ETA: r.ETA, ETASigma: r.ETASigma, AltFt: a.AltBaro, VertRate: a.VertRate}
		dist := math.NaN()
		if a.Lat != nil && a.Lon != nil {
			dist = geo.HaversineKm(*a.Lat, *a.Lon, ap.Lat, ap.Lon)
			row.DistKm = math.Round(dist*10) / 10
		}
		switch {
		case matchCode(ap, r.To, r.ToIATA):
			b.Arrivals = append(b.Arrivals, row)
		case matchCode(ap, r.From, r.FromIATA):
			onGround := a.OnGround != nil && *a.OnGround
			climbing := a.VertRate != nil && *a.VertRate > 200
			if !onGround && climbing && !math.IsNaN(dist) && dist <= BoardDepRadiusKm {
				b.Departures = append(b.Departures, row)
			}
		}
	}
	sort.SliceStable(b.Arrivals, func(i, j int) bool {
		ei, ej := b.Arrivals[i].ETA, b.Arrivals[j].ETA
		if (ei == 0) != (ej == 0) {
			return ej == 0
		}
		if ei != ej {
			return ei < ej
		}
		return b.Arrivals[i].DistKm < b.Arrivals[j].DistKm
	})
	sort.SliceStable(b.Departures, func(i, j int) bool { return b.Departures[i].DistKm < b.Departures[j].DistKm })
	cut := now.Add(-BoardWindow).Unix()
	for _, l := range landed {
		if l.End < cut || l.ArrConf < BoardMinConf || !matchCode(ap, l.Arr, "") {
			continue
		}
		b.Landed = append(b.Landed, BoardRow{Callsign: l.Callsign, From: l.Dep, To: l.Arr, T: l.End, Conf: math.Round(l.ArrConf*100) / 100})
	}
	sort.SliceStable(b.Landed, func(i, j int) bool { return b.Landed[i].T > b.Landed[j].T })
	return b
}

// Airport resolves one airport code; ok is false when it is unknown.
func (s *Service) Airport(ctx context.Context, code string) (AirportRef, bool, error) {
	st, err := s.stops(ctx, []string{code})
	if err != nil || len(st) != 1 || !st[0].Known {
		return AirportRef{}, false, err
	}
	return refOf(st[0]), true, nil
}

// LegQuery selects legs (routes_leg). Empty fields do not filter; airport
// fields match the stored codes (ICAO, else what the airport guess used).
type LegQuery struct {
	From, To int64  // start time range, Unix s (To 0 = now)
	Callsign string // exact, upper case
	Airline  string // 3-letter ICAO prefix of the callsign
	Dep, Arr string // observed departure / arrival airport
	Route    string // shown route at close, e.g. "EGLL-KJFK"
	Limit    int    // 1..5000 (default 500)
}

// Legs returns legs newest first.
func (s *Service) Legs(ctx context.Context, q LegQuery) ([]Leg, error) {
	out := []Leg{}
	if s.o.Store == nil {
		return out, nil
	}
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 500
	}
	if q.To <= 0 {
		q.To = math.MaxInt64
	}
	where := []string{"start >= ?", "start < ?"}
	args := []any{q.From, q.To}
	add := func(cond, v string) {
		if v = strings.ToUpper(strings.TrimSpace(v)); v != "" {
			where = append(where, cond)
			args = append(args, v)
		}
	}
	add("callsign = ?", q.Callsign)
	if a := strings.ToUpper(strings.TrimSpace(q.Airline)); len(a) == 3 {
		where = append(where, "substr(callsign, 1, 3) = ?")
		args = append(args, a)
	}
	add("dep = ?", q.Dep)
	add("arr = ?", q.Arr)
	add("route = ?", q.Route)
	args = append(args, q.Limit)
	rows, err := s.o.Store.DB().QueryContext(ctx, `SELECT flight, start, "end", callsign, dep, dep_conf, dep_tier, arr, arr_conf, arr_tier, route, route_src
  FROM routes_leg WHERE `+strings.Join(where, " AND ")+` ORDER BY start DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l Leg
		if err := rows.Scan(&l.Flight, &l.Start, &l.End, &l.Callsign, &l.Dep, &l.DepConf, &l.DepTier, &l.Arr, &l.ArrConf, &l.ArrTier, &l.Route, &l.RouteSrc); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LandedAt returns the legs that ended at an airport (any of codes) since t.
func (s *Service) LandedAt(ctx context.Context, codes []string, since int64) ([]Leg, error) {
	out := []Leg{}
	if s.o.Store == nil || len(codes) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(codes)), ",")
	args := []any{since - int64(12*time.Hour/time.Second), since}
	for _, c := range codes {
		args = append(args, strings.ToUpper(c))
	}
	rows, err := s.o.Store.DB().QueryContext(ctx, `SELECT flight, start, "end", callsign, dep, dep_conf, dep_tier, arr, arr_conf, arr_tier, route, route_src
  FROM routes_leg WHERE start >= ? AND "end" >= ? AND arr IN (`+ph+`) ORDER BY "end" DESC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l Leg
		if err := rows.Scan(&l.Flight, &l.Start, &l.End, &l.Callsign, &l.Dep, &l.DepConf, &l.DepTier, &l.Arr, &l.ArrConf, &l.ArrTier, &l.Route, &l.RouteSrc); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Leg statistics kinds (GET /api/routes/stats?kind=).
const (
	StatRoutes   = "routes"   // observed dep-arr pairs
	StatAirports = "airports" // airports, departures + arrivals
	StatAirlines = "airlines" // 3-letter callsign prefixes
	StatCoverage = "coverage" // share of legs with a shown route, by source; both ends observed
)

// StatRow is one row of a leg statistic.
type StatRow struct {
	Key  string  `json:"key"`
	N    int     `json:"n"`
	Dep  int     `json:"dep,omitempty"`  // airports: departures
	Arr  int     `json:"arr,omitempty"`  // airports: arrivals
	Pct  float64 `json:"pct,omitempty"`  // coverage: share of all legs, %
	Note string  `json:"note,omitempty"` // coverage: what the row counts
}

// LegStats counts observed legs that started in [from, to). Only ends
// observed with confidence >= 0.5 count for routes and airports.
func (s *Service) LegStats(ctx context.Context, kind string, from, to int64, limit int) ([]StatRow, error) {
	out := []StatRow{}
	if s.o.Store == nil {
		return out, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	if to <= 0 {
		to = math.MaxInt64
	}
	db := s.o.Store.DB()
	scan := func(q string, args ...any) error {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r StatRow
			if kind == StatAirports {
				if err := rows.Scan(&r.Key, &r.Dep, &r.Arr); err != nil {
					return err
				}
				r.N = r.Dep + r.Arr
			} else if err := rows.Scan(&r.Key, &r.N); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}
	switch kind {
	case StatRoutes:
		return out, scan(`SELECT dep || '-' || arr, COUNT(*) n FROM routes_leg
  WHERE start >= ? AND start < ? AND dep <> '' AND arr <> '' AND dep_conf >= 0.5 AND arr_conf >= 0.5
  GROUP BY dep, arr ORDER BY n DESC, 1 LIMIT ?`, from, to, limit)
	case StatAirports:
		return out, scan(`SELECT code, SUM(d), SUM(a) FROM (
    SELECT dep code, 1 d, 0 a FROM routes_leg WHERE start >= ? AND start < ? AND dep <> '' AND dep_conf >= 0.5
    UNION ALL SELECT arr, 0, 1 FROM routes_leg WHERE start >= ? AND start < ? AND arr <> '' AND arr_conf >= 0.5)
  GROUP BY code ORDER BY SUM(d) + SUM(a) DESC, code LIMIT ?`, from, to, from, to, limit)
	case StatAirlines:
		return out, scan(`SELECT substr(callsign, 1, 3) al, COUNT(*) n FROM routes_leg
  WHERE start >= ? AND start < ? AND length(callsign) >= 4 GROUP BY al ORDER BY n DESC, al LIMIT ?`, from, to, limit)
	case StatCoverage:
		var total, both int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(dep_tier IN ('T1','T2') AND arr_tier IN ('L1','L2')), 0)
  FROM routes_leg WHERE start >= ? AND start < ?`, from, to).Scan(&total, &both); err != nil {
			return nil, err
		}
		if err := scan(`SELECT CASE WHEN route = '' THEN 'none' ELSE route_src END k, COUNT(*) n FROM routes_leg
  WHERE start >= ? AND start < ? GROUP BY k ORDER BY n DESC`, from, to); err != nil {
			return nil, err
		}
		for i := range out {
			out[i].Note = "legs by the source of the route shown when the flight ended (none: no route shown)"
		}
		out = append(out, StatRow{Key: "both-ends-observed", N: both, Note: "take-off (T1/T2) and landing (L1/L2) both observed"})
		out = append(out, StatRow{Key: "total", N: total})
		if total > 0 {
			for i := range out {
				out[i].Pct = math.Round(float64(out[i].N)*1000/float64(total)) / 10
			}
		}
		return out, nil
	}
	return nil, ErrNotFound
}
