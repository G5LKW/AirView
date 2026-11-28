package ssr

// Interrogator locating from beam sweeps (ROADMAP 1.1 "Interrogator radar
// locating"). A rotating Mode S radar elicits DF11 all-call replies, which
// carry its interrogator code (II 1-15 or SI 1-63), only while its beam
// points at the aircraft. The times of those reply bursts therefore follow
// the beam: the azimuth of the replying aircraft seen from the radar is
// φ + ω·t. With the aircraft positions known from ADS-B, the site (x, y),
// the rotation rate ω and the phase φ are the unknowns of a bearing-only
// fit, solved here without any 1030 MHz receiver and without knowing the
// station position. This complements LocateInterrogators (which needs
// common-interrogation pairs, i.e. Mode A/C or unlocked all-calls): it only
// needs DF11 replies with a non-zero interrogator code, one burst per
// aircraft and scan.
//
// Method (own derivation; textbook bearing-only geometry):
//  1. Rotation period from the repeat intervals of each aircraft's bursts:
//     successive scans are k·P apart, so the periodogram of the intervals
//     peaks at f = 1/P and its harmonics; the lowest strong peak is P.
//  2. Site by grid search on a gnomonic plane (great circles are straight
//     lines, angles at the site are distorted by < 0.1° at 500 km): the
//     score is the windowed phase coherence of e^{j(az_i − ω t_i)}, for
//     both senses of rotation, coarse to fine.
//  3. Robust Gauss-Newton on (x, y, ω, one phase per window) with Huber
//     weights; the formal covariance gives the error ellipse.
//
// Limits (stated in the API and the manual): lockout means most aircraft
// answer a radar's all-calls only until it has acquired them, so busy
// lockout environments give few repeat bursts and no period; the error
// ellipse is formal (it does not contain beam squint, burst truncation by
// lockout or position latency); one code may be shared by several radars,
// so up to three sites are fitted per code.

import (
	"math"
	"math/cmplx"
	"sort"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

// SweepObs is one DF11 all-call burst ("dwell") of one aircraft for one
// interrogator code.
type SweepObs struct {
	T    float64  // mean local time of the burst's replies, s
	ICAO uint32   // replying aircraft
	IID  uint8    // interrogator code: II 1-15, SI 1-63 as 16+SI
	P    geo.Vec3 // aircraft ECEF position at T
	N    int      // replies in the burst
}

// CodeName returns "II 5" / "SI 12" for an interrogator code.
func CodeName(iid uint8) string {
	if iid >= 16 {
		return "SI " + itoa(int(iid)-16)
	}
	return "II " + itoa(int(iid))
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for v > 0 && i > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// SweepRadar is one interrogator located from its beam sweeps.
type SweepRadar struct {
	IID  uint8
	Code string
	// OK: every quality gate passed. When false, Reason says which one
	// failed; LLH is still the best estimate when NUsed > 0.
	OK     bool
	Reason string
	LLH    geo.LLH // HAE 0 (height is not observable)
	// SemiMajorM / SemiMinorM: formal 1σ error ellipse, OrientDeg the
	// azimuth of its major axis. SigmaHM = sqrt(major² + minor²).
	SemiMajorM, SemiMinorM, OrientDeg, SigmaHM float64
	// PeriodS is the antenna rotation period, negative for an
	// anticlockwise antenna; PeriodSigmaS its formal 1σ.
	PeriodS, PeriodSigmaS float64
	// Coherence (0..1) is how well the burst times follow one rotating
	// beam; RMSDeg the robust residual scatter of the burst azimuths.
	Coherence, RMSDeg      float64
	NObs, NUsed, NAircraft int
	// SpreadDeg: azimuth spread of the used bursts seen from the site.
	SpreadDeg float64
	// SpanS: time covered by the used bursts.
	SpanS float64
	// RangeMinM / RangeMaxM: nearest and farthest used aircraft.
	RangeMinM, RangeMaxM float64
	// Ref / RefDistM: the nearest configured site (Config.Sites, e.g.
	// copied from a published radar list) and its distance; RefDistM < 0
	// when no site is configured.
	Ref      string
	RefDistM float64
}

// Quality gates and bounds of the sweep locator.
const (
	sweepKeepS        = 3600  // observations older than this are dropped
	sweepMaxObs       = 30000 // memory bound (~1.7 MB)
	sweepGridMaxObs   = 900   // observations used by the fine grid search
	sweepCoarseMaxObs = 300   // observations used by the coarse grid
	sweepMaxIntervals = 8000
	sweepMaxGapS      = 75 // repeat intervals considered for the period
	sweepMinObs       = 40
	sweepMinAircraft  = 6
	sweepMinIntervals = 15
	sweepMinSpreadDeg = 20
	sweepMinCoherence = 0.5
	sweepMaxSigmaM    = 15e3
	sweepMaxPerCode   = 3
	sweepCoarseCellM  = 8e3
)

// sweepPt is one observation on the gnomonic plane.
type sweepPt struct {
	x, y, t float64
	icao    uint32
}

// LocateBySweep locates the interrogators of the observations, per code.
// The search square is Config.RadarRegionM about the hint position (or the
// centroid of the aircraft positions). Results are sorted by code.
func LocateBySweep(obs []SweepObs, h Hint, cfg Config) []SweepRadar {
	cfg.Normalize()
	if len(obs) == 0 {
		return nil
	}
	centre := h.Pos
	if !h.Valid {
		var s geo.Vec3
		for i := range obs {
			s = s.Add(obs[i].P)
		}
		centre = geo.FromECEF(s.Scale(1 / float64(len(obs))))
	}
	g := NewGnomonic(centre)
	by := map[uint8][]sweepPt{}
	for i := range obs {
		o := &obs[i]
		if o.IID == 0 {
			continue
		}
		x, y, ok := g.Fwd(o.P)
		if !ok {
			continue
		}
		by[o.IID] = append(by[o.IID], sweepPt{x: x, y: y, t: o.T, icao: o.ICAO})
	}
	codes := make([]int, 0, len(by))
	for c := range by {
		codes = append(codes, int(c))
	}
	sort.Ints(codes)
	refs := SitesFromConfig(cfg)
	var out []SweepRadar
	for _, c := range codes {
		pts := by[uint8(c)]
		sort.Slice(pts, func(i, j int) bool { return pts[i].t < pts[j].t })
		for k := 0; k < sweepMaxPerCode; k++ {
			r, inl := locateOne(pts, &g, cfg)
			r.IID, r.Code = uint8(c), CodeName(uint8(c))
			r.RefDistM = -1
			if r.NUsed > 0 {
				p := r.LLH.ECEF()
				for i := range refs {
					d := horizDist(p, refs[i].ECEF)
					if r.RefDistM < 0 || d < r.RefDistM {
						r.RefDistM, r.Ref = d, refs[i].Name
					}
				}
			}
			if k == 0 || r.OK {
				out = append(out, r)
			}
			if !r.OK || len(inl) == 0 {
				break
			}
			// Another radar may share the code: fit the rest.
			drop := map[int]bool{}
			for _, i := range inl {
				drop[i] = true
			}
			rest := pts[:0:0]
			for i := range pts {
				if !drop[i] {
					rest = append(rest, pts[i])
				}
			}
			if len(rest) < sweepMinObs {
				break
			}
			pts = rest
		}
	}
	return out
}

func horizDist(a, b geo.Vec3) float64 {
	f := geo.NewFrameECEF(a)
	d := f.ToENU(b)
	return math.Hypot(d.X, d.Y)
}

// sweepPeriod estimates |P| from the repeat intervals of each aircraft's
// bursts. It returns the period, the periodogram score at it (0..1) and
// the number of intervals.
func sweepPeriod(pts []sweepPt, minP, maxP float64) (float64, float64, int) {
	last := map[uint32][]float64{}
	var ds []float64
	for i := range pts {
		p := &pts[i]
		prev := last[p.icao]
		k := 0
		for _, t := range prev {
			if d := p.t - t; d <= sweepMaxGapS {
				if d > 0.6*minP {
					ds = append(ds, d)
				}
				prev[k] = t
				k++
			}
		}
		last[p.icao] = append(prev[:k], p.t)
	}
	n := len(ds)
	if n < sweepMinIntervals {
		return 0, 0, n
	}
	if n > sweepMaxIntervals {
		st := float64(n) / sweepMaxIntervals
		k := 0
		for f := 0.0; int(f) < n && k < sweepMaxIntervals; f += st {
			ds[k] = ds[int(f)]
			k++
		}
		ds = ds[:k]
	}
	fmin, fmax := 1/maxP, 1/minP
	const df = 0.0004 // Hz: < 0.15 rad of phase over sweepMaxGapS / 2
	nf := int((fmax-fmin)/df) + 1
	score := make([]float64, nf)
	for _, d := range ds {
		z := cmplx.Exp(complex(0, 2*math.Pi*d*fmin))
		st := cmplx.Exp(complex(0, 2*math.Pi*d*df))
		for k := 0; k < nf; k++ {
			score[k] += real(z)
			z *= st
		}
	}
	best := 0.0
	for k := range score {
		score[k] /= float64(len(ds))
		best = math.Max(best, score[k])
	}
	if best < 0.3 {
		return 0, best, n
	}
	// The lowest strong local maximum: higher harmonics (P/2, P/3) score
	// as well as the fundamental, the sub-harmonic 2P does not.
	kb := -1
	for k := range score {
		if score[k] >= 0.8*best && (k == 0 || score[k] >= score[k-1]) && (k == nf-1 || score[k] >= score[k+1]) {
			kb = k
			break
		}
	}
	if kb < 0 {
		return 0, best, n
	}
	// Refine on a fine grid about the peak.
	fc := fmin + float64(kb)*df
	f0, s0 := fc, score[kb]
	for f := fc - df; f <= fc+df; f += df / 40 {
		var s float64
		for _, d := range ds {
			s += math.Cos(2 * math.Pi * d * f)
		}
		s /= float64(len(ds))
		if s > s0 {
			f0, s0 = f, s
		}
	}
	return 1 / f0, s0, n
}

// sweepWin assigns observations to phase windows.
type sweepWin struct {
	idx []int     // window of each observation (-1: dropped)
	tc  []float64 // window centre time
	n   int
}

func newSweepWin(pts []sweepPt, windowS float64) sweepWin {
	w := sweepWin{idx: make([]int, len(pts))}
	if len(pts) == 0 {
		return w
	}
	t0 := pts[0].t
	cnt := map[int]int{}
	raw := make([]int, len(pts))
	for i := range pts {
		raw[i] = int(math.Floor((pts[i].t - t0) / windowS))
		cnt[raw[i]]++
	}
	ids := map[int]int{}
	for i := range pts {
		if cnt[raw[i]] < 2 {
			w.idx[i] = -1
			continue
		}
		id, ok := ids[raw[i]]
		if !ok {
			id = len(w.tc)
			ids[raw[i]] = id
			w.tc = append(w.tc, t0+(float64(raw[i])+0.5)*windowS)
		}
		w.idx[i] = id
	}
	w.n = len(w.tc)
	return w
}

// gridScore is the windowed coherence of the beam model at site (x, y):
// the unbiased mean of cos(ψ_i − ψ_j) over pairs in one window.
func gridScore(pts []sweepPt, use []int, rot []complex128, win *sweepWin, sum []complex128, cnt []float64, x, y float64) float64 {
	for k := range sum {
		sum[k], cnt[k] = 0, 0
	}
	for _, i := range use {
		w := win.idx[i]
		dx, dy := pts[i].x-x, pts[i].y-y
		r := math.Hypot(dx, dy)
		if r < 1000 {
			continue
		}
		sum[w] += complex(dy/r, dx/r) * rot[i]
		cnt[w]++
	}
	var num, den float64
	for k := range sum {
		n := cnt[k]
		if n < 2 {
			continue
		}
		s := sum[k]
		num += real(s)*real(s) + imag(s)*imag(s) - n
		den += n * (n - 1)
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// locateOne fits one interrogator to pts; it returns the result and the
// indices of its inliers.
func locateOne(pts []sweepPt, g *Gnomonic, cfg Config) (SweepRadar, []int) {
	r := SweepRadar{NObs: len(pts)}
	if len(pts) < sweepMinObs {
		r.Reason = "too few all-call bursts"
		return r, nil
	}
	P, _, nint := sweepPeriod(pts, cfg.MinPeriodS, cfg.MaxPeriodS)
	if P == 0 {
		if nint < sweepMinIntervals {
			r.Reason = "too few repeat bursts to find the rotation period (lockout?)"
		} else {
			r.Reason = "no rotation period found"
		}
		return r, nil
	}
	win := newSweepWin(pts, cfg.RotWindowS)
	if win.n == 0 {
		r.Reason = "too few bursts per time window"
		return r, nil
	}
	var use []int
	for i := range pts {
		if win.idx[i] >= 0 {
			use = append(use, i)
		}
	}
	if len(use) > sweepGridMaxObs {
		st := float64(len(use)) / sweepGridMaxObs
		k := 0
		for f := 0.0; int(f) < len(use) && k < sweepGridMaxObs; f += st {
			use[k] = use[int(f)]
			k++
		}
		use = use[:k]
	}
	coarse := use
	if len(coarse) > sweepCoarseMaxObs {
		coarse = make([]int, 0, sweepCoarseMaxObs)
		st := float64(len(use)) / sweepCoarseMaxObs
		for f := 0.0; int(f) < len(use) && len(coarse) < sweepCoarseMaxObs; f += st {
			coarse = append(coarse, use[int(f)])
		}
	}
	sum := make([]complex128, win.n)
	cnt := make([]float64, win.n)
	rot := make([]complex128, len(pts))
	type cand struct{ x, y, s, sign float64 }
	var best cand
	half := cfg.RadarRegionM
	for _, sign := range []float64{1, -1} {
		om := sign * 2 * math.Pi / P
		for i := range pts {
			if w := win.idx[i]; w >= 0 {
				rot[i] = cmplx.Exp(complex(0, -om*(pts[i].t-win.tc[w])))
			}
		}
		// Coarse grid, then the best few peaks refined coarse to fine.
		nc := int(2*half/sweepCoarseCellM) + 1
		sc := make([]float64, nc*nc)
		for iy := 0; iy < nc; iy++ {
			for ix := 0; ix < nc; ix++ {
				sc[iy*nc+ix] = gridScore(pts, coarse, rot, &win, sum, cnt, -half+float64(ix)*sweepCoarseCellM, -half+float64(iy)*sweepCoarseCellM)
			}
		}
		var peaks []cand
		for iy := 0; iy < nc; iy++ {
			for ix := 0; ix < nc; ix++ {
				v := sc[iy*nc+ix]
				if v <= 0 {
					continue
				}
				isMax := true
				for dy := -1; dy <= 1 && isMax; dy++ {
					for dx := -1; dx <= 1; dx++ {
						jx, jy := ix+dx, iy+dy
						if (dx != 0 || dy != 0) && jx >= 0 && jy >= 0 && jx < nc && jy < nc && sc[jy*nc+jx] > v {
							isMax = false
							break
						}
					}
				}
				if isMax {
					peaks = append(peaks, cand{-half + float64(ix)*sweepCoarseCellM, -half + float64(iy)*sweepCoarseCellM, v, sign})
				}
			}
		}
		sort.Slice(peaks, func(i, j int) bool { return peaks[i].s > peaks[j].s })
		if len(peaks) > 4 {
			peaks = peaks[:4]
		}
		for _, p := range peaks {
			cell := sweepCoarseCellM
			for lvl := 0; lvl < 4; lvl++ {
				cell /= 4
				bx, by, bs := p.x, p.y, p.s
				for iy := -6; iy <= 6; iy++ {
					for ix := -6; ix <= 6; ix++ {
						x, y := p.x+float64(ix)*cell, p.y+float64(iy)*cell
						if s := gridScore(pts, use, rot, &win, sum, cnt, x, y); s > bs {
							bx, by, bs = x, y, s
						}
					}
				}
				p.x, p.y, p.s = bx, by, bs
			}
			if p.s > best.s {
				best = p
			}
		}
	}
	if best.s <= 0 {
		r.Reason = "no consistent rotating beam"
		return r, nil
	}
	return refineSweep(pts, &win, g, best.x, best.y, best.sign*2*math.Pi/P, r, half)
}

func wrapPi(a float64) float64 {
	a = math.Mod(a+math.Pi, 2*math.Pi)
	if a < 0 {
		a += 2 * math.Pi
	}
	return a - math.Pi
}

// refineSweep is the robust Gauss-Newton fit of (x, y, ω, φ_w).
func refineSweep(pts []sweepPt, win *sweepWin, g *Gnomonic, x, y, om float64, r SweepRadar, half float64) (SweepRadar, []int) {
	W := win.n
	np := 3 + W
	phi := make([]float64, W)
	setPhases := func() {
		s := make([]complex128, W)
		for i := range pts {
			w := win.idx[i]
			if w < 0 {
				continue
			}
			dx, dy := pts[i].x-x, pts[i].y-y
			az := math.Atan2(dx, dy)
			s[w] += cmplx.Exp(complex(0, az-om*(pts[i].t-win.tc[w])))
		}
		for k := range phi {
			phi[k] = cmplx.Phase(s[k])
		}
	}
	setPhases()
	res := make([]float64, len(pts))
	wt := make([]float64, len(pts))
	H := make([]float64, np*np)
	gv := make([]float64, np)
	jr := make([]float64, np)
	sigma := 2 * math.Pi / 180
	resid := func() {
		for i := range pts {
			w := win.idx[i]
			if w < 0 {
				res[i] = math.NaN()
				continue
			}
			dx, dy := pts[i].x-x, pts[i].y-y
			res[i] = wrapPi(math.Atan2(dx, dy) - phi[w] - om*(pts[i].t-win.tc[w]))
		}
	}
	robustSigma := func() float64 {
		var a []float64
		for _, v := range res {
			if !math.IsNaN(v) {
				a = append(a, math.Abs(v))
			}
		}
		if len(a) == 0 {
			return sigma
		}
		sort.Float64s(a)
		return math.Max(1.4826*a[len(a)/2], 0.05*math.Pi/180)
	}
	const huber = 2.5
	converged := false
	for it := 0; it < 30; it++ {
		resid()
		sigma = robustSigma()
		for k := range H {
			H[k] = 0
		}
		for k := range gv {
			gv[k] = 0
		}
		for i := range pts {
			w := win.idx[i]
			if w < 0 {
				wt[i] = 0
				continue
			}
			a := math.Abs(res[i]) / sigma
			switch {
			case a > 6:
				wt[i] = 0
				continue
			case a > huber:
				wt[i] = huber / a
			default:
				wt[i] = 1
			}
			dx, dy := pts[i].x-x, pts[i].y-y
			r2 := dx*dx + dy*dy
			if r2 < 1e6 {
				wt[i] = 0
				continue
			}
			for k := range jr {
				jr[k] = 0
			}
			jr[0], jr[1] = -dy/r2, dx/r2 // ∂az/∂x, ∂az/∂y
			jr[2] = -(pts[i].t - win.tc[w])
			jr[3+w] = -1
			nz := [4]int{0, 1, 2, 3 + w}
			for _, a := range nz {
				gv[a] += wt[i] * jr[a] * res[i]
				for _, b := range nz {
					H[a*np+b] += wt[i] * jr[a] * jr[b]
				}
			}
		}
		L := append([]float64(nil), H...)
		for k := 0; k < np; k++ {
			L[k*np+k] += 1e-12 + 1e-9*H[k*np+k]
		}
		if linalg.Cholesky(L, np) != nil {
			break
		}
		// Solve H·δ = −J'W r for the residual r = model error.
		step := append([]float64(nil), gv...)
		linalg.CholSolve(L, np, step)
		// res = az − model; ∂res/∂p = jr, so δ = −(H⁻¹ g).
		x -= step[0]
		y -= step[1]
		om -= step[2]
		for k := range phi {
			phi[k] -= step[3+k]
		}
		if math.Hypot(step[0], step[1]) < 1 && math.Abs(step[2]) < 1e-9 {
			converged = true
			break
		}
	}
	resid()
	sigma = robustSigma()
	// Inliers, statistics and covariance.
	var inl []int
	var chi, nw float64
	seen := map[uint32]bool{}
	var azs []float64
	tmin, tmax := math.Inf(1), math.Inf(-1)
	rmin, rmax := math.Inf(1), 0.0
	csum := make([]complex128, W)
	ccnt := make([]float64, W)
	for i := range pts {
		if math.IsNaN(res[i]) || math.Abs(res[i]) > math.Max(3*sigma, math.Pi/180) {
			continue
		}
		inl = append(inl, i)
		w := win.idx[i]
		chi += res[i] * res[i]
		nw++
		seen[pts[i].icao] = true
		dx, dy := pts[i].x-x, pts[i].y-y
		azs = append(azs, math.Atan2(dx, dy)+math.Pi)
		tmin, tmax = math.Min(tmin, pts[i].t), math.Max(tmax, pts[i].t)
		d := math.Hypot(dx, dy)
		rmin, rmax = math.Min(rmin, d), math.Max(rmax, d)
		csum[w] += cmplx.Exp(complex(0, res[i]))
		ccnt[w]++
	}
	r.NUsed, r.NAircraft = len(inl), len(seen)
	r.LLH = g.Inv(x, y, 0)
	r.PeriodS = 2 * math.Pi / om
	r.RMSDeg = sigma * 180 / math.Pi
	if len(inl) > 0 {
		r.SpanS = tmax - tmin
		r.RangeMinM, r.RangeMaxM = rmin, rmax
		r.SpreadDeg = azSpreadDeg(azs)
	}
	var num, den float64
	for k := range csum {
		if n := ccnt[k]; n >= 2 {
			s := csum[k]
			num += real(s)*real(s) + imag(s)*imag(s) - n
			den += n * (n - 1)
		}
	}
	if den > 0 && num > 0 {
		r.Coherence = math.Sqrt(num / den)
	}
	// Formal covariance from the inliers only.
	for k := range H {
		H[k] = 0
	}
	for _, i := range inl {
		w := win.idx[i]
		dx, dy := pts[i].x-x, pts[i].y-y
		r2 := dx*dx + dy*dy
		if r2 < 1e6 {
			continue
		}
		jr0, jr1, jr2 := -dy/r2, dx/r2, -(pts[i].t - win.tc[w])
		idx := [4]int{0, 1, 2, 3 + w}
		v := [4]float64{jr0, jr1, jr2, -1}
		for a := 0; a < 4; a++ {
			for b := 0; b < 4; b++ {
				H[idx[a]*np+idx[b]] += v[a] * v[b]
			}
		}
	}
	dof := nw - float64(np)
	s2 := sigma * sigma
	if dof > 0 {
		s2 = math.Max(s2, chi/dof)
	}
	cov := append([]float64(nil), H...)
	for k := 0; k < np; k++ {
		cov[k*np+k] += 1e-12
	}
	okCov := linalg.Cholesky(cov, np) == nil && linalg.CholToInverse(cov, np, nil) == nil
	if okCov {
		cxx, cxy, cyy := cov[0]*s2, cov[1]*s2, cov[np+1]*s2
		tr, det := cxx+cyy, cxx*cyy-cxy*cxy
		disc := math.Sqrt(math.Max(tr*tr/4-det, 0))
		l1, l2 := tr/2+disc, math.Max(tr/2-disc, 0)
		r.SemiMajorM, r.SemiMinorM = math.Sqrt(l1), math.Sqrt(l2)
		r.SigmaHM = math.Sqrt(l1 + l2)
		// Major axis direction (x = east, y = north) as an azimuth.
		ang := 0.5 * math.Atan2(2*cxy, cxx-cyy) // from the x (east) axis
		r.OrientDeg = math.Mod(90-ang*180/math.Pi+360, 180)
		som := math.Sqrt(cov[2*np+2] * s2)
		r.PeriodSigmaS = 2 * math.Pi / (om * om) * som
	}
	switch {
	case !converged:
		r.Reason = "fit did not converge"
	case !okCov:
		r.Reason = "geometry too weak (singular fit)"
	case r.NUsed < sweepMinObs:
		r.Reason = "too few consistent bursts"
	case r.NAircraft < sweepMinAircraft:
		r.Reason = "too few aircraft"
	case r.SpreadDeg < sweepMinSpreadDeg:
		r.Reason = "aircraft cover too narrow an azimuth sector"
	case r.Coherence < sweepMinCoherence:
		r.Reason = "burst times do not follow one rotating beam well"
	case math.Abs(x) > 0.97*half || math.Abs(y) > 0.97*half:
		r.Reason = "at the edge of the search region"
	case r.SemiMajorM > sweepMaxSigmaM:
		r.Reason = "error ellipse too large"
	default:
		r.OK = true
	}
	return r, inl
}
