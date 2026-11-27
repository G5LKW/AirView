package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/sdr"
	"airview/internal/sdr/winusb"
	"airview/internal/uplink"
	"airview/internal/uplink/avftest"
)

// TestSetupSDRAndBeastOut: the setup flags of the phase 4-5 features write
// ordinary feeder.json settings, and refuse what does not fit.
func TestSetupSDRAndBeastOut(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "feeder.json")
	var out, errb bytes.Buffer
	run := func(args ...string) int {
		out.Reset()
		errb.Reset()
		return runMain(append([]string{"setup", "-config", cf, "-data", dir, "-non-interactive"}, args...), &out, &errb)
	}
	if code := run("-server", "agg.example.org", "-name", "dongle", "-input", "127.0.0.1:1234", "-input-type", "rtltcp",
		"-sdr-gain", "40.2", "-sdr-ppm", "-12", "-sdr-bias-tee", "-sdr-sample-rate", "2000000",
		"-beast-out", "feed.other.example,10.0.0.5:30005", "-beast-format", "beast_reduce_out", "-beast-uuid", "0f1e2d3c-4b5a-4978-8695-a4b3c2d1e0f9"); code != 0 {
		t.Fatalf("setup: %d %s %s", code, out.String(), errb.String())
	}
	lc, err := loadConfig(loadOptions{File: cf})
	if err != nil {
		t.Fatal(err)
	}
	c := lc.Eff
	in := c.Receivers[0].Input
	if in.Type != model.FeedRTLTCP || in.Host != "127.0.0.1" || in.Port != 1234 || in.SDR.Gain != "40.2" || in.SDR.PPM != -12 || !in.SDR.BiasTee || in.SDR.SampleRate != 2_000_000 {
		t.Fatalf("rtltcp input %+v", in)
	}
	var beast []model.Uplink
	avfs := 0
	for _, u := range c.Uplinks {
		if u.IsBeast() {
			beast = append(beast, u)
		} else {
			avfs++
		}
	}
	if avfs != 1 || len(beast) != 2 || beast[0].Host != "feed.other.example" || beast[0].Port != 30004 || beast[1].Port != 30005 ||
		beast[0].Format != "beast_reduce_out" || beast[0].UUID != "0f1e2d3c-4b5a-4978-8695-a4b3c2d1e0f9" || beast[0].ID == beast[1].ID {
		t.Fatalf("uplinks %+v", c.Uplinks)
	}

	// A re-run with -server changes the AVF entry, never a Beast output.
	if code := run("-server", "agg2.example.org"); code != 0 {
		t.Fatalf("rerun: %d %s", code, errb.String())
	}
	lc, _ = loadConfig(loadOptions{File: cf})
	if u := lc.Eff.Uplinks; len(u) != 3 || u[0].Host != "agg2.example.org" || !u[1].IsBeast() || u[1].Host != "feed.other.example" {
		t.Fatalf("after -server: %+v", u)
	}
	// The same Beast address again updates it instead of adding one.
	if code := run("-beast-out", "feed.other.example", "-no-timestamps"); code != 0 {
		t.Fatalf("beast update: %d %s", code, errb.String())
	}
	lc, _ = loadConfig(loadOptions{File: cf})
	if u := lc.Eff.Uplinks; len(u) != 3 || u[1].Timestamps {
		t.Fatalf("after beast update: %+v", u)
	}
	// off removes every Beast output.
	if code := run("-beast-out", "off"); code != 0 {
		t.Fatalf("beast off: %d %s", code, errb.String())
	}
	lc, _ = loadConfig(loadOptions{File: cf})
	if u := lc.Eff.Uplinks; len(u) != 1 || u[0].IsBeast() {
		t.Fatalf("after off: %+v", u)
	}

	// Refusals: SDR flags need an rtltcp input; bad values; Beast options
	// without an output.
	if code := run("-input", "127.0.0.1:30005", "-input-type", "beast", "-sdr-ppm", "3"); code != 2 || !strings.Contains(errb.String(), "rtltcp") {
		t.Fatalf("sdr flag on a Beast input: %d %s", code, errb.String())
	}
	if code := run("-input", "127.0.0.1:1234", "-input-type", "rtltcp", "-sdr-ppm", "900"); code != 2 || !strings.Contains(errb.String(), "ppm") {
		t.Fatalf("ppm 900: %d %s", code, errb.String())
	}
	if code := run("-beast-format", "beast_out"); code != 2 {
		t.Fatalf("beast format without output: %d", code)
	}
	if code := run("-beast-out", "bad host:x"); code != 2 {
		t.Fatalf("bad beast address: %d", code)
	}
}

// TestDetectSDR: "detect -sdr" reports an rtl_tcp server with its tuner,
// the USB dongles and their driver state.
func TestDetectSDR(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &sdr.FakeRTLTCP{Tuner: sdr.TunerR820T, GainCount: 29, Rate: 2.4e6, Source: func(n int) []byte { return make([]byte, 2*n) }}
	go fake.Serve(ctx, ln)
	port := ln.Addr().(*net.TCPAddr).Port

	oldEnum := enumerateDongles
	defer func() { enumerateDongles = oldEnum }()
	enumerateDongles = func() ([]winusb.DeviceInfo, error) {
		return []winusb.DeviceInfo{
			{VID: 0x0bda, PID: 0x2838, Interface: 0, Driver: "RTL2832UBDA", Desc: "RTL2838UHIDIR"},
			{VID: 0x0bda, PID: 0x2832, Interface: -1, Driver: "WinUSB", Path: `\\?\usb#x`, Desc: "RTL2832U"},
			{VID: 0x0bda, PID: 0x8153, Interface: -1, Driver: "rtux64w10", Desc: "USB LAN"},
		}, nil
	}
	d := detectSDR(context.Background(), "127.0.0.1", port)
	if !d.RTLTCP.Found || d.RTLTCP.Tuner != sdr.TunerR820T.String() || d.RTLTCP.GainSteps != 29 {
		t.Fatalf("rtl_tcp %+v", d.RTLTCP)
	}
	if len(d.Dongles) != 2 || d.Dongles[0].Ready || !d.Dongles[1].Ready {
		t.Fatalf("dongles %+v", d.Dongles)
	}
	var out, errb bytes.Buffer
	if code := runMain([]string{"detect", "-host", "127.0.0.1", "-port", strconv.Itoa(freePort(t)), "-wait", "100ms", "-sdr", "-sdr-port", strconv.Itoa(port)}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "-input-type rtltcp") || !strings.Contains(out.String(), "Zadig") {
		t.Fatalf("detect -sdr: %d\n%s%s", code, out.String(), errb.String())
	}
	// Nothing listening and no USB support: reported, not fatal.
	enumerateDongles = func() ([]winusb.DeviceInfo, error) { return nil, errors.New("unsupported") }
	d = detectSDR(context.Background(), "127.0.0.1", freePort(t))
	if d.RTLTCP.Found || d.RTLTCP.Err == "" || d.USBNote == "" {
		t.Fatalf("empty detection %+v", d)
	}
}

// countingBeastSink counts the Mode S records of every client.
func countingBeastSink(t *testing.T) (int, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 8192)
				esc := false
				for {
					k, err := c.Read(buf)
					for _, b := range buf[:k] {
						if esc {
							esc = false
							if b == '2' || b == '3' {
								n.Add(1)
							}
						} else if b == 0x1a {
							esc = true
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, &n
}

// TestFeederRTLTCPEndToEnd: airview-feeder reads an RTL-SDR through
// rtl_tcp (pure-Go demodulator), feeds an AirView aggregator over AVF/1
// and a third-party aggregator over Beast at the same time.
func TestFeederRTLTCPEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	msg, _ := hex.DecodeString("8D4840D6202CC371C32CE0576098")
	var frames []sdr.SynthFrame
	for i := 0; i < 50; i++ {
		frames = append(frames, sdr.SynthFrame{Data: msg, Start: float64(1000 + i*24000), SNRDB: 25, Phase: 0.3})
	}
	iq := sdr.Synthesize(sdr.SynthConfig{Seed: 3}, 1_200_000, frames)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pos := 0
	fake := &sdr.FakeRTLTCP{Tuner: sdr.TunerR820T, GainCount: 29, Rate: 2.4e6, Source: func(n int) []byte {
		out := make([]byte, 0, 2*n)
		for len(out) < 2*n {
			k := min(2*n-len(out), len(iq)-pos)
			out = append(out, iq[pos:pos+k]...)
			pos = (pos + k) % len(iq)
		}
		return out
	}}
	go fake.Serve(ctx, ln)

	srv, err := avftest.New(avftest.Options{Name: "testnet"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	beastPort, beastFrames := countingBeastSink(t)

	dir := t.TempDir()
	cf := filepath.Join(dir, "feeder.json")
	cfg := model.DefaultFeederConfig()
	cfg.Receivers[0].Name = "dongle"
	cfg.Receivers[0].Input = model.Feed{ID: "input-1", Name: "RTL-SDR", Enabled: true, Type: model.FeedRTLTCP,
		Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, SDR: model.FeedSDR{Gain: "max"}}
	cfg.Receivers[0].Located, cfg.Receivers[0].Lat, cfg.Receivers[0].Lon, cfg.Receivers[0].AltM = true, 51.5, -0.1, 30
	bo := model.DefaultUplink()
	bo.ID, bo.Type, bo.Host, bo.Port, bo.Format = "other", model.UplinkBeast, "127.0.0.1", beastPort, "beast_out"
	cfg.Uplinks = []model.Uplink{srv.Uplink("test"), bo}
	cfg.UI.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
	cfg.Results.BeastPort = 0
	if err := saveConfig(cf, cfg); err != nil {
		t.Fatal(err)
	}
	f, err := startFeeder(runOptions{ConfigFile: cf, DataDir: dir, Logf: t.Logf, DetectEvery: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer f.stop()
	waitFor(t, 30*time.Second, func() bool {
		st := f.um.Status()
		return len(st) == 1 && st[0].State == uplink.StateConnected && srv.FrameCount() > 20 && beastFrames.Load() > 20
	})
	bs := f.um.BeastStatus()
	if len(bs) != 1 || bs[0].State != "connected" || bs[0].FramesSent == 0 {
		t.Fatalf("Beast output %+v", bs)
	}
	for _, e := range srv.Errors() {
		t.Errorf("protocol violation: %v", e)
	}
	hs := srv.Hellos()
	if len(hs) == 0 {
		t.Fatal("no hello at the aggregator")
	}
}
