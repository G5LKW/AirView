package radio

import (
	"math"

	"airview/internal/geo"
	"airview/internal/model"
)

// Antenna pattern models.
//
// These are engineering models, not electromagnetic simulations: each
// produces a plausible far-field gain for every direction from the handful
// of parameters a station owner knows (type, gain, beamwidths, front-to-back
// ratio, pointing). They are meant for link-budget predictions and for
// drawing the "expected" pattern next to the one measured from traffic.
//
// Directivity ↔ beamwidth relations used when a parameter is missing:
//
//   - Directional antennas (Kraus): D = 41253 / (θh·θv) for a single lossless
//     main lobe, beamwidths in degrees (41253 = 4π·(180/π)² square degrees in
//     a sphere). Real antennas radiate part of their power outside the main
//     lobe and have some ohmic loss, so G = η·41253/(θh·θv) with η =
//     BeamEfficiency (0.7 → G ≈ 29000/(θh·θv), between Kraus's ideal value
//     and Stutzman's 26000 rule for practical antennas).
//   - Omnidirectional antennas (McDonald 1978, quoted in Balanis, "Antenna
//     Theory", §2.7.2): D = 101.5 / (θv − 0.00272·θv²). A half-wave dipole
//     (θv = 78°) gives 1.64 = 2.15 dBi.
//
// Main lobes use a cos^n power pattern, n = ln(1/2)/ln(cos(θ/2)), which has
// exactly the requested −3 dB beamwidth θ.

// BeamEfficiency is the fraction η used in G = η·41253/(θh·θv).
const BeamEfficiency = 0.7

const (
	squareDegreesPerSphere = 41253.0
	degToRad               = math.Pi / 180
	radToDeg               = 180 / math.Pi

	// First sidelobe of a uniformly excited collinear array: −13.3 dB, at
	// 1.61 full half-power beamwidths from the beam peak
	// (sinθ = 1.43·λ/L versus a beamwidth of 0.886·λ/L).
	uniformArraySidelobeDB   = 13.3
	firstSidelobeBeamwidths  = 1.61
	defaultOmniFloorDB       = 30.0 // deepest null of a real omni, dB below the peak
	halfWaveDipoleDBi        = 2.15
	halfWaveDipoleBeamwidth  = 78.0
	defaultCollinearDBi      = 6.0
	defaultYagiDBi           = 10.0
	defaultPatchDBi          = 8.0
	defaultYagiFrontToBackDB = 15.0
	defaultPatchFrontToBack  = 20.0
	defaultYagiSidelobeDB    = 13.0
	minBeamwidthDeg          = 2.0
	maxOmniBeamwidthDeg      = 120.0
	maxDirBeamwidthDeg       = 170.0
)

// Pattern is a far-field antenna gain model. azimuthDeg is degrees true
// (clockwise from north), elevationDeg is degrees above the horizon; the
// result is the gain in dBi towards that direction. Implementations are
// immutable and safe for concurrent use.
type Pattern interface {
	GainDBi(azimuthDeg, elevationDeg float64) float64
}

// Isotropic radiates equally in all directions with the given gain in dBi.
type Isotropic struct{ GainDB float64 }

// GainDBi implements Pattern.
func (i Isotropic) GainDBi(_, _ float64) float64 { return i.GainDB }

// cosExponent returns n such that cos^n has a −3 dB full width of bwDeg.
func cosExponent(bwDeg float64) float64 {
	bwDeg = clamp(bwDeg, 0.5, maxDirBeamwidthDeg)
	return math.Ln2 / -math.Log(math.Cos(bwDeg/2*degToRad))
}

// ---------------------------------------------------------------------------
// Omnidirectional verticals

// Omni models a vertical omnidirectional antenna: a quarter- or half-wave
// vertical (one broad lobe on the horizon) or a collinear (the same power
// squeezed into a narrow elevation lobe, with sidelobes and a null towards
// the zenith — the "cone of silence" above a high-gain vertical).
//
//	main lobe   G(el) = peak + 10·n·log10(cos(el − tilt))
//	sidelobes   envelope −SidelobeDB − 20·log10(sin|el − tilt| / sin(1.61·θv))
//	            + 20·log10(cos el)   (element pattern: null overhead)
//	floor       never below peak − FloorDB
//
// Build it with NewOmni (or FromAntenna); the zero value is not usable.
type Omni struct {
	PeakDBi       float64 // gain at the beam peak
	VBeamwidthDeg float64 // −3 dB elevation beamwidth
	TiltDeg       float64 // elevation of the beam peak (electrical up-tilt), usually 0
	SidelobeDB    float64 // first-sidelobe level below the peak, positive dB; 0 = no sidelobe model
	FloorDB       float64 // depth of the deepest null below the peak, positive dB

	n      float64 // cos exponent of the main lobe
	sinSL1 float64 // sin of the angle at which the sidelobe envelope starts to fall
}

// NewOmni builds an omnidirectional pattern. sidelobeDB <= 0 disables the
// sidelobe envelope (right for simple verticals); floorDB <= 0 selects 30 dB.
func NewOmni(peakDBi, vBeamwidthDeg, tiltDeg, sidelobeDB, floorDB float64) Omni {
	o := Omni{
		PeakDBi:       peakDBi,
		VBeamwidthDeg: clamp(vBeamwidthDeg, minBeamwidthDeg, maxOmniBeamwidthDeg),
		TiltDeg:       clamp(tiltDeg, -45, 45),
		SidelobeDB:    math.Max(0, sidelobeDB),
		FloorDB:       floorDB,
	}
	if !(o.FloorDB > 0) {
		o.FloorDB = defaultOmniFloorDB
	}
	o.n = cosExponent(o.VBeamwidthDeg)
	o.sinSL1 = math.Sin(math.Min(firstSidelobeBeamwidths*o.VBeamwidthDeg, 90) * degToRad)
	return o
}

// GainDBi implements Pattern. The azimuth is ignored.
func (o Omni) GainDBi(_, elevationDeg float64) float64 {
	if elevationDeg != elevationDeg {
		return math.NaN()
	}
	el := clamp(elevationDeg, -90, 90)
	d := math.Abs(el-o.TiltDeg) * degToRad
	rel := -o.FloorDB
	if d < math.Pi/2 {
		if m := 10 * o.n * math.Log10(math.Cos(d)); m > rel {
			rel = m
		}
	}
	if o.SidelobeDB > 0 {
		if ce := math.Cos(el * degToRad); ce > 1e-9 {
			env := -o.SidelobeDB + 20*math.Log10(ce)
			if s := math.Abs(math.Sin(d)); s > o.sinSL1 {
				env -= 20 * math.Log10(s/o.sinSL1)
			}
			if env > rel {
				rel = env
			}
		}
	}
	return o.PeakDBi + rel
}

// OmniBeamwidthForGain returns the −3 dB elevation beamwidth in degrees of a
// lossless omnidirectional antenna with the given gain, by inverting
// McDonald's D = 101.5/(θ − 0.00272·θ²). Gains at or below ~1 dBi give the
// 120° cap.
func OmniBeamwidthForGain(gainDBi float64) float64 {
	x := 4 * 0.00272 * 101.5 / FromDB(gainDBi)
	if x >= 1 {
		return maxOmniBeamwidthDeg
	}
	return clamp((1-math.Sqrt(1-x))/(2*0.00272), minBeamwidthDeg, maxOmniBeamwidthDeg)
}

// OmniGainForBeamwidth returns the gain in dBi of a lossless omnidirectional
// antenna with the given −3 dB elevation beamwidth (McDonald).
func OmniGainForBeamwidth(vBeamwidthDeg float64) float64 {
	t := clamp(vBeamwidthDeg, minBeamwidthDeg, maxOmniBeamwidthDeg)
	return DB(101.5 / (t - 0.00272*t*t))
}

// ---------------------------------------------------------------------------
// Directional antennas

// Directional models a yagi, patch/panel or other single-beam antenna:
//
//	main lobe   cos^n(ψ) of the off-boresight angle ψ, with n blended between
//	            the horizontal- and vertical-plane exponents according to the
//	            direction of the offset (an elliptical beam with exactly the
//	            requested −3 dB widths in both principal planes)
//	sidelobes   envelope falling smoothly from −SidelobeDB next to the main
//	            lobe to −FrontToBackDB directly behind:
//	            −[SLL + (F/B − SLL)·(1 − cos ψ)/2]
//	gain        peak + max(main lobe, envelope)
//
// Build it with NewDirectional (or FromAntenna); the zero value is not usable.
type Directional struct {
	PeakDBi       float64 // boresight gain
	AzimuthDeg    float64 // boresight azimuth, degrees true
	TiltDeg       float64 // boresight elevation above the horizon
	HBeamwidthDeg float64 // −3 dB width in the plane containing boresight and the horizontal cross axis
	VBeamwidthDeg float64 // −3 dB width in the vertical plane through boresight
	FrontToBackDB float64 // gain directly behind, dB below the peak (positive)
	SidelobeDB    float64 // sidelobe envelope next to the main lobe, dB below the peak (positive)

	nh, nv           float64
	sinTilt, cosTilt float64
}

// NewDirectional builds a directional pattern. Beamwidths are clamped to
// 2°…170°; frontToBackDB <= 0 selects 15 dB; sidelobeDB <= 0 or larger than
// the front-to-back ratio is set equal to it (a flat back-lobe floor).
func NewDirectional(peakDBi, azimuthDeg, tiltDeg, hBeamwidthDeg, vBeamwidthDeg, frontToBackDB, sidelobeDB float64) Directional {
	d := Directional{
		PeakDBi:       peakDBi,
		AzimuthDeg:    geo.NormalizeDeg(azimuthDeg),
		TiltDeg:       clamp(tiltDeg, -90, 90),
		HBeamwidthDeg: clamp(hBeamwidthDeg, minBeamwidthDeg, maxDirBeamwidthDeg),
		VBeamwidthDeg: clamp(vBeamwidthDeg, minBeamwidthDeg, maxDirBeamwidthDeg),
		FrontToBackDB: frontToBackDB,
		SidelobeDB:    sidelobeDB,
	}
	if !(d.FrontToBackDB > 0) {
		d.FrontToBackDB = defaultYagiFrontToBackDB
	}
	if !(d.SidelobeDB > 0) || d.SidelobeDB > d.FrontToBackDB {
		d.SidelobeDB = d.FrontToBackDB
	}
	d.nh = cosExponent(d.HBeamwidthDeg)
	d.nv = cosExponent(d.VBeamwidthDeg)
	d.sinTilt, d.cosTilt = math.Sincos(d.TiltDeg * degToRad)
	return d
}

// GainDBi implements Pattern.
func (d Directional) GainDBi(azimuthDeg, elevationDeg float64) float64 {
	sinEl, cosEl := math.Sincos(elevationDeg * degToRad)
	sinDA, cosDA := math.Sincos((azimuthDeg - d.AzimuthDeg) * degToRad)
	// Target direction in the antenna frame: x along boresight, y horizontal
	// to the right of it, z perpendicular to both (up when the tilt is 0).
	tx := sinEl*d.sinTilt + cosEl*d.cosTilt*cosDA // = cos ψ
	ty := cosEl * sinDA
	tz := sinEl*d.cosTilt - cosEl*d.sinTilt*cosDA
	if tx > 1 {
		tx = 1
	} else if tx < -1 {
		tx = -1
	}
	rel := -(d.SidelobeDB + (d.FrontToBackDB-d.SidelobeDB)*(1-tx)/2)
	if tx > 0 {
		n := d.nh
		if s := ty*ty + tz*tz; s > 1e-18 {
			n = (d.nh*ty*ty + d.nv*tz*tz) / s
		}
		if m := 10 * n * math.Log10(tx); m > rel {
			rel = m
		}
	}
	return d.PeakDBi + rel
}

// DirectionalGainForBeamwidths returns G = η·41253/(θh·θv) in dBi.
func DirectionalGainForBeamwidths(hBeamwidthDeg, vBeamwidthDeg float64) float64 {
	h := clamp(hBeamwidthDeg, minBeamwidthDeg, maxDirBeamwidthDeg)
	v := clamp(vBeamwidthDeg, minBeamwidthDeg, maxDirBeamwidthDeg)
	return DB(BeamEfficiency * squareDegreesPerSphere / (h * v))
}

// BeamwidthProductForGain returns θh·θv in square degrees for a directional
// antenna of the given gain: η·41253/G.
func BeamwidthProductForGain(gainDBi float64) float64 {
	return BeamEfficiency * squareDegreesPerSphere / FromDB(gainDBi)
}

// YagiGainForElements estimates the gain in dBi of a yagi from its element
// count with the rule of thumb G ≈ 10·log10(N) + 3 dBi (each doubling of the
// element count, and with it roughly of the boom length, adds 3 dB): 3
// elements ≈ 7.8, 5 ≈ 10, 9 ≈ 12.5, 15 ≈ 14.8 dBi. Real designs scatter
// around this by ±1.5 dB; enter the manufacturer's figure when known.
func YagiGainForElements(elements int) float64 {
	if elements < 2 {
		elements = 2
	} else if elements > 40 {
		elements = 40
	}
	return 10*math.Log10(float64(elements)) + 3
}

// yagiAspect is the assumed ratio of the H-plane to the E-plane beamwidth of
// a yagi. Short yagis are much wider in the H-plane (a 3-element beam is
// roughly 65° × 110°), long ones are nearly circular; 1 + 1.5/(N−1) is a
// simple interpolation of that behaviour (heuristic).
func yagiAspect(elements int) float64 {
	if elements < 2 {
		return 1.2
	}
	return 1 + 1.5/float64(elements-1)
}

// ---------------------------------------------------------------------------
// From configuration

// PatternInfo is the fully resolved parameter set of a pattern model: what
// the user configured plus everything that had to be estimated.
type PatternInfo struct {
	Kind          string  `json:"kind"`          // model.Ant* constant
	Directional   bool    `json:"directional"`   // single-beam antenna (yagi, patch); false = omni
	PeakGainDBi   float64 `json:"peakGainDbi"`   //
	AzimuthDeg    float64 `json:"azimuthDeg"`    // boresight azimuth (0 for an omni)
	TiltDeg       float64 `json:"tiltDeg"`       // boresight / beam-peak elevation
	HBeamwidthDeg float64 `json:"hBeamwidthDeg"` // 360 for an omni
	VBeamwidthDeg float64 `json:"vBeamwidthDeg"` //
	FrontToBackDB float64 `json:"frontToBackDb"` // 0 for an omni
	SidelobeDB    float64 `json:"sidelobeDb"`    // sidelobe envelope below the peak; 0 = none
	FloorDB       float64 `json:"floorDb"`       // omni: deepest null below the peak
	// Estimated lists the model.Antenna JSON field names whose values were
	// not configured and had to be derived ("gainDbi", "hBeamwidthDeg",
	// "vBeamwidthDeg", "frontToBackDb").
	Estimated []string `json:"estimated"`
}

// Resolve turns an antenna configuration into the complete parameter set of
// its pattern model, deriving whatever is missing (zero):
//
//   - vertical / collinear / other without a horizontal beamwidth → omni.
//     Missing elevation beamwidth ← gain (McDonald), missing gain ←
//     beamwidth; neither: a half-wave vertical (2.15 dBi, 78°) or a 6 dBi
//     collinear. Collinears and any vertical narrower than 45° get the
//     uniform-array sidelobe envelope and overhead null.
//   - yagi / patch / other with a horizontal beamwidth → directional.
//     Missing gain ← both beamwidths (η·41253/θhθv), else ← yagi element
//     count (YagiGainForElements), else 10 dBi (yagi) / 8 dBi (patch).
//     Missing beamwidths ← gain: the known one fixes the other, with neither
//     the product is split using the yagi aspect ratio (wider in the plane
//     perpendicular to the elements) or equally for a patch. Missing
//     front-to-back ratio: 15 dB (yagi) / 20 dB (patch).
//
// A gain of exactly 0 dBi counts as "not set".
func Resolve(a model.Antenna) PatternInfo {
	kind := a.Type
	switch kind {
	case model.AntVertical, model.AntCollinear, model.AntYagi, model.AntPatch:
	default:
		kind = model.AntOther
	}
	directional := kind == model.AntYagi || kind == model.AntPatch ||
		(kind == model.AntOther && a.HBeamwidthDeg > 0 && a.HBeamwidthDeg < 360)
	if directional {
		return resolveDirectional(kind, a)
	}
	return resolveOmni(kind, a)
}

func resolveOmni(kind string, a model.Antenna) PatternInfo {
	info := PatternInfo{Kind: kind, HBeamwidthDeg: 360, Estimated: []string{}}
	gain, vbw := a.GainDBi, a.VBeamwidthDeg
	haveGain := gain != 0 && gain == gain
	haveBW := vbw > 0 && vbw <= 180
	switch {
	case haveGain && haveBW:
	case haveGain:
		vbw = OmniBeamwidthForGain(gain)
		info.Estimated = append(info.Estimated, "vBeamwidthDeg")
	case haveBW:
		gain = OmniGainForBeamwidth(vbw)
		info.Estimated = append(info.Estimated, "gainDbi")
	default:
		if kind == model.AntCollinear {
			gain = defaultCollinearDBi
			vbw = OmniBeamwidthForGain(gain)
		} else {
			gain, vbw = halfWaveDipoleDBi, halfWaveDipoleBeamwidth
		}
		info.Estimated = append(info.Estimated, "gainDbi", "vBeamwidthDeg")
	}
	o := NewOmni(gain, vbw, a.TiltDeg, 0, 0)
	if kind == model.AntCollinear || o.VBeamwidthDeg < 45 {
		o = NewOmni(gain, vbw, a.TiltDeg, uniformArraySidelobeDB, 0)
	}
	info.PeakGainDBi, info.VBeamwidthDeg, info.TiltDeg = o.PeakDBi, o.VBeamwidthDeg, o.TiltDeg
	info.SidelobeDB, info.FloorDB = o.SidelobeDB, o.FloorDB
	return info
}

func resolveDirectional(kind string, a model.Antenna) PatternInfo {
	info := PatternInfo{Kind: kind, Directional: true, Estimated: []string{}}
	gain := a.GainDBi
	h, v := a.HBeamwidthDeg, a.VBeamwidthDeg
	haveH := h > 0 && h < 360
	haveV := v > 0 && v <= 180
	if gain == 0 || gain != gain {
		switch {
		case haveH && haveV:
			gain = DirectionalGainForBeamwidths(h, v)
		case kind == model.AntYagi && a.Elements >= 2:
			gain = YagiGainForElements(a.Elements)
		case kind == model.AntYagi:
			gain = defaultYagiDBi
		default:
			gain = defaultPatchDBi
		}
		info.Estimated = append(info.Estimated, "gainDbi")
	}
	if !haveH || !haveV {
		prod := BeamwidthProductForGain(gain)
		switch {
		case haveH:
			v = prod / h
			info.Estimated = append(info.Estimated, "vBeamwidthDeg")
		case haveV:
			h = prod / v
			info.Estimated = append(info.Estimated, "hBeamwidthDeg")
		default:
			r := 1.0
			if kind == model.AntYagi {
				r = yagiAspect(a.Elements)
			}
			narrow := math.Sqrt(prod / r)
			wide := r * narrow
			// The beam is narrower in the plane of the elements (E-plane):
			// the vertical plane for the vertically polarised antennas used
			// for ADS-B, the horizontal plane for a horizontally mounted one.
			if a.Polarization == "horizontal" {
				h, v = narrow, wide
			} else {
				h, v = wide, narrow
			}
			info.Estimated = append(info.Estimated, "hBeamwidthDeg", "vBeamwidthDeg")
		}
	}
	ftb := a.FrontToBackDB
	if !(ftb > 0) {
		if kind == model.AntPatch {
			ftb = defaultPatchFrontToBack
		} else {
			ftb = defaultYagiFrontToBackDB
		}
		info.Estimated = append(info.Estimated, "frontToBackDb")
	}
	sll := ftb // patch: no distinct sidelobes, a flat back-lobe floor
	if kind != model.AntPatch {
		sll = math.Min(defaultYagiSidelobeDB, ftb)
	}
	d := NewDirectional(gain, a.AzimuthDeg, a.TiltDeg, h, v, ftb, sll)
	info.PeakGainDBi, info.AzimuthDeg, info.TiltDeg = d.PeakDBi, d.AzimuthDeg, d.TiltDeg
	info.HBeamwidthDeg, info.VBeamwidthDeg = d.HBeamwidthDeg, d.VBeamwidthDeg
	info.FrontToBackDB, info.SidelobeDB = d.FrontToBackDB, d.SidelobeDB
	return info
}

// Pattern builds the pattern model described by the info.
func (i PatternInfo) Pattern() Pattern {
	if i.Directional {
		return NewDirectional(i.PeakGainDBi, i.AzimuthDeg, i.TiltDeg, i.HBeamwidthDeg, i.VBeamwidthDeg, i.FrontToBackDB, i.SidelobeDB)
	}
	return NewOmni(i.PeakGainDBi, i.VBeamwidthDeg, i.TiltDeg, i.SidelobeDB, i.FloorDB)
}

// FromAntenna returns the pattern model for a configured antenna (an Omni or
// a Directional value, see Resolve for how missing parameters are derived).
// Cable, filter and polarisation losses are not part of the pattern; the
// link budget (NewLink) accounts for them.
func FromAntenna(a model.Antenna) Pattern { return Resolve(a).Pattern() }

// HalfWaveVertical is a half-wave vertical dipole: 2.15 dBi, 78° elevation
// beamwidth (the exact dipole pattern cos(π/2·sin el)/cos el is matched
// within 1 dB down to −8 dB).
func HalfWaveVertical() Omni {
	return NewOmni(halfWaveDipoleDBi, halfWaveDipoleBeamwidth, 0, 0, 0)
}

// QuarterWaveVertical is a quarter-wave ground-plane vertical with radials
// (the common ADS-B "spider"). With drooping radials it behaves like a
// slightly asymmetric dipole; it is modelled as 2.0 dBi with an 80° lobe on
// the horizon. (Only over an infinite ground plane would it reach 5.15 dBi.)
func QuarterWaveVertical() Omni { return NewOmni(2.0, 80, 0, 0, 0) }

// Collinear is a collinear vertical of the given gain in dBi (<= 0 selects
// 6 dBi) with the matching compressed elevation lobe, uniform-array
// sidelobes and an overhead null.
func Collinear(gainDBi float64) Omni {
	if !(gainDBi > 0) {
		gainDBi = defaultCollinearDBi
	}
	return NewOmni(gainDBi, OmniBeamwidthForGain(gainDBi), 0, uniformArraySidelobeDB, 0)
}

// ---------------------------------------------------------------------------
// Cuts for plotting

// Cut is a one-dimensional slice through a pattern, ready for plotting.
type Cut struct {
	AngleDeg []float64 `json:"angleDeg"`
	GainDBi  []float64 `json:"gainDbi"`
	// Wrap is true for an azimuth cut (the first sample follows the last).
	Wrap bool `json:"wrap"`
}

const maxCutPoints = 7201

// AzimuthCut samples the pattern around the compass at a fixed elevation:
// azimuths 0, step, 2·step, … < 360 (stepDeg <= 0 selects 1°).
func AzimuthCut(p Pattern, elevationDeg, stepDeg float64) Cut {
	n := cutCount(360, stepDeg)
	c := Cut{AngleDeg: make([]float64, n), GainDBi: make([]float64, n), Wrap: true}
	for i := 0; i < n; i++ {
		az := 360 * float64(i) / float64(n)
		c.AngleDeg[i] = az
		c.GainDBi[i] = p.GainDBi(az, elevationDeg)
	}
	return c
}

// ElevationCut samples the pattern in the vertical plane at a fixed azimuth,
// from −90° to +90° elevation inclusive (stepDeg <= 0 selects 1°).
func ElevationCut(p Pattern, azimuthDeg, stepDeg float64) Cut {
	return ElevationCutRange(p, azimuthDeg, -90, 90, stepDeg)
}

// ElevationCutRange is ElevationCut limited to [minElDeg, maxElDeg], e.g.
// −2…+90 to match the elevation histogram.
func ElevationCutRange(p Pattern, azimuthDeg, minElDeg, maxElDeg, stepDeg float64) Cut {
	minElDeg = clamp(minElDeg, -90, 90)
	maxElDeg = clamp(maxElDeg, minElDeg, 90)
	n := cutCount(maxElDeg-minElDeg, stepDeg) + 1
	c := Cut{AngleDeg: make([]float64, n), GainDBi: make([]float64, n)}
	for i := 0; i < n; i++ {
		el := maxElDeg
		if n > 1 {
			el = minElDeg + (maxElDeg-minElDeg)*float64(i)/float64(n-1)
		}
		c.AngleDeg[i] = el
		c.GainDBi[i] = p.GainDBi(azimuthDeg, el)
	}
	return c
}

func cutCount(spanDeg, stepDeg float64) int {
	if !(stepDeg > 0) {
		stepDeg = 1
	}
	// Clamp before converting: int(NaN) and int(+Inf) are platform dependent.
	f := math.Round(spanDeg / stepDeg)
	if !(f >= 1) {
		return 1
	}
	if f > maxCutPoints-1 {
		return maxCutPoints - 1
	}
	return int(f)
}

// Peak returns the angle and gain of the strongest sample (NaN, -Inf for an
// empty cut).
func (c Cut) Peak() (angleDeg, gainDBi float64) {
	angleDeg, gainDBi = math.NaN(), math.Inf(-1)
	for i, g := range c.GainDBi {
		if g > gainDBi {
			angleDeg, gainDBi = c.AngleDeg[i], g
		}
	}
	return angleDeg, gainDBi
}

// Beamwidth returns the full width in degrees of the lobe around the peak
// between the points where the gain has dropped downDB (3 for the usual
// half-power beamwidth) below it, linearly interpolated between samples.
// ok is false when the gain never drops that far on one of the sides (an
// omni's azimuth cut, or a lobe cut off by the end of an elevation cut).
func (c Cut) Beamwidth(downDB float64) (widthDeg float64, ok bool) {
	n := len(c.GainDBi)
	if n < 3 || !(downDB > 0) {
		return 0, false
	}
	pk := 0
	for i, g := range c.GainDBi {
		if g > c.GainDBi[pk] {
			pk = i
		}
	}
	level := c.GainDBi[pk] - downDB
	step := math.Abs(c.AngleDeg[1] - c.AngleDeg[0])
	side := func(dir int) (float64, bool) {
		prev := pk
		for k := 1; k < n; k++ {
			i := pk + dir*k
			if c.Wrap {
				i = ((i % n) + n) % n
			} else if i < 0 || i >= n {
				return 0, false
			}
			if g := c.GainDBi[i]; g < level {
				gp := c.GainDBi[prev]
				return (float64(k-1) + (gp-level)/(gp-g)) * step, true
			}
			prev = i
		}
		return 0, false
	}
	lo, ok1 := side(-1)
	hi, ok2 := side(+1)
	if !ok1 || !ok2 {
		return 0, false
	}
	return lo + hi, true
}
