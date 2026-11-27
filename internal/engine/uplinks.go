package engine

import (
	"fmt"
	"path/filepath"
	"strings"

	"airview/internal/avf"
	"airview/internal/feed"
	"airview/internal/ingest"
	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/uplink"
)

// Uplinks (DESIGN §1 "Uplink tee", §12.4): in standalone and aggregator
// modes the station can feed one or more AirView aggregators over AVF/1
// while keeping its full local UI. The uplink manager is one branch of the
// feed manager's sink tee; it refuses everything that must never leave the
// station by itself (MLAT results, demo and hub feeds, NoForward feeds).
//
// The manager is created on first need (an uplink is configured) and then
// kept for the life of the engine, so its feeder identity and spool stay
// open; with no uplinks configured it is idle (Apply(nil)). It never runs in
// remote-data or remote-web mode.

// uplinkStateDir is the manager's state directory inside the data
// directory (identity.key, uuid, per-uplink state and spool).
const uplinkStateDir = "uplink"

// uplinksAllowed reports whether mode may run uplinks.
func uplinksAllowed(mode string) bool {
	return mode == model.ModeStandalone || mode == model.ModeAggregator
}

// syncUplinks makes the uplink manager match the stored configuration.
// Caller holds opMu.
func (e *Engine) syncUplinks(stored model.Config, eff *effective) {
	var ups []model.Uplink
	if uplinksAllowed(eff.mode) {
		for _, u := range stored.Uplinks {
			if !u.Disabled {
				ups = append(ups, u)
			}
		}
	}
	m := e.up.Load()
	if m == nil {
		if len(ups) == 0 {
			return
		}
		opts := uplink.Options{
			Software: "airview",
			Version:  e.opts.Version,
			StateDir: filepath.Join(e.dir, uplinkStateDir),
			Source:   uplink.SourceFuncs{ForwardableFn: e.uplinkForwardable, ReceiverFn: e.uplinkReceiver},
			Inject:   injectSink{e},
			Position: e.uplinkPosition,
			Now:      ntp.Now,
			Logf: func(format string, args ...any) {
				e.log.Info(fmt.Sprintf(format, args...), "component", "uplink")
			},
		}
		opts.ApplyTuning(stored.UplinkTuning)
		var err error
		m, err = uplink.NewManager(opts)
		if err != nil {
			e.log.Error("uplinks cannot start", "err", err)
			e.Notify("warn", "Uplinks cannot start: "+err.Error())
			return
		}
		e.up.Store(m)
		e.log.Info("uplink manager started", "feederId", m.Identity().FeederID)
	}
	if notes := m.ApplyAll(ups); len(notes) > 0 {
		for _, n := range notes {
			e.log.Warn("uplink: "+n, "component", "uplink")
		}
	}
}

// closeUplinks stops every uplink (unsent data goes to the spool). Called
// from Stop after the feeds are closed.
func (e *Engine) closeUplinks() {
	if m := e.up.Swap(nil); m != nil {
		m.Close()
	}
}

// Uplinks returns the uplink manager (nil until an uplink was configured).
// It is an http.Handler for the admin status route (internal/uplink
// WIRING.md §3).
func (e *Engine) Uplinks() *uplink.Manager { return e.up.Load() }

// uplinkForwardable is the per-feed uplink rule (feed.Manager.Forwardable).
func (e *Engine) uplinkForwardable(feedID string) bool {
	if fm := e.fm.Load(); fm != nil {
		return fm.Forwardable(feedID)
	}
	return false
}

// uplinkReceiver describes a local feed as an AVF receiver. Never host
// names, LAN addresses or account names (AVF §5.4).
func (e *Engine) uplinkReceiver(feedID string) uplink.ReceiverInfo {
	if e.holder == nil {
		return uplink.ReceiverInfo{}
	}
	cfg := e.holder.Get()
	idx := -1
	for i, f := range cfg.Feeds {
		if f.ID == feedID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return uplink.ReceiverInfo{}
	}
	f := cfg.Feeds[idx]
	info := uplink.ReceiverInfo{
		RxNo:      idx + 1, // stable as long as the feed list order is
		Name:      safeName(firstNonEmpty(f.Name, f.ID)),
		Decoder:   firstNonEmpty(cfg.Receiver.Model, f.Type),
		InputType: f.Type,
	}
	chans := map[int]bool{}
	for _, a := range cfg.Antennas {
		if f.AntennaID != "" && a.ID != f.AntennaID {
			continue
		}
		info.Antennas = append(info.Antennas, avf.Antenna{ID: a.ID, Kind: a.Type, AGLM: a.HeightAGLM})
		if a.Channel >= 0 {
			chans[a.Channel] = true
		}
	}
	info.Channels = max(1, len(chans))
	if sp := e.Station(); sp.Source != "none" {
		if g, ok := hostGeo(sp); ok {
			src := model.LocSrcManual
			if sp.Source == "gps" {
				src = model.LocSrcGNSS
			}
			info.Location = &avf.Location{Lat: g.Lat, Lon: g.Lon, Alt: g.HAE, AltRef: model.AltRefHAE, Source: src}
		}
	}
	for _, s := range e.FeedStatus() {
		if s.ID == feedID {
			info.Connected = s.Connected
			info.FPS = s.MsgRate
			info.SinceMs = s.Since
			break
		}
	}
	return info
}

// injectSink delivers MLAT results returned by an aggregator into the
// local picture: the tracker directly (station 0, Src mlat-av; in
// aggregator mode they credit no lane).
type injectSink struct{ e *Engine }

func (s injectSink) HandleFrame(f model.RawFrame) {
	if tr := s.e.tr; tr != nil {
		tr.HandleFrame(f)
	}
}

func (s injectSink) HandleUpdate(u model.DecodedUpdate) {
	if tr := s.e.tr; tr != nil {
		tr.HandleUpdate(u)
	}
}

// onClockEvent forwards receiver clock epochs of the local feeds to the
// uplinks and, in aggregator mode, to the ingest as clock events of the host
// station (before the first frame of the new epoch, as feed.Manager
// guarantees).
func (e *Engine) onClockEvent(feedID string, ev feed.ClockEvent) {
	if nc := e.net; nc != nil && nc.core != nil {
		nc.core.ClockEvent(model.HostStation, ingest.ClockEvent{
			OldEpoch: ev.OldEpoch, NewEpoch: ev.NewEpoch, Reason: ev.Reason,
			SamplesLost: int64(ev.SamplesLost), HostNs: ntp.Now().UnixNano(),
		})
	}
	if m := e.up.Load(); m != nil && m.Active() {
		m.HandleClockEvent(feedID, ev)
	}
}

// safeName keeps [A-Za-z0-9 _.-] and at most 32 characters.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 32 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ' ', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// uplinkPosition is the station position for MLAT outputs (ellipsoid
// height in metres, as the mlat-client protocol's "alt" is commonly taken).
func (e *Engine) uplinkPosition() (lat, lon, altM float64, ok bool) {
	sp := e.Station()
	if sp.Source == "none" {
		return 0, 0, 0, false
	}
	g, ok := hostGeo(sp)
	if !ok {
		return 0, 0, 0, false
	}
	return g.Lat, g.Lon, g.HAE, true
}
