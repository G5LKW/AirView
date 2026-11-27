package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/selfupdate"
	"airview/internal/server"
)

// fakeExe writes a file that passes the executable-format check for the
// running target (ELF / PE / Mach-O magic) followed by fill.
func fakeExe(t *testing.T, path string, fill byte) {
	t.Helper()
	var magic []byte
	switch runtime.GOOS {
	case "windows":
		magic = []byte("MZ\x90\x00")
	case "darwin":
		magic = []byte{0xcf, 0xfa, 0xed, 0xfe}
	default:
		magic = []byte("\x7fELF")
	}
	b := append(magic, []byte(strings.Repeat(string([]byte{fill}), 4096))...)
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// releaseServer publishes version ver of the "airview" product for the
// running target and serves it like /feeder/* (selfupdate.ReleaseHandler).
func releaseServer(t *testing.T, ver string) (url, pub string) {
	t.Helper()
	pk, priv, err := selfupdate.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	art := filepath.Join(dir, "artifact")
	fakeExe(t, art, 0xa2)
	out := filepath.Join(dir, "rel")
	if _, err := selfupdate.Publish(selfupdate.PublishOptions{Product: "airview", Version: ver, Channel: "stable",
		Artifacts: map[string]string{selfupdate.CurrentTarget(): art}, OutDir: out, Key: priv}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(selfupdate.ReleaseHandler{Dir: out})
	t.Cleanup(srv.Close)
	return srv.URL + "/manifest.json", pk.String()
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestUpdateCtl: state, check and a background install from the web page
// (self-confirm mode: the program file is swapped and the restarter is
// asked to start the new version); the desktop window and a read-only
// program file cannot install and say why.
func TestUpdateCtl(t *testing.T) {
	murl, pub := releaseServer(t, "99.0.0")
	dir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "airview-under-test"+map[bool]string{true: ".exe"}[runtime.GOOS == "windows"])
	fakeExe(t, exe, 0x11)
	var restarts atomic.Int32
	set := model.UpdateSettings{Channel: "stable", ManifestURL: murl, Keys: []string{pub}, CheckIntervalMin: 1440, HealthWindowS: 60, MaxDownloadMB: 64}
	exited := make(chan struct{}, 1)
	mk := func() *updateCtl {
		return &updateCtl{dir: dir, log: quietLog(), settings: func() model.UpdateSettings { return set }, install: true, exe: exe,
			restarter: selfupdate.RestarterFunc(func(context.Context) error { restarts.Add(1); return nil }),
			exitReq:   func() { exited <- struct{}{} }}
	}
	c := mk()

	st := c.UpdateState()
	if !st.Supported || !st.Configured || !st.CanInstall || st.InstallNote != "" || st.Version != version {
		t.Fatalf("state: %+v", st)
	}
	st, err := c.CheckUpdate(context.Background())
	if err != nil || st.Available != "99.0.0" || st.LastCheck == "" {
		t.Fatalf("check: %v %+v", err, st)
	}
	if _, err := c.StartInstall(); err != nil {
		t.Fatalf("install: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for c.UpdateState().Busy && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	st = c.UpdateState()
	if st.Busy || st.ActionError != "" || st.Result == nil || (st.Result.Outcome != selfupdate.OutcomePending && st.Result.Outcome != selfupdate.OutcomeUpdated) {
		t.Fatalf("after install: %+v (result %+v)", st, st.Result)
	}
	if restarts.Load() != 1 {
		t.Fatalf("restarts %d", restarts.Load())
	}
	b, _ := os.ReadFile(exe)
	if len(b) < 10 || b[len(b)-1] != 0xa2 {
		t.Fatal("the program file was not replaced by the release")
	}
	if st.Last.To != "99.0.0" || st.Last.Phase == "" {
		t.Fatalf("journal: %+v", st.Last)
	}

	// A second install while busy is refused; an up-to-date one too.
	old := version
	version = "100.0.0"
	if _, err := c.StartInstall(); !errors.Is(err, server.ErrUpToDate) {
		t.Fatalf("up to date: %v", err)
	}
	version = old

	// Desktop window: no install, a reason.
	d := mk()
	d.install = false
	if st := d.UpdateState(); st.CanInstall || !strings.Contains(st.InstallNote, "desktop") {
		t.Fatalf("desktop: %+v", st)
	}
	if _, err := d.StartInstall(); !errors.Is(err, server.ErrUpdateNotPossible) {
		t.Fatalf("desktop install: %v", err)
	}
	// Read-only program file: the command to run instead.
	ro := mk()
	ro.writable = func(string) error { return errors.New("permission denied") }
	if st := ro.UpdateState(); st.CanInstall || !strings.Contains(st.InstallNote, "airview update") {
		t.Fatalf("read-only: %+v", st)
	}
	// Not configured.
	nc := mk()
	nc.settings = func() model.UpdateSettings { return model.UpdateSettings{Channel: "stable"} }
	if _, err := nc.CheckUpdate(context.Background()); !errors.Is(err, selfupdate.ErrNotConfigured) {
		t.Fatalf("not configured: %v", err)
	}
	if st := nc.UpdateState(); st.Configured || st.CanInstall {
		t.Fatalf("not configured state: %+v", st)
	}
	// A tampered key: the check fails, nothing is available.
	tk := mk()
	pk2, _, _ := selfupdate.GenerateKey()
	tk.settings = func() model.UpdateSettings { s := set; s.Keys = []string{pk2.String()}; return s }
	if st, err := tk.CheckUpdate(context.Background()); err == nil || st.Available != "" {
		t.Fatalf("foreign key accepted: %v %+v", err, st)
	}
}

// TestUpdateCtlLoopOff: the automatic loop does nothing while
// update.enabled is off and ends with its context.
func TestUpdateCtlLoopOff(t *testing.T) {
	c := &updateCtl{dir: t.TempDir(), log: quietLog(), settings: func() model.UpdateSettings { return model.UpdateSettings{} }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.loop(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not end")
	}
}
