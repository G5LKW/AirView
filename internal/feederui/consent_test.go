package feederui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestConsentTextsMatchDoc: the C2-C4 texts next to the pin, name and
// statistics settings of the status page are CONSENT.md word for word.
func TestConsentTextsMatchDoc(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "aggregator", "CONSENT.md"))
	if err != nil {
		t.Fatal(err)
	}
	sec := map[string]string{}
	id := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(doc), "\r\n", "\n"), "\n") {
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
	js, err := os.ReadFile(filepath.Join("static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"C1", "C2", "C3", "C4"} {
		m := regexp.MustCompile("const CONSENT_" + id + " = `([^`]*)`").FindSubmatch(js)
		if m == nil {
			t.Errorf("%s: not in app.js", id)
			continue
		}
		if want := norm(sec[id]); want == "" || norm(string(m[1])) != want {
			t.Errorf("%s differs from CONSENT.md:\n got  %q\n want %q", id, norm(string(m[1])), want)
		}
		if !strings.Contains(string(js), "consentHTML(CONSENT_"+id+")") {
			t.Errorf("%s is not shown", id)
		}
	}
}
