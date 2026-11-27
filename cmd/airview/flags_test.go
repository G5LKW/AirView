package main

import (
	"io"
	"slices"
	"testing"
)

func TestParseFlagsConfigLayers(t *testing.T) {
	f, err := parseFlags([]string{"-config", "/etc/airview/airview.jsonc", "-set", "http.port=8080", "-set", "ui.units=metric", "-headless"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if f.config != "/etc/airview/airview.jsonc" || !slices.Equal(f.sets, []string{"http.port=8080", "ui.units=metric"}) || !f.headless {
		t.Fatalf("flags: %+v", f)
	}
	if _, err := parseFlags([]string{"-set", "nonsense"}, io.Discard); err == nil {
		t.Fatal("-set without = accepted")
	}
}
