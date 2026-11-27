// Package radio models 1090 MHz propagation for AirView: radio horizon and
// line-of-sight range, free-space and gaseous path loss, a complete link
// budget with antenna pattern models, and the atmospheric physics that
// modulates all of it (radio refractivity, k-factor, ducting, the ICAO
// standard atmosphere).
//
// Units: distances km, heights/altitudes metres unless a name says Ft,
// angles degrees, pressure hPa, temperature °C (K where named so), power
// dBm, gains dBi, frequencies MHz. Everything is a pure function or an
// immutable value and is safe for concurrent use.
package radio

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Water vapour (Recommendation ITU-R P.453-13/-14, section 1, eq. 8-10)

// SaturationVapourPressure returns the saturation vapour pressure over
// liquid water in hPa at temperature tC (°C):
//
//	es = EF · a · exp[ (b − t/d)·t / (t + c) ]
//	a = 6.1121, b = 18.678, c = 257.14, d = 234.5       (water, −40…+50 °C)
//	EF = 1 + 1e-4·[7.2 + P·(0.0320 + 5.9e-6·t²)]
//
// The enhancement factor EF (moist air instead of pure vapour, ~0.4 %) is
// applied when pressureHPa > 0; pass 0 to omit it. Relative humidity is by
// meteorological convention defined over water even below freezing, so this
// is the function to pair with RH.
func SaturationVapourPressure(tC, pressureHPa float64) float64 {
	ef := 1.0
	if pressureHPa > 0 {
		ef = 1 + 1e-4*(7.2+pressureHPa*(0.0320+5.9e-6*tC*tC))
	}
	return ef * 6.1121 * math.Exp((18.678-tC/234.5)*tC/(tC+257.14))
}

// SaturationVapourPressureIce is the saturation vapour pressure over ice in
// hPa (ITU-R P.453: a = 6.1115, b = 23.036, c = 279.82, d = 333.7,
// EF = 1 + 1e-4·[2.2 + P·(0.0383 + 6.4e-6·t²)], valid −80…0 °C).
func SaturationVapourPressureIce(tC, pressureHPa float64) float64 {
	ef := 1.0
	if pressureHPa > 0 {
		ef = 1 + 1e-4*(2.2+pressureHPa*(0.0383+6.4e-6*tC*tC))
	}
	return ef * 6.1115 * math.Exp((23.036-tC/333.7)*tC/(tC+279.82))
}

// VapourPressureFromRH returns the water vapour partial pressure e in hPa
// for temperature tC (°C), relative humidity rhPct (%) and total pressure
// pressureHPa (0 = ignore the enhancement factor): e = RH·es/100.
func VapourPressureFromRH(tC, rhPct, pressureHPa float64) float64 {
	if rhPct < 0 {
		rhPct = 0
	} else if rhPct > 100 {
		rhPct = 100
	}
	return rhPct / 100 * SaturationVapourPressure(tC, pressureHPa)
}

// VapourPressureFromDewPoint returns e in hPa from the dew point (°C): the
// saturation vapour pressure at the dew-point temperature.
func VapourPressureFromDewPoint(dewPointC, pressureHPa float64) float64 {
	return SaturationVapourPressure(dewPointC, pressureHPa)
}

// RHFromDewPoint returns the relative humidity in % for an air temperature
// and dew point in °C.
func RHFromDewPoint(tC, dewPointC float64) float64 {
	rh := 100 * SaturationVapourPressure(dewPointC, 0) / SaturationVapourPressure(tC, 0)
	if rh > 100 {
		rh = 100
	}
	return rh
}

// DewPointFromRH returns the dew point in °C for an air temperature (°C)
// and relative humidity (%), by inverting SaturationVapourPressure.
func DewPointFromRH(tC, rhPct float64) float64 {
	if rhPct >= 100 {
		return tC
	}
	if rhPct < 0.1 {
		rhPct = 0.1
	}
	e := rhPct / 100 * SaturationVapourPressure(tC, 0)
	// Newton iterations on ln(es(td)) = ln(e); start from the Magnus inverse.
	l := math.Log(e / 6.1121)
	td := 257.14 * l / (18.678 - l)
	for i := 0; i < 4; i++ {
		f := math.Log(SaturationVapourPressure(td, 0) / e)
		d := (math.Log(SaturationVapourPressure(td+0.01, 0)/e) - f) / 0.01
		if d == 0 {
			break
		}
		td -= f / d
	}
	return td
}

// WaterVapourDensity returns the absolute humidity in g/m³ for vapour
// pressure e (hPa) at temperature tC: ρ = 216.7·e/T (ITU-R P.453 eq. 10).
func WaterVapourDensity(eHPa, tC float64) float64 {
	return 216.7 * eHPa / (tC + 273.15)
}

// ---------------------------------------------------------------------------
// Refractivity (ITU-R P.453 section 1)

// Refractivity returns the radio refractivity N = (n−1)·1e6 in N-units for
// total pressure pHPa, temperature tC (°C) and water vapour pressure eHPa:
//
//	N = 77.6·P/T + 3.73e5·e/T²  =  (77.6/T)·(P + 4810·e/T)      (T in kelvin)
//
// the classic two-term form (Smith & Weintraub 1953; dry term 77.6·P/T and
// wet term 3.732e5·e/T² of ITU-R P.453-7), good to 0.5 % up to 100 GHz. The
// current P.453-13/-14 writes N = 77.6·Pd/T + 72·e/T + 3.75e5·e/T² with the
// dry pressure Pd = P − e; the two agree to better than 0.05 N-units.
// Sea-level reference atmosphere: 1013.25 hPa, 15 °C, 60 % RH → N ≈ 319;
// ITU-R's global reference value is N0 = 315.
func Refractivity(pHPa, tC, eHPa float64) float64 {
	T := tC + 273.15
	return 77.6 / T * (pHPa + 4810*eHPa/T)
}

// RefractivityTerms returns the dry (hydrostatic) and wet parts of N.
func RefractivityTerms(pHPa, tC, eHPa float64) (dry, wet float64) {
	T := tC + 273.15
	return 77.6 * pHPa / T, 77.6 * 4810 * eHPa / (T * T)
}

// RefractivityRH is Refractivity with humidity given as RH in %.
func RefractivityRH(pHPa, tC, rhPct float64) float64 {
	return Refractivity(pHPa, tC, VapourPressureFromRH(tC, rhPct, pHPa))
}

// MPerKm is the gradient that converts N to modified refractivity: the
// earth's curvature expressed in N-units per km (1e6/6371 km ≈ 157).
const MPerKm = 157.0

// ModifiedRefractivity returns M = N + 157·h(km): refractivity with the
// earth's curvature folded in, so that dM/dh < 0 marks a trapping layer
// (ITU-R P.453 section 3: M = N + 157·h).
func ModifiedRefractivity(n, heightM float64) float64 {
	return n + MPerKm*heightM/1000
}

// GradientNPerKm returns the vertical refractivity gradient dN/dh in
// N-units per km between two levels (heights in metres). The standard
// atmosphere has about −39 … −40 N/km in the first kilometre.
func GradientNPerKm(n1, h1M, n2, h2M float64) float64 {
	dh := h2M - h1M
	if dh == 0 {
		return 0
	}
	return (n2 - n1) / dh * 1000
}

// Effective-earth-radius factor limits.
const (
	// KStandard is k for the standard gradient of about −39 N/km.
	KStandard = 4.0 / 3.0
	// KMax is returned for gradients at or beyond −157 N/km, where rays bend
	// at least as much as the earth curves (trapping). It is finite so that
	// values stay JSON-encodable; treat k >= KMax as "flat earth / ducting".
	KMax = 1000.0
)

// KFactor returns the effective-earth-radius factor for a refractivity
// gradient in N/km: k = 157 / (157 + dN/dh) (ITU-R P.453 / P.834).
// −39 N/km gives 4/3, 0 gives 1, −157 and below (trapping) give KMax.
func KFactor(dNdhPerKm float64) float64 {
	den := MPerKm + dNdhPerKm
	if den <= MPerKm/KMax {
		return KMax
	}
	return MPerKm / den
}

// GradientForK is the inverse of KFactor: the gradient in N/km that produces
// a given k (k <= 0 or >= KMax gives −157).
func GradientForK(k float64) float64 {
	if !(k > 0) || k >= KMax {
		return -MPerKm
	}
	return MPerKm/k - MPerKm
}

// Refraction classes, by refractivity gradient (N-units/km).
const (
	ClassSubRefraction   = "sub-refraction"   // dN/dh > 0: rays bend upwards, range shrinks
	ClassStandard        = "standard"         // 0 … −79: normal conditions
	ClassSuperRefraction = "super-refraction" // −79 … −157: rays bend strongly down, range grows
	ClassDucting         = "ducting"          // <= −157: rays are trapped
)

// ClassifyGradient names the refraction regime for a gradient in N/km using
// the conventional thresholds 0, −79 and −157 N/km.
func ClassifyGradient(dNdhPerKm float64) string {
	switch {
	case dNdhPerKm > 0:
		return ClassSubRefraction
	case dNdhPerKm > -79:
		return ClassStandard
	case dNdhPerKm > -157:
		return ClassSuperRefraction
	default:
		return ClassDucting
	}
}

// ---------------------------------------------------------------------------
// Vertical profiles and ducts

// Level is one level of a vertical sounding or numerical-model profile
// (e.g. Open-Meteo pressure-level data: geopotential height, temperature and
// relative humidity on 1000, 975, 950 … hPa surfaces, plus the surface).
type Level struct {
	HeightM     float64 `json:"heightM"`     // height above mean sea level
	PressureHPa float64 `json:"pressureHpa"` // total pressure
	TempC       float64 `json:"tempC"`       // air temperature
	RH          float64 `json:"rh"`          // relative humidity, %
}

// N returns the level's radio refractivity.
func (l Level) N() float64 { return RefractivityRH(l.PressureHPa, l.TempC, l.RH) }

// ProfilePoint is one analysed level.
type ProfilePoint struct {
	HeightM float64 `json:"heightM"`
	N       float64 `json:"n"` // refractivity
	M       float64 `json:"m"` // modified refractivity
}

// Layer is the slab between two consecutive levels.
type Layer struct {
	BaseM float64 `json:"baseM"`
	TopM  float64 `json:"topM"`
	DNDH  float64 `json:"dNdh"` // N-units per km
	DMDH  float64 `json:"dMdh"` // M-units per km (= dN/dh + 157); negative = trapping layer
	Class string  `json:"class"`
}

// Duct kinds.
const (
	DuctSurface      = "surface"       // trapping layer starts at the surface
	DuctSurfaceBased = "surface-based" // elevated trapping layer whose duct reaches the surface
	DuctElevated     = "elevated"      // duct base above the surface
)

// Duct is a tropospheric duct found in a profile.
type Duct struct {
	Kind       string  `json:"kind"`       // Duct* constant
	BaseM      float64 `json:"baseM"`      // bottom of the duct, m AMSL
	TopM       float64 `json:"topM"`       // top of the duct = top of the trapping layer
	TrapBaseM  float64 `json:"trapBaseM"`  // bottom of the trapping layer (dM/dh < 0)
	ThicknessM float64 `json:"thicknessM"` // TopM − BaseM
	StrengthM  float64 `json:"strengthM"`  // ΔM: M deficit across the trapping layer, M-units
	// MaxWavelengthM is the longest wavelength the duct can trap and
	// CutoffMHz the corresponding lowest frequency.
	MaxWavelengthM float64 `json:"maxWavelengthM"`
	CutoffMHz      float64 `json:"cutoffMHz"`
	Traps1090      bool    `json:"traps1090"` // 1090 MHz is above the cut-off
}

// DuctCutoff estimates the longest trapped wavelength (m) and lowest trapped
// frequency (MHz) of a duct of the given thickness (m) and strength ΔM
// (M-units) from the waveguide-mode approximation
//
//	λmax = (2/3)·C·D·sqrt(ΔM),  C = 3.77e-3 (surface ducts), 5.66e-3 (elevated ducts)
//
// (Turton, Bennetts & Farmer, "An introduction to radio ducting",
// Meteorological Magazine 117, 1988). The constants are 8·√2/3·1e-3 and
// 4·√2·1e-3 from the phase integral of the lowest trapped mode; for a
// surface duct this is the classical λmax ≈ 2.5e-3·D·sqrt(ΔM). A 100 m
// surface duct with ΔM = 10 traps down to ~380 MHz; 1090 MHz needs about
// 35 m at that strength. Trapping is not a sharp cut-off in practice; treat
// the result as an order-of-magnitude guide.
func DuctCutoff(thicknessM, strengthM float64, elevated bool) (maxWavelengthM, cutoffMHz float64) {
	if !(thicknessM > 0) || !(strengthM > 0) {
		return 0, math.Inf(1)
	}
	c := 3.77e-3
	if elevated {
		c = 5.66e-3
	}
	maxWavelengthM = 2.0 / 3.0 * c * thicknessM * math.Sqrt(strengthM)
	return maxWavelengthM, SpeedOfLightMPerS / 1e6 / maxWavelengthM
}

// Profile is the refractivity analysis of a vertical profile.
type Profile struct {
	Points []ProfilePoint `json:"points"`
	Layers []Layer        `json:"layers"`
	Ducts  []Duct         `json:"ducts"`

	SurfaceN float64 `json:"surfaceN"` // refractivity at the lowest level
	// GradientNPerKm is dN/dh over the lowest kilometre (interpolated to
	// surface + 1000 m; over the whole profile if it is shallower). This is
	// the ΔN that conventionally defines the k-factor.
	GradientNPerKm float64 `json:"gradientNPerKm"`
	// MinGradientNPerKm is the most negative layer gradient found below
	// 3 km above the surface, MinGradientHeightM the base of that layer.
	MinGradientNPerKm  float64 `json:"minGradientNPerKm"`
	MinGradientHeightM float64 `json:"minGradientHeightM"`
	K                  float64 `json:"k"`     // KFactor(GradientNPerKm)
	Class              string  `json:"class"` // ClassifyGradient(GradientNPerKm), or ducting when a duct was found
}

// AnalyzeProfile computes refractivity, layer gradients and ducts for a
// vertical profile. Levels may be given in any order; levels with
// non-physical values and duplicate heights are dropped. At least two valid
// levels are required, otherwise ok is false.
//
// Model pressure levels are 200–800 m apart, which resolves deep elevated
// ducts and strong surface-based ducts but not shallow evaporation ducts;
// the absence of a duct in such data is not proof that none exists.
func AnalyzeProfile(levels []Level) (p Profile, ok bool) {
	lv := make([]Level, 0, len(levels))
	for _, l := range levels {
		if l.PressureHPa > 1 && l.PressureHPa < 1200 && l.TempC > -120 && l.TempC < 70 &&
			l.RH >= 0 && l.RH <= 110 && l.HeightM > -500 && l.HeightM < 40000 {
			lv = append(lv, l)
		}
	}
	sort.Slice(lv, func(i, j int) bool { return lv[i].HeightM < lv[j].HeightM })
	pts := make([]ProfilePoint, 0, len(lv))
	for _, l := range lv {
		if n := len(pts); n > 0 && l.HeightM-pts[n-1].HeightM < 1 {
			continue // duplicate height
		}
		nn := l.N()
		pts = append(pts, ProfilePoint{HeightM: l.HeightM, N: nn, M: ModifiedRefractivity(nn, l.HeightM)})
	}
	if len(pts) < 2 {
		return Profile{}, false
	}
	p = analyzePoints(pts)
	return p, true
}

// AnalyzePoints is AnalyzeProfile for a profile that is already expressed as
// refractivity versus height (sorted by height, at least two points).
func AnalyzePoints(points []ProfilePoint) (Profile, bool) {
	if len(points) < 2 {
		return Profile{}, false
	}
	pts := make([]ProfilePoint, len(points))
	copy(pts, points)
	sort.Slice(pts, func(i, j int) bool { return pts[i].HeightM < pts[j].HeightM })
	for i := range pts {
		pts[i].M = ModifiedRefractivity(pts[i].N, pts[i].HeightM)
	}
	return analyzePoints(pts), true
}

func analyzePoints(pts []ProfilePoint) Profile {
	p := Profile{Points: pts, SurfaceN: pts[0].N, Ducts: []Duct{}}
	h0 := pts[0].HeightM

	p.Layers = make([]Layer, 0, len(pts)-1)
	p.MinGradientNPerKm = math.Inf(1)
	for i := 0; i+1 < len(pts); i++ {
		g := GradientNPerKm(pts[i].N, pts[i].HeightM, pts[i+1].N, pts[i+1].HeightM)
		p.Layers = append(p.Layers, Layer{
			BaseM: pts[i].HeightM, TopM: pts[i+1].HeightM,
			DNDH: g, DMDH: g + MPerKm, Class: ClassifyGradient(g),
		})
		if pts[i].HeightM-h0 < 3000 && g < p.MinGradientNPerKm {
			p.MinGradientNPerKm, p.MinGradientHeightM = g, pts[i].HeightM
		}
	}

	// dN/dh over the lowest kilometre.
	top := pts[len(pts)-1]
	if top.HeightM-h0 > 1000 {
		p.GradientNPerKm = interpN(pts, h0+1000) - pts[0].N
	} else {
		p.GradientNPerKm = GradientNPerKm(pts[0].N, h0, top.N, top.HeightM)
	}
	p.K = KFactor(p.GradientNPerKm)
	p.Class = ClassifyGradient(p.GradientNPerKm)

	// Ducts: every run of layers with dM/dh < 0 is a trapping layer. The
	// duct extends from the top of the trapping layer (local M minimum) down
	// to the height where M has the same value again, or to the surface.
	for i := 0; i+1 < len(pts); {
		if pts[i+1].M >= pts[i].M {
			i++
			continue
		}
		s := i // trapping-layer base (local M maximum)
		e := i + 1
		for e+1 < len(pts) && pts[e+1].M < pts[e].M {
			e++
		}
		mTop := pts[e].M
		d := Duct{TopM: pts[e].HeightM, TrapBaseM: pts[s].HeightM, StrengthM: pts[s].M - mTop}
		base := h0
		kind := DuctSurfaceBased
		if s == 0 {
			kind = DuctSurface
		}
		for j := s - 1; j >= 0; j-- {
			if pts[j].M <= mTop {
				// M(top) is crossed between j and j+1.
				f := (mTop - pts[j].M) / (pts[j+1].M - pts[j].M)
				base = pts[j].HeightM + f*(pts[j+1].HeightM-pts[j].HeightM)
				kind = DuctElevated
				break
			}
		}
		d.Kind, d.BaseM = kind, base
		d.ThicknessM = d.TopM - d.BaseM
		d.MaxWavelengthM, d.CutoffMHz = DuctCutoff(d.ThicknessM, d.StrengthM, kind == DuctElevated)
		d.Traps1090 = d.CutoffMHz <= FreqMHz
		p.Ducts = append(p.Ducts, d)
		i = e
	}
	if len(p.Ducts) > 0 {
		p.Class = ClassDucting
	}
	return p
}

// interpN linearly interpolates N at height hM (clamped to the profile).
func interpN(pts []ProfilePoint, hM float64) float64 {
	if hM <= pts[0].HeightM {
		return pts[0].N
	}
	for i := 0; i+1 < len(pts); i++ {
		if hM <= pts[i+1].HeightM {
			f := (hM - pts[i].HeightM) / (pts[i+1].HeightM - pts[i].HeightM)
			return pts[i].N + f*(pts[i+1].N-pts[i].N)
		}
	}
	return pts[len(pts)-1].N
}

// ---------------------------------------------------------------------------
// Propagation index

// Assessment condenses the state of the lower atmosphere into one number
// and one sentence for the UI and for correlation with measured range.
type Assessment struct {
	// Index is 0–100: 50 = standard atmosphere, below = reduced range
	// (sub-refraction), above = enhanced range, 85+ = ducting likely.
	Index          int     `json:"index"`
	Class          string  `json:"class"` // Class* constant
	GradientNPerKm float64 `json:"gradientNPerKm"`
	K              float64 `json:"k"`
	// HorizonFactor is the radio-horizon distance relative to the standard
	// 4/3-earth value, sqrt(k / (4/3)), capped at 3.
	HorizonFactor float64 `json:"horizonFactor"`
	Ducts         []Duct  `json:"ducts"`
	Ducting1090   bool    `json:"ducting1090"` // a duct able to trap 1090 MHz is present
	Summary       string  `json:"summary"`
}

// index anchor points: gradient (N/km) → score.
var indexAnchors = [...][2]float64{
	{120, 0}, {80, 8}, {0, 30}, {-40, 50}, {-79, 65}, {-157, 85}, {-400, 92},
}

func gradientScore(g float64) float64 {
	a := indexAnchors[:]
	if g >= a[0][0] {
		return a[0][1]
	}
	for i := 0; i+1 < len(a); i++ {
		if g >= a[i+1][0] {
			f := (a[i][0] - g) / (a[i][0] - a[i+1][0])
			return a[i][1] + f*(a[i+1][1]-a[i][1])
		}
	}
	return a[len(a)-1][1]
}

// HorizonFactor returns how far the radio horizon reaches relative to the
// standard atmosphere for effective-earth factor k: sqrt(k/(4/3)), capped
// at 3 (beyond that the path is duct- and power-limited, not horizon-limited).
func HorizonFactor(k float64) float64 {
	if !(k > 0) {
		return 3
	}
	f := math.Sqrt(k / KStandard)
	if f > 3 {
		f = 3
	}
	return f
}

// AssessGradient rates propagation from a single low-level refractivity
// gradient in N/km (for example between the surface and the 925 hPa level).
func AssessGradient(dNdhPerKm float64) Assessment {
	return assess(dNdhPerKm, nil)
}

// Assess rates propagation for an analysed profile: the gradient of the
// lowest kilometre sets the base score and ducts add to it, most of all
// when they can trap 1090 MHz.
func Assess(p Profile) Assessment {
	return assess(p.GradientNPerKm, p.Ducts)
}

func assess(g float64, ducts []Duct) Assessment {
	a := Assessment{GradientNPerKm: g, K: KFactor(g), Class: ClassifyGradient(g), Ducts: ducts}
	if a.Ducts == nil {
		a.Ducts = []Duct{}
	}
	a.HorizonFactor = HorizonFactor(a.K)
	score := gradientScore(g)

	var best *Duct
	bonus := 0.0
	for i := range ducts {
		d := &ducts[i]
		var b float64
		if d.Traps1090 {
			a.Ducting1090 = true
			b = math.Min(15, 6+d.StrengthM/2)
		} else {
			b = math.Min(5, 1+d.StrengthM/4)
		}
		if b > bonus {
			bonus, best = b, d
		}
	}
	score += bonus
	if a.Ducting1090 && score < 80 {
		score = 80
	}
	if len(ducts) > 0 {
		a.Class = ClassDucting
	}
	a.Index = int(math.Round(math.Max(0, math.Min(100, score))))

	var sb strings.Builder
	pct := (a.HorizonFactor - 1) * 100
	switch ClassifyGradient(g) {
	case ClassSubRefraction:
		fmt.Fprintf(&sb, "Sub-refraction (dN/dh %+.0f N/km, k = %.2f): radio waves bend away from the ground, radio horizon about %.0f%% shorter than standard.", g, a.K, -pct)
	case ClassStandard:
		switch {
		case pct > 3:
			fmt.Fprintf(&sb, "Near-standard refraction (dN/dh %.0f N/km, k = %.2f): radio horizon about %.0f%% beyond standard.", g, a.K, pct)
		case pct < -3:
			fmt.Fprintf(&sb, "Weak refraction (dN/dh %.0f N/km, k = %.2f): radio horizon about %.0f%% shorter than standard.", g, a.K, -pct)
		default:
			fmt.Fprintf(&sb, "Standard refraction (dN/dh %.0f N/km, k = %.2f): normal radio horizon.", g, a.K)
		}
	case ClassSuperRefraction:
		fmt.Fprintf(&sb, "Super-refraction (dN/dh %.0f N/km, k = %.2f): radio horizon extended about %.0f%% beyond standard.", g, a.K, pct)
	default:
		fmt.Fprintf(&sb, "Trapping gradient (dN/dh %.0f N/km): rays follow the earth's curvature, reception far beyond the normal horizon is possible.", g)
	}
	if best != nil {
		kind := best.Kind
		fmt.Fprintf(&sb, " %s duct %.0f–%.0f m (ΔM %.0f, cut-off ~%s)", strings.ToUpper(kind[:1])+kind[1:], best.BaseM, best.TopM, best.StrengthM, fmtMHz(best.CutoffMHz))
		if best.Traps1090 {
			sb.WriteString(" can trap 1090 MHz: over-the-horizon reception likely for aircraft and paths inside it.")
		} else {
			sb.WriteString(" is too thin/weak to trap 1090 MHz but still bends rays strongly.")
		}
		if n := len(ducts); n > 1 {
			fmt.Fprintf(&sb, " (%d ducts in the profile.)", n)
		}
	}
	a.Summary = sb.String()
	return a
}

func fmtMHz(f float64) string {
	switch {
	case math.IsInf(f, 0) || f > 1e6:
		return "none"
	case f >= 10000:
		return fmt.Sprintf("%.0f GHz", f/1000)
	case f >= 1000:
		return fmt.Sprintf("%.1f GHz", f/1000)
	default:
		return fmt.Sprintf("%.0f MHz", f)
	}
}
