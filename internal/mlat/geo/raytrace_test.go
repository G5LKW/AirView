package geo

import (
	"math"
	"testing"
)

// rayTrace integrates the 2-D ray equation d/ds(n·t) = ∇n through a
// spherically stratified exponential atmosphere (sphere radius R) with RK4,
// and shoots the launch elevation at the receiver (height h1) so that the
// ray reaches height h2 at central angle theta. It returns the optical path
// ∫n ds and the minimum ray height; ok is false when no ray connects the
// points above the ground. This is the independent reference for the
// closed model in refraction.go.
func rayTrace(atm Atmosphere, R, h1, h2, theta, ds float64) (optical, minH float64, ok bool) {
	nOf := func(x, y float64) (n, gx, gy float64) {
		r := math.Hypot(x, y)
		N := atm.K * atm.N0 * math.Exp(-(r-R)/atm.H)
		n = 1 + N*1e-6
		dn := -N * 1e-6 / atm.H // dn/dr
		return n, dn * x / r, dn * y / r
	}
	// State: position (x,y), direction t (unit), optical path.
	type st struct{ x, y, tx, ty float64 }
	deriv := func(s st) (st, float64) {
		n, gx, gy := nOf(s.x, s.y)
		gt := gx*s.tx + gy*s.ty
		return st{s.tx, s.ty, (gx - gt*s.tx) / n, (gy - gt*s.ty) / n}, n
	}
	shoot := func(e float64) (miss, opt, mh float64, hit bool) {
		s := st{0, R + h1, math.Cos(e), math.Sin(e)}
		mh = h1
		ang := 0.0
		for steps := 0; steps < 10_000_000; steps++ {
			k1, n1 := deriv(s)
			s2 := st{s.x + ds/2*k1.x, s.y + ds/2*k1.y, s.tx + ds/2*k1.tx, s.ty + ds/2*k1.ty}
			k2, n2 := deriv(s2)
			s3 := st{s.x + ds/2*k2.x, s.y + ds/2*k2.y, s.tx + ds/2*k2.tx, s.ty + ds/2*k2.ty}
			k3, n3 := deriv(s3)
			s4 := st{s.x + ds*k3.x, s.y + ds*k3.y, s.tx + ds*k3.tx, s.ty + ds*k3.ty}
			k4, n4 := deriv(s4)
			ns := st{
				s.x + ds/6*(k1.x+2*k2.x+2*k3.x+k4.x),
				s.y + ds/6*(k1.y+2*k2.y+2*k3.y+k4.y),
				s.tx + ds/6*(k1.tx+2*k2.tx+2*k3.tx+k4.tx),
				s.ty + ds/6*(k1.ty+2*k2.ty+2*k3.ty+k4.ty),
			}
			tn := math.Hypot(ns.tx, ns.ty)
			ns.tx, ns.ty = ns.tx/tn, ns.ty/tn
			dOpt := ds / 6 * (n1 + 2*n2 + 2*n3 + n4)
			nang := math.Atan2(ns.x, ns.y)
			if nang >= theta {
				// Fractional last step, linear in the angle.
				f := (theta - ang) / (nang - ang)
				r0, r1 := math.Hypot(s.x, s.y), math.Hypot(ns.x, ns.y)
				rEnd := r0 + f*(r1-r0)
				// Arc length of the partial step: f·ds, to O(ds²·κ).
				return rEnd - (R + h2), opt + f*dOpt, mh, true
			}
			s, ang, opt = ns, nang, opt+dOpt
			if h := math.Hypot(s.x, s.y) - R; h < mh {
				mh = h
			}
			if math.Hypot(s.x, s.y)-R < -50 {
				return -1e9, opt, mh, false // into the ground: below the target
			}
			if math.Hypot(s.x, s.y)-R > 200_000 {
				return 1e9, opt, mh, false
			}
		}
		return 0, 0, 0, false
	}
	lo, hi := -3*deg, 89*deg
	mlo, _, _, _ := shoot(lo)
	mhi, _, _, _ := shoot(hi)
	if !(mlo < 0 && mhi > 0) {
		return 0, 0, false
	}
	for i := 0; i < 80; i++ {
		mid := (lo + hi) / 2
		m, o, mh, _ := shoot(mid)
		if math.Abs(m) < 1e-7 || hi-lo < 1e-15 {
			return o, mh, mh >= 0
		}
		if m < 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	m, o, mh, _ := shoot((lo + hi) / 2)
	_ = m
	return o, mh, mh >= 0
}

// sphereEnds returns the ECEF-like end points of the trace geometry.
func sphereEnds(R, h1, h2, theta float64) (Vec3, Vec3) {
	return Vec3{0, R + h1, 0}, Vec3{(R + h2) * math.Sin(theta), (R + h2) * math.Cos(theta), 0}
}

// TestRefractionVsRayTrace is the acceptance check of DESIGN §9.1: the
// closed model must stay within 1 m of a numeric ray trace at 400 km.
func TestRefractionVsRayTrace(t *testing.T) {
	R := MeanEarthRadius
	type tc struct{ h1, h2, groundKm float64 }
	cases := []tc{
		{10, 3000, 50}, {10, 10000, 50}, {300, 11000, 150},
		{10, 10000, 300}, {50, 12000, 300}, {300, 6000, 250},
		{50, 11000, 400}, {300, 12000, 400}, {800, 10000, 400}, {1000, 12500, 420},
		{100, 10000, 5}, {100, 10000, 20}, // steep
		{9000, 11000, 400}, // aircraft-to-aircraft-like, high path
	}
	if testing.Short() {
		cases = cases[6:8]
	}
	for _, atm := range []Atmosphere{DefaultAtmosphere(), {N0: 380, H: 6000, K: 1.2}, {N0: 280, H: 8000, K: 0.8}} {
		for _, c := range cases {
			theta := c.groundKm * 1000 / R
			opt, minH, ok := rayTrace(atm, R, c.h1, c.h2, theta, 25)
			if !ok {
				t.Logf("skip %+v: no clear ray (min height %.0f m)", c, minH)
				continue
			}
			p1, p2 := sphereEnds(R, c.h1, c.h2, theta)
			m := atm
			m.sphereR = R
			L := m.Range(p1, p2)
			D := p1.Dist(p2)
			st := m.StraightExcess(D, c.h1, c.h2)
			err := L - opt
			t.Logf("N0=%v H=%v K=%v h1=%5.0f h2=%6.0f d=%3.0f km: excess trace %.3f m model %.3f m (err %+.3f m), straight-line formula %+.2f m",
				atm.N0, atm.H, atm.K, c.h1, c.h2, c.groundKm, opt-D, L-D, err, D+st-opt)
			if math.Abs(err) > 0.05 { // acceptance is 1 m; guard the model at 5 cm
				t.Errorf("%+v: model differs from ray trace by %.3f m", c, err)
			}
			if L < D {
				t.Errorf("%+v: optical path shorter than chord", c)
			}
		}
	}
}

// TestRayTraceConverged guards the reference itself: halving the step must
// not change the optical path by more than a centimetre.
func TestRayTraceConverged(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	atm := DefaultAtmosphere()
	R := MeanEarthRadius
	theta := 400e3 / R
	a, _, ok1 := rayTrace(atm, R, 300, 12000, theta, 50)
	b, _, ok2 := rayTrace(atm, R, 300, 12000, theta, 25)
	if !ok1 || !ok2 {
		t.Fatal("trace failed")
	}
	if math.Abs(a-b) > 0.01 {
		t.Errorf("ray trace not converged: %.4f vs %.4f", a, b)
	}
}

// TestRangeGradVsRayTrace checks the height derivative of the optical path
// against central differences of the numeric ray trace.
func TestRangeGradVsRayTrace(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	atm := DefaultAtmosphere()
	R := MeanEarthRadius
	m := atm
	m.sphereR = R
	for _, c := range [][3]float64{{50, 6000, 300}, {50, 11000, 400}, {300, 3000, 150}} {
		th := c[2] * 1000 / R
		p1, p2 := sphereEnds(R, c[0], c[1], th)
		_, g := m.RangeGrad(p1, p2)
		const dh = 5.0
		tp, _, ok1 := rayTrace(atm, R, c[0], c[1]+dh, th, 10)
		tm, _, ok2 := rayTrace(atm, R, c[0], c[1]-dh, th, 10)
		if !ok1 || !ok2 {
			t.Fatalf("%v: trace failed", c)
		}
		want := (tp - tm) / (2 * dh)
		got := g.Dot(p2.Unit())
		if math.Abs(got-want) > 1e-6 {
			t.Errorf("%v: dL/dh model %.8f, ray trace %.8f", c, got, want)
		}
	}
}
