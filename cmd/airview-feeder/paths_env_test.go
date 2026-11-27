package main

import "testing"

func TestDefaultPathsFromMainEnv(t *testing.T) {
	old := getenv
	defer func() { getenv = old }()
	env := map[string]string{"AIRVIEW_CONFIG": "/etc/x/feeder.json", "AIRVIEW_DATA": "/var/lib/x"}
	getenv = func(k string) string { return env[k] }
	if got := defaultConfigFile(""); got != "/etc/x/feeder.json" {
		t.Errorf("config %q", got)
	}
	if got := defaultDataDir(); got != "/var/lib/x" {
		t.Errorf("data %q", got)
	}
	// The feeder's own names win.
	env["AIRVIEW_FEEDER_CONFIG"], env["AIRVIEW_FEEDER_DATA"] = "/a.json", "/b"
	if defaultConfigFile("") != "/a.json" || defaultDataDir() != "/b" {
		t.Error("AIRVIEW_FEEDER_* must take precedence")
	}
}
