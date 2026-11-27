package refdata

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
)

// Cross-checks against a real aircraft database. They need a local copy of
// the tar1090-db aircraft.csv.gz (or the uncompressed aircraft.csv) and are
// skipped unless its path is given:
//
//	AIRVIEW_REFDATA_DB=C:\path\aircraft.csv.gz go test ./internal/refdata/ -run AgainstDatabase -v

const dbEnv = "AIRVIEW_REFDATA_DB"

// loadDatabase streams the database named by AIRVIEW_REFDATA_DB through
// Parse and hands every record to fn.
func loadDatabase(t *testing.T, fn func(ACRecord)) ParseResult {
	t.Helper()
	path := os.Getenv(dbEnv)
	if path == "" {
		t.Skipf("set %s to the path of a tar1090-db aircraft.csv(.gz) to run this check", dbEnv)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res, err := Parse(context.Background(), f, SinkFunc(func(r ACRecord) error {
		fn(r)
		return nil
	}), ParseOptions{})
	if err != nil {
		t.Fatalf("Parse(%s): %v", path, err)
	}
	t.Logf("database: %d records, %d skipped, format %s, gzip %v", res.Records, res.Skipped, res.Format, res.Gzip)
	return res
}

// registrationRule names the rule Registration applies to icao ("" = none).
func registrationRule(icao uint32) string {
	if IsMilitary(icao) {
		return ""
	}
	switch {
	case icao >= usFirst && icao <= usLast:
		return "N (United States)"
	case icao >= jaFirst && icao <= jaLast:
		return "JA (Japan)"
	case icao >= hlFirst && icao <= hlLast:
		return "HL (South Korea)"
	}
	for i := range numericMaps {
		if m := &numericMaps[i]; icao >= m.lo && icao <= m.hi {
			return fmt.Sprintf("%s<number> @%06X", m.prefix, m.lo)
		}
	}
	if m := findStride(icao); m != nil {
		return fmt.Sprintf("%sxxx @%06X", m.prefix, m.lo)
	}
	return ""
}

// TestRegistrationAgainstDatabase measures, for every address-to-registration
// rule, how many database airframes inside the rule's range carry exactly the
// registration the rule computes. This is the measurement quoted in
// registration.go; it fails when a rule drops below 90 %.
func TestRegistrationAgainstDatabase(t *testing.T) {
	type tally struct {
		total, match int
		example      string
	}
	rules := map[string]*tally{}
	loadDatabase(t, func(r ACRecord) {
		if r.Reg == "" || r.Reg == "GND" || r.Reg == "TWR" {
			return // no mark, or an airport ground vehicle / tower transponder
		}
		// About a tenth of the Canadian C-Fxxx airframes are recorded in the
		// database with the F missing ("C-AAC" for C-FAAC). "C-" plus three
		// letters is not a Canadian mark at all, so those rows are noise.
		if len(r.Reg) == 5 && r.Reg[:2] == "C-" && r.ICAO >= 0xC00000 && r.ICAO <= 0xC3FFFF {
			return
		}
		rule := registrationRule(r.ICAO)
		if rule == "" {
			return
		}
		got := Registration(r.ICAO)
		if got == "" {
			return // address in range but not a valid letter combination
		}
		ty := rules[rule]
		if ty == nil {
			ty = &tally{}
			rules[rule] = ty
		}
		ty.total++
		if got == r.Reg {
			ty.match++
		} else if ty.example == "" {
			ty.example = fmt.Sprintf("%06X db=%s derived=%s", r.ICAO, r.Reg, got)
		}
	})
	names := make([]string, 0, len(rules))
	for n := range rules {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ty := rules[n]
		pct := 100 * float64(ty.match) / float64(ty.total)
		t.Logf("%-28s %7d airframes  %6.2f %% agree   first mismatch: %s", n, ty.total, pct, ty.example)
		if ty.total >= 20 && pct < 90 {
			t.Errorf("rule %s agrees with only %.1f %% of %d database airframes", n, pct, ty.total)
		}
	}
	if len(rules) == 0 {
		t.Error("no database airframe fell into any registration rule")
	}
}

// TestCountryAgainstDatabase checks the allocation table against the
// registration prefixes recorded in the database: an airframe whose address is
// in a state's block should, in the overwhelming majority of cases, carry one
// of that state's nationality marks.
func TestCountryAgainstDatabase(t *testing.T) {
	// A sample of unambiguous nationality marks.
	marks := map[string]string{
		"D-": "DE", "G-": "GB", "F-": "FR", "I-": "IT", "EC-": "ES", "PH-": "NL", "OO-": "BE",
		"HB-": "CH", "OE-": "AT", "OY-": "DK", "SE-": "SE", "LN-": "NO", "OH-": "FI", "EI-": "IE",
		"SP-": "PL", "OK-": "CZ", "CS-": "PT", "SX-": "GR", "TC-": "TR", "VH-": "AU", "ZK-": "NZ",
		"C-": "CA", "JA": "JP", "HL": "KR", "VT-": "IN", "9V-": "SG", "A6-": "AE", "A7-": "QA",
		"ZS-": "ZA", "LV-": "AR", "PR-": "BR", "PT-": "BR", "PP-": "BR", "XA-": "MX", "RA-": "RU",
		"UR-": "UA", "YR-": "RO", "HA-": "HU", "LZ-": "BG", "9A-": "HR", "S5-": "SI", "OM-": "SK",
		"ES-": "EE", "YL-": "LV", "LY-": "LT", "TF-": "IS", "LX-": "LU", "9H-": "MT", "4X-": "IL",
		"HZ-": "SA", "9M-": "MY", "HS-": "TH", "PK-": "ID", "RP-": "PH", "CC-": "CL", "HK-": "CO",
	}
	type tally struct{ total, match int }
	byISO := map[string]*tally{}
	loadDatabase(t, func(r ACRecord) {
		if r.Reg == "" {
			return
		}
		var iso string
		for n := 3; n >= 2 && iso == ""; n-- {
			if len(r.Reg) > n {
				iso = marks[r.Reg[:n]]
			}
		}
		if iso == "" {
			return
		}
		ty := byISO[iso]
		if ty == nil {
			ty = &tally{}
			byISO[iso] = ty
		}
		ty.total++
		if c, ok := StateOf(r.ICAO); ok && c.ISO2 == iso {
			ty.match++
		}
	})
	isos := make([]string, 0, len(byISO))
	for iso := range byISO {
		isos = append(isos, iso)
	}
	sort.Strings(isos)
	for _, iso := range isos {
		ty := byISO[iso]
		pct := 100 * float64(ty.match) / float64(ty.total)
		t.Logf("%s %-22s %7d airframes with the state's mark, %6.2f %% inside its address block", iso, CountryName(iso), ty.total, pct)
		if ty.total >= 50 && pct < 90 {
			t.Errorf("%s: only %.1f %% of %d airframes are inside the state's address block", iso, pct, ty.total)
		}
	}
}
