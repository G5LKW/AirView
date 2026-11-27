package quality

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
	"airview/internal/mlat/solve"
)

// Spoof detection by MLAT versus reported position (DESIGN §9.6 ADS-B
// verification, phase 4). VerifyTDOA tests a reported position against the
// raw arrival times of one message; CompareMLAT tests it against an
// independent MLAT solution of the same message (or a track prediction at
// the same instant). Both feed the same Verifier.
//
// With Δ the horizontal difference MLAT − reported in the local ENU frame
// at the reported position and S = Σ_rep,h + Σ_mlat,h the sum of the two
// horizontal covariances,
//
//	d² = Δᵀ S⁻¹ Δ  ~  χ²(2)
//
// when both are right. The message is contradicted when d² exceeds the
// 1 − PFA quantile (−2·ln PFA) AND |Δ| > MinBiasM, confirmed when
// d² <= 13.82 (99.9 %), consistent otherwise ("none" in between). The
// thresholds are the same as VerifyTDOA's and were validated on simnet
// with synthetic spoofers (spoof_simnet_test.go, docs in WIRING.md).

// CompareParams tune CompareMLAT. Zero fields take the defaults.
type CompareParams struct {
	PFA      float64 // false-alarm probability per message (default 1e-4)
	MinBiasM float64 // smallest horizontal difference that may contradict (default 300 m)
	// MaxConfirmMDOM > 0: an agreement confirms only when the comparison
	// could have seen an offset of this size (MDO <= MaxConfirmMDOM, m);
	// otherwise it is consistent.
	MaxConfirmMDOM float64
}

// DefaultCompareParams returns the phase-4 settings validated with
// DefaultVerifyParams.
func DefaultCompareParams() CompareParams {
	return CompareParams{PFA: 1e-4, MinBiasM: 300, MaxConfirmMDOM: 1000}
}

// chi2(2) 99.9 % quantile: −2·ln(1e-3).
const chi2Two999 = 13.815510557964274

// CompareMLAT compares a reported position (ECEF, covariance covRep m²
// row-major, ECEF frame) with an MLAT position of the same instant
// (covMLAT, ECEF frame). The result has Chi2 = d², Dof = 2, MaxBiasM = the
// horizontal distance and MDOM = sqrt(λ₀(2)·λmax(S)), the offset the test
// sees with 80 % power in the worst direction. VerdictConfirmed means the
// MLAT position agrees; the caller decides whether that MLAT position is
// itself trustworthy (≥ 3 timed stations for "verified").
func CompareMLAT(rep geo.Vec3, covRep [9]float64, mlat geo.Vec3, covMLAT [9]float64, p CompareParams) VerifyResult {
	chiOnce.Do(chiTables)
	res := VerifyResult{Dof: 2, Culprit: -1, MDOM: math.Inf(1)}
	fr := geo.NewFrameECEF(rep)
	d := fr.ToENU(mlat)
	res.MaxBiasM = math.Hypot(d.X, d.Y)
	a := fr.CovToENU(covRep)
	b := fr.CovToENU(covMLAT)
	see, sen, snn := a[0]+b[0], a[1]+b[1], a[4]+b[4]
	det := see*snn - sen*sen
	if !(det > 0) || math.IsNaN(res.MaxBiasM) {
		res.Chi2 = math.NaN()
		return res
	}
	d2 := (snn*d.X*d.X - 2*sen*d.X*d.Y + see*d.Y*d.Y) / det
	res.Chi2 = d2
	l1, l2 := linalg.SymEigen2(see, sen, snn)
	res.MDOM = math.Sqrt(lam0[2] * math.Max(l1, l2))
	pfa := p.PFA
	if !(pfa > 0 && pfa < 1) {
		pfa = 1e-4
	}
	minBias := p.MinBiasM
	if !(minBias > 0) {
		minBias = 300
	}
	if p.MaxConfirmMDOM > 0 && res.MDOM > minBias {
		// phase 4: a comparison contradicts only beyond what it can resolve
		minBias = res.MDOM
	}
	switch {
	case d2 > -2*math.Log(pfa) && res.MaxBiasM > minBias:
		res.Verdict = VerdictContradicted
	case d2 <= chi2Two999:
		res.Verdict = VerdictConfirmed
		if p.MaxConfirmMDOM > 0 && !(res.MDOM <= p.MaxConfirmMDOM) {
			res.Verdict = VerdictConsistent
		}
	}
	return res
}

// nacpEPU is the 95 % horizontal estimated position uncertainty (m) by
// NACp (DO-260B Table 2-71 bounds; NACp 0 = unknown).
var nacpEPU = [12]float64{0, 18520, 7408, 3704, 1852, 926, 555.6, 185.2, 92.6, 30, 10, 3}

// RefCovECEF returns a reported position's covariance (ECEF, m²): the NACp
// bound as a circular horizontal Gaussian (σ = EPU / 2.448, the 95 % radius
// of a circular Gaussian), plus the along-track spread of an unknown
// position latency (vel·velᵀ·latSigmaS², vel the ECEF velocity, m/s), plus
// vertSigmaM² vertically. NACp 0 (unknown) is treated as NACp 6 (0.3 NM).
func RefCovECEF(pos, vel geo.Vec3, nacp uint8, latSigmaS, vertSigmaM float64) [9]float64 {
	if nacp == 0 || nacp > 11 {
		nacp = 6
	}
	sh := nacpEPU[nacp] / 2.4477
	fr := geo.NewFrameECEF(pos)
	v := fr.Rotate(vel)
	var enu [9]float64
	enu[0], enu[4], enu[8] = sh*sh, sh*sh, vertSigmaM*vertSigmaM
	l2 := latSigmaS * latSigmaS
	vv := [3]float64{v.X, v.Y, v.Z}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			enu[i*3+j] += vv[i] * vv[j] * l2
		}
	}
	return fr.CovToECEF(enu)
}

// VerifyChi2Thresholds returns the contradiction and confirmation χ²
// thresholds VerifyTDOA uses for dof degrees of freedom at false-alarm
// probability pfa (0 = the default 1e-4). Tools and tests use it to
// reproduce a verdict.
func VerifyChi2Thresholds(pfa float64, dof int) (contradict, confirm float64) {
	if !(pfa > 0 && pfa < 1) {
		pfa = 1e-4
	}
	return solve.Chi2Quantile(pfa, dof), solve.Chi2Quantile(1e-3, dof)
}

// MLATComparable reports whether an MLAT fix may judge a reported position
// with CompareMLAT: accepted, unambiguous, not a prior-fused posterior (it
// would carry the reported track back in), and with two or more redundant
// rows so that RAIM could have caught a bad timestamp (DOF >= 2 lowered
// the honest per-message contradiction rate on simnet from 0.36 % to
// 0.32 %; the phase-4 power rules did the rest).
func MLATComparable(f *solve.Fix) bool {
	return f.OK() && f.DOF >= 2 && f.Flags&(solve.FlagAmbiguous|solve.FlagPriorFused) == 0
}
