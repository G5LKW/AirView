package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sqlite "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// FileName is the database file inside the data directory.
const FileName = "airview.db"

// ErrClosed is returned by operations on a Store after Close.
var ErrClosed = errors.New("store: closed")

// Options tunes a Store; the zero value selects the defaults, which are
// sized for a Raspberry Pi 4 with an SD card.
type Options struct {
	// FlushInterval is the longest time queued (asynchronous) writes wait
	// before they are committed. Default 2 s.
	FlushInterval time.Duration
	// FlushRows triggers an early flush once this many rows are queued.
	// Default 2,000 rows per second of FlushInterval, at least 4,000 and at
	// most 60,000 (standard 20,000, low-wear 60,000, server 10,000;
	// stations.db: 20000).
	FlushRows int
	// MaxQueueRows bounds the queue. When it is exceeded (the disk cannot
	// keep up) the oldest low-value rows are dropped first; see Stats.
	// Default 200000 (stations.db: 2000000).
	MaxQueueRows int
	// ReadConns is the size of the read-only connection pool behind DB().
	// Default 4.
	ReadConns int
	// Logger receives the store's log output. Default slog.Default().
	Logger *slog.Logger
	// Kind selects the database: KindMain (zero value: airview.db) or
	// KindStations (stations.db, see OpenStations).
	Kind Kind
	// Profile is the platform storage profile (ResolveProfile). The zero
	// value resolves "auto" for the data directory at Open. Explicit
	// FlushInterval / ReadConns above take precedence over its values.
	Profile Profile
	// MigrationDelay is how long after Open the background data migrations
	// start (default 15 s, so the station's own start-up comes first). A
	// negative value disables the background run (MigrateNow still works).
	MigrationDelay time.Duration

	// testDataMigs replaces the registered data migrations (tests only).
	testDataMigs []DataMigration
	// testDiskFree replaces the free-space query (tests only).
	testDiskFree func(dir string) (uint64, error)
	// testMigHook is called around every data-migration commit (tests).
	testMigHook func(name string, step int, phase string) error
	// testRecheck replaces the 10-minute disk / backup re-check (tests).
	testRecheck time.Duration
}

// maxDefaultFlushRows caps the default FlushRows (a long FlushInterval
// must not let the queue grow without bound before the first flush).
const maxDefaultFlushRows = 60000

func (o Options) withDefaults() Options {
	if o.Kind != KindStations {
		o.Kind = KindMain
	}
	if o.Kind == KindStations {
		// stations.db takes the statistics of every station: the lanes'
		// 5-minute histogram / heat flushes of 1,000 stations are bursts of
		// millions of rows (DESIGN §20.2), so the queue is larger.
		if o.FlushRows <= 0 {
			o.FlushRows = 20000
		}
		if o.MaxQueueRows <= 0 {
			o.MaxQueueRows = 2_000_000
		}
	}
	o.Profile = o.Profile.fill(hostPlatform())
	if o.FlushInterval <= 0 {
		o.FlushInterval = o.Profile.FlushInterval
	}
	if o.MigrationDelay == 0 {
		o.MigrationDelay = 15 * time.Second
	}
	if o.FlushRows <= 0 {
		// 1.0.0 flushed early at 4,000 queued rows per 2-s interval: keep
		// that rate (2,000 rows/s), so a longer profile interval is not cut
		// short by early flushes (a busy low-wear station queues ~6,000
		// rows per 30 s; at 4,000 every flush was an early one).
		o.FlushRows = min(max(4000, int(2000*o.FlushInterval.Seconds())), maxDefaultFlushRows)
	}
	if o.MaxQueueRows <= 0 {
		o.MaxQueueRows = 200000
	}
	if o.MaxQueueRows < o.FlushRows {
		o.MaxQueueRows = o.FlushRows
	}
	if o.ReadConns <= 0 {
		o.ReadConns = o.Profile.ReadConns
	}
	o.Profile.ReadConns = o.ReadConns
	o.Profile.FlushInterval = o.FlushInterval
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// handles is one generation of database connections (replaced by Reset).
type handles struct {
	wdb   *sql.DB              // exactly one connection: the writer
	rdb   *sql.DB              // read-only pool
	cdb   *sql.DB              // the checkpointer's dedicated connection
	stmts map[string]*sql.Stmt // statements prepared on wdb, used by the writer goroutine only
	kind  Kind                 // database kind (BLOB encodings)

	rmu    sync.Mutex
	rstmts map[string]*sql.Stmt // hot read statements prepared on rdb (ReadStmt)
}

func (h *handles) close() error {
	var errs []error
	for _, st := range h.stmts {
		if err := st.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	h.stmts = nil
	h.closeReadStmts()
	if h.rdb != nil {
		if err := h.rdb.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if h.cdb != nil {
		if err := h.cdb.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if h.wdb != nil {
		if err := h.wdb.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Store is the AirView SQLite database. All methods are safe for concurrent
// use. See the package documentation for the overall design.
type Store struct {
	dir  string
	path string
	kind Kind
	opts Options
	log  *slog.Logger

	life sync.Mutex // serialises Close and Reset
	cur  atomic.Pointer[handles]

	closed   atomic.Bool
	closedCh chan struct{} // closed by Close

	// Writer goroutine plumbing. reqCh and kick live as long as the Store;
	// stop/done belong to the current writer goroutine (guarded by life).
	reqCh  chan *request
	idleCh chan *idleJob // checkpoints and other idle-slot work
	kick   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	cdone  chan struct{} // the checkpointer goroutine of the current generation

	ckpt       ckptCounters
	vacuumTail atomic.Bool // post-migration incremental vacuum still running
	mig        *migrator

	// Asynchronous queue.
	qmu       sync.Mutex
	queue     []op
	qrows     int
	lastShed  time.Time // last overflow warning (guarded by qmu)
	nextFlush []op      // recycled backing array (guarded by qmu)

	nextFlight atomic.Int64

	// Counters behind Stats().
	nFlushes     atomic.Uint64
	nFlushedRows atomic.Uint64
	nFlushErrors atomic.Uint64
	nFailedOps   atomic.Uint64
	nCombined    atomic.Uint64    // synchronous writes committed together with the queue
	nDropped     [3]atomic.Uint64 // rows dropped per tier
	lastFlushNs  atomic.Int64     // UnixNano of the last successful flush
	lastFlushDur atomic.Int64     // its duration in ns
	lastErr      atomic.Pointer[string]

	// retryAt (UnixNano) holds back size-triggered flushes after a flush
	// failed because of the database itself (see flushQueue).
	retryAt      atomic.Int64
	lastFlushLog atomic.Int64 // UnixNano of the last "flush failed" log line

	counts countCache // TableCounts cache
}

// Open opens (creating it if necessary) the database airview.db in dir with
// default Options, applies all pending migrations and starts the writer.
func Open(dir string) (*Store, error) { return OpenWithOptions(dir, Options{}) }

// OpenWithOptions is Open with explicit tuning.
func OpenWithOptions(dir string, opts Options) (*Store, error) {
	if dir == "" {
		return nil, errors.New("store: empty data directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if opts.Profile.Name == "" {
		kind := KindMain
		if opts.Kind == KindStations {
			kind = KindStations
		}
		opts.Profile = ResolveProfile(ProfileAuto, kind, abs)
	}
	opts = opts.withDefaults()
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	s := &Store{
		dir:      abs,
		path:     filepath.Join(abs, opts.Kind.fileName()),
		kind:     opts.Kind,
		opts:     opts,
		log:      opts.Logger,
		closedCh: make(chan struct{}),
		reqCh:    make(chan *request),
		idleCh:   make(chan *idleJob),
		kick:     make(chan struct{}, 1),
	}
	s.counts.run = make(chan struct{}, 1)
	s.mig = newMigrator(s)

	// Become visible to RegisterMigration before migrating, so a migration
	// registered concurrently is applied either by migrate below or through
	// the writer as soon as it runs - never missed.
	registry.mu.Lock()
	registry.stores[s] = struct{}{}
	registry.mu.Unlock()

	h, err := s.openHandles()
	if err != nil {
		s.closed.Store(true)
		close(s.closedCh)
		registry.mu.Lock()
		delete(registry.stores, s)
		registry.mu.Unlock()
		return nil, err
	}
	s.cur.Store(h)
	s.startWriter(h)
	s.log.Info("store: opened", "db", s.kind.String(), "profile", opts.Profile.Name,
		"reason", opts.Profile.Reason, "flush", opts.FlushInterval)
	s.mig.start(opts.MigrationDelay)
	return s, nil
}

// openHandles opens the writer connection, migrates, opens the read pool and
// prepares the writer statements. A file that is not an SQLite database is
// moved aside (airview.db.corrupt-<n>) and replaced by a fresh one.
func (s *Store) openHandles() (*handles, error) {
	h, err := s.tryOpenHandles()
	if err == nil {
		return h, nil
	}
	if !isNotADB(err) {
		return nil, err
	}
	aside, rerr := s.quarantine()
	if rerr != nil {
		return nil, fmt.Errorf("%w (and the damaged file could not be moved aside: %v)", err, rerr)
	}
	s.log.Error("store: database file is not a valid SQLite database; starting a new one",
		"moved_to", aside, "err", err)
	return s.tryOpenHandles()
}

func (s *Store) tryOpenHandles() (*handles, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := s.opts.Profile

	wdb, err := sql.Open("sqlite", writerDSN(s.path, p))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", s.path, err)
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(0)
	wdb.SetConnMaxIdleTime(0)
	h := &handles{wdb: wdb, stmts: map[string]*sql.Stmt{}, kind: s.kind}

	fail := func(err error) (*handles, error) {
		_ = h.close()
		return nil, err
	}
	if err := wdb.PingContext(ctx); err != nil {
		return fail(fmt.Errorf("store: open %s: %w", s.path, err))
	}
	if err := migrate(ctx, wdb, s.kind); err != nil {
		return fail(err)
	}

	rdb, err := sql.Open("sqlite", readerDSN(s.path, p))
	if err != nil {
		return fail(fmt.Errorf("store: open read pool: %w", err))
	}
	rdb.SetMaxOpenConns(s.opts.ReadConns)
	rdb.SetMaxIdleConns(s.opts.ReadConns)
	rdb.SetConnMaxLifetime(0)
	// Idle readers close after a while on small hosts, which returns their
	// page cache (Profile.ReaderIdleClose, 0 = never).
	rdb.SetConnMaxIdleTime(p.ReaderIdleClose)
	h.rdb = rdb
	if err := rdb.PingContext(ctx); err != nil {
		return fail(fmt.Errorf("store: open read pool: %w", err))
	}
	if h.cdb, err = openCheckpointConn(s.path, p); err != nil {
		return fail(fmt.Errorf("store: open checkpoint connection: %w", err))
	}

	for _, q := range s.kind.prepared() {
		st, err := wdb.PrepareContext(ctx, q)
		if err != nil {
			return fail(fmt.Errorf("store: preparing %q: %w", firstLine(q), err))
		}
		h.stmts[q] = st
	}

	if s.kind != KindMain {
		return h, nil // no flights in stations.db
	}
	// Flight IDs are allocated in memory (NextFlightID); continue after the
	// highest ID that ever reached the database (flights, or any stored
	// trail point: legacy positions, trail tail or chunks).
	var maxFlight sql.NullInt64
	if err := wdb.QueryRowContext(ctx, `SELECT MAX(id) FROM flights`).Scan(&maxFlight); err != nil {
		return fail(fmt.Errorf("store: %w", err))
	}
	maxPos, err := maxStoredFlightID(ctx, wdb)
	if err != nil {
		return fail(fmt.Errorf("store: %w", err))
	}
	s.nextFlight.Store(max(maxFlight.Int64, maxPos, 0))
	return h, nil
}

// Connection tuning (docs/perf/DB-EFFICIENCY-DESIGN.md §3.1, §3.18, §4.1);
// the sizes come from the store's Profile:
//
//	journal_mode=WAL, synchronous=NORMAL  no fsync per commit; durable across
//	                                      application crashes, may lose the last
//	                                      transactions on power loss, never corrupts
//	busy_timeout                          wait instead of failing with SQLITE_BUSY
//	auto_vacuum=INCREMENTAL               pruned space can be returned without VACUUM
//	cache_size                            writer / per reader (Profile)
//	mmap_size                             Profile (none on 32-bit hosts)
//	wal_autocheckpoint                    hard cap only; the checkpointer runs
//	                                      PASSIVE checkpoints outside the commits
//	journal_size_limit                    Profile (32 MiB stations, 128 MiB server)
//	analysis_limit=400                    bounded PRAGMA optimize
//	temp_store=MEMORY                     no temp files on the SD card
//
// mmap_size stays modest on SD cards (64 MiB on a Pi): an I/O error through
// a mapping faults the process instead of returning an error
// (https://www.sqlite.org/mmap.html).
//
// The DSN keys are those of modernc.org/sqlite v1.62 (see the doc comment of
// its Driver.Open): the shorthand keys are validated and applied in a fixed
// order - _busy_timeout and _auto_vacuum first, then the _pragma list, then
// _journal_mode / _synchronous / _foreign_keys, _query_only last - on every
// connection the pool opens. _txlock=immediate makes the writer's BeginTx a
// BEGIN IMMEDIATE.
const busyTimeoutMs = "10000"

func baseValues(busy string) url.Values {
	q := url.Values{}
	q.Set("_busy_timeout", busy)
	q.Set("_foreign_keys", "on")
	q.Add("_pragma", "temp_store(MEMORY)")
	return q
}

func writerDSN(path string, p Profile) string {
	q := baseValues(busyTimeoutMs)
	q.Set("_auto_vacuum", "INCREMENTAL") // only takes effect on a brand-new database
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "cache_size(-"+strconv.Itoa(p.WriterCacheKiB)+")")
	q.Add("_pragma", "mmap_size("+strconv.FormatInt(p.MmapBytes, 10)+")")
	q.Add("_pragma", "wal_autocheckpoint("+strconv.Itoa(p.WALHardCapPages)+")")
	q.Add("_pragma", "journal_size_limit("+strconv.FormatInt(p.JournalSizeLimit, 10)+")")
	q.Add("_pragma", "analysis_limit(400)")
	return dsn(path, q)
}

func readerDSN(path string, p Profile) string {
	q := baseValues(busyTimeoutMs)
	q.Set("_query_only", "1") // readers can never write, whatever SQL they are given
	q.Add("_pragma", "cache_size(-"+strconv.Itoa(p.ReaderCacheKiB)+")")
	q.Add("_pragma", "mmap_size("+strconv.FormatInt(p.MmapBytes, 10)+")")
	return dsn(path, q)
}

// dsn builds a modernc.org/sqlite data source name. A plain path keeps its
// query string for the driver only; a path containing '?' has to go through
// the URI form, where it can be escaped.
func dsn(path string, q url.Values) string {
	if !strings.ContainsRune(path, '?') {
		return path + "?" + q.Encode()
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String() + "?" + q.Encode()
}
func isNotADB(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqliteNotADB
}

// Primary SQLite result codes used for error classification.
const (
	sqliteBusy     = 5
	sqliteLocked   = 6
	sqliteNoMem    = 7
	sqliteReadOnly = 8
	sqliteIOErr    = 10
	sqliteCorrupt  = 11
	sqliteFull     = 13
	sqliteCantOpen = 14
	sqliteProtocol = 15
	sqliteNotADB   = 26
)

// isEnvironmentError reports whether err is a problem of the database as a
// whole (disk full, I/O error, locked, ...) rather than of one statement's
// data. Such errors are retried later instead of blaming a single operation.
func isEnvironmentError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return true
	}
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case sqliteBusy, sqliteLocked, sqliteNoMem, sqliteReadOnly, sqliteIOErr, sqliteCorrupt,
		sqliteFull, sqliteCantOpen, sqliteProtocol, sqliteNotADB:
		return true
	}
	return false
}

// dbFiles returns the database file and its WAL companions.
func (s *Store) dbFiles() []string {
	return []string{s.path, s.path + "-wal", s.path + "-shm"}
}

// quarantine renames a damaged database (and companions) out of the way.
func (s *Store) quarantine() (string, error) {
	for n := 1; n <= 10000; n++ {
		aside := s.path + ".corrupt-" + strconv.Itoa(n)
		if _, err := os.Lstat(aside); err == nil {
			continue
		}
		if err := os.Rename(s.path, aside); err != nil {
			return "", err
		}
		_ = os.Remove(s.path + "-wal")
		_ = os.Remove(s.path + "-shm")
		return aside, nil
	}
	return "", errors.New("too many quarantined database files")
}

// DB returns the read-only connection pool for SELECT queries by other
// packages (the schema is documented in the package comment). Connections
// are opened with query_only=1, so writes through it fail; use Write or a
// Batch instead.
//
// Rows queued asynchronously become visible after the next flush (at most
// Options.FlushInterval later, or after Flush). Do not cache the returned
// pointer across a Reset: Reset closes the pool and opens a new one, so call
// DB() where the query is made.
func (s *Store) DB() *sql.DB { return s.cur.Load().rdb }

// readStmt returns query prepared once on the read pool (design §3.16:
// the hot point reads - trail chunks, aircraft and flight by key, kv - skip
// SQLite's parse and plan on every call; database/sql prepares it lazily
// on each pool connection). The statement belongs to the current handles
// generation and is closed by Reset / Close; callers must not close it.
func (s *Store) readStmt(ctx context.Context, query string) (*sql.Stmt, error) {
	h := s.cur.Load()
	h.rmu.Lock()
	defer h.rmu.Unlock()
	if st, ok := h.rstmts[query]; ok {
		return st, nil
	}
	if h.rdb == nil {
		return nil, ErrClosed
	}
	st, err := h.rdb.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	if h.rstmts == nil {
		h.rstmts = map[string]*sql.Stmt{}
	}
	h.rstmts[query] = st
	return st, nil
}

func (h *handles) closeReadStmts() {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	for _, st := range h.rstmts {
		_ = st.Close()
	}
	h.rstmts = nil
}

// Dir returns the data directory the store was opened in.
func (s *Store) Dir() string { return s.dir }

// Path returns the full path of the database file.
func (s *Store) Path() string { return s.path }

// NextFlightID allocates a new flights.id. IDs are unique, increase
// monotonically and continue after the highest ID in the database, so the
// tracker can use the ID at once (in the live DTO, for trail positions) and
// write the flight row later through a Batch.
func (s *Store) NextFlightID() int64 { return s.nextFlight.Add(1) }

// Close flushes the queue, checkpoints the WAL and closes the database.
// Further writes fail with ErrClosed; queued rows submitted after Close are
// discarded. Close is idempotent.
func (s *Store) Close() error {
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed.Load() {
		return nil
	}

	// Stop the background data migration between two steps (every step is
	// its own transaction; the next start resumes after the last one).
	s.mig.stop()
	// Flush through the writer while it is still running, then stop it.
	flushErr := s.Flush()
	s.closed.Store(true)
	close(s.closedCh)
	s.stopWriter()

	registry.mu.Lock()
	delete(registry.stores, s)
	registry.mu.Unlock()

	h := s.cur.Load()
	// Rows enqueued between the Flush above and closed=true.
	if err := s.flushQueue(h); err != nil && flushErr == nil {
		flushErr = err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := h.wdb.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		s.log.Debug("store: PRAGMA optimize failed", "err", err)
	}
	// Close the readers first so the checkpoint can truncate the WAL.
	stmts, rdb := h.stmts, h.rdb
	for _, st := range stmts {
		_ = st.Close()
	}
	h.stmts = nil
	h.closeReadStmts()
	rerr := rdb.Close()
	if h.cdb != nil {
		_ = h.cdb.Close()
		h.cdb = nil
	}
	if _, err := h.wdb.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		s.log.Warn("store: final checkpoint failed", "err", err)
	}
	werr := h.wdb.Close()
	return errors.Join(flushErr, rerr, werr)
}

// Reset wipes the database for a factory reset: the writer is stopped,
// queued rows are discarded, airview.db, -wal and -shm are deleted and a
// fresh, fully migrated database is opened. The Store stays usable (the same
// *Store), but DB() returns a new pool afterwards and flight IDs restart at 1.
//
// Callers should stop the components that read or write the store first.
// Should the files be impossible to delete (on Windows: a handle that is
// still open somewhere), Reset falls back to dropping every table inside the
// existing file, which has the same visible result.
func (s *Store) Reset() error {
	if s.kind == KindStations {
		return ErrResetUnsupported
	}
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}

	s.mig.stop()
	s.stopWriter()
	s.qmu.Lock()
	s.queue, s.qrows = nil, 0
	s.qmu.Unlock()
	s.retryAt.Store(0)
	s.dropCounts()

	old := s.cur.Load()
	if err := old.close(); err != nil {
		s.log.Warn("store: closing database for reset", "err", err)
	}

	removeErr := s.removeFiles()
	h, err := s.openHandles()
	if err != nil {
		// Leave the store in a defined (closed) state.
		s.closed.Store(true)
		close(s.closedCh)
		registry.mu.Lock()
		delete(registry.stores, s)
		registry.mu.Unlock()
		return fmt.Errorf("store: reset: reopening: %w", errors.Join(err, removeErr))
	}
	if removeErr != nil {
		s.log.Warn("store: database files could not be deleted; wiping tables instead", "err", removeErr)
		if err := s.wipeInPlace(h); err != nil {
			s.cur.Store(h)
			s.startWriter(h)
			return fmt.Errorf("store: reset: %w", errors.Join(removeErr, err))
		}
	}
	s.cur.Store(h)
	s.startWriter(h)
	s.mig.start(s.opts.MigrationDelay)
	return nil
}

// removeFiles deletes the database files, retrying for a few seconds
// because Windows refuses to delete a file while any handle is open.
func (s *Store) removeFiles() error {
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		err = nil
		for _, f := range s.dbFiles() {
			if e := os.Remove(f); e != nil && !errors.Is(e, os.ErrNotExist) {
				err = e
			}
		}
		if err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// wipeInPlace empties an open database by dropping every user object and
// re-running the migrations. Used only when the files cannot be deleted.
func (s *Store) wipeInPlace(h *handles) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for _, st := range h.stmts {
		_ = st.Close()
	}
	h.stmts = map[string]*sql.Stmt{}

	rows, err := h.wdb.QueryContext(ctx,
		`SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' AND type IN ('table','view','trigger')`)
	if err != nil {
		return err
	}
	type obj struct{ typ, name string }
	var objs []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.typ, &o.name); err != nil {
			rows.Close()
			return err
		}
		objs = append(objs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := h.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, kind := range []string{"trigger", "view", "table"} {
		for _, o := range objs {
			if o.typ != kind {
				continue
			}
			stmt := "DROP " + strings.ToUpper(kind) + " IF EXISTS " + quoteIdent(o.name)
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := h.wdb.ExecContext(ctx, `VACUUM`); err != nil {
		s.log.Debug("store: VACUUM after in-place wipe failed", "err", err)
	}
	if err := migrate(ctx, h.wdb, s.kind); err != nil {
		return err
	}
	for _, q := range s.kind.prepared() {
		st, err := h.wdb.PrepareContext(ctx, q)
		if err != nil {
			return err
		}
		h.stmts[q] = st
	}
	s.nextFlight.Store(0)
	return nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func firstLine(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexByte(q, '\n'); i >= 0 {
		q = q[:i]
	}
	if len(q) > 80 {
		q = q[:80]
	}
	return q
}

func (s *Store) setLastError(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	s.lastErr.Store(&msg)
}
