package selfupdate

import (
	"strings"
	"testing"
)

func TestVersionOrder(t *testing.T) {
	// Ascending (SemVer 2.0.0 §11 example plus AirView forms).
	asc := []string{"0.1.0", "0.9.9", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "v1.0.1", "1.1", "1.10.0", "2.0.0"}
	for i := range asc {
		for j := range asc {
			got := CompareVersions(asc[i], asc[j])
			want := sign(i - j)
			if got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", asc[i], asc[j], got, want)
			}
		}
	}
	if CompareVersions("1.0.0+build.5", "1.0.0") != 0 {
		t.Error("build metadata must not order")
	}
	if CompareVersions("dev", "0.0.1") != -1 || CompareVersions("0.0.1", "dev") != 1 {
		t.Error("invalid versions sort first")
	}
	for _, bad := range []string{"", "1.2.3.4", "01.2.3", "1.-2", "1.2.3-", "1.2.3-a..b", "a.b.c"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	v, _ := ParseVersion("1.2.0-beta.1")
	if !v.IsPrerelease() || v.String() != "1.2.0-beta.1" {
		t.Error(v)
	}
}

func TestTargets(t *testing.T) {
	if targetFor("linux", "arm") != "linux-armv7" || targetFor("windows", "amd64") != "windows-amd64" {
		t.Fatal("targetFor")
	}
	if !ValidTarget(CurrentTarget()) {
		t.Fatalf("current target %s", CurrentTarget())
	}
	for _, bad := range []string{"linux", "linux-arm", "Linux-amd64", "linux-amd64 ", "../x"} {
		if ValidTarget(bad) {
			t.Errorf("%q valid", bad)
		}
	}
}

func TestSettings(t *testing.T) {
	pub, _, _ := GenerateKey()
	s := DefaultSettings()
	if err := s.Check(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	s.Enabled = true
	if err := s.Check(); err == nil {
		t.Error("enabled without URL/key accepted")
	}
	env := map[string]string{
		"AIRVIEW_UPDATE_ENABLED": "true", "AIRVIEW_UPDATE_CHANNEL": "beta",
		"AIRVIEW_UPDATE_MANIFEST_URL":    "https://agg.example/feeder/manifest.json",
		"AIRVIEW_UPDATE_KEYS":            pub.String() + "," + pub.String(),
		"AIRVIEW_UPDATE_HEALTH_WINDOW_S": "90", "AIRVIEW_UPDATE_CHECK_INTERVAL_MIN": "720",
		"AIRVIEW_UPDATE_HEALTH_URL": "http://127.0.0.1:8099/api/health", "AIRVIEW_UPDATE_MAX_DOWNLOAD_MB": "64",
	}
	if err := s.ApplyEnv("AIRVIEW_UPDATE_", func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	if s.Channel != "beta" || len(s.Keys) != 2 || s.HealthWindowS != 90 || s.CheckIntervalMin != 720 || s.MaxDownloadMB != 64 {
		t.Fatalf("%+v", s)
	}
	u, err := s.ChannelManifestURL()
	if err != nil || u != "https://agg.example/feeder/beta/manifest.json" {
		t.Fatalf("%s %v", u, err)
	}
	if err := s.ApplyEnv("AIRVIEW_UPDATE_", func(k string) string {
		return map[string]string{"AIRVIEW_UPDATE_ENABLED": "maybe", "AIRVIEW_UPDATE_HEALTH_WINDOW_S": "x"}[k]
	}); err == nil || !strings.Contains(err.Error(), "ENABLED") || !strings.Contains(err.Error(), "HEALTH_WINDOW_S") {
		t.Errorf("bad env: %v", err)
	}
	bad := []Settings{
		{Channel: "nightly"}, {ManifestURL: "ftp://x/m.json"}, {ManifestURL: "https://u:p@x/m.json"},
		{Keys: []string{"nope"}}, {CheckIntervalMin: 5}, {HealthWindowS: 5}, {HealthURL: "localhost:80"}, {MaxDownloadMB: -1},
	}
	for i, b := range bad {
		if err := b.Check(); err == nil {
			t.Errorf("bad[%d] %+v accepted", i, b)
		}
	}
	if u, _ := ChannelURL("https://h/rel/{channel}/m.json", "stable"); u != "https://h/rel/stable/m.json" {
		t.Error(u)
	}
	if u, _ := ChannelURL("https://h/feeder/manifest.json", "stable"); u != "https://h/feeder/manifest.json" {
		t.Error(u)
	}
}
