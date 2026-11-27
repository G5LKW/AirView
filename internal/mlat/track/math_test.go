package track

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

func TestChi2Quantile(t *testing.T) {
	// reference values of the chi-square 0.999 / 0.99 / 0.5 quantiles
	cases := []struct {
		p    float64
		k    int
		want float64
	}{
		{0.999, 1, 10.828}, {0.999, 2, 13.816}, {0.999, 3, 16.266}, {0.999, 4, 18.467},
		{0.999, 10, 29.588}, {0.999, 30, 59.703}, {0.99, 5, 15.086}, {0.5, 1, 0.45494},
		{0.6827, 2, 2.2958},
	}
	for _, c := range cases {
		got := Chi2Quantile(c.p, c.k)
		if math.Abs(got-c.want) > 2e-4*c.want+1e-4 {
			t.Errorf("Chi2Quantile(%v, %d) = %.5f, want %.5f", c.p, c.k, got, c.want)
		}
	}
	if !math.IsNaN(Chi2Quantile(1, 3)) || !math.IsNaN(Chi2Quantile(0.5, 0)) {
		t.Error("invalid arguments must give NaN")
	}
}

// numJac returns the numerical Jacobian of transition(k, ·, T).
func numJac(k int, x vec, T float64) (J mat) {
	for c := range nx {
		h := 1e-6 * math.Max(1, math.Abs(x[c]))
		if c == iW {
			h = 1e-7
		}
		xp, xm := x, x
		xp[c] += h
		xm[c] -= h
		fp, _ := transition(k, &xp, T)
		fm, _ := transition(k, &xm, T)
		for r := range nx {
			J[r*nx+c] = (fp[r] - fm[r]) / (2 * h)
		}
	}
	return
}

func TestTransitionJacobian(t *testing.T) {
	for _, w := range []float64{0, 1e-7, 3e-5, 0.01, 0.0524, -0.08} {
		for _, T := range []float64{0.05, 1, 7.3, -0.8} {
			x := vec{1200, -3400, 9000, 230, -95, 4, w}
			for k := range nModes {
				_, F := transition(k, &x, T)
				J := numJac(k, x, T)
				for i := range F {
					tol := 1e-5 * math.Max(1, math.Abs(J[i]))
					if i%nx == iW && i/nx < iVU {
						tol = 1e-3 * math.Max(1, math.Abs(J[i])) // ω column: larger numeric error
					}
					if math.Abs(F[i]-J[i]) > tol {
						t.Fatalf("model %d ω=%g T=%g: F[%d,%d] = %.9g, numeric %.9g", k, w, T, i/nx, i%nx, F[i], J[i])
					}
				}
			}
		}
	}
}

func TestCTExactCircle(t *testing.T) {
	// A 3°/s turn at 200 m/s: after 120 s (a full circle) the aircraft is
	// back at its start, and 60 steps of 2 s equal one step of 120 s.
	w := 3 * math.Pi / 180
	x0 := vec{0, 0, 3000, 200, 0, 0, w}
	x := x0
	for range 60 {
		x, _ = transition(modeCT, &x, 2)
	}
	one, _ := transition(modeCT, &x0, 120)
	for c := range nx {
		if math.Abs(x[c]-one[c]) > 1e-6 {
			t.Fatalf("composition differs at %d: %v vs %v", c, x[c], one[c])
		}
	}
	if math.Hypot(x[iE], x[iN]) > 1e-6 {
		t.Fatalf("not back at start after a full circle: %v", x)
	}
	// quarter circle: radius v/ω, moved to (R, R)
	q, _ := transition(modeCT, &x0, 30)
	R := 200 / w
	if math.Abs(q[iE]-R) > 1e-6 || math.Abs(q[iN]-R) > 1e-6 || math.Abs(q[iVN]-200) > 1e-9 {
		t.Fatalf("quarter circle wrong: %v (R = %v)", q, R)
	}
	// the small-ω series agrees with the closed form at the switch-over
	xs := x0
	xs[iVN] = -50
	for _, wt := range []float64{0.5e-4, 0.99e-4} {
		w := wt / 10
		xs[iW] = w
		a, _ := transition(modeCT, &xs, 10)
		s, c := math.Sincos(wt)
		e := s/w*xs[iVE] - (1-c)/w*xs[iVN]
		n := (1-c)/w*xs[iVE] + s/w*xs[iVN]
		if math.Abs(a[iE]-e) > 1e-6 || math.Abs(a[iN]-n) > 1e-6 {
			t.Fatalf("small-ω series differs from closed form: %v,%v vs %v,%v", a[iE], a[iN], e, n)
		}
	}
}

func TestProcessNoiseSPD(t *testing.T) {
	p := DefaultParams()
	for _, T := range []float64{0.01, 1, 10, -2} {
		for k := range nModes {
			Q := processNoise(k, T, &p, 0)
			// Q is PSD; add a tiny ridge for the strict test
			for i := range nx {
				Q[i*nx+i] += 1e-12
			}
			if linalg.Cholesky(Q[:], nx) != nil {
				t.Fatalf("Q(model %d, T %g) not PSD", k, T)
			}
		}
	}
}

// testFrame is a frame at 50°N 8°E.
func testFrame() geo.Frame { return geo.NewFrame(geo.LLH{Lat: 50, Lon: 8}) }

func TestEvalJacobian(t *testing.T) {
	fr := testFrame()
	tr := New(Params{})
	ms := &tr.ms
	ms.kind = kTOA
	ms.nst = 4
	ms.alt = true
	ms.rows = 4
	pts := [][2]float64{{50.6, 8.2}, {49.5, 7.1}, {50.1, 9.3}, {49.8, 8.5}}
	for j, q := range pts {
		ms.st[j] = member{st: uint32(j), pos: geo.ToECEF(q[0], q[1], 200)}
	}
	x := vec{12000, -25000, 9500, 200, 30, 0, 0}
	p := fr.FromENU(geo.Vec3{X: x[0], Y: x[1], Z: x[2]})
	for j := range 4 {
		tr.excess(&hyp{}, &ms.st[j], p)
	}
	tr.w.eval(ms, &fr, &x)
	H := tr.w.H
	for c := range 3 {
		xp, xm := x, x
		xp[c] += 1
		xm[c] -= 1
		var hp, hm [maxRows]float64
		tr.w.eval(ms, &fr, &xp)
		hp = tr.w.h
		tr.w.eval(ms, &fr, &xm)
		hm = tr.w.h
		for r := range 4 {
			num := (hp[r] - hm[r]) / 2
			if math.Abs(num-H[r*nx+c]) > 2e-6 {
				t.Fatalf("row %d col %d: analytic %.9f numeric %.9f", r, c, H[r*nx+c], num)
			}
		}
	}
	// linearised excess vs the exact model 2 km away (the refresh distance)
	q := p.Add(geo.Vec3{X: 1200, Y: -900, Z: 1300})
	m := &ms.st[1]
	exact := tr.p.Atmosphere.Range(m.pos, q)
	lin := q.Dist(m.pos) + m.exc + m.g.Dot(q.Sub(m.p0))
	if math.Abs(exact-lin) > 0.2 {
		t.Fatalf("linearised refraction off by %.3f m at 2 km", exact-lin)
	}
}

// TestUpdateLinearMatchesKalman checks the information-form update, its
// NIS and its log-likelihood against the textbook covariance-form Kalman
// update for a linear (position) measurement.
func TestUpdateLinearMatchesKalman(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	fr := testFrame()
	tr := New(Params{})
	for trial := range 50 {
		// random SPD prior
		var A mat
		for i := range A {
			A[i] = r.NormFloat64()
		}
		var P mat
		for i := range nx {
			for j := range nx {
				var s float64
				for k := range nx {
					s += A[i*nx+k] * A[j*nx+k]
				}
				P[i*nx+j] = s
			}
			P[i*nx+i] += 1
		}
		sc := [nx]float64{300, 300, 100, 20, 20, 5, 0.01}
		for i := range nx {
			for j := range nx {
				P[i*nx+j] *= sc[i] * sc[j]
			}
		}
		x := vec{100, 200, 9000, 150, -30, 1, 0.001}
		ms := &tr.ms
		ms.kind, ms.rows = kFix, 3
		var Rm [9]float64
		for i := range 3 {
			ms.z[i] = x[i] + r.NormFloat64()*500
			Rm[i*3+i] = 50 * 50 * (1 + float64(i))
		}
		Rm[1], Rm[3] = 400, 400
		copy(ms.R[:9], Rm[:])
		var out modelOut
		tr.w.updateModel(ms, &fr, modeCV, &x, &P, 0, &tr.p, &out)
		if !out.ok {
			t.Fatal("update failed")
		}
		// covariance form: S = HPHᵀ + R, K = PHᵀS⁻¹
		var S linalg.Mat3
		for i := range 3 {
			for j := range 3 {
				S[i*3+j] = P[i*nx+j] + Rm[i*3+j]
			}
		}
		Si, _ := S.InvSPD()
		var nu linalg.Vec3
		for i := range 3 {
			nu[i] = ms.z[i] - x[i]
		}
		nis := Si.Quad(nu)
		var K [nx * 3]float64
		for i := range nx {
			for j := range 3 {
				var s float64
				for k := range 3 {
					s += P[i*nx+k] * Si[k*3+j]
				}
				K[i*3+j] = s
			}
		}
		for i := range nx {
			var dx float64
			for j := range 3 {
				dx += K[i*3+j] * nu[j]
			}
			if d := math.Abs(out.x[i] - (x[i] + dx)); d > 1e-6*math.Max(1, math.Abs(x[i])) {
				t.Fatalf("trial %d: x[%d] differs by %g", trial, i, d)
			}
			for j := range nx {
				var kp float64
				for k := range 3 {
					kp += K[i*3+k] * P[k*nx+j]
				}
				want := P[i*nx+j] - kp
				if d := math.Abs(out.P[i*nx+j] - want); d > 1e-6*math.Sqrt(P[i*nx+i]*P[j*nx+j]) {
					t.Fatalf("trial %d: P[%d,%d] = %g, want %g", trial, i, j, out.P[i*nx+j], want)
				}
			}
		}
		if math.Abs(out.nis-nis) > 1e-7*math.Max(1, nis) {
			t.Fatalf("NIS %g, want %g", out.nis, nis)
		}
		ldS := math.Log(S[0]*(S[4]*S[8]-S[5]*S[7]) - S[1]*(S[3]*S[8]-S[5]*S[6]) + S[2]*(S[3]*S[7]-S[4]*S[6]))
		ll := -0.5 * (nis + ldS + 3*log2Pi)
		if math.Abs(out.ll-ll) > 1e-7*math.Max(1, math.Abs(ll)) {
			t.Fatalf("log-likelihood %g, want %g", out.ll, ll)
		}
	}
}

// TestOOSMMatchesRetrodiction: a late linear measurement applied with the
// retrodiction form must match the exact solution of a filter that saw it
// in sequence, to within the approximation (no process noise case: exact).
func TestOOSMExactWithoutProcessNoise(t *testing.T) {
	p := DefaultParams()
	p.QCV, p.QVert, p.QOmega = 1e-12, 1e-12, 1e-14
	p = p.Normalize()
	p.QCV, p.QVert, p.QOmega = 1e-12, 1e-12, 1e-14 // below the clamp on purpose
	tr := New(Params{})
	tr.p = p
	fr := testFrame()
	x := vec{0, 0, 9000, 200, 10, 0, 0}
	var P mat
	for i, s := range [nx]float64{100, 100, 50, 20, 20, 3, 0.01} {
		P[i*nx+i] = s * s
	}
	ms := &tr.ms
	ms.kind, ms.rows = kFix, 3
	z := [3]float64{-180, 10, 9020}
	for i := range 3 {
		ms.z[i] = z[i]
	}
	clear(ms.R[:9])
	ms.R[0], ms.R[4], ms.R[8] = 30*30, 30*30, 30*30
	// (a) in sequence: state at t = −1 s, update, predict to 0
	xa, Pa := x, P
	predict(modeCV, &xa, &Pa, -1, &p, 0) // exact back-propagation (Q≈0)
	var oa modelOut
	tr.w.updateModel(ms, &fr, modeCV, &xa, &Pa, 0, &p, &oa)
	predict(modeCV, &oa.x, &oa.P, 1, &p, 0)
	// (b) out of sequence at t = 0 with back = −1
	var ob modelOut
	tr.w.updateModel(ms, &fr, modeCV, &x, &P, -1, &p, &ob)
	for i := range nx {
		if math.Abs(oa.x[i]-ob.x[i]) > 1e-6*math.Max(1, math.Abs(oa.x[i])) {
			t.Fatalf("x[%d]: in-sequence %g, OOSM %g", i, oa.x[i], ob.x[i])
		}
	}
	for i := range oa.P {
		if math.Abs(oa.P[i]-ob.P[i]) > 1e-6*math.Max(1, math.Abs(oa.P[i])) {
			t.Fatalf("P[%d]: in-sequence %g, OOSM %g", i, oa.P[i], ob.P[i])
		}
	}
}

func TestRecentrePreservesState(t *testing.T) {
	tr := New(Params{})
	h := &hyp{}
	h.frame = testFrame()
	var P mat
	for i, s := range [nx]float64{100, 300, 50, 20, 10, 3, 0.01} {
		P[i*nx+i] = s * s
	}
	P[0*nx+1], P[1*nx+0] = 5000, 5000
	P[0*nx+3], P[3*nx+0] = 600, 600
	x := vec{80000, -60000, 10000, 210, -40, 2, 0.01}
	for j := range nModes {
		h.m[j] = modelState{x: x, P: P}
	}
	h.mu = [nModes]float64{0.7, 0.3}
	before := h.frame.FromENU(geo.Vec3{X: x[0], Y: x[1], Z: x[2]})
	vb := h.frame.Unrotate(geo.Vec3{X: x[3], Y: x[4], Z: x[5]})
	var pc [9]float64
	for a := range 3 {
		for b := range 3 {
			pc[a*3+b] = P[a*nx+b]
		}
	}
	cb := h.frame.CovToECEF(pc)
	tr.recentre(h)
	x2 := h.m[0].x
	after := h.frame.FromENU(geo.Vec3{X: x2[0], Y: x2[1], Z: x2[2]})
	va := h.frame.Unrotate(geo.Vec3{X: x2[3], Y: x2[4], Z: x2[5]})
	if before.Dist(after) > 1e-6 || vb.Dist(va) > 1e-9 {
		t.Fatalf("recentre moved the state: %v -> %v, %v -> %v", before, after, vb, va)
	}
	if math.Hypot(x2[0], x2[1]) > 1e-6 {
		t.Fatalf("not centred: %v", x2)
	}
	for a := range 3 {
		for b := range 3 {
			pc[a*3+b] = h.m[0].P[a*nx+b]
		}
	}
	ca := h.frame.CovToECEF(pc)
	for i := range ca {
		if math.Abs(ca[i]-cb[i]) > 1e-6*math.Max(1, math.Abs(cb[i])) {
			t.Fatalf("covariance changed in ECEF: %v vs %v", ca, cb)
		}
	}
}

func TestParamsNormalize(t *testing.T) {
	p := Params{}.Normalize()
	if p != DefaultParams().Normalize() {
		t.Fatal("zero params must normalise to the defaults")
	}
	q := Params{GateProb: 2, MaxHyp: 99, MaxMembers: 1000, Iterations: 50, MaxExclude: -1}.Normalize()
	if q.GateProb >= 1 || q.MaxHyp != maxHyp || q.MaxMembers != MaxClusterMembers || q.Iterations != 10 || q.MaxExclude != 0 {
		t.Fatalf("clamps: %+v", q)
	}
	if p.Normalize() != p {
		t.Fatal("Normalize must be idempotent")
	}
}
