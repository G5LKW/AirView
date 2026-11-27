package winusb

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupported is returned on platforms without WinUSB.
var ErrUnsupported = errors.New("winusb: not supported on this platform")

// ErrNotFound is returned by Open when no matching device exists.
var ErrNotFound = errors.New("winusb: no matching device")

// DeviceInfo describes a present USB device (or a function of a composite
// device).
type DeviceInfo struct {
	VID, PID   uint16
	Interface  int    // MI_xx of a composite device's function, -1 for the device itself
	InstanceID string // e.g. USB\VID_0BDA&PID_2838\00000001
	Driver     string // the devnode's service (WinUSB, usbccgp, RTL2832UBDA, ...)
	Path       string // device interface path for Open ("" when the driver is not WinUSB)
	Desc       string // device description
}

// WinUSBReady reports whether the device can be opened through WinUSB.
func (d DeviceInfo) WinUSBReady() bool {
	return strings.EqualFold(d.Driver, "WinUSB") && d.Path != ""
}

func (d DeviceInfo) String() string {
	mi := ""
	if d.Interface >= 0 {
		mi = fmt.Sprintf(" MI_%02d", d.Interface)
	}
	return fmt.Sprintf("%04x:%04x%s %q driver=%s", d.VID, d.PID, mi, d.Desc, d.Driver)
}

// Known RTL2832U-based SDR dongles (USB IDs are public facts printed on
// every listing of these devices).
var RTLSDRIDs = [][2]uint16{
	{0x0bda, 0x2838}, // generic RTL2832U "RTL2838UHIDIR"
	{0x0bda, 0x2832}, // generic RTL2832U
}

// IsRTLSDR reports whether the device has one of RTLSDRIDs.
func (d DeviceInfo) IsRTLSDR() bool {
	for _, id := range RTLSDRIDs {
		if d.VID == id[0] && d.PID == id[1] {
			return true
		}
	}
	return false
}

// parseHardwareID extracts VID, PID and MI from an instance or hardware ID
// such as USB\VID_0BDA&PID_2838&MI_00\6&...; ok is false when there is no
// VID/PID.
func parseHardwareID(id string) (vid, pid uint16, mi int, ok bool) {
	mi = -1
	u := strings.ToUpper(id)
	get := func(key string, digits int) (uint64, bool) {
		i := strings.Index(u, key)
		if i < 0 || i+len(key)+digits > len(u) {
			return 0, false
		}
		v, err := strconv.ParseUint(u[i+len(key):i+len(key)+digits], 16, 16)
		return v, err == nil
	}
	v, ok1 := get("VID_", 4)
	p, ok2 := get("PID_", 4)
	if !ok1 || !ok2 {
		return 0, 0, -1, false
	}
	if m, ok := get("MI_", 2); ok {
		mi = int(m)
	}
	return uint16(v), uint16(p), mi, true
}

// SetupPacket is a USB control request (USB 2.0 §9.3).
type SetupPacket struct {
	RequestType byte // bmRequestType: direction | type | recipient
	Request     byte
	Value       uint16
	Index       uint16
	Length      uint16 // filled in by ControlTransfer from the buffer
}

// Request type bits.
const (
	DirOut       = 0x00
	DirIn        = 0x80
	TypeStandard = 0x00
	TypeClass    = 0x20
	TypeVendor   = 0x40
	RecipDevice  = 0x00
	RecipIface   = 0x01
	RecipEndpt   = 0x02
)

// Pipe policies (WinUsb_SetPipePolicy PolicyType).
const (
	PolicyShortPacketTerminate = 0x01
	PolicyAutoClearStall       = 0x02
	PolicyTransferTimeout      = 0x03 // ULONG milliseconds
	PolicyIgnoreShortPackets   = 0x04
	PolicyAllowPartialReads    = 0x05
	PolicyAutoFlush            = 0x06
	PolicyRawIO                = 0x07
	PolicyMaxTransferSize      = 0x08 // read-only
)

// PipeInfo describes an endpoint of the selected interface.
type PipeInfo struct {
	Type     int  // 0 control, 1 isochronous, 2 bulk, 3 interrupt
	ID       byte // endpoint address (bit 7 = IN)
	MaxPkt   uint16
	Interval byte
}

// Transport is what a device driver built on WinUSB needs; *Device
// implements it, and tests can substitute a fake.
type Transport interface {
	Control(setup SetupPacket, data []byte, timeout time.Duration) (int, error)
	ReadPipe(pipe byte, buf []byte) (int, error)
	WritePipe(pipe byte, buf []byte) (int, error)
	Close() error
}
