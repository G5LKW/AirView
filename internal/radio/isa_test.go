package radio

import (
	"math"
	"testing"
)

func TestISA(t *testing.T) {
	// Published ICAO / ISO 2533 / US Standard Atmosphere 1976 table values at
	// geopotential altitudes (layer bases are exact by definition).
	cases := []struct {
		altM, tempK, pHPa, pTol float64
	}{
		{0, 288.15, 1013.25, 1e-9},
		{5000, 255.65, 540.20, 0.02},
		{11000, 216.65, 226.32, 0.01},
		{20000, 216.65, 54.748, 0.002},
		{32000, 228.65, 8.6801, 0.0005},
		{47000, 270.65, 1.1090, 0.0002},
		{51000, 270.65, 0.66938, 0.0002},
		{71000, 214.65, 0.039564, 0.00002},
	}
	for _, c := range cases {
		s := ISA(c.altM)
		near(t, "ISA temp", s.TempK, c.tempK, 1e-9)
		near(t, "ISA pressure", s.PressureHPa, c.pHPa, c.pTol)
		near(t, "ISA tempC", s.TempC, c.tempK-273.15, 1e-9)
		near(t, "pressure altitude round trip", PressureAltitudeM(s.PressureHPa), c.altM, 1e-6*math.Max(1, c.altM))
	}
	s := ISA(0)
	near(t, "sea-level density", s.DensityKgM3, 1.2250, 0.0001)
	near(t, "sea-level speed of sound", s.SpeedOfSoundMS, 340.294, 0.001)
	s = ISA(11000)
	near(t, "tropopause density", s.DensityKgM3, 0.36392, 0.0001)
	near(t, "tropopause speed of sound", s.SpeedOfSoundMS, 295.07, 0.01)

	// FL350: −54.3 °C and 238.4 hPa; the 500 hPa surface is at 18 289 ft.
	s = ISAFt(35000)
	near(t, "FL350 temp", s.TempC, -54.34, 0.01)
	near(t, "FL350 pressure", s.PressureHPa, 238.42, 0.03)
	near(t, "FL350 altitude", s.AltM, 10668, 1e-9)
	near(t, "500 hPa", PressureAltitudeM(500)/0.3048, 18289, 2)
	near(t, "ISATempC", ISATempC(2000), 2.0, 1e-9)
	near(t, "ISAPressureHPa", ISAPressureHPa(2000), 794.95, 0.02)

	// Continuity across every layer boundary and monotonic pressure.
	prev := math.Inf(1)
	for h := -5000.0; h <= 84852; h += 250 {
		p := ISA(h).PressureHPa
		if !(p < prev) || p <= 0 {
			t.Fatalf("pressure not strictly decreasing at %v m: %v after %v", h, p, prev)
		}
		prev = p
	}
	for _, l := range isaLayers[1:] {
		below, above := ISA(l.baseM-1e-6), ISA(l.baseM+1e-6)
		near(t, "T continuity", below.TempK, above.TempK, 1e-6)
		near(t, "P continuity", below.PressureHPa, above.PressureHPa, 1e-6)
	}

	// Clamping and bad input.
	if ISA(-9000).AltM != -5000 || ISA(200000).AltM != 84852 || ISA(math.NaN()).AltM != 0 {
		t.Error("ISA altitude clamping")
	}
	near(t, "top temperature", ISA(84852).TempK, 186.946, 0.001)
	if PressureAltitudeM(0) != 84852 || PressureAltitudeM(-3) != 84852 || PressureAltitudeM(5000) != -5000 {
		t.Error("PressureAltitudeM clamping")
	}
	for _, h := range []float64{-1000, 300, 8000, 15000, 25000, 40000, 49000, 60000, 80000} {
		near(t, "round trip", PressureAltitudeM(ISAPressureHPa(h)), h, 1e-6)
	}
}

func TestAirHelpers(t *testing.T) {
	near(t, "a(15 °C)", SpeedOfSoundMS(15), 340.294, 0.001)
	near(t, "a(15 °C) kt", SpeedOfSoundKt(15), 661.48, 0.01)
	near(t, "a(−56.5 °C)", SpeedOfSoundMS(-56.5), 295.07, 0.01)
	near(t, "a below absolute zero", SpeedOfSoundMS(-300), 0, 0)
	near(t, "density", AirDensityKgM3(1013.25, 15), 1.2250, 0.0001)

	// Mach 0.78 in the stratosphere (−56.5 °C): 0.78 · 573.57 kt = 447.4 kt.
	tas := TASFromMachKt(0.78, -56.5)
	near(t, "TAS", tas, 447.4, 0.1)
	oat, ok := TempFromMachTAS(0.78, tas)
	if !ok {
		t.Fatal("TempFromMachTAS not ok")
	}
	near(t, "OAT from Mach/TAS", oat, -56.5, 1e-9)
	if _, ok := TempFromMachTAS(0, 400); ok {
		t.Error("Mach 0 must not give a temperature")
	}
	if _, ok := TempFromMachTAS(0.5, -1); ok {
		t.Error("negative TAS must not give a temperature")
	}
	if _, ok := TempFromMachTAS(math.NaN(), 400); ok {
		t.Error("NaN Mach must not give a temperature")
	}

	// "ISA +10" at FL350.
	near(t, "ISA deviation", ISADeviationC(35000, -44.34), 10, 0.01)

	// Geopotential vs geometric height: 10 km geometric is 9984.3 gpm.
	near(t, "geopotential", GeopotentialM(10000), 9984.29, 0.01)
	near(t, "geometric round trip", GeometricM(GeopotentialM(12345)), 12345, 1e-6)
}
