package plan

import (
	"time"

	"airview/internal/model"
)

// ConfigFromModel maps the file-configurable block aggregator.mlat.plan
// (and aggregator.mlat.modeAc) onto Config: zero fields keep DefaultConfig,
// the result is normalized.
func ConfigFromModel(m model.AggMLAT) Config {
	c := DefaultConfig()
	p := m.Plan
	setF := func(dst *float64, v float64) {
		if v > 0 {
			*dst = v
		}
	}
	setI := func(dst *int, v int) {
		if v > 0 {
			*dst = v
		}
	}
	setS := func(dst *time.Duration, v int) {
		if v > 0 {
			*dst = time.Duration(v) * time.Second
		}
	}
	setI(&c.SyncSetMin, p.SyncSetMin)
	setI(&c.SyncSetMax, p.SyncSetMax)
	setI(&c.MLATSetMax, p.MLATSetMax)
	setF(&c.StationFPSBudget, p.StationFPSBudget)
	setI(&c.StationsPerAircraft, p.StationsPerAircraft)
	setI(&c.MinStationsPerAircraft, p.MinStationsPerAircraft)
	setF(&c.TargetSigmaM, p.TargetSigmaM)
	setS(&c.HeardTTL, p.HeardTTLS)
	setS(&c.ADSBGoodAge, p.ADSBGoodAgeS)
	setS(&c.MinDwell, p.MinDwellS)
	setI(&c.MaxChurn, p.MaxChurn)
	setS(&c.ReplanEvery, p.ReplanEveryS)
	setF(&c.AcquireHz, p.AcquireHz)
	setF(&c.TrackedHz, p.TrackedHz)
	setF(&c.HoldOutHz, p.HoldOutHz)
	setF(&c.SuspectHz, p.SuspectHz)
	setF(&c.GlobalSolveHz, p.GlobalSolveHz)
	c.ModeAC.Enabled = m.ModeAC
	return c.Normalize()
}
