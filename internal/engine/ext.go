package engine

import (
	"context"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/acas"
	"airview/internal/alerts"
	"airview/internal/flowlearn"
	"airview/internal/met"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/navdata"
	"airview/internal/rfstat"
	"airview/internal/routes"
	"airview/internal/terrain"
	"airview/internal/trajectory"
)

// Wiring of the trajectory filter, RF statistics, terrain analysis,
// navigation data and alert engine (see the WIRING.md of each package).
//
// Long-lived objects are built once in Start (buildExt) and survive factory
// resets; their goroutines run in the runtime (startExt). The 1 Hz live
// enrichment of the tracker snapshot (terrain visibility, closest approach,
// route, alert highlights) runs in enrich, which the tracker calls once per
// newly built snapshot.

// maxCoastHard is the trajectory manager's MaxCoast: the upper bound of
// config.prediction.coastSec.
const maxCoastHard = 120 * time.Second

type extState struct {
	tj   *trajectory.Manager
	terr *terrain.Service
	nd   *navdata.NavData
	al   *alerts.Engine

	rf       atomic.Pointer[rfstat.Stats]
	rfMu     sync.Mutex // guards rfCancel, rfKey, rfDone
	rfCancel context.CancelFunc
	rfDone   chan struct{}
	rfKey    string

	region atomic.Pointer[navdata.Region]
	wind   windProvider

	enrichMu sync.Mutex
	cpa      map[uint32]cpaEntry
	cpaSweep int64

	rt       *routes.Service
	acas     *acas.Recorder
	flow     *flowlearn.Learner
	routeCfg atomic.Pointer[model.RoutesConfig]

	regionKick  chan struct{}
	airportKick chan struct{} // wakes airportLoop (airports.go)
}

type cpaEntry struct {
	km, inS float64
	at      int64 // data time ms of the computation
	ok      bool
}

const (
	cpaEvery  = 5000 // ms between closest-approach recomputations per aircraft
	cpaBudget = 80   // recomputations per snapshot
)

// buildExt creates the extension components. Caller holds opMu (Start).
func (e *Engine) buildExt(cfg model.Config) {
	x := &extState{
		cpa:         map[uint32]cpaEntry{},
		regionKick:  make(chan struct{}, 1),
		airportKick: make(chan struct{}, 1),
	}
	e.ext = x
	// The filter runs on the host clock: the tracker stamps every observation
	// (GPS-timed Radarcape frames converted with the NTP offset) and asks for
	// predictions on it, and so do the API handlers (time.Now).
	x.tj = trajectory.NewManager(trajectory.Config{MaxCoast: maxCoastHard, Now: time.Now})

	dir := filepath.Join(e.dir, "terrain")
	// Every finished terrain job (successful or not) leaves its large
	// working set (elevation tiles, profiles, panoramas) to the GC: hand it
	// back to the OS so a Raspberry Pi's resident set shrinks again.
	so := terrain.ServiceOptions{Dir: dir, Logger: e.log, OnJobDone: func(terrain.JobStatus) { e.releaseMemory("terrain analysis") }}
	if e.opts.Offline {
		// No downloads: a synthetic hill near the site keeps every terrain
		// endpoint working (tests, air-gapped demo).
		rx := e.eff.Load().cfg.Receiver
		so.Source = terrain.ConeSource{Lat: rx.Lat + 0.25, Lon: rx.Lon + 0.35, PeakM: 900, BaseM: rx.GroundElevM, RadiusKm: 25}
	} else {
		so.Provider = terrain.NewProvider(terrain.ProviderOptions{
			Dir: dir, MaxDiskMB: 1024, MemTiles: 64, Logger: e.log,
			UserAgent: model.AppName + "/" + e.opts.Version,
		})
	}
	x.terr = terrain.NewService(so)
	x.nd = navdata.New(e.st, e.navOptions(cfg))
	rc := cfg.NavData.Routes
	x.routeCfg.Store(&rc)
	x.rt = routes.New(routes.Options{
		Store: e.st, Nav: x.nd, Logger: e.log,
		Config:        func() model.RoutesConfig { return *x.routeCfg.Load() },
		OverridesFile: filepath.Join(e.dir, "routes.overrides.csv"),
	})
	x.flow = flowlearn.New()
	x.acas = acas.New(e.st, e.log)
	e.tr.SetACASHook(x.acas.Observe)
	x.al = alerts.New(alerts.Options{Store: e.st, Logger: e.log, UserAgent: model.AppName + "/" + e.opts.Version})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := x.al.Load(ctx); err != nil {
		e.log.Warn("alerts: loading rules", "err", err)
	}
	cancel()
	x.al.SetWebhooks(cfg.Alerts.Webhooks)

	e.tr.SetEnricher(e.enrich)
	e.tr.SetNewAirframeHook(func(icao uint32) {
		if c := e.EffectiveConfig(); c.Alerts.Enabled {
			x.al.NoteNewAirframe(icao, time.Now())
		}
	})
	e.applyExt(nil, e.eff.Load())
}

func (e *Engine) navOptions(cfg model.Config) navdata.Options {
	return navdata.Options{
		UserAgent: model.AppName + "/" + e.opts.Version,
		Logger:    e.log,
		Routes: navdata.RouteOptions{
			Online:     cfg.NavData.Routes.Enabled && cfg.NavData.Routes.Online,
			DailyLimit: cfg.NavData.Routes.DailyCap,
			Contact:    routeContact(cfg.NavData.Routes.Contact),
		},
		Photos: navdata.PhotoOptions{Enabled: cfg.NavData.Photos, Contact: cfg.NavData.PhotoContact},
	}
}

// routeContact is the contact in the User-Agent of online route look-ups:
// the configured one, else the project URL (never a personal address).
func routeContact(c string) string {
	if c = strings.TrimSpace(c); c != "" {
		return c
	}
	return model.ProjectURL
}

// FlowLearner returns the learned traffic-flow predictor.
func (e *Engine) FlowLearner() *flowlearn.Learner {
	if e.ext == nil {
		return nil
	}
	return e.ext.flow
}

// ACAS returns the resolution advisory recorder.
func (e *Engine) ACAS() *acas.Recorder {
	if e.ext == nil {
		return nil
	}
	return e.ext.acas
}

// Routes returns the route service.
func (e *Engine) Routes() *routes.Service {
	if e.ext == nil {
		return nil
	}
	return e.ext.rt
}

// terrainRequest builds the terrain analysis request of an effective configuration.
func terrainRequest(cfg model.Config) terrain.Request {
	req := terrain.RequestFromConfig(cfg.Receiver, cfg.Antennas, cfg.Terrain.KFactor)
	req.RadiusKm = float64(cfg.Terrain.RadiusKm)
	req.Zoom = cfg.Terrain.Zoom
	return req
}

// rfKeyOf identifies what an rfstat.Stats instance depends on.
func rfKeyOf(cfg model.Config) string {
	var b strings.Builder
	for _, a := range cfg.Antennas {
		b.WriteString(a.ID)
		b.WriteByte(',')
	}
	if located(cfg.Receiver) {
		b.WriteString(strconv.FormatFloat(cfg.Receiver.Lat, 'f', 4, 64))
		b.WriteByte('/')
		b.WriteString(strconv.FormatFloat(cfg.Receiver.Lon, 'f', 4, 64))
	}
	return b.String()
}

// applyExt applies the effective configuration step prev -> cur to the
// extension components (prev nil = initial). Caller holds opMu.
func (e *Engine) applyExt(prev, cur *effective) {
	x := e.ext
	if x == nil || cur == nil {
		return
	}
	c := cur.cfg

	// Prediction.
	if c.Prediction.Enabled {
		e.tr.SetTrajectory(x.tj, c.Prediction.CoastSec)
	} else {
		e.tr.SetTrajectory(nil, 0)
	}
	if c.Prediction.Enabled && c.Prediction.LearnedTurns && x.flow != nil {
		e.tr.SetLearnedTurn(x.flow.Blend)
	} else {
		e.tr.SetLearnedTurn(nil)
	}
	if located(c.Receiver) {
		x.tj.SetReceiver(c.Receiver.Lat, c.Receiver.Lon)
	} else {
		x.tj.ClearReceiver()
	}
	x.tj.SetWind(&x.wind)

	// Navigation data.
	x.nd.SetOptions(e.navOptions(c))
	rc := c.NavData.Routes
	x.routeCfg.Store(&rc)
	if prev == nil || !located(prev.cfg.Receiver) || prev.cfg.Receiver.Lat != c.Receiver.Lat || prev.cfg.Receiver.Lon != c.Receiver.Lon {
		e.kick(x.regionKick)
	}

	// Alerts.
	x.al.SetWebhooks(c.Alerts.Webhooks)

	// RF statistics: a new instance when the antennas or the site change.
	e.syncRF(c)

	// Terrain: Submit is a no-op while the request key is unchanged.
	if c.Terrain.Enabled && c.Terrain.AutoPrefetch && located(c.Receiver) {
		x.terr.Submit(terrainRequest(c), false)
	}
}

// syncRF makes the RF statistics instance match cfg. Caller holds opMu.
func (e *Engine) syncRF(c model.Config) {
	x := e.ext
	x.rfMu.Lock()
	defer x.rfMu.Unlock()
	want := c.RFStat.Enabled
	key := rfKeyOf(c)
	if want && x.rf.Load() != nil && key == x.rfKey {
		return
	}
	// Stop the old instance (its Run flushes queued hourly summaries).
	if x.rfCancel != nil {
		x.rfCancel()
		<-x.rfDone
		x.rfCancel, x.rfDone = nil, nil
	}
	if !want {
		x.rf.Store(nil)
		e.tr.SetRFStats(nil)
		x.rfKey = ""
		return
	}
	ids := make([]string, 0, len(c.Antennas))
	for _, a := range c.Antennas {
		ids = append(ids, a.ID)
	}
	rc := rfstat.Config{Antennas: ids, Logger: e.log}
	if located(c.Receiver) {
		rc.StationLat, rc.StationLon = c.Receiver.Lat, c.Receiver.Lon
	}
	rs := rfstat.New(rc)
	x.rf.Store(rs)
	x.rfKey = key
	e.tr.SetRFStats(rs)
	if rt := e.rt; rt != nil {
		e.startRFRun(rt, rs)
	}
}

// startRFRun runs rs until the runtime stops or syncRF replaces it. Caller
// holds rfMu.
func (e *Engine) startRFRun(rt *runtimeState, rs *rfstat.Stats) {
	x := e.ext
	ctx, cancel := context.WithCancel(rt.ctx)
	done := make(chan struct{})
	x.rfCancel, x.rfDone = cancel, done
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		defer close(done)
		_ = rs.Run(ctx, e.st)
	}()
}

// startExt launches the extension goroutines. Caller holds opMu (startRuntime).
func (e *Engine) startExt(rt *runtimeState, run func(string, func(context.Context) error)) {
	x := e.ext
	if x == nil {
		return
	}
	run("trajectory", x.tj.Run)
	run("terrain", x.terr.Run)
	run("alerts", func(ctx context.Context) error { x.al.Run(ctx); return nil })
	run("live", func(ctx context.Context) error { e.liveLoop(ctx); return nil })
	run("routes", func(ctx context.Context) error { x.rt.Run(ctx); return nil })
	run("acas", func(ctx context.Context) error { x.acas.Run(ctx, nil); return nil })
	run("navdata", func(ctx context.Context) error { e.navLoop(ctx); return nil })
	run("airports", func(ctx context.Context) error { e.airportLoop(ctx); return nil })
	x.rfMu.Lock()
	if rs := x.rf.Load(); rs != nil {
		e.startRFRun(rt, rs)
	}
	x.rfMu.Unlock()
	e.kick(x.regionKick)
}

// stopExt is called after the runtime goroutines ended. Caller holds opMu.
func (e *Engine) stopExt() {
	x := e.ext
	if x == nil {
		return
	}
	x.rfMu.Lock()
	x.rfCancel, x.rfDone = nil, nil
	x.rfMu.Unlock()
}

// resetExt forgets the state that refers to the wiped database (factory
// reset). Caller holds opMu; the runtime is stopped.
func (e *Engine) resetExt() {
	x := e.ext
	if x == nil {
		return
	}
	x.nd.CancelImport()
	x.al.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := x.al.Load(ctx); err != nil {
		e.log.Warn("alerts: reloading rules", "err", err)
	}
	cancel()
	x.region.Store(nil)
	x.tj.SetRunways(nil)
	x.rt.Invalidate("")
	x.enrichMu.Lock()
	x.cpa = map[uint32]cpaEntry{}
	x.enrichMu.Unlock()
	x.rfMu.Lock()
	x.rf.Store(nil)
	x.rfKey = ""
	x.rfMu.Unlock()
	e.tr.SetRFStats(nil)
}

// liveLoop drives the 1 Hz work: the snapshot (and with it the enrichment
// and alert evaluation) even when no client is connected, the alert status
// checks every 5 s and the wind table of the tracking filter every minute.
func (e *Engine) liveLoop(ctx context.Context) {
	x := e.ext
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	n := 0
	alerts, cancel := x.al.Subscribe(64)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case al := <-alerts:
			e.alertNotice(al)
		case <-tick.C:
			n++
			_ = e.tr.Snapshot()
			cfg := e.EffectiveConfig()
			if n%5 == 0 && cfg.Alerts.Enabled {
				x.al.CheckStatus(e.Status(), time.Now())
			}
			if n%60 == 1 {
				x.wind.update(e.met.Current(time.Time{}))
			}
		}
	}
}

// alertNotice publishes an alert as a notice (SSE "notice", kind "alert")
// when its rule asks for a notification. Emergencies, military catches and
// records are already announced by the tracker's own notices, so the
// built-in rules for those are not repeated.
func (e *Engine) alertNotice(al alerts.Alert) {
	if !al.Notify {
		return
	}
	switch al.RuleID {
	case "builtin-emergency", "builtin-military", "builtin-record":
		return
	}
	level := "info"
	switch al.Severity {
	case alerts.SeverityCritical:
		level = "alert"
	case alerts.SeverityWarning:
		level = "warn"
	}
	e.notices.publish(Notice{Level: level, Text: al.Text, T: al.T, Kind: "alert", ICAO: al.ICAO})
}

// onTrackerNotice forwards tracker notices to the UI and turns new records
// into alert system events.
func (e *Engine) onTrackerNotice(n Notice) {
	e.notices.publish(n)
	x := e.ext
	if x == nil || n.Kind != "record" || !e.EffectiveConfig().Alerts.Enabled {
		return
	}
	ev := alerts.SystemEvent{Kind: alerts.SysRecord, Key: n.Key, Text: strings.TrimPrefix(n.Text, "New record: "), Value: n.Value}
	if n.ICAO != "" {
		if key, err := modes.ParseICAO(n.ICAO); err == nil {
			if ac, ok := e.tr.Aircraft(key); ok {
				ev.Aircraft = &ac
			}
		}
	}
	x.al.HandleSystem(ev)
}

// enrich adds the 1 Hz per-aircraft extras to a new live snapshot and runs
// the alert rules on it (called by the tracker, without its lock).
func (e *Engine) enrich(s *model.LiveSnapshot) {
	x := e.ext
	eff := e.eff.Load()
	if x == nil || eff == nil {
		return
	}
	cfg := &eff.cfg
	now := time.UnixMilli(s.Now)
	var an *terrain.Analysis
	if cfg.Terrain.Enabled {
		an = x.terr.Analysis()
	}
	var tj *trajectory.Manager
	if cfg.Prediction.Enabled && located(cfg.Receiver) {
		tj = x.tj
	}

	if marks := x.acas.LiveMarks(s.Now); len(marks) > 0 {
		for i := range s.Aircraft {
			if key, err := modes.ParseICAO(s.Aircraft[i].ICAO); err == nil {
				if m, ok := marks[key]; ok {
					s.Aircraft[i].ACAS = &model.ACASMark{Advisory: m.Advisory, Sense: m.Sense, Since: m.Since}
				}
			}
		}
	}

	x.enrichMu.Lock()
	budget := cpaBudget
	for i := range s.Aircraft {
		a := &s.Aircraft[i]
		if a.Lat == nil || a.Lon == nil {
			if a.Callsign != "" {
				a.Route = x.rt.Live(a, now)
			}
			continue
		}
		if an != nil {
			alt := math.NaN()
			switch {
			case a.AltGeom != nil:
				alt = *a.AltGeom
			case a.AltBaro != nil:
				alt = *a.AltBaro
			case a.OnGround != nil && *a.OnGround:
				alt = cfg.Receiver.GroundElevM / 0.3048
			}
			if !math.IsNaN(alt) {
				if v := an.Visible("", *a.Lat, *a.Lon, alt); v.Known && !math.IsNaN(v.MarginDeg) && !math.IsInf(v.MarginDeg, 0) {
					vis, m := v.Visible, math.Round(v.MarginDeg*100)/100
					a.TerrainVisible, a.LOSMarginDeg = &vis, &m
				}
			}
		}
		if tj != nil {
			if key, err := modes.ParseICAO(a.ICAO); err == nil {
				c, ok := x.cpa[key]
				if (!ok || s.Now-c.at >= cpaEvery || s.Now < c.at) && budget > 0 {
					budget--
					c = cpaEntry{at: s.Now}
					if in, ok := tj.Intent(key, now); ok && in.CPA != nil && finite(in.CPA.DistKm) && finite(in.CPA.InSec) {
						c.km, c.inS, c.ok = in.CPA.DistKm, in.CPA.InSec, true
					}
					x.cpa[key] = c
				}
				if c.ok {
					in := math.Max(0, c.inS-float64(s.Now-c.at)/1000)
					km, sec := math.Round(c.km*100)/100, math.Round(in)
					a.CPAKm, a.CPAInS = &km, &sec
				}
			}
		}
		if a.Callsign != "" {
			a.Route = x.rt.Live(a, now)
		}
		// learn the traffic flow from fresh real positions
		if !a.Predicted && a.PosAge != nil && *a.PosAge < 2 && (a.OnGround == nil || !*a.OnGround) && a.Track != nil && a.GS != nil {
			if key, err := modes.ParseICAO(a.ICAO); err == nil {
				alt := math.NaN()
				switch {
				case a.AltBaro != nil:
					alt = *a.AltBaro
				case a.AltGeom != nil:
					alt = *a.AltGeom
				}
				x.flow.Observe(key, s.Now-int64(*a.PosAge*1000), *a.Lat, *a.Lon, alt, *a.Track, *a.GS)
			}
		}
	}
	if s.Now-x.cpaSweep > 60_000 || s.Now < x.cpaSweep {
		x.cpaSweep = s.Now
		for k, c := range x.cpa {
			if s.Now-c.at > 60_000 || s.Now < c.at {
				delete(x.cpa, k)
			}
		}
	}
	x.enrichMu.Unlock()

	if !cfg.Alerts.Enabled {
		return
	}
	x.al.Evaluate(s.Aircraft, now)
	hls := x.al.Highlights(now)
	if len(hls) == 0 {
		return
	}
	marks := make(map[string]*model.AlertMark, len(hls))
	names := map[string]string{}
	for _, h := range hls {
		name, ok := names[h.RuleID]
		if !ok {
			if r, found := x.al.Rule(h.RuleID); found {
				name = r.Name
			}
			names[h.RuleID] = name
		}
		marks[h.ICAO] = &model.AlertMark{RuleID: h.RuleID, Color: h.Color, Label: name, Severity: h.Severity}
	}
	for i := range s.Aircraft {
		a := &s.Aircraft[i]
		if m := marks[strings.TrimPrefix(a.ICAO, "~")]; m != nil && !strings.HasPrefix(a.ICAO, "~") {
			a.Highlight = m
		}
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// ------------------------------------------------------------- navdata

// navLoop (re)loads the airport region around the receiver at start, after
// a receiver move and after every finished import, and clears the route
// cache after an import.
func (e *Engine) navLoop(ctx context.Context) {
	x := e.ext
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	wasRunning := false
	var auto navRefresh
	for {
		select {
		case <-ctx.Done():
			return
		case <-x.regionKick:
			e.loadRegion(ctx)
		case <-tick.C:
			st := x.nd.ImportStatus()
			if wasRunning && !st.Running {
				x.rt.RecheckStanding()
				byAuto := auto.running
				auto.done(st.Error != "", time.Now())
				e.loadRegion(ctx)
				switch {
				case st.Error != "" && byAuto:
					// an automatic check: no notice (a station without internet
					// would get one every retry); the log and Settings show it
					e.log.Warn("navdata: automatic reference data update failed", "err", st.Error, "retry", auto.next.Format(time.RFC3339))
				case st.Error != "":
					e.Notify("warn", "Reference data import failed: "+st.Error)
				case byAuto:
					e.log.Info("navdata: reference data checked for updates")
					e.airportsAfterImport(ctx)
				default:
					e.Notify("info", "Reference data import finished.")
					e.airportsAfterImport(ctx)
				}
			}
			wasRunning = st.Running
			if !st.Running {
				e.autoRefresh(ctx, &auto, time.Now())
				wasRunning = x.nd.ImportStatus().Running
			}
		}
	}
}

func (e *Engine) loadRegion(ctx context.Context) {
	x := e.ext
	rx := e.EffectiveConfig().Receiver
	if !located(rx) {
		x.region.Store(nil)
		x.tj.SetRunways(nil)
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	reg, err := x.nd.LoadRegion(rctx, rx.Lat, rx.Lon, 800, navdata.SizeSmall)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("navdata: loading the airport region", "err", err)
		}
		return
	}
	x.region.Store(reg)
	if reg.Len() > 0 {
		x.tj.SetRunways(runwayAdapter{reg})
		e.kick(x.airportKick)
	} else {
		x.tj.SetRunways(nil)
	}
	e.log.Debug("navdata: airport region loaded", "airports", reg.Len())
}

// runwayAdapter serves the region's runway ends to the trajectory filter.
type runwayAdapter struct{ r *navdata.Region }

func (a runwayAdapter) RunwaysNear(lat, lon, radiusKm float64, dst []trajectory.Runway) []trajectory.Runway {
	var buf [32]navdata.RunwayEndInfo
	for _, e := range a.r.RunwaysNear(lat, lon, radiusKm, buf[:0]) {
		dst = append(dst, trajectory.Runway{Airport: e.AirportCode, Ident: e.Runway, Lat: e.Lat, Lon: e.Lon,
			HeadingDeg: e.HeadingT, ElevationFt: e.ElevFt})
	}
	return dst
}

// --------------------------------------------------------------- wind

// windProvider serves the aircraft-derived wind profile (internal/met) to
// the trajectory filter: a table refreshed once a minute, read lock-free.
type windProvider struct {
	tab atomic.Pointer[[]windLevel]
}

type windLevel struct{ alt, u, v float64 } // ft, kt east, kt north (direction of movement)

// update rebuilds the table from a profile (levels ascending).
func (w *windProvider) update(p met.Profile) {
	tab := make([]windLevel, 0, len(p.Levels))
	for _, l := range p.Levels {
		if l.WindDir == nil || l.WindKt == nil || l.NWind < 3 || !finite(l.AltFt) {
			continue
		}
		s, c := math.Sincos(*l.WindDir * math.Pi / 180)
		// Wind blows FROM WindDir: the air moves towards WindDir+180.
		tab = append(tab, windLevel{alt: l.AltFt, u: -*l.WindKt * s, v: -*l.WindKt * c})
	}
	if len(tab) == 0 {
		w.tab.Store(nil)
		return
	}
	w.tab.Store(&tab)
}

// WindAt implements trajectory.WindProvider.
func (w *windProvider) WindAt(lat, lon, altFt float64) (dirFrom, kt float64, ok bool) {
	tp := w.tab.Load()
	if tp == nil || len(*tp) == 0 || !finite(altFt) {
		return 0, 0, false
	}
	t := *tp
	var u, v float64
	switch {
	case altFt <= t[0].alt:
		if t[0].alt-altFt > 3000 {
			return 0, 0, false
		}
		u, v = t[0].u, t[0].v
	case altFt >= t[len(t)-1].alt:
		if altFt-t[len(t)-1].alt > 3000 {
			return 0, 0, false
		}
		u, v = t[len(t)-1].u, t[len(t)-1].v
	default:
		i := 1
		for i < len(t) && t[i].alt < altFt {
			i++
		}
		a, b := t[i-1], t[i]
		if b.alt-a.alt > 6000 {
			return 0, 0, false
		}
		f := (altFt - a.alt) / (b.alt - a.alt)
		u, v = a.u+f*(b.u-a.u), a.v+f*(b.v-a.v)
	}
	kt = math.Hypot(u, v)
	dirFrom = math.Mod(math.Atan2(-u, -v)*180/math.Pi+360, 360)
	return dirFrom, kt, true
}

// ---------------------------------------------------------- accessors

// Trajectory returns the tracking filter (nil before Start).
func (e *Engine) Trajectory() *trajectory.Manager {
	if e.ext == nil {
		return nil
	}
	return e.ext.tj
}

// PredictionEnabled reports whether the tracking filter is fed.
func (e *Engine) PredictionEnabled() bool { return e.EffectiveConfig().Prediction.Enabled }

// RFStats returns the RF statistics engine (nil when disabled or before Start).
func (e *Engine) RFStats() *rfstat.Stats {
	if e.ext == nil {
		return nil
	}
	return e.ext.rf.Load()
}

// Terrain returns the terrain analysis service (nil before Start).
func (e *Engine) Terrain() *terrain.Service {
	if e.ext == nil {
		return nil
	}
	return e.ext.terr
}

// TerrainPrefetch submits the terrain analysis of the current effective
// configuration (downloading missing elevation tiles), also when
// automatic prefetching is off. force re-runs it ignoring caches. It
// reports whether a job was started; ok is false when the receiver is not
// located or terrain is disabled.
func (e *Engine) TerrainPrefetch(force bool) (started, ok bool) {
	if e.ext == nil {
		return false, false
	}
	c := e.EffectiveConfig()
	if !c.Terrain.Enabled || !located(c.Receiver) {
		return false, false
	}
	return e.ext.terr.Submit(terrainRequest(c), force), true
}

// NavData returns the navigation data service (nil before Start).
func (e *Engine) NavData() *navdata.NavData {
	if e.ext == nil {
		return nil
	}
	return e.ext.nd
}

// Region returns the airport region around the receiver (nil until loaded).
func (e *Engine) Region() *navdata.Region {
	if e.ext == nil {
		return nil
	}
	return e.ext.region.Load()
}

// RuntimeContext returns the context of the running components (cancelled
// on Stop and factory reset); background work started by the API (imports)
// is bound to it.
func (e *Engine) RuntimeContext() context.Context {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.rt == nil {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	return e.rt.ctx
}

// Alerts returns the alert engine (nil before Start).
func (e *Engine) Alerts() *alerts.Engine {
	if e.ext == nil {
		return nil
	}
	return e.ext.al
}
