package engine

// Per-station components of the aggregator (DESIGN §6.5, HOOKS: WP-E ->
// internal/analytics). They are built lazily through Stations().Acquire for
// a page that asks and retired with the runtime.

import (
	"time"

	"airview/internal/analytics"
	"airview/internal/config"
	"airview/internal/geoid"
	"airview/internal/model"
	"airview/internal/store"
)

// StationAnalyticsName is the component name of a station's analyzer.
const StationAnalyticsName = "analytics"

// StationAnalytics is a station's statistics analyzer over stations.db
// (ant = "s<idx>") with its flights from airview.db.
type StationAnalytics struct {
	*analytics.Analyzer
}

// Close implements StationComponent.
func (StationAnalytics) Close() {}

// registerStationComponents registers the per-station factories. Without
// stations.db there is nothing per station to analyse.
func (e *Engine) registerStationComponents(nc *netCore) {
	if nc.stationsDB == nil || nc.stations == nil {
		return
	}
	mainDB := e.st
	nc.stations.Register(StationAnalyticsName, func(meta model.StationMeta) StationComponent {
		idx := meta.Idx
		cfg := func() model.Config { return e.stationConfig(nc, idx) }
		return StationAnalytics{analytics.New(nc.stationsDB, cfg,
			analytics.WithAntPrefix(store.StationAntPrefix(idx)), analytics.WithNetwork(mainDB, meta.SID))}
	})
}

// stationConfig is the effective configuration seen from station idx: its
// effective position as the receiver, no antenna table (stations do not
// report one yet).
func (e *Engine) stationConfig(nc *netCore, idx model.StationIdx) model.Config {
	c := config.Clone(e.EffectiveConfig())
	c.Antennas = nil
	c.Receiver = model.Receiver{Name: "station"}
	st, ok := nc.reg.Station(idx)
	if !ok {
		return c
	}
	c.Receiver.Name = st.Name
	if st.Located {
		g := st.Effective
		c.Receiver.Located, c.Receiver.Lat, c.Receiver.Lon = true, g.Lat, g.Lon
		// The station height is HAE; the receiver table wants the site's
		// height above mean sea level.
		c.Receiver.GroundElevM = g.HAE
		if msl, ok := geoid.HAEToMSL(g.Lat, g.Lon, g.HAE); ok {
			c.Receiver.GroundElevM = msl
		}
	}
	return c
}

// StationAnalyzer returns the analyzer of station sid, building the
// station's runtime on demand (nil in standalone mode, for an unknown
// station or without stations.db).
func (e *Engine) StationAnalyzer(sid string) *analytics.Analyzer {
	nc := e.net
	if nc == nil || nc.stationsDB == nil {
		return nil
	}
	st, ok := nc.reg.BySID(sid)
	if !ok {
		return nil
	}
	meta, ok := nc.reg.Resolver().Info(st.Idx)
	if !ok {
		return nil
	}
	rt := nc.stations.Acquire(meta, time.Now())
	if rt == nil {
		return nil
	}
	if c, ok := rt.Component(StationAnalyticsName).(StationAnalytics); ok {
		return c.Analyzer
	}
	return nil
}
