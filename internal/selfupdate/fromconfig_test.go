package selfupdate

import (
	"reflect"
	"testing"

	"airview/internal/model"
)

// TestSettingsFromModel: every field of the model block arrives (the two
// structs must stay in step).
func TestSettingsFromModel(t *testing.T) {
	m := model.UpdateSettings{Enabled: true, Channel: "beta", ManifestURL: "https://h/feeder/manifest.json",
		Keys: []string{"k"}, CheckIntervalMin: 30, HealthWindowS: 20, MaxDownloadMB: 9, HealthURL: "http://127.0.0.1:1/x"}
	s := SettingsFromModel(m)
	if reflect.TypeOf(s).NumField() != reflect.TypeOf(m).NumField() {
		t.Fatalf("Settings has %d fields, model.UpdateSettings %d", reflect.TypeOf(s).NumField(), reflect.TypeOf(m).NumField())
	}
	want := Settings{Enabled: true, Channel: "beta", ManifestURL: m.ManifestURL, Keys: []string{"k"},
		CheckIntervalMin: 30, HealthWindowS: 20, HealthURL: m.HealthURL, MaxDownloadMB: 9}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("got %+v", s)
	}
	if FeederSettings(true, model.UpdateSettings{}).Enabled {
		t.Fatal("feeder enabled without URL and key")
	}
	if !FeederSettings(true, m).Enabled || FeederSettings(false, model.UpdateSettings{ManifestURL: "x", Keys: []string{"k"}}).Enabled {
		t.Fatal("feeder autoUpdate mapping")
	}
}
