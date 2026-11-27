package server

import (
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"airview/internal/ingest/beastin"
	"airview/internal/model"
	"airview/internal/registry"
)

// Network wires the aggregator-mode services into the server. cmd/airview
// (or the engine) calls SetNetwork after opening them; every field is
// optional and a route whose service is missing answers 503.
//
//	srv.SetNetwork(server.Network{
//	    Registry: reg,
//	    Links:    []server.LinkSource{server.BeastLinks(beastSrv), avfSrv, mlatcSrv},
//	    Beast:    beastSrv,
//	    Uplinks:  uplinkManager,
//	})
type Network struct {
	// Registry is the station registry (stations.db).
	Registry *registry.Registry
	// Links report the live feeder connections of every ingest listener;
	// they drive the Feed Check page (/api/me/check), compat claims (same
	// IP) and the admin link view.
	Links []LinkSource
	// Beast is the 30004 listener for the admin ingest view.
	Beast BeastAdmin
	// Uplinks is the uplink manager's status handler (internal/uplink
	// Manager), mounted at /api/uplinks. It is used in standalone mode too.
	Uplinks http.Handler
}

// LinkSource is implemented by an ingest listener (avfsrv, beastin through
// BeastLinks, mlatc).
type LinkSource interface {
	LiveLinks() []LiveLink
}

// LiveLink is one live feeder connection. IP stays in memory: it is used
// to match a visitor's address and is shown only to the admin.
type LiveLink struct {
	Proto   string           // "avf" | "beast" | "mlatc"
	Station model.StationIdx // attached station (0 = none yet)
	// UUID is the compat UUID ("" = none sent); FeederID the native feeder.
	UUID     string
	FeederID string
	IP       netip.Addr
	Since    time.Time
	MsgRate  float64 // frames/s
	PosRate  float64 // positions/s (0 = unknown)
	KBps     float64 // received kB/s
	Bytes    uint64  // received bytes on this connection (0 = unknown)
	RTTMs    float64 // round-trip time (0 = unknown)
	// ClockClass is the clock class name ("" = unknown).
	ClockClass string
	// Synced reports MLAT clock synchronisation (nil = not applicable).
	Synced   *bool
	Software string
	Pending  bool // admission pending
	// Results is the number of MLAT fixes sent to this link (nil = the
	// listener does not report it). Shown to the station's owner.
	Results *uint64
	// SplitSync: the mlat-client link was offered split_sync (nil = not an
	// mlat-client link). ADSBRate: ADS-B positions per second of its last
	// rate_report. HostClampMs: the correction applied to an AVF feeder's
	// host clock (0 = none).
	SplitSync   *bool
	ADSBRate    float64
	HostClampMs float64
}

// BeastAdmin is the admin view of the 30004 listener (*beastin.Server).
type BeastAdmin interface {
	Stats() beastin.Stats
	Links() []beastin.LinkStats
	Disconnect(id uint64) bool
}

// netState is the installed Network (replaced atomically).
type netState struct{ p atomic.Pointer[Network] }

// SetNetwork installs the aggregator services (see Network). It may be
// called before Start or at any time later; nil fields disable the routes
// that need them.
func (s *Server) SetNetwork(n Network) {
	c := n
	s.netw.p.Store(&c)
	s.cache.invalidate()
}

func (s *Server) network() Network {
	if n := s.netw.p.Load(); n != nil {
		return *n
	}
	return Network{}
}

func (s *Server) registry() *registry.Registry { return s.network().Registry }

// liveLinks collects the live links of every source.
func (s *Server) liveLinks() []LiveLink {
	var out []LiveLink
	for _, src := range s.network().Links {
		if src != nil {
			out = append(out, src.LiveLinks()...)
		}
	}
	return out
}

// BeastLinks adapts the 30004 listener to LinkSource.
func BeastLinks(b BeastAdmin) LinkSource { return beastLinks{b} }

type beastLinks struct{ b BeastAdmin }

func (b beastLinks) LiveLinks() []LiveLink {
	if b.b == nil {
		return nil
	}
	ls := b.b.Links()
	out := make([]LiveLink, 0, len(ls))
	for _, l := range ls {
		if l.Closed {
			continue
		}
		ll := LiveLink{Proto: "beast", UUID: l.UUID, IP: parseRemote(l.Remote), Since: l.Connected,
			MsgRate: l.FramesPerSec, KBps: l.BytesPerSec / 1000, RTTMs: l.RTTAvgMs, Pending: l.Pending}
		for _, st := range l.Stations {
			if st.Station != 0 && !st.Refused {
				ll.Station, ll.ClockClass = st.Station, st.ClockClass
				break
			}
		}
		out = append(out, ll)
	}
	return out
}

// parseRemote parses "ip:port" or "ip".
func parseRemote(s string) netip.Addr {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap()
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	a, _ := netip.ParseAddr(s)
	return a.Unmap()
}
