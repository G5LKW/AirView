// Package weather fetches free weather data for the station: Open-Meteo
// (current conditions, hourly forecast, pressure-level profiles, historical
// backfill, terrain elevation; no API key) and the RainViewer radar frame
// index for the map overlay. A Poller keeps the latest Bundle fresh.
//
// Units follow the AirView API convention: °C, hPa, km/h, mm, %, metres;
// snowfall is in cm (as delivered by Open-Meteo), timestamps are Unix
// milliseconds UTC. Every measured value is a Num: NaN means "missing" and is
// encoded as JSON null.
package weather

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// Num is a float64 measurement where NaN means "not available". It encodes
// to JSON null when missing (plain float64 NaN cannot be JSON-encoded) and
// decodes null back to NaN.
type Num float64

// NaN is the missing value.
var NaN = Num(math.NaN())

// Valid reports whether the value is present (finite).
func (n Num) Valid() bool {
	f := float64(n)
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// Float returns the value as a float64 (NaN when missing).
func (n Num) Float() float64 { return float64(n) }

// Or returns the value, or def when it is missing.
func (n Num) Or(def float64) float64 {
	if n.Valid() {
		return float64(n)
	}
	return def
}

// Ptr returns a pointer to the value, or nil when it is missing (handy for
// SQL NULLs and model-style optional DTO fields).
func (n Num) Ptr() *float64 {
	if !n.Valid() {
		return nil
	}
	f := float64(n)
	return &f
}

// MarshalJSON encodes missing values as null.
func (n Num) MarshalJSON() ([]byte, error) {
	if !n.Valid() {
		return []byte("null"), nil
	}
	f := float64(n)
	format := byte('f')
	if a := math.Abs(f); a != 0 && (a < 1e-6 || a >= 1e21) {
		format = 'e'
	}
	return strconv.AppendFloat(make([]byte, 0, 24), f, format, -1, 64), nil
}

// UnmarshalJSON decodes null as NaN.
func (n *Num) UnmarshalJSON(b []byte) error {
	if len(b) == 4 && string(b) == "null" {
		*n = NaN
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return err
	}
	*n = Num(f)
	return nil
}

// Surface holds the near-surface variables that exist both as current
// conditions and as hourly values.
type Surface struct {
	TempC           Num `json:"tempC"`           // air temperature at 2 m
	RH              Num `json:"rh"`              // relative humidity at 2 m, %
	DewPointC       Num `json:"dewPointC"`       // dew point at 2 m
	ApparentC       Num `json:"apparentC"`       // feels-like temperature
	PrecipMM        Num `json:"precipMm"`        // total precipitation in the preceding hour (rain + showers + snow water)
	RainMM          Num `json:"rainMm"`          // large-scale rain
	ShowersMM       Num `json:"showersMm"`       // convective rain
	SnowfallCM      Num `json:"snowCm"`          // snowfall, cm
	WeatherCode     Num `json:"weatherCode"`     // WMO weather interpretation code, see WeatherCodeText
	Cloud           Num `json:"cloud"`           // total cloud cover, %
	CloudLow        Num `json:"cloudLow"`        // cloud below 2 km, %
	CloudMid        Num `json:"cloudMid"`        // cloud 2–6 km, %
	CloudHigh       Num `json:"cloudHigh"`       // cloud above 6 km, %
	PressureMSL     Num `json:"pressureMsl"`     // sea-level pressure, hPa
	SurfacePressure Num `json:"surfacePressure"` // pressure at the surface, hPa
	WindKmh         Num `json:"windKmh"`         // wind speed at 10 m
	WindDir         Num `json:"windDir"`         // wind direction at 10 m, degrees the wind blows FROM
	GustKmh         Num `json:"gustKmh"`         // gusts at 10 m
	VisibilityM     Num `json:"visibilityM"`     // horizontal visibility, m
	IsDay           Num `json:"isDay"`           // 1 = daylight, 0 = night
	CAPE            Num `json:"cape"`            // convective available potential energy, J/kg
}

// Current is the latest analysed conditions (Open-Meteo "current", a
// 15-minute value interpolated from the best model for the location).
type Current struct {
	T           int64 `json:"t"`           // Unix ms of the value
	IntervalSec int   `json:"intervalSec"` // aggregation interval of the value (900)
	Surface
}

// Time returns the timestamp of the value.
func (c *Current) Time() time.Time { return time.UnixMilli(c.T).UTC() }

// Level is one isobaric level of the vertical profile.
type Level struct {
	HPa     float64 `json:"hpa"` // pressure level
	TempC   Num     `json:"t"`   // air temperature
	RH      Num     `json:"rh"`  // relative humidity, %
	HeightM Num     `json:"z"`   // geopotential height, metres above mean sea level
	WindKmh Num     `json:"ws"`  // wind speed
	WindDir Num     `json:"wd"`  // direction the wind blows FROM, degrees
}

// HourPoint is one hour of data: surface variables, boundary-layer and
// stability diagnostics, low-level tower heights and the pressure-level
// profile. Whether it is an analysis/observation or a forecast depends only
// on T relative to the time of the fetch (see Bundle.Split).
type HourPoint struct {
	T int64 `json:"t"` // Unix ms, start of the hour (UTC)
	Surface

	PrecipProb     Num `json:"precipProb"`     // probability of precipitation, % (forecast only)
	BoundaryLayerM Num `json:"boundaryLayerM"` // planetary boundary layer height above ground, m
	LiftedIndex    Num `json:"liftedIndex"`    // K; negative = unstable
	CIN            Num `json:"cin"`            // convective inhibition, J/kg
	FreezingLevelM Num `json:"freezingLevelM"` // 0 °C level, metres above mean sea level
	ShortwaveWm2   Num `json:"shortwaveWm2"`   // global horizontal irradiance, preceding-hour mean, W/m²
	SoilTempC      Num `json:"soilTempC"`      // soil surface temperature (0 cm; 0–7 cm for reanalysis)
	PWATmm         Num `json:"pwatMm"`         // total column integrated water vapour, kg/m² (= mm)
	SnowDepthM     Num `json:"snowDepthM"`     // snow depth on the ground, m

	Temp80C    Num `json:"temp80C"`    // temperature 80 m above ground
	Temp120C   Num `json:"temp120C"`   // 120 m
	Temp180C   Num `json:"temp180C"`   // 180 m
	Wind80Kmh  Num `json:"wind80Kmh"`  // wind speed 80 m above ground
	Wind100Kmh Num `json:"wind100Kmh"` // 100 m (reanalysis archive only)
	Wind120Kmh Num `json:"wind120Kmh"` // 120 m
	Wind180Kmh Num `json:"wind180Kmh"` // 180 m
	WindDir80  Num `json:"windDir80"`  // wind direction 80 m above ground
	WindDir100 Num `json:"windDir100"` // 100 m (reanalysis archive only)
	WindDir120 Num `json:"windDir120"` // 120 m
	WindDir180 Num `json:"windDir180"` // 180 m

	// Levels is the pressure-level profile ordered from the ground up
	// (1000 hPa first). Nil when the request did not include levels (surface-
	// only archive data). Levels below the terrain are still reported by the
	// model (extrapolated); compare Level.HeightM with the station elevation.
	Levels []Level `json:"levels,omitempty"`
}

// Time returns the start of the hour.
func (h *HourPoint) Time() time.Time { return time.UnixMilli(h.T).UTC() }

// HasProfile reports whether the hour carries a usable vertical profile
// (at least two levels with temperature, humidity and height).
func (h *HourPoint) HasProfile() bool {
	n := 0
	for i := range h.Levels {
		l := &h.Levels[i]
		if l.TempC.Valid() && l.RH.Valid() && l.HeightM.Valid() {
			n++
		}
	}
	return n >= 2
}

// Bundle kinds.
const (
	KindForecast = "forecast" // api.open-meteo.com/v1/forecast (past days + forecast)
	// KindHistoricalForecast: archived runs of the high-resolution forecast
	// models, with pressure levels where the archive has them (see HistorySource).
	KindHistoricalForecast = "historical-forecast"
	KindArchive            = "archive" // ERA5 reanalysis (1940+), surface variables only
	// KindMixed: a history bundle whose hours come partly from the model
	// archive and partly from ERA5 (HistoryAuto across the start of the model
	// archive). Tell the hours apart by HourPoint.Levels (nil for ERA5 hours).
	KindMixed = KindArchive + "+" + KindHistoricalForecast
)

// Attribution required by the data licences.
const (
	Source              = "open-meteo"
	Attribution         = "Weather data by Open-Meteo.com (CC BY 4.0)"
	AttributionURL      = "https://open-meteo.com/"
	RadarAttribution    = "Radar data by RainViewer"
	RadarAttributionURL = "https://www.rainviewer.com/"
)

// Bundle is one self-contained weather data set for a location. A published
// Bundle is immutable: treat it as read-only and share the pointer freely.
type Bundle struct {
	Source         string `json:"source"`         // "open-meteo"
	Kind           string `json:"kind"`           // Kind* constant
	Model          string `json:"model"`          // weather model selection ("best_match" unless overridden)
	Attribution    string `json:"attribution"`    // licence credit to show next to the data
	AttributionURL string `json:"attributionUrl"` //

	Lat     float64 `json:"lat"`     // requested coordinate
	Lon     float64 `json:"lon"`     //
	GridLat Num     `json:"gridLat"` // centre of the model grid cell actually used (NaN if not reported)
	GridLon Num     `json:"gridLon"` //
	// ElevationM is the terrain elevation (90 m DEM) Open-Meteo used for
	// statistical downscaling at the requested coordinate.
	ElevationM Num `json:"elevationM"`

	Fetched int64 `json:"fetched"` // Unix ms when the data was downloaded
	// Cost is the weighted number of Open-Meteo API calls this bundle consumed
	// (as reported by the server, else estimated).
	Cost float64 `json:"cost"`

	Current *Current    `json:"current"` // nil for historical bundles
	Hourly  []HourPoint `json:"hourly"`  // ascending in time, one per hour
	// LevelsHPa lists the pressure levels requested for the profiles.
	LevelsHPa []int `json:"levelsHpa,omitempty"`
}

// FetchedTime returns the download time.
func (b *Bundle) FetchedTime() time.Time { return time.UnixMilli(b.Fetched).UTC() }

// Index returns the index of the hourly point whose hour contains t, or -1.
func (b *Bundle) Index(t time.Time) int {
	if b == nil || len(b.Hourly) == 0 {
		return -1
	}
	ms := t.UnixMilli()
	// First point strictly after t, minus one.
	i := sort.Search(len(b.Hourly), func(i int) bool { return b.Hourly[i].T > ms }) - 1
	if i < 0 || ms-b.Hourly[i].T >= int64(time.Hour/time.Millisecond) {
		return -1
	}
	return i
}

// At returns the hourly point whose hour contains t.
func (b *Bundle) At(t time.Time) (HourPoint, bool) {
	i := b.Index(t)
	if i < 0 {
		return HourPoint{}, false
	}
	return b.Hourly[i], true
}

// Split divides the hourly points at now: past holds every hour that has
// started (T <= now; model analysis / short-range hindcast), future the
// forecast hours. Both alias b.Hourly.
func (b *Bundle) Split(now time.Time) (past, future []HourPoint) {
	if b == nil {
		return nil, nil
	}
	ms := now.UnixMilli()
	i := sort.Search(len(b.Hourly), func(i int) bool { return b.Hourly[i].T > ms })
	return b.Hourly[:i], b.Hourly[i:]
}

// WeatherCodeText returns the English description of a WMO weather
// interpretation code as used by Open-Meteo ("" for unknown codes).
func WeatherCodeText(code int) string {
	switch code {
	case 0:
		return "Clear sky"
	case 1:
		return "Mainly clear"
	case 2:
		return "Partly cloudy"
	case 3:
		return "Overcast"
	case 45:
		return "Fog"
	case 48:
		return "Depositing rime fog"
	case 51:
		return "Light drizzle"
	case 53:
		return "Moderate drizzle"
	case 55:
		return "Dense drizzle"
	case 56:
		return "Light freezing drizzle"
	case 57:
		return "Dense freezing drizzle"
	case 61:
		return "Slight rain"
	case 63:
		return "Moderate rain"
	case 65:
		return "Heavy rain"
	case 66:
		return "Light freezing rain"
	case 67:
		return "Heavy freezing rain"
	case 71:
		return "Slight snowfall"
	case 73:
		return "Moderate snowfall"
	case 75:
		return "Heavy snowfall"
	case 77:
		return "Snow grains"
	case 80:
		return "Slight rain showers"
	case 81:
		return "Moderate rain showers"
	case 82:
		return "Violent rain showers"
	case 85:
		return "Slight snow showers"
	case 86:
		return "Heavy snow showers"
	case 95:
		return "Thunderstorm"
	case 96:
		return "Thunderstorm with slight hail"
	case 99:
		return "Thunderstorm with heavy hail"
	}
	return ""
}
