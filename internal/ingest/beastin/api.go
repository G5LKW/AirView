package beastin

import (
	"context"
	"errors"
	"net/netip"
	"strconv"

	"airview/internal/feed"
	"airview/internal/model"
)

// Identity is what a Beast link tells the server about who it is. It is
// handed to Registry.Attach.
type Identity struct {
	// Link is the server-local link number (unique per Server).
	Link uint64
	// Kind is always model.StationKindBeast.
	Kind string
	// UUID is the normalised feeder UUID from the 0xE4 escape, "" for an
	// unidentified link.
	UUID string
	// IPKey identifies the link's address without revealing it:
	// "ip:" + base32(HMAC-SHA256(serverKey, ip ‖ listen port)[:10]). It is
	// always set; for an unidentified link it is the station key, and the
	// registry may also use it to join a UUID-less link to a station seen
	// on the same address (DESIGN §3 link_guess).
	IPKey string
	// Slot separates concurrent unidentified links from one address: the
	// first gets slot 0, a second concurrent one slot 1, and so on (the
	// lowest free slot). A feeder that reconnects alone gets slot 0 again,
	// so its station's statistics continue ("merge by IP", DESIGN §3), while
	// two UUID-less receivers behind one NAT stay two stations. It is -1
	// for identities with a UUID.
	Slot int
	// RemoteIP is the peer address. It is private data: the registry may
	// use it for claims and Feed Check (DESIGN §7) but beastin never logs it.
	RemoteIP netip.Addr
	// Relay is set for a sub-station of a trusted relay (DESIGN §3): UUID is
	// the relay's UUID and ReceiverID the 0xE3 receiver ID.
	Relay      bool
	ReceiverID uint64
}

// Unidentified reports whether the identity carries no UUID.
func (id Identity) Unidentified() bool { return id.UUID == "" }

// Key returns the identity's stable key: "uuid:<uuid>", "uuid:<uuid>/rx:<hex>"
// for a relay sub-station, or for an unidentified link the IPKey (slot 0)
// or "<IPKey>/<slot>". It is the key of cool-downs and of the per-IP
// station count.
func (id Identity) Key() string {
	switch {
	case id.UUID == "" && id.Slot > 0:
		return id.IPKey + "/" + strconv.Itoa(id.Slot)
	case id.UUID == "":
		return id.IPKey
	case id.Relay:
		return "uuid:" + id.UUID + "/rx:" + hex64(id.ReceiverID)
	}
	return "uuid:" + id.UUID
}

// Attachment is the registry's answer to Attach.
type Attachment struct {
	// Station is the station the frames are credited to (never 0 when the
	// attachment is admitted).
	Station model.StationIdx
	// FeedID is stamped on the frames (aggregator form "st/<sid>/<rx>").
	FeedID string
	// Admission is model.AdmApproved, model.AdmPending (frames still flow;
	// the ingest keeps them in the station's own lane), or model.AdmDenied /
	// model.AdmRevoked (the link is closed and the identity cooled down).
	Admission string
	// Reason is a short operator-facing text for a refusal.
	Reason string
	// EpochBase continues the station's clock-epoch numbering from an
	// earlier connection (0 = start at 1).
	EpochBase uint32
	// Located reports whether the station has a usable position (informative).
	Located bool
}

// Admitted reports whether frames may flow.
func (a Attachment) Admitted() bool {
	return a.Station != 0 && (a.Admission == model.AdmApproved || a.Admission == model.AdmPending)
}

// Registry maps link identities to stations. The engine implements it over
// internal/registry; MemRegistry is an in-memory reference implementation.
// Attach and Detach are called from link goroutines and must be safe for
// concurrent use. Attach may block briefly (database access).
type Registry interface {
	// Attach registers that a link is now delivering frames for id. An error
	// refuses the link (it is closed and the identity cooled down).
	Attach(ctx context.Context, id Identity) (Attachment, error)
	// Detach reports that the link no longer delivers frames for the
	// station. It is called exactly once per Attach that returned no error
	// and an admitted Attachment (Attachment.Admitted), after the link's
	// last frames for that station were handed to the Sink.
	Detach(id Identity, a Attachment)
}

// ErrRefused is a generic refusal a Registry can return.
var ErrRefused = errors.New("beastin: refused by registry")

// ClockEvent is a receiver clock epoch change seen on a link (one per
// station: each trusted-relay sub-station has its own clock).
type ClockEvent struct {
	// At is the index in Batch.Frames of the first frame of the new epoch:
	// the event happened after Frames[:At] and before Frames[At:].
	At      int
	Station model.StationIdx
	Event   feed.ClockEvent
}

// Batch is a run of frames from one link in arrival order.
type Batch struct {
	// Link is the link number (see Identity.Link).
	Link uint64
	// MapStatsOnly is always true: 30004 data feeds the map, lanes and
	// statistics only; never clock sync, MLAT or survey (DESIGN §5.1).
	MapStatsOnly bool
	// Frames have Station and FeedID set. Their Data slices are owned by the
	// frame (they may be retained); the Frames slice itself is reused after
	// HandleBeast returns.
	Frames []model.RawFrame
	// Events are the clock epoch changes inside this batch.
	Events []ClockEvent

	final bool
	l     *link
	after []func() // run by the worker after delivery (deferred detaches)
}

// Sink receives the frames. HandleBeast is called from the delivery pool:
// batches of one link arrive in order and never concurrently, different
// links concurrently. It must not retain b after returning.
type Sink interface {
	HandleBeast(b *Batch)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(b *Batch)

// HandleBeast implements Sink.
func (f SinkFunc) HandleBeast(b *Batch) { f(b) }

// FrameSinkAdapter delivers batches frame by frame into a model.FrameSink
// (the tracker, or the engine's sink tee) until the station-aware ingest
// (internal/ingest) is wired. Clock events are dropped.
type FrameSinkAdapter struct{ Sink model.FrameSink }

// HandleBeast implements Sink.
func (a FrameSinkAdapter) HandleBeast(b *Batch) {
	for i := range b.Frames {
		a.Sink.HandleFrame(b.Frames[i])
	}
}

// Budget is an optional global ingest budget (DESIGN §4.6 "global ingest
// semaphore", owned by internal/ingest). Take reports whether n more frames
// may be accepted now; a refused batch is dropped and counted.
type Budget interface {
	Take(n int) bool
}

func hex64(v uint64) string {
	const digits = "0123456789abcdef"
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = digits[v&15]
		v >>= 4
	}
	return string(b[:])
}
