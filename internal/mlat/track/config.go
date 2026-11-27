package track

import "airview/internal/model"

// ParamsFromConfig maps the file-configurable block aggregator.mlat.track
// onto Params (zero fields keep the defaults applied by Normalize). The
// engine sets Atmosphere separately.
func ParamsFromConfig(c model.AggMLATTrack) Params {
	return Params{
		QCV: c.QCv, QCT: c.QCt, QVert: c.QVert, QOmega: c.QOmega, OmegaTauS: c.OmegaTauS,
		SojournCVS: c.SojournCvS, SojournCTS: c.SojournCtS, InitPCT: c.InitPCt,
		InitSigmaVelH: c.InitSigmaVelH, InitSigmaVelU: c.InitSigmaVelU, InitSigmaOmega: c.InitSigmaOmega,
		GateProb: c.GateProb, Iterations: c.Iterations,
		ConfirmWindowS: c.ConfirmWindowS, ConfirmUpdates: c.ConfirmUpdates, CoastS: c.CoastS,
		MaxSigmaHM: c.MaxSigmaHM, MaxSpeedMS: c.MaxSpeedMS, HypLR: c.HypLR, MaxHyp: c.MaxHyp,
		RespawnMisses: c.RespawnMisses, LOSMarginM: c.LOSMarginM, MaxMembers: c.MaxMembers,
		MaxExclude: c.MaxExclude, MaxLagS: c.MaxLagS, ExcessRefreshM: c.ExcessRefreshM,
		RecentreM: c.RecentreM, SyncTauS: c.SyncTauS,
	}
}
