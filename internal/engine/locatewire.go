package engine

// Locate Me v2 (internal/analytics locate2*, DESIGN §10.4). The tracker's
// guess samples (ADS-B positions received off-air, with signal level) are
// recorded to guess_samples: in standalone mode for the receiver itself
// (airview.db), in aggregator mode only for stations that have no location
// yet (stations.db), so an owner can be shown an approximate position
// before the survey converges. Locate Me v2 itself runs on request
// (/api/guess, owner station views); the T=1 timing tracks of a
// GPS-stamping receiver are collected from the latency analyser's input
// (standalone only: the host's timing says nothing about other stations).
//
// Settings: locate.enabled, retentionDays, maxRowsPerStation,
// maxRowsTotal (read at start; a change takes effect after a restart).

import (
	"context"
	"sync"
	"time"

	"airview/internal/analytics"
	"airview/internal/model"
	"airview/internal/store"
)

// locateWiring is the engine's Locate Me v2 state.
type locateWiring struct {
	rec *analytics.GuessRecorder
	col *analytics.L2TimingCollector
	// recorded remembers the aggregator stations samples were kept for, so
	// their rows are deleted once they are located (DeleteGuessSamples).
	mu       sync.Mutex
	recorded map[model.StationIdx]struct{}
	db       *store.Store
}

// installLocate builds the recorder and installs the hooks. Caller holds
// opMu (Start), after the tracker and the extensions exist.
func (e *Engine) installLocate(cfg model.Config) {
	lc := cfg.Locate
	if !lc.Enabled {
		return
	}
	lw := &locateWiring{recorded: map[model.StationIdx]struct{}{}, db: e.st}
	opt := analytics.GuessRecorderOptions{
		Retention:  time.Duration(lc.RetentionDays) * 24 * time.Hour,
		PerStation: int64(lc.MaxRowsPerStation),
		Total:      int64(lc.MaxRowsTotal),
	}
	if nc := e.net; nc != nil {
		if nc.stationsDB == nil {
			return // no per-station database: nothing to record into
		}
		lw.db = nc.stationsDB
		reg := nc.reg
		opt.Want = func(idx model.StationIdx) bool {
			if idx == model.HostStation {
				return false
			}
			st, ok := reg.Station(idx)
			if !ok || st.Located {
				return false
			}
			lw.mu.Lock()
			if len(lw.recorded) < 1_000_000 {
				lw.recorded[idx] = struct{}{}
			}
			lw.mu.Unlock()
			return true
		}
	}
	lw.rec = analytics.NewGuessRecorder(lw.db, opt)
	e.tr.SetGuessSampleHook(lw.rec.TrackerHook())
	src := analytics.LocateSources{}
	if x := e.ext; x != nil && x.terr != nil {
		if p := x.terr.Provider(); p != nil {
			src.Terrain = p // cached tiles only
		}
	}
	if e.net == nil {
		lw.col = &analytics.L2TimingCollector{}
		if e.lat != nil {
			e.lat.SetTap(lw.col.Observe)
		}
		src.Timing = lw.col.Tracks
		src.GNSS = func() (float64, float64, bool) {
			g := e.ReceiverGPS()
			if g == nil {
				return 0, 0, false
			}
			return g.Lat, g.Lon, true
		}
	}
	analytics.SetLocateSources(src)
	e.locate.Store(lw)
}

// pruneLocated deletes the guess samples of aggregator stations that got
// a location since they were recorded (netMaintainLoop).
func (e *Engine) pruneLocated(ctx context.Context) {
	lw, nc := e.locate.Load(), e.net
	if lw == nil || nc == nil {
		return
	}
	lw.mu.Lock()
	var done []model.StationIdx
	for idx := range lw.recorded {
		if st, ok := nc.reg.Station(idx); !ok || st.Located {
			done = append(done, idx)
		}
	}
	for _, idx := range done {
		delete(lw.recorded, idx)
	}
	lw.mu.Unlock()
	for _, idx := range done {
		if err := lw.db.DeleteGuessSamples(ctx, int(idx)); err != nil {
			e.log.Warn("locate: deleting guess samples", "station", int(idx), "err", err)
		}
	}
}

// closeLocate removes the hooks and flushes the recorder (Stop, after the
// tracker stopped and before the stores close).
func (e *Engine) closeLocate() {
	lw := e.locate.Swap(nil)
	if lw == nil {
		return
	}
	e.tr.SetGuessSampleHook(nil)
	if e.lat != nil {
		e.lat.SetTap(nil)
	}
	lw.rec.Close()
}

// GuessRecorderStats returns the Locate Me v2 recorder's counters (rows
// written and pruned; zero when it is off).
func (e *Engine) GuessRecorderStats() (written, pruned int64) {
	if lw := e.locate.Load(); lw != nil {
		return lw.rec.Stats()
	}
	return 0, 0
}
