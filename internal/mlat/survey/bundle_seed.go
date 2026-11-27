package survey

import (
	"math"
	"slices"

	"airview/internal/mlat/geo"
)

// seedAll gives every used station a start position: its prior position
// (anchors, declared positions, hints with σ_h ≤ SeedSigmaAboveM), else a
// grid search against the stations seeded so far (the station with the
// most co-observations with seeded stations first). A station that
// cannot be seeded (no co-observations with a seeded station) is left out
// ("unseeded").
func (b *bundle) seedAll() error {
	cfg := b.cfg
	seeded := make([]bool, len(b.st))
	any := false
	for i := range b.st {
		st := &b.st[i]
		if st.slot < 0 {
			continue
		}
		in := st.in
		switch {
		case in.Prior == PriorAnchor || in.Prior == PriorDeclared,
			in.Prior == PriorHint && pos(in.SigmaHM, cfg.HintSigmaM) <= cfg.SeedSigmaAboveM:
			st.u = in.Pos
			seeded[i] = true
			any = true
		}
		st.start = st.u
	}
	if !any {
		// nothing to start from: the network frame comes from the
		// aircraft, but a grid needs at least one located station
		for i := range b.st {
			if b.st[i].slot >= 0 {
				b.st[i].why = "unseeded"
			}
		}
		return ErrBundleTooFew
	}
	for {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		// the unseeded station with the most co-observations
		co := make([]int, len(b.st))
		for fi := range b.frames {
			f := &b.frames[fi]
			if !f.used {
				continue
			}
			hasSeed := false
			for ri := f.r0; ri < f.r0+f.nr; ri++ {
				if r := &b.recs[ri]; r.blk >= 0 && seeded[r.st] {
					hasSeed = true
					break
				}
			}
			if !hasSeed {
				continue
			}
			for ri := f.r0; ri < f.r0+f.nr; ri++ {
				if r := &b.recs[ri]; r.blk >= 0 && !seeded[r.st] {
					co[r.st]++
				}
			}
		}
		best, bn := -1, 0
		for i := range b.st {
			if b.st[i].slot >= 0 && !seeded[i] && co[i] > bn {
				best, bn = i, co[i]
			}
		}
		if best < 0 || bn < cfg.MinStationRecs/2 {
			break
		}
		u, ok := b.gridSeed(best, seeded)
		if !ok {
			b.st[best].why = "unseeded"
			seeded[best] = true // not a candidate again; removed below
			continue
		}
		b.st[best].u = u
		b.st[best].start = u
		b.st[best].seeded = true
		seeded[best] = true
	}
	drop := false
	for i := range b.st {
		st := &b.st[i]
		if st.slot >= 0 && (!seeded[i] || st.why != "") {
			if st.why == "" {
				st.why = "unseeded"
			}
			drop = true
		}
	}
	if drop {
		return b.reindex(false)
	}
	return nil
}

// seedPair is one co-observation of the station being seeded with a
// seeded station: d0 − R(p, u) is their clock difference (plus noise).
type seedPair struct {
	p   geo.Vec3
	d0  float64
	x   float64
	grp int32
}

type seedGroup struct {
	n   int
	deg int
	inv [9]float64 // inverse of Σ φφᵀ (deg+1 square, row-major in 3×3)
	ok  bool
}

// gridSeed searches the start position of station i (DESIGN §10.2 seed
// grid): candidates on its height prior within SearchRadiusM of its hint
// or of the centroid of the seeded stations it co-observes with, scored
// by the robust misfit of the clock differences to the seeded stations
// after a polynomial per station pair and window; the best three
// candidates are refined by halving steps down to 30 m.
func (b *bundle) gridSeed(i int, seeded []bool) (geo.Vec3, bool) {
	cfg := b.cfg
	st := &b.st[i]
	var pairs []seedPair
	grp := map[[2]int32]int32{}
	var groups []seedGroup
	var cen geo.Vec3
	ncen := 0
	var hs []float64
	type ref struct {
		ri   int32
		kind PriorKind
	}
	var refs []ref
	for fi := range b.frames {
		f := &b.frames[fi]
		if !f.used {
			continue
		}
		own := int32(-1)
		refs = refs[:0]
		for ri := f.r0; ri < f.r0+f.nr; ri++ {
			r := &b.recs[ri]
			if r.blk < 0 {
				continue
			}
			if int(r.st) == i {
				own = ri
			} else if seeded[r.st] && b.st[r.st].why == "" {
				refs = append(refs, ref{ri, b.st[r.st].in.Prior})
			}
		}
		if own < 0 || len(refs) == 0 {
			continue
		}
		// anchors first, at most three references per frame
		slices.SortStableFunc(refs, func(a, c ref) int { return int(c.kind) - int(a.kind) })
		ro := &b.recs[own]
		for _, rf := range refs[:min(3, len(refs))] {
			rr := &b.recs[rf.ri]
			sr := &b.st[rr.st]
			key := [2]int32{ro.blk, rr.blk}
			g, ok := grp[key]
			if !ok {
				g = int32(len(groups))
				grp[key] = g
				groups = append(groups, seedGroup{deg: max(st.deg, sr.deg)})
			}
			groups[g].n++
			pairs = append(pairs, seedPair{p: f.p, d0: ro.y - rr.y + b.atm.Range(f.p, sr.u), x: f.x, grp: g})
			cen = cen.Add(sr.u)
			ncen++
			if len(hs) < 4096 {
				hs = append(hs, geo.FromECEF(sr.u).H)
			}
		}
	}
	if ncen == 0 {
		return geo.Vec3{}, false
	}
	// subsample to MaxSeedPairs (whole groups stay balanced enough)
	if len(pairs) > cfg.MaxSeedPairs {
		stride := float64(len(pairs)) / float64(cfg.MaxSeedPairs)
		k := 0
		for f := 0.0; int(f) < len(pairs) && k < cfg.MaxSeedPairs; f += stride {
			pairs[k] = pairs[int(f)]
			k++
		}
		pairs = pairs[:k]
		for g := range groups {
			groups[g].n = 0
		}
		for _, p := range pairs {
			groups[p.grp].n++
		}
	}
	// per-group normal matrices of the clock polynomial (fixed: x only)
	for g := range groups {
		groups[g].ok = groups[g].n >= groups[g].deg+3
	}
	S := make([][9]float64, len(groups))
	for _, p := range pairs {
		phi := [3]float64{1, p.x, p.x * p.x}
		gs := &S[p.grp]
		for a := 0; a < 3; a++ {
			for c := 0; c < 3; c++ {
				gs[a*3+c] += phi[a] * phi[c]
			}
		}
	}
	for g := range groups {
		gr := &groups[g]
		if !gr.ok {
			continue
		}
		inv, ok := invSmall(S[g], gr.deg+1)
		gr.inv, gr.ok = inv, ok
	}
	// height and centre
	h := median(hs)
	if st.in.HeightSigmaM > 0 {
		h = st.in.HeightHAE
	}
	centre := cen.Scale(1 / float64(ncen))
	radius := cfg.SearchRadiusM
	if st.in.Prior == PriorHint {
		centre = st.in.Pos
		radius = math.Min(radius, math.Max(5*pos(st.in.SigmaHM, cfg.HintSigmaM), 20e3))
	}
	centre = withHeight(centre, h)
	fr := geo.NewFrameECEF(centre)
	at := func(e, n float64) geo.Vec3 { return withHeight(fr.FromENU(geo.Vec3{X: e, Y: n}), h) }
	T := make([][3]float64, len(groups))
	coef := make([][3]float64, len(groups))
	res := make([]float64, len(pairs))
	score := func(u geo.Vec3, delta float64) float64 {
		clear(T)
		for k := range pairs {
			p := &pairs[k]
			e := p.d0 - p.p.Dist(u)
			res[k] = e
			if !groups[p.grp].ok {
				continue
			}
			t := &T[p.grp]
			t[0] += e
			t[1] += e * p.x
			t[2] += e * p.x * p.x
		}
		clear(coef)
		for g := range groups {
			gr := &groups[g]
			if !gr.ok {
				continue
			}
			n := gr.deg + 1
			for a := 0; a < n; a++ {
				for c := 0; c < n; c++ {
					coef[g][a] += gr.inv[a*3+c] * T[g][c]
				}
			}
		}
		s, n := 0.0, 0
		d2 := delta * delta
		for k := range pairs {
			p := &pairs[k]
			if !groups[p.grp].ok {
				continue
			}
			c := coef[p.grp]
			r := res[k] - (c[0] + c[1]*p.x + c[2]*p.x*p.x)
			s += d2 * (math.Sqrt(1+r*r/d2) - 1)
			n++
		}
		if n == 0 {
			return math.Inf(1)
		}
		return s / float64(n)
	}
	type cand struct {
		e, n, s float64
	}
	search := func(ce, cn float64) []cand {
		step := radius / 10
		var cs []cand
		for a := -10; a <= 10; a++ {
			for c := -10; c <= 10; c++ {
				e, n := ce+float64(a)*step, cn+float64(c)*step
				if math.Hypot(e-ce, n-cn) > radius+1 {
					continue
				}
				cs = append(cs, cand{e, n, score(at(e, n), math.Max(step/2, 30))})
			}
		}
		slices.SortFunc(cs, func(x, y cand) int {
			switch {
			case x.s < y.s:
				return -1
			case x.s > y.s:
				return 1
			}
			return 0
		})
		// the three best that are not neighbours of a better one
		var best []cand
		for _, c := range cs {
			near := false
			for _, o := range best {
				if math.Hypot(c.e-o.e, c.n-o.n) < 2.5*step {
					near = true
					break
				}
			}
			if !near {
				best = append(best, c)
			}
			if len(best) == 3 {
				break
			}
		}
		// refine
		for k := range best {
			c := &best[k]
			for s := step / 2; s >= 30; s /= 2 {
				delta := math.Max(s/2, 30)
				bc := cand{c.e, c.n, score(at(c.e, c.n), delta)}
				for a := -2; a <= 2; a++ {
					for d := -2; d <= 2; d++ {
						if a == 0 && d == 0 {
							continue
						}
						e, n := c.e+float64(a)*s, c.n+float64(d)*s
						if v := score(at(e, n), delta); v < bc.s {
							bc = cand{e, n, v}
						}
					}
				}
				*c = bc
			}
			c.s = score(at(c.e, c.n), 30)
		}
		return best
	}
	best := search(0, 0)
	if len(best) == 0 {
		return geo.Vec3{}, false
	}
	slices.SortFunc(best, func(x, y cand) int {
		switch {
		case x.s < y.s:
			return -1
		case x.s > y.s:
			return 1
		}
		return 0
	})
	if math.Hypot(best[0].e, best[0].n) > 0.9*radius {
		// on the boundary: search once more around it
		more := search(best[0].e, best[0].n)
		if len(more) > 0 && more[0].s < best[0].s {
			slices.SortFunc(more, func(x, y cand) int {
				switch {
				case x.s < y.s:
					return -1
				case x.s > y.s:
					return 1
				}
				return 0
			})
			best = more
		}
	}
	if math.IsInf(best[0].s, 1) {
		return geo.Vec3{}, false
	}
	return at(best[0].e, best[0].n), true
}

// invSmall inverts the leading n×n block (n ≤ 3) of the row-major 3×3 a
// (symmetric positive definite), returning it in the same layout.
func invSmall(a [9]float64, n int) ([9]float64, bool) {
	var out [9]float64
	switch n {
	case 1:
		if !(a[0] > 0) {
			return out, false
		}
		out[0] = 1 / a[0]
	case 2:
		det := a[0]*a[4] - a[1]*a[3]
		if !(det > 1e-12*a[0]*a[4]) {
			return out, false
		}
		out[0], out[1], out[3], out[4] = a[4]/det, -a[1]/det, -a[3]/det, a[0]/det
	default:
		c0 := a[4]*a[8] - a[5]*a[7]
		c1 := a[5]*a[6] - a[3]*a[8]
		c2 := a[3]*a[7] - a[4]*a[6]
		det := a[0]*c0 + a[1]*c1 + a[2]*c2
		if !(math.Abs(det) > 1e-12*a[0]*a[4]*a[8]) {
			return out, false
		}
		out[0] = c0 / det
		out[1] = (a[2]*a[7] - a[1]*a[8]) / det
		out[2] = (a[1]*a[5] - a[2]*a[4]) / det
		out[3] = c1 / det
		out[4] = (a[0]*a[8] - a[2]*a[6]) / det
		out[5] = (a[2]*a[3] - a[0]*a[5]) / det
		out[6] = c2 / det
		out[7] = (a[1]*a[6] - a[0]*a[7]) / det
		out[8] = (a[0]*a[4] - a[1]*a[3]) / det
	}
	return out, true
}
