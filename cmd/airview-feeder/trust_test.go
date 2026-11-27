package main

import (
	"testing"

	"airview/internal/model"
)

// SEC-R1: the status page cannot change the update trust anchors.
func TestLockUpdateAnchors(t *testing.T) {
	file := model.FeederConfig{AutoUpdate: false}
	file.Update.ManifestURL = "https://agg.example/feeder/manifest.json"
	file.Update.Keys = []string{"K1"}
	file.Update.Channel = "stable"

	same := file
	same.Update.Keys = []string{"K1"}
	if lockUpdateAnchors(&same, file) {
		t.Error("unchanged anchors reported as changed")
	}

	evil := file
	evil.AutoUpdate = true
	evil.Update.ManifestURL = "https://attacker.example/m.json"
	evil.Update.Keys = []string{"ATTACKER"}
	evil.Update.Channel = "beta"
	if !lockUpdateAnchors(&evil, file) {
		t.Error("changed anchors not reported")
	}
	if evil.Update.ManifestURL != file.Update.ManifestURL || len(evil.Update.Keys) != 1 || evil.Update.Keys[0] != "K1" {
		t.Errorf("anchors not restored: %+v", evil.Update)
	}
	// The channel and the automatic switch remain page settings.
	if !evil.AutoUpdate || evil.Update.Channel != "beta" {
		t.Errorf("page settings dropped: %+v auto=%v", evil.Update, evil.AutoUpdate)
	}
	health := file
	health.Update.HealthURL = "http://attacker.example/health"
	if !lockUpdateAnchors(&health, file) || health.Update.HealthURL != "" {
		t.Errorf("health URL not locked: %q", health.Update.HealthURL)
	}
	chOnly := file
	chOnly.Update.Channel = "beta"
	chOnly.AutoUpdate = true
	if lockUpdateAnchors(&chOnly, file) {
		t.Error("channel / automatic change reported as an anchor change")
	}
}
