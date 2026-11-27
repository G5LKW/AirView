package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// consentSections reads docs/aggregator/CONSENT.md and returns, per ID
// (C1..C8), the text of its block quotes.
func consentSections(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "docs", "aggregator", "CONSENT.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	head := regexp.MustCompile(`^## (C[1-8])\.`)
	id := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if m := head.FindStringSubmatch(line); m != nil {
			id = m[1]
			continue
		}
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "---") {
			id = ""
		}
		if id != "" && strings.HasPrefix(line, ">") {
			out[id] += strings.TrimSpace(strings.TrimPrefix(line, ">")) + "\n"
		}
	}
	return out
}

// consentFragments splits a Markdown block quote into the plain fragments
// between its markup (bold, list bullets, link brackets, outer quotes).
func consentFragments(md string) []string {
	var out []string
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "- ")
		line = strings.Trim(line, `"`)
		for _, f := range regexp.MustCompile(`\*\*|\[|\]`).Split(line, -1) {
			f = strings.TrimSpace(f)
			if len(f) > 3 {
				out = append(out, f)
			}
		}
	}
	return out
}

// TestConsentTextsMatchDoc: the web UI shows the consent texts of
// CONSENT.md word for word (consent.js), and view.js's CONSENT_TEXT is C1.
func TestConsentTextsMatchDoc(t *testing.T) {
	secs := consentSections(t)
	if len(secs) != 8 {
		t.Fatalf("CONSENT.md: %d texts found, want C1..C8", len(secs))
	}
	js, err := fs.ReadFile(Static(), "js/net/consent.js")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(js), `\'`, "'")
	for id, md := range secs {
		frags := consentFragments(md)
		if len(frags) == 0 {
			t.Errorf("%s: no text", id)
		}
		for _, f := range frags {
			if !strings.Contains(src, f) {
				t.Errorf("%s: consent.js lacks %q", id, f)
			}
		}
	}
	view, err := fs.ReadFile(Static(), "js/net/view.js")
	if err != nil {
		t.Fatal(err)
	}
	c1 := strings.Trim(strings.TrimSpace(secs["C1"]), `"`)
	if !strings.Contains(string(view), c1) {
		t.Error("view.js CONSENT_TEXT is not C1")
	}
}

// TestConsentTextsPlaced: every text is used by the page CONSENT.md names.
func TestConsentTextsPlaced(t *testing.T) {
	where := map[string][]string{
		"C1": {"js/pages/claim.js", "js/pages/feeder.js"},
		"C2": {"js/pages/me.js"},
		"C3": {"js/pages/me.js"},
		"C4": {"js/pages/me.js"},
		"C5": {"js/pages/feeder.js", "js/pages/claim.js"},
		"C6": {"js/pages/claim.js", "js/pages/me.js"},
		"C7": {"js/net/auth.js"},
		"C8": {"js/pages/me.js", "js/pages/account.js"},
	}
	for id, files := range where {
		for _, f := range files {
			b, err := fs.ReadFile(Static(), f)
			if err != nil {
				t.Fatal(err)
			}
			s := string(b)
			if !strings.Contains(s, "consentBlock('"+id+"'") && !(id == "C6" && strings.Contains(s, "C6_NOTICE")) &&
				!(id == "C1" && strings.Contains(s, "consentBlock('C1'")) {
				t.Errorf("%s does not show %s", f, id)
			}
		}
	}
}
