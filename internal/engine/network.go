package engine

// Aggregator mode: the station runtime (docs/aggregator/DESIGN.md §1, §2,
// §6; WP-E).
//
// When the stored configuration says mode "aggregator" at Start (and the
// -demo flag does not force standalone), the engine opens the station
// registry (<data>/stations.db), builds the account service on it, puts the
// tracker into network mode with per-station lanes, and runs the network
// ingest in front of the tracker. The host's own feeds become station #1
// ("host", owned by the admin, exact location, anchor-eligible), and the
// compat Beast listener (30004) and the localhost SBS-in bridge feed the
// same ingest. Feeder listeners stay closed until an admin account with an
// authenticator exists (DESIGN §7).
//
// Switching between standalone and aggregator needs a restart: the mode is
// fixed at Start, a stored change only sets RestartRequired.
//
// Lifetimes:
//
//   - netCore (registry, accounts, lanes, ingest, station runtimes, the
//     admin/host maintenance loop) lives from Start to Stop;
//   - the listeners (Beast, SBS-in) are part of the runtime: they stop
//     before the feed manager on stopRuntime (factory reset, Stop) and
//     follow port changes on ApplyConfig.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/auth"
	"airview/internal/avf"
	"airview/internal/geoid"
	"airview/internal/ingest"
	"airview/internal/ingest/avfsrv"
	"airview/internal/ingest/beastin"
	"airview/internal/ingest/mlatc"
	"airview/internal/mlat"
	"airview/internal/mlat/group"
	"airview/internal/mlat/ref"
	"airview/internal/mlat/survey"
	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/registry"
	"airview/internal/registry/trust"
	"airview/internal/store"
	"airview/internal/tracker"
)

// AccountOptions maps the file-configurable account policy
// (aggregator.auth) onto the auth service options. cmd/airview's
// "admin init|reset" uses the same mapping as the running server.
func AccountOptions(agg model.Aggregator) auth.Options {
	a := agg.Auth
	return auth.Options{
		Issuer: agg.Name,
		Policy: auth.Policy{
			OwnerTTL:  time.Duration(a.OwnerSessionDays) * 24 * time.Hour,
			OwnerIdle: time.Duration(a.OwnerIdleDays) * 24 * time.Hour,
			AdminTTL:  time.Duration(a.AdminSessionHours) * time.Hour,
			AdminIdle: time.Duration(a.AdminIdleMinutes) * time.Minute,
		},
		IPThrottle:         auth.NewThrottle(a.LoginFreeFailures, 15*time.Minute, 5*time.Minute, 16384),
		RegistrationClosed: a.Registration == model.RegistrationClosed,
	}
}

// OpenRegistry opens <dir>/stations.db with the policy of the aggregator
// configuration.
func OpenRegistry(dir string, agg model.Aggregator, log *slog.Logger) (*registry.Registry, error) {
	return registry.OpenDir(dir, registry.Options{Policy: registry.PolicyFromConfig(agg), Logger: log})
}

// Host station defaults.
const (
	hostDefaultName = "host"
	// netMaintainEvery is the period of the admin / host maintenance loop
	// once the admin exists (every 2 s before).
	netMaintainEvery = 15 * time.Second
	bridgeFeedID     = "mlat-ext" // FeedID of SBS-in bridge updates
)

// netCore is the aggregator state that lives from Start to Stop.
type netCore struct {
	log      *slog.Logger
	reg      *registry.Registry
	accounts *auth.Service
	lanes    *tracker.Lanes
	ingest   NetIngest
	core     *ingest.Ingest  // the station-aware ingest core (internal/ingest)
	group    *groupIngest    // the fallback grouper, only when the core cannot be built
	hostSess *ingest.Session // the host station's ingest session (core only)
	// stationsDB holds the per-station statistics (<data>/stations.db, the
	// same file as the registry; DESIGN §6.6). nil when it could not be
	// opened: the lanes then write to airview.db (ant = "s<idx>").
	stationsDB *store.Store
	host       *hostSink
	stations   *StationRuntimes

	ready   atomic.Bool  // an admin with an authenticator exists (listener gate)
	adminID atomic.Int64 // owner of the host station (0 = not yet known)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	flightStationRows atomic.Uint64 // flight_station rows emitted by the lanes

	mu       sync.Mutex // guards the fields below
	beast    *beastin.Server
	beastReg *registry.BeastRegistry
	beastAt  string // listen address of the running Beast listener
	beastErr string
	sbs      *sbsBridge
	sbsAt    string
	sbsErr   string
	avf      *avfState
	avfErr   string
	mlatc    *mlatc.Server // mlat-client listener (aggregator.mlatPort)
	mlatcAt  string
	mlatcUDP string // UDP settings of the running listener (restart key)
	mlatcErr string

	// fed is the federation node (federation.go).
	fed fedState
	// minutes is the selective-mode MINUTE_STATS merge (minutes.go).
	minutes stationMinutes

	// mlat is the MLAT engine (nil when aggregator.mlat.enabled is off or
	// the ingest core could not be built); set once in attachNet.
	mlat *mlat.Engine
	// mlatHS tracks the MLAT counters for the health line (under mu).
	mlatHS mlatHealthState
	// mlatTap counts the MLAT engine's input per station; mlatRec writes
	// the MLAT metrics (mlatmetrics.go). Both nil without the engine.
	mlatTap *mlatTap
	mlatRec *mlatRecorder
	// trust is the timing quarantine between the ingest and the MLAT
	// engine (internal/registry/trust); nil without the engine.
	trust *trust.Gate
	// surv is the reverse-MLAT survey in front of the quarantine
	// (surveywire.go; nil without the engine or when disabled), survFwd
	// the engine's survey-request seam, survReg its registry adapter.
	surv    *survey.Surveyor
	survFwd *surveyFwd
	survReg *surveyRegistry
	// bundler is the network bundle adjustment (bundlewire.go; nil = off).
	bundler *survey.Bundler
	// hints are the Locate Me v2 search hints of unlocated stations for
	// the survey (surveyloc.go; nil without the survey).
	hints *surveyHints
	// posNotices keeps the owner notices of MLAT position-source changes.
	posNotices *posNotices
	// mlatWx feeds the host's weather bundles to the MLAT reference store
	// (GNSS−baro field fallback where no cell is learned; H-clock).
	mlatWx *ref.BundleWeather
	// flushEst is the adaptive flush estimator (aggregator.ingest.adaptiveFlush; nil = fixed flush).
	flushEst *group.FlushEstimator
	// mlatBoot is the aggregator.mlat block the engine was built with
	// (changes take effect after a restart).
	mlatBoot model.AggMLAT

	// AVF state that outlives a listener restart (port change, factory
	// reset): the clone rule and the seq watermarks of resuming feeders.
	clones     *registry.CloneGuard
	watermarks *avf.WatermarkStore
	rxLoc      avfLocations
	hostGeo    model.StationGeo // last host location written to the registry
	hostLoc    bool
	hostName   string
	hostSeen   uint64 // host frame count at the last Seen
}

// NetworkStatus is the operator view of the aggregator runtime (admin
// scope: it contains listener addresses and counters, no station data).
type NetworkStatus struct {
	Aggregator bool   `json:"aggregator"`
	AdminReady bool   `json:"adminReady"`
	HostSID    string `json:"hostSid,omitempty"`
	HostFrames uint64 `json:"hostFrames"`
	// Stations counts the registered stations per admission state.
	Stations map[string]int `json:"stations"`
	Lanes    int            `json:"lanes"` // stations with a live lane
	// Core holds the ingest core's counters (internal/ingest); Fallback
	// those of the engine's minimal grouper when the core is not in use.
	Core     *ingest.Stats   `json:"core,omitempty"`
	Fallback *NetIngestStats `json:"fallback,omitempty"`
	Beast    ListenerStatus  `json:"beast"`
	SBSIn    ListenerStatus  `json:"sbsIn"`
	AVF      ListenerStatus  `json:"avf"`
	MLATC    ListenerStatus  `json:"mlatc"`
	Runtimes int             `json:"runtimes"` // active per-station runtimes
	// FlightStationRows counts flight_station rows emitted by the lanes.
	FlightStationRows uint64            `json:"flightStationRows"`
	Lock              tracker.LockStats `json:"lock"`
	BeastStats        *beastin.Stats    `json:"beastStats,omitempty"`
	AVFStats          *avfsrv.Stats     `json:"avfStats,omitempty"`
	MLATCStats        *mlatc.Stats      `json:"mlatcStats,omitempty"`
	// MLAT holds the MLAT engine's counters (nil when it does not run).
	MLAT *mlat.Stats `json:"mlat,omitempty"`
	// Trust: the timing quarantine of new stations (internal/registry/trust).
	Trust *trust.Stats `json:"trust,omitempty"`
	// AVFPin is the SPKI pin feeders configure (uplinks[].serverPin).
	AVFPin string `json:"avfPin,omitempty"`
}

// ListenerStatus describes one feeder listener.
type ListenerStatus struct {
	Port      int    `json:"port"`            // configured port (0 = off)
	Listening bool   `json:"listening"`       // accepting connections
	Addr      string `json:"addr,omitempty"`  // bound address
	Error     string `json:"error,omitempty"` // last start error
	Note      string `json:"note,omitempty"`  // why it is not running
	// UDP is the mlat-client UDP transport state ("active", "off" or the
	// bind error; mlat-client listener only).
	UDP string `json:"udp,omitempty"`
}

// HealthCheck is one line of the health endpoint.
type HealthCheck struct {
	State string // "ok", "off", "closed", "not built", or a failure text
	Fail  bool   // the service cannot do its job: the endpoint answers 503
	// Degraded: the service works but in a reduced way (the endpoint
	// answers 200 "degraded").
	Degraded bool
}

// writerStall is how long a store may go without a successful flush while
// rows are waiting before the health endpoint reports it as failing.
const writerStall = 2 * time.Minute

// HealthChecks returns the aggregator's health lines (SO-2): every
// configured feeder listener (a configured port that could not be bound
// fails the check) and the writers of airview.db and stations.db. Cheap: no
// database query. nil in standalone mode.
func (e *Engine) HealthChecks() map[string]HealthCheck {
	nc := e.net
	if nc == nil {
		return nil
	}
	cfg := e.EffectiveConfig()
	out := map[string]HealthCheck{}
	nc.mu.Lock()
	lst := []struct {
		name string
		port int
		up   bool
		err  string
	}{
		{"listener.avf", cfg.Aggregator.AVFPort, nc.avf != nil, nc.avfErr},
		{"listener.beast", cfg.Aggregator.BeastPort, nc.beast != nil, nc.beastErr},
		{"listener.sbsIn", cfg.Aggregator.SBSInLocalPort, nc.sbs != nil, nc.sbsErr},
	}
	if nc.mlat != nil && nc.core != nil {
		lst = append(lst, struct {
			name string
			port int
			up   bool
			err  string
		}{"listener.mlatc", cfg.Aggregator.MLATPort, nc.mlatc != nil, nc.mlatcErr})
	}
	nc.mu.Unlock()
	ready := e.AdminReady()
	for _, l := range lst {
		switch {
		case l.port == 0:
			out[l.name] = HealthCheck{State: "off"}
		case l.err != "":
			out[l.name] = HealthCheck{State: "failed: " + l.err, Fail: true}
		case l.up && !ready:
			out[l.name] = HealthCheck{State: "closed until the admin exists"}
		case l.up:
			out[l.name] = HealthCheck{State: "listening"}
		case !ready:
			out[l.name] = HealthCheck{State: "waiting for the admin"}
		default:
			out[l.name] = HealthCheck{State: "not listening", Fail: true}
		}
	}
	if _, ok := out["listener.mlatc"]; !ok && cfg.Aggregator.MLATPort != 0 {
		out["listener.mlatc"] = HealthCheck{State: "off (the MLAT engine is not running)"}
	}
	switch {
	case !cfg.Aggregator.MLAT.Enabled:
		out["mlat"] = HealthCheck{State: "off"}
	case nc.mlat == nil:
		out["mlat"] = HealthCheck{State: "not running (see the log)"}
	default:
		out["mlat"] = nc.mlatHealth(time.Now())
	}
	now := time.Now()
	for name, db := range map[string]*store.Store{"store.main": e.st, "store.stations": nc.stationsDB} {
		if db == nil {
			continue
		}
		w := db.WriterState()
		switch {
		case w.QueuedRows > 0 && !w.LastFlush.IsZero() && now.Sub(w.LastFlush) > writerStall:
			msg := "no successful write for " + now.Sub(w.LastFlush).Round(time.Second).String()
			if w.LastError != "" {
				msg += ": " + w.LastError
			}
			out[name] = HealthCheck{State: msg, Fail: true}
		case w.Dropped > 0:
			out[name] = HealthCheck{State: "ok (" + strconv.FormatUint(w.Dropped, 10) + " rows dropped on queue overflow since start)"}
		default:
			out[name] = HealthCheck{State: "ok"}
		}
	}
	return out
}

// mlatDegradeHold is how long a growing ADSBMuted counter or a recovered
// clock-filter panic keeps the "mlat" health line degraded.
const (
	mlatMutedHold = 2 * time.Minute
	mlatPanicHold = time.Hour
	// mlatStartupGrace: muting within this time after the first ADS-B
	// verification is the expected start-up sync (the clocks of a fresh
	// network converge while the first fixes are checked; release soak:
	// muted from minute 6 to 9 of every start). It is shown, not degraded.
	mlatStartupGrace = 10 * time.Minute
)

// mlatHealthState remembers when the MLAT engine's breaker and panic
// counters last grew (fix:core hook: ADSBMuted, PanicResets) and when ADS-B
// verification first ran.
type mlatHealthState struct {
	muted, panics     uint64
	mutedAt, panicsAt time.Time
	verifyStart       time.Time
}

// mlatHealth is the "mlat" health line: degraded while TDOA verification
// is muted by the network-wide breaker (SCALE-05) or for an hour after a
// recovered clock-filter or engine panic (SCALE-01).
func (nc *netCore) mlatHealth(now time.Time) HealthCheck {
	st := nc.mlat.Stats()
	nc.mu.Lock()
	defer nc.mu.Unlock()
	return nc.mlatHS.update(now, st.ADSBMuted, st.Clock.PanicResets+st.AdvancePanics, st.ADSBConfirmed+st.ADSBContradicted)
}

// update takes the current counters and returns the health line. verified
// is the number of ADS-B verification verdicts so far.
func (h *mlatHealthState) update(now time.Time, muted, panics, verified uint64) HealthCheck {
	if verified > 0 && h.verifyStart.IsZero() {
		h.verifyStart = now
	}
	if muted > h.muted {
		h.mutedAt = now
	}
	if panics > h.panics {
		h.panicsAt = now
	}
	h.muted, h.panics = muted, panics
	var why []string
	startup := false
	if !h.mutedAt.IsZero() && now.Sub(h.mutedAt) < mlatMutedHold {
		if !h.verifyStart.IsZero() && h.mutedAt.Sub(h.verifyStart) < mlatStartupGrace {
			startup = true
		} else {
			why = append(why, "TDOA verification muted")
		}
	}
	if !h.panicsAt.IsZero() && now.Sub(h.panicsAt) < mlatPanicHold {
		why = append(why, "recovered panic within the last hour ("+strconv.FormatUint(panics, 10)+" since start)")
	}
	if len(why) == 0 {
		if startup {
			return HealthCheck{State: "ok (start-up sync: TDOA verification muted)"}
		}
		return HealthCheck{State: "ok"}
	}
	return HealthCheck{State: "degraded: " + strings.Join(why, "; "), Degraded: true}
}

// errNetDisabled is returned by network accessors in standalone mode.
var errNetDisabled = errors.New("engine: not in aggregator mode")

// wantNetwork reports whether cfg asks for aggregator mode and the options
// allow it.
func (e *Engine) wantNetwork(cfg model.Config) bool {
	return cfg.Mode == model.ModeAggregator && !e.opts.Demo
}

// openNet opens the registry and the account service. The tracker is not
// built yet: attachNet finishes the wiring.
func (e *Engine) openNet(cfg model.Config, mainDB *store.Store) (*netCore, error) {
	nc := &netCore{
		log:    e.log.With("component", "network"),
		clones: registry.NewCloneGuard(), watermarks: &avf.WatermarkStore{},
	}
	ropts := registry.Options{
		Policy: registry.PolicyFromConfig(cfg.Aggregator),
		Logger: e.log.With("component", "registry"),
		// A deleted station's rows in airview.db (flight_station,
		// mlat_minute); the registry job itself covers stations.db.
		DeleteExtra: func(ctx context.Context, idx model.StationIdx, sid string) error {
			_, err := mainDB.DeleteStationData(ctx, store.StationKeys{Prefix: store.StationAntPrefix(idx), SID: sid})
			return err
		},
		// The station's lane is discarded (never flushed again, never
		// re-created) and the rows it already queued are written before
		// the job deletes them (C4).
		BeforeDelete: func(ctx context.Context, idx model.StationIdx) error {
			if nc.lanes != nil {
				nc.lanes.Drop(idx)
			}
			for _, db := range []*store.Store{nc.stationsDB, mainDB} {
				if db != nil {
					if err := db.Flush(); err != nil {
						return err
					}
				}
			}
			return nil
		},
		// Owner exports carry the statistics of the owner's stations.
		ExportExtra: func(ctx context.Context, _ int64, sts []registry.Station, add func(string, any) error) error {
			for _, s := range sts {
				k := store.StationKeys{Prefix: store.StationAntPrefix(s.Idx), SID: s.SID}
				pre := "stations/" + s.SID + "/"
				// History from the current owner's assignment on: a
				// re-claimed station does not export the previous owner's.
				from := int64(0)
				if !s.OwnedSince.IsZero() {
					from = s.OwnedSince.Unix()
				}
				sub := func(name string, v any) error { return add(pre+name, v) }
				for _, db := range []*store.Store{nc.stationsDB, mainDB} {
					if db == nil {
						continue
					}
					if err := db.ExportStationData(ctx, store.ExportSpec{Keys: k, From: from}, sub); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	// Per-station statistics in stations.db, beside the registry tables.
	sprof, sover := StorageProfile(cfg, store.KindStations, e.dir, true)
	if len(sover) > 0 {
		e.log.Info("storage profile overrides", "db", "stations.db", "settings", sover)
	}
	sdb, err := store.OpenStations(e.dir, store.Options{
		Logger: e.log.With("component", "stations-db"), MaxQueueRows: cfg.Aggregator.Storage.QueueRows,
		Profile: sprof,
	})
	if err != nil {
		nc.log.Error("stations.db statistics cannot be opened; station statistics go to airview.db", "err", err)
	} else {
		nc.stationsDB = sdb
		// store.Maintain applies the statistics tiers (netStoreLoop); the
		// registry's own retention must not delete hours before their days
		// were rolled up.
		ropts.StoreMaintained = true
	}
	reg, err := registry.OpenDir(e.dir, ropts)
	if err != nil {
		if sdb != nil {
			_ = sdb.Close()
		}
		return nil, fmt.Errorf("engine: aggregator mode: station registry: %w", err)
	}
	nc.reg = reg
	nc.ctx, nc.cancel = context.WithCancel(context.Background())
	nc.accounts = auth.NewService(reg.Accounts(), reg.Sessions(), AccountOptions(cfg.Aggregator))
	return nc, nil
}

// attachNet switches the tracker to network mode and builds the ingest.
// Called by Start before any frame flows.
func (e *Engine) attachNet(nc *netCore, cfg model.Config) error {
	lo := tracker.LaneOptionsFromConfig(cfg.Aggregator)
	lo.Resolver = nc.reg.Resolver()
	// Lane statistics rows (ant = "s<idx>" / "s<idx>/<ch>") go to
	// stations.db; to airview.db only when stations.db could not be opened
	// (they never mix with the combined slot, ant = "").
	lo.Store = e.st
	if nc.stationsDB != nil {
		lo.Store = nc.stationsDB
	}
	mainDB := e.st
	lo.OnFlightStation = func(rows []tracker.FlightStation) {
		nc.flightStationRows.Add(uint64(len(rows)))
		if fn := e.flightStationSink.Load(); fn != nil {
			(*fn)(rows)
			return
		}
		mainDB.UpsertFlightStation(flightStationRows(rows))
	}
	nc.lanes = tracker.NewLanes(lo)
	e.tr.SetNetwork(nc.reg.Resolver(), nc.lanes)
	io := ingest.OptionsFromConfig(cfg.Aggregator)
	io.Handler = fedHandler{NetHandler: e.tr, fed: &nc.fed}
	io.Resolver = nc.reg.Resolver()
	io.Now = ntp.Now
	io.Logger = e.log.With("component", "ingest")
	nc.mlatBoot = cfg.Aggregator.MLAT
	if cfg.Aggregator.MLAT.Enabled {
		if err := e.newMLAT(nc, cfg, &io); err != nil {
			nc.log.Error("MLAT engine cannot be built; MLAT is off", "err", err)
		}
	}
	if core, err := ingest.New(io); err == nil {
		nc.core, nc.ingest = core, core
		// The host station is an in-process link of its own: no FLOW or
		// traffic mode, never selective.
		nc.hostSess = core.OpenSession(ingest.SessionInfo{Proto: "host", Label: "host station"})
		nc.hostSess.AddStation(model.HostStation)
	} else {
		nc.log.Error("ingest core cannot be built; using the engine's minimal grouper", "err", err)
		if nc.mlat != nil {
			nc.mlat.Close()
			nc.mlat = nil
		}
		nc.group = newGroupIngest(e.tr, nc.reg.Resolver())
		nc.ingest = nc.group
	}
	nc.stations = newStationRuntimes(stationRuntimeOptions{Logger: nc.log})
	e.registerStationComponents(nc)

	ctx, cancel := context.WithTimeout(nc.ctx, 30*time.Second)
	defer cancel()
	st, err := nc.reg.EnsureHost(ctx, hostName(cfg), 0)
	if err != nil {
		return fmt.Errorf("engine: aggregator mode: host station: %w", err)
	}
	nc.host = newHostSink(nc.ingest, st.SID)
	nc.host.setFeeds(cfg.Feeds)
	nc.hostName = st.Name
	e.syncHostStation(ctx, nc, cfg)

	if ready, err := nc.accounts.AdminReady(ctx); err == nil && ready {
		nc.ready.Store(true)
		e.adoptAdmin(ctx, nc, cfg)
	}
	if nc.core != nil {
		nc.wg.Add(1)
		go func() { defer nc.wg.Done(); _ = nc.core.Run(nc.ctx) }()
	}
	e.startMLAT(nc)
	nc.wg.Add(4)
	go func() { defer nc.wg.Done(); e.netStoreLoop(nc) }()
	go func() { defer nc.wg.Done(); nc.reg.Run(nc.ctx) }()
	go func() { defer nc.wg.Done(); nc.accounts.RunJanitor(nc.ctx, 10*time.Minute) }()
	go func() { defer nc.wg.Done(); e.netMaintainLoop(nc) }()
	return nil
}

// closeNet stops the network loops and closes the registry. Called by Stop
// after the runtime (and with it every listener) is down and the tracker
// and lanes are flushed.
func (e *Engine) closeNet() {
	nc := e.net
	if nc == nil {
		return
	}
	nc.cancel()
	nc.wg.Wait()
	if nc.hostSess != nil {
		nc.hostSess.Close(0)
	}
	nc.stations.closeAll()
	// After the lanes' final flush (stopRuntime): the last statistics rows
	// are committed before the file is closed.
	if nc.stationsDB != nil {
		if err := nc.stationsDB.Close(); err != nil {
			e.log.Warn("stations.db close", "err", err)
		}
	}
	if err := nc.reg.Close(); err != nil {
		e.log.Warn("station registry close", "err", err)
	}
}

// flightStationRows converts the lanes' rows into the store's.
func flightStationRows(rows []tracker.FlightStation) []store.FlightStation {
	out := make([]store.FlightStation, len(rows))
	for i, r := range rows {
		out[i] = store.FlightStation{FlightID: r.FlightID, SID: r.SID, ICAO: r.ICAO, First: r.First, Last: r.Last,
			Msgs: r.Msgs, Positions: r.Positions, MaxDistKm: r.MaxDistKm, MinDistKm: r.MinDistKm,
			RSSIMin: r.RSSIMin, RSSIMax: r.RSSIMax, RSSIAvg: r.RSSIAvg}
	}
	return out
}

// netStoreMaintainEvery is the period of the tiered store maintenance in
// aggregator mode (DESIGN §20.2); the first pass runs netStoreMaintainFirst
// after the start.
const (
	netStoreMaintainEvery = time.Hour
	netStoreMaintainFirst = 5 * time.Minute
)

// netStoreLoop runs store.Maintain on airview.db and stations.db hourly
// with the aggregator.storage policy.
func (e *Engine) netStoreLoop(nc *netCore) {
	t := time.NewTimer(netStoreMaintainFirst)
	defer t.Stop()
	for {
		select {
		case <-nc.ctx.Done():
			return
		case <-t.C:
		}
		e.netStoreMaintain(nc)
		t.Reset(netStoreMaintainEvery)
	}
}

// netStoreMaintain is one pass of netStoreLoop.
func (e *Engine) netStoreMaintain(nc *netCore) {
	if !e.clockPlausible(time.Now()) {
		nc.log.Warn("store maintenance skipped: the system clock is not plausible")
		return
	}
	pol := store.TierPolicyFromConfig(e.holder.Get().Aggregator)
	for _, db := range []*store.Store{e.st, nc.stationsDB} {
		if db == nil {
			continue
		}
		if _, err := db.Maintain(nc.ctx, pol); err != nil && nc.ctx.Err() == nil {
			nc.log.Warn("store maintenance failed", "db", db.Kind().String(), "err", err)
		}
	}
}

// StationsDB returns the per-station statistics database (nil in standalone
// mode or when it could not be opened).
func (e *Engine) StationsDB() *store.Store {
	if e.net == nil {
		return nil
	}
	return e.net.stationsDB
}

// hostName is the display name of the host station.
func hostName(cfg model.Config) string {
	if cfg.Receiver.Name != "" {
		return cfg.Receiver.Name
	}
	return hostDefaultName
}

// hostGeo returns the host station's position from the configured receiver
// (or the receiver's own GNSS fix).
func hostGeo(sp StationPosition) (model.StationGeo, bool) {
	if sp.Source == "none" || math.IsNaN(sp.Lat) || math.IsNaN(sp.Lon) {
		return model.StationGeo{}, false
	}
	hae := sp.HeightM
	if h, ok := geoid.MSLToHAE(sp.Lat, sp.Lon, sp.HeightM); ok {
		hae = h
	}
	src := model.LocSrcHost
	if sp.Source == "gps" {
		src = model.LocSrcGNSS
	}
	return model.StationGeo{Lat: sp.Lat, Lon: sp.Lon, HAE: hae, Source: src}, true
}

// syncHostStation writes the host station's name and location to the
// registry when they changed.
func (e *Engine) syncHostStation(ctx context.Context, nc *netCore, cfg model.Config) {
	g, located := hostGeo(stationOf(cfg, e.ReceiverGPS()))
	name := hostName(cfg)
	nc.mu.Lock()
	same := nc.hostLoc == located && nc.hostGeo == g && nc.hostName == name
	nc.mu.Unlock()
	if same {
		if st, ok := nc.reg.Station(model.HostStation); ok && st.Located == located && st.Effective == g {
			return
		}
	}
	status := model.LocUnknown
	if located {
		status = model.LocDeclared
		if g.Source == model.LocSrcGNSS {
			status = model.LocGNSS
		}
	}
	if _, err := nc.reg.Update(ctx, model.HostStation, func(s *registry.Station) error {
		s.Name = name
		s.Located, s.Effective, s.LocStatus = located, g, status
		if located {
			s.Declared = registry.Position{Lat: g.Lat, Lon: g.Lon, Alt: g.HAE, Valid: true}
			s.DeclaredAltRef = model.AltRefHAE
		}
		s.Anchor = located // anchor-eligible; the MLAT engine decides on use
		return nil
	}); err != nil {
		nc.log.Warn("host station update", "err", err)
		return
	}
	nc.mu.Lock()
	nc.hostLoc, nc.hostGeo, nc.hostName = located, g, name
	nc.mu.Unlock()
}

// adoptAdmin makes the first admin the owner of the host station.
func (e *Engine) adoptAdmin(ctx context.Context, nc *netCore, cfg model.Config) {
	admins, err := nc.accounts.Accounts().ListRole(ctx, auth.RoleAdmin)
	if err != nil {
		nc.log.Warn("listing admins", "err", err)
		return
	}
	for _, a := range admins {
		if a.Disabled {
			continue
		}
		if _, err := nc.reg.EnsureHost(ctx, hostName(cfg), a.ID); err != nil {
			nc.log.Warn("host station owner", "err", err)
			return
		}
		nc.adminID.Store(a.ID)
		return
	}
}

// netMaintainLoop polls for the admin account (the listener gate), keeps
// the host station's owner, location and last-seen time current and
// retires idle per-station runtimes.
func (e *Engine) netMaintainLoop(nc *netCore) {
	every := 2 * time.Second
	t := time.NewTimer(every)
	defer t.Stop()
	for {
		select {
		case <-nc.ctx.Done():
			return
		case <-t.C:
		}
		e.netMaintain(nc)
		if nc.ready.Load() {
			every = netMaintainEvery
		}
		t.Reset(every)
	}
}

// netMaintain is one pass of netMaintainLoop.
func (e *Engine) netMaintain(nc *netCore) {
	ctx, cancel := context.WithTimeout(nc.ctx, 20*time.Second)
	defer cancel()
	cfg := e.holder.Get()
	e.pruneLocated(ctx)
	if !nc.ready.Load() {
		if ready, err := nc.accounts.AdminReady(ctx); err == nil && ready {
			nc.ready.Store(true)
			nc.log.Info("admin account ready: feeder listeners accept connections")
			e.adoptAdmin(ctx, nc, cfg)
		}
	} else if nc.adminID.Load() == 0 {
		e.adoptAdmin(ctx, nc, cfg)
	}
	e.syncHostStation(ctx, nc, e.EffectiveConfig())

	n := nc.host.frames.Load()
	nc.mu.Lock()
	seen := n != nc.hostSeen
	nc.hostSeen = n
	nc.mu.Unlock()
	if seen {
		nc.reg.Seen(model.HostStation, time.Now())
	}
	if nc.lanes != nil {
		nc.stations.observe(nc.lanes.List(), time.Now())
	}
}

// ---------------------------------------------------------------------------
// Listeners (part of the runtime)
// ---------------------------------------------------------------------------

// listenAddr joins the listen host and a port: the -listen-host flag
// ($AIRVIEW_LISTEN_HOST) first, then aggregator.listenAddr ("" = every
// interface).
func (e *Engine) listenAddr(port int) string {
	host := e.opts.ListenHost
	if host == "" && e.holder != nil {
		host = e.holder.Get().Aggregator.ListenAddr
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// syncNetListeners makes the feeder listeners match cfg. Caller holds opMu;
// rt is running.
func (e *Engine) syncNetListeners(rt *runtimeState, cfg model.Config) {
	nc := e.net
	if nc == nil || rt == nil {
		return
	}
	nc.reg.SetPolicy(registry.PolicyFromConfig(cfg.Aggregator))
	if nc.core != nil {
		nc.core.SetLimits(cfg.Aggregator.Limits)
	}
	e.syncBeast(rt, nc, cfg)
	e.syncAVF(rt, nc, cfg)
	e.syncSBSIn(rt, nc, cfg)
	e.syncMLATC(rt, nc, cfg)
	e.syncFederation(rt, nc, cfg)
}

func (e *Engine) syncBeast(rt *runtimeState, nc *netCore, cfg model.Config) {
	port := cfg.Aggregator.BeastPort
	addr := ""
	if port > 0 {
		addr = e.listenAddr(port)
	}
	nc.mu.Lock()
	cur, curAt := nc.beast, nc.beastAt
	nc.mu.Unlock()
	if cur != nil && curAt == addr {
		cur.SetPolicy(beastin.PolicyFromConfig(cfg.Aggregator))
		return
	}
	if cur != nil {
		nc.mu.Lock()
		nc.beast, nc.beastAt = nil, ""
		nc.mu.Unlock()
		_ = cur.Close()
		nc.log.Info("Beast listener stopped")
	}
	if addr == "" {
		nc.mu.Lock()
		nc.beastErr = ""
		nc.mu.Unlock()
		return
	}
	opt := beastin.OptionsFromConfig(cfg.Aggregator)
	opt.Addr = addr
	opt.ServerKey = nc.reg.IngestKey()
	opt.Now = ntp.Now
	opt.Logger = e.log.With("component", "beastin")
	opt.Gate = nc.ready.Load
	if nc.core != nil {
		opt.Budget = nc.core.Budget()
	}
	breg := nc.reg.Beast()
	srv, err := beastin.New(opt, breg, nc.ingest)
	var ln net.Listener
	if err == nil {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		e.listenerFailed(nc, &nc.beastErr, "Beast", port, err)
		return
	}
	nc.mu.Lock()
	nc.beast, nc.beastAt, nc.beastReg, nc.beastErr = srv, addr, breg, ""
	nc.mu.Unlock()
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		if err := srv.Serve(rt.ctx, ln); err != nil && rt.ctx.Err() == nil {
			nc.log.Error("Beast listener stopped", "err", err)
		}
	}()
	nc.log.Info("Beast listener (compat, map and stats only)", "addr", ln.Addr().String(), "gated", !nc.ready.Load())
}

func (e *Engine) syncSBSIn(rt *runtimeState, nc *netCore, cfg model.Config) {
	port := cfg.Aggregator.SBSInLocalPort
	addr := ""
	if port > 0 {
		// Never on a public interface (DESIGN §5.3).
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	nc.mu.Lock()
	cur, curAt := nc.sbs, nc.sbsAt
	nc.mu.Unlock()
	if cur != nil && curAt == addr {
		return
	}
	if cur != nil {
		nc.mu.Lock()
		nc.sbs, nc.sbsAt = nil, ""
		nc.mu.Unlock()
		cur.Close()
		nc.log.Info("SBS-in bridge stopped")
	}
	if addr == "" {
		nc.mu.Lock()
		nc.sbsErr = ""
		nc.mu.Unlock()
		return
	}
	b, err := startSBSBridge(rt.ctx, addr, nc.ingest, nc.log)
	if err != nil {
		e.listenerFailed(nc, &nc.sbsErr, "SBS-in bridge", port, err)
		return
	}
	nc.mu.Lock()
	nc.sbs, nc.sbsAt, nc.sbsErr = b, addr, ""
	nc.mu.Unlock()
	nc.log.Info("SBS-in bridge for an external mlat-server", "addr", b.Addr())
}

// listenerFailed records and announces a listener start error once.
func (e *Engine) listenerFailed(nc *netCore, slot *string, what string, port int, err error) {
	msg := err.Error()
	nc.mu.Lock()
	first := *slot != msg
	*slot = msg
	nc.mu.Unlock()
	if first {
		nc.log.Warn(what+" listener could not be started", "port", port, "err", err)
		e.Notify("warn", fmt.Sprintf("%s port %d could not be started: %v", what, port, err))
	}
}

// flushIngest closes the ingest's open groups (shutdown, after the
// listeners and feeds stopped).
func (e *Engine) flushIngest() {
	if nc := e.net; nc != nil && nc.core != nil {
		nc.core.Flush()
	}
}

// stopNetListeners closes every listener and waits until their last frames
// reached the ingest. Caller holds opMu.
func (e *Engine) stopNetListeners() {
	nc := e.net
	if nc == nil {
		return
	}
	nc.mu.Lock()
	b, s, a, m := nc.beast, nc.sbs, nc.avf, nc.mlatc
	nc.beast, nc.beastAt, nc.sbs, nc.sbsAt, nc.avf = nil, "", nil, "", nil
	nc.mlatc, nc.mlatcAt = nil, ""
	nc.mu.Unlock()
	nc.closeFederation()
	if m != nil {
		_ = m.Close()
	}
	if a != nil {
		_ = a.srv.Close()
	}
	if b != nil {
		_ = b.Close()
	}
	if s != nil {
		s.Close()
	}
}

// ---------------------------------------------------------------------------
// Accessors
// ---------------------------------------------------------------------------

// Aggregator reports whether the engine runs in aggregator mode (fixed at
// Start).
func (e *Engine) Aggregator() bool { return e.net != nil }

// Registry returns the station registry (nil in standalone mode).
func (e *Engine) Registry() *registry.Registry {
	if e.net == nil {
		return nil
	}
	return e.net.reg
}

// Accounts returns the aggregator account service (nil in standalone mode).
func (e *Engine) Accounts() *auth.Service {
	if e.net == nil {
		return nil
	}
	return e.net.accounts
}

// Lanes returns the per-station lanes (nil in standalone mode).
func (e *Engine) Lanes() *tracker.Lanes {
	if e.net == nil {
		return nil
	}
	return e.net.lanes
}

// NetIngest returns the network ingest (nil in standalone mode). The
// SBS-in bridge, the listeners and the host station all feed it.
func (e *Engine) NetIngest() NetIngest {
	if e.net == nil {
		return nil
	}
	return e.net.ingest
}

// BeastIngest returns the running compat Beast listener (nil when off or in
// standalone mode). Its Links() contain private data: admin scope only.
func (e *Engine) BeastIngest() *beastin.Server {
	if e.net == nil {
		return nil
	}
	e.net.mu.Lock()
	defer e.net.mu.Unlock()
	return e.net.beast
}

// Stations returns the per-station runtimes (nil in standalone mode).
func (e *Engine) Stations() *StationRuntimes {
	if e.net == nil {
		return nil
	}
	return e.net.stations
}

// AdminReady reports whether the aggregator's admin account exists (always
// true in standalone mode, where the feeder listeners do not exist).
func (e *Engine) AdminReady() bool {
	return e.net == nil || e.net.ready.Load()
}

// HostSID returns the public ID of the host station ("" in standalone
// mode).
func (e *Engine) HostSID() string {
	if e.net == nil || e.net.host == nil {
		return ""
	}
	return e.net.host.sid
}

// SetFlightStationSink installs the writer of the lanes' flight_station rows
// (internal/store, DESIGN §6.4). nil drops the rows (the default until
// internal/store has the table). fn is called without locks, never
// concurrently with itself; rows are reused after it returns.
func (e *Engine) SetFlightStationSink(fn func(rows []tracker.FlightStation)) {
	if fn == nil {
		e.flightStationSink.Store(nil)
		return
	}
	e.flightStationSink.Store(&fn)
}

// NetworkStatus returns the operator view of the aggregator runtime.
func (e *Engine) NetworkStatus() NetworkStatus {
	nc := e.net
	if nc == nil {
		return NetworkStatus{}
	}
	cfg := e.EffectiveConfig()
	s := NetworkStatus{
		Aggregator: true,
		AdminReady: nc.ready.Load(),
		Stations:   nc.reg.Count(),
		Runtimes:   nc.stations.Len(),
		Lock:       e.tr.LockStats(),

		FlightStationRows: nc.flightStationRows.Load(),
	}
	if nc.host != nil {
		s.HostSID = nc.host.sid
		s.HostFrames = nc.host.frames.Load()
	}
	if nc.core != nil {
		cs := nc.core.Stats()
		s.Core = &cs
	}
	if nc.group != nil {
		gs := nc.group.Stats()
		s.Fallback = &gs
	}
	if nc.lanes != nil {
		s.Lanes = len(nc.lanes.List())
	}
	nc.mu.Lock()
	s.Beast = ListenerStatus{Port: cfg.Aggregator.BeastPort, Listening: nc.beast != nil, Addr: nc.beastAt, Error: nc.beastErr}
	s.SBSIn = ListenerStatus{Port: cfg.Aggregator.SBSInLocalPort, Listening: nc.sbs != nil, Addr: nc.sbsAt, Error: nc.sbsErr}
	s.AVF = ListenerStatus{Port: cfg.Aggregator.AVFPort, Error: nc.avfErr}
	if nc.avf != nil {
		s.AVF.Listening, s.AVF.Addr, s.AVFPin = true, nc.avf.at, nc.avf.pin
		as := nc.avf.srv.Stats()
		s.AVFStats = &as
	}
	beast := nc.beast
	nc.mu.Unlock()
	if beast != nil {
		bs := beast.Stats()
		s.BeastStats = &bs
	}
	gated := "closed until the admin account exists"
	if !s.AdminReady {
		if s.Beast.Listening {
			s.Beast.Note = gated
		}
	}
	if !s.AdminReady && s.AVF.Listening {
		s.AVF.Note = gated
	}
	s.MLATC = nc.mlatcStatus(cfg)
	if !s.AdminReady && s.MLATC.Listening {
		s.MLATC.Note = gated
	}
	if m := e.MLATCIngest(); m != nil {
		ms := m.Stats()
		s.MLATCStats = &ms
	}
	if nc.mlat != nil {
		st := nc.mlat.Stats()
		s.MLAT = &st
	}
	if nc.trust != nil {
		ts := nc.trust.Stats()
		s.Trust = &ts
	}
	return s
}

// The lanes write their rows through tracker.LaneStore (stations.db, see
// attachNet).
var _ tracker.LaneStore = (*store.Store)(nil)
