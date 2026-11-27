package radio

import (
	"math"

	"airview/internal/geo"
)

// Radio horizon over a smooth spherical earth.
//
// With the effective-earth-radius model (ITU-R P.834) rays are straight over
// an earth of radius Re = k·R, so a point at height h sees the surface out to
//
//	d = Re·acos(Re/(Re + h)) ≈ sqrt(2·k·R·h)
//	d_km ≈ 3.57·sqrt(k·h_m)   →   4.12·sqrt(h_m) for k = 4/3
//
// and two elevated points see each other out to the sum of their horizons.
// Heights are above the smooth surface: use heights above mean sea level over
// the sea or low flat country; for an inland plateau subtract the typical
// terrain elevation of the surroundings from both ends, and remember that
// real terrain and clutter only ever shorten these figures.
//
// k: pass 0 for the standard 4/3. Values at or beyond KMax (and negative
// ones) mean trapping and are treated as a flat earth, see geo.EffectiveK.

// RadioHorizonKm is the ground distance in km to the radio horizon from a
// height of heightM metres above a smooth earth for effective-earth factor k
// (exact spherical formula).
func RadioHorizonKm(heightM, k float64) float64 { return geo.HorizonKm(heightM, k) }

// RadioHorizonApproxKm is the usual closed form sqrt(2·k·R·h) =
// 3.57·sqrt(k·h_m) km (4.12·sqrt(h_m) for k = 4/3). It agrees with
// RadioHorizonKm to better than 0.1 % for any aircraft altitude.
func RadioHorizonApproxKm(heightM, k float64) float64 {
	if !(heightM > 0) {
		return 0
	}
	return math.Sqrt(2 * geo.EffectiveK(k) * geo.EarthRadiusKm * heightM / 1000)
}

// LineOfSightKm is the maximum ground distance in km at which an antenna at
// antennaM metres and an aircraft at aircraftM metres above the smooth earth
// are within radio line of sight: the sum of the two radio horizons.
func LineOfSightKm(antennaM, aircraftM, k float64) float64 {
	return geo.HorizonKm(antennaM, k) + geo.HorizonKm(aircraftM, k)
}

// LineOfSightFtKm is LineOfSightKm with the aircraft altitude in feet.
func LineOfSightFtKm(antennaM, aircraftFt, k float64) float64 {
	return LineOfSightKm(antennaM, aircraftFt*geo.MetresPerFoot, k)
}

// MinAltitudeM is the inverse: the lowest altitude in metres at which an
// aircraft at ground distance groundKm is above the smooth-earth radio
// horizon of an antenna at antennaM metres (0 inside the antenna's own horizon).
func MinAltitudeM(groundKm, antennaM, k float64) float64 {
	return geo.MinVisibleAltitudeM(groundKm, antennaM, k)
}

// topBandCeilingFt closes the open-ended highest altitude band for tables.
const topBandCeilingFt = 50000.0

// BandRange is the radio line-of-sight range for one of the standard
// altitude bands (geo.AltBand).
type BandRange struct {
	Band    int     `json:"band"`    // 0..6
	Label   string  `json:"label"`   // "<1k", "1-5k", …
	AltFt   float64 `json:"altFt"`   // representative altitude of the band (geo.AltBandTypicalFt)
	LOSKm   float64 `json:"losKm"`   // line-of-sight range at AltFt
	LOSLoKm float64 `json:"losLoKm"` // at the band's lower edge (0 ft for the lowest band)
	LOSHiKm float64 `json:"losHiKm"` // at the band's upper edge (50 000 ft for the highest band)
}

// HorizonTable returns the smooth-earth line-of-sight range for each of the
// seven standard altitude bands, for an antenna at antennaM metres and
// effective-earth factor k (0 = 4/3).
func HorizonTable(antennaM, k float64) []BandRange {
	out := make([]BandRange, geo.AltBands)
	for b := range out {
		lo, hi := bandEdgesFt(b)
		typ := geo.AltBandTypicalFt(b)
		out[b] = BandRange{
			Band: b, Label: geo.AltBandLabel(b), AltFt: typ,
			LOSKm:   LineOfSightFtKm(antennaM, typ, k),
			LOSLoKm: LineOfSightFtKm(antennaM, lo, k),
			LOSHiKm: LineOfSightFtKm(antennaM, hi, k),
		}
	}
	return out
}

// bandEdgesFt returns finite edges for an altitude band.
func bandEdgesFt(band int) (lo, hi float64) {
	lo, hi = geo.AltBandBoundsFt(band)
	if math.IsInf(lo, -1) {
		lo = 0
	}
	if math.IsInf(hi, 1) {
		hi = topBandCeilingFt
	}
	return lo, hi
}

// HorizonShift describes how the line-of-sight range moves when the
// atmosphere's k-factor departs from the standard 4/3.
type HorizonShift struct {
	K          float64 `json:"k"`          // k-factor used (sanitised)
	RangeKm    float64 `json:"rangeKm"`    // line-of-sight range for K
	StandardKm float64 `json:"standardKm"` // line-of-sight range for k = 4/3
	DeltaKm    float64 `json:"deltaKm"`    // RangeKm − StandardKm
	Factor     float64 `json:"factor"`     // RangeKm / StandardKm (≈ sqrt(k / (4/3)))
	// Trapping is true when k is at or beyond KMax (dN/dh <= −157 N/km): the
	// earth is effectively flat for the ray, the horizon no longer limits
	// the range and RangeKm is only a geometric upper bound.
	Trapping bool `json:"trapping"`
}

// HorizonShiftForK predicts how far the radio horizon moves for
// effective-earth factor k, for an antenna at antennaM and an aircraft at
// aircraftM metres above the smooth earth. Use KFactor to obtain k from a
// refractivity gradient.
func HorizonShiftForK(antennaM, aircraftM, k float64) HorizonShift {
	trapping := k < 0 || k >= KMax
	ke := geo.EffectiveK(k)
	s := HorizonShift{
		K:          ke,
		RangeKm:    LineOfSightKm(antennaM, aircraftM, ke),
		StandardKm: LineOfSightKm(antennaM, aircraftM, KStandard),
		Trapping:   trapping,
	}
	s.DeltaKm = s.RangeKm - s.StandardKm
	if s.StandardKm > 0 {
		s.Factor = s.RangeKm / s.StandardKm
	} else {
		s.Factor = 1
	}
	return s
}
