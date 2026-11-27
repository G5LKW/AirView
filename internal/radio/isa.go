package radio

import "math"

// ICAO Standard Atmosphere (ICAO Doc 7488/3, identical to ISO 2533 and, up
// to 32 km, to the U.S. Standard Atmosphere 1976).
//
// The model is defined on geopotential altitude H, which is also what an
// aircraft's pressure altimeter reports: a Mode S barometric altitude is the
// ISA geopotential altitude of the measured static pressure. Within each
// layer the temperature is linear in H,
//
//	T = Tb + L·(H − Hb)
//	P = Pb·(T/Tb)^(−g0/(L·R))          L ≠ 0
//	P = Pb·exp(−g0·(H − Hb)/(R·Tb))    L = 0
//
// with g0 = 9.80665 m/s², R = 287.05287 J/(kg·K), sea level 288.15 K and
// 1013.25 hPa. The speed of sound is a = sqrt(γ·R·T), γ = 1.4.
const (
	ISASeaLevelTempK       = 288.15
	ISASeaLevelPressureHPa = 1013.25
	ISATropopauseM         = 11000.0 // geopotential metres
	ISATropopauseTempK     = 216.65
	ISALapseKPerM          = 0.0065    // troposphere temperature lapse rate
	GasConstantAir         = 287.05287 // specific gas constant of dry air, J/(kg·K)
	GammaAir               = 1.4       // ratio of specific heats
	G0                     = 9.80665   // standard gravity, m/s²

	isaMinM = -5000.0
	isaMaxM = 84852.0

	// isaEarthRadiusM is the nominal earth radius used by the standard for
	// the geometric ↔ geopotential conversion.
	isaEarthRadiusM = 6356766.0

	ktPerMS = 3600.0 / 1852.0
)

type isaLayer struct {
	baseM float64 // geopotential base height
	baseK float64 // temperature at the base
	lapse float64 // dT/dH, K/m
	baseP float64 // pressure at the base, hPa (filled in by init)
}

var isaLayers = [...]isaLayer{
	{baseM: 0, baseK: 288.15, lapse: -0.0065},
	{baseM: 11000, baseK: 216.65, lapse: 0},
	{baseM: 20000, baseK: 216.65, lapse: 0.001},
	{baseM: 32000, baseK: 228.65, lapse: 0.0028},
	{baseM: 47000, baseK: 270.65, lapse: 0},
	{baseM: 51000, baseK: 270.65, lapse: -0.0028},
	{baseM: 71000, baseK: 214.65, lapse: -0.002},
}

func init() {
	isaLayers[0].baseP = ISASeaLevelPressureHPa
	for i := 1; i < len(isaLayers); i++ {
		isaLayers[i].baseP = isaLayers[i-1].pressureAt(isaLayers[i].baseM)
	}
}

func (l *isaLayer) pressureAt(hM float64) float64 {
	if l.lapse == 0 {
		return l.baseP * math.Exp(-G0*(hM-l.baseM)/(GasConstantAir*l.baseK))
	}
	t := l.baseK + l.lapse*(hM-l.baseM)
	return l.baseP * math.Pow(t/l.baseK, -G0/(l.lapse*GasConstantAir))
}

func isaLayerFor(hM float64) *isaLayer {
	i := len(isaLayers) - 1
	for i > 0 && hM < isaLayers[i].baseM {
		i--
	}
	return &isaLayers[i]
}

// ISAState is the standard atmosphere at one altitude.
type ISAState struct {
	AltM           float64 `json:"altM"` // geopotential (pressure) altitude, metres
	TempK          float64 `json:"tempK"`
	TempC          float64 `json:"tempC"`
	PressureHPa    float64 `json:"pressureHpa"`
	DensityKgM3    float64 `json:"densityKgM3"`
	SpeedOfSoundMS float64 `json:"speedOfSoundMs"`
}

// ISA returns the ICAO standard atmosphere at a geopotential altitude in
// metres (for aircraft: the pressure altitude). The altitude is clamped to
// the range of the model, −5 km … 84.852 km.
func ISA(altM float64) ISAState {
	if altM != altM {
		altM = 0
	}
	h := clamp(altM, isaMinM, isaMaxM)
	l := isaLayerFor(h)
	t := l.baseK + l.lapse*(h-l.baseM)
	p := l.pressureAt(h)
	return ISAState{
		AltM:           h,
		TempK:          t,
		TempC:          t - 273.15,
		PressureHPa:    p,
		DensityKgM3:    p * 100 / (GasConstantAir * t),
		SpeedOfSoundMS: math.Sqrt(GammaAir * GasConstantAir * t),
	}
}

// ISAFt is ISA for an altitude in feet (the API unit).
func ISAFt(altFt float64) ISAState { return ISA(altFt * 0.3048) }

// ISATempC returns the standard temperature in °C at a pressure altitude in metres.
func ISATempC(altM float64) float64 { return ISA(altM).TempC }

// ISAPressureHPa returns the standard pressure in hPa at a pressure altitude in metres.
func ISAPressureHPa(altM float64) float64 { return ISA(altM).PressureHPa }

// PressureAltitudeM is the inverse of ISAPressureHPa: the geopotential
// altitude in metres at which the standard atmosphere has pressure pHPa.
// Pressures outside the model range are clamped; non-positive input gives
// the top of the model.
func PressureAltitudeM(pHPa float64) float64 {
	if !(pHPa > 0) {
		return isaMaxM
	}
	i := 0
	for i+1 < len(isaLayers) && pHPa <= isaLayers[i+1].baseP {
		i++
	}
	l := &isaLayers[i]
	var h float64
	if l.lapse == 0 {
		h = l.baseM - GasConstantAir*l.baseK/G0*math.Log(pHPa/l.baseP)
	} else {
		t := l.baseK * math.Pow(pHPa/l.baseP, -l.lapse*GasConstantAir/G0)
		h = l.baseM + (t-l.baseK)/l.lapse
	}
	return clamp(h, isaMinM, isaMaxM)
}

// GeopotentialM converts a geometric height above mean sea level to
// geopotential height: H = r·h/(r + h), r = 6 356 766 m. The difference is
// 0.16 % at 10 km.
func GeopotentialM(geometricM float64) float64 {
	return isaEarthRadiusM * geometricM / (isaEarthRadiusM + geometricM)
}

// GeometricM converts a geopotential height to geometric height: h = r·H/(r − H).
func GeometricM(geopotentialM float64) float64 {
	return isaEarthRadiusM * geopotentialM / (isaEarthRadiusM - geopotentialM)
}

// AirDensityKgM3 returns the density of dry air in kg/m³ from pressure (hPa)
// and temperature (°C): ρ = P/(R·T).
func AirDensityKgM3(pHPa, tC float64) float64 {
	return pHPa * 100 / (GasConstantAir * (tC + 273.15))
}

// SpeedOfSoundMS returns the speed of sound in m/s in air at static
// temperature tC (°C): a = sqrt(γ·R·T) (340.294 m/s at 15 °C).
func SpeedOfSoundMS(tC float64) float64 {
	t := tC + 273.15
	if t < 0 {
		t = 0
	}
	return math.Sqrt(GammaAir * GasConstantAir * t)
}

// SpeedOfSoundKt is SpeedOfSoundMS in knots (661.48 kt at 15 °C).
func SpeedOfSoundKt(tC float64) float64 { return SpeedOfSoundMS(tC) * ktPerMS }

// TASFromMachKt returns the true airspeed in knots for a Mach number at
// static air temperature tC (°C): TAS = M·a.
func TASFromMachKt(mach, tC float64) float64 { return mach * SpeedOfSoundKt(tC) }

// TempFromMachTAS returns the static air temperature in °C implied by a
// Mach number and a true airspeed in knots, T = (TAS/M)²/(γ·R). This is how
// outside air temperature is derived from Mode S EHS (BDS 5,0 TAS and
// BDS 6,0 Mach). ok is false when the inputs cannot give a sensible value
// (Mach or TAS not positive).
func TempFromMachTAS(mach, tasKt float64) (tC float64, ok bool) {
	if !(mach > 0) || !(tasKt > 0) {
		return 0, false
	}
	a := tasKt / ktPerMS / mach
	return a*a/(GammaAir*GasConstantAir) - 273.15, true
}

// ISADeviationC returns how much warmer (+) or colder (−) than the standard
// atmosphere an observed static air temperature is at a pressure altitude in
// feet ("ISA +10").
func ISADeviationC(pressureAltFt, oatC float64) float64 {
	return oatC - ISAFt(pressureAltFt).TempC
}
