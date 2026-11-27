package mlatc

import (
	"log/slog"
	"net/netip"
	"runtime"
	"strconv"
	"time"

	"airview/internal/model"
)

// Policy holds the limits that may change at run time (SetPolicy).
type Policy struct {
	// MaxClients caps concurrent connections (default 4096).
	MaxClients int
	// MaxConnsPerIP / MaxConnsPerSubnet (IPv4 /24, IPv6 /48) / MaxUnauthPerIP
	// (connections still in the handshake) / PerIPStations (UUID-less
	// clients per address get separate stations; this caps the stations per
	// address). Defaults 16 / 32 / 4 / 8 (aggregator.limits).
	MaxConnsPerIP     int
	MaxConnsPerSubnet int
	MaxUnauthPerIP    int
	PerIPStations     int
	// AllowCIDRs are exempt from the per-address limits.
	AllowCIDRs []string

	// MaxMsgsPerSec is the per-client observation rate (frames/s, a sync
	// counts two) above which observations are dropped; staying above it
	// for OverLimitSeconds disconnects the client with a cool-down
	// (default aggregator.limits.maxFpsPerStation, 6000).
	MaxMsgsPerSec int
	// MaxKBps is the received (compressed) byte rate per client in kB/s
	// (default aggregator.limits.maxKBpsPerStation, 200). A client that
	// negotiated no compression counts a quarter of its bytes
	// (uncompressedByteFactor), the usual deflate ratio of its JSON lines.
	MaxKBps int
	// OverLimitSeconds (default 5) and CoolDown (default 5 min) for rate
	// offenders; RefusedCoolDown (default 1 min) for refused identities.
	OverLimitSeconds int
	CoolDown         time.Duration
	RefusedCoolDown  time.Duration

	// MaxRequested caps the aircraft one client is asked to send
	// (start_sending), default 2000. MaxSeen caps the remembered seen set
	// (default 10000).
	MaxRequested int
	MaxSeen      int
	// MaxBadLines: a client whose malformed lines exceed this number and
	// half of all its lines is disconnected (default 200).
	MaxBadLines int

	// DisableResults stops result delivery (aggregator.mlat.returnResults
	// false, or MLAT disabled). Results go only to stations that
	// contributed (SendResult is called per contributor by the engine).
	DisableResults bool
	// MOTD is sent in the handshake reply ("" = none).
	MOTD string
}

// DefaultPolicy returns the default limits.
func DefaultPolicy() Policy {
	l := model.DefaultAggregator().Limits
	return Policy{
		MaxClients: 4096, MaxConnsPerIP: l.MaxConnsPerIP, MaxConnsPerSubnet: l.MaxConnsPerSubnet,
		MaxUnauthPerIP: l.MaxUnauthPerIP, PerIPStations: l.PerIPStations, AllowCIDRs: l.AllowCIDRs,
		MaxMsgsPerSec: l.MaxFPSPerStation, MaxKBps: l.MaxKBpsPerStation,
		OverLimitSeconds: 5, CoolDown: 5 * time.Minute, RefusedCoolDown: time.Minute,
		MaxRequested: 2000, MaxSeen: 10000, MaxBadLines: 200,
	}
}

// PolicyFromConfig maps the aggregator configuration (limits and the MLAT
// result switch) onto a Policy.
func PolicyFromConfig(a model.Aggregator) Policy {
	p := DefaultPolicy()
	l := a.Limits
	setPos(&p.MaxConnsPerIP, l.MaxConnsPerIP)
	setPos(&p.MaxConnsPerSubnet, l.MaxConnsPerSubnet)
	setPos(&p.MaxUnauthPerIP, l.MaxUnauthPerIP)
	setPos(&p.PerIPStations, l.PerIPStations)
	setPos(&p.MaxMsgsPerSec, l.MaxFPSPerStation)
	setPos(&p.MaxKBps, l.MaxKBpsPerStation)
	p.AllowCIDRs = append([]string(nil), l.AllowCIDRs...)
	c := a.MLATC
	setPos(&p.MaxRequested, c.MaxRequested)
	setPos(&p.MaxSeen, c.MaxSeen)
	setPos(&p.MaxBadLines, c.MaxBadLines)
	setPos(&p.OverLimitSeconds, c.OverLimitSeconds)
	setPos(&p.MaxClients, c.MaxClients)
	if c.CoolDownSec > 0 {
		p.CoolDown = time.Duration(c.CoolDownSec) * time.Second
	}
	p.DisableResults = !(a.MLAT.Enabled && a.MLAT.ReturnResults)
	if a.Name != "" {
		p.MOTD = sanitize(a.Name, 80)
	}
	return p
}

func setPos(dst *int, v int) {
	if v > 0 {
		*dst = v
	}
}

func (p Policy) normalized() Policy {
	d := DefaultPolicy()
	num := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	num(&p.MaxClients, d.MaxClients)
	num(&p.MaxConnsPerIP, d.MaxConnsPerIP)
	num(&p.MaxConnsPerSubnet, d.MaxConnsPerSubnet)
	num(&p.MaxUnauthPerIP, d.MaxUnauthPerIP)
	num(&p.PerIPStations, d.PerIPStations)
	num(&p.MaxMsgsPerSec, d.MaxMsgsPerSec)
	num(&p.MaxKBps, d.MaxKBps)
	num(&p.OverLimitSeconds, d.OverLimitSeconds)
	num(&p.MaxRequested, d.MaxRequested)
	num(&p.MaxSeen, d.MaxSeen)
	num(&p.MaxBadLines, d.MaxBadLines)
	if p.CoolDown < 0 {
		p.CoolDown = 0
	} else if p.CoolDown == 0 {
		p.CoolDown = d.CoolDown
	}
	if p.RefusedCoolDown < 0 {
		p.RefusedCoolDown = 0
	} else if p.RefusedCoolDown == 0 {
		p.RefusedCoolDown = d.RefusedCoolDown
	}
	return p
}

// compiled policy: parsed CIDRs.
type policy struct {
	Policy
	allow []netip.Prefix
}

func compilePolicy(p Policy) *policy {
	p = p.normalized()
	c := &policy{Policy: p}
	for _, s := range p.AllowCIDRs {
		if pf, err := netip.ParsePrefix(s); err == nil {
			c.allow = append(c.allow, pf.Masked())
		} else if a, err := netip.ParseAddr(s); err == nil {
			a = a.Unmap()
			c.allow = append(c.allow, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return c
}

func (p *policy) allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, pf := range p.allow {
		if pf.Contains(ip) {
			return true
		}
	}
	return false
}

// Options configures a Server. Zero values take the documented defaults.
type Options struct {
	// Addr is the listen address of ListenAndServe (default ":31090").
	Addr string
	// Policy holds the limits (changeable with SetPolicy).
	Policy Policy

	// HandshakeTimeout bounds the wait for the handshake line (default 30 s).
	HandshakeTimeout time.Duration
	// IdleTimeout drops a client silent this long (default 150 s, DESIGN
	// §5.2). The reference client sends a heartbeat every 120 s at most.
	IdleTimeout time.Duration
	// HeartbeatEvery is the server heartbeat period (default 30 s; the
	// reference client gives up after 60 s without data).
	HeartbeatEvery time.Duration
	// StatsEvery is the period of "stats" messages when a StatsSource is
	// set (default 60 s).
	StatsEvery time.Duration
	// FlushEvery batches start/stop_sending updates (default 1 s).
	FlushEvery time.Duration
	// WriteTimeout bounds one write to a client (default 10 s); a client
	// that does not read is disconnected.
	WriteTimeout time.Duration
	// MaxOutQueue is the per-client output queue in bytes; results beyond
	// it are dropped and counted (default 256 KiB).
	MaxOutQueue int
	// BatchFrames is the largest frame batch handed to the Sink (default 64;
	// a batch is also handed over whenever the client's data is consumed).
	BatchFrames int
	// ReconnectIn is the reconnect delay suggested with a deny (default 60 s).
	ReconnectIn time.Duration

	// DisableCompression refuses zlib/zlib2 (default: offered methods are
	// accepted, zlib2 first).
	DisableCompression bool
	// SplitSync decides the split_sync flag of the handshake reply: with it
	// the client sends every sync candidate on its own ("ssync") as it
	// arrives instead of as an even/odd pair once the second half arrived.
	// Pairs delay the first half by the pairing interval (so it misses its
	// transmission's MLAT group, MLATC.md §8) and send it twice; AirView's
	// clock filter needs single frames only.
	//
	// The default (SplitSyncAuto) offers it and falls back per client
	// identity when the client's single-sync messages turn out unusable
	// (see SplitSyncMode); SplitSyncOn offers it without the fallback,
	// SplitSyncOff never offers it.
	SplitSync SplitSyncMode
	// RawTicks: clients send receiver stamps in the AirView convention
	// (unwrapped counters, absolute GPS nanoseconds; no frame-start offsets).
	// For AirView's simulator and test tools only; real mlat-clients need
	// the default.
	RawTicks bool

	// ServerKey keys the HMAC of unidentified IP keys (pass the registry's
	// persistent key; nil = random per process).
	ServerKey []byte
	// Now is the server clock (default time.Now; the engine passes ntp.Now).
	Now func() time.Time
	// Logger (no raw IP addresses are logged).
	Logger *slog.Logger
	// Gate, when set, must return true for clients to be admitted (DESIGN
	// §7: closed until the admin account exists). Refused clients get a
	// deny with a long reconnect delay.
	Gate func() bool
	// Hooks are optional callbacks (planner, registry, station pages).
	Hooks Hooks
	// Stats supplies "stats" message content (nil = no stats messages).
	Stats StatsSource
	// Trust, when set, tells which stations are in timing quarantine
	// (internal/registry/trust). In selective traffic mode such a station
	// is additionally asked for its ProbeAircraft highest-rate ADS-B
	// aircraft from its rate_report, so the quarantine has sync data to
	// judge its timing by even before the planner wants anything from it.
	Trust TrustSource
	// ProbeAircraft is the size of that probe set (default 16; < 0 = none).
	ProbeAircraft int

	// UDP accepts the mlat-client UDP transport (aggregator.mlat.udp,
	// MLATC.md §9): a client that offers udp_transport 2 is answered with
	// [host, port, key] and then sends its timed frames (sync, ssync,
	// mlat) as datagrams; everything else stays on TCP. Serve opens the
	// datagram socket itself (UDPAddr); without one (bind error) clients
	// are answered udp_transport false and keep using TCP.
	UDP bool
	// UDPAddr is the datagram listen address (default: the TCP listener's
	// address and port, i.e. UDP 31090 next to TCP 31090).
	UDPAddr string
	// UDPHost / UDPPort are what the handshake reply tells the client to
	// send to. Default: the local address of the client's TCP connection
	// (the address the client reached us at) and the bound UDP port. Set
	// them behind NAT or port mapping, where the local address is not the
	// one clients can reach.
	UDPHost string
	UDPPort int
	// UDPAnySource accepts datagrams from any source address. By default a
	// datagram must come from the address of the TCP connection its key
	// belongs to (a 32-bit key alone is guessable); clients behind
	// carrier-grade NAT with several egress addresses may need this.
	UDPAnySource bool
	// UDPReaders is the number of goroutines reading the datagram socket
	// (default min(4, GOMAXPROCS)).
	UDPReaders int

	// ModeACAllClients sends Mode A/C requests (start_sending_modeac,
	// MLATC.md §10) to every client, not only to clients that announced the
	// AirView extension with "modeac": true in their handshake. Off by
	// default: the reference client knows no such message.
	ModeACAllClients bool
}

// TrustSource reports whether a station's timing is still unproven
// (quarantined). It must be cheap and safe for concurrent use.
type TrustSource interface {
	Quarantined(st model.StationIdx) bool
}

func (o Options) normalized() Options {
	if o.Addr == "" {
		o.Addr = ":" + strconv.Itoa(model.DefaultMLATCPort)
	}
	dur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	dur(&o.HandshakeTimeout, 30*time.Second)
	dur(&o.IdleTimeout, 150*time.Second)
	dur(&o.HeartbeatEvery, 30*time.Second)
	dur(&o.StatsEvery, 60*time.Second)
	dur(&o.FlushEvery, time.Second)
	dur(&o.WriteTimeout, 10*time.Second)
	dur(&o.ReconnectIn, 60*time.Second)
	if o.MaxOutQueue <= 0 {
		o.MaxOutQueue = 256 << 10
	}
	if o.BatchFrames <= 0 {
		o.BatchFrames = 64
	}
	if o.ProbeAircraft == 0 {
		o.ProbeAircraft = 16
	}
	if o.UDPReaders <= 0 {
		o.UDPReaders = min(4, runtime.GOMAXPROCS(0))
	}
	if o.UDPPort < 0 || o.UDPPort > 65535 {
		o.UDPPort = 0
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o
}

// OptionsFromConfig returns the options of the aggregator configuration:
// listen port, limits, handshake timeout, the result switch and the
// aggregator.mlatc tunables.
func OptionsFromConfig(a model.Aggregator) Options {
	o := Options{Policy: PolicyFromConfig(a)}
	if a.MLATPort > 0 {
		o.Addr = ":" + strconv.Itoa(a.MLATPort)
	}
	if a.Limits.HandshakeTimeoutSec > 0 {
		o.HandshakeTimeout = time.Duration(a.Limits.HandshakeTimeoutSec) * time.Second
	}
	// aggregator.mlatc (zero = default).
	c := a.MLATC
	sec := func(v int) time.Duration { return time.Duration(max(v, 0)) * time.Second }
	o.IdleTimeout, o.HeartbeatEvery, o.StatsEvery = sec(c.IdleTimeoutSec), sec(c.HeartbeatSec), sec(c.StatsSec)
	o.MaxOutQueue = max(c.MaxOutQueueKB, 0) << 10
	// aggregator.mlatc.splitSync: "auto" (default), "on" or "off".
	if m, ok := ParseSplitSync(string(c.SplitSync)); ok {
		o.SplitSync = m
	}
	// aggregator.mlat.udp: the datagram transport on the same port number.
	o.UDP = a.MLAT.UDP
	o.UDPHost, o.UDPPort, o.UDPAddr = c.UDPHost, c.UDPPort, c.UDPListen
	o.UDPAnySource, o.UDPReaders = c.UDPAnySource, c.UDPReaders
	o.ModeACAllClients = c.ModeACAllClients
	return o
}

// SplitSyncMode is the split_sync policy (Options.SplitSync).
//
// Measured on simnet through the real ingest and MLAT engine (MLATC.md §8,
// the H-mlatc section of docs/aggregator/PROGRESS.md): with pairs, the
// first half of every pair reaches the server up to the pairing window
// late, so a hold-out aircraft's DF17 copies from mlat-clients miss their
// MLAT groups; with split sync they arrive like AVF/1 copies. A client
// that ignores the flag keeps sending pairs, which the server still
// accepts, so offering it never loses data from such clients.
type SplitSyncMode uint8

const (
	// SplitSyncAuto (the default) offers split_sync. When a client that
	// was offered it sends single-sync messages the server cannot use (at
	// least splitBadMin malformed "ssync" and no valid one), or sends no
	// sync data at all while unknown messages arrive (splitSilentAfter),
	// its identity is remembered for splitFallbackFor, the client is
	// disconnected once, and its next connection is answered without
	// split_sync.
	SplitSyncAuto SplitSyncMode = iota
	// SplitSyncOn always offers split_sync.
	SplitSyncOn
	// SplitSyncOff never offers it (the reference protocol's default).
	SplitSyncOff
)

// String returns "auto", "on" or "off".
func (m SplitSyncMode) String() string {
	switch m {
	case SplitSyncOn:
		return "on"
	case SplitSyncOff:
		return "off"
	}
	return "auto"
}

// ParseSplitSync maps "auto" / "" , "on" / "true" and "off" / "false" to
// a mode (ok false for anything else).
func ParseSplitSync(s string) (SplitSyncMode, bool) {
	switch s {
	case "", "auto":
		return SplitSyncAuto, true
	case "on", "true":
		return SplitSyncOn, true
	case "off", "false":
		return SplitSyncOff, true
	}
	return SplitSyncAuto, false
}
