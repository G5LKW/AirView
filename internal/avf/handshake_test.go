package avf

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

func TestPinForms(t *testing.T) {
	_, pin := testCert(t)
	if !strings.HasPrefix(pin, PinPrefix) || len(pin) != len(PinPrefix)+44 || !model.ValidServerPin(pin) {
		t.Fatalf("pin %q", pin)
	}
	h, err := ParsePin(pin)
	if err != nil {
		t.Fatal(err)
	}
	raw := base64.RawStdEncoding.EncodeToString(h[:])
	url := base64.URLEncoding.EncodeToString(h[:])
	rawURL := base64.RawURLEncoding.EncodeToString(h[:])
	for _, s := range []string{PinPrefix + raw, PinPrefix + url, PinPrefix + rawURL, " " + pin + " "} {
		c, err := CanonicalPin(s)
		if err != nil || c != pin {
			t.Errorf("%q -> %q %v", s, c, err)
		}
	}
	for _, s := range []string{"", "md5/abc", PinPrefix + "AAAA", PinPrefix + "!!"} {
		if _, err := ParsePin(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestSelfSignedPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "avf-tls")
	c1, pin1, err := LoadOrCreateSelfSigned(dir, []string{"agg.example", "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c1.Leaf.DNSNames) != 1 || len(c1.Leaf.IPAddresses) != 1 {
		t.Fatal("SANs", c1.Leaf.DNSNames, c1.Leaf.IPAddresses)
	}
	if p, _ := TLSCertPin(c1); p != pin1 {
		t.Fatal("TLSCertPin")
	}
	_, pin2, err := LoadOrCreateSelfSigned(dir, nil)
	if err != nil || pin2 != pin1 {
		t.Fatal("reload changed the pin", err)
	}
	// Losing the certificate re-issues it for the same key: same pin.
	os.Remove(filepath.Join(dir, SelfSignedCertFile))
	c3, pin3, err := LoadOrCreateSelfSigned(dir, nil)
	if err != nil || pin3 != pin1 || c3.Leaf.SerialNumber.Cmp(c1.Leaf.SerialNumber) == 0 {
		t.Fatal("re-issue", err)
	}
	if c3.Leaf.PublicKeyAlgorithm.String() != "ECDSA" {
		t.Fatal(c3.Leaf.PublicKeyAlgorithm)
	}
}

func TestCertReloader(t *testing.T) {
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	write := func() {
		c, k, err := GenerateSelfSigned([]string{"x"})
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(cp, c, 0o644)
		os.WriteFile(kp, k, 0o600)
	}
	write()
	r, err := NewCertReloader(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r.GetCertificate(nil)
	pa, _ := TLSCertPin(a)
	write()
	future := time.Now().Add(time.Hour)
	os.Chtimes(cp, future, future)
	os.Chtimes(kp, future, future)
	r.mu.Lock()
	r.lastCheck = time.Time{}
	r.mu.Unlock()
	b, _ := r.GetCertificate(nil)
	pb, _ := TLSCertPin(b)
	if pa == pb {
		t.Fatal("certificate not reloaded")
	}
	// A broken file keeps the old certificate.
	os.WriteFile(cp, []byte("garbage"), 0o644)
	later := future.Add(time.Hour)
	os.Chtimes(cp, later, later)
	r.mu.Lock()
	r.lastCheck = time.Time{}
	r.mu.Unlock()
	c, _ := r.GetCertificate(nil)
	if _, err := r.Current(); c != b || err == nil {
		t.Fatal("broken reload must keep the old certificate and report an error")
	}
}

func TestHandshakeApproved(t *testing.T) {
	cert, pin := testCert(t)
	addr, results := startServer(t, cert, ServerConfig{Name: "Club", Admission: model.AdmissionOpen,
		Features: []string{FeatFlate, FeatResults, FeatClaim}}, func(p *PendingSession) *Welcome {
		return &Welcome{Status: StatusApproved, SessionID: "abc", ResumeToken: "tok", AckSeq: 41,
			Config: WelcomeConfig{BatchMs: 20, Mode: ModeFull, MLAT: true, ReturnResults: true}, MOTD: "hi"}
	})
	id := testIdentity(t)
	body := testBody()
	body.UUID = "{12345678-1234-1234-1234-123456789ABC}"
	body.Features = []string{FeatFlate, FeatResults, FeatGNSS, "future-thing"}
	c, res, err := dialTest(t, addr, pin, id, body)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sr := waitResult(t, results)
	if sr.err != nil {
		t.Fatal(sr.err)
	}
	defer sr.conn.Close()
	p := sr.p
	if p.FeederID != id.FeederID() || p.Body.Software != SoftwareFeeder || p.Body.UUID != "12345678123412341234123456789abc" {
		t.Fatalf("%+v", p.Body)
	}
	if strings.Join(p.Features, ",") != "flate,results" || strings.Join(res.Features, ",") != "flate,results" {
		t.Fatal(p.Features, res.Features)
	}
	if res.HelloS.Name != "Club" || res.HelloS.ServerID != testServerID || len(res.HelloS.Nonce) != 32 {
		t.Fatalf("%+v", res.HelloS)
	}
	if res.Welcome.AckSeq != 41 || res.Welcome.FeederID != id.FeederID() || res.Welcome.Config.BatchMs != 20 {
		t.Fatalf("%+v", res.Welcome)
	}
	if c.w.Batch() != 20*time.Millisecond || !c.HasFeature(FeatResults) {
		t.Fatal("client batch/features")
	}
	// Records flow both ways.
	if err := c.WriteSeqJSON(TReceiver, 42, &Receiver{RxNo: 1, ClockClass: "hw12", NominalHz: 12e6, Channels: 1}); err != nil {
		t.Fatal(err)
	}
	c.Flush()
	rec, err := sr.conn.ReadRecord()
	if err != nil || rec.Type != TReceiver {
		t.Fatal(rec.Type, err)
	}
	var r Receiver
	if seq, err := DecodeSeqJSON(rec.Payload, &r); err != nil || seq != 42 || r.Class() != model.ClockHW12 {
		t.Fatal(seq, err)
	}
	if err := sr.conn.WriteAck(42); err != nil {
		t.Fatal(err)
	}
	if rec, err := c.ReadRecord(); err != nil || rec.Type != TAck {
		t.Fatal(rec.Type, err)
	}
}

func TestHandshakePendingAndDenied(t *testing.T) {
	cert, pin := testCert(t)
	status := StatusPending
	addr, results := startServer(t, cert, ServerConfig{}, func(p *PendingSession) *Welcome {
		if status == StatusDenied {
			return &Welcome{Status: StatusDenied, Code: CodeEnrolRequired, Reason: "token needed", ReconnectIn: 900}
		}
		return &Welcome{Status: StatusPending, SessionID: "x"}
	})
	c, res, err := dialTest(t, addr, pin, testIdentity(t), testBody())
	if err != nil || res.Welcome.Status != StatusPending {
		t.Fatal(err)
	}
	c.Close()
	waitResult(t, results)
	status = StatusDenied
	_, res, err = dialTest(t, addr, pin, testIdentity(t), testBody())
	var de *DeniedError
	if !errors.As(err, &de) || de.Welcome.Code != CodeEnrolRequired || de.RetryAfter() != 900*time.Second {
		t.Fatalf("%v", err)
	}
	if res == nil || res.Welcome.Reason != "token needed" || ActionFor(de.Welcome.Code) != ActionStop {
		t.Fatal("result")
	}
	if (&DeniedError{}).RetryAfter() != DeniedMinBackoff {
		t.Fatal("min back-off")
	}
	waitResult(t, results)
}

func TestHandshakePinMismatch(t *testing.T) {
	cert, _ := testCert(t)
	_, otherPin := testCert(t)
	addr, results := startServer(t, cert, ServerConfig{}, nil)
	_, _, err := dialTest(t, addr, otherPin, testIdentity(t), testBody())
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("want pin mismatch, got %v", err)
	}
	if r := waitResult(t, results); r.err == nil {
		t.Fatal("server must fail too")
	}
}

func TestHandshakeRequiresALPNAndTLS13(t *testing.T) {
	cert, _ := testCert(t)
	addr, results := startServer(t, cert, ServerConfig{}, nil)
	for name, cfg := range map[string]*tls.Config{
		"no ALPN":    {InsecureSkipVerify: true, MinVersion: tls.VersionTLS13},
		"other ALPN": {InsecureSkipVerify: true, NextProtos: []string{"h2"}},
		"TLS 1.2":    {InsecureSkipVerify: true, NextProtos: []string{ALPN}, MaxVersion: tls.VersionTLS12},
	} {
		c, err := tls.Dial("tcp", addr, cfg)
		if err == nil {
			// The client may finish before the server's alert arrives; the
			// first read must fail.
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err = c.Read(make([]byte, 1))
			c.Close()
		}
		if err == nil {
			t.Errorf("%s: connection accepted", name)
		}
		if r := waitResult(t, results); r.err == nil {
			t.Errorf("%s: server accepted", name)
		}
	}
	// A client config refuses a server without ALPN.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		s := tls.Server(nc, &tls.Config{Certificates: []tls.Certificate{*cert}})
		s.Handshake()
		io.Copy(io.Discard, s)
	}()
	tc, _ := ClientTLSConfig("127.0.0.1", "", nil)
	tc.InsecureSkipVerify = true
	c, err := tls.Dial("tcp", ln.Addr().String(), tc)
	if err == nil {
		c.Close()
		t.Fatal("client accepted a server without ALPN")
	}
}

// relay is a TLS-terminating man in the middle: it presents its own
// certificate to the client, opens its own TLS session to the server and
// copies the plaintext both ways.
func startRelay(t *testing.T, serverAddr, serverPin string) (addr, pin string) {
	t.Helper()
	cert, pin := testCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				down := tls.Server(nc, ServerTLSConfig(StaticCertificate(cert)))
				if err := down.Handshake(); err != nil {
					nc.Close()
					return
				}
				cfg, _ := ClientTLSConfig("127.0.0.1", serverPin, nil)
				up, err := tls.Dial("tcp", serverAddr, cfg)
				if err != nil {
					down.Close()
					return
				}
				go func() { io.Copy(up, down); up.Close() }()
				io.Copy(down, up)
				down.Close()
			}()
		}
	}()
	return ln.Addr().String(), pin
}

// Acceptance: a MITM relay that terminates TLS fails the signature.
func TestMITMRelayFailsSignature(t *testing.T) {
	cert, pin := testCert(t)
	addr, results := startServer(t, cert, ServerConfig{}, nil)
	relayAddr, relayPin := startRelay(t, addr, pin)

	// The client trusts the relay's key (worst case: a mis-issued WebPKI
	// certificate or a swapped pin); the relay forwards every byte.
	_, res, err := dialTest(t, relayAddr, relayPin, testIdentity(t), testBody())
	var de *DeniedError
	if !errors.As(err, &de) || de.Welcome.Code != CodeBadSignature {
		t.Fatalf("client: want denied bad_signature, got %v", err)
	}
	if res.HelloS.ServerID != testServerID {
		t.Fatal("relay did not forward HELLO_S")
	}
	r := waitResult(t, results)
	var he *HandshakeError
	if !errors.As(r.err, &he) || he.Code != CodeBadSignature || !errors.Is(r.err, ErrBadSignature) {
		t.Fatalf("server: want bad_signature, got %v", r.err)
	}

	// Control: the same client and server succeed without the relay.
	c, _, err := dialTest(t, addr, pin, testIdentity(t), testBody())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if r := waitResult(t, results); r.err != nil {
		t.Fatal(r.err)
	}
}

// A HELLO_C captured on one connection fails on another (fresh nonce and
// exporter).
func TestHelloReplayFails(t *testing.T) {
	cert, pin := testCert(t)
	addr, results := startServer(t, cert, ServerConfig{}, nil)
	id := testIdentity(t)
	cfg, _ := ClientTLSConfig("127.0.0.1", pin, nil)

	c1, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var hs1 HelloS
	if err := ReadMessage(c1, &hs1); err != nil {
		t.Fatal(err)
	}
	cs1 := c1.ConnectionState()
	hc, err := SignHello(id.Priv, &cs1, &hs1, testBody())
	if err != nil {
		t.Fatal(err)
	}
	c1.Close()
	waitResult(t, results)

	c2, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	var hs2 HelloS
	ReadMessage(c2, &hs2)
	if err := WriteMessage(c2, hc); err != nil {
		t.Fatal(err)
	}
	var w Welcome
	if err := ReadMessage(c2, &w); err != nil || w.Status != StatusDenied || w.Code != CodeBadSignature {
		t.Fatalf("%+v %v", w, err)
	}
	waitResult(t, results)
}

func TestHandshakeBadHelloAndFlate(t *testing.T) {
	cert, pin := testCert(t)
	addr, results := startServer(t, cert, ServerConfig{}, nil)
	id := testIdentity(t)
	// Missing flate -> proto_version.
	b := testBody()
	b.Features = []string{FeatResults}
	_, _, err := dialTest(t, addr, pin, id, b)
	var de *DeniedError
	if !errors.As(err, &de) || de.Welcome.Code != CodeProtoVersion {
		t.Fatalf("%v", err)
	}
	waitResult(t, results)
	// Bad seqEpoch -> bad_hello.
	b = testBody()
	b.SeqEpoch = "XYZ"
	_, _, err = dialTest(t, addr, pin, id, b)
	if !errors.As(err, &de) || de.Welcome.Code != CodeBadHello {
		t.Fatalf("%v", err)
	}
	waitResult(t, results)
	// Garbage HELLO_C -> bad_hello.
	cfg, _ := ClientTLSConfig("127.0.0.1", pin, nil)
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var hs HelloS
	ReadMessage(c, &hs)
	c.Write([]byte{0, 0, 0, 3, 'x', 'y', 'z'})
	var w Welcome
	if err := ReadMessage(c, &w); err != nil || w.Code != CodeBadHello {
		t.Fatalf("%+v %v", w, err)
	}
	c.Close()
	waitResult(t, results)
	// Oversized message length: closed without a reply.
	c, _ = tls.Dial("tcp", addr, cfg)
	ReadMessage(c, &hs)
	c.Write([]byte{0, 1, 0, 1})
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := ReadMessage(c, &w); err == nil {
		// bad_hello reply is acceptable as well; what matters is the close.
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("connection kept open")
		}
	}
	c.Close()
	waitResult(t, results)
}

func TestHandshakeTimeoutAndServerIDCheck(t *testing.T) {
	cert, pin := testCert(t)
	addr, results := startServer(t, cert, ServerConfig{HandshakeTimeout: 300 * time.Millisecond}, nil)
	cfg, _ := ClientTLSConfig("127.0.0.1", pin, nil)
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var hs HelloS
	ReadMessage(c, &hs)
	start := time.Now()
	r := waitResult(t, results) // never sends HELLO_C
	if r.err == nil || time.Since(start) > 5*time.Second {
		t.Fatal("handshake timeout not enforced", r.err)
	}
	// A client that expects another server ID refuses.
	tc, _ := ClientTLSConfig("127.0.0.1", pin, nil)
	_, _, err = Dial(context.Background(), addr, tc, ClientConfig{Identity: testIdentity(t), Body: testBody(), ServerID: "aaaaaaaaaaaaaaaa"})
	if err == nil || !strings.Contains(err.Error(), "server ID changed") {
		t.Fatal(err)
	}
	waitResult(t, results)
}

func TestSignatureMessageLayout(t *testing.T) {
	m := SignatureMessage(testServerID, make([]byte, 32), make([]byte, 32), []byte("{}"))
	if len(m) != 116 || string(m[:4]) != "AVF1" || string(m[4:20]) != testServerID {
		t.Fatalf("len %d", len(m))
	}
}

func TestIdentityFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", IdentityFile)
	id1, created, err := LoadOrCreateIdentity(p)
	if err != nil || !created {
		t.Fatal(err)
	}
	id2, created, err := LoadOrCreateIdentity(p)
	if err != nil || created || id2.FeederID() != id1.FeederID() || !model.ValidID(id1.FeederID()) {
		t.Fatal(err)
	}
	id3, err := ReplaceIdentity(p)
	if err != nil || id3.FeederID() == id1.FeederID() {
		t.Fatal("new-identity")
	}
	if _, err := ParseIdentity([]byte("nope")); err == nil {
		t.Fatal("garbage parsed")
	}
}
