package rfstat

import "math"

// advance moves the engine clock to Unix second sec, closing every second,
// minute and hour in between. Called with s.mu held.
func (s *Stats) advance(sec int64) {
	if s.sec == 0 {
		s.sec, s.start = sec, sec
		s.hour = sec / 3600
		s.minute = sec / 60
		s.icMinT[s.minute%60] = s.minute
		s.secs[sec%secRing] = secRec{sec: sec}
		s.lastEvict = sec
		return
	}
	if sec <= s.sec {
		return
	}
	if sec-s.sec > 6*3600 {
		// Long outage / clock jump: close the current hour and restart.
		s.closeSecond(s.sec)
		s.hourRoll(sec / 3600)
		s.prev.reset()
		s.secs = [secRing]secRec{}
		s.icMin = [60][nIID]uint16{}
		s.icMinT = [60]int64{}
		s.minute = sec / 60
		s.icMinT[s.minute%60] = s.minute
		s.sec = sec
		s.secs[sec%secRing] = secRec{sec: sec}
		s.evict(sec)
		return
	}
	for x := s.sec + 1; x <= sec; x++ {
		s.closeSecond(x - 1)
		if m := x / 60; m != s.minute {
			s.minuteRoll(m, x)
		}
		if h := x / 3600; h != s.hour {
			s.hourRoll(h)
		}
		s.secs[x%secRing] = secRec{sec: x}
		s.sec = x
		if x-s.lastEvict >= 10 {
			s.evict(x)
		}
	}
}

// closeSecond finalises the per-second channel record of second x.
func (s *Stats) closeSecond(x int64) {
	a := &s.cur
	a[oSecs]++
	r := &s.secs[x%secRing]
	if r.sec != x {
		*r = secRec{sec: x}
	}
	fr := float64(r.frames)
	b := clampInt(int(fr/loadStep), 0, nLoad-1)
	a[oLoadSecs+b]++
	a[oLoadFrames+b] += fr
	a[oLoadBad+b] += float64(r.bad) + float64(r.rejectd)

	// burst detection against an EWMA of the per-second rate
	if s.rateN >= 30 {
		sd := math.Sqrt(s.rateVar)
		if fr > s.rateAvg+5*sd+10 && fr > 1.5*s.rateAvg {
			a[oBursts]++
			s.bursts[s.nBurst%maxBursts] = Burst{
				T: x * 1000, Frames: int(r.frames), Expected: Num(s.rateAvg),
				BadFrac: Num(ratio(float64(r.bad)+float64(r.rejectd), fr)),
			}
			s.nBurst++
			return // keep bursts out of the baseline
		}
	}
	const alpha = 1.0 / 60
	if s.rateN == 0 {
		s.rateAvg = fr
	} else {
		d := fr - s.rateAvg
		s.rateAvg += alpha * d
		s.rateVar = (1 - alpha) * (s.rateVar + alpha*d*d)
	}
	s.rateN++
}

// minuteRoll closes minute s.minute and starts minute m.
func (s *Stats) minuteRoll(m, nowSec int64) {
	closed := s.minute
	g := &s.gnss[closed%gnssRing]
	*g = gnssMin{min: closed}
	now := nowSec * 1e9
	a := &s.cur
	for i := range s.tab.slots {
		ac := &s.tab.slots[i]
		if !ac.used {
			continue
		}
		g.jmp += ac.jumpsMin
		ac.jumpsMin = 0
		if ac.flags&fADSB == 0 || now-ac.posTMinute > 60e9 || (ac.nacp < 0 && ac.nic < 0) {
			continue
		}
		g.tot++
		cell := -1
		if d, b := float64(ac.dist), float64(ac.brg); finite(d) && finite(b) {
			cell = brgBin(b)*nCellRing + clampInt(int(d/cellKm), 0, nCellRing-1)
			a[oCellTot+cell]++
		}
		if ac.degraded {
			g.deg++
			if cell >= 0 {
				a[oCellDeg+cell]++
			}
		}
	}
	s.minute = m
	row := m % 60
	s.icMin[row] = [nIID]uint16{}
	s.icMinT[row] = m
}

// evict removes aircraft not heard for AircraftTimeout.
func (s *Stats) evict(nowSec int64) {
	s.lastEvict = nowSec
	limit := nowSec*1e9 - s.cfg.AircraftTimeout.Nanoseconds()
	for i := range s.tab.slots {
		ac := &s.tab.slots[i]
		if ac.used && ac.last < limit {
			s.tab.remove(ac.icao)
		}
	}
}

// hourRoll closes the current hour (summary + history) and starts hour h.
func (s *Stats) hourRoll(h int64) {
	if s.cur[oSecs] > 0 {
		sum := s.summarize(&s.cur, s.hour*3600)
		if len(s.history) == s.cfg.History {
			copy(s.history, s.history[1:])
			s.history = s.history[:len(s.history)-1]
		}
		s.history = append(s.history, sum)
		select {
		case s.pending <- sum:
		default:
			s.log.Warn("rfstat: hourly summary queue full, dropping", "hour", sum.T)
		}
	}
	if h == s.hour+1 {
		s.prev = s.cur
	} else {
		s.prev.reset()
	}
	s.cur.reset()
	s.hour = h
}
