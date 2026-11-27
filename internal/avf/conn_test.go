package avf

import (
	"context"
	"errors"
	"testing"
	"time"
)

func connectedPair(t *testing.T, srvOpts, cliOpts ConnOptions) (srv, cli *Conn) {
	t.Helper()
	cert, pin := testCert(t)
	addr, results := startServer(t, cert, ServerConfig{Session: srvOpts}, nil)
	tlsCfg, _ := ClientTLSConfig("127.0.0.1", pin, nil)
	c, _, err := Dial(context.Background(), addr, tlsCfg, ClientConfig{Identity: testIdentity(t), Body: testBody(), Session: cliOpts})
	if err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, results)
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { c.Close(); r.conn.Close() })
	return r.conn, c
}

func TestSessionKeepaliveAckAndClock(t *testing.T) {
	srv, cli := connectedPair(t, ConnOptions{}, ConnOptions{})
	seqr := NewSequencer(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kaErr := make(chan error, 1)
	go func() { kaErr <- srv.RunServerKeepalive(ctx, seqr.Watermark) }()

	// Client reader: answers PING automatically, collects ACKs.
	acks := make(chan uint64, 16)
	go func() {
		for {
			rec, err := cli.ReadRecord()
			if err != nil {
				return
			}
			if rec.Type == TAck {
				v, _ := DecodeAck(rec.Payload)
				acks <- v
			}
		}
	}()
	// Client sends 3 FRAMES records.
	for i := uint64(1); i <= 3; i++ {
		f := &Frames{Seq: i, RxNo: 1, Frames: []Frame{{Ticks: i, HostNs: int64(i) * 1000, Data: make([]byte, 7)}}}
		if err := cli.WriteFrames(f); err != nil {
			t.Fatal(err)
		}
	}
	// Duplicate and then a gap.
	cli.WriteFrames(&Frames{Seq: 2, RxNo: 1, Frames: []Frame{{Data: make([]byte, 7)}}})
	cli.WriteFrames(&Frames{Seq: 9, RxNo: 1, Frames: []Frame{{Data: make([]byte, 7)}}})
	cli.Flush()

	var processed, dups int
	var gapErr error
	var pongs int
	for gapErr == nil {
		rec, err := srv.ReadRecord()
		if err != nil {
			t.Fatal(err)
		}
		switch rec.Type {
		case TPong:
			pongs++
		case TFrames:
			var f Frames
			if err := f.Decode(rec.Payload); err != nil {
				t.Fatal(err)
			}
			ok, err := seqr.Accept(f.Seq)
			switch {
			case err != nil:
				gapErr = err
			case ok:
				processed++
			default:
				dups++
			}
		}
	}
	if processed != 3 || dups != 1 || !errors.Is(gapErr, ErrProtocol) || seqr.Watermark() != 3 {
		t.Fatalf("processed %d dups %d gap %v wm %d", processed, dups, gapErr, seqr.Watermark())
	}
	// The immediate PING got a PONG: wait for it if not yet read.
	for pongs == 0 {
		rec, err := srv.ReadRecord()
		if err != nil {
			t.Fatal(err)
		}
		if rec.Type == TPong {
			pongs++
		}
	}
	// ACK of the watermark arrives within ~1 s.
	deadline := time.After(5 * time.Second)
	for got := uint64(0); got != 3; {
		select {
		case got = <-acks:
		case <-deadline:
			t.Fatal("no ACK 3")
		}
	}
	rtt, off, ok := srv.Clock()
	if !ok || rtt < 0 || rtt > time.Second || off > 100*time.Millisecond || off < -100*time.Millisecond {
		t.Fatalf("clock rtt %v off %v ok %v", rtt, off, ok)
	}
	if cli.LastLiveness().IsZero() || srv.Stats().RecordsIn < 5 {
		t.Fatal("stats")
	}
	// Goodbye from the server reaches the client.
	cancel()
	<-kaErr
}

func TestSessionGoodbyeAndDirection(t *testing.T) {
	srv, cli := connectedPair(t, ConnOptions{}, ConnOptions{})
	if err := cli.WriteAck(1); !errors.Is(err, ErrProtocol) {
		t.Fatal("client sent a server record")
	}
	if err := srv.WriteRecord(TFrames, nil); !errors.Is(err, ErrProtocol) {
		t.Fatal("server sent a client record")
	}
	if err := srv.Goodbye(CodeShuttingDown, "restart", 15); err != nil {
		t.Fatal(err)
	}
	rec, err := cli.ReadRecord()
	if err != nil || rec.Type != TGoodbyeS {
		t.Fatal(rec.Type, err)
	}
	var g Goodbye
	if err := DecodeJSON(rec.Payload, &g); err != nil || g.Code != CodeShuttingDown || g.ReconnectIn != 15 {
		t.Fatalf("%+v %v", g, err)
	}
	if _, err := cli.ReadRecord(); err == nil {
		t.Fatal("expected end of session")
	}
}

func TestSessionIdleTimeouts(t *testing.T) {
	// Server idle timeout: no records at all.
	srv, cli := connectedPair(t, ConnOptions{IdleTimeout: 300 * time.Millisecond}, ConnOptions{NoAutoPong: true})
	_ = cli
	start := time.Now()
	if _, err := srv.ReadRecord(); !errors.Is(err, ErrIdle) {
		t.Fatalf("server: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("server idle too late")
	}
	// Client liveness: records other than PING/ACK do not keep it alive.
	srv2, cli2 := connectedPair(t, ConnOptions{}, ConnOptions{IdleTimeout: 400 * time.Millisecond})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				srv2.WriteJSON(TNotice, &Notice{Code: NoticeUpdateAvailable, Level: LevelInfo, Text: "x"})
			}
		}
	}()
	var err error
	for err == nil {
		_, err = cli2.ReadRecord()
	}
	if !errors.Is(err, ErrIdle) {
		t.Fatalf("client: %v", err)
	}
}

func TestClockEstimatorMinRTT(t *testing.T) {
	var e ClockEstimator
	// Feeder clock 5 ms ahead; RTTs 40, 10, 25 ms (symmetric).
	add := func(send, rtt int64) {
		const off = 5_000_000
		rx := send + rtt/2 + off
		tx := rx + 100_000
		e.Add(send, tx-off+rtt/2, Pong{RxNs: rx, TxNs: tx})
	}
	add(1e9, 40e6)
	add(2e9, 10e6)
	add(3e9, 25e6)
	rtt, off, ok := e.Best()
	if !ok || rtt != 10*time.Millisecond || off != 5*time.Millisecond {
		t.Fatal(rtt, off, ok)
	}
	// Old samples expire.
	e.Window = time.Second
	add(100e9, 30e6)
	if rtt, _, _ := e.Best(); rtt != 30*time.Millisecond {
		t.Fatal(rtt)
	}
}
