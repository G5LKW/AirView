package terrain

import (
	"math"

	"airview/internal/geo"
	"airview/internal/radio"
)

// maxPanoramaBins bounds the distance resolution kept per ray (memory:
// rays × bins × 12 bytes, ≈ 4.4 MB for 720 × 512).
const maxPanoramaBins = 512

// noTerrainDeg marks "no terrain considered yet" in the running maximum.
const noTerrainDeg = -90.0

// Panorama is the terrain horizon of one antenna: for every bearing the
// running maximum terrain elevation angle as a function of distance. It is
// derived from a PolarTerrain for one antenna height and k, is immutable and
// safe for concurrent use. Its Visible / Forecast methods are the cheap
// per-aircraft helpers (no allocation, a few hundred ns).
type Panorama struct {
	Lat           float64
	Lon           float64
	AntennaM      float64 // antenna AMSL
	K             float64
	StepDeg       float64
	SampleKm      float64
	BinKm         float64
	RadiusKm      float64
	MinObstacleKm float64

	rays, bins int
	o          origin
	binEnd     []float64 // km, per bin
	cumAng     []float32 // [ray*bins+b]: max terrain elevation angle over bins 0..b (deg)
	cumDist    []float32 // distance (km) of that maximum; 0 = none
	floor      []float32 // min visible altitude AMSL (m) at binEnd[b]; +Inf = never

	skyAng, skyDist, skyElev    []float32 // per ray: overall skyline
	nearDist, nearAng, nearElev []float32 // per ray: nearest terrain above the horizontal (-1 = none)

	dipDeg    float64 // elevation angle of the smooth sea-level horizon (<= 0)
	horizonKm float64 // smooth-earth horizon distance of the antenna
	noData    float64 // fraction of samples without DEM data
}

// NewPanorama derives the panorama of an antenna at antennaM metres AMSL from
// the polar terrain pt, for effective-earth factor k (0 = 4/3). Terrain
// closer than minObstacleKm (0 = DefaultMinObstacleKm, < 0 = none) is ignored.
func NewPanorama(pt *PolarTerrain, antennaM, k, minObstacleKm float64) *Panorama {
	k = geo.EffectiveK(k)
	if minObstacleKm == 0 {
		minObstacleKm = DefaultMinObstacleKm
	} else if minObstacleKm < 0 {
		minObstacleKm = 0
	}
	ns := pt.samples
	m := (ns + maxPanoramaBins - 1) / maxPanoramaBins
	if m < 1 {
		m = 1
	}
	bins := (ns + m - 1) / m
	p := &Panorama{
		Lat: pt.Spec.Lat, Lon: pt.Spec.Lon, AntennaM: antennaM, K: k,
		StepDeg: pt.Spec.BearingStepDeg, SampleKm: pt.Spec.SampleKm, BinKm: float64(m) * pt.Spec.SampleKm,
		RadiusKm: float64(ns) * pt.Spec.SampleKm, MinObstacleKm: minObstacleKm,
		rays: pt.rays, bins: bins, o: newOrigin(pt.Spec.Lat, pt.Spec.Lon),
		binEnd: make([]float64, bins),
		cumAng: make([]float32, pt.rays*bins), cumDist: make([]float32, pt.rays*bins), floor: make([]float32, pt.rays*bins),
		skyAng: make([]float32, pt.rays), skyDist: make([]float32, pt.rays), skyElev: make([]float32, pt.rays),
		nearDist: make([]float32, pt.rays), nearAng: make([]float32, pt.rays), nearElev: make([]float32, pt.rays),
		noData: pt.NoDataFraction(),
	}
	for b := 0; b < bins; b++ {
		end := (b + 1) * m
		if end > ns {
			end = ns
		}
		p.binEnd[b] = float64(end) * pt.Spec.SampleKm
	}
	p.horizonKm = geo.HorizonKm(antennaM, k)
	if antennaM > 0 {
		p.dipDeg = geo.ElevationDeg(p.horizonKm, antennaM, 0, k)
	}
	for i := 0; i < pt.rays; i++ {
		row := pt.elev[i*ns : (i+1)*ns]
		run, runD, runE := noTerrainDeg, 0.0, 0.0
		nearD, nearA, nearE := -1.0, 0.0, 0.0
		j := 0
		for b := 0; b < bins; b++ {
			end := (b + 1) * m
			if end > ns {
				end = ns
			}
			for ; j < end; j++ {
				d := float64(j+1) * pt.Spec.SampleKm
				if d < minObstacleKm {
					continue
				}
				e := float64(row[j])
				a := geo.ElevationDeg(d, antennaM, e, k)
				if a > run {
					run, runD, runE = a, d, e
				}
				if nearD < 0 && a > 0 {
					nearD, nearA, nearE = d, a, e
				}
			}
			idx := i*bins + b
			p.cumAng[idx] = float32(run)
			p.cumDist[idx] = float32(runD)
			p.floor[idx] = float32(altitudeOnRay(p.binEnd[b], antennaM, run, k))
		}
		p.skyAng[i], p.skyDist[i], p.skyElev[i] = float32(run), float32(runD), float32(runE)
		p.nearDist[i], p.nearAng[i], p.nearElev[i] = float32(nearD), float32(nearA), float32(nearE)
	}
	return p
}

// altitudeOnRay is the altitude AMSL (m, >= 0) of the straight effective-earth
// ray leaving an antenna at antennaM with elevation epsDeg, at ground
// distance dKm. +Inf when the ray never gets there (beyond 90° of arc).
// Unlike geo.AltitudeAtElevationM it ignores where the ray touches the earth:
// the ray grazing a terrain point continues beyond it.
func altitudeOnRay(dKm, antennaM, epsDeg, k float64) float64 {
	if epsDeg <= noTerrainDeg {
		return 0
	}
	re := k * geo.EarthRadiusKm
	r1 := re + antennaM/1000
	eps := epsDeg * degToRad
	theta := dKm / re
	c := math.Cos(eps + theta)
	if eps+theta >= math.Pi/2 || c <= 0 {
		return math.Inf(1)
	}
	h := (r1*math.Cos(eps)/c - re) * 1000
	if h < 0 {
		return 0
	}
	return h
}

// Rays returns the number of bearings.
func (p *Panorama) Rays() int { return p.rays }

// Bins returns the number of distance bins per bearing.
func (p *Panorama) Bins() int { return p.bins }

// NoDataFraction is the fraction of terrain samples without DEM data.
func (p *Panorama) NoDataFraction() float64 { return p.noData }

// SmoothHorizon returns the smooth-earth horizon distance and its elevation
// angle (the sea-level "dip", <= 0) for the antenna.
func (p *Panorama) SmoothHorizon() (km, dipDeg float64) { return p.horizonKm, p.dipDeg }

// Visibility is the per-aircraft terrain visibility verdict.
type Visibility struct {
	Known        bool    `json:"known"`        // a panorama was available
	Visible      bool    `json:"visible"`      // above the terrain horizon
	MarginDeg    float64 `json:"marginDeg"`    // elevation − terrain mask angle (+ = above)
	ElevationDeg float64 `json:"elevationDeg"` // aircraft elevation seen from the antenna
	SkylineDeg   float64 `json:"skylineDeg"`   // terrain mask angle at that bearing, up to that distance
	FloorFt      float64 `json:"floorFt"`      // lowest visible altitude there (ft AMSL)
	DistKm       float64 `json:"distKm"`
	BearingDeg   float64 `json:"bearingDeg"`
	BeyondRange  bool    `json:"beyondRange"` // farther than the analysed radius (terrain beyond unknown)
	AntennaID    string  `json:"antennaId,omitempty"`
}

// maskAt returns the terrain mask angle (deg) for ground distance d on
// bearing brg, considering terrain strictly before d (complete bins), and
// whether d is beyond the analysed radius.
func (p *Panorama) maskAt(brg, d float64) (float64, bool) {
	r := brg / p.StepDeg
	fi := math.Floor(r)
	f := r - fi
	i0 := int(fi) % p.rays
	if i0 < 0 {
		i0 += p.rays
	}
	i1 := (i0 + 1) % p.rays
	nb := int(d / p.BinKm)
	beyond := d > p.RadiusKm
	if nb > p.bins {
		nb = p.bins
	}
	if nb <= 0 {
		return noTerrainDeg, beyond
	}
	b := nb - 1
	a := lerpAng(float64(p.cumAng[i0*p.bins+b]), float64(p.cumAng[i1*p.bins+b]), f)
	if beyond && d > p.horizonKm && a < p.dipDeg {
		a = p.dipDeg
	}
	return a, beyond
}

// Visible reports whether an aircraft at (lat, lon, altFt AMSL) is above the
// terrain horizon of this antenna. Cheap enough for every aircraft at 1 Hz;
// does not allocate.
func (p *Panorama) Visible(lat, lon, altFt float64) Visibility {
	if p == nil || math.IsNaN(lat) || math.IsNaN(lon) || math.IsNaN(altFt) {
		return Visibility{}
	}
	d, brg := p.o.inverse(lat, lon)
	altM := altFt * geo.MetresPerFoot
	mask, beyond := p.maskAt(brg, d)
	v := Visibility{Known: true, DistKm: d, BearingDeg: brg, BeyondRange: beyond, SkylineDeg: mask}
	if d < 1e-6 {
		v.Visible, v.ElevationDeg, v.MarginDeg = true, 90, 180
		if altM < p.AntennaM {
			v.ElevationDeg = -90
		}
		return v
	}
	v.ElevationDeg = geo.ElevationDeg(d, p.AntennaM, altM, p.K)
	v.MarginDeg = v.ElevationDeg - mask
	v.Visible = v.MarginDeg > 0
	v.FloorFt = finite(altitudeOnRay(d, p.AntennaM, mask, p.K)*geo.FeetPerMetre, 1e6)
	return v
}

// MarginDeg is Visible reduced to the elevation margin above the terrain
// horizon in degrees (+ = visible). NaN input gives NaN.
func (p *Panorama) MarginDeg(lat, lon, altFt float64) float64 {
	if p == nil {
		return math.NaN()
	}
	v := p.Visible(lat, lon, altFt)
	if !v.Known {
		return math.NaN()
	}
	return v.MarginDeg
}

// PathPoint is one predicted future position.
type PathPoint struct {
	TMs   int64   `json:"t"` // Unix ms
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	AltFt float64 `json:"altFt"`
}

// VisibilityForecast tells when an aircraft is expected to disappear behind
// or rise above the terrain horizon along a predicted path.
type VisibilityForecast struct {
	Known      bool    `json:"known"`
	VisibleNow bool    `json:"visibleNow"` // at the first path point
	MarginDeg  float64 `json:"marginDeg"`  // at the first path point
	// LostAtMs is the first predicted time the aircraft drops below the
	// terrain horizon (0 = not within the path).
	LostAtMs int64 `json:"lostAtMs"`
	// AcquiredAtMs is the first predicted time it rises above it (0 = not
	// within the path).
	AcquiredAtMs int64 `json:"acquiredAtMs"`
}

// Forecast evaluates a predicted path (ascending times; the first point is
// "now") and interpolates the times at which the aircraft crosses the terrain
// horizon. No allocation.
func (p *Panorama) Forecast(path []PathPoint) VisibilityForecast {
	return forecast(path, func(pt PathPoint) (float64, bool) {
		v := p.Visible(pt.Lat, pt.Lon, pt.AltFt)
		return v.MarginDeg, v.Known
	})
}

func forecast(path []PathPoint, margin func(PathPoint) (float64, bool)) VisibilityForecast {
	var f VisibilityForecast
	if len(path) == 0 {
		return f
	}
	m0, ok := margin(path[0])
	if !ok {
		return f
	}
	f.Known, f.MarginDeg, f.VisibleNow = true, m0, m0 > 0
	prevM, prevT := m0, path[0].TMs
	for _, pt := range path[1:] {
		m, ok := margin(pt)
		if !ok || math.IsNaN(m) {
			continue
		}
		if (prevM > 0) != (m > 0) {
			frac := 0.5
			if prevM != m {
				frac = prevM / (prevM - m)
			}
			t := prevT + int64(math.Round(float64(pt.TMs-prevT)*frac))
			if prevM > 0 && f.LostAtMs == 0 {
				f.LostAtMs = t
			} else if prevM <= 0 && f.AcquiredAtMs == 0 {
				f.AcquiredAtMs = t
			}
			if f.LostAtMs != 0 && f.AcquiredAtMs != 0 {
				break
			}
		}
		prevM, prevT = m, pt.TMs
	}
	return f
}

// RangeKm returns the outermost distance on ray i at which an aircraft at
// altM metres AMSL is above the terrain horizon, and whether that reaches the
// analysed radius (capped).
func (p *Panorama) RangeKm(i int, altM float64) (km float64, capped bool) {
	row := p.floor[i*p.bins : (i+1)*p.bins]
	for b := p.bins - 1; b >= 0; b-- {
		fb := float64(row[b])
		if fb > altM {
			continue
		}
		if b == p.bins-1 {
			return p.binEnd[b], true
		}
		fn := float64(row[b+1])
		if math.IsInf(fn, 1) || fn <= fb {
			return p.binEnd[b], false
		}
		return p.binEnd[b] + (altM-fb)/(fn-fb)*(p.binEnd[b+1]-p.binEnd[b]), false
	}
	return 0, false
}

// Horizon is the terrain skyline around the antenna (JSON-ready).
type Horizon struct {
	AntennaID        string  `json:"antennaId,omitempty"`
	AntennaM         float64 `json:"antennaM"` // antenna AMSL
	K                float64 `json:"k"`
	StepDeg          float64 `json:"stepDeg"`
	RadiusKm         float64 `json:"radiusKm"`
	MinObstacleKm    float64 `json:"minObstacleKm"`
	SmoothDipDeg     float64 `json:"smoothDipDeg"`     // elevation of the sea-level horizon
	SmoothHorizonKm  float64 `json:"smoothHorizonKm"`  // its distance
	MeanSkylineDeg   float64 `json:"meanSkylineDeg"`   //
	MaxSkylineDeg    float64 `json:"maxSkylineDeg"`    //
	MaxSkylineBrgDeg float64 `json:"maxSkylineBrgDeg"` //
	// Per bearing (index i ↔ BearingDeg[i] = i·StepDeg):
	BearingDeg   []float64 `json:"bearingDeg"`
	ElevationDeg []float64 `json:"elevationDeg"` // skyline elevation angle
	DistanceKm   []float64 `json:"distanceKm"`   // distance of the limiting obstacle (0 = none)
	TerrainM     []float64 `json:"terrainM"`     // its terrain elevation AMSL
	// NearestObstacleKm is the nearest terrain rising above the antenna's
	// horizontal (elevation > 0°), -1 = none within the radius.
	NearestObstacleKm  []float64 `json:"nearestObstacleKm"`
	NearestObstacleDeg []float64 `json:"nearestObstacleDeg"`
	NoDataFraction     float64   `json:"noDataFraction"`
}

// Horizon exports the skyline.
func (p *Panorama) Horizon() Horizon {
	h := Horizon{
		AntennaM: round(p.AntennaM, 2), K: round(p.K, 4), StepDeg: p.StepDeg, RadiusKm: round(p.RadiusKm, 3),
		MinObstacleKm: p.MinObstacleKm, SmoothDipDeg: round(p.dipDeg, 4), SmoothHorizonKm: round(p.horizonKm, 2),
		BearingDeg: make([]float64, p.rays), ElevationDeg: make([]float64, p.rays), DistanceKm: make([]float64, p.rays),
		TerrainM: make([]float64, p.rays), NearestObstacleKm: make([]float64, p.rays), NearestObstacleDeg: make([]float64, p.rays),
		NoDataFraction: round(p.noData, 5),
	}
	sum, mx, mxB := 0.0, math.Inf(-1), 0.0
	for i := 0; i < p.rays; i++ {
		brg := float64(i) * p.StepDeg
		a := float64(p.skyAng[i])
		h.BearingDeg[i] = round(brg, 4)
		h.ElevationDeg[i] = round(a, 4)
		h.DistanceKm[i] = round(float64(p.skyDist[i]), 3)
		h.TerrainM[i] = round(float64(p.skyElev[i]), 1)
		h.NearestObstacleKm[i] = round(float64(p.nearDist[i]), 3)
		h.NearestObstacleDeg[i] = round(float64(p.nearAng[i]), 4)
		sum += a
		if a > mx {
			mx, mxB = a, brg
		}
	}
	h.MeanSkylineDeg = round(sum/float64(p.rays), 4)
	h.MaxSkylineDeg = round(mx, 4)
	h.MaxSkylineBrgDeg = mxB
	return h
}

// AltRange is the line-of-sight (or radio) range around the compass for one
// flight altitude.
type AltRange struct {
	AltFt    float64   `json:"altFt"` // AMSL
	AltM     float64   `json:"altM"`
	SmoothKm float64   `json:"smoothKm"` // smooth-earth line-of-sight range
	RangeKm  []float64 `json:"rangeKm"`  // per bearing (Horizon.BearingDeg)
	Capped   int       `json:"capped"`   // bearings that reach the analysed radius
	MinKm    float64   `json:"minKm"`
	MaxKm    float64   `json:"maxKm"`
	MeanKm   float64   `json:"meanKm"`
	AreaKm2  float64   `json:"areaKm2"`
}

// LOSRanges computes the terrain line-of-sight range per bearing for each
// altitude (ft AMSL).
func (p *Panorama) LOSRanges(altsFt []float64) []AltRange {
	out := make([]AltRange, len(altsFt))
	for a, ft := range altsFt {
		altM := ft * geo.MetresPerFoot
		r := AltRange{AltFt: ft, AltM: round(altM, 1), SmoothKm: round(radio.LineOfSightKm(p.AntennaM, altM, p.K), 3),
			RangeKm: make([]float64, p.rays)}
		for i := 0; i < p.rays; i++ {
			km, capped := p.RangeKm(i, altM)
			r.RangeKm[i] = km
			if capped {
				r.Capped++
			}
		}
		finishRange(&r, p.StepDeg)
		out[a] = r
	}
	return out
}

// finishRange fills the summary fields and rounds the arrays.
func finishRange(r *AltRange, stepDeg float64) {
	n := len(r.RangeKm)
	if n == 0 {
		return
	}
	mn, mx, sum, area := math.Inf(1), 0.0, 0.0, 0.0
	dth := stepDeg * degToRad
	for i, v := range r.RangeKm {
		v = finite(v, 0)
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
		sum += v
		area += 0.5 * v * v * dth
		r.RangeKm[i] = round(v, 3)
	}
	r.MinKm, r.MaxKm, r.MeanKm, r.AreaKm2 = round(mn, 3), round(mx, 3), round(sum/float64(n), 3), round(area, 1)
}

// FloorM returns the minimum visible altitude (m AMSL) at bearing brg and
// distance d (conservative: terrain up to the end of d's bin) and whether d
// is inside the analysed radius.
func (p *Panorama) FloorM(brg, d float64) (float64, bool) {
	if d > p.RadiusKm {
		return math.Inf(1), false
	}
	b := int(d / p.BinKm)
	if b >= p.bins {
		b = p.bins - 1
	}
	r := brg / p.StepDeg
	fi := math.Floor(r)
	f := r - fi
	i0 := int(fi) % p.rays
	if i0 < 0 {
		i0 += p.rays
	}
	i1 := (i0 + 1) % p.rays
	a := lerpAng(float64(p.cumAng[i0*p.bins+b]), float64(p.cumAng[i1*p.bins+b]), f)
	return altitudeOnRay(d, p.AntennaM, a, p.K), true
}

// lerpAng interpolates two mask angles; "no terrain" on one side yields the other.
func lerpAng(a0, a1, f float64) float64 {
	if a0 <= noTerrainDeg {
		return a1
	}
	if a1 <= noTerrainDeg {
		return a0
	}
	return a0 + (a1-a0)*f
}
