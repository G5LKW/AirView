package met

import (
	"math"
	"time"

	"airview/internal/radio"
)

// Input is one aircraft state sample, as the engine assembles it from the
// tracker: position, pressure altitude, the ground vector (ADS-B or
// BDS 5,0), the air vector (BDS 5,0 true airspeed, BDS 6,0 magnetic
// heading / IAS / Mach) and, when the aircraft answers it, the direct
// meteorological report of BDS 4,4. Speeds are knots, angles degrees,
// altitude feet. NaN means unknown.
type Input struct {
	T    time.Time
	ICAO uint32

	Lat, Lon, AltFt, GS, Track, TAS, IAS, Mach, MagHeading, Roll float64 // NaN = unknown

	// Direct BDS 4,4 (MRAR) values, NaN if none. MRARTemp is the value
	// decoded with the 0.25 degC scale (modes.MeteoRoutine.Temp); the
	// 0.125 degC alternative is exactly half of it and is tried too.
	MRARWindSpd, MRARWindDir, MRARTemp float64
}

// Derived is the meteorological result for one Input. WindDir is the
// direction the wind blows FROM in degrees true, WindSpd in knots, OAT the
// static air temperature in degC. A value is meaningful only when its Has
// flag is set (it is 0, never NaN, otherwise). Quality is 0..1 (0 = nothing
// usable).
type Derived struct {
	WindDir, WindSpd, OAT float64
	HasWind, HasOAT       bool
	Quality               float64
}

// Source says where a wind or temperature value came from.
type Source uint8

const (
	SourceNone Source = iota
	SourceEHS         // computed from Mode S EHS (BDS 5,0 / 6,0) + ground vector
	SourceMRAR        // direct BDS 4,4 meteorological routine air report
)

// String returns "", "ehs" or "mrar".
func (s Source) String() string {
	switch s {
	case SourceEHS:
		return "ehs"
	case SourceMRAR:
		return "mrar"
	}
	return ""
}

// Reject is a bit set of the reasons why (part of) an observation was not
// used.
type Reject uint16

const (
	RejectTurning      Reject = 1 << iota // |roll| above the limit: heading is not the air vector direction
	RejectLowSpeed                        // too slow for a meaningful wind (taxi, approach, helicopters)
	RejectNoPosition                      // no position: magnetic declination unknown
	RejectNoHeading                       // no magnetic heading
	RejectNoAirspeed                      // no true airspeed and none derivable
	RejectNoGround                        // no ground speed / track
	RejectInconsistent                    // TAS, IAS and Mach contradict each other
	RejectDrift                           // drift angle beyond anything an airliner flies
	RejectWindRange                       // wind speed absurd
	RejectTempRange                       // temperature too far from the standard atmosphere
	RejectLowMach                         // Mach too low for a temperature
	RejectAltitude                        // altitude missing or out of range
	RejectMRAR                            // a BDS 4,4 value was present but implausible
)

var rejectNames = [...]string{"turning", "lowSpeed", "noPosition", "noHeading", "noAirspeed", "noGround",
	"inconsistent", "drift", "windRange", "tempRange", "lowMach", "altitude", "mrar"}

// Strings lists the set reasons.
func (r Reject) Strings() []string {
	var out []string
	for i, n := range rejectNames {
		if r&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return out
}

// Quality gates, after de Haan (KNMI, "High-resolution wind and temperature
// observations from aircraft tracked by Mode-S air traffic control radar",
// JGR 2011) and the EMADDC processing.
const (
	MaxRollDeg       = 5.0   // wind is rejected above this bank angle
	MinTASKt         = 120.0 // wind needs at least this true airspeed ...
	MinGSKt          = 50.0  // ... and this ground speed
	MinMachForTemp   = 0.2   // Mach/TAS temperature is too noisy below
	MaxWindKt        = 250.0
	MaxISADeviationC = 40.0
	MaxDriftDeg      = 40.0
	MinAltFt         = -1500.0
	MaxAltFt         = 60000.0

	// maxMachDisagree is the largest tolerated difference between the
	// reported Mach number and the one implied by IAS and pressure
	// altitude (CAS/IAS position error is far smaller).
	maxMachDisagree = 0.04
	// mrarWindTolKt / mrarTempTolC: a BDS 4,4 value is implausible when it
	// differs by more than this from a good value computed from EHS (the
	// register is inferred, not labelled, so misidentified replies occur).
	mrarWindTolKt = 40.0
	mrarTempTolC  = 10.0

	isaA0Kt = 661.4786 // sea-level speed of sound, kt
	isaP0   = radio.ISASeaLevelPressureHPa
)

// Detail is the full result of a derivation: the Derived values plus
// everything a caller needs to judge or reuse them. Fields that could not
// be computed are NaN.
type Detail struct {
	Derived

	U, V        float64 // wind components, kt: U eastward, V northward (air motion)
	TAS         float64 // true airspeed used, kt
	TASDerived  bool    // TAS came from Mach / IAS, not from BDS 5,0
	Mach        float64 // Mach number used (reported, or from IAS + altitude)
	MagVar      float64 // declination applied, degrees east
	TrueHeading float64 // heading used for the air vector, degrees true

	WindSource, TempSource Source
	// WindQuality and TempQuality are the 0..1 confidences of the two
	// values separately (Quality combines them).
	WindQuality, TempQuality float64
	// EHSTemp is the temperature computed from Mach and TAS, also when a
	// BDS 4,4 value took precedence (NaN if not computable).
	EHSTemp float64
	// EHSU, EHSV is the wind computed from EHS, also when BDS 4,4 took
	// precedence (NaN if not computable).
	EHSU, EHSV float64
	Reject     Reject
}

// deriveOpts are the hooks the Collector uses; the zero value plus NaNs
// gives the pure behaviour of Derive.
type deriveOpts struct {
	magVar      float64 // declination to use; NaN = evaluate the WMM
	headingCorr float64 // degrees added to the true heading (per-aircraft bias)
	tempHintC   float64 // best temperature estimate at the aircraft; NaN = ISA
}

// Derive computes wind and temperature from one aircraft state sample. It
// is a pure function.
//
// Wind: the ground vector (GS along the true track) minus the air vector
// (TAS along the true heading), with true heading = magnetic heading +
// WMM2025 declination at the aircraft. Temperature: from Mach and TAS,
// T = T0 * (TAS / (Mach * a0))^2. When Mach is missing it is computed from
// IAS (treated as CAS) and the pressure altitude with the compressible-flow
// (Saint-Venant) relations; when TAS is missing it is computed from Mach
// and the standard-atmosphere temperature, which costs accuracy (about 1 kt
// per degC of ISA deviation at cruise) and gives no temperature.
//
// Direct BDS 4,4 values take precedence when they are plausible.
func Derive(in Input) Derived {
	return derive(&in, deriveOpts{magVar: math.NaN(), tempHintC: math.NaN()}).Derived
}

// DeriveDetail is Derive with the complete working.
func DeriveDetail(in Input) Detail {
	return derive(&in, deriveOpts{magVar: math.NaN(), tempHintC: math.NaN()})
}

// MachFromCAS returns the Mach number of a calibrated airspeed (kt) at a
// static pressure (hPa): impact pressure from the sea-level standard
// definition of CAS, then the isentropic pitot relation (subsonic).
func MachFromCAS(casKt, pHPa float64) float64 {
	if !(casKt > 0) || !(pHPa > 0) {
		return math.NaN()
	}
	x := casKt / isaA0Kt
	qc := isaP0 * (math.Pow(1+0.2*x*x, 3.5) - 1)
	return math.Sqrt(5 * (math.Pow(qc/pHPa+1, 2.0/7.0) - 1))
}

// CASFromMach is the inverse of MachFromCAS.
func CASFromMach(mach, pHPa float64) float64 {
	if !(mach > 0) || !(pHPa > 0) {
		return math.NaN()
	}
	qc := pHPa * (math.Pow(1+0.2*mach*mach, 3.5) - 1)
	return isaA0Kt * math.Sqrt(5*(math.Pow(qc/isaP0+1, 2.0/7.0)-1))
}

func derive(in *Input, o deriveOpts) Detail {
	nan := math.NaN()
	d := Detail{U: nan, V: nan, TAS: nan, Mach: nan, MagVar: nan, TrueHeading: nan, EHSTemp: nan, EHSU: nan, EHSV: nan}

	// Altitude and the standard atmosphere there.
	haveAlt := finite(in.AltFt) && in.AltFt >= MinAltFt && in.AltFt <= MaxAltFt
	var isaT, press float64
	if haveAlt {
		isa := radio.ISAFt(in.AltFt)
		isaT, press = isa.TempC, isa.PressureHPa
	} else {
		d.Reject |= RejectAltitude
	}

	// ---- airspeed -------------------------------------------------------
	mach := nan
	machReported := finite(in.Mach) && in.Mach > 0.05 && in.Mach < 1.2
	machFromIAS := nan
	if haveAlt && finite(in.IAS) && in.IAS >= 40 && in.IAS < 700 {
		machFromIAS = MachFromCAS(in.IAS, press)
	}
	inconsistent := false
	switch {
	case machReported:
		mach = in.Mach
		if finite(machFromIAS) && math.Abs(machFromIAS-mach) > maxMachDisagree {
			inconsistent = true
		}
	case finite(machFromIAS):
		mach = machFromIAS
	}
	d.Mach = mach

	tasReported := finite(in.TAS) && in.TAS > 0 && in.TAS < 800
	tas := nan
	tempQ := 0.0
	ehsTemp := nan
	if tasReported {
		tas = in.TAS
		if finite(mach) {
			// Static temperature from Mach and TAS.
			if mach < MinMachForTemp {
				d.Reject |= RejectLowMach
			} else if t, ok := radio.TempFromMachTAS(mach, tas); ok {
				switch {
				case inconsistent:
					// handled below
				case haveAlt && math.Abs(t-isaT) > MaxISADeviationC, !haveAlt && (t < -95 || t > 60):
					// Mach and TAS do not describe the same air: one of
					// them is stale or mis-decoded.
					inconsistent = true
					d.Reject |= RejectTempRange
				default:
					ehsTemp = t
					// Quantisation: TAS 2 kt, Mach 0.004 (uniform errors).
					sigma := 2 * (t + 273.15) * math.Hypot(0.577/tas, 0.00115/mach)
					tempQ = clamp01(1.2 / (0.6 + sigma))
					if !machReported {
						tempQ *= 0.6 // IAS is coarser (1 kt) and not quite CAS
					}
				}
			}
		}
	} else if finite(mach) && haveAlt && !inconsistent {
		// No TAS: Mach times the speed of sound at the best temperature
		// estimate available.
		t := o.tempHintC
		if !finite(t) || math.Abs(t-isaT) > MaxISADeviationC {
			t = isaT
		}
		tas = radio.TASFromMachKt(mach, t)
		d.TASDerived = true
	}
	if inconsistent {
		d.Reject |= RejectInconsistent
		ehsTemp, tempQ = nan, 0
	}
	d.TAS = tas
	d.EHSTemp = ehsTemp

	// ---- wind from the vector triangle ------------------------------------
	windQ := 0.0
	ehsU, ehsV := nan, nan
	haveGround := finite(in.GS) && finite(in.Track) && in.GS >= 0 && in.GS < 1200
	switch {
	case !haveGround:
		d.Reject |= RejectNoGround
	case !finite(tas):
		d.Reject |= RejectNoAirspeed
	case !finite(in.MagHeading):
		d.Reject |= RejectNoHeading
	case inconsistent:
		// already flagged
	case tas < MinTASKt || in.GS < MinGSKt:
		d.Reject |= RejectLowSpeed
	case finite(in.Roll) && math.Abs(in.Roll) > MaxRollDeg:
		d.Reject |= RejectTurning
	default:
		mv := o.magVar
		if !finite(mv) {
			altKm := 0.0
			if haveAlt {
				altKm = in.AltFt * 0.0003048
			}
			mv = MagVar(in.Lat, in.Lon, altKm, in.T)
		}
		if !finite(mv) {
			d.Reject |= RejectNoPosition
			break
		}
		d.MagVar = mv
		hdg := norm360(in.MagHeading + mv + o.headingCorr)
		d.TrueHeading = hdg
		if math.Abs(angDiff(hdg, in.Track)) > MaxDriftDeg {
			d.Reject |= RejectDrift
			break
		}
		st, ct := math.Sincos(in.Track * math.Pi / 180)
		sh, ch := math.Sincos(hdg * math.Pi / 180)
		u := in.GS*st - tas*sh
		v := in.GS*ct - tas*ch
		if math.Hypot(u, v) > MaxWindKt {
			d.Reject |= RejectWindRange
			break
		}
		ehsU, ehsV = u, v
		windQ = 1
		if d.TASDerived {
			windQ *= 0.5
		}
		if !finite(in.Roll) {
			windQ *= 0.7
		} else {
			windQ *= 1 - 0.08*math.Abs(in.Roll) // 0.6 at the 5 degree limit
		}
		// A heading error of 0.5 degrees is a cross wind error of
		// TAS * 0.0087: slow aircraft give the better winds, but below
		// about 200 kt they are usually manoeuvring.
		if tas < 200 {
			windQ *= 0.8
		}
	}
	d.EHSU, d.EHSV = ehsU, ehsV

	// ---- direct BDS 4,4 -----------------------------------------------------
	if finite(ehsU) {
		d.U, d.V, d.WindSource = ehsU, ehsV, SourceEHS
	}
	if finite(in.MRARWindSpd) || finite(in.MRARWindDir) {
		ok := finite(in.MRARWindSpd) && finite(in.MRARWindDir) &&
			in.MRARWindSpd >= 0 && in.MRARWindSpd <= MaxWindKt && in.MRARWindDir >= 0 && in.MRARWindDir <= 360
		if ok {
			s, c := math.Sincos(in.MRARWindDir * math.Pi / 180)
			mu, mvv := -in.MRARWindSpd*s, -in.MRARWindSpd*c
			if finite(ehsU) && windQ >= 0.5 && math.Hypot(mu-ehsU, mvv-ehsV) > mrarWindTolKt {
				ok = false
			} else {
				d.U, d.V, d.WindSource = mu, mvv, SourceMRAR
				windQ = 1
			}
		}
		if !ok {
			d.Reject |= RejectMRAR
		}
	}

	if finite(ehsTemp) {
		d.OAT, d.HasOAT, d.TempSource = ehsTemp, true, SourceEHS
	}
	if finite(in.MRARTemp) {
		// Scale ambiguity: 0.25 degC/LSB as transmitted by real
		// transponders, or 0.125 degC/LSB as Doc 9871 can be read. Take
		// the reading closest to the best independent estimate.
		ref, tol, haveRef := 0.0, MaxISADeviationC, false
		switch {
		case finite(ehsTemp) && tempQ >= 0.3:
			ref, tol, haveRef = ehsTemp, mrarTempTolC, true
		case haveAlt && finite(o.tempHintC):
			ref, haveRef = o.tempHintC, true
		case haveAlt:
			ref, haveRef = isaT, true
		}
		t := in.MRARTemp
		if haveRef && math.Abs(in.MRARTemp/2-ref) < math.Abs(t-ref) {
			t = in.MRARTemp / 2
		}
		ok := haveRef && math.Abs(t-ref) <= tol && t >= -95 && t <= 60
		if ok && haveAlt && math.Abs(t-isaT) > MaxISADeviationC {
			ok = false
		}
		if ok {
			d.OAT, d.HasOAT, d.TempSource = t, true, SourceMRAR
			tempQ = 1
		} else {
			d.Reject |= RejectMRAR
		}
	}

	if d.WindSource != SourceNone {
		d.HasWind = true
		d.WindSpd = math.Hypot(d.U, d.V)
		if d.WindSpd > 0 {
			d.WindDir = norm360(math.Atan2(-d.U, -d.V) * 180 / math.Pi)
		}
	}
	if !d.HasWind {
		windQ = 0
	}
	if !d.HasOAT {
		tempQ = 0
	}
	d.WindQuality, d.TempQuality = clamp01(windQ), clamp01(tempQ)
	switch {
	case d.HasWind && d.HasOAT:
		d.Quality = (windQ + tempQ) / 2
	case d.HasWind:
		d.Quality = windQ
	case d.HasOAT:
		d.Quality = tempQ
	}
	d.Quality = clamp01(d.Quality)
	return d
}

func clamp01(v float64) float64 {
	if !(v > 0) {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
