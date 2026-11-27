package group

import "airview/internal/model"

// ConfigFromModel maps the file-configurable block aggregator.mlat.group
// onto Config: zero fields keep DefaultConfig, the result is normalized.
func ConfigFromModel(m model.AggMLATGroup) Config {
	c := DefaultConfig()
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
	setF(&c.NSigma, m.NSigma)
	setF(&c.SlackM, m.SlackM)
	setF(&c.HintSlackM, m.HintSlackM)
	setF(&c.HintMaxSigmaM, m.HintMaxSigmaM)
	setF(&c.ColocatedM, m.ColocatedM)
	setI(&c.MinSites, m.MinSites)
	setI(&c.MinSitesAlt, m.MinSitesAlt)
	setI(&c.MaxParts, m.MaxParts)
	setI(&c.LateSlots, m.LateSlots)
	if m.LateHoldMs > 0 {
		c.LateHoldNs = int64(m.LateHoldMs) * 1e6
	}
	return c.Normalize()
}
