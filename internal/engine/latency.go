package engine

import (
	"context"
	"math"
	"time"

	"airview/internal/geo"
	"airview/internal/latency"
	"airview/internal/model"
)

// Wiring of the latency-chain analysis (internal/latency, see its WIRING.md):
// the analyzer is built once in Start, fed by the tracker, clocked by the NTP
// sampler (config.ntp.server is its only reference), positioned at the
// effective receiver (or the receiver's own GPS position when the receiver
// is not located), persisted once a minute and seeded from the database at
// start.

// latencySeedWindow is how much persisted per-feed history is loaded at start.
const latencySeedWindow = 24 * time.Hour

// buildLatency creates the analyzer. Caller holds opMu (Start), before the
// tracker is built.
func (e *Engine) buildLatency(ctx context.Context) {
	e.lat = latency.New(latency.Options{})
	e.lat.SetClock(e.ntp, latency.SamplerInfo(e.ntp))
	rows, err := latency.LoadMinutes(ctx, e.st.DB(), time.Now().Add(-latencySeedWindow).Unix(), nil, 0)
	if err != nil {
		e.log.Warn("loading latency history", "err", err)
		return
	}
	if len(rows) > 0 {
		e.lat.Seed(rows)
	}
}

// mastHeightM is the feed-point height above ground of the highest antenna
// (0 when none is configured).
func mastHeightM(cfg model.Config) float64 {
	h := 0.0
	for _, a := range cfg.Antennas {
		if a.HeightAGLM > h && !math.IsInf(a.HeightAGLM, 0) {
			h = a.HeightAGLM
		}
	}
	return h
}

// StationPosition is the station position the components actually use.
type StationPosition struct {
	Lat, Lon float64
	// HeightM is the antenna height in metres AMSL (ground elevation plus the
	// highest antenna, or the GPS antenna height).
	HeightM float64
	// Source is "config" (located receiver, including the demo site), "gps"
	// (the receiver's own GPS position, Beast type-5 frames) or "none".
	Source string
}

// Station returns the effective station position: the located receiver of
// the effective configuration, else the receiver-reported GPS position.
func (e *Engine) Station() StationPosition {
	cfg := e.EffectiveConfig()
	return stationOf(cfg, e.ReceiverGPS())
}

func stationOf(cfg model.Config, gps *model.ReceiverGPS) StationPosition {
	if rx := cfg.Receiver; located(rx) {
		elev := rx.GroundElevM
		if math.IsNaN(elev) || math.IsInf(elev, 0) {
			elev = 0
		}
		return StationPosition{Lat: rx.Lat, Lon: rx.Lon, HeightM: elev + mastHeightM(cfg), Source: "config"}
	}
	if gps != nil && geo.ValidLatLon(gps.Lat, gps.Lon) && !(gps.Lat == 0 && gps.Lon == 0) {
		h := gps.AltM
		if math.IsNaN(h) || math.IsInf(h, 0) {
			h = 0
		}
		return StationPosition{Lat: gps.Lat, Lon: gps.Lon, HeightM: h, Source: "gps"}
	}
	return StationPosition{Lat: math.NaN(), Lon: math.NaN(), Source: "none"}
}

// syncLatencyReceiver hands the effective station position to the latency
// analyzer when it changed. Safe for concurrent use.
func (e *Engine) syncLatencyReceiver() {
	if e.lat == nil {
		return
	}
	st := e.Station()
	e.latMu.Lock()
	defer e.latMu.Unlock()
	if e.latRxSet && st.Source == e.latRx.Source && sameF(st.Lat, e.latRx.Lat) && sameF(st.Lon, e.latRx.Lon) &&
		math.Abs(st.HeightM-e.latRx.HeightM) < 1 {
		return
	}
	e.latRx, e.latRxSet = st, true
	if st.Source == "none" {
		e.lat.SetReceiver(math.NaN(), 0, 0)
		return
	}
	e.lat.SetReceiver(st.Lat, st.Lon, st.HeightM)
}

// sameF compares coordinates to about 1 m (GPS reports jitter in the last digits).
func sameF(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	return math.Abs(a-b) < 1e-5
}

// persistLatency writes the finished latency minutes and applies the
// minute-statistics retention.
func (e *Engine) persistLatency(ctx context.Context) {
	if e.lat == nil {
		return
	}
	rows := e.lat.DrainMinutes()
	days := 0
	if e.clockPlausible(time.Now()) {
		days = e.holder.Get().Storage.MinuteRetentionDays
	}
	if err := latency.Persist(ctx, e.st, rows, days); err != nil && ctx.Err() == nil {
		e.log.Warn("persisting latency minutes", "err", err)
	}
}

// Latency returns the latency-chain analyzer (nil before Start).
func (e *Engine) Latency() *latency.Analyzer { return e.lat }
