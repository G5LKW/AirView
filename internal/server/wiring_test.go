package server

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// internal/server/WIRING.md carries a generated table of the network
// routes (routes_net.go) with their access class. Regenerate with
//
//	$env:AIRVIEW_GEN_SERVER_WIRING="1"; go test ./internal/server -run TestServerWiringDoc; Remove-Item Env:AIRVIEW_GEN_SERVER_WIRING
//
// TestServerWiringDoc fails when the table is stale.

const (
	wiringPath  = "WIRING.md"
	wiringBegin = "<!-- BEGIN GENERATED ROUTES (go test ./internal/server -run TestServerWiringDoc) -->"
	wiringEnd   = "<!-- END GENERATED ROUTES -->"
)

func wiringTable() string {
	var b bytes.Buffer
	b.WriteString("| Method | Path | Access | Station | Mode | Summary |\n|---|---|---|---|---|---|\n")
	for _, rt := range wiringRoutes() {
		st := "-"
		switch rt.Station {
		case StationPath:
			st = "{sid}"
		case StationQuery:
			st = "?station="
		}
		mode := "both"
		if rt.NetOnly {
			mode = "aggregator"
		}
		acc := routeAccess(&rt).String()
		if rt.Admin {
			acc += " (mutating)"
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s | %s | %s |\n", rt.Method, rt.Path, acc, st, mode,
			strings.ReplaceAll(firstSentence(rt.Summary), "|", "\\|"))
	}
	return b.String()
}

func TestServerWiringDoc(t *testing.T) {
	raw, err := os.ReadFile(wiringPath)
	if err != nil {
		t.Fatalf("WIRING.md: %v", err)
	}
	doc := strings.ReplaceAll(string(raw), "\r\n", "\n")
	i, j := strings.Index(doc, wiringBegin), strings.Index(doc, wiringEnd)
	if i < 0 || j < i {
		t.Fatal("WIRING.md: generated route markers missing")
	}
	want := doc[:i+len(wiringBegin)] + "\n" + wiringTable() + doc[j:]
	if os.Getenv("AIRVIEW_GEN_SERVER_WIRING") != "" {
		if err := os.WriteFile(wiringPath, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if want != doc {
		t.Error("WIRING.md route table is stale: regenerate it (see wiring_test.go)")
	}
}

// wiringRoutes are the network routes plus the legacy routes that take a
// station selection.
func wiringRoutes() []Route {
	out := append([]Route(nil), netRoutes...)
	for _, rt := range routes {
		if rt.Station != StationNone && rt.Group != grpPublic && rt.Group != grpMe && rt.Group != grpNetAdm && rt.Group != grpUplinks {
			out = append(out, rt)
		}
	}
	return out
}

// netRoutesPendingAPIDoc returns the network routes listed in WIRING.md:
// TestDocsCoverage accepts them until docs/API.md is regenerated.
func netRoutesPendingAPIDoc(t *testing.T) map[string]bool {
	raw, err := os.ReadFile(wiringPath)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, rt := range wiringRoutes() {
		if strings.Contains(string(raw), "| "+rt.Method+" | `"+rt.Path+"` |") {
			out[rt.Method+" "+rt.Path] = true
		}
	}
	return out
}
