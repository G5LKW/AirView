// Package predict forecasts what the station will receive: traffic levels,
// reception (propagation) conditions and the local weather, each with
// intervals, a confidence and - once there is enough history - a skill
// figure from rolling-origin backtesting.
//
// It reads the hourly history kept by the store (stats_hour, range_hour,
// weather_obs, weather_fc, weather_fc_log), combines the physics of
// internal/radio with models from internal/stat, and degrades gracefully:
// with hours of data the answers are physics / climatology only and flagged
// low-confidence, with weeks of data learned models take over.
//
// # Products
//
//	f := predict.New(st, holder.Get)
//	go f.Run(ctx)                  // refits hourly, refreshes every few minutes
//
//	f.Traffic(ctx)       *TrafficForecast    aircraft count and message rate, next 72 h
//	f.Reception(ctx)     *ReceptionForecast  p95 / max range, 0-100 index, far-reception probability
//	f.Weather(ctx)       *WeatherOutlook     bias-corrected forecast, Zambretti, patterns, analogues
//	f.Verification(ctx)  *Verification       backtest skill, forecast-versus-actual series, data sufficiency
//	f.Nowcast(ctx)       *Nowcast            one-line summary for the top bar
//
// All result types are JSON-ready (no NaN: unknown numbers encode as null);
// timestamps are Unix milliseconds. The getters are cheap: they return the
// cached snapshot and only recompute when it is older than a few minutes.
//
// # Models and persistence
//
// The fitted models (seasonal profiles, AR and ridge / logistic models as
// stat.Model, calibration, bias table, backtest results) are stored as one
// JSON document in the store's kv table under KVModels, so a restart serves
// forecasts at once. Issued forecasts are logged to the package's own table
// predict_forecast_log and verified against what was later measured (the
// "live" part of the verification report), which also covers the error of
// the weather forecast itself.
//
// The writer side of the weather columns this package reads lives here too:
// RowFromHour converts an Open-Meteo hour into a store.Weather row with
// n_surface, dn_dh, k_factor, duct and profile_json filled in.
package predict

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
	"airview/internal/radio"
	"airview/internal/stat"
	"airview/internal/store"
)

// KVModels is the kv key of the persisted models.
const KVModels = "predict.models.v1"

const modelsVersion = 1

const (
	snapshotTTL   = 5 * time.Minute // getters recompute after this
	refitInterval = time.Hour
	refitDelaySec = 150 // seconds into the hour before the hourly refit
	logKeepDays   = 120
)

func init() {
	if err := store.RegisterMigration("predict", 1, `
CREATE TABLE predict_forecast_log (
  product TEXT NOT NULL,
  t       INTEGER NOT NULL,
  lead_h  INTEGER NOT NULL,
  issued  INTEGER NOT NULL,
  value   REAL,
  lo      REAL,
  hi      REAL,
  PRIMARY KEY (product, t, lead_h)
) WITHOUT ROWID;`); err != nil {
		slog.Error("predict: registering migration", "err", err)
	}
}

// Options tunes a Forecaster; the zero value is right for production.
type Options struct {
	// Now replaces the clock (tests, simulations).
	Now func() time.Time
	// UTCOffsetSec fixes the local-time offset used for the hour-of-week
	// traffic profile. nil: the host's time zone when it has one, else the
	// zone implied by the station longitude.
	UTCOffsetSec *int
	// Ant selects the antenna whose statistics are forecast ("" = all
	// antennas combined, the default).
	Ant    string
	Logger *slog.Logger
}

// Forecaster produces the forecast products. It is safe for concurrent use.
type Forecaster struct {
	st  *store.Store
	cfg func() model.Config
	now func() time.Time
	off *int
	ant string
	log *slog.Logger

	mu     sync.Mutex // serialises refresh / refit
	hist   *history   // guarded by mu
	models *models    // guarded by mu
	loaded bool       // kv looked up; guarded by mu
	snap   atomic.Pointer[snapshot]
	lastEr atomic.Pointer[string]
}

// snapshot is one consistent set of products.
type snapshot struct {
	built     time.Time
	hour      int64
	traffic   *TrafficForecast
	reception *ReceptionForecast
	weather   *WeatherOutlook
	verify    *Verification
	nowcast   *Nowcast
	fitted    int64
}

// models is everything that is learned, as persisted in kv.
type models struct {
	Version   int             `json:"version"`
	Fitted    int64           `json:"fitted"` // Unix seconds
	Ant       string          `json:"ant"`
	Aircraft  *seriesModel    `json:"aircraft,omitempty"`
	MsgRate   *seriesModel    `json:"msgRate,omitempty"`
	Reception *receptionModel `json:"reception,omitempty"`
	Bias      []BiasRow       `json:"bias,omitempty"`

	AnalogueVerify []AnalogueSkill `json:"analogueVerify,omitempty"`
	// Chart series of the backtests.
	AircraftSeries  []VerifyPoint `json:"aircraftSeries,omitempty"`
	MsgRateSeries   []VerifyPoint `json:"msgRateSeries,omitempty"`
	ReceptionSeries []VerifyPoint `json:"receptionSeries,omitempty"`
	Live            []LiveSkill   `json:"live,omitempty"`
}

// rxInfo is the part of the configuration the forecasts depend on.
type rxInfo struct {
	located  bool
	lat, lon float64
	elevM    float64
	antM     float64 // highest antenna, m AMSL
}

// New returns a Forecaster reading from st. cfg supplies the current
// configuration (e.g. config.Holder.Get); nil means model.DefaultConfig.
func New(st *store.Store, cfg func() model.Config) *Forecaster {
	return NewWithOptions(st, cfg, Options{})
}

// NewWithOptions is New with explicit options.
func NewWithOptions(st *store.Store, cfg func() model.Config, opt Options) *Forecaster {
	if cfg == nil {
		cfg = model.DefaultConfig
	}
	f := &Forecaster{st: st, cfg: cfg, now: opt.Now, off: opt.UTCOffsetSec, ant: opt.Ant, log: opt.Logger}
	if f.now == nil {
		f.now = time.Now
	}
	if f.log == nil {
		f.log = slog.Default()
	}
	return f
}

func (f *Forecaster) rx() rxInfo {
	c := f.cfg()
	r := rxInfo{located: c.Receiver.Located, lat: c.Receiver.Lat, lon: c.Receiver.Lon, elevM: c.Receiver.GroundElevM}
	if !fin(r.lat) || !fin(r.lon) || math.Abs(r.lat) > 90 || math.Abs(r.lon) > 180 {
		r.located, r.lat, r.lon = false, 0, 0
	}
	if !r.located {
		r.lat, r.lon = 0, 0
	}
	if !fin(r.elevM) {
		r.elevM = 0
	}
	agl := 0.0
	for _, a := range c.Antennas {
		if f.ant != "" && a.ID != f.ant {
			continue
		}
		if h := radio.AntennaHeightAMSL(c.Receiver, a) - c.Receiver.GroundElevM; fin(h) && h > agl {
			agl = h
		}
	}
	if agl < 2 {
		agl = 2
	}
	r.antM = math.Max(r.elevM, 0) + agl
	return r
}

// utcOffset returns the local-time offset in seconds at Unix time t.
func (f *Forecaster) utcOffset(t int64) int {
	if f.off != nil {
		return *f.off
	}
	if name, off := time.Unix(t, 0).In(time.Local).Zone(); off != 0 || (name != "UTC" && name != "") {
		return off
	}
	if c := f.cfg(); c.Receiver.Located && fin(c.Receiver.Lon) {
		return int(math.Round(c.Receiver.Lon/15)) * hourSec
	}
	return 0
}

// ---------------------------------------------------------------- lifecycle

// Run keeps the forecasts fresh until ctx is cancelled: the models are
// loaded from kv (or fitted) at start, refitted once an hour shortly after
// the hour, and the products are rebuilt every five minutes so that a new
// weather forecast shows up promptly. It returns nil.
func (f *Forecaster) Run(ctx context.Context) error {
	if err := f.refresh(ctx, false, false); err != nil && ctx.Err() == nil {
		f.log.Warn("predict: initial forecast failed", "err", err)
	}
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		// Wake at the next five-minute boundary (plus a few seconds, so the
		// engine's hourly rollup has run).
		now := time.Now()
		next := now.Truncate(5 * time.Minute).Add(5*time.Minute + 20*time.Second)
		t.Reset(next.Sub(now))
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := f.refresh(ctx, false, false); err != nil && ctx.Err() == nil {
			f.log.Warn("predict: refresh failed", "err", err)
		}
	}
}

// Refit refits all models now, persists them and rebuilds the products.
func (f *Forecaster) Refit(ctx context.Context) error { return f.refresh(ctx, true, false) }

// Refresh rebuilds the products now (refitting the models when a new hour
// has begun since the last fit).
func (f *Forecaster) Refresh(ctx context.Context) error { return f.refresh(ctx, false, false) }

// Reset forgets the cached history, models and products, e.g. after
// store.Reset (factory reset). The next request starts from scratch.
func (f *Forecaster) Reset() {
	f.mu.Lock()
	f.hist, f.models, f.loaded = nil, nil, false
	f.snap.Store(nil)
	f.mu.Unlock()
}

// Status describes the state of the forecaster.
type Status struct {
	Fitted    int64  `json:"fitted"`    // Unix ms of the last model fit, 0 = never
	Built     int64  `json:"built"`     // Unix ms of the last product build
	DataHours int    `json:"dataHours"` // hours of traffic history
	LastError string `json:"lastError,omitempty"`
}

// Status returns the state without triggering any work.
func (f *Forecaster) Status() Status {
	var s Status
	if sn := f.snap.Load(); sn != nil {
		s.Fitted, s.Built = sn.fitted*1000, sn.built.UnixMilli()
		if sn.traffic != nil {
			s.DataHours = sn.traffic.DataHours
		}
	}
	if e := f.lastEr.Load(); e != nil {
		s.LastError = *e
	}
	return s
}

func (f *Forecaster) current(ctx context.Context) (*snapshot, error) {
	now := f.now()
	if f.fresh(now) {
		return f.snap.Load(), nil
	}
	if err := f.refresh(ctx, false, true); err != nil {
		if sn := f.snap.Load(); sn != nil {
			return sn, nil // serve the stale snapshot rather than nothing
		}
		return nil, err
	}
	return f.snap.Load(), nil
}

func (f *Forecaster) fresh(now time.Time) bool {
	sn := f.snap.Load()
	return sn != nil && now.Sub(sn.built) < snapshotTTL && now.Sub(sn.built) >= 0 && floorHour(now.Unix()) == sn.hour
}

// Traffic returns product 1, the traffic forecast.
func (f *Forecaster) Traffic(ctx context.Context) (*TrafficForecast, error) {
	sn, err := f.current(ctx)
	if err != nil {
		return nil, err
	}
	return sn.traffic, nil
}

// Reception returns product 2, the reception-conditions forecast.
func (f *Forecaster) Reception(ctx context.Context) (*ReceptionForecast, error) {
	sn, err := f.current(ctx)
	if err != nil {
		return nil, err
	}
	return sn.reception, nil
}

// Weather returns product 3, the local weather outlook.
func (f *Forecaster) Weather(ctx context.Context) (*WeatherOutlook, error) {
	sn, err := f.current(ctx)
	if err != nil {
		return nil, err
	}
	return sn.weather, nil
}

// Verification returns product 4, the verification / backtest report.
func (f *Forecaster) Verification(ctx context.Context) (*Verification, error) {
	sn, err := f.current(ctx)
	if err != nil {
		return nil, err
	}
	return sn.verify, nil
}

// Nowcast returns product 5, the compact summary for the top bar.
func (f *Forecaster) Nowcast(ctx context.Context) (*Nowcast, error) {
	sn, err := f.current(ctx)
	if err != nil {
		return nil, err
	}
	return sn.nowcast, nil
}

// ---------------------------------------------------------------- refresh

// refresh rebuilds the snapshot. refit forces a model fit; onlyIfStale
// returns at once when another caller rebuilt the snapshot while this one
// waited for the lock (many HTTP requests arriving together).
func (f *Forecaster) refresh(ctx context.Context, refit, onlyIfStale bool) (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if onlyIfStale && f.fresh(f.now()) {
		return nil
	}
	defer func() {
		if err != nil {
			s := err.Error()
			f.lastEr.Store(&s)
		} else {
			f.lastEr.Store(nil)
		}
	}()
	if f.st == nil {
		return errors.New("predict: no store")
	}
	now := f.now()
	rx := f.rx()

	if !f.loaded {
		f.loaded = true
		if m := f.loadModels(ctx); m != nil {
			f.models = m
		}
	}
	m := f.models
	// Refit once per hour, a few minutes into the hour so that the hourly
	// rollup of the hour just ended has been written.
	sinceHour := now.Unix() - floorHour(now.Unix())
	stale := m == nil || m.Ant != f.ant || m.Fitted > now.Unix() ||
		now.Unix()-m.Fitted >= 2*int64(refitInterval/time.Second) ||
		(floorHour(now.Unix()) != floorHour(m.Fitted) && sinceHour >= refitDelaySec)
	refit = refit || stale

	prev := f.hist
	if refit {
		prev = nil // an hourly full reload also picks up late corrections
	}
	h, err := f.loadHistory(ctx, now, prev, rx.elevM)
	if err != nil {
		return err
	}
	f.hist = h
	ph := newPhysics(rx.antM)

	if refit {
		m = f.fit(ctx, h, rx, ph, now)
		f.models = m
		if b, jerr := json.Marshal(m); jerr != nil {
			f.log.Warn("predict: encoding models", "err", jerr)
		} else if kerr := f.st.KVSet(ctx, KVModels, string(b)); kerr != nil && ctx.Err() == nil {
			f.log.Warn("predict: saving models", "err", kerr)
		}
	}

	// Latest forecast rows for the coming week.
	fcRows, err := f.st.ForecastRows(ctx, h.cur, h.cur+168*hourSec)
	if err != nil {
		return err
	}
	fc := make(map[int64]*store.Weather, len(fcRows))
	for i := range fcRows {
		fc[floorHour(fcRows[i].T)] = &fcRows[i]
	}

	nowMs := now.UnixMilli()
	sn := &snapshot{built: now, hour: h.cur, fitted: m.Fitted}
	var ac []float64
	sn.traffic, ac = f.buildTraffic(h, m, nowMs)
	sn.reception = f.buildReception(h, m, fc, ac, ph, rx.elevM, rx.lon, nowMs)
	sn.weather = f.buildWeather(h, m, fcRows, rx, now)
	sn.verify = buildVerification(h, m, nowMs)
	sn.nowcast = buildNowcast(sn.reception, sn.traffic, nowMs)
	f.snap.Store(sn)

	if refit {
		if lerr := f.logForecasts(ctx, sn, now.Unix()); lerr != nil && ctx.Err() == nil {
			f.log.Warn("predict: logging forecasts", "err", lerr)
		}
	}
	return nil
}

func (f *Forecaster) loadModels(ctx context.Context) *models {
	s, ok, err := f.st.KVGet(ctx, KVModels)
	if err != nil || !ok || s == "" {
		return nil
	}
	var m models
	if json.Unmarshal([]byte(s), &m) != nil || m.Version != modelsVersion {
		return nil
	}
	if r := m.Reception; r != nil {
		for _, sm := range []**stat.Model{&r.P95, &r.Max, &r.Far} {
			if *sm != nil && ((*sm).Validate() != nil || (*sm).NumFeatures() != nFeat) {
				*sm = nil
			}
		}
	}
	return &m
}

// fit refits every model from the history.
func (f *Forecaster) fit(ctx context.Context, h *history, rx rxInfo, ph physics, now time.Time) *models {
	m := &models{Version: modelsVersion, Fitted: now.Unix(), Ant: f.ant}

	m.Aircraft = fitSeries(h.aircraft, h.sW, h.sD)
	m.AircraftSeries = backtestSeries(m.Aircraft, h.aircraft, h.sW, h.sD, h.t0, "aircraft")
	m.MsgRate = fitSeries(h.msgRate, h.sW, h.sD)
	m.MsgRateSeries = backtestSeries(m.MsgRate, h.msgRate, h.sW, h.sD, h.t0, "frames/s")

	rows := trainRows(h, rx.lon)
	rm := fitReception(rows, h.p95, h.maxKm, ph, true)
	rm.Skill, rm.FarSkill, m.ReceptionSeries = backtestReception(rows, h, ph)
	m.Reception = rm

	if st, err := f.scanForecastLog(ctx, h.cur); err != nil {
		if ctx.Err() == nil {
			f.log.Warn("predict: forecast log scan", "err", err)
		}
	} else {
		m.Bias = st.bias
		m.AnalogueVerify = verifyAnalogue(h, rx, st.om)
	}
	if live, err := f.liveSkill(ctx, h); err != nil {
		if ctx.Err() == nil {
			f.log.Warn("predict: live verification", "err", err)
		}
	} else {
		m.Live = live
	}
	return m
}

// ---------------------------------------------------------------- forecast log

// logLeads are the lead times (hours) at which issued forecasts are logged.
var logLeads = [...]int{1, 3, 6, 12, 24, 48, 71}

// Products in predict_forecast_log.
const (
	logAircraft = "traffic.aircraft"
	logMsgRate  = "traffic.msgRate"
	logP95      = "reception.p95"
	logMax      = "reception.max"
)

func (f *Forecaster) logForecasts(ctx context.Context, sn *snapshot, issued int64) error {
	type row struct {
		product string
		t       int64
		lead    int
		e       Est
	}
	var rows []row
	for _, l := range logLeads {
		if sn.traffic != nil && l < len(sn.traffic.Hours) {
			h := sn.traffic.Hours[l]
			if h.Aircraft.Value.Valid() {
				rows = append(rows, row{logAircraft, h.T / 1000, l, h.Aircraft})
			}
			if h.MsgRate.Value.Valid() {
				rows = append(rows, row{logMsgRate, h.T / 1000, l, h.MsgRate})
			}
		}
		if sn.reception != nil && l < len(sn.reception.Hours) {
			h := sn.reception.Hours[l]
			if h.P95Km.Value.Valid() {
				rows = append(rows, row{logP95, h.T / 1000, l, h.P95Km}, row{logMax, h.T / 1000, l, h.MaxKm})
			}
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return f.st.Write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO predict_forecast_log (product, t, lead_h, issued, value, lo, hi) VALUES (?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rows {
			if _, err := stmt.ExecContext(ctx, r.product, r.t, r.lead, issued, r.e.Value.Ptr(), r.e.Lo.Ptr(), r.e.Hi.Ptr()); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM predict_forecast_log WHERE t < ?`, issued-logKeepDays*daySec)
		return err
	})
}

// LiveSkill verifies the forecasts that were actually issued (logged in
// predict_forecast_log) against what was measured afterwards. Unlike the
// backtests it includes the error of the weather forecast itself.
type LiveSkill struct {
	Product string `json:"product"` // traffic.aircraft | traffic.msgRate | reception.p95 | reception.max
	LeadH   int    `json:"leadH"`
	N       int    `json:"n"`
	MAE     Num    `json:"mae"`
	RMSE    Num    `json:"rmse"`
	Bias    Num    `json:"bias"` // mean of forecast - actual
	// Coverage is the share of the actuals inside the 80 % interval.
	Coverage Num `json:"coverage"`
}

func (f *Forecaster) liveSkill(ctx context.Context, h *history) ([]LiveSkill, error) {
	rows, err := f.st.DB().QueryContext(ctx,
		`SELECT product, t, lead_h, value, lo, hi FROM predict_forecast_log WHERE t >= ? AND t < ? ORDER BY product, lead_h, t`,
		h.cur-60*daySec, h.cur)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct {
		product string
		lead    int
	}
	type acc struct {
		n, nIv, in    int
		abs, sqr, sum float64
	}
	accs := map[key]*acc{}
	var order []key
	for rows.Next() {
		var product string
		var t int64
		var lead int
		var v, lo, hi sql.NullFloat64
		if err := rows.Scan(&product, &t, &lead, &v, &lo, &hi); err != nil {
			return nil, err
		}
		i := h.idx(t)
		if i < 0 || !v.Valid {
			continue
		}
		var actual float64
		switch product {
		case logAircraft:
			actual = h.aircraft[i]
		case logMsgRate:
			actual = h.msgRate[i]
		case logP95:
			actual = h.p95[i]
		case logMax:
			actual = h.maxKm[i]
		default:
			continue
		}
		if !fin(actual) {
			continue
		}
		k := key{product, lead}
		a := accs[k]
		if a == nil {
			a = &acc{}
			accs[k] = a
			order = append(order, k)
		}
		e := v.Float64 - actual
		a.n++
		a.abs += math.Abs(e)
		a.sqr += e * e
		a.sum += e
		if lo.Valid && hi.Valid {
			a.nIv++
			if actual >= lo.Float64 && actual <= hi.Float64 {
				a.in++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]LiveSkill, 0, len(order))
	for _, k := range order {
		a := accs[k]
		n := float64(a.n)
		ls := LiveSkill{Product: k.product, LeadH: k.lead, N: a.n, MAE: rn(a.abs/n, 3), RMSE: rn(math.Sqrt(a.sqr/n), 3),
			Bias: rn(a.sum/n, 3), Coverage: Num(nan)}
		if a.nIv > 0 {
			ls.Coverage = rn(float64(a.in)/float64(a.nIv), 3)
		}
		out = append(out, ls)
	}
	return out, nil
}
