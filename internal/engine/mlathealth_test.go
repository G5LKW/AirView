package engine

import (
	"strings"
	"testing"
	"time"
)

// Release soak: healthz showed "degraded: TDOA verification muted" for
// about 3 minutes of every start (the breaker muting while the clocks of a
// fresh network converge). Muting in the start-up grace after the first
// verification is reported as ok; later muting and panics still degrade.
func TestMLATHealthStartupGrace(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var h mlatHealthState
	if c := h.update(t0, 0, 0, 0); c.Degraded || c.State != "ok" {
		t.Fatalf("idle: %+v", c)
	}
	// verification starts, the breaker mutes 3 minutes later (start-up sync)
	h.update(t0.Add(time.Minute), 0, 0, 10)
	c := h.update(t0.Add(4*time.Minute), 50, 0, 500)
	if c.Degraded || !strings.Contains(c.State, "start-up sync") {
		t.Fatalf("start-up muting must not degrade: %+v", c)
	}
	if c := h.update(t0.Add(7*time.Minute), 50, 0, 900); c.Degraded || c.State != "ok" {
		t.Fatalf("after the hold: %+v", c)
	}
	// muting after the grace is a real problem
	c = h.update(t0.Add(30*time.Minute), 80, 0, 5000)
	if !c.Degraded || !strings.Contains(c.State, "TDOA verification muted") {
		t.Fatalf("muting after the grace must degrade: %+v", c)
	}
	if c := h.update(t0.Add(33*time.Minute), 80, 0, 6000); c.Degraded {
		t.Fatalf("hold over: %+v", c)
	}
	// a recovered panic degrades even during the start-up grace
	var p mlatHealthState
	p.update(t0, 0, 0, 1)
	if c := p.update(t0.Add(time.Minute), 5, 1, 100); !c.Degraded || !strings.Contains(c.State, "recovered panic") || strings.Contains(c.State, "muted") {
		t.Fatalf("panic: %+v", c)
	}
}
