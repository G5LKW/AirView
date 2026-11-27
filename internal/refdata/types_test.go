package refdata

import (
	"regexp"
	"strings"
	"testing"
)

func TestTypeTableWellFormed(t *testing.T) {
	designator := regexp.MustCompile(`^[A-Z][A-Z0-9]{1,3}$`)
	description := regexp.MustCompile(`^(-|[LSAHGT][1-8C][JTPER])$`)
	wtc := map[string]bool{"-": true, "L": true, "M": true, "H": true, "J": true, "L/M": true}
	lines := 0
	seen := map[string]bool{}
	for _, line := range strings.Split(typeTable, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines++
		f := strings.Split(line, "|")
		if len(f) != 6 {
			t.Errorf("line %q: %d fields, want 6", line, len(f))
			continue
		}
		if !designator.MatchString(f[0]) {
			t.Errorf("line %q: bad designator", line)
		}
		if seen[f[0]] {
			t.Errorf("designator %s listed twice", f[0])
		}
		seen[f[0]] = true
		if f[1] == "" || f[2] == "" {
			t.Errorf("%s: empty manufacturer or model", f[0])
		}
		if !description.MatchString(f[3]) {
			t.Errorf("%s: bad description code %q", f[0], f[3])
		}
		if !wtc[f[4]] {
			t.Errorf("%s: bad wake turbulence category %q", f[0], f[4])
		}
		if len(f[5]) != 1 || !strings.Contains("ARFBTGLHMUX", f[5]) {
			t.Errorf("%s: bad role %q", f[0], f[5])
		}
		// Role and description code must not contradict each other.
		if f[5] == "H" && f[3][0] != 'H' {
			t.Errorf("%s: helicopter role with description %q", f[0], f[3])
		}
		if f[3][0] == 'H' && f[5] != "H" && f[5] != "M" {
			t.Errorf("%s: helicopter description with role %q", f[0], f[5])
		}
	}
	if lines != TypeCount() {
		t.Errorf("table has %d lines but %d types were loaded", lines, TypeCount())
	}
	if TypeCount() < 300 {
		t.Errorf("only %d types embedded, want a few hundred", TypeCount())
	}
}

func TestLookupType(t *testing.T) {
	tests := []struct {
		designator, name, desc, wtc string
		role                        TypeRole
		engines                     int
		engineType, class           string
	}{
		{"A20N", "Airbus A320neo", "L2J", "M", RoleAirliner, 2, "jet", "landplane"},
		{"A319", "Airbus A319", "L2J", "M", RoleAirliner, 2, "jet", "landplane"},
		{"B738", "Boeing 737-800", "L2J", "M", RoleAirliner, 2, "jet", "landplane"},
		{"B744", "Boeing 747-400", "L4J", "H", RoleAirliner, 4, "jet", "landplane"},
		{"B77W", "Boeing 777-300ER", "L2J", "H", RoleAirliner, 2, "jet", "landplane"},
		{"B789", "Boeing 787-9 Dreamliner", "L2J", "H", RoleAirliner, 2, "jet", "landplane"},
		{"A388", "Airbus A380-800", "L4J", "J", RoleAirliner, 4, "jet", "landplane"},
		{"A359", "Airbus A350-900", "L2J", "H", RoleAirliner, 2, "jet", "landplane"},
		{"BCS3", "Airbus A220-300", "L2J", "M", RoleAirliner, 2, "jet", "landplane"},
		{"MD11", "McDonnell Douglas MD-11", "L3J", "H", RoleAirliner, 3, "jet", "landplane"},
		{"E190", "Embraer E190", "L2J", "M", RoleRegional, 2, "jet", "landplane"},
		{"CRJ9", "Bombardier CRJ900", "L2J", "M", RoleRegional, 2, "jet", "landplane"},
		{"AT76", "ATR ATR 72-600", "L2T", "M", RoleRegional, 2, "turboprop", "landplane"},
		{"DH8D", "De Havilland Canada Dash 8-400", "L2T", "M", RoleRegional, 2, "turboprop", "landplane"},
		{"A124", "Antonov An-124 Ruslan", "L4J", "H", RoleFreighter, 4, "jet", "landplane"},
		{"GLF6", "Gulfstream G650", "L2J", "M", RoleBusiness, 2, "jet", "landplane"},
		{"C56X", "Cessna Citation Excel / XLS", "L2J", "M", RoleBusiness, 2, "jet", "landplane"},
		{"F900", "Dassault Falcon 900", "L3J", "M", RoleBusiness, 3, "jet", "landplane"},
		{"PC12", "Pilatus PC-12", "L1T", "L", RoleUtility, 1, "turboprop", "landplane"},
		{"C172", "Cessna 172 Skyhawk", "L1P", "L", RoleGA, 1, "piston", "landplane"},
		{"P28A", "Piper PA-28 Cherokee / Warrior / Archer", "L1P", "L", RoleGA, 1, "piston", "landplane"},
		{"DA42", "Diamond DA42 Twin Star", "L2P", "L", RoleGA, 2, "piston", "landplane"},
		{"EC35", "Airbus Helicopters EC135 / H135", "H2T", "L", RoleHelicopter, 2, "turboshaft", "helicopter"},
		{"R44", "Robinson R44", "H1P", "L", RoleHelicopter, 1, "piston", "helicopter"},
		{"H47", "Boeing CH-47 Chinook", "H2T", "M", RoleMilitary, 2, "turboshaft", "helicopter"},
		{"C130", "Lockheed C-130 Hercules", "L4T", "M", RoleMilitary, 4, "turboprop", "landplane"},
		{"C17", "Boeing C-17 Globemaster III", "L4J", "H", RoleMilitary, 4, "jet", "landplane"},
		{"B52", "Boeing B-52 Stratofortress", "L8J", "H", RoleMilitary, 8, "jet", "landplane"},
		{"A400", "Airbus A400M Atlas", "L4T", "H", RoleMilitary, 4, "turboprop", "landplane"},
		{"EUFI", "Eurofighter Typhoon", "L2J", "M", RoleMilitary, 2, "jet", "landplane"},
		{"V22", "Bell Boeing V-22 Osprey", "T2T", "M", RoleMilitary, 2, "turboprop", "tiltrotor"},
		{"Q4", "Northrop Grumman RQ-4 Global Hawk", "L1J", "M", RoleUAV, 1, "jet", "landplane"},
		{"GLID", "Glider / sailplane", "", "", RoleLight, 0, "", ""},
		{"BALL", "Balloon", "", "", RoleLight, 0, "", ""},
		{"ZZZZ", "Type without ICAO designator", "", "", RoleOther, 0, "", ""},
	}
	for _, tc := range tests {
		ty, ok := LookupType(tc.designator)
		if !ok {
			t.Errorf("LookupType(%s) not found", tc.designator)
			continue
		}
		if ty.Designator != tc.designator || ty.Name != tc.name || ty.Description != tc.desc ||
			ty.WTC != tc.wtc || ty.Role != tc.role {
			t.Errorf("LookupType(%s) = %+v", tc.designator, ty)
		}
		if ty.Engines() != tc.engines || ty.EngineType() != tc.engineType || ty.AirframeClass() != tc.class {
			t.Errorf("%s: engines %d %q class %q; want %d %q %q", tc.designator,
				ty.Engines(), ty.EngineType(), ty.AirframeClass(), tc.engines, tc.engineType, tc.class)
		}
		if got := TypeName(tc.designator); got != tc.name {
			t.Errorf("TypeName(%s) = %q, want %q", tc.designator, got, tc.name)
		}
		if ty.Manufacturer != "" && ty.Name != ty.Manufacturer+" "+ty.Model {
			t.Errorf("%s: name %q is not manufacturer + model", tc.designator, ty.Name)
		}
	}
	for _, d := range []string{"", "XXXX", "b738", "B7380", "B73"} {
		if ty, ok := LookupType(d); ok {
			t.Errorf("LookupType(%q) = %+v, want not found", d, ty)
		}
		if n := TypeName(d); n != "" {
			t.Errorf("TypeName(%q) = %q, want empty", d, n)
		}
	}
	// Generic designators have no manufacturer.
	if ty, _ := LookupType("GLID"); ty.Manufacturer != "" || ty.Model != "Glider / sailplane" {
		t.Errorf("GLID: %+v", ty)
	}
}

func TestTypeTexts(t *testing.T) {
	a388, _ := LookupType("A388")
	if got := a388.ClassText(); got != "Landplane, 4 jet engines" {
		t.Errorf("A388 ClassText = %q", got)
	}
	if got := a388.WTCText(); got != "Super (Airbus A380)" {
		t.Errorf("A388 WTCText = %q", got)
	}
	for desc, want := range map[string]string{
		"L2J": "Landplane, 2 jet engines",
		"L1P": "Landplane, 1 piston engine",
		"L4T": "Landplane, 4 turboprop engines",
		"H1T": "Helicopter, 1 turboshaft engine",
		"H2T": "Helicopter, 2 turboshaft engines",
		"A1P": "Amphibian, 1 piston engine",
		"S1P": "Seaplane, 1 piston engine",
		"G1P": "Gyrocopter, 1 piston engine",
		"T2T": "Tiltrotor, 2 turboprop engines",
		"R2T": "Tiltrotor, 2 turboprop engines", // Mictronics spelling
		"L1E": "Landplane, 1 electric engine",
		"L1R": "Landplane, 1 rocket engine",
		"LCP": "Landplane, 2 piston engines", // C = two coupled engines
		"L0-": "Landplane",                   // glider: no engine
		"L":   "Landplane",
		"":    "",
		"-":   "",
		"X2J": "",
	} {
		if got := TypeClassText(desc); got != want {
			t.Errorf("TypeClassText(%q) = %q, want %q", desc, got, want)
		}
	}
	for code, want := range map[string]string{
		"L": "Light (7,000 kg or less)", "M": "Medium (7,000 to 136,000 kg)",
		"H": "Heavy (136,000 kg or more)", "J": "Super (Airbus A380)",
		"L/M": "Light or medium, depending on variant", "": "", "X": "", "l": "",
	} {
		if got := WTCText(code); got != want {
			t.Errorf("WTCText(%q) = %q, want %q", code, got, want)
		}
	}
	if TypeEngines("L") != 0 || TypeEngines("L9J") != 0 || TypeEngineType("L2") != "" || TypeEngineType("L2X") != "" {
		t.Error("malformed description codes must give zero values")
	}
}

func TestTypeLookupAllocations(t *testing.T) {
	var sink int
	n := testing.AllocsPerRun(200, func() {
		if ty, ok := LookupType("B738"); ok {
			sink += len(ty.Name) + ty.Engines() + len(ty.EngineType()) + len(ty.AirframeClass()) + len(ty.WTCText())
		}
		sink += len(TypeName("A20N")) + len(TypeName("NOPE"))
	})
	if n != 0 {
		t.Errorf("type lookups allocate %.1f times per run, want 0", n)
	}
	_ = sink
}
