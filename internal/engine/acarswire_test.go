package engine

import (
	"net"
	"testing"
	"time"

	"airview/internal/acars"
	"airview/internal/model"
)

// TestACARSWiring: off by default; on, a datagram on the UDP input is
// stored; a change of the listen address keeps the stored messages; off
// again stops it.
func TestACARSWiring(t *testing.T) {
	dir := t.TempDir()
	cfg := model.DefaultConfig()
	cfg.NTP.Enabled = false
	cfg.Feeds = nil
	e := startEngine(t, dir, cfg, Options{})
	if s := e.ACARSStatus(); s.Enabled || s.Stats != nil {
		t.Fatalf("on by default: %+v", s)
	}
	if _, ok := e.ACARSMessages(acars.Filter{}); ok {
		t.Fatal("messages while off")
	}

	cfg.ACARS = model.ACARSConfig{Enabled: true, UDPListen: "127.0.0.1:0"}
	if _, _, err := e.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	addr := waitACARSUDP(t, e)
	sendUDP(t, addr, `{"timestamp":0,"label":"Q0","tail":".G-TEST","text":"hello"}`)
	waitACARSTotal(t, e, 1)

	cfg.ACARS.TCPListen = "127.0.0.1:0"
	if _, _, err := e.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	msgs, ok := e.ACARSMessages(acars.Filter{Reg: "GTEST"})
	if !ok || len(msgs) != 1 || msgs[0].Text != "hello" {
		t.Fatalf("messages lost on a listener change: %v %+v", ok, msgs)
	}
	if a := waitACARSUDP(t, e); a == addr {
		t.Logf("same UDP port after the change: %s", a)
	}

	cfg.ACARS.Enabled = false
	if _, _, err := e.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if s := e.ACARSStatus(); s.Enabled || s.Stats != nil {
		t.Fatalf("still on: %+v", s)
	}
}

func waitACARSUDP(t *testing.T, e *Engine) string {
	t.Helper()
	for i := 0; i < 300; i++ {
		if s := e.ACARSStatus(); s.Input != nil && s.Input.UDP != "" {
			return s.Input.UDP
		} else if s.Input != nil && s.Input.UDPError != "" {
			t.Fatal(s.Input.UDPError)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("UDP input not listening")
	return ""
}

func waitACARSTotal(t *testing.T, e *Engine, n uint64) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if s := e.ACARSStatus(); s.Stats != nil && s.Stats.Total >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no ACARS message stored: %+v", e.ACARSStatus())
}

func sendUDP(t *testing.T, addr, line string) {
	t.Helper()
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
}
