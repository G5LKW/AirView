package piimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// findShell returns a POSIX shell: dash when present (the Pi's /bin/sh),
// else bash in POSIX mode (Git Bash on Windows; not System32\bash.exe, WSL).
func findShell(t *testing.T) []string {
	t.Helper()
	if s := os.Getenv("AVF_TEST_BASH"); s != "" {
		return []string{s, "--posix"}
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{`C:\Program Files\Git\bin\bash.exe`, `C:\Program Files (x86)\Git\bin\bash.exe`} {
			if _, err := os.Stat(p); err == nil {
				return []string{p, "--posix"}
			}
		}
		t.Skip("Git Bash not found; set AVF_TEST_BASH")
	}
	if p, err := exec.LookPath("dash"); err == nil {
		return []string{p}
	}
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no shell")
	}
	return []string{p, "--posix"}
}

func TestFilesAreLFAndShellSyntaxIsValid(t *testing.T) {
	err := fs.WalkDir(Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(Files, p)
		if bytes.Contains(b, []byte("\r\n")) {
			t.Errorf("%s has CRLF line endings", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sh := findShell(t)
	for _, s := range []string{"firstboot.sh", "vendor-data", "pi-gen/build.sh", "pi-gen/stage-airview/prerun.sh",
		"pi-gen/stage-airview/00-airview/00-run.sh", "pi-gen/stage-airview/00-airview/01-run-chroot.sh"} {
		out, err := exec.Command(sh[0], append(sh[1:], "-n", s)...).CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s", s, err, out)
		}
		b, _ := os.ReadFile(s)
		if !bytes.HasPrefix(b, []byte("#!/bin/sh\n")) && !bytes.HasPrefix(b, []byte("#!/bin/bash")) {
			t.Errorf("%s: missing shebang", s)
		}
		if bytes.Contains(b, []byte("\r\n")) {
			t.Errorf("%s has CRLF line endings", s)
		}
	}
}

func TestVendorData(t *testing.T) {
	b, _ := fs.ReadFile(Files, "vendor-data")
	s := string(b)
	if !strings.HasPrefix(s, "#!/bin/sh\n") || !strings.Contains(s, "AirView Raspberry Pi image") ||
		!strings.Contains(s, "exec /bin/sh /boot/firmware/airview/firstboot.sh") {
		t.Fatalf("vendor-data:\n%s", s)
	}
}

func TestPisetupUnit(t *testing.T) {
	b, _ := fs.ReadFile(Files, "airview-pisetup.service")
	s := string(b)
	for _, want := range []string{"ConditionPathExists=!/etc/airview-feeder/feeder.json", "ExecStart=/opt/airview-image/airview-pisetup -listen :8099", "WantedBy=multi-user.target", "Restart=on-failure"} {
		if !strings.Contains(s, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
}

// fakeRoot builds a root tree with a boot partition holding an image payload.
func fakeRoot(t *testing.T, kind, hostname string, payload map[string]string) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "boot", "firmware", "airview")
	for _, d := range []string{src, filepath.Join(root, "etc", "cloud", "cloud.cfg.d"), filepath.Join(root, "etc", "systemd", "system")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "etc", "hostname"), hostname+"\n")
	write(filepath.Join(root, "etc", "hosts"), "127.0.0.1\tlocalhost\n\n127.0.1.1\t\t"+hostname+"\n")
	host := map[string]string{"station": "airview", "feeder": "airview-feeder"}[kind]
	write(filepath.Join(src, "image.conf"), "# test\r\nAIRVIEW_IMAGE_KIND="+kind+"\r\nAIRVIEW_IMAGE_VERSION=9.9.9\r\nAIRVIEW_HOSTNAME="+host+"\r\n")
	fb, _ := fs.ReadFile(Files, "firstboot.sh")
	write(filepath.Join(src, "firstboot.sh"), string(fb))
	var sums strings.Builder
	for name, content := range payload {
		write(filepath.Join(src, name), content)
		s := sha256.Sum256([]byte(content))
		sums.WriteString(hex.EncodeToString(s[:]) + "  " + name + "\n")
	}
	write(filepath.Join(src, "SHA256SUMS"), sums.String())
	return root
}

func runFirstboot(t *testing.T, root string) (string, error) {
	t.Helper()
	sh := findShell(t)
	script := filepath.ToSlash(filepath.Join(root, "boot", "firmware", "airview", "firstboot.sh"))
	cmd := exec.Command(sh[0], append(sh[1:], script)...)
	cmd.Env = append(os.Environ(), "AVI_ROOT="+filepath.ToSlash(root), "AVI_DRYRUN=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var feederPayload = map[string]string{
	"airview-feeder-linux-arm64": "ELF feeder",
	"install-feeder.sh":          "#!/bin/bash\necho feeder installer\n",
	"airview-pisetup":            "ELF pisetup",
	"airview-pisetup.service":    "[Unit]\n",
}

func TestFirstbootFeeder(t *testing.T) {
	root := fakeRoot(t, "feeder", "raspberrypi", feederPayload)
	src := filepath.Join(root, "boot", "firmware", "airview")
	if err := os.WriteFile(filepath.Join(src, "feeder-setup.txt"), []byte("SERVER=feed.example.net\r\nTOKEN=secret\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runFirstboot(t, root)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"setting the host name to 'airview-feeder'",
		"+ systemctl enable airview-pisetup.service",
		"+ systemctl start --no-block airview-pisetup.service",
		"feeder-setup.txt has an aggregator",
		"done: open http://airview-feeder.local:8099/",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if h := read(t, filepath.Join(root, "etc", "hostname")); h != "airview-feeder\n" {
		t.Errorf("hostname %q", h)
	}
	if h := read(t, filepath.Join(root, "etc", "hosts")); !strings.Contains(h, "127.0.1.1\tairview-feeder\n") || strings.Contains(h, "raspberrypi") {
		t.Errorf("hosts:\n%s", h)
	}
	if c := read(t, filepath.Join(root, "etc", "cloud", "cloud.cfg.d", "90-airview-hostname.cfg")); !strings.Contains(c, "preserve_hostname: true") {
		t.Errorf("cloud cfg %q", c)
	}
	for name, content := range feederPayload {
		if got := read(t, filepath.Join(root, "opt", "airview-image", name)); got != content {
			t.Errorf("/opt/airview-image/%s = %q", name, got)
		}
		if exists(filepath.Join(src, name)) {
			t.Errorf("%s left on the boot partition", name)
		}
	}
	if !exists(filepath.Join(root, "etc", "systemd", "system", "airview-pisetup.service")) {
		t.Error("unit not installed")
	}
	if s := read(t, filepath.Join(root, "var", "lib", "airview-image", "feeder-setup.txt")); !strings.Contains(s, "TOKEN=secret") {
		t.Error("setup answers not handed to pisetup")
	}
	if exists(filepath.Join(src, "feeder-setup.txt")) || exists(filepath.Join(src, "SHA256SUMS")) {
		t.Error("secrets/checksums left on the boot partition")
	}
	if !exists(filepath.Join(src, "INSTALLED.txt")) || !exists(filepath.Join(src, "firstboot.sh")) {
		t.Error("INSTALLED.txt or firstboot.sh missing")
	}
	if d := read(t, filepath.Join(root, "var", "lib", "airview-image", "firstboot.done")); !strings.HasPrefix(d, "feeder 9.9.9 ") {
		t.Errorf("done marker %q", d)
	}
	// Second run: nothing happens.
	out, err = runFirstboot(t, root)
	if err != nil || !strings.Contains(out, "already provisioned") || strings.Contains(out, "systemctl") {
		t.Fatalf("second run: %v\n%s", err, out)
	}
}

func TestFirstbootStationKeepsImagerHostname(t *testing.T) {
	payload := map[string]string{
		"airview-linux-arm64": "ELF airview",
		"install.sh":          "#!/bin/sh\n",
		"airview.service":     "[Unit]\n",
	}
	root := fakeRoot(t, "station", "my-pi", payload)
	src := filepath.Join(root, "boot", "firmware", "airview")
	os.WriteFile(filepath.Join(src, "airview.jsonc"), []byte("{\r\n  \"http\": {}\r\n}\r\n"), 0o644)
	out, err := runFirstboot(t, root)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "keeping the host name 'my-pi'") || !strings.Contains(out, "/opt/airview-image/install.sh") ||
		!strings.Contains(out, "done: open http://my-pi.local/") {
		t.Fatalf("output:\n%s", out)
	}
	if h := read(t, filepath.Join(root, "etc", "hostname")); h != "my-pi\n" {
		t.Errorf("hostname changed to %q", h)
	}
	if exists(filepath.Join(root, "etc", "cloud", "cloud.cfg.d", "90-airview-hostname.cfg")) {
		t.Error("preserve_hostname written although the name was kept")
	}
	if c := read(t, filepath.Join(root, "etc", "airview", "airview.jsonc")); c != "{\n  \"http\": {}\n}\n" {
		t.Errorf("operator file %q", c)
	}
	if exists(filepath.Join(src, "airview.jsonc")) || exists(filepath.Join(src, "airview-linux-arm64")) {
		t.Error("boot partition not cleaned")
	}
}

func TestFirstbootRefusesDamagedPayload(t *testing.T) {
	root := fakeRoot(t, "feeder", "raspberrypi", feederPayload)
	src := filepath.Join(root, "boot", "firmware", "airview")
	os.WriteFile(filepath.Join(src, "airview-pisetup"), []byte("corrupted"), 0o644)
	out, err := runFirstboot(t, root)
	if err == nil || !strings.Contains(out, "damaged") {
		t.Fatalf("damaged payload accepted: %v\n%s", err, out)
	}
	if exists(filepath.Join(root, "var", "lib", "airview-image", "firstboot.done")) || !exists(filepath.Join(src, "airview-feeder-linux-arm64")) {
		t.Fatal("failed run marked done or removed the payload")
	}
	// Unknown kind and missing image.conf.
	root = fakeRoot(t, "toaster", "raspberrypi", feederPayload)
	if out, err := runFirstboot(t, root); err == nil || !strings.Contains(out, "unknown AIRVIEW_IMAGE_KIND") {
		t.Fatalf("unknown kind: %v\n%s", err, out)
	}
	root = fakeRoot(t, "feeder", "raspberrypi", feederPayload)
	os.Remove(filepath.Join(root, "boot", "firmware", "airview", "image.conf"))
	if out, err := runFirstboot(t, root); err == nil || !strings.Contains(out, "not an AirView image") {
		t.Fatalf("missing image.conf: %v\n%s", err, out)
	}
	// A payload name with a path is refused.
	root = fakeRoot(t, "feeder", "raspberrypi", feederPayload)
	sums := filepath.Join(root, "boot", "firmware", "airview", "SHA256SUMS")
	os.WriteFile(sums, append([]byte(read(t, sums)), []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  ../evil\n")...), 0o644)
	os.WriteFile(filepath.Join(root, "boot", "firmware", "evil"), nil, 0o644)
	if out, err := runFirstboot(t, root); err == nil || !strings.Contains(out, "unexpected payload name") {
		t.Fatalf("path in SHA256SUMS: %v\n%s", err, out)
	}
}
