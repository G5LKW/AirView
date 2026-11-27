package sdr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
)

// Options configure a Receiver. The zero value connects to a local rtl_tcp
// on its default port and receives 1090 MHz at 2.4 MS/s with maximum gain.
type Options struct {
	// Addr is the rtl_tcp server, host:port (default "127.0.0.1:1234").
	Addr string
	// FrequencyHz is the tuning frequency (default 1,090,000,000).
	FrequencyHz uint32
	// SampleRate is the complex sample rate (default 2,400,000; 2.0-3.2 MS/s).
	SampleRate uint32
	// Gain is "max" (default: the highest manual gain step), "auto" (the
	// tuner's AGC, not recommended for Mode S) or a manual gain in dB such
	// as "40.2" (the server picks the nearest step).
	Gain string
	// PPM is the oscillator frequency correction.
	PPM int
	// RTLAGC enables the RTL2832U digital AGC (default off).
	RTLAGC bool
	// BiasTee switches on the antenna supply of dongles that have one.
	BiasTee bool
	// Demod tunes the demodulator; its SampleRate is taken from SampleRate.
	Demod DemodConfig
	// FeedID is stamped on every frame.
	FeedID string
	// ReconnectMin / ReconnectMax bound the reconnect back-off
	// (defaults 1 s / 30 s).
	ReconnectMin, ReconnectMax time.Duration
	// StallTimeout: no samples for this long closes the connection and
	// reconnects (default 5 s).
	StallTimeout time.Duration
	// DropTolerance: when the samples fall behind the wall clock by more
	// than this beyond the oscillator drift, rtl_tcp has dropped buffers and
	// a new clock epoch begins (default 50 ms).
	DropTolerance time.Duration
	// Buffers is the number of 256 KiB read buffers between the socket and
	// the demodulator (default 16, about 0.9 s at 2.4 MS/s). When all are
	// in use the newest data is discarded locally and the demodulator's
	// sample counter is advanced over it, so timestamps stay continuous.
	Buffers int
	// Logf receives diagnostics (nil = silent).
	Logf func(format string, args ...any)
	// OnClockEvent is called when a new timestamp epoch begins.
	OnClockEvent func(epoch uint32, reason model.ClockEventReason)
	// Dial replaces the TCP dialer (tests).
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	// Now replaces the wall clock (tests).
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.Addr == "" {
		o.Addr = "127.0.0.1:1234"
	}
	if o.FrequencyHz == 0 {
		o.FrequencyHz = 1090000000
	}
	if o.SampleRate == 0 {
		o.SampleRate = 2400000
	}
	if o.Gain == "" {
		o.Gain = "max"
	}
	if o.ReconnectMin == 0 {
		o.ReconnectMin = time.Second
	}
	if o.ReconnectMax == 0 {
		o.ReconnectMax = 30 * time.Second
	}
	if o.StallTimeout == 0 {
		o.StallTimeout = 5 * time.Second
	}
	if o.DropTolerance == 0 {
		o.DropTolerance = 50 * time.Millisecond
	}
	if o.Buffers == 0 {
		o.Buffers = 16
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Dial == nil {
		o.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	o.Demod.SampleRate = float64(o.SampleRate)
	return o
}

// ParseGain checks a Gain option value. It returns the manual gain in
// tenths of a dB, or auto/max.
func ParseGain(s string) (tenths int, auto, max bool, err error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "max":
		return 0, false, true, nil
	case "auto", "agc":
		return 0, true, false, nil
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "dB"), 64)
	if err != nil || v < 0 || v > 60 || math.IsNaN(v) {
		return 0, false, false, fmt.Errorf("sdr: gain %q is not max, auto or 0-60 dB", s)
	}
	return int(math.Round(v * 10)), false, false, nil
}

// Validate reports invalid options.
func (o Options) Validate() error {
	o = o.withDefaults()
	if _, _, err := net.SplitHostPort(o.Addr); err != nil {
		return fmt.Errorf("sdr: address %q: %w", o.Addr, err)
	}
	if _, _, _, err := ParseGain(o.Gain); err != nil {
		return err
	}
	if o.PPM < -500 || o.PPM > 500 {
		return errors.New("sdr: ppm must be within ±500")
	}
	if o.FrequencyHz < 24e6 || o.FrequencyHz > 1766e6 {
		return errors.New("sdr: frequency outside 24-1766 MHz")
	}
	return o.Demod.Validate()
}

// Stats are the receiver's counters.
type Stats struct {
	Addr        string
	Connected   bool
	Tuner       string
	GainSteps   uint32
	Epoch       uint32
	Connects    uint64 // successful connections
	DialErrors  uint64
	SampleDrops uint64 // epochs started because rtl_tcp dropped samples
	LocalDrops  uint64 // samples discarded because the demodulator fell behind
	LastError   string
	Demod       DemodStats
}

// Receiver reads an rtl_tcp stream, demodulates it and delivers
// model.RawFrame values with sdr12 timestamps. Create it with NewReceiver
// and call Run.
type Receiver struct {
	o Options

	mu     sync.Mutex
	stats  Stats
	demod  *Demodulator
	epoch  atomic.Uint32
	client atomic.Pointer[Client]
}

// NewReceiver returns a receiver; call Run to start it. It panics on
// invalid options (check Options.Validate for user input).
func NewReceiver(o Options) *Receiver {
	if err := o.Validate(); err != nil {
		panic(err)
	}
	o = o.withDefaults()
	r := &Receiver{o: o, demod: NewDemodulator(o.Demod)}
	r.epoch.Store(uint32(o.Now().Unix()))
	r.stats.Addr = o.Addr
	return r
}

// Stats returns a snapshot of the counters.
func (r *Receiver) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	s.Demod = r.demod.Stats()
	s.Epoch = r.epoch.Load()
	return s
}

// Client returns the current rtl_tcp connection (nil when disconnected), for
// run-time changes such as SetGain.
func (r *Receiver) Client() *Client { return r.client.Load() }

func (r *Receiver) logf(format string, args ...any) {
	if r.o.Logf != nil {
		r.o.Logf(format, args...)
	}
}

// Run connects, demodulates and reconnects until ctx is cancelled. Frames go
// to sink.HandleFrame from Run's goroutine (one at a time, in stream order).
func (r *Receiver) Run(ctx context.Context, sink model.FrameSink) error {
	backoff := r.o.ReconnectMin
	for {
		start := r.o.Now()
		err := r.session(ctx, sink)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.mu.Lock()
		r.stats.Connected = false
		if err != nil {
			r.stats.LastError = err.Error()
		}
		r.mu.Unlock()
		if err != nil {
			r.logf("sdr: %s: %v", r.o.Addr, err)
		}
		if r.o.Now().Sub(start) > time.Minute {
			backoff = r.o.ReconnectMin // the last session was healthy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > r.o.ReconnectMax {
			backoff = r.o.ReconnectMax
		}
	}
}

func (r *Receiver) configure(c *Client) error {
	o := r.o
	tenths, auto, max, _ := ParseGain(o.Gain)
	steps := []struct {
		name string
		fn   func() error
	}{
		{"sample rate", func() error { return c.SetSampleRate(o.SampleRate) }},
		{"frequency", func() error { return c.SetFrequency(o.FrequencyHz) }},
		{"ppm", func() error { return c.SetFreqCorrection(o.PPM) }},
		{"gain mode", func() error { return c.SetTunerAGC(auto) }},
		{"gain", func() error {
			switch {
			case auto:
				return nil
			case max && c.Info().GainCount > 0:
				return c.SetGainIndex(int(c.Info().GainCount) - 1)
			case max:
				return c.SetGain(500)
			}
			return c.SetGain(tenths)
		}},
		{"rtl agc", func() error { return c.SetRTLAGC(o.RTLAGC) }},
	}
	if o.BiasTee {
		steps = append(steps, struct {
			name string
			fn   func() error
		}{"bias tee", func() error { return c.SetBiasTee(true) }})
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			return fmt.Errorf("setting %s: %w", s.name, err)
		}
	}
	return nil
}

// newEpoch starts a new timestamp epoch.
func (r *Receiver) newEpoch(reason model.ClockEventReason) uint32 {
	e := r.epoch.Add(1)
	if reason == model.ClockEvRestart {
		r.demod.ResetStream()
	} else {
		r.demod.Reset()
	}
	if r.o.OnClockEvent != nil {
		r.o.OnClockEvent(e, reason)
	}
	return e
}

const readBufSize = 256 << 10

type chunk struct {
	buf  []byte
	n    int
	at   time.Time // wall time the read returned
	skip int       // bytes discarded locally before this chunk
	err  error
}

func (r *Receiver) session(ctx context.Context, sink model.FrameSink) error {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := r.o.Dial(dctx, r.o.Addr)
	cancel()
	if err != nil {
		r.mu.Lock()
		r.stats.DialErrors++
		r.mu.Unlock()
		return err
	}
	_ = conn.SetReadDeadline(r.o.Now().Add(r.o.StallTimeout))
	c, err := NewClient(conn)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if err := r.configure(c); err != nil {
		return err
	}
	r.client.Store(c)
	defer r.client.Store(nil)
	r.mu.Lock()
	r.stats.Connected = true
	r.stats.Connects++
	r.stats.Tuner = c.Info().Tuner.String()
	r.stats.GainSteps = c.Info().GainCount
	r.stats.LastError = ""
	r.mu.Unlock()
	r.logf("sdr: connected to rtl_tcp %s (tuner %s, %d gain steps), %.1f MHz, %.2f MS/s, gain %s",
		r.o.Addr, c.Info().Tuner, c.Info().GainCount, float64(r.o.FrequencyHz)/1e6, float64(r.o.SampleRate)/1e6, r.o.Gain)

	epoch := r.newEpoch(model.ClockEvRestart)

	// reader goroutine: socket -> chunks; a fixed pool of buffers
	pool := make(chan []byte, r.o.Buffers)
	for i := 0; i < r.o.Buffers; i++ {
		pool <- make([]byte, readBufSize)
	}
	chunks := make(chan chunk, r.o.Buffers)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(chunks)
		spare := make([]byte, readBufSize)
		skipped := 0
		for {
			var buf []byte
			select {
			case buf = <-pool:
			default:
				buf = nil
			}
			target := buf
			if target == nil {
				target = spare
			}
			_ = conn.SetReadDeadline(r.o.Now().Add(r.o.StallTimeout))
			n, err := c.Read(target)
			at := r.o.Now()
			if buf == nil { // demodulator behind: discard, but count
				skipped += n
				if err == nil {
					continue
				}
				buf = spare
				n = 0
			}
			select {
			case chunks <- chunk{buf: buf, n: n, at: at, skip: skipped, err: err}:
				skipped = 0
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	wg.Add(1)
	go func() { // unblock the reader on cancellation
		defer wg.Done()
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()
	defer func() {
		close(stop)
		conn.Close()
		wg.Wait()
	}()

	fs := float64(r.o.SampleRate)
	dm := r.demod
	var (
		anchorWall time.Time // wall time of the epoch's first chunk
		anchorSmp  uint64
		baseline   float64 // smallest lag (samples) seen, drift-limited
		winMin     = math.Inf(1)
		winStart   time.Time
		chunkAt    time.Time
		chunkEnd   uint64 // sample count at the end of the current chunk
	)
	resetClock := func(at time.Time) {
		anchorWall, anchorSmp = at, dm.SampleCount()
		baseline, winMin, winStart = math.Inf(1), math.Inf(1), at
	}
	emit := func(f *Frame) {
		kind := model.FrameModeSShort
		if f.Len == 14 {
			kind = model.FrameModeSLong
		}
		// host time of the frame: the wall time of the read that delivered
		// the end of the chunk, minus the samples that followed the frame
		host := chunkAt.Add(-time.Duration((float64(chunkEnd) - f.Sample) / fs * 1e9))
		sink.HandleFrame(model.RawFrame{
			Recv:       chunkAt,
			GPSNs:      -1,
			Clock12MHz: f.Ticks & (1<<48 - 1),
			Signal:     math.Min(0, f.SignalDBFS),
			Kind:       kind,
			Data:       append([]byte(nil), f.Bytes()...),
			FeedID:     r.o.FeedID,
			Channel:    -1,
			Epoch:      epoch,
			ClockCls:   model.ClockSDR12,
			Ticks:      f.Ticks,
			HostNs:     host.UnixNano(),
		})
	}
	first := true
	for ch := range chunks {
		if ch.skip > 0 {
			dm.SkipBytes(ch.skip)
			r.mu.Lock()
			r.stats.LocalDrops += uint64(ch.skip / 2)
			r.mu.Unlock()
		}
		if ch.n > 0 {
			if first {
				resetClock(ch.at)
				first = false
			}
			chunkAt = ch.at
			chunkEnd = dm.SampleCount() + uint64(ch.n/2)
			dm.Process(ch.buf[:ch.n], emit)
			// sample-drop detection: lag = wall-clock samples - received
			elapsed := ch.at.Sub(anchorWall).Seconds()
			lag := elapsed*fs - float64(dm.SampleCount()-anchorSmp)
			if lag < winMin {
				winMin = lag
			}
			if ch.at.Sub(winStart) >= 2*time.Second {
				// the baseline follows the windowed minimum down at once and
				// up by at most 300 ppm (oscillator error and drift)
				win := ch.at.Sub(winStart).Seconds()
				switch {
				case math.IsInf(baseline, 1):
					baseline = winMin
				case winMin-baseline > r.o.DropTolerance.Seconds()*fs+300e-6*win*fs:
					r.mu.Lock()
					r.stats.SampleDrops++
					r.mu.Unlock()
					r.logf("sdr: samples behind the clock by %.0f ms: rtl_tcp dropped data, new clock epoch", (winMin-baseline)/fs*1e3)
					epoch = r.newEpoch(model.ClockEvSampleDrop)
					resetClock(ch.at)
				default:
					baseline = math.Min(winMin, baseline+300e-6*win*fs)
				}
				winMin, winStart = math.Inf(1), ch.at
			}
		}
		if ch.buf != nil && len(ch.buf) == readBufSize {
			select {
			case pool <- ch.buf:
			default:
			}
		}
		if ch.err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return ch.err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
	return nil
}
