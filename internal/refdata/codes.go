package refdata

// Code tables: ADS-B emitter category, Mode A (squawk) code meanings,
// emergency states, ADS-B versions and the integrity / accuracy indicators
// (NIC, NACp, NACv, SIL, SDA, GVA). The values follow RTCA DO-260B / EUROCAE
// ED-102A and ICAO Doc 9871. All functions are allocation free and safe for
// concurrent use.

// emitterCategories is indexed [set][category]: set 0 = A (type code 4),
// 1 = B (type code 3), 2 = C (type code 2), 3 = D (type code 1).
var emitterCategories = [4][8]string{
	{
		"No category information",
		"Light (below 15,500 lb / 7 t)",
		"Small (15,500 to 75,000 lb / 7 to 34 t)",
		"Large (75,000 to 300,000 lb / 34 to 136 t)",
		"High vortex large (e.g. Boeing 757)",
		"Heavy (above 300,000 lb / 136 t)",
		"High performance (above 5 g and 400 kt)",
		"Rotorcraft",
	},
	{
		"No category information",
		"Glider / sailplane",
		"Lighter than air",
		"Parachutist / skydiver",
		"Ultralight / hang-glider / paraglider",
		"Reserved",
		"Unmanned aerial vehicle",
		"Space / trans-atmospheric vehicle",
	},
	{
		"No category information",
		"Surface vehicle - emergency",
		"Surface vehicle - service",
		"Point obstacle (includes tethered balloons)",
		"Cluster obstacle",
		"Line obstacle",
		"Reserved",
		"Reserved",
	},
	{
		"No category information",
		"Reserved", "Reserved", "Reserved", "Reserved", "Reserved", "Reserved", "Reserved",
	},
}

// emitterCodes holds the two-character codes so EmitterCategoryCode does not
// have to build strings.
var emitterCodes = [4][8]string{
	{"A0", "A1", "A2", "A3", "A4", "A5", "A6", "A7"},
	{"B0", "B1", "B2", "B3", "B4", "B5", "B6", "B7"},
	{"C0", "C1", "C2", "C3", "C4", "C5", "C6", "C7"},
	{"D0", "D1", "D2", "D3", "D4", "D5", "D6", "D7"},
}

// EmitterCategory returns the description of an ADS-B emitter category code
// "A0".."D7" (as used in model.Aircraft.Category), or "" for anything else.
func EmitterCategory(code string) string {
	if len(code) != 2 {
		return ""
	}
	set, cat := code[0]-'A', code[1]-'0'
	if set > 3 || cat > 7 {
		return ""
	}
	return emitterCategories[set][cat]
}

// EmitterCategoryCode converts the identification message's type code (1..4)
// and 3-bit category field into the "A0".."D7" code: type code 4 is set A,
// 3 is B, 2 is C and 1 is D. It returns "" for values out of range.
func EmitterCategoryCode(typeCode, category int) string {
	if typeCode < 1 || typeCode > 4 || category < 0 || category > 7 {
		return ""
	}
	return emitterCodes[4-typeCode][category]
}

// EmitterCategoryKnown reports whether the code carries information, i.e. is
// a valid code that is neither "no category information" nor reserved.
func EmitterCategoryKnown(code string) bool {
	d := EmitterCategory(code)
	return d != "" && d != "Reserved" && code[1] != '0'
}

// SquawkInfo describes a Mode A code with a conventional meaning.
type SquawkInfo struct {
	Code    string `json:"code"`    // four octal digits
	Meaning string `json:"meaning"` // short label
	// Region says where the meaning applies: "Worldwide" (ICAO) or a list of
	// states / regions. The same code can mean something else elsewhere.
	Region string `json:"region"`
	// Emergency is the model.Aircraft.Emergency state the code implies
	// ("general", "nordo", "unlawful"), "" for non-emergency codes.
	Emergency string `json:"emergency,omitempty"`
	// Kind is "emergency", "conspicuity" (general VFR / IFR codes that many
	// aircraft set at the same time) or "special" (a particular activity).
	Kind string `json:"kind"`
}

// Squawk kinds (SquawkInfo.Kind).
const (
	SquawkKindEmergency   = "emergency"
	SquawkKindConspicuity = "conspicuity"
	SquawkKindSpecial     = "special"
)

// squawks is sorted by code (checked by the tests). Meanings are regional
// conventions except for the ICAO-wide codes; where states disagree the most
// widely used meaning comes first and the others are named in the label.
var squawks = []SquawkInfo{
	{"0000", "Non-discrete code that should not be assigned (UK: transponder / Mode C failure)", "Europe, United States", "", SquawkKindSpecial},
	{"0021", "Fixed-wing aircraft receiving service from a ship (Germany until 2007: VFR at or below 5,000 ft)", "United Kingdom", "", SquawkKindSpecial},
	{"0022", "Helicopter receiving service from a ship (Germany until 2007: VFR above 5,000 ft)", "United Kingdom", "", SquawkKindSpecial},
	{"0033", "Parachute dropping in progress", "United Kingdom", "", SquawkKindSpecial},
	{"0100", "Operating at an aerodrome, when assigned by ATC", "Australia", "", SquawkKindSpecial},
	{"1000", "IFR conspicuity: Mode S / ADS-B flight identified by its flight ID instead of a discrete code", "Worldwide (ICAO); Canada: IFR below 18,000 ft without assigned code", "", SquawkKindConspicuity},
	{"1200", "VFR conspicuity", "United States, Canada; Australia (class E and G airspace)", "", SquawkKindConspicuity},
	{"1202", "VFR glider not in contact with ATC", "United States, Canada", "", SquawkKindSpecial},
	{"1255", "Firefighting aircraft not in contact with ATC", "United States", "", SquawkKindSpecial},
	{"1276", "Air defence identification zone penetration without ATC contact", "United States", "", SquawkKindSpecial},
	{"1277", "VFR search and rescue mission (USAF / USCG)", "United States", "", SquawkKindSpecial},
	{"1400", "VFR conspicuity at high level (Canada above 12,500 ft, Japan above 10,000 ft)", "Canada, Japan", "", SquawkKindConspicuity},
	{"2000", "No code assigned: entering SSR coverage / no ATS instruction (Australia: IFR in class G; Canada: uncontrolled IFR at or above 18,000 ft)", "Worldwide (ICAO), Europe", "", SquawkKindConspicuity},
	{"2100", "Ground testing by maintenance staff", "Australia", "", SquawkKindSpecial},
	{"3000", "Civil flight in class A, C or D airspace, or IFR in class E", "Australia", "", SquawkKindConspicuity},
	{"4000", "Military VFR training route / frequent or rapid altitude changes (Australia: civil flight in class G more than 15 NM offshore)", "United States", "", SquawkKindSpecial},
	{"5000", "Reserved for NORAD (Australia: military operations)", "United States, Canada", "", SquawkKindSpecial},
	{"7000", "VFR conspicuity (Australia: unmanned aircraft)", "Worldwide (ICAO), Europe", "", SquawkKindConspicuity},
	{"7001", "Military low-level climb-out (France: VFR conspicuity)", "United Kingdom", "", SquawkKindSpecial},
	{"7004", "Aerobatics and display flying", "United Kingdom and some other European states", "", SquawkKindSpecial},
	{"7100", "Search and rescue (Rega)", "Switzerland", "", SquawkKindSpecial},
	{"7400", "Unmanned aircraft lost command link", "United States, United Kingdom, Australia", "", SquawkKindSpecial},
	{"7500", "Unlawful interference (hijack)", "Worldwide", EmergencyUnlawful, SquawkKindEmergency},
	{"7600", "Radio communication failure", "Worldwide", EmergencyNoRadio, SquawkKindEmergency},
	{"7615", "Civil littoral surveillance flight", "Australia", "", SquawkKindSpecial},
	{"7700", "General emergency", "Worldwide", EmergencyGeneral, SquawkKindEmergency},
	{"7777", "Fixed radar test transponder (US also: military interceptor on active air-defence mission)", "United States, Germany, United Kingdom, Belgium, Netherlands", "", SquawkKindSpecial},
}

// Squawk returns the conventional meaning of a Mode A code given as four
// octal digits. ok is false for discrete codes without a general meaning and
// for malformed input. It does not allocate.
func Squawk(code string) (info SquawkInfo, ok bool) {
	if !ValidSquawk(code) {
		return SquawkInfo{}, false
	}
	lo, hi := 0, len(squawks)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if squawks[mid].Code < code {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(squawks) && squawks[lo].Code == code {
		return squawks[lo], true
	}
	return SquawkInfo{}, false
}

// SquawkMeaning returns Squawk(code).Meaning, or "" when the code has no
// conventional meaning.
func SquawkMeaning(code string) string {
	info, _ := Squawk(code)
	return info.Meaning
}

// ValidSquawk reports whether code is four octal digits.
func ValidSquawk(code string) bool {
	if len(code) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if code[i] < '0' || code[i] > '7' {
			return false
		}
	}
	return true
}

// SquawkEmergency returns the emergency state implied by a Mode A code:
// "unlawful" for 7500, "nordo" for 7600, "general" for 7700, otherwise "".
func SquawkEmergency(code string) string {
	switch code {
	case "7500":
		return EmergencyUnlawful
	case "7600":
		return EmergencyNoRadio
	case "7700":
		return EmergencyGeneral
	}
	return ""
}

// Emergency state identifiers, identical to the values of
// model.Aircraft.Emergency.
const (
	EmergencyNone     = ""
	EmergencyGeneral  = "general"
	EmergencyMedical  = "medical"
	EmergencyMinFuel  = "minfuel"
	EmergencyNoRadio  = "nordo"
	EmergencyUnlawful = "unlawful"
	EmergencyDowned   = "downed"
)

// emergencyStates is indexed by the 3-bit emergency state field of the ADS-B
// aircraft status message (type code 28 subtype 1).
var emergencyStates = [8]struct{ state, label string }{
	{EmergencyNone, "No emergency"},
	{EmergencyGeneral, "General emergency"},
	{EmergencyMedical, "Medical emergency (lifeguard)"},
	{EmergencyMinFuel, "Minimum fuel"},
	{EmergencyNoRadio, "No communications"},
	{EmergencyUnlawful, "Unlawful interference"},
	{EmergencyDowned, "Downed aircraft"},
	{EmergencyNone, "Reserved"},
}

// EmergencyState converts the 3-bit emergency state field of the ADS-B
// aircraft status message (0..7) into the model.Aircraft.Emergency identifier.
// 0, the reserved value 7 and out-of-range values give "".
func EmergencyState(code int) string {
	if code < 0 || code > 7 {
		return ""
	}
	return emergencyStates[code].state
}

// EmergencyLabel returns the human readable label of an emergency state
// identifier ("general" -> "General emergency"). The empty state gives
// "No emergency"; an unknown identifier gives "".
func EmergencyLabel(state string) string {
	for i := 0; i < 7; i++ {
		if emergencyStates[i].state == state {
			return emergencyStates[i].label
		}
	}
	return ""
}

var adsbVersions = [4]string{
	"Version 0 (DO-260 / ED-102)",
	"Version 1 (DO-260A)",
	"Version 2 (DO-260B / ED-102A)",
	"Version 3 (DO-260C / ED-102B)",
}

// ADSBVersionLabel returns the label of an ADS-B MOPS version number (0..3),
// "Reserved" for 4..7 and "" for anything else.
func ADSBVersionLabel(version int) string {
	switch {
	case version >= 0 && version < len(adsbVersions):
		return adsbVersions[version]
	case version >= 4 && version <= 7:
		return "Reserved"
	}
	return ""
}

// accuracy is one row of an integrity / accuracy table.
type accuracy struct {
	text  string
	limit float64 // upper bound in the table's unit; 0 = unknown / unbounded
}

// nicTable: Navigation Integrity Category -> horizontal containment radius Rc
// (metres). NIC 6 covers three sub-cases selected by the NIC supplement bits
// (Rc < 0.6 NM, < 0.5 NM, < 0.3 NM); the widest is given.
var nicTable = [12]accuracy{
	{"Integrity unknown", 0},
	{"Rc < 20 NM (37 km)", 37040},
	{"Rc < 8 NM (14.8 km)", 14816},
	{"Rc < 4 NM (7.4 km)", 7408},
	{"Rc < 2 NM (3.7 km)", 3704},
	{"Rc < 1 NM (1852 m)", 1852},
	{"Rc < 0.6 NM (1111 m)", 1111.2},
	{"Rc < 0.2 NM (370 m)", 370.4},
	{"Rc < 0.1 NM (185 m)", 185.2},
	{"Rc < 75 m", 75},
	{"Rc < 25 m", 25},
	{"Rc < 7.5 m", 7.5},
}

// nacpTable: Navigation Accuracy Category for position -> 95 % horizontal
// position uncertainty EPU (metres).
var nacpTable = [12]accuracy{
	{"Accuracy unknown (EPU >= 10 NM)", 0},
	{"EPU < 10 NM (18.5 km)", 18520},
	{"EPU < 4 NM (7.4 km)", 7408},
	{"EPU < 2 NM (3.7 km)", 3704},
	{"EPU < 1 NM (1852 m)", 1852},
	{"EPU < 0.5 NM (926 m)", 926},
	{"EPU < 0.3 NM (556 m)", 555.6},
	{"EPU < 0.1 NM (185 m)", 185.2},
	{"EPU < 0.05 NM (93 m)", 92.6},
	{"EPU < 30 m", 30},
	{"EPU < 10 m", 10},
	{"EPU < 3 m", 3},
}

// nacvTable: Navigation Accuracy Category for velocity -> 95 % horizontal
// velocity error (m/s).
var nacvTable = [5]accuracy{
	{"Velocity accuracy unknown (>= 10 m/s)", 0},
	{"Velocity error < 10 m/s", 10},
	{"Velocity error < 3 m/s", 3},
	{"Velocity error < 1 m/s", 1},
	{"Velocity error < 0.3 m/s", 0.3},
}

// silTable: Source Integrity Level -> probability of exceeding the NIC
// containment radius without alerting, per flight hour or per sample.
var silTable = [4]accuracy{
	{"Integrity level unknown (> 1e-3)", 0},
	{"<= 1e-3 per hour / sample", 1e-3},
	{"<= 1e-5 per hour / sample", 1e-5},
	{"<= 1e-7 per hour / sample", 1e-7},
}

// sdaTable: System Design Assurance -> supported failure condition.
var sdaTable = [4]string{
	"Unknown / no safety effect (> 1e-3 per hour)",
	"Minor failure condition (<= 1e-3 per hour)",
	"Major failure condition (<= 1e-5 per hour)",
	"Hazardous failure condition (<= 1e-7 per hour)",
}

// gvaTable: Geometric Vertical Accuracy -> 95 % vertical figure of merit.
var gvaTable = [4]string{
	"Vertical accuracy unknown (> 150 m)",
	"Vertical accuracy <= 150 m",
	"Vertical accuracy <= 45 m",
	"Reserved",
}

// NICText describes a Navigation Integrity Category (0..11) as the bound on
// the horizontal containment radius, e.g. NICText(8) = "Rc < 0.1 NM (185 m)".
// It returns "" for values out of range.
func NICText(nic int) string {
	if nic < 0 || nic >= len(nicTable) {
		return ""
	}
	return nicTable[nic].text
}

// NICRadiusM returns the containment radius bound Rc of a Navigation
// Integrity Category in metres; ok is false for NIC 0 (unknown) and values
// out of range.
func NICRadiusM(nic int) (rc float64, ok bool) {
	if nic < 1 || nic >= len(nicTable) {
		return 0, false
	}
	return nicTable[nic].limit, true
}

// NACpText describes a Navigation Accuracy Category for position (0..11) as
// the 95 % bound on the horizontal position error, e.g. NACpText(9) =
// "EPU < 30 m". It returns "" for values out of range.
func NACpText(nacp int) string {
	if nacp < 0 || nacp >= len(nacpTable) {
		return ""
	}
	return nacpTable[nacp].text
}

// NACpAccuracyM returns the 95 % horizontal position accuracy bound (EPU) of
// a NACp value in metres; ok is false for NACp 0 (unknown) and values out of
// range.
func NACpAccuracyM(nacp int) (epu float64, ok bool) {
	if nacp < 1 || nacp >= len(nacpTable) {
		return 0, false
	}
	return nacpTable[nacp].limit, true
}

// NACvText describes a Navigation Accuracy Category for velocity (0..4).
// It returns "" for values out of range.
func NACvText(nacv int) string {
	if nacv < 0 || nacv >= len(nacvTable) {
		return ""
	}
	return nacvTable[nacv].text
}

// SILText describes a Source Integrity Level (0..3): the probability that
// the position error exceeds the NIC containment radius without an alert.
// It returns "" for values out of range.
func SILText(sil int) string {
	if sil < 0 || sil >= len(silTable) {
		return ""
	}
	return silTable[sil].text
}

// SDAText describes a System Design Assurance level (0..3).
// It returns "" for values out of range.
func SDAText(sda int) string {
	if sda < 0 || sda >= len(sdaTable) {
		return ""
	}
	return sdaTable[sda]
}

// GVAText describes a Geometric Vertical Accuracy value (0..3).
// It returns "" for values out of range.
func GVAText(gva int) string {
	if gva < 0 || gva >= len(gvaTable) {
		return ""
	}
	return gvaTable[gva]
}
