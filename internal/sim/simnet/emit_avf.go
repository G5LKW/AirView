package simnet

import (
	"math"
	"slices"

	"airview/internal/model"
)

// AVF/1 records (docs/aggregator/AVF.md §5) as plain values. simnet does not
// encode the wire format itself: an AVFWriter (the real AVF client encoder,
// or AVFRecorder in tests) receives these records in order and assigns
// sequence numbers.

// AVFLocation is the RECEIVER location object.
type AVFLocation struct {
	Lat, Lon, Alt float64
	AltRef        string // "hae" | "msl"
	Source        string // "manual"
	SigmaM        float64
}

// AVFReceiver is a RECEIVER (0x01) record.
type AVFReceiver struct {
	RxNo       int
	Name       string
	ClockClass model.ClockClass
	NominalHz  float64
	EpochID    uint32
	Channels   int
	Decoder    string
	Location   *AVFLocation
	Timestamps bool
}

// AVFFrame is one frame of a FRAMES record.
type AVFFrame struct {
	Kind      model.FrameKind
	Late      bool
	TsInvalid bool
	Channel   int // -1 = unknown
	Ticks     uint64
	HostNs    int64
	Sig       byte
	Payload   []byte
}

// AVFFrames is a FRAMES (0x02) record. Frames are sorted by Ticks.
type AVFFrames struct {
	RxNo       int
	EpochID    uint32
	BaseTicks  uint64
	BaseHostNs int64
	Frames     []AVFFrame
}

// AVFClockStatus is a CLOCK_STATUS (0x03) record.
type AVFClockStatus struct {
	RxNo           int
	HostNs         int64
	Settings       byte
	GPSStatus      byte
	PPSOffsetTicks int
	Raw            []byte
}

// AVFClockPair is a CLOCK_PAIR (0x04) record.
type AVFClockPair struct {
	RxNo         int
	EpochID      uint32
	Ticks        uint64
	HostUTCNs    int64
	HostErrEstNs uint64
}

// AVFClockEvent is a CLOCK_EVENT (0x05) record.
type AVFClockEvent struct {
	RxNo        int
	OldEpoch    uint32
	NewEpoch    uint32
	Reason      model.ClockEventReason
	SamplesLost int
	HostNs      int64
}

// AVFBackfill is a BACKFILL (0x0B) marker. FromBatch/ToBatch number the
// emitter's FRAMES records (the writer maps them to its seq numbers).
type AVFBackfill struct {
	Phase              string // "begin" | "end"
	FromBatch, ToBatch uint64
	SpanNs             int64
}

// AVFWriter receives AVF records in stream order. hostNs is the feeder host
// time the record is written at. Records passed by pointer are only valid
// during the call.
type AVFWriter interface {
	// Connect is called at the start of every session (first data and
	// after each reconnect), before any record of that session.
	Connect(hostNs int64, session int)
	Receiver(hostNs int64, r *AVFReceiver)
	Frames(hostNs int64, f *AVFFrames)
	ClockStatus(hostNs int64, s *AVFClockStatus)
	ClockPair(hostNs int64, p *AVFClockPair)
	ClockEvent(hostNs int64, e *AVFClockEvent)
	Backfill(hostNs int64, b *AVFBackfill)
	// Flush is called from Advance: everything up to untilHostNs has been
	// written.
	Flush(untilHostNs int64)
}

// AVFOptions configures an AVFEmitter.
type AVFOptions struct {
	BatchMs   float64 // FRAMES batching interval (0 = 50 ms)
	MaxFrames int     // frames per record (0 = 4096)
	// PairEveryS is the CLOCK_PAIR interval (0 = 5 s); StatusEveryS the
	// CLOCK_STATUS interval for Radarcape devices (0 = 1 s, negative =
	// none).
	PairEveryS   float64
	StatusEveryS float64
	// SendLocation includes the declared location in RECEIVER.
	SendLocation bool
	// Conn models the uplink connection (nil = always up). While it is down
	// FRAMES are held (up to HoldS, 0 = 600 s, the RAM ring) and replayed as
	// backfill with the late bit after the reconnect.
	Conn  ConnState
	HoldS float64
	// Unsorted disables sorting frames by ticks inside a record (to test
	// the server's rejection of a protocol violation).
	Unsorted bool
}

// AVFEmitter turns one receiver's observations into AVF/1 records.
type AVFEmitter struct {
	n        *Network
	rx       int
	w        AVFWriter
	opt      AVFOptions
	session  int
	connOK   bool
	batch    AVFFrames
	bStart   int64
	epoch    uint32
	class    model.ClockClass
	started  bool
	lastTk   uint64
	lastHost int64
	// lastFrameHost is the host time of the frame lastTk belongs to.
	lastFrameHost int64
	needRecv      bool
	needReplay    bool
	nextPair      float64
	nextStat      float64
	nBatch        uint64
	held          []AVFFrames
	heldFrom      uint64

	Batches, FramesOut, Late uint64
}

// NewAVFEmitter creates the emitter for receiver rx and attaches it.
func (n *Network) NewAVFEmitter(rx int, w AVFWriter, opt AVFOptions) *AVFEmitter {
	if !(opt.BatchMs > 0) {
		opt.BatchMs = 50
	}
	if opt.MaxFrames <= 0 || opt.MaxFrames > 4096 {
		opt.MaxFrames = 4096
	}
	if !(opt.PairEveryS > 0) {
		opt.PairEveryS = 5
	}
	if opt.StatusEveryS == 0 {
		opt.StatusEveryS = 1
	}
	if !(opt.HoldS > 0) {
		opt.HoldS = 600
	}
	e := &AVFEmitter{n: n, rx: rx, w: w, opt: opt, session: -1}
	h := max(n.horizon, 0)
	e.nextPair = h + opt.PairEveryS
	e.nextStat = math.Ceil(h)
	if e.nextStat == 0 {
		e.nextStat = 1
	}
	n.Attach(rx, e)
	return e
}

func (e *AVFEmitter) spec() *ReceiverSpec { return &e.n.cfg.Receivers[e.rx] }

func (e *AVFEmitter) receiverRecord() AVFReceiver {
	sp := e.spec()
	r := AVFReceiver{RxNo: sp.RxNo, Name: sp.Name, ClockClass: e.class, NominalHz: e.class.NominalHz(),
		EpochID: e.epoch, Channels: 1, Decoder: "simnet " + string(sp.Clock.Device), Timestamps: true}
	if e.opt.SendLocation {
		r.Location = &AVFLocation{Lat: sp.declaredLat, Lon: sp.declaredLon, Alt: sp.declaredAlt,
			AltRef: sp.DeclaredAltRef, Source: "manual", SigmaM: math.Max(sp.DeclaredErrorM, 10)}
	}
	return r
}

// up reports whether records can be written at host time h and handles a
// (re)connect: RECEIVER first, then backfill of the held batches.
func (e *AVFEmitter) up(h int64) bool {
	up, s := true, 0
	if e.opt.Conn != nil {
		up, s = e.opt.Conn.Up(h)
	}
	if !up {
		e.connOK = false
		return false
	}
	if s != e.session || !e.connOK {
		e.session, e.connOK = s, true
		e.w.Connect(h, s)
		e.needRecv, e.needReplay = true, true
	}
	if e.started && e.needRecv {
		e.needRecv = false
		r := e.receiverRecord()
		e.w.Receiver(h, &r)
	}
	if e.needReplay && !e.needRecv {
		e.needReplay = false
		e.replay(h)
	}
	return true
}

func (e *AVFEmitter) replay(h int64) {
	if len(e.held) == 0 {
		return
	}
	span := e.held[len(e.held)-1].Frames[len(e.held[len(e.held)-1].Frames)-1].HostNs - e.held[0].BaseHostNs
	from := e.heldFrom
	e.w.Backfill(h, &AVFBackfill{Phase: "begin", FromBatch: from, ToBatch: from + uint64(len(e.held)) - 1, SpanNs: span})
	for i := range e.held {
		b := &e.held[i]
		for j := range b.Frames {
			b.Frames[j].Late = true
		}
		e.Late += uint64(len(b.Frames))
		e.w.Frames(h, b)
	}
	e.w.Backfill(h, &AVFBackfill{Phase: "end", FromBatch: from, ToBatch: from + uint64(len(e.held)) - 1, SpanNs: span})
	e.held = e.held[:0]
}

func (e *AVFEmitter) host(h int64) int64 {
	if h < e.lastHost {
		h = e.lastHost
	}
	e.lastHost = h
	return h
}

func (e *AVFEmitter) flushBatch() {
	b := &e.batch
	if len(b.Frames) == 0 {
		return
	}
	if !e.opt.Unsorted {
		slices.SortStableFunc(b.Frames, func(x, y AVFFrame) int {
			switch {
			case x.Ticks < y.Ticks:
				return -1
			case x.Ticks > y.Ticks:
				return 1
			}
			return 0
		})
	}
	b.BaseTicks = b.Frames[0].Ticks
	b.BaseHostNs = b.Frames[0].HostNs
	e.nBatch++
	h := e.lastHost
	if e.up(h) {
		e.w.Frames(h, b)
		e.Batches++
		e.FramesOut += uint64(len(b.Frames))
	} else {
		// held in the RAM ring for backfill
		if len(e.held) == 0 {
			e.heldFrom = e.nBatch
		}
		cp := *b
		cp.Frames = slices.Clone(b.Frames)
		e.held = append(e.held, cp)
		for len(e.held) > 0 && float64(h-e.held[0].BaseHostNs)/1e9 > e.opt.HoldS {
			e.held = e.held[1:]
			e.heldFrom++
		}
	}
	b.Frames = b.Frames[:0]
}

func (e *AVFEmitter) hostAt(s float64) int64 {
	cs := &e.spec().Clock
	return e.n.UnixNs(s) + int64((cs.HostDelayMs+cs.HostOffsetMs)*1e6)
}

func (e *AVFEmitter) timeDriven(t float64) {
	if !e.started {
		return
	}
	for e.nextPair <= t {
		s := e.nextPair
		e.nextPair += e.opt.PairEveryS
		// The feeder pairs the ticks and host time of its latest frame.
		if e.lastTk == 0 {
			continue
		}
		h := e.host(e.hostAt(s))
		if e.up(h) {
			cs := &e.spec().Clock
			e.w.ClockPair(h, &AVFClockPair{RxNo: e.spec().RxNo, EpochID: e.epoch, Ticks: e.lastTk,
				HostUTCNs: e.lastFrameHost, HostErrEstNs: uint64(math.Max(1e6, math.Abs(cs.HostOffsetMs)*1e6))})
		}
	}
	dev := e.spec().Clock.Device
	if e.opt.StatusEveryS > 0 && (dev == DeviceRadarcapeGPS || dev == DeviceRadarcape12) {
		clk := e.n.rx[e.rx].clk
		for e.nextStat <= t {
			s := e.nextStat
			e.nextStat += e.opt.StatusEveryS
			h := e.host(e.hostAt(s))
			if !e.up(h) {
				continue
			}
			st := clk.status(s)
			ppsTicks := int(int8(st[1]))
			e.w.ClockStatus(h, &AVFClockStatus{RxNo: e.spec().RxNo, HostNs: h, Settings: st[0], GPSStatus: st[2],
				PPSOffsetTicks: ppsTicks, Raw: st[:]})
		}
	}
}

// Observe implements RxSink.
func (e *AVFEmitter) Observe(o *Observation) {
	h := e.host(o.HostNs)
	if !e.started || o.Epoch != e.epoch || o.Class != e.class {
		e.flushBatch()
		old := e.epoch
		first := !e.started
		e.epoch, e.class, e.started = o.Epoch, o.Class, true
		if e.up(h) && !first {
			ev := AVFClockEvent{RxNo: e.spec().RxNo, OldEpoch: old, NewEpoch: o.Epoch, Reason: model.ClockEvRestart, HostNs: h}
			for _, x := range e.n.ClockEvents(e.rx) {
				if x.NewEpoch == o.Epoch {
					ev.Reason, ev.SamplesLost = x.Reason, x.SamplesLost
				}
			}
			e.w.ClockEvent(h, &ev)
			e.needRecv = true
			e.up(h)
		}
		e.lastTk = 0
	}
	e.timeDriven(o.ArrivalS)
	if len(e.batch.Frames) > 0 && (h-e.bStart >= int64(e.opt.BatchMs*1e6) || len(e.batch.Frames) >= e.opt.MaxFrames) {
		e.flushBatch()
	}
	if len(e.batch.Frames) == 0 {
		e.bStart = h
		e.batch.RxNo, e.batch.EpochID = e.spec().RxNo, e.epoch
	}
	e.batch.Frames = append(e.batch.Frames, AVFFrame{Kind: o.Kind, Channel: -1, Ticks: o.Ticks, HostNs: o.HostNs, Sig: o.SignalByte, Payload: o.Data})
	e.lastTk, e.lastFrameHost = o.Ticks, o.HostNs
}

// Advance implements RxSink.
func (e *AVFEmitter) Advance(horizonS float64) {
	e.timeDriven(horizonS)
	if len(e.batch.Frames) > 0 && e.n.UnixNs(horizonS)-e.bStart >= int64(e.opt.BatchMs*1e6) {
		e.flushBatch()
	}
	e.w.Flush(e.n.UnixNs(horizonS))
}

// AVFRecord is one record captured by AVFRecorder.
type AVFRecord struct {
	HostNs   int64
	Session  int
	Type     byte // AVF record type code (0x01 RECEIVER ... 0x0B BACKFILL); 0 = connect marker
	Receiver *AVFReceiver
	Frames   *AVFFrames
	Status   *AVFClockStatus
	Pair     *AVFClockPair
	Event    *AVFClockEvent
	Backfill *AVFBackfill
}

// AVFRecorder is an AVFWriter that keeps deep copies of every record.
type AVFRecorder struct {
	Records []AVFRecord
	session int
}

// Connect implements AVFWriter.
func (r *AVFRecorder) Connect(h int64, s int) {
	r.session = s
	r.Records = append(r.Records, AVFRecord{HostNs: h, Session: s})
}

// Receiver implements AVFWriter.
func (r *AVFRecorder) Receiver(h int64, x *AVFReceiver) {
	c := *x
	if x.Location != nil {
		l := *x.Location
		c.Location = &l
	}
	r.Records = append(r.Records, AVFRecord{HostNs: h, Session: r.session, Type: 0x01, Receiver: &c})
}

// Frames implements AVFWriter.
func (r *AVFRecorder) Frames(h int64, x *AVFFrames) {
	c := *x
	c.Frames = slices.Clone(x.Frames)
	r.Records = append(r.Records, AVFRecord{HostNs: h, Session: r.session, Type: 0x02, Frames: &c})
}

// ClockStatus implements AVFWriter.
func (r *AVFRecorder) ClockStatus(h int64, x *AVFClockStatus) {
	c := *x
	c.Raw = slices.Clone(x.Raw)
	r.Records = append(r.Records, AVFRecord{HostNs: h, Session: r.session, Type: 0x03, Status: &c})
}

// ClockPair implements AVFWriter.
func (r *AVFRecorder) ClockPair(h int64, x *AVFClockPair) {
	c := *x
	r.Records = append(r.Records, AVFRecord{HostNs: h, Session: r.session, Type: 0x04, Pair: &c})
}

// ClockEvent implements AVFWriter.
func (r *AVFRecorder) ClockEvent(h int64, x *AVFClockEvent) {
	c := *x
	r.Records = append(r.Records, AVFRecord{HostNs: h, Session: r.session, Type: 0x05, Event: &c})
}

// Backfill implements AVFWriter.
func (r *AVFRecorder) Backfill(h int64, x *AVFBackfill) {
	c := *x
	r.Records = append(r.Records, AVFRecord{HostNs: h, Session: r.session, Type: 0x0B, Backfill: &c})
}

// Flush implements AVFWriter.
func (r *AVFRecorder) Flush(int64) {}
