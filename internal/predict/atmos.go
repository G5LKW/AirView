package predict

import (
	"encoding/json"
	"math"

	"airview/internal/radio"
	"airview/internal/store"
	"airview/internal/weather"
)

// Atmos is the radio-propagation state of the lower atmosphere derived from
// one hourly weather row (observation or forecast): the features of the
// reception model. All numbers are finite when Valid.
type Atmos struct {
	Valid bool `json:"-"`
	// HasProfile is true when a pressure-level profile was analysed; false
	// when the gradient comes from the stored columns or, as a last resort,
	// from the climatological relation with surface refractivity.
	HasProfile bool `json:"hasProfile"`
	// Estimated is true when dN/dh had to be estimated from surface
	// refractivity alone (dN = -7.32 exp(0.005577 Ns), CRPL exponential
	// reference atmosphere).
	Estimated bool `json:"estimated,omitempty"`

	NSurface      float64 `json:"nSurface"`      // surface refractivity, N-units
	DNDH          float64 `json:"dNdh"`          // gradient over the lowest km, N/km
	MinGradient   float64 `json:"minGradient"`   // most negative layer gradient below 3 km, N/km
	MinGradientM  float64 `json:"minGradientM"`  // base height of that layer, m AMSL
	K             float64 `json:"k"`             // effective earth radius factor (radio.KMax = trapping)
	HorizonFactor float64 `json:"horizonFactor"` // radio horizon relative to standard (capped at 3)
	Index         int     `json:"index"`         // radio.Assess index, 50 = standard, 85+ = ducting
	Class         string  `json:"class"`         // radio.Class*

	Ducts         int     `json:"ducts"`              // ducts found in the profile
	DuctStrengthM float64 `json:"ductStrengthM"`      // strongest duct, M-units (0 = none)
	DuctKind      string  `json:"ductKind,omitempty"` // radio.Duct* of the strongest duct
	DuctBaseM     float64 `json:"ductBaseM"`          // m AMSL
	DuctTopM      float64 `json:"ductTopM"`           // m AMSL
	Ducting1090   bool    `json:"ducting1090"`        // a duct able to trap 1090 MHz is present
	// InversionC is the temperature excess of the warmest level in the
	// lowest 1.5 km over the 2 m temperature (positive = inversion), NaN
	// without a profile.
	InversionC Num    `json:"inversionC"`
	Summary    string `json:"summary,omitempty"`

	ductTraps bool
}

// standardAtmos is the reference state used when no weather data exists.
func standardAtmos() Atmos {
	as := radio.AssessGradient(-40)
	return Atmos{
		Valid: true, Estimated: true, NSurface: 315, DNDH: -40, MinGradient: -40, K: as.K,
		HorizonFactor: as.HorizonFactor, Index: as.Index, Class: as.Class, InversionC: Num(nan),
	}
}

// profileDoc is the JSON document stored in weather_obs.profile_json /
// weather_fc.profile_json (written by EncodeProfile): the pressure-level
// profile of the hour plus the diagnostics that have no column of their own.
type profileDoc struct {
	Levels         []levelJSON `json:"levels"`
	LiftedIndex    *float64    `json:"liftedIndex,omitempty"`
	CIN            *float64    `json:"cin,omitempty"`
	BoundaryLayerM *float64    `json:"boundaryLayerM,omitempty"`
	PrecipProb     *float64    `json:"precipProb,omitempty"`
	FreezingLevelM *float64    `json:"freezingLevelM,omitempty"`
}

type levelJSON struct {
	HPa float64  `json:"hpa"`
	T   *float64 `json:"t"`
	RH  *float64 `json:"rh"`
	Z   *float64 `json:"z"`
	WS  *float64 `json:"ws,omitempty"`
	WD  *float64 `json:"wd,omitempty"`
}

// EncodeProfile returns the profile_json document for an Open-Meteo hour:
//
//	{"levels":[{"hpa":1000,"t":12.1,"rh":81,"z":110,"ws":14,"wd":230},...],
//	 "liftedIndex":1.2,"cin":-3,"boundaryLayerM":640,"precipProb":20,"freezingLevelM":2100}
//
// (levels ground-up; missing values are null / omitted). It returns "" when
// the hour has neither a profile nor any of the diagnostics. This is the
// format ParseProfile and the forecaster read; the bare array form
// [{"hpa":..},...] is accepted as well.
func EncodeProfile(h *weather.HourPoint) string {
	if h == nil {
		return ""
	}
	d := profileDoc{
		LiftedIndex: h.LiftedIndex.Ptr(), CIN: h.CIN.Ptr(), BoundaryLayerM: h.BoundaryLayerM.Ptr(),
		PrecipProb: h.PrecipProb.Ptr(), FreezingLevelM: h.FreezingLevelM.Ptr(),
	}
	for _, l := range h.Levels {
		if !l.TempC.Valid() && !l.RH.Valid() && !l.HeightM.Valid() {
			continue
		}
		d.Levels = append(d.Levels, levelJSON{
			HPa: l.HPa, T: rptr(l.TempC, 2), RH: rptr(l.RH, 1), Z: rptr(l.HeightM, 1),
			WS: rptr(l.WindKmh, 1), WD: rptr(l.WindDir, 0),
		})
	}
	if len(d.Levels) == 0 && d.LiftedIndex == nil && d.CIN == nil && d.BoundaryLayerM == nil &&
		d.PrecipProb == nil && d.FreezingLevelM == nil {
		return ""
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

func rptr(n weather.Num, d int) *float64 {
	if !n.Valid() {
		return nil
	}
	v := round(n.Float(), d)
	return &v
}

// parseProfile decodes a profile_json document (object or bare level array).
func parseProfile(s string) (profileDoc, bool) {
	var d profileDoc
	if len(s) < 2 {
		return d, false
	}
	switch s[0] {
	case '{':
		if json.Unmarshal([]byte(s), &d) != nil {
			return profileDoc{}, false
		}
	case '[':
		if json.Unmarshal([]byte(s), &d.Levels) != nil {
			return profileDoc{}, false
		}
	default:
		return d, false
	}
	return d, true
}

// ReduceToMSL reduces a station pressure (hPa) measured at elevM metres to
// mean sea level with the hypsometric formula for a 6.5 K/km lapse rate,
// using the station temperature tC (NaN = 15 °C).
func ReduceToMSL(pressureHPa, elevM, tC float64) float64 {
	if !fin(tC) {
		tC = 15
	}
	if !fin(elevM) || elevM == 0 {
		return pressureHPa
	}
	return pressureHPa * math.Pow(1-0.0065*elevM/(tC+0.0065*elevM+273.15), -5.257)
}

// surfaceFromMSL is the inverse of ReduceToMSL.
func surfaceFromMSL(pmsl, elevM, tC float64) float64 {
	if !fin(tC) {
		tC = 15
	}
	return pmsl * math.Pow(1-0.0065*elevM/(tC+0.0065*elevM+273.15), 5.257)
}

// minLevelSepM is the smallest height above the surface at which a pressure
// level is used: closer levels turn the difference between the 2 m values
// and the model level into absurd gradients.
const minLevelSepM = 100

// AnalyzeWeather derives the propagation state from one weather row.
// stationElevM is the terrain elevation of the station (m AMSL). The best
// available source wins: the pressure-level profile in ProfileJSON, else the
// stored dn_dh / duct columns, else surface refractivity alone. Valid is
// false when the row carries nothing usable.
func AnalyzeWeather(w *store.Weather, stationElevM float64) Atmos {
	a := Atmos{InversionC: Num(nan)}
	if w == nil {
		return a
	}
	if !fin(stationElevM) {
		stationElevM = 0
	}
	t, rh := w.TempC, w.RH
	if !fin(rh) && fin(t) && fin(w.DewpointC) {
		rh = radio.RHFromDewPoint(t, w.DewpointC)
	}
	ps := w.SurfacePressure
	if !fin(ps) && fin(w.PressureMSL) {
		ps = surfaceFromMSL(w.PressureMSL, stationElevM, t)
	}
	surfaceOK := fin(t) && fin(rh) && fin(ps) && ps > 300 && ps < 1100
	if surfaceOK {
		a.NSurface = radio.RefractivityRH(ps, t, rh)
	} else if fin(w.NSurface) {
		a.NSurface = w.NSurface
	} else {
		a.NSurface = nan
	}

	// 1. Full profile.
	if doc, ok := parseProfile(w.ProfileJSON); ok && surfaceOK && len(doc.Levels) > 0 {
		levels := make([]radio.Level, 0, len(doc.Levels)+1)
		levels = append(levels, radio.Level{HeightM: stationElevM + 2, PressureHPa: ps, TempC: t, RH: rh})
		firstT := nan
		for _, l := range doc.Levels {
			if l.T == nil || l.RH == nil || l.Z == nil || !(l.HPa > 0) {
				continue
			}
			if *l.Z < stationElevM+minLevelSepM || *l.Z > stationElevM+6000 || l.HPa >= ps {
				continue
			}
			if *l.Z <= stationElevM+1500 && (!fin(firstT) || *l.T > firstT) {
				firstT = *l.T // warmest level of the lowest 1.5 km
			}
			levels = append(levels, radio.Level{HeightM: *l.Z, PressureHPa: l.HPa, TempC: *l.T, RH: *l.RH})
		}
		if len(levels) >= 3 {
			if p, ok := radio.AnalyzeProfile(levels); ok {
				a.fromAssessment(radio.Assess(p))
				a.HasProfile = true
				a.NSurface = p.SurfaceN
				a.MinGradient, a.MinGradientM = p.MinGradientNPerKm, p.MinGradientHeightM
				if !fin(a.MinGradient) {
					a.MinGradient, a.MinGradientM = a.DNDH, stationElevM
				}
				if fin(firstT) {
					a.InversionC = rn(firstT-t, 1)
				}
				a.Valid = true
				return a
			}
		}
	}

	// 2. Stored gradient columns.
	if fin(w.DNDH) {
		as := radio.AssessGradient(w.DNDH)
		a.fromAssessment(as)
		a.MinGradient, a.MinGradientM = w.DNDH, stationElevM
		if fin(w.Duct) && w.Duct > 0 {
			a.Ducts, a.DuctStrengthM, a.DuctKind = 1, w.Duct, radio.DuctSurfaceBased
			a.Ducting1090 = true
			a.Class = radio.ClassDucting
			if a.Index < 80 {
				a.Index = 80
			}
		}
		if !fin(a.NSurface) {
			a.NSurface = 315
		}
		a.Valid = true
		return a
	}

	// 3. Surface refractivity only: climatological gradient.
	if fin(a.NSurface) {
		g := clamp(-7.32*math.Exp(0.005577*a.NSurface), -75, -20)
		a.fromAssessment(radio.AssessGradient(g))
		a.MinGradient, a.MinGradientM = g, stationElevM
		a.Estimated = true
		a.Valid = true
		return a
	}
	return a
}

func (a *Atmos) fromAssessment(as radio.Assessment) {
	a.DNDH = as.GradientNPerKm
	a.K = as.K
	a.HorizonFactor = as.HorizonFactor
	a.Index = as.Index
	a.Class = as.Class
	a.Ducting1090 = as.Ducting1090
	a.Summary = as.Summary
	a.Ducts = len(as.Ducts)
	for _, d := range as.Ducts {
		// The strongest 1090-capable duct, else the strongest duct.
		better := d.StrengthM > a.DuctStrengthM
		if a.DuctKind != "" && d.Traps1090 != a.ductTraps {
			better = d.Traps1090
		}
		if a.DuctKind == "" || better {
			a.DuctStrengthM, a.DuctKind, a.DuctBaseM, a.DuctTopM = d.StrengthM, d.Kind, d.BaseM, d.TopM
			a.ductTraps = d.Traps1090
		}
	}
}

// ductColumn is the value stored in the weather duct column: the strength
// (M-units) of the strongest duct that can trap 1090 MHz, 0 = none.
func (a *Atmos) ductColumn() float64 {
	if a.Ducting1090 && a.DuctStrengthM > 0 {
		return round(a.DuctStrengthM, 1)
	}
	return 0
}

// RowFromHour converts an Open-Meteo hour to a store.Weather row (T in Unix
// seconds) with the derived refractivity columns filled in: n_surface,
// dn_dh, k_factor, duct (strength in M-units of the strongest duct able to
// trap 1090 MHz, 0 = none) and profile_json (EncodeProfile). It is the
// writer-side counterpart of AnalyzeWeather: the engine uses it for
// weather_obs, weather_fc and weather_fc_log rows.
func RowFromHour(h *weather.HourPoint, stationElevM float64) store.Weather {
	w := store.NewWeather(h.T / 1000)
	w.TempC, w.DewpointC, w.RH = h.TempC.Float(), h.DewPointC.Float(), h.RH.Float()
	w.PressureMSL, w.SurfacePressure = h.PressureMSL.Float(), h.SurfacePressure.Float()
	w.WindKmh, w.WindDir, w.GustKmh = h.WindKmh.Float(), h.WindDir.Float(), h.GustKmh.Float()
	w.Cloud, w.CloudLow, w.CloudMid, w.CloudHigh = h.Cloud.Float(), h.CloudLow.Float(), h.CloudMid.Float(), h.CloudHigh.Float()
	w.PrecipMM, w.RainMM, w.SnowCM = h.PrecipMM.Float(), h.RainMM.Float(), h.SnowfallCM.Float()
	w.VisibilityM, w.CAPE = h.VisibilityM.Float(), h.CAPE.Float()
	w.WeatherCode, w.IsDay = h.WeatherCode.Float(), h.IsDay.Float()
	w.ProfileJSON = EncodeProfile(h)
	if a := AnalyzeWeather(&w, stationElevM); a.Valid {
		w.NSurface = round(a.NSurface, 1)
		if !a.Estimated {
			w.DNDH, w.KFactor, w.Duct = round(a.DNDH, 1), round(a.K, 3), a.ductColumn()
		}
	}
	return w
}
