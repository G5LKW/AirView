package quality

import (
	"math"
	"sync"
)

// Sample is one hold-out comparison: an MLAT position of an aircraft that
// also reports ADS-B, against its latency-corrected reference position at
// the same transmit instant (DESIGN §9.6).
type Sample struct {
	// TNs is the transmit instant (Unix ns, server/network time).
	TNs int64
	// Lat, Lon (degrees) and AltM (height, m) of the reference position:
	// they select the error-map cell.
	Lat, Lon, AltM float64
	// EastM, NorthM is the horizontal error (MLAT minus reference) in the
	// local ENU frame; UpM the vertical error.
	EastM, NorthM, UpM float64
	// VelE, VelN is the reference ground velocity (m/s); it splits the
	// error into along-track and cross-track parts (zero = unknown).
	VelE, VelN float64
	// CovEE, CovEN, CovNN is the horizontal covariance the engine reported
	// for the MLAT position (m²).
	CovEE, CovEN, CovNN float64
	// RefCovEE, RefCovEN, RefCovNN is the reference position's horizontal
	// covariance (m²; NACp, smoother and latency). It is added to the MLAT
	// covariance for the NEES test, never to the reported σ.
	RefCovEE, RefCovEN, RefCovNN float64
	// Grade is the MLAT grade (model.Grade*).
	Grade uint8
}

// ErrH returns the horizontal error (m).
func (s *Sample) ErrH() float64 { return math.Hypot(s.EastM, s.NorthM) }

// SigmaH returns the reported horizontal 1σ (m): sqrt(σE² + σN²).
func (s *Sample) SigmaH() float64 { return math.Sqrt(math.Max(s.CovEE+s.CovNN, 0)) }

// AlongCross splits the horizontal error along and across the reference
// velocity (absolute values; along = ErrH, cross = 0 when the velocity is
// unknown or below 10 m/s).
func (s *Sample) AlongCross() (along, cross float64) {
	v := math.Hypot(s.VelE, s.VelN)
	if !(v > 10) {
		return s.ErrH(), 0
	}
	ue, un := s.VelE/v, s.VelN/v
	return math.Abs(s.EastM*ue + s.NorthM*un), math.Abs(-s.EastM*un + s.NorthM*ue)
}

// NEES returns the horizontal normalised estimation error squared under the
// sum of the MLAT and reference covariances (χ² with 2 degrees of freedom
// when both are consistent), NaN when the covariance is singular.
func (s *Sample) NEES() float64 {
	a := s.CovEE + s.RefCovEE
	b := s.CovEN + s.RefCovEN
	c := s.CovNN + s.RefCovNN
	det := a*c - b*b
	if !(det > 0) {
		return math.NaN()
	}
	return (c*s.EastM*s.EastM - 2*b*s.EastM*s.NorthM + a*s.NorthM*s.NorthM) / det
}

// χ²(2) quantiles: P(χ²₂ ≤ x) = 1 − exp(−x/2).
const (
	nees68 = 2.2788 // 68.27 %
	nees95 = 5.9915 // 95 %
)

// bucket accumulates one time slice.
type bucket struct {
	start   int64 // slice start (Unix ns); 0 = empty
	h       Hist  // horizontal error
	cross   Hist  // cross-track error (velocity known)
	n       uint64
	in1     uint64 // ErrH <= σ_h
	in2     uint64 // ErrH <= 2σ_h
	nees    uint64 // NEES finite
	nees68  uint64
	nees95  uint64
	byGrade [5]uint64
}

func (b *bucket) add(s *Sample) {
	e := s.ErrH()
	b.h.Add(e)
	if _, cr := s.AlongCross(); math.Hypot(s.VelE, s.VelN) > 10 {
		b.cross.Add(cr)
	}
	b.n++
	sg := s.SigmaH()
	if e <= sg {
		b.in1++
	}
	if e <= 2*sg {
		b.in2++
	}
	if q := s.NEES(); !math.IsNaN(q) {
		b.nees++
		if q <= nees68 {
			b.nees68++
		}
		if q <= nees95 {
			b.nees95++
		}
	}
	if s.Grade < 5 {
		b.byGrade[s.Grade]++
	}
}

func (b *bucket) merge(o *bucket) {
	b.h.Merge(&o.h)
	b.cross.Merge(&o.cross)
	b.n += o.n
	b.in1 += o.in1
	b.in2 += o.in2
	b.nees += o.nees
	b.nees68 += o.nees68
	b.nees95 += o.nees95
	for i := range b.byGrade {
		b.byGrade[i] += o.byGrade[i]
	}
}

// Summary is the error distribution over a window.
type Summary struct {
	N uint64 `json:"n"`
	// P50/P95/P99 of the horizontal error (m, ±2.5 % bin resolution).
	P50 float64 `json:"p50M"`
	P95 float64 `json:"p95M"`
	P99 float64 `json:"p99M"`
	// CrossP50/CrossP95: cross-track error (insensitive to an unknown
	// position latency of the reference), m.
	CrossP50 float64 `json:"crossP50M"`
	CrossP95 float64 `json:"crossP95M"`
	// Within1Sigma / Within2Sigma: fraction of errors <= σ_h / 2σ_h, where
	// σ_h = sqrt(σE² + σN²) is the reported horizontal 1σ. For a
	// consistent circular Gaussian error these are 63.2 % and 98.2 %.
	Within1Sigma float64 `json:"within1Sigma"`
	Within2Sigma float64 `json:"within2Sigma"`
	// NEES68 / NEES95: fraction of horizontal NEES (MLAT + reference
	// covariance) below the 68.27 % / 95 % χ²(2) quantiles.
	NEES68 float64 `json:"nees68"`
	NEES95 float64 `json:"nees95"`
	// ByGrade counts samples per grade (index = model.Grade*).
	ByGrade [5]uint64 `json:"byGrade"`
	// PerMin is the sample rate over the window (samples per minute).
	PerMin float64 `json:"perMin"`
}

func (b *bucket) summary(windowNs int64) Summary {
	s := Summary{N: b.n, P50: b.h.Quantile(0.5), P95: b.h.Quantile(0.95), P99: b.h.Quantile(0.99),
		CrossP50: b.cross.Quantile(0.5), CrossP95: b.cross.Quantile(0.95), ByGrade: b.byGrade}
	if b.n > 0 {
		s.Within1Sigma = float64(b.in1) / float64(b.n)
		s.Within2Sigma = float64(b.in2) / float64(b.n)
	}
	if b.nees > 0 {
		s.NEES68 = float64(b.nees68) / float64(b.nees)
		s.NEES95 = float64(b.nees95) / float64(b.nees)
	}
	if windowNs > 0 {
		s.PerMin = float64(b.n) / (float64(windowNs) / 60e9)
	}
	return s
}

// ErrorStats keeps the hold-out error distribution: one-minute slices for
// the last hour, hourly slices for 14 days (the week-on-week alarm, DESIGN
// §9.6) and a total since start. Safe for concurrent use. Add is O(1).
type ErrorStats struct {
	mu    sync.Mutex
	min   [60]bucket
	hour  [336]bucket
	total bucket
	first int64
}

const (
	minuteNs = int64(60e9)
	hourNs   = int64(3600e9)
)

func slot(ring []bucket, start, width int64) *bucket {
	i := int((start / width) % int64(len(ring)))
	b := &ring[i]
	if b.start != start {
		*b = bucket{start: start}
	}
	return b
}

// Add records one sample.
func (e *ErrorStats) Add(s *Sample) {
	if math.IsNaN(s.EastM) || math.IsNaN(s.NorthM) {
		return
	}
	t := s.TNs
	e.mu.Lock()
	if e.first == 0 || t < e.first {
		e.first = t
	}
	slot(e.min[:], t-t%minuteNs, minuteNs).add(s)
	slot(e.hour[:], t-t%hourNs, hourNs).add(s)
	e.total.add(s)
	e.mu.Unlock()
}

// Window returns the summary of the samples of the last windowNs (at most
// one hour, minute resolution) before nowNs.
func (e *ErrorStats) Window(nowNs, windowNs int64) Summary {
	windowNs = min(max(windowNs, minuteNs), 60*minuteNs)
	from := nowNs - windowNs
	var acc bucket
	e.mu.Lock()
	for i := range e.min {
		b := &e.min[i]
		if b.start != 0 && b.start+minuteNs > from && b.start <= nowNs {
			acc.merge(b)
		}
	}
	e.mu.Unlock()
	return acc.summary(windowNs)
}

// Total returns the summary of every sample since start.
func (e *ErrorStats) Total(nowNs int64) Summary {
	e.mu.Lock()
	acc := e.total
	first := e.first
	e.mu.Unlock()
	w := int64(0)
	if first > 0 {
		w = nowNs - first
	}
	return acc.summary(w)
}

// Alarm compares the P95 of the last 7 days with the 7 days before (DESIGN
// §9.6: alert when it regresses by more than 25 % week on week). ok is false
// until both weeks have at least minN samples.
func (e *ErrorStats) Alarm(nowNs int64, minN uint64) (thisP95, lastP95 float64, alarm, ok bool) {
	week := 7 * 24 * hourNs
	cur := nowNs - nowNs%hourNs
	var a, b bucket
	e.mu.Lock()
	for i := range e.hour {
		h := &e.hour[i]
		if h.start == 0 {
			continue
		}
		switch age := cur - h.start; {
		case age >= 0 && age < week:
			a.merge(h)
		case age >= week && age < 2*week:
			b.merge(h)
		}
	}
	e.mu.Unlock()
	if a.n < minN || b.n < minN {
		return 0, 0, false, false
	}
	thisP95, lastP95 = a.h.Quantile(0.95), b.h.Quantile(0.95)
	return thisP95, lastP95, thisP95 > 1.25*lastP95, true
}
