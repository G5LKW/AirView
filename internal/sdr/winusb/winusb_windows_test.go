//go:build windows

package winusb

import (
	"errors"
	"testing"
	"unsafe"
)

func TestSetupPacketLayout(t *testing.T) {
	a := setupArgs(SetupPacket{RequestType: 0xC0, Request: 0x01, Value: 0x1234, Index: 0x5678, Length: 0x9ABC})
	// little-endian WINUSB_SETUP_PACKET bytes: C0 01 34 12 78 56 BC 9A
	const want = uint64(0x9ABC5678123401C0)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		if len(a) != 1 || uint64(a[0]) != want {
			t.Fatalf("%#x", a)
		}
	} else if len(a) != 2 || uint64(a[0])|uint64(a[1])<<32 != want {
		t.Fatalf("%#x", a)
	}
}

// TestEnumerate runs the real discovery (read-only). It cannot assume any
// device is attached; it checks that the calls work and that a search for
// an RTL-SDR either finds one with a consistent record or reports
// ErrNotFound from Open.
func TestEnumerate(t *testing.T) {
	if !Available() {
		t.Skip("winusb.dll not available")
	}
	all, err := Enumerate(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d USB devnodes present", len(all))
	for _, d := range all {
		if d.VID == 0 && d.PID == 0 {
			t.Errorf("record without IDs: %+v", d)
		}
		if d.WinUSBReady() {
			t.Logf("WinUSB device: %s", d)
		}
	}
	for _, id := range RTLSDRIDs {
		devs, err := Enumerate(id[0], id[1])
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range devs {
			t.Logf("RTL-SDR present: %s ready=%v", d, d.WinUSBReady())
		}
		if len(devs) == 0 {
			if _, err := Open(id[0], id[1]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Open without device: %v", err)
			}
		}
	}
}
