package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/model"
)

func TestParseLength(t *testing.T) {
	for in, want := range map[string]float64{"120m": 120, "120 m": 120, "400ft": 121.92, "400 FT": 121.92, "-10m": -10} {
		v, err := parseLength(in, false)
		if err != nil || v < want-0.01 || v > want+0.01 {
			t.Errorf("%s -> %v %v", in, v, err)
		}
	}
	if _, err := parseLength("120", false); err == nil {
		t.Error("bare altitude accepted")
	}
	if v, err := parseLength("8", true); err != nil || v != 8 {
		t.Error("bare height")
	}
	if _, err := parseLength("xm", true); err == nil {
		t.Error("garbage")
	}
}

func TestSetupNonInteractive(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "feeder.json")
	var out, errb bytes.Buffer
	code := runMain([]string{"setup", "-config", cf, "-data", dir, "-non-interactive",
		"-server", "feed.example.org", "-pin", "sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "-enrol", "TOK", "-name", "loft-1",
		"-lat", "51.5", "-lon", "-0.12", "-alt", "400ft", "-agl", "8m", "-input", "192.168.1.20:30005",
		"-privacy", "region", "-no-timestamps", "-readsb-push", "127.0.0.1:30104"}, &out, &errb)
	if code != 0 {
		t.Fatalf("setup: %d %s %s", code, out.String(), errb.String())
	}
	lc, err := loadConfig(loadOptions{File: cf})
	if err != nil {
		t.Fatal(err)
	}
	c := lc.Eff
	u := c.Uplinks[0]
	if u.ID != "feed-example-org" || u.Host != "feed.example.org" || u.Port != 30160 || u.ServerPin != "sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" || u.EnrolToken != "TOK" || u.Pin != "region" || u.Timestamps {
		t.Fatalf("uplink %+v", u)
	}
	r := c.Receivers[0]
	if r.Name != "loft-1" || !r.Located || r.AltM != 121.9 || r.AltRef != "msl" || r.AGLM != 8 || r.Input.Type != model.FeedBeast || r.Input.Host != "192.168.1.20" || r.Input.Port != 30005 {
		t.Fatalf("receiver %+v", r)
	}
	if c.Results.ReadsbAddr != "127.0.0.1:30104" {
		t.Fatal("readsb push")
	}

	// re-running keeps the values and changes only what is given
	code = runMain([]string{"setup", "-config", cf, "-non-interactive", "-timestamps", "-input", "auto", "-no-location"}, &out, &errb)
	if code != 0 {
		t.Fatalf("rerun: %d %s", code, errb.String())
	}
	lc, _ = loadConfig(loadOptions{File: cf})
	c = lc.Eff
	if len(c.Uplinks) != 1 || !c.Uplinks[0].Timestamps || c.Uplinks[0].EnrolToken != "TOK" || c.Receivers[0].Input.Type != model.FeedAuto || c.Receivers[0].Located || c.Receivers[0].Name != "loft-1" {
		t.Fatalf("rerun result %+v", c)
	}

	// bad input is refused without writing
	if code := runMain([]string{"setup", "-config", cf, "-non-interactive", "-name", "bad name!"}, &out, &errb); code != 2 {
		t.Fatalf("bad name: %d", code)
	}
	if code := runMain([]string{"setup", "-config", cf, "-non-interactive", "-lat", "51", "-lon", "0.1", "-alt", "100"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "unit") {
		t.Fatalf("altitude without unit: %d %s", code, errb.String())
	}
}

func TestSetupInteractive(t *testing.T) {
	cfg := model.DefaultFeederConfig()
	answers := strings.Join([]string{
		"agg.example:31000", // server
		"",                  // pin
		"",                  // enrol token
		"",                  // privacy (hidden)
		"y",                 // publish statistics (C4)
		"n",                 // show the name (C3)
		"y",                 // timestamps
		"Roof",              // name
		"",                  // input auto
		"",                  // latitude: leave blank
		"5m",                // agl
	}, "\n") + "\n"
	var out bytes.Buffer
	p := prompter{in: bufio.NewReader(strings.NewReader(answers)), out: &out}
	if err := applySetup(&cfg, setupOptions{given: map[string]bool{}, resultsPort: -1}, &p); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if cfg.Uplinks[0].Host != "agg.example" || cfg.Uplinks[0].Port != 31000 || !cfg.Uplinks[0].Timestamps || cfg.Receivers[0].Name != "Roof" || cfg.Receivers[0].AGLM != 5 || cfg.Receivers[0].Located {
		t.Fatalf("%+v %+v", cfg.Uplinks, cfg.Receivers)
	}
	if !strings.Contains(out.String(), "network operator can determine its location") {
		t.Fatal("consent text not shown")
	}
	for id, c := range map[string]string{"C2": consentPin, "C3": consentName, "C4": consentStats} {
		if !strings.Contains(out.String(), c) {
			t.Errorf("%s not shown", id)
		}
	}
	if !cfg.Uplinks[0].PublicStats || cfg.Uplinks[0].ShowName {
		t.Fatalf("publicStats/showName %+v", cfg.Uplinks[0])
	}
}

// TestFeederConsentTextsMatchDoc: C1-C4 of the setup are CONSENT.md word
// for word.
func TestFeederConsentTextsMatchDoc(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "aggregator", "CONSENT.md"))
	if err != nil {
		t.Fatal(err)
	}
	sec := map[string]string{}
	id := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "## C") && len(line) > 5 {
			id = line[3:5]
			continue
		}
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "---") {
			id = ""
		}
		if id != "" && strings.HasPrefix(line, ">") {
			l := strings.TrimSpace(strings.TrimPrefix(line, ">"))
			l = strings.TrimPrefix(l, "- ")
			l = strings.Trim(strings.ReplaceAll(l, "**", ""), `"`)
			sec[id] += l + "\n"
		}
	}
	norm := func(s string) string {
		s = strings.ReplaceAll(s, "  - ", "")
		return strings.Join(strings.Fields(s), " ")
	}
	for id, got := range map[string]string{"C1": consentText, "C2": consentPin, "C3": consentName, "C4": consentStats} {
		if want := norm(sec[id]); want == "" || norm(got) != want {
			t.Errorf("%s differs from CONSENT.md:\n got  %q\n want %q", id, norm(got), want)
		}
	}
}

func TestSlugFromHost(t *testing.T) {
	for in, want := range map[string]string{"Feed.Example.org": "feed-example-org", "10.0.0.1": "10-0-0-1", "::1": "1", "--": "aggregator"} {
		if got := slugFromHost(in); got != want {
			t.Errorf("%s -> %s", in, got)
		}
	}
}
