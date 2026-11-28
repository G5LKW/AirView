// Package engine wires AirView together and owns every lifecycle.
//
// An Engine loads the configuration, opens the store and builds the
// long-lived components (tracker, met collector, NTP sampler, weather poller,
// analyzer, forecaster). Start launches the "runtime": the goroutines of
// those components plus the feed manager, the hub data port, the scheduler
// and the housekeeping loops. ApplyConfig reconfigures only what changed,
// FactoryReset stops the runtime, wipes the store and the configuration and
// starts the runtime again, Stop shuts everything down in order (feeds,
// tracker flush, store).
//
// # Stored and effective configuration
//
// Config() is the stored configuration: exactly what config.json holds and
// what the settings page edits. EffectiveConfig() is what the components run
// on. The two differ
//
//   - in demo mode (Config.Demo.Enabled in standalone mode, or the -demo
//     flag): the configured feeds are suspended, a synthetic "demo" feed backed
//     by internal/sim is added, and a receiver that is not located uses the
//     simulator's default site;
//   - in remote-data mode: the feeds are replaced by one hub feed to
//     Config.Remote, and with Remote.UseHostReceiver the host's receiver,
//     antennas and feed attribution are adopted;
//   - in remote-web mode: all feeds are suspended (the window shows the host).
//
// None of this is ever written to config.json.
//
// The component pointers returned by the accessors stay valid for the life
// of the Engine, with two exceptions that are replaced on a factory reset or
// a hub port change: Feeds() and Hub(). Call the accessor each time.
//
// # MLAT in aggregator mode (DESIGN §9, WP-E2)
//
// With aggregator.mlat.enabled (default on) the engine builds one
// internal/mlat Engine behind the ingest core (mlatwire.go):
//
//	feeders (AVF/1 :30160, mlat-client :31090, host station)
//	   → ingest grouper → mlatTap (per-station counters) → mlat.Engine
//	   → positions: ingest.HandleUpdate (MLAT, Src mlat-av, Grade, σ) → global tracker
//	   → results: avfsrv / mlatc SendResults, contributors only (DESIGN §8.4)
//	   → planner: ingest interest + mlat-client start/stop_sending
//
// Beast 30004 copies are MapOnly and never reach the engine; the SBS-in
// bridge (aggregator.sbsInLocalPort, 0 = off, the default) feeds an external
// mlat-server's positions as Src mlat-ext and is independent of the engine.
// aggregator.mlat.returnResults switched off takes effect at once (the
// listeners' policies); every other change of aggregator.mlat needs a
// restart (RestartRequired). A standalone station with an uplink receives
// its results through internal/uplink, which injects them into the local
// tracker only: they never enter the feed tee, so they are never forwarded
// to an uplink or the hub (DESIGN §1, §9.7).
//
// # MLAT metrics
//
// mlatmetrics.go writes mlat_minute, station_clock_minute and
// mlat_quality_hour once a minute. Most columns are exact counter deltas
// or end-of-minute gauges. Three need a derivation:
//
// Hold-out error (mlat_quality_hour). For every published hold-out track
// estimate of grade A-C the recorder takes the ADS-B reference state at the
// fix instant (confirmed, fresh, not in probation, not suspended, unstable,
// on the ground or backward: the engine's own scoring rule) and measures
//
//	e_h = | (R · (p_fix − p_ref))_{east,north} |
//
// with R the ECEF→ENU rotation at the reference position. e_h goes into the
// 0.1-decade log histogram of (hour, 1° cell of the reference, altitude band
// of the reference HAE). The reference carries its own airframe latency and
// NACp error, so e_h overstates the true error (internal/mlat WIRING.md:
// 39 m hold-out vs 6.8 m true P50 on simnet).
//
// Synced seconds and sync frames. The clock snapshot is sampled every 10 s
// (at :00.2, :10.2, … :50.2); synced_secs adds 10 s per sample in which the
// station was synced, and sync_frames adds the filter's Accepted30s at the
// samples nearest :20 and :50 (two disjoint 30-s windows).
//
// Fixes and outliers per station. The quality table exposes only sums
// over the last hour of one-minute slots: at time t in minute k,
// S(t) = C(t) − C(end of minute k−60), C the cumulative count. The recorder
// samples δ = 0.2 s after each minute starts and keeps
//
//	Ĉ_k = S_k + Ĉ_{k−59}   (Ĉ = 0 before the engine started),
//
// since C(end of k−60) = C(sample k−59) − e_{k−59}, e_j being the counts in
// the first δ of minute j. The value written for the minute is
// d̂_k = Ĉ_k − Ĉ_{k−1} = S_k − S_{k−1} + d̂_{k−59}. Against the true count
// d_k between two samples, the error ε_k = d̂_k − d_k obeys
//
//	ε_k = ε_{k−59} + e_{k−59} − e_{k−60},
//
// a zero-mean random walk with one step an hour of size |e − e'| (counts of
// 0.2 s: 0 or ±1 at 50 fixes a minute). The cumulative total telescopes, so
// it carries the error of one chain only. Outlier counts come from the same
// recursion on round(OutlierPct · round(used · 100/UsedPct) / 100); a minute
// in which a station had excluded rows only (UsedPct = 0) cannot be
// recovered and contributes no outliers. When the MLAT engine offers exact
// cumulative counters (StationTotals; HOOKS.md), they replace the recursion.
package engine

import (
	"airview/internal/sdr/sdrfeed"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/analytics"
	"airview/internal/config"
	"airview/internal/feed"
	"airview/internal/hub"
	"airview/internal/latency"
	"airview/internal/met"
	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/predict"
	"airview/internal/privacy/publicdto"
	"airview/internal/store"
	"airview/internal/tracker"
	"airview/internal/uplink"
	"airview/internal/weather"
)

// DemoFeedID is the ID of the synthetic simulator feed in demo mode.
const DemoFeedID = "demo"

// RemoteFeedID is the ID of the synthetic hub feed in remote-data mode.
const RemoteFeedID = "remote"

// Errors.
var (
	ErrNotRunning = errors.New("engine: not running")
	ErrRunning    = errors.New("engine: already started")
)

// Options configures New. The zero value is a plain standalone engine.
type Options struct {
	// Demo forces demo mode (the -demo flag): standalone with the simulator
	// feed, whatever the configuration says.
	Demo bool
	// AllowRemoteData lets Config.Mode "remote-data" take effect. When false
	// (the headless Pi build) the mode is treated as standalone.
	AllowRemoteData bool
	// AllowRemoteWeb lets Config.Mode "remote-web" take effect (desktop
	// window only). When false the mode is treated as standalone.
	AllowRemoteWeb bool
	// Offline disables everything that talks to the internet on the
	// engine's own initiative: weather polling, history backfill and the
	// elevation look-up (tests, air-gapped stations). Feeds, NTP and a
	// user-requested aircraft database import are not affected.
	Offline bool
	// Version is reported in Status; "" = model.Version.
	Version string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// DemoSeed fixes the simulator seed (tests); 0 = random.
	DemoSeed uint64
	// ConfigFile is the operator config file (-config); "" = $AIRVIEW_CONFIG
	// from Environ, else <data>/airview.jsonc when it exists (DESIGN §20.3).
	ConfigFile string
	// ConfigSets are "path=value" overrides from the command line (-set).
	ConfigSets []string
	// Environ is the process environment for AIRVIEW_* overrides (as from
	// os.Environ). nil = no environment overrides (tests).
	Environ []string
	// ListenHost is the address the aggregator's feeder listeners bind to
	// ("" = every interface, the normal server setting; "127.0.0.1" for
	// tests and for installs behind a TCP proxy). The SBS-in bridge always
	// binds the loopback address.
	ListenHost string
}

// effective is the configuration the components run on.
type effective struct {
	cfg   model.Config // effective configuration (see the package comment)
	feeds []model.Feed // what the feed manager runs
	mode  string       // effective run mode
	demo  bool
}

// runtimeState is everything that lives between startRuntime and stopRuntime.
type runtimeState struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	fm     *feed.Manager
}

// Engine owns the components. Create it with New.
type Engine struct {
	dir  string
	opts Options
	log  *slog.Logger

	// opMu serialises lifecycle operations: Start, Stop, ApplyConfig,
	// FactoryReset and the asynchronous re-applies.
	opMu    sync.Mutex
	started bool
	stopped bool

	holder   *config.Holder
	layers   *config.Layers // operator file / env / -set overrides (DESIGN §20.3)
	st       *store.Store
	tr       *tracker.Tracker
	met      *met.Collector
	hazards  *met.HazardLog // BDS 4,5 hazard reports (internal/engine/metgrid.go)
	ntp      *ntp.Sampler
	wx       *weather.Poller
	an       *analytics.Analyzer
	fc       *predict.Forecaster
	notices  *broker
	startedT time.Time

	ext *extState                      // trajectory, rfstat, terrain, navdata, alerts (ext.go)
	net *netCore                       // aggregator mode (network.go); nil = standalone. Set once by Start.
	up  atomic.Pointer[uplink.Manager] // uplinks (uplinks.go); nil until one is configured
	// fedGuard is the public map island guard for the federation export
	// (SetFederationGuard; federation.go).
	fedGuard atomic.Pointer[func() publicdto.Guard]

	flightStationSink atomic.Pointer[func([]tracker.FlightStation)]
	rt                *runtimeState                // guarded by opMu
	fm                atomic.Pointer[feed.Manager] // current feed manager (nil when stopped)
	hub               atomic.Pointer[hub.Server]   // running data port (nil when off)
	eff               atomic.Pointer[effective]

	mu          sync.Mutex // guards the fields below
	hostStation *hub.Station
	hubPort     int    // port of the running hub server
	hubErr      string // last hub start error
	bootHTTP    model.HTTPConfig
	// bootStorage is the storage profile part of the configuration the
	// databases were opened with (a change needs a restart).
	bootStorage model.StorageConfig
	restartReq  bool
	wxPersistH  int64 // clock hour (Unix s) of the last full weather persist
	wxPersistAt string
	pruneDue    bool
	imp         importState

	rxGPS atomic.Pointer[model.ReceiverGPS]

	// Latency-chain analysis (latency.go).
	lat      *latency.Analyzer
	latMu    sync.Mutex // guards latRx, latRxSet
	latRx    StationPosition
	latRxSet bool

	// wxFresh is set when a full weather bundle has been queued; the chore
	// loop then flushes the store and rebuilds the forecast products, so a
	// new Open-Meteo forecast (in particular the first one after start) shows
	// up at once instead of at the forecaster's next five-minute rebuild.
	wxFresh atomic.Bool

	reapplyC chan struct{}
	choreC   chan struct{}
	// locate is the Locate Me v2 recorder and hooks (locatewire.go).
	locate atomic.Pointer[locateWiring]
	// ssr is the experimental SSR self-location (ssrwire.go); nil = off.
	ssr atomic.Pointer[ssrWiring]
}

// New creates an engine for the data directory. Nothing is opened or started
// until Start.
func New(dataDir string, opts Options) *Engine {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Version == "" {
		opts.Version = model.Version
	}
	return &Engine{
		dir:      dataDir,
		opts:     opts,
		log:      opts.Logger,
		notices:  newBroker(),
		reapplyC: make(chan struct{}, 1),
		choreC:   make(chan struct{}, 1),
	}
}

// Start loads the configuration, opens the store, builds the components and
// starts them. ctx only bounds the start-up itself; the engine runs until
// Stop.
func (e *Engine) Start(ctx context.Context) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.started {
		return ErrRunning
	}
	if e.stopped {
		return ErrNotRunning
	}

	cfg, layers, warns, err := config.LoadLayered(config.LoadOptions{
		Dir: e.dir, File: e.opts.ConfigFile, Environ: e.opts.Environ, Sets: e.opts.ConfigSets,
	})
	e.layers = layers
	if err != nil {
		// The returned configuration is always usable.
		e.log.Warn("configuration problem", "err", err)
		warns = append(warns, "configuration: "+err.Error())
	}
	mainOpts := e.mainStoreOptions(cfg)
	// The writer's fallback seal (3 x the interval without a regular seal
	// from the checkpointer) follows the profile too.
	store.SetTrailSealInterval(mainOpts.Profile.SealInterval)
	st, err := store.OpenWithOptions(e.dir, mainOpts)
	if err != nil {
		return fmt.Errorf("engine: open database: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = st.Close()
		return err
	}
	// Aggregator mode is fixed for the life of the engine (network.go).
	if e.wantNetwork(cfg) {
		nc, err := e.openNet(cfg, st)
		if err != nil {
			_ = st.Close()
			return err
		}
		e.net = nc
	}

	e.st = st
	e.startedT = time.Now()
	e.holder = config.NewHolder(cfg)
	e.bootHTTP = cfg.HTTP
	e.bootStorage = cfg.Storage
	eff := e.computeEffective(cfg)
	e.eff.Store(eff)

	e.ntp = ntp.NewSampler(cfg.NTP, ntp.Options{Logger: e.log})
	e.seedNTP(ctx)
	e.ntp.OnSample(func(s ntp.Sample) {
		e.st.AddNTPSamples([]store.NTPSample{{
			T: s.T / 1000, Server: s.Server, OffsetMs: s.OffsetMs, DelayMs: s.DelayMs, Stratum: s.Stratum,
		}})
	})

	e.buildLatency(ctx)
	e.syncLatencyReceiver()

	effCfg := config.Clone(eff.cfg)
	tro := tracker.Options{
		Store:    st,
		Clock:    e.ntp,
		Logger:   e.log,
		Config:   &effCfg,
		OnNotice: e.onTrackerNotice,
		Latency:  e.lat,
	}
	// Persistence cadence of the storage profile (0 = tracker default; the
	// server profile leaves them to the aggregator lanes).
	prof := st.Profile()
	tro.TrailFlush, tro.IncFlush, tro.HeatFlush, tro.RangePartial = prof.TrailFlush, prof.IncFlush, prof.HeatFlush, prof.RangePartial
	tro.AircraftRefresh = prof.AircraftRefresh
	if e.net != nil {
		tro.MaxRangeKm = float64(cfg.Aggregator.Lanes.MaxRangeKm)
		tro.MaxAircraft = cfg.Aggregator.Limits.MaxAircraft
	}
	e.tr = tracker.New(tro)
	if e.net != nil {
		if err := e.attachNet(e.net, cfg); err != nil {
			e.closeNet()
			e.net = nil
			_ = st.Close()
			return err
		}
	}
	e.met = met.New(st, met.Options{Logger: e.log})
	e.tr.SetEHSHook(e.ehsHook)
	e.hazards = met.NewHazardLog()
	e.tr.SetHazardHook(e.hazardHook)
	e.buildExt(cfg)

	e.installLocate(cfg)
	// Pages merge what the tracker holds in memory, so the profile's
	// longer flush intervals never show.
	e.an = analytics.New(st, e.EffectiveConfig, analytics.WithPending(e.tr.PendingIncs))
	e.fc = predict.NewWithOptions(st, e.EffectiveConfig, predict.Options{Logger: e.log})

	e.wx = weather.NewPoller(e.weatherConfig(cfg), eff.cfg.Receiver, weather.PollerOptions{Logger: e.log})
	e.wx.OnUpdate(e.onWeather)

	e.started = true
	e.syncUplinks(cfg, eff)
	e.syncSSR(eff.cfg)
	e.startRuntime()

	for _, w := range warns {
		e.Notify("warn", "Configuration: "+w)
	}
	e.log.Info("engine started", "dataDir", e.dir, "mode", eff.mode, "demo", eff.demo,
		"located", eff.cfg.Receiver.Located, "feeds", len(eff.feeds))
	return nil
}

// Stop shuts the engine down in order: feeds, hub, background loops, tracker
// flush, store. It is safe to call more than once and without Start.
func (e *Engine) Stop() {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if !e.started || e.stopped {
		e.stopped = true
		return
	}
	e.stopped = true
	e.cancelImport()
	if e.ext != nil {
		e.ext.nd.CancelImport()
	}
	e.stopRuntime(true)
	e.closeLocate()
	e.closeSSR()
	e.closeUplinks()
	e.closeNet()
	if err := e.st.Close(); err != nil {
		e.log.Warn("closing database", "err", err)
	}
	e.notices.close()
	e.log.Info("engine stopped")
}

// startRuntime launches every goroutine. Caller holds opMu.
func (e *Engine) startRuntime() {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &runtimeState{ctx: ctx, cancel: cancel}
	e.rt = rt
	eff := e.eff.Load()

	run := func(name string, fn func(context.Context) error) {
		rt.wg.Add(1)
		go func() {
			defer rt.wg.Done()
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				e.log.Error("component stopped", "component", name, "err", err)
			}
		}()
	}
	run("tracker", e.tr.Run)
	run("met", e.met.Run)
	run("ntp", e.ntp.Run)
	run("weather", e.wx.Run)
	run("forecast", e.fc.Run)

	t := &tee{tr: e.tr, hub: &e.hub, up: &e.up, ssr: &e.ssr}
	if e.net != nil {
		t.host = e.net.host
	}
	fm := feed.NewWithOptions(t, feed.Options{Logger: e.log})
	fm.RegisterType(model.FeedDemo, e.demoFactory)
	fm.RegisterType(model.FeedRTLTCP, sdrfeed.Factory(func(format string, args ...any) {
		e.log.Info(fmt.Sprintf(format, args...), "component", "sdr")
	}))
	fm.OnStation(e.onStation)
	fm.OnReceiverPosition(e.onReceiverPosition)
	fm.OnClockEvent(e.onClockEvent)
	fm.SetHubOptions(hubOptions(e.holder.Get()))
	fm.Apply(eff.feeds)
	rt.fm = fm
	e.fm.Store(fm)

	e.syncHub(rt, e.holder.Get(), eff)
	e.syncNetListeners(rt, e.holder.Get())

	run("scheduler", func(ctx context.Context) error { e.schedulerLoop(ctx); return nil })
	run("feedwatch", func(ctx context.Context) error { e.feedWatchLoop(ctx, fm); return nil })
	run("reapply", func(ctx context.Context) error { e.reapplyLoop(ctx); return nil })
	run("chores", func(ctx context.Context) error { e.choreLoop(ctx); return nil })
	e.startExt(rt, run)
}

// stopRuntime stops every goroutine. With flush set the tracker's open
// accumulators are written (not wanted before a factory reset). Caller holds
// opMu.
func (e *Engine) stopRuntime(flush bool) {
	rt := e.rt
	if rt == nil {
		return
	}
	e.rt = nil
	// 1. No more frames: feeder listeners (they wait until their last
	// frames reached the ingest), then the local feeds.
	e.stopNetListeners()
	rt.fm.Close()
	e.flushIngest()
	e.fm.Store(nil)
	// 2. Data port.
	if h := e.hub.Swap(nil); h != nil {
		_ = h.Close()
	}
	// 3. Background loops (the met collector flushes on its way out).
	rt.cancel()
	rt.wg.Wait()
	e.stopExt()
	// 4. Tracker accumulators, then the write queue.
	if flush {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := e.tr.Flush(ctx); err != nil {
			e.log.Warn("final tracker flush", "err", err)
		}
		e.persistLatency(ctx)
		cancel()
	}
}

// tee is the feed manager's sink (DESIGN §1 "Uplink tee"): the tracker in
// standalone mode or the network ingest as the host station in aggregator
// mode, plus the hub data port while it is running, plus the uplinks while
// any is configured. The hub and the uplinks see the local feed IDs; the
// uplink manager drops whatever must not leave the station.
type tee struct {
	tr   *tracker.Tracker
	host *hostSink // aggregator mode: station #1 into the network ingest
	hub  *atomic.Pointer[hub.Server]
	up   *atomic.Pointer[uplink.Manager]
	ssr  *atomic.Pointer[ssrWiring] // experimental SSR self-location (nil = off)
}

func (t *tee) HandleFrame(f model.RawFrame) {
	if t.ssr != nil {
		if s := t.ssr.Load(); s != nil {
			s.observe(&f)
		}
	}
	if t.host != nil {
		t.host.HandleFrame(f)
	} else {
		t.tr.HandleFrame(f)
	}
	if h := t.hub.Load(); h != nil {
		h.Broadcast(f)
	}
	if m := t.up.Load(); m != nil && m.Active() {
		m.HandleFrame(f)
	}
}

func (t *tee) HandleUpdate(u model.DecodedUpdate) {
	if t.host != nil {
		t.host.HandleUpdate(u)
	} else {
		t.tr.HandleUpdate(u)
	}
	if h := t.hub.Load(); h != nil {
		h.BroadcastUpdate(u)
	}
	if m := t.up.Load(); m != nil && m.Active() {
		m.HandleUpdate(u)
	}
}

// ehsHook adapts the tracker's EHS hook to the met collector.
func (e *Engine) ehsHook(ev tracker.EHSEvent) tracker.EHSResult {
	d := e.met.Observe(met.Input{
		T: ev.T, ICAO: ev.ICAO, Lat: ev.Lat, Lon: ev.Lon, AltFt: ev.AltFt,
		GS: ev.GS, Track: ev.Track, TAS: ev.TAS, IAS: ev.IAS, Mach: ev.Mach,
		MagHeading: ev.MagHeading, Roll: ev.Roll,
		MRARWindSpd: ev.MRARWindSpd, MRARWindDir: ev.MRARWindDir, MRARTemp: ev.MRARTemp,
	})
	return tracker.EHSResult{
		WindDir: d.WindDir, WindSpd: d.WindSpd, OAT: d.OAT,
		HasWind: d.HasWind, HasOAT: d.HasOAT,
	}
}

func (e *Engine) seedNTP(ctx context.Context) {
	now := time.Now().Unix()
	rows, err := e.st.NTPSamples(ctx, now-7*86400, now+1, 8192)
	if err != nil {
		e.log.Warn("loading NTP history", "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	samples := make([]ntp.Sample, len(rows))
	for i, r := range rows {
		samples[i] = ntp.Sample{
			T: r.T * 1000, Server: r.Server, OffsetMs: r.OffsetMs, DelayMs: r.DelayMs,
			Stratum: r.Stratum, SmoothedMs: r.OffsetMs, Sent: 1, Replies: 1,
		}
	}
	e.ntp.Seed(samples)
}

func (e *Engine) onReceiverPosition(feedID string, pos feed.ReceiverPosition) {
	e.rxGPS.Store(&model.ReceiverGPS{
		FeedID: feedID, Lat: pos.Lat, Lon: pos.Lon, AltM: pos.AltM, T: time.Now().UnixMilli(),
	})
	e.syncLatencyReceiver()
	if m := e.up.Load(); m != nil && m.Active() {
		// Radarcape type '5' altitudes are GNSS heights above the ellipsoid.
		m.HandleGNSS(feedID, pos.Lat, pos.Lon, pos.AltM, model.AltRefHAE, ntp.Now())
	}
}

// ---------------------------------------------------------------------------
// Accessors
// ---------------------------------------------------------------------------

// DataDir returns the data directory.
func (e *Engine) DataDir() string { return e.dir }

// Version returns the application version reported in Status.
func (e *Engine) Version() string { return e.opts.Version }

// Config returns the holder of the stored configuration (nil before Start).
// Do not call Set / Update on it: use ApplyConfig so that the change is
// validated, saved and applied.
func (e *Engine) Config() *config.Holder { return e.holder }

// EffectiveConfig returns a copy of the configuration the components run on
// (see the package comment). Before Start it is the factory default.
func (e *Engine) EffectiveConfig() model.Config {
	if eff := e.eff.Load(); eff != nil {
		return config.Clone(eff.cfg)
	}
	return model.DefaultConfig()
}

// Mode returns the effective run mode (model.Mode* constant).
func (e *Engine) Mode() string {
	if eff := e.eff.Load(); eff != nil {
		return eff.mode
	}
	return model.ModeStandalone
}

// Demo reports whether the simulator feed is active.
func (e *Engine) Demo() bool {
	eff := e.eff.Load()
	return eff != nil && eff.demo
}

// Store returns the database (nil before Start).
func (e *Engine) Store() *store.Store { return e.st }

// Tracker returns the live aircraft table (nil before Start).
func (e *Engine) Tracker() *tracker.Tracker { return e.tr }

// Analyzer returns the read-side analysis (nil before Start).
func (e *Engine) Analyzer() *analytics.Analyzer { return e.an }

// Forecaster returns the forecast products (nil before Start).
func (e *Engine) Forecaster() *predict.Forecaster { return e.fc }

// Met returns the aircraft-derived meteorology collector (nil before Start).
func (e *Engine) Met() *met.Collector { return e.met }

// NTP returns the NTP sampler (nil before Start).
func (e *Engine) NTP() *ntp.Sampler { return e.ntp }

// Weather returns the weather poller (nil before Start).
func (e *Engine) Weather() *weather.Poller { return e.wx }

// Feeds returns the current feed manager, or nil while the engine is
// stopped or in the middle of a factory reset. It is replaced by a factory
// reset: do not keep the pointer.
func (e *Engine) Feeds() *feed.Manager { return e.fm.Load() }

// FeedStatus returns the runtime state of every feed (never nil).
func (e *Engine) FeedStatus() []model.FeedStatus {
	if fm := e.fm.Load(); fm != nil {
		if s := fm.Status(); s != nil {
			return s
		}
	}
	return []model.FeedStatus{}
}

// Hub returns the running data port server, or nil when it is off.
func (e *Engine) Hub() *hub.Server { return e.hub.Load() }

// HubStatus returns the data port state.
func (e *Engine) HubStatus() model.HubStatus {
	if h := e.hub.Load(); h != nil {
		return h.Status()
	}
	var s model.HubStatus
	if e.holder != nil {
		c := e.holder.Get()
		s.Port = c.Hub.Port
	}
	return s
}

// HubError returns the reason the data port could not be started ("" = none).
func (e *Engine) HubError() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hubErr
}

// ReceiverGPS returns the receiver's own GPS position, when a feed reported
// one.
func (e *Engine) ReceiverGPS() *model.ReceiverGPS {
	if p := e.rxGPS.Load(); p != nil {
		c := *p
		return &c
	}
	return nil
}

// RestartRequired reports whether a saved setting only takes effect after a
// restart of the application (HTTP port or bind address).
func (e *Engine) RestartRequired() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.restartReq
}

// Running reports whether the engine is started and not stopped.
func (e *Engine) Running() bool {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.started && !e.stopped
}

// Status returns the engine part of model.Status. The server adds GUI,
// HTTPPort, AdminLocked and TileCacheHosts.
func (e *Engine) Status() model.Status {
	now := time.Now()
	s := model.Status{
		App:      model.AppName,
		Version:  e.opts.Version,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Now:      now.UnixMilli(),
		Started:  e.startedT.UnixMilli(),
		Mode:     model.ModeStandalone,
		DataDir:  e.dir,
		Feeds:    []model.FeedStatus{},
		Station:  model.StationInfo{Source: "none"},
	}
	eff := e.eff.Load()
	if eff == nil || e.holder == nil {
		return s
	}
	s.Mode = eff.mode
	s.Demo = eff.demo
	s.Located = eff.cfg.Receiver.Located
	s.TileCache = eff.cfg.HTTP.TileCache
	s.Feeds = e.FeedStatus()
	s.Counts = e.tr.Snapshot().Counts
	s.DBBytes = e.st.DBSize()
	s.NTP = e.ntp.Status()
	s.Hub = e.HubStatus()
	s.ReceiverGPS = e.ReceiverGPS()
	if sp := stationOf(eff.cfg, s.ReceiverGPS); sp.Source != "none" {
		lat, lon := sp.Lat, sp.Lon
		s.Station = model.StationInfo{Lat: &lat, Lon: &lon, Source: sp.Source}
	}
	return s
}
