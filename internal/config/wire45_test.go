package config

import (
	"strings"
	"testing"
)

// TestSSRBlockLayers: the experimental ssr block from the environment and
// -set in standalone mode, its list settings, and the checks.
func TestSSRBlockLayers(t *testing.T) {
	dir := t.TempDir()
	r := Check(LoadOptions{Dir: dir, NoFile: true,
		Environ: []string{"AIRVIEW_SSR_ENABLED=true", "AIRVIEW_SSR_MATCH_TOL_NS=300", "AIRVIEW_SSR_TIMING_SIGMA_AC_NS=180",
			"AIRVIEW_SSR_SITES_0_NAME=Claxby", "AIRVIEW_SSR_SITES_0_LAT=53.45", "AIRVIEW_SSR_SITES_0_LON=-0.38", "AIRVIEW_SSR_SITES_0_HEIGHT_M=170"},
		Sets: []string{"ssr.feedId=evo", "ssr.modeAcCodeFormat=nibbles", "ssr.conspicuityCodes=[\"7000\",\"2000\"]"}})
	for _, is := range r.Issues {
		if is.Severity == SevError {
			t.Errorf("issue: %v", is)
		}
	}
	s := r.Effective.SSR
	if !s.Enabled || s.MatchTolNs != 300 || s.TimingSigmaACNs != 180 || s.FeedID != "evo" || s.ModeACCodeFormat != "nibbles" {
		t.Fatalf("ssr %+v\n%s", s, issuesText(r.Issues))
	}
	if len(s.Sites) != 1 || s.Sites[0].Name != "Claxby" || s.Sites[0].HeightM != 170 || len(s.ConspicuityCodes) != 2 {
		t.Fatalf("lists %+v %v", s.Sites, s.ConspicuityCodes)
	}

	r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: []string{"ssr.enabled=true", "ssr.sites=[{\"name\":\"x\",\"lat\":95,\"lon\":1}]",
		"ssr.conspicuityCodes=[\"7008\"]", "ssr.modeAcCodeFormat=hex", "ssr.minPeriodS=10", "ssr.maxPeriodS=5"}})
	text := issuesText(r.Issues)
	for _, want := range []string{"ssr.sites[0]", "conspicuityCodes[0]", "modeAcCodeFormat", "maxPeriodS"} {
		if !strings.Contains(text, want) {
			t.Errorf("no issue for %s:\n%s", want, text)
		}
	}
	if s := r.Effective.SSR; len(s.Sites) != 0 || len(s.ConspicuityCodes) != 0 || s.ModeACCodeFormat != "" || s.MaxPeriodS != 0 {
		t.Fatalf("bad values kept: %+v", s)
	}
}

// TestLatencyCalBlock: aggregator.mlat.latencyCal from the environment,
// and its cross-check.
func TestLatencyCalBlock(t *testing.T) {
	dir := t.TempDir()
	r := Check(LoadOptions{Dir: dir, NoFile: true, Environ: []string{"AIRVIEW_MODE=aggregator",
		"AIRVIEW_AGGREGATOR_MLAT_LATENCY_CAL_ENABLED=true", "AIRVIEW_AGGREGATOR_MLAT_LATENCY_CAL_MIN_GAIN=0.1"}})
	c := r.Effective.Aggregator.MLAT.LatencyCal
	if !c.Enabled || c.MinGain != 0.1 {
		t.Fatalf("latencyCal %+v\n%s", c, issuesText(r.Issues))
	}
	r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: []string{"mode=aggregator",
		"aggregator.mlat.latencyCal.zEnable=2", "aggregator.mlat.latencyCal.zDisable=5"}})
	if !issueAt(r, "aggregator.mlat.latencyCal.zDisable") || r.Effective.Aggregator.MLAT.LatencyCal.ZDisable != 0 {
		t.Fatalf("zDisable above zEnable not reported:\n%s", issuesText(r.Issues))
	}
}
