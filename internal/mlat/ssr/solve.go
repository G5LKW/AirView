package ssr

import (
	"fmt"
	"math"
	"sort"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

// Hint is optional prior knowledge of the station position.
type Hint struct {
	// Valid: Pos (lat/lon) is a search centre; RadiusM the seed grid's
	// radius (0 = 50 km) and the 1σ of a weak horizontal prior.
	Valid   bool
	Pos     geo.LLH
	RadiusM float64
	// HeightKnown: HeightM (HAE, m) is a prior with 1σ HeightSigmaM
	// (0 = 20 m). Otherwise the prior is 100 m ± Config.HeightSigmaM.
	HeightKnown  bool
	HeightM      float64
	HeightSigmaM float64
}

// Result is an experimental bistatic SSR position estimate. It is advisory
// only: no AirView component uses it to locate a station automatically.
type Result struct {
	OK           bool
	Reason       string // why OK is false
	Experimental bool   // always true
	LLH          geo.LLH
	ECEF         geo.Vec3
	// CovENU is the position covariance (east, north, up), m², scaled by
	// the a-posteriori variance factor when it exceeds 1.
	CovENU     [9]float64
	SigmaHM    float64
	SigmaUM    float64
	SemiMajorM float64 // 1σ error ellipse
	SemiMinorM float64
	NObs       int // observations offered
	NUsed      int // observations in the final fit
	NAircraft  int // airframe/class biases solved
	NRadars    int // interrogators with observations
	// RMSNs is the RMS of the final residuals (ns); VarFactor the
	// a-posteriori variance factor χ²/dof.
	RMSNs      float64
	VarFactor  float64
	Iterations int
	Converged  bool
	Seed       geo.LLH
	Radars     []Interrogator // refined interrogator positions
}

type sobs struct {
	o      *PairObs
	y      float64 // c·Delta, m
	ik, ij int     // bias parameter index (absolute)
	lk, lj int     // latency parameter index (-1 none)
	r, alt int     // radar index, alternative
	st2    float64 // per-interrogation timing variance of the difference, m²
	w      float64 // Huber weight
	out    bool
}

func pseudoHuber(r, s float64) float64 {
	q := r / s
	return s * s * (math.Sqrt(1+q*q) - 1)
}

// SolveObs estimates the station position from pair observations (see the
// package documentation for the model). The observations are not
// modified.
func SolveObs(in []PairObs, h Hint, cfg Config, atm geo.Atmosphere) Result {
	cfg.Normalize()
	atm = atm.Normalize()
	res := Result{Experimental: true, NObs: len(in), VarFactor: math.NaN(), RMSNs: math.NaN()}
	if len(in) < cfg.MinObs {
		res.Reason = fmt.Sprintf("only %d observations (min %d)", len(in), cfg.MinObs)
		return res
	}
	obs := append([]PairObs(nil), in...)

	var radars []Interrogator
	if len(cfg.Sites) > 0 {
		radars = SitesFromConfig(cfg)
	} else {
		radars, _ = LocateInterrogators(obs, cfg)
	}
	if len(radars) == 0 {
		res.Reason = "no interrogator located"
		return res
	}
	alt := assignWithRotation(obs, &radars, cfg)
	// Interrogators need MinRadarObs observations.
	cnt := make([]int, len(radars))
	for i := range obs {
		if obs[i].Radar >= 0 {
			cnt[obs[i].Radar]++
		}
	}
	keepR := make([]int, len(radars))
	var rs []Interrogator
	for k := range radars {
		keepR[k] = -1
		if cnt[k] >= cfg.MinRadarObs {
			keepR[k] = len(rs)
			radars[k].Obs = cnt[k]
			rs = append(rs, radars[k])
		}
	}
	radars = rs
	if len(radars) == 0 {
		res.Reason = "no interrogator with enough observations"
		return res
	}

	// Airframe biases: the MaxAircraft most observed keys.
	kc := map[AircraftKey]int{}
	for i := range obs {
		if obs[i].Radar >= 0 && keepR[obs[i].Radar] >= 0 {
			kc[obs[i].K]++
			kc[obs[i].J]++
		}
	}
	keys := make([]AircraftKey, 0, len(kc))
	for k := range kc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if kc[keys[i]] != kc[keys[j]] {
			return kc[keys[i]] > kc[keys[j]]
		}
		if keys[i].ICAO != keys[j].ICAO {
			return keys[i].ICAO < keys[j].ICAO
		}
		return keys[i].Class < keys[j].Class
	})
	if len(keys) > cfg.MaxAircraft {
		keys = keys[:cfg.MaxAircraft]
	}
	nR := len(radars)
	base := 3 + 3*nR
	kidx := make(map[AircraftKey]int, len(keys))
	for i, k := range keys {
		kidx[k] = base + i
	}
	// Position latency per airframe (all classes share it).
	lidx := map[uint32]int{}
	lbase := base + len(keys)
	if !cfg.NoLatency {
		for _, k := range keys {
			if _, ok := lidx[k.ICAO]; !ok {
				lidx[k.ICAO] = lbase + len(lidx)
			}
		}
	}
	P := lbase + len(lidx)

	cm := geo.C * 1e-9
	var so []sobs
	for i := range obs {
		o := &obs[i]
		if o.Radar < 0 || keepR[o.Radar] < 0 {
			continue
		}
		ik, ok1 := kidx[o.K]
		ij, ok2 := kidx[o.J]
		if !ok1 || !ok2 || o.N <= 0 {
			continue
		}
		st := cfg.TimingSigmaACNs * cm
		if o.K.Class == ClassModeS {
			st = cfg.TimingSigmaSNs * cm
		}
		if o.N >= 3 && o.Spread*geo.C > 4*math.Sqrt2*st {
			continue // the matched replies scatter too much: a bad match
		}
		a := -1
		if alt[i] >= 0 {
			a = keepR[alt[i]]
		}
		lk, lj := -1, -1
		if v, ok := lidx[o.K.ICAO]; ok && o.VK != (geo.Vec3{}) {
			lk = v
		}
		if v, ok := lidx[o.J.ICAO]; ok && o.VJ != (geo.Vec3{}) {
			lj = v
		}
		so = append(so, sobs{o: o, y: geo.C * o.Delta, ik: ik, ij: ij, lk: lk, lj: lj, r: keepR[o.Radar], alt: a, st2: 2 * st * st, w: 1})
	}
	if len(so) < cfg.MinObs {
		res.Reason = fmt.Sprintf("only %d usable observations (min %d)", len(so), cfg.MinObs)
		return res
	}

	// Height prior.
	h0, hs := 100.0, cfg.HeightSigmaM
	if h.HeightKnown {
		h0, hs = h.HeightM, h.HeightSigmaM
		if hs <= 0 {
			hs = 20
		}
	}

	// Seed.
	var centre geo.LLH
	radius := cfg.SearchRadiusM
	if h.Valid {
		centre = h.Pos
		radius = h.RadiusM
		if radius <= 0 {
			radius = 50e3
		}
	} else {
		var c geo.Vec3
		for _, s := range so {
			c = c.Add(s.o.PK).Add(s.o.PJ)
		}
		centre = geo.FromECEF(c.Scale(1 / float64(2*len(so))))
	}
	u := seedGrid(so, radars, centre, radius, h0, cfg.SeedMaxObs)
	res.Seed = geo.FromECEF(u)

	// Parameters.
	x := make([]float64, P)
	x[0], x[1], x[2] = u.X, u.Y, u.Z
	r0 := make([]geo.Vec3, nR)
	rinfo := make([][9]float64, nR)
	for k := range radars {
		r0[k] = radars[k].ECEF
		x[3+3*k], x[4+3*k], x[5+3*k] = r0[k].X, r0[k].Y, r0[k].Z
		rinfo[k] = radarInfo(&radars[k])
	}
	bInfoAC := 1 / sq(cfg.BiasSigmaACNs*cm)
	bInfoS := 1 / sq(cfg.BiasSigmaSNs*cm)
	lInfo := 1 / sq(cfg.LatencySigmaS)
	var hInfoH float64
	var hCentre geo.Vec3
	if h.Valid {
		hInfoH = 1 / sq(radius)
		hCentre = geo.ToECEF(h.Pos.Lat, h.Pos.Lon, h0)
	}

	H := make([]float64, P*P)
	g := make([]float64, P)
	vec := func(i int) geo.Vec3 { return geo.Vec3{X: x[i], Y: x[i+1], Z: x[i+2]} }

	model := func(s *sobs, rIdx int) (f float64, row jrow, sig2 float64) {
		U := vec(0)
		R := vec(3 + 3*rIdx)
		pk, pj := s.o.PK, s.o.PJ
		if s.lk >= 0 {
			pk = pk.Sub(s.o.VK.Scale(x[s.lk]))
		}
		if s.lj >= 0 {
			pj = pj.Sub(s.o.VJ.Scale(x[s.lj]))
		}
		f = atm.Range(R, pj) - atm.Range(R, pk) + atm.Range(U, pj) - atm.Range(U, pk) + x[s.ij] - x[s.ik]
		ujU, ukU := pj.Sub(U).Unit(), pk.Sub(U).Unit()
		ujR, ukR := pj.Sub(R).Unit(), pk.Sub(R).Unit()
		ju := ukU.Sub(ujU)
		jr := ukR.Sub(ujR)
		gk, gj := ukR.Add(ukU), ujR.Add(ujU)
		row.add(0, ju.X)
		row.add(1, ju.Y)
		row.add(2, ju.Z)
		row.add(3+3*rIdx, jr.X)
		row.add(4+3*rIdx, jr.Y)
		row.add(5+3*rIdx, jr.Z)
		row.add(s.ij, 1)
		row.add(s.ik, -1)
		if s.lk >= 0 {
			row.add(s.lk, gk.Dot(s.o.VK))
		}
		if s.lj >= 0 {
			row.add(s.lj, -gj.Dot(s.o.VJ))
		}
		sig2 = s.st2/float64(s.o.N) + sq(gk.Norm()*s.o.SK) + sq(gj.Norm()*s.o.SJ) + sq(cfg.FloorSigmaM)
		return
	}

	var used int
	var chi float64
	converged := false
	iters := 0
	solveOnce := func() error {
		converged = false
		for it := 0; it < cfg.MaxIter; it++ {
			iters++
			linalg.Zero(H)
			linalg.Zero(g)
			used, chi = 0, 0
			for i := range so {
				s := &so[i]
				if s.out {
					continue
				}
				if s.alt >= 0 {
					f1, _, _ := model(s, s.r)
					f2, _, _ := model(s, s.alt)
					if math.Abs(s.y-f2) < math.Abs(s.y-f1) {
						s.r, s.alt = s.alt, s.r
					}
				}
				f, row, sig2 := model(s, s.r)
				res := s.y - f
				z := math.Abs(res) / math.Sqrt(sig2)
				w := 1.0
				if z > cfg.HuberK {
					w = cfg.HuberK / z
				}
				s.w = w
				w /= sig2
				chi += res * res / sig2
				used++
				row.accumulate(H, g, P, w, res)
			}
			// Height prior.
			U := vec(0)
			ll := geo.FromECEF(U)
			up := geo.Up(ll.Lat, ll.Lon)
			addOuter(H, P, 0, up, 1/(hs*hs))
			addVec(g, 0, up.Scale((h0-ll.H)/(hs*hs)))
			if hInfoH > 0 {
				// Weak isotropic horizontal prior about the hint.
				addHorizPrior(H, P, up, hInfoH)
				d := hCentre.Sub(U)
				addVec(g, 0, d.Sub(up.Scale(d.Dot(up))).Scale(hInfoH))
			}
			for k := 0; k < nR; k++ {
				o := 3 + 3*k
				d := r0[k].Sub(vec(o))
				for a := 0; a < 3; a++ {
					for b := 0; b < 3; b++ {
						H[(o+a)*P+o+b] += rinfo[k][a*3+b]
					}
					g[o+a] += rinfo[k][a*3]*d.X + rinfo[k][a*3+1]*d.Y + rinfo[k][a*3+2]*d.Z
				}
			}
			for i, key := range keys {
				p := base + i
				info := bInfoAC
				if key.Class == ClassModeS {
					info = bInfoS
				}
				H[p*P+p] += info
				g[p] -= info * x[p]
			}
			for p := lbase; p < P; p++ {
				H[p*P+p] += lInfo
				g[p] -= lInfo * x[p]
			}
			if used < cfg.MinObs {
				return fmt.Errorf("only %d observations left (min %d)", used, cfg.MinObs)
			}
			L := append([]float64(nil), H...)
			if _, err := linalg.CholeskyJitter(L, H, P, 1e-9, 6); err != nil {
				return fmt.Errorf("normal equations singular: %v", err)
			}
			linalg.CholSolve(L, P, g)
			step := 0.0
			for i := range x {
				x[i] += g[i]
			}
			step = math.Sqrt(g[0]*g[0] + g[1]*g[1] + g[2]*g[2])
			rs := 0.0
			for k := 0; k < nR; k++ {
				o := 3 + 3*k
				rs = math.Max(rs, math.Sqrt(g[o]*g[o]+g[o+1]*g[o+1]+g[o+2]*g[o+2]))
			}
			if step > 1e6 {
				return fmt.Errorf("diverged")
			}
			if step < 0.01 && rs < 0.1 {
				converged = true
				return nil
			}
		}
		return nil
	}

	for round := 0; round < 4; round++ {
		if err := solveOnce(); err != nil {
			res.Reason = err.Error()
			res.Iterations = iters
			return res
		}
		dof := math.Max(1, float64(used-3-3*nR))
		f := math.Max(1, chi/dof)
		dropped := 0
		for i := range so {
			s := &so[i]
			if s.out {
				continue
			}
			fv, _, sig2 := model(s, s.r)
			if math.Abs(s.y-fv)/math.Sqrt(sig2*f) > cfg.OutlierZ {
				s.out = true
				dropped++
			}
		}
		if dropped == 0 {
			break
		}
	}

	// Covariance from the final normal matrix (rebuild at the solution).
	linalg.Zero(H)
	var sumR2 float64
	var nIn int
	chi, used = 0, 0
	aircraft := map[int]bool{}
	radarUsed := map[int]bool{}
	for i := range so {
		s := &so[i]
		if s.out {
			continue
		}
		f, row, sig2 := model(s, s.r)
		r := s.y - f
		chi += r * r / sig2
		sumR2 += r * r
		nIn++
		used++
		aircraft[s.ik], aircraft[s.ij] = true, true
		radarUsed[s.r] = true
		row.accumulate(H, nil, P, s.w/sig2, 0)
	}
	U := vec(0)
	ll := geo.FromECEF(U)
	up := geo.Up(ll.Lat, ll.Lon)
	addOuter(H, P, 0, up, 1/(hs*hs))
	if hInfoH > 0 {
		addHorizPrior(H, P, up, hInfoH)
	}
	for k := 0; k < nR; k++ {
		o := 3 + 3*k
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				H[(o+a)*P+o+b] += rinfo[k][a*3+b]
			}
		}
	}
	for i, key := range keys {
		p := base + i
		if key.Class == ClassModeS {
			H[p*P+p] += bInfoS
		} else {
			H[p*P+p] += bInfoAC
		}
	}
	for p := lbase; p < P; p++ {
		H[p*P+p] += lInfo
	}
	dof := math.Max(1, float64(used-3-3*nR))
	vf := chi / dof
	if err := linalg.SPDInverse(H, P, nil); err != nil {
		res.Reason = "covariance singular"
		return res
	}
	scale := math.Max(1, vf)
	var cu [9]float64
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			cu[a*3+b] = H[a*P+b] * scale
		}
	}
	fr := geo.NewFrame(ll)
	ce := fr.CovToENU(cu)
	res.LLH, res.ECEF, res.CovENU = ll, U, ce
	res.SigmaHM = math.Sqrt(math.Max(ce[0]+ce[4], 0))
	res.SigmaUM = math.Sqrt(math.Max(ce[8], 0))
	l1, l2 := linalg.SymEigen2(ce[0], ce[1], ce[4])
	res.SemiMajorM, res.SemiMinorM = math.Sqrt(math.Max(l1, 0)), math.Sqrt(math.Max(l2, 0))
	res.NUsed, res.NAircraft, res.NRadars = nIn, len(aircraft), len(radarUsed)
	res.VarFactor = vf
	if nIn > 0 {
		res.RMSNs = math.Sqrt(sumR2/float64(nIn)) / geo.C * 1e9
	}
	res.Iterations, res.Converged = iters, converged
	for k := range radars {
		o := 3 + 3*k
		rp := vec(o)
		rl := geo.FromECEF(rp)
		rf := geo.NewFrame(rl)
		var rc [9]float64
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				rc[a*3+b] = H[(o+a)*P+o+b] * scale
			}
		}
		rce := rf.CovToENU(rc)
		radars[k].LLH, radars[k].ECEF = rl, rp
		radars[k].CovEN = [3]float64{rce[0], rce[1], rce[4]}
		radars[k].SigmaUM = math.Sqrt(math.Max(rce[8], 0))
	}
	res.Radars = radars
	switch {
	case !converged:
		res.Reason = "not converged"
	case nIn < cfg.MinObs:
		res.Reason = fmt.Sprintf("only %d observations in the fit (min %d)", nIn, cfg.MinObs)
	case len(aircraft) < cfg.MinAircraft:
		res.Reason = fmt.Sprintf("only %d aircraft (min %d)", len(aircraft), cfg.MinAircraft)
	case !(res.SigmaHM <= cfg.MaxSigmaHM):
		res.Reason = fmt.Sprintf("sigma_h %.0f m > %.0f m", res.SigmaHM, cfg.MaxSigmaHM)
	default:
		res.OK = true
	}
	return res
}

// assignWithRotation assigns the observations geometrically, recovers
// each interrogator's rotation from its observations, drops learned
// interrogators without a coherent rotation (spurious line crossings) and
// assigns again gated by the beam phase. Configured sites without a
// coherent rotation are kept, ungated.
func assignWithRotation(obs []PairObs, radars *[]Interrogator, cfg Config) []int {
	alt := Assign(obs, *radars, cfg)
	for round := 0; round < 2; round++ {
		var keep []Interrogator
		for k := range *radars {
			r := (*radars)[k]
			r.Rotation = rotationFor(obs, k, &r, &cfg)
			if r.Configured || r.Rotation.Coherence >= cfg.MinCoherence {
				keep = append(keep, r)
			}
		}
		*radars = keep
		alt = Assign(obs, *radars, cfg)
	}
	// A learned "site" whose observations mostly also fit a much busier
	// site is a ghost on that site's bundle of bearing lines: drop it.
	n := len(*radars)
	cnt := make([]int, n)
	both := make([]int, n*n)
	for i := range obs {
		if r := obs[i].Radar; r >= 0 {
			cnt[r]++
			if a := alt[i]; a >= 0 {
				both[r*n+a]++
			}
		}
	}
	var keep []Interrogator
	for k := range *radars {
		ghost := false
		for j := 0; j < n; j++ {
			if j != k && !(*radars)[k].Configured && both[k*n+j]*10 >= cnt[k]*6 && cnt[j] >= 3*cnt[k] {
				ghost = true
			}
		}
		if !ghost {
			keep = append(keep, (*radars)[k])
		}
	}
	if len(keep) != n {
		*radars = keep
		alt = Assign(obs, *radars, cfg)
	}
	return alt
}

// jrow is one sparse Jacobian row.
type jrow struct {
	idx [10]int
	v   [10]float64
	n   int
}

func (r *jrow) add(i int, v float64) {
	r.idx[r.n], r.v[r.n] = i, v
	r.n++
}

// accumulate adds w·rowᵀ·row to H and w·rowᵀ·res to g (g may be nil).
func (r *jrow) accumulate(H, g []float64, P int, w, res float64) {
	for a := 0; a < r.n; a++ {
		ia := r.idx[a] * P
		wa := w * r.v[a]
		if g != nil {
			g[r.idx[a]] += wa * res
		}
		for b := 0; b < r.n; b++ {
			H[ia+r.idx[b]] += wa * r.v[b]
		}
	}
}

func sq(x float64) float64 { return x * x }

func comp(v geo.Vec3, i int) float64 {
	switch i {
	case 0:
		return v.X
	case 1:
		return v.Y
	}
	return v.Z
}

func addOuter(H []float64, P, o int, v geo.Vec3, s float64) {
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			H[(o+a)*P+o+b] += s * comp(v, a) * comp(v, b)
		}
	}
}

// addHorizPrior adds s·(I - up·upᵀ) to the position block.
func addHorizPrior(H []float64, P int, up geo.Vec3, s float64) {
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			v := -comp(up, a) * comp(up, b)
			if a == b {
				v++
			}
			H[a*P+b] += s * v
		}
	}
}

func addVec(g []float64, o int, v geo.Vec3) {
	g[o] += v.X
	g[o+1] += v.Y
	g[o+2] += v.Z
}

// radarInfo returns the ECEF information matrix of a site prior.
func radarInfo(r *Interrogator) [9]float64 {
	f := geo.NewFrame(r.LLH)
	ee, en, nn := r.CovEN[0], r.CovEN[1], r.CovEN[2]
	ee, nn = math.Max(ee, 1), math.Max(nn, 1)
	det := ee*nn - en*en
	if det <= 0 {
		en, det = 0, ee*nn
	}
	su := math.Max(r.SigmaUM, 1)
	ienu := [9]float64{nn / det, -en / det, 0, -en / det, ee / det, 0, 0, 0, 1 / (su * su)}
	return f.CovToECEF(ienu)
}

// seedGrid scores horizontal positions by a pseudo-Huber cost with straight
// line ranges and zero airframe biases, coarse to fine.
func seedGrid(so []sobs, radars []Interrogator, centre geo.LLH, radius, h0 float64, maxObs int) geo.Vec3 {
	stride := 1
	if len(so) > maxObs {
		stride = (len(so) + maxObs - 1) / maxObs
	}
	type sd struct {
		fixed  float64
		pk, pj geo.Vec3
	}
	var ss []sd
	for i := 0; i < len(so); i += stride {
		s := &so[i]
		R := radars[s.r].ECEF
		ss = append(ss, sd{s.y - (R.Dist(s.o.PJ) - R.Dist(s.o.PK)), s.o.PK, s.o.PJ})
	}
	centre.H = 0
	f := geo.NewFrame(centre)
	pt := func(e, n float64) geo.Vec3 {
		l := geo.FromECEF(f.FromENU(geo.Vec3{X: e, Y: n}))
		return geo.ToECEF(l.Lat, l.Lon, h0)
	}
	cost := func(u geo.Vec3, s float64) float64 {
		var c float64
		for i := range ss {
			c += pseudoHuber(ss[i].fixed-(u.Dist(ss[i].pj)-u.Dist(ss[i].pk)), s)
		}
		return c
	}
	type cand struct {
		e, n, c float64
	}
	const N = 20
	step := radius / N
	scale := math.Max(150, step)
	var cs []cand
	for i := -N; i <= N; i++ {
		for j := -N; j <= N; j++ {
			e, n := float64(i)*step, float64(j)*step
			if e*e+n*n > radius*radius*1.01 {
				continue
			}
			cs = append(cs, cand{e, n, cost(pt(e, n), scale)})
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].c < cs[j].c })
	var tops []cand
	for _, c := range cs {
		far := true
		for _, t := range tops {
			if math.Hypot(t.e-c.e, t.n-c.n) < 3*step {
				far = false
				break
			}
		}
		if far {
			tops = append(tops, c)
			if len(tops) == 3 {
				break
			}
		}
	}
	best := cand{c: math.Inf(1)}
	for _, t := range tops {
		e, n := t.e, t.n
		st := step
		var c float64
		for st > 30 {
			st /= 4
			sc := math.Max(150, st)
			c = math.Inf(1)
			be, bn := e, n
			for i := -4; i <= 4; i++ {
				for j := -4; j <= 4; j++ {
					v := cost(pt(e+float64(i)*st, n+float64(j)*st), sc)
					if v < c {
						c, be, bn = v, e+float64(i)*st, n+float64(j)*st
					}
				}
			}
			e, n = be, bn
		}
		c = cost(pt(e, n), 150)
		if c < best.c {
			best = cand{e, n, c}
		}
	}
	return pt(best.e, best.n)
}
