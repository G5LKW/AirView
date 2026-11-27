package simnet

import (
	"context"
	"testing"
	"time"
)

type countSink struct{ n, bytes uint64 }

func (c *countSink) Write(_ int64, p []byte) { c.bytes += uint64(len(p)) }

func bigNet(tb testing.TB, receivers, aircraft int) *Network {
	tb.Helper()
	specs := RandomReceivers(1, receivers, Placement{CenterLat: 50, CenterLon: 9, RadiusKm: 600})
	n, err := New(Config{Seed: 1, Aircraft: aircraft, Receivers: specs})
	if err != nil {
		tb.Fatal(err)
	}
	return n
}

// BenchmarkNetwork1000 runs 1000 receivers under 2000 aircraft; one op is
// one simulated second (10 steps). With emitters, every receiver also
// writes a Beast stream.
func BenchmarkNetwork1000(b *testing.B) {
	for _, emit := range []bool{false, true} {
		name := "core"
		if emit {
			name = "beast"
		}
		b.Run(name, func(b *testing.B) {
			n := bigNet(b, 1000, 2000)
			if emit {
				for i := 0; i < n.NumReceivers(); i++ {
					n.NewBeastEmitter(i, &countSink{}, BeastOptions{})
				}
			}
			ctx := context.Background()
			if err := n.Advance(ctx, 2*time.Second); err != nil { // warm up
				b.Fatal(err)
			}
			s0 := n.Stats()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := n.Advance(ctx, time.Second); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			s := n.Stats()
			obs := float64(s.Observations - s0.Observations)
			sec := b.Elapsed().Seconds()
			b.ReportMetric(obs/float64(b.N), "obs/simsec")
			b.ReportMetric(float64(s.Transmissions-s0.Transmissions)/float64(b.N), "tx/simsec")
			b.ReportMetric(sec*1e9/obs, "ns/obs")
			b.ReportMetric(float64(b.N)/sec, "x-realtime")
		})
	}
}

// TestScale1000 is a short run of the 1000-receiver network that checks it
// works at that size (skipped with -short).
func TestScale1000(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	n := bigNet(t, 1000, 2000)
	t0 := time.Now()
	if err := n.Advance(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	s := n.Stats()
	if s.Observations < 1_000_000 {
		t.Fatalf("only %d observations", s.Observations)
	}
	t.Logf("1000 receivers, %d aircraft: %d tx, %d observations in 5 simulated s, %v wall", s.Aircraft, s.Transmissions, s.Observations, time.Since(t0))
}
