package refdata

import (
	"strings"

	"airview/internal/model"
)

// Airline is one entry of the embedded operator designator table.
type Airline struct {
	Designator  string `json:"designator"`  // ICAO three-letter designator, e.g. "BAW"
	Name        string `json:"name"`        // operator name, e.g. "British Airways"
	Telephony   string `json:"telephony"`   // radiotelephony callsign, e.g. "SPEEDBIRD" ("" if none)
	CountryCode string `json:"countryCode"` // ISO 3166-1 alpha-2
	// Type is "airline", "cargo", "business", "military", "government" or "other".
	Type string `json:"type"`
}

// Country returns the English country name of the operator.
func (a Airline) Country() string { return CountryName(a.CountryCode) }

// Military reports whether the operator is a military one.
func (a Airline) Military() bool { return a.Type == OperatorMilitary }

// Operator type strings used in Airline.Type and CallsignInfo.OperatorType.
const (
	OperatorAirline    = "airline"
	OperatorCargo      = "cargo"
	OperatorBusiness   = "business"
	OperatorMilitary   = "military"
	OperatorGovernment = "government"
	OperatorOther      = "other"
)

// airlines holds the parsed airlineTable; airlineIndex maps the designator
// (packed base-26) to index+1 into airlines. Both are written once during
// package initialisation and are read-only afterwards.
var (
	airlines     []Airline
	airlineIndex [26 * 26 * 26]uint16
)

func init() {
	airlines = make([]Airline, 0, 640)
	rest := airlineTable
	for len(rest) > 0 {
		var line string
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			line, rest = rest, ""
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var f [5]string
		n := 0
		for n < 4 {
			i := strings.IndexByte(line, '|')
			if i < 0 {
				break
			}
			f[n], line = line[:i], line[i+1:]
			n++
		}
		f[n] = line
		if n != 4 || len(f[0]) != 3 {
			continue // malformed line: ignored (the tests assert there are none)
		}
		k, ok := designatorKey(f[0])
		if !ok || airlineIndex[k] != 0 {
			continue
		}
		a := Airline{Designator: f[0], Name: f[1], Telephony: f[2], CountryCode: f[3]}
		switch f[4] {
		case "P":
			a.Type = OperatorAirline
		case "C":
			a.Type = OperatorCargo
		case "B":
			a.Type = OperatorBusiness
		case "M":
			a.Type = OperatorMilitary
		case "G":
			a.Type = OperatorGovernment
		default:
			a.Type = OperatorOther
		}
		airlines = append(airlines, a)
		airlineIndex[k] = uint16(len(airlines))
	}
}

// designatorKey packs three upper-case letters into 0..17575.
func designatorKey(s string) (int, bool) {
	if len(s) < 3 {
		return 0, false
	}
	a, b, c := s[0]-'A', s[1]-'A', s[2]-'A'
	if a > 25 || b > 25 || c > 25 {
		return 0, false
	}
	return int(a)*676 + int(b)*26 + int(c), true
}

// LookupAirline returns the operator for an ICAO three-letter designator
// (upper case). It does not allocate.
func LookupAirline(designator string) (Airline, bool) {
	if len(designator) != 3 {
		return Airline{}, false
	}
	k, ok := designatorKey(designator)
	if !ok || airlineIndex[k] == 0 {
		return Airline{}, false
	}
	return airlines[airlineIndex[k]-1], true
}

// AirlineCount returns the number of operators in the embedded table.
func AirlineCount() int { return len(airlines) }

// CallsignKind classifies a transmitted flight identification.
type CallsignKind uint8

const (
	// CallsignUnknown: empty or not interpretable.
	CallsignUnknown CallsignKind = iota
	// CallsignFlight: operator designator + flight number ("BAW123A").
	CallsignFlight
	// CallsignRegistration: the aircraft registration used as callsign
	// ("GBYTE" = G-BYTE), typical for general aviation.
	CallsignRegistration
	// CallsignMilitary: a recognised military word callsign ("NATO01").
	CallsignMilitary
	// CallsignOther: syntactically fine but matches nothing known.
	CallsignOther
)

// String returns "unknown", "flight", "registration", "military" or "other".
func (k CallsignKind) String() string {
	switch k {
	case CallsignFlight:
		return "flight"
	case CallsignRegistration:
		return "registration"
	case CallsignMilitary:
		return "military"
	case CallsignOther:
		return "other"
	}
	return "unknown"
}

// MarshalText makes CallsignKind serialise as its String form in JSON.
func (k CallsignKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// CallsignInfo is the interpretation of a flight identification.
type CallsignInfo struct {
	Callsign string       `json:"callsign"` // normalised: trimmed, upper case
	Kind     CallsignKind `json:"kind"`
	// Flight callsigns: the three-letter designator and the rest. Designator
	// is set for every callsign of the form LLL + digit..., even when the
	// operator is not in the embedded table (then Operator is "").
	Designator   string `json:"designator,omitempty"`
	FlightNumber string `json:"flightNumber,omitempty"`
	Operator     string `json:"operator,omitempty"`     // operator name
	OperatorType string `json:"operatorType,omitempty"` // Operator* constant
	Telephony    string `json:"telephony,omitempty"`    // radiotelephony callsign
	Country      string `json:"country,omitempty"`      // operator / registration country
	CountryCode  string `json:"countryCode,omitempty"`  // ISO 3166-1 alpha-2
	Military     bool   `json:"military,omitempty"`     // military operator or military word callsign
	// Registration is the formatted mark ("G-BYTE", "N123AB") when Kind is
	// CallsignRegistration.
	Registration string `json:"reg,omitempty"`
}

// Spoken returns how the callsign is said on the radio when the operator's
// telephony designator is known ("SPEEDBIRD 123A"), otherwise "".
func (c CallsignInfo) Spoken() string {
	if c.Kind != CallsignFlight || c.Telephony == "" {
		return ""
	}
	return c.Telephony + " " + c.FlightNumber
}

// Apply copies what the callsign tells into the live aircraft DTO without
// overwriting anything: the operator name when the DTO has none, the
// registration of a registration callsign when the DTO has none, and the
// military flag for military operators / word callsigns (only ever set).
// Use it after Info.Apply and ACRecord.Apply, and build c with
// LookupCallsignFor so that registrations are checked against the address.
func (c CallsignInfo) Apply(a *model.Aircraft) {
	if a == nil {
		return
	}
	if a.Operator == "" && c.Operator != "" {
		a.Operator = c.Operator
	}
	if a.Registration == "" && c.Kind == CallsignRegistration {
		a.Registration = c.Registration
	}
	if c.Military {
		a.Military = true
	}
}

// LookupCallsign interprets a flight identification as transmitted in ADS-B
// identification messages or Mode S BDS 2,0 (padding is ignored).
//
// It is safe for concurrent use. It does not allocate when cs is already
// upper case, except for the formatted Registration of registration
// callsigns.
//
// Without the aircraft address a five-letter word such as "MAGIC" cannot be
// told apart from the registration M-AGIC; use LookupCallsignFor when the
// address is known.
func LookupCallsign(cs string) CallsignInfo { return lookupCallsign(cs, 0) }

// LookupCallsignFor is LookupCallsign with a plausibility check against the
// aircraft's 24-bit address: a callsign is only reported as a registration
// when the nationality mark belongs to the state (or territory) the address
// was allocated to. An icao of 0 disables the check.
func LookupCallsignFor(cs string, icao uint32) CallsignInfo { return lookupCallsign(cs, icao) }

func lookupCallsign(cs string, icao uint32) CallsignInfo {
	cs = NormalizeCallsign(cs)
	info := CallsignInfo{Callsign: cs}
	if cs == "" {
		return info
	}
	for i := 0; i < len(cs); i++ {
		if !isAlnum(cs[i]) {
			info.Kind = CallsignOther
			return info
		}
	}

	// Operator designator + flight number: LLL D [alnum]{0,4}.
	flight := len(cs) >= 4 && len(cs) <= 8 &&
		isUpper(cs[0]) && isUpper(cs[1]) && isUpper(cs[2]) && isDigit(cs[3])
	if flight {
		if a, ok := LookupAirline(cs[:3]); ok {
			info.Kind = CallsignFlight
			info.Designator = cs[:3]
			info.FlightNumber = cs[3:]
			info.Operator = a.Name
			info.OperatorType = a.Type
			info.Telephony = a.Telephony
			info.CountryCode = a.CountryCode
			info.Country = CountryName(a.CountryCode)
			info.Military = a.Type == OperatorMilitary
			return info
		}
	}

	// Registration used as callsign.
	if p := matchRegistration(cs); p != nil && regPlausible(p, icao) {
		info.Kind = CallsignRegistration
		info.CountryCode = p.iso
		info.Country = CountryName(p.iso)
		if p.hyphen > 0 {
			info.Registration = cs[:p.hyphen] + "-" + cs[p.hyphen:]
		} else {
			info.Registration = cs
		}
		return info
	}

	// Military word callsigns.
	for i := range militaryWords {
		w := &militaryWords[i]
		if len(cs) > len(w.word) && cs[:len(w.word)] == w.word && isDigit(cs[len(w.word)]) {
			info.Kind = CallsignMilitary
			info.Military = true
			info.Operator = w.operator
			info.OperatorType = OperatorMilitary
			info.Telephony = w.word
			info.FlightNumber = cs[len(w.word):]
			info.CountryCode = w.iso
			info.Country = CountryName(w.iso)
			return info
		}
	}

	if flight {
		// Looks like an airline flight of an operator we do not know.
		info.Kind = CallsignFlight
		info.Designator = cs[:3]
		info.FlightNumber = cs[3:]
		return info
	}
	info.Kind = CallsignOther
	return info
}

// NormalizeCallsign trims padding (spaces, NUL, '@', '_', '#') and upper-cases
// the flight identification. It returns a sub-slice of the argument when no
// case conversion is needed.
func NormalizeCallsign(cs string) string {
	i, j := 0, len(cs)
	for i < j && isPad(cs[i]) {
		i++
	}
	for j > i && isPad(cs[j-1]) {
		j--
	}
	cs = cs[i:j]
	for k := 0; k < len(cs); k++ {
		if c := cs[k]; c >= 'a' && c <= 'z' {
			return strings.ToUpper(cs)
		}
	}
	return cs
}

func isPad(c byte) bool   { return c == ' ' || c == 0 || c == '@' || c == '_' || c == '#' }
func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlnum(c byte) bool { return isUpper(c) || isDigit(c) }

// militaryWord is a callsign word used by a military operator in the flight
// identification instead of a three-letter designator.
type militaryWord struct {
	word     string
	operator string
	iso      string
}

// militaryWords: a deliberately short list of unambiguous, frequently seen
// words. Heuristic, like everything "military" in this package.
var militaryWords = []militaryWord{
	{"NATO", "NATO Airborne Early Warning & Control Force", ""},
	{"SPAR", "United States Air Force (special air mission)", "US"},
	{"EVAC", "United States Air Force (aeromedical evacuation)", "US"},
	{"FORTE", "United States Air Force (RQ-4 Global Hawk)", "US"},
	{"JAKE", "United States Air Force (RC-135)", "US"},
	{"LAGR", "United States Air Force (air refuelling)", "US"},
	{"QUID", "United States Air Force (air refuelling)", "US"},
	{"SNTRY", "United States Air Force (E-3 Sentry)", "US"},
	{"IRON", "United States Navy (E-6B Mercury)", "US"},
	{"REDEYE", "United States Air Force (E-8 JSTARS)", "US"},
	{"TARTN", "Royal Air Force (air refuelling)", "GB"},
}

// regPattern describes how one state's registration marks look when sent
// without the hyphen as a callsign.
type regPattern struct {
	prefix string // leading characters that must match exactly
	hyphen int    // position of the hyphen in the formatted mark; 0 = none
	suffix string // pattern for the rest: L letter, D digit, X letter or digit
	iso    string // ISO country of the nationality mark
	iso2   string // alternative ISO accepted by the address check ("" = none)
}

// regPatterns lists the common forms. Patterns containing only letters can
// never collide with an airline flight (which needs a digit in position 4).
var regPatterns = []regPattern{
	// Single-character marks followed by four letters.
	{"G", 1, "LLLL", "GB", ""},
	{"D", 1, "LLLL", "DE", ""},
	{"F", 1, "LLLL", "FR", ""},
	{"I", 1, "LLLL", "IT", ""},
	{"M", 1, "LLLL", "IM", "GB"},
	{"2", 1, "LLLL", "GG", "GB"},
	// Canada.
	{"CF", 1, "LLL", "CA", ""},
	{"CG", 1, "LLL", "CA", ""},
	{"CI", 1, "LLL", "CA", ""},
	// China, Taiwan, Hong Kong, Macau.
	{"BH", 1, "LL", "HK", "CN"},
	{"BK", 1, "LL", "HK", "CN"},
	{"BL", 1, "LL", "HK", "CN"},
	{"BM", 1, "LL", "MO", "CN"},
	{"B", 1, "DDDDD", "TW", ""},
	{"B", 1, "DDDD", "CN", ""},
	{"B", 1, "DDDL", "CN", ""},
	{"B", 1, "DDLL", "CN", ""},
	// Japan, South Korea: no hyphen.
	{"JA", 0, "DDDD", "JP", ""},
	{"JA", 0, "DDDL", "JP", ""},
	{"JA", 0, "DDLL", "JP", ""},
	{"HL", 0, "DDDD", "KR", ""},
	// Two-character marks followed by three letters.
	{"OO", 2, "LLL", "BE", ""},
	{"OY", 2, "LLL", "DK", ""},
	{"OH", 2, "LLL", "FI", ""},
	{"SX", 2, "LLL", "GR", ""},
	{"CS", 2, "LLL", "PT", ""},
	{"YR", 2, "LLL", "RO", ""},
	{"TC", 2, "LLL", "TR", ""},
	{"HB", 2, "LLL", "CH", ""},
	{"OE", 2, "LLL", "AT", ""},
	{"PH", 2, "LLL", "NL", ""},
	{"SE", 2, "LLL", "SE", ""},
	{"LN", 2, "LLL", "NO", ""},
	{"EI", 2, "LLL", "IE", ""},
	{"EC", 2, "LLL", "ES", ""},
	{"SP", 2, "LLL", "PL", ""},
	{"OK", 2, "LLL", "CZ", ""},
	{"OM", 2, "LLL", "SK", ""},
	{"HA", 2, "LLL", "HU", ""},
	{"LZ", 2, "LLL", "BG", ""},
	{"9A", 2, "LLL", "HR", ""},
	{"S5", 2, "LLL", "SI", ""},
	{"YU", 2, "LLL", "RS", ""},
	{"Z3", 2, "LLL", "MK", ""},
	{"9H", 2, "LLL", "MT", ""},
	{"LX", 2, "LLL", "LU", ""},
	{"TF", 2, "LLL", "IS", ""},
	{"ES", 2, "LLL", "EE", ""},
	{"YL", 2, "LLL", "LV", ""},
	{"LY", 2, "LLL", "LT", ""},
	{"UR", 2, "LLL", "UA", ""},
	{"ER", 2, "LLL", "MD", ""},
	{"4L", 2, "LLL", "GE", ""},
	{"4K", 2, "LLL", "AZ", ""},
	{"4X", 2, "LLL", "IL", ""},
	{"5B", 2, "LLL", "CY", ""},
	{"T7", 2, "LLL", "SM", ""},
	{"P4", 2, "LLL", "AW", ""},
	{"ZS", 2, "LLL", "ZA", ""},
	{"ZK", 2, "LLL", "NZ", ""},
	{"VH", 2, "LLL", "AU", ""},
	{"VT", 2, "LLL", "IN", ""},
	{"9V", 2, "LLL", "SG", ""},
	{"9M", 2, "LLL", "MY", ""},
	{"HS", 2, "LLL", "TH", ""},
	{"PK", 2, "LLL", "ID", ""},
	{"AP", 2, "LLL", "PK", ""},
	{"A6", 2, "LLL", "AE", ""},
	{"A7", 2, "LLL", "QA", ""},
	{"HZ", 2, "LLL", "SA", ""},
	{"HZ", 2, "LLDD", "SA", ""},
	{"9K", 2, "LLL", "KW", ""},
	{"JY", 2, "LLL", "JO", ""},
	{"OD", 2, "LLL", "LB", ""},
	{"EP", 2, "LLL", "IR", ""},
	{"YI", 2, "LLL", "IQ", ""},
	{"YK", 2, "LLL", "SY", ""},
	{"SU", 2, "LLL", "EG", ""},
	{"CN", 2, "LLL", "MA", ""},
	{"TS", 2, "LLL", "TN", ""},
	{"7T", 2, "LLL", "DZ", ""},
	{"ET", 2, "LLL", "ET", ""},
	{"5Y", 2, "LLL", "KE", ""},
	{"5H", 2, "LLL", "TZ", ""},
	{"5N", 2, "LLL", "NG", ""},
	{"LV", 2, "LLL", "AR", ""},
	{"CC", 2, "LLL", "CL", ""},
	{"CX", 2, "LLL", "UY", ""},
	{"PP", 2, "LLL", "BR", ""},
	{"PR", 2, "LLL", "BR", ""},
	{"PS", 2, "LLL", "BR", ""},
	{"PT", 2, "LLL", "BR", ""},
	{"PU", 2, "LLL", "BR", ""},
	{"XA", 2, "LLL", "MX", ""},
	{"XB", 2, "LLL", "MX", ""},
	{"XC", 2, "LLL", "MX", ""},
	{"HC", 2, "LLL", "EC", ""},
	{"TI", 2, "LLL", "CR", ""},
	{"TG", 2, "LLL", "GT", ""},
	{"C6", 2, "LLL", "BS", ""},
	// Marks followed by digits.
	{"RA", 2, "DDDDD", "RU", ""},
	{"EW", 2, "DDDLL", "BY", ""},
	{"EK", 2, "DDDDD", "AM", ""},
	{"UK", 2, "DDDDD", "UZ", ""},
	{"EX", 2, "DDDDD", "KG", ""},
	{"UP", 2, "LDDDD", "KZ", ""},
	{"VN", 2, "LDDD", "VN", ""},
	{"RP", 2, "CDDDD", "PH", ""},
	{"HK", 2, "DDDD", "CO", ""},
	{"HK", 2, "DDDDL", "CO", ""},
	{"YV", 2, "DDDD", "VE", ""},
	{"OB", 2, "DDDD", "PE", ""},
	{"CP", 2, "DDDD", "BO", ""},
	// British overseas territories.
	{"VPB", 2, "LL", "BM", "GB"},
	{"VQB", 2, "LL", "BM", "GB"},
	{"VPC", 2, "LL", "KY", "GB"},
	// Oman.
	{"A4O", 3, "LL", "OM", ""},
}

// regByFirst indexes regPatterns by the first prefix byte (read-only after init).
var regByFirst [128][]uint8

func init() {
	for i := range regPatterns {
		c := regPatterns[i].prefix[0]
		regByFirst[c] = append(regByFirst[c], uint8(i))
	}
}

// matchRegistration returns the pattern cs matches, or nil. US N-numbers are
// handled separately because of their variable form.
func matchRegistration(cs string) *regPattern {
	if len(cs) < 2 || cs[0] >= 128 {
		return nil
	}
	if cs[0] == 'N' && isNNumber(cs) {
		return &usPattern
	}
	for _, idx := range regByFirst[cs[0]] {
		p := &regPatterns[idx]
		if len(cs) != len(p.prefix)+len(p.suffix) || cs[:len(p.prefix)] != p.prefix {
			continue
		}
		ok := true
		rest := cs[len(p.prefix):]
		for i := 0; i < len(rest) && ok; i++ {
			switch p.suffix[i] {
			case 'L':
				ok = isUpper(rest[i])
			case 'D':
				ok = isDigit(rest[i])
			case 'X':
				ok = isAlnum(rest[i])
			default: // literal character
				ok = rest[i] == p.suffix[i]
			}
		}
		if ok {
			return p
		}
	}
	return nil
}

var usPattern = regPattern{prefix: "N", iso: "US"}

// isNNumber reports whether s is a syntactically valid US registration:
// N, a digit 1-9, then up to four more characters of which only the last two
// may be letters (never I or O).
func isNNumber(s string) bool {
	if len(s) < 2 || len(s) > 6 || s[0] != 'N' || s[1] < '1' || s[1] > '9' {
		return false
	}
	letters := 0
	for i := 2; i < len(s); i++ {
		c := s[i]
		switch {
		case isDigit(c):
			if letters > 0 {
				return false
			}
		case isUpper(c) && c != 'I' && c != 'O':
			letters++
			if letters > 2 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// regPlausible checks the nationality mark against the address allocation.
func regPlausible(p *regPattern, icao uint32) bool {
	if icao == 0 {
		return true
	}
	c, kind, _ := allocation(icao, true)
	if kind != AllocState && kind != AllocTerritory {
		return true // unknown block: nothing to contradict the mark
	}
	if c.ISO2 == p.iso || (p.iso2 != "" && c.ISO2 == p.iso2) {
		return true
	}
	if kind == AllocTerritory {
		// A territory sub-block: also accept the parent state's marks.
		if s, _, _ := allocation(icao, false); s.ISO2 == p.iso || (p.iso2 != "" && s.ISO2 == p.iso2) {
			return true
		}
	}
	// Liechtenstein uses Swiss marks and addresses; Taiwan / Hong Kong / Macau
	// all use the B- mark.
	return false
}
