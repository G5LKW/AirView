package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The Windows tray program airview-feeder-tray.exe is airview-feeder.exe
// with the PE optional header's Subsystem set to IMAGE_SUBSYSTEM_WINDOWS_GUI
// (built with -H windowsgui). The updater replaces only airview-feeder.exe,
// so after an update the tray copy is refreshed from it (HOOKS: WP-WF ->
// selfupdate "the updater must also refresh airview-feeder-tray.exe").

const (
	peSubsystemOffset = 0x5C // from e_lfanew: 4 (signature) + 20 (file header) + 68
	peSubsystemGUI    = 2
)

// guiCopy returns exe with the PE Subsystem field set to GUI.
func guiCopy(exe []byte) ([]byte, error) {
	if len(exe) < 0x40 || exe[0] != 'M' || exe[1] != 'Z' {
		return nil, errors.New("not a PE file")
	}
	off := int(binary.LittleEndian.Uint32(exe[0x3C:]))
	if off <= 0 || off+peSubsystemOffset+2 > len(exe) || !bytes.Equal(exe[off:off+4], []byte("PE\x00\x00")) {
		return nil, errors.New("no PE header")
	}
	out := bytes.Clone(exe)
	binary.LittleEndian.PutUint16(out[off+peSubsystemOffset:], peSubsystemGUI)
	return out, nil
}

// refreshTray rewrites trayName next to exePath when it differs from the
// GUI copy of exePath. A tray that is running is renamed aside first
// (Windows allows renaming a running image). It reports whether it wrote.
func refreshTray(exePath, trayName string) (bool, error) {
	tray := filepath.Join(filepath.Dir(exePath), trayName)
	old, err := os.ReadFile(tray)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // no tray installed
	}
	if err != nil {
		return false, err
	}
	exe, err := os.ReadFile(exePath)
	if err != nil {
		return false, err
	}
	want, err := guiCopy(exe)
	if err != nil {
		return false, err
	}
	if bytes.Equal(old, want) {
		return false, nil
	}
	tmp := tray + ".new"
	if err := os.WriteFile(tmp, want, 0o755); err != nil {
		return false, err
	}
	aside := fmt.Sprintf("%s.%d.del", tray, time.Now().UnixNano())
	if err := os.Rename(tray, aside); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, tray); err != nil {
		_ = os.Rename(aside, tray)
		return false, err
	}
	_ = os.Remove(aside) // fails while the old tray still runs; removed next time
	cleanupAside(tray)
	return true, nil
}

// cleanupAside removes leftovers of earlier refreshes.
func cleanupAside(tray string) {
	m, _ := filepath.Glob(tray + ".*.del")
	for _, f := range m {
		if strings.HasSuffix(f, ".del") {
			_ = os.Remove(f)
		}
	}
}
