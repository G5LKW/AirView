package main

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"airview/internal/engine"
	"airview/internal/ingest/beastin"
	"airview/internal/model"
	"airview/internal/server"
)

// wireNetwork installs the aggregator services (registry, live links, the
// Beast listener's admin view) and the uplink status handler in the HTTP
// server. The engine replaces the Beast listener on a port change and
// creates the uplink manager on first use, so the server gets adapters that
// ask the engine on every call instead of fixed pointers.
func (a *app) wireNetwork() {
	n := server.Network{Uplinks: engineUplinks{a.eng}}
	if a.eng.Aggregator() {
		b := engineBeast{a.eng}
		n.Registry = a.eng.Registry()
		n.Beast = b
		n.Links = []server.LinkSource{server.BeastLinks(b), engineAVFLinks{e: a.eng, r: &avfRates{}}, engineMLATCLinks{a.eng}}
	}
	a.srv.SetNetwork(n)
}

// engineBeast is server.BeastAdmin over the engine's current Beast
// listener (empty while it is off).
type engineBeast struct{ e *engine.Engine }

func (b engineBeast) Stats() beastin.Stats {
	if s := b.e.BeastIngest(); s != nil {
		return s.Stats()
	}
	return beastin.Stats{}
}

func (b engineBeast) Links() []beastin.LinkStats {
	if s := b.e.BeastIngest(); s != nil {
		return s.Links()
	}
	return nil
}

func (b engineBeast) Disconnect(id uint64) bool {
	if s := b.e.BeastIngest(); s != nil {
		return s.Disconnect(id)
	}
	return false
}

// engineUplinks serves the uplink manager's status API; before an uplink
// was ever configured it answers with an empty list.
type engineUplinks struct{ e *engine.Engine }

func (u engineUplinks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m := u.e.Uplinks(); m != nil {
		m.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		http.Error(w, "no uplink is configured", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"feederId":"","uuid":"","uplinks":[]}`))
}

// engineAVFLinks reports the live AVF/1 sessions (one LiveLink per
// receiver) of the engine's current AVF listener.
type engineAVFLinks struct {
	e *engine.Engine
	r *avfRates
}

// avfRates turns the cumulative session counters (frames, wire bytes)
// into rates (UX-01): a session's rate is the difference between two
// samples at least 5 s apart.
type avfRates struct {
	mu   sync.Mutex
	last map[string]avfSample
}

type avfSample struct {
	frames, bytes uint64
	at            time.Time
	fps, kBps     float64
	seen          bool
}

// rates returns the frame rate and kB/s of the session key.
func (r *avfRates) rates(key string, frames, bytes uint64, now time.Time) (fps, kBps float64) {
	if r == nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[string]avfSample{}
	}
	for k, v := range r.last { // forget closed sessions
		if now.Sub(v.at) > 10*time.Minute {
			delete(r.last, k)
		}
	}
	p, ok := r.last[key]
	if !ok || frames < p.frames || bytes < p.bytes {
		r.last[key] = avfSample{frames: frames, bytes: bytes, at: now}
		return 0, 0
	}
	if dt := now.Sub(p.at).Seconds(); dt >= 5 {
		p.fps = float64(frames-p.frames) / dt
		p.kBps = float64(bytes-p.bytes) / dt / 1000
		p.frames, p.bytes, p.at = frames, bytes, now
		r.last[key] = p
	}
	return p.fps, p.kBps
}

func (l engineAVFLinks) LiveLinks() []server.LiveLink {
	srv, _ := l.e.AVFIngest()
	if srv == nil {
		return nil
	}
	now := time.Now()
	var out []server.LiveLink
	for _, s := range srv.Sessions() {
		fps, kBps := l.r.rates(s.FeederID+"|"+strconv.FormatInt(s.StartedMs, 10), s.FramesIn, s.WireBytes, now)
		sw := s.Software
		if sw != "" && s.Version != "" {
			sw += " " + s.Version
		}
		base := server.LiveLink{Proto: "avf", FeederID: s.FeederID, IP: remoteAddr(s.Remote),
			Since: time.UnixMilli(s.StartedMs), RTTMs: s.RTTMs, Software: sw, HostClampMs: s.HostClampMs,
			MsgRate: fps, KBps: kBps, Bytes: s.WireBytes}
		if len(s.Stations) == 0 {
			out = append(out, base)
			continue
		}
		for _, st := range s.Stations {
			ll := base
			ll.Station, ll.ClockClass, ll.Pending = st.Station, st.Class, st.Status == "pending"
			res := st.Results
			ll.Results = &res
			out = append(out, ll)
		}
	}
	return out
}

// engineMLATCLinks reports the live mlat-client links of the engine's
// current mlat-client listener, with the MLAT clock state of their station.
type engineMLATCLinks struct{ e *engine.Engine }

func (l engineMLATCLinks) LiveLinks() []server.LiveLink {
	srv := l.e.MLATCIngest()
	if srv == nil {
		return nil
	}
	eng := l.e.MLAT()
	var out []server.LiveLink
	for _, c := range srv.Links() {
		ll := server.LiveLink{Proto: "mlatc", Station: c.Station, UUID: c.UUID, IP: c.IP, Since: c.Since,
			MsgRate: c.MsgRate, KBps: c.KBps, ClockClass: c.ClockClass, Software: c.ClientVersion,
			Pending: c.Admission == model.AdmPending, ADSBRate: c.ADSBRate}
		split := c.SplitSync
		ll.SplitSync = &split
		res := c.Results
		ll.Results = &res
		if eng != nil && c.Station != 0 {
			if rep, ok := eng.Station(c.Station); ok && rep.Clock != nil {
				synced := rep.Clock.Synced
				ll.Synced = &synced
			}
		}
		out = append(out, ll)
	}
	return out
}

// remoteAddr parses "ip:port" or "ip" (zero Addr when neither).
func remoteAddr(s string) netip.Addr {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap()
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	a, _ := netip.ParseAddr(s)
	return a.Unmap()
}
