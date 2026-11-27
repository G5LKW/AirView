package rfstat

import "math"

// signalAircraft updates fading, aspect and multipath statistics with one
// range-normalised signal sample resid = dBFS + 20*log10(km).
func (s *Stats) signalAircraft(ac *acState, f *Frame, t int64, resid, d float64, hasElev bool) {
	a := &s.cur
	ac.residN++
	if ac.residN == 1 {
		ac.residEWMA = float32(resid)
	} else {
		// long-run mean of the aircraft (removes transmitter power differences)
		n := float32(ac.residN)
		if n > 400 {
			n = 400
		}
		ac.residEWMA += (float32(resid) - ac.residEWMA) / n
	}
	if ac.residN > 20 {
		dev := resid - float64(ac.residEWMA)
		if finite(f.TrackDeg) && finite(f.BearingDeg) {
			// aspect: angle between the aircraft track and the direction from
			// the aircraft to the receiver (0 = nose-on, 180 = tail-on).
			asp := math.Abs(math.Mod(f.TrackDeg-(f.BearingDeg+180)+540, 360) - 180)
			b := clampInt(int(asp/10), 0, nAspect-1)
			a[oAspSum+b] += dev
			a[oAspSq+b] += dev * dev
			a[oAspN+b]++
		}
		if hasElev && f.ElevDeg >= mpLo && f.ElevDeg < mpLo+nMP*mpStep {
			b := int((f.ElevDeg - mpLo) / mpStep)
			a[oMPSum+b] += dev
			a[oMPN+b]++
		}
	}

	// fading window
	w := s.cfg.FadeWindow.Nanoseconds()
	if ac.fadeStart == 0 || t-ac.fadeStart >= w || t < ac.fadeStart {
		s.closeFade(ac)
		ac.fadeStart = t
	}
	p := math.Exp(resid * (math.Ln10 / 10))
	ac.fadeN++
	ac.fP += p
	ac.fP2 += p * p
	ac.fDb += resid
	ac.fDb2 += resid * resid
}

// riceK returns the moment-method Rician K estimate from power samples:
// gamma = Var[P]/E[P]^2 = (1+2K)/(1+K)^2  =>  K = sqrt(1-gamma)/(1-sqrt(1-gamma)).
func riceK(n, sp, sp2 float64) float64 {
	if n < 2 || sp <= 0 {
		return nan
	}
	m := sp / n
	v := (sp2 - n*m*m) / (n - 1)
	if v < 0 {
		v = 0
	}
	g := v / (m * m)
	if g >= 1 {
		return 0
	}
	r := math.Sqrt(1 - g)
	if r >= 1 {
		return 1e6
	}
	return r / (1 - r)
}

const minFadeN = 8

func (s *Stats) closeFade(ac *acState) {
	n := float64(ac.fadeN)
	if ac.fadeN >= minFadeN {
		a := &s.cur
		k := riceK(n, ac.fP, ac.fP2)
		kdb := kLo
		if k > 0.1 {
			kdb = 10 * math.Log10(k)
		}
		a[oKHist+clampInt(int(kdb-kLo), 0, nKBins-1)]++
		m := ac.fDb / n
		v := (ac.fDb2 - n*m*m) / (n - 1)
		if v < 0 {
			v = 0
		}
		sd := math.Sqrt(v)
		a[oStdHist+clampInt(int(sd/stdStep), 0, nStdBins-1)]++
		if e := float64(ac.elev); finite(e) {
			b := elevBand(e)
			a[oKElevSum+b] += math.Max(kLo, math.Min(kdb, kLo+nKBins))
			a[oKElevN+b]++
		}
		if ac.fadeWins == 0 {
			ac.kEWMA, ac.stdEWMA = float32(kdb), float32(sd)
		} else {
			ac.kEWMA += 0.2 * (float32(kdb) - ac.kEWMA)
			ac.stdEWMA += 0.2 * (float32(sd) - ac.stdEWMA)
		}
		ac.fadeWins++
	}
	ac.fadeN = 0
	ac.fP, ac.fP2, ac.fDb, ac.fDb2 = 0, 0, 0, 0
}

// pdAdvance closes every reception-probability window of ac that ended
// before t. Windows inside silent gaps count as zero receptions as long as
// the gap is shorter than PdMaxGap; the first window of a track (which
// starts on a received frame) is a warm-up and not counted.
func (s *Stats) pdAdvance(ac *acState, t int64) {
	w := s.cfg.PdWindow.Nanoseconds()
	if ac.pdStart == 0 || t < ac.pdStart {
		ac.pdStart = t
		ac.pdWarm = true
		ac.pdReset()
		return
	}
	maxGap := s.cfg.PdMaxGap.Nanoseconds()
	for t >= ac.pdStart+w {
		if !ac.pdWarm {
			s.pdClose(ac)
		}
		ac.pdWarm = false
		ac.pdReset()
		ac.pdStart += w
		if t-ac.pdStart > maxGap {
			ac.pdStart = t
			ac.pdWarm = true
			return
		}
	}
}

func (ac *acState) pdReset() {
	ac.pdCnt = [nPdTypes]uint16{}
	ac.pdRSum, ac.pdRN = 0, 0
}

func (s *Stats) pdClose(ac *acState) {
	if ac.flags&fADSB == 0 || ac.flags&fGround != 0 || ac.pdSeen == 0 {
		return
	}
	a := &s.cur
	wSec := s.cfg.PdWindow.Seconds()
	r := float64(ac.rssiEWMA)
	if ac.pdRN > 0 {
		r = float64(ac.pdRSum) / float64(ac.pdRN)
	}
	d, e, b := float64(ac.dist), float64(ac.elev), float64(ac.brg)
	di, ei, bi, ri := -1, -1, -1, -1
	if finite(d) {
		di = clampInt(int(d/pdDistKm), 0, nPdDist-1)
	}
	if finite(e) {
		ei = pdElevBin(e)
	}
	if finite(b) {
		bi = brgBin(b)
	}
	if finite(r) {
		ri = clampInt(int((r-pdRSSILo)/pdRSSIW), 0, nPdRSSI-1)
	}
	for typ := 0; typ < nPdTypes; typ++ {
		if ac.pdSeen&(1<<typ) == 0 {
			continue
		}
		exp := wSec * nominalRate[typ]
		got := float64(ac.pdCnt[typ])
		ac.lifeExp[typ] += float32(exp)
		ac.lifeGot[typ] += uint32(ac.pdCnt[typ])
		a[oPdTotExp+typ] += exp
		a[oPdTotGot+typ] += got
		if di >= 0 {
			a[oPdDistExp+typ*nPdDist+di] += exp
			a[oPdDistGot+typ*nPdDist+di] += got
		}
		if ei >= 0 {
			a[oPdElevExp+typ*nPdElev+ei] += exp
			a[oPdElevGot+typ*nPdElev+ei] += got
		}
		if bi >= 0 {
			a[oPdBrgExp+typ*nPdBrg+bi] += exp
			a[oPdBrgGot+typ*nPdBrg+bi] += got
		}
		if ri >= 0 {
			a[oPdRSSIExp+typ*nPdRSSI+ri] += exp
			a[oPdRSSIGot+typ*nPdRSSI+ri] += got
		}
	}
}

// Nominal squitter interval bounds (s) per type and the tolerance used when
// classifying measured GPS intervals.
var (
	ivLo  = [nTimeKind]float64{0.4, 0.4, 4.8}
	ivHi  = [nTimeKind]float64{0.6, 0.6, 5.2}
	ivTol = [nTimeKind]float64{0.01, 0.01, 0.05}
)

// classifyInterval returns k (number of nominal intervals spanned, >= 1) or
// 0 when the interval cannot be produced by a compliant transmitter with
// lost squitters (shorter than the minimum, or in the hole between one and
// two intervals). Larger k are not checked (the ranges overlap from k=3).
func classifyInterval(typ int, sec float64) int {
	lo, hi, tol := ivLo[typ], ivHi[typ], ivTol[typ]
	switch {
	case sec < lo-tol:
		return 0
	case sec <= hi+tol:
		return 1
	case sec < 2*lo-tol:
		return 0
	case sec <= 2*hi+tol:
		return 2
	}
	return 3
}

func (s *Stats) timing(ac *acState, f *Frame, pt int, kind Kind) {
	if ac.gpsFeed == -2 {
		ac.gpsFeed = f.Feed
	} else if f.Feed != ac.gpsFeed {
		return
	}
	last := ac.lastGPS[pt]
	odd := kind == KindPosOdd
	dt := f.GPSNs - last
	if last >= 0 && dt > -1e6 && dt < 1e6 {
		return // duplicate of the same squitter
	}
	ac.lastGPS[pt] = f.GPSNs
	if last < 0 {
		if pt == 0 {
			ac.lastOdd = odd
		}
		return
	}
	if dt < 0 {
		dt += 86400e9 // UTC midnight wrap
	}
	sec := float64(dt) / 1e9
	if sec > 4*ivHi[pt]+1 {
		if pt == 0 {
			ac.lastOdd = odd
		}
		return // track gap: not an interval
	}
	a := &s.cur
	if w := timeBinW[pt]; sec < w*nTimeBins {
		a[oTimeHist+pt*nTimeBins+int(sec/w)]++
	}
	k := classifyInterval(pt, sec)
	a[oTimeN+pt]++
	if pt != 2 {
		ac.tN++
	}
	if k == 0 {
		a[oTimeBad+pt]++
		if pt != 2 {
			ac.tBad++
		}
	}
	if k == 1 && pt == 0 {
		ac.t1N++
		ac.t1Sum += sec
		ac.t1Sq += sec * sec
	}
	if pt == 0 {
		switch k {
		case 1:
			a[oAltK1]++
			ac.altN++
			if odd != ac.lastOdd {
				a[oAltK1OK]++
				ac.altOK++
			}
		case 2:
			a[oAltK2]++
			if odd == ac.lastOdd {
				a[oAltK2OK]++
			}
		}
		ac.lastOdd = odd
	}
	if ac.tN >= 50 && float64(ac.tBad) > 0.05*float64(ac.tN) {
		ac.flags |= fTimingBad
	}
}
