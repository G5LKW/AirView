package refdata

import (
	"testing"
)

func checkBlocks(t *testing.T, name string, blocks []addrBlock, wantISO bool) {
	t.Helper()
	if len(blocks) == 0 {
		t.Fatalf("%s is empty", name)
	}
	for i, b := range blocks {
		if b.lo > b.hi || b.hi > 0xFFFFFF {
			t.Errorf("%s[%d] %06X-%06X: bad range", name, i, b.lo, b.hi)
		}
		if i > 0 && blocks[i-1].hi >= b.lo {
			t.Errorf("%s[%d] %06X-%06X overlaps or is not sorted after %06X-%06X",
				name, i, b.lo, b.hi, blocks[i-1].lo, blocks[i-1].hi)
		}
		if b.name == "" {
			t.Errorf("%s[%d] %06X: empty name", name, i, b.lo)
		}
		if wantISO && (len(b.iso) != 2 || !isUpper(b.iso[0]) || !isUpper(b.iso[1])) {
			t.Errorf("%s[%d] %s: bad ISO code %q", name, i, b.name, b.iso)
		}
	}
}

func TestBlockTablesWellFormed(t *testing.T) {
	checkBlocks(t, "stateBlocks", stateBlocks, true)
	checkBlocks(t, "territoryBlocks", territoryBlocks, true)
	checkBlocks(t, "militaryBlocks", militaryBlocks, true)
	for i, r := range regionBlocks {
		if r.lo > r.hi || r.hi > 0xFFFFFF || r.name == "" {
			t.Errorf("regionBlocks[%d]: bad entry", i)
		}
		if i > 0 && regionBlocks[i-1].hi >= r.lo {
			t.Errorf("regionBlocks[%d] overlaps or is not sorted", i)
		}
		if r.kind != AllocRegion && r.kind != AllocReserved {
			t.Errorf("regionBlocks[%d]: kind %v", i, r.kind)
		}
	}
	// The ICAO table has 190-odd contracting states; guard against the table
	// being truncated by accident.
	if len(stateBlocks) < 190 {
		t.Errorf("stateBlocks has only %d entries", len(stateBlocks))
	}
	// One name per ISO code and one ISO code per name.
	byISO := map[string]string{}
	for _, b := range stateBlocks {
		if n, ok := byISO[b.iso]; ok && n != b.name {
			t.Errorf("ISO %s used for both %q and %q", b.iso, n, b.name)
		}
		byISO[b.iso] = b.name
	}
}

// Territory and military blocks are refinements of a state's block.
func TestSubBlocksInsideStates(t *testing.T) {
	for _, b := range territoryBlocks {
		i, j := findBlock(stateBlocks, b.lo), findBlock(stateBlocks, b.hi)
		if i < 0 || i != j {
			t.Errorf("territory %s %06X-%06X is not inside one state block", b.name, b.lo, b.hi)
		}
	}
	for _, b := range militaryBlocks {
		i, j := findBlock(stateBlocks, b.lo), findBlock(stateBlocks, b.hi)
		if i < 0 || i != j {
			t.Errorf("military block %06X-%06X is not inside one state block", b.lo, b.hi)
			continue
		}
		if stateBlocks[i].iso != b.iso || stateBlocks[i].name != b.name {
			t.Errorf("military block %06X-%06X labelled %s/%s but lies in %s/%s",
				b.lo, b.hi, b.name, b.iso, stateBlocks[i].name, stateBlocks[i].iso)
		}
	}
}

// Real airframes (address and registration as recorded in the tar1090-db
// aircraft database, November 2026).
func TestCountryOfRealAircraft(t *testing.T) {
	tests := []struct {
		icao      uint32
		name, iso string
		what      string
	}{
		{0x3C6444, "Germany", "DE", "D-AIBD Lufthansa A319"},
		{0x406A9F, "United Kingdom", "GB", "G-ZBJH British Airways 787"},
		{0x4CA242, "Ireland", "IE", "EI-DCL Ryanair 737"},
		{0x484161, "Netherlands", "NL", "PH-BXH KLM 737"},
		{0x39BDA4, "France", "FR", "F-HPNE Air France A220"},
		{0x4B17E5, "Switzerland", "CH", "HB-JBH Swiss A220"},
		{0x44CDC1, "Belgium", "BE", "OO-SNA"},
		{0x45AC45, "Denmark", "DK", "OY-KBE"},
		{0x461F36, "Finland", "FI", "OH-LVI"},
		{0x46B8A4, "Greece", "GR", "SX-NED"},
		{0x471F62, "Hungary", "HU", "HA-LXA Wizz Air"},
		{0x48AE01, "Poland", "PL", "SP-LRB LOT 787"},
		{0x4952A6, "Portugal", "PT", "CS-TUF"},
		{0x4A0661, "Romania", "RO", "YR-ASA"},
		{0x4AC9E1, "Sweden", "SE", "SE-ROA"},
		{0x4BA9C1, "Turkey", "TR", "TC-JNA"},
		{0x4C01E4, "Serbia", "RS", "YU-APE"},
		{0x4D03C2, "Luxembourg", "LU", "LX-N90444 NATO E-3A"},
		{0x151DA6, "Russia", "RU", "RA-73126"},
		{0x02A1A0, "Tunisia", "TN", "TS-INA"},
		{0x06A0A4, "Qatar", "QA", "A7-MBK"},
		{0x0B04E2, "Cuba", "CU", "CU-T1250"},
		{0x710105, "Saudi Arabia", "SA", "HZ-AK27"},
		{0x71BE31, "South Korea", "KR", "HL7631"},
		{0x728639, "Iraq", "IQ", "YI-AQY"},
		{0x732441, "Iran", "IR", "EP-IBA"},
		{0x738065, "Israel", "IL", "4X-EKC"},
		{0x740733, "Jordan", "JO", "JY-AYS"},
		{0x750467, "Malaysia", "MY", "9M-AGU"},
		{0x7608EA, "Pakistan", "PK", "AP-BGJ"},
		{0x76BCC8, "Singapore", "SG", "9V-OFH"},
		{0x778564, "Syria", "SY", "YK-AKD"},
		{0x780A3F, "China", "CN", "B-LJJ (China block)"},
		{0x7C4920, "Australia", "AU", "VH-OQA Qantas A380"},
		{0x8406E8, "Japan", "JP", "JA01XJ"},
		{0x88003D, "Thailand", "TH", "HS-TKA"},
		{0x896451, "United Arab Emirates", "AE", "A6-EQB Emirates 777"},
		{0xA00001, "United States", "US", "N1"},
		{0xAB1644, "United States", "US", "N813UA"},
		{0xAE01CE, "United States", "US", "62-4134 USAF RC-135"},
		{0xC0172E, "Canada", "CA", "C-FIUF Air Canada 777"},
		{0xC822AE, "New Zealand", "NZ", "ZK-GBF"},
		{0xE06541, "Argentina", "AR", "LV-FUA"},
		{0xE48B2A, "Brazil", "BR", "PP-SSZ"},
		{0x901000, "Cook Islands", "CK", "first Cook Islands address"},
	}
	for _, tc := range tests {
		c, ok := CountryOf(tc.icao)
		if !ok || c.Name != tc.name || c.ISO2 != tc.iso {
			t.Errorf("CountryOf(%06X) [%s] = %+v, %v; want %s/%s", tc.icao, tc.what, c, ok, tc.name, tc.iso)
		}
		if s, ok := StateOf(tc.icao); !ok || s != c {
			t.Errorf("StateOf(%06X) = %+v, %v; want the same as CountryOf", tc.icao, s, ok)
		}
		if got := CountryName(tc.iso); got != tc.name {
			t.Errorf("CountryName(%s) = %q, want %q", tc.iso, got, tc.name)
		}
	}
}

func TestCountryBlockEdges(t *testing.T) {
	tests := []struct {
		icao uint32
		iso  string // "" = not allocated to a state
	}{
		{0x37FFFF, "ES"}, {0x380000, "FR"}, {0x3BFFFF, "FR"}, {0x3C0000, "DE"}, {0x3FFFFF, "DE"},
		{0x43FFFF, "GB"}, {0x440000, "AT"}, {0x447FFF, "AT"}, {0x448000, "BE"},
		{0x9FFFFF, ""}, {0xA00000, "US"}, {0xAFFFFF, "US"}, {0xB00000, ""},
		{0xBFFFFF, ""}, {0xC00000, "CA"}, {0xC3FFFF, "CA"}, {0xC40000, ""},
		{0x003FFF, ""}, {0x004000, "ZW"}, {0x0047FF, "ZW"}, {0x004800, ""},
		{0xE94FFF, "BO"}, {0xE95000, ""}, {0xFFFFFF, ""},
		{0x000000, ""}, {0x000001, ""}, {0x1000000, ""}, {0xFFFFFFFF, ""},
	}
	for _, tc := range tests {
		c, ok := StateOf(tc.icao)
		if ok != (tc.iso != "") || c.ISO2 != tc.iso {
			t.Errorf("StateOf(%06X) = %+v, %v; want ISO %q", tc.icao, c, ok, tc.iso)
		}
	}
}

func TestTerritories(t *testing.T) {
	tests := []struct {
		icao             uint32
		country, iso     string
		state, stateCode string
	}{
		{0x424B5A, "Isle of Man", "IM", "United Kingdom", "GB"}, // M-ABMI
		{0x43E8DE, "Isle of Man", "IM", "United Kingdom", "GB"}, // M-AGIC
		{0x400000, "Bermuda", "BM", "United Kingdom", "GB"},
		{0x4001C5, "Cayman Islands", "KY", "United Kingdom", "GB"},
		{0x789123, "Hong Kong", "HK", "China", "CN"},
	}
	for _, tc := range tests {
		c, ok := CountryOf(tc.icao)
		if !ok || c.Name != tc.country || c.ISO2 != tc.iso {
			t.Errorf("CountryOf(%06X) = %+v, %v; want %s/%s", tc.icao, c, ok, tc.country, tc.iso)
		}
		s, ok := StateOf(tc.icao)
		if !ok || s.Name != tc.state || s.ISO2 != tc.stateCode {
			t.Errorf("StateOf(%06X) = %+v, %v; want %s/%s", tc.icao, s, ok, tc.state, tc.stateCode)
		}
	}
}

// The binary search must agree with a plain linear scan everywhere.
func TestFindBlockMatchesLinearScan(t *testing.T) {
	linear := func(blocks []addrBlock, a uint32) int {
		for i := range blocks {
			if a >= blocks[i].lo && a <= blocks[i].hi {
				return i
			}
		}
		return -1
	}
	for _, blocks := range [][]addrBlock{stateBlocks, territoryBlocks, militaryBlocks} {
		check := func(a uint32) {
			if got, want := findBlock(blocks, a), linear(blocks, a); got != want {
				t.Fatalf("findBlock(%06X) = %d, linear scan = %d", a, got, want)
			}
		}
		for _, b := range blocks {
			for _, a := range []uint32{b.lo - 1, b.lo, b.lo + 1, b.hi - 1, b.hi, b.hi + 1} {
				check(a)
			}
		}
		for a := uint32(0); a <= 0xFFFFFF; a += 4099 {
			check(a)
		}
	}
}

// IsMilitary on real airframes that the database flags as military, and on
// civil ones.
func TestIsMilitary(t *testing.T) {
	military := map[uint32]string{
		0xAE01CE: "62-4134 USAF RC-135W",
		0xADFDF8: "82-8000 USAF VC-25A",
		0xAE07E1: "94-0067 USAF C-17",
		0x43C6F3: "ZZ330 RAF Voyager",
		0x3B7540: "F-SDXE French military",
		0x3EA12C: "10+01 Luftwaffe A350",
		0x3F4567: "German military block",
		0xADF7C8: "first address after N99999",
		0xAFFFFF: "last US address",
		0xC20000: "first Canadian military address",
	}
	for icao, what := range military {
		if !IsMilitary(icao) {
			t.Errorf("IsMilitary(%06X) [%s] = false", icao, what)
		}
	}
	civil := map[uint32]string{
		0x3C6444:  "D-AIBD",
		0x406A9F:  "G-ZBJH",
		0xA00001:  "N1",
		0xADF7C7:  "N99999",
		0xC0172E:  "C-FIUF",
		0x4D03C2:  "LX-N90444 (military, but in the ordinary Luxembourg block: the heuristic cannot know)",
		0xB00000:  "reserved",
		0x000000:  "zero",
		0x1AE01CE: "not a 24-bit address",
	}
	for icao, what := range civil {
		if IsMilitary(icao) {
			t.Errorf("IsMilitary(%06X) [%s] = true", icao, what)
		}
	}
}

func TestCountryName(t *testing.T) {
	for iso, want := range map[string]string{
		"DE": "Germany", "GB": "United Kingdom", "HK": "Hong Kong", "MO": "Macau", "FO": "Faroe Islands",
		"TW": "Taiwan", "XX": "", "": "", "de": "",
	} {
		if got := CountryName(iso); got != want {
			t.Errorf("CountryName(%q) = %q, want %q", iso, got, want)
		}
	}
}

func TestAllocKindText(t *testing.T) {
	for k, want := range map[AllocKind]string{
		AllocNone: "none", AllocState: "state", AllocTerritory: "territory",
		AllocRegion: "region", AllocReserved: "reserved", AllocKind(99): "none",
	} {
		if k.String() != want {
			t.Errorf("AllocKind(%d).String() = %q, want %q", k, k.String(), want)
		}
		if b, err := k.MarshalText(); err != nil || string(b) != want {
			t.Errorf("AllocKind(%d).MarshalText() = %q, %v", k, b, err)
		}
	}
}

func TestCountryLookupsDoNotAllocate(t *testing.T) {
	addrs := []uint32{0x3C6444, 0x424B5A, 0xAE01CE, 0x000001, 0xF00001, 0x789123, 0xFFFFFF}
	var sink int
	n := testing.AllocsPerRun(200, func() {
		for _, a := range addrs {
			c, ok := CountryOf(a)
			s, _ := StateOf(a)
			if ok && IsMilitary(a) {
				sink++
			}
			sink += len(c.Name) + len(s.ISO2)
		}
	})
	if n != 0 {
		t.Errorf("CountryOf / StateOf / IsMilitary allocate %.1f times per run, want 0", n)
	}
	_ = sink
}

func BenchmarkCountryOf(b *testing.B) {
	b.ReportAllocs()
	var n int
	for i := 0; i < b.N; i++ {
		c, _ := CountryOf(uint32(i*7919) & 0xFFFFFF)
		n += len(c.ISO2)
	}
	_ = n
}
