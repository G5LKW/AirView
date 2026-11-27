package rfstat

import (
	"math"
	"sort"
	"strconv"

	"airview/internal/geo"
	"airview/internal/modes"
)

// Report returns a snapshot over a sliding window of about one hour (the
// current clock hour plus the not yet elapsed fraction of the previous one).
// It allocates and takes O(ms); call it from HTTP handlers (cache ~1 s).
func (s *Stats) Report() Report {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	frac := float64(s.sec-s.hour*3600) / 3600
	if frac > 1 {
		frac = 1
	}
	s.scratch.blend(&s.cur, &s.prev, 1-frac)
	return s.build(&s.scratch, true)
}

// History returns the in-memory hourly summaries, oldest first.
func (s *Stats) History() []HourSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]HourSummary(nil), s.history...)
}

func (s *Stats) build(a *accum, live bool) Report {
	r := Report{
		T:         s.sec * 1000,
		WindowSec: Num(a[oSecs]),
		Since:     s.start * 1000,
		Live:      live,
	}
	r.Signal = s.signalReport(a)
	r.Detection = s.detectionReport(a)
	r.Channel = s.channelReport(a, live)
	r.Interrogation = s.interrogationReport(a)
	r.Timing = s.timingReport(a)
	r.Census = s.censusReport(a)
	r.Health = healthReport(&r)
	return r
}

func icaoStr(a uint32) string { return modes.FormatICAO(a) }

// ---------------------------------------------------------------- signal

func strongFrac(h []float64, thr float64) float64 {
	b := rssiBin(thr)
	return ratio(sum(h[b:]), sum(h))
}

func band(label string, lo, hi float64, h []float64, withHist bool) SignalBand {
	n := sum(h)
	sb := SignalBand{Label: label, Lo: Num(lo), Hi: Num(hi), N: int64(math.Round(n)),
		P05:  Num(histQuantile(h, rssiLo, rssiStep, 0.05)),
		P50:  Num(histQuantile(h, rssiLo, rssiStep, 0.5)),
		P95:  Num(histQuantile(h, rssiLo, rssiStep, 0.95)),
		Mean: Num(histMean(h, rssiLo, rssiStep)), StrongFrac: Num(strongFrac(h, -3)),
	}
	if withHist && n > 0 {
		hh := trimHist(h, rssiLo, rssiStep)
		sb.Hist = &hh
	}
	return sb
}

// trimHist drops empty leading/trailing bins.
func trimHist(h []float64, lo, step float64) Hist {
	i, j := 0, len(h)
	for i < j && h[i] == 0 {
		i++
	}
	for j > i && h[j-1] == 0 {
		j--
	}
	c := make([]Num, j-i)
	for k := i; k < j; k++ {
		c[k-i] = Num(math.Round(h[k]*100) / 100)
	}
	return Hist{Lo: Num(lo + float64(i)*step), Step: Num(step), Counts: c}
}

func elevLabel(b int) (string, float64, float64) {
	lo, hi := nan, nan
	if b > 0 {
		lo = elevEdges[b-1]
	}
	if b < len(elevEdges) {
		hi = elevEdges[b]
	}
	switch {
	case b == 0:
		return "<0°", lo, hi
	case b == len(elevEdges):
		return ">" + ftoa(lo) + "°", lo, hi
	}
	return ftoa(lo) + "–" + ftoa(hi) + "°", lo, hi
}

func ftoa(f float64) string {
	b, _ := Num(f).MarshalJSON()
	return string(b)
}

func (s *Stats) signalReport(a *accum) SignalStats {
	all := a[oRSSIAll : oRSSIAll+nRSSI]
	st := SignalStats{Frames: int64(math.Round(sum(all)))}
	st.All = band("all", nan, nan, all, true)
	for i := 0; i < MaxAntennas; i++ {
		h := a[oRSSIAnt+i*nRSSI : oRSSIAnt+(i+1)*nRSSI]
		if sum(h) == 0 {
			continue
		}
		id := "ant" + ftoa(float64(i))
		if i < len(s.cfg.Antennas) {
			id = s.cfg.Antennas[i]
		}
		st.PerAntenna = append(st.PerAntenna, band(id, nan, nan, h, true))
	}
	for i := 0; i < nRing; i++ {
		h := a[oRSSIRing+i*nRSSI : oRSSIRing+(i+1)*nRSSI]
		if sum(h) == 0 {
			continue
		}
		lo, hi := float64(i)*ringKm, float64(i+1)*ringKm
		if i == nRing-1 {
			hi = nan
		}
		st.PerRing = append(st.PerRing, band(ftoa(lo)+" km", lo, hi, h, false))
	}
	for i := 0; i < nElev; i++ {
		h := a[oRSSIElev+i*nRSSI : oRSSIElev+(i+1)*nRSSI]
		if sum(h) == 0 {
			continue
		}
		l, lo, hi := elevLabel(i)
		st.PerElev = append(st.PerElev, band(l, lo, hi, h, false))
	}
	st.DecodeFloor = Num(histQuantile(all, rssiLo, rssiStep, 0.01))
	st.AssumedSNR = Num(s.cfg.DecodeSNRdB)
	st.NoiseFloorEst = Num(float64(st.DecodeFloor) - s.cfg.DecodeSNRdB)
	st.P50 = Num(histQuantile(all, rssiLo, rssiStep, 0.5))
	st.P999 = Num(histQuantile(all, rssiLo, rssiStep, 0.999))
	st.DynamicRange = Num(float64(st.P999) - histQuantile(all, rssiLo, rssiStep, 0.001))
	st.SNRMargin = Num(float64(st.P50) - float64(st.DecodeFloor))
	st.SNREst = Num(float64(st.P50) - float64(st.NoiseFloorEst))
	st.StrongFrac = Num(strongFrac(all, -3))
	st.NearClipFrac = Num(strongFrac(all, -1))

	// fading
	kh := a[oKHist : oKHist+nKBins]
	sh := a[oStdHist : oStdHist+nStdBins]
	f := FadingStats{
		Windows:     int64(math.Round(sum(kh))),
		KMedianDB:   Num(histQuantile(kh, kLo, 1, 0.5)),
		StdMedianDB: Num(histQuantile(sh, 0, stdStep, 0.5)),
		KHist:       Hist{Lo: Num(kLo), Step: 1, Counts: nums(kh)},
		StdHist:     Hist{Lo: 0, Step: stdStep, Counts: nums(sh)},
	}
	for b := 0; b < nElev; b++ {
		n := a[oKElevN+b]
		if n == 0 {
			continue
		}
		l, _, _ := elevLabel(b)
		f.KByElev = append(f.KByElev, ElevK{Label: l, KDB: Num(a[oKElevSum+b] / n), N: int64(n)})
	}
	for i := range s.tab.slots {
		ac := &s.tab.slots[i]
		if !ac.used || ac.fadeWins < 3 {
			continue
		}
		f.Worst = append(f.Worst, AircraftFad{ICAO: icaoStr(ac.icao), KDB: Num(ac.kEWMA), StdDB: Num(ac.stdEWMA),
			Windows: int(ac.fadeWins), DistKm: Num(ac.dist), ElevDeg: Num(ac.elev)})
	}
	sort.Slice(f.Worst, func(i, j int) bool { return f.Worst[i].StdDB > f.Worst[j].StdDB })
	if len(f.Worst) > 10 {
		f.Worst = f.Worst[:10]
	}
	st.Fading = f
	st.Multipath = multipath(a)
	st.Aspect = aspect(a)
	return st
}

// lambdaM is the 1090 MHz wavelength in metres.
const lambdaM = 299792458.0 / 1090e6

// multipath detrends the residual-vs-elevation profile and searches the
// periodogram (in sin(elev)) for two-ray lobing from reflector heights 4-60 m.
func multipath(a *accum) MultipathStats {
	const minN = 20
	var el, dev []float64
	for i := 0; i < nMP; i++ {
		n := a[oMPN+i]
		if n < minN {
			continue
		}
		el = append(el, mpLo+(float64(i)+0.5)*mpStep)
		dev = append(dev, a[oMPSum+i]/n)
	}
	m := MultipathStats{Bins: len(el), RippleDB: Num(nan), ReflectorHeightM: Num(nan), PeriodDeg: Num(nan), Strength: Num(nan)}
	if len(el) < 20 {
		return m
	}
	// centred moving average over +-1 deg as the smooth trend
	res := make([]float64, len(el))
	for i := range el {
		var sm, n float64
		for j := range el {
			if math.Abs(el[j]-el[i]) <= 1.0 {
				sm += dev[j]
				n++
			}
		}
		res[i] = dev[i] - sm/n
	}
	var ss float64
	for _, v := range res {
		ss += v * v
	}
	N := float64(len(res))
	m.RippleDB = Num(math.Sqrt(ss / N))
	m.ElevDeg = nums(el)
	m.DevDB = nums(res)
	if ss <= 0 {
		return m
	}
	x := make([]float64, len(el))
	for i, e := range el {
		x[i] = math.Sin(e * math.Pi / 180)
	}
	bestP, bestH := 0.0, nan
	for h := 4.0; h <= 60; h += 0.1 {
		fr := 2 * h / lambdaM // cycles per unit sin(elev)
		var c, sn float64
		for i, v := range res {
			sv, cv := math.Sincos(2 * math.Pi * fr * x[i])
			c += v * cv
			sn += v * sv
		}
		p := 2 * (c*c + sn*sn) / (N * ss)
		if p > bestP {
			bestP, bestH = p, h
		}
	}
	m.Strength = Num(math.Min(bestP, 1))
	m.ReflectorHeightM = Num(bestH)
	// lobe spacing in degrees near 2 deg elevation
	ds := lambdaM / (2 * bestH)
	m.PeriodDeg = Num((math.Asin(math.Sin(2*math.Pi/180)+ds) - 2*math.Pi/180) * 180 / math.Pi)
	m.Detected = bestP > 0.3 && float64(m.RippleDB) >= 0.5 && len(el) >= 40
	return m
}

func aspect(a *accum) AspectStats {
	var st AspectStats
	mean := func(lo, hi int) float64 {
		var s, n float64
		for i := lo; i < hi; i++ {
			s += a[oAspSum+i]
			n += a[oAspN+i]
		}
		if n < 50 {
			return nan
		}
		return s / n
	}
	for i := 0; i < nAspect; i++ {
		n := a[oAspN+i]
		st.FromDeg = append(st.FromDeg, Num(i*10))
		st.N = append(st.N, int64(n))
		if n < 20 {
			st.MeanDB = append(st.MeanDB, Num(nan))
			st.StdDB = append(st.StdDB, Num(nan))
			continue
		}
		m := a[oAspSum+i] / n
		v := a[oAspSq+i]/n - m*m
		st.MeanDB = append(st.MeanDB, Num(m))
		st.StdDB = append(st.StdDB, Num(math.Sqrt(math.Max(v, 0))))
	}
	st.NoseDB = Num(mean(0, 3))
	st.BeamDB = Num(mean(6, 12))
	st.TailDB = Num(mean(15, 18))
	st.NoseTailDB = Num(float64(st.NoseDB) - float64(st.TailDB))
	return st
}

// ---------------------------------------------------------------- detection

var pdTypeNames = [nPdTypes]string{"position", "velocity", "ident"}

const minPdExp = 100 // expected squitters (per type) for a bin to report Pd

func pdAxis(a *accum, oExp, oGot, n int, lo, hi func(i int) float64) PdAxis {
	var ax PdAxis
	for i := 0; i < n; i++ {
		ax.Lo = append(ax.Lo, Num(lo(i)))
		ax.Hi = append(ax.Hi, Num(hi(i)))
		ax.Expected = append(ax.Expected, Num(a[oExp+i]))
		for typ := 0; typ < nPdTypes; typ++ {
			e := a[oExp+typ*n+i]
			v := nan
			// identification is 10x rarer: scale the threshold
			if e >= minPdExp*nominalRate[typ]/2 {
				v = math.Min(1, a[oGot+typ*n+i]/e)
			}
			switch typ {
			case 0:
				ax.Pos = append(ax.Pos, Num(v))
			case 1:
				ax.Vel = append(ax.Vel, Num(v))
			case 2:
				ax.Ident = append(ax.Ident, Num(v))
			}
		}
	}
	return ax
}

// pdRange returns the distance where position Pd falls below thr: the
// outermost bin with Pd >= thr, interpolated to the next valid bin.
func pdRange(ax PdAxis, thr float64) float64 {
	last := -1
	for i, p := range ax.Pos {
		if p.Valid() && float64(p) >= thr {
			last = i
		}
	}
	if last < 0 {
		return nan
	}
	c := func(i int) float64 { return (float64(ax.Lo[i]) + float64(ax.Hi[i])) / 2 }
	for j := last + 1; j < len(ax.Pos); j++ {
		if p := ax.Pos[j]; p.Valid() {
			p0, p1 := float64(ax.Pos[last]), float64(p)
			return c(last) + (p0-thr)/(p0-p1)*(c(j)-c(last))
		}
	}
	return float64(ax.Hi[last])
}

func (s *Stats) detectionReport(a *accum) DetectionStats {
	var d DetectionStats
	for typ := 0; typ < nPdTypes; typ++ {
		e, g := a[oPdTotExp+typ], a[oPdTotGot+typ]
		d.Types = append(d.Types, PdType{Type: pdTypeNames[typ], NominalRate: Num(nominalRate[typ]),
			Expected: Num(e), Decoded: Num(g), Pd: Num(math.Min(1, ratio(g, e)))})
	}
	d.VsDistance = pdAxis(a, oPdDistExp, oPdDistGot, nPdDist,
		func(i int) float64 { return float64(i) * pdDistKm },
		func(i int) float64 { return float64(i+1) * pdDistKm })
	d.VsElev = pdAxis(a, oPdElevExp, oPdElevGot, nPdElev,
		func(i int) float64 {
			if i == 0 {
				return nan
			}
			return pdElevEdges[i-1]
		},
		func(i int) float64 {
			if i == len(pdElevEdges) {
				return 90
			}
			return pdElevEdges[i]
		})
	d.VsBearing = pdAxis(a, oPdBrgExp, oPdBrgGot, nPdBrg,
		func(i int) float64 { return float64(i * 10) },
		func(i int) float64 { return float64(i*10 + 10) })
	d.VsRSSI = pdAxis(a, oPdRSSIExp, oPdRSSIGot, nPdRSSI,
		func(i int) float64 { return pdRSSILo + float64(i)*pdRSSIW },
		func(i int) float64 { return pdRSSILo + float64(i+1)*pdRSSIW })
	d.Range50Km = Num(pdRange(d.VsDistance, 0.5))
	d.Range90Km = Num(pdRange(d.VsDistance, 0.9))
	for b := 0; b < nGapBands; b++ {
		h := a[oGapHist+b*nGapBins : oGapHist+(b+1)*nGapBins]
		n := a[oGapN+b]
		lo, hi := 0.0, nan
		if b > 0 {
			lo = gapBandEdges[b-1]
		}
		if b < len(gapBandEdges) {
			hi = gapBandEdges[b]
		}
		g := GapBand{LoKm: Num(lo), HiKm: Num(hi), N: int64(math.Round(n)),
			P50: Num(histQuantile(h, 0, gapStep, 0.5)), P90: Num(histQuantile(h, 0, gapStep, 0.9)),
			P95: Num(histQuantile(h, 0, gapStep, 0.95)), P99: Num(histQuantile(h, 0, gapStep, 0.99)),
			Mean: Num(ratio(a[oGapSum+b], n))}
		for i, T := range gapT {
			g.TSec = append(g.TSec, Num(T))
			g.PUpdate = append(g.PUpdate, Num(ratio(a[oGapMin+b*nGapT+i], a[oGapSum+b])))
		}
		d.Gaps = append(d.Gaps, g)
	}
	for i := range s.tab.slots {
		ac := &s.tab.slots[i]
		if !ac.used || ac.lifeExp[0]+ac.lifeExp[1] < 40 {
			continue
		}
		pd := func(t int) Num {
			if ac.lifeExp[t] <= 0 {
				return Num(nan)
			}
			return Num(math.Min(1, float64(ac.lifeGot[t])/float64(ac.lifeExp[t])))
		}
		d.Aircraft = append(d.Aircraft, AircraftPd{ICAO: icaoStr(ac.icao), DistKm: Num(ac.dist),
			PdPos: pd(0), PdVel: pd(1), PdID: pd(2), RSSI: Num(ac.rssiEWMA)})
	}
	sort.Slice(d.Aircraft, func(i, j int) bool { return lessNum(d.Aircraft[i].DistKm, d.Aircraft[j].DistKm) })
	if len(d.Aircraft) > 200 {
		d.Aircraft = d.Aircraft[:200]
	}
	return d
}

func lessNum(a, b Num) bool {
	if !a.Valid() {
		return false
	}
	if !b.Valid() {
		return true
	}
	return a < b
}

// ---------------------------------------------------------------- channel

func dfLabel(df int) string {
	switch {
	case df == DFModeAC:
		return "Mode A/C"
	case df == nDF-1:
		return "other"
	}
	return "DF" + ftoa(float64(df)) + " " + modes.DFName(uint8(df))
}

func (s *Stats) channelReport(a *accum, live bool) ChannelStats {
	secs := a[oSecs]
	fr := a[oFrames]
	c := ChannelStats{Seconds: Num(secs), Frames: int64(math.Round(fr)), FPS: Num(ratio(fr, secs)),
		ByCRC: map[string]int64{}}
	for df := 0; df < nDF; df++ {
		if n := a[oDF+df]; n > 0 {
			c.ByDF = append(c.ByDF, DFRate{DF: df, Label: dfLabel(df), Count: int64(math.Round(n)), Rate: Num(ratio(n, secs))})
		}
	}
	for i := CRCStatus(0); i < numCRC; i++ {
		c.ByCRC[i.String()] = int64(math.Round(a[oCRC+int(i)]))
	}
	bad, fixed := a[oCRC+int(CRCBad)], a[oCRC+int(CRCFixed)]
	acc, rej := a[oCRC+int(CRCAddrAccepted)], a[oCRC+int(CRCAddrRejected)]
	c.OccupancyPct = Num(ratio(a[oAirUs], secs*1e6) * 100)
	tau := ratio(a[oAirUs], fr) // us
	c.MeanReplyUs = Num(tau)
	c.BadFrac = Num(ratio(bad, fr))
	c.FixedFrac = Num(ratio(fixed, fr))
	c.RejectedFrac = Num(ratio(rej, acc+rej))
	fail := ratio(bad+rej, fr)
	c.FailFrac = Num(fail)
	lam := ratio(fr, secs)
	c.OverlapProb = Num(overlap(lam, tau))
	if finite(fail) && fail < 1 && tau > 0 {
		c.ImpliedRate = Num(-math.Log(1-fail) / (2 * tau * 1e-6))
	} else {
		c.ImpliedRate = Num(nan)
	}
	c.ExcessFail = Num(fail - float64(c.OverlapProb))
	for b := 0; b < nLoad; b++ {
		n := a[oLoadSecs+b]
		if n < 1 {
			continue
		}
		f := a[oLoadFrames+b]
		mean := f / n
		c.VsLoad = append(c.VsLoad, LoadBin{LoFPS: Num(float64(b) * loadStep), HiFPS: Num(float64(b+1) * loadStep),
			Seconds: int64(math.Round(n)), MeanFPS: Num(mean), FailFrac: Num(ratio(a[oLoadBad+b], f)),
			ModelFail: Num(overlap(mean, tau))})
	}
	c.BurstCount = int64(math.Round(a[oBursts]))
	if live {
		const n = 300
		end := s.sec - 1 // last closed second
		start := end - n + 1
		c.Recent = Series{T0: start * 1000, Frames: make([]int, n), Bad: make([]int, n), OccPct: make([]Num, n)}
		peak := 0.0
		for x := start; x <= end; x++ {
			i := int(x - start)
			r := &s.secs[((x%secRing)+secRing)%secRing]
			if r.sec != x {
				c.Recent.OccPct[i] = 0
				continue
			}
			c.Recent.Frames[i] = int(r.frames)
			c.Recent.Bad[i] = int(r.bad) + int(r.rejectd)
			o := float64(r.airUs) / 1e4
			c.Recent.OccPct[i] = Num(o)
		}
		for i := range s.secs {
			if r := &s.secs[i]; r.sec > s.sec-secRing && r.sec < s.sec {
				peak = math.Max(peak, float64(r.airUs)/1e4)
			}
		}
		c.PeakOccPct = Num(peak)
		cnt := s.nBurst
		if cnt > maxBursts {
			cnt = maxBursts
		}
		for k := 0; k < cnt; k++ {
			c.Bursts = append(c.Bursts, s.bursts[(s.nBurst-1-k)%maxBursts])
		}
	} else {
		c.PeakOccPct = Num(nan)
	}
	return c
}

// overlap is the Poisson probability that at least one other reply starts
// within +-tau of a given reply at rate lam (1/s), tau in microseconds.
func overlap(lam, tauUs float64) float64 {
	if !finite(lam) || !finite(tauUs) {
		return nan
	}
	return 1 - math.Exp(-2*lam*tauUs*1e-6)
}

// ---------------------------------------------------------------- interrogation

func iidLabel(iid int) string {
	if iid < 16 {
		return "II " + ftoa(float64(iid))
	}
	return "SI " + ftoa(float64(iid-16))
}

func (s *Stats) interrogationReport(a *accum) InterrogationStats {
	var st InterrogationStats
	secs := a[oSecs]
	now := s.sec * 1e9
	recent := now - 120e9
	var perCode [nIID]int
	survRecent := 0
	var rates []float64
	for i := range s.tab.slots {
		ac := &s.tab.slots[i]
		if !ac.used {
			continue
		}
		for k, c := range ac.codes {
			if c != 0 && ac.codeLast[k] >= recent {
				perCode[c-1]++
			}
		}
		active := ac.last >= recent
		survOn := ac.lastSurv >= recent
		adsbOn := ac.lastADSB >= recent
		if survOn {
			survRecent++
		}
		if ac.flags&fACAS != 0 {
			st.ACASAircraft++
		}
		if active {
			st.Active++
			switch {
			case adsbOn && survOn:
				st.ADSBAndModeS++
			case adsbOn:
				st.ADSBOnly++
			case survOn:
				st.ModeSOnly++
			}
		}
		if dur := float64(ac.last-ac.first) / 1e9; dur >= 60 && ac.survN > 0 {
			rate := float64(ac.survN) / dur
			rates = append(rates, rate)
			st.TopSurv = append(st.TopSurv, AircraftRate{ICAO: icaoStr(ac.icao), Rate: Num(rate), Replies: int64(ac.survN)})
		}
	}
	st.RadarShare = Num(ratio(float64(st.ADSBAndModeS), float64(st.ADSBAndModeS+st.ADSBOnly)))
	sort.Float64s(rates)
	st.SurvRateP50, st.SurvRateP90, st.SurvRateMax = Num(qSorted(rates, 0.5)), Num(qSorted(rates, 0.9)), Num(qSorted(rates, 1))
	sort.Slice(st.TopSurv, func(i, j int) bool { return st.TopSurv[i].Rate > st.TopSurv[j].Rate })
	if len(st.TopSurv) > 10 {
		st.TopSurv = st.TopSurv[:10]
	}
	for iid := 0; iid < nIID; iid++ {
		n := a[oIID+iid]
		var last10 int64
		tl := make([]int, 60)
		any := n > 0
		for k := 0; k < 60; k++ {
			m := s.minute - 59 + int64(k)
			row := int(((m % 60) + 60) % 60)
			if s.icMinT[row] != m {
				continue
			}
			v := int(s.icMin[row][iid])
			tl[k] = v
			if v > 0 {
				any = true
			}
			if m > s.minute-10 {
				last10 += int64(v)
			}
		}
		if !any {
			continue
		}
		ic := Interrogator{Code: iidLabel(iid), IID: iid, Replies: int64(math.Round(n)), Rate: Num(ratio(n, secs)),
			Last10m: last10, Aircraft: perCode[iid], ReplyingFrac: Num(ratio(float64(perCode[iid]), float64(survRecent))),
			Timeline: tl, AcqSquitter: iid == 0}
		ic.Lockout = iid != 0 && last10 >= 30 && survRecent >= 5 && float64(perCode[iid]) < 0.2*float64(survRecent)
		if iid != 0 && last10 >= 10 {
			st.ActiveRadars++
		}
		st.Codes = append(st.Codes, ic)
	}
	sort.Slice(st.Codes, func(i, j int) bool { return st.Codes[i].Replies > st.Codes[j].Replies })
	var tot float64
	for i := 0; i < nBDS; i++ {
		tot += a[oBDS+i]
	}
	for i := 0; i < nBDS; i++ {
		n := a[oBDS+i]
		if n == 0 {
			continue
		}
		lbl := "other"
		switch {
		case i < len(bdsList):
			lbl = modes.BDS(bdsList[i]).String()
		case i == nBDS-1:
			lbl = "unknown"
		}
		st.CommB = append(st.CommB, BDSCount{BDS: lbl, Count: int64(math.Round(n)), Frac: Num(n / tot)})
	}
	st.DF0Rate = Num(ratio(a[oDF+0], secs))
	st.DF16Rate = Num(ratio(a[oDF+16], secs))
	st.RAReports = int64(math.Round(a[oRA]))
	return st
}

func qSorted(x []float64, p float64) float64 {
	if len(x) == 0 {
		return nan
	}
	h := p * float64(len(x)-1)
	i := int(h)
	if i >= len(x)-1 {
		return x[len(x)-1]
	}
	return x[i] + (h-float64(i))*(x[i+1]-x[i])
}

// ---------------------------------------------------------------- timing

const chi2Crit9 = 27.877 // chi-square 0.999 quantile, 9 degrees of freedom

func (s *Stats) timingReport(a *accum) TimingStats {
	ts := TimingStats{Note: "Single receiver: GPS timestamps allow interval statistics only; " +
		"time-of-arrival ranging or position verification needs several synchronised receivers (MLAT) and is not attempted."}
	for typ := 0; typ < nTimeKind; typ++ {
		h := a[oTimeHist+typ*nTimeBins : oTimeHist+(typ+1)*nTimeBins]
		n := a[oTimeN+typ]
		w := timeBinW[typ]
		tt := TimingType{Type: pdTypeNames[typ], Intervals: int64(math.Round(n)), BadFrac: Num(ratio(a[oTimeBad+typ], n)),
			Hist: trimHist(h, 0, w), NominalLo: Num(ivLo[typ]), NominalHi: Num(ivHi[typ])}
		lo := int(math.Round(ivLo[typ] / w))
		hi := int(math.Round(ivHi[typ] / w))
		var sm, sq, cnt float64
		for i := lo; i < hi; i++ {
			c := (float64(i) + 0.5) * w
			sm += h[i] * c
			sq += h[i] * c * c
			cnt += h[i]
		}
		tt.Mean, tt.Std, tt.Chi2 = Num(nan), Num(nan), Num(nan)
		if cnt >= 50 {
			m := sm / cnt
			tt.Mean = Num(m)
			tt.Std = Num(math.Sqrt(math.Max(0, sq/cnt-m*m)))
			per := (hi - lo) / 10
			if per >= 1 {
				exp := cnt / 10
				var chi float64
				for k := 0; k < 10; k++ {
					var o float64
					for i := lo + k*per; i < lo+(k+1)*per; i++ {
						o += h[i]
					}
					chi += (o - exp) * (o - exp) / exp
				}
				tt.Chi2 = Num(chi)
				tt.UniformOK = chi < chi2Crit9
			}
		}
		if n > 0 {
			ts.Available = true
		}
		ts.Types = append(ts.Types, tt)
	}
	ts.AltPairs = int64(math.Round(a[oAltK1]))
	ts.AltRegularity = Num(ratio(a[oAltK1OK], a[oAltK1]))
	ts.Alt2Pairs = int64(math.Round(a[oAltK2]))
	ts.Alt2Same = Num(ratio(a[oAltK2OK], a[oAltK2]))
	for i := range s.tab.slots {
		ac := &s.tab.slots[i]
		if !ac.used || ac.tN < 50 {
			continue
		}
		bad := float64(ac.tBad) / float64(ac.tN)
		mean, std := nan, nan
		if ac.t1N >= 30 {
			n := float64(ac.t1N)
			mean = ac.t1Sum / n
			std = math.Sqrt(math.Max(0, ac.t1Sq/n-mean*mean))
		}
		reason := ""
		switch {
		case bad > 0.05:
			reason = "intervals outside 0.4-0.6 s (and multiples)"
		case finite(std) && std < 0.025:
			reason = "no random jitter (std < 25 ms)"
		case finite(mean) && math.Abs(mean-0.5) > 0.03:
			reason = "mean interval off nominal 0.5 s"
		case ac.altN >= 30 && float64(ac.altOK) < 0.9*float64(ac.altN):
			reason = "even/odd CPR formats do not alternate"
		}
		if reason == "" {
			continue
		}
		ts.NonCompliant = append(ts.NonCompliant, NonCompliant{ICAO: icaoStr(ac.icao), Intervals: int(ac.tN),
			BadFrac: Num(bad), Mean: Num(mean), Std: Num(std), Reason: reason})
	}
	sort.Slice(ts.NonCompliant, func(i, j int) bool { return ts.NonCompliant[i].ICAO < ts.NonCompliant[j].ICAO })
	return ts
}

// ---------------------------------------------------------------- census

var altBandNames = [nAltBands]string{"<1000", "1-5k", "5-10k", "10-20k", "20-30k", "30-40k", ">40k"}

func counts(labels []string, c []int, tot int) []Count {
	out := make([]Count, 0, len(c))
	for i, n := range c {
		if n == 0 {
			continue
		}
		out = append(out, Count{Label: labels[i], Count: n, Share: Num(ratio(float64(n), float64(tot)))})
	}
	return out
}

func numLabels(n int) []string {
	l := make([]string, n+1)
	for i := 0; i < n; i++ {
		l[i] = ftoa(float64(i))
	}
	l[n] = "unknown"
	return l
}

func (s *Stats) censusReport(a *accum) CensusStats {
	var c CensusStats
	ver := make([]int, 4)
	nic := make([]int, 13)
	nacp := make([]int, 13)
	sil := make([]int, 5)
	cats := map[uint8]int{}
	var tc28, tc29, tc31, gnss, poor int
	for i := range s.tab.slots {
		ac := &s.tab.slots[i]
		if !ac.used {
			continue
		}
		c.Aircraft++
		if ac.jumps > 0 {
			c.JumpAircraft++
		}
		if ac.flags&fADSB == 0 {
			continue
		}
		c.ADSB++
		v := int(ac.version)
		if v < 0 || v > 2 {
			v = 3
		}
		ver[v]++
		idx := func(x int8, n int) int {
			if x < 0 || int(x) >= n {
				return n
			}
			return int(x)
		}
		nic[idx(ac.nic, 12)]++
		nacp[idx(ac.nacp, 12)]++
		sil[idx(ac.sil, 4)]++
		if ac.category != 0 {
			cats[ac.category]++
		}
		if ac.flags&fTC28 != 0 {
			tc28++
		}
		if ac.flags&fTC29 != 0 {
			tc29++
		}
		if ac.flags&fTC31 != 0 {
			tc31++
		}
		if ac.flags&fGNSSAlt != 0 {
			gnss++
		}
		// 14 CFR 91.227 performance: NACp >= 8, NIC >= 7, SIL = 3.
		if (ac.nacp >= 0 && ac.nacp < 8) || (ac.nic >= 0 && ac.nic < 7) || (ac.sil >= 0 && ac.sil < 3) {
			poor++
		}
		if ac.degraded {
			c.Degraded++
			if len(c.DegradedList) < 50 {
				c.DegradedList = append(c.DegradedList, icaoStr(ac.icao))
			}
		}
	}
	t := c.ADSB
	c.Versions = counts([]string{"v0", "v1", "v2", "unknown"}, ver, t)
	c.NIC = counts(numLabels(12), nic, t)
	c.NACp = counts(numLabels(12), nacp, t)
	c.SIL = counts(numLabels(4), sil, t)
	c.PoorIntegrity = Num(ratio(float64(poor), float64(t)))
	keys := make([]int, 0, len(cats))
	for k := range cats {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	for _, k := range keys {
		lbl := modes.CategoryName(uint8(k))
		if lbl == "" {
			lbl = "0x" + strconv.FormatUint(uint64(k), 16)
		}
		c.Categories = append(c.Categories, Count{Label: lbl, Count: cats[uint8(k)],
			Share: Num(ratio(float64(cats[uint8(k)]), float64(t)))})
	}
	c.TC28Share = Num(ratio(float64(tc28), float64(t)))
	c.TC29Share = Num(ratio(float64(tc29), float64(t)))
	c.TC31Share = Num(ratio(float64(tc31), float64(t)))
	c.GNSSAltShare = Num(ratio(float64(gnss), float64(t)))
	gd := a[oGDHist : oGDHist+nGD]
	c.GNSSDelta = trimHist(gd, gdLo, gdStep)
	var sm, n float64
	for b := 0; b < nAltBands; b++ {
		sm += a[oGDSum+b]
		n += a[oGDN+b]
		if a[oGDN+b] == 0 {
			continue
		}
		m := a[oGDSum+b] / a[oGDN+b]
		c.GNSSDeltaByAlt = append(c.GNSSDeltaByAlt, AltBandDelta{Band: altBandNames[b], MeanFt: Num(m),
			StdFt: Num(math.Sqrt(math.Max(0, a[oGDSq+b]/a[oGDN+b]-m*m))), N: int64(a[oGDN+b])})
	}
	c.GNSSDeltaMean = Num(ratio(sm, n))

	c.GNSSIndexNow = Num(nan)
	for k := int64(gnssRing); k >= 1; k-- {
		m := s.minute - k
		g := &s.gnss[((m%gnssRing)+gnssRing)%gnssRing]
		if g.min != m || (g.tot == 0 && g.jmp == 0) {
			continue
		}
		idx := ratio(float64(g.deg), float64(g.tot)) * 100
		c.GNSSSeries = append(c.GNSSSeries, GNSSPoint{T: m * 60000, Index: Num(idx), Tot: int(g.tot), Deg: int(g.deg), Jumps: int(g.jmp)})
		if k == 1 {
			c.GNSSIndexNow = Num(idx)
		}
	}
	haveStation := finite(s.cfg.StationLat) && finite(s.cfg.StationLon) && (s.cfg.StationLat != 0 || s.cfg.StationLon != 0)
	for cell := 0; cell < nCellSec*nCellRing; cell++ {
		tot := a[oCellTot+cell]
		if tot == 0 {
			continue
		}
		sec, ring := cell/nCellRing, cell%nCellRing
		gc := GNSSCell{Sector: sec, Ring: ring, Tot: int64(math.Round(tot)), Deg: int64(math.Round(a[oCellDeg+cell])),
			Index: Num(a[oCellDeg+cell] / tot * 100), CenterLat: Num(nan), CenterLon: Num(nan)}
		if haveStation {
			lat, lon := geo.Destination(s.cfg.StationLat, s.cfg.StationLon, float64(sec*10+5), (float64(ring)+0.5)*cellKm)
			gc.CenterLat, gc.CenterLon = Num(lat), Num(lon)
		}
		c.GNSSCells = append(c.GNSSCells, gc)
	}
	c.DroppedFrames = s.dropped
	return c
}

// ---------------------------------------------------------------- summary

func (s *Stats) summarize(a *accum, hourStart int64) HourSummary {
	r := s.build(a, false)
	h := HourSummary{
		T: hourStart * 1000, Seconds: r.Channel.Seconds, Frames: r.Channel.Frames, FPS: r.Channel.FPS,
		OccupancyPct: r.Channel.OccupancyPct, BadFrac: r.Channel.BadFrac, FixedFrac: r.Channel.FixedFrac,
		RejectedFrac: r.Channel.RejectedFrac, OverlapProb: r.Channel.OverlapProb,
		DecodeFloor: r.Signal.DecodeFloor, P50: r.Signal.P50, StrongFrac: r.Signal.StrongFrac,
		KMedianDB: r.Signal.Fading.KMedianDB,
		PdPos:     r.Detection.Types[0].Pd, PdVel: r.Detection.Types[1].Pd, PdIdent: r.Detection.Types[2].Pd,
		Range50Km: r.Detection.Range50Km, Range90Km: r.Detection.Range90Km,
		Radars: r.Interrogation.ActiveRadars, RAReports: r.Interrogation.RAReports,
		ACASRate: Num(float64(r.Interrogation.DF0Rate) + float64(r.Interrogation.DF16Rate)),
		Bursts:   r.Channel.BurstCount, Health: r.Health.Score,
		DF11ByIID: map[string]int64{},
	}
	// overall position update statistics over all bands
	var gs, gm, gn float64
	all := make([]float64, nGapBins)
	for b := 0; b < nGapBands; b++ {
		gs += a[oGapSum+b]
		gm += a[oGapMin+b*nGapT+2] // T = 5 s
		gn += a[oGapN+b]
		for i := 0; i < nGapBins; i++ {
			all[i] += a[oGapHist+b*nGapBins+i]
		}
	}
	h.PUpdate5s = Num(ratio(gm, gs))
	h.GapP95 = Num(histQuantile(all, 0, gapStep, 0.95))
	for iid := 0; iid < nIID; iid++ {
		if n := a[oIID+iid]; n > 0 {
			h.DF11ByIID[iidLabel(iid)] = int64(math.Round(n))
		}
	}
	h.TimingBad = Num(ratio(a[oTimeBad]+a[oTimeBad+1], a[oTimeN]+a[oTimeN+1]))
	mx := nan
	for _, p := range r.Census.GNSSSeries {
		if p.T >= hourStart*1000 && p.T < (hourStart+3600)*1000 && p.Index.Valid() && (!finite(mx) || float64(p.Index) > mx) {
			mx = float64(p.Index)
		}
	}
	h.GNSSIndexMax = Num(mx)
	return h
}
