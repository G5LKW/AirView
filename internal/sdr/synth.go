package sdr

import (
	"math"
	"math/rand/v2"
)

// Synthesizer of RTL-SDR style I/Q streams for tests, benchmarks and demos.
//
// The model: each Mode S frame is a train of rectangular 0.5 µs chips
// (preamble + PPM data) on a carrier offset from the tuner frequency by
// FreqHz with a random phase; the receiver integrates the complex envelope
// over each sample period (rendered at Oversample sub-steps), adds complex
// white Gaussian noise and quantises I and Q to unsigned 8 bits around 127.5
// with clipping, as the RTL2832U delivers them.

// SynthConfig configures Synthesize.
type SynthConfig struct {
	SampleRate float64 // Hz (default 2.4e6)
	Oversample int     // sub-steps per output sample (default 10)
	// NoiseLSB is the RMS of the noise in each of I and Q, in 8-bit LSB
	// (default 4). The noise power per complex sample is 2·NoiseLSB².
	NoiseLSB float64
	// Smooth applies a [0.2 0.6 0.2] filter at the output rate, a harsher
	// band limit than the RTL2832U's own filter (stress test).
	Smooth bool
	Seed   uint64
}

// SynthFrame is one frame to place in a synthesised stream.
type SynthFrame struct {
	Data []byte // 7 or 14 bytes
	// Start is the leading edge of the first preamble pulse as a fractional
	// sample index (the instant Frame.Sample should report).
	Start float64
	// SNRDB is the pulse power over the noise power per complex sample.
	SNRDB float64
	// FreqHz is the carrier offset from the tuned frequency.
	FreqHz float64
	// Phase is the carrier phase at Start, radians (NaN = random).
	Phase float64
}

// Amplitude returns the pulse amplitude in LSB of a frame with the given SNR
// over the configured noise.
func (c SynthConfig) Amplitude(snrDB float64) float64 {
	c = c.withDefaults()
	return c.NoiseLSB * math.Sqrt2 * math.Pow(10, snrDB/20)
}

func (c SynthConfig) withDefaults() SynthConfig {
	if c.SampleRate == 0 {
		c.SampleRate = 2.4e6
	}
	if c.Oversample <= 0 {
		c.Oversample = 10
	}
	if c.NoiseLSB == 0 {
		c.NoiseLSB = 4
	}
	return c
}

// Synthesize returns nSamples complex samples (2·nSamples bytes) containing
// the frames. Frames may overlap (their envelopes add as complex signals).
func Synthesize(cfg SynthConfig, nSamples int, frames []SynthFrame) []byte {
	cfg = cfg.withDefaults()
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x5DEECE66D))
	re := make([]float64, nSamples)
	im := make([]float64, nSamples)
	chip := cfg.SampleRate * chipSeconds
	K := cfg.Oversample
	for _, f := range frames {
		nbits := len(f.Data) * 8
		nch := preambleChips + 2*nbits
		high := func(k int) bool {
			if k < 0 || k >= nch {
				return false
			}
			if k < preambleChips {
				return k == 0 || k == 2 || k == 7 || k == 9
			}
			b := (k - preambleChips) / 2
			one := f.Data[b>>3]&(0x80>>uint(b&7)) != 0
			return one == ((k-preambleChips)%2 == 0)
		}
		amp := cfg.Amplitude(f.SNRDB)
		ph0 := f.Phase
		if math.IsNaN(ph0) {
			ph0 = rng.Float64() * 2 * math.Pi
		}
		w := 2 * math.Pi * f.FreqHz / cfg.SampleRate // rad per sample
		first := int(math.Floor(f.Start))
		last := int(math.Ceil(f.Start + float64(nch)*chip))
		for i := first; i <= last && i < nSamples; i++ {
			if i < 0 {
				continue
			}
			var sr, si float64
			for u := 0; u < K; u++ {
				t := float64(i) + (float64(u)+0.5)/float64(K)
				k := int(math.Floor((t - f.Start) / chip))
				if t < f.Start || !high(k) {
					continue
				}
				a := ph0 + w*(t-f.Start)
				sr += math.Cos(a)
				si += math.Sin(a)
			}
			re[i] += amp * sr / float64(K)
			im[i] += amp * si / float64(K)
		}
	}
	if cfg.Smooth {
		re, im = smooth3(re), smooth3(im)
	}
	out := make([]byte, 2*nSamples)
	for i := 0; i < nSamples; i++ {
		out[2*i] = quant(re[i] + rng.NormFloat64()*cfg.NoiseLSB)
		out[2*i+1] = quant(im[i] + rng.NormFloat64()*cfg.NoiseLSB)
	}
	return out
}

func smooth3(x []float64) []float64 {
	y := make([]float64, len(x))
	for i := range x {
		v := 0.6 * x[i]
		if i > 0 {
			v += 0.2 * x[i-1]
		}
		if i+1 < len(x) {
			v += 0.2 * x[i+1]
		}
		y[i] = v
	}
	return y
}

func quant(v float64) byte {
	v = math.Round(127.5 + v)
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
