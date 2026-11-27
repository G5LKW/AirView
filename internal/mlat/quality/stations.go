package quality

import (
	"math"
	"sync"

	"airview/internal/model"
)

// Residual is one station's residual in an accepted solver fix (DESIGN
// §9.6 "per-station residual bias and outlier rate", "σ learning").
type Residual struct {
	Station model.StationIdx
	// ResidNs is model minus measurement, in ns (the solver's metres / c).
	ResidNs float64
	// W is the Baarda w-test statistic of the row (≈ N(0,1) when the
	// station's σ is right); 0 for excluded rows.
	W float64
	// ClockVarNs2 and JitterVarNs2 are the two parts of the variance the
	// solver used for the row (clock-state and per-frame), ns².
	ClockVarNs2, JitterVarNs2 float64
	// Excluded: RAIM excluded the row. Outlier: |W| above the outlier
	// threshold or excluded.
	Excluded bool
}

// outlierW is the |w| above which a used row counts as an outlier.
const outlierW = 4

// biasBinNs is the width of a bias bin; BiasBins consecutive biased bins
// (one hour) flag a station.
const (
	biasBinNs = int64(10 * 60e9)
	BiasBins  = 6
)

// wHist covers |w| in [0, 10) in steps of 0.05 for the robust scale.
const (
	wBins = 200
	wStep = 0.05
)

// stQual is the quality state of one station.
type stQual struct {
	// robust scale of w: histogram of |w|, decayed by halving
	wh    [wBins]float32
	whN   float32
	clkV  float64 // EWMA of the clock-variance part (ns²)
	jitV  float64 // EWMA of the jitter part (ns²)
	bias  float64 // EWMA of the residual (ns)
	biasW float64 // EWMA of w
	// minute counters for contribution rates (ring of 60 one-minute slots)
	used, excl, outl [60]uint32
	slotT            [60]int64
	// bias bins
	binStart       int64
	binSumW        float64
	binN           int
	run            int     // consecutive biased bins (signed by direction)
	flagged        bool    // bias > 3σ for BiasBins bins
	flaggedSinceNs int64   // when the flag was set
	lastNs         int64   // last observation
	priorNs        float64 // class prior jitter (ns)
}

// Stations learns per-station timing quality from solver residuals. Safe
// for concurrent use.
type Stations struct {
	mu sync.Mutex
	m  map[model.StationIdx]*stQual
	// tot: cumulative used rows and outliers (excluded or |w| > outlierW)
	// per station since the table was created; never pruned by Forget, so
	// the totals stay monotonic for the metrics recorder.
	tot map[model.StationIdx]*[2]uint64
	// OnFlag (optional) is called (outside the lock) when a station's
	// residual bias flag is raised: the engine asks for a survey.
	OnFlag func(st model.StationIdx, biasNs float64)
}

// NewStations returns an empty table.
func NewStations() *Stations {
	return &Stations{m: map[model.StationIdx]*stQual{}, tot: map[model.StationIdx]*[2]uint64{}}
}

const ewmaA = 0.01 // ~100-fix memory for the EWMAs

// Observe records the residuals of one accepted fix at nowNs. priorNs
// gives each station's class prior jitter (ns).
func (s *Stations) Observe(nowNs int64, rs []Residual, priorNs func(model.StationIdx) float64) {
	var flagged [8]model.StationIdx
	var flaggedBias [8]float64
	nf := 0
	s.mu.Lock()
	for i := range rs {
		r := &rs[i]
		q := s.m[r.Station]
		if q == nil {
			q = &stQual{}
			s.m[r.Station] = q
		}
		if priorNs != nil {
			q.priorNs = priorNs(r.Station)
		}
		q.lastNs = nowNs
		k := int((nowNs / minuteNs) % 60)
		if st := nowNs - nowNs%minuteNs; q.slotT[k] != st {
			q.slotT[k], q.used[k], q.excl[k], q.outl[k] = st, 0, 0, 0
		}
		tt := s.tot[r.Station]
		if tt == nil {
			tt = new([2]uint64)
			s.tot[r.Station] = tt
		}
		if r.Excluded {
			q.excl[k]++
			q.outl[k]++
			tt[1]++
			continue
		}
		tt[0]++
		q.used[k]++
		aw := math.Abs(r.W)
		if aw > outlierW {
			q.outl[k]++
			tt[1]++
		}
		// robust scale of w
		b := int(aw / wStep)
		if b >= wBins {
			b = wBins - 1
		}
		q.wh[b]++
		q.whN++
		if q.whN > 4000 {
			for j := range q.wh {
				q.wh[j] *= 0.5
			}
			q.whN *= 0.5
		}
		if q.clkV == 0 && q.jitV == 0 {
			q.clkV, q.jitV = r.ClockVarNs2, r.JitterVarNs2
		} else {
			q.clkV += ewmaA * (r.ClockVarNs2 - q.clkV)
			q.jitV += ewmaA * (r.JitterVarNs2 - q.jitV)
		}
		if aw <= outlierW {
			q.bias += ewmaA * (r.ResidNs - q.bias)
			q.biasW += ewmaA * (r.W - q.biasW)
			// bias bins: mean w over 10 minutes
			bs := nowNs - nowNs%biasBinNs
			if q.binStart != bs {
				if q.binStart != 0 {
					if ok := q.closeBin(); ok && nf < len(flagged) {
						flagged[nf], flaggedBias[nf] = r.Station, q.bias
						nf++
					}
					if !q.flagged {
						q.flaggedSinceNs = 0
					}
				}
				q.binStart, q.binSumW, q.binN = bs, 0, 0
			}
			q.binSumW += r.W
			q.binN++
		}
	}
	s.mu.Unlock()
	if s.OnFlag != nil {
		for i := 0; i < nf; i++ {
			s.OnFlag(flagged[i], flaggedBias[i])
		}
	}
}

// closeBin ends a bias bin; it returns true when the station became
// flagged. A bin is biased when its mean w is significant at 3σ
// (|mean|·√n > 3) and at least 0.5 (half a σ: smaller biases do not
// matter for positioning).
func (q *stQual) closeBin() bool {
	if q.binN < 20 {
		return false
	}
	m := q.binSumW / float64(q.binN)
	biased := math.Abs(m)*math.Sqrt(float64(q.binN)) > 3 && math.Abs(m) >= 0.5
	switch {
	case biased && m > 0:
		q.run = max(q.run, 0) + 1
	case biased && m < 0:
		q.run = min(q.run, 0) - 1
	default:
		q.run = 0
		q.flagged = false
		return false
	}
	if !q.flagged && (q.run >= BiasBins || q.run <= -BiasBins) {
		q.flagged = true
		q.flaggedSinceNs = q.binStart + biasBinNs
		return true
	}
	return false
}

// wScale returns the robust standard deviation of w (median |w| / 0.6745)
// and the effective sample count.
func (q *stQual) wScale() (float64, float64) {
	if q.whN < 1 {
		return math.NaN(), 0
	}
	half := q.whN / 2
	cum := float32(0)
	for b, c := range q.wh {
		cum += c
		if cum >= half {
			return (float64(b) + 0.5) * wStep / 0.6745, float64(q.whN)
		}
	}
	return math.NaN(), float64(q.whN)
}

// minLearnN is the number of fixes before a learned jitter is reported.
const minLearnN = 50

// learned returns the learned per-frame jitter (ns): the observed scale s
// of the studentised residual applies to the total variance, so the
// jitter that explains it is J' = s²(J + C) − C, clamped to ×0.5..×4 of
// the class prior (DESIGN §9.6).
func (q *stQual) learned() (float64, bool) {
	s, n := q.wScale()
	if n < minLearnN || math.IsNaN(s) || q.priorNs <= 0 {
		return 0, false
	}
	j := s*s*(q.jitV+q.clkV) - q.clkV
	jn := math.Sqrt(math.Max(j, 0))
	return min(max(jn, 0.5*q.priorNs), 4*q.priorNs), true
}

// StationReport is a station's MLAT quality (owner/admin, DESIGN §11
// "MLAT" section).
type StationReport struct {
	// ContribPerHour: fixes this station was used in over the last hour.
	ContribPerHour int `json:"contribPerHour"`
	// UsedPct / ExcludedPct: share of its rows used / excluded by RAIM
	// over the last hour (%).
	UsedPct     float64 `json:"usedPct"`
	ExcludedPct float64 `json:"excludedPct"`
	// OutlierPct: excluded or |w| > 4 (%).
	OutlierPct float64 `json:"outlierPct"`
	// ResidualBiasNs: EWMA of its residual (model − measurement, ns).
	ResidualBiasNs float64 `json:"residualBiasNs"`
	// WScale is the robust σ of its studentised residuals (1 = the σ in
	// use is right).
	WScale float64 `json:"wScale"`
	// LearnedJitterNs is the per-frame jitter explaining WScale (0 = not
	// enough data).
	LearnedJitterNs float64 `json:"learnedJitterNs"`
	// BiasFlag: residual bias above 3σ for an hour (a survey is asked
	// for); FlaggedSinceNs when it was raised.
	BiasFlag       bool  `json:"biasFlag"`
	FlaggedSinceNs int64 `json:"flaggedSinceNs,omitempty"`
	LastNs         int64 `json:"lastNs"`
}

// Report returns the station's quality at nowNs.
func (s *Stations) Report(st model.StationIdx, nowNs int64) (StationReport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.m[st]
	if q == nil {
		return StationReport{}, false
	}
	var used, excl, outl int
	for k := range q.slotT {
		if nowNs-q.slotT[k] < hourNs {
			used += int(q.used[k])
			excl += int(q.excl[k])
			outl += int(q.outl[k])
		}
	}
	r := StationReport{ContribPerHour: used, ResidualBiasNs: q.bias, BiasFlag: q.flagged,
		FlaggedSinceNs: q.flaggedSinceNs, LastNs: q.lastNs}
	if tot := used + excl; tot > 0 {
		r.UsedPct = 100 * float64(used) / float64(tot)
		r.ExcludedPct = 100 * float64(excl) / float64(tot)
		r.OutlierPct = 100 * float64(outl) / float64(tot)
	}
	if w, _ := q.wScale(); !math.IsNaN(w) {
		r.WScale = w
	}
	if j, ok := q.learned(); ok {
		r.LearnedJitterNs = j
	}
	return r, true
}

// LearnedJitterNs returns the learned per-frame jitter of a station.
func (s *Stations) LearnedJitterNs(st model.StationIdx) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q := s.m[st]; q != nil {
		return q.learned()
	}
	return 0, false
}

// Forget drops stations without a residual since beforeNs.
func (s *Stations) Forget(beforeNs int64) {
	s.mu.Lock()
	for k, q := range s.m {
		if q.lastNs < beforeNs {
			delete(s.m, k)
		}
	}
	s.mu.Unlock()
}

// Totals returns a station's cumulative counts since the table was
// created: fixes in which the station had a used row, and its outlier rows
// (excluded by RAIM, or used with |w| above the outlier threshold, the
// same rule as the hourly report). ok is false for a station never seen.
func (s *Stations) Totals(st model.StationIdx) (fixes, outliers uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tt := s.tot[st]; tt != nil {
		return tt[0], tt[1], true
	}
	return 0, 0, false
}
