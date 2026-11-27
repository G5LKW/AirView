package model

import (
	"fmt"
	"slices"
	"strings"
)

// Settings added after Wave-A (phase 4-5 libraries): RTL-SDR input, the
// network bundle adjustment, Mode A/C,
// the mlat-client UDP transport and federation (DESIGN §20.3: every
// tunable is file-configurable, has an AIRVIEW_* variable and is checked
// by `airview config check`).

// FeedRTLTCP is an RTL-SDR dongle read through the user's own rtl_tcp
// server (internal/sdr: pure-Go demodulator). Host/Port is the rtl_tcp
// address (default 127.0.0.1:1234; rtl_tcp has no authentication, so keep
// it on loopback). Feed.SDR holds the tuner settings.
const FeedRTLTCP = "rtltcp"

// DefaultRTLTCPPort is the conventional rtl_tcp port.
const DefaultRTLTCPPort = 1234

// FeedSDR tunes an rtltcp feed (Feed.SDR). Zero = default.
type FeedSDR struct {
	// Gain is "max" (default: the highest manual gain step), "auto" (the
	// tuner's AGC, not recommended for Mode S) or a manual gain in dB such
	// as "40.2".
	Gain string `json:"gain,omitempty"`
	// PPM is the oscillator frequency correction (-500..500).
	PPM int `json:"ppm,omitempty"`
	// BiasTee switches on the antenna supply of dongles that have one.
	BiasTee bool `json:"biasTee,omitempty"`
	// SampleRate is the complex sample rate in samples per second (default
	// 2400000, 2000000..3200000).
	SampleRate int `json:"sampleRate,omitempty"`
	// Spawn starts the user's own rtl_tcp as a child process (found on PATH
	// or next to the program, or at RTLTCPPath). AirView never ships or
	// downloads rtl_tcp.
	Spawn bool `json:"spawn,omitempty"`
	// RTLTCPPath is the rtl_tcp executable for Spawn ("" = search).
	RTLTCPPath string `json:"rtltcpPath,omitempty"`
	// Device is the dongle index passed to a spawned rtl_tcp (0..31).
	Device int `json:"device,omitempty"`
	// AcceptUnverifiedAP passes address/parity replies whose address was not
	// confirmed by a recent all-call or ADS-B message (default false).
	AcceptUnverifiedAP bool `json:"acceptUnverifiedAp,omitempty"`
}

// Normalize corrects out-of-range values and returns notes (path is the
// feed's prefix, e.g. "feeds[0].sdr").
func (s *FeedSDR) Normalize(path string) []string {
	var n notes
	s.Gain = strings.ToLower(strings.TrimSpace(s.Gain))
	switch s.Gain {
	case "", "max", "auto":
	default:
		var db float64
		if _, err := fmt.Sscanf(s.Gain, "%g", &db); err != nil || db < 0 || db > 60 {
			n.addf("%s.gain %q is not max, auto or a gain in dB (0..60); max is used", path, s.Gain)
			s.Gain = ""
		}
	}
	if s.PPM < -500 || s.PPM > 500 {
		n.addf("%s.ppm %d is out of range -500..500; 0 is used", path, s.PPM)
		s.PPM = 0
	}
	if s.SampleRate != 0 && (s.SampleRate < 2_000_000 || s.SampleRate > 3_200_000) {
		n.addf("%s.sampleRate %d is out of range 2000000..3200000; the default is used", path, s.SampleRate)
		s.SampleRate = 0
	}
	if s.Device < 0 || s.Device > 31 {
		n.addf("%s.device %d is out of range 0..31; 0 is used", path, s.Device)
		s.Device = 0
	}
	s.RTLTCPPath = strings.TrimSpace(s.RTLTCPPath)
	return n
}

// AggMLATBundle tunes the network bundle adjustment
// (aggregator.mlat.survey.bundle; internal/mlat/survey Bundler): station
// positions and clocks of up to maxStations stations refined together,
// wrong declarations released and flagged. Zero = default.
type AggMLATBundle struct {
	// Disabled turns the bundle adjustment off (default false).
	Disabled bool `json:"disabled"`
	// EveryMin is the adjustment period (default 60, 1..1440 min).
	EveryMin int `json:"everyMin"`
	// SpanMin is the data span of one adjustment (default 60, 5..360 min).
	SpanMin int `json:"spanMin"`
	// MaxStations bounds one adjustment (default 64, 2..512).
	MaxStations int `json:"maxStations"`
	// WindowS is the clock polynomial window (default 10, 2..120 s).
	WindowS float64 `json:"windowS"`
	// ThinIntervalS keeps one transmission per aircraft per interval
	// (default 8, 1..60 s).
	ThinIntervalS int `json:"thinIntervalS"`
	// MaxFrames bounds the stored transmissions (default 200000,
	// 1000..5000000).
	MaxFrames int `json:"maxFrames"`
	// MinStationRecs: a station with fewer usable receptions is left out
	// (default 100, 10..1000000).
	MinStationRecs int `json:"minStationRecs"`
	// OutlierMinM: a prior position is contradicted beyond
	// max(outlierMinM, outlierK x sigma) (default 100, 1..100000 m).
	OutlierMinM float64 `json:"outlierMinM"`
	// OutlierK is that sigma factor (default 3, 1..100).
	OutlierK float64 `json:"outlierK"`
	// DeclaredSigmaM is the prior 1-sigma of a declared position that
	// passed the test (default 30, 1..10000 m).
	DeclaredSigmaM float64 `json:"declaredSigmaM"`
	// AnchorSigmaM is the horizontal prior 1-sigma of an anchor (GNSS or
	// confirmed position; default 5, 0.1..1000 m).
	AnchorSigmaM float64 `json:"anchorSigmaM"`
	// MaxReleases bounds the priors released in one adjustment (default 16,
	// 1..512).
	MaxReleases int `json:"maxReleases"`
	// Workers is the number of goroutines of one adjustment (default
	// min(GOMAXPROCS, 8), up to 256).
	Workers int `json:"workers"`
}

// AggFederation is aggregator.federation: links to other AirView networks
// over AVFED/1 and federation-safe outbound feeds (DESIGN §12.1 phase 5).
// It has exactly the JSON shape of internal/federation.Config (a test
// keeps the two in step). Zero = off / default.
type AggFederation struct {
	// Enabled switches federation on. Off, nothing listens or dials.
	Enabled bool `json:"enabled"`
	// Name is this network's display name sent to peers (optional).
	Name string `json:"name,omitempty"`
	// Listen is the address of the inbound federation listener ("" =
	// accept no inbound peers, e.g. ":30170").
	Listen string `json:"listen,omitempty"`
	// IdentityFile is the node's Ed25519 key file ("" = federation.key in
	// the data directory). The node ID shown to peer operators derives
	// from it.
	IdentityFile string `json:"identityFile,omitempty"`
	// TLSCert is an operator certificate for the listener ("" = a
	// self-signed certificate kept in <data>/fed-tls).
	TLSCert string `json:"tlsCert,omitempty"`
	// TLSKey is the key of TLSCert (a secret path).
	TLSKey string `json:"tlsKey,omitempty"`
	// IntervalMs is the aircraft-state export period (default 1000,
	// 100..60000).
	IntervalMs int `json:"intervalMs,omitempty"`
	// MLATIntervalMs is the MLAT-result flush period (default 250,
	// 50..10000).
	MLATIntervalMs int `json:"mlatIntervalMs,omitempty"`
	// FullRefreshS: every this many seconds a peer gets every aircraft,
	// not only the changed ones (default 30, 1..3600).
	FullRefreshS int `json:"fullRefreshS,omitempty"`
	// TTLS is how long a remote aircraft stays without an update (default
	// 60, 5..3600 s).
	TTLS int `json:"ttlS,omitempty"`
	// Transit allows forwarding data received from one peer to other peers
	// (each peer must also allow it in send.transit; default off).
	Transit bool `json:"transit,omitempty"`
	// MaxHops bounds the path length of forwarded data, origin included
	// (default 3, 1..8).
	MaxHops int `json:"maxHops,omitempty"`
	// MaxRemoteAircraft bounds the remote aircraft store (default 50000,
	// 100..1000000).
	MaxRemoteAircraft int `json:"maxRemoteAircraft,omitempty"`
	// MaxInflateBps bounds the decompressed inbound rate per peer in bytes
	// per second (default 16000000, 100000..1000000000).
	MaxInflateBps int64 `json:"maxInflateBps,omitempty"`
	// QueueRecords bounds the per-peer send queue (default 256, 8..100000).
	QueueRecords int `json:"queueRecords,omitempty"`
	// HideRemote keeps received aircraft off the map (default false: they
	// are drawn as an overlay).
	HideRemote bool `json:"hideRemote,omitempty"`
	// Inject hands received aircraft to the tracker (default false; keep it
	// off: remote data would enter statistics and records).
	Inject bool `json:"inject,omitempty"`
	// Peers are the federated networks.
	Peers []AggFedPeer `json:"peers,omitempty"`
	// Feeds are federation-safe outbound feeds to other networks.
	Feeds []AggFedFeed `json:"feeds,omitempty"`
}

// AggFedPeer is one federated AirView network (aggregator.federation.peers[]).
type AggFedPeer struct {
	// Name is the local label of the peer.
	Name string `json:"name"`
	// ID is the peer's node ID (shown by the peer's `airview federation
	// id`); mandatory.
	ID string `json:"id"`
	// Addr is "host:port" to dial ("" = only accept this peer).
	Addr string `json:"addr,omitempty"`
	// Pin pins the peer listener's TLS key ("sha256/<b64>") or requires a
	// WebPKI certificate ("webpki"); "" relies on the mutual identity check.
	Pin string `json:"pin,omitempty"`
	// Disabled keeps the entry but neither dials nor accepts it.
	Disabled bool `json:"disabled,omitempty"`
	// Send is what this network shares with the peer.
	Send AggFedPolicy `json:"send"`
	// Receive is what this network accepts from the peer.
	Receive AggFedReceive `json:"receive"`
}

// AggFedBox is a latitude/longitude rectangle in degrees (West > East
// crosses the antimeridian).
type AggFedBox struct {
	// South is the southern edge.
	South float64 `json:"south"`
	// West is the western edge.
	West float64 `json:"west"`
	// North is the northern edge.
	North float64 `json:"north"`
	// East is the eastern edge.
	East float64 `json:"east"`
}

// AggFedPolicy says what is shared with one peer or out-feed. Feeder
// identities, station positions, receiver lists, signal levels and IP
// addresses are never shared.
type AggFedPolicy struct {
	// Aircraft is "all" (default), "nomlat" (MLAT positions removed) or
	// "none".
	Aircraft string `json:"aircraft,omitempty"`
	// MLAT shares MLAT result records (peers only; default off).
	MLAT bool `json:"mlat,omitempty"`
	// Transit forwards data of other peers to this peer (needs
	// federation.transit too).
	Transit bool `json:"transit,omitempty"`
	// Island is what happens to island-guard aircraft: "coarsen" (default)
	// or "drop".
	Island string `json:"island,omitempty"`
	// MinAltFt drops aircraft below this altitude and on the ground (0 = no
	// floor).
	MinAltFt int `json:"minAltFt,omitempty"`
	// NoGround drops aircraft on the ground.
	NoGround bool `json:"noGround,omitempty"`
	// NoMilitary drops aircraft the reference data marks military.
	NoMilitary bool `json:"noMilitary,omitempty"`
	// Boxes limits sharing to positions inside any box (empty = anywhere).
	Boxes []AggFedBox `json:"boxes,omitempty"`
	// ExcludeBoxes drops positions inside any of these boxes.
	ExcludeBoxes []AggFedBox `json:"excludeBoxes,omitempty"`
	// DelayS delays everything sent to this peer (0..600 s; peers only).
	DelayS int `json:"delayS,omitempty"`
	// MaxAircraft caps the aircraft per export (0 = no cap).
	MaxAircraft int `json:"maxAircraft,omitempty"`
}

// AggFedReceive says what is accepted from a peer.
type AggFedReceive struct {
	// Aircraft is "all" (default) or "none".
	Aircraft string `json:"aircraft,omitempty"`
	// MLAT is "all" (default) or "none".
	MLAT string `json:"mlat,omitempty"`
	// Boxes keeps only positions inside any box (empty = anywhere).
	Boxes []AggFedBox `json:"boxes,omitempty"`
	// MaxAircraft bounds the aircraft held from this peer (default 20000).
	MaxAircraft int `json:"maxAircraft,omitempty"`
}

// AggFedFeed is one federation-safe outbound feed
// (aggregator.federation.feeds[]).
type AggFedFeed struct {
	// Name is the label (status, logs).
	Name string `json:"name"`
	// Format is "beast" (raw Mode S, timestamps and signal zeroed) or
	// "json" (one aircraft file per line).
	Format string `json:"format"`
	// Mode is "connect" (dial Addr and push) or "listen" (serve on Addr).
	Mode string `json:"mode"`
	// Addr is "host:port".
	Addr string `json:"addr"`
	// Allow lists the client networks of a listen feed (default loopback
	// only).
	Allow []string `json:"allow,omitempty"`
	// MaxClients bounds the clients of a listen feed (default 16).
	MaxClients int `json:"maxClients,omitempty"`
	// IntervalMs is the JSON emission period (default 1000).
	IntervalMs int `json:"intervalMs,omitempty"`
	// ModeAC includes Mode A/C replies in a Beast feed (default off).
	ModeAC bool `json:"modeAC,omitempty"`
	// Disabled keeps the entry without running it.
	Disabled bool `json:"disabled,omitempty"`
	// Policy filters the feed (mlat, transit and delayS do not apply).
	Policy AggFedPolicy `json:"policy"`
}

// clone deep-copies the federation lists.
func (f AggFederation) clone() AggFederation {
	cp := func(p AggFedPolicy) AggFedPolicy {
		p.Boxes, p.ExcludeBoxes = slices.Clone(p.Boxes), slices.Clone(p.ExcludeBoxes)
		return p
	}
	if f.Peers != nil {
		ps := make([]AggFedPeer, len(f.Peers))
		for i, p := range f.Peers {
			p.Send = cp(p.Send)
			p.Receive.Boxes = slices.Clone(p.Receive.Boxes)
			ps[i] = p
		}
		f.Peers = ps
	}
	if f.Feeds != nil {
		fs := make([]AggFedFeed, len(f.Feeds))
		for i, x := range f.Feeds {
			x.Allow = slices.Clone(x.Allow)
			x.Policy = cp(x.Policy)
			fs[i] = x
		}
		f.Feeds = fs
	}
	return f
}

// normalizeWave3 checks the blocks added after Wave-A.
func (a *Aggregator) normalizeWave3(n *notes) {
	p := "aggregator.mlat."
	ck := &a.MLAT.Clock
	if ck.PartitionStations < 0 {
		n.addf("%sclock.partitionStations %d is negative; the default is used", p, ck.PartitionStations)
		ck.PartitionStations = 0
	}
	zeroOrRangeI(n, p+"clock.partitionStations", &ck.PartitionStations, 4, 2000)
	if ck.ExternalStates < 0 {
		ck.ExternalStates = -1 // any negative value = off
	} else if ck.ExternalStates > 1024 {
		n.addf("%sclock.externalStates %d is above 1024; the default is used", p, ck.ExternalStates)
		ck.ExternalStates = 0
	}
	if ck.PartitionDriftNs < 0 {
		ck.PartitionDriftNs = -1 // off
	} else if ck.PartitionDriftNs > 100 || ck.PartitionDriftNs != ck.PartitionDriftNs {
		n.addf("%sclock.partitionDriftNs %g is out of range 0..100; the default is used", p, ck.PartitionDriftNs)
		ck.PartitionDriftNs = 0
	}

	e := &a.MLAT.Engine
	switch f := strings.ToLower(strings.TrimSpace(e.ModeACCodeFormat)); f {
	case "", "pulses", "nibbles":
		e.ModeACCodeFormat = f
	default:
		n.addf("%sengine.modeAcCodeFormat %q is not pulses or nibbles; pulses is used", p, e.ModeACCodeFormat)
		e.ModeACCodeFormat = ""
	}
	if e.ModeACJitterNs < 0 {
		e.ModeACJitterNs = -1 // none
	} else if e.ModeACJitterNs > 1000 || e.ModeACJitterNs != e.ModeACJitterNs {
		n.addf("%sengine.modeAcJitterNs %g is above 1000; the default is used", p, e.ModeACJitterNs)
		e.ModeACJitterNs = 0
	}

	b := &a.MLAT.Survey.Bundle
	bp := p + "survey.bundle."
	zeroOrRangeI(n, bp+"everyMin", &b.EveryMin, 1, 1440)
	zeroOrRangeI(n, bp+"spanMin", &b.SpanMin, 5, 360)
	zeroOrRangeI(n, bp+"maxStations", &b.MaxStations, 2, 512)
	zeroOrRangeF(n, bp+"windowS", &b.WindowS, 2, 120)
	zeroOrRangeI(n, bp+"thinIntervalS", &b.ThinIntervalS, 1, 60)
	zeroOrRangeI(n, bp+"maxFrames", &b.MaxFrames, 1000, 5_000_000)
	zeroOrRangeI(n, bp+"minStationRecs", &b.MinStationRecs, 10, 1_000_000)
	zeroOrRangeF(n, bp+"outlierMinM", &b.OutlierMinM, 1, 100000)
	zeroOrRangeF(n, bp+"outlierK", &b.OutlierK, 1, 100)
	zeroOrRangeF(n, bp+"declaredSigmaM", &b.DeclaredSigmaM, 1, 10000)
	zeroOrRangeF(n, bp+"anchorSigmaM", &b.AnchorSigmaM, 0.1, 1000)
	zeroOrRangeI(n, bp+"maxReleases", &b.MaxReleases, 1, 512)
	zeroOrRangeI(n, bp+"workers", &b.Workers, 1, 256)

	a.normalizeFederation(n)
}

// normalizeFederation range-checks the scalar federation settings; the
// detailed peer/feed checks are internal/federation's Config.Check (run
// by `airview config check`).
func (a *Aggregator) normalizeFederation(n *notes) {
	f := &a.Federation
	p := "aggregator.federation."
	f.Name = strings.TrimSpace(f.Name)
	f.Listen = strings.TrimSpace(f.Listen)
	zeroOrRangeI(n, p+"intervalMs", &f.IntervalMs, 100, 60000)
	zeroOrRangeI(n, p+"mlatIntervalMs", &f.MLATIntervalMs, 50, 10000)
	zeroOrRangeI(n, p+"fullRefreshS", &f.FullRefreshS, 1, 3600)
	zeroOrRangeI(n, p+"ttlS", &f.TTLS, 5, 3600)
	zeroOrRangeI(n, p+"maxHops", &f.MaxHops, 1, 8)
	zeroOrRangeI(n, p+"maxRemoteAircraft", &f.MaxRemoteAircraft, 100, 1_000_000)
	zeroOrRangeI(n, p+"queueRecords", &f.QueueRecords, 8, 100000)
	if f.MaxInflateBps != 0 && (f.MaxInflateBps < 100_000 || f.MaxInflateBps > 1_000_000_000) {
		n.addf("%smaxInflateBps %d is out of range 100000..1000000000; the default is used", p, f.MaxInflateBps)
		f.MaxInflateBps = 0
	}
	if len(f.Peers) == 0 {
		f.Peers = nil
	}
	if len(f.Feeds) == 0 {
		f.Feeds = nil
	}
}
