package federation

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"airview/internal/avf"
	"airview/internal/model"
	"airview/internal/privacy/publicdto"
)

// AVFED/1 record layer (PROTOCOL.md §4). Records travel inside one raw
// DEFLATE stream per direction (avf.StreamWriter / avf.StreamReader) as
// "u8 type | uvarint len | payload", the framing of AVF/1. Both directions
// use the same type codes; unknown types are skipped by length.

// RecordType is an AVFED/1 record type.
type RecordType uint8

// Record types.
const (
	TInfo    RecordType = 0x20 // JSON Info, once after the handshake (and on change)
	TState   RecordType = 0x21 // aircraft state batch
	TMLAT    RecordType = 0x22 // MLAT result batch
	TPing    RecordType = 0x30 // u64 id | i64 sendNs
	TPong    RecordType = 0x31 // u64 id | i64 echoed sendNs
	TGoodbye RecordType = 0x3F // JSON Goodbye
)

func (t RecordType) known() bool {
	switch t {
	case TInfo, TState, TMLAT, TPing, TPong, TGoodbye:
		return true
	}
	return false
}

func (t RecordType) control() bool { return t == TPing || t == TPong || t == TGoodbye || t == TInfo }

// String names the record type.
func (t RecordType) String() string {
	switch t {
	case TInfo:
		return "INFO"
	case TState:
		return "STATE"
	case TMLAT:
		return "MLAT"
	case TPing:
		return "PING"
	case TPong:
		return "PONG"
	case TGoodbye:
		return "GOODBYE"
	}
	return fmt.Sprintf("0x%02X", uint8(t))
}

// Limits of the record layer.
const (
	MaxRecordLen   = avf.MaxRecordLen // 64 KiB per record payload
	MaxPathLen     = 16               // path entries per record
	MaxItems       = 4096             // aircraft or fixes per record
	maxStr         = 64               // string field bytes
	targetRecBytes = 48 << 10         // encoder splits batches near this size
)

// ErrProtocol is wrapped by every malformed-input error of this package.
var ErrProtocol = errors.New("federation: protocol error")

func perr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
}

// Info is the INFO record: what the sender will send on this link. It is
// informational (status page); the receiver enforces its own policy.
type Info struct {
	Name       string `json:"name,omitempty"`
	Software   string `json:"software"`
	Version    string `json:"version,omitempty"`
	Aircraft   string `json:"aircraft"`
	MLAT       bool   `json:"mlat"`
	Transit    bool   `json:"transit"`
	IntervalMs int    `json:"intervalMs"`
	DelayS     int    `json:"delayS,omitempty"`
}

// Goodbye is the GOODBYE record sent before a deliberate close.
type Goodbye struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
}

// Goodbye codes.
const (
	ByeShutdown  = "shutting_down"
	ByeDuplicate = "duplicate" // a second link to the same peer lost the tie-break
	ByeProtocol  = "protocol_error"
	ByeDisabled  = "disabled"
)

// ---------------------------------------------------------------- framing

// appendRecord appends "u8 type | uvarint len | payload".
func appendRecord(dst []byte, t RecordType, payload []byte) []byte {
	dst = append(dst, byte(t))
	dst = binary.AppendUvarint(dst, uint64(len(payload)))
	return append(dst, payload...)
}

// recordReader reads records from the inflated stream.
type recordReader struct {
	r       *bufio.Reader
	buf     []byte
	skipped uint64
}

func newRecordReader(r io.Reader) *recordReader {
	return &recordReader{r: bufio.NewReaderSize(r, 32<<10)}
}

// next returns the next known record; the payload is valid until the next
// call.
func (rr *recordReader) next() (RecordType, []byte, error) {
	for {
		tb, err := rr.r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n, err := binary.ReadUvarint(rr.r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return 0, nil, err
		}
		if n > MaxRecordLen {
			return 0, nil, perr("record 0x%02X of %d bytes", tb, n)
		}
		t := RecordType(tb)
		if !t.known() {
			if _, err := rr.r.Discard(int(n)); err != nil {
				return 0, nil, io.ErrUnexpectedEOF
			}
			rr.skipped++
			continue
		}
		if cap(rr.buf) < int(n) {
			rr.buf = make([]byte, n, max(int(n), 4096))
		}
		rr.buf = rr.buf[:n]
		if _, err := io.ReadFull(rr.r, rr.buf); err != nil {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return t, rr.buf, nil
	}
}

// ---------------------------------------------------------------- decoder

type dec struct {
	b   []byte
	err error
}

func (d *dec) fail(what string) {
	if d.err == nil {
		d.err = perr("truncated or invalid %s", what)
	}
}

func (d *dec) u8() uint8 {
	if d.err != nil || len(d.b) < 1 {
		d.fail("u8")
		return 0
	}
	v := d.b[0]
	d.b = d.b[1:]
	return v
}

func (d *dec) u32() uint32 {
	if d.err != nil || len(d.b) < 4 {
		d.fail("u32")
		return 0
	}
	v := binary.BigEndian.Uint32(d.b)
	d.b = d.b[4:]
	return v
}

func (d *dec) u64() uint64 {
	if d.err != nil || len(d.b) < 8 {
		d.fail("u64")
		return 0
	}
	v := binary.BigEndian.Uint64(d.b)
	d.b = d.b[8:]
	return v
}

func (d *dec) uv() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.fail("uvarint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *dec) sv() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.fail("varint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *dec) str() string {
	n := d.uv()
	if d.err != nil {
		return ""
	}
	if n > maxStr || uint64(len(d.b)) < n {
		d.fail("string")
		return ""
	}
	s := d.b[:n]
	d.b = d.b[n:]
	return cleanStr(s)
}

// cleanStr keeps printable characters of valid UTF-8 only.
func cleanStr(b []byte) string {
	ok := utf8.Valid(b)
	if ok {
		for _, c := range b {
			if c < 0x20 || c == 0x7F {
				ok = false
				break
			}
		}
	}
	if ok {
		return string(b)
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F || r == utf8.RuneError {
			return -1
		}
		return r
	}, string(b))
}

func appendStr(dst []byte, s string) []byte {
	if len(s) > maxStr {
		s = s[:maxStr]
	}
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// ---------------------------------------------------------------- path

// appendPath appends "u8 n | n × 16-byte node IDs".
func appendPath(dst []byte, path []string) []byte {
	dst = append(dst, byte(len(path)))
	for _, id := range path {
		var b [model.IDLen]byte
		copy(b[:], id)
		dst = append(dst, b[:]...)
	}
	return dst
}

func (d *dec) path() []string {
	n := int(d.u8())
	if d.err != nil {
		return nil
	}
	if n < 1 || n > MaxPathLen || len(d.b) < n*model.IDLen {
		d.fail("path")
		return nil
	}
	out := make([]string, n)
	for i := range out {
		id, ok := model.CanonicalID(string(d.b[:model.IDLen]))
		if !ok {
			d.fail("path node ID")
			return nil
		}
		out[i] = id
		d.b = d.b[model.IDLen:]
	}
	return out
}

// ---------------------------------------------------------------- STATE

// StateBatch is a decoded STATE record.
type StateBatch struct {
	// TMs is the sender's clock (Unix ms) at the snapshot; every time in
	// Aircraft is in the sender's clock and should be rebased by the
	// receiver (store.go does: local now − TMs).
	TMs int64
	// Full marks a complete snapshot (the sender's full refresh).
	Full bool
	// Path is the origin network first, the sending network last.
	Path []string
	// Aircraft are the items. PosAge is seconds relative to TMs.
	Aircraft []publicdto.Aircraft
}

// Field presence bits of a STATE item.
const (
	fCallsign = 1 << iota
	fSquawk
	fCategory
	fAddrType
	fPos
	fAltBaro
	fAltGeom
	fVertRate
	fGS
	fTrack
	fIAS
	fTAS
	fMach
	fMagHdg
	fRoll
	fTrackRate
	fSelHdg
	fSelAlt
	fQNH
	fWind
	fOAT
	fVersion
	fNIC
	fNACp
	fNACv
	fSIL
	fRc
	fEmergency
	fNav
	fMLAT
	fOnGround
	fFlags
)

// Item flag bits (fFlags).
const (
	iflAlert    = 1 << 0
	iflSPI      = 1 << 1
	iflMilitary = 1 << 2
	iflCoarse   = 1 << 3
)

var sources = []string{"", model.SrcADSB, model.SrcMLAT, model.SrcTISB, model.SrcADSR, model.SrcModeS, model.SrcOther}

func sourceCode(s string) byte {
	for i, v := range sources {
		if v == s {
			return byte(i)
		}
	}
	return byte(len(sources) - 1) // SrcOther
}

// navNames are the navigation modes of model.Aircraft.NavModes; unknown
// names are not transmitted.
var navNames = []string{"autopilot", "vnav", "althold", "approach", "lnav", "tcas"}

// Quantisation helpers. Encoders round; decoders return float64 values.
func q(v, scale float64) int64 { return int64(math.Round(v * scale)) }

func finite(p *float64) bool { return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) }

func fptr(v float64) *float64 { return &v }

func iptr(v int) *int { return &v }

func uq(v, scale float64) uint64 {
	if v <= 0 || math.IsNaN(v) {
		return 0
	}
	return uint64(math.Round(v * scale))
}

func small(p *int) bool { return p != nil && *p >= 0 && *p <= 254 }

// AppendState appends one STATE payload carrying items aircraft[from:to]
// and returns it with the index after the last item written. It stops near
// targetRecBytes so the payload never exceeds MaxRecordLen.
func AppendState(dst []byte, tMs int64, full bool, path []string, ac []publicdto.Aircraft) ([]byte, int) {
	start := len(dst)
	dst = binary.AppendUvarint(dst, uint64(max(tMs, 0)))
	var fl byte
	if full {
		fl = 1
	}
	dst = append(dst, fl)
	dst = appendPath(dst, path)
	// Count placeholder: the count is a uvarint, written after the items
	// into a fixed 2-byte slot (MaxItems < 16384 fits two uvarint bytes).
	cnt := len(dst)
	dst = append(dst, 0x80, 0x00)
	n := 0
	for n < len(ac) && n < MaxItems && len(dst)-start < targetRecBytes {
		dst = appendItem(dst, tMs, &ac[n])
		n++
	}
	dst[cnt] = byte(n&0x7F) | 0x80
	dst[cnt+1] = byte(n >> 7)
	return dst, n
}

func appendItem(dst []byte, tMs int64, a *publicdto.Aircraft) []byte {
	addr, nonICAO, _ := avf.ParseICAO(a.ICAO)
	w := addr & 0xFFFFFF
	if nonICAO {
		w |= 1 << 24
	}
	dst = binary.BigEndian.AppendUint32(dst, w)
	var m uint64
	set := func(b uint64, ok bool) {
		if ok {
			m |= b
		}
	}
	hasPos := finite(a.Lat) && finite(a.Lon) && *a.Lat >= -90 && *a.Lat <= 90 && *a.Lon >= -180 && *a.Lon <= 180
	set(fCallsign, a.Callsign != "")
	set(fSquawk, a.Squawk != "")
	set(fCategory, a.Category != "")
	set(fAddrType, a.AddrType != "")
	set(fPos, hasPos)
	set(fAltBaro, finite(a.AltBaro))
	set(fAltGeom, finite(a.AltGeom))
	set(fVertRate, finite(a.VertRate))
	set(fGS, finite(a.GS))
	set(fTrack, finite(a.Track))
	set(fIAS, finite(a.IAS))
	set(fTAS, finite(a.TAS))
	set(fMach, finite(a.Mach))
	set(fMagHdg, finite(a.MagHeading))
	set(fRoll, finite(a.Roll))
	set(fTrackRate, finite(a.TrackRate))
	set(fSelHdg, finite(a.SelHeading))
	set(fSelAlt, finite(a.SelAlt))
	set(fQNH, finite(a.QNH))
	set(fWind, finite(a.WindDir) && finite(a.WindSpd))
	set(fOAT, finite(a.OAT))
	set(fVersion, small(a.ADSBVersion))
	set(fNIC, small(a.NIC))
	set(fNACp, small(a.NACp))
	set(fNACv, small(a.NACv))
	set(fSIL, small(a.SIL))
	set(fRc, finite(a.RcM))
	set(fEmergency, a.Emergency != "")
	var nav uint64
	for _, s := range a.NavModes {
		for i, n := range navNames {
			if s == n {
				nav |= 1 << i
			}
		}
	}
	set(fNav, nav != 0)
	set(fMLAT, a.MLATGrade != "" || finite(a.MLATSigmaM))
	set(fOnGround, a.OnGround != nil)
	var flags byte
	if a.Alert {
		flags |= iflAlert
	}
	if a.SPI {
		flags |= iflSPI
	}
	if a.Military {
		flags |= iflMilitary
	}
	if a.Coarse {
		flags |= iflCoarse
	}
	set(fFlags, flags != 0)
	dst = binary.AppendUvarint(dst, m)
	dst = append(dst, sourceCode(a.Source))
	lastAge := max(tMs-a.LastSeen, 0)
	if a.LastSeen <= 0 {
		lastAge = 0
	}
	dst = binary.AppendUvarint(dst, uint64(lastAge))
	firstAge := int64(0)
	if a.FirstSeen > 0 && a.LastSeen >= a.FirstSeen {
		firstAge = (a.LastSeen - a.FirstSeen) / 1000
	}
	dst = binary.AppendUvarint(dst, uint64(firstAge))
	if m&fCallsign != 0 {
		dst = appendStr(dst, a.Callsign)
	}
	if m&fSquawk != 0 {
		dst = appendStr(dst, a.Squawk)
	}
	if m&fCategory != 0 {
		dst = appendStr(dst, a.Category)
	}
	if m&fAddrType != 0 {
		dst = appendStr(dst, a.AddrType)
	}
	if m&fPos != 0 {
		dst = binary.BigEndian.AppendUint32(dst, uint32(int32(q(*a.Lat, 1e7))))
		dst = binary.BigEndian.AppendUint32(dst, uint32(int32(q(*a.Lon, 1e7))))
		age := 0.0
		if finite(a.PosAge) {
			age = *a.PosAge
		}
		dst = binary.AppendUvarint(dst, uq(age, 1000))
	}
	sv := func(bit uint64, p *float64, scale float64) {
		if m&bit != 0 {
			dst = binary.AppendVarint(dst, q(*p, scale))
		}
	}
	uvf := func(bit uint64, p *float64, scale float64) {
		if m&bit != 0 {
			dst = binary.AppendUvarint(dst, uq(*p, scale))
		}
	}
	sv(fAltBaro, a.AltBaro, 1)
	sv(fAltGeom, a.AltGeom, 1)
	sv(fVertRate, a.VertRate, 1)
	uvf(fGS, a.GS, 10)
	uvf(fTrack, a.Track, 100)
	uvf(fIAS, a.IAS, 1)
	uvf(fTAS, a.TAS, 1)
	uvf(fMach, a.Mach, 1000)
	uvf(fMagHdg, a.MagHeading, 100)
	sv(fRoll, a.Roll, 100)
	sv(fTrackRate, a.TrackRate, 100)
	uvf(fSelHdg, a.SelHeading, 100)
	sv(fSelAlt, a.SelAlt, 1)
	uvf(fQNH, a.QNH, 10)
	if m&fWind != 0 {
		dst = binary.AppendUvarint(dst, uq(*a.WindDir, 10))
		dst = binary.AppendUvarint(dst, uq(*a.WindSpd, 10))
	}
	sv(fOAT, a.OAT, 10)
	for _, f := range []struct {
		bit uint64
		p   *int
	}{{fVersion, a.ADSBVersion}, {fNIC, a.NIC}, {fNACp, a.NACp}, {fNACv, a.NACv}, {fSIL, a.SIL}} {
		if m&f.bit != 0 {
			dst = append(dst, byte(*f.p))
		}
	}
	uvf(fRc, a.RcM, 1)
	if m&fEmergency != 0 {
		dst = appendStr(dst, a.Emergency)
	}
	if m&fNav != 0 {
		dst = binary.AppendUvarint(dst, nav)
	}
	if m&fMLAT != 0 {
		dst = appendStr(dst, a.MLATGrade)
		var s uint64
		if finite(a.MLATSigmaM) {
			s = uq(*a.MLATSigmaM, 1) + 1
		}
		dst = binary.AppendUvarint(dst, s)
	}
	if m&fOnGround != 0 {
		var g byte
		if *a.OnGround {
			g = 1
		}
		dst = append(dst, g)
	}
	if m&fFlags != 0 {
		dst = append(dst, flags)
	}
	return dst
}

// DecodeState decodes a STATE payload. into is reused for the items.
func DecodeState(p []byte, into []publicdto.Aircraft) (StateBatch, error) {
	d := dec{b: p}
	var sb StateBatch
	sb.TMs = int64(d.uv())
	sb.Full = d.u8()&1 != 0
	sb.Path = d.path()
	n := d.uv()
	if d.err != nil {
		return sb, d.err
	}
	if n > MaxItems || sb.TMs < 0 {
		return sb, perr("STATE count %d", n)
	}
	sb.Aircraft = into[:0]
	for i := uint64(0); i < n; i++ {
		var a publicdto.Aircraft
		decodeItem(&d, sb.TMs, &a)
		if d.err != nil {
			return sb, d.err
		}
		sb.Aircraft = append(sb.Aircraft, a)
	}
	return sb, nil
}

func decodeItem(d *dec, tMs int64, a *publicdto.Aircraft) {
	w := d.u32()
	a.ICAO = avf.FormatICAO(w&0xFFFFFF, w&(1<<24) != 0)
	m := d.uv()
	src := int(d.u8())
	if src < len(sources) {
		a.Source = sources[src]
	} else {
		a.Source = model.SrcOther
	}
	lastAge := int64(min(d.uv(), 1<<40))
	firstAge := int64(min(d.uv(), 1<<30))
	a.LastSeen = tMs - lastAge
	a.FirstSeen = a.LastSeen - firstAge*1000
	if m&fCallsign != 0 {
		a.Callsign = d.str()
	}
	if m&fSquawk != 0 {
		a.Squawk = d.str()
	}
	if m&fCategory != 0 {
		a.Category = d.str()
	}
	if m&fAddrType != 0 {
		a.AddrType = d.str()
	}
	if m&fPos != 0 {
		lat := float64(int32(d.u32())) / 1e7
		lon := float64(int32(d.u32())) / 1e7
		age := float64(d.uv()) / 1000
		if lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 {
			a.Lat, a.Lon, a.PosAge = fptr(lat), fptr(lon), fptr(age)
		}
	}
	sv := func(bit uint64, scale float64) *float64 {
		if m&bit == 0 {
			return nil
		}
		return fptr(float64(d.sv()) / scale)
	}
	uvf := func(bit uint64, scale float64) *float64 {
		if m&bit == 0 {
			return nil
		}
		return fptr(float64(d.uv()) / scale)
	}
	a.AltBaro = sv(fAltBaro, 1)
	a.AltGeom = sv(fAltGeom, 1)
	a.VertRate = sv(fVertRate, 1)
	a.GS = uvf(fGS, 10)
	a.Track = uvf(fTrack, 100)
	a.IAS = uvf(fIAS, 1)
	a.TAS = uvf(fTAS, 1)
	a.Mach = uvf(fMach, 1000)
	a.MagHeading = uvf(fMagHdg, 100)
	a.Roll = sv(fRoll, 100)
	a.TrackRate = sv(fTrackRate, 100)
	a.SelHeading = uvf(fSelHdg, 100)
	a.SelAlt = sv(fSelAlt, 1)
	a.QNH = uvf(fQNH, 10)
	if m&fWind != 0 {
		a.WindDir = fptr(float64(d.uv()) / 10)
		a.WindSpd = fptr(float64(d.uv()) / 10)
	}
	a.OAT = sv(fOAT, 10)
	u8p := func(bit uint64) *int {
		if m&bit == 0 {
			return nil
		}
		return iptr(int(d.u8()))
	}
	a.ADSBVersion = u8p(fVersion)
	a.NIC = u8p(fNIC)
	a.NACp = u8p(fNACp)
	a.NACv = u8p(fNACv)
	a.SIL = u8p(fSIL)
	a.RcM = uvf(fRc, 1)
	if m&fEmergency != 0 {
		a.Emergency = d.str()
	}
	if m&fNav != 0 {
		nav := d.uv()
		for i, n := range navNames {
			if nav&(1<<i) != 0 {
				a.NavModes = append(a.NavModes, n)
			}
		}
	}
	if m&fMLAT != 0 {
		a.MLATGrade = d.str()
		if s := d.uv(); s > 0 {
			a.MLATSigmaM = fptr(float64(s - 1))
		}
	}
	if m&fOnGround != 0 {
		g := d.u8() != 0
		a.OnGround = &g
	}
	if m&fFlags != 0 {
		fl := d.u8()
		a.Alert = fl&iflAlert != 0
		a.SPI = fl&iflSPI != 0
		a.Military = fl&iflMilitary != 0
		a.Coarse = fl&iflCoarse != 0
	}
}

// ---------------------------------------------------------------- MLAT

// Fix is one MLAT result as exchanged between federated networks. It
// carries no station identities and no station count beyond the bucket of
// model.MLATFix, and its time has millisecond precision.
type Fix struct {
	ICAO    uint32  // 24-bit address
	TMs     int64   // transmit time, Unix ms (sender clock; see MLATBatch)
	Lat     float64 // WGS-84 degrees
	Lon     float64
	AltM    float64 // height above the ellipsoid, m
	SigmaHM float64 // 1-sigma horizontal uncertainty, m
	SigmaVM float64 // 1-sigma vertical uncertainty, m
	Grade   uint8   // model.Grade*
	NBucket uint8   // model.NBucket*
	Flags   uint8   // model.FixFlag*
	// Vel is the ENU velocity in m/s, nil when unknown.
	Vel *[3]float32
}

// MLATBatch is a decoded MLAT record.
type MLATBatch struct {
	TMs   int64
	Path  []string
	Fixes []Fix
}

// AppendMLAT appends one MLAT payload with fixes[0:n] and returns n.
func AppendMLAT(dst []byte, tMs int64, path []string, fixes []Fix) ([]byte, int) {
	start := len(dst)
	dst = binary.AppendUvarint(dst, uint64(max(tMs, 0)))
	dst = appendPath(dst, path)
	cnt := len(dst)
	dst = append(dst, 0x80, 0x00)
	n := 0
	for n < len(fixes) && n < MaxItems && len(dst)-start < targetRecBytes {
		f := &fixes[n]
		dst = binary.BigEndian.AppendUint32(dst, f.ICAO&0xFFFFFF)
		dst = binary.AppendVarint(dst, f.TMs-tMs)
		dst = binary.BigEndian.AppendUint32(dst, uint32(int32(q(f.Lat, 1e7))))
		dst = binary.BigEndian.AppendUint32(dst, uint32(int32(q(f.Lon, 1e7))))
		dst = binary.AppendVarint(dst, q(f.AltM, 10))
		dst = binary.AppendUvarint(dst, uq(f.SigmaHM, 10))
		dst = binary.AppendUvarint(dst, uq(f.SigmaVM, 10))
		var wf byte
		if f.Vel != nil {
			wf = 1
		}
		dst = append(dst, f.Grade, f.NBucket, f.Flags, wf)
		if f.Vel != nil {
			for _, v := range f.Vel {
				dst = binary.AppendVarint(dst, q(float64(v), 100))
			}
		}
		n++
	}
	dst[cnt] = byte(n&0x7F) | 0x80
	dst[cnt+1] = byte(n >> 7)
	return dst, n
}

// DecodeMLAT decodes an MLAT payload.
func DecodeMLAT(p []byte, into []Fix) (MLATBatch, error) {
	d := dec{b: p}
	var mb MLATBatch
	mb.TMs = int64(d.uv())
	mb.Path = d.path()
	n := d.uv()
	if d.err != nil {
		return mb, d.err
	}
	if n > MaxItems || mb.TMs < 0 {
		return mb, perr("MLAT count %d", n)
	}
	mb.Fixes = into[:0]
	for i := uint64(0); i < n; i++ {
		var f Fix
		f.ICAO = d.u32() & 0xFFFFFF
		f.TMs = mb.TMs + max(min(d.sv(), 1<<40), -(1<<40))
		f.Lat = float64(int32(d.u32())) / 1e7
		f.Lon = float64(int32(d.u32())) / 1e7
		f.AltM = float64(d.sv()) / 10
		f.SigmaHM = float64(d.uv()) / 10
		f.SigmaVM = float64(d.uv()) / 10
		f.Grade, f.NBucket, f.Flags = d.u8(), d.u8(), d.u8()
		if d.u8()&1 != 0 {
			var v [3]float32
			for j := range v {
				v[j] = float32(float64(d.sv()) / 100)
			}
			f.Vel = &v
		}
		if d.err != nil {
			return mb, d.err
		}
		if f.Lat < -90 || f.Lat > 90 || f.Lon < -180 || f.Lon > 180 {
			continue
		}
		mb.Fixes = append(mb.Fixes, f)
	}
	return mb, nil
}

// ---------------------------------------------------------------- PING

func appendPing(dst []byte, id uint64, ns int64) []byte {
	dst = binary.BigEndian.AppendUint64(dst, id)
	return binary.BigEndian.AppendUint64(dst, uint64(ns))
}

func decodePing(p []byte) (id uint64, ns int64, err error) {
	d := dec{b: p}
	id = d.u64()
	ns = int64(d.u64())
	return id, ns, d.err
}
