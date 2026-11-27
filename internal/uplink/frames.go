package uplink

import (
	"math"

	"airview/internal/avf"
	"airview/internal/model"
)

// The uplink keeps every seq record as its body: the payload after the seq
// varint. Sending prefixes the seq of the moment (records are renumbered on
// a reconnect, see queue.connect). The avf encoders write the seq first, so
// bodies are encoded with seq 0 (one 0x00 byte) and that byte is cut.

// stripSeq0 removes the leading seq 0 of an avf-encoded payload.
func stripSeq0(p []byte) []byte {
	if len(p) > 0 && p[0] == 0 {
		return p[1:]
	}
	return p
}

// frameBatch collects the frames of one FRAMES record of one receiver.
type frameBatch struct {
	r         avf.Frames
	est       int // encoded size estimate
	firstHost int64
	lastTicks uint64
	baseSet   bool
}

// maxBatchBytes keeps a FRAMES record well below avf.MaxRecordLen.
const maxBatchBytes = avf.MaxRecordLen - 1024

func (b *frameBatch) reset(rxNo int, epoch uint32) {
	b.r.RxNo, b.r.EpochID = uint8(rxNo), epoch
	b.r.BaseTicks, b.r.BaseHostNs = 0, 0
	clear(b.r.Frames)
	b.r.Frames = b.r.Frames[:0]
	b.est, b.firstHost, b.lastTicks, b.baseSet = 40, 0, 0, false
}

func (b *frameBatch) n() int { return len(b.r.Frames) }

// canAppend reports whether a frame can join: room left, and timed frames
// in non-decreasing tick order.
func (b *frameBatch) canAppend(ticks uint64, timed bool, dataLen int) bool {
	if len(b.r.Frames) >= avf.MaxFramesPerRecord || b.est+dataLen+24 > maxBatchBytes {
		return false
	}
	return !timed || !b.baseSet || ticks >= b.lastTicks
}

// add appends a frame; data is retained until the batch is encoded (feeds
// hand over an owned, read-only slice per frame).
func (b *frameBatch) add(timed bool, channel int, ticks uint64, hostNs int64, sig byte, data []byte) {
	if len(b.r.Frames) == 0 {
		b.firstHost = hostNs
		b.r.BaseHostNs = hostNs - hostNs%1000 // whole µs: exact host times
	}
	f := avf.Frame{HostNs: hostNs, Data: data, Sig: sig, TsInvalid: !timed}
	if channel >= 0 && channel <= 14 {
		f.Channel = uint8(channel + 1)
	}
	if timed {
		if !b.baseSet {
			b.r.BaseTicks, b.baseSet = ticks, true
		}
		f.Ticks = ticks
		b.lastTicks = ticks
	}
	b.r.Frames = append(b.r.Frames, f)
	b.est += len(data) + 8
}

// body encodes the record body (without seq).
func (b *frameBatch) body() ([]byte, error) {
	b.r.Seq = 0
	p, err := avf.AppendFrames(make([]byte, 0, b.est+16), &b.r)
	if err != nil {
		return nil, err
	}
	return stripSeq0(p), nil
}

// markLate re-encodes a FRAMES body with the late bit on every frame.
func markLate(body []byte) ([]byte, int, error) {
	p := make([]byte, 0, len(body)+1)
	p = append(append(p, 0), body...)
	var r avf.Frames
	if err := r.Decode(p); err != nil {
		return nil, 0, err
	}
	for i := range r.Frames {
		r.Frames[i].Late = true
	}
	out, err := avf.AppendFrames(make([]byte, 0, len(p)), &r)
	if err != nil {
		return nil, 0, err
	}
	return stripSeq0(out), len(r.Frames), nil
}

// sigByte converts a dBFS level back to the raw Beast signal byte
// (dBFS = 20·log10(b/255)); NaN / infinite = 0 (unknown).
func sigByte(dbfs float64) byte {
	if math.IsNaN(dbfs) || math.IsInf(dbfs, 0) {
		return 0
	}
	v := math.Round(255 * math.Pow(10, dbfs/20))
	switch {
	case v < 1:
		return 1
	case v > 255:
		return 255
	}
	return byte(v)
}

// wireKind checks a model frame kind against its data length.
func wireKind(k model.FrameKind, n int) bool {
	switch k {
	case model.FrameModeAC:
		return n == 2
	case model.FrameModeSShort:
		return n == 7
	case model.FrameModeSLong:
		return n == 14
	}
	return false
}

// iptr rounds an optional float to an optional int (DECODED altitudes and
// vertical rate are integers).
func iptr(v *float64) *int {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	n := int(math.Round(*v))
	return &n
}
