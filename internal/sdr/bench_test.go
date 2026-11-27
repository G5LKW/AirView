package sdr

import (
	"math/rand/v2"
	"testing"
)

// benchStream is 0.5 s of 2.4 MS/s with 1,000 frames (2,000 frames/s, a
// busy site) at SNRs spread over 6-30 dB.
func benchStream() []byte {
	rng := rand.New(rand.NewPCG(3, 3))
	var fr []SynthFrame
	n := int(2.4e6 * 0.5)
	for i := 0; i < 1000; i++ {
		var d []byte
		if i%3 == 0 {
			d = randomDF11(rng.Uint32() & 0xFFFFFF)
		} else {
			d = randomES(rng)
		}
		fr = append(fr, SynthFrame{Data: d, Start: rng.Float64() * float64(n-400), SNRDB: 6 + 24*rng.Float64(), FreqHz: 1e5 * (rng.Float64() - 0.5)})
	}
	return Synthesize(SynthConfig{Seed: 3}, n, fr)
}

// BenchmarkDemod2400k reports the CPU cost of real-time demodulation; the
// budget is 25 % of one core (ns/sample < 104) so a Pi 4 class CPU keeps up.
func BenchmarkDemod2400k(b *testing.B) {
	iq := benchStream()
	d := NewDemodulator(DemodConfig{})
	b.SetBytes(int64(len(iq)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for off := 0; off < len(iq); off += 262144 {
			end := min(off+262144, len(iq))
			d.Process(iq[off:end], func(*Frame) {})
		}
	}
	samples := float64(len(iq)/2) * float64(b.N)
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/samples, "ns/sample")
	b.ReportMetric(100*float64(b.Elapsed().Seconds())/(samples/2.4e6), "%core")
}

// BenchmarkDemodNoise is the floor: noise only.
func BenchmarkDemodNoise(b *testing.B) {
	iq := Synthesize(SynthConfig{Seed: 5}, 1200000, nil)
	d := NewDemodulator(DemodConfig{})
	b.SetBytes(int64(len(iq)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Process(iq, func(*Frame) {})
	}
	samples := float64(len(iq)/2) * float64(b.N)
	b.ReportMetric(100*float64(b.Elapsed().Seconds())/(samples/2.4e6), "%core")
}

func TestBenchStreamDecodes(t *testing.T) {
	iq := benchStream()
	d := NewDemodulator(DemodConfig{})
	n := 0
	d.Process(iq, func(*Frame) { n++ })
	s := d.Stats()
	t.Logf("busy stream: %d of 1000 frames decoded (random overlaps, SNR 6-30 dB), %d candidates, %d repaired", n, s.Candidates, s.Corrected)
	if n < 600 {
		t.Fatalf("only %d frames", n)
	}
}
