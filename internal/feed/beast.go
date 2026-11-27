package feed

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"

	"airview/internal/model"
)

// Beast binary frame type bytes (the byte after the 0x1A escape).
const (
	BeastModeAC     = '1' // 6-byte timestamp, signal, 2 bytes Mode A/C
	BeastModeSShort = '2' // 6-byte timestamp, signal, 7 bytes Mode S
	BeastModeSLong  = '3' // 6-byte timestamp, signal, 14 bytes Mode S
	BeastStatusType = '4' // 6-byte timestamp, 1 unused byte, 14 bytes Radarcape status
	BeastPosition   = '5' // 21 bytes Radarcape GPS position, no timestamp/signal

	beastEsc = 0x1A
)

// Beast escape (extension) types used by readsb-style feeders on aggregator
// connections (beast_reduce_plus_out). They carry no Mode S data; the
// parser reports them through BeastParser.OnEscape instead of the frame
// callback. Payload lengths are after de-escaping.
const (
	BeastReceiverID = 0xE3 // 8 bytes: big-endian receiver ID of the frames that follow
	BeastUUID       = 0xE4 // 36 bytes: ASCII feeder UUID, sent once at connect
	BeastPing       = 'P'  // 3 bytes: big-endian 24-bit millisecond counter
	BeastOption     = 'W'  // 1 byte: option / request ('O' ping on, 'S' slow down, ...)

	BeastUUIDLen = 36
)

// MLAT synthetic timestamp range used by mlat-client and its relatives for
// the DF18 frames that carry multilateration results.
const (
	mlatMagicStart uint64 = 0xFF004D4C4154 // "\xFF\x00MLAT"
	mlatMagicEnd   uint64 = mlatMagicStart + 10
)

// IsMLATTimestamp reports whether a 48-bit Beast timestamp is the magic
// value that marks a synthetic MLAT result frame.
func IsMLATTimestamp(ts uint64) bool { return ts >= mlatMagicStart && ts <= mlatMagicEnd }

// BeastFrame is one de-escaped Beast binary frame as produced by
// BeastParser. Payload aliases the parser's internal buffer and is valid
// only during the callback.
type BeastFrame struct {
	Type      byte   // BeastModeAC .. BeastPosition
	Timestamp uint64 // 48-bit raw timestamp (0 for BeastPosition)
	Signal    byte   // raw signal-level byte (0 for BeastStatusType / BeastPosition)
	Payload   []byte // 2, 7, 14, 14 or 21 bytes
}

// beastBodyLen returns the number of de-escaped bytes that follow the type
// byte, or 0 for an unknown type.
func beastBodyLen(typ byte) int {
	switch typ {
	case BeastModeAC:
		return 7 + 2
	case BeastModeSShort:
		return 7 + 7
	case BeastModeSLong, BeastStatusType:
		return 7 + 14
	case BeastPosition:
		return 21
	case BeastReceiverID:
		return 8
	case BeastUUID:
		return BeastUUIDLen
	case BeastPing:
		return 3
	case BeastOption:
		return 1
	}
	return 0
}

// IsBeastEscapeType reports whether typ is one of the known extension
// (non-frame) escape types: BeastReceiverID, BeastUUID, BeastPing or
// BeastOption.
func IsBeastEscapeType(typ byte) bool {
	switch typ {
	case BeastReceiverID, BeastUUID, BeastPing, BeastOption:
		return true
	}
	return false
}

// BeastParser is a streaming decoder for the Beast binary format. Feed it
// arbitrary chunks of the TCP stream with Write; it calls the emit function
// once per complete frame. It copes with frames split across chunks, with
// escaped 0x1A bytes anywhere, with truncated frames and with garbage
// between frames (it resynchronises on the next 0x1A + known type).
//
// Extension messages (BeastReceiverID, BeastUUID, BeastPing, BeastOption)
// are not frames: they are passed to OnEscape when set and counted in
// Skipped. A 0x1A followed by any other unknown type byte is reported to
// OnEscape with a nil body, counted in Unknown and skipped: its length is
// unknowable, so the parser resynchronises on the next frame start. Nothing
// a peer sends can make the parser fail; the caller never has to drop a
// link because of the content.
//
// The parser does not allocate. It is not safe for concurrent use.
type BeastParser struct {
	// OnEscape, when set, receives every extension message: typ is the
	// escape type and body its de-escaped payload (aliasing the parser's
	// buffer, valid only during the call), or nil for an unknown type. It
	// may be set or changed between Write calls.
	OnEscape func(typ byte, body []byte)

	emit  func(*BeastFrame)
	state uint8
	typ   byte
	need  int
	n     int
	buf   [BeastUUIDLen]byte
	out   BeastFrame

	Frames    uint64 // complete frames emitted
	Skipped   uint64 // complete extension messages (receiver ID, UUID, ping, option)
	Unknown   uint64 // escapes with an unknown type byte (skipped)
	Garbage   uint64 // bytes discarded while out of sync
	Truncated uint64 // frames cut short by the start of another frame
}

const (
	bsSync    = iota // looking for 0x1A
	bsType           // 0x1A seen, expecting the type byte
	bsBody           // collecting the frame body
	bsBodyEsc        // 0x1A seen inside the body
)

// NewBeastParser returns a parser that reports frames to emit.
func NewBeastParser(emit func(*BeastFrame)) *BeastParser {
	return &BeastParser{emit: emit}
}

// Reset drops any partially collected frame (use after a reconnect).
func (p *BeastParser) Reset() { p.state, p.n = bsSync, 0 }

// Write consumes the next chunk of the stream.
func (p *BeastParser) Write(b []byte) {
	i := 0
	for i < len(b) {
		switch p.state {
		case bsSync:
			j := bytes.IndexByte(b[i:], beastEsc)
			if j < 0 {
				p.Garbage += uint64(len(b) - i)
				return
			}
			p.Garbage += uint64(j)
			i += j + 1
			p.state = bsType

		case bsType:
			c := b[i]
			i++
			p.startFrame(c)

		case bsBody:
			chunk := b[i:]
			if room := p.need - p.n; len(chunk) > room {
				chunk = chunk[:room]
			}
			j := bytes.IndexByte(chunk, beastEsc)
			if j < 0 {
				p.n += copy(p.buf[p.n:], chunk)
				i += len(chunk)
			} else {
				p.n += copy(p.buf[p.n:], chunk[:j])
				i += j + 1
				p.state = bsBodyEsc
				continue
			}
			if p.n == p.need {
				p.finish()
			}

		case bsBodyEsc:
			c := b[i]
			i++
			if c == beastEsc { // escaped literal 0x1A
				p.buf[p.n] = beastEsc
				p.n++
				p.state = bsBody
				if p.n == p.need {
					p.finish()
				}
				continue
			}
			// A lone 0x1A inside a frame: the frame was cut short and a new
			// one starts here.
			p.Truncated++
			p.startFrame(c)
		}
	}
}

// startFrame handles the type byte that followed a 0x1A.
func (p *BeastParser) startFrame(typ byte) {
	need := beastBodyLen(typ)
	if need == 0 {
		// Unknown type, or "1A 1A": an escaped data byte seen while out of
		// sync. Either way these two bytes are not a frame start.
		p.Garbage += 2
		p.state = bsSync
		if typ != beastEsc {
			p.Unknown++
			if p.OnEscape != nil {
				p.OnEscape(typ, nil)
			}
		}
		return
	}
	p.typ, p.need, p.n = typ, need, 0
	p.state = bsBody
}

func (p *BeastParser) finish() {
	p.state = bsSync
	body := p.buf[:p.n]
	switch p.typ {
	case BeastReceiverID, BeastUUID, BeastPing, BeastOption:
		p.Skipped++
		if p.OnEscape != nil {
			p.OnEscape(p.typ, body)
		}
		return
	case BeastPosition:
		p.out = BeastFrame{Type: p.typ, Payload: body}
	default:
		p.out = BeastFrame{
			Type:      p.typ,
			Timestamp: uint64(body[0])<<40 | uint64(body[1])<<32 | uint64(body[2])<<24 | uint64(body[3])<<16 | uint64(body[4])<<8 | uint64(body[5]),
			Signal:    body[6],
			Payload:   body[7:],
		}
	}
	p.Frames++
	p.emit(&p.out)
}

// --- signal level -------------------------------------------------------------

var beastSignalTable = func() (t [256]float64) {
	t[0] = math.NaN()
	for i := 1; i < 256; i++ {
		t[i] = 20 * math.Log10(float64(i)/255)
	}
	return
}()

// BeastSignalDBFS converts the Beast signal-level byte to dBFS using the
// de-facto convention (the byte is the signal amplitude scaled to 0..255, so
// power = (b/255)^2 and dBFS = 20*log10(b/255)). 0 means "unknown" and
// yields NaN; 255 yields 0 dBFS; 1 yields -48.1 dBFS.
func BeastSignalDBFS(b byte) float64 { return beastSignalTable[b] }

// --- Radarcape status frame ---------------------------------------------------

// BeastStatus is the decoded Radarcape status frame (Beast type '4'). Parse
// RawFrame.Data of a model.FrameStatus frame with ParseBeastStatus.
type BeastStatus struct {
	Settings  byte // byte 0: mirror of the FPGA configuration switches
	PPSOffset int8 // byte 1: time stamp error at the 1PPS edge, int8 ticks of PPSTickNs (15 ns)
	GPS       byte // byte 2: GPS status bits

	// Decoded Settings bits.
	BinaryOutput   bool // bit 0: port 10002 outputs binary (else AVR)
	DF17Only       bool // bit 1: only DF11/DF17/DF18 are output
	AVRTimestamps  bool // bit 2: timestamps enabled in AVR format
	GPSTimestamps  bool // bit 4: GPS timestamps (else legacy 12 MHz counter)
	FlowControlOff bool // bit 5: serial flow control disabled
	FECDisabled    bool // bit 6: 1-bit error correction disabled
	ModeAC         bool // bit 7: Mode A/C decoding enabled

	// Decoded GPS status bits.
	AntennaOK    bool // bit 0
	Tracking     bool // bit 1: tracking at least one satellite
	Sats3        bool // bit 2: tracking at least three satellites
	UTC          bool // bit 3: time base is UTC (else GPS time)
	FPGASync     bool // bit 4: FPGA clock synchronised to GPS (per mlat-client; "unused" in the wiki)
	TimestampBad bool // bit 5: timestamps not usable / emulated (per mlat-client)
	TrueUTCRange bool // bit 7: second-of-day runs 0..86399 (else legacy 1..86400)

	// GPSDetail reports whether the GPS status bits carry information at
	// all. Firmware from before the "true UTC" change leaves the whole byte
	// zero (mlat-client likewise only trusts the detail bits when bit 7 is
	// set); a zero byte then means "not reported", not "antenna fault".
	GPSDetail bool
}

// ParseBeastStatus decodes the payload of a Radarcape status frame
// (RawFrame.Data of a FrameStatus frame; at least 3 bytes).
func ParseBeastStatus(data []byte) (BeastStatus, bool) {
	if len(data) < 3 {
		return BeastStatus{}, false
	}
	s, g := data[0], data[2]
	return BeastStatus{
		Settings: s, PPSOffset: int8(data[1]), GPS: g,
		BinaryOutput: s&0x01 != 0, DF17Only: s&0x02 != 0, AVRTimestamps: s&0x04 != 0,
		GPSTimestamps: s&0x10 != 0, FlowControlOff: s&0x20 != 0, FECDisabled: s&0x40 != 0, ModeAC: s&0x80 != 0,
		AntennaOK: g&0x01 != 0, Tracking: g&0x02 != 0, Sats3: g&0x04 != 0, UTC: g&0x08 != 0,
		FPGASync: g&0x10 != 0, TimestampBad: g&0x20 != 0, TrueUTCRange: g&0x80 != 0,
		GPSDetail: g != 0,
	}, true
}

// PPSTickNs is the length of one 1PPS error tick of the status frame. The
// Jetvision Beast documentation ("Mode-S Beast:Data Output Formats", message
// "4") states "time stamp error ticks as int8_t (1 tick is 15ns)".
const PPSTickNs = 15

// PPSTypicalTicks is the 1PPS error the Jetvision documentation calls
// typical ("within about +-3 ticks").
const PPSTypicalTicks = 3

// PPSOffsetNs returns the 1PPS error in nanoseconds (PPSTickNs per tick).
func (s BeastStatus) PPSOffsetNs() float64 { return float64(s.PPSOffset) * PPSTickNs }

// GPSLocked reports whether the receiver's GPS is known to deliver a usable
// time: GPS timestamps selected, antenna fine, at least three satellites
// tracked and the timestamp not flagged as bad. It is false when the
// firmware does not report GPS detail (see GPSDetail).
func (s BeastStatus) GPSLocked() bool {
	return s.GPSTimestamps && s.AntennaOK && s.Sats3 && !s.TimestampBad
}

// gpsVerdict is the one-phrase GPS state of String. The phrases are matched
// by the web UI (web/static/js/pages/timing.js gpsState): "GPS locked",
// "antenna fault", "not valid", "acquiring", "no GPS satellites",
// "not reported"; keep them.
func (s BeastStatus) gpsVerdict() string {
	switch {
	case !s.GPSDetail:
		return "GPS state not reported"
	case !s.AntennaOK:
		return "GPS antenna fault"
	case !s.Tracking:
		return "no GPS satellites"
	case !s.Sats3:
		return "GPS acquiring (<3 satellites)"
	case s.TimestampBad:
		return "GPS time not valid (the Radarcape marks its timestamps as emulated, not GPS-disciplined)"
	case !s.FPGASync:
		return "GPS fix, but the timestamp clock is not synchronised to the 1PPS yet"
	}
	return "GPS locked"
}

// Advice returns what the user should check on the receiver, or "" when the
// status frame shows nothing to act on. It is part of String.
func (s BeastStatus) Advice() string {
	if !s.GPSTimestamps {
		return ""
	}
	const where = "Radarcape web interface, GPS page / systemstatus.json"
	switch {
	case !s.GPSDetail:
		return "the firmware does not report the GPS state in the status frame: check the GPS state on the " + where + " and consider a firmware update"
	case !s.AntennaOK:
		return "check the GPS antenna, its cable and connectors (antenna status on the " + where + ")"
	case !s.Tracking:
		return "no satellite is tracked: check that the GPS antenna is connected and has an open view of the sky"
	case !s.Sats3:
		return "fewer than three satellites: give the GPS antenna a better view of the sky, or wait a few minutes after power-up"
	case s.TimestampBad:
		return "on the " + where + " check the satellites in use, 'Sync' (DPLL locked) and 'TimeUTC'; " +
			"after power-up or a GPS outage this clears by itself within a few minutes once the GPS has a timing fix; " +
			"if it persists with good satellites, restart the Radarcape. Until then its timestamps are not usable for MLAT or latency"
	case !s.FPGASync:
		return "the DPLL of the nanosecond timestamps is not locked ('noSync' on the " + where + "); wait a few minutes after power-up, restart the Radarcape if it persists"
	case !s.UTC:
		return "the Radarcape has not received the GPS-UTC offset yet ('TimeGPS'): timestamps run 18 s ahead of UTC until the GPS almanac arrives (up to 12.5 min)"
	case s.PPSOffset > 2*PPSTypicalTicks || s.PPSOffset < -2*PPSTypicalTicks:
		return fmt.Sprintf("the 1PPS error (%+d ticks) is well outside the typical +-%d ticks: check the GPS antenna placement (multipath) and the GPS page", s.PPSOffset, PPSTypicalTicks)
	}
	return ""
}

// String renders a human-readable summary for FeedStatus.Info: the GPS
// verdict, the raw GPS flags, the 1PPS error and the decoder settings,
// followed by what to check when something is wrong (see Advice).
func (s BeastStatus) String() string {
	var b strings.Builder
	if s.GPSTimestamps {
		b.WriteString("GPS timestamps, ")
		b.WriteString(s.gpsVerdict())
		if s.GPSDetail {
			if s.UTC {
				b.WriteString(", UTC")
			} else {
				b.WriteString(", GPS time base (UTC offset not yet received)")
			}
			if s.FPGASync {
				b.WriteString(", DPLL sync")
			} else {
				b.WriteString(", DPLL no sync")
			}
		}
		fmt.Fprintf(&b, ", 1PPS error %+d ticks (%+.0f ns)", s.PPSOffset, s.PPSOffsetNs())
		fmt.Fprintf(&b, ", status byte 0x%02X", s.GPS)
	} else {
		b.WriteString("12 MHz timestamps")
	}
	if s.DF17Only {
		b.WriteString(", DF11/17/18 only")
	} else {
		b.WriteString(", all DF")
	}
	if s.ModeAC {
		b.WriteString(", Mode A/C on")
	}
	if s.FECDisabled {
		b.WriteString(", FEC off")
	}
	if a := s.Advice(); a != "" {
		b.WriteString(" - check: ")
		b.WriteString(a)
	}
	return b.String()
}

// ReceiverPosition is the receiver's own GPS position as reported by a
// Radarcape position frame (Beast type '5').
type ReceiverPosition struct {
	Lat, Lon float64 // degrees
	AltM     float64 // metres (GPS altitude as reported by the receiver)
}

// ParseBeastPosition decodes the 21-byte payload of a Beast type '5' frame.
// It reports false for non-finite or out-of-range values.
func ParseBeastPosition(p []byte) (ReceiverPosition, bool) {
	if len(p) < 16 {
		return ReceiverPosition{}, false
	}
	lat := float64(math.Float32frombits(binary.LittleEndian.Uint32(p[4:])))
	lon := float64(math.Float32frombits(binary.LittleEndian.Uint32(p[8:])))
	alt := float64(math.Float32frombits(binary.LittleEndian.Uint32(p[12:])))
	if math.IsNaN(lat) || math.IsNaN(lon) || math.IsNaN(alt) || math.IsInf(alt, 0) ||
		lat < -90 || lat > 90 || lon < -180 || lon > 180 || (lat == 0 && lon == 0) {
		return ReceiverPosition{}, false
	}
	return ReceiverPosition{Lat: lat, Lon: lon, AltM: alt}, true
}

// --- timestamp interpretation -------------------------------------------------

// TimestampMode says how the 48-bit receiver timestamps of a stream are to
// be read.
type TimestampMode uint8

const (
	TimestampUnknown TimestampMode = iota // not determined yet: frames carry no receiver time
	Timestamp12MHz                        // free-running 12 MHz counter (classic Beast, dump1090)
	TimestampGPS                          // Radarcape GPS: second-of-day<<30 | nanoseconds
)

func (m TimestampMode) String() string {
	switch m {
	case Timestamp12MHz:
		return "12MHz"
	case TimestampGPS:
		return "gps"
	}
	return "unknown"
}

const (
	detectWindow    = 5 * time.Second // minimum observation time per verdict
	detectMinFrames = 16              // minimum frames per verdict
	nsPerSec        = 1_000_000_000
	secsPerDay      = 86400
)

// Timestamper decides whether a stream's 48-bit timestamps are Radarcape
// GPS timestamps or 12 MHz counter values, converts them, and (for the
// network) classifies the receiver clock and produces unwrapped ticks and
// clock epochs.
//
// A Radarcape status frame is authoritative (SetStatus). Without one the
// mode is inferred statistically over windows of at least 5 s and 16 frames:
// the stream is GPS-timestamped when every frame in the window has a
// nanosecond field below 1e9 and a second-of-day field of at most 86400, and
// the second-of-day field advanced in step with the wall clock (within
// 1.5 s) across the window. A 12 MHz counter fails this: its upper 18 bits
// advance once per 89.5 s. The first verdict is adopted immediately; a later
// contradicting verdict must be repeated in two consecutive windows.
//
// Until the mode is known, Convert reports no timestamp at all rather than
// a possibly wrong one.
//
// # Clock class
//
// The detected class (DetectedClass) follows DESIGN §4.3:
//
//   - 12 MHz with Radarcape status frames: hw12; 12 MHz detected
//     statistically: sdr12 (the conservative choice, since the server may
//     downgrade but never upgrade a class; configure hw12 for a Mode-S
//     Beast).
//   - GPS with status frames: gps_utc when the GPS status reports lock, UTC
//     time base and FPGA sync; gps_gpst when locked with the UTC bit clear;
//     gps_emul otherwise (no lock, timestamp flagged bad, FPGA not synced,
//     or firmware that does not report GPS detail). Losing lock takes
//     effect at once; regaining it needs GPSRegainStatuses consecutive good
//     status frames so a flapping receiver does not churn epochs.
//   - GPS detected statistically (no status frames): gps_emul.
//
// A class override (SetClassOverride) replaces the detected class when it
// uses the same tick unit (any GPS class for a GPS stream, any 12 MHz class
// for a 12 MHz stream) or is ClockNone; otherwise it is ignored.
//
// # Ticks and epochs
//
// Stamp turns a timestamp into ticks of the current epoch through a
// ClockTracker: GPS timestamps become dayIndex*NsPerDay + nsOfDay (after the
// legacy 1..86400 second-of-day correction of Convert, with the day rolling
// over at midnight), 12 MHz timestamps the unwrapped counter. Every class
// change and every step starts a new epoch, reported to OnClockEvent.
type Timestamper struct {
	// OnClockEvent, when set, receives every new clock epoch before the
	// first frame stamped with it.
	OnClockEvent func(ClockEvent)

	mode       TimestampMode
	haveStatus bool
	legacyDay  bool // second-of-day runs 1..86400 (status frame said so)

	// current observation window
	n         int
	start     time.Time
	firstSecs uint64
	lastSecs  uint64
	plausible bool
	pending   TimestampMode // contradicting verdict awaiting confirmation

	// clock class and epochs
	clk         ClockTracker
	override    model.ClockClass
	hasOverride bool
	gpsCls      model.ClockClass // GPS class from status frames (with hysteresis); ClockNone = none yet
	gpsGood     int              // consecutive status frames reporting a good GPS time
}

// GPSRegainStatuses is the number of consecutive good GPS status frames
// (one per second) needed before a receiver that lost GPS lock is trusted
// as GPS-disciplined again.
const GPSRegainStatuses = 3

// Mode returns the current interpretation.
func (t *Timestamper) Mode() TimestampMode { return t.mode }

// SetStatus applies a Radarcape status frame, which states the timestamp
// mode, the second-of-day range and the GPS state explicitly.
func (t *Timestamper) SetStatus(s BeastStatus) {
	first := !t.haveStatus
	t.haveStatus = true
	prevMode, prevLegacy := t.mode, t.legacyDay
	if s.GPSTimestamps {
		t.mode = TimestampGPS
	} else {
		t.mode = Timestamp12MHz
	}
	t.legacyDay = !s.TrueUTCRange

	detail := ClockDetailClassChange
	if t.mode == TimestampGPS {
		want := gpsClassFromStatus(s)
		switch {
		case want == model.ClockGPSEmul:
			if t.gpsCls == model.ClockGPSUTC || t.gpsCls == model.ClockGPSGPST {
				detail = ClockDetailGPSLost
			}
			t.gpsCls, t.gpsGood = want, 0
		default:
			t.gpsGood++
			switch t.gpsCls {
			case model.ClockNone:
				t.gpsCls = want
			case model.ClockGPSEmul:
				if t.gpsGood >= GPSRegainStatuses {
					t.gpsCls, detail = want, ClockDetailGPSRegained
				}
			default:
				if t.gpsCls != want {
					t.gpsCls, detail = want, ClockDetailTimeBase
				}
			}
		}
		if !first && prevMode == TimestampGPS && prevLegacy != t.legacyDay {
			// Same class, but every timestamp moved by one second.
			t.clk.ModeChange(ClockDetailDayRange)
		}
	}
	t.updateClass(detail)
}

// gpsClassFromStatus maps the GPS bits of a status frame to a clock class.
func gpsClassFromStatus(s BeastStatus) model.ClockClass {
	if !s.GPSDetail || !s.GPSLocked() || !s.FPGASync {
		return model.ClockGPSEmul
	}
	if s.UTC {
		return model.ClockGPSUTC
	}
	return model.ClockGPSGPST
}

// Observe feeds one off-air frame timestamp (not a synthetic MLAT one) and
// its host receive time into the statistical detector. It reports true when
// the mode changed.
func (t *Timestamper) Observe(ts uint64, recv time.Time) bool {
	if t.haveStatus {
		return false
	}
	secs, nanos := ts>>30, ts&0x3FFFFFFF
	if t.n == 0 {
		t.start, t.firstSecs, t.plausible = recv, secs, true
	}
	t.n++
	t.lastSecs = secs
	if nanos >= nsPerSec || secs > secsPerDay {
		t.plausible = false
	}
	elapsed := recv.Sub(t.start)
	if elapsed < 0 { // host clock stepped backwards: start over
		t.n = 0
		return false
	}
	if elapsed < detectWindow || t.n < detectMinFrames {
		return false
	}

	verdict := Timestamp12MHz
	if t.plausible {
		adv := float64((t.lastSecs + secsPerDay - t.firstSecs) % secsPerDay)
		if math.Abs(adv-elapsed.Seconds()) <= 1.5 {
			verdict = TimestampGPS
		}
	}
	t.n = 0

	switch {
	case verdict == t.mode:
		t.pending = TimestampUnknown
	case t.mode == TimestampUnknown, verdict == t.pending:
		t.mode = verdict
		t.pending = TimestampUnknown
		t.updateClass(ClockDetailClassChange)
		return true
	default:
		t.pending = verdict
	}
	return false
}

// Convert turns a raw 48-bit timestamp into the model representation:
// gpsNs is nanoseconds since UTC midnight or -1, clock is the 12 MHz
// counter value or 0.
func (t *Timestamper) Convert(ts uint64) (gpsNs int64, clock uint64) {
	switch t.mode {
	case TimestampGPS:
		secs, nanos := ts>>30, ts&0x3FFFFFFF
		if nanos >= nsPerSec || secs > secsPerDay {
			return -1, 0 // not a valid GPS timestamp (corrupt frame)
		}
		if t.legacyDay {
			// Legacy firmware counts the second of the day 1..86400.
			if secs == 0 {
				secs = secsPerDay - 1
			} else {
				secs--
			}
		} else if secs == secsPerDay {
			return -1, 0
		}
		return int64(secs)*nsPerSec + int64(nanos), 0
	case Timestamp12MHz:
		return -1, ts
	}
	return -1, 0
}

// DetectedClass returns the clock class detected from the stream (see the
// type documentation), ignoring any override.
func (t *Timestamper) DetectedClass() model.ClockClass {
	switch t.mode {
	case TimestampGPS:
		if t.haveStatus && t.gpsCls != model.ClockNone {
			return t.gpsCls
		}
		return model.ClockGPSEmul
	case Timestamp12MHz:
		if t.haveStatus {
			return model.ClockHW12
		}
		return model.ClockSDR12
	}
	return model.ClockNone
}

// Class returns the effective clock class frames are stamped with: the
// override when one is set and applicable, else the detected class.
func (t *Timestamper) Class() model.ClockClass {
	det := t.DetectedClass()
	if t.hasOverride && OverrideApplies(t.override, det) {
		return t.override
	}
	return det
}

// OverrideApplies reports whether a configured class override can replace
// the detected class det: ClockNone always can; otherwise both must be
// timed classes with the same tick rate.
func OverrideApplies(override, det model.ClockClass) bool {
	if override == model.ClockNone {
		return true
	}
	return override.Timed() && det.Timed() && override.NominalHz() == det.NominalHz()
}

// SetClassOverride sets (set=true) or clears the configured clock class
// (model.Feed.ClockClass). A change of the effective class starts a new
// epoch.
func (t *Timestamper) SetClassOverride(cls model.ClockClass, set bool) {
	t.override, t.hasOverride = cls, set
	t.updateClass(ClockDetailOverride)
}

// ClassOverride returns the configured override and whether one is set.
func (t *Timestamper) ClassOverride() (model.ClockClass, bool) { return t.override, t.hasOverride }

func (t *Timestamper) updateClass(detail string) {
	t.clk.SetClass(t.Class(), detail)
}

// SetEpochBase continues the epoch numbering of a previous connection of
// the same receiver: the first epoch becomes prev+1 and is reported as a
// reconnect (reason restart). Call it before the first frame.
func (t *Timestamper) SetEpochBase(prev uint32) { t.clk.SetEpochBase(prev) }

// Epoch returns the current clock epoch (0 = none yet).
func (t *Timestamper) Epoch() uint32 { return t.clk.Epoch() }

// LastTicks returns the ticks of the newest accepted frame of the open
// epoch (0 when no epoch is open).
func (t *Timestamper) LastTicks() uint64 { return t.clk.LastTicks() }

// Clock gives access to the underlying ClockTracker (tunables and
// counters). Do not call its SetClass; use SetClassOverride.
func (t *Timestamper) Clock() *ClockTracker { return &t.clk }

// Stamp turns a raw 48-bit timestamp received at hostNs (host UTC, Unix ns)
// into the receiver clock class, epoch and unwrapped ticks. ok is false
// when the frame carries no usable receiver time: mode not known yet, class
// none, an invalid timestamp, or a frame of an unconfirmed step. MLAT magic
// and all-zero timestamps must not be passed in.
func (t *Timestamper) Stamp(ts uint64, hostNs int64) (cls model.ClockClass, epoch uint32, ticks uint64, ok bool) {
	t.clk.OnEvent = t.OnClockEvent
	cls = t.clk.Class()
	if !cls.Timed() {
		return model.ClockNone, 0, 0, false
	}
	var raw uint64
	switch t.mode {
	case TimestampGPS:
		ns, _ := t.Convert(ts)
		if ns < 0 {
			return model.ClockNone, 0, 0, false
		}
		raw = uint64(ns)
	case Timestamp12MHz:
		raw = ts
	default:
		return model.ClockNone, 0, 0, false
	}
	ticks, ok = t.clk.Stamp(raw, hostNs)
	if !ok {
		return model.ClockNone, 0, 0, false
	}
	return cls, t.clk.Epoch(), ticks, true
}

// --- Beast frame -> model.RawFrame --------------------------------------------

// BeastDecoder turns parsed Beast frames into model.RawFrame values and
// delivers them to a sink. It owns the stream's Timestamper.
//
// Besides the legacy GPSNs / Clock12MHz fields it fills the network clock
// fields of every frame: HostNs (from Recv), and for frames with a usable
// receiver time ClockCls, Epoch and Ticks (see Timestamper.Stamp). Set
// TS.OnClockEvent to learn about new epochs; events arrive before the
// first frame of the epoch is delivered to Sink. MLAT results and frames
// with an all-zero timestamp are never timed.
//
// Usage: create one per connection, set Recv before every chunk handed to
// the BeastParser whose emit function is Decoder.Handle.
type BeastDecoder struct {
	Sink     model.FrameSink
	FeedID   string
	MLATOnly bool // mark every frame as an MLAT result
	// Recv is stamped on every frame; set it once per socket read.
	Recv time.Time
	// OnStatus, when set, is called when the Radarcape status changes
	// (the first status frame and every change of its three bytes).
	OnStatus func(BeastStatus)
	// OnPosition, when set, is called for every valid receiver position frame.
	OnPosition func(ReceiverPosition)
	// OnMode, when set, is called when the timestamp mode changes.
	OnMode func(TimestampMode)

	TS         Timestamper
	lastStatus [3]byte
	hasStatus  bool
}

// Handle processes one parsed frame (use as the BeastParser emit function).
func (d *BeastDecoder) Handle(fr *BeastFrame) {
	f := model.RawFrame{
		Recv:    d.Recv,
		GPSNs:   -1,
		FeedID:  d.FeedID,
		Channel: -1,
		MLAT:    d.MLATOnly,
	}
	switch fr.Type {
	case BeastModeAC:
		f.Kind = model.FrameModeAC
	case BeastModeSShort:
		f.Kind = model.FrameModeSShort
	case BeastModeSLong:
		f.Kind = model.FrameModeSLong
	case BeastStatusType:
		d.handleStatus(fr)
		return
	case BeastPosition:
		if pos, ok := ParseBeastPosition(fr.Payload); ok && d.OnPosition != nil {
			d.OnPosition(pos)
		}
		return
	default:
		return
	}

	f.Signal = BeastSignalDBFS(fr.Signal)
	switch {
	case IsMLATTimestamp(fr.Timestamp):
		f.MLAT = true
	case fr.Timestamp == 0:
		// An all-zero timestamp is what relays write when they have none;
		// it is never reported as "GPS midnight".
	default:
		if d.TS.Observe(fr.Timestamp, d.Recv) && d.OnMode != nil {
			d.OnMode(d.TS.Mode())
		}
		f.GPSNs, f.Clock12MHz = d.TS.Convert(fr.Timestamp)
		if !f.MLAT {
			stampFrame(&d.TS, &f, fr.Timestamp)
		}
	}
	if f.HostNs == 0 && !f.Recv.IsZero() {
		f.HostNs = f.Recv.UnixNano()
	}
	f.Data = make([]byte, len(fr.Payload))
	copy(f.Data, fr.Payload)
	d.Sink.HandleFrame(f)
}

func (d *BeastDecoder) handleStatus(fr *BeastFrame) {
	st, ok := ParseBeastStatus(fr.Payload)
	if !ok {
		return
	}
	before := d.TS.Mode()
	d.TS.SetStatus(st)
	if d.TS.Mode() != before && d.OnMode != nil {
		d.OnMode(d.TS.Mode())
	}
	key := [3]byte{st.Settings, byte(st.PPSOffset), st.GPS}
	if !d.hasStatus || key != d.lastStatus {
		d.hasStatus, d.lastStatus = true, key
		if d.OnStatus != nil {
			d.OnStatus(st)
		}
	}

	f := model.RawFrame{
		Recv:    d.Recv,
		Signal:  math.NaN(),
		Kind:    model.FrameStatus,
		FeedID:  d.FeedID,
		Channel: -1,
	}
	f.GPSNs, f.Clock12MHz = d.TS.Convert(fr.Timestamp)
	if fr.Timestamp != 0 {
		stampFrame(&d.TS, &f, fr.Timestamp)
	}
	if f.HostNs == 0 && !f.Recv.IsZero() {
		f.HostNs = f.Recv.UnixNano()
	}
	f.Data = make([]byte, len(fr.Payload))
	copy(f.Data, fr.Payload)
	d.Sink.HandleFrame(f)
}

// stampFrame fills the network clock fields (HostNs, ClockCls, Epoch,
// Ticks) of f from the raw 48-bit timestamp ts. Frames without a host
// receive time stay untimed.
func stampFrame(ts *Timestamper, f *model.RawFrame, raw uint64) {
	if f.Recv.IsZero() {
		return
	}
	f.HostNs = f.Recv.UnixNano()
	if cls, ep, ticks, ok := ts.Stamp(raw, f.HostNs); ok {
		f.ClockCls, f.Epoch, f.Ticks = cls, ep, ticks
	}
}

// AppendBeastFrame appends one Beast binary frame (with escaping) to dst.
// typ is the frame type byte, ts the 48-bit timestamp, sig the signal byte.
// For BeastPosition, ts and sig are ignored. It is the encoder matching
// BeastParser, exported for the simulator and tests.
func AppendBeastFrame(dst []byte, typ byte, ts uint64, sig byte, payload []byte) []byte {
	dst = append(dst, beastEsc, typ)
	esc := func(b byte) {
		dst = append(dst, b)
		if b == beastEsc {
			dst = append(dst, beastEsc)
		}
	}
	if typ != BeastPosition {
		for shift := 40; shift >= 0; shift -= 8 {
			esc(byte(ts >> uint(shift)))
		}
		esc(sig)
	}
	for _, b := range payload {
		esc(b)
	}
	return dst
}

// GPSTimestamp builds a Radarcape GPS-mode 48-bit timestamp from a
// second-of-day (0..86399) and nanoseconds (0..999999999).
func GPSTimestamp(secOfDay int, nanos int) uint64 {
	return uint64(secOfDay)<<30 | uint64(nanos)&0x3FFFFFFF
}
