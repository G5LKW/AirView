package avfsrv

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/avf"
	"airview/internal/ingest"
	"airview/internal/model"
	"airview/internal/registry"
	"airview/internal/tracker"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// handler counts first copies (a stand-in for the tracker).
type handler struct {
	n, late, updates atomic.Uint64
	mu               sync.Mutex
	frames           []model.RawFrame
}

func (h *handler) Handle(f model.RawFrame) tracker.Result {
	if f.Late {
		h.late.Add(1)
		return tracker.Result{Status: tracker.ResultLate}
	}
	h.n.Add(1)
	h.mu.Lock()
	f.Data = append([]byte(nil), f.Data...)
	h.frames = append(h.frames, f)
	h.mu.Unlock()
	return tracker.Result{Status: tracker.ResultOK, Key: 1}
}
func (h *handler) HandleUpdate(model.DecodedUpdate) { h.updates.Add(1) }
func (h *handler) Lanes() *tracker.Lanes            { return nil }

type env struct {
	t     *testing.T
	reg   *registry.Registry
	ing   *ingest.Ingest
	h     *handler
	srv   *Server
	addr  string
	pin   string
	sid   string
	ctx   context.Context
	stop  context.CancelFunc
	done  chan struct{}
	hooks struct {
		mu       sync.Mutex
		online   map[model.StationIdx]bool
		receiver int
	}
}

func newEnv(t *testing.T, admission string, mut func(*Options, *ingest.Options)) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, h: &handler{}}
	var err error
	e.reg, err = registry.Open(filepath.Join(dir, "stations.db"), registry.Options{
		Policy: registry.Policy{Admission: admission}, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.reg.Close() })
	agg := model.DefaultAggregator()
	agg.Admission = admission
	tlsCfg, pin, err := TLSFromConfig(dir, agg)
	if err != nil {
		t.Fatal(err)
	}
	e.pin = pin
	e.sid, err = LoadOrCreateServerID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := LoadOrCreateServerID(dir); again != e.sid {
		t.Fatalf("server ID not stable: %s vs %s", e.sid, again)
	}
	io := ingest.Options{Handler: e.h, Resolver: e.reg.Resolver(), Logger: quiet()}
	o := OptionsFromConfig(agg)
	o.Addr = "127.0.0.1:0"
	o.TLSConfig, o.ServerID, o.Resolver, o.Logger = tlsCfg, e.sid, e.reg.Resolver(), quiet()
	e.hooks.online = map[model.StationIdx]bool{}
	o.Hooks = Hooks{
		OnOnline: func(st model.StationIdx, on bool) {
			e.hooks.mu.Lock()
			e.hooks.online[st] = on
			e.hooks.mu.Unlock()
		},
		OnReceiver: func(model.StationIdx, avf.Receiver) {
			e.hooks.mu.Lock()
			e.hooks.receiver++
			e.hooks.mu.Unlock()
		},
	}
	if mut != nil {
		mut(&o, &io)
	}
	e.ing, err = ingest.New(io)
	if err != nil {
		t.Fatal(err)
	}
	e.srv, err = New(o, e.reg, e.ing)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		t.Fatal(err)
	}
	e.addr = ln.Addr().String()
	e.ctx, e.stop = context.WithCancel(context.Background())
	e.done = make(chan struct{})
	go e.ing.Run(e.ctx)
	go func() { e.srv.Serve(e.ctx, ln); close(e.done) }()
	t.Cleanup(e.close)
	return e
}

func (e *env) close() {
	e.stop()
	<-e.done
	e.srv.Close()
}

// client is a minimal AVF client.
type client struct {
	t    *testing.T
	id   avf.Identity
	c    *avf.Conn
	res  *avf.ClientResult
	seq  uint64
	ep   string
	recs chan avf.Record
	errc chan error
}

func (e *env) tlsCfg() *tls.Config {
	cfg, err := avf.ClientTLSConfig("127.0.0.1", e.pin, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return cfg
}

func (e *env) dial(id avf.Identity, seqEpoch, enrol string, ackFrom uint64) (*client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if seqEpoch == "" {
		seqEpoch = avf.NewSeqEpoch()
	}
	c, res, err := avf.Dial(ctx, e.addr, e.tlsCfg(), avf.ClientConfig{Identity: id, ServerID: e.sid,
		Body: avf.HelloBody{Software: avf.SoftwareFeeder, Version: "test", SeqEpoch: seqEpoch, EnrolToken: enrol}})
	if err != nil {
		return nil, err
	}
	cl := &client{t: e.t, id: id, c: c, res: res, seq: res.Welcome.AckSeq, ep: seqEpoch,
		recs: make(chan avf.Record, 1024), errc: make(chan error, 1)}
	go func() {
		for {
			r, err := c.ReadRecord()
			if err != nil {
				cl.errc <- err
				close(cl.recs)
				return
			}
			r.Payload = append([]byte(nil), r.Payload...)
			cl.recs <- r
		}
	}()
	return cl, nil
}

func (cl *client) receiver(rx int, cls model.ClockClass) {
	cl.seq++
	if err := cl.c.WriteSeqJSON(avf.TReceiver, cl.seq, &avf.Receiver{RxNo: rx, ClockClass: cls.String(),
		NominalHz: cls.NominalHz(), EpochID: 7, Channels: 1, Timestamps: true, Name: "test rx"}); err != nil {
		cl.t.Fatal(err)
	}
}

func (cl *client) frames(rx int, base int64, payloads ...[]byte) {
	cl.seq++
	fr := avf.Frames{Seq: cl.seq, RxNo: uint8(rx), EpochID: 7, BaseTicks: uint64(base), BaseHostNs: base}
	for i, p := range payloads {
		fr.Frames = append(fr.Frames, avf.Frame{Ticks: uint64(base) + uint64(i)*1000, HostNs: base + int64(i)*1000, Data: p, Sig: 100})
	}
	if err := cl.c.WriteFrames(&fr); err != nil {
		cl.t.Fatal(err)
	}
	cl.c.Flush()
}

// wait returns the first record of type t (fails after 10 s).
func (cl *client) wait(t avf.RecordType) avf.Record {
	cl.t.Helper()
	tm := time.After(10 * time.Second)
	for {
		select {
		case r, ok := <-cl.recs:
			if !ok {
				cl.t.Fatalf("connection closed while waiting for %s: %v", t, <-cl.errc)
			}
			if r.Type == t {
				return r
			}
		case <-tm:
			cl.t.Fatalf("no %s within 10 s", t)
		}
	}
}

func newID(t *testing.T) avf.Identity {
	id, err := avf.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func payload(addr uint32, n byte) []byte {
	return []byte{0x8D, byte(addr >> 16), byte(addr >> 8), byte(addr), 0x58, n, 1, 2, 3, 4, 5, 6, 7, 8}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestOpenSessionFramesAckClaim(t *testing.T) {
	e := newEnv(t, model.AdmissionOpen, nil)
	cl, err := e.dial(newID(t), "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cl.res.Welcome.Status != avf.StatusApproved || cl.res.Welcome.AckSeq != 0 || cl.res.Welcome.Config.Mode != avf.ModeFull {
		t.Fatalf("welcome %+v", cl.res.Welcome)
	}
	cl.receiver(1, model.ClockGPSUTC)
	now := time.Now().UnixNano()
	cl.frames(1, now, payload(0x4840D6, 1), payload(0x4840D6, 2), []byte{0x5D, 0x48, 0x40, 0xD6, 1, 2, 3})
	ack := cl.wait(avf.TAck)
	if v, _ := avf.DecodeAck(ack.Payload); v == 0 {
		t.Fatalf("ack %d", v)
	}
	waitFor(t, "3 first copies", func() bool { return e.h.n.Load() == 3 })
	e.h.mu.Lock()
	f := e.h.frames[0]
	e.h.mu.Unlock()
	if f.Station == 0 || f.ClockCls != model.ClockGPSUTC || f.Ticks != uint64(now) || f.Epoch != 7 || f.HostNs != now ||
		f.FeedID == "" || f.Channel != -1 || f.Recv.IsZero() {
		t.Fatalf("normalised frame %+v", f)
	}
	st, ok := e.reg.Station(f.Station)
	if !ok || st.FeederID != cl.id.FeederID() || st.RxNo != 1 || st.Name != "test rx" {
		t.Fatalf("registry station %+v", st)
	}
	// CLAIM -> CLAIM_CODE
	if err := cl.c.WriteJSON(avf.TClaim, &avf.Claim{RxNo: 1}); err != nil {
		t.Fatal(err)
	}
	r := cl.wait(avf.TClaimCode)
	var cc avf.ClaimCode
	if err := avf.DecodeJSON(r.Payload, &cc); err != nil || len(cc.Code) != model.ClaimCodeLen || cc.RxNo != 1 {
		t.Fatalf("claim code %+v %v", cc, err)
	}
	ss := e.srv.Sessions()
	if len(ss) != 1 || len(ss[0].Stations) != 1 || ss[0].Stations[0].Status != registry.StatusApproved {
		t.Fatalf("sessions %+v", ss)
	}
	// results and feedback reach the feeder
	if !e.srv.SendResults(f.Station, []model.MLATFix{{ICAO: 0x4840D6, TUnixNs: now, Grade: model.GradeA}}) {
		t.Fatal("SendResults refused")
	}
	cl.wait(avf.TMLATResults)
	if ss := e.srv.Sessions(); len(ss) != 1 || len(ss[0].Stations) != 1 || ss[0].Stations[0].Results != 1 || ss[0].Results != 1 {
		t.Fatalf("per-station results %+v", ss)
	}
	if !e.srv.SendFeedback(f.Station, avf.Feedback{Sync: "synced"}) {
		t.Fatal("SendFeedback refused")
	}
	cl.wait(avf.TFeedback)
	// GOODBYE ends the session without grace
	cl.c.Goodbye("shutting_down", "", 0)
	waitFor(t, "offline", func() bool {
		e.hooks.mu.Lock()
		defer e.hooks.mu.Unlock()
		on, seen := e.hooks.online[f.Station]
		return seen && !on
	})
	if e.ing.Online(f.Station) {
		t.Error("station still online after a graceful GOODBYE")
	}
}

func TestInviteAdmission(t *testing.T) {
	e := newEnv(t, model.AdmissionInvite, nil)
	id := newID(t)
	_, err := e.dial(id, "", "", 0)
	var de *avf.DeniedError
	if !errors.As(err, &de) || de.Welcome.Code != avf.CodeEnrolRequired {
		t.Fatalf("no token: %v", err)
	}
	_, err = e.dial(id, "", "avf-bogus", 0)
	if !errors.As(err, &de) || de.Welcome.Code != avf.CodeEnrolInvalid {
		t.Fatalf("bad token: %v", err)
	}
	secret, _, err := e.reg.MintToken(context.Background(), registry.SystemActor, "test", 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := e.dial(id, "", secret, 0)
	if err != nil {
		t.Fatal(err)
	}
	cl.receiver(1, model.ClockHW12)
	cl.receiver(2, model.ClockSDR12) // the approved feeder adds a second receiver without a new token
	waitFor(t, "two stations", func() bool { return len(e.srv.Sessions()) == 1 && len(e.srv.Sessions()[0].Stations) == 2 })
	for _, s := range e.srv.Sessions()[0].Stations {
		if s.Status != registry.StatusApproved {
			t.Errorf("station %+v", s)
		}
	}
	cl.c.Close()
	// the token is used up, but the feeder is known: it reconnects without it
	cl2, err := e.dial(id, "", "", 0)
	if err != nil {
		t.Fatalf("reconnect of an approved feeder: %v", err)
	}
	cl2.c.Close()
	// another feeder cannot reuse the token
	if _, err := e.dial(newID(t), "", secret, 0); !errors.As(err, &de) || de.Welcome.Code != avf.CodeEnrolInvalid {
		t.Fatalf("token reuse: %v", err)
	}
}

func TestReplaceSameIPAndResume(t *testing.T) {
	e := newEnv(t, model.AdmissionOpen, nil)
	id := newID(t)
	ep := avf.NewSeqEpoch()
	cl, err := e.dial(id, ep, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	cl.receiver(1, model.ClockGPSUTC)
	now := time.Now().UnixNano()
	cl.frames(1, now, payload(1, 1))
	cl.frames(1, now+1e6, payload(1, 2))
	waitFor(t, "2 frames", func() bool { return e.h.n.Load() == 2 })
	waitFor(t, "ack 3", func() bool { return e.srv.Sessions()[0].Watermark == 3 })
	// a second session of the same key from the same IP replaces the first
	cl2, err := e.dial(id, ep, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	g := cl.wait(avf.TGoodbyeS)
	var gb avf.Goodbye
	avf.DecodeJSON(g.Payload, &gb)
	if gb.Code != avf.CodeReplaced {
		t.Fatalf("old session got %+v", gb)
	}
	if cl2.res.Welcome.AckSeq != 3 {
		t.Fatalf("resume ackSeq %d, want 3", cl2.res.Welcome.AckSeq)
	}
	// strategy (a): replay held records unchanged (seq 3 is a duplicate),
	// then new data before any RECEIVER: the receiver table was kept
	cl2.seq = 2
	cl2.frames(1, now+1e6, payload(1, 2)) // seq 3: duplicate, dropped
	cl2.frames(1, now+2e6, payload(1, 3)) // seq 4: new
	waitFor(t, "3 frames", func() bool { return e.h.n.Load() == 3 })
	time.Sleep(100 * time.Millisecond)
	if n := e.h.n.Load(); n != 3 {
		t.Fatalf("%d first copies, want 3 (duplicate seq processed?)", n)
	}
	// a seq gap is a protocol error
	cl2.seq += 5
	cl2.frames(1, now+3e6, payload(1, 4))
	g = cl2.wait(avf.TGoodbyeS)
	avf.DecodeJSON(g.Payload, &gb)
	if gb.Code != avf.CodeProtocolError {
		t.Fatalf("gap: %+v", gb)
	}
}

func TestLimitsAndGate(t *testing.T) {
	var gate atomic.Bool
	e := newEnv(t, model.AdmissionOpen, func(o *Options, _ *ingest.Options) {
		o.Policy.Limits.MaxUnauthPerIP = 1
		o.Gate = gate.Load
	})
	c1, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c1.Read(make([]byte, 1)); err == nil {
		t.Fatal("gate closed: the connection must be dropped")
	}
	gate.Store(true)
	a, err := net.Dial("tcp", e.addr) // stays unauthenticated
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	time.Sleep(100 * time.Millisecond)
	b, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := b.Read(make([]byte, 1)); err == nil || errors.Is(err, nil) {
		// closed is expected
	}
	waitFor(t, "rejection counted", func() bool {
		r := e.srv.Stats().Reasons
		return r["gate_closed"] == 1 && r[rejectUnauth] == 1
	})
}

func TestApproveNoticeAndPendingLane(t *testing.T) {
	e := newEnv(t, model.AdmissionApprove, nil)
	cl, err := e.dial(newID(t), "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cl.res.Welcome.Status != avf.StatusPending {
		t.Fatalf("welcome %+v", cl.res.Welcome)
	}
	cl.receiver(1, model.ClockGPSUTC)
	n := cl.wait(avf.TNotice)
	var no avf.Notice
	avf.DecodeJSON(n.Payload, &no)
	if no.Code != avf.NoticePending {
		t.Fatalf("notice %+v", no)
	}
	cl.frames(1, time.Now().UnixNano(), payload(9, 9))
	waitFor(t, "pending frame", func() bool { return e.ing.Stats().Pending == 1 })
	if e.h.n.Load() != 0 {
		t.Fatal("a pending station's frame reached the tracker")
	}
	st := e.srv.Sessions()[0].Stations[0].Station
	if _, err := e.reg.SetAdmission(context.Background(), registry.SystemActor, st, model.AdmApproved); err != nil {
		t.Fatal(err)
	}
	n = cl.wait(avf.TNotice) // checked every 5 s
	avf.DecodeJSON(n.Payload, &no)
	if no.Code != avf.NoticeApproved {
		t.Fatalf("notice %+v", no)
	}
	cl.frames(1, time.Now().UnixNano(), payload(9, 10))
	waitFor(t, "approved frame", func() bool { return e.h.n.Load() == 1 })
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRateLimitFlowAndCooldown(t *testing.T) {
	logs := &syncBuf{}
	e := newEnv(t, model.AdmissionOpen, func(o *Options, io *ingest.Options) {
		o.Logger = slog.New(slog.NewTextHandler(logs, nil))
		io.Limits = model.DefaultAggregator().Limits
		io.Limits.MaxFPSPerStation = 100
		io.FlowGrace = 2 * time.Second
	})
	id := newID(t)
	cl, err := e.dial(id, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	cl.receiver(1, model.ClockGPSUTC)
	stop := make(chan struct{})
	go func() {
		k := byte(0)
		for {
			select {
			case <-stop:
				return
			default:
			}
			ps := make([][]byte, 0, 50)
			for i := 0; i < 50; i++ {
				k++
				ps = append(ps, payload(uint32(k)<<8|uint32(i), k))
			}
			cl.seq++
			fr := avf.Frames{Seq: cl.seq, RxNo: 1, EpochID: 7}
			now := time.Now().UnixNano()
			fr.BaseTicks, fr.BaseHostNs = uint64(now), now
			for i, p := range ps {
				fr.Frames = append(fr.Frames, avf.Frame{Ticks: uint64(now) + uint64(i), HostNs: now, Data: p})
			}
			if cl.c.WriteFrames(&fr) != nil {
				return
			}
			time.Sleep(20 * time.Millisecond) // ~2500 fps
		}
	}()
	defer close(stop)
	fl := cl.wait(avf.TFlow)
	var f avf.Flow
	avf.DecodeJSON(fl.Payload, &f)
	if f.SlowFactor <= 0 || f.SlowFactor > 0.1 {
		t.Fatalf("flow %+v", f)
	}
	g := cl.wait(avf.TGoodbyeS)
	var gb avf.Goodbye
	avf.DecodeJSON(g.Payload, &gb)
	if gb.Code != avf.CodeRateLimited || gb.ReconnectIn != 300 {
		t.Fatalf("goodbye %+v", gb)
	}
	var de *avf.DeniedError
	if _, err := e.dial(id, "", "", 0); !errors.As(err, &de) || de.Welcome.Code != avf.CodeRateLimited || de.Welcome.ReconnectIn < 290 {
		t.Fatalf("cool-down: %v", err)
	}
	// SO-8: the disconnect line names the cause and the cool-down.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "feeder disconnected") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	out := logs.String()
	if !strings.Contains(out, `reason="server: rate_limited`) || !strings.Contains(out, "reconnectIn=5m0s") {
		t.Errorf("disconnect line without its reason:\n%s", out)
	}
	if n := strings.Count(out, "over its rate limit"); n != 1 {
		t.Errorf("rate-limit warning logged %d times:\n%s", n, out)
	}
}

func TestDecodedBackfillAndClockRecords(t *testing.T) {
	e := newEnv(t, model.AdmissionOpen, nil)
	cl, err := e.dial(newID(t), "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	cl.receiver(1, model.ClockSDR12)
	now := time.Now()
	lat, lon := 51.5, -0.1
	cl.seq++
	if err := cl.c.Send(avf.TDecoded, func(d []byte) ([]byte, error) {
		return avf.AppendDecoded(d, cl.seq, 1, []avf.DecodedUpdate{{ICAO: "4840D6", TMs: now.UnixMilli(), Lat: &lat, Lon: &lon}})
	}); err != nil {
		t.Fatal(err)
	}
	cl.seq++
	cl.c.WriteSeqJSON(avf.TClockEvent, cl.seq, &avf.ClockEvent{RxNo: 1, OldEpoch: 7, NewEpoch: 8, Reason: model.ClockEvRestart})
	cl.seq++
	cl.c.WriteSeqJSON(avf.TBackfill, cl.seq, &avf.Backfill{Phase: avf.BackfillBegin})
	cl.frames(1, now.Add(-10*time.Minute).UnixNano(), payload(5, 5))
	cl.seq++
	cl.c.WriteSeqJSON(avf.TBackfill, cl.seq, &avf.Backfill{Phase: avf.BackfillEnd})
	cl.c.Flush()
	waitFor(t, "update and late frame", func() bool { return e.h.updates.Load() == 1 && e.h.late.Load() == 1 })
	ss := e.srv.Sessions()
	if ss[0].Stations[0].Epoch != 8 || ss[0].Backfill {
		t.Fatalf("session %+v", ss[0])
	}
}

// SO-8 / DESIGN §8.4: a read error's reason carries no socket addresses.
func TestErrNoAddr(t *testing.T) {
	err := &net.OpError{Op: "read", Net: "tcp", Source: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		Addr: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 2}, Err: errors.New("connection reset by peer")}
	if got := errNoAddr(err); strings.Contains(got, "203.0.113.9") || strings.Contains(got, "127.0.0.1") || !strings.Contains(got, "reset") {
		t.Fatalf("%q", got)
	}
}
