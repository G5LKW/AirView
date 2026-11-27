package ref

import "math"

// axis is the state of one kinematic axis of a reference track: position,
// velocity and acceleration with their covariance. The three axes of a
// track (east-like x, north-like y, height z; see doc.go "Coordinates") are
// filtered independently, so every matrix here is 3×3 and lives on the
// stack.
//
// The covariance is stored as the upper triangle in row order:
//
//	p = [p00 p01 p02 p11 p12 p22]
type axis struct {
	x [3]float64
	p [6]float64
}

// Process model: white jerk ("constant acceleration" / Wiener-process
// acceleration). With T the step and q the jerk power spectral density
// (m²/s⁵):
//
//	F(T) = | 1  T  T²/2 |      Q(T) = q·| T⁵/20  T⁴/8  T³/6 |
//	       | 0  1  T    |               | T⁴/8   T³/3  T²/2 |
//	       | 0  0  1    |               | T³/6   T²/2  T    |
//
// Q is the exact discretisation of the continuous model (doc.go "Process
// model"). For T < 0 (backward extrapolation before the first node) F(T) is
// used with Q(|T|), a conservative approximation.

// predict propagates the axis by dt seconds with jerk density q.
func (a *axis) predict(dt, q float64) {
	if dt == 0 {
		return
	}
	h := dt * dt / 2
	a.x[0] += dt*a.x[1] + h*a.x[2]
	a.x[1] += dt * a.x[2]
	a.p = propagateP(a.p, dt, q)
}

// propagateP returns F P Fᵀ + Q for step dt.
func propagateP(p [6]float64, dt, q float64) [6]float64 {
	T := dt
	h := T * T / 2
	a, b, c, d, e, f := p[0], p[1], p[2], p[3], p[4], p[5]
	// rows of F P
	r00 := a + T*b + h*c
	r01 := b + T*d + h*e
	r02 := c + T*e + h*f
	r11 := d + T*e
	r12 := e + T*f
	var o [6]float64
	o[0] = r00 + T*r01 + h*r02
	o[1] = r01 + T*r02
	o[2] = r02
	o[3] = r11 + T*r12
	o[4] = r12
	o[5] = f
	if q > 0 {
		t := math.Abs(T)
		t2 := t * t
		t3 := t2 * t
		o[0] += q * t3 * t2 / 20
		o[1] += q * t2 * t2 / 8 * sign(T)
		o[2] += q * t3 / 6
		o[3] += q * t3 / 3
		o[4] += q * t2 / 2 * sign(T)
		o[5] += q * t
	}
	return o
}

func sign(x float64) float64 {
	if x < 0 {
		return -1
	}
	return 1
}

// diagIdx maps a state component (0 pos, 1 vel, 2 acc) to its variance in
// the packed covariance.
var diagIdx = [3]int{0, 3, 5}

// col returns column i of the packed symmetric matrix.
func col(p *[6]float64, i int) [3]float64 {
	switch i {
	case 0:
		return [3]float64{p[0], p[1], p[2]}
	case 1:
		return [3]float64{p[1], p[3], p[4]}
	}
	return [3]float64{p[2], p[4], p[5]}
}

// innov returns the innovation of a scalar measurement z of component i and
// its variance S = P_ii + r.
func (a *axis) innov(i int, z, r float64) (nu, s float64) {
	return z - a.x[i], a.p[diagIdx[i]] + r
}

// update applies a scalar measurement z (variance r) of component i
// (0 position, 1 velocity). Standard form P⁺ = P − K Kᵀ S, which keeps P
// exactly symmetric.
func (a *axis) update(i int, z, r float64) {
	nu, s := a.innov(i, z, r)
	if !(s > 0) {
		return
	}
	k := col(&a.p, i)
	for j := range k {
		k[j] /= s
	}
	a.x[0] += k[0] * nu
	a.x[1] += k[1] * nu
	a.x[2] += k[2] * nu
	a.p[0] -= k[0] * k[0] * s
	a.p[1] -= k[0] * k[1] * s
	a.p[2] -= k[0] * k[2] * s
	a.p[3] -= k[1] * k[1] * s
	a.p[4] -= k[1] * k[2] * s
	a.p[5] -= k[2] * k[2] * s
	// guard against round-off making a variance negative
	for _, j := range diagIdx {
		if a.p[j] < 1e-12 {
			a.p[j] = 1e-12
		}
	}
}

// full expands a packed symmetric matrix.
func full(p *[6]float64) [3][3]float64 {
	return [3][3]float64{
		{p[0], p[1], p[2]},
		{p[1], p[3], p[4]},
		{p[2], p[4], p[5]},
	}
}

// invSym inverts a packed symmetric positive definite 3×3 matrix by the
// adjugate. ok is false when it is not (numerically) positive definite.
func invSym(p *[6]float64) (inv [3][3]float64, ok bool) {
	a, b, c, d, e, f := p[0], p[1], p[2], p[3], p[4], p[5]
	A := d*f - e*e
	B := c*e - b*f
	C := b*e - c*d
	det := a*A + b*B + c*C
	if !(det > 0) || !(a > 0) {
		return inv, false
	}
	D := a*f - c*c
	E := b*c - a*e
	F := a*d - b*b
	id := 1 / det
	inv = [3][3]float64{
		{A * id, B * id, C * id},
		{B * id, D * id, E * id},
		{C * id, E * id, F * id},
	}
	return inv, true
}

// smooth performs one Rauch-Tung-Striebel backward step. filt is the
// filtered state at time t, next the smoothed state at t+dt; the result is
// the smoothed state at t:
//
//	x⁻ = F x_f,  P⁻ = F P_f Fᵀ + Q
//	C  = P_f Fᵀ (P⁻)⁻¹
//	x_s = x_f + C (x_next − x⁻)
//	P_s = P_f + C (P_next − P⁻) Cᵀ
//
// When P⁻ is singular (never with q > 0) the filtered state is returned.
func smooth(filt *axis, dt, q float64, next *axis) axis {
	pred := *filt
	pred.predict(dt, q)
	inv, ok := invSym(&pred.p)
	if !ok {
		return *filt
	}
	T := dt
	h := T * T / 2
	pf := full(&filt.p)
	// M = P_f Fᵀ ; column j of Fᵀ is row j of F.
	var m [3][3]float64
	for i := 0; i < 3; i++ {
		m[i][0] = pf[i][0] + T*pf[i][1] + h*pf[i][2]
		m[i][1] = pf[i][1] + T*pf[i][2]
		m[i][2] = pf[i][2]
	}
	// C = M inv
	var cm [3][3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			cm[i][j] = m[i][0]*inv[0][j] + m[i][1]*inv[1][j] + m[i][2]*inv[2][j]
		}
	}
	var out axis
	var dx [3]float64
	for i := 0; i < 3; i++ {
		dx[i] = next.x[i] - pred.x[i]
	}
	for i := 0; i < 3; i++ {
		out.x[i] = filt.x[i] + cm[i][0]*dx[0] + cm[i][1]*dx[1] + cm[i][2]*dx[2]
	}
	// D = P_next − P⁻ ; P_s = P_f + C D Cᵀ
	var dp [6]float64
	for i := range dp {
		dp[i] = next.p[i] - pred.p[i]
	}
	d := full(&dp)
	var cd [3][3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			cd[i][j] = cm[i][0]*d[0][j] + cm[i][1]*d[1][j] + cm[i][2]*d[2][j]
		}
	}
	k := 0
	for i := 0; i < 3; i++ {
		for j := i; j < 3; j++ {
			out.p[k] = pf[i][j] + cd[i][0]*cm[j][0] + cd[i][1]*cm[j][1] + cd[i][2]*cm[j][2]
			k++
		}
	}
	for _, j := range diagIdx {
		if out.p[j] < 1e-12 {
			out.p[j] = 1e-12
		}
	}
	return out
}
