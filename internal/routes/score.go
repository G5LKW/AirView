package routes

import (
	"math"
	"strings"

	"airview/internal/geo"
)

// Plausibility score of a route against the live track
// (docs/ROUTES-DESIGN.md §3.2). Pure functions, no I/O: Evaluate costs a
// few great-circle computations per leg.

// Route states.
const (
	StateFits      = "fits"
	StateUncertain = "uncertain"
	StateDoubtful  = "doubtful"
	StateReversed  = "reversed"
	StateUnchecked = "unchecked"
)

// Stop is one airport of a route.
type Stop struct {
	Code   string  `json:"code"`
	IATA   string  `json:"iata,omitempty"`
	Name   string  `json:"name,omitempty"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	ElevFt float64 `json:"elevFt"` // NaN = unknown
	Size   int     `json:"size"`   // navdata.SizeClass: 3 large, 2 medium, 1 small
	Known  bool    `json:"known"`  // resolved from the airport data
}

// Obs is the aircraft's current state. Unknown values are NaN.
type Obs struct {
	Lat, Lon float64
	AltFt    float64 // baro, else geometric
	VR       float64 // ft/min
	Track    float64 // deg true
	GS       float64 // kt
	OnGround bool
	Category string // ADS-B emitter category, e.g. "A3"
}

// Score is the evaluation of one route at one position.
type Score struct {
	State     string  `json:"state"`
	S         float64 `json:"score"`   // smaller = better; fits <= 1.5 < uncertain < 4 <= doubtful
	Z         float64 `json:"z"`       // detour (ellipse) term of the active leg
	Leg       int     `json:"leg"`     // 0-based active leg
	AlongPct  float64 `json:"along"`   // 0..100 along the active leg
	DistRemKm float64 `json:"distRem"` // remaining great-circle distance to the active leg's end
	Reason    string  `json:"reason"`
}

// Scoring thresholds.
const (
	fitsMax      = 1.5
	doubtfulMin  = 4.0
	zHardFail    = 2.0
	nearEndKm    = 40.0
	nearEndAGLFt = 12000.0
	headingOffKm = 150.0
)

// detourZ is the normalised detour of P on the leg A->B: 0 on the great
// circle, 1 on the edge of the tolerance ellipse, > 2 a hard fail.
func detourZ(dA, dB, d float64) float64 {
	eps := dA + dB - d
	k := math.Max(0.10, math.Min(0.60, 0.10+60/math.Max(d, 1)))
	return math.Max(0, eps) / (k*d + 20)
}

// Evaluate scores a route (stops in flight order) against the observation.
func Evaluate(stops []Stop, o Obs) Score {
	known := make([]int, 0, len(stops))
	for i := range stops {
		if stops[i].Known {
			known = append(known, i)
		}
	}
	if len(known) < 2 || !geo.ValidLatLon(o.Lat, o.Lon) {
		return Score{State: StateUnchecked, Leg: -1}
	}
	// Active leg: smallest detour, tie-broken by heading towards the leg end.
	best, bestSel := -1, math.Inf(1)
	var bz, bdA, bdB, bD float64
	for li := 0; li+1 < len(known); li++ {
		a, b := &stops[known[li]], &stops[known[li+1]]
		d := geo.HaversineKm(a.Lat, a.Lon, b.Lat, b.Lon)
		dA := geo.HaversineKm(a.Lat, a.Lon, o.Lat, o.Lon)
		dB := geo.HaversineKm(o.Lat, o.Lon, b.Lat, b.Lon)
		z := detourZ(dA, dB, d)
		sel := z
		if finite(o.Track) && dB > 1 {
			sel += 0.1 * math.Abs(geo.AngleDiff(o.Track, geo.InitialBearing(o.Lat, o.Lon, b.Lat, b.Lon))) / 180
		}
		if sel < bestSel {
			best, bestSel = li, sel
			bz, bdA, bdB, bD = z, dA, dB, d
		}
	}
	a, b := &stops[known[best]], &stops[known[best+1]]
	sc := Score{Z: round(bz, 2), Leg: best, DistRemKm: round(bdB, 1)}
	if bD > 0 {
		along := geo.AlongTrackKm(o.Lat, o.Lon, a.Lat, a.Lon, b.Lat, b.Lon)
		sc.AlongPct = round(math.Max(0, math.Min(100, along/bD*100)), 1)
	}
	var why []string
	s := bz * bz
	if bz > zHardFail {
		why = append(why, "far off the route")
	}

	climbing := finite(o.VR) && o.VR > 500
	descending := finite(o.VR) && o.VR < -300
	agl := func(st *Stop) float64 {
		if !finite(o.AltFt) {
			return math.NaN()
		}
		if o.OnGround {
			return 0
		}
		if finite(st.ElevFt) {
			return o.AltFt - st.ElevFt
		}
		return o.AltFt
	}

	// Near an end the aircraft must be low enough, or be climbing out of the
	// origin / descending into the destination: an airliner cruising over
	// its "destination" hub is not arriving there.
	if bdA <= nearEndKm {
		if h := agl(a); finite(h) && h > nearEndAGLFt && !climbing {
			s += 4
			why = append(why, "high over the origin")
		}
	} else if bdB <= nearEndKm {
		if h := agl(b); finite(h) && h > nearEndAGLFt && !descending {
			s += 4
			why = append(why, "high over the destination")
		}
	}

	// Heading: cruising away from the destination does not fit.
	reversed := false
	if finite(o.Track) && bdA > headingOffKm && bdB > headingOffKm && !o.OnGround {
		dB := math.Abs(geo.AngleDiff(o.Track, geo.InitialBearing(o.Lat, o.Lon, b.Lat, b.Lon)))
		dA := math.Abs(geo.AngleDiff(o.Track, geo.InitialBearing(o.Lat, o.Lon, a.Lat, a.Lon)))
		s += 0.5 * (dB / 45) * (dB / 45)
		if dA < 30 && dB > 150 && sc.AlongPct > 10 && sc.AlongPct < 90 {
			reversed = true
		}
		if dB > 90 && !reversed {
			why = append(why, "heading away from the destination")
		}
	}

	// Vertical evidence.
	switch {
	case bdA <= 100 && climbing:
		s -= 1
		why = append(why, "climbing out of the origin")
	case bdA <= nearEndKm && descending && finite(agl(a)) && agl(a) < 5000:
		s += 2
		why = append(why, "descending into the origin")
	}
	switch {
	case bdB <= 200 && descending:
		s -= 1
		why = append(why, "descending towards the destination")
	case bdB <= nearEndKm && climbing && finite(agl(b)) && agl(b) < 10000:
		s += 2
		why = append(why, "climbing out of the destination")
	}
	s = math.Max(0, s)
	sc.S = round(s, 2)
	switch {
	case reversed:
		sc.State = StateReversed
		why = append(why, "flying the route backwards")
	case bz > zHardFail || s >= doubtfulMin:
		sc.State = StateDoubtful
	case s <= fitsMax:
		sc.State = StateFits
	default:
		sc.State = StateUncertain
	}
	if len(why) == 0 && sc.State == StateFits {
		why = append(why, "fits the track")
	}
	sc.Reason = strings.Join(why, ", ")
	return sc
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func round(v float64, dp int) float64 {
	p := math.Pow(10, float64(dp))
	return math.Round(v*p) / p
}
