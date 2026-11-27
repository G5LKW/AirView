package met

import (
	"math"
	"sort"
	"time"

	"airview/internal/radio"
	"airview/internal/weather"
)

const kmhPerKt = 1.852

// ModelDiff compares the observed profile with one model pressure level.
// Differences are observed minus model.
type ModelDiff struct {
	HPa          float64  `json:"hpa"`
	AltFt        float64  `json:"altFt"` // pressure altitude of the level
	ObsTempC     *float64 `json:"obsTempC,omitempty"`
	ModelTempC   *float64 `json:"modelTempC,omitempty"`
	DTempC       *float64 `json:"dTempC,omitempty"`
	ObsWindDir   *float64 `json:"obsWindDir,omitempty"`
	ObsWindKt    *float64 `json:"obsWindKt,omitempty"`
	ModelWindDir *float64 `json:"modelWindDir,omitempty"`
	ModelWindKt  *float64 `json:"modelWindKt,omitempty"`
	DSpeedKt     *float64 `json:"dSpeedKt,omitempty"`
	DDirDeg      *float64 `json:"dDirDeg,omitempty"`   // signed, -180..180
	VecDiffKt    *float64 `json:"vecDiffKt,omitempty"` // magnitude of the vector difference
}

// Comparison is the observed profile against a model profile.
type Comparison struct {
	Levels []ModelDiff `json:"levels"` // one per model level inside the observed range, ascending

	NTemp     int      `json:"nTemp"`
	TempBiasC *float64 `json:"tempBiasC,omitempty"` // mean (obs - model)
	TempRMSC  *float64 `json:"tempRmsC,omitempty"`

	NWind       int      `json:"nWind"`
	SpeedBiasKt *float64 `json:"speedBiasKt,omitempty"`
	SpeedRMSKt  *float64 `json:"speedRmsKt,omitempty"`
	DirBiasDeg  *float64 `json:"dirBiasDeg,omitempty"` // mean signed direction difference (levels with >= 10 kt)
	UBiasKt     *float64 `json:"uBiasKt,omitempty"`
	VBiasKt     *float64 `json:"vBiasKt,omitempty"`
	RMSVDKt     *float64 `json:"rmsvdKt,omitempty"` // RMS vector difference, the standard wind verification score
}

// Compare evaluates profile levels against a model profile (Open-Meteo
// pressure levels). Model levels are placed at their pressure altitude -
// the vertical coordinate aircraft report - and the observations are
// interpolated to them. Model levels outside the observed range are left
// out.
func Compare(levels []Level, model []weather.Level) Comparison {
	return compare(pointsOf(levels), model)
}

// CompareModel is Compare for the Collector's current profile.
func (c *Collector) CompareModel(now time.Time, model []weather.Level) Comparison {
	return Compare(c.Current(now).Levels, model)
}

func compare(pts []point, model []weather.Level) Comparison {
	cmp := Comparison{Levels: []ModelDiff{}}
	var sT, sT2, sS, sS2, sD, sU, sV, sVD2 float64
	nD := 0
	for _, m := range model {
		if !(m.HPa > 0) {
			continue
		}
		alt := radio.PressureAltitudeM(m.HPa) / 0.3048
		d := ModelDiff{HPa: m.HPa, AltFt: math.Round(alt)}
		used := false
		if t, ok := sampleTemp(pts, alt); ok && m.TempC.Valid() {
			dt := t - m.TempC.Float()
			d.ObsTempC, d.ModelTempC, d.DTempC = fp1(t), fp1(m.TempC.Float()), fp(round2(dt))
			sT += dt
			sT2 += dt * dt
			cmp.NTemp++
			used = true
		}
		if u, v, ok := sampleWind(pts, alt); ok && m.WindKmh.Valid() && m.WindDir.Valid() {
			ms := m.WindKmh.Float() / kmhPerKt
			sn, cs := math.Sincos(m.WindDir.Float() * math.Pi / 180)
			mu, mv := -ms*sn, -ms*cs
			os := math.Hypot(u, v)
			od := 0.0
			if os > 0 {
				od = norm360(math.Atan2(-u, -v) * 180 / math.Pi)
			}
			vd := math.Hypot(u-mu, v-mv)
			d.ObsWindDir, d.ObsWindKt = fp1(od), fp1(os)
			d.ModelWindDir, d.ModelWindKt = fp1(norm360(m.WindDir.Float())), fp1(ms)
			d.DSpeedKt, d.VecDiffKt = fp1(os-ms), fp1(vd)
			if os >= 10 && ms >= 10 {
				dd := angDiff(od, m.WindDir.Float())
				d.DDirDeg = fp1(dd)
				sD += dd
				nD++
			}
			sS += os - ms
			sS2 += (os - ms) * (os - ms)
			sU += u - mu
			sV += v - mv
			sVD2 += vd * vd
			cmp.NWind++
			used = true
		}
		if used {
			cmp.Levels = append(cmp.Levels, d)
		}
	}
	sort.SliceStable(cmp.Levels, func(i, j int) bool { return cmp.Levels[i].AltFt < cmp.Levels[j].AltFt })
	if n := float64(cmp.NTemp); n > 0 {
		cmp.TempBiasC, cmp.TempRMSC = fp(round2(sT/n)), fp(round2(math.Sqrt(sT2/n)))
	}
	if n := float64(cmp.NWind); n > 0 {
		cmp.SpeedBiasKt, cmp.SpeedRMSKt = fp1(sS/n), fp1(math.Sqrt(sS2/n))
		cmp.UBiasKt, cmp.VBiasKt = fp1(sU/n), fp1(sV/n)
		cmp.RMSVDKt = fp1(math.Sqrt(sVD2 / n))
	}
	if nD > 0 {
		cmp.DirBiasDeg = fp1(sD / float64(nD))
	}
	return cmp
}

// ------------------------------------------------- radio-relevant layers

// RadioLayer is a temperature inversion aloft seen by aircraft, with the
// refractivity gradient it implies for 1090 MHz propagation. A temperature
// inversion alone bends rays only a little more than standard; it becomes
// super-refractive or ducting when the humidity also drops through it,
// which is why the model's humidity is combined with the observed
// temperatures here.
type RadioLayer struct {
	BaseFt     float64 `json:"baseFt"` // pressure altitude
	TopFt      float64 `json:"topFt"`
	BaseM      float64 `json:"baseM"` // the same in metres (standard atmosphere)
	TopM       float64 `json:"topM"`
	ThicknessM float64 `json:"thicknessM"` // geometric
	StrengthC  float64 `json:"strengthC"`  // temperature rise through the layer
	GradKPerKm float64 `json:"grad"`       // dT/dz
	// DNDH is the mean refractivity gradient through the layer, N-units
	// per km (standard about -40; <= -79 super-refraction; <= -157
	// ducting). nil when it could not be computed.
	DNDH  *float64 `json:"dndh,omitempty"`
	K     *float64 `json:"k,omitempty"`     // effective earth radius factor for that gradient
	Class string   `json:"class,omitempty"` // radio.Class* of DNDH
	// Humidity is "model" when the model's relative humidity was
	// available at both ends of the layer, "dry" when the gradient is the
	// dry-air term only (a lower bound of the bending).
	Humidity string   `json:"humidity"`
	RHBase   *float64 `json:"rhBase,omitempty"`
	RHTop    *float64 `json:"rhTop,omitempty"`
}

// RadioInversions is the propagation product of a profile.
type RadioInversions struct {
	Layers []RadioLayer `json:"layers"` // ascending
	// Strongest is the index into Layers of the most negative refractivity
	// gradient (-1 when there is none).
	Strongest int `json:"strongest"`
	// Class is the regime of the strongest layer ("" without layers).
	Class string `json:"class"`
	// LowestBaseFt is the base of the lowest inversion (nil without one):
	// the most relevant one for signals arriving near the horizon.
	LowestBaseFt *float64 `json:"lowestBaseFt,omitempty"`
	// Note is a one-line plain-language summary.
	Note string `json:"note"`
}

// RadioLayers evaluates the temperature inversions of a profile for radio
// propagation. model may be nil or lack humidity; the gradient is then the
// dry term only.
//
// The layers are averages over at least 1000 ft, so thin, sharp ducts are
// smoothed: a layer classed "super-refraction" here may well contain a
// thinner trapping layer.
func RadioLayers(levels []Level, model []weather.Level) RadioInversions {
	pts := pointsOf(levels)
	an := analyze(pts)
	out := RadioInversions{Layers: []RadioLayer{}, Strongest: -1}

	// Model humidity against pressure altitude.
	type rhPoint struct{ alt, rh float64 }
	var rh []rhPoint
	for _, m := range model {
		if m.HPa > 0 && m.RH.Valid() {
			rh = append(rh, rhPoint{radio.PressureAltitudeM(m.HPa) / 0.3048, math.Max(0, math.Min(100, m.RH.Float()))})
		}
	}
	sort.Slice(rh, func(i, j int) bool { return rh[i].alt < rh[j].alt })
	rhAt := func(alt float64) (float64, bool) {
		if len(rh) == 0 || alt < rh[0].alt-1500 || alt > rh[len(rh)-1].alt+1500 {
			return 0, false
		}
		i := sort.Search(len(rh), func(i int) bool { return rh[i].alt >= alt })
		switch {
		case i == 0:
			return rh[0].rh, true
		case i == len(rh):
			return rh[len(rh)-1].rh, true
		}
		a, b := rh[i-1], rh[i]
		if b.alt == a.alt {
			return b.rh, true
		}
		return a.rh + (b.rh-a.rh)*(alt-a.alt)/(b.alt-a.alt), true
	}

	best := math.Inf(1)
	for _, inv := range an.Inversions {
		l := RadioLayer{BaseFt: inv.BaseFt, TopFt: inv.TopFt,
			BaseM: math.Round(inv.BaseFt * 0.3048), TopM: math.Round(inv.TopFt * 0.3048),
			ThicknessM: inv.ThicknessM, StrengthC: inv.StrengthC, GradKPerKm: inv.GradKPerKm, Humidity: "dry"}
		pb, pt := radio.ISAFt(inv.BaseFt).PressureHPa, radio.ISAFt(inv.TopFt).PressureHPa
		rb, okb := rhAt(inv.BaseFt)
		rt, okt := rhAt(inv.TopFt)
		if okb && okt {
			l.Humidity = "model"
			l.RHBase, l.RHTop = fp1(rb), fp1(rt)
		} else {
			rb, rt = 0, 0
		}
		if inv.ThicknessM > 0 {
			nb := radio.RefractivityRH(pb, inv.BaseTempC, rb)
			nt := radio.RefractivityRH(pt, inv.TopTempC, rt)
			g := (nt - nb) / (inv.ThicknessM / 1000)
			if finite(g) {
				l.DNDH = fp1(g)
				if k := radio.KFactor(g); finite(k) {
					l.K = fp(round2(k))
				}
				l.Class = radio.ClassifyGradient(g)
				if g < best {
					best = g
					out.Strongest = len(out.Layers)
					out.Class = l.Class
				}
			}
		}
		out.Layers = append(out.Layers, l)
	}

	switch {
	case len(out.Layers) == 0:
		out.Note = "No temperature inversion aloft in the aircraft-derived profile."
	default:
		out.LowestBaseFt = fp(out.Layers[0].BaseFt)
		switch out.Class {
		case radio.ClassDucting:
			out.Note = "Inversion with a trapping refractivity gradient: elevated ducting likely, expect abnormal range in some directions."
		case radio.ClassSuperRefraction:
			out.Note = "Inversion with a super-refractive gradient: extended range beyond the normal radio horizon is likely."
		case "":
			out.Note = "Temperature inversion aloft; refractivity gradient not available."
		default:
			out.Note = "Temperature inversion aloft, but the refractivity gradient through it is near standard."
		}
	}
	return out
}

// RadioLayers is the propagation product of the current profile.
func (c *Collector) RadioLayers(now time.Time, model []weather.Level) RadioInversions {
	return RadioLayers(c.Current(now).Levels, model)
}
