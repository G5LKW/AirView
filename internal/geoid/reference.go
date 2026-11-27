package geoid

// RefPoint is a published EGM96 geoid height used to validate a grid.
type RefPoint struct {
	Lat, Lon float64 // degrees (Lon may be 0…360)
	N        float64 // EGM96 geoid undulation, metres
}

// NGATestPoints are the check points distributed by NGA with its EGM96
// interpolation program (INTPT.DAT / OUTINTPT.DAT), transcribed here. A
// correct 1° grid with bilinear interpolation must reproduce them within
// 2 m (the acceptance tolerance of DESIGN §9.1); the full model reproduces
// them to the millimetre.
var NGATestPoints = []RefPoint{
	{38.6281550, 269.7791550, -31.628},
	{-14.6212170, 305.0211140, -2.969},
	{46.8743190, 102.4487290, -43.575},
	{-23.6174460, 133.8747120, 15.871},
	{38.6254730, 359.9995000, 50.066},
	{-0.4667440, 0.0023000, 17.329},
}

// Extremes are the well-known global extremes of the EGM96 geoid, for a
// coarse sanity check of a grid (tolerance a few metres at 1°): the low
// south of India and the high north of Australia / New Guinea.
var Extremes = []RefPoint{
	{4.7, 78.8, -107.0},
	{-8.1, 147.3, 85.4},
}
