package aggregatordocs

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"airview/internal/config"
	"airview/internal/model"
)

// guides are the documents owned by WP-DOC-docs and checked here.
var guides = []string{
	"OPERATOR-GUIDE.md",
	"FEEDER-GUIDE.md",
	"PRIVACY-NOTICE.md",
	"LIA.md",
	"CONSENT.md",
	"RETENTION.md",
	"CLEANROOM.md",
	"README.md",
}

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// lineOf returns the 1-based line of byte offset off in s.
func lineOf(s string, off int) int { return strings.Count(s[:off], "\n") + 1 }

// settingRoots are the top-level config.json sections. A backticked span
// starting with one of them followed by "." is a setting path.
var settingRoots = []string{
	"http", "receiver", "antennas", "feeds", "ntp", "weather", "hub", "remote",
	"storage", "ui", "demo", "prediction", "terrain", "navData", "alerts",
	"rfStat", "uplinks", "uplinkTuning", "aggregator",
}

var pathRe = regexp.MustCompile(`^([A-Za-z]+)(\[\d*\])?(\.[A-Za-z][A-Za-z0-9]*(\[\d*\])?)+$`)

// settingPath reports whether span looks like a config.json setting path.
func settingPath(span string) bool {
	m := pathRe.FindStringSubmatch(span)
	if m == nil {
		return false
	}
	switch filepath.Ext(span) {
	case ".json", ".jsonc", ".db", ".md", ".log":
		return false // a file name such as receiver.json, not a setting
	}
	for _, r := range settingRoots {
		if m[1] == r {
			return true
		}
	}
	return false
}

var backtickRe = regexp.MustCompile("`([^`\n]+)`")

// configDefaults parses the generated docs/CONFIG.md tables: path ->
// default as written there (backticks, surrounding quotes and a "(secret)"
// note removed).
func configDefaults(t *testing.T) map[string]string {
	t.Helper()
	src := read(t, filepath.Join("..", "CONFIG.md"))
	row := regexp.MustCompile("(?m)^\\| `([^`]+)` \\| [^|]+ \\| `([^`]*)`[^|]*\\| `(AIRVIEW_[^`]+)` \\|")
	out := map[string]string{}
	for _, m := range row.FindAllStringSubmatch(src, -1) {
		out[m[1]] = unquote(m[2])
	}
	if len(out) < 100 {
		t.Fatalf("parsed only %d settings from docs/CONFIG.md; table format changed?", len(out))
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// genericPath turns "uplinks[0].x" into "uplinks[].x".
func genericPath(p string) string {
	return regexp.MustCompile(`\[\d+\]`).ReplaceAllString(p, "[]")
}

// TestSettingPathsExist: every setting path the guides mention is a real
// setting (config.FieldByPath) and is listed in docs/CONFIG.md.
func TestSettingPathsExist(t *testing.T) {
	defaults := configDefaults(t)
	n := 0
	for _, g := range guides {
		src := read(t, g)
		for _, m := range backtickRe.FindAllStringSubmatchIndex(src, -1) {
			span := src[m[2]:m[3]]
			if !settingPath(span) {
				continue
			}
			n++
			if _, ok := config.FieldByPath(span); !ok {
				t.Errorf("%s:%d: `%s` is not a setting (config.FieldByPath)", g, lineOf(src, m[0]), span)
				continue
			}
			if _, ok := defaults[genericPath(span)]; !ok {
				t.Errorf("%s:%d: `%s` is missing from docs/CONFIG.md", g, lineOf(src, m[0]), span)
			}
		}
	}
	if n < 40 {
		t.Fatalf("only %d setting paths found in the guides; extraction broken?", n)
	}
}

// TestStatedDefaults: "`path` (default `v`)" and "`path` = `v`" statements
// in the guides match the generated defaults.
func TestStatedDefaults(t *testing.T) {
	defaults := configDefaults(t)
	re := regexp.MustCompile("`([^`\n]+)`(?: \\(default | = )`([^`\n]+)`")
	n := 0
	for _, g := range guides {
		src := read(t, g)
		for _, m := range re.FindAllStringSubmatchIndex(src, -1) {
			path, val := src[m[2]:m[3]], src[m[4]:m[5]]
			if !settingPath(path) {
				continue
			}
			n++
			want, ok := defaults[genericPath(path)]
			if !ok {
				t.Errorf("%s:%d: `%s`: no default in docs/CONFIG.md", g, lineOf(src, m[0]), path)
				continue
			}
			if !sameValue(val, want) {
				t.Errorf("%s:%d: `%s` stated default %q, code default %q", g, lineOf(src, m[0]), path, val, want)
			}
		}
	}
	if n < 20 {
		t.Fatalf("only %d default statements found; extraction broken?", n)
	}
}

func sameValue(a, b string) bool {
	a, b = unquote(strings.TrimSpace(a)), unquote(strings.TrimSpace(b))
	if a == b {
		return true
	}
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	return ea == nil && eb == nil && fa == fb
}

// TestRetentionMatchesModel: the published retention schedule states the
// built-in defaults (DESIGN §8.5).
func TestRetentionMatchesModel(t *testing.T) {
	src := read(t, "RETENTION.md")
	d := model.DefaultAggregator()
	want := map[string]int{
		"aggregator.retention.connLogDays":        d.Retention.ConnLogDays,
		"aggregator.retention.minuteDays":         d.Retention.MinuteDays,
		"aggregator.retention.hourMonths":         d.Retention.HourMonths,
		"aggregator.retention.offlinePurgeDays":   d.Retention.OfflinePurgeDays,
		"aggregator.retention.auditMonths":        d.Retention.AuditMonths,
		"aggregator.retention.estimatesPerMethod": d.Retention.EstimatesPerMethod,
		"aggregator.auth.ownerSessionDays":        d.Auth.OwnerSessionDays,
		"aggregator.auth.ownerIdleDays":           d.Auth.OwnerIdleDays,
		"aggregator.auth.adminSessionHours":       d.Auth.AdminSessionHours,
		"aggregator.auth.adminIdleMinutes":        d.Auth.AdminIdleMinutes,
	}
	for path, v := range want {
		s := "`" + path + "` = `" + strconv.Itoa(v) + "`"
		if !strings.Contains(src, s) {
			t.Errorf("RETENTION.md does not state %s", s)
		}
	}
	// DESIGN §8.5 values that the privacy notice repeats in prose.
	notice := read(t, "PRIVACY-NOTICE.md")
	for _, s := range []string{
		"| Connection log | " + strconv.Itoa(d.Retention.ConnLogDays) + " days |",
		"| Per-minute statistics of your station | " + strconv.Itoa(d.Retention.MinuteDays) + " days |",
		"| Hourly statistics, range and coverage of your station | " + strconv.Itoa(d.Retention.HourMonths) + " months |",
		"the latest " + strconv.Itoa(d.Retention.EstimatesPerMethod) + " of each method",
		"deleted after " + strconv.Itoa(d.Retention.OfflinePurgeDays) + " days",
		"| Audit log | " + strconv.Itoa(d.Retention.AuditMonths) + " months",
	} {
		if !strings.Contains(notice, s) {
			t.Errorf("PRIVACY-NOTICE.md section 6 does not contain %q", s)
		}
	}
}

// TestPrivacyDefaultsMatchDecisions: the user decisions of DESIGN §19 that
// the guides describe are the code defaults.
func TestPrivacyDefaultsMatchDecisions(t *testing.T) {
	d := model.DefaultAggregator()
	if d.DefaultPin != model.PinHidden {
		t.Errorf("default pin %q, guides say hidden (DESIGN §19.2)", d.DefaultPin)
	}
	if !d.MLAT.AutoUseSurvey || d.MLAT.AutoUseMaxSigmaM != 50 {
		t.Errorf("autoUseSurvey %v / %v m, guides say on at 50 m (DESIGN §19.3)", d.MLAT.AutoUseSurvey, d.MLAT.AutoUseMaxSigmaM)
	}
	if d.Admission != "invite" {
		t.Errorf("admission %q, guides say invite", d.Admission)
	}
	if d.IslandGuard {
		t.Error("islandGuard on by default, guides say off")
	}
	u := model.DefaultUplink()
	if u.Pin != model.PinHidden || u.ShareGNSS || !u.Timestamps || u.PublicStats || u.ShowName {
		t.Errorf("uplink defaults %+v differ from FEEDER-GUIDE.md §1/§6", u)
	}
	if model.DefaultAVFPort != 30160 || model.DefaultMLATCPort != 31090 {
		t.Errorf("ports %d/%d differ from the guides' 30160/31090", model.DefaultAVFPort, model.DefaultMLATCPort)
	}
}

var envRe = regexp.MustCompile(`AIRVIEW_[A-Z0-9_]*[A-Z0-9]`)

// TestEnvVarsExist: every AIRVIEW_* variable in the guides exists.
func TestEnvVarsExist(t *testing.T) {
	cfg := read(t, filepath.Join("..", "CONFIG.md"))
	known := map[string]bool{"AIRVIEW_CONFIG": true, "AIRVIEW_DATA": true, "AIRVIEW_DEBUG": true}
	for _, v := range envRe.FindAllString(cfg, -1) {
		known[v] = true
	}
	for _, g := range guides {
		src := read(t, g)
		for _, m := range envRe.FindAllStringIndex(src, -1) {
			v := src[m[0]:m[1]]
			if m[1] < len(src) && src[m[1]] == '<' || m[1] < len(src) && src[m[1]] == '_' {
				continue // a pattern such as AIRVIEW_<NAME>_FILE or AIRVIEW_UPLINKS_<n>
			}
			base := strings.TrimSuffix(v, "_FILE")
			if !known[v] && !known[base] {
				t.Errorf("%s:%d: unknown environment variable %s", g, lineOf(src, m[0]), v)
			}
		}
	}
}

// TestConsentTextMatchesDesign: consent text C1 is DESIGN §10.5 word for
// word, and the privacy notice and the feeder guide quote it verbatim.
func TestConsentTextMatchesDesign(t *testing.T) {
	design := read(t, "DESIGN.md")
	i := strings.Index(design, "### 10.5 Consent text")
	if i < 0 {
		t.Fatal("DESIGN.md §10.5 not found")
	}
	m := regexp.MustCompile(`(?m)^> (".+")$`).FindStringSubmatch(design[i:])
	if m == nil {
		t.Fatal("no quoted consent text in DESIGN.md §10.5")
	}
	c1 := m[1]
	for _, g := range []string{"CONSENT.md", "PRIVACY-NOTICE.md", "FEEDER-GUIDE.md"} {
		if !strings.Contains(read(t, g), "> "+c1) {
			t.Errorf("%s does not quote DESIGN §10.5 verbatim: %s", g, c1)
		}
	}
	// The places that show C1 to feeder owners carry the same words.
	plain := strings.Trim(c1, `"`)
	for _, f := range []string{
		filepath.Join("..", "..", "web", "static", "js", "net", "view.js"),
		filepath.Join("..", "..", "deploy", "feeder", "pi", "install.sh"),
		filepath.Join("..", "..", "cmd", "airview-feeder", "setup.go"),
	} {
		if !strings.Contains(read(t, f), plain) {
			t.Errorf("%s does not carry consent text C1 verbatim", f)
		}
	}
}

var linkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// TestRelativeLinks: relative Markdown links resolve to existing files.
func TestRelativeLinks(t *testing.T) {
	for _, g := range guides {
		src := read(t, g)
		for _, m := range linkRe.FindAllStringSubmatchIndex(src, -1) {
			target := src[m[2]:m[3]]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			if _, err := os.Stat(filepath.FromSlash(target)); err != nil {
				t.Errorf("%s:%d: broken link %s", g, lineOf(src, m[0]), target)
			}
		}
	}
}

// TestOperatorExampleIsValid: the operator file of OPERATOR-GUIDE.md §3.3
// passes "airview config check" without errors or warnings.
func TestOperatorExampleIsValid(t *testing.T) {
	src := read(t, "OPERATOR-GUIDE.md")
	m := regexp.MustCompile("(?s)```jsonc\n(// /etc/airview/airview.jsonc.*?)```").FindStringSubmatch(src)
	if m == nil {
		t.Fatal("operator file example not found in OPERATOR-GUIDE.md")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "airview.jsonc")
	if err := os.WriteFile(file, []byte(m[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := config.Check(config.LoadOptions{Dir: dir, File: file, ReadOnly: true})
	for _, is := range rep.Issues {
		t.Errorf("operator example: %+v", is)
	}
	eff := rep.Effective
	if eff.Mode != model.ModeAggregator || eff.HTTP.Port != 8080 || eff.HTTP.BindAddr != "127.0.0.1" {
		t.Errorf("operator example: effective mode %q http %+v", eff.Mode, eff.HTTP)
	}
	if eff.Aggregator.Admission != "invite" || len(eff.Aggregator.TrustedProxies) != 2 {
		t.Errorf("operator example: aggregator %+v", eff.Aggregator)
	}
}
