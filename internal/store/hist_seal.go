package store

import (
	"bytes"
	"compress/flate"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"time"
)

// Sealed day histograms (DB-EFFICIENCY-DESIGN §3.12, KindMain only).
//
// coverage, signal_hist and elev_hist get one row per bin and day (~7,600
// rows a day for three antennas, 20-45 bytes each) and are kept forever.
// Days older than histSealAge are packed into one row per (antenna, table,
// day) of
//
//	hist_day(id INTEGER PRIMARY KEY, ant TEXT, tbl INTEGER, day INTEGER,
//	         n INTEGER, data BLOB), UNIQUE(ant, tbl, day)
//
// (a rowid table: the BLOBs are kilobytes) and their rows deleted from the
// live table in the same transaction. data is lossless: a codec byte (1 =
// plain, 2 = raw deflate) and a columnar payload of zig-zag varints; REAL
// values are written as a scaled integer plus the exact distance in ULPs
// when that is shorter, else as their 8 IEEE bytes. n is the number of bins.
//
// The readers of stats_read.go (CoverageRows, CoverageBands, SignalTotals,
// ElevTotals) and the logical tables of the three names return sealed and
// live rows together; a late increment for a sealed day lands in the live
// table and is merged by the readers (UPSERT rules) and folded into the
// sealed row by the next sealing run.

const (
	histDayTable = "hist_day"
	// histSealAge: days older than this many days (UTC) are sealed.
	histSealAge = 2

	histCoverage = 1
	histSignal   = 2
	histElev     = 3

	histCodecPlain   = 1
	histCodecDeflate = 2
)

const histDayDDL = `CREATE TABLE hist_day(
  id   INTEGER PRIMARY KEY,
  ant  TEXT NOT NULL,
  tbl  INTEGER NOT NULL,
  day  INTEGER NOT NULL,
  n    INTEGER NOT NULL,
  data BLOB NOT NULL
);
CREATE UNIQUE INDEX hist_day_key ON hist_day(ant, tbl, day);`

// histTableNames maps the tbl code to the live table.
var histTableNames = map[int]string{histCoverage: "coverage", histSignal: "signal_hist", histElev: "elev_hist"}

func init() {
	if err := RegisterMigrationFor(KindMain, histDayTable, 1, histDayDDL); err != nil {
		panic(err)
	}
	registerHourly(histDayTable, func(ctx context.Context, s *Store) error {
		_, err := s.SealHistDays(ctx, time.Now().Unix(), 0)
		return err
	})
	// TableCounts reports the logical rows: live rows plus sealed bins, so
	// sealing a day (or the 1.1 data migration) does not show as data loss.
	for tbl, name := range histTableNames {
		tbl, name := tbl, name
		registerTableCounter(name, func(ctx context.Context, tx *sql.Tx) (int64, bool, error) {
			if !txHasTable(ctx, tx, histDayTable) {
				return 0, false, nil
			}
			var live, sealed sql.NullInt64
			err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM `+name+`), (SELECT SUM(n) FROM hist_day WHERE tbl = ?)`, tbl).Scan(&live, &sealed)
			if err != nil {
				return 0, false, err
			}
			return live.Int64 + sealed.Int64, true, nil
		})
	}
}

// ------------------------------------------------------------------ codec

type histEnc struct{ b []byte }

func (e *histEnc) int(v int64)   { e.b = binary.AppendVarint(e.b, v) }
func (e *histEnc) uint(v uint64) { e.b = binary.AppendUvarint(e.b, v) }

// real writes v exactly: uvarint(zigzag(ulps) << 1) + varint(q) with
// x = q / scale and v = x moved by ulps, or uvarint(1) + 8 bytes.
func (e *histEnc) real(v, scale float64) {
	if q := math.Round(v * scale); math.Abs(q) < 1<<52 && !math.IsNaN(v) {
		x := float64(int64(q)) / scale
		if math.Signbit(x) == math.Signbit(v) {
			d := int64(math.Float64bits(v)) - int64(math.Float64bits(x))
			if d > -(1<<24) && d < 1<<24 {
				e.uint(uint64(d<<1^d>>63) << 1)
				e.int(int64(q))
				return
			}
		}
	}
	e.uint(1)
	e.b = binary.LittleEndian.AppendUint64(e.b, math.Float64bits(v))
}

var errHistCodec = errors.New("store: hist_day: corrupt data")

type histDec struct {
	b   []byte
	err error
}

func (d *histDec) int() int64 {
	v, k := binary.Varint(d.b)
	if k <= 0 {
		d.fail()
		return 0
	}
	d.b = d.b[k:]
	return v
}

func (d *histDec) uint() uint64 {
	v, k := binary.Uvarint(d.b)
	if k <= 0 {
		d.fail()
		return 0
	}
	d.b = d.b[k:]
	return v
}

func (d *histDec) fail() {
	if d.err == nil {
		d.err = errHistCodec
	}
	d.b = nil
}

func (d *histDec) real(scale float64) float64 {
	u := d.uint()
	if u == 1 {
		if len(d.b) < 8 {
			d.fail()
			return 0
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(d.b))
		d.b = d.b[8:]
		return v
	}
	z := u >> 1
	ulps := int64(z>>1) ^ -int64(z&1)
	x := float64(d.int()) / scale
	return math.Float64frombits(uint64(int64(math.Float64bits(x)) + ulps))
}

// pack wraps a payload with its codec byte, deflated when that is smaller.
func histPack(payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(histCodecDeflate)
	if w, err := flate.NewWriter(&buf, flate.BestCompression); err == nil {
		_, _ = w.Write(payload)
		if w.Close() == nil && buf.Len() < len(payload)+1 {
			return buf.Bytes()
		}
	}
	return append([]byte{histCodecPlain}, payload...)
}

func histUnpack(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errHistCodec
	}
	switch b[0] {
	case histCodecPlain:
		return b[1:], nil
	case histCodecDeflate:
		return io.ReadAll(flate.NewReader(bytes.NewReader(b[1:])))
	}
	return nil, errHistCodec
}

// The three day encoders take the rows of one antenna and one day sorted by
// their bin key.

func encodeCoverageDay(rows []CoverageRow) []byte {
	e := &histEnc{}
	e.uint(uint64(len(rows)))
	prev := 0
	for _, r := range rows {
		e.int(int64(r.BrgBin - prev))
		e.int(int64(r.AltBand))
		prev = r.BrgBin
	}
	for _, r := range rows {
		e.int(r.Count)
		e.int(r.Count - r.RSSIN)
	}
	for _, r := range rows {
		e.real(r.RSSISum, 10)
	}
	for _, r := range rows {
		e.real(r.MaxKm, 1000)
	}
	return histPack(e.b)
}

func decodeCoverageDay(day int, b []byte) ([]CoverageRow, error) {
	p, err := histUnpack(b)
	if err != nil {
		return nil, err
	}
	d := &histDec{b: p}
	n := d.uint()
	if n > 1<<20 {
		return nil, errHistCodec
	}
	rows := make([]CoverageRow, n)
	prev := 0
	for i := range rows {
		rows[i].Day = day
		rows[i].BrgBin = prev + int(d.int())
		rows[i].AltBand = int(d.int())
		prev = rows[i].BrgBin
	}
	for i := range rows {
		rows[i].Count = d.int()
		rows[i].RSSIN = rows[i].Count - d.int()
	}
	for i := range rows {
		rows[i].RSSISum = d.real(10)
	}
	for i := range rows {
		rows[i].MaxKm = d.real(1000)
	}
	return rows, d.err
}

func encodeSignalDay(rows []SignalRow) []byte {
	e := &histEnc{}
	e.uint(uint64(len(rows)))
	prevD, prevR := 0, 0
	for _, r := range rows {
		e.int(int64(r.DistBin - prevD))
		e.int(int64(r.RSSIBin - prevR))
		prevD, prevR = r.DistBin, r.RSSIBin
	}
	for _, r := range rows {
		e.int(r.Count)
	}
	return histPack(e.b)
}

func decodeSignalDay(day int, b []byte) ([]SignalRow, error) {
	p, err := histUnpack(b)
	if err != nil {
		return nil, err
	}
	d := &histDec{b: p}
	n := d.uint()
	if n > 1<<20 {
		return nil, errHistCodec
	}
	rows := make([]SignalRow, n)
	prevD, prevR := 0, 0
	for i := range rows {
		rows[i].Day = day
		rows[i].DistBin = prevD + int(d.int())
		rows[i].RSSIBin = prevR + int(d.int())
		prevD, prevR = rows[i].DistBin, rows[i].RSSIBin
	}
	for i := range rows {
		rows[i].Count = d.int()
	}
	return rows, d.err
}

func encodeElevDay(rows []ElevRow) []byte {
	e := &histEnc{}
	e.uint(uint64(len(rows)))
	prev := 0
	for _, r := range rows {
		e.int(int64(r.ElevBin - prev))
		prev = r.ElevBin
	}
	for _, r := range rows {
		e.int(r.Count)
	}
	for _, r := range rows {
		e.real(r.RSSISum, 10)
	}
	for _, r := range rows {
		e.real(r.ResidSum, 1000)
	}
	return histPack(e.b)
}

func decodeElevDay(day int, b []byte) ([]ElevRow, error) {
	p, err := histUnpack(b)
	if err != nil {
		return nil, err
	}
	d := &histDec{b: p}
	n := d.uint()
	if n > 1<<20 {
		return nil, errHistCodec
	}
	rows := make([]ElevRow, n)
	prev := 0
	for i := range rows {
		rows[i].Day = day
		rows[i].ElevBin = prev + int(d.int())
		prev = rows[i].ElevBin
	}
	for i := range rows {
		rows[i].Count = d.int()
	}
	for i := range rows {
		rows[i].RSSISum = d.real(10)
	}
	for i := range rows {
		rows[i].ResidSum = d.real(1000)
	}
	return rows, d.err
}

// ---------------------------------------------------------------- readers

// sealedDays calls fn for every sealed day d0..d1 of (tbl, ant), oldest
// first. Without hist_day (stations.db, or before the migration) it does
// nothing.
func (s *Store) sealedDays(ctx context.Context, tbl int, ant string, d0, d1 int, fn func(day int, data []byte) error) error {
	if s.kind != KindMain || !s.hasHistDay() {
		return nil
	}
	return s.queryEach(ctx, `SELECT day, data FROM hist_day WHERE ant = ? AND tbl = ? AND day >= ? AND day <= ? ORDER BY day`,
		[]any{ant, tbl, d0, d1}, func(rs *sql.Rows) error {
			var day int
			var b []byte
			if err := rs.Scan(&day, &b); err != nil {
				return err
			}
			return fn(day, b)
		})
}

var histDayStores sync.Map // *Store -> true once hist_day was seen

// hasHistDay caches that hist_day exists (it is never dropped).
func (s *Store) hasHistDay() bool {
	if _, ok := histDayStores.Load(s); ok {
		return true
	}
	if !s.HasTable(histDayTable) {
		return false
	}
	histDayStores.Store(s, true)
	return true
}

// ------------------------------------------------------------------ seal

// SealReport says what a SealHistDays call did.
type SealReport struct {
	Days int   // (table, day) pairs sealed
	Rows int64 // live rows packed and deleted
}

// SealHistDays seals the closed days of coverage, signal_hist and elev_hist
// older than histSealAge days at now, oldest first, one (table, day) per
// transaction; maxDays > 0 bounds the pairs sealed by this call.
func (s *Store) SealHistDays(ctx context.Context, now int64, maxDays int) (SealReport, error) {
	var rep SealReport
	if s.kind != KindMain || !s.hasHistDay() {
		return rep, nil
	}
	horizon := DayOf(now) - histSealAge - 1
	for maxDays <= 0 || rep.Days < maxDays {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		var n int64
		sealed := false
		err := s.Write(ctx, func(tx *sql.Tx) error {
			var err error
			n, sealed, err = sealOneDay(ctx, tx, horizon)
			return err
		})
		if err != nil {
			return rep, err
		}
		if !sealed {
			return rep, nil
		}
		rep.Days++
		rep.Rows += n
	}
	return rep, nil
}

// sealOneDay seals the oldest live day <= horizon of the first table that
// has one. sealed is false when nothing is left to seal.
func sealOneDay(ctx context.Context, tx *sql.Tx, horizon int) (rows int64, sealed bool, err error) {
	for _, tbl := range []int{histCoverage, histSignal, histElev} {
		name := histTableNames[tbl]
		var day sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(day) FROM `+name).Scan(&day); err != nil {
			return 0, false, err
		}
		if !day.Valid || day.Int64 > int64(horizon) {
			continue
		}
		n, err := sealDay(ctx, tx, tbl, int(day.Int64))
		return n, true, err
	}
	return 0, false, nil
}

// sealDay packs every antenna's rows of one table and day into hist_day
// (merging with a row sealed before) and deletes them from the live table.
func sealDay(ctx context.Context, tx *sql.Tx, tbl, day int) (int64, error) {
	name := histTableNames[tbl]
	var q string
	switch tbl {
	case histCoverage:
		q = `SELECT ant, brg_bin, alt_band, max_km, count, rssi_sum, rssi_n FROM coverage WHERE day = ? ORDER BY ant, brg_bin, alt_band`
	case histSignal:
		q = `SELECT ant, dist_bin, rssi_bin, count FROM signal_hist WHERE day = ? ORDER BY ant, dist_bin, rssi_bin`
	default:
		q = `SELECT ant, elev_bin, count, rssi_sum, resid_sum FROM elev_hist WHERE day = ? ORDER BY ant, elev_bin`
	}
	rs, err := tx.QueryContext(ctx, q, day)
	if err != nil {
		return 0, err
	}
	cov := map[string][]CoverageRow{}
	sig := map[string][]SignalRow{}
	elev := map[string][]ElevRow{}
	var ants []string
	var n int64
	for rs.Next() {
		var ant string
		switch tbl {
		case histCoverage:
			r := CoverageRow{Day: day}
			err = rs.Scan(&ant, &r.BrgBin, &r.AltBand, &r.MaxKm, &r.Count, &r.RSSISum, &r.RSSIN)
			if _, ok := cov[ant]; !ok {
				ants = append(ants, ant)
			}
			cov[ant] = append(cov[ant], r)
		case histSignal:
			r := SignalRow{Day: day}
			err = rs.Scan(&ant, &r.DistBin, &r.RSSIBin, &r.Count)
			if _, ok := sig[ant]; !ok {
				ants = append(ants, ant)
			}
			sig[ant] = append(sig[ant], r)
		default:
			r := ElevRow{Day: day}
			err = rs.Scan(&ant, &r.ElevBin, &r.Count, &r.RSSISum, &r.ResidSum)
			if _, ok := elev[ant]; !ok {
				ants = append(ants, ant)
			}
			elev[ant] = append(elev[ant], r)
		}
		if err != nil {
			rs.Close()
			return 0, err
		}
		n++
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return 0, err
	}
	for _, ant := range ants {
		var old []byte
		err := tx.QueryRowContext(ctx, `SELECT data FROM hist_day WHERE ant = ? AND tbl = ? AND day = ?`, ant, tbl, day).Scan(&old)
		if err != nil && !isNoRows(err) {
			return 0, err
		}
		var data []byte
		var cnt int
		switch tbl {
		case histCoverage:
			rows := cov[ant]
			if old != nil {
				prev, err := decodeCoverageDay(day, old)
				if err != nil {
					return 0, err
				}
				rows = coalesceCoverage(mergeSorted(prev, rows, cmpCoverage))
			}
			data, cnt = encodeCoverageDay(rows), len(rows)
		case histSignal:
			rows := sig[ant]
			if old != nil {
				prev, err := decodeSignalDay(day, old)
				if err != nil {
					return 0, err
				}
				rows = coalesceSignal(mergeSorted(prev, rows, cmpSignal))
			}
			data, cnt = encodeSignalDay(rows), len(rows)
		default:
			rows := elev[ant]
			if old != nil {
				prev, err := decodeElevDay(day, old)
				if err != nil {
					return 0, err
				}
				rows = coalesceElev(mergeSorted(prev, rows, cmpElev))
			}
			data, cnt = encodeElevDay(rows), len(rows)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hist_day(ant, tbl, day, n, data) VALUES(?, ?, ?, ?, ?)
ON CONFLICT(ant, tbl, day) DO UPDATE SET n = excluded.n, data = excluded.data`, ant, tbl, day, cnt, data); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+name+` WHERE day = ?`, day); err != nil {
		return 0, err
	}
	return n, nil
}

// coalesceCoverage merges adjacent rows with the same key (a sealed row
// followed by a later live increment) with the UPSERT rules.
func coalesceCoverage(rows []CoverageRow) []CoverageRow {
	out := rows[:0]
	for _, r := range rows {
		if k := len(out) - 1; k >= 0 && cmpCoverage(out[k], r) == 0 {
			o := &out[k]
			o.MaxKm = max(o.MaxKm, r.MaxKm)
			o.Count += r.Count
			o.RSSISum += r.RSSISum
			o.RSSIN += r.RSSIN
			continue
		}
		out = append(out, r)
	}
	return out
}

func coalesceSignal(rows []SignalRow) []SignalRow {
	out := rows[:0]
	for _, r := range rows {
		if k := len(out) - 1; k >= 0 && cmpSignal(out[k], r) == 0 {
			out[k].Count += r.Count
			continue
		}
		out = append(out, r)
	}
	return out
}

func coalesceElev(rows []ElevRow) []ElevRow {
	out := rows[:0]
	for _, r := range rows {
		if k := len(out) - 1; k >= 0 && cmpElev(out[k], r) == 0 {
			o := &out[k]
			o.Count += r.Count
			o.RSSISum += r.RSSISum
			o.ResidSum += r.ResidSum
			continue
		}
		out = append(out, r)
	}
	return out
}

// histRowCount is the number of logical rows of a histogram table: live
// rows plus the bins of its sealed days.
func (s *Store) histRowCount(ctx context.Context, tbl int) (int64, error) {
	var live, sealed sql.NullInt64
	q := `SELECT (SELECT COUNT(*) FROM ` + histTableNames[tbl] + `), 0`
	if s.kind == KindMain && s.hasHistDay() {
		q = `SELECT (SELECT COUNT(*) FROM ` + histTableNames[tbl] + `), (SELECT SUM(n) FROM hist_day WHERE tbl = ?)`
		err := s.DB().QueryRowContext(ctx, q, tbl).Scan(&live, &sealed)
		return live.Int64 + sealed.Int64, err
	}
	err := s.DB().QueryRowContext(ctx, q).Scan(&live, &sealed)
	return live.Int64 + sealed.Int64, err
}

// --------------------------------------------------------- logical tables

// histCols are the 1.0.0 columns of the three tables, in order.
var histCols = map[int][]string{
	histCoverage: {"day", "ant", "brg_bin", "alt_band", "max_km", "count", "rssi_sum", "rssi_n"},
	histSignal:   {"day", "ant", "dist_bin", "rssi_bin", "count"},
	histElev:     {"day", "ant", "elev_bin", "count", "rssi_sum", "resid_sum"},
}

// scanHistLogical streams a histogram table as 1.0.0 held it: sealed and
// live rows of every day in primary-key order (day, ant, bins), a sealed
// bin and a later live increment of the same key merged into one row.
func scanHistLogical(ctx context.Context, q Querier, tbl int, span *Span, limit int64, fn func([]any) error) error {
	name := histTableNames[tbl]
	cond, args := "", []any{}
	if span != nil {
		cond = ` AND day >= ? AND day < ?`
	}
	dq := `SELECT day FROM ` + name + ` WHERE 1` + cond + ` UNION SELECT day FROM hist_day WHERE tbl = ?` + cond + ` ORDER BY 1`
	if span != nil {
		args = append(args, span.From, span.To, tbl, span.From, span.To)
	} else {
		args = append(args, tbl)
	}
	days, err := queryInt64s(ctx, q, dq, args...)
	if err != nil {
		return err
	}
	var emitted int64
	for _, d := range days {
		day := int(d)
		rows, err := histLogicalDay(ctx, q, tbl, day)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if limit > 0 && emitted >= limit {
				return nil
			}
			if err := fn(r); err != nil {
				return err
			}
			emitted++
		}
	}
	return nil
}

// histLogicalDay returns the logical rows of one day of a histogram table.
func histLogicalDay(ctx context.Context, q Querier, tbl, day int) ([][]any, error) {
	sealed := map[string][]byte{}
	var ants []string
	rs, err := q.QueryContext(ctx, `SELECT ant, data FROM hist_day WHERE tbl = ? AND day = ?`, tbl, day)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var ant string
		var b []byte
		if err := rs.Scan(&ant, &b); err != nil {
			rs.Close()
			return nil, err
		}
		sealed[ant] = b
		ants = append(ants, ant)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	cov := map[string][]CoverageRow{}
	sig := map[string][]SignalRow{}
	elev := map[string][]ElevRow{}
	var lq string
	switch tbl {
	case histCoverage:
		lq = `SELECT ant, brg_bin, alt_band, max_km, count, rssi_sum, rssi_n FROM coverage WHERE day = ? ORDER BY ant, brg_bin, alt_band`
	case histSignal:
		lq = `SELECT ant, dist_bin, rssi_bin, count FROM signal_hist WHERE day = ? ORDER BY ant, dist_bin, rssi_bin`
	default:
		lq = `SELECT ant, elev_bin, count, rssi_sum, resid_sum FROM elev_hist WHERE day = ? ORDER BY ant, elev_bin`
	}
	rs, err = q.QueryContext(ctx, lq, day)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var ant string
		switch tbl {
		case histCoverage:
			r := CoverageRow{Day: day}
			err = rs.Scan(&ant, &r.BrgBin, &r.AltBand, &r.MaxKm, &r.Count, &r.RSSISum, &r.RSSIN)
			cov[ant] = append(cov[ant], r)
		case histSignal:
			r := SignalRow{Day: day}
			err = rs.Scan(&ant, &r.DistBin, &r.RSSIBin, &r.Count)
			sig[ant] = append(sig[ant], r)
		default:
			r := ElevRow{Day: day}
			err = rs.Scan(&ant, &r.ElevBin, &r.Count, &r.RSSISum, &r.ResidSum)
			elev[ant] = append(elev[ant], r)
		}
		if err != nil {
			rs.Close()
			return nil, err
		}
		if _, ok := sealed[ant]; !ok {
			sealed[ant] = nil
			ants = append(ants, ant)
		}
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	sortStringsAsc(ants)
	var out [][]any
	d := int64(day)
	for _, ant := range ants {
		switch tbl {
		case histCoverage:
			var prev []CoverageRow
			if b := sealed[ant]; b != nil {
				if prev, err = decodeCoverageDay(day, b); err != nil {
					return nil, err
				}
			}
			for _, r := range coalesceCoverage(mergeSorted(prev, cov[ant], cmpCoverage)) {
				out = append(out, []any{d, ant, int64(r.BrgBin), int64(r.AltBand), r.MaxKm, r.Count, r.RSSISum, r.RSSIN})
			}
		case histSignal:
			var prev []SignalRow
			if b := sealed[ant]; b != nil {
				if prev, err = decodeSignalDay(day, b); err != nil {
					return nil, err
				}
			}
			for _, r := range coalesceSignal(mergeSorted(prev, sig[ant], cmpSignal)) {
				out = append(out, []any{d, ant, int64(r.DistBin), int64(r.RSSIBin), r.Count})
			}
		default:
			var prev []ElevRow
			if b := sealed[ant]; b != nil {
				if prev, err = decodeElevDay(day, b); err != nil {
					return nil, err
				}
			}
			for _, r := range coalesceElev(mergeSorted(prev, elev[ant], cmpElev)) {
				out = append(out, []any{d, ant, int64(r.ElevBin), r.Count, r.RSSISum, r.ResidSum})
			}
		}
	}
	return out, nil
}
