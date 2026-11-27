package modes

import (
	"errors"
	"reflect"
	"testing"

	"airview/internal/model"
)

func TestFormatParseICAO(t *testing.T) {
	for _, c := range []struct {
		addr uint32
		s    string
	}{
		{0x000000, "000000"}, {0x4840D6, "4840D6"}, {0xFFFFFF, "FFFFFF"}, {0x00ABCD, "00ABCD"},
		{0x4840D6 | NonICAOFlag, "~4840D6"}, {NonICAOFlag, "~000000"},
	} {
		if got := FormatICAO(c.addr); got != c.s {
			t.Errorf("FormatICAO(%X) = %q, want %q", c.addr, got, c.s)
		}
		if got := string(AppendICAO([]byte("x"), c.addr)); got != "x"+c.s {
			t.Errorf("AppendICAO(%X) = %q", c.addr, got)
		}
		got, err := ParseICAO(c.s)
		if err != nil || got != c.addr {
			t.Errorf("ParseICAO(%q) = %X, %v", c.s, got, err)
		}
	}
	if got, err := ParseICAO("4840d6"); err != nil || got != 0x4840D6 {
		t.Errorf("lower case: %X, %v", got, err)
	}
	for _, bad := range []string{"", "4840D", "4840D6A", "4840DG", "~4840D", "~~840D6", " 4840D6", "0x4840"} {
		if _, err := ParseICAO(bad); !errors.Is(err, ErrBadICAO) {
			t.Errorf("ParseICAO(%q) = %v, want ErrBadICAO", bad, err)
		}
	}
	// Exhaustive round trip over a stride of the address space.
	for a := uint32(0); a <= 0xFFFFFF; a += 4099 {
		if got, err := ParseICAO(FormatICAO(a)); err != nil || got != a {
			t.Fatalf("round trip %06X: %X, %v", a, got, err)
		}
	}
	m := mustDecode(t, bookIdent)
	if m.ICAOString() != "4840D6" {
		t.Errorf("ICAOString = %q", m.ICAOString())
	}
}

func TestSquawkAndCallsignStrings(t *testing.T) {
	for squawk, want := range map[uint16]string{0x0000: "0000", 0x7700: "7700", 0x1234: "1234", 0x7777: "7777", 0x0356: "0356"} {
		if got := FormatSquawk(squawk); got != want {
			t.Errorf("FormatSquawk(%04X) = %q", squawk, got)
		}
		if got := string(AppendSquawk(nil, squawk)); got != want {
			t.Errorf("AppendSquawk(%04X) = %q", squawk, got)
		}
	}
	if got := TrimCallsign([8]byte{'K', 'L', 'M', '1', '0', '2', '3', ' '}); got != "KLM1023" {
		t.Errorf("TrimCallsign = %q", got)
	}
	if got := TrimCallsign([8]byte{' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}); got != "" {
		t.Errorf("TrimCallsign(spaces) = %q", got)
	}
	if got := TrimCallsign([8]byte{'A', ' ', 'B'}); got != "A B" {
		t.Errorf("TrimCallsign(zero padded) = %q", got)
	}
	var empty Message
	if empty.CallsignString() != "" || empty.SquawkString() != "" || empty.CategoryString() != "" || empty.EmergencyString() != "" {
		t.Error("empty message must give empty strings")
	}
}

func TestCategoryName(t *testing.T) {
	for _, c := range []struct {
		cat  uint8
		want string
	}{
		{0xA0, "A0"}, {0xA3, "A3"}, {0xA7, "A7"}, {0xB1, "B1"}, {0xC2, "C2"}, {0xD7, "D7"},
		{0x00, ""}, {0x93, ""}, {0xE0, ""}, {0xA8, ""}, {0xAF, ""}, {0xFF, ""},
	} {
		if got := CategoryName(c.cat); got != c.want {
			t.Errorf("CategoryName(%02X) = %q, want %q", c.cat, got, c.want)
		}
	}
}

func TestEmergencyNames(t *testing.T) {
	// The names are the values of model.Aircraft.Emergency.
	want := []string{"", "general", "medical", "minfuel", "nordo", "unlawful", "downed", ""}
	for state, name := range want {
		if got := EmergencyName(uint8(state)); got != name {
			t.Errorf("EmergencyName(%d) = %q, want %q", state, got, name)
		}
	}
	if EmergencyName(8) != "" || EmergencyName(255) != "" {
		t.Error("out of range emergency state must be empty")
	}
	if EmergencyGeneral != 1 || EmergencyMedical != 2 || EmergencyMinFuel != 3 || EmergencyNoComms != 4 ||
		EmergencyUnlawful != 5 || EmergencyDowned != 6 || EmergencyReserved != 7 || EmergencyNone != 0 {
		t.Error("emergency constants do not match the DO-260B encoding")
	}
	for squawk, name := range map[uint16]string{0x7500: "unlawful", 0x7600: "nordo", 0x7700: "general", 0x7000: "", 0x1200: ""} {
		if got := SquawkEmergency(squawk); got != name {
			t.Errorf("SquawkEmergency(%04X) = %q, want %q", squawk, got, name)
		}
	}
	// A DF5 reply squawking 7600.
	f := make([]byte, 7)
	f[0] = 5 << 3
	id := id13FromModeA(0x7600)
	f[2], f[3] = byte(id>>8), byte(id)
	SetParity(f, 0x400000)
	var m Message
	if err := Decode(f, &m); err != nil || m.EmergencyString() != "nordo" {
		t.Errorf("DF5 7600: err=%v emergency=%q", err, m.EmergencyString())
	}
}

func TestDFHelpers(t *testing.T) {
	seen := map[int]uint8{}
	for df := uint8(0); df < 32; df++ {
		idx := DFIndex(df)
		if idx < 0 || idx >= NumDFIndex {
			t.Fatalf("DFIndex(%d) = %d out of range", df, idx)
		}
		if DFName(df) == "" {
			t.Errorf("DFName(%d) is empty", df)
		}
		if want := 7 + 7*int(df>>4); FrameLen(df) != want {
			t.Errorf("FrameLen(%d) = %d", df, FrameLen(df))
		}
		if idx != DFIndexOther {
			if prev, dup := seen[idx]; dup {
				t.Errorf("DF%d and DF%d share index %d", prev, df, idx)
			}
			seen[idx] = df
		}
	}
	// The order is the column order of the stats tables in ARCHITECTURE.md:
	// df0,df4,df5,df11,df16,df17,df18,df20,df21,df_other.
	wantLabels := []string{"df0", "df4", "df5", "df11", "df16", "df17", "df18", "df20", "df21", "df_other"}
	if NumDFIndex != len(wantLabels) {
		t.Fatalf("NumDFIndex = %d", NumDFIndex)
	}
	for i, df := range []uint8{0, 4, 5, 11, 16, 17, 18, 20, 21} {
		if DFIndex(df) != i || DFIndexLabel(i) != wantLabels[i] {
			t.Errorf("DF%d: index %d label %q", df, DFIndex(df), DFIndexLabel(i))
		}
	}
	if DFIndexLabel(DFIndexOther) != "df_other" || DFIndexLabel(-1) != "" || DFIndexLabel(NumDFIndex) != "" {
		t.Error("DFIndexLabel bounds")
	}
	if DFIndex(19) != DFIndexOther || DFIndex(24) != DFIndexOther || DFIndex(200) != DFIndexOther {
		t.Error("formats without a counter must share DFIndexOther")
	}
	if DFName(17) != "Extended squitter" || DFName(11) != "All-call reply" || DFName(27) != DFName(24) || DFName(3) != "Unassigned" {
		t.Error("DFName")
	}
	if FrameDF(nil) != 0xFF || FrameDF([]byte{0x8D}) != 17 || FrameDF([]byte{0x5D}) != 11 || FrameDF([]byte{0xFF}) != 24 || FrameDF([]byte{0x02}) != 0 {
		t.Error("FrameDF")
	}
}

func TestEnumLabels(t *testing.T) {
	// Address type labels are the values of model.Aircraft.AddrType.
	for at, want := range map[AddrType][2]string{
		AddrUnknown:       {"non-icao", "unknown"},
		AddrModeS:         {"icao", "mode_s"},
		AddrADSBICAO:      {"icao", "adsb_icao"},
		AddrADSBICAONT:    {"icao", "adsb_icao_nt"},
		AddrADSBOther:     {"non-icao", "adsb_other"},
		AddrTISBICAO:      {"icao", "tisb_icao"},
		AddrTISBTrackFile: {"tisb-track", "tisb_trackfile"},
		AddrTISBOther:     {"anon", "tisb_other"},
		AddrADSRICAO:      {"icao", "adsr_icao"},
		AddrADSROther:     {"non-icao", "adsr_other"},
	} {
		if at.Label() != want[0] || at.String() != want[1] || at.IsICAO() != (want[0] == "icao") {
			t.Errorf("AddrType %d: label %q string %q icao=%v", at, at.Label(), at.String(), at.IsICAO())
		}
	}
	for src, want := range map[Source]string{
		SourceUnknown: model.SrcOther, SourceModeS: model.SrcModeS, SourceADSB: model.SrcADSB,
		SourceTISB: model.SrcTISB, SourceADSR: model.SrcADSR,
	} {
		if src.Label() != want {
			t.Errorf("Source %d label %q, want %q", src, src.Label(), want)
		}
	}
	for crc, want := range map[CRCStatus]string{
		CRCUnknown: "unknown", CRCOK: "ok", CRCInterrogator: "interrogator", CRCAddressParity: "address-parity",
		CRCFixed1: "fixed-1bit", CRCFixed2: "fixed-2bit", CRCBad: "bad",
	} {
		if crc.String() != want {
			t.Errorf("CRCStatus %d = %q", crc, crc.String())
		}
	}
	// Nav mode names are the values of model.Aircraft.NavModes, in order.
	all := NavAutopilot | NavVNAV | NavAltHold | NavApproach | NavLNAV | NavTCAS
	if got := all.AppendNames(nil); !reflect.DeepEqual(got, []string{"autopilot", "vnav", "althold", "approach", "lnav", "tcas"}) {
		t.Errorf("AppendNames(all) = %v", got)
	}
	if got := (NavVNAV | NavTCAS).AppendNames([]string{"x"}); !reflect.DeepEqual(got, []string{"x", "vnav", "tcas"}) {
		t.Errorf("AppendNames = %v", got)
	}
	if got := NavModes(0).AppendNames(nil); len(got) != 0 {
		t.Errorf("AppendNames(0) = %v", got)
	}
	for b, want := range map[BDS]string{BDS10: "1,0", BDS17: "1,7", BDS20: "2,0", BDS30: "3,0", BDS40: "4,0", BDS44: "4,4", BDS45: "4,5", BDS50: "5,0", BDS60: "6,0", 0x0A: "0,A", 0x5F: "5,F"} {
		if b.String() != want {
			t.Errorf("BDS %02X = %q", uint8(b), b.String())
		}
	}
}
