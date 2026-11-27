package avfsrv

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
	"airview/internal/registry"
)

// Registry is what the server needs from internal/registry; *registry.Registry
// implements it.
type Registry interface {
	// Feeder returns a native feeder by ID.
	Feeder(ctx context.Context, feederID string) (registry.Feeder, bool, error)
	// CheckToken reports whether an enrol token is valid (not consumed).
	CheckToken(ctx context.Context, secret string) (bool, error)
	// Admit admits one receiver as a station (consumes the token once).
	Admit(ctx context.Context, req registry.AdmitRequest) (registry.AdmitResult, error)
	// MintClaim mints a claim code for a station.
	MintClaim(ctx context.Context, idx model.StationIdx) (string, time.Time, error)
	// ConnOpen / ConnClose write the connection log (hashed IP).
	ConnOpen(ctx context.Context, idx model.StationIdx, ip netip.Addr, proto string) (int64, error)
	ConnClose(ctx context.Context, id int64, bytes int64) error
	// Seen records liveness (memory only).
	Seen(idx model.StationIdx, t time.Time)
}

var _ Registry = (*registry.Registry)(nil)

// Hooks are optional callbacks into the engine. They are called from the
// connection goroutines and must not block for long.
type Hooks struct {
	// OnReceiver is called for every RECEIVER record of an admitted station
	// (name, declared location, privacy, antennas, decoder).
	OnReceiver func(st model.StationIdx, r avf.Receiver)
	// OnGNSS receives GNSS_POS records (only with the gnss feature).
	OnGNSS func(st model.StationIdx, p avf.GNSSPos)
	// OnMinuteStats receives MINUTE_STATS records; late is set inside
	// BACKFILL.
	OnMinuteStats func(st model.StationIdx, m avf.MinuteStats, late bool)
	// OnStationMinute receives the selective-mode merge of a MINUTE_STATS
	// record with the frames the server received for that minute
	// (minutes.go). Several calls for one (Station, Minute) replace each
	// other (upsert): the last one carries the sum of all parts. When set,
	// the server counts the received frames per station and minute.
	OnStationMinute func(m StationMinute)
	// OnInputStatus receives INPUT_STATUS records.
	OnInputStatus func(st model.StationIdx, s avf.InputStatus)
	// OnOnline is called when a station's first connection of a session is
	// attached (online true) and when the session ends (online false).
	OnOnline func(st model.StationIdx, online bool)
}

// Policy holds the settings that may change at runtime (SetPolicy).
type Policy struct {
	// Admission is model.AdmissionOpen, AdmissionApprove or AdmissionInvite.
	Admission string
	// Limits: MaxUnauthPerIP, MaxConnsPerIP, MaxConnsPerSubnet,
	// PerIPStations, HandshakeTimeoutSec and AllowCIDRs are applied here;
	// the rate limits are applied by the ingest sessions.
	Limits model.AggLimits
	// MLAT says whether the network uses the feeders' timestamps (WELCOME
	// config.mlat); ReturnResults whether MLAT_RESULTS are sent.
	MLAT, ReturnResults bool
	// MOTD is shown to feeder owners (WELCOME motd).
	MOTD string
	// Name is the network display name (HELLO_S name).
	Name string
}

// PolicyFromConfig maps the aggregator configuration.
func PolicyFromConfig(a model.Aggregator) Policy {
	return Policy{Admission: a.Admission, Limits: a.Limits, MLAT: a.MLAT.Enabled,
		ReturnResults: a.MLAT.Enabled && a.MLAT.ReturnResults, Name: a.Name}
}

// Options configures a Server.
type Options struct {
	// Addr is the listen address (":30160").
	Addr string
	// TLSConfig is the server TLS configuration (avf.ServerTLSConfig; see
	// TLSFromConfig). Required.
	TLSConfig *tls.Config
	// ServerID is the network's stable ID (16 base32 characters; see
	// LoadOrCreateServerID). Required.
	ServerID string
	// Policy is the initial policy.
	Policy Policy
	// Resolver is the registry's resolver: admission changes of connected
	// stations (approved, revoked) are noticed through it. nil = none.
	Resolver model.StationResolver
	// Gate keeps the listener closed (connections are dropped silently)
	// while it returns false: DESIGN §7, feeders wait until the admin
	// account exists. nil = open.
	Gate func() bool
	// CloneGuard is the clone rule state (default a new one).
	CloneGuard *registry.CloneGuard
	// Watermarks keeps seq watermarks across reconnects (default a new one).
	Watermarks *avf.WatermarkStore
	// QueueBatches is the per-connection queue between the record reader
	// and the ingest (default 64 FRAMES records).
	QueueBatches int
	// Guard limits decompression per connection (default avf.DefaultGuard).
	Guard *avf.Guard
	// MaxSessions caps concurrent connections (default 10000).
	MaxSessions int
	// ResumeGrace keeps a station online after a connection ends without
	// GOODBYE (default 30 s, DESIGN §4.5).
	ResumeGrace time.Duration
	// Hooks are optional engine callbacks.
	Hooks Hooks
	// Now is the clock (default time.Now; use ntp.Now in production).
	Now func() time.Time
	// Logger (default slog.Default()). The server never logs raw IPs or
	// secrets; feeder IDs are logged.
	Logger *slog.Logger
	// Listen overrides net.Listen (tests).
	Listen func(network, addr string) (net.Listener, error)
	// HostLatencySlack bounds how much earlier than physically plausible a
	// feeder's host times may place its frames (finding ROB-1, hostguard.go):
	// when even the fastest live frame of the last 20-30 s claims to be
	// older than RTT/2 + HostLatencySlack, the host-clock offset handed to
	// the ingest is corrected by the excess (default 300 ms; < 0 = off).
	HostLatencySlack time.Duration
}

// OptionsFromConfig maps the aggregator configuration (the caller sets
// TLSConfig, ServerID, Resolver, Gate, Hooks, Now and Logger).
func OptionsFromConfig(a model.Aggregator) Options {
	return Options{
		Addr: ":" + strconv.Itoa(a.AVFPort), Policy: PolicyFromConfig(a),
		QueueBatches: a.Ingest.AVFQueueRecords,
		MaxSessions:  a.Ingest.AVFMaxSessions,
		ResumeGrace:  time.Duration(a.Ingest.AVFResumeGraceSec) * time.Second,
		// 0 = default (300 ms), negative = off.
		HostLatencySlack: time.Duration(a.Ingest.AVFHostSlackMs) * time.Millisecond,
	}
}

func (o Options) withDefaults() (Options, error) {
	if o.HostLatencySlack == 0 {
		o.HostLatencySlack = 300 * time.Millisecond
	}
	if o.TLSConfig == nil {
		return o, errors.New("avfsrv: Options.TLSConfig is required")
	}
	if !model.ValidID(o.ServerID) {
		return o, fmt.Errorf("avfsrv: invalid server ID %q", o.ServerID)
	}
	if o.Addr == "" {
		o.Addr = ":" + strconv.Itoa(model.DefaultAVFPort)
	}
	if o.CloneGuard == nil {
		o.CloneGuard = registry.NewCloneGuard()
	}
	if o.Watermarks == nil {
		o.Watermarks = &avf.WatermarkStore{}
	}
	if o.QueueBatches <= 0 {
		o.QueueBatches = 64
	}
	if o.MaxSessions <= 0 {
		o.MaxSessions = 10000
	}
	if o.ResumeGrace <= 0 {
		o.ResumeGrace = 30 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Listen == nil {
		o.Listen = net.Listen
	}
	o.Policy = o.Policy.normalized()
	return o, nil
}

func (p Policy) normalized() Policy {
	d := model.DefaultAggregator()
	if p.Admission == "" {
		p.Admission = d.Admission
	}
	l, dl := &p.Limits, d.Limits
	if l.MaxUnauthPerIP <= 0 {
		l.MaxUnauthPerIP = dl.MaxUnauthPerIP
	}
	if l.MaxConnsPerIP <= 0 {
		l.MaxConnsPerIP = dl.MaxConnsPerIP
	}
	if l.MaxConnsPerSubnet <= 0 {
		l.MaxConnsPerSubnet = dl.MaxConnsPerSubnet
	}
	if l.PerIPStations <= 0 {
		l.PerIPStations = dl.PerIPStations
	}
	if l.HandshakeTimeoutSec <= 0 {
		l.HandshakeTimeoutSec = dl.HandshakeTimeoutSec
	}
	return p
}

// ------------------------------------------------------------ helpers

// TLSFromConfig builds the server TLS configuration of DESIGN §4.1: the
// operator certificate (Aggregator.TLSCert/TLSKey, reloaded when the files
// change) or, by default, a self-signed ECDSA P-256 certificate in
// <dataDir>/avf-tls/. It returns the SPKI pin to show in the admin UI and
// the install one-liner.
func TLSFromConfig(dataDir string, a model.Aggregator) (*tls.Config, string, error) {
	if a.TLSCert != "" || a.TLSKey != "" {
		r, err := avf.NewCertReloader(a.TLSCert, a.TLSKey)
		if err != nil {
			return nil, "", err
		}
		c, err := r.Current()
		if err != nil {
			return nil, "", err
		}
		pin, err := avf.TLSCertPin(c)
		if err != nil {
			return nil, "", err
		}
		return avf.ServerTLSConfig(r.GetCertificate), pin, nil
	}
	var hosts []string
	if a.PublicHost != "" {
		hosts = append(hosts, a.PublicHost)
	}
	cert, pin, err := avf.LoadOrCreateSelfSigned(filepath.Join(dataDir, "avf-tls"), hosts)
	if err != nil {
		return nil, "", err
	}
	return avf.ServerTLSConfig(avf.StaticCertificate(cert)), pin, nil
}

// ServerIDFile is the file LoadOrCreateServerID keeps the network ID in,
// relative to the data directory.
const ServerIDFile = "avf-tls/server-id"

// LoadOrCreateServerID returns the network's stable AVF serverId, creating
// a random one in <dataDir>/avf-tls/server-id (mode 0600) on first use.
// Feeders remember it, so it must never change for a network.
func LoadOrCreateServerID(dataDir string) (string, error) {
	p := filepath.Join(dataDir, filepath.FromSlash(ServerIDFile))
	if b, err := os.ReadFile(p); err == nil {
		id, ok := model.CanonicalID(string(b))
		if !ok {
			return "", fmt.Errorf("avfsrv: %s does not hold a valid server ID", p)
		}
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id, err := model.NewSID(rand.Reader)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	return id, nil
}
