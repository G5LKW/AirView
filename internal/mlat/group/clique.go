package group

import "math/bits"

// cliqueSearch finds a maximum clique of the compatibility graph of up to 64
// copies (adjacency as bit sets) by Bron–Kerbosch with Tomita pivoting and a
// size bound. Among cliques of the maximum size the one with the smallest sum
// of squared gate ratios wins (the most self-consistent set). The search is
// bounded by budget calls; when the budget runs out, the best clique found so
// far is returned (exhausted is set).
//
// A set of copies of a single transmission is always a clique (every pair
// passes its gate with probability ≈ 1 − 6e-5 at 4σ), so the maximum clique is
// the largest set of copies that can be one transmission. Growing a cluster
// greedily from the newest copy (DESIGN §9.3 step 3) is order dependent: a
// copy of the other transmission of a burst that is admitted early can lock
// out several copies of the true one. The exact search does not have that
// failure mode, and on the near-complete graphs of real groups it is cheap
// (one branch per level with the pivot rule).
type cliqueSearch struct {
	adj   *[MaxMembers]uint64
	ratio *[MaxMembers][MaxMembers]float32

	best      uint64
	bestN     int
	bestScore float64
	budget    int
	exhausted bool
}

func (s *cliqueSearch) run(p uint64, budget int) uint64 {
	s.best, s.bestN, s.bestScore = 0, 0, 0
	s.budget, s.exhausted = budget, false
	s.bk(0, 0, p, 0)
	return s.best
}

func (s *cliqueSearch) bk(r uint64, nr int, p, x uint64) {
	s.budget--
	np := bits.OnesCount64(p)
	if nr+np < s.bestN {
		return
	}
	if p == 0 {
		if x == 0 {
			s.leaf(r, nr)
		}
		return
	}
	// Pivot: the vertex of P ∪ X with the most neighbours in P.
	// A vertex adjacent to all of P (the common case on the near-complete
	// graphs of real groups) cannot be beaten: stop there.
	pu, bestDeg := -1, -1
	for m := p | x; m != 0; m &= m - 1 {
		u := bits.TrailingZeros64(m)
		if d := bits.OnesCount64(p & s.adj[u]); d > bestDeg {
			pu, bestDeg = u, d
			if p&^s.adj[u]&^(1<<uint(u)) == 0 {
				break
			}
		}
	}
	cand := p &^ s.adj[pu]
	for cand != 0 {
		if s.budget <= 0 {
			s.exhausted = true
			return
		}
		v := bits.TrailingZeros64(cand)
		bit := uint64(1) << v
		cand &^= bit
		s.bk(r|bit, nr+1, p&s.adj[v], x&s.adj[v])
		p &^= bit
		x |= bit
	}
}

// leaf scores a maximal clique.
func (s *cliqueSearch) leaf(r uint64, nr int) {
	if nr < s.bestN {
		return
	}
	var score float64
	for a := r; a != 0; a &= a - 1 {
		i := bits.TrailingZeros64(a)
		for b := a & (a - 1); b != 0; b &= b - 1 {
			q := float64(s.ratio[i][bits.TrailingZeros64(b)])
			score += q * q
		}
	}
	if nr > s.bestN || score < s.bestScore {
		s.best, s.bestN, s.bestScore = r, nr, score
	}
}

// greedy grows a clique from the newest member (DESIGN §9.3 step 3), visiting
// the others from newest to oldest; order lists the members of p sorted by
// increasing time. Used when the exact search ran out of budget without a
// result.
func greedy(adj *[MaxMembers]uint64, p uint64, order []int8) uint64 {
	var r uint64
	allowed := p
	for i := len(order) - 1; i >= 0; i-- {
		v := uint(order[i])
		bit := uint64(1) << v
		if allowed&bit == 0 {
			continue
		}
		r |= bit
		allowed &= adj[v]
	}
	return r
}
