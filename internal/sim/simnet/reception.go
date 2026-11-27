package simnet

import (
	"fmt"
	"math"
	"math/rand/v2"

	"airview/internal/model"
)

// ReceptionSpec configures which transmissions a receiver hears, how strong
// they are and which propagation impairments their timing suffers. Fields
// are used as given; DefaultReception fills realistic values per device and
// normalize fills the structural defaults documented per field.
type ReceptionSpec struct {
	// Antenna and RF chain.
	AntennaGainDBi float64
	// Pattern is the elevation pattern: model.AntVertical (dipole, cos²) or
	// model.AntCollinear (compressed vertical beam, cos⁶). "" = vertical.
	Pattern     string
	CableLossDB float64
	// HorizonHeightM is the antenna height above the surrounding terrain used
	// for the radio horizon (0 = AntennaAGL + 10 m).
	HorizonHeightM float64

	// SensitivityDBm is the power at the receiver input at which half of
	// the long frames are decoded (0 = -88 dBm). Short frames need 1 dB
	// less.
	SensitivityDBm float64
	// DetectWidthDB is the width of the logistic detection curve (0 = 1.5).
	DetectWidthDB float64
	// FullScaleDBm is the input power that reads 0 dBFS (0 = sensitivity +
	// 48 dB). Levels are clamped to -48.1..0 dBFS (Beast byte 1..255).
	FullScaleDBm float64
	// FadingDB is the per-message log-normal fading (1σ, dB).
	FadingDB float64
	// LossProb is an independent loss probability per frame (garbling,
	// decoder overload).
	LossProb float64

	// MinElevDeg masks targets below this elevation (0 = no mask). MaskDeg
	// is an optional per-azimuth mask: len(MaskDeg) equal sectors clockwise
	// from true north; the larger of the two applies.
	MinElevDeg float64
	MaskDeg    []float64
	// MaxRangeKm is a hard range limit (0 = none).
	MaxRangeKm float64

	// Timing impairments. Multipath adds an exponentially distributed
	// positive delay (mean MultipathMeanNs) with probability MultipathProb,
	// four times as often below 3 degrees elevation. Outliers add a uniform
	// error of ±OutlierMaxNs with probability OutlierProb.
	MultipathProb   float64
	MultipathMeanNs float64
	OutlierProb     float64
	OutlierMaxNs    float64
}

// DefaultReception returns a realistic randomised reception setup for a
// device.
func DefaultReception(d Device, r *rand.Rand) ReceptionSpec {
	s := ReceptionSpec{
		AntennaGainDBi:  uniform(r, 2, 6),
		Pattern:         model.AntVertical,
		CableLossDB:     uniform(r, 0.5, 3),
		DetectWidthDB:   1.5,
		FadingDB:        2.5,
		LossProb:        0.03,
		MultipathProb:   0.01,
		MultipathMeanNs: 100,
		OutlierProb:     1e-4,
		OutlierMaxNs:    5000,
	}
	if r.IntN(3) == 0 {
		s.Pattern = model.AntCollinear
	}
	switch d {
	case DeviceRadarcapeGPS, DeviceRadarcape12:
		s.SensitivityDBm = uniform(r, -94, -90)
	case DeviceBeast:
		s.SensitivityDBm = uniform(r, -92, -88)
	case DeviceSDR:
		s.SensitivityDBm = uniform(r, -88, -82)
		s.LossProb = 0.06
		s.MultipathProb = 0.02
	case DeviceAirspy:
		s.SensitivityDBm = uniform(r, -92, -88)
	case DeviceSBS:
		s.SensitivityDBm = uniform(r, -86, -82)
		s.LossProb = 0.05
	}
	return s
}

func (s *ReceptionSpec) normalize(aglM float64) error {
	if s.Pattern == "" {
		s.Pattern = model.AntVertical
	}
	if s.Pattern != model.AntVertical && s.Pattern != model.AntCollinear {
		return fmt.Errorf("unsupported antenna pattern %q", s.Pattern)
	}
	if !(s.HorizonHeightM > 0) {
		s.HorizonHeightM = aglM + 10
	}
	if s.SensitivityDBm == 0 {
		s.SensitivityDBm = -88
	}
	if !(s.DetectWidthDB > 0) {
		s.DetectWidthDB = 1.5
	}
	if s.FullScaleDBm == 0 {
		s.FullScaleDBm = s.SensitivityDBm + 48
	}
	for _, p := range []float64{s.LossProb, s.MultipathProb, s.OutlierProb} {
		if p < 0 || p > 1 || math.IsNaN(p) {
			return fmt.Errorf("reception probability %v outside 0..1", p)
		}
	}
	return nil
}

// fsplConst is 20·log10(4π·1090 MHz / c): free-space path loss at 1 m.
var fsplConst = 20 * math.Log10(4*math.Pi*1090e6/SpeedOfLight)

// antennaGain returns the gain towards elevation el (radians).
func (s *ReceptionSpec) antennaGain(sinEl float64) float64 {
	c2 := 1 - sinEl*sinEl // cos² el
	if c2 < 1e-6 {
		c2 = 1e-6
	}
	g := 10 * math.Log10(c2)
	if s.Pattern == model.AntCollinear {
		g *= 3
	}
	if g < -25 {
		g = -25
	}
	return s.AntennaGainDBi + g
}

// maskAt returns the elevation mask (degrees) towards an azimuth.
func (s *ReceptionSpec) maskAt(azDeg float64) float64 {
	m := s.MinElevDeg
	if n := len(s.MaskDeg); n > 0 {
		i := int(math.Floor(azDeg/360*float64(n))) % n
		if i < 0 {
			i += n
		}
		m = math.Max(m, s.MaskDeg[i])
	}
	return m
}

func (s *ReceptionSpec) hasMask() bool { return s.MinElevDeg != 0 || len(s.MaskDeg) > 0 }

// signalByte converts dBFS to the Beast amplitude byte (dBFS =
// 20·log10(b/255)), 1..255.
func signalByte(dbfs float64) byte {
	v := math.Round(255 * math.Pow(10, dbfs/20))
	return byte(clamp(v, 1, 255))
}
