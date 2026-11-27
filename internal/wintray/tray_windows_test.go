package wintray

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// The structure sizes must match the C definitions (64-bit Windows).
func TestStructLayouts(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("sizes checked for 64-bit targets")
	}
	for name, got := range map[string]uintptr{
		"NOTIFYICONDATAW":   unsafe.Sizeof(notifyIconData{}),
		"WNDCLASSEXW":       unsafe.Sizeof(wndClassEx{}),
		"SHELLEXECUTEINFOW": unsafe.Sizeof(shellExecuteInfo{}),
		"MSG":               unsafe.Sizeof(msg{}),
	} {
		want := map[string]uintptr{"NOTIFYICONDATAW": 976, "WNDCLASSEXW": 80, "SHELLEXECUTEINFOW": 112, "MSG": 48}[name]
		if got != want {
			t.Errorf("sizeof(%s) = %d, want %d", name, got, want)
		}
	}
}

func TestCopyUTF16(t *testing.T) {
	var b [8]uint16
	copyUTF16(b[:], "abc\x00def-long-text")
	if got := string(utf16ToRunes(b[:])); got != "abcdef-" {
		t.Fatalf("got %q", got)
	}
	if b[7] != 0 {
		t.Fatal("not terminated")
	}
	if Escape("R&D") != "R&&D" {
		t.Fatal("Escape")
	}
}

func utf16ToRunes(u []uint16) []rune {
	var r []rune
	for _, c := range u {
		if c == 0 {
			break
		}
		r = append(r, rune(c))
	}
	return r
}

// TestTrayLifecycle creates a real notification-area icon, drives it from
// other goroutines and through window messages, and closes it with a
// signal, as the installer does. Set WINTRAY_NO_GUI=1 to skip (e.g. a
// service account without a desktop).
func TestTrayLifecycle(t *testing.T) {
	if os.Getenv("WINTRAY_NO_GUI") != "" {
		t.Skip("WINTRAY_NO_GUI set")
	}
	class := fmt.Sprintf("AirViewWinTrayTest-%d", os.Getpid())
	var clicked, refreshed atomic.Int32
	ready := make(chan *Tray, 1)
	done := make(chan error, 1)
	level := LevelOK
	go func() {
		done <- Run(Options{
			ClassName: class,
			Tooltip:   "wintray test",
			Icon:      func(size int) []byte { return PNG(size, level) },
			Menu: func() []Item {
				return []Item{
					{Title: "Status: fine & dandy", Disabled: true},
					{Separator: true},
					{Title: "Open", Default: true, OnClick: func() { clicked.Add(1) }},
					{Title: "Checked", Checked: true, OnClick: func() {}},
				}
			},
			OnSignal: func(s Signal) {
				if s == SignalRefresh {
					refreshed.Add(1)
				}
			},
			Ready: func(tr *Tray) { ready <- tr },
		})
	}()
	var tr *Tray
	select {
	case tr = <-ready:
	case err := <-done:
		if err != nil && strings.Contains(err.Error(), "Shell_NotifyIcon") {
			t.Skipf("no notification area in this session: %v", err)
		}
		t.Fatalf("Run returned early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("tray did not start")
	}

	// Updates from another goroutine are marshalled onto the UI thread.
	ran := make(chan bool, 1)
	if !tr.Do(func() { ran <- true }) {
		t.Fatal("Do refused")
	}
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("Do did not run")
	}
	tr.SetTooltip("wintray test: updated")
	tr.SetIcon(func(size int) []byte { return PNG(size, LevelWarn) })
	tr.Notify("wintray test", "balloon text", NotifyInfo)

	if !tr.Choose("Open") {
		t.Fatal("Choose(Open) = false")
	}
	if tr.Choose("Status: fine & dandy") {
		t.Fatal("a disabled entry was chosen")
	}
	waitFor(t, func() bool { return clicked.Load() == 1 }, "OnClick")

	if n := SignalRunning(class, SignalRefresh); n != 1 {
		t.Fatalf("SignalRunning found %d trays, want 1", n)
	}
	waitFor(t, func() bool { return refreshed.Load() == 1 }, "OnSignal")

	if n := SignalRunning(class, SignalQuit); n != 1 {
		t.Fatalf("SignalRunning(quit) found %d trays", n)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tray did not quit")
	}
	if tr.Do(func() {}) {
		t.Fatal("Do accepted work after the tray closed")
	}
	if n := SignalRunning(class, SignalQuit); n != 0 {
		t.Fatalf("window still present after quit (%d)", n)
	}
}

func TestClipboardRoundTrip(t *testing.T) {
	if os.Getenv("WINTRAY_NO_GUI") != "" {
		t.Skip("WINTRAY_NO_GUI set")
	}
	old, err := ClipboardText()
	if err != nil {
		t.Skipf("clipboard unavailable: %v", err)
	}
	defer func() { _ = SetClipboardText(old) }()
	want := "AV-TEST-1234 ünïcode"
	if err := SetClipboardText(want); err != nil {
		t.Fatal(err)
	}
	got, err := ClipboardText()
	if err != nil || got != want {
		t.Fatalf("clipboard %q, %v; want %q", got, err, want)
	}
}

func TestRunHiddenExitCode(t *testing.T) {
	cmd := os.Getenv("ComSpec")
	if cmd == "" {
		cmd = `C:\Windows\System32\cmd.exe`
	}
	code, err := RunHidden(cmd, "/c exit 7", 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Fatalf("exit code %d, want 7", code)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
