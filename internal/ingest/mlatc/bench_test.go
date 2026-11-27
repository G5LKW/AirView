package mlatc

import (
	"fmt"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// nopSink discards frames.
type nopSink struct{ fakeSink }

func (*nopSink) Submit([]model.RawFrame) {}

// benchConn returns a connection in the attached state without a socket,
// for measuring the per-line path (parse, convert, dedup, batch).
func benchConn(b testing.TB) *conn {
	s, err := New(Options{Policy: Policy{MaxMsgsPerSec: 1 << 30}}, NewMemRegistry(), &nopSink{})
	if err != nil {
		b.Fatal(err)
	}
	c := &conn{s: s, done: make(chan struct{}), outSig: make(chan struct{}, 1)}
	c.att = Attachment{Station: 2, FeedID: "st/x/0", Admission: model.AdmApproved}
	c.clk.setClass(model.ClockSDR12, base12)
	c.clk.epoch = 1
	c.sel = newSelection(100, 100)
	c.batch = make([]model.RawFrame, 0, 256)
	c.arena = make([]byte, 0, 256*14)
	c.nowNs = time.Now().UnixNano()
	return c
}

func BenchmarkHandleLineMLAT(b *testing.B) {
	c := benchConn(b)
	lines := make([][]byte, 1024)
	for i := range lines {
		lines[i] = fmt.Appendf(nil, `{"mlat":{"t":%d,"m":"5d4840d6c0ffee"}}`+"\n", 1_000_000_000+i*12000)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i&1023 == 0 {
			c.clk.have = false // keep the ticks in range
		}
		if err := c.handleLine(lines[i&1023]); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if c.frames.Load() == 0 {
		b.Fatal("no frames")
	}
}

func BenchmarkHandleLineSync(b *testing.B) {
	c := benchConn(b)
	lines := make([][]byte, 1024)
	for i := range lines {
		lines[i] = fmt.Appendf(nil, `{"sync":{"et":%d,"em":"8d4840d6202cc371c32ce0576098","ot":%d,"om":"8d4840d6202cc371c32ce0576099"}}`+"\n",
			1_000_000_000+i*12000, 1_000_006_000+i*12000)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i&1023 == 0 {
			c.clk.have = false
			c.recent = [dedupSlots]dedupEntry{}
		}
		if err := c.handleLine(lines[i&1023]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResultEncode(b *testing.B) {
	p := geo.ToECEF(51, 0, 10000)
	vel := [3]float32{200, 10, -5}
	r := Result{MLATFix: model.MLATFix{ICAO: 0x4840d6, TUnixNs: time.Now().UnixNano(), ECEF: [3]float64{p.X, p.Y, p.Z},
		Cov: [6]float32{100, 1, 2, 100, 3, 400}, Vel: &vel, NBucket: model.NBucket5to8}, Callsign: "KLM123"}
	buf := make([]byte, 0, 512)
	for _, ecef := range []bool{true, false} {
		b.Run(map[bool]string{true: "ecef", false: "legacy"}[ecef], func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				buf = appendResult(buf[:0], &r, ecef)
			}
		})
	}
}

// TestHotPathZeroAlloc pins the 0-allocation property of the per-line path.
func TestHotPathZeroAlloc(t *testing.T) {
	c := benchConn(t)
	mlat := []byte(`{"mlat":{"t":1000000000,"m":"5d4840d6c0ffee"}}` + "\n")
	sync := []byte(`{"sync":{"et":1000012000,"em":"8d4840d6202cc371c32ce0576098","ot":1000018000,"om":"8d4840d6202cc371c32ce0576099"}}` + "\n")
	n := testing.AllocsPerRun(2000, func() {
		c.recent = [dedupSlots]dedupEntry{}
		c.handleLine(mlat)
		c.handleLine(sync)
		if len(c.batch) > 200 {
			c.flushBatch()
		}
	})
	if n != 0 {
		t.Fatalf("%v allocations per line pair", n)
	}
}
