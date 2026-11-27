package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WP-E2: aggregator.mlat.enabled / returnResults / holdout from the file,
// the environment and -set, with config check.
func TestMLATSettingsFromEveryLayer(t *testing.T) {
	dir := t.TempDir()
	op := filepath.Join(dir, OperatorFileName)
	src := `{
  // operator file
  "mode": "aggregator",
  "aggregator": {"mlat": {"enabled": false, "returnResults": false, "holdout": 0.2}}
}`
	if err := os.WriteFile(op, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Check(LoadOptions{Dir: dir})
	m := r.Effective.Aggregator.MLAT
	if m.Enabled || m.ReturnResults || m.Holdout != 0.2 {
		t.Fatalf("file: %+v", m)
	}
	// The environment overrides the file, -set overrides the environment.
	r = Check(LoadOptions{Dir: dir, Environ: []string{
		"AIRVIEW_AGGREGATOR_MLAT_ENABLED=true", "AIRVIEW_AGGREGATOR_MLAT_RETURN_RESULTS=true", "AIRVIEW_AGGREGATOR_MLAT_HOLDOUT=0.3",
	}, Sets: []string{"aggregator.mlat.holdout=0.05"}})
	m = r.Effective.Aggregator.MLAT
	if !m.Enabled || !m.ReturnResults || m.Holdout != 0.05 {
		t.Fatalf("env/-set: %+v\n%s", m, issuesText(r.Issues))
	}
	locked := map[string]string{}
	for _, o := range r.Overrides {
		locked[o.Path] = o.Origin
	}
	if !strings.Contains(locked["aggregator.mlat.enabled"], "AIRVIEW_AGGREGATOR_MLAT_ENABLED") ||
		!strings.Contains(locked["aggregator.mlat.holdout"], "-set") {
		t.Fatalf("overrides %v", locked)
	}
	// Out of range and wrong types are errors at their source.
	r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: []string{"mode=aggregator", "aggregator.mlat.holdout=0.7"}})
	if !findIssue(r, SevError, "", 0, 0, "aggregator.mlat.holdout", "out of range") {
		t.Fatalf("holdout range:\n%s", issuesText(r.Issues))
	}
	r = Check(LoadOptions{Dir: dir, NoFile: true, Environ: []string{"AIRVIEW_MODE=aggregator", "AIRVIEW_AGGREGATOR_MLAT_ENABLED=maybe"}})
	if r.Errors() == 0 {
		t.Fatalf("bad bool accepted:\n%s", issuesText(r.Issues))
	}
}

func TestCheckMLATCombinations(t *testing.T) {
	dir := t.TempDir()
	has := func(r Report, path, msg string) bool { return findIssue(r, SevWarning, "", 0, 0, path, msg) }
	base := []string{"mode=aggregator", "http.bindAddr=127.0.0.1"}

	r := Check(LoadOptions{Dir: dir, NoFile: true, Sets: base})
	for _, is := range r.Issues {
		if strings.HasPrefix(is.Path, "aggregator.mlat") || is.Path == "aggregator.mlatPort" || is.Path == "aggregator.sbsInLocalPort" {
			t.Fatalf("defaults warn: %v", is)
		}
	}
	r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: append(base, "aggregator.mlat.enabled=false")})
	if !has(r, "aggregator.mlatPort", "stays closed") || !has(r, "aggregator.mlat.returnResults", "no effect") {
		t.Fatalf("engine off:\n%s", issuesText(r.Issues))
	}
	r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: append(base, "aggregator.avfPort=0", "aggregator.mlatPort=0")})
	if !has(r, "aggregator.mlat.enabled", "no timed input") {
		t.Fatalf("no timed input:\n%s", issuesText(r.Issues))
	}
	r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: append(base, "aggregator.sbsInLocalPort=30105", "aggregator.mlat.holdout=0")})
	if !has(r, "aggregator.sbsInLocalPort", "mlat-ext") || !has(r, "aggregator.mlat.holdout", "self-validation off") {
		t.Fatalf("bridge/holdout:\n%s", issuesText(r.Issues))
	}
	// Standalone: none of this applies.
	r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: []string{"aggregator.mlat.enabled=false", "aggregator.mlat.holdout=0"}})
	if len(mlatIssues(r.Effective)) != 0 {
		t.Fatal("warnings in standalone mode")
	}
}
