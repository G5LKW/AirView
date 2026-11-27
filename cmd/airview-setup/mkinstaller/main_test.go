package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"airview/internal/wininstall"
)

func TestRunBuildsInstallerWithOverrides(t *testing.T) {
	dir := t.TempDir()
	stub := make([]byte, 2048)
	copy(stub, "MZ")
	binary.LittleEndian.PutUint32(stub[0x3c:], 0)
	write := func(n, c string) string {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stubPath := write("stub.exe", string(stub))
	exe := write("airview.exe", "MZ app")
	lic := write("LICENSE.txt", "terms")
	out := filepath.Join(dir, "out", "setup.exe")
	product := filepath.Join("..", "..", "..", "deploy", "installer", "products", "feeder.json")
	tray := write("tray.exe", "MZ tray")
	args := []string{"-stub", stubPath, "-product", product, "-version", "2.0.1", "-out", out,
		"-id", "AirViewFeederTest", "-name", "AirView Feeder Test", "-var", "service=AirViewFeederTest", "-data-dir", `C:\scratch\data`,
		"-file", "airview-feeder.exe=" + exe, "-file", "airview-feeder-tray.exe=" + tray, "-file", "LICENSE.txt=" + lic}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	pl, err := wininstall.OpenPayload(out)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()
	p := pl.Product
	if p.ID != "AirViewFeederTest" || p.Name != "AirView Feeder Test" || p.Version != "2.0.1" || p.Vars["service"] != "AirViewFeederTest" ||
		len(p.DataDirs) != 1 || p.DataDirs[0] != `C:\scratch\data` || len(pl.Files) != 3 || pl.StubSize != 2048 {
		t.Errorf("payload %+v (%d files, stub %d)", p, len(pl.Files), pl.StubSize)
	}
	if err := run([]string{"-verify", out}); err != nil {
		t.Errorf("verify: %v", err)
	}
	for _, bad := range [][]string{
		{"-stub", stubPath, "-product", product, "-out", out}, // no version
		append(append([]string(nil), args...), "-file", "nosep"),
		append(append([]string(nil), args...), "-var", "novalue"),
		append(append([]string(nil), args...), "-id", "bad id"),
	} {
		if err := run(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
