package sdrfeed

import (
	"context"
	"encoding/hex"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
	"airview/internal/sdr"
)

type countSink struct{ n atomic.Int64 }

func (c *countSink) HandleFrame(f model.RawFrame) {
	if f.ClockCls == model.ClockSDR12 && f.FeedID == "sdr" {
		c.n.Add(1)
	}
}
func (c *countSink) HandleUpdate(model.DecodedUpdate) {}

// TestRTLTCPFeed runs an rtltcp feed through feed.Manager against the fake
// rtl_tcp server streaming a synthesised DF17 every 10 ms.
func TestRTLTCPFeed(t *testing.T) {
	msg, _ := hex.DecodeString("8D4840D6202CC371C32CE0576098")
	var frames []sdr.SynthFrame
	for i := 0; i < 50; i++ {
		frames = append(frames, sdr.SynthFrame{Data: msg, Start: float64(1000 + i*24000), SNRDB: 25, Phase: 0.3})
	}
	iq := sdr.Synthesize(sdr.SynthConfig{Seed: 3}, 1_200_000, frames)
	pos := 0
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &sdr.FakeRTLTCP{Tuner: sdr.TunerR820T, GainCount: 29, Rate: 2.4e6, Source: func(n int) []byte {
		out := make([]byte, 0, 2*n)
		for len(out) < 2*n {
			k := min(2*n-len(out), len(iq)-pos)
			out = append(out, iq[pos:pos+k]...)
			pos = (pos + k) % len(iq)
		}
		return out
	}}
	go fake.Serve(ctx, ln)

	sink := &countSink{}
	m := feed.New(sink)
	defer m.Close()
	m.RegisterType(model.FeedRTLTCP, Factory(t.Logf))
	a := ln.Addr().(*net.TCPAddr)
	m.Apply([]model.Feed{{ID: "sdr", Name: "sdr", Enabled: true, Type: model.FeedRTLTCP, Host: "127.0.0.1", Port: a.Port}})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && sink.n.Load() < 20 {
		time.Sleep(100 * time.Millisecond)
	}
	if sink.n.Load() < 20 {
		t.Fatalf("only %d sdr12 frames", sink.n.Load())
	}
	connected := false
	for time.Now().Before(deadline) && !connected {
		for _, s := range m.Status() {
			connected = connected || (s.ID == "sdr" && s.Connected)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !connected {
		t.Fatalf("feed not shown connected: %+v", m.Status())
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options(model.Feed{ID: "x", Type: model.FeedRTLTCP})
	if o.Addr != "127.0.0.1:1234" || o.Validate() != nil {
		t.Fatalf("%+v %v", o, o.Validate())
	}
}
