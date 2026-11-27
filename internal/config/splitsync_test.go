package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSplitSyncLegacyBool: a config file with the old boolean
// aggregator.mlatc.splitSync still loads (true = "on") with a warning, and
// "off" is accepted.
func TestSplitSyncLegacyBool(t *testing.T) {
	dir := t.TempDir()
	for in, want := range map[string]string{`true`: "on", `"off"`: "off", `"auto"`: "auto"} {
		f := filepath.Join(dir, "op.jsonc")
		if err := os.WriteFile(f, []byte(`{"mode":"aggregator","aggregator":{"mlatc":{"splitSync":`+in+`}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		r := Check(LoadOptions{Dir: dir, File: f})
		for _, is := range r.Issues {
			if is.Severity == SevError {
				t.Errorf("%s: %v", in, is)
			}
		}
		if got := effectiveAt(t, r, "aggregator.mlatc.splitSync"); got != want {
			t.Errorf("%s: effective %v, want %s", in, got, want)
		}
	}
}
