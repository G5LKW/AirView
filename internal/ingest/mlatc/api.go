package mlatc

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"time"

	"airview/internal/ingest"
	"airview/internal/model"
)

// Identity is what an mlat-client tells the server about itself in its
// handshake. It is handed to Registry.Attach.
type Identity struct {
	// Link is the server-local connection number.
	Link uint64
	// Kind is always model.StationKindMLATC.
	Kind string
	// UUID is the normalised client UUID (model.NormalizeUUID), "" when the
	// client sent none or an invalid one.
	UUID string
	// IPKey identifies the client's address without revealing it:
	// "ip:" + base32(HMAC-SHA256(serverKey, ip16 ‖ listen port)[:10]).
	// It is the station key of an unidentified client.
	IPKey string
	// Slot separates concurrent UUID-less clients behind one address (0 for
	// the first, then the lowest free number); -1 with a UUID.
	Slot int
	// RemoteIP is private (claims, Feed Check); never logged.
	RemoteIP netip.Addr

	// User is the sanitised "user" field (display name suggestion).
	User string
	// ClientVersion is the sanitised "client_version" field.
	ClientVersion string
	// ClockType is the handshake clock_type ("dump1090" when absent) and
	// ClockCls its class (DESIGN §5.2 mapping).
	ClockType string
	ClockCls  model.ClockClass
	// Privacy is the client's privacy flag. PrivacyOff is set only when the
	// client explicitly sent "privacy": false (DESIGN §19.2: region pin and
	// name); a missing flag is neither.
	Privacy    bool
	PrivacyOff bool
	// Located reports a valid declared position: Lat/Lon in degrees and
	// Alt in metres. AltRef is always model.AltRefAmbiguous: mlat-client
	// does not say whether alt is above the geoid or the ellipsoid; the
	// survey resolves it (DESIGN §5.2).
	Located  bool
	Lat, Lon float64
	Alt      float64
	AltRef   string
}

// Key returns the identity's stable key: "uuid:<uuid>", or for a UUID-less
// client the IPKey (slot 0) or "<IPKey>/<slot>".
func (id Identity) Key() string {
	switch {
	case id.UUID != "":
		return "uuid:" + id.UUID
	case id.Slot > 0:
		return id.IPKey + "/" + strconv.Itoa(id.Slot)
	}
	return id.IPKey
}

// Attachment is the registry's answer to Attach.
type Attachment struct {
	// Station receives the client's observations (never 0 when admitted).
	Station model.StationIdx
	// FeedID is stamped on the frames ("st/<sid>/<rx>").
	FeedID string
	// Admission is model.AdmApproved or model.AdmPending (the link is kept;
	// the ingest holds a pending station in its own lane), or
	// model.AdmDenied / model.AdmRevoked (the client gets a deny).
	Admission string
	// Reason is a short, client-safe text for a refusal (sent in "deny").
	Reason string
	// EpochBase continues the station's clock-epoch numbering (0 = start at 1).
	EpochBase uint32
	// Timestamps false means the owner withholds timing: the frames still
	// reach the map but never MLAT (ingest.SetTimestamps).
	NoTimestamps bool
}

// Admitted reports whether the client may stay connected.
func (a Attachment) Admitted() bool {
	return a.Station != 0 && (a.Admission == model.AdmApproved || a.Admission == model.AdmPending)
}

// Registry maps client identities to stations; the engine implements it
// over internal/registry (Admit with Kind mlatc, UUID or IdentKey,
// PrivacyOff, name and declared position). Calls come from connection
// goroutines and must be safe for concurrent use.
type Registry interface {
	// Attach registers that a client now delivers observations for id.
	// An error refuses the client (deny + cool-down).
	Attach(ctx context.Context, id Identity) (Attachment, error)
	// Detach is called exactly once per admitted Attach, after the
	// client's last frames were handed to the Sink.
	Detach(id Identity, a Attachment)
}

// ErrRefused is a generic refusal a Registry can return.
var ErrRefused = errors.New("mlatc: refused by registry")

// Sink is where the server delivers observations: the station-aware ingest
// (FromIngest adapts *ingest.Ingest). Calls for one connection are
// sequential; different connections call concurrently.
type Sink interface {
	// Submit hands a batch of timed frames of one station (Station, Epoch,
	// ClockCls, Ticks, HostNs set; never map-only). Data stays valid only
	// until Submit returns.
	Submit(frames []model.RawFrame)
	// ClockEvent reports a new clock epoch of a station, before the first
	// frame of that epoch is submitted.
	ClockEvent(st model.StationIdx, ev ingest.ClockEvent)
	// SetTimestamps says whether the station's timing may be used.
	SetTimestamps(st model.StationIdx, ok bool)
	// OpenSession registers a link so the ingest can steer its traffic
	// (selective mode, interest sets) and count its rates. It may return
	// nil (no session support).
	OpenSession(info ingest.SessionInfo) SessionHandle
}

// SessionHandle is the part of *ingest.Session the server uses.
type SessionHandle interface {
	AddStation(st model.StationIdx) string
	RemoveStation(st model.StationIdx)
	AddBytes(n int)
	SetQueueFill(f float64)
	Close(keepOnline time.Duration)
}

// FromIngest adapts the station-aware ingest to Sink.
func FromIngest(ing *ingest.Ingest) Sink { return ingestSink{ing} }

type ingestSink struct{ ing *ingest.Ingest }

func (s ingestSink) Submit(f []model.RawFrame) { s.ing.Submit(f, false) }
func (s ingestSink) ClockEvent(st model.StationIdx, ev ingest.ClockEvent) {
	s.ing.ClockEvent(st, ev)
}
func (s ingestSink) SetTimestamps(st model.StationIdx, ok bool) { s.ing.SetTimestamps(st, ok) }
func (s ingestSink) OpenSession(info ingest.SessionInfo) SessionHandle {
	return s.ing.OpenSession(info)
}

// Hooks are optional callbacks for the MLAT planner, the registry and the
// station pages. They are called from connection goroutines, must be fast
// and must not call back into the Server synchronously.
type Hooks struct {
	// OnAttach / OnDetach bracket a client's life on a station.
	OnAttach func(st model.StationIdx, id Identity)
	OnDetach func(st model.StationIdx, reason string)
	// OnSeen / OnLost report the client's visible-aircraft set changes.
	// OnSeen gets only the addresses new to the client's seen set (within
	// MaxSeen); the slice is reused after the call returns.
	// (24-bit addresses; the slices are only valid during the call).
	OnSeen func(st model.StationIdx, addrs []uint32)
	OnLost func(st model.StationIdx, addrs []uint32)
	// OnRates reports a rate_report: ADS-B position rates per aircraft
	// (messages per second; valid only during the call).
	OnRates func(st model.StationIdx, rates []AircraftRate)
	// OnInput reports input_connected (true) / input_disconnected (false):
	// the client's link to its own receiver.
	OnInput func(st model.StationIdx, connected bool)
	// OnPosition reports a position_update the client sent (a Radarcape
	// GNSS position, metres for Alt, reference unknown).
	OnPosition func(st model.StationIdx, lat, lon, alt float64)
}

// AircraftRate is one entry of a rate_report.
type AircraftRate struct {
	Addr uint32
	Rate float32
}

// StatsSource supplies the per-station sync statistics sent to the client
// in "stats" messages (the clock filter implements it). ok false = nothing
// to report yet. Never a peer list (DESIGN §5.2).
type StatsSource interface {
	ClientStats(st model.StationIdx) (s ClientStats, ok bool)
}

// ClientStats is the content of a "stats" message.
type ClientStats struct {
	// PeerCount is the number of stations this station is synchronised with.
	PeerCount int
	// OutlierPercent is the share of its recent observations rejected as
	// outliers (0..100).
	OutlierPercent float64
	// BadSyncTimeout is how many seconds the station stays excluded after
	// bad synchronisation (0 = none).
	BadSyncTimeout float64
}

// Result is one MLAT result for delivery to a client (see SendResult).
type Result struct {
	model.MLATFix
	// Callsign and Squawk are optional ("" = unknown).
	Callsign string
	Squawk   string
	// NStations is the station count to report; 0 = derived from
	// MLATFix.NBucket (the bucket's lower bound).
	NStations int
}
