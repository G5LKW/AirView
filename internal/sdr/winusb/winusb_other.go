//go:build !windows

package winusb

import "time"

// Available reports whether winusb.dll can be loaded (never, here).
func Available() bool { return false }

// Enumerate is not supported on this platform.
func Enumerate(vid, pid uint16) ([]DeviceInfo, error) { return nil, ErrUnsupported }

// Device is an open WinUSB device (never, here).
type Device struct{}

// Open is not supported on this platform.
func Open(vid, pid uint16) (*Device, error) { return nil, ErrUnsupported }

// OpenPath is not supported on this platform.
func OpenPath(path string) (*Device, error) { return nil, ErrUnsupported }

// Control is not supported on this platform.
func (d *Device) Control(setup SetupPacket, data []byte, timeout time.Duration) (int, error) {
	return 0, ErrUnsupported
}

// ReadPipe is not supported on this platform.
func (d *Device) ReadPipe(pipe byte, buf []byte) (int, error) { return 0, ErrUnsupported }

// WritePipe is not supported on this platform.
func (d *Device) WritePipe(pipe byte, buf []byte) (int, error) { return 0, ErrUnsupported }

// Close does nothing.
func (d *Device) Close() error { return nil }
