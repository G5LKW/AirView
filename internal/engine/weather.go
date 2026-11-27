package engine

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"airview/internal/model"
	"airview/internal/predict"
	"airview/internal/store"
	"airview/internal/weather"
)

const (
	// kvElevation / kvBackfill remember the location the one-time jobs ran for.
	kvElevation = "engine.elevation.autofill"
	kvBackfill  = "engine.weather.backfill"

	// BackfillDays is how far back the one-time weather history download
	// reaches (the poller itself covers the last two days).
	BackfillDays = 60

	// fcLogKeepDays bounds weather_fc_log (the forecaster reads 45 days).
	fcLogKeepDays = 120
)

// fcLogLeads are the forecast lead times (hours) logged for verification:
// dense where forecasts change fastest, covering every lead class the
// forecaster's bias table uses (1-6, 7-12, 13-24, 25-48, 49-72, 73-168 h).
var fcLogLeads = [...]int{1, 2, 3, 4, 5, 6, 9, 12, 18, 24, 36, 48, 60, 72, 96, 120, 144, 168}

// locKey identifies a station location to about 100 m.
func locKey(rx model.Receiver) string {
	return fmt.Sprintf("%.3f,%.3f", rx.Lat, rx.Lon)
}

// weatherRows converts Open-Meteo hours to store rows with the derived
// radio metrics (surface refractivity, dN/dh, k-factor, duct strength and the
// profile document) filled in by predict.RowFromHour, which computes them
// with internal/radio.
func weatherRows(hours []weather.HourPoint, elevM float64, issued int64) []store.Weather {
	rows := make([]store.Weather, 0, len(hours))
	for i := range hours {
		r := predict.RowFromHour(&hours[i], elevM)
		r.Issued = issued
		rows = append(rows, r)
	}
	return rows
}

// onWeather persists a fresh bundle. It runs on the poller's goroutine.
//
// To be gentle on an SD card the full set (two days of analysis, the whole
// forecast and the verification log) is written once per clock hour and
// station location; the updates in between only refresh the newest three
// observation hours. Both write only what changed (Store.AddWeatherObsDiff,
// Store.SetWeatherForecastDiff): between two runs of the weather model a
// poll repeats almost every hour unchanged, and each row is ~1.25 KB.
func (e *Engine) onWeather(b *weather.Bundle) {
	if b == nil || len(b.Hourly) == 0 {
		return
	}
	if nc := e.net; nc != nil && nc.mlatWx != nil {
		nc.mlatWx.Set(b) // the MLAT GNSS−baro field's surface-weather fallback
	}
	eff := e.eff.Load()
	if eff == nil {
		return
	}
	now := time.Now()
	n, full := e.persistWeather(b, eff.cfg.Receiver, now)
	if full {
		e.log.Info("weather stored", "hours", n, "fetched", b.FetchedTime().UTC().Format(time.RFC3339))
		e.an.Invalidate()
		e.wxFresh.Store(true)
	}
	e.kick(e.choreC)
}

// persistWeather writes b to weather_obs / weather_fc / weather_fc_log and
// returns the number of rows queued and whether it was a full write.
func (e *Engine) persistWeather(b *weather.Bundle, rx model.Receiver, now time.Time) (int, bool) {
	past, future := b.Split(now)
	hour := now.Unix() / 3600 * 3600
	at := locKey(rx)
	issued := b.Fetched / 1000
	if issued <= 0 {
		issued = now.Unix()
	}

	e.mu.Lock()
	full := e.wxPersistH != hour || e.wxPersistAt != at
	e.wxPersistH, e.wxPersistAt = hour, at
	e.mu.Unlock()

	elev := rx.GroundElevM
	if !full {
		if len(past) > 3 {
			past = past[len(past)-3:]
		}
		e.st.AddWeatherObsDiff(weatherRows(past, elev, 0))
		return len(past), false
	}

	batch := store.NewBatch()
	obs := weatherRows(past, elev, 0)
	batch.AddWeatherObsDiff(obs)
	fc := weatherRows(future, elev, issued)
	batch.SetWeatherForecastDiff(fc)

	// Verification log: what was forecast lead_h hours ahead. The lead is
	// counted from the current clock hour.
	var fclog []store.Weather
	for _, lead := range fcLogLeads {
		t := hour + int64(lead)*3600
		for i := range fc {
			if fc[i].T == t {
				r := fc[i]
				r.Issued = hour
				r.ProfileJSON = ""
				fclog = append(fclog, r)
				break
			}
		}
	}
	batch.AddWeatherForecastLog(fclog)
	e.st.Submit(batch)
	return len(obs) + len(fc) + len(fclog), true
}

// choreLoop runs the slow one-time jobs off the hot paths: elevation
// auto-fill and the weather history backfill.
func (e *Engine) choreLoop(ctx context.Context) {
	t := time.NewTimer(8 * time.Second)
	defer t.Stop()
	retry := time.NewTicker(time.Hour)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-retry.C:
		case <-e.choreC:
			// Let a burst of changes settle.
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		e.runChores(ctx)
	}
}

func (e *Engine) runChores(ctx context.Context) {
	if e.wxFresh.Swap(false) {
		// The bundle is queued in the store's writer: commit it, then
		// rebuild the products so they include the new forecast hours.
		if err := e.st.Flush(); err != nil && ctx.Err() == nil {
			e.log.Warn("flushing weather rows", "err", err)
		}
		if err := e.fc.Refresh(ctx); err != nil && ctx.Err() == nil {
			e.log.Warn("forecast rebuild after weather update", "err", err)
		}
	}
	if e.opts.Offline {
		return
	}
	stored := e.holder.Get()
	if !stored.Weather.Enabled {
		return
	}
	e.autoElevation(ctx, stored)
	e.backfillWeather(ctx)
}

// autoElevation fills Receiver.GroundElevM from the elevation API when the
// station is located and the elevation is still 0. It runs once per location
// (a site that really is at sea level is not asked about again).
func (e *Engine) autoElevation(ctx context.Context, stored model.Config) {
	rx := stored.Receiver
	if !located(rx) || rx.GroundElevM != 0 {
		return
	}
	key := locKey(rx)
	if v, ok, err := e.st.KVGet(ctx, kvElevation); err != nil || (ok && v == key) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	elev, err := e.wx.Client().Elevation(cctx, rx.Lat, rx.Lon)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("elevation look-up failed", "err", err)
		}
		return
	}
	if math.IsNaN(elev) || elev < -500 || elev > 9000 {
		return
	}
	elev = math.Round(elev)
	_ = e.st.KVSet(ctx, kvElevation, key)
	if elev == 0 {
		return
	}
	err = e.mutateConfig(ctx, func(c *model.Config) bool {
		if locKey(c.Receiver) != key || !c.Receiver.Located || c.Receiver.GroundElevM != 0 {
			return false // changed meanwhile
		}
		c.Receiver.GroundElevM = elev
		return true
	})
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("saving ground elevation", "err", err)
		}
		return
	}
	e.log.Info("ground elevation set from terrain model", "elevM", elev)
	e.Notify("info", fmt.Sprintf("Ground elevation set to %.0f m from the terrain model.", elev))
}

// backfillWeather downloads the last BackfillDays of hourly weather once per
// station location, so that the weather correlations and the reception model
// start with context instead of an empty table.
func (e *Engine) backfillWeather(ctx context.Context) {
	eff := e.eff.Load()
	stored := e.holder.Get()
	rx := eff.cfg.Receiver
	if !located(rx) {
		return
	}
	if eff.demo && !located(stored.Receiver) {
		return // the simulator's default site: not worth the download
	}
	key := locKey(rx)
	if v, ok, err := e.st.KVGet(ctx, kvBackfill); err != nil || (ok && v == key) {
		return
	}
	// Wait for the first successful poll: it proves the network works and
	// covers the newest two days.
	if e.wx.Latest() == nil {
		return
	}
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -BackfillDays)
	to := now.AddDate(0, 0, -2)
	e.log.Info("weather history backfill started", "days", BackfillDays)
	var hours int
	err := e.wx.Client().HistoryEach(ctx, rx.Lat, rx.Lon, from, to, weather.HistoryOptions{},
		func(b *weather.Bundle) error {
			// Only hours that have happened; never overwrite the future.
			past, _ := b.Split(time.Now())
			rows := weatherRows(past, rx.GroundElevM, 0)
			hours += len(rows)
			e.st.AddWeatherObsDiff(rows)
			return nil
		})
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("weather history backfill failed (will retry)", "hours", hours, "err", err)
		}
		return
	}
	if err := e.st.KVSet(ctx, kvBackfill, key); err != nil {
		e.log.Warn("weather history backfill: saving marker", "err", err)
	}
	e.an.Invalidate()
	e.log.Info("weather history backfill finished", "hours", hours)
	e.Notify("info", fmt.Sprintf("Weather history loaded: %d days (%d hours).", BackfillDays, hours))
}

// pruneForecastLog bounds weather_fc_log, which the store's Prune leaves alone.
func (e *Engine) pruneForecastLog(ctx context.Context) {
	cutoff := time.Now().Unix() - fcLogKeepDays*86400
	err := e.st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM weather_fc_log WHERE t < ?`, cutoff)
		return err
	})
	if err != nil && ctx.Err() == nil {
		e.log.Warn("pruning forecast log", "err", err)
	}
}
