package modes

import "testing"

const nm = 1852.0

func TestNICRcVersion2(t *testing.T) {
	// The 1090 MHz Riddle, uncertainty chapter, "NIC parameters and their
	// values (version 2)" (DO-260B tables 2-69/2-70).
	for _, c := range []struct {
		tc         uint8
		a, b, c    bool
		nic        int
		rc         float64
		commentary string
	}{
		{5, false, false, false, 11, 7.5, "surface"},
		{6, false, false, false, 10, 25, "surface"},
		{7, true, false, false, 9, 75, "surface"},
		{7, false, false, false, 8, 0.1 * nm, "surface"},
		{8, true, false, true, 7, 0.2 * nm, "surface"},
		{8, true, false, false, 6, 0.3 * nm, "surface"},
		{8, false, false, true, 6, 0.6 * nm, "surface"},
		{8, false, false, false, 0, 0, "surface, unknown"},
		{9, false, false, false, 11, 7.5, ""},
		{10, false, false, false, 10, 25, ""},
		{11, true, true, false, 9, 75, ""},
		{11, false, false, false, 8, 0.1 * nm, ""},
		{12, false, false, false, 7, 0.2 * nm, ""},
		{13, false, true, false, 6, 0.3 * nm, ""},
		{13, false, false, false, 6, 0.5 * nm, ""},
		{13, true, true, false, 6, 0.6 * nm, ""},
		{14, false, false, false, 5, 1 * nm, ""},
		{15, false, false, false, 4, 2 * nm, ""},
		{16, true, true, false, 3, 4 * nm, ""},
		{16, false, false, false, 2, 8 * nm, ""},
		{17, false, false, false, 1, 20 * nm, ""},
		{18, false, false, false, 0, 0, "unknown"},
		{20, false, false, false, 11, 7.5, "GNSS height"},
		{21, false, false, false, 10, 25, "GNSS height"},
		{22, false, false, false, 0, 0, "GNSS height, unknown"},
		{0, false, false, false, 0, 0, "no position"},
	} {
		nic, rc, ok := NICRc(c.tc, 2, c.a, c.b, c.c)
		if !ok || nic != c.nic || !near(rc, c.rc, 0.01) {
			t.Errorf("v2 TC%d A=%v B=%v C=%v: NIC %d Rc %v ok=%v; want NIC %d Rc %v", c.tc, c.a, c.b, c.c, nic, rc, ok, c.nic, c.rc)
		}
	}
	// Combinations the tables do not assign fall back to the lower integrity.
	if nic, rc, _ := NICRc(11, 2, true, false, false); nic != 8 || !near(rc, 0.1*nm, 0.01) {
		t.Errorf("v2 TC11 A=1 B=0: NIC %d Rc %v", nic, rc)
	}
	if nic, rc, _ := NICRc(16, 2, false, true, false); nic != 2 || !near(rc, 8*nm, 0.01) {
		t.Errorf("v2 TC16 A=0 B=1: NIC %d Rc %v", nic, rc)
	}
	if nic, rc, _ := NICRc(13, 2, true, false, false); nic != 6 || !near(rc, 0.6*nm, 0.01) {
		t.Errorf("v2 TC13 A=1 B=0: NIC %d Rc %v", nic, rc)
	}
}

func TestNICRcVersion1(t *testing.T) {
	// "NIC parameters and their values (version 1)": a single supplement bit.
	for _, c := range []struct {
		tc   uint8
		supp bool
		nic  int
		rc   float64
	}{
		{5, false, 11, 7.5}, {6, false, 10, 25}, {7, true, 9, 75}, {7, false, 8, 0.1 * nm}, {8, false, 0, 0},
		{9, false, 11, 7.5}, {10, false, 10, 25}, {11, true, 9, 75}, {11, false, 8, 0.1 * nm},
		{12, false, 7, 0.2 * nm}, {13, false, 6, 0.5 * nm}, {13, true, 6, 0.6 * nm}, {14, false, 5, 1 * nm},
		{15, false, 4, 2 * nm}, {16, true, 3, 4 * nm}, {16, false, 2, 8 * nm}, {17, false, 1, 20 * nm},
		{18, false, 0, 0}, {20, false, 11, 7.5}, {21, false, 10, 25}, {22, false, 0, 0},
	} {
		// Supplements B and C do not exist in version 1 and must be ignored.
		for _, bc := range []bool{false, true} {
			nic, rc, ok := NICRc(c.tc, 1, c.supp, bc, bc)
			if !ok || nic != c.nic || !near(rc, c.rc, 0.01) {
				t.Errorf("v1 TC%d supp=%v (B=C=%v): NIC %d Rc %v; want NIC %d Rc %v", c.tc, c.supp, bc, nic, rc, c.nic, c.rc)
			}
		}
	}
}

func TestNICRcVersion0(t *testing.T) {
	// Version 0 reports NUCp; Rc is the horizontal protection limit of the
	// "NUCp parameters and their values" table and no supplement bit counts.
	for _, c := range []struct {
		tc   uint8
		nucp int
		rc   float64
	}{
		{0, 0, 0}, {5, 9, 7.5}, {6, 8, 25}, {7, 7, 0.1 * nm}, {8, 6, 0},
		{9, 9, 7.5}, {10, 8, 25}, {11, 7, 0.1 * nm}, {12, 6, 0.2 * nm}, {13, 5, 0.5 * nm}, {14, 4, 1 * nm},
		{15, 3, 2 * nm}, {16, 2, 10 * nm}, {17, 1, 20 * nm}, {18, 0, 0},
		{20, 9, 7.5}, {21, 8, 25}, {22, 0, 0},
	} {
		for _, supp := range []bool{false, true} {
			_, rc, ok := NICRc(c.tc, 0, supp, supp, supp)
			if !ok || !near(rc, c.rc, 0.01) {
				t.Errorf("v0 TC%d (supp=%v): Rc %v ok=%v; want %v", c.tc, supp, rc, ok, c.rc)
			}
		}
		if got, ok := NUCp(c.tc); !ok || got != c.nucp {
			t.Errorf("NUCp(TC%d) = %d, %v; want %d", c.tc, got, ok, c.nucp)
		}
	}
	// The version 0 radius is never smaller than that of later versions with
	// cleared supplements, so it is a safe default for an unknown version.
	for tc := uint8(0); tc < 32; tc++ {
		_, rc0, ok0 := NICRc(tc, 0, false, false, false)
		for v := 1; v <= 2; v++ {
			_, rc, ok := NICRc(tc, v, false, false, false)
			if ok != ok0 {
				t.Errorf("TC%d: ok differs between versions", tc)
			}
			if rc0 != 0 && rc > rc0 {
				t.Errorf("TC%d: v%d radius %v exceeds v0 radius %v", tc, v, rc, rc0)
			}
		}
	}
}

func TestNICRcNonPositionTypes(t *testing.T) {
	for _, tc := range []uint8{1, 2, 3, 4, 19, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 200} {
		for v := 0; v <= 2; v++ {
			if nic, rc, ok := NICRc(tc, v, true, true, true); ok || nic != 0 || rc != 0 {
				t.Errorf("TC%d v%d: NIC %d Rc %v ok=%v", tc, v, nic, rc, ok)
			}
		}
		if _, ok := NUCp(tc); ok {
			t.Errorf("NUCp(TC%d) ok", tc)
		}
	}
}

func TestMessageNICRc(t *testing.T) {
	// Self-generated positions: type codes 18 down to 9 map to NIC 0..11.
	// The NIC supplement B bit (ME bit 8) separates NIC 2/3 (TC16) and 8/9
	// (TC11); the expected values assume the aircraft announced NIC
	// supplement A whenever the message itself carries supplement B.
	for _, c := range []struct {
		tc   uint8
		nicB bool
		nic  int
	}{
		{18, false, 0}, {17, false, 1}, {16, false, 2}, {16, true, 3}, {14, false, 5}, {13, false, 6},
		{12, false, 7}, {11, false, 8}, {11, true, 9}, {10, false, 10}, {9, false, 11},
	} {
		frame := esHex(0x8D, 0x44A1B2, airbornePosME(c.tc, 0, c.nicB, ac12For(31000), false, false, 70000, 20000))
		m := mustDecode(t, frame)
		nicA := m.NICSuppB
		nic, rc, ok := m.NICRc(2, nicA, false)
		if !ok || nic != c.nic {
			t.Errorf("%s (TC%d, B=%v): NIC %d, want %d", frame, m.TC, m.NICSuppB, nic, c.nic)
		}
		want, wantRc, _ := NICRc(m.TC, 2, nicA, m.NICSuppB, false)
		if nic != want || rc != wantRc {
			t.Errorf("%s: method and function disagree", frame)
		}
	}
	// Surface position with NIC supplement C; non-position message.
	s := mustDecode(t, bookSurf) // TC7
	if nic, rc, ok := s.NICRc(2, true, false); !ok || nic != 9 || rc != 75 {
		t.Errorf("surface TC7 A=1: NIC %d Rc %v ok=%v", nic, rc, ok)
	}
	v := mustDecode(t, bookVelGS)
	if _, _, ok := v.NICRc(2, true, true); ok {
		t.Error("velocity message has no NIC")
	}
	var empty Message
	if _, _, ok := empty.NICRc(2, false, false); ok {
		t.Error("empty message has no NIC")
	}
}

func TestAccuracyTables(t *testing.T) {
	// "NACp parameters and their values" / "NACv parameters and their values".
	epu := []float64{0, 10 * nm, 4 * nm, 2 * nm, 1 * nm, 0.5 * nm, 0.3 * nm, 0.1 * nm, 0.05 * nm, 30, 10, 3}
	for nacp, want := range epu {
		if got := NACpEPU(uint8(nacp)); !near(got, want, 0.01) {
			t.Errorf("NACpEPU(%d) = %v, want %v", nacp, got, want)
		}
	}
	for _, nacp := range []uint8{12, 15, 255} {
		if got := NACpEPU(nacp); got != 0 {
			t.Errorf("NACpEPU(%d) = %v, want 0", nacp, got)
		}
	}
	for nacv, want := range []float64{0, 10, 3, 1, 0.3, 0, 0, 0} {
		if got := NACvHFOM(uint8(nacv)); got != want {
			t.Errorf("NACvHFOM(%d) = %v, want %v", nacv, got, want)
		}
	}
}
