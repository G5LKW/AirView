package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
)

// AVFED/1 handshake (PROTOCOL.md §3). It reuses the AVF/1 building blocks
// (internal/avf): TLS 1.3 only, the self-signed / reloadable certificate
// helpers, SPKI pins, Ed25519 identities and the length-prefixed JSON
// handshake messages, with its own ALPN, exporter label and signature
// domain so an AVF feeder session and a federation session can never be
// confused. Unlike AVF/1, authentication is mutual: both nodes sign the
// other's nonce together with the TLS exporter value, so a relay that
// terminates TLS (a man in the middle) fails on both sides even when no
// certificate is checked.

// Protocol identifiers.
const (
	ALPN          = "avfed/1"
	Revision      = 1
	ExporterLabel = "EXPORTER-AirView-AVFED1"
	exporterLen   = 32
	nonceLen      = 32
	sigDomainC    = "AVFED1/C" // client signature domain
	sigDomainS    = "AVFED1/S" // server signature domain
	// Software is the software name in Hello / Auth.
	Software = "airview"
	// PinWebPKI in PeerConfig.Pin requires a certificate valid for the host.
	PinWebPKI = "webpki"
)

// Handshake errors.
var (
	ErrNoALPN       = errors.New("federation: peer did not negotiate ALPN " + ALPN)
	ErrBadSignature = errors.New("federation: peer signature invalid")
	ErrWrongPeer    = errors.New("federation: peer proved a different node ID")
)

// Hello is the server's first message.
type Hello struct {
	Proto    int    `json:"proto"`
	NodeID   string `json:"nodeId"`
	Name     string `json:"name,omitempty"`
	Nonce    []byte `json:"nonce"`
	TimeMs   int64  `json:"timeMs"`
	Software string `json:"software"`
}

// AuthBody is the signed body of both Auth messages.
type AuthBody struct {
	Pub      []byte `json:"pub"`
	Name     string `json:"name,omitempty"`
	Software string `json:"software"`
	Version  string `json:"version,omitempty"`
	// Nonce is the client's nonce (client body only); the server signs it.
	Nonce []byte `json:"nonce,omitempty"`
	// Peer is the node ID the signer expects on the other side.
	Peer string `json:"peer"`
}

// Auth is a signed body.
type Auth struct {
	Body []byte `json:"body"`
	Sig  []byte `json:"sig"`
}

// Verdict is the server's answer to the client's Auth.
type Verdict struct {
	Status string `json:"status"` // "ok" | "denied"
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	Auth   *Auth  `json:"auth,omitempty"` // the server's signed body when ok
}

// Deny codes.
const (
	DenyUnknownPeer = "unknown_peer"
	DenyDisabled    = "disabled"
	DenyBadAuth     = "bad_auth"
	DenyProto       = "proto_version"
)

// DeniedError is returned to a client the server denied.
type DeniedError struct{ Code, Reason string }

func (e *DeniedError) Error() string {
	return fmt.Sprintf("federation: denied by peer: %s (%s)", e.Code, e.Reason)
}

// PeerInfo is what one side learned about the other.
type PeerInfo struct {
	NodeID   string
	Name     string
	Software string
	Version  string
}

// ServerTLSConfig returns the listener's TLS configuration (TLS 1.3 only,
// ALPN avfed/1 mandatory).
func ServerTLSConfig(getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	cfg := avf.ServerTLSConfig(getCert)
	cfg.NextProtos = []string{ALPN}
	cfg.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		if !slices.Contains(chi.SupportedProtos, ALPN) {
			return nil, ErrNoALPN
		}
		return nil, nil
	}
	return cfg
}

// ClientTLSConfig returns the dialer's TLS configuration for a peer.
// pin "" accepts any certificate (the mutual Ed25519 check authenticates
// the peer and is bound to this TLS session), PinWebPKI verifies the chain
// against roots (nil = system roots) and host, "sha256/<b64>" pins the
// leaf key.
func ClientTLSConfig(host, pin string, roots *x509.CertPool) (*tls.Config, error) {
	var cfg *tls.Config
	switch pin {
	case "":
		cfg = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, InsecureSkipVerify: true} //nolint:gosec // identity is verified by the AVFED handshake
	case PinWebPKI:
		c, err := avf.ClientTLSConfig(host, "", roots)
		if err != nil {
			return nil, err
		}
		cfg = c
	default:
		c, err := avf.ClientTLSConfig(host, pin, nil)
		if err != nil {
			return nil, err
		}
		cfg = c
	}
	cfg.NextProtos = []string{ALPN}
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if cs.NegotiatedProtocol != ALPN {
			return ErrNoALPN
		}
		return nil
	}
	return cfg, nil
}

func exportKM(cs *tls.ConnectionState) ([]byte, error) {
	if cs.Version != tls.VersionTLS13 || !cs.HandshakeComplete {
		return nil, errors.New("federation: TLS 1.3 handshake not complete")
	}
	return cs.ExportKeyingMaterial(ExporterLabel, nil, exporterLen)
}

// sigMessage is domain ‖ peerNodeID ‖ peerNonce ‖ ekm ‖ SHA-256(body).
func sigMessage(domain, peerID string, nonce, ekm, body []byte) []byte {
	sum := sha256.Sum256(body)
	m := make([]byte, 0, len(domain)+len(peerID)+len(nonce)+len(ekm)+len(sum))
	m = append(m, domain...)
	m = append(m, peerID...)
	m = append(m, nonce...)
	m = append(m, ekm...)
	return append(m, sum[:]...)
}

func sign(id avf.Identity, domain, peerID string, nonce, ekm []byte, body AuthBody) (*Auth, error) {
	body.Pub = id.Public()
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &Auth{Body: raw, Sig: ed25519.Sign(id.Priv, sigMessage(domain, peerID, nonce, ekm, raw))}, nil
}

// verify checks a over (domain, ownID, ownNonce, ekm) and returns the body
// and the signer's node ID.
func verify(a *Auth, domain, ownID string, nonce, ekm []byte) (*AuthBody, string, error) {
	if a == nil {
		return nil, "", perr("missing auth")
	}
	var body AuthBody
	if err := json.Unmarshal(a.Body, &body); err != nil {
		return nil, "", perr("auth body: %v", err)
	}
	if len(body.Pub) != ed25519.PublicKeySize || len(a.Sig) != ed25519.SignatureSize {
		return nil, "", perr("auth pub/sig length")
	}
	if !ed25519.Verify(ed25519.PublicKey(body.Pub), sigMessage(domain, ownID, nonce, ekm, a.Body), a.Sig) {
		return nil, "", ErrBadSignature
	}
	if len(body.Name) > 128 || len(body.Software) > 64 || len(body.Version) > 64 {
		return nil, "", perr("auth field too long")
	}
	body.Name, body.Software, body.Version = cleanStr([]byte(body.Name)), cleanStr([]byte(body.Software)), cleanStr([]byte(body.Version))
	return &body, model.FeederIDFromPub(body.Pub), nil
}

// HandshakeConfig is one side's handshake input.
type HandshakeConfig struct {
	Identity avf.Identity
	Name     string
	Version  string
	Timeout  time.Duration // default 10 s
	Now      func() time.Time
}

func (h *HandshakeConfig) defaults() {
	if h.Timeout <= 0 {
		h.Timeout = 10 * time.Second
	}
	if h.Now == nil {
		h.Now = time.Now
	}
}

// Authorize decides about an authenticated client: "" admits it, else a
// deny code with a reason.
type Authorize func(peerID string) (code, reason string)

// ServerHandshake runs TLS and the AVFED handshake on an accepted
// connection. On success the deadline is cleared and the TLS connection is
// returned with what the client proved.
func ServerHandshake(ctx context.Context, nc net.Conn, tlsCfg *tls.Config, hc HandshakeConfig, auth Authorize) (*tls.Conn, PeerInfo, error) {
	hc.defaults()
	deadline := hc.Now().Add(hc.Timeout)
	_ = nc.SetDeadline(deadline)
	tc := tls.Server(nc, tlsCfg)
	hctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	fail := func(err error) (*tls.Conn, PeerInfo, error) {
		tc.Close()
		return nil, PeerInfo{}, err
	}
	if err := tc.HandshakeContext(hctx); err != nil {
		return fail(err)
	}
	cs := tc.ConnectionState()
	if cs.NegotiatedProtocol != ALPN {
		return fail(ErrNoALPN)
	}
	ekm, err := exportKM(&cs)
	if err != nil {
		return fail(err)
	}
	own := hc.Identity.FeederID()
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return fail(err)
	}
	if err := avf.WriteMessage(tc, &Hello{Proto: Revision, NodeID: own, Name: hc.Name, Nonce: nonce,
		TimeMs: hc.Now().UnixMilli(), Software: Software}); err != nil {
		return fail(err)
	}
	var ca Auth
	if err := avf.ReadMessage(tc, &ca); err != nil {
		return fail(err)
	}
	deny := func(code, reason string, err error) (*tls.Conn, PeerInfo, error) {
		_ = avf.WriteMessage(tc, &Verdict{Status: "denied", Code: code, Reason: reason})
		return fail(err)
	}
	body, peerID, err := verify(&ca, sigDomainC, own, nonce, ekm)
	if err != nil {
		return deny(DenyBadAuth, "authentication failed", err)
	}
	if body.Peer != own {
		return deny(DenyBadAuth, "wrong node", fmt.Errorf("%w: client expects %s", ErrWrongPeer, body.Peer))
	}
	if len(body.Nonce) != nonceLen {
		return deny(DenyBadAuth, "bad nonce", perr("client nonce of %d bytes", len(body.Nonce)))
	}
	if auth != nil {
		if code, reason := auth(peerID); code != "" {
			return deny(code, reason, &DeniedError{Code: code, Reason: reason})
		}
	}
	sa, err := sign(hc.Identity, sigDomainS, peerID, body.Nonce, ekm, AuthBody{Name: hc.Name, Software: Software, Version: hc.Version, Peer: peerID})
	if err != nil {
		return fail(err)
	}
	if err := avf.WriteMessage(tc, &Verdict{Status: "ok", Auth: sa}); err != nil {
		return fail(err)
	}
	_ = tc.SetDeadline(time.Time{})
	return tc, PeerInfo{NodeID: peerID, Name: body.Name, Software: body.Software, Version: body.Version}, nil
}

// ClientHandshake runs TLS and the AVFED handshake on a dialled
// connection to the peer that must prove node ID want.
func ClientHandshake(ctx context.Context, nc net.Conn, tlsCfg *tls.Config, hc HandshakeConfig, want string) (*tls.Conn, PeerInfo, error) {
	hc.defaults()
	deadline := hc.Now().Add(hc.Timeout)
	_ = nc.SetDeadline(deadline)
	tc := tls.Client(nc, tlsCfg)
	hctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	fail := func(err error) (*tls.Conn, PeerInfo, error) {
		tc.Close()
		return nil, PeerInfo{}, err
	}
	if err := tc.HandshakeContext(hctx); err != nil {
		return fail(err)
	}
	cs := tc.ConnectionState()
	ekm, err := exportKM(&cs)
	if err != nil {
		return fail(err)
	}
	var h Hello
	if err := avf.ReadMessage(tc, &h); err != nil {
		return fail(err)
	}
	switch {
	case h.Proto != Revision:
		return fail(fmt.Errorf("federation: peer speaks revision %d", h.Proto))
	case len(h.Nonce) != nonceLen:
		return fail(perr("hello nonce of %d bytes", len(h.Nonce)))
	case !strings.EqualFold(h.NodeID, want):
		return fail(fmt.Errorf("%w: hello says %s, want %s", ErrWrongPeer, h.NodeID, want))
	}
	own := hc.Identity.FeederID()
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return fail(err)
	}
	ca, err := sign(hc.Identity, sigDomainC, want, h.Nonce, ekm, AuthBody{Name: hc.Name, Software: Software, Version: hc.Version, Nonce: nonce, Peer: want})
	if err != nil {
		return fail(err)
	}
	if err := avf.WriteMessage(tc, ca); err != nil {
		return fail(err)
	}
	var v Verdict
	if err := avf.ReadMessage(tc, &v); err != nil {
		return fail(err)
	}
	if v.Status != "ok" {
		return fail(&DeniedError{Code: v.Code, Reason: v.Reason})
	}
	body, peerID, err := verify(v.Auth, sigDomainS, own, nonce, ekm)
	if err != nil {
		return fail(err)
	}
	if peerID != want {
		return fail(fmt.Errorf("%w: proved %s, want %s", ErrWrongPeer, peerID, want))
	}
	if body.Peer != own {
		return fail(fmt.Errorf("%w: server expects %s", ErrWrongPeer, body.Peer))
	}
	_ = tc.SetDeadline(time.Time{})
	return tc, PeerInfo{NodeID: peerID, Name: body.Name, Software: body.Software, Version: body.Version}, nil
}
