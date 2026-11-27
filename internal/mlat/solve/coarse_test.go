package solve

import (
	"math"
	"testing"

	"airview/internal/mlat/linalg"
)

// TestCoarsePhaseMatchesExact checks the H-track solver speed-ups against
// the exact-only path: the coarse straight-path excess model is only a
// travel aid (the final estimate is the minimiser of the exact model), the
// seed mirroring and the skipped grid only avoid wasted starts. Over every
// benchmark shape (64 geometries each) the accepted fixes must be the same
// set and agree to 0.1σ (Mahalanobis, 3-D; both paths stop the excess
// re-linearisation walk along the flat valley of far-out targets after at
// most four steps, which leaves a few hundredths of σ), and σ must agree to 1 %.
func TestCoarsePhaseMatchesExact(t *testing.T) {
	type shape struct {
		n          int
		alt, fault bool
		sig        float64
	}
	for _, sh := range []shape{{6, false, false, 50}, {6, true, false, 50}, {6, true, false, 400}, {4, false, false, 50},
		{4, true, false, 50}, {10, true, false, 50}, {20, true, false, 300}, {3, true, false, 50},
		{6, true, true, 50}, {10, true, true, 50}, {8, false, false, 400}} {
		probs := benchProblems(sh.n, sh.alt, sh.fault, sh.sig)
		fast := NewSolver(Options{})
		exact := NewSolver(Options{})
		exact.exactOnly = true
		var a, b Fix
		worst, okA, okB, diff := 0.0, 0, 0, 0
		for i := range probs {
			_ = fast.Solve(&probs[i], &a)
			_ = exact.Solve(&probs[i], &b)
			if a.OK() {
				okA++
			}
			if b.OK() {
				okB++
			}
			if a.OK() != b.OK() {
				diff++
				continue
			}
			if !a.OK() {
				continue
			}
			d := a.ECEF.Sub(b.ECEF)
			inv, err := linalg.Mat3(b.CovECEF).InvSPD()
			if err != nil {
				continue
			}
			worst = math.Max(worst, math.Sqrt(inv.Quad(linalg.Vec3{d.X, d.Y, d.Z})))
			if math.Abs(a.SigmaH/b.SigmaH-1) > 0.01 {
				t.Errorf("n %d alt %v fault %v σ %g problem %d: σh %.1f vs exact %.1f", sh.n, sh.alt, sh.fault, sh.sig, i, a.SigmaH, b.SigmaH)
			}
		}
		t.Logf("n %d alt %v fault %v σ %g ns: accepted %d (exact path %d), differing %d, worst shift %.4f σ",
			sh.n, sh.alt, sh.fault, sh.sig, okA, okB, diff, worst)
		if diff > 1 || worst > 0.1 {
			t.Errorf("n %d alt %v fault %v: %d acceptance differences, worst shift %.3f σ", sh.n, sh.alt, sh.fault, diff, worst)
		}
	}
}
