package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"time"

	"airview/internal/config"
	"airview/internal/feed"
	"airview/internal/hub"
	"airview/internal/model"
	"airview/internal/sim"
)

// located reports whether rx has usable coordinates.
func located(rx model.Receiver) bool {
	return rx.Located && !math.IsNaN(rx.Lat) && !math.IsNaN(rx.Lon) &&
		rx.Lat >= -90 && rx.Lat <= 90 && rx.Lon >= -180 && rx.Lon <= 180
}

// effectiveMode maps the configured mode to the one that is honoured.
func (e *Engine) effectiveMode(cfg model.Config) string {
	if e.opts.Demo {
		return model.ModeStandalone
	}
	switch cfg.Mode {
	case model.ModeRemoteData:
		if e.opts.AllowRemoteData && cfg.Remote.Host != "" {
			return model.ModeRemoteData
		}
	case model.ModeRemoteWeb:
		if e.opts.AllowRemoteWeb && cfg.Remote.Host != "" {
			return model.ModeRemoteWeb
		}
	}
	if e.net != nil {
		// Aggregator mode is fixed at Start: the engine keeps running as an
		// aggregator until a restart, whatever the stored mode says
		// (RestartRequired reports the pending switch).
		return model.ModeAggregator
	}
	return model.ModeStandalone
}

// computeEffective derives the effective configuration and feed list from
// the stored configuration (and the adopted host station, if any).
func (e *Engine) computeEffective(stored model.Config) *effective {
	eff := &effective{cfg: config.Clone(stored)}
	eff.mode = e.effectiveMode(stored)
	eff.demo = e.opts.Demo || (stored.Demo.Enabled && eff.mode == model.ModeStandalone)
	eff.cfg.Mode = eff.mode

	suspended := func() []model.Feed {
		out := make([]model.Feed, len(stored.Feeds))
		copy(out, stored.Feeds)
		for i := range out {
			out[i].Enabled = false
		}
		return out
	}

	switch {
	case eff.demo:
		// The simulator falls back to its default site for an unlocated
		// receiver and for latitudes beyond +-70 degrees; follow it so that
		// the tracker's geometry matches the simulated traffic.
		rx := &eff.cfg.Receiver
		if !located(*rx) || math.Abs(rx.Lat) > 70 {
			rx.Located = true
			rx.Lat, rx.Lon, rx.GroundElevM = sim.DefaultLat, sim.DefaultLon, sim.DefaultGroundElevM
		}
		n := eff.cfg.Demo.Aircraft
		if n <= 0 {
			n = sim.DefaultAircraft
		}
		demo := model.Feed{
			ID: DemoFeedID, Name: "Simulator (demo)", Enabled: true, Type: model.FeedDemo,
			Host: "simulator",
			// The manager restarts a feed whose definition changed: encode
			// everything the simulator depends on.
			Notes: fmt.Sprintf("%d aircraft around %.4f, %.4f [%s]", n, rx.Lat, rx.Lon, antennaDigest(eff.cfg)),
		}
		feeds := suspended()
		for i := range feeds {
			if feeds[i].ID == DemoFeedID {
				feeds = append(feeds[:i], feeds[i+1:]...)
				break
			}
		}
		eff.feeds = append(feeds, demo)
		eff.cfg.Feeds = eff.feeds

	case eff.mode == model.ModeRemoteData:
		port := stored.Remote.DataPort
		if port <= 0 {
			port = hub.DefaultPort
		}
		remote := model.Feed{
			ID: RemoteFeedID, Name: "AirView host " + stored.Remote.Host, Enabled: true,
			Type: model.FeedHub, Host: stored.Remote.Host, Port: port,
		}
		eff.feeds = []model.Feed{remote}
		eff.cfg.Feeds = []model.Feed{remote}
		e.mu.Lock()
		hs := e.hostStation
		e.mu.Unlock()
		if stored.Remote.UseHostReceiver && hs != nil {
			eff.cfg.Receiver = hs.Receiver
			eff.cfg.Antennas = append([]model.Antenna(nil), hs.Antennas...)
			// The host's feed IDs travel with the frames
			// (HubOptions.KeepHostFeedIDs); listing them gives the tracker
			// the host's feed-to-antenna attribution.
			for _, hf := range hs.Feeds {
				if hf.ID == "" || hf.ID == RemoteFeedID {
					continue
				}
				eff.cfg.Feeds = append(eff.cfg.Feeds, model.Feed{
					ID: hf.ID, Name: hf.Name, Enabled: true, Type: hf.Type,
					AntennaID: hf.AntennaID, MLATOnly: hf.MLATOnly,
				})
			}
		}

	case eff.mode == model.ModeRemoteWeb:
		eff.feeds = suspended()
		eff.cfg.Feeds = eff.feeds

	default:
		eff.feeds = append([]model.Feed(nil), stored.Feeds...)
		if stored.Mode == model.ModeAggregator {
			// SCALE-08: the factory "radarcape" Beast feed of a station
			// makes no sense on an aggregator (a VPS has no receiver
			// called radarcape): it would retry forever and log errors.
			// An operator-edited feed is kept.
			eff.feeds = slices.DeleteFunc(eff.feeds, model.IsFactoryDefaultFeed)
			eff.cfg.Feeds = eff.feeds
		}
	}
	return eff
}

// antennaDigest is a short fingerprint of the antenna set.
func antennaDigest(cfg model.Config) string {
	b, _ := json.Marshal(cfg.Antennas)
	h := fnv.New32a()
	_, _ = h.Write(b)
	return fmt.Sprintf("%08x", h.Sum32())
}

func hubOptions(cfg model.Config) feed.HubOptions {
	return feed.HubOptions{Password: cfg.Remote.Password, KeepHostFeedIDs: cfg.Remote.UseHostReceiver}
}

// weatherConfig returns the poller configuration (off when Offline).
func (e *Engine) weatherConfig(cfg model.Config) model.WeatherConfig {
	w := cfg.Weather
	if e.opts.Offline {
		w.Enabled = false
	}
	return w
}

// demoFactory builds the simulator runner for the synthetic demo feed.
func (e *Engine) demoFactory(f model.Feed) (feed.Runner, error) {
	return feed.RunnerFunc(func(ctx context.Context, s *feed.Session) error {
		sc := sim.FromModel(e.EffectiveConfig(), f.ID)
		sc.Seed = e.opts.DemoSeed
		sm := sim.New(sc)
		site := sm.Station()
		s.SetConnected("simulator")
		s.SetGPSTime(true)
		s.SetInfo(fmt.Sprintf("built-in simulator, up to %d aircraft around %.3f, %.3f", sc.Aircraft, site.Lat, site.Lon))
		err := sm.Run(ctx, &dropoutSink{next: s})
		s.SetDisconnected(nil)
		if err == nil {
			// Run only returns on cancellation.
			return ctx.Err()
		}
		return err
	}), nil
}

// onStation receives the host's station metadata from the hub feed (on the
// feed's goroutine: hand the work to the re-apply loop).
func (e *Engine) onStation(feedID string, st hub.Station) {
	c := st
	e.mu.Lock()
	e.hostStation = &c
	e.mu.Unlock()
	e.kick(e.reapplyC)
}

func (e *Engine) kick(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// lockOp takes opMu unless ctx ends first. Background loops must use it:
// stopRuntime waits for them while holding opMu.
func (e *Engine) lockOp(ctx context.Context) bool {
	for !e.opMu.TryLock() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
	}
	if ctx.Err() != nil {
		e.opMu.Unlock()
		return false
	}
	return true
}

// reapplyLoop re-derives the effective configuration when something outside
// the stored configuration changed (host station, hub retry).
func (e *Engine) reapplyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.reapplyC:
		}
		if !e.lockOp(ctx) {
			return
		}
		cur := e.holder.Get()
		e.reconfigure(cur, cur)
		e.opMu.Unlock()
	}
}

// syncHub makes the data port match the configuration. Caller holds opMu.
func (e *Engine) syncHub(rt *runtimeState, stored model.Config, eff *effective) {
	want := stored.Hub.Enabled && eff.mode != model.ModeRemoteWeb
	port := stored.Hub.Port
	if port <= 0 {
		port = hub.DefaultPort
	}
	h := e.hub.Load()
	e.mu.Lock()
	runningPort := e.hubPort
	e.mu.Unlock()

	if h != nil && (!want || runningPort != port) {
		e.hub.Store(nil)
		_ = h.Close()
		h = nil
		e.log.Info("data port stopped")
	}
	if !want {
		e.mu.Lock()
		e.hubErr = ""
		e.mu.Unlock()
		return
	}
	if h == nil {
		adminOnly := eff.mode == model.ModeAggregator
		if adminOnly && stored.Hub.Password == "" {
			e.Notify("warn", "Data port: aggregator mode requires a hub password; all logins are refused")
		}
		// Aggregator mode: admin login only, non-protocol clients closed
		// quietly (DESIGN §5.4).
		h = hub.NewServer(hub.Options{Port: port, Password: stored.Hub.Password, AdminOnly: adminOnly, Logger: e.log})
		h.SetStation(eff.cfg.Receiver, eff.cfg.Antennas, hub.StationFeeds(eff.cfg.Feeds))
		if err := h.Start(rt.ctx); err != nil {
			_ = h.Close()
			msg := err.Error()
			e.mu.Lock()
			first := e.hubErr != msg
			e.hubErr = msg
			e.mu.Unlock()
			if first {
				e.log.Warn("data port could not be started", "port", port, "err", err)
				e.Notify("warn", fmt.Sprintf("Data port %d could not be started: %v", port, err))
			}
			return
		}
		e.mu.Lock()
		e.hubPort = port
		e.hubErr = ""
		e.mu.Unlock()
		e.hub.Store(h)
		e.log.Info("data port listening", "port", port, "protected", stored.Hub.Password != "")
		return
	}
	h.SetPassword(stored.Hub.Password)
	h.SetAdminOnly(eff.mode == model.ModeAggregator)
	h.SetStation(eff.cfg.Receiver, eff.cfg.Antennas, hub.StationFeeds(eff.cfg.Feeds))
}

// reconfigure applies the step from the stored configuration old to cur to
// the running components. Caller holds opMu; the runtime is running.
func (e *Engine) reconfigure(old, cur model.Config) {
	// Leaving the host (or pointing at another one) forgets its station.
	if cur.Mode != model.ModeRemoteData || cur.Remote.Host != old.Remote.Host ||
		cur.Remote.DataPort != old.Remote.DataPort || !cur.Remote.UseHostReceiver {
		e.mu.Lock()
		e.hostStation = nil
		e.mu.Unlock()
	}

	prev := e.eff.Load()
	eff := e.computeEffective(cur)
	e.eff.Store(eff)

	if prev == nil || !config.Equal(prev.cfg, eff.cfg) {
		e.tr.SetConfig(config.Clone(eff.cfg))
		e.applyExt(prev, eff)
	}
	if prev != nil && (prev.mode != eff.mode || prev.demo != eff.demo) {
		e.log.Info("run mode changed", "mode", eff.mode, "demo", eff.demo)
		// A different data source: a receiver position reported by the old
		// one no longer applies.
		e.rxGPS.Store(nil)
	}
	e.syncLatencyReceiver()

	if e.net != nil && e.net.host != nil {
		e.net.host.setFeeds(eff.feeds)
	}
	if rt := e.rt; rt != nil {
		rt.fm.SetHubOptions(hubOptions(cur))
		rt.fm.Apply(eff.feeds)
		e.syncHub(rt, cur, eff)
		e.syncNetListeners(rt, cur)
	}
	e.syncUplinks(cur, eff)
	e.syncSSR(eff.cfg)
	if old.NTP != cur.NTP {
		e.ntp.Reconfigure(cur.NTP)
	}
	e.wx.Reconfigure(e.weatherConfig(cur), eff.cfg.Receiver)

	e.mu.Lock()
	e.restartReq = cur.HTTP.Port != e.bootHTTP.Port || cur.HTTP.BindAddr != e.bootHTTP.BindAddr ||
		e.wantNetwork(cur) != (e.net != nil) || e.net.mlatChanged(cur) || storageProfileChanged(e.bootStorage, cur.Storage)
	if old.Storage != cur.Storage {
		e.pruneDue = true
	}
	e.mu.Unlock()
	e.kick(e.choreC)
}

// ApplyConfig is the PUT /api/config pipeline: the incoming configuration is
// merged with the stored secrets and validated (config.ApplyUpdate), saved,
// published through the holder and applied to the affected components only.
// It returns the stored configuration (not redacted) and the validation
// notes. A changed HTTP port or bind address is saved but needs a restart:
// see RestartRequired.
func (e *Engine) ApplyConfig(incoming model.Config) (model.Config, []string, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if !e.started || e.stopped {
		return model.Config{}, nil, ErrNotRunning
	}
	cur := e.holder.Get()
	next, notes := config.ApplyUpdate(incoming, cur)
	if config.Equal(cur, next) {
		return next, notes, nil
	}
	// Commit keeps operator-file / env / -set values out of config.json and
	// re-applies them, so locked settings stay in force.
	next, err := e.layers.Commit(e.dir, next)
	if err != nil {
		return cur, notes, fmt.Errorf("engine: save configuration: %w", err)
	}
	if config.Equal(cur, next) {
		return next, notes, nil
	}
	e.holder.Set(next)
	e.reconfigure(cur, next)
	e.log.Info("configuration applied", "mode", e.Mode(), "demo", e.Demo(), "notes", len(notes))
	return next, notes, nil
}

// mutateConfig changes the stored configuration from a background loop.
func (e *Engine) mutateConfig(ctx context.Context, fn func(c *model.Config) bool) error {
	if !e.lockOp(ctx) {
		return ctx.Err()
	}
	defer e.opMu.Unlock()
	cur := e.holder.Get()
	next := config.Clone(cur)
	if !fn(&next) {
		return nil
	}
	next, _ = config.Validate(next)
	if config.Equal(cur, next) {
		return nil
	}
	next, err := e.layers.Commit(e.dir, next)
	if err != nil {
		return err
	}
	if config.Equal(cur, next) {
		return nil
	}
	e.holder.Set(next)
	e.reconfigure(cur, next)
	return nil
}

// Layers returns the configuration layers (operator file, environment and
// -set overrides); nil before Start. A nil *config.Layers reports no
// overrides. It is set once by Start and never replaced.
func (e *Engine) Layers() *config.Layers { return e.layers }

// FactoryReset stops the components, wipes the database, restores the
// factory configuration and starts the components again. The -demo flag and
// the other Options stay in force.
func (e *Engine) FactoryReset() error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if !e.started || e.stopped {
		return ErrNotRunning
	}
	e.log.Warn("factory reset requested")
	e.cancelImport()
	e.stopRuntime(false)

	var firstErr error
	if err := e.st.Reset(); err != nil {
		e.log.Error("factory reset: database", "err", err)
		firstErr = fmt.Errorf("engine: reset database: %w", err)
	}
	cfg, err := e.layers.FactoryReset(e.dir)
	if err != nil {
		e.log.Error("factory reset: configuration", "err", err)
		if firstErr == nil {
			firstErr = fmt.Errorf("engine: reset configuration: %w", err)
		}
	}
	old := e.holder.Get()
	e.holder.Set(cfg)

	e.mu.Lock()
	e.hostStation = nil
	e.hubErr = ""
	e.wxPersistH, e.wxPersistAt = 0, ""
	e.pruneDue = false
	e.restartReq = cfg.HTTP.Port != e.bootHTTP.Port || cfg.HTTP.BindAddr != e.bootHTTP.BindAddr ||
		e.wantNetwork(cfg) != (e.net != nil) || e.net.mlatChanged(cfg) || storageProfileChanged(e.bootStorage, cfg.Storage)
	e.mu.Unlock()
	e.rxGPS.Store(nil)

	e.tr.Reset()
	e.lat.Reset()
	e.met.Reset()
	e.fc.Reset()
	e.an.Invalidate()
	e.resetExt()

	eff := e.computeEffective(cfg)
	e.eff.Store(eff)
	e.tr.SetConfig(config.Clone(eff.cfg))
	e.syncLatencyReceiver()
	e.applyExt(nil, eff)
	e.syncSSR(eff.cfg)
	if old.NTP != cfg.NTP {
		e.ntp.Reconfigure(cfg.NTP)
	}
	e.wx.Reconfigure(e.weatherConfig(cfg), eff.cfg.Receiver)

	e.startRuntime()
	e.Notify("info", "Factory reset complete: database and settings are back to defaults.")
	e.log.Warn("factory reset done")
	return firstErr
}
