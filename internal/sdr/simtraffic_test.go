package sdr

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/sim"
)

// frameCollector is a model.FrameSink that keeps the Mode S frames of one
// RF channel.
type frameCollector struct {
	mu     sync.Mutex
	frames []model.RawFrame
}

func (c *frameCollector) HandleFrame(f model.RawFrame) {
	if f.Channel > 0 || (f.Kind != model.FrameModeSShort && f.Kind != model.FrameModeSLong) || f.GPSNs < 0 {
		return
	}
	f.Data = append([]byte(nil), f.Data...)
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
}

func (c *frameCollector) HandleUpdate(model.DecodedUpdate) {}

// TestSimTraffic demodulates the traffic of the AirView simulator (bit-exact
// frames of a realistic sky with its real timing, overlaps and signal
// levels) rendered as an RTL-SDR stream, and compares with the truth.
func TestSimTraffic(t *testing.T) {
	secs := 6.0
	if testing.Short() {
		secs = 2
	}
	s := sim.New(sim.Config{Seed: 21, Start: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC), Aircraft: 250, NoDiurnal: true})
	var col frameCollector
	if err := s.Advance(context.Background(), 30*time.Second, &frameCollector{}); err != nil { // let traffic settle
		t.Fatal(err)
	}
	if err := s.Advance(context.Background(), time.Duration(secs*float64(time.Second))+time.Second, &col); err != nil {
		t.Fatal(err)
	}
	fr := col.frames
	sort.Slice(fr, func(i, j int) bool { return fr[i].GPSNs < fr[j].GPSNs })
	if len(fr) < 100 {
		t.Fatalf("simulator produced %d frames", len(fr))
	}
	const fs = 2.4e6
	cfg := SynthConfig{NoiseLSB: 2, Seed: 1}
	// GPSNs is the end of the frame at the antenna; the start is a frame
	// duration earlier
	t0 := fr[0].GPSNs
	var truth []SynthFrame
	rng := rand.New(rand.NewPCG(4, 2))
	for _, f := range fr {
		durNs := float64(8+len(f.Data)*8) * 1000
		start := (float64(f.GPSNs-t0) - durNs) * 1e-9 * fs
		if start < 100 || start > secs*fs {
			continue
		}
		amp := 128 * math.Pow(10, f.Signal/20)                // LSB
		snr := 20 * math.Log10(amp/(cfg.NoiseLSB*math.Sqrt2)) // dB
		truth = append(truth, SynthFrame{Data: f.Data, Start: start, SNRDB: snr, FreqHz: (rng.Float64() - 0.5) * 2e5, Phase: math.NaN()})
	}
	// render and demodulate in 0.25 s blocks (frames across a boundary are
	// rendered into both)
	block := int(fs / 4)
	total := int(secs * fs)
	d := NewDemodulator(DemodConfig{})
	var got []Frame
	for b0 := 0; b0 < total; b0 += block {
		var in []SynthFrame
		for _, tf := range truth {
			if tf.Start+300 >= float64(b0) && tf.Start < float64(b0+block) {
				tf.Start -= float64(b0)
				in = append(in, tf)
			}
		}
		c := cfg
		c.Seed = uint64(b0) + 1
		d.Process(Synthesize(c, block, in), func(f *Frame) { got = append(got, *f) })
	}
	// score by SNR bucket
	type bucket struct{ sent, found int }
	buckets := map[int]*bucket{}
	key := func(snr float64) int { return int(math.Floor(snr/4)) * 4 }
	used := make([]bool, len(got))
	var errNs []float64
	for _, tf := range truth {
		if tf.Start+300 > float64(total) {
			continue
		}
		k := key(tf.SNRDB)
		if buckets[k] == nil {
			buckets[k] = &bucket{}
		}
		buckets[k].sent++
		for j := range got {
			if !used[j] && math.Abs(got[j].Sample-tf.Start) < 3 && bytes.Equal(got[j].Bytes(), tf.Data) {
				used[j] = true
				buckets[k].found++
				errNs = append(errNs, (got[j].Sample-tf.Start)/fs*1e9)
				break
			}
		}
	}
	wrong := 0
	for j := range got {
		if !used[j] {
			wrong++
			// a frame we decoded but did not send: must at least be one of
			// the transmitted frames at another time (impossible) or noise
			near := ""
			for _, tf := range truth {
				if math.Abs(tf.Start-got[j].Sample) < 3 {
					near = fmt.Sprintf(" (sent there: % x, %.1f dB)", tf.Data, tf.SNRDB)
				}
			}
			t.Logf("not sent: % x at sample %.1f (SNR %.1f dB)%s", got[j].Bytes(), got[j].Sample, got[j].SNRDB, near)
		}
	}
	var keys []int
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	sent, found := 0, 0
	t.Logf("simulator sky, %.0f s, %d frames (%.0f/s), noise %.0f LSB rms", secs, len(truth), float64(len(truth))/secs, cfg.NoiseLSB)
	for _, k := range keys {
		b := buckets[k]
		sent += b.sent
		found += b.found
		t.Logf("  SNR %3d..%3d dB: %5d sent, %5.1f %% decoded", k, k+4, b.sent, 100*float64(b.found)/float64(b.sent))
	}
	st := d.Stats()
	t.Logf("  overall %.1f %% (%d/%d), %d not sent, timing |P50| %.0f ns |P95| %.0f ns, repaired %d, clipped samples %d",
		100*float64(found)/float64(sent), found, sent, wrong, pct(errNs, 0.5), pct(errNs, 0.95), st.Corrected, st.Clipped)
	if wrong != 0 {
		t.Errorf("%d frames decoded that were not sent", wrong)
	}
	// frames at >= 16 dB must decode unless garbled by an overlap
	strong, strongFound := 0, 0
	for _, k := range keys {
		if k >= 16 {
			strong += buckets[k].sent
			strongFound += buckets[k].found
		}
	}
	if strong > 0 && float64(strongFound)/float64(strong) < 0.9 {
		t.Errorf("strong frames: %d/%d decoded", strongFound, strong)
	}
}
