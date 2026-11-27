package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/model"
)

func TestParseMemTotal(t *testing.T) {
	if got := parseMemTotal("MemFree: 1 kB\nMemTotal:       16314172 kB\n"); got != 16314172<<10 {
		t.Fatalf("got %d", got)
	}
	if parseMemTotal("nothing") != 0 || physSoftLimit(512<<20) != 0 || physSoftLimit(10<<30) != (10<<30)/100*80 {
		t.Fatal("limits")
	}
}

func TestLogRotationKeepsFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "airview.log")
	r := openLogFile(p)
	defer r.Close()
	r.setLimits(100, 3)
	line := []byte(strings.Repeat("x", 60) + "\n")
	for range 10 {
		_, _ = r.Write(line)
	}
	for _, s := range []string{"", ".1", ".2", ".3"} {
		if _, err := os.Stat(p + s); err != nil {
			t.Fatalf("missing %s: %v", p+s, err)
		}
	}
	if _, err := os.Stat(p + ".4"); err == nil {
		t.Fatal("more files than process.logFiles")
	}
}

func TestProcessConfigNormalize(t *testing.T) {
	p := model.ProcessConfig{LogLevel: "LOUD", LogMaxMB: 99999, LogFiles: 5, MemoryLimitMB: -1}
	if n := p.Normalize(); len(n) != 2 || p.LogLevel != "" || p.LogMaxMB != 0 || p.LogFiles != 5 || p.MemoryLimitMB != -1 {
		t.Fatalf("%+v %v", p, n)
	}
	if b, f := (model.ProcessConfig{}).LogLimits(true); b != 64<<20 || f != 10 {
		t.Fatalf("aggregator defaults %d %d", b, f)
	}
}
