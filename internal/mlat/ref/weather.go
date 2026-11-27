package ref

import (
	"math"
	"sync"
	"time"

	"airview/internal/weather"
)

// WeatherProfile is optionally implemented by a Weather source: the height
// above mean sea level (m, geometric) of the isobaric surface pHPa near a
// position, from an analysed pressure-level profile. With it the GNSS−baro
// fallback converts a pressure altitude through the real atmosphere instead
// of the ISA column with a surface correction (FieldOptions.ProfileSigmaM /
// ProfileSigmaTK).
type WeatherProfile interface {
	MSLHeightOfPressure(lat, lon, pHPa float64, tNs int64) (hM float64, ok bool)
}

// BundleWeather adapts AirView's weather package (internal/weather, the
// Open-Meteo bundles the station weather poller already fetches) to the
// field's Weather and WeatherProfile: surface pressure reduced to mean sea
// level (pressure_msl, used as QNH), 2 m temperature and the
// pressure-level geopotential heights. internal/weather has no METAR source
// (HOOKS H-clock -> internal/weather); a model analysis at the station is
// what the fallback needs anyway: the QNH of the area and the temperature
// of the column, at hourly resolution.
//
// Bundles are kept per location (Set replaces a bundle within 5 km of a
// stored one); a query uses the nearest bundle within MaxDistKm whose
// hourly data cover the instant, interpolated linearly between the two
// hours around it. Safe for concurrent use; queries do not allocate.
type BundleWeather struct {
	// MaxDistKm is the largest distance from a bundle's location at which
	// it answers (default 250 km, the scale of synoptic pressure patterns
	// that matter at the 60 m fallback level).
	MaxDistKm float64
	// MaxBundles bounds the number of locations kept (default 512; the
	// oldest fetch is dropped first).
	MaxBundles int

	mu sync.RWMutex
	b  []*weather.Bundle
}

// NewBundleWeather returns an empty adapter with the defaults.
func NewBundleWeather() *BundleWeather {
	return &BundleWeather{MaxDistKm: 250, MaxBundles: 512}
}

// Set stores (or replaces) the bundle of its location. nil is ignored.
func (w *BundleWeather) Set(b *weather.Bundle) {
	if b == nil || len(b.Hourly) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, o := range w.b {
		if surfDist(o.Lat, o.Lon, b.Lat, b.Lon) < 5000 {
			w.b[i] = b
			return
		}
	}
	if lim := w.maxBundles(); len(w.b) >= lim {
		oldest := 0
		for i, o := range w.b {
			if o.Fetched < w.b[oldest].Fetched {
				oldest = i
			}
		}
		w.b[oldest] = b
		return
	}
	w.b = append(w.b, b)
}

// Len returns the number of stored locations.
func (w *BundleWeather) Len() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.b)
}

func (w *BundleWeather) maxBundles() int {
	if w.MaxBundles > 0 {
		return w.MaxBundles
	}
	return 512
}

// nearest returns the nearest bundle within MaxDistKm (caller holds mu).
func (w *BundleWeather) nearest(lat, lon float64) *weather.Bundle {
	lim := w.MaxDistKm
	if !(lim > 0) {
		lim = 250
	}
	var best *weather.Bundle
	bd := lim * 1000
	for _, b := range w.b {
		if d := surfDist(lat, lon, b.Lat, b.Lon); d <= bd {
			best, bd = b, d
		}
	}
	return best
}

// hours returns the hourly points around tNs and the interpolation weight of
// the second (0 when only the first applies).
func hours(b *weather.Bundle, tNs int64) (a, c *weather.HourPoint, f float64, ok bool) {
	t := time.Unix(0, tNs)
	i := b.Index(t)
	if i < 0 {
		return nil, nil, 0, false
	}
	a = &b.Hourly[i]
	if i+1 < len(b.Hourly) {
		c = &b.Hourly[i+1]
		f = float64(t.UnixMilli()-a.T) / float64(c.T-a.T)
	}
	return a, c, f, true
}

// lerp interpolates two optional values (the first alone when the second is
// missing).
func lerp(a, c weather.Num, f float64, haveC bool) (float64, bool) {
	if !a.Valid() {
		return 0, false
	}
	if !haveC || !c.Valid() || f <= 0 {
		return float64(a), true
	}
	return float64(a)*(1-f) + float64(c)*f, true
}

// SurfaceAt implements Weather: QNH (pressure reduced to mean sea level,
// hPa) and the 2 m temperature (°C).
func (w *BundleWeather) SurfaceAt(lat, lon float64, tNs int64) (qnhHPa, tempC float64, ok bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	b := w.nearest(lat, lon)
	if b == nil {
		return 0, 0, false
	}
	a, c, f, ok := hours(b, tNs)
	if !ok {
		return 0, 0, false
	}
	var cp, ct weather.Num
	if c != nil {
		cp, ct = c.PressureMSL, c.TempC
	}
	qnh, ok1 := lerp(a.PressureMSL, cp, f, c != nil)
	temp, ok2 := lerp(a.TempC, ct, f, c != nil)
	if !ok1 || !ok2 || qnh < 850 || qnh > 1100 {
		return 0, 0, false
	}
	return qnh, temp, true
}

// earthRgpm is the effective earth radius of the geopotential-to-geometric
// height conversion z = R·H/(R − H) (m).
const earthRgpm = 6356766.0

// levelHeight returns the geometric height (m above mean sea level) of the
// pressure pHPa in one hour's profile. Height is nearly linear in log
// pressure (exactly so in an isothermal layer); the curvature from the
// lapse rate is taken by a quadratic through the two levels around pHPa
// and the next one (linear in log pressure when only two exist).
func levelHeight(h *weather.HourPoint, pHPa float64) (float64, bool) {
	lp := math.Log(pHPa)
	lo, hi := -1, -1 // lo: nearest level at higher pressure (below), hi: at lower pressure (above)
	for i := range h.Levels {
		l := &h.Levels[i]
		if !l.HeightM.Valid() || !(l.HPa > 0) {
			continue
		}
		if l.HPa >= pHPa && (lo < 0 || l.HPa < h.Levels[lo].HPa) {
			lo = i
		}
		if l.HPa <= pHPa && (hi < 0 || l.HPa > h.Levels[hi].HPa) {
			hi = i
		}
	}
	if lo < 0 || hi < 0 {
		return 0, false
	}
	var gh float64
	if lo == hi {
		gh = float64(h.Levels[lo].HeightM)
	} else {
		x0, y0 := math.Log(h.Levels[lo].HPa), float64(h.Levels[lo].HeightM)
		x1, y1 := math.Log(h.Levels[hi].HPa), float64(h.Levels[hi].HeightM)
		gh = y0 + (lp-x0)*(y1-y0)/(x1-x0)
		// third level: the nearest further one on either side
		third := -1
		for i := range h.Levels {
			l := &h.Levels[i]
			if i == lo || i == hi || !l.HeightM.Valid() || !(l.HPa > 0) {
				continue
			}
			if third < 0 || math.Abs(math.Log(l.HPa)-lp) < math.Abs(math.Log(h.Levels[third].HPa)-lp) {
				third = i
			}
		}
		if third >= 0 {
			x2, y2 := math.Log(h.Levels[third].HPa), float64(h.Levels[third].HeightM)
			// Newton form: add the second divided difference term
			d01 := (y1 - y0) / (x1 - x0)
			d12 := (y2 - y1) / (x2 - x1)
			gh += (lp - x0) * (lp - x1) * (d12 - d01) / (x2 - x0)
		}
	}
	return earthRgpm * gh / (earthRgpm - gh), true
}

// MSLHeightOfPressure implements WeatherProfile from the bundle's
// pressure-level profile (time-interpolated between the hours around tNs).
func (w *BundleWeather) MSLHeightOfPressure(lat, lon, pHPa float64, tNs int64) (float64, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	b := w.nearest(lat, lon)
	if b == nil {
		return 0, false
	}
	a, c, f, ok := hours(b, tNs)
	if !ok {
		return 0, false
	}
	ha, ok := levelHeight(a, pHPa)
	if !ok {
		return 0, false
	}
	if c != nil && f > 0 {
		if hc, ok := levelHeight(c, pHPa); ok {
			return ha*(1-f) + hc*f, true
		}
	}
	return ha, true
}

// isaPressure is the ISA pressure (hPa) at the pressure altitude hpM:
// troposphere to 11 km, isothermal stratosphere above.
func isaPressure(hpM float64) float64 {
	const (
		t0 = 288.15
		l  = 0.0065
		ex = 5.25588
	)
	if hpM <= 11000 {
		return 1013.25 * math.Pow(1-l*hpM/t0, ex)
	}
	return 226.3206 * math.Exp(-(hpM-11000)/6341.62)
}
