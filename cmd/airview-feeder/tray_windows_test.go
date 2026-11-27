package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"

	"airview/internal/feederui"
	"airview/internal/model"
	"airview/internal/uplink"
	"airview/internal/uplink/avftest"
	"airview/internal/wintray"
)

func snapWith(uplinks []uplink.Status, rxConnected bool) *feederui.Snapshot {
	sn := &feederui.Snapshot{Uplinks: uplinks}
	sn.Receivers = []feederui.ReceiverStatus{{RxNo: 1, Connected: rxConnected}}
	sn.Receivers[0].Stats.FPS = 312.4
	sn.Receivers[0].Stats.Aircraft = 45
	return sn
}

func installed(state svc.State) serviceInfo {
	return serviceInfo{Name: "AirViewFeeder", Installed: true, state: state, State: stateName(state)}
}

func TestSummarize(t *testing.T) {
	up := uplink.Status{State: "connected"}
	down := uplink.Status{State: "backoff", LastError: "connection refused"}
	refused := uplink.Status{State: "backoff", LastError: "unknown enrol token", ErrorCode: "AUTH"}
	cases := []struct {
		name  string
		st    trayState
		level wintray.Level
		line  string
	}{
		{"not installed", trayState{}, wintray.LevelIdle, "service not installed"},
		{"scm error", trayState{svc: serviceInfo{Error: "Access is denied."}}, wintray.LevelError, "service: Access is denied."},
		{"stopped", trayState{svc: installed(svc.Stopped)}, wintray.LevelIdle, "service stopped"},
		{"stopped with error", trayState{svc: serviceInfo{Installed: true, state: svc.Stopped, SvcCode: 1}}, wintray.LevelError, "service stopped with an error"},
		{"paused", trayState{svc: installed(svc.Paused)}, wintray.LevelIdle, "paused"},
		{"starting", trayState{svc: installed(svc.StartPending)}, wintray.LevelWarn, "starting"},
		{"page down", trayState{svc: installed(svc.Running), snapErr: "refused"}, wintray.LevelWarn, "running; status page not reachable"},
		{"no uplinks", trayState{svc: installed(svc.Running), snap: snapWith(nil, true)}, wintray.LevelWarn, "no aggregator configured"},
		{"feeding", trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{up}, true)}, wintray.LevelOK, "feeding"},
		{"feeding two", trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{up, up}, true)}, wintray.LevelOK, "feeding 2 aggregators"},
		{"one of two", trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{up, down}, true)}, wintray.LevelWarn, "1 of 2 aggregators connected"},
		{"connecting", trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{down}, true)}, wintray.LevelWarn, "connecting: connection refused"},
		{"refused", trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{refused}, true)}, wintray.LevelError, "refused by the aggregator: unknown enrol token"},
		{"receiver down", trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{up}, false)}, wintray.LevelWarn, "receiver not connected"},
		{"disabled", trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{{Disabled: true}}, true)}, wintray.LevelWarn, "all aggregators are disabled"},
		{"embedded paused", trayState{embedded: true, embPaused: true}, wintray.LevelIdle, "paused"},
		{"embedded failed", trayState{embedded: true, embErr: "another feeder uses D:"}, wintray.LevelError, "not running: another feeder uses D:"},
		{"embedded feeding", trayState{embedded: true, snap: snapWith([]uplink.Status{up}, true)}, wintray.LevelOK, "feeding"},
	}
	for _, c := range cases {
		level, tip, line := summarize(c.st)
		if level != c.level || !strings.HasPrefix(line, c.line) {
			t.Errorf("%s: got %v %q, want %v %q", c.name, level, line, c.level, c.line)
		}
		if !strings.HasPrefix(tip, "AirView Feeder: ") || len([]rune(tip)) > 127 {
			t.Errorf("%s: tooltip %q", c.name, tip)
		}
	}
	_, tip, _ := summarize(trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{up}, true)})
	if !strings.Contains(tip, "312 msg/s, 45 aircraft") {
		t.Errorf("tooltip %q", tip)
	}
	long := uplink.Status{State: "backoff", LastError: strings.Repeat("very long error ", 20)}
	if _, tip, _ := summarize(trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{long}, true)}); len([]rune(tip)) > 127 {
		t.Errorf("tooltip not truncated: %d", len([]rune(tip)))
	}
	if _, _, line := summarize(trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{long}, true)}); len([]rune(line)) > 80 {
		t.Errorf("menu status line not shortened: %q", line)
	}
}

func TestShortError(t *testing.T) {
	for in, want := range map[string]string{
		"": "",
		"dial tcp 127.0.0.1:30160: connectex: No connection could be made because the target machine actively refused it.": "connection refused",
		"read tcp 1.2.3.4:5->6.7.8.9:30160: wsarecv: An existing connection was forcibly closed by the remote host.":       "connection reset",
		"dial tcp: lookup feed.example.org: no such host":                                                                  "unknown host",
		"dial tcp 10.0.0.1:30160: i/o timeout":                                                                             "timed out",
		"tls: failed to verify certificate: x509: certificate signed by unknown authority":                                 "certificate not accepted",
		"handshake: server closed the session":                                                                             "server closed the session",
		strings.Repeat("x", 100):                                                                                           strings.Repeat("x", 59) + "…",
	} {
		if got := shortError(in); got != want {
			t.Errorf("shortError(%q) = %q, want %q", in, got, want)
		}
	}
}

func menuTitles(items []wintray.Item) map[string]wintray.Item {
	m := map[string]wintray.Item{}
	for _, it := range items {
		if !it.Separator {
			m[strings.ReplaceAll(it.Title, "&", "")] = it
		}
	}
	return m
}

func TestBuildTrayMenu(t *testing.T) {
	nop := func() {}
	act := trayActions{nop, nop, nop, nop, nop, nop, nop, nop, nop, nop, nop, nop}
	up := uplink.Status{State: "connected", Host: "feed.example.net"}

	// Service running and feeding.
	m := menuTitles(buildTrayMenu(trayState{svc: installed(svc.Running), snap: snapWith([]uplink.Status{up}, true), url: "http://127.0.0.1:8099/"}, act, false, true))
	for _, want := range []string{"Open status page", "Settings…", "Get claim code…", "Pause feeding", "Restart feeder", "Open logs folder", "Check for updates…", "Quit tray"} {
		it, ok := m[want]
		if !ok || it.Disabled || it.OnClick == nil {
			t.Errorf("running: %q missing or disabled (%v)", want, ok)
		}
	}
	if !m["Open status page"].Default {
		t.Error("Open status page is not the default entry")
	}
	if _, ok := m["Install as Windows service…"]; ok {
		t.Error("install offered with the service installed")
	}
	if it := m["Start with Windows (set by the installer)"]; !it.Checked || !it.Disabled {
		t.Error("machine autostart not shown as set by the installer")
	}
	if it := m["Status: feeding"]; !it.Disabled {
		t.Errorf("status line: %+v", m)
	}

	// Paused service: resume, no claim.
	m = menuTitles(buildTrayMenu(trayState{svc: installed(svc.Paused)}, act, true, false))
	if _, ok := m["Resume feeding"]; !ok {
		t.Error("paused: no Resume")
	}
	if !m["Get claim code…"].Disabled || !m["Open status page"].Disabled {
		t.Error("paused: page actions enabled")
	}
	if it := m["Start with Windows"]; !it.Checked || it.Disabled {
		t.Error("user autostart")
	}

	// Stopped service: start, no restart.
	m = menuTitles(buildTrayMenu(trayState{svc: installed(svc.Stopped)}, act, false, false))
	if _, ok := m["Start feeder"]; !ok {
		t.Error("stopped: no Start")
	}
	if _, ok := m["Restart feeder"]; ok {
		t.Error("stopped: Restart offered")
	}

	// Portable: the tray hosts the feeder.
	m = menuTitles(buildTrayMenu(trayState{embedded: true, snap: snapWith(nil, true)}, act, false, false))
	for _, want := range []string{"Pause feeding", "Install as Windows service…", "Quit (stops feeding)"} {
		if _, ok := m[want]; !ok {
			t.Errorf("embedded: %q missing", want)
		}
	}
	if !m["Get claim code…"].Disabled {
		t.Error("claim enabled without an aggregator")
	}
}

func TestClaimMessageAndCommand(t *testing.T) {
	msg := claimMessage(feederui.ClaimResponse{Code: "ABCD-1234", ExpiresMs: time.Date(2026, 11, 14, 12, 30, 0, 0, time.Local).UnixMilli()}, "feed.example.net", " (copied)")
	for _, want := range []string{"Claim code: ABCD-1234 (copied)", "2026-11-14 12:30", "https://feed.example.net/claim", "Do not share it"} {
		if !strings.Contains(msg, want) {
			t.Errorf("claim message lacks %q:\n%s", want, msg)
		}
	}
	cmd := trayCommand(trayOptions{name: "Other", c: common{data: `C:\Feeder Data`}})
	args := splitCommandLine(cmd)
	if len(args) < 6 || args[1] != "tray" || args[3] != "Other" || args[5] != `C:\Feeder Data` {
		t.Fatalf("trayCommand %q -> %q", cmd, args)
	}
}

func TestTrayStatusURLDiscovery(t *testing.T) {
	dir := t.TempDir()
	a := newTrayApp(trayOptions{name: "x"})
	c := common{config: filepath.Join(dir, "feeder.json"), data: dir}
	logs := logSettings{Dir: filepath.Join(dir, "logs")}
	// Nothing known: the default page.
	if u := a.statusURL(trayState{}, c, logs); u != "http://127.0.0.1:8099/" {
		t.Fatalf("default %q", u)
	}
	// The configuration's ui.addr.
	cfg := model.DefaultFeederConfig()
	cfg.UI.Addr = "127.0.0.1:18799"
	if err := saveConfig(c.config, cfg); err != nil {
		t.Fatal(err)
	}
	if u := a.statusURL(trayState{}, c, logs); u != "http://127.0.0.1:18799/" {
		t.Fatalf("config %q", u)
	}
	// What the service recorded wins.
	os.MkdirAll(logs.Dir, 0o755)
	writeStatusURL(logs.Dir, "http://127.0.0.1:18800/")
	if u := a.statusURL(trayState{}, c, logs); u != "http://127.0.0.1:18800/" {
		t.Fatalf("recorded %q", u)
	}
	// -url wins over everything.
	a.o.url = "http://127.0.0.1:1"
	if u := a.statusURL(trayState{}, c, logs); u != "http://127.0.0.1:1/" {
		t.Fatalf("flag %q", u)
	}
}

// TestTrayHostedFeeder: without a service the tray runs the feeder itself
// (portable mode), reports it, pauses and resumes it, and refuses a second
// feeder on the same data directory.
func TestTrayHostedFeeder(t *testing.T) {
	srv, err := avftest.New(avftest.Options{Name: "testnet"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	src := beastSource(t)
	dir := filepath.Join(t.TempDir(), "airview-feeder")
	cf := filepath.Join(t.TempDir(), "feeder.json")
	cfg := model.DefaultFeederConfig()
	cfg.Receivers[0].Input.Host, cfg.Receivers[0].Input.Port = "127.0.0.1", src
	cfg.Uplinks = []model.Uplink{srv.Uplink("test")}
	cfg.UI.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
	cfg.Results.BeastPort = 0
	if err := saveConfig(cf, cfg); err != nil {
		t.Fatal(err)
	}
	a := newTrayApp(trayOptions{name: "AirViewFeederTrayTest" + strconv.Itoa(os.Getpid()), embedded: "auto", c: common{config: cf, data: dir}})
	defer a.stopEmbedded()

	waitFor(t, 20*time.Second, func() bool {
		a.poll()
		l, _, _ := summarize(a.state())
		return l == wintray.LevelOK
	})
	st := a.state()
	if !st.embedded || st.url != "http://"+cfg.UI.Addr+"/" || st.snap == nil || st.snap.Uplinks[0].State != "connected" {
		t.Fatalf("state %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", logFileName)); err != nil {
		t.Fatalf("tray-hosted feeder log: %v", err)
	}
	// A second feeder (service or console) cannot take the same identity.
	if _, err := lockDataDirWin(dir); err == nil {
		t.Fatal("data directory not locked by the tray-hosted feeder")
	}

	a.control("pause")
	a.poll()
	if l, _, line := summarize(a.state()); l != wintray.LevelIdle || line != "paused" {
		t.Fatalf("after pause: %v %q", l, line)
	}
	if rel, err := lockDataDirWin(dir); err != nil {
		t.Fatalf("lock not released on pause: %v", err)
	} else {
		rel()
	}
	a.control("continue")
	waitFor(t, 20*time.Second, func() bool {
		a.poll()
		l, _, _ := summarize(a.state())
		return l == wintray.LevelOK
	})
}

func TestUpdateMessageAndArgs(t *testing.T) {
	manual := "Download it by hand."
	for _, c := range []struct {
		code      int
		now, want string
		icon      uint32
	}{
		{0, version, "up to date", wintray.MBIconInfo},
		{0, "", "up to date", wintray.MBIconInfo},
		{0, "99.0.0", "updated to 99.0.0", wintray.MBIconInfo},
		{3, version, manual, wintray.MBIconInfo},
		{2, version, "not set up", wintray.MBIconInfo},
		{4, version, "previous version runs again", wintray.MBIconWarning},
		{1, version, "code 1", wintray.MBIconWarning},
	} {
		msg, icon := updateMessage(c.code, c.now, manual)
		if !strings.Contains(msg, c.want) || icon != c.icon {
			t.Errorf("code %d now %q: %q (icon %#x), want %q (icon %#x)", c.code, c.now, msg, icon, c.want, c.icon)
		}
	}
	got := strings.Join(updateArgs("AirViewFeeder", serviceInfo{Config: `C:\P D\feeder.json`, DataDir: `C:\P D`}), "|")
	if want := `update|-service|AirViewFeeder|-config|C:\P D\feeder.json|-data|C:\P D`; got != want {
		t.Errorf("updateArgs = %q, want %q", got, want)
	}
}
