package ingest

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestLearnExcludeHoldout: squitters of excluded (hold-out) aircraft never
// train the stations' latency estimates (DESIGN §9.6); others still do.
func TestLearnExcludeHoldout(t *testing.T) {
	run := func(exclude uint32) (int64, uint64) {
		var asked atomic.Uint64
		x := newTester(t, func(o *Options) {
			o.LearnExclude = func(addr uint32, _ int64) bool {
				asked.Add(1)
				return addr == exclude
			}
		})
		slow := int64(600 * time.Millisecond)
		for i := 0; i < 40; i++ {
			h := t0 + int64(i)*int64(100*time.Millisecond)
			p := df17pos(0x654321, i)
			x.send(tframe(2, 0, h, p), false)
			x.send(tframe(3, 0, h, p), false)
			x.sweepTo(h + int64(25*time.Millisecond))
			f := tframe(4, 0, h, p)
			x.ing.frame(&f, false, max(x.clk.ns.Load(), h+slow))
		}
		x.sweepTo(t0 + int64(8*time.Second))
		ti, _ := x.ing.Timing(4)
		x.finish()
		return ti.LatencyNs, asked.Load()
	}
	if lat, n := run(0x654321); lat != 0 || n == 0 {
		t.Errorf("hold-out aircraft trained the latency: %v (asked %d)", time.Duration(lat), n)
	}
	if lat, _ := run(0x111111); lat < int64(500*time.Millisecond) {
		t.Errorf("other aircraft did not train the latency: %v", time.Duration(lat))
	}
}
