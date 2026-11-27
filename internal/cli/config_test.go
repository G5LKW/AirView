package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/config"
	"airview/internal/model"
)

func run(t *testing.T, environ []string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	exe := t.TempDir()
	code := RunConfig(args, Env{Stdout: &out, Stderr: &errb, Environ: environ, ExeDir: exe, UserConfigDir: filepath.Join(exe, "none")})
	return code, out.String(), errb.String()
}

func TestUsage(t *testing.T) {
	if code, _, e := run(t, nil); code != ExitUsage || !strings.Contains(e, "Usage") {
		t.Fatal("no args")
	}
	if code, _, _ := run(t, nil, "frobnicate"); code != ExitUsage {
		t.Fatal("unknown command")
	}
	if code, o, _ := run(t, nil, "help"); code != ExitOK || !strings.Contains(o, "check") {
		t.Fatal("help")
	}
	if code, _, _ := run(t, nil, "check", "extra"); code != ExitUsage {
		t.Fatal("extra arg")
	}
	if code, _, _ := run(t, nil, "check", "-bogus"); code != ExitUsage {
		t.Fatal("bad flag")
	}
	if !IsConfigCommand([]string{"config", "check"}) || IsConfigCommand([]string{"-headless"}) || IsConfigCommand(nil) {
		t.Fatal("IsConfigCommand")
	}
}

func TestCheckCommand(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := run(t, nil, "check", "-data", dir)
	if code != ExitOK || !strings.Contains(out, "configuration is valid: 0 error(s)") || !strings.Contains(out, "not found") {
		t.Fatalf("defaults: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); err == nil {
		t.Fatal("check created config.json")
	}

	op := filepath.Join(dir, "op.jsonc")
	if err := os.WriteFile(op, []byte("{\n  \"http\": {\"port\": \"x\"}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, []string{"AIRVIEW_DATA=" + dir, "AIRVIEW_CONFIG=" + op, "AIRVIEW_NOPE=1"}, "check")
	if code != ExitError || !strings.Contains(out, "op.jsonc:2:20: http.port: expected an integer") ||
		!strings.Contains(out, "warning: env AIRVIEW_NOPE") || !strings.Contains(out, "1 error(s), 1 warning(s)") {
		t.Fatalf("errors: %d\n%s", code, out)
	}
	code, out, _ = run(t, []string{"AIRVIEW_NOPE=1"}, "check", "-data", dir, "-config", op, "-q")
	if code != ExitError || strings.Contains(out, "warning") || strings.Count(out, "\n") != 1 {
		t.Fatalf("quiet: %s", out)
	}
	code, out, _ = run(t, []string{"AIRVIEW_NOPE=1"}, "check", "-data", dir, "-config", op, "-no-env", "-json")
	var rep struct {
		Valid  bool
		Errors int
		Issues []struct {
			Severity, Source, Path, Message string
			Line, Column                    int
		}
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if code != ExitError || rep.Valid || rep.Errors != 1 || len(rep.Issues) != 1 || rep.Issues[0].Line != 2 || rep.Issues[0].Column != 20 || rep.Issues[0].Path != "http.port" {
		t.Fatalf("json report: %+v", rep)
	}
	code, _, _ = run(t, nil, "check", "-data", dir, "-no-config", "-set", "http.port=8080")
	if code != ExitOK {
		t.Fatal("-no-config / -set")
	}
}

func TestPrintCommand(t *testing.T) {
	dir := t.TempDir()
	c := model.DefaultConfig()
	c.Hub.Password = "topsecret"
	if err := config.Save(dir, c); err != nil {
		t.Fatal(err)
	}
	env := []string{"AIRVIEW_HTTP_PORT=8123", "AIRVIEW_UPLINKS_0_HOST=agg.example.org", "AIRVIEW_UPLINKS_0_ENROL_TOKEN=tok"}
	code, out, _ := run(t, env, "print", "-data", dir)
	if code != ExitOK || strings.Contains(out, "topsecret") || strings.Contains(out, `"tok"`) {
		t.Fatalf("secrets leaked or failed: %d\n%s", code, out)
	}
	var got model.Config
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.HTTP.Port != 8123 || got.Hub.Password != "set" || got.Uplinks[0].EnrolToken != "set" {
		t.Fatalf("print: %+v %+v", got.HTTP, got.Hub)
	}
	code, out, _ = run(t, env, "print", "-data", dir, "-sources")
	if code != ExitOK || !strings.Contains(out, "// from env AIRVIEW_HTTP_PORT\n    \"port\": 8123") ||
		!strings.Contains(out, "// from env AIRVIEW_UPLINKS_0_ENROL_TOKEN") {
		t.Fatalf("sources:\n%s", out)
	}
	// The annotated output is itself a valid operator file.
	root, err := config.ParseJSONC([]byte(out))
	if err != nil || root.Kind != config.ObjectNode {
		t.Fatalf("annotated output does not parse: %v", err)
	}
	if strings.Index(out, `"mode"`) > strings.Index(out, `"http"`) {
		t.Fatal("field order lost")
	}
}

func TestExampleAndEnvCommands(t *testing.T) {
	code, out, _ := run(t, nil, "example")
	if code != ExitOK || out != config.ExampleJSONC() {
		t.Fatal("example")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "ex.jsonc")
	if code, _, _ := run(t, nil, "example", "-o", file); code != ExitOK {
		t.Fatal("example -o")
	}
	if code, _, e := run(t, nil, "example", "-o", file); code != ExitError || e == "" {
		t.Fatal("example -o must not overwrite")
	}
	// The written example checks clean.
	if code, out, _ := run(t, nil, "check", "-data", dir, "-config", file); code != ExitOK || !strings.Contains(out, "0 error(s), 0 warning(s)") {
		t.Fatalf("example does not check clean:\n%s", out)
	}
	code, out, _ = run(t, nil, "env")
	if code != ExitOK || !strings.Contains(out, "AIRVIEW_HTTP_PORT") || !strings.Contains(out, "uplinks[].enrolToken (string, secret)") {
		t.Fatalf("env:\n%s", out)
	}
	code, out, _ = run(t, nil, "env", "-json")
	var rows []map[string]any
	if code != ExitOK || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != len(config.Fields()) {
		t.Fatal("env -json")
	}
}

func TestFindDataDir(t *testing.T) {
	exe := t.TempDir()
	ucd := t.TempDir()
	env := Env{ExeDir: exe, UserConfigDir: ucd}
	if got := findDataDir("", env); got != filepath.Join(exe, "data") {
		t.Fatalf("fallback: %s", got)
	}
	if err := os.MkdirAll(filepath.Join(ucd, model.AppName), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findDataDir("", env); got != filepath.Join(ucd, model.AppName) {
		t.Fatalf("user dir: %s", got)
	}
	if err := os.MkdirAll(filepath.Join(exe, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findDataDir("", env); got != filepath.Join(exe, "data") {
		t.Fatalf("exe dir: %s", got)
	}
	env.Environ = []string{"AIRVIEW_DATA=" + ucd}
	if got := findDataDir("", env); got != ucd {
		t.Fatalf("env: %s", got)
	}
	if got := findDataDir(exe, env); got != exe {
		t.Fatalf("flag: %s", got)
	}
}
