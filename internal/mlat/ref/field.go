package ref

import (
	"math"
	"sync"

	"airview/internal/geoid"
)

// FieldOptions configures the GNSS−baro field (DESIGN §9.6).
type FieldOptions struct {
	// CellKm is the horizontal cell size (default 50 km) and BandFt the
	// altitude band (default 2000 ft of pressure altitude).
	CellKm float64
	BandFt float64
	// DriftMPerH is the random-walk rate of a cell (default 20 m per √h):
	// how fast the GNSS−baro difference of a region changes with the
	// weather. A cell without data loses accuracy at this rate until it is
	// no better than the fallback model, which it then returns (doc.go
	// "GNSS−baro field").
	DriftMPerH float64
	// GradientMPerCell and GradientMPerBand are the representativeness
	// errors of a cell value used at a distance of one cell horizontally
	// or one band vertically (defaults 5 m and 12 m).
	GradientMPerCell float64
	GradientMPerBand float64
	// ObsSigmaM is the 1σ of one decimated per-aircraft observation
	// (default 12 m: airframe altimetry-system error and GNSS vertical
	// error, which do not average out within one aircraft).
	ObsSigmaM float64
	// DecimateS: one aircraft contributes at most one observation per cell
	// per DecimateS (default 30 s), the mean of its reports.
	DecimateS float64
	// FallbackSigmaM is the height-independent 1σ of the fallback model
	// with surface weather (default 60 m: surface pressure and geoid) and
	// NoWeatherSigmaM without (default 150 m: unknown QNH).
	FallbackSigmaM  float64
	NoWeatherSigmaM float64
	// FallbackSigmaTK (default 6 K, surface temperature known) and
	// NoWeatherSigmaTK (default 11 K) are the 1σ of the column's mean
	// temperature deviation from the model. The geometric height of a
	// pressure level scales with the mean column temperature, so this part
	// grows with height: σ_T·h_p/T_mean(h_p), about 120 m at FL100 and
	// 520 m at FL390 without weather (finding M3). The fallback σ is
	// √(σ_base² + (h_p·σ_T/T_mean)²).
	FallbackSigmaTK  float64
	NoWeatherSigmaTK float64
	// ProfileSigmaM (default 25 m: geoid, model analysis and the hour's
	// interpolation) and ProfileSigmaTK (default 1.5 K) are the fallback σ
	// parts when the Weather source also gives a pressure-level profile
	// (WeatherProfile): the height of the aircraft's isobaric surface is
	// then taken from the analysed column instead of an ISA column.
	ProfileSigmaM  float64
	ProfileSigmaTK float64
	// MaxCells bounds the memory (default 200000 cells, about 13 MB);
	// beyond it no new cells are created until Prune frees some.
	MaxCells int
}

func (o FieldOptions) withDefaults() FieldOptions {
	def := func(v *float64, d float64) {
		if !(*v > 0) {
			*v = d
		}
	}
	def(&o.CellKm, 50)
	def(&o.BandFt, 2000)
	def(&o.DriftMPerH, 20)
	def(&o.GradientMPerCell, 5)
	def(&o.GradientMPerBand, 12)
	def(&o.ObsSigmaM, 12)
	def(&o.DecimateS, 30)
	def(&o.FallbackSigmaM, 60)
	def(&o.NoWeatherSigmaM, 150)
	def(&o.FallbackSigmaTK, 6)
	def(&o.NoWeatherSigmaTK, 11)
	def(&o.ProfileSigmaM, 25)
	def(&o.ProfileSigmaTK, 1.5)
	if o.MaxCells <= 0 {
		o.MaxCells = 200000
	}
	return o
}

// Weather supplies surface weather for the fallback model: QNH (hPa) and
// temperature (°C) near a position. ok false = unknown. Implementations
// must be safe for concurrent use and fast (cache per station/cell).
type Weather interface {
	SurfaceAt(lat, lon float64, tNs int64) (qnhHPa, tempC float64, ok bool)
}

// AltSource says where a height conversion came from.
type AltSource uint8

const (
	AltNone     AltSource = iota
	AltGNSS               // reported GNSS height (TC 20-22)
	AltOwnDelta           // barometric + the aircraft's own GNSS−baro difference (TC 19)
	AltField              // barometric + the learned regional field
	AltFallback           // barometric + QNH/temperature/geoid model (or geoid only)
)

func (s AltSource) String() string {
	switch s {
	case AltGNSS:
		return "gnss"
	case AltOwnDelta:
		return "own-delta"
	case AltField:
		return "field"
	case AltFallback:
		return "fallback"
	}
	return "none"
}

const ftM = 0.3048

type cellKey struct {
	i, j int32 // latitude row, longitude column within the row
	b    int16 // altitude band
}

type cell struct {
	x, p float64 // GNSS−baro difference (m) and its variance
	t    int64   // last update, ns
	n    uint32  // observations
}

// Field is the regional GNSS−baro height difference: geometric height above
// the WGS-84 ellipsoid minus pressure altitude, learned per 50 km × 2000 ft
// cell with a scalar Kalman filter (doc.go "GNSS−baro field"). It converts
// Mode S pressure altitudes into heights above the ellipsoid for the
// solver's altitude row and for reference tracks without own GNSS data.
// Safe for concurrent use.
type Field struct {
	opt     FieldOptions
	latStep float64 // degrees
	mu      sync.RWMutex
	cells   map[cellKey]*cell
	wx      Weather
	geoidOK bool
}

// NewField returns an empty field. wx may be nil (no surface weather).
func NewField(opt FieldOptions, wx Weather) *Field {
	opt = opt.withDefaults()
	return &Field{
		opt:     opt,
		latStep: opt.CellKm / 111.2,
		cells:   make(map[cellKey]*cell),
		wx:      wx,
		geoidOK: geoid.Available(),
	}
}

// SetWeather replaces the surface-weather source (nil = none).
func (f *Field) SetWeather(wx Weather) {
	f.mu.Lock()
	f.wx = wx
	f.mu.Unlock()
}

// rowLonStep is the longitude step of latitude row i, chosen so that cells
// stay about CellKm wide.
func (f *Field) rowLonStep(i int32) float64 {
	c := math.Cos((float64(i) + 0.5) * f.latStep * math.Pi / 180)
	if c < 0.05 {
		c = 0.05
	}
	return math.Min(f.latStep/c, 360)
}

func (f *Field) key(lat, lon, baroFt float64) cellKey {
	i := int32(math.Floor(lat / f.latStep))
	j := int32(math.Floor(wrap180(lon) / f.rowLonStep(i)))
	b := int16(math.Floor(baroFt / f.opt.BandFt))
	return cellKey{i, j, b}
}

// center returns the cell centre (lat, lon, baro ft).
func (f *Field) center(k cellKey) (lat, lon, baroFt float64) {
	lat = (float64(k.i) + 0.5) * f.latStep
	lon = (float64(k.j) + 0.5) * f.rowLonStep(k.i)
	baroFt = (float64(k.b) + 0.5) * f.opt.BandFt
	return
}

// Fallback returns the model GNSS−baro difference (m) without learned data:
// the QNH/temperature (ISA) conversion of pressure altitude to height above
// mean sea level when Weather knows the surface weather, plus the EGM96
// geoid undulation. sigma is its 1σ.
func (f *Field) Fallback(lat, lon, baroFt float64, tNs int64) (deltaM, sigmaM float64) {
	f.mu.RLock()
	wx := f.wx
	f.mu.RUnlock()
	return f.fallback(wx, lat, lon, baroFt, tNs)
}

// columnSigma is the height-dependent part of the fallback σ (m): a mean
// column temperature error sigmaTK (K) scales the geometric thickness of
// the hpM metres of pressure altitude below the aircraft by σ_T/T_mean.
func columnSigma(hpM, sigmaTK float64) float64 {
	if !(hpM > 0) {
		return 0
	}
	return hpM * sigmaTK / isaMeanTemp(hpM)
}

// isaMeanTemp is the mean ISA temperature (K) of the layer from sea level
// to the pressure altitude hpM (troposphere lapse 6.5 K/km to 11 km, then
// isothermal 216.65 K), by height.
func isaMeanTemp(hpM float64) float64 {
	const (
		t0 = 288.15
		tt = 216.65
		ht = 11000.0
	)
	if hpM <= ht {
		return t0 - 0.0065*hpM/2
	}
	return (ht*(t0+tt)/2 + (hpM-ht)*tt) / hpM
}

// sigmaAt is the fallback model's 1σ at a pressure altitude (m) for a
// height-independent part base and a column temperature σ sigmaTK.
func sigmaAt(base, sigmaTK, hpM float64) float64 {
	return math.Hypot(base, columnSigma(hpM, sigmaTK))
}

func (f *Field) fallback(wx Weather, lat, lon, baroFt float64, tNs int64) (deltaM, sigmaM float64) {
	n := 0.0
	sigma := f.opt.NoWeatherSigmaM
	sigT := f.opt.NoWeatherSigmaTK
	if f.geoidOK {
		if u, ok := geoid.Undulation(lat, lon); ok {
			n = u
		}
	}
	hp := baroFt * ftM
	msl := hp
	if prof, ok := wx.(WeatherProfile); ok {
		if h, ok := prof.MSLHeightOfPressure(lat, lon, isaPressure(hp), tNs); ok && math.Abs(h-hp) < 1500 {
			sigma, sigT := f.opt.ProfileSigmaM, f.opt.ProfileSigmaTK
			if !f.geoidOK {
				sigma = math.Hypot(sigma, 30)
			}
			return h + n - hp, sigmaAt(sigma, sigT, hp)
		}
	}
	if wx != nil {
		if qnh, t, ok := wx.SurfaceAt(lat, lon, tNs); ok && qnh > 850 && qnh < 1100 {
			msl = PressureToMSL(hp, qnh, t)
			sigma = f.opt.FallbackSigmaM
			sigT = f.opt.FallbackSigmaTK
		}
	}
	if !f.geoidOK {
		sigma = math.Hypot(sigma, 30)
	}
	return msl + n - hp, sigmaAt(sigma, sigT, hp)
}

// PressureToMSL converts a pressure altitude (m, ISA 1013.25 hPa datum) to
// a height above mean sea level for a surface pressure qnhHPa and surface
// temperature tempC, using the ISA troposphere for the pressure and a mean
// layer temperature correction (doc.go "GNSS−baro field").
func PressureToMSL(hpM, qnhHPa, tempC float64) float64 {
	const (
		t0 = 288.15
		l  = 0.0065
		ex = 5.25588 // g·M/(R·L)
	)
	// pressure at the aircraft
	p := 1013.25 * math.Pow(1-l*hpM/t0, ex)
	// ISA height above the QNH level
	h := t0 / l * (1 - math.Pow(p/qnhHPa, 1/ex))
	// temperature correction: the layer between sea level and h is warmer
	// or colder than ISA by dT; geometric thickness scales with the mean
	// absolute temperature of the layer.
	dT := tempC - 15
	tm := t0 - l*h/2
	if tm < 150 {
		tm = 150
	}
	return h + dT*h/tm
}

// Update adds one observation: the GNSS−baro difference deltaM (m) of an
// aircraft at lat/lon and pressure altitude baroFt, with 1σ sigmaM
// (0 = FieldOptions.ObsSigmaM), at tNs. Callers decimate per aircraft (the
// Store does).
func (f *Field) Update(lat, lon, baroFt, deltaM, sigmaM float64, tNs int64) {
	if math.IsNaN(deltaM) || math.IsNaN(lat) || math.IsNaN(lon) || math.Abs(deltaM) > 1500 {
		return
	}
	if !(sigmaM > 0) {
		sigmaM = f.opt.ObsSigmaM
	}
	k := f.key(lat, lon, baroFt)
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cells[k]
	if c == nil {
		if len(f.cells) >= f.opt.MaxCells {
			return
		}
		clat, clon, cb := f.center(k)
		m, s := f.fallback(f.wx, clat, clon, cb, tNs)
		c = &cell{x: m, p: s * s, t: tNs}
		f.cells[k] = c
	} else {
		f.relax(c, k, tNs)
	}
	// The observation is taken at baroFt, the cell value is the difference
	// at the band centre: move the observation along the fallback model's
	// vertical slope (the ISA temperature term) to the centre.
	_, _, cb := f.center(k)
	z := deltaM + f.slope(lat, lon, tNs)*(cb-baroFt)*ftM
	r := sigmaM * sigmaM
	s := c.p + r
	g := c.p / s
	c.x += g * (z - c.x)
	c.p -= g * c.p
	c.n++
	if tNs > c.t {
		c.t = tNs
	}
}

// slope is d(delta)/d(height) of the fallback model (dimensionless).
func (f *Field) slope(lat, lon float64, tNs int64) float64 {
	wx := f.wx
	if wx == nil {
		return 0
	}
	qnh, t, ok := wx.SurfaceAt(lat, lon, tNs)
	if !ok {
		return 0
	}
	a := PressureToMSL(3000, qnh, t)
	b := PressureToMSL(6000, qnh, t)
	return (b-a)/3000 - 1
}

// q returns the random-walk variance rate of a cell, m²/s.
func (f *Field) q() float64 {
	return f.opt.DriftMPerH * f.opt.DriftMPerH / 3600
}

// relax applies the random-walk time update to a cell (caller holds mu).
// Once the cell is no better than the fallback model it is reset to it.
func (f *Field) relax(c *cell, k cellKey, tNs int64) {
	dt := float64(tNs-c.t) / 1e9
	if dt <= 0 {
		return
	}
	c.p += f.q() * dt
	clat, clon, cb := f.center(k)
	m, s := f.fallback(f.wx, clat, clon, cb, tNs)
	if c.p >= s*s {
		c.x, c.p = m, s*s
	}
	c.t = tNs
}

// cellAt returns a cell's value propagated to tNs without modifying it, or
// the fallback when the cell is empty or no better than the fallback.
// data reports whether learned data was used.
func (f *Field) cellAt(k cellKey, tNs int64) (x, p float64, data bool) {
	clat, clon, cb := f.center(k)
	m, s := f.fallback(f.wx, clat, clon, cb, tNs)
	c := f.cells[k]
	if c == nil {
		return m, s * s, false
	}
	p = c.p
	if dt := float64(tNs-c.t) / 1e9; dt > 0 {
		p += f.q() * dt
	}
	if p >= s*s {
		return m, s * s, false
	}
	return c.x, p, true
}

// Offset returns the GNSS−baro difference (m) at a position and pressure
// altitude from the eight surrounding cell centres (two latitude rows, two
// longitude columns, two altitude bands). Each cell is weighted by its
// bilinear weight over its variance, the variance including a
// representativeness term for the distance from the cell centre; empty
// cells contribute the fallback model. sigma is the conservative (fully
// correlated) combination. src is AltField when learned cells carry at
// least half of the weight, else AltFallback.
func (f *Field) Offset(lat, lon, baroFt float64, tNs int64) (deltaM, sigmaM float64, src AltSource) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	u := lat/f.latStep - 0.5
	i0 := int32(math.Floor(u))
	fu := u - float64(i0)
	vb := baroFt/f.opt.BandFt - 0.5
	b0 := int16(math.Floor(vb))
	fb := vb - float64(b0)
	lon = wrap180(lon)
	slope := f.slope(lat, lon, tNs)
	gc, gb := f.opt.GradientMPerCell, f.opt.GradientMPerBand
	var sw, sx, ss, wData float64
	for di := int32(0); di < 2; di++ {
		i := i0 + di
		wi, du := 1-fu, fu
		if di == 1 {
			wi, du = fu, 1-fu
		}
		st := f.rowLonStep(i)
		v := lon/st - 0.5
		j0 := int32(math.Floor(v))
		fv := v - float64(j0)
		for dj := int32(0); dj < 2; dj++ {
			wj, dv := 1-fv, fv
			if dj == 1 {
				wj, dv = fv, 1-fv
			}
			for db := int16(0); db < 2; db++ {
				wb, dz := 1-fb, fb
				if db == 1 {
					wb, dz = fb, 1-fb
				}
				b := wi * wj * wb
				if b < 1e-9 {
					continue
				}
				k := cellKey{i, j0 + dj, b0 + db}
				x, p, data := f.cellAt(k, tNs)
				_, _, cb := f.center(k)
				x += slope * (baroFt - cb) * ftM
				p += gc*gc*(du*du+dv*dv) + gb*gb*dz*dz
				w := b / p
				sw += w
				sx += w * x
				ss += w * math.Sqrt(p)
				if data {
					wData += w
				}
			}
		}
	}
	src = AltFallback
	if wData >= 0.5*sw {
		src = AltField
	}
	return sx / sw, ss / sw, src
}

// BaroToHAE converts a pressure altitude (ft) at a position to a height
// above the ellipsoid (m) with its 1σ.
func (f *Field) BaroToHAE(lat, lon, baroFt float64, tNs int64) (haeM, sigmaM float64, src AltSource) {
	d, s, src := f.Offset(lat, lon, baroFt, tNs)
	return baroFt*ftM + d, s, src
}

// FieldStats summarises the field.
type FieldStats struct {
	Cells        int     `json:"cells"`
	Observations uint64  `json:"observations"`
	MeanSigmaM   float64 `json:"meanSigmaM"` // over cells with data, at their last update
}

// Stats returns a summary.
func (f *Field) Stats() FieldStats {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var s FieldStats
	s.Cells = len(f.cells)
	var sum float64
	for _, c := range f.cells {
		s.Observations += uint64(c.n)
		sum += math.Sqrt(c.p)
	}
	if s.Cells > 0 {
		s.MeanSigmaM = sum / float64(s.Cells)
	}
	return s
}

// Prune drops cells that have drifted to the accuracy of the no-weather
// fallback at their altitude band since their last update (they carry no
// information any more).
func (f *Field) Prune(nowNs int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.q()
	n := 0
	for k, c := range f.cells {
		hp := (float64(k.b) + 0.5) * f.opt.BandFt * ftM
		s := sigmaAt(f.opt.NoWeatherSigmaM, f.opt.NoWeatherSigmaTK, hp)
		if c.p+q*float64(nowNs-c.t)/1e9 >= s*s {
			delete(f.cells, k)
			n++
		}
	}
	return n
}

func wrap180(lon float64) float64 {
	if lon >= -180 && lon < 180 {
		return lon
	}
	lon = math.Mod(lon+180, 360)
	if lon < 0 {
		lon += 360
	}
	return lon - 180
}
