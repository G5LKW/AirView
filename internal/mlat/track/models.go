package track

import "math"

// State layout (see doc.go): position and velocity in the hypothesis' local
// ENU frame, plus the turn rate.
const (
	iE  = 0 // east, m
	iN  = 1 // north, m
	iU  = 2 // up, m
	iVE = 3 // east velocity, m/s
	iVN = 4 // north velocity, m/s
	iVU = 5 // vertical velocity, m/s
	iW  = 6 // turn rate ω, rad/s (positive = counter-clockwise from above)
	nx  = 7
)

// Motion models of the IMM.
const (
	modeCV = 0 // constant velocity (ω carried passively)
	modeCT = 1 // coordinated turn with turn-rate state
	nModes = 2
)

type vec [nx]float64
type mat [nx * nx]float64

func identity() (m mat) {
	for i := range nx {
		m[i*nx+i] = 1
	}
	return
}

// mul returns a·b.
func mul(a, b *mat) (c mat) {
	for i := range nx {
		ai := a[i*nx : i*nx+nx]
		ci := c[i*nx : i*nx+nx]
		for k, v := range ai {
			if v == 0 {
				continue
			}
			bk := b[k*nx : k*nx+nx]
			for j := range ci {
				ci[j] += v * bk[j]
			}
		}
	}
	return
}

// congruence returns F·P·Fᵀ (symmetric).
func congruence(F, P *mat) mat {
	t := mul(F, P)
	var c mat
	for i := range nx {
		ti := t[i*nx : i*nx+nx]
		for j := 0; j <= i; j++ {
			fj := F[j*nx : j*nx+nx]
			var s float64
			for k := range ti {
				s += ti[k] * fj[k]
			}
			c[i*nx+j] = s
			c[j*nx+i] = s
		}
	}
	return c
}

func symmetrize(P *mat) {
	for i := range nx {
		for j := range i {
			v := 0.5 * (P[i*nx+j] + P[j*nx+i])
			P[i*nx+j], P[j*nx+i] = v, v
		}
	}
}

// transition returns f(x, T) and its Jacobian F for motion model k over T
// seconds (T may be negative: retrodiction for out-of-sequence updates).
//
// CV:  p' = p + T·v, v' = v, ω' = ω.
// CT:  the horizontal velocity rotates at ω (exact solution of
// v̇ = ω × v); the vertical channel is CV in both models.
func transition(k int, x *vec, T float64) (xn vec, F mat) {
	F = identity()
	xn = *x
	// vertical channel (both models)
	xn[iU] = x[iU] + T*x[iVU]
	F[iU*nx+iVU] = T
	if k == modeCV {
		xn[iE] = x[iE] + T*x[iVE]
		xn[iN] = x[iN] + T*x[iVN]
		F[iE*nx+iVE] = T
		F[iN*nx+iVN] = T
		return
	}
	w := x[iW]
	ve, vn := x[iVE], x[iVN]
	wt := w * T
	var sw, cw, A, B, s, c float64 // sw = sin(ωT)/ω, cw = (1−cos ωT)/ω
	if math.Abs(wt) < 1e-4 {
		wt2 := wt * wt
		sw = T * (1 - wt2/6)
		cw = w * T * T / 2 * (1 - wt2/12)
		s = wt * (1 - wt2/6)
		c = 1 - wt2/2
		A = -w * T * T * T / 3      // (T·cos ωT·ω − sin ωT)/ω², to O(ω³)
		B = T * T / 2 * (1 - wt2/4) // (T·sin ωT·ω − (1 − cos ωT))/ω², to O(ω²)
	} else {
		s, c = math.Sincos(wt)
		sw = s / w
		cw = (1 - c) / w
		A = (T*c*w - s) / (w * w)
		B = (T*s*w - (1 - c)) / (w * w)
	}
	xn[iE] = x[iE] + sw*ve - cw*vn
	xn[iN] = x[iN] + cw*ve + sw*vn
	xn[iVE] = c*ve - s*vn
	xn[iVN] = s*ve + c*vn
	F[iE*nx+iVE], F[iE*nx+iVN], F[iE*nx+iW] = sw, -cw, A*ve-B*vn
	F[iN*nx+iVE], F[iN*nx+iVN], F[iN*nx+iW] = cw, sw, B*ve+A*vn
	F[iVE*nx+iVE], F[iVE*nx+iVN], F[iVE*nx+iW] = c, -s, -T*s*ve-T*c*vn
	F[iVN*nx+iVE], F[iVN*nx+iVN], F[iVN*nx+iW] = s, c, T*c*ve-T*s*vn
	return
}

// motion is transition followed by the turn-rate decay of the
// Ornstein-Uhlenbeck turn model: ω' = e^(−T/τ_ω)·ω (aircraft do not turn
// forever; doc.go §3).
func motion(k int, x *vec, T float64, p *Params) (xn vec, F mat) {
	xn, F = transition(k, x, T)
	a := math.Exp(-T / p.OmegaTauS)
	xn[iW] = a * x[iW]
	F[iW*nx+iW] = a
	return
}

// processNoise returns the discrete process noise of model k over T
// seconds (gq is added to the horizontal PSD: the manoeuvre allowance of an
// unobservable stretch, Tracker.gapQ): continuous white-noise acceleration
// per axis (PSD q) gives
// [[q|T|³/3, qT|T|/2], [qT|T|/2, q|T|]]; the turn rate is an
// Ornstein-Uhlenbeck process with driving PSD QOmega and time constant
// τ_ω, whose discrete noise is QOmega·τ_ω/2·(1 − e^(−2|T|/τ_ω)). The
// signed cross term makes the backward (T < 0) noise correct for
// retrodiction.
func processNoise(k int, T float64, p *Params, gq float64) (Q mat) {
	a := math.Abs(T)
	qh := p.QCV
	if k == modeCT {
		qh = p.QCT
	}
	qh += gq
	axis := func(i, j int, q float64) {
		Q[i*nx+i] = q * a * a * a / 3
		Q[i*nx+j] = q * T * a / 2
		Q[j*nx+i] = q * T * a / 2
		Q[j*nx+j] = q * a
	}
	axis(iE, iVE, qh)
	axis(iN, iVN, qh)
	axis(iU, iVU, p.QVert)
	Q[iW*nx+iW] = p.QOmega * p.OmegaTauS / 2 * -math.Expm1(-2*a/p.OmegaTauS)
	return
}

// predict propagates (x, P) of model k by T seconds in place.
func predict(k int, x *vec, P *mat, T float64, p *Params, gq float64) {
	if T == 0 {
		return
	}
	xn, F := motion(k, x, T, p)
	Pn := congruence(&F, P)
	Q := processNoise(k, T, p, gq)
	for i := range Pn {
		Pn[i] += Q[i]
	}
	*x, *P = xn, Pn
}

// switchProb returns the Markov transition matrix π_ij(T) (row i = from
// mode i): leave mode i with probability 1 − exp(−T/τ_i).
func switchProb(T float64, p *Params) (pi [nModes][nModes]float64) {
	if T < 0 {
		T = 0
	}
	l0 := -math.Expm1(-T / p.SojournCVS)
	l1 := -math.Expm1(-T / p.SojournCTS)
	pi[0][0], pi[0][1] = 1-l0, l0
	pi[1][0], pi[1][1] = l1, 1-l1
	return
}
