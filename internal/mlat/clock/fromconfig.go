package clock

import "airview/internal/model"

// ConfigFromModel copies the file block aggregator.mlat.clock onto base
// (which carries the fields the engine sets itself: origin, epoch,
// atmosphere, callbacks). Zero fields of the block keep base's value, so
// withDefaults applies the package default.
func ConfigFromModel(m model.AggMLATClock, base Config) Config {
	c := base
	f := func(dst *float64, v float64) {
		if v != 0 {
			*dst = v
		}
	}
	i := func(dst *int, v int) {
		if v != 0 {
			*dst = v
		}
	}
	f(&c.HostSigmaS, m.HostSigmaS)
	f(&c.PPSCorrelatedNs, m.PPSCorrelatedNs)
	f(&c.ResetThetaSigmaNs, m.ResetThetaSigmaNs)
	i(&c.MaxMembers, m.MaxMembers)
	f(&c.SyncRatePerStation, m.SyncRatePerStation)
	f(&c.MaxObsAgeS, m.MaxObsAgeS)
	f(&c.StationTimeoutS, m.StationTimeoutS)
	f(&c.CoObsWindowS, m.CoObsWindowS)
	f(&c.ProbationS, m.ProbationS)
	f(&c.AnchorFreqMaxPPB, m.AnchorFreqMaxPPB)
	f(&c.AnchorOffsetMaxNs, m.AnchorOffsetMaxNs)
	f(&c.AnchorDriftMaxNsPerH, m.AnchorDriftMaxNsPerH)
	f(&c.AnchorHostMaxS, m.AnchorHostMaxS)
	f(&c.GateProb, m.GateProb)
	f(&c.WTestMax, m.WTestMax)
	f(&c.StepSigma, m.StepSigma)
	i(&c.StepCount, m.StepCount)
	i(&c.StepRunCount, m.StepRunCount)
	f(&c.StepRunWindowS, m.StepRunWindowS)
	f(&c.BlacklistS, m.BlacklistS)
	f(&c.BlacklistMaxFrac, m.BlacklistMaxFrac)
	f(&c.StationBadOutlier, m.StationBadOutlier)
	i(&c.SyncedMinObs, m.SyncedMinObs)
	f(&c.SyncedWindowS, m.SyncedWindowS)
	i(&c.Workers, m.Workers)
	if m.NoJitterLearning {
		c.NoJitterLearning = true
	}
	f(&c.ReoriginS, m.ReoriginS)
	f(&c.RelGaugeBetaSigmaNs, m.RelGaugeBetaSigmaNs)
	f(&c.ExternalInflate, m.ExternalInflate)
	i(&c.PartitionStations, m.PartitionStations)
	i(&c.ExternalStates, m.ExternalStates) // < 0 = off
	f(&c.PartitionDriftNs, m.PartitionDriftNs)
	return c
}
