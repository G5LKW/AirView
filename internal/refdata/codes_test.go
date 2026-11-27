package refdata

import (
	"strings"
	"testing"
)

func TestEmitterCategory(t *testing.T) {
	want := map[string]string{
		"A0": "No category information",
		"A1": "Light (below 15,500 lb / 7 t)",
		"A2": "Small (15,500 to 75,000 lb / 7 to 34 t)",
		"A3": "Large (75,000 to 300,000 lb / 34 to 136 t)",
		"A4": "High vortex large (e.g. Boeing 757)",
		"A5": "Heavy (above 300,000 lb / 136 t)",
		"A6": "High performance (above 5 g and 400 kt)",
		"A7": "Rotorcraft",
		"B1": "Glider / sailplane",
		"B2": "Lighter than air",
		"B3": "Parachutist / skydiver",
		"B4": "Ultralight / hang-glider / paraglider",
		"B6": "Unmanned aerial vehicle",
		"B7": "Space / trans-atmospheric vehicle",
		"C1": "Surface vehicle - emergency",
		"C2": "Surface vehicle - service",
		"C3": "Point obstacle (includes tethered balloons)",
		"C4": "Cluster obstacle",
		"C5": "Line obstacle",
	}
	for code, text := range want {
		if got := EmitterCategory(code); got != text {
			t.Errorf("EmitterCategory(%s) = %q, want %q", code, got, text)
		}
		if !EmitterCategoryKnown(code) && code != "A0" {
			t.Errorf("EmitterCategoryKnown(%s) = false", code)
		}
	}
	// All 32 codes A0..D7 have a text.
	for _, set := range "ABCD" {
		for cat := '0'; cat <= '7'; cat++ {
			code := string(set) + string(cat)
			if EmitterCategory(code) == "" {
				t.Errorf("EmitterCategory(%s) is empty", code)
			}
		}
	}
	for _, code := range []string{"", "A", "A8", "E0", "a1", "A10", "1A", "@0"} {
		if got := EmitterCategory(code); got != "" {
			t.Errorf("EmitterCategory(%q) = %q, want empty", code, got)
		}
		if EmitterCategoryKnown(code) {
			t.Errorf("EmitterCategoryKnown(%q) = true", code)
		}
	}
	// "No information" and reserved values carry no information.
	for _, code := range []string{"A0", "B0", "C0", "D0", "B5", "C6", "C7", "D1", "D7"} {
		if EmitterCategoryKnown(code) {
			t.Errorf("EmitterCategoryKnown(%s) = true", code)
		}
	}
}

// Identification messages: type code 4 carries set A, 3 = B, 2 = C, 1 = D.
func TestEmitterCategoryCode(t *testing.T) {
	tests := []struct {
		tc, cat int
		want    string
	}{
		{4, 0, "A0"}, {4, 3, "A3"}, {4, 5, "A5"}, {4, 7, "A7"},
		{3, 1, "B1"}, {3, 6, "B6"}, {2, 1, "C1"}, {2, 3, "C3"}, {1, 0, "D0"}, {1, 7, "D7"},
		{0, 1, ""}, {5, 1, ""}, {4, 8, ""}, {4, -1, ""}, {-1, 0, ""},
	}
	for _, tc := range tests {
		if got := EmitterCategoryCode(tc.tc, tc.cat); got != tc.want {
			t.Errorf("EmitterCategoryCode(%d, %d) = %q, want %q", tc.tc, tc.cat, got, tc.want)
		}
	}
}

func TestSquawk(t *testing.T) {
	for i, s := range squawks {
		if !ValidSquawk(s.Code) {
			t.Errorf("squawks[%d]: invalid code %q", i, s.Code)
		}
		if i > 0 && squawks[i-1].Code >= s.Code {
			t.Errorf("squawks[%d] %s is not sorted after %s", i, s.Code, squawks[i-1].Code)
		}
		if s.Meaning == "" || s.Region == "" {
			t.Errorf("squawk %s: empty meaning or region", s.Code)
		}
		switch s.Kind {
		case SquawkKindEmergency:
			if s.Emergency == "" {
				t.Errorf("squawk %s: emergency kind without state", s.Code)
			}
		case SquawkKindConspicuity, SquawkKindSpecial:
			if s.Emergency != "" {
				t.Errorf("squawk %s: non-emergency kind with state %q", s.Code, s.Emergency)
			}
		default:
			t.Errorf("squawk %s: unknown kind %q", s.Code, s.Kind)
		}
		if got := SquawkEmergency(s.Code); got != s.Emergency {
			t.Errorf("SquawkEmergency(%s) = %q, table says %q", s.Code, got, s.Emergency)
		}
	}

	tests := []struct {
		code, emergency, kind string
		meaning, region       string // substrings
	}{
		{"7500", EmergencyUnlawful, SquawkKindEmergency, "Unlawful interference", "Worldwide"},
		{"7600", EmergencyNoRadio, SquawkKindEmergency, "Radio communication failure", "Worldwide"},
		{"7700", EmergencyGeneral, SquawkKindEmergency, "General emergency", "Worldwide"},
		{"7000", "", SquawkKindConspicuity, "VFR conspicuity", "Europe"},
		{"1200", "", SquawkKindConspicuity, "VFR conspicuity", "United States"},
		{"2000", "", SquawkKindConspicuity, "No code assigned", "ICAO"},
		{"1000", "", SquawkKindConspicuity, "IFR conspicuity", "ICAO"},
		{"7400", "", SquawkKindSpecial, "lost command link", "United States"},
		{"7777", "", SquawkKindSpecial, "test transponder", "United States"},
		{"0033", "", SquawkKindSpecial, "Parachute", "United Kingdom"},
	}
	for _, tc := range tests {
		info, ok := Squawk(tc.code)
		if !ok {
			t.Errorf("Squawk(%s) not found", tc.code)
			continue
		}
		if info.Code != tc.code || info.Emergency != tc.emergency || info.Kind != tc.kind ||
			!strings.Contains(info.Meaning, tc.meaning) || !strings.Contains(info.Region, tc.region) {
			t.Errorf("Squawk(%s) = %+v", tc.code, info)
		}
		if SquawkMeaning(tc.code) != info.Meaning {
			t.Errorf("SquawkMeaning(%s) differs from Squawk().Meaning", tc.code)
		}
	}
	// Discrete codes and malformed input.
	for _, code := range []string{"1234", "4521", "7501", "", "77", "77000", "7800", "7 00", "abcd", "７７００"} {
		if info, ok := Squawk(code); ok {
			t.Errorf("Squawk(%q) = %+v, want not found", code, info)
		}
		if SquawkMeaning(code) != "" {
			t.Errorf("SquawkMeaning(%q) is not empty", code)
		}
	}
	if !ValidSquawk("0000") || !ValidSquawk("7777") || ValidSquawk("8000") || ValidSquawk("123") || ValidSquawk("12345") {
		t.Error("ValidSquawk gives wrong answers")
	}
	if SquawkEmergency("7000") != "" || SquawkEmergency("") != "" {
		t.Error("SquawkEmergency must be empty for non-emergency codes")
	}
}

// The emergency identifiers are the ones model.Aircraft.Emergency documents.
func TestEmergencyState(t *testing.T) {
	want := []struct{ state, label string }{
		{"", "No emergency"},
		{"general", "General emergency"},
		{"medical", "Medical emergency (lifeguard)"},
		{"minfuel", "Minimum fuel"},
		{"nordo", "No communications"},
		{"unlawful", "Unlawful interference"},
		{"downed", "Downed aircraft"},
		{"", ""}, // 7 is reserved
	}
	for code, w := range want {
		if got := EmergencyState(code); got != w.state {
			t.Errorf("EmergencyState(%d) = %q, want %q", code, got, w.state)
		}
		if code < 7 {
			if got := EmergencyLabel(w.state); got != w.label {
				t.Errorf("EmergencyLabel(%q) = %q, want %q", w.state, got, w.label)
			}
		}
	}
	if EmergencyState(-1) != "" || EmergencyState(8) != "" {
		t.Error("EmergencyState out of range must be empty")
	}
	if EmergencyLabel("bogus") != "" {
		t.Error("EmergencyLabel of an unknown identifier must be empty")
	}
	// Squawk-implied states have labels too.
	for _, code := range []string{"7500", "7600", "7700"} {
		if EmergencyLabel(SquawkEmergency(code)) == "" || SquawkEmergency(code) == "" {
			t.Errorf("no label for the emergency implied by squawk %s", code)
		}
	}
}

func TestADSBVersionLabel(t *testing.T) {
	for v, want := range map[int]string{
		0: "Version 0 (DO-260 / ED-102)", 1: "Version 1 (DO-260A)", 2: "Version 2 (DO-260B / ED-102A)",
		3: "Version 3 (DO-260C / ED-102B)", 4: "Reserved", 7: "Reserved", 8: "", -1: "",
	} {
		if got := ADSBVersionLabel(v); got != want {
			t.Errorf("ADSBVersionLabel(%d) = %q, want %q", v, got, want)
		}
	}
}

// NIC and NACp per RTCA DO-260B tables (containment radius / position
// uncertainty bounds).
func TestIntegrityAndAccuracy(t *testing.T) {
	nic := []float64{0, 37040, 14816, 7408, 3704, 1852, 1111.2, 370.4, 185.2, 75, 25, 7.5}
	for v, want := range nic {
		rc, ok := NICRadiusM(v)
		if v == 0 {
			if ok {
				t.Errorf("NICRadiusM(0) must be unknown")
			}
		} else if !ok || rc != want {
			t.Errorf("NICRadiusM(%d) = %v, %v; want %v", v, rc, ok, want)
		}
		if NICText(v) == "" {
			t.Errorf("NICText(%d) is empty", v)
		}
		if v > 1 && nic[v] >= nic[v-1] {
			t.Errorf("NIC table is not strictly decreasing at %d", v)
		}
	}
	nacp := []float64{0, 18520, 7408, 3704, 1852, 926, 555.6, 185.2, 92.6, 30, 10, 3}
	for v, want := range nacp {
		epu, ok := NACpAccuracyM(v)
		if v == 0 {
			if ok {
				t.Errorf("NACpAccuracyM(0) must be unknown")
			}
		} else if !ok || epu != want {
			t.Errorf("NACpAccuracyM(%d) = %v, %v; want %v", v, epu, ok, want)
		}
		if NACpText(v) == "" {
			t.Errorf("NACpText(%d) is empty", v)
		}
	}
	texts := []struct{ got, want string }{
		{NICText(0), "Integrity unknown"},
		{NICText(8), "Rc < 0.1 NM (185 m)"},
		{NICText(11), "Rc < 7.5 m"},
		{NACpText(0), "Accuracy unknown (EPU >= 10 NM)"},
		{NACpText(9), "EPU < 30 m"},
		{NACpText(10), "EPU < 10 m"},
		{NACpText(11), "EPU < 3 m"},
		{NACvText(0), "Velocity accuracy unknown (>= 10 m/s)"},
		{NACvText(1), "Velocity error < 10 m/s"},
		{NACvText(2), "Velocity error < 3 m/s"},
		{NACvText(4), "Velocity error < 0.3 m/s"},
		{SILText(0), "Integrity level unknown (> 1e-3)"},
		{SILText(3), "<= 1e-7 per hour / sample"},
		{SDAText(0), "Unknown / no safety effect (> 1e-3 per hour)"},
		{SDAText(2), "Major failure condition (<= 1e-5 per hour)"},
		{GVAText(1), "Vertical accuracy <= 150 m"},
		{GVAText(2), "Vertical accuracy <= 45 m"},
	}
	for _, tc := range texts {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
	// Out of range values give empty text / not ok.
	if NICText(-1) != "" || NICText(12) != "" || NACpText(-1) != "" || NACpText(12) != "" ||
		NACvText(-1) != "" || NACvText(5) != "" || SILText(-1) != "" || SILText(4) != "" ||
		SDAText(-1) != "" || SDAText(4) != "" || GVAText(-1) != "" || GVAText(4) != "" {
		t.Error("out-of-range values must give empty text")
	}
	if _, ok := NICRadiusM(12); ok {
		t.Error("NICRadiusM(12) must fail")
	}
	if _, ok := NACpAccuracyM(-1); ok {
		t.Error("NACpAccuracyM(-1) must fail")
	}
	for v := 0; v <= 3; v++ {
		if SILText(v) == "" || SDAText(v) == "" || GVAText(v) == "" {
			t.Errorf("empty SIL / SDA / GVA text for %d", v)
		}
	}
}

func TestCodeLookupsDoNotAllocate(t *testing.T) {
	var sink int
	n := testing.AllocsPerRun(200, func() {
		sink += len(EmitterCategory("A3")) + len(EmitterCategoryCode(4, 3)) + len(SquawkMeaning("7700")) +
			len(SquawkMeaning("1234")) + len(SquawkEmergency("7600")) + len(EmergencyState(1)) +
			len(EmergencyLabel("general")) + len(ADSBVersionLabel(2)) + len(NICText(8)) + len(NACpText(9)) +
			len(NACvText(2)) + len(SILText(3)) + len(SDAText(2)) + len(GVAText(2))
		if info, ok := Squawk("7000"); ok {
			sink += len(info.Region)
		}
	})
	if n != 0 {
		t.Errorf("code lookups allocate %.1f times per run, want 0", n)
	}
	_ = sink
}
