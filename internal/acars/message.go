// Package acars ingests ACARS and VDL Mode 2 messages decoded by external
// programs (acarsdec and dumpvdl2, both in their JSON output modes), keeps
// a bounded in-memory history, links messages to Mode S aircraft and
// serves them to the HTTP API.
//
// AirView does not demodulate VHF itself: a separate receiver (a second
// RTL-SDR on 129-137 MHz) runs the decoder, which sends one JSON object per
// message over UDP or TCP. Both formats were implemented from the field
// names in the decoders' documented JSON output (clean room: no decoder
// source code was used). Unknown fields are ignored, so newer decoder
// versions keep working.
package acars

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Source names of the decoders.
const (
	SrcAcarsdec = "acarsdec"
	SrcDumpVDL2 = "dumpvdl2"
)

// Links.
const (
	LinkACARS = "ACARS" // plain-old ACARS (VHF, MSK 2400 bit/s)
	LinkVDL2  = "VDL2"  // VDL Mode 2 (D8PSK 31.5 kbit/s)
)

// MaxTextBytes bounds the stored message text.
const MaxTextBytes = 4096

// Message is one ACARS message (both links).
type Message struct {
	ID      uint64  `json:"id"`
	T       int64   `json:"t"` // Unix milliseconds (decoder time, else arrival time)
	Source  string  `json:"src"`
	Link    string  `json:"link"`
	Station string  `json:"station,omitempty"` // decoder station id
	FreqMHz float64 `json:"freqMHz,omitempty"`
	// LevelDB is the signal level the decoder reports (dB, decoder-specific
	// reference: compare only messages of the same decoder).
	LevelDB *float64 `json:"levelDb,omitempty"`

	ICAO string `json:"icao,omitempty"` // 6 upper-case hex digits
	// ICAOFrom says how ICAO was found: "message" (the decoder reported
	// the aircraft address) or "reg" (matched by registration against the
	// live Mode S picture).
	ICAOFrom string `json:"icaoFrom,omitempty"`
	Reg      string `json:"reg,omitempty"`    // registration as sent, leading dots removed
	Flight   string `json:"flight,omitempty"` // flight id as sent (usually IATA style, "BA0123")

	Mode    string `json:"mode,omitempty"`
	Label   string `json:"label,omitempty"`
	Sublbl  string `json:"sublabel,omitempty"`
	BlockID string `json:"blockId,omitempty"`
	MsgNo   string `json:"msgNo,omitempty"`
	Ack     string `json:"ack,omitempty"`
	// Uplink: ground to air (block id is a letter); false = downlink.
	Uplink bool   `json:"uplink"`
	Dep    string `json:"dep,omitempty"`
	Dst    string `json:"dst,omitempty"`
	Text   string `json:"text,omitempty"`
	// More: the decoder reports further blocks of the same message.
	More bool `json:"more,omitempty"`
}

// Errors of Parse.
var (
	ErrNotJSON    = errors.New("not a JSON object")
	ErrUnknown    = errors.New("unknown message format")
	ErrNotACARS   = errors.New("VDL2 frame without ACARS content")
	ErrBadMessage = errors.New("ACARS message failed the decoder's CRC")
)

// acarsdecJSON holds the fields of acarsdec's JSON output.
type acarsdecJSON struct {
	Timestamp float64         `json:"timestamp"`
	StationID string          `json:"station_id"`
	Freq      float64         `json:"freq"` // MHz
	Level     *float64        `json:"level"`
	Error     int             `json:"error"` // corrected bits (informative; only CRC-valid messages are output)
	Mode      string          `json:"mode"`
	Label     string          `json:"label"`
	Sublabel  string          `json:"sublabel"`
	BlockID   string          `json:"block_id"`
	Ack       json.RawMessage `json:"ack"` // false or a character
	Tail      string          `json:"tail"`
	Flight    string          `json:"flight"`
	MsgNo     string          `json:"msgno"`
	Text      string          `json:"text"`
	End       *bool           `json:"end"`
	Depa      string          `json:"depa"`
	Dsta      string          `json:"dsta"`
	ICAO      json.RawMessage `json:"icao"` // number or hex string
	App       *struct {
		Name string `json:"name"`
	} `json:"app"`
}

// vdl2JSON holds the fields of dumpvdl2's JSON output that matter here.
type vdl2JSON struct {
	VDL2 *struct {
		Station string `json:"station"`
		T       struct {
			Sec  int64 `json:"sec"`
			Usec int64 `json:"usec"`
		} `json:"t"`
		Freq     float64  `json:"freq"` // Hz
		SigLevel *float64 `json:"sig_level"`
		AVLC     *struct {
			Src struct {
				Addr string `json:"addr"`
				Type string `json:"type"`
			} `json:"src"`
			Dst struct {
				Addr string `json:"addr"`
				Type string `json:"type"`
			} `json:"dst"`
			ACARS *struct {
				Err      bool   `json:"err"`
				CRCOK    *bool  `json:"crc_ok"`
				More     bool   `json:"more"`
				Reg      string `json:"reg"`
				Mode     string `json:"mode"`
				Label    string `json:"label"`
				Sublabel string `json:"sublabel"`
				BlkID    string `json:"blk_id"`
				Ack      string `json:"ack"`
				Flight   string `json:"flight"`
				MsgNum   string `json:"msg_num"`
				MsgSeq   string `json:"msg_num_seq"`
				Text     string `json:"msg_text"`
				Text2    string `json:"text"`
			} `json:"acars"`
		} `json:"avlc"`
	} `json:"vdl2"`
}

// Parse decodes one JSON object of either decoder. now is the arrival
// time; it replaces a missing or implausible decoder time (more than an
// hour away from now).
func Parse(line []byte, now time.Time) (Message, error) {
	line = bytes.TrimSpace(line)
	if len(line) < 2 || line[0] != '{' {
		return Message{}, ErrNotJSON
	}
	if bytes.Contains(line, []byte(`"vdl2"`)) {
		var v vdl2JSON
		if err := json.Unmarshal(line, &v); err != nil {
			return Message{}, ErrNotJSON
		}
		if v.VDL2 != nil {
			return fromVDL2(&v, now)
		}
	}
	var a acarsdecJSON
	if err := json.Unmarshal(line, &a); err != nil {
		return Message{}, ErrNotJSON
	}
	if a.Label == "" && a.Text == "" && a.Tail == "" {
		return Message{}, ErrUnknown
	}
	return fromAcarsdec(&a, now)
}

func fromAcarsdec(a *acarsdecJSON, now time.Time) (Message, error) {
	m := Message{
		Source:  SrcAcarsdec,
		Link:    LinkACARS,
		Station: clean(a.StationID, 32),
		LevelDB: finitePtr(a.Level),
		Reg:     cleanReg(a.Tail),
		Flight:  cleanFlight(a.Flight),
		Mode:    clean(a.Mode, 1),
		Label:   clean(a.Label, 2),
		Sublbl:  clean(a.Sublabel, 2),
		BlockID: clean(a.BlockID, 1),
		MsgNo:   clean(a.MsgNo, 4),
		Ack:     ackChar(a.Ack),
		Dep:     clean(a.Depa, 4),
		Dst:     clean(a.Dsta, 4),
		Text:    cleanText(a.Text),
	}
	if a.End != nil {
		m.More = !*a.End
	}
	if a.Freq > 0 && a.Freq < 10000 {
		m.FreqMHz = round3(a.Freq)
	}
	m.T = msTime(a.Timestamp, now)
	m.Uplink = isUplinkBlock(m.BlockID)
	if icao := icaoFromRaw(a.ICAO); icao != "" {
		m.ICAO, m.ICAOFrom = icao, "message"
	}
	return m, nil
}

func fromVDL2(v *vdl2JSON, now time.Time) (Message, error) {
	d := v.VDL2
	if d.AVLC == nil || d.AVLC.ACARS == nil {
		return Message{}, ErrNotACARS
	}
	a := d.AVLC.ACARS
	if a.Err || (a.CRCOK != nil && !*a.CRCOK) {
		return Message{}, ErrBadMessage
	}
	text := a.Text
	if text == "" {
		text = a.Text2
	}
	m := Message{
		Source:  SrcDumpVDL2,
		Link:    LinkVDL2,
		Station: clean(d.Station, 32),
		LevelDB: finitePtr(d.SigLevel),
		Reg:     cleanReg(a.Reg),
		Flight:  cleanFlight(a.Flight),
		Mode:    clean(a.Mode, 1),
		Label:   clean(a.Label, 2),
		Sublbl:  clean(a.Sublabel, 2),
		BlockID: clean(a.BlkID, 1),
		MsgNo:   clean(a.MsgNum+a.MsgSeq, 4),
		Ack:     clean(a.Ack, 1),
		Text:    cleanText(text),
		More:    a.More,
	}
	if d.Freq > 1e6 && d.Freq < 1e10 {
		m.FreqMHz = round3(d.Freq / 1e6)
	}
	sec := float64(d.T.Sec) + float64(d.T.Usec)/1e6
	m.T = msTime(sec, now)
	src, dst := d.AVLC.Src, d.AVLC.Dst
	switch {
	case strings.EqualFold(src.Type, "Aircraft"):
		m.ICAO = hex6(src.Addr)
	case strings.EqualFold(dst.Type, "Aircraft"):
		m.ICAO = hex6(dst.Addr)
		m.Uplink = true
	default:
		m.Uplink = isUplinkBlock(m.BlockID)
	}
	if m.ICAO != "" {
		m.ICAOFrom = "message"
	}
	return m, nil
}

// isUplinkBlock: ACARS downlink block ids are digits, uplink ids letters.
func isUplinkBlock(b string) bool {
	if b == "" {
		return false
	}
	c := b[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func msTime(sec float64, now time.Time) int64 {
	n := now.UnixMilli()
	if sec <= 0 || math.IsNaN(sec) || math.IsInf(sec, 0) {
		return n
	}
	t := int64(sec * 1000)
	if d := t - n; d > 3_600_000 || d < -3_600_000 {
		return n
	}
	return t
}

func icaoFromRaw(r json.RawMessage) string {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || string(r) == "null" {
		return ""
	}
	if r[0] == '"' {
		var s string
		if json.Unmarshal(r, &s) != nil {
			return ""
		}
		return hex6(s)
	}
	v, err := strconv.ParseUint(string(r), 10, 32)
	if err != nil || v == 0 || v > 0xFFFFFF {
		return ""
	}
	return strings.ToUpper(strconv.FormatUint(v|1<<24, 16)[1:])
}

// hex6 normalises a 24-bit address to 6 upper-case hex digits ("" when it
// is not one, or is zero).
func hex6(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 0 || len(s) > 6 {
		return ""
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil || v == 0 {
		return ""
	}
	return strings.ToUpper(strconv.FormatUint(v|1<<24, 16)[1:])
}

func ackChar(r json.RawMessage) string {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || r[0] != '"' {
		return "" // false / absent: no acknowledgement
	}
	var s string
	if json.Unmarshal(r, &s) != nil {
		return ""
	}
	return clean(s, 1)
}

// clean keeps printable ASCII, trimmed, at most n bytes.
func clean(s string, n int) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < n; i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// cleanReg removes the leading dots ACARS pads registrations with.
func cleanReg(s string) string {
	return strings.TrimLeft(clean(s, 10), ". ")
}

func cleanFlight(s string) string {
	return strings.ToUpper(clean(s, 8))
}

// cleanText keeps the message text readable: valid UTF-8, CR LF / CR as
// LF, other control characters removed, at most MaxTextBytes.
func cleanText(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	b.Grow(min(len(s), MaxTextBytes))
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > MaxTextBytes {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), "\n\t ")
}

func finitePtr(p *float64) *float64 {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
		return nil
	}
	v := math.Round(*p*10) / 10
	return &v
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// NormReg returns the comparison form of a registration: upper case, no
// dots, dashes or spaces ("G-EUPT" and ".GEUPT" -> "GEUPT").
func NormReg(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - 32)
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		}
	}
	return b.String()
}
