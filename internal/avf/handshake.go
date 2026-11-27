package avf

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"slices"
	"time"

	"airview/internal/model"
)

// Handshake messages (AVF.md §3). Each is "u32 length | JSON object",
// length 1..65536, uncompressed, in the order HELLO_S, HELLO_C, WELCOME.

// HelloS is the server's first message.
type HelloS struct {
	Proto     int      `json:"proto"`
	ProtoMin  int      `json:"protoMin,omitempty"`
	ServerID  string   `json:"serverId"`
	Nonce     []byte   `json:"nonce"`
	TimeNs    int64    `json:"timeNs"`
	Features  []string `json:"features"`
	Admission string   `json:"admission"`
	Name      string   `json:"name,omitempty"`
}

// HelloBody is the signed body of HELLO_C.
type HelloBody struct {
	Pub         []byte   `json:"pub"`
	UUID        string   `json:"uuid,omitempty"`
	Software    string   `json:"software"`
	Version     string   `json:"version"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	Features    []string `json:"features"`
	SeqEpoch    string   `json:"seqEpoch"`
	ResumeToken string   `json:"resumeToken,omitempty"`
	EnrolToken  string   `json:"enrolToken,omitempty"`
}

// HelloC is the client's message: the body bytes as transmitted and the
// Ed25519 signature.
type HelloC struct {
	Body []byte `json:"body"`
	Sig  []byte `json:"sig"`
}

// WelcomeConfig is WELCOME.config.
type WelcomeConfig struct {
	BatchMs       int    `json:"batchMs"`
	Mode          string `json:"mode"`
	MLAT          bool   `json:"mlat"`
	ReturnResults bool   `json:"returnResults"`
}

// Welcome is the server's answer to HELLO_C.
type Welcome struct {
	Status      string        `json:"status"`
	FeederID    string        `json:"feederId,omitempty"`
	SessionID   string        `json:"sessionId,omitempty"`
	ResumeToken string        `json:"resumeToken,omitempty"`
	AckSeq      uint64        `json:"ackSeq"`
	Config      WelcomeConfig `json:"config"`
	Features    []string      `json:"features,omitempty"`
	MOTD        string        `json:"motd,omitempty"`
	ReconnectIn int           `json:"reconnectIn,omitempty"`
	Code        string        `json:"code,omitempty"`
	Reason      string        `json:"reason,omitempty"`
}

// WriteMessage writes one handshake message.
func WriteMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) < 1 || len(b) > MaxHandshakeLen {
		return protoErr("handshake message of %d bytes", len(b))
	}
	buf := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	_, err = w.Write(append(buf, b...))
	return err
}

// ReadMessageBytes reads one handshake message and returns its JSON bytes.
// It reads exactly the message, never beyond it.
func ReadMessageBytes(r io.Reader) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n < 1 || n > MaxHandshakeLen {
		return nil, protoErr("handshake message length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, unexpected(err)
	}
	return b, nil
}

// ReadMessage reads one handshake message into v.
func ReadMessage(r io.Reader, v any) error {
	b, err := ReadMessageBytes(r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return protoErr("bad handshake JSON: %v", err)
	}
	return nil
}

// SignatureMessage builds the signed message of HELLO_C:
// "AVF1" ‖ serverId ‖ nonce ‖ ekm ‖ SHA-256(body).
func SignatureMessage(serverID string, nonce, ekm, body []byte) []byte {
	sum := sha256.Sum256(body)
	m := make([]byte, 0, len(SigMagic)+len(serverID)+len(nonce)+len(ekm)+len(sum))
	m = append(m, SigMagic...)
	m = append(m, serverID...)
	m = append(m, nonce...)
	m = append(m, ekm...)
	return append(m, sum[:]...)
}

// SignHello marshals body (with Pub set from priv) and signs it for the
// TLS session cs and the server's HELLO_S.
func SignHello(priv ed25519.PrivateKey, cs *tls.ConnectionState, hs *HelloS, body HelloBody) (*HelloC, error) {
	ekm, err := ExportKeyingMaterial(cs)
	if err != nil {
		return nil, err
	}
	body.Pub = priv.Public().(ed25519.PublicKey)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, SignatureMessage(hs.ServerID, hs.Nonce, ekm, raw))
	return &HelloC{Body: raw, Sig: sig}, nil
}

// ErrBadSignature is returned when a HELLO_C signature does not verify
// (wrong key, replay, or a TLS-terminating relay in the path).
var ErrBadSignature = errors.New("avf: HELLO_C signature invalid")

// VerifyHello checks the HELLO_C signature against this TLS session and
// the server's own serverId and nonce, and only then decodes the body.
func VerifyHello(hc *HelloC, cs *tls.ConnectionState, serverID string, nonce []byte) (*HelloBody, ed25519.PublicKey, error) {
	var pubOnly struct {
		Pub []byte `json:"pub"`
	}
	if err := json.Unmarshal(hc.Body, &pubOnly); err != nil {
		return nil, nil, protoErr("HELLO_C body: %v", err)
	}
	if len(pubOnly.Pub) != ed25519.PublicKeySize || len(hc.Sig) != ed25519.SignatureSize {
		return nil, nil, protoErr("HELLO_C pub/sig length")
	}
	ekm, err := ExportKeyingMaterial(cs)
	if err != nil {
		return nil, nil, err
	}
	pub := ed25519.PublicKey(pubOnly.Pub)
	if !ed25519.Verify(pub, SignatureMessage(serverID, nonce, ekm, hc.Body), hc.Sig) {
		return nil, nil, ErrBadSignature
	}
	var body HelloBody
	if err := json.Unmarshal(hc.Body, &body); err != nil {
		return nil, nil, protoErr("HELLO_C body: %v", err)
	}
	return &body, pub, nil
}

// IntersectFeatures returns the features present in both lists, in the
// order of a, without duplicates.
func IntersectFeatures(a, b []string) []string {
	out := make([]string, 0, len(a))
	for _, f := range a {
		if slices.Contains(b, f) && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// HandshakeError is a handshake failure the server answered with a denied
// WELCOME carrying Code.
type HandshakeError struct {
	Code string
	Err  error
}

func (e *HandshakeError) Error() string { return "avf: handshake " + e.Code + ": " + e.Err.Error() }
func (e *HandshakeError) Unwrap() error { return e.Err }

// DeniedError is returned to a client whose WELCOME was denied.
type DeniedError struct {
	Welcome Welcome
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("avf: denied by server: %s (%s)", e.Welcome.Code, e.Welcome.Reason)
}

// RetryAfter returns the wait the client must honour before reconnecting:
// reconnectIn, or at least DeniedMinBackoff when it is absent.
func (e *DeniedError) RetryAfter() time.Duration {
	if e.Welcome.ReconnectIn > 0 {
		return time.Duration(e.Welcome.ReconnectIn) * time.Second
	}
	return DeniedMinBackoff
}

// ---------------------------------------------------------------- server

// ServerConfig configures the server side of the handshake.
type ServerConfig struct {
	// ServerID is the network's stable ID (16 base32 characters).
	ServerID string
	// Name is the network display name (optional).
	Name string
	// Admission is model.AdmissionOpen / Approve / Invite.
	Admission string
	// Features offered (default AllFeatures).
	Features []string
	// HandshakeTimeout bounds TCP accept to WELCOME (default 10 s).
	HandshakeTimeout time.Duration
	// Session options for the record stream.
	Session ConnOptions
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// PendingSession is a server-side connection whose HELLO_C was verified
// and that waits for the caller's admission decision (Welcome or Deny).
// The handshake deadline stays armed until then.
type PendingSession struct {
	tc  *tls.Conn
	cfg ServerConfig

	// HelloS is what the server sent.
	HelloS HelloS
	// Body is the verified HELLO_C body; BodyRaw its bytes as signed.
	Body    HelloBody
	BodyRaw []byte
	// Pub is the feeder's public key and FeederID the ID derived from it.
	Pub      ed25519.PublicKey
	FeederID string
	// Features is the effective feature set (intersection).
	Features []string
	done     bool
}

// ServerHandshake runs the TLS handshake and the HELLO_S / HELLO_C
// exchange on an accepted connection. On a malformed HELLO_C, a missing
// "flate" feature or an invalid signature it sends a denied WELCOME
// (bad_hello, proto_version, bad_signature), closes the connection and
// returns a *HandshakeError. Other failures close the connection silently.
func ServerHandshake(ctx context.Context, nc net.Conn, tlsCfg *tls.Config, cfg ServerConfig) (*PendingSession, error) {
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Features == nil {
		cfg.Features = AllFeatures
	}
	if !model.ValidID(cfg.ServerID) {
		nc.Close()
		return nil, fmt.Errorf("avf: invalid server ID %q", cfg.ServerID)
	}
	deadline := cfg.Now().Add(cfg.HandshakeTimeout)
	_ = nc.SetDeadline(deadline)
	tc := tls.Server(nc, tlsCfg)
	hctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		tc.Close()
		return nil, err
	}
	cs := tc.ConnectionState()
	if cs.Version != tls.VersionTLS13 || cs.NegotiatedProtocol != ALPN {
		tc.Close()
		return nil, ErrNoALPN
	}
	nonce := make([]byte, NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		tc.Close()
		return nil, err
	}
	p := &PendingSession{tc: tc, cfg: cfg}
	p.HelloS = HelloS{
		Proto: Revision, ProtoMin: Revision, ServerID: cfg.ServerID, Nonce: nonce,
		TimeNs: cfg.Now().UnixNano(), Features: cfg.Features, Admission: cfg.Admission, Name: cfg.Name,
	}
	if err := WriteMessage(tc, &p.HelloS); err != nil {
		tc.Close()
		return nil, err
	}
	var hc HelloC
	if err := ReadMessage(tc, &hc); err != nil {
		if errors.Is(err, ErrProtocol) {
			return nil, p.fail(CodeBadHello, err)
		}
		tc.Close()
		return nil, err
	}
	body, pub, err := VerifyHello(&hc, &cs, cfg.ServerID, nonce)
	if err != nil {
		if errors.Is(err, ErrBadSignature) {
			return nil, p.fail(CodeBadSignature, err)
		}
		return nil, p.fail(CodeBadHello, err)
	}
	if err := checkBody(body); err != nil {
		return nil, p.fail(CodeBadHello, err)
	}
	if !slices.Contains(body.Features, FeatFlate) {
		return nil, p.fail(CodeProtoVersion, errors.New("client lacks the flate feature"))
	}
	p.Body, p.BodyRaw, p.Pub = *body, hc.Body, pub
	p.FeederID = model.FeederIDFromPub(pub)
	p.Features = IntersectFeatures(cfg.Features, body.Features)
	return p, nil
}

func checkBody(b *HelloBody) error {
	switch {
	case b.Software == "" || b.Version == "" || b.OS == "" || b.Arch == "":
		return protoErr("HELLO_C lacks software/version/os/arch")
	case !ValidSeqEpoch(b.SeqEpoch):
		return protoErr("HELLO_C seqEpoch %q", b.SeqEpoch)
	case len(b.Software) > 64 || len(b.Version) > 64 || len(b.OS) > 32 || len(b.Arch) > 32:
		return protoErr("HELLO_C field too long")
	case len(b.ResumeToken) > 512 || len(b.EnrolToken) > 512:
		return protoErr("HELLO_C token too long")
	}
	if b.UUID != "" {
		u, ok := model.NormalizeUUID(b.UUID)
		if !ok {
			b.UUID = "" // unusable; the UUID is optional
		} else {
			b.UUID = u
		}
	}
	return nil
}

func (p *PendingSession) fail(code string, err error) error {
	_ = WriteMessage(p.tc, &Welcome{Status: StatusDenied, Code: code, Reason: failReason(code)})
	p.tc.Close()
	p.done = true
	return &HandshakeError{Code: code, Err: err}
}

func failReason(code string) string {
	switch code {
	case CodeBadSignature:
		return "identity signature invalid (a TLS-terminating proxy in the path?)"
	case CodeProtoVersion:
		return "protocol version not supported; update the feeder"
	}
	return "malformed HELLO"
}

// Conn returns the TLS connection (for RemoteAddr, logging).
func (p *PendingSession) Conn() *tls.Conn { return p.tc }

// Accept sends an approved or pending WELCOME and returns the record
// session. FeederID and Features are filled when empty.
func (p *PendingSession) Accept(w Welcome) (*Conn, error) {
	if p.done {
		return nil, errors.New("avf: session already answered")
	}
	if w.Status != StatusApproved && w.Status != StatusPending {
		return nil, fmt.Errorf("avf: Accept with status %q", w.Status)
	}
	if w.FeederID == "" {
		w.FeederID = p.FeederID
	}
	if w.Features == nil {
		w.Features = p.Features
	}
	if w.Config.BatchMs <= 0 {
		w.Config.BatchMs = int(DefaultBatch / time.Millisecond)
	}
	if w.Config.Mode == "" {
		w.Config.Mode = ModeFull
	}
	p.done = true
	if err := WriteMessage(p.tc, &w); err != nil {
		p.tc.Close()
		return nil, err
	}
	_ = p.tc.SetDeadline(time.Time{})
	opts := p.cfg.Session
	return newConn(p.tc, FromServer, opts, w.Features), nil
}

// Deny sends a denied WELCOME and closes the connection.
func (p *PendingSession) Deny(code, reason string, reconnectIn int) error {
	if p.done {
		return errors.New("avf: session already answered")
	}
	p.done = true
	err := WriteMessage(p.tc, &Welcome{Status: StatusDenied, Code: code, Reason: reason, ReconnectIn: reconnectIn})
	if cerr := p.tc.Close(); err == nil {
		err = cerr
	}
	return err
}

// Close abandons the session without an answer.
func (p *PendingSession) Close() error {
	p.done = true
	return p.tc.Close()
}

// ---------------------------------------------------------------- client

// ClientConfig configures the client side of the handshake.
type ClientConfig struct {
	// Identity signs HELLO_C.
	Identity Identity
	// Body is the HELLO_C body. Pub is set from Identity; empty OS/Arch get
	// runtime values; nil Features get AllFeatures.
	Body HelloBody
	// ServerID, when set, must equal HELLO_S serverId (a changed network
	// identity is reported as an error).
	ServerID string
	// HandshakeTimeout bounds the TLS handshake to WELCOME (default 10 s).
	HandshakeTimeout time.Duration
	// Session options for the record stream. Batch defaults to WELCOME
	// config.batchMs.
	Session ConnOptions
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// ClientResult is what the client learned during the handshake.
type ClientResult struct {
	HelloS   HelloS
	Welcome  Welcome
	Features []string // effective features
}

// ErrProtoVersion is returned when the server speaks no common revision or
// lacks the flate feature.
var ErrProtoVersion = errors.New("avf: no common protocol revision")

// ClientHandshake runs the TLS handshake and HELLO_S / HELLO_C / WELCOME on
// nc. On approval or pending it returns the record session. A denied
// WELCOME is returned as a *DeniedError together with the result.
func ClientHandshake(ctx context.Context, nc net.Conn, tlsCfg *tls.Config, cfg ClientConfig) (*Conn, *ClientResult, error) {
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Identity.Priv == nil {
		nc.Close()
		return nil, nil, errors.New("avf: client identity missing")
	}
	body := cfg.Body
	if body.OS == "" {
		body.OS = runtime.GOOS
	}
	if body.Arch == "" {
		body.Arch = runtime.GOARCH
	}
	if body.Features == nil {
		body.Features = AllFeatures
	}
	deadline := cfg.Now().Add(cfg.HandshakeTimeout)
	_ = nc.SetDeadline(deadline)
	tc := tls.Client(nc, tlsCfg)
	hctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		tc.Close()
		return nil, nil, err
	}
	cs := tc.ConnectionState()
	if cs.NegotiatedProtocol != ALPN {
		tc.Close()
		return nil, nil, ErrNoALPN
	}
	res := &ClientResult{}
	fail := func(err error) (*Conn, *ClientResult, error) {
		tc.Close()
		return nil, res, err
	}
	if err := ReadMessage(tc, &res.HelloS); err != nil {
		return fail(err)
	}
	hs := &res.HelloS
	protoMin := hs.ProtoMin
	if protoMin == 0 {
		protoMin = hs.Proto
	}
	switch {
	case hs.Proto < 1 || protoMin > Revision:
		return fail(fmt.Errorf("%w: server speaks %d..%d", ErrProtoVersion, protoMin, hs.Proto))
	case !slices.Contains(hs.Features, FeatFlate):
		return fail(fmt.Errorf("%w: server lacks flate", ErrProtoVersion))
	case !model.ValidID(hs.ServerID):
		return fail(protoErr("HELLO_S serverId %q", hs.ServerID))
	case len(hs.Nonce) != NonceLen:
		return fail(protoErr("HELLO_S nonce of %d bytes", len(hs.Nonce)))
	case cfg.ServerID != "" && cfg.ServerID != hs.ServerID:
		return fail(fmt.Errorf("avf: server ID changed from %s to %s", cfg.ServerID, hs.ServerID))
	}
	hc, err := SignHello(cfg.Identity.Priv, &cs, hs, body)
	if err != nil {
		return fail(err)
	}
	if err := WriteMessage(tc, hc); err != nil {
		return fail(err)
	}
	w := &res.Welcome
	if err := ReadMessage(tc, w); err != nil {
		return fail(err)
	}
	if w.Features != nil {
		res.Features = IntersectFeatures(w.Features, body.Features)
	} else {
		res.Features = IntersectFeatures(hs.Features, body.Features)
	}
	switch w.Status {
	case StatusApproved, StatusPending:
	case StatusDenied:
		return fail(&DeniedError{Welcome: *w})
	default:
		return fail(protoErr("WELCOME status %q", w.Status))
	}
	_ = tc.SetDeadline(time.Time{})
	opts := cfg.Session
	if opts.Batch == 0 && w.Config.BatchMs > 0 {
		opts.Batch = time.Duration(w.Config.BatchMs) * time.Millisecond
	}
	return newConn(tc, FromClient, opts, res.Features), res, nil
}

// Dial connects to addr ("host:port") and runs ClientHandshake.
func Dial(ctx context.Context, addr string, tlsCfg *tls.Config, cfg ClientConfig) (*Conn, *ClientResult, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	return ClientHandshake(ctx, nc, tlsCfg, cfg)
}
