package terrain

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"strconv"
	"sync"

	"airview/internal/geo"
	"airview/internal/radio"
)

// DefaultAltitudesFt are the flight levels of the theoretical coverage
// ("up in the air") rings, feet AMSL.
var DefaultAltitudesFt = []float64{1000, 3000, 5000, 10000, 20000, 30000, 40000}

// RadioRanges extends the line-of-sight range with the link budget: for each
// altitude and bearing, the outermost distance at which the predicted signal
// (link: transponder EIRP − FSPL − gases − polarisation/misc + antenna
// pattern gain) minus the single knife-edge diffraction loss J(v) of the
// limiting terrain obstacle still reaches the receiver sensitivity. Beyond the
// terrain horizon this is the diffraction-extended range; where the link
// budget runs out first it is shorter than the LOS range. The link must have
// been built for this panorama's antenna height (link.AntennaM).
func (p *Panorama) RadioRanges(link *radio.Link, altsFt []float64) []AltRange {
	out := make([]AltRange, len(altsFt))
	if link == nil {
		return out
	}
	lambda := radio.WavelengthM
	fixed := link.EIRPDBm - link.PolarizationLossDB - link.Params.MiscLossDB - link.SensitivityDBm
	beta := make([]float64, p.bins)
	base := make([]float64, p.bins)
	slant := make([]float64, p.bins)
	margin := make([]float64, p.bins)
	_, omni := link.Pattern.(radio.Omni)
	for a, ft := range altsFt {
		altM := ft * geo.MetresPerFoot
		for b := 0; b < p.bins; b++ {
			d := p.binEnd[b]
			slant[b] = geo.SlantRangeKm(d, p.AntennaM, altM)
			beta[b] = geo.ElevationDeg(d, p.AntennaM, altM, p.K)
			base[b] = fixed - radio.FSPL1090DB(slant[b]) - link.Params.Gas.PathLossDB(d, p.AntennaM, altM, p.K)
		}
		r := AltRange{AltFt: ft, AltM: round(altM, 1), SmoothKm: round(radio.LineOfSightKm(p.AntennaM, altM, p.K), 3),
			RangeKm: make([]float64, p.rays)}
		var gainCache []float64
		if omni {
			gainCache = make([]float64, p.bins)
			for b := range gainCache {
				gainCache[b] = link.Pattern.GainDBi(0, beta[b])
			}
		}
		for i := 0; i < p.rays; i++ {
			az := float64(i) * p.StepDeg
			for b := 0; b < p.bins; b++ {
				var g float64
				if omni {
					g = gainCache[b]
				} else {
					g = link.Pattern.GainDBi(az, beta[b])
				}
				var j float64
				if b > 0 {
					ang := float64(p.cumAng[i*p.bins+b-1])
					dObs := float64(p.cumDist[i*p.bins+b-1])
					if ang > noTerrainDeg && dObs > 0 {
						d1 := dObs * 1000
						d2 := slant[b]*1000 - d1
						if d2 < 1 {
							d2 = 1
						}
						h := d1 * math.Sin((ang-beta[b])*degToRad)
						j = KnifeEdgeLossDB(h * math.Sqrt(2/lambda*(1/d1+1/d2)))
					}
				}
				margin[b] = base[b] + g - j
			}
			km, capped := outermost(margin, p.binEnd)
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

// outermost returns the largest distance with a non-negative margin,
// interpolated between bin ends.
func outermost(margin, binEnd []float64) (float64, bool) {
	n := len(margin)
	for b := n - 1; b >= 0; b-- {
		if margin[b] < 0 {
			continue
		}
		if b == n-1 {
			return binEnd[b], true
		}
		m0, m1 := margin[b], margin[b+1]
		f := 0.0
		if m0 != m1 {
			f = m0 / (m0 - m1)
		}
		return binEnd[b] + f*(binEnd[b+1]-binEnd[b]), false
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Analysis result

// AntennaCoverage is the terrain analysis of one antenna.
type AntennaCoverage struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	HeightAGLM float64 `json:"heightAglM"`
	AntennaM   float64 `json:"antennaM"` // antenna AMSL used
	Horizon    Horizon `json:"horizon"`
	// LOS: terrain line-of-sight range per altitude (Analysis.AltitudesFt).
	LOS []AltRange `json:"los"`
	// Radio: diffraction-extended / power-limited range per altitude (only
	// when the request enabled the link budget).
	Radio []AltRange `json:"radio,omitempty"`
	// SensitivityDBm is the receiver sensitivity at the antenna terminals
	// used for Radio (0 when not computed).
	SensitivityDBm float64 `json:"sensitivityDbm,omitempty"`

	pano   *Panorama
	advice *SiteAdvice // site advice of this antenna (Analysis.AdviceFor)
}

// Panorama returns the underlying panorama.
func (c *AntennaCoverage) Panorama() *Panorama { return c.pano }

// Analysis is a complete, immutable terrain analysis of the station.
type Analysis struct {
	Key            string             `json:"key"`
	ComputedMs     int64              `json:"computedMs"`
	ComputeMs      int64              `json:"computeMs"` // duration of the job
	Lat            float64            `json:"lat"`
	Lon            float64            `json:"lon"`
	GroundM        float64            `json:"groundM"`      // site ground elevation used
	GroundSource   string             `json:"groundSource"` // "dem" | "config"
	DEMGroundM     float64            `json:"demGroundM"`   // DEM elevation at the site (0 if none)
	DEMGroundOK    bool               `json:"demGroundOk"`
	K              float64            `json:"k"`
	RadiusKm       float64            `json:"radiusKm"`
	StepDeg        float64            `json:"stepDeg"`
	SampleKm       float64            `json:"sampleKm"`
	Zoom           int                `json:"zoom"`
	NoDataFraction float64            `json:"noDataFraction"`
	DataComplete   bool               `json:"dataComplete"`
	AltitudesFt    []float64          `json:"altitudesFt"`
	Antennas       []*AntennaCoverage `json:"antennas"`
	Advice         *SiteAdvice        `json:"advice"`
	Attribution    string             `json:"attribution"`
	AttributionURL string             `json:"attributionUrl"`

	pngMu    sync.Mutex
	pngCache map[string]*FloorImage
}

// Antenna returns the coverage of antenna id ("" = the first antenna), or nil.
func (a *Analysis) Antenna(id string) *AntennaCoverage {
	if a == nil || len(a.Antennas) == 0 {
		return nil
	}
	if id == "" {
		return a.Antennas[0]
	}
	for _, c := range a.Antennas {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// AdviceFor returns the site advice of antenna id ("" = the first antenna),
// or nil for an unknown antenna.
func (a *Analysis) AdviceFor(id string) *SiteAdvice {
	c := a.Antenna(id)
	if c == nil {
		return nil
	}
	if c.advice == nil && c == a.Antenna("") {
		return a.Advice
	}
	return c.advice
}

// Panorama returns the panorama of antenna id ("" = first), or nil.
func (a *Analysis) Panorama(id string) *Panorama {
	if c := a.Antenna(id); c != nil {
		return c.pano
	}
	return nil
}

// Visible evaluates an aircraft against the terrain horizon. antID "" means
// the best of all antennas (largest margin; AntennaID tells which).
// No allocation; nil-safe (returns Known=false).
func (a *Analysis) Visible(antID string, lat, lon, altFt float64) Visibility {
	if a == nil || len(a.Antennas) == 0 {
		return Visibility{}
	}
	if antID != "" {
		c := a.Antenna(antID)
		if c == nil {
			return Visibility{}
		}
		v := c.pano.Visible(lat, lon, altFt)
		v.AntennaID = c.ID
		return v
	}
	var best Visibility
	for i, c := range a.Antennas {
		v := c.pano.Visible(lat, lon, altFt)
		v.AntennaID = c.ID
		if i == 0 || v.MarginDeg > best.MarginDeg {
			best = v
		}
	}
	return best
}

// Forecast predicts terrain loss / acquisition along a path (antID "" = any
// antenna: visible when at least one antenna sees the aircraft).
func (a *Analysis) Forecast(antID string, path []PathPoint) VisibilityForecast {
	if a == nil || len(a.Antennas) == 0 {
		return VisibilityForecast{}
	}
	return forecast(path, func(pt PathPoint) (float64, bool) {
		v := a.Visible(antID, pt.Lat, pt.Lon, pt.AltFt)
		return v.MarginDeg, v.Known
	})
}

// ---------------------------------------------------------------------------
// GeoJSON

// Geometry is a GeoJSON Polygon or Point geometry.
type Geometry struct {
	Type        string `json:"type"`
	Coordinates any    `json:"coordinates"`
}

// Feature is a GeoJSON feature.
type Feature struct {
	Type       string         `json:"type"`
	Geometry   Geometry       `json:"geometry"`
	Properties map[string]any `json:"properties"`
}

// FeatureCollection is a GeoJSON feature collection.
type FeatureCollection struct {
	Type     string    `json:"type"`
	Features []Feature `json:"features"`
}

// Coverage kinds for GeoJSON.
const (
	KindLOS   = "los"
	KindRadio = "radio"
)

// GeoJSON returns the coverage rings of antenna antID ("" = first) as a
// FeatureCollection with one Polygon per altitude (highest first so that
// lower, smaller rings draw on top). kind is KindLOS or KindRadio.
func (a *Analysis) GeoJSON(antID, kind string) FeatureCollection {
	fc := FeatureCollection{Type: "FeatureCollection", Features: []Feature{}}
	c := a.Antenna(antID)
	if c == nil {
		return fc
	}
	ranges := c.LOS
	if kind == KindRadio {
		ranges = c.Radio
	} else {
		kind = KindLOS
	}
	o := newOrigin(a.Lat, a.Lon)
	for idx := len(ranges) - 1; idx >= 0; idx-- {
		r := ranges[idx]
		ring := make([][2]float64, 0, len(r.RangeKm)+1)
		for i, km := range r.RangeKm {
			la, lo := a.Lat, a.Lon
			if km > 0 {
				la, lo = o.destKm(float64(i)*c.pano.StepDeg, km)
			}
			ring = append(ring, [2]float64{round(lo, 5), round(la, 5)})
		}
		if len(ring) > 0 {
			ring = append(ring, ring[0])
		}
		fc.Features = append(fc.Features, Feature{
			Type:     "Feature",
			Geometry: Geometry{Type: "Polygon", Coordinates: [][][2]float64{ring}},
			Properties: map[string]any{
				"kind": kind, "antennaId": c.ID, "altFt": r.AltFt, "altM": r.AltM,
				"areaKm2": r.AreaKm2, "meanKm": r.MeanKm, "maxKm": r.MaxKm, "minKm": r.MinKm,
				"smoothKm": r.SmoothKm, "capped": r.Capped, "color": AltitudeColor(r.AltFt),
			},
		})
	}
	return fc
}

// ---------------------------------------------------------------------------
// Coverage floor (minimum visible altitude)

// FloorGrid is the minimum altitude (ft AMSL) at which an aircraft is above
// the terrain horizon, on a regular lat/lon grid. With CellDeg = 0.05 the
// cells coincide with the geo heat-map cells (store table heat), so the
// theoretical floor can be compared with the measured heat.min_alt.
type FloorGrid struct {
	AntennaID string  `json:"antennaId"` // "" = best of all antennas
	CellDeg   float64 `json:"cellDeg"`
	South     float64 `json:"south"` // edge of row 0
	West      float64 `json:"west"`  // edge of column 0
	North     float64 `json:"north"`
	East      float64 `json:"east"`
	Rows      int     `json:"rows"`
	Cols      int     `json:"cols"`
	// FloorFt is row-major starting at the south-west cell (row 0 = south);
	// -1 = outside the analysed radius or never visible.
	FloorFt     []int32 `json:"floorFt"`
	HeatAligned bool    `json:"heatAligned"`
}

const maxGridCells = 1 << 20

// floorAt returns the coverage floor (m AMSL) at a point: the minimum over
// the selected antennas; +Inf if outside the radius / never visible.
func (a *Analysis) floorAt(c *AntennaCoverage, o *origin, lat, lon float64) float64 {
	d, brg := o.inverse(lat, lon)
	best := math.Inf(1)
	if c != nil {
		if f, ok := c.pano.FloorM(brg, d); ok {
			best = f
		}
		return best
	}
	for _, ac := range a.Antennas {
		if f, ok := ac.pano.FloorM(brg, d); ok && f < best {
			best = f
		}
	}
	return best
}

// FloorGrid computes the coverage floor grid for antenna antID ("" = best of
// all antennas) with the given cell size (0 = 0.05°, heat-map aligned).
func (a *Analysis) FloorGrid(antID string, cellDeg float64) FloorGrid {
	g := FloorGrid{AntennaID: antID, FloorFt: []int32{}}
	if a == nil || len(a.Antennas) == 0 {
		return g
	}
	var c *AntennaCoverage
	if antID != "" {
		if c = a.Antenna(antID); c == nil {
			return g
		}
	}
	if !(cellDeg > 0) {
		cellDeg = geo.HeatCellDeg
	}
	dLat := a.RadiusKm / 111.19492664455873
	dLon := 180.0
	if cl := math.Cos(math.Min(89, math.Abs(a.Lat)+dLat) * degToRad); cl > 1e-6 {
		dLon = math.Min(180, dLat/cl)
	}
	for {
		g.South = math.Floor((a.Lat-dLat+90)/cellDeg)*cellDeg - 90
		g.North = math.Ceil((a.Lat+dLat+90)/cellDeg)*cellDeg - 90
		g.West = math.Floor((a.Lon-dLon+180)/cellDeg)*cellDeg - 180
		g.East = math.Ceil((a.Lon+dLon+180)/cellDeg)*cellDeg - 180
		g.Rows = int(math.Round((g.North - g.South) / cellDeg))
		g.Cols = int(math.Round((g.East - g.West) / cellDeg))
		if g.Rows*g.Cols <= maxGridCells {
			break
		}
		cellDeg *= 2
	}
	g.CellDeg = cellDeg
	g.HeatAligned = math.Abs(cellDeg-geo.HeatCellDeg) < 1e-12
	g.FloorFt = make([]int32, g.Rows*g.Cols)
	o := newOrigin(a.Lat, a.Lon)
	for r := 0; r < g.Rows; r++ {
		lat := g.South + (float64(r)+0.5)*cellDeg
		for col := 0; col < g.Cols; col++ {
			lon := geo.NormalizeLon(g.West + (float64(col)+0.5)*cellDeg)
			f := a.floorAt(c, &o, lat, lon)
			v := int32(-1)
			if !math.IsInf(f, 1) && !math.IsNaN(f) {
				v = int32(math.Round(f * geo.FeetPerMetre))
			}
			g.FloorFt[r*g.Cols+col] = v
		}
	}
	return g
}

// LegendBand is one colour band of the coverage-floor map.
type LegendBand struct {
	MaxFt float64 `json:"maxFt"` // floor below this altitude
	Color string  `json:"color"` // #rrggbb
	Label string  `json:"label"`
}

// FloorLegend returns the colour bands used by FloorImage and AltitudeColor.
func FloorLegend() []LegendBand {
	return []LegendBand{
		{1000, "#22d3ee", "< 1 000 ft"},
		{3000, "#2dd4bf", "1 000 – 3 000 ft"},
		{5000, "#4ade80", "3 000 – 5 000 ft"},
		{10000, "#a3e635", "5 000 – 10 000 ft"},
		{20000, "#facc15", "10 000 – 20 000 ft"},
		{30000, "#fb923c", "20 000 – 30 000 ft"},
		{40000, "#f87171", "30 000 – 40 000 ft"},
		{60000, "#c084fc", "40 000 – 60 000 ft"},
	}
}

// AltitudeColor returns the legend colour for an altitude (ft).
func AltitudeColor(ft float64) string {
	l := FloorLegend()
	for _, b := range l {
		if ft <= b.MaxFt {
			return b.Color
		}
	}
	return l[len(l)-1].Color
}

func parseHex(c string) color.NRGBA {
	v, _ := strconv.ParseUint(c[1:], 16, 32)
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}
}

// FloorImage is a rendered coverage-floor overlay for a MapLibre image
// source. Rows are evenly spaced in Web Mercator so the image maps exactly
// onto the four corner Coordinates ([lon,lat]: top-left, top-right,
// bottom-right, bottom-left).
type FloorImage struct {
	PNG         []byte        `json:"-"`
	Width       int           `json:"width"`
	Height      int           `json:"height"`
	Coordinates [4][2]float64 `json:"coordinates"`
	Legend      []LegendBand  `json:"legend"`
}

// FloorImage renders (and caches) the coverage-floor overlay for antenna
// antID ("" = best of all antennas), width pixels wide (0 = 1024, max 2048).
// Pixels whose floor is above the top legend band or outside the radius are
// transparent.
func (a *Analysis) FloorImage(antID string, width int) (*FloorImage, error) {
	if a == nil || len(a.Antennas) == 0 {
		return nil, errNoAnalysis
	}
	var c *AntennaCoverage
	if antID != "" {
		if c = a.Antenna(antID); c == nil {
			return nil, errNoAntenna
		}
	}
	if width <= 0 {
		width = 1024
	}
	if width > 2048 {
		width = 2048
	}
	if width < 64 {
		width = 64
	}
	ck := antID + "|" + strconv.Itoa(width)
	a.pngMu.Lock()
	defer a.pngMu.Unlock()
	if im, ok := a.pngCache[ck]; ok {
		return im, nil
	}
	dLat := a.RadiusKm / 111.19492664455873
	dLon := 180.0
	if cl := math.Cos(math.Min(89, math.Abs(a.Lat)+dLat) * degToRad); cl > 1e-6 {
		dLon = math.Min(180, dLat/cl)
	}
	west, east := a.Lon-dLon, a.Lon+dLon
	north, south := math.Min(a.Lat+dLat, maxMercatorLat), math.Max(a.Lat-dLat, -maxMercatorLat)
	mx0, mx1 := (west+180)/360, (east+180)/360
	my0, my1 := MercatorY(north), MercatorY(south)
	height := int(math.Round(float64(width) * (my1 - my0) / (mx1 - mx0)))
	if height < 1 {
		height = 1
	}
	if height > 4096 {
		height = 4096
	}
	legend := FloorLegend()
	cols := make([]color.NRGBA, len(legend))
	for i, b := range legend {
		cols[i] = parseHex(b.Color)
		cols[i].A = 150
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	o := newOrigin(a.Lat, a.Lon)
	for y := 0; y < height; y++ {
		lat := MercatorLat(my0 + (float64(y)+0.5)/float64(height)*(my1-my0))
		for x := 0; x < width; x++ {
			lon := geo.NormalizeLon(west + (float64(x)+0.5)/float64(width)*(east-west))
			f := a.floorAt(c, &o, lat, lon) * geo.FeetPerMetre
			if math.IsInf(f, 1) || math.IsNaN(f) {
				continue
			}
			for i, b := range legend {
				if f < b.MaxFt {
					img.SetNRGBA(x, y, cols[i])
					break
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	im := &FloorImage{
		PNG: buf.Bytes(), Width: width, Height: height, Legend: legend,
		Coordinates: [4][2]float64{
			{round(west, 6), round(north, 6)}, {round(east, 6), round(north, 6)},
			{round(east, 6), round(south, 6)}, {round(west, 6), round(south, 6)},
		},
	}
	if a.pngCache == nil {
		a.pngCache = make(map[string]*FloorImage)
	}
	if len(a.pngCache) >= 8 {
		clear(a.pngCache)
	}
	a.pngCache[ck] = im
	return im, nil
}
