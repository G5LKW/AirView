package avf

import (
	"encoding/binary"
	"math"

	"airview/internal/model"
)

// Binary payload codecs (AVF.md §5, §6). Append* functions append the
// payload only (no record header); Decode* methods parse a payload and
// ignore trailing bytes. Decoded byte slices alias the input payload.

// dec is a bounds-checked big-endian cursor. The first error sticks.
type dec struct {
	b    []byte
	what string
	err  error
}

func (d *dec) fail() {
	if d.err == nil {
		d.err = protoErr("short or malformed %s payload", d.what)
	}
	d.b = nil
}

func (d *dec) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *dec) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *dec) u8() uint8 {
	if d.err != nil || len(d.b) < 1 {
		d.fail()
		return 0
	}
	v := d.b[0]
	d.b = d.b[1:]
	return v
}

func (d *dec) u24() uint32 {
	if d.err != nil || len(d.b) < 3 {
		d.fail()
		return 0
	}
	v := uint32(d.b[0])<<16 | uint32(d.b[1])<<8 | uint32(d.b[2])
	d.b = d.b[3:]
	return v
}

func (d *dec) u32() uint32 {
	if d.err != nil || len(d.b) < 4 {
		d.fail()
		return 0
	}
	v := binary.BigEndian.Uint32(d.b)
	d.b = d.b[4:]
	return v
}

func (d *dec) u64() uint64 {
	if d.err != nil || len(d.b) < 8 {
		d.fail()
		return 0
	}
	v := binary.BigEndian.Uint64(d.b)
	d.b = d.b[8:]
	return v
}

func (d *dec) i64() int64   { return int64(d.u64()) }
func (d *dec) f64() float64 { return math.Float64frombits(d.u64()) }
func (d *dec) f32() float32 { return math.Float32frombits(d.u32()) }

func (d *dec) bytes(n int) []byte {
	if d.err != nil || n < 0 || len(d.b) < n {
		d.fail()
		return nil
	}
	v := d.b[:n:n]
	d.b = d.b[n:]
	return v
}

func appendU64(b []byte, v uint64) []byte { return binary.BigEndian.AppendUint64(b, v) }
func appendU32(b []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(b, v) }

// ---------------------------------------------------------------- seq

// AppendSeq appends the "uvarint seq" prefix of a seq-carrying record.
func AppendSeq(dst []byte, seq uint64) []byte { return binary.AppendUvarint(dst, seq) }

// SplitSeq splits a seq-carrying payload into its seq and the body that
// follows it.
func SplitSeq(payload []byte) (seq uint64, body []byte, err error) {
	v, n := binary.Uvarint(payload)
	if n <= 0 {
		return 0, nil, protoErr("missing or malformed seq")
	}
	return v, payload[n:], nil
}

// ---------------------------------------------------------------- FRAMES

// FrameKind is the hdr.kind of a frame (AVF.md §5.5).
type FrameKind uint8

// Frame kinds and their payload lengths.
const (
	KindModeAC     FrameKind = 0 // 2 bytes
	KindModeSShort FrameKind = 1 // 7 bytes
	KindModeSLong  FrameKind = 2 // 14 bytes
)

var kindLen = [3]int{2, 7, 14}

// KindForLen returns the frame kind of a payload length.
func KindForLen(n int) (FrameKind, bool) {
	switch n {
	case 2:
		return KindModeAC, true
	case 7:
		return KindModeSShort, true
	case 14:
		return KindModeSLong, true
	}
	return 0, false
}

// hdr bits.
const (
	hdrKindMask  = 0x03
	hdrLate      = 0x04
	hdrTsInvalid = 0x08
	hdrChanShift = 4
)

// Frame is one received frame inside a FRAMES record.
type Frame struct {
	// Ticks is the receiver timestamp (unwrapped ticks of the receiver's
	// clock class). It is 0 when TsInvalid is set.
	Ticks uint64
	// HostNs is the feeder's host UTC time of reception, Unix ns. The wire
	// carries it with 1 µs resolution.
	HostNs int64
	// Data is the raw Mode A/C (2), Mode S short (7) or long (14) bytes. On
	// decode it aliases the record payload.
	Data []byte
	// Channel is the RF channel + 1 (0 = unknown, 1..15 = channel 0..14).
	Channel uint8
	// Sig is the raw Beast signal byte (0 = unknown).
	Sig uint8
	// Late marks a backfilled frame (AVF.md §5.14).
	Late bool
	// TsInvalid marks a frame without a valid receiver timestamp.
	TsInvalid bool
}

// Kind returns the frame's kind from its data length.
func (f *Frame) Kind() FrameKind { k, _ := KindForLen(len(f.Data)); return k }

// Frames is a FRAMES (0x02) record: frames of one receiver and one clock
// epoch.
type Frames struct {
	Seq        uint64
	RxNo       uint8
	EpochID    uint32
	BaseTicks  uint64 // ticks of the reference point (first valid frame recommended; 0 for class none)
	BaseHostNs int64  // host UTC ns of the reference point (a multiple of 1000 keeps host times exact)
	Frames     []Frame
}

// AppendFrames appends the FRAMES payload of r to dst. Frames with a valid
// timestamp must be in non-decreasing tick order and not before BaseTicks.
func AppendFrames(dst []byte, r *Frames) ([]byte, error) {
	n := len(r.Frames)
	if n < 1 || n > MaxFramesPerRecord {
		return dst, protoErr("FRAMES with %d frames (1..%d)", n, MaxFramesPerRecord)
	}
	if r.RxNo == 0 {
		return dst, protoErr("FRAMES rx_no 0")
	}
	dst = binary.AppendUvarint(dst, r.Seq)
	dst = binary.AppendUvarint(dst, uint64(r.RxNo))
	dst = binary.AppendUvarint(dst, uint64(r.EpochID))
	dst = appendU64(dst, r.BaseTicks)
	dst = appendU64(dst, uint64(r.BaseHostNs))
	dst = binary.AppendUvarint(dst, uint64(n))
	ticks := r.BaseTicks
	hostUs := floorDiv1000(r.BaseHostNs)
	for i := range r.Frames {
		f := &r.Frames[i]
		kind, ok := KindForLen(len(f.Data))
		if !ok {
			return dst, protoErr("frame %d has %d data bytes", i, len(f.Data))
		}
		if f.Channel > 15 {
			return dst, protoErr("frame %d channel field %d > 15", i, f.Channel)
		}
		hdr := byte(kind) | f.Channel<<hdrChanShift
		if f.Late {
			hdr |= hdrLate
		}
		var dt uint64
		if f.TsInvalid {
			hdr |= hdrTsInvalid
		} else {
			if f.Ticks < ticks {
				return dst, protoErr("frame %d ticks go backwards", i)
			}
			dt = f.Ticks - ticks
			ticks = f.Ticks
		}
		us := floorDiv1000(f.HostNs)
		dst = append(dst, hdr)
		dst = binary.AppendUvarint(dst, dt)
		dst = binary.AppendVarint(dst, us-hostUs)
		hostUs = us
		dst = append(dst, f.Sig)
		dst = append(dst, f.Data...)
	}
	return dst, nil
}

// Decode parses a FRAMES payload into r, reusing r.Frames' storage. Frame
// data aliases payload.
func (r *Frames) Decode(payload []byte) error {
	d := dec{b: payload, what: "FRAMES"}
	r.Seq = d.uvarint()
	rx := d.uvarint()
	ep := d.uvarint()
	r.BaseTicks = d.u64()
	r.BaseHostNs = d.i64()
	n := d.uvarint()
	if d.err != nil {
		return d.err
	}
	if rx < 1 || rx > 255 {
		return protoErr("FRAMES rx_no %d", rx)
	}
	if ep > math.MaxUint32 {
		return protoErr("FRAMES epochId %d exceeds u32", ep)
	}
	if n < 1 || n > MaxFramesPerRecord {
		return protoErr("FRAMES with %d frames", n)
	}
	// Each frame needs at least 1+1+1+1+2 bytes; reject impossible counts
	// before allocating.
	if uint64(len(d.b)) < n*6 {
		return protoErr("FRAMES too short for %d frames", n)
	}
	r.RxNo, r.EpochID = uint8(rx), uint32(ep)
	if cap(r.Frames) < int(n) {
		r.Frames = make([]Frame, n)
	}
	r.Frames = r.Frames[:n]
	ticks := r.BaseTicks
	host := r.BaseHostNs
	for i := range r.Frames {
		hdr := d.u8()
		dt := d.uvarint()
		dh := d.varint()
		sig := d.u8()
		if d.err != nil {
			return d.err
		}
		kind := hdr & hdrKindMask
		if kind == 3 {
			return protoErr("frame %d has reserved kind 3", i)
		}
		f := &r.Frames[i]
		f.Data = d.bytes(kindLen[kind])
		if d.err != nil {
			return d.err
		}
		f.Sig = sig
		f.Channel = hdr >> hdrChanShift
		f.Late = hdr&hdrLate != 0
		f.TsInvalid = hdr&hdrTsInvalid != 0
		if f.TsInvalid {
			if dt != 0 {
				return protoErr("frame %d: tsInvalid with dTicks %d", i, dt)
			}
			f.Ticks = 0
		} else {
			if ticks+dt < ticks {
				return protoErr("frame %d: ticks overflow", i)
			}
			ticks += dt
			f.Ticks = ticks
		}
		if dh > math.MaxInt64/1000 || dh < math.MinInt64/1000 {
			return protoErr("frame %d: host delta out of range", i)
		}
		nh := host + dh*1000
		if (dh > 0 && nh < host) || (dh < 0 && nh > host) {
			return protoErr("frame %d: host time overflows", i)
		}
		host = nh
		f.HostNs = host
	}
	return nil
}

// floorDiv1000 returns floor(v/1000) (host µs of a host ns value).
func floorDiv1000(v int64) int64 {
	q := v / 1000
	if v%1000 < 0 {
		q--
	}
	return q
}

// ---------------------------------------------------------------- CLOCK_STATUS

// ClockStatus is a CLOCK_STATUS (0x03) record: a Radarcape type-'4' status.
type ClockStatus struct {
	Seq            uint64
	RxNo           uint8
	HostNs         int64
	Settings       uint8
	GPSStatus      uint8
	PPSOffsetTicks int64
	Raw            []byte // complete type-'4' payload, at most 64 bytes
}

// AppendClockStatus appends the CLOCK_STATUS payload.
func AppendClockStatus(dst []byte, c *ClockStatus) ([]byte, error) {
	if len(c.Raw) > MaxClockStatusRaw {
		return dst, protoErr("CLOCK_STATUS raw of %d bytes", len(c.Raw))
	}
	dst = binary.AppendUvarint(dst, c.Seq)
	dst = binary.AppendUvarint(dst, uint64(c.RxNo))
	dst = appendU64(dst, uint64(c.HostNs))
	dst = append(dst, c.Settings, c.GPSStatus)
	dst = binary.AppendVarint(dst, c.PPSOffsetTicks)
	dst = binary.AppendUvarint(dst, uint64(len(c.Raw)))
	return append(dst, c.Raw...), nil
}

// Decode parses a CLOCK_STATUS payload.
func (c *ClockStatus) Decode(p []byte) error {
	d := dec{b: p, what: "CLOCK_STATUS"}
	c.Seq = d.uvarint()
	rx := d.uvarint()
	c.HostNs = d.i64()
	c.Settings = d.u8()
	c.GPSStatus = d.u8()
	c.PPSOffsetTicks = d.varint()
	n := d.uvarint()
	if d.err == nil && n > MaxClockStatusRaw {
		return protoErr("CLOCK_STATUS raw length %d", n)
	}
	c.Raw = d.bytes(int(n))
	if d.err != nil {
		return d.err
	}
	if rx < 1 || rx > 255 {
		return protoErr("CLOCK_STATUS rx_no %d", rx)
	}
	c.RxNo = uint8(rx)
	return nil
}

// ---------------------------------------------------------------- CLOCK_PAIR

// ClockPair is a CLOCK_PAIR (0x04) record: one (ticks, host UTC) sample.
type ClockPair struct {
	Seq          uint64
	RxNo         uint8
	EpochID      uint32
	Ticks        uint64
	HostUTCNs    int64
	HostErrEstNs uint64
}

// AppendClockPair appends the CLOCK_PAIR payload.
func AppendClockPair(dst []byte, c *ClockPair) []byte {
	dst = binary.AppendUvarint(dst, c.Seq)
	dst = binary.AppendUvarint(dst, uint64(c.RxNo))
	dst = binary.AppendUvarint(dst, uint64(c.EpochID))
	dst = appendU64(dst, c.Ticks)
	dst = appendU64(dst, uint64(c.HostUTCNs))
	return binary.AppendUvarint(dst, c.HostErrEstNs)
}

// Decode parses a CLOCK_PAIR payload.
func (c *ClockPair) Decode(p []byte) error {
	d := dec{b: p, what: "CLOCK_PAIR"}
	c.Seq = d.uvarint()
	rx := d.uvarint()
	ep := d.uvarint()
	c.Ticks = d.u64()
	c.HostUTCNs = d.i64()
	c.HostErrEstNs = d.uvarint()
	if d.err != nil {
		return d.err
	}
	if rx < 1 || rx > 255 || ep > math.MaxUint32 {
		return protoErr("CLOCK_PAIR rx_no %d epoch %d", rx, ep)
	}
	c.RxNo, c.EpochID = uint8(rx), uint32(ep)
	return nil
}

// ---------------------------------------------------------------- PING / PONG / ACK

// Ping is a PING (0x81) record.
type Ping struct {
	ID           uint64
	ServerSendNs int64
}

// AppendPing appends the PING payload.
func AppendPing(dst []byte, p Ping) []byte {
	dst = binary.AppendUvarint(dst, p.ID)
	return appendU64(dst, uint64(p.ServerSendNs))
}

// Decode parses a PING payload.
func (p *Ping) Decode(b []byte) error {
	d := dec{b: b, what: "PING"}
	p.ID = d.uvarint()
	p.ServerSendNs = d.i64()
	return d.err
}

// Pong is a PONG (0x0A) record.
type Pong struct {
	ID   uint64
	RxNs int64 // feeder host UTC when the PING was read
	TxNs int64 // feeder host UTC when the PONG was written
}

// AppendPong appends the PONG payload.
func AppendPong(dst []byte, p Pong) []byte {
	dst = binary.AppendUvarint(dst, p.ID)
	dst = appendU64(dst, uint64(p.RxNs))
	return appendU64(dst, uint64(p.TxNs))
}

// Decode parses a PONG payload.
func (p *Pong) Decode(b []byte) error {
	d := dec{b: b, what: "PONG"}
	p.ID = d.uvarint()
	p.RxNs = d.i64()
	p.TxNs = d.i64()
	return d.err
}

// AppendAck appends the ACK (0x82) payload.
func AppendAck(dst []byte, seq uint64) []byte { return binary.AppendUvarint(dst, seq) }

// DecodeAck parses an ACK payload.
func DecodeAck(b []byte) (uint64, error) {
	d := dec{b: b, what: "ACK"}
	v := d.uvarint()
	return v, d.err
}

// ---------------------------------------------------------------- MLAT_RESULTS

const (
	fixLen    = 3 + 8 + 24 + 24 + 4
	fixVelLen = 12
	optVel    = 0x01
)

// AppendMLATResults appends the MLAT_RESULTS (0x85) payload. The fixes
// carry no station identities by construction.
func AppendMLATResults(dst []byte, fixes []model.MLATFix) ([]byte, error) {
	if len(fixes) < 1 || len(fixes) > MaxFixesPerRecord {
		return dst, protoErr("MLAT_RESULTS with %d fixes (1..%d)", len(fixes), MaxFixesPerRecord)
	}
	dst = binary.AppendUvarint(dst, uint64(len(fixes)))
	for i := range fixes {
		f := &fixes[i]
		if f.ICAO > 0xFFFFFF {
			return dst, protoErr("fix ICAO %X exceeds 24 bits", f.ICAO)
		}
		dst = append(dst, byte(f.ICAO>>16), byte(f.ICAO>>8), byte(f.ICAO))
		dst = appendU64(dst, uint64(f.TUnixNs))
		for _, v := range f.ECEF {
			dst = appendU64(dst, math.Float64bits(v))
		}
		for _, v := range f.Cov {
			dst = appendU32(dst, math.Float32bits(v))
		}
		var opt byte
		if f.Vel != nil {
			opt |= optVel
		}
		dst = append(dst, f.NBucket, f.Grade, f.Flags, opt)
		if f.Vel != nil {
			for _, v := range f.Vel {
				dst = appendU32(dst, math.Float32bits(v))
			}
		}
	}
	return dst, nil
}

// DecodeMLATResults parses an MLAT_RESULTS payload, appending the fixes to
// dst. Velocities of one record share one allocation.
func DecodeMLATResults(dst []model.MLATFix, p []byte) ([]model.MLATFix, error) {
	d := dec{b: p, what: "MLAT_RESULTS"}
	n := d.uvarint()
	if d.err != nil {
		return dst, d.err
	}
	if n < 1 || n > MaxFixesPerRecord {
		return dst, protoErr("MLAT_RESULTS with %d fixes", n)
	}
	if uint64(len(d.b)) < n*fixLen {
		return dst, protoErr("MLAT_RESULTS too short for %d fixes", n)
	}
	var vels [][3]float32
	for i := uint64(0); i < n; i++ {
		var f model.MLATFix
		f.ICAO = d.u24()
		f.TUnixNs = d.i64()
		for k := range f.ECEF {
			f.ECEF[k] = d.f64()
		}
		for k := range f.Cov {
			f.Cov[k] = d.f32()
		}
		f.NBucket = d.u8()
		f.Grade = d.u8()
		f.Flags = d.u8()
		opt := d.u8()
		if opt&optVel != 0 {
			if vels == nil {
				vels = make([][3]float32, 0, n-i)
			}
			var v [3]float32
			for k := range v {
				v[k] = d.f32()
			}
			vels = append(vels, v)
			f.Vel = &vels[len(vels)-1]
		}
		if d.err != nil {
			return dst, d.err
		}
		dst = append(dst, f)
	}
	return dst, nil
}

// ---------------------------------------------------------------- DECODED head

// AppendDecodedHead appends the binary head of a DECODED (0x08) record
// (seq, rx_no); the JSON array follows to the end of the payload.
func AppendDecodedHead(dst []byte, seq uint64, rxNo uint8) []byte {
	dst = binary.AppendUvarint(dst, seq)
	return binary.AppendUvarint(dst, uint64(rxNo))
}

// SplitDecoded splits a DECODED payload into seq, rx_no and the JSON array.
func SplitDecoded(p []byte) (seq uint64, rxNo uint8, js []byte, err error) {
	d := dec{b: p, what: "DECODED"}
	seq = d.uvarint()
	rx := d.uvarint()
	if d.err != nil {
		return 0, 0, nil, d.err
	}
	if rx < 1 || rx > 255 {
		return 0, 0, nil, protoErr("DECODED rx_no %d", rx)
	}
	return seq, uint8(rx), d.b, nil
}
