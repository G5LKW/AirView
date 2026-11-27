package beastin

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"airview/internal/feed"
)

// newBareLink returns a server with running workers and a link that is
// driven directly through consume (no reader goroutine, no network).
func newBareLink(tb testing.TB, opt Options, sink Sink) (*Server, *link) {
	s, err := New(opt, NewMemRegistry(), sink)
	if err != nil {
		tb.Fatal(err)
	}
	s.startWorkers()
	c1, c2 := net.Pipe()
	ip := netip.MustParseAddr("192.0.2.1")
	l := newLink(s, c1, 1, ip, time.Now())
	l.now = time.Now()
	// Register the link as accept does, so finish and Close work.
	s.mu.Lock()
	s.perIP[ip] = &ipState{conns: 1, unattached: 1, stations: map[string]int{}}
	s.perNet[subnetOf(ip)]++
	s.links[l.id] = l
	s.readers.Add(1)
	s.alive.Add(1)
	s.mu.Unlock()
	tb.Cleanup(func() {
		l.finish()
		c2.Close()
		s.Close()
	})
	return s, l
}

// FuzzLinkConsume feeds arbitrary bytes to a link (after a UUID, as a
// trusted relay so 0xE3 creates sub-stations): nothing a peer sends may
// panic or close the link when the limits are off.
func FuzzLinkConsume(f *testing.F) {
	f.Add([]byte{0x1A, 0x99, 1, 2, 0x1A, '2', 1, 2, 3, 4, 5, 6, 7, 1, 2, 3, 4, 5, 6, 7})
	f.Add(feed.AppendBeastReceiverID(frames(nil, 2, 1), 5))
	f.Add(feed.AppendBeastPing(feed.AppendBeastOption(nil, 'O'), 0x1A1A1A))
	f.Add(feed.AppendBeastUUID(nil, "ffffffffffffffffffffffffffffffffffff"))
	f.Fuzz(func(t *testing.T, data []byte) {
		opt := Options{Policy: Policy{MaxFPS: -1, MaxBytesPerSec: -1, TrustedRelays: []string{testUUID}}, QueueBatches: 1 << 12, Workers: 1}
		_, l := newBareLink(t, opt, SinkFunc(func(*Batch) {}))
		l.consume(feed.AppendBeastUUID(nil, testUUID))
		for len(data) > 0 {
			n := min(len(data), 1+int(data[0])%17)
			l.consume(data[:n])
			l.tick()
			data = data[n:]
		}
		if l.dead {
			t.Fatalf("link closed: %v", *l.reason.Load())
		}
	})
}

// BenchmarkLinkConsume measures the reader path of one link without the
// network: parse, rate limits, decode (timestamps, clock class) and batch
// handover to the delivery pool. Reported per Beast frame.
func BenchmarkLinkConsume(b *testing.B) {
	sink := &countSink{}
	s, l := newBareLink(b, Options{Policy: Policy{MaxFPS: -1, MaxBytesPerSec: -1}, QueueBatches: 1 << 16}, sink)
	l.consume(feed.AppendBeastUUID(nil, testUUID))
	if !l.own.attached {
		b.Fatal("not attached")
	}
	const per = 1000
	buf := frames(nil, per, 1e6)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.now = time.Now()
		l.consume(buf)
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*per), "ns/frame")
	for sink.n.Load() < int64(b.N*per) && s.Stats().QueueDropped == 0 {
		time.Sleep(time.Millisecond)
	}
}

// BenchmarkLoopbackLink measures one link end to end over loopback TCP,
// including the delivery pool. Reported per Beast frame.
func BenchmarkLoopbackLink(b *testing.B) {
	sink := &countSink{}
	s := startServer(b, Options{Policy: Policy{MaxFPS: -1, MaxBytesPerSec: -1}, QueueBatches: 1 << 12}, NewMemRegistry(), sink)
	c := dial(b, s)
	c.Write(feed.AppendBeastUUID(nil, testUUID))
	const per = 1000
	buf := frames(nil, per, 1e6)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
	want := int64(b.N * per)
	for sink.n.Load()+int64(s.Stats().QueueDropped) < want {
		time.Sleep(100 * time.Microsecond)
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(want), "ns/frame")
	if d := s.Stats().QueueDropped; d > 0 {
		b.Logf("queue dropped %d frames", d)
	}
}
