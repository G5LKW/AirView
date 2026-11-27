package server

import (
	"math"
	"strconv"
	"sync"
	"time"

	"airview/internal/model"
)

// Public API rate limit (DESIGN §8.4): a token bucket per client address,
// aggregator.limits.publicRps tokens per second up to publicBurst (defaults
// 1 and 10). Only requests in public scope are limited; owners and admins
// are not (their accounts are rate-limited at login).

type rateSpec struct {
	rps   float64
	burst float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter holds the buckets. Memory is bounded: idle buckets (full
// again) are dropped by a sweep when the map grows, and above
// rateMaxClients the oldest half is dropped.
type rateLimiter struct {
	mu        sync.Mutex
	m         map[string]*bucket
	now       func() time.Time
	lastSweep time.Time
}

const rateMaxClients = 200_000

func newRateLimiter() *rateLimiter {
	return &rateLimiter{m: map[string]*bucket{}, now: time.Now}
}

// allow takes one token for key. It returns false and the time until a
// token is available when the bucket is empty.
func (l *rateLimiter) allow(key string, spec rateSpec) (time.Duration, bool) {
	if spec.rps <= 0 || key == "" {
		return 0, true
	}
	if spec.burst < 1 {
		spec.burst = 1
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		if len(l.m) >= 4096 && now.Sub(l.lastSweep) > 10*time.Second {
			l.sweepLocked(now, spec)
		}
		if len(l.m) >= rateMaxClients {
			l.dropHalfLocked()
		}
		b = &bucket{tokens: spec.burst, last: now}
		l.m[key] = b
	} else {
		b.tokens = math.Min(spec.burst, b.tokens+now.Sub(b.last).Seconds()*spec.rps)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}
	return time.Duration((1 - b.tokens) / spec.rps * float64(time.Second)), false
}

// sweepLocked drops buckets that would be full again.
func (l *rateLimiter) sweepLocked(now time.Time, spec rateSpec) {
	l.lastSweep = now
	full := time.Duration(spec.burst / spec.rps * float64(time.Second))
	for k, b := range l.m {
		if now.Sub(b.last) >= full {
			delete(l.m, k)
		}
	}
}

func (l *rateLimiter) dropHalfLocked() {
	n := len(l.m) / 2
	for k := range l.m {
		if n <= 0 {
			break
		}
		delete(l.m, k)
		n--
	}
}

func (l *rateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// acctRate is the per-account rate of AccountLimited routes: one request
// every 2 s on average, bursts of 30 (the owner pages poll far slower).
var acctRate = rateSpec{rps: 0.5, burst: 30}

// publicRate returns the configured public rate (aggregator.limits).
func (s *Server) publicRate() rateSpec {
	if s.testRate != nil {
		return *s.testRate
	}
	lim := model.DefaultAggregator().Limits
	if s.eng != nil {
		if h := s.eng.Config(); h != nil {
			lim = h.Get().Aggregator.Limits
		}
	}
	return rateSpec{rps: lim.PublicRPS, burst: float64(lim.PublicBurst)}
}

// retryAfter formats a Retry-After value (whole seconds, at least 1).
func retryAfter(d time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(d.Seconds()))))
}
