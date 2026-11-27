package ntp

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"airview/internal/model"
)

// Sampler defaults and limits.
const (
	DefaultIntervalSec = 64   // model.NTPConfig default
	MinIntervalSec     = 8    // shortest accepted sampling interval
	MaxIntervalSec     = 3600 // longest accepted sampling interval
	DefaultBurst       = 4    // queries per sampling burst
	DefaultHistory     = 8192 // ring capacity: ~6 days at 64 s

	smoothWindow  = 8  // samples in the robust (median) offset filter
	jitterWindow  = 16 // samples used for the RMS jitter
	syncIntervals = 3  // Synced() = good sample within this many intervals
	minHoldover   = 15 * time.Minute
	stepFloor     = 20 * time.Millisecond
)

// Sample is one accepted measurement: the minimum-delay reply of a burst.
// It is the unit handed to OnSample (for persistence in ntp_samples) and
// kept in the in-memory history ring.
type Sample struct {
	T           int64   `json:"t"`               // Unix ms (host clock) of the measurement
	Server      string  `json:"server"`          // configured server string
	Addr        string  `json:"addr,omitempty"`  // "ip:port" of the server that answered
	OffsetMs    float64 `json:"offsetMs"`        // true UTC - host clock, this sample
	DelayMs     float64 `json:"delayMs"`         // round-trip delay of this sample
	Stratum     int     `json:"stratum"`         //
	RefID       string  `json:"refId,omitempty"` //
	Leap        int     `json:"leap"`            // Leap* constant announced by the server
	RootDelayMs float64 `json:"rootDelayMs"`     //
	RootDispMs  float64 `json:"rootDispMs"`      //
	SmoothedMs  float64 `json:"smoothedMs"`      // robust smoothed offset after this sample
	JitterMs    float64 `json:"jitterMs"`        // RMS jitter after this sample
	Sent        int     `json:"sent"`            // queries sent in the burst
	Replies     int     `json:"replies"`         // valid replies in the burst
}

// Time returns the sample time.
func (s Sample) Time() time.Time { return time.UnixMilli(s.T) }

// Stats summarises the history ring over a time window.
type Stats struct {
	Count        int     `json:"count"`
	SpanSec      float64 `json:"spanSec"` // time between first and last sample used
	OffsetMeanMs float64 `json:"offsetMeanMs"`
	OffsetMinMs  float64 `json:"offsetMinMs"`
	OffsetMaxMs  float64 `json:"offsetMaxMs"`
	OffsetStdMs  float64 `json:"offsetStdMs"`
	DelayMeanMs  float64 `json:"delayMeanMs"`
	DelayMinMs   float64 `json:"delayMinMs"`
	DelayMaxMs   float64 `json:"delayMaxMs"`
	// DriftPPM is the least-squares slope of offset against time in parts per
	// million. Positive = the host clock is running slow relative to the
	// server. Nil when fewer than 4 samples or less than 2 minutes are covered.
	DriftPPM *float64 `json:"driftPpm"`
	Polls    uint64   `json:"polls"`    // bursts attempted since start
	Failures uint64   `json:"failures"` // bursts without any valid reply
	Steps    uint64   `json:"steps"`    // host clock steps detected
}

// Options tunes a Sampler. The zero value is fine.
type Options struct {
	// Burst is the number of queries per sampling burst (1..8, default 4).
	// The reply with the smallest round-trip delay is kept. Bursts suit a
	// server on the local network (the Radarcape's GPS-disciplined NTP
	// server); public servers usually rate-limit them. The Sampler notices
	// that (a RATE kiss, or the tail of the burst being dropped twice in a
	// row) and falls back to single queries for that server on its own, but
	// set 1 up front when you know the server is a public one.
	Burst int
	// BurstGap is the pause between queries of a burst (default 250 ms).
	BurstGap time.Duration
	// Timeout bounds each individual query (default 1.5 s).
	Timeout time.Duration
	// HistorySize is the capacity of the history ring (default 8192 samples).
	HistorySize int
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Query replaces the network exchange (tests, alternative transports).
	Query func(ctx context.Context, server string) (*Response, error)
}

// Sampler periodically measures the offset between the host clock and the
// configured NTP server. It implements model.Clock and is safe for
// concurrent use. Create it with NewSampler and start it with Run.
type Sampler struct {
	log      *slog.Logger
	query    func(ctx context.Context, server string) (*Response, error)
	burst    int
	burstGap time.Duration

	wake     chan struct{} // config changed
	burstSem chan struct{} // serialises bursts (Run loop vs SampleNow)

	mu       sync.Mutex
	cfg      model.NTPConfig
	gen      uint64 // bumped on every effective config change
	ring     []Sample
	head     int // index of the oldest sample
	count    int
	win      []float64 // smoothing window, offsets in nanoseconds, oldest first
	recent   []float64 // jitter window, offsets in nanoseconds, oldest first
	pending  *float64  // first sample of a suspected clock step
	last     Sample
	haveLast bool
	lastOK   time.Time // carries a monotonic reading
	smoothed time.Duration
	jitterMs float64
	lastErr  string
	failing  bool
	kiss     int    // RATE back-off exponent
	denied   int    // consecutive DENY/RSTR answers
	pinned   string // "ip:port" that answered last; reused so that a pool name does not hop between servers
	limited  bool   // the server rate-limits bursts: send single queries
	cutShort int    // consecutive bursts whose tail went unanswered after a reply
	onSample func(Sample)
	polls    uint64
	failures uint64
	steps    uint64
}

var _ model.Clock = (*Sampler)(nil)

// NewSampler creates a stopped Sampler for cfg. Call Run to start sampling.
func NewSampler(cfg model.NTPConfig, opts Options) *Sampler {
	s := &Sampler{
		log:      opts.Logger,
		query:    opts.Query,
		burst:    opts.Burst,
		burstGap: opts.BurstGap,
		wake:     make(chan struct{}, 1),
		burstSem: make(chan struct{}, 1),
		cfg:      normalizeConfig(cfg),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.burst <= 0 {
		s.burst = DefaultBurst
	}
	if s.burst > 8 {
		s.burst = 8
	}
	if s.burstGap <= 0 {
		s.burstGap = 250 * time.Millisecond
	}
	if s.query == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = 1500 * time.Millisecond
		}
		c := &Client{Timeout: timeout}
		s.query = c.Query
	}
	size := opts.HistorySize
	if size <= 0 {
		size = DefaultHistory
	}
	s.ring = make([]Sample, size)
	return s
}

func normalizeConfig(cfg model.NTPConfig) model.NTPConfig {
	cfg.Server = strings.TrimSpace(cfg.Server)
	if cfg.IntervalSec <= 0 {
		cfg.IntervalSec = DefaultIntervalSec
	}
	if cfg.IntervalSec < MinIntervalSec {
		cfg.IntervalSec = MinIntervalSec
	}
	if cfg.IntervalSec > MaxIntervalSec {
		cfg.IntervalSec = MaxIntervalSec
	}
	return cfg
}

func active(cfg model.NTPConfig) bool { return cfg.Enabled && cfg.Server != "" }

func interval(cfg model.NTPConfig) time.Duration {
	return time.Duration(cfg.IntervalSec) * time.Second
}

// OnSample registers fn to be called (from the sampling goroutine, without
// internal locks held) after every accepted sample. It replaces any previous
// callback; nil removes it. The callback must not block for long.
func (s *Sampler) OnSample(fn func(Sample)) {
	s.mu.Lock()
	s.onSample = fn
	s.mu.Unlock()
}

// Reconfigure applies a new configuration live. Changing the server or the
// enabled flag discards the current synchronisation state (the history ring
// is kept; every sample records its server) and triggers an immediate burst.
func (s *Sampler) Reconfigure(cfg model.NTPConfig) {
	cfg = normalizeConfig(cfg)
	s.mu.Lock()
	old := s.cfg
	if old == cfg {
		s.mu.Unlock()
		return
	}
	s.cfg = cfg
	s.gen++
	if old.Server != cfg.Server || old.Enabled != cfg.Enabled {
		s.resetSyncLocked()
		s.lastErr = ""
		s.failing = false
		s.kiss = 0
		s.denied = 0
		s.pinned = ""
		s.limited = false
		s.cutShort = 0
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Sampler) resetSyncLocked() {
	s.win = s.win[:0]
	s.recent = s.recent[:0]
	s.pending = nil
	s.haveLast = false
	s.last = Sample{}
	s.lastOK = time.Time{}
	s.smoothed = 0
	s.jitterMs = 0
}

// Config returns the configuration currently in effect (normalised).
func (s *Sampler) Config() model.NTPConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Run samples until ctx is cancelled, then returns nil. While NTP is disabled
// or no server is configured it idles until Reconfigure is called.
func (s *Sampler) Run(ctx context.Context) error {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		s.mu.Lock()
		cfg, gen := s.cfg, s.gen
		s.mu.Unlock()

		if !active(cfg) {
			select {
			case <-ctx.Done():
				return nil
			case <-s.wake:
				continue
			}
		}
		_, err := s.sample(ctx, cfg, gen)
		if ctx.Err() != nil {
			return nil
		}
		timer.Reset(s.nextDelay(cfg, err))
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// nextDelay returns the pause before the next burst: the configured interval
// with ±5 % jitter, lengthened while the server is telling us to back off
// (RFC 5905 §7.4): each RATE kiss doubles the interval (up to 16×) until a
// burst succeeds; DENY/RSTR mean "go away", so the server is left alone for
// 1 h, doubling up to 24 h (a pool name resolves to another server by then).
func (s *Sampler) nextDelay(cfg model.NTPConfig, err error) time.Duration {
	d := interval(cfg)
	s.mu.Lock()
	var ke *KissError
	switch {
	case err == nil:
		s.kiss = 0
		s.denied = 0
	case errors.As(err, &ke):
		switch ke.Code {
		case "RATE":
			if s.kiss < 4 {
				s.kiss++
			}
		case "DENY", "RSTR":
			if s.denied < 6 {
				s.denied++
			}
		}
	}
	d <<= uint(s.kiss)
	if s.denied > 0 {
		if hold := min(time.Hour<<uint(s.denied-1), 24*time.Hour); d < hold {
			d = hold
		}
	}
	s.mu.Unlock()
	return time.Duration(float64(d) * (0.95 + 0.1*rand.Float64()))
}

// SampleNow runs one burst immediately (in addition to the periodic ones) and
// returns the accepted sample. It fails when NTP is disabled.
func (s *Sampler) SampleNow(ctx context.Context) (Sample, error) {
	s.mu.Lock()
	cfg, gen := s.cfg, s.gen
	s.mu.Unlock()
	if !active(cfg) {
		return Sample{}, ErrNoServer
	}
	return s.sample(ctx, cfg, gen)
}

// errSuperseded is returned when the configuration changed mid-burst.
var errSuperseded = errors.New("ntp: configuration changed during measurement")

// sample runs one burst against cfg.Server and records the outcome.
func (s *Sampler) sample(ctx context.Context, cfg model.NTPConfig, gen uint64) (Sample, error) {
	select {
	case s.burstSem <- struct{}{}:
	case <-ctx.Done():
		return Sample{}, ctx.Err()
	}
	defer func() { <-s.burstSem }()

	// Stick to the address that answered last time: a pool name such as
	// pool.ntp.org resolves to a different server on every lookup, and mixing
	// servers inside a burst (or between bursts) would show up as jitter.
	s.mu.Lock()
	target := cfg.Server
	burst := s.burst
	if s.gen == gen && s.pinned != "" {
		target = s.pinned
	}
	if s.limited {
		burst = 1
	}
	s.mu.Unlock()

	var (
		best     *Response
		lastErr  error
		sent     int
		replies  int
		failsRow int
		rateKiss bool // the server asked us to slow down
		tailLost bool // a reply, then two unanswered queries: the classic silent rate limit
	)
	for i := 0; i < burst; i++ {
		if i > 0 {
			t := time.NewTimer(s.burstGap)
			select {
			case <-ctx.Done():
				t.Stop()
				return Sample{}, ctx.Err()
			case <-t.C:
			}
		}
		sent++
		r, err := s.query(ctx, target)
		if ctx.Err() != nil {
			return Sample{}, ctx.Err()
		}
		if err != nil {
			lastErr = err
			failsRow++
			// A kiss-o'-death means "stop"; two failures in a row mean the
			// server is not there (or drops our burst): do not keep hammering it.
			var ke *KissError
			if errors.As(err, &ke) {
				rateKiss = ke.Code == "RATE"
				break
			}
			if failsRow >= 2 {
				tailLost = replies > 0
				break
			}
			continue
		}
		failsRow = 0
		replies++
		if best == nil || r.Delay < best.Delay {
			best = r
		}
		if r.Addr != "" {
			target = r.Addr
		}
	}

	s.mu.Lock()
	if s.gen != gen {
		s.mu.Unlock()
		return Sample{}, errSuperseded
	}
	s.polls++
	// Adapt to servers that rate-limit bursts (ntpd "restrict limited [kod]",
	// chrony "ratelimit", most public pool servers): keep to one query per
	// interval for as long as this server is configured.
	nowLimited := false
	if burst > 1 {
		switch {
		case rateKiss:
			nowLimited = true
		case tailLost:
			s.cutShort++
			nowLimited = s.cutShort >= 2
		case best != nil:
			s.cutShort = 0
		}
		if nowLimited {
			s.limited = true
		}
	}
	if best == nil {
		s.pinned = "" // resolve the name afresh next time
		s.failures++
		if lastErr == nil {
			lastErr = ErrTimeout
		}
		s.lastErr = lastErr.Error()
		first := !s.failing
		s.failing = true
		s.mu.Unlock()
		if nowLimited {
			s.log.Info("ntp: server rate-limits query bursts, switching to single queries", "server", cfg.Server)
		}
		if first {
			s.log.Warn("ntp: sampling failed", "server", cfg.Server, "err", lastErr)
		} else {
			s.log.Debug("ntp: sampling failed", "server", cfg.Server, "err", lastErr)
		}
		return Sample{}, lastErr
	}
	s.pinned = best.Addr
	smp, stepped := s.recordLocked(cfg, best, sent, replies)
	recovered := s.failing
	s.failing = false
	s.lastErr = ""
	fn := s.onSample
	s.mu.Unlock()

	if nowLimited {
		s.log.Info("ntp: server rate-limits query bursts, switching to single queries", "server", cfg.Server)
	}
	if stepped {
		s.log.Info("ntp: host clock step detected", "server", cfg.Server, "offsetMs", smp.OffsetMs)
	}
	if recovered {
		s.log.Info("ntp: sampling recovered", "server", cfg.Server, "offsetMs", smp.OffsetMs)
	}
	s.log.Debug("ntp: sample", "server", cfg.Server, "offsetMs", smp.OffsetMs,
		"delayMs", smp.DelayMs, "stratum", smp.Stratum, "replies", replies)
	if fn != nil {
		fn(smp)
	}
	return smp, nil
}

// recordLocked folds a measurement into the filters and the history ring.
func (s *Sampler) recordLocked(cfg model.NTPConfig, r *Response, sent, replies int) (Sample, bool) {
	off := float64(r.Offset) // nanoseconds (exact in a float64)
	stepped := false

	// A long gap (host asleep, server away) makes the old window meaningless.
	if s.haveLast && r.Time.UnixMilli()-s.last.T > int64(smoothWindow)*interval(cfg).Milliseconds() {
		s.win = s.win[:0]
		s.recent = s.recent[:0]
		s.pending = nil
	}

	// Step detection: two consecutive samples that both sit far away from the
	// window median and agree with each other mean the host clock was stepped
	// (or the server changed its mind); restart the filters from them instead
	// of letting the median lag for half a window.
	if len(s.win) >= 3 {
		med := median(s.win)
		thr := math.Max(float64(stepFloor), 10*mad(s.win, med))
		if math.Abs(off-med) > thr {
			if s.pending != nil && math.Abs(off-*s.pending) <= thr {
				s.win = append(s.win[:0], *s.pending)
				s.recent = append(s.recent[:0], *s.pending)
				s.pending = nil
				s.steps++
				stepped = true
			} else {
				p := off
				s.pending = &p
			}
		} else {
			s.pending = nil
		}
	}

	s.win = appendCapped(s.win, off, smoothWindow)
	s.recent = appendCapped(s.recent, off, jitterWindow)
	smoothedNs := median(s.win)
	s.smoothed = time.Duration(math.Round(smoothedNs))
	s.jitterMs = rmsSuccessive(s.recent) / 1e6

	smp := Sample{
		T:           r.Time.UnixMilli(),
		Server:      cfg.Server,
		Addr:        r.Addr,
		OffsetMs:    off / 1e6,
		DelayMs:     float64(r.Delay) / 1e6,
		Stratum:     r.Stratum,
		RefID:       r.RefID,
		Leap:        r.Leap,
		RootDelayMs: float64(r.RootDelay) / 1e6,
		RootDispMs:  float64(r.RootDispersion) / 1e6,
		SmoothedMs:  smoothedNs / 1e6,
		JitterMs:    s.jitterMs,
		Sent:        sent,
		Replies:     replies,
	}
	s.last = smp
	s.haveLast = true
	s.lastOK = time.Now()
	s.pushLocked(smp)
	return smp, stepped
}

func (s *Sampler) pushLocked(smp Sample) {
	n := len(s.ring)
	if s.count < n {
		s.ring[(s.head+s.count)%n] = smp
		s.count++
		return
	}
	s.ring[s.head] = smp
	s.head = (s.head + 1) % n
}

// holdover is how long a measured offset stays usable without a new sample.
func holdover(cfg model.NTPConfig) time.Duration {
	h := syncIntervals * interval(cfg)
	if h < minHoldover {
		h = minHoldover
	}
	return h
}

// Offset implements model.Clock: the robust smoothed offset (median of the
// last few samples) such that true UTC ≈ time.Now().Add(Offset()). It is 0
// when NTP is disabled, nothing has been measured yet, or the last good
// sample is older than the holdover period (max(3 intervals, 15 min)).
func (s *Sampler) Offset() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !active(s.cfg) || !s.haveLast || time.Since(s.lastOK) > holdover(s.cfg) {
		return 0
	}
	return s.smoothed
}

// Now returns the best estimate of true UTC: the precise host clock (see the
// package-level Now) corrected by Offset().
func (s *Sampler) Now() time.Time { return Now().Add(s.Offset()) }

// Synced implements model.Clock: true when a good sample was taken within
// the last three sampling intervals.
func (s *Sampler) Synced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncedLocked()
}

func (s *Sampler) syncedLocked() bool {
	return active(s.cfg) && s.haveLast && time.Since(s.lastOK) <= syncIntervals*interval(s.cfg)
}

// Status returns the summary shown in /api/status. OffsetMs is the smoothed
// offset; DelayMs, Stratum and RefID come from the latest accepted sample.
func (s *Sampler) Status() model.NTPStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := model.NTPStatus{
		Enabled:   active(s.cfg),
		Server:    s.cfg.Server,
		LastError: s.lastErr,
	}
	if !st.Enabled {
		st.LastError = ""
		return st
	}
	st.Synced = s.syncedLocked()
	if s.haveLast {
		st.OffsetMs = float64(s.smoothed) / 1e6
		st.DelayMs = s.last.DelayMs
		st.JitterMs = s.jitterMs
		st.Stratum = s.last.Stratum
		st.RefID = s.last.RefID
		st.LastOK = s.last.T
	}
	return st
}

// Latest returns the most recent accepted sample of the current server.
func (s *Sampler) Latest() (Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, s.haveLast
}

// History returns a copy of the history ring, oldest first, restricted to
// samples at or after since (zero time = everything).
func (s *Sampler) History(since time.Time) []Sample {
	var min int64 = math.MinInt64
	if !since.IsZero() {
		min = since.UnixMilli()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sample, 0, s.count)
	for i := 0; i < s.count; i++ {
		smp := s.ring[(s.head+i)%len(s.ring)]
		if smp.T >= min {
			out = append(out, smp)
		}
	}
	return out
}

// Seed preloads the history ring with persisted samples (e.g. from the
// ntp_samples table at start-up) so charts survive a restart. Seeded samples
// never affect Offset/Synced. Only samples older than anything already in the
// ring are kept; the newest ones win when the ring is too small.
func (s *Sampler) Seed(samples []Sample) {
	if len(samples) == 0 {
		return
	}
	in := make([]Sample, len(samples))
	copy(in, samples)
	sort.SliceStable(in, func(i, j int) bool { return in[i].T < in[j].T })

	s.mu.Lock()
	defer s.mu.Unlock()
	existing := make([]Sample, 0, s.count)
	for i := 0; i < s.count; i++ {
		existing = append(existing, s.ring[(s.head+i)%len(s.ring)])
	}
	if len(existing) > 0 {
		first := existing[0].T
		cut := sort.Search(len(in), func(i int) bool { return in[i].T >= first })
		in = in[:cut]
	}
	all := append(in, existing...)
	if len(all) > len(s.ring) {
		all = all[len(all)-len(s.ring):]
	}
	s.head = 0
	s.count = copy(s.ring, all)
}

// Stats summarises the samples of the last window (0 = whole ring).
func (s *Sampler) Stats(window time.Duration) Stats {
	var since time.Time
	if window > 0 {
		since = time.Now().Add(-window)
	}
	h := s.History(since)
	s.mu.Lock()
	st := Stats{Polls: s.polls, Failures: s.failures, Steps: s.steps}
	s.mu.Unlock()
	st.Count = len(h)
	if len(h) == 0 {
		return st
	}
	st.OffsetMinMs, st.OffsetMaxMs = math.Inf(1), math.Inf(-1)
	st.DelayMinMs, st.DelayMaxMs = math.Inf(1), math.Inf(-1)
	var sumO, sumD float64
	for _, x := range h {
		sumO += x.OffsetMs
		sumD += x.DelayMs
		st.OffsetMinMs = math.Min(st.OffsetMinMs, x.OffsetMs)
		st.OffsetMaxMs = math.Max(st.OffsetMaxMs, x.OffsetMs)
		st.DelayMinMs = math.Min(st.DelayMinMs, x.DelayMs)
		st.DelayMaxMs = math.Max(st.DelayMaxMs, x.DelayMs)
	}
	n := float64(len(h))
	st.OffsetMeanMs = sumO / n
	st.DelayMeanMs = sumD / n
	var ss float64
	for _, x := range h {
		d := x.OffsetMs - st.OffsetMeanMs
		ss += d * d
	}
	if len(h) > 1 {
		st.OffsetStdMs = math.Sqrt(ss / (n - 1))
	}
	st.SpanSec = float64(h[len(h)-1].T-h[0].T) / 1e3
	if ppm, ok := driftPPM(h); ok {
		st.DriftPPM = &ppm
	}
	return st
}

// driftPPM fits offset(t) = a + b·t by least squares and returns b in ppm.
func driftPPM(h []Sample) (float64, bool) {
	if len(h) < 4 {
		return 0, false
	}
	t0 := h[0].T
	span := float64(h[len(h)-1].T-t0) / 1e3
	if span < 120 {
		return 0, false
	}
	var sx, sy, sxx, sxy float64
	for _, x := range h {
		t := float64(x.T-t0) / 1e3 // seconds
		y := x.OffsetMs / 1e3      // seconds
		sx += t
		sy += y
		sxx += t * t
		sxy += t * y
	}
	n := float64(len(h))
	den := n*sxx - sx*sx
	if den <= 0 {
		return 0, false
	}
	return (n*sxy - sx*sy) / den * 1e6, true
}

func appendCapped(s []float64, v float64, max int) []float64 {
	if len(s) >= max {
		copy(s, s[len(s)-max+1:])
		s = s[:max-1]
	}
	return append(s, v)
}

// median returns the median of v (mean of the two middle values for even
// counts); 0 for an empty slice. v is not modified.
func median(v []float64) float64 {
	n := len(v)
	if n == 0 {
		return 0
	}
	tmp := make([]float64, n)
	copy(tmp, v)
	sort.Float64s(tmp)
	if n%2 == 1 {
		return tmp[n/2]
	}
	return (tmp[n/2-1] + tmp[n/2]) / 2
}

// mad returns the median absolute deviation of v around med.
func mad(v []float64, med float64) float64 {
	d := make([]float64, len(v))
	for i, x := range v {
		d[i] = math.Abs(x - med)
	}
	return median(d)
}

// rmsSuccessive returns the RMS of the differences between successive values.
func rmsSuccessive(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	var ss float64
	for i := 1; i < len(v); i++ {
		d := v[i] - v[i-1]
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(v)-1))
}
