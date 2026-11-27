package mlatc

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode"

	"airview/internal/model"
)

// hello is the decoded client handshake (MLATC.md §2). Every field is read
// tolerantly: booleans may also be 0/1 or "true"/"false", numbers may be
// numeric strings, compress may be a list or one string.
type hello struct {
	version       int
	clientVersion string
	compress      []string
	selective     bool
	heartbeat     bool
	returnResults bool
	ecef          bool // return_result_format "ecef"
	returnStats   bool
	udp           int  // udp_transport version offered (0 = none; the reference client offers 2)
	modeAC        bool // AirView extension: the client understands Mode A/C requests (MLATC.md §10)
	uuidRaw       string
	uuid          string // normalised, "" if absent/invalid
	user          string
	clockType     string
	privacy       bool
	privacySet    bool
	lat, lon, alt float64
	located       bool
}

// Handshake refusal texts (sent in "deny"; short, no internals).
const (
	denyBadHello   = "invalid handshake"
	denyVersion    = "unsupported protocol version (need 2 or 3)"
	denyBusy       = "server busy, try later"
	denyLimit      = "too many connections from your address"
	denyCoolDown   = "temporarily refused, try later"
	denyNotReady   = "server not ready"
	denyDuplicate  = "this receiver is already connected"
	denyRegistry   = "not admitted"
	denyRateLimit  = "message rate limit exceeded"
	denyStationsIP = "too many stations from your address"
)

// parseHello decodes the handshake line. ok false means it is not a JSON
// object at all.
func parseHello(line []byte) (h hello, ok bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(line), &m); err != nil || m == nil {
		return h, false
	}
	num := func(k string) (float64, bool) {
		v, ok := m[k]
		if !ok {
			return 0, false
		}
		var f float64
		if json.Unmarshal(v, &f) == nil {
			return f, !math.IsNaN(f) && !math.IsInf(f, 0)
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f, true
			}
		}
		return 0, false
	}
	flag := func(k string) (val, present bool) {
		v, ok := m[k]
		if !ok {
			return false, false
		}
		var b bool
		if json.Unmarshal(v, &b) == nil {
			return b, true
		}
		var f float64
		if json.Unmarshal(v, &f) == nil {
			return f != 0, true
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true", "yes", "1", "on":
				return true, true
			case "false", "no", "0", "off", "":
				return false, true
			}
		}
		return false, false
	}
	text := func(k string) string {
		var s string
		if v, ok := m[k]; ok && json.Unmarshal(v, &s) == nil {
			return s
		}
		return ""
	}
	if v, ok := num("version"); ok && v == math.Trunc(v) && v >= 0 && v < 1000 {
		h.version = int(v)
	}
	h.clientVersion = sanitize(text("client_version"), 64)
	if v, ok := m["compress"]; ok {
		var list []string
		if json.Unmarshal(v, &list) == nil {
			h.compress = list
		} else if s := text("compress"); s != "" {
			h.compress = []string{s}
		}
	}
	h.selective, _ = flag("selective_traffic")
	h.heartbeat, _ = flag("heartbeat")
	h.returnResults, _ = flag("return_results")
	h.returnStats, _ = flag("return_stats")
	h.ecef = strings.EqualFold(text("return_result_format"), "ecef")
	if v, ok := m["udp_transport"]; ok {
		// A number names the datagram format version (2); false or
		// anything else is no offer (a bare true names no format).
		var f float64
		if json.Unmarshal(v, &f) == nil && f == math.Trunc(f) && f > 0 && f < 1000 {
			h.udp = int(f)
		}
	}
	h.uuidRaw = text("uuid")
	if u, ok := model.NormalizeUUID(h.uuidRaw); ok {
		h.uuid = u
	}
	h.user = sanitize(text("user"), 64)
	h.clockType = strings.ToLower(sanitize(text("clock_type"), 32))
	if h.clockType == "" {
		h.clockType = "dump1090" // the reference client's default
	}
	h.privacy, h.privacySet = flag("privacy")
	if v, ok := flag("modeac"); ok {
		h.modeAC = v
	} else {
		h.modeAC, _ = flag("mode_ac")
	}
	lat, okLat := num("lat")
	lon, okLon := num("lon")
	alt, okAlt := num("alt")
	if !okAlt {
		alt = 0
	}
	if okLat && okLon && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 && !(lat == 0 && lon == 0) &&
		alt >= -1000 && alt <= 10000 {
		h.lat, h.lon, h.alt, h.located = lat, lon, alt, true
	}
	return h, true
}

// sanitize keeps printable characters, trims, and cuts to max runes.
func sanitize(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if n >= max {
			break
		}
		if !unicode.IsPrint(r) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// chooseCompression picks zlib2, then zlib, then none among the offered
// methods (DESIGN §5.2). A client that offers nothing gets none.
func chooseCompression(offered []string, allow bool) string {
	if !allow {
		return compressNone
	}
	has := func(s string) bool {
		for _, o := range offered {
			if strings.EqualFold(strings.TrimSpace(o), s) {
				return true
			}
		}
		return false
	}
	switch {
	case has(compressZlib2):
		return compressZlib2
	case has(compressZlib):
		return compressZlib
	}
	return compressNone
}

// helloReply is the server's handshake answer.
type helloReply struct {
	Compress string `json:"compress"`
	// UDPTransport is false, or [host, port, key] when the client offered
	// udp_transport 2 and the server accepts datagrams (MLATC.md §9).
	UDPTransport any    `json:"udp_transport"`
	SplitSync    bool   `json:"split_sync"`
	MOTD         string `json:"motd,omitempty"`
}

type denyReply struct {
	Deny        []string `json:"deny"`
	ReconnectIn int      `json:"reconnect_in,omitempty"`
}

func marshalLine(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}
