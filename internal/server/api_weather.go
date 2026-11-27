package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"airview/internal/latency"
	"airview/internal/met"
	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/predict"
	"airview/internal/radio"
	"airview/internal/store"
	"airview/internal/tracker"
	"airview/internal/weather"
)

const trackerFarKm = tracker.FarKm

var nan = math.NaN()

func parseFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err == nil && (math.IsNaN(f) || math.IsInf(f, 0)) {
		err = errors.New("not finite")
	}
	return f, err
}

func hourStart(t time.Time) int64 { return t.Unix() / 3600 * 3600 }

// weatherRow returns the stored hourly row for the hour containing t: the
// observation / analysis when there is one, else the forecast. source is
// "observation", "forecast" or "".
func (s *Server) weatherRow(ctx context.Context, t time.Time) (row store.Weather, source string, err error) {
	h := hourStart(t)
	st := s.eng.Store()
	obs, err := st.WeatherObs(ctx, h, h)
	if err != nil {
		return row, "", err
	}
	if len(obs) > 0 {
		return obs[0], "observation", nil
	}
	fc, err := st.ForecastRows(ctx, h, h)
	if err != nil {
		return row, "", err
	}
	if len(fc) > 0 {
		return fc[0], "forecast", nil
	}
	return row, "", nil
}

// latestWeatherRow is weatherRow for now, falling back to the newest
// observation of the last 6 hours.
func (s *Server) latestWeatherRow(ctx context.Context) (store.Weather, string, error) {
	now := time.Now()
	row, src, err := s.weatherRow(ctx, now)
	if err != nil || src != "" {
		return row, src, err
	}
	h := hourStart(now)
	obs, err := s.eng.Store().WeatherObs(ctx, h-6*3600, h)
	if err != nil || len(obs) == 0 {
		return row, "", err
	}
	return obs[len(obs)-1], "observation", nil
}

// profileLevels decodes the profile_json document of a weather row.
func profileLevels(doc string) []weather.Level {
	if doc == "" {
		return nil
	}
	var d struct {
		Levels []weather.Level `json:"levels"`
	}
	if json.Unmarshal([]byte(doc), &d) == nil && len(d.Levels) > 0 {
		return d.Levels
	}
	var bare []weather.Level
	if json.Unmarshal([]byte(doc), &bare) == nil {
		return bare
	}
	return nil
}

// modelLevels returns the pressure-level profile of the weather model for
// the hour containing t and the hour it belongs to (0 = none).
func (s *Server) modelLevels(ctx context.Context, t time.Time) ([]weather.Level, int64) {
	if wx := s.eng.Weather(); wx != nil {
		if b := wx.Latest(); b != nil {
			if hp, ok := b.At(t); ok && hp.HasProfile() {
				return hp.Levels, hp.T
			}
		}
	}
	row, src, err := s.weatherRow(ctx, t)
	if err != nil || src == "" {
		return nil, 0
	}
	if lv := profileLevels(row.ProfileJSON); len(lv) >= 2 {
		return lv, row.T * 1000
	}
	return nil, 0
}

func (s *Server) atmos(row *store.Weather) *predict.Atmos {
	a := predict.AnalyzeWeather(row, s.eng.EffectiveConfig().Receiver.GroundElevM)
	if !a.Valid {
		return nil
	}
	return &a
}

type weatherCurrentReply struct {
	Enabled        bool                 `json:"enabled"` // weather is switched on in the settings
	Located        bool                 `json:"located"` // needs the receiver position
	Status         weather.PollerStatus `json:"status"`
	Attribution    string               `json:"attribution"`
	AttributionURL string               `json:"attributionUrl"`
	Fetched        int64                `json:"fetched"`    // Unix ms of the newest download, 0 = none
	Model          string               `json:"model"`      // weather model
	ElevationM     weather.Num          `json:"elevationM"` // terrain height used by the weather model
	Current        *weather.Current     `json:"current"`    // 15-minute current conditions, null until fetched
	Text           string               `json:"text"`       // current weather in words
	Hour           *store.Weather       `json:"hour"`       // stored row of the current hour (with refractivity columns), null = none
	HourSource     string               `json:"hourSource"` // "observation" | "forecast" | ""
	Atmosphere     *predict.Atmos       `json:"atmosphere"` // radio propagation state derived from hour
}

func apiWeatherCurrent(s *Server, r *http.Request) (any, error) {
	cfg := s.eng.EffectiveConfig()
	out := weatherCurrentReply{
		Enabled: s.eng.Config().Get().Weather.Enabled, Located: cfg.Receiver.Located,
		Attribution: weather.Attribution, AttributionURL: weather.AttributionURL,
		ElevationM: weather.NaN,
	}
	if wx := s.eng.Weather(); wx != nil {
		out.Status = wx.Status()
		if b := wx.Latest(); b != nil {
			out.Fetched, out.Model, out.ElevationM, out.Current = b.Fetched, b.Model, b.ElevationM, b.Current
			if b.Attribution != "" {
				out.Attribution, out.AttributionURL = b.Attribution, b.AttributionURL
			}
			if c := b.Current; c != nil && c.WeatherCode.Valid() {
				out.Text = weather.WeatherCodeText(int(c.WeatherCode.Float()))
			}
		}
	}
	row, src, err := s.latestWeatherRow(r.Context())
	if err != nil {
		return nil, err
	}
	if src != "" {
		out.Hour, out.HourSource = &row, src
		out.Atmosphere = s.atmos(&row)
		if out.Text == "" && !math.IsNaN(row.WeatherCode) {
			out.Text = weather.WeatherCodeText(int(row.WeatherCode))
		}
		row.ProfileJSON = "" // see /api/weather/profile
	}
	return out, nil
}

type weatherHourlyReply struct {
	From           int64           `json:"from"`
	To             int64           `json:"to"`
	Now            int64           `json:"now"`
	Attribution    string          `json:"attribution"`
	AttributionURL string          `json:"attributionUrl"`
	Obs            []store.Weather `json:"obs"`      // observed / analysed hours up to now
	Forecast       []store.Weather `json:"forecast"` // forecast hours from the current hour on
}

// qAround parses a range that may reach into the future: from / to (Unix
// ms), or hours=N back and ahead=N hours forward of now.
func qAround(r *http.Request, defBack, defAhead time.Duration) (from, to time.Time, err error) {
	now := time.Now()
	fromMs, err := qInt64(r, "from", 0)
	if err != nil {
		return
	}
	toMs, err := qInt64(r, "to", 0)
	if err != nil {
		return
	}
	back, err := qFloat(r, "hours", defBack.Hours())
	if err != nil {
		return
	}
	if d, e := qFloat(r, "days", 0); e != nil {
		return from, to, e
	} else if d > 0 {
		back = d * 24
	}
	ahead, err := qFloat(r, "ahead", defAhead.Hours())
	if err != nil {
		return
	}
	if back < 0 || ahead < 0 || back > maxRangeDays*24 || ahead > 16*24 {
		err = badRequest("hours / ahead out of range")
		return
	}
	from = now.Add(-time.Duration(back * float64(time.Hour)))
	to = now.Add(time.Duration(ahead * float64(time.Hour)))
	if fromMs > 0 {
		from = time.UnixMilli(fromMs)
	}
	if toMs > 0 {
		to = time.UnixMilli(toMs)
	}
	if !from.Before(to) {
		err = badRequest("time range is empty: from must be before to")
	}
	return
}

func apiWeatherHourly(s *Server, r *http.Request) (any, error) {
	from, to, err := qAround(r, 48*time.Hour, 72*time.Hour)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	st := s.eng.Store()
	out := weatherHourlyReply{
		From: from.UnixMilli(), To: to.UnixMilli(), Now: now.UnixMilli(),
		Attribution: weather.Attribution, AttributionURL: weather.AttributionURL,
		Obs: []store.Weather{}, Forecast: []store.Weather{},
	}
	h := hourStart(now)
	if from.Unix() <= h {
		rows, err := st.WeatherObs(r.Context(), from.Unix(), min(to.Unix(), h))
		if err != nil {
			return nil, err
		}
		if rows != nil {
			out.Obs = rows
		}
	}
	if to.Unix() >= h {
		rows, err := st.ForecastRows(r.Context(), max(from.Unix(), h), to.Unix())
		if err != nil {
			return nil, err
		}
		if rows != nil {
			out.Forecast = rows
		}
	}
	if !qBool(r, "profile", false) {
		for i := range out.Obs {
			out.Obs[i].ProfileJSON = ""
		}
		for i := range out.Forecast {
			out.Forecast[i].ProfileJSON = ""
		}
	}
	return out, nil
}

type refractPoint struct {
	HeightM num `json:"heightM"` // m AMSL
	N       num `json:"n"`       // refractivity, N-units
	M       num `json:"m"`       // modified refractivity, M-units
}

type refractLayer struct {
	BaseM num    `json:"baseM"`
	TopM  num    `json:"topM"`
	DNDH  num    `json:"dNdh"` // N-units per km
	DMDH  num    `json:"dMdh"` // M-units per km, negative = trapping layer
	Class string `json:"class"`
}

type weatherProfileReply struct {
	T          int64           `json:"t"`      // hour of the profile, Unix ms (0 = none found)
	Source     string          `json:"source"` // "observation" | "forecast" | ""
	Levels     []weather.Level `json:"levels"` // pressure levels, ground up
	Surface    *store.Weather  `json:"surface"`
	Atmosphere *predict.Atmos  `json:"atmosphere"`
	// Refractivity analysis of the levels (null without a profile).
	Points  []refractPoint `json:"points"`
	Layers  []refractLayer `json:"layers"`
	Ducts   []radio.Duct   `json:"ducts"`
	Summary string         `json:"summary"`
}

func apiWeatherProfile(s *Server, r *http.Request) (any, error) {
	tMs, err := qInt64(r, "t", 0)
	if err != nil {
		return nil, err
	}
	t := time.Now()
	if tMs > 0 {
		t = time.UnixMilli(tMs)
	}
	out := weatherProfileReply{Levels: []weather.Level{}, Points: []refractPoint{}, Layers: []refractLayer{}, Ducts: []radio.Duct{}}
	var row store.Weather
	var src string
	if tMs > 0 {
		row, src, err = s.weatherRow(r.Context(), t)
	} else {
		row, src, err = s.latestWeatherRow(r.Context())
	}
	if err != nil {
		return nil, err
	}
	if src == "" {
		return out, nil
	}
	out.T, out.Source = row.T*1000, src
	out.Atmosphere = s.atmos(&row)
	if out.Atmosphere != nil {
		out.Summary = out.Atmosphere.Summary
	}
	levels := profileLevels(row.ProfileJSON)
	row.ProfileJSON = ""
	out.Surface = &row
	if len(levels) == 0 {
		return out, nil
	}
	out.Levels = levels
	var rl []radio.Level
	for _, l := range levels {
		if l.TempC.Valid() && l.RH.Valid() && l.HeightM.Valid() {
			rl = append(rl, radio.Level{HeightM: l.HeightM.Float(), PressureHPa: l.HPa, TempC: l.TempC.Float(), RH: l.RH.Float()})
		}
	}
	if p, ok := radio.AnalyzeProfile(rl); ok {
		for _, pt := range p.Points {
			out.Points = append(out.Points, refractPoint{rnd(pt.HeightM, 0), rnd(pt.N, 1), rnd(pt.M, 1)})
		}
		for _, l := range p.Layers {
			out.Layers = append(out.Layers, refractLayer{rnd(l.BaseM, 0), rnd(l.TopM, 0), rnd(l.DNDH, 1), rnd(l.DMDH, 1), l.Class})
		}
		if p.Ducts != nil {
			out.Ducts = p.Ducts
		}
	}
	return out, nil
}

type radarReply struct {
	Enabled   bool `json:"enabled"`   // weather.radarOverlay is on in the settings
	Available bool `json:"available"` // a frame index has been downloaded
	weather.Radar
}

func apiWeatherRadar(s *Server, r *http.Request) (any, error) {
	cfg := s.eng.Config().Get().Weather
	out := radarReply{
		Enabled: cfg.Enabled && cfg.RadarOverlay,
		Radar: weather.Radar{
			Past: []weather.RadarFrame{}, Nowcast: []weather.RadarFrame{},
			TileSize: weather.RadarTileSize, MaxZoom: weather.RadarMaxZoom,
			Attribution: weather.RadarAttribution, AttributionURL: weather.RadarAttributionURL,
		},
	}
	if wx := s.eng.Weather(); wx != nil {
		if rd := wx.Radar(); rd != nil {
			out.Available = true
			out.Radar = *rd
			if out.Past == nil {
				out.Past = []weather.RadarFrame{}
			}
			if out.Nowcast == nil {
				out.Nowcast = []weather.RadarFrame{}
			}
		}
	}
	return out, nil
}

func apiWeatherImpact(s *Server, r *http.Request) (any, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().WeatherImpact(r.Context(), rg, ant)
}

type ductHour struct {
	T      int64  `json:"t"`
	Source string `json:"source"` // "observation" | "forecast"
	predict.Atmos
}

type ductingReply struct {
	From         int64               `json:"from"`
	To           int64               `json:"to"`
	Now          int64               `json:"now"`
	StationElevM float64             `json:"stationElevM"`
	Current      *predict.Atmos      `json:"current"`  // state of the current hour, null without weather data
	Hours        []ductHour          `json:"hours"`    // observed hours, then forecast hours
	Aircraft     met.RadioInversions `json:"aircraft"` // inversions measured by aircraft right now
	DuctHours    int                 `json:"ductHours"`
}

func apiWeatherDucting(s *Server, r *http.Request) (any, error) {
	from, to, err := qAround(r, 48*time.Hour, 72*time.Hour)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	now := time.Now()
	h := hourStart(now)
	elev := s.eng.EffectiveConfig().Receiver.GroundElevM
	out := ductingReply{
		From: from.UnixMilli(), To: to.UnixMilli(), Now: now.UnixMilli(), StationElevM: elev,
		Hours: []ductHour{},
	}
	add := func(rows []store.Weather, src string) {
		for i := range rows {
			a := predict.AnalyzeWeather(&rows[i], elev)
			if !a.Valid {
				continue
			}
			out.Hours = append(out.Hours, ductHour{T: rows[i].T * 1000, Source: src, Atmos: a})
			if a.Ducting1090 {
				out.DuctHours++
			}
			if rows[i].T == h {
				cp := a
				out.Current = &cp
			}
		}
	}
	st := s.eng.Store()
	if from.Unix() <= h {
		rows, err := st.WeatherObs(ctx, from.Unix(), min(to.Unix(), h))
		if err != nil {
			return nil, err
		}
		add(rows, "observation")
	}
	if to.Unix() > h {
		lo := h + 3600
		if out.Current == nil {
			lo = h // no analysis of the current hour yet: take the forecast
		}
		rows, err := st.ForecastRows(ctx, max(from.Unix(), lo), to.Unix())
		if err != nil {
			return nil, err
		}
		add(rows, "forecast")
	}
	levels, _ := s.modelLevels(ctx, now)
	out.Aircraft = s.eng.Met().RadioLayers(now, levels)
	if out.Aircraft.Layers == nil {
		out.Aircraft.Layers = []met.RadioLayer{}
	}
	return out, nil
}

func apiWeatherRefresh(s *Server, r *http.Request) (any, error) {
	wx := s.eng.Weather()
	if wx == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "weather is not available")
	}
	b, err := wx.Refresh(r.Context())
	switch {
	case errors.Is(err, weather.ErrDisabled):
		return nil, errStatus(http.StatusConflict, "weather is disabled in the settings")
	case errors.Is(err, weather.ErrNotLocated):
		return nil, errStatus(http.StatusConflict, "the receiver position is not set")
	case errors.Is(err, weather.ErrBudget):
		return nil, errStatus(http.StatusTooManyRequests, "the Open-Meteo request budget is exhausted")
	case err != nil:
		return nil, errStatus(http.StatusBadGateway, "weather download failed: %v", err)
	}
	s.cache.invalidate()
	return map[string]any{"ok": true, "fetched": b.Fetched, "status": wx.Status()}, nil
}

// ---------------------------------------------------------------------------
// Aircraft-derived meteorology
// ---------------------------------------------------------------------------

func fixProfile(p met.Profile) met.Profile {
	if p.Levels == nil {
		p.Levels = []met.Level{}
	}
	if p.Lapse == nil {
		p.Lapse = []met.LapseLayer{}
	}
	if p.Inversions == nil {
		p.Inversions = []met.Inversion{}
	}
	if p.WindsAloft == nil {
		p.WindsAloft = []met.WindAloft{}
	}
	return p
}

func apiMetProfile(s *Server, r *http.Request) (any, error) {
	tMs, err := qInt64(r, "t", 0)
	if err != nil {
		return nil, err
	}
	if tMs <= 0 {
		return fixProfile(s.eng.Met().Current(time.Time{})), nil
	}
	p, err := s.eng.Met().HourProfile(r.Context(), time.UnixMilli(tMs))
	if err != nil {
		return nil, err
	}
	return fixProfile(p), nil
}

func apiMetHistory(s *Server, r *http.Request) (any, error) {
	from, to, err := qSpanDefault(r, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	th, err := s.eng.Met().History(r.Context(), from, to)
	if err != nil {
		return nil, err
	}
	if th.Hours == nil {
		th.Hours = []int64{}
	}
	if th.Cells == nil {
		th.Cells = []met.HistCell{}
	}
	return th, nil
}

type metObsReply struct {
	Now          int64     `json:"now"`
	MaxAgeS      int       `json:"maxAgeS"`
	Count        int       `json:"count"`
	Observations []met.Obs `json:"observations"`
}

func apiMetObservations(s *Server, r *http.Request) (any, error) {
	age, err := qInt(r, "maxAge", 900, 10, 7200)
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 1000, 1, 5000)
	if err != nil {
		return nil, err
	}
	obs := s.eng.Met().Latest(time.Time{}, time.Duration(age)*time.Second, limit, qBool(r, "perAircraft", true))
	if obs == nil {
		obs = []met.Obs{}
	}
	return metObsReply{Now: time.Now().UnixMilli(), MaxAgeS: age, Count: len(obs), Observations: obs}, nil
}

type metWindsReply struct {
	T               int64           `json:"t"`
	From            int64           `json:"from"`
	WindowMin       float64         `json:"windowMin"`
	NWind           int             `json:"nWind"`
	NTemp           int             `json:"nTemp"`
	WindsAloft      []met.WindAloft `json:"windsAloft"` // standard flight levels with data
	MaxWind         *met.MaxWind    `json:"maxWind"`
	MaxShear        *met.Shear      `json:"maxShear"`
	FreezingLevelFt *float64        `json:"freezingLevelFt"`
	Tropopause      *met.Tropopause `json:"tropopause"`
}

func apiMetWinds(s *Server, r *http.Request) (any, error) {
	p := fixProfile(s.eng.Met().Current(time.Time{}))
	return metWindsReply{
		T: p.T, From: p.From, WindowMin: p.WindowMin, NWind: p.NWind, NTemp: p.NTemp,
		WindsAloft: p.WindsAloft, MaxWind: p.MaxWind, MaxShear: p.MaxShear,
		FreezingLevelFt: p.FreezingLevelFt, Tropopause: p.Tropopause,
	}, nil
}

type metCompareReply struct {
	T              int64               `json:"t"`
	ModelAvailable bool                `json:"modelAvailable"` // a weather-model profile exists for this hour
	ModelT         int64               `json:"modelT"`         // hour of the model profile, Unix ms
	Attribution    string              `json:"attribution"`
	Comparison     met.Comparison      `json:"comparison"` // aircraft minus model
	Radio          met.RadioInversions `json:"radio"`
}

func apiMetCompare(s *Server, r *http.Request) (any, error) {
	now := time.Now()
	levels, mt := s.modelLevels(r.Context(), now)
	c := s.eng.Met()
	out := metCompareReply{
		T: now.UnixMilli(), ModelAvailable: len(levels) > 0, ModelT: mt, Attribution: weather.Attribution,
		Comparison: c.CompareModel(now, levels), Radio: c.RadioLayers(now, levels),
	}
	if out.Comparison.Levels == nil {
		out.Comparison.Levels = []met.ModelDiff{}
	}
	if out.Radio.Layers == nil {
		out.Radio.Layers = []met.RadioLayer{}
	}
	return out, nil
}

func apiMetStats(s *Server, r *http.Request) (any, error) {
	limit, err := qInt(r, "limit", 50, 1, 500)
	if err != nil {
		return nil, err
	}
	b := s.eng.Met().HeadingBiases(limit)
	if b == nil {
		b = []met.HeadingBias{}
	}
	return map[string]any{"stats": s.eng.Met().Stats(), "biases": b}, nil
}

// ---------------------------------------------------------------------------
// Forecasts
// ---------------------------------------------------------------------------

func apiForecastTraffic(s *Server, r *http.Request) (any, error) {
	return s.eng.Forecaster().Traffic(r.Context())
}

func apiForecastReception(s *Server, r *http.Request) (any, error) {
	return s.eng.Forecaster().Reception(r.Context())
}

func apiForecastWeather(s *Server, r *http.Request) (any, error) {
	return s.eng.Forecaster().Weather(r.Context())
}

func apiForecastVerification(s *Server, r *http.Request) (any, error) {
	return s.eng.Forecaster().Verification(r.Context())
}

func apiForecastNowcast(s *Server, r *http.Request) (any, error) {
	return s.eng.Forecaster().Nowcast(r.Context())
}

func apiForecastStatus(s *Server, r *http.Request) (any, error) {
	return s.eng.Forecaster().Status(), nil
}

type outlookTraffic struct {
	Level      string                `json:"level"`
	Confidence string                `json:"confidence"`
	Busiest    []predict.BusyHour    `json:"busiest"`
	Quietest   *predict.BusyHour     `json:"quietest"`
	Next       []predict.TrafficHour `json:"next"` // the next 12 hours
}

type outlookReception struct {
	Level        string                  `json:"level"`
	Confidence   string                  `json:"confidence"`
	TypicalP95Km predict.Num             `json:"typicalP95Km"`
	FarKm        predict.Num             `json:"farKm"`
	Best         *predict.ReceptionHour  `json:"best"`
	Bands        []predict.BandOutlook   `json:"bands"`
	Next         []predict.ReceptionHour `json:"next"` // the next 12 hours
}

type outlookWeather struct {
	Level      string                    `json:"level"`
	Confidence string                    `json:"confidence"`
	Current    *predict.WeatherNow       `json:"current"`
	Zambretti  *predict.ZambrettiOutlook `json:"zambretti"`
	Patterns   []predict.Pattern         `json:"patterns"`
	Next       []predict.WeatherHour     `json:"next"` // the next 12 hours
}

type outlookReply struct {
	Generated int64            `json:"generated"`
	Status    predict.Status   `json:"status"`
	Nowcast   *predict.Nowcast `json:"nowcast"`
	Traffic   outlookTraffic   `json:"traffic"`
	Reception outlookReception `json:"reception"`
	Weather   outlookWeather   `json:"weather"`
}

func head[T any](v []T, n int) []T {
	if len(v) > n {
		v = v[:n]
	}
	if v == nil {
		v = []T{}
	}
	return v
}

// apiForecastOutlook is the digest of all products for a home page.
func apiForecastOutlook(s *Server, r *http.Request) (any, error) {
	ctx := r.Context()
	f := s.eng.Forecaster()
	out := outlookReply{Generated: time.Now().UnixMilli()}
	nc, err := f.Nowcast(ctx)
	if err != nil {
		return nil, err
	}
	out.Nowcast = nc
	tf, err := f.Traffic(ctx)
	if err != nil {
		return nil, err
	}
	out.Traffic = outlookTraffic{
		Level: tf.Level, Confidence: tf.Confidence, Busiest: head(tf.Busiest, 5), Quietest: tf.Quietest,
		Next: head(tf.Hours, 12),
	}
	rf, err := f.Reception(ctx)
	if err != nil {
		return nil, err
	}
	out.Reception = outlookReception{
		Level: rf.Level, Confidence: rf.Confidence, TypicalP95Km: rf.TypicalP95Km, FarKm: rf.FarKm,
		Best: rf.Best, Bands: head(rf.Bands, 16), Next: head(rf.Hours, 12),
	}
	wo, err := f.Weather(ctx)
	if err != nil {
		return nil, err
	}
	out.Weather = outlookWeather{
		Level: wo.Level, Confidence: wo.Confidence, Current: wo.Current, Zambretti: wo.Zambretti,
		Patterns: head(wo.Patterns, 32), Next: head(wo.Hours, 12),
	}
	out.Status = f.Status()
	return out, nil
}

func apiForecastRefit(s *Server, r *http.Request) (any, error) {
	if err := s.eng.Forecaster().Refit(r.Context()); err != nil {
		return nil, err
	}
	s.cache.invalidate()
	return map[string]any{"ok": true, "status": s.eng.Forecaster().Status()}, nil
}

// ---------------------------------------------------------------------------
// Timing
// ---------------------------------------------------------------------------

type ntpReply struct {
	Config model.NTPConfig `json:"config"`
	Status model.NTPStatus `json:"status"`
	Latest *ntp.Sample     `json:"latest"` // newest measurement, null = none yet
	Hour   ntp.Stats       `json:"hour"`   // statistics of the last hour
	Day    ntp.Stats       `json:"day"`    // last 24 hours
	All    ntp.Stats       `json:"all"`    // everything in memory (about 6 days)
}

func apiTimingNTP(s *Server, r *http.Request) (any, error) {
	sm := s.eng.NTP()
	out := ntpReply{
		Config: sm.Config(), Status: sm.Status(),
		Hour: sm.Stats(time.Hour), Day: sm.Stats(24 * time.Hour), All: sm.Stats(0),
	}
	if l, ok := sm.Latest(); ok {
		out.Latest = &l
	}
	return out, nil
}

type ntpHistoryReply struct {
	From    int64        `json:"from"`
	To      int64        `json:"to"`
	Source  string       `json:"source"` // "memory" (full samples) | "database" (offset, delay, stratum only)
	Total   int          `json:"total"`  // samples in the range before thinning
	Samples []ntp.Sample `json:"samples"`
}

func apiTimingHistory(s *Server, r *http.Request) (any, error) {
	from, to, err := qSpanDefault(r, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 3000, 10, 20000)
	if err != nil {
		return nil, err
	}
	out := ntpHistoryReply{From: from.UnixMilli(), To: to.UnixMilli(), Source: "memory"}
	var samples []ntp.Sample
	for _, sm := range s.eng.NTP().History(from) {
		if sm.T <= out.To {
			samples = append(samples, sm)
		}
	}
	// The ring holds about six days; older ranges come from the database.
	if len(samples) == 0 || samples[0].T-out.From > int64(2*time.Hour/time.Millisecond) {
		rows, err := s.eng.Store().NTPSamples(r.Context(), from.Unix(), to.Unix(), 200000)
		if err != nil {
			return nil, err
		}
		if len(rows) > len(samples) {
			out.Source = "database"
			samples = samples[:0]
			for _, row := range rows {
				if math.IsNaN(row.OffsetMs) {
					continue
				}
				d := row.DelayMs
				if math.IsNaN(d) {
					d = 0
				}
				samples = append(samples, ntp.Sample{
					T: row.T * 1000, Server: row.Server, OffsetMs: row.OffsetMs, DelayMs: d,
					Stratum: row.Stratum, SmoothedMs: row.OffsetMs,
				})
			}
		}
	}
	out.Total = len(samples)
	if len(samples) > limit {
		step := float64(len(samples)) / float64(limit)
		thin := make([]ntp.Sample, 0, limit)
		for i := 0; i < limit; i++ {
			thin = append(thin, samples[int(float64(i)*step)])
		}
		thin[len(thin)-1] = samples[len(samples)-1]
		samples = thin
	}
	if samples == nil {
		samples = []ntp.Sample{}
	}
	out.Samples = samples
	return out, nil
}

type latencyReply struct {
	Now           int64                 `json:"now"`
	ClockOffsetMs float64               `json:"clockOffsetMs"` // NTP correction in use (true UTC - host clock)
	ClockSynced   bool                  `json:"clockSynced"`
	Feeds         []tracker.FeedLatency `json:"feeds"` // only feeds with GPS timestamps
	// Chain is the latency-chain summary (internal/latency): stages,
	// counters, per-feed clock filter, findings. Null when unavailable.
	Chain *latency.Summary `json:"chain"`
	// Histograms are the stage histograms of the summary window.
	Histograms []latency.Histogram `json:"histograms"`
}

func apiTimingLatency(s *Server, r *http.Request) (any, error) {
	fl := s.eng.Tracker().FeedLatency()
	if fl == nil {
		fl = []tracker.FeedLatency{}
	}
	for i := range fl {
		if fl[i].Hist == nil {
			fl[i].Hist = []tracker.LatencyBin{}
		}
		if fl[i].History == nil {
			fl[i].History = []tracker.LatencyPoint{}
		}
	}
	clk := s.eng.NTP()
	out := latencyReply{
		Now: time.Now().UnixMilli(), ClockOffsetMs: float64(clk.Offset()) / 1e6, ClockSynced: clk.Synced(),
		Feeds: fl, Histograms: []latency.Histogram{},
	}
	if a := s.eng.Latency(); a != nil {
		sum := a.Summary()
		out.Chain = &sum
		if h := a.Histograms(); h != nil {
			out.Histograms = h
		}
	}
	return out, nil
}

type receiverFeed struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Connected bool   `json:"connected"`
	GPSTime   bool   `json:"gpsTime"` // the feed delivers GPS timestamps
	Info      string `json:"info"`    // last receiver status text
	LastFrame int64  `json:"lastFrame"`
}

type receiverStatusRow struct {
	T      int64           `json:"t"`
	Feed   string          `json:"feed"`
	Status json.RawMessage `json:"status"` // raw receiver status payload bytes (Radarcape type '4'); sampled once per feed and minute
}

type receiverReply struct {
	Now          int64               `json:"now"`
	Model        string              `json:"model"` // configured receiver model
	Feeds        []receiverFeed      `json:"feeds"`
	ReceiverGPS  *model.ReceiverGPS  `json:"receiverGps"`  // position reported by the receiver, null = none
	StatusFrames uint64              `json:"statusFrames"` // receiver status frames seen since start
	History      []receiverStatusRow `json:"history"`      // stored status frames, newest first
}

func apiTimingReceiver(s *Server, r *http.Request) (any, error) {
	out := receiverReply{
		Now: time.Now().UnixMilli(), Model: s.eng.EffectiveConfig().Receiver.Model,
		Feeds: []receiverFeed{}, ReceiverGPS: s.eng.ReceiverGPS(),
		StatusFrames: s.eng.Tracker().Counters().Status, History: []receiverStatusRow{},
	}
	for _, f := range s.eng.FeedStatus() {
		out.Feeds = append(out.Feeds, receiverFeed{
			ID: f.ID, Name: f.Name, Type: f.Type, Connected: f.Connected, GPSTime: f.GPSTime,
			Info: f.Info, LastFrame: f.LastFrame,
		})
	}
	limit, err := qInt(r, "limit", 20, 1, 500)
	if err != nil {
		return nil, err
	}
	rows, err := s.eng.Store().DB().QueryContext(r.Context(),
		`SELECT t, feed, json FROM receiver_status ORDER BY t DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row receiverStatusRow
		var doc string
		if err := rows.Scan(&row.T, &row.Feed, &doc); err != nil {
			return nil, err
		}
		row.T *= 1000
		if json.Valid([]byte(doc)) {
			row.Status = json.RawMessage(doc)
		} else {
			row.Status, _ = json.Marshal(doc)
		}
		out.History = append(out.History, row)
	}
	return out, rows.Err()
}
