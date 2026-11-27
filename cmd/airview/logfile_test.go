package main

import (
	"bytes"
	"strings"
	"testing"

	"airview/internal/model"
)

// TestJournalLevel: under systemd (JOURNAL_STREAM) the stderr copy carries
// warnings and errors only unless process.journalLevel says otherwise; the
// log file keeps process.logLevel.
func TestJournalLevel(t *testing.T) {
	t.Setenv("AIRVIEW_DEBUG", "")
	cases := []struct {
		journal  string
		p        model.ProcessConfig
		wantErr  string // what stderr gets of an INFO and a WARN line
		wantFile string
	}{
		{"", model.ProcessConfig{}, "IW", "IW"},
		{"8:1234", model.ProcessConfig{}, "W", "IW"},
		{"8:1234", model.ProcessConfig{JournalLevel: "info"}, "IW", "IW"},
		{"", model.ProcessConfig{LogLevel: "warn"}, "W", "W"},
		{"8:1234", model.ProcessConfig{LogLevel: "warn", JournalLevel: "error"}, "", "W"},
	}
	for _, c := range cases {
		t.Setenv("JOURNAL_STREAM", c.journal)
		var se, lf bytes.Buffer
		log := newLogger(&se, &lf)
		setLogLevels(c.p)
		log.Info("I-line")
		log.Warn("W-line")
		got := func(b *bytes.Buffer) string {
			s := ""
			if strings.Contains(b.String(), "I-line") {
				s += "I"
			}
			if strings.Contains(b.String(), "W-line") {
				s += "W"
			}
			return s
		}
		if g := got(&se); g != c.wantErr {
			t.Errorf("JOURNAL_STREAM=%q %+v: stderr got %q, want %q", c.journal, c.p, g, c.wantErr)
		}
		if g := got(&lf); g != c.wantFile {
			t.Errorf("JOURNAL_STREAM=%q %+v: file got %q, want %q", c.journal, c.p, g, c.wantFile)
		}
	}
}
