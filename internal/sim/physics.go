package sim

import "math"

const (
	earthRadiusKm = 6371.0088
	deg2rad       = math.Pi / 180
	rad2deg       = 180 / math.Pi
	ktToMps       = 0.514444
	ftToM         = 0.3048
	// degPerM converts metres along a great circle to degrees of arc.
	degPerM = 180 / (math.Pi * earthRadiusKm * 1000)
	gravity = 9.80665
)

func wrap360(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}

func wrap180(d float64) float64 {
	d = math.Mod(d+180, 360)
	if d < 0 {
		d += 360
	}
	return d - 180
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// distBearing returns the great-circle distance (km) and initial true bearing
// (degrees) from point 1 to point 2.
func distBearing(lat1, lon1, lat2, lon2 float64) (km, brg float64) {
	p1, p2 := lat1*deg2rad, lat2*deg2rad
	dl := (lon2 - lon1) * deg2rad
	s1, c1 := math.Sincos(p1)
	s2, c2 := math.Sincos(p2)
	sdl, cdl := math.Sincos(dl)
	sdp := math.Sin((p2 - p1) / 2)
	sdl2 := math.Sin(dl / 2)
	h := sdp*sdp + c1*c2*sdl2*sdl2
	km = 2 * earthRadiusKm * math.Asin(math.Min(1, math.Sqrt(h)))
	brg = wrap360(math.Atan2(sdl*c2, c1*s2-s1*c2*cdl) * rad2deg)
	return km, brg
}

// destination returns the point reached from (lat, lon) after km along the
// great circle with initial bearing brg.
func destination(lat, lon, brg, km float64) (float64, float64) {
	p := lat * deg2rad
	d := km / earthRadiusKm
	sp, cp := math.Sincos(p)
	sd, cd := math.Sincos(d)
	sb, cb := math.Sincos(brg * deg2rad)
	s2 := clamp(sp*cd+cp*sd*cb, -1, 1)
	lat2 := math.Asin(s2)
	lon2 := lon*deg2rad + math.Atan2(sb*sd*cp, cd-sp*s2)
	return lat2 * rad2deg, wrap180(lon2 * rad2deg)
}

// --- International Standard Atmosphere -------------------------------------

const tropopauseFt = 36089.24

// isaTempK is the ISA temperature (K) at a pressure altitude in feet.
func isaTempK(altFt float64) float64 {
	if altFt < tropopauseFt {
		return 288.15 - 0.0019812*altFt
	}
	return 216.65
}

// isaPressHPa is the static pressure (hPa) at a pressure altitude in feet.
func isaPressHPa(altFt float64) float64 {
	if altFt < tropopauseFt {
		return 1013.25 * math.Pow(1-6.87535e-6*altFt, 5.2561)
	}
	return 226.32 * math.Exp(-(altFt-tropopauseFt)/20805.8)
}

// soundKt is the speed of sound (kt) at temperature tK.
func soundKt(tK float64) float64 { return 38.967854 * math.Sqrt(tK) }

const seaLevelSoundKt = 661.4786

// machFromCAS converts calibrated airspeed (kt) to Mach at static pressure p (hPa).
func machFromCAS(cas, p float64) float64 {
	x := cas / seaLevelSoundKt
	qc := 1013.25 * (math.Pow(1+0.2*x*x, 3.5) - 1)
	return math.Sqrt(5 * (math.Pow(qc/p+1, 2.0/7) - 1))
}

// casFromMach converts Mach to calibrated airspeed (kt) at static pressure p (hPa).
func casFromMach(m, p float64) float64 {
	qc := p * (math.Pow(1+0.2*m*m, 3.5) - 1)
	return seaLevelSoundKt * math.Sqrt(5*(math.Pow(qc/1013.25+1, 2.0/7)-1))
}

// --- slowly varying environment ---------------------------------------------

// env is the simulated environment, refreshed once per simulated second.
type env struct {
	kFactor   float64 // effective earth radius factor (4/3 = standard atmosphere)
	windDir0  float64 // surface wind direction (deg true, blowing from)
	windScale float64 // multiplier on the wind speed profile
	dT        float64 // ISA temperature deviation, deg C
	qnh       float64 // hPa
	density   float64 // diurnal traffic density 0..1
	localHour float64 // local solar time at the station
}

// trafficDensity is the diurnal traffic curve (0..1) for a local solar hour:
// a quiet night, a morning wave, a midday plateau and an evening wave.
func trafficDensity(h float64) float64 {
	g := func(c, w float64) float64 {
		d := math.Abs(h - c)
		if d > 12 {
			d = 24 - d
		}
		return math.Exp(-d * d / (2 * w * w))
	}
	v := (0.18 + 0.72*g(8.0, 2.3) + 0.55*g(13.0, 2.6) + 0.70*g(18.0, 2.4)) / 0.99
	return math.Min(v, 1)
}

func (s *Sim) updateEnv(t float64) {
	const tau = 2 * math.Pi
	vt := s.epochNs + s.worldNs
	sod := float64(vt%86400e9) / 1e9
	h := pmod(sod/3600+s.st.lon/15, 24)
	e := &s.env
	e.localHour = h

	// Propagation: a few incommensurate slow oscillations (hours), a diurnal
	// term peaking before dawn (radiation inversions) and rare ducting bumps.
	g := 0.10*math.Sin(tau*t/26280+s.ph[0]) +
		0.07*math.Sin(tau*t/11160+s.ph[1]) +
		0.05*math.Sin(tau*t/61200+s.ph[2]) +
		0.06*math.Cos(tau*(h-5)/24)
	duct := math.Max(0, math.Sin(tau*t/190800+s.ph[3])-0.82) * 1.6
	e.kFactor = 4.0/3.0*math.Exp(g) + duct

	e.windDir0 = 255 + 25*math.Sin(tau*t/40000+s.ph[4])
	e.windScale = 1 + 0.3*math.Sin(tau*t/29000+s.ph[5])
	e.dT = s.dT0 + 3*math.Sin(tau*t/170000+s.ph[6])
	e.qnh = 1013 + 12*math.Sin(tau*t/260000+s.ph[7])
	if s.cfg.NoDiurnal {
		e.density = 1
	} else {
		e.density = trafficDensity(h)
	}
}

// windAt returns the wind at a pressure altitude: direction it blows from
// (deg true) and speed (kt).
func (s *Sim) windAt(altFt float64) (dirFrom, kt float64) {
	x := clamp(altFt/34000, 0, 1.2)
	kt = s.env.windScale * (6 + 34*math.Pow(x, 1.3))
	dirFrom = wrap360(s.env.windDir0 + 25*x)
	return dirFrom, kt
}

// windVec returns the north/east components (kt) of the air mass motion.
func (s *Sim) windVec(altFt float64) (wN, wE float64) {
	dir, kt := s.windAt(altFt)
	sn, cs := math.Sincos(dir * deg2rad)
	return -kt * cs, -kt * sn
}

// gnssMinusBaro models the difference between GNSS height (HAE) and pressure
// altitude in feet: QNH offset, temperature error and geoid separation.
func (s *Sim) gnssMinusBaro(altFt float64) float64 {
	return (s.env.qnh-1013.25)*27.3 + altFt*s.env.dT/273 + s.geoidFt
}
