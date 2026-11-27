package server

import (
	"net"
	"testing"
	"time"

	"airview/internal/model"
)

// SEC-05: per-client and global connection caps in aggregator mode.
func TestConnLimiter(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.srv.auth.net.testMode = model.ModeAggregator
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := r.srv.limitListener(ln).(*connLimiter)
	defer l.Close()
	fake := func(addr string) net.Conn { return fakeConn{remote: addr} }
	_, perIP := l.limits()
	var held []net.Conn
	for i := 0; i < perIP; i++ {
		c, ok := l.admit(fake("198.51.100.7:1000"))
		if !ok {
			t.Fatalf("connection %d refused", i)
		}
		held = append(held, c)
	}
	if _, ok := l.admit(fake("198.51.100.7:2000")); ok {
		t.Fatal("per-IP cap not applied")
	}
	// The same /64 is one client.
	var held6 []net.Conn
	for i := 0; i < perIP; i++ {
		c, ok := l.admit(fake("[2001:db8:1:2::" + string(rune('a'+i%6)) + "]:1"))
		if !ok {
			t.Fatalf("v6 connection %d refused", i)
		}
		held6 = append(held6, c)
	}
	if _, ok := l.admit(fake("[2001:db8:1:2:ffff::9]:1")); ok {
		t.Fatal("per-/64 cap not applied")
	}
	// Loopback (a local reverse proxy) is exempt.
	for i := 0; i < perIP+5; i++ {
		c, ok := l.admit(fake("127.0.0.1:5"))
		if !ok {
			t.Fatal("loopback limited")
		}
		defer c.Close()
	}
	held[0].Close()
	held[0].Close() // double close counts once
	if _, ok := l.admit(fake("198.51.100.7:3000")); !ok {
		t.Fatal("released slot not reusable")
	}
	for _, c := range append(held[1:], held6...) {
		c.Close()
	}
	// Standalone: unlimited.
	r.srv.auth.net.testMode = model.ModeStandalone
	if tot, _ := l.limits(); tot != 0 {
		t.Fatalf("standalone limited: %d", tot)
	}
}

type fakeConn struct {
	net.Conn
	remote string
}

func (f fakeConn) RemoteAddr() net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", f.remote)
	return a
}
func (f fakeConn) Close() error                     { return nil }
func (f fakeConn) SetDeadline(time.Time) error      { return nil }
func (f fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (f fakeConn) SetWriteDeadline(time.Time) error { return nil }
