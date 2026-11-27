package uplink

import (
	"slices"
	"time"

	"airview/internal/avf"
)

// Connection states of Status.State.
const (
	StateIdle       = "idle"
	StateDisabled   = "disabled"
	StateConnecting = "connecting"
	StateHandshake  = "handshake"
	StateConnected  = "connected"
	StateBackoff    = "backoff"
	StateStopped    = "stopped" // refused permanently until the settings change (see ErrorCode)
)

// statusState is the mutable part of Status (guarded by Client.mu).
type statusState struct {
	state         string
	since         time.Time
	lastErr       string
	lastErrAt     time.Time
	errCode       string
	nextRetry     time.Time
	serverID      string
	serverName    string
	admission     string
	approval      string
	feederID      string
	sessionID     string
	mode          string
	mlat          bool
	returnResults bool
	motd          string
	features      []string
	connectedAt   time.Time
	ackSeq        uint64
	paused        bool
	slow          float64
	notices       []NoticeStatus
}

// NoticeStatus is a NOTICE received from the server.
type NoticeStatus struct {
	Code  string `json:"code"`
	Level string `json:"level,omitempty"`
	Text  string `json:"text,omitempty"`
	AtMs  int64  `json:"atMs"`
}

// Status is the state of one uplink for UIs (GET /api/uplinks, the feeder
// status page). It contains no secrets: no tokens, keys or claim codes.
type Status struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Disabled bool   `json:"disabled"`
	Pinned   bool   `json:"pinned"` // a server pin is configured

	State     string `json:"state"`   // idle | disabled | connecting | handshake | connected | backoff | stopped
	SinceMs   int64  `json:"sinceMs"` // when State was entered
	LastError string `json:"lastError,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"` // AVF error code of the last refusal / GOODBYE
	ErrorAtMs int64  `json:"errorAtMs,omitempty"`
	RetryAtMs int64  `json:"retryAtMs,omitempty"` // next attempt (backoff)

	FeederID      string   `json:"feederId"`
	ServerName    string   `json:"serverName,omitempty"`
	Admission     string   `json:"admission,omitempty"`
	Approval      string   `json:"approval,omitempty"` // approved | pending (last WELCOME / NOTICE)
	SessionID     string   `json:"sessionId,omitempty"`
	Mode          string   `json:"mode,omitempty"` // full | selective
	MLAT          bool     `json:"mlat"`
	ReturnResults bool     `json:"returnResults"`
	Motd          string   `json:"motd,omitempty"`
	Features      []string `json:"features,omitempty"`
	ConnectedMs   int64    `json:"connectedMs,omitempty"`
	Paused        bool     `json:"paused,omitempty"`     // FLOW pause in effect
	SlowFactor    float64  `json:"slowFactor,omitempty"` // FLOW slow factor (< 1 when slowed)
	Sessions      uint64   `json:"sessions"`

	Counters Counters       `json:"counters"`
	Buffer   BufferStatus   `json:"buffer"`
	Rx       []RxStatus     `json:"receivers"`
	Notices  []NoticeStatus `json:"notices,omitempty"`
}

// Counters are cumulative since the uplink started.
type Counters struct {
	FramesIn        uint64 `json:"framesIn"`     // frames accepted from local feeds
	FramesQueued    uint64 `json:"framesQueued"` // frames encoded into records
	FramesSent      uint64 `json:"framesSent"`   // frames written (live and backfill)
	FramesLate      uint64 `json:"framesLate"`   // of which backfill
	Untimed         uint64 `json:"untimed"`      // frames sent without a valid timestamp
	MLATRefused     uint64 `json:"mlatRefused"`  // MLAT-stamped frames / MLAT updates never forwarded
	Filtered        uint64 `json:"filtered"`     // frames held back by selective / FLOW reduction
	RecordsSent     uint64 `json:"recordsSent"`
	BytesWire       uint64 `json:"bytesWire"` // compressed bytes of the current session
	DecodedIn       uint64 `json:"decodedIn"`
	ResultsIn       uint64 `json:"resultsIn"`       // MLAT results received
	ResultsInjected uint64 `json:"resultsInjected"` // of which injected into the local tracker
	DroppedRecords  uint64 `json:"droppedRecords"`  // lost: buffers full
	DroppedBytes    uint64 `json:"droppedBytes"`
	SpilledRecords  uint64 `json:"spilledRecords"` // written to the disk spool
	SpoolDropBytes  int64  `json:"spoolDroppedBytes"`
	AckSeq          uint64 `json:"ackSeq"`
}

// BufferStatus describes the buffered data.
type BufferStatus struct {
	RingRecords     int     `json:"ringRecords"`
	RingBytes       int     `json:"ringBytes"`
	RingSpanS       float64 `json:"ringSpanS"`
	BackfillRecords int     `json:"backfillRecords"`
	BackfillBytes   int     `json:"backfillBytes"`
	SpoolMB         int     `json:"spoolMb"` // effective spool size (0 = RAM only)
	SpoolBytes      int64   `json:"spoolBytes"`
}

// RxStatus is one announced receiver.
type RxStatus struct {
	RxNo       int           `json:"rxNo"`
	FeedID     string        `json:"feedId"`
	ClockClass string        `json:"clockClass"`
	Epoch      uint32        `json:"epoch"`
	Mode       string        `json:"mode"` // full | selective
	Feedback   *avf.Feedback `json:"feedback,omitempty"`
}

// Status returns the uplink's state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := &c.stat
	s := Status{ID: c.cfg.ID, Host: c.cfg.Host, Port: c.cfg.Port, Disabled: c.cfg.Disabled, Pinned: c.cfg.ServerPin != "",
		State: st.state, SinceMs: ms(st.since), LastError: st.lastErr, ErrorCode: st.errCode, ErrorAtMs: ms(st.lastErrAt),
		RetryAtMs: ms(st.nextRetry), FeederID: c.id.FeederID, ServerName: st.serverName, Admission: st.admission,
		Approval: st.approval, Mode: st.mode, MLAT: st.mlat, ReturnResults: st.returnResults, Motd: st.motd,
		Features: slices.Clone(st.features), Paused: st.paused, Sessions: c.ctr.sessions.Load(),
		Notices: slices.Clone(st.notices)}
	if st.slow > 0 && st.slow < 1 {
		s.SlowFactor = st.slow
	}
	if st.state == StateConnected {
		s.SessionID = st.sessionID
		s.ConnectedMs = ms(st.connectedAt)
	}
	q := c.q
	s.Counters = Counters{FramesIn: c.ctr.framesIn.Load(), FramesQueued: c.ctr.framesQueued.Load(),
		FramesSent: c.ctr.framesSent.Load(), FramesLate: c.ctr.framesLate.Load(), Untimed: c.ctr.untimed.Load(),
		MLATRefused: c.ctr.mlatRefused.Load(), Filtered: c.ctr.filtered.Load(), RecordsSent: c.ctr.recordsSent.Load(),
		BytesWire: c.ctr.bytesWire.Load(), DecodedIn: c.ctr.decodedIn.Load(), ResultsIn: c.ctr.resultsIn.Load(),
		ResultsInjected: c.ctr.resultsInjected.Load(), DroppedRecords: q.dropped, DroppedBytes: q.droppedBytes,
		SpilledRecords: q.spilled, AckSeq: st.ackSeq}
	s.Buffer = BufferStatus{RingRecords: q.inflight.len() + q.live.len(), RingBytes: q.ringBytes(),
		BackfillRecords: q.back.len(), BackfillBytes: q.back.bytes, SpoolMB: c.spoolMB}
	if r := q.oldestRing(); r != nil && q.newest > r.hostNs {
		s.Buffer.RingSpanS = float64(q.newest-r.hostNs) / 1e9
	}
	if q.sp != nil {
		s.Buffer.SpoolBytes = q.sp.Bytes()
		s.Counters.SpoolDropBytes = q.sp.droppedBytes
	}
	nos := make([]int, 0, len(c.rxByNo))
	for n := range c.rxByNo {
		nos = append(nos, n)
	}
	slices.Sort(nos)
	for _, n := range nos {
		rx := c.rxByNo[n]
		mode := avf.ModeFull
		if rx.filt.selective {
			mode = avf.ModeSelective
		}
		s.Rx = append(s.Rx, RxStatus{RxNo: n, FeedID: rx.feedID, ClockClass: rx.class.String(), Epoch: rx.epoch,
			Mode: mode, Feedback: rx.feedback})
	}
	return s
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
