package mlat

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/quality"
	"airview/internal/mlat/ref"
	"airview/internal/model"
)

// Airframe-latency calibration (aggregator.mlat.latencyCal, quality
// LatencyCal; DESIGN §9.6, WP-Q2 item 4). Off by default.
//
// Wired here: the parameters from the file block, the A/B scoring of every
// hold-out snapshot fix (Score; only calibrated airframes count), the
// learning entry point for MLAT fixes of ADS-B aircraft outside the
// hold-out (Learn), the decision once a minute (Decide) and its report in
// Stats.LatencyCal.
//
// Not wired (design decision, carried in HOOKS.md): the engine solves ADS-B
// aircraft only for the hold-out set, so no learning samples arrive until
// the planner grants a budget for "learning" solves of non-hold-out ADS-B
// aircraft; and the reference store does not yet apply ForReference. With
// no calibrated airframe both arms stay empty, the decision stays
// "insufficient" and nothing is applied, so switching the block on is safe.

// latencyCalParams maps the file block onto quality.LatencyCalParams (zero
// = the package default).
func latencyCalParams(c model.AggMLATLatencyCal, salt uint64) quality.LatencyCalParams {
	return quality.LatencyCalParams{
		Salt:             salt ^ 0x5a,
		MinGain:          c.MinGain,
		ZEnable:          c.ZEnable,
		ZDisable:         c.ZDisable,
		TreatFraction:    c.TreatFraction,
		MinCalSamples:    c.MinCalSamples,
		MinArmSamples:    c.MinArmSamples,
		MinArmAircraft:   c.MinArmAircraft,
		WindowNs:         int64(c.WindowH) * 3600e9,
		PriorSigmaS:      c.PriorSigmaS,
		CalibratedSigmaS: c.CalibratedSigmaS,
	}
}

// latencySample builds the along-track comparison of an MLAT position
// (ECEF, covariance) with the reference state st. ok is false when the
// reference has no usable ground speed.
func latencySample(key uint32, nowNs int64, st *ref.State, pos geo.Vec3, cov [9]float64) (quality.LatencySample, bool) {
	fr := geo.NewFrame(geo.LLH{Lat: st.Lat, Lon: st.Lon, H: st.HAE})
	d := fr.Rotate(pos.Sub(st.Pos))
	v := fr.Rotate(st.Vel)
	sp := math.Hypot(v.X, v.Y)
	if !(sp > 1) {
		return quality.LatencySample{}, false
	}
	ue, un := v.X/sp, v.Y/sp
	c := fr.CovToENU(cov)
	varM := ue*ue*c[0] + 2*ue*un*c[1] + un*un*c[4]
	r := st.CovENU
	varR := ue*ue*r[0] + 2*ue*un*r[1] + un*un*r[4]
	return quality.LatencySample{Key: key, NowNs: nowNs,
		AlongM: d.X*ue + d.Y*un, SigmaAlongM: math.Sqrt(math.Max(varM, 0) + math.Max(varR, 0)),
		SpeedMps: sp, AppliedS: st.LatencyS, NACp: st.NACp}, true
}

// observeLatency feeds one MLAT fix of an ADS-B aircraft to the
// calibration: hold-out fixes score the A/B arms, other fixes teach.
func (e *Engine) observeLatency(key uint32, inHoldOut bool, nowNs int64, st *ref.State, pos geo.Vec3, cov [9]float64) {
	if e.latcal == nil {
		return
	}
	s, ok := latencySample(key, nowNs, st, pos, cov)
	if !ok {
		return
	}
	if inHoldOut {
		e.latcal.Score(s)
	} else {
		e.latcal.Learn(s)
	}
}

// LatencyCalReport returns the A/B state (nil while the calibration is off).
func (e *Engine) LatencyCalReport() *quality.ABReport {
	if e.latcal == nil {
		return nil
	}
	r := e.latcal.Last()
	return &r
}
