package sdr

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"testing"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
)

// loopSource returns a Source that plays iq in a loop.
func loopSource(iq []byte) func(n int) []byte {
	pos := 0
	return func(n int) []byte {
		out := make([]byte, 0, 2*n)
		for len(out) < 2*n {
			k := min(2*n-len(out), len(iq)-pos)
			out = append(out, iq[pos:pos+k]...)
			pos = (pos + k) % len(iq)
		}
		return out
	}
}

// testStream is a 0.5 s stream with frames every 2,000 samples at 20 dB.
func testStream(t testing.TB) ([]byte, []SynthFrame) {
	rng := rand.New(rand.NewPCG(9, 9))
	var datas [][]byte
	for i := 0; i < 590; i++ {
		datas = append(datas, randomES(rng))
	}
	truth := layout(rng, datas, 2000, 20, 50e3)
	return Synthesize(SynthConfig{Seed: 6}, 1200000, truth), truth
}

type sinkFunc func(model.RawFrame)

func (s sinkFunc) HandleFrame(f model.RawFrame)     { s(f) }
func (s sinkFunc) HandleUpdate(model.DecodedUpdate) {}

func listen(t testing.TB) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestClientHeaderAndCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := listen(t)
	fake := &FakeRTLTCP{Tuner: TunerR820T, GainCount: 29, Source: func(n int) []byte { return make([]byte, 2*n) }, Rate: 1e5}
	go fake.Serve(ctx, ln)
	c, err := Dial(ctx, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Info().Tuner != TunerR820T || c.Info().GainCount != 29 || c.Info().Tuner.String() != "R820T/R820T2/R860" {
		t.Fatalf("info %+v", c.Info())
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.SetSampleRate(2400000))
	must(c.SetFrequency(1090000000))
	must(c.SetFreqCorrection(-12))
	must(c.SetTunerAGC(false))
	must(c.SetGain(496))
	must(c.SetGainIndex(28))
	must(c.SetRTLAGC(true))
	must(c.SetBiasTee(true))
	must(c.SetDirectSampling(2))
	must(c.SetOffsetTuning(true))
	buf := make([]byte, 1000)
	if _, err := c.Read(buf); err != nil {
		t.Fatal(err)
	}
	want := []FakeCommand{{0x02, 2400000}, {0x01, 1090000000}, {0x05, 0xFFFFFFF4}, {0x03, 1}, {0x04, 496},
		{0x0d, 28}, {0x08, 1}, {0x0e, 1}, {0x09, 2}, {0x0a, 1}}
	deadline := time.Now().Add(2 * time.Second)
	for len(fake.Commands()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := fake.Commands()
	if len(got) != len(want) {
		t.Fatalf("commands %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestClientRejectsNonRTLTCP(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Write([]byte("HTTP/1.1 400 Bad\r\n\r\n"))
			c.Close()
		}
	}()
	_, err := Dial(context.Background(), ln.Addr().String())
	if !errors.Is(err, ErrNotRTLTCP) {
		t.Fatalf("err %v", err)
	}
}

// TestReceiverEndToEnd runs the full path: fake rtl_tcp -> Receiver ->
// RawFrame, checks the configuration commands, the frames against the
// truth and the 12 MHz timestamps against the true sample positions.
func TestReceiverEndToEnd(t *testing.T) {
	iq, truth := testStream(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := listen(t)
	fake := &FakeRTLTCP{Tuner: TunerR820T, GainCount: 29, Source: loopSource(iq), Rate: 12e6} // 5x real time
	go fake.Serve(ctx, ln)

	var mu sync.Mutex
	var frames []model.RawFrame
	var events []model.ClockEventReason
	r := NewReceiver(Options{Addr: ln.Addr().String(), PPM: 3, FeedID: "rtl",
		OnClockEvent: func(e uint32, why model.ClockEventReason) {
			mu.Lock()
			events = append(events, why)
			mu.Unlock()
		}})
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, sinkFunc(func(f model.RawFrame) {
			mu.Lock()
			frames = append(frames, f)
			n := len(frames)
			mu.Unlock()
			if n == 2*len(truth) {
				cancel()
			}
		}))
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(frames) < 2*len(truth) {
		t.Fatalf("%d frames", len(frames))
	}
	period := uint64(len(iq) / 2)
	for i, f := range frames[:2*len(truth)] {
		tf := truth[i%len(truth)]
		if !bytes.Equal(f.Data, tf.Data) || f.Kind != model.FrameModeSLong || f.FeedID != "rtl" || f.Channel != -1 ||
			f.ClockCls != model.ClockSDR12 || f.GPSNs != -1 || f.Clock12MHz != f.Ticks || f.HostNs == 0 || f.Signal > 0 {
			t.Fatalf("frame %d: %+v", i, f)
		}
		start := tf.Start + float64(uint64(i/len(truth))*period)
		if d := float64(f.Ticks) - start*5; math.Abs(d) > 1.5 { // ±125 ns
			t.Fatalf("frame %d: ticks %d, want %.1f", i, f.Ticks, start*5)
		}
		if f.Epoch != frames[0].Epoch {
			t.Fatal("epoch changed")
		}
	}
	if len(events) != 1 || events[0] != model.ClockEvRestart {
		t.Fatalf("clock events %v", events)
	}
	cmds := fake.Commands()
	want := map[byte]uint32{0x02: 2400000, 0x01: 1090000000, 0x05: 3, 0x03: 1, 0x0d: 28, 0x08: 0}
	for _, c := range cmds {
		if w, ok := want[c.Cmd]; ok {
			if c.Param != w {
				t.Fatalf("command %#x = %d, want %d", c.Cmd, c.Param, w)
			}
			delete(want, c.Cmd)
		}
	}
	if len(want) != 0 {
		t.Fatalf("commands not sent: %v (got %v)", want, cmds)
	}
	st := r.Stats()
	if st.Connects != 1 || st.Tuner != "R820T/R820T2/R860" || st.Demod.Frames < uint64(2*len(truth)) || st.LocalDrops != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestReceiverReconnectsWithNewEpoch(t *testing.T) {
	iq, _ := testStream(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := listen(t)
	// each connection delivers 20 chunks (~0.14 s) and is then closed
	fake := &FakeRTLTCP{Tuner: TunerR820T, GainCount: 29, Source: loopSource(iq), CloseAfter: 20}
	go fake.Serve(ctx, ln)
	var mu sync.Mutex
	epochs := map[uint32]int{}
	var reasons []model.ClockEventReason
	r := NewReceiver(Options{Addr: ln.Addr().String(), ReconnectMin: 20 * time.Millisecond, ReconnectMax: 50 * time.Millisecond,
		OnClockEvent: func(_ uint32, why model.ClockEventReason) {
			mu.Lock()
			reasons = append(reasons, why)
			mu.Unlock()
		}})
	go r.Run(ctx, sinkFunc(func(f model.RawFrame) {
		mu.Lock()
		epochs[f.Epoch]++
		mu.Unlock()
	}))
	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		n := len(epochs)
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("epochs %v stats %+v", epochs, r.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if st := r.Stats(); st.Connects < 3 {
		t.Fatalf("connects %d", st.Connects)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, why := range reasons {
		if why != model.ClockEvRestart {
			t.Fatalf("reasons %v", reasons)
		}
	}
}

// TestReceiverDetectsServerDrops paces the fake server in real time and has
// it drop 300 ms of samples; the receiver must start a new epoch with reason
// sample_drop, and the frames after it must carry the new epoch.
func TestReceiverDetectsServerDrops(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time test")
	}
	iq, _ := testStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ln := listen(t)
	const chunk = 24000 // 10 ms
	fake := &FakeRTLTCP{Tuner: TunerR820T, GainCount: 29, Source: loopSource(iq), Rate: 2.4e6, ChunkSamples: chunk,
		Drop: func(i int) bool { return i >= 150 && i < 180 }} // 1.5 s .. 1.8 s
	go fake.Serve(ctx, ln)
	var mu sync.Mutex
	var reasons []model.ClockEventReason
	epochs := map[uint32]int{}
	r := NewReceiver(Options{Addr: ln.Addr().String(),
		OnClockEvent: func(_ uint32, why model.ClockEventReason) {
			mu.Lock()
			reasons = append(reasons, why)
			mu.Unlock()
		}})
	go r.Run(ctx, sinkFunc(func(f model.RawFrame) {
		mu.Lock()
		epochs[f.Epoch]++
		mu.Unlock()
	}))
	for ctx.Err() == nil {
		mu.Lock()
		n := len(reasons)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 2 || reasons[0] != model.ClockEvRestart || reasons[1] != model.ClockEvSampleDrop {
		t.Fatalf("clock events %v", reasons)
	}
	if len(epochs) != 2 {
		t.Fatalf("epochs %v", epochs)
	}
	if st := r.Stats(); st.SampleDrops != 1 {
		t.Fatalf("SampleDrops %d", st.SampleDrops)
	}
}

// TestReceiverLocalDropsKeepTime makes the sink slow so that the reader has
// to discard data; the timestamps of the frames that do get through must
// still match their true sample positions.
func TestReceiverLocalDropsKeepTime(t *testing.T) {
	iq, truth := testStream(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := listen(t)
	fake := &FakeRTLTCP{Tuner: TunerR820T, GainCount: 29, Source: loopSource(iq), ChunkSamples: 4096}
	go fake.Serve(ctx, ln)
	var mu sync.Mutex
	var frames []model.RawFrame
	r := NewReceiver(Options{Addr: ln.Addr().String(), Buffers: 2})
	go r.Run(ctx, sinkFunc(func(f model.RawFrame) {
		time.Sleep(200 * time.Microsecond) // slower than real time
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	}))
	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		n := len(frames)
		mu.Unlock()
		if n >= 3000 && r.Stats().LocalDrops > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d frames, stats %+v", n, r.Stats())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	period := float64(len(iq) / 2)
	byData := map[string]float64{}
	for _, tf := range truth {
		byData[string(tf.Data)] = tf.Start
	}
	for i, f := range frames {
		start, ok := byData[string(f.Data)]
		if !ok {
			t.Fatalf("frame %d not sent", i)
		}
		s := float64(f.Ticks) / 5
		cycles := math.Round((s - start) / period)
		if d := s - start - cycles*period; math.Abs(d) > 0.3 {
			t.Fatalf("frame %d: sample %.2f is %.2f samples off its true position", i, s, d)
		}
	}
	t.Logf("%d frames, %d samples discarded locally", len(frames), r.Stats().LocalDrops)
}

func TestBeastEncodingRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 5))
	var recs []model.RawFrame
	var buf []byte
	for i := 0; i < 500; i++ {
		d := randomES(rng)
		kind := model.FrameModeSLong
		if i%3 == 0 {
			d = randomDF11(rng.Uint32() & 0xFFFFFF)
			kind = model.FrameModeSShort
		}
		if i%7 == 0 {
			d[2] = 0x1A // force escapes
		}
		ticks := rng.Uint64() & (1<<48 - 1)
		if i%5 == 0 {
			ticks = 0x1A1A1A1A1A1A
		}
		f := model.RawFrame{Kind: kind, Data: d, Ticks: ticks, ClockCls: model.ClockSDR12, Signal: -20 * rng.Float64()}
		recs = append(recs, f)
		buf = AppendBeast(buf, f)
	}
	var got []feed.BeastFrame
	p := feed.NewBeastParser(func(b *feed.BeastFrame) {
		c := *b
		c.Payload = append([]byte(nil), b.Payload...)
		got = append(got, c)
	})
	p.Write(buf)
	if len(got) != len(recs) {
		t.Fatalf("parsed %d of %d", len(got), len(recs))
	}
	for i, g := range got {
		r := recs[i]
		if !bytes.Equal(g.Payload, r.Data) || g.Timestamp != r.Ticks {
			t.Fatalf("record %d: %+v vs %+v", i, g, r)
		}
		if d := feed.BeastSignalDBFS(g.Signal) - r.Signal; math.Abs(d) > 0.6 && r.Signal > -40 {
			t.Fatalf("record %d: signal %.2f vs %.2f", i, feed.BeastSignalDBFS(g.Signal), r.Signal)
		}
	}
	if AppendBeast(nil, model.RawFrame{Kind: model.FrameStatus}) != nil {
		t.Fatal("status frame encoded")
	}
}

func TestBeastServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewBeastServer()
	if err := s.Listen(ctx, "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for s.Clients() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	f := model.RawFrame{Kind: model.FrameModeSShort, Data: randomDF11(0x4CA123), Ticks: 123456, ClockCls: model.ClockSDR12, Signal: -10}
	s.HandleFrame(f)
	want := AppendBeast(nil, f)
	got := make([]byte, len(want))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := readFull(c, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("got % x err %v", got, err)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for s.Clients() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Clients() != 0 {
		t.Fatal("client not dropped on close")
	}
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		k, err := c.Read(b[n:])
		n += k
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func TestOptionsAndGain(t *testing.T) {
	for _, tc := range []struct {
		in         string
		tenths     int
		auto, maxG bool
		bad        bool
	}{{"", 0, false, true, false}, {"max", 0, false, true, false}, {"AUTO", 0, true, false, false},
		{"40.2", 402, false, false, false}, {"49.6dB", 496, false, false, false}, {"-3", 0, false, false, true}, {"loud", 0, false, false, true}} {
		tenths, auto, maxG, err := ParseGain(tc.in)
		if (err != nil) != tc.bad || (!tc.bad && (tenths != tc.tenths || auto != tc.auto || maxG != tc.maxG)) {
			t.Errorf("ParseGain(%q) = %d %v %v %v", tc.in, tenths, auto, maxG, err)
		}
	}
	for _, o := range []Options{{Addr: "nohost"}, {Gain: "x"}, {PPM: 900}, {SampleRate: 1000000}, {FrequencyHz: 4e9}} {
		if o.Validate() == nil {
			t.Errorf("%+v accepted", o)
		}
	}
	if err := (Options{}).Validate(); err != nil {
		t.Fatal(err)
	}
	args, err := ChildOptions{Addr: "127.0.0.1:1250", Device: 1}.Args()
	if err != nil || len(args) != 10 || args[1] != "127.0.0.1" || args[3] != "1250" || args[5] != "1" || args[7] != "2400000" || args[9] != "1090000000" {
		t.Fatalf("args %v %v", args, err)
	}
	if err := RunChild(context.Background(), ChildOptions{Path: "Z:\\definitely\\missing\\rtl_tcp.exe"}, nil); err == nil {
		t.Fatal("missing rtl_tcp started")
	}
}
