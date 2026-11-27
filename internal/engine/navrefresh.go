package engine

import (
	"context"
	"hash/fnv"
	"os"
	"time"

	"airview/internal/navdata"
)

// Automatic reference-data refresh (docs/ROUTES-DESIGN.md §2.4, user
// decision 1: on by default). With navData.autoRefresh the engine downloads
// the reference data (airports, runways, navaids, countries, VRS standing
// data routes) once when none is imported, and then checks it once a day
// at a per-install time of day with a conditional GET (the importer sends
// If-None-Match; an unchanged file costs a 304). Offline engines (tests,
// air-gapped demo) never download.

const (
	navFirstDelay  = time.Minute    // after start, before the first download
	navRetry       = time.Hour      // after a failed import
	navCheckEvery  = time.Minute    // how often the schedule is looked at
	navRefreshDays = 24 * time.Hour // between conditional refreshes
)

// navRefresh is the scheduler state (navLoop goroutine only).
type navRefresh struct {
	started  time.Time
	lastLook time.Time
	next     time.Time // next refresh; zero = not scheduled yet
	missing  bool      // airports or routes never imported
	known    bool      // missing is valid
}

// refreshOffset is the per-install time of day of the daily check, so that
// many installations never hit the download servers together.
func (e *Engine) refreshOffset() time.Duration {
	h := fnv.New64a()
	host, _ := os.Hostname()
	h.Write([]byte(host))
	h.Write([]byte{0})
	h.Write([]byte(e.dir))
	return time.Duration(h.Sum64()%86400) * time.Second
}

// nextRefreshAfter returns the first per-install refresh time after t.
func nextRefreshAfter(t time.Time, off time.Duration) time.Time {
	day := t.UTC().Truncate(24 * time.Hour)
	n := day.Add(off)
	for !n.After(t) {
		n = n.Add(24 * time.Hour)
	}
	return n
}

// autoRefresh starts an import when one is due. Called from navLoop.
func (e *Engine) autoRefresh(ctx context.Context, st *navRefresh, now time.Time) {
	if e.opts.Offline || !e.EffectiveConfig().NavData.AutoRefresh {
		return
	}
	if st.started.IsZero() {
		st.started = now
	}
	if now.Sub(st.lastLook) < navCheckEvery || now.Sub(st.started) < navFirstDelay {
		return
	}
	st.lastLook = now
	nd := e.ext.nd
	if nd.ImportStatus().Running {
		return
	}
	if !st.known {
		ds, err := nd.Datasets(ctx)
		if err != nil {
			return
		}
		rows := map[navdata.Dataset]int64{}
		for _, d := range ds {
			rows[d.Dataset] = d.Rows
		}
		st.missing = rows[navdata.DatasetAirports] == 0 || rows[navdata.DatasetRoutes] == 0
		st.known = true
	}
	if !st.next.IsZero() && now.Before(st.next) {
		return
	}
	if !st.missing && st.next.IsZero() {
		st.next = nextRefreshAfter(now, e.refreshOffset())
		return
	}
	if err := nd.StartImport(e.RuntimeContext(), navdata.ImportOptions{}); err != nil {
		return
	}
	e.log.Info("navdata: checking the reference data for updates", "first", st.missing)
	st.next = nextRefreshAfter(now, e.refreshOffset())
}

// done updates the schedule after an import finished.
func (st *navRefresh) done(failed bool, now time.Time) {
	st.known = false // re-read the row counts
	if failed {
		st.next = now.Add(navRetry)
	}
}

// RefreshNavData starts a conditional refresh of all reference datasets now.
func (e *Engine) RefreshNavData() error {
	if e.ext == nil {
		return navdata.ErrNoStore
	}
	if e.opts.Offline {
		return navdata.ErrDisabled
	}
	return e.ext.nd.StartImport(e.RuntimeContext(), navdata.ImportOptions{})
}
