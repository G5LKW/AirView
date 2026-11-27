package mlat

import (
	"sync/atomic"

	"airview/internal/mlat/quality"
)

// verifyBreaker is the network-wide plausibility check of the ADS-B
// verification (DESIGN §9.6; finding SCALE-05). Spoofed or wrong ADS-B
// positions are rare: when most checks of the last one to two minutes
// contradict, the TDOAs themselves are wrong (clocks that diverged after a
// restructure, an overloaded engine, a wrong atmosphere) and charging every
// aircraft would flood the operator with false spoofing suspects (simnet,
// 1,000 feeders, no spoofers: 90 % contradicted, 4,041 suspects). While the
// contradicted share is above verifyMuteShare (with at least
// verifyMuteMinN checks), contradictions are counted but not charged to the
// aircraft (Stats.ADSBMuted).
type verifyBreaker struct {
	cur, prev [2]atomic.Uint64 // [confirmed, contradicted] per minute
}

const (
	verifyMuteShare = 0.3
	verifyMuteMinN  = 200
)

// note records a verdict and reports whether a contradiction may be
// charged to the aircraft (always true for other verdicts).
func (b *verifyBreaker) note(v quality.Verdict) bool {
	switch v {
	case quality.VerdictConfirmed:
		b.cur[0].Add(1)
		return true
	case quality.VerdictContradicted:
		b.cur[1].Add(1)
		return !b.muted()
	}
	return true
}

// muted reports whether the contradicted share of the current and the
// previous minute is implausibly high.
func (b *verifyBreaker) muted() bool {
	conf := b.cur[0].Load() + b.prev[0].Load()
	con := b.cur[1].Load() + b.prev[1].Load()
	n := conf + con
	return n >= verifyMuteMinN && float64(con) > verifyMuteShare*float64(n)
}

// rotate starts a new minute (Advance, once a minute).
func (b *verifyBreaker) rotate() {
	for i := range b.cur {
		b.prev[i].Store(b.cur[i].Swap(0))
	}
}
