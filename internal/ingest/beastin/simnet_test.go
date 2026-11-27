package beastin

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/feed"
	"airview/internal/ingest/beastin/internal/cpustat"
	"airview/internal/model"
	"airview/internal/sim/simnet"
)

// simnetNet builds a small network with one receiver per device.
func simnetNet(t testing.TB, seed uint64, devs ...simnet.Device) *simnet.Network {
	t.Helper()
	var specs []simnet.ReceiverSpec
	for i, d := range devs {
		specs = append(specs, simnet.ReceiverSpec{
			Lat: 50 + 0.3*float64(i), Lon: 8 + 0.2*float64(i), GroundHAE: 150, AntennaAGL: 15,
			Clock: simnet.ClockSpec{Device: d, JitterNs: 50, InitialPPM: 10, CableBiasNs: 100, PPSJitterNs: 20,
				FrameRefShortNs: 64000, FrameRefLongNs: 120000},
		})
	}
	n, err := simnet.New(simnet.Config{Seed: seed, Aircraft: 150, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// parseCount counts what a plain feed.BeastParser sees in b: frames that
// beastin delivers (types 1-4 without the MLAT magic) and the escapes.
func parseCount(b []byte) (deliverable int, uuid string, rxIDs int) {
	p := feed.NewBeastParser(func(f *feed.BeastFrame) {
		switch f.Type {
		case feed.BeastModeAC, feed.BeastModeSShort, feed.BeastModeSLong, feed.BeastStatusType:
			if !feed.IsMLATTimestamp(f.Timestamp) {
				deliverable++
			}
		}
	})
	p.OnEscape = func(typ byte, body []byte) {
		switch typ {
		case feed.BeastUUID:
			uuid, _ = normalizeUUID(string(body))
		case feed.BeastReceiverID:
			rxIDs++
		}
	}
	p.Write(b)
	return
}

// TestSimnetStreams replays simnet's Beast outputs (Radarcape GPS with
// status and position frames, 12 MHz Beast, SDR and readsb
// beast_reduce_plus_out with a receiver ID) through TCP links and checks
// that every frame arrives, attributed to the right station.
func TestSimnetStreams(t *testing.T) {
	devs := []simnet.Device{simnet.DeviceRadarcapeGPS, simnet.DeviceBeast, simnet.DeviceSDR, simnet.DeviceSDR}
	n := simnetNet(t, 41, devs...)
	bufs := make([]*bytes.Buffer, len(devs))
	for i := range devs {
		bufs[i] = &bytes.Buffer{}
		opt := simnet.BeastOptions{PositionEveryS: 5}
		switch i {
		case 2:
			opt = simnet.BeastOptions{Format: simnet.BeastReducePlus, HeartbeatS: 1}
		case 3:
			opt = simnet.BeastOptions{Format: simnet.BeastReducePlus, ReceiverID: 0x1A2B3C4D5E6F7081}
		}
		n.NewBeastEmitter(i, &simnet.WriterSink{W: bufs[i]}, opt)
	}
	if err := n.Advance(context.Background(), 20*time.Second); err != nil {
		t.Fatal(err)
	}

	reg := NewMemRegistry()
	col := &collector{}
	// The streams are replayed as fast as possible: a deep queue keeps the
	// slow collecting sink from shedding batches.
	s := startServer(t, Options{IdentifyWait: 30 * time.Millisecond, QueueBatches: 4096, Policy: Policy{MaxFPS: -1, MaxBytesPerSec: -1}}, reg, col)
	want := 0
	for i, buf := range bufs {
		b := buf.Bytes()
		cnt, uuid, rx := parseCount(b)
		if cnt == 0 {
			t.Fatalf("receiver %d: empty stream", i)
		}
		if i >= 2 {
			wantUUID, _ := normalizeUUID(n.Receiver(i).UUID)
			if uuid == "" || uuid != wantUUID {
				t.Fatalf("receiver %d: uuid %q want %q", i, uuid, wantUUID)
			}
		}
		if i == 3 && rx != 1 {
			t.Fatalf("receiver id escapes %d", rx)
		}
		want += cnt
		c := dial(t, s)
		for off := 0; off < len(b); off += 1400 { // MTU-sized writes
			if _, err := c.Write(b[off:min(off+1400, len(b))]); err != nil {
				t.Fatal(err)
			}
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for col.count() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := col.count(); got != want {
		for _, ls := range s.Links() {
			t.Logf("link %+v", ls)
		}
		t.Fatalf("delivered %d want %d (stats %+v)", got, want, s.Stats())
	}

	// Attribution: the two reduce_plus links are keyed by their UUIDs (the
	// receiver ID of an untrusted link is ignored), the plain Beast links
	// are unidentified.
	byStation := col.byStation()
	for i := 2; i < 4; i++ {
		u, _ := normalizeUUID(n.Receiver(i).UUID)
		st, ok := reg.Lookup("uuid:" + u)
		if !ok || byStation[st] == 0 {
			t.Fatalf("receiver %d: station %v frames %d", i, st, byStation[st])
		}
	}
	// The two UUID-less links from one address are two stations (slots 0
	// and 1 of the address key).
	if len(byStation) != 4 {
		t.Fatalf("stations %v", byStation)
	}
	if st := s.Stats(); st.Links != 4 || st.Identified != 2 || st.Unidentified != 2 || st.RelayIgnored != 1 || st.UnknownEscapes != 0 {
		t.Fatalf("stats %+v", st)
	}
	// Timestamps are classified per link (kept for latency statistics).
	classes := map[model.ClockClass]int{}
	for _, f := range col.snapshot() {
		if f.Kind != model.FrameStatus {
			classes[f.ClockCls]++
		}
	}
	if classes[model.ClockGPSUTC]+classes[model.ClockGPSGPST]+classes[model.ClockGPSEmul] == 0 {
		t.Fatalf("no GPS-class frames from the Radarcape GPS link: %v", classes)
	}
	for _, ls := range s.Links() {
		if ls.Garbage > 0 || ls.Truncated > 0 {
			t.Fatalf("link %d: garbage %d truncated %d", ls.Link, ls.Garbage, ls.Truncated)
		}
	}
}

// --- 1,000 concurrent links -------------------------------------------------------

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// loadStreams returns k reduce_plus streams of d seconds without their
// connection preamble (each load link sends its own UUID).
func loadStreams(t testing.TB, k int, d time.Duration) [][]byte {
	t.Helper()
	specs := simnet.RandomReceivers(7, k, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: 250})
	n, err := simnet.New(simnet.Config{Seed: 7, Aircraft: 400, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	bufs := make([]*bytes.Buffer, k)
	for i := range bufs {
		bufs[i] = &bytes.Buffer{}
		n.NewBeastEmitter(i, &simnet.WriterSink{W: bufs[i]}, simnet.BeastOptions{Format: simnet.BeastReducePlus, NoStatus: true})
	}
	if err := n.Advance(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	out := make([][]byte, k)
	for i, b := range bufs {
		raw := b.Bytes()
		// Drop the 0xE4 UUID (38 bytes) and 'W' 'O' (3 bytes) preamble.
		if len(raw) > 41 && raw[0] == 0x1A && raw[1] == 0xE4 {
			raw = raw[41:]
		}
		out[i] = raw
	}
	return out
}

type countSink struct{ n atomic.Int64 }

func (c *countSink) HandleBeast(b *Batch) { c.n.Add(int64(len(b.Frames))) }

// TestLoad1000Links runs 1,000 concurrent links (BEASTIN_LOAD_LINKS) from
// loopback for BEASTIN_LOAD_SECONDS (default 5) at the simulated feeders'
// real-time rate (BEASTIN_LOAD_SPEED times faster) and reports CPU and
// memory. The CPU figure includes the in-process load generator; the
// beastin-load tool measures server and clients in separate processes.
func TestLoad1000Links(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	links := envInt("BEASTIN_LOAD_LINKS", 1000)
	secs := envInt("BEASTIN_LOAD_SECONDS", 5)
	speed := envInt("BEASTIN_LOAD_SPEED", 1)
	streams := loadStreams(t, 40, 30*time.Second)

	reg := NewMemRegistry()
	sink := &countSink{}
	opt := Options{Policy: Policy{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}}
	s := startServer(t, opt, reg, sink)

	before := cpustat.Take()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var sent atomic.Int64
	var dialErr atomic.Value
	for i := 0; i < links; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := netDial(s)
			if err != nil {
				dialErr.Store(err)
				return
			}
			defer c.Close()
			uuid := strconv.FormatUint(0x10000000_00000000+uint64(i), 16) + "0123456789abcdef"
			c.Write(feed.AppendBeastUUID(nil, uuid))
			src := streams[i%len(streams)]
			per := max(1, len(src)*speed/300) // 30 s of data in 300 ticks of 100 ms
			off := (i * 7919) % len(src)
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				end := off + per
				var chunk []byte
				if end <= len(src) {
					chunk = src[off:end]
					off = end
				} else {
					chunk = append(append([]byte(nil), src[off:]...), src[:end-len(src)]...)
					off = end - len(src)
				}
				if _, err := c.Write(chunk); err != nil {
					return
				}
				sent.Add(int64(len(chunk)))
			}
		}(i)
	}
	// Sample at the half-time mark for a steady-state reading.
	time.Sleep(time.Duration(secs) * time.Second / 2)
	mid := cpustat.Take()
	midStats := s.Stats()
	wg.Wait()
	after := cpustat.Take()
	if err, _ := dialErr.Load().(error); err != nil {
		t.Fatalf("dial: %v", err)
	}
	st := s.Stats()
	wall := after.Wall.Sub(before.Wall).Seconds()
	t.Logf("links %d (peak connected %d, identified %d), %.1f s, GOMAXPROCS-wide CPU %.0f%% of one core (incl. load generator), "+
		"heap %.0f MB, sys %.0f MB, goroutines %d at mid-run",
		links, midStats.Links, midStats.Identified, wall, cpustat.CPUPercent(before, after), mid.HeapMB, mid.SysMB, mid.Goroutines)
	t.Logf("ingest: %.0f frames/s, %.2f MB/s; delivered %d, queue drops %d, rate drops %d, rejected %v",
		float64(st.Frames)/wall, float64(st.Bytes)/wall/1e6, sink.n.Load(), st.QueueDropped, st.RateDropped, st.Rejected)
	if midStats.Links != links || midStats.Identified != links {
		t.Fatalf("only %d of %d links connected (%d identified)", midStats.Links, links, midStats.Identified)
	}
	if st.Frames == 0 || sink.n.Load() == 0 || len(st.Rejected) != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// dialSem limits concurrent dials: a burst of 1,000 dials overflows the
// listen backlog on Windows.
var dialSem = make(chan struct{}, 32)

// netDial dials with retries.
func netDial(s *Server) (net.Conn, error) {
	dialSem <- struct{}{}
	defer func() { <-dialSem }()
	var err error
	for i := 0; i < 30; i++ {
		var c net.Conn
		if c, err = net.DialTimeout("tcp", s.Addr().String(), 5*time.Second); err == nil {
			return c, nil
		}
		time.Sleep(time.Duration(20*(i+1)) * time.Millisecond)
	}
	return nil, err
}

// TestCaptureReplay replays binary captures of real feeders placed in
// testdata/captures/*.bin (for example the user's own readsb
// beast_reduce_plus_out, DESIGN §13.3). It is skipped when there are none.
func TestCaptureReplay(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "captures", "*.bin"))
	if len(files) == 0 {
		t.Skip("no captures in testdata/captures")
	}
	for _, name := range files {
		t.Run(filepath.Base(name), func(t *testing.T) {
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			want, _, _ := parseCount(b)
			col := &collector{}
			s := startServer(t, Options{IdentifyWait: 30 * time.Millisecond, QueueBatches: 1 << 16, Policy: Policy{MaxFPS: -1, MaxBytesPerSec: -1}}, NewMemRegistry(), col)
			c := dial(t, s)
			for off := 0; off < len(b); off += 1400 {
				if _, err := c.Write(b[off:min(off+1400, len(b))]); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, "every frame", func() bool { return col.count() >= want })
			ls := linkOf(t, s)
			t.Logf("%d frames, uuid %q, unknown escapes %d, garbage %d, ping on %v", want, ls.UUID, ls.UnknownEscapes, ls.Garbage, ls.PingOn)
			if ls.Closed {
				t.Fatalf("link closed: %s", ls.CloseReason)
			}
		})
	}
}
