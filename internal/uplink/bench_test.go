package uplink

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"airview/internal/model"
)

// BenchmarkHandleFrame measures the per-frame cost of the uplink tee: rules,
// minute statistics and AVF FRAMES encoding into the RAM ring (DESIGN §13.2
// budget: AVF encode <= 400 ns per frame).
func BenchmarkHandleFrame(b *testing.B) {
	o := Options{Logf: func(string, ...any) {}, SDRoot: func() bool { return false },
		Dial:       func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("offline") },
		BackoffMin: time.Hour, BackoffMax: time.Hour}
	m, err := NewManager(o)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	u := model.DefaultUplink()
	u.ID, u.Host, u.SpoolMB = "bench", "127.0.0.1", 0
	m.Apply([]model.Uplink{u})
	frames := make([]model.RawFrame, 1024)
	g := &frameGen{feedID: "beast", epoch: 1, class: model.ClockHW12}
	h := time.Now().UnixNano()
	for i := range frames {
		frames[i] = g.next(h)
	}
	m.HandleFrame(frames[0])
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := frames[i&1023]
		f.HostNs = h + int64(i)*100_000
		f.Ticks = uint64(i)*1200 + 1
		m.HandleFrame(f)
	}
}

// BenchmarkFramesEncode measures the FRAMES batch encoder of the uplink
// (append + record body encoding, 256 frames per record).
func BenchmarkFramesEncode(b *testing.B) {
	var fb frameBatch
	p := df17(0x4840D6, 1)
	fb.reset(1, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fb.add(true, -1, uint64(i)*1200+1, int64(i)*100_000, 0x80, p)
		if fb.n() >= 256 {
			if _, err := fb.body(); err != nil {
				b.Fatal(err)
			}
			fb.reset(1, 1)
		}
	}
}
