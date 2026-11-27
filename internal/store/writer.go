package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// tier ranks queued operations for the overflow policy: when the queue is
// full, tierLow rows are dropped first (oldest first), then tierMid, and
// tierHigh only when nothing else is left.
type tier uint8

const (
	tierLow  tier = iota // histogram / grid increments, raw met observations
	tierMid              // trail positions, NTP samples, receiver status
	tierHigh             // aircraft, flights, stats, weather, records, events, kv
)

// op is one queued typed operation (a slice of rows for one table).
type op struct {
	name string // table / operation name for logs and errors
	tier tier
	rows int
	run  func(tc *txc) error
}

// txc is the context an op runs in: the transaction plus access to the
// writer's prepared statements.
type txc struct {
	ctx   context.Context
	tx    *sql.Tx
	h     *handles
	local map[string]*sql.Stmt // statements bound to tx
}

// stmt returns the prepared statement for query bound to the current
// transaction. Statements listed in preparedSQL were prepared once on the
// writer connection and are reused across transactions; anything else is
// prepared for this transaction only.
func (tc *txc) stmt(query string) (*sql.Stmt, error) {
	if st, ok := tc.local[query]; ok {
		return st, nil
	}
	var st *sql.Stmt
	if base, ok := tc.h.stmts[query]; ok {
		st = tc.tx.StmtContext(tc.ctx, base)
	} else {
		var err error
		if st, err = tc.tx.PrepareContext(tc.ctx, query); err != nil {
			return nil, err
		}
	}
	if tc.local == nil {
		tc.local = make(map[string]*sql.Stmt, 8)
	}
	tc.local[query] = st
	return st, nil
}

// exec runs a prepared statement once.
func (tc *txc) exec(query string, args ...any) error {
	st, err := tc.stmt(query)
	if err != nil {
		return err
	}
	_, err = st.ExecContext(tc.ctx, args...)
	return err
}

// request is a synchronous job for the writer goroutine.
type request struct {
	ctx   context.Context
	fn    func(tx *sql.Tx) error                      // run inside a transaction
	raw   func(ctx context.Context, db *sql.DB) error // run outside a transaction (VACUUM, checkpoint)
	batch *Batch                                      // typed ops, one transaction
	done  chan error                                  // buffered, exactly one reply
	// noFlush: do not commit the queue first (data-migration steps).
	noFlush bool
}

func (s *Store) startWriter(h *handles) {
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.cdone = make(chan struct{})
	go s.run(h, s.stop, s.done)
	go s.runCheckpointer(h, s.stop, s.cdone)
}

// stopWriter stops the writer and checkpointer goroutines and waits for
// them. life must be held.
func (s *Store) stopWriter() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done
	<-s.cdone
	s.stop, s.done, s.cdone = nil, nil, nil
}

// run is the single writer goroutine: every write to the database happens
// here, either as a synchronous request or as a periodic flush of the queue.
func (s *Store) run(h *handles, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.opts.FlushInterval)
	defer ticker.Stop()
	for {
		// Stopping takes priority over new work.
		select {
		case <-stop:
			return
		default:
		}
		select {
		case <-stop:
			return
		case r := <-s.reqCh:
			s.handle(h, r)
		case <-s.kick:
			// After a failed flush the queue stays large and every enqueue
			// kicks again; wait for the timer instead of spinning on a
			// database that is in trouble.
			if time.Now().UnixNano() >= s.retryAt.Load() {
				_ = s.flushQueue(h)
			}
		case <-ticker.C:
			_ = s.flushQueue(h)
		case j := <-s.idleCh:
			// Idle-slot work (checkpoints): a due flush goes first.
			if s.writerBusy() {
				_ = s.flushQueue(h)
			}
			func() {
				defer close(j.done)
				_ = safely(func() error { j.fn(h); return nil })
			}()
		}
	}
}

func (s *Store) handle(h *handles, r *request) {
	if err := r.ctx.Err(); err != nil {
		r.done <- err
		return
	}
	// Keep submission order: everything queued before this request is
	// committed first (background data-migration steps do not touch the
	// queued tables' new rows and leave the queue to the group commit).
	// A transactional request shares one commit with the queue (combined).
	if !r.noFlush && (r.fn != nil || r.batch != nil) {
		if err, ok := s.runCombined(h, r); ok {
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				s.setLastError(err)
			}
			if err == nil {
				s.invalidateCounts()
			}
			r.done <- err
			return
		}
	}
	var flushErr error
	if !r.noFlush {
		flushErr = s.flushQueue(h)
	}

	var err error
	switch {
	case r.fn != nil:
		err = s.runTx(r.ctx, h, func(tc *txc) error { return r.fn(tc.tx) })
	case r.batch != nil:
		err = s.runTx(r.ctx, h, func(tc *txc) error {
			for i := range r.batch.ops {
				o := &r.batch.ops[i]
				if err := o.run(tc); err != nil {
					return fmt.Errorf("%s: %w", o.name, err)
				}
			}
			return nil
		})
	case r.raw != nil:
		err = safely(func() error { return r.raw(r.ctx, h.wdb) })
	default: // plain Flush
		err = flushErr
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.setLastError(err)
	}
	if err == nil {
		s.invalidateCounts()
	}
	r.done <- err
}

// requestOps runs the work of a transactional request inside tc.
func requestOps(r *request, tc *txc) error {
	if r.fn != nil {
		return r.fn(tc.tx)
	}
	for i := range r.batch.ops {
		o := &r.batch.ops[i]
		if err := o.run(tc); err != nil {
			return fmt.Errorf("%s: %w", o.name, err)
		}
	}
	return nil
}

// runCombined commits the queued operations and the synchronous request r
// in ONE transaction (design §3.2: a synchronous write no longer costs a
// commit of its own on top of the queue's): the queued operations first,
// then r inside a savepoint. When r fails, only its savepoint is rolled
// back and the queued rows are committed; r's error is returned. ok=false
// means nothing was queued or the queued operations failed on their own
// (then the caller falls back to flushQueue's per-operation isolation and
// runs r in a transaction of its own, as before).
func (s *Store) runCombined(h *handles, r *request) (err error, ok bool) {
	s.qmu.Lock()
	ops, rows := s.queue, s.qrows
	s.queue, s.qrows = nil, 0
	s.qmu.Unlock()
	if len(ops) == 0 {
		return nil, false
	}
	start := time.Now()
	bg := context.Background()
	tx, err := h.wdb.BeginTx(bg, nil)
	if err != nil {
		s.requeue(ops, rows)
		return nil, false
	}
	tc := &txc{ctx: bg, tx: tx, h: h}
	for i := range ops {
		if err := safely(func() error { return ops[i].run(tc) }); err != nil {
			_ = tx.Rollback()
			s.requeue(ops, rows)
			return nil, false // per-operation isolation path
		}
	}
	if _, err := tx.ExecContext(bg, `SAVEPOINT airview_req`); err != nil {
		_ = tx.Rollback()
		s.requeue(ops, rows)
		return nil, false
	}
	rc := &txc{ctx: r.ctx, tx: tx, h: h, local: tc.local}
	reqErr := safely(func() error { return requestOps(r, rc) })
	if reqErr != nil {
		if _, err := tx.ExecContext(bg, `ROLLBACK TO airview_req`); err != nil {
			_ = tx.Rollback()
			s.requeue(ops, rows)
			return nil, false
		}
	}
	if _, err := tx.ExecContext(bg, `RELEASE airview_req`); err != nil {
		_ = tx.Rollback()
		s.requeue(ops, rows)
		return nil, false
	}
	if err := tx.Commit(); err != nil {
		// The database itself is in trouble: keep the rows queued.
		s.nFlushErrors.Add(1)
		s.requeue(ops, rows)
		s.retryAt.Store(time.Now().Add(s.opts.FlushInterval).UnixNano())
		return err, true
	}
	s.retryAt.Store(0)
	s.nFlushes.Add(1)
	s.nFlushedRows.Add(uint64(rows))
	s.lastFlushNs.Store(time.Now().UnixNano())
	s.lastFlushDur.Store(int64(time.Since(start)))
	s.nCombined.Add(1)
	return reqErr, true
}

// runTx runs fn in one write transaction. A panic in fn is turned into an
// error (and a rollback) so that it cannot kill the writer.
func (s *Store) runTx(ctx context.Context, h *handles, fn func(tc *txc) error) (err error) {
	tx, err := h.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	tc := &txc{ctx: ctx, tx: tx, h: h}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			err = fmt.Errorf("store: panic in write transaction: %v", p)
		}
	}()
	if err = fn(tc); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func safely(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("store: panic in writer: %v", p)
		}
	}()
	return fn()
}

// submit hands r to the writer and waits for the reply.
func (s *Store) submit(r *request) error {
	if s.closed.Load() {
		return ErrClosed
	}
	select {
	case s.reqCh <- r:
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-s.closedCh:
		return ErrClosed
	}
	return <-r.done
}

// Write runs fn inside its own write transaction on the writer goroutine
// and returns after the transaction committed (fn returned nil) or was
// rolled back (fn returned an error, which Write returns; a panic in fn is
// returned as an error too). Rows queued earlier are committed before fn
// runs, so writes take effect in submission order.
//
// fn must not call Write, Flush, Apply or any other synchronous Store method
// (the writer is busy running fn: deadlock) and must not retain tx. Keep fn
// short - every other write waits behind it.
func (s *Store) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if fn == nil {
		return errors.New("store: Write: nil function")
	}
	return s.submit(&request{ctx: ctx, fn: fn, done: make(chan error, 1)})
}

// Apply writes the rows collected in b synchronously in one transaction (all
// or nothing) and resets b for reuse - also when it fails: the rows of a
// failed Apply are gone, they are not queued for a retry.
func (s *Store) Apply(ctx context.Context, b *Batch) error {
	if b == nil || len(b.ops) == 0 {
		return nil
	}
	moved := &Batch{ops: b.ops, rows: b.rows}
	b.ops, b.rows = nil, 0
	return s.submit(&request{ctx: ctx, batch: moved, done: make(chan error, 1)})
}

// Flush commits everything queued so far and returns when it is on disk
// (in the WAL). It returns the flush error, if any; rows that could not be
// written because of a database-level problem stay queued for a retry.
func (s *Store) Flush() error {
	return s.submit(&request{ctx: context.Background(), done: make(chan error, 1)})
}

// execRaw runs fn on the writer goroutine outside any transaction.
func (s *Store) execRaw(ctx context.Context, fn func(ctx context.Context, db *sql.DB) error) error {
	return s.submit(&request{ctx: ctx, raw: fn, done: make(chan error, 1)})
}

// Submit queues the rows collected in b for the next flush and resets b for
// reuse. It never blocks on the database. The operations of one Submit are
// committed together in one transaction unless the store has to fall back
// to per-operation isolation after an error (see flushQueue).
func (s *Store) Submit(b *Batch) {
	if b == nil || len(b.ops) == 0 {
		return
	}
	ops, rows := b.ops, b.rows
	b.ops, b.rows = nil, 0
	s.enqueue(ops, rows)
}

func (s *Store) enqueue(ops []op, rows int) {
	if s.closed.Load() {
		for i := range ops {
			s.nDropped[ops[i].tier].Add(uint64(ops[i].rows))
		}
		return
	}
	s.qmu.Lock()
	if s.queue == nil && s.nextFlush != nil {
		s.queue, s.nextFlush = s.nextFlush[:0], nil
	}
	s.queue = append(s.queue, ops...)
	s.qrows += rows
	if s.qrows > s.opts.MaxQueueRows {
		s.shedLocked()
	}
	kick := s.qrows >= s.opts.FlushRows
	s.qmu.Unlock()

	if kick {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// shedLocked enforces MaxQueueRows by dropping the oldest operations of the
// lowest tiers until the queue is back to 3/4 of the limit (the slack avoids
// rescanning the queue on every enqueue while the disk is stalled).
func (s *Store) shedLocked() {
	target := s.opts.MaxQueueRows / 4 * 3
	var dropped [3]int
	for t := tierLow; t <= tierHigh && s.qrows > target; t++ {
		kept := s.queue[:0]
		for i := range s.queue {
			o := s.queue[i]
			if o.tier == t && s.qrows > target {
				s.qrows -= o.rows
				dropped[t] += o.rows
				continue
			}
			kept = append(kept, o)
		}
		for i := len(kept); i < len(s.queue); i++ {
			s.queue[i] = op{} // release closures
		}
		s.queue = kept
	}
	for t, n := range dropped {
		if n > 0 {
			s.nDropped[t].Add(uint64(n))
		}
	}
	if now := time.Now(); now.Sub(s.lastShed) > 30*time.Second {
		s.lastShed = now
		s.log.Warn("store: write queue overflow, dropping queued rows (is the disk too slow or full?)",
			"dropped_low", dropped[tierLow], "dropped_mid", dropped[tierMid], "dropped_high", dropped[tierHigh],
			"queued_rows", s.qrows)
	}
}

// flushQueue commits the queued operations. It is called on the writer
// goroutine (and by Close after the writer stopped).
//
// Normal case: one transaction for everything. If that fails, the batch is
// replayed with every operation in its own savepoint, so a single bad
// operation (constraint violation, bad data) is dropped and counted while
// the rest is saved. If the database itself is in trouble (disk full, I/O
// error, locked), nothing is lost: the operations go back to the front of
// the queue and are retried on the next flush.
func (s *Store) flushQueue(h *handles) error {
	s.qmu.Lock()
	ops, rows := s.queue, s.qrows
	s.queue, s.qrows = nil, 0
	s.qmu.Unlock()
	if len(ops) == 0 {
		return nil
	}

	start := time.Now()
	ctx := context.Background()
	err := s.runTx(ctx, h, func(tc *txc) error {
		for i := range ops {
			if err := ops[i].run(tc); err != nil {
				return fmt.Errorf("%s: %w", ops[i].name, err)
			}
		}
		return nil
	})
	if err != nil && !isEnvironmentError(err) {
		s.log.Warn("store: batch failed, retrying operations one by one", "err", err)
		err = s.flushIsolated(ctx, h, ops)
	}
	if err != nil {
		s.nFlushErrors.Add(1)
		s.setLastError(err)
		// The flush is retried every FlushInterval; do not fill the log (on
		// the same full disk, typically) with one line per attempt.
		if now := time.Now().UnixNano(); now-s.lastFlushLog.Load() >= int64(30*time.Second) {
			s.lastFlushLog.Store(now)
			s.log.Error("store: flush failed, rows stay queued and are retried", "rows", rows, "err", err)
		}
		s.retryAt.Store(time.Now().Add(s.opts.FlushInterval).UnixNano())
		s.requeue(ops, rows)
		return err
	}

	s.retryAt.Store(0)
	s.nFlushes.Add(1)
	s.nFlushedRows.Add(uint64(rows))
	s.lastFlushNs.Store(time.Now().UnixNano())
	s.lastFlushDur.Store(int64(time.Since(start)))
	s.invalidateCounts()

	// Hand the backing array back for the next round.
	for i := range ops {
		ops[i] = op{}
	}
	s.qmu.Lock()
	if s.nextFlush == nil && cap(ops) <= 4096 {
		s.nextFlush = ops[:0]
	}
	s.qmu.Unlock()
	return nil
}

// flushIsolated replays ops in one transaction with a savepoint around each
// operation, skipping (and counting) the ones that fail on their own data.
func (s *Store) flushIsolated(ctx context.Context, h *handles, ops []op) error {
	failed := 0
	err := s.runTx(ctx, h, func(tc *txc) error {
		for i := range ops {
			if _, err := tc.tx.ExecContext(ctx, `SAVEPOINT airview_op`); err != nil {
				return err
			}
			// A panicking operation must not poison the whole queue forever.
			if err := safely(func() error { return ops[i].run(tc) }); err != nil {
				if isEnvironmentError(err) {
					return fmt.Errorf("%s: %w", ops[i].name, err)
				}
				if _, rerr := tc.tx.ExecContext(ctx, `ROLLBACK TO airview_op`); rerr != nil {
					return rerr
				}
				failed++
				s.setLastError(fmt.Errorf("%s: %w", ops[i].name, err))
				if failed <= 5 {
					s.log.Error("store: dropping operation that cannot be written",
						"op", ops[i].name, "rows", ops[i].rows, "err", err)
				}
			}
			if _, err := tc.tx.ExecContext(ctx, `RELEASE airview_op`); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.nFailedOps.Add(uint64(failed))
		if failed > 5 {
			s.log.Error("store: more operations dropped", "count", failed-5)
		}
	}
	return err
}

// requeue puts ops back in front of whatever was queued meanwhile.
func (s *Store) requeue(ops []op, rows int) {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	s.queue = append(ops, s.queue...)
	s.qrows += rows
	if s.qrows > s.opts.MaxQueueRows {
		s.shedLocked()
	}
}
