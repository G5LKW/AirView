package terrain

import (
	"math"

	"airview/internal/geo"
	"airview/internal/radio"
)

// Profile sampling limits.
const (
	minProfileSamples = 32
	maxProfileSamples = 4000
	// DefaultMinObstacleKm: terrain closer than this to the receive antenna
	// is ignored as an obstacle. DEM pixels around the site (30-150 m) are
	// too coarse to tell whether the mast stands above them; local clutter
	// (buildings, trees) is not in the DEM anyway.
	DefaultMinObstacleKm = 0.3
)

// ProfilePoint is one sample of a terrain profile.
type ProfilePoint struct {
	DistKm   float64 `json:"distKm"`   // from the start point
	Lat      float64 `json:"lat"`      //
	Lon      float64 `json:"lon"`      //
	TerrainM float64 `json:"terrainM"` // terrain elevation AMSL
	// BulgeM is the earth bulge d1·d2/(2·k·R) in metres: the terrain is
	// drawn at TerrainM + BulgeM above the straight chord between the ends.
	BulgeM float64 `json:"bulgeM"`
	// RayM is the height of the direct ray in the same (bulged) frame; it
	// is filled by LOS (0 in a bare profile).
	RayM float64 `json:"rayM"`
	// FresnelM is the first Fresnel-zone radius at 1090 MHz (filled by LOS).
	FresnelM float64 `json:"fresnelM"`
	NoData   bool    `json:"noData,omitempty"` // no elevation data (sea level assumed)
}

// Profile is a terrain profile along a great circle.
type Profile struct {
	FromLat    float64        `json:"fromLat"`
	FromLon    float64        `json:"fromLon"`
	ToLat      float64        `json:"toLat"`
	ToLon      float64        `json:"toLon"`
	DistKm     float64        `json:"distKm"`
	BearingDeg float64        `json:"bearingDeg"`
	K          float64        `json:"k"`
	NoData     int            `json:"noData"` // samples without elevation data
	Points     []ProfilePoint `json:"points"`
}

// autoSamples picks a sample count that resolves the source's pixels.
func autoSamples(src Source, lat, distKm float64) int {
	res := sourceResolution(src, lat)
	n := int(math.Ceil(distKm*1000/res)) + 1
	if n < minProfileSamples {
		n = minProfileSamples
	}
	if n > maxProfileSamples {
		n = maxProfileSamples
	}
	return n
}

// BuildProfile samples the terrain at n points (both ends included; n <= 1
// selects one sample per source pixel, 32..4000) along the great circle
// from (lat1, lon1) to (lat2, lon2) and adds the earth bulge for the
// effective-earth factor k (0 = 4/3).
func BuildProfile(src Source, lat1, lon1, lat2, lon2 float64, n int, k float64) Profile {
	k = geo.EffectiveK(k)
	o := newOrigin(lat1, lon1)
	dist, brg := o.inverse(lat2, lon2)
	if n <= 1 {
		n = autoSamples(src, lat1, dist)
	}
	if n > maxProfileSamples {
		n = maxProfileSamples
	}
	pr := Profile{FromLat: lat1, FromLon: lon1, ToLat: lat2, ToLon: lon2, DistKm: dist, BearingDeg: brg, K: k,
		Points: make([]ProfilePoint, n)}
	reM := k * geo.EarthRadiusKm * 1000
	dM := dist * 1000
	sb, cb := math.Sincos(brg * degToRad)
	for i := 0; i < n; i++ {
		f := float64(i) / float64(n-1)
		d := dist * f
		var la, lo float64
		switch i {
		case 0:
			la, lo = lat1, lon1
		case n - 1:
			la, lo = lat2, lon2
		default:
			sd, cd := math.Sincos(d / geo.EarthRadiusKm)
			la, lo = o.dest(sb, cb, sd, cd)
		}
		e, ok := src.ElevationM(la, lo)
		x := d * 1000
		pp := ProfilePoint{DistKm: d, Lat: la, Lon: lo, TerrainM: e, BulgeM: x * (dM - x) / (2 * reM), NoData: !ok}
		if !ok {
			pr.NoData++
		}
		pr.Points[i] = pp
	}
	return pr
}

// LOSInput describes a receiver → target path.
type LOSInput struct {
	RxLat float64 `json:"rxLat"`
	RxLon float64 `json:"rxLon"`
	// RxGroundM is the site ground elevation AMSL. It is used unless
	// GroundFromDEM is set (and the DEM has data at the site).
	RxGroundM     float64 `json:"rxGroundM"`
	GroundFromDEM bool    `json:"groundFromDem"`
	RxAGLM        float64 `json:"rxAglM"` // antenna height above ground
	TgtLat        float64 `json:"tgtLat"`
	TgtLon        float64 `json:"tgtLon"`
	TgtAltM       float64 `json:"tgtAltM"` // target altitude AMSL
	K             float64 `json:"k"`       // effective earth factor (0 = 4/3)
	// Samples along the path (0 = automatic from the source resolution).
	Samples int `json:"samples"`
	// MinObstacleKm: terrain closer than this to the receiver is ignored
	// (0 = DefaultMinObstacleKm, negative = 0).
	MinObstacleKm float64 `json:"minObstacleKm"`
	// MaxEdges limits the Deygout recursion: 1 = single principal edge,
	// 3 (default) = principal + one secondary edge each side.
	MaxEdges int `json:"maxEdges"`
	// Gas is the gaseous absorption model (nil = radio standard atmosphere).
	Gas *radio.GasModel `json:"-"`
	// KeepProfile returns the sampled profile in the result.
	KeepProfile bool `json:"keepProfile"`
}

// Obstruction is the terrain point that intrudes most into the direct ray.
type Obstruction struct {
	DistKm       float64 `json:"distKm"`       // from the receiver
	Lat          float64 `json:"lat"`          //
	Lon          float64 `json:"lon"`          //
	TerrainM     float64 `json:"terrainM"`     // terrain elevation AMSL
	AboveRayM    float64 `json:"aboveRayM"`    // height of terrain (+ bulge) above the ray
	ElevationDeg float64 `json:"elevationDeg"` // elevation angle of that point seen from the antenna
}

// LOSResult is the result of LOS.
type LOSResult struct {
	Clear        bool    `json:"clear"` // no terrain above the direct ray
	RxLat        float64 `json:"rxLat"`
	RxLon        float64 `json:"rxLon"`
	RxGroundM    float64 `json:"rxGroundM"`  // ground elevation used
	RxAntennaM   float64 `json:"rxAntennaM"` // antenna AMSL
	TgtLat       float64 `json:"tgtLat"`
	TgtLon       float64 `json:"tgtLon"`
	TgtAltM      float64 `json:"tgtAltM"`
	K            float64 `json:"k"`
	GroundKm     float64 `json:"groundKm"`
	SlantKm      float64 `json:"slantKm"`
	BearingDeg   float64 `json:"bearingDeg"`
	ElevationDeg float64 `json:"elevationDeg"` // elevation of the target seen from the antenna
	// Obstruction is the most obstructing point when the path is blocked
	// (null when clear).
	Obstruction *Obstruction `json:"obstruction"`
	// MinClearanceM is the smallest clearance of the ray over the terrain
	// (negative when blocked) and where it occurs.
	MinClearanceM      float64 `json:"minClearanceM"`
	MinClearanceDistKm float64 `json:"minClearanceDistKm"`
	// FresnelRatio is the smallest clearance / first-Fresnel-zone radius
	// along the path (>= 0.6 is the usual "free-space" criterion).
	FresnelRatio   float64 `json:"fresnelRatio"`
	Fresnel60Clear bool    `json:"fresnel60Clear"`
	DiffractionDB  float64 `json:"diffractionDb"` // Deygout knife-edge loss
	Edges          []Edge  `json:"edges"`
	FSPLDB         float64 `json:"fsplDb"`
	GasDB          float64 `json:"gasDb"`
	TotalLossDB    float64 `json:"totalLossDb"` // FSPL + diffraction + gases
	Samples        int     `json:"samples"`
	NoDataSamples  int     `json:"noDataSamples"`
	// Profile is the sampled terrain profile (only with KeepProfile).
	Profile []ProfilePoint `json:"profile,omitempty"`
}

// LOS analyses the radio path from a receive antenna to a target (aircraft)
// at 1090 MHz over the terrain of src: line of sight, the obstruction point,
// first-Fresnel-zone clearance, knife-edge diffraction (ITU-R P.526 J(v),
// Deygout for several edges) and the total path loss FSPL + diffraction +
// gaseous absorption.
func LOS(src Source, in LOSInput) LOSResult {
	k := geo.EffectiveK(in.K)
	ground := in.RxGroundM
	if in.GroundFromDEM {
		if e, ok := src.ElevationM(in.RxLat, in.RxLon); ok {
			ground = e
		}
	}
	h0 := ground + in.RxAGLM
	h1 := in.TgtAltM
	pr := BuildProfile(src, in.RxLat, in.RxLon, in.TgtLat, in.TgtLon, in.Samples, k)
	res := LOSResult{
		RxLat: in.RxLat, RxLon: in.RxLon, RxGroundM: ground, RxAntennaM: h0,
		TgtLat: in.TgtLat, TgtLon: in.TgtLon, TgtAltM: h1, K: k,
		GroundKm: pr.DistKm, BearingDeg: pr.BearingDeg,
		Samples: len(pr.Points), NoDataSamples: pr.NoData, Edges: []Edge{},
		FresnelRatio: 0,
	}
	res.SlantKm = geo.SlantRangeKm(pr.DistKm, h0, h1)
	res.ElevationDeg = geo.ElevationDeg(pr.DistKm, h0, h1, k)
	res.FSPLDB = radio.FSPL1090DB(math.Max(res.SlantKm, 1e-3))
	if in.Gas != nil {
		res.GasDB = in.Gas.PathLossDB(pr.DistKm, h0, h1, k)
	} else {
		res.GasDB = radio.GaseousLossDB(pr.DistKm, h0, h1, k)
	}

	minObs := in.MinObstacleKm
	if minObs == 0 {
		minObs = DefaultMinObstacleKm
	} else if minObs < 0 {
		minObs = 0
	}
	n := len(pr.Points)
	dM := pr.DistKm * 1000
	lambda := radio.WavelengthM
	x := make([]float64, n)
	h := make([]float64, n)
	minClear, minClearAt := math.Inf(1), -1
	minRatio := math.Inf(1)
	worst, worstAbove := -1, math.Inf(-1)
	for i := range pr.Points {
		p := &pr.Points[i]
		x[i] = p.DistKm * 1000
		p.RayM = h0
		if dM > 0 {
			p.RayM = h0 + (h1-h0)*x[i]/dM
		}
		p.FresnelM = FresnelRadiusM(x[i], dM-x[i], lambda)
		switch {
		case i == 0:
			h[i] = h0
			continue
		case i == n-1:
			h[i] = h1
			continue
		}
		t := p.TerrainM + p.BulgeM
		if p.DistKm < minObs {
			h[i] = -1e9 // ignored near the antenna
			continue
		}
		h[i] = t
		c := p.RayM - t
		if c < minClear {
			minClear, minClearAt = c, i
		}
		if p.FresnelM > 0 {
			if r := c / p.FresnelM; r < minRatio {
				minRatio = r
			}
		}
		if -c > worstAbove {
			worst, worstAbove = i, -c
		}
	}
	if minClearAt >= 0 {
		res.MinClearanceM = minClear
		res.MinClearanceDistKm = pr.Points[minClearAt].DistKm
	} else {
		res.MinClearanceM = math.Max(0, math.Min(h0, h1))
	}
	if math.IsInf(minRatio, 1) {
		minRatio = 10
	}
	res.FresnelRatio = minRatio
	res.Fresnel60Clear = minRatio >= 0.6
	res.Clear = worst < 0 || worstAbove <= 0
	if !res.Clear {
		p := pr.Points[worst]
		res.Obstruction = &Obstruction{
			DistKm: p.DistKm, Lat: p.Lat, Lon: p.Lon, TerrainM: p.TerrainM, AboveRayM: worstAbove,
			ElevationDeg: geo.ElevationDeg(p.DistKm, h0, p.TerrainM, k),
		}
	}
	maxLevel := 1
	switch {
	case in.MaxEdges == 1:
		maxLevel = 0
	case in.MaxEdges > 3:
		maxLevel = 2
	}
	if n >= 3 && dM > 0 {
		var loss float64
		loss, res.Edges = deygout(x, h, 0, n-1, 0, maxLevel, lambda, res.Edges)
		res.DiffractionDB = loss
		for i := range res.Edges {
			e := &res.Edges[i]
			p := pr.Points[e.Index]
			e.DistKm, e.Lat, e.Lon, e.TerrainM = p.DistKm, p.Lat, p.Lon, p.TerrainM
		}
	}
	res.TotalLossDB = res.FSPLDB + res.DiffractionDB + res.GasDB
	if in.KeepProfile {
		res.Profile = pr.Points
	}
	sanitizeLOS(&res)
	return res
}

func sanitizeLOS(r *LOSResult) {
	for _, p := range []*float64{&r.RxGroundM, &r.RxAntennaM, &r.GroundKm, &r.SlantKm, &r.BearingDeg, &r.ElevationDeg,
		&r.MinClearanceM, &r.MinClearanceDistKm, &r.FresnelRatio, &r.DiffractionDB, &r.FSPLDB, &r.GasDB, &r.TotalLossDB} {
		*p = finite(*p, 0)
	}
	for i := range r.Edges {
		r.Edges[i].V = finite(r.Edges[i].V, 0)
		r.Edges[i].HeightM = finite(r.Edges[i].HeightM, 0)
	}
}
