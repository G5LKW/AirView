package refdata

import (
	"encoding/json"
	"sync"
	"testing"

	"airview/internal/model"
)

func TestLookup(t *testing.T) {
	tests := []struct {
		icao uint32
		want Info
	}{
		{0x3C6444, Info{Country: "Germany", CountryCode: "DE", State: "Germany", StateCode: "DE",
			Alloc: AllocState, Registration: "D-AIBD"}},
		{0xA00001, Info{Country: "United States", CountryCode: "US", State: "United States", StateCode: "US",
			Alloc: AllocState, Registration: "N1"}},
		{0x406A9F, Info{Country: "United Kingdom", CountryCode: "GB", State: "United Kingdom", StateCode: "GB",
			Alloc: AllocState}},
		{0xAE01CE, Info{Country: "United States", CountryCode: "US", State: "United States", StateCode: "US",
			Alloc: AllocState, Military: true}},
		{0x43C6F3, Info{Country: "United Kingdom", CountryCode: "GB", State: "United Kingdom", StateCode: "GB",
			Alloc: AllocState, Military: true}},
		{0x424B5A, Info{Country: "Isle of Man", CountryCode: "IM", State: "United Kingdom", StateCode: "GB",
			Alloc: AllocTerritory}},
		{0x789123, Info{Country: "Hong Kong", CountryCode: "HK", State: "China", StateCode: "CN",
			Alloc: AllocTerritory}},
		// San Marino's block is carved out of the European regional pool.
		{0x500001, Info{Country: "San Marino", CountryCode: "SM", State: "San Marino", StateCode: "SM",
			Alloc: AllocState, Region: "ICAO EUR/NAT region (Europe, North Atlantic)"}},
		{0x5FFFFF, Info{Alloc: AllocRegion, Region: "ICAO EUR/NAT region (Europe, North Atlantic)"}},
		{0xF00001, Info{Alloc: AllocReserved, Region: "ICAO (temporary allocations)"}},
		{0xB12345, Info{Alloc: AllocReserved, Region: "Reserved"}},
		{0x000001, Info{Alloc: AllocNone}},
		{0x000000, Info{Alloc: AllocNone}},
		{0x1000000, Info{Alloc: AllocNone}},
	}
	for _, tc := range tests {
		tc.want.ICAO = tc.icao
		if got := Lookup(tc.icao); got != tc.want {
			t.Errorf("Lookup(%06X)\n got %+v\nwant %+v", tc.icao, got, tc.want)
		}
	}
}

func TestInfoJSON(t *testing.T) {
	b, err := json.Marshal(Lookup(0x3C6444))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"country":"Germany","countryCode":"DE","state":"Germany","stateCode":"DE","alloc":"state","reg":"D-AIBD"}`
	if string(b) != want {
		t.Errorf("JSON = %s\nwant   %s", b, want)
	}
	b, _ = json.Marshal(Lookup(0xAE01CE))
	want = `{"country":"United States","countryCode":"US","state":"United States","stateCode":"US","alloc":"state","military":true}`
	if string(b) != want {
		t.Errorf("JSON = %s\nwant   %s", b, want)
	}
	if got := Lookup(0x3C6444).Hex(); got != "3C6444" {
		t.Errorf("Hex() = %q", got)
	}
}

func TestInfoApply(t *testing.T) {
	a := model.Aircraft{ICAO: "3C6444"}
	Lookup(0x3C6444).Apply(&a)
	if a.Country != "Germany" || a.CountryCode != "DE" || a.Military || a.Registration != "D-AIBD" {
		t.Errorf("Apply: %+v", a)
	}

	// A registration that is already there (aircraft database) wins.
	a = model.Aircraft{ICAO: "3C6444", Registration: "D-TEST"}
	Lookup(0x3C6444).Apply(&a)
	if a.Registration != "D-TEST" {
		t.Errorf("Apply overwrote the registration: %q", a.Registration)
	}

	// The military flag is only ever set, never cleared (the database may
	// know better than the address heuristic).
	a = model.Aircraft{ICAO: "4D03C2", Military: true}
	Lookup(0x4D03C2).Apply(&a)
	if !a.Military || a.CountryCode != "LU" {
		t.Errorf("Apply: %+v", a)
	}
	a = model.Aircraft{}
	Lookup(0xAE01CE).Apply(&a)
	if !a.Military || a.Registration != "" {
		t.Errorf("Apply: %+v", a)
	}

	Lookup(0x3C6444).Apply(nil) // must not panic
}

// The three Apply methods layered the way the tracker uses them: address,
// then aircraft database, then callsign.
func TestApplyLayers(t *testing.T) {
	// Airliner with a database record.
	var a model.Aircraft
	Lookup(0x3C6444).Apply(&a)
	ACRecord{ICAO: 0x3C6444, Reg: "D-AIBD", Type: "A319", Desc: "AIRBUS A-319"}.Apply(&a)
	LookupCallsignFor("DLH4AB", 0x3C6444).Apply(&a)
	if a.Registration != "D-AIBD" || a.TypeCode != "A319" || a.TypeDesc != "AIRBUS A-319" ||
		a.Operator != "Lufthansa" || a.Country != "Germany" || a.Military {
		t.Errorf("airliner: %+v", a)
	}

	// The database registration replaces the derived one; a database operator
	// is not replaced by the callsign's; a missing description comes from the
	// embedded type table.
	a = model.Aircraft{}
	Lookup(0x3C6444).Apply(&a)
	ACRecord{ICAO: 0x3C6444, Reg: "D-AIBX", Type: "B738", Operator: "Somebody Leasing"}.Apply(&a)
	LookupCallsign("DLH4AB").Apply(&a)
	if a.Registration != "D-AIBX" || a.TypeDesc != "Boeing 737-800" || a.Operator != "Somebody Leasing" {
		t.Errorf("database precedence: %+v", a)
	}

	// Military flag from the database, from a military operator and from a
	// word callsign.
	a = model.Aircraft{}
	rec := ACRecord{ICAO: 0x4D03C2, Reg: "LX-N90444", Type: "E3TF", Flags: ACMilitary}
	if !rec.Military() {
		t.Error("ACRecord.Military() = false")
	}
	rec.Apply(&a)
	if !a.Military || a.TypeDesc != "Boeing E-3 Sentry (TF33)" {
		t.Errorf("military record: %+v", a)
	}
	a = model.Aircraft{}
	LookupCallsign("RCH871").Apply(&a)
	if !a.Military || a.Operator != "United States Air Force Air Mobility Command" {
		t.Errorf("military operator: %+v", a)
	}
	a = model.Aircraft{}
	LookupCallsign("NATO01").Apply(&a)
	if !a.Military {
		t.Errorf("military word callsign: %+v", a)
	}

	// General aviation without a database record: the registration comes
	// from the callsign, but only when it fits the address.
	a = model.Aircraft{}
	Lookup(0x406A9F).Apply(&a)
	LookupCallsignFor("GBYTE", 0x406A9F).Apply(&a)
	if a.Registration != "G-BYTE" || a.Operator != "" {
		t.Errorf("GA: %+v", a)
	}
	a = model.Aircraft{}
	Lookup(0xA00001).Apply(&a)
	LookupCallsignFor("GBYTE", 0xA00001).Apply(&a)
	if a.Registration != "N1" {
		t.Errorf("GA with contradicting address: %+v", a)
	}

	// Empty records and nil DTOs are harmless.
	a = model.Aircraft{Registration: "X", TypeCode: "Y", TypeDesc: "Z", Operator: "O"}
	ACRecord{}.Apply(&a)
	CallsignInfo{}.Apply(&a)
	if a.Registration != "X" || a.TypeCode != "Y" || a.TypeDesc != "Z" || a.Operator != "O" || a.Military {
		t.Errorf("empty Apply changed the DTO: %+v", a)
	}
	ACRecord{Reg: "X"}.Apply(nil)
	CallsignInfo{}.Apply(nil)
}

func TestFormatParseICAO(t *testing.T) {
	for icao, want := range map[uint32]string{
		0: "000000", 1: "000001", 0x3C6444: "3C6444", 0xABCDEF: "ABCDEF", 0xFFFFFF: "FFFFFF",
		0x1ABCDEF: "ABCDEF", // bits above 24 are dropped
	} {
		if got := FormatICAO(icao); got != want {
			t.Errorf("FormatICAO(%X) = %q, want %q", icao, got, want)
		}
	}
	good := map[string]uint32{
		"3C6444": 0x3C6444, "3c6444": 0x3C6444, "abcdef": 0xABCDEF, "ABCDEF": 0xABCDEF,
		"1": 1, "00a1": 0xA1, "~29f3a1": 0x29F3A1, "FFFFFF": 0xFFFFFF, "000000": 0,
	}
	for s, want := range good {
		if got, ok := ParseICAO(s); !ok || got != want {
			t.Errorf("ParseICAO(%q) = %X, %v; want %X", s, got, ok, want)
		}
	}
	for _, s := range []string{"", "~", "1234567", "3C644G", "3C 644", "0x3C64", " 3C6444", "-1"} {
		if got, ok := ParseICAO(s); ok {
			t.Errorf("ParseICAO(%q) = %X, true; want failure", s, got)
		}
	}
	for a := uint32(0); a <= 0xFFFFFF; a += 9973 {
		if got, ok := ParseICAO(FormatICAO(a)); !ok || got != a {
			t.Fatalf("round trip of %06X gave %06X, %v", a, got, ok)
		}
	}
}

func TestLookupAllocations(t *testing.T) {
	var sink int
	// No derived registration: nothing to allocate.
	n := testing.AllocsPerRun(200, func() {
		sink += len(Lookup(0x406A9F).Country) + len(Lookup(0xAE01CE).Country) + len(Lookup(0x000001).Region)
	})
	if n != 0 {
		t.Errorf("Lookup without registration allocates %.1f times, want 0", n)
	}
	// With a derived registration: exactly the one string.
	n = testing.AllocsPerRun(200, func() {
		sink += len(Lookup(0x3C6444).Registration)
	})
	if n > 1 {
		t.Errorf("Lookup with registration allocates %.1f times, want at most 1", n)
	}
	n = testing.AllocsPerRun(200, func() {
		if a, ok := ParseICAO("3c6444"); ok {
			sink += int(a)
		}
	})
	if n != 0 {
		t.Errorf("ParseICAO allocates %.1f times, want 0", n)
	}
	_ = sink
}

// All lookups work on tables that are immutable after init; hammer them from
// several goroutines and compare with the single-threaded answers.
func TestConcurrentLookups(t *testing.T) {
	addrs := []uint32{0x3C6444, 0xA00001, 0xAE01CE, 0x424B5A, 0x7C4920, 0x71BE31, 0x8406E8, 0x000001}
	calls := []string{"BAW123A", "GBYTE", "RCH871", "NATO01", "N172SP", "XYZ12"}
	wantInfo := make([]Info, len(addrs))
	for i, a := range addrs {
		wantInfo[i] = Lookup(a)
	}
	wantCS := make([]CallsignInfo, len(calls))
	for i, c := range calls {
		wantCS[i] = LookupCallsign(c)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 2000; n++ {
				i := n % len(addrs)
				if got := Lookup(addrs[i]); got != wantInfo[i] {
					select {
					case errs <- "Lookup mismatch for " + FormatICAO(addrs[i]):
					default:
					}
					return
				}
				j := n % len(calls)
				if got := LookupCallsign(calls[j]); got != wantCS[j] {
					select {
					case errs <- "LookupCallsign mismatch for " + calls[j]:
					default:
					}
					return
				}
				if _, ok := LookupType("B738"); !ok {
					select {
					case errs <- "LookupType(B738) failed":
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func BenchmarkLookup(b *testing.B) {
	b.ReportAllocs()
	var n int
	for i := 0; i < b.N; i++ {
		n += len(Lookup(0x3C0000 + uint32(i&0xFFFF)).CountryCode)
	}
	_ = n
}
