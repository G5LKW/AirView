package refdata

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every line of the embedded table must have been accepted by the parser.
func TestAirlineTableWellFormed(t *testing.T) {
	lines := 0
	seen := map[string]bool{}
	for _, line := range strings.Split(airlineTable, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines++
		f := strings.Split(line, "|")
		if len(f) != 5 {
			t.Errorf("line %q: %d fields, want 5", line, len(f))
			continue
		}
		d := f[0]
		if len(d) != 3 || !isUpper(d[0]) || !isUpper(d[1]) || !isUpper(d[2]) {
			t.Errorf("line %q: bad designator", line)
		}
		if seen[d] {
			t.Errorf("designator %s listed twice", d)
		}
		seen[d] = true
		if f[1] == "" {
			t.Errorf("%s: empty name", d)
		}
		if f[2] != strings.ToUpper(f[2]) || f[2] != strings.TrimSpace(f[2]) {
			t.Errorf("%s: telephony %q is not upper case / trimmed", d, f[2])
		}
		if CountryName(f[3]) == "" {
			t.Errorf("%s: unknown country code %q", d, f[3])
		}
		if !strings.Contains("PCBMGX", f[4]) || len(f[4]) != 1 {
			t.Errorf("%s: bad kind %q", d, f[4])
		}
	}
	if lines != AirlineCount() {
		t.Errorf("table has %d lines but %d operators were loaded", lines, AirlineCount())
	}
	if AirlineCount() < 500 {
		t.Errorf("only %d operators embedded, want at least 500", AirlineCount())
	}
}

func TestLookupAirline(t *testing.T) {
	tests := []struct {
		designator, name, telephony, cc, country, typ string
	}{
		{"BAW", "British Airways", "SPEEDBIRD", "GB", "United Kingdom", OperatorAirline},
		{"DLH", "Lufthansa", "LUFTHANSA", "DE", "Germany", OperatorAirline},
		{"RYR", "Ryanair", "RYANAIR", "IE", "Ireland", OperatorAirline},
		{"EZY", "easyJet", "EASY", "GB", "United Kingdom", OperatorAirline},
		{"AFR", "Air France", "AIRFRANS", "FR", "France", OperatorAirline},
		{"KLM", "KLM Royal Dutch Airlines", "KLM", "NL", "Netherlands", OperatorAirline},
		{"UAE", "Emirates", "EMIRATES", "AE", "United Arab Emirates", OperatorAirline},
		{"QTR", "Qatar Airways", "QATARI", "QA", "Qatar", OperatorAirline},
		{"AAL", "American Airlines", "AMERICAN", "US", "United States", OperatorAirline},
		{"SWA", "Southwest Airlines", "SOUTHWEST", "US", "United States", OperatorAirline},
		{"QFA", "Qantas", "QANTAS", "AU", "Australia", OperatorAirline},
		{"CPA", "Cathay Pacific", "CATHAY", "HK", "Hong Kong", OperatorAirline},
		{"CAL", "China Airlines", "DYNASTY", "TW", "Taiwan", OperatorAirline},
		{"AAR", "Asiana Airlines", "ASIANA", "KR", "South Korea", OperatorAirline},
		{"EIN", "Aer Lingus", "SHAMROCK", "IE", "Ireland", OperatorAirline},
		{"FDX", "FedEx Express", "FEDEX", "US", "United States", OperatorCargo},
		{"UPS", "UPS Airlines", "UPS", "US", "United States", OperatorCargo},
		{"CLX", "Cargolux", "CARGOLUX", "LU", "Luxembourg", OperatorCargo},
		{"GTI", "Atlas Air", "GIANT", "US", "United States", OperatorCargo},
		{"EJA", "NetJets Aviation", "EXECJET", "US", "United States", OperatorBusiness},
		{"NJE", "NetJets Europe", "FRACTION", "PT", "Portugal", OperatorBusiness},
		{"RCH", "United States Air Force Air Mobility Command", "REACH", "US", "United States", OperatorMilitary},
		{"RRR", "Royal Air Force", "ASCOT", "GB", "United Kingdom", OperatorMilitary},
		{"GAF", "German Air Force", "GERMAN AIR FORCE", "DE", "Germany", OperatorMilitary},
		{"CHX", "ADAC Luftrettung", "CHRISTOPH", "DE", "Germany", OperatorGovernment},
		{"BOE", "Boeing", "BOEING", "US", "United States", OperatorOther},
	}
	for _, tc := range tests {
		a, ok := LookupAirline(tc.designator)
		if !ok {
			t.Errorf("LookupAirline(%s) not found", tc.designator)
			continue
		}
		if a.Designator != tc.designator || a.Name != tc.name || a.Telephony != tc.telephony ||
			a.CountryCode != tc.cc || a.Country() != tc.country || a.Type != tc.typ {
			t.Errorf("LookupAirline(%s) = %+v (country %q)", tc.designator, a, a.Country())
		}
		if a.Military() != (tc.typ == OperatorMilitary) {
			t.Errorf("%s: Military() = %v", tc.designator, a.Military())
		}
	}
	for _, d := range []string{"", "BA", "BAWW", "baw", "B4W", "ZZZ", "123", "BA ", "Ä12"} {
		if a, ok := LookupAirline(d); ok {
			t.Errorf("LookupAirline(%q) = %+v, want not found", d, a)
		}
	}
}

func TestLookupCallsign(t *testing.T) {
	tests := []struct {
		in   string
		want CallsignInfo
	}{
		{"BAW123A ", CallsignInfo{Callsign: "BAW123A", Kind: CallsignFlight, Designator: "BAW", FlightNumber: "123A",
			Operator: "British Airways", OperatorType: OperatorAirline, Telephony: "SPEEDBIRD",
			Country: "United Kingdom", CountryCode: "GB"}},
		{"dlh4ab", CallsignInfo{Callsign: "DLH4AB", Kind: CallsignFlight, Designator: "DLH", FlightNumber: "4AB",
			Operator: "Lufthansa", OperatorType: OperatorAirline, Telephony: "LUFTHANSA",
			Country: "Germany", CountryCode: "DE"}},
		{"EZY45KP_", CallsignInfo{Callsign: "EZY45KP", Kind: CallsignFlight, Designator: "EZY", FlightNumber: "45KP",
			Operator: "easyJet", OperatorType: OperatorAirline, Telephony: "EASY",
			Country: "United Kingdom", CountryCode: "GB"}},
		{"UAE2", CallsignInfo{Callsign: "UAE2", Kind: CallsignFlight, Designator: "UAE", FlightNumber: "2",
			Operator: "Emirates", OperatorType: OperatorAirline, Telephony: "EMIRATES",
			Country: "United Arab Emirates", CountryCode: "AE"}},
		{"FDX1234", CallsignInfo{Callsign: "FDX1234", Kind: CallsignFlight, Designator: "FDX", FlightNumber: "1234",
			Operator: "FedEx Express", OperatorType: OperatorCargo, Telephony: "FEDEX",
			Country: "United States", CountryCode: "US"}},
		{"RCH871", CallsignInfo{Callsign: "RCH871", Kind: CallsignFlight, Designator: "RCH", FlightNumber: "871",
			Operator: "United States Air Force Air Mobility Command", OperatorType: OperatorMilitary,
			Telephony: "REACH", Country: "United States", CountryCode: "US", Military: true}},
		{"EJA501", CallsignInfo{Callsign: "EJA501", Kind: CallsignFlight, Designator: "EJA", FlightNumber: "501",
			Operator: "NetJets Aviation", OperatorType: OperatorBusiness, Telephony: "EXECJET",
			Country: "United States", CountryCode: "US"}},
		// Unknown operator: still recognised as a flight.
		{"XYZ12", CallsignInfo{Callsign: "XYZ12", Kind: CallsignFlight, Designator: "XYZ", FlightNumber: "12"}},

		// Registration used as callsign (general aviation).
		{"GBYTE", CallsignInfo{Callsign: "GBYTE", Kind: CallsignRegistration, Registration: "G-BYTE",
			Country: "United Kingdom", CountryCode: "GB"}},
		{"DEOTO   ", CallsignInfo{Callsign: "DEOTO", Kind: CallsignRegistration, Registration: "D-EOTO",
			Country: "Germany", CountryCode: "DE"}},
		{"N172SP", CallsignInfo{Callsign: "N172SP", Kind: CallsignRegistration, Registration: "N172SP",
			Country: "United States", CountryCode: "US"}},
		{"N1", CallsignInfo{Callsign: "N1", Kind: CallsignRegistration, Registration: "N1",
			Country: "United States", CountryCode: "US"}},
		{"HBJBH", CallsignInfo{Callsign: "HBJBH", Kind: CallsignRegistration, Registration: "HB-JBH",
			Country: "Switzerland", CountryCode: "CH"}},
		{"OOSNA", CallsignInfo{Callsign: "OOSNA", Kind: CallsignRegistration, Registration: "OO-SNA",
			Country: "Belgium", CountryCode: "BE"}},
		{"VHOQA", CallsignInfo{Callsign: "VHOQA", Kind: CallsignRegistration, Registration: "VH-OQA",
			Country: "Australia", CountryCode: "AU"}},
		{"CFIUF", CallsignInfo{Callsign: "CFIUF", Kind: CallsignRegistration, Registration: "C-FIUF",
			Country: "Canada", CountryCode: "CA"}},
		{"JA743A", CallsignInfo{Callsign: "JA743A", Kind: CallsignRegistration, Registration: "JA743A",
			Country: "Japan", CountryCode: "JP"}},
		{"HL7631", CallsignInfo{Callsign: "HL7631", Kind: CallsignRegistration, Registration: "HL7631",
			Country: "South Korea", CountryCode: "KR"}},
		{"RA73126", CallsignInfo{Callsign: "RA73126", Kind: CallsignRegistration, Registration: "RA-73126",
			Country: "Russia", CountryCode: "RU"}},
		{"BLJJ", CallsignInfo{Callsign: "BLJJ", Kind: CallsignRegistration, Registration: "B-LJJ",
			Country: "Hong Kong", CountryCode: "HK"}},
		{"B6140", CallsignInfo{Callsign: "B6140", Kind: CallsignRegistration, Registration: "B-6140",
			Country: "China", CountryCode: "CN"}},
		{"VPBLU", CallsignInfo{Callsign: "VPBLU", Kind: CallsignRegistration, Registration: "VP-BLU",
			Country: "Bermuda", CountryCode: "BM"}},
		{"A4OBA", CallsignInfo{Callsign: "A4OBA", Kind: CallsignRegistration, Registration: "A4O-BA",
			Country: "Oman", CountryCode: "OM"}},
		{"RPC3345", CallsignInfo{Callsign: "RPC3345", Kind: CallsignRegistration, Registration: "RP-C3345",
			Country: "Philippines", CountryCode: "PH"}},

		// Military word callsigns.
		{"NATO01", CallsignInfo{Callsign: "NATO01", Kind: CallsignMilitary, FlightNumber: "01",
			Operator: "NATO Airborne Early Warning & Control Force", OperatorType: OperatorMilitary,
			Telephony: "NATO", Military: true}},
		{"FORTE11", CallsignInfo{Callsign: "FORTE11", Kind: CallsignMilitary, FlightNumber: "11",
			Operator: "United States Air Force (RQ-4 Global Hawk)", OperatorType: OperatorMilitary,
			Telephony: "FORTE", Country: "United States", CountryCode: "US", Military: true}},

		// Nothing recognisable.
		{"", CallsignInfo{}},
		{"        ", CallsignInfo{}},
		{"@@@@@@@@", CallsignInfo{}},
		{"\x00\x00\x00\x00", CallsignInfo{}},
		{"TEST1234", CallsignInfo{Callsign: "TEST1234", Kind: CallsignOther}},
		{"12345", CallsignInfo{Callsign: "12345", Kind: CallsignOther}},
		{"AB*12", CallsignInfo{Callsign: "AB*12", Kind: CallsignOther}},
		{"BA 123", CallsignInfo{Callsign: "BA 123", Kind: CallsignOther}},
		{"BAW", CallsignInfo{Callsign: "BAW", Kind: CallsignOther}},
		{"NATO", CallsignInfo{Callsign: "NATO", Kind: CallsignOther}},
		{"BAW123456", CallsignInfo{Callsign: "BAW123456", Kind: CallsignOther}}, // 9 characters: not a flight ID
	}
	for _, tc := range tests {
		if got := LookupCallsign(tc.in); got != tc.want {
			t.Errorf("LookupCallsign(%q)\n got %+v\nwant %+v", tc.in, got, tc.want)
		}
	}
}

func TestCallsignSpoken(t *testing.T) {
	for in, want := range map[string]string{
		"BAW123A": "SPEEDBIRD 123A",
		"RCH871":  "REACH 871",
		"EIN1":    "SHAMROCK 1",
		"XYZ12":   "", // unknown operator
		"GBYTE":   "", // registration
		"NATO01":  "", // word callsign: already spoken as written
		"":        "",
	} {
		if got := LookupCallsign(in).Spoken(); got != want {
			t.Errorf("LookupCallsign(%q).Spoken() = %q, want %q", in, got, want)
		}
	}
}

// With the address, a registration callsign must fit the state of the address.
func TestLookupCallsignFor(t *testing.T) {
	tests := []struct {
		cs   string
		icao uint32
		kind CallsignKind
		reg  string
		iso  string
	}{
		{"GBYTE", 0x406A9F, CallsignRegistration, "G-BYTE", "GB"},  // UK address
		{"GBYTE", 0xA00001, CallsignOther, "", ""},                 // US address: not a G- registration
		{"GBYTE", 0, CallsignRegistration, "G-BYTE", "GB"},         // no address: no check
		{"MAGIC", 0xAE01CE, CallsignOther, "", ""},                 // USAF aircraft using the word MAGIC
		{"MAGIC", 0x43E8DE, CallsignRegistration, "M-AGIC", "IM"},  // the real M-AGIC (Isle of Man block)
		{"MAGIC", 0x406A9F, CallsignRegistration, "M-AGIC", "IM"},  // M- marks also sit in the general UK block
		{"DEOTO", 0x3D2B9C, CallsignRegistration, "D-EOTO", "DE"},  // the real D-EOTO
		{"DELTA", 0xA00001, CallsignOther, "", ""},                 // not D-ELTA
		{"BLJJ", 0x780A3F, CallsignRegistration, "B-LJJ", "HK"},    // Hong Kong mark, China block
		{"BLJJ", 0x789123, CallsignRegistration, "B-LJJ", "HK"},    // Hong Kong sub-block
		{"N172SP", 0xA12239, CallsignRegistration, "N172SP", "US"}, // the real N172SP
		{"N172SP", 0x3C6444, CallsignOther, "", ""},                //
		{"N172SP", 0xF00001, CallsignRegistration, "N172SP", "US"}, // unallocated address: nothing to contradict
		{"VPBLU", 0x400100, CallsignRegistration, "VP-BLU", "BM"},  // Bermuda sub-block
		{"VPBLU", 0x424B5A, CallsignRegistration, "VP-BLU", "BM"},  // other UK territory sub-block: parent state accepted
		{"BAW123", 0xA00001, CallsignFlight, "", "GB"},             // flights are never checked against the address
	}
	for _, tc := range tests {
		got := LookupCallsignFor(tc.cs, tc.icao)
		if got.Kind != tc.kind || got.Registration != tc.reg || got.CountryCode != tc.iso {
			t.Errorf("LookupCallsignFor(%q, %06X) = %+v; want kind %v reg %q country %q",
				tc.cs, tc.icao, got, tc.kind, tc.reg, tc.iso)
		}
	}
}

func TestNormalizeCallsign(t *testing.T) {
	for in, want := range map[string]string{
		"BAW123  ":       "BAW123",
		"  BAW123":       "BAW123",
		"baw123":         "BAW123",
		"BAW123@@":       "BAW123",
		"BAW123\x00\x00": "BAW123",
		"__#":            "",
		"":               "",
		"A B":            "A B",
	} {
		if got := NormalizeCallsign(in); got != want {
			t.Errorf("NormalizeCallsign(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsNNumber(t *testing.T) {
	for _, s := range []string{"N1", "N9", "N10", "N1A", "N1AB", "N12345", "N1234A", "N123AB", "N99999", "N100", "N1KE"} {
		if !isNNumber(s) {
			t.Errorf("isNNumber(%q) = false", s)
		}
	}
	for _, s := range []string{"", "N", "N0", "N01", "NA", "N1ABC", "N123456", "N12I", "N12O", "N1A2", "N12AB3", "M123", "N1234AB", "N1-2"} {
		if isNNumber(s) {
			t.Errorf("isNNumber(%q) = true", s)
		}
	}
}

func TestRegPatternTable(t *testing.T) {
	for i, p := range regPatterns {
		if p.prefix == "" || p.suffix == "" {
			t.Errorf("regPatterns[%d]: empty prefix or suffix", i)
		}
		if p.hyphen < 0 || p.hyphen > len(p.prefix) {
			t.Errorf("regPatterns[%d] %s: hyphen position %d", i, p.prefix, p.hyphen)
		}
		if CountryName(p.iso) == "" {
			t.Errorf("regPatterns[%d] %s: unknown country %q", i, p.prefix, p.iso)
		}
		if p.iso2 != "" && CountryName(p.iso2) == "" {
			t.Errorf("regPatterns[%d] %s: unknown alternative country %q", i, p.prefix, p.iso2)
		}
		if n := len(p.prefix) + len(p.suffix); n > 8 {
			t.Errorf("regPatterns[%d] %s: %d characters do not fit a flight ID", i, p.prefix, n)
		}
	}
	for i, w := range militaryWords {
		if w.word == "" || w.word != strings.ToUpper(w.word) || w.operator == "" {
			t.Errorf("militaryWords[%d]: bad entry %+v", i, w)
		}
		if w.iso != "" && CountryName(w.iso) == "" {
			t.Errorf("militaryWords[%d] %s: unknown country %q", i, w.word, w.iso)
		}
		// A word that is also an airline designator + digits would be shadowed.
		if len(w.word) == 3 {
			if _, ok := LookupAirline(w.word); ok {
				t.Errorf("militaryWords[%d] %s collides with an airline designator", i, w.word)
			}
		}
	}
}

func TestCallsignKindText(t *testing.T) {
	for k, want := range map[CallsignKind]string{
		CallsignUnknown: "unknown", CallsignFlight: "flight", CallsignRegistration: "registration",
		CallsignMilitary: "military", CallsignOther: "other", CallsignKind(77): "unknown",
	} {
		if k.String() != want {
			t.Errorf("CallsignKind(%d).String() = %q, want %q", k, k.String(), want)
		}
	}
	b, err := json.Marshal(LookupCallsign("BAW123A"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"callsign":"BAW123A","kind":"flight","designator":"BAW","flightNumber":"123A","operator":"British Airways",` +
		`"operatorType":"airline","telephony":"SPEEDBIRD","country":"United Kingdom","countryCode":"GB"}`
	if string(b) != want {
		t.Errorf("JSON = %s\nwant   %s", b, want)
	}
	b, _ = json.Marshal(LookupCallsign("GBYTE"))
	want = `{"callsign":"GBYTE","kind":"registration","country":"United Kingdom","countryCode":"GB","reg":"G-BYTE"}`
	if string(b) != want {
		t.Errorf("JSON = %s\nwant   %s", b, want)
	}
}

func TestCallsignLookupAllocations(t *testing.T) {
	var sink int
	n := testing.AllocsPerRun(200, func() {
		sink += len(LookupCallsign("BAW123A ").Operator)   // known airline, padded
		sink += len(LookupCallsign("XYZ12").Designator)    // unknown airline
		sink += len(LookupCallsign("NATO01").Operator)     // military word
		sink += len(LookupCallsign("TEST1234").Callsign)   // nothing
		sink += len(LookupCallsign("N172SP").Registration) // registration without hyphen
		sink += len(LookupCallsign("").Callsign)
		if a, ok := LookupAirline("DLH"); ok {
			sink += len(a.Name)
		}
	})
	if n != 0 {
		t.Errorf("callsign lookups allocate %.1f times per run, want 0", n)
	}
	// A hyphenated registration costs the one formatted string.
	n = testing.AllocsPerRun(200, func() { sink += len(LookupCallsign("GBYTE").Registration) })
	if n > 1 {
		t.Errorf("LookupCallsign(GBYTE) allocates %.1f times, want at most 1", n)
	}
	_ = sink
}

func BenchmarkLookupCallsign(b *testing.B) {
	b.ReportAllocs()
	in := []string{"BAW123A ", "DLH4AB  ", "RYR1TX  ", "GBYTE   ", "XYZ12   ", "N172SP  "}
	var n int
	for i := 0; i < b.N; i++ {
		n += len(LookupCallsign(in[i%len(in)]).Operator)
	}
	_ = n
}
