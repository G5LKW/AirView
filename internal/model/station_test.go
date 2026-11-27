package model

import (
	"bytes"
	"crypto/rand"
	"math"
	"strings"
	"testing"
	"time"
)

func TestSIDAndFeederID(t *testing.T) {
	sid, err := NewSID(rand.Reader)
	if err != nil || len(sid) != IDLen || !ValidID(sid) || sid != strings.ToLower(sid) {
		t.Fatalf("NewSID %q %v", sid, err)
	}
	if c, ok := CanonicalID(" " + strings.ToUpper(sid) + " "); !ok || c != sid {
		t.Fatalf("CanonicalID %q %v", c, ok)
	}
	if _, err := NewSID(bytes.NewReader([]byte{1, 2})); err == nil {
		t.Fatal("short reader accepted")
	}
	for _, bad := range []string{"", "abc", strings.Repeat("a", 15), strings.Repeat("1", 16), strings.Repeat("a", 17)} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q)", bad)
		}
	}
	pub := make([]byte, 32)
	id := FeederIDFromPub(pub)
	// SHA-256 of 32 zero bytes starts 66 68 7a ad f8 62 bd 77 6c 8f.
	if id != "mzuhvlpymk6xo3ep" || !ValidID(id) {
		t.Fatalf("FeederIDFromPub = %q", id)
	}
	pub[0] = 1
	if FeederIDFromPub(pub) == id {
		t.Fatal("feeder ID ignores the key")
	}
}

func TestClaimCode(t *testing.T) {
	c, err := NewClaimCode(rand.Reader)
	if err != nil || len(c) != ClaimCodeLen {
		t.Fatalf("NewClaimCode %q %v", c, err)
	}
	if n, ok := NormalizeClaimCode(strings.ToLower(c[:4]) + "-" + c[4:]); !ok || n != c {
		t.Fatalf("normalize %q %v", n, ok)
	}
	if n, ok := NormalizeClaimCode("abcd 01e8"); !ok || n != "ABCDOIEB" {
		t.Fatalf("look-alikes: %q %v", n, ok)
	}
	if _, ok := NormalizeClaimCode("ABCD9EFG"); ok {
		t.Fatal("9 accepted")
	}
	if _, ok := NormalizeClaimCode("ABC"); ok {
		t.Fatal("short accepted")
	}
}

func TestNormalizeUUID(t *testing.T) {
	cases := map[string]string{
		"01234567-89AB-CDEF-0123-456789ABCDEF":   "0123456789abcdef0123456789abcdef",
		"{01234567-89ab-cdef-0123-456789abcdef}": "0123456789abcdef0123456789abcdef",
		"0123456789abcdef0123456789ab":           "0123456789abcdef0123456789ab",
		"0123456789abcdef0123456789a":            "",
		"0123456789abcdef0123456789abcdefaa":     "",
		"0123456789abcdef0123456789abcdeg":       "",
	}
	for in, want := range cases {
		got, ok := NormalizeUUID(in)
		if got != want || ok != (want != "") {
			t.Errorf("NormalizeUUID(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}

func TestStationFeedIDAndAntKey(t *testing.T) {
	id := StationFeedID("abcdefghijklmnop", 2)
	if id != "st/abcdefghijklmnop/2" {
		t.Fatal(id)
	}
	if sid, rx, ok := ParseStationFeedID(id); !ok || sid != "abcdefghijklmnop" || rx != 2 {
		t.Fatalf("parse %q %d %v", sid, rx, ok)
	}
	for _, bad := range []string{"radarcape", "st/", "st//1", "st/x", "st/x/-1", "st/x/a"} {
		if _, _, ok := ParseStationFeedID(bad); ok {
			t.Errorf("ParseStationFeedID(%q) accepted", bad)
		}
	}
	if StationAntKey(7, -1) != "s7" || StationAntKey(7, 1) != "s7/1" {
		t.Fatal("StationAntKey")
	}
}

func TestPinsAndBands(t *testing.T) {
	if PinCellDeg(PinRegion) != 0.5 || PinCellDeg(PinCell01) != 0.1 || PinCellDeg(PinHidden) != 0 {
		t.Fatal("PinCellDeg")
	}
	if !ValidPin(PinHidden) || ValidPin("exact") {
		t.Fatal("ValidPin")
	}
	for km, want := range map[float64]string{0: "<50", 49.9: "<50", 50: "50-150", 150: "50-150", 151: ">150"} {
		if got := PeerBandFor(km).String(); got != want {
			t.Errorf("band(%v) = %s, want %s", km, got, want)
		}
	}
}

func TestClockClasses(t *testing.T) {
	for c := ClockClass(0); c < NumClockClasses; c++ {
		p, ok := ParseClockClass(c.String())
		if !ok || p != c {
			t.Errorf("parse(%s) = %v %v", c, p, ok)
		}
		if c.Timed() != (c != ClockNone) {
			t.Errorf("%s Timed", c)
		}
		if c.Timed() && (c.NominalHz() == 0 || c.PriorJitterNs() == 0 || c.PriorFreqSigma() == 0 || c.SyncedSigmaNs() == 0) {
			t.Errorf("%s has no priors", c)
		}
	}
	if ClockClass(9).Valid() || ClockClass(9).String() != "class9" || ClockClass(9).Timed() {
		t.Fatal("undefined class")
	}
	if _, ok := ParseClockClass("warp"); ok {
		t.Fatal("parse unknown")
	}
	if !ClockGPSUTC.AnchorCapable() || !ClockGPSGPST.AnchorCapable() || ClockGPSEmul.AnchorCapable() || ClockHW12.AnchorCapable() {
		t.Fatal("AnchorCapable")
	}
	if ClockGPSEmul.NominalHz() != 1e9 || ClockHW12.NominalHz() != 12e6 || ClockSBS20.NominalHz() != 20e6 {
		t.Fatal("NominalHz")
	}
	// Wire codes are frozen.
	if ClockSDR12 != 5 || ClockSBS20 != 7 {
		t.Fatal("clock class codes renumbered")
	}
	for ty, want := range map[string]ClockClass{
		"radarcape_gps": ClockGPSUTC, "beast": ClockHW12, "radarcape_12mhz": ClockHW12,
		"dump1090": ClockSDR12, "unknown": ClockSDR12, "auto": ClockSDR12, "sbs": ClockSBS20,
	} {
		if got := ClockFromMLATClientType(ty); got != want {
			t.Errorf("clock_type %s -> %s, want %s", ty, got, want)
		}
	}
	if !ClockEvJump.Valid() || ClockEventReason("x").Valid() {
		t.Fatal("ClockEventReason.Valid")
	}
}

func TestGPSTicks(t *testing.T) {
	day := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		host time.Time
		nsOD int64
		want time.Time
	}{
		{day.Add(12 * time.Hour), int64(12*time.Hour + 5), day.Add(12*time.Hour + 5)},
		// Host just after midnight, stamp just before: previous day.
		{day.Add(2 * time.Second), int64(24*time.Hour - time.Second), day.Add(-time.Second)},
		// Host just before midnight, stamp just after: next day.
		{day.Add(-time.Second), int64(500 * time.Millisecond), day.Add(500 * time.Millisecond)},
	}
	for _, c := range cases {
		got, ok := GPSTicks(c.nsOD, c.host.UnixNano())
		if !ok || int64(got) != c.want.UnixNano() {
			t.Errorf("GPSTicks(%d, %v) = %d, want %v", c.nsOD, c.host, got, c.want)
		}
	}
	if _, ok := GPSTicks(NsPerDay, day.UnixNano()); ok {
		t.Fatal("out-of-range ns accepted")
	}
	if _, ok := GPSTicks(-1, day.UnixNano()); ok {
		t.Fatal("negative ns accepted")
	}
}

func TestUnwrap(t *testing.T) {
	const m48 = uint64(1) << 48
	if got := Unwrap48(m48-10, 5); got != m48+5 {
		t.Fatalf("forward wrap: %d", got)
	}
	if got := Unwrap48(m48+5, m48-10); got != m48-10 {
		t.Fatalf("late frame before wrap: %d", got)
	}
	if got := Unwrap48(3*m48+100, 200); got != 3*m48+200 {
		t.Fatalf("no wrap: %d", got)
	}
	if got := Unwrap48(5, m48-3); got != m48-3 {
		// Cannot go below 0, so a "backwards" value near the top stays there.
		t.Fatalf("near zero: %d", got)
	}
	var acc uint64
	raw := uint64(0)
	for i := 0; i < 100; i++ {
		raw = (raw + 6_000_000) & (1<<24 - 1) // 0.3 s at 20 MHz
		acc = Unwrap24(acc, raw)
	}
	if acc != 100*6_000_000 {
		t.Fatalf("Unwrap24 accumulated %d", acc)
	}
}

func TestScope(t *testing.T) {
	if !ScopeAdmin.Allows(ScopeOwner) || !ScopeOwner.Allows(ScopePublic) || ScopePublic.Allows(ScopeOwner) || ScopeOwner.Allows(ScopeAdmin) {
		t.Fatal("Allows ordering")
	}
	if Scope(9).Allows(ScopeOwner) || !Scope(9).Allows(ScopePublic) {
		t.Fatal("undefined scope must be public at most")
	}
	for _, s := range []Scope{ScopePublic, ScopeOwner, ScopeAdmin} {
		if p, ok := ParseScope(s.String()); !ok || p != s {
			t.Errorf("ParseScope(%s)", s)
		}
	}
	if _, ok := ParseScope("root"); ok {
		t.Fatal("ParseScope root")
	}
	a := Viewer{Scope: ScopeOwner, OwnerID: 1, SID: "x"}
	b := Viewer{Scope: ScopeOwner, OwnerID: 2, SID: "x"}
	if a.CacheKey() == b.CacheKey() || PublicViewer.CacheKey() == a.CacheKey() {
		t.Fatal("cache keys collide")
	}
}

func TestMLATGrades(t *testing.T) {
	cases := []struct {
		s    float64
		dof  int
		want string
	}{
		{20, 2, "A"}, {20, 1, "B"}, {150, 3, "B"}, {400, 3, "C"}, {1999, 3, "D"}, {2500, 3, ""}, {math.NaN(), 3, ""},
	}
	for _, c := range cases {
		if got := GradeString(GradeFor(c.s, c.dof)); got != c.want {
			t.Errorf("GradeFor(%v,%d) = %q, want %q", c.s, c.dof, got, c.want)
		}
	}
	if !Displayable(GradeC) || Displayable(GradeD) || Displayable(GradeNone) {
		t.Fatal("Displayable")
	}
	for n, want := range map[int]uint8{2: NBucket3, 3: NBucket3, 4: NBucket4, 5: NBucket5to8, 8: NBucket5to8, 9: NBucket9Plus, 40: NBucket9Plus} {
		if NBucketFor(n) != want {
			t.Errorf("NBucketFor(%d)", n)
		}
	}
}
