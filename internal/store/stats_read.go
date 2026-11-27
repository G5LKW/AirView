package store

import (
	"cmp"
	"context"
	"database/sql"
	"math"
	"slices"
)

// Readers and merge helpers of the statistics tables owned by the
// statistics group (heat, range_hour, coverage / signal_hist / elev_hist
// and their sealed days, flights_hour). They understand every layout a
// database can be in while the data migrations of stats_migrate.go run, so
// callers never see the difference.

// ------------------------------------------------------------------ heat

// HeatTile returns the 4 x 4-cell tile of a heat cell (the first key column
// of heat on airview.db since migration "heat" 2, key (tile, cell, ant)):
// (latIdx >> 2) << 14 | (lonIdx >> 2), with cell = latIdx << 16 | lonIdx.
// A tile is ~22 km wide: a track stays on the same B-tree leaves for
// minutes instead of touching a new leaf every 5.5 km of latitude rows,
// and the cells of all antennas stay together (one position updates the
// same cell of every antenna). Measured on gen7 (DB-EFFICIENCY-DESIGN §3.6,
// docs/perf/DB-RESULTS.md): distinct heat leaves per flush -28 % (60 s),
// -41 % (300 s), -35 % (900 s) against (cell, ant); the design's
// (ant, tile16, cell) was +64 % / +8 % / -2 % because it splits the three
// antennas' copies of a cell onto three leaves. The cell values are
// unchanged.
func HeatTile(cell int64) int64 { return (cell>>18)<<14 | (cell&0xffff)>>2 }

// heatTileSQL is HeatTile in SQL of the expression x.
func heatTileSQL(x string) string {
	return "(((" + x + ") >> 18) << 14 | (((" + x + ") & 65535) >> 2))"
}

// heatMergeSet is the merge of an UPSERT into heat (shared by both layouts;
// the same rules as Batch.MergeHeat).
var heatMergeSet = `count = count + excluded.count, rssi_sum = rssi_sum + excluded.rssi_sum, rssi_n = rssi_n + excluded.rssi_n,
  ` + nullMin("min_alt") + `,
  ` + nullMax("max_alt") + `,
  last_t = MAX(last_t, excluded.last_t)`

// sqlMergeHeatTiled is the heat UPSERT of airview.db once heat is keyed
// (tile, cell, ant). It takes the same eight arguments as sqlMergeHeat
// (cell, ant, count, rssi_sum, rssi_n, min_alt, max_alt, last_t); the tile
// is computed from the cell.
var sqlMergeHeatTiled = `INSERT INTO heat(cell, ant, count, rssi_sum, rssi_n, min_alt, max_alt, last_t, tile)
VALUES(?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ` + heatTileSQL("?1") + `)
ON CONFLICT(tile, cell, ant) DO UPDATE SET
  ` + heatMergeSet

// heatMergeSQL returns the heat UPSERT for a database kind: the tiled one on
// airview.db, sqlMergeHeat (key (cell, ant) or (ant, cell)) elsewhere.
// Batch.MergeHeat uses it (DBEFF hook C -> ops.go).
func heatMergeSQL(k Kind) string {
	if k == KindMain && heatTiled {
		return sqlMergeHeatTiled
	}
	return sqlMergeHeat
}

// HeatRow is one cell of the all-time heat grid of one antenna.
type HeatRow struct {
	Cell    int64
	Count   int64
	RSSISum float64
	RSSIN   int64
	MinAlt  float64 // ft; NaN unknown
	MaxAlt  float64 // ft; NaN unknown
	LastT   int64
}

// MergeHeatInc applies an increment to a cell exactly as the heat UPSERT
// does (count / rssi sums added, altitudes rounded to whole feet, NULL
// loses in min / max, last_t max). It is the read overlay of increments
// the tracker has not written yet.
func (r *HeatRow) MergeHeatInc(in HeatInc) {
	r.Count += in.Count
	r.RSSISum += zf(in.RSSISum)
	r.RSSIN += in.RSSIN
	r.MinAlt = nullMinF(r.MinAlt, roundAlt(in.MinAlt))
	r.MaxAlt = nullMaxF(r.MaxAlt, roundAlt(in.MaxAlt))
	r.LastT = max(r.LastT, in.LastT)
}

// HeatRowOf returns the row the heat UPSERT inserts for a first increment.
func HeatRowOf(in HeatInc) HeatRow {
	r := HeatRow{Cell: in.Cell, MinAlt: math.NaN(), MaxAlt: math.NaN()}
	r.MergeHeatInc(in)
	return r
}

// roundAlt is ni for the overlay: the stored integer as a float, NaN for NULL.
func roundAlt(v float64) float64 {
	if !isNum(v) {
		return math.NaN()
	}
	const lim = 1e15
	return float64(int64(math.Round(math.Max(-lim, math.Min(lim, v)))))
}

func nullMinF(a, b float64) float64 {
	switch {
	case math.IsNaN(a):
		return b
	case math.IsNaN(b):
		return a
	}
	return min(a, b)
}

func nullMaxF(a, b float64) float64 {
	switch {
	case math.IsNaN(a):
		return b
	case math.IsNaN(b):
		return a
	}
	return max(a, b)
}

// HeatCells returns the heat cells of antenna key ant with count > 0,
// ordered by cell (the 1.0.0 order). On airview.db the key is
// (tile, cell, ant) and the rows are sorted back by cell; while migration "heat" 2 runs, rows still in the
// legacy table heat_v1 are merged in.
func (s *Store) HeatCells(ctx context.Context, ant string) ([]HeatRow, error) {
	db := s.DB()
	q := `SELECT cell, count, rssi_sum, rssi_n, min_alt, max_alt, last_t FROM heat WHERE ant = ? AND count > 0`
	args := []any{ant}
	if s.kind == KindMain && s.HasTable(heatLegacyTable) {
		q = `SELECT cell, SUM(count), SUM(rssi_sum), SUM(rssi_n), MIN(min_alt), MAX(max_alt), MAX(last_t) FROM (
  SELECT cell, count, rssi_sum, rssi_n, min_alt, max_alt, last_t FROM heat WHERE ant = ?
  UNION ALL
  SELECT cell, count, rssi_sum, rssi_n, min_alt, max_alt, last_t FROM ` + heatLegacyTable + ` WHERE ant = ?)
GROUP BY cell HAVING SUM(count) > 0`
		args = append(args, ant)
	}
	rs, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []HeatRow
	sorted := true
	for rs.Next() {
		var r HeatRow
		var lo, hi sql.NullFloat64
		if err := rs.Scan(&r.Cell, &r.Count, &r.RSSISum, &r.RSSIN, &lo, &hi, &r.LastT); err != nil {
			return nil, err
		}
		r.MinAlt, r.MaxAlt = math.NaN(), math.NaN()
		if lo.Valid {
			r.MinAlt = lo.Float64
		}
		if hi.Valid {
			r.MaxAlt = hi.Float64
		}
		if n := len(out); n > 0 && out[n-1].Cell > r.Cell {
			sorted = false
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if !sorted { // tile order: back to the cell order of 1.0.0
		slices.SortFunc(out, func(a, b HeatRow) int { return cmp.Compare(a.Cell, b.Cell) })
	}
	return out, nil
}

// OverlayHeat merges the increments of ant from pending into cells (sorted
// by cell, as HeatCells returns them) and returns the cells with count > 0,
// still sorted by cell.
func OverlayHeat(cells []HeatRow, ant string, pending []HeatInc) []HeatRow {
	var mine []HeatInc
	for i := range pending {
		if pending[i].Ant == ant {
			mine = append(mine, pending[i])
		}
	}
	if len(mine) == 0 {
		return cells
	}
	idx := make(map[int64]int, len(cells))
	for i := range cells {
		idx[cells[i].Cell] = i
	}
	added := false
	for _, in := range mine {
		if i, ok := idx[in.Cell]; ok {
			cells[i].MergeHeatInc(in)
			continue
		}
		idx[in.Cell] = len(cells)
		cells = append(cells, HeatRowOf(in))
		added = true
	}
	if added {
		slices.SortFunc(cells, func(a, b HeatRow) int { return cmp.Compare(a.Cell, b.Cell) })
	}
	return slices.DeleteFunc(cells, func(r HeatRow) bool { return r.Count <= 0 })
}

// ------------------------------------------------------------- range_hour

// MergeRangeHour combines a stored range_hour row with a later increment
// exactly as Batch.AddRangeHours does (read overlay of partial rows).
func MergeRangeHour(old, in RangeHour) RangeHour { return mergeRangeHour(old, in) }

// OverlayRangeHours merges the partial rows of ant from pending into rows
// (sorted by T, as Store.RangeHours returns them) for hours in [from, to]
// (to <= 0: open) and returns the result sorted by T.
func OverlayRangeHours(rows []RangeHour, ant string, from, to int64, pending []RangeHour) []RangeHour {
	if to <= 0 {
		to = math.MaxInt64
	}
	added := false
	for _, p := range pending {
		if p.Ant != ant {
			continue
		}
		p.T = floorTo(p.T, 3600)
		if p.T < from || p.T > to {
			continue
		}
		i, ok := slices.BinarySearchFunc(rows, p.T, func(r RangeHour, t int64) int { return cmp.Compare(r.T, t) })
		if ok {
			rows[i] = mergeRangeHour(rows[i], p)
			rows[i].T, rows[i].Ant = p.T, ant
			continue
		}
		p.DistHist = slices.Clone(p.DistHist)
		p.SectorMax = slices.Clone(p.SectorMax)
		rows = slices.Insert(rows, i, p)
		added = true
	}
	_ = added
	return rows
}

// ----------------------------------------------------- day histogram rows

// CoverageRow is one stored coverage bin (a day, a bearing bin, an altitude
// band) of one antenna.
type CoverageRow struct {
	Day     int
	BrgBin  int
	AltBand int
	MaxKm   float64
	Count   int64
	RSSISum float64
	RSSIN   int64
}

// SignalRow is one stored signal_hist bin.
type SignalRow struct {
	Day     int
	DistBin int
	RSSIBin int
	Count   int64
}

// ElevRow is one stored elev_hist bin.
type ElevRow struct {
	Day      int
	ElevBin  int
	Count    int64
	RSSISum  float64
	ResidSum float64
}

func cmpCoverage(a, b CoverageRow) int {
	if c := cmp.Compare(a.Day, b.Day); c != 0 {
		return c
	}
	if c := cmp.Compare(a.BrgBin, b.BrgBin); c != 0 {
		return c
	}
	return cmp.Compare(a.AltBand, b.AltBand)
}

func cmpSignal(a, b SignalRow) int {
	if c := cmp.Compare(a.Day, b.Day); c != 0 {
		return c
	}
	if c := cmp.Compare(a.DistBin, b.DistBin); c != 0 {
		return c
	}
	return cmp.Compare(a.RSSIBin, b.RSSIBin)
}

func cmpElev(a, b ElevRow) int {
	if c := cmp.Compare(a.Day, b.Day); c != 0 {
		return c
	}
	return cmp.Compare(a.ElevBin, b.ElevBin)
}

// OverlayCoverage merges the coverage increments of ant for days d0..d1
// from pending into rows (sorted by day, brg_bin, alt_band) exactly as the
// coverage UPSERT does, keeping the order.
func OverlayCoverage(rows []CoverageRow, ant string, d0, d1 int, pending []CoverageInc) []CoverageRow {
	for _, p := range pending {
		day := dayNumber(p.Day)
		if p.Ant != ant || day < d0 || day > d1 {
			continue
		}
		k := CoverageRow{Day: day, BrgBin: p.BrgBin, AltBand: p.AltBand}
		i, ok := slices.BinarySearchFunc(rows, k, cmpCoverage)
		if !ok {
			rows = slices.Insert(rows, i, k)
		}
		r := &rows[i]
		r.MaxKm = sqlMax(r.MaxKm, zf(p.MaxKm), ok)
		r.Count += p.Count
		r.RSSISum += zf(p.RSSISum)
		r.RSSIN += p.RSSIN
	}
	return rows
}

// sqlMax is MAX(stored, v) of the UPSERT; a new row takes v.
func sqlMax(stored, v float64, existed bool) float64 {
	if !existed {
		return v
	}
	return max(stored, v)
}

// OverlaySignal merges signal_hist increments (see OverlayCoverage).
func OverlaySignal(rows []SignalRow, ant string, d0, d1 int, pending []SignalHistInc) []SignalRow {
	for _, p := range pending {
		day := dayNumber(p.Day)
		if p.Ant != ant || day < d0 || day > d1 {
			continue
		}
		k := SignalRow{Day: day, DistBin: p.DistBin, RSSIBin: p.RSSIBin}
		i, ok := slices.BinarySearchFunc(rows, k, cmpSignal)
		if !ok {
			rows = slices.Insert(rows, i, k)
		}
		rows[i].Count += p.Count
	}
	return rows
}

// OverlayElev merges elev_hist increments (see OverlayCoverage).
func OverlayElev(rows []ElevRow, ant string, d0, d1 int, pending []ElevHistInc) []ElevRow {
	for _, p := range pending {
		day := dayNumber(p.Day)
		if p.Ant != ant || day < d0 || day > d1 {
			continue
		}
		k := ElevRow{Day: day, ElevBin: p.ElevBin}
		i, ok := slices.BinarySearchFunc(rows, k, cmpElev)
		if !ok {
			rows = slices.Insert(rows, i, k)
		}
		r := &rows[i]
		r.Count += p.Count
		r.RSSISum += zf(p.RSSISum)
		r.ResidSum += zf(p.ResidSum)
	}
	return rows
}

// dayCondFor returns the WITH prefix, the condition selecting days d0..d1
// of a day-keyed table and its arguments (WITH arguments first): the plain
// range on airview.db, one key probe per day on stations.db (SeriesIn).
func (s *Store) dayCondFor(d0, d1 int) (with, cond string, wargs, cargs []any) {
	if s.kind != KindStations {
		return "", "day >= ? AND day <= ?", nil, []any{d0, d1}
	}
	with, cond, wargs = SeriesIn("dd", "day", 1, int64(d0), int64(d1))
	return with, cond, wargs, nil
}

func (s *Store) queryEach(ctx context.Context, q string, args []any, fn func(*sql.Rows) error) error {
	rs, err := s.DB().QueryContext(ctx, q, args...)
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

func concatArgs(parts ...[]any) []any {
	var out []any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// CoverageRows returns the coverage bins of antenna key ant for days
// d0..d1 ordered by (day, brg_bin, alt_band): live rows, sealed days
// (hist_day) and the pending increments (nil = none) merged as the UPSERT
// would merge them.
func (s *Store) CoverageRows(ctx context.Context, ant string, d0, d1 int, pending []CoverageInc) ([]CoverageRow, error) {
	var out []CoverageRow
	if err := s.sealedDays(ctx, histCoverage, ant, d0, d1, func(day int, b []byte) error {
		rows, err := decodeCoverageDay(day, b)
		out = append(out, rows...)
		return err
	}); err != nil {
		return nil, err
	}
	with, cond, wargs, cargs := s.dayCondFor(d0, d1)
	var live []CoverageRow
	err := s.queryEach(ctx, with+`SELECT day, brg_bin, alt_band, max_km, count, rssi_sum, rssi_n FROM coverage
WHERE ant = ? AND `+cond+` ORDER BY day, brg_bin, alt_band`, concatArgs(wargs, []any{ant}, cargs), func(rs *sql.Rows) error {
		var r CoverageRow
		if err := rs.Scan(&r.Day, &r.BrgBin, &r.AltBand, &r.MaxKm, &r.Count, &r.RSSISum, &r.RSSIN); err != nil {
			return err
		}
		live = append(live, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	out = coalesceCoverage(mergeSorted(out, live, cmpCoverage))
	return OverlayCoverage(out, ant, d0, d1, pending), nil
}

// mergeSorted merges two sorted row lists (sealed and live days never
// overlap; should they, both rows are kept in order).
func mergeSorted[T any](a, b []T, c func(T, T) int) []T {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]T, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if c(a[i], b[j]) <= 0 {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return append(append(out, a[i:]...), b[j:]...)
}

// CoverageBand is the coverage of one altitude band over a day range.
type CoverageBand struct {
	AltBand int
	Count   int64
	MaxKm   float64
}

// CoverageBands returns per altitude band the summed count and the largest
// max_km of antenna key ant over days d0..d1 (bands without rows are
// missing), ordered by band; sealed days and pending increments included.
func (s *Store) CoverageBands(ctx context.Context, ant string, d0, d1 int, pending []CoverageInc) ([]CoverageBand, error) {
	acc := map[int]*CoverageBand{}
	add := func(band int, n int64, km float64) {
		b := acc[band]
		if b == nil {
			acc[band] = &CoverageBand{AltBand: band, Count: n, MaxKm: km}
			return
		}
		b.Count += n
		b.MaxKm = max(b.MaxKm, km)
	}
	if err := s.sealedDays(ctx, histCoverage, ant, d0, d1, func(day int, b []byte) error {
		rows, err := decodeCoverageDay(day, b)
		for _, r := range rows {
			add(r.AltBand, r.Count, r.MaxKm)
		}
		return err
	}); err != nil {
		return nil, err
	}
	with, cond, wargs, cargs := s.dayCondFor(d0, d1)
	err := s.queryEach(ctx, with+`SELECT alt_band, SUM(count), MAX(max_km) FROM coverage
WHERE ant = ? AND `+cond+` GROUP BY alt_band`, concatArgs(wargs, []any{ant}, cargs), func(rs *sql.Rows) error {
		var b int
		var n sql.NullInt64
		var km sql.NullFloat64
		if err := rs.Scan(&b, &n, &km); err != nil {
			return err
		}
		if !km.Valid {
			km.Float64 = math.Inf(-1)
		}
		add(b, n.Int64, km.Float64)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, p := range pending {
		if day := dayNumber(p.Day); p.Ant == ant && day >= d0 && day <= d1 {
			add(p.AltBand, p.Count, zf(p.MaxKm))
		}
	}
	out := make([]CoverageBand, 0, len(acc))
	for _, b := range acc {
		out = append(out, *b)
	}
	slices.SortFunc(out, func(a, b CoverageBand) int { return cmp.Compare(a.AltBand, b.AltBand) })
	return out, nil
}

// SignalTotals returns per (dist_bin, rssi_bin) the summed count of
// antenna key ant over days d0..d1 (Day = 0), ordered by (dist_bin,
// rssi_bin); sealed days and pending increments included.
func (s *Store) SignalTotals(ctx context.Context, ant string, d0, d1 int, pending []SignalHistInc) ([]SignalRow, error) {
	acc := map[[2]int]int64{}
	if err := s.sealedDays(ctx, histSignal, ant, d0, d1, func(day int, b []byte) error {
		rows, err := decodeSignalDay(day, b)
		for _, r := range rows {
			acc[[2]int{r.DistBin, r.RSSIBin}] += r.Count
		}
		return err
	}); err != nil {
		return nil, err
	}
	with, cond, wargs, cargs := s.dayCondFor(d0, d1)
	err := s.queryEach(ctx, with+`SELECT dist_bin, rssi_bin, SUM(count) FROM signal_hist
WHERE ant = ? AND `+cond+` GROUP BY dist_bin, rssi_bin`, concatArgs(wargs, []any{ant}, cargs), func(rs *sql.Rows) error {
		var db, rb int
		var n int64
		if err := rs.Scan(&db, &rb, &n); err != nil {
			return err
		}
		acc[[2]int{db, rb}] += n
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, p := range pending {
		if day := dayNumber(p.Day); p.Ant == ant && day >= d0 && day <= d1 {
			acc[[2]int{p.DistBin, p.RSSIBin}] += p.Count
		}
	}
	out := make([]SignalRow, 0, len(acc))
	for k, n := range acc {
		out = append(out, SignalRow{DistBin: k[0], RSSIBin: k[1], Count: n})
	}
	slices.SortFunc(out, cmpSignal)
	return out, nil
}

// ElevTotals returns per elevation bin the summed count, rssi_sum and
// resid_sum of antenna key ant over days d0..d1 (Day = 0), ordered by bin;
// sealed days and pending increments included. The REAL sums use the
// compensated summation of SQLite's SUM() in day order.
func (s *Store) ElevTotals(ctx context.Context, ant string, d0, d1 int, pending []ElevHistInc) ([]ElevRow, error) {
	var rows []ElevRow
	if err := s.sealedDays(ctx, histElev, ant, d0, d1, func(day int, b []byte) error {
		r, err := decodeElevDay(day, b)
		rows = append(rows, r...)
		return err
	}); err != nil {
		return nil, err
	}
	with, cond, wargs, cargs := s.dayCondFor(d0, d1)
	var live []ElevRow
	err := s.queryEach(ctx, with+`SELECT day, elev_bin, count, rssi_sum, resid_sum FROM elev_hist
WHERE ant = ? AND `+cond+` ORDER BY day, elev_bin`, concatArgs(wargs, []any{ant}, cargs), func(rs *sql.Rows) error {
		var r ElevRow
		if err := rs.Scan(&r.Day, &r.ElevBin, &r.Count, &r.RSSISum, &r.ResidSum); err != nil {
			return err
		}
		live = append(live, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows = OverlayElev(coalesceElev(mergeSorted(rows, live, cmpElev)), ant, d0, d1, pending)
	type accum struct {
		n           int64
		rssi, resid kbnSum
	}
	acc := map[int]*accum{}
	for _, r := range rows {
		a := acc[r.ElevBin]
		if a == nil {
			a = &accum{}
			acc[r.ElevBin] = a
		}
		a.n += r.Count
		a.rssi.add(r.RSSISum)
		a.resid.add(r.ResidSum)
	}
	out := make([]ElevRow, 0, len(acc))
	for b, a := range acc {
		out = append(out, ElevRow{ElevBin: b, Count: a.n, RSSISum: a.rssi.sum(), ResidSum: a.resid.sum()})
	}
	slices.SortFunc(out, cmpElev)
	return out, nil
}

// kbnSum is the Kahan-Babuska-Neumaier summation SQLite's SUM() uses for
// REAL values, so a sum taken in Go equals the one SQL returns for the same
// values in the same order.
type kbnSum struct{ s, err float64 }

func (k *kbnSum) add(r float64) {
	t := k.s + r
	if math.Abs(k.s) > math.Abs(r) {
		k.err += (k.s - t) + r
	} else {
		k.err += (r - t) + k.s
	}
	k.s = t
}

func (k *kbnSum) sum() float64 {
	if math.IsInf(k.err, 0) || math.IsNaN(k.err) {
		return k.s
	}
	return k.s + k.err
}
