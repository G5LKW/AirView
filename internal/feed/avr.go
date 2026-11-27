package feed

import (
	"math"
	"time"

	"airview/internal/model"
)

// AVRFrame is one decoded AVR-format line as produced by AVRParser. Data
// aliases the parser's internal buffer and is valid only during the callback.
type AVRFrame struct {
	Prefix       byte   // '*', ':', '@', '%' or '<'
	HasTimestamp bool   // '@', '%', '<'
	Timestamp    uint64 // 48-bit receiver timestamp
	HasSignal    bool   // '<'
	Signal       byte   // raw signal-level byte (same scale as Beast binary)
	Data         []byte // 2 (Mode A/C), 7 or 14 bytes
}

// AVRParser is a streaming decoder for the AVR / "raw" text formats:
//
//	*<hex>;                                   frame only (also ':' as start character)
//	@<12 hex timestamp><hex>;                 48-bit timestamp + frame
//	%<12 hex timestamp><hex>;                 same, frame passed the CRC check
//	<<12 hex timestamp><2 hex signal><hex>;   timestamp + signal level + frame
//
// <hex> is 4 (Mode A/C), 14 (Mode S short) or 28 (Mode S long) hex digits.
// Lines may be separated by CR, LF or nothing at all. Anything that is not
// a well-formed record is skipped and counted in Garbage.
//
// The parser does not allocate. It is not safe for concurrent use.
type AVRParser struct {
	emit   func(*AVRFrame)
	active bool
	prefix byte
	n      int
	hex    [48]byte // collected hex digits of the current record
	bin    [14]byte
	out    AVRFrame

	Frames  uint64 // well-formed records emitted
	Garbage uint64 // malformed records discarded
}

// NewAVRParser returns a parser that reports frames to emit.
func NewAVRParser(emit func(*AVRFrame)) *AVRParser {
	return &AVRParser{emit: emit}
}

// Reset drops any partially collected record.
func (p *AVRParser) Reset() { p.active, p.n = false, 0 }

func isAVRStart(c byte) bool {
	return c == '*' || c == ':' || c == '@' || c == '%' || c == '<'
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// Write consumes the next chunk of the stream.
func (p *AVRParser) Write(b []byte) {
	for _, c := range b {
		switch {
		case isAVRStart(c):
			if p.active {
				p.Garbage++ // previous record never terminated
			}
			p.active, p.prefix, p.n = true, c, 0
		case !p.active:
			// between records: line ends, spaces, noise
		case c == ';':
			p.active = false
			p.finish()
		case hexVal(c) >= 0:
			if p.n == len(p.hex) {
				p.active = false
				p.Garbage++
				continue
			}
			p.hex[p.n] = c
			p.n++
		default:
			// Any other byte inside a record (including a line break)
			// invalidates it.
			p.active = false
			p.Garbage++
		}
	}
}

func (p *AVRParser) finish() {
	h := p.hex[:p.n]
	out := &p.out
	*out = AVRFrame{Prefix: p.prefix}
	switch p.prefix {
	case '@', '%':
		if len(h) < 12 {
			p.Garbage++
			return
		}
		out.HasTimestamp = true
		out.Timestamp = hexUint(h[:12])
		h = h[12:]
	case '<':
		if len(h) < 14 {
			p.Garbage++
			return
		}
		out.HasTimestamp, out.HasSignal = true, true
		out.Timestamp = hexUint(h[:12])
		out.Signal = byte(hexUint(h[12:14]))
		h = h[14:]
	}
	if len(h) != 4 && len(h) != 14 && len(h) != 28 {
		p.Garbage++
		return
	}
	n := len(h) / 2
	for i := 0; i < n; i++ {
		p.bin[i] = byte(hexVal(h[2*i])<<4 | hexVal(h[2*i+1]))
	}
	out.Data = p.bin[:n]
	p.Frames++
	p.emit(out)
}

// hexUint converts validated hex digits to a number.
func hexUint(h []byte) uint64 {
	var v uint64
	for _, c := range h {
		v = v<<4 | uint64(hexVal(c))
	}
	return v
}

// AVRDecoder turns parsed AVR records into model.RawFrame values. Like
// BeastDecoder it owns a Timestamper; AVR streams carry no status frame, so
// GPS versus 12 MHz timestamps are always told apart statistically (so the
// clock class is sdr12 or gps_emul unless overridden). The network clock
// fields are filled as by BeastDecoder.
type AVRDecoder struct {
	Sink     model.FrameSink
	FeedID   string
	MLATOnly bool
	Recv     time.Time // set once per socket read
	OnMode   func(TimestampMode)

	TS Timestamper
}

// Handle processes one parsed record (use as the AVRParser emit function).
func (d *AVRDecoder) Handle(fr *AVRFrame) {
	f := model.RawFrame{
		Recv:    d.Recv,
		GPSNs:   -1,
		Signal:  math.NaN(),
		FeedID:  d.FeedID,
		Channel: -1,
		MLAT:    d.MLATOnly,
	}
	switch len(fr.Data) {
	case 2:
		f.Kind = model.FrameModeAC
	case 7:
		f.Kind = model.FrameModeSShort
	case 14:
		f.Kind = model.FrameModeSLong
	default:
		return
	}
	if fr.HasSignal {
		f.Signal = BeastSignalDBFS(fr.Signal)
	}
	if fr.HasTimestamp {
		switch {
		case IsMLATTimestamp(fr.Timestamp):
			f.MLAT = true
		case fr.Timestamp == 0:
			// no timestamp available at the source
		default:
			if d.TS.Observe(fr.Timestamp, d.Recv) && d.OnMode != nil {
				d.OnMode(d.TS.Mode())
			}
			f.GPSNs, f.Clock12MHz = d.TS.Convert(fr.Timestamp)
			if !f.MLAT {
				stampFrame(&d.TS, &f, fr.Timestamp)
			}
		}
	}
	if f.HostNs == 0 && !f.Recv.IsZero() {
		f.HostNs = f.Recv.UnixNano()
	}
	f.Data = make([]byte, len(fr.Data))
	copy(f.Data, fr.Data)
	d.Sink.HandleFrame(f)
}
