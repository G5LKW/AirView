package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"airview/internal/model"
)

func TestEnvNamesAreUniqueAndResolveBack(t *testing.T) {
	seen := map[string]string{}
	for _, f := range Fields() {
		if prev, dup := seen[f.Env]; dup {
			t.Fatalf("%s is used by %s and %s", f.Env, prev, f.Path)
		}
		seen[f.Env] = f.Path
		name := strings.ReplaceAll(f.Env, "<n>", "3")
		segs, typ, ok := resolveEnv(name)
		if !ok {
			t.Errorf("%s does not resolve", name)
			continue
		}
		if got, want := segsString(segs), strings.ReplaceAll(f.Path, "[]", "[3]"); got != want {
			t.Errorf("%s resolves to %s, want %s", name, got, want)
		}
		if typ != f.Type {
			t.Errorf("%s: type %v, want %v", name, typ, f.Type)
		}
		if knownNonSettingEnv[f.Env] {
			t.Errorf("%s collides with a reserved variable", f.Env)
		}
		env, ok := EnvName(strings.ReplaceAll(f.Path, "[]", "[3]"))
		if !ok || env != name {
			t.Errorf("EnvName(%s) = %s, want %s", f.Path, env, name)
		}
	}
	for name, want := range map[string]string{
		"AIRVIEW_HTTP_PORT":                               "http.port",
		"AIRVIEW_HTTP_BIND_ADDR":                          "http.bindAddr",
		"AIRVIEW_UI_AIRCRAFT_TTL_S":                       "ui.aircraftTtlS",
		"AIRVIEW_AGGREGATOR_LIMITS_MAX_K_BPS_PER_STATION": "aggregator.limits.maxKBpsPerStation",
		"AIRVIEW_AGGREGATOR_MLAT_AUTO_USE_SURVEY":         "aggregator.mlat.autoUseSurvey",
		"AIRVIEW_UPLINKS_0_ENROL_TOKEN":                   "uplinks[0].enrolToken",
		"AIRVIEW_UPLINKS_12":                              "uplinks[12]",
		"AIRVIEW_FEEDS":                                   "feeds",
		"AIRVIEW_ANTENNAS_1_GAIN_DBI":                     "antennas[1].gainDbi",
	} {
		segs, _, ok := resolveEnv(name)
		if !ok || segsString(segs) != want {
			t.Errorf("%s -> %v %v, want %s", name, segsString(segs), ok, want)
		}
	}
	for _, bad := range []string{"AIRVIEW_HTTP", "AIRVIEW_AGGREGATOR", "AIRVIEW_UPLINKS_X_HOST", "AIRVIEW_UPLINKS_01_HOST", "AIRVIEW_NOPE", "AIRVIEW_VERSION", "HTTP_PORT"} {
		if _, _, ok := resolveEnv(bad); ok {
			t.Errorf("%s should not resolve", bad)
		}
	}
}

func TestEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "hubpw")
	if err := os.WriteFile(secretFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := []string{
		"PATH=/bin",
		"AIRVIEW_HTTP_PORT=8080",
		"AIRVIEW_HTTP_BIND_ADDR=127.0.0.1",
		"AIRVIEW_HTTP_TILE_CACHE=off",
		"AIRVIEW_MODE=aggregator",
		"AIRVIEW_RECEIVER_NAME=Env Station",
		"AIRVIEW_RECEIVER_LAT=51.5",
		"AIRVIEW_UI_RANGE_RINGS_KM=50, 150",
		"AIRVIEW_HUB_PASSWORD_FILE=" + secretFile,
		"AIRVIEW_AGGREGATOR_LIMITS_MAX_STATIONS=1500",
		"AIRVIEW_AGGREGATOR_MLAT_HOLDOUT=0.2",
		"AIRVIEW_AGGREGATOR_TRUSTED_PROXIES=[\"10.0.0.0/8\", \"192.168.1.1\"]",
		"AIRVIEW_UPLINKS_1_HOST=b.example.org", // after _0 although sorted later as a string
		"AIRVIEW_UPLINKS_0_HOST=a.example.org",
		"AIRVIEW_UPLINKS_0_ENROL_TOKEN=tok0",
		"AIRVIEW_FEEDS_0_PORT=30006",
		"AIRVIEW_HTTP_PORT=8081", // last one wins
		"AIRVIEW_DEBUG=1",        // reserved, ignored silently
	}
	cfg, l, warns, err := LoadLayered(LoadOptions{Dir: dir, Environ: environ})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Issues()) != 0 {
		t.Fatalf("issues: %v (warnings %v)", l.Issues(), warns)
	}
	if cfg.HTTP.Port != 8081 || cfg.HTTP.BindAddr != "127.0.0.1" || cfg.HTTP.TileCache {
		t.Fatalf("http: %+v", cfg.HTTP)
	}
	if cfg.Mode != model.ModeAggregator || cfg.Receiver.Name != "Env Station" || cfg.Receiver.Lat != 51.5 {
		t.Fatalf("mode/receiver: %s %+v", cfg.Mode, cfg.Receiver)
	}
	if !reflect.DeepEqual(cfg.UI.RangeRingsKm, []int{50, 150}) {
		t.Fatalf("rings: %v", cfg.UI.RangeRingsKm)
	}
	if cfg.Hub.Password != "s3cret" {
		t.Fatalf("hub password from file: %q", cfg.Hub.Password)
	}
	if cfg.Aggregator.Limits.MaxStations != 1500 || cfg.Aggregator.MLAT.Holdout != 0.2 {
		t.Fatalf("aggregator: %+v", cfg.Aggregator.Limits)
	}
	if !reflect.DeepEqual(cfg.Aggregator.TrustedProxies, []string{"10.0.0.0/8", "192.168.1.1/32"}) {
		t.Fatalf("proxies: %v", cfg.Aggregator.TrustedProxies)
	}
	if len(cfg.Uplinks) != 2 || cfg.Uplinks[0].Host != "a.example.org" || cfg.Uplinks[1].Host != "b.example.org" ||
		cfg.Uplinks[0].EnrolToken != "tok0" || cfg.Uplinks[0].Port != model.DefaultAVFPort || !cfg.Uplinks[0].Timestamps {
		t.Fatalf("uplinks: %+v", cfg.Uplinks)
	}
	if cfg.Feeds[0].Port != 30006 || cfg.Feeds[0].ID != model.DefaultFeedID {
		t.Fatalf("feed: %+v", cfg.Feeds[0])
	}
	if !l.Locked("http.port") || !l.Locked("uplinks[0].host") || l.Locked("http.adminPasswordHash") || !l.Locked("aggregator.trustedProxies") {
		t.Fatal("Locked")
	}

	// Nothing from the environment reaches config.json.
	saved, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(saved, model.DefaultConfig()) {
		t.Fatalf("environment leaked into config.json: %+v", saved.HTTP)
	}
}

func TestEnvErrors(t *testing.T) {
	dir := t.TempDir()
	environ := []string{
		"AIRVIEW_HTTP_PORT=eighty",
		"AIRVIEW_HTTP_TILE_CACHE=maybe",
		"AIRVIEW_TERRAIN_K_FACTOR=NaN",
		"AIRVIEW_UPLINKS=[{\"id\": 5}]",
		"AIRVIEW_UPLINKS_3_HOST=x.example.org", // skips elements
		"AIRVIEW_HTPP_PORT=1",                  // typo
		"AIRVIEW_HUB_PASSWORD_FILE=" + filepath.Join(dir, "missing"),
		"AIRVIEW_UI_RANGE_RINGS_KM=1,x",
	}
	rep := Check(LoadOptions{Dir: dir, Environ: environ})
	want := map[string]string{
		"env AIRVIEW_HTTP_PORT":         "not an integer",
		"env AIRVIEW_HTTP_TILE_CACHE":   "not a boolean",
		"env AIRVIEW_TERRAIN_K_FACTOR":  "not a number",
		"env AIRVIEW_UPLINKS":           "uplinks[0].id: expected a string",
		"env AIRVIEW_UPLINKS_3_HOST":    "skips elements",
		"env AIRVIEW_HTPP_PORT":         "not a known AirView setting",
		"env AIRVIEW_HUB_PASSWORD_FILE": "cannot read the value file",
		"env AIRVIEW_UI_RANGE_RINGS_KM": "item 2",
	}
	for src, msg := range want {
		found := false
		for _, is := range rep.Issues {
			if (is.Source == src || strings.HasPrefix(is.Msg, src)) && strings.Contains(is.String(), msg) {
				found = true
			}
		}
		if !found {
			t.Errorf("no issue %s ... %s in\n%s", src, msg, issuesText(rep.Issues))
		}
	}
	if rep.Errors() < 7 || rep.Warnings() != 1 {
		t.Fatalf("errors %d warnings %d\n%s", rep.Errors(), rep.Warnings(), issuesText(rep.Issues))
	}
	if rep.Effective.HTTP.Port != 80 {
		t.Fatalf("bad value applied: %d", rep.Effective.HTTP.Port)
	}
}

func issuesText(is []Issue) string {
	var b strings.Builder
	for _, i := range is {
		b.WriteString(i.String() + "\n")
	}
	return b.String()
}

func TestOperatorFileOverridesAndCommit(t *testing.T) {
	dir := t.TempDir()
	base := model.DefaultConfig()
	base.Receiver.Name = "From UI"
	base.HTTP.Port = 8000
	base.Hub.Password = "uipw"
	if err := Save(dir, base); err != nil {
		t.Fatal(err)
	}
	op := `// operator settings
{
  "http": { "port": 9000, },          // pinned port
  "hub": { "enabled": true, "password": "oppw" },
  "uplinks": [ { "id": "club", "host": "agg.example.org", "enrolToken": "T1" } ],
  "aggregator": { "limits": { "maxStations": 3000 } },
}`
	if err := os.WriteFile(filepath.Join(dir, OperatorFileName), []byte(op), 0o600); err != nil {
		t.Fatal(err)
	}
	eff, l, _, err := LoadLayered(LoadOptions{Dir: dir, Environ: []string{"AIRVIEW_AGGREGATOR_LIMITS_MAX_STATIONS=4000"}})
	if err != nil || len(l.Issues()) != 0 {
		t.Fatalf("%v %v", err, l.Issues())
	}
	if l.File() != filepath.Join(dir, OperatorFileName) {
		t.Fatalf("file: %s", l.File())
	}
	if eff.HTTP.Port != 9000 || eff.Receiver.Name != "From UI" || !eff.Hub.Enabled || eff.Hub.Password != "oppw" {
		t.Fatalf("effective: %+v %+v %+v", eff.HTTP, eff.Receiver, eff.Hub)
	}
	if len(eff.Uplinks) != 1 || eff.Uplinks[0].EnrolToken != "T1" || !eff.Uplinks[0].ReturnMLAT {
		t.Fatalf("uplinks: %+v", eff.Uplinks)
	}
	if eff.Aggregator.Limits.MaxStations != 4000 { // env beats the file
		t.Fatalf("maxStations: %d", eff.Aggregator.Limits.MaxStations)
	}
	ov := l.Overrides()
	if len(ov) != 6 || ov[0].Path != "http.port" || !strings.HasSuffix(ov[0].Origin, OperatorFileName+":3:13") ||
		ov[5].Origin != "env AIRVIEW_AGGREGATOR_LIMITS_MAX_STATIONS" {
		t.Fatalf("overrides: %+v", ov)
	}

	// The UI changes the name (unlocked) and the port (locked): the name is
	// saved, the port keeps its config.json value on disk and the operator
	// value in effect; secrets from the operator file never reach the disk.
	ui := Redact(eff)
	ui.Receiver.Name = "Renamed"
	ui.HTTP.Port = 1234
	next, _ := ApplyUpdate(ui, eff)
	eff2, err := l.Commit(dir, next)
	if err != nil {
		t.Fatal(err)
	}
	if eff2.Receiver.Name != "Renamed" || eff2.HTTP.Port != 9000 || eff2.Hub.Password != "oppw" || len(eff2.Uplinks) != 1 {
		t.Fatalf("effective after commit: %+v %+v", eff2.Receiver, eff2.HTTP)
	}
	disk, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if disk.Receiver.Name != "Renamed" || disk.HTTP.Port != 8000 || disk.Hub.Password != "uipw" || disk.Hub.Enabled ||
		len(disk.Uplinks) != 0 || disk.Aggregator.Limits.MaxStations != 2000 {
		t.Fatalf("config.json after commit: %+v %+v %+v %d", disk.Receiver, disk.HTTP, disk.Hub, len(disk.Uplinks))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, FileName))
	if strings.Contains(string(raw), "oppw") || strings.Contains(string(raw), "T1") {
		t.Fatal("operator secrets written to config.json")
	}
	if !Equal(l.Base(), disk) {
		t.Fatal("Base() not updated")
	}

	// Factory reset keeps the overrides in force.
	eff3, err := l.FactoryReset(dir)
	if err != nil || eff3.HTTP.Port != 9000 || eff3.Receiver.Name != "My Station" {
		t.Fatalf("factory reset: %v %+v", err, eff3.HTTP)
	}
}

func TestOperatorFileProblems(t *testing.T) {
	dir := t.TempDir()
	// Explicit missing file is an error; the rest still loads.
	_, _, _, err := LoadLayered(LoadOptions{Dir: dir, File: filepath.Join(dir, "nope.jsonc"), ReadOnly: true})
	if err == nil {
		t.Fatal("missing explicit operator file accepted")
	}
	// AIRVIEW_CONFIG names the file.
	file := filepath.Join(dir, "x.jsonc")
	if err := os.WriteFile(file, []byte(`{"http": {"port": 7000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, l, _, err := LoadLayered(LoadOptions{Dir: dir, Environ: []string{"AIRVIEW_CONFIG=" + file}, ReadOnly: true})
	if err != nil || cfg.HTTP.Port != 7000 || l.File() != file {
		t.Fatalf("AIRVIEW_CONFIG: %v %d", err, cfg.HTTP.Port)
	}
	// NoFile ignores it.
	cfg, _, _, _ = LoadLayered(LoadOptions{Dir: dir, File: file, NoFile: true, ReadOnly: true})
	if cfg.HTTP.Port != 80 {
		t.Fatal("NoFile")
	}
	// Syntax error: reported, file ignored, env still applied.
	if err := os.WriteFile(file, []byte("{\n  \"http\": {\"port\": 7000,,}\n}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, l, warns, err := LoadLayered(LoadOptions{Dir: dir, File: file, Environ: []string{"AIRVIEW_HTTP_PORT=7100"}, ReadOnly: true})
	if err == nil || cfg.HTTP.Port != 7100 || l.File() != "" || !strings.Contains(strings.Join(warns, "\n"), file+":2:25") {
		t.Fatalf("syntax error handling: %v %d %v", err, cfg.HTTP.Port, warns)
	}
	// ReadOnly loading created nothing.
	if _, err := os.Stat(filepath.Join(dir, FileName)); err == nil {
		t.Fatal("ReadOnly wrote config.json")
	}
}

func TestSetFlags(t *testing.T) {
	dir := t.TempDir()
	cfg, l, _, err := LoadLayered(LoadOptions{Dir: dir, ReadOnly: true,
		Environ: []string{"AIRVIEW_HTTP_PORT=8080"},
		Sets:    []string{"http.port=9090", "demo.enabled=true", "uplinks[0]={\"host\":\"h.example.org\"}", "ui.rangeRingsKm=[10,20]"}})
	if err != nil || len(l.Issues()) > 0 {
		t.Fatalf("%v %v", err, l.Issues())
	}
	if cfg.HTTP.Port != 9090 || !cfg.Demo.Enabled || len(cfg.Uplinks) != 1 || cfg.Uplinks[0].ID != "uplink" ||
		!reflect.DeepEqual(cfg.UI.RangeRingsKm, []int{10, 20}) {
		t.Fatalf("sets: %+v", cfg)
	}
	rep := Check(LoadOptions{Dir: dir, Sets: []string{"http.nope=1", "http=1", "version=3", "http.port", "http.port=x"}})
	if rep.Errors() != 5 {
		t.Fatalf("set errors:\n%s", issuesText(rep.Issues))
	}
}

func TestPlainAdminPasswordOverride(t *testing.T) {
	dir := t.TempDir()
	cfg, l, _, _ := LoadLayered(LoadOptions{Dir: dir, ReadOnly: true, Environ: []string{"AIRVIEW_HTTP_ADMIN_PASSWORD_HASH=plain:hunter2"}})
	if !CheckPassword(cfg.HTTP.AdminPasswordHash, "hunter2") {
		t.Fatalf("plain admin password not hashed: %q", cfg.HTTP.AdminPasswordHash)
	}
	again, _ := l.Apply(model.DefaultConfig())
	if again.HTTP.AdminPasswordHash != cfg.HTTP.AdminPasswordHash {
		t.Fatal("hash must be stable across Apply")
	}
}

func TestNilLayers(t *testing.T) {
	var l *Layers
	dir := t.TempDir()
	c := model.DefaultConfig()
	c.HTTP.Port = 81
	eff, err := l.Commit(dir, c)
	if err != nil || eff.HTTP.Port != 81 || l.Locked("http.port") || l.Overrides() != nil || l.File() != "" || l.Issues() != nil {
		t.Fatal("nil layers")
	}
	if p := l.Persistable(c); p.HTTP.Port != 81 {
		t.Fatal("nil Persistable")
	}
}

// Round trip: every non-default value survives Save -> Load, and a config
// printed as an operator file reproduces itself.
func TestRoundTripCustomNetworkConfig(t *testing.T) {
	dir := t.TempDir()
	c := model.DefaultConfig()
	c.Mode = model.ModeAggregator
	c.Feeds[0].NoForward = true
	c.Feeds[0].ClockClass = "gps_utc"
	c.Uplinks = []model.Uplink{model.DefaultUplink()}
	c.Uplinks[0].ID, c.Uplinks[0].Host, c.Uplinks[0].EnrolToken = "club", "agg.example.org", "tok"
	c.Uplinks[0].Inputs = []string{model.DefaultFeedID}
	c.Aggregator.Name = "Club Net"
	c.Aggregator.TrustedProxies = []string{"10.0.0.0/8"}
	c.Aggregator.Limits.AllowCIDRs = []string{"192.0.2.0/24"}
	c.Aggregator.MLAT.AutoUseSurvey = false
	c.Aggregator.OutputToken = "out"
	want, notes := Validate(c)
	if len(notes) > 0 {
		t.Fatalf("notes: %v", notes)
	}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	got, warns, err := LoadWithWarnings(dir)
	if err != nil || len(warns) > 0 || !Equal(got, want) {
		t.Fatalf("round trip: %v %v\n got %+v\nwant %+v", err, warns, got, want)
	}
	// Operator file round trip from the JSON form.
	data, _ := os.ReadFile(filepath.Join(dir, FileName))
	op := filepath.Join(dir, "op.jsonc")
	if err := os.WriteFile(op, append([]byte("// copy\n"), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	eff, l, _, err := LoadLayered(LoadOptions{Dir: empty, File: op, ReadOnly: true})
	if err != nil || len(l.Issues()) > 0 || !Equal(eff, want) {
		t.Fatalf("operator round trip: %v %v", err, l.Issues())
	}
}
