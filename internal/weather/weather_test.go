package weather

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var forecastVars = varSet{current: surfaceVars, surface: surfaceVars, extra: forecastExtraVars, levels: DefaultLevels}

// recorder is an httptest server that logs every request.
type recorder struct {
	srv *httptest.Server
	mu  sync.Mutex
	log []*http.Request
}

func newRecorder(t testing.TB, h func(w http.ResponseWriter, r *http.Request)) *recorder {
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.log = append(rec.log, r.Clone(context.Background()))
		rec.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, q := range r.log {
		if path == "" || q.URL.Path == path {
			n++
		}
	}
	return n
}

func (r *recorder) request(i int) *http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 {
		i += len(r.log)
	}
	return r.log[i]
}

// testClient points every endpoint at the recorder and removes waits.
func testClient(rec *recorder, o Options) *Client {
	o.ForecastURL = rec.srv.URL + "/forecast"
	o.HistoricalForecastURL = rec.srv.URL + "/hf"
	o.ArchiveURL = rec.srv.URL + "/archive"
	o.ElevationURL = rec.srv.URL + "/elevation"
	o.RainViewerURL = rec.srv.URL + "/radar"
	if o.Budget == (Budget{}) {
		o.Budget = Budget{PerMinute: 1e9, PerHour: 1e9, PerDay: 1e9}
	}
	c := NewClient(o)
	c.retryBase = time.Millisecond
	return c
}

func list(q url.Values, key string) []string {
	v := q.Get(key)
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func TestNumJSON(t *testing.T) {
	type rec struct {
		A, B, C Num
	}
	b, err := json.Marshal(rec{1.5, NaN, Num(math.Inf(1))})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"A":1.5,"B":null,"C":null}` {
		t.Fatalf("marshal = %s", b)
	}
	var back rec
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.A != 1.5 || back.B.Valid() || back.C.Valid() {
		t.Fatalf("unmarshal = %+v", back)
	}
	for in, want := range map[Num]string{0: "0", -0.25: "-0.25", 1013.2: "1013.2", 1e-9: "1e-09", 40060: "40060", 1e22: "1e+22"} {
		got, _ := json.Marshal(in)
		if string(got) != want {
			t.Errorf("Marshal(%v) = %s, want %s", float64(in), got, want)
		}
		var rt Num
		if err := json.Unmarshal(got, &rt); err != nil || rt != in {
			t.Errorf("round trip of %s = %v, %v", got, rt, err)
		}
	}
	var n Num
	if err := json.Unmarshal([]byte(`"x"`), &n); err == nil {
		t.Error("string accepted as Num")
	}
	if NaN.Ptr() != nil || *Num(2).Ptr() != 2 || NaN.Or(7) != 7 || Num(3).Or(7) != 3 {
		t.Error("Ptr/Or helpers")
	}
	// A plain float64 NaN would make encoding/json fail; Num must not.
	if _, err := json.Marshal(struct{ V []Num }{[]Num{NaN, 1}}); err != nil {
		t.Errorf("slice with NaN: %v", err)
	}
}

// allNumsNaN walks v and reports the first Num field that is not NaN.
func allNumsNaN(v reflect.Value, path string) string {
	numT := reflect.TypeOf(Num(0))
	switch {
	case v.Type() == numT:
		if !math.IsNaN(v.Float()) {
			return path
		}
	case v.Kind() == reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if bad := allNumsNaN(v.Field(i), path+"."+v.Type().Field(i).Name); bad != "" {
				return bad
			}
		}
	}
	return ""
}

func TestMissingValueTemplatesCoverEveryField(t *testing.T) {
	// Every Num field must be reachable through a variable table; otherwise a
	// field nobody fills would silently read as 0 instead of "missing".
	if bad := allNumsNaN(reflect.ValueOf(nanHour), "HourPoint"); bad != "" {
		t.Errorf("%s is not NaN in the empty HourPoint template", bad)
	}
	if bad := allNumsNaN(reflect.ValueOf(nanSurface), "Surface"); bad != "" {
		t.Errorf("%s is not NaN in the empty Surface template", bad)
	}
	if bad := allNumsNaN(reflect.ValueOf(nanLevel), "Level"); bad != "" {
		t.Errorf("%s is not NaN in the empty Level template", bad)
	}
	cur, hourly := forecastVars.names(nil)
	if len(cur) != 21 || len(hourly) != 109 {
		t.Errorf("default request has %d current + %d hourly variables, want 21 + 109", len(cur), len(hourly))
	}
	seen := map[string]bool{}
	for _, n := range hourly {
		if seen[n] {
			t.Errorf("duplicate hourly variable %s", n)
		}
		seen[n] = true
	}
	if got := estimateCost(len(cur)+len(hourly), 9); math.Abs(got-13.0) > 1e-9 {
		t.Errorf("default poll cost = %v, want 13.0", got)
	}
	// Values observed in the X-RateLimit-Cost header of real requests.
	if got := estimateCost(131, 9); math.Abs(got-13.1) > 1e-9 {
		t.Errorf("cost(131 vars, 9 days) = %v, want 13.1", got)
	}
	if got := estimateCost(109, 23); math.Abs(got-17.907143) > 1e-5 {
		t.Errorf("cost(109 vars, 23 days) = %v, want 17.907143", got)
	}
	if got := estimateCost(1, 1); got != 1 {
		t.Errorf("cost(1,1) = %v", got)
	}
	// More header observations (variables, days → cost): the time factor only
	// scales the variable factor, it is not a floor of its own.
	for _, c := range []struct {
		vars, days int
		want       float64
	}{
		{45, 2, 4.5}, {20, 7, 2}, {20, 9, 2}, {20, 15, 2.142857}, {20, 28, 4}, {20, 56, 8},
		{10, 28, 2}, {5, 56, 2}, {1, 28, 1}, {1, 56, 1}, {1, 108, 1},
	} {
		if got := estimateCost(c.vars, c.days); math.Abs(got-c.want) > 1e-5 {
			t.Errorf("cost(%d vars, %d days) = %v, want %v", c.vars, c.days, got, c.want)
		}
	}
}

// Mapping from Open-Meteo variable names to the JSON field names of our DTOs,
// written out independently of the setter tables to catch cross-wiring.
var jsonNames = map[string]string{
	"temperature_2m": "tempC", "relative_humidity_2m": "rh", "dew_point_2m": "dewPointC",
	"apparent_temperature": "apparentC", "precipitation": "precipMm", "rain": "rainMm",
	"showers": "showersMm", "snowfall": "snowCm", "weather_code": "weatherCode",
	"cloud_cover": "cloud", "cloud_cover_low": "cloudLow", "cloud_cover_mid": "cloudMid",
	"cloud_cover_high": "cloudHigh", "pressure_msl": "pressureMsl", "surface_pressure": "surfacePressure",
	"wind_speed_10m": "windKmh", "wind_direction_10m": "windDir", "wind_gusts_10m": "gustKmh",
	"visibility": "visibilityM", "is_day": "isDay", "cape": "cape",
	"precipitation_probability": "precipProb", "boundary_layer_height": "boundaryLayerM",
	"lifted_index": "liftedIndex", "convective_inhibition": "cin", "freezing_level_height": "freezingLevelM",
	"shortwave_radiation": "shortwaveWm2", "soil_temperature_0cm": "soilTempC",
	"total_column_integrated_water_vapour": "pwatMm", "snow_depth": "snowDepthM",
	"temperature_80m": "temp80C", "temperature_120m": "temp120C", "temperature_180m": "temp180C",
	"wind_speed_80m": "wind80Kmh", "wind_speed_120m": "wind120Kmh", "wind_speed_180m": "wind180Kmh",
	"wind_direction_80m": "windDir80", "wind_direction_120m": "windDir120", "wind_direction_180m": "windDir180",
}

var levelJSONNames = map[string]string{
	"temperature": "t", "relative_humidity": "rh", "geopotential_height": "z",
	"wind_speed": "ws", "wind_direction": "wd",
}

func TestParseForecastFixture(t *testing.T) {
	raw := fixture(t, "forecast.json")
	b, err := parseBundle(raw, forecastVars, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Hourly) != 24 {
		t.Fatalf("hours = %d, want 24", len(b.Hourly))
	}
	if b.Hourly[0].T != 1794355200000 || b.Hourly[23].T != 1794438000000 {
		t.Errorf("hour range %d .. %d", b.Hourly[0].T, b.Hourly[23].T)
	}
	if b.Source != "open-meteo" || b.Attribution == "" || b.ElevationM != 109 || math.Abs(b.GridLat.Float()-49.997505) > 1e-9 {
		t.Errorf("meta: %+v", *b)
	}
	c := b.Current
	if c == nil {
		t.Fatal("no current block")
	}
	if c.T != 1794439800000 || c.IntervalSec != 900 || c.TempC != 7.5 || c.RH != 100 || c.WeatherCode != 45 ||
		c.VisibilityM != 820 || c.IsDay != 0 || c.PressureMSL != 1000.8 || c.SurfacePressure != 987.6 ||
		c.WindDir != 243 || c.WindKmh != 2.4 || c.GustKmh != 5 || c.CloudLow != 91 || c.CloudMid != 79 || c.CAPE != 0 {
		t.Errorf("current = %+v", *c)
	}
	if got := WeatherCodeText(int(c.WeatherCode)); got != "Fog" {
		t.Errorf("weather code text = %q", got)
	}
	h := b.Hourly[5]
	if h.TempC != 5.4 || len(h.Levels) != 14 || h.Levels[5].HPa != 850 || h.Levels[5].HeightM != 1378 {
		t.Errorf("hour 5: temp %v levels %+v", h.TempC, h.Levels)
	}
	if b.Hourly[0].Levels[0].HPa != 1000 || b.Hourly[0].Levels[0].TempC != 5.3 ||
		b.Hourly[23].Levels[13].HPa != 200 || b.Hourly[23].Levels[13].WindDir != 273 {
		t.Errorf("profile corners wrong")
	}
	if b.Hourly[12].BoundaryLayerM != 35 || b.Hourly[3].PrecipProb != 68 {
		t.Errorf("extras wrong: %v %v", b.Hourly[12].BoundaryLayerM, b.Hourly[3].PrecipProb)
	}
	if !h.HasProfile() || !reflect.DeepEqual(b.LevelsHPa, DefaultLevels) {
		t.Error("profile metadata wrong")
	}
	// Fields that only the reanalysis archive provides stay missing.
	if h.Wind100Kmh.Valid() || h.WindDir100.Valid() {
		t.Error("100 m wind should be missing in forecast data")
	}

	// Lookup helpers.
	if i := b.Index(time.UnixMilli(b.Hourly[7].T + 59*60*1000)); i != 7 {
		t.Errorf("Index = %d, want 7", i)
	}
	if i := b.Index(time.UnixMilli(b.Hourly[23].T + 3600*1000)); i != -1 {
		t.Errorf("Index past the end = %d", i)
	}
	if _, ok := b.At(time.UnixMilli(b.Hourly[0].T - 1)); ok {
		t.Error("At before the start succeeded")
	}
	past, future := b.Split(time.UnixMilli(b.Hourly[9].T + 5))
	if len(past) != 10 || len(future) != 14 {
		t.Errorf("Split = %d / %d", len(past), len(future))
	}

	// Cross-check every variable of every hour against an independent decode
	// of the fixture, going through our JSON encoding.
	enc, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var src struct {
		Hourly map[string][]*float64 `json:"hourly"`
	}
	var out struct {
		Hourly []map[string]json.RawMessage `json:"hourly"`
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(enc, &out); err != nil {
		t.Fatal(err)
	}
	num := func(raw json.RawMessage) *float64 {
		var p *float64
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("bad number %s", raw)
		}
		return p
	}
	same := func(a, b *float64) bool {
		if a == nil || b == nil {
			return a == b
		}
		return *a == *b
	}
	checked := 0
	for om, field := range jsonNames {
		col := src.Hourly[om]
		if len(col) != 24 {
			t.Fatalf("fixture lacks column %s", om)
		}
		for i := range col {
			if got := num(out.Hourly[i][field]); !same(got, col[i]) {
				t.Fatalf("hour %d %s (%s): got %v want %v", i, field, om, got, col[i])
			}
			checked++
		}
	}
	for i := range out.Hourly {
		var levels []map[string]json.RawMessage
		if err := json.Unmarshal(out.Hourly[i]["levels"], &levels); err != nil || len(levels) != 14 {
			t.Fatalf("hour %d levels: %v", i, err)
		}
		for j, hpa := range DefaultLevels {
			if got := num(levels[j]["hpa"]); got == nil || int(*got) != hpa {
				t.Fatalf("hour %d level %d hpa = %v", i, j, got)
			}
			for prefix, field := range levelJSONNames {
				want := src.Hourly[levelName(prefix, hpa)][i]
				if got := num(levels[j][field]); !same(got, want) {
					t.Fatalf("hour %d %d hPa %s: got %v want %v", i, hpa, field, got, want)
				}
				checked++
			}
		}
	}
	if checked != 24*(39+70) {
		t.Errorf("checked %d values, want %d", checked, 24*(39+70))
	}

	// The bundle survives a JSON round trip unchanged (including NaN fields).
	var back Bundle
	if err := json.Unmarshal(enc, &back); err != nil {
		t.Fatal(err)
	}
	enc2, err := json.Marshal(&back)
	if err != nil {
		t.Fatal(err)
	}
	if string(enc) != string(enc2) {
		t.Error("JSON round trip changed the bundle")
	}
	if back.Hourly[0].Wind100Kmh.Valid() {
		t.Error("missing value did not survive the JSON round trip as NaN")
	}
}

func TestParseNullsAndMissingColumns(t *testing.T) {
	body := `{"latitude":50,"longitude":8,"elevation":null,"hourly":{
		"time":[3600,7200,10800],
		"temperature_2m":[1.5,null,3.5],
		"cape":[null,null,null],
		"relative_humidity_2m":[50,60],
		"weather_code":["rain","snow","fog"],
		"temperature_850hPa":[null,2,3],
		"something_new":[{"a":1}]}}`
	vs := varSet{surface: surfaceVars, extra: forecastExtraVars, levels: []int{850}}
	b, err := parseBundle([]byte(body), vs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Current != nil || len(b.Hourly) != 3 || b.ElevationM.Valid() {
		t.Fatalf("bundle = %+v", *b)
	}
	h := b.Hourly
	if h[0].TempC != 1.5 || h[1].TempC.Valid() || h[2].TempC != 3.5 {
		t.Errorf("temperature column: %v %v %v", h[0].TempC, h[1].TempC, h[2].TempC)
	}
	if h[0].CAPE.Valid() || h[2].CAPE.Valid() {
		t.Error("all-null column should be missing")
	}
	if h[1].RH != 60 || h[2].RH.Valid() {
		t.Error("short column should leave the tail missing")
	}
	if h[0].WeatherCode.Valid() {
		t.Error("wrongly typed column should be missing, not fatal")
	}
	if h[0].PressureMSL.Valid() || h[0].BoundaryLayerM.Valid() {
		t.Error("absent columns should be missing")
	}
	if l := h[0].Levels; len(l) != 1 || l[0].HPa != 850 || l[0].TempC.Valid() || l[0].HeightM.Valid() || h[1].Levels[0].TempC != 2 {
		t.Errorf("levels = %+v", l)
	}
	if h[0].HasProfile() {
		t.Error("HasProfile with one incomplete level")
	}
	enc, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("bundle with missing values does not encode: %v", err)
	}
	if !json.Valid(enc) || !strings.Contains(string(enc), `"tempC":null`) || strings.Contains(string(enc), "NaN") {
		t.Errorf("encoding = %s", enc)
	}

	bad := map[string]string{
		"no time":       `{"hourly":{"temperature_2m":[1]}}`,
		"iso time":      `{"hourly":{"time":["2026-01-01T00:00"]}}`,
		"not ascending": `{"hourly":{"time":[7200,3600]}}`,
		"no hourly":     `{"latitude":1}`,
		"garbage":       `<html>busy</html>`,
		"api error":     `{"error":true,"reason":"boom"}`,
	}
	for name, body := range bad {
		if _, err := parseBundle([]byte(body), vs, nil); err == nil {
			t.Errorf("%s: accepted", name)
		} else if name == "api error" {
			var ae *APIError
			if !errors.As(err, &ae) || ae.Reason != "boom" {
				t.Errorf("api error = %v", err)
			}
		}
	}
	// A current-only request does not need an hourly block.
	cb, err := parseBundle([]byte(`{"current":{"time":3600,"interval":900,"temperature_2m":4,"is_day":1}}`), varSet{current: surfaceVars}, nil)
	if err != nil || cb.Current == nil || cb.Current.TempC != 4 || cb.Current.IsDay != 1 || cb.Current.RH.Valid() || len(cb.Hourly) != 0 {
		t.Errorf("current only: %v %+v", err, cb)
	}
	enc, err = json.Marshal(cb)
	if err != nil {
		t.Fatalf("bundle without grid coordinates does not encode: %v", err)
	}
	if !strings.Contains(string(enc), `"hourly":[]`) || !strings.Contains(string(enc), `"gridLat":null`) {
		t.Errorf("empty hourly should encode as [] and the unknown grid cell as null: %s", enc)
	}
	// Variables in the skip set stay missing even when the body carries them.
	sb, err := parseBundle([]byte(body), vs, map[string]bool{"temperature_2m": true, "temperature_850hPa": true})
	if err != nil || sb.Hourly[0].TempC.Valid() || sb.Hourly[1].Levels[0].TempC.Valid() || sb.Hourly[1].RH != 60 {
		t.Errorf("skip set not honoured: %v", err)
	}
}

func TestForecastRequest(t *testing.T) {
	body := fixture(t, "forecast.json")
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Cost", "13.0")
		w.Header().Set("X-RateLimit-Remaining", "587.0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(40*time.Second).Unix(), 10))
		_, _ = w.Write(body)
	})
	c := testClient(rec, Options{})
	ctx := context.Background()

	b, err := c.Forecast(ctx, 50.00001, 8, DefaultForecastOptions())
	if err != nil {
		t.Fatal(err)
	}
	r := rec.request(0)
	q := r.URL.Query()
	if r.URL.Path != "/forecast" || q.Get("latitude") != "50.0000" || q.Get("longitude") != "8.0000" {
		t.Errorf("url = %s", r.URL)
	}
	if n := len(list(q, "current")); n != 21 {
		t.Errorf("current variables = %d", n)
	}
	if n := len(list(q, "hourly")); n != 109 {
		t.Errorf("hourly variables = %d", n)
	}
	want := map[string]string{
		"past_days": "2", "forecast_days": "7", "timezone": "GMT", "timeformat": "unixtime",
		"wind_speed_unit": "kmh", "temperature_unit": "celsius", "precipitation_unit": "mm", "models": "",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("param %s = %q, want %q", k, q.Get(k), v)
		}
	}
	if ua := r.Header.Get("User-Agent"); ua != "AirView/0.1.0" && !strings.HasPrefix(ua, "AirView/") {
		t.Errorf("User-Agent = %q", ua)
	}
	if b.Kind != KindForecast || b.Model != "best_match" || b.Lat != 50.00001 || b.Lon != 8 || b.Cost != 13 ||
		time.Since(b.FetchedTime()) > time.Minute || len(b.Hourly) != 24 || b.Current == nil {
		t.Errorf("bundle meta: kind %s model %s lat %v cost %v", b.Kind, b.Model, b.Lat, b.Cost)
	}
	u := c.Usage()
	if u.Minute != 13 || u.Hour != 13 || u.Day != 13 || u.ServerRemaining != 587 {
		t.Errorf("usage = %+v", u)
	}

	// Option variants.
	if _, err := c.Forecast(ctx, 50.123456, -8.5, ForecastOptions{PastDays: 1, ForecastDays: 3, NoLevels: true, NoCurrent: true, Model: "icon_seamless"}); err != nil {
		t.Fatal(err)
	}
	q = rec.request(-1).URL.Query()
	if q.Has("current") || len(list(q, "hourly")) != 39 || q.Get("models") != "icon_seamless" ||
		q.Get("latitude") != "50.1235" || q.Get("longitude") != "-8.5000" || q.Get("past_days") != "1" || q.Get("forecast_days") != "3" {
		t.Errorf("variant url = %s", rec.request(-1).URL)
	}
	if strings.Contains(q.Get("hourly"), "hPa") {
		t.Error("NoLevels still requested pressure levels")
	}
	if _, err := c.Forecast(ctx, 50, 8, ForecastOptions{Levels: []int{925, 850}, ForecastDays: 1}); err != nil {
		t.Fatal(err)
	}
	q = rec.request(-1).URL.Query()
	if n := len(list(q, "hourly")); n != 39+10 || !strings.Contains(q.Get("hourly"), "geopotential_height_925hPa") {
		t.Errorf("custom levels: %d hourly variables", n)
	}
	// Zero options = current conditions only.
	if _, err := c.Forecast(ctx, 50, 8, ForecastOptions{}); err != nil {
		t.Fatal(err)
	}
	q = rec.request(-1).URL.Query()
	if q.Has("hourly") || len(list(q, "current")) != 21 || q.Get("forecast_days") != "0" {
		t.Errorf("current-only url = %s", rec.request(-1).URL)
	}

	// Invalid input never reaches the network.
	before := rec.count("")
	for _, bad := range [][2]float64{{91, 0}, {0, 181}, {math.NaN(), 0}, {-90.1, 0}} {
		if _, err := c.Forecast(ctx, bad[0], bad[1], DefaultForecastOptions()); !errors.Is(err, ErrBadCoordinate) {
			t.Errorf("coordinate %v: %v", bad, err)
		}
	}
	if _, err := c.Forecast(ctx, 50, 8, ForecastOptions{ForecastDays: 1, Model: "x&y=1"}); err == nil {
		t.Error("bad model accepted")
	}
	if _, err := c.Forecast(ctx, 50, 8, ForecastOptions{NoCurrent: true}); err == nil {
		t.Error("empty request accepted")
	}
	if _, err := c.Forecast(ctx, 50, 8, ForecastOptions{ForecastDays: 1, Levels: []int{5000}}); err == nil {
		t.Error("bad level accepted")
	}
	if rec.count("") != before {
		t.Error("invalid input caused network requests")
	}
}

func TestUnknownVariableIsDropped(t *testing.T) {
	body := fixture(t, "forecast.json")
	unknown := fixture(t, "error_unknown_variable.json")
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("hourly"), "soil_temperature_0cm") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(strings.Replace(string(unknown), "bogus_var", "soil_temperature_0cm", 1)))
			return
		}
		_, _ = w.Write(body)
	})
	c := testClient(rec, Options{})
	b, err := c.Forecast(context.Background(), 50, 8, DefaultForecastOptions())
	if err != nil {
		t.Fatal(err)
	}
	if rec.count("") != 2 {
		t.Errorf("requests = %d, want 2 (reject + retry)", rec.count(""))
	}
	if n := len(list(rec.request(-1).URL.Query(), "hourly")); n != 108 {
		t.Errorf("retry asked for %d hourly variables, want 108", n)
	}
	if b.Hourly[0].SoilTempC.Valid() || !b.Hourly[0].TempC.Valid() {
		t.Error("dropped variable should be missing, the rest present")
	}
	// The client remembers: the next poll does not trip over it again.
	if _, err := c.Forecast(context.Background(), 50, 8, DefaultForecastOptions()); err != nil {
		t.Fatal(err)
	}
	if rec.count("") != 3 {
		t.Errorf("requests = %d, want 3", rec.count(""))
	}
	// A 400 about something we did not ask for is a plain error.
	rec2 := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(unknown)
	})
	_, err = testClient(rec2, Options{}).Forecast(context.Background(), 50, 8, DefaultForecastOptions())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 || !strings.Contains(ae.Reason, "bogus_var") || rec2.count("") != 1 {
		t.Errorf("err = %v after %d requests", err, rec2.count(""))
	}
}

func TestHTTPErrorHandling(t *testing.T) {
	body := fixture(t, "forecast.json")
	ctx := context.Background()

	// 429 is never retried and carries the server's Retry-After.
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":true,"reason":"Minutely API request limit exceeded. Please try again in one minute."}`))
	})
	_, err := testClient(rec, Options{}).Forecast(ctx, 50, 8, DefaultForecastOptions())
	var ae *APIError
	if !errors.As(err, &ae) || !ae.RateLimited() || ae.RetryAfter != 2*time.Minute || !strings.Contains(ae.Reason, "Minutely") {
		t.Errorf("429: %v (%+v)", err, ae)
	}
	if rec.count("") != 1 {
		t.Errorf("429 was retried: %d requests", rec.count(""))
	}

	// 5xx is retried, then succeeds.
	var mu sync.Mutex
	fails := 2
	rec = newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
			return
		}
		_, _ = w.Write(body)
	})
	if _, err := testClient(rec, Options{}).Forecast(ctx, 50, 8, DefaultForecastOptions()); err != nil {
		t.Errorf("retry after 5xx: %v", err)
	}
	if rec.count("") != 3 {
		t.Errorf("requests = %d, want 3", rec.count(""))
	}

	// Persistent 5xx gives up after the configured retries.
	rec = newRecorder(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	_, err = testClient(rec, Options{Retries: 1}).Forecast(ctx, 50, 8, DefaultForecastOptions())
	if !errors.As(err, &ae) || ae.Status != 503 || rec.count("") != 2 {
		t.Errorf("503: %v after %d requests", err, rec.count(""))
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error text = %q", err)
	}

	// 4xx with a reason is reported verbatim and not retried.
	rec = newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"reason":"Latitude must be in range of -90 to 90°. Given: 95.0.","error":true}`))
	})
	_, err = testClient(rec, Options{}).Forecast(ctx, 50, 8, DefaultForecastOptions())
	if !errors.As(err, &ae) || ae.Status != 400 || !strings.HasPrefix(ae.Reason, "Latitude must be") || rec.count("") != 1 {
		t.Errorf("400: %v after %d requests", err, rec.count(""))
	}

	// Network errors must not leak the request URL (station coordinates).
	rec = newRecorder(t, func(w http.ResponseWriter, r *http.Request) {})
	c := testClient(rec, Options{Retries: -1})
	rec.srv.Close()
	_, err = c.Forecast(ctx, 50.4321, 8.1234, DefaultForecastOptions())
	if err == nil || strings.Contains(err.Error(), "50.4321") || strings.Contains(err.Error(), "latitude") {
		t.Errorf("network error = %v", err)
	}

	// Cancellation is reported as such.
	rec = newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := testClient(rec, Options{}).Forecast(cctx, 50, 8, DefaultForecastOptions()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cancel: %v", err)
	}
}

func TestRetryAfterHeuristics(t *testing.T) {
	now := time.Date(2026, 11, 11, 22, 10, 30, 0, time.UTC)
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	cases := []struct {
		name   string
		h      http.Header
		reason string
		want   time.Duration
	}{
		{"retry-after seconds", hdr("Retry-After", "30"), "", 30 * time.Second},
		{"retry-after date", hdr("Retry-After", now.Add(90*time.Second).Format(http.TimeFormat)), "", 90 * time.Second},
		{"daily", hdr(), "Daily API request limit exceeded. Please try again tomorrow.", 1*time.Hour + 54*time.Minute + 30*time.Second},
		{"hourly", hdr(), "Hourly API request limit exceeded. Please try again in the next hour.", 50*time.Minute + 30*time.Second},
		{"reset header", hdr("X-RateLimit-Reset", strconv.FormatInt(now.Add(39*time.Second).Unix(), 10)), "Minutely API request limit exceeded.", 40 * time.Second},
		{"minutely", hdr(), "Minutely API request limit exceeded. Please try again in one minute.", 61 * time.Second},
		{"concurrent", hdr(), "Too many concurrent requests", 30 * time.Second},
		{"unknown", hdr(), "", 5 * time.Minute},
	}
	for _, c := range cases {
		if got := retryAfter(c.h, c.reason, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBudget(t *testing.T) {
	body := fixture(t, "forecast.json")
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) })
	ctx := context.Background()

	c := testClient(rec, Options{Budget: Budget{PerMinute: 20, PerHour: 1000, PerDay: 1000}})
	clock := time.Date(2026, 11, 11, 12, 0, 10, 0, time.UTC)
	c.now = func() time.Time { return clock }
	if _, err := c.Forecast(ctx, 50, 8, DefaultForecastOptions()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Forecast(ctx, 50, 8, DefaultForecastOptions()); !errors.Is(err, ErrBudget) {
		t.Fatalf("second call inside the minute: %v", err)
	}
	if rec.count("") != 1 {
		t.Errorf("over-budget call reached the network (%d requests)", rec.count(""))
	}
	if u := c.Usage(); u.Minute != 13 || u.Budget.PerMinute != 20 || u.ServerRemaining != -1 {
		t.Errorf("usage = %+v", u)
	}
	clock = clock.Add(2 * time.Minute)
	if _, err := c.Forecast(ctx, 50, 8, DefaultForecastOptions()); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if u := c.Usage(); u.Minute != 13 || u.Hour != 26 || u.Day != 26 {
		t.Errorf("usage = %+v", u)
	}

	// Daily cap.
	c = testClient(rec, Options{Budget: Budget{PerMinute: 1000, PerHour: 1000, PerDay: 20}})
	clock = time.Date(2026, 11, 11, 12, 0, 10, 0, time.UTC)
	c.now = func() time.Time { return clock }
	if _, err := c.Forecast(ctx, 50, 8, DefaultForecastOptions()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(23 * time.Hour)
	if _, err := c.Forecast(ctx, 50, 8, DefaultForecastOptions()); !errors.Is(err, ErrBudget) {
		t.Fatalf("daily cap: %v", err)
	}
	clock = clock.Add(61 * time.Minute)
	if _, err := c.Forecast(ctx, 50, 8, DefaultForecastOptions()); err != nil {
		t.Fatalf("after 24 h: %v", err)
	}
	if u := c.Usage(); u.Day != 13 {
		t.Errorf("old usage not pruned: %+v", u)
	}

	// Defaults stay well inside the free tier.
	d := NewClient(Options{}).Usage().Budget
	if d.PerMinute > FreeTierPerMinute/2 || d.PerHour > FreeTierPerHour/2 || d.PerDay > FreeTierPerDay/2 {
		t.Errorf("default budget %+v exceeds half the free tier", d)
	}
	perDayAtMinInterval := float64(24*60/MinIntervalMin) * estimateCost(130, 9)
	if perDayAtMinInterval > d.PerDay {
		t.Errorf("polling at the minimum interval needs %.0f calls/day, budget is %.0f", perDayAtMinInterval, d.PerDay)
	}
}

func TestHistoryChunksAndSources(t *testing.T) {
	archive, hf := fixture(t, "archive.json"), fixture(t, "historical_forecast.json")
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/archive":
			_, _ = w.Write(archive)
		case "/hf":
			_, _ = w.Write(hf)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	c := testClient(rec, Options{})
	ctx := context.Background()
	date := func(s string) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	b, err := c.History(ctx, 50, 8, date("2015-12-20").Add(13*time.Hour), date("2016-01-20"), HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	type call struct{ path, start, end string }
	var got []call
	for i := 0; i < rec.count(""); i++ {
		r := rec.request(i)
		got = append(got, call{r.URL.Path, r.URL.Query().Get("start_date"), r.URL.Query().Get("end_date")})
	}
	want := []call{
		{"/archive", "2015-12-20", "2015-12-31"},
		{"/hf", "2016-01-01", "2016-01-14"},
		{"/hf", "2016-01-15", "2016-01-20"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v\nwant %v", got, want)
	}
	aq := rec.request(0).URL.Query()
	ah := aq.Get("hourly")
	if n := len(list(aq, "hourly")); n != 26 {
		t.Errorf("archive hourly variables = %d, want 26", n)
	}
	if strings.Contains(ah, "hPa") || strings.Contains(ah, "visibility") || strings.Contains(ah, "cape") ||
		!strings.Contains(ah, "soil_temperature_0_to_7cm") || !strings.Contains(ah, "wind_speed_100m") {
		t.Errorf("archive variable list wrong: %s", ah)
	}
	if aq.Has("current") || aq.Get("timeformat") != "unixtime" {
		t.Errorf("archive params: %s", rec.request(0).URL.RawQuery)
	}
	if n := len(list(rec.request(1).URL.Query(), "hourly")); n != 109 {
		t.Errorf("historical-forecast hourly variables = %d, want 109", n)
	}

	if len(b.Hourly) != 72 || b.Current != nil || b.Kind != "archive+historical-forecast" {
		t.Fatalf("bundle: %d hours, kind %s", len(b.Hourly), b.Kind)
	}
	if wantCost := 2.6 + 10.9 + 10.9; math.Abs(b.Cost-wantCost) > 1e-9 {
		t.Errorf("cost = %v, want %v", b.Cost, wantCost)
	}
	a := b.Hourly[7] // ERA5 hour: surface only
	if a.Levels != nil || a.CAPE.Valid() || a.VisibilityM.Valid() || a.Wind100Kmh != 11.5 || a.SoilTempC != 16.6 ||
		b.Hourly[0].TempC != 18.9 || b.Hourly[0].T != 1790812800000 {
		t.Errorf("archive hour = %+v", a)
	}
	m := b.Hourly[24+10] // model hour: with profile
	if !m.HasProfile() || m.Levels[5].TempC != 11.6 || b.Hourly[24+14].CAPE != 140 || m.Wind100Kmh.Valid() {
		t.Errorf("historical-forecast hour = %+v", m)
	}
	if !reflect.DeepEqual(b.LevelsHPa, DefaultLevels) {
		t.Errorf("LevelsHPa = %v", b.LevelsHPa)
	}

	// Forced sources and option handling.
	n0 := rec.count("")
	if _, err := c.History(ctx, 50, 8, date("2024-03-01"), date("2024-03-03"), HistoryOptions{Source: HistoryReanalysis}); err != nil {
		t.Fatal(err)
	}
	if r := rec.request(-1); rec.count("") != n0+1 || r.URL.Path != "/archive" {
		t.Errorf("forced reanalysis used %s", r.URL.Path)
	}
	if _, err := c.History(ctx, 50, 8, date("2024-03-01"), date("2024-03-10"), HistoryOptions{Source: HistoryModel, NoLevels: true, ChunkDays: 5}); err != nil {
		t.Fatal(err)
	}
	if r := rec.request(-1); rec.count("") != n0+3 || r.URL.Path != "/hf" || strings.Contains(r.URL.Query().Get("hourly"), "hPa") ||
		r.URL.Query().Get("start_date") != "2024-03-06" || r.URL.Query().Get("end_date") != "2024-03-10" {
		t.Errorf("forced model: %s", r.URL)
	}

	n0 = rec.count("")
	if _, err := c.History(ctx, 50, 8, date("2015-06-01"), date("2015-06-02"), HistoryOptions{Source: HistoryModel}); err == nil {
		t.Error("historical-forecast before 2016 accepted")
	}
	if _, err := c.History(ctx, 50, 8, date("2024-03-02"), date("2024-03-01"), HistoryOptions{}); err == nil {
		t.Error("reversed range accepted")
	}
	if _, err := c.History(ctx, 50, 8, date("1939-12-31"), date("1940-01-02"), HistoryOptions{}); err == nil {
		t.Error("range before 1940 accepted")
	}
	if _, err := c.History(ctx, 99, 8, date("2024-03-01"), date("2024-03-01"), HistoryOptions{}); !errors.Is(err, ErrBadCoordinate) {
		t.Errorf("bad coordinate: %v", err)
	}
	if rec.count("") != n0 {
		t.Error("invalid history requests reached the network")
	}

	// The callback can stop a backfill.
	stop := errors.New("stop")
	calls := 0
	err = c.HistoryEach(ctx, 50, 8, date("2024-01-01"), date("2024-03-01"), HistoryOptions{}, func(*Bundle) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("HistoryEach stop: %v after %d calls", err, calls)
	}
}

// A backfill sits out short rate-limit answers (the archive hosts answer
// "Too many concurrent requests" while an earlier, timed-out request of the
// same client is still being processed) but reports long ones.
func TestHistoryRateLimitHandling(t *testing.T) {
	hf := fixture(t, "historical_forecast.json")
	var mu sync.Mutex
	busy := 2
	reason := `{"reason":"Too many concurrent requests","error":true}`
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if busy > 0 {
			busy--
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(reason))
			return
		}
		_, _ = w.Write(hf)
	})
	c := testClient(rec, Options{})
	ctx := context.Background()
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	b, err := c.History(ctx, 50, 8, day, day, HistoryOptions{})
	if err != nil || len(b.Hourly) != 24 || rec.count("/hf") != 3 {
		t.Fatalf("history through two 429s: %v after %d requests", err, rec.count("/hf"))
	}
	if c.histTimeout != 90*time.Second || c.timeout != 25*time.Second {
		t.Errorf("default timeouts = %v / %v", c.timeout, c.histTimeout)
	}

	// The interactive forecast path never waits on a 429.
	rec2 := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(reason))
	})
	c2 := testClient(rec2, Options{})
	_, err = c2.Forecast(ctx, 50, 8, DefaultForecastOptions())
	var ae *APIError
	if !errors.As(err, &ae) || !ae.RateLimited() || ae.RetryAfter != 30*time.Second || rec2.count("") != 1 {
		t.Errorf("forecast 429: %v after %d requests", err, rec2.count(""))
	}
	// A long limit (hourly/daily) ends the backfill instead of blocking it.
	rec3 := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"reason":"Daily API request limit exceeded. Please try again tomorrow.","error":true}`))
	})
	_, err = testClient(rec3, Options{}).History(ctx, 50, 8, day, day, HistoryOptions{})
	if !errors.As(err, &ae) || !ae.RateLimited() || rec3.count("") != 1 {
		t.Errorf("daily limit during backfill: %v after %d requests", err, rec3.count(""))
	}
}

func TestParseArchiveFixture(t *testing.T) {
	vs := varSet{extra: archiveExtraVars}
	for _, v := range surfaceVars {
		if !archiveMissing[v.name] {
			vs.surface = append(vs.surface, v)
		}
	}
	_, hourly := vs.names(nil)
	// Every variable we request from the archive is present in the recording.
	var src struct {
		Hourly map[string]json.RawMessage `json:"hourly"`
	}
	if err := json.Unmarshal(fixture(t, "archive.json"), &src); err != nil {
		t.Fatal(err)
	}
	for _, n := range hourly {
		if n == "showers" {
			continue // verified separately against the live API; not in the recording
		}
		if _, ok := src.Hourly[n]; !ok {
			t.Errorf("archive recording lacks %s", n)
		}
	}
	b, err := parseBundle(fixture(t, "archive.json"), vs, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := b.Hourly[12]
	if len(b.Hourly) != 24 || !h.TempC.Valid() || !h.PressureMSL.Valid() || !h.BoundaryLayerM.Valid() ||
		!h.ShortwaveWm2.Valid() || !h.PWATmm.Valid() || !h.WindDir100.Valid() || h.Levels != nil || h.HasProfile() {
		t.Errorf("archive hour 12 = %+v", h)
	}
}

func TestElevation(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("latitude") {
		case "50.0000":
			_, _ = w.Write(fixture(t, "elevation.json"))
		case "1.0000":
			_, _ = w.Write([]byte(`{"elevation":[]}`))
		case "2.0000":
			_, _ = w.Write([]byte(`{"elevation":[null]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"reason":"nope","error":true}`))
		}
	})
	c := testClient(rec, Options{})
	ctx := context.Background()
	e, err := c.Elevation(ctx, 50, 8)
	if err != nil || e != 109 {
		t.Fatalf("elevation = %v, %v", e, err)
	}
	if r := rec.request(0); r.URL.Path != "/elevation" || r.URL.Query().Get("longitude") != "8.0000" {
		t.Errorf("url = %s", r.URL)
	}
	if c.Usage().Day != 1 {
		t.Errorf("elevation cost = %v, want 1", c.Usage().Day)
	}
	for _, lat := range []float64{1, 2, 3} {
		if _, err := c.Elevation(ctx, lat, 8); err == nil {
			t.Errorf("lat %v: no error", lat)
		}
	}
	if _, err := c.Elevation(ctx, -91, 8); !errors.Is(err, ErrBadCoordinate) {
		t.Errorf("bad coordinate: %v", err)
	}
}

func TestRadarIndex(t *testing.T) {
	r, err := parseRadar(fixture(t, "rainviewer.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Host != "https://tilecache.rainviewer.com" || r.Generated != 1794439821000 || r.MaxZoom != 7 || r.TileSize != 256 ||
		r.Attribution == "" || len(r.Nowcast) != 0 {
		t.Errorf("radar = %+v", *r)
	}
	if len(r.Past) != 13 {
		t.Fatalf("past frames = %d, want 13", len(r.Past))
	}
	f := r.Past[0]
	if f.T != 1794432600000 || f.Path != "/v2/radar/1231b6938e3f" ||
		f.URL != "https://tilecache.rainviewer.com/v2/radar/1231b6938e3f/256/{z}/{x}/{y}/2/1_1.png" {
		t.Errorf("first frame = %+v", f)
	}
	for i := 1; i < len(r.Past); i++ {
		if r.Past[i].T-r.Past[i-1].T != 600000 {
			t.Errorf("frame spacing %d ms", r.Past[i].T-r.Past[i-1].T)
		}
	}
	if last, ok := r.Latest(); !ok || last != r.Past[12] {
		t.Error("Latest wrong")
	}
	if got := r.TileURL(f, 512, 0, false, false); got != "https://tilecache.rainviewer.com/v2/radar/1231b6938e3f/512/{z}/{x}/{y}/0/0_0.png" {
		t.Errorf("TileURL = %s", got)
	}
	if got := r.TileURL(f, 300, -4, true, false); !strings.HasSuffix(got, "/256/{z}/{x}/{y}/2/1_0.png") {
		t.Errorf("TileURL with bad options = %s", got)
	}
	enc, _ := json.Marshal(r)
	if !strings.Contains(string(enc), `"nowcast":[]`) {
		t.Errorf("nowcast should encode as []: %s", enc)
	}

	// The index is untrusted input that ends up in browser tile URLs.
	bad := []string{
		`{"host":"http://tilecache.rainviewer.com","radar":{"past":[{"time":1,"path":"/v2/radar/a"}]}}`,
		`{"host":"https://evil.example","radar":{"past":[{"time":1,"path":"/v2/radar/a"}]}}`,
		`{"host":"https://rainviewer.com.evil.example","radar":{"past":[{"time":1,"path":"/v2/radar/a"}]}}`,
		`{"host":"https://user@tilecache.rainviewer.com","radar":{"past":[{"time":1,"path":"/v2/radar/a"}]}}`,
		`{"host":"https://tilecache.rainviewer.com/x?y=1","radar":{"past":[{"time":1,"path":"/v2/radar/a"}]}}`,
		`{"host":"https://tilecache.rainviewer.com","radar":{"past":[]}}`,
		`{"host":"https://tilecache.rainviewer.com","radar":{"past":[{"time":1,"path":"/v2/../../x"},{"time":2,"path":"/a\"><script>"},{"time":0,"path":"/v2/radar/a"},{"time":3,"path":"no-slash"}]}}`,
		`not json`,
	}
	for _, doc := range bad {
		if r, err := parseRadar([]byte(doc)); err == nil {
			t.Errorf("accepted %s -> %+v", doc, r)
		}
	}
	ok := `{"host":"https://TileCache.RainViewer.com/","radar":{"past":[{"time":5,"path":"/v2/radar/a"},{"time":6,"path":"/x y"}],"nowcast":[{"time":7,"path":"/v2/radar/nowcast_b"}]}}`
	r, err = parseRadar([]byte(ok))
	if err != nil || len(r.Past) != 1 || len(r.Nowcast) != 1 || r.Past[0].T != 5000 {
		t.Errorf("sanitised index: %v %+v", err, r)
	}
}

func TestRadarConditionalGet(t *testing.T) {
	body := fixture(t, "rainviewer.json")
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		const etag = `"W/"3388100446""` // RainViewer's (oddly quoted) validator, verbatim
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write(body)
	})
	c := testClient(rec, Options{})
	a, err := c.RadarFrames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.RadarFrames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec.count("/radar") != 2 || rec.request(0).Header.Get("If-None-Match") != "" || rec.request(1).Header.Get("If-None-Match") == "" {
		t.Errorf("conditional request not sent")
	}
	if len(b.Past) != len(a.Past) || b.Generated != a.Generated {
		t.Error("304 answer did not reuse the cached index")
	}
	if c.Usage().Day != 0 {
		t.Error("RainViewer requests must not count against the Open-Meteo budget")
	}
}

// Live tests hit the real services; opt in with AIRVIEW_LIVE_TESTS=1.
func TestLive(t *testing.T) {
	if os.Getenv("AIRVIEW_LIVE_TESTS") == "" {
		t.Skip("set AIRVIEW_LIVE_TESTS=1 to run tests against the real Open-Meteo and RainViewer APIs")
	}
	c := NewClient(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const lat, lon = 50.0, 8.0 // neutral test location

	b, err := c.Forecast(ctx, lat, lon, DefaultForecastOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("forecast: %d hours, cost %.1f, elevation %.0f m, usage %+v", len(b.Hourly), b.Cost, b.ElevationM.Float(), c.Usage())
	if len(b.Hourly) != 9*24 || b.Current == nil || math.Abs(b.Cost-13.0) > 0.01 {
		t.Errorf("forecast: %d hours, current %v, cost %v", len(b.Hourly), b.Current != nil, b.Cost)
	}
	if !b.Current.TempC.Valid() || !b.Current.PressureMSL.Valid() || !b.Current.VisibilityM.Valid() || !b.Current.CAPE.Valid() {
		t.Errorf("current incomplete: %+v", *b.Current)
	}
	now, ok := b.At(time.Now())
	if !ok || !now.HasProfile() || len(now.Levels) != 14 {
		t.Fatalf("no profile for the current hour")
	}
	if bad := firstMissing(now); bad != "" {
		t.Errorf("current hour lacks %s", bad)
	}
	for _, l := range now.Levels {
		if !l.TempC.Valid() || !l.RH.Valid() || !l.HeightM.Valid() || !l.WindKmh.Valid() || !l.WindDir.Valid() {
			t.Errorf("level %v hPa incomplete: %+v", l.HPa, l)
		}
	}
	if _, err := json.Marshal(b); err != nil {
		t.Errorf("encode: %v", err)
	}

	to := time.Now().AddDate(0, -1, 0)
	h, err := c.History(ctx, lat, lon, to.AddDate(0, 0, -1), to, HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Hourly) != 48 || h.Kind != KindHistoricalForecast || !h.Hourly[30].HasProfile() || !h.Hourly[30].TempC.Valid() {
		t.Errorf("history: %d hours kind %s", len(h.Hourly), h.Kind)
	}
	if bad := firstMissing(h.Hourly[30]); bad != "" {
		t.Errorf("historical-forecast hour lacks %s", bad)
	}
	for _, l := range h.Hourly[30].Levels {
		if !l.TempC.Valid() || !l.RH.Valid() || !l.HeightM.Valid() || !l.WindKmh.Valid() || !l.WindDir.Valid() {
			t.Errorf("historical-forecast level %v hPa incomplete: %+v", l.HPa, l)
		}
	}
	// Before the model archive begins (2017 at this location) the
	// historical-forecast API answers nulls; HistoryAuto must fall back to ERA5.
	early := time.Date(2016, 6, 1, 0, 0, 0, 0, time.UTC)
	h, err = c.History(ctx, lat, lon, early, early, HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Hourly) != 24 || h.Kind != KindArchive || !h.Hourly[12].TempC.Valid() || h.Hourly[12].Levels != nil {
		t.Errorf("2016 history: %d hours kind %s hour12 temp %v", len(h.Hourly), h.Kind, h.Hourly[12].TempC)
	}
	// Model surface data without profiles (2017 .. 2021-03 at this location).
	mid := time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC)
	h, err = c.History(ctx, lat, lon, mid, mid, HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Hourly) != 24 || h.Kind != KindHistoricalForecast || !h.Hourly[12].TempC.Valid() {
		t.Errorf("2019 history: %d hours kind %s hour12 temp %v", len(h.Hourly), h.Kind, h.Hourly[12].TempC)
	}
	t.Logf("2019-06-01 12Z: profile %v (pressure-level archive starts 2021-03-23 at the test location)", h.Hourly[12].HasProfile())
	old := time.Date(1990, 7, 1, 0, 0, 0, 0, time.UTC)
	h, err = c.History(ctx, lat, lon, old, old, HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Hourly) != 24 || h.Kind != KindArchive || !h.Hourly[12].TempC.Valid() || !h.Hourly[12].ShowersMM.Valid() ||
		!h.Hourly[12].Wind100Kmh.Valid() || h.Hourly[12].Levels != nil {
		t.Errorf("archive: %d hours kind %s hour12 %+v", len(h.Hourly), h.Kind, h.Hourly[12])
	}
	// Every variable we request from ERA5 is really there.
	a := h.Hourly[12]
	for name, v := range map[string]Num{
		"rh": a.RH, "dewPoint": a.DewPointC, "apparent": a.ApparentC, "precip": a.PrecipMM, "rain": a.RainMM,
		"snow": a.SnowfallCM, "weatherCode": a.WeatherCode, "cloud": a.Cloud, "cloudLow": a.CloudLow,
		"cloudMid": a.CloudMid, "cloudHigh": a.CloudHigh, "pressureMsl": a.PressureMSL,
		"surfacePressure": a.SurfacePressure, "wind": a.WindKmh, "windDir": a.WindDir, "gust": a.GustKmh,
		"isDay": a.IsDay, "boundaryLayer": a.BoundaryLayerM, "shortwave": a.ShortwaveWm2, "soilTemp": a.SoilTempC,
		"pwat": a.PWATmm, "snowDepth": a.SnowDepthM, "windDir100": a.WindDir100,
	} {
		if !v.Valid() {
			t.Errorf("archive hour lacks %s", name)
		}
	}

	e, err := c.Elevation(ctx, lat, lon)
	if err != nil || e < 50 || e > 200 {
		t.Errorf("elevation = %v, %v", e, err)
	}
	r, err := c.RadarFrames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Past) < 6 || !strings.HasPrefix(r.Past[0].URL, "https://") {
		t.Errorf("radar: %+v", r)
	}
	t.Logf("radar: %d frames, latest %s; usage %+v", len(r.Past), r.Past[len(r.Past)-1].URL, c.Usage())
}

// firstMissing names a forecast-hour field that is unexpectedly missing.
func firstMissing(h HourPoint) string {
	skip := map[string]bool{"Wind100Kmh": true, "WindDir100": true}
	v := reflect.ValueOf(h)
	var walk func(v reflect.Value) string
	walk = func(v reflect.Value) string {
		for i := 0; i < v.NumField(); i++ {
			f, name := v.Field(i), v.Type().Field(i).Name
			switch {
			case f.Kind() == reflect.Struct:
				if bad := walk(f); bad != "" {
					return bad
				}
			case f.Type() == reflect.TypeOf(Num(0)):
				if math.IsNaN(f.Float()) && !skip[name] {
					return name
				}
			}
		}
		return ""
	}
	return walk(v)
}
