package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/config/docgen"
	"airview/internal/model"
)

const regenHint = "generated files are stale: run  go generate ./internal/config"

// Every setting must be documented (doc comment in internal/model or an
// entry in docOverrides).
func TestEverySettingIsDocumented(t *testing.T) {
	var missing []string
	for _, f := range Fields() {
		if strings.TrimSpace(FieldDoc(f.Path)) == "" {
			missing = append(missing, f.Path+" ("+f.DocKey+")")
		}
	}
	if len(missing) > 0 {
		t.Fatalf("undocumented settings (add a doc comment in internal/model or a docOverrides entry):\n%s", strings.Join(missing, "\n"))
	}
}

func TestGeneratedDocsAreCurrent(t *testing.T) {
	docs, err := docgen.Extract(filepath.Join("..", "model"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := docgen.GoSource("config", "modelDocs", docs)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := os.ReadFile("fielddocs_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if normNL(string(cur)) != normNL(string(src)) {
		t.Fatal("fielddocs_gen.go: " + regenHint)
	}
}

func TestExampleFileIsCurrent(t *testing.T) {
	cur, err := os.ReadFile(ExampleFileName)
	if err != nil {
		t.Fatal(err)
	}
	if normNL(string(cur)) != ExampleJSONC() {
		t.Fatal(ExampleFileName + ": " + regenHint)
	}
}

func TestReferenceDocIsCurrent(t *testing.T) {
	cur, err := os.ReadFile(filepath.Join("..", "..", "docs", "CONFIG.md"))
	if err != nil {
		t.Fatal(err)
	}
	if normNL(string(cur)) != ReferenceMarkdown() {
		t.Fatal("docs/CONFIG.md: " + regenHint)
	}
	// Every environment variable is in the reference.
	for _, f := range Fields() {
		if !strings.Contains(string(cur), "`"+f.Env+"`") {
			t.Errorf("docs/CONFIG.md lacks %s", f.Env)
		}
	}
}

// The example parses (as an operator file and as config.json) and yields
// exactly the defaults.
func TestExampleParsesToDefaults(t *testing.T) {
	ex := []byte(ExampleJSONC())
	root, err := ParseJSONC(ex)
	if err != nil {
		t.Fatal(err)
	}
	if is := checkDocument("example", root, SevError); len(is) > 0 {
		t.Fatalf("example has issues: %v", is)
	}
	cfg, warns, _, err := parse(ex)
	if err != nil {
		t.Fatal(err)
	}
	got, notes := Validate(cfg)
	want, _ := Validate(model.DefaultConfig())
	if len(notes) > 0 {
		t.Fatalf("validate notes on the example: %v", notes)
	}
	if !Equal(got, want) {
		t.Fatalf("example != defaults\n got %+v\nwant %+v", got, want)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "comments") {
		t.Fatalf("warnings: %v", warns)
	}

	// As the operator file over a customised config.json it pins everything
	// back to the defaults.
	dir := t.TempDir()
	custom := model.DefaultConfig()
	custom.HTTP.Port = 8080
	custom.Receiver.Name = "Custom"
	if err := Save(dir, custom); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "op.jsonc")
	if err := os.WriteFile(file, ex, 0o600); err != nil {
		t.Fatal(err)
	}
	eff, l, w, err := LoadLayered(LoadOptions{Dir: dir, File: file})
	if err != nil || len(l.Issues()) > 0 {
		t.Fatalf("load: %v %v %v", err, l.Issues(), w)
	}
	if !Equal(eff, want) {
		t.Fatalf("operator example did not pin defaults: %+v", eff.HTTP)
	}
}

func normNL(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
