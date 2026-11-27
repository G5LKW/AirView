package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// This file holds the configuration contract of the AirView network
// (docs/aggregator/DESIGN.md §1, §19, §20): uplinks that feed another AirView
// over AVF/1, the aggregator settings, and the stand-alone feeder program's
// configuration.
//
// Every block has a Default* constructor, a Normalize method that clamps
// values to their documented ranges (it returns one human readable note per
// correction, the format config.Validate and "airview config check" print),
// and Clone/Equal helpers because the blocks contain slices.

// Default ports of the network listeners (DESIGN §0).
const (
	DefaultAVFPort      = 30160 // AVF/1 native feeder protocol (TLS)
	DefaultBeastInPort  = 30004 // Beast / beast_reduce(_plus)_out compat ingest
	DefaultMLATCPort    = 31090 // mlat-client protocol v3 compat ingest
	DefaultResultsPort  = 30105 // feeder: local Beast output of MLAT results
	DefaultFeederUIAddr = "127.0.0.1:8099"
)

// Admission policies for new stations (DESIGN §3).
const (
	AdmissionOpen    = "open"    // auto-approve every new station
	AdmissionApprove = "approve" // hold new stations pending until the admin approves
	AdmissionInvite  = "invite"  // require an admin-minted enrol token (default)
)

// SpoolAuto, as Uplink.SpoolMB, selects the platform default spool size at
// run time (see ResolveSpoolMB).
const SpoolAuto = -1

// Uplink is one connection from this AirView (standalone or aggregator
// mode) or from airview-feeder to an AirView aggregator over AVF/1.
// Config.Uplinks / FeederConfig.Uplinks hold them.
//
// Missing JSON fields take the DefaultUplink values (see UnmarshalJSON), so a
// hand-written entry needs only "id" and "host".
type Uplink struct {
	// ID is a stable slug, unique among the uplinks.
	ID string `json:"id"`
	// Host is the aggregator's host name or IP address.
	Host string `json:"host"`
	// Port is the aggregator's AVF/1 port (default 30160).
	Port int `json:"port"`
	// ServerPin is the expected server key: "sha256/<base64 SHA-256 of the
	// certificate's SubjectPublicKeyInfo>". "" verifies the certificate
	// against the system roots instead.
	ServerPin string `json:"serverPin"`
	// EnrolToken is the admin-minted enrol token for an "invite" aggregator.
	// It is a secret: config redaction treats it like the hub password.
	EnrolToken string `json:"enrolToken"`
	// Inputs lists the local feed IDs (Config.Feeds[].ID) forwarded on this
	// uplink. Empty = every enabled feed that delivers timed frames.
	Inputs []string `json:"inputs,omitempty"`
	// SendLocation sends the receiver position in the RECEIVER record
	// (default true). Off = the aggregator must locate the station itself.
	SendLocation bool `json:"sendLocation"`
	// ShareGNSS forwards the receiver's own GNSS position (Radarcape type
	// '5' frames) as GNSS_POS records (default false).
	ShareGNSS bool `json:"shareGnss"`
	// Timestamps sends receiver timestamps (default true). Off = no MLAT
	// and no timing-based self-location: only untimed data is sent.
	Timestamps bool `json:"timestamps"`
	// ReturnMLAT asks for MLAT results for aircraft this station helped to
	// locate (default true).
	ReturnMLAT bool `json:"returnMlat"`
	// SpoolMB is the size of the on-disk spool written while disconnected.
	// 0 = RAM ring only; SpoolAuto (-1, default) = 0 when the root
	// filesystem is an SD card, 256 on other Linux hosts, 1024 on Windows.
	SpoolMB int `json:"spoolMb"`
	// Pin is the requested public map pin precision (PinHidden default,
	// PinRegion, PinCell01).
	Pin string `json:"pin"`
	// PublicStats publishes non-locating statistics of this station
	// (default false).
	PublicStats bool `json:"publicStats"`
	// ShowName shows the station name publicly (default false).
	ShowName bool `json:"showName"`
	// Federate is used by an aggregator only (phase 5 federation): besides
	// the host station, forward every approved network station as its own
	// receiver. Default false.
	Federate bool `json:"federate,omitempty"`
	// Disabled keeps the entry but does not connect.
	Disabled bool `json:"disabled"`

	// Type is UplinkAVF ("avf", default when empty: an AirView aggregator
	// over AVF/1) or UplinkBeast ("beast": "also feed others", a Beast
	// output to a third-party aggregator such as readsb on port 30004;
	// DESIGN §12.1 phase 5). For a Beast entry the port defaults to 30004,
	// timestamps applies, and serverPin, enrolToken, spoolMb, shareGnss,
	// returnMlat, sendLocation, pin, publicStats, showName and federate do
	// not apply.
	Type string `json:"type,omitempty"`
	// Format is the Beast output format (Beast only):
	// beast_reduce_plus_out (default), beast_reduce_out or beast_out.
	Format string `json:"format,omitempty"`
	// UUID is the station UUID sent to a Beast aggregator (Beast only;
	// "" = a random UUID generated for this uplink). An identifier at that
	// aggregator, not a password: shown to the owner, never public.
	UUID string `json:"uuid,omitempty"`
	// ReduceMs is the Beast reduce interval (Beast only; default 250,
	// 50..5000 ms; ignored for beast_out).
	ReduceMs int `json:"reduceMs,omitempty"`
}

// Uplink types (Uplink.Type).
const (
	UplinkAVF   = "avf"
	UplinkBeast = "beast"
	// DefaultBeastOutPort is the default port of a Beast uplink.
	DefaultBeastOutPort = 30004
)

// IsBeast reports whether u is a Beast output ("also feed others").
func (u Uplink) IsBeast() bool { return u.Type == UplinkBeast }

// DefaultUplink returns an Uplink with every default applied and no ID/host.
func DefaultUplink() Uplink {
	return Uplink{
		Port:         DefaultAVFPort,
		SendLocation: true,
		Timestamps:   true,
		ReturnMLAT:   true,
		SpoolMB:      SpoolAuto,
		Pin:          PinHidden,
	}
}

// UnmarshalJSON decodes an uplink on top of DefaultUplink, so fields missing
// from the JSON keep their defaults (important for the true-by-default
// booleans). A field of the wrong JSON type keeps its default and does not
// abort decoding of the surrounding document.
func (u *Uplink) UnmarshalJSON(b []byte) error {
	type plain Uplink
	p := plain(DefaultUplink())
	if err := json.Unmarshal(b, &p); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return err
		}
	}
	*u = Uplink(p)
	return nil
}

// Clone returns a deep copy of u.
func (u Uplink) Clone() Uplink {
	u.Inputs = slices.Clone(u.Inputs)
	return u
}

// Equal reports whether two uplinks are identical in every setting.
func (u Uplink) Equal(o Uplink) bool { return reflect.DeepEqual(u, o) }

// Addr returns "host:port".
func (u Uplink) Addr() string { return net.JoinHostPort(u.Host, fmt.Sprint(u.Port)) }

// ResolveSpoolMB turns an Uplink.SpoolMB value into the effective spool size
// in MiB for the given GOOS and root filesystem (sdRoot = the root
// filesystem lives on an mmcblk SD card).
func ResolveSpoolMB(spoolMB int, goos string, sdRoot bool) int {
	if spoolMB >= 0 {
		return spoolMB
	}
	switch {
	case goos == "windows":
		return 1024
	case sdRoot:
		return 0
	}
	return 256
}

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidSlug reports whether s is a valid config ID (lower-case letters,
// digits, '-' and '_', 1-32 characters, starting with a letter or digit).
func ValidSlug(s string) bool { return slugRE.MatchString(s) }

// ValidServerPin reports whether s is "" or "sha256/<base64 of 32 bytes>".
func ValidServerPin(s string) bool {
	if s == "" {
		return true
	}
	b64, ok := strings.CutPrefix(s, "sha256/")
	if !ok {
		return false
	}
	_, err := decodeB64Len(b64, 32)
	return err == nil
}

// Normalize corrects out-of-range values in place and returns one note per
// correction. path prefixes the notes (e.g. "uplinks[0]").
func (u *Uplink) Normalize(path string) []string {
	var n notes
	def := DefaultUplink()
	u.ID = strings.TrimSpace(u.ID)
	u.Host = strings.TrimSpace(u.Host)
	switch t := strings.ToLower(strings.TrimSpace(u.Type)); t {
	case "", UplinkAVF:
		u.Type = ""
	case UplinkBeast:
		u.Type = UplinkBeast
		u.normalizeBeast(path, &n)
	default:
		n.addf("%s.type %q is not avf or beast; avf is used", path, u.Type)
		u.Type = ""
	}
	if !u.IsBeast() && (u.Format != "" || u.UUID != "" || u.ReduceMs != 0) {
		n.addf("%s: format, uuid and reduceMs apply to Beast uplinks only; they were cleared", path)
		u.Format, u.UUID, u.ReduceMs = "", "", 0
	}
	if u.Port < 1 || u.Port > 65535 {
		n.addf("%s.port %d is out of range 1..65535; %d is used", path, u.Port, def.Port)
		u.Port = def.Port
	}
	if !ValidServerPin(u.ServerPin) {
		n.addf("%s.serverPin is not \"sha256/<base64>\"; it was cleared (system roots are used)", path)
		u.ServerPin = ""
	}
	if u.SpoolMB < SpoolAuto || u.SpoolMB > 65536 {
		n.addf("%s.spoolMb %d is out of range -1..65536; automatic sizing is used", path, u.SpoolMB)
		u.SpoolMB = SpoolAuto
	}
	if !ValidPin(u.Pin) {
		n.addf("%s.pin %q is not hidden/region/cell01; hidden is used", path, u.Pin)
		u.Pin = PinHidden
	}
	if u.Host == "" && !u.Disabled {
		n.addf("%s has no host; it was disabled", path)
		u.Disabled = true
	}
	return n
}

// normalizeBeast applies the Beast defaults and ranges. The AVF default
// port (a missing "port") becomes 30004. Settings that apply to AVF/1 only
// are left alone (the file stays as written) but reported when they differ
// from the default.
func (u *Uplink) normalizeBeast(path string, n *notes) {
	if u.Port == 0 || u.Port == DefaultAVFPort {
		u.Port = DefaultBeastOutPort
	}
	switch f := strings.ToLower(strings.TrimSpace(u.Format)); f {
	case "", "beast_reduce_plus_out", "reduce_plus", "beast_reduce_plus":
		u.Format = "beast_reduce_plus_out"
	case "beast_reduce_out", "reduce", "beast_reduce":
		u.Format = "beast_reduce_out"
	case "beast_out", "beast", "full":
		u.Format = "beast_out"
	default:
		n.addf("%s.format %q is unknown; beast_reduce_plus_out is used", path, u.Format)
		u.Format = "beast_reduce_plus_out"
	}
	if u.ReduceMs != 0 && (u.ReduceMs < 50 || u.ReduceMs > 5000) {
		n.addf("%s.reduceMs %d is out of range 50..5000; 250 is used", path, u.ReduceMs)
		u.ReduceMs = 0
	}
	u.UUID = strings.TrimSpace(u.UUID)
	if u.UUID != "" && !validUUIDText(u.UUID) {
		n.addf("%s.uuid is not a valid UUID; a generated one is used", path)
		u.UUID = ""
	}
	def := DefaultUplink()
	var ignored []string
	if u.ServerPin != "" {
		ignored = append(ignored, "serverPin")
	}
	if u.EnrolToken != "" {
		ignored = append(ignored, "enrolToken")
	}
	if u.SpoolMB != def.SpoolMB {
		ignored = append(ignored, "spoolMb")
	}
	if u.ShareGNSS {
		ignored = append(ignored, "shareGnss")
	}
	if u.Pin != def.Pin && u.Pin != "" {
		ignored = append(ignored, "pin")
	}
	if u.PublicStats {
		ignored = append(ignored, "publicStats")
	}
	if u.ShowName {
		ignored = append(ignored, "showName")
	}
	if u.Federate {
		ignored = append(ignored, "federate")
	}
	if len(ignored) > 0 {
		n.addf("%s: %s do not apply to a Beast uplink and are ignored", path, strings.Join(ignored, ", "))
	}
}

// validUUIDText accepts 28..36 hex digits with optional dashes (the forms
// readsb-style aggregators accept).
func validUUIDText(s string) bool {
	h := 0
	for _, r := range s {
		switch {
		case r == '-':
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
			h++
		default:
			return false
		}
	}
	return h >= 28 && h <= 36
}

// NormalizeUplinks normalizes every uplink, makes IDs valid and unique
// ("uplink", "uplink-2", ...) and returns the notes.
func NormalizeUplinks(ups []Uplink) []string {
	var n notes
	seen := map[string]bool{}
	for i := range ups {
		path := fmt.Sprintf("uplinks[%d]", i)
		n = append(n, ups[i].Normalize(path)...)
		id := strings.ToLower(ups[i].ID)
		if !ValidSlug(id) {
			id = "uplink"
		}
		base, k := id, 2
		for seen[id] {
			id = fmt.Sprintf("%s-%d", base, k)
			k++
		}
		if id != ups[i].ID {
			n.addf("%s.id %q was changed to %q", path, ups[i].ID, id)
			ups[i].ID = id
		}
		seen[id] = true
	}
	return n
}

// CloneUplinks deep-copies a list of uplinks (nil stays nil).
func CloneUplinks(ups []Uplink) []Uplink {
	if ups == nil {
		return nil
	}
	out := make([]Uplink, len(ups))
	for i, u := range ups {
		out[i] = u.Clone()
	}
	return out
}

// AggMLAT holds the MLAT engine settings of an aggregator (DESIGN §9, §19).
type AggMLAT struct {
	// Enabled runs the own MLAT engine (default true).
	Enabled bool `json:"enabled"`
	// ReturnResults sends results to contributing stations (default true;
	// the minimum-contribution rule of DESIGN §8.4 always applies).
	ReturnResults bool `json:"returnResults"`
	// Holdout is the fraction of good ADS-B aircraft solved as Mode S-only
	// for self-validation (default 0.10, 0..0.5).
	Holdout float64 `json:"holdout"`
	// AutoUseSurvey uses a reverse-MLAT station position for MLAT without
	// owner confirmation when its horizontal sigma is at most
	// AutoUseMaxSigmaM and it passes the suspect checks (default true,
	// user decision DESIGN §19.3).
	AutoUseSurvey bool `json:"autoUseSurvey"`
	// AutoUseMaxSigmaM is that sigma threshold in metres (default 50, 5..500).
	AutoUseMaxSigmaM float64 `json:"autoUseMaxSigmaM"`
	// EpochMs is the clock filter epoch (default 500, 100..2000).
	EpochMs int `json:"epochMs"`
	// MaxComponentStations is the station count above which a clock-sync
	// component is partitioned (default 200, 20..2000; DESIGN §20.2).
	MaxComponentStations int `json:"maxComponentStations"`
	// ModeAC enables Mode A/C multilateration among anchors (default false).
	ModeAC bool `json:"modeAc"`
	// UDP accepts the mlat-client UDP transport on MLATPort (default false).
	UDP bool `json:"udp"`
	// SurveyIntervalH is how often declared positions are re-surveyed
	// (default 6, 1..168 hours).
	SurveyIntervalH int `json:"surveyIntervalH"`
	// RefractivityN0 is the surface radio refractivity in N-units used for
	// the propagation delay (default 315, 200..450; 0 = derive it from METAR
	// when available, else 315).
	RefractivityN0 float64 `json:"refractivityN0"`
	// RefractivityScaleHeightM is the scale height of the exponential
	// refractivity profile in metres (default 7000, 4000..12000).
	RefractivityScaleHeightM float64 `json:"refractivityScaleHeightM"`
	// GeoidFile is a geoid grid replacing the embedded EGM96 1-degree grid
	// (NGA WW15MGH.GRD, GeographicLib .pgm or AirView .avg; "" = embedded).
	GeoidFile string `json:"geoidFile,omitempty"`

	// Ref, Group, Plan, Solve, Track and Engine tune the MLAT engine's parts
	// (zero = the package default; see each block).
	Ref   AggMLATRef   `json:"ref"`
	Group AggMLATGroup `json:"group"`
	Plan  AggMLATPlan  `json:"plan"`
	Solve AggMLATSolve `json:"solve"`
	Track AggMLATTrack `json:"track"`
	// Engine tunes the MLAT engine root (internal/mlat): workers, queues,
	// result routing and the quality feedback switches (zero = default).
	Engine AggMLATEngine `json:"engine"`
	// Clock tunes the joint clock filter (internal/mlat/clock): gates, step
	// detection, anchor probation and blacklisting (zero = default).
	Clock AggMLATClock `json:"clock"`
	// Quarantine is the timing quarantine of new stations
	// (internal/registry/trust; zero = default).
	Quarantine AggMLATQuarantine `json:"quarantine"`
	// Survey tunes the reverse-MLAT station survey (internal/mlat/survey;
	// zero = default).
	Survey AggMLATSurvey `json:"survey"`
	// LatencyCal is the per-airframe ADS-B latency calibration of the
	// reference tracks with its A/B gate (default off).
	LatencyCal AggMLATLatencyCal `json:"latencyCal"`
}

// AggAuth holds the aggregator account and session policy (DESIGN §7, §20.3).
type AggAuth struct {
	// OwnerSessionDays is the absolute lifetime of a station-owner session
	// (default 30, 1..365 days).
	OwnerSessionDays int `json:"ownerSessionDays"`
	// OwnerIdleDays ends an owner session without activity (default 14,
	// 1..365 days).
	OwnerIdleDays int `json:"ownerIdleDays"`
	// AdminSessionHours is the absolute lifetime of an admin session
	// (default 12, 1..168 hours).
	AdminSessionHours int `json:"adminSessionHours"`
	// AdminIdleMinutes ends an admin session without activity (default 120,
	// 5..1440 minutes).
	AdminIdleMinutes int `json:"adminIdleMinutes"`
	// Registration is "open" (default: anyone may create an owner account)
	// or "closed" (accounts are created by the admin only).
	Registration string `json:"registration"`
	// LoginFreeFailures is the number of failed logins per client and user
	// name before the login throttle starts delaying (default 5, 1..100).
	LoginFreeFailures int `json:"loginFreeFailures"`
}

// Registration policies of AggAuth.Registration.
const (
	RegistrationOpen   = "open"
	RegistrationClosed = "closed"
)

// AggRetention holds the aggregator's retention schedule (DESIGN §8.5, §20.2).
type AggRetention struct {
	ConnLogDays      int `json:"connLogDays"`      // connection log (default 30, 1..365)
	MinuteDays       int `json:"minuteDays"`       // per-station minute stats (default 30, 1..365)
	HourMonths       int `json:"hourMonths"`       // per-station hourly stats (default 13, 1..120)
	OfflinePurgeDays int `json:"offlinePurgeDays"` // unclaimed + offline stations are purged after (default 90, 7..3650)
	AuditMonths      int `json:"auditMonths"`      // audit log (default 24, 1..120)
	// EstimatesPerMethod is how many location estimates are kept per
	// station and method (default 5, 1..100).
	EstimatesPerMethod int `json:"estimatesPerMethod"`
	// BackupDays is how long the operator's backups keep deleted data
	// (default 30, 1..3650). AirView makes no backups by itself; the value
	// is what owners are told when they delete their data (consent text C8,
	// privacy notice), so set it to the retention of your backup rotation.
	BackupDays int `json:"backupDays"`
}

// AggLimits holds the aggregator's admission and load limits (DESIGN §4.6),
// sized for the publication target of 1,000+ feeders and 10k+ concurrent
// aircraft on a mid-size VPS (DESIGN §20.2).
type AggLimits struct {
	// MaxStations is the maximum number of registered, approved stations
	// (default 2000, 1..100000).
	MaxStations int `json:"maxStations"`
	// SelectiveAbove switches stations to selective traffic when more than
	// this many approved stations are online (default 100, 0..100000;
	// 0 = always selective).
	SelectiveAbove int `json:"selectiveAbove"`
	// PerIPStations is the maximum number of stations behind one IP address
	// unless the address is in AllowCIDRs (default 8, 1..1000).
	PerIPStations int `json:"perIpStations"`
	// MaxFPSPerStation: above this frame rate the station gets FLOW, then is
	// disconnected with a cool-down (default 6000, 100..100000).
	MaxFPSPerStation int `json:"maxFpsPerStation"`
	// MaxKBpsPerStation is the compressed byte-rate limit per station in
	// kB/s (default 200, 10..10000).
	MaxKBpsPerStation int `json:"maxKBpsPerStation"`
	// MaxUnauthPerIP is the number of not yet authenticated connections per
	// IP (default 4, 1..1000).
	MaxUnauthPerIP int `json:"maxUnauthPerIp"`
	// MaxConnsPerIP is the number of connections per IP (default 16, 1..10000).
	MaxConnsPerIP int `json:"maxConnsPerIp"`
	// MaxConnsPerSubnet is the number of connections per IPv4 /24 or IPv6 /48
	// (default 32, 1..100000).
	MaxConnsPerSubnet int `json:"maxConnsPerSubnet"`
	// HandshakeTimeoutSec bounds the TLS + AVF handshake (default 10, 2..120).
	HandshakeTimeoutSec int `json:"handshakeTimeoutSec"`
	// IngestFPS is the global ingest budget in frames per second across all
	// stations; selective-eligible traffic is shed first above it
	// (default 3,000,000, 10,000..100,000,000).
	IngestFPS int `json:"ingestFps"`
	// MaxAircraft is the maximum number of concurrently tracked aircraft
	// (default 30000, 1000..1,000,000).
	MaxAircraft int `json:"maxAircraft"`
	// TrackerShards is the number of ICAO shards of the global tracker
	// (default 0 = automatic from GOMAXPROCS, else 1..256).
	TrackerShards int `json:"trackerShards"`
	// PublicRPS / PublicBurst rate-limit the public API per client IP
	// (defaults 1 and 10).
	PublicRPS   float64 `json:"publicRps"`
	PublicBurst int     `json:"publicBurst"`
	// MaxPublicSSE is the total number of public SSE streams (one per IP;
	// default 2000, 1..100000).
	MaxPublicSSE int `json:"maxPublicSse"`
	// MaxHTTPConns is the total number of open web (HTTP) connections
	// (default 20000, 100..1,000,000); further connections wait in the
	// kernel backlog.
	MaxHTTPConns int `json:"maxHttpConns"`
	// MaxHTTPConnsPerIP is the number of open web connections per client
	// IPv4 address or IPv6 /64 (default 64, 1..100000). Loopback and
	// aggregator.trustedProxies are exempt.
	MaxHTTPConnsPerIP int `json:"maxHttpConnsPerIp"`
	// AllowCIDRs are exempt from PerIPStations and the per-IP / per-subnet
	// connection limits (e.g. a club's relay host).
	AllowCIDRs []string `json:"allowCidrs,omitempty"`
}

// Aggregator is the configuration of aggregator mode (DESIGN §1). It is used
// only when Config.Mode is ModeAggregator. Registry data, keys, owners,
// claims and enrol tokens are mutable secrets that live in stations.db, not
// here.
//
// Missing JSON fields take their default (see UnmarshalJSON), so a config
// file needs to list only the settings it changes.
type Aggregator struct {
	// Name is the network's display name.
	Name string `json:"name"`
	// PublicHost is the host name feeders use to reach this aggregator
	// (embedded in install one-liners). "" = taken from the HTTP request.
	PublicHost string `json:"publicHost"`
	// PrivacyNoticeURL is the address of the operator's published privacy
	// notice (template: docs/aggregator/PRIVACY-NOTICE.md), linked from the
	// registration form, the feeder page and the consent texts: an http(s)
	// URL or a path on this server starting with "/" ("" = no link).
	PrivacyNoticeURL string `json:"privacyNoticeUrl"`
	// Listener ports; 0 disables a listener.
	AVFPort        int `json:"avfPort"`        // AVF/1 (default 30160)
	BeastPort      int `json:"beastPort"`      // Beast compat ingest (default 30004)
	MLATPort       int `json:"mlatPort"`       // mlat-client compat ingest (default 31090)
	SBSInLocalPort int `json:"sbsInLocalPort"` // 127.0.0.1-only SBS bridge input (default 0)
	BeastOutPort   int `json:"beastOutPort"`   // merged Beast output (default 0)
	SBSOutPort     int `json:"sbsOutPort"`     // merged SBS output (default 0)
	// OutputToken protects BeastOutPort / SBSOutPort ("" = outputs refuse
	// all clients). It is a secret.
	OutputToken string `json:"outputToken"`
	// Admission is AdmissionOpen, AdmissionApprove or AdmissionInvite (default).
	Admission string `json:"admission"`
	// TLSCert / TLSKey are PEM files of an operator certificate; empty =
	// self-signed ECDSA P-256 certificate in <data>/avf-tls/. The key path is
	// treated as a secret by config redaction.
	TLSCert string `json:"tlsCert"`
	TLSKey  string `json:"tlsKey"`
	// TrustedProxies are CIDRs whose X-Forwarded-For header is honoured.
	TrustedProxies []string `json:"trustedProxies,omitempty"`
	// PublicMap / PublicStats / TarJSON enable the public merged map, the
	// public network statistics and the tar1090-shaped /data/*.json
	// (defaults true, true, true).
	PublicMap   bool `json:"publicMap"`
	PublicStats bool `json:"publicStats"`
	TarJSON     bool `json:"tarJson"`
	// DefaultPin is the pin precision of new stations: PinHidden (default),
	// PinRegion or PinCell01.
	DefaultPin string `json:"defaultPin"`
	// IslandGuard coarsens low single-station aircraft heard only by hidden
	// stations (DESIGN §8.6; default false).
	IslandGuard bool `json:"islandGuard"`
	// TrustedRelays are the Beast relays whose 0xE3 receiver IDs are
	// honoured (each becomes a sub-station): "uuid@cidr" binds the relay's
	// station UUID to the addresses it connects from; a plain "uuid" is
	// honoured only from limits.allowCidrs. The UUID alone is no secret.
	TrustedRelays []string `json:"trustedRelays,omitempty"`
	// ListenAddr is the bind address of the feeder listeners (AVF, Beast,
	// mlat-client); "" = every interface. The -listen-host flag and
	// $AIRVIEW_LISTEN_HOST take precedence.
	ListenAddr string `json:"listenAddr"`
	// FeederReleaseDir is a directory with signed airview-feeder releases
	// served to the Pi installer: manifest.json, manifest.json.sig and one
	// file per target (linux-arm64, linux-armv7, linux-amd64, ...) at
	// /feeder/manifest.json, /feeder/manifest.json.sig and
	// /feeder/dl/<target> ("" = not served; the installer then needs a
	// local binary).
	FeederReleaseDir string `json:"feederReleaseDir"`
	// FeederManifestKey is the base64 Ed25519 public key (32 bytes) that
	// signs manifest.json; /feeder/install.sh carries it as its preset
	// ("" = none).
	FeederManifestKey string `json:"feederManifestKey"`

	MLAT      AggMLAT      `json:"mlat"`
	Retention AggRetention `json:"retention"`
	Limits    AggLimits    `json:"limits"`
	Auth      AggAuth      `json:"auth"`
	Beast     AggBeast     `json:"beast"`
	Lanes     AggLanes     `json:"lanes"`
	Storage   AggStorage   `json:"storage"`
	Ingest    AggIngest    `json:"ingest"`
	MLATC     AggMLATC     `json:"mlatc"`
	// Positions rate-limits and holds station positions announced by the
	// feeders themselves (zero = default).
	Positions AggPositions `json:"positions"`
	// Federation links this network with other AirView networks and runs
	// federation-safe outbound feeds (default off).
	Federation AggFederation `json:"federation"`
	// ShareLinks limits the read-only share links of station owners
	// (default on).
	ShareLinks AggShareLinks `json:"shareLinks"`
}

// AggStorage holds the tiered retention and maintenance settings of the
// aggregator's databases (airview.db and stations.db; DESIGN §20.2, §20.3).
// They map onto store.TierPolicy.
type AggStorage struct {
	// MinuteDays keeps minute statistics this long (0 = retention.minuteDays,
	// else 1..3650).
	MinuteDays int `json:"minuteDays"`
	// HourDays keeps hourly statistics this long (0 = 30 x retention.hourMonths,
	// else 7..3650).
	HourDays int `json:"hourDays"`
	// DayDays keeps the daily totals this long (default 0 = forever, else
	// 1..36500).
	DayDays int `json:"dayDays"`
	// HistDailyDays keeps histograms per day this long before they are merged
	// into month buckets (default 35, 1..3650).
	HistDailyDays int `json:"histDailyDays"`
	// HistDays deletes histogram rows older than this (default 395,
	// 0 = forever).
	HistDays int `json:"histDays"`
	// HeatIdleDays deletes heat cells not updated for this long (default 395,
	// 0 = forever).
	HeatIdleDays int `json:"heatIdleDays"`
	// FlightStationDays deletes the station links of flights that ended
	// longer ago (default 90; 0 = keep them as long as the flights).
	FlightStationDays int `json:"flightStationDays"`
	// TrailDays keeps the stored position trails (the positions table of
	// airview.db, the largest table of a busy aggregator: about 0.6 GB per
	// 1000 aircraft and day) of flights that ended this long ago; it
	// replaces storage.trailRetentionDays in aggregator mode (default 7,
	// 1..3650).
	TrailDays int `json:"trailDays"`
	// TrailFullDays keeps the stored trails of flights at full resolution
	// for this many days after the flight ended, then thins them to one
	// point per TrailThinSec (default 2, 0..3650; 0 = never thin).
	TrailFullDays int `json:"trailFullDays"`
	// TrailThinSec is the point interval of thinned trails in seconds
	// (default 30, 0 or 2..3600; 0 = never thin).
	TrailThinSec int `json:"trailThinSec"`
	// FreePct releases free pages when more than this share of a file is
	// free (default 10, 1..90 %).
	FreePct int `json:"freePct"`
	// WALCheckpointMB truncates a write-ahead log larger than this (default
	// 64, 4..4096 MiB).
	WALCheckpointMB int `json:"walCheckpointMb"`
	// OptimizeHours is the interval of PRAGMA optimize (default 1, 1..720 h; bounded by analysis_limit, so an hourly run costs milliseconds).
	OptimizeHours int `json:"optimizeHours"`
	// QueueRows bounds the write queue of stations.db in rows (default
	// 2000000, 10000..50000000).
	QueueRows int `json:"queueRows"`
}

// DefaultAggStorage returns the storage defaults.
func DefaultAggStorage() AggStorage {
	return AggStorage{
		HistDailyDays: 35, HistDays: 395, HeatIdleDays: 395, FlightStationDays: 90, TrailDays: 7,
		TrailFullDays: 2, TrailThinSec: 30,
		FreePct: 10, WALCheckpointMB: 64, OptimizeHours: 1, QueueRows: 2_000_000,
	}
}

// AggIngest holds the tunables of the station-aware ingest and the AVF/1
// listener (internal/ingest, internal/ingest/avfsrv; DESIGN §6.2, §20.3).
type AggIngest struct {
	// WindowMs: a group accepts copies this long after its first copy
	// (default 1000, 200..5000).
	WindowMs int `json:"windowMs"`
	// FlushAfterMs: a group goes to MLAT this long after its first copy
	// (default 400, 100..windowMs).
	FlushAfterMs int `json:"flushAfterMs"`
	// RetainMs keeps a closed group for de-duplication (default 1000,
	// 100..10000).
	RetainMs int `json:"retainMs"`
	// MaxMatchMs caps the time tolerance of a match (default 300, 20..700).
	MaxMatchMs int `json:"maxMatchMs"`
	// MinMatchMs is the smallest tolerance (default 10, 1..maxMatchMs).
	MinMatchMs int `json:"minMatchMs"`
	// UniqueSkewMs is the largest skew at which a non-repeating payload
	// joins a group before the station's time correction is calibrated
	// (default 2500, 100..10000).
	UniqueSkewMs int `json:"uniqueSkewMs"`
	// LateAfterMs routes older frames to the statistics lanes only
	// (0 = windowMs + retainMs + 500, else 500..60000).
	LateAfterMs int `json:"lateAfterMs"`
	// Shards is the number of grouper shards (default 64, 1..4096; rounded
	// up to a power of two).
	Shards int `json:"shards"`
	// ReduceMs is the reduce interval of selective mode (default 500,
	// 100..5000).
	ReduceMs int `json:"reduceMs"`
	// FlowHoldSec tunes the FLOW rules (default 5, 1..60).
	FlowHoldSec int `json:"flowHoldSec"`
	// FlowGraceSec tunes the FLOW rules (default 10, 1..300).
	FlowGraceSec int `json:"flowGraceSec"`
	// AVFQueueRecords is the per-connection queue of the AVF listener in
	// record batches (default 64, 4..4096).
	AVFQueueRecords int `json:"avfQueueRecords"`
	// AVFResumeGraceSec keeps a station online after its connection ended,
	// for a resume (default 30, 1..300).
	AVFResumeGraceSec int `json:"avfResumeGraceSec"`
	// AVFMaxSessions caps concurrent AVF connections (default 10000,
	// 1..100000).
	AVFMaxSessions int `json:"avfMaxSessions"`
	// ClockGateFloorUs is the smallest tolerance of the synced-station
	// network-time gate in µs (0 = default 50; -1 = no floor).
	ClockGateFloorUs int `json:"clockGateFloorUs"`
	// FlushSampleEvery feeds every n-th group's delays to the adaptive
	// flush estimator (0 = default 4).
	FlushSampleEvery int `json:"flushSampleEvery"`
	// AVFHostSlackMs bounds how much earlier than physically plausible an
	// AVF feeder's host times may place its frames before the host-clock
	// offset is corrected (0 = default 300; -1 = off).
	AVFHostSlackMs int `json:"avfHostSlackMs"`
	// AdaptiveFlush replaces the fixed flushAfterMs of MLAT groups by a
	// per-aircraft delay from the delivery latency of the stations that hear
	// it (150-600 ms; default false).
	AdaptiveFlush bool `json:"adaptiveFlush"`
}

// DefaultAggIngest returns the ingest defaults.
func DefaultAggIngest() AggIngest {
	return AggIngest{
		WindowMs: 1000, FlushAfterMs: 400, RetainMs: 1000, MaxMatchMs: 300, MinMatchMs: 10,
		UniqueSkewMs: 2500, Shards: 64, ReduceMs: 500, FlowHoldSec: 5, FlowGraceSec: 10,
		AVFQueueRecords: 64, AVFResumeGraceSec: 30, AVFMaxSessions: 10000,
	}
}

// AggBeast holds the tunables of the Beast compat listener on BeastPort
// (DESIGN §5.1, §20.3). The rate and connection limits are in AggLimits.
type AggBeast struct {
	// IdentifyWaitMs is how long a link may send Beast data before its UUID
	// arrives; after it the link is attached as unidentified (default 2000,
	// 100..60000 ms).
	IdentifyWaitMs int `json:"identifyWaitMs"`
	// IdleTimeoutSec closes links that are silent this long (default 180,
	// 10..3600 s).
	IdleTimeoutSec int `json:"idleTimeoutSec"`
	// PingIntervalSec is the period of the server's own pings once the
	// feeder asked for them (default 5, 1..300 s).
	PingIntervalSec int `json:"pingIntervalSec"`
	// SlowDownEverySec is the minimum interval between two slow-down
	// requests on one link (default 10, 1..600 s).
	SlowDownEverySec int `json:"slowDownEverySec"`
	// QueueBatches is the per-link delivery queue length in batches
	// (default 64, 4..65536).
	QueueBatches int `json:"queueBatches"`
	// MaxLinks caps concurrent Beast links (default 4096, 1..100000).
	MaxLinks int `json:"maxLinks"`
	// AcceptMlatResults passes frames carrying the MLAT magic timestamp
	// (default false: they are dropped and counted).
	AcceptMlatResults bool `json:"acceptMlatResults"`
	// StripClock clears the receiver timestamps of every Beast frame
	// (default false; Beast frames never enter MLAT either way).
	StripClock bool `json:"stripClock"`
	// OverLimitSeconds consecutive seconds above the rate limits disconnect
	// a link (default 5, 1..300).
	OverLimitSeconds int `json:"overLimitSeconds"`
	// CoolDownSec is how long a station disconnected for its rate is refused
	// (default 300, 0..86400 s; 0 = no cool-down).
	CoolDownSec int `json:"coolDownSec"`
}

// AggLanes holds the per-station statistics lane settings of the network
// tracker (DESIGN §6.3-6.4, §20.3).
type AggLanes struct {
	// FlushSec is the interval of the per-station minute statistics rows
	// (default 10, 2..60 s).
	FlushSec int `json:"flushSec"`
	// SlowFlushSec is the interval of the range, coverage, signal, elevation
	// and heat increments (default 300, 60..3600 s).
	SlowFlushSec int `json:"slowFlushSec"`
	// EvictHours drops the in-memory lane of a station silent this long
	// (default 24, 1..720 h).
	EvictHours int `json:"evictHours"`
	// MaxRangeKm is the range gate: positions farther than this from a
	// station never enter its range and coverage products, and are refused
	// as implausible for it (default 800, 100..1500 km).
	MaxRangeKm float64 `json:"maxRangeKm"`
}

// UplinkTuning holds the global tunables of the AVF/1 uplink client
// (internal/uplink; DESIGN §5.2, §20.3). They apply to every uplink.
type UplinkTuning struct {
	// RingSec is the span of the in-memory ring of unacknowledged data
	// (default 600, 60..3600 s).
	RingSec int `json:"ringSec"`
	// RingMB is the size of that ring (default 12, 1..512 MiB).
	RingMB int `json:"ringMb"`
	// BackfillQueueMB is the in-memory backfill queue (default 24, 1..1024 MiB).
	BackfillQueueMB int `json:"backfillQueueMb"`
	// ResumeWindowSec: unacknowledged data younger than this is resent live
	// after a reconnect, older data as backfill (default 30, 5..300 s).
	ResumeWindowSec int `json:"resumeWindowSec"`
	// BackfillMinKBps is the backfill rate floor (default 64, 1..100000 KiB/s).
	BackfillMinKBps int `json:"backfillMinKBps"`
	// BackoffMinSec is the first reconnect delay (default 1, 1..60 s).
	BackoffMinSec int `json:"backoffMinSec"`
	// BackoffMaxSec is the longest reconnect delay (default 60, 5..3600 s).
	BackoffMaxSec int `json:"backoffMaxSec"`
	// LivenessSec reconnects after this long without PING or ACK (default
	// 30, 5..600 s).
	LivenessSec int `json:"livenessSec"`
	// SpoolSegmentMB is the size of one disk spool segment (default 4,
	// 1..64 MiB).
	SpoolSegmentMB int `json:"spoolSegmentMb"`
}

// DefaultUplinkTuning returns the uplink tunables' defaults.
func DefaultUplinkTuning() UplinkTuning {
	return UplinkTuning{
		RingSec: 600, RingMB: 12, BackfillQueueMB: 24, ResumeWindowSec: 30,
		BackfillMinKBps: 64, BackoffMinSec: 1, BackoffMaxSec: 60,
		LivenessSec: 30, SpoolSegmentMB: 4,
	}
}

// UnmarshalJSON decodes the block on top of DefaultUplinkTuning.
func (t *UplinkTuning) UnmarshalJSON(b []byte) error {
	type plain UplinkTuning
	p := plain(DefaultUplinkTuning())
	if err := json.Unmarshal(b, &p); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return err
		}
	}
	*t = UplinkTuning(p)
	return nil
}

// Normalize clamps the tunables to their ranges and returns notes. A zero
// block is replaced by the defaults without a note. prefix is the JSON path
// of the block ("uplinkTuning").
func (t *UplinkTuning) Normalize(prefix string) []string {
	if *t == (UplinkTuning{}) {
		*t = DefaultUplinkTuning()
		return nil
	}
	var n notes
	d := DefaultUplinkTuning()
	t.RingSec = intRange(&n, prefix+".ringSec", t.RingSec, 60, 3600, d.RingSec)
	t.RingMB = intRange(&n, prefix+".ringMb", t.RingMB, 1, 512, d.RingMB)
	t.BackfillQueueMB = intRange(&n, prefix+".backfillQueueMb", t.BackfillQueueMB, 1, 1024, d.BackfillQueueMB)
	t.ResumeWindowSec = intRange(&n, prefix+".resumeWindowSec", t.ResumeWindowSec, 5, 300, d.ResumeWindowSec)
	t.BackfillMinKBps = intRange(&n, prefix+".backfillMinKBps", t.BackfillMinKBps, 1, 100000, d.BackfillMinKBps)
	t.BackoffMinSec = intRange(&n, prefix+".backoffMinSec", t.BackoffMinSec, 1, 60, d.BackoffMinSec)
	t.BackoffMaxSec = intRange(&n, prefix+".backoffMaxSec", t.BackoffMaxSec, 5, 3600, d.BackoffMaxSec)
	if t.BackoffMaxSec < t.BackoffMinSec {
		n.addf("%s.backoffMaxSec %d is below backoffMinSec %d; raised", prefix, t.BackoffMaxSec, t.BackoffMinSec)
		t.BackoffMaxSec = t.BackoffMinSec
	}
	t.LivenessSec = intRange(&n, prefix+".livenessSec", t.LivenessSec, 5, 600, d.LivenessSec)
	t.SpoolSegmentMB = intRange(&n, prefix+".spoolSegmentMb", t.SpoolSegmentMB, 1, 64, d.SpoolSegmentMB)
	return n
}

// DefaultAggregator returns the aggregator defaults.
func DefaultAggregator() Aggregator {
	return Aggregator{
		Name:        "AirView Network",
		AVFPort:     DefaultAVFPort,
		BeastPort:   DefaultBeastInPort,
		MLATPort:    DefaultMLATCPort,
		Admission:   AdmissionInvite,
		PublicMap:   true,
		PublicStats: true,
		TarJSON:     true,
		DefaultPin:  PinHidden,
		MLAT: AggMLAT{
			Enabled: true, ReturnResults: true, Holdout: 0.10,
			AutoUseSurvey: true, AutoUseMaxSigmaM: 50,
			EpochMs: 500, MaxComponentStations: 200, SurveyIntervalH: 6,
			RefractivityN0: 315, RefractivityScaleHeightM: 7000,
		},
		Retention: AggRetention{
			ConnLogDays: 30, MinuteDays: 30, HourMonths: 13,
			OfflinePurgeDays: 90, AuditMonths: 24, EstimatesPerMethod: 5, BackupDays: 30,
		},
		Limits: AggLimits{
			MaxStations: 2000, SelectiveAbove: 100, PerIPStations: 8,
			MaxFPSPerStation: 6000, MaxKBpsPerStation: 200,
			MaxUnauthPerIP: 4, MaxConnsPerIP: 16, MaxConnsPerSubnet: 32,
			HandshakeTimeoutSec: 10, IngestFPS: 3_000_000, MaxAircraft: 30000,
			PublicRPS: 1, PublicBurst: 10, MaxPublicSSE: 2000, MaxHTTPConns: 20000, MaxHTTPConnsPerIP: 64,
		},
		Auth: AggAuth{
			OwnerSessionDays: 30, OwnerIdleDays: 14,
			AdminSessionHours: 12, AdminIdleMinutes: 120,
			Registration: RegistrationOpen, LoginFreeFailures: 5,
		},
		Beast: AggBeast{
			IdentifyWaitMs: 2000, IdleTimeoutSec: 180, PingIntervalSec: 5,
			SlowDownEverySec: 10, QueueBatches: 64, MaxLinks: 4096,
			OverLimitSeconds: 5, CoolDownSec: 300,
		},
		Lanes:   AggLanes{FlushSec: 10, SlowFlushSec: 300, EvictHours: 24, MaxRangeKm: 800},
		Storage: DefaultAggStorage(),
		Ingest:  DefaultAggIngest(),
	}
}

// UnmarshalJSON decodes the block on top of DefaultAggregator, so fields
// missing from the JSON keep their defaults. A field of the wrong JSON type
// keeps its default and does not abort decoding of the surrounding document.
func (a *Aggregator) UnmarshalJSON(b []byte) error {
	type plain Aggregator
	p := plain(DefaultAggregator())
	if err := json.Unmarshal(b, &p); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return err
		}
	}
	*a = Aggregator(p)
	return nil
}

// IsDefault reports whether a equals DefaultAggregator().
func (a Aggregator) IsDefault() bool { return a.Equal(DefaultAggregator()) }

// Equal reports whether two blocks are identical in every setting (nil and
// empty lists are equal).
func (a Aggregator) Equal(o Aggregator) bool {
	norm := func(x Aggregator) Aggregator {
		if len(x.TrustedProxies) == 0 {
			x.TrustedProxies = nil
		}
		if len(x.TrustedRelays) == 0 {
			x.TrustedRelays = nil
		}
		if len(x.Limits.AllowCIDRs) == 0 {
			x.Limits.AllowCIDRs = nil
		}
		if len(x.Federation.Peers) == 0 {
			x.Federation.Peers = nil
		}
		if len(x.Federation.Feeds) == 0 {
			x.Federation.Feeds = nil
		}
		return x
	}
	return reflect.DeepEqual(norm(a), norm(o))
}

// Clone returns a deep copy of a.
func (a Aggregator) Clone() Aggregator {
	a.TrustedProxies = slices.Clone(a.TrustedProxies)
	a.TrustedRelays = slices.Clone(a.TrustedRelays)
	a.Limits.AllowCIDRs = slices.Clone(a.Limits.AllowCIDRs)
	a.Federation = a.Federation.clone()
	return a
}

// Normalize corrects out-of-range values in place and returns one note per
// correction. A completely zero-valued block (for example decoded from a
// document without an "aggregator" key into a zero Config) is replaced by
// DefaultAggregator() without a note.
func (a *Aggregator) Normalize() []string {
	if reflect.ValueOf(*a).IsZero() {
		*a = DefaultAggregator()
		return nil
	}
	var n notes
	d := DefaultAggregator()
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		a.Name = d.Name
	}
	a.PublicHost = strings.TrimSpace(a.PublicHost)
	if u := strings.TrimSpace(a.PrivacyNoticeURL); u != "" && !validNoticeURL(u) {
		n.addf("aggregator.privacyNoticeUrl %q is not an http(s) URL or a path starting with /; cleared", u)
		a.PrivacyNoticeURL = ""
	} else {
		a.PrivacyNoticeURL = u
	}
	for _, p := range []struct {
		name string
		v    *int
	}{
		{"avfPort", &a.AVFPort}, {"beastPort", &a.BeastPort}, {"mlatPort", &a.MLATPort},
		{"sbsInLocalPort", &a.SBSInLocalPort}, {"beastOutPort", &a.BeastOutPort}, {"sbsOutPort", &a.SBSOutPort},
	} {
		if *p.v < 0 || *p.v > 65535 {
			n.addf("aggregator.%s %d is out of range 0..65535; the listener is off", p.name, *p.v)
			*p.v = 0
		}
	}
	ports := map[int]string{}
	for _, p := range []struct {
		name string
		v    *int
	}{
		{"avfPort", &a.AVFPort}, {"beastPort", &a.BeastPort}, {"mlatPort", &a.MLATPort},
		{"sbsInLocalPort", &a.SBSInLocalPort}, {"beastOutPort", &a.BeastOutPort}, {"sbsOutPort", &a.SBSOutPort},
	} {
		if *p.v == 0 {
			continue
		}
		if other, dup := ports[*p.v]; dup {
			n.addf("aggregator.%s %d is already used by %s; the listener is off", p.name, *p.v, other)
			*p.v = 0
			continue
		}
		ports[*p.v] = p.name
	}
	a.Admission = enumOr(&n, "aggregator.admission", a.Admission, d.Admission,
		AdmissionOpen, AdmissionApprove, AdmissionInvite)
	a.DefaultPin = enumOr(&n, "aggregator.defaultPin", a.DefaultPin, d.DefaultPin,
		PinHidden, PinRegion, PinCell01)
	if (a.TLSCert == "") != (a.TLSKey == "") {
		n.addf("aggregator: tlsCert and tlsKey must be set together; the self-signed certificate is used")
		a.TLSCert, a.TLSKey = "", ""
	}
	a.TrustedProxies = cidrList(&n, "aggregator.trustedProxies", a.TrustedProxies)
	a.Limits.AllowCIDRs = cidrList(&n, "aggregator.limits.allowCidrs", a.Limits.AllowCIDRs)
	var relays []string
	for _, r := range a.TrustedRelays {
		uPart, cPart, hasCIDR := strings.Cut(strings.TrimSpace(r), "@")
		u, ok := NormalizeUUID(uPart)
		if !ok {
			n.addf("aggregator.trustedRelays: %q is not a UUID (or uuid@cidr); ignored", r)
			continue
		}
		if hasCIDR {
			c := strings.TrimSpace(cPart)
			if p, err := netip.ParsePrefix(c); err == nil {
				c = p.Masked().String()
			} else if ip, err := netip.ParseAddr(c); err == nil {
				c = ip.Unmap().String()
			} else {
				n.addf("aggregator.trustedRelays: %q: %q is not an address or CIDR; ignored", r, cPart)
				continue
			}
			u += "@" + c
		}
		if !slices.Contains(relays, u) {
			relays = append(relays, u)
		}
	}
	a.TrustedRelays = relays

	m := &a.MLAT
	m.Holdout = floatRange(&n, "aggregator.mlat.holdout", m.Holdout, 0, 0.5, d.MLAT.Holdout)
	m.AutoUseMaxSigmaM = floatRange(&n, "aggregator.mlat.autoUseMaxSigmaM", m.AutoUseMaxSigmaM, 5, 500, d.MLAT.AutoUseMaxSigmaM)
	m.EpochMs = intRange(&n, "aggregator.mlat.epochMs", m.EpochMs, 100, 2000, d.MLAT.EpochMs)
	m.MaxComponentStations = intRange(&n, "aggregator.mlat.maxComponentStations", m.MaxComponentStations, 20, 2000, d.MLAT.MaxComponentStations)
	m.SurveyIntervalH = intRange(&n, "aggregator.mlat.surveyIntervalH", m.SurveyIntervalH, 1, 168, d.MLAT.SurveyIntervalH)
	if m.RefractivityN0 != 0 {
		m.RefractivityN0 = floatRange(&n, "aggregator.mlat.refractivityN0", m.RefractivityN0, 200, 450, d.MLAT.RefractivityN0)
	}
	m.RefractivityScaleHeightM = floatRange(&n, "aggregator.mlat.refractivityScaleHeightM", m.RefractivityScaleHeightM, 4000, 12000, d.MLAT.RefractivityScaleHeightM)
	m.GeoidFile = strings.TrimSpace(m.GeoidFile)

	au := &a.Auth
	au.OwnerSessionDays = intRange(&n, "aggregator.auth.ownerSessionDays", au.OwnerSessionDays, 1, 365, d.Auth.OwnerSessionDays)
	au.OwnerIdleDays = intRange(&n, "aggregator.auth.ownerIdleDays", au.OwnerIdleDays, 1, 365, d.Auth.OwnerIdleDays)
	au.AdminSessionHours = intRange(&n, "aggregator.auth.adminSessionHours", au.AdminSessionHours, 1, 168, d.Auth.AdminSessionHours)
	au.AdminIdleMinutes = intRange(&n, "aggregator.auth.adminIdleMinutes", au.AdminIdleMinutes, 5, 1440, d.Auth.AdminIdleMinutes)
	au.Registration = enumOr(&n, "aggregator.auth.registration", au.Registration, d.Auth.Registration,
		RegistrationOpen, RegistrationClosed)
	au.LoginFreeFailures = intRange(&n, "aggregator.auth.loginFreeFailures", au.LoginFreeFailures, 1, 100, d.Auth.LoginFreeFailures)

	r := &a.Retention
	r.ConnLogDays = intRange(&n, "aggregator.retention.connLogDays", r.ConnLogDays, 1, 365, d.Retention.ConnLogDays)
	r.MinuteDays = intRange(&n, "aggregator.retention.minuteDays", r.MinuteDays, 1, 365, d.Retention.MinuteDays)
	r.HourMonths = intRange(&n, "aggregator.retention.hourMonths", r.HourMonths, 1, 120, d.Retention.HourMonths)
	r.OfflinePurgeDays = intRange(&n, "aggregator.retention.offlinePurgeDays", r.OfflinePurgeDays, 7, 3650, d.Retention.OfflinePurgeDays)
	r.AuditMonths = intRange(&n, "aggregator.retention.auditMonths", r.AuditMonths, 1, 120, d.Retention.AuditMonths)
	r.EstimatesPerMethod = intRange(&n, "aggregator.retention.estimatesPerMethod", r.EstimatesPerMethod, 1, 100, d.Retention.EstimatesPerMethod)
	r.BackupDays = intRange(&n, "aggregator.retention.backupDays", r.BackupDays, 1, 3650, d.Retention.BackupDays)

	l := &a.Limits
	l.MaxStations = intRange(&n, "aggregator.limits.maxStations", l.MaxStations, 1, 100000, d.Limits.MaxStations)
	l.SelectiveAbove = intRange(&n, "aggregator.limits.selectiveAbove", l.SelectiveAbove, 0, 100000, d.Limits.SelectiveAbove)
	l.PerIPStations = intRange(&n, "aggregator.limits.perIpStations", l.PerIPStations, 1, 1000, d.Limits.PerIPStations)
	l.MaxFPSPerStation = intRange(&n, "aggregator.limits.maxFpsPerStation", l.MaxFPSPerStation, 100, 100000, d.Limits.MaxFPSPerStation)
	l.MaxKBpsPerStation = intRange(&n, "aggregator.limits.maxKBpsPerStation", l.MaxKBpsPerStation, 10, 10000, d.Limits.MaxKBpsPerStation)
	l.MaxUnauthPerIP = intRange(&n, "aggregator.limits.maxUnauthPerIp", l.MaxUnauthPerIP, 1, 1000, d.Limits.MaxUnauthPerIP)
	l.MaxConnsPerIP = intRange(&n, "aggregator.limits.maxConnsPerIp", l.MaxConnsPerIP, 1, 10000, d.Limits.MaxConnsPerIP)
	l.MaxConnsPerSubnet = intRange(&n, "aggregator.limits.maxConnsPerSubnet", l.MaxConnsPerSubnet, 1, 100000, d.Limits.MaxConnsPerSubnet)
	l.HandshakeTimeoutSec = intRange(&n, "aggregator.limits.handshakeTimeoutSec", l.HandshakeTimeoutSec, 2, 120, d.Limits.HandshakeTimeoutSec)
	l.IngestFPS = intRange(&n, "aggregator.limits.ingestFps", l.IngestFPS, 10_000, 100_000_000, d.Limits.IngestFPS)
	l.MaxAircraft = intRange(&n, "aggregator.limits.maxAircraft", l.MaxAircraft, 1000, 1_000_000, d.Limits.MaxAircraft)
	l.TrackerShards = intRange(&n, "aggregator.limits.trackerShards", l.TrackerShards, 0, 256, d.Limits.TrackerShards)
	l.PublicRPS = floatRange(&n, "aggregator.limits.publicRps", l.PublicRPS, 0.1, 1000, d.Limits.PublicRPS)
	l.PublicBurst = intRange(&n, "aggregator.limits.publicBurst", l.PublicBurst, 1, 10000, d.Limits.PublicBurst)
	l.MaxPublicSSE = intRange(&n, "aggregator.limits.maxPublicSse", l.MaxPublicSSE, 1, 100000, d.Limits.MaxPublicSSE)
	l.MaxHTTPConns = intRange(&n, "aggregator.limits.maxHttpConns", l.MaxHTTPConns, 100, 1000000, d.Limits.MaxHTTPConns)
	l.MaxHTTPConnsPerIP = intRange(&n, "aggregator.limits.maxHttpConnsPerIp", l.MaxHTTPConnsPerIP, 1, 100000, d.Limits.MaxHTTPConnsPerIP)
	if l.MaxConnsPerIP < l.MaxUnauthPerIP {
		n.addf("aggregator.limits.maxConnsPerIp %d is below maxUnauthPerIp %d; raised", l.MaxConnsPerIP, l.MaxUnauthPerIP)
		l.MaxConnsPerIP = l.MaxUnauthPerIP
	}

	// Blocks added after Wave 1A: a zero block (an Aggregator built in code
	// before they existed) gets the defaults.
	if a.Beast == (AggBeast{}) {
		a.Beast = d.Beast
	}
	b := &a.Beast
	b.IdentifyWaitMs = intRange(&n, "aggregator.beast.identifyWaitMs", b.IdentifyWaitMs, 100, 60000, d.Beast.IdentifyWaitMs)
	b.IdleTimeoutSec = intRange(&n, "aggregator.beast.idleTimeoutSec", b.IdleTimeoutSec, 10, 3600, d.Beast.IdleTimeoutSec)
	b.PingIntervalSec = intRange(&n, "aggregator.beast.pingIntervalSec", b.PingIntervalSec, 1, 300, d.Beast.PingIntervalSec)
	b.SlowDownEverySec = intRange(&n, "aggregator.beast.slowDownEverySec", b.SlowDownEverySec, 1, 600, d.Beast.SlowDownEverySec)
	b.QueueBatches = intRange(&n, "aggregator.beast.queueBatches", b.QueueBatches, 4, 65536, d.Beast.QueueBatches)
	b.MaxLinks = intRange(&n, "aggregator.beast.maxLinks", b.MaxLinks, 1, 100000, d.Beast.MaxLinks)
	b.OverLimitSeconds = intRange(&n, "aggregator.beast.overLimitSeconds", b.OverLimitSeconds, 1, 300, d.Beast.OverLimitSeconds)
	b.CoolDownSec = intRange(&n, "aggregator.beast.coolDownSec", b.CoolDownSec, 0, 86400, d.Beast.CoolDownSec)

	if a.Lanes == (AggLanes{}) {
		a.Lanes = d.Lanes
	}
	ln := &a.Lanes
	ln.FlushSec = intRange(&n, "aggregator.lanes.flushSec", ln.FlushSec, 2, 60, d.Lanes.FlushSec)
	ln.SlowFlushSec = intRange(&n, "aggregator.lanes.slowFlushSec", ln.SlowFlushSec, 60, 3600, d.Lanes.SlowFlushSec)
	ln.EvictHours = intRange(&n, "aggregator.lanes.evictHours", ln.EvictHours, 1, 720, d.Lanes.EvictHours)
	ln.MaxRangeKm = floatRange(&n, "aggregator.lanes.maxRangeKm", ln.MaxRangeKm, 100, 1500, d.Lanes.MaxRangeKm)

	// Blocks added after Wave 1B.
	a.ListenAddr = strings.TrimSpace(a.ListenAddr)
	if a.ListenAddr != "" {
		h := strings.TrimSuffix(strings.TrimPrefix(a.ListenAddr, "["), "]")
		if _, err := netip.ParseAddr(h); err != nil && h != "localhost" {
			n.addf("aggregator.listenAddr %q is not an IP address; every interface is used", a.ListenAddr)
			h = ""
		}
		a.ListenAddr = h
	}
	if a.Storage == (AggStorage{}) {
		a.Storage = d.Storage
	}
	sg, ds := &a.Storage, d.Storage
	if sg.MinuteDays != 0 {
		sg.MinuteDays = intRange(&n, "aggregator.storage.minuteDays", sg.MinuteDays, 1, 3650, 0)
	}
	if sg.HourDays != 0 {
		sg.HourDays = intRange(&n, "aggregator.storage.hourDays", sg.HourDays, 7, 3650, 0)
	}
	sg.DayDays = intRange(&n, "aggregator.storage.dayDays", sg.DayDays, 0, 36500, ds.DayDays)
	sg.HistDailyDays = intRange(&n, "aggregator.storage.histDailyDays", sg.HistDailyDays, 1, 3650, ds.HistDailyDays)
	sg.HistDays = intRange(&n, "aggregator.storage.histDays", sg.HistDays, 0, 36500, ds.HistDays)
	sg.HeatIdleDays = intRange(&n, "aggregator.storage.heatIdleDays", sg.HeatIdleDays, 0, 36500, ds.HeatIdleDays)
	sg.FlightStationDays = intRange(&n, "aggregator.storage.flightStationDays", sg.FlightStationDays, 0, 3650, ds.FlightStationDays)
	sg.TrailDays = intRange(&n, "aggregator.storage.trailDays", sg.TrailDays, 1, 3650, ds.TrailDays)
	sg.TrailFullDays = intRange(&n, "aggregator.storage.trailFullDays", sg.TrailFullDays, 0, 3650, ds.TrailFullDays)
	if sg.TrailThinSec != 0 {
		sg.TrailThinSec = intRange(&n, "aggregator.storage.trailThinSec", sg.TrailThinSec, 2, 3600, ds.TrailThinSec)
	}
	sg.FreePct = intRange(&n, "aggregator.storage.freePct", sg.FreePct, 1, 90, ds.FreePct)
	sg.WALCheckpointMB = intRange(&n, "aggregator.storage.walCheckpointMb", sg.WALCheckpointMB, 4, 4096, ds.WALCheckpointMB)
	sg.OptimizeHours = intRange(&n, "aggregator.storage.optimizeHours", sg.OptimizeHours, 1, 720, ds.OptimizeHours)
	sg.QueueRows = intRange(&n, "aggregator.storage.queueRows", sg.QueueRows, 10_000, 50_000_000, ds.QueueRows)

	if a.Ingest == (AggIngest{}) {
		a.Ingest = d.Ingest
	}
	ig, di := &a.Ingest, d.Ingest
	ig.WindowMs = intRange(&n, "aggregator.ingest.windowMs", ig.WindowMs, 200, 5000, di.WindowMs)
	ig.FlushAfterMs = intRange(&n, "aggregator.ingest.flushAfterMs", ig.FlushAfterMs, 100, ig.WindowMs, min(di.FlushAfterMs, ig.WindowMs))
	ig.RetainMs = intRange(&n, "aggregator.ingest.retainMs", ig.RetainMs, 100, 10000, di.RetainMs)
	ig.MaxMatchMs = intRange(&n, "aggregator.ingest.maxMatchMs", ig.MaxMatchMs, 20, 700, di.MaxMatchMs)
	ig.MinMatchMs = intRange(&n, "aggregator.ingest.minMatchMs", ig.MinMatchMs, 1, ig.MaxMatchMs, min(di.MinMatchMs, ig.MaxMatchMs))
	ig.UniqueSkewMs = intRange(&n, "aggregator.ingest.uniqueSkewMs", ig.UniqueSkewMs, 100, 10000, di.UniqueSkewMs)
	if ig.LateAfterMs != 0 {
		ig.LateAfterMs = intRange(&n, "aggregator.ingest.lateAfterMs", ig.LateAfterMs, 500, 60000, 0)
	}
	ig.Shards = intRange(&n, "aggregator.ingest.shards", ig.Shards, 1, 4096, di.Shards)
	ig.ReduceMs = intRange(&n, "aggregator.ingest.reduceMs", ig.ReduceMs, 100, 5000, di.ReduceMs)
	ig.FlowHoldSec = intRange(&n, "aggregator.ingest.flowHoldSec", ig.FlowHoldSec, 1, 60, di.FlowHoldSec)
	ig.FlowGraceSec = intRange(&n, "aggregator.ingest.flowGraceSec", ig.FlowGraceSec, 1, 300, di.FlowGraceSec)
	ig.AVFQueueRecords = intRange(&n, "aggregator.ingest.avfQueueRecords", ig.AVFQueueRecords, 4, 4096, di.AVFQueueRecords)
	ig.AVFResumeGraceSec = intRange(&n, "aggregator.ingest.avfResumeGraceSec", ig.AVFResumeGraceSec, 1, 300, di.AVFResumeGraceSec)
	ig.AVFMaxSessions = intRange(&n, "aggregator.ingest.avfMaxSessions", ig.AVFMaxSessions, 1, 100000, di.AVFMaxSessions)

	// Blocks added after Wave 2A (zero = package default).
	a.normalizeMLATTuning(&n)
	a.normalizeHardenTuning(&n)
	a.normalizeSurveyTuning(&n)
	return n
}

// FeederReceiver is one receiver (one timestamp clock) attached to
// airview-feeder. Each becomes its own station at the aggregator.
type FeederReceiver struct {
	// RxNo is the receiver number announced in AVF RECEIVER records
	// (1..255, unique within the feeder).
	RxNo int `json:"rxNo"`
	// Name is the station's display name ([A-Za-z0-9_-]{1,32} recommended).
	Name string `json:"name"`
	// Input is where the receiver's data is read. Input.Type "auto" probes
	// 127.0.0.1:30005 (readsb/dump1090) and a Radarcape on :10003.
	Input Feed `json:"input"`
	// ClockClass overrides the detected clock class (ClockClass.String()
	// names, "" = detect).
	ClockClass string `json:"clockClass,omitempty"`
	// Located is true when Lat/Lon/AltM are a declared position.
	Located bool    `json:"located"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	// AltM is the antenna altitude in metres, referenced by AltRef.
	AltM float64 `json:"altM"`
	// AltRef is AltRefMSL (default) or AltRefHAE.
	AltRef string `json:"altRef"`
	// AGLM is the antenna height above ground in metres (survey height prior).
	AGLM float64 `json:"aglM"`
}

// FeederUI configures airview-feeder's local status page.
type FeederUI struct {
	// Addr is the listen address (default "127.0.0.1:8099"). A non-loopback
	// address requires PasswordHash.
	Addr string `json:"addr"`
	// PasswordHash protects the page on non-loopback addresses (same format
	// as HTTPConfig.AdminPasswordHash). A secret.
	PasswordHash string `json:"passwordHash"`
}

// FeederResults configures where airview-feeder delivers MLAT results
// received from the aggregator. Results are never re-uploaded.
type FeederResults struct {
	// BeastPort is a local Beast listener carrying results with the MLAT
	// magic timestamp (default 30105, 0 = off).
	BeastPort int `json:"beastPort"`
	// ReadsbAddr, when set, is a readsb Beast input (typically
	// "127.0.0.1:30104") the feeder connects to and pushes results into.
	ReadsbAddr string `json:"readsbAddr"`
	// BindAddr is the address of the results Beast listener (default
	// "127.0.0.1"; "" = loopback). Another address lets a decoder elsewhere
	// on the LAN connect.
	BindAddr string `json:"bindAddr"`
}

// FeederConfig is the configuration file of the headless airview-feeder
// program (feeder.json; Mode is always ModeFeeder).
type FeederConfig struct {
	Version   int              `json:"version"`   // ConfigVersion
	Mode      string           `json:"mode"`      // always ModeFeeder
	Receivers []FeederReceiver `json:"receivers"` // attached receivers, one station each
	Uplinks   []Uplink         `json:"uplinks"`   // aggregators to feed
	// UplinkTuning holds the global uplink client tunables.
	UplinkTuning UplinkTuning  `json:"uplinkTuning"`
	NTP          NTPConfig     `json:"ntp"`     // host clock reference for host timestamps
	UI           FeederUI      `json:"ui"`      // local status page
	Results      FeederResults `json:"results"` // local delivery of returned MLAT results
	// AutoUpdate installs signed updates published by the aggregator
	// (default true).
	AutoUpdate bool `json:"autoUpdate"`
	// Update is where and how signed updates are fetched (manifest URL and
	// release keys, written by the installers); autoUpdate switches the
	// automatic installs on when both are set.
	Update UpdateSettings `json:"update"`
}

// DefaultFeederConfig returns the feeder defaults: one auto-detected
// receiver and no uplink (the installer adds one).
func DefaultFeederConfig() FeederConfig {
	return FeederConfig{
		Version: ConfigVersion,
		Mode:    ModeFeeder,
		Receivers: []FeederReceiver{{
			RxNo: 1, Name: "receiver",
			Input:  Feed{ID: "input-1", Name: "Auto-detected receiver", Enabled: true, Type: FeedAuto},
			AltRef: AltRefMSL,
		}},
		UplinkTuning: DefaultUplinkTuning(),
		NTP:          NTPConfig{IntervalSec: 64},
		UI:           FeederUI{Addr: DefaultFeederUIAddr},
		Results:      FeederResults{BeastPort: DefaultResultsPort, BindAddr: "127.0.0.1"},
		AutoUpdate:   true,
		Update:       DefaultUpdateSettings(),
	}
}

// Normalize corrects the feeder configuration in place and returns notes.
func (f *FeederConfig) Normalize() []string {
	var n notes
	f.Version = ConfigVersion
	f.Mode = ModeFeeder
	seen := map[int]bool{}
	for i := range f.Receivers {
		r := &f.Receivers[i]
		path := fmt.Sprintf("receivers[%d]", i)
		if r.RxNo < 1 || r.RxNo > 255 || seen[r.RxNo] {
			k := 1
			for seen[k] {
				k++
			}
			n.addf("%s.rxNo %d is invalid or duplicate; %d is used", path, r.RxNo, k)
			r.RxNo = k
		}
		seen[r.RxNo] = true
		if r.ClockClass != "" {
			if _, ok := ParseClockClass(r.ClockClass); !ok {
				n.addf("%s.clockClass %q is unknown; detection is used", path, r.ClockClass)
				r.ClockClass = ""
			}
		}
		r.AltRef = enumOr(&n, path+".altRef", r.AltRef, AltRefMSL, AltRefMSL, AltRefHAE)
		if r.Located && (r.Lat < -90 || r.Lat > 90 || r.Lon < -180 || r.Lon > 180) {
			n.addf("%s: position %.5f,%.5f is out of range; it was cleared", path, r.Lat, r.Lon)
			r.Located, r.Lat, r.Lon = false, 0, 0
		}
		if r.AGLM < 0 || r.AGLM > 1000 {
			n.addf("%s.aglM %.1f is out of range 0..1000; 10 is used", path, r.AGLM)
			r.AGLM = 10
		}
	}
	n = append(n, NormalizeUplinks(f.Uplinks)...)
	n = append(n, f.UplinkTuning.Normalize("uplinkTuning")...)
	n = append(n, f.Update.Normalize("update")...)
	if f.UI.Addr == "" {
		f.UI.Addr = DefaultFeederUIAddr
	}
	if host, _, err := net.SplitHostPort(f.UI.Addr); err != nil {
		n.addf("ui.addr %q is not host:port; %s is used", f.UI.Addr, DefaultFeederUIAddr)
		f.UI.Addr = DefaultFeederUIAddr
	} else if ip, perr := netip.ParseAddr(host); (perr != nil || !ip.IsLoopback()) && host != "localhost" && f.UI.PasswordHash == "" {
		n.addf("ui.addr %q is reachable from the network but no password is set; %s is used", f.UI.Addr, DefaultFeederUIAddr)
		f.UI.Addr = DefaultFeederUIAddr
	}
	f.Results.BeastPort = intRange(&n, "results.beastPort", f.Results.BeastPort, 0, 65535, DefaultResultsPort)
	f.Results.BindAddr = strings.Trim(strings.TrimSpace(f.Results.BindAddr), "[]")
	if f.Results.BindAddr == "" {
		f.Results.BindAddr = "127.0.0.1"
	} else if _, err := netip.ParseAddr(f.Results.BindAddr); err != nil && f.Results.BindAddr != "localhost" {
		n.addf("results.bindAddr %q is not an IP address; 127.0.0.1 is used", f.Results.BindAddr)
		f.Results.BindAddr = "127.0.0.1"
	}
	return n
}

// --- helpers ------------------------------------------------------------------

// notes collects human readable correction messages.
type notes []string

func (n *notes) addf(format string, args ...any) { *n = append(*n, fmt.Sprintf(format, args...)) }

func intRange(n *notes, name string, v, lo, hi, def int) int {
	if v < lo || v > hi {
		n.addf("%s %d is out of range %d..%d; %d is used", name, v, lo, hi, def)
		return def
	}
	return v
}

func floatRange(n *notes, name string, v, lo, hi, def float64) float64 {
	if v != v || v < lo || v > hi {
		n.addf("%s %g is out of range %g..%g; %g is used", name, v, lo, hi, def)
		return def
	}
	return v
}

func enumOr(n *notes, name, v, def string, allowed ...string) string {
	if slices.Contains(allowed, v) {
		return v
	}
	n.addf("%s %q is not one of %s; %q is used", name, v, strings.Join(allowed, "/"), def)
	return def
}

// cidrList keeps valid CIDRs (a bare address becomes a /32 or /128), in
// canonical form and without duplicates.
func cidrList(n *notes, name string, in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				n.addf("%s: %q is not a CIDR or IP address; ignored", name, s)
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		c := p.Masked().String()
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// validNoticeURL accepts an absolute http(s) URL with a host, or a path on
// this server ("/privacy.html"); no spaces, quotes or angle brackets (the
// value ends up in an href).
func validNoticeURL(u string) bool {
	if len(u) > 512 || strings.ContainsAny(u, " \t\r\n\"'<>\\`") {
		return false
	}
	switch {
	case strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//"):
		return true
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		rest := u[strings.Index(u, "//")+2:]
		host := rest
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			host = rest[:i]
		}
		return host != "" && !strings.Contains(host, "@")
	}
	return false
}
