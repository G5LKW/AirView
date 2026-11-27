package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"airview/internal/selfupdate"
)

// TestSelfUpdateE2E is the WP-W3-wire acceptance test of the self-update
// (DESIGN §14 phase 3): real airview binaries in a scratch release.
//
//  1. airview 1.0.0 runs headless (serve) with update.manifestUrl / keys
//     pointing at a local release server that publishes 2.0.0;
//  2. "Install" on the web API (POST /api/update/install): 1.0.0 swaps in
//     2.0.0 and restarts into it; 2.0.0 confirms itself after the health
//     window (journal "committed"), /healthz reports 2.0.0, the program
//     file is the 2.0.0 build;
//  3. a broken 3.0.0 (its trial self check fails: -X main.e2eFault=unhealthy)
//     is published and installed the same way: it starts, fails its health
//     check, the previous binary is restored and restarted (journal
//     "rolledback"), and 2.0.0 runs again.
//
// It builds airview three times and runs for about two minutes, so it only
// runs with AIRVIEW_E2E_UPDATE=1:
//
//	AIRVIEW_E2E_UPDATE=1 go test ./cmd/airview -run TestSelfUpdateE2E -v -timeout 15m
func TestSelfUpdateE2E(t *testing.T) {
	if os.Getenv("AIRVIEW_E2E_UPDATE") != "1" {
		t.Skip("set AIRVIEW_E2E_UPDATE=1 to run the self-update end-to-end test (builds airview three times)")
	}
	tmp := t.TempDir()
	exeName := "airview"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	build := func(ver, fault string) string {
		out := filepath.Join(tmp, "builds", ver, exeName)
		ld := "-X main.version=" + ver
		if fault != "" {
			ld += " -X main.e2eFault=" + fault
		}
		cmd := exec.Command("go", "build", "-o", out, "-ldflags", ld, "airview/cmd/airview")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", ver, err, b)
		}
		return out
	}
	v1, v2, v3 := build("1.0.0", ""), build("2.0.0", ""), build("3.0.0", "unhealthy")

	pk, priv, err := selfupdate.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(tmp, "release")
	publish := func(ver, art string) {
		if _, err := selfupdate.Publish(selfupdate.PublishOptions{Product: "airview", Version: ver, Channel: "stable",
			Artifacts: map[string]string{selfupdate.CurrentTarget(): art}, OutDir: rel, Key: priv}); err != nil {
			t.Fatal(err)
		}
	}
	publish("2.0.0", v2)
	srv := httptest.NewServer(selfupdate.ReleaseHandler{Dir: rel})
	defer srv.Close()

	// The installation: a copy of 1.0.0, its own data directory and port.
	instDir := filepath.Join(tmp, "install")
	data := filepath.Join(tmp, "data")
	_ = os.MkdirAll(instDir, 0o755)
	_ = os.MkdirAll(data, 0o755)
	exe := filepath.Join(instDir, exeName)
	copyFile(t, v1, exe)
	port := freeTCPPort(t)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	logf, err := os.Create(filepath.Join(tmp, "airview.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	cmd := exec.Command(exe, "serve", "-data", data, "-port", strconv.Itoa(port))
	cmd.Env = append(os.Environ(),
		"AIRVIEW_UPDATE_MANIFEST_URL="+srv.URL+"/manifest.json",
		"AIRVIEW_UPDATE_KEYS="+pk.String(),
		"AIRVIEW_UPDATE_HEALTH_WINDOW_S=10",
		"AIRVIEW_NTP_ENABLED=false",
		"AIRVIEW_FEEDS=[]", // no receiver: never connect to a real one on the LAN
		"AIRVIEW_DATA=",
	)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Stop whatever process runs the installation at the end: the one
	// started here, and the restarted ones (their PID is in airview.run).
	defer func() {
		_ = cmd.Process.Kill()
		if info, ok := readInstanceInfo(data); ok && info.PID > 0 && info.PID != os.Getpid() {
			if p, err := os.FindProcess(info.PID); err == nil {
				_ = p.Kill()
			}
		}
		time.Sleep(500 * time.Millisecond)
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(tmp, "airview.log"))
			t.Logf("airview log:\n%s", tail(string(b), 6000))
		}
	}()

	hc := &http.Client{Timeout: 5 * time.Second}
	health := func() (string, int64) {
		resp, err := hc.Get(base + "/healthz")
		if err != nil {
			return "", 0
		}
		defer resp.Body.Close()
		var h struct {
			Version string `json:"version"`
			UptimeS int64  `json:"uptimeS"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&h)
		return h.Version, h.UptimeS
	}
	waitVersion := func(want string, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if v, _ := health(); v == want {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		v, _ := health()
		t.Fatalf("version %q not running after %s (now %q)", want, within, v)
	}
	type state struct {
		Version, Available string
		CanInstall         bool
		InstallNote        string
		Last               selfupdate.Journal
	}
	getState := func() state {
		var s state
		resp, err := hc.Get(base + "/api/update")
		if err != nil {
			return s
		}
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(&s)
		return s
	}
	post := func(path string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, base+path, nil)
		resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	waitPhase := func(phase, to string, within time.Duration) state {
		t.Helper()
		deadline := time.Now().Add(within)
		var s state
		for time.Now().Before(deadline) {
			if s = getState(); s.Last.Phase == phase && s.Last.To == to {
				return s
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("journal phase %q for %s not reached in %s (last %+v)", phase, to, within, s.Last)
		return s
	}

	// 1. 1.0.0 runs and sees 2.0.0.
	waitVersion("1.0.0", 60*time.Second)
	if code, body := post("/api/update/check"); code != 200 || !strings.Contains(body, `"available":"2.0.0"`) {
		t.Fatalf("check: %d %s", code, body)
	}
	if s := getState(); !s.CanInstall {
		t.Fatalf("headless install refused: %+v", s)
	}

	// 2. Install 2.0.0 from the web API; it confirms itself.
	if code, body := post("/api/update/install"); code != http.StatusAccepted {
		t.Fatalf("install 2.0.0: %d %s", code, body)
	}
	waitVersion("2.0.0", 90*time.Second)
	waitPhase("committed", "2.0.0", 60*time.Second)
	if !sameFile(t, exe, v2) {
		t.Fatal("the program file is not the 2.0.0 build")
	}
	t.Logf("1.0.0 -> 2.0.0 installed and committed")

	// 3. A broken 3.0.0: installed, fails its trial, rolled back to 2.0.0.
	time.Sleep(1100 * time.Millisecond) // a later publication time than 2.0.0's
	publish("3.0.0", v3)
	if code, body := post("/api/update/check"); code != 200 || !strings.Contains(body, `"available":"3.0.0"`) {
		t.Fatalf("check 3.0.0: %d %s", code, body)
	}
	if code, body := post("/api/update/install"); code != http.StatusAccepted {
		t.Fatalf("install 3.0.0: %d %s", code, body)
	}
	s := waitPhase("rolledback", "3.0.0", 120*time.Second)
	waitVersion("2.0.0", 60*time.Second)
	if !sameFile(t, exe, v2) {
		t.Fatal("after the rollback the program file is not the 2.0.0 build")
	}
	t.Logf("3.0.0 rolled back: %+v", s.Last)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("...%s", s[len(s)-n:])
}
