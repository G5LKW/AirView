package sdr

import (
	"errors"
	"math"
	"time"

	"airview/internal/modes"
)

// TicksPerSecond is the rate of the timestamps the demodulator produces: the
// 12 MHz counter convention of Beast receivers (DESIGN §5.2, clock class
// sdr12), so the frames can travel through every existing 12 MHz path.
const TicksPerSecond = 12e6

// Mode S signal geometry (ICAO Annex 10 Vol IV §3.1.2.1): the preamble is four
// 0.5 µs pulses starting at 0, 1.0, 3.5 and 4.5 µs; the data block starts at
// 8 µs with one bit per microsecond in pulse-position modulation (a pulse in
// the first half of the bit period is a 1, in the second half a 0).
// Everything below counts in 0.5 µs "chips".
const (
	preambleChips = 16
	maxBits       = 112
	totalChips    = preambleChips + 2*maxBits
	chipSeconds   = 0.5e-6
	phases        = 5 // sub-sample start hypotheses per sample
)

var (
	preambleHigh = [4]int{0, 2, 7, 9}
	preambleLow  = [12]int{1, 3, 4, 5, 6, 8, 10, 11, 12, 13, 14, 15}
)

// DemodConfig configures a Demodulator. The zero value is the 2.4 MS/s
// configuration with the defaults documented on each field.
type DemodConfig struct {
	// SampleRate is the complex sample rate in Hz (default 2.4e6). 2.0-3.2 MS/s
	// are supported (the RTL2832U's useful range); the frame timing is derived
	// from it, so it must be the rate the device really delivers.
	SampleRate float64
	// ScreenDB is the preamble detection threshold: the weakest preamble
	// energy (the sum of the two samples it falls on) must exceed the noise
	// by this many dB in amplitude (default 3). Lower values find weaker
	// frames and cost more CPU; the parity check rejects the extra noise.
	ScreenDB float64
	// NoFix disables the single-bit repair of DF11/17/18 frames.
	NoFix bool
	// FixWeakBits limits the repair to a bit that is among this many least
	// certain bits of the frame (default 8). This keeps the repair from
	// turning noise into a valid-looking frame.
	FixWeakBits int
	// AcceptUnverifiedAP passes address/parity replies (DF0/4/5/16/20/21/24)
	// and the uncheckable DF19/22 even when their address was not recently
	// confirmed by a DF11/17/18 frame. Off by default: with it on, noise
	// produces random-address replies that the tracker has to reject.
	AcceptUnverifiedAP bool
	// AddrTTL is how long a confirmed address validates address/parity
	// replies (default 60 s of stream time).
	AddrTTL time.Duration
}

func (c DemodConfig) withDefaults() DemodConfig {
	if c.SampleRate == 0 {
		c.SampleRate = 2.4e6
	}
	if c.ScreenDB == 0 {
		c.ScreenDB = 3
	}
	if c.FixWeakBits == 0 {
		c.FixWeakBits = 8
	}
	if c.AddrTTL == 0 {
		c.AddrTTL = 60 * time.Second
	}
	return c
}

// Validate reports a configuration the demodulator cannot run.
func (c DemodConfig) Validate() error {
	c = c.withDefaults()
	if c.SampleRate < 2e6 || c.SampleRate > 3.2e6 {
		return errors.New("sdr: sample rate must be 2.0-3.2 MS/s")
	}
	if c.ScreenDB < 1 || c.ScreenDB > 30 {
		return errors.New("sdr: ScreenDB must be 1-30 dB")
	}
	if c.FixWeakBits < 1 || c.FixWeakBits > 112 {
		return errors.New("sdr: FixWeakBits must be 1-112")
	}
	return nil
}

// Frame is one demodulated, parity-checked Mode S frame.
type Frame struct {
	Data [14]byte
	Len  int // 7 or 14
	// Ticks is the 12 MHz timestamp of the leading edge of the first
	// preamble pulse, counted from the first sample the demodulator saw
	// since New / Reset (sample index × 12e6 / SampleRate, refined to a
	// fraction of a sample).
	Ticks uint64
	// Sample is the same instant as a fractional sample index.
	Sample float64
	// SignalDBFS is the mean pulse amplitude in dB relative to the 8-bit
	// full scale (0 dBFS = an I/Q vector of length 128).
	SignalDBFS float64
	// SNRDB is the mean pulse amplitude over the mean noise magnitude, dB.
	SNRDB float64
	// Corrected is the number of bits repaired (0 or 1).
	Corrected int
	// Unverified marks an address/parity reply passed by
	// AcceptUnverifiedAP whose address was not confirmed.
	Unverified bool
}

// Bytes returns the frame's data bytes.
func (f *Frame) Bytes() []byte { return f.Data[:f.Len] }

// DF returns the downlink format (24 for all DF24 variants).
func (f *Frame) DF() int {
	df := int(f.Data[0] >> 3)
	if df >= 24 {
		return 24
	}
	return df
}

// DemodStats are cumulative demodulator counters.
type DemodStats struct {
	Samples    uint64     // complex samples processed
	Clipped    uint64     // samples with I or Q at 0 or 255 (reduce gain if high)
	Candidates uint64     // preamble screen hits that were examined
	Frames     uint64     // frames emitted
	Corrected  uint64     // of which with one repaired bit
	BadParity  uint64     // candidates whose DF11/17/18 decode failed the parity check
	UnknownAP  uint64     // candidates decoding to a reply from an unconfirmed address
	Unclean    uint64     // candidates decoding to an unverifiable reply with uncertain bits
	ByDF       [25]uint64 // frames emitted per downlink format (index 24 = DF24)
	// NoiseDBFS is the current mean noise magnitude in dBFS.
	NoiseDBFS float64
	// Spread is the learned pulse spread β of the front end (0 = an ideal
	// integrate-and-dump receiver; see viterbi.go).
	Spread float64
}

// Demodulator turns a stream of 8-bit unsigned interleaved I/Q samples (the
// rtl_tcp / RTL2832U format) into Mode S frames. It is not safe for
// concurrent use; Process may be called with buffers of any size, including
// odd byte counts.
type Demodulator struct {
	cfg        DemodConfig
	fs         float64
	chip       float64 // samples per chip
	ticksPerSm float64

	// chip integration tables, per sub-sample phase and chip: the first
	// sample offset relative to the hypothesis' base sample and up to three
	// overlap weights.
	off [phases][totalChips]int32
	w   [phases][totalChips][3]float32

	// screen geometry (sample offsets from the candidate sample)
	pulseAt [4]int
	quiet   []int

	need int // samples needed beyond the candidate sample

	mag     []uint16 // magnitudes; mag[0] is absolute sample base
	base    uint64
	pending int  // a leftover I byte, -1 when none
	dropOne bool // the next byte belongs to a lost sample
	scanAbs uint64

	noise   float64 // mean noise magnitude, LUT units
	noiseOK bool
	hist    [(46200 >> histShift) + 1]uint32
	histN   int
	reject  int // reason the current candidate's best hypothesis failed
	screenK float64

	addrs *addrTable
	stats DemodStats

	// scratch
	hm      [3 * phases]float64
	order   [3 * phases]int
	conf    [maxBits]float64
	tab     [phases]phaseTable
	br      [maxBits][npat]float32 // Viterbi branch costs [bit][pattern]
	costAt  [maxBits][4]float32    // Viterbi state costs after each bit
	back    [maxBits][4]uint8      // Viterbi back pointers
	vBase   int                    // hypothesis of the running decode
	vPh     int
	vDone   int
	vAmp    float64
	beta    float64 // stream estimate of the pulse spread (see viterbi.go)
	spreadN uint64
	pred    [predN + 1]float32 // per-slice prediction by coverage
	covBuf  []float64
	bits    [14]byte
	frame   Frame
}

// magLUT maps (I<<8 | Q) to the vector length × 256.
var magLUT = func() *[65536]uint16 {
	t := new([65536]uint16)
	for i := 0; i < 256; i++ {
		for q := 0; q < 256; q++ {
			fi, fq := float64(i)-127.5, float64(q)-127.5
			t[i<<8|q] = uint16(math.Round(math.Sqrt(fi*fi+fq*fq) * 256))
		}
	}
	return t
}()

// lutFullScale is the LUT value of a 0 dBFS amplitude (vector length 128).
const lutFullScale = 128 * 256

// NewDemodulator returns a demodulator. It panics on an invalid
// configuration; call cfg.Validate first for user input.
func NewDemodulator(cfg DemodConfig) *Demodulator {
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	cfg = cfg.withDefaults()
	d := &Demodulator{cfg: cfg, fs: cfg.SampleRate, pending: -1}
	d.chip = d.fs * chipSeconds
	d.ticksPerSm = TicksPerSecond / d.fs
	for ph := 0; ph < phases; ph++ {
		phi := float64(ph) / phases
		for k := 0; k < totalChips; k++ {
			a := phi + float64(k)*d.chip
			b := a + d.chip
			first := int(math.Floor(a))
			d.off[ph][k] = int32(first)
			for m := 0; m < 3; m++ {
				lo := math.Max(a, float64(first+m))
				hi := math.Min(b, float64(first+m+1))
				if hi > lo {
					d.w[ph][k][m] = float32((hi - lo) / d.chip)
				}
			}
		}
	}
	for i, k := range preambleHigh {
		d.pulseAt[i] = int(math.Floor(float64(k) * d.chip))
	}
	// quiet samples: wholly inside the gaps (chips 3-6 and 10-15) for any
	// start phase in [0, 1) sample
	for _, g := range [][2]int{{3, 7}, {10, 16}} {
		lo := int(math.Ceil(float64(g[0])*d.chip + 1))
		hi := int(math.Floor(float64(g[1])*d.chip)) - 1
		for i := lo; i <= hi; i++ {
			d.quiet = append(d.quiet, i)
		}
	}
	// hypotheses reach from candidate-1 to candidate+1+(phases-1)/phases
	d.need = 2 + int(math.Ceil(float64(totalChips)*d.chip)) + 3
	d.buildTables()
	for ph := range d.tab {
		d.tab[ph].xBeta = -1
	}
	for ph := range d.tab {
		// decoding reaches base p+1, the timing fit one sample further
		if n := 3 + int(d.tab[ph].last) + 1; n > d.need {
			d.need = n
		}
	}
	d.screenK = math.Pow(10, cfg.ScreenDB/20)
	d.addrs = newAddrTable(uint64(cfg.AddrTTL.Seconds() * TicksPerSecond))
	return d
}

// Config returns the effective configuration.
func (d *Demodulator) Config() DemodConfig { return d.cfg }

// Stats returns the cumulative counters.
func (d *Demodulator) Stats() DemodStats {
	s := d.stats
	s.NoiseDBFS = math.Inf(-1)
	s.Spread = d.beta
	if d.noise > 0 {
		s.NoiseDBFS = 20 * math.Log10(d.noise/lutFullScale)
	}
	return s
}

// SampleCount returns the number of complex samples consumed since New or
// Reset (the timestamp base of the next sample).
func (d *Demodulator) SampleCount() uint64 { return d.base + uint64(len(d.mag)) }

// Reset starts a new timestamp epoch: the sample counter restarts at zero,
// buffered samples and the confirmed-address table are dropped. Counters
// are kept.
func (d *Demodulator) Reset() {
	d.mag = d.mag[:0]
	d.base = 0
	d.scanAbs = 0
	d.addrs.reset()
}

// ResetStream is Reset for a new byte stream (a new connection): it also
// forgets a half-received I/Q pair.
func (d *Demodulator) ResetStream() {
	d.Reset()
	d.pending = -1
	d.dropOne = false
}

// Process demodulates iq (interleaved unsigned 8-bit I, Q) and calls emit for
// every valid frame in stream order. The *Frame passed to emit is reused
// after emit returns; copy it to keep it.
func (d *Demodulator) Process(iq []byte, emit func(*Frame)) {
	start := len(d.mag)
	if d.dropOne && len(iq) > 0 {
		iq = iq[1:] // the Q byte of a sample whose I byte was lost
		d.dropOne = false
	}
	if d.pending >= 0 && len(iq) > 0 {
		d.appendSample(byte(d.pending), iq[0])
		iq = iq[1:]
		d.pending = -1
	}
	n := len(iq) &^ 1
	if cap(d.mag)-len(d.mag) < n/2 {
		nm := make([]uint16, len(d.mag), len(d.mag)+n/2+d.need+4096)
		copy(nm, d.mag)
		d.mag = nm
	}
	var clipped uint64
	for i := 0; i < n; i += 2 {
		I, Q := iq[i], iq[i+1]
		if I == 0 || I == 255 || Q == 0 || Q == 255 {
			clipped++
		}
		d.mag = append(d.mag, magLUT[uint16(I)<<8|uint16(Q)])
	}
	if n < len(iq) {
		d.pending = int(iq[n])
	}
	d.stats.Clipped += clipped
	added := len(d.mag) - start
	d.stats.Samples += uint64(added)
	d.updateNoise(d.mag[start:])
	d.scan(emit)
}

func (d *Demodulator) appendSample(I, Q byte) {
	if I == 0 || I == 255 || Q == 0 || Q == 255 {
		d.stats.Clipped++
	}
	d.mag = append(d.mag, magLUT[uint16(I)<<8|uint16(Q)])
}

// updateNoise tracks the mean noise magnitude of the stream. It must not be
// pulled up by the signals themselves (a busy channel, a strong nearby
// transmitter), so it is derived from the 25th percentile of the magnitudes
// rather than their mean: for complex Gaussian noise the magnitude is
// Rayleigh distributed with q25 = sigma*sqrt(-2 ln 0.75) and mean = sigma*sqrt(pi/2),
// so mean = q25 * 1.6524. The quartile stays a noise statistic as long as
// three quarters of the samples are noise, and inside a Mode S frame half the
// chips are empty anyway. Blocks of 64k samples; time constant ~0.5 s.
func (d *Demodulator) updateNoise(m []uint16) {
	for len(m) > 0 {
		n := len(m)
		if room := noiseBlock - d.histN; n > room {
			n = room
		}
		for i := 0; i < n; i += 2 { // every other sample is plenty
			d.hist[m[i]>>histShift]++
		}
		d.histN += n
		m = m[n:]
		if d.histN < noiseBlock {
			break
		}
		total := uint32(0)
		for _, c := range d.hist {
			total += c
		}
		target, acc := total/4, uint32(0)
		q := 0
		for i, c := range d.hist {
			acc += c
			if acc > target {
				q = i
				break
			}
		}
		est := (float64(q) + 0.5) * (1 << histShift) * 1.6524
		if !d.noiseOK {
			d.noise, d.noiseOK = est, true
		} else {
			a := noiseBlock / (0.5 * d.fs)
			d.noise += (est - d.noise) * a
		}
		d.hist = [len(d.hist)]uint32{}
		d.histN = 0
	}
	if !d.noiseOK && d.histN > 0 {
		// before the first full block: a provisional estimate from what
		// has been seen so far
		total := uint32(0)
		for _, c := range d.hist {
			total += c
		}
		target, acc := total/4, uint32(0)
		for i, c := range d.hist {
			acc += c
			if acc > target {
				d.noise = (float64(i) + 0.5) * (1 << histShift) * 1.6524
				break
			}
		}
	}
}

const (
	noiseBlock = 65536
	histShift  = 6 // bins of 0.25 LSB
)

func (d *Demodulator) scan(emit func(*Frame)) {
	m := d.mag
	last := len(m) - d.need // last candidate index with enough samples
	p := 2
	if d.scanAbs > d.base+2 {
		p = int(d.scanAbs - d.base)
	}
	noiseFloor := 2 * d.noise * d.screenK // threshold for a two-sample pulse sum
	if noiseFloor < 512 {                 // never below one LSB per sample
		noiseFloor = 512
	}
	for ; p <= last; p++ {
		if !d.screenPass(p, noiseFloor) {
			continue
		}
		d.stats.Candidates++
		if end, ok := d.decodeAt(p, emit); ok {
			p = end - 1
		}
	}
	if p < 2 {
		p = 2
	}
	d.scanAbs = d.base + uint64(p)
	// keep the unscanned tail plus two look-behind samples
	keep := p - 2
	if keep > 0 {
		n := copy(m, m[keep:])
		d.mag = m[:n]
		d.base += uint64(keep)
	}
}

// chipE returns the mean magnitude of chip k for the hypothesis starting at
// sample base+phase/phases.
func (d *Demodulator) chipE(base, ph, k int) float64 {
	o := base + int(d.off[ph][k])
	w := &d.w[ph][k]
	m := d.mag
	return float64(w[0])*float64(m[o]) + float64(w[1])*float64(m[o+1]) + float64(w[2])*float64(m[o+2])
}

// metric scores one start hypothesis: preamble contrast plus the eye
// opening of the first 56 data bits (each bit's two chips should differ
// strongly). The eye term dominates and pins the timing far better than the
// four preamble pulses alone.
func (d *Demodulator) metric(base, ph int) float64 {
	var hi, lo float64
	for _, k := range preambleHigh {
		hi += d.chipE(base, ph, k)
	}
	for _, k := range preambleLow {
		lo += d.chipE(base, ph, k)
	}
	s := hi - lo/3
	for b := 0; b < 56; b++ {
		a := d.chipE(base, ph, preambleChips+2*b)
		c := d.chipE(base, ph, preambleChips+2*b+1)
		if a > c {
			s += a - c
		} else {
			s += c - a
		}
	}
	return s
}

// decodeAt examines the start hypotheses around candidate sample p and emits
// the first one that yields a valid frame. It returns the sample index just
// after the frame.
func (d *Demodulator) decodeAt(p int, emit func(*Frame)) (int, bool) {
	const nh = 3 * phases
	for i := 0; i < nh; i++ {
		base := p - 1 + i/phases
		d.hm[i] = d.metric(base, i%phases)
		d.order[i] = i
	}
	ord := d.order[:]
	for i := 1; i < nh; i++ { // insertion sort, best metric first (no allocation)
		for j := i; j > 0 && d.hm[ord[j]] > d.hm[ord[j-1]]; j-- {
			ord[j], ord[j-1] = ord[j-1], ord[j]
		}
	}
	// Three hypotheses are usually enough; a strong preamble (12 dB over
	// the noise) earns a search of all of them, because at some sample
	// rate / phase combinations (2.0 MS/s with a half-sample offset) the
	// eye metric peaks half a chip away from the true timing.
	maxTries := 3
	if best := ord[0]; d.preambleAmp(p-1+best/phases, best%phases) > 4*d.noise {
		maxTries = nh
	}
	tried := 0
	d.reject = rejectNone
	var prevBits [14]byte
	for _, h := range ord {
		if tried == maxTries || d.hm[h] <= 0 {
			break
		}
		base, ph := p-1+h/phases, h%phases
		// the format comes from a 56-bit decode: the sequence detector needs
		// the bits that follow to settle the first five
		d.vitStart(base, ph)
		d.vitRun(56)
		pulse := d.vitDecide(56)
		df := int(d.bits[0] >> 3)
		nbits := frameBits(df)
		if nbits == 0 {
			tried++
			continue
		}
		if nbits == 112 {
			d.vitRun(112)
			pulse = d.vitDecide(112)
		}
		if tried > 0 && d.bits == prevBits {
			continue // same bits as a hypothesis already rejected
		}
		prevBits = d.bits
		tried++
		fixed, unverified, ok := d.check(df, nbits, pulse)
		if !ok {
			continue
		}
		smp := float64(d.base) + d.refine((p-1)*phases+h, nbits, pulse)
		f := &d.frame
		*f = Frame{Len: nbits / 8, Sample: smp, Corrected: fixed, Unverified: unverified}
		copy(f.Data[:], d.bits[:nbits/8])
		f.Ticks = uint64(math.Round(smp * d.ticksPerSm))
		f.SignalDBFS = 20 * math.Log10(pulse/lutFullScale)
		if d.noise > 0 {
			f.SNRDB = 20 * math.Log10(pulse/d.noise)
		}
		dfIdx := df
		if dfIdx > 24 {
			dfIdx = 24
		}
		d.stats.Frames++
		d.stats.ByDF[dfIdx]++
		if fixed > 0 {
			d.stats.Corrected++
		}
		d.learnSpread(base, ph, f.SNRDB)
		emit(f)
		end := int(math.Ceil(float64(base) + float64(ph)/phases + float64(preambleChips+2*nbits)*d.chip))
		return end, true
	}
	// counted per examined candidate: one transmission that fails is
	// usually seen by two or three neighbouring candidate samples
	switch d.reject {
	case rejectParity:
		d.stats.BadParity++
	case rejectAP:
		d.stats.UnknownAP++
	case rejectUnclean:
		d.stats.Unclean++
	}
	return 0, false
}

const (
	rejectNone   = iota
	rejectParity // ordered by how much the candidate looked like a frame
	rejectUnclean
	rejectAP
)

// check validates d.bits as a frame of nbits bits with downlink format df,
// repairing one weak bit of a DF11/17/18 frame when allowed.
func (d *Demodulator) check(df, nbits int, amp float64) (fixed int, unverified bool, ok bool) {
	data := d.bits[:nbits/8]
	r := modes.Residual(data)
	now := uint64(0)
	if d.fs > 0 {
		now = uint64(float64(d.base) * d.ticksPerSm)
	}
	switch parityRule(df) {
	case parityPI:
		// DF11 replies to an interrogator carry its code in the low seven
		// parity bits, so any damage confined to those bits also looks like
		// a valid reply; accept a non-zero code only from a clean decode.
		good := r == 0 || (df == 11 && r&^0x7F == 0 && d.clean(nbits, amp))
		if !good && !d.cfg.NoFix && (df == 17 || df == 18) {
			if bit, found := syndromeBit(r, nbits); found && d.isWeak(bit, nbits) && d.weakBits(nbits, amp) <= fixMaxWeak {
				data[bit>>3] ^= 0x80 >> uint(bit&7)
				fixed, good = 1, true
			}
		}
		if !good {
			d.setReject(rejectParity)
			return 0, false, false
		}
		addr := uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
		if df == 17 || df == 11 || (df == 18 && data[0]&7 == 0) {
			d.addrs.add(addr, now)
		}
		return fixed, false, true
	case parityAP:
		// An address/parity reply cannot prove its own integrity: any bit
		// error turns it into a reply from another address. Accept it only
		// when the demodulation is clean (at most apMaxWeak bits with less
		// than a quarter of the pulse amplitude between their two chips),
		// otherwise noise and garbled frames whose residual happens to hit
		// one of the hundreds of known addresses would get through.
		if !d.clean(nbits, amp) {
			d.setReject(rejectUnclean)
			return 0, false, false
		}
		if d.addrs.known(r, now) {
			return 0, false, true
		}
	case parityNone:
		if !d.clean(nbits, amp) {
			d.setReject(rejectUnclean)
			return 0, false, false
		}
	default:
		return 0, false, false
	}
	if d.cfg.AcceptUnverifiedAP {
		return 0, true, true
	}
	d.setReject(rejectAP)
	return 0, false, false
}

// isWeak reports whether bit is among the FixWeakBits least certain bits.
func (d *Demodulator) isWeak(bit, nbits int) bool {
	c := d.conf[bit]
	worse := 0
	for i := 0; i < nbits; i++ {
		if d.conf[i] < c {
			worse++
			if worse >= d.cfg.FixWeakBits {
				return false
			}
		}
	}
	return true
}

// apMaxWeak is the number of uncertain bits an unverifiable frame may have.
const apMaxWeak = 2

// clean reports whether at most apMaxWeak bits have a chip difference below
// a quarter of the pulse amplitude.
func (d *Demodulator) clean(nbits int, amp float64) bool {
	lim := 0.25 * amp
	weak := 0
	for i := 0; i < nbits; i++ {
		if d.conf[i] < lim {
			weak++
			if weak > apMaxWeak {
				return false
			}
		}
	}
	return true
}

// fixMaxWeak is the most uncertain bits a frame may have for the single-bit
// repair: a frame with many marginal bits more likely has several errors,
// and a residual that happens to match one single-bit pattern is then a
// coincidence, not a repair.
const fixMaxWeak = 16

// weakBits counts the bits whose confidence is below a quarter of the pulse
// amplitude.
func (d *Demodulator) weakBits(nbits int, amp float64) int {
	lim := 0.25 * amp
	n := 0
	for i := 0; i < nbits; i++ {
		if d.conf[i] < lim {
			n++
		}
	}
	return n
}

// setReject records why the current candidate failed, keeping the reason of
// the hypothesis that came closest to a frame.
func (d *Demodulator) setReject(r int) {
	if r > d.reject {
		d.reject = r
	}
}

// SkipBytes accounts for n bytes of the stream that were lost (discarded by
// the reader because the demodulator fell behind). The sample counter, and
// so every later timestamp, advances as if they had been processed; frames
// overlapping the gap are lost.
func (d *Demodulator) SkipBytes(n int) {
	if n <= 0 {
		return
	}
	if d.dropOne { // the first lost byte is the Q we were going to drop
		d.dropOne = false
		n--
	}
	held := 0
	if d.pending >= 0 {
		held, d.pending = 1, -1
	}
	total := held + n
	samples := uint64(total / 2)
	if total%2 == 1 {
		samples++ // its Q byte is the next byte to arrive
		d.dropOne = true
	}
	d.base += uint64(len(d.mag)) + samples
	d.mag = d.mag[:0]
	d.scanAbs = 0
	d.stats.Samples += samples
}

// screenPass is the cheap preamble screen at candidate sample p: every
// preamble pulse well above the noise (floor, for the sum of the two samples
// a 0.5 µs pulse can fall on, so the test does not depend on the sub-sample
// phase) and above the quiet parts of the preamble.
func (d *Demodulator) screenPass(p int, floor float64) bool {
	m, pa := d.mag, d.pulseAt
	h := uint32(m[p+pa[0]]) + uint32(m[p+pa[0]+1])
	if float64(h) < floor {
		return false
	}
	for _, o := range pa[1:] {
		if v := uint32(m[p+o]) + uint32(m[p+o+1]); v < h {
			h = v
		}
	}
	if float64(h) < floor {
		return false
	}
	var q uint32
	for _, o := range d.quiet {
		q += uint32(m[p+o])
	}
	// pulse-pair mean above 1.2 × the mean quiet sample
	return float64(h)*float64(len(d.quiet)) >= 2.4*float64(q)
}
