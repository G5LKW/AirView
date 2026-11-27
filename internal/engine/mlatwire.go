package engine

// MLAT engine and mlat-client listener (DESIGN §5.2, §9; internal/mlat
// WIRING.md). In aggregator mode with aggregator.mlat.enabled the engine
// builds one mlat.Engine behind the ingest core: closed timed groups, clock
// records and late copies go in, positions come back into the ingest as
// MLAT updates (global tracker) and as results to the contributing
// feeders over AVF/1 and the mlat-client protocol. The registry's stations
// are pushed into the engine by a sync loop (the registry has no change
// notification; one pass over the in-memory cache every few seconds is
// cheap even at 1,000+ stations).
//
// The engine is built once at Start: switching aggregator.mlat.enabled
// takes effect after a restart. The mlat-client listener (aggregator.mlatPort)
// follows the config like the other listeners, but only runs while the
// engine does: without it nothing would consume the clients' timing.

import (
	"airview/internal/mlat/survey"
	"context"
	"fmt"
	"math"
	"net"
	"time"

	"airview/internal/geoid"
	"airview/internal/ingest"
	"airview/internal/ingest/mlatc"
	"airview/internal/mlat"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/group"
	"airview/internal/mlat/plan"
	"airview/internal/mlat/ref"
	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/registry"
	"airview/internal/registry/trust"
)

// mlatStationSyncEvery is the period of the registry → MLAT engine sync.
const mlatStationSyncEvery = 5 * time.Second

// newMLAT builds the MLAT engine and wires it into the ingest options io.
// The engine runs once attachNet has built the ingest core.
func (e *Engine) newMLAT(nc *netCore, cfg model.Config, io *ingest.Options) error {
	nc.posNotices = newPosNotices(nc.reg, e.log.With("component", "mlat"), ntp.Now)
	nc.survFwd = &surveyFwd{}
	nc.mlatWx = ref.NewBundleWeather()
	if e.wx != nil {
		nc.mlatWx.Set(e.wx.Latest())
	}
	eng, err := mlat.New(mlat.Options{
		Config:      cfg.Aggregator.MLAT,
		MaxAircraft: cfg.Aggregator.Limits.MaxAircraft,
		Now:         ntp.Now,
		Logger:      e.log.With("component", "mlat"),
		Updates:     mlatUpdates{nc},
		Results:     []mlat.ResultSender{avfResults{nc}, mlatcResults{nc}},
		PlanOut: plan.Fanout{
			plan.OutFunc(func(st model.StationIdx, d *plan.Delta) {
				if nc.core != nil {
					plan.IngestOut{Ingest: nc.core}.Interest(st, d)
				}
			}),
			plan.OutFunc(func(st model.StationIdx, d *plan.Delta) {
				if s := nc.mlatcServer(); s != nil {
					plan.MLATCOut{C: s}.Interest(st, d)
				}
			}),
		},
		Notifier: nc.posNotices,
		Weather:  nc.mlatWx,
		Survey:   nc.survFwd,
		// Mode A/C (DESIGN §9.9): the planner asks exactly its Mode A/C
		// stations' mlat-clients for every reply; the listener may be
		// restarted, so it is looked up per call.
		ModeACOut: plan.ModeACFunc(func(st model.StationIdx, on bool) {
			if s := nc.mlatcServer(); s != nil {
				s.SetModeACAll(st, on)
			}
		}),
	})
	if err != nil {
		return err
	}
	nc.mlat = eng
	surv, err := e.newSurvey(nc, cfg, eng)
	if err != nil {
		return err
	}
	nc.surv = surv
	nc.survFwd.s.Store(surv)
	if surv != nil {
		bd, err := e.newBundler(nc, cfg, eng)
		if err != nil {
			e.log.Warn("bundle adjustment disabled", "err", err)
		}
		nc.bundler = bd
	}
	// The tap counts each station's timed copies and groups for the
	// mlat_minute rows (mlatmetrics.go) and passes everything on.
	// The timing quarantine (internal/registry/trust) sits between the tap
	// and the engine: copies of stations whose timing is not proven yet
	// never reach the solver or the clock filter (H-mlatc).
	nc.trust = trust.New(eng, trust.RegistrySource{R: nc.reg}, trust.ConfigFromModel(cfg.Aggregator.MLAT),
		trust.RegistryPromoter(nc.reg, e.log.With("component", "trust")))
	// The survey sits in front of the quarantine: a station under survey
	// usually has no proven timing yet (survey.Tee; WIRING.md there).
	if nc.surv != nil {
		var next ingest.MLATSink = nc.trust
		if nc.bundler != nil {
			next = bundleTee{Next: nc.trust, B: nc.bundler}
		}
		nc.mlatTap = newMLATTap(survey.Tee{Next: next, S: nc.surv})
	} else {
		nc.mlatTap = newMLATTap(nc.trust)
	}
	io.MLAT = nc.mlatTap
	if cfg.Aggregator.MLAT.ModeAC {
		io.ModeAC = func(f model.RawFrame) {
			fs := [1]model.RawFrame{f}
			eng.ObserveModeACFrames(fs[:])
		}
	}
	io.Clock = eng.ClockView()
	io.ClockObserver = eng
	if cfg.Aggregator.Ingest.AdaptiveFlush {
		nc.flushEst = group.NewFlushEstimator() // adaptive flush (DESIGN §9.3; H-clock)
		io.Flush = nc.flushEst
	}
	// Hold-out aircraft never train the stations' latency (DESIGN §9.6).
	hold := eng.HoldOut()
	io.LearnExclude = func(addr uint32, nowNs int64) bool { return hold.In(addr, nowNs) }
	return nil
}

// startMLAT runs the engine and the station sync loop (after the ingest
// core exists).
func (e *Engine) startMLAT(nc *netCore) {
	eng := nc.mlat
	if eng == nil {
		return
	}
	nc.wg.Add(3)
	go func() {
		defer nc.wg.Done()
		if err := eng.Run(nc.ctx); err != nil && nc.ctx.Err() == nil {
			nc.log.Error("MLAT engine stopped", "err", err)
		}
	}()
	if surv := nc.surv; surv != nil {
		nc.wg.Add(2)
		go func() {
			defer nc.wg.Done()
			_ = surv.Run(nc.ctx)
			surv.Close()
		}()
		go func() { defer nc.wg.Done(); e.runSurveyHints(nc) }()
	}
	if bd := nc.bundler; bd != nil {
		nc.wg.Add(1)
		go func() { defer nc.wg.Done(); bd.Run(nc.ctx) }()
	}
	reg := nc.reg
	nc.mlatRec = newMLATRecorder(engineMetrics{eng}, nc.mlatTap, eng.Fixes(), e.st, nc.stationsDB,
		func() []registry.Station { return reg.List(registry.Filter{Admission: model.AdmApproved}) }, ntp.Now)
	nc.mlatRec.tee = nc.publishFederationMLAT
	go func() { defer nc.wg.Done(); nc.mlatRec.run(nc.ctx) }()
	go func() {
		defer nc.wg.Done()
		last := map[model.StationIdx]mlat.StationInfo{}
		lastSurv := map[model.StationIdx]surveyKey{}
		lastBundle := map[model.StationIdx]string{}
		var restore []model.StationIdx
		t := time.NewTicker(mlatStationSyncEvery)
		defer t.Stop()
		prunedAt := time.Now()
		for {
			syncMLATStations(nc.reg, eng, last)
			if surv := nc.surv; surv != nil {
				list := reg.List(registry.Filter{Admission: model.AdmApproved})
				restore = restore[:0]
				syncSurveyStations(list, surv, lastSurv,
					func(idx model.StationIdx) { restore = append(restore, idx) }, nc.hints)
				if bd := nc.bundler; bd != nil {
					syncBundleStations(list, bd, lastBundle, nc.hints)
				}
				for _, idx := range restore {
					ctx, cancel := context.WithTimeout(nc.ctx, 5*time.Second)
					nc.restoreSurvey(ctx, idx)
					cancel()
				}
				// The registry follows the engine's AutoUseSurvey verdict
				// (surveyloc.go).
				ctx, cancel := context.WithTimeout(nc.ctx, 20*time.Second)
				reconcileSurveyLocations(ctx, reg, list, eng, func(idx model.StationIdx) bool {
					stt, ok := surv.Status(idx)
					return ok && stt.Running
				})
				cancel()
			}
			if fe := nc.flushEst; fe != nil && time.Since(prunedAt) >= time.Minute {
				prunedAt = time.Now()
				fe.Prune(ntp.Now().Add(-10 * time.Minute).UnixNano())
			}
			select {
			case <-nc.ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	nc.log.Info("MLAT engine running")
}

// surveyOverrider is the part of mlat.Engine that applies the owner's
// persisted survey rejection.
type surveyOverrider interface {
	OverrideSurvey(st model.StationIdx, reject bool) bool
}

// mlatStationTarget is the part of mlat.Engine the station sync uses.
type mlatStationTarget interface {
	SetStation(st model.StationIdx, in mlat.StationInfo)
	RemoveStation(st model.StationIdx)
}

// syncMLATStations pushes every approved station whose description changed
// since the last pass and removes the stations that are gone (deleted,
// revoked, denied). last is the state of the previous pass (updated).
func syncMLATStations(reg *registry.Registry, eng mlatStationTarget, last map[model.StationIdx]mlat.StationInfo) {
	seen := make(map[model.StationIdx]struct{}, len(last))
	for _, st := range reg.List(registry.Filter{Admission: model.AdmApproved}) {
		in := mlatStationInfo(&st)
		seen[st.Idx] = struct{}{}
		prev, known := last[st.Idx]
		if known && prev == in {
			continue
		}
		eng.SetStation(st.Idx, in)
		last[st.Idx] = in
		if !known && st.Has(registry.FlagSurveyRejected) {
			if o, ok := eng.(surveyOverrider); ok {
				o.OverrideSurvey(st.Idx, true) // the owner's persisted choice
			}
		}
	}
	for idx := range last {
		if _, ok := seen[idx]; !ok {
			eng.RemoveStation(idx)
			delete(last, idx)
		}
	}
}

// mlatStationInfo maps a registry station onto the engine's description
// (DESIGN §10.1): the receiver's GNSS, the owner-confirmed position, the
// declared position (MSL converted with the geoid; an ambiguous height
// from an mlat-client handshake gets the geoid undulation added to its σ)
// and the survey's suspect verdict.
//
// Anchor eligibility (finding ROB-4): an mlat-client station with a GPS
// clock (clock_type radarcape_gps) never sends Radarcape status frames, so
// it is NoStatus (anchor condition (a) is skipped; the clock filter's
// server-time check still applies). A station the admin barred from
// anchoring (FlagNoAnchor) or with negative trust is Flagged.
func mlatStationInfo(st *registry.Station) mlat.StationInfo {
	in := mlat.StationInfo{Class: st.ClockCls}
	in.NoStatus = st.Kind == model.StationKindMLATC && st.ClockCls.GPS()
	in.Flagged = st.Has(registry.FlagNoAnchor) || st.Trust < 0
	if st.GNSS.Valid {
		in.GNSS = mlat.Position{Valid: true, ECEF: geo.ToECEF(st.GNSS.Lat, st.GNSS.Lon, st.GNSS.Alt)}
	}
	if st.Located {
		g := st.Effective
		p := mlat.Position{Valid: true, ECEF: geo.ToECEF(g.Lat, g.Lon, g.HAE)}
		switch {
		case st.LocStatus == model.LocGNSS && !in.GNSS.Valid:
			in.GNSS = p
		case st.LocStatus == model.LocConfirmed:
			in.Confirmed = p
		}
	}
	if d := st.Declared; d.Valid {
		hae, sigma := d.Alt, 0.0 // 0 = the engine's default for declared positions
		switch st.DeclaredAltRef {
		case model.AltRefHAE:
		case model.AltRefAmbiguous:
			// MSL or HAE: two values N apart. The midpoint with a vertical
			// σ of |N|/2 is the better estimate; mlat.Position has one σ, so
			// it combines the declared 30 m with |N|/2 (H-mlatc).
			if h, ok := geoid.MSLToHAE(d.Lat, d.Lon, d.Alt); ok {
				hae = (h + d.Alt) / 2
				sigma = math.Hypot(30, math.Abs(h-d.Alt)/2)
			}
		default: // MSL (the owner form's default)
			if h, ok := geoid.MSLToHAE(d.Lat, d.Lon, d.Alt); ok {
				hae = h
			}
		}
		in.Declared = mlat.Position{Valid: true, ECEF: geo.ToECEF(d.Lat, d.Lon, hae), SigmaM: sigma}
		if st.LocStatus == model.LocSuspect {
			in.Suspect = true
			if est := st.Estimated; est.Valid {
				in.DiscrepancyM = geo.ToECEF(est.Lat, est.Lon, est.Alt).Sub(in.Declared.ECEF).Norm()
			}
		}
	}
	return in
}

// mlatUpdates hands the engine's positions to the ingest (global tracker).
type mlatUpdates struct{ nc *netCore }

func (u mlatUpdates) HandleUpdate(d model.DecodedUpdate) {
	if in := u.nc.ingest; in != nil {
		in.HandleUpdate(d)
	}
}

// avfResults and mlatcResults deliver results through whichever listener
// currently runs (they are restarted on port changes).
type avfResults struct{ nc *netCore }

func (r avfResults) SendResults(st model.StationIdx, fixes []model.MLATFix) bool {
	r.nc.mu.Lock()
	a := r.nc.avf
	r.nc.mu.Unlock()
	return a != nil && a.srv.SendResults(st, fixes)
}

type mlatcResults struct{ nc *netCore }

func (r mlatcResults) SendResults(st model.StationIdx, fixes []model.MLATFix) bool {
	s := r.nc.mlatcServer()
	return s != nil && s.SendResults(st, fixes)
}

// mlatcServer returns the running mlat-client listener (nil = none).
func (nc *netCore) mlatcServer() *mlatc.Server {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	return nc.mlatc
}

// syncMLATC makes the mlat-client listener match cfg (aggregator.mlatPort).
func (e *Engine) syncMLATC(rt *runtimeState, nc *netCore, cfg model.Config) {
	port := cfg.Aggregator.MLATPort
	addr := ""
	if port > 0 && nc.core != nil && nc.mlat != nil {
		addr = e.listenAddr(port)
	}
	// The UDP transport settings are fixed at listener start, so a change
	// restarts the listener like an address change (WP-MLATC-udp).
	udpKey := ""
	if addr != "" {
		m := cfg.Aggregator.MLATC
		udpKey = fmt.Sprintf("%v|%s|%d|%s|%v|%d|%v", cfg.Aggregator.MLAT.UDP, m.UDPHost, m.UDPPort, m.UDPListen,
			m.UDPAnySource, m.UDPReaders, m.ModeACAllClients)
	}
	nc.mu.Lock()
	cur, curAt, curUDP := nc.mlatc, nc.mlatcAt, nc.mlatcUDP
	nc.mu.Unlock()
	if cur != nil && curAt == addr && curUDP == udpKey {
		cur.SetPolicy(mlatc.PolicyFromConfig(cfg.Aggregator))
		return
	}
	if cur != nil {
		nc.mu.Lock()
		nc.mlatc, nc.mlatcAt = nil, ""
		nc.mu.Unlock()
		_ = cur.Close()
		nc.log.Info("mlat-client listener stopped")
	}
	if addr == "" {
		nc.mu.Lock()
		nc.mlatcErr = ""
		nc.mu.Unlock()
		return
	}
	opt := mlatc.OptionsFromConfig(cfg.Aggregator)
	opt.Addr = addr
	opt.ServerKey = nc.reg.IngestKey()
	opt.Now = ntp.Now
	opt.Logger = e.log.With("component", "mlatc")
	opt.Gate = nc.ready.Load
	opt.Stats = nc.mlat
	opt.Trust = nc.trust // probe requests for quarantined mlat-clients
	eng := nc.mlat
	opt.Hooks = mlatc.Hooks{OnSeen: eng.NoteSeen, OnRates: func(st model.StationIdx, rates []mlatc.AircraftRate) {
		// rate_report: an aircraft with a non-zero ADS-B position rate is
		// heard by the station, so the planner can rank the station for the
		// aircraft's sync set before any group of it arrives (H-mlatc).
		addrs := make([]uint32, 0, len(rates))
		for _, r := range rates {
			if r.Rate > 0 {
				addrs = append(addrs, r.Addr)
			}
		}
		if len(addrs) > 0 {
			eng.NoteSeen(st, addrs)
		}
	}}
	srv, err := mlatc.New(opt, nc.reg.MLATC(), mlatc.FromIngest(nc.core))
	var ln net.Listener
	if err == nil {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		if srv != nil {
			_ = srv.Close()
		}
		e.listenerFailed(nc, &nc.mlatcErr, "mlat-client", port, err)
		return
	}
	nc.mu.Lock()
	nc.mlatc, nc.mlatcAt, nc.mlatcErr, nc.mlatcUDP = srv, addr, "", udpKey
	nc.mu.Unlock()
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		if err := srv.Serve(rt.ctx, ln); err != nil && rt.ctx.Err() == nil {
			nc.log.Error("mlat-client listener stopped", "err", err)
		}
	}()
	nc.log.Info("mlat-client listener", "addr", ln.Addr().String(), "gated", !nc.ready.Load())
}

// mlatcStatus is the NetworkStatus line of the mlat-client listener.
func (nc *netCore) mlatcStatus(cfg model.Config) ListenerStatus {
	s := ListenerStatus{Port: cfg.Aggregator.MLATPort}
	if s.Port == 0 {
		return s
	}
	nc.mu.Lock()
	s.Listening, s.Addr, s.Error = nc.mlatc != nil, nc.mlatcAt, nc.mlatcErr
	srv := nc.mlatc
	nc.mu.Unlock()
	if srv != nil {
		switch active, uerr := srv.UDPStatus(); {
		case active:
			s.UDP = "active"
		case uerr != "":
			s.UDP = uerr
		case cfg.Aggregator.MLAT.UDP:
			s.UDP = "starting"
		default:
			s.UDP = "off"
		}
	}
	switch {
	case nc.mlat == nil:
		s.Note = "the MLAT engine is not running (aggregator.mlat.enabled)"
	case nc.core == nil:
		s.Note = "the ingest core is not running"
	}
	return s
}

// MLAT returns the running MLAT engine (nil in standalone mode or when
// aggregator.mlat.enabled is off). Its read side is safe from any
// goroutine.
func (e *Engine) MLAT() *mlat.Engine {
	if e.net == nil {
		return nil
	}
	return e.net.mlat
}

// TrustGate returns the timing quarantine in front of the MLAT engine (nil
// without the engine).
func (e *Engine) TrustGate() *trust.Gate {
	if e.net == nil {
		return nil
	}
	return e.net.trust
}

// MLATCIngest returns the running mlat-client listener (nil = none).
func (e *Engine) MLATCIngest() *mlatc.Server {
	if e.net == nil {
		return nil
	}
	return e.net.mlatcServer()
}

// mlatChanged reports whether cfg's aggregator.mlat block differs from the
// one the MLAT engine was built with (RestartRequired). nc may be nil.
//
// returnResults is the one exception: the listeners follow it at once
// (avfsrv / mlatc policies), so switching it off needs no restart; only
// switching it on when the engine was built with it off does (the engine
// then routes nothing).
func (nc *netCore) mlatChanged(cfg model.Config) bool {
	if nc == nil || cfg.Mode != model.ModeAggregator {
		return false
	}
	cur, boot := cfg.Aggregator.MLAT, nc.mlatBoot
	if boot.ReturnResults && !cur.ReturnResults {
		cur.ReturnResults = true
	}
	return cur != boot
}
