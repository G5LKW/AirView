package terrain

import "math"

// Synthetic terrain sources: used by the tests and handy for demo mode
// (internal/sim) and for checking the UI without network access.

// FlatSource is a flat terrain at a constant elevation.
type FlatSource struct {
	ElevM float64
	// ResM is the reported resolution (default 100 m).
	ResM float64
}

// ElevationM implements Source.
func (f FlatSource) ElevationM(lat, lon float64) (float64, bool) { return f.ElevM, true }

// ResolutionM implements Resolver.
func (f FlatSource) ResolutionM(lat float64) float64 {
	if f.ResM > 0 {
		return f.ResM
	}
	return 100
}

// ConeSource is a conical mountain on otherwise flat terrain: the elevation
// falls linearly from PeakM at (Lat, Lon) to BaseM at RadiusKm.
type ConeSource struct {
	Lat, Lon float64
	PeakM    float64
	BaseM    float64
	RadiusKm float64
	ResM     float64
}

// ElevationM implements Source.
func (c ConeSource) ElevationM(lat, lon float64) (float64, bool) {
	d := sphereKm(c.Lat, c.Lon, lat, lon)
	if !(c.RadiusKm > 0) || d >= c.RadiusKm {
		return c.BaseM, true
	}
	return c.BaseM + (c.PeakM-c.BaseM)*(1-d/c.RadiusKm), true
}

// ResolutionM implements Resolver.
func (c ConeSource) ResolutionM(lat float64) float64 {
	if c.ResM > 0 {
		return c.ResM
	}
	return 100
}

// ArcRidgeSource is a ridge shaped as a ring segment around a centre point
// (normally the station): every point whose great-circle distance from
// (Lat, Lon) is in [DistKm, DistKm+WidthKm] and whose bearing from it is
// within HalfAngleDeg of BearingDeg has elevation HeightM; everything else is
// at BaseM. HalfAngleDeg >= 180 makes a complete ring wall.
type ArcRidgeSource struct {
	Lat, Lon     float64
	BearingDeg   float64
	HalfAngleDeg float64
	DistKm       float64
	WidthKm      float64
	HeightM      float64
	BaseM        float64
	ResM         float64
}

// ElevationM implements Source.
func (r ArcRidgeSource) ElevationM(lat, lon float64) (float64, bool) {
	o := newOrigin(r.Lat, r.Lon)
	d, brg := o.inverse(lat, lon)
	if d < r.DistKm || d > r.DistKm+r.WidthKm {
		return r.BaseM, true
	}
	if r.HalfAngleDeg < 180 {
		diff := math.Abs(math.Mod(brg-r.BearingDeg+540, 360) - 180)
		if diff > r.HalfAngleDeg {
			return r.BaseM, true
		}
	}
	return r.HeightM, true
}

// ResolutionM implements Resolver.
func (r ArcRidgeSource) ResolutionM(lat float64) float64 {
	if r.ResM > 0 {
		return r.ResM
	}
	return 100
}

// NoDataSource never has data (every point reads as sea level, ok=false).
type NoDataSource struct{}

// ElevationM implements Source.
func (NoDataSource) ElevationM(lat, lon float64) (float64, bool) { return 0, false }
