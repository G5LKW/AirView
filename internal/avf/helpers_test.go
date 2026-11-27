package avf

import (
	"context"
	"crypto/tls"
	"net"
	"path/filepath"
	"testing"
	"time"
)

const testServerID = "k3x2abcdefghijkl" // 16 base32 characters (a-z, 2-7)

// testCert returns a self-signed certificate and its pin, created in a
// temporary directory.
func testCert(t testing.TB) (*tls.Certificate, string) {
	t.Helper()
	c, pin, err := LoadOrCreateSelfSigned(filepath.Join(t.TempDir(), "avf-tls"), []string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return c, pin
}

func testIdentity(t testing.TB) Identity {
	t.Helper()
	id, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testBody() HelloBody {
	return HelloBody{Software: SoftwareFeeder, Version: "1.0.0-test", SeqEpoch: "0123456789abcdef"}
}

// serverResult is what a test server goroutine reports.
type serverResult struct {
	p    *PendingSession
	conn *Conn
	err  error
}

// startServer listens on loopback and runs ServerHandshake for each
// accepted connection; decide maps the pending session to a WELCOME (nil =
// approved).
func startServer(t testing.TB, cert *tls.Certificate, cfg ServerConfig, decide func(*PendingSession) *Welcome) (addr string, results <-chan serverResult) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	if cfg.ServerID == "" {
		cfg.ServerID = testServerID
	}
	ch := make(chan serverResult, 16)
	tlsCfg := ServerTLSConfig(StaticCertificate(cert))
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				p, err := ServerHandshake(context.Background(), nc, tlsCfg, cfg)
				if err != nil {
					ch <- serverResult{err: err}
					return
				}
				w := &Welcome{Status: StatusApproved, SessionID: "s1", ResumeToken: "rt", AckSeq: 0}
				if decide != nil {
					w = decide(p)
				}
				if w.Status == StatusDenied {
					err := p.Deny(w.Code, w.Reason, w.ReconnectIn)
					ch <- serverResult{p: p, err: err}
					return
				}
				c, err := p.Accept(*w)
				ch <- serverResult{p: p, conn: c, err: err}
			}()
		}
	}()
	return ln.Addr().String(), ch
}

func waitResult(t testing.TB, ch <-chan serverResult) serverResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("server result timeout")
	}
	return serverResult{}
}

func dialTest(t testing.TB, addr, pin string, id Identity, body HelloBody) (*Conn, *ClientResult, error) {
	t.Helper()
	tlsCfg, err := ClientTLSConfig("127.0.0.1", pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Dial(ctx, addr, tlsCfg, ClientConfig{Identity: id, Body: body})
}
