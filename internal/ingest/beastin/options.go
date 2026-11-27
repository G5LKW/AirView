package beastin

import (
	"log/slog"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"time"

	"airview/internal/model"
)

// Policy holds the limits that can change at run time (Server.SetPolicy),
// e.g. when the operator edits the aggregator config. Zero fields take the
// defaults of DefaultPolicy; a negative value disables that limit.
type Policy struct {
	// MaxFPS is the frame-rate limit per station (DESIGN §4.6: 6000).
	MaxFPS int
	// MaxBytesPerSec is the byte-rate limit per link and station on it
	// (200 kB/s; Beast is uncompressed, so this is the raw stream rate).
	MaxBytesPerSec int
	// OverLimitSeconds consecutive over-limit seconds disconnect the link
	// (default 5).
	OverLimitSeconds int
	// CoolDown is how long an identity disconnected for its rate is refused
	// (default 5 minutes).
	CoolDown time.Duration
	// RefusedCoolDown is how long an identity the registry refused is
	// refused without asking again (default 1 minute).
	RefusedCoolDown time.Duration

	// MaxConnsPerIP / MaxUnattachedPerIP / MaxConnsPerSubnet bound
	// concurrent links per address, per address while not yet attached to a
	// station (the "unauthenticated" limit of DESIGN §4.6) and per IPv4 /24
	// or IPv6 /48 (defaults 16, 4, 32).
	MaxConnsPerIP      int
	MaxUnattachedPerIP int
	MaxConnsPerSubnet  int
	// PerIPStations bounds the distinct identities (stations) behind one
	// address; trusted-relay sub-stations are not counted (default 8).
	PerIPStations int
	// AllowCIDRs are exempt from the per-IP and per-subnet limits.
	AllowCIDRs []netip.Prefix
	// TrustedRelays are the relays whose 0xE3 receiver IDs are honoured:
	// "uuid@cidr" (the relay must connect from inside cidr), or a plain
	// "uuid", honoured only from AllowCIDRs. A self-declared 0xE4 UUID is
	// no secret (it travels in clear text), so it never grants the relay
	// privilege on its own (SEC-03).
	TrustedRelays []string
}

// DefaultPolicy returns the DESIGN §4.6 limits.
func DefaultPolicy() Policy {
	return Policy{
		MaxFPS: 6000, MaxBytesPerSec: 200_000, OverLimitSeconds: 5,
		CoolDown: 5 * time.Minute, RefusedCoolDown: time.Minute,
		MaxConnsPerIP: 16, MaxUnattachedPerIP: 4, MaxConnsPerSubnet: 32,
		PerIPStations: 8,
	}
}

// PolicyFromConfig derives the policy from the aggregator config (limits
// block and trustedRelays). Invalid CIDRs and UUIDs are skipped; the config
// layer has already normalised them.
func PolicyFromConfig(a model.Aggregator) Policy {
	p := DefaultPolicy()
	l := a.Limits
	setPos(&p.MaxFPS, l.MaxFPSPerStation)
	setPos(&p.MaxBytesPerSec, l.MaxKBpsPerStation*1000)
	setPos(&p.MaxConnsPerIP, l.MaxConnsPerIP)
	setPos(&p.MaxUnattachedPerIP, l.MaxUnauthPerIP)
	setPos(&p.MaxConnsPerSubnet, l.MaxConnsPerSubnet)
	setPos(&p.PerIPStations, l.PerIPStations)
	setPos(&p.OverLimitSeconds, a.Beast.OverLimitSeconds)
	switch b := a.Beast; {
	case b.CoolDownSec > 0:
		p.CoolDown = time.Duration(b.CoolDownSec) * time.Second
	case b.CoolDownSec == 0 && b != (model.AggBeast{}):
		p.CoolDown = -1 // configured 0 = no cool-down
	}
	for _, c := range l.AllowCIDRs {
		if pfx, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil {
			p.AllowCIDRs = append(p.AllowCIDRs, pfx.Masked())
		} else if ip, err := netip.ParseAddr(strings.TrimSpace(c)); err == nil {
			ip = ip.Unmap()
			p.AllowCIDRs = append(p.AllowCIDRs, netip.PrefixFrom(ip, ip.BitLen()))
		}
	}
	for _, u := range a.TrustedRelays {
		if n, src, ok := parseRelay(u); ok {
			if src.IsValid() {
				n += "@" + src.String()
			}
			p.TrustedRelays = append(p.TrustedRelays, n)
		}
	}
	return p
}

// parseRelay splits a TrustedRelays entry into the normalised UUID and the
// source prefix (invalid prefix = bound to AllowCIDRs).
func parseRelay(s string) (uuid string, src netip.Prefix, ok bool) {
	u, cidr, hasCIDR := strings.Cut(strings.TrimSpace(s), "@")
	if uuid, ok = normalizeUUID(u); !ok {
		return "", netip.Prefix{}, false
	}
	if !hasCIDR {
		return uuid, netip.Prefix{}, true
	}
	cidr = strings.TrimSpace(cidr)
	if pfx, err := netip.ParsePrefix(cidr); err == nil {
		return uuid, pfx.Masked(), true
	}
	if ip, err := netip.ParseAddr(cidr); err == nil {
		ip = ip.Unmap()
		return uuid, netip.PrefixFrom(ip, ip.BitLen()), true
	}
	return "", netip.Prefix{}, false
}

func setPos(dst *int, v int) {
	if v > 0 {
		*dst = v
	}
}

// normalized fills defaults.
func (p Policy) normalized() Policy {
	d := DefaultPolicy()
	def := func(v *int, dv int) {
		if *v == 0 {
			*v = dv
		}
	}
	def(&p.MaxFPS, d.MaxFPS)
	def(&p.MaxBytesPerSec, d.MaxBytesPerSec)
	def(&p.OverLimitSeconds, d.OverLimitSeconds)
	def(&p.MaxConnsPerIP, d.MaxConnsPerIP)
	def(&p.MaxUnattachedPerIP, d.MaxUnattachedPerIP)
	def(&p.MaxConnsPerSubnet, d.MaxConnsPerSubnet)
	def(&p.PerIPStations, d.PerIPStations)
	if p.CoolDown == 0 {
		p.CoolDown = d.CoolDown
	}
	if p.RefusedCoolDown == 0 {
		p.RefusedCoolDown = d.RefusedCoolDown
	}
	return p
}

// policy is the compiled, immutable form used by the links.
type policy struct {
	Policy
	// trusted maps a relay UUID to the source prefixes it may connect
	// from (an invalid prefix stands for AllowCIDRs).
	trusted map[string][]netip.Prefix
}

func compilePolicy(p Policy) *policy {
	p = p.normalized()
	c := &policy{Policy: p, trusted: map[string][]netip.Prefix{}}
	for _, e := range p.TrustedRelays {
		if u, src, ok := parseRelay(e); ok {
			c.trusted[u] = append(c.trusted[u], src)
		}
	}
	return c
}

// relayTrusted reports whether a link with this UUID from ip may act as a
// trusted relay: the UUID is listed and ip is inside the bound prefix (or
// AllowCIDRs for an entry without one).
func (p *policy) relayTrusted(uuid string, ip netip.Addr) bool {
	if uuid == "" || !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	for _, src := range p.trusted[uuid] {
		if src.IsValid() {
			if src.Contains(ip) {
				return true
			}
			continue
		}
		for _, a := range p.AllowCIDRs {
			if a.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func (p *policy) allowed(ip netip.Addr) bool {
	for _, pfx := range p.AllowCIDRs {
		if pfx.Contains(ip) {
			return true
		}
	}
	return false
}

// Options configures a Server. Zero values take the documented defaults.
type Options struct {
	// Addr is the listen address for ListenAndServe (default ":30004").
	Addr string
	// Policy holds the limits (see Policy; changeable with SetPolicy).
	Policy Policy

	// IdentifyWait is how long a link that sends Beast data may take to send
	// its UUID before it is attached as unidentified (default 2 s).
	IdentifyWait time.Duration
	// IdentifyMaxFrames frames are held at most while waiting for the UUID;
	// more attach the link as unidentified at once (default 512).
	IdentifyMaxFrames int
	// HandshakeTimeout closes links that send nothing at all (default 10 s).
	HandshakeTimeout time.Duration
	// IdleTimeout closes links silent for this long (default 3 minutes;
	// readsb sends keep-alive frames well within that).
	IdleTimeout time.Duration
	// PingInterval is the period of our own pings after 'W' 'O' (default 5 s).
	PingInterval time.Duration
	// SlowDownEvery is the minimum interval between two 'W' 'S' requests
	// on one link (default 10 s).
	SlowDownEvery time.Duration
	// WriteTimeout bounds writes to the feeder (pings, echoes; default 5 s).
	WriteTimeout time.Duration

	// MaxLinks caps concurrent links (default 4096).
	MaxLinks int
	// Workers is the size of the delivery pool (default GOMAXPROCS).
	Workers int
	// QueueBatches is the per-link delivery queue length in batches
	// (default 64). Above half of it 'W' 'S' is sent; when full, batches
	// are dropped and counted.
	QueueBatches int
	// BatchFrames is the maximum number of frames per batch (default 256).
	BatchFrames int
	// ReadBuffer is the socket read size (default 16 KiB).
	ReadBuffer int

	// ServerKey keys the HMAC of unidentified IP keys. Pass the registry's
	// persistent key so keys survive restarts; nil = a random key per
	// process.
	ServerKey []byte
	// Now is the clock used for receive times (default time.Now; the engine
	// passes ntp.Now).
	Now func() time.Time
	// Logger receives link events (no raw IP addresses are logged).
	Logger *slog.Logger
	// Gate, when set, must return true for new links to be accepted (DESIGN
	// §7: feeder listeners stay closed until the admin account is ready).
	Gate func() bool
	// Budget is the optional global ingest budget.
	Budget Budget

	// AcceptMLATResults passes frames carrying the MLAT magic timestamp
	// (default false: they are dropped and counted).
	AcceptMLATResults bool
	// StripClock clears ClockCls, Epoch and Ticks of every delivered frame
	// (default false: the ticks are kept for latency statistics, and
	// Batch.MapStatsOnly keeps them out of MLAT).
	StripClock bool
}

func (o Options) normalized() Options {
	if o.Addr == "" {
		o.Addr = ":30004"
	}
	dur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	num := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	dur(&o.IdentifyWait, 2*time.Second)
	dur(&o.HandshakeTimeout, 10*time.Second)
	dur(&o.IdleTimeout, 3*time.Minute)
	dur(&o.PingInterval, 5*time.Second)
	dur(&o.SlowDownEvery, 10*time.Second)
	dur(&o.WriteTimeout, 5*time.Second)
	num(&o.IdentifyMaxFrames, 512)
	num(&o.MaxLinks, 4096)
	num(&o.Workers, runtime.GOMAXPROCS(0))
	num(&o.QueueBatches, 64)
	num(&o.BatchFrames, 256)
	num(&o.ReadBuffer, 16<<10)
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o
}

// OptionsFromConfig returns the options of the aggregator config: listen
// port, limits, handshake timeout, trusted relays and the aggregator.beast
// tunables (zero fields keep the package defaults).
func OptionsFromConfig(a model.Aggregator) Options {
	o := Options{Policy: PolicyFromConfig(a)}
	if a.BeastPort > 0 {
		o.Addr = ":" + strconv.Itoa(a.BeastPort)
	}
	if a.Limits.HandshakeTimeoutSec > 0 {
		o.HandshakeTimeout = time.Duration(a.Limits.HandshakeTimeoutSec) * time.Second
	}
	b := a.Beast
	o.IdentifyWait = time.Duration(b.IdentifyWaitMs) * time.Millisecond
	o.IdleTimeout = time.Duration(b.IdleTimeoutSec) * time.Second
	o.PingInterval = time.Duration(b.PingIntervalSec) * time.Second
	o.SlowDownEvery = time.Duration(b.SlowDownEverySec) * time.Second
	o.QueueBatches = b.QueueBatches
	o.MaxLinks = b.MaxLinks
	o.AcceptMLATResults = b.AcceptMlatResults
	o.StripClock = b.StripClock
	return o
}
