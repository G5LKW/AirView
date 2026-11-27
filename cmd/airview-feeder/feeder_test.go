package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/avf"
	"airview/internal/feed"
	"airview/internal/feederui"
	"airview/internal/model"
	"airview/internal/uplink"
	"airview/internal/uplink/avftest"
)

// beastSource is a fake readsb Beast output: it streams DF17 frames with a
// running 12 MHz counter to every client.
func beastSource(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() { close(done); ln.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				start := time.Now()
				tk := time.NewTicker(5 * time.Millisecond)
				defer tk.Stop()
				icaos := []uint32{0x4ca123, 0x3c6544, 0x406b90}
				for i := 0; ; i++ {
					select {
					case <-done:
						return
					case <-tk.C:
					}
					ts := uint64(time.Since(start).Seconds() * 12e6)
					me := uint64(11)<<51 | uint64(i&0x1FFFF)
					b := appendBeastLong(nil, ts, 120, esFrame(icaos[i%3], me))
					if _, err := c.Write(b); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestFeederEndToEnd(t *testing.T) {
	srv, err := avftest.New(avftest.Options{Name: "testnet"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	src := beastSource(t)

	dir := t.TempDir()
	cf := filepath.Join(dir, "feeder.json")
	uiPort, resPort := freePort(t), freePort(t)
	cfg := model.DefaultFeederConfig()
	cfg.Receivers[0].Name = "roof"
	cfg.Receivers[0].Input.Host, cfg.Receivers[0].Input.Port = "127.0.0.1", src // auto on this port
	cfg.Receivers[0].Located, cfg.Receivers[0].Lat, cfg.Receivers[0].Lon, cfg.Receivers[0].AltM = true, 51.5, -0.1, 30
	cfg.Uplinks = []model.Uplink{srv.Uplink("test")}
	cfg.UI.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(uiPort))
	cfg.Results.BeastPort = resPort
	if err := saveConfig(cf, cfg); err != nil {
		t.Fatal(err)
	}

	f, err := startFeeder(runOptions{ConfigFile: cf, DataDir: dir, Logf: t.Logf, DetectEvery: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			f.stop()
		}
	}()

	// connects, announces the receiver, forwards frames
	waitFor(t, 15*time.Second, func() bool {
		st := f.um.Status()
		return len(st) == 1 && st[0].State == uplink.StateConnected && srv.FrameCount() > 50
	})
	hs := srv.Hellos()
	if len(hs) == 0 || hs[0].Software != avf.SoftwareFeeder {
		t.Fatalf("hello %+v", hs)
	}
	for _, e := range srv.Errors() {
		t.Errorf("protocol violation: %v", e)
	}

	// status page
	base := "http://" + cfg.UI.Addr + "/"
	var snap feederui.Snapshot
	if err := localCall(context.Background(), "GET", base+"api/status", nil, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Receivers) != 1 || snap.Receivers[0].Detected == "" || !snap.Receivers[0].Connected || snap.Receivers[0].Stats.Frames == 0 {
		t.Fatalf("receiver status %+v", snap.Receivers)
	}
	if snap.FeederID == "" || len(snap.Uplinks) != 1 || snap.Uplinks[0].ServerName == "" && snap.Uplinks[0].State != uplink.StateConnected {
		t.Fatalf("snapshot %+v", snap)
	}
	resp, err := http.Get(base)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("page: %v", err)
	}
	resp.Body.Close()

	// CLI: status and claim talk to the running feeder
	var out, errb bytes.Buffer
	if code := runMain([]string{"status", "-config", cf, "-data", dir}, &out, &errb); code != 0 || !strings.Contains(out.String(), "connected") {
		t.Fatalf("status: %d %s %s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := runMain([]string{"claim", "-config", cf, "-data", dir}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Claim code: ") {
		t.Fatalf("claim: %d %s %s", code, out.String(), errb.String())
	}
	if srv.Claims() != 1 {
		t.Fatalf("claims at server: %d", srv.Claims())
	}

	// MLAT results come back out on the local Beast port with the magic timestamp
	rc, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(resPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	waitFor(t, 5*time.Second, func() bool { return f.results.status().Clients == 1 })
	srv.SendResults([]model.MLATFix{fixAt(0x4ca123, 51.6, -0.2, 9000, nil)})
	_ = rc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1024)
	n, err := rc.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if fr := parseBeast(buf[:n]); len(fr) == 0 || !feed.IsMLATTimestamp(fr[0].Timestamp) {
		t.Fatalf("results frames %d", len(fr))
	}

	// settings from the page are saved and applied
	var doc feederui.SettingsDoc
	if err := localCall(context.Background(), "GET", base+"api/settings", nil, &doc); err != nil {
		t.Fatal(err)
	}
	doc.Config.Receivers[0].Name = "attic"
	var res map[string]any
	if err := localCall(context.Background(), "PUT", base+"api/settings", doc.Config, &res); err != nil {
		t.Fatal(err)
	}
	lc, _ := loadConfig(loadOptions{File: cf})
	if lc.FileCfg.Receivers[0].Name != "attic" || lc.FileCfg.Uplinks[0].ServerPin != srv.Pin {
		t.Fatalf("saved %+v", lc.FileCfg)
	}

	// a stopped feeder: claim connects with the feeder identity itself
	f.stop()
	stopped = true
	out.Reset()
	errb.Reset()
	if code := runMain([]string{"claim", "-config", cf, "-data", dir, "-timeout", "20s"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Claim code: ") {
		t.Fatalf("offline claim: %d %s %s", code, out.String(), errb.String())
	}
	if code := runMain([]string{"status", "-config", cf, "-data", dir}, &out, &errb); code != 3 {
		t.Fatalf("status of a stopped feeder: %d", code)
	}
	// same identity both times
	if hs := srv.Hellos(); !bytes.Equal(hs[len(hs)-1].Pub, hs[0].Pub) {
		t.Fatal("offline claim used another identity")
	}
}

func TestUninstallHelperAndIdentity(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "airview-feeder")
	cf := filepath.Join(data, "feeder.json")
	var out, errb bytes.Buffer
	if code := runMain([]string{"setup", "-config", cf, "-data", data, "-non-interactive", "-name", "x"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	id1, err := uplink.LoadOrCreateIdentity(data)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := runMain([]string{"new-identity", "-data", data, "-yes"}, &out, &errb); code != 0 {
		t.Fatalf("new-identity %s", errb.String())
	}
	id2, _ := uplink.LoadOrCreateIdentity(data)
	if id1.FeederID == id2.FeederID || id1.UUID != id2.UUID {
		t.Fatal("identity not replaced or UUID changed")
	}
	out.Reset()
	if code := runMain([]string{"uninstall-helper", "-config", cf, "-data", data, "-json"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	var info uninstallInfo
	if err := json.Unmarshal(out.Bytes(), &info); err != nil || len(info.Files) < 3 || info.Running {
		t.Fatalf("info %s", out.String())
	}
	if code := runMain([]string{"uninstall-helper", "-config", cf, "-data", data, "-purge"}, &out, &errb); code != 2 {
		t.Fatal("purge without -yes")
	}
	if code := runMain([]string{"uninstall-helper", "-config", cf, "-data", data, "-purge", "-yes"}, &out, &errb); code != 0 {
		t.Fatalf("purge %s", errb.String())
	}
	if _, err := loadConfig(loadOptions{File: cf}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "*")); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestMainHelpAndUnknown(t *testing.T) {
	var out, errb bytes.Buffer
	if runMain([]string{"help"}, &out, &errb) != 0 || !strings.Contains(out.String(), "claim") {
		t.Fatal("help")
	}
	if runMain([]string{"--version"}, &out, &errb) != 0 || !strings.Contains(out.String(), "airview-feeder") {
		t.Fatal("version")
	}
	if runMain([]string{"frobnicate"}, &out, &errb) != 2 {
		t.Fatal("unknown command")
	}
	if runMain([]string{"update"}, &out, &errb) != 3 {
		t.Fatal("update")
	}
}
