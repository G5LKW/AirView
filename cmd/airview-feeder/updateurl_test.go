package main

import (
	"testing"

	"airview/internal/model"
)

func TestDefaultManifestURL(t *testing.T) {
	ups := []model.Uplink{
		{ID: "b", Host: "readsb.example", Type: model.UplinkBeast},
		{ID: "off", Host: "old.example", Disabled: true},
		{ID: "a", Host: "agg.example", Port: 30160},
	}
	c := model.FeederConfig{Uplinks: ups}
	c.Update.Keys = []string{"k"}
	defaultManifestURL(&c)
	if c.Update.ManifestURL != "https://agg.example/feeder/manifest.json" {
		t.Fatalf("got %q", c.Update.ManifestURL)
	}
	// An explicit URL is kept; without keys nothing is derived.
	c.Update.ManifestURL = "https://rel.example/m.json"
	defaultManifestURL(&c)
	if c.Update.ManifestURL != "https://rel.example/m.json" {
		t.Fatalf("explicit URL replaced: %q", c.Update.ManifestURL)
	}
	d := model.FeederConfig{Uplinks: ups}
	defaultManifestURL(&d)
	if d.Update.ManifestURL != "" {
		t.Fatalf("derived without keys: %q", d.Update.ManifestURL)
	}
	if got := derivedManifestURL([]model.Uplink{{Host: "2001:db8::1"}}); got != "https://[2001:db8::1]/feeder/manifest.json" {
		t.Fatalf("ipv6: %q", got)
	}
}
