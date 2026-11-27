package avf

import (
	"context"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLiveTCP is a live end-to-end run over real TCP on a fixed address. It
// runs only when AVF_LIVE_ADDR is set (e.g. 127.0.0.1:18511), with the TLS
// directory under AVF_LIVE_DATA (default: a temp dir), AVF_LIVE_FEEDERS
// clients (default 20) and AVF_LIVE_SECONDS (default 10). Each client sends
// ~2000 frames/s in 50 ms batches with seq/ACK; the server verifies seqs,
// ACKs and pings.
func TestLiveTCP(t *testing.T) {
	addr := os.Getenv("AVF_LIVE_ADDR")
	if addr == "" {
		t.Skip("set AVF_LIVE_ADDR to run")
	}
	dataDir := os.Getenv("AVF_LIVE_DATA")
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	feeders := envInt("AVF_LIVE_FEEDERS", 20)
	secs := envInt("AVF_LIVE_SECONDS", 10)
	cert, pin, err := LoadOrCreateSelfSigned(dataDir+"/avf-tls", []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("server pin %s", pin)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var store WatermarkStore
	var framesIn, recordsIn, dups atomic.Int64
	var srvWG sync.WaitGroup
	tlsCfg := ServerTLSConfig(StaticCertificate(cert))
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			srvWG.Add(1)
			go func() {
				defer srvWG.Done()
				p, err := ServerHandshake(ctx, nc, tlsCfg, ServerConfig{ServerID: testServerID, Admission: "open"})
				if err != nil {
					t.Errorf("server handshake: %v", err)
					return
				}
				ack := store.Begin(p.FeederID, p.Body.SeqEpoch)
				c, err := p.Accept(Welcome{Status: StatusApproved, SessionID: p.FeederID, AckSeq: ack,
					Config: WelcomeConfig{BatchMs: 50, Mode: ModeFull, MLAT: true}})
				if err != nil {
					t.Errorf("accept: %v", err)
					return
				}
				seqr := NewSequencer(ack)
				kctx, kcancel := context.WithCancel(ctx)
				go c.RunServerKeepalive(kctx, seqr.Watermark)
				defer func() {
					kcancel()
					store.End(p.FeederID, p.Body.SeqEpoch, seqr.Watermark(), time.Now())
					c.Close()
				}()
				var f Frames
				for {
					rec, err := c.ReadRecord()
					if err != nil {
						return
					}
					recordsIn.Add(1)
					if rec.Type != TFrames {
						continue
					}
					if err := f.Decode(rec.Payload); err != nil {
						t.Errorf("decode: %v", err)
						c.Goodbye(CodeProtocolError, err.Error(), 0)
						return
					}
					ok, err := seqr.Accept(f.Seq)
					if err != nil {
						t.Errorf("seq: %v", err)
						c.Goodbye(CodeProtocolError, err.Error(), 0)
						return
					}
					if !ok {
						dups.Add(1)
						continue
					}
					framesIn.Add(int64(len(f.Frames)))
				}
			}()
		}
	}()

	var sent, acked atomic.Int64
	var cliWG sync.WaitGroup
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for i := 0; i < feeders; i++ {
		cliWG.Add(1)
		go func(i int) {
			defer cliWG.Done()
			time.Sleep(time.Duration(i%100) * 10 * time.Millisecond) // stay under the OS listen backlog
			tc, _ := ClientTLSConfig("127.0.0.1", pin, nil)
			c, _, err := Dial(ctx, addr, tc, ClientConfig{Identity: testIdentity(t), Body: testBody()})
			if err != nil {
				t.Errorf("client %d: %v", i, err)
				return
			}
			defer c.Close()
			ring := NewSendRing("", 0)
			go func() {
				for {
					rec, err := c.ReadRecord()
					if err != nil {
						return
					}
					if rec.Type == TAck {
						v, _ := DecodeAck(rec.Payload)
						ring.Ack(v)
						acked.Store(int64(v))
					}
				}
			}()
			rng := rand.New(rand.NewPCG(uint64(i), 9))
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			buf := make([]byte, 0, 8192)
			for time.Now().Before(deadline) {
				<-tick.C
				f := benchFrames(100, rng) // 100 frames / 50 ms = 2000 fps
				f.Seq = ring.NextSeq()
				buf, err = AppendFrames(buf[:0], f)
				if err != nil {
					t.Error(err)
					return
				}
				ring.Hold(TFrames, f.Seq, buf, time.Now())
				if err := c.WriteRecord(TFrames, buf); err != nil {
					t.Errorf("client %d write: %v", i, err)
					return
				}
				sent.Add(100)
			}
			c.Flush()
			time.Sleep(1500 * time.Millisecond) // let the last ACK arrive
			if n, _ := ring.Held(); n > 2 {
				t.Errorf("client %d: %d records still unacknowledged", i, n)
			}
			c.Goodbye(CodeShuttingDown, "done", 0)
		}(i)
	}
	cliWG.Wait()
	time.Sleep(500 * time.Millisecond)
	cancel()
	ln.Close()
	srvWG.Wait()
	t.Logf("feeders %d, %ds: frames sent %d, received %d (%.0f fps aggregate), records %d, dups %d",
		feeders, secs, sent.Load(), framesIn.Load(), float64(framesIn.Load())/float64(secs), recordsIn.Load(), dups.Load())
	if framesIn.Load() != sent.Load() {
		t.Errorf("lost frames: sent %d received %d", sent.Load(), framesIn.Load())
	}
	if store.Len() != feeders {
		t.Errorf("watermark store has %d epochs", store.Len())
	}
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}
