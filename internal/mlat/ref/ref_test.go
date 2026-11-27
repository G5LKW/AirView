package ref

import (
	"math"
	"math/rand/v2"
	"testing"
	"unsafe"

	"airview/internal/latency"
	"airview/internal/mlat/geo"
	"airview/internal/modes"
)

// invDense inverts a dense square matrix by Gauss-Jordan elimination with
// partial pivoting (test reference only).
func invDense(a [][]float64) [][]float64 {
	n := len(a)
	m := make([][]float64, n)
	for i := range a {
		m[i] = make([]float64, 2*n)
		copy(m[i], a[i])
		m[i][n+i] = 1
	}
	for c := 0; c < n; c++ {
		p := c
		for r := c + 1; r < n; r++ {
			if math.Abs(m[r][c]) > math.Abs(m[p][c]) {
				p = r
			}
		}
		m[c], m[p] = m[p], m[c]
		d := m[c][c]
		for j := range m[c] {
			m[c][j] /= d
		}
		for r := 0; r < n; r++ {
			if r != c && m[r][c] != 0 {
				f := m[r][c]
				for j := range m[r] {
					m[r][j] -= f * m[c][j]
				}
			}
		}
	}
	out := make([][]float64, n)
	for i := range out {
		out[i] = m[i][n:]
	}
	return out
}

func fMat(dt float64) [3][3]float64 {
	return [3][3]float64{{1, dt, dt * dt / 2}, {0, 1, dt}, {0, 0, 1}}
}

func qMat(dt, q float64) [3][3]float64 {
	p := propagateP([6]float64{}, dt, q)
	return full(&p)
}

func inv3(m [3][3]float64) [3][3]float64 {
	a := [][]float64{m[0][:], m[1][:], m[2][:]}
	r := invDense(a)
	return [3][3]float64{{r[0][0], r[0][1], r[0][2]}, {r[1][0], r[1][1], r[1][2]}, {r[2][0], r[2][1], r[2][2]}}
}

type scalarMeas struct {
	comp int
	z, r float64
}

// batchSmooth solves the joint Gaussian of the states at times ts (sorted)
// given a prior on the first state and scalar measurements per time, and
// returns the posterior mean and covariance at index qi.
func batchSmooth(ts []float64, prior axis, q float64, ms [][]scalarMeas, qi int) ([3]float64, [3][3]float64) {
	// the model is translation invariant in position: solve about the
	// prior mean to avoid cancellation in the dense reference
	off := prior.x[0]
	prior.x[0] = 0
	n := len(ts)
	N := 3 * n
	L := make([][]float64, N)
	for i := range L {
		L[i] = make([]float64, N)
	}
	eta := make([]float64, N)
	add := func(i, j int, m [3][3]float64) {
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				L[3*i+a][3*j+b] += m[a][b]
			}
		}
	}
	p0 := inv3(full(&prior.p))
	add(0, 0, p0)
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			eta[a] += p0[a][b] * prior.x[b]
		}
	}
	for k := 0; k+1 < n; k++ {
		dt := ts[k+1] - ts[k]
		F := fMat(dt)
		Qi := inv3(qMat(dt, q))
		// (x1 − F x0)ᵀ Qi (x1 − F x0)
		var FtQi, FtQiF [3][3]float64
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				for c := 0; c < 3; c++ {
					FtQi[a][b] += F[c][a] * Qi[c][b]
				}
			}
		}
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				for c := 0; c < 3; c++ {
					FtQiF[a][b] += FtQi[a][c] * F[c][b]
				}
			}
		}
		add(k, k, FtQiF)
		add(k+1, k+1, Qi)
		var neg, negT [3][3]float64
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				neg[a][b] = -FtQi[a][b]
				negT[b][a] = -FtQi[a][b]
			}
		}
		add(k, k+1, neg)
		add(k+1, k, negT)
	}
	for k, list := range ms {
		for _, m := range list {
			L[3*k+m.comp][3*k+m.comp] += 1 / m.r
			z := m.z
			if m.comp == 0 {
				z -= off
			}
			eta[3*k+m.comp] += z / m.r
		}
	}
	P := invDense(L)
	var mean [3]float64
	var cov [3][3]float64
	for a := 0; a < 3; a++ {
		for j := 0; j < N; j++ {
			mean[a] += P[3*qi+a][j] * eta[j]
		}
		for b := 0; b < 3; b++ {
			cov[a][b] = P[3*qi+a][3*qi+b]
		}
	}
	mean[0] += off
	return mean, cov
}

// TestSmootherMatchesBatch proves the filter + lazy RTS smoother of a track
// (including the pseudo-node interpolation between two measurements and
// the forward prediction after the newest) equals the exact batch
// posterior of the same linear-Gaussian model.
func TestSmootherMatchesBatch(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	o := Options{}.withDefaults()
	var worst [4]float64
	defer func() {
		t.Logf("largest |smoother − batch| / σ per query kind (mid-first, at-node, mid, after): %.2g", worst)
	}()
	for trial := 0; trial < 40; trial++ {
		tr := &track{nodes: make([]node, 16)}
		tr.setFrame(50, 8)
		tt := 0.0
		for i := 0; i < 10; i++ {
			tt += 0.1 + rng.Float64()*0.6
			nd := node{t: tt}
			if i == 0 || rng.Float64() < 0.5 {
				nd.kind = nPosH | nPosV
				nd.lat = 50 + rng.Float64()*0.01
				nd.lon = 8 + rng.Float64()*0.01
				nd.h = 9000 + rng.Float64()*100
				nd.re, nd.rn, nd.rh = 1+rng.Float64()*4, 1+rng.Float64()*4, 5+rng.Float64()*10
			} else {
				nd.kind = nVelH | nVelV
				nd.lat, nd.lon, nd.h = 50.005, 8.005, 9000
				nd.ve, nd.vn, nd.vu = 200+rng.NormFloat64()*5, -50+rng.NormFloat64()*5, rng.NormFloat64()
				nd.rv, nd.rvu = 0.1+rng.Float64(), 0.2
			}
			tr.insert(nd, o.QHorizontal, o.QVertical)
		}
		// query instants: between nodes, at a node, after the newest
		// a moderate prior keeps the dense batch reference well conditioned
		pr := tr.prior(0)
		for a := range pr {
			pr[a].p = [6]float64{100, 0, 0, 400, 0, 4}
		}
		tr.refilterWith(pr, o.QHorizontal, o.QVertical)
		qs := []float64{(tr.at(0).t + tr.at(1).t) / 2, tr.at(4).t, (tr.at(5).t + tr.at(6).t) / 2, tr.last().t + 0.3}
		for qk, qt := range qs {
			got, _ := tr.smoothedAt(qt, o.QHorizontal, o.QVertical)
			for a := 0; a < 3; a++ {
				qa := o.QHorizontal
				if a == 2 {
					qa = o.QVertical
				}
				var ts []float64
				var ms [][]scalarMeas
				qi := -1
				for i := 0; i < tr.n; i++ {
					nd := tr.at(i)
					if qi < 0 && qt < nd.t {
						qi = len(ts)
						ts = append(ts, qt)
						ms = append(ms, nil)
					}
					m := tr.meas(nd)
					var l []scalarMeas
					switch a {
					case 0:
						if nd.kind&nPosH != 0 {
							l = append(l, scalarMeas{0, m.x, m.rx})
						}
						if nd.kind&nVelH != 0 {
							l = append(l, scalarMeas{1, m.vx, m.rvx})
						}
					case 1:
						if nd.kind&nPosH != 0 {
							l = append(l, scalarMeas{0, m.y, m.ry})
						}
						if nd.kind&nVelH != 0 {
							l = append(l, scalarMeas{1, m.vy, m.rvy})
						}
					case 2:
						if nd.kind&nPosV != 0 {
							l = append(l, scalarMeas{0, m.z, m.rz})
						}
						if nd.kind&nVelV != 0 {
							l = append(l, scalarMeas{1, m.vz, m.rvz})
						}
					}
					if qi < 0 && qt == nd.t {
						qi = len(ts)
					}
					ts = append(ts, nd.t)
					ms = append(ms, l)
				}
				if qi < 0 {
					qi = len(ts)
					ts = append(ts, qt)
					ms = append(ms, nil)
				}
				mean, cov := batchSmooth(ts, pr[a], qa, ms, qi)
				g := got[a]
				gc := full(&g.p)
				for c := 0; c < 3; c++ {
					// equal to 1e-4 of the posterior σ (measured: ≤ 2e-5 σ smoothed,
					// ≤ 1e-6 σ forward; an algebra error shows at ≥ 1e-2 σ)
					tol := 1e-4*math.Sqrt(cov[c][c]) + 1e-12*math.Abs(mean[c])
					if e := math.Abs(g.x[c]-mean[c]) / math.Sqrt(cov[c][c]); e > worst[qk] {
						worst[qk] = e
					}
					if math.Abs(g.x[c]-mean[c]) > tol {
						t.Fatalf("trial %d axis %d query %.3f comp %d: smoother %.9g batch %.9g", trial, a, qt, c, g.x[c], mean[c])
					}
					for d := 0; d < 3; d++ {
						if math.Abs(gc[c][d]-cov[c][d]) > 1e-4*math.Sqrt(cov[c][c]*cov[d][d]) {
							t.Fatalf("trial %d axis %d query %.3f cov[%d][%d]: smoother %.9g batch %.9g", trial, a, qt, c, d, gc[c][d], cov[c][d])
						}
					}
				}
			}
		}
	}
}

// TestOutOfOrderInsertion: the filtered/smoothed result does not depend on
// the arrival order of the measurements.
func TestOutOfOrderInsertion(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	o := Options{}.withDefaults()
	var nodes []node
	tt := 0.0
	for i := 0; i < 12; i++ {
		tt += 0.1 + rng.Float64()*0.4
		nd := node{t: tt, kind: nPosH | nPosV, lat: 50 + 0.0001*float64(i), lon: 8 + 0.0003*float64(i), h: 9000, re: 2, rn: 2, rh: 9}
		if i%2 == 1 {
			nd = node{t: tt, kind: nVelH, lat: 50, lon: 8, h: 9000, ve: 200, vn: 10, rv: 0.2}
		}
		nodes = append(nodes, nd)
	}
	a := &track{nodes: make([]node, 16)}
	a.setFrame(50, 8)
	for _, nd := range nodes {
		a.insert(nd, o.QHorizontal, o.QVertical)
	}
	b := &track{nodes: make([]node, 16)}
	b.setFrame(50, 8)
	perm := []int{0, 2, 1, 3, 5, 4, 6, 9, 7, 8, 11, 10}
	for _, i := range perm {
		b.insert(nodes[i], o.QHorizontal, o.QVertical)
	}
	for _, qt := range []float64{nodes[3].t + 0.05, nodes[10].t, tt + 0.2} {
		sa, _ := a.smoothedAt(qt, o.QHorizontal, o.QVertical)
		sb, _ := b.smoothedAt(qt, o.QHorizontal, o.QVertical)
		for ax := 0; ax < 3; ax++ {
			for c := 0; c < 3; c++ {
				if d := math.Abs(sa[ax].x[c] - sb[ax].x[c]); d > 1e-7*(1+math.Abs(sa[ax].x[c])) {
					t.Fatalf("order dependence at %.2f axis %d comp %d: %g vs %g", qt, ax, c, sa[ax].x[c], sb[ax].x[c])
				}
			}
		}
	}
}

// TestRecenterExact: moving the kinematic frame origin does not change the
// geodetic answer.
func TestRecenterExact(t *testing.T) {
	o := Options{}.withDefaults()
	tr := &track{nodes: make([]node, 16)}
	tr.setFrame(50, 8)
	for i := 0; i < 8; i++ {
		tr.insert(node{t: float64(i) * 0.5, kind: nPosH | nPosV, lat: 50.3 + 0.002*float64(i), lon: 8.9 + 0.003*float64(i), h: 9000, re: 2, rn: 2, rh: 9}, o.QHorizontal, o.QVertical)
	}
	s1, _ := tr.smoothedAt(2.2, o.QHorizontal, o.QVertical)
	lat1, lon1 := tr.fromXY(s1[0].x[0], s1[1].x[0])
	tr.recenter(51.4, 10.2)
	s2, _ := tr.smoothedAt(2.2, o.QHorizontal, o.QVertical)
	lat2, lon2 := tr.fromXY(s2[0].x[0], s2[1].x[0])
	if d := surfDist(lat1, lon1, lat2, lon2); d > 1e-6 {
		t.Fatalf("recenter moved the answer by %g m", d)
	}
	sx1, _ := (&track{kx: 1, ky: 1}).scales(0, 0)
	_ = sx1
}

// TestNearestEpochMatchesLatency: the T=1 epoch rule is the one of
// internal/latency.
func TestNearestEpochMatchesLatency(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	for i := 0; i < 100000; i++ {
		ns := 1_700_000_000_000_000_000 + rng.Int64N(1e13)
		for _, odd := range []bool{false, true} {
			if a, b := nearestEpoch(ns, odd), latency.NearestEpoch(ns, odd); a != b {
				t.Fatalf("%d odd=%v: %d vs %d", ns, odd, a, b)
			}
		}
	}
}

func TestCPRSigma(t *testing.T) {
	se, sn := cprSigma(50, false)
	// 6°/2^17 of latitude ≈ 5.09 m → σ ≈ 1.47 m
	if math.Abs(sn-1.47) > 0.02 || se < 1 || se > 2 {
		t.Fatalf("cpr sigma %.3f %.3f", se, sn)
	}
}

// TestSyntheticTurn measures interpolation through a 3°/s coordinated turn
// (the simnet traffic is mostly straight): every 0.1 s instant compared
// with the analytic truth, 0.4 s after the instant.
func TestSyntheticTurn(t *testing.T) {
	for _, omegaDeg := range []float64{0, 1.5, 3} {
		s := New(Options{}, nil)
		fd := newFeeder(s, 0x3c0001, 50, 8, 230, 80)
		fd.k.turnA, fd.k.turnB, fd.k.omega = 40, 100, omegaDeg*math.Pi/180
		var hs []float64
		var turnH []float64
		for t0 := 0.0; t0 < 140; t0 += 1 {
			fd.run(t0, t0+1)
			// evaluate instants that are 0.4 s old
			for q := t0 + 1 - 0.5; q < t0+1-0.4-1e-9; q += 0.1 {
				if q < 20 {
					continue
				}
				var st State
				if !s.At(0x3c0001, fd.ns(q), 0, &st) {
					t.Fatal("no state")
				}
				p, llh, _, _, _ := fd.k.truth(q)
				f := geo.NewFrame(llh)
				d := f.Rotate(st.Pos.Sub(p))
				h := math.Hypot(d.X, d.Y)
				hs = append(hs, h)
				if q > fd.k.turnA && q < fd.k.turnB+5 {
					turnH = append(turnH, h)
				}
				if !st.Usable() {
					t.Fatalf("unusable state at %.1f: %v", q, st.Flags)
				}
			}
		}
		t.Logf("turn %.1f°/s: all P50 %.2f P95 %.2f max %.2f m; in/after turn P95 %.2f max %.2f m",
			omegaDeg, pct(hs, 50), pct(hs, 95), pct(hs, 100), pct(turnH, 95), pct(turnH, 100))
		if pct(hs, 95) > 5 || pct(turnH, 95) > 5 {
			t.Errorf("turn %.1f°/s: P95 %.2f / %.2f m > 5 m", omegaDeg, pct(hs, 95), pct(turnH, 95))
		}
	}
}

// TestJumpRejected: a reference whose positions suddenly move 20 km
// (spoofing, wrong decode) is rejected, then re-initialised on probation;
// a second jump marks it unstable and suspends it.
func TestJumpRejected(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c0002, 50, 8, 230, 90)
	fd.run(0, 30)
	var st State
	if !s.At(0x3c0002, fd.ns(29.5), 0, &st) || !st.Usable() {
		t.Fatalf("not usable before the jump: %v", st.Flags)
	}
	off := 20000.0
	fd.posFn = func(t float64) (float64, float64) {
		_, llh, _, _, _ := fd.k.truth(t)
		return llh.Lat, llh.Lon + off/(111320*math.Cos(llh.Lat*math.Pi/180))
	}
	res := fd.run(30, 33)
	if res[0] != Rejected || res[1] != Rejected {
		t.Fatalf("jump not rejected: %v", res)
	}
	jumped := false
	for _, r := range res {
		jumped = jumped || r == Jumped
	}
	if !jumped {
		t.Fatalf("consistent new position not adopted: %v", res)
	}
	if !s.At(0x3c0002, fd.ns(33), 0, &st) || st.Usable() || st.Flags&FlagProbation == 0 {
		t.Fatalf("expected probation after the jump: %v", st.Flags)
	}
	fd.run(33, 70)
	if !s.At(0x3c0002, fd.ns(69.5), 0, &st) || !st.Usable() {
		t.Fatalf("not usable after probation: %v", st.Flags)
	}
	fd.posFn = nil // jumps back: second jump → unstable, suspended
	fd.run(70, 75)
	if !s.At(0x3c0002, fd.ns(74.5), 0, &st) || st.Flags&(FlagUnstable|FlagSuspended) != FlagUnstable|FlagSuspended {
		t.Fatalf("second jump did not mark unstable: %v", st.Flags)
	}
	fd.run(75, 300)
	if !s.At(0x3c0002, fd.ns(299.5), 0, &st) || st.Usable() {
		t.Fatalf("suspension lifted too early: %v", st.Flags)
	}
	t.Logf("info: %+v", must(s.Info(0x3c0002)))
}

func must[T any](v T, ok bool) T {
	if !ok {
		panic("not found")
	}
	return v
}

// TestDuplicateAddress: two aircraft 30 km apart with the same address
// alternate their positions; the track must not be usable.
func TestDuplicateAddress(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c0003, 50, 8, 230, 90)
	fd.run(0, 20)
	n := 0
	fd.posFn = func(t float64) (float64, float64) {
		_, llh, _, _, _ := fd.k.truth(t)
		n++
		if n%2 == 0 {
			return llh.Lat + 0.27, llh.Lon
		}
		return llh.Lat, llh.Lon
	}
	fd.run(20, 40)
	var st State
	if !s.At(0x3c0003, fd.ns(39.5), 0, &st) || st.Usable() || st.Flags&FlagUnstable == 0 {
		t.Fatalf("duplicate address not detected: %v", st.Flags)
	}
}

// TestVelocityInconsistent: reported velocities that contradict the
// positions (a naive spoofer) make the reference unusable.
func TestVelocityInconsistent(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c0004, 50, 8, 230, 90)
	fd.run(0, 20)
	fd.velFn = func(float64) (float64, float64) { return 0, 230 } // claims north, flies east
	fd.run(20, 25)
	var st State
	if !s.At(0x3c0004, fd.ns(24.5), 0, &st) || st.Usable() || st.Flags&FlagVelInconsistent == 0 {
		t.Fatalf("velocity inconsistency not flagged: %v", st.Flags)
	}
}

// TestLowQualityGate: NACp 7 references are tracked but not usable.
func TestLowQualityGate(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c0005, 50, 8, 230, 90)
	fd.nacp = 7
	fd.run(0, 20)
	var st State
	if !s.At(0x3c0005, fd.ns(19.5), 0, &st) || st.Usable() || st.Flags&FlagLowQuality == 0 {
		t.Fatalf("NACp 7 usable: %v", st.Flags)
	}
	if st.NACp != 7 || st.SigmaBiasH < 70 {
		t.Fatalf("NACp 7 bias σ %.1f", st.SigmaBiasH)
	}
}

// TestStaleAndExtrapolation: instants far from any position are flagged.
func TestStaleAndExtrapolation(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c0006, 50, 8, 230, 90)
	fd.run(0, 20)
	var st State
	if !s.At(0x3c0006, fd.ns(21.8), 0, &st) || st.Flags&(FlagStale|FlagExtrapolated) != FlagStale|FlagExtrapolated || st.Usable() {
		t.Fatalf("expected stale extrapolation: %v age %.2f", st.Flags, st.PosAgeS)
	}
	if s.At(0x3c0006, fd.ns(40), 0, &st) {
		t.Fatal("answered 20 s past the newest report")
	}
}

// TestT1Untrusted: a T=1 transmitter that sends 300 ms after its epochs
// is demoted to T=0 tagging.
func TestT1Untrusted(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c0007, 50, 8, 230, 90)
	fd.t1 = true
	// t0 on an epoch + 300 ms: every position at k·0.5 s is 300 ms (or
	// 100 ms) away from the nearest epoch of its parity
	fd.t0ns = 1_700_000_000_000_000_000 + 300_000_000
	fd.run(0, 20)
	var st State
	if !s.At(0x3c0007, fd.ns(19.5), 0, &st) || st.Flags&FlagT1Untrusted == 0 || st.Flags&FlagT1 != 0 {
		t.Fatalf("T=1 fault not detected: %v", st.Flags)
	}
}

// TestT1Tagging: a compliant T=1 transmitter's positions are tagged with
// their epoch: positions encoded for the epoch and sent 120 ms before it
// interpolate exactly.
func TestT1Tagging(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c0008, 50, 8, 230, 90)
	fd.t1 = true
	fd.t0ns = 1_700_000_000_000_000_000 // an even epoch
	for k := 0; k < 160; k++ {
		e := float64(k) * 0.2 // epoch k has parity k&1
		if k%2 == 0 {
			_, _, ve, vn, vu := fd.k.truth(e)
			fd.obs(velFrame(fd.icao, ve, vn, vu, 0, 2), e+0.05)
		}
		if k%20 == 0 {
			fd.obs(opStatusFrame(fd.icao, 9), e+0.01)
		}
		if k%2 == 1 || k%4 == 0 {
			_, llh, _, _, _ := fd.k.truth(e)
			fd.obs(posFrame(fd.icao, 11, llh.H/ftM, true, k&1 == 1, llh.Lat, llh.Lon), e-0.12)
		}
	}
	var st State
	q := 25.33
	if !s.At(fd.icao, fd.ns(q), 0, &st) || st.Flags&FlagT1 == 0 {
		t.Fatalf("no T=1 state: %v", st.Flags)
	}
	p, llh, _, _, _ := fd.k.truth(q)
	f := geo.NewFrame(llh)
	d := f.Rotate(st.Pos.Sub(p))
	if h := math.Hypot(d.X, d.Y); h > 3 {
		t.Fatalf("T=1 error %.2f m", h)
	}
}

// TestCheckHeard: a position far below a receiver's radio horizon is
// implausible.
func TestCheckHeard(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c0009, 50, 8, 230, 90)
	fd.k.h = 300 // low aircraft
	fd.run(0, 20)
	near := geo.ToECEF(50, 8.1, 100)
	far := geo.ToECEF(50, 16, 100) // ~570 km away
	if !s.CheckHeard(fd.icao, fd.ns(19.5), []geo.Vec3{near}) {
		t.Fatal("near receiver rejected")
	}
	if s.CheckHeard(fd.icao, fd.ns(19.5), []geo.Vec3{near, far}) {
		t.Fatal("receiver 570 km away accepted for a 300 m aircraft")
	}
	var st State
	if !s.At(fd.icao, fd.ns(19.5), 0, &st) || st.Flags&FlagBeyondHorizon == 0 || st.Usable() {
		t.Fatalf("beyond-horizon flag missing: %v", st.Flags)
	}
}

// TestSuspend: the external blacklist hook.
func TestSuspend(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c000a, 50, 8, 230, 90)
	fd.run(0, 20)
	s.Suspend(fd.icao, fd.ns(600))
	var st State
	if !s.At(fd.icao, fd.ns(19.5), 0, &st) || st.Usable() || st.Flags&FlagSuspended == 0 {
		t.Fatalf("suspend ignored: %v", st.Flags)
	}
	s.Suspend(fd.icao, 0)
	if !s.At(fd.icao, fd.ns(19.5), 0, &st) || !st.Usable() {
		t.Fatalf("suspend not lifted: %v", st.Flags)
	}
}

// TestTransmitInstant: τ recovered from arrival times at four receivers.
func TestTransmitInstant(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c000b, 50, 8, 230, 90)
	fd.run(0, 20)
	tau := 19.3
	p, _, _, _, _ := fd.k.truth(tau)
	rx := []geo.Vec3{geo.ToECEF(50.5, 8, 100), geo.ToECEF(49.6, 8.4, 200), geo.ToECEF(50.1, 9.2, 50), geo.ToECEF(49.9, 7.1, 300)}
	atm := geo.DefaultAtmosphere()
	base := fd.ns(19)
	var arr []Arrival
	for _, r := range rx {
		arr = append(arr, Arrival{Station: r, DtNs: (tau-19)*1e9 + atm.PropagationNs(r, p), SigmaNs: 50})
	}
	var st State
	got, sig, ok := s.TransmitInstant(fd.icao, base, arr, &atm, &st)
	if !ok {
		t.Fatal("no estimate")
	}
	if d := got - (tau-19)*1e9; math.Abs(d) > 30 {
		t.Fatalf("τ error %.1f ns (σ %.1f)", d, sig)
	}
	if d := st.Pos.Dist(p); d > 5 {
		t.Fatalf("position at τ off by %.1f m", d)
	}
}

func TestResultString(t *testing.T) {
	if Jumped.String() != "jumped" || Result(200).String() != "?" {
		t.Fatal(Jumped.String())
	}
	if (FlagT1 | FlagStale).String() != "t1|stale" {
		t.Fatal((FlagT1 | FlagStale).String())
	}
}

// TestExpire drops idle tracks.
func TestExpire(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3c000c, 50, 8, 230, 90)
	fd.run(0, 10)
	if s.Expire(fd.ns(60)) != 0 || !s.Has(fd.icao) {
		t.Fatal("expired too early")
	}
	if s.Expire(fd.ns(200)) != 1 || s.Has(fd.icao) {
		t.Fatal("not expired")
	}
}

// TestConcurrent: Observe and At from many goroutines (shard locking).
func TestConcurrent(t *testing.T) {
	s := New(Options{}, nil)
	msgs := benchStream(0x3c0000, 20)
	done := make(chan bool)
	for g := 0; g < 8; g++ {
		go func(g int) {
			for a := 0; a < 20; a++ {
				key := uint32(0x500000 + g*100 + a)
				var st State
				for i := range msgs {
					m := msgs[i].m
					m.ICAO = key
					s.Observe(&m, msgs[i].ns, 0)
					s.At(key, msgs[i].ns-300_000_000, 0, &st)
					s.At(uint32(0x500000+((g+1)%8)*100+a), msgs[i].ns, 0, &st)
				}
			}
			done <- true
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	if st := s.Stats(); st.Tracks != 160 {
		t.Fatalf("tracks %d", st.Tracks)
	}
}

// TestFootprint documents the memory per track (DESIGN §20: 10k aircraft).
func TestFootprint(t *testing.T) {
	o := Options{}.withDefaults()
	per := int(unsafe.Sizeof(track{})) + o.Window*int(unsafe.Sizeof(node{}))
	s := New(Options{}, nil)
	var m modes.Message
	if err := modes.Decode(opStatusFrame(0x3c0d00, 9), &m); err != nil {
		t.Fatal(err)
	}
	if got := testing.AllocsPerRun(1, func() { s.Observe(&m, 1, 0) }); got > 2 {
		t.Fatalf("status-only aircraft allocates %v times", got)
	}
	t.Logf("node %d B, track %d B + %d nodes = %d B per aircraft; 10k aircraft ≈ %.0f MB",
		unsafe.Sizeof(node{}), unsafe.Sizeof(track{}), o.Window, per, float64(per)*1e4/1e6)
	if per > 8000 {
		t.Fatalf("track footprint %d B", per)
	}
}
