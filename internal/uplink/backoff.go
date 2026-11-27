package uplink

import (
	"math/rand/v2"
	"time"
)

// backoff is the reconnect delay of AVF §9.1: from min doubling to max with
// ±20 % jitter, reset after a session that lasted at least resetAfter.
type backoff struct {
	min, max time.Duration
	cur      time.Duration
}

const backoffResetAfter = 60 * time.Second

func newBackoff(min, max time.Duration) backoff { return backoff{min: min, max: max, cur: min} }

// next returns the delay before the next attempt and advances the state.
func (b *backoff) next() time.Duration {
	d := b.cur
	b.cur = min(b.cur*2, b.max)
	return jitter(d)
}

// reset returns to the minimum delay.
func (b *backoff) reset() { b.cur = b.min }

// jitter applies ±20 % uniform jitter.
func jitter(d time.Duration) time.Duration {
	f := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * f)
}
