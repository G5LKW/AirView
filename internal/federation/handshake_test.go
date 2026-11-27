package federation

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"airview/internal/avf"
)

type hsResult struct {
	info PeerInfo
	err  error
}

// serveOnce accepts one connection on ln and runs the server handshake.
func serveOnce(t *testing.T, ln net.Listener, srv HandshakeConfig, cert *tls.Certificate, auth Authorize) <-chan hsResult {
	ch := make(chan hsResult, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			ch <- hsResult{err: err}
			return
		}
		tc, info, err := ServerHandshake(context.Background(), c, ServerTLSConfig(avf.StaticCertificate(cert)), srv, auth)
		if tc != nil {
			tc.Close()
		}
		ch <- hsResult{info, err}
	}()
	return ch
}

func testCert(t *testing.T) (*tls.Certificate, string) {
	t.Helper()
	certPEM, keyPEM, err := avf.GenerateSelfSigned([]string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := avf.TLSCertPin(&c)
	if err != nil {
		t.Fatal(err)
	}
	return &c, pin
}

func dialHS(t *testing.T, addr, pin string, cli HandshakeConfig, want string) (PeerInfo, error) {
	t.Helper()
	cfg, err := ClientTLSConfig("127.0.0.1", pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	tc, info, err := ClientHandshake(context.Background(), nc, cfg, cli, want)
	if tc != nil {
		tc.Close()
	}
	return info, err
}

func TestHandshakeMutual(t *testing.T) {
	srvID, cliID := newID(t), newID(t)
	cert, pin := testCert(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	srv := HandshakeConfig{Identity: srvID, Name: "server-net", Version: "1"}
	cli := HandshakeConfig{Identity: cliID, Name: "client-net", Version: "2"}

	for _, p := range []string{"", pin} {
		ch := serveOnce(t, ln, srv, cert, func(id string) (string, string) {
			if id != cliID.FeederID() {
				return DenyUnknownPeer, "?"
			}
			return "", ""
		})
		info, err := dialHS(t, ln.Addr().String(), p, cli, srvID.FeederID())
		if err != nil {
			t.Fatalf("pin %q: %v", p, err)
		}
		r := <-ch
		if r.err != nil {
			t.Fatal(r.err)
		}
		if info.NodeID != srvID.FeederID() || info.Name != "server-net" || r.info.NodeID != cliID.FeederID() || r.info.Name != "client-net" || r.info.Version != "2" {
			t.Fatalf("infos %+v %+v", info, r.info)
		}
	}
}

func TestHandshakeFailures(t *testing.T) {
	srvID, cliID, other := newID(t), newID(t), newID(t)
	cert, _ := testCert(t)
	_, otherPin := testCert(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	srv := HandshakeConfig{Identity: srvID, Timeout: 3 * time.Second}
	cli := HandshakeConfig{Identity: cliID, Timeout: 3 * time.Second}

	// Unknown client: denied with the code.
	ch := serveOnce(t, ln, srv, cert, func(string) (string, string) { return DenyUnknownPeer, "not configured" })
	_, err := dialHS(t, ln.Addr().String(), "", cli, srvID.FeederID())
	var de *DeniedError
	if !errors.As(err, &de) || de.Code != DenyUnknownPeer {
		t.Fatalf("want denial, got %v", err)
	}
	<-ch

	// Client expects a different node: fails on HELLO before signing.
	ch = serveOnce(t, ln, srv, cert, nil)
	if _, err := dialHS(t, ln.Addr().String(), "", cli, other.FeederID()); !errors.Is(err, ErrWrongPeer) {
		t.Fatalf("want wrong peer, got %v", err)
	}
	<-ch

	// Pin mismatch.
	ch = serveOnce(t, ln, srv, cert, nil)
	if _, err := dialHS(t, ln.Addr().String(), otherPin, cli, srvID.FeederID()); err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("want pin mismatch, got %v", err)
	}
	<-ch

	// WebPKI against a self-signed certificate fails.
	ch = serveOnce(t, ln, srv, cert, nil)
	if _, err := dialHS(t, ln.Addr().String(), PinWebPKI, cli, srvID.FeederID()); err == nil {
		t.Fatal("webpki accepted a self-signed certificate")
	}
	<-ch

	// A client without our ALPN is refused by TLS.
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _, _ = ServerHandshake(context.Background(), c, ServerTLSConfig(avf.StaticCertificate(cert)), srv, nil)
		}
	}()
	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"avf/1"}, MinVersion: tls.VersionTLS13})
	if err == nil {
		c.Close()
		t.Fatal("server accepted ALPN avf/1")
	}
}

// TestHandshakeRelay: a relay that terminates TLS on both sides (a man in
// the middle with its own certificate) cannot complete the handshake,
// because each signature is bound to its own TLS session's exporter.
func TestHandshakeRelay(t *testing.T) {
	srvID, cliID := newID(t), newID(t)
	cert, _ := testCert(t)
	mitmCert, _ := testCert(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	mitm, _ := net.Listen("tcp", "127.0.0.1:0")
	defer mitm.Close()
	ch := serveOnce(t, ln, HandshakeConfig{Identity: srvID, Timeout: 3 * time.Second}, cert, nil)
	go func() {
		c, err := mitm.Accept()
		if err != nil {
			return
		}
		down := tls.Server(c, ServerTLSConfig(avf.StaticCertificate(mitmCert)))
		ucfg, _ := ClientTLSConfig("127.0.0.1", "", nil)
		up, err := tls.Dial("tcp", ln.Addr().String(), ucfg)
		if err != nil {
			c.Close()
			return
		}
		go func() { _, _ = io.Copy(up, down); up.Close() }()
		_, _ = io.Copy(down, up)
		down.Close()
	}()
	_, err := dialHS(t, mitm.Addr().String(), "", HandshakeConfig{Identity: cliID, Timeout: 3 * time.Second}, srvID.FeederID())
	if err == nil {
		t.Fatal("handshake through a TLS-terminating relay succeeded")
	}
	r := <-ch
	if !errors.Is(r.err, ErrBadSignature) {
		t.Fatalf("server: want bad signature, got %v", r.err)
	}
}

func TestLoadMaterial(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Listen: ":0"}
	m, err := LoadMaterial(dir, &cfg, nil)
	if err != nil || !m.Created || m.Pin == "" || m.Certificate == nil {
		t.Fatalf("%+v %v", m, err)
	}
	m2, err := LoadMaterial(dir, &cfg, nil)
	if err != nil || m2.Created || m2.Identity.FeederID() != m.Identity.FeederID() || m2.Pin != m.Pin {
		t.Fatal("material not stable across loads")
	}
}
