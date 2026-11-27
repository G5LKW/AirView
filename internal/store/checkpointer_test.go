package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

// writePages appends about n pages of WAL (one 3.5 KB row per page).
func writePages(t *testing.T, s *Store, n int) {
	t.Helper()
	if err := s.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS ckpt_test(id INTEGER PRIMARY KEY, b BLOB)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3500)
	for done := 0; done < n; {
		k := min(200, n-done)
		if err := s.Write(bg, func(tx *sql.Tx) error {
			for i := 0; i < k; i++ {
				_, _ = rand.Read(buf)
				if _, err := tx.Exec(`INSERT INTO ckpt_test(b) VALUES(?)`, buf); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		done += k
	}
}

func walState(t *testing.T, s *Store) (mx, bf uint32) {
	t.Helper()
	mx, bf, _, ok := walIndex(s.Path() + "-shm")
	if !ok {
		t.Fatal("WAL index not readable")
	}
	return mx, bf
}

// The writer's automatic checkpoint is only a hard cap: thousands of pages
// of WAL are not back-filled inside the commits (1.0.0: every 1,000 pages).
func TestNoCheckpointInsideCommits(t *testing.T) {
	p := Profile{Name: ProfileStandard, CheckpointSoftBytes: 1 << 40, CheckpointInterval: time.Hour}
	s := openTest(t, Options{Profile: p, FlushInterval: time.Hour})
	writePages(t, s, 3000)
	mx, bf := walState(t, s)
	if mx < 3000 {
		t.Fatalf("WAL frames %d, want >= 3000", mx)
	}
	if bf != 0 {
		t.Fatalf("frames back-filled inside commits: %d", bf)
	}
	if st := s.Stats(); st.Checkpoints != 0 || st.WALPendingBytes < 3000*4096 {
		t.Fatalf("stats %+v", st)
	}
}

// The soft size triggers a PASSIVE checkpoint by the checkpointer.
func TestCheckpointerSoftSize(t *testing.T) {
	p := Profile{Name: ProfileStandard, CheckpointSoftBytes: 2 << 20, CheckpointInterval: 30 * time.Second}
	s := openTest(t, Options{Profile: p, FlushInterval: time.Hour})
	writePages(t, s, 1200) // ~4.7 MiB
	deadline := time.Now().Add(10 * time.Second)
	for s.Stats().Checkpoints == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no checkpoint after the WAL passed the soft size")
		}
		time.Sleep(100 * time.Millisecond)
	}
	mx, bf := walState(t, s)
	if bf != mx {
		t.Fatalf("checkpoint left frames: mx %d backfill %d", mx, bf)
	}
	if st := s.Stats(); st.WALPendingBytes != 0 || st.LastCheckpoint == 0 {
		t.Fatalf("stats after checkpoint: %+v", st)
	}
	// The next write restarts the WAL from its beginning.
	writePages(t, s, 10)
	if mx2, _ := walState(t, s); mx2 > 20 {
		t.Fatalf("WAL not reset after a complete checkpoint: %d frames", mx2)
	}
}

// The interval triggers a checkpoint of a small WAL.
func TestCheckpointerInterval(t *testing.T) {
	p := Profile{Name: ProfileStandard, CheckpointSoftBytes: 1 << 40, CheckpointInterval: 2 * time.Second}
	s := openTest(t, Options{Profile: p, FlushInterval: time.Hour})
	writePages(t, s, 5)
	deadline := time.Now().Add(10 * time.Second)
	for s.Stats().Checkpoints == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no timed checkpoint")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Nothing new in the WAL: no further checkpoint is needed.
	n := s.Stats().Checkpoints
	time.Sleep(3 * time.Second)
	if got := s.Stats().Checkpoints; got > n+1 {
		t.Fatalf("checkpoints of an empty WAL: %d -> %d", n, got)
	}
}

// A reader holding an old snapshot keeps PASSIVE from back-filling; after
// three incomplete runs with a WAL above twice the soft size the
// checkpointer escalates to RESTART.
func TestCheckpointerEscalatesToRestart(t *testing.T) {
	p := Profile{Name: ProfileStandard, CheckpointSoftBytes: 1 << 20, CheckpointInterval: 30 * time.Second}
	s := openTest(t, Options{Profile: p, FlushInterval: time.Hour})
	writePages(t, s, 10)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	tx, err := s.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM ckpt_test`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	writePages(t, s, 800) // > 2 x soft, behind the reader's snapshot
	deadline := time.Now().Add(20 * time.Second)
	for s.Stats().CheckpointRestarts == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no escalation: %+v", s.Stats())
		}
		writePages(t, s, 20)
		time.Sleep(200 * time.Millisecond)
	}
	if s.Stats().CheckpointsIncomplete < 3 {
		t.Fatalf("incomplete %d", s.Stats().CheckpointsIncomplete)
	}
	_ = tx.Rollback()
	// With the reader gone the next checkpoint completes.
	deadline = time.Now().Add(20 * time.Second)
	for {
		writePages(t, s, 300)
		time.Sleep(1500 * time.Millisecond)
		if mx, bf := walState(t, s); mx == bf {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WAL never fully checkpointed after the reader ended")
		}
	}
}

// Truncate checkpoints are counted and run the before-truncate jobs.
func TestCheckpointTruncateRunsBeforeTruncateJobs(t *testing.T) {
	ran := 0
	registerPeriodic("test.before-truncate", func(Profile) time.Duration { return 0 }, true,
		func(ctx context.Context, s *Store) error { ran++; return nil })
	t.Cleanup(func() {
		hooks.mu.Lock()
		for i, p := range hooks.periodic {
			if p.name == "test.before-truncate" {
				hooks.periodic = append(hooks.periodic[:i], hooks.periodic[i+1:]...)
				break
			}
		}
		hooks.mu.Unlock()
	})
	s := openTest(t, manual)
	writePages(t, s, 50)
	if err := s.Checkpoint(bg); err != nil {
		t.Fatal(err)
	}
	if ran != 1 || s.Stats().CheckpointTruncates != 1 {
		t.Fatalf("ran %d truncates %d", ran, s.Stats().CheckpointTruncates)
	}
	if mx, _ := walState(t, s); mx != 0 {
		t.Fatalf("WAL not truncated: %d frames", mx)
	}
}

func TestCheckpointTick(t *testing.T) {
	for _, c := range []struct {
		in, want time.Duration
	}{{5 * time.Minute, 10 * time.Second}, {10 * time.Minute, 10 * time.Second}, {time.Minute, 2 * time.Second}, {time.Second, time.Second}} {
		if got := checkpointTick(Profile{CheckpointInterval: c.in}); got != c.want {
			t.Errorf("tick(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// A synchronous write shares one commit with the queued rows; when it
// fails, only its own changes are rolled back.
func TestSyncWriteSharesQueueCommit(t *testing.T) {
	s := openTest(t, manual)
	var b Batch
	b.AddEvents([]Event{{T: 1, Kind: "queued"}})
	s.Submit(&b)
	before := s.Stats().Flushes
	if err := s.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO events(t, kind) VALUES(2, 'sync')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Flushes != before+1 || st.CombinedCommits != 1 {
		t.Fatalf("flushes %d -> %d, combined %d", before, st.Flushes, st.CombinedCommits)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events`); n != 2 {
		t.Fatalf("events %d", n)
	}
	// The queued row is committed first (lower id).
	if k := scalar[string](t, s, `SELECT kind FROM events ORDER BY id LIMIT 1`); k != "queued" {
		t.Fatalf("order: first %q", k)
	}

	// A failing write: its rows are rolled back, the queued rows kept.
	b.AddEvents([]Event{{T: 3, Kind: "queued2"}})
	s.Submit(&b)
	boom := errors.New("boom")
	err := s.Write(bg, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO events(t, kind) VALUES(4, 'rolled back')`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events WHERE kind = 'rolled back'`); n != 0 {
		t.Fatal("failed write committed")
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events WHERE kind = 'queued2'`); n != 1 {
		t.Fatal("queued row lost with the failed write")
	}
	// A panicking write behaves like a failing one.
	b.AddEvents([]Event{{T: 5, Kind: "queued3"}})
	s.Submit(&b)
	if err := s.Write(bg, func(tx *sql.Tx) error { panic("oops") }); err == nil {
		t.Fatal("panic not reported")
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events WHERE kind = 'queued3'`); n != 1 {
		t.Fatal("queued row lost with the panicking write")
	}
	// A bad queued operation falls back to per-operation isolation.
	b.add("bad", tierHigh, 1, func(tc *txc) error {
		_, err := tc.tx.Exec(`INSERT INTO no_such_table VALUES(1)`)
		return err
	})
	b.AddEvents([]Event{{T: 6, Kind: "queued4"}})
	s.Submit(&b)
	if err := s.Write(bg, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO events(t, kind) VALUES(7, 'sync2')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM events WHERE kind IN ('queued4', 'sync2')`); n != 2 {
		t.Fatalf("isolation fallback lost rows: %d", n)
	}
	if s.Stats().FailedOps != 1 {
		t.Fatalf("failed ops %d", s.Stats().FailedOps)
	}
}

// The post-migration vacuum tail returns the freed pages one 2,000-page
// step per checkpointer tick (each with a checkpoint) and then ends; with
// a 30 s interval (1 s tick) 12,000 free pages are gone within seconds.
func TestVacuumTailRunsEveryTick(t *testing.T) {
	p := Profile{Name: ProfileStandard, CheckpointSoftBytes: 1 << 40, CheckpointInterval: 30 * time.Second, JournalSizeLimit: 64 << 20}
	s := openTest(t, Options{Profile: p, FlushInterval: time.Hour})
	writePages(t, s, 12000)
	if err := s.Write(bg, func(tx *sql.Tx) error { _, err := tx.Exec(`DELETE FROM ckpt_test`); return err }); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(bg); err != nil {
		t.Fatal(err)
	}
	free := scalar[int](t, s, `PRAGMA freelist_count`)
	if free < 11000 {
		t.Fatalf("free pages %d", free)
	}
	before := s.Stats().Checkpoints
	s.vacuumTail.Store(true)
	deadline := time.Now().Add(20 * time.Second)
	for s.vacuumTail.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("vacuum tail still running: %d pages returned", s.Stats().VacuumedPages)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if left := scalar[int](t, s, `PRAGMA freelist_count`); left != 0 {
		t.Fatalf("%d free pages left", left)
	}
	if st := s.Stats(); st.VacuumedPages < int64(free) || st.Checkpoints-before < uint64(free/vacuumTailPages) {
		t.Fatalf("vacuumed %d of %d pages, %d checkpoints", st.VacuumedPages, free, st.Checkpoints-before)
	}
}

// Prune returns a large free list in incremental-vacuum steps of up to
// 4,000 pages (16 MB of WAL each); a checkpoint between the steps keeps the
// WAL at about one step instead of all of them (the hard cap's automatic
// checkpoint would otherwise run inside a commit).
func TestReleaseFreePagesCheckpointsBetweenSteps(t *testing.T) {
	p := Profile{Name: ProfileStandard, CheckpointSoftBytes: 1 << 20, CheckpointInterval: time.Hour,
		WALHardCapPages: 1 << 20, JournalSizeLimit: 1 << 30}
	s := openTest(t, Options{Profile: p, FlushInterval: time.Hour})
	writePages(t, s, 4*vacuumPages+500)
	if err := s.Write(bg, func(tx *sql.Tx) error { _, err := tx.Exec(`DELETE FROM ckpt_test`); return err }); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(bg); err != nil {
		t.Fatal(err)
	}
	before := s.Stats().Checkpoints
	freed, err := s.releaseFreePages(bg)
	if err != nil {
		t.Fatal(err)
	}
	if freed < 3*vacuumPages {
		t.Fatalf("freed %d pages", freed)
	}
	if n := s.Stats().Checkpoints - before; n < 3 {
		t.Fatalf("%d checkpoints between %d vacuum steps", n, freed/vacuumPages)
	}
	fi, err := os.Stat(s.Path() + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 2*vacuumPages*4200 {
		t.Fatalf("WAL file %d bytes after the release (one step ~%d)", fi.Size(), vacuumPages*4120)
	}
}
