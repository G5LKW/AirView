package server

import (
	"context"
	"net"
	"strings"
	"testing"
)

// SO-3: with StrictPort the server does not move to a fallback port.
func TestStrictPortNoFallback(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	r := newRig(t, rigOptions{})
	s := New(r.eng, Options{PortOverride: busy, StrictPort: true})
	s.log = quietLogger()
	if _, _, err := s.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot be bound") {
		_ = s.Shutdown(context.Background())
		t.Fatalf("strict start on a busy port: %v", err)
	}
}
