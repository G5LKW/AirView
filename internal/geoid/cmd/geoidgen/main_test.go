package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/geoid"
)

func TestRun(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "grid.grd")
	var sb strings.Builder
	fmt.Fprintf(&sb, "-90 90 0 360 0.5 0.5\n")
	for i := 0; i <= 360; i++ {
		for j := 0; j <= 720; j++ {
			lat := 90 - float64(i)*0.5
			fmt.Fprintf(&sb, "%.3f ", 10*math.Sin(lat*math.Pi/180)+float64(j%720)*0.01)
		}
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(in, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.avg")
	if err := run(in, out, 1, false); err != nil {
		t.Fatal(err)
	}
	g, err := geoid.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if g.NLat != 181 || g.NLon != 360 {
		t.Fatalf("output %d×%d", g.NLat, g.NLon)
	}
	if v := g.Undulation(30, 2); math.Abs(v-(10*math.Sin(30*math.Pi/180)+0.04)) > 0.006 {
		t.Errorf("node value %v", v)
	}
	// The synthetic grid cannot match EGM96: verification must fail.
	if err := run(in, out, 1, true); err == nil {
		t.Error("verification passed on synthetic data")
	}
	if err := run(in, out, 0.3, false); err == nil {
		t.Error("non-multiple resolution accepted")
	}
}
