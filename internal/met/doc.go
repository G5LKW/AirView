// Package met derives meteorology from aircraft: wind and temperature
// aloft from Mode S enhanced surveillance (the "Mode-S EHS" technique of
// de Haan / KNMI and EMADDC) and from direct BDS 4,4 reports, vertical
// profiles above the station, and their comparison with a model profile.
//
// Pieces:
//
//   - Derive / DeriveDetail: pure function from one aircraft state (Input)
//     to wind and static air temperature with quality gating.
//   - MagVar / MagField: World Magnetic Model 2025 (degree 12, secular
//     variation), needed to turn magnetic heading into true heading.
//   - Collector: rate-limited Observe, per-airframe heading-bias learning,
//     10-minute and hourly accumulation in 1000 ft layers, persistence
//     through internal/store (met_profile increments once per closed hour, thinned met_sample
//     samples, met_bias corrections) and the query methods Current,
//     History, HourProfile, Latest, CompareModel, RadioLayers.
//   - Analyze, Compare, RadioLayers: pure functions on profile levels
//     (freezing level, tropopause, jet maximum, lapse rates, inversions,
//     winds-aloft table, model verification, refractivity of inversions).
//
// Conventions: knots, feet (pressure altitude), degrees true, wind
// direction = where the wind blows FROM, U east / V north = air motion,
// degC. Inputs use NaN for "unknown"; every result type meant for JSON
// uses nil pointers instead and never contains NaN.
//
// The package does not import internal/tracker; the engine converts
// tracker updates into Input values.
package met
