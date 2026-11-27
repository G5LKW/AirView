package weather

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"airview/internal/model"
)

// Poller limits.
const (
	DefaultIntervalMin = 15  // model.WeatherConfig default
	MinIntervalMin     = 5   // shortest accepted refresh period
	MaxIntervalMin     = 360 // longest accepted refresh period
)

// Errors returned by Poller.Refresh.
var (
	// ErrDisabled: weather ingestion is switched off in the configuration.
	ErrDisabled = errors.New("weather: disabled in configuration")
	// ErrNotLocated: the station has no coordinates yet.
	ErrNotLocated = errors.New("weather: station location is not set")
	// ErrSuperseded: the station moved while the request was in flight.
	ErrSuperseded = errors.New("weather: location changed during the request")
)

// PollerOptions tunes a Poller. The zero value is fine.
type PollerOptions struct {
	// Client defaults to NewClient(Options{}).
	Client *Client
	// Forecast selects what each poll downloads; nil = DefaultForecastOptions()
	// (current + 2 past days + 7 forecast days + profiles, 13.0 weighted calls).
	Forecast *ForecastOptions
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// PollerStatus is a snapshot of the poller state for the UI.
type PollerStatus struct {
	Enabled     bool   `json:"enabled"`             // WeatherConfig.Enabled
	Located     bool   `json:"located"`             // station has valid coordinates
	IntervalMin int    `json:"intervalMin"`         // effective refresh period
	LastAttempt int64  `json:"lastAttempt"`         // Unix ms, 0 = never
	LastOK      int64  `json:"lastOk"`              // Unix ms, 0 = never
	LastError   string `json:"lastError,omitempty"` //
	Failures    int    `json:"failures"`            // consecutive failed polls
	NextPoll    int64  `json:"nextPoll"`            // Unix ms of the next scheduled poll, 0 = none

	RadarEnabled bool   `json:"radarEnabled"`
	RadarLastOK  int64  `json:"radarLastOk"`
	RadarError   string `json:"radarError,omitempty"`

	Usage Usage `json:"usage"` // Open-Meteo call accounting
}

// Poller keeps the latest weather Bundle (and optionally the RainViewer frame
// index) fresh. It does nothing while weather is disabled or the station is
// not located. Safe for concurrent use.
type Poller struct {
	c    *Client
	fo   ForecastOptions
	log  *slog.Logger
	wake chan struct{} // schedule changed
	sem  chan struct{} // serialises weather fetches (Run loop vs Refresh)

	// Timing knobs (shortened by tests).
	backoffBase time.Duration // first retry delay after a failed poll
	radarEvery  time.Duration // RainViewer index refresh period
	refreshGap  time.Duration // manual refreshes closer together than this reuse the last bundle
	intervalFn  func(model.WeatherConfig) time.Duration

	mu          sync.Mutex
	cfg         model.WeatherConfig
	rx          model.Receiver
	gen         uint64 // bumped when the location changes
	latest      *Bundle
	radar       *Radar
	onUpdate    func(*Bundle)
	onRadar     func(*Radar)
	nextWx      time.Time // zero = as soon as possible
	nextRadar   time.Time
	lastAttempt time.Time
	lastOK      time.Time
	lastErr     string
	failures    int
	radarOK     time.Time
	radarErr    string
	radarFails  int
}

// NewPoller creates a stopped Poller for the given configuration and station.
// Call Run to start it.
func NewPoller(cfg model.WeatherConfig, rx model.Receiver, o PollerOptions) *Poller {
	p := &Poller{
		c:           o.Client,
		log:         o.Logger,
		wake:        make(chan struct{}, 1),
		sem:         make(chan struct{}, 1),
		backoffBase: 30 * time.Second,
		radarEvery:  5 * time.Minute,
		refreshGap:  20 * time.Second,
		intervalFn:  pollInterval,
		cfg:         cfg,
		rx:          rx,
	}
	if p.c == nil {
		p.c = NewClient(Options{Logger: o.Logger})
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	if o.Forecast != nil {
		p.fo = *o.Forecast
	} else {
		p.fo = DefaultForecastOptions()
	}
	return p
}

// pollInterval returns the effective refresh period for cfg.
func pollInterval(cfg model.WeatherConfig) time.Duration {
	m := cfg.IntervalMin
	if m <= 0 {
		m = DefaultIntervalMin
	}
	m = min(max(m, MinIntervalMin), MaxIntervalMin)
	return time.Duration(m) * time.Minute
}

// Client returns the underlying API client (history backfill, elevation
// lookup and usage accounting share its request budget).
func (p *Poller) Client() *Client { return p.c }

// OnUpdate registers fn to be called with every freshly downloaded Bundle
// (from the polling goroutine or the Refresh caller, in fetch order). It
// replaces any previous callback. The callback must not block for long and
// must not call Refresh.
func (p *Poller) OnUpdate(fn func(*Bundle)) {
	p.mu.Lock()
	p.onUpdate = fn
	p.mu.Unlock()
}

// OnRadar registers fn to be called whenever the radar frame index changes.
func (p *Poller) OnRadar(fn func(*Radar)) {
	p.mu.Lock()
	p.onRadar = fn
	p.mu.Unlock()
}

// Latest returns the most recent Bundle for the current station location, or
// nil when none has been fetched yet. The Bundle must be treated as read-only.
// While weather is disabled the last Bundle is kept (check Bundle.Fetched).
func (p *Poller) Latest() *Bundle {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latest
}

// Radar returns the latest RainViewer frame index, or nil when the radar
// overlay is disabled or nothing has been fetched yet.
func (p *Poller) Radar() *Radar {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.radar
}

func located(rx model.Receiver) bool { return rx.Located && validCoord(rx.Lat, rx.Lon) }

func sameLocation(a, b model.Receiver) bool {
	return located(a) == located(b) && fmtCoord(a.Lat) == fmtCoord(b.Lat) && fmtCoord(a.Lon) == fmtCoord(b.Lon)
}

// Reconfigure applies new settings live. A changed station location discards
// the current Bundle and fetches a new one immediately; enabling fetches as
// soon as the data is older than the interval; an interval change reschedules.
func (p *Poller) Reconfigure(cfg model.WeatherConfig, rx model.Receiver) {
	p.mu.Lock()
	old, oldRx := p.cfg, p.rx
	p.cfg, p.rx = cfg, rx
	switch {
	case !sameLocation(oldRx, rx):
		p.gen++
		p.latest = nil
		p.nextWx = time.Time{}
		p.lastOK = time.Time{}
		p.lastErr = ""
		p.failures = 0
	case cfg.Enabled && !old.Enabled, p.intervalFn(cfg) != p.intervalFn(old):
		if p.failures == 0 {
			if p.lastOK.IsZero() {
				p.nextWx = time.Time{}
			} else {
				p.nextWx = p.lastOK.Add(p.intervalFn(cfg))
			}
		}
	}
	radarOn := cfg.Enabled && cfg.RadarOverlay
	if radarOn != (old.Enabled && old.RadarOverlay) {
		p.nextRadar = time.Time{}
		p.radarErr = ""
		p.radarFails = 0
		if !radarOn {
			p.radar = nil
		}
	}
	p.mu.Unlock()
	p.signal()
}

func (p *Poller) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Seed installs a previously persisted Bundle (e.g. restored from the
// database at start-up) so the UI has data immediately and a restart does not
// cost an API request. It is ignored unless the Bundle belongs to the current
// station location and nothing newer is held. The next poll is scheduled one
// interval after the Bundle's fetch time.
func (p *Poller) Seed(b *Bundle) bool {
	if b == nil || b.Kind != KindForecast {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !located(p.rx) || fmtCoord(b.Lat) != fmtCoord(p.rx.Lat) || fmtCoord(b.Lon) != fmtCoord(p.rx.Lon) {
		return false
	}
	if p.latest != nil && p.latest.Fetched >= b.Fetched {
		return false
	}
	fetched := b.FetchedTime()
	if fetched.After(time.Now()) {
		return false
	}
	p.latest = b
	p.lastOK = fetched
	if p.failures == 0 {
		p.nextWx = fetched.Add(p.intervalFn(p.cfg))
	}
	p.signal()
	return true
}

// Status returns a snapshot of the poller state.
func (p *Poller) Status() PollerStatus {
	usage := p.c.Usage()
	p.mu.Lock()
	defer p.mu.Unlock()
	st := PollerStatus{
		Enabled:      p.cfg.Enabled,
		Located:      located(p.rx),
		IntervalMin:  int(p.intervalFn(p.cfg) / time.Minute),
		LastError:    p.lastErr,
		Failures:     p.failures,
		RadarEnabled: p.cfg.Enabled && p.cfg.RadarOverlay,
		RadarError:   p.radarErr,
		Usage:        usage,
	}
	if !p.lastAttempt.IsZero() {
		st.LastAttempt = p.lastAttempt.UnixMilli()
	}
	if !p.lastOK.IsZero() {
		st.LastOK = p.lastOK.UnixMilli()
	}
	if !p.radarOK.IsZero() {
		st.RadarLastOK = p.radarOK.UnixMilli()
	}
	if st.Enabled && st.Located {
		next := p.nextWx
		if now := time.Now(); next.Before(now) {
			next = now
		}
		st.NextPoll = next.UnixMilli()
	}
	return st
}

// Run polls until ctx is cancelled, then returns nil. The first fetch happens
// immediately (when enabled and located); later ones follow the configured
// interval with ±10 % jitter, or an exponential back-off after failures.
func (p *Poller) Run(ctx context.Context) error {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		p.mu.Lock()
		now := time.Now()
		wxOn := p.cfg.Enabled && located(p.rx)
		radarOn := p.cfg.Enabled && p.cfg.RadarOverlay
		wxWait, radarWait := p.nextWx.Sub(now), p.nextRadar.Sub(now)
		p.mu.Unlock()

		if wxOn && wxWait <= 0 {
			_, _ = p.fetch(ctx, false)
			continue
		}
		if radarOn && radarWait <= 0 {
			p.pollRadar(ctx)
			continue
		}
		wait := time.Duration(-1)
		if wxOn {
			wait = wxWait
		}
		if radarOn && (wait < 0 || radarWait < wait) {
			wait = radarWait
		}
		if wait < 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-p.wake:
			}
			continue
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return nil
		case <-p.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Refresh fetches a new Bundle now, outside the regular schedule, and returns
// it. To stay polite it returns the current Bundle instead when that is less
// than 20 seconds old. It also brings the radar index refresh forward.
// Errors: ErrDisabled, ErrNotLocated, ErrSuperseded, ErrBudget, *APIError,
// network errors.
func (p *Poller) Refresh(ctx context.Context) (*Bundle, error) {
	b, err := p.fetch(ctx, true)
	p.mu.Lock()
	p.nextRadar = time.Time{}
	p.mu.Unlock()
	p.signal()
	return b, err
}

// fetch downloads one Bundle and updates state and schedule.
func (p *Poller) fetch(ctx context.Context, manual bool) (*Bundle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-p.sem }()

	p.mu.Lock()
	cfg, rx, gen := p.cfg, p.rx, p.gen
	switch {
	case !cfg.Enabled:
		p.mu.Unlock()
		return nil, ErrDisabled
	case !located(rx):
		p.mu.Unlock()
		return nil, ErrNotLocated
	}
	if manual && p.latest != nil && time.Since(p.lastOK) < p.refreshGap {
		b := p.latest
		p.mu.Unlock()
		return b, nil
	}
	if !manual && time.Now().Before(p.nextWx) {
		// A manual Refresh ran while this scheduled poll was waiting for the
		// semaphore and has already rescheduled: nothing to do.
		b := p.latest
		p.mu.Unlock()
		return b, nil
	}
	p.lastAttempt = time.Now()
	p.mu.Unlock()

	b, err := p.c.Forecast(ctx, rx.Lat, rx.Lon, p.fo)

	p.mu.Lock()
	if p.gen != gen {
		// The station moved meanwhile; Reconfigure already scheduled a new fetch.
		p.mu.Unlock()
		return nil, ErrSuperseded
	}
	now := time.Now()
	interval := p.intervalFn(p.cfg)
	if err != nil {
		if ctx.Err() != nil {
			p.mu.Unlock()
			return nil, ctx.Err()
		}
		p.failures++
		p.lastErr = err.Error()
		delay := p.backoff(err, p.failures, interval)
		p.nextWx = now.Add(delay)
		first := p.failures == 1
		p.mu.Unlock()
		if first {
			p.log.Warn("weather: update failed", "err", err, "retryIn", delay.Round(time.Second))
		} else {
			p.log.Debug("weather: update failed", "err", err, "retryIn", delay.Round(time.Second))
		}
		return nil, err
	}
	recovered := p.failures > 0
	p.failures = 0
	p.lastErr = ""
	p.lastOK = now
	p.latest = b
	p.nextWx = now.Add(jitter(interval, 0.1))
	fn := p.onUpdate
	p.mu.Unlock()

	if recovered {
		p.log.Info("weather: updates recovered")
	}
	p.log.Debug("weather: updated", "hours", len(b.Hourly), "cost", b.Cost)
	if fn != nil {
		fn(b)
	}
	return b, nil
}

// backoff returns the delay before the next attempt after a failure:
// exponential from backoffBase up to the regular interval, or longer when
// the server (HTTP 429) or our own budget says so.
func (p *Poller) backoff(err error, failures int, interval time.Duration) time.Duration {
	d := p.backoffBase
	for i := 1; i < failures && d < interval; i++ {
		d *= 2
	}
	if d > interval {
		d = interval
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.RateLimited() {
		if ae.RetryAfter > d {
			d = ae.RetryAfter
		}
	}
	if errors.Is(err, ErrBudget) && d < interval {
		d = interval
	}
	return jitter(d, 0.1)
}

// jitter spreads d by ±frac.
func jitter(d time.Duration, frac float64) time.Duration {
	return time.Duration(float64(d) * (1 - frac + 2*frac*rand.Float64()))
}

// pollRadar refreshes the RainViewer frame index.
func (p *Poller) pollRadar(ctx context.Context) {
	r, err := p.c.RadarFrames(ctx)
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	now := time.Now()
	if !(p.cfg.Enabled && p.cfg.RadarOverlay) {
		p.mu.Unlock()
		return
	}
	if err != nil {
		p.radarFails++
		p.radarErr = err.Error()
		d := p.radarEvery
		for i := 1; i < p.radarFails && d < 30*time.Minute; i++ {
			d *= 2
		}
		p.nextRadar = now.Add(jitter(d, 0.1))
		first := p.radarFails == 1
		p.mu.Unlock()
		if first {
			p.log.Warn("weather: radar index update failed", "err", err)
		}
		return
	}
	changed := p.radar == nil || p.radar.Generated != r.Generated || len(p.radar.Past) != len(r.Past)
	if !changed {
		a, _ := p.radar.Latest()
		b, _ := r.Latest()
		changed = a != b
	}
	p.radar = r
	p.radarOK = now
	p.radarErr = ""
	p.radarFails = 0
	p.nextRadar = now.Add(jitter(p.radarEvery, 0.1))
	fn := p.onRadar
	p.mu.Unlock()
	if changed && fn != nil {
		fn(r)
	}
}
