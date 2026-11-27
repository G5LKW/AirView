package hub

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"airview/internal/model"
)

// Protocol constants.
const (
	// ProtocolVersion is the wire protocol version spoken by this package.
	ProtocolVersion = 1
	// DefaultPort is the default TCP port of the data port.
	DefaultPort = 30154

	greetingWord = "AIRVIEW-HUB"
	authHMAC     = "hmac-sha256"
	authOpen     = "open"
	nonceLen     = 32
	maxLine      = 512
	maxRecord    = 65535 // maximum value of the uint16 length field
	recHeader    = 3     // uint16 length + uint8 type

	// unknownFeed is the reserved feed index meaning "no / unknown feed ID".
	unknownFeed = 255
)

// Record types.
const (
	recStation   byte = 1
	recFeedDef   byte = 2
	recFrame     byte = 3
	recUpdate    byte = 4
	recHeartbeat byte = 5
)

// Frame record flag bits.
const (
	flagMLAT   = 1 << 0
	flagGPS    = 1 << 1
	flagClock  = 1 << 2
	flagSignal = 1 << 3
)

// Update record mask bits.
const (
	updMLAT = 1 << iota
	updCallsign
	updSquawk
	updLat
	updLon
	updAltBaro
	updAltGeom
	updGS
	updTrack
	updVertRate
	updOnGroundSet
	updOnGroundVal
	updSignal
)

// Errors reported by the client. Use errors.Is.
var (
	// ErrAuth means the server rejected the password, or the server failed
	// to prove that it knows the password.
	ErrAuth = errors.New("hub: authentication failed")
	// ErrBusy means the server has reached its client limit.
	ErrBusy = errors.New("hub: server is full")
	// ErrThrottled means the server temporarily refuses logins from this address.
	ErrThrottled = errors.New("hub: too many failed logins, try again later")
	// ErrProtocol means the peer did not speak the expected protocol/version.
	ErrProtocol = errors.New("hub: protocol error")
)

// Station is the station metadata a host publishes to its clients
// (record type 1).
type Station struct {
	App      string          `json:"app"`
	Version  string          `json:"version"`
	Proto    int             `json:"proto"`
	Receiver model.Receiver  `json:"receiver"`
	Antennas []model.Antenna `json:"antennas"`
	// Feeds lists the host's configured feeds so that a client can map the
	// feed IDs carried by frames to the host's antenna attribution.
	Feeds []StationFeed `json:"feeds,omitempty"`
}

// StationFeed is the public part of one host feed definition.
type StationFeed struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	AntennaID string `json:"antennaId"`
	MLATOnly  bool   `json:"mlatOnly"`
}

// StationFeeds converts configured feeds (enabled ones only) to their
// public form for Server.SetStation.
func StationFeeds(feeds []model.Feed) []StationFeed {
	out := make([]StationFeed, 0, len(feeds))
	for _, f := range feeds {
		if !f.Enabled {
			continue
		}
		out = append(out, StationFeed{ID: f.ID, Name: f.Name, Type: f.Type, AntennaID: f.AntennaID, MLATOnly: f.MLATOnly})
	}
	return out
}

func clientProof(password string, serverNonce []byte) []byte {
	m := hmac.New(sha256.New, []byte(password))
	m.Write(serverNonce)
	return m.Sum(nil)
}

func serverProof(password string, serverNonce, clientNonce []byte) []byte {
	m := hmac.New(sha256.New, []byte(password))
	m.Write(clientNonce)
	m.Write(serverNonce)
	return m.Sum(nil)
}

// readLine reads one "\n"-terminated line of at most maxLine bytes and
// returns it without the line terminator.
func readLine(r *bufio.Reader) (string, error) {
	var buf [maxLine]byte
	n := 0
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			break
		}
		if n == len(buf) {
			return "", fmt.Errorf("%w: handshake line too long", ErrProtocol)
		}
		buf[n] = b
		n++
	}
	if n > 0 && buf[n-1] == '\r' {
		n--
	}
	return string(buf[:n]), nil
}

// beginRecord appends a record header with a zero length placeholder and
// returns the extended slice and the offset of the header.
func beginRecord(dst []byte, typ byte) ([]byte, int) {
	start := len(dst)
	return append(dst, 0, 0, typ), start
}

// endRecord patches the length field of the record that starts at start.
// It reports false (and truncates the record away) when it is too large.
func endRecord(dst []byte, start int) ([]byte, bool) {
	n := len(dst) - start - 2
	if n > maxRecord {
		return dst[:start], false
	}
	binary.BigEndian.PutUint16(dst[start:], uint16(n))
	return dst, true
}

func timeToNs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// appendFrame appends a Frame record for f.
func appendFrame(dst []byte, f *model.RawFrame, feed byte) []byte {
	dst, start := beginRecord(dst, recFrame)
	var flags byte
	if f.MLAT {
		flags |= flagMLAT
	}
	if f.GPSNs >= 0 {
		flags |= flagGPS
	}
	clock := f.Clock12MHz & 0xFFFFFFFFFFFF
	if clock != 0 {
		flags |= flagClock
	}
	if !math.IsNaN(f.Signal) {
		flags |= flagSignal
	}
	ch := f.Channel
	if ch < -1 || ch > 126 {
		ch = -1
	}
	dst = append(dst, flags, byte(f.Kind), feed, byte(int8(ch)))
	dst = binary.BigEndian.AppendUint64(dst, uint64(timeToNs(f.Recv)))
	if flags&flagGPS != 0 {
		dst = binary.BigEndian.AppendUint64(dst, uint64(f.GPSNs))
	}
	if flags&flagClock != 0 {
		dst = append(dst, byte(clock>>40), byte(clock>>32), byte(clock>>24), byte(clock>>16), byte(clock>>8), byte(clock))
	}
	if flags&flagSignal != 0 {
		dst = binary.BigEndian.AppendUint64(dst, math.Float64bits(f.Signal))
	}
	dst = append(dst, f.Data...)
	dst, _ = endRecord(dst, start) // cannot overflow: Data is at most a few bytes
	return dst
}

var errShort = fmt.Errorf("%w: truncated record", ErrProtocol)

// decodeFrame parses a Frame record payload. The returned frame owns a
// fresh copy of the data bytes. recvNs is the host receive time; the frame's
// Recv field is left zero for the caller to fill in.
func decodeFrame(p []byte) (f model.RawFrame, feed byte, recvNs int64, err error) {
	if len(p) < 12 {
		return f, 0, 0, errShort
	}
	flags := p[0]
	f.Kind = model.FrameKind(p[1])
	feed = p[2]
	f.Channel = int(int8(p[3]))
	recvNs = int64(binary.BigEndian.Uint64(p[4:]))
	p = p[12:]
	f.MLAT = flags&flagMLAT != 0
	f.GPSNs = -1
	if flags&flagGPS != 0 {
		if len(p) < 8 {
			return f, 0, 0, errShort
		}
		f.GPSNs = int64(binary.BigEndian.Uint64(p))
		p = p[8:]
	}
	if flags&flagClock != 0 {
		if len(p) < 6 {
			return f, 0, 0, errShort
		}
		f.Clock12MHz = uint64(p[0])<<40 | uint64(p[1])<<32 | uint64(p[2])<<24 | uint64(p[3])<<16 | uint64(p[4])<<8 | uint64(p[5])
		p = p[6:]
	}
	f.Signal = math.NaN()
	if flags&flagSignal != 0 {
		if len(p) < 8 {
			return f, 0, 0, errShort
		}
		f.Signal = math.Float64frombits(binary.BigEndian.Uint64(p))
		p = p[8:]
	}
	if len(p) > 0 {
		f.Data = make([]byte, len(p))
		copy(f.Data, p)
	}
	return f, feed, recvNs, nil
}

func appendString8(dst []byte, s string) []byte {
	if len(s) > 255 {
		s = s[:255]
	}
	dst = append(dst, byte(len(s)))
	return append(dst, s...)
}

func appendF64(dst []byte, v float64) []byte {
	return binary.BigEndian.AppendUint64(dst, math.Float64bits(v))
}

// appendUpdate appends an Update record for u.
func appendUpdate(dst []byte, u *model.DecodedUpdate, feed byte) []byte {
	dst, start := beginRecord(dst, recUpdate)
	var mask uint16
	if u.MLAT {
		mask |= updMLAT
	}
	if u.Callsign != nil {
		mask |= updCallsign
	}
	if u.Squawk != nil {
		mask |= updSquawk
	}
	if u.Lat != nil {
		mask |= updLat
	}
	if u.Lon != nil {
		mask |= updLon
	}
	if u.AltBaro != nil {
		mask |= updAltBaro
	}
	if u.AltGeom != nil {
		mask |= updAltGeom
	}
	if u.GS != nil {
		mask |= updGS
	}
	if u.Track != nil {
		mask |= updTrack
	}
	if u.VertRate != nil {
		mask |= updVertRate
	}
	if u.OnGround != nil {
		mask |= updOnGroundSet
		if *u.OnGround {
			mask |= updOnGroundVal
		}
	}
	if u.Signal != nil {
		mask |= updSignal
	}
	// The wire field is 31 bits wide in effect: int is 32 bits on linux/arm.
	msgs := u.Msgs
	if msgs < 0 {
		msgs = 0
	}
	if msgs > math.MaxInt32 {
		msgs = math.MaxInt32
	}
	dst = binary.BigEndian.AppendUint16(dst, mask)
	dst = append(dst, feed)
	dst = binary.BigEndian.AppendUint64(dst, uint64(timeToNs(u.Recv)))
	dst = binary.BigEndian.AppendUint32(dst, u.ICAO)
	dst = binary.BigEndian.AppendUint32(dst, uint32(msgs))
	if u.Callsign != nil {
		dst = appendString8(dst, *u.Callsign)
	}
	if u.Squawk != nil {
		dst = appendString8(dst, *u.Squawk)
	}
	for _, p := range [...]*float64{u.Lat, u.Lon, u.AltBaro, u.AltGeom, u.GS, u.Track, u.VertRate, u.Signal} {
		if p != nil {
			dst = appendF64(dst, *p)
		}
	}
	dst, _ = endRecord(dst, start) // at most ~600 bytes
	return dst
}

// decodeUpdate parses an Update record payload.
func decodeUpdate(p []byte) (u model.DecodedUpdate, feed byte, recvNs int64, err error) {
	if len(p) < 19 {
		return u, 0, 0, errShort
	}
	mask := binary.BigEndian.Uint16(p)
	feed = p[2]
	recvNs = int64(binary.BigEndian.Uint64(p[3:]))
	u.ICAO = binary.BigEndian.Uint32(p[11:])
	if m := binary.BigEndian.Uint32(p[15:]); m > math.MaxInt32 {
		u.Msgs = math.MaxInt32
	} else {
		u.Msgs = int(m)
	}
	p = p[19:]
	u.MLAT = mask&updMLAT != 0

	str := func() (*string, bool) {
		if len(p) < 1 || len(p) < 1+int(p[0]) {
			return nil, false
		}
		s := string(p[1 : 1+int(p[0])])
		p = p[1+int(p[0]):]
		return &s, true
	}
	var ok bool
	if mask&updCallsign != 0 {
		if u.Callsign, ok = str(); !ok {
			return u, 0, 0, errShort
		}
	}
	if mask&updSquawk != 0 {
		if u.Squawk, ok = str(); !ok {
			return u, 0, 0, errShort
		}
	}
	fields := [...]struct {
		bit uint16
		dst **float64
	}{
		{updLat, &u.Lat}, {updLon, &u.Lon}, {updAltBaro, &u.AltBaro}, {updAltGeom, &u.AltGeom},
		{updGS, &u.GS}, {updTrack, &u.Track}, {updVertRate, &u.VertRate}, {updSignal, &u.Signal},
	}
	for _, fl := range fields {
		if mask&fl.bit == 0 {
			continue
		}
		if len(p) < 8 {
			return u, 0, 0, errShort
		}
		v := math.Float64frombits(binary.BigEndian.Uint64(p))
		p = p[8:]
		*fl.dst = &v
	}
	if mask&updOnGroundSet != 0 {
		g := mask&updOnGroundVal != 0
		u.OnGround = &g
	}
	return u, feed, recvNs, nil
}

// appendHeartbeat appends a Heartbeat record.
func appendHeartbeat(dst []byte, now time.Time, seq uint32) []byte {
	dst, start := beginRecord(dst, recHeartbeat)
	dst = binary.BigEndian.AppendUint64(dst, uint64(now.UnixNano()))
	dst = binary.BigEndian.AppendUint32(dst, seq)
	dst, _ = endRecord(dst, start)
	return dst
}

// appendFeedDef appends a FeedDef record.
func appendFeedDef(dst []byte, index byte, id string) []byte {
	dst, start := beginRecord(dst, recFeedDef)
	dst = append(dst, index)
	dst = append(dst, id...)
	dst, _ = endRecord(dst, start)
	return dst
}
