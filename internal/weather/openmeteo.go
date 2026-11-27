package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultLevels are the pressure levels (hPa) requested for the vertical
// profile, ground first. They span the surface to the tropopause, dense in
// the lowest 1.5 km where ducting layers form.
var DefaultLevels = []int{1000, 975, 950, 925, 900, 850, 800, 700, 600, 500, 400, 300, 250, 200}

// Variable tables: the single source of truth for both the request variable
// lists and the response decoding. All names verified against the live API.

type surfaceVar struct {
	name string
	set  func(*Surface, Num)
}

var surfaceVars = []surfaceVar{
	{"temperature_2m", func(s *Surface, v Num) { s.TempC = v }},
	{"relative_humidity_2m", func(s *Surface, v Num) { s.RH = v }},
	{"dew_point_2m", func(s *Surface, v Num) { s.DewPointC = v }},
	{"apparent_temperature", func(s *Surface, v Num) { s.ApparentC = v }},
	{"precipitation", func(s *Surface, v Num) { s.PrecipMM = v }},
	{"rain", func(s *Surface, v Num) { s.RainMM = v }},
	{"showers", func(s *Surface, v Num) { s.ShowersMM = v }},
	{"snowfall", func(s *Surface, v Num) { s.SnowfallCM = v }},
	{"weather_code", func(s *Surface, v Num) { s.WeatherCode = v }},
	{"cloud_cover", func(s *Surface, v Num) { s.Cloud = v }},
	{"cloud_cover_low", func(s *Surface, v Num) { s.CloudLow = v }},
	{"cloud_cover_mid", func(s *Surface, v Num) { s.CloudMid = v }},
	{"cloud_cover_high", func(s *Surface, v Num) { s.CloudHigh = v }},
	{"pressure_msl", func(s *Surface, v Num) { s.PressureMSL = v }},
	{"surface_pressure", func(s *Surface, v Num) { s.SurfacePressure = v }},
	{"wind_speed_10m", func(s *Surface, v Num) { s.WindKmh = v }},
	{"wind_direction_10m", func(s *Surface, v Num) { s.WindDir = v }},
	{"wind_gusts_10m", func(s *Surface, v Num) { s.GustKmh = v }},
	{"visibility", func(s *Surface, v Num) { s.VisibilityM = v }},
	{"is_day", func(s *Surface, v Num) { s.IsDay = v }},
	{"cape", func(s *Surface, v Num) { s.CAPE = v }},
}

// archiveMissing lists surface variables the ERA5 archive does not carry
// (it answers them with all-null columns, so they are not requested).
var archiveMissing = map[string]bool{"visibility": true, "cape": true}

type hourVar struct {
	name string
	set  func(*HourPoint, Num)
}

// forecastExtraVars are the hourly-only variables of the forecast and
// historical-forecast APIs.
var forecastExtraVars = []hourVar{
	{"precipitation_probability", func(h *HourPoint, v Num) { h.PrecipProb = v }},
	{"boundary_layer_height", func(h *HourPoint, v Num) { h.BoundaryLayerM = v }},
	{"lifted_index", func(h *HourPoint, v Num) { h.LiftedIndex = v }},
	{"convective_inhibition", func(h *HourPoint, v Num) { h.CIN = v }},
	{"freezing_level_height", func(h *HourPoint, v Num) { h.FreezingLevelM = v }},
	{"shortwave_radiation", func(h *HourPoint, v Num) { h.ShortwaveWm2 = v }},
	{"soil_temperature_0cm", func(h *HourPoint, v Num) { h.SoilTempC = v }},
	{"total_column_integrated_water_vapour", func(h *HourPoint, v Num) { h.PWATmm = v }},
	{"snow_depth", func(h *HourPoint, v Num) { h.SnowDepthM = v }},
	{"temperature_80m", func(h *HourPoint, v Num) { h.Temp80C = v }},
	{"temperature_120m", func(h *HourPoint, v Num) { h.Temp120C = v }},
	{"temperature_180m", func(h *HourPoint, v Num) { h.Temp180C = v }},
	{"wind_speed_80m", func(h *HourPoint, v Num) { h.Wind80Kmh = v }},
	{"wind_speed_120m", func(h *HourPoint, v Num) { h.Wind120Kmh = v }},
	{"wind_speed_180m", func(h *HourPoint, v Num) { h.Wind180Kmh = v }},
	{"wind_direction_80m", func(h *HourPoint, v Num) { h.WindDir80 = v }},
	{"wind_direction_120m", func(h *HourPoint, v Num) { h.WindDir120 = v }},
	{"wind_direction_180m", func(h *HourPoint, v Num) { h.WindDir180 = v }},
}

// archiveExtraVars are the hourly-only variables of the ERA5 archive API.
var archiveExtraVars = []hourVar{
	{"boundary_layer_height", func(h *HourPoint, v Num) { h.BoundaryLayerM = v }},
	{"shortwave_radiation", func(h *HourPoint, v Num) { h.ShortwaveWm2 = v }},
	{"soil_temperature_0_to_7cm", func(h *HourPoint, v Num) { h.SoilTempC = v }},
	{"total_column_integrated_water_vapour", func(h *HourPoint, v Num) { h.PWATmm = v }},
	{"snow_depth", func(h *HourPoint, v Num) { h.SnowDepthM = v }},
	{"wind_speed_100m", func(h *HourPoint, v Num) { h.Wind100Kmh = v }},
	{"wind_direction_100m", func(h *HourPoint, v Num) { h.WindDir100 = v }},
}

type levelVar struct {
	prefix string
	set    func(*Level, Num)
}

var levelVars = []levelVar{
	{"temperature", func(l *Level, v Num) { l.TempC = v }},
	{"relative_humidity", func(l *Level, v Num) { l.RH = v }},
	{"geopotential_height", func(l *Level, v Num) { l.HeightM = v }},
	{"wind_speed", func(l *Level, v Num) { l.WindKmh = v }},
	{"wind_direction", func(l *Level, v Num) { l.WindDir = v }},
}

func levelName(prefix string, hpa int) string {
	return prefix + "_" + strconv.Itoa(hpa) + "hPa"
}

// nanSurface / nanHour are templates with every measurement missing.
var (
	nanSurface = func() Surface {
		var s Surface
		for _, v := range surfaceVars {
			v.set(&s, NaN)
		}
		return s
	}()
	nanHour = func() HourPoint {
		h := HourPoint{Surface: nanSurface}
		for _, v := range forecastExtraVars {
			v.set(&h, NaN)
		}
		for _, v := range archiveExtraVars {
			v.set(&h, NaN)
		}
		return h
	}()
	nanLevel = func() Level {
		var l Level
		for _, v := range levelVars {
			v.set(&l, NaN)
		}
		return l
	}()
)

// varSet is the variable selection of one request.
type varSet struct {
	current []surfaceVar // "current" block; nil = none
	surface []surfaceVar // hourly surface variables
	extra   []hourVar    // hourly-only variables
	levels  []int        // pressure levels; nil = none
}

// names returns the request variable lists, leaving out anything in skip.
func (vs varSet) names(skip map[string]bool) (current, hourly []string) {
	for _, v := range vs.current {
		if !skip[v.name] {
			current = append(current, v.name)
		}
	}
	for _, v := range vs.surface {
		if !skip[v.name] {
			hourly = append(hourly, v.name)
		}
	}
	for _, v := range vs.extra {
		if !skip[v.name] {
			hourly = append(hourly, v.name)
		}
	}
	for _, hpa := range vs.levels {
		for _, v := range levelVars {
			if n := levelName(v.prefix, hpa); !skip[n] {
				hourly = append(hourly, n)
			}
		}
	}
	return current, hourly
}

// estimateCost returns Open-Meteo's weighted call count for a request:
//
//	max(1, variables/10 × max(1, days/14))
//
// Derived from the X-RateLimit-Cost response header of real requests
// (variables × days → cost): 130×9 → 13.0, 45×2 → 4.5, 20×7 → 2.0,
// 20×15 → 2.142857, 20×28 → 4.0, 10×28 → 2.0, 5×56 → 2.0, 1×56 → 1.0,
// 1×108 → 1.0. The header value replaces the estimate whenever the server
// sends it.
func estimateCost(variables, days int) float64 {
	return math.Max(1, float64(variables)/10*math.Max(1, float64(days)/14))
}

// ForecastOptions selects what Client.Forecast downloads. The zero value
// requests current conditions only; use DefaultForecastOptions for the
// standard station bundle.
type ForecastOptions struct {
	PastDays     int   // hourly history to include, 0..92 days
	ForecastDays int   // hourly forecast to include (today counts as day 1), 0..16 days
	NoCurrent    bool  // leave out the current-conditions block
	NoLevels     bool  // leave out the pressure-level profiles (cost drops from 13.0 to 6.0 calls)
	Levels       []int // pressure levels in hPa; nil = DefaultLevels
	// Model selects one Open-Meteo weather model (e.g. "icon_seamless",
	// "ecmwf_ifs025", "gfs_seamless"); "" = automatic best match.
	Model string
}

// DefaultForecastOptions returns the standard request: current conditions,
// 2 past days, 7 forecast days, 14 pressure levels (13.0 weighted calls).
func DefaultForecastOptions() ForecastOptions {
	return ForecastOptions{PastDays: 2, ForecastDays: 7}
}

var modelRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func checkModel(m string) error {
	if m != "" && !modelRE.MatchString(m) {
		return fmt.Errorf("weather: invalid model name %q", m)
	}
	return nil
}

func checkLevels(levels []int) ([]int, error) {
	if levels == nil {
		return DefaultLevels, nil
	}
	if len(levels) > 32 {
		return nil, errors.New("weather: too many pressure levels")
	}
	for _, l := range levels {
		if l < 10 || l > 1100 {
			return nil, fmt.Errorf("weather: invalid pressure level %d hPa", l)
		}
	}
	return levels, nil
}

// Forecast downloads current conditions, the hourly past/forecast window and
// the pressure-level profiles for a coordinate in a single request.
func (c *Client) Forecast(ctx context.Context, lat, lon float64, o ForecastOptions) (*Bundle, error) {
	if !validCoord(lat, lon) {
		return nil, ErrBadCoordinate
	}
	if err := checkModel(o.Model); err != nil {
		return nil, err
	}
	o.PastDays = min(max(o.PastDays, 0), 92)
	o.ForecastDays = min(max(o.ForecastDays, 0), 16)
	days := o.PastDays + o.ForecastDays

	var vs varSet
	if !o.NoCurrent {
		vs.current = surfaceVars
	}
	if days > 0 {
		vs.surface = surfaceVars
		vs.extra = forecastExtraVars
		if !o.NoLevels {
			lv, err := checkLevels(o.Levels)
			if err != nil {
				return nil, err
			}
			vs.levels = lv
		}
	}
	if vs.current == nil && days == 0 {
		return nil, errors.New("weather: nothing requested (no current block and zero days)")
	}
	params := "past_days=" + strconv.Itoa(o.PastDays) + "&forecast_days=" + strconv.Itoa(o.ForecastDays)
	return c.fetchBundle(ctx, bundleRequest{
		kind: KindForecast, base: c.forecastURL, lat: lat, lon: lon,
		params: params, vs: vs, days: days, model: o.Model,
	})
}

type bundleRequest struct {
	kind     string
	base     string
	lat, lon float64
	params   string // endpoint-specific query parameters
	vs       varSet
	days     int
	model    string
	pace     bool
	timeout  time.Duration // per-attempt timeout; 0 = Client default
}

var unknownVarRE = regexp.MustCompile(`invalid String value ([A-Za-z0-9_]+)`)

// fetchBundle runs one Open-Meteo request and decodes it. If the server
// rejects a variable name as unknown (HTTP 400 "... invalid String value
// <name>", e.g. after an upstream rename) the variable is dropped for the
// lifetime of the client and the request repeated, so one renamed field
// cannot take the whole weather feed down.
func (c *Client) fetchBundle(ctx context.Context, br bundleRequest) (*Bundle, error) {
	for tries := 0; ; tries++ {
		c.mu.Lock()
		skip := maps.Clone(c.skip) // snapshot: the same set drives request and decoding
		c.mu.Unlock()
		current, hourly := br.vs.names(skip)

		var sb strings.Builder
		sb.WriteString(br.base)
		if strings.Contains(br.base, "?") {
			sb.WriteByte('&')
		} else {
			sb.WriteByte('?')
		}
		sb.WriteString("latitude=" + fmtCoord(br.lat) + "&longitude=" + fmtCoord(br.lon))
		if len(current) > 0 {
			sb.WriteString("&current=" + strings.Join(current, ","))
		}
		if len(hourly) > 0 {
			sb.WriteString("&hourly=" + strings.Join(hourly, ","))
		}
		sb.WriteString("&" + br.params)
		if br.model != "" {
			sb.WriteString("&models=" + br.model)
		}
		sb.WriteString("&timezone=GMT&timeformat=unixtime&temperature_unit=celsius&wind_speed_unit=kmh&precipitation_unit=mm")

		body, cost, err := c.get(ctx, request{
			url:     sb.String(),
			cost:    estimateCost(len(current)+len(hourly), br.days),
			pace:    br.pace,
			timeout: br.timeout,
		})
		if err != nil {
			var ae *APIError
			if tries < 8 && errors.As(err, &ae) && ae.Status == 400 {
				if m := unknownVarRE.FindStringSubmatch(ae.Reason); m != nil && contains(current, hourly, m[1]) {
					c.mu.Lock()
					c.skip[m[1]] = true
					c.mu.Unlock()
					c.log.Warn("weather: Open-Meteo rejected a variable; continuing without it", "variable", m[1])
					continue
				}
			}
			return nil, err
		}
		b, err := parseBundle(body, br.vs, skip)
		if err != nil {
			return nil, err
		}
		b.Kind = br.kind
		b.Model = br.model
		if b.Model == "" {
			b.Model = "best_match"
		}
		b.Lat, b.Lon = br.lat, br.lon
		b.Fetched = c.now().UnixMilli()
		b.Cost = cost
		return b, nil
	}
}

func contains(a, b []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	for _, x := range b {
		if x == s {
			return true
		}
	}
	return false
}

// omResponse is the part of an Open-Meteo answer AirView reads. Columns are
// kept raw so that unknown or oddly typed extra fields can never break
// decoding of the ones we need.
type omResponse struct {
	Latitude  Num                        `json:"latitude"`
	Longitude Num                        `json:"longitude"`
	Elevation Num                        `json:"elevation"`
	Current   map[string]json.RawMessage `json:"current"`
	Hourly    map[string]json.RawMessage `json:"hourly"`
	Error     bool                       `json:"error"`
	Reason    string                     `json:"reason"`
}

// parseBundle decodes an Open-Meteo forecast/archive response (requested
// with timeformat=unixtime) according to vs. Columns missing from the
// response and null values become NaN. Variables named in skip were not
// requested and are left missing even if the body happens to carry them.
func parseBundle(body []byte, vs varSet, skip map[string]bool) (*Bundle, error) {
	var r omResponse
	r.Latitude, r.Longitude, r.Elevation = NaN, NaN, NaN
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("weather: decoding Open-Meteo response: %w", err)
	}
	if r.Error {
		return nil, &APIError{Status: 200, Reason: r.Reason}
	}
	b := &Bundle{
		Source:         Source,
		Attribution:    Attribution,
		AttributionURL: AttributionURL,
		GridLat:        r.Latitude,
		GridLon:        r.Longitude,
		ElevationM:     r.Elevation,
		Hourly:         []HourPoint{},
	}

	if vs.current != nil {
		if r.Current == nil {
			return nil, errors.New("weather: Open-Meteo response has no current block")
		}
		num := func(name string) Num {
			raw, ok := r.Current[name]
			if !ok || skip[name] {
				return NaN
			}
			v := NaN
			if json.Unmarshal(raw, &v) != nil {
				return NaN
			}
			return v
		}
		t := num("time")
		if !t.Valid() || t < 0 {
			return nil, errors.New("weather: Open-Meteo current block has no valid time")
		}
		cur := &Current{T: int64(t) * 1000, Surface: nanSurface}
		if iv := num("interval"); iv.Valid() && iv > 0 {
			cur.IntervalSec = int(iv)
		}
		for _, v := range vs.current {
			v.set(&cur.Surface, num(v.name))
		}
		b.Current = cur
	}

	if vs.surface == nil && vs.extra == nil && vs.levels == nil {
		return b, nil
	}
	if r.Hourly == nil {
		return nil, errors.New("weather: Open-Meteo response has no hourly block")
	}
	column := func(name string) []Num {
		raw, ok := r.Hourly[name]
		if !ok || skip[name] {
			return nil
		}
		var col []Num
		if json.Unmarshal(raw, &col) != nil {
			return nil
		}
		return col
	}
	times := column("time")
	if times == nil {
		return nil, errors.New("weather: Open-Meteo hourly block has no usable time column")
	}
	n := len(times)
	hours := make([]HourPoint, n)
	for i, t := range times {
		if !t.Valid() || t < 0 {
			return nil, errors.New("weather: Open-Meteo hourly time column contains invalid values")
		}
		hours[i] = nanHour
		hours[i].T = int64(t) * 1000
		if i > 0 && hours[i].T <= hours[i-1].T {
			return nil, errors.New("weather: Open-Meteo hourly time column is not ascending")
		}
	}
	for _, v := range vs.surface {
		col := column(v.name)
		for i := 0; i < n && i < len(col); i++ {
			v.set(&hours[i].Surface, col[i])
		}
	}
	for _, v := range vs.extra {
		col := column(v.name)
		for i := 0; i < n && i < len(col); i++ {
			v.set(&hours[i], col[i])
		}
	}
	if nl := len(vs.levels); nl > 0 {
		backing := make([]Level, n*nl)
		for i := range hours {
			lv := backing[i*nl : (i+1)*nl : (i+1)*nl]
			for j, hpa := range vs.levels {
				lv[j] = nanLevel
				lv[j].HPa = float64(hpa)
			}
			hours[i].Levels = lv
		}
		for j, hpa := range vs.levels {
			for _, v := range levelVars {
				col := column(levelName(v.prefix, hpa))
				for i := 0; i < n && i < len(col); i++ {
					v.set(&hours[i].Levels[j], col[i])
				}
			}
		}
		b.LevelsHPa = append([]int(nil), vs.levels...)
	}
	b.Hourly = hours
	return b, nil
}

// HistorySource selects the Open-Meteo data set used for backfill.
//
// What the two historical data sets carry (verified against the live API on
// 2026-11-12 at 50°N 8°E with the default "best match" model selection; the
// dates depend on which models cover the location):
//
//   - Historical-forecast API: archived runs of the high-resolution forecast
//     models, the same variables as Forecast INCLUDING pressure-level
//     profiles. The API accepts dates from 2016-01-01 but answers nulls before
//     its archives begin: surface variables from 2017-01-01, pressure levels,
//     CAPE and visibility from 2021-03-23, 80–180 m values from 2022-11-13,
//     precipitation probability from 2024-04-25, boundary layer height from
//     2024-09-01.
//   - ERA5 archive API (1940 onward, about 5 days behind real time): surface
//     variables only - no pressure levels, CAPE, visibility, lifted index,
//     CIN, freezing level or precipitation probability.
type HistorySource int

const (
	// HistoryAuto asks the historical-forecast API first and fills every hour
	// it has no surface data for (TempC missing) from the ERA5 archive, so a
	// backfill always yields the best data available for each hour: profiles
	// where the model archive has them, surface-only reanalysis before.
	HistoryAuto HistorySource = iota
	// HistoryModel forces the historical-forecast API (2016-01-01 onward).
	// Hours before the model archive begins come back with every value missing.
	HistoryModel
	// HistoryReanalysis forces the ERA5 archive API: surface variables only.
	HistoryReanalysis
)

// HistoryOptions tunes a historical backfill.
type HistoryOptions struct {
	Source    HistorySource
	NoLevels  bool   // leave out pressure-level profiles (historical-forecast only)
	Levels    []int  // nil = DefaultLevels
	ChunkDays int    // days per request, 1..31 (default 14)
	Model     string // "" = best match
}

var (
	// historicalForecastStart is the first date the historical-forecast API
	// accepts ("Parameter 'start_date' is out of allowed range from 2016-01-01").
	historicalForecastStart = time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)
	archiveStart            = time.Date(1940, 1, 1, 0, 0, 0, 0, time.UTC)
)

const day = 24 * time.Hour

func utcDate(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func dateParams(start, end time.Time) string {
	return "start_date=" + start.Format("2006-01-02") + "&end_date=" + end.Format("2006-01-02")
}

// HistoryEach downloads hourly data for every UTC day from the day of `from`
// to the day of `to` (both inclusive) in chunks, oldest first, calling fn
// with each chunk's Bundle (Current is nil; Kind tells where the hours came
// from, KindMixed when a chunk straddles the start of the model archive). It
// stops at the first error.
//
// Requests are paced to stay inside the per-minute budget, so long ranges
// simply take longer; the hourly/daily budget still applies. With profiles a
// 14-day chunk costs about 11 weighted calls (≈ 290 per year of backfill),
// surface-only ERA5 chunks about 2.6. In HistoryAuto mode, locating the start
// of the model archive costs one wasted chunk plus at most a dozen one-call
// probes per HistoryEach call, and only when the range reaches back that far.
func (c *Client) HistoryEach(ctx context.Context, lat, lon float64, from, to time.Time, o HistoryOptions, fn func(*Bundle) error) error {
	if !validCoord(lat, lon) {
		return ErrBadCoordinate
	}
	if err := checkModel(o.Model); err != nil {
		return err
	}
	from, to = utcDate(from), utcDate(to)
	if to.Before(from) {
		return errors.New("weather: history range ends before it starts")
	}
	if from.Before(archiveStart) {
		return errors.New("weather: history is available from 1940-01-01")
	}
	if o.Source == HistoryModel && from.Before(historicalForecastStart) {
		return errors.New("weather: the historical-forecast data set starts on 2016-01-01")
	}
	chunk := o.ChunkDays
	if chunk <= 0 {
		chunk = 14
	}
	chunk = min(chunk, 31)

	hfVars := varSet{surface: surfaceVars, extra: forecastExtraVars}
	if !o.NoLevels {
		lv, err := checkLevels(o.Levels)
		if err != nil {
			return err
		}
		hfVars.levels = lv
	}
	arVars := varSet{extra: archiveExtraVars}
	for _, v := range surfaceVars {
		if !archiveMissing[v.name] {
			arVars.surface = append(arVars.surface, v)
		}
	}
	fetch := func(kind string, start, end time.Time) (*Bundle, error) {
		br := bundleRequest{
			kind: kind, lat: lat, lon: lon,
			params:  dateParams(start, end),
			days:    int(end.Sub(start)/day) + 1,
			model:   o.Model,
			pace:    true,
			timeout: c.histTimeout,
		}
		switch kind {
		case KindArchive:
			br.base, br.vs = c.archiveURL, arVars
		case kindProbe:
			br.kind, br.base, br.vs = KindHistoricalForecast, c.histFcURL, probeVars
		default:
			br.base, br.vs = c.histFcURL, hfVars
		}
		b, err := c.fetchBundle(ctx, br)
		if err != nil {
			return nil, fmt.Errorf("history %s..%s: %w", start.Format("2006-01-02"), end.Format("2006-01-02"), err)
		}
		return b, nil
	}

	// modelFrom is the first day worth asking the model archive for. It
	// starts at the API's hard limit and moves forward once a chunk shows
	// where the archive really begins for this location.
	modelFrom := historicalForecastStart
	probed := false

	for start := from; !start.After(to); {
		useArchive := o.Source == HistoryReanalysis ||
			(o.Source == HistoryAuto && start.Before(modelFrom))
		end := start.Add(time.Duration(chunk-1) * day)
		if end.After(to) {
			end = to
		}
		if useArchive && o.Source == HistoryAuto && !end.Before(modelFrom) {
			end = modelFrom.Add(-day)
		}
		var b *Bundle
		var err error
		if useArchive {
			b, err = fetch(KindArchive, start, end)
		} else {
			b, err = fetch(KindHistoricalForecast, start, end)
			if err == nil && o.Source == HistoryAuto {
				if have := countSurface(b.Hourly); have < len(b.Hourly) {
					// (Part of) this chunk predates the model archive.
					if have == 0 && !probed && end.Before(to) {
						probed = true
						first, cost, perr := c.firstModelDay(end.Add(day), to, fetch)
						if perr != nil {
							return perr
						}
						b.Cost += cost
						modelFrom = first
					}
					var ab *Bundle
					if ab, err = fetch(KindArchive, start, end); err == nil {
						b = fillFromArchive(b, ab)
					}
				}
			}
		}
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
		start = end.Add(day)
		if !start.After(to) {
			if err := sleepCtx(ctx, c.retryBase/4); err != nil {
				return err
			}
		}
	}
	return nil
}

// kindProbe is the internal request kind of firstModelDay's one-variable
// availability probes.
const kindProbe = "probe"

// probeVars is the cheapest request that tells whether the model archive has
// data for a day (1 weighted call).
var probeVars = varSet{surface: surfaceVars[:1]} // temperature_2m

// countSurface returns the number of hours that carry surface data.
func countSurface(hours []HourPoint) int {
	n := 0
	for i := range hours {
		if hours[i].TempC.Valid() {
			n++
		}
	}
	return n
}

// firstModelDay finds the first UTC day in [lo, hi] for which the
// historical-forecast API has surface data, by bisection with one-day,
// one-variable requests (the archive of a model only ever grows forward, so
// availability is monotonic in time). When no day has data it returns the
// day after hi. cost is the weighted calls spent.
func (c *Client) firstModelDay(lo, hi time.Time, fetch func(kind string, start, end time.Time) (*Bundle, error)) (first time.Time, cost float64, err error) {
	has := func(d time.Time) (bool, error) {
		b, err := fetch(kindProbe, d, d)
		if err != nil {
			return false, err
		}
		cost += b.Cost
		return countSurface(b.Hourly) > 0, nil
	}
	ok, err := has(hi)
	if err != nil {
		return time.Time{}, cost, err
	}
	if !ok {
		return hi.Add(day), cost, nil
	}
	// Invariant: hi has data; every day before lo has none.
	for lo.Before(hi) {
		mid := lo.Add(time.Duration(int(hi.Sub(lo)/day)/2) * day)
		ok, err := has(mid)
		if err != nil {
			return time.Time{}, cost, err
		}
		if ok {
			hi = mid
		} else {
			lo = mid.Add(day)
		}
	}
	c.log.Debug("weather: model archive start located", "day", hi.Format("2006-01-02"), "cost", cost)
	return hi, cost, nil
}

// fillFromArchive replaces every hour of the model bundle m that has no
// surface data with the reanalysis hour of the same time from a (which has
// no profile). The result is a itself when m has no data at all.
func fillFromArchive(m, a *Bundle) *Bundle {
	cost := m.Cost + a.Cost
	if countSurface(m.Hourly) == 0 {
		a.Cost = cost
		return a
	}
	j, filled := 0, 0
	for i := range m.Hourly {
		h := &m.Hourly[i]
		if h.TempC.Valid() {
			continue
		}
		for j < len(a.Hourly) && a.Hourly[j].T < h.T {
			j++
		}
		if j < len(a.Hourly) && a.Hourly[j].T == h.T {
			*h = a.Hourly[j]
			filled++
		}
	}
	m.Cost = cost
	if filled > 0 {
		m.Kind = KindMixed
	}
	return m
}

// History is HistoryEach collected into a single Bundle. Kind is the kind of
// the chunks, or KindMixed when the range straddles the start of the model
// archive in HistoryAuto mode.
func (c *Client) History(ctx context.Context, lat, lon float64, from, to time.Time, o HistoryOptions) (*Bundle, error) {
	var out *Bundle
	err := c.HistoryEach(ctx, lat, lon, from, to, o, func(b *Bundle) error {
		if out == nil {
			out = b
			return nil
		}
		if out.Kind != b.Kind {
			out.Kind = KindMixed
		}
		if out.LevelsHPa == nil {
			out.LevelsHPa = b.LevelsHPa
		}
		out.Hourly = append(out.Hourly, b.Hourly...)
		out.Cost += b.Cost
		out.Fetched = b.Fetched
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Elevation returns the terrain elevation in metres above mean sea level at
// a coordinate (Copernicus 90 m digital elevation model), for auto-filling
// the station ground elevation. Costs one API call.
func (c *Client) Elevation(ctx context.Context, lat, lon float64) (float64, error) {
	if !validCoord(lat, lon) {
		return 0, ErrBadCoordinate
	}
	sep := "?"
	if strings.Contains(c.elevationURL, "?") {
		sep = "&"
	}
	u := c.elevationURL + sep + "latitude=" + fmtCoord(lat) + "&longitude=" + fmtCoord(lon)
	body, _, err := c.get(ctx, request{url: u, cost: 1})
	if err != nil {
		return 0, err
	}
	var r struct {
		Elevation []Num  `json:"elevation"`
		Error     bool   `json:"error"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, fmt.Errorf("weather: decoding elevation response: %w", err)
	}
	if r.Error {
		return 0, &APIError{Status: 200, Reason: r.Reason}
	}
	if len(r.Elevation) == 0 || !r.Elevation[0].Valid() {
		return 0, errors.New("weather: no elevation data for this coordinate")
	}
	return r.Elevation[0].Float(), nil
}
