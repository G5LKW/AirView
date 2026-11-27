// Package alerts is AirView's alerting rules engine: user-defined rules with
// AND/OR/NOT condition trees over the live aircraft picture and system events,
// pre-compiled for a cheap 1 Hz evaluation, with per-rule and per-aircraft
// cooldowns, de-duplication, watch-lists, map highlights, an event log and
// generic JSON webhooks.
package alerts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Severity levels.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Condition types (Cond.Type).
const (
	CondICAO        = "icao"        // Values: "3C6444", "3C*" (hex glob), "3C0000-3CFFFF" (range)
	CondReg         = "reg"         // Values: globs (* ?), case-insensitive
	CondCallsign    = "callsign"    // Values: globs
	CondOperator    = "operator"    // Values: globs
	CondAircraft    = "actype"      // Values: ICAO type designator globs ("A38*", "B74?")
	CondCategory    = "category"    // Values: emitter category globs ("A7", "B*")
	CondMilitary    = "military"    // Value (default true)
	CondCountry     = "country"     // Values: ISO2 code or country-name globs
	CondSquawk      = "squawk"      // Values: globs ("7700", "76*")
	CondEmergency   = "emergency"   // Values: states ("general","unlawful",...) - empty = any emergency
	CondAltitude    = "altitude"    // Min/Max ft (baro, geometric fallback)
	CondSpeed       = "speed"       // Min/Max kt ground speed
	CondVRate       = "vrate"       // Min/Max ft/min
	CondDistance    = "distance"    // Min/Max km from the receiver
	CondGeofence    = "geofence"    // Circle (Lat, Lon, RadiusKm) or Polygon [[lon,lat]...]; Inside (default true)
	CondBearing     = "bearing"     // From/To degrees true, clockwise sector from the receiver
	CondNewAirframe = "newAirframe" // first time ever seen (NoteNewAirframe); Value (default true)
	CondADSBVersion = "adsbVersion" // Min/Max
	CondNIC         = "nic"         // Min/Max
	CondRSSI        = "rssi"        // Min/Max dBFS (smoothed, last-frame fallback)
	CondSource      = "source"      // Values: adsb, mlat, tisb, adsr, modes, other
	CondWatchlist   = "watchlist"   // Values: list names - empty = any list
	CondOnGround    = "onGround"    // Value (default true)
	CondSystem      = "system"      // Values: system event kinds, optionally "kind:key" globs (e.g. "record:*")
)

// System event kinds (SystemEvent.Kind).
const (
	SysRecord         = "record"
	SysFeedDown       = "feed_down"
	SysFeedUp         = "feed_up"
	SysNTPUnsync      = "ntp_unsync"
	SysNTPSync        = "ntp_sync"
	SysRangeCollapse  = "range_collapse"
	SysRangeRestored  = "range_restored"
	SysCustom         = "custom"
	SysNewAirframeEvt = "new_airframe"
)

var systemKinds = map[string]bool{SysRecord: true, SysFeedDown: true, SysFeedUp: true, SysNTPUnsync: true,
	SysNTPSync: true, SysRangeCollapse: true, SysRangeRestored: true, SysCustom: true, SysNewAirframeEvt: true}

// Cond is one node of a rule's condition tree: either a group (Op with
// Conds) or a leaf (Type with its parameters). Negate inverts any node.
type Cond struct {
	Op     string `json:"op,omitempty"` // "and" | "or" (groups only)
	Conds  []Cond `json:"conds,omitempty"`
	Negate bool   `json:"negate,omitempty"`

	Type     string       `json:"type,omitempty"`
	Values   []string     `json:"values,omitempty"`
	Min      *float64     `json:"min,omitempty"`
	Max      *float64     `json:"max,omitempty"`
	Value    *bool        `json:"value,omitempty"`
	Lat      *float64     `json:"lat,omitempty"`
	Lon      *float64     `json:"lon,omitempty"`
	RadiusKm *float64     `json:"radiusKm,omitempty"`
	Polygon  [][2]float64 `json:"polygon,omitempty"` // [lon, lat] (GeoJSON order), open or closed ring
	Inside   *bool        `json:"inside,omitempty"`
	From     *float64     `json:"from,omitempty"`
	To       *float64     `json:"to,omitempty"`
}

// Actions say what happens when a rule fires.
type Actions struct {
	Notify    bool   `json:"notify"`              // UI toast
	Sound     bool   `json:"sound"`               // UI plays a sound with the toast
	Log       bool   `json:"log"`                 // store event (kind "alert")
	Webhook   string `json:"webhook,omitempty"`   // POST JSON to this http(s) URL
	Highlight string `json:"highlight,omitempty"` // map highlight colour "#rrggbb" while matching
}

// Rule is one alert rule (the JSON stored and exchanged with the UI).
type Rule struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Description     string  `json:"description,omitempty"`
	Enabled         bool    `json:"enabled"`
	Builtin         bool    `json:"builtin,omitempty"` // shipped default (may be edited/disabled; ResetDefaults restores)
	Severity        string  `json:"severity"`
	Match           Cond    `json:"match"`
	CooldownSec     int     `json:"cooldownSec"`               // per aircraft (or per system event key); default 900
	RuleCooldownSec int     `json:"ruleCooldownSec,omitempty"` // minimum gap between any two alerts of this rule
	Message         string  `json:"message,omitempty"`         // template, see Expand
	Actions         Actions `json:"actions"`
	// IncludePredicted lets the position-dependent conditions (altitude,
	// distance, geofence, bearing) match aircraft shown at a predicted
	// (coasting) position. Default false: such a rule ignores those aircraft
	// until a real position is received again.
	IncludePredicted bool  `json:"includePredicted,omitempty"`
	Created          int64 `json:"created,omitempty"` // Unix ms
	Updated          int64 `json:"updated,omitempty"`
}

// Limits for hostile input.
const (
	MaxRuleBytes   = 64 << 10
	MaxRules       = 500
	MaxDepth       = 8
	MaxNodes       = 200
	MaxValues      = 1000
	MaxStringLen   = 128
	MaxNameLen     = 100
	MaxMessageLen  = 500
	MaxPolygon     = 1000
	MaxCooldownSec = 7 * 24 * 3600
	MaxWebhookLen  = 2048
)

// ErrInvalid wraps every validation error.
var ErrInvalid = errors.New("alerts: invalid rule")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

var (
	idRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	colourRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
)

// ParseRule decodes and validates one rule from JSON (unknown fields are
// rejected). The result is normalised (defaults filled in).
func ParseRule(data []byte) (Rule, error) {
	if len(data) > MaxRuleBytes {
		return Rule{}, invalid("rule larger than %d bytes", MaxRuleBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Rule
	if err := dec.Decode(&r); err != nil {
		return Rule{}, invalid("%v", err)
	}
	if dec.More() {
		return Rule{}, invalid("trailing data after the rule")
	}
	return r, r.Normalize()
}

// Normalize fills defaults and validates r in place.
func (r *Rule) Normalize() error {
	r.ID = strings.TrimSpace(r.ID)
	if r.ID != "" && !idRe.MatchString(r.ID) {
		return invalid("id must be 1-64 characters of A-Z a-z 0-9 _ -")
	}
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return invalid("name is required")
	}
	if err := checkText("name", r.Name, MaxNameLen); err != nil {
		return err
	}
	if err := checkText("description", r.Description, MaxMessageLen); err != nil {
		return err
	}
	if err := checkText("message", r.Message, MaxMessageLen); err != nil {
		return err
	}
	switch r.Severity {
	case "":
		r.Severity = SeverityInfo
	case SeverityInfo, SeverityWarning, SeverityCritical:
	default:
		return invalid("severity must be info, warning or critical")
	}
	if r.CooldownSec == 0 {
		r.CooldownSec = 900
	}
	if r.CooldownSec < 0 || r.CooldownSec > MaxCooldownSec || r.RuleCooldownSec < 0 || r.RuleCooldownSec > MaxCooldownSec {
		return invalid("cooldowns must be between 0 and %d s", MaxCooldownSec)
	}
	if r.Actions.Highlight != "" && !colourRe.MatchString(r.Actions.Highlight) {
		return invalid("highlight must be a #rgb or #rrggbb colour")
	}
	if r.Actions.Webhook != "" {
		if err := ValidateWebhookURL(r.Actions.Webhook); err != nil {
			return err
		}
	}
	nodes := 0
	if err := validateCond(&r.Match, 1, &nodes); err != nil {
		return err
	}
	return nil
}

// ValidateWebhookURL checks a user-supplied webhook URL.
func ValidateWebhookURL(s string) error {
	if len(s) > MaxWebhookLen {
		return invalid("webhook URL too long")
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" {
		return invalid("webhook must be an absolute http(s) URL")
	}
	if strings.ContainsAny(s, " \r\n\t") {
		return invalid("webhook URL contains whitespace")
	}
	return nil
}

func checkText(field, s string, max int) error {
	if len(s) > max*4 || utf8.RuneCountInString(s) > max {
		return invalid("%s longer than %d characters", field, max)
	}
	if !utf8.ValidString(s) {
		return invalid("%s is not valid UTF-8", field)
	}
	for _, r := range s {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return invalid("%s contains control characters", field)
		}
	}
	return nil
}

func finite(p *float64) bool { return p == nil || (!math.IsNaN(*p) && !math.IsInf(*p, 0)) }

func validateCond(c *Cond, depth int, nodes *int) error {
	*nodes++
	if *nodes > MaxNodes {
		return invalid("more than %d conditions", MaxNodes)
	}
	if depth > MaxDepth {
		return invalid("conditions nested deeper than %d", MaxDepth)
	}
	if c.Op != "" || len(c.Conds) > 0 {
		if c.Type != "" {
			return invalid("a condition is either a group (op) or a leaf (type), not both")
		}
		c.Op = strings.ToLower(c.Op)
		if c.Op == "" {
			c.Op = "and"
		}
		if c.Op != "and" && c.Op != "or" {
			return invalid("op must be and/or, got %q", c.Op)
		}
		if len(c.Conds) == 0 {
			return invalid("empty %s group", c.Op)
		}
		for i := range c.Conds {
			if err := validateCond(&c.Conds[i], depth+1, nodes); err != nil {
				return err
			}
		}
		return nil
	}
	if c.Type == "" {
		return invalid("condition without type or op")
	}
	for _, p := range []*float64{c.Min, c.Max, c.Lat, c.Lon, c.RadiusKm, c.From, c.To} {
		if !finite(p) {
			return invalid("%s: numbers must be finite", c.Type)
		}
	}
	if len(c.Values) > MaxValues {
		return invalid("%s: more than %d values", c.Type, MaxValues)
	}
	for i, v := range c.Values {
		v = strings.TrimSpace(v)
		if v == "" || len(v) > MaxStringLen || !utf8.ValidString(v) {
			return invalid("%s: value %d is empty, too long or not UTF-8", c.Type, i)
		}
		c.Values[i] = v
	}
	needValues := func() error {
		if len(c.Values) == 0 {
			return invalid("%s: at least one value is required", c.Type)
		}
		return nil
	}
	needRange := func(lo, hi float64) error {
		if c.Min == nil && c.Max == nil {
			return invalid("%s: min and/or max is required", c.Type)
		}
		for _, p := range []*float64{c.Min, c.Max} {
			if p != nil && (*p < lo || *p > hi) {
				return invalid("%s: limits must be within %g..%g", c.Type, lo, hi)
			}
		}
		if c.Min != nil && c.Max != nil && *c.Min > *c.Max {
			return invalid("%s: min > max", c.Type)
		}
		return nil
	}
	switch c.Type {
	case CondICAO:
		if err := needValues(); err != nil {
			return err
		}
		for _, v := range c.Values {
			if _, _, _, err := parseICAOValue(v); err != nil {
				return err
			}
		}
	case CondReg, CondCallsign, CondOperator, CondAircraft, CondCategory, CondCountry, CondSquawk:
		return needValues()
	case CondSource:
		if err := needValues(); err != nil {
			return err
		}
		for _, v := range c.Values {
			switch strings.ToLower(v) {
			case "adsb", "mlat", "tisb", "adsr", "modes", "other":
			default:
				return invalid("source: unknown source %q", v)
			}
		}
	case CondEmergency:
		for _, v := range c.Values {
			switch strings.ToLower(v) {
			case "any", "general", "medical", "minfuel", "nordo", "unlawful", "downed":
			default:
				return invalid("emergency: unknown state %q", v)
			}
		}
	case CondMilitary, CondNewAirframe, CondOnGround:
	case CondWatchlist:
		for _, v := range c.Values {
			if !validListName(v) {
				return invalid("watchlist: bad list name %q", v)
			}
		}
	case CondSystem:
		if err := needValues(); err != nil {
			return err
		}
		for _, v := range c.Values {
			kind, _, _ := strings.Cut(v, ":")
			if !systemKinds[strings.ToLower(kind)] {
				return invalid("system: unknown event kind %q", kind)
			}
		}
	case CondAltitude:
		return needRange(-2000, 100000)
	case CondSpeed:
		return needRange(0, 5000)
	case CondVRate:
		return needRange(-30000, 30000)
	case CondDistance:
		return needRange(0, 20000)
	case CondADSBVersion:
		return needRange(0, 7)
	case CondNIC:
		return needRange(0, 11)
	case CondRSSI:
		return needRange(-100, 10)
	case CondBearing:
		if c.From == nil || c.To == nil || *c.From < 0 || *c.From > 360 || *c.To < 0 || *c.To > 360 {
			return invalid("bearing: from and to (0..360) are required")
		}
	case CondGeofence:
		circle := c.Lat != nil || c.Lon != nil || c.RadiusKm != nil
		if circle == (len(c.Polygon) > 0) {
			return invalid("geofence: give either lat/lon/radiusKm or polygon")
		}
		if circle {
			if c.Lat == nil || c.Lon == nil || c.RadiusKm == nil || *c.Lat < -90 || *c.Lat > 90 || *c.Lon < -180 ||
				*c.Lon > 180 || *c.RadiusKm <= 0 || *c.RadiusKm > 5000 {
				return invalid("geofence: circle needs lat (-90..90), lon (-180..180) and radiusKm (0..5000]")
			}
		} else {
			if len(c.Polygon) < 3 || len(c.Polygon) > MaxPolygon+1 {
				return invalid("geofence: polygon needs 3..%d points", MaxPolygon)
			}
			for _, p := range c.Polygon {
				if math.IsNaN(p[0]) || math.IsNaN(p[1]) || p[0] < -180 || p[0] > 180 || p[1] < -90 || p[1] > 90 {
					return invalid("geofence: polygon point out of range (points are [lon, lat])")
				}
			}
		}
	default:
		return invalid("unknown condition type %q", c.Type)
	}
	return nil
}

// parseICAOValue parses "3C6444", "~3C6444", "3C*" or "3C0000-3CFFFF".
func parseICAOValue(v string) (lo, hi uint32, glob string, err error) {
	v = strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(v), "~"))
	if strings.ContainsAny(v, "*?") {
		if len(v) > 6 || strings.Trim(v, "0123456789ABCDEF*?") != "" {
			return 0, 0, "", invalid("icao: bad pattern %q", v)
		}
		return 0, 0, v, nil
	}
	if a, b, ok := strings.Cut(v, "-"); ok {
		x, e1 := parseHex24(a)
		y, e2 := parseHex24(b)
		if e1 != nil || e2 != nil || x > y {
			return 0, 0, "", invalid("icao: bad range %q", v)
		}
		return x, y, "", nil
	}
	x, e := parseHex24(v)
	if e != nil {
		return 0, 0, "", invalid("icao: bad address %q", v)
	}
	return x, x, "", nil
}

func parseHex24(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 6 {
		return 0, errors.New("bad hex")
	}
	v, err := strconv.ParseUint(s, 16, 32)
	return uint32(v), err
}

var listNameRe = regexp.MustCompile(`^[A-Za-z0-9 _.-]{1,40}$`)

func validListName(s string) bool { return listNameRe.MatchString(s) }
