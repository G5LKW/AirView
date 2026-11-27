//go:build windows

package wininstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestShellLinkRoundTrip writes a real .lnk through IShellLinkW and reads
// it back with the pure-Go parser.
func TestShellLinkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Program Files", "AirView Test", "airview.exe")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	lnk := filepath.Join(dir, "Start Menu", "AirView Test.lnk")
	want := ShortcutTarget{Target: target, Args: `-data "C:\x y"`, WorkDir: filepath.Dir(target), Icon: target, Description: "ADS-B station"}
	if err := (WindowsSystem{}).CreateShortcut(lnk, want); err != nil {
		t.Fatal(err)
	}
	l, err := ReadLink(lnk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(l.Target, target) || l.Args != want.Args || !strings.EqualFold(l.WorkDir, want.WorkDir) ||
		l.Description != want.Description || !strings.EqualFold(l.IconPath, target) {
		t.Errorf("read back %+v", l)
	}
}

func TestParseLinkRejectsGarbage(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("not a link"), make([]byte, 0x4C)} {
		if _, err := ParseLink(b); err == nil {
			t.Errorf("%q accepted", b)
		}
	}
}

func TestWindowsSystemBasics(t *testing.T) {
	s := WindowsSystem{}
	f, err := s.Folders(PerUser)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{FolderProgramFiles, FolderPrograms, FolderDesktop, FolderUserAppData, FolderCommonAppData} {
		if !filepath.IsAbs(f[k]) {
			t.Errorf("%s = %q", k, f[k])
		}
	}
	t.Setenv(EnvFolderPrefix+"DESKTOP", `D:\scratch\desk`)
	f, _ = s.Folders(PerMachine)
	if f[FolderDesktop] != `D:\scratch\desk` {
		t.Errorf("override: %q", f[FolderDesktop])
	}
	out, code, err := s.Exec(filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), []string{"/c", "echo hi&& exit 3"}, 10*time.Second)
	if err != nil || code != 3 || !strings.Contains(out, "hi") {
		t.Errorf("exec: %q %d %v", out, code, err)
	}
	if ex, _, err := s.ServiceExists("AirViewNoSuchService-7c1e"); ex || err != nil {
		t.Errorf("missing service: %v %v", ex, err)
	}
	if ex, _, _ := s.ServiceExists("EventLog"); !ex {
		t.Error("EventLog service not found")
	}
	if _, ok, err := s.ReadUninstall(PerUser, "AirViewNoSuchProduct-7c1e"); ok || err != nil {
		t.Errorf("missing key: %v %v", ok, err)
	}
}
