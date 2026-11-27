package privacy

import (
	"math"

	"airview/internal/model"
)

// StripAircraft removes every receiver-related field of a model.Aircraft:
// geometry relative to a receiver (distance, bearing, elevation, slant
// range, closest approach, terrain line of sight), reception (RSSI, feeds,
// antennas) and the network fields Mine and Stations unless keepOwn is set
// (owner scope, where the tracker has already limited Stations to the
// owner's own stations).
//
// The tracker's per-viewer projection does the same in aggregator mode;
// this is the second line of defence for paths that might reach a
// non-admin viewer before the tracker runs in network mode (DESIGN §8.4).
func StripAircraft(a *model.Aircraft, keepOwn bool) {
	a.DistKm, a.Bearing, a.Elevation, a.SlantKm = nil, nil, nil, nil
	a.CPAKm, a.CPAInS = nil, nil
	a.TerrainVisible, a.LOSMarginDeg = nil, nil
	a.RSSI, a.RSSIAvg = nil, nil
	a.Feeds, a.Antennas = nil, nil
	if !keepOwn {
		a.Mine, a.Stations = false, nil
	}
}

// StripSnapshot copies s and strips every aircraft (see StripAircraft). The
// maximum range of the live picture is a receiver product and is zeroed.
func StripSnapshot(s model.LiveSnapshot, keepOwn bool) model.LiveSnapshot {
	out := s
	out.Aircraft = make([]model.Aircraft, len(s.Aircraft))
	copy(out.Aircraft, s.Aircraft)
	for i := range out.Aircraft {
		StripAircraft(&out.Aircraft[i], keepOwn)
	}
	out.Counts.MaxRangeKm = 0
	return out
}

// Island guard (DESIGN §8.6). A low aircraft heard only by one station
// reveals that station's area; with the guard on, aircraft on the ground or
// below IslandAltFt whose only receivers are hidden-pin stations are shown
// at IslandGridDeg rounding.
const (
	IslandAltFt   = 1000
	IslandGridDeg = 0.05
)

// IslandUnknownGroundFt is the ground elevation the island guard assumes
// when neither the elevation model nor the hearing stations tell it: the
// guard then errs on the side of coarsening (DESIGN §8.6 "below 1000 ft
// AGL" must hold over high terrain too).
const IslandUnknownGroundFt = 5000

// IslandMaxGroundFt bounds any ground elevation (highest terrain on earth,
// rounded up): aircraft above IslandAltFt+IslandMaxGroundFt are never low.
const IslandMaxGroundFt = 29500

// IslandLow reports whether an aircraft is a candidate for the island guard
// over sea-level ground: on the ground, or below IslandAltFt (barometric,
// else geometric altitude). Aircraft without a known altitude are treated
// as low. Prefer IslandLowAGL, which takes the ground elevation.
func IslandLow(onGround *bool, altBaro, altGeom *float64) bool {
	return IslandLowAGL(onGround, altBaro, altGeom, 0)
}

// IslandLowAGL reports whether an aircraft is on the ground or less than
// IslandAltFt above the ground, groundFt being the ground (terrain)
// elevation in feet AMSL under or near the aircraft. Aircraft without a
// known altitude are treated as low.
func IslandLowAGL(onGround *bool, altBaro, altGeom *float64, groundFt float64) bool {
	if onGround != nil && *onGround {
		return true
	}
	return AltLowAGL(altOf(altBaro, altGeom), groundFt)
}

// AltLowAGL reports whether altFt (AMSL; NaN = unknown) is less than
// IslandAltFt above groundFt. Unknown altitudes count as low.
func AltLowAGL(altFt, groundFt float64) bool {
	if math.IsNaN(altFt) {
		return true
	}
	if math.IsNaN(groundFt) || groundFt < 0 {
		groundFt = 0
	}
	return altFt < IslandAltFt+groundFt
}

func altOf(altBaro, altGeom *float64) float64 {
	switch {
	case altBaro != nil:
		return *altBaro
	case altGeom != nil:
		return *altGeom
	}
	return math.NaN()
}

// RoundGrid rounds a coordinate to the island-guard grid (0.05°).
func RoundGrid(v float64) float64 {
	return math.Round(math.Round(v/IslandGridDeg)*IslandGridDeg*1e4) / 1e4
}

// OnlyHidden reports whether every station in heard has a hidden pin
// (unknown stations count as hidden). An empty list is not "only hidden":
// the aircraft is not attributed to any station.
func OnlyHidden(heard []model.StationIdx, res model.StationResolver) bool {
	if len(heard) == 0 {
		return false
	}
	for _, st := range heard {
		if res == nil {
			return true
		}
		m, ok := res.Info(st)
		if ok && m.Pin != "" && m.Pin != model.PinHidden {
			return false
		}
	}
	return true
}
