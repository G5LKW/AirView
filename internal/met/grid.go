package met

import (
	"math"
	"sort"
	"sync"

	"airview/internal/store"
)

// Gridded winds and temperatures aloft from the stored aircraft
// observations (met_sample): every observation within a flight-level band
// and a time window is put into a latitude/longitude cell; a cell reports
// the vector-mean wind (so opposite winds cancel instead of averaging their
// speeds), the mean static air temperature and its deviation from the ICAO
// standard atmosphere at the band's level, and how many observations and
// aircraft it rests on. Nothing is interpolated or extrapolated: a cell
// without observations is simply absent.

// GridQuery selects the observations of a grid.
type GridQuery struct {
	LevelFt float64 // centre of the band, ft pressure altitude (e.g. 30000 = FL300)
	BandFt  float64 // half width of the band (default 2000 ft)
	CellDeg float64 // cell size in latitude degrees; longitude cells are widened by 1/cos(lat) (default 0.5)
	MinObs  int     // observations a cell needs (default 2)
}

// GridCell is one cell of the grid.
type GridCell struct {
	Lat     float64  `json:"lat"` // cell centre
	Lon     float64  `json:"lon"`
	WindDir *float64 `json:"windDir,omitempty"` // deg true, FROM
	WindKt  *float64 `json:"windKt,omitempty"`
	TempC   *float64 `json:"tempC,omitempty"`   // mean static air temperature
	ISADevC *float64 `json:"isaDevC,omitempty"` // tempC minus the ISA temperature at the band's level
	NWind   int      `json:"nWind"`
	NTemp   int      `json:"nTemp"`
	NAc     int      `json:"nAc"` // distinct aircraft
	AltFt   float64  `json:"altFt"`
}

// Grid is the result.
type Grid struct {
	From    int64      `json:"from"` // Unix ms
	To      int64      `json:"to"`
	LevelFt float64    `json:"levelFt"`
	BandFt  float64    `json:"bandFt"`
	CellDeg float64    `json:"cellDeg"`
	Samples int        `json:"samples"` // observations in the band and window
	Cells   []GridCell `json:"cells"`
}

// ISATempC is the ICAO standard atmosphere temperature at a pressure
// altitude (troposphere lapse 1.98 °C / 1000 ft, isothermal -56.5 °C above
// 36 089 ft).
func ISATempC(altFt float64) float64 {
	if altFt >= 36089 {
		return -56.5
	}
	return 15 - 1.98*altFt/1000
}

type gacc struct {
	u, v, t, alt float64
	nw, nt, na   int
	ac           map[uint32]struct{}
}

// BuildGrid grids the observations (from, to in Unix ms).
func BuildGrid(obs []store.MetObs, q GridQuery, from, to int64) Grid {
	if q.BandFt <= 0 {
		q.BandFt = 2000
	}
	if q.CellDeg <= 0 {
		q.CellDeg = 0.5
	}
	q.CellDeg = math.Max(0.1, math.Min(5, q.CellDeg))
	if q.MinObs <= 0 {
		q.MinObs = 2
	}
	g := Grid{From: from, To: to, LevelFt: q.LevelFt, BandFt: q.BandFt, CellDeg: q.CellDeg, Cells: []GridCell{}}
	type key struct{ i, j int }
	cells := map[key]*gacc{}
	for k := range obs {
		o := &obs[k]
		if o.T*1000 < from || o.T*1000 > to || math.Abs(o.Alt-q.LevelFt) > q.BandFt || math.IsNaN(o.Lat) || math.IsNaN(o.Lon) {
			continue
		}
		hasW := !math.IsNaN(o.WindDir) && !math.IsNaN(o.WindKt)
		hasT := !math.IsNaN(o.OATC)
		if !hasW && !hasT {
			continue
		}
		g.Samples++
		i := int(math.Floor(o.Lat / q.CellDeg))
		latC := (float64(i) + 0.5) * q.CellDeg
		lonDeg := q.CellDeg / math.Max(0.2, math.Cos(latC*math.Pi/180))
		j := int(math.Floor(o.Lon / lonDeg))
		a := cells[key{i, j}]
		if a == nil {
			a = &gacc{ac: map[uint32]struct{}{}}
			cells[key{i, j}] = a
		}
		a.ac[o.ICAO] = struct{}{}
		a.alt += o.Alt
		a.na++
		if hasW {
			// U/V of the air motion (the wind blows TOWARDS dir+180)
			r := o.WindDir * math.Pi / 180
			a.u -= o.WindKt * math.Sin(r)
			a.v -= o.WindKt * math.Cos(r)
			a.nw++
		}
		if hasT {
			// reduce to the band centre with the standard lapse rate
			a.t += o.OATC + (ISATempC(q.LevelFt) - ISATempC(o.Alt))
			a.nt++
		}
	}
	for k, a := range cells {
		if a.nw < q.MinObs && a.nt < q.MinObs {
			continue
		}
		latC := (float64(k.i) + 0.5) * q.CellDeg
		lonDeg := q.CellDeg / math.Max(0.2, math.Cos(latC*math.Pi/180))
		c := GridCell{Lat: round2(latC), Lon: round2((float64(k.j) + 0.5) * lonDeg), NWind: a.nw, NTemp: a.nt, NAc: len(a.ac), AltFt: math.Round(a.alt / float64(a.na))}
		if a.nw >= q.MinObs {
			u, v := a.u/float64(a.nw), a.v/float64(a.nw)
			spd := math.Hypot(u, v)
			dir := math.Mod(math.Atan2(-u, -v)*180/math.Pi+360, 360)
			spd, dir = round1(spd), math.Round(dir)
			c.WindKt, c.WindDir = &spd, &dir
		}
		if a.nt >= q.MinObs {
			t := round1(a.t / float64(a.nt))
			d := round1(t - ISATempC(q.LevelFt))
			c.TempC, c.ISADevC = &t, &d
		}
		g.Cells = append(g.Cells, c)
	}
	sort.Slice(g.Cells, func(i, j int) bool {
		if g.Cells[i].Lat != g.Cells[j].Lat {
			return g.Cells[i].Lat < g.Cells[j].Lat
		}
		return g.Cells[i].Lon < g.Cells[j].Lon
	})
	return g
}

// ---------------------------------------------------------------- hazards

// Hazard is one BDS 4,5 meteorological hazard report. BDS 4,5 carries no
// identifying bits: a reply is only taken as 4,5 when register inference
// picks it, which is rare and less certain than for the other registers;
// the reports are shown as received and never enter the profiles.
type Hazard struct {
	T           int64    `json:"t"` // Unix ms
	ICAO        string   `json:"icao"`
	Callsign    string   `json:"callsign,omitempty"`
	Lat         *float64 `json:"lat,omitempty"`
	Lon         *float64 `json:"lon,omitempty"`
	AltFt       *float64 `json:"altFt,omitempty"`
	Turbulence  *uint8   `json:"turbulence,omitempty"` // 0 nil, 1 light, 2 moderate, 3 severe
	WindShear   *uint8   `json:"windShear,omitempty"`
	Microburst  *uint8   `json:"microburst,omitempty"`
	Icing       *uint8   `json:"icing,omitempty"`
	WakeVortex  *uint8   `json:"wakeVortex,omitempty"`
	TempC       *float64 `json:"tempC,omitempty"`
	PressureHPa *float64 `json:"pressureHpa,omitempty"`
	RadioHtFt   *int32   `json:"radioHeightFt,omitempty"`
}

// Severe reports whether any hazard is moderate or severe.
func (h *Hazard) Severe() bool {
	for _, p := range []*uint8{h.Turbulence, h.WindShear, h.Microburst, h.Icing, h.WakeVortex} {
		if p != nil && *p >= 2 {
			return true
		}
	}
	return false
}

// HazardLog keeps the recent hazard reports in memory (one per aircraft
// per minute).
type HazardLog struct {
	mu   sync.Mutex
	list []Hazard
	last map[string]int64
	n    uint64
}

const (
	hazardMax   = 2000
	hazardEvery = 60_000
)

// NewHazardLog creates an empty log.
func NewHazardLog() *HazardLog { return &HazardLog{last: map[string]int64{}} }

// Add records a report (thinned to one per aircraft per minute).
func (l *HazardLog) Add(h Hazard) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[h.ICAO]; ok && h.T-t < hazardEvery && h.T >= t {
		return
	}
	l.last[h.ICAO] = h.T
	l.n++
	l.list = append(l.list, h)
	if len(l.list) > hazardMax {
		l.list = append(l.list[:0], l.list[len(l.list)-hazardMax:]...)
	}
	if len(l.last) > 4*hazardMax {
		clear(l.last)
	}
}

// Since returns the reports since from (Unix ms), newest first.
func (l *HazardLog) Since(from int64) []Hazard {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Hazard{}
	for i := len(l.list) - 1; i >= 0; i-- {
		if l.list[i].T >= from {
			out = append(out, l.list[i])
		}
	}
	return out
}

// Count returns the number of reports recorded.
func (l *HazardLog) Count() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}
