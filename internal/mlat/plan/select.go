package plan

import (
	"math"

	"airview/internal/mlat/geo"
)

// Station selection for one MLAT aircraft: greedy D-optimal design.
//
// With the TOA model c·t_j = |p − r_j| + b + noise_j (b = c·τ, the unknown
// transmit time), the Jacobian row of station j for the unknowns (p, b) is
//
//	h_j = [u_jᵀ, 1],   u_j = (p − r_j)/|p − r_j|,
//
// and the Fisher information of a station set S is J(S) = J₀ + Σ_{j∈S} h_j h_jᵀ/σ_j²
// (J₀ a weak prior that keeps J invertible before four stations). Adding
// station k multiplies det J by (1 + h_kᵀ J⁻¹ h_k / σ_k²) (matrix determinant
// lemma), so the greedy D-optimal choice is the station with the largest
//
//	gain_k = h_kᵀ P h_k / σ_k²,   P = J⁻¹,
//
// and P is updated in O(16) by Sherman–Morrison:
//
//	P ← P − (P h)(P h)ᵀ / (σ_k² + hᵀ P h).
//
// log det J is submodular in S, so the greedy set is within (1 − 1/e) of the
// best set of its size in information volume; in practice it picks the
// stations that open the angles the set lacks (a distant station on the far
// side beats a third one next to two already chosen). The expected
// horizontal CRLB is σ_h² = tr(P_pp) − upᵀ P_pp up; stations are added until
// StationsPerAircraft, or MinStationsPerAircraft and σ_h ≤ TargetSigmaM.

const (
	priorPosM  = 1e5 // weak position prior, m
	priorBiasM = 1e5 // weak transmit-time prior, m
)

type mat4 [4][4]float64

func (m *mat4) mulVec(h *[4]float64) (r [4]float64) {
	for i := 0; i < 4; i++ {
		r[i] = m[i][0]*h[0] + m[i][1]*h[1] + m[i][2]*h[2] + m[i][3]*h[3]
	}
	return
}

// selectStations chooses stations for aircraft a among its located hearers
// and appends the requests to w.req.
func (w *workspace) selectStations(ai int32, cfg *Config) {
	a := &w.ac[ai]
	a.r0 = int32(len(w.req))
	// Candidate stations: located hearers.
	w.cand = w.cand[:0]
	var centroid geo.Vec3
	for _, h := range w.hs[a.h0:a.h1] {
		s := &w.st[h.si]
		if s.ok && s.info.Located {
			w.cand = append(w.cand, h.si)
			centroid = centroid.Add(s.info.Pos)
		}
	}
	if len(w.cand) == 0 {
		a.r1 = a.r0
		a.sigmaH = math.Inf(1)
		return
	}
	p := a.pos
	if !a.posOK {
		c := centroid.Scale(1 / float64(len(w.cand)))
		p = c.Add(c.Unit().Scale(9000))
	}
	up := p.Unit()
	var P mat4
	for i := 0; i < 3; i++ {
		P[i][i] = priorPosM * priorPosM
	}
	P[3][3] = priorBiasM * priorBiasM
	var used uint64 // bit per w.cand entry (only the first 64 candidates are considered)
	n := min(len(w.cand), 64)
	a.sigmaH = math.Inf(1)
	for k := 0; k < cfg.StationsPerAircraft; k++ {
		if k >= cfg.MinStationsPerAircraft && a.sigmaH <= cfg.TargetSigmaM {
			break
		}
		bi, bg := -1, 0.0
		var bh [4]float64
		var bs2 float64
		for i := 0; i < n; i++ {
			if used&(1<<uint(i)) != 0 {
				continue
			}
			s := &w.st[w.cand[i]]
			d := p.Sub(s.info.Pos)
			r := d.Norm()
			if r < 1 {
				continue
			}
			h := [4]float64{d.X / r, d.Y / r, d.Z / r, 1}
			ph := P.mulVec(&h)
			s2 := s.sigM * s.sigM
			if g := (h[0]*ph[0] + h[1]*ph[1] + h[2]*ph[2] + h[3]*ph[3]) / s2; g > bg {
				bi, bg, bh, bs2 = i, g, h, s2
			}
		}
		if bi < 0 {
			break
		}
		used |= 1 << uint(bi)
		ph := P.mulVec(&bh)
		den := bs2 + bh[0]*ph[0] + bh[1]*ph[1] + bh[2]*ph[2] + bh[3]*ph[3]
		for i := 0; i < 4; i++ {
			for j := 0; j < 4; j++ {
				P[i][j] -= ph[i] * ph[j] / den
			}
		}
		tr := P[0][0] + P[1][1] + P[2][2]
		u := [3]float64{up.X, up.Y, up.Z}
		var vu float64
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				vu += u[i] * P[i][j] * u[j]
			}
		}
		if k >= 3 {
			a.sigmaH = math.Sqrt(max(tr-vu, 0))
		}
		w.req = append(w.req, request{ai: ai, si: w.cand[bi], prio: a.prio, gain: math.Log1p(bg)})
	}
	a.r1 = int32(len(w.req))
}

// ---------------------------------------------------------------- sync sets

type heapItem struct {
	gain float64
	ai   int32
}

func (w *workspace) push(it heapItem) {
	w.heap = append(w.heap, it)
	i := len(w.heap) - 1
	for i > 0 {
		par := (i - 1) / 2
		if w.heap[par].gain >= w.heap[i].gain {
			break
		}
		w.heap[par], w.heap[i] = w.heap[i], w.heap[par]
		i = par
	}
}

func (w *workspace) pop() heapItem {
	h := w.heap
	top := h[0]
	last := len(h) - 1
	h[0] = h[last]
	h = h[:last]
	i := 0
	for {
		l, r, m := 2*i+1, 2*i+2, i
		if l < len(h) && h[l].gain > h[m].gain {
			m = l
		}
		if r < len(h) && h[r].gain > h[m].gain {
			m = r
		}
		if m == i {
			break
		}
		h[i], h[m] = h[m], h[i]
		i = m
	}
	w.heap = h
	return top
}

// syncGain is the value of aircraft ai for station si's sync set given the
// current coverage counts: Σ over the other hearers j of w_j/(1 + c_j).
// Each additional aircraft shared with peer j is worth less (diminishing
// returns), so the set spreads over many peers, which is what connects the
// clock graph and makes every pair's offset observable.
func (w *workspace) syncGain(ai, si int32) float64 {
	a := &w.ac[ai]
	var g float64
	for _, h := range w.hs[a.h0:a.h1] {
		if h.si == si {
			continue
		}
		g += w.st[h.si].w / (1 + float64(w.cov[h.si]))
	}
	// Aircraft sending few frames are worth proportionally less.
	return g * float64(min(a.rate/2, 1))
}

// selectSync chooses station si's sync set by lazy greedy maximisation of
// the submodular coverage gain (current members get the SwapMargin bonus).
// The chosen aircraft indices are appended to w.chosen.
func (w *workspace) selectSync(si int32, cfg *Config, cur map[uint32]int64, fpsCap float64) (fps float64) {
	s := &w.st[si]
	w.heap = w.heap[:0]
	for _, ai := range w.scand[s.sc0:s.sc1] {
		g := w.syncGain(ai, si)
		if _, ok := cur[w.ac[ai].key]; ok {
			g *= cfg.SwapMargin
		}
		if g > 0 {
			w.push(heapItem{gain: g, ai: ai})
		}
	}
	w.touched = w.touched[:0]
	const minGain = 0.25
	n := 0
	for len(w.heap) > 0 && n < cfg.SyncSetMax {
		it := w.pop()
		g := w.syncGain(it.ai, si)
		if _, ok := cur[w.ac[it.ai].key]; ok {
			g *= cfg.SwapMargin
		}
		if len(w.heap) > 0 && g < w.heap[0].gain {
			it.gain = g
			w.push(it) // stale bound: re-insert with the true gain
			continue
		}
		if n >= cfg.SyncSetMin && g < minGain {
			break
		}
		r := float64(w.ac[it.ai].rate)
		if fps+r > fpsCap && n >= cfg.SyncSetMin {
			continue
		}
		fps += r
		n++
		w.chosen = append(w.chosen, w.ac[it.ai].key)
		a := &w.ac[it.ai]
		for _, h := range w.hs[a.h0:a.h1] {
			if w.cov[h.si] == 0 {
				w.touched = append(w.touched, h.si)
			}
			w.cov[h.si]++
		}
	}
	for _, t := range w.touched {
		w.cov[t] = 0
	}
	return fps
}
