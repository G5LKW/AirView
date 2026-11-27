package clock

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
	"airview/internal/model"
)

func randSPD(r *rand.Rand, n int, scale []float64) []float64 {
	b := make([]float64, n*n)
	for i := range b {
		b[i] = r.NormFloat64()
	}
	a := make([]float64, n*n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			s := 0.0
			for k := 0; k < n; k++ {
				s += b[i*n+k] * b[j*n+k]
			}
			a[i*n+j] = s * scale[i] * scale[j]
		}
		a[i*n+i] += 0.5 * scale[i] * scale[i]
	}
	return a
}

func matMul(a, b []float64, n int) []float64 {
	c := make([]float64, n*n)
	for i := 0; i < n; i++ {
		for k := 0; k < n; k++ {
			if a[i*n+k] == 0 {
				continue
			}
			for j := 0; j < n; j++ {
				c[i*n+j] += a[i*n+k] * b[k*n+j]
			}
		}
	}
	return c
}

func transpose(a []float64, n int) []float64 {
	t := make([]float64, n*n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			t[j*n+i] = a[i*n+j]
		}
	}
	return t
}

func maxRelDiff(a, b []float64) float64 {
	m, s := 0.0, 0.0
	for i := range a {
		m = math.Max(m, math.Abs(a[i]-b[i]))
		s = math.Max(s, math.Abs(b[i]))
	}
	return m / s
}

// testComponent builds a component of an SDR (θ, φ, ψ, β), a hw12 (θ, φ,
// β) and an anchor (δ, β) with a random covariance.
func testComponent(r *rand.Rand) (*component, []*station) {
	mk := func(idx model.StationIdx, cls model.ClockClass, m mode) *station {
		s := newStation(idx)
		s.setInfo(StationInfo{Class: cls, Located: true})
		s.mode = m
		s.fresh = true
		s.qScale = 0.7 + r.Float64()
		return s
	}
	sts := []*station{mk(2, model.ClockSDR12, modeFree), mk(3, model.ClockHW12, modeFree), mk(4, model.ClockGPSUTC, modeAnchor)}
	c := buildComponent(1, 1, sts, 10, 1e6)
	n := c.n
	scale := make([]float64, n)
	for i := range scale {
		scale[i] = math.Pow(10, r.Float64()*4-1)
	}
	copy(c.P, randSPD(r, n, scale))
	for i := range c.x {
		c.x[i] = r.NormFloat64() * scale[i] * 10
	}
	return c, sts
}

// fullFQ returns the transition and process-noise matrices of the
// component over dt, built entry by entry from the model definition.
func fullFQ(c *component, dt float64) (F, Q []float64) {
	n := c.n
	F = make([]float64, n*n)
	Q = make([]float64, n*n)
	for i := 0; i < n; i++ {
		F[i*n+i] = 1
	}
	for _, s := range c.sts {
		it := s.off + int(s.layout[kTheta])
		ib := s.off + int(s.layout[kBeta])
		Q[ib*n+ib] = qBeta * dt
		if s.mode == modeAnchor {
			Q[it*n+it] = s.qScale * qDelta * dt
			continue
		}
		ip := s.off + int(s.layout[kPhi])
		F[it*n+ip] = dt
		q := s.qScale
		// continuous white-noise model integrated exactly:
		// Q = ∫ Φ(τ) diag(qθ, qφ, qψ) Φ(τ)ᵀ dτ
		qt, qp, qs := q*s.prior.qTheta, q*s.prior.qPhi, 0.0
		idx := []int{it, ip}
		if s.psi {
			is := s.off + int(s.layout[kPsi])
			F[it*n+is] = dt * dt / 2
			F[ip*n+is] = dt
			idx = append(idx, is)
			qs = q * s.prior.qPsi
		}
		// numerical integration (Simpson, 200 steps) of the 3×3 block
		const steps = 200
		var blk [3][3]float64
		for k := 0; k <= steps; k++ {
			tau := dt * float64(k) / steps
			w := 1.0
			if k > 0 && k < steps {
				if k%2 == 1 {
					w = 4
				} else {
					w = 2
				}
			}
			phi := [3][3]float64{{1, tau, tau * tau / 2}, {0, 1, tau}, {0, 0, 1}}
			qd := [3]float64{qt, qp, qs}
			for a := 0; a < 3; a++ {
				for b := 0; b < 3; b++ {
					v := 0.0
					for m := 0; m < 3; m++ {
						v += phi[a][m] * qd[m] * phi[b][m]
					}
					blk[a][b] += w * v * dt / steps / 3
				}
			}
		}
		for a, ia := range idx {
			for b, ib2 := range idx {
				Q[ia*n+ib2] = blk[a][b]
			}
		}
	}
	return F, Q
}

func TestPredictMatchesFPFtQ(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	for trial := 0; trial < 5; trial++ {
		c, _ := testComponent(r)
		n := c.n
		dt := 0.1 + r.Float64()*2
		F, Q := fullFQ(c, dt)
		want := matMul(matMul(F, c.P, n), transpose(F, n), n)
		for i := range want {
			want[i] += Q[i]
		}
		wantX := make([]float64, n)
		linalg.MatVec(wantX, F, c.x, n, n)
		c.predict(c.tE + dt)
		if d := maxRelDiff(c.P, want); d > 1e-9 {
			t.Errorf("P mismatch %.3g", d)
		}
		if d := maxRelDiff(c.x, wantX); d > 1e-12 {
			t.Errorf("x mismatch %.3g", d)
		}
	}
}

func TestConditionMatchesKalman(t *testing.T) {
	r := rand.New(rand.NewPCG(2, 2))
	c, sts := testComponent(r)
	n := c.n
	i := sts[0].off + int(sts[0].layout[kPhi])
	P0 := append([]float64(nil), c.P...)
	x0 := append([]float64(nil), c.x...)
	c.condition(i, 3)
	for a := 0; a < n; a++ {
		wantX := x0[a] + P0[a*n+i]/P0[i*n+i]*(3-x0[i])
		if math.Abs(c.x[a]-wantX) > 1e-9*math.Max(1, math.Abs(wantX)) {
			t.Errorf("x[%d] %g want %g", a, c.x[a], wantX)
		}
		for b := 0; b < n; b++ {
			want := P0[a*n+b] - P0[a*n+i]*P0[i*n+b]/P0[i*n+i]
			if a == i || b == i {
				want = 0
			}
			if math.Abs(c.P[a*n+b]-want) > 1e-9*math.Sqrt(P0[a*n+a]*P0[b*n+b]) {
				t.Errorf("P[%d,%d] %g want %g", a, b, c.P[a*n+b], want)
			}
		}
	}
}

// offsetLocal returns the local time read by a station's clock state at
// network time t (θ in ns), for regauge checks.
func offsetLocal(c *component, s *station, t float64) float64 {
	x := c.x[s.off : s.off+s.dim]
	dl := t - c.tE
	th := x[s.layout[kTheta]]
	if s.mode == modeFree {
		th += x[s.layout[kPhi]] * dl
		if s.psi {
			th += x[s.layout[kPsi]] * dl * dl / 2
		}
	}
	return t + th*1e-9
}

// TestRegauge checks the time-scale change u = t + Δs + r·(t − t_e): every
// clock reads the same local time at corresponding instants afterwards.
func TestRegauge(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 3))
	c, sts := testComponent(r)
	free := sts[:2]
	c.sts = free             // anchors only allow r = 0; test the free blocks
	ds, rr := 1.3e-3, 2500.0 // 1.3 ms, 2.5 ppm
	type pt struct{ t, y float64 }
	var before [2][]pt
	for k, s := range free {
		for _, dt := range []float64{-2, -0.5, 0, 0.7, 3} {
			tt := c.tE + dt
			before[k] = append(before[k], pt{tt, offsetLocal(c, s, tt)})
		}
	}
	c.regauge(ds, rr)
	for k, s := range free {
		for _, p := range before[k] {
			u := p.t + ds + rr*1e-9*(p.t-c.tE)
			y := offsetLocal(c, s, u)
			if d := (y - p.y) * 1e9; math.Abs(d) > 0.01 {
				t.Errorf("station %d: local time differs by %.4f ns after regauge (dt %.1f)", s.idx, d, p.t-c.tE)
			}
		}
	}
}

// TestNuisanceElimination checks the measurement weight of a group:
// recordAccepted's low-rank form D⁻¹ − Σ z zᵀ equals both the τ-eliminated
// weight R⁻¹ − R⁻¹a aᵀR⁻¹/(aᵀR⁻¹a) and, for a = 1, the differencing form
// Dᵀ(D R Dᵀ)⁻¹D of DESIGN §9.2.2 (R = diag + V Vᵀ, position covariance).
func TestNuisanceElimination(t *testing.T) {
	r := rand.New(rand.NewPCG(4, 4))
	f, _ := New(Config{OriginUnixNs: 1})
	for _, unitA := range []bool{true, false} {
		m := 6
		c := &component{}
		mm := make([]gm, m)
		act := make([]int, m)
		for i := range mm {
			act[i] = i
			mm[i] = gm{st: newStation(model.StationIdx(i + 2)), off: -1, d: 100 + r.Float64()*1e5, a: 1, e: r.NormFloat64() * 300}
			if !unitA {
				mm[i].a = 1 + r.NormFloat64()*1e-4
			}
			for k := 0; k < 3; k++ {
				mm[i].v[k] = r.NormFloat64() * 50
			}
		}
		f.recordAccepted(c, &c.gw, mm, act, 0, false)
		lr := make([]float64, m*m)
		for i := 0; i < m; i++ {
			for j := 0; j < m; j++ {
				v := 0.0
				if i == j {
					v = c.ar.mem[i].dinv
				}
				for k := 0; k < 4; k++ {
					v -= c.ar.mem[i].z[k] * c.ar.mem[j].z[k]
				}
				lr[i*m+j] = v
			}
		}
		R := make([]float64, m*m)
		for i := 0; i < m; i++ {
			for j := 0; j < m; j++ {
				R[i*m+j] = mm[i].v[0]*mm[j].v[0] + mm[i].v[1]*mm[j].v[1] + mm[i].v[2]*mm[j].v[2]
			}
			R[i*m+i] += mm[i].d
		}
		W := append([]float64(nil), R...)
		if err := linalg.SPDInverse(W, m, nil); err != nil {
			t.Fatal(err)
		}
		wa := make([]float64, m)
		ata := 0.0
		for i := 0; i < m; i++ {
			for j := 0; j < m; j++ {
				wa[i] += W[i*m+j] * mm[j].a
			}
			ata += mm[i].a * wa[i]
		}
		want := make([]float64, m*m)
		for i := 0; i < m; i++ {
			for j := 0; j < m; j++ {
				want[i*m+j] = W[i*m+j] - wa[i]*wa[j]/ata
			}
		}
		if d := maxRelDiff(lr, want); d > 1e-9 {
			t.Errorf("unitA=%v: low-rank weight differs from projected R⁻¹ by %.3g", unitA, d)
		}
		if unitA {
			// differencing against member 0: z = Dd·y, Dd = [-1 | I]
			k := m - 1
			Dd := make([]float64, k*m)
			for i := 0; i < k; i++ {
				Dd[i*m] = -1
				Dd[i*m+i+1] = 1
			}
			RD := make([]float64, k*k)
			for i := 0; i < k; i++ {
				for j := 0; j < k; j++ {
					v := 0.0
					for p := 0; p < m; p++ {
						for q := 0; q < m; q++ {
							v += Dd[i*m+p] * R[p*m+q] * Dd[j*m+q]
						}
					}
					RD[i*k+j] = v
				}
			}
			if err := linalg.SPDInverse(RD, k, nil); err != nil {
				t.Fatal(err)
			}
			diff := make([]float64, m*m)
			for p := 0; p < m; p++ {
				for q := 0; q < m; q++ {
					v := 0.0
					for i := 0; i < k; i++ {
						for j := 0; j < k; j++ {
							v += Dd[i*m+p] * RD[i*k+j] * Dd[j*m+q]
						}
					}
					diff[p*m+q] = v
				}
			}
			if d := maxRelDiff(lr, diff); d > 1e-9 {
				t.Errorf("τ elimination differs from differencing by %.3g", d)
			}
		}
	}
}

// TestInformationUpdate checks accumulate + update against the explicit
// information-form formulas with dense matrices.
func TestInformationUpdate(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 5))
	f, _ := New(Config{OriginUnixNs: 1})
	c, sts := testComponent(r)
	n := c.n
	// one group with the three stations + one external member
	m := 4
	mm := make([]gm, m)
	act := []int{0, 1, 2, 3}
	H := make([]float64, m*n)
	for i := 0; i < m; i++ {
		g := gm{off: -1, d: 1000 + r.Float64()*1e5, a: 1 + r.NormFloat64()*1e-5, e: r.NormFloat64() * 200}
		for k := 0; k < 3; k++ {
			g.v[k] = r.NormFloat64() * 30
		}
		if i < 3 {
			s := sts[i]
			g.st, g.off, g.dim = s, s.off, s.dim
			thetaInternal(c, s, -0.3*r.Float64(), i == 0, &g.h)
			for p := 0; p < s.dim; p++ {
				H[i*n+s.off+p] = g.h[p]
			}
		} else {
			g.st = newStation(9)
		}
		mm[i] = g
	}
	f.recordAccepted(c, &c.gw, mm, act, 3, false)
	c.ensure()
	clear(c.J)
	clear(c.g)
	f.accumulate(c)
	// explicit Wp
	R := make([]float64, m*m)
	for i := 0; i < m; i++ {
		for j := 0; j < m; j++ {
			R[i*m+j] = mm[i].v[0]*mm[j].v[0] + mm[i].v[1]*mm[j].v[1] + mm[i].v[2]*mm[j].v[2]
		}
		R[i*m+i] += mm[i].d
	}
	W := append([]float64(nil), R...)
	linalg.SPDInverse(W, m, nil)
	wa := make([]float64, m)
	ata := 0.0
	for i := 0; i < m; i++ {
		for j := 0; j < m; j++ {
			wa[i] += W[i*m+j] * mm[j].a
		}
		ata += mm[i].a * wa[i]
	}
	Wp := make([]float64, m*m)
	for i := 0; i < m; i++ {
		for j := 0; j < m; j++ {
			Wp[i*m+j] = W[i*m+j] - wa[i]*wa[j]/ata
		}
	}
	J := make([]float64, n*n)
	g := make([]float64, n)
	for a := 0; a < n; a++ {
		for i := 0; i < m; i++ {
			for j := 0; j < m; j++ {
				g[a] += H[i*n+a] * Wp[i*m+j] * mm[j].e
				for b := 0; b < n; b++ {
					J[a*n+b] += H[i*n+a] * Wp[i*m+j] * H[j*n+b]
				}
			}
		}
	}
	if d := maxRelDiff(c.J, J); d > 1e-9 {
		t.Errorf("J differs by %.3g", d)
	}
	if d := maxRelDiff(c.g, g); d > 1e-9 {
		t.Errorf("g differs by %.3g", d)
	}
	// explicit update
	Pi := append([]float64(nil), c.P...)
	linalg.SPDInverse(Pi, n, nil)
	for i := range Pi {
		Pi[i] += J[i]
	}
	linalg.SPDInverse(Pi, n, nil)
	dx := make([]float64, n)
	linalg.MatVec(dx, Pi, g, n, n)
	x0 := append([]float64(nil), c.x...)
	if !c.update() {
		t.Fatal("update failed")
	}
	if d := maxRelDiff(c.P, Pi); d > 1e-8 {
		t.Errorf("P⁺ differs by %.3g", d)
	}
	for i := range dx {
		if math.Abs(c.x[i]-x0[i]-dx[i]) > 1e-8*math.Max(1, math.Abs(dx[i])) {
			t.Errorf("x⁺[%d] differs: %g vs %g", i, c.x[i]-x0[i], dx[i])
		}
	}
}

func TestChi2Quantile(t *testing.T) {
	for _, c := range []struct {
		k    int
		want float64
	}{{1, 10.828}, {5, 20.515}, {10, 29.588}, {20, 45.315}, {30, 59.703}, {50, 86.661}} {
		got := chi2Quantile(0.999, c.k)
		if math.Abs(got-c.want)/c.want > 0.01 {
			t.Errorf("χ²(%d) = %.3f, want %.3f", c.k, got, c.want)
		}
	}
	if q := normQuantile(0.975); math.Abs(q-1.959964) > 1e-6 {
		t.Errorf("z(0.975) = %v", q)
	}
}

func TestBisect(t *testing.T) {
	r := rand.New(rand.NewPCG(6, 6))
	var sts []*station
	for i := 0; i < 437; i++ {
		s := newStation(model.StationIdx(i + 2))
		s.info.Pos = geo.ToECEF(48+r.Float64()*6, 5+r.Float64()*10, 100)
		sts = append(sts, s)
	}
	parts := bisect(sts, 100)
	seen := map[model.StationIdx]bool{}
	for _, p := range parts {
		if len(p) > 100 || len(p) < 50 {
			t.Errorf("part of %d stations", len(p))
		}
		for _, s := range p {
			if seen[s.idx] {
				t.Errorf("station %d twice", s.idx)
			}
			seen[s.idx] = true
		}
	}
	if len(seen) != len(sts) || len(parts) != 5 {
		t.Errorf("%d stations in %d parts", len(seen), len(parts))
	}
}

func TestSyncDF(t *testing.T) {
	for df := uint8(0); df < 32; df++ {
		want := df == 0 || df == 4 || df == 5 || df == 11 || df == 17 || df == 18
		if SyncDF(df) != want {
			t.Errorf("SyncDF(%d)", df)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("missing origin accepted")
	}
	f, err := New(Config{OriginUnixNs: 1})
	if err != nil {
		t.Fatal(err)
	}
	c := f.Config()
	if c.EpochS != 0.5 || c.MaxComponentStations != 200 || c.ProbationS != 600 || c.StepCount != 5 || c.ResetThetaSigmaNs != 1e6 {
		t.Errorf("defaults: %+v", c)
	}
	if DefaultFrameRefNs(model.ClockGPSUTC, model.FrameModeSLong) != 120000 || DefaultFrameRefNs(model.ClockSDR12, model.FrameModeSShort) != 0 {
		t.Error("frame reference table")
	}
	if !priorFor(model.ClockSDR12).psi || priorFor(model.ClockHW12).psi {
		t.Error("ψ states")
	}
}
