package config

import (
	"testing"

	"airview/internal/model"
)

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"": "",
		"http://u:p@127.0.0.1:1/data/aircraft.json?apikey=K": "http://127.0.0.1:1/data/aircraft.json",
		"https://h.example/x#frag":                           "https://h.example/x",
		"192.168.1.5:30005":                                  "192.168.1.5:30005",
		"simulator":                                          "simulator",
		"u:p@host/x?k=1":                                     "host/x",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactPublic(t *testing.T) {
	cfg := model.DefaultConfig()
	cfg.Feeds = []model.Feed{{ID: "j", Type: model.FeedJSON, URL: "http://u:pw@h/a.json?apikey=K"}}
	cfg.NavData.PhotoContact = "owner@example.org"
	out := RedactPublic(cfg)
	if out.Feeds[0].URL != "http://h/a.json" || out.NavData.PhotoContact != SecretSet {
		t.Fatalf("RedactPublic: %q %q", out.Feeds[0].URL, out.NavData.PhotoContact)
	}
	if cfg.Feeds[0].URL != "http://u:pw@h/a.json?apikey=K" {
		t.Fatal("RedactPublic changed its input")
	}
}
