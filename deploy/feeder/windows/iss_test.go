package winfeeder

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const issFile = "airview-feeder.iss"

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// issSection returns the lines of one [Section] of the script.
func issSection(t *testing.T, iss, name string) []string {
	t.Helper()
	var out []string
	in := false
	sc := bufio.NewScanner(strings.NewReader(iss))
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			in = strings.EqualFold(l, "["+name+"]")
			continue
		}
		if in {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no [%s] section", issFile, name)
	}
	return out
}

func customMessages(t *testing.T, iss string) map[string]string {
	m := map[string]string{}
	for _, l := range issSection(t, iss, "CustomMessages") {
		if l == "" || strings.HasPrefix(l, ";") {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			t.Fatalf("bad CustomMessages line %q", l)
		}
		m[k] = strings.ReplaceAll(v, "%n", "\n")
	}
	return m
}

func define(t *testing.T, iss, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^#define ` + name + ` "([^"]*)"`).FindStringSubmatch(iss)
	if m == nil {
		t.Fatalf("%s: no #define %s", issFile, name)
	}
	return m[1]
}

// goConst finds `name = "value"` or name = `value` in a Go source file.
func goConst(t *testing.T, file, name string) string {
	t.Helper()
	src := read(t, file)
	if m := regexp.MustCompile(`(?m)\b` + name + `\s*=\s*"([^"]*)"`).FindStringSubmatch(src); m != nil {
		return m[1]
	}
	if m := regexp.MustCompile("(?s)\\b" + name + "\\s*=\\s*`([^`]*)`").FindStringSubmatch(src); m != nil {
		return m[1]
	}
	t.Fatalf("%s: constant %s not found", file, name)
	return ""
}

// TestConsentTexts: the installer shows C1 and C2 word for word.
func TestConsentTexts(t *testing.T) {
	iss := read(t, issFile)
	cm := customMessages(t, iss)

	doc := read(t, filepath.Join("..", "..", "..", "docs", "aggregator", "CONSENT.md"))
	m := regexp.MustCompile(`(?m)^> "(If your receiver sends timestamps[^"]*)"\s*$`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("CONSENT.md: C1 quote not found")
	}
	if cm["C1"] != m[1] {
		t.Errorf("C1 differs from CONSENT.md:\n iss: %q\n doc: %q", cm["C1"], m[1])
	}
	if c1 := goConst(t, filepath.Join("..", "..", "..", "cmd", "airview-feeder", "setup.go"), "consentText"); cm["C1"] != c1 {
		t.Errorf("C1 differs from cmd/airview-feeder consentText:\n iss: %q\n go:  %q", cm["C1"], c1)
	}

	got := cm["C2Title"] + "\n  - " + cm["C2Hidden"] + "\n  - " + cm["C2Region"] + "\n  - " + cm["C2Area"] + "\n" + cm["C2Note"]
	want := goConst(t, filepath.Join("..", "..", "..", "cmd", "airview-feeder", "consent.go"), "consentPin")
	if got != want {
		t.Errorf("C2 differs from cmd/airview-feeder consentPin:\n iss: %q\n go:  %q", got, want)
	}
	// Each C2 message must be used on the page.
	for _, k := range []string{"C1", "C2Title", "C2Hidden", "C2Region", "C2Area", "C2Note"} {
		if !strings.Contains(iss, "CustomMessage('"+k+"')") {
			t.Errorf("custom message %s is never shown", k)
		}
	}
	// The pin choice labels map to the documented values.
	for _, v := range []string{"-privacy hidden", "-privacy region", "-privacy cell01", "-no-timestamps", "-timestamps"} {
		if !strings.Contains(iss, v) {
			t.Errorf("%s does not pass %q", issFile, v)
		}
	}
}

// TestNamesMatchProgram: names the installer shares with the program.
func TestNamesMatchProgram(t *testing.T) {
	iss := read(t, issFile)
	feeder := filepath.Join("..", "..", "..", "cmd", "airview-feeder")
	if got, want := define(t, iss, "TrayRunValue"), goConst(t, filepath.Join(feeder, "tray_windows.go"), "trayRunValue"); got != want {
		t.Errorf("TrayRunValue %q, tray uses %q", got, want)
	}
	if got, want := define(t, iss, "ServiceName"), goConst(t, filepath.Join(feeder, "winmain_windows.go"), "defaultServiceName"); got != want {
		t.Errorf("ServiceName %q, program default %q", got, want)
	}
	if got, want := define(t, iss, "AppName"), goConst(t, filepath.Join(feeder, "winmain_windows.go"), "defaultServiceDisplay"); got != want {
		t.Errorf("AppName %q (the data directory name), program uses %q", got, want)
	}
	// The data directory the installer writes is the program's default.
	if p := read(t, filepath.Join(feeder, "paths.go")); !strings.Contains(p, `"AirView Feeder"`) {
		t.Error(`paths.go no longer uses %ProgramData%\AirView Feeder; update the installer`)
	}
	if !regexp.MustCompile(`(?m)^AppId=\{\{[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}\}\r?$`).MatchString(iss) {
		t.Error("AppId must be a fixed GUID (upgrades find the old install by it)")
	}
	// Every file the script packs is staged by build.ps1, and the programs
	// may be in use at uninstall time.
	build := read(t, "build.ps1")
	for _, l := range issSection(t, iss, "Files") {
		m := regexp.MustCompile(`Source: "\{#SrcDir\}\\([^"]+)"`).FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if !strings.Contains(build, m[1]) {
			t.Errorf("build.ps1 does not stage %s", m[1])
		}
		if strings.HasSuffix(m[1], ".exe") && !strings.Contains(l, "uninsrestartdelete") {
			t.Errorf("%s: an exe that may be running needs uninsrestartdelete", m[1])
		}
	}
	// The firewall rule added on install is the one removed on uninstall.
	if n := strings.Count(iss, `name="{#FirewallRule}"`); n < 3 {
		t.Errorf("firewall rule referenced %d times, want add + delete on install + delete on uninstall", n)
	}
}

// TestPortableScripts: CRLF batch files that call existing commands.
func TestPortableScripts(t *testing.T) {
	for _, f := range []string{"Install service.cmd", "Remove service.cmd"} {
		b := []byte(read(t, filepath.Join("portable", f)))
		if bytes.Count(b, []byte("\n")) != bytes.Count(b, []byte("\r\n")) {
			t.Errorf("%s: batch files need CRLF line ends", f)
		}
	}
	if s := read(t, filepath.Join("portable", "Install service.cmd")); !strings.Contains(s, "service install -now") || !strings.Contains(s, "tray -signal quit") {
		t.Error("Install service.cmd: unexpected commands")
	}
	if s := read(t, filepath.Join("portable", "Remove service.cmd")); !strings.Contains(s, "service remove") {
		t.Error("Remove service.cmd: unexpected commands")
	}
}

// buildFeeder builds airview-feeder for this machine (Windows only: the
// service and tray commands exist only there).
func buildFeeder(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("the Windows commands exist only in Windows builds")
	}
	if testing.Short() {
		t.Skip("builds the feeder")
	}
	exe := filepath.Join(t.TempDir(), "airview-feeder.exe")
	cmd := exec.Command("go", "build", "-o", exe, "airview/cmd/airview-feeder")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return exe
}

func run(t *testing.T, exe string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "AIRVIEW_FEEDER_CONFIG=", "AIRVIEW_FEEDER_DATA=")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// TestCommandsExist: every command and flag the installer and the portable
// scripts use is accepted by the program, and the installer's setup and
// config calls work as written.
func TestCommandsExist(t *testing.T) {
	exe := buildFeeder(t)
	iss := read(t, issFile)
	uses := []struct {
		cmd   []string
		flags []string
	}{
		{[]string{"tray"}, []string{"signal"}},
		{[]string{"service", "install"}, []string{"name", "now"}},
		{[]string{"service", "stop"}, []string{"name", "wait"}},
		{[]string{"service", "remove"}, []string{"name"}},
		{[]string{"uninstall-helper"}, []string{"data", "purge", "yes"}},
		{[]string{"setup"}, []string{"non-interactive", "config", "data", "server", "pin", "enrol", "name",
			"no-location", "lat", "lon", "alt", "agl", "no-timestamps", "timestamps", "privacy"}},
	}
	for _, u := range uses {
		out, code := run(t, exe, append(append([]string{}, u.cmd...), "-h")...)
		if code != 0 {
			t.Errorf("%v -h: exit %d\n%s", u.cmd, code, out)
			continue
		}
		for _, f := range u.flags {
			if !regexp.MustCompile(`(?m)^\s+-` + regexp.QuoteMeta(f) + `(\s|$)`).MatchString(out) {
				t.Errorf("%v has no -%s flag", u.cmd, f)
			}
			if !strings.Contains(iss, " -"+f) && !strings.Contains(iss, "'-"+f) {
				t.Errorf("%s does not use %v -%s (update this test)", issFile, u.cmd, f)
			}
		}
	}

	// The installer's setup line, with values a user would type.
	dir := t.TempDir()
	conf := filepath.Join(dir, "feeder.json")
	out, code := run(t, exe, "setup", "-non-interactive", "-config", conf, "-data", dir, "-server", "feed.example.org",
		"-name", "home_1", "-lat", "51.4775", "-lon", "-0.4614", "-alt", "35m", "-agl", "8m", "-timestamps", "-privacy", "cell01")
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	// Bad values are refused with exit 2 (the installer's validation step).
	if out, code := run(t, exe, "setup", "-non-interactive", "-config", filepath.Join(dir, "x.json"), "-data", dir,
		"-server", "feed.example.org", "-lat", "91", "-lon", "0", "-alt", "35m"); code != 2 {
		t.Errorf("setup with latitude 91: exit %d, want 2\n%s", code, out)
	}
	// The LAN status page call.
	if out, code := run(t, exe, "config", "set", "-config", conf, "-data", dir, "ui.passwordHash=correct horse", "ui.addr=0.0.0.0:8099"); code != 0 {
		t.Fatalf("config set: exit %d\n%s", code, out)
	}
	c := read(t, conf)
	if strings.Contains(c, "correct horse") || !strings.Contains(c, `"0.0.0.0:8099"`) || !strings.Contains(c, "pbkdf2") {
		t.Errorf("config set did not store a hashed password and the LAN address:\n%s", c)
	}
	if out, code := run(t, exe, "config", "check", "-config", conf, "-data", dir); code != 0 {
		t.Errorf("config check after the installer's calls: exit %d\n%s", code, out)
	}
	if !strings.Contains(c, `"pin": "cell01"`) {
		t.Errorf("privacy cell01 not stored:\n%s", c)
	}
	// /MANIFESTURL= /MANIFESTKEY=: the update settings, then the update
	// command the scheduled task and the tray run accepts them.
	key := strings.Repeat("B", 43) + "="
	if out, code := run(t, exe, "config", "set", "-config", conf, "-data", dir,
		"update.manifestUrl=https://feed.example.org/feeder/manifest.json", "update.keys="+key); code != 0 {
		t.Fatalf("config set update: exit %d\n%s", code, out)
	}
	if c := read(t, conf); !strings.Contains(c, key) || !strings.Contains(c, "/feeder/manifest.json") {
		t.Errorf("update settings not stored:\n%s", c)
	}
	if out, code := run(t, exe, "update", "-status", "-config", conf, "-data", dir); code != 0 {
		t.Errorf("update -status with the installer's settings: exit %d\n%s", code, out)
	}
	if !strings.Contains(iss, "update.manifestUrl=") || !strings.Contains(iss, "update.keys=") {
		t.Errorf("%s does not write the update settings", issFile)
	}
	// uninstall-helper lists without deleting.
	if out, code := run(t, exe, "uninstall-helper", "-data", dir, "-config", conf); code != 0 || !strings.Contains(out, "feeder.json") {
		t.Errorf("uninstall-helper: exit %d\n%s", code, out)
	}
}

// TestBuildScriptParses: build.ps1 has no PowerShell syntax errors.
func TestBuildScriptParses(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell")
	}
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe not found")
	}
	abs, _ := filepath.Abs("build.ps1")
	script := `$e=$null; [void][System.Management.Automation.Language.Parser]::ParseFile('` + abs + `',[ref]$null,[ref]$e); if ($e.Count) { $e | % { $_.ToString() }; exit 1 }`
	out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("build.ps1 does not parse: %v\n%s", err, out)
	}
}

// DLV-05: the portable ZIP and the Inno installer ship the licence terms and
// the third-party notices next to the Go binaries.
func TestNoticesShipped(t *testing.T) {
	ps := read(t, "build.ps1")
	for _, want := range []string{"'LICENSE.txt'", "'THIRD-PARTY-NOTICES.txt'", "'./deploy/relkit', 'notices'"} {
		if !strings.Contains(ps, want) {
			t.Errorf("build.ps1 lacks %s", want)
		}
	}
	var zipCopy string
	for _, l := range strings.Split(ps, "\n") {
		if strings.Contains(l, "Copy-Item") && strings.Contains(l, "$folder") && strings.Contains(l, "*.exe") {
			zipCopy = l
		}
	}
	if !strings.Contains(zipCopy, "LICENSE.txt") || !strings.Contains(zipCopy, "THIRD-PARTY-NOTICES.txt") {
		t.Errorf("the portable ZIP copy does not include the notices: %q", zipCopy)
	}
	files := strings.Join(issSection(t, read(t, issFile), "Files"), "\n")
	for _, f := range []string{`\LICENSE.txt"`, `\THIRD-PARTY-NOTICES.txt"`} {
		if !strings.Contains(files, f) {
			t.Errorf("%s [Files] lacks %s", issFile, f)
		}
	}
}
