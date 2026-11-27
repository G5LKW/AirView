package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/feederui"
	"airview/internal/model"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingFileIsDefaults(t *testing.T) {
	lc, err := loadConfig(loadOptions{File: filepath.Join(t.TempDir(), "feeder.json")})
	if err != nil || lc.Exists {
		t.Fatalf("err %v exists %v", err, lc.Exists)
	}
	if len(lc.Eff.Receivers) != 1 || lc.Eff.Receivers[0].Input.Type != model.FeedAuto || lc.Eff.Receivers[0].Input.ID != "input-1" {
		t.Fatalf("defaults: %+v", lc.Eff.Receivers)
	}
	if lc.Eff.UI.Addr != model.DefaultFeederUIAddr || lc.Eff.Results.BeastPort != model.DefaultResultsPort {
		t.Fatalf("ui/results defaults: %+v %+v", lc.Eff.UI, lc.Eff.Results)
	}
}

func TestHandWrittenConfigJSONC(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "feeder.json", `{
  // a comment
  "receivers": [ {"name": "loft", "input": {"type": "beast", "host": "192.168.1.20"}},
                 {"input": {"type": "auto"}, "located": true, "lat": 51.5, "lon": -0.1, "altM": 30} ],
  "uplinks": [ {"id": "club", "host": "feed.example.org"} ],
  "ui": {"adrr": "x"},
  "results": {"beastPort": 0},
}`)
	lc, err := loadConfig(loadOptions{File: p})
	if err != nil {
		t.Fatal(err)
	}
	if !lc.Comment {
		t.Error("comment not noticed")
	}
	var unknown bool
	for _, is := range lc.Issues {
		if strings.Contains(is.Msg, "unknown setting") && is.Path == "ui.adrr" && strings.Contains(is.Msg, `"addr"`) {
			unknown = true
		}
		if is.Error {
			t.Errorf("unexpected error %s", is)
		}
	}
	if !unknown {
		t.Errorf("no unknown-key warning: %v", lc.Issues)
	}
	rs := lc.Eff.Receivers
	if len(rs) != 2 {
		t.Fatalf("receivers %+v", rs)
	}
	if !rs[0].Input.Enabled || rs[0].RxNo != 1 || rs[0].Input.Port != 30005 || rs[0].Input.ID != "rx1" || rs[0].Name != "loft" {
		t.Fatalf("receiver 0 %+v", rs[0])
	}
	if !rs[1].Input.Enabled || rs[1].RxNo != 2 || rs[1].Input.ID != "rx2" || rs[1].AltRef != model.AltRefMSL || !rs[1].Located {
		t.Fatalf("receiver 1 %+v", rs[1])
	}
	u := lc.Eff.Uplinks[0]
	if u.Port != 30160 || !u.Timestamps || !u.SendLocation || u.Pin != model.PinHidden {
		t.Fatalf("uplink defaults %+v", u)
	}
	if lc.Eff.Results.BeastPort != 0 {
		t.Fatal("beastPort 0 not kept")
	}
}

func TestConfigErrors(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "feeder.json", `{"uplinks": [ {"id": "a", "host": 5} ]}`)
	lc, err := loadConfig(loadOptions{File: p})
	if err == nil || lc.errorCount() == 0 {
		t.Fatalf("type error not reported: %v %v", err, lc.Issues)
	}
	p = writeFile(t, dir, "feeder.json", "{\n \"uplinks\": [\n")
	lc, err = loadConfig(loadOptions{File: p})
	if err == nil || !strings.Contains(lc.Issues[0].Source, "feeder.json:") {
		t.Fatalf("syntax error position: %v %v", err, lc.Issues)
	}
	// still usable defaults
	if len(lc.Eff.Receivers) != 1 {
		t.Fatal("no defaults after a syntax error")
	}
}

func TestEnvAndSetOverrides(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "feeder.json", `{"uplinks":[{"id":"club","host":"a.example","enrolToken":"filetok"}]}`)
	secret := writeFile(t, dir, "tok", "envtok\n")
	env := []string{
		"AIRVIEW_FEEDER_UPLINKS_0_HOST=b.example",
		"AIRVIEW_FEEDER_UPLINKS_0_ENROL_TOKEN_FILE=" + secret,
		"AIRVIEW_FEEDER_UPLINKS_1_HOST=c.example", // appends a second uplink
		"AIRVIEW_FEEDER_UPLINKS_1_ID=second",
		"AIRVIEW_FEEDER_UPLINK_TUNING_RING_SEC=120",
		"AIRVIEW_FEEDER_RESULTS_BEAST_PORT=0",
		"AIRVIEW_FEEDER_UPLINKS_0_INPUTS=rx1,rx2",
		"AIRVIEW_FEEDER_RECEIVERS_0_SHARE=1", // unknown
		"AIRVIEW_FEEDER_UI_ADDR=127.0.0.1:9000",
		"AIRVIEW_FEEDER_DATA=/ignored", // not a setting
		"PATH=/bin",
	}
	lc, err := loadConfig(loadOptions{File: p, Environ: env, Sets: []string{"receivers[0].name=roof", "uplinks[0].port=31000", "bogus=1"}})
	if err != nil {
		t.Fatal(err)
	}
	e := lc.Eff
	if len(e.Uplinks) != 2 || e.Uplinks[0].Host != "b.example" || e.Uplinks[0].EnrolToken != "envtok" || e.Uplinks[1].Host != "c.example" || e.Uplinks[1].ID != "second" {
		t.Fatalf("uplinks %+v", e.Uplinks)
	}
	if e.Uplinks[0].Port != 31000 || len(e.Uplinks[0].Inputs) != 2 || e.UplinkTuning.RingSec != 120 || e.Results.BeastPort != 0 || e.UI.Addr != "127.0.0.1:9000" || e.Receivers[0].Name != "roof" {
		t.Fatalf("overrides not applied: %+v", e)
	}
	// the file layer is untouched
	if lc.FileCfg.Uplinks[0].Host != "a.example" || lc.FileCfg.Uplinks[0].EnrolToken != "filetok" {
		t.Fatalf("file layer changed: %+v", lc.FileCfg.Uplinks)
	}
	var unknownEnv, badSet bool
	for _, is := range lc.Issues {
		if strings.Contains(is.Source, "RECEIVERS_0_SHARE") {
			unknownEnv = true
		}
		if is.Source == "-set bogus" && is.Error {
			badSet = true
		}
		if strings.Contains(is.Source, "AIRVIEW_FEEDER_DATA") {
			t.Errorf("non-setting variable reported: %s", is)
		}
	}
	if !unknownEnv || !badSet {
		t.Fatalf("issues %v", lc.Issues)
	}
	if len(lc.Locked) < 5 {
		t.Fatalf("locked %v", lc.Locked)
	}
}

func TestResolveEnvNames(t *testing.T) {
	for name, want := range map[string]string{
		"UI_PASSWORD_HASH":                 "ui.passwordHash",
		"UPLINKS_3_SHARE_GNSS":             "uplinks[3].shareGnss",
		"UPLINK_TUNING_BACKFILL_MIN_K_BPS": "uplinkTuning.backfillMinKBps",
		"RECEIVERS_0_INPUT_HOST":           "receivers[0].input.host",
		"RECEIVERS_0_AGL_M":                "receivers[0].aglM",
		"UPLINKS":                          "uplinks",
	} {
		p, _, ok := resolveEnv(name)
		if !ok || pathString(p) != want {
			t.Errorf("%s -> %v %v, want %s", name, pathString(p), ok, want)
		}
		if ok && envName(p) != envPrefix+name {
			t.Errorf("envName(%s) = %s", want, envName(p))
		}
	}
	for _, bad := range []string{"VERSION", "MODE", "UPLINKS_X_HOST", "NOPE"} {
		if _, _, ok := resolveEnv(bad); ok {
			t.Errorf("%s resolved", bad)
		}
	}
}

func TestParsePath(t *testing.T) {
	for in, want := range map[string]string{"uplinks[0].host": "uplinks[0].host", "uplinks.0.host": "uplinks[0].host", "ui.addr": "ui.addr"} {
		p, ok := parsePath(in)
		if !ok || pathString(p) != want {
			t.Errorf("%s -> %s", in, pathString(p))
		}
	}
	if _, _, err := settingPath("uplinks[0].nope"); err == nil {
		t.Error("unknown path accepted")
	}
	if _, _, err := settingPath("version"); err == nil {
		t.Error("version is settable")
	}
}

func TestSaveRedactMerge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "feeder.json")
	cfg := model.DefaultFeederConfig()
	u := model.DefaultUplink()
	u.ID, u.Host, u.EnrolToken = "club", "x.example", "tok"
	cfg.Uplinks = []model.Uplink{u}
	cfg.UI.PasswordHash = hashPassword("pw")
	if err := saveConfig(p, cfg); err != nil {
		t.Fatal(err)
	}
	lc, err := loadConfig(loadOptions{File: p})
	if err != nil || lc.Eff.Uplinks[0].EnrolToken != "tok" {
		t.Fatalf("round trip: %v %+v", err, lc.Eff.Uplinks)
	}
	red := redactConfig(lc.FileCfg, feederui.Redacted)
	if red.Uplinks[0].EnrolToken != feederui.Redacted || red.UI.PasswordHash != feederui.Redacted || lc.FileCfg.Uplinks[0].EnrolToken != "tok" {
		t.Fatal("redaction")
	}
	back := mergeSecrets(red, lc.FileCfg, feederui.Redacted)
	if back.Uplinks[0].EnrolToken != "tok" || back.UI.PasswordHash != cfg.UI.PasswordHash {
		t.Fatal("merge")
	}
	red.UI.PasswordHash = "plain"
	if h := mergeSecrets(red, lc.FileCfg, feederui.Redacted).UI.PasswordHash; !strings.HasPrefix(h, "pbkdf2-sha256$") {
		t.Fatalf("plain password not hashed: %q", h)
	}
	// SEC-11: the kept token never follows the uplink to another server.
	red2 := redactConfig(lc.FileCfg, feederui.Redacted)
	red2.Uplinks[0].Host = "attacker.example"
	if tok := mergeSecrets(red2, lc.FileCfg, feederui.Redacted).Uplinks[0].EnrolToken; tok != "" {
		t.Fatalf("enrol token carried to another host: %q", tok)
	}
	red2 = redactConfig(lc.FileCfg, feederui.Redacted)
	red2.Uplinks[0].ServerPin = ""
	red2.Uplinks[0].Port++
	if tok := mergeSecrets(red2, lc.FileCfg, feederui.Redacted).Uplinks[0].EnrolToken; tok != "" {
		t.Fatalf("enrol token carried to another port: %q", tok)
	}
}

func TestConfigCommands(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "feeder.json")
	var out, errb bytes.Buffer
	run := func(args ...string) int {
		out.Reset()
		errb.Reset()
		return runMain(args, &out, &errb)
	}
	if c := run("config", "set", "-config", cf, "-data", dir, "uplinks[0].host=agg.example", "uplinks[0].serverPin=sha256/AAAA", "receivers[0].name=roof", "ui.passwordHash=pw"); c != 0 {
		t.Fatalf("set: %d %s %s", c, out.String(), errb.String())
	}
	if c := run("config", "get", "-config", cf, "uplinks[0].host"); c != 0 || strings.TrimSpace(out.String()) != "agg.example" {
		t.Fatalf("get: %d %q", c, out.String())
	}
	if c := run("config", "check", "-config", cf); c != 0 || !strings.Contains(out.String(), "OK") {
		t.Fatalf("check: %d %s", c, out.String())
	}
	if c := run("config", "print", "-config", cf); c != 0 || strings.Contains(out.String(), "pbkdf2-sha256") || strings.Contains(out.String(), "sha256:") || !strings.Contains(out.String(), "(redacted)") {
		t.Fatalf("print leaks or fails: %d %s", c, out.String())
	}
	if c := run("config", "env"); c != 0 || !strings.Contains(out.String(), "AIRVIEW_FEEDER_UPLINKS_<n>_ENROL_TOKEN") || !strings.Contains(out.String(), "AIRVIEW_FEEDER_UI_ADDR") {
		t.Fatalf("env: %s", out.String())
	}
	if c := run("config", "example"); c != 0 {
		t.Fatal("example")
	}
	// the example parses and checks cleanly
	ex := writeFile(t, dir, "example.json", out.String())
	lc, err := loadConfig(loadOptions{File: ex})
	if err != nil {
		t.Fatalf("example: %v %v", err, lc.Issues)
	}
	for _, is := range lc.Issues {
		t.Errorf("example issue: %s", is)
	}
	writeFile(t, dir, "bad.json", `{"ui": 3}`)
	if c := run("config", "check", "-config", filepath.Join(dir, "bad.json")); c != 1 {
		t.Fatalf("check bad: %d %s", c, out.String())
	}
}
