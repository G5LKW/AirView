package sim

import "math"

// Event flags.
const (
	evMLAT   uint8 = 1 << iota // synthetic MLAT result, not an off-air reception
	evTop                      // radiated from the top antenna
	evStatus                   // receiver status frame (no aircraft)
)

// txEvent is one transmission (or synthetic frame) generated inside a step.
type txEvent struct {
	t     float64 // seconds after the step start
	ac    int32   // index into Sim.ac, -1 for receiver status frames
	seq   uint32  // generation order, tie-break for equal times
	n     uint8   // message length in bytes
	flags uint8
	bds   uint8 // Comm-B register carried in MB (0x40, 0x50 ...), 0 otherwise
	lost  uint8 // per-antenna bitmask: destroyed by an overlapping transmission
	msg   [14]byte
	pw    [maxAnt]float32 // received level per antenna, dBFS
}

func (s *Sim) newEvent(idx int32, dt float64, n uint8) *txEvent {
	if dt < 0 {
		dt = 0
	}
	s.events = append(s.events, txEvent{t: dt, ac: idx, n: n, seq: s.seq})
	s.seq++
	return &s.events[len(s.events)-1]
}

// squitter marks an event as an unsolicited squitter: aircraft with antenna
// diversity alternate them between the top and the bottom antenna.
func (a *aircraft) squitter(e *txEvent) {
	if a.diversity {
		a.top = !a.top
		if a.top {
			e.flags |= evTop
		}
	}
}

// emit generates all transmissions of one aircraft that fall inside [t0, t1).
func (s *Sim) emit(a *aircraft, idx int32, t0, t1 float64) {
	r := s.rng
	for a.nextAllCall < t1 {
		s.txAllCall(a, idx, a.nextAllCall-t0)
		a.nextAllCall += 0.8 + 0.4*r.Float64()
	}
	for a.nextSurv < t1 {
		s.txSurv(a, idx, a.nextSurv-t0, t0)
		a.nextSurv += s.expo(a.survRate)
	}
	if a.acasRate > 0 {
		for a.nextACAS < t1 {
			s.txACAS(a, idx, a.nextACAS-t0)
			a.nextACAS += s.expo(a.acasRate)
		}
	}
	if a.mlat {
		for a.nextMLAT < t1 {
			if !a.onGround && t0-a.lastHeard < 15 {
				s.txMLAT(a, idx, a.nextMLAT-t0)
			}
			a.nextMLAT += 1.5 + 4.5*r.Float64()
		}
	}
	if !a.adsb {
		return
	}
	moving := !a.onGround || a.gs > 0.5
	for a.nextPos < t1 {
		s.txPosition(a, idx, a.nextPos-t0, t0)
		if moving {
			a.nextPos += 0.4 + 0.2*r.Float64()
		} else {
			a.nextPos += 4.8 + 0.4*r.Float64()
		}
	}
	for a.nextVel < t1 {
		if !a.onGround {
			s.txVelocity(a, idx, a.nextVel-t0)
		}
		a.nextVel += 0.4 + 0.2*r.Float64()
	}
	for a.nextIdent < t1 {
		s.txIdent(a, idx, a.nextIdent-t0)
		if moving {
			a.nextIdent += 4.8 + 0.4*r.Float64()
		} else {
			a.nextIdent += 9.8 + 0.4*r.Float64()
		}
	}
	if a.version < 2 {
		return
	}
	for a.nextTSS < t1 {
		if !a.onGround {
			s.txTargetState(a, idx, a.nextTSS-t0)
		}
		a.nextTSS += 1.2 + 0.1*r.Float64()
	}
	for a.nextOps < t1 {
		s.txOpStatus(a, idx, a.nextOps-t0)
		if moving {
			a.nextOps += 2.4 + 0.2*r.Float64()
		} else {
			a.nextOps += 4.8 + 0.4*r.Float64()
		}
	}
	for a.nextEmg < t1 {
		// The emergency / priority status message also carries the Mode A
		// code; it is not broadcast for the conspicuity code 1000 unless an
		// emergency is declared.
		if a.emerg != 0 || a.squawk != 0x1000 {
			s.txEmergency(a, idx, a.nextEmg-t0)
		}
		// 0.8 s rate while in emergency or for 24 s after a Mode A code change
		if a.emerg != 0 || t0-a.sqChanged < 24 {
			a.nextEmg += 0.7 + 0.2*r.Float64()
		} else {
			a.nextEmg += 4.8 + 0.4*r.Float64()
		}
	}
}

func (a *aircraft) ca() uint8 {
	if a.onGround {
		return 4
	}
	return 5
}

func (s *Sim) txAllCall(a *aircraft, idx int32, dt float64) {
	e := s.newEvent(idx, dt, 7)
	putDF11(e.msg[:7], a.ca(), a.icao)
	a.squitter(e)
}

func (s *Sim) txIdent(a *aircraft, idx int32, dt float64) {
	e := s.newEvent(idx, dt, 14)
	putES(e.msg[:], 17, a.ca(), a.icao, meIdent(a.catTC, a.cat, &a.callsign))
	a.squitter(e)
}

func (s *Sim) txPosition(a *aircraft, idx int32, dt, t0 float64) {
	dt = max(dt, 0)
	e := s.newEvent(idx, dt, 14)
	odd := a.cprOdd
	a.cprOdd = !a.cprOdd
	utc := a.utcT && !a.onGround
	at := dt
	if utc {
		// UTC-coupled (T=1): the register is loaded a.t1Lead before every
		// 0.2 s UTC epoch with the position extrapolated to that epoch and
		// held until the next load; the CPR format bit is the epoch parity
		// (DO-260A Appendix A §A.1.4.2.3.1). Transmit - epoch is therefore
		// in [-lead, 200 ms - lead), within the ±150 ms tolerance.
		tx := s.epochNs + s.worldNs + int64(dt*1e9+0.5)
		k := floorDivI(tx+a.t1Lead, utcEpochNs)
		odd = k&1 == 1
		at = dt + float64(k*utcEpochNs-tx)/1e9
	}
	lat, lon, alt := a.at(at)
	var me uint64
	if a.onGround {
		yz, xz := cprEncodeSurface(lat, lon, odd)
		me = meSurfacePos(7, encodeMovement(a.gs), true, a.trk, odd, yz, xz)
	} else {
		tc := a.posTC
		code := encodeAC12(alt)
		if a.gnssPos {
			tc = 20
			code = encodeAC12(alt + s.gnssMinusBaro(alt))
		}
		var ss uint8
		switch {
		case a.emerg != 0:
			ss = 1 // permanent alert
		case t0-a.sqChanged < 18:
			ss = 2 // temporary alert
		}
		yz, xz := cprEncodeAirborne(lat, lon, odd)
		me = meAirbornePos(tc, ss, false, code, utc, odd, yz, xz)
	}
	putES(e.msg[:], 17, a.ca(), a.icao, me)
	a.squitter(e)
}

func (s *Sim) txVelocity(a *aircraft, idx int32, dt float64) {
	e := s.newEvent(idx, dt, 14)
	sn, cs := math.Sincos(a.trk * deg2rad)
	me := meVelocity(a.nacv, a.gs*sn, a.gs*cs, a.vrBaro, a.vrate, s.gnssMinusBaro(a.alt))
	putES(e.msg[:], 17, 5, a.icao, me)
	a.squitter(e)
}

func (s *Sim) baroSetting(a *aircraft) float64 {
	if a.alt < 6000 {
		return math.Round(s.env.qnh*10) / 10
	}
	return 1013.2
}

func (s *Sim) txTargetState(a *aircraft, idx int32, dt float64) {
	e := s.newEvent(idx, dt, 14)
	ad := s.airData(&a.kin)
	level := math.Abs(a.vrate) < 200
	f := tssFields{
		selAltFt:  a.selAlt(),
		baroHPa:   s.baroSetting(a),
		selHdgDeg: wrap360(ad.hdgMag + wrap180(a.desTrk-a.trk)),
		nacp:      a.nacp,
		sil:       a.sil,
		nicBaro:   true,
		modeBits:  true,
		autopilot: a.jet(),
		vnav:      !level,
		altHold:   level,
		approach:  a.phase == phFinal || a.phase == phFlare,
		tcas:      a.acas,
		lnav:      a.phase == phNav,
	}
	putES(e.msg[:], 17, 5, a.icao, meTargetState(&f))
	a.squitter(e)
}

func (s *Sim) txOpStatus(a *aircraft, idx int32, dt float64) {
	e := s.newEvent(idx, dt, 14)
	f := opFields{
		surface:       a.onGround,
		acas:          a.acas,
		singleAntenna: a.singleAnt,
		sda:           2,
		nacp:          a.nacp,
		nacv:          a.nacv,
		gva:           2,
		sil:           a.sil,
		nicBaro:       true,
		lenWidth:      a.lenWidth,
	}
	putES(e.msg[:], 17, a.ca(), a.icao, meOpStatus(&f))
	a.squitter(e)
}

func (s *Sim) txEmergency(a *aircraft, idx int32, dt float64) {
	e := s.newEvent(idx, dt, 14)
	putES(e.msg[:], 17, a.ca(), a.icao, meEmergency(a.emerg, encodeID13(a.squawk)))
	a.squitter(e)
}

// txSurv generates a reply to a ground SSR interrogation: DF4/DF5 or, with a
// Comm-B register, DF20/DF21.
func (s *Sim) txSurv(a *aircraft, idx int32, dt, t0 float64) {
	r := s.rng
	long := r.Float64() < a.longFrac
	ident := r.Float64() < 0.22
	_, _, alt := a.at(dt)
	fs := a.fs(t0)
	field := encodeAC13(alt)
	if ident {
		field = encodeID13(a.squawk)
	}
	if !long {
		e := s.newEvent(idx, dt, 7)
		df := uint8(4)
		if ident {
			df = 5
		}
		putSurvShort(e.msg[:7], df, fs, field, a.icao)
		return
	}
	e := s.newEvent(idx, dt, 14)
	df := uint8(20)
	if ident {
		df = 21
	}
	mb, bds := s.commB(a)
	e.bds = bds
	putSurvLong(e.msg[:], df, fs, field, mb, a.icao)
}

// commB picks a Comm-B register the ground station asked for and builds its
// content from the aircraft state.
func (s *Sim) commB(a *aircraft) (mb uint64, bds uint8) {
	x := s.rng.Float64()
	if !a.ehs || a.onGround || x < 0.07 {
		return mbBDS20(&a.callsign), 0x20
	}
	switch {
	case x < 0.37:
		level := math.Abs(a.vrate) < 200
		return mbBDS40(a.selAlt(), 0, s.baroSetting(a), !level, level,
			a.phase == phFinal || a.phase == phFlare), 0x40
	case x < 0.67:
		return mbBDS50(a.roll, a.trk, a.gs, a.trkRate, a.tas), 0x50
	case x < 0.94 || !a.mrar:
		ad := s.airData(&a.kin)
		return mbBDS60(ad.hdgMag, ad.ias, ad.mach, a.vrate, a.vrate), 0x60
	default:
		ad := s.airData(&a.kin)
		return mbBDS44(ad.windKt, ad.windDir, ad.oatC, ad.pressHPa), 0x44
	}
}

// acasSL is the ACAS sensitivity level for an altitude.
func acasSL(a *aircraft) uint8 {
	agl := a.alt - a.gndElev
	switch {
	case !a.acas:
		return 0
	case a.onGround || agl < 1000:
		return 2
	case agl < 2350:
		return 3
	case a.alt < 5000:
		return 4
	case a.alt < 10000:
		return 5
	case a.alt < 20000:
		return 6
	}
	return 7
}

// txACAS generates a DF0 short air-air surveillance reply.
func (s *Sim) txACAS(a *aircraft, idx int32, dt float64) {
	e := s.newEvent(idx, dt, 7)
	_, _, alt := a.at(dt)
	var ri uint8
	if a.acas && !a.onGround {
		ri = 3 // ACAS with vertical-only resolution capability
	}
	putDF0(e.msg[:7], a.onGround, true, acasSL(a), ri, encodeAC13(alt), a.icao)
	if s.rng.Float64() < 0.5 {
		e.flags |= evTop // replies go out on the antenna that was interrogated
	}
}

// txMLAT generates a synthetic MLAT result the way mlat-client hands results
// to a decoder: an even + odd DF18 (CF=2) TC18 position pair and a TC19
// velocity message.
func (s *Sim) txMLAT(a *aircraft, idx int32, dt float64) {
	r := s.rng
	lat, lon, alt := a.at(dt)
	const sigmaM = 120.0
	lat += r.NormFloat64() * sigmaM * degPerM
	lon += r.NormFloat64() * sigmaM * degPerM / a.cosLat
	code := encodeAC12(alt)
	for i := range 2 {
		odd := i == 1
		yz, xz := cprEncodeAirborne(lat, lon, odd)
		e := s.newEvent(idx, dt, 14)
		e.flags |= evMLAT
		putES(e.msg[:], 18, 2, a.icao, meAirbornePos(18, 0, false, code, false, odd, yz, xz))
	}
	sn, cs := math.Sincos(a.trk * deg2rad)
	e := s.newEvent(idx, dt, 14)
	e.flags |= evMLAT
	ew := a.gs*sn + 4*r.NormFloat64()
	ns := a.gs*cs + 4*r.NormFloat64()
	putES(e.msg[:], 18, 2, a.icao, meVelocity(0, ew, ns, true, a.vrate, math.NaN()))
}
