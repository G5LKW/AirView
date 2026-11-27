package group

import (
	"math"
	"math/bits"

	"airview/internal/ingest"
	"airview/internal/model"
)

// header identifies the transmission of a cluster.
type header struct {
	hash    uint64
	key     uint32
	df      uint8
	kind    model.FrameKind
	data    [14]byte
	dlen    uint8
	firstNs int64
}

func headerOf(g *ingest.Group) header {
	h := header{hash: g.Hash, key: g.Key, df: g.DF, kind: g.Kind, firstNs: g.FirstNs}
	h.dlen = uint8(copy(h.data[:], g.Data))
	return h
}

// emit builds the cluster of the members in mask (indices into cs), decides
// its use, retains it for late copies and hands it to the sink.
func (a *Assembler) emit(h header, cs []cand, mask uint64, part, parts uint8, seq uint64, altFt float64, altSrc uint8,
	hinted, conflict, late bool) {
	// Members in index order; then sites by union-find over co-located pairs.
	k := 0
	for m := mask; m != 0; m &= m - 1 {
		a.idx[k] = int8(bits.TrailingZeros64(m))
		k++
	}
	var parent [MaxMembers]int8
	for i := 0; i < k; i++ {
		parent[i] = int8(i)
	}
	find := func(i int8) int8 {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	var maxRatio float64
	for i := 0; i < k; i++ {
		ii := a.idx[i]
		for j := i + 1; j < k; j++ {
			jj := a.idx[j]
			if r := float64(a.srch.ratio[ii][jj]); r > maxRatio {
				maxRatio = r
			}
			if a.colo[ii]&(1<<uint(jj)) != 0 {
				if ri, rj := find(int8(i)), find(int8(j)); ri != rj {
					parent[max(ri, rj)] = min(ri, rj)
				}
			}
		}
	}
	// Site numbers in order of their lowest member index.
	ns := 0
	var siteNo [MaxMembers]int8
	for i := 0; i < k; i++ {
		r := find(int8(i))
		if r == int8(i) {
			siteNo[i] = int8(ns)
			ns++
		}
		a.siteOf[i] = r
	}
	for i := 0; i < k; i++ {
		a.siteOf[i] = siteNo[a.siteOf[i]]
	}
	if ns < 2 {
		a.stats.dropped.Add(1)
		return
	}

	// Order members by (site, variance) into the output buffer.
	var perm [MaxMembers]int8
	for i := 0; i < k; i++ {
		perm[i] = int8(i)
		for j := i; j > 0; j-- {
			p, q := perm[j], perm[j-1]
			sp, sq := a.siteOf[p], a.siteOf[q]
			if sp < sq || (sp == sq && cs[a.idx[p]].v < cs[a.idx[q]].v) {
				perm[j], perm[j-1] = q, p
				continue
			}
			break
		}
	}
	cl := &a.cl
	*cl = Cluster{Seq: seq, Hash: h.hash, Key: h.key, DF: h.df, Kind: h.kind, Data: h.data, DataLen: h.dlen,
		FirstNs: h.firstNs, AltFt: altFt, AltSrc: altSrc, Part: part, Parts: parts, Hinted: hinted,
		HintConflict: conflict, MaxRatio: maxRatio, Late: late}
	ms := a.members[:k]
	for o := 0; o < k; o++ {
		c := &cs[a.idx[perm[o]]]
		ms[o] = Member{Station: c.st, Channel: c.ch, Site: uint8(a.siteOf[perm[o]]), Late: c.late, Epoch: c.epoch,
			Ticks: c.ticks, T: c.t, ClockVar: c.cv, JitterVar: c.jv, Pos: c.pos, PosSigmaM: c.ps, Signal: c.sig, W: 1}
	}
	cl.Members = ms
	cl.Sites = a.fuse(ms, ns)
	best := math.Inf(1)
	for i := range cl.Sites {
		if v := cl.Sites[i].Var; v < best {
			best, cl.Ref = v, i
		}
	}

	switch {
	case ns >= a.cfg.MinSites:
		cl.Use = UseSolve
	case ns >= a.cfg.MinSitesAlt && altSrc != AltNone:
		cl.Use = UseSolveAlt
	default:
		cl.Use = UseTrack
	}
	if cl.Use >= UseSolveAlt && a.sel != nil && !late && !a.sel.Admit(h.key, ns, h.firstNs) {
		cl.Use = UseTrack
		a.stats.notAdmitted.Add(1)
	}
	switch cl.Use {
	case UseSolve:
		a.stats.solve.Add(1)
	case UseSolveAlt:
		a.stats.solveAlt.Add(1)
	default:
		a.stats.track.Add(1)
	}
	if late {
		a.stats.lateClusters.Add(1)
	} else if a.late != nil {
		a.late.retain(cl, a.cfg.LateMembers)
	}
	a.stats.clusters.Add(1)
	a.stats.members.Add(uint64(k))
	a.sink.Cluster(cl)
}

// fuse computes the sites of the members (grouped by site) and their fusion
// weights (package documentation, "Co-located stations").
func (a *Assembler) fuse(ms []Member, ns int) []Site {
	sites := a.sitesB[:ns]
	rhoMin := a.cfg.FuseMinRangeM
	i := 0
	for s := 0; s < ns; s++ {
		j := i
		for j < len(ms) && int(ms[j].Site) == s {
			j++
		}
		st := &sites[s]
		*st = Site{First: uint8(i), Count: uint8(j - i)}
		if j-i == 1 {
			m := &ms[i]
			st.Pos, st.PosSigmaM, st.T, st.Var = m.Pos, m.PosSigmaM, m.T, max(m.Var(), 1e-6)
			i = j
			continue
		}
		var wsum float64
		for k := i; k < j; k++ {
			ms[k].W = 1 / max(ms[k].Var(), 1e-6)
			wsum += ms[k].W
		}
		t0 := ms[i].T
		var dt, ps2, v float64
		for k := i; k < j; k++ {
			m := &ms[k]
			m.W /= wsum
			st.Pos = st.Pos.Add(m.Pos.Scale(m.W))
			dt += m.W * m.T.Sub(t0)
			ps2 += m.W * m.W * m.PosSigmaM * m.PosSigmaM
			v += m.W * m.W * max(m.Var(), 1e-6)
		}
		if a.cross != nil {
			for k := i; k < j; k++ {
				for l := k + 1; l < j; l++ {
					v += 2 * ms[k].W * ms[l].W * a.cross.CrossCov(ms[k].Station, ms[l].Station)
				}
			}
		}
		// Second-order geometry bound: b = Σ w_k |r_k − r̄|² / (2 ρ_min).
		var b float64
		for k := i; k < j; k++ {
			d := ms[k].Pos.Sub(st.Pos)
			b += ms[k].W * d.Dot(d)
		}
		b /= 2 * rhoMin
		v += (b / mPerNs) * (b / mPerNs)
		st.T = t0.Add(dt)
		st.PosSigmaM = math.Sqrt(ps2)
		st.Var = max(v, 1e-6)
		i = j
	}
	return sites
}
