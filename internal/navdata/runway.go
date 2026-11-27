package navdata

import (
	"math"
	"strconv"
	"strings"

	"airview/internal/geo"
)

// RunwayEndInfo is one usable landing/take-off direction of a runway, in a
// flat, hot-path friendly form (all values finite).
type RunwayEndInfo struct {
	AirportID    int64   `json:"airportId"`
	AirportIdent string  `json:"airportIdent"`
	AirportCode  string  `json:"airportCode"` // ICAO if known
	Runway       string  `json:"runway"`      // end ident, e.g. "27L"
	Lat          float64 `json:"lat"`         // landing threshold (displaced threshold applied)
	Lon          float64 `json:"lon"`
	EndLat       float64 `json:"endLat"` // physical start of this direction's pavement
	EndLon       float64 `json:"endLon"`
	HeadingT     float64 `json:"headingT"` // degrees true, direction of travel when landing on it
	HeadingSrc   string  `json:"headingSrc"`
	ElevFt       float64 `json:"elevFt"` // threshold elevation (airport elevation fallback, 0 unknown)
	LengthFt     float64 `json:"lengthFt"`
	WidthFt      float64 `json:"widthFt"`
}

// RunwayEnds appends the usable runway ends of a (with runways attached) to
// dst. Closed runways and ends without a position are skipped. Heading source:
// "published" (le/he_heading_degT), "geometry" (bearing between both ends) or
// "ident" (designator x 10, magnetic - approximate).
func RunwayEnds(a *Airport, dst []RunwayEndInfo) []RunwayEndInfo {
	for i := range a.Runways {
		r := &a.Runways[i]
		if r.Closed {
			continue
		}
		for side := 0; side < 2; side++ {
			this, other := &r.LE, &r.HE
			if side == 1 {
				this, other = &r.HE, &r.LE
			}
			if this.Lat == nil || this.Lon == nil {
				continue
			}
			e := RunwayEndInfo{AirportID: a.ID, AirportIdent: a.Ident, AirportCode: a.Code(), Runway: this.Ident,
				EndLat: *this.Lat, EndLon: *this.Lon, ElevFt: a.elevFt()}
			if this.ElevFt != nil {
				e.ElevFt = float64(*this.ElevFt)
			}
			if r.LengthFt != nil {
				e.LengthFt = float64(*r.LengthFt)
			}
			if r.WidthFt != nil {
				e.WidthFt = float64(*r.WidthFt)
			}
			switch {
			case this.HeadingT != nil:
				e.HeadingT, e.HeadingSrc = geo.NormalizeDeg(*this.HeadingT), "published"
			case other.Lat != nil && other.Lon != nil:
				e.HeadingT, e.HeadingSrc = geo.InitialBearing(*this.Lat, *this.Lon, *other.Lat, *other.Lon), "geometry"
			default:
				h, ok := headingFromIdent(this.Ident)
				if !ok {
					continue
				}
				e.HeadingT, e.HeadingSrc = h, "ident"
			}
			e.Lat, e.Lon = e.EndLat, e.EndLon
			if this.DisplacedFt != nil && *this.DisplacedFt > 0 && *this.DisplacedFt < 20000 {
				e.Lat, e.Lon = geo.Destination(e.EndLat, e.EndLon, e.HeadingT, geo.FeetToMetres(float64(*this.DisplacedFt))/1000)
			}
			if e.LengthFt == 0 && other.Lat != nil && other.Lon != nil {
				e.LengthFt = geo.MetresToFeet(geo.HaversineKm(*this.Lat, *this.Lon, *other.Lat, *other.Lon) * 1000)
			}
			dst = append(dst, e)
		}
	}
	return dst
}

// headingFromIdent turns "09L" into 90 (magnetic, approximate).
func headingFromIdent(id string) (float64, bool) {
	id = strings.TrimRight(strings.ToUpper(id), "LRCWGSUTX")
	n, err := strconv.Atoi(id)
	if err != nil || n < 1 || n > 36 {
		return 0, false
	}
	return float64(n * 10 % 360), true
}

// ApproachFit describes how well an aircraft position fits the final
// approach to a runway end.
type ApproachFit struct {
	End           RunwayEndInfo `json:"end"`
	DistKm        float64       `json:"distKm"`        // along the extended centreline to the threshold
	LateralKm     float64       `json:"lateralKm"`     // signed, + = right of the centreline seen from the approaching aircraft
	TrackDiffDeg  float64       `json:"trackDiffDeg"`  // aircraft track minus runway heading
	HeightAboveFt float64       `json:"heightAboveFt"` // above threshold elevation
	GlideDeg      float64       `json:"glideDeg"`      // elevation angle seen from the threshold
	Score         float64       `json:"score"`         // lower is better; < 1 is a good fit
}

// Approach tolerances.
const (
	ApproachMaxKm       = 30.0 // ~16 NM final
	approachTrackTolDeg = 30.0
)

// MatchApproach returns the runway end among ends that the aircraft at
// (lat, lon, altFt, trackDeg) is most plausibly established on final for.
// altFt or trackDeg may be NaN (unknown; the check is then skipped).
// Allocation-free.
func MatchApproach(ends []RunwayEndInfo, lat, lon, altFt, trackDeg float64) (ApproachFit, bool) {
	var best ApproachFit
	found := false
	for i := range ends {
		e := &ends[i]
		d, brg := geo.DistanceBearing(e.Lat, e.Lon, lat, lon)
		if d > ApproachMaxKm || d < 0.05 {
			continue
		}
		// The aircraft must be on the approach side: bearing from threshold ~ heading+180.
		off := geo.AngleDiff(geo.NormalizeDeg(e.HeadingT+180), brg)
		along := d * math.Cos(geo.Rad(off))
		if along <= 0 {
			continue
		}
		lateral := d * math.Sin(geo.Rad(off))
		latTol := 0.3 + 0.08*along
		if math.Abs(lateral) > latTol {
			continue
		}
		// off > 0: the aircraft lies towards heading+90, i.e. right of the centreline.
		f := ApproachFit{End: *e, DistKm: round(along, 3), LateralKm: round(lateral, 3)}
		score := math.Abs(lateral) / latTol
		if !math.IsNaN(trackDeg) {
			td := geo.AngleDiff(trackDeg, e.HeadingT)
			if math.Abs(td) > approachTrackTolDeg {
				continue
			}
			f.TrackDiffDeg = round(td, 1)
			score += math.Abs(td) / approachTrackTolDeg
		}
		if !math.IsNaN(altFt) {
			h := altFt - e.ElevFt
			glide := geo.Deg(math.Atan2(geo.FeetToMetres(h), along*1000))
			if h < -300 || glide > 7 {
				continue
			}
			f.HeightAboveFt = math.Round(h)
			f.GlideDeg = round(glide, 2)
			score += math.Abs(glide-3) / 4
		}
		f.Score = round(score, 3)
		if !found || f.Score < best.Score {
			best, found = f, true
		}
	}
	return best, found
}

// RunwayOutline returns the runway pavement as a closed [lon,lat] ring (nil
// when both end positions are not known). Unknown width defaults to 100 ft.
func RunwayOutline(r *Runway) [][2]float64 {
	if r.LE.Lat == nil || r.LE.Lon == nil || r.HE.Lat == nil || r.HE.Lon == nil {
		return nil
	}
	w := 100.0
	if r.WidthFt != nil && *r.WidthFt > 0 && *r.WidthFt < 1000 {
		w = float64(*r.WidthFt)
	}
	half := geo.FeetToMetres(w) / 2000 // km
	la, lo := *r.LE.Lat, *r.LE.Lon
	ha, ho := *r.HE.Lat, *r.HE.Lon
	h := geo.InitialBearing(la, lo, ha, ho)
	hb := geo.InitialBearing(ha, ho, la, lo)
	p1a, p1o := geo.Destination(la, lo, h-90, half)
	p2a, p2o := geo.Destination(ha, ho, hb+90, half)
	p3a, p3o := geo.Destination(ha, ho, hb-90, half)
	p4a, p4o := geo.Destination(la, lo, h+90, half)
	return [][2]float64{{r6(p1o), r6(p1a)}, {r6(p2o), r6(p2a)}, {r6(p3o), r6(p3a)}, {r6(p4o), r6(p4a)}, {r6(p1o), r6(p1a)}}
}

// Centerline returns the extended centreline of a runway end as a [lon,lat]
// line from the threshold outwards (against the landing direction) over lengthKm.
func Centerline(e *RunwayEndInfo, lengthKm float64) [][2]float64 {
	la, lo := geo.Destination(e.Lat, e.Lon, e.HeadingT+180, lengthKm)
	return [][2]float64{{r6(e.Lon), r6(e.Lat)}, {r6(lo), r6(la)}}
}

func r6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// GeoJSON types (minimal, JSON-ready).
type (
	// FeatureCollection is a GeoJSON FeatureCollection.
	FeatureCollection struct {
		Type     string    `json:"type"`
		Features []Feature `json:"features"`
	}
	// Feature is a GeoJSON Feature.
	Feature struct {
		Type       string         `json:"type"`
		Geometry   Geometry       `json:"geometry"`
		Properties map[string]any `json:"properties"`
	}
	// Geometry is a GeoJSON geometry; Coordinates is [2]float64, [][2]float64 or [][][2]float64.
	Geometry struct {
		Type        string `json:"type"`
		Coordinates any    `json:"coordinates"`
	}
)

// OverlayOptions tune AirportOverlay.
type OverlayOptions struct {
	CenterlineKm float64 // extended centreline length (default 18.52 km = 10 NM; < 0 = none)
	// CenterlineMinSize: only airports of at least this size get centrelines (default SizeMedium).
	CenterlineMinSize SizeClass
}

// AirportOverlay converts airports (with runways) to one GeoJSON
// FeatureCollection for the map: Point features (kind "airport"), runway
// Polygons (kind "runway") and extended-centreline LineStrings (kind "centerline").
func AirportOverlay(as []Airport, o OverlayOptions) FeatureCollection {
	if o.CenterlineKm == 0 {
		o.CenterlineKm = 18.52
	}
	if o.CenterlineMinSize == 0 {
		o.CenterlineMinSize = SizeMedium
	}
	fc := FeatureCollection{Type: "FeatureCollection", Features: []Feature{}}
	var ends []RunwayEndInfo
	for i := range as {
		a := &as[i]
		props := map[string]any{"kind": "airport", "id": a.ID, "ident": a.Ident, "code": a.Code(), "name": a.Name,
			"size": int(a.Size), "sizeName": a.Size.String(), "type": a.Type}
		if a.IATA != "" {
			props["iata"] = a.IATA
		}
		if a.ElevFt != nil {
			props["elevFt"] = *a.ElevFt
		}
		fc.Features = append(fc.Features, Feature{Type: "Feature",
			Geometry: Geometry{Type: "Point", Coordinates: [2]float64{r6(a.Lon), r6(a.Lat)}}, Properties: props})
		for j := range a.Runways {
			r := &a.Runways[j]
			if ring := RunwayOutline(r); ring != nil {
				p := map[string]any{"kind": "runway", "airport": a.Ident, "ident": r.LE.Ident + "/" + r.HE.Ident,
					"surface": r.Surface, "closed": r.Closed}
				if r.LengthFt != nil {
					p["lengthFt"] = *r.LengthFt
				}
				fc.Features = append(fc.Features, Feature{Type: "Feature",
					Geometry: Geometry{Type: "Polygon", Coordinates: [][][2]float64{ring}}, Properties: p})
			}
		}
		if o.CenterlineKm > 0 && a.Size >= o.CenterlineMinSize {
			ends = RunwayEnds(a, ends[:0])
			for k := range ends {
				e := &ends[k]
				fc.Features = append(fc.Features, Feature{Type: "Feature",
					Geometry: Geometry{Type: "LineString", Coordinates: Centerline(e, o.CenterlineKm)},
					Properties: map[string]any{"kind": "centerline", "airport": a.Ident, "runway": e.Runway,
						"headingT": round(e.HeadingT, 1)}})
			}
		}
	}
	return fc
}
