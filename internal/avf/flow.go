package avf

import (
	"math/rand/v2"
	"sync"
	"time"
)

// Flow control, backfill share and reconnect back-off (AVF.md §5.14, §7.3,
// §9.1).

// FlowState is the client's view of the server's FLOW instructions. It is
// safe for concurrent use.
type FlowState struct {
	mu     sync.Mutex
	batch  time.Duration
	paused bool
	slow   float64
}

// NewFlowState starts unpaused at full rate with the given batch interval.
func NewFlowState(batch time.Duration) *FlowState {
	return &FlowState{batch: batch, slow: 1}
}

// Apply merges a FLOW record. batchMs 0 keeps the current interval;
// slowFactor 0 means 1 (full rate). It reports whether the batch interval
// changed (call Conn.SetBatch then).
func (f *FlowState) Apply(fl Flow) (batchChanged bool, err error) {
	if err := fl.Validate(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fl.BatchMs > 0 {
		d := time.Duration(fl.BatchMs) * time.Millisecond
		batchChanged = d != f.batch
		f.batch = d
	}
	f.paused = fl.Pause
	f.slow = fl.SlowFactor
	if f.slow == 0 {
		f.slow = 1
	}
	return batchChanged, nil
}

// Batch returns the flush interval.
func (f *FlowState) Batch() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.batch
}

// Paused reports whether the server asked to pause data.
func (f *FlowState) Paused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

// SlowFactor returns the requested fraction of the current data rate (0,1].
func (f *FlowState) SlowFactor() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.slow
}

// Allowed reports whether a record of type t may be sent now. While paused
// only control records, CLOCK_PAIR and INPUT_STATUS (and RECEIVER, which
// carries no bulk data) may be sent; everything else stays in the ring.
func (f *FlowState) Allowed(t RecordType) bool {
	if !f.Paused() {
		return true
	}
	return t.Control() || t == TClockPair || t == TInputStatus || t == TReceiver
}

// TokenBucket is a byte-rate limiter (for the 25 % backfill share and
// slowFactor). It is safe for concurrent use.
type TokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

// NewTokenBucket returns a full bucket.
func NewTokenBucket(ratePerSec, burst float64) *TokenBucket {
	return &TokenBucket{rate: ratePerSec, burst: burst, tokens: burst}
}

// SetRate changes the refill rate.
func (b *TokenBucket) SetRate(ratePerSec float64) {
	b.mu.Lock()
	b.rate = ratePerSec
	b.mu.Unlock()
}

// Take consumes n tokens if available at now and reports success.
func (b *TokenBucket) Take(n float64, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.last.IsZero() {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.last = now
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// BackfillShare is the maximum fraction of the link rate backfill may use.
const BackfillShare = 0.25

// BackfillRate returns the byte rate backfill may use given the measured
// live link rate: the share of the total rate, i.e. live·share/(1−share),
// but at least floor bytes per second so backfill progresses on an idle
// link.
func BackfillRate(liveBytesPerSec, floor float64) float64 {
	r := liveBytesPerSec * BackfillShare / (1 - BackfillShare)
	if r < floor {
		r = floor
	}
	return r
}

// ---------------------------------------------------------------- server

// FlowAction is the server's decision for one station.
type FlowAction int

// Flow actions.
const (
	FlowNone           FlowAction = iota
	FlowSend                      // send FLOW (slow down)
	FlowSelective                 // move the station to selective mode
	FlowDisconnect                // FLOW was ignored: GOODBYE rate_limited, reconnectIn 300
	FlowSendOverLimits            // over per-station limits: send FLOW now (first step)
)

// FlowMonitor implements the server rules of AVF.md §7.3 for one station:
// FLOW when the station's queue stays above 50 % for 5 s, selective when
// that persists another 5 s, and for rates above MaxFPS / MaxBytesPerSec
// FLOW first, then disconnect if still above after Grace.
type FlowMonitor struct {
	MaxFPS         float64       // frames per second (model.AggLimits.MaxFPSPerStation)
	MaxBytesPerSec float64       // compressed bytes per second (MaxKBpsPerStation·1000)
	Hold           time.Duration // queue-high duration before FLOW (default 5 s)
	Grace          time.Duration // over-limit duration after FLOW before disconnect (default 10 s)

	highSince  time.Time
	flowSentAt time.Time
	overSince  time.Time
	overFlowed bool
	selective  bool
}

// Observe takes the current queue fill (0..1) and measured rates and
// returns the action to take now. Each action is returned once per
// escalation step.
func (m *FlowMonitor) Observe(now time.Time, queueFill, fps, bytesPerSec float64) FlowAction {
	hold := m.Hold
	if hold <= 0 {
		hold = 5 * time.Second
	}
	grace := m.Grace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	over := (m.MaxFPS > 0 && fps > m.MaxFPS) || (m.MaxBytesPerSec > 0 && bytesPerSec > m.MaxBytesPerSec)
	if over {
		if m.overSince.IsZero() {
			m.overSince = now
		}
		if !m.overFlowed {
			m.overFlowed = true
			return FlowSendOverLimits
		}
		if now.Sub(m.overSince) >= grace {
			return FlowDisconnect
		}
	} else {
		m.overSince, m.overFlowed = time.Time{}, false
	}
	if queueFill <= 0.5 {
		m.highSince, m.flowSentAt = time.Time{}, time.Time{}
		return FlowNone
	}
	if m.highSince.IsZero() {
		m.highSince = now
		return FlowNone
	}
	if m.flowSentAt.IsZero() {
		if now.Sub(m.highSince) >= hold {
			m.flowSentAt = now
			return FlowSend
		}
		return FlowNone
	}
	if !m.selective && now.Sub(m.flowSentAt) >= hold {
		m.selective = true
		return FlowSelective
	}
	return FlowNone
}

// ---------------------------------------------------------------- backoff

// Backoff is the client's reconnect delay: from Min doubling to Max with
// ±Jitter, reset after a session that stayed up for ResetAfter (AVF.md
// §9.1). It is not safe for concurrent use.
type Backoff struct {
	Min, Max   time.Duration // default 1 s, 60 s
	Jitter     float64       // default 0.2
	ResetAfter time.Duration // default 60 s

	cur      time.Duration
	override time.Duration
}

// Next returns the delay before the next attempt. A pending override
// (reconnectIn, denied back-off) wins once, exactly and without jitter.
func (b *Backoff) Next() time.Duration {
	min, max, j := b.Min, b.Max, b.Jitter
	if min <= 0 {
		min = time.Second
	}
	if max <= 0 {
		max = 60 * time.Second
	}
	if j <= 0 {
		j = 0.2
	}
	if b.override > 0 {
		d := b.override
		b.override = 0
		return d
	}
	if b.cur < min {
		b.cur = min
	} else {
		b.cur *= 2
		if b.cur > max {
			b.cur = max
		}
	}
	f := 1 + j*(2*rand.Float64()-1)
	return time.Duration(float64(b.cur) * f)
}

// Override makes the next delay exactly d (reconnectIn).
func (b *Backoff) Override(d time.Duration) {
	if d > b.override {
		b.override = d
	}
}

// SessionEnded resets the back-off when the session lasted ResetAfter.
func (b *Backoff) SessionEnded(upFor time.Duration) {
	r := b.ResetAfter
	if r <= 0 {
		r = 60 * time.Second
	}
	if upFor >= r {
		b.cur = 0
	}
}
