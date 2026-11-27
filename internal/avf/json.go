package avf

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"

	"airview/internal/model"
)

// JSON payloads (AVF.md §5, §6, §7). Unknown members are ignored on decode,
// and bytes after the first JSON value are ignored (forward compatibility).

// AppendJSON appends the JSON encoding of v (a no-seq JSON payload).
func AppendJSON(dst []byte, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return dst, err
	}
	return append(dst, b...), nil
}

// AppendSeqJSON appends "uvarint seq | JSON(v)".
func AppendSeqJSON(dst []byte, seq uint64, v any) ([]byte, error) {
	return AppendJSON(binary.AppendUvarint(dst, seq), v)
}

// DecodeJSON decodes the first JSON value of p into v. A missing or
// malformed value is a protocol error.
func DecodeJSON(p []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(p))
	if err := dec.Decode(v); err != nil {
		return protoErr("bad JSON payload: %v", err)
	}
	return nil
}

// DecodeSeqJSON decodes "uvarint seq | JSON" into v and returns the seq.
func DecodeSeqJSON(p []byte, v any) (uint64, error) {
	seq, body, err := SplitSeq(p)
	if err != nil {
		return 0, err
	}
	return seq, DecodeJSON(body, v)
}

// ---------------------------------------------------------------- RECEIVER

// Location is the optional receiver location of RECEIVER (sent only with
// Uplink.SendLocation).
type Location struct {
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Alt    float64 `json:"alt"`
	AltRef string  `json:"altRef"`           // "hae" | "msl"
	Source string  `json:"source"`           // "manual" | "gnss" | "estimated"
	SigmaM float64 `json:"sigmaM,omitempty"` // 1σ horizontal, m
}

// Antenna describes one antenna of a receiver.
type Antenna struct {
	ID   string  `json:"id"`
	Kind string  `json:"kind"`
	AGLM float64 `json:"aglM"`
}

// Privacy holds the owner's privacy choices for a receiver.
type Privacy struct {
	Pin         string `json:"pin"`
	PublicStats bool   `json:"publicStats"`
	ShowName    bool   `json:"showName"`
}

// Receiver is a RECEIVER (0x01) record body. It MUST NOT carry host names,
// LAN addresses, MAC addresses or account names.
type Receiver struct {
	RxNo       int       `json:"rxNo"`
	Name       string    `json:"name,omitempty"`
	ClockClass string    `json:"clockClass"`
	NominalHz  float64   `json:"nominalHz"`
	EpochID    uint32    `json:"epochId"`
	Channels   int       `json:"channels"`
	Decoder    string    `json:"decoder,omitempty"`
	Location   *Location `json:"location,omitempty"`
	Antennas   []Antenna `json:"antennas,omitempty"`
	Privacy    Privacy   `json:"privacy"`
	Timestamps bool      `json:"timestamps"`
}

// Class returns the clock class; unknown names are treated as none.
func (r *Receiver) Class() model.ClockClass {
	c, _ := model.ParseClockClass(r.ClockClass)
	return c
}

// Validate checks the mandatory fields' ranges.
func (r *Receiver) Validate() error {
	switch {
	case r.RxNo < 1 || r.RxNo > 255:
		return protoErr("RECEIVER rxNo %d", r.RxNo)
	case r.Channels < 1 || r.Channels > 15:
		return protoErr("RECEIVER channels %d", r.Channels)
	case r.Location != nil && (r.Location.Lat < -90 || r.Location.Lat > 90 || r.Location.Lon < -180 || r.Location.Lon > 180):
		return protoErr("RECEIVER location out of range")
	}
	return nil
}

// ---------------------------------------------------------------- CLOCK_EVENT etc.

// ClockEvent is a CLOCK_EVENT (0x05) record body.
type ClockEvent struct {
	RxNo        int                    `json:"rxNo"`
	OldEpoch    uint32                 `json:"oldEpoch"`
	NewEpoch    uint32                 `json:"newEpoch"`
	Reason      model.ClockEventReason `json:"reason"`
	SamplesLost int64                  `json:"samplesLost,omitempty"`
	HostNs      int64                  `json:"hostNs"`
}

// InputStatus is an INPUT_STATUS (0x06) record body.
type InputStatus struct {
	RxNo       int     `json:"rxNo"`
	Connected  bool    `json:"connected"`
	FPS        float64 `json:"fps"`
	CRCErrors  uint64  `json:"crcErrors"`
	Type       string  `json:"type"`
	ClockClass string  `json:"clockClass"`
	SinceMs    int64   `json:"sinceMs"`
}

// GNSSPos is a GNSS_POS (0x07) record body.
type GNSSPos struct {
	RxNo   int     `json:"rxNo"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	AltM   float64 `json:"altM"`
	AltRef string  `json:"altRef"`
	TMs    int64   `json:"tMs"`
}

// DecodedUpdate is one element of the DECODED (0x08) JSON array. Optional
// members are pointers so that zero values (latitude 0, on-ground false)
// survive.
type DecodedUpdate struct {
	ICAO    string   `json:"icao"`
	TMs     int64    `json:"tMs"`
	CS      *string  `json:"cs,omitempty"`
	Sq      *string  `json:"sq,omitempty"`
	Lat     *float64 `json:"lat,omitempty"`
	Lon     *float64 `json:"lon,omitempty"`
	AltBaro *int     `json:"altBaro,omitempty"`
	AltGeom *int     `json:"altGeom,omitempty"`
	GS      *float64 `json:"gs,omitempty"`
	Trk     *float64 `json:"trk,omitempty"`
	VR      *int     `json:"vr,omitempty"`
	Gnd     *bool    `json:"gnd,omitempty"`
	Sig     *float64 `json:"sig,omitempty"`
	N       int      `json:"n,omitempty"`
}

// AppendDecoded appends a complete DECODED payload.
func AppendDecoded(dst []byte, seq uint64, rxNo uint8, ups []DecodedUpdate) ([]byte, error) {
	return AppendJSON(AppendDecodedHead(dst, seq, rxNo), ups)
}

// DecodeDecoded parses a DECODED payload.
func DecodeDecoded(p []byte) (seq uint64, rxNo uint8, ups []DecodedUpdate, err error) {
	seq, rxNo, js, err := SplitDecoded(p)
	if err != nil {
		return 0, 0, nil, err
	}
	if err := DecodeJSON(js, &ups); err != nil {
		return 0, 0, nil, err
	}
	return seq, rxNo, ups, nil
}

// RSSIHist is the 1 dB signal histogram of MINUTE_STATS: Counts[i] counts
// frames with signal in [Lo+i, Lo+i+1) dBFS.
type RSSIHist struct {
	Lo     int      `json:"lo"`
	Counts []uint64 `json:"counts"`
}

// MinuteStats is a MINUTE_STATS (0x09) record body.
type MinuteStats struct {
	RxNo      int               `json:"rxNo"`
	Minute    int64             `json:"minute"` // Unix second at the start of the minute
	DF        map[string]uint64 `json:"df"`
	ModeAC    uint64            `json:"modeAC"`
	ICAOs     uint64            `json:"icaos"`
	Positions uint64            `json:"positions"`
	CRCFail   uint64            `json:"crcFail"`
	RSSI      *RSSIHist         `json:"rssi,omitempty"`
}

// Backfill phases.
const (
	BackfillBegin = "begin"
	BackfillEnd   = "end"
)

// Backfill is a BACKFILL (0x0B) record body.
type Backfill struct {
	Phase   string `json:"phase"`
	FromSeq uint64 `json:"fromSeq"`
	ToSeq   uint64 `json:"toSeq"`
	SpanNs  int64  `json:"spanNs"`
}

// Claim is a CLAIM (0x0C) record body.
type Claim struct {
	RxNo int `json:"rxNo"`
}

// ClaimCode is a CLAIM_CODE (0x87) record body.
type ClaimCode struct {
	RxNo      int    `json:"rxNo"`
	Code      string `json:"code"`
	ExpiresMs int64  `json:"expiresMs"`
}

// Goodbye is a GOODBYE (0x0D / 0x89) record body. ReconnectIn (seconds) is
// meaningful only server → client.
type Goodbye struct {
	Code        string `json:"code"`
	Reason      string `json:"reason,omitempty"`
	ReconnectIn int    `json:"reconnectIn,omitempty"`
}

// ---------------------------------------------------------------- server → client JSON

// Notice is a NOTICE (0x88) record body.
type Notice struct {
	Code  string `json:"code"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

// Interest is an INTEREST (0x83) record body: selective-mode set deltas.
// ICAO strings are 6 upper-case hex digits ("~" prefix for non-ICAO).
type Interest struct {
	RxNo     int      `json:"rxNo"`
	Mode     string   `json:"mode"`
	ReduceMs int      `json:"reduceMs,omitempty"`
	Reset    bool     `json:"reset,omitempty"`
	SyncAdd  []string `json:"syncAdd,omitempty"`
	SyncDel  []string `json:"syncDel,omitempty"`
	MLATAdd  []string `json:"mlatAdd,omitempty"`
	MLATDel  []string `json:"mlatDel,omitempty"`
}

// Validate checks mode and ICAO strings.
func (in *Interest) Validate() error {
	if in.Mode != ModeFull && in.Mode != ModeSelective {
		return protoErr("INTEREST mode %q", in.Mode)
	}
	if in.ReduceMs < 0 {
		return protoErr("INTEREST reduceMs %d", in.ReduceMs)
	}
	for _, l := range [][]string{in.SyncAdd, in.SyncDel, in.MLATAdd, in.MLATDel} {
		for _, s := range l {
			if _, _, ok := ParseICAO(s); !ok {
				return protoErr("INTEREST ICAO %q", s)
			}
		}
	}
	return nil
}

// ParseICAO parses a JSON ICAO string: 6 hex digits, optionally prefixed
// with "~" for a non-ICAO address.
func ParseICAO(s string) (addr uint32, nonICAO bool, ok bool) {
	if strings.HasPrefix(s, "~") {
		nonICAO = true
		s = s[1:]
	}
	if len(s) != 6 {
		return 0, false, false
	}
	for i := 0; i < 6; i++ {
		c := s[i]
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		default:
			return 0, false, false
		}
		addr = addr<<4 | uint32(v)
	}
	return addr, nonICAO, true
}

// FormatICAO formats an address as in AVF JSON.
func FormatICAO(addr uint32, nonICAO bool) string {
	const hx = "0123456789ABCDEF"
	b := make([]byte, 0, 7)
	if nonICAO {
		b = append(b, '~')
	}
	for sh := 20; sh >= 0; sh -= 4 {
		b = append(b, hx[(addr>>uint(sh))&0xF])
	}
	return string(b)
}

// Flow is a FLOW (0x84) record body.
type Flow struct {
	BatchMs    int     `json:"batchMs,omitempty"`
	Pause      bool    `json:"pause"`
	SlowFactor float64 `json:"slowFactor,omitempty"`
}

// Validate checks the ranges of AVF.md §7.3.
func (f *Flow) Validate() error {
	if f.BatchMs < 0 || f.BatchMs > 60000 {
		return protoErr("FLOW batchMs %d", f.BatchMs)
	}
	if f.SlowFactor < 0 || f.SlowFactor > 1 {
		return protoErr("FLOW slowFactor %v", f.SlowFactor)
	}
	return nil
}

// FeedbackLocation is the location status inside FEEDBACK.
type FeedbackLocation struct {
	Status string  `json:"status"`
	DeltaM float64 `json:"deltaM,omitempty"`
}

// Feedback is a FEEDBACK (0x86) record body (own station only; peer counts
// per distance band, never identities).
type Feedback struct {
	RxNo         int               `json:"rxNo"`
	Sync         string            `json:"sync"`
	Anchor       string            `json:"anchor"`
	SigmaThetaNs float64           `json:"sigmaThetaNs"`
	DriftPpb     float64           `json:"driftPpb"`
	OutlierPct   float64           `json:"outlierPct"`
	Peers        map[string]int    `json:"peers,omitempty"`
	Location     *FeedbackLocation `json:"location,omitempty"`
	Warnings     []string          `json:"warnings,omitempty"`
}
