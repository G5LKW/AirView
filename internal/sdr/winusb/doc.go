// Package winusb is a minimal, dependency-free binding of the Windows WinUSB
// user-mode USB API (winusb.dll, SetupAPI and the configuration manager
// through golang.org/x/sys/windows), meant as the transport for direct
// RTL-SDR access without rtl_tcp (DESIGN §12.3, phase 5).
//
// # Status
//
// Implemented and usable: device discovery by vendor/product ID (including
// the interface devnodes of composite devices, through the
// DeviceInterfaceGUIDs value that WinUSB INF installers such as Zadig
// write), open/close, control transfers, bulk/interrupt pipe reads and
// writes, pipe policies (timeouts, RAW_IO), pipe reset/abort/flush, device
// and interface descriptors. Other platforms get stubs that return
// ErrUnsupported.
//
// Not implemented, deliberately: the RTL2832U demodulator and the
// R820T/R820T2/R828D/E4000 tuner register programming that turns this
// transport into an I/Q source. The only complete public description of
// those registers is the rtl-sdr library and the Linux DVB drivers, both
// GPL; AirView's clean-room rule (docs/aggregator/CLEANROOM.md) forbids
// deriving code from them. A clean-room driver needs either vendor
// datasheets under terms that allow an implementation, or a written
// register-level specification produced by someone else from black-box USB
// captures of the user's own dongle (the "black-box observation" source of
// CLEANROOM.md §1). That specification would be implemented behind the
// sdr package's I/Q stream interface (an io.Reader of interleaved unsigned
// 8-bit I/Q, exactly what sdr.Receiver reads from rtl_tcp), so the
// demodulator, the receiver and the feeder wiring need no change. See
// WIRING.md in the parent directory for the plan.
//
// Until then RTL-SDR dongles are used through the user's own rtl_tcp
// (sdr.Receiver, sdr.RunChild). Note that rtl_tcp itself needs the WinUSB
// driver bound to the dongle (Zadig), so Enumerate already tells the
// feeder's set-up page whether the driver step has been done: a dongle
// whose Driver is "WinUSB" is ready, one with "RTL2832UBDA" (or similar)
// still has the DVB-T driver.
//
// Sources: Microsoft's WinUSB, SetupAPI and CfgMgr32 documentation (function
// signatures, WINUSB_SETUP_PACKET layout, pipe policy numbers,
// GUID_DEVINTERFACE_USB_DEVICE) and the USB 2.0 specification (standard
// requests and descriptor layouts). No libusb, librtlsdr or other GPL/LGPL
// code was consulted.
package winusb
