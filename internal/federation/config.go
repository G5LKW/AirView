package federation

import (
	"fmt"
	"math"
	"net"
	"net/netip"
	"strings"
	"time"

	"airview/internal/model"
)

// DefaultPort is the suggested TCP port of the federation listener.
const DefaultPort = 30170

// Defaults (Config.Normalize fills zero values with these).
const (
	DefaultIntervalMs     = 1000
	DefaultMLATIntervalMs = 250
	DefaultFullRefreshS   = 30
	DefaultTTLS           = 60
	DefaultMaxHops        = 3
	MaxMaxHops            = 8
	DefaultMaxRemote      = 50_000
	DefaultMaxInflateBps  = 16_000_000
	DefaultQueueRecords   = 256
	DefaultFeedIntervalMs = 1000
	DefaultFeedMaxClients = 16
	MaxDelayS             = 600
)

// Policy values.
const (
	ShareAll    = "all"    // every aircraft with its best position
	ShareNoMLAT = "nomlat" // aircraft, but MLAT-derived positions are removed
	ShareNone   = "none"   // nothing

	IslandCoarsen = "coarsen" // island-guard aircraft are rounded to 0.05 deg (as on the public map)
	IslandDrop    = "drop"    // island-guard aircraft are not shared at all
)

// Out-feed formats and modes.
const (
	FormatBeast = "beast"
	FormatJSON  = "json"
	ModeListen  = "listen"
	ModeConnect = "connect"
)

// Config is the federation block (aggregator.federation in the config
// file; see WIRING.md for the file/env mapping). Zero values select the
// defaults of Normalize.
type Config struct {
	// Enabled switches federation on. Off, nothing listens or dials.
	Enabled bool `json:"enabled"`
	// Name is this network's display name sent to peers (optional).
	Name string `json:"name,omitempty"`
	// Listen is the address of the inbound federation listener
	// ("" = accept no inbound peers, e.g. ":30170").
	Listen string `json:"listen,omitempty"`
	// IdentityFile is the node's Ed25519 key file ("" = federation.key in
	// the data directory). The node ID shown to peer operators derives
	// from it.
	IdentityFile string `json:"identityFile,omitempty"`
	// TLSCert / TLSKey are an operator certificate for the listener; empty
	// = a self-signed certificate kept in <data>/fed-tls.
	TLSCert string `json:"tlsCert,omitempty"`
	TLSKey  string `json:"tlsKey,omitempty"`
	// IntervalMs is the aircraft-state export period (default 1000).
	IntervalMs int `json:"intervalMs,omitempty"`
	// MLATIntervalMs is the MLAT-result flush period (default 250).
	MLATIntervalMs int `json:"mlatIntervalMs,omitempty"`
	// FullRefreshS: every this many seconds a peer gets every aircraft,
	// not only the changed ones (default 30).
	FullRefreshS int `json:"fullRefreshS,omitempty"`
	// TTLS is how long a remote aircraft stays without an update (default 60).
	TTLS int `json:"ttlS,omitempty"`
	// Transit allows forwarding data received from one peer to other peers
	// (each peer must also allow it in send.transit). Default off: only
	// this network's own aircraft are shared.
	Transit bool `json:"transit,omitempty"`
	// MaxHops bounds the path length of forwarded data, origin included
	// (default 3, at most 8). Data with a longer path is dropped.
	MaxHops int `json:"maxHops,omitempty"`
	// MaxRemoteAircraft bounds the remote aircraft store (default 50000).
	MaxRemoteAircraft int `json:"maxRemoteAircraft,omitempty"`
	// MaxInflateBps bounds the decompressed inbound rate per peer in bytes
	// per second (default 16 MB/s).
	MaxInflateBps int64 `json:"maxInflateBps,omitempty"`
	// QueueRecords bounds the per-peer send queue (default 256 records).
	// A full queue drops the batch and schedules a full refresh.
	QueueRecords int `json:"queueRecords,omitempty"`
	// HideRemote keeps received aircraft off the map (they are still
	// stored, forwarded per policy and listed in the status). Default:
	// the map shows them as an overlay (engine/server wiring).
	HideRemote bool `json:"hideRemote,omitempty"`
	// Inject additionally hands received aircraft and MLAT results to the
	// tracker as updates with FeedID "fed:<peer>" (engine wiring,
	// Options.Sink). Default off: the overlay alone keeps remote data out
	// of statistics, records and the island guard.
	Inject bool `json:"inject,omitempty"`
	// Peers are the federated networks.
	Peers []PeerConfig `json:"peers,omitempty"`
	// Feeds are federation-safe outbound feeds to other networks.
	Feeds []FeedConfig `json:"feeds,omitempty"`
}

// PeerConfig is one federated AirView network.
type PeerConfig struct {
	// Name is the local label of the peer (status page, logs).
	Name string `json:"name"`
	// ID is the peer's node ID (16 base32 characters, shown by the peer's
	// `airview federation id`). It is mandatory: the peer must prove it in
	// the handshake.
	ID string `json:"id"`
	// Addr is "host:port" to dial. Empty = this side only accepts the peer.
	Addr string `json:"addr,omitempty"`
	// Pin optionally pins the peer listener's TLS key ("sha256/<b64>") or
	// requires a WebPKI certificate ("webpki"). Empty relies on the
	// mutual Ed25519 identity check, which is bound to the TLS session.
	Pin string `json:"pin,omitempty"`
	// Disabled keeps the entry but neither dials nor accepts it.
	Disabled bool `json:"disabled,omitempty"`
	// Send is what this network shares with the peer.
	Send Policy `json:"send"`
	// Receive is what this network accepts from the peer.
	Receive ReceivePolicy `json:"receive"`
}

// Box is a latitude/longitude rectangle in degrees. West > East crosses the
// antimeridian.
type Box struct {
	South float64 `json:"south"`
	West  float64 `json:"west"`
	North float64 `json:"north"`
	East  float64 `json:"east"`
}

// Contains reports whether (lat, lon) lies inside the box (edges included).
func (b Box) Contains(lat, lon float64) bool {
	if lat < b.South || lat > b.North {
		return false
	}
	if b.West <= b.East {
		return lon >= b.West && lon <= b.East
	}
	return lon >= b.West || lon <= b.East
}

func (b Box) valid() bool {
	ok := func(v, lim float64) bool { return !math.IsNaN(v) && v >= -lim && v <= lim }
	return ok(b.South, 90) && ok(b.North, 90) && ok(b.West, 180) && ok(b.East, 180) && b.South <= b.North
}

// Policy says what is shared with one peer or out-feed. Feeder identities,
// station positions, receiver lists, signal levels, distances and IP
// addresses are never shared whatever the policy says: the wire format has
// no fields for them.
type Policy struct {
	// Aircraft: "all" (default), "nomlat" (MLAT positions removed) or "none".
	Aircraft string `json:"aircraft,omitempty"`
	// MLAT shares MLAT result records (peers only; default off).
	MLAT bool `json:"mlat,omitempty"`
	// Transit forwards data of other peers to this peer (needs
	// Config.Transit too; default off).
	Transit bool `json:"transit,omitempty"`
	// Island is what happens to aircraft the island guard flags (low
	// aircraft heard only by hidden-pin stations): "coarsen" (default,
	// as on the public map) or "drop".
	Island string `json:"island,omitempty"`
	// MinAltFt drops aircraft below this barometric (else geometric)
	// altitude and aircraft on the ground (0 = no floor).
	MinAltFt int `json:"minAltFt,omitempty"`
	// NoGround drops aircraft on the ground.
	NoGround bool `json:"noGround,omitempty"`
	// NoMilitary drops aircraft the reference data marks military.
	NoMilitary bool `json:"noMilitary,omitempty"`
	// Boxes limits sharing to positions inside any box (empty = anywhere).
	// Aircraft without a position are shared only when Boxes is empty.
	Boxes []Box `json:"boxes,omitempty"`
	// ExcludeBoxes drops positions inside any of these boxes.
	ExcludeBoxes []Box `json:"excludeBoxes,omitempty"`
	// DelayS delays everything sent to this peer by this many seconds
	// (0..600; peers only).
	DelayS int `json:"delayS,omitempty"`
	// MaxAircraft caps the aircraft per export (0 = no cap).
	MaxAircraft int `json:"maxAircraft,omitempty"`
}

// ReceivePolicy says what is accepted from a peer.
type ReceivePolicy struct {
	// Aircraft: "all" (default) or "none".
	Aircraft string `json:"aircraft,omitempty"`
	// MLAT: "all" (default) or "none".
	MLAT string `json:"mlat,omitempty"`
	// Boxes keeps only positions inside any box (empty = anywhere).
	Boxes []Box `json:"boxes,omitempty"`
	// MaxAircraft bounds the aircraft held from this peer (default 20000).
	MaxAircraft int `json:"maxAircraft,omitempty"`
}

// FeedConfig is one federation-safe outbound feed for another network.
type FeedConfig struct {
	// Name is the label (status, logs).
	Name string `json:"name"`
	// Format: "beast" (raw Mode S, timestamps and signal zeroed) or "json"
	// (one tar1090-style aircraft file per line).
	Format string `json:"format"`
	// Mode: "connect" (dial Addr and push) or "listen" (serve on Addr).
	Mode string `json:"mode"`
	// Addr is "host:port".
	Addr string `json:"addr"`
	// Allow lists the client networks of a listen feed (CIDR or address;
	// default loopback only).
	Allow []string `json:"allow,omitempty"`
	// MaxClients bounds the clients of a listen feed (default 16).
	MaxClients int `json:"maxClients,omitempty"`
	// IntervalMs is the JSON emission period (default 1000).
	IntervalMs int `json:"intervalMs,omitempty"`
	// ModeAC includes Mode A/C replies in a Beast feed (default off).
	ModeAC bool `json:"modeAC,omitempty"`
	// Disabled keeps the entry without running it.
	Disabled bool `json:"disabled,omitempty"`
	// Policy filters the feed. MLAT, Transit and DelayS do not apply;
	// a Beast feed never carries MLAT results (they are not off-air
	// frames) and applies Island "drop" to every flagged aircraft (a raw
	// frame cannot be coarsened).
	Policy Policy `json:"policy"`
}

// Normalize fills defaults in place. It never fails; Check reports
// problems.
func (c *Config) Normalize() {
	def := func(p *int, v int) {
		if *p <= 0 {
			*p = v
		}
	}
	def(&c.IntervalMs, DefaultIntervalMs)
	def(&c.MLATIntervalMs, DefaultMLATIntervalMs)
	def(&c.FullRefreshS, DefaultFullRefreshS)
	def(&c.TTLS, DefaultTTLS)
	def(&c.MaxHops, DefaultMaxHops)
	if c.MaxHops > MaxMaxHops {
		c.MaxHops = MaxMaxHops
	}
	def(&c.MaxRemoteAircraft, DefaultMaxRemote)
	def(&c.QueueRecords, DefaultQueueRecords)
	if c.MaxInflateBps <= 0 {
		c.MaxInflateBps = DefaultMaxInflateBps
	}
	c.Name = strings.TrimSpace(c.Name)
	for i := range c.Peers {
		p := &c.Peers[i]
		p.ID = strings.ToLower(strings.TrimSpace(p.ID))
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			p.Name = p.ID
		}
		p.Send.normalize()
		if p.Receive.Aircraft == "" {
			p.Receive.Aircraft = ShareAll
		}
		if p.Receive.MLAT == "" {
			p.Receive.MLAT = ShareAll
		}
		def(&p.Receive.MaxAircraft, 20_000)
	}
	for i := range c.Feeds {
		f := &c.Feeds[i]
		f.Format = strings.ToLower(strings.TrimSpace(f.Format))
		f.Mode = strings.ToLower(strings.TrimSpace(f.Mode))
		if f.Name == "" {
			f.Name = fmt.Sprintf("%s-%d", f.Format, i)
		}
		def(&f.IntervalMs, DefaultFeedIntervalMs)
		def(&f.MaxClients, DefaultFeedMaxClients)
		if len(f.Allow) == 0 {
			f.Allow = []string{"127.0.0.0/8", "::1/128"}
		}
		f.Policy.normalize()
		f.Policy.MLAT, f.Policy.Transit, f.Policy.DelayS = false, false, 0
	}
}

func (p *Policy) normalize() {
	if p.Aircraft == "" {
		p.Aircraft = ShareAll
	}
	if p.Island == "" {
		p.Island = IslandCoarsen
	}
}

// Interval returns the export period.
func (c *Config) Interval() time.Duration { return time.Duration(c.IntervalMs) * time.Millisecond }

// Check validates a normalized configuration and returns one message per
// problem, each prefixed with its config path below prefix (for example
// "aggregator.federation"). An empty result means the block is valid.
func (c *Config) Check(prefix string) []string {
	var out []string
	add := func(path, format string, args ...any) {
		out = append(out, prefix+"."+path+": "+fmt.Sprintf(format, args...))
	}
	if c.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Listen); err != nil {
			add("listen", "%v", err)
		}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		add("tlsCert", "tlsCert and tlsKey must be set together")
	}
	rng := func(path string, v, lo, hi int) {
		if v < lo || v > hi {
			add(path, "%d out of range %d..%d", v, lo, hi)
		}
	}
	rng("intervalMs", c.IntervalMs, 100, 60_000)
	rng("mlatIntervalMs", c.MLATIntervalMs, 50, 10_000)
	rng("fullRefreshS", c.FullRefreshS, 1, 3600)
	rng("ttlS", c.TTLS, 5, 3600)
	rng("maxHops", c.MaxHops, 1, MaxMaxHops)
	rng("maxRemoteAircraft", c.MaxRemoteAircraft, 100, 1_000_000)
	rng("queueRecords", c.QueueRecords, 8, 100_000)
	if c.MaxInflateBps < 100_000 {
		add("maxInflateBps", "%d below 100000", c.MaxInflateBps)
	}
	seen := map[string]int{}
	for i, p := range c.Peers {
		pp := fmt.Sprintf("peers[%d]", i)
		if !model.ValidID(p.ID) {
			add(pp+".id", "%q is not a node ID (16 base32 characters)", p.ID)
		} else if j, dup := seen[p.ID]; dup {
			add(pp+".id", "duplicate of peers[%d]", j)
		} else {
			seen[p.ID] = i
		}
		if p.Addr != "" {
			if _, port, err := net.SplitHostPort(p.Addr); err != nil || port == "" {
				add(pp+".addr", "want host:port, got %q", p.Addr)
			}
		}
		if p.Pin != "" && p.Pin != PinWebPKI && !strings.HasPrefix(p.Pin, "sha256/") {
			add(pp+".pin", "want \"\", %q or \"sha256/<base64>\"", PinWebPKI)
		}
		out = append(out, p.Send.check(prefix+"."+pp+".send", true)...)
		switch p.Receive.Aircraft {
		case ShareAll, ShareNone:
		default:
			add(pp+".receive.aircraft", "want all or none, got %q", p.Receive.Aircraft)
		}
		switch p.Receive.MLAT {
		case ShareAll, ShareNone:
		default:
			add(pp+".receive.mlat", "want all or none, got %q", p.Receive.MLAT)
		}
		for j, b := range p.Receive.Boxes {
			if !b.valid() {
				add(fmt.Sprintf("%s.receive.boxes[%d]", pp, j), "invalid box")
			}
		}
		rng(pp+".receive.maxAircraft", p.Receive.MaxAircraft, 1, 1_000_000)
	}
	names := map[string]bool{}
	for i, f := range c.Feeds {
		fp := fmt.Sprintf("feeds[%d]", i)
		if names[f.Name] {
			add(fp+".name", "duplicate name %q", f.Name)
		}
		names[f.Name] = true
		if f.Format != FormatBeast && f.Format != FormatJSON {
			add(fp+".format", "want beast or json, got %q", f.Format)
		}
		if f.Mode != ModeListen && f.Mode != ModeConnect {
			add(fp+".mode", "want listen or connect, got %q", f.Mode)
		}
		if _, _, err := net.SplitHostPort(f.Addr); err != nil {
			add(fp+".addr", "%v", err)
		}
		for j, a := range f.Allow {
			if _, err := parsePrefix(a); err != nil {
				add(fmt.Sprintf("%s.allow[%d]", fp, j), "%v", err)
			}
		}
		rng(fp+".intervalMs", f.IntervalMs, 100, 60_000)
		rng(fp+".maxClients", f.MaxClients, 1, 1000)
		out = append(out, f.Policy.check(prefix+"."+fp+".policy", false)...)
	}
	return out
}

func (p *Policy) check(prefix string, peer bool) []string {
	var out []string
	add := func(path, format string, args ...any) {
		out = append(out, prefix+"."+path+": "+fmt.Sprintf(format, args...))
	}
	switch p.Aircraft {
	case ShareAll, ShareNoMLAT, ShareNone:
	default:
		add("aircraft", "want all, nomlat or none, got %q", p.Aircraft)
	}
	switch p.Island {
	case IslandCoarsen, IslandDrop:
	default:
		add("island", "want coarsen or drop, got %q", p.Island)
	}
	if p.MinAltFt < 0 || p.MinAltFt > 60_000 {
		add("minAltFt", "%d out of range 0..60000", p.MinAltFt)
	}
	if p.DelayS < 0 || p.DelayS > MaxDelayS {
		add("delayS", "%d out of range 0..%d", p.DelayS, MaxDelayS)
	}
	if p.MaxAircraft < 0 {
		add("maxAircraft", "negative")
	}
	for j, b := range p.Boxes {
		if !b.valid() {
			add(fmt.Sprintf("boxes[%d]", j), "invalid box")
		}
	}
	for j, b := range p.ExcludeBoxes {
		if !b.valid() {
			add(fmt.Sprintf("excludeBoxes[%d]", j), "invalid box")
		}
	}
	return out
}

func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}
