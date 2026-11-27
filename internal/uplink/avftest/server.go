// Package avftest is an in-process AVF/1 server stub for tests of the uplink
// client (internal/uplink) and of programs built on it (airview-feeder).
//
// It speaks the server side of docs/aggregator/AVF.md through internal/avf
// (TLS 1.3 + ALPN, signed HELLO_C bound to the TLS exporter, DEFLATE record
// streams) and records everything the client sends. It keeps seq
// watermarks per (feeder, seqEpoch) like the real server, sends ACK and
// PING, and checks the client's protocol duties, reporting violations
// through Errors: seq gaps, a first record that is not RECEIVER, late frames
// outside BACKFILL or live frames inside it, timed frames with a zero
// timestamp, unsorted ticks, frames of an epoch that was never announced.
//
// It is not the aggregator: no registry, no MLAT, no ingest. See
// internal/ingest/avfsrv for the real server.
package avftest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
)

// Decision is the server's answer to one HELLO_C.
type Decision struct {
	Status      string // avf.StatusApproved (default) | StatusPending | StatusDenied
	Code        string // denied: AVF error code
	Reason      string
	ReconnectIn int
}

// Options configure the stub.
type Options struct {
	ServerID  string   // default: random
	Name      string   // HELLO_S name
	Admission string   // default "open"
	Features  []string // default avf.AllFeatures
	Mode      string   // WELCOME config.mode (default full)
	BatchMs   int      // WELCOME config.batchMs (default 50)
	// EnrolToken: when set, feeders not yet enrolled must present it
	// (invite admission); a missing token is denied with enrol_required,
	// a wrong one with enrol_invalid.
	EnrolToken string
	// Decide overrides the admission decision (after the token check).
	Decide func(b avf.HelloBody) Decision
	// AckEvery is the ACK interval (default 1 s); PingEvery the PING
	// interval (default 5 s).
	AckEvery, PingEvery time.Duration
	// Motd is the WELCOME motd.
	Motd string
	// Addr is the listen address (default "127.0.0.1:0").
	Addr string
	// Cert, when set, replaces the generated self-signed certificate.
	Cert *tls.Certificate
}

// Record is one accepted (non-duplicate) seq-carrying record.
type Record struct {
	Session    int
	FeederID   string
	SeqEpoch   string
	Type       avf.RecordType
	Seq        uint64
	Body       []byte // after the seq (copy)
	InBackfill bool
}

// Frame is one received frame.
type Frame struct {
	Session   int
	Seq       uint64
	RxNo      int
	Epoch     uint32
	Kind      avf.FrameKind
	Late      bool
	TsInvalid bool
	Channel   int // RF channel, -1 = unknown
	Ticks     uint64
	HostNs    int64
	Sig       byte
	Payload   string // hex
}

// Key identifies a frame independently of the record it travelled in.
func (f Frame) Key() string {
	return fmt.Sprintf("%d/%d/%d/%s", f.RxNo, f.Ticks, f.HostNs/1000, f.Payload)
}

// Server is a running stub.
type Server struct {
	Addr     string
	Host     string
	Port     int
	Pin      string
	ServerID string

	o   Options
	ln  net.Listener
	cfg *tls.Config

	refuse atomic.Bool
	wg     sync.WaitGroup

	mu       sync.Mutex
	conns    map[*sconn]bool
	marks    map[string]uint64
	enrolled map[string]bool
	hellos   []avf.HelloBody
	recs     []Record
	frames   []Frame
	errs     []error
	dups     int
	pongs    int
	goodbyes []avf.Goodbye
	claims   int
	nsess    int
	closed   bool
}

// Start starts a stub on 127.0.0.1 (random port); it is closed at the end
// of the test.
func Start(t testing.TB, o Options) *Server {
	t.Helper()
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// SelfSigned returns a fresh self-signed server certificate and its pin.
func SelfSigned(hosts ...string) (tls.Certificate, string, error) {
	cp, kp, err := avf.GenerateSelfSigned(hosts)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair(cp, kp)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	pin, err := avf.TLSCertPin(&cert)
	return cert, pin, err
}

// New starts a stub without a testing.TB.
func New(o Options) (*Server, error) {
	if o.ServerID == "" {
		var b [10]byte
		_, _ = rand.Read(b[:])
		o.ServerID = strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
	}
	if o.Admission == "" {
		o.Admission = model.AdmissionOpen
	}
	if o.Features == nil {
		o.Features = avf.AllFeatures
	}
	if o.Mode == "" {
		o.Mode = avf.ModeFull
	}
	if o.BatchMs == 0 {
		o.BatchMs = 50
	}
	if o.AckEvery <= 0 {
		o.AckEvery = time.Second
	}
	if o.PingEvery <= 0 {
		o.PingEvery = 5 * time.Second
	}
	var cert tls.Certificate
	var pin string
	var err error
	if o.Cert != nil {
		cert = *o.Cert
		pin, err = avf.TLSCertPin(&cert)
	} else {
		cert, pin, err = SelfSigned("127.0.0.1", "localhost")
	}
	if err != nil {
		return nil, err
	}
	if o.Addr == "" {
		o.Addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return nil, err
	}
	a := ln.Addr().(*net.TCPAddr)
	s := &Server{Addr: a.String(), Host: a.IP.String(), Port: a.Port, Pin: pin, ServerID: o.ServerID, o: o, ln: ln,
		cfg: avf.ServerTLSConfig(avf.StaticCertificate(&cert)), conns: map[*sconn]bool{}, marks: map[string]uint64{},
		enrolled: map[string]bool{}}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Uplink returns an uplink configuration pointing at the stub (RAM ring
// only; set SpoolMB for a spool).
func (s *Server) Uplink(id string) model.Uplink {
	u := model.DefaultUplink()
	u.ID, u.Host, u.Port, u.ServerPin = id, s.Host, s.Port, s.Pin
	u.SpoolMB = 0
	return u
}

// Close stops the stub and every session.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for c := range s.conns {
		_ = c.raw.Close()
	}
	s.mu.Unlock()
	_ = s.ln.Close()
	s.wg.Wait()
}

// Refuse makes the stub drop new TCP connections at once (an outage).
func (s *Server) Refuse(on bool) { s.refuse.Store(on) }

// DropAll aborts every session without GOODBYE (a network failure).
func (s *Server) DropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		_ = c.raw.Close()
	}
}

// Sessions returns the number of established sessions.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for c := range s.conns {
		if c.up.Load() {
			n++
		}
	}
	return n
}

// TotalSessions returns the number of sessions established so far.
func (s *Server) TotalSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nsess
}

func (s *Server) live() []*sconn {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []*sconn
	for c := range s.conns {
		if c.up.Load() {
			list = append(list, c)
		}
	}
	return list
}

// Send writes a server record to every established session (flushed at
// once) and returns how many got it.
func (s *Server) Send(typ avf.RecordType, payload []byte) int {
	n := 0
	for _, c := range s.live() {
		if c.conn.WriteRecord(typ, payload) == nil && c.conn.Flush() == nil {
			n++
		}
	}
	return n
}

// SendJSON sends a JSON record.
func (s *Server) SendJSON(typ avf.RecordType, v any) int {
	b, _ := json.Marshal(v)
	return s.Send(typ, b)
}

// SendResults sends MLAT_RESULTS to every session.
func (s *Server) SendResults(fixes []model.MLATFix) int {
	p, err := avf.AppendMLATResults(nil, fixes)
	if err != nil {
		return 0
	}
	return s.Send(avf.TMLATResults, p)
}

// Goodbye sends GOODBYE to every session and closes them.
func (s *Server) Goodbye(code, reason string, reconnectIn int) {
	for _, c := range s.live() {
		_ = c.conn.Goodbye(code, reason, reconnectIn)
	}
	time.Sleep(50 * time.Millisecond)
	s.DropAll()
}

// Records returns a copy of every accepted (non-duplicate) seq record.
func (s *Server) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.recs)
}

// Frames returns a copy of every received frame.
func (s *Server) Frames() []Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.frames)
}

// FrameCount returns the number of frames received.
func (s *Server) FrameCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

// Errors returns the protocol violations seen.
func (s *Server) Errors() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.errs)
}

// Hellos returns the HELLO_C bodies received (valid signatures only).
func (s *Server) Hellos() []avf.HelloBody {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.hellos)
}

// Duplicates returns the number of records dropped as seq duplicates.
func (s *Server) Duplicates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dups
}

// Pongs returns the number of PONG records received.
func (s *Server) Pongs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pongs
}

// Claims returns the number of CLAIM records received.
func (s *Server) Claims() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims
}

// Goodbyes returns the client GOODBYE records received.
func (s *Server) Goodbyes() []avf.Goodbye {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.goodbyes)
}

// Watermark returns the watermark of (feeder, seqEpoch).
func (s *Server) Watermark(feederID, seqEpoch string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marks[feederID+"|"+seqEpoch]
}

func (s *Server) fail(err error) {
	s.mu.Lock()
	s.errs = append(s.errs, err)
	s.mu.Unlock()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		raw, err := s.ln.Accept()
		if err != nil {
			return
		}
		if s.refuse.Load() {
			_ = raw.Close()
			continue
		}
		c := &sconn{s: s, raw: raw}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = raw.Close()
			return
		}
		s.conns[c] = true
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			c.serve()
			_ = raw.Close()
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
		}()
	}
}

// sconn is one connection.
type sconn struct {
	s    *Server
	raw  net.Conn
	conn *avf.Conn
	up   atomic.Bool

	session  int
	feeder   string
	seqEpoch string
	key      string
	gotRecv  bool
	inBF     bool
	epochs   map[int]uint32 // rx -> announced epoch
}

func (c *sconn) serve() {
	s := c.s
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := avf.ServerHandshake(ctx, c.raw, s.cfg, avf.ServerConfig{ServerID: s.ServerID, Name: s.o.Name,
		Admission: s.o.Admission, Features: s.o.Features})
	if err != nil {
		return
	}
	body := p.Body
	feeder := p.FeederID
	s.mu.Lock()
	s.hellos = append(s.hellos, body)
	enrolled := s.enrolled[feeder]
	s.mu.Unlock()
	d := Decision{Status: avf.StatusApproved}
	switch {
	case s.o.EnrolToken != "" && !enrolled && body.EnrolToken == "":
		d = Decision{Status: avf.StatusDenied, Code: avf.CodeEnrolRequired, Reason: "an enrol token is required"}
	case s.o.EnrolToken != "" && !enrolled && body.EnrolToken != s.o.EnrolToken:
		d = Decision{Status: avf.StatusDenied, Code: avf.CodeEnrolInvalid, Reason: "unknown enrol token"}
	case s.o.Decide != nil:
		d = s.o.Decide(body)
		if d.Status == "" {
			d.Status = avf.StatusApproved
		}
	}
	if d.Status == avf.StatusDenied {
		_ = p.Deny(d.Code, d.Reason, d.ReconnectIn)
		return
	}
	if s.o.EnrolToken != "" && body.EnrolToken == s.o.EnrolToken {
		s.mu.Lock()
		s.enrolled[feeder] = true
		s.mu.Unlock()
	}
	c.feeder, c.seqEpoch = feeder, body.SeqEpoch
	c.key = feeder + "|" + body.SeqEpoch
	c.epochs = map[int]uint32{}
	s.mu.Lock()
	s.nsess++
	c.session = s.nsess
	ack := s.marks[c.key]
	s.mu.Unlock()
	conn, err := p.Accept(avf.Welcome{Status: d.Status, FeederID: feeder, SessionID: fmt.Sprintf("s%d", c.session),
		ResumeToken: fmt.Sprintf("rt%d", c.session), AckSeq: ack,
		Config: avf.WelcomeConfig{BatchMs: s.o.BatchMs, Mode: s.o.Mode, MLAT: true,
			ReturnResults: slices.Contains(p.Features, avf.FeatResults)},
		Features: p.Features, MOTD: s.o.Motd})
	if err != nil {
		return
	}
	c.conn = conn
	c.up.Store(true)
	defer c.up.Store(false)

	done := make(chan struct{})
	defer close(done)
	go c.timers(done)
	for {
		rec, err := conn.ReadRecord()
		if err != nil {
			if errors.Is(err, avf.ErrProtocol) {
				s.fail(fmt.Errorf("session %d: %w", c.session, err))
			}
			return
		}
		if err := c.record(rec.Type, rec.Payload); err != nil {
			s.fail(fmt.Errorf("session %d: %w", c.session, err))
			_ = conn.Goodbye(avf.CodeProtocolError, err.Error(), 0)
			return
		}
	}
}

func (c *sconn) timers(done chan struct{}) {
	s := c.s
	ack := time.NewTicker(s.o.AckEvery)
	ping := time.NewTicker(s.o.PingEvery)
	defer ack.Stop()
	defer ping.Stop()
	var last uint64
	for {
		select {
		case <-done:
			return
		case <-ack.C:
			s.mu.Lock()
			m := s.marks[c.key]
			s.mu.Unlock()
			if m != last {
				last = m
				if c.conn.WriteAck(m) != nil || c.conn.Flush() != nil {
					return
				}
			}
		case <-ping.C:
			if c.conn.WritePing() != nil || c.conn.Flush() != nil {
				return
			}
		}
	}
}

// record processes one client record.
func (c *sconn) record(typ avf.RecordType, p []byte) error {
	s := c.s
	switch typ {
	case avf.TPong:
		s.mu.Lock()
		s.pongs++
		s.mu.Unlock()
		return nil
	case avf.TGoodbyeC:
		var g avf.Goodbye
		_ = avf.DecodeJSON(p, &g)
		s.mu.Lock()
		s.goodbyes = append(s.goodbyes, g)
		s.mu.Unlock()
		return nil
	case avf.TClaim:
		var cl avf.Claim
		if err := avf.DecodeJSON(p, &cl); err != nil {
			return fmt.Errorf("CLAIM: %w", err)
		}
		s.mu.Lock()
		s.claims++
		s.mu.Unlock()
		return c.conn.WriteJSON(avf.TClaimCode, avf.ClaimCode{RxNo: cl.RxNo, Code: "ABCDOIEB",
			ExpiresMs: time.Now().Add(24 * time.Hour).UnixMilli()})
	}
	if !typ.HasSeq() {
		return nil // unknown type: skipped
	}
	seq, body, err := avf.SplitSeq(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	m := s.marks[c.key]
	switch {
	case seq <= m:
		s.dups++
		s.mu.Unlock()
		return nil
	case seq > m+1:
		s.mu.Unlock()
		return fmt.Errorf("seq gap: got %d after watermark %d", seq, m)
	}
	s.marks[c.key] = seq
	s.mu.Unlock()

	if !c.gotRecv && typ != avf.TReceiver {
		return fmt.Errorf("first record is %s, not RECEIVER", typ)
	}
	inBF := c.inBF
	switch typ {
	case avf.TReceiver:
		c.gotRecv = true
		var r avf.Receiver
		if err := avf.DecodeJSON(body, &r); err != nil {
			return fmt.Errorf("RECEIVER: %w", err)
		}
		if err := r.Validate(); err != nil {
			return fmt.Errorf("RECEIVER: %w", err)
		}
		c.epochs[r.RxNo] = r.EpochID
	case avf.TClockEvent:
		var ev avf.ClockEvent
		if err := avf.DecodeJSON(body, &ev); err != nil {
			return fmt.Errorf("CLOCK_EVENT: %w", err)
		}
		c.epochs[ev.RxNo] = ev.NewEpoch
	case avf.TBackfill:
		var b avf.Backfill
		if err := avf.DecodeJSON(body, &b); err != nil {
			return fmt.Errorf("BACKFILL: %w", err)
		}
		switch {
		case b.Phase == avf.BackfillBegin && !c.inBF:
			c.inBF = true
		case b.Phase == avf.BackfillEnd && c.inBF:
			c.inBF = false
		default:
			return fmt.Errorf("BACKFILL %s out of order", b.Phase)
		}
	case avf.TFrames:
		var r avf.Frames
		if err := r.Decode(p); err != nil {
			return fmt.Errorf("FRAMES seq %d: %w", seq, err)
		}
		if ep, ok := c.epochs[int(r.RxNo)]; !inBF && (!ok || (ep != r.EpochID && r.EpochID != 0)) {
			return fmt.Errorf("FRAMES seq %d: rx %d epoch %d not announced (have %d)", seq, r.RxNo, r.EpochID, ep)
		}
		out := make([]Frame, 0, len(r.Frames))
		var prev uint64
		hasPrev := false
		for i := range r.Frames {
			f := &r.Frames[i]
			if f.Late != inBF {
				return fmt.Errorf("frame late=%v inside backfill=%v", f.Late, inBF)
			}
			if !f.TsInvalid {
				if f.Ticks == 0 {
					return errors.New("timed frame with a zero timestamp")
				}
				if hasPrev && f.Ticks < prev {
					return errors.New("ticks not sorted")
				}
				prev, hasPrev = f.Ticks, true
			}
			out = append(out, Frame{Session: c.session, Seq: seq, RxNo: int(r.RxNo), Epoch: r.EpochID, Kind: f.Kind(),
				Late: f.Late, TsInvalid: f.TsInvalid, Channel: int(f.Channel) - 1, Ticks: f.Ticks, HostNs: f.HostNs,
				Sig: f.Sig, Payload: hex.EncodeToString(f.Data)})
		}
		s.mu.Lock()
		s.frames = append(s.frames, out...)
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.recs = append(s.recs, Record{Session: c.session, FeederID: c.feeder, SeqEpoch: c.seqEpoch, Type: typ, Seq: seq,
		Body: bytes.Clone(body), InBackfill: inBF})
	s.mu.Unlock()
	return nil
}
