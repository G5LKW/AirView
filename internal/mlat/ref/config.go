package ref

import "airview/internal/model"

// OptionsFromConfig maps the file-configurable block aggregator.mlat.ref
// onto Options. Zero fields keep the package defaults (withDefaults).
func OptionsFromConfig(c model.AggMLATRef) Options {
	o := Options{
		Window: c.Window, QHorizontal: c.QHorizontal, QVertical: c.QVertical,
		MaxPosAgeS: c.MaxPosAgeS, LatencySigmaS: c.LatencySigmaS,
		GateChi2: c.GateChi2, GateFloorM: c.GateFloorM, ProbationS: c.ProbationS,
		SuspendS: c.SuspendS, TrackTimeoutS: c.TrackTimeoutS,
		AltReplyMinS: c.AltReplyMinS, PairBaroS: c.PairBaroS, NoAltReplies: c.NoAltReplies,
		Field: FieldOptions{CellKm: c.Field.CellKm, BandFt: c.Field.BandFt,
			DriftMPerH: c.Field.DriftMPerH, ObsSigmaM: c.Field.ObsSigmaM,
			ProfileSigmaM: c.Field.ProfileSigmaM, ProfileSigmaTK: c.Field.ProfileSigmaTK},
	}
	if c.MinNACp > 0 && c.MinNACp <= 11 {
		o.MinNACp = uint8(c.MinNACp)
	}
	if c.MinNIC > 0 && c.MinNIC <= 11 {
		o.MinNIC = uint8(c.MinNIC)
	}
	return o
}
