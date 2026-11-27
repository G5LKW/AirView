package navdata

import (
	"math"

	"airview/internal/geo"
)

// TrackPoint is one position of a flight for airport association.
// Unknown values are NaN (AltFt, GS, Track).
type TrackPoint struct {
	T        int64   // Unix ms
	Lat, Lon float64 //
	AltFt    float64 // baro preferred
	GS       float64 // kt
	Track    float64 // deg true
	OnGround bool
}

// AirportGuess is a likely departure or arrival airport.
type AirportGuess struct {
	Airport     Airport  `json:"airport"`
	Runway      string   `json:"runway,omitempty"` // best aligned runway end, when one fits
	DistKm      float64  `json:"distKm"`           // from the reference track point
	HeightAGLFt *float64 `json:"heightAglFt,omitempty"`
	Confidence  float64  `json:"confidence"` // 0..1
	Reason      string   `json:"reason"`
	PointT      int64    `json:"pointT"` // time of the track point used (Unix ms)
}

// Association limits.
const (
	assocMaxKm      = 15.0   // search radius around the low point
	assocMaxAGLFt   = 4000.0 // the reference point must be lower than this above the field
	assocLookPoints = 6      // how many first/last points are considered
)

// GuessDeparture estimates the departure airport from the first
// low-altitude positions of a flight (points in time order). ok=false when
// the flight was first seen too high or far from any airport.
func GuessDeparture(points []TrackPoint, idx RunwayIndex) (AirportGuess, bool) {
	n := min(len(points), assocLookPoints)
	return guess(points[:n], idx, false)
}

// GuessArrival estimates the arrival airport from the last low-altitude
// positions of a flight (points in time order).
func GuessArrival(points []TrackPoint, idx RunwayIndex) (AirportGuess, bool) {
	n := min(len(points), assocLookPoints)
	return guess(points[len(points)-n:], idx, true)
}

func guess(pts []TrackPoint, idx RunwayIndex, arrival bool) (AirportGuess, bool) {
	if idx == nil || len(pts) == 0 {
		return AirportGuess{}, false
	}
	var best AirportGuess
	bestScore := math.Inf(1)
	var cand [16]Airport
	var endsBuf [32]RunwayEndInfo
	for pi := range pts {
		p := &pts[pi]
		if !geo.ValidLatLon(p.Lat, p.Lon) {
			continue
		}
		as := idx.AirportsNear(p.Lat, p.Lon, assocMaxKm, SizeSmall, cand[:0])
		for ai := range as {
			a := &as[ai]
			d := geo.HaversineKm(p.Lat, p.Lon, a.Lat, a.Lon)
			agl := math.NaN()
			if !math.IsNaN(p.AltFt) {
				agl = p.AltFt - a.elevFt()
			}
			if p.OnGround {
				agl = 0
			}
			if !math.IsNaN(agl) && agl > assocMaxAGLFt {
				continue
			}
			// Score: distance (km) + height (per 1000 ft) - size bonus - runway alignment bonus.
			score := d
			if !math.IsNaN(agl) {
				score += math.Max(agl, 0) / 1000 * 1.5
			} else {
				score += 3
			}
			score -= float64(a.Size) * 0.7
			if a.Scheduled {
				score -= 0.5
			}
			rwy := ""
			if !math.IsNaN(p.Track) {
				ends := RunwayEnds(a, endsBuf[:0])
				bestLat := math.Inf(1)
				for ei := range ends {
					e := &ends[ei]
					// Arrival: track ~ landing heading. Departure: track ~ take-off heading (same convention).
					if math.Abs(geo.AngleDiff(p.Track, e.HeadingT)) > 20 {
						continue
					}
					// Among aligned ends prefer the one whose centreline passes closest (parallel runways).
					dd, brg := geo.DistanceBearing(e.Lat, e.Lon, p.Lat, p.Lon)
					lat := math.Abs(dd * math.Sin(geo.Rad(brg-e.HeadingT)))
					if lat < bestLat {
						bestLat, rwy = lat, e.Runway
					}
				}
				if rwy != "" && bestLat <= 2 {
					score -= 2
				} else {
					rwy = ""
				}
			}
			if score < bestScore {
				bestScore = score
				best = AirportGuess{Airport: *a, Runway: rwy, DistKm: round(d, 2), PointT: p.T}
				best.Airport.Runways = nil
				best.Airport.DistKm = nil
				if !math.IsNaN(agl) {
					best.HeightAGLFt = fptr(math.Round(agl))
				} else {
					best.HeightAGLFt = nil
				}
				best.Confidence = confidence(d, agl, rwy != "")
				switch {
				case p.OnGround:
					best.Reason = "on ground"
				case rwy != "":
					best.Reason = "low and aligned with runway " + rwy
				default:
					best.Reason = "low near airport"
				}
				if arrival {
					best.Reason = "arrival: " + best.Reason
				} else {
					best.Reason = "departure: " + best.Reason
				}
			}
		}
	}
	return best, !math.IsInf(bestScore, 1)
}

func confidence(distKm, agl float64, aligned bool) float64 {
	c := 1 - distKm/assocMaxKm
	if math.IsNaN(agl) {
		c *= 0.6
	} else {
		c *= 1 - math.Max(agl, 0)/assocMaxAGLFt*0.6
	}
	if aligned {
		c = math.Min(1, c+0.2)
	}
	return round(math.Max(0, math.Min(1, c)), 2)
}

// Phase is a coarse flight phase.
type Phase string

// Flight phases.
const (
	PhaseUnknown  Phase = "unknown"
	PhaseGround   Phase = "ground"
	PhaseTakeoff  Phase = "takeoff"
	PhaseClimb    Phase = "climb"
	PhaseCruise   Phase = "cruise"
	PhaseDescent  Phase = "descent"
	PhaseApproach Phase = "approach"
)

// ClassifyPhase derives a coarse flight phase from one state sample.
// heightAGLFt is the height above the nearest airport (NaN if unknown; then
// altFt is used), vrFpm the vertical rate, gsKt the ground speed.
// onApproach should be true when MatchApproach found a fitting runway.
func ClassifyPhase(altFt, heightAGLFt, vrFpm, gsKt float64, onGround, onApproach bool) Phase {
	if onGround {
		return PhaseGround
	}
	h := heightAGLFt
	if math.IsNaN(h) {
		h = altFt
	}
	if math.IsNaN(h) {
		return PhaseUnknown
	}
	vr := vrFpm
	if math.IsNaN(vr) {
		vr = 0
	}
	switch {
	case onApproach && vr <= 300:
		return PhaseApproach
	case h < 1500 && vr > 300:
		return PhaseTakeoff
	case h < 3000 && vr < -300:
		return PhaseApproach
	case vr > 500:
		return PhaseClimb
	case vr < -500:
		return PhaseDescent
	case !math.IsNaN(gsKt) && gsKt < 50 && h < 500:
		return PhaseGround
	case altFt >= 10000 || (math.IsNaN(altFt) && h >= 10000):
		return PhaseCruise
	case math.Abs(vr) <= 500:
		return PhaseCruise
	}
	return PhaseUnknown
}
