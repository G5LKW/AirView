package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// trailTestRows returns the logical positions table (every stored trail
// point, legacy + chunks + tail) in (flight_id, t) order. Tests of other
// files use it where they used to read the positions table with SQL.
func trailTestRows(t testing.TB, s *Store) []TrailRow {
	t.Helper()
	var out []TrailRow
	if err := s.ForEachTrailRow(context.Background(), math.MinInt64, 0, 0, func(r TrailRow) error {
		out = append(out, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// trailTestCount counts the logical positions rows matching keep (nil = all).
func trailTestCount(t testing.TB, s *Store, keep func(r TrailRow) bool) int {
	t.Helper()
	n := 0
	for _, r := range trailTestRows(t, s) {
		if keep == nil || keep(r) {
			n++
		}
	}
	return n
}

// trailModel is the reference: the 1.0.0 positions table (INSERT OR
// REPLACE, key (flight_id, t)).
type trailModel map[[2]int64]TrailRow

func (m trailModel) put(r TrailRow) { m[[2]int64{r.FlightID, r.T}] = r }

func (m trailModel) rows(keep func(TrailRow) bool) []TrailRow {
	var out []TrailRow
	for _, r := range m {
		if keep == nil || keep(r) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, cmpTrailKey)
	return out
}

func randTrailRow(rng *rand.Rand, fid, t int64) TrailRow {
	r := TrailRow{FlightID: fid, T: t, LatE5: 5_000_000 + rng.Int64N(10_000), LonE5: 800_000 + rng.Int64N(10_000),
		Src: int64(rng.IntN(3))}
	if rng.IntN(4) != 0 {
		r.Alt, r.Has = rng.Int64N(40_000), r.Has|TrailHasAlt
	}
	if rng.IntN(3) != 0 {
		r.GS, r.Has = rng.Int64N(500), r.Has|TrailHasGS
	}
	if rng.IntN(3) != 0 {
		r.Track, r.Has = rng.Int64N(360), r.Has|TrailHasTrack
	}
	if rng.IntN(2) != 0 {
		r.RSSIX10, r.Has = -rng.Int64N(400), r.Has|TrailHasRSSI
	}
	return r
}

// insertLegacy writes rows straight into the 1.0.0 positions table.
func insertLegacy(t testing.TB, s *Store, rows []TrailRow) {
	t.Helper()
	if err := s.Write(bg, func(tx *sql.Tx) error {
		for i := range rows {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO positions(`+trailCols+`) VALUES(?,?,?,?,?,?,?,?,?)`, rows[i].Values()...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func addTrailRows(s *Store, rows []TrailRow) {
	ps := make([]Position, len(rows))
	for i := range rows {
		ps[i] = rows[i].Position()
	}
	s.AddPositions(ps)
}

func checkTrails(t *testing.T, s *Store, m trailModel, flights []Flight, label string) {
	t.Helper()
	want := m.rows(nil)
	got := trailTestRows(t, s)
	if !slices.Equal(got, want) {
		t.Fatalf("%s: logical rows differ: got %d want %d", label, len(got), len(want))
	}
	// Per flight, with a time window.
	for _, f := range flights {
		from, to := f.Start+40, f.Start+400
		ps, err := s.Positions(bg, f.ID, from, to)
		if err != nil {
			t.Fatal(err)
		}
		exp := m.rows(func(r TrailRow) bool { return r.FlightID == f.ID && r.T >= from && r.T <= to })
		if len(ps) != len(exp) {
			t.Fatalf("%s: flight %d: %d points, want %d", label, f.ID, len(ps), len(exp))
		}
		for i := range ps {
			if ps[i] != exp[i].Position() && !samePosition(ps[i], exp[i].Position()) {
				t.Fatalf("%s: flight %d point %d: %+v want %+v", label, f.ID, i, ps[i], exp[i].Position())
			}
		}
	}
	// ForEachPosition: flights by end, points by t, only flights with a row.
	var seq []TrailRow
	if err := s.ForEachPosition(bg, math.MinInt64/2, math.MaxInt64/2, func(icao uint32, p Position) error {
		r, _ := trailRowFromPosition(&p)
		seq = append(seq, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	byEnd := slices.Clone(flights)
	slices.SortFunc(byEnd, func(a, b Flight) int { return cmp.Or(cmp.Compare(a.End, b.End), cmp.Compare(a.ID, b.ID)) })
	var exp []TrailRow
	for _, f := range byEnd {
		exp = append(exp, m.rows(func(r TrailRow) bool { return r.FlightID == f.ID })...)
	}
	if !slices.EqualFunc(seq, exp, func(a, b TrailRow) bool { return samePosition(a.Position(), b.Position()) }) {
		t.Fatalf("%s: ForEachPosition differs: %d vs %d", label, len(seq), len(exp))
	}
	// Range and limit of the logical table.
	lo, hi := int64(1_000_200), int64(1_003_000)
	var part []TrailRow
	if err := s.ForEachTrailRow(bg, lo, hi, 50, func(r TrailRow) error { part = append(part, r); return nil }); err != nil {
		t.Fatal(err)
	}
	wpart := m.rows(func(r TrailRow) bool { return r.T >= lo && r.T < hi })
	if len(wpart) > 50 {
		wpart = wpart[:50]
	}
	if !slices.Equal(part, wpart) {
		t.Fatalf("%s: ForEachTrailRow range/limit differs", label)
	}
}

func samePosition(a, b Position) bool {
	eq := func(x, y float64) bool { return x == y || (math.IsNaN(x) && math.IsNaN(y)) }
	return a.FlightID == b.FlightID && a.T == b.T && a.Lat == b.Lat && a.Lon == b.Lon && eq(a.Alt, b.Alt) &&
		eq(a.GS, b.GS) && eq(a.Track, b.Track) && eq(a.RSSI, b.RSSI) && a.Src == b.Src
}

// The trail tiers (legacy table, tail, sealed chunks, merges of late
// points, compaction, migration) always read back exactly what the 1.0.0
// positions table would hold, and the O(1) counters stay exact.
func TestTrailStoreMatchesModel(t *testing.T) {
	SetTrailSealInterval(time.Hour) // seals only where the test asks
	defer SetTrailSealInterval(0)
	s := openTest(t, manual)
	rng := rand.New(rand.NewPCG(7, 8))
	m := trailModel{}
	var flights []Flight
	for i := 1; i <= 30; i++ {
		f := NewFlight(int64(i), uint32(0x100+i), 1_000_000+int64(i)*50)
		f.End = f.Start + 3000
		flights = append(flights, f)
	}
	s.UpsertFlights(flights)
	// 1.0.0 rows of the first 20 flights.
	var legacy []TrailRow
	for _, f := range flights[:20] {
		for k := int64(0); k < 120; k++ {
			r := randTrailRow(rng, f.ID, f.Start+k*5)
			legacy = append(legacy, r)
			m.put(r)
		}
	}
	mustFlush(t, s)
	insertLegacy(t, s, legacy)
	checkTrails(t, s, m, flights, "legacy only")

	round := func(label string, seal bool) {
		var batch []TrailRow
		for i := 0; i < 400; i++ {
			f := flights[rng.IntN(len(flights))]
			r := randTrailRow(rng, f.ID, f.Start+rng.Int64N(800))
			batch = append(batch, r)
			if rng.IntN(5) == 0 { // a repeat within the batch: the later one wins
				r2 := randTrailRow(rng, f.ID, r.T)
				batch = append(batch, r2)
			}
		}
		for _, r := range batch {
			m.put(r)
		}
		addTrailRows(s, batch)
		mustFlush(t, s)
		if seal {
			if _, err := s.SealTrails(bg); err != nil {
				t.Fatal(err)
			}
		}
		checkTrails(t, s, m, flights, label)
	}
	round("tail", false)
	round("tail 2", false)
	round("sealed", true)
	round("late points into chunks", true)
	round("tail over chunks", false)

	counts, err := s.TableCounts(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SealTrails(bg); err != nil {
		t.Fatal(err)
	}
	s.invalidateCounts()
	if counts, err = s.TableCounts(bg); err != nil || counts["positions"] != int64(len(m)) {
		t.Fatalf("positions count %d want %d (%v)", counts["positions"], len(m), err)
	}

	// Migration of the legacy rows (small steps), then compaction.
	if steps, err := s.migrateTrailsNow(bg, 300); err != nil || steps < 2 {
		t.Fatalf("migration: %d steps, %v", steps, err)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM positions`); n != 0 {
		t.Fatalf("legacy rows left: %d", n)
	}
	checkTrails(t, s, m, flights, "migrated")
	round("after migration", true)
	if _, err := s.CompactTrails(bg); err != nil {
		t.Fatal(err)
	}
	checkTrails(t, s, m, flights, "compacted")
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM trail_chunk WHERE n != 256 AND id NOT IN (SELECT MAX(id) FROM trail_chunk GROUP BY flight_id)`); n != 0 {
		t.Fatalf("%d short chunks after compaction", n)
	}
	s.invalidateCounts()
	counts, _ = s.TableCounts(bg)
	if counts["positions"] != int64(len(m)) {
		t.Fatalf("positions count %d want %d", counts["positions"], len(m))
	}
	if d, err := s.VerifyCounts(bg); err != nil || d != (CountDrift{}) {
		t.Fatalf("drift %+v %v", d, err)
	}

	// Deleting trails keeps the counters exact.
	var n int64
	if err := s.Write(bg, func(tx *sql.Tx) error {
		var err error
		n, err = deleteTrails(bg, tx, flights[0].ID, flights[25].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := 0
	for k := range m {
		if k[0] == flights[0].ID || k[0] == flights[25].ID {
			delete(m, k)
			want++
		}
	}
	if n != int64(want) {
		t.Fatalf("deleteTrails: %d want %d", n, want)
	}
	checkTrails(t, s, m, flights[1:], "deleted")
	if d, err := s.VerifyCounts(bg); err != nil || d != (CountDrift{}) {
		t.Fatalf("drift after delete %+v %v", d, err)
	}
	if id, err := maxStoredFlightID(bg, s.DB()); err != nil || id != 30 {
		t.Fatalf("maxStoredFlightID %d %v", id, err)
	}
}

// The writer seals the tail on its own once the interval has passed, and a
// trail survives a restart in every tier.
func TestTrailSealByWriterAndReopen(t *testing.T) {
	SetTrailSealInterval(time.Millisecond)
	defer SetTrailSealInterval(0)
	dir := t.TempDir()
	s := openDir(t, dir, manual)
	f := NewFlight(5, 0xabc, 2_000_000)
	f.End = f.Start + 1000
	s.UpsertFlights([]Flight{f})
	m := trailModel{}
	rng := rand.New(rand.NewPCG(1, 1))
	for k := 0; k < 5; k++ {
		var b []TrailRow
		for i := 0; i < 50; i++ {
			r := randTrailRow(rng, 5, f.Start+int64(k*50+i))
			b = append(b, r)
			m.put(r)
		}
		addTrailRows(s, b)
		mustFlush(t, s)
		time.Sleep(5 * time.Millisecond)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM trail_tail`); n != 0 {
		t.Fatalf("tail not sealed: %d", n)
	}
	checkTrails(t, s, m, []Flight{f}, "sealed by writer")
	SetTrailSealInterval(time.Hour)
	last := randTrailRow(rng, 5, f.Start+999)
	m.put(last)
	addTrailRows(s, []TrailRow{last}) // stays in the tail
	mustFlush(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openDir(t, dir, manual)
	checkTrails(t, s2, m, []Flight{f}, "reopened")
	if s2.NextFlightID() != 6 {
		t.Fatal("flight IDs do not continue after the stored ones")
	}
}

// replayFrames is the frame computation of /api/replay (api_replay.go):
// per flight, frame k shows the newest point at or before its time if it is
// at most stale seconds old.
func replayFrames(flights [][]Position, from, to, step, stale int64) [][][2]int64 {
	n := int((to-from)/step) + 1
	frames := make([][][2]int64, n)
	for fi, cur := range flights {
		if len(cur) == 0 {
			continue
		}
		j := 0
		k0 := int((cur[0].T - from + step - 1) / step)
		if cur[0].T <= from {
			k0 = 0
		}
		for k := max(k0, 0); k < n; k++ {
			ft := from + int64(k)*step
			for j+1 < len(cur) && cur[j+1].T <= ft {
				j++
			}
			p := cur[j]
			if p.T > ft {
				continue
			}
			if ft-p.T > stale {
				if j == len(cur)-1 {
					break
				}
				continue
			}
			frames[k] = append(frames[k], [2]int64{int64(fi), p.T})
		}
	}
	return frames
}

// ForEachPositionStep hands out only the points the frames use, and frames
// built from them equal the frames built from every point (also the
// aircraft list: every flight keeps its first point).
func TestForEachPositionStep(t *testing.T) {
	SetTrailSealInterval(time.Hour)
	defer SetTrailSealInterval(0)
	s := openTest(t, manual)
	rng := rand.New(rand.NewPCG(3, 9))
	var flights []Flight
	var rows []TrailRow
	for i := 1; i <= 40; i++ {
		f := NewFlight(int64(i), uint32(i), 3_000_000+rng.Int64N(20_000))
		t0 := f.Start
		for k := 0; k < 300; k++ {
			t0 += 1 + rng.Int64N(12)
			if rng.IntN(60) == 0 {
				t0 += rng.Int64N(900) // a gap
			}
			rows = append(rows, randTrailRow(rng, f.ID, t0))
		}
		f.End = t0
		flights = append(flights, f)
	}
	s.UpsertFlights(flights)
	addTrailRows(s, rows)
	mustFlush(t, s)
	if _, err := s.SealTrails(bg); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ from, to, step, stale int64 }{
		{3_000_000, 3_030_000, 5, 30},
		{3_005_000, 3_020_000, 144, 288},
		{3_000_000, 3_100_000, 50, 100},
		{2_990_000, 3_001_000, 7, 30},
	} {
		collect := func(step bool) ([][]Position, int) {
			var out [][]Position
			cur := int64(-1)
			fn := func(icao uint32, p Position) error {
				if p.FlightID != cur {
					out = append(out, nil)
					cur = p.FlightID
				}
				out[len(out)-1] = append(out[len(out)-1], p)
				return nil
			}
			if step {
				st, err := s.ForEachPositionStep(bg, c.from, c.to, c.step, c.stale, fn)
				if err != nil {
					t.Fatal(err)
				}
				return out, st.Read
			}
			n := 0
			if err := s.ForEachPosition(bg, c.from-c.stale, c.to, func(icao uint32, p Position) error { n++; return fn(icao, p) }); err != nil {
				t.Fatal(err)
			}
			return out, n
		}
		all, nAll := collect(false)
		used, nRead := collect(true)
		if nRead != nAll || len(all) != len(used) {
			t.Fatalf("%+v: read %d vs %d, flights %d vs %d", c, nRead, nAll, len(used), len(all))
		}
		fa, fu := replayFrames(all, c.from, c.to, c.step, c.stale), replayFrames(used, c.from, c.to, c.step, c.stale)
		for k := range fa {
			if !slices.Equal(fa[k], fu[k]) {
				t.Fatalf("%+v: frame %d differs", c, k)
			}
		}
		nu := 0
		for _, f := range used {
			nu += len(f)
		}
		t.Logf("%+v: %d points read, %d used", c, nAll, nu)
	}
}

// pruneTrailOrphans applies the 1.0.0 orphan rule to every tier.
func TestPruneTrailOrphans(t *testing.T) {
	SetTrailSealInterval(time.Hour)
	defer SetTrailSealInterval(0)
	s := openTest(t, manual)
	f := NewFlight(100, 1, 5000)
	f.End = 6000
	s.UpsertFlights([]Flight{f})
	rng := rand.New(rand.NewPCG(4, 4))
	var orphan, own []TrailRow
	for k := int64(0); k < 100; k++ {
		orphan = append(orphan, randTrailRow(rng, 7, 1000+k*10)) // 1000 .. 1990
		own = append(own, randTrailRow(rng, 100, 5000+k))
	}
	insertLegacy(t, s, orphan[:30])
	addTrailRows(s, orphan[30:70])
	mustFlush(t, s)
	if _, err := s.SealTrails(bg); err != nil {
		t.Fatal(err)
	}
	addTrailRows(s, append(orphan[70:], own...))
	mustFlush(t, s)
	var total int64
	for {
		var n int64
		if err := s.Write(bg, func(tx *sql.Tx) error {
			var err error
			n, err = pruneTrailOrphans(bg, tx, 1500)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		total += n
		if n == 0 {
			break
		}
	}
	if total != 50 {
		t.Fatalf("pruned %d, want 50", total)
	}
	left := trailTestRows(t, s)
	if len(left) != 150 || left[0].T != 1500 || left[0].FlightID != 7 {
		t.Fatalf("left %d rows, first %+v", len(left), left[0])
	}
	if d, err := s.VerifyCounts(bg); err != nil || d != (CountDrift{}) {
		t.Fatalf("drift %+v %v", d, err)
	}
}

// A 1.0.0 database (core schema with a filled positions table, made by the
// 1.0.0 statements) is migrated on open: the trail and flights migrations
// run, the legacy rows stay readable, the data migration moves them.
func TestTrailMigrationFromOldFormat(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{schemaMigrationsDDL}
	for _, m := range coreMigrations {
		stmts = append(stmts, m.ddl, `INSERT INTO schema_migrations(name, version, applied) VALUES('core', `+strconv.Itoa(m.version)+`, 0)`)
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%.50s: %v", q, err)
		}
	}
	rng := rand.New(rand.NewPCG(9, 9))
	m := trailModel{}
	var flights []Flight
	for i := 1; i <= 12; i++ {
		f := NewFlight(int64(i), uint32(i), 1_000_000+int64(i)*100)
		f.End = f.Start + 2000
		flights = append(flights, f)
		if _, err := db.Exec(`INSERT INTO flights(id, icao, start, "end", dep) VALUES(?,?,?,?,?)`, f.ID, f.ICAO, f.Start, f.End,
			map[bool]any{true: "EDDF", false: nil}[i%3 == 0]); err != nil {
			t.Fatal(err)
		}
		for k := int64(0); k < 300; k++ {
			r := randTrailRow(rng, f.ID, f.Start+k*3)
			m.put(r)
			// The 1.0.0 statement (ops.go of 1.0.0).
			if _, err := db.Exec(`INSERT OR REPLACE INTO positions(flight_id, t, lat_e5, lon_e5, alt, gs, track, rssi_x10, src)
VALUES(?,?,?,?,?,?,?,?,?)`, r.Values()...); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = db.Close()

	s := openDir(t, dir, manual)
	for _, name := range []string{"trail", "flights"} {
		if n := scalar[int](t, s, `SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name); n != 1 {
			t.Fatalf("migration %s not applied", name)
		}
	}
	if s.NextFlightID() != 13 {
		t.Fatal("flight IDs")
	}
	checkTrails(t, s, m, flights, "1.0.0 file")
	counts, err := s.TableCounts(bg)
	if err != nil || counts["positions"] != int64(len(m)) {
		t.Fatalf("count of the 1.0.0 table: %d %v", counts["positions"], err)
	}
	rows, peak, err := trailMigrateEstimate(bg, s.DB())
	if err != nil || rows != int64(len(m)) || peak <= 0 {
		t.Fatalf("estimate %d %d %v", rows, peak, err)
	}
	// New points of a flight that also has legacy rows, then the migration.
	var extra []TrailRow
	for k := int64(0); k < 50; k++ {
		r := randTrailRow(rng, 3, flights[2].Start+k*7) // overlaps the legacy rows
		extra = append(extra, r)
		m.put(r)
	}
	addTrailRows(s, extra)
	mustFlush(t, s)
	if _, err := s.SealTrails(bg); err != nil {
		t.Fatal(err)
	}
	checkTrails(t, s, m, flights, "legacy + chunks")
	if _, err := s.migrateTrailsNow(bg, 500); err != nil {
		t.Fatal(err)
	}
	checkTrails(t, s, m, flights, "migrated")
	s.invalidateCounts()
	counts, _ = s.TableCounts(bg)
	if counts["positions"] != int64(len(m)) {
		t.Fatalf("count after migration %d want %d", counts["positions"], len(m))
	}
	if d, err := s.VerifyCounts(bg); err != nil || d != (CountDrift{}) {
		t.Fatalf("drift %+v %v", d, err)
	}
	// A crash in the middle of a step leaves nothing behind: the step is
	// one transaction.
	errBoom := errors.New("boom")
	if err := s.Write(bg, func(tx *sql.Tx) error {
		if _, _, err := trailMigrateStep(bg, tx, nil, 0); err != nil {
			return err
		}
		return errBoom
	}); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	checkTrails(t, s, m, flights, "after rolled-back step")
}

// The trail of a long flight (13.7 k points, the longest of the baseline)
// read while it is active (one seal chunk per seal interval plus a tail of
// a few minutes) and after compaction. DB-EFFICIENCY-DESIGN target: <= 6 ms
// on the i9 (1.0.0: 3.5-4.2 ms).
func TestTrailLongFlightLatency(t *testing.T) {
	SetTrailSealInterval(time.Hour)
	defer SetTrailSealInterval(0)
	s := openTest(t, Options{FlushInterval: time.Hour, FlushRows: 1 << 30, MaxQueueRows: 1 << 30, MigrationDelay: -1})
	rng := rand.New(rand.NewPCG(5, 3))
	const n, perSeal = 13_700, 60
	f := NewFlight(77, 0x4ca, 1_800_000_000)
	var other []Flight
	for i := 0; i < 250; i++ { // other active flights share the tail
		g := NewFlight(int64(100+i), uint32(i+1), f.Start)
		g.End = f.Start + 5*n
		other = append(other, g)
	}
	f.End = f.Start + 5*n
	s.UpsertFlights(append(other, f))
	var batch []TrailRow
	for k := 0; k < n; k++ {
		batch = append(batch, randTrailRow(rng, f.ID, f.Start+int64(k)*5))
		if len(batch) == perSeal {
			addTrailRows(s, batch)
			batch = batch[:0]
			if k < n-600 {
				mustFlush(t, s)
				if _, err := s.SealTrails(bg); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	addTrailRows(s, batch)
	var tail []TrailRow // 5 minutes of 250 other flights in the tail
	for i, g := range other {
		for k := 0; k < 60; k++ {
			tail = append(tail, randTrailRow(rng, g.ID, g.Start+int64(k*5+i%5)))
		}
	}
	addTrailRows(s, tail)
	mustFlush(t, s)
	measure := func(label string) time.Duration {
		var ds []time.Duration
		for i := 0; i < 15; i++ {
			t0 := time.Now()
			tr, err := s.Trail(bg, f.ID)
			if err != nil || len(tr) != n {
				t.Fatalf("%s: %d points, %v", label, len(tr), err)
			}
			ds = append(ds, time.Since(t0))
		}
		slices.Sort(ds)
		t.Logf("%s: median %v, max %v", label, ds[len(ds)/2], ds[len(ds)-1])
		return ds[len(ds)/2]
	}
	active := measure("active (seal chunks + tail of 15,600 rows)")
	if _, err := s.SealTrails(bg); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE flights SET "end" = ? WHERE id = ?`, f.End-2*3600, f.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.End -= 2 * 3600
	if err := s.Write(bg, func(tx *sql.Tx) error {
		st, err := readTrailState(bg, tx)
		if err != nil {
			return err
		}
		if _, err := compactTrailsTx(bg, tx, &st, math.MaxInt64/2); err != nil {
			return err
		}
		return writeTrailState(bg, tx, st)
	}); err != nil {
		t.Fatal(err)
	}
	closed := measure("compacted (54 chunks)")
	if active > 60*time.Millisecond || closed > 60*time.Millisecond {
		t.Fatalf("trail read too slow: %v / %v", active, closed)
	}
}
