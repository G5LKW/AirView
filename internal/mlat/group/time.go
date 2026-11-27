package group

import "math"

// Time is an instant of network time (DESIGN §9.2: UTC when the component
// has anchors, else the component's relative time scale) as whole Unix
// nanoseconds plus a fraction of a nanosecond.
//
// A float64 holding Unix nanoseconds resolves only 256 ns today, far too
// coarse for TDOA work, so network times are carried split: Ns is exact and
// Frac (0 <= Frac < 1) keeps the sub-nanosecond part. Differences of two
// Times are formed exactly in integer arithmetic first and only then
// converted to float64.
type Time struct {
	Ns   int64
	Frac float64
}

// MakeTime returns the normalised Time ns + frac (frac may be any finite
// value).
func MakeTime(ns int64, frac float64) Time {
	if frac >= 0 && frac < 1 {
		return Time{Ns: ns, Frac: frac}
	}
	w := math.Floor(frac)
	return Time{Ns: ns + int64(w), Frac: frac - w}
}

// Sub returns t − u in nanoseconds.
func (t Time) Sub(u Time) float64 { return float64(t.Ns-u.Ns) + (t.Frac - u.Frac) }

// Add returns t + d (d in nanoseconds).
func (t Time) Add(d float64) Time {
	w := math.Floor(d)
	return MakeTime(t.Ns+int64(w), t.Frac+(d-w))
}

// Before reports whether t is earlier than u.
func (t Time) Before(u Time) bool { return t.Ns < u.Ns || (t.Ns == u.Ns && t.Frac < u.Frac) }

// IsZero reports whether t is the zero Time.
func (t Time) IsZero() bool { return t.Ns == 0 && t.Frac == 0 }

// UnixNs returns t rounded down to whole Unix nanoseconds.
func (t Time) UnixNs() int64 { return t.Ns }
