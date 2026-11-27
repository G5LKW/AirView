package uplink

import (
	"context"
	"net"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
)

// Source describes the local inputs to the uplink. internal/engine passes an
// adapter around feed.Manager and the station configuration; airview-feeder
// passes its own receivers.
type Source interface {
	// Forwardable reports whether frames of the local feed may be sent
	// upstream at all (feed.Manager.Forwardable: configured, not MLATOnly,
	// not NoForward, not demo, not hub).
	Forwardable(feedID string) bool
	// Receiver describes the local feed as an AVF receiver. It is called
	// when a feed is first seen and then about every 60 s; it must be cheap
	// and must not block.
	Receiver(feedID string) ReceiverInfo
}

// ReceiverInfo is what the uplink announces about one local receiver.
type ReceiverInfo struct {
	// RxNo is the AVF receiver number (1..255). 0 = assigned by the uplink
	// and remembered in its state directory.
	RxNo     int
	Name     string // display name ([A-Za-z0-9 _.-], at most 32 characters)
	Decoder  string // receiver / decoder model and version
	Channels int    // RF channels (default 1)
	// Location is the receiver position; sent only with Uplink.SendLocation.
	Location *avf.Location
	Antennas []avf.Antenna
	// Input status (INPUT_STATUS).
	InputType string // "beast", "avr", "sbs", ...
	Connected bool
	FPS       float64
	CRCErrors uint64
	SinceMs   int64
	// AliasGNSS is set when the receiver reports an alias (fake) GNSS
	// position: GNSS_POS is then never sent.
	AliasGNSS bool
}

// SourceFuncs adapts two functions to Source. A nil function allows every
// feed / returns an empty description.
type SourceFuncs struct {
	ForwardableFn func(feedID string) bool
	ReceiverFn    func(feedID string) ReceiverInfo
}

// Forwardable implements Source.
func (s SourceFuncs) Forwardable(id string) bool {
	if s.ForwardableFn == nil {
		return true
	}
	return s.ForwardableFn(id)
}

// Receiver implements Source.
func (s SourceFuncs) Receiver(id string) ReceiverInfo {
	if s.ReceiverFn == nil {
		return ReceiverInfo{}
	}
	return s.ReceiverFn(id)
}

// Options configure a Manager (and every Client it runs).
type Options struct {
	// Software and Version go into HELLO_C ("airview" / "airview-feeder").
	Software, Version string
	// StateDir holds identity.key, uuid and per-uplink state and spool
	// directories (<StateDir>/uplinks/<id>/). "" keeps everything in memory:
	// a fresh identity per process and no disk spool.
	StateDir string
	// Identity overrides the identity loaded from StateDir.
	Identity *Identity

	// Source describes and filters the local inputs. nil = every feed is
	// forwardable (frame-level MLAT rules still apply).
	Source Source
	// Inject receives MLAT results from the aggregators as DecodedUpdates
	// (local tracker). They carry FeedID "uplink/<id>" and are never
	// forwarded upstream. nil = not injected.
	Inject model.FrameSink
	// OnResults, if set, receives every MLAT result batch (for example to
	// republish them as Beast on 30105 in airview-feeder).
	OnResults func(uplinkID string, fixes []model.MLATFix)

	// Position returns the receiver position for MLAT outputs (WGS84
	// degrees, metres; ok=false when unknown). nil = unknown.
	Position func() (lat, lon, altM float64, ok bool)
	// Logf receives log lines. Secrets (tokens, claim codes) are never
	// logged.
	Logf func(format string, args ...any)
	// Now is the host UTC clock (default time.Now; airview passes ntp.Now).
	Now func() time.Time
	// HostErrNs estimates the host clock error for CLOCK_PAIR (default 2 ms).
	HostErrNs func() uint64
	// Dial opens the TCP connection (default net.Dialer).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// SDRoot reports whether the root filesystem is an SD card (spool
	// default 0). nil = detected from /proc/mounts.
	SDRoot func() bool

	// Tunables; zero values select the documented defaults.
	RingAge        time.Duration // RAM ring span (10 min)
	RingBytes      int           // RAM ring size (12 MiB)
	BackfillBytes  int           // in-memory backfill queue (2 x RingBytes)
	ResumeWindow   time.Duration // unacknowledged data younger than this is resent live (30 s)
	BackfillMinBps int           // backfill floor in uncompressed bytes/s (64 KiB/s)
	BackoffMin     time.Duration // 1 s
	BackoffMax     time.Duration // 60 s
	Liveness       time.Duration // reconnect after this long without PING/ACK (30 s)
	HandshakeTO    time.Duration // 10 s
	SegmentBytes   int64         // spool segment size (4 MiB)
}

// Defaults of Options.
const (
	DefaultRingAge        = 10 * time.Minute
	DefaultRingBytes      = 12 << 20
	DefaultResumeWindow   = 30 * time.Second
	DefaultBackfillMinBps = 64 << 10
	DefaultBackoffMin     = time.Second
	DefaultBackoffMax     = 60 * time.Second
	DefaultLiveness       = 30 * time.Second
	DefaultHandshakeTO    = 10 * time.Second
	DefaultSegmentBytes   = 4 << 20
	DefaultBatchMs        = 50
)

func (o *Options) normalize() {
	if o.Software == "" {
		o.Software = "airview"
	}
	if o.Version == "" {
		o.Version = model.Version
	}
	if o.Source == nil {
		o.Source = SourceFuncs{}
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HostErrNs == nil {
		o.HostErrNs = func() uint64 { return 2e6 }
	}
	if o.Dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		o.Dial = d.DialContext
	}
	if o.SDRoot == nil {
		o.SDRoot = RootOnSD
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.RingAge, DefaultRingAge)
	def(&o.ResumeWindow, DefaultResumeWindow)
	def(&o.BackoffMin, DefaultBackoffMin)
	def(&o.BackoffMax, DefaultBackoffMax)
	def(&o.Liveness, DefaultLiveness)
	def(&o.HandshakeTO, DefaultHandshakeTO)
	if o.RingBytes <= 0 {
		o.RingBytes = DefaultRingBytes
	}
	if o.BackfillBytes <= 0 {
		o.BackfillBytes = 2 * o.RingBytes
	}
	if o.BackfillMinBps <= 0 {
		o.BackfillMinBps = DefaultBackfillMinBps
	}
	if o.SegmentBytes <= 0 {
		o.SegmentBytes = DefaultSegmentBytes
	}
}
