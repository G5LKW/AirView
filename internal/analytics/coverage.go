package analytics

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"

	"airview/internal/geo"
	"airview/internal/radio"
	"airview/internal/stat"
	"airview/internal/store"
)

const (
	brgBins   = geo.BearingBins // 180 x 2 degrees
	brgBinDeg = geo.BearingBinDeg
	altBands  = geo.AltBands

	// overallHorizonFt is the reference altitude for the "all altitudes"
	// horizon comparison (typical top of airline cruise levels).
	overallHorizonFt = 43000.0
	// topBandCeilFt closes the open-ended top band.
	topBandCeilFt = 45000.0
)

// --------------------------------------------------------------- raw data

// covData is the coverage table aggregated over a day range.
type covData struct {
	days      int   // days with any row
	positions int64 //
	// daily maxima per band / bearing (one entry per day with data) and for
	// all bands together (index altBands).
	daily [altBands + 1][brgBins][]float32
	count [altBands + 1][brgBins]int64
	rssiS [altBands + 1][brgBins]float64
	rssiN [altBands + 1][brgBins]int64
}

func (a *Analyzer) loadCoverage(ctx context.Context, ant string, dayFrom, dayTo int) (*covData, error) {
	return cached(ctx, a, fmt.Sprintf("covraw|%s|%d|%d", ant, dayFrom, dayTo), ttlCoverage, func(ctx context.Context) (*covData, error) {
		d := &covData{}
		curDay := math.MinInt
		var dayMax [brgBins]float32
		flush := func() {
			if curDay == math.MinInt {
				return
			}
			d.days++
			for b, v := range dayMax {
				if v > 0 {
					d.daily[altBands][b] = append(d.daily[altBands][b], v)
				}
			}
			dayMax = [brgBins]float32{}
		}
		rows, err := a.st.CoverageRows(ctx, a.antKey(ant), dayFrom, dayTo, a.pending().Coverage)
		for _, r := range rows {
			day, brg, band := r.Day, r.BrgBin, r.AltBand
			km, rs1, n, rn := r.MaxKm, r.RSSISum, r.Count, r.RSSIN
			if brg < 0 || brg >= brgBins || band < 0 || band >= altBands {
				continue
			}
			if day != curDay {
				flush()
				curDay = day
			}
			d.positions += n
			for _, k := range [2]int{band, altBands} {
				d.count[k][brg] += n
				d.rssiS[k][brg] += rs1
				d.rssiN[k][brg] += rn
			}
			if km > 0 && isNum(km) {
				d.daily[band][brg] = append(d.daily[band][brg], float32(km))
				if float32(km) > dayMax[brg] {
					dayMax[brg] = float32(km)
				}
			}
		}
		flush()
		return d, err
	})
}

// heatCell is one row of the heat table.
type heatCell struct {
	latIdx, lonIdx int
	lat, lon       float64 // cell centre
	count          int64
	rssi           float64 // mean dBFS, NaN unknown
	rssiN          int64
	minAlt, maxAlt float64 // ft, NaN unknown
	lastT          int64
}

func (a *Analyzer) loadHeat(ctx context.Context, ant string) ([]heatCell, error) {
	return cached(ctx, a, "heatraw|"+ant, ttlCoverage, func(ctx context.Context) ([]heatCell, error) {
		key := a.antKey(ant)
		rows, err := a.st.HeatCells(ctx, key)
		if err != nil {
			return nil, err
		}
		rows = store.OverlayHeat(rows, key, a.pending().Heat)
		var out []heatCell // nil when empty, as in 1.0.0
		if len(rows) > 0 {
			out = make([]heatCell, 0, len(rows))
		}
		for _, r := range rows {
			cell := r.Cell
			c := heatCell{count: r.Count, rssiN: r.RSSIN, rssi: math.NaN(), minAlt: r.MinAlt, maxAlt: r.MaxAlt, lastT: r.LastT}
			c.latIdx, c.lonIdx = geo.HeatCellIndices(uint32(cell))
			c.lat, c.lon = geo.HeatCellCenter(uint32(cell))
			if r.RSSIN > 0 {
				c.rssi = r.RSSISum / float64(r.RSSIN)
			}
			out = append(out, c)
		}
		return out, nil
	})
}

// ---------------------------------------------------------------- results

// PolarBand is the polar range plot of one altitude band (or of all
// altitudes: Band = -1). Every slice has one value per 2 degree bearing bin
// (see Coverage.BearingDeg); null = nothing received on that bearing.
type PolarBand struct {
	Band  int     `json:"band"`
	Label string  `json:"label"`
	AltFt float64 `json:"altFt"` // representative altitude
	// MaxKm is the absolute maximum over the range; RobustKm the 95th
	// percentile of the daily maxima with isolated spikes clipped to their
	// neighbourhood (what the station reliably reaches); TypicalKm the
	// median daily maximum.
	MaxKm     Nums    `json:"maxKm"`
	RobustKm  Nums    `json:"robustKm"`
	TypicalKm Nums    `json:"typicalKm"`
	Count     []int64 `json:"count"`
	RSSI      Nums    `json:"rssi"` // mean dBFS

	Positions int64 `json:"positions"`
	Bearings  int   `json:"bearings"` // bearing bins with data
	BestKm    Num   `json:"bestKm"`
	BestBrg   Num   `json:"bestBrg"`

	// Radio horizon comparison (smooth earth, k = 4/3).
	HorizonKm    Num  `json:"horizonKm"`    // line of sight at the top of the band
	HorizonTypKm Num  `json:"horizonTypKm"` // at the representative altitude
	PctOfHorizon Nums `json:"pctOfHorizon"` // 100 * RobustKm / HorizonKm per bearing
	MeanPct      Num  `json:"meanPct"`      // mean over all 180 bearings (no data = 0 %)
}

// Skyline is the measured horizon: per bearing the lowest elevation angle
// at which aircraft are still received.
type Skyline struct {
	// Source is "heat" (lowest aircraft per heat-grid cell, needs a located
	// receiver) or "coverage" (derived from the range per altitude band).
	Source    string `json:"source"`
	ElevDeg   Nums   `json:"elevDeg"`   // per bearing bin; null = no data
	MedianDeg Num    `json:"medianDeg"` //
	Bearings  int    `json:"bearings"`
}

// Obstruction is a sector where low-altitude coverage is persistently short.
type Obstruction struct {
	FromDeg   float64 `json:"fromDeg"`
	ToDeg     float64 `json:"toDeg"` // clockwise from FromDeg (may wrap through north)
	CenterDeg float64 `json:"centerDeg"`
	WidthDeg  float64 `json:"widthDeg"`
	Compass   string  `json:"compass"`
	ElevDeg   Num     `json:"elevDeg"`   // mean skyline elevation in the sector
	ExcessDeg Num     `json:"excessDeg"` // above the median skyline
	RangePct  Num     `json:"rangePct"`  // low-altitude range relative to the all-round median, %
	Severity  string  `json:"severity"`  // "minor" | "moderate" | "severe"
	Text      string  `json:"text"`
}

// Coverage is the result of Analyzer.Coverage.
type Coverage struct {
	From         int64         `json:"from"`
	To           int64         `json:"to"`
	Ant          string        `json:"ant"`
	Days         int           `json:"days"` // days with data
	Positions    int64         `json:"positions"`
	Located      bool          `json:"located"`
	Receiver     *LatLon       `json:"receiver,omitempty"`
	AntennaAMSLM float64       `json:"antennaAmslM"`
	BinDeg       float64       `json:"binDeg"`
	BearingDeg   Nums          `json:"bearingDeg"` // bin centres
	Overall      PolarBand     `json:"overall"`
	Bands        []PolarBand   `json:"bands"`
	Skyline      Skyline       `json:"skyline"`
	Obstructions []Obstruction `json:"obstructions"`

	gen uint64 // computation stamp (see Analyzer.nextGen)
}

func dayRange(r Range, now int64, defDays int) (from, to int64, dayFrom, dayTo int) {
	from, to = r.resolve(now, int64(defDays)*86400, 86400)
	return from, to, store.DayOf(from), store.DayOf(to - 1)
}

// Coverage returns the polar range plots per altitude band and overall, the
// comparison with the radio horizon, the measured skyline and the detected
// obstructions. Default range: the last 30 days.
func (a *Analyzer) Coverage(ctx context.Context, r Range, ant string) (*Coverage, error) {
	now, _ := a.clock()
	from, to, d0, d1 := dayRange(r, now.Unix(), 30)
	cfg := a.cfg()
	return cached(ctx, a, a.key("coverage", ant, cfg, int64(d0), int64(d1)), ttlCoverage, func(ctx context.Context) (*Coverage, error) {
		cd, err := a.loadCoverage(ctx, ant, d0, d1)
		if err != nil {
			return nil, err
		}
		s := siteOf(cfg, ant)
		res := &Coverage{
			From: from * 1000, To: to * 1000, Ant: ant, Days: cd.days, Positions: cd.positions,
			Located: s.located, Receiver: s.pos(), AntennaAMSLM: round(s.antM, 1), BinDeg: brgBinDeg,
			BearingDeg: make(Nums, brgBins), Obstructions: []Obstruction{},
		}
		for b := range res.BearingDeg {
			res.BearingDeg[b] = geo.BearingBinCenter(b)
		}
		table := radio.HorizonTable(s.antM, 0)
		res.Bands = make([]PolarBand, altBands)
		for b := 0; b < altBands; b++ {
			res.Bands[b] = polarBand(cd, b, geo.AltBandLabel(b), geo.AltBandTypicalFt(b), table[b].LOSHiKm, table[b].LOSKm)
		}
		hz := radio.LineOfSightFtKm(s.antM, overallHorizonFt, 0)
		res.Overall = polarBand(cd, altBands, "all", overallHorizonFt, hz, hz)
		res.Overall.Band = -1

		var heat []heatCell
		if s.located {
			if heat, err = a.loadHeat(ctx, ant); err != nil {
				return nil, err
			}
		}
		res.Skyline = skyline(s, heat, res.Bands)
		res.Obstructions = obstructions(res.Skyline, res.Bands)
		res.gen = a.nextGen()
		return res, nil
	})
}

// polarBand builds the plot of band index k of cd (altBands = overall).
func polarBand(cd *covData, k int, label string, altFt, horizonKm, horizonTypKm float64) PolarBand {
	p := PolarBand{
		Band: k, Label: label, AltFt: altFt,
		MaxKm: nanSlice(brgBins), RobustKm: nanSlice(brgBins), TypicalKm: nanSlice(brgBins),
		Count: make([]int64, brgBins), RSSI: nanSlice(brgBins), PctOfHorizon: nanSlice(brgBins),
		BestKm: Num(math.NaN()), BestBrg: Num(math.NaN()),
		HorizonKm: Num(horizonKm), HorizonTypKm: Num(horizonTypKm),
	}
	q95 := nanSlice(brgBins)
	buf := make([]float64, 0, 64)
	for b := 0; b < brgBins; b++ {
		p.Count[b] = cd.count[k][b]
		p.Positions += cd.count[k][b]
		if cd.rssiN[k][b] > 0 {
			p.RSSI[b] = cd.rssiS[k][b] / float64(cd.rssiN[k][b])
		}
		vals := cd.daily[k][b]
		if len(vals) == 0 {
			continue
		}
		buf = buf[:0]
		for _, v := range vals {
			buf = append(buf, float64(v))
		}
		sort.Float64s(buf)
		p.MaxKm[b] = buf[len(buf)-1]
		p.TypicalKm[b] = stat.QuantileSorted(buf, 0.5)
		q95[b] = stat.QuantileSorted(buf, 0.95)
		p.Bearings++
		if !(float64(p.BestKm) >= p.MaxKm[b]) {
			p.BestKm, p.BestBrg = Num(p.MaxKm[b]), Num(geo.BearingBinCenter(b))
		}
	}
	// Clip isolated spikes (one bad position decode on one bearing) to 1.25
	// times the median of the +-3 bin neighbourhood.
	var win [7]float64
	sum := 0.0
	for b := 0; b < brgBins; b++ {
		if !isNum(q95[b]) {
			continue
		}
		n := 0
		for o := -3; o <= 3; o++ {
			if v := q95[(b+o+brgBins)%brgBins]; isNum(v) && o != 0 {
				win[n] = v
				n++
			}
		}
		v := q95[b]
		if n >= 4 {
			sort.Float64s(win[:n])
			if lim := 1.25 * stat.QuantileSorted(win[:n], 0.5); v > lim {
				v = lim
			}
		}
		p.RobustKm[b] = v
		if horizonKm > 0 {
			p.PctOfHorizon[b] = 100 * v / horizonKm
			sum += math.Min(100, p.PctOfHorizon[b])
		}
	}
	p.MeanPct = Num(sum / brgBins)
	return p
}

// --------------------------------------------------------------- skyline

func bandCeilM(b int) float64 {
	_, hi := geo.AltBandBoundsFt(b)
	if math.IsInf(hi, 1) {
		hi = topBandCeilFt
	}
	return geo.FeetToMetres(hi)
}

// skyline estimates the lowest elevation angle with reception per bearing.
func skyline(s site, heat []heatCell, bands []PolarBand) Skyline {
	sk := Skyline{ElevDeg: nanSlice(brgBins), MedianDeg: Num(math.NaN())}
	if s.located && len(heat) > 0 {
		var lists [brgBins][]float64
		st := geo.NewStation(s.lat, s.lon, s.antM, 0)
		for i := range heat {
			c := &heat[i]
			if c.count < 2 || !isNum(c.minAlt) {
				continue
			}
			km, brg := st.DistanceBearing(c.lat, c.lon)
			if km < 15 || km > 700 {
				continue
			}
			altM := geo.FeetToMetres(c.minAlt)
			// Below the smooth-earth floor by a wide margin = bad decode.
			if altM < geo.MinVisibleAltitudeM(km, s.antM, 0)-400 {
				continue
			}
			b := geo.BearingBin(brg)
			lists[b] = append(lists[b], geo.ElevationDeg(km, s.antM, altM, 0))
		}
		var pool []float64
		for b := 0; b < brgBins; b++ {
			pool = pool[:0]
			for o := -1; o <= 1; o++ {
				pool = append(pool, lists[(b+o+brgBins)%brgBins]...)
			}
			if len(lists[b]) == 0 || len(pool) < 3 {
				continue
			}
			sort.Float64s(pool)
			if len(pool) >= 8 {
				sk.ElevDeg[b] = stat.QuantileSorted(pool, 0.05)
			} else {
				sk.ElevDeg[b] = pool[0]
			}
			sk.Bearings++
		}
		if sk.Bearings >= 6 {
			sk.Source = "heat"
		} else {
			sk.ElevDeg, sk.Bearings = nanSlice(brgBins), 0
		}
	}
	if sk.Source == "" {
		for b := 0; b < brgBins; b++ {
			best := math.NaN()
			for k := range bands {
				km := bands[k].RobustKm[b]
				if !isNum(km) || km < 5 || bands[k].Count[b] < 5 {
					continue
				}
				best = nanMin(best, geo.ElevationDeg(km, s.antM, bandCeilM(k), 0))
			}
			if isNum(best) {
				sk.ElevDeg[b] = best
				sk.Bearings++
			}
		}
		if sk.Bearings > 0 {
			sk.Source = "coverage"
		}
	}
	sk.MedianDeg = Num(stat.Median(sk.ElevDeg))
	return sk
}

// obstructions finds sectors whose skyline is clearly above the all-round
// median or whose low-altitude range is persistently short.
func obstructions(sk Skyline, bands []PolarBand) []Obstruction {
	out := []Obstruction{}
	// Low-altitude range relative to the all-round median, bands 1..3
	// (1000 - 20000 ft), averaged over the bands that have real coverage.
	ratio := nanSlice(brgBins)
	var rsum, rn [brgBins]float64
	lowBands := 0
	for k := 1; k <= 3 && k < len(bands); k++ {
		if bands[k].Bearings < brgBins/3 {
			continue
		}
		med := stat.Median(bands[k].RobustKm)
		if !(med > 0) {
			continue
		}
		lowBands++
		for b := 0; b < brgBins; b++ {
			v := bands[k].RobustKm[b]
			if !isNum(v) {
				v = 0
			}
			rsum[b] += v / med
			rn[b]++
		}
	}
	for b := 0; b < brgBins; b++ {
		if rn[b] > 0 {
			ratio[b] = rsum[b] / rn[b]
		}
	}
	med := float64(sk.MedianDeg)
	thr := math.NaN()
	if sk.Bearings >= brgBins/3 && isNum(med) {
		thr = math.Max(0.6, 2.5*stat.RobustStd(sk.ElevDeg))
	}
	flag := make([]bool, brgBins)
	anyFlag := false
	for b := 0; b < brgBins; b++ {
		bySky := isNum(thr) && isNum(sk.ElevDeg[b]) && sk.ElevDeg[b]-med > thr
		byRange := lowBands > 0 && isNum(ratio[b]) && ratio[b] < 0.6
		flag[b] = bySky || byRange
		anyFlag = anyFlag || flag[b]
	}
	if !anyFlag {
		return out
	}
	// Majority filter over 5 bins removes speckle.
	sm := make([]bool, brgBins)
	all := true
	for b := 0; b < brgBins; b++ {
		n := 0
		for o := -2; o <= 2; o++ {
			if flag[(b+o+brgBins)%brgBins] {
				n++
			}
		}
		sm[b] = n >= 3
		all = all && sm[b]
	}
	if all {
		return out // no reference direction left
	}
	start := 0
	for sm[start] {
		start++ // a clear bin to start the scan from
	}
	for i := 1; i <= brgBins; i++ {
		b := (start + i) % brgBins
		if !sm[b] {
			continue
		}
		run := 0
		for run < brgBins && sm[(b+run)%brgBins] {
			run++
		}
		if run >= 3 {
			o := Obstruction{
				FromDeg: float64(b) * brgBinDeg, ToDeg: math.Mod(float64(b+run)*brgBinDeg, 360),
				WidthDeg: float64(run) * brgBinDeg,
				ElevDeg:  Num(math.NaN()), ExcessDeg: Num(math.NaN()), RangePct: Num(math.NaN()),
			}
			o.CenterDeg = math.Mod(o.FromDeg+o.WidthDeg/2, 360)
			o.Compass = compass(o.CenterDeg)
			var es, en, rs, rcount float64
			for j := 0; j < run; j++ {
				k := (b + j) % brgBins
				if isNum(sk.ElevDeg[k]) {
					es += sk.ElevDeg[k]
					en++
				}
				if isNum(ratio[k]) {
					rs += ratio[k]
					rcount++
				}
			}
			if en > 0 {
				o.ElevDeg = Num(es / en)
				if isNum(med) {
					o.ExcessDeg = Num(es/en - med)
				}
			}
			if rcount > 0 {
				o.RangePct = Num(100 * rs / rcount)
			}
			o.Severity = "minor"
			ex, rp := float64(o.ExcessDeg), float64(o.RangePct)
			switch {
			case (isNum(ex) && ex > 3) || (isNum(rp) && rp < 30):
				o.Severity = "severe"
			case (isNum(ex) && ex > 1.5) || (isNum(rp) && rp < 50):
				o.Severity = "moderate"
			}
			o.Text = fmt.Sprintf("Blocked to the %s (%s-%s)", o.Compass, fmtDeg(o.FromDeg), fmtDeg(o.ToDeg))
			if isNum(float64(o.ElevDeg)) {
				o.Text += fmt.Sprintf(" below %.1f° elevation", float64(o.ElevDeg))
			}
			if isNum(rp) {
				o.Text += fmt.Sprintf("; low-altitude range is %.0f %% of the all-round median", rp)
			}
			out = append(out, o)
		}
		i += run
	}
	return out
}

// ---------------------------------------------------------------- GeoJSON

// FeatureCollection is a GeoJSON FeatureCollection of Polygons.
type FeatureCollection struct {
	Type     string    `json:"type"`
	Features []Feature `json:"features"`
}

// Feature is a GeoJSON Polygon feature.
type Feature struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	Geometry   Geometry       `json:"geometry"`
}

// Geometry is a GeoJSON Polygon; rings are closed [lon, lat] lists.
type Geometry struct {
	Type        string         `json:"type"`
	Coordinates [][][2]float64 `json:"coordinates"`
}

// CoverageGeoJSON returns the coverage envelopes as GeoJSON polygons around
// the receiver for the live-map overlay: for all altitudes and for each
// altitude band with data, one polygon of the absolute maximum (property
// kind = "max") and one of the robust envelope (kind = "robust"). Further
// properties: band (-1 = all), label, altFt, loFt, hiFt (null = open), ant.
// A polygon follows only the bearings with data (bearings without data are
// left out, not drawn at the receiver); an envelope with fewer than three
// such bearings yields no feature. The collection is empty when the
// receiver is not located or there is no data.
func (a *Analyzer) CoverageGeoJSON(ctx context.Context, r Range, ant string) (*FeatureCollection, error) {
	return a.coverageFeatures(ctx, r, ant, OutlineOptions{Band: outlineDefault})
}

// Altitude selections of OutlineOptions.Band besides a single band
// 0..geo.AltBands-1 (the alt_band encoding of the coverage table).
const (
	OutlineOverall  = -1 // all altitudes together (band property -1)
	OutlineAllBands = -2 // one outline per altitude band with data

	outlineDefault = -3 // CoverageGeoJSON: overall plus every band
)

// OutlineOptions selects the outlines returned by CoverageOutline.
type OutlineOptions struct {
	// Band is an altitude band 0..geo.AltBands-1, OutlineOverall or
	// OutlineAllBands.
	Band int
	// Kind restricts the envelopes to "max" or "robust"; "" returns both.
	Kind string
}

// ParseOutlineBand parses the altBand query parameter of the coverage
// outline route: "all" = OutlineAllBands, "-1" = OutlineOverall, "0".."6" a
// single band. ok is false for anything else (the empty string included:
// the caller then keeps the CoverageGeoJSON default).
func ParseOutlineBand(s string) (band int, ok bool) {
	if s == "all" {
		return OutlineAllBands, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < OutlineOverall || n >= altBands {
		return 0, false
	}
	return n, true
}

// CoverageOutline is CoverageGeoJSON restricted to one altitude band
// (opt.Band = 0..geo.AltBands-1), to all altitudes together (OutlineOverall)
// or to one outline per band (OutlineAllBands); opt.Kind optionally keeps
// only the "max" or the "robust" envelope. Every feature carries band,
// label, altFt, loFt, hiFt (feet; null = open-ended), kind and ant. A band
// with fewer than three bearings with data yields no feature.
func (a *Analyzer) CoverageOutline(ctx context.Context, r Range, ant string, opt OutlineOptions) (*FeatureCollection, error) {
	if opt.Band < OutlineAllBands || opt.Band >= altBands {
		return nil, fmt.Errorf("altitude band %d out of range", opt.Band)
	}
	if opt.Kind != "" && opt.Kind != "max" && opt.Kind != "robust" {
		return nil, fmt.Errorf("unknown outline kind %q (max or robust)", opt.Kind)
	}
	return a.coverageFeatures(ctx, r, ant, opt)
}

func (a *Analyzer) coverageFeatures(ctx context.Context, r Range, ant string, opt OutlineOptions) (*FeatureCollection, error) {
	cov, err := a.Coverage(ctx, r, ant)
	if err != nil {
		return nil, err
	}
	fc := &FeatureCollection{Type: "FeatureCollection", Features: []Feature{}}
	if cov.Receiver == nil {
		return fc, nil
	}
	add := func(p *PolarBand) {
		if p.Bearings < 3 {
			return
		}
		var lo, hi any = 0.0, nil
		if p.Band >= 0 {
			l, h := geo.AltBandBoundsFt(p.Band)
			if isNum(l) {
				lo = l
			}
			if isNum(h) {
				hi = h
			}
		}
		for _, k := range [2]string{"max", "robust"} {
			if opt.Kind != "" && opt.Kind != k {
				continue
			}
			vals := p.MaxKm
			if k == "robust" {
				vals = p.RobustKm
			}
			ring := rangeRing(cov.Receiver.Lat, cov.Receiver.Lon, vals)
			if ring == nil {
				continue
			}
			fc.Features = append(fc.Features, Feature{
				Type: "Feature",
				Properties: map[string]any{"kind": k, "band": p.Band, "label": p.Label, "altFt": p.AltFt,
					"loFt": lo, "hiFt": hi, "ant": ant},
				Geometry: Geometry{Type: "Polygon", Coordinates: [][][2]float64{ring}},
			})
		}
	}
	switch b := opt.Band; {
	case b == outlineDefault:
		add(&cov.Overall)
		for i := range cov.Bands {
			add(&cov.Bands[i])
		}
	case b == OutlineAllBands:
		for i := range cov.Bands {
			add(&cov.Bands[i])
		}
	case b == OutlineOverall:
		add(&cov.Overall)
	case b >= 0 && b < len(cov.Bands):
		add(&cov.Bands[b])
	}
	return fc, nil
}

// rangeRing converts a per-bearing-bin range list to a closed
// counter-clockwise [lon, lat] ring around (lat, lon). Bins without data
// are left out, so the outline joins the neighbouring measured bearings
// instead of dipping to the receiver; nil when fewer than three bins have
// data (no area to draw).
func rangeRing(lat, lon float64, km []float64) [][2]float64 {
	n := len(km)
	ring := make([][2]float64, 0, n+1)
	for i := n - 1; i >= 0; i-- { // bearings run clockwise; GeoJSON wants CCW
		d := km[i]
		if !isNum(d) || d <= 0 {
			continue
		}
		la, lo := geo.Destination(lat, lon, (float64(i)+0.5)*360/float64(n), d)
		ring = append(ring, [2]float64{round(geo.NormalizeLon(lo), 5), round(la, 5)})
	}
	if len(ring) < 3 {
		return nil
	}
	return append(ring, ring[0])
}

// -------------------------------------------------------------- heat grid

// HeatOptions selects what Analyzer.HeatGrid exports.
type HeatOptions struct {
	MinCount int64 // drop cells with fewer positions (default 1)
	MaxCells int   // aggregate n x n cells until at most this many remain (default 20000)
	// Bounds restricts the export to [west, south, east, north] degrees.
	Bounds *[4]float64
}

// HeatGrid is the all-time position grid as compact parallel arrays.
type HeatGrid struct {
	Ant        string    `json:"ant"`
	CellDeg    float64   `json:"cellDeg"` // size of the exported cells (0.05 x Step)
	Step       int       `json:"step"`
	Cells      int       `json:"cells"`
	TotalCells int       `json:"totalCells"` // cells in the table for this antenna
	Positions  int64     `json:"positions"`
	MaxCount   int64     `json:"maxCount"`
	Lat        []float64 `json:"lat"` // cell centres
	Lon        []float64 `json:"lon"`
	Count      []int64   `json:"count"`
	RSSI       Nums      `json:"rssi"`   // mean dBFS
	MinAlt     Nums      `json:"minAlt"` // lowest altitude seen, ft = coverage floor
	MaxAlt     Nums      `json:"maxAlt"`
}

// HeatGrid exports the heat-map grid (count, mean RSSI, minimum / maximum
// altitude per cell) for a map layer.
func (a *Analyzer) HeatGrid(ctx context.Context, ant string, opt HeatOptions) (*HeatGrid, error) {
	if opt.MinCount < 1 {
		opt.MinCount = 1
	}
	if opt.MaxCells <= 0 {
		opt.MaxCells = 20000
	}
	key := fmt.Sprintf("heatgrid|%s|%d|%d", ant, opt.MinCount, opt.MaxCells)
	if opt.Bounds != nil {
		key += fmt.Sprintf("|%.2f,%.2f,%.2f,%.2f", opt.Bounds[0], opt.Bounds[1], opt.Bounds[2], opt.Bounds[3])
	}
	return cached(ctx, a, key, ttlCoverage, func(ctx context.Context) (*HeatGrid, error) {
		cells, err := a.loadHeat(ctx, ant)
		if err != nil {
			return nil, err
		}
		res := &HeatGrid{Ant: ant, TotalCells: len(cells), Step: 1}
		in := func(c *heatCell) bool {
			if opt.Bounds == nil {
				return true
			}
			w, s, e, n := opt.Bounds[0], opt.Bounds[1], opt.Bounds[2], opt.Bounds[3]
			if c.lat < s || c.lat > n {
				return false
			}
			if w <= e {
				return c.lon >= w && c.lon <= e
			}
			return c.lon >= w || c.lon <= e // box across the date line
		}
		var agg []heatCell
		for _, step := range [...]int{1, 2, 3, 4, 6, 8, 12, 16, 24, 32, 64} {
			agg = aggregateHeat(cells, step, in)
			res.Step = step
			if len(agg) <= opt.MaxCells {
				break
			}
		}
		res.CellDeg = round(geo.HeatCellDeg*float64(res.Step), 4)
		n := 0
		for i := range agg {
			if agg[i].count >= opt.MinCount {
				n++
			}
		}
		res.Lat, res.Lon, res.Count = make([]float64, 0, n), make([]float64, 0, n), make([]int64, 0, n)
		res.RSSI, res.MinAlt, res.MaxAlt = make(Nums, 0, n), make(Nums, 0, n), make(Nums, 0, n)
		for i := range agg {
			c := &agg[i]
			if c.count < opt.MinCount {
				continue
			}
			res.Lat = append(res.Lat, round(c.lat, 4))
			res.Lon = append(res.Lon, round(c.lon, 4))
			res.Count = append(res.Count, c.count)
			res.RSSI = append(res.RSSI, round1(c.rssi))
			res.MinAlt = append(res.MinAlt, c.minAlt)
			res.MaxAlt = append(res.MaxAlt, c.maxAlt)
			res.Positions += c.count
			res.MaxCount = max(res.MaxCount, c.count)
		}
		res.Cells = len(res.Lat)
		return res, nil
	})
}

func round1(v float64) float64 {
	if !isNum(v) {
		return math.NaN()
	}
	return math.Round(v*10) / 10
}

// aggregateHeat merges step x step blocks of cells (step 1 = filter only).
func aggregateHeat(cells []heatCell, step int, keep func(*heatCell) bool) []heatCell {
	if step <= 1 {
		out := make([]heatCell, 0, len(cells))
		for i := range cells {
			if keep == nil || keep(&cells[i]) {
				out = append(out, cells[i])
			}
		}
		return out
	}
	idx := map[uint32]int{}
	var out []heatCell
	for i := range cells {
		c := &cells[i]
		if keep != nil && !keep(c) {
			continue
		}
		la, lo := c.latIdx/step, c.lonIdx/step
		k := uint32(la)<<16 | uint32(lo)
		j, ok := idx[k]
		if !ok {
			j = len(out)
			idx[k] = j
			out = append(out, heatCell{
				latIdx: la * step, lonIdx: lo * step,
				lat:  (float64(la*step)+float64(step)/2)*geo.HeatCellDeg - 90,
				lon:  (float64(lo*step)+float64(step)/2)*geo.HeatCellDeg - 180,
				rssi: 0, minAlt: math.NaN(), maxAlt: math.NaN(),
			})
		}
		o := &out[j]
		o.count += c.count
		if c.rssiN > 0 && isNum(c.rssi) {
			o.rssi += c.rssi * float64(c.rssiN)
			o.rssiN += c.rssiN
		}
		o.minAlt = nanMin(o.minAlt, c.minAlt)
		o.maxAlt = nanMax(o.maxAlt, c.maxAlt)
		o.lastT = max(o.lastT, c.lastT)
	}
	for i := range out {
		if out[i].rssiN > 0 {
			out[i].rssi /= float64(out[i].rssiN)
		} else {
			out[i].rssi = math.NaN()
		}
	}
	return out
}
