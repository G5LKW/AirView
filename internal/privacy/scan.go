package privacy

import (
	"bytes"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// ForbiddenPublicKeys are JSON object keys that must never appear in a
// response produced for the public scope (DESIGN §8.4 plus every other
// receiver-related member of the model types). The golden tests fail on
// any of them.
var ForbiddenPublicKeys = []string{
	// DESIGN §8.4 list.
	"seenBy", "feeds", "distKm", "bearing", "recentReceiverIds",
	// Geometry relative to a receiver.
	"elevation", "slantKm", "cpaKm", "cpaInS", "terrainVisible", "losMarginDeg", "maxRangeKm",
	// Reception and receiver lists. ("stations" is not listed: it names the
	// public station list; the aircraft receiver list (model.Aircraft.Stations)
	// cannot be in a public type (publicdto.TestNoForbiddenFields) and its
	// SIDs are caught by the hidden-SID check.)
	"rssi", "rssiAvg", "antennas", "mine", "receivers",
	// Station identity and location.
	"receiverGps", "station", "declared", "effective", "gnss", "estimated", "estSigmaM",
	"feederId", "uuid", "ipKey", "remote", "ip", "ipHash", "ownerId", "owner", "dataDir",
}

// Point is an exact station position used as a fixture.
type Point struct {
	Lat, Lon float64
	// Strict reports a leak for a single matching coordinate. When false
	// (a point near live traffic, where an aircraft may share one
	// coordinate by chance), a leak needs the latitude and the longitude
	// within PairWindow bytes of each other.
	Strict bool
	Label  string
}

// PairWindow is the distance in bytes within which a matching latitude
// and longitude count as one leaked position.
const PairWindow = 160

// Fixture lists the secrets a body is scanned for.
type Fixture struct {
	Points []Point
	// IPs are fixture addresses (IPv4 and IPv6). Any IPv4 address is a leak
	// regardless; IPv6 addresses are matched literally (and by their /64).
	IPs []string
	// SIDs of stations whose pin is hidden: they must not appear at all.
	SIDs []string
	// Secrets are other strings that must not appear (UUIDs, feeder IDs,
	// receiver tick values as decimal strings, ...).
	Secrets []string
	// AllowIPs are addresses that may appear (an owner's own current IP
	// on the owner's pages, DESIGN §8.2).
	AllowIPs []string
	// AllowKeys exempts JSON keys from ForbiddenPublicKeys (for scans of
	// owner-scope bodies, which may carry their own station's data).
	AllowKeys []string
	// NoKeyCheck disables the forbidden-key check (owner/admin crawls that
	// only look for other stations' data).
	NoKeyCheck bool
}

// Leak is one finding of Scan.
type Leak struct {
	Kind   string // "coordinate", "ip", "sid", "secret", "ns-timestamp", "key"
	Detail string
	Offset int
}

func (l Leak) String() string {
	return fmt.Sprintf("%s at byte %d: %s", l.Kind, l.Offset, l.Detail)
}

var (
	numberRE = regexp.MustCompile(`-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?`)
	ipv4RE   = regexp.MustCompile(`(?:^|[^\d.])(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})(?:[^\d.]|$)`)
	keyRE    = regexp.MustCompile(`"([A-Za-z0-9_]+)"\s*:`)
)

// NsTimestampMin is the smallest integer treated as a nanosecond-precision
// Unix timestamp (1e17 ns is March 1973; ms timestamps are ~1.7e12).
const NsTimestampMin = 1e17

// Scan returns every leak of f found in body. body may be JSON, an SSE
// stream, CSV or text.
func (f *Fixture) Scan(body []byte) []Leak {
	var out []Leak
	out = append(out, f.scanNumbers(body)...)
	for _, m := range ipv4RE.FindAllSubmatchIndex(body, -1) {
		s := string(body[m[2]:m[3]])
		if a, err := netip.ParseAddr(s); err == nil && a.Is4() && !f.allowedIP(a) {
			out = append(out, Leak{Kind: "ip", Detail: s, Offset: m[2]})
		}
	}
	lower := bytes.ToLower(body)
	for _, ip := range f.IPs {
		a, err := netip.ParseAddr(ip)
		if err != nil || f.allowedIP(a) {
			continue
		}
		cands := []string{strings.ToLower(a.String())}
		if a.Is6() && !a.Is4In6() {
			if p, err := a.Prefix(64); err == nil {
				// The /64 printed as its first four groups ("2001:db8:1:2:").
				g := strings.Split(p.Addr().StringExpanded(), ":")
				if len(g) >= 4 {
					cands = append(cands, trimGroups(g[:4]))
				}
			}
		}
		for _, c := range cands {
			if c == "" {
				continue
			}
			if i := bytes.Index(lower, []byte(c)); i >= 0 {
				out = append(out, Leak{Kind: "ip", Detail: ip, Offset: i})
				break
			}
		}
	}
	for _, sid := range f.SIDs {
		if sid == "" {
			continue
		}
		if i := bytes.Index(body, []byte(sid)); i >= 0 {
			out = append(out, Leak{Kind: "sid", Detail: sid, Offset: i})
		}
	}
	for _, s := range f.Secrets {
		if s == "" {
			continue
		}
		if i := bytes.Index(lower, []byte(strings.ToLower(s))); i >= 0 {
			out = append(out, Leak{Kind: "secret", Detail: s, Offset: i})
		}
	}
	if !f.NoKeyCheck {
		allowed := map[string]bool{}
		for _, k := range f.AllowKeys {
			allowed[k] = true
		}
		forbidden := map[string]bool{}
		for _, k := range ForbiddenPublicKeys {
			if !allowed[k] {
				forbidden[k] = true
			}
		}
		for _, m := range keyRE.FindAllSubmatchIndex(body, -1) {
			k := string(body[m[2]:m[3]])
			if forbidden[k] {
				out = append(out, Leak{Kind: "key", Detail: k, Offset: m[2]})
			}
		}
	}
	return out
}

// trimGroups formats expanded IPv6 groups without leading zeros, joined by
// ':' and with a trailing ':' ("2001:db8:1:2:").
func trimGroups(g []string) string {
	var b strings.Builder
	for _, x := range g {
		x = strings.TrimLeft(x, "0")
		if x == "" {
			x = "0"
		}
		b.WriteString(x)
		b.WriteByte(':')
	}
	return b.String()
}

type hit struct {
	off int
	lat bool
	pt  int
}

// scanNumbers finds fixture coordinates and ns timestamps among the
// numbers of body.
func (f *Fixture) scanNumbers(body []byte) []Leak {
	var out []Leak
	var hits []hit
	for _, m := range numberRE.FindAllIndex(body, -1) {
		// Skip digits that are part of an identifier ("a1b2", "st/3").
		if m[0] > 0 {
			c := body[m[0]-1]
			if c == '_' || c == '/' || (c|0x20 >= 'a' && c|0x20 <= 'z') {
				continue
			}
		}
		tok := string(body[m[0]:m[1]])
		v, err := strconv.ParseFloat(tok, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if !strings.ContainsAny(tok, ".eE") && math.Abs(v) >= NsTimestampMin {
			out = append(out, Leak{Kind: "ns-timestamp", Detail: tok, Offset: m[0]})
			continue
		}
		for i, p := range f.Points {
			if coordMatch(v, tok, p.Lat) {
				hits = append(hits, hit{off: m[0], lat: true, pt: i})
			}
			if coordMatch(v, tok, p.Lon) {
				hits = append(hits, hit{off: m[0], lat: false, pt: i})
			}
		}
	}
	reported := map[int]bool{}
	for _, h := range hits {
		p := f.Points[h.pt]
		if reported[h.pt] {
			continue
		}
		if p.Strict {
			reported[h.pt] = true
			out = append(out, Leak{Kind: "coordinate", Detail: fmt.Sprintf("%s (%.5f, %.5f)", p.Label, p.Lat, p.Lon), Offset: h.off})
			continue
		}
		for _, o := range hits {
			if o.pt == h.pt && o.lat != h.lat && abs(o.off-h.off) <= PairWindow {
				reported[h.pt] = true
				out = append(out, Leak{Kind: "coordinate", Detail: fmt.Sprintf("%s (%.5f, %.5f) as a pair", p.Label, p.Lat, p.Lon), Offset: h.off})
				break
			}
		}
	}
	return out
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// coordMatch reports whether number v (token tok) is coordinate c to 3
// decimals, or c scaled by 1e5 / 1e6 / 1e7 as an integer.
// decimals returns the number of decimals of c as Go prints it, at most 3.
func decimals(c float64) int {
	s := strconv.FormatFloat(c, 'f', -1, 64)
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return 0
	}
	return min(3, len(s)-i-1)
}

func coordMatch(v float64, tok string, c float64) bool {
	if c == 0 {
		return false
	}
	if strings.ContainsAny(tok, ".eE") {
		// A number printed with fewer decimals than the coordinate needs
		// (at most 3) is a rounded value, not the coordinate: "50.1" is
		// not a leak of 50.0712, but "50.05" is one of 50.05.
		if i := strings.IndexByte(tok, '.'); i >= 0 && !strings.ContainsAny(tok, "eE") && len(tok)-i-1 < decimals(c) {
			return false
		}
		return math.Abs(v-c) < 0.0005
	}
	for _, s := range []float64{1e5, 1e6, 1e7} {
		if math.Abs(v-c*s) < 0.0005*s && math.Abs(v) >= s {
			return true
		}
	}
	return false
}

// allowedIP reports whether a is one of AllowIPs.
func (f *Fixture) allowedIP(a netip.Addr) bool {
	for _, s := range f.AllowIPs {
		if b, err := netip.ParseAddr(s); err == nil && b.Unmap() == a.Unmap() {
			return true
		}
	}
	return false
}
