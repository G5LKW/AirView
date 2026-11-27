package registry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"airview/internal/model"
	"airview/internal/store"
)

// FileName is the registry database file inside the data directory.
const FileName = "stations.db"

// Policy is the part of the aggregator configuration the registry applies.
// It can be changed at run time with SetPolicy (config reloads).
type Policy struct {
	// Admission is model.AdmissionOpen, AdmissionApprove or AdmissionInvite.
	Admission string
	// DefaultPin is the pin precision of new stations (model.Pin*).
	DefaultPin string
	// MaxStations caps approved stations; pending stations are capped at
	// the same number again (flood guard).
	MaxStations int
	// Retention is the retention schedule (DESIGN §8.5).
	Retention model.AggRetention
	// NoTimingQuarantine admits new stations straight into MLAT. By
	// default (false) a new station starts in timing quarantine
	// (FlagTimingQuarantine) and joins MLAT once internal/registry/trust
	// has proven its timing, so open admission cannot poison the MLAT
	// quality (H-mlatc). Stations that existed before are not affected.
	NoTimingQuarantine bool
	// Positions rate-limits and holds position changes that feeders
	// announce themselves (RECEIVER records, mlat-client handshakes;
	// finding PRIV-1). Zero fields take the defaults (position.go).
	Positions PositionPolicy
}

// PolicyFromConfig extracts the registry policy from the aggregator block.
func PolicyFromConfig(a model.Aggregator) Policy {
	ps := a.Positions
	hold := time.Duration(ps.HoldMaxDays * 24 * float64(time.Hour))
	if ps.HoldMaxDays < 0 {
		hold = -1 // never
	}
	return Policy{Admission: a.Admission, DefaultPin: a.DefaultPin,
		MaxStations: a.Limits.MaxStations, Retention: a.Retention,
		NoTimingQuarantine: a.MLAT.Quarantine.Disabled,
		Positions: PositionPolicy{
			JitterM:      ps.JitterM,
			MinInterval:  time.Duration(ps.MinIntervalHours * float64(time.Hour)),
			HoldBeyondM:  ps.HoldBeyondM,
			HoldMax:      hold,
			SurveyMatchM: ps.SurveyMatchM,
		}}
}

// normalized fills zero or invalid fields with the defaults of
// model.DefaultAggregator.
func (p Policy) normalized() Policy {
	d := PolicyFromConfig(model.DefaultAggregator())
	switch p.Admission {
	case model.AdmissionOpen, model.AdmissionApprove, model.AdmissionInvite:
	default:
		p.Admission = d.Admission
	}
	if !model.ValidPin(p.DefaultPin) {
		p.DefaultPin = d.DefaultPin
	}
	if p.MaxStations <= 0 {
		p.MaxStations = d.MaxStations
	}
	r, dr := &p.Retention, d.Retention
	pos := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	pos(&r.ConnLogDays, dr.ConnLogDays)
	pos(&r.MinuteDays, dr.MinuteDays)
	pos(&r.HourMonths, dr.HourMonths)
	pos(&r.OfflinePurgeDays, dr.OfflinePurgeDays)
	pos(&r.AuditMonths, dr.AuditMonths)
	pos(&r.EstimatesPerMethod, dr.EstimatesPerMethod)
	return p
}

// ExportFunc adds extra files to an owner's data export (for example the
// statistics of the owner's stations kept by internal/store). add writes
// one JSON file into the archive.
type ExportFunc func(ctx context.Context, ownerID int64, stations []Station, add func(name string, v any) error) error

// Options configures Open. Zero values take defaults.
type Options struct {
	Policy Policy
	// ClaimTTL is the validity of a claim code (default 24 h, DESIGN §7).
	ClaimTTL time.Duration
	// MaxOpenClaims bounds the unused claim codes per station (default 5);
	// minting another one drops the oldest.
	MaxOpenClaims int
	// TokenTTL / TokenMaxUses are the enrol-token defaults (7 days, 1 use).
	TokenTTL     time.Duration
	TokenMaxUses int
	// MaxSessionsPerAccount bounds stored sessions per account (default 32);
	// the one that expires first is evicted.
	MaxSessionsPerAccount int
	// SeenFlushEvery is how often Run writes last-seen times (default 30 s).
	SeenFlushEvery time.Duration
	// RetentionEvery is how often Run applies the retention schedule
	// (default 1 h). Deletions queued in between are processed every
	// DeletionEvery (default 10 s).
	RetentionEvery time.Duration
	DeletionEvery  time.Duration
	// DeleteBatch is the number of rows one DELETE statement of the deletion
	// and retention jobs removes before it yields the writer (default 5000).
	DeleteBatch int
	// ExportExtra adds files to owner exports (optional).
	ExportExtra ExportFunc
	// DeleteExtra removes a station's rows outside stations.db (for example
	// flight_station and mlat_minute in airview.db) when its deletion job
	// runs. It is called before the station's registry rows are removed; an
	// error leaves the deletion queued for the next pass. Optional.
	DeleteExtra func(ctx context.Context, idx model.StationIdx, sid string) error
	// BeforeDelete runs first in a station's deletion job: it stops every
	// in-memory producer of the station's statistics (its lane) and waits
	// until their queued rows are written, so nothing re-creates the rows
	// the job deletes (C4). An error leaves the deletion queued. Optional.
	BeforeDelete func(ctx context.Context, idx model.StationIdx) error
	// StoreMaintained skips the statistics tables of DefaultRetentionRules in
	// RunRetention: internal/store's tiered Maintain owns their retention
	// (it rolls hours into days before deleting them). Set it when the
	// engine runs store.Maintain on stations.db.
	StoreMaintained bool
	// Logger receives job summaries (default slog.Default()).
	Logger *slog.Logger
	// Now and Rand replace the clock and the randomness source (tests).
	Now  func() time.Time
	Rand io.Reader
	// ReadConns is the size of the read-only connection pool (default 4).
	ReadConns int
	// WALHardCapPages is wal_autocheckpoint of the registry's writer
	// connection while internal/store has the same file open (default
	// 131072 pages = 512 MiB: the hard cap of the store's "server"
	// profile). The store's checkpointer then checkpoints stations.db off
	// the commit path; the registry has no checkpoint loop of its own and
	// must not run SQLite's automatic checkpoint (every 1,000 pages) inside
	// its commits - on a WAL the checkpointer keeps at up to 64 MiB that
	// would be nearly every registry commit, each stalled behind fsyncs. A
	// registry opened without the store (tools, tests) keeps SQLite's
	// 1,000-page checkpoint, which bounds its WAL. Checked before every
	// write (store.CheckpointServes; a pragma on the open connection).
	WALHardCapPages int
	// JournalSizeLimit truncates the WAL file after a checkpoint reset
	// (default 128 MiB, the server profile's value).
	JournalSizeLimit int64
	// WriterCacheKiB / ReaderCacheKiB are the page caches of the writer and
	// of each reader connection (defaults 8 MiB / 4 MiB: the registry
	// tables are small; the statistics tables of stations.db are read
	// through the store's own pool).
	WriterCacheKiB, ReaderCacheKiB int
}

func (o Options) withDefaults() Options {
	if o.ClaimTTL <= 0 {
		o.ClaimTTL = 24 * time.Hour
	}
	if o.MaxOpenClaims <= 0 {
		o.MaxOpenClaims = 5
	}
	if o.TokenTTL <= 0 {
		o.TokenTTL = 7 * 24 * time.Hour
	}
	if o.TokenMaxUses <= 0 {
		o.TokenMaxUses = 1
	}
	if o.MaxSessionsPerAccount <= 0 {
		o.MaxSessionsPerAccount = 32
	}
	if o.SeenFlushEvery <= 0 {
		o.SeenFlushEvery = 30 * time.Second
	}
	if o.RetentionEvery <= 0 {
		o.RetentionEvery = time.Hour
	}
	if o.DeletionEvery <= 0 {
		o.DeletionEvery = 10 * time.Second
	}
	if o.DeleteBatch <= 0 {
		o.DeleteBatch = 5000
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Reader
	}
	if o.ReadConns <= 0 {
		o.ReadConns = 4
	}
	if o.WALHardCapPages <= 0 {
		o.WALHardCapPages = 131072
	}
	if o.JournalSizeLimit <= 0 {
		o.JournalSizeLimit = 128 << 20
	}
	if o.WriterCacheKiB <= 0 {
		o.WriterCacheKiB = 8 << 10
	}
	if o.ReaderCacheKiB <= 0 {
		o.ReaderCacheKiB = 4 << 10
	}
	return o
}

// Registry is the station registry. Create it with Open; it is safe for
// concurrent use.
type Registry struct {
	path string
	opts Options
	log  *slog.Logger
	now  func() time.Time
	rnd  io.Reader

	wdb *sql.DB // one connection, BEGIN IMMEDIATE
	rdb *sql.DB // read-only pool

	policy atomic.Pointer[Policy]
	keys   keySet

	// wmu serialises write transactions with the in-memory mirror updates,
	// so the mirror always reflects the committed order. Admissions take it
	// with priority over bulk writes (writelock.go; finding SCALE-09).
	wmu prioLock

	cache stationCache

	seenMu sync.Mutex
	seen   map[model.StationIdx]int64 // pending last-seen (Unix s)

	saltMu  sync.Mutex
	saltDay int64
	salt    []byte

	claimIP, claimAcct claimLimiter

	posMu sync.Mutex // FeederPosition decisions (position.go)

	// autoCkpt is the wal_autocheckpoint the writer connection has (0 =
	// the DSN's, SQLite's default); changed under wmu (alignCheckpoint).
	autoCkpt int

	bandMu   sync.Mutex
	bandRefs map[string]bandRef // frozen peer-band references (position.go)

	closed atomic.Bool
}

// ErrClosed is returned after Close.
var ErrClosed = errors.New("registry: closed")

// OpenDir opens <dir>/stations.db (see Open).
func OpenDir(dir string, opts Options) (*Registry, error) {
	if dir == "" {
		return nil, errors.New("registry: empty data directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	return Open(filepath.Join(dir, FileName), opts)
}

// Open opens (creating it when necessary) the registry database at path,
// applies the migrations, loads or creates the server keys and loads the
// station table into memory. Stations whose deletion was queued before a
// restart are deleted by the next ProcessDeletions / Run.
func Open(path string, opts Options) (*Registry, error) {
	if strings.ContainsRune(path, '?') {
		return nil, errors.New("registry: database path must not contain '?'")
	}
	opts = opts.withDefaults()
	r := &Registry{
		path: path, opts: opts, log: opts.Logger, now: opts.Now, rnd: opts.Rand,
		seen: map[model.StationIdx]int64{},
	}
	r.claimIP = newClaimLimiter(5, 10*time.Minute, opts.Now)
	r.claimAcct = newClaimLimiter(20, 24*time.Hour, opts.Now)
	p := opts.Policy.normalized()
	r.policy.Store(&p)

	var err error
	if r.wdb, err = sql.Open("sqlite", writerDSN(path, opts)); err != nil {
		return nil, fmt.Errorf("registry: open: %w", err)
	}
	r.wdb.SetMaxOpenConns(1)
	r.wdb.SetMaxIdleConns(1)
	r.wdb.SetConnMaxLifetime(0)
	ctx := context.Background()
	if err := r.wdb.PingContext(ctx); err != nil {
		_ = r.wdb.Close()
		return nil, fmt.Errorf("registry: open %s: %w", path, err)
	}
	if err := migrate(ctx, r.wdb); err != nil {
		_ = r.wdb.Close()
		return nil, err
	}
	if r.rdb, err = sql.Open("sqlite", readerDSN(path, opts)); err != nil {
		_ = r.wdb.Close()
		return nil, fmt.Errorf("registry: open reader: %w", err)
	}
	r.rdb.SetMaxOpenConns(opts.ReadConns)
	r.rdb.SetMaxIdleConns(opts.ReadConns)
	if err := r.loadKeys(ctx); err != nil {
		r.closeDBs()
		return nil, err
	}
	if err := r.loadStations(ctx); err != nil {
		r.closeDBs()
		return nil, err
	}
	if err := r.closeDangling(ctx); err != nil {
		r.closeDBs()
		return nil, err
	}
	return r, nil
}

func (r *Registry) closeDBs() {
	if r.rdb != nil {
		_ = r.rdb.Close()
	}
	if r.wdb != nil {
		_ = r.wdb.Close()
	}
}

// Close flushes pending last-seen times and closes the database.
func (r *Registry) Close() error {
	if r.closed.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ferr := r.flushSeen(ctx)
	if r.closed.Swap(true) {
		return nil
	}
	r.wmu.lock(true) // wait for a write in progress
	r.closeDBs()
	r.wmu.unlock()
	return ferr
}

// Path returns the database file.
func (r *Registry) Path() string { return r.path }

// Policy returns the current policy.
func (r *Registry) Policy() Policy { return *r.policy.Load() }

// SetPolicy replaces the policy (after a configuration change). Invalid
// fields take their defaults.
func (r *Registry) SetPolicy(p Policy) {
	p = p.normalized()
	r.policy.Store(&p)
}

// DSNs: see internal/store for the rationale of each setting. The registry
// is small; the caches are modest. The WAL settings follow the store's
// "server" profile (Options.JournalSizeLimit here, Options.WALHardCapPages
// through alignCheckpoint).
func writerDSN(path string, o Options) string {
	q := url.Values{}
	q.Set("_busy_timeout", "10000")
	q.Set("_auto_vacuum", "INCREMENTAL") // only takes effect on a brand-new file
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "temp_store(MEMORY)")
	q.Add("_pragma", "cache_size(-"+strconv.Itoa(o.WriterCacheKiB)+")")
	q.Add("_pragma", "journal_size_limit("+strconv.FormatInt(o.JournalSizeLimit, 10)+")")
	return path + "?" + q.Encode()
}

func readerDSN(path string, o Options) string {
	q := url.Values{}
	q.Set("_busy_timeout", "10000")
	q.Set("_query_only", "1")
	q.Add("_pragma", "temp_store(MEMORY)")
	q.Add("_pragma", "cache_size(-"+strconv.Itoa(o.ReaderCacheKiB)+")")
	return path + "?" + q.Encode()
}

// write runs fn in one write transaction while holding wmu; after a
// successful commit it runs after (still under wmu) to update the in-memory
// mirror.
func (r *Registry) write(ctx context.Context, fn func(tx *sql.Tx) error, after func()) error {
	return r.writePrio(ctx, false, fn, after)
}

// writeHigh is write for admissions: it overtakes queued bulk writes
// (connection log, last-seen, retention), so a feeder's admission does not
// time out behind hundreds of reconnect writes (finding SCALE-09:
// "registry: begin: context deadline exceeded" at 1,000 feeders).
func (r *Registry) writeHigh(ctx context.Context, fn func(tx *sql.Tx) error, after func()) error {
	return r.writePrio(ctx, true, fn, after)
}

func (r *Registry) writePrio(ctx context.Context, high bool, fn func(tx *sql.Tx) error, after func()) error {
	if r.closed.Load() {
		return ErrClosed
	}
	if err := r.wmu.lockCtx(ctx, high); err != nil {
		return fmt.Errorf("registry: begin: %w", err)
	}
	defer r.wmu.unlock()
	if r.closed.Load() {
		return ErrClosed
	}
	r.alignCheckpoint(ctx)
	tx, err := r.wdb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registry: commit: %w", err)
	}
	if after != nil {
		after()
	}
	return nil
}

// sqliteAutoCheckpoint is SQLite's default wal_autocheckpoint (pages).
const sqliteAutoCheckpoint = 1000

// alignCheckpoint sets the writer connection's wal_autocheckpoint for the
// coming commit: the hard cap while internal/store has the file open (its
// checkpointer serves the file), SQLite's default otherwise. wmu is held.
func (r *Registry) alignCheckpoint(ctx context.Context) {
	want := sqliteAutoCheckpoint
	if store.CheckpointServes(r.path) {
		want = r.opts.WALHardCapPages
	}
	if want == r.autoCkpt || (r.autoCkpt == 0 && want == sqliteAutoCheckpoint) {
		return
	}
	if _, err := r.wdb.ExecContext(ctx, "PRAGMA wal_autocheckpoint = "+strconv.Itoa(want)); err != nil {
		r.log.Debug("registry: wal_autocheckpoint", "err", err)
		return
	}
	r.autoCkpt = want
}

// unixS returns t as Unix seconds (0 for the zero time).
func unixS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// fromS is the inverse of unixS.
func fromS(s int64) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0).UTC()
}

// unixNs returns t as Unix nanoseconds (0 for the zero time).
func unixNs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fromNs is the inverse of unixNs.
func fromNs(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
