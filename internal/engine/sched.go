package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
	"airview/internal/store"
)

// minPlausibleYear is the earliest year the host clock may show before the
// engine trusts it enough to delete old rows. A Raspberry Pi without a
// real-time clock boots in 1970 (or at the last shutdown time) until NTP
// answers.
const minPlausibleYear = 2025

// clockPlausible reports whether "now" can be trusted for retention: the
// host clock is past the build era and, when NTP is measuring, it does not
// disagree by more than an hour.
func (e *Engine) clockPlausible(now time.Time) bool {
	if now.Year() < minPlausibleYear {
		return false
	}
	if e.ntp != nil && e.ntp.Synced() {
		off := e.ntp.Offset()
		if off > time.Hour || off < -time.Hour {
			return false
		}
	}
	return true
}

// schedulerLoop runs the periodic database jobs. Everything is spread out
// and small so that a Pi with an SD card is not stalled:
//
//   - the store's hourly maintenance 3 min after every hour (and shortly
//     after start): roll-up of finished stats_minute hours into stats_hour,
//     the hourly jobs of the store, PRAGMA optimize and a WAL checkpoint;
//   - retention (store.Prune) once a day and after a storage setting
//     change, skipped while the clock is implausible;
//   - a retry of the data port when it could not be bound.
func (e *Engine) schedulerLoop(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()

	start := time.Now()
	var lastRollupHour int64 = -1
	firstRollup := false
	var lastPrune time.Time
	firstPrune := false
	lastLatency := start

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		up := now.Sub(start)

		// Hourly roll-up.
		hour := now.Unix() / 3600
		due := (!firstRollup && up >= 45*time.Second) ||
			(hour != lastRollupHour && now.Unix()%3600 >= 180)
		if due {
			firstRollup = true
			lastRollupHour = hour
			e.rollup(ctx)
		}

		// Retention.
		e.mu.Lock()
		pruneNow := e.pruneDue
		e.pruneDue = false
		e.mu.Unlock()
		if !firstPrune && up >= 5*time.Minute {
			firstPrune = true
			pruneNow = true
		}
		if !lastPrune.IsZero() && now.Sub(lastPrune) >= 24*time.Hour {
			pruneNow = true
		}
		if pruneNow {
			lastPrune = now
			e.prune(ctx, now)
		}

		// Latency minutes (finished minutes only; about once a minute).
		if now.Sub(lastLatency) >= time.Minute {
			lastLatency = now
			e.persistLatency(ctx)
		}

		// Data port retry.
		if e.HubError() != "" {
			e.kick(e.reapplyC)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// rollup runs the store's hourly maintenance: the stats_hour roll-up, the
// registered hourly jobs (trail compaction, sealed histogram days, hour
// rollups), PRAGMA optimize and a TRUNCATE checkpoint.
func (e *Engine) rollup(ctx context.Context) {
	t0 := time.Now()
	rep, err := e.st.HourlyMaintenance(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("hourly maintenance failed", "err", err, "hours", rep.Hours)
		}
		return
	}
	if rep.Hours > 0 || len(rep.Errors) > 0 {
		e.log.Debug("hourly maintenance", "hours", rep.Hours, "jobs", len(rep.Jobs), "errors", len(rep.Errors),
			"took", time.Since(t0).Round(time.Millisecond))
	}
}

func (e *Engine) prune(ctx context.Context, now time.Time) {
	if !e.clockPlausible(now) {
		e.log.Warn("retention skipped: the system clock is not plausible", "now", now.UTC().Format(time.RFC3339))
		// Try again at the next tick after the clock was fixed.
		e.mu.Lock()
		e.pruneDue = true
		e.mu.Unlock()
		return
	}
	cfg := e.holder.Get()
	res, err := e.st.Prune(ctx, pruneStorage(cfg))
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("retention failed", "err", err)
		}
		return
	}
	e.pruneForecastLog(ctx)
	if res.Total() > 0 {
		args := []any{"flights", res.Flights, "positions", res.Positions,
			"minuteStats", res.MinuteStats, "metObs", res.MetObs, "ntpSamples", res.NTPSamples}
		for _, k := range slices.Sorted(maps.Keys(res.Extra)) {
			args = append(args, k, res.Extra[k])
		}
		e.log.Info("retention", args...)
		e.an.Invalidate()
	}
}

// feedWatchLoop turns feed connection changes into notices and events.
func (e *Engine) feedWatchLoop(ctx context.Context, fm *feed.Manager) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	type state struct {
		connected bool
		seen      bool
	}
	states := make(map[string]*state)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		list := fm.Status()
		for _, fs := range list {
			s := states[fs.ID]
			if s == nil {
				s = &state{}
				states[fs.ID] = s
			}
			s.seen = true
			if fs.Connected == s.connected {
				continue
			}
			s.connected = fs.Connected
			name := fs.Name
			if name == "" {
				name = fs.ID
			}
			kind, level, text := "feed_down", "warn", fmt.Sprintf("Feed %q disconnected", name)
			if fs.Connected {
				kind, level, text = "feed_up", "info", fmt.Sprintf("Feed %q connected (%s)", name, fs.Addr)
			} else if fs.LastError != "" {
				text += ": " + fs.LastError
			} else if !fs.Enabled {
				text = fmt.Sprintf("Feed %q stopped", name)
				level = "info"
			}
			e.notices.publish(Notice{Level: level, Text: text, Kind: kind})
			detail, _ := json.Marshal(map[string]any{
				"feed": fs.ID, "name": fs.Name, "type": fs.Type, "addr": fs.Addr, "error": fs.LastError,
			})
			e.st.AddEvents([]store.Event{{T: time.Now().Unix(), Kind: kind, Detail: string(detail)}})
		}
		// Forget feeds that were removed.
		for id, s := range states {
			if !s.seen {
				delete(states, id)
			}
			s.seen = false
		}
	}
}

// pruneStorage returns the retention periods of airview.db. In aggregator
// mode the position trails follow aggregator.storage.trailDays (SO-6: at
// thousands of aircraft the positions table grows by gigabytes per day).
func pruneStorage(cfg model.Config) model.StorageConfig {
	st := cfg.Storage
	if cfg.Mode == model.ModeAggregator && cfg.Aggregator.Storage.TrailDays > 0 {
		st.TrailRetentionDays = cfg.Aggregator.Storage.TrailDays
	}
	return st
}
