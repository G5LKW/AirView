package navdata

import (
	"context"
	"math"
	"strings"

	"airview/internal/geo"
)

// RunwayIndex is the read-only lookup a trajectory predictor or tracker needs
// to recognise approaches and associate flights with airports. *Region
// implements it; implementations must be safe for concurrent reads.
type RunwayIndex interface {
	// RunwaysNear appends the runway ends whose threshold lies within radiusKm.
	RunwaysNear(lat, lon, radiusKm float64, dst []RunwayEndInfo) []RunwayEndInfo
	// NearestAirport returns the nearest airport of at least minSize within maxKm.
	NearestAirport(lat, lon float64, minSize SizeClass, maxKm float64) (Airport, float64, bool)
	// AirportsNear appends airports of at least minSize within radiusKm (unordered).
	AirportsNear(lat, lon, radiusKm float64, minSize SizeClass, dst []Airport) []Airport
	// AirportByCode looks up ICAO / ident / IATA / GPS code.
	AirportByCode(code string) (Airport, bool)
}

// Region is an immutable in-memory airport + runway index for an area
// (typically 500-800 km around the receiver). Queries are allocation-free
// apart from growing the caller's dst slice. The returned Airport values share
// their Runways slices with the index: treat them as read-only.
type Region struct {
	airports []Airport
	ends     []RunwayEndInfo
	aptGrid  map[int32][]int32
	endGrid  map[int32][]int32
	codes    map[string]int32
}

const cellDeg = 0.5

func cellKey(lat, lon float64) int32 {
	la := int32(math.Floor((lat + 90) / cellDeg))
	lo := int32(math.Floor((geo.NormalizeLon(lon) + 180) / cellDeg))
	return la*1000 + lo
}

// NewRegion builds an index over airports (runways attached for runway queries).
func NewRegion(airports []Airport) *Region {
	r := &Region{airports: airports, aptGrid: map[int32][]int32{}, endGrid: map[int32][]int32{}, codes: map[string]int32{}}
	for i := range airports {
		a := &airports[i]
		k := cellKey(a.Lat, a.Lon)
		r.aptGrid[k] = append(r.aptGrid[k], int32(i))
		for _, c := range []string{a.Ident, a.GPS, a.IATA, a.ICAO} { // later (better) codes overwrite
			if c == "" {
				continue
			}
			c = strings.ToUpper(c)
			if j, ok := r.codes[c]; ok && airports[j].Size > a.Size && c != a.ICAO {
				continue
			}
			r.codes[c] = int32(i)
		}
		r.ends = RunwayEnds(a, r.ends)
	}
	for i := range r.ends {
		k := cellKey(r.ends[i].Lat, r.ends[i].Lon)
		r.endGrid[k] = append(r.endGrid[k], int32(i))
	}
	return r
}

// LoadRegion loads every airport of at least minSize within radiusKm of the
// centre (with runways) and indexes it. Rebuild when the receiver moves or
// after an import.
func (n *NavData) LoadRegion(ctx context.Context, lat, lon, radiusKm float64, minSize SizeClass) (*Region, error) {
	as, err := n.AirportsNear(ctx, lat, lon, radiusKm, minSize, 10000, true)
	if err != nil {
		return nil, err
	}
	for i := range as {
		as[i].DistKm = nil
	}
	return NewRegion(as), nil
}

// Len returns the number of indexed airports.
func (r *Region) Len() int { return len(r.airports) }

// Ends returns all indexed runway ends (read-only).
func (r *Region) Ends() []RunwayEndInfo { return r.ends }

// cells calls fn for every grid key covering the circle.
func cells(lat, lon, radiusKm float64, fn func(int32)) {
	dLat := radiusKm / 111.195
	c := math.Cos(geo.Rad(math.Min(89, math.Abs(lat)+dLat)))
	dLon := radiusKm / (111.195 * math.Max(c, 0.01))
	if dLon > 180 {
		dLon = 180
	}
	for la := math.Floor((lat - dLat) / cellDeg); la <= math.Floor((lat+dLat)/cellDeg); la++ {
		if la*cellDeg < -90 || la*cellDeg >= 90 {
			continue
		}
		n := int(math.Floor((lon+dLon)/cellDeg) - math.Floor((lon-dLon)/cellDeg))
		lo0 := math.Floor((lon - dLon) / cellDeg)
		for i := 0; i <= n && i < int(360/cellDeg); i++ {
			cLon := (lo0+float64(i))*cellDeg + cellDeg/2
			fn(cellKey(la*cellDeg+cellDeg/2, cLon))
		}
	}
}

// RunwaysNear implements RunwayIndex.
func (r *Region) RunwaysNear(lat, lon, radiusKm float64, dst []RunwayEndInfo) []RunwayEndInfo {
	if r == nil || !geo.ValidLatLon(lat, lon) || !(radiusKm > 0) {
		return dst
	}
	cells(lat, lon, radiusKm, func(k int32) {
		for _, i := range r.endGrid[k] {
			e := &r.ends[i]
			if geo.HaversineKm(lat, lon, e.Lat, e.Lon) <= radiusKm {
				dst = append(dst, *e)
			}
		}
	})
	return dst
}

// AirportsNear implements RunwayIndex.
func (r *Region) AirportsNear(lat, lon, radiusKm float64, minSize SizeClass, dst []Airport) []Airport {
	if r == nil || !geo.ValidLatLon(lat, lon) || !(radiusKm > 0) {
		return dst
	}
	cells(lat, lon, radiusKm, func(k int32) {
		for _, i := range r.aptGrid[k] {
			a := &r.airports[i]
			if a.Size >= minSize && geo.HaversineKm(lat, lon, a.Lat, a.Lon) <= radiusKm {
				dst = append(dst, *a)
			}
		}
	})
	return dst
}

// NearestAirport implements RunwayIndex.
func (r *Region) NearestAirport(lat, lon float64, minSize SizeClass, maxKm float64) (Airport, float64, bool) {
	if r == nil || !geo.ValidLatLon(lat, lon) || !(maxKm > 0) {
		return Airport{}, 0, false
	}
	best, bestD := int32(-1), math.Inf(1)
	cells(lat, lon, maxKm, func(k int32) {
		for _, i := range r.aptGrid[k] {
			a := &r.airports[i]
			if a.Size < minSize {
				continue
			}
			if d := geo.HaversineKm(lat, lon, a.Lat, a.Lon); d <= maxKm && d < bestD {
				best, bestD = i, d
			}
		}
	})
	if best < 0 {
		return Airport{}, 0, false
	}
	return r.airports[best], bestD, true
}

// AirportByCode implements RunwayIndex.
func (r *Region) AirportByCode(code string) (Airport, bool) {
	if r == nil {
		return Airport{}, false
	}
	i, ok := r.codes[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return Airport{}, false
	}
	return r.airports[i], true
}
