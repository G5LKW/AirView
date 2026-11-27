package trajectory

import "math"

// Horizontal state: x, y (m, east/north in the track's tangent plane),
// vx, vy (m/s, ground velocity), w (turn rate, rad/s, + = clockwise / right).
const (
	ix = iota
	iy
	ivx
	ivy
	iw
	nx
)

type vec5 [nx]float64
type mat5 [nx][nx]float64

const (
	mCV = 0 // constant velocity
	mCT = 1 // coordinated turn (air-relative velocity rotates; constant wind)
)

func ident5() mat5 {
	var m mat5
	for i := 0; i < nx; i++ {
		m[i][i] = 1
	}
	return m
}

// fpft returns F P F'.
func fpft(F, P *mat5) mat5 {
	var t, r mat5
	for i := 0; i < nx; i++ {
		for j := 0; j < nx; j++ {
			var s float64
			for k := 0; k < nx; k++ {
				s += F[i][k] * P[k][j]
			}
			t[i][j] = s
		}
	}
	for i := 0; i < nx; i++ {
		for j := i; j < nx; j++ {
			var s float64
			for k := 0; k < nx; k++ {
				s += t[i][k] * F[j][k]
			}
			r[i][j] = s
			r[j][i] = s
		}
	}
	return r
}

// sanitize keeps P symmetric with sane, finite diagonals.
func sanitize(P *mat5) bool {
	ok := true
	floor := [nx]float64{0.01, 0.01, 1e-4, 1e-4, 1e-10}
	ceil := [nx]float64{1e14, 1e14, 1e6, 1e6, 1}
	for i := 0; i < nx; i++ {
		d := P[i][i]
		if !finite(d) {
			ok = false
			d = ceil[i]
		}
		P[i][i] = clamp(d, floor[i], ceil[i])
	}
	for i := 0; i < nx; i++ {
		for j := i + 1; j < nx; j++ {
			v := 0.5 * (P[i][j] + P[j][i])
			if !finite(v) {
				ok = false
				v = 0
			}
			lim := 0.999 * math.Sqrt(P[i][i]*P[j][j])
			v = clamp(v, -lim, lim)
			P[i][j], P[j][i] = v, v
		}
	}
	return ok
}

// motion advances x over T with turn rate w (rad/s) applied to the
// air-relative velocity (ground velocity minus wind wx, wy). If F != nil it
// receives the Jacobian; column iw is scaled by dw (= d w / d x[iw]).
func motion(x *vec5, T, w, wx, wy, dw float64, F *mat5) {
	vax, vay := x[ivx]-wx, x[ivy]-wy
	wt := w * T
	var a, b, da, db float64
	s, c := math.Sincos(wt)
	if math.Abs(wt) < 1e-4 {
		T2 := T * T
		a = T - w*w*T2*T/6
		b = w*T2/2 - w*w*w*T2*T2/24
		da = -w * T2 * T / 3
		db = T2/2 - w*w*T2*T2/8
	} else {
		a = s / w
		b = (1 - c) / w
		da = (T*c*w - s) / (w * w)
		db = (T*s*w - (1 - c)) / (w * w)
	}
	if F != nil {
		*F = mat5{}
		F[ix][ix] = 1
		F[ix][ivx] = a
		F[ix][ivy] = b
		F[iy][iy] = 1
		F[iy][ivx] = -b
		F[iy][ivy] = a
		F[ivx][ivx] = c
		F[ivx][ivy] = s
		F[ivy][ivx] = -s
		F[ivy][ivy] = c
		F[ix][iw] = (da*vax + db*vay) * dw
		F[iy][iw] = (-db*vax + da*vay) * dw
		F[ivx][iw] = T * (-s*vax + c*vay) * dw
		F[ivy][iw] = T * (-c*vax - s*vay) * dw
		F[iw][iw] = 1
	}
	x[ix] += wx*T + a*vax + b*vay
	x[iy] += wy*T - b*vax + a*vay
	x[ivx] = wx + c*vax + s*vay
	x[ivy] = wy - s*vax + c*vay
}

// addQ adds discrete white-acceleration noise q (m^2/s^3) and turn-rate
// random walk qw ((rad/s)^2/s) over T.
func addQ(P *mat5, T, q, qw float64) {
	t2, t3 := T*T, T*T*T
	P[ix][ix] += q * t3 / 3
	P[iy][iy] += q * t3 / 3
	P[ix][ivx] += q * t2 / 2
	P[ivx][ix] += q * t2 / 2
	P[iy][ivy] += q * t2 / 2
	P[ivy][iy] += q * t2 / 2
	P[ivx][ivx] += q * T
	P[ivy][ivy] += q * T
	P[iw][iw] += qw * T
}

type innov struct {
	n      int
	idx    [2]int
	nu     [2]float64
	R      [2][2]float64
	si     [2][2]float64 // S^-1
	d2     float64
	logLik float64
	ok     bool
}

func (in *innov) compute(x *vec5, P *mat5, z [2]float64) {
	in.ok = false
	n := in.n
	var S [2][2]float64
	for a := 0; a < n; a++ {
		in.nu[a] = z[a] - x[in.idx[a]]
		for b := 0; b < n; b++ {
			S[a][b] = P[in.idx[a]][in.idx[b]] + in.R[a][b]
		}
	}
	if n == 1 {
		if !(S[0][0] > 0) {
			return
		}
		in.si[0][0] = 1 / S[0][0]
		in.d2 = in.nu[0] * in.nu[0] * in.si[0][0]
		in.logLik = -0.5 * (in.d2 + math.Log(2*math.Pi*S[0][0]))
	} else {
		det := S[0][0]*S[1][1] - S[0][1]*S[1][0]
		if !(det > 0) {
			return
		}
		in.si[0][0] = S[1][1] / det
		in.si[1][1] = S[0][0] / det
		in.si[0][1] = -S[0][1] / det
		in.si[1][0] = -S[1][0] / det
		v0, v1 := in.nu[0], in.nu[1]
		in.d2 = v0*(in.si[0][0]*v0+in.si[0][1]*v1) + v1*(in.si[1][0]*v0+in.si[1][1]*v1)
		in.logLik = -0.5 * (in.d2 + math.Log(4*math.Pi*math.Pi*det))
	}
	in.ok = finite(in.d2) && finite(in.logLik)
}

// apply performs the Joseph-form update with a computed innovation.
func (in *innov) apply(x *vec5, P *mat5) {
	n := in.n
	var K [nx][2]float64
	for r := 0; r < nx; r++ {
		for j := 0; j < n; j++ {
			var s float64
			for a := 0; a < n; a++ {
				s += P[r][in.idx[a]] * in.si[a][j]
			}
			K[r][j] = s
		}
	}
	for r := 0; r < nx; r++ {
		for j := 0; j < n; j++ {
			x[r] += K[r][j] * in.nu[j]
		}
	}
	A := ident5()
	for r := 0; r < nx; r++ {
		for j := 0; j < n; j++ {
			A[r][in.idx[j]] -= K[r][j]
		}
	}
	np := fpft(&A, P)
	for r := 0; r < nx; r++ {
		for c := r; c < nx; c++ {
			var s float64
			for a := 0; a < n; a++ {
				for b := 0; b < n; b++ {
					s += K[r][a] * in.R[a][b] * K[c][b]
				}
			}
			np[r][c] += s
			if c != r {
				np[c][r] += s
			}
		}
	}
	*P = np
	sanitize(P)
}

type modelState struct {
	x vec5
	P mat5
}

// imm is the two-model (CV / CT) interacting multiple model filter.
type imm struct {
	fr  frame
	t   float64 // seconds (Unix)
	m   [2]modelState
	mu  [2]float64
	x   vec5 // combined
	P   mat5
	ll  [2]float64 // log-likelihood accumulated in the current cycle
	nUp int        // measurement updates in the current cycle
}

type immParams struct {
	qCV, qCT, qW float64
	tauCV, tauCT float64
	gate         float64
}

func (f *imm) init(fr frame, t float64, x vec5, P mat5) {
	f.fr = fr
	f.t = t
	f.mu = [2]float64{0.8, 0.2}
	for j := range f.m {
		f.m[j].x = x
		f.m[j].P = P
	}
	f.m[mCV].x[iw] = 0
	f.m[mCV].P[iw][iw] = 1e-8
	for k := 0; k < iw; k++ {
		f.m[mCV].P[iw][k], f.m[mCV].P[k][iw] = 0, 0
	}
	f.combine()
}

// predict mixes and propagates both models to time t (wind wx, wy in m/s).
func (f *imm) predict(t float64, p *immParams, wx, wy float64) {
	f.ll = [2]float64{}
	f.nUp = 0
	dt := t - f.t
	if !(dt > 0) {
		return
	}
	f.t = t
	// Markov switching probabilities over dt.
	p01 := clamp(1-math.Exp(-dt/p.tauCV), 1e-3, 0.5)
	p10 := clamp(1-math.Exp(-dt/p.tauCT), 1e-3, 0.5)
	pi := [2][2]float64{{1 - p01, p01}, {p10, 1 - p10}}
	var cbar [2]float64
	for j := 0; j < 2; j++ {
		cbar[j] = pi[0][j]*f.mu[0] + pi[1][j]*f.mu[1]
	}
	var mixed [2]modelState
	for j := 0; j < 2; j++ {
		var w [2]float64
		for i := 0; i < 2; i++ {
			w[i] = pi[i][j] * f.mu[i] / cbar[j]
		}
		var xm vec5
		for i := 0; i < 2; i++ {
			for k := 0; k < nx; k++ {
				xm[k] += w[i] * f.m[i].x[k]
			}
		}
		var Pm mat5
		for i := 0; i < 2; i++ {
			var d vec5
			for k := 0; k < nx; k++ {
				d[k] = f.m[i].x[k] - xm[k]
			}
			for r := 0; r < nx; r++ {
				for c := 0; c < nx; c++ {
					Pm[r][c] += w[i] * (f.m[i].P[r][c] + d[r]*d[c])
				}
			}
		}
		mixed[j] = modelState{xm, Pm}
	}
	f.mu = cbar
	var F mat5
	// CV: no turn; turn-rate state pinned near zero.
	cv := &f.m[mCV]
	*cv = mixed[mCV]
	cv.x[iw] = 0
	motion(&cv.x, dt, 0, 0, 0, 0, &F)
	F[iw][iw] = 0
	cv.P = fpft(&F, &cv.P)
	addQ(&cv.P, dt, p.qCV, 0)
	cv.P[iw][iw] = 1e-8
	sanitize(&cv.P)
	// CT.
	ct := &f.m[mCT]
	*ct = mixed[mCT]
	motion(&ct.x, dt, ct.x[iw], wx, wy, 1, &F)
	ct.P = fpft(&F, &ct.P)
	addQ(&ct.P, dt, p.qCT, p.qW)
	sanitize(&ct.P)
}

// update applies one measurement to both models. It returns the smallest
// Mahalanobis distance and whether the measurement passed the gate (and was applied).
func (f *imm) update(n int, idx [2]int, z [2]float64, R [2][2]float64, gate float64) (float64, bool) {
	var in [2]innov
	d2 := math.Inf(1)
	for j := 0; j < 2; j++ {
		in[j] = innov{n: n, idx: idx, R: R}
		in[j].compute(&f.m[j].x, &f.m[j].P, z)
		if !in[j].ok {
			return math.Inf(1), false
		}
		if in[j].d2 < d2 {
			d2 = in[j].d2
		}
	}
	if gate > 0 && d2 > gate {
		return d2, false
	}
	for j := 0; j < 2; j++ {
		in[j].apply(&f.m[j].x, &f.m[j].P)
		f.ll[j] += in[j].logLik
	}
	f.m[mCV].x[iw] = 0
	f.nUp++
	return d2, true
}

// finish updates the model probabilities from this cycle's likelihoods and
// recombines.
func (f *imm) finish() {
	if f.nUp > 0 {
		mx := math.Max(f.ll[0], f.ll[1])
		var s float64
		var nm [2]float64
		for j := 0; j < 2; j++ {
			nm[j] = f.mu[j] * math.Exp(f.ll[j]-mx)
			s += nm[j]
		}
		if s > 0 && finite(s) {
			for j := 0; j < 2; j++ {
				f.mu[j] = clamp(nm[j]/s, 1e-4, 1-1e-4)
			}
		}
	}
	f.mu[1] = 1 - f.mu[0]
	f.combine()
}

func (f *imm) combine() {
	var x vec5
	for j := 0; j < 2; j++ {
		for k := 0; k < nx; k++ {
			x[k] += f.mu[j] * f.m[j].x[k]
		}
	}
	var P mat5
	for j := 0; j < 2; j++ {
		var d vec5
		for k := 0; k < nx; k++ {
			d[k] = f.m[j].x[k] - x[k]
		}
		for r := 0; r < nx; r++ {
			for c := 0; c < nx; c++ {
				P[r][c] += f.mu[j] * (f.m[j].P[r][c] + d[r]*d[c])
			}
		}
	}
	sanitize(&P)
	f.x, f.P = x, P
}

// recenter moves the tangent plane origin to the current combined position,
// rotating velocities and covariances by the grid convergence.
func (f *imm) recenter() {
	lat, lon := f.fr.toLatLon(f.x[ix], f.x[iy])
	g := f.fr.gridConvergence(lat, lon)
	nf := newFrame(lat, lon)
	sg, cg := math.Sincos(g)
	var Rt mat5
	Rt[ix][ix], Rt[ix][iy], Rt[iy][ix], Rt[iy][iy] = cg, -sg, sg, cg
	Rt[ivx][ivx], Rt[ivx][ivy], Rt[ivy][ivx], Rt[ivy][ivy] = cg, -sg, sg, cg
	Rt[iw][iw] = 1
	for j := 0; j < 2; j++ {
		m := &f.m[j]
		la, lo := f.fr.toLatLon(m.x[ix], m.x[iy])
		m.x[ix], m.x[iy] = nf.toXY(la, lo)
		vx, vy := m.x[ivx], m.x[ivy]
		m.x[ivx] = cg*vx - sg*vy
		m.x[ivy] = sg*vx + cg*vy
		m.P = fpft(&Rt, &m.P)
	}
	f.fr = nf
	f.combine()
}

// kf2 is a two-state (value, rate) Kalman filter with white-acceleration noise.
type kf2 struct {
	x  [2]float64
	P  [2][2]float64
	t  float64
	ok bool
	// consecutive rejected value measurements
	rej int
}

func (k *kf2) init(t, v, varV, r, varR float64) {
	k.t = t
	k.x = [2]float64{v, r}
	k.P = [2][2]float64{{varV, 0}, {0, varR}}
	k.ok = true
	k.rej = 0
}

func (k *kf2) predict(t, q float64) {
	dt := t - k.t
	if !(dt > 0) {
		return
	}
	k.t = t
	k.x[0] += k.x[1] * dt
	p := k.P
	k.P[0][0] = p[0][0] + 2*dt*p[0][1] + dt*dt*p[1][1] + q*dt*dt*dt/3
	k.P[0][1] = p[0][1] + dt*p[1][1] + q*dt*dt/2
	k.P[1][0] = k.P[0][1]
	k.P[1][1] = p[1][1] + q*dt
}

// update measures state i with variance r; returns d2 and whether applied.
func (k *kf2) update(i int, z, r, gate float64) (float64, bool) {
	s := k.P[i][i] + r
	if !(s > 0) {
		return math.Inf(1), false
	}
	nu := z - k.x[i]
	d2 := nu * nu / s
	if gate > 0 && d2 > gate {
		return d2, false
	}
	k0 := k.P[0][i] / s
	k1 := k.P[1][i] / s
	k.x[0] += k0 * nu
	k.x[1] += k1 * nu
	// Joseph form for H = e_i.
	var A [2][2]float64
	A[0][0], A[1][1] = 1, 1
	A[0][i] -= k0
	A[1][i] -= k1
	p := k.P
	var t [2][2]float64
	for a := 0; a < 2; a++ {
		for b := 0; b < 2; b++ {
			t[a][b] = A[a][0]*p[0][b] + A[a][1]*p[1][b]
		}
	}
	for a := 0; a < 2; a++ {
		for b := 0; b < 2; b++ {
			k.P[a][b] = t[a][0]*A[b][0] + t[a][1]*A[b][1]
		}
	}
	kk := [2]float64{k0, k1}
	for a := 0; a < 2; a++ {
		for b := 0; b < 2; b++ {
			k.P[a][b] += kk[a] * r * kk[b]
		}
	}
	sym := 0.5 * (k.P[0][1] + k.P[1][0])
	k.P[0][1], k.P[1][0] = sym, sym
	k.P[0][0] = math.Max(k.P[0][0], 1e-6)
	k.P[1][1] = math.Max(k.P[1][1], 1e-8)
	if !finite(k.x[0]) || !finite(k.x[1]) || !finite(k.P[0][0]) || !finite(k.P[1][1]) || !finite(sym) {
		k.ok = false
	}
	return d2, true
}
