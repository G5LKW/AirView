package clock

import (
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/linalg"
)

// TestExternalMarginalization checks accumulate + marginalize against the
// explicit augmented information form: the external members' shared
// prediction errors are states [x; δθ_e] with prior N(0, v_e), the groups'
// τ-eliminated weights Wp are accumulated over the augmented H, and the
// external states are removed by the Schur complement. One external is
// shared by both groups (one state, not two independent errors); one
// exceeds the slot cap and keeps its variance in D.
func TestExternalMarginalization(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	f, _ := New(Config{OriginUnixNs: 1, ExternalStates: 2})
	c, sts := testComponent(r)
	n := c.n
	extA, extB, extC := newStation(20), newStation(21), newStation(22)
	type mem struct {
		st  *station
		int bool
	}
	groups := [][]mem{
		{{sts[0], true}, {sts[1], true}, {extA, false}, {extC, false}},
		{{sts[1], true}, {sts[2], true}, {extA, false}, {extB, false}},
	}
	c.xs.reset(n, f.cfg.ExternalStates)
	type rec struct {
		mm []gm
		H  []float64 // m × (n+2)
	}
	const N2 = 2
	na := n + N2
	var recs []rec
	for _, grp := range groups {
		m := len(grp)
		mm := make([]gm, m)
		act := make([]int, m)
		for i, x := range grp {
			act[i] = i
			g := gm{off: -1, st: x.st, d: 1000 + r.Float64()*1e5, a: 1 + r.NormFloat64()*1e-5, e: r.NormFloat64() * 200}
			for k := 0; k < 3; k++ {
				g.v[k] = r.NormFloat64() * 30
			}
			if x.int {
				g.off, g.dim = x.st.off, x.st.dim
				thetaInternal(c, x.st, -0.3*r.Float64(), false, &g.h)
			} else {
				g.vext = g.d * (0.2 + 0.6*r.Float64())
			}
			mm[i] = g
		}
		f.recordAccepted(c, &c.gw, mm, act, 0, false)
		H := make([]float64, m*na)
		for i, x := range grp {
			g := &mm[i]
			switch {
			case x.int:
				for p := 0; p < g.dim; p++ {
					H[i*na+g.off+p] = g.h[p]
				}
			default:
				if sl, ok := c.xs.slot[extKey{x.st, false}]; ok {
					H[i*na+n+int(sl)] = 1
				}
			}
		}
		recs = append(recs, rec{mm, H})
	}
	if c.xs.k != 2 {
		t.Fatalf("slots %d, want 2", c.xs.k)
	}
	if _, ok := c.xs.slot[extKey{extB, false}]; ok {
		t.Fatal("extB got a slot beyond the cap")
	}
	// explicit augmented information
	Ja := make([]float64, na*na)
	ga := make([]float64, na)
	for _, rc := range recs {
		mm := rc.mm
		m := len(mm)
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
		for a := 0; a < na; a++ {
			for i := 0; i < m; i++ {
				for j := 0; j < m; j++ {
					wp := W[i*m+j] - wa[i]*wa[j]/ata
					ga[a] += rc.H[i*na+a] * wp * mm[j].e
					for b := 0; b < na; b++ {
						Ja[a*na+b] += rc.H[i*na+a] * wp * rc.H[j*na+b]
					}
				}
			}
		}
	}
	for e := 0; e < N2; e++ {
		Ja[(n+e)*na+n+e] += 1 / c.xs.vr[e]
	}
	// Schur complement onto x
	A := []float64{Ja[n*na+n], Ja[n*na+n+1], Ja[(n+1)*na+n], Ja[(n+1)*na+n+1]}
	if err := linalg.SPDInverse(A, 2, nil); err != nil {
		t.Fatal(err)
	}
	J := make([]float64, n*n)
	g := make([]float64, n)
	for i := 0; i < n; i++ {
		g[i] = ga[i]
		for p := 0; p < 2; p++ {
			for q := 0; q < 2; q++ {
				g[i] -= Ja[i*na+n+p] * A[p*2+q] * ga[n+q]
			}
		}
		for j := 0; j < n; j++ {
			J[i*n+j] = Ja[i*na+j]
			for p := 0; p < 2; p++ {
				for q := 0; q < 2; q++ {
					J[i*n+j] -= Ja[i*na+n+p] * A[p*2+q] * Ja[(n+q)*na+j]
				}
			}
		}
	}
	c.ensure()
	clear(c.J)
	clear(c.g)
	f.accumulate(c)
	if !c.marginalize() {
		t.Fatal("marginalize failed")
	}
	if d := maxRelDiff(c.J, J); d > 1e-9 {
		t.Errorf("J differs by %.3g", d)
	}
	if d := maxRelDiff(c.g, g); d > 1e-9 {
		t.Errorf("g differs by %.3g", d)
	}
	// the block is cleared for the next epoch
	for _, v := range c.xs.jxe {
		if v != 0 {
			t.Fatal("jxe not cleared")
		}
	}
	for _, v := range c.xs.jee[:4] {
		if v != 0 {
			t.Fatal("jee not cleared")
		}
	}
	c.xs.reset(n, f.cfg.ExternalStates)
	if c.xs.k != 0 || len(c.xs.rows) != 0 {
		t.Fatal("reset left slots")
	}
}

// TestExternalStatesConfig: 0 means the default, negative switches the
// epoch-local external states off.
func TestExternalStatesConfig(t *testing.T) {
	for _, c := range []struct{ in, want int }{{0, 64}, {-1, 0}, {16, 16}} {
		f, err := New(Config{OriginUnixNs: 1, ExternalStates: c.in})
		if err != nil {
			t.Fatal(err)
		}
		if got := f.Config().ExternalStates; got != c.want {
			t.Errorf("ExternalStates %d -> %d, want %d", c.in, got, c.want)
		}
	}
}
