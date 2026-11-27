package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"airview/internal/model"
)

func writeFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// ---------------------------------------------------------------- data dir

func TestResolveDataDirFlag(t *testing.T) {
	want := filepath.Join(t.TempDir(), "my data", "nested")
	got, err := ResolveDataDir("  " + want + "  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestResolveDataDirFlagUnusable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDataDir(filepath.Join(file, "sub")); err == nil {
		t.Fatal("expected an error for a data dir below a regular file")
	}
}

func TestResolveDataDirNextToExecutable(t *testing.T) {
	exeDir := t.TempDir()
	exe := func() (string, error) { return filepath.Join(exeDir, "airview.exe"), nil }
	cfgDir := func() (string, error) { t.Fatal("user config dir must not be consulted"); return "", nil }
	got, err := resolveDataDir("", exe, cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(exeDir, "data") {
		t.Fatalf("got %q", got)
	}
	if names := dirNames(t, got); len(names) != 0 {
		t.Fatalf("write probe left files behind: %v", names)
	}
}

func TestResolveDataDirFallsBackToUserConfig(t *testing.T) {
	exeDir := t.TempDir()
	// A regular file named "data" next to the executable makes that location unusable.
	if err := os.WriteFile(filepath.Join(exeDir, "data"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	user := t.TempDir()
	exe := func() (string, error) { return filepath.Join(exeDir, "airview.exe"), nil }
	got, err := resolveDataDir("", exe, func() (string, error) { return user, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(user, model.AppName) {
		t.Fatalf("got %q", got)
	}

	// Executable unknown: straight to the user config dir.
	got, err = resolveDataDir("", func() (string, error) { return "", errors.New("no exe") },
		func() (string, error) { return user, nil })
	if err != nil || got != filepath.Join(user, model.AppName) {
		t.Fatalf("got %q, %v", got, err)
	}

	// Nothing usable at all.
	_, err = resolveDataDir("", exe, func() (string, error) { return "", errors.New("no home") })
	if err == nil || !strings.Contains(err.Error(), "-data") {
		t.Fatalf("expected a helpful error, got %v", err)
	}
}

// -------------------------------------------------------------------- load

func TestLoadMissingCreatesDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, warnings, err := LoadWithWarnings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if !Equal(cfg, model.DefaultConfig()) {
		t.Fatalf("not the defaults: %+v", cfg)
	}
	var onDisk model.Config
	if err := json.Unmarshal([]byte(readFile(t, dir)), &onDisk); err != nil {
		t.Fatal(err)
	}
	if !Equal(onDisk, model.DefaultConfig()) {
		t.Fatal("saved file differs from defaults")
	}
}

func TestSaveLoadRoundTripAndAtomicity(t *testing.T) {
	dir := t.TempDir()
	cfg := model.DefaultConfig()
	cfg.HTTP.Port = 8123
	cfg.Receiver = model.Receiver{Name: "Roof", Model: "Radarcape EVO 2CH", Located: true, Lat: 51.4775, Lon: -0.4614, GroundElevM: 25}
	cfg.NTP = model.NTPConfig{Enabled: true, Server: "192.168.1.10", IntervalSec: 16}
	cfg.Hub.Password = "s3cret"
	cfg.HTTP.AdminPasswordHash = HashPassword("admin")
	addYagi(&cfg)
	cfg.Antennas[1].AzimuthDeg = 275
	cfg.UI.RangeRingsKm = []int{50, 150}

	for i := 0; i < 3; i++ { // repeated saves replace the file
		if err := Save(dir, cfg); err != nil {
			t.Fatal(err)
		}
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != FileName {
		t.Fatalf("unexpected files after save: %v", names)
	}
	before := readFile(t, dir)

	got, warnings, err := LoadWithWarnings(dir)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	if !Equal(got, cfg) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, cfg)
	}
	if readFile(t, dir) != before {
		t.Fatal("loading a canonical file must not rewrite it")
	}
}

func TestLoadVersionlessPartialFileIsMigrated(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "\xef\xbb\xbf"+`{
	  "http": {"port": 8080},
	  "antennas": [{"id": "Roof Yagi", "type": "YAGI", "azimuthDeg": 370}],
	  "feeds": [{"id": "rc", "type": "beast", "host": "10.0.0.5", "enabled": true, "antennaId": "Roof Yagi"}],
	  "futureSetting": {"x": 1}
	}`)
	cfg, warnings, err := LoadWithWarnings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSub(warnings, "migrated from config version 0") {
		t.Fatalf("no migration warning: %v", warnings)
	}
	if cfg.Version != model.ConfigVersion || cfg.HTTP.Port != 8080 {
		t.Fatalf("version/port: %+v", cfg.HTTP)
	}
	// Settings missing from the file keep their defaults.
	if !cfg.HTTP.TileCache || cfg.Weather.IntervalMin != 15 || cfg.Hub.Port != 30154 || cfg.UI.Units != "aviation" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if len(cfg.UI.RangeRingsKm) != 4 {
		t.Fatalf("range rings default lost: %v", cfg.UI.RangeRingsKm)
	}
	// The list replaces the default list and its element does not inherit
	// anything from the default first antenna (gain 5, colour, height 8).
	if len(cfg.Antennas) != 1 {
		t.Fatalf("antennas: %+v", cfg.Antennas)
	}
	a := cfg.Antennas[0]
	if a.ID != "roof-yagi" || a.Type != model.AntYagi || a.AzimuthDeg != 10 || a.GainDBi != 0 || a.HeightAGLM != 0 {
		t.Fatalf("antenna: %+v", a)
	}
	if len(cfg.Feeds) != 1 || cfg.Feeds[0].AntennaID != "roof-yagi" || cfg.Feeds[0].Port != 10003 {
		t.Fatalf("feed: %+v", cfg.Feeds)
	}
	// The file was rewritten in canonical form.
	if s := readFile(t, dir); !strings.Contains(s, `"version": 1`) || strings.Contains(s, "futureSetting") {
		t.Fatalf("file not rewritten:\n%s", s)
	}
	if _, w2, _ := LoadWithWarnings(dir); len(w2) != 0 {
		t.Fatalf("second load should be clean, got %v", w2)
	}
}

func TestLoadNewerVersionKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	orig := `{"version": 99, "http": {"port": 8099}, "quantum": true}`
	writeFile(t, dir, orig)
	cfg, warnings, err := LoadWithWarnings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Port != 8099 || cfg.Version != model.ConfigVersion {
		t.Fatalf("cfg: %+v", cfg.HTTP)
	}
	if !containsSub(warnings, "newer AirView") {
		t.Fatalf("warnings: %v", warnings)
	}
	bak, err := os.ReadFile(filepath.Join(dir, FileName+".v99.bak"))
	if err != nil || string(bak) != orig {
		t.Fatalf("backup missing or different: %v %q", err, bak)
	}
}

func TestLoadCorruptFileIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	for n, content := range []string{`{"http": {"port": 81`, ``, `[1,2,3]`, `null`} {
		writeFile(t, dir, content)
		cfg, warnings, err := LoadWithWarnings(dir)
		if err != nil {
			t.Fatalf("case %d: %v", n, err)
		}
		if !Equal(cfg, model.DefaultConfig()) {
			t.Fatalf("case %d: defaults not returned", n)
		}
		aside := FileName + ".bad-" + string(rune('1'+n))
		if len(warnings) != 1 || !strings.Contains(warnings[0], "corrupt") || !strings.Contains(warnings[0], aside) {
			t.Fatalf("case %d: warnings %v", n, warnings)
		}
		kept, err := os.ReadFile(filepath.Join(dir, aside))
		if err != nil || string(kept) != content {
			t.Fatalf("case %d: bad file not preserved: %v %q", n, err, kept)
		}
		var onDisk model.Config
		if err := json.Unmarshal([]byte(readFile(t, dir)), &onDisk); err != nil || !Equal(onDisk, model.DefaultConfig()) {
			t.Fatalf("case %d: defaults not saved (%v)", n, err)
		}
	}
	// Plain Load reports the same situation without failing.
	writeFile(t, dir, "{{{{")
	if cfg, err := Load(dir); err != nil || cfg.HTTP.Port != 80 {
		t.Fatalf("Load: %v %+v", err, cfg.HTTP)
	}
}

func TestLoadWrongFieldTypeKeepsRest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"version": 1, "http": {"port": "eighty", "bindAddr": "127.0.0.1"}, "demo": {"enabled": true, "aircraft": 25}}`)
	cfg, warnings, err := LoadWithWarnings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSub(warnings, "wrong type") {
		t.Fatalf("warnings: %v", warnings)
	}
	if cfg.HTTP.Port != 80 || cfg.HTTP.BindAddr != "127.0.0.1" || !cfg.Demo.Enabled || cfg.Demo.Aircraft != 25 {
		t.Fatalf("cfg: %+v %+v", cfg.HTTP, cfg.Demo)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("a recoverable file must not be moved aside: %v", names)
	}
}

func TestFactoryReset(t *testing.T) {
	dir := t.TempDir()
	cfg := model.DefaultConfig()
	cfg.HTTP.Port = 9000
	cfg.Hub.Password = "pw"
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := FactoryReset(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(got, model.DefaultConfig()) {
		t.Fatal("FactoryReset did not return the defaults")
	}
	loaded, warnings, err := LoadWithWarnings(dir)
	if err != nil || len(warnings) != 0 || !Equal(loaded, model.DefaultConfig()) {
		t.Fatalf("after reset: %v %v", err, warnings)
	}
}

// ---------------------------------------------------------------- validate

func TestValidateDefaultsAreClean(t *testing.T) {
	got, problems := Validate(model.DefaultConfig())
	if len(problems) != 0 {
		t.Fatalf("defaults produce problems: %v", problems)
	}
	if !Equal(got, model.DefaultConfig()) {
		t.Fatalf("defaults were changed:\n%+v", got)
	}
}

func TestValidateZeroValue(t *testing.T) {
	got, _ := Validate(model.Config{})
	def := model.DefaultConfig()
	if got.Version != model.ConfigVersion || got.Mode != model.ModeStandalone || got.HTTP.Port != 80 ||
		got.Hub.Port != 30154 || got.Remote.DataPort != 30154 || got.Remote.HTTPPort != 80 {
		t.Fatalf("zero config not defaulted: %+v", got)
	}
	wantStorage := def.Storage
	wantStorage.FlightRetentionDays = 0 // 0 is a legal value: keep flights forever
	if got.Storage != wantStorage {
		t.Fatalf("storage: %+v", got.Storage)
	}
	if got.NTP.IntervalSec != 64 || got.Weather.IntervalMin != 15 || got.Demo.Aircraft != 60 ||
		got.UI.TrailMinutes != 15 || got.UI.AircraftTTLs != 60 || got.UI.MapStyle != "airview-dark" ||
		got.UI.Units != "aviation" || got.UI.ColorTrailsBy != "altitude" || got.Receiver.Name != "My Station" {
		t.Fatalf("scalars: %+v", got)
	}
	if got.Antennas == nil || got.Feeds == nil || got.UI.RangeRingsKm == nil {
		t.Fatal("nil lists must become empty lists (JSON [] not null)")
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "null") {
		t.Fatalf("null in JSON: %s", b)
	}
}

func hostileConfig() model.Config {
	long := strings.Repeat("x", 5000)
	c := model.Config{
		Version: -7,
		Mode:    "ROOT-KIT",
		HTTP:    model.HTTPConfig{Port: 70000, BindAddr: "not an ip; rm -rf", AdminPasswordHash: "plain:oops"},
		Receiver: model.Receiver{
			Name: "  \x00Sta\x07tion\n ", Model: long, Located: true,
			Lat: math.NaN(), Lon: math.Inf(1), GroundElevM: math.Inf(-1),
		},
		NTP:     model.NTPConfig{Enabled: true, Server: "bad host!", IntervalSec: -5},
		Weather: model.WeatherConfig{IntervalMin: 1 << 30},
		Hub:     model.HubConfig{Port: -1, Password: long + "\xff\xfe"},
		Remote:  model.RemoteConfig{Host: "http://pi.local:8080/path?x=1", DataPort: 99999, HTTPPort: -3, Password: "\xff"},
		Storage: model.StorageConfig{TrailIntervalSec: -1, TrailRetentionDays: 1 << 40 >> 10, FlightRetentionDays: -9, MinuteRetentionDays: 99999},
		UI: model.UIConfig{
			Units: "furlongs", TrailMinutes: -1, RangeRingsKm: []int{300, -1, 300, 0, 100, 99999, 200},
			AircraftTTLs: 1, MapStyle: long, ColorTrailsBy: "\x00",
		},
		Demo: model.DemoConfig{Aircraft: -100},
	}
	for i := 0; i < MaxAntennas+5; i++ {
		c.Antennas = append(c.Antennas, model.Antenna{
			ID: "Yagi #1 (north)", Name: long, Type: "dish", Channel: 99,
			GainDBi: math.NaN(), HeightAGLM: -4, AzimuthDeg: -725.5, TiltDeg: 1e9,
			HBeamwidthDeg: 1e9, VBeamwidthDeg: math.Inf(1), FrontToBackDB: -1, Elements: -5,
			Polarization: "diagonal", CableLossDB: math.NaN(), LNAGainDB: 1e308, LNANoiseFigDB: -1e308,
			FilterLossDB: math.Inf(-1), Notes: long + "\n\ttab", Color: "javascript:alert(1)",
		})
	}
	c.Antennas[1].ID = ""
	c.Antennas[1].Name = ""
	c.Antennas[2].ID = "\xff\xff"
	c.Antennas[2].Name = ""
	for i := 0; i < MaxFeeds+3; i++ {
		c.Feeds = append(c.Feeds, model.Feed{
			ID: "Feed/One", Name: "", Enabled: true, Type: "telepathy", Host: "exa mple.com", Port: -80,
			URL: "ftp://x/y", AntennaID: "Yagi #1 (north)", Notes: long,
		})
	}
	c.Feeds[1] = model.Feed{ID: "json", Type: "JSON ", URL: "192.168.1.9/data/aircraft.json", AntennaID: "ghost", Port: 123456}
	c.Feeds[2] = model.Feed{ID: "b", Type: "beast", Host: "[::1]:30005", AntennaID: "YAGI-1-NORTH"}
	c.Feeds[3] = model.Feed{ID: "s", Type: "sbs", Host: "10.0.0.2:31003", Port: 5}
	return c
}

func TestValidateHostileInput(t *testing.T) {
	got, problems := Validate(hostileConfig())
	if len(problems) < 20 {
		t.Fatalf("expected many problems, got %d: %v", len(problems), problems)
	}
	for _, p := range problems {
		if len(p) > 400 {
			t.Fatalf("problem message echoes too much input (%d bytes)", len(p))
		}
	}

	if got.Version != model.ConfigVersion || got.Mode != model.ModeStandalone {
		t.Fatalf("version/mode: %d %q", got.Version, got.Mode)
	}
	if got.HTTP.Port != 80 || got.HTTP.BindAddr != "" {
		t.Fatalf("http: %+v", got.HTTP)
	}
	if !validHash(got.HTTP.AdminPasswordHash) || CheckPassword(got.HTTP.AdminPasswordHash, "oops") ||
		CheckPassword(got.HTTP.AdminPasswordHash, "plain:oops") {
		t.Fatalf("malformed admin hash must be replaced by an unusable, well-formed one: %q", got.HTTP.AdminPasswordHash)
	}

	r := got.Receiver
	if r.Name != "Station" || len([]rune(r.Model)) != maxNameLen || r.Located || r.Lat != 0 || r.Lon != 0 || r.GroundElevM != 0 {
		t.Fatalf("receiver: %+v", r)
	}
	if got.NTP.Enabled || got.NTP.Server != "" || got.NTP.IntervalSec != 2 {
		t.Fatalf("ntp: %+v", got.NTP)
	}
	if got.Weather.IntervalMin != 1440 || got.Hub.Port != 30154 || len(got.Hub.Password) != maxSecretLen {
		t.Fatalf("weather/hub: %+v %d", got.Weather, len(got.Hub.Password))
	}
	if got.Remote.Host != "pi.local" || got.Remote.DataPort != 30154 || got.Remote.HTTPPort != 80 || got.Remote.Password != "" {
		t.Fatalf("remote: %+v", got.Remote)
	}
	if got.Storage != (model.StorageConfig{TrailIntervalSec: 1, TrailRetentionDays: 3650, FlightRetentionDays: 730, MinuteRetentionDays: 3650}) {
		t.Fatalf("storage: %+v", got.Storage)
	}
	ui := got.UI
	if ui.Units != "aviation" || ui.TrailMinutes != 1 || ui.AircraftTTLs != 5 || len([]rune(ui.MapStyle)) != 64 || ui.ColorTrailsBy != "altitude" {
		t.Fatalf("ui: %+v", ui)
	}
	if len(ui.RangeRingsKm) != 3 || ui.RangeRingsKm[0] != 100 || ui.RangeRingsKm[1] != 200 || ui.RangeRingsKm[2] != 300 {
		t.Fatalf("rings: %v", ui.RangeRingsKm)
	}
	if got.Demo.Aircraft != 1 {
		t.Fatalf("demo: %+v", got.Demo)
	}

	if len(got.Antennas) != MaxAntennas {
		t.Fatalf("antenna count %d", len(got.Antennas))
	}
	seen := map[string]bool{}
	for i, a := range got.Antennas {
		if a.ID == "" || a.ID != Slug(a.ID) || len(a.ID) > maxIDLen || seen[a.ID] {
			t.Fatalf("antenna %d: bad or duplicate id %q", i, a.ID)
		}
		seen[a.ID] = true
		if a.Name == "" || a.Type != model.AntOther || a.Channel != -1 || a.GainDBi != 0 || a.HeightAGLM != 0 ||
			a.TiltDeg != 90 || a.HBeamwidthDeg != 360 || a.VBeamwidthDeg != 0 || a.FrontToBackDB != 0 ||
			a.Elements != 0 || a.Polarization != "vertical" || a.CableLossDB != 0 || a.LNAGainDB != 60 ||
			a.LNANoiseFigDB != 0 || a.FilterLossDB != 0 || len([]rune(a.Notes)) > maxNotesLen {
			t.Fatalf("antenna %d not normalised: %+v", i, a)
		}
		if math.Abs(a.AzimuthDeg-354.5) > 1e-9 {
			t.Fatalf("antenna %d azimuth %v", i, a.AzimuthDeg)
		}
		if _, ok := normColor(a.Color); !ok {
			t.Fatalf("antenna %d colour %q", i, a.Color)
		}
	}
	if got.Antennas[0].ID != "yagi-1-north" || got.Antennas[1].ID != "antenna" || got.Antennas[2].ID != "antenna-2" ||
		got.Antennas[3].ID != "yagi-1-north-2" {
		t.Fatalf("antenna ids: %q %q %q %q", got.Antennas[0].ID, got.Antennas[1].ID, got.Antennas[2].ID, got.Antennas[3].ID)
	}

	if len(got.Feeds) != MaxFeeds {
		t.Fatalf("feed count %d", len(got.Feeds))
	}
	seen = map[string]bool{}
	for i, f := range got.Feeds {
		if f.ID == "" || seen[f.ID] || f.Name == "" {
			t.Fatalf("feed %d: id %q name %q", i, f.ID, f.Name)
		}
		seen[f.ID] = true
	}
	f0 := got.Feeds[0]
	if f0.ID != "feed-one" || f0.Type != model.FeedBeast || f0.Enabled || f0.Host != "" || f0.Port != 10003 ||
		f0.URL != "" || f0.AntennaID != "yagi-1-north" {
		t.Fatalf("feed 0: %+v", f0)
	}
	f1 := got.Feeds[1]
	if f1.Type != model.FeedJSON || f1.URL != "http://192.168.1.9/data/aircraft.json" || f1.AntennaID != "" || f1.Port != 0 {
		t.Fatalf("feed 1: %+v", f1)
	}
	f2 := got.Feeds[2]
	if f2.Host != "::1" || f2.Port != 30005 || f2.AntennaID != "yagi-1-north" {
		t.Fatalf("feed 2: %+v", f2)
	}
	if f3 := got.Feeds[3]; f3.Host != "10.0.0.2" || f3.Port != 5 {
		t.Fatalf("feed 3: %+v", f3)
	}

	// Idempotent: a second pass changes nothing and reports nothing.
	again, problems2 := Validate(got)
	if len(problems2) != 0 {
		t.Fatalf("second validation reported: %v", problems2)
	}
	if !Equal(again, got) {
		t.Fatal("Validate is not idempotent")
	}
	// The input was not modified.
	if in := hostileConfig(); in.Antennas[0].ID != "Yagi #1 (north)" {
		t.Fatal("test bug")
	}
}

func TestValidateDoesNotAliasInput(t *testing.T) {
	in := model.DefaultConfig()
	in.Antennas[0].ID = "UPPER"
	in.UI.RangeRingsKm = []int{300, 100}
	out, _ := Validate(in)
	if in.Antennas[0].ID != "UPPER" || in.UI.RangeRingsKm[0] != 300 {
		t.Fatal("Validate modified its argument")
	}
	if out.Antennas[0].ID != "upper" || out.UI.RangeRingsKm[0] != 100 {
		t.Fatalf("out: %+v", out.Antennas[0])
	}
}

func TestValidateSpecifics(t *testing.T) {
	c := model.DefaultConfig()
	c.Mode = "Remote-Web"
	c.Remote.Host = " MyPi.local "
	c.HTTP.BindAddr = "LOCALHOST"
	c.Receiver.Located = true
	c.Receiver.Name = "Roof top  ä\t1" // Unicode line/paragraph separators and a tab are stripped
	c.Receiver.Lat, c.Receiver.Lon = 95, 190
	c.NTP = model.NTPConfig{Enabled: true, Server: "ntp://[fe80::1]:1123/", IntervalSec: 1}
	addYagi(&c)
	c.Antennas[1].AzimuthDeg = 360
	c.Antennas[1].Color = "#ABC"
	c.Feeds = append(c.Feeds, model.Feed{ID: model.DefaultFeedID, Type: "avr", Host: "radarcape", AntennaID: "YAGI"})
	got, problems := Validate(c)

	if got.Mode != model.ModeRemoteWeb || got.Remote.Host != "MyPi.local" || got.HTTP.BindAddr != "127.0.0.1" {
		t.Fatalf("mode/remote/bind: %q %q %q", got.Mode, got.Remote.Host, got.HTTP.BindAddr)
	}
	if got.Receiver.Lat != 90 || got.Receiver.Lon != -170 || !got.Receiver.Located {
		t.Fatalf("receiver: %+v", got.Receiver)
	}
	if got.Receiver.Name != "Rooftop ä1" {
		t.Fatalf("receiver name: %q", got.Receiver.Name)
	}
	if got.NTP.Server != "[fe80::1]:1123" || !got.NTP.Enabled || got.NTP.IntervalSec != 2 {
		t.Fatalf("ntp: %+v", got.NTP)
	}
	if got.Antennas[1].AzimuthDeg != 0 || got.Antennas[1].Color != "#abc" {
		t.Fatalf("antenna: %+v", got.Antennas[1])
	}
	if f := got.Feeds[1]; f.ID != model.DefaultFeedID+"-2" || f.Port != 10002 || f.AntennaID != "yagi" {
		t.Fatalf("feed: %+v", f)
	}
	if !containsSub(problems, "receiver.lat") || !containsSub(problems, "receiver.lon") {
		t.Fatalf("problems: %v", problems)
	}

	// A remote mode without a host cannot work.
	c2 := model.DefaultConfig()
	c2.Mode = model.ModeRemoteData
	got2, problems2 := Validate(c2)
	if got2.Mode != model.ModeStandalone || !containsSub(problems2, "remote.host") {
		t.Fatalf("mode %q problems %v", got2.Mode, problems2)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Yagi #1 (north)":        "yagi-1-north",
		"  --Vertical__ ":        "vertical",
		"ÄÖ":                     "",
		"a__b--c":                "a__b-c",
		"UPPER_case-09":          "upper_case-09",
		"":                       "",
		"\x00\xff":               "",
		"a.b/c":                  "a-b-c",
		strings.Repeat("a-", 40): strings.TrimRight(strings.Repeat("a-", 16), "-"),
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
		if got := Slug(Slug(in)); got != Slug(in) {
			t.Errorf("Slug not idempotent for %q", in)
		}
	}
}

func TestSplitHost(t *testing.T) {
	type res struct {
		host string
		port int
		ok   bool
	}
	cases := map[string]res{
		"":                           {"", 0, true},
		"radarcape":                  {"radarcape", 0, true},
		" 192.168.1.5:10003 ":        {"192.168.1.5", 10003, true},
		"http://pi.local:8080/x?y#z": {"pi.local", 8080, true},
		"user:pw@host.example":       {"host.example", 0, true},
		"[::1]":                      {"::1", 0, true},
		"[2001:db8::1]:123":          {"2001:db8::1", 123, true},
		"fe80::1%eth0":               {"fe80::1%eth0", 0, true},
		"host:0":                     {"", 0, false},
		"host:99999":                 {"", 0, false},
		"host:abc":                   {"", 0, false},
		"[::1":                       {"", 0, false},
		"[::1]x":                     {"", 0, false},
		"a:b:c":                      {"", 0, false},
		"exa mple.com":               {"", 0, false},
		"semi;colon":                 {"", 0, false},
		"bücher.example":             {"bücher.example", 0, true},
		strings.Repeat("a", 400):     {"", 0, false},
	}
	for in, want := range cases {
		h, p, ok := splitHost(in)
		if (res{h, p, ok}) != want {
			t.Errorf("splitHost(%q) = %q,%d,%v want %+v", in, h, p, ok, want)
		}
	}
}

// ----------------------------------------------------------------- secrets

func TestHashAndCheckPassword(t *testing.T) {
	h1 := HashPassword("correct horse")
	h2 := HashPassword("correct horse")
	if h1 == h2 {
		t.Fatal("salt is not random")
	}
	// SEC-08: new hashes are PBKDF2 (auth format), never a single SHA-256.
	if !strings.HasPrefix(h1, "pbkdf2-sha256$200000$") || IsLegacyHash(h1) {
		t.Fatalf("format: %q", h1)
	}
	legacy := legacyHash("correct horse")
	if !IsLegacyHash(legacy) || !CheckPassword(legacy, "correct horse") || CheckPassword(legacy, "x") {
		t.Fatal("legacy hash")
	}
	parts := strings.Split(legacy, ":")
	if !CheckPassword(h1, "correct horse") || !CheckPassword(h2, "correct horse") {
		t.Fatal("correct password rejected")
	}
	for _, bad := range []string{"", "Correct horse", "correct horse ", "x"} {
		if CheckPassword(h1, bad) {
			t.Fatalf("wrong password %q accepted", bad)
		}
	}
	if HashPassword("") != "" {
		t.Fatal("empty password must hash to the empty string")
	}
	for _, malformed := range []string{"", "set", "sha256", "sha256::", "sha256:zz:" + parts[2], "md5:" + parts[1] + ":" + parts[2],
		"sha256:" + parts[1] + ":abcd", "sha256:" + parts[1] + ":" + parts[2] + ":x", strings.Repeat("a", 4000)} {
		if CheckPassword(malformed, "correct horse") || CheckPassword(malformed, "") {
			t.Fatalf("malformed hash %q matched", malformed)
		}
	}
	// The documented construction, computed independently:
	// hex(SHA-256(salt bytes || password bytes)).
	sum := sha256.Sum256(append([]byte{0x00, 0xff}, "pw"...))
	if !CheckPassword("sha256:00ff:"+hex.EncodeToString(sum[:]), "pw") {
		t.Fatal("hash built from the documented construction rejected")
	}
	if CheckPassword("sha256:00fe:"+hex.EncodeToString(sum[:]), "pw") {
		t.Fatal("salt is not part of the hash")
	}
}

func TestRedact(t *testing.T) {
	c := model.DefaultConfig()
	if r := Redact(c); r.HTTP.AdminPasswordHash != "" || r.Hub.Password != "" || r.Remote.Password != "" {
		t.Fatalf("unset secrets must stay empty: %+v", r)
	}
	c.HTTP.AdminPasswordHash = HashPassword("a")
	c.Hub.Password = "hubpw"
	c.Remote.Password = "remotepw"
	r := Redact(c)
	if r.HTTP.AdminPasswordHash != SecretSet || r.Hub.Password != SecretSet || r.Remote.Password != SecretSet {
		t.Fatalf("redacted: %+v", r)
	}
	b, _ := json.Marshal(r)
	for _, leak := range []string{"hubpw", "remotepw", "sha256:"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("secret %q leaked: %s", leak, b)
		}
	}
	if c.Hub.Password != "hubpw" {
		t.Fatal("Redact modified its argument")
	}
	r.Antennas[0].Name = "changed"
	if c.Antennas[0].Name == "changed" {
		t.Fatal("Redact result aliases the input")
	}
}

func TestMergeSecrets(t *testing.T) {
	cur := model.DefaultConfig()
	cur.HTTP.AdminPasswordHash = HashPassword("old-admin")
	cur.Hub.Password = "hub-old"
	cur.Remote.Password = "remote-old"

	// A redacted config sent straight back keeps everything.
	in := Redact(cur)
	in.HTTP.Port = 8080
	out := MergeSecrets(in, cur)
	if out.HTTP.AdminPasswordHash != cur.HTTP.AdminPasswordHash || out.Hub.Password != "hub-old" ||
		out.Remote.Password != "remote-old" || out.HTTP.Port != 8080 {
		t.Fatalf("keep: %+v", out)
	}
	// Blank fields keep too.
	in.HTTP.AdminPasswordHash, in.Hub.Password, in.Remote.Password = "", "", ""
	out = MergeSecrets(in, cur)
	if out.HTTP.AdminPasswordHash != cur.HTTP.AdminPasswordHash || out.Hub.Password != "hub-old" || out.Remote.Password != "remote-old" {
		t.Fatalf("blank keep: %+v", out)
	}

	// New values.
	in.HTTP.AdminPasswordHash, in.Hub.Password, in.Remote.Password = "plain:new-admin", "hub-new", "plain:set"
	out = MergeSecrets(in, cur)
	if !CheckPassword(out.HTTP.AdminPasswordHash, "new-admin") || CheckPassword(out.HTTP.AdminPasswordHash, "old-admin") {
		t.Fatal("admin password not replaced")
	}
	if out.Hub.Password != "hub-new" || out.Remote.Password != "set" {
		t.Fatalf("passwords: %q %q", out.Hub.Password, out.Remote.Password)
	}
	// A bare value in the admin field is a plain password too; it is never stored as is.
	in.HTTP.AdminPasswordHash = "hunter2"
	out = MergeSecrets(in, cur)
	if !validHash(out.HTTP.AdminPasswordHash) || !CheckPassword(out.HTTP.AdminPasswordHash, "hunter2") {
		t.Fatalf("bare admin password: %q", out.HTTP.AdminPasswordHash)
	}
	// A well-formed hash (config restore) is taken verbatim.
	restored := HashPassword("from-backup")
	in.HTTP.AdminPasswordHash = restored
	if out = MergeSecrets(in, cur); out.HTTP.AdminPasswordHash != restored {
		t.Fatal("well-formed hash not kept verbatim")
	}

	// Clearing.
	in.HTTP.AdminPasswordHash, in.Hub.Password, in.Remote.Password = SecretClear, SecretClear, PlainPrefix
	out = MergeSecrets(in, cur)
	if out.HTTP.AdminPasswordHash != "" || out.Hub.Password != "" || out.Remote.Password != "" {
		t.Fatalf("clear: %+v", out)
	}
	in.HTTP.AdminPasswordHash = PlainPrefix
	if out = MergeSecrets(in, cur); out.HTTP.AdminPasswordHash != "" {
		t.Fatal("plain: with no password must clear the admin password")
	}

	// The full PUT pipeline never lets a marker or plain password reach the result.
	in = Redact(cur)
	in.HTTP.AdminPasswordHash = "plain:pipeline"
	final, problems := ApplyUpdate(in, cur)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if !CheckPassword(final.HTTP.AdminPasswordHash, "pipeline") || final.Hub.Password != "hub-old" {
		t.Fatalf("pipeline: %+v", final.HTTP)
	}
}

func TestSaveNeverWritesMarkers(t *testing.T) {
	dir := t.TempDir()
	c := model.DefaultConfig()
	c.HTTP.AdminPasswordHash = "plain:leak-me" // MergeSecrets forgotten by the caller
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	s := readFile(t, dir)
	if strings.Contains(s, "leak-me") {
		t.Fatalf("plain password written to disk:\n%s", s)
	}
	loaded, _, err := LoadWithWarnings(dir)
	if err != nil || !validHash(loaded.HTTP.AdminPasswordHash) {
		t.Fatalf("stored hash: %q (%v)", loaded.HTTP.AdminPasswordHash, err)
	}
}

// ------------------------------------------------------------------ holder

func TestHolderDeepCopies(t *testing.T) {
	src := model.DefaultConfig()
	h := NewHolder(src)
	src.Antennas[0].Name = "mutated"
	a := h.Get()
	if a.Antennas[0].Name != "Vertical" {
		t.Fatal("NewHolder kept a reference to the caller's slices")
	}
	a.Antennas[0].Name = "x"
	a.Feeds[0].Host = "y"
	a.UI.RangeRingsKm[0] = 1
	b := h.Get()
	if b.Antennas[0].Name != "Vertical" || b.Feeds[0].Host != model.DefaultFeedHost || b.UI.RangeRingsKm[0] != 100 {
		t.Fatal("Get returned shared memory")
	}
}

func TestHolderSubscribers(t *testing.T) {
	h := NewHolder(model.DefaultConfig())
	var calls []string
	unsubA := h.Subscribe(func(old, cur model.Config) {
		calls = append(calls, "a")
		if old.HTTP.Port != 80 || cur.HTTP.Port != 8080 {
			t.Errorf("a: old %d cur %d", old.HTTP.Port, cur.HTTP.Port)
		}
		if got := h.Get(); got.HTTP.Port != 8080 { // Get inside a callback sees the new value
			t.Errorf("Get in callback: %d", got.HTTP.Port)
		}
		cur.Antennas[0].Name = "scribble" // must not leak to other subscribers / the holder
	})
	h.Subscribe(func(old, cur model.Config) {
		calls = append(calls, "b")
		if cur.Antennas[0].Name != "Vertical" {
			t.Errorf("subscriber copies are shared")
		}
	})

	next := h.Get()
	next.HTTP.Port = 8080
	h.Set(next)
	if strings.Join(calls, "") != "ab" {
		t.Fatalf("calls: %v", calls)
	}
	if h.Get().Antennas[0].Name != "Vertical" {
		t.Fatal("subscriber modified the stored config")
	}

	unsubA()
	unsubA() // idempotent
	calls = nil
	got := h.Update(func(c *model.Config) { c.Demo.Enabled = true })
	if !got.Demo.Enabled || !h.Get().Demo.Enabled || got.HTTP.Port != 8080 {
		t.Fatalf("Update: %+v", got.Demo)
	}
	if strings.Join(calls, "") != "b" {
		t.Fatalf("calls after unsubscribe: %v", calls)
	}
}

func TestHolderConcurrent(t *testing.T) {
	h := NewHolder(model.DefaultConfig())
	var notified atomic.Int64
	var lastSeen int
	h.Subscribe(func(old, cur model.Config) {
		notified.Add(1)
		// Serialised and ordered: each notification follows on from the previous one.
		if old.Demo.Aircraft != lastSeen && lastSeen != 0 {
			t.Errorf("out of order: old %d, last %d", old.Demo.Aircraft, lastSeen)
		}
		lastSeen = cur.Demo.Aircraft
	})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				h.Update(func(c *model.Config) { c.Demo.Aircraft++ })
				c := h.Get()
				c.Antennas[0].Name = "local"
				unsub := h.Subscribe(func(_, _ model.Config) {})
				unsub()
			}
		}(w)
	}
	wg.Wait()
	if got := h.Get().Demo.Aircraft; got != 60+1600 {
		t.Fatalf("lost updates: %d", got)
	}
	if notified.Load() != 1600 {
		t.Fatalf("notifications: %d", notified.Load())
	}
}

func containsSub(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// SEC-11: a kept enrol token is not carried over to another server.
func TestMergeSecretsUplinkTarget(t *testing.T) {
	cur := model.DefaultConfig()
	u := model.DefaultUplink()
	u.ID, u.Host, u.EnrolToken, u.ServerPin = "club", "agg.example.org", "tok", "sha256/AAAA"
	cur.Uplinks = []model.Uplink{u}
	in := Redact(cur)
	if got := MergeSecrets(in, cur).Uplinks[0].EnrolToken; got != "tok" {
		t.Fatalf("unchanged uplink lost its token: %q", got)
	}
	for _, mod := range []func(*model.Uplink){
		func(x *model.Uplink) { x.Host = "evil.example" },
		func(x *model.Uplink) { x.Port = 40000 },
		func(x *model.Uplink) { x.ServerPin = "" },
	} {
		in := Redact(cur)
		mod(&in.Uplinks[0])
		if got := MergeSecrets(in, cur).Uplinks[0].EnrolToken; got != "" {
			t.Fatalf("token followed a changed target: %q", got)
		}
	}
}

// addYagi adds a second (directional) antenna like the two-antenna factory
// default of earlier releases; DefaultConfig ships one antenna.
func addYagi(c *model.Config) {
	c.Antennas = append(c.Antennas, model.Antenna{
		ID: "yagi", Name: "Yagi", Type: model.AntYagi, Channel: 1,
		GainDBi: 12, HeightAGLM: 8, AzimuthDeg: 0, HBeamwidthDeg: 40, VBeamwidthDeg: 40,
		FrontToBackDB: 18, Elements: 9, Polarization: "vertical", Color: "#f59e0b",
	})
}
