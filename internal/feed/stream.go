package feed

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"airview/internal/model"
	"airview/internal/ntp"
)

// Stream kinds handled by streamRunner.
const (
	streamBeast = iota + 1
	streamAVR
	streamSBS
)

// streamRunner is the TCP client for Beast, AVR and SBS feeds.
type streamRunner struct {
	cfg  model.Feed
	kind int
}

func newStreamRunner(cfg model.Feed, kind int) (Runner, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("feed has no host")
	}
	return &streamRunner{cfg: cfg, kind: kind}, nil
}

// Run implements Runner: it stays connected to the configured host until
// ctx is cancelled.
func (r *streamRunner) Run(ctx context.Context, s *Session) error {
	addr := net.JoinHostPort(strings.TrimSpace(r.cfg.Host), strconv.Itoa(feedPort(r.cfg)))
	buf := make([]byte, 64<<10) // reused for the lifetime of the feed
	idle := s.m.opts.IdleTimeout

	return s.dialLoop(ctx, addr, func(conn net.Conn) error {
		// Fresh decoder state per connection: nothing of a previous
		// (possibly different) stream may leak into this one.
		var consume func(b []byte, now time.Time)
		if r.kind == streamSBS {
			dec := &SBSDecoder{Sink: s, FeedID: r.cfg.ID, MLATOnly: r.cfg.MLATOnly}
			consume = func(b []byte, now time.Time) {
				dec.Recv = now
				dec.Write(b)
			}
			s.SetInfo("BaseStation (SBS) text")
		} else {
			raw := newRawStream(s, r.cfg, r.kind)
			consume = raw.Write
			s.SetInfo("")
		}
		s.SetGPSTime(false)

		for {
			// A silent link is a dead link: reconnect.
			conn.SetReadDeadline(time.Now().Add(idle))
			n, err := conn.Read(buf)
			if n > 0 {
				s.AddBytes(n)
				consume(buf[:n], ntp.Now())
			}
			if err != nil {
				return err
			}
		}
	})
}

// sniffLimit is how many bytes are examined at most before the configured
// format is assumed.
const sniffLimit = 64

// sniffFormat guesses whether a stream prefix is Beast binary or AVR text.
// It returns 0 while the prefix is inconclusive.
func sniffFormat(b []byte) int {
	text := true
	beastMarks, avrRecords := 0, 0
	for i, c := range b {
		switch {
		case c == beastEsc:
			text = false
			if i+1 < len(b) && b[i+1] >= BeastModeAC && b[i+1] <= BeastPosition {
				beastMarks++
			}
		case c == ';':
			if i > 0 && hexVal(b[i-1]) >= 0 {
				avrRecords++
			}
		case hexVal(c) >= 0, isAVRStart(c), c == '\r', c == '\n', c == ' ':
		default:
			text = false
		}
	}
	switch {
	case text && avrRecords > 0:
		return streamAVR
	case !text && beastMarks > 0:
		return streamBeast
	}
	return 0
}

// rawStream decodes a raw Mode S stream that is either Beast binary or AVR
// text. The Radarcape's raw port and several other sources can be switched
// between the two, so the format is sniffed from the first bytes of every
// connection; the feed's configured type only decides when the stream gives
// no clue.
type rawStream struct {
	s      *Session
	cfg    model.Feed
	prefer int
	format int // 0 = undecided
	sniff  []byte

	bp *BeastParser
	bd BeastDecoder
	ap *AVRParser
	ad AVRDecoder

	status    string
	position  string
	detected  TimestampMode
	hasStatus bool
	clockCls  model.ClockClass
}

func newRawStream(s *Session, cfg model.Feed, prefer int) *rawStream {
	r := &rawStream{s: s, cfg: cfg, prefer: prefer}
	r.bd = BeastDecoder{
		Sink: s, FeedID: cfg.ID, MLATOnly: cfg.MLATOnly,
		OnStatus:   r.onStatus,
		OnPosition: r.onPosition,
		OnMode:     r.onMode,
	}
	r.bp = NewBeastParser(r.bd.Handle)
	r.bp.OnEscape = r.onEscape
	r.ad = AVRDecoder{Sink: s, FeedID: cfg.ID, MLATOnly: cfg.MLATOnly, OnMode: r.onMode}
	r.ap = NewAVRParser(r.ad.Handle)

	// Clock epochs continue the numbering of the feed's previous
	// connection; a configured class overrides the detected one.
	prev := s.lastEpoch()
	override, hasOverride := s.clockOverride()
	for _, ts := range []*Timestamper{&r.bd.TS, &r.ad.TS} {
		ts.SetEpochBase(prev)
		if hasOverride {
			ts.SetClassOverride(override, true)
		}
		ts.OnClockEvent = r.onClockEvent
	}
	return r
}

// ts returns the Timestamper of the decoder in use.
func (r *rawStream) ts() *Timestamper {
	if r.format == streamAVR {
		return &r.ad.TS
	}
	return &r.bd.TS
}

// onEscape notes Beast extension messages. A plain receiver output does not
// send them; they are counted and otherwise ignored here (the aggregator's
// Beast ingest gives them meaning).
func (r *rawStream) onEscape(typ byte, body []byte) {
	if body == nil {
		r.s.escUnknown.Add(1)
		return
	}
	r.s.escKnown.Add(1)
}

// onClockEvent forwards a new clock epoch to the session (and from there to
// Manager.OnClockEvent) and refreshes the published class.
func (r *rawStream) onClockEvent(ev ClockEvent) {
	r.s.clockEvent(ev)
	r.syncClock()
}

// syncClock publishes the decoder's clock class to the session.
func (r *rawStream) syncClock() {
	cls := r.ts().Class()
	r.s.setClockClass(cls, r.ts().DetectedClass())
	if cls != r.clockCls {
		r.clockCls = cls
		r.publishInfo()
	}
}

// Write consumes one chunk read from the socket at time now.
func (r *rawStream) Write(b []byte, now time.Time) {
	if r.format == 0 {
		r.sniff = append(r.sniff, b...)
		f := sniffFormat(r.sniff)
		if f == 0 {
			if len(r.sniff) < sniffLimit {
				return
			}
			f = r.prefer
		}
		r.format = f
		if f != r.prefer {
			name := "Beast binary"
			if f == streamAVR {
				name = "AVR text"
			}
			r.s.log.Info("stream format differs from the configured feed type; following the stream", "format", name)
		}
		b, r.sniff = r.sniff, nil
		r.publishInfo()
	}
	if r.format == streamBeast {
		r.bd.Recv = now
		r.bp.Write(b)
	} else {
		r.ad.Recv = now
		r.ap.Write(b)
	}
}

func (r *rawStream) onStatus(st BeastStatus) {
	r.hasStatus = true
	r.status = st.String()
	r.publishInfo()
	r.syncClock()
}

func (r *rawStream) onPosition(pos ReceiverPosition) {
	r.s.ReportPosition(pos)
	text := fmt.Sprintf("receiver GPS position %.5f, %.5f, %.0f m", pos.Lat, pos.Lon, pos.AltM)
	if text != r.position {
		r.position = text
		r.publishInfo()
	}
}

func (r *rawStream) onMode(mode TimestampMode) {
	r.detected = mode
	r.s.SetGPSTime(mode == TimestampGPS)
	r.publishInfo()
	r.syncClock()
}

// publishInfo composes FeedStatus.Info from what is known about the stream.
func (r *rawStream) publishInfo() {
	var parts []string
	if r.format == streamAVR {
		parts = append(parts, "AVR text")
	} else {
		parts = append(parts, "Beast binary")
	}
	switch {
	case r.hasStatus:
		parts = append(parts, "Radarcape: "+r.status)
	case r.detected == TimestampGPS:
		parts = append(parts, "GPS timestamps (detected)")
	case r.detected == Timestamp12MHz:
		parts = append(parts, "12 MHz timestamps (detected)")
	}
	if r.clockCls.Timed() {
		parts = append(parts, "clock "+r.clockCls.String())
	}
	if ov, ok := r.ts().ClassOverride(); ok && r.ts().DetectedClass().Timed() && !OverrideApplies(ov, r.ts().DetectedClass()) {
		parts = append(parts, "clock class override "+ov.String()+" ignored (stream is "+r.ts().DetectedClass().String()+")")
	}
	if r.position != "" {
		parts = append(parts, r.position)
	}
	r.s.SetInfo(strings.Join(parts, "; "))
}
