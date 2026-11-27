package analytics

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strings"
	"time"

	"airview/internal/geo"
	"airview/internal/stat"
	"airview/internal/store"
)

// ---------------------------------------------------------------- loading

// hourStats returns hourly statistics for [from, to): the rolled-up
// stats_hour rows plus, for the hours not rolled up yet, an aggregation of
// stats_minute.
func (a *Analyzer) hourStats(ctx context.Context, ant string, from, to int64) ([]store.StatsRow, error) {
	rows, err := a.st.HourStats(ctx, a.antKey(ant), from, to-1)
	if err != nil {
		return nil, err
	}
	// The hourly rollup also writes the newest (still open) hour, so the
	// last hour row may be partial (UX-02: "today" and "24 h" froze until
	// the next rollup). Re-aggregate it from the minute rows whenever they
	// still exist; an hour whose minutes are pruned keeps its row.
	tail := from
	if n := len(rows); n > 0 {
		tail = rows[n-1].T
	}
	if tail >= to {
		return rows, nil
	}
	mins, err := a.st.MinuteStats(ctx, a.antKey(ant), tail, to-1)
	if err != nil {
		return nil, err
	}
	if n := len(rows); n > 0 && len(mins) > 0 && mins[0].T < rows[n-1].T+3600 {
		rows = rows[:n-1]
	}
	return append(rows, aggregateHours(mins)...), nil
}

// aggregateHours rolls minute rows (sorted by time) up to hours the same
// way store.RollupHours does.
func aggregateHours(mins []store.StatsRow) []store.StatsRow {
	var out []store.StatsRow
	var cur *store.StatsRow
	var rssiW, rssiSum, acSum float64
	flush := func() {
		if cur == nil {
			return
		}
		if rssiW > 0 {
			cur.RSSIAvg = rssiSum / rssiW
		}
		if cur.Minutes > 0 {
			cur.AircraftAvg = acSum / float64(cur.Minutes)
		}
		out = append(out, *cur)
		cur = nil
	}
	for i := range mins {
		m := &mins[i]
		h := floorDiv(m.T, 3600) * 3600
		if cur == nil || cur.T != h {
			flush()
			nan := math.NaN()
			cur = &store.StatsRow{T: h, Ant: m.Ant, MaxKm: nan, RSSIAvg: nan, RSSIMin: nan, RSSIMax: nan, AircraftAvg: nan}
			rssiW, rssiSum, acSum = 0, 0, 0
		}
		cur.Frames += m.Frames
		cur.DF0 += m.DF0
		cur.DF4 += m.DF4
		cur.DF5 += m.DF5
		cur.DF11 += m.DF11
		cur.DF16 += m.DF16
		cur.DF17 += m.DF17
		cur.DF18 += m.DF18
		cur.DF20 += m.DF20
		cur.DF21 += m.DF21
		cur.DFOther += m.DFOther
		cur.CRCBad += m.CRCBad
		cur.Positions += m.Positions
		cur.MLATPositions += m.MLATPositions
		cur.Aircraft = max(cur.Aircraft, m.Aircraft)
		cur.AircraftPos = max(cur.AircraftPos, m.AircraftPos)
		cur.ADSB = max(cur.ADSB, m.ADSB)
		cur.MLAT = max(cur.MLAT, m.MLAT)
		cur.ModeS = max(cur.ModeS, m.ModeS)
		cur.MaxKm = nanMax(cur.MaxKm, m.MaxKm)
		cur.RSSIMax = nanMax(cur.RSSIMax, m.RSSIMax)
		cur.RSSIMin = nanMin(cur.RSSIMin, m.RSSIMin)
		if isNum(m.RSSIAvg) {
			w := float64(max(m.Frames, 1))
			rssiSum += m.RSSIAvg * w
			rssiW += w
		}
		acSum += float64(m.Aircraft)
		cur.Minutes++
		cur.Secs += m.CoveredSec(false)
	}
	flush()
	return out
}

func nanMax(a, b float64) float64 {
	if !isNum(b) {
		return a
	}
	if !isNum(a) || b > a {
		return b
	}
	return a
}

func nanMin(a, b float64) float64 {
	if !isNum(b) {
		return a
	}
	if !isNum(a) || b < a {
		return b
	}
	return a
}

// ---------------------------------------------------------------- traffic

// TimeSeries is a columnar time series (all slices have the length of T).
type TimeSeries struct {
	T               []int64 `json:"t"`               // interval start, Unix ms
	FramesPerSec    Nums    `json:"framesPerSec"`    // message rate
	PositionsPerSec Nums    `json:"positionsPerSec"` // position rate
	Aircraft        []int   `json:"aircraft"`        // distinct aircraft heard (hour: peak minute)
	AircraftPos     []int   `json:"aircraftPos"`     // with a position
	ADSB            []int   `json:"adsb"`
	MLAT            []int   `json:"mlat"`
	ModeS           []int   `json:"modes"`
	MaxKm           Nums    `json:"maxKm"`
	RSSIAvg         Nums    `json:"rssiAvg"`
}

// DFCount is the share of one downlink format.
type DFCount struct {
	DF    string `json:"df"` // "DF17", ..., "other"
	Name  string `json:"name"`
	Count int64  `json:"count"`
	Pct   Num    `json:"pct"`
}

// SourceShare splits traffic into ADS-B / MLAT / Mode-S-only.
type SourceShare struct {
	// Aircraft shares, weighted by time on air (sum of the per-interval
	// aircraft gauges).
	ADSBPct  Num `json:"adsbPct"`
	MLATPct  Num `json:"mlatPct"`
	ModeSPct Num `json:"modesPct"`
	// Position counts.
	ADSBPositions int64 `json:"adsbPositions"`
	MLATPositions int64 `json:"mlatPositions"`
}

// TrafficTotals summarises a range.
type TrafficTotals struct {
	Frames          int64 `json:"frames"`
	Positions       int64 `json:"positions"`
	MLATPositions   int64 `json:"mlatPositions"`
	CRCBad          int64 `json:"crcBad"`
	DecodePct       Num   `json:"decodePct"`       // frames that passed CRC, %
	FramesPerSec    Num   `json:"framesPerSec"`    // mean over the time the receiver was up
	PeakAircraft    int   `json:"peakAircraft"`    //
	PeakAircraftT   int64 `json:"peakAircraftT"`   // Unix ms, 0 = none
	MaxKm           Num   `json:"maxKm"`           //
	RSSIAvg         Num   `json:"rssiAvg"`         // frame-weighted
	UptimePct       Num   `json:"uptimePct"`       // minutes with data / minutes in range
	UniqueAircraft  int64 `json:"uniqueAircraft"`  // distinct airframes with a flight in range
	Flights         int64 `json:"flights"`         //
	MinutesWithData int64 `json:"minutesWithData"` //
}

// Traffic is the result of Analyzer.Traffic.
type Traffic struct {
	From       int64         `json:"from"`
	To         int64         `json:"to"`
	Ant        string        `json:"ant"`
	Resolution string        `json:"resolution"` // "minute" | "hour"
	StepSec    int           `json:"stepSec"`
	Series     TimeSeries    `json:"series"`
	Totals     TrafficTotals `json:"totals"`
	DF         []DFCount     `json:"df"`
	Sources    SourceShare   `json:"sources"`
}

// minuteResolutionMaxSpan is the longest range served at minute resolution.
const minuteResolutionMaxSpan = 12 * 3600

// Traffic returns message / aircraft / position rates over time (minute
// resolution up to 12 h, hourly beyond), totals, the per-DF breakdown and
// the ADS-B / MLAT / Mode-S-only split. Default range: the last 24 h.
func (a *Analyzer) Traffic(ctx context.Context, r Range, ant string) (*Traffic, error) {
	now, _ := a.clock()
	span := r.Span
	if r.From > 0 {
		to := r.To
		if to <= 0 {
			to = now.Unix()
		}
		span = to - r.From
	}
	if span <= 0 {
		span = 86400
	}
	minute := span <= minuteResolutionMaxSpan
	gran, ttl := int64(3600), ttlTraffic
	if minute {
		gran, ttl = 60, ttlLive
	}
	from, to := r.resolve(now.Unix(), 86400, gran)
	cfg := a.cfg()
	return cached(ctx, a, a.key("traffic", ant, cfg, from, to), ttl, func(ctx context.Context) (*Traffic, error) {
		var rows []store.StatsRow
		var err error
		if minute {
			rows, err = a.st.MinuteStats(ctx, a.antKey(ant), from, to-1)
		} else {
			rows, err = a.hourStats(ctx, ant, from, to)
		}
		if err != nil {
			return nil, err
		}
		res := &Traffic{From: from * 1000, To: to * 1000, Ant: ant, Resolution: "hour", StepSec: 3600}
		if minute {
			res.Resolution, res.StepSec = "minute", 60
		}
		res.Series, res.Totals, res.DF, res.Sources = summarise(rows, minute, from, to, now.Unix())
		src, sargs := a.flightsSrc()
		err = a.flightDB().QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT icao), COUNT(*) FROM `+src+` fl WHERE "end" >= ? AND start < ?`, sqlArgs(sargs, []any{from, to})...).
			Scan(&res.Totals.UniqueAircraft, &res.Totals.Flights)
		if err != nil {
			return nil, err
		}
		return res, nil
	})
}

var dfNames = [...][2]string{
	{"DF0", "Short air-air surveillance"}, {"DF4", "Altitude reply"}, {"DF5", "Identity reply"},
	{"DF11", "All-call reply"}, {"DF16", "Long air-air surveillance"}, {"DF17", "ADS-B extended squitter"},
	{"DF18", "TIS-B / ADS-R"}, {"DF20", "Comm-B altitude"}, {"DF21", "Comm-B identity"}, {"other", "Other / Mode A/C"},
}

func summarise(rows []store.StatsRow, minute bool, from, to, now int64) (TimeSeries, TrafficTotals, []DFCount, SourceShare) {
	n := len(rows)
	ts := TimeSeries{
		T: make([]int64, n), FramesPerSec: make(Nums, n), PositionsPerSec: make(Nums, n),
		Aircraft: make([]int, n), AircraftPos: make([]int, n), ADSB: make([]int, n), MLAT: make([]int, n),
		ModeS: make([]int, n), MaxKm: make(Nums, n), RSSIAvg: make(Nums, n),
	}
	tot := TrafficTotals{MaxKm: Num(math.NaN()), RSSIAvg: Num(math.NaN()), DecodePct: Num(math.NaN()), FramesPerSec: Num(math.NaN())}
	var df [10]int64
	var rssiSum, rssiW, adsb, mlat, modes, totSecs float64
	maxKm := math.NaN()
	for i := range rows {
		r := &rows[i]
		// The time base is the covered part of the interval (a partial
		// first / last minute after a start or before a stop), not a full
		// 60 s per minute.
		secs := r.CoveredSec(!minute)
		mins := int64(1)
		if !minute {
			mins = int64(r.Minutes)
			if mins <= 0 {
				mins = 60
			}
		}
		totSecs += secs
		ts.T[i] = r.T * 1000
		ts.FramesPerSec[i] = float64(r.Frames) / secs
		ts.PositionsPerSec[i] = float64(r.Positions) / secs
		ts.Aircraft[i], ts.AircraftPos[i] = r.Aircraft, r.AircraftPos
		ts.ADSB[i], ts.MLAT[i], ts.ModeS[i] = r.ADSB, r.MLAT, r.ModeS
		ts.MaxKm[i], ts.RSSIAvg[i] = r.MaxKm, r.RSSIAvg

		tot.Frames += r.Frames
		tot.Positions += r.Positions
		tot.MLATPositions += r.MLATPositions
		tot.CRCBad += r.CRCBad
		tot.MinutesWithData += mins
		if r.Aircraft > tot.PeakAircraft {
			tot.PeakAircraft, tot.PeakAircraftT = r.Aircraft, r.T*1000
		}
		maxKm = nanMax(maxKm, r.MaxKm)
		if isNum(r.RSSIAvg) {
			w := float64(max(r.Frames, 1))
			rssiSum += r.RSSIAvg * w
			rssiW += w
		}
		for j, v := range [...]int64{r.DF0, r.DF4, r.DF5, r.DF11, r.DF16, r.DF17, r.DF18, r.DF20, r.DF21, r.DFOther} {
			df[j] += v
		}
		adsb += float64(r.ADSB)
		mlat += float64(r.MLAT)
		modes += float64(r.ModeS)
	}
	tot.MaxKm = Num(maxKm)
	if rssiW > 0 {
		tot.RSSIAvg = Num(rssiSum / rssiW)
	}
	if tot.Frames > 0 {
		tot.DecodePct = Num(100 * (1 - float64(tot.CRCBad)/float64(tot.Frames)))
	}
	if totSecs > 0 {
		tot.FramesPerSec = Num(float64(tot.Frames) / totSecs)
	}
	end := min(to, now+60)
	if total := (end - from) / 60; total > 0 {
		tot.UptimePct = Num(clamp(pct(float64(tot.MinutesWithData), float64(total)), 0, 100))
	}
	var dfTotal int64
	for _, v := range df {
		dfTotal += v
	}
	dfs := make([]DFCount, 0, len(df))
	for j, v := range df {
		dfs = append(dfs, DFCount{DF: dfNames[j][0], Name: dfNames[j][1], Count: v, Pct: Num(pct(float64(v), float64(dfTotal)))})
	}
	src := SourceShare{
		ADSBPct: Num(pct(adsb, adsb+mlat+modes)), MLATPct: Num(pct(mlat, adsb+mlat+modes)),
		ModeSPct:      Num(pct(modes, adsb+mlat+modes)),
		ADSBPositions: tot.Positions - tot.MLATPositions, MLATPositions: tot.MLATPositions,
	}
	if src.ADSBPositions < 0 {
		src.ADSBPositions = 0
	}
	return ts, tot, dfs, src
}

// ---------------------------------------------------------------- heatmap

// WeekHeatmap is traffic by hour of day and weekday (local time).
type WeekHeatmap struct {
	From         int64  `json:"from"`
	To           int64  `json:"to"`
	Ant          string `json:"ant"`
	UTCOffsetSec int    `json:"utcOffsetSec"`
	// Rows are weekdays Monday..Sunday, columns hours 0..23; null = no data.
	Aircraft     []Nums   `json:"aircraft"`     // mean aircraft in view
	FramesPerSec []Nums   `json:"framesPerSec"` // mean message rate
	Hours        [][]int  `json:"hours"`        // hours of data behind each cell
	Days         []string `json:"days"`
	// Busiest slot of the week.
	BusiestDay  int `json:"busiestDay"`  // 0 = Monday, -1 = no data
	BusiestHour int `json:"busiestHour"` // 0..23
	// HourOfDay is the mean aircraft count per hour of day over all weekdays.
	HourOfDay Nums `json:"hourOfDay"`
}

// WeekHeatmap returns the hour-of-day x weekday traffic heat map. Default
// range: the last 28 days.
func (a *Analyzer) WeekHeatmap(ctx context.Context, r Range, ant string) (*WeekHeatmap, error) {
	now, off := a.clock()
	from, to := r.resolve(now.Unix(), 28*86400, 3600)
	cfg := a.cfg()
	return cached(ctx, a, a.key("weekheat", ant, cfg, from, to, off), ttlTraffic, func(ctx context.Context) (*WeekHeatmap, error) {
		rows, err := a.hourStats(ctx, ant, from, to)
		if err != nil {
			return nil, err
		}
		res := &WeekHeatmap{
			From: from * 1000, To: to * 1000, Ant: ant, UTCOffsetSec: int(off),
			Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, BusiestDay: -1,
			HourOfDay: nanSlice(24),
		}
		var ac, fr [168]float64
		var n [168]int
		for i := range rows {
			row := &rows[i]
			slot := stat.HourOfWeek(row.T, int(off))
			if slot < 0 || slot >= 168 {
				continue
			}
			v := row.AircraftAvg
			if !isNum(v) || v <= 0 {
				v = float64(row.Aircraft)
			}
			ac[slot] += v
			fr[slot] += float64(row.Frames) / row.CoveredSec(true)
			n[slot]++
		}
		best := -1.0
		var hod, hodN [24]float64
		for d := 0; d < 7; d++ {
			ra, rf, rn := nanSlice(24), nanSlice(24), make([]int, 24)
			for h := 0; h < 24; h++ {
				s := d*24 + h
				rn[h] = n[s]
				if n[s] == 0 {
					continue
				}
				ra[h] = ac[s] / float64(n[s])
				rf[h] = fr[s] / float64(n[s])
				hod[h] += ac[s]
				hodN[h] += float64(n[s])
				if ra[h] > best {
					best, res.BusiestDay, res.BusiestHour = ra[h], d, h
				}
			}
			res.Aircraft = append(res.Aircraft, ra)
			res.FramesPerSec = append(res.FramesPerSec, rf)
			res.Hours = append(res.Hours, rn)
		}
		for h := 0; h < 24; h++ {
			if hodN[h] > 0 {
				res.HourOfDay[h] = hod[h] / hodN[h]
			}
		}
		return res, nil
	})
}

// ------------------------------------------------------------------ daily

// DayTotals is one local calendar day.
type DayTotals struct {
	T              int64  `json:"t"`    // local midnight, Unix ms
	Date           string `json:"date"` // YYYY-MM-DD (local)
	Frames         int64  `json:"frames"`
	Positions      int64  `json:"positions"`
	PeakAircraft   int    `json:"peakAircraft"`
	UniqueAircraft int64  `json:"uniqueAircraft"` // distinct airframes that started a flight
	Flights        int64  `json:"flights"`
	NewAirframes   int64  `json:"newAirframes"` // never seen before that day
	MaxKm          Num    `json:"maxKm"`
	UptimeMin      int    `json:"uptimeMin"`
	// BusiestHour is the hour of this day with the most aircraft in view
	// (null when no hour had aircraft).
	BusiestHour *BusiestHour `json:"busiestHour"`
	// Farthest is the flight that started this day with the largest
	// maximum distance (null when no flight had a position).
	Farthest *FarthestFlight `json:"farthest"`
}

// FarthestFlight is the flight with the largest maximum distance of a day.
type FarthestFlight struct {
	FlightID int64  `json:"flightId"` // flights.id (history / replay endpoints)
	ICAO     string `json:"icao"`     // hex, e.g. "3C6444"
	Callsign string `json:"callsign"` // "" when unknown
	Km       Num    `json:"km"`       // maximum distance from the receiver
	T        int64  `json:"t"`        // flight start, Unix ms
}

// BusiestHour is the hour with the most aircraft.
type BusiestHour struct {
	T        int64 `json:"t"`
	Aircraft int   `json:"aircraft"`
	Frames   int64 `json:"frames"`
}

// Daily is the result of Analyzer.Daily.
type Daily struct {
	From         int64        `json:"from"`
	To           int64        `json:"to"`
	Ant          string       `json:"ant"`
	UTCOffsetSec int          `json:"utcOffsetSec"`
	Days         []DayTotals  `json:"days"`
	BusiestDay   *DayTotals   `json:"busiestDay,omitempty"`  // most unique aircraft (frames when no flights)
	BusiestHour  *BusiestHour `json:"busiestHour,omitempty"` // most aircraft in view
}

// Daily returns per-day totals (local days): messages, positions, unique
// aircraft, flights, new airframes, maximum range, uptime; plus the busiest
// day and hour. Default range: the last 30 days.
func (a *Analyzer) Daily(ctx context.Context, r Range, ant string) (*Daily, error) {
	now, off := a.clock()
	from, to := r.resolve(now.Unix(), 30*86400, 3600)
	// Whole local days.
	from = floorDiv(from+off, 86400)*86400 - off
	to = floorDiv(to+off+86399, 86400)*86400 - off
	cfg := a.cfg()
	ttlFor := (*Daily).CacheTTL
	return cachedFor(ctx, a, a.key("daily", ant, cfg, from, to), ttlTraffic, ttlFor, func(ctx context.Context) (*Daily, error) {
		rows, err := a.hourStats(ctx, ant, from, to)
		if err != nil {
			return nil, err
		}
		res := &Daily{From: from * 1000, To: to * 1000, Ant: ant, UTCOffsetSec: int(off), Days: []DayTotals{}}
		days := map[int64]*DayTotals{}
		get := func(d int64) *DayTotals {
			dt := days[d]
			if dt == nil {
				start := d*86400 - off
				dt = &DayTotals{T: start * 1000, Date: time.Unix(d*86400, 0).UTC().Format("2006-01-02"), MaxKm: Num(math.NaN())}
				days[d] = dt
			}
			return dt
		}
		for i := range rows {
			row := &rows[i]
			dt := get(floorDiv(row.T+off, 86400))
			dt.Frames += row.Frames
			dt.Positions += row.Positions
			dt.PeakAircraft = max(dt.PeakAircraft, row.Aircraft)
			dt.MaxKm = Num(nanMax(float64(dt.MaxKm), row.MaxKm))
			mins := row.Minutes
			if mins <= 0 {
				mins = 60
			}
			dt.UptimeMin += mins
			if row.Aircraft > 0 && (dt.BusiestHour == nil || row.Aircraft > dt.BusiestHour.Aircraft) {
				dt.BusiestHour = &BusiestHour{T: row.T * 1000, Aircraft: row.Aircraft, Frames: row.Frames}
			}
			if res.BusiestHour == nil || row.Aircraft > res.BusiestHour.Aircraft {
				if row.Aircraft > 0 {
					res.BusiestHour = &BusiestHour{T: row.T * 1000, Aircraft: row.Aircraft, Frames: row.Frames}
				}
			}
		}
		if err := a.addDayTier(ctx, ant, from, to, off, rows, get); err != nil {
			return nil, err
		}
		db := a.flightDB()
		src, sargs := a.flightsSrc()
		if a.sid == "" {
			// Hourly ICAO-set rollup (store.FlightDays): the answer of the
			// grouped COUNT(DISTINCT) below without walking every flight.
			fd, err := a.mainSt().FlightDays(ctx, from, to, off)
			if err != nil {
				return nil, err
			}
			for d, v := range fd {
				dt := get(d)
				dt.UniqueAircraft, dt.Flights = v.Aircraft, v.Flights
			}
		} else if err := queryEach(ctx, db, `SELECT (start + ?) / 86400, COUNT(DISTINCT icao), COUNT(*) FROM `+src+` fl
WHERE start >= ? AND start < ? GROUP BY 1`, sqlArgs([]any{off}, sargs, []any{from, to}), func(rs *sql.Rows) error {
			var d, u, n int64
			if err := rs.Scan(&d, &u, &n); err != nil {
				return err
			}
			dt := get(d)
			dt.UniqueAircraft, dt.Flights = u, n
			return nil
		}); err != nil {
			return nil, err
		}
		// Farthest flight per day in one grouped query: SQLite returns the
		// bare columns of the row that holds MAX() of the group.
		if err := queryEach(ctx, db, `SELECT (start + ?) / 86400, id, icao, callsign, MAX(max_dist_km), start FROM `+src+` fl
WHERE start >= ? AND start < ? AND max_dist_km IS NOT NULL AND max_dist_km > 0 GROUP BY 1`, sqlArgs([]any{off}, sargs, []any{from, to}), func(rs *sql.Rows) error {
			var d, id, icao, start int64
			var cs string
			var km float64
			if err := rs.Scan(&d, &id, &icao, &cs, &km, &start); err != nil {
				return err
			}
			get(d).Farthest = &FarthestFlight{FlightID: id, ICAO: store.HexICAO(uint32(icao)), Callsign: strings.TrimSpace(cs), Km: Num(km), T: start * 1000}
			return nil
		}); err != nil {
			return nil, err
		}
		newQ, newArgs := `SELECT (first_seen + ?) / 86400, COUNT(*) FROM aircraft
WHERE first_seen >= ? AND first_seen < ? GROUP BY 1`, []any{off, from, to}
		if a.sid != "" { // first heard by this station
			newQ, newArgs = `SELECT (first_seen + ?) / 86400, COUNT(*) FROM (
  SELECT icao, MIN(first) AS first_seen FROM flight_station WHERE sid = ? AND icao != 0 GROUP BY icao)
WHERE first_seen >= ? AND first_seen < ? GROUP BY 1`, []any{off, a.sid, from, to}
		}
		if err := queryEach(ctx, db, newQ, newArgs, func(rs *sql.Rows) error {
			var d, n int64
			if err := rs.Scan(&d, &n); err != nil {
				return err
			}
			get(d).NewAirframes = n
			return nil
		}); err != nil {
			return nil, err
		}
		keys := make([]int64, 0, len(days))
		for d := range days {
			keys = append(keys, d)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		anyFlights := false
		for _, d := range keys {
			res.Days = append(res.Days, *days[d])
			anyFlights = anyFlights || days[d].UniqueAircraft > 0
		}
		for i := range res.Days {
			d := &res.Days[i]
			better := res.BusiestDay == nil
			if !better {
				if anyFlights {
					better = d.UniqueAircraft > res.BusiestDay.UniqueAircraft
				} else {
					better = d.Frames > res.BusiestDay.Frames
				}
			}
			if better && (d.UniqueAircraft > 0 || d.Frames > 0) {
				c := *d
				res.BusiestDay = &c
			}
		}
		return res, nil
	})
}

// addDayTier fills the days of [from, to) that lie entirely before the
// oldest hourly row from stats_day (the daily tier that outlives the hourly
// statistics on an aggregator, see store.TierPolicy). Day rows are UTC days;
// each is credited to the local day that contains its noon. Without a
// stats_day table, or when the hourly data reaches back to from, nothing
// changes.
func (a *Analyzer) addDayTier(ctx context.Context, ant string, from, to, off int64, hours []store.StatsRow, get func(int64) *DayTotals) error {
	oldest := to
	if len(hours) > 0 {
		oldest = hours[0].T
	}
	if oldest-from < 86400 || !a.st.HasTable("stats_day") {
		return nil
	}
	days, err := a.st.DayStats(ctx, a.antKey(ant), from-86400, oldest-86400)
	if err != nil {
		return err
	}
	for i := range days {
		r := &days[i]
		noon := r.T + 43200
		if r.T+86400 > oldest || noon < from || noon >= to {
			continue
		}
		dt := get(floorDiv(noon+off, 86400))
		dt.Frames += r.Frames
		dt.Positions += r.Positions
		dt.PeakAircraft = max(dt.PeakAircraft, r.Aircraft)
		dt.MaxKm = Num(nanMax(float64(dt.MaxKm), r.MaxKm))
		dt.UptimeMin += r.Minutes
	}
	return nil
}

func queryEach(ctx context.Context, db *sql.DB, q string, args []any, fn func(*sql.Rows) error) error {
	rs, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rs.Close()
	for rs.Next() {
		if err := fn(rs); err != nil {
			return err
		}
	}
	return rs.Err()
}

// -------------------------------------------------------------------- top

// TopEntry is one row of a top list.
type TopEntry struct {
	Key      string `json:"key"`                // operator / country / type code / ICAO hex
	Label    string `json:"label,omitempty"`    // type description, registration ...
	Flights  int64  `json:"flights"`            //
	Aircraft int64  `json:"aircraft"`           // distinct airframes
	Msgs     int64  `json:"msgs"`               //
	Pct      Num    `json:"pct"`                // share of all flights in range
	Type     string `json:"type,omitempty"`     // airframes only
	Operator string `json:"operator,omitempty"` // airframes only
	Country  string `json:"country,omitempty"`  // airframes only
	MaxKm    Num    `json:"maxKm"`              // airframes only
}

// Top is the result of Analyzer.Top.
type Top struct {
	From      int64      `json:"from"`
	To        int64      `json:"to"`
	Flights   int64      `json:"flights"`
	Aircraft  int64      `json:"aircraft"`
	Operators []TopEntry `json:"operators"`
	Countries []TopEntry `json:"countries"`
	Types     []TopEntry `json:"types"`
	Airframes []TopEntry `json:"airframes"`
}

// Top returns the most frequent operators, countries of registration,
// aircraft types and the most-seen airframes among the flights that started
// in the range (flights joined with aircraft and the imported aircraft
// database). limit defaults to 15 (max 200). Default range: 30 days.
func (a *Analyzer) Top(ctx context.Context, r Range, limit int) (*Top, error) {
	if limit <= 0 {
		limit = 15
	}
	limit = min(limit, 200)
	now, _ := a.clock()
	from, to := r.resolve(now.Unix(), 30*86400, 3600)
	// Empty / near-empty lists (a fresh database) are not kept for the full
	// ttlTraffic, or the dropdowns built from them stay empty after the
	// first flights arrive.
	ttlFor := (*Top).CacheTTL
	return cachedFor(ctx, a, a.key("top", "", a.cfg(), from, to, int64(limit)), ttlTraffic, ttlFor, func(ctx context.Context) (*Top, error) {
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
		// The flights of the range, aggregated per airframe in the order of
		// their first flight; the aircraft / acdb attributes are then looked
		// up once per airframe instead of once per flight (the 1.0.0 query
		// joined both tables for every flight). The lists are built by
		// replaying the airframes in that order, which gives exactly the
		// 1.0.0 result (labels are taken from the first airframe that has
		// one, as before).
		type frameAgg struct {
			icao    int64
			flights int64
			msgs    int64
			maxKm   float64
		}
		var order []*frameAgg
		byICAO := map[int64]*frameAgg{}
		src, sargs := a.flightsSrc()
		err := queryEach(ctx, a.flightDB(), `SELECT f.icao, f.msgs, f.max_dist_km FROM `+src+` f
WHERE f.start >= ? AND f.start < ?`, sqlArgs(sargs, []any{from, to}), func(rs *sql.Rows) error {
			var icao, msgs int64
			var dist sql.NullFloat64
			if err := rs.Scan(&icao, &msgs, &dist); err != nil {
				return err
			}
			g := byICAO[icao]
			if g == nil {
				g = &frameAgg{icao: icao, maxKm: math.NaN()}
				byICAO[icao] = g
				order = append(order, g)
			}
			g.flights++
			g.msgs += msgs
			if dist.Valid {
				g.maxKm = nanMax(g.maxKm, dist.Float64)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		attrs, err := airframeAttrs(ctx, a, order, func(g *frameAgg) int64 { return g.icao })
		if err != nil {
			return nil, err
		}
		for _, g := range order {
			at := attrs[g.icao]
			ic := uint32(g.icao)
			res.Flights += g.flights
			for _, m := range []struct {
				m     map[string]*agg
				key   string
				label string
			}{{ops, at.op, ""}, {ctys, at.cty, ""}, {types, at.typ, at.desc}} {
				if m.key == "" {
					continue
				}
				add(m.m, m.key, m.label, ic, g.msgs)
				m.m[m.key].e.Flights += g.flights - 1
			}
			f := &agg{e: TopEntry{Key: store.HexICAO(ic), Label: at.reg, Type: at.typ, Operator: at.op, Country: at.cty, Aircraft: 1,
				MaxKm: Num(g.maxKm), Flights: g.flights, Msgs: g.msgs}}
			if old := frames[ic]; old != nil { // two stored ICAOs mapping to one uint32
				old.e.Flights += f.e.Flights
				old.e.Msgs += f.e.Msgs
				old.e.MaxKm = Num(nanMax(float64(old.e.MaxKm), g.maxKm))
				continue
			}
			frames[ic] = f
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
		return res, nil
	})
}

func topN(l []TopEntry, limit int, total int64) []TopEntry {
	sort.Slice(l, func(i, j int) bool {
		if l[i].Flights != l[j].Flights {
			return l[i].Flights > l[j].Flights
		}
		if l[i].Msgs != l[j].Msgs {
			return l[i].Msgs > l[j].Msgs
		}
		return l[i].Key < l[j].Key
	})
	if len(l) > limit {
		l = l[:limit]
	}
	for i := range l {
		l[i].Pct = Num(pct(float64(l[i].Flights), float64(total)))
	}
	return l
}

// --------------------------------------------------------------- altitude

// AltBandCount is the number of positions received in one altitude band.
type AltBandCount struct {
	Band      int    `json:"band"`
	Label     string `json:"label"`
	Positions int64  `json:"positions"`
	Pct       Num    `json:"pct"`
	MaxKm     Num    `json:"maxKm"`
}

// AltitudeDistribution is the result of Analyzer.AltitudeDistribution.
type AltitudeDistribution struct {
	From  int64          `json:"from"`
	To    int64          `json:"to"`
	Ant   string         `json:"ant"`
	Bands []AltBandCount `json:"bands"` // positions per altitude band (coverage table)
	// Histogram of the maximum altitude of the flights in range.
	BinFt      int     `json:"binFt"`
	FlightAlt  []int64 `json:"flightAlt"`  // count per BinFt bin starting at 0 ft
	FlightsN   int64   `json:"flightsN"`   //
	MedianFt   Num     `json:"medianFt"`   // median flight maximum altitude
	LowestFt   Num     `json:"lowestFt"`   // lowest altitude seen on any flight
	HighestFt  Num     `json:"highestFt"`  //
	CruiseBand string  `json:"cruiseBand"` // label of the busiest band
}

// AltitudeDistribution returns where in the vertical the traffic is: the
// positions per altitude band and a histogram of flight maximum altitudes.
// Default range: 30 days.
func (a *Analyzer) AltitudeDistribution(ctx context.Context, r Range, ant string) (*AltitudeDistribution, error) {
	now, _ := a.clock()
	from, to := r.resolve(now.Unix(), 30*86400, 86400)
	ttlFor := (*AltitudeDistribution).CacheTTL
	return cachedFor(ctx, a, a.key("altdist", ant, a.cfg(), from, to), ttlTraffic, ttlFor, func(ctx context.Context) (*AltitudeDistribution, error) {
		const binFt, bins = 2000, 30
		res := &AltitudeDistribution{
			From: from * 1000, To: to * 1000, Ant: ant, BinFt: binFt, FlightAlt: make([]int64, bins),
			MedianFt: Num(math.NaN()), LowestFt: Num(math.NaN()), HighestFt: Num(math.NaN()),
		}
		res.Bands = make([]AltBandCount, geo.AltBands)
		for b := range res.Bands {
			res.Bands[b] = AltBandCount{Band: b, Label: geo.AltBandLabel(b), MaxKm: Num(math.NaN())}
		}
		var total int64
		bands, err := a.st.CoverageBands(ctx, a.antKey(ant), store.DayOf(from), store.DayOf(to-1), a.pending().Coverage)
		for _, cb := range bands {
			if b := cb.AltBand; b >= 0 && b < len(res.Bands) {
				res.Bands[b].Positions = cb.Count
				if cb.MaxKm > 0 {
					res.Bands[b].MaxKm = Num(cb.MaxKm)
				}
				total += cb.Count
			}
		}
		if err != nil {
			return nil, err
		}
		var best int64
		for b := range res.Bands {
			res.Bands[b].Pct = Num(pct(float64(res.Bands[b].Positions), float64(total)))
			if res.Bands[b].Positions > best {
				best, res.CruiseBand = res.Bands[b].Positions, res.Bands[b].Label
			}
		}
		var lo, hi sql.NullFloat64
		src, sargs := a.flightsSrc()
		err = queryEach(ctx, a.flightDB(), `SELECT CAST(max_alt / ? AS INTEGER), COUNT(*), MIN(min_alt), MAX(max_alt) FROM `+src+` fl
WHERE start >= ? AND start < ? AND max_alt IS NOT NULL GROUP BY 1`, sqlArgs([]any{binFt}, sargs, []any{from, to}), func(rs *sql.Rows) error {
			var b, n int64
			var mn, mx sql.NullFloat64
			if err := rs.Scan(&b, &n, &mn, &mx); err != nil {
				return err
			}
			b = min(max(b, 0), bins-1)
			res.FlightAlt[b] += n
			res.FlightsN += n
			if mn.Valid && (!lo.Valid || mn.Float64 < lo.Float64) {
				lo = mn
			}
			if mx.Valid && (!hi.Valid || mx.Float64 > hi.Float64) {
				hi = mx
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if res.FlightsN > 0 {
			res.MedianFt = Num(stat.HistQuantile(res.FlightAlt, 0, binFt, 0.5))
		}
		if lo.Valid {
			res.LowestFt = Num(lo.Float64)
		}
		if hi.Valid {
			res.HighestFt = Num(hi.Float64)
		}
		return res, nil
	})
}

// ------------------------------------------------------- records / events

// Records returns the all-time records (store rows marshal to API form).
func (a *Analyzer) Records(ctx context.Context) ([]store.Record, error) {
	return cached(ctx, a, "records", ttlLive, func(ctx context.Context) ([]store.Record, error) {
		recs, err := a.mainSt().Records(ctx)
		if recs == nil {
			recs = []store.Record{}
		}
		return recs, err
	})
}

// RecentEvents returns the newest events, newest first (limit default 50).
func (a *Analyzer) RecentEvents(ctx context.Context, limit int) ([]store.Event, error) {
	if limit <= 0 {
		limit = 50
	}
	ev, err := a.mainSt().Events(ctx, store.EventQuery{Limit: limit})
	if ev == nil {
		ev = []store.Event{}
	}
	return ev, err
}

// --------------------------------------------------------------- overview

// AllTime are lifetime totals of the database.
type AllTime struct {
	Airframes int64 `json:"airframes"`
	Flights   int64 `json:"flights"`
	Msgs      int64 `json:"msgs"`
	Positions int64 `json:"positions"`
	Since     int64 `json:"since"` // first sighting, Unix ms; 0 = empty database
	MaxKm     Num   `json:"maxKm"`
	Countries int64 `json:"countries"`
}

// Overview bundles what a statistics landing page needs in one call.
type Overview struct {
	Now      int64          `json:"now"`
	Ant      string         `json:"ant"`
	Last24h  TrafficTotals  `json:"last24h"`
	Sources  SourceShare    `json:"sources"`
	DF       []DFCount      `json:"df"`
	AllTime  AllTime        `json:"allTime"`
	Today    *DayTotals     `json:"today,omitempty"`
	Busiest  *Daily         `json:"busiest,omitempty"` // 30-day daily totals with busiest day / hour
	Top      *Top           `json:"top,omitempty"`     // 30 days, 5 rows each
	Records  []store.Record `json:"records"`
	Events   []store.Event  `json:"events"`
	Located  bool           `json:"located"`
	Receiver *LatLon        `json:"receiver,omitempty"`
}

// Overview returns the 24 h totals, lifetime totals, today's figures, the
// 30-day daily series with the busiest day / hour, short top lists, the
// records and the latest events.
func (a *Analyzer) Overview(ctx context.Context, ant string) (*Overview, error) {
	now, _ := a.clock()
	cfg := a.cfg()
	return cached(ctx, a, a.key("overview", ant, cfg, now.Unix()/10), ttlLive, func(ctx context.Context) (*Overview, error) {
		s := siteOf(cfg, ant)
		res := &Overview{Now: now.UnixMilli(), Ant: ant, Located: s.located, Receiver: s.pos()}
		tr, err := a.Traffic(ctx, LastHours(24), ant)
		if err != nil {
			return nil, err
		}
		res.Last24h, res.Sources, res.DF = tr.Totals, tr.Sources, tr.DF
		res.AllTime.MaxKm = Num(math.NaN())
		var since sql.NullInt64
		var maxKm sql.NullFloat64
		var msgs, pos sql.NullInt64
		allQ, allArgs := `SELECT COUNT(*), SUM(msgs), SUM(positions), MIN(first_seen), MAX(max_dist_km),
  COUNT(DISTINCT NULLIF(country, '')) FROM aircraft`, []any(nil)
		if a.sid != "" { // what this station heard
			allQ, allArgs = `SELECT COUNT(*), SUM(m), SUM(p), MIN(f), MAX(d), COUNT(DISTINCT NULLIF(a.country, '')) FROM (
  SELECT icao, SUM(msgs) AS m, SUM(positions) AS p, MIN(first) AS f, MAX(max_dist_km) AS d
  FROM flight_station WHERE sid = ? AND icao != 0 GROUP BY icao) s LEFT JOIN aircraft a ON a.icao = s.icao`, []any{a.sid}
		}
		if err := a.flightDB().QueryRowContext(ctx, allQ, allArgs...).
			Scan(&res.AllTime.Airframes, &msgs, &pos, &since, &maxKm, &res.AllTime.Countries); err != nil {
			return nil, err
		}
		res.AllTime.Msgs, res.AllTime.Positions = msgs.Int64, pos.Int64
		res.AllTime.Since = since.Int64 * 1000
		if maxKm.Valid && maxKm.Float64 > 0 {
			res.AllTime.MaxKm = Num(maxKm.Float64)
		}
		src, sargs := a.flightsSrc()
		if err := a.flightDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+src+` fl`, sargs...).Scan(&res.AllTime.Flights); err != nil {
			return nil, err
		}
		daily, err := a.Daily(ctx, LastDays(30), ant)
		if err != nil {
			return nil, err
		}
		res.Busiest = daily
		if n := len(daily.Days); n > 0 {
			if last := daily.Days[n-1]; last.T <= now.UnixMilli() && now.UnixMilli() < last.T+86400_000 {
				res.Today = &last
			}
		}
		if res.Top, err = a.Top(ctx, LastDays(30), 5); err != nil {
			return nil, err
		}
		if res.Records, err = a.Records(ctx); err != nil {
			return nil, err
		}
		if res.Events, err = a.RecentEvents(ctx, 20); err != nil {
			return nil, err
		}
		return res, nil
	})
}

// CacheTTL is how long a Daily answer may be kept, by this package and by
// the HTTP response cache (which looks for this method): not at all when
// nothing is received yet, briefly while fewer than sparseFlights flights
// exist, else ttlTraffic.
func (d *Daily) CacheTTL() time.Duration {
	if d == nil {
		return 0
	}
	var n int64
	for i := range d.Days {
		n += d.Days[i].Flights
	}
	if n == 0 && len(d.Days) > 0 {
		return sparseTTL // receiving, but no flights closed yet
	}
	return flightsTTL(n)
}

// CacheTTL: see (*Daily).CacheTTL.
func (t *Top) CacheTTL() time.Duration {
	if t == nil {
		return 0
	}
	return flightsTTL(t.Flights)
}

// CacheTTL: see (*Daily).CacheTTL.
func (d *AltitudeDistribution) CacheTTL() time.Duration {
	if d == nil {
		return 0
	}
	return flightsTTL(d.FlightsN)
}
