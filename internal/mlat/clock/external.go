package clock

import (
	"math"

	"airview/internal/mlat/linalg"
)

// Cross-partition members as epoch-local states (DESIGN §14 phase 5,
// §20.2; doc.go "Partitioned filter").
//
// A member of a sync group that belongs to another partition of the same
// time domain ("external") is predicted from the previous snapshot. Its
// prediction error δθ_e is one random variable shared by every group of the
// epoch in which the station appears. Putting its variance into each
// group's measurement noise D (the earlier "consider" treatment) counts
// that error as independent per group: a partition that crossed the
// boundary through a few busy stations became N-fold over-confident about
// its common mode (1,000-station simnet: GPS-anchor e²/σ² 13 in the
// non-gauge partitions).
//
// Here the first Config.ExternalStates external (station, frame kind)
// pairs of the epoch become augmented states: each group's information is
// accumulated over [x; δθ_e] (H = 1 on the external column, D keeps only
// the white part: jitter and the propagation from the epoch to the arrival
// time), the external prior N(0, v_e) is added (v_e = the snapshot's
// variance at the epoch × ExternalInflate) and the external states are
// marginalised out before the dense update:
//
//	J_xx ← J_xx − J_xe·A⁻¹·J_ex,  g_x ← g_x − J_xe·A⁻¹·g_e,
//	A = J_ee + diag(1/v_e).
//
// With A = L·Lᵀ and B = J_xe·L⁻ᵀ this is J_xx −= B·Bᵀ, g_x −= B·(L⁻¹g_e);
// B has non-zero rows only for the internal states that share a group with
// an external member, so the cost is O(r²·k + r·k² + k³) for r such rows
// and k externals, small against the O(n³) update. External members beyond
// the cap keep the per-group treatment.
//
// Externals of different partitions are taken as uncorrelated (each comes
// from its own partition's marginal); the dependence between the external
// estimate and this partition's own state (both were informed by groups
// that cross the boundary in earlier epochs) is covered by
// ExternalInflate (covariance-intersection bound).

type extKey struct {
	st    *station
	short bool
}

// extState is the per-epoch external block of one component.
type extState struct {
	slot map[extKey]int32
	vr   []float64       // prior variance of each slot (ns², inflated)
	sc   []*StationClock // snapshot clock of each slot
	sh   []bool          // short-frame slot (θ + β_short)
	infl float64         // ExternalInflate
	pe   []float64       // k × k Cholesky scratch (marginalize)
	gs   []float64       // k scratch (marginalize)
	k    int             // slots used this epoch
	kcap int             // stride of jxe / jee (Config.ExternalStates)
	jxe  []float64       // n × kcap
	jee  []float64       // kcap × kcap
	ge   []float64       // kcap
	rows []int32         // internal rows with a non-zero jxe row
	used []bool          // n
	b    []float64       // len(rows) × k scratch
}

// reset prepares the block for an epoch of component c (n states).
func (xs *extState) reset(n, kcap int) {
	if xs.slot == nil {
		xs.slot = map[extKey]int32{}
	}
	clear(xs.slot)
	xs.vr, xs.sc, xs.sh = xs.vr[:0], xs.sc[:0], xs.sh[:0]
	xs.k = 0
	xs.rows = xs.rows[:0]
	if kcap <= 0 {
		xs.kcap = 0
		return
	}
	if xs.kcap != kcap || len(xs.used) != n {
		xs.kcap = kcap
		xs.jxe = make([]float64, n*kcap)
		xs.jee = make([]float64, kcap*kcap)
		xs.ge = make([]float64, kcap)
		xs.used = make([]bool, n)
		return
	}
	// jxe rows and jee/ge were cleared by the previous marginalisation
}

// slotFor returns the external slot of (s, short), allocating one with
// prior variance vr while slots are left; -1 when the cap is reached.
func (xs *extState) slotFor(s *station, sc *StationClock, short bool, vr float64) int32 {
	if xs.kcap == 0 {
		return -1
	}
	key := extKey{s, short}
	if i, ok := xs.slot[key]; ok {
		return i
	}
	if xs.k >= xs.kcap || !(vr > 0) {
		return -1
	}
	i := int32(xs.k)
	xs.k++
	xs.slot[key] = i
	xs.vr = append(xs.vr, vr)
	xs.sc = append(xs.sc, sc)
	xs.sh = append(xs.sh, short)
	return i
}

// addXE adds v to J_xe[i, e].
func (xs *extState) addXE(i, e int, v float64) {
	if !xs.used[i] {
		xs.used[i] = true
		xs.rows = append(xs.rows, int32(i))
	}
	xs.jxe[i*xs.kcap+e] += v
}

// marginalize folds the external block into c.J, c.g and clears it. It
// returns false (and leaves J, g without the cross-partition information
// of the externals) when the external block is not positive definite.
func (c *component) marginalize() bool {
	xs := &c.xs
	k, kc, n := xs.k, xs.kcap, c.n
	if k == 0 {
		return true
	}
	defer func() {
		for _, i := range xs.rows {
			clear(xs.jxe[int(i)*kc : int(i)*kc+k])
			xs.used[i] = false
		}
		for e := 0; e < k; e++ {
			clear(xs.jee[e*kc : e*kc+k])
		}
		clear(xs.ge[:k])
	}()
	// A = J_ee + diag(1/v_e), compacted to k×k (lower triangle)
	if cap(xs.pe) < k*k {
		xs.pe = make([]float64, k*k)
	}
	a := xs.pe[:k*k]
	clear(a)
	for i := 0; i < k; i++ {
		for j := 0; j <= i; j++ {
			a[i*k+j] = xs.jee[i*kc+j]
		}
		a[i*k+i] += 1 / xs.vr[i]
	}
	if linalg.Cholesky(a, k) != nil {
		return false
	}
	r := len(xs.rows)
	if cap(xs.b) < r*k {
		xs.b = make([]float64, r*k)
	}
	b := xs.b[:r*k]
	for ri, i := range xs.rows {
		bi := b[ri*k : ri*k+k]
		copy(bi, xs.jxe[int(i)*kc:int(i)*kc+k])
		linalg.SolveLower(a, k, bi)
	}
	if cap(xs.gs) < k {
		xs.gs = make([]float64, k)
	}
	ge := xs.gs[:k]
	copy(ge, xs.ge[:k])
	linalg.SolveLower(a, k, ge)
	for ri, i := range xs.rows {
		bi := b[ri*k : ri*k+k]
		row := c.J[int(i)*n:]
		for rj, j := range xs.rows {
			row[j] -= linalg.Dot(bi, b[rj*k:rj*k+k])
		}
		c.g[i] -= linalg.Dot(bi, ge)
	}
	return true
}

// extShared returns the variance of an external member's prediction at
// the epoch time (shared by all of the epoch's groups), inflated.
func extShared(g *gm, tE float64, short bool, inflate float64) float64 {
	_, v := g.ext.offsetAt(tE, short)
	return math.Max(v, 0) * inflate
}
