package main

import (
	"airview/internal/sdr/sdrfeed"
	"airview/internal/selfupdate"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/avf"
	"airview/internal/feed"
	"airview/internal/feederui"
	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/uplink"
)

// runOptions configure one feeder process.
type runOptions struct {
	ConfigFile string
	DataDir    string
	Environ    []string
	Sets       []string
	Logf       func(format string, args ...any)
	Logger     *slog.Logger
	// UIAddr overrides ui.addr (tests: "127.0.0.1:0").
	UIAddr string
	// ResultsHost overrides the results listener host (tests).
	ResultsHost string
	// DetectEvery is the auto-detection retry interval (default 15 s).
	DetectEvery time.Duration
	// Ready, if set, is called once everything is started.
	Ready func(f *feeder)
}

// feeder is the running airview-feeder: inputs (internal/feed), the
// uplinks (internal/uplink), the local result outputs and the status page.
// It implements feederui.Backend.
type feeder struct {
	o       runOptions
	started time.Time
	id      *uplink.Identity

	cfgMu sync.Mutex // guards lc and the apply sequence
	lc    *loadedConfig

	fm      *feed.Manager
	um      *uplink.Manager
	stats   *feederui.Stats
	results *resultsOut
	ntp     *ntp.Sampler

	rx atomic.Pointer[map[string]model.FeederReceiver] // feed ID -> receiver

	detMu sync.Mutex
	det   map[string]*detState // feed ID of auto receivers

	uiMu   sync.Mutex
	ui     *http.Server
	uiLn   net.Listener
	uiAddr string
	uiErr  string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// att is the self-update attachment (update.go; nil = not set up);
	// exitReq ends the run loop (self-update restart or rollback).
	att     *selfupdate.Attachment
	exitReq chan int
}

// detState is the auto-detection state of one "auto" receiver.
type detState struct {
	found     bool
	d         detection
	lastTry   time.Time
	downSince time.Time
	tries     int
}

// tee delivers every input frame to the local statistics and the uplinks.
type tee struct {
	st *feederui.Stats
	um *uplink.Manager
}

func (t tee) HandleFrame(f model.RawFrame) {
	t.st.HandleFrame(f)
	t.um.HandleFrame(f)
}

func (t tee) HandleUpdate(u model.DecodedUpdate) {
	t.st.HandleUpdate(u)
	t.um.HandleUpdate(u)
}

// startFeeder loads the configuration and starts everything. A missing or
// broken configuration file is reported but not fatal unless it cannot be
// parsed at all (then the error is returned and nothing runs).
func startFeeder(o runOptions) (*feeder, error) {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.DetectEvery <= 0 {
		o.DetectEvery = 15 * time.Second
	}
	lc, err := loadConfig(loadOptions{File: o.ConfigFile, Environ: o.Environ, Sets: o.Sets})
	for _, is := range lc.Issues {
		o.Logf("config: %s", is)
	}
	if err != nil && lc.Exists {
		return nil, fmt.Errorf("configuration %s: %w", o.ConfigFile, err)
	}
	if !lc.Exists {
		o.Logf("config: %s does not exist; running with defaults (run \"airview-feeder setup\")", o.ConfigFile)
	}
	for _, n := range lc.Notes {
		o.Logf("config: %s", n)
	}
	if err := os.MkdirAll(o.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("data directory: %w", err)
	}

	f := &feeder{o: o, started: time.Now(), lc: lc, det: map[string]*detState{}, exitReq: make(chan int, 1)}
	if err := f.attachUpdate(lc.Eff); err != nil {
		return nil, err
	}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	f.stats = feederui.NewStats(nil)
	f.results = newResultsOut(o.Logf)
	if o.ResultsHost != "" {
		f.results.listenAt = o.ResultsHost
	}
	f.ntp = ntp.NewSampler(lc.Eff.NTP, ntp.Options{Logger: o.Logger, Burst: 1})

	uo := uplink.Options{
		Software:  avf.SoftwareFeeder,
		Version:   version,
		StateDir:  uplinkStateDir(o.DataDir),
		Source:    uplink.SourceFuncs{ForwardableFn: f.forwardable, ReceiverFn: f.receiverInfo},
		OnResults: f.results.handle,
		Logf:      o.Logf,
		Now:       ntp.Now,
		HostErrNs: f.hostErrNs,
	}
	uo.ApplyTuning(lc.Eff.UplinkTuning)
	um, err := uplink.NewManager(uo)
	if err != nil {
		return nil, fmt.Errorf("feeder identity: %w", err)
	}
	f.um = um
	f.id = um.Identity()
	f.fm = feed.NewWithOptions(tee{st: f.stats, um: um}, feed.Options{Logger: o.Logger})
	f.fm.RegisterType(model.FeedRTLTCP, sdrfeed.Factory(o.Logf))
	f.fm.OnClockEvent(um.HandleClockEvent)
	f.fm.OnReceiverPosition(func(id string, p feed.ReceiverPosition) {
		um.HandleGNSS(id, p.Lat, p.Lon, p.AltM, model.AltRefHAE, ntp.Now())
	})
	f.fm.Start(f.ctx)

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		_ = f.ntp.Run(f.ctx)
	}()

	f.cfgMu.Lock()
	f.applyLocked()
	f.cfgMu.Unlock()

	f.wg.Add(1)
	go f.detectLoop()

	f.runUpdate()
	o.Logf("airview-feeder %s started: feeder ID %s, config %s, data %s", version, f.id.FeederID, o.ConfigFile, o.DataDir)
	if o.Ready != nil {
		o.Ready(f)
	}
	return f, nil
}

// applyLocked makes the running parts match f.lc.Eff. cfgMu must be held.
func (f *feeder) applyLocked() {
	eff := f.lc.Eff
	rx := map[string]model.FeederReceiver{}
	for _, r := range eff.Receivers {
		rx[r.Input.ID] = r
	}
	f.rx.Store(&rx)

	f.detMu.Lock()
	for id := range f.det {
		if r, ok := rx[id]; !ok || r.Input.Type != model.FeedAuto {
			delete(f.det, id)
		}
	}
	for id, r := range rx {
		if r.Input.Type == model.FeedAuto && f.det[id] == nil {
			f.det[id] = &detState{}
		}
	}
	f.detMu.Unlock()

	f.applyFeeds()
	f.um.ApplyAll(eff.Uplinks)
	f.results.apply(eff.Results)
	f.ntp.Reconfigure(eff.NTP)
	f.applyUI(eff.UI)
}

// feeds returns the concrete feed list: configured inputs plus the
// detected address of every auto receiver found so far.
func (f *feeder) feeds() []model.Feed {
	f.detMu.Lock()
	defer f.detMu.Unlock()
	var out []model.Feed
	for _, r := range f.lc.Eff.Receivers {
		in := r.Input
		if !in.Enabled {
			continue
		}
		if in.Type == model.FeedAuto {
			st := f.det[in.ID]
			if st == nil || !st.found {
				continue
			}
			in = detectedFeed(in, st.d)
		}
		out = append(out, in)
	}
	return out
}

func (f *feeder) applyFeeds() {
	f.fm.Apply(f.feeds())
}

// forwardable is uplink.Source.Forwardable: only configured receivers.
func (f *feeder) forwardable(feedID string) bool {
	if _, ok := (*f.rx.Load())[feedID]; !ok {
		return false
	}
	return f.fm.Forwardable(feedID)
}

// receiverInfo is uplink.Source.Receiver. It never includes host names or
// addresses (AVF §5.4).
func (f *feeder) receiverInfo(feedID string) uplink.ReceiverInfo {
	r, ok := (*f.rx.Load())[feedID]
	if !ok {
		return uplink.ReceiverInfo{}
	}
	info := uplink.ReceiverInfo{RxNo: r.RxNo, Name: r.Name, Channels: 1, InputType: r.Input.Type}
	info.Decoder = r.Input.Type + " input"
	f.detMu.Lock()
	if st := f.det[feedID]; st != nil && st.found {
		info.Decoder, info.InputType = st.d.Label, st.d.Type
	}
	f.detMu.Unlock()
	if r.Located {
		info.Location = &avf.Location{Lat: r.Lat, Lon: r.Lon, Alt: r.AltM, AltRef: r.AltRef, Source: "manual"}
	}
	info.Antennas = []avf.Antenna{{ID: "main", Kind: "antenna", AGLM: r.AGLM}}
	for _, s := range f.fm.Status() {
		if s.ID == feedID {
			info.Connected, info.FPS, info.SinceMs = s.Connected, s.MsgRate, s.Since
		}
	}
	return info
}

func (f *feeder) hostErrNs() uint64 {
	if f.ntp != nil && f.ntp.Synced() {
		off := f.ntp.Offset()
		if off < 0 {
			off = -off
		}
		return uint64(off) + 1e6
	}
	return 2e6
}

// detectLoop finds the receivers of "auto" inputs, and looks again when a
// detected receiver has been unreachable for two minutes.
func (f *feeder) detectLoop() {
	defer f.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		f.detectOnce()
		select {
		case <-f.ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (f *feeder) detectOnce() {
	now := time.Now()
	status := map[string]model.FeedStatus{}
	for _, s := range f.fm.Status() {
		status[s.ID] = s
	}
	type job struct {
		id   string
		host string
		port int
	}
	var jobs []job
	rx := *f.rx.Load()
	f.detMu.Lock()
	for id, st := range f.det {
		r := rx[id]
		if st.found {
			s, ok := status[id]
			switch {
			case ok && s.Connected:
				st.downSince = time.Time{}
			case st.downSince.IsZero():
				st.downSince = now
			case now.Sub(st.downSince) > 2*time.Minute && now.Sub(st.lastTry) > f.o.DetectEvery:
				jobs = append(jobs, job{id, r.Input.Host, r.Input.Port})
				st.lastTry = now
			}
			continue
		}
		every := f.o.DetectEvery
		if st.tries < 4 {
			every = min(every, 5*time.Second)
		}
		if now.Sub(st.lastTry) >= every {
			jobs = append(jobs, job{id, r.Input.Host, r.Input.Port})
			st.lastTry = now
			st.tries++
		}
	}
	f.detMu.Unlock()
	if len(jobs) == 0 {
		return
	}
	changed := false
	for _, j := range jobs {
		ds := detect(f.ctx, j.host, j.port, 1500*time.Millisecond)
		d, ok := best(ds)
		f.detMu.Lock()
		st := f.det[j.id]
		if st != nil {
			switch {
			case ok && (!st.found || st.d != d):
				f.o.Logf("input %s: found %s at %s", j.id, d.Label, d.Addr())
				st.found, st.d, st.downSince = true, d, time.Time{}
				changed = true
			case !ok && !st.found && (st.tries == 1 || st.tries%20 == 0):
				f.o.Logf("input %s: no receiver found yet (tried ports 10003, 30005, 30002, 30003 on %s); retrying", j.id, hostOr(j.host))
			}
		}
		f.detMu.Unlock()
	}
	if changed {
		f.cfgMu.Lock()
		f.applyFeeds()
		f.cfgMu.Unlock()
	}
}

func hostOr(h string) string {
	if h == "" {
		return "127.0.0.1"
	}
	return h
}

// applyUI (re)starts the status page when its address changed.
func (f *feeder) applyUI(c model.FeederUI) {
	addr := c.Addr
	if f.o.UIAddr != "" {
		addr = f.o.UIAddr
	}
	f.uiMu.Lock()
	defer f.uiMu.Unlock()
	if f.ui != nil && f.uiAddr == addr {
		return
	}
	if f.ui != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = f.ui.Shutdown(ctx)
		cancel()
		f.ui, f.uiLn = nil, nil
	}
	f.uiAddr, f.uiErr = addr, ""
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		f.uiErr = err.Error()
		f.o.Logf("status page: cannot listen on %s: %v", addr, err)
		return
	}
	h := feederui.New(feederui.Options{
		Backend: f,
		PasswordHash: func() string {
			f.cfgMu.Lock()
			defer f.cfgMu.Unlock()
			return f.lc.Eff.UI.PasswordHash
		},
		Logf: f.o.Logf,
	})
	// The self-update routes sit in front of the page (updateapi.go).
	srv := &http.Server{Handler: newUpdateAPI(f, h), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	f.ui, f.uiLn = srv, ln
	f.o.Logf("status page: http://%s/", ln.Addr())
	if ta, ok := ln.Addr().(*net.TCPAddr); ok && !ta.IP.IsLoopback() {
		// SEC-08: Basic auth over plain HTTP sends the password with every
		// request; anyone on the LAN path can read it.
		f.o.Logf("WARNING: the status page listens beyond this machine (%s) over plain HTTP: its password crosses the network unencrypted. Keep ui.addr on 127.0.0.1 unless the LAN is trusted.", ln.Addr())
	}
	go func() { _ = srv.Serve(ln) }()
}

// statusURL returns the base URL of the running status page.
func (f *feeder) statusURL() string {
	f.uiMu.Lock()
	defer f.uiMu.Unlock()
	if f.uiLn == nil {
		return ""
	}
	return "http://" + f.uiLn.Addr().String() + "/"
}

// reload re-reads the configuration file (SIGHUP).
func (f *feeder) reload() {
	lc, err := loadConfig(loadOptions{File: f.o.ConfigFile, Environ: f.o.Environ, Sets: f.o.Sets})
	for _, is := range lc.Issues {
		f.o.Logf("config: %s", is)
	}
	if err != nil && lc.Exists {
		f.o.Logf("config: reload refused, keeping the running configuration: %v", err)
		return
	}
	for _, n := range lc.Notes {
		f.o.Logf("config: %s", n)
	}
	f.cfgMu.Lock()
	f.lc = lc
	f.applyLocked()
	f.cfgMu.Unlock()
	f.o.Logf("config: reloaded %s", f.o.ConfigFile)
}

// stop shuts everything down: the uplinks flush and spill their unsent data
// to the spool.
func (f *feeder) stop() {
	f.uiMu.Lock()
	if f.ui != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = f.ui.Shutdown(ctx)
		cancel()
		f.ui = nil
	}
	f.uiMu.Unlock()
	f.um.Close()
	f.cancel()
	f.fm.Close()
	f.results.close()
	f.wg.Wait()
	f.o.Logf("airview-feeder stopped")
}

// ---- feederui.Backend ------------------------------------------------------

// Snapshot implements feederui.Backend.
func (f *feeder) Snapshot() feederui.Snapshot {
	f.cfgMu.Lock()
	eff := f.lc.Eff
	notes := append([]string(nil), f.lc.Notes...)
	for _, is := range f.lc.Issues {
		notes = append(notes, is.String())
	}
	f.cfgMu.Unlock()
	f.uiMu.Lock()
	if f.uiErr != "" {
		notes = append(notes, "status page: "+f.uiErr)
	}
	f.uiMu.Unlock()

	now := time.Now()
	s := feederui.Snapshot{
		Software: avf.SoftwareFeeder, Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		FeederID: f.id.FeederID, UUID: f.id.UUID, StartedMs: f.started.UnixMilli(), NowMs: now.UnixMilli(),
		ConfigFile: f.o.ConfigFile, DataDir: f.o.DataDir, Notes: notes,
		Uplinks: f.um.Status(), Beast: f.um.BeastStatus(), Results: f.results.status(),
	}
	if eff.NTP.Enabled {
		ns := f.ntp.Status()
		s.NTP = &ns
	}
	status := map[string]model.FeedStatus{}
	for _, st := range f.fm.Status() {
		status[st.ID] = st
	}
	stats := f.stats.Snapshot()
	for _, r := range eff.Receivers {
		rs := feederui.ReceiverStatus{
			RxNo: r.RxNo, Name: r.Name, FeedID: r.Input.ID, InputType: r.Input.Type, Enabled: r.Input.Enabled,
			Located: r.Located, Lat: r.Lat, Lon: r.Lon, AltM: r.AltM, AltRef: r.AltRef, Stats: stats[r.Input.ID],
		}
		if rs.Stats.FeedID == "" {
			rs.Stats.FeedID = r.Input.ID
		}
		if r.Input.Type != model.FeedAuto && r.Input.Type != model.FeedJSON {
			rs.Addr = net.JoinHostPort(r.Input.Host, fmt.Sprint(r.Input.Port))
		}
		f.detMu.Lock()
		if st := f.det[r.Input.ID]; st != nil && st.found {
			rs.Detected, rs.Addr = st.d.Label, st.d.Addr()
		}
		f.detMu.Unlock()
		if st, ok := status[r.Input.ID]; ok {
			rs.Connected, rs.SinceMs, rs.LastError, rs.Info, rs.GPSTime = st.Connected, st.Since, st.LastError, st.Info, st.GPSTime
		}
		if cs, ok := f.fm.Clock(r.Input.ID); ok {
			if cs.Class != model.ClockNone {
				rs.ClockClass = cs.Class.String()
			}
			if cs.Detected != model.ClockNone {
				rs.ClockDetected = cs.Detected.String()
			}
			rs.Epoch, rs.ClockEvents = cs.Epoch, cs.Events
		}
		s.Receivers = append(s.Receivers, rs)
	}
	return s
}

// Claim implements feederui.Backend.
func (f *feeder) Claim(ctx context.Context, uplinkID string, rxNo int) (string, time.Time, error) {
	return f.um.Claim(ctx, uplinkID, rxNo)
}

// Retry implements feederui.Backend.
func (f *feeder) Retry(uplinkID string) error { return f.um.Retry(uplinkID) }

// Settings implements feederui.Backend.
func (f *feeder) Settings() feederui.SettingsDoc {
	f.cfgMu.Lock()
	defer f.cfgMu.Unlock()
	return feederui.SettingsDoc{
		Config: redactConfig(f.lc.FileCfg, feederui.Redacted),
		File:   f.o.ConfigFile,
		Locked: append([]string(nil), f.lc.Locked...),
	}
}

// SaveSettings implements feederui.Backend.
func (f *feeder) SaveSettings(in model.FeederConfig) ([]string, error) {
	f.cfgMu.Lock()
	defer f.cfgMu.Unlock()
	cfg := mergeSecrets(in, f.lc.FileCfg, feederui.Redacted)
	// The page cannot change where it listens or its password: that needs
	// the configuration file (a page bound to the LAN by mistake would be
	// hard to undo from a browser that can no longer reach it).
	cfg.UI = f.lc.FileCfg.UI
	// SEC-R1: the release manifest URL and keys (and the health URL the
	// installer polls) are trust anchors written by the installer or an
	// administrator. The status page is open to every local user (loopback
	// needs no login), and on Windows a SYSTEM task installs what they
	// point to, so the page never changes them. The channel and the
	// automatic switch stay page settings ("Software updates"): they only
	// choose among releases signed by the trusted keys.
	anchorsChanged := lockUpdateAnchors(&cfg, f.lc.FileCfg)
	if len(cfg.Receivers) == 0 {
		return nil, errors.New("at least one receiver is required")
	}
	notes := normalizeFeeder(&cfg)
	if anchorsChanged {
		notes = append(notes, "The update manifest URL, release keys and health URL were not changed: "+
			"they are set by the installer or in the configuration file by an administrator.")
	}
	if err := saveConfig(f.o.ConfigFile, cfg); err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", f.o.ConfigFile, err)
	}
	f.lc.Exists = true
	f.lc.reapply(cfg)
	f.applyLocked()
	return notes, nil
}

// lockUpdateAnchors restores the update trust anchors of cfg (manifest
// URL, release keys, health URL) from the file's (SEC-R1) and reports
// whether the request tried to change them.
func lockUpdateAnchors(cfg *model.FeederConfig, file model.FeederConfig) bool {
	changed := cfg.Update.ManifestURL != file.Update.ManifestURL || cfg.Update.HealthURL != file.Update.HealthURL ||
		!sameKeys(cfg.Update.Keys, file.Update.Keys)
	cfg.Update.ManifestURL, cfg.Update.HealthURL = file.Update.ManifestURL, file.Update.HealthURL
	cfg.Update.Keys = append([]string(nil), file.Update.Keys...)
	return changed
}

// sameKeys compares two key lists (nil equals empty).
func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ feederui.Backend = (*feeder)(nil)
