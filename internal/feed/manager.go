package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/config"
	"airview/internal/hub"
	"airview/internal/model"
	"airview/internal/ntp"
)

// Runner is one running feed instance. Run delivers data to s (which is a
// model.FrameSink) and reports its connection state through s until ctx is
// cancelled. Built-in runners reconnect internally and only return when ctx
// is done; if a Runner returns earlier the Manager restarts it with backoff.
type Runner interface {
	Run(ctx context.Context, s *Session) error
}

// RunnerFunc adapts a function to the Runner interface.
type RunnerFunc func(ctx context.Context, s *Session) error

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, s *Session) error { return f(ctx, s) }

// Factory creates the Runner for one configured feed. It is called every
// time the feed is (re)started. Returning an error leaves the feed stopped
// with that error as FeedStatus.LastError.
type Factory func(cfg model.Feed) (Runner, error)

// Options tunes a Manager. Zero values select the defaults.
type Options struct {
	DialTimeout  time.Duration // TCP connect timeout, default 10 s
	IdleTimeout  time.Duration // reconnect when no data arrives for this long, default 60 s
	MinBackoff   time.Duration // first reconnect delay, default 1 s
	MaxBackoff   time.Duration // reconnect delay ceiling, default 30 s
	JSONInterval time.Duration // polling period of "json" feeds, default 1 s
	Logger       *slog.Logger  // default slog.Default()
}

func (o *Options) setDefaults() {
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 60 * time.Second
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 30 * time.Second
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = o.MinBackoff
	}
	if o.JSONInterval <= 0 {
		o.JSONInterval = time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// HubOptions are the settings shared by all feeds of type "hub" (the
// model.Feed struct has no room for them).
type HubOptions struct {
	// Password is the host's data port password (Config.Remote.Password).
	Password string
	// KeepHostFeedIDs delivers frames with the host's own feed IDs instead
	// of the local hub feed's ID (see hub.Station.Feeds for their meaning).
	KeepHostFeedIDs bool
}

// Manager runs the configured feeds and keeps them in line with the
// configuration. All methods are safe for concurrent use.
type Manager struct {
	sink model.FrameSink
	opts Options
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu         sync.Mutex
	closed     bool
	stopParent func() bool
	factories  map[string]Factory
	typeGen    map[string]uint64 // bumped when a type's factory or shared options change
	desired    []model.Feed
	order      []string
	feeds      map[string]*entry
	hubOpts    HubOptions
	ticking    bool

	// fwd maps every configured feed ID to its forwarding permission
	// (model.Feed.Forwardable); replaced wholesale on every reconcile.
	fwd atomic.Pointer[map[string]bool]

	handlers atomic.Pointer[handlers]
}

type handlers struct {
	station  func(feedID string, st hub.Station)
	position func(feedID string, pos ReceiverPosition)
	clock    func(feedID string, ev ClockEvent)
}

// entry is one configured feed.
type entry struct {
	cfg     model.Feed
	sess    *Session
	gen     uint64
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// New returns a Manager delivering to sink, with default options. Feeds
// start with the first Apply. Bind the Manager to a context with Start, or
// stop it with Close.
func New(sink model.FrameSink) *Manager { return NewWithOptions(sink, Options{}) }

// NewWithOptions is New with explicit tunables.
func NewWithOptions(sink model.FrameSink, opts Options) *Manager {
	opts.setDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		sink:      sink,
		opts:      opts,
		log:       opts.Logger.With("component", "feed"),
		ctx:       ctx,
		cancel:    cancel,
		factories: make(map[string]Factory),
		typeGen:   make(map[string]uint64),
		feeds:     make(map[string]*entry),
	}
	m.handlers.Store(&handlers{})
	m.factories[model.FeedBeast] = func(cfg model.Feed) (Runner, error) { return newStreamRunner(cfg, streamBeast) }
	m.factories[model.FeedAVR] = func(cfg model.Feed) (Runner, error) { return newStreamRunner(cfg, streamAVR) }
	m.factories[model.FeedSBS] = func(cfg model.Feed) (Runner, error) { return newStreamRunner(cfg, streamSBS) }
	m.factories[model.FeedJSON] = func(cfg model.Feed) (Runner, error) {
		if _, err := jsonCandidates(cfg); err != nil {
			return nil, err
		}
		return newJSONRunner(cfg, m.opts.JSONInterval), nil
	}
	m.factories[model.FeedHub] = func(cfg model.Feed) (Runner, error) { return m.newHubRunner(cfg) }
	return m
}

// Start ties the Manager's lifetime to ctx: when ctx is cancelled all feeds
// are stopped as if Close had been called. It does not block.
func (m *Manager) Start(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() { m.Close() })
	m.mu.Lock()
	if m.stopParent != nil {
		m.stopParent()
	}
	m.stopParent = stop
	m.mu.Unlock()
}

// Close stops all feeds and waits for their goroutines to finish. After
// Close, Apply is a no-op. Safe to call more than once.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	if m.stopParent != nil {
		m.stopParent()
		m.stopParent = nil
	}
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}

// RegisterType installs (or replaces) the factory for a feed type, e.g.
// the engine's simulator for model.FeedDemo. Feeds of that type that are
// already configured are (re)started with the new factory.
func (m *Manager) RegisterType(typ string, f Factory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f == nil {
		delete(m.factories, typ)
	} else {
		m.factories[typ] = f
	}
	m.typeGen[typ]++
	m.reconcileLocked()
}

// SetHubOptions sets the password and feed-ID policy used by "hub" feeds.
// Running hub feeds reconnect when the options changed.
func (m *Manager) SetHubOptions(o HubOptions) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o == m.hubOpts {
		return
	}
	m.hubOpts = o
	m.typeGen[model.FeedHub]++
	m.reconcileLocked()
}

// OnStation registers the callback that receives the host's station
// metadata (receiver, antennas, feeds) from "hub" feeds: after every login
// and whenever the host changes it. The callback runs on the feed's
// goroutine and must not block for long.
func (m *Manager) OnStation(fn func(feedID string, st hub.Station)) {
	for {
		old := m.handlers.Load()
		h := *old
		h.station = fn
		if m.handlers.CompareAndSwap(old, &h) {
			return
		}
	}
}

// OnReceiverPosition registers the callback that receives the receiver's
// own GPS position whenever a Radarcape reports it (Beast type '5' frame).
func (m *Manager) OnReceiverPosition(fn func(feedID string, pos ReceiverPosition)) {
	for {
		old := m.handlers.Load()
		h := *old
		h.position = fn
		if m.handlers.CompareAndSwap(old, &h) {
			return
		}
	}
}

// OnClockEvent registers the callback that receives every new receiver
// clock epoch of a feed (Beast and AVR inputs): the first epoch of a
// connection (reason restart), steps (jump, backwards) and class changes
// such as GPS lock lost or regained (mode_change). It runs on the feed's
// goroutine before the first frame of the new epoch is delivered to the
// sink, so a consumer that handles both on one ordered path sees the event
// first. It must not block for long.
func (m *Manager) OnClockEvent(fn func(feedID string, ev ClockEvent)) {
	for {
		old := m.handlers.Load()
		h := *old
		h.clock = fn
		if m.handlers.CompareAndSwap(old, &h) {
			return
		}
	}
}

// Forwardable reports whether frames of the configured feed feedID may be
// sent on an uplink (DESIGN §1): the feed must be configured and neither
// MLATOnly nor NoForward (model.Feed.Forwardable). Beyond that rule,
// simulator ("demo") feeds and "hub" feeds are never forwardable: the
// former is synthetic data, the latter was received by another station and
// would be attributed to this one. Unknown feed IDs (for example host feed
// IDs kept by a hub feed) are not forwardable either. Safe for concurrent
// use and cheap enough to call per frame.
func (m *Manager) Forwardable(feedID string) bool {
	if p := m.fwd.Load(); p != nil {
		return (*p)[feedID]
	}
	return false
}

// ForwardableFrame applies the per-frame part of the uplink rule (DESIGN
// §1, AVF §5.5): MLAT results are never forwarded; a frame without a usable
// receiver time (all-zero timestamp, unknown mode, unconfirmed step) may
// only be sent as untimed, which this reports through timed=false. Combine
// it with Manager.Forwardable(f.FeedID).
func ForwardableFrame(f *model.RawFrame) (ok, timed bool) {
	if f.MLAT || f.Kind == model.FrameStatus {
		return false, false
	}
	return true, f.ClockCls.Timed()
}

// feedForwardable is the per-feed uplink rule of Manager.Forwardable.
func feedForwardable(cfg model.Feed) bool {
	return cfg.Forwardable() && cfg.Type != model.FeedDemo && cfg.Type != model.FeedHub
}

// Clock returns the receiver clock state of the configured feed feedID.
func (m *Manager) Clock(feedID string) (ClockState, bool) {
	m.mu.Lock()
	e := m.feeds[feedID]
	m.mu.Unlock()
	if e == nil {
		return ClockState{}, false
	}
	return e.sess.Clock(), true
}

// Apply makes the running feeds match the given configuration: feeds that
// disappeared or were disabled are stopped, new ones started, and feeds
// whose definition changed are restarted. Unchanged feeds keep running
// undisturbed (their counters too). Apply does not block on the network.
func (m *Manager) Apply(feeds []model.Feed) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.desired = append([]model.Feed(nil), feeds...)
	if !m.ticking {
		m.ticking = true
		m.wg.Add(1)
		go m.rateLoop()
	}
	m.reconcileLocked()
}

func (m *Manager) reconcileLocked() {
	if m.closed {
		return
	}
	want := make(map[string]struct{}, len(m.desired))
	order := make([]string, 0, len(m.desired))
	for _, cfg := range m.desired {
		if cfg.ID == "" {
			m.log.Warn("ignoring feed without an ID", "name", cfg.Name)
			continue
		}
		if _, dup := want[cfg.ID]; dup {
			m.log.Warn("ignoring feed with a duplicate ID", "id", cfg.ID)
			continue
		}
		want[cfg.ID] = struct{}{}
		order = append(order, cfg.ID)

		e := m.feeds[cfg.ID]
		gen := m.typeGen[cfg.Type]
		if e != nil && e.cfg == cfg && e.gen == gen && e.running == cfg.Enabled {
			continue
		}
		var prev chan struct{}
		if e != nil {
			prev = m.stopLocked(e)
		}
		ne := &entry{cfg: cfg, gen: gen, sess: newSession(m, cfg)}
		if e != nil {
			// The receiver behind the feed may be the same: keep its
			// epoch numbering increasing.
			ne.sess.epochCarry.Store(e.sess.lastEpoch())
		}
		if e != nil && e.cfg == cfg {
			// Same definition (restart because of a factory / option change
			// or a previously failed start): keep the counters.
			ne.sess.frames.Store(e.sess.frames.Load())
			ne.sess.bytes.Store(e.sess.bytes.Load())
			ne.sess.lastFrame.Store(e.sess.lastFrame.Load())
		}
		m.feeds[cfg.ID] = ne
		if cfg.Enabled {
			m.startLocked(ne, prev)
		}
	}
	for id, e := range m.feeds {
		if _, ok := want[id]; !ok {
			m.stopLocked(e)
			delete(m.feeds, id)
		}
	}
	m.order = order
	fwd := make(map[string]bool, len(order))
	for _, cfg := range m.desired {
		if _, ok := want[cfg.ID]; ok {
			if _, dup := fwd[cfg.ID]; !dup {
				fwd[cfg.ID] = feedForwardable(cfg)
			}
		}
	}
	m.fwd.Store(&fwd)
}

// stopLocked cancels a running entry and returns the channel that is closed
// once its goroutine has finished (nil when it was not running).
func (m *Manager) stopLocked(e *entry) chan struct{} {
	if !e.running {
		return nil
	}
	e.running = false
	e.cancel()
	return e.done
}

func (m *Manager) startLocked(e *entry, prev chan struct{}) {
	factory := m.factories[e.cfg.Type]
	if factory == nil {
		e.sess.SetDisconnected(fmt.Errorf("unknown feed type %q", e.cfg.Type))
		return
	}
	runner, err := factory(e.cfg)
	if err != nil {
		e.sess.SetDisconnected(err)
		return
	}
	if runner == nil {
		e.sess.SetDisconnected(fmt.Errorf("feed type %q produced no runner", e.cfg.Type))
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	e.cancel = cancel
	e.done = make(chan struct{})
	e.running = true
	m.wg.Add(1)
	go m.supervise(ctx, e.sess, runner, prev, e.done)
}

// supervise runs one feed until its context is cancelled.
func (m *Manager) supervise(ctx context.Context, s *Session, r Runner, prev <-chan struct{}, done chan<- struct{}) {
	defer m.wg.Done()
	defer close(done)
	defer s.stopped()

	if prev != nil {
		// Never run two instances of the same feed side by side.
		t := time.NewTimer(10 * time.Second)
		select {
		case <-prev:
		case <-ctx.Done():
		case <-t.C:
			m.log.Warn("previous instance of feed did not stop in time", "feed", s.cfg.ID)
		}
		t.Stop()
	}
	bo := s.newBackoff()
	for ctx.Err() == nil {
		start := time.Now()
		err := runGuarded(ctx, r, s)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("feed ended unexpectedly")
		}
		s.SetDisconnected(err)
		if time.Since(start) > time.Minute {
			bo.reset()
		}
		if !sleepCtx(ctx, bo.next()) {
			return
		}
	}
}

// runGuarded runs r and converts a panic into an error so that one broken
// feed cannot take the whole station down.
func runGuarded(ctx context.Context, r Runner, s *Session) (err error) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("feed panicked", "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("feed panicked: %v", p)
		}
	}()
	return r.Run(ctx, s)
}

// rateLoop refreshes the message-rate averages once a second.
func (m *Manager) rateLoop() {
	defer m.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-t.C:
			m.mu.Lock()
			for _, e := range m.feeds {
				e.sess.tick(now)
			}
			m.mu.Unlock()
		}
	}
}

// Status returns the state of every configured feed, in configuration order.
func (m *Manager) Status() []model.FeedStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.FeedStatus, 0, len(m.order))
	for _, id := range m.order {
		if e := m.feeds[id]; e != nil {
			out = append(out, e.sess.status())
		}
	}
	return out
}

// --- Session ------------------------------------------------------------------

// Session is the Manager's handle for one running feed. It is the
// model.FrameSink a Runner delivers into (it counts and forwards to the
// Manager's sink) and the place where the Runner reports connection state.
// All methods are safe for concurrent use.
type Session struct {
	m    *Manager
	cfg  model.Feed
	sink model.FrameSink
	log  *slog.Logger

	frames    atomic.Uint64
	bytes     atomic.Uint64
	lastFrame atomic.Int64 // Unix ms

	escKnown   atomic.Uint64 // Beast extension messages seen
	escUnknown atomic.Uint64 // Beast escapes of unknown type seen
	epochCarry atomic.Uint32 // last epoch of the previous session of this feed ID

	mu         sync.Mutex
	addr       string
	connected  bool
	since      int64
	lastErr    string
	gpsTime    bool
	info       string
	rate       float64
	rateInit   bool
	prevFrames uint64
	prevTick   time.Time
	clock      ClockState
}

// ClockState is the receiver clock state of a feed (DESIGN §4.3).
type ClockState struct {
	Class    model.ClockClass // effective class (override applied)
	Detected model.ClockClass // class detected from the stream
	Epoch    uint32           // current / last epoch (0 = none yet)
	Events   uint64           // clock events so far in this session
	Last     ClockEvent       // most recent event (valid when Events > 0)
	// Beast extension messages seen on the input (readsb receiver ID,
	// UUID, ping, option) and escapes of unknown type, both skipped.
	Escapes, UnknownEscapes uint64
}

// Clock returns the feed's receiver clock state.
func (s *Session) Clock() ClockState {
	s.mu.Lock()
	c := s.clock
	s.mu.Unlock()
	c.Escapes, c.UnknownEscapes = s.escKnown.Load(), s.escUnknown.Load()
	return c
}

// Forwardable reports whether this feed's frames may be sent on an uplink
// (see Manager.Forwardable).
func (s *Session) Forwardable() bool { return feedForwardable(s.cfg) }

// lastEpoch returns the newest clock epoch of this feed, carried over from
// a previous session of the same feed ID when this one has none yet.
func (s *Session) lastEpoch() uint32 {
	s.mu.Lock()
	ep := s.clock.Epoch
	s.mu.Unlock()
	if ep == 0 {
		ep = s.epochCarry.Load()
	}
	return ep
}

// clockOverride parses the configured clock class (model.Feed.ClockClass).
func (s *Session) clockOverride() (model.ClockClass, bool) {
	name := strings.TrimSpace(s.cfg.ClockClass)
	if name == "" {
		return model.ClockNone, false
	}
	cls, ok := model.ParseClockClass(name)
	if !ok {
		s.log.Warn("unknown clock class in feed configuration; detection is used", "clockClass", name)
		return model.ClockNone, false
	}
	return cls, true
}

// setClockClass records the effective and detected clock class.
func (s *Session) setClockClass(cls, detected model.ClockClass) {
	s.mu.Lock()
	s.clock.Class, s.clock.Detected = cls, detected
	s.mu.Unlock()
}

// clockEvent records a new clock epoch and passes it to the Manager's
// OnClockEvent callback. Runners of custom feed types that produce timed
// frames report their epochs through ReportClockEvent.
func (s *Session) clockEvent(ev ClockEvent) {
	s.mu.Lock()
	s.clock.Class, s.clock.Epoch = ev.NewClass, ev.NewEpoch
	s.clock.Events++
	s.clock.Last = ev
	s.mu.Unlock()
	lvl := slog.LevelInfo
	if ev.Reason == model.ClockEvJump || ev.Reason == model.ClockEvBackwards || ev.Detail == ClockDetailGPSLost {
		lvl = slog.LevelWarn
	}
	s.log.Log(context.Background(), lvl, "receiver clock epoch", "epoch", ev.NewEpoch, "previous", ev.OldEpoch,
		"reason", string(ev.Reason), "detail", ev.Detail, "class", ev.NewClass.String(), "stepNs", ev.StepNs)
	if fn := s.m.handlers.Load().clock; fn != nil {
		fn(s.cfg.ID, ev)
	}
}

// ReportClockEvent lets a custom Runner (see Manager.RegisterType) report a
// new clock epoch of its timed frames; see Manager.OnClockEvent.
func (s *Session) ReportClockEvent(ev ClockEvent) { s.clockEvent(ev) }

func newSession(m *Manager, cfg model.Feed) *Session {
	return &Session{
		m:     m,
		cfg:   cfg,
		sink:  m.sink,
		log:   m.log.With("feed", cfg.ID, "type", cfg.Type),
		addr:  feedAddr(cfg),
		since: time.Now().UnixMilli(),
	}
}

// Feed returns the feed definition this session runs.
func (s *Session) Feed() model.Feed { return s.cfg }

// HandleFrame implements model.FrameSink: it fills in FeedID when empty,
// applies Feed.MLATOnly, counts the frame and forwards it.
func (s *Session) HandleFrame(f model.RawFrame) {
	if f.FeedID == "" {
		f.FeedID = s.cfg.ID
	}
	if s.cfg.MLATOnly && f.Kind != model.FrameStatus {
		f.MLAT = true
	}
	if f.Recv.IsZero() {
		f.Recv = ntp.Now()
	}
	s.frames.Add(1)
	s.lastFrame.Store(f.Recv.UnixMilli())
	s.sink.HandleFrame(f)
}

// HandleUpdate implements model.FrameSink for pre-decoded updates.
func (s *Session) HandleUpdate(u model.DecodedUpdate) {
	if u.FeedID == "" {
		u.FeedID = s.cfg.ID
	}
	if s.cfg.MLATOnly {
		u.MLAT = true
	}
	if u.Recv.IsZero() {
		u.Recv = ntp.Now()
	}
	if u.Msgs < 1 {
		u.Msgs = 1
	}
	s.frames.Add(1)
	s.lastFrame.Store(u.Recv.UnixMilli())
	s.sink.HandleUpdate(u)
}

// AddBytes adds n to the feed's received-bytes counter.
func (s *Session) AddBytes(n int) {
	if n > 0 {
		s.bytes.Add(uint64(n))
	}
}

// SetConnected marks the feed as connected to addr and clears the last error.
func (s *Session) SetConnected(addr string) {
	s.mu.Lock()
	changed := !s.connected
	s.connected = true
	if addr != "" {
		s.addr = addr
	}
	s.lastErr = ""
	if changed {
		s.since = time.Now().UnixMilli()
	}
	s.mu.Unlock()
	if changed {
		s.log.Info("feed connected", "addr", config.RedactURL(addr))
	}
}

// SetDisconnected marks the feed as not connected; err (may be nil) becomes
// FeedStatus.LastError.
func (s *Session) SetDisconnected(err error) {
	msg := ""
	if err != nil {
		msg = describeErr(err, s.m.opts.IdleTimeout)
	}
	s.mu.Lock()
	was := s.connected
	changed := was || msg != s.lastErr
	s.connected = false
	if msg != "" {
		s.lastErr = msg
	}
	if was {
		s.since = time.Now().UnixMilli()
	}
	s.mu.Unlock()
	if changed && msg != "" { // log state changes, not every retry
		s.log.Warn("feed not connected", "err", msg)
	}
}

// SetGPSTime reports whether the feed delivers Radarcape GPS timestamps.
func (s *Session) SetGPSTime(on bool) {
	s.mu.Lock()
	s.gpsTime = on
	s.mu.Unlock()
}

// SetInfo sets the free-text receiver status shown in FeedStatus.Info.
func (s *Session) SetInfo(info string) {
	s.mu.Lock()
	s.info = info
	s.mu.Unlock()
}

// ReportPosition forwards the receiver's own GPS position to the
// Manager's OnReceiverPosition callback.
func (s *Session) ReportPosition(pos ReceiverPosition) {
	if fn := s.m.handlers.Load().position; fn != nil {
		fn(s.cfg.ID, pos)
	}
}

// stopped runs when the feed's goroutine ends.
func (s *Session) stopped() {
	s.mu.Lock()
	if s.connected {
		s.connected = false
		s.since = time.Now().UnixMilli()
	}
	s.mu.Unlock()
}

// tick updates the exponentially weighted message rate (time constant of
// roughly 3 s at the 1 Hz tick).
func (s *Session) tick(now time.Time) {
	n := s.frames.Load()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prevTick.IsZero() {
		s.prevTick, s.prevFrames = now, n
		return
	}
	dt := now.Sub(s.prevTick).Seconds()
	if dt <= 0 {
		return
	}
	inst := float64(n-s.prevFrames) / dt
	s.prevTick, s.prevFrames = now, n
	if !s.rateInit {
		s.rate, s.rateInit = inst, true
	} else {
		s.rate += 0.3 * (inst - s.rate)
	}
	if s.rate < 0.01 {
		s.rate = 0
	}
}

func (s *Session) status() model.FeedStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return model.FeedStatus{
		ID:        s.cfg.ID,
		Name:      s.cfg.Name,
		Type:      s.cfg.Type,
		Addr:      config.RedactURL(s.addr),
		Enabled:   s.cfg.Enabled,
		Connected: s.connected,
		Since:     s.since,
		Frames:    s.frames.Load(),
		Bytes:     s.bytes.Load(),
		MsgRate:   float64(int(s.rate*10+0.5)) / 10,
		LastFrame: s.lastFrame.Load(),
		LastError: scrubURLs(s.lastErr),
		GPSTime:   s.gpsTime,
		Info:      s.info,
	}
}

// feedAddr is the configured (not yet resolved) address shown in the status.
func feedAddr(cfg model.Feed) string {
	switch cfg.Type {
	case model.FeedJSON:
		if c, err := jsonCandidates(cfg); err == nil && len(c) == 1 {
			return c[0]
		}
		if cfg.URL != "" {
			return cfg.URL
		}
		return cfg.Host
	case model.FeedDemo:
		return "simulator"
	}
	if cfg.Host == "" {
		return ""
	}
	return net.JoinHostPort(cfg.Host, fmt.Sprint(feedPort(cfg)))
}

// feedPort returns the configured port or the conventional default of the
// feed type.
func feedPort(cfg model.Feed) int {
	if cfg.Port > 0 {
		return cfg.Port
	}
	switch cfg.Type {
	case model.FeedBeast:
		return 10003 // Radarcape: binary, all Mode S frames, CRC pre-checked
	case model.FeedAVR:
		return 30002 // dump1090 / readsb raw output
	case model.FeedSBS:
		return 30003
	case model.FeedHub:
		return hub.DefaultPort
	}
	return 0
}

// describeErr turns low-level network errors into text fit for the UI.
func describeErr(err error, idle time.Duration) string {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Sprintf("no data for %s", idle)
	case errors.Is(err, io.EOF):
		return "connection closed by peer"
	}
	return scrubURLs(err.Error())
}

var urlInText = regexp.MustCompile(`https?://[^\s"'<>]+`)

// scrubURLs removes credentials and query strings from every http(s) URL in
// a message (feed URLs may embed user:password or an apikey parameter, and
// net/http errors repeat the full URL).
func scrubURLs(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlInText.ReplaceAllStringFunc(s, config.RedactURL)
}

// --- reconnect helpers --------------------------------------------------------

// backoff produces exponentially growing, jittered delays.
type backoff struct {
	min, max, cur time.Duration
}

func (s *Session) newBackoff() backoff {
	return backoff{min: s.m.opts.MinBackoff, max: s.m.opts.MaxBackoff}
}

// next returns the next delay: between 50 % and 100 % of the nominal value,
// which doubles on every call from min up to max.
func (b *backoff) next() time.Duration {
	if b.min <= 0 {
		b.min = time.Second
	}
	if b.max < b.min {
		b.max = 30 * time.Second
		if b.max < b.min {
			b.max = b.min
		}
	}
	if b.cur < b.min {
		b.cur = b.min
	}
	d := b.cur
	if b.cur *= 2; b.cur > b.max {
		b.cur = b.max
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func (b *backoff) reset() { b.cur = 0 }

// sleepCtx waits for d or until ctx is done; it reports false in the latter case.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// dialLoop keeps a TCP connection to addr alive until ctx is cancelled:
// connect (keepalive enabled), run handle on the connection, and after any
// failure reconnect with exponential backoff and jitter. The backoff starts
// over once a connection has delivered data. handle must return when the
// connection fails; the connection is closed when ctx is cancelled.
func (s *Session) dialLoop(ctx context.Context, addr string, handle func(conn net.Conn) error) error {
	bo := s.newBackoff()
	d := net.Dialer{Timeout: s.m.opts.DialTimeout, KeepAlive: 30 * time.Second}
	for {
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			s.SetConnected(conn.RemoteAddr().String())
			before := s.bytes.Load()
			err = handle(conn)
			stop()
			conn.Close()
			if s.bytes.Load() != before {
				bo.reset()
			}
			if err == nil {
				err = io.EOF
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.SetDisconnected(err)
		if !sleepCtx(ctx, bo.next()) {
			return ctx.Err()
		}
	}
}
