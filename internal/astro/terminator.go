package astro

import (
	"math"
	"time"
)

// The night side of the earth for a sun-altitude limit a (a <= 0) is the
// spherical cap of angular radius 90°+a centred on the anti-solar point.
// Two cases have to be drawn differently on a longitude/latitude map:
//
//   - The cap contains a geographic pole (always true for a = 0 except at the
//     exact equinox instant). Its boundary then crosses every meridian once,
//     so the ring runs along the boundary from lon −180 to +180 and is closed
//     over the dark pole: …,(180,φ),(180,±90),(−180,±90),(−180,φ). This is the
//     "pole wrap" that naive implementations get wrong around the equinoxes.
//   - The cap contains no pole (deep-twilight caps near the equinoxes). The
//     boundary is an ordinary closed curve; its longitudes are unwrapped to be
//     continuous around the anti-solar meridian and may therefore run outside
//     [−180,180] — GeoJSON renderers such as MapLibre draw that correctly
//     across the antimeridian.
//
// Rings are returned in GeoJSON order ([lon, lat]), closed (first == last)
// and counter-clockwise (RFC 7946 exterior ring). Pole closure points use
// latitude ±90 exactly; MapLibre/geojson-vt clamp that to the Web-Mercator
// limit, other libraries may need the caller to clamp to ±85.0511.

// DefaultTerminatorStepDeg is the sampling step used when stepDeg <= 0.
const DefaultTerminatorStepDeg = 2.0

// WebMercatorMaxLat is the latitude at which the square Web-Mercator world
// ends: atan(sinh(π)) = 85.0511°.
const WebMercatorMaxLat = 85.05112877980659

// ClampRing returns a copy of a ring with every latitude limited to
// ±maxLatDeg (use WebMercatorMaxLat), for map libraries that cannot project
// the ±90° pole-closure vertices themselves. Longitudes are left untouched.
func ClampRing(ring [][2]float64, maxLatDeg float64) [][2]float64 {
	out := make([][2]float64, len(ring))
	for i, p := range ring {
		if p[1] > maxLatDeg {
			p[1] = maxLatDeg
		} else if p[1] < -maxLatDeg {
			p[1] = -maxLatDeg
		}
		out[i] = p
	}
	return out
}

// NightRing returns the closed ring bounding the region where the sun's
// geometric altitude is below sunAltDeg at time t. Use 0 for the classical
// terminator, SunriseAltitudeDeg (-0.833) for "the sun has set", and -6,
// -12, -18 for the twilight zones. sunAltDeg is clamped to [-89, 0];
// stepDeg is the sampling step in degrees (<= 0 selects 2°).
func NightRing(t time.Time, sunAltDeg, stepDeg float64) [][2]float64 {
	s := solarCoords(JulianDay(t))
	subLon := norm180((720 - utcMinutes(t) - s.eqTime) / 4)
	return nightRing(s.decl, subLon, sunAltDeg, stepDeg)
}

func nightRing(decl, subLon, sunAltDeg, stepDeg float64) [][2]float64 {
	a := sunAltDeg
	if a > 0 {
		a = 0
	} else if a < -89 {
		a = -89
	}
	if !(stepDeg > 0) {
		stepDeg = DefaultTerminatorStepDeg
	}
	if stepDeg < 0.1 {
		stepDeg = 0.1
	} else if stepDeg > 30 {
		stepDeg = 30
	}

	// Keep the boundary from passing exactly through a pole.
	const nudge = 1e-6
	if math.Abs(decl-a) < nudge {
		decl = a - nudge
	} else if math.Abs(decl+a) < nudge {
		decl = -a + nudge
	}
	northDark := decl < a  // north pole inside the cap
	southDark := decl > -a // south pole inside the cap

	if northDark || southDark {
		n := int(math.Ceil(360 / stepDeg))
		ring := make([][2]float64, 0, n+4)
		sinDec, cosDec := math.Sincos(decl * degToRad)
		c := sinD(a)
		for i := 0; i <= n; i++ {
			lon := -180 + 360*float64(i)/float64(n)
			// sin(alt) = A·sinφ + B·cosφ with A = sinδ, B = cosδ·cosH.
			// Solve A·sinφ + B·cosφ = c  =>  sin(φ+α) = c/R, α = atan2(B, A).
			B := cosDec * cosD(lon-subLon)
			R := math.Hypot(sinDec, B)
			alpha := math.Atan2(B, sinDec)
			q := c / R
			if q > 1 {
				q = 1
			} else if q < -1 {
				q = -1
			}
			as := math.Asin(q)
			phi := wrapPi(as - alpha)
			if phi < -math.Pi/2 || phi > math.Pi/2 {
				phi = wrapPi(math.Pi - as - alpha)
			}
			if phi > math.Pi/2 {
				phi = math.Pi / 2
			} else if phi < -math.Pi/2 {
				phi = -math.Pi / 2
			}
			ring = append(ring, [2]float64{lon, phi * radToDeg})
		}
		if northDark {
			// west→east along the boundary, then back over the north pole: CCW.
			ring = append(ring, [2]float64{180, 90}, [2]float64{-180, 90})
		} else {
			// east→west along the boundary, then back under the south pole: CCW.
			for i, j := 0, len(ring)-1; i < j; i, j = i+1, j-1 {
				ring[i], ring[j] = ring[j], ring[i]
			}
			ring = append(ring, [2]float64{-180, -90}, [2]float64{180, -90})
		}
		return append(ring, ring[0])
	}

	// No pole inside: sample the small circle around the anti-solar point.
	cLat := -decl
	cLon := norm180(subLon + 180)
	radius := (90 + a) * degToRad
	n := int(math.Ceil(360 / stepDeg))
	ring := make([][2]float64, 0, n+1)
	sinC, cosC := math.Sincos(cLat * degToRad)
	sinR, cosR := math.Sincos(radius)
	// Bearings run clockwise (N→E→S→W); iterate them backwards for a CCW ring.
	for i := 0; i < n; i++ {
		brg := -2 * math.Pi * float64(i) / float64(n)
		sinB, cosB := math.Sincos(brg)
		sinLat := sinC*cosR + cosC*sinR*cosB
		if sinLat > 1 {
			sinLat = 1
		} else if sinLat < -1 {
			sinLat = -1
		}
		dLon := math.Atan2(sinB*sinR*cosC, cosR-sinC*sinLat) * radToDeg // (-180,180]: continuous
		ring = append(ring, [2]float64{cLon + dLon, math.Asin(sinLat) * radToDeg})
	}
	return append(ring, ring[0])
}

func wrapPi(x float64) float64 {
	x = math.Mod(x+math.Pi, 2*math.Pi)
	if x < 0 {
		x += 2 * math.Pi
	}
	return x - math.Pi
}

// NightSide describes the dark side of the earth at one instant as nested rings.
type NightSide struct {
	TimeMs       int64        `json:"t"`           // Unix ms the rings are valid for
	SubsolarLat  float64      `json:"subsolarLat"` // where the sun is overhead
	SubsolarLon  float64      `json:"subsolarLon"`
	Sunset       [][2]float64 `json:"sunset"`       // sun below -0.833°: not day
	Civil        [][2]float64 `json:"civil"`        // below -6°: civil twilight over
	Nautical     [][2]float64 `json:"nautical"`     // below -12°
	Astronomical [][2]float64 `json:"astronomical"` // below -18°: full night
}

// NightSideAt returns the four nested night rings (see NightRing) and the
// sub-solar point for time t. Drawing the four polygons on top of each other
// with a low opacity each gives the usual graded day/night overlay.
func NightSideAt(t time.Time, stepDeg float64) NightSide {
	s := solarCoords(JulianDay(t))
	subLon := norm180((720 - utcMinutes(t) - s.eqTime) / 4)
	return NightSide{
		TimeMs:       t.UnixMilli(),
		SubsolarLat:  s.decl,
		SubsolarLon:  subLon,
		Sunset:       nightRing(s.decl, subLon, SunriseAltitudeDeg, stepDeg),
		Civil:        nightRing(s.decl, subLon, CivilAltitudeDeg, stepDeg),
		Nautical:     nightRing(s.decl, subLon, NauticalAltitudeDeg, stepDeg),
		Astronomical: nightRing(s.decl, subLon, AstronomicalAltitudeDeg, stepDeg),
	}
}

// GeoJSON returns the night rings as a GeoJSON FeatureCollection (as plain
// maps/slices ready for encoding/json): four Polygon features with the
// property "kind" = "sunset" | "civil" | "nautical" | "astronomical" and
// "sunAlt" = the altitude limit in degrees.
func (n NightSide) GeoJSON() map[string]any {
	feat := func(kind string, alt float64, ring [][2]float64) map[string]any {
		return map[string]any{
			"type":       "Feature",
			"properties": map[string]any{"kind": kind, "sunAlt": alt},
			"geometry": map[string]any{
				"type":        "Polygon",
				"coordinates": [][][2]float64{ring},
			},
		}
	}
	return map[string]any{
		"type": "FeatureCollection",
		"features": []map[string]any{
			feat("sunset", SunriseAltitudeDeg, n.Sunset),
			feat("civil", CivilAltitudeDeg, n.Civil),
			feat("nautical", NauticalAltitudeDeg, n.Nautical),
			feat("astronomical", AstronomicalAltitudeDeg, n.Astronomical),
		},
	}
}
