package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// tunablePrefixes are the file-configurable MLAT, mlat-client, ingest and
// storage tunables (DESIGN §20.3). Every leaf below them must be settable
// from the environment and checked by `airview config check`.
var tunablePrefixes = []string{
	"aggregator.mlat.", "aggregator.mlatc.", "aggregator.ingest.", "aggregator.storage.", "aggregator.positions.",
	"update.", "locate.", "aggregator.federation.", "ssr.", "storage.tuning.",
}

// negativeAllowed lists the tunables where a negative value has a meaning
// (off / none / below the ellipsoid) and is therefore accepted silently.
var negativeAllowed = map[string]bool{
	"aggregator.mlat.solve.minHaeM":              true, // heights below the ellipsoid
	"aggregator.mlat.track.maxExclude":           true, // none
	"aggregator.mlat.engine.verifyEverySec":      true, // -1 = off
	"aggregator.mlat.engine.syncWarmupSec":       true, // -1 = none
	"aggregator.mlat.clock.stepRunCount":         true, // -1 = off
	"aggregator.mlat.clock.relGaugeBetaSigmaNs":  true, // off
	"aggregator.ingest.clockGateFloorUs":         true, // -1 = no floor
	"aggregator.ingest.avfHostSlackMs":           true, // -1 = off
	"aggregator.positions.holdMaxDays":           true, // never
	"aggregator.mlat.clock.ppsCorrelatedNs":      true, // off
	"aggregator.mlat.survey.ownerMinIntervalMin": true, // no limit
	"aggregator.mlat.clock.externalStates":       true, // off
	"aggregator.mlat.clock.partitionDriftNs":     true, // off
	"aggregator.mlat.engine.modeAcJitterNs":      true, // none
	"ssr.radarHeightM":                           true, // below the ellipsoid
}

// effectiveAt reads a dotted path from the effective config.
func effectiveAt(t *testing.T, r Report, path string) any {
	t.Helper()
	b, err := json.Marshal(r.Effective)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var cur any = doc
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func issueAt(r Report, path string) bool {
	for _, is := range r.Issues {
		if is.Path == path {
			return true
		}
	}
	return false
}

// TestCheckCoversEveryTunable: each MLAT / ingest / storage tunable has an
// AIRVIEW_* variable that config check resolves and applies, and a value
// AirView would correct is reported at that setting.
func TestCheckCoversEveryTunable(t *testing.T) {
	dir := t.TempDir()
	seen := 0
	for _, f := range Fields() {
		ok := false
		for _, p := range tunablePrefixes {
			ok = ok || strings.HasPrefix(f.Path, p)
		}
		if !ok || f.InList || (f.Kind != KindInt && f.Kind != KindFloat && f.Kind != KindBool) {
			continue
		}
		seen++
		val := map[FieldKind]string{KindInt: "7", KindFloat: "0.37", KindBool: "true"}[f.Kind]
		r := Check(LoadOptions{Dir: dir, NoFile: true, Environ: []string{"AIRVIEW_MODE=aggregator", f.Env + "=" + val}})
		for _, is := range r.Issues {
			if strings.Contains(is.Source, f.Env) && is.Severity == SevError && strings.Contains(is.Msg, "unknown") {
				t.Errorf("%s: env not known: %v", f.Path, is)
			}
		}
		got := fmt.Sprint(effectiveAt(t, r, f.Path))
		if got != val && !issueAt(r, f.Path) {
			t.Errorf("%s=%s via %s: effective %s and no issue reported", f.Path, val, f.Env, got)
		}
		if f.Kind == KindBool {
			continue
		}
		r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: []string{"mode=aggregator", f.Path + "=-3"}})
		got = fmt.Sprint(effectiveAt(t, r, f.Path))
		switch {
		case negativeAllowed[f.Path]:
			if issueAt(r, f.Path) && got == "-3" {
				t.Errorf("%s: accepted negative value also reported", f.Path)
			}
		case !issueAt(r, f.Path):
			t.Errorf("%s=-3: not reported by config check (effective %s)", f.Path, got)
		case got == "-3":
			t.Errorf("%s=-3: reported but kept", f.Path)
		}
	}
	if seen < 150 {
		t.Fatalf("only %d tunables found", seen)
	}
}

// TestMLATClockBlockLayers: the new clock block from the file, the
// environment and -set.
func TestMLATClockBlockLayers(t *testing.T) {
	dir := t.TempDir()
	r := Check(LoadOptions{Dir: dir, NoFile: true,
		Environ: []string{"AIRVIEW_MODE=aggregator", "AIRVIEW_AGGREGATOR_MLAT_CLOCK_STEP_COUNT=8"},
		Sets:    []string{"aggregator.mlat.clock.gateProb=0.995", "aggregator.mlat.clock.noJitterLearning=true"}})
	c := r.Effective.Aggregator.MLAT.Clock
	if c.StepCount != 8 || c.GateProb != 0.995 || !c.NoJitterLearning {
		t.Fatalf("clock %+v\n%s", c, issuesText(r.Issues))
	}
	r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: []string{"mode=aggregator", "aggregator.mlat.clock.gateProb=1"}})
	if !issueAt(r, "aggregator.mlat.clock.gateProb") || r.Effective.Aggregator.MLAT.Clock.GateProb != 0 {
		t.Fatalf("gateProb 1 not reported:\n%s", issuesText(r.Issues))
	}
}
