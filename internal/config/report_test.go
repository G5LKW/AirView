package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/model"
)

func findIssue(r Report, sev Severity, source string, line, col int, path, msg string) bool {
	for _, is := range r.Issues {
		if is.Severity == sev && strings.HasSuffix(is.Source, source) && is.Pos.Line == line && is.Pos.Col == col &&
			is.Path == path && strings.Contains(is.Msg, msg) {
			return true
		}
	}
	return false
}

func TestCheckReportsPreciseErrors(t *testing.T) {
	dir := t.TempDir()
	op := filepath.Join(dir, OperatorFileName)
	src := `{
  "mode": "aggregator",
  "http": {
    "port": "8080",
    "bindAdr": "127.0.0.1",
    "tileCache": "true"
  },
  "Receiver": {"name": "x"},
  "ui": {"rangeRingsKm": [100, "200"]},
  "hub": {"port": 70000},
  "aggregator": {
    "admission": "everyone",
    "limits": {"maxStations": 2.5},
    "mlat": {"holdout": 0.9},
  },
  "uplinks": [{"id": "a", "host": "h.example.org", "port": 0, "inputs": ["nofeed"]}],
  "demo": {"aircraft": 50, "aircraft": 60},
}`
	if err := os.WriteFile(op, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Check(LoadOptions{Dir: dir})
	checks := []struct {
		line, col int
		path, msg string
	}{
		{4, 13, "http.port", "expected an integer, found a string (remove the quotes)"},
		{5, 5, "http.bindAdr", `unknown setting (did you mean "bindAddr"?)`},
		{6, 18, "http.tileCache", "write true or false without quotes"},
		{8, 3, "Receiver", `did you mean "receiver"? names are case-sensitive`},
		{9, 32, "ui.rangeRingsKm[1]", "expected an integer"},
		{10, 11, "hub.port", "not a valid port"},
		{12, 5, "aggregator.admission", "is not one of open/approve/invite"},
		{13, 31, "aggregator.limits.maxStations", "expected an integer, found 2.5"},
		{14, 14, "aggregator.mlat.holdout", "out of range"},
		{16, 52, "uplinks[0].port", "out of range"},
		{16, 63, "uplinks[0].inputs", "does not match any feed"},
		{17, 28, "demo.aircraft", "duplicate setting (first given at line 17)"},
	}
	for _, c := range checks {
		if !findIssue(r, SevError, OperatorFileName, c.line, c.col, c.path, c.msg) {
			t.Errorf("missing %d:%d %s %q", c.line, c.col, c.path, c.msg)
		}
	}
	if t.Failed() {
		t.Log("\n" + issuesText(r.Issues))
	}
	if r.Errors() != len(checks) || r.Warnings() != 1 { // the plain-HTTP login warning (SEC-01)
		t.Fatalf("errors %d warnings %d\n%s", r.Errors(), r.Warnings(), issuesText(r.Issues))
	}
	if r.Effective.Mode != model.ModeAggregator || r.Effective.Demo.Aircraft != 60 {
		t.Fatalf("effective: %s %d", r.Effective.Mode, r.Effective.Demo.Aircraft)
	}
	// Nothing was written.
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("check wrote files: %v", ents)
	}
}

func TestCheckConfigJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	// Unknown keys in config.json are warnings (a newer AirView may have
	// written them); out-of-range values are errors located in the file.
	if err := os.WriteFile(path, []byte("{\n  \"version\": 1,\n  \"futureThing\": 1,\n  \"storage\": {\"trailRetentionDays\": 99999}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Check(LoadOptions{Dir: dir})
	if !findIssue(r, SevWarning, FileName, 3, 3, "futureThing", "unknown setting") ||
		!findIssue(r, SevError, FileName, 4, 15, "storage.trailRetentionDays", "above the maximum") {
		t.Fatalf("\n%s", issuesText(r.Issues))
	}
	if !r.ConfigJSONFound || r.Errors() != 1 {
		t.Fatalf("errors %d", r.Errors())
	}
	// A setting overridden by the environment is attributed to the variable.
	r = Check(LoadOptions{Dir: dir, Environ: []string{"AIRVIEW_STORAGE_TRAIL_RETENTION_DAYS=5000"}})
	if r.Errors() != 1 || !findIssue(r, SevError, "env AIRVIEW_STORAGE_TRAIL_RETENTION_DAYS", 0, 0, "storage.trailRetentionDays", "above the maximum") {
		t.Fatalf("\n%s", issuesText(r.Issues))
	}

	// Syntax error in config.json.
	if err := os.WriteFile(path, []byte("{\n  \"http\": {\"port\": 80}\n  \"ui\": {}\n}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = Check(LoadOptions{Dir: dir})
	if !findIssue(r, SevError, FileName, 3, 3, "", "comma missing") {
		t.Fatalf("\n%s", issuesText(r.Issues))
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"ui"`) {
		t.Fatal("check must not move the damaged file")
	}

	// Defaults (no files) are valid.
	r = Check(LoadOptions{Dir: t.TempDir()})
	if len(r.Issues) != 0 || r.ConfigJSONFound {
		t.Fatalf("defaults: %v", r.Issues)
	}
}

func TestConfigJSONWithCommentsIsNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "{\n  // hand edit\n  \"http\": {\"port\": 8088,},\n}\n")
	cfg, warns, err := LoadWithWarnings(dir)
	if err != nil || cfg.HTTP.Port != 8088 {
		t.Fatalf("%v %d", err, cfg.HTTP.Port)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "comments") {
		t.Fatalf("warnings: %v", warns)
	}
	if strings.Contains(readFile(t, dir), "//") {
		t.Fatal("expected the canonical re-save")
	}
}

func TestValidateNetworkSettings(t *testing.T) {
	c := model.DefaultConfig()
	c.Mode = model.ModeAggregator
	got, notes := Validate(c)
	if got.Mode != model.ModeAggregator || len(notes) != 0 {
		t.Fatalf("aggregator mode: %s %v", got.Mode, notes)
	}
	c.Mode = model.ModeFeeder
	if got, _ := Validate(c); got.Mode != model.ModeStandalone {
		t.Fatal("feeder mode must not be accepted by the main app")
	}

	c = model.DefaultConfig()
	c.Feeds[0].ClockClass = "atomic"
	c.Uplinks = []model.Uplink{
		{ID: "A b", Host: "https://agg.example.org:31000/x", Port: model.DefaultAVFPort, Inputs: []string{"Beast", "beast", "ghost"}, EnrolToken: "t"},
		{ID: "a-b", Host: "bad host!", Port: 1},
	}
	c.Aggregator = model.Aggregator{} // zero block -> defaults
	got, notes = Validate(c)
	if got.Feeds[0].ClockClass != "" || !containsSub(notes, "clockClass") {
		t.Fatalf("clockClass: %v", notes)
	}
	u := got.Uplinks
	if u[0].Host != "agg.example.org" || u[0].Port != 31000 || len(u[0].Inputs) != 1 || u[0].Inputs[0] != "beast" || u[0].ID != "uplink" {
		t.Fatalf("uplink 0: %+v %v", u[0], notes)
	}
	if u[1].Host != "" || !u[1].Disabled || u[1].ID != "a-b" {
		t.Fatalf("uplink 1: %+v", u[1])
	}
	if !got.Aggregator.Equal(model.DefaultAggregator()) {
		t.Fatal("zero aggregator block not defaulted")
	}
	again, notes2 := Validate(got)
	if len(notes2) != 0 || !Equal(again, got) {
		t.Fatalf("not idempotent: %v", notes2)
	}
	c.Uplinks = []model.Uplink{}
	if got, _ := Validate(c); got.Uplinks != nil {
		t.Fatal("empty uplinks must become nil")
	}
}

func TestNetworkSecrets(t *testing.T) {
	c := model.DefaultConfig()
	c.Uplinks = []model.Uplink{model.DefaultUplink(), model.DefaultUplink()}
	c.Uplinks[0].ID, c.Uplinks[0].Host, c.Uplinks[0].EnrolToken = "a", "a.example.org", "tokA"
	c.Uplinks[1].ID, c.Uplinks[1].Host, c.Uplinks[1].EnrolToken = "b", "b.example.org", "tokB"
	c.Aggregator.OutputToken = "out"
	c.Aggregator.TLSCert, c.Aggregator.TLSKey = "/etc/c.pem", "/etc/k.pem"
	r := Redact(c)
	if r.Uplinks[0].EnrolToken != "set" || r.Aggregator.OutputToken != "set" || r.Aggregator.TLSKey != "set" ||
		r.Aggregator.TLSCert != "/etc/c.pem" || c.Uplinks[0].EnrolToken != "tokA" {
		t.Fatalf("redact: %+v %+v", r.Uplinks, r.Aggregator)
	}
	// Round trip through the UI: reordered uplinks keep their own tokens.
	in := r
	in.Uplinks = []model.Uplink{r.Uplinks[1], r.Uplinks[0], model.DefaultUplink()}
	in.Uplinks[2].ID, in.Uplinks[2].Host, in.Uplinks[2].EnrolToken = "c", "c.example.org", "plain:set"
	m := MergeSecrets(in, c)
	if m.Uplinks[0].EnrolToken != "tokB" || m.Uplinks[1].EnrolToken != "tokA" || m.Uplinks[2].EnrolToken != "set" ||
		m.Aggregator.OutputToken != "out" || m.Aggregator.TLSKey != "/etc/k.pem" {
		t.Fatalf("merge: %+v %+v", m.Uplinks, m.Aggregator)
	}
	in.Aggregator.OutputToken = "clear"
	if MergeSecrets(in, c).Aggregator.OutputToken != "" {
		t.Fatal("clear")
	}
	// Deep copy of the new slices.
	cl := Clone(c)
	cl.Uplinks[0].Inputs = append(cl.Uplinks[0].Inputs, "x")
	cl.Aggregator.TrustedProxies = append(cl.Aggregator.TrustedProxies, "10.0.0.0/8")
	if len(c.Uplinks[0].Inputs) != 0 || len(c.Aggregator.TrustedProxies) != 0 {
		t.Fatal("Clone aliases network slices")
	}
}

func TestCheckHubPasswordInAggregatorMode(t *testing.T) {
	dir := t.TempDir()
	sets := []string{"mode=aggregator", "hub.enabled=true"}
	r := Check(LoadOptions{Dir: dir, Sets: sets})
	if !findIssue(r, SevWarning, "", 0, 0, "hub.password", "every login is refused") {
		t.Fatalf("\n%s", issuesText(r.Issues))
	}
	r = Check(LoadOptions{Dir: dir, Sets: append(sets, "hub.password=secret")})
	for _, is := range r.Issues {
		if is.Path == "hub.password" {
			t.Fatalf("warning with a password: %v", is)
		}
	}
}

// C5: an env-only list element is not blamed on config.json, and filling
// its id with the default is a warning, not an error.
func TestCheckEnvOnlyUplink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"mode":"standalone"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Check(LoadOptions{Dir: dir, Environ: []string{"AIRVIEW_UPLINKS_0_HOST=agg.example.org"}})
	if r.Errors() != 0 {
		t.Fatalf("errors: %s", issuesText(r.Issues))
	}
	for _, is := range r.Issues {
		if strings.Contains(is.Source, FileName) && strings.HasPrefix(is.Path, "uplinks") {
			t.Errorf("blamed on config.json: %+v", is)
		}
	}
	if len(r.Effective.Uplinks) != 1 || r.Effective.Uplinks[0].Host != "agg.example.org" {
		t.Fatalf("uplink: %+v", r.Effective.Uplinks)
	}
}

// SO-3: http.port and the feeder listener ports are one namespace.
func TestCheckHTTPPortCollision(t *testing.T) {
	dir := t.TempDir()
	src := `{"mode":"aggregator","http":{"port":18564},"aggregator":{"avfPort":18564}}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Check(LoadOptions{Dir: dir})
	if !findIssue(r, SevError, FileName, 1, 58, "aggregator.avfPort", "already used by http.port") {
		t.Fatalf("collision not reported:\n%s", issuesText(r.Issues))
	}
}
