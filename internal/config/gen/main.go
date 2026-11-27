// Command gen regenerates the files derived from the configuration structs:
//
//	internal/config/fielddocs_gen.go     doc comments of internal/model
//	internal/config/airview.example.jsonc  commented example configuration
//	docs/CONFIG.md                        configuration reference
//
// Run it with "go generate ./internal/config" after changing a field or a
// doc comment in internal/model. The config package tests fail while the
// generated files are stale.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"airview/internal/config"
	"airview/internal/config/docgen"
)

func main() {
	pkgDir := flag.String("pkg", ".", "the internal/config directory")
	flag.Parse()
	if err := run(*pkgDir); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	docs, err := docgen.Extract(filepath.Join(dir, "..", "model"))
	if err != nil {
		return err
	}
	src, err := docgen.GoSource("config", "modelDocs", docs)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		filepath.Join(dir, "fielddocs_gen.go"):              src,
		filepath.Join(dir, config.ExampleFileName):          []byte(config.RenderExample(docs)),
		filepath.Join(dir, "..", "..", "docs", "CONFIG.md"): []byte(config.RenderReference(docs)),
	}
	for path, data := range files {
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
			continue
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	return nil
}
