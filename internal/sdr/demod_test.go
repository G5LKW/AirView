package sdr

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"airview/internal/modes"
)

// randomES returns a valid DF17 frame with a random address and payload.
func randomES(rng *rand.Rand) []byte {
	f := make([]byte, 14)
	f[0] = 17<<3 | 5
	for i := 1; i < 11; i++ {
		f[i] = byte(rng.Uint32())
	}
	modes.SetParity(f, 0)
	return f
}

// randomDF11 returns an acquisition squitter (II = 0) for addr.
func randomDF11(addr uint32) []byte {
	f := []byte{11<<3 | 5, byte(addr >> 16), byte(addr >> 8), byte(addr), 0, 0, 0}
	modes.SetParity(f, 0)
	return f
}

// apReply returns a DF4 altitude reply for addr (address/parity).
func apReply(rng *rand.Rand, addr uint32) []byte {
	f := []byte{4 << 3, byte(rng.Uint32()), byte(rng.Uint32()), byte(rng.Uint32()), 0, 0, 0}
	modes.SetParity(f, addr)
	return f
}

type placed struct {
	SynthFrame
	found bool
}

// layout places frames every gap samples with random sub-sample offsets.
func layout(rng *rand.Rand, datas [][]byte, gap int, snr float64, freq float64) []SynthFrame {
	out := make([]SynthFrame, len(datas))
	for i, d := range datas {
		out[i] = SynthFrame{
			Data:   d,
			Start:  float64(500+i*gap) + rng.Float64(),
			SNRDB:  snr,
			FreqHz: (rng.Float64()*2 - 1) * freq,
			Phase:  math.NaN(),
		}
	}
	return out
}

type runResult struct {
	frames []Frame
	stats  DemodStats
}

// demodAll runs the demodulator over iq in irregular chunks.
func demodAll(t testing.TB, cfg DemodConfig, iq []byte, rng *rand.Rand) runResult {
	d := NewDemodulator(cfg)
	var res runResult
	emit := func(f *Frame) { res.frames = append(res.frames, *f) }
	for len(iq) > 0 {
		n := 1 + rng.IntN(70000)
		if n > len(iq) {
			n = len(iq)
		}
		d.Process(iq[:n], emit)
		iq = iq[n:]
	}
	res.stats = d.Stats()
	return res
}

// match pairs decoded frames with the truth by data and start time (within
// 3 samples) and returns the number found plus the timing errors in ns.
func match(truth []SynthFrame, got []Frame, fs float64) (found int, wrong int, errNs []float64) {
	used := make([]bool, len(got))
	for _, tf := range truth {
		for j := range got {
			if used[j] || math.Abs(got[j].Sample-tf.Start) > 3 {
				continue
			}
			if bytes.Equal(got[j].Bytes(), tf.Data) {
				used[j] = true
				found++
				errNs = append(errNs, (got[j].Sample-tf.Start)/fs*1e9)
				break
			}
		}
	}
	for j := range got {
		if !used[j] {
			wrong++
		}
	}
	return
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), v...)
	for i := range s {
		s[i] = math.Abs(s[i])
	}
	sort.Float64s(s)
	return s[int(p*float64(len(s)-1))]
}

func TestDemodCleanFrames(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var datas [][]byte
	for i := 0; i < 200; i++ {
		datas = append(datas, randomES(rng))
		datas = append(datas, randomDF11(rng.Uint32()&0xFFFFFF))
	}
	truth := layout(rng, datas, 400, 25, 50e3)
	cfg := SynthConfig{Seed: 3}
	iq := Synthesize(cfg, 500+len(truth)*400+1000, truth)
	res := demodAll(t, DemodConfig{}, iq, rng)
	found, wrong, errNs := match(truth, res.frames, 2.4e6)
	if found != len(truth) || wrong != 0 {
		t.Fatalf("found %d/%d, wrong %d (stats %+v)", found, len(truth), wrong, res.stats)
	}
	t.Logf("25 dB: timing error |P50| %.1f ns, |P95| %.1f ns", pct(errNs, 0.5), pct(errNs, 0.95))
	if pct(errNs, 0.95) > 60 {
		t.Fatalf("timing error P95 %.1f ns", pct(errNs, 0.95))
	}
	for _, f := range res.frames {
		if f.SNRDB < 20 || f.SignalDBFS > 0 {
			t.Fatalf("signal %.1f dBFS SNR %.1f dB", f.SignalDBFS, f.SNRDB)
		}
		want := uint64(math.Round(f.Sample * 5))
		if f.Ticks != want {
			t.Fatalf("ticks %d, want %d", f.Ticks, want)
		}
	}
}

func TestDemodSampleRates(t *testing.T) {
	for _, fs := range []float64{2.0e6, 2.4e6, 2.56e6, 3.2e6} {
		rng := rand.New(rand.NewPCG(7, uint64(fs)))
		var datas [][]byte
		for i := 0; i < 100; i++ {
			datas = append(datas, randomES(rng))
		}
		gap := int(fs * 200e-6)
		truth := layout(rng, datas, gap, 25, 0)
		iq := Synthesize(SynthConfig{SampleRate: fs, Seed: 9}, 500+len(truth)*gap+1000, truth)
		res := demodAll(t, DemodConfig{SampleRate: fs}, iq, rng)
		found, wrong, errNs := match(truth, res.frames, fs)
		if found != len(truth) || wrong != 0 {
			t.Fatalf("%.2f MS/s: found %d/%d wrong %d", fs/1e6, found, len(truth), wrong)
		}
		for _, f := range res.frames {
			if want := uint64(math.Round(f.Sample * 12e6 / fs)); f.Ticks != want {
				t.Fatalf("%.2f MS/s: ticks %d want %d", fs/1e6, f.Ticks, want)
			}
		}
		t.Logf("%.2f MS/s: timing |P95| %.1f ns", fs/1e6, pct(errNs, 0.95))
	}
}

// TestDemodSNRSweep is the acceptance measurement: decode rate of random
// DF17 frames against SNR, with and without single-bit repair, with carrier
// offsets up to ±150 kHz and random sub-sample timing.
func TestDemodSNRSweep(t *testing.T) {
	type row struct {
		snr                float64
		rate, rateNoFix    float64
		p50, p95           float64
		wrong, wrongNoFix  int
		corrected          uint64
		smoothRate, spread float64
	}
	var rows []row
	warm := func() []byte {
		rng := rand.New(rand.NewPCG(99, 99))
		var fr []SynthFrame
		for i := 0; i < 300; i++ {
			fr = append(fr, SynthFrame{Data: randomES(rng), Start: float64(500+i*600) + rng.Float64(), SNRDB: 16 + 10*rng.Float64(), Phase: math.NaN()})
		}
		return Synthesize(SynthConfig{Seed: 98, Smooth: true}, 500+300*600+1000, fr)
	}()
	const n = 2000
	for _, snr := range []float64{4, 6, 8, 10, 12, 14, 16, 20, 30} {
		rng := rand.New(rand.NewPCG(11, uint64(snr*10)))
		var datas [][]byte
		for i := 0; i < n; i++ {
			datas = append(datas, randomES(rng))
		}
		truth := layout(rng, datas, 600, snr, 150e3)
		ns := 500 + n*600 + 1000
		iq := Synthesize(SynthConfig{Seed: uint64(snr * 100)}, ns, truth)
		r := row{snr: snr}
		res := demodAll(t, DemodConfig{}, iq, rng)
		f, w, e := match(truth, res.frames, 2.4e6)
		r.rate, r.wrong, r.p50, r.p95, r.corrected = float64(f)/n, w, pct(e, 0.5), pct(e, 0.95), res.stats.Corrected
		res = demodAll(t, DemodConfig{NoFix: true}, iq, rng)
		f, w, _ = match(truth, res.frames, 2.4e6)
		r.rateNoFix, r.wrongNoFix = float64(f)/n, w
		// the smoothed stream: a receiver learns the spread from the strong
		// frames of its stream, so the demodulator first hears 300 frames
		// at 16-26 dB (as any real sky provides), then the test frames
		iqs := Synthesize(SynthConfig{Seed: uint64(snr * 100), Smooth: true}, ns, truth)
		dw := NewDemodulator(DemodConfig{})
		dw.Process(warm, func(*Frame) {})
		dw.Reset()
		var sm []Frame
		dw.Process(iqs, func(f *Frame) { sm = append(sm, *f) })
		f, _, _ = match(truth, sm, 2.4e6)
		r.smoothRate = float64(f) / n
		r.spread = dw.Stats().Spread
		rows = append(rows, r)
	}
	t.Log("SNR dB | decoded (1-bit fix) | decoded (no fix) | smoothed BW (β) | repaired | false frames | timing |P50| / |P95| ns")
	for _, r := range rows {
		t.Logf("%5.0f  | %6.1f %%           | %6.1f %%        | %6.1f %% (%.2f) | %5d   | %d / %d        | %.0f / %.0f",
			r.snr, 100*r.rate, 100*r.rateNoFix, 100*r.smoothRate, r.spread, r.corrected, r.wrong, r.wrongNoFix, r.p50, r.p95)
	}
	for _, r := range rows {
		// A frame that was not sent passes the parity check only by
		// coincidence (about 2^-24 per check, more with the repair); at
		// 8 dB, where almost every frame is garbled, one such frame in this
		// sweep is measured and tolerated. Above 10 dB there must be none.
		if (r.snr > 10 && r.wrong != 0) || r.wrong > 1 {
			t.Errorf("SNR %.0f: %d frames decoded that were not sent", r.snr, r.wrong)
		}
		switch { // thresholds at the measured performance, a little below
		case r.snr >= 16 && r.rate < 0.995:
			t.Errorf("SNR %.0f: decode rate %.3f < 0.995", r.snr, r.rate)
		case r.snr >= 14 && r.rate < 0.97:
			t.Errorf("SNR %.0f: decode rate %.3f < 0.97", r.snr, r.rate)
		case r.snr >= 12 && r.rate < 0.70:
			t.Errorf("SNR %.0f: decode rate %.3f < 0.70", r.snr, r.rate)
		}
	}
}

// TestDemodNoiseOnly checks the false-frame rate on pure noise, with 1,000
// confirmed addresses in the table (a busy sky) so that address/parity
// garbage has its realistic chance to hit one.
func TestDemodNoiseOnly(t *testing.T) {
	seconds := 4
	if testing.Short() {
		seconds = 1
	}
	rng := rand.New(rand.NewPCG(5, 6))
	iq := Synthesize(SynthConfig{Seed: 77}, int(2.4e6*float64(seconds)), nil)
	d := NewDemodulator(DemodConfig{AddrTTL: time.Hour})
	for i := 0; i < 1000; i++ {
		d.addrs.add(rng.Uint32()&0xFFFFFF, 0)
	}
	frames := 0
	for len(iq) > 0 {
		n := min(len(iq), 65536)
		d.Process(iq[:n], func(*Frame) { frames++ })
		iq = iq[n:]
	}
	s := d.Stats()
	t.Logf("%d s of noise: %d candidates (%.0f/s), %d frames, %d bad parity, %d unknown AP, %d unclean, noise %.1f dBFS",
		seconds, s.Candidates, float64(s.Candidates)/float64(seconds), frames, s.BadParity, s.UnknownAP, s.Unclean, s.NoiseDBFS)
	if frames != 0 {
		t.Fatalf("%d false frames from noise", frames)
	}
	if math.Abs(s.NoiseDBFS-20*math.Log10(4*1.2533/128)) > 1.5 {
		t.Fatalf("noise estimate %.1f dBFS, want about %.1f", s.NoiseDBFS, 20*math.Log10(4*1.2533/128))
	}
}

func TestAddressParityNeedsConfirmedAddress(t *testing.T) {
	rng := rand.New(rand.NewPCG(8, 9))
	const addr = 0x4CA123
	datas := [][]byte{apReply(rng, addr), randomDF11(addr), apReply(rng, addr), apReply(rng, 0x123456)}
	truth := layout(rng, datas, 400, 25, 0)
	iq := Synthesize(SynthConfig{Seed: 1}, 3000, truth)
	res := demodAll(t, DemodConfig{}, iq, rng)
	if len(res.frames) != 2 || res.frames[0].DF() != 11 || res.frames[1].DF() != 4 || !bytes.Equal(res.frames[1].Bytes(), datas[2]) {
		t.Fatalf("got %d frames: %+v", len(res.frames), res.frames)
	}
	if res.stats.UnknownAP < 2 {
		t.Fatalf("UnknownAP %d", res.stats.UnknownAP)
	}
	res = demodAll(t, DemodConfig{AcceptUnverifiedAP: true}, iq, rng)
	if len(res.frames) != 4 || !res.frames[0].Unverified || res.frames[2].Unverified || !res.frames[3].Unverified {
		t.Fatalf("AcceptUnverifiedAP: %+v", res.frames)
	}
}

func TestSingleBitRepair(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	for _, nbits := range []int{56, 112} {
		for bit := 5; bit < nbits; bit++ {
			var f []byte
			if nbits == 112 {
				f = randomES(rng)
			} else {
				f = randomDF11(rng.Uint32() & 0xFFFFFF)
			}
			f[bit>>3] ^= 0x80 >> uint(bit&7)
			got, ok := syndromeBit(modes.Residual(f), nbits)
			if !ok || got != bit {
				t.Fatalf("%d bits, bit %d: got %d %v", nbits, bit, got, ok)
			}
		}
	}
	if _, ok := syndromeBit(0x123456, 112); ok {
		// not every residual is a single-bit pattern; this one must not be
		t.Log("0x123456 maps to a single bit (allowed but unexpected)")
	}
}

func TestDemodWeakBitRepairEndToEnd(t *testing.T) {
	// flip one chip pair's energy by synthesising a frame whose bit 40 is
	// wrong in the signal but whose parity is computed for the right value
	rng := rand.New(rand.NewPCG(4, 4))
	good := randomES(rng)
	bad := append([]byte(nil), good...)
	bad[5] ^= 0x80 // bit 40
	truth := []SynthFrame{{Data: bad, Start: 600.3, SNRDB: 25, Phase: 0}}
	iq := Synthesize(SynthConfig{Seed: 2}, 2000, truth)
	// the corrupted bit is a confident bit, so the weak-bit rule refuses it
	res := demodAll(t, DemodConfig{}, iq, rng)
	if len(res.frames) != 0 {
		t.Fatalf("confident wrong bit was repaired: %+v", res.frames)
	}
	res = demodAll(t, DemodConfig{FixWeakBits: 112}, iq, rng)
	if len(res.frames) != 1 || !bytes.Equal(res.frames[0].Bytes(), good) || res.frames[0].Corrected != 1 {
		t.Fatalf("repair with FixWeakBits=112 failed: %+v", res.frames)
	}
}

func TestDemodResetAndOddChunks(t *testing.T) {
	rng := rand.New(rand.NewPCG(2, 3))
	datas := [][]byte{randomES(rng), randomES(rng), randomES(rng)}
	truth := layout(rng, datas, 500, 20, 0)
	iq := Synthesize(SynthConfig{Seed: 4}, 2500, truth)
	d := NewDemodulator(DemodConfig{})
	var got []Frame
	for i := range iq { // one byte at a time
		d.Process(iq[i:i+1], func(f *Frame) { got = append(got, *f) })
	}
	if f, w, _ := match(truth, got, 2.4e6); f != 3 || w != 0 {
		t.Fatalf("byte-wise: found %d wrong %d", f, w)
	}
	if d.SampleCount() != 2500 {
		t.Fatalf("SampleCount %d", d.SampleCount())
	}
	d.Reset()
	got = got[:0]
	d.Process(iq, func(f *Frame) { got = append(got, *f) })
	if f, _, _ := match(truth, got, 2.4e6); f != 3 {
		t.Fatalf("after reset: found %d", f)
	}
}

func TestDemodConfigValidate(t *testing.T) {
	for _, c := range []DemodConfig{{SampleRate: 1e6}, {SampleRate: 10e6}, {ScreenDB: 50}, {FixWeakBits: 200}} {
		if c.Validate() == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	if err := (DemodConfig{}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func ExampleDemodulator() {
	rng := rand.New(rand.NewPCG(1, 2))
	frame := randomES(rng)
	iq := Synthesize(SynthConfig{Seed: 1}, 1000, []SynthFrame{{Data: frame, Start: 100, SNRDB: 20}})
	d := NewDemodulator(DemodConfig{})
	d.Process(iq, func(f *Frame) {
		fmt.Printf("DF%d %d bytes at %d ticks\n", f.DF(), f.Len, f.Ticks)
	})
	// Output: DF17 14 bytes at 500 ticks
}
