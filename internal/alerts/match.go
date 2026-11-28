package alerts

import (
	"math"
	"strings"

	"airview/internal/geo"
	"airview/internal/model"
)

// Leaf kinds of the compiled tree.
const (
	kAnd uint8 = iota
	kOr
	kICAO
	kReg
	kCallsign
	kOperator
	kActype
	kCategory
	kMilitary
	kCountry
	kSquawk
	kEmergency
	kAltitude
	kSpeed
	kVRate
	kDistance
	kCircle
	kPolygon
	kBearing
	kNewAirframe
	kADSBVersion
	kNIC
	kRSSI
	kSource
	kWatchlist
	kOnGround
	kSystem
	kRouteFrom
	kRouteTo
	kRouteVia
	kRouteAirport
	kRoute
	kRouteState
	kRouteKind
	kETAWithin
	kACASRA
)

type icaoRange struct{ lo, hi uint32 }

// node is one compiled condition. Evaluation never allocates.
type node struct {
	kind     uint8
	negate   bool
	children []node
	globs    []string // upper-cased patterns
	ranges   []icaoRange
	min, max float64 // NaN = open
	b        bool
	lat, lon float64
	radiusKm float64
	poly     *polygon
	from, to float64
}

// compiled is a validated, compiled rule.
type compiled struct {
	rule   Rule
	root   node
	system bool // contains a system leaf: evaluated only on system events
	// posDep: the tree tests the position or altitude (altitude, distance,
	// geofence, bearing). Such a rule is not evaluated for an aircraft shown
	// at a predicted (coasting) position unless Rule.IncludePredicted.
	posDep bool
}

func compileRule(r Rule) *compiled {
	c := &compiled{rule: r}
	c.root = compileCond(&r.Match, &c.system)
	c.posDep = c.root.positional()
	return c
}

// positional reports whether n or a descendant depends on the aircraft's
// position or altitude.
func (n *node) positional() bool {
	switch n.kind {
	case kAltitude, kDistance, kCircle, kPolygon, kBearing:
		return true
	}
	for i := range n.children {
		if n.children[i].positional() {
			return true
		}
	}
	return false
}

// skip reports whether c must not be evaluated for the aircraft in v: a
// position-dependent rule and an aircraft at a predicted position.
func (c *compiled) skip(v *view) bool {
	return v.predicted && c.posDep && !c.rule.IncludePredicted
}

func optF(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func optB(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func upperAll(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.ToUpper(strings.TrimSpace(v))
	}
	return out
}

func compileCond(c *Cond, system *bool) node {
	n := node{negate: c.Negate, min: optF(c.Min), max: optF(c.Max)}
	if c.Type == "" {
		n.kind = kAnd
		if c.Op == "or" {
			n.kind = kOr
		}
		n.children = make([]node, len(c.Conds))
		for i := range c.Conds {
			n.children[i] = compileCond(&c.Conds[i], system)
		}
		return n
	}
	switch c.Type {
	case CondICAO:
		n.kind = kICAO
		for _, v := range c.Values {
			lo, hi, g, _ := parseICAOValue(v)
			if g != "" {
				n.globs = append(n.globs, g)
			} else {
				n.ranges = append(n.ranges, icaoRange{lo, hi})
			}
		}
	case CondReg:
		n.kind, n.globs = kReg, upperAll(c.Values)
	case CondCallsign:
		n.kind, n.globs = kCallsign, upperAll(c.Values)
	case CondOperator:
		n.kind, n.globs = kOperator, upperAll(c.Values)
	case CondAircraft:
		n.kind, n.globs = kActype, upperAll(c.Values)
	case CondCategory:
		n.kind, n.globs = kCategory, upperAll(c.Values)
	case CondCountry:
		n.kind, n.globs = kCountry, upperAll(c.Values)
	case CondSquawk:
		n.kind, n.globs = kSquawk, upperAll(c.Values)
	case CondSource:
		n.kind, n.globs = kSource, upperAll(c.Values)
	case CondWatchlist:
		n.kind, n.globs = kWatchlist, c.Values // list names are case-sensitive keys
	case CondSystem:
		n.kind, n.globs = kSystem, upperAll(c.Values)
		*system = true
	case CondRouteFrom:
		n.kind, n.globs = kRouteFrom, upperAll(c.Values)
	case CondRouteTo:
		n.kind, n.globs = kRouteTo, upperAll(c.Values)
	case CondRouteVia:
		n.kind, n.globs = kRouteVia, upperAll(c.Values)
	case CondRouteAirport:
		n.kind, n.globs = kRouteAirport, upperAll(c.Values)
	case CondRoute:
		// "FROM-TO" patterns are stored as (from glob, to glob) pairs: matching needs no string building
		n.kind = kRoute
		for _, v := range upperAll(c.Values) {
			f, t, _ := strings.Cut(v, "-")
			n.globs = append(n.globs, f, t)
		}
	case CondRouteState:
		n.kind, n.globs = kRouteState, upperAll(c.Values)
	case CondRouteKind:
		n.kind, n.globs = kRouteKind, upperAll(c.Values)
	case CondETAWithin:
		n.kind, n.max = kETAWithin, optF(c.Max)
	case CondACASRA:
		n.kind, n.b = kACASRA, optB(c.Value, true)
		n.globs = upperAll(c.Values)
	case CondEmergency:
		n.kind = kEmergency
		for _, v := range upperAll(c.Values) {
			if v == "ANY" {
				n.globs = nil
				break
			}
			n.globs = append(n.globs, v)
		}
	case CondMilitary:
		n.kind, n.b = kMilitary, optB(c.Value, true)
	case CondNewAirframe:
		n.kind, n.b = kNewAirframe, optB(c.Value, true)
	case CondOnGround:
		n.kind, n.b = kOnGround, optB(c.Value, true)
	case CondAltitude:
		n.kind = kAltitude
	case CondSpeed:
		n.kind = kSpeed
	case CondVRate:
		n.kind = kVRate
	case CondDistance:
		n.kind = kDistance
	case CondADSBVersion:
		n.kind = kADSBVersion
	case CondNIC:
		n.kind = kNIC
	case CondRSSI:
		n.kind = kRSSI
	case CondBearing:
		n.kind, n.from, n.to = kBearing, geo.NormalizeDeg(*c.From), geo.NormalizeDeg(*c.To)
		if *c.To == 360 && *c.From == 0 {
			n.from, n.to = 0, 360
		}
	case CondGeofence:
		n.b = optB(c.Inside, true)
		if len(c.Polygon) > 0 {
			n.kind, n.poly = kPolygon, newPolygon(c.Polygon)
		} else {
			n.kind, n.lat, n.lon, n.radiusKm = kCircle, *c.Lat, *c.Lon, *c.RadiusKm
		}
	}
	return n
}

// view is the per-aircraft data prepared once per Evaluate (no allocation).
type view struct {
	ac     *model.Aircraft
	icao   uint32
	icaoOK bool
	hasPos bool
	// predicted: the position (and the values derived from it) is the
	// tracking filter's prediction, not a received one.
	predicted bool
	lat, lon  float64
	alt       float64 // NaN unknown
	newAF     bool
	watch     *watchSet
	sys       *SystemEvent
	sysKind   string // "KIND" upper case
	sysKey    string // "KIND:KEY" upper case
	nowS      int64  // evaluation time, Unix s (etaWithin)
}

func (v *view) set(a *model.Aircraft) {
	v.ac = a
	v.icao, v.icaoOK = parseHexICAO(a.ICAO)
	v.hasPos = a.Lat != nil && a.Lon != nil && geo.ValidLatLon(*a.Lat, *a.Lon)
	v.predicted = a.Predicted
	if v.hasPos {
		v.lat, v.lon = *a.Lat, *a.Lon
	}
	switch {
	case a.AltBaro != nil:
		v.alt = *a.AltBaro
	case a.AltGeom != nil:
		v.alt = *a.AltGeom
	default:
		v.alt = math.NaN()
	}
}

// parseHexICAO parses up to 6 hex digits with an optional "~" prefix.
func parseHexICAO(s string) (uint32, bool) {
	if len(s) > 0 && s[0] == '~' {
		s = s[1:]
	}
	if len(s) == 0 || len(s) > 6 {
		return 0, false
	}
	var v uint32
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint32(c-'0')
		case c >= 'A' && c <= 'F':
			v = v<<4 | uint32(c-'A'+10)
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint32(c-'a'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

func inRange(x, lo, hi float64) bool {
	if math.IsNaN(x) {
		return false
	}
	return (math.IsNaN(lo) || x >= lo) && (math.IsNaN(hi) || x <= hi)
}

func fv(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func iv(p *int) float64 {
	if p == nil {
		return math.NaN()
	}
	return float64(*p)
}

func anyGlob(globs []string, s string) bool {
	if s == "" {
		return false
	}
	for _, g := range globs {
		if globMatch(g, s) {
			return true
		}
	}
	return false
}

// eval evaluates n for v.
func (n *node) eval(v *view) bool {
	r := n.evalRaw(v)
	if n.negate {
		return !r
	}
	return r
}

func (n *node) evalRaw(v *view) bool {
	if n.kind == kSystem {
		return v.sys != nil && (anyGlob(n.globs, v.sysKind) || anyGlob(n.globs, v.sysKey))
	}
	if n.kind == kAnd {
		for i := range n.children {
			if !n.children[i].eval(v) {
				return false
			}
		}
		return true
	}
	if n.kind == kOr {
		for i := range n.children {
			if n.children[i].eval(v) {
				return true
			}
		}
		return false
	}
	a := v.ac
	if a == nil {
		return false
	}
	switch n.kind {
	case kICAO:
		if !v.icaoOK {
			return false
		}
		for _, r := range n.ranges {
			if v.icao >= r.lo && v.icao <= r.hi {
				return true
			}
		}
		if len(n.globs) > 0 {
			var buf [6]byte
			const hexd = "0123456789ABCDEF"
			for i := 0; i < 6; i++ {
				buf[i] = hexd[(v.icao>>(20-4*uint(i)))&0xF]
			}
			for _, g := range n.globs {
				if globMatchBytes(g, buf[:]) {
					return true
				}
			}
		}
		return false
	case kReg:
		return anyGlob(n.globs, a.Registration)
	case kCallsign:
		return anyGlob(n.globs, strings.TrimSpace(a.Callsign))
	case kOperator:
		return anyGlob(n.globs, a.Operator)
	case kActype:
		return anyGlob(n.globs, a.TypeCode)
	case kCategory:
		return anyGlob(n.globs, a.Category)
	case kCountry:
		return anyGlob(n.globs, a.CountryCode) || anyGlob(n.globs, a.Country)
	case kSquawk:
		return anyGlob(n.globs, a.Squawk)
	case kSource:
		return anyGlob(n.globs, a.Source)
	case kEmergency:
		if a.Emergency == "" || a.Emergency == "none" {
			return false
		}
		return len(n.globs) == 0 || anyGlob(n.globs, a.Emergency)
	case kRouteFrom:
		r := a.Route
		return r != nil && (anyGlob(n.globs, r.From) || anyGlob(n.globs, r.FromIATA))
	case kRouteTo:
		r := a.Route
		return r != nil && (anyGlob(n.globs, r.To) || anyGlob(n.globs, r.ToIATA))
	case kRouteVia:
		r := a.Route
		if r == nil || len(r.Stops) < 3 {
			return false
		}
		for _, s := range r.Stops[1 : len(r.Stops)-1] {
			if anyGlob(n.globs, s) {
				return true
			}
		}
		return false
	case kRouteAirport:
		r := a.Route
		if r == nil {
			return false
		}
		if anyGlob(n.globs, r.From) || anyGlob(n.globs, r.FromIATA) || anyGlob(n.globs, r.To) || anyGlob(n.globs, r.ToIATA) {
			return true
		}
		for _, s := range r.Stops {
			if anyGlob(n.globs, s) {
				return true
			}
		}
		return false
	case kRoute:
		r := a.Route
		if r == nil || r.From == "" || r.To == "" {
			return false
		}
		for i := 0; i+1 < len(n.globs); i += 2 {
			if globMatch(n.globs[i], r.From) && globMatch(n.globs[i+1], r.To) ||
				r.FromIATA != "" && r.ToIATA != "" && globMatch(n.globs[i], r.FromIATA) && globMatch(n.globs[i+1], r.ToIATA) {
				return true
			}
		}
		return false
	case kRouteState:
		return a.Route != nil && anyGlob(n.globs, a.Route.State)
	case kRouteKind:
		return a.Route != nil && anyGlob(n.globs, a.Route.Kind)
	case kETAWithin:
		r := a.Route
		if r == nil || r.ETA <= 0 || v.nowS <= 0 {
			return false
		}
		left := float64(r.ETA-v.nowS) / 60
		return left >= -1 && left <= n.max
	case kACASRA:
		ra := a.ACAS
		if ra == nil {
			return !n.b
		}
		if !n.b {
			return false
		}
		return len(n.globs) == 0 || anyGlob(n.globs, ra.Sense) || anyGlob(n.globs, ra.Advisory)
	case kMilitary:
		return a.Military == n.b
	case kOnGround:
		return a.OnGround != nil && *a.OnGround == n.b
	case kNewAirframe:
		return v.newAF == n.b
	case kAltitude:
		return inRange(v.alt, n.min, n.max)
	case kSpeed:
		return inRange(fv(a.GS), n.min, n.max)
	case kVRate:
		return inRange(fv(a.VertRate), n.min, n.max)
	case kDistance:
		return inRange(fv(a.DistKm), n.min, n.max)
	case kADSBVersion:
		return inRange(iv(a.ADSBVersion), n.min, n.max)
	case kNIC:
		return inRange(iv(a.NIC), n.min, n.max)
	case kRSSI:
		x := fv(a.RSSIAvg)
		if math.IsNaN(x) {
			x = fv(a.RSSI)
		}
		return inRange(x, n.min, n.max)
	case kBearing:
		if a.Bearing == nil {
			return false
		}
		return inSector(*a.Bearing, n.from, n.to)
	case kCircle:
		if !v.hasPos {
			return false
		}
		return (geo.HaversineKm(n.lat, n.lon, v.lat, v.lon) <= n.radiusKm) == n.b
	case kPolygon:
		if !v.hasPos {
			return false
		}
		return n.poly.contains(v.lon, v.lat) == n.b
	case kWatchlist:
		if !v.icaoOK || v.watch == nil {
			return false
		}
		return v.watch.contains(n.globs, v.icao)
	}
	return false
}

// inSector reports whether brg lies in the clockwise sector from..to.
func inSector(brg, from, to float64) bool {
	if from == 0 && to == 360 {
		return true
	}
	brg = geo.NormalizeDeg(brg)
	if from <= to {
		return brg >= from && brg <= to
	}
	return brg >= from || brg <= to
}

func upper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 32
	}
	return c
}

// globMatch matches s against an upper-case pattern with * and ?
// (ASCII case-insensitive, iterative, allocation-free).
func globMatch(p, s string) bool {
	px, sx := 0, 0
	nextP, nextS := -1, -1
	for px < len(p) || sx < len(s) {
		if px < len(p) {
			switch c := p[px]; c {
			case '*':
				nextP, nextS = px, sx+1
				px++
				continue
			case '?':
				if sx < len(s) {
					px++
					sx++
					continue
				}
			default:
				if sx < len(s) && upper(s[sx]) == c {
					px++
					sx++
					continue
				}
			}
		}
		if nextS > 0 && nextS <= len(s) {
			px, sx = nextP, nextS
			continue
		}
		return false
	}
	return true
}

func globMatchBytes(p string, s []byte) bool {
	// Small fixed-size conversion without allocation.
	var buf [8]byte
	n := copy(buf[:], s)
	return globMatchArr(p, &buf, n)
}

func globMatchArr(p string, b *[8]byte, n int) bool {
	px, sx := 0, 0
	nextP, nextS := -1, -1
	for px < len(p) || sx < n {
		if px < len(p) {
			switch c := p[px]; c {
			case '*':
				nextP, nextS = px, sx+1
				px++
				continue
			case '?':
				if sx < n {
					px++
					sx++
					continue
				}
			default:
				if sx < n && upper(b[sx]) == c {
					px++
					sx++
					continue
				}
			}
		}
		if nextS > 0 && nextS <= n {
			px, sx = nextP, nextS
			continue
		}
		return false
	}
	return true
}
