package main

import (
	"context"
	"net"
	"strconv"
	"time"

	"airview/internal/model"
	"airview/internal/sdr"
	"airview/internal/sdr/winusb"
)

// RTL-SDR detection for "airview-feeder detect -sdr" (WP-WIRE-45). Never
// part of the automatic input detection: an rtl_tcp server streams I/Q to
// whoever connects, and the feeder must not take a dongle another program
// is about to use. The user picks it explicitly (-input-type rtltcp).

// sdrDetection is what "detect -sdr" reports.
type sdrDetection struct {
	// RTLTCP is the rtl_tcp probe at host:port.
	RTLTCP struct {
		Addr      string `json:"addr"`
		Found     bool   `json:"found"`
		Tuner     string `json:"tuner,omitempty"`
		GainSteps uint32 `json:"gainSteps,omitempty"`
		Err       string `json:"error,omitempty"`
	} `json:"rtltcp"`
	// Dongles are the RTL-SDR USB devices on this computer (Windows only:
	// whether the WinUSB driver that rtl_tcp needs is bound).
	Dongles []sdrDongle `json:"dongles,omitempty"`
	// USBNote explains an empty or unavailable USB listing.
	USBNote string `json:"usbNote,omitempty"`
	// Executable is the rtl_tcp found on PATH / next to the program ("" =
	// none; -sdr-spawn needs one).
	Executable string `json:"rtltcpProgram,omitempty"`
}

type sdrDongle struct {
	Device string `json:"device"`
	Driver string `json:"driver"`
	Ready  bool   `json:"winusbReady"`
}

// probeRTLTCP is replaced in tests.
var probeRTLTCP = func(ctx context.Context, addr string) (sdr.DongleInfo, error) {
	c, err := sdr.Dial(ctx, addr)
	if err != nil {
		return sdr.DongleInfo{}, err
	}
	defer c.Close()
	return c.Info(), nil
}

// enumerateDongles is replaced in tests.
var enumerateDongles = func() ([]winusb.DeviceInfo, error) { return winusb.Enumerate(0x0bda, 0) }

func detectSDR(ctx context.Context, host string, port int) sdrDetection {
	if host == "" {
		host = "127.0.0.1"
	}
	if port <= 0 {
		port = model.DefaultRTLTCPPort
	}
	var d sdrDetection
	d.RTLTCP.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	info, err := probeRTLTCP(pctx, d.RTLTCP.Addr)
	cancel()
	if err != nil {
		d.RTLTCP.Err = describeDialErr(err)
	} else {
		d.RTLTCP.Found = true
		d.RTLTCP.Tuner = info.Tuner.String()
		d.RTLTCP.GainSteps = info.GainCount
	}
	devs, err := enumerateDongles()
	switch {
	case err != nil:
		d.USBNote = "USB listing not available on this system (" + err.Error() + ")"
	default:
		for _, x := range devs {
			if !x.IsRTLSDR() {
				continue
			}
			d.Dongles = append(d.Dongles, sdrDongle{Device: x.String(), Driver: x.Driver, Ready: x.WinUSBReady()})
		}
		if len(d.Dongles) == 0 {
			d.USBNote = "no RTL-SDR dongle found on USB"
		}
	}
	d.Executable = sdr.FindRTLTCP()
	return d
}
