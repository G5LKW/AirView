package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Data migrations (docs/perf/DB-EFFICIENCY-DESIGN.md §5.1).
//
// DDL migrations (RegisterMigration) run once, inside Open. A data
// migration converts existing rows into a new layout in the background, in
// small transactions, while the station keeps running:
//
//   - progress (an opaque cursor) is stored in schema_data_migrations in the
//     same transaction as the step's own changes, so a crash, a kill or a
//     power cut at any moment resumes exactly after the last committed step:
//     never twice, never skipped;
//   - steps run on the writer goroutine between two flushes (never inside
//     one), each bounded by a row budget that the runner adapts so that one
//     step takes about Profile.MigrationStep (200 ms, low-wear 100 ms), with a
//     pause of the same length after it; the runner waits while the write
//     queue holds a flush worth of rows;
//   - before the start and every 100th step the free disk space must cover
//     the migration's peak plus max(256 MiB, 10 % of the file), else the
//     migration waits (state "waiting_for_disk", re-checked every 10 min);
//   - before the first step a backup may be written (VACUUM INTO, §5.3);
//   - when every migration is done: PRAGMA optimize, then the freed pages
//     are returned to the file system, 2,000 pages per checkpointer tick
//     (10 s on stations, 2 s on servers), each step with a checkpoint.
//
// Readers of a table under migration must understand both layouts until
// DataMigrationDone reports the migration done.

// Querier is the read side of *sql.DB, *sql.Tx and *sql.Conn.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DataMigration converts existing rows in the background, in small
// transactions, after the DDL migrations ran.
type DataMigration struct {
	Name    string // owner name, e.g. "trail"; unique with Version
	Version int
	Kinds   Kind     // database kinds (0 = KindMain)
	After   []string // names that must be done first
	// Estimate returns the rows left and the extra disk bytes the rest of
	// the migration needs at its peak (used by the disk-space check). It
	// runs on the read pool; nil means (0, 0).
	Estimate func(ctx context.Context, q Querier) (rows, peakBytes int64, err error)
	// Step converts at most budget rows inside tx and returns the new
	// cursor; done=true marks the migration complete. A step must be
	// idempotent for a given cursor (it may be re-run after a crash, never
	// after its commit).
	Step func(ctx context.Context, tx *sql.Tx, cursor []byte, budget int) (next []byte, done bool, err error)
	// Finish runs once after done (e.g. DROP the emptied legacy table), in
	// its own transaction; nil = nothing.
	Finish func(ctx context.Context, tx *sql.Tx) error
}

func (m DataMigration) key() string { return m.Name + "/" + strconv.Itoa(m.Version) }

func (m DataMigration) kinds() Kind {
	if m.Kinds&kindAll == 0 {
		return KindMain
	}
	return m.Kinds & kindAll
}

var dataMigs struct {
	mu   sync.Mutex
	list []DataMigration
}

// registerDataMigration adds a data migration (call from an init function
// of the owner's file). It panics on an invalid or conflicting
// registration: that is a programming error.
func registerDataMigration(m DataMigration) {
	if m.Name == "" || m.Version < 1 || m.Step == nil {
		panic("store: registerDataMigration: name, version >= 1 and Step are required")
	}
	dataMigs.mu.Lock()
	defer dataMigs.mu.Unlock()
	for _, e := range dataMigs.list {
		if e.Name == m.Name && e.Version == m.Version {
			panic("store: data migration " + m.key() + " registered twice")
		}
	}
	dataMigs.list = append(dataMigs.list, m)
}

// dataMigrations returns the migrations of the store's kind in run order:
// dependencies (After) first, then by name and version.
func (s *Store) dataMigrations() []DataMigration {
	var all []DataMigration
	if s.opts.testDataMigs != nil {
		all = append(all, s.opts.testDataMigs...)
	} else {
		dataMigs.mu.Lock()
		all = append(all, dataMigs.list...)
		dataMigs.mu.Unlock()
	}
	var list []DataMigration
	for _, m := range all {
		if m.kinds()&s.kind != 0 {
			list = append(list, m)
		}
	}
	return orderDataMigrations(list)
}

// orderDataMigrations sorts by (name, version) and then moves every
// migration behind the migrations named in its After (stable; a cycle
// leaves the remaining ones in name order).
func orderDataMigrations(list []DataMigration) []DataMigration {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		return list[i].Version < list[j].Version
	})
	var out []DataMigration
	placed := map[string]bool{} // keys
	remaining := append([]DataMigration(nil), list...)
	for len(remaining) > 0 {
		progress := false
		var next []DataMigration
		for _, m := range remaining {
			ok := true
			for _, dep := range m.After {
				for _, o := range list {
					if o.Name == dep && o.key() != m.key() && !placed[o.key()] {
						ok = false
					}
				}
			}
			// Earlier versions of the same name first.
			for _, o := range list {
				if o.Name == m.Name && o.Version < m.Version && !placed[o.key()] {
					ok = false
				}
			}
			if ok {
				out = append(out, m)
				placed[m.key()] = true
				progress = true
			} else {
				next = append(next, m)
			}
		}
		if !progress {
			return append(out, next...)
		}
		remaining = next
	}
	return out
}

const schemaDataMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_data_migrations(
  name       TEXT NOT NULL,
  version    INTEGER NOT NULL,
  cursor     BLOB,
  done       INTEGER NOT NULL DEFAULT 0,
  started    INTEGER,
  updated    INTEGER,
  rows_done  INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  PRIMARY KEY(name, version)
) WITHOUT ROWID`

// done values of schema_data_migrations.
const (
	migInProgress = 0 // steps still to run
	migStepped    = 1 // Step reported done; Finish still to run
	migComplete   = 2
)

// Migration states reported by MigrationStatus.
const (
	MigPending          = "pending"
	MigRunning          = "running"
	MigWaitingForDisk   = "waiting_for_disk"
	MigWaitingForBackup = "waiting_for_backup"
	MigFailed           = "failed"
	MigDone             = "done"
)

// MigrationState is the progress of one data migration.
type MigrationState struct {
	Name      string `json:"name"`
	Version   int    `json:"version"`
	State     string `json:"state"`
	RowsLeft  int64  `json:"rowsLeft"` // estimate
	RowsDone  int64  `json:"rowsDone"` // approximate (SQLite change counter / 2)
	Steps     int64  `json:"steps"`    // steps run by this process
	Started   int64  `json:"started"`  // Unix ms, 0 = not started
	Updated   int64  `json:"updated"`  // Unix ms of the last step
	ETA       int64  `json:"eta"`      // Unix ms, 0 = unknown
	LastError string `json:"lastError,omitempty"`
}

// MigrationInfo is the data-migration block of /api/db/stats.
type MigrationInfo struct {
	// State: "none" (nothing registered), "pending", "running",
	// "waiting_for_disk", "waiting_for_backup", "failed" or "done".
	State      string           `json:"state"`
	Migrations []MigrationState `json:"migrations"`
	BackupMode string           `json:"backupMode"`
	Backup     string           `json:"backup,omitempty"` // pre-migration backup file (path)
	VacuumTail bool             `json:"vacuumTail"`       // free pages still being returned
	Hint       string           `json:"hint,omitempty"`
}

var (
	errWaitDisk   = errors.New("store: data migration: not enough free disk space")
	errWaitBackup = errors.New("store: data migration: waiting for the pre-migration backup")
)

// migrator runs the data migrations of one store in the background.
type migrator struct {
	s *Store

	mu     sync.Mutex
	states map[string]*MigrationState
	hint   string
	backup string
	cancel context.CancelFunc
	done   chan struct{}

	fast     atomic.Bool   // MigrateNow: no pauses
	wake     chan struct{} // cuts a wait short
	complete atomic.Bool   // every migration done (this generation)
	doneSet  atomic.Pointer[map[string]bool]
	backupOK bool // backup decision taken in this process

	recheck  time.Duration // disk / backup re-check interval
	diskFree func(dir string) (uint64, error)
	hook     func(name string, step int, phase string) error // tests
}

func newMigrator(s *Store) *migrator {
	m := &migrator{s: s, states: map[string]*MigrationState{}, wake: make(chan struct{}, 1),
		recheck: 10 * time.Minute, diskFree: diskFree}
	if s.opts.testDiskFree != nil {
		m.diskFree = s.opts.testDiskFree
	}
	m.hook = s.opts.testMigHook
	if s.opts.testRecheck > 0 {
		m.recheck = s.opts.testRecheck
	}
	empty := map[string]bool{}
	m.doneSet.Store(&empty)
	return m
}

// start launches the background runner after delay (< 0: not at all).
func (m *migrator) start(delay time.Duration) {
	m.reload()
	if delay < 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel, m.done = cancel, make(chan struct{})
	go m.run(ctx, delay, m.done)
}

// stop cancels the runner and waits for it (between two steps).
func (m *migrator) stop() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (m *migrator) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func sleepCtx(ctx context.Context, d time.Duration, wake <-chan struct{}) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-wake:
		return true
	}
}

func (m *migrator) run(ctx context.Context, delay time.Duration, done chan struct{}) {
	defer close(done)
	if !m.fast.Load() && !sleepCtx(ctx, delay, m.wake) {
		return
	}
	for {
		err := m.pass(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		wait := time.Minute
		if errors.Is(err, errWaitDisk) || errors.Is(err, errWaitBackup) {
			wait = m.recheck
		}
		if !sleepCtx(ctx, wait, m.wake) {
			return
		}
	}
}

type migRow struct {
	cursor    []byte
	done      int
	started   int64
	updated   int64
	rowsDone  int64
	lastError string
}

func loadMigRows(ctx context.Context, q Querier) (map[string]migRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name, version, cursor, done, COALESCE(started, 0), COALESCE(updated, 0), rows_done, COALESCE(last_error, '')
FROM schema_data_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]migRow{}
	for rows.Next() {
		var name string
		var ver int
		var r migRow
		if err := rows.Scan(&name, &ver, &r.cursor, &r.done, &r.started, &r.updated, &r.rowsDone, &r.lastError); err != nil {
			return nil, err
		}
		out[name+"/"+strconv.Itoa(ver)] = r
	}
	return out, rows.Err()
}

// reload refreshes the in-memory states and the done set from the table.
func (m *migrator) reload() {
	s := m.s
	if s.closed.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := loadMigRows(ctx, s.DB())
	if err != nil {
		s.log.Debug("store: reading data-migration progress", "err", err)
		return
	}
	set := map[string]bool{}
	m.mu.Lock()
	defer m.mu.Unlock()
	allDone := true
	for _, dm := range s.dataMigrations() {
		k := dm.key()
		st := m.states[k]
		if st == nil {
			st = &MigrationState{Name: dm.Name, Version: dm.Version, State: MigPending}
			m.states[k] = st
		}
		if r, ok := rows[k]; ok {
			st.Started, st.Updated, st.RowsDone, st.LastError = r.started*1000, r.updated*1000, r.rowsDone, r.lastError
			if r.done == migComplete {
				st.State, st.RowsLeft, st.ETA = MigDone, 0, 0
				set[k] = true
			}
		}
		if !set[k] {
			allDone = false
		}
	}
	m.doneSet.Store(&set)
	m.complete.Store(allDone)
}

// DataMigrationDone reports whether every registered data migration of
// that name (all versions, for this database kind) has completed. Readers
// that still have to understand a legacy layout use it; it is cheap (an
// in-memory set refreshed when a migration completes).
func (s *Store) DataMigrationDone(name string) bool {
	set := *s.mig.doneSet.Load()
	found := false
	for _, dm := range s.dataMigrations() {
		if dm.Name != name {
			continue
		}
		found = true
		if !set[dm.key()] {
			return false
		}
	}
	return found
}

// MigrationStatus returns the state of every data migration of this
// database, in run order.
func (s *Store) MigrationStatus() []MigrationState {
	m := s.mig
	m.mu.Lock()
	empty := len(m.states) == 0
	m.mu.Unlock()
	if empty {
		m.reload()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MigrationState
	for _, dm := range s.dataMigrations() {
		if st := m.states[dm.key()]; st != nil {
			out = append(out, *st)
		} else {
			out = append(out, MigrationState{Name: dm.Name, Version: dm.Version, State: MigPending})
		}
	}
	return out
}

// MigrationInfo summarises the data migrations for /api/db/stats.
func (s *Store) MigrationInfo() MigrationInfo {
	list := s.MigrationStatus()
	info := MigrationInfo{Migrations: list, BackupMode: s.opts.Profile.BackupBeforeMigrate,
		VacuumTail: s.vacuumTail.Load()}
	if info.Migrations == nil {
		info.Migrations = []MigrationState{}
	}
	s.mig.mu.Lock()
	info.Hint, info.Backup = s.mig.hint, s.mig.backup
	s.mig.mu.Unlock()
	if len(list) == 0 {
		info.State = "none"
		return info
	}
	rank := map[string]int{MigDone: 0, MigPending: 1, MigRunning: 2, MigWaitingForBackup: 3, MigWaitingForDisk: 4, MigFailed: 5}
	info.State = MigDone
	for _, st := range list {
		if rank[st.State] > rank[info.State] {
			info.State = st.State
		}
	}
	return info
}

func (m *migrator) setState(dm DataMigration, fn func(st *MigrationState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.states[dm.key()]
	if st == nil {
		st = &MigrationState{Name: dm.Name, Version: dm.Version, State: MigPending}
		m.states[dm.key()] = st
	}
	fn(st)
}

// MigrateNow runs the pending data migrations without pauses and returns
// when they are done (or ctx ends). The background runner, if running,
// switches to the same pace; it is used by `airview db migrate --now`.
func (s *Store) MigrateNow(ctx context.Context) error {
	if s.closed.Load() {
		return ErrClosed
	}
	m := s.mig
	m.fast.Store(true)
	defer m.fast.Store(false)
	m.mu.Lock()
	running := m.done != nil
	m.mu.Unlock()
	if !running {
		return m.pass(ctx)
	}
	m.poke()
	for {
		if m.complete.Load() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.closedCh:
			return ErrClosed
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// pass runs every pending migration once to completion. It returns nil
// when all are done.
func (m *migrator) pass(ctx context.Context) error {
	s := m.s
	list := s.dataMigrations()
	if len(list) == 0 {
		m.complete.Store(true)
		return nil
	}
	rows, err := loadMigRows(ctx, s.DB())
	if err != nil {
		return err
	}
	var pending []DataMigration
	for _, dm := range list {
		if rows[dm.key()].done != migComplete {
			pending = append(pending, dm)
		}
	}
	if len(pending) == 0 {
		m.reload()
		m.allDone(ctx, false)
		return nil
	}

	// Backup decision (once per database).
	if !m.backupOK {
		var total, peak int64
		for _, dm := range pending {
			r, p, err := m.estimate(ctx, dm)
			if err != nil {
				return err
			}
			total += r
			peak = max(peak, p)
		}
		if total > 0 {
			if err := m.backupBeforeMigrate(ctx); err != nil {
				for _, dm := range pending {
					m.setState(dm, func(st *MigrationState) { st.State = MigWaitingForBackup; st.LastError = err.Error() })
				}
				return err
			}
		}
		m.backupOK = true
	}

	for _, dm := range pending {
		if err := m.runOne(ctx, dm); err != nil {
			return err
		}
	}
	m.reload()
	m.allDone(ctx, true)
	return nil
}

func (m *migrator) estimate(ctx context.Context, dm DataMigration) (rows, peak int64, err error) {
	if dm.Estimate == nil {
		return 0, 0, nil
	}
	err = safely(func() error {
		var e error
		rows, peak, e = dm.Estimate(ctx, m.s.DB())
		return e
	})
	if err != nil {
		return 0, 0, fmt.Errorf("store: data migration %s: estimate: %w", dm.key(), err)
	}
	return rows, peak, nil
}

// diskOK checks free space >= peak + max(256 MiB, 10 % of the file).
func (m *migrator) diskOK(peak int64) (free uint64, ok bool) {
	f, err := m.diskFree(m.s.dir)
	if err != nil {
		m.s.log.Debug("store: free disk space unknown", "err", err)
		return 0, true // cannot tell: do not block the migration
	}
	need := peak + max(256<<20, m.s.DBSize()/10)
	return f, int64(f) >= need
}

func (m *migrator) waitDisk(dm DataMigration, free uint64, peak int64) error {
	m.setState(dm, func(st *MigrationState) { st.State = MigWaitingForDisk })
	m.s.log.Warn("store: data migration paused: not enough free disk space",
		"migration", dm.key(), "free_mb", free>>20, "needed_mb", (peak+max(256<<20, m.s.DBSize()/10))>>20,
		"recheck", m.recheck)
	return errWaitDisk
}

// Budget bounds of a step (rows).
const (
	migBudgetStart = 256
	migBudgetMax   = 1 << 20
)

// runOne steps dm to completion.
func (m *migrator) runOne(ctx context.Context, dm DataMigration) error {
	s := m.s
	rowsLeft, peak, err := m.estimate(ctx, dm)
	if err != nil {
		m.setState(dm, func(st *MigrationState) { st.State = MigFailed; st.LastError = err.Error() })
		return err
	}
	if free, ok := m.diskOK(peak); !ok {
		return m.waitDisk(dm, free, peak)
	}
	m.setState(dm, func(st *MigrationState) {
		st.State, st.RowsLeft = MigRunning, rowsLeft
		if st.Started == 0 {
			st.Started = time.Now().UnixMilli()
		}
	})
	s.log.Info("store: data migration running", "migration", dm.key(), "rows", rowsLeft)

	target := s.opts.Profile.MigrationStep
	if target <= 0 {
		target = 200 * time.Millisecond
	}
	budget := migBudgetStart
	var doneRows int64
	t0 := time.Now()
	for step := 1; ; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Free space before every 100th step (with the last peak estimate;
		// the estimate itself, which may count rows, every 1,000th).
		if step%1000 == 0 {
			if r, p, err := m.estimate(ctx, dm); err == nil {
				rowsLeft, peak, doneRows = r, p, 0
				t0 = time.Now()
			}
		}
		if step%100 == 0 {
			if free, ok := m.diskOK(peak); !ok {
				return m.waitDisk(dm, free, peak)
			}
		}
		// The group commit goes first.
		for s.writerBusy() {
			if !sleepCtx(ctx, 20*time.Millisecond, nil) {
				return ctx.Err()
			}
		}

		start := time.Now()
		var finished, already bool
		var changed int64
		err := s.submit(&request{ctx: ctx, noFlush: true, done: make(chan error, 1), fn: func(tx *sql.Tx) error {
			finished, already, changed = false, false, 0
			var cursor []byte
			var state int
			err := tx.QueryRowContext(ctx, `SELECT cursor, done FROM schema_data_migrations WHERE name = ? AND version = ?`,
				dm.Name, dm.Version).Scan(&cursor, &state)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if state != migInProgress {
				already = true
				return nil
			}
			var before, after int64
			if err := tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&before); err != nil {
				return err
			}
			var next []byte
			var fin bool
			if err := safely(func() error {
				var e error
				next, fin, e = dm.Step(ctx, tx, cursor, budget)
				return e
			}); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&after); err != nil {
				return err
			}
			changed = (after - before + 1) / 2
			newState := migInProgress
			if fin {
				newState = migStepped
				if dm.Finish == nil {
					newState = migComplete
				}
			}
			now := time.Now().Unix()
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_data_migrations(name, version, cursor, done, started, updated, rows_done)
VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name, version) DO UPDATE SET cursor = excluded.cursor, done = excluded.done,
  updated = excluded.updated, rows_done = rows_done + excluded.rows_done, last_error = NULL`,
				dm.Name, dm.Version, next, newState, now, now, changed); err != nil {
				return err
			}
			if m.hook != nil {
				if err := m.hook(dm.Name, step, "before-commit"); err != nil {
					return err
				}
			}
			finished = fin
			return nil
		}})
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrClosed) {
				return err
			}
			m.fail(dm, err)
			return err
		}
		if m.hook != nil {
			if err := m.hook(dm.Name, step, "after-commit"); err != nil {
				return err
			}
		}
		dur := time.Since(start)
		doneRows += changed
		m.setState(dm, func(st *MigrationState) {
			st.Steps++
			st.RowsDone += changed
			st.Updated = time.Now().UnixMilli()
			st.RowsLeft = max(rowsLeft-doneRows, 0)
			st.LastError = ""
			if el := time.Since(t0); doneRows > 0 && st.RowsLeft > 0 {
				st.ETA = time.Now().Add(time.Duration(float64(el) * float64(st.RowsLeft) / float64(doneRows))).UnixMilli()
			}
		})
		if finished || already {
			break
		}
		// A migration writes far faster than the station: checkpoint as
		// soon as the WAL holds the soft size, so the WAL stays at about
		// the soft size instead of reaching the hard cap (whose automatic
		// checkpoint would run inside the next commit).
		s.checkpointIfFull(ctx)
		// Adapt the budget to the target step time.
		switch {
		case dur < target/2 && budget < migBudgetMax:
			budget = min(budget*2, migBudgetMax)
		case dur > target*3/2 && budget > 1:
			budget = max(1, int(float64(budget)*float64(target)/float64(dur)))
		}
		if !m.fast.Load() && !sleepCtx(ctx, dur, nil) {
			return ctx.Err()
		}
	}

	if dm.Finish != nil {
		err := s.submit(&request{ctx: ctx, noFlush: true, done: make(chan error, 1), fn: func(tx *sql.Tx) error {
			var state int
			if err := tx.QueryRowContext(ctx, `SELECT done FROM schema_data_migrations WHERE name = ? AND version = ?`,
				dm.Name, dm.Version).Scan(&state); err != nil {
				return err
			}
			if state != migStepped {
				return nil
			}
			if err := safely(func() error { return dm.Finish(ctx, tx) }); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE schema_data_migrations SET done = ?, updated = ? WHERE name = ? AND version = ?`,
				migComplete, time.Now().Unix(), dm.Name, dm.Version)
			return err
		}})
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrClosed) {
				return err
			}
			m.fail(dm, err)
			return err
		}
	}
	m.setState(dm, func(st *MigrationState) { st.State, st.RowsLeft, st.ETA = MigDone, 0, 0 })
	m.markDone(dm)
	s.invalidateCounts()
	s.log.Info("store: data migration done", "migration", dm.key())
	return nil
}

func (m *migrator) markDone(dm DataMigration) {
	old := *m.doneSet.Load()
	set := make(map[string]bool, len(old)+1)
	for k, v := range old {
		set[k] = v
	}
	set[dm.key()] = true
	m.doneSet.Store(&set)
}

func (m *migrator) fail(dm DataMigration, err error) {
	s := m.s
	m.setState(dm, func(st *MigrationState) { st.State = MigFailed; st.LastError = err.Error() })
	s.log.Error("store: data migration step failed; retrying later", "migration", dm.key(), "err", err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		_, e := tx.ExecContext(ctx, `INSERT INTO schema_data_migrations(name, version, done, started, updated, last_error)
VALUES(?, ?, 0, ?, ?, ?) ON CONFLICT(name, version) DO UPDATE SET last_error = excluded.last_error, updated = excluded.updated`,
			dm.Name, dm.Version, now, now, err.Error())
		return e
	})
}

// kv keys of the migration framework.
const (
	kvDataMigDoneAt  = "store.datamig_done_at"   // Unix s when every data migration had completed
	kvPreMigBackup   = "store.premig_backup"     // path of the pre-migration backup written by the store
	kvPreMigDecision = "store.premig_backup_how" // "written", "skipped", "never"
	kvVacuumHinted   = "store.vacuum_hint"       // the auto_vacuum=0 hint was logged
)

// allDone runs once all migrations are complete: record the time, PRAGMA
// optimize and start the vacuum tail when this process migrated something.
func (m *migrator) allDone(ctx context.Context, migrated bool) {
	s := m.s
	m.complete.Store(true)
	if _, ok, _ := kvInt(ctx, s.DB(), kvDataMigDoneAt); !ok {
		_ = s.KVSet(ctx, kvDataMigDoneAt, strconv.FormatInt(time.Now().Unix(), 10))
		migrated = true
	}
	if !migrated {
		return
	}
	if err := s.optimizeLimited(ctx); err != nil && ctx.Err() == nil {
		s.log.Debug("store: optimize after data migration", "err", err)
	}
	var av, free int64
	db := s.DB()
	_ = db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&av)
	_ = db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free)
	switch {
	case free == 0:
	case av == 2: // INCREMENTAL
		s.vacuumTail.Store(true)
	default:
		hint := "this database file was created without incremental auto-vacuum: its " +
			strconv.FormatInt(free, 10) + " free pages are reused for new rows; run `airview db vacuum` " +
			"(needs free disk space of about the file size) to shrink the file"
		m.mu.Lock()
		m.hint = hint
		m.mu.Unlock()
		if _, ok, _ := s.KVGet(ctx, kvVacuumHinted); !ok {
			s.log.Info("store: " + hint)
			_ = s.KVSet(ctx, kvVacuumHinted, "1")
		}
	}
}

// HasDataMigrations reports whether this build registers data migrations
// for databases of kind k (stations.db has only DDL migrations, which run
// at Open).
func HasDataMigrations(k Kind) bool {
	dataMigs.mu.Lock()
	defer dataMigs.mu.Unlock()
	for _, m := range dataMigs.list {
		if m.kinds()&k != 0 {
			return true
		}
	}
	return false
}
