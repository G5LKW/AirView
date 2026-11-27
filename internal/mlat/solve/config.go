package solve

import "airview/internal/model"

// OptionsFromConfig maps the file-configurable block aggregator.mlat.solve
// onto Options (zero fields keep the defaults applied by Defaults). The
// engine sets Atmosphere separately.
func OptionsFromConfig(c model.AggMLATSolve) Options {
	return Options{
		MaxIter: c.MaxIter, HuberK: c.HuberK, PFA: c.PFA, MaxSigmaH: c.MaxSigmaHM,
		AltRejectSigma: c.AltRejectSigma, PriorSeedMaxSigma: c.PriorSeedMaxSigmaM,
		GridStepM: c.GridStepM, GridHalfM: c.GridHalfM, MaxSpeedMps: c.MaxSpeedMps,
		MachFactor: c.MachFactor, HostTolS: c.HostTolS, HorizonSlackM: c.HorizonSlackM,
		MinHAE: c.MinHAEM, MaxHAE: c.MaxHAEM, MaxRangeM: c.MaxRangeM, RelinM: c.RelinM,
		RANSACMinStations: c.RANSACMinStations, RANSACMaxSubsets: c.RANSACMaxSubsets,
		AlwaysAlternates: c.AlwaysAlternates,
	}
}
