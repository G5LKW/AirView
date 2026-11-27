package simnet

import (
	"encoding/binary"
	"math"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
)

// emitBase is the buffering and connection handling shared by the byte
// stream emitters.
type emitBase struct {
	n        *Network
	rx       int
	out      ByteSink
	conn     ConnState
	flushNs  int64
	buf      []byte
	bufStart int64
	lastHost int64
	session  int
	fresh    bool // a (re)connect happened: send the preamble first
	down     bool
	Dropped  uint64 // bytes not written because the connection was down
}

func (b *emitBase) init(n *Network, rx int, out ByteSink, flushMs float64) {
	b.n, b.rx, b.out = n, rx, out
	if cs, ok := out.(ConnState); ok {
		b.conn = cs
	}
	if !(flushMs > 0) {
		flushMs = 20
	}
	b.flushNs = int64(flushMs * 1e6)
	b.fresh = true
	b.session = -1
}

// up checks the connection at host time h; it reports whether data can be
// written and sets fresh on a new session.
func (b *emitBase) up(h int64) bool {
	if b.conn == nil {
		return true
	}
	up, s := b.conn.Up(h)
	if !up {
		if !b.down {
			b.flush()
			b.buf = b.buf[:0]
		}
		b.down = true
		return false
	}
	b.down = false
	if s != b.session {
		b.flush()
		b.session = s
		b.fresh = true
	}
	return true
}

func (b *emitBase) host(h int64) int64 {
	if h < b.lastHost {
		h = b.lastHost
	}
	b.lastHost = h
	return h
}

// begin prepares appending a record written at host time h.
func (b *emitBase) begin(h int64) {
	if len(b.buf) > 0 && h-b.bufStart >= b.flushNs {
		b.flush()
	}
	if len(b.buf) == 0 {
		b.bufStart = h
	}
}

func (b *emitBase) flush() {
	if len(b.buf) == 0 {
		return
	}
	b.out.Write(b.lastHost, b.buf)
	b.buf = b.buf[:0]
}

func (b *emitBase) advance(horizonS float64) {
	b.flush()
	if f, ok := b.out.(Flusher); ok {
		f.Flush(b.n.UnixNs(horizonS))
	}
}

// BeastFormat selects the Beast flavour.
type BeastFormat uint8

// Beast formats.
const (
	// BeastFull is a plain Beast binary stream (dump1090/readsb
	// beast_out, Radarcape port 10003): every frame.
	BeastFull BeastFormat = iota
	// BeastReducePlus is readsb's beast_reduce_plus_out: on connect the
	// 0x1A 0xE4 escape with a fixed 36-character ASCII UUID and 0x1A 'W'
	// 'O', then frames thinned per aircraft and message type to one per
	// ReduceInterval (non-position data at 4x the interval).
	BeastReducePlus
)

// BeastOptions configures a BeastEmitter.
type BeastOptions struct {
	Format         BeastFormat
	ReduceInterval time.Duration // 0 = 250 ms (BeastReducePlus)
	// ReceiverID > 0 sends the 0x1A 0xE3 receiver-ID escape (8 bytes,
	// big-endian) after the connection preamble.
	ReceiverID uint64
	// NoStatus suppresses the Radarcape type '4' status frame that
	// Radarcape devices send every second.
	NoStatus bool
	// PositionEveryS > 0 sends a Radarcape type '5' position frame (the
	// declared position) that often (Radarcape devices only).
	PositionEveryS float64
	// HeartbeatS > 0 sends an all-zero Mode A/C frame after that many
	// seconds without output (readsb's keep-alive).
	HeartbeatS float64
	// FlushMs batches output into writes of at most this host-time span
	// (0 = 20 ms).
	FlushMs float64
}

// BeastEmitter turns one receiver's observations into a Beast binary
// stream: types '2'/'3' with the receiver's 48-bit timestamp (Radarcape GPS
// format for GPS devices, the 12 MHz counter for Beast/SDR/Airspy, the
// 24-bit 20 MHz counter for SBS) and the signal byte.
type BeastEmitter struct {
	emitBase
	opt      BeastOptions
	radar    bool
	nextPPS  float64
	nextPos  float64
	lastOutS float64
	reduce   map[uint64]float64
	pruneAt  float64
	interval float64

	Frames  uint64 // Mode S frames written
	Reduced uint64 // frames suppressed by the reduce filter
	Status  uint64 // status frames written
}

// NewBeastEmitter creates a Beast emitter for receiver rx writing to out and
// attaches it to the network.
func (n *Network) NewBeastEmitter(rx int, out ByteSink, opt BeastOptions) *BeastEmitter {
	e := &BeastEmitter{opt: opt}
	e.init(n, rx, out, opt.FlushMs)
	dev := n.cfg.Receivers[rx].Clock.Device
	e.radar = dev == DeviceRadarcapeGPS || dev == DeviceRadarcape12
	e.nextPPS = math.Ceil(math.Max(n.horizon, 0))
	if e.nextPPS == 0 {
		e.nextPPS = 1
	}
	if e.radar {
		// A GPS-disciplined clock's status frame is stamped with a whole
		// second, so it must be written at the instant the clock reads that
		// second: when the network start is not on a whole UTC second, the
		// simulation's integer seconds are not, and a status frame stamped
		// with the rounded second went up to 0.5 s back against the Mode S
		// frames around it (a false "backwards" clock step every second;
		// found in the integrator's live smoke run with -host-listen).
		if ct, _ := n.rx[rx].clk.truth(e.nextPPS); ct.Class.GPS() {
			_, frac := math.Modf(ct.Ticks / 1e9)
			e.nextPPS -= frac
			if e.nextPPS < math.Max(n.horizon, 0) {
				e.nextPPS++
			}
		}
	}
	e.nextPos = math.Max(n.horizon, 0)
	if opt.Format == BeastReducePlus {
		e.interval = 0.25
		if opt.ReduceInterval > 0 {
			e.interval = opt.ReduceInterval.Seconds()
		}
		e.reduce = make(map[uint64]float64)
	}
	n.Attach(rx, e)
	return e
}

func (e *BeastEmitter) preamble() {
	if e.opt.Format == BeastReducePlus {
		e.buf = append(e.buf, 0x1A, 0xE4)
		e.buf = append(e.buf, uuid36(e.n.cfg.Receivers[e.rx].UUID)...)
		e.buf = append(e.buf, 0x1A, 'W', 'O')
	}
	if e.opt.ReceiverID != 0 {
		var id [8]byte
		binary.BigEndian.PutUint64(id[:], e.opt.ReceiverID)
		e.buf = append(e.buf, 0x1A, 0xE3)
		for _, c := range id {
			e.buf = append(e.buf, c)
			if c == 0x1A {
				e.buf = append(e.buf, 0x1A)
			}
		}
	}
	e.fresh = false
}

// uuid36 pads or truncates a UUID to readsb's fixed 36 ASCII characters
// (padding with 'f').
func uuid36(u string) []byte {
	b := make([]byte, 36)
	for i := range b {
		b[i] = 'f'
	}
	copy(b, u)
	for i, c := range b {
		if c == 0x1A || c >= 0x80 {
			b[i] = 'f'
		}
	}
	return b
}

func (e *BeastEmitter) hostAt(s float64) int64 {
	cs := &e.n.cfg.Receivers[e.rx].Clock
	return e.n.UnixNs(s) + int64((cs.HostDelayMs+cs.HostOffsetMs)*1e6)
}

// timeDriven writes the status / position / heartbeat frames due before t.
func (e *BeastEmitter) timeDriven(t float64) {
	r := e.n.rx[e.rx]
	if e.radar && !e.opt.NoStatus {
		for e.nextPPS <= t {
			s := e.nextPPS
			e.nextPPS++
			ct, _ := r.clk.truth(s)
			var ts uint64
			if ct.Class.GPS() {
				ns := int64(math.Round(ct.Ticks/1e9)) * 1e9
				day := ns % model.NsPerDay
				ts = feed.GPSTimestamp(int(day/1e9), 0)
			} else {
				ts = uint64(ct.Ticks) & (1<<48 - 1)
			}
			st := r.clk.status(s)
			e.write(e.hostAt(s), feed.BeastStatusType, ts, 0, st[:], s)
			e.Status++
		}
	}
	if e.radar && e.opt.PositionEveryS > 0 {
		for e.nextPos <= t {
			s := e.nextPos
			e.nextPos += e.opt.PositionEveryS
			sp := &e.n.cfg.Receivers[e.rx]
			var p [21]byte
			binary.LittleEndian.PutUint32(p[4:], math.Float32bits(float32(sp.declaredLat)))
			binary.LittleEndian.PutUint32(p[8:], math.Float32bits(float32(sp.declaredLon)))
			binary.LittleEndian.PutUint32(p[12:], math.Float32bits(float32(sp.declaredAlt)))
			e.write(e.hostAt(s), feed.BeastPosition, 0, 0, p[:], s)
		}
	}
	if e.opt.HeartbeatS > 0 && t-e.lastOutS >= e.opt.HeartbeatS {
		s := e.lastOutS + e.opt.HeartbeatS
		e.write(e.hostAt(s), feed.BeastModeAC, 0, 0, []byte{0, 0}, s)
	}
}

func (e *BeastEmitter) write(h int64, typ byte, ts uint64, sig byte, payload []byte, s float64) {
	h = e.host(h)
	if !e.up(h) {
		e.Dropped += uint64(len(payload) + 9)
		return
	}
	e.begin(h)
	if e.fresh {
		e.preamble()
	}
	e.buf = feed.AppendBeastFrame(e.buf, typ, ts, sig, payload)
	e.lastOutS = s
}

// reduceKey classifies a frame for the reduce filter. pos reports
// position-relevant data (sent at the base interval).
func reduceKey(o *Observation) (key uint64, pos bool) {
	d := o.Data
	df := d[0] >> 3
	k := uint64(o.ICAO)<<16 | uint64(df)<<8
	if (df == 17 || df == 18) && len(d) == 14 {
		tc := d[4] >> 3
		k |= uint64(tc) << 1
		switch {
		case tc >= 5 && tc <= 18, tc >= 20 && tc <= 22:
			k |= uint64((d[6] >> 2) & 1) // CPR odd/even kept apart
			pos = true
		case tc == 19:
			pos = true
		}
	}
	return k, pos
}

// Observe implements RxSink.
func (e *BeastEmitter) Observe(o *Observation) {
	e.timeDriven(o.ArrivalS)
	if e.reduce != nil {
		k, pos := reduceKey(o)
		iv := e.interval
		if !pos {
			iv *= 4
		}
		if last, ok := e.reduce[k]; ok && o.ArrivalS-last < iv {
			e.Reduced++
			return
		}
		e.reduce[k] = o.ArrivalS
		if o.ArrivalS >= e.pruneAt {
			e.pruneAt = o.ArrivalS + 60
			for k, v := range e.reduce {
				if o.ArrivalS-v > 30 {
					delete(e.reduce, k)
				}
			}
		}
	}
	e.write(o.HostNs, beastType(o.Kind), o.BeastTS, o.SignalByte, o.Data, o.ArrivalS)
	e.Frames++
}

// Advance implements RxSink.
func (e *BeastEmitter) Advance(horizonS float64) {
	e.timeDriven(horizonS)
	e.advance(horizonS)
}
