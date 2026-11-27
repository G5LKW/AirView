package refdata

import "strings"

// TypeRole is AirView's coarse grouping of aircraft types (used for map icons
// and statistics). It is not an ICAO concept.
type TypeRole string

const (
	RoleAirliner   TypeRole = "airliner"   // mainline jet / large propliner
	RoleRegional   TypeRole = "regional"   // regional jet or commuter turboprop
	RoleFreighter  TypeRole = "freighter"  // dedicated cargo / outsize transport
	RoleBusiness   TypeRole = "business"   // business jet
	RoleUtility    TypeRole = "utility"    // turboprop single / twin
	RoleGA         TypeRole = "ga"         // piston general aviation, warbirds
	RoleLight      TypeRole = "light"      // ultralight, glider, balloon, airship
	RoleHelicopter TypeRole = "helicopter" // civil helicopter
	RoleMilitary   TypeRole = "military"   // military type (any class)
	RoleUAV        TypeRole = "uav"        // unmanned
	RoleOther      TypeRole = "other"
)

// AircraftType is one entry of the embedded ICAO type designator table
// (ICAO Doc 8643).
type AircraftType struct {
	Designator   string `json:"designator"`   // e.g. "A20N"
	Manufacturer string `json:"manufacturer"` // e.g. "Airbus"; "" for generic designators
	Model        string `json:"model"`        // e.g. "A320neo"
	Name         string `json:"name"`         // manufacturer + model, e.g. "Airbus A320neo"
	// Description is the ICAO description code: airframe class letter, engine
	// count, engine type (e.g. "L2J"); "" when the designator has none.
	Description string `json:"description,omitempty"`
	// WTC is the ICAO wake turbulence category: "L", "M", "H", "J" (super) or
	// "L/M" (depends on the variant); "" when not applicable.
	WTC  string   `json:"wtc,omitempty"`
	Role TypeRole `json:"role"`
}

// Engines returns the number of engines, 0 when unknown.
func (t AircraftType) Engines() int { return TypeEngines(t.Description) }

// EngineType returns "jet", "turboprop", "turboshaft", "piston", "electric",
// "rocket" or "" (unknown / none).
func (t AircraftType) EngineType() string { return TypeEngineType(t.Description) }

// AirframeClass returns "landplane", "seaplane", "amphibian", "helicopter",
// "gyrocopter", "tiltrotor" or "" (unknown).
func (t AircraftType) AirframeClass() string { return TypeAirframeClass(t.Description) }

// ClassText returns the description code in words, e.g. "Landplane, 2 jet
// engines"; "" when the type has no description code.
func (t AircraftType) ClassText() string { return TypeClassText(t.Description) }

// WTCText returns the wake turbulence category in words.
func (t AircraftType) WTCText() string { return WTCText(t.WTC) }

// types holds the parsed typeTable, typeIndex maps the designator to its
// index. Both are written once during package initialisation and are
// read-only afterwards.
var (
	types     []AircraftType
	typeIndex map[string]int32
)

func init() {
	types = make([]AircraftType, 0, 448)
	typeIndex = make(map[string]int32, 448)
	rest := typeTable
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
		f := strings.Split(line, "|")
		if len(f) != 6 || f[0] == "" {
			continue // malformed line: ignored (the tests assert there are none)
		}
		if _, dup := typeIndex[f[0]]; dup {
			continue
		}
		t := AircraftType{Designator: f[0], Manufacturer: f[1], Model: f[2], Description: f[3], WTC: f[4]}
		if strings.HasPrefix(t.Manufacturer, "(") { // "(generic)", "(unknown)"
			t.Manufacturer = ""
			t.Name = t.Model
		} else {
			t.Name = t.Manufacturer + " " + t.Model
		}
		if t.Description == "-" {
			t.Description = ""
		}
		if t.WTC == "-" {
			t.WTC = ""
		}
		t.Role = roleFromLetter(f[5])
		typeIndex[t.Designator] = int32(len(types))
		types = append(types, t)
	}
}

func roleFromLetter(s string) TypeRole {
	switch s {
	case "A":
		return RoleAirliner
	case "R":
		return RoleRegional
	case "F":
		return RoleFreighter
	case "B":
		return RoleBusiness
	case "T":
		return RoleUtility
	case "G":
		return RoleGA
	case "L":
		return RoleLight
	case "H":
		return RoleHelicopter
	case "M":
		return RoleMilitary
	case "U":
		return RoleUAV
	}
	return RoleOther
}

// LookupType returns the embedded entry for an ICAO type designator (upper
// case, 2-4 characters, e.g. "B738"). It does not allocate.
//
// The embedded table covers the few hundred most common types only; ok is
// false for everything else. TypeClassText and WTCText still work on the
// description / WTC strings that the imported aircraft database may supply.
func LookupType(designator string) (t AircraftType, ok bool) {
	i, ok := typeIndex[designator]
	if !ok {
		return AircraftType{}, false
	}
	return types[i], true
}

// TypeName returns "Manufacturer Model" for a type designator, or "" when the
// designator is not in the embedded table. It does not allocate.
func TypeName(designator string) string {
	if i, ok := typeIndex[designator]; ok {
		return types[i].Name
	}
	return ""
}

// TypeCount returns the number of entries in the embedded type table.
func TypeCount() int { return len(types) }

// TypeEngines returns the engine count of an ICAO description code such as
// "L2J" (0 when unknown; "C" = two coupled engines driving one propeller).
func TypeEngines(desc string) int {
	if len(desc) < 2 {
		return 0
	}
	switch c := desc[1]; {
	case c >= '1' && c <= '8':
		return int(c - '0')
	case c == 'C':
		return 2
	}
	return 0
}

// TypeEngineType returns the engine type of an ICAO description code:
// "jet", "turboprop" ("turboshaft" for rotorcraft), "piston", "electric",
// "rocket" or "".
func TypeEngineType(desc string) string {
	if len(desc) < 3 {
		return ""
	}
	switch desc[2] {
	case 'J':
		return "jet"
	case 'T':
		if desc[0] == 'H' || desc[0] == 'G' {
			return "turboshaft"
		}
		return "turboprop"
	case 'P':
		return "piston"
	case 'E':
		return "electric"
	case 'R':
		return "rocket"
	}
	return ""
}

// TypeAirframeClass returns the airframe class of an ICAO description code:
// "landplane", "seaplane", "amphibian", "helicopter", "gyrocopter",
// "tiltrotor" or "".
func TypeAirframeClass(desc string) string {
	if len(desc) < 1 {
		return ""
	}
	switch desc[0] {
	case 'L':
		return "landplane"
	case 'S':
		return "seaplane"
	case 'A':
		return "amphibian"
	case 'H':
		return "helicopter"
	case 'G':
		return "gyrocopter"
	case 'T', 'R': // ICAO Doc 8643 writes T; the Mictronics type list writes R
		return "tiltrotor"
	}
	return ""
}

// TypeClassText spells out an ICAO description code, e.g. "L2J" ->
// "Landplane, 2 jet engines", "H1T" -> "Helicopter, 1 turboshaft engine".
// It returns "" for an empty or unintelligible code.
func TypeClassText(desc string) string {
	class := TypeAirframeClass(desc)
	if class == "" {
		return ""
	}
	n, et := TypeEngines(desc), TypeEngineType(desc)
	var b strings.Builder
	b.Grow(40)
	b.WriteByte(class[0] - 'a' + 'A')
	b.WriteString(class[1:])
	if n == 0 || et == "" {
		return b.String()
	}
	b.WriteString(", ")
	b.WriteByte(byte('0' + n))
	b.WriteByte(' ')
	b.WriteString(et)
	if n == 1 {
		b.WriteString(" engine")
	} else {
		b.WriteString(" engines")
	}
	return b.String()
}

// WTCText returns the ICAO wake turbulence category in words ("" for an
// unknown code). Mass limits are maximum certificated take-off mass.
func WTCText(wtc string) string {
	switch wtc {
	case "L":
		return "Light (7,000 kg or less)"
	case "M":
		return "Medium (7,000 to 136,000 kg)"
	case "H":
		return "Heavy (136,000 kg or more)"
	case "J":
		return "Super (Airbus A380)"
	case "L/M":
		return "Light or medium, depending on variant"
	}
	return ""
}
