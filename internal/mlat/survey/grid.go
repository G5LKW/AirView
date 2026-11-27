package survey

import (
	"math"
	"slices"

	"airview/internal/mlat/geo"
)

// gridMaxObs bounds the observations the seed grid evaluates per
// candidate.
const gridMaxObs = 3000

type cand struct {
	e, n, cost float64
}

// grid finds the seed of the Gauss-Newton refinement (DESIGN §10.2 "a
// ±50 km grid seeded by the declared position, Locate Me v2, or the
// centroid of the stations U co-observes with"): candidates on the local
// tangent plane of seed at height h0, each scored by fitting the clock
// alone (the clock is linear given the position) and summing a
// pseudo-Huber loss of the residuals whose scale follows the grid step, so
// the coarse levels see a smooth, wide basin. Coarse-to-fine: radius/10,
// then the best three candidates refined by a quarter step until the step
// is below 50 m. edge reports that the best point lies on the outer ring
// (the station may be outside the search area).
func (p *prob) grid(seed geo.Vec3, radius, h0 float64) (geo.Vec3, bool) {
	fr := geo.NewFrameECEF(seed)
	stride := max(1, p.n/gridMaxObs)
	sub := make([]int, 0, p.n/stride+1)
	for k := 0; k < p.n; k += stride {
		sub = append(sub, k)
	}
	clear(p.w)
	s0 := p.segs[0].jitM
	for _, k := range sub {
		p.w[k] = 1 / (s0*s0 + p.in[k].VarTauM2)
	}
	point := func(e, n float64) geo.Vec3 {
		llh := geo.FromECEF(fr.FromENU(geo.Vec3{X: e, Y: n}))
		llh.H = h0
		return llh.ECEF()
	}
	eval := func(e, n, scale float64) float64 {
		u := point(e, n)
		clear(p.b)
		for _, k := range sub {
			p.res[k] = p.rho[k] - p.in[k].P.Sub(u).Norm()
		}
		if _, _, err := p.solveStep(u, &fr, false, heightPrior{}); err != nil {
			return math.Inf(1)
		}
		copy(p.b, p.z)
		c2 := scale * scale
		cost := 0.0
		for _, k := range sub {
			r := p.res[k] - p.clockAt(k)
			cost += math.Sqrt(1+r*r/c2) - 1
		}
		return cost * c2
	}
	step := radius / 10
	var cs []cand
	for i := -10; i <= 10; i++ {
		for j := -10; j <= 10; j++ {
			if i*i+j*j > 100 {
				continue
			}
			e, n := float64(i)*step, float64(j)*step
			cs = append(cs, cand{e, n, eval(e, n, math.Max(3*s0, step/2))})
		}
	}
	keep := func(cs []cand, k int) []cand {
		slices.SortFunc(cs, func(a, b cand) int {
			switch {
			case a.cost < b.cost:
				return -1
			case a.cost > b.cost:
				return 1
			}
			return 0
		})
		return cs[:min(k, len(cs))]
	}
	best := keep(cs, 3)
	for step > 50 {
		step /= 4
		var next []cand
		for _, c := range best {
			for i := -3; i <= 3; i++ {
				for j := -3; j <= 3; j++ {
					e, n := c.e+float64(i)*step, c.n+float64(j)*step
					next = append(next, cand{e, n, eval(e, n, math.Max(3*s0, step/2))})
				}
			}
		}
		best = keep(next, 3)
	}
	b := best[0]
	edge := math.Hypot(b.e, b.n) > radius*0.95
	clear(p.b)
	return point(b.e, b.n), edge
}
