package ssr

import (
	"math"
	"math/cmplx"
	"sort"

	"airview/internal/mlat/geo"
)

// Rotation is the antenna rotation of an interrogator, recovered from the
// times at which the pairs it interrogated were in its beam: the beam
// azimuth is Phase(t) = phase_w + 360°·t/PeriodS, with one phase per
// window of RotWindowS seconds (so slow rotation-rate wander and receiver
// clock drift do not break it). A negative period is an anticlockwise
// antenna.
type Rotation struct {
	PeriodS   float64
	Coherence float64 // mean resultant length over the windows, 0..1
	WindowS   float64
	T0        float64 // start of window 0 (local time)
	Phase     []float64
	PhaseOK   []bool
	Global    float64 // phase of the whole run (used for empty windows)
	GlobalOK  bool
}

// Valid reports whether a rotation was found.
func (r *Rotation) Valid() bool { return r != nil && r.PeriodS != 0 }

// Residual returns the angle (degrees, -180..180) between azimuth az
// (degrees) and the modelled beam azimuth at time t, and false when the
// model has no phase for t.
func (r *Rotation) Residual(t, az float64) (float64, bool) {
	if !r.Valid() {
		return 0, false
	}
	w := int(math.Floor((t - r.T0) / r.WindowS))
	ph, ok := r.Global, r.GlobalOK
	if w >= 0 && w < len(r.Phase) && r.PhaseOK[w] {
		ph, ok = r.Phase[w], true
	}
	if !ok {
		return 0, false
	}
	d := az - ph - 360*t/r.PeriodS
	return math.Mod(math.Mod(d+180, 360)+360, 360) - 180, true
}

// pairAzimuth returns the azimuth (degrees) of the pair's midpoint seen
// from the site, and the time of the pair.
func pairAzimuth(f *geo.Frame, o *PairObs) (az, t float64) {
	e := f.ToENU(o.PK.Add(o.PJ).Scale(0.5))
	return math.Atan2(e.X, e.Y) * 180 / math.Pi, (o.TK + o.TJ) / 2
}

// EstimateRotation finds the rotation period that makes the beam
// azimuths of the observations (times ts, azimuths az in degrees) most
// coherent, searching |period| in [minP, maxP] in both senses.
func EstimateRotation(ts, az []float64, windowS, minP, maxP float64) Rotation {
	n := len(ts)
	if n < 5 {
		return Rotation{}
	}
	t0 := ts[0]
	for _, t := range ts {
		t0 = math.Min(t0, t)
	}
	nw := 0
	win := make([]int, n)
	for i, t := range ts {
		win[i] = int((t - t0) / windowS)
		if win[i]+1 > nw {
			nw = win[i] + 1
		}
	}
	rad := make([]float64, n)
	for i := range az {
		rad[i] = az[i] * math.Pi / 180
	}
	S := make([]complex128, nw)
	score := func(f float64) float64 { // f: rotations per second (signed)
		for i := range S {
			S[i] = 0
		}
		for i := range ts {
			S[win[i]] += cmplx.Rect(1, rad[i]-2*math.Pi*f*(ts[i]-t0))
		}
		var s float64
		for _, v := range S {
			s += cmplx.Abs(v)
		}
		return s / float64(n)
	}
	df := 0.25 / (2 * math.Pi * windowS)
	bestF, best := 0.0, -1.0
	for _, sgn := range [2]float64{1, -1} {
		for f := 1 / maxP; f <= 1/minP; f += df {
			if v := score(sgn * f); v > best {
				best, bestF = v, sgn*f
			}
		}
	}
	// Refine.
	step := df / 2
	for step > df/200 {
		for _, c := range [2]float64{bestF - step, bestF + step} {
			if v := score(c); v > best {
				best, bestF = v, c
			}
		}
		step /= 2
	}
	score(bestF)
	r := Rotation{PeriodS: 1 / bestF, Coherence: best, WindowS: windowS, T0: t0,
		Phase: make([]float64, nw), PhaseOK: make([]bool, nw)}
	cnt := make([]int, nw)
	for i := range ts {
		cnt[win[i]]++
	}
	var G complex128
	for w, v := range S {
		G += v
		if cnt[w] >= 3 && cmplx.Abs(v) >= 0.7*float64(cnt[w]) {
			r.Phase[w] = (cmplx.Phase(v) - 2*math.Pi*bestF*t0) * 180 / math.Pi
			r.PhaseOK[w] = true
		}
	}
	if cmplx.Abs(G) >= 0.7*float64(n) {
		r.Global, r.GlobalOK = (cmplx.Phase(G)-2*math.Pi*bestF*t0)*180/math.Pi, true
	}
	return r
}

// rotationFor estimates the rotation of radar k from its assigned
// observations (at most 3000, evenly thinned).
func rotationFor(obs []PairObs, k int, site *Interrogator, cfg *Config) Rotation {
	var idx []int
	for i := range obs {
		if obs[i].Radar == k {
			idx = append(idx, i)
		}
	}
	if len(idx) < cfg.MinRadarObs {
		return Rotation{}
	}
	sort.Slice(idx, func(a, b int) bool { return obs[idx[a]].TK < obs[idx[b]].TK })
	stride := 1
	if len(idx) > 3000 {
		stride = (len(idx) + 2999) / 3000
	}
	f := geo.NewFrame(site.LLH)
	var ts, az []float64
	for j := 0; j < len(idx); j += stride {
		a, t := pairAzimuth(&f, &obs[idx[j]])
		ts = append(ts, t)
		az = append(az, a)
	}
	return EstimateRotation(ts, az, cfg.RotWindowS, cfg.MinPeriodS, cfg.MaxPeriodS)
}
