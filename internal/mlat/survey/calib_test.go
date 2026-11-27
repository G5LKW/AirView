package survey

import (
	"math"
	"testing"

	"airview/internal/mlat/geo"
)

// TestCalibrationSynth checks that the reported covariance is honest over
// many synthetic surveys: the horizontal Mahalanobis² (2 dof) is within its
// 95 % quantile and the vertical error within 2σ in most runs.
func TestCalibrationSynth(t *testing.T) {
	if testing.Short() {
		t.Skip("calibration: 60 surveys")
	}
	for _, kind := range []string{"gps", "sdr"} {
		n, in95h, in2v := 0, 0, 0
		var sumH, sumS float64
		for seed := uint64(100); seed < 130; seed++ {
			s := gpsSynth(seed)
			if kind == "sdr" {
				s = sdrSynth(seed)
				s.durS = 900
			}
			sol := solveSynth(t, s, 15e3)
			f := geo.NewFrame(s.truth)
			d := f.ToENU(sol.ECEF)
			c := sol.CovENU
			det := c[0]*c[4] - c[1]*c[1]
			m2 := (c[4]*d.X*d.X - 2*c[1]*d.X*d.Y + c[0]*d.Y*d.Y) / det
			n++
			if m2 <= 5.99 {
				in95h++
			}
			if math.Abs(d.Z) <= 2*sol.SigmaVM {
				in2v++
			}
			sumH += math.Hypot(d.X, d.Y)
			sumS += sol.SigmaHM
		}
		t.Logf("%s: horizontal within 95 %%: %d/%d, vertical within 2σ: %d/%d, mean error %.1f m, mean σ_h %.1f m",
			kind, in95h, n, in2v, n, sumH/float64(n), sumS/float64(n))
		if float64(in95h) < 0.85*float64(n) || float64(in2v) < 0.8*float64(n) {
			t.Errorf("%s: covariance not honest", kind)
		}
	}
}
