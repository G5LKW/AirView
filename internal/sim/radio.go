package sim

import (
	"math"
	"slices"
	"time"

	"airview/internal/model"
)

// Reception model constants.
const (
	// fullScaleDBm is the received power (at the antenna connector, after
	// feedline loss) that maps to 0 dBFS.
	fullScaleDBm = -50.5
	// noiseFloorDBFS is the noise level of the reference receiver chain
	// (6 dB system noise figure) on the dBFS scale.
	noiseFloorDBFS = -38.5
	// detection: SNR (dB) at which half of the frames decode, and the slope
	// of the logistic detection curve.
	snr50Short = 3.5
	snr50Long  = 4.2
	snrSlope   = 1.1
	// fsplConst is 32.4478 + 20*log10(1090 MHz): FSPL(dB) = fsplConst + 20*log10(km).
	fsplConst = 93.196
	dayNs     = 86400_000_000_000
)

type station struct {
	lat, lon float64
	groundM  float64 // terrain elevation, m AMSL
}

// antModel is the receive model of one antenna + RF chain.
type antModel struct {
	id      string
	channel int
	omni    bool
	gain    float64 // peak gain minus feedline / filter loss, dB
	az      float64 // boresight azimuth, deg true
	tilt    float64 // boresight elevation, deg
	hbw     float64 // -3 dB horizontal beamwidth, deg
	vbw     float64 // -3 dB vertical beamwidth, deg
	fb      float64 // front-to-back ratio (directional) / pattern floor (omni), dB
	floor   float64 // the same as a power ratio
	heightM float64 // feed point height above ground
	noise   float64 // noise floor on the dBFS scale
}

func newAntModel(a model.Antenna, index int) antModel {
	m := antModel{
		id:      a.ID,
		channel: a.Channel,
		gain:    a.GainDBi - a.CableLossDB - a.FilterLossDB,
		az:      a.AzimuthDeg,
		tilt:    a.TiltDeg,
		hbw:     a.HBeamwidthDeg,
		vbw:     a.VBeamwidthDeg,
		fb:      a.FrontToBackDB,
		heightM: math.Max(a.HeightAGLM, 1),
	}
	if m.channel < 0 {
		m.channel = index
	}
	g := math.Pow(10, a.GainDBi/10)
	switch a.Type {
	case model.AntYagi, model.AntPatch:
		// beamwidths from directivity when not given: G ~ 30000 / (hbw * vbw)
		est := math.Sqrt(30000 / math.Max(g, 1.5))
		if m.hbw <= 0 {
			m.hbw = clamp(est, 10, 120)
		}
		if m.vbw <= 0 {
			m.vbw = clamp(est, 10, 120)
		}
		if m.fb <= 0 {
			m.fb = 15
		}
	default:
		m.omni = true
		if m.vbw <= 0 {
			// omni: G ~ 101.5 / vbw
			m.vbw = clamp(101.5/math.Max(g, 1), 6, 80)
		}
		m.fb = 18
	}
	m.floor = math.Pow(10, -m.fb/10)
	// Noise: the reference chain has a 6 dB noise figure. A mast-head LNA
	// replaces it with its own (lower) figure.
	nf := 6.0
	if a.LNAGainDB > 0 {
		nf = a.LNANoiseFigDB
		if nf <= 0 {
			nf = 1.5
		}
	}
	m.noise = noiseFloorDBFS + (nf - 6)
	return m
}

// rel returns the pattern gain relative to the peak (dB, <= 0) towards the
// given true bearing and elevation angle.
func (m *antModel) rel(brg, el float64) float64 {
	de := (el - m.tilt) / m.vbw
	main := 12 * de * de
	if !m.omni {
		da := wrap180(brg-m.az) / m.hbw
		main += 12 * da * da
	}
	// power-sum of the main lobe and the side/back-lobe floor, normalised to
	// 0 dB on boresight
	lobe := 0.0
	if main < 60 {
		lobe = math.Exp(-main * (math.Ln10 / 10))
	}
	return 10 * math.Log10((lobe+m.floor)/(1+m.floor))
}

// diffractionLoss is the extra path loss (dB) around the radio horizon, x km
// beyond it (negative = still inside): a soft 6 dB grazing step followed by a
// steady smooth-earth diffraction slope.
func diffractionLoss(x float64) float64 {
	if x < -20 {
		return 0
	}
	l := 6 / (1 + math.Exp(-x/4))
	if x > 0 {
		l += 0.8 * x
	}
	return l
}

// updateLink recomputes the mean link budget of an aircraft to every antenna.
func (s *Sim) updateLink(a *aircraft) {
	d, brg := distBearing(s.st.lat, s.st.lon, a.lat, a.lon)
	a.stDist, a.stBrg = d, brg
	haM := a.alt*ftToM - s.st.groundM
	if a.onGround {
		haM = a.gndElev*ftToM - s.st.groundM + 4
	}
	ha := math.Max(haM, 2) / 1000 // km
	ae := s.env.kFactor * earthRadiusKm
	th := d / ae
	sn, cs := math.Sincos(th)
	sh := math.Sin(th / 2)
	for j := range s.ants {
		m := &s.ants[j]
		hs := m.heightM / 1000
		el := math.Atan2((ae+ha)*cs-(ae+hs), (ae+ha)*sn) * rad2deg
		r := math.Sqrt((ha-hs)*(ha-hs) + 4*(ae+hs)*(ae+ha)*sh*sh)
		if r < 0.05 {
			r = 0.05
		}
		x := d - (math.Sqrt(2*ae*hs) + math.Sqrt(2*ae*ha))
		mean := a.eirp - (fsplConst + 20*math.Log10(r)) - diffractionLoss(x) +
			m.gain + m.rel(brg, el) + a.shadow - 0.1*math.Abs(a.roll) - fullScaleDBm
		// multipath grows towards and beyond the horizon
		sigma := 0.16 + 0.45*clamp((x+25)/35, 0, 1)
		a.link[j] = link{mean: float32(mean), sigma: float32(sigma)}
		if j == 0 {
			a.stElev = el
			a.slantKm = r
		}
	}
	// the top antenna is shadowed by the fuselage for receivers below
	a.topPen = float32(3 + 4*clamp(a.stElev/10, 0, 1))
}

// softClip compresses levels near full scale so nothing exceeds -0.5 dBFS.
func softClip(p float64) float64 {
	if p > -3 {
		p = -3 + 2.5*(1-math.Exp(-(p+3)/2.5))
	}
	return math.Round(p*10) / 10
}

func cmpEvent(a, b txEvent) int {
	switch {
	case a.t < b.t:
		return -1
	case a.t > b.t:
		return 1
	case a.seq < b.seq:
		return -1
	case a.seq > b.seq:
		return 1
	}
	return 0
}

// frameData copies a message into the slab and returns the stable slice.
func (s *Sim) frameData(msg []byte) []byte {
	n := len(msg)
	if len(s.slab)+n > cap(s.slab) {
		s.slab = make([]byte, 0, 4096)
	}
	off := len(s.slab)
	s.slab = append(s.slab, msg...)
	return s.slab[off : off+n : off+n]
}

// receive turns the step's transmissions into received frames in s.batch,
// applying the link budget, fading, mutual interference and the detection curve.
func (s *Sim) receive() {
	ev := s.events
	if len(ev) > 1 {
		slices.SortFunc(ev, cmpEvent)
	}
	s.batch = s.batch[:0]
	nAnt := len(s.ants)
	perfect := s.cfg.PerfectReception
	r := s.rng

	// pass 1: instantaneous received level per antenna
	for i := range ev {
		e := &ev[i]
		if e.ac < 0 || e.flags&evMLAT != 0 {
			continue
		}
		a := &s.ac[e.ac]
		s.txCount++
		for j := range nAnt {
			l := &a.link[j]
			p := float64(l.mean)
			if e.flags&evTop != 0 {
				p -= float64(a.topPen)
			}
			if perfect {
				e.pw[j] = float32(p)
				continue
			}
			if p < s.ants[j].noise-10 {
				e.pw[j] = -200 // hopeless: skip the fading draw
				continue
			}
			// Rician fading: unit line-of-sight phasor plus complex Gaussian scatter
			sg := float64(l.sigma)
			x := 1 + sg*r.NormFloat64()
			y := sg * r.NormFloat64()
			p += 10 * math.Log10((x*x+y*y)/(1+2*sg*sg)+1e-9)
			e.pw[j] = float32(p)
		}
	}

	// pass 2: overlapping transmissions garble each other
	if !perfect {
		for i := 0; i+1 < len(ev); i++ {
			a := &ev[i]
			if a.ac < 0 || a.flags&evMLAT != 0 {
				continue
			}
			dur := 64e-6
			if a.n == 14 {
				dur = 120e-6
			}
			for k := i + 1; k < len(ev) && ev[k].t-a.t < dur; k++ {
				b := &ev[k]
				if b.ac < 0 || b.flags&evMLAT != 0 {
					continue
				}
				for j := range nAnt {
					floor := float32(s.ants[j].noise) - 3
					pa, pb := a.pw[j], b.pw[j]
					if pa < floor || pb < floor {
						continue
					}
					bit := uint8(1) << j
					switch {
					case pa-pb >= 6:
						b.lost |= bit
					case pb-pa >= 9:
						a.lost |= bit
					default:
						a.lost |= bit
						b.lost |= bit
					}
				}
			}
		}
	}

	// pass 3: detection and frame output
	base := s.epochNs + s.worldNs
	t0 := float64(s.worldNs) / 1e9
	fruit := 1 - (0.015 + 0.0006*float64(len(s.ac))) // loss to traffic we do not simulate
	for i := range ev {
		e := &ev[i]
		ns := base + int64(e.t*1e9+0.5)
		if e.ac >= 0 && e.flags&evMLAT == 0 {
			// Arrival at the antenna, time-stamped like a Radarcape at the
			// END of the frame (8 us preamble + 56 / 112 bits).
			ns += int64(s.ac[e.ac].slantKm*1e9/radioKmPerS + 0.5)
			if e.n == 14 {
				ns += 120_000
			} else {
				ns += 64_000
			}
		}
		sod := ns % dayNs
		if sod < 0 {
			sod += dayNs
		}
		f := model.RawFrame{
			Recv:    time.Unix(0, ns).UTC(), // arrival; stampRecv adds the receiver -> host delay
			GPSNs:   sod,
			Signal:  math.NaN(),
			Kind:    model.FrameModeSLong,
			FeedID:  s.cfg.FeedID,
			Channel: -1,
		}
		if e.n == 7 {
			f.Kind = model.FrameModeSShort
		}
		switch {
		case e.flags&evStatus != 0:
			f.Kind = model.FrameStatus
			f.Data = s.frameData(e.msg[:e.n])
			s.batch = append(s.batch, f)
			continue
		case e.flags&evMLAT != 0:
			f.MLAT = true
			f.GPSNs = -1
			f.Data = s.frameData(e.msg[:e.n])
			s.batch = append(s.batch, f)
			s.rxCount++
			continue
		}
		thr := snr50Short
		if e.n == 14 {
			thr = snr50Long
		}
		var data []byte
		for j := range nAnt {
			p := float64(e.pw[j])
			if !perfect {
				if e.lost&(1<<j) != 0 {
					continue
				}
				noise := s.ants[j].noise
				z := (p - noise - thr) / snrSlope
				if z < -5 {
					continue
				}
				if r.Float64() >= fruit/(1+math.Exp(-z)) {
					continue
				}
				// The receiver measures signal plus noise power, so reported
				// levels flatten out just above the noise floor.
				p += 10 * math.Log10(1+math.Exp((noise-p)*(math.Ln10/10)))
				p += 0.3 * r.NormFloat64() // level measurement noise
			}
			if data == nil {
				data = s.frameData(e.msg[:e.n])
				s.ac[e.ac].lastHeard = t0
			}
			f.Data = data
			f.Signal = math.Max(softClip(p), -60)
			f.Channel = s.ants[j].channel
			s.batch = append(s.batch, f)
			s.rxCount++
		}
	}
	s.stampRecv(base + stepNs)
}

// radioKmPerS is the speed of 1090 MHz radio waves in the lower atmosphere
// (c / 1.0003).
const radioKmPerS = 299_702.5

// rxDelayNs draws the delay from the end of a frame at the antenna to the
// host reading it from the socket: receiver processing and USB / LAN
// transfer (~1.6 ms plus an exponential tail) and, now and then, a burst of
// TCP coalescing or scheduling delay of up to ~25 ms. It never exceeds 60 ms.
func (s *Sim) rxDelayNs() int64 {
	r := s.rxRng
	d := 1.6e6 + 0.7e6*r.ExpFloat64()
	if r.Float64() < 0.005 {
		d += 3e6 + 22e6*r.Float64()
	}
	return int64(math.Min(d, 60e6))
}

// stampRecv turns the arrival times in s.batch (Recv as set by receive) into
// host receive times like a real feed. Frames leave the receiver in arrival
// order, so GPS timestamps are monotone in the stream; frames that arrive
// after stepEnd (late in the step and far away) wait for the next step's
// earlier arrivals. Each frame's Recv is its arrival plus a receiver -> host
// delay, and a delayed frame holds up the ones behind it, so Recv is
// monotone too. Frames whose Recv falls beyond stepEnd are carried into the
// next step: every batch holds exactly the frames received during its step.
func (s *Sim) stampRecv(stepEnd int64) {
	nb := s.batch
	w := append(s.work[:0], s.pendArr...)
	w = append(w, nb...)
	// arrival order (insertion sort: nearly sorted already, stable, no allocation)
	for i := 1; i < len(w); i++ {
		for j := i; j > 0 && w[j].Recv.Before(w[j-1].Recv); j-- {
			w[j], w[j-1] = w[j-1], w[j]
		}
	}
	k := len(w)
	for k > 0 && w[k-1].Recv.UnixNano() >= stepEnd {
		k--
	}
	s.pendArr = append(s.pendArr[:0], w[k:]...)
	ready := w[:k]
	for i := range ready {
		d := s.rxDelayNs()
		if ready[i].MLAT {
			d = 0 // results injected by the MLAT client: no receiver delay of their own
		}
		r := max(ready[i].Recv.UnixNano()+d, s.lastRecv)
		s.lastRecv = r
		ready[i].Recv = time.Unix(0, r).UTC()
	}
	s.work = w[:0]
	out := append(s.spare[:0], s.carry...)
	out = append(out, ready...)
	n := len(out)
	for n > 0 && out[n-1].Recv.UnixNano() >= stepEnd {
		n--
	}
	s.carry = append(s.carry[:0], out[n:]...)
	s.batch, s.spare = out[:n], nb[:0]
}
