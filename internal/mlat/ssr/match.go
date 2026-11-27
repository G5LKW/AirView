package ssr

import (
	"math"
	"sort"
)

// TrainMatch is one common interrogation sequence of two reply trains.
type TrainMatch struct {
	Delta  float64 // mean b - a time difference over the matched replies, s
	N      int     // matched replies
	TA, TB float64 // mean times of the matched replies of a and b
	Spread float64 // RMS of the differences about Delta, s
}

type tdiff struct {
	d    float64
	i, j int32
}

// window returns the start and end (exclusive) of the densest run of
// width 2·tol among ds (sorted by d), counting only entries accepted by
// keep (nil = all).
func densest(ds []tdiff, tol float64, keep func(*tdiff) bool) (lo, hi, n int) {
	best := -1
	e, cnt := 0, 0
	for s := 0; s < len(ds); s++ {
		if e < s {
			e, cnt = s, 0
		}
		for e < len(ds) && ds[e].d-ds[s].d <= 2*tol {
			if keep == nil || keep(&ds[e]) {
				cnt++
			}
			e++
		}
		if cnt > best {
			best, lo, hi = cnt, s, e
		}
		if keep == nil || keep(&ds[s]) {
			cnt--
		}
	}
	return lo, hi, best
}

// MatchTrains finds the interrogation sequences that two reply trains a
// and b (sorted arrival times of two aircraft) have in common: every
// interrogation of a beam dwell that both aircraft answered shows as the
// same difference b_j - a_i (to within the reply jitter), so the
// differences cluster. Up to maxPeaks clusters with at least minMatches
// one-to-one matches and |Delta| <= maxLag are returned, largest first.
//
// A cluster whose replies also line up, with at least ambRatio of its
// matches, at another difference is a PRI alias (a constant-PRF
// interrogator answered in a dwell longer than the geometry window) and is
// rejected: the true difference cannot be told from the alias.
func MatchTrains(a, b []float64, maxLag, tol float64, minMatches int, ambRatio float64, maxPeaks int) []TrainMatch {
	if len(a) < minMatches || len(b) < minMatches {
		return nil
	}
	ds := make([]tdiff, 0, len(a)*2)
	j0 := 0
	for i, t := range a {
		for j0 < len(b) && b[j0] < t-maxLag {
			j0++
		}
		for j := j0; j < len(b) && b[j] <= t+maxLag; j++ {
			ds = append(ds, tdiff{b[j] - t, int32(i), int32(j)})
		}
	}
	if len(ds) < minMatches {
		return nil
	}
	sort.Slice(ds, func(x, y int) bool { return ds[x].d < ds[y].d })
	usedA := make([]bool, len(a))
	usedB := make([]bool, len(b))
	free := func(t *tdiff) bool { return !usedA[t.i] && !usedB[t.j] }
	var out []TrainMatch
	for len(out) < maxPeaks {
		lo, hi, n := densest(ds, tol, free)
		if n < minMatches {
			break
		}
		// One-to-one matches inside the window, closest to the median first.
		win := make([]tdiff, 0, hi-lo)
		for k := lo; k < hi; k++ {
			if free(&ds[k]) {
				win = append(win, ds[k])
			}
		}
		med := win[len(win)/2].d
		sort.Slice(win, func(x, y int) bool { return math.Abs(win[x].d-med) < math.Abs(win[y].d-med) })
		inA := map[int32]bool{}
		inB := map[int32]bool{}
		var m []tdiff
		for _, w := range win {
			if inA[w.i] || inB[w.j] {
				continue
			}
			inA[w.i], inB[w.j] = true, true
			m = append(m, w)
		}
		for _, w := range m {
			usedA[w.i], usedB[w.j] = true, true
		}
		if len(m) < minMatches {
			continue
		}
		// Alias test: the same replies of a lining up elsewhere.
		sameA := func(t *tdiff) bool { return inA[t.i] && math.Abs(t.d-med) > 2*tol }
		_, _, alias := densest(ds, tol, sameA)
		if float64(alias) >= ambRatio*float64(len(m)) {
			continue
		}
		var sd, sa, sb float64
		for _, w := range m {
			sd += w.d
			sa += a[w.i]
			sb += b[w.j]
		}
		k := float64(len(m))
		mean := sd / k
		var v float64
		for _, w := range m {
			v += (w.d - mean) * (w.d - mean)
		}
		out = append(out, TrainMatch{Delta: mean, N: len(m), TA: sa / k, TB: sb / k, Spread: math.Sqrt(v / k)})
	}
	return out
}
