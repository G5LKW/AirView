package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// The checkpointer (docs/perf/DB-EFFICIENCY-DESIGN.md §3.1).
//
// SQLite's own automatic checkpoint runs inside the COMMIT of the
// connection that pushes the WAL over wal_autocheckpoint pages: on a 1.0.0
// station that was every ~4 MiB of WAL (~93 times an hour), each time with
// two fsyncs and a burst of random page writes into the database file, and
// always inside the writer's commit. Now:
//
//   - wal_autocheckpoint of the writer is only a hard cap
//     (Profile.WALHardCapPages: 64 MiB on stations, 512 MiB on servers);
//   - this goroutine watches the WAL index (the -shm header: frames written
//     and frames already back-filled; reading 100 bytes, no lock) and asks
//     for a PASSIVE checkpoint when the un-checkpointed WAL exceeds
//     Profile.CheckpointSoftBytes or Profile.CheckpointInterval has passed
//     since the last one, whichever comes first;
//   - the checkpoint itself runs on a dedicated connection inside the
//     writer goroutine's idle slot (between two transactions, never inside
//     a commit, and only while no flush is pending);
//   - when a long reader keeps a PASSIVE checkpoint from back-filling
//     everything three times in a row and the WAL is above twice the soft
//     size, it escalates to RESTART (1 s busy timeout);
//   - the hourly TRUNCATE (Checkpoint, HourlyMaintenance) is unchanged;
//   - journal_size_limit truncates a WAL that grew (migration, export);
//   - after the data migrations, the incremental-vacuum tail returns the
//     freed pages, 2,000 per tick (each with a checkpoint), until none is
//     left.
//
// Pages rewritten many times between two checkpoints (heat cells, today's
// histogram bins, the trail tail) reach the database file once.

// checkpointer state; the goroutine belongs to one handles generation.
type ckptCounters struct {
	passive    atomic.Uint64 // PASSIVE checkpoints run
	restarts   atomic.Uint64 // escalations to RESTART
	truncates  atomic.Uint64 // TRUNCATE checkpoints (Checkpoint)
	incomplete atomic.Uint64 // PASSIVE runs that could not back-fill everything
	lastNs     atomic.Int64  // UnixNano of the last checkpoint of any kind
	lastDurNs  atomic.Int64
	lastFrames atomic.Int64 // frames back-filled by the last one
	vacuumed   atomic.Int64 // pages released by the post-migration vacuum tail
}

// walIndex reads the WAL-index header from the -shm file: mxFrame (frames
// in the WAL since its last reset), nBackfill (frames already copied into
// the database) and the page size. ok=false when the file is missing or
// unreadable (then only the timer triggers checkpoints).
//
// Layout (sqlite/src/wal.c, native byte order): WalIndexHdr at 0 (48
// bytes: iVersion, unused, iChange, isInit u8, bigEndCksum u8, szPage u16
// at 14, mxFrame at 16, nPage at 20 ...), a second copy at 48, then
// WalCkptInfo at 96 (nBackfill at 96, read marks 100..119, locks 120..127).
// Only bytes 0..99 are read, so the lock bytes - which Windows locks
// mandatorily - are never touched.
func walIndex(shmPath string) (mxFrame, nBackfill uint32, pageSize int64, ok bool) {
	f, err := os.Open(shmPath)
	if err != nil {
		return 0, 0, 0, false
	}
	defer f.Close()
	var b [100]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return 0, 0, 0, false
	}
	ne := binary.NativeEndian
	if b[12] == 0 { // isInit
		return 0, 0, 0, false
	}
	sz := int64(ne.Uint16(b[14:16]))
	if sz == 1 {
		sz = 65536
	}
	if sz < 512 {
		return 0, 0, 0, false
	}
	return ne.Uint32(b[16:20]), ne.Uint32(b[96:100]), sz, true
}

// walPendingBytes is the size of the WAL content not yet checkpointed.
func (s *Store) walPendingBytes() (int64, bool) {
	mx, bf, sz, ok := walIndex(s.path + "-shm")
	if !ok {
		return 0, false
	}
	if bf > mx {
		return 0, true
	}
	return int64(mx-bf) * sz, true
}

// checkpointerDSN: a normal (not query_only) connection with a short busy
// timeout that never commits anything itself.
func checkpointerDSN(path string, p Profile) string {
	q := baseValues("1000")
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Add("_pragma", "wal_autocheckpoint(0)")
	q.Add("_pragma", "journal_size_limit("+strconv.FormatInt(p.JournalSizeLimit, 10)+")")
	q.Add("_pragma", "cache_size(-256)")
	return dsn(path, q)
}

func openCheckpointConn(path string, p Profile) (*sql.DB, error) {
	db, err := sql.Open("sqlite", checkpointerDSN(path, p))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	return db, nil
}

// idleJob is work for the writer goroutine's idle slot.
type idleJob struct {
	fn   func(h *handles)
	done chan struct{}
}

// runIdle hands fn to the writer goroutine and waits until it ran (or the
// writer stopped). It reports whether fn ran.
func (s *Store) runIdle(stop <-chan struct{}, fn func(h *handles)) bool {
	j := &idleJob{fn: fn, done: make(chan struct{})}
	select {
	case s.idleCh <- j:
	case <-stop:
		return false
	case <-s.closedCh:
		return false
	}
	select {
	case <-j.done:
		return true
	case <-stop:
		return false
	}
}

// writerBusy reports whether a flush is due (the idle slot is then left to
// the flush first).
func (s *Store) writerBusy() bool {
	s.qmu.Lock()
	busy := s.qrows >= s.opts.FlushRows
	s.qmu.Unlock()
	return busy || len(s.kick) > 0
}

// checkpointTick is how often the checkpointer looks at the WAL index.
func checkpointTick(p Profile) time.Duration {
	t := p.CheckpointInterval / 30
	return min(max(t, time.Second), 10*time.Second)
}

// runCheckpointer is the checkpointer goroutine of one handles generation.
func (s *Store) runCheckpointer(h *handles, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	p := s.opts.Profile
	tick := time.NewTicker(checkpointTick(p))
	defer tick.Stop()
	last := time.Now()
	incomplete := 0
	periodic := map[string]time.Time{}
	start := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		now := time.Now()

		// Periodic jobs of other owners (trail seal ...), on this goroutine.
		for _, j := range registeredPeriodic() {
			every := time.Duration(0)
			if j.every != nil {
				every = j.every(p)
			}
			if every <= 0 {
				continue
			}
			lastRun, ok := periodic[j.name]
			if !ok {
				lastRun = start
			}
			if now.Sub(lastRun) < every {
				continue
			}
			periodic[j.name] = now
			if err := safely(func() error { return j.fn(ctx, s) }); err != nil && ctx.Err() == nil {
				s.log.Warn("store: periodic job failed", "job", j.name, "err", err)
			}
			if ctx.Err() != nil {
				return
			}
		}

		pending, known := s.walPendingBytes()
		due := known && pending >= p.CheckpointSoftBytes
		if now.Sub(last) >= p.CheckpointInterval && (!known || pending > 0) {
			due = true
		}
		if !due && s.vacuumTail.Load() {
			// The post-migration vacuum tail: one step (and the checkpoint
			// that writes the previous step's pages back) every tick, so
			// a migrated 7-day file is back to its size within minutes
			// instead of hours; it ends by itself (vacuumTailStep).
			due = true
		}
		if !due {
			continue
		}
		if s.writerBusy() {
			continue // next tick; a flush is due first
		}
		restart := incomplete >= 3 && known && pending > 2*p.CheckpointSoftBytes
		var res ckptResult
		ran := s.runIdle(stop, func(h *handles) { res = s.checkpointNow(h, restart) })
		if !ran {
			return
		}
		last = time.Now()
		if res.err != nil {
			s.log.Debug("store: checkpoint failed", "err", res.err)
			continue
		}
		if res.log > res.done {
			incomplete++
		} else {
			incomplete = 0
		}
	}
}

// checkpointIfFull runs a PASSIVE checkpoint in the writer's idle slot when
// the un-checkpointed WAL has reached Profile.CheckpointSoftBytes. Bulk
// work that writes far faster than the station (data-migration steps, the
// incremental-vacuum steps of Prune and Maintain) calls it between its
// transactions, so the WAL stays near the soft size instead of reaching
// the hard cap, whose automatic checkpoint would run inside a commit. It
// must not be called on the writer goroutine.
func (s *Store) checkpointIfFull(ctx context.Context) {
	soft := s.opts.Profile.CheckpointSoftBytes
	if soft <= 0 {
		return
	}
	if pending, ok := s.walPendingBytes(); !ok || pending < soft {
		return
	}
	s.runIdle(ctx.Done(), func(h *handles) {
		if r := s.checkpointNow(h, false); r.err != nil {
			s.log.Debug("store: checkpoint between bulk steps failed", "err", r.err)
		}
	})
}

type ckptResult struct {
	log, done int64
	err       error
}

// checkpointNow runs one PASSIVE (or RESTART) checkpoint on the dedicated
// connection, then one step of the post-migration vacuum tail on the
// writer connection. It runs on the writer goroutine.
func (s *Store) checkpointNow(h *handles, restart bool) (r ckptResult) {
	if h.cdb == nil {
		return ckptResult{err: ErrClosed}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	mode := "PASSIVE"
	if restart {
		mode = "RESTART"
	}
	t0 := time.Now()
	var busy sql.NullInt64
	var logF, doneF sql.NullInt64
	err := h.cdb.QueryRowContext(ctx, `PRAGMA wal_checkpoint(`+mode+`)`).Scan(&busy, &logF, &doneF)
	if err != nil {
		return ckptResult{err: err}
	}
	s.ckpt.lastNs.Store(time.Now().UnixNano())
	s.ckpt.lastDurNs.Store(int64(time.Since(t0)))
	s.ckpt.lastFrames.Store(doneF.Int64)
	if restart {
		s.ckpt.restarts.Add(1)
	} else {
		s.ckpt.passive.Add(1)
	}
	if logF.Int64 > doneF.Int64 {
		s.ckpt.incomplete.Add(1)
	}
	r = ckptResult{log: logF.Int64, done: doneF.Int64}

	if s.vacuumTail.Load() {
		s.vacuumTailStep(ctx, h)
	}
	return r
}

// vacuumTailPages is the incremental-vacuum step after the data
// migrations (one step per checkpoint, §5.1).
const vacuumTailPages = 2000

// vacuumTailStep returns up to vacuumTailPages free pages to the file
// system; it clears vacuumTail when nothing is left.
func (s *Store) vacuumTailStep(ctx context.Context, h *handles) {
	var free int64
	if err := h.wdb.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return
	}
	if free == 0 {
		s.vacuumTail.Store(false)
		return
	}
	if err := drain(h.wdb.QueryContext(ctx, `PRAGMA incremental_vacuum(`+strconv.Itoa(vacuumTailPages)+`)`)); err != nil {
		s.log.Debug("store: incremental vacuum step failed", "err", err)
		return
	}
	var after int64
	if err := h.wdb.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&after); err != nil {
		return
	}
	if after < free {
		s.ckpt.vacuumed.Add(free - after)
	}
	if after == 0 || after >= free {
		s.vacuumTail.Store(false) // done, or auto_vacuum is off
	}
}
