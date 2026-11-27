//go:build windows

package winusb

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	modwinusb = windows.NewLazySystemDLL("winusb.dll")

	procInitialize            = modwinusb.NewProc("WinUsb_Initialize")
	procFree                  = modwinusb.NewProc("WinUsb_Free")
	procControlTransfer       = modwinusb.NewProc("WinUsb_ControlTransfer")
	procReadPipe              = modwinusb.NewProc("WinUsb_ReadPipe")
	procWritePipe             = modwinusb.NewProc("WinUsb_WritePipe")
	procSetPipePolicy         = modwinusb.NewProc("WinUsb_SetPipePolicy")
	procResetPipe             = modwinusb.NewProc("WinUsb_ResetPipe")
	procAbortPipe             = modwinusb.NewProc("WinUsb_AbortPipe")
	procFlushPipe             = modwinusb.NewProc("WinUsb_FlushPipe")
	procGetDescriptor         = modwinusb.NewProc("WinUsb_GetDescriptor")
	procQueryInterfaceSetting = modwinusb.NewProc("WinUsb_QueryInterfaceSettings")
	procQueryPipe             = modwinusb.NewProc("WinUsb_QueryPipe")
)

// guidUSBDevice is GUID_DEVINTERFACE_USB_DEVICE, registered for every USB
// device devnode.
var guidUSBDevice = windows.GUID{Data1: 0xA5DCBF10, Data2: 0x6530, Data3: 0x11D2,
	Data4: [8]byte{0x90, 0x1F, 0x00, 0xC0, 0x4F, 0xB9, 0x51, 0xED}}

// Available reports whether winusb.dll can be loaded.
func Available() bool { return modwinusb.Load() == nil }

// Enumerate lists the present USB devnodes, optionally filtered by vendor
// and product ID (0 = any). For each it reports the driver and, when the
// driver is WinUSB, the interface path Open needs.
func Enumerate(vid, pid uint16) ([]DeviceInfo, error) {
	set, err := windows.SetupDiGetClassDevsEx(nil, "USB", 0, windows.DIGCF_ALLCLASSES|windows.DIGCF_PRESENT, 0, "")
	if err != nil {
		return nil, fmt.Errorf("winusb: SetupDiGetClassDevsEx: %w", err)
	}
	defer set.Close()
	var out []DeviceInfo
	for i := 0; ; i++ {
		data, err := set.EnumDeviceInfo(i)
		if err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
				break
			}
			continue
		}
		inst, err := set.DeviceInstanceID(data)
		if err != nil {
			continue
		}
		v, p, mi, ok := parseHardwareID(inst)
		if !ok || (vid != 0 && v != vid) || (pid != 0 && p != pid) {
			continue
		}
		d := DeviceInfo{VID: v, PID: p, Interface: mi, InstanceID: inst}
		if s, err := set.DeviceRegistryProperty(data, windows.SPDRP_SERVICE); err == nil {
			d.Driver, _ = s.(string)
		}
		if s, err := set.DeviceRegistryProperty(data, windows.SPDRP_FRIENDLYNAME); err == nil {
			d.Desc, _ = s.(string)
		} else if s, err := set.DeviceRegistryProperty(data, windows.SPDRP_DEVICEDESC); err == nil {
			d.Desc, _ = s.(string)
		}
		if strings.EqualFold(d.Driver, "WinUSB") {
			d.Path = interfacePath(set, data, inst)
		}
		out = append(out, d)
	}
	return out, nil
}

// interfacePath finds the device interface of a WinUSB devnode: the
// GUID(s) its INF wrote to DeviceInterfaceGUIDs / DeviceInterfaceGUID in the
// device's hardware key, else the generic USB device interface (which
// reaches WinUSB when it is the driver of the whole device).
func interfacePath(set windows.DevInfo, data *windows.DevInfoData, inst string) string {
	var guids []windows.GUID
	if h, err := set.OpenDevRegKey(data, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DEV, windows.KEY_READ); err == nil {
		k := registry.Key(h)
		if ss, _, err := k.GetStringsValue("DeviceInterfaceGUIDs"); err == nil {
			for _, s := range ss {
				if g, err := windows.GUIDFromString(s); err == nil {
					guids = append(guids, g)
				}
			}
		}
		if s, _, err := k.GetStringValue("DeviceInterfaceGUID"); err == nil {
			if g, err := windows.GUIDFromString(s); err == nil {
				guids = append(guids, g)
			}
		}
		k.Close()
	}
	guids = append(guids, guidUSBDevice)
	for i := range guids {
		list, err := windows.CM_Get_Device_Interface_List(inst, &guids[i], windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
		if err == nil && len(list) > 0 && list[0] != "" {
			return list[0]
		}
	}
	return ""
}

// Device is an open WinUSB device (its first interface).
type Device struct {
	mu      sync.Mutex
	file    windows.Handle
	iface   uintptr
	timeout time.Duration // current control pipe timeout
	closed  bool
}

// Open opens the first WinUSB-ready device with the vendor/product ID.
func Open(vid, pid uint16) (*Device, error) {
	devs, err := Enumerate(vid, pid)
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		if d.WinUSBReady() {
			return OpenPath(d.Path)
		}
	}
	if len(devs) > 0 {
		return nil, fmt.Errorf("%w: %04x:%04x is present but its driver is %q, not WinUSB (install it with Zadig)", ErrNotFound, vid, pid, devs[0].Driver)
	}
	return nil, ErrNotFound
}

// OpenPath opens a device interface path from Enumerate.
func OpenPath(path string) (*Device, error) {
	if err := modwinusb.Load(); err != nil {
		return nil, fmt.Errorf("winusb: %w", err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("winusb: open %s: %w", path, err)
	}
	var iface uintptr
	r, _, e := procInitialize.Call(uintptr(h), uintptr(unsafe.Pointer(&iface)))
	if r == 0 {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("winusb: WinUsb_Initialize: %w", e)
	}
	return &Device{file: h, iface: iface}, nil
}

func (d *Device) check() error {
	if d == nil || d.closed {
		return errors.New("winusb: device closed")
	}
	return nil
}

// setupArgs passes the 8-byte WINUSB_SETUP_PACKET by value: one register
// on 64-bit targets, two stack words on 32-bit x86.
func setupArgs(s SetupPacket) []uintptr {
	v := uint64(s.RequestType) | uint64(s.Request)<<8 | uint64(s.Value)<<16 | uint64(s.Index)<<32 | uint64(s.Length)<<48
	if unsafe.Sizeof(uintptr(0)) == 8 {
		return []uintptr{uintptr(v)}
	}
	return []uintptr{uintptr(uint32(v)), uintptr(uint32(v >> 32))}
}

// Control performs a control transfer on the default pipe. The direction is
// taken from setup.RequestType; data is the payload (IN: filled).
func (d *Device) Control(setup SetupPacket, data []byte, timeout time.Duration) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(); err != nil {
		return 0, err
	}
	if timeout > 0 && timeout != d.timeout {
		if err := d.setPolicy(0, PolicyTransferTimeout, uint32(timeout/time.Millisecond)); err != nil {
			return 0, err
		}
		d.timeout = timeout
	}
	setup.Length = uint16(len(data))
	var buf uintptr
	if len(data) > 0 {
		buf = uintptr(unsafe.Pointer(&data[0]))
	}
	var n uint32
	args := append([]uintptr{d.iface}, setupArgs(setup)...)
	args = append(args, buf, uintptr(len(data)), uintptr(unsafe.Pointer(&n)), 0)
	r, _, e := syscall.SyscallN(procControlTransfer.Addr(), args...)
	if r == 0 {
		return int(n), fmt.Errorf("winusb: control transfer: %w", e)
	}
	return int(n), nil
}

// ReadPipe reads from an IN pipe (synchronously; set a timeout with
// SetPipeTimeout).
func (d *Device) ReadPipe(pipe byte, buf []byte) (int, error) {
	if err := d.check(); err != nil {
		return 0, err
	}
	if len(buf) == 0 {
		return 0, nil
	}
	var n uint32
	r, _, e := procReadPipe.Call(d.iface, uintptr(pipe), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n)), 0)
	if r == 0 {
		return int(n), fmt.Errorf("winusb: read pipe %#x: %w", pipe, e)
	}
	return int(n), nil
}

// WritePipe writes to an OUT pipe.
func (d *Device) WritePipe(pipe byte, buf []byte) (int, error) {
	if err := d.check(); err != nil {
		return 0, err
	}
	var p uintptr
	if len(buf) > 0 {
		p = uintptr(unsafe.Pointer(&buf[0]))
	}
	var n uint32
	r, _, e := procWritePipe.Call(d.iface, uintptr(pipe), p, uintptr(len(buf)), uintptr(unsafe.Pointer(&n)), 0)
	if r == 0 {
		return int(n), fmt.Errorf("winusb: write pipe %#x: %w", pipe, e)
	}
	return int(n), nil
}

func (d *Device) setPolicy(pipe byte, policy uint32, value uint32) error {
	var r uintptr
	var e error
	if policy == PolicyTransferTimeout {
		v := value
		r, _, e = procSetPipePolicy.Call(d.iface, uintptr(pipe), uintptr(policy), 4, uintptr(unsafe.Pointer(&v)))
	} else {
		v := byte(value)
		r, _, e = procSetPipePolicy.Call(d.iface, uintptr(pipe), uintptr(policy), 1, uintptr(unsafe.Pointer(&v)))
	}
	if r == 0 {
		return fmt.Errorf("winusb: set pipe %#x policy %d: %w", pipe, policy, e)
	}
	return nil
}

// SetPipePolicy sets a boolean policy (Policy* except the timeout).
func (d *Device) SetPipePolicy(pipe byte, policy uint32, on bool) error {
	if err := d.check(); err != nil {
		return err
	}
	v := uint32(0)
	if on {
		v = 1
	}
	return d.setPolicy(pipe, policy, v)
}

// SetPipeTimeout sets a pipe's transfer timeout (0 = none).
func (d *Device) SetPipeTimeout(pipe byte, t time.Duration) error {
	if err := d.check(); err != nil {
		return err
	}
	return d.setPolicy(pipe, PolicyTransferTimeout, uint32(t/time.Millisecond))
}

func (d *Device) pipeOp(p *windows.LazyProc, pipe byte) error {
	if err := d.check(); err != nil {
		return err
	}
	if r, _, e := p.Call(d.iface, uintptr(pipe)); r == 0 {
		return fmt.Errorf("winusb: %s %#x: %w", p.Name, pipe, e)
	}
	return nil
}

// ResetPipe clears a stall and resets the data toggle.
func (d *Device) ResetPipe(pipe byte) error { return d.pipeOp(procResetPipe, pipe) }

// AbortPipe cancels pending transfers on a pipe.
func (d *Device) AbortPipe(pipe byte) error { return d.pipeOp(procAbortPipe, pipe) }

// FlushPipe discards data cached by WinUSB for a pipe.
func (d *Device) FlushPipe(pipe byte) error { return d.pipeOp(procFlushPipe, pipe) }

// Descriptor reads a descriptor (type 1 device, 2 configuration, 3 string).
func (d *Device) Descriptor(typ, index byte, lang uint16, buf []byte) (int, error) {
	if err := d.check(); err != nil {
		return 0, err
	}
	if len(buf) == 0 {
		return 0, nil
	}
	var n uint32
	r, _, e := procGetDescriptor.Call(d.iface, uintptr(typ), uintptr(index), uintptr(lang),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return int(n), fmt.Errorf("winusb: get descriptor %d/%d: %w", typ, index, e)
	}
	return int(n), nil
}

// Pipes lists the endpoints of the interface's alternate setting 0.
func (d *Device) Pipes() ([]PipeInfo, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	var desc [9]byte // USB_INTERFACE_DESCRIPTOR
	if r, _, e := procQueryInterfaceSetting.Call(d.iface, 0, uintptr(unsafe.Pointer(&desc[0]))); r == 0 {
		return nil, fmt.Errorf("winusb: query interface: %w", e)
	}
	n := int(desc[4]) // bNumEndpoints
	out := make([]PipeInfo, 0, n)
	for i := 0; i < n; i++ {
		// WINUSB_PIPE_INFORMATION: USBD_PIPE_TYPE (int32), UCHAR PipeId,
		// USHORT MaximumPacketSize, UCHAR Interval (natural alignment)
		var pi struct {
			Type     int32
			ID       byte
			_        byte
			MaxPkt   uint16
			Interval byte
			_        [3]byte
		}
		if r, _, e := procQueryPipe.Call(d.iface, 0, uintptr(i), uintptr(unsafe.Pointer(&pi))); r == 0 {
			return out, fmt.Errorf("winusb: query pipe %d: %w", i, e)
		}
		out = append(out, PipeInfo{Type: int(pi.Type), ID: pi.ID, MaxPkt: pi.MaxPkt, Interval: pi.Interval})
	}
	return out, nil
}

// Close releases the device.
func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	procFree.Call(d.iface)
	return windows.CloseHandle(d.file)
}
