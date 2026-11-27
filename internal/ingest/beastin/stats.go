package beastin

import (
	"time"

	"airview/internal/model"
)

// LinkStation is one station delivered over a link.
type LinkStation struct {
	Station    model.StationIdx `json:"station"`
	FeedID     string           `json:"feedId"`
	Admission  string           `json:"admission"`
	Relay      bool             `json:"relay,omitempty"`      // a trusted relay's sub-station
	ReceiverID uint64           `json:"receiverId,omitempty"` // 0xE3 receiver ID of a sub-station
	Refused    bool             `json:"refused,omitempty"`    // relay receiver ID refused by the registry
	Frames     uint64           `json:"frames"`               // frames decoded for this station
	RateDrop   uint64           `json:"rateDropped"`          // frames shed by the frame limit (or refused)
	ClockClass string           `json:"clockClass"`
	Epoch      uint32           `json:"epoch,omitempty"`
}

// LinkStats is a snapshot of one link. Remote and UUID are private: admin
// scope only (DESIGN §8.2).
type LinkStats struct {
	Link        uint64    `json:"link"`
	Remote      string    `json:"remote"`
	IPKey       string    `json:"ipKey"`
	UUID        string    `json:"uuid,omitempty"`
	Attached    bool      `json:"attached"`
	Pending     bool      `json:"pending,omitempty"` // admission pending
	Relay       bool      `json:"relay,omitempty"`   // trusted relay (0xE3 honoured)
	Connected   time.Time `json:"connected"`
	LastData    time.Time `json:"lastData"`
	Closed      bool      `json:"closed,omitempty"`
	CloseReason string    `json:"closeReason,omitempty"`

	Bytes        uint64  `json:"bytes"`
	Frames       uint64  `json:"frames"`       // all Beast frames parsed
	BytesPerSec  float64 `json:"bytesPerSec"`  // average since connect
	FramesPerSec float64 `json:"framesPerSec"` // average since connect
	Delivered    uint64  `json:"delivered"`    // frames handed to the Sink
	QueueDepth   int     `json:"queueDepth"`
	QueueDropped uint64  `json:"queueDropped"`
	MLATDropped  uint64  `json:"mlatDropped"`
	RateDropped  uint64  `json:"rateDropped"`

	UUIDEscapes    uint64 `json:"uuidEscapes"`
	ReceiverIDs    uint64 `json:"receiverIds"`
	RelayIgnored   uint64 `json:"relayIgnored"` // 0xE3 ignored (link not a trusted relay)
	Options        uint64 `json:"options"`
	UnknownEscapes uint64 `json:"unknownEscapes"`
	Garbage        uint64 `json:"garbage"`   // bytes discarded while out of sync
	Truncated      uint64 `json:"truncated"` // frames cut short

	PingOn      bool    `json:"pingOn"` // the feeder asked for pings ('W' 'O')
	PingsSent   uint64  `json:"pingsSent"`
	PingsEchoed uint64  `json:"pingsEchoed"` // client pings echoed back
	SlowDowns   uint64  `json:"slowDowns"`   // 'W' 'S' sent
	RTTLastMs   float64 `json:"rttLastMs,omitempty"`
	RTTMinMs    float64 `json:"rttMinMs,omitempty"`
	RTTAvgMs    float64 `json:"rttAvgMs,omitempty"`

	Stations []LinkStation `json:"stations,omitempty"`
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

// publish copies the reader's state into the snapshot (reader goroutine).
func (l *link) publish() {
	p := l.p
	var rate, frate float64
	if dt := l.now.Sub(l.connected).Seconds(); dt > 0 {
		rate = float64(l.bytes) / dt
		frate = float64(p.Frames) / dt
	}
	stations := make([]LinkStation, 0, 1+len(l.subs))
	addSub := func(u *sub) {
		if !u.attached && !u.refused {
			return
		}
		ls := LinkStation{
			Station: u.att.Station, FeedID: u.att.FeedID, Admission: u.att.Admission,
			Relay: u.id.Relay, ReceiverID: u.id.ReceiverID, Refused: u.refused,
			Frames: u.frames, RateDrop: u.dropped,
			ClockClass: u.dec.TS.Class().String(), Epoch: u.dec.TS.Epoch(),
		}
		stations = append(stations, ls)
	}
	addSub(l.own)
	for _, u := range l.subs {
		addSub(u)
	}
	l.mu.Lock()
	st := &l.pub
	st.IPKey = l.ipKey
	st.UUID = l.uuid
	st.Attached = l.own.attached
	st.Pending = l.own.attached && l.own.att.Admission == model.AdmPending
	st.Relay = l.relay
	st.LastData = l.lastData
	st.Bytes = l.bytes
	st.Frames = p.Frames
	st.BytesPerSec = rate
	st.FramesPerSec = frate
	st.MLATDropped = l.mlatDrop
	rd := l.own.dropped
	for _, u := range l.subs {
		rd += u.dropped
	}
	st.RateDropped = rd
	st.UUIDEscapes = l.uuids
	st.ReceiverIDs = l.rxIDs
	st.RelayIgnored = l.relayIgn
	st.Options = l.options
	st.UnknownEscapes = p.Unknown
	st.Garbage = p.Garbage
	st.Truncated = p.Truncated
	st.PingOn = l.pingOn
	st.PingsSent = l.sent
	st.PingsEchoed = l.echoed
	st.SlowDowns = l.slowSent
	st.RTTLastMs, st.RTTMinMs, st.RTTAvgMs = ms(l.rttLast), ms(l.rttMin), ms(l.rttAvg)
	st.Stations = stations
	l.mu.Unlock()
}

// snapshot returns the published state plus the live queue counters.
func (l *link) snapshot() (LinkStats, bool) {
	l.mu.Lock()
	st := l.pub
	st.Stations = append([]LinkStation(nil), l.pub.Stations...)
	l.mu.Unlock()
	l.qmu.Lock()
	st.QueueDepth = len(l.q)
	st.Delivered = l.delivered
	st.QueueDropped = l.queueDrop
	l.qmu.Unlock()
	return st, true
}
