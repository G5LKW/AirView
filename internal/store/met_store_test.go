package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"airview/internal/model"
)

// Tests of met_sample (met_store.go), data migration met/2 and the logical
// table met_obs (met_migrate.go), the weather diff writers and QueueTx
// (ops_net.go): the tables of other packages, DB-EFFICIENCY-DESIGN §3.9,
// §3.20, §5.2.

func msOpen(t *testing.T, dir string) *Store {
	t.Helper()
	st, err := OpenWithOptions(dir, Options{MigrationDelay: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// msObs is a collector-like observation: wind and temperature rounded to
// 0.1 (internal/met round1), lat/lon full precision, NaN = unknown.
func msObs(r *rand.Rand, t int64, icao uint32) MetObs {
	o := MetObs{T: t, ICAO: icao, Lat: 47 + 6*r.Float64(), Lon: 5 + 10*r.Float64(), Alt: float64(1000 + r.IntN(40000)),
		WindDir: math.NaN(), WindKt: math.NaN(), OATC: math.NaN(), Src: uint8(r.IntN(4))}
	if r.IntN(10) > 0 {
		o.WindDir, o.WindKt = math.Round(r.Float64()*3600)/10, math.Round(r.Float64()*1800)/10
	}
	if r.IntN(10) > 1 {
		o.OATC = math.Round((r.Float64()*80-60)*10) / 10
	}
	if r.IntN(50) == 0 {
		o.Lat, o.Lon = math.NaN(), math.NaN()
	}
	return o
}

func sameF(a, b, tol float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	return math.Abs(a-b) <= tol
}

// msSame compares at the resolution of met_sample: lat/lon 1e-5 degrees,
// everything else exact.
func msSame(a, b MetObs) bool {
	return a.T == b.T && a.ICAO == b.ICAO && a.Src == b.Src && sameF(a.Lat, b.Lat, 0.5e-5+1e-12) &&
		sameF(a.Lon, b.Lon, 0.5e-5+1e-12) && sameF(a.Alt, b.Alt, 0) && sameF(a.WindDir, b.WindDir, 0) &&
		sameF(a.WindKt, b.WindKt, 0) && sameF(a.OATC, b.OATC, 0)
}

func msCount(t *testing.T, st *Store, q string) int64 {
	t.Helper()
	var n int64
	if err := st.DB().QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMetSampleRoundTripAndCounter(t *testing.T) {
	ctx := context.Background()
	st := msOpen(t, t.TempDir())
	r := rand.New(rand.NewPCG(1, 1))
	var in []MetObs
	for i := range 3000 {
		in = append(in, msObs(r, 1_700_000_000+int64(i/30)*60, uint32(0x400000+i%30)))
	}
	st.AddMetSamples(in[:2000])
	st.AddMetSamples(in[2000:])
	// A second observation of the same aircraft in the same second wins.
	dup := in[10]
	dup.OATC, dup.Src = -42.5, 3
	st.AddMetSamples([]MetObs{dup})
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	in[10] = dup
	got, err := st.MetSamplesSince(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in) {
		t.Fatalf("%d rows back, want %d", len(got), len(in))
	}
	for i := range got {
		if !msSame(got[i], in[i]) {
			t.Fatalf("row %d: got %+v want %+v", i, got[i], in[i])
		}
	}
	if n, _ := metSampleCount(ctx, st.DB()); n != int64(len(in)) || msCount(t, st, `SELECT COUNT(*) FROM met_sample`) != n {
		t.Fatalf("counter %d, table %d, want %d", n, msCount(t, st, `SELECT COUNT(*) FROM met_sample`), len(in))
	}
	// The hourly roll moves the watermark; later rows above it, an
	// out-of-order row below it and a replacement keep the count exact.
	if _, err := st.HourlyMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	if c, _ := readMetCount(ctx, st.DB()); c.N != int64(len(in)) || c.Hi < 0 {
		t.Fatalf("after the hourly roll: %+v", c)
	}
	late := msObs(r, 1_700_000_000+3, 0x7ABCDE) // older than the watermark
	newer := msObs(r, 1_800_000_000, 0x7ABCDE)
	st.AddMetSamples([]MetObs{late, newer, in[5]}) // in[5]: replaces itself
	_ = st.Flush()
	in = append(in, late, newer)
	if n, _ := metSampleCount(ctx, st.DB()); n != int64(len(in)) || msCount(t, st, `SELECT COUNT(*) FROM met_sample`) != n {
		t.Fatalf("counter %d after late rows, table %d", n, msCount(t, st, `SELECT COUNT(*) FROM met_sample`))
	}
	in = in[:len(in)-2]
	_ = st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM met_sample WHERE icao = ?`, 0x7ABCDE)
		return err
	})
	// (a foreign delete drifts the counter; the store never does this)
	_ = st.Write(ctx, func(tx *sql.Tx) error {
		c, _ := readMetCount(ctx, tx)
		c.N--
		return writeMetCount(ctx, tx, c)
	})
	// from / limit
	part, _ := st.MetSamplesSince(ctx, 1_700_000_000+50*60, 7)
	if len(part) != 7 || part[0].T != 1_700_000_000+50*60 {
		t.Fatalf("since: %d rows from %d", len(part), part[0].T)
	}
	counts, err := st.TableCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["met_obs"] != int64(len(in)) || counts[MetSampleTable] != int64(len(in)) {
		t.Fatalf("table counts met_obs %d met_sample %d", counts["met_obs"], counts[MetSampleTable])
	}
}

// addLegacyMetObs queues rows into the 1.0.0 met_obs table, as the 1.0.0
// Batch.AddMetObs did (the store itself now writes met_sample).
func addLegacyMetObs(b *Batch, rows []MetObs) {
	rows = slices.Clone(rows)
	b.add("met_obs", tierLow, len(rows), func(tc *txc) error {
		st, err := tc.stmt(sqlInsertMetObs)
		if err != nil {
			return err
		}
		for i := range rows {
			r := &rows[i]
			if _, err := st.ExecContext(tc.ctx, r.T, int64(r.ICAO), nf(r.Lat), nf(r.Lon), ni(r.Alt),
				nf(r.WindDir), nf(r.WindKt), nf(r.OATC), int64(r.Src)); err != nil {
				return err
			}
		}
		return nil
	})
}

// msLegacyInsert writes rows the way 1.0.0 did (into met_obs).
func msLegacyInsert(t *testing.T, st *Store, rows []MetObs) {
	t.Helper()
	b := NewBatch()
	addLegacyMetObs(b, rows)
	if err := st.Apply(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

// msLogical reads the logical met_obs table (export form).
func msLogical(t *testing.T, st *Store, span *Span) [][]any {
	t.Helper()
	var out [][]any
	err := st.ScanTable(context.Background(), "met_obs", "t", span, 0, func(cols []string) error {
		if fmt.Sprint(cols) != fmt.Sprint(metObsColumns) {
			return fmt.Errorf("columns %v", cols)
		}
		return nil
	}, func(v []any) error {
		out = append(out, append([]any(nil), v...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// msPhysical reads 1.0.0 met_obs as `SELECT *` returned it.
func msPhysical(t *testing.T, st *Store) [][]any {
	t.Helper()
	rows, err := st.DB().Query(`SELECT * FROM met_obs ORDER BY t, rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]any
	if err := scanRowsAny(rows, 9, func(v []any) error {
		out = append(out, append([]any(nil), v...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// msRowsEqual compares exported rows: same types, lat/lon (columns 2, 3)
// within half a unit of the 5th decimal, everything else exact.
func msRowsEqual(a, b []any) bool {
	for i := range a {
		if fmt.Sprintf("%T", a[i]) != fmt.Sprintf("%T", b[i]) {
			return false
		}
		if (i == 2 || i == 3) && a[i] != nil {
			if math.Abs(a[i].(float64)-b[i].(float64)) > 0.5e-5+1e-12 {
				return false
			}
			continue
		}
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMetMigrationFromLegacyTable(t *testing.T) {
	ctx := context.Background()
	st := msOpen(t, t.TempDir())
	r := rand.New(rand.NewPCG(2, 2))
	var legacy []MetObs
	for i := range 45_000 { // three steps of 20,000
		legacy = append(legacy, msObs(r, 1_700_000_000+int64(i/40)*60+int64(i%7), uint32(0x300000+i%40)))
	}
	msLegacyInsert(t, st, legacy)
	want := msPhysical(t, st)
	before, err := st.MetSamplesSince(ctx, 0, 1<<30)
	if err != nil || len(before) != len(legacy) {
		t.Fatalf("legacy read: %d rows, %v", len(before), err)
	}
	// Live rows written after the upgrade, newer than the legacy ones.
	var live []MetObs
	for i := range 500 {
		live = append(live, msObs(r, 1_700_100_000+int64(i/10)*60, uint32(0x500000+i%10)))
	}
	st.AddMetSamples(live)
	_ = st.Flush()

	// One step by hand: the readers see both layouts.
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, _, err := stepMetMigration(ctx, tx, nil, 20_000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	mid, err := st.MetSamplesSince(ctx, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if len(mid) != len(legacy)+len(live) {
		t.Fatalf("mid-migration read: %d rows, want %d", len(mid), len(legacy)+len(live))
	}
	for i := range legacy {
		if mid[i].T != before[i].T {
			t.Fatalf("mid-migration order at %d", i)
		}
	}
	if n := len(msLogical(t, st, nil)); n != len(legacy)+len(live) {
		t.Fatalf("logical rows mid-migration: %d", n)
	}
	counts, _ := st.TableCounts(ctx)
	if counts["met_obs"] != int64(len(legacy)+len(live)) {
		t.Fatalf("met_obs count mid-migration %d", counts["met_obs"])
	}

	if err := st.MigrateNow(ctx); err != nil {
		t.Fatal(err)
	}
	if !st.DataMigrationDone("met") {
		t.Fatal("met migration not done")
	}
	if n := msCount(t, st, `SELECT COUNT(*) FROM met_obs`); n != 0 {
		t.Fatalf("%d legacy rows left", n)
	}
	if n := msCount(t, st, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'met_obs_t'`); n != 0 {
		t.Fatal("met_obs_t still exists")
	}
	if n, _ := metSampleCount(ctx, st.DB()); n != int64(len(legacy)+len(live)) {
		t.Fatalf("counter %d", n)
	}
	// Every 1.0.0 row is there, at the resolution of met_sample, with the
	// 1.0.0 value types.
	got := msLogical(t, st, nil)
	if len(got) != len(want)+len(live) {
		t.Fatalf("logical rows %d want %d", len(got), len(want)+len(live))
	}
	byKey := map[[2]int64][]any{}
	for _, g := range got {
		byKey[[2]int64{g[0].(int64), g[1].(int64)}] = g
	}
	for _, w := range want {
		g := byKey[[2]int64{w[0].(int64), w[1].(int64)}]
		if g == nil || !msRowsEqual(w, g) {
			t.Fatalf("row %v migrated as %v", w, g)
		}
	}
	after, err := st.MetSamplesSince(ctx, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	for i := range before {
		// Same second: met_sample orders by ICAO, 1.0.0 by insertion.
		j := i
		for j < len(after) && (after[j].T != before[i].T || after[j].ICAO != before[i].ICAO) {
			j++
		}
		if j == len(after) {
			for j = 0; j < len(after) && (after[j].T != before[i].T || after[j].ICAO != before[i].ICAO); j++ {
			}
		}
		if j == len(after) || !msSame(after[j], before[i]) {
			t.Fatalf("observation %+v changed by the migration", before[i])
		}
	}
	// Span filter of the export.
	span := &Span{From: 1_700_000_000 + 600, To: 1_700_000_000 + 1200}
	for _, row := range msLogical(t, st, span) {
		if tv := row[0].(int64); tv < span.From || tv >= span.To {
			t.Fatalf("span row %d", tv)
		}
	}
	// Reopening keeps working (the stub met_obs keeps 1.0.0 statements
	// preparing).
	dir := st.dir
	_ = st.Close()
	st2 := msOpen(t, dir)
	if n, _ := metSampleCount(ctx, st2.DB()); n != int64(len(legacy)+len(live)) {
		t.Fatalf("counter after reopen %d", n)
	}
}

func TestMetSamplePruner(t *testing.T) {
	ctx := context.Background()
	st := msOpen(t, t.TempDir())
	now := time.Now().Unix()
	r := rand.New(rand.NewPCG(3, 3))
	var rows []MetObs
	for i := range 12_000 {
		rows = append(rows, msObs(r, now-40*86400+int64(i)*240, uint32(i%7+1)))
	}
	st.AddMetSamples(rows)
	_ = st.Flush()
	var old int64
	for _, o := range rows {
		if o.T < now-30*86400 {
			old++
		}
	}
	res, err := st.Prune(ctx, model.StorageConfig{TrailRetentionDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if res.MetObs != old {
		t.Fatalf("pruned %d, want %d", res.MetObs, old)
	}
	if n, _ := metSampleCount(ctx, st.DB()); n != int64(len(rows))-old || msCount(t, st, `SELECT COUNT(*) FROM met_sample`) != n {
		t.Fatalf("counter %d after prune", n)
	}
}

// TestMetSampleBytesPerRow measures the on-disk size of a realistic
// two-hour sample stream (one observation per aircraft per minute, 250
// aircraft) with dbstat.
func TestMetSampleBytesPerRow(t *testing.T) {
	st := msOpen(t, t.TempDir())
	var one int
	if err := st.DB().QueryRow(`SELECT 1 FROM dbstat LIMIT 1`).Scan(&one); err != nil {
		t.Skip("dbstat not available:", err)
	}
	r := rand.New(rand.NewPCG(4, 4))
	t0 := int64(1_763_000_000)
	n := 0
	for m := range 120 { // two hours, one flush per 10 s as on a station
		var batch []MetObs
		for a := range 250 {
			batch = append(batch, msObs(r, t0+int64(m)*60+int64(a%60), uint32(0x380000+a*977)))
		}
		slices.SortFunc(batch, func(a, b MetObs) int { return cmpKey64(a.T, b.T) })
		for i := 0; i < len(batch); i += 42 {
			st.AddMetSamples(batch[i:min(i+42, len(batch))])
			_ = st.Flush()
		}
		n += len(batch)
	}
	var pg, payload int64
	if err := st.DB().QueryRow(`SELECT SUM(pgsize), SUM(payload) FROM dbstat WHERE name = 'met_sample'`).Scan(&pg, &payload); err != nil {
		t.Fatal(err)
	}
	perRow := float64(pg) / float64(n)
	t.Logf("met_sample: %d rows, %d pages bytes = %.1f B/row (payload %.1f B/row)", n, pg, perRow, float64(payload)/float64(n))
	if perRow > 32 {
		t.Errorf("met_sample uses %.1f B/row, want <= 32", perRow)
	}
}

// ---------------------------------------------------------------- weather

func wxRow(t, issued int64, temp float64, profile string) Weather {
	w := NewWeather(t)
	w.Issued = issued
	w.TempC, w.RH, w.PressureMSL, w.WeatherCode, w.IsDay = temp, 81, 1013.2, 3, 1
	w.ProfileJSON = profile
	return w
}

func wxForecast(h0, n, issued int64, bump map[int64]float64) []Weather {
	var out []Weather
	for i := range n {
		t := h0 + i*3600
		out = append(out, wxRow(t, issued, 10+float64(t/3600%17)/10+bump[t], fmt.Sprintf(`{"levels":[%d]}`, t/3600%5)))
	}
	return out
}

func wxAll(t *testing.T, st *Store, overlay bool) []Weather {
	t.Helper()
	var rows []Weather
	var err error
	if overlay {
		rows, err = st.ForecastRows(context.Background(), 0, 0)
	} else {
		rows, err = st.queryWeather(context.Background(), "weather_fc", 0, 0) // the stored rows
	}
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func wxEqual(a, b []Weather) error {
	if len(a) != len(b) {
		return fmt.Errorf("%d rows vs %d", len(a), len(b))
	}
	for i := range a {
		x, y := a[i], b[i]
		jx, _ := x.MarshalJSON()
		jy, _ := y.MarshalJSON()
		if string(jx) != string(jy) {
			return fmt.Errorf("row %d:\n %s\n %s", i, jx, jy)
		}
	}
	return nil
}

// TestWeatherForecastDiffEqualsFullWrite runs the same sequence of forecast
// issues through the 1.0.0 writer and the diff writer: every reader answer
// is equal, and an unchanged forecast writes almost nothing.
func TestWeatherForecastDiffEqualsFullWrite(t *testing.T) {
	ctx := context.Background()
	full := msOpen(t, t.TempDir())
	diff := msOpen(t, t.TempDir())
	h := int64(1_763_000_000) / 3600 * 3600
	type issue struct {
		rows []Weather
	}
	var seq []issue
	issued := h - 600
	add := func(from, n int64, bump map[int64]float64) {
		issued += 900
		seq = append(seq, issue{wxForecast(from, n, issued, bump)})
	}
	add(h+3600, 168, nil)                                   // first issue
	add(h+3600, 168, nil)                                   // same hours, unchanged values
	add(h+2*3600, 168, nil)                                 // next hour: window shifts
	add(h+2*3600, 168, map[int64]float64{h + 5*3600: 0.3})  // one hour changed
	add(h+3*3600, 150, nil)                                 // shorter horizon
	add(h+3*3600, 170, map[int64]float64{h + 160*3600: -1}) // longer horizon
	add(h+3*3600, 1, nil)                                   // a single hour
	gap := wxForecast(h+4*3600, 10, issued+900, nil)        // not consecutive:
	gap = append(gap[:3], gap[4:]...)                       // falls back to a full write
	seq = append(seq, issue{gap})
	issued += 900
	add(h+4*3600, 168, nil) // and back to the diff path
	for i, is := range seq {
		b := NewBatch()
		b.SetWeatherForecast(is.rows)
		if err := full.Apply(ctx, b); err != nil {
			t.Fatal(err)
		}
		before := wxAll(t, diff, false)
		b = NewBatch()
		b.SetWeatherForecastDiff(is.rows)
		if err := diff.Apply(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := wxEqual(wxAll(t, full, false), wxAll(t, diff, true)); err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
		// Rows physically written by this issue: changed rows.
		after := wxAll(t, diff, false)
		old := map[int64]Weather{}
		for _, w := range before {
			old[w.T] = w
		}
		written := 0
		for _, w := range after {
			o, ok := old[w.T]
			jo, _ := o.MarshalJSON()
			jw, _ := w.MarshalJSON()
			if !ok || string(jo) != string(jw) {
				written++
			}
		}
		t.Logf("issue %d: %d rows, %d written (1.0.0: %d)", i, len(is.rows), written, len(is.rows))
		if i == 1 && written != 0 {
			t.Errorf("unchanged forecast wrote %d rows", written)
		}
		if i == 2 && written > len(is.rows)/5 {
			t.Errorf("shifted unchanged forecast wrote %d of %d rows", written, len(is.rows))
		}
	}
	// The table export (logical weather_fc) equals the 1.0.0 table.
	dump := func(st *Store) []string {
		var out []string
		err := st.ScanTable(ctx, "weather_fc", "t", nil, 0, func([]string) error { return nil }, func(v []any) error {
			out = append(out, fmt.Sprint(v...))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if a, b := dump(full), dump(diff); fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("weather_fc export differs: %v / %v", a[:2], b[:2])
	}
	// Single-hour readers (the /api/weather current row) agree too.
	for _, tt := range []int64{h + 3600, h + 4*3600, h + 100*3600, h + 172*3600} {
		a, _ := full.WeatherForecast(ctx, tt, tt)
		b, _ := diff.ForecastRows(ctx, tt, tt)
		if err := wxEqual(a, b); err != nil {
			t.Fatalf("hour %d: %v", tt, err)
		}
	}
}

func TestWeatherObsDiffEqualsMerge(t *testing.T) {
	ctx := context.Background()
	full := msOpen(t, t.TempDir())
	diff := msOpen(t, t.TempDir())
	h := int64(1_763_000_000) / 3600 * 3600
	r := rand.New(rand.NewPCG(5, 5))
	for round := range 30 {
		var rows []Weather
		for i := range 48 {
			w := wxRow(h+int64(i+round/3)*3600, 0, 5+float64(i%9), "")
			switch r.IntN(6) {
			case 0:
				w.TempC = math.NaN() // missing: keeps the stored value
			case 1:
				w.RH = float64(r.IntN(100))
			case 2:
				w.ProfileJSON = fmt.Sprintf(`{"p":%d}`, r.IntN(3))
			}
			rows = append(rows, w)
		}
		b := NewBatch()
		b.AddWeatherObs(rows)
		if err := full.Apply(ctx, b); err != nil {
			t.Fatal(err)
		}
		b = NewBatch()
		b.AddWeatherObsDiff(rows)
		if err := diff.Apply(ctx, b); err != nil {
			t.Fatal(err)
		}
		a, _ := full.WeatherObs(ctx, 0, 0)
		d, _ := diff.WeatherObs(ctx, 0, 0)
		if err := wxEqual(a, d); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}

func TestQueueTxRidesTheGroupCommit(t *testing.T) {
	ctx := context.Background()
	st := msOpen(t, t.TempDir())
	before := st.Stats().Flushes
	for i := range 5 {
		st.QueueTx("test", 1, func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO kv(key, value, updated) VALUES(?, 'x', 0)`, fmt.Sprint("qtx.", i))
			return err
		})
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	if n := msCount(t, st, `SELECT COUNT(*) FROM kv WHERE key LIKE 'qtx.%'`); n != 5 {
		t.Fatalf("%d rows", n)
	}
	if d := st.Stats().Flushes - before; d < 1 || d > 2 {
		t.Fatalf("%d commits for 5 queued transactions", d)
	}
	_ = ctx
}

// TestMetWALPerHour replays one hour of a station's met writes (250
// aircraft, one stored observation per aircraft and minute, the collector
// handing its queue over every 15 s, the store committing every 10 s) with
// the 1.0.0 write pattern (met_obs rows plus met_profile UPSERTs every 15
// s) and with the new one (met_sample rows, met_profile once per closed
// hour), and counts the WAL bytes each appends. Nothing checkpoints during
// the run, so the WAL file size is the total.
func TestMetWALPerHour(t *testing.T) {
	if testing.Short() {
		t.Skip("WAL measurement")
	}
	run := func(legacy bool) (frames int64, rows int) {
		dir := t.TempDir()
		st, err := OpenWithOptions(dir, Options{MigrationDelay: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Profile: Profile{Name: ProfileStandard, WALHardCapPages: 1 << 30, CheckpointSoftBytes: 1 << 40,
				CheckpointInterval: 24 * time.Hour}})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		r := rand.New(rand.NewPCG(9, 9))
		t0 := int64(1_763_000_000) / 3600 * 3600
		const aircraft = 250
		var pendObs []MetObs
		prof := map[int]*MetProfileInc{}
		for sec := int64(0); sec < 3600; sec += 5 {
			now := t0 + sec
			// Observations: aircraft a stores at second a%60 of each minute.
			for a := range aircraft {
				if (now-t0)%60 >= int64(a%60) && (now-t0)%60 < int64(a%60)+5 {
					o := msObs(r, now, uint32(0x380000+a*977))
					o.Src = 0
					pendObs = append(pendObs, o)
					lvl := int(o.Alt / 1000)
					p := prof[lvl]
					if p == nil {
						p = &MetProfileInc{Hour: t0, Level: lvl}
						prof[lvl] = p
					}
					p.USum, p.VSum, p.TSum, p.NWind, p.NTemp = p.USum+1.5, p.VSum-2, p.TSum-20, p.NWind+1, p.NTemp+1
				}
			}
			if sec%15 == 10 { // collector flush
				b := NewBatch()
				if legacy {
					addLegacyMetObs(b, pendObs)
					var incs []MetProfileInc
					for _, p := range prof {
						incs = append(incs, *p)
					}
					b.MergeMetProfile(incs)
					clear(prof)
				} else {
					b.AddMetSamples(pendObs)
				}
				rows += len(pendObs)
				pendObs = nil
				st.Submit(b)
			}
			if sec%10 == 5 { // group commit
				if err := st.Flush(); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !legacy { // the hour closes: one UPSERT per level
			var incs []MetProfileInc
			for _, p := range prof {
				incs = append(incs, *p)
			}
			st.MergeMetProfile(incs)
		}
		if err := st.Flush(); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(dir, "airview.db-wal"))
		if err != nil {
			t.Fatal(err)
		}
		return (fi.Size() - 32) / (4096 + 24), rows
	}
	// The empty store itself (schema) is in the WAL too: subtract it.
	base := func() int64 {
		dir := t.TempDir()
		st, err := OpenWithOptions(dir, Options{MigrationDelay: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Profile: Profile{Name: ProfileStandard, WALHardCapPages: 1 << 30, CheckpointSoftBytes: 1 << 40,
				CheckpointInterval: 24 * time.Hour}})
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Flush()
		fi, _ := os.Stat(filepath.Join(dir, "airview.db-wal"))
		_ = st.Close()
		if fi == nil {
			return 0
		}
		return (fi.Size() - 32) / (4096 + 24)
	}()
	oldF, n := run(true)
	newF, _ := run(false)
	mb := func(f int64) float64 { return float64(f-base) * (4096 + 24) / 1e6 }
	t.Logf("one hour, %d observations: 1.0.0 pattern %.2f MB of WAL, new %.2f MB (schema frames %d excluded)", n, mb(oldF), mb(newF), base)
	if mb(newF) > 3 {
		t.Errorf("met WAL %.2f MB/h, want <= 3", mb(newF))
	}
	if mb(newF) > mb(oldF)/2 {
		t.Errorf("met WAL %.2f MB/h is not below half of 1.0.0 (%.2f)", mb(newF), mb(oldF))
	}
}

// TestMigrationOf100File migrates a copy of a database written by the
// 1.0.0 release build (AIRVIEW_DBEFF_D_OLDDB = path of its airview.db; the
// -wal file next to it is copied too): every met_obs row comes back through
// the logical table at the resolution of met_sample with the 1.0.0 value
// types, the guess_samples counters equal a GROUP BY count, and the
// forecast rows are unchanged.
func TestMigrationOf100File(t *testing.T) {
	src := os.Getenv("AIRVIEW_DBEFF_D_OLDDB")
	if src == "" {
		t.Skip("AIRVIEW_DBEFF_D_OLDDB not set")
	}
	ctx := context.Background()
	dir := t.TempDir()
	for _, sfx := range []string{"", "-wal"} {
		b, err := os.ReadFile(src + sfx)
		if err != nil {
			if sfx != "" && os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "airview.db"+sfx), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The 1.0.0 content, read with plain SQL before the new store opens.
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "airview.db")))
	if err != nil {
		t.Fatal(err)
	}
	var want [][]any
	rows, err := raw.Query(`SELECT * FROM met_obs ORDER BY t, rowid`)
	if err != nil {
		t.Fatal(err)
	}
	if err := scanRowsAny(rows, 9, func(v []any) error {
		want = append(want, append([]any(nil), v...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	guess := map[int]int64{}
	rows, err = raw.Query(`SELECT stn, COUNT(*) FROM guess_samples GROUP BY stn`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var stn int
		var n int64
		_ = rows.Scan(&stn, &n)
		guess[stn] = n
	}
	rows.Close()
	var fcWant int64
	_ = raw.QueryRow(`SELECT COUNT(*) FROM weather_fc`).Scan(&fcWant)
	_ = raw.Close()
	if len(want) == 0 {
		t.Fatal("the 1.0.0 file has no met_obs rows")
	}

	st := msOpen(t, dir)
	if got, _ := st.GuessSampleCounts(ctx); fmt.Sprint(got) != fmt.Sprint(guess) {
		t.Fatalf("guess counters %v, GROUP BY %v", got, guess)
	}
	if err := st.MigrateNow(ctx); err != nil {
		t.Fatal(err)
	}
	if !st.DataMigrationDone("met") {
		t.Fatal("met migration not done")
	}
	got := msLogical(t, st, nil)
	if len(got) != len(want) {
		t.Fatalf("logical met_obs: %d rows, 1.0.0 %d", len(got), len(want))
	}
	byKey := map[[2]int64][]any{}
	for _, g := range got {
		byKey[[2]int64{g[0].(int64), g[1].(int64)}] = g
	}
	for _, w := range want {
		if g := byKey[[2]int64{w[0].(int64), w[1].(int64)}]; g == nil || !msRowsEqual(w, g) {
			t.Fatalf("1.0.0 row %v migrated as %v", w, g)
		}
	}
	counts, err := st.TableCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["met_obs"] != int64(len(want)) || counts[GuessSamplesTable] != sumCounts(guess) {
		t.Fatalf("table counts met_obs %d guess_samples %d", counts["met_obs"], counts[GuessSamplesTable])
	}
	if fc, err := st.ForecastRows(ctx, 0, 0); err != nil || int64(len(fc)) != fcWant {
		t.Fatalf("forecast rows %d (1.0.0 %d), %v", len(fc), fcWant, err)
	}
	var pages int64
	_ = st.DB().QueryRow(`SELECT SUM(pgsize) FROM dbstat WHERE name = 'met_sample'`).Scan(&pages)
	t.Logf("1.0.0 file: %d met_obs rows -> met_sample %.1f B/row; guess %v", len(want), float64(pages)/float64(len(want)), guess)
}

func sumCounts(m map[int]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}
