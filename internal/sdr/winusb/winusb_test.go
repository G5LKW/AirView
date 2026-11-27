package winusb

import "testing"

var _ Transport = (*Device)(nil)

func TestParseHardwareID(t *testing.T) {
	for _, tc := range []struct {
		id       string
		vid, pid uint16
		mi       int
		ok       bool
	}{
		{`USB\VID_0BDA&PID_2838\00000001`, 0x0bda, 0x2838, -1, true},
		{`USB\VID_0BDA&PID_2838&MI_00\6&2b4b5c5&0&0000`, 0x0bda, 0x2838, 0, true},
		{`usb\vid_1d50&pid_60a1&mi_01\x`, 0x1d50, 0x60a1, 1, true},
		{`USB\ROOT_HUB30\4&1`, 0, 0, -1, false},
		{`USB\VID_ZZZZ&PID_0001`, 0, 0, -1, false},
	} {
		v, p, mi, ok := parseHardwareID(tc.id)
		if ok != tc.ok || (ok && (v != tc.vid || p != tc.pid || mi != tc.mi)) {
			t.Errorf("%s: %04x %04x %d %v", tc.id, v, p, mi, ok)
		}
	}
	d := DeviceInfo{VID: 0x0bda, PID: 0x2838, Interface: 0, Driver: "WinUSB", Path: `\\?\usb#x`}
	if !d.IsRTLSDR() || !d.WinUSBReady() || d.String() == "" {
		t.Fatal(d)
	}
	if (DeviceInfo{VID: 0x0bda, PID: 0x2838, Driver: "RTL2832UBDA"}).WinUSBReady() {
		t.Fatal("DVB-T driver reported ready")
	}
}
