package docgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	src := `package m

// Thing configures things.
type Thing struct {
	// Port is the port
	// (default 80).
	Port int ` + "`json:\"port\"`" + `
	Name string ` + "`json:\"name\"`" + ` // display name
	// Both has both.
	Both bool ` + "`json:\"both\"`" + ` // trailing
	A, B int
	none int
}

type (
	// Grouped doc.
	G struct{ X int }
)
`
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "m_test.go"), []byte("package m\n// T doc.\ntype T struct{ Y int // y\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	docs, err := Extract(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Thing":      "Thing configures things.",
		"Thing.Port": "Port is the port (default 80).",
		"Thing.Name": "display name",
		"Thing.Both": "Both has both. trailing",
		"G":          "Grouped doc.",
	}
	for k, v := range want {
		if docs[k] != v {
			t.Errorf("%s = %q, want %q", k, docs[k], v)
		}
	}
	if _, ok := docs["T"]; ok {
		t.Error("test files must be skipped")
	}
	if len(docs) != len(want) {
		t.Errorf("extra entries: %v", docs)
	}
	src2, err := GoSource("p", "v", docs)
	if err != nil || !strings.Contains(string(src2), "DO NOT EDIT") || !strings.Contains(string(src2), `"Thing.Port": "Port is the port (default 80).",`) {
		t.Fatalf("GoSource: %v\n%s", err, src2)
	}
}
