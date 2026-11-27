package radio

import "math"

// Physical constants and the Mode S / ADS-B downlink frequency.
const (
	// FreqMHz is the Mode S / ADS-B (1090ES) downlink carrier frequency.
	FreqMHz = 1090.0
	// SpeedOfLightMPerS is the speed of light in vacuum (exact, SI).
	SpeedOfLightMPerS = 299792458.0
	// WavelengthM is the free-space wavelength at 1090 MHz (≈ 0.275 m).
	WavelengthM = SpeedOfLightMPerS / (FreqMHz * 1e6)
	// BoltzmannJPerK is the Boltzmann constant (exact, SI 2019).
	BoltzmannJPerK = 1.380649e-23
	// T0K is the IEEE reference temperature for noise figures.
	T0K = 290.0
)

// DB converts a power ratio to decibels (10·log10). Non-positive ratios give -Inf.
func DB(ratio float64) float64 {
	if !(ratio > 0) {
		return math.Inf(-1)
	}
	return 10 * math.Log10(ratio)
}

// FromDB converts decibels to a power ratio (10^(dB/10)).
func FromDB(db float64) float64 { return math.Pow(10, db/10) }

// WattsToDBm converts a power in watts to dBm (1 W = 30 dBm).
func WattsToDBm(w float64) float64 { return DB(w) + 30 }

// DBmToWatts converts a power in dBm to watts.
func DBmToWatts(dbm float64) float64 { return FromDB(dbm - 30) }

// NoisePowerDBm returns the thermal noise power k·T·B in dBm for a noise
// temperature in kelvin and a bandwidth in Hz (−173.98 dBm/Hz at 290 K).
func NoisePowerDBm(tempK, bandwidthHz float64) float64 {
	return DB(BoltzmannJPerK*tempK*bandwidthHz) + 30
}

// NoiseTempFromNF converts a noise figure in dB to an equivalent input noise
// temperature in kelvin: Te = T0·(F − 1).
func NoiseTempFromNF(nfDB float64) float64 { return T0K * (FromDB(nfDB) - 1) }

// NFFromNoiseTemp converts an equivalent input noise temperature in kelvin to
// a noise figure in dB: F = 1 + Te/T0.
func NFFromNoiseTemp(teK float64) float64 { return DB(1 + teK/T0K) }

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
