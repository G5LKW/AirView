package sdr

import "math"

// Sample-level frame model: bit decisions (Viterbi), amplitude and
// inter-sample spread estimate, and timing fit.
//
// Each sample's magnitude is predicted from the fraction of its sample
// period that is covered by pulses ("coverage", what an ideal integrate-and-
// dump receiver would see), spread over the neighbouring samples by a short
// symmetric kernel [β, 1-2β, β] that stands for the front end's band limit:
//
//	pred_i = sqrt( (A · (β·c_{i-1} + (1-2β)·c_i + β·c_{i+1}))² + n² )
//
// with A the pulse amplitude, n the mean noise magnitude (a simple stand-in
// for the Rician mean) and c the coverages. A and β are fitted to the
// preamble of every frame, so a dongle whose filter smears the pulses more
// (β up to 0.3) is decoded with the matching model. All coverages depend
// only on the sub-sample phase of the frame start; they are tabulated once
// per phase in NewDemodulator, and the per-frame work is multiply-adds and
// one square root per prediction.

// maxBeta bounds the fitted spread.
const maxBeta = 0.3

// npat is the number of bit patterns (x[b-2], x[b-1], x[b]) a sample's
// prediction can depend on.
const npat = 8

// vEnt is one sample's share in the decision of the bit its spread window
// [i-1, i+2) ends in: the coverage of samples i-1, i and i+1 for each
// pattern x[b-2]<<2 | x[b-1]<<1 | x[b] (bits before the data block and after
// the frame are empty).
type vEnt struct {
	off int32
	c   [3][npat]float32
	xi  [npat]uint8 // (β·c[0] + (1-2β)·c[1] + β·c[2])·predN for the table's β
}

// preEnt is a preamble sample and the pulse coverage of it and its two
// neighbours.
type preEnt struct {
	off int32
	c   [3]float32
}

// fEnt is a sample of the whole frame with its overlap with up to three
// consecutive chips starting at chip k0 (-1 = the empty chip before the
// preamble).
type fEnt struct {
	off int32
	k0  int16
	w   [3]float32
}

type phaseTable struct {
	pre  []preEnt
	vit  []vEnt
	vIdx [maxBits + 1]int32 // vit[vIdx[b]:vIdx[b+1]] belong to bit b
	// extra[b] are the samples whose window ends within 2 samples after bit
	// b (owned by bit b when it is the last bit of the frame)
	extra [maxBits][]vEnt
	fit   []fEnt
	fitN  [2]int  // entries covering a 56- and a 112-bit frame
	first int32   // smallest sample offset any table uses (may be -1)
	last  int32   // largest sample offset any table uses
	xBeta float64 // the β the x fields were computed for (-1 = none)
}

// betaStep quantises β for the combined coverage tables, so that the slowly
// moving stream estimate does not rebuild them for every frame.
const betaStep = 0.005

// ensureX makes the combined coverages of tb match beta (quantised).
func (tb *phaseTable) ensureX(beta float64) float64 {
	q := math.Round(beta/betaStep) * betaStep
	if q == tb.xBeta {
		return q
	}
	tb.xBeta = q
	w0, w1 := float32(q), float32(1-2*q)
	set := func(es []vEnt) {
		for k := range es {
			e := &es[k]
			for p := 0; p < npat; p++ {
				x := w0*e.c[0][p] + w1*e.c[1][p] + w0*e.c[2][p]
				e.xi[p] = uint8(math.Min(predN, math.Max(0, math.Round(float64(x)*predN))))
			}
		}
	}
	set(tb.vit)
	for b := range tb.extra {
		set(tb.extra[b])
	}
	return q
}

// predN is the resolution of the per-frame prediction table: predictions
// are looked up for the coverage rounded to 1/predN (an error of at most
// A/(2·predN), far below the noise).
const predN = 255

func (d *Demodulator) buildTables() {
	c := d.chip
	for ph := 0; ph < phases; ph++ {
		tb := &d.tab[ph]
		t0 := float64(ph) / phases
		upd := func(i int) {
			if int32(i) > tb.last {
				tb.last = int32(i)
			}
			if int32(i) < tb.first {
				tb.first = int32(i)
			}
		}
		chipCov := func(i int, k int) float64 { // coverage of sample i by chip k
			lo := t0 + float64(k)*c
			return overlap(float64(i), float64(i+1), lo, lo+c)
		}
		// preamble
		preCov := func(i int) float64 {
			var cov float64
			for _, k := range preambleHigh {
				cov += chipCov(i, k)
			}
			return cov
		}
		for i := int(math.Floor(t0)) - 1; i <= int(math.Ceil(t0+preambleChips*c)); i++ {
			e := preEnt{off: int32(i)}
			for j := 0; j < 3; j++ {
				e.c[j] = float32(preCov(i - 1 + j))
			}
			if e.c[0] > 0 || e.c[1] > 0 || e.c[2] > 0 {
				tb.pre = append(tb.pre, e)
				upd(i - 1)
				upd(i + 1)
			}
		}
		// data: coverage of sample i under a pattern for bits b-2..b
		dataStart := t0 + preambleChips*c
		patCov := func(i, b, pat int) float64 {
			var cov float64
			for k := 0; k < 3; k++ { // bit b-2+k
				bb := b - 2 + k
				if bb < 0 || bb >= maxBits {
					continue
				}
				x := pat >> uint(2-k) & 1
				chip := preambleChips + 2*bb
				if x == 0 {
					chip++
				}
				cov += chipCov(i, chip)
			}
			return cov
		}
		ent := func(i, b int) vEnt {
			e := vEnt{off: int32(i)}
			for j := 0; j < 3; j++ {
				for pat := 0; pat < npat; pat++ {
					e.c[j][pat] = float32(patCov(i-1+j, b, pat))
				}
			}
			upd(i - 1)
			upd(i + 1)
			return e
		}
		// sample i belongs to the bit its window end i+2 falls in
		i := int(math.Floor(dataStart)) - 2
		for b := 0; b < maxBits; b++ {
			bitStart := dataStart + float64(2*b)*c
			bitEnd := bitStart + 2*c
			tb.vIdx[b] = int32(len(tb.vit))
			for ; float64(i+2) <= bitEnd; i++ {
				if float64(i+2) <= bitStart {
					continue
				}
				tb.vit = append(tb.vit, ent(i, b))
			}
			// windows ending up to 2 samples after the bit: with bit b as
			// the last one, only bits b-1 and b can reach them
			for j := i; float64(j+2) <= bitEnd+2; j++ {
				tb.extra[b] = append(tb.extra[b], ent(j, b))
			}
		}
		tb.vIdx[maxBits] = int32(len(tb.vit))
		// whole-frame fit (one extra sample each side for the spread)
		for i := int(math.Floor(t0)) - 1; i <= int(math.Ceil(t0+totalChips*c))+1; i++ {
			s, e := float64(i), float64(i+1)
			k0 := int(math.Floor((s - t0) / c))
			fe := fEnt{off: int32(i), k0: int16(k0)}
			for j := 0; j < 3; j++ {
				lo := t0 + float64(k0+j)*c
				fe.w[j] = float32(overlap(s, e, lo, lo+c))
			}
			tb.fit = append(tb.fit, fe)
			upd(i)
			for n, nb := range [2]int{56, 112} {
				if i <= int(math.Ceil(t0+float64(preambleChips+2*nb)*c))+1 {
					tb.fitN[n] = len(tb.fit)
				}
			}
		}
	}
}

// overlap returns the length of [a, b) ∩ [lo, hi).
func overlap(a, b, lo, hi float64) float64 {
	if lo > a {
		a = lo
	}
	if hi < b {
		b = hi
	}
	if b > a {
		return b - a
	}
	return 0
}

// preambleFit estimates the pulse amplitude A (LUT units) and the spread β
// of the hypothesis (base, ph) by least squares over the preamble samples:
// m_i ≈ A·c_i + Aβ·(c_{i-1} + c_{i+1} - 2·c_i), linear in (A, Aβ).
func (d *Demodulator) preambleFit(base, ph int) (amp, beta float64) {
	var s11, s12, s22, r1, r2 float64
	m := d.mag
	for _, e := range d.tab[ph].pre {
		if m[base+int(e.off)] > clipMag {
			continue // possibly clipped: the edges still carry the spread
		}
		x1 := float64(e.c[1])
		x2 := float64(e.c[0]) + float64(e.c[2]) - 2*x1
		y := float64(m[base+int(e.off)])
		s11 += x1 * x1
		s12 += x1 * x2
		s22 += x2 * x2
		r1 += x1 * y
		r2 += x2 * y
	}
	det := s11*s22 - s12*s12
	if s11 == 0 {
		return 0, 0
	}
	if det <= 1e-9*s11*s22 {
		return r1 / s11, 0
	}
	u := (r1*s22 - r2*s12) / det
	v := (s11*r2 - s12*r1) / det
	if u <= 0 {
		return r1 / s11, 0
	}
	beta = v / u
	if beta < 0 || math.IsNaN(beta) {
		beta = 0
	} else if beta > maxBeta {
		beta = maxBeta
	}
	// amplitude refit with β fixed
	var num, den float64
	for _, e := range d.tab[ph].pre {
		x := beta*float64(e.c[0]) + (1-2*beta)*float64(e.c[1]) + beta*float64(e.c[2])
		num += x * float64(m[base+int(e.off)])
		den += x * x
	}
	if den == 0 {
		return 0, beta
	}
	return num / den, beta
}

// preambleAmp estimates the pulse amplitude of the hypothesis (base, ph)
// from the preamble with the stream's spread d.beta.
func (d *Demodulator) preambleAmp(base, ph int) float64 {
	beta := d.beta
	var num, den float64
	m := d.mag
	for _, e := range d.tab[ph].pre {
		x := beta*float64(e.c[0]) + (1-2*beta)*float64(e.c[1]) + beta*float64(e.c[2])
		num += x * float64(m[base+int(e.off)])
		den += x * x
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// learnSpread updates the stream's spread estimate from the preamble of an
// accepted strong frame. The spread is a property of the receiver's filter,
// not of the frame, so it is averaged over many frames (a per-frame fit
// from four pulses is too noisy to decode with, and clipped pulses would
// distort it).
func (d *Demodulator) learnSpread(base, ph int, snrDB float64) {
	if snrDB < 15 {
		return
	}
	n := 0
	for _, e := range d.tab[ph].pre {
		if d.mag[base+int(e.off)] <= clipMag && (e.c[0] != e.c[1] || e.c[1] != e.c[2]) {
			n++
		}
	}
	if n < 8 {
		return
	}
	_, b := d.preambleFit(base, ph)
	d.beta += (b - d.beta) * 0.05
	d.spreadN++
}

// clipMag is a magnitude (LUT units) above which a sample may be clipped:
// an I/Q vector longer than 120 LSB can have a component at the rail.
const clipMag = 120 * 256

// addBranch accumulates the squared prediction errors of samples ents into
// br (one per pattern), with predictions from d.pred.
func (d *Demodulator) addBranch(br *[npat]float32, ents []vEnt, base int) {
	m := d.mag
	pred := &d.pred
	for k := range ents {
		e := &ents[k]
		v := float32(m[base+int(e.off)])
		for pat := 0; pat < npat; pat++ {
			r := v - pred[e.xi[pat]]
			br[pat] += r * r
		}
	}
}

// The bits are found by a Viterbi search over the sample model rather than
// by comparing the two chips of each bit in isolation. At 2.4 MS/s a chip is
// 1.2 samples, so most samples straddle two chips and carry part of the
// neighbouring bit's pulse, and the front end's band limit spreads every
// pulse further; comparing chip energies alone loses much of the eye
// opening to that inter-symbol interference. A sample's prediction depends
// on at most three consecutive bits, so the state is the last two bits
// (four states, x[b]<<1 | x[b-1]) and each bit adds the squared errors of
// the samples whose spread window ends in it. The forward pass is
// resumable: a candidate is first decided as a 56-bit frame, and only when
// its format says 112 bits does the pass continue.

// vitStart begins a sequence decode of the hypothesis (base, ph) and
// returns the pulse amplitude estimate.
func (d *Demodulator) vitStart(base, ph int) float64 {
	tb := &d.tab[ph]
	tb.ensureX(d.beta)
	amp := d.preambleAmp(base, ph)
	a2 := amp * amp
	n0sq := d.noise * d.noise
	for i := range d.pred {
		x := float64(i) / predN
		d.pred[i] = float32(math.Sqrt(a2*x*x + n0sq))
	}
	d.vBase, d.vPh, d.vDone, d.vAmp = base, ph, 0, amp
	return amp
}

// vitRun extends the forward pass to bit to (exclusive). Before the data
// every state is equivalent (the tables treat bits before the data block as
// empty), so all start at cost 0.
func (d *Demodulator) vitRun(to int) {
	tb := &d.tab[d.vPh]
	for b := d.vDone; b < to; b++ {
		br := &d.br[b]
		*br = [npat]float32{}
		d.addBranch(br, tb.vit[tb.vIdx[b]:tb.vIdx[b+1]], d.vBase)
		var prev [4]float32
		if b > 0 {
			prev = d.costAt[b-1]
		}
		d.costAt[b], d.back[b] = step(prev, br)
	}
	if to > d.vDone {
		d.vDone = to
	}
}

// step is one Viterbi recursion: from the costs of states (x[b-1], x[b-2])
// to the states (x[b], x[b-1]) with branch costs br[x[b-2]<<2|x[b-1]<<1|x[b]].
func step(cost [4]float32, br *[npat]float32) (next [4]float32, back [4]uint8) {
	for st := 0; st < 4; st++ {
		xb, xb1 := st>>1, st&1
		c0 := cost[xb1<<1] + br[xb1<<1|xb]
		c1 := cost[xb1<<1|1] + br[4|xb1<<1|xb]
		if c0 <= c1 {
			next[st], back[st] = c0, 0
		} else {
			next[st], back[st] = c1, 1
		}
	}
	return
}

// vitDecide ends the decode as an nbits frame (vitRun(nbits) must have
// run): the last bit also owns the samples just after the frame. It fills
// d.bits and d.conf and returns the amplitude.
func (d *Demodulator) vitDecide(nbits int) float64 {
	tb := &d.tab[d.vPh]
	last := nbits - 1
	brLast := d.br[last]
	d.addBranch(&brLast, tb.extra[last], d.vBase)
	var prev [4]float32
	if last > 0 {
		prev = d.costAt[last-1]
	}
	cost, backLast := step(prev, &brLast)
	st := 0
	for s := 1; s < 4; s++ {
		if cost[s] < cost[st] {
			st = s
		}
	}
	d.bits = [14]byte{}
	for b := last; b >= 0; b-- {
		if st>>1 == 1 {
			d.bits[b>>3] |= 0x80 >> uint(b&7)
		}
		bk := d.back[b][st]
		if b == last {
			bk = backLast[st]
		}
		st = (st&1)<<1 | int(bk)
	}
	// confidence of each bit: how much the squared prediction error grows
	// when that bit alone is flipped (it changes the patterns of bits b,
	// b+1 and b+2), divided by the amplitude so that a cleanly received bit
	// scores in the order of the amplitude
	bit := func(b int) int {
		if b < 0 || b >= nbits {
			return 0
		}
		return int(d.bits[b>>3]>>uint(7-b&7)) & 1
	}
	amp := d.vAmp
	for b := 0; b < nbits; b++ {
		var delta float64
		for k := 0; k < 3 && b+k < nbits; k++ {
			p := bit(b+k-2)<<2 | bit(b+k-1)<<1 | bit(b+k)
			br := &d.br[b+k]
			if b+k == last {
				br = &brLast
			}
			delta += float64(br[p^(1<<uint(k))] - br[p])
		}
		if amp > 0 {
			d.conf[b] = delta / amp
		} else {
			d.conf[b] = 0
		}
	}
	return amp
}

// slice decodes nbits bits for the hypothesis (base, ph) into d.bits and
// d.conf and returns the pulse amplitude estimate.
func (d *Demodulator) slice(base, ph, nbits int) float64 {
	d.vitStart(base, ph)
	d.vitRun(nbits)
	return d.vitDecide(nbits)
}

// refine returns the start time (local fractional sample index) that best
// explains the samples given the decided bits: the squared error of the
// predicted magnitudes is evaluated at 1/phases sample steps within ±1
// sample of the decoding hypothesis g (in 1/phases sample units), and a
// parabola through the minimum and its neighbours gives the sub-step
// estimate. The search metric only ranks hypotheses for decoding; this fit
// uses every sample of the frame and is what the timestamp comes from.
func (d *Demodulator) refine(g, nbits int, amp float64) float64 {
	const span = phases
	var high [totalChips + 8]bool // index k+1
	for _, k := range preambleHigh {
		high[k+1] = true
	}
	for b := 0; b < nbits; b++ {
		if d.bits[b>>3]&(0x80>>uint(b&7)) != 0 {
			high[preambleChips+2*b+1] = true
		} else {
			high[preambleChips+2*b+2] = true
		}
	}
	var cost [2*span + 1]float64
	best := -1
	for j := -span; j <= span; j++ {
		gg := g + j
		cost[j+span] = math.Inf(1)
		base, ph := gg/phases, gg%phases
		if gg < 0 || base+int(d.tab[ph].first) < 0 || base+int(d.tab[ph].last) >= len(d.mag) {
			continue
		}
		cost[j+span] = d.fitCost(base, ph, nbits, amp, &high)
		if best < 0 || cost[j+span] < cost[best] {
			best = j + span
		}
	}
	if best < 0 {
		return float64(g) / phases
	}
	t := float64(g+best-span) / phases
	if best > 0 && best < 2*span && !math.IsInf(cost[best-1], 1) && !math.IsInf(cost[best+1], 1) {
		c0, c1, c2 := cost[best-1], cost[best], cost[best+1]
		if den := c0 - 2*c1 + c2; den > 0 {
			dl := 0.5 * (c0 - c2) / den
			t += math.Max(-0.5, math.Min(0.5, dl)) / phases
		}
	}
	return t
}

// fitCost is the squared error between the samples and the magnitudes
// predicted for the frame (chip pattern high, index k+1) starting at the
// hypothesis (base, ph), with the spread d.beta of the decoding hypothesis.
func (d *Demodulator) fitCost(base, ph, nbits int, amp float64, high *[totalChips + 8]bool) float64 {
	tb := &d.tab[ph]
	n := tb.fitN[0]
	if nbits == 112 {
		n = tb.fitN[1]
	}
	ents := tb.fit[:n]
	cov := d.covBuf[:0]
	for _, e := range ents {
		var c float64
		k := int(e.k0) + 1
		if k >= 0 && high[k] {
			c += float64(e.w[0])
		}
		if high[k+1] {
			c += float64(e.w[1])
		}
		if high[k+2] {
			c += float64(e.w[2])
		}
		cov = append(cov, c)
	}
	d.covBuf = cov
	beta := d.beta
	a2, n0sq := amp*amp, d.noise*d.noise
	m := d.mag
	var sum float64
	for i := 1; i+1 < len(cov); i++ {
		x := beta*cov[i-1] + (1-2*beta)*cov[i] + beta*cov[i+1]
		r := float64(m[base+int(ents[i].off)]) - math.Sqrt(a2*x*x+n0sq)
		sum += r * r
	}
	return sum
}
