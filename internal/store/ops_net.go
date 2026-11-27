package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Network tables of aggregator mode (DESIGN §6.4, §6.6): row types, SQL and
// Batch operations. flight_station, mlat_minute and mlat_quality_hour live in
// airview.db (KindMain); station_minute and station_clock_minute in
// stations.db (KindStations).

// ------------------------------------------------------------------- types

// FlightStation is one row of flight_station: what one station (SID)
// contributed to one flight. Batch.UpsertFlightStation MERGES into an
// existing (FlightID, SID) row: Msgs and Positions add, First takes the
// minimum, Last the maximum, MaxDistKm / RSSIMax the maximum, MinDistKm /
// RSSIMin the minimum (NaN = unknown, never wins), RSSIAvg the Msgs-weighted
// mean, ICAO the latest non-zero value.
type FlightStation struct {
	FlightID int64
	SID      string
	ICAO     uint32
	// First / Last are the Unix seconds of the station's first and last copy.
	First, Last int64
	Msgs        int64
	Positions   int64
	// MaxDistKm / MinDistKm: farthest / nearest accepted position from the
	// station (NaN when the station was not located).
	MaxDistKm, MinDistKm float64
	// RSSI statistics of the copies, dBFS (NaN = none).
	RSSIMin, RSSIMax, RSSIAvg float64
}

// MLATMinute is one row of mlat_minute: the MLAT contribution of a station
// (SID) or of the whole network (SID "") in one minute (T = start of the
// minute). Batch.AddMLATMinutes MERGES: the counters add, Peers takes the
// maximum, SyncedSecs adds up (capped at 60; NaN = unknown), and the clock
// figures SigmaNs / ResidRMSNs / DriftPPM keep the latest known value.
type MLATMinute struct {
	T   int64
	SID string
	// Frames: timed frames received; SyncFrames: frames used by the clock
	// filter; Groups: timed groups the station took part in; Fixes: MLAT
	// fixes it contributed to; Outliers: its copies rejected by the solver
	// (RAIM / gating).
	Frames, SyncFrames, Groups, Fixes, Outliers int64
	// Peers is the number of synced peers (gauge).
	Peers int64
	// SyncedSecs: seconds of the minute with a synced clock (NaN = unknown).
	SyncedSecs float64
	// SigmaNs: clock offset standard deviation; ResidRMSNs: RMS of the
	// station's TDOA residuals; DriftPPM: relative frequency (NaN = unknown).
	SigmaNs, ResidRMSNs, DriftPPM float64
}

// MLAT error histogram geometry (mlat_quality_hour.err_hist): MLATErrBins
// logarithmic bins of 0.1 decade from 1 m; bin 0 also holds errors below
// 1 m, the last bin everything from 10^((MLATErrBins-1)/10) m (~8 km).
const (
	MLATErrBins       = 40
	mlatErrBinsPerDec = 10.0
)

// MLATErrBin returns the err_hist bin of a horizontal error in metres.
func MLATErrBin(m float64) int {
	if !(m > 1) {
		return 0
	}
	b := int(math.Log10(m) * mlatErrBinsPerDec)
	return min(max(b, 0), MLATErrBins-1)
}

// MLATErrPercentile returns the q-quantile (0..1) of an err_hist in metres,
// interpolated geometrically inside the bin (NaN for an empty histogram).
func MLATErrPercentile(hist []uint32, q float64) float64 {
	var total uint64
	for _, c := range hist {
		total += uint64(c)
	}
	if total == 0 {
		return math.NaN()
	}
	q = math.Max(0, math.Min(1, q))
	target := q * float64(total)
	var cum float64
	for i, c := range hist {
		if c == 0 {
			continue
		}
		next := cum + float64(c)
		if next >= target {
			frac := (target - cum) / float64(c)
			return math.Pow(10, (float64(i)+frac)/mlatErrBinsPerDec)
		}
		cum = next
	}
	return math.Pow(10, float64(len(hist))/mlatErrBinsPerDec)
}

// Cell1Deg is the 1° cell index of mlat_quality_hour.cell1deg:
// (floor(lat)+90)*360 + (floor(lon)+180), lon wrapped to -180..180.
func Cell1Deg(lat, lon float64) int {
	if !isNum(lat) || !isNum(lon) {
		return -1
	}
	lat = math.Max(-90, math.Min(89.999999, lat))
	lon = math.Mod(lon+180, 360)
	if lon < 0 {
		lon += 360
	}
	return (int(math.Floor(lat))+90)*360 + int(math.Floor(lon))
}

// Cell1DegOrigin returns the south-west corner of a Cell1Deg cell.
func Cell1DegOrigin(cell int) (lat, lon float64) {
	return float64(cell/360 - 90), float64(cell%360 - 180)
}

// MLATQualityInc is an increment of mlat_quality_hour: hold-out errors of
// one hour (T, rounded down), 1° cell (Cell1Deg) and altitude band (AltBand)
// as an err_hist of MLATErrBins counts. Merge: histograms add; N and the
// percentiles are recomputed from the merged histogram.
type MLATQualityInc struct {
	T       int64
	Cell    int
	AltBand int
	Hist    []uint32
}

// MLATQualityHour is a row of mlat_quality_hour (read API).
type MLATQualityHour struct {
	T             int64
	Cell, AltBand int
	N             int64
	P50, P95, P99 float64 // metres (NaN = no data)
	Hist          []uint32
}

// StationMinute is one row of station_minute (stations.db): link health of
// a station in one minute. Merge: counters add; UpSecs adds (capped at 60);
// RTTMs keeps the latest known value.
type StationMinute struct {
	T   int64
	SID string
	// Frames received, of which Timed carried a usable timestamp and Late
	// were backfilled; Dropped frames refused by budgets; Bytes on the wire.
	Frames, Timed, Late, Dropped, Bytes int64
	UpSecs                              float64 // seconds connected (NaN = unknown)
	RTTMs                               float64 // keepalive round trip (NaN = unknown)
}

// StationClockMinute is one row of station_clock_minute (stations.db): the
// clock-filter state of a station in one minute. Merge: Outliers and
// EpochChanges add, Peers takes the maximum, SyncedSecs adds (capped at
// 60), the other figures keep the latest known value.
type StationClockMinute struct {
	T                                          int64
	SID                                        string
	SyncedSecs                                 float64
	SigmaNs, DriftPPM, BetaShortNs, ResidRMSNs float64
	Outliers, Peers, EpochChanges              int64
}

// --------------------------------------------------------------------- SQL

// latestKnown keeps a nullable value up to date: a known new value replaces
// the stored one, NULL keeps it.
func latestKnown(col string) string { return col + " = COALESCE(excluded." + col + ", " + col + ")" }

// secsAdd adds covered seconds capped at a minute; NULL + x = x.
func secsAdd(col string) string {
	return col + " = CASE WHEN " + col + " IS NULL THEN excluded." + col + " WHEN excluded." + col +
		" IS NULL THEN " + col + " ELSE MIN(60.0, " + col + " + excluded." + col + ") END"
}

var sqlMergeFlightStation = `INSERT INTO flight_station(flight_id, sid, icao, first, last, msgs, positions,
  max_dist_km, min_dist_km, rssi_min, rssi_max, rssi_avg)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(flight_id, sid) DO UPDATE SET
  icao = CASE WHEN excluded.icao != 0 THEN excluded.icao ELSE icao END,
  first = MIN(first, excluded.first), last = MAX(last, excluded.last),
  rssi_avg = CASE
      WHEN rssi_avg IS NULL THEN excluded.rssi_avg
      WHEN excluded.rssi_avg IS NULL THEN rssi_avg
      WHEN msgs + excluded.msgs > 0
        THEN (rssi_avg * msgs + excluded.rssi_avg * excluded.msgs) / (msgs + excluded.msgs)
      ELSE (rssi_avg + excluded.rssi_avg) / 2.0 END,
  msgs = msgs + excluded.msgs, positions = positions + excluded.positions,
  ` + nullMax("max_dist_km") + `,
  ` + nullMin("min_dist_km") + `,
  ` + nullMin("rssi_min") + `,
  ` + nullMax("rssi_max")

var sqlMergeMLATMinute = `INSERT INTO mlat_minute(t, sid, frames, sync_frames, groups, fixes, outliers, peers,
  synced_secs, sigma_ns, resid_rms_ns, drift_ppm)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(t, sid) DO UPDATE SET
  frames = frames + excluded.frames, sync_frames = sync_frames + excluded.sync_frames,
  groups = groups + excluded.groups, fixes = fixes + excluded.fixes, outliers = outliers + excluded.outliers,
  peers = MAX(peers, excluded.peers),
  ` + strings.Join([]string{secsAdd("synced_secs"), latestKnown("sigma_ns"), latestKnown("resid_rms_ns"), latestKnown("drift_ppm")}, ",\n  ")

const sqlGetMLATQuality = `SELECT err_hist FROM mlat_quality_hour WHERE t = ? AND cell1deg = ? AND altband = ?`

const sqlPutMLATQuality = `INSERT OR REPLACE INTO mlat_quality_hour(t, cell1deg, altband, n, p50, p95, p99, err_hist)
VALUES(?,?,?,?,?,?,?,?)`

var sqlMergeStationMinute = `INSERT INTO station_minute(t, sid, frames, timed, late, dropped, bytes, up_secs, rtt_ms)
VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(t, sid) DO UPDATE SET
  frames = frames + excluded.frames, timed = timed + excluded.timed, late = late + excluded.late,
  dropped = dropped + excluded.dropped, bytes = bytes + excluded.bytes,
  ` + secsAdd("up_secs") + `,
  ` + latestKnown("rtt_ms")

var sqlMergeStationClockMinute = `INSERT INTO station_clock_minute(t, sid, synced_secs, sigma_ns, drift_ppm,
  beta_short_ns, resid_rms_ns, outliers, peers, epoch_changes)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(t, sid) DO UPDATE SET
  outliers = outliers + excluded.outliers, epoch_changes = epoch_changes + excluded.epoch_changes,
  peers = MAX(peers, excluded.peers),
  ` + strings.Join([]string{secsAdd("synced_secs"), latestKnown("sigma_ns"), latestKnown("drift_ppm"),
	latestKnown("beta_short_ns"), latestKnown("resid_rms_ns")}, ",\n  ")

// secsNull maps covered seconds to SQL: NaN / <= 0 = NULL, capped at 60.
func secsNull(v float64) any {
	if !(v > 0) {
		return nil
	}
	return min(v, 60)
}

// --------------------------------------------------------------- batch ops

// UpsertFlightStation merges flight_station rows (airview.db; see
// FlightStation for the merge rules). Rows with FlightID <= 0 or an empty SID
// are ignored.
func (b *Batch) UpsertFlightStation(rows []FlightStation) {
	rows = slices.Clone(rows)
	b.add("flight_station", tierHigh, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlMergeFlightStation)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if r.FlightID <= 0 || r.SID == "" {
				continue
			}
			first, last := r.First, r.Last
			if last < first {
				first, last = last, first
			}
			if _, err := st.ExecContext(tc.ctx, r.FlightID, r.SID, int64(r.ICAO), first, last, r.Msgs, r.Positions,
				nf(r.MaxDistKm), nf(r.MinDistKm), nf(r.RSSIMin), nf(r.RSSIMax), nf(r.RSSIAvg)); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddMLATMinutes merges mlat_minute rows (airview.db; see MLATMinute). T is
// rounded down to the minute.
func (b *Batch) AddMLATMinutes(rows []MLATMinute) {
	rows = slices.Clone(rows)
	b.add("mlat_minute", tierHigh, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlMergeMLATMinute)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if _, err := st.ExecContext(tc.ctx, floorTo(r.T, 60), r.SID, r.Frames, r.SyncFrames, r.Groups, r.Fixes,
				r.Outliers, r.Peers, secsNull(r.SyncedSecs), nf(r.SigmaNs), nf(r.ResidRMSNs), nf(r.DriftPPM)); err != nil {
				return err
			}
		}
		return nil
	})
}

// MergeMLATQuality adds hold-out error histograms to mlat_quality_hour
// (airview.db; see MLATQualityInc). T is rounded down to the hour; rows with
// an invalid cell or an empty histogram are ignored.
func (b *Batch) MergeMLATQuality(rows []MLATQualityInc) {
	rows = slices.Clone(rows)
	for i := range rows {
		rows[i].Hist = slices.Clone(rows[i].Hist)
	}
	b.add("mlat_quality_hour", tierMid, len(rows), func(tc *txc) error {
		get, err := tc.stmt(sqlGetMLATQuality)
		if err != nil {
			return err
		}
		put, err := tc.stmt(sqlPutMLATQuality)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if r.Cell < 0 || len(r.Hist) == 0 {
				continue
			}
			t := floorTo(r.T, 3600)
			var blob []byte
			if err := get.QueryRowContext(tc.ctx, t, r.Cell, r.AltBand).Scan(&blob); err != nil && !isNoRows(err) {
				return err
			}
			h := addHist(DecodeDistHist(blob), r.Hist, MLATErrBins)
			var n uint64
			for _, c := range h {
				n += uint64(c)
			}
			if n == 0 {
				continue
			}
			if _, err := put.ExecContext(tc.ctx, t, r.Cell, r.AltBand, int64(n),
				nf(MLATErrPercentile(h, 0.50)), nf(MLATErrPercentile(h, 0.95)), nf(MLATErrPercentile(h, 0.99)),
				EncodeDistHist(h)); err != nil {
				return err
			}
		}
		return nil
	})
}

// addHist adds b to a bin by bin (saturating), padding to at least n bins.
func addHist(a, b []uint32, n int) []uint32 {
	out := make([]uint32, max(n, len(a), len(b)))
	for i := range out {
		var x, y uint64
		if i < len(a) {
			x = uint64(a[i])
		}
		if i < len(b) {
			y = uint64(b[i])
		}
		out[i] = uint32(min(x+y, math.MaxUint32))
	}
	return out
}

// AddStationMinutes merges station_minute rows (stations.db; see
// StationMinute). T is rounded down to the minute; rows without SID are
// ignored.
func (b *Batch) AddStationMinutes(rows []StationMinute) {
	rows = slices.Clone(rows)
	b.add("station_minute", tierHigh, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlMergeStationMinute)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if r.SID == "" {
				continue
			}
			if _, err := st.ExecContext(tc.ctx, floorTo(r.T, 60), r.SID, r.Frames, r.Timed, r.Late, r.Dropped, r.Bytes,
				secsNull(r.UpSecs), nf(r.RTTMs)); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddStationClockMinutes merges station_clock_minute rows (stations.db; see
// StationClockMinute). T is rounded down to the minute; rows without SID
// are ignored.
func (b *Batch) AddStationClockMinutes(rows []StationClockMinute) {
	rows = slices.Clone(rows)
	b.add("station_clock_minute", tierMid, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlMergeStationClockMinute)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if r.SID == "" {
				continue
			}
			if _, err := st.ExecContext(tc.ctx, floorTo(r.T, 60), r.SID, secsNull(r.SyncedSecs), nf(r.SigmaNs),
				nf(r.DriftPPM), nf(r.BetaShortNs), nf(r.ResidRMSNs), r.Outliers, r.Peers, r.EpochChanges); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpsertFlightStation queues Batch.UpsertFlightStation.
func (s *Store) UpsertFlightStation(rows []FlightStation) {
	s.queue1(func(b *Batch) { b.UpsertFlightStation(rows) })
}

// AddMLATMinutes queues Batch.AddMLATMinutes.
func (s *Store) AddMLATMinutes(rows []MLATMinute) {
	s.queue1(func(b *Batch) { b.AddMLATMinutes(rows) })
}

// MergeMLATQuality queues Batch.MergeMLATQuality.
func (s *Store) MergeMLATQuality(rows []MLATQualityInc) {
	s.queue1(func(b *Batch) { b.MergeMLATQuality(rows) })
}

// AddStationMinutes queues Batch.AddStationMinutes.
func (s *Store) AddStationMinutes(rows []StationMinute) {
	s.queue1(func(b *Batch) { b.AddStationMinutes(rows) })
}

// AddStationClockMinutes queues Batch.AddStationClockMinutes.
func (s *Store) AddStationClockMinutes(rows []StationClockMinute) {
	s.queue1(func(b *Batch) { b.AddStationClockMinutes(rows) })
}

// ------------------------------------------- queued transactions (group commit)

// Tx queues fn to run inside the next group commit, with the other queued
// operations, instead of in a transaction of its own (Store.Write commits
// alone and forces the queue out first: two commits per call). Use it for
// small periodic writes of other packages (latency minutes, RF statistics,
// forecast logs, learned corrections, caches) whose caller does not need to
// wait for the commit. rows is the row count for the queue accounting.
//
// fn may run more than once (a failed group commit is retried, the second
// time with every operation isolated by a savepoint), so it must only
// write what it was given: no side effects outside tx, no mutation of
// captured state. It must not call Write, Flush, Apply or another
// synchronous Store method. Errors are logged by the writer (the operation
// is skipped when it fails on its own data). The operation has the middle
// priority of the overflow policy (shed after histogram increments, before
// aircraft and flights).
func (b *Batch) Tx(name string, rows int, fn func(ctx context.Context, tx *sql.Tx) error) {
	if fn == nil {
		return
	}
	b.add(name, tierMid, max(rows, 1), func(tc *txc) error { return fn(tc.ctx, tc.tx) })
}

// QueueTx queues Batch.Tx.
func (s *Store) QueueTx(name string, rows int, fn func(ctx context.Context, tx *sql.Tx) error) {
	s.queue1(func(b *Batch) { b.Tx(name, rows, fn) })
}

// ------------------------------------------------ weather_fc: diff writes

// KVForecastIssue holds the issue of the newest forecast written with
// SetWeatherForecastDiff: "issued,from,to" (Unix seconds). Every weather_fc
// row with from <= t <= to belongs to that issue, whether or not its row was
// rewritten (rows whose values did not change are not written, so their
// stored issued column can be older); readers apply it with
// ApplyForecastIssue. Rows outside the window carry their final issued
// value in the table.
const KVForecastIssue = "store.weather_fc.issue"

// ForecastIssue is the decoded KVForecastIssue (zero: none).
type ForecastIssue struct {
	Issued, From, To int64
}

func (fi ForecastIssue) String() string {
	return strconv.FormatInt(fi.Issued, 10) + "," + strconv.FormatInt(fi.From, 10) + "," + strconv.FormatInt(fi.To, 10)
}

// ParseForecastIssue decodes a KVForecastIssue value (ok false: none or
// malformed).
func ParseForecastIssue(v string) (ForecastIssue, bool) {
	p := strings.Split(v, ",")
	if len(p) != 3 {
		return ForecastIssue{}, false
	}
	var fi ForecastIssue
	var err [3]error
	fi.Issued, err[0] = strconv.ParseInt(p[0], 10, 64)
	fi.From, err[1] = strconv.ParseInt(p[1], 10, 64)
	fi.To, err[2] = strconv.ParseInt(p[2], 10, 64)
	if err[0] != nil || err[1] != nil || err[2] != nil || fi.Issued <= 0 || fi.To < fi.From {
		return ForecastIssue{}, false
	}
	return fi, true
}

// Apply sets Issued on the rows inside the issue window.
func (fi ForecastIssue) Apply(rows []Weather) {
	if fi.Issued <= 0 {
		return
	}
	for i := range rows {
		if rows[i].T >= fi.From && rows[i].T <= fi.To {
			rows[i].Issued = fi.Issued
		}
	}
}

// sqlWeatherFcSame finds a weather_fc row equal to the arguments in every
// measurement column and the profile (IS compares NULLs; the column
// affinity converts the arguments exactly as the INSERT would).
var sqlWeatherFcSame = func() string {
	conds := make([]string, 0, len(weatherCols)+2)
	conds = append(conds, "t = ?")
	for _, c := range weatherCols {
		conds = append(conds, c+" IS ?")
	}
	conds = append(conds, "profile_json IS ?")
	return "SELECT 1 FROM weather_fc WHERE " + strings.Join(conds, " AND ")
}()

// SetWeatherForecastDiff is SetWeatherForecast for a whole forecast issue
// (rows: the hours of one forecast; Issued set on every row, 0 = now) that
// writes only the rows whose values changed. A forecast poll usually
// repeats most hours unchanged (the weather model runs every few hours);
// 1.0.0 rewrote every row (~1.25 KB each, ~170 rows) with the new issue
// time. The issue time of the unchanged rows is kept in KVForecastIssue
// instead (same transaction) and applied by the readers (Store.ForecastRows,
// Store.ApplyForecastIssue), so every reader still sees each row of the
// window with the newest issue, as before. Rows of the previous window that
// the new window does not cover get their final issued value written (at
// most a few rows per issue).
//
// When the rows are not consecutive whole hours of one issue the window
// cannot describe them: every row is written and the window is cleared
// (1.0.0 behaviour).
func (b *Batch) SetWeatherForecastDiff(rows []Weather) {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b Weather) int { return cmpKey64(a.T, b.T) })
	b.add("weather_fc", tierHigh, len(rows), func(tc *txc) error {
		if len(rows) == 0 {
			return nil
		}
		now := time.Now().Unix()
		issued := rows[0].Issued
		if issued <= 0 {
			issued = now
		}
		contiguous := true
		for i := range rows {
			if (rows[i].Issued > 0 && rows[i].Issued != issued) || rows[i].T%3600 != 0 ||
				(i > 0 && rows[i].T != rows[i-1].T+3600) {
				contiguous = false
				break
			}
		}
		cur := ForecastIssue{Issued: issued, From: rows[0].T, To: rows[len(rows)-1].T}

		// 1. Finalise the rows of the previous window that the new window
		// does not cover: they keep the previous issue for good.
		var prev ForecastIssue
		var pv string
		err := tc.tx.QueryRowContext(tc.ctx, `SELECT value FROM kv WHERE key = ?`, KVForecastIssue).Scan(&pv)
		switch {
		case err == nil:
			prev, _ = ParseForecastIssue(pv)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if prev.Issued > 0 {
			q := `UPDATE weather_fc SET issued = ?1 WHERE t >= ?2 AND t <= ?3 AND issued <> ?1`
			args := []any{prev.Issued, prev.From, prev.To}
			if contiguous {
				q += ` AND (t < ?4 OR t > ?5)`
				args = append(args, cur.From, cur.To)
			}
			if _, err := tc.tx.ExecContext(tc.ctx, q, args...); err != nil {
				return err
			}
		}

		// 2. Write the changed (or new) rows.
		ins, err := tc.stmt(sqlUpsertWeatherFc)
		if err != nil {
			return err
		}
		same, err := tc.stmt(sqlWeatherFcSame)
		if err != nil {
			return err
		}
		for i := range rows {
			w := &rows[i]
			vals := append(weatherArgs(w), ns(w.ProfileJSON))
			if contiguous {
				var one int
				err := same.QueryRowContext(tc.ctx, append([]any{w.T}, vals...)...).Scan(&one)
				if err == nil {
					continue // unchanged: KVForecastIssue carries the issue
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			if _, err := ins.ExecContext(tc.ctx, append([]any{w.T, issued}, vals...)...); err != nil {
				return err
			}
		}

		// 3. Remember the window.
		if !contiguous {
			_, err := tc.tx.ExecContext(tc.ctx, sqlDeleteKV, KVForecastIssue)
			return err
		}
		_, err = tc.tx.ExecContext(tc.ctx, sqlSetKV, KVForecastIssue, cur.String(), now)
		return err
	})
}

// sqlWeatherObsSame finds a weather_obs row that the merge of the
// arguments would leave unchanged: every present (non-NULL) argument equals
// the stored value (missing values keep what is stored, see AddWeatherObs).
var sqlWeatherObsSame = func() string {
	conds := make([]string, 0, len(weatherCols)+2)
	conds = append(conds, "t = ?1")
	for i, c := range append(slices.Clone(weatherCols), "profile_json") {
		n := strconv.Itoa(i + 2)
		conds = append(conds, "(?"+n+" IS NULL OR "+c+" IS ?"+n+")")
	}
	return "SELECT 1 FROM weather_obs WHERE " + strings.Join(conds, " AND ")
}()

// AddWeatherObsDiff is AddWeatherObs that skips the rows whose merge would
// not change the stored row (the hourly poll repeats two days of analysis
// hours, almost all of them unchanged; each is ~1.25 KB). The result is
// the same table content as AddWeatherObs.
func (b *Batch) AddWeatherObsDiff(rows []Weather) {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b Weather) int { return cmpKey64(a.T, b.T) })
	b.add("weather_obs", tierHigh, len(rows), func(tc *txc) error {
		up, err := tc.stmt(sqlUpsertWeatherObs)
		if err != nil {
			return err
		}
		same, err := tc.stmt(sqlWeatherObsSame)
		if err != nil {
			return err
		}
		for i := range rows {
			w := &rows[i]
			args := append([]any{w.T}, weatherArgs(w)...)
			args = append(args, ns(w.ProfileJSON))
			var one int
			err := same.QueryRowContext(tc.ctx, args...).Scan(&one)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := up.ExecContext(tc.ctx, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddWeatherObsDiff queues Batch.AddWeatherObsDiff.
func (s *Store) AddWeatherObsDiff(rows []Weather) {
	s.queue1(func(b *Batch) { b.AddWeatherObsDiff(rows) })
}

// SetWeatherForecastDiff queues Batch.SetWeatherForecastDiff.
func (s *Store) SetWeatherForecastDiff(rows []Weather) {
	s.queue1(func(b *Batch) { b.SetWeatherForecastDiff(rows) })
}

func cmpKey64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// finaliseForecastIssue writes the issue time of the newest diff-written
// forecast (KVForecastIssue) into its rows and clears the window, so that a
// full SetWeatherForecast after diff writes is not overridden by an older
// window on read. No-op (one kv point read) without a window.
func finaliseForecastIssue(ctx context.Context, tx *sql.Tx) error {
	var v string
	err := tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, KVForecastIssue).Scan(&v)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi, ok := ParseForecastIssue(v); ok {
		if _, err := tx.ExecContext(ctx, `UPDATE weather_fc SET issued = ? WHERE t BETWEEN ? AND ? AND issued != ?`,
			fi.Issued, fi.From, fi.To, fi.Issued); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, KVForecastIssue)
	return err
}
