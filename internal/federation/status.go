package federation

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"path/filepath"

	"airview/internal/avf"
)

// Status is the federation status (admin scope only: it names peer
// addresses). It carries no feeder data.
type Status struct {
	Enabled   bool         `json:"enabled"`
	NodeID    string       `json:"nodeId"`
	Name      string       `json:"name,omitempty"`
	Listen    string       `json:"listen,omitempty"`
	StartedMs int64        `json:"startedMs"`
	Transit   bool         `json:"transit"`
	MaxHops   int          `json:"maxHops"`
	Peers     []PeerStatus `json:"peers"`
	Remote    RemoteStatus `json:"received"`
	Feeds     []FeedStatus `json:"outFeeds"`
}

// PeerStatus is one peer's state.
type PeerStatus struct {
	Name        string  `json:"name"`
	ID          string  `json:"id"`
	Addr        string  `json:"addr,omitempty"`
	State       string  `json:"state"`
	Direction   string  `json:"direction,omitempty"` // "in" | "out" while connected
	SinceMs     int64   `json:"sinceMs,omitempty"`
	RTTMs       float64 `json:"rttMs,omitempty"`
	Connects    uint64  `json:"connects"`
	LastError   string  `json:"lastError,omitempty"`
	LastErrMs   int64   `json:"lastErrorMs,omitempty"`
	PeerName    string  `json:"peerName,omitempty"`
	PeerVer     string  `json:"peerVersion,omitempty"`
	PeerInfo    *Info   `json:"peerInfo,omitempty"` // what the peer says it sends
	AircraftIn  uint64  `json:"aircraftIn"`
	AircraftOut uint64  `json:"aircraftOut"`
	MLATIn      uint64  `json:"mlatIn"`
	MLATOut     uint64  `json:"mlatOut"`
	Rejected    uint64  `json:"rejected"`
	Dropped     uint64  `json:"dropped"`
	Remote      int     `json:"remoteAircraft"` // store entries this peer supplied
	WireIn      uint64  `json:"wireBytesIn"`
	WireOut     uint64  `json:"wireBytesOut"`
}

// RemoteStatus summarises received data and the loop guards.
type RemoteStatus struct {
	Aircraft    int    `json:"aircraft"`
	Fixes       int    `json:"fixes"`
	Loops       uint64 `json:"loopsDropped"`
	HopDrops    uint64 `json:"hopLimitDropped"`
	Spoofed     uint64 `json:"pathSpoofDropped"`
	ProtoErrors uint64 `json:"protocolErrors"`
	Denied      uint64 `json:"deniedHandshakes"`
	Exports     uint64 `json:"exports"`
}

// Status returns a snapshot of the node's state.
func (n *Node) Status() Status {
	st := Status{Enabled: n.cfg.Enabled, NodeID: n.id, Name: n.cfg.Name, Listen: n.cfg.Listen,
		StartedMs: n.startMs, Transit: n.cfg.Transit, MaxHops: n.cfg.MaxHops, Peers: []PeerStatus{}, Feeds: []FeedStatus{}}
	if a := n.Addr(); a != nil {
		st.Listen = a.String()
	}
	for _, p := range n.order {
		p.mu.Lock()
		ps := PeerStatus{Name: p.cfg.Name, ID: p.cfg.ID, Addr: p.cfg.Addr, State: p.state, Connects: p.connects,
			LastError: p.lastErr, LastErrMs: p.lastErrMs, AircraftIn: p.totalIn, AircraftOut: p.totalOut}
		if p.info != nil {
			in := *p.info
			ps.PeerInfo = &in
		}
		l := p.link
		if l != nil {
			ps.PeerName, ps.PeerVer = p.pinfo.Name, p.pinfo.Version
		}
		p.mu.Unlock()
		if l != nil {
			ps.Direction = "in"
			if l.outbound {
				ps.Direction = "out"
			}
			ps.SinceMs = l.since.UnixMilli()
			ps.RTTMs = float64(l.rttNs.Load()/1000) / 1000
			ps.AircraftIn += l.acIn.Load()
			ps.AircraftOut += l.acOut.Load()
			ps.MLATIn, ps.MLATOut = l.mlatIn.Load(), l.mlatOut.Load()
			ps.Rejected, ps.Dropped = l.rejected.Load(), l.dropped.Load()
			ps.WireOut = l.w.Stats().WireBytes
			ps.WireIn = l.sr.Stats().WireBytes
		}
		ps.Remote = n.store.fromPeer(p.cfg.ID)
		st.Peers = append(st.Peers, ps)
	}
	n.mlatMu.Lock()
	st.Remote.Fixes = len(n.remoteFix)
	n.mlatMu.Unlock()
	st.Remote.Aircraft = n.store.len()
	st.Remote.Loops, st.Remote.HopDrops, st.Remote.Spoofed = n.stats.loops.Load(), n.stats.hopDrops.Load(), n.stats.spoofed.Load()
	st.Remote.ProtoErrors, st.Remote.Denied, st.Remote.Exports = n.stats.protoErrors.Load(), n.stats.denied.Load(), n.stats.exports.Load()
	for _, f := range n.feeds {
		st.Feeds = append(st.Feeds, f.status())
	}
	return st
}

// StatusHandler serves Status as JSON (GET only). Mount it on an
// admin-scoped route: it lists peer addresses.
func (n *Node) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(n.Status())
	})
}

// AircraftJSONHandler serves AircraftJSON(p) (tar1090 aircraft.json shape)
// for networks that poll instead of receiving a push. It is as safe as the
// public map; rate limiting is the server's job.
func (n *Node) AircraftJSONHandler(p Policy) http.Handler {
	p.normalize()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(n.AircraftJSON(&p))
	})
}

// File names below the data directory.
const (
	IdentityFileName = "federation.key"
	TLSDirName       = "fed-tls"
)

// Material is what LoadMaterial returns for New: the identity, the
// listener certificate and its pin (for the peer operator's pin setting).
type Material struct {
	Identity    avf.Identity
	Created     bool // the identity was created now
	Certificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	Pin         string
}

// LoadMaterial loads or creates the node identity (cfg.IdentityFile or
// <dataDir>/federation.key, mode 0600) and the listener certificate
// (cfg.TLSCert/TLSKey reloaded on change, else a self-signed one kept in
// <dataDir>/fed-tls). hosts are the certificate names of a self-signed
// certificate.
func LoadMaterial(dataDir string, cfg *Config, hosts []string) (Material, error) {
	var m Material
	path := cfg.IdentityFile
	if path == "" {
		path = filepath.Join(dataDir, IdentityFileName)
	}
	id, created, err := avf.LoadOrCreateIdentity(path)
	if err != nil {
		return m, err
	}
	m.Identity, m.Created = id, created
	if cfg.Listen == "" {
		return m, nil
	}
	if cfg.TLSCert != "" {
		r, err := avf.NewCertReloader(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return m, err
		}
		m.Certificate = r.GetCertificate
		if c, err := r.Current(); err == nil {
			m.Pin, _ = avf.TLSCertPin(c)
		}
		return m, nil
	}
	if len(hosts) == 0 {
		hosts = []string{"airview-federation"}
	}
	c, pin, err := avf.LoadOrCreateSelfSigned(filepath.Join(dataDir, TLSDirName), hosts)
	if err != nil {
		return m, err
	}
	m.Certificate, m.Pin = avf.StaticCertificate(c), pin
	return m, nil
}
