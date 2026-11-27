package predict

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/radio"
	"airview/internal/store"
	"airview/internal/weather"
)

// ---------------------------------------------------------------- synthetic station

var testNow = time.Date(2026, 6, 15, 12, 20, 0, 0, time.UTC)

// smooth is a deterministic slowly varying process in about [-1, 1].
func smooth(t int64, seed float64) float64 {
	h := float64(t) / 3600
	return (math.Sin(2*math.Pi*h/31+seed) + math.Sin(2*math.Pi*h/53+2.1*seed) +
		math.Sin(2*math.Pi*h/97+0.7*seed) + math.Sin(2*math.Pi*h/171+1.3*seed)) / 2.6
}

// trueAircraft is the planted traffic pattern: a daytime bulge, quieter at
// weekends (UTC).
func trueAircraft(t int64) float64 {
	tm := time.Unix(t, 0).UTC()
	hod := float64(tm.Hour())
	day := math.Max(0, math.Sin(math.Pi*(hod-5)/17))
	v := 12 + 40*day
	if wd := tm.Weekday(); wd == time.Saturday || wd == time.Sunday {
		v *= 0.6
	}
	return v
}

// trueGradient is the planted refractivity gradient (N/km).
func trueGradient(t int64) float64 {
	s := smooth(t, 0.4)
	g := -45 - 50*s
	if s > 0.6 {
		g -= 200 * (s - 0.6)
	}
	return g
}

// trueP95 is the planted weather -> range dependence: 0.5 km per N/km of
// extra downward bending, plus a traffic term.
func trueP95(t int64) float64 {
	return 250 + 0.5*(-40-trueGradient(t)) + 12*math.Log1p(trueAircraft(t))
}

func synthWeather(t int64) store.Weather {
	w := store.NewWeather(t)
	hod := float64((t % 86400) / 3600)
	w.TempC = 12 + 6*math.Sin(2*math.Pi*(hod-9)/24) + 4*smooth(t, 1.9)
	w.DewpointC = w.TempC - (3 + 3*(0.5+0.5*smooth(t, 2.7)))
	w.RH = radio.RHFromDewPoint(w.TempC, w.DewpointC)
	w.PressureMSL = 1013 + 10*smooth(t, 3.3)
	w.SurfacePressure = w.PressureMSL - 12
	w.WindKmh = 14 + 9*smooth(t, 4.1)
	w.WindDir = math.Mod(220+70*smooth(t, 5.2)+360, 360)
	w.GustKmh = w.WindKmh * 1.6
	w.Cloud = clamp(50+55*smooth(t, 6.0), 0, 100)
	w.CloudLow = w.Cloud * 0.6
	if smooth(t, 6.0) > 0.55 {
		w.PrecipMM = 0.8
	} else {
		w.PrecipMM = 0
	}
	w.CAPE = 50
	w.VisibilityM = 30000
	w.WeatherCode = 2
	if hod >= 5 && hod < 20 {
		w.IsDay = 1
	} else {
		w.IsDay = 0
	}
	g := trueGradient(t)
	w.DNDH = g
	w.KFactor = radio.KFactor(g)
	w.NSurface = 320
	if g <= -157 {
		w.Duct = 8
	} else {
		w.Duct = 0
	}
	return w
}

type seedOpts struct {
	days     int
	stats    bool // stats_hour + range_hour
	weather  bool // weather_obs
	forecast bool // weather_fc for the next 96 h
	fcLog    bool // weather_fc_log with a planted +1.5 K temperature bias
	// mutate lets a test change the observation of an hour (hoursAgo counts
	// back from the current hour, 1 = last complete hour).
	mutate func(hoursAgo int, w *store.Weather)
	fcMut  func(lead int, w *store.Weather)
	// poisonLast replaces the range / traffic of the last N complete hours.
	poisonLast int
}

func seed(t *testing.T, st *store.Store, now time.Time, o seedOpts) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))
	cur := floorHour(now.Unix())
	b := store.NewBatch()
	var obs, fcl []store.Weather
	type sr struct {
		t           int64
		ac, frames  float64
		p95, maxKm  float64
		haveWeather bool
	}
	var srs []sr
	for ts := cur - int64(o.days)*86400; ts < cur; ts += 3600 {
		hoursAgo := int((cur - ts) / 3600)
		// Draw the noise unconditionally so that variants of a scenario
		// share the same random sequence.
		nAc, nFr, nP, nM, nT := rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64()
		ac := math.Max(0, trueAircraft(ts)+3*nAc)
		r := sr{t: ts, ac: math.Round(ac), frames: math.Round((20*ac + 15*nFr) * 3600)}
		r.p95 = trueP95(ts) + 6*nP
		r.maxKm = r.p95 + 60 + 0.3*(-40-trueGradient(ts)) + 10*nM
		if hoursAgo <= o.poisonLast {
			r.p95, r.maxKm, r.ac, r.frames = 5000, 5000, 900, 9e8
		}
		srs = append(srs, r)
		if o.weather {
			w := synthWeather(ts)
			if o.mutate != nil {
				o.mutate(hoursAgo, &w)
			}
			obs = append(obs, w)
			if o.fcLog {
				for _, lead := range []int{6, 12, 24} {
					f := w
					f.TempC = w.TempC + 1.5 + 0.4*nT
					f.LeadH, f.Issued = lead, 0
					fcl = append(fcl, f)
				}
			}
		}
	}
	if o.weather {
		b.AddWeatherObs(obs)
	}
	if o.fcLog {
		b.AddWeatherForecastLog(fcl)
	}
	if o.forecast {
		var fc []store.Weather
		for k := 0; k < 96; k++ {
			w := synthWeather(cur + int64(k)*3600)
			w.Issued = now.Unix()
			if o.fcMut != nil {
				o.fcMut(k, &w)
			}
			fc = append(fc, w)
		}
		b.SetWeatherForecast(fc)
	}
	if err := st.Apply(ctx, b); err != nil {
		t.Fatal(err)
	}
	if o.stats {
		err := st.Write(ctx, func(tx *sql.Tx) error {
			for _, r := range srs {
				if _, err := tx.Exec(`INSERT INTO stats_hour (t, ant, frames, aircraft, aircraft_avg, minutes) VALUES (?, '', ?, ?, ?, 60)`,
					r.t, int64(r.frames), int64(r.ac), r.ac); err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO range_hour (t, ant, max_km, p95_km, p50_km, positions, aircraft) VALUES (?, '', ?, ?, ?, 800, ?)`,
					r.t, r.maxKm, r.p95, r.p95/2, int64(r.ac)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func testConfig() model.Config {
	c := model.DefaultConfig()
	c.Receiver.Located, c.Receiver.Lat, c.Receiver.Lon, c.Receiver.GroundElevM = true, 52.3, 4.8, 5
	return c
}

func newTest(t *testing.T, o seedOpts) (*Forecaster, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if o.days > 0 {
		seed(t, st, testNow, o)
	}
	zero := 0
	f := NewWithOptions(st, testConfig, Options{Now: func() time.Time { return testNow }, UTCOffsetSec: &zero})
	return f, st
}

// products fetches every product and checks that all of them encode.
func products(t *testing.T, f *Forecaster) (*TrafficForecast, *ReceptionForecast, *WeatherOutlook, *Verification, *Nowcast) {
	t.Helper()
	ctx := context.Background()
	tr, err := f.Traffic(ctx)
	if err != nil {
		t.Fatalf("Traffic: %v", err)
	}
	re, err := f.Reception(ctx)
	if err != nil {
		t.Fatalf("Reception: %v", err)
	}
	we, err := f.Weather(ctx)
	if err != nil {
		t.Fatalf("Weather: %v", err)
	}
	ve, err := f.Verification(ctx)
	if err != nil {
		t.Fatalf("Verification: %v", err)
	}
	nc, err := f.Nowcast(ctx)
	if err != nil {
		t.Fatalf("Nowcast: %v", err)
	}
	for name, v := range map[string]any{"traffic": tr, "reception": re, "weather": we, "verification": ve, "nowcast": nc} {
		if v == nil {
			t.Fatalf("%s is nil", name)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s does not encode: %v", name, err)
		}
		if strings.Contains(string(b), "NaN") || strings.Contains(string(b), "Inf") {
			t.Fatalf("%s JSON contains NaN/Inf", name)
		}
	}
	return tr, re, we, ve, nc
}

// ---------------------------------------------------------------- graceful degradation

func TestEmptyStore(t *testing.T) {
	f, _ := newTest(t, seedOpts{})
	tr, re, we, ve, nc := products(t, f)
	if tr.Level != LevelNone || len(tr.Hours) != HorizonHours || tr.Confidence != ConfLow {
		t.Errorf("traffic: level %q hours %d conf %q", tr.Level, len(tr.Hours), tr.Confidence)
	}
	if tr.Hours[0].Aircraft.Value.Valid() {
		t.Errorf("traffic value without data: %v", tr.Hours[0].Aircraft.Value)
	}
	if re.Level != LevelPhysics || len(re.Hours) != HorizonHours || re.Confidence != ConfLow {
		t.Errorf("reception: level %q hours %d conf %q", re.Level, len(re.Hours), re.Confidence)
	}
	h0 := re.Hours[0]
	if h0.Source != "climatology" || h0.Index != 50 || h0.Label != LabelNormal || !h0.P95Km.Value.Valid() {
		t.Errorf("reception hour 0: %+v", h0)
	}
	if p := h0.P95Km.Value.Float(); p < 150 || p > 500 {
		t.Errorf("physics-only p95 = %v km", p)
	}
	if re.Skill != nil || re.Model != nil {
		t.Errorf("skill / model without data")
	}
	if we.Level != LevelNone || we.Zambretti != nil || we.Analogue != nil {
		t.Errorf("weather: %+v", we.Meta)
	}
	if len(ve.Models) != 6 {
		t.Fatalf("verification models = %d", len(ve.Models))
	}
	for _, m := range ve.Models {
		if m.Status != StatusInsufficient || m.Needs == "" {
			t.Errorf("model %s/%s: status %q needs %q", m.Product, m.Target, m.Status, m.Needs)
		}
	}
	if nc.Index != 50 || nc.Sentence == "" || nc.Trend != TrendStable {
		t.Errorf("nowcast: %+v", nc)
	}
}

func TestShortHistory(t *testing.T) {
	// Five hours of everything: physics-only answers, flagged low confidence.
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Seed two days, then pretend only the last five hours exist by using
	// a station whose history starts five hours ago.
	ctx := context.Background()
	cur := floorHour(testNow.Unix())
	b := store.NewBatch()
	var obs, fc []store.Weather
	for k := 5; k >= 1; k-- {
		obs = append(obs, synthWeather(cur-int64(k)*3600))
	}
	for k := 0; k < 48; k++ {
		w := synthWeather(cur + int64(k)*3600)
		w.Issued = testNow.Unix()
		fc = append(fc, w)
	}
	b.AddWeatherObs(obs)
	b.SetWeatherForecast(fc)
	if err := st.Apply(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		for k := 5; k >= 1; k-- {
			ts := cur - int64(k)*3600
			if _, err := tx.Exec(`INSERT INTO stats_hour (t, ant, frames, aircraft, minutes) VALUES (?, '', 720000, 30, 60)`, ts); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO range_hour (t, ant, max_km, p95_km, positions, aircraft) VALUES (?, '', 380, 310, 500, 30)`, ts); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	zero := 0
	f := NewWithOptions(st, testConfig, Options{Now: func() time.Time { return testNow }, UTCOffsetSec: &zero})
	tr, re, we, ve, nc := products(t, f)
	if tr.Level != LevelPhysics || tr.Confidence != ConfLow || !tr.Hours[0].Aircraft.Value.Valid() {
		t.Errorf("traffic: level %q conf %q value %v", tr.Level, tr.Confidence, tr.Hours[0].Aircraft.Value)
	}
	if v := tr.Hours[0].Aircraft.Value.Float(); math.Abs(v-30) > 1 {
		t.Errorf("traffic with 5 h of data = %v, want about 30", v)
	}
	if re.Level != LevelPhysics || re.Confidence != ConfLow || re.Skill != nil {
		t.Errorf("reception: %+v", re.Meta)
	}
	if re.WeatherHours != 48 || re.Hours[0].Source != "forecast" || re.Hours[60].Source != "climatology" {
		t.Errorf("weather hours %d, sources %q %q", re.WeatherHours, re.Hours[0].Source, re.Hours[60].Source)
	}
	if len(re.Notes) == 0 || !strings.Contains(strings.Join(re.Notes, "|"), "needs") {
		t.Errorf("reception notes: %v", re.Notes)
	}
	if we.Current == nil || we.Zambretti == nil || len(we.Hours) != 48 || we.Analogue != nil {
		t.Errorf("weather: current %v zambretti %v hours %d", we.Current, we.Zambretti, len(we.Hours))
	}
	for _, m := range ve.Models {
		if m.Skill != nil {
			t.Errorf("skill with 5 h of data: %s/%s", m.Product, m.Target)
		}
	}
	if nc.Level != LevelPhysics || nc.Confidence != ConfLow {
		t.Errorf("nowcast: %+v", nc)
	}
}

// ---------------------------------------------------------------- learned models

func TestLearnedForecasts(t *testing.T) {
	f, st := newTest(t, seedOpts{days: 56, stats: true, weather: true, forecast: true, fcLog: true})
	start := time.Now()
	tr, re, we, ve, nc := products(t, f)
	t.Logf("first refresh (load + fit + backtests + build) took %v", time.Since(start))
	cur := floorHour(testNow.Unix())

	// --- traffic: learned, beats both baselines, tracks the planted pattern.
	if tr.Level != LevelLearned || tr.Skill == nil {
		t.Fatalf("traffic level %q skill %v", tr.Level, tr.Skill)
	}
	t.Logf("traffic skill: %+v", *tr.Skill)
	if s := tr.Skill.VsSeasonalNaive.Float(); !(s > 0.05) {
		t.Errorf("traffic does not beat seasonal-naive: skill %v", s)
	}
	if s := tr.Skill.VsPersistence.Float(); !(s > 0.3) {
		t.Errorf("traffic does not beat persistence: skill %v", s)
	}
	if tr.MsgRateSkill == nil || !(tr.MsgRateSkill.VsSeasonalNaive.Float() > 0) {
		t.Errorf("message-rate skill: %+v", tr.MsgRateSkill)
	}
	var se float64
	inside := 0
	for k, h := range tr.Hours {
		truth := trueAircraft(cur + int64(k)*3600)
		se += sq(h.Aircraft.Value.Float() - truth)
		if truth >= h.Aircraft.Lo.Float() && truth <= h.Aircraft.Hi.Float() {
			inside++
		}
		if !(h.Aircraft.Lo.Float() <= h.Aircraft.Value.Float() && h.Aircraft.Value.Float() <= h.Aircraft.Hi.Float()) {
			t.Fatalf("hour %d: interval %v..%v does not contain %v", k, h.Aircraft.Lo, h.Aircraft.Hi, h.Aircraft.Value)
		}
	}
	if rmse := math.Sqrt(se / HorizonHours); rmse > 3 {
		t.Errorf("traffic forecast RMSE against the planted pattern = %.2f aircraft", rmse)
	}
	if inside < HorizonHours*8/10 {
		t.Errorf("only %d of %d true values inside the interval", inside, HorizonHours)
	}
	if len(tr.Busiest) != 5 {
		t.Fatalf("busiest = %d", len(tr.Busiest))
	}
	if hr := time.UnixMilli(tr.Busiest[0].T).UTC().Hour(); hr < 10 || hr > 17 {
		t.Errorf("busiest hour at %02d:00, planted peak is around 13:30", hr)
	}
	if wd := time.UnixMilli(tr.Busiest[0].T).UTC().Weekday(); wd == time.Saturday || wd == time.Sunday {
		t.Errorf("busiest hour on a weekend")
	}

	// --- reception: learned model recovers the planted dependence.
	if re.Model == nil || re.Skill == nil {
		t.Fatalf("reception model %v skill %v", re.Model, re.Skill)
	}
	t.Logf("reception skill: %+v", *re.Skill)
	t.Logf("reception model: n=%d weight=%.2f R2=%v gradient effect=%v km", re.Model.TrainingHours, re.Model.Weight, re.Model.R2, re.Model.GradientEffectKm)
	if re.Level != LevelLearned {
		t.Errorf("reception level %q", re.Level)
	}
	// Planted: 0.5 km per N/km, i.e. +40 km for -40 -> -120 N/km.
	if g := re.Model.GradientEffectKm.Float(); !(g > 25 && g < 55) {
		t.Errorf("gradient effect = %v km, planted +40", g)
	}
	for _, c := range re.Model.Coefficients {
		if c.Feature == "dndh" && !(c.PerUnit.Float() < 0) {
			t.Errorf("dN/dh coefficient %v: sign not recovered (steeper negative gradient must give more range)", c.PerUnit)
		}
		if c.Feature == "traffic" && !(c.PerUnit.Float() > 0) {
			t.Errorf("traffic coefficient %v: sign not recovered", c.PerUnit)
		}
	}
	if s := re.Skill.VsPersistence.Float(); !(s > 0.2) {
		t.Errorf("reception does not beat persistence: %v", s)
	}
	if s := re.Skill.VsSeasonalNaive.Float(); !(s > 0.2) {
		t.Errorf("reception does not beat seasonal-naive: %v", s)
	}
	se = 0
	for k, h := range re.Hours {
		if h.Source != "forecast" {
			t.Fatalf("hour %d source %q", k, h.Source)
		}
		se += sq(h.P95Km.Value.Float() - trueP95(cur+int64(k)*3600))
		if h.Label == "" || h.Summary == "" || h.Index < 0 || h.Index > 100 {
			t.Fatalf("hour %d: %+v", k, h)
		}
		if p := h.FarProbability.Float(); !(p >= 0 && p <= 1) {
			t.Fatalf("hour %d far probability %v", k, p)
		}
	}
	if rmse := math.Sqrt(se / HorizonHours); rmse > 12 {
		t.Errorf("reception forecast RMSE against the planted truth = %.1f km", rmse)
	}
	// The steepest-gradient hour must read better than the flattest one.
	lo, hi := 0, 0
	for k := range re.Hours {
		if re.Hours[k].Atmosphere.DNDH < re.Hours[lo].Atmosphere.DNDH {
			lo = k
		}
		if re.Hours[k].Atmosphere.DNDH > re.Hours[hi].Atmosphere.DNDH {
			hi = k
		}
	}
	if re.Hours[lo].Index <= re.Hours[hi].Index {
		t.Errorf("index %d at dN/dh %.0f is not above %d at dN/dh %.0f", re.Hours[lo].Index, re.Hours[lo].Atmosphere.DNDH, re.Hours[hi].Index, re.Hours[hi].Atmosphere.DNDH)
	}
	if re.Best == nil || len(re.Bands) != 7 {
		t.Errorf("best %v bands %d", re.Best, len(re.Bands))
	}

	// --- weather: the planted +1.5 K forecast bias is found and removed.
	found := false
	for _, b := range we.Bias {
		if b.Variable == "tempC" && b.LeadFrom == 1 {
			found = true
			if math.Abs(b.Bias.Float()-1.5) > 0.15 || !b.Applied {
				t.Errorf("temperature bias row: %+v", b)
			}
			if !(b.MAECorrected.Float() < b.MAEHoldout.Float()) {
				t.Errorf("correction does not help on the hold-out: %+v", b)
			}
		}
		if b.Variable == "pressureMsl" && b.Applied {
			t.Errorf("pressure has no planted bias but a correction is applied: %+v", b)
		}
	}
	if !found {
		t.Fatalf("no temperature bias row in %+v", we.Bias)
	}
	h3 := we.Hours[3]
	if d := h3.TempRawC.Float() - h3.TempC.Float(); math.Abs(d-1.5) > 0.3 || !h3.Corrected {
		t.Errorf("bias correction at lead 3 = %v K", d)
	}
	if we.Level != LevelLearned || we.Zambretti == nil || we.Current == nil || we.Analogue == nil {
		t.Fatalf("weather: level %q zambretti %v current %v analogue %v", we.Level, we.Zambretti, we.Current, we.Analogue)
	}
	if len(we.Analogue.Hours) != analogueH || len(we.Analogue.Analogues) < 3 || len(we.Analogue.Verification) == 0 {
		t.Errorf("analogue: %d hours, %d analogues, %d verification rows", len(we.Analogue.Hours), len(we.Analogue.Analogues), len(we.Analogue.Verification))
	}
	for _, s := range we.Analogue.Verification {
		if s.Variable == "tempC" && s.LeadH == 6 {
			t.Logf("analogue temp +6 h: MAE %v vs persistence %v, Open-Meteo %v (n=%d)", s.AnalogueMAE, s.PersistenceMAE, s.OpenMeteoMAE, s.NOpenMeteo)
			if !(s.VsPersistence.Float() > 0) {
				t.Errorf("analogue temperature forecast does not beat persistence at 6 h: %+v", s)
			}
			if s.NOpenMeteo == 0 || math.Abs(s.OpenMeteoMAE.Float()-1.5) > 0.3 {
				t.Errorf("Open-Meteo comparison: %+v", s)
			}
		}
	}

	// --- verification report.
	byTarget := map[string]ModelReport{}
	for _, m := range ve.Models {
		byTarget[m.Target] = m
	}
	for _, tg := range []string{"aircraft", "msgRate", "p95Km"} {
		m := byTarget[tg]
		if m.Status != StatusOK || m.Skill == nil || len(m.Series) == 0 {
			t.Errorf("report %s: status %q skill %v series %d", tg, m.Status, m.Skill, len(m.Series))
		}
	}
	if len(byTarget["aircraft"].ByLead) != 7 {
		t.Errorf("by-lead errors: %+v", byTarget["aircraft"].ByLead)
	}
	if byTarget["bias"].Status != StatusOK {
		t.Errorf("bias report: %+v", byTarget["bias"])
	}

	// --- nowcast.
	if nc.Index != re.Hours[0].Index || nc.Label != re.Hours[0].Label || len(nc.Next6h) != 7 || nc.Sentence == "" {
		t.Errorf("nowcast: %+v", nc)
	}

	// --- persistence: models are in kv and a new forecaster starts from them.
	raw, ok, err := st.KVGet(context.Background(), KVModels)
	if err != nil || !ok || len(raw) < 100 {
		t.Fatalf("models not persisted: ok=%v err=%v len=%d", ok, err, len(raw))
	}
	t.Logf("persisted models: %d bytes", len(raw))
	zero := 0
	f2 := NewWithOptions(st, testConfig, Options{Now: func() time.Time { return testNow.Add(2 * time.Minute) }, UTCOffsetSec: &zero})
	start = time.Now()
	tr2, err := f2.Traffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("restart refresh from persisted models took %v", time.Since(start))
	if f2.Status().Fitted != f.Status().Fitted {
		t.Errorf("restart refitted the models: %d vs %d", f2.Status().Fitted, f.Status().Fitted)
	}
	if tr2.Hours[10].Aircraft.Value != tr.Hours[10].Aircraft.Value || tr2.Skill == nil {
		t.Errorf("forecast from persisted models differs: %v vs %v", tr2.Hours[10].Aircraft.Value, tr.Hours[10].Aircraft.Value)
	}

	// --- issued forecasts are logged for live verification.
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM predict_forecast_log`).Scan(&n); err != nil || n != 4*len(logLeads) {
		t.Errorf("forecast log rows = %d, err %v", n, err)
	}
}

func TestLiveVerification(t *testing.T) {
	f, st := newTest(t, seedOpts{days: 20, stats: true, weather: true, forecast: true})
	ctx := context.Background()
	// Forecasts "issued" a day ago for hours that have since been measured.
	cur := floorHour(testNow.Unix())
	err := st.Write(ctx, func(tx *sql.Tx) error {
		for k := 1; k <= 20; k++ {
			ts := cur - int64(k)*3600
			if _, err := tx.Exec(`INSERT INTO predict_forecast_log (product, t, lead_h, issued, value, lo, hi) VALUES (?,?,?,?,?,?,?)`,
				logAircraft, ts, 24, ts-86400, trueAircraft(ts)+2, trueAircraft(ts)-10, trueAircraft(ts)+14); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ve, err := f.Verification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ve.Live) != 1 {
		t.Fatalf("live rows: %+v", ve.Live)
	}
	l := ve.Live[0]
	if l.Product != logAircraft || l.LeadH != 24 || l.N != 20 || math.Abs(l.Bias.Float()-2) > 2 || !(l.Coverage.Float() > 0.8) {
		t.Errorf("live skill: %+v", l)
	}
}

// ---------------------------------------------------------------- leakage

func TestBacktestsDoNotLookAhead(t *testing.T) {
	// Two stations identical except for the measurements of the last 24
	// complete hours. Every backtest forecast for those hours must be the
	// same in both: the forecasts may not depend on what they are scored
	// against (or on anything later).
	run := func(poison int) *models {
		f, _ := newTest(t, seedOpts{days: 30, stats: true, weather: true, poisonLast: poison})
		if err := f.Refit(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.models
	}
	a, b := run(0), run(24)
	cut := (floorHour(testNow.Unix()) - 24*3600) * 1000
	check := func(name string, x, y []VerifyPoint) {
		t.Helper()
		if len(x) == 0 || len(x) != len(y) {
			t.Fatalf("%s: series lengths %d / %d", name, len(x), len(y))
		}
		compared, differ := 0, 0
		for i := range x {
			if x[i].T != y[i].T {
				t.Fatalf("%s: time mismatch at %d", name, i)
			}
			// Only forecasts for poisoned hours whose origin lies at or
			// before the start of the poisoned span: a later origin may
			// (rightly) have learned from the poisoned hours before it.
			if x[i].T < cut || x[i].T-int64(x[i].LeadH-1)*3600000 > cut {
				continue
			}
			compared++
			if x[i].Actual != y[i].Actual {
				differ++
			}
			if x[i].Forecast != y[i].Forecast {
				t.Fatalf("%s: forecast for %s changed with its own actual: %v vs %v", name,
					time.UnixMilli(x[i].T).UTC().Format(time.RFC3339), x[i].Forecast, y[i].Forecast)
			}
		}
		if compared == 0 || differ == 0 {
			t.Fatalf("%s: test is vacuous (compared %d, actuals differing %d)", name, compared, differ)
		}
	}
	check("aircraft", a.AircraftSeries, b.AircraftSeries)
	check("msgRate", a.MsgRateSeries, b.MsgRateSeries)
	check("reception", a.ReceptionSeries, b.ReceptionSeries)
}

func TestSeriesForecastUsesOnlyHistory(t *testing.T) {
	n := 24 * 21
	y := make([]float64, n)
	sW, sD := make([]int, n+HorizonHours), make([]int, n+HorizonHours)
	for i := range sW {
		sW[i], sD[i] = i%168, i%24
	}
	for i := range y {
		y[i] = 10 + 5*math.Sin(2*math.Pi*float64(i)/24)
	}
	o := n - 100
	m1 := fitSeries(y[:o], sW, sD)
	f1 := m1.forecast(y[:o], sW, sD, HorizonHours)
	for i := o; i < n; i++ {
		y[i] = 1e6 // the future changes completely
	}
	m2 := fitSeries(y[:o], sW, sD)
	f2 := m2.forecast(y[:o], sW, sD, HorizonHours)
	for k := range f1 {
		if f1[k] != f2[k] {
			t.Fatalf("lead %d: %v vs %v", k+1, f1[k], f2[k])
		}
		want := 10 + 5*math.Sin(2*math.Pi*float64(o+k)/24)
		if math.Abs(f1[k]-want) > 0.01 {
			t.Fatalf("lead %d: %v, want %v", k+1, f1[k], want)
		}
	}
}

func TestAnalogueSearchRespectsLimit(t *testing.T) {
	f, _ := newTest(t, seedOpts{days: 30, weather: true})
	h, err := f.loadHistory(context.Background(), testNow, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	ix := newAnalogueIndex(h, f.rx())
	for _, o := range []int{300, 500, h.n - 1} {
		hits, cand := ix.find(o, o, analogueK)
		if len(hits) < 3 || cand == 0 {
			t.Fatalf("origin %d: %d hits of %d candidates", o, len(hits), cand)
		}
		for i, c := range hits {
			if c.j+analogueH > o {
				t.Errorf("origin %d: analogue %d ends at %d, after the origin", o, c.j, c.j+analogueH)
			}
			for _, d := range hits[:i] {
				if dj := d.j - c.j; dj > -analogueSep && dj < analogueSep {
					t.Errorf("analogues %d and %d closer than %d h", d.j, c.j, analogueSep)
				}
			}
		}
	}
}

// ---------------------------------------------------------------- weather patterns

func TestPressureFallAndRisks(t *testing.T) {
	f, _ := newTest(t, seedOpts{
		days: 14, weather: true, forecast: true,
		mutate: func(hoursAgo int, w *store.Weather) {
			// Pressure collapsing 1.5 hPa per hour over the last 12 hours,
			// wind backing from west to south.
			if hoursAgo <= 12 {
				w.PressureMSL = 1015 - 1.5*float64(12-hoursAgo)
				w.WindKmh = 25
				w.WindDir = 180 + 7.5*float64(hoursAgo)
			} else {
				w.PressureMSL = 1015
			}
		},
		fcMut: func(lead int, w *store.Weather) {
			if lead >= 12 && lead <= 16 { // 00:00-04:00 UTC: saturated, calm, clear
				w.TempC, w.DewpointC, w.WindKmh, w.Cloud, w.IsDay = 8, 7.8, 3, 5, 0
			}
			if lead == 4 {
				w.CAPE = 1900
			}
		},
	})
	_, _, we, _, _ := products(t, f)
	if we.Current == nil || we.Zambretti == nil {
		t.Fatal("no current conditions / Zambretti")
	}
	if c := we.Current.Change3h.Float(); math.Abs(c+4.5) > 0.6 {
		t.Errorf("Kalman tendency = %v hPa/3 h, planted -4.5", c)
	}
	if we.Current.Tendency != TendencyFallingRapidly {
		t.Errorf("tendency %q", we.Current.Tendency)
	}
	if p6 := we.Current.Pressure6h.Value.Float(); math.Abs(p6-(997-9)) > 2.5 {
		t.Errorf("6 h pressure extrapolation = %v", p6)
	}
	z := we.Zambretti
	if z.Trend != TrendFalling || z.Season != "summer" || z.Hemisphere != "north" || z.Wind != "S" {
		t.Errorf("zambretti: %+v", z)
	}
	// 997 hPa, S wind (-12), summer + falling (-7) -> 978 -> step 6 -> Z.
	if z.Letter != "Z" || !strings.Contains(z.Outlook, "approaching front") {
		t.Errorf("zambretti letter %q outlook %q", z.Letter, z.Outlook)
	}
	kinds := map[string]Pattern{}
	for _, p := range we.Patterns {
		kinds[p.Kind] = p
	}
	if p, ok := kinds[PatternFrontNear]; !ok || p.Severity < 2 {
		t.Errorf("no approaching-front pattern: %+v", we.Patterns)
	}
	if p, ok := kinds[PatternPressure]; !ok || p.Severity != 2 {
		t.Errorf("pressure pattern: %+v", p)
	}
	if p, ok := kinds[PatternFog]; !ok || p.Probability.Float() < 0.7 || p.From == 0 {
		t.Errorf("fog pattern: %+v", p)
	}
	if _, ok := kinds[PatternInversion]; !ok {
		t.Errorf("no inversion pattern: %+v", we.Patterns)
	}
	if p, ok := kinds[PatternConvective]; !ok || p.Probability.Float() < 0.9 {
		t.Errorf("convective pattern: %+v", p)
	}
	if we.Hours[13].FogRisk < 0.7 || we.Hours[30].FogRisk > 0.3 {
		t.Errorf("fog risk %v at the planted hour, %v elsewhere", we.Hours[13].FogRisk, we.Hours[30].FogRisk)
	}
}

func TestFrontalPassage(t *testing.T) {
	f, _ := newTest(t, seedOpts{
		days: 14, weather: true,
		mutate: func(hoursAgo int, w *store.Weather) {
			if hoursAgo > 30 {
				return
			}
			// Cold front eight hours ago: V-shaped pressure, wind veering
			// SW -> NW, temperature dropping 6 K, no diurnal cycle.
			d := float64(hoursAgo - 8)
			w.PressureMSL = 1002 + 1.2*math.Abs(d)
			w.WindKmh = 30
			if d > 0 {
				w.WindDir, w.TempC = 210, 16
			} else {
				w.WindDir, w.TempC = 310, 10
			}
			w.DewpointC = w.TempC - 4
		},
	})
	_, _, we, _, _ := products(t, f)
	var front *Pattern
	for i := range we.Patterns {
		if we.Patterns[i].Kind == PatternFrontPast {
			front = &we.Patterns[i]
		}
	}
	if front == nil {
		t.Fatalf("frontal passage not detected: %+v", we.Patterns)
	}
	if !strings.HasPrefix(front.Title, "Cold front passed about") || !strings.Contains(front.Detail, "veered 100") {
		t.Errorf("front: %q / %q", front.Title, front.Detail)
	}
	ago := (floorHour(testNow.Unix())*1000 - front.From) / 3600000
	if ago < 6 || ago > 10 {
		t.Errorf("front located %d h ago, planted 8", ago)
	}
	for _, p := range we.Patterns {
		if p.Kind == PatternFrontNear {
			t.Errorf("approaching front reported while pressure rises: %+v", p)
		}
	}
}

// ---------------------------------------------------------------- Zambretti

func TestZambretti(t *testing.T) {
	calm := math.NaN()
	cases := []struct {
		name string
		in   ZambrettiInput
		want string // letter
		text string
	}{
		// Cases on which the published formula variant (Z = 144 - 0.13 P
		// steady, 127 - 0.12 P falling, 185 - 0.16 P rising; winter, no
		// wind) and the digitised instrument scales agree.
		{"high steady", ZambrettiInput{PressureMSL: 1033, Trend: TrendSteady, WindDir: calm, Month: time.January}, "A", "Settled fine"},
		{"1000 steady", ZambrettiInput{PressureMSL: 1000, Trend: TrendSteady, WindDir: calm, Month: time.January}, "N", "Showery, bright intervals"},
		{"1008 steady", ZambrettiInput{PressureMSL: 1008, Trend: TrendSteady, WindDir: calm, Month: time.January}, "K", "Fairly fine, showers likely"},
		{"985 falling", ZambrettiInput{PressureMSL: 985, Trend: TrendFalling, WindDir: calm, Month: time.January}, "Z", "Stormy, much rain"},
		{"990 rising", ZambrettiInput{PressureMSL: 990, Trend: TrendRising, WindDir: calm, Month: time.January}, "L", "Rather unsettled, clearing later"},
		{"1040 rising", ZambrettiInput{PressureMSL: 1040, Trend: TrendRising, WindDir: calm, Month: time.January}, "A", "Settled fine"},
		{"1045 falling", ZambrettiInput{PressureMSL: 1045, Trend: TrendFalling, WindDir: calm, Month: time.January}, "A", "Settled fine"},
		{"960 steady", ZambrettiInput{PressureMSL: 960, Trend: TrendSteady, WindDir: calm, Month: time.January}, "Z", "Stormy, much rain"},
		// Wind: a southerly lowers the reading by 12 hPa, a northerly raises
		// it by 6: 1005 falling -> 993 -> step 9 -> X; 1005 -> 1011 -> step 13 -> O.
		{"1005 falling S", ZambrettiInput{PressureMSL: 1005, Trend: TrendFalling, WindDir: 180, Month: time.January}, "X", "Rain, very unsettled"},
		{"1005 falling N", ZambrettiInput{PressureMSL: 1005, Trend: TrendFalling, WindDir: 0, Month: time.January}, "O", "Showery, becoming less settled"},
		// Southern hemisphere mirrors the wind: a northerly there acts like
		// a southerly here (January is summer there: a further -7).
		{"1005 falling N south", ZambrettiInput{PressureMSL: 1005, Trend: TrendFalling, WindDir: 0, Month: time.July, Southern: true}, "X", "Rain, very unsettled"},
		{"1005 falling N south summer", ZambrettiInput{PressureMSL: 1005, Trend: TrendFalling, WindDir: 0, Month: time.January, Southern: true}, "Z", "Stormy, much rain"},
		// Season: summer + rising adds 7 hPa: 1000 -> step 11 (G) in winter,
		// 1007 -> step 12 (F) in summer.
		{"1000 rising winter", ZambrettiInput{PressureMSL: 1000, Trend: TrendRising, WindDir: calm, Month: time.December}, "G", "Fairly fine, possible showers early"},
		{"1000 rising summer", ZambrettiInput{PressureMSL: 1000, Trend: TrendRising, WindDir: calm, Month: time.July}, "F", "Fairly fine, improving"},
		// Trend derived from the 3 h change (threshold 1.6 hPa).
		{"change -2", ZambrettiInput{PressureMSL: 985, Change3h: -2, WindDir: calm, Month: time.January}, "Z", "Stormy, much rain"},
		{"change -1", ZambrettiInput{PressureMSL: 1000, Change3h: -1, WindDir: calm, Month: time.January}, "N", "Showery, bright intervals"},
		{"change +1.6", ZambrettiInput{PressureMSL: 990, Change3h: 1.6, WindDir: calm, Month: time.January}, "L", "Rather unsettled, clearing later"},
	}
	for _, c := range cases {
		got, ok := Zambretti(c.in)
		if !ok || got.Letter != c.want || got.Text != c.text {
			t.Errorf("%s: got %s %q (ok=%v), want %s %q", c.name, got.Letter, got.Text, ok, c.want, c.text)
		}
	}
	// The three scales run monotonically from stormy to settled and use
	// exactly the letters of the instrument.
	for name, tc := range map[string]struct {
		scale   [22]int
		letters string
	}{
		"rising":  {zRise, "ABCFGIJLMQTYZ"},
		"steady":  {zSteady, "ABEKNPSWXZ"},
		"falling": {zFall, "ABDHORUVXZ"},
	} {
		seen := map[byte]bool{}
		for i, v := range tc.scale {
			if i > 0 && v > tc.scale[i-1] {
				t.Errorf("%s scale not monotonic at step %d", name, i)
			}
			seen[byte('A'+v)] = true
		}
		for l := byte('A'); l <= 'Z'; l++ {
			if seen[l] != strings.ContainsRune(tc.letters, rune(l)) {
				t.Errorf("%s scale: letter %c present=%v", name, l, seen[l])
			}
		}
	}
	if r, ok := Zambretti(ZambrettiInput{PressureMSL: 940, Trend: TrendSteady, WindDir: calm}); !ok || !r.Exceptional || r.Letter != "Z" {
		t.Errorf("below scale: %+v", r)
	}
	if _, ok := Zambretti(ZambrettiInput{PressureMSL: math.NaN()}); ok {
		t.Error("NaN pressure accepted")
	}
	if CompassPoint(348.8) != "N" || CompassPoint(11.3) != "NNE" || CompassPoint(-90) != "W" || CompassPoint(math.NaN()) != "" {
		t.Error("CompassPoint")
	}
	if ClassifyTendency(-4) != TendencyFallingRapidly || ClassifyTendency(0.2) != TendencySteady || ClassifyTendency(7) != TendencyRisingVRapidly {
		t.Error("ClassifyTendency")
	}
	if p := ReduceToMSL(954.6, 500, 15); math.Abs(p-1013.2) > 1 {
		t.Errorf("ReduceToMSL = %v", p)
	}
}

// ---------------------------------------------------------------- atmosphere

func hourWithProfile(surfT, surfRH float64, levels [][4]float64) *weather.HourPoint {
	h := &weather.HourPoint{T: testNow.UnixMilli() / 3600000 * 3600000}
	// Everything missing unless set.
	h.Surface = weather.Surface{TempC: weather.Num(surfT), RH: weather.Num(surfRH), DewPointC: weather.Num(radio.DewPointFromRH(surfT, surfRH)),
		ApparentC: weather.NaN, PrecipMM: weather.NaN, RainMM: weather.NaN, ShowersMM: weather.NaN, SnowfallCM: weather.NaN,
		WeatherCode: weather.NaN, Cloud: weather.NaN, CloudLow: weather.NaN, CloudMid: weather.NaN, CloudHigh: weather.NaN,
		PressureMSL: 1015, SurfacePressure: 1013, WindKmh: 8, WindDir: 200, GustKmh: weather.NaN, VisibilityM: weather.NaN,
		IsDay: 0, CAPE: weather.NaN}
	h.PrecipProb, h.BoundaryLayerM, h.CIN, h.FreezingLevelM = weather.NaN, weather.NaN, weather.NaN, weather.NaN
	h.LiftedIndex = -3
	for _, l := range levels {
		h.Levels = append(h.Levels, weather.Level{HPa: l[0], HeightM: weather.Num(l[1]), TempC: weather.Num(l[2]), RH: weather.Num(l[3]),
			WindKmh: weather.NaN, WindDir: weather.NaN})
	}
	return h
}

func TestAtmosphereFromProfile(t *testing.T) {
	// Well-mixed standard-like profile: about -6.5 K/km, humidity falling slowly.
	std := hourWithProfile(15, 60, [][4]float64{
		{1000, 110, 14.3, 60}, {975, 320, 12.9, 60}, {950, 540, 11.5, 58}, {925, 760, 10.1, 56}, {900, 990, 8.6, 55}, {850, 1460, 5.5, 50}, {800, 1950, 2.3, 45},
	})
	row := RowFromHour(std, 10)
	if row.T != std.T/1000 || !strings.Contains(row.ProfileJSON, `"levels"`) || !strings.Contains(row.ProfileJSON, `"liftedIndex":-3`) {
		t.Fatalf("row: T=%d profile=%s", row.T, row.ProfileJSON)
	}
	a := AnalyzeWeather(&row, 10)
	if !a.Valid || !a.HasProfile || a.Estimated {
		t.Fatalf("standard profile: %+v", a)
	}
	if a.DNDH > -25 || a.DNDH < -65 || a.Class != radio.ClassStandard || a.Ducts != 0 || a.Index < 42 || a.Index > 62 {
		t.Errorf("standard profile: dN/dh %.1f class %q ducts %d index %d", a.DNDH, a.Class, a.Ducts, a.Index)
	}
	if math.Abs(row.DNDH-a.DNDH) > 0.06 || row.Duct != 0 || math.Abs(row.NSurface-a.NSurface) > 0.06 {
		t.Errorf("row columns: dn_dh %v duct %v n %v", row.DNDH, row.Duct, row.NSurface)
	}
	// The 1000 hPa level 100 m above the station is skipped only when it is
	// closer than minLevelSepM; here it is exactly at the limit.

	// Strong surface-based inversion with a dry layer on top: a duct.
	duct := hourWithProfile(12, 95, [][4]float64{
		{1000, 110, 12.5, 93}, {975, 320, 19, 30}, {950, 540, 18, 25}, {925, 760, 16.5, 25}, {900, 990, 15, 25}, {850, 1460, 12, 25}, {800, 1950, 9, 25},
	})
	row = RowFromHour(duct, 10)
	a = AnalyzeWeather(&row, 10)
	if !a.Valid || !a.HasProfile || a.Ducts == 0 || !a.Ducting1090 || a.Class != radio.ClassDucting || a.Index < 80 {
		t.Fatalf("ducting profile: %+v", a)
	}
	if a.MinGradient > -157 || a.DuctStrengthM <= 0 || row.Duct <= 0 || !(a.InversionC.Float() > 5) {
		t.Errorf("ducting profile: min gradient %.0f strength %.1f column %v inversion %v", a.MinGradient, a.DuctStrengthM, row.Duct, a.InversionC)
	}
	// Physics: the ducting hour must promise more range than the standard one.
	ph := newPhysics(15)
	sa := standardAtmos()
	if !(ph.range95(0.78, &a, 40) > ph.range95(0.78, &sa, 40)*1.05) || !(ph.rangeMax(0.95, &a, 40) > ph.rangeMax(0.95, &sa, 40)*1.1) {
		t.Errorf("physics baseline does not grow with ducting")
	}
	if !(ph.range95(0.78, &sa, 0) < ph.range95(0.78, &sa, 40)) {
		t.Errorf("no traffic must mean less range")
	}
	if !(physFar(&a, 40) > 0.5 && physFar(&sa, 40) < 0.05 && physFar(&a, 0) == 0) {
		t.Errorf("far probability: duct %v standard %v no traffic %v", physFar(&a, 40), physFar(&sa, 40), physFar(&a, 0))
	}

	// Fallbacks: stored columns, then surface values only, then nothing.
	w := store.NewWeather(1)
	w.DNDH = -120
	if a := AnalyzeWeather(&w, 0); !a.Valid || a.HasProfile || a.Class != radio.ClassSuperRefraction || a.NSurface != 315 {
		t.Errorf("column fallback: %+v", a)
	}
	w = store.NewWeather(1)
	w.TempC, w.RH, w.PressureMSL = 20, 70, 1015
	if a := AnalyzeWeather(&w, 100); !a.Valid || !a.Estimated || a.DNDH > -30 || a.DNDH < -75 {
		t.Errorf("surface fallback: %+v", a)
	}
	w = store.NewWeather(1)
	if a := AnalyzeWeather(&w, 0); a.Valid {
		t.Errorf("empty row analysed: %+v", a)
	}
	if a := AnalyzeWeather(nil, 0); a.Valid {
		t.Error("nil row analysed")
	}
	// Bare-array profile documents are accepted, garbage is not.
	if d, ok := parseProfile(`[{"hpa":925,"t":10,"rh":50,"z":760}]`); !ok || len(d.Levels) != 1 || *d.Levels[0].Z != 760 {
		t.Errorf("array profile: %+v %v", d, ok)
	}
	if _, ok := parseProfile(`{"levels":`); ok {
		t.Error("broken JSON accepted")
	}
}

// ---------------------------------------------------------------- lifecycle

func TestRunStopsOnCancel(t *testing.T) {
	f, st := newTest(t, seedOpts{days: 3, stats: true, weather: true, forecast: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for f.Status().Built == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not build a snapshot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if _, ok, _ := st.KVGet(context.Background(), KVModels); !ok {
		t.Error("models not persisted by Run")
	}
	// Three days: a daily profile, flagged as still learning.
	tr, _, _, ve, _ := products(t, f)
	if tr.Level != LevelBlend || !strings.Contains(strings.Join(tr.Notes, "|"), "needs") {
		t.Errorf("traffic after 3 days: level %q notes %v", tr.Level, tr.Notes)
	}
	if ve.Models[0].Status == StatusOK {
		t.Errorf("aircraft model reported fully trained after 3 days")
	}
	f.Reset()
	if f.Status().Built != 0 {
		t.Error("Reset kept the snapshot")
	}
	if _, err := f.Nowcast(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalHistory(t *testing.T) {
	f, st := newTest(t, seedOpts{days: 10, stats: true, weather: true})
	ctx := context.Background()
	full, err := f.loadHistory(ctx, testNow, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	// One hour later, with one more hour of data.
	later := testNow.Add(time.Hour)
	ts := floorHour(testNow.Unix())
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO stats_hour (t, ant, frames, aircraft, minutes) VALUES (?, '', 360000, 77, 60)`, ts)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	inc, err := f.loadHistory(ctx, later, full, 5)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.loadHistory(ctx, later, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if inc.n != ref.n || inc.t0 != ref.t0 || inc.n != full.n+1 || inc.aircraft[inc.n-1] != 77 {
		t.Fatalf("incremental n=%d t0=%d, full reload n=%d t0=%d, last=%v", inc.n, inc.t0, ref.n, ref.t0, inc.aircraft[inc.n-1])
	}
	for i := 0; i < ref.n; i++ {
		same := func(a, b float64) bool { return a == b || (math.IsNaN(a) && math.IsNaN(b)) }
		if !same(inc.aircraft[i], ref.aircraft[i]) || !same(inc.p95[i], ref.p95[i]) || inc.has[i] != ref.has[i] ||
			inc.atm[i].Valid != ref.atm[i].Valid || !same(inc.atm[i].DNDH, ref.atm[i].DNDH) || !same(inc.wx[i].TempC, ref.wx[i].TempC) {
			t.Fatalf("hour %d differs between incremental and full load", i)
		}
	}
}
