package survey

import (
	"math"
	"sync"
	"sync/atomic"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

// passOut is one linearisation of the bundle at the current positions:
// the reduced normal equations of the positions H·δ = g (clocks and
// emission times eliminated), the weighted cost and its degrees of
// freedom; per jackknife group the data part of H and g.
type passOut struct {
	P    int
	H    []float64
	g    []float64
	cost float64
	dof  float64
	Hg   [][]float64
	gg   [][]float64
}

// maxFrameRecs bounds the receptions of one frame the solver uses.
const maxFrameRecs = 64

// scratch is one worker's memory for the window passes.
type scratch struct {
	// per frame
	idx  []int32 // reception indices (active)
	rej  []int32 // rejected receptions of the frame
	R    []float64
	v0   []float64
	h    []geo.Vec3
	phi  [][3]float64
	C    []float64
	Q    []float64
	q    []float64
	work []float64
	// per window: geometry of its receptions (range and gradient with
	// respect to the station, from rbase on)
	rbase        int32
	wR           []float64
	wH           []geo.Vec3
	A, Bm, Cp, Y []float64
	bc, bp, as   []float64
	red          []float64
	Aorig        []float64
	r            [maxFrameRecs]float64
	loc          []int32 // station → local slot (−1)
	locSt        []int32 // local slot → station
	blkLoc       []int32 // global block → local parameter offset
	// accumulators
	H, g      []float64
	cost, dof float64
}

// scratchFor returns worker k's scratch, reused across passes (its
// per-frame and per-window buffers keep their capacity) with the
// accumulators cleared; a new one when the problem's size changed.
func (b *bundle) scratchFor(k int) *scratch {
	for len(b.scratch) <= k {
		b.scratch = append(b.scratch, nil)
	}
	P := 3 * b.nUsed
	sc := b.scratch[k]
	if sc == nil || len(sc.H) != P*P || len(sc.blkLoc) != len(b.blocks) || len(sc.loc) != len(b.st) {
		sc = b.newScratch()
		b.scratch[k] = sc
		return sc
	}
	clear(sc.H)
	clear(sc.g)
	sc.cost, sc.dof = 0, 0
	return sc
}

func (b *bundle) newScratch() *scratch {
	sc := &scratch{loc: make([]int32, len(b.st)), blkLoc: make([]int32, len(b.blocks))}
	for i := range sc.loc {
		sc.loc[i] = -1
	}
	P := 3 * b.nUsed
	sc.H = make([]float64, P*P)
	sc.g = make([]float64, P)
	return sc
}

func grow(s []float64, n int) []float64 {
	if cap(s) < n {
		return make([]float64, n)
	}
	s = s[:n]
	clear(s)
	return s
}

// pass linearises the bundle at the current positions. With groups it
// also returns the jackknife groups' data parts. Residuals (brec.z) are
// refreshed.
func (b *bundle) pass(groups int) *passOut {
	P := 3 * b.nUsed
	out := &passOut{P: P, H: make([]float64, P*P), g: make([]float64, P)}
	var gmu []sync.Mutex
	if groups > 0 {
		out.Hg = make([][]float64, groups)
		out.gg = make([][]float64, groups)
		gmu = make([]sync.Mutex, groups)
		for i := range groups {
			out.Hg[i] = make([]float64, P*P)
			out.gg[i] = make([]float64, P)
		}
		for i := range b.wins {
			b.wins[i].group = i * groups / len(b.wins)
		}
	}
	nw := min(b.cfg.Workers, len(b.wins))
	var next atomic.Int64
	var wg sync.WaitGroup
	var mu sync.Mutex
	for k := range max(nw, 1) {
		sc := b.scratchFor(k)
		wg.Add(1)
		go func() {
			defer wg.Done()
			var gH, gg []float64
			if groups > 0 {
				gH = make([]float64, P*P)
				gg = make([]float64, P)
			}
			for {
				i := int(next.Add(1)) - 1
				if i >= len(b.wins) {
					break
				}
				w := &b.wins[i]
				if groups > 0 {
					clear(gH)
					clear(gg)
					b.window(w, sc, gH, gg)
					gm := &gmu[w.group]
					gm.Lock()
					linalg.Axpy(1, gH, out.Hg[w.group])
					linalg.Axpy(1, gg, out.gg[w.group])
					gm.Unlock()
				} else {
					b.window(w, sc, nil, nil)
				}
			}
			mu.Lock()
			linalg.Axpy(1, sc.H, out.H)
			linalg.Axpy(1, sc.g, out.g)
			out.cost += sc.cost
			out.dof += sc.dof
			mu.Unlock()
		}()
	}
	wg.Wait()
	out.dof -= float64(P)
	return out
}

// frame prepares the active receptions of frame f: ranges, gradients,
// clock basis, the covariance C (station noise / weight + aircraft
// position error) and the τ-eliminating precision Q = C⁻¹ − C⁻¹11ᵀC⁻¹ /
// 1ᵀC⁻¹1. It returns the number of active receptions (0: unusable).
func (b *bundle) frame(f *bframe, sc *scratch) int {
	sc.idx = sc.idx[:0]
	sc.rej = sc.rej[:0]
	for ri := f.r0; ri < f.r0+f.nr; ri++ {
		r := &b.recs[ri]
		if r.blk < 0 {
			continue
		}
		if r.w <= 0 {
			sc.rej = append(sc.rej, ri)
			continue
		}
		if len(sc.idx) < maxFrameRecs {
			sc.idx = append(sc.idx, ri)
		}
	}
	m := len(sc.idx)
	if m < 2 {
		return 0
	}
	sc.R = grow(sc.R, m)
	sc.v0 = grow(sc.v0, m)
	if cap(sc.h) < m {
		sc.h = make([]geo.Vec3, m)
		sc.phi = make([][3]float64, m)
	}
	sc.h, sc.phi = sc.h[:m], sc.phi[:m]
	sc.C = grow(sc.C, m) // the diagonal noise
	var gp [maxFrameRecs]geo.Vec3
	x := f.x
	for i, ri := range sc.idx {
		r := &b.recs[ri]
		st := &b.st[r.st]
		k := ri - sc.rbase
		L, hu := sc.wR[k], sc.wH[k]
		sc.R[i] = L
		sc.h[i] = hu
		// the aircraft-side gradient only weights the position error:
		// the chord direction is close enough
		gp[i] = f.p.Sub(st.u).Unit()
		// the clock is read at the arrival: its rate error scales the range
		xi := x + L/(cLight*b.cfg.WindowS/2)
		sc.phi[i] = [3]float64{1, xi, xi * xi}
		sc.v0[i] = r.y - L
		sc.C[i] = st.sig2 / float64(r.w)
	}
	// C = D + G·Σp·Gᵀ (D the station noise, G the unit vectors station →
	// aircraft): C⁻¹ by Woodbury with Σp = Lp·Lpᵀ, U = G·Lp:
	// C⁻¹ = D⁻¹ − D⁻¹U·(I + UᵀD⁻¹U)⁻¹·UᵀD⁻¹ (3×3 inverse, O(m²)).
	c := &f.cov
	cp := linalg.Mat3{c[0], c[1], c[2], c[1], c[3], c[4], c[2], c[4], c[5]}
	lp, err := cp.Chol()
	if err != nil {
		lp = linalg.Mat3{}
	}
	var u [maxFrameRecs][3]float64
	M := linalg.Identity3()
	for i := 0; i < m; i++ {
		g := gp[i]
		// uᵢ = Lpᵀ·gᵢ
		ui := [3]float64{lp[0]*g.X + lp[3]*g.Y + lp[6]*g.Z, lp[4]*g.Y + lp[7]*g.Z, lp[8] * g.Z}
		u[i] = ui
		di := 1 / sc.C[i]
		for a := 0; a < 3; a++ {
			for k := 0; k < 3; k++ {
				M[a*3+k] += ui[a] * ui[k] * di
			}
		}
	}
	Mi, err := M.InvSPD()
	if err != nil {
		return 0
	}
	sc.work = grow(sc.work, m)
	sc.Q = grow(sc.Q, m*m)
	for i := 0; i < m; i++ {
		di := 1 / sc.C[i]
		// wᵢ = Mi·uᵢ·dᵢ⁻¹
		ui := u[i]
		w0 := (Mi[0]*ui[0] + Mi[1]*ui[1] + Mi[2]*ui[2]) * di
		w1 := (Mi[3]*ui[0] + Mi[4]*ui[1] + Mi[5]*ui[2]) * di
		w2 := (Mi[6]*ui[0] + Mi[7]*ui[1] + Mi[8]*ui[2]) * di
		row := sc.Q[i*m : i*m+m]
		for j := 0; j <= i; j++ {
			uj := u[j]
			v := -(w0*uj[0] + w1*uj[1] + w2*uj[2]) / sc.C[j]
			if j == i {
				v += di
			}
			row[j] = v
			sc.Q[j*m+i] = v
		}
	}
	sc.q = grow(sc.q, m)
	sq := 0.0
	for i := 0; i < m; i++ {
		s := 0.0
		for j := 0; j < m; j++ {
			s += sc.Q[i*m+j]
		}
		sc.q[i] = s
		sq += s
	}
	if !(sq > 0) {
		return 0
	}
	for i := 0; i < m; i++ {
		for j := 0; j < m; j++ {
			sc.Q[i*m+j] -= sc.q[i] * sc.q[j] / sq
		}
	}
	// keep 1/sq (variance of the emission estimate) in work[0] for the
	// residuals of rejected receptions; q is C⁻¹1
	sc.work[0] = sq
	return m
}

// window assembles one clock window: the frames' contributions to the
// clock blocks (A), the clock-position coupling (Bm) and the positions
// (Cp), eliminates the clocks (Schur complement) into sc.H / sc.g (and
// gH / gg when given), and refreshes the residuals with the eliminated
// clocks.
func (b *bundle) window(w *bwin, sc *scratch, gH, gg []float64) {
	// local parameters
	Lc := 0
	ns := 0
	sc.locSt = sc.locSt[:0]
	for _, bi := range w.blocks {
		blk := &b.blocks[bi]
		sc.blkLoc[bi] = int32(Lc)
		Lc += blk.deg + 1
		if sc.loc[blk.st] < 0 {
			sc.loc[blk.st] = int32(ns)
			sc.locSt = append(sc.locSt, blk.st)
			ns++
		}
	}
	defer func() {
		for _, s := range sc.locSt {
			sc.loc[s] = -1
		}
	}()
	Lp := 3 * ns
	b.geometry(w, sc)
	sc.A = grow(sc.A, Lc*Lc)
	sc.Bm = grow(sc.Bm, Lc*Lp)
	sc.Cp = grow(sc.Cp, Lp*Lp)
	sc.bc = grow(sc.bc, Lc)
	sc.bp = grow(sc.bp, Lp)
	cost0 := 0.0
	dof := 0.0
	r := sc.r[:]
	for fi := w.f0; fi < w.f0+w.nf; fi++ {
		f := &b.frames[fi]
		if !f.used {
			continue
		}
		m := b.frame(f, sc)
		if m == 0 {
			continue
		}
		dof += float64(m - 1)
		Q := sc.Q
		for i := 0; i < m; i++ {
			s := 0.0
			for j := 0; j < m; j++ {
				s += Q[i*m+j] * sc.v0[j]
			}
			r[i] = s
			cost0 += sc.v0[i] * s
		}
		for i := 0; i < m; i++ {
			rec := &b.recs[sc.idx[i]]
			li := int(sc.loc[rec.st]) * 3
			ci := int(sc.blkLoc[rec.blk])
			di := b.blocks[rec.blk].deg + 1
			hi := sc.h[i]
			pi := &sc.phi[i]
			sc.bp[li] += hi.X * r[i]
			sc.bp[li+1] += hi.Y * r[i]
			sc.bp[li+2] += hi.Z * r[i]
			for a := 0; a < di; a++ {
				sc.bc[ci+a] += pi[a] * r[i]
			}
			for j := 0; j < m; j++ {
				q := Q[i*m+j]
				if q == 0 {
					continue
				}
				rj := &b.recs[sc.idx[j]]
				lj := int(sc.loc[rj.st]) * 3
				cj := int(sc.blkLoc[rj.blk])
				dj := b.blocks[rj.blk].deg + 1
				hj := sc.h[j]
				pj := &sc.phi[j]
				hiv := [3]float64{hi.X, hi.Y, hi.Z}
				hjv := [3]float64{hj.X, hj.Y, hj.Z}
				for a := 0; a < 3; a++ {
					row := sc.Cp[(li+a)*Lp+lj:]
					qa := q * hiv[a]
					row[0] += qa * hjv[0]
					row[1] += qa * hjv[1]
					row[2] += qa * hjv[2]
				}
				for a := 0; a < di; a++ {
					qa := q * pi[a]
					rowA := sc.A[(ci+a)*Lc+cj:]
					for c := 0; c < dj; c++ {
						rowA[c] += qa * pj[c]
					}
					rowB := sc.Bm[(ci+a)*Lp+lj:]
					rowB[0] += qa * hjv[0]
					rowB[1] += qa * hjv[1]
					rowB[2] += qa * hjv[2]
				}
			}
		}
	}
	// eliminate the clocks: A + ridge (the common clock of all stations in
	// the window is the emission time's gauge: A is singular along it, b
	// and Bm are orthogonal to it, so the ridge only fixes that direction)
	maxD := 0.0
	maxDeg := 0
	for i := 0; i < Lc; i++ {
		maxD = math.Max(maxD, sc.A[i*Lc+i])
	}
	for _, bi := range w.blocks {
		maxDeg = max(maxDeg, b.blocks[bi].deg)
	}
	ridge := maxD*1e-11 + 1e-12
	for i := 0; i < Lc; i++ {
		sc.A[i*Lc+i] += ridge
	}
	sc.Aorig = grow(sc.Aorig, Lc*Lc)
	copy(sc.Aorig, sc.A)
	if _, err := linalg.CholeskyJitter(sc.A, sc.Aorig, Lc, 1e-10, 6); err != nil {
		return
	}
	sc.Y = grow(sc.Y, Lc*Lp)
	copy(sc.Y, sc.Bm)
	linalg.CholSolveMulti(sc.A, Lc, sc.Y, Lp)
	sc.as = grow(sc.as, Lc)
	copy(sc.as, sc.bc)
	linalg.CholSolve(sc.A, Lc, sc.as)
	// reduced: Cp − Bmᵀ·Y, bp − Bmᵀ·a*
	sc.red = grow(sc.red, Lp*Lp)
	red := sc.red
	linalg.MatMulAtB(red, sc.Bm, sc.Y, Lc, Lp, Lp)
	costW := cost0 - linalg.Dot(sc.bc, sc.as)
	dof -= float64(Lc - (maxDeg + 1))
	P := 3 * b.nUsed
	for a := 0; a < ns; a++ {
		ga := b.st[sc.locSt[a]].slot * 3
		for c := 0; c < ns; c++ {
			gc := b.st[sc.locSt[c]].slot * 3
			for i := 0; i < 3; i++ {
				for j := 0; j < 3; j++ {
					v := sc.Cp[(3*a+i)*Lp+3*c+j] - red[(3*a+i)*Lp+3*c+j]
					sc.H[(ga+i)*P+gc+j] += v
					if gH != nil {
						gH[(ga+i)*P+gc+j] += v
					}
				}
			}
		}
		for i := 0; i < 3; i++ {
			// column 3a+i of Bm dotted with a*
			v := sc.bp[3*a+i]
			s := 0.0
			for k := 0; k < Lc; k++ {
				s += sc.Bm[k*Lp+3*a+i] * sc.as[k]
			}
			v -= s
			sc.g[ga+i] += v
			if gg != nil {
				gg[ga+i] += v
			}
		}
	}
	sc.cost += costW
	sc.dof += dof
	// residuals with the eliminated clocks
	for fi := w.f0; fi < w.f0+w.nf; fi++ {
		f := &b.frames[fi]
		if !f.used {
			continue
		}
		m := b.frame(f, sc)
		if m == 0 {
			for ri := f.r0; ri < f.r0+f.nr; ri++ {
				b.recs[ri].z = 0
			}
			continue
		}
		sq := sc.work[0]
		v := r[:m]
		qv := 0.0
		for i := 0; i < m; i++ {
			rec := &b.recs[sc.idx[i]]
			ci := int(sc.blkLoc[rec.blk])
			pi := &sc.phi[i]
			c := 0.0
			for a := 0; a <= b.blocks[rec.blk].deg; a++ {
				c += pi[a] * sc.as[ci+a]
			}
			v[i] = sc.v0[i] - c
			qv += sc.q[i] * v[i]
		}
		for i := 0; i < m; i++ {
			s := 0.0
			for j := 0; j < m; j++ {
				s += sc.Q[i*m+j] * v[j]
			}
			rec := &b.recs[sc.idx[i]]
			z := s / math.Sqrt(math.Max(sc.Q[i*m+i], 1e-300))
			rec.z = float32(z / math.Sqrt(float64(rec.w)))
		}
		// rejected receptions: against the frame's emission estimate
		tau := qv / sq
		for _, ri := range sc.rej {
			rec := &b.recs[ri]
			st := &b.st[rec.st]
			L := sc.wR[ri-sc.rbase]
			ci := int(sc.blkLoc[rec.blk])
			c := 0.0
			xi := f.x + L/(cLight*b.cfg.WindowS/2)
			phi := [3]float64{1, xi, xi * xi}
			for a := 0; a <= b.blocks[rec.blk].deg; a++ {
				c += phi[a] * sc.as[ci+a]
			}
			e := rec.y - L - c - tau
			rec.z = float32(e / math.Sqrt(st.sig2+1/sq))
		}
	}
}

// geometry computes the range and its gradient (station side) of every
// reception of window w once per pass (the refraction model is the
// expensive part of a pass).
func (b *bundle) geometry(w *bwin, sc *scratch) {
	first := &b.frames[w.f0]
	last := &b.frames[w.f0+w.nf-1]
	sc.rbase = first.r0
	n := int(last.r0 + last.nr - first.r0)
	if cap(sc.wR) < n {
		sc.wR = make([]float64, n)
		sc.wH = make([]geo.Vec3, n)
	}
	sc.wR, sc.wH = sc.wR[:n], sc.wH[:n]
	for fi := w.f0; fi < w.f0+w.nf; fi++ {
		f := &b.frames[fi]
		if !f.used {
			continue
		}
		for ri := f.r0; ri < f.r0+f.nr; ri++ {
			r := &b.recs[ri]
			if r.blk < 0 {
				continue
			}
			k := ri - sc.rbase
			sc.wR[k], sc.wH[k] = b.atm.RangeGrad(f.p, b.st[r.st].u)
		}
	}
}

// addPriors adds the position priors to a pass (cost, H, g).
func (b *bundle) addPriors(o *passOut) {
	P := o.P
	for i := range b.st {
		st := &b.st[i]
		if st.slot < 0 {
			continue
		}
		k := st.slot * 3
		add := func(e geo.Vec3, res, sig float64) {
			w := 1 / (sig * sig)
			o.cost += w * res * res
			ev := [3]float64{e.X, e.Y, e.Z}
			for a := 0; a < 3; a++ {
				o.g[k+a] += w * ev[a] * res
				for c := 0; c < 3; c++ {
					o.H[(k+a)*P+k+c] += w * ev[a] * ev[c]
				}
			}
		}
		if st.hOn {
			fr := geo.NewFrameECEF(st.m)
			d := st.m.Sub(st.u)
			add(fr.East(), d.Dot(fr.East()), st.hSig)
			add(fr.North(), d.Dot(fr.North()), st.hSig)
		}
		l := geo.FromECEF(st.u)
		add(geo.Up(l.Lat, l.Lon), st.mH-l.H, st.vSig)
	}
}

// step solves H·δ = g and applies δ (bounded per station by MaxStepM).
// It returns the step and the largest station step (m).
func (b *bundle) step(o *passOut) ([]float64, float64, error) {
	P := o.P
	L := append([]float64(nil), o.H...)
	if _, err := linalg.CholeskyJitter(L, o.H, P, 1e-12, 8); err != nil {
		return nil, 0, err
	}
	d := append([]float64(nil), o.g...)
	linalg.CholSolve(L, P, d)
	maxS := 0.0
	for i := range b.st {
		st := &b.st[i]
		if st.slot < 0 {
			continue
		}
		k := st.slot * 3
		v := geo.Vec3{X: d[k], Y: d[k+1], Z: d[k+2]}
		n := v.Norm()
		if n > b.cfg.MaxStepM {
			v = v.Scale(b.cfg.MaxStepM / n)
			d[k], d[k+1], d[k+2] = v.X, v.Y, v.Z
			n = b.cfg.MaxStepM
		}
		maxS = math.Max(maxS, n)
		st.u = st.u.Add(v)
	}
	return d, maxS, nil
}

func (b *bundle) unstep(d []float64, frac float64) {
	for i := range b.st {
		st := &b.st[i]
		if st.slot < 0 {
			continue
		}
		k := st.slot * 3
		st.u = st.u.Sub(geo.Vec3{X: d[k], Y: d[k+1], Z: d[k+2]}.Scale(frac))
	}
}

// gn runs Gauss-Newton with the current weights and priors until the
// largest station step is below 1 mm (or MaxIter), with step halving when
// the cost rises. It ends with a pass at the final positions (b.final).
func (b *bundle) gn() error {
	prev := math.Inf(1)
	var last []float64
	lastMax := math.Inf(1)
	halvings := 0
	for it := 0; ; it++ {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		o := b.pass(0)
		b.addPriors(o)
		if last != nil && o.cost > prev*(1+1e-9)+1e-6 && halvings < 8 {
			for k := range last {
				last[k] /= 2
			}
			b.unstep(last, 1)
			lastMax /= 2
			halvings++
			continue
		}
		halvings = 0
		b.final = o
		if lastMax < 1e-3 || it >= b.cfg.MaxIter {
			return nil
		}
		prev = o.cost
		d, mx, err := b.step(o)
		if err != nil {
			return err
		}
		b.iters++
		last, lastMax = d, mx
	}
}

// solve runs rounds of Gauss-Newton and reweighting (learned jitter per
// station, Huber weights, rejections).
func (b *bundle) solve(rounds int) error {
	for round := 0; round < rounds; round++ {
		if err := b.gn(); err != nil {
			return err
		}
		if round < rounds-1 {
			b.reweight(!(b.stage == 1 && round == 0))
		}
	}
	return nil
}

// reweight learns every station's timing noise from its standardised
// residuals (median of z² at the χ²₁ median, corrected for the clock
// block's leverage) and sets the Huber weights.
func (b *bundle) reweight(huber bool) {
	cfg := b.cfg
	z2 := make([][]float64, len(b.st))
	for i := range z2 {
		z2[i] = make([]float64, 0, b.st[i].nrec)
	}
	for ri := range b.recs {
		r := &b.recs[ri]
		if r.blk < 0 {
			continue
		}
		blk := &b.blocks[r.blk]
		lev := float64(blk.n) / math.Max(float64(blk.n-blk.deg-1), 1)
		z := float64(r.z)
		z2[r.st] = append(z2[r.st], z*z*lev)
	}
	fac := make([]float64, len(b.st))
	for i := range b.st {
		st := &b.st[i]
		fac[i] = 1
		if st.slot < 0 || len(z2[i]) < 10 {
			continue
		}
		f := median(z2[i]) / 0.4549
		f = math.Min(math.Max(f, 0.04), 25)
		st.sig2 = math.Min(math.Max(st.sig2*f, 0.09), 3e4*3e4)
		fac[i] = f
	}
	for i := range b.st {
		b.st[i].nrej = 0
	}
	for ri := range b.recs {
		r := &b.recs[ri]
		if r.blk < 0 {
			continue
		}
		z := math.Abs(float64(r.z)) / math.Sqrt(fac[r.st])
		r.z = float32(float64(r.z) / math.Sqrt(fac[r.st]))
		w := 1.0
		if huber {
			switch {
			case z > cfg.RejectK:
				w = 0
			case z > cfg.HuberK:
				w = cfg.HuberK / z
			}
		}
		r.w = float32(w)
		if w == 0 {
			b.st[r.st].nrej++
		}
	}
}

// covariance computes the formal and jackknife covariance of every
// station at the final positions (one pass with jackknife groups).
func (b *bundle) covariance() ([]float64, error) {
	cfg := b.cfg
	G := cfg.JackknifeGroups
	if len(b.wins) < 2*G {
		G = len(b.wins) / 2
	}
	if G < 4 {
		G = 0
	}
	o := b.pass(G)
	if o.dof > 0 {
		b.vf = o.cost / o.dof
	}
	b.addPriors(o)
	b.final = o
	P := o.P
	S := append([]float64(nil), o.H...)
	orig := append([]float64(nil), o.H...)
	if _, err := linalg.CholeskyJitter(S, orig, P, 1e-12, 8); err != nil {
		return nil, err
	}
	d0 := append([]float64(nil), o.g...)
	linalg.CholSolve(S, P, d0)
	if err := linalg.CholToInverse(S, P, nil); err != nil {
		return nil, err
	}
	vf := math.Max(1, b.vf)
	for i := range b.st {
		st := &b.st[i]
		if st.slot < 0 {
			continue
		}
		k := st.slot * 3
		for a := 0; a < 3; a++ {
			for c := 0; c < 3; c++ {
				st.formal[a*3+c] = S[(k+a)*P+k+c] * vf
			}
		}
		st.jkOK = false
	}
	b.jkGroups = 0
	if G >= 4 {
		reps := make([][]float64, 0, G)
		for g := 0; g < G; g++ {
			Hm := make([]float64, P*P)
			for k := range Hm {
				Hm[k] = o.H[k] - o.Hg[g][k]
			}
			Lm := append([]float64(nil), Hm...)
			if _, err := linalg.CholeskyJitter(Lm, Hm, P, 1e-12, 8); err != nil {
				continue
			}
			d := make([]float64, P)
			for k := range d {
				d[k] = o.g[k] - o.gg[g][k]
			}
			linalg.CholSolve(Lm, P, d)
			reps = append(reps, d)
		}
		m := len(reps)
		if m >= 4 {
			b.jkGroups = m
			mean := make([]float64, P)
			for _, d := range reps {
				linalg.Axpy(1/float64(m), d, mean)
			}
			infl := math.Pow(1+1/math.Sqrt(2*float64(m-1)), 2)
			for i := range b.st {
				st := &b.st[i]
				if st.slot < 0 {
					continue
				}
				k := st.slot * 3
				var c [9]float64
				for _, d := range reps {
					for a := 0; a < 3; a++ {
						for e := 0; e < 3; e++ {
							c[a*3+e] += (d[k+a] - mean[k+a]) * (d[k+e] - mean[k+e])
						}
					}
				}
				for a := range c {
					c[a] *= float64(m-1) / float64(m) * infl
				}
				st.jk, st.jkOK = c, true
			}
		}
	}
	for i := range b.st {
		st := &b.st[i]
		if st.slot < 0 {
			continue
		}
		st.final = st.formal
		if st.jkOK && b.jkGroups >= 8 && st.jk[0]+st.jk[4]+st.jk[8] > st.formal[0]+st.formal[4]+st.formal[8] {
			st.final = st.jk
		}
		fl := cfg.SysFloorM
		if st.deg > 1 {
			fl = cfg.SysFloorFreeM
		}
		st.final[0] += fl * fl
		st.final[4] += fl * fl
		st.final[8] += fl * fl
		if st.deg > 1 {
			// their level-dependent timing biases go mostly into the
			// height (simnet SDR: 2-3 formal σ): a wider vertical floor
			// along the local up direction
			up := geo.Up(geo.FromECEF(st.u).Lat, geo.FromECEF(st.u).Lon)
			uv := [3]float64{up.X, up.Y, up.Z}
			extra := 8 * fl * fl // (3·floor)² − floor²
			for a := 0; a < 3; a++ {
				for c := 0; c < 3; c++ {
					st.final[a*3+c] += extra * uv[a] * uv[c]
				}
			}
		}
		fr := geo.NewFrameECEF(st.u)
		e := fr.CovToENU(st.final)
		st.sigH = math.Sqrt(math.Max(eigMax2(e[0], e[1], e[4]), 0))
		st.sigV = math.Sqrt(math.Max(e[8], 0))
		st.determined = st.sigH <= cfg.MaxSigmaHM
	}
	return S, nil
}

// priorTest computes the data-only position of every station with an
// active horizontal prior (its own prior removed, linearised at the
// solution, others kept) and its distance from the prior. S is the formal
// inverse of the normal matrix (unscaled).
func (b *bundle) priorTest(S []float64) {
	cfg := b.cfg
	P := 3 * b.nUsed
	for i := range b.st {
		st := &b.st[i]
		if st.slot < 0 || !st.hOn || st.in.Prior == PriorHint {
			continue
		}
		k := st.slot * 3
		var Sss linalg.Mat3
		for a := 0; a < 3; a++ {
			for c := 0; c < 3; c++ {
				Sss[a*3+c] = S[(k+a)*P+k+c]
			}
		}
		fr := geo.NewFrameECEF(st.m)
		E, N := fr.East(), fr.North()
		w := 1 / (st.hSig * st.hSig)
		lam := linalg.Mat3(geoOuter(E, E).Add(geoOuter(N, N)).Scale(w))
		info, err := Sss.Symmetrize().InvSPD()
		if err != nil {
			st.tested = false
			continue
		}
		Cd, err := info.Sub(lam).Symmetrize().InvSPD()
		if err != nil {
			st.tested = false
			continue
		}
		dm := st.m.Sub(st.u)
		corr := Cd.MulVec(lam.MulVec(linalg.Vec3{dm.X, dm.Y, dm.Z}))
		x := st.u.Sub(geo.Vec3{X: corr[0], Y: corr[1], Z: corr[2]})
		// horizontal σ of the data-only position, scaled like the final
		// covariance (jackknife inflation, variance factor)
		var cd [9]float64
		copy(cd[:], Cd[:])
		e := fr.CovToENU(cd)
		sh := math.Sqrt(math.Max(eigMax2(e[0], e[1], e[4]), 0))
		if tf := st.formal[0] + st.formal[4] + st.formal[8]; tf > 0 {
			sh *= math.Sqrt(math.Max(1, (st.final[0]+st.final[4]+st.final[8])/tf))
		}
		if b.vf > 1 {
			sh *= math.Sqrt(b.vf)
		}
		d := fr.ToENU(x)
		st.tested = true
		st.dataOnly = x
		st.dataSigH = sh
		st.distH = math.Hypot(d.X, d.Y)
		st.limit = math.Max(cfg.OutlierMinM, cfg.OutlierK*sh)
	}
}

// eigMax2 is the larger eigenvalue of the symmetric 2×2 [[a b] [b c]].
func eigMax2(a, b, c float64) float64 {
	l1, _ := linalg.SymEigen2(a, b, c)
	return l1
}

func geoOuter(a, b geo.Vec3) linalg.Mat3 {
	return linalg.Mat3{a.X * b.X, a.X * b.Y, a.X * b.Z, a.Y * b.X, a.Y * b.Y, a.Y * b.Z, a.Z * b.X, a.Z * b.Y, a.Z * b.Z}
}

// release removes a contradicted prior (the station is solved from the
// data with its height prior only); its receptions get a fresh start.
func (b *bundle) release(i int) {
	st := &b.st[i]
	st.released = true
	st.flaggedPrior = true
	st.hOn = false
	b.released++
	st.sig2 = st.sig0*st.sig0 + 15*15
	for ri := range b.recs {
		if r := &b.recs[ri]; int(r.st) == i && r.blk >= 0 {
			r.w = 1
		}
	}
}

// snoop is the stage-1 test: every contradicted declared position and the
// worst contradicted anchor are released, then the bundle is solved again,
// until nothing is contradicted (or MaxReleases).
func (b *bundle) snoop(bool) error {
	for b.released < b.cfg.MaxReleases {
		S, err := b.covariance()
		if err != nil {
			return err
		}
		b.priorTest(S)
		worstA, worst := -1, 1.0
		n := 0
		for i := range b.st {
			st := &b.st[i]
			if !st.tested || st.released || st.distH <= st.limit {
				continue
			}
			switch st.in.Prior {
			case PriorDeclared:
				if b.released < b.cfg.MaxReleases {
					b.release(i)
					n++
				}
			case PriorAnchor:
				if r := st.distH / st.limit; r > worst {
					worstA, worst = i, r
				}
			}
		}
		if worstA >= 0 && b.released < b.cfg.MaxReleases {
			b.release(worstA)
			n++
		}
		if n == 0 {
			return nil
		}
		b.setPriors(1)
		if err := b.solve(2); err != nil {
			return err
		}
	}
	return nil
}

// snoopOne is the stage-2 test: the single worst contradicted prior is
// released. It reports whether one was.
func (b *bundle) snoopOne() (bool, error) {
	S, err := b.covariance()
	if err != nil {
		return false, err
	}
	b.priorTest(S)
	wi, worst := -1, 1.0
	for i := range b.st {
		st := &b.st[i]
		if !st.tested || st.released || st.distH <= st.limit {
			continue
		}
		if r := st.distH / st.limit; r > worst {
			wi, worst = i, r
		}
	}
	if wi < 0 {
		return false, nil
	}
	b.release(wi)
	return true, nil
}

// finish computes the final covariance and the prior tests of the stations
// whose priors are kept.
func (b *bundle) finish() error {
	S, err := b.covariance()
	if err != nil {
		return err
	}
	b.priorTest(S)
	return nil
}
