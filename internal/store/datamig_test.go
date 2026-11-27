package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
)

// fakeMigration moves rows from fake_src to fake_dst, perStep rows per step
// (ignoring the budget so that a test controls the number of steps), and
// counts them in fake_cnt. A row processed twice violates fake_dst's
// primary key; a skipped row stays in fake_src.
func fakeMigration(perStep int, stepDelay time.Duration) DataMigration {
	return DataMigration{
		Name: "fake", Version: 1, Kinds: KindMain,
		Estimate: func(ctx context.Context, q Querier) (int64, int64, error) {
			if ok, err := hasTableTx(ctx, q, "fake_src"); err != nil || !ok {
				return 0, 0, err
			}
			var n int64
			err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM fake_src`).Scan(&n)
			return n, n * 100, err
		},
		Step: func(ctx context.Context, tx *sql.Tx, cursor []byte, budget int) ([]byte, bool, error) {
			if ok, err := hasTableTx(ctx, tx, "fake_src"); err != nil || !ok {
				return cursor, true, err
			}
			last, _ := strconv.ParseInt(string(cursor), 10, 64)
			ids, err := queryInt64s(ctx, tx, `SELECT id FROM fake_src WHERE id > ? ORDER BY id LIMIT ?`, last, perStep)
			if err != nil {
				return nil, false, err
			}
			if stepDelay > 0 {
				time.Sleep(stepDelay)
			}
			for _, id := range ids {
				if _, err := tx.ExecContext(ctx, `INSERT INTO fake_dst(id, v) SELECT id, v FROM fake_src WHERE id = ?`, id); err != nil {
					return nil, false, err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM fake_src WHERE id = ?`, id); err != nil {
					return nil, false, err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE fake_cnt SET n = n + ?`, len(ids)); err != nil {
				return nil, false, err
			}
			if len(ids) > 0 {
				last = ids[len(ids)-1]
			}
			return []byte(strconv.FormatInt(last, 10)), len(ids) < perStep, nil
		},
		Finish: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS fake_src`)
			return err
		},
	}
}

func seedFake(t *testing.T, s *Store, n int) {
	t.Helper()
	if err := s.Write(bg, func(tx *sql.Tx) error {
		for _, q := range []string{
			`CREATE TABLE fake_src(id INTEGER PRIMARY KEY, v TEXT NOT NULL)`,
			`CREATE TABLE fake_dst(id INTEGER PRIMARY KEY, v TEXT NOT NULL)`,
			`CREATE TABLE fake_cnt(n INTEGER NOT NULL)`, `INSERT INTO fake_cnt VALUES(0)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		for i := 1; i <= n; i++ {
			if _, err := tx.Exec(`INSERT INTO fake_src VALUES(?, ?)`, i, fmt.Sprintf("row %d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// checkFake verifies the exactly-once invariants; complete=true also wants
// the migration finished.
func checkFake(t *testing.T, s *Store, n int, complete bool) {
	t.Helper()
	dst := scalar[int](t, s, `SELECT COUNT(*) FROM fake_dst`)
	cnt := scalar[int](t, s, `SELECT n FROM fake_cnt`)
	src := 0
	if s.HasTable("fake_src") {
		src = scalar[int](t, s, `SELECT COUNT(*) FROM fake_src`)
	}
	if dst != cnt || dst+src != n {
		t.Fatalf("dst %d counter %d src %d (n %d): a step ran twice or was skipped", dst, cnt, src, n)
	}
	if complete {
		if src != 0 || s.HasTable("fake_src") || !s.DataMigrationDone("fake") {
			t.Fatalf("not complete: src %d table %v done %v", src, s.HasTable("fake_src"), s.DataMigrationDone("fake"))
		}
		if v := scalar[string](t, s, `SELECT group_concat(v, '') FROM (SELECT v FROM fake_dst WHERE id IN (1, ?) ORDER BY id)`, n); v != fmt.Sprintf("row 1row %d", n) {
			t.Fatalf("row content %q", v)
		}
	}
}

func migOpts(dm DataMigration) Options {
	o := manual
	o.MigrationDelay = -1 // tests drive MigrateNow themselves
	o.testDataMigs = []DataMigration{dm}
	return o
}

func TestDataMigrationRunsToCompletion(t *testing.T) {
	dir := t.TempDir()
	s := openDir(t, dir, migOpts(fakeMigration(70, 0)))
	seedFake(t, s, 1000)
	if st := s.MigrationStatus(); len(st) != 1 || st[0].State != MigPending {
		t.Fatalf("status before: %+v", st)
	}
	if err := s.MigrateNow(bg); err != nil {
		t.Fatal(err)
	}
	checkFake(t, s, 1000, true)
	st := s.MigrationStatus()
	if st[0].State != MigDone || st[0].Steps != 15 || st[0].RowsDone < 1000 {
		t.Fatalf("status after: %+v", st)
	}
	if info := s.MigrationInfo(); info.State != MigDone {
		t.Fatalf("info %+v", info)
	}
	// A reopened store knows the migration is done without running it.
	_ = s.Close()
	s2 := openDir(t, dir, migOpts(fakeMigration(70, 0)))
	if !s2.DataMigrationDone("fake") || s2.MigrationStatus()[0].State != MigDone {
		t.Fatalf("not done after reopen: %+v", s2.MigrationStatus())
	}
	if !s2.DataMigrationDone("fake") || s2.DataMigrationDone("other") {
		t.Fatal("DataMigrationDone")
	}
	if n := scalar[int](t, s2, `SELECT rows_done FROM schema_data_migrations WHERE name = 'fake'`); n < 1000 {
		t.Fatalf("rows_done %d", n)
	}
}

var errKill = errors.New("simulated crash")

// The store is "killed" in-process between every two steps: once with the
// step's transaction rolled back just before its commit, once right after
// the commit; every time the store is closed and reopened. The migration
// must still move every row exactly once.
func TestDataMigrationKilledBetweenEverySteps(t *testing.T) {
	const n, per = 600, 25 // 25 steps
	dir := t.TempDir()
	s := openDir(t, dir, migOpts(fakeMigration(per, 0)))
	seedFake(t, s, n)
	_ = s.Close()

	for iter := 0; iter < 200; iter++ {
		phase := []string{"before-commit", "after-commit"}[iter%2]
		o := migOpts(fakeMigration(per, 0))
		o.testMigHook = func(name string, step int, ph string) error {
			if step == 2 && ph == phase {
				return errKill
			}
			return nil
		}
		s := openDir(t, dir, o)
		checkFake(t, s, n, false)
		err := s.MigrateNow(bg)
		if err == nil {
			checkFake(t, s, n, true)
			if iter < 10 {
				t.Fatalf("finished after %d iterations, too early", iter)
			}
			_ = s.Close()
			return
		}
		if !errors.Is(err, errKill) {
			t.Fatalf("iteration %d: %v", iter, err)
		}
		_ = s.Close()
	}
	t.Fatal("migration never finished")
}

// Child process for TestDataMigrationKilledChildProcess.
func TestDataMigrationChildHelper(t *testing.T) {
	dir := os.Getenv("AIRVIEW_DATAMIG_CHILD")
	if dir == "" {
		t.Skip("helper process")
	}
	o := migOpts(fakeMigration(20, 10*time.Millisecond))
	o.Logger = quiet()
	s, err := OpenWithOptions(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateNow(bg); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// A child process migrating the database is killed (os.Process.Kill:
// TerminateProcess / SIGKILL) at random moments; the final run completes
// with every row moved exactly once and an intact file.
func TestDataMigrationKilledChildProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	const n = 8000
	dir := t.TempDir()
	s := openDir(t, dir, migOpts(fakeMigration(20, 0)))
	seedFake(t, s, n)
	_ = s.Close()

	run := func(kill time.Duration) error {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDataMigrationChildHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "AIRVIEW_DATAMIG_CHILD="+dir)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if kill <= 0 {
			return cmd.Wait()
		}
		time.Sleep(kill)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil
	}
	rng := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 5; i++ {
		_ = run(time.Duration(300+rng.IntN(700)) * time.Millisecond)
		s := openDir(t, dir, migOpts(fakeMigration(20, 0)))
		checkFake(t, s, n, false)
		if v := scalar[string](t, s, `PRAGMA integrity_check`); v != "ok" {
			t.Fatalf("integrity after kill %d: %s", i, v)
		}
		left := 0
		if s.HasTable("fake_src") {
			left = scalar[int](t, s, `SELECT COUNT(*) FROM fake_src`)
		}
		t.Logf("kill %d: %d rows left", i+1, left)
		_ = s.Close()
	}
	if err := run(0); err != nil {
		t.Fatalf("final run: %v", err)
	}
	s = openDir(t, dir, migOpts(fakeMigration(20, 0)))
	checkFake(t, s, n, true)
}

// With too little free disk the migration waits ("waiting_for_disk") and
// continues on its own once space is back.
func TestDataMigrationWaitsForDisk(t *testing.T) {
	var free atomic.Uint64
	free.Store(10 << 20)
	o := migOpts(fakeMigration(50, 0))
	o.MigrationDelay = time.Millisecond
	o.testDiskFree = func(string) (uint64, error) { return free.Load(), nil }
	o.testRecheck = 50 * time.Millisecond
	o.Profile = Profile{Name: ProfileStandard, BackupBeforeMigrate: "never"}
	dir := t.TempDir()
	s := openDir(t, dir, migOpts(fakeMigration(50, 0)))
	seedFake(t, s, 500)
	_ = s.Close()

	s = openDir(t, dir, o)
	deadline := time.Now().Add(10 * time.Second)
	for s.MigrationInfo().State != MigWaitingForDisk {
		if time.Now().After(deadline) {
			t.Fatalf("never waited for disk: %+v", s.MigrationInfo())
		}
		time.Sleep(10 * time.Millisecond)
	}
	checkFake(t, s, 500, false)
	if left := scalar[int](t, s, `SELECT COUNT(*) FROM fake_src`); left != 500 {
		t.Fatalf("migrated %d rows without disk space", 500-left)
	}
	free.Store(1 << 40)
	for !s.DataMigrationDone("fake") {
		if time.Now().After(deadline) {
			t.Fatalf("did not resume: %+v", s.MigrationInfo())
		}
		time.Sleep(10 * time.Millisecond)
	}
	checkFake(t, s, 500, true)
}

// The background runner starts after MigrationDelay and pauses between
// steps; Close stops it between two steps.
func TestDataMigrationBackgroundAndClose(t *testing.T) {
	o := migOpts(fakeMigration(10, 0))
	o.MigrationDelay = time.Millisecond
	o.Profile = Profile{Name: ProfileStandard, BackupBeforeMigrate: "never", MigrationStep: 20 * time.Millisecond}
	dir := t.TempDir()
	s := openDir(t, dir, migOpts(fakeMigration(10, 0)))
	seedFake(t, s, 300)
	_ = s.Close()
	s = openDir(t, dir, o)
	deadline := time.Now().Add(10 * time.Second)
	for scalar[int](t, s, `SELECT COUNT(*) FROM fake_dst`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("background migration did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openDir(t, dir, migOpts(fakeMigration(10, 0)))
	checkFake(t, s, 300, false)
	if err := s.MigrateNow(bg); err != nil {
		t.Fatal(err)
	}
	checkFake(t, s, 300, true)
}

func TestOrderDataMigrations(t *testing.T) {
	mk := func(name string, v int, after ...string) DataMigration {
		return DataMigration{Name: name, Version: v, After: after}
	}
	got := orderDataMigrations([]DataMigration{mk("met", 2), mk("trail", 1, "flights"), mk("flights", 2), mk("heat", 2), mk("flights", 1)})
	var keys []string
	for _, m := range got {
		keys = append(keys, m.key())
	}
	want := "[flights/1 flights/2 heat/2 met/2 trail/1]"
	if fmt.Sprint(keys) != want {
		t.Fatalf("order %v, want %v", keys, want)
	}
	// After puts a migration behind its dependency even when its name sorts first.
	got = orderDataMigrations([]DataMigration{mk("a", 1, "z"), mk("z", 1)})
	if got[0].Name != "z" {
		t.Fatalf("dependency order %v", got)
	}
}

func TestPrunerRegistryAndExtra(t *testing.T) {
	var calls atomic.Int32
	registerPruner("test.pruner", func(ctx context.Context, s *Store, cfg model.StorageConfig, now int64, res *PruneResult) error {
		calls.Add(1)
		res.AddExtra("test_rows", 7)
		return nil
	})
	t.Cleanup(func() {
		hooks.mu.Lock()
		for i, p := range hooks.pruners {
			if p.name == "test.pruner" {
				hooks.pruners = append(hooks.pruners[:i], hooks.pruners[i+1:]...)
				break
			}
		}
		hooks.mu.Unlock()
	})
	s := openTest(t, manual)
	res, err := s.Prune(bg, model.StorageConfig{TrailRetentionDays: 1, FlightRetentionDays: 2, MinuteRetentionDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || res.Extra["test_rows"] != 7 || res.Total() != 7 {
		t.Fatalf("calls %d res %+v", calls.Load(), res)
	}
}

func TestHourlyMaintenance(t *testing.T) {
	var ran atomic.Int32
	registerHourly("test.ok", func(ctx context.Context, s *Store) error { ran.Add(1); return nil })
	registerHourly("test.fail", func(ctx context.Context, s *Store) error { return errors.New("boom") })
	t.Cleanup(func() {
		hooks.mu.Lock()
		kept := hooks.hourly[:0]
		for _, h := range hooks.hourly {
			if h.name != "test.ok" && h.name != "test.fail" {
				kept = append(kept, h)
			}
		}
		hooks.hourly = kept
		hooks.mu.Unlock()
	})
	s := openTest(t, manual)
	writePages(t, s, 50)
	rep, err := s.HourlyMaintenance(bg)
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 || rep.Errors["test.fail"] != "boom" || !rep.Optimized || !rep.Checkpointed {
		t.Fatalf("report %+v", rep)
	}
	if mx, _ := walState(t, s); mx != 0 {
		t.Fatalf("WAL not truncated: %d", mx)
	}
	if s.Stats().CheckpointTruncates != 1 {
		t.Fatal("truncate not counted")
	}
}

// Legacy pruning does not fail when a data migration dropped or emptied
// the 1.0.0 tables (positions, met_obs).
func TestPruneWithoutLegacyTables(t *testing.T) {
	s := openTest(t, manual)
	if err := s.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DROP TABLE met_obs`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(bg, model.StorageConfig{TrailRetentionDays: 1, FlightRetentionDays: 2, MinuteRetentionDays: 1}); err != nil {
		t.Fatal(err)
	}
}

// A data migration writes much faster than the station: the runner
// checkpoints as soon as the WAL holds the soft size, so the WAL stays near
// the soft size instead of growing to the hard cap (whose automatic
// checkpoint would run inside a commit).
func TestDataMigrationKeepsWALAtSoftSize(t *testing.T) {
	dir := t.TempDir()
	dm := fakeMigration(400, 0)
	o := migOpts(dm)
	o.Profile = ResolveProfile(ProfileStandard, KindMain, dir)
	o.Profile.CheckpointSoftBytes = 256 << 10
	o.Profile.CheckpointInterval = time.Hour // only the migration checkpoints
	o.Profile.WALHardCapPages = 1 << 20      // and never SQLite's own
	o.Profile.JournalSizeLimit = 64 << 20
	var maxPending int64
	var s *Store
	o.testMigHook = func(name string, step int, phase string) error {
		if phase == "after-commit" {
			if p, ok := s.walPendingBytes(); ok {
				maxPending = max(maxPending, p)
			}
		}
		return nil
	}
	s = openDir(t, dir, o)
	const n = 20000
	if err := s.Write(bg, func(tx *sql.Tx) error {
		for _, q := range []string{
			`CREATE TABLE fake_src(id INTEGER PRIMARY KEY, v TEXT NOT NULL)`,
			`CREATE TABLE fake_dst(id INTEGER PRIMARY KEY, v TEXT NOT NULL)`,
			`CREATE TABLE fake_cnt(n INTEGER NOT NULL)`, `INSERT INTO fake_cnt VALUES(0)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		for i := 1; i <= n; i++ {
			if _, err := tx.Exec(`INSERT INTO fake_src VALUES(?, ?)`, i, fmt.Sprintf("row %d %0200d", i, i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(bg); err != nil {
		t.Fatal(err)
	}
	before := s.Stats().Checkpoints
	if err := s.MigrateNow(bg); err != nil {
		t.Fatal(err)
	}
	if dst := scalar[int](t, s, `SELECT COUNT(*) FROM fake_dst`); dst != n {
		t.Fatalf("moved %d of %d rows", dst, n)
	}
	// ~4.6 MB move through the WAL; one step writes ~250 KB.
	if maxPending == 0 || maxPending > 1<<20 {
		t.Fatalf("largest un-checkpointed WAL after a step: %d bytes (soft size 256 KiB)", maxPending)
	}
	if got := s.Stats().Checkpoints - before; got < 4 {
		t.Fatalf("%d checkpoints during the migration", got)
	}
}
