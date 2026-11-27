package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func fakePE(sub uint16) []byte {
	b := make([]byte, 0x200)
	b[0], b[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(b[0x3C:], 0x80)
	copy(b[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[0x80+peSubsystemOffset:], sub)
	b[0x1F0] = 0x42
	return b
}

// TestRefreshTray: the tray copy gets the GUI subsystem, is rewritten only
// when it differs, and a missing tray is left alone.
func TestRefreshTray(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "airview-feeder.exe")
	if err := os.WriteFile(exe, fakePE(3), 0o755); err != nil {
		t.Fatal(err)
	}
	if w, err := refreshTray(exe, "airview-feeder-tray.exe"); w || err != nil {
		t.Fatalf("no tray: %v %v", w, err)
	}
	tray := filepath.Join(dir, "airview-feeder-tray.exe")
	if err := os.WriteFile(tray, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if w, err := refreshTray(exe, "airview-feeder-tray.exe"); !w || err != nil {
		t.Fatalf("refresh: %v %v", w, err)
	}
	got, _ := os.ReadFile(tray)
	if !bytes.Equal(got, fakePE(2)) {
		t.Fatal("tray is not the GUI copy")
	}
	if w, err := refreshTray(exe, "airview-feeder-tray.exe"); w || err != nil {
		t.Fatalf("second refresh: %v %v", w, err)
	}
	if _, err := guiCopy([]byte("not a pe")); err == nil {
		t.Fatal("garbage accepted")
	}
}
