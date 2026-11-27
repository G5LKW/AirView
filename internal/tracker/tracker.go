// Package tracker keeps the live aircraft table and is the write side of all
// AirView statistics.
//
// A Tracker implements model.FrameSink: feeds push raw Mode S frames
// (HandleFrame, decoded with internal/modes) and pre-decoded updates
// (HandleUpdate, SBS / aircraft.json). From them it maintains
//
//   - per-aircraft state with per-field freshness, CPR position decoding with
//     sanity filters, geometry relative to the receiver, in-memory trails;
//   - flight sessions, aircraft sightings and down-sampled positions, written
//     to internal/store in batches;
//   - statistics accumulators per antenna and combined (per-minute counters,
//     hourly range metrics, coverage, signal / elevation histograms, heat
//     cells), flushed on a timer - never per frame;
//   - all-time records, events and UI notices;
//   - per-feed GPS timestamp latency statistics.
//
// # Concurrency and time
//
// All state is owned by one mutex. HandleFrame holds it for well under a
// microsecond per frame and does not allocate in steady state; everything
// expensive (snapshots, flush batches, database look-ups) runs at 1 Hz or
// slower.
//
// In aggregator mode (SetNetwork on a fresh tracker) the tracker can split
// itself into N ICAO shards (Options.Shards, aggregator.limits.trackerShards,
// else AutoShards): each shard is a Tracker with its own mutex and data
// clock holding the aircraft whose address hashes to it, the Tracker the
// caller holds becomes a front that routes and merges (shard.go). Standalone
// trackers are never sharded. See WIRING.md "Scaling".
//
// The tracker runs on "data time": the newest RawFrame.Recv it has seen.
// Housekeeping (expiry, rates, minute / hour roll-over, flushes) is driven by
// that clock from inside HandleFrame, so a simulator replaying hours of
// traffic in seconds produces exactly the same rows as a live feed. Run only
// keeps the clock moving while no frames arrive and resolves aircraft
// database look-ups in the background.
package tracker

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/geo"
	lchain "airview/internal/latency"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/rfstat"
	"airview/internal/store"
	"airview/internal/trajectory"
)

// Tunables. Times are milliseconds of data time unless the name says
// otherwise.
const (
	dedupeWindowMs  = 100            // identical frame data within this window is one transmission
	confirmWindowMs = 60_000         // parity-address frames need a CRC-confirmed frame this recent
	cprPairMs       = 10_000         // max age of the other CPR frame for an airborne global decode
	cprSurfPairMs   = 30_000         // same for surface positions (sent every 5 s when parked)
	localRefMs      = 60_000         // max age of the last position as a local CPR reference
	speedRefMs      = 120_000        // max age of the last position for the speed plausibility check
	pendingMs       = 30_000         // max age of an unconfirmed fix waiting for a second one
	fieldTTLMs      = 60_000         // altitude, velocity, integrity, air/ground
	ehsTTLMs        = 30_000         // Comm-B derived values, target state
	identTTLMs      = 300_000        // squawk
	posTTLMs        = 60_000         // a position older than this is no longer shown
	heardTTLMs      = 30_000         // "antennas / feeds that heard it recently"
	ehsFreshMs      = 10_000         // max age of a value passed to the EHS hook
	hookIntervalMs  = 2_000          // min interval between EHS hook calls per aircraft
	flightGapMs     = 30 * 60 * 1000 // silence that ends a flight
	rowCheckMs      = 15_000         // how often dirty flight / aircraft rows, records and events are looked at
	rowSaveEveryS   = 60             // flight / aircraft row refresh while active
	trailMinGapMs   = 3_000          // regular trail point spacing
	trailTurnGapMs  = 1_000          // spacing when turning / changing level
	maxTrailPoints  = 1500
	maxFeeds        = 64
	maxSlots        = 1 + 16 // combined + config.MaxAntennas
	maxPendingPos   = 100_000
	rateTauS        = 5.0 // time constant of the smoothed rates

	// FarKm is the distance beyond which a position counts as "far" in
	// range_hour.far_count.
	FarKm = 300.0
)

// Notice is a message for the UI (SSE "notice" event).
type Notice struct {
	Level string `json:"level"` // "info" | "warn" | "alert"
	Text  string `json:"text"`
	T     int64  `json:"t"` // Unix ms
	Kind  string `json:"kind,omitempty"`
	ICAO  string `json:"icao,omitempty"`
	// Key and Value identify a new all-time record (kind "record"): the
	// record key (e.g. "max_dist_km") and its new value.
	Key   string   `json:"key,omitempty"`
	Value *float64 `json:"value,omitempty"`
}

// EHSEvent is handed to the meteorology hook when an aircraft with a known
// position delivers fresh Mode S EHS data (BDS 5,0 / 6,0) or a meteorological
// routine report (BDS 4,4). Unknown values, and values older than about
// 10 s, are NaN.
type EHSEvent struct {
	T                                                            time.Time
	ICAO                                                         uint32
	Lat, Lon, AltFt, GS, Track, TAS, IAS, Mach, MagHeading, Roll float64
	MRARWindSpd, MRARWindDir, MRARTemp                           float64
}

// EHSResult is the hook's answer; it is stored in the aircraft state
// (model.Aircraft WindDir / WindSpd / OAT).
type EHSResult struct {
	WindDir, WindSpd, OAT float64
	HasWind, HasOAT       bool
}

// Options configures New.
type Options struct {
	// Store receives all persistent rows. nil = keep everything in memory
	// only (tests).
	Store *store.Store
	// Clock supplies the NTP correction used for the feed latency
	// statistics. nil = no correction.
	Clock model.Clock
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Config is the initial configuration; nil = model.DefaultConfig().
	Config *model.Config
	// OnNotice receives UI notices (emergencies, records, catches). It is
	// called without tracker locks held, possibly from several goroutines.
	OnNotice func(Notice)
	// NoErrorCorrection disables the single-bit repair of DF17/18 frames
	// (which is only ever applied to aircraft that are already tracked).
	NoErrorCorrection bool
	// MaxRangeKm rejects positions farther than this from a located
	// receiver. Default 800.
	MaxRangeKm float64
	// MaxAircraft bounds the aircraft table (live + dormant). Default 20000.
	MaxAircraft int
	// Latency receives every accepted frame for the latency-chain analysis
	// (internal/latency). nil = off; see also SetLatency.
	Latency *lchain.Analyzer
	// Shards is the number of ICAO shards the tracker splits into when
	// SetNetwork switches it to aggregator mode (shard.go): 0 = take
	// Config.Aggregator.Limits.TrackerShards (whose 0 = AutoShards()),
	// 1 = never shard, n > 1 = n shards (at most MaxShards). Standalone
	// trackers are never sharded.
	Shards int

	// Persistence cadence (DB-EFFICIENCY-DESIGN §3.3): each stream is
	// written on its own clock (data time); 0 selects the default of the
	// "standard" storage profile. The minute rows are written every minute
	// and dirty flight rows once a minute whatever these say; a UTC day roll
	// writes the old day's increments first, and Flush writes everything.
	// What is not written yet is served by PendingIncs (statistics) and
	// FlushAircraftRows (aircraft rows, written on demand for a reader).
	//
	// TrailFlush: trail points -> trail tail (default 60 s).
	TrailFlush time.Duration
	// IncFlush: coverage / signal / elevation day increments (default 60 s;
	// low-wear 300 s).
	IncFlush time.Duration
	// HeatFlush: increments of the all-time position grid (default 300 s;
	// low-wear 900 s).
	HeatFlush time.Duration
	// RangePartial: partial range_hour rows of the open hour (default 15 min;
	// low-wear 30 min).
	RangePartial time.Duration
	// AircraftRefresh: refresh of the aircraft-table rows of the active
	// airframes (default 300 s; low-wear 900 s; at least 60 s). A new
	// airframe's row is still written at the next 15-s row check, and a
	// reader of aircraft rows calls FlushAircraftRows first, which writes
	// rows older than 60 s (1.0.0's freshness) before it answers.
	AircraftRefresh time.Duration
}

// Default persistence cadence (profile "standard").
const (
	DefaultTrailFlush   = 60 * time.Second
	DefaultIncFlush     = 60 * time.Second
	DefaultHeatFlush    = 300 * time.Second
	DefaultRangePartial = 15 * time.Minute
	// DefaultAircraftRefresh: the aircraft rows of the active airframes sit
	// on random pages of the all-time aircraft table (one 4 KiB page write
	// per active airframe and refresh); a 60-s refresh was half of all WAL
	// bytes of a busy station.
	DefaultAircraftRefresh = 300 * time.Second
)

// cadence is the resolved persistence cadence in milliseconds.
type cadence struct {
	trailMs, incMs, heatMs, rangeMs, acSec int64
}

func (o Options) cadence() cadence {
	ms := func(d, def time.Duration) int64 {
		if d <= 0 {
			d = def
		}
		return max(d.Milliseconds(), 1000)
	}
	return cadence{
		trailMs: ms(o.TrailFlush, DefaultTrailFlush),
		incMs:   ms(o.IncFlush, DefaultIncFlush),
		heatMs:  ms(o.HeatFlush, DefaultHeatFlush),
		rangeMs: ms(o.RangePartial, DefaultRangePartial),
		acSec:   max(ms(o.AircraftRefresh, DefaultAircraftRefresh)/1000, rowSaveEveryS),
	}
}

// Tracker is the live aircraft table. Create it with New.
type Tracker struct {
	mu    sync.Mutex
	st    *store.Store
	clock model.Clock
	log   *slog.Logger

	decOpts modes.Options
	msg     modes.Message // decode scratch, owned by mu

	// Configuration-derived state.
	cfg         model.Config
	station     *geo.Station
	groundElevM float64
	slots       []*antStats // 0 = combined, 1.. = cfg.Antennas
	chanSlot    [16]uint8   // RF channel -> slot (0 = none)
	feeds       map[string]*feedState
	feedList    []*feedState
	lastFeed    *feedState
	ttlMs       int64
	trailKeepMs int64
	trailEveryS int64
	maxRangeKm  float64
	maxAircraft int

	ac map[uint32]*aircraft
	dd [ddSets][2]ddEntry

	dayScr dayScratch // row buffers of the day-table flush (under mu)

	// Clock.
	nowMs      int64 // data time
	wallAtTick time.Time
	nextTickMs int64
	lastTickMs int64
	tickN      uint64
	started    time.Time
	startMs    int64
	// lateSince is the wall time (monotonic) since which every frame has
	// arrived more than clockStepMs behind data time; zero when frames are
	// in step. It detects a host clock that stepped backwards.
	lateSince time.Time

	// Statistics timing.
	minStart          int64 // Unix s
	minCovMs          int64 // data time (ms) from which the current minute is counted (first frame, reset, last final flush)
	hourStart         int64
	lastHourPartialMs int64 // partial range_hour rows (cad.rangeMs)
	lastRowMs         int64 // flight / aircraft rows, records, events (rowCheckMs)
	lastRefreshSec    int64 // last refresh of the active flight rows (rowSaveEveryS)
	lastAcRefreshSec  int64 // last refresh of the active aircraft rows (cad.acSec, FlushAircraftRows)
	lastTrailMs       int64 // trail points (cad.trailMs)
	lastIncMs         int64 // coverage / signal / elevation increments (cad.incMs)
	lastHeatMs        int64 // heat increments (cad.heatMs)
	lastIncSec        int64 // data second of the newest credited position
	incDay            int   // UTC day the accumulated day increments belong to
	incDayOK          bool  // incDay is set
	cad               cadence

	c       counters
	prev    counters // values at the previous tick (for rates)
	msgRate float64
	posRate float64
	msgEW   ewRate
	posEW   ewRate
	live    int // aircraft shown at the last tick

	snap     *model.LiveSnapshot
	snapTick uint64

	// Rows waiting for the next flush.
	pendPos     []store.Position
	pendFlights []store.Flight
	pendSeen    []store.AircraftSeen // first rows of new airframes: written at the next row check
	heldSeen    []store.AircraftSeen // refreshed rows: written at the next aircraft refresh
	pendEvents  []store.Event

	recs    *recTable // shared by the shards of a sharded tracker
	notices []Notice

	onNotice func(Notice)

	hookMu      sync.Mutex
	hook        func(EHSEvent) EHSResult
	hookPending bool
	hookEv      EHSEvent
	hookKey     uint32

	acdb          map[uint32]acdbCached
	acdbOrder     []uint32
	acdbQ         []uint32
	localFlightID int64

	// New-airframe detection (see resolveRefdata): afQ holds the aircraft
	// instances created since the last check; afFired the addresses already
	// reported this session (bounded FIFO), so the hook fires exactly once.
	afQ       []afCheck
	afFired   map[uint32]struct{}
	afOrder   []uint32
	afChecked int64 // statistics for tests / debugging

	// Extensions (see extend.go). Guarded by mu.
	tj      *trajectory.Manager // tracking filter (nil = off)
	coastMs int64               // longest predicted position shown after the last real one
	rf      *rfstat.Stats       // RF statistics (nil = off)
	onNewAF func(icao uint32)   // first sighting of an airframe ever (called without locks)
	enrich  func(s *model.LiveSnapshot)
	snapMu  sync.Mutex // serialises snapshot builds (enrich runs without mu)
	lastBDS uint8      // Comm-B register settled on for the frame being applied

	// Latency-chain analysis (nil = off). obs is the reused observation;
	// cprLat / cprLon / cprOK is the position handleCPR decoded from the
	// frame being applied.
	lat            *lchain.Analyzer
	obs            lchain.Obs
	cprLat, cprLon float64
	cprOK          bool

	// Aggregator mode (network.go); nil = standalone. lanesP mirrors
	// net.lanes for the lock-free late-frame path. cur is the station of the
	// frame being applied; laneDueMs the data time of a lanes Tick to run
	// once mu is released; ndd the scratch cache entry of Handle (which does
	// not de-duplicate). snapSide is the aggregator part of snap.
	net       *netState
	lanesP    atomic.Pointer[Lanes]
	cur       model.StationIdx
	laneDueMs int64
	ndd       ddEntry
	snapSide  []netSide

	// Locate Me v2 samples (guess.go): queued under mu, delivered at most
	// about once per second without it.
	guessFn    func([]GuessSample)
	guessQ     []GuessSample
	guessSpare []GuessSample
	guessDue   bool
	guessMu    sync.Mutex

	// Sampled mutex hold time (LockStats): every 1024th handling is timed.
	lockN  uint64
	holdNs int64
	holdK  int64

	// ICAO sharding (shard.go). sh is set on the tracker the engine holds
	// (the front) once SetNetwork has split it: every exported method then
	// delegates to the shards. part is set on each shard; flightSeq is the
	// shards' shared flight-id counter when there is no store. opts are the
	// options New was called with (for creating shards).
	sh        atomic.Pointer[shardSet]
	part      *shardPart
	flightSeq *atomic.Int64
	opts      Options
}

var _ model.FrameSink = (*Tracker)(nil)

// New creates a tracker. It works without a store (Options.Store == nil).
func New(o Options) *Tracker { return newTracker(o, true) }

// newTracker is New; loadRecs = false skips reading the stored records (a
// shard shares its front's record table).
func newTracker(o Options, loadRecs bool) *Tracker {
	t := &Tracker{
		opts:        o,
		recs:        newRecTable(),
		st:          o.Store,
		clock:       o.Clock,
		log:         o.Logger,
		onNotice:    o.OnNotice,
		maxRangeKm:  o.MaxRangeKm,
		maxAircraft: o.MaxAircraft,
		ac:          make(map[uint32]*aircraft, 256),
		feeds:       make(map[string]*feedState),
		acdb:        make(map[uint32]acdbCached),
		afFired:     make(map[uint32]struct{}),
		started:     time.Now(),
		lat:         o.Latency,
		cad:         o.cadence(),
	}
	t.obs.Reset()
	if t.log == nil {
		t.log = slog.Default()
	}
	if !(t.maxRangeKm > 0) {
		t.maxRangeKm = 800
	}
	if t.maxAircraft <= 0 {
		t.maxAircraft = 20000
	}
	if !o.NoErrorCorrection {
		t.decOpts.FixErrors = 1
	}
	cfg := model.DefaultConfig()
	if o.Config != nil {
		cfg = *o.Config
	}
	t.applyConfig(cfg)
	if loadRecs {
		t.loadRecords()
	}
	return t
}

// SetConfig applies a new configuration live: receiver position, antennas,
// feed-to-antenna attribution, storage intervals and the UI aircraft TTL.
// Statistics accumulated for the old antenna set are flushed first.
func (t *Tracker) SetConfig(cfg model.Config) {
	if s := t.sh.Load(); s != nil {
		s.setConfig(t, cfg)
		return
	}
	t.mu.Lock()
	if t.nowMs != 0 && !sameAntennas(t.cfg.Antennas, cfg.Antennas) {
		t.flush(true)
	}
	t.applyConfig(cfg)
	t.snap = nil
	t.mu.Unlock()
}

func sameAntennas(a, b []model.Antenna) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Channel != b[i].Channel {
			return false
		}
	}
	return true
}

// applyConfig rebuilds everything derived from the configuration. Caller
// holds mu (or is New).
func (t *Tracker) applyConfig(cfg model.Config) {
	rebuild := t.slots == nil || !sameAntennas(t.cfg.Antennas, cfg.Antennas)
	oldRx := t.cfg.Receiver
	t.cfg = cfg
	t.cfg.Antennas = append([]model.Antenna(nil), cfg.Antennas...)
	t.cfg.Feeds = append([]model.Feed(nil), cfg.Feeds...)
	if len(t.cfg.Antennas) > maxSlots-1 {
		t.cfg.Antennas = t.cfg.Antennas[:maxSlots-1]
	}
	// Aggregator mode: no receiver geometry and only the combined
	// (station-free) statistics slot; stations have their lanes.
	ants := t.cfg.Antennas
	if t.net != nil {
		ants = nil
	}

	// Receiver geometry: antenna height AMSL = ground + mean antenna AGL.
	t.groundElevM = finiteOr(cfg.Receiver.GroundElevM, 0)
	rx := cfg.Receiver
	if rx.Located && geo.ValidLatLon(rx.Lat, rx.Lon) && t.net == nil {
		agl, n := 0.0, 0
		for _, a := range t.cfg.Antennas {
			if a.HeightAGLM > 0 && !math.IsInf(a.HeightAGLM, 0) {
				agl += a.HeightAGLM
				n++
			}
		}
		if n > 0 {
			agl /= float64(n)
		}
		t.station = geo.NewStation(rx.Lat, rx.Lon, t.groundElevM+agl, 0)
	} else {
		t.station = nil
	}
	if oldRx.Located != rx.Located || oldRx.Lat != rx.Lat || oldRx.Lon != rx.Lon {
		// Geometry of every aircraft refers to the old site.
		for _, ac := range t.ac {
			ac.geoValid = false
		}
	}

	if rebuild {
		t.slots = t.slots[:0]
		t.slots = append(t.slots, newAntStats(""))
		for _, a := range ants {
			t.slots = append(t.slots, newAntStats(a.ID))
		}
		for _, ac := range t.ac {
			ac.antSeen = [maxSlots]int64{}
			ac.minMask, ac.minPosMask, ac.hourMask = 0, 0, 0
		}
	}
	t.chanSlot = [16]uint8{}
	for i, a := range ants {
		if a.Channel >= 0 && a.Channel < len(t.chanSlot) && t.chanSlot[a.Channel] == 0 {
			t.chanSlot[a.Channel] = uint8(i + 1)
		}
	}
	// Feed attribution.
	for _, fs := range t.feedList {
		fs.slot, fs.mlatOnly = 0, false
	}
	for _, f := range t.cfg.Feeds {
		fs := t.feedLocked(f.ID)
		if t.net == nil {
			fs.slot = t.slotOf(f.AntennaID)
		}
		fs.mlatOnly = f.MLATOnly
	}

	ttl := cfg.UI.AircraftTTLs
	if ttl < 10 {
		ttl = 60
	}
	if ttl > 3600 {
		ttl = 3600
	}
	t.ttlMs = int64(ttl) * 1000
	tm := cfg.UI.TrailMinutes
	if tm < 15 {
		tm = 15
	}
	if tm > 120 {
		tm = 120
	}
	t.trailKeepMs = int64(tm) * 60_000
	t.trailEveryS = int64(cfg.Storage.TrailIntervalSec)
	if t.trailEveryS < 1 {
		t.trailEveryS = 5
	}
}

// slotOf returns the statistics slot of an antenna ID (0 = none).
func (t *Tracker) slotOf(antID string) int {
	if antID == "" {
		return 0
	}
	for i, a := range t.cfg.Antennas {
		if a.ID == antID {
			return i + 1
		}
	}
	return 0
}

// SetEHSHook installs the meteorology hook. It is called at most about once
// per aircraft every 2 s, without tracker locks held and never concurrently
// with itself; its result is stored in the aircraft state. nil removes it.
func (t *Tracker) SetEHSHook(fn func(EHSEvent) EHSResult) {
	t.hookMu.Lock()
	t.mu.Lock()
	t.hook = fn
	t.hookPending = false
	t.mu.Unlock()
	t.hookMu.Unlock()
	if s := t.sh.Load(); s != nil {
		s.setHooks(t)
	}
}

// SetOnNotice replaces the notice callback.
func (t *Tracker) SetOnNotice(fn func(Notice)) {
	t.mu.Lock()
	t.onNotice = fn
	t.mu.Unlock()
	if s := t.sh.Load(); s != nil {
		s.setHooks(t)
	}
}

// Run keeps the tracker's clock moving while no frames arrive (so silent
// aircraft expire and minute rows are written) and resolves aircraft
// database look-ups. It returns nil when ctx is cancelled; call Flush
// afterwards to persist the open accumulators.
func (t *Tracker) Run(ctx context.Context) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if s := t.sh.Load(); s != nil {
				s.tick(ctx, t)
				continue
			}
			t.mu.Lock()
			if t.nowMs != 0 {
				// Extrapolate data time by the wall time elapsed since the
				// last tick that frames drove.
				now := t.lastTickMs + time.Since(t.wallAtTick).Milliseconds()
				if now >= t.nextTickMs {
					t.advance(now)
				}
			}
			t.finishSlow()
			t.resolveRefdata(ctx)
		}
	}
}

// Flush writes every open accumulator (partial minute and hour rows, open
// flights, pending positions, increments, records) and waits until the store
// has committed them. Use it on shutdown; the tracker stays usable.
func (t *Tracker) Flush(ctx context.Context) error {
	if s := t.sh.Load(); s != nil {
		return s.flush(ctx, t)
	}
	t.resolveRefdata(ctx)
	t.mu.Lock()
	t.flush(true)
	var ls *Lanes
	if t.net != nil {
		ls = t.net.lanes
	}
	now := t.nowMs
	t.finishSlow()
	if ls != nil {
		ls.FlushAll(now)
	}
	if t.st == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.st.Flush()
}

// acFreshS is how old the aircraft rows may be when FlushAircraftRows
// returns: 1.0.0 refreshed every active airframe's row once a minute.
const acFreshS = rowSaveEveryS

// FlushAircraftRows makes the aircraft table current for a reader of
// aircraft rows (the aircraft list and detail, the aircraft export): when
// the active airframes' rows were last refreshed more than a minute ago
// (data time) it refreshes them now and waits until the store has committed
// them, so the reader sees rows at most a minute old, as with 1.0.0's
// one-minute refresh. Between readers the rows are refreshed every
// Options.AircraftRefresh only. Costs at most one commit a minute.
func (t *Tracker) FlushAircraftRows(ctx context.Context) error {
	parts := []*Tracker{t}
	if s := t.sh.Load(); s != nil {
		parts = s.parts
	}
	wrote := false
	for _, p := range parts {
		if p.flushAircraftRows() {
			wrote = true
		}
	}
	if !wrote || t.st == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.st.Flush()
}

// flushAircraftRows refreshes the aircraft rows of one (part) tracker when
// they are older than acFreshS and reports whether it queued any.
func (t *Tracker) flushAircraftRows() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.nowMs == 0 || t.st == nil {
		return false
	}
	sec := t.nowMs / 1000
	if sec-t.lastAcRefreshSec < acFreshS && len(t.pendSeen) == 0 {
		return false
	}
	t.lastAcRefreshSec = sec
	for _, ac := range t.ac {
		if ac.seenDirty && ac.shown() {
			t.saveSeen(ac, sec)
		}
	}
	rows := append(t.pendSeen, t.heldSeen...)
	if len(rows) == 0 {
		return false
	}
	b := store.NewBatch()
	b.UpsertAircraft(rows)
	t.pendSeen, t.heldSeen = t.pendSeen[:0], t.heldSeen[:0]
	t.st.Submit(b)
	return true
}

// Reset drops all state: aircraft, trails, flights (ids restart with the
// store's), statistics accumulators, counters, records and latency
// statistics. Nothing is written. Use it for a factory reset, after the
// store has been reset.
func (t *Tracker) Reset() {
	if s := t.sh.Load(); s != nil {
		s.reset(t)
		return
	}
	t.mu.Lock()
	t.ac = make(map[uint32]*aircraft, 256)
	t.dd = [ddSets][2]ddEntry{}
	for i, s := range t.slots {
		t.slots[i] = newAntStats(s.id)
	}
	for _, fs := range t.feedList {
		fs.reset()
	}
	t.nowMs, t.nextTickMs, t.lastTickMs, t.startMs = 0, 0, 0, 0
	t.minStart, t.hourStart, t.lastHourPartialMs, t.lastIncSec = 0, 0, 0, 0
	t.setStreamClocks(0)
	t.incDay, t.incDayOK = 0, false
	t.c, t.prev = counters{}, counters{}
	t.msgRate, t.posRate, t.live = 0, 0, 0
	t.msgEW, t.posEW = ewRate{}, ewRate{}
	t.snap, t.snapSide = nil, nil
	t.laneDueMs = 0
	t.guessQ, t.guessDue = t.guessQ[:0], false
	if t.net != nil {
		clear(t.net.geo)
	}
	t.pendPos, t.pendFlights, t.pendSeen, t.heldSeen, t.pendEvents = nil, nil, nil, nil, nil
	t.recs.reset()
	if t.flightSeq != nil && t.part == nil {
		t.flightSeq.Store(0)
	}
	t.notices = nil
	t.hookPending = false
	t.acdb = make(map[uint32]acdbCached)
	t.acdbOrder, t.acdbQ = nil, nil
	t.afQ, t.afFired, t.afOrder = nil, make(map[uint32]struct{}), nil
	t.localFlightID = 0
	t.lateSince = time.Time{}
	t.started = time.Now()
	t.mu.Unlock()
}

// stamp converts a frame's receive time to data-time milliseconds, advances
// the tracker clock and runs the periodic work when a second boundary was
// crossed.
func (t *Tracker) stamp(recv time.Time) int64 {
	var ms int64
	if recv.IsZero() {
		ms = t.nowMs
		if ms == 0 {
			ms = time.Now().UnixMilli()
		}
	} else {
		ms = recv.UnixMilli()
	}
	switch {
	case ms >= t.nextTickMs:
		t.lateSince = time.Time{}
		t.advance(ms)
	case ms > t.nowMs:
		t.lateSince = time.Time{}
		t.nowMs = ms
	case ms < t.nowMs-clockStepMs:
		// Every frame far behind data time for a while: the host clock
		// stepped backwards. Follow it instead of clamping forever.
		if t.lateSince.IsZero() {
			t.lateSince = time.Now()
		} else if time.Since(t.lateSince) >= clockStepHold {
			t.rebase(ms)
			return ms
		}
		ms = t.nowMs
	case ms < t.nowMs-5000:
		ms = t.nowMs // badly late frame: do not rewind field times
	default:
		t.lateSince = time.Time{}
	}
	return ms
}

// clockStepMs is how far behind data time frames must arrive to count as a
// backward step of the host clock.
const clockStepMs = 30_000

// clockStepHold is how long (wall time) every frame must stay that far behind
// before data time is re-initialised from the frames (a var for tests).
var clockStepHold = 5 * time.Second

// rebase re-initialises data time at ms after the host clock stepped
// backwards. Aircraft (whose times are on the old base) are closed and the
// open minute / hour accumulators written, then the clocks restart from ms
// as on the first frame. Caller holds mu.
func (t *Tracker) rebase(ms int64) {
	for key, ac := range t.ac {
		t.remove(key, ac)
	}
	t.dd = [ddSets][2]ddEntry{}
	sec := ms / 1000
	oldHour := t.hourStart
	t.nowMs, t.startMs, t.lastTickMs = ms, ms, ms
	t.wallAtTick = time.Now()
	t.nextTickMs = (sec + 1) * 1000
	t.rollMinute(sec) // writes the old minute (and hour, when it changes)
	t.flush(true)     // and every other stream: their clocks restart at ms
	t.incDayOK = false
	if t.hourStart != oldHour || t.lastHourPartialMs > ms {
		t.lastHourPartialMs = ms
	}
	t.lateSince = time.Time{}
	t.snap = nil
}

// advance moves data time to ms (>= nextTickMs) and runs the 1 Hz work.
func (t *Tracker) advance(ms int64) {
	if t.nowMs == 0 {
		// First frame: initialise the clocks.
		t.nowMs, t.startMs, t.lastTickMs = ms, ms, ms
		t.wallAtTick = time.Now()
		sec := ms / 1000
		t.minStart = sec - mod(sec, 60)
		t.minCovMs = ms
		t.hourStart = sec - mod(sec, 3600)
		t.lastHourPartialMs = ms
		t.setStreamClocks(ms)
		t.nextTickMs = (sec + 1) * 1000
		return
	}
	if ms > t.nowMs {
		t.nowMs = ms
	}
	t.tick(t.nowMs)
	t.nextTickMs = (t.nowMs/1000 + 1) * 1000
}

// ewRate is an exponentially weighted rate with start-up bias correction.
// A plain EWMA starts at 0 and reads 37 % low one time constant after the
// first frame (after start, a reset, or for a new feed / aircraft), so a new
// station showed far too low a rate at first; dividing by the accumulated
// weight 1 - exp(-elapsed/tau) gives the true rate from the first tick on.
type ewRate struct{ ew, w float64 }

func (r *ewRate) add(alpha, x float64) float64 {
	r.ew += alpha * (x - r.ew)
	r.w += alpha * (1 - r.w)
	if r.w <= 0 {
		return 0
	}
	return r.ew / r.w
}

func mod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

// tick is the 1 Hz housekeeping (data time).
func (t *Tracker) tick(now int64) {
	dt := float64(now-t.lastTickMs) / 1000
	t.lastTickMs = now
	t.wallAtTick = time.Now()
	t.tickN++
	if dt <= 0 {
		dt = 1
	}
	alpha := 1 - math.Exp(-dt/rateTauS)

	t.msgRate = t.msgEW.add(alpha, float64(t.c.Frames+t.c.UpdateMsgs-t.prev.Frames-t.prev.UpdateMsgs)/dt)
	t.posRate = t.posEW.add(alpha, float64(t.c.Positions-t.prev.Positions)/dt)
	t.prev = t.c
	for _, fs := range t.feedList {
		n := fs.frames + fs.updateMsgs
		fs.rate = fs.ew.add(alpha, float64(n-fs.prevN)/dt)
		fs.prevN = n
	}
	for _, s := range t.slots {
		s.rate = s.ew.add(alpha, float64(s.totFrames-s.prevFrames)/dt)
		s.prevFrames = s.totFrames
	}

	live := 0
	for key, ac := range t.ac {
		ac.rate = ac.ew.add(alpha, float64(ac.msgs-ac.prevMsgs)/dt)
		ac.prevMsgs = ac.msgs
		age := now - ac.lastSeen
		switch {
		case age > flightGapMs, age > t.ttlMs && !ac.shown():
			t.remove(key, ac)
		case age <= t.ttlMs && ac.shown():
			live++
		}
	}
	t.live = live
	sec := now / 1000
	if p := t.part; p != nil {
		// A shard sees only its part of the sky: the records of the totals
		// use every shard's latest values (shard.go).
		p.set.noteTick(t, p.idx, live, t.msgRate, now)
	} else {
		if live > 0 {
			t.record(recMaxAircraft, float64(live), 0, sec, recordMax, nil)
		}
		if now-t.startMs > 20_000 {
			t.record(recMaxMsgRate, math.Round(t.msgRate*10)/10, 0, sec, recordMax, nil)
		}
	}

	if sec >= t.minStart+60 || sec < t.minStart {
		t.rollMinute(sec) // the minute rows, plus whatever stream is due
	} else if t.streamsDue(now) {
		t.flush(false)
	}
	// Work that runs once mu is released (finishSlow).
	if t.net != nil && t.net.lanes != nil && (t.part == nil || t.part.idx == 0) {
		t.laneDueMs = now // one shard drives the shared lanes
	}
	if len(t.guessQ) > 0 {
		t.guessDue = true
	}
}

// slowPending reports whether finishSlow has work after a frame. Caller
// holds mu.
func (t *Tracker) slowPending() bool {
	return t.hookPending || len(t.notices) > 0 || t.laneDueMs != 0 || t.guessDue
}

// unlockAndDeliver releases mu and delivers queued notices.
func (t *Tracker) unlockAndDeliver() {
	if len(t.notices) == 0 {
		t.mu.Unlock()
		return
	}
	ns := t.notices
	t.notices = nil
	fn := t.onNotice
	t.mu.Unlock()
	if fn != nil {
		for _, n := range ns {
			fn(n)
		}
	}
}

func finiteOr(v, def float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return def
	}
	return v
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func fresh(t, now, ttl int64) bool { return t != 0 && now-t <= ttl }
