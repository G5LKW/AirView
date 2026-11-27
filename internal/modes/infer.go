package modes

import "math"

// Comm-B register inference.
//
// A DF20/21 reply does not name the register it carries. Following the
// published description in "The 1090 Megahertz Riddle" (J. Sun) and the
// register definitions of ICAO Doc 9871, each supported register is tested
// with hard rules (status bits that contradict their data, reserved bits,
// physically impossible values). Every register that survives becomes a
// candidate, and soft plausibility checks turn into a confidence so that the
// caller can rank them, ideally after re-scoring against known aircraft state
// with DisambiguateBDS.

// unknown marks an unavailable float input.
var unknown = math.NaN()

func known(v float64) bool { return !math.IsNaN(v) }

// Standard atmosphere helpers (pressure altitude in feet, speeds in knots).
const (
	isaA0         = 661.4786 // speed of sound at sea level, kt
	isaTropopause = 36089.24 // ft
	isaLapse      = 6.87535e-6
)

// isaDelta returns the pressure ratio p/p0 at a pressure altitude.
func isaDelta(altFt float64) float64 {
	if altFt < isaTropopause {
		return math.Pow(1-isaLapse*altFt, 5.2561)
	}
	return 0.223361 * math.Exp(-(altFt-isaTropopause)/20805.8)
}

// isaTheta returns the temperature ratio T/T0 of the standard atmosphere.
func isaTheta(altFt float64) float64 {
	if altFt < isaTropopause {
		return 1 - isaLapse*altFt
	}
	return 0.751865
}

// isaTempC returns the standard atmosphere temperature in °C.
func isaTempC(altFt float64) float64 { return 288.15*isaTheta(altFt) - 273.15 }

// machToCAS converts a Mach number to calibrated airspeed. The relation
// depends on pressure altitude only, not on temperature, so it is exact for
// the real atmosphere.
func machToCAS(mach, altFt float64) float64 {
	qc := isaDelta(altFt) * (math.Pow(1+0.2*mach*mach, 3.5) - 1)
	return isaA0 * math.Sqrt(5*(math.Pow(qc+1, 2.0/7.0)-1))
}

// casToMach is the inverse of machToCAS.
func casToMach(cas, altFt float64) float64 {
	x := cas / isaA0
	qc := math.Pow(1+0.2*x*x, 3.5) - 1
	return math.Sqrt(5 * (math.Pow(qc/isaDelta(altFt)+1, 2.0/7.0) - 1))
}

// machToTAS converts Mach to true airspeed assuming standard temperature.
func machToTAS(mach, altFt float64) float64 {
	return mach * isaA0 * math.Sqrt(isaTheta(altFt))
}

func clampConf(c float64) float32 {
	if c < 0.05 {
		c = 0.05
	}
	if c > 0.99 {
		c = 0.99
	}
	return float32(c)
}

// InferBDS scores a 56-bit Comm-B payload (Message.MB) against every
// supported register and returns the candidates, best first. altFt is the
// barometric altitude of the aircraft when known (the AC field of a DF20
// reply, or tracker state for DF21), NaN otherwise; it sharpens the BDS 6,0
// and meteorological tests considerably.
//
// An all-zero payload has no candidates. Decode calls this for every DF20/21
// reply and stores the result in Message.BDS.
func InferBDS(mb uint64, altFt float64) (c [MaxBDS]BDSCandidate, n int) {
	n = inferBDS(mb, altFt, &c)
	return c, n
}

func inferBDS(mb uint64, altFt float64, out *[MaxBDS]BDSCandidate) int {
	mb &= 1<<56 - 1
	if mb == 0 {
		return 0
	}
	n := 0
	add := func(b BDS, conf float32) {
		if conf <= 0 {
			return
		}
		// Insertion sort, best first; ties keep the order of the calls below.
		i := n
		if n < MaxBDS {
			n++
		} else if out[MaxBDS-1].Confidence >= conf {
			return
		} else {
			i = MaxBDS - 1
		}
		for i > 0 && out[i-1].Confidence < conf {
			out[i] = out[i-1]
			i--
		}
		out[i] = BDSCandidate{b, conf}
	}
	add(BDS10, scoreBDS10(mb))
	s17 := scoreBDS17(mb)
	add(BDS17, s17)
	add(BDS20, scoreBDS20(mb))
	add(BDS30, scoreBDS30(mb))
	add(BDS40, scoreBDS40(mb))
	add(BDS50, scoreBDS50(mb))
	add(BDS60, scoreBDS60(mb, altFt))
	add(BDS44, scoreBDS44(mb, altFt))
	if s17 == 0 {
		add(BDS45, scoreBDS45(mb, altFt))
	}
	return n
}

// scoreBDS10: data link capability report. The register number is in the
// first byte and bits 10-14 are reserved.
func scoreBDS10(mb uint64) float32 {
	if mb>>48 != 0x10 || field(mb, 10, 14) != 0 {
		return 0
	}
	// Subnetwork versions 0..5 are assigned (Doc 9871 edition 2). Allow for
	// a few future editions, but not for arbitrary numbers.
	ver := field(mb, 17, 23)
	switch {
	case ver > 15:
		return 0
	case ver > 5:
		return 0.55
	}
	// Consistency heuristic (not a rule of the ICAO text): the overlay
	// command capability only exists from Mode S subnetwork version 5 on,
	// so the two fields of a genuine BDS 1,0 agree.
	ovc := flag(mb, 15)
	if (ovc && ver < 5) || (!ovc && ver > 4) {
		return 0.7
	}
	return 0.95
}

// scoreBDS17: common usage GICB capability report. Bits 29-56 are reserved
// and BDS 2,0 (bit 7) is mandatory for every Mode S transponder.
func scoreBDS17(mb uint64) float32 {
	if !flag(mb, 7) || mb&(1<<28-1) != 0 {
		return 0
	}
	// Extended squitter capable transponders announce 0,5 0,6 0,8 and 0,9.
	if flag(mb, 1) && flag(mb, 2) && flag(mb, 4) && flag(mb, 5) {
		return 0.92
	}
	return 0.8
}

// scoreBDS20: aircraft identification.
func scoreBDS20(mb uint64) float32 {
	cs, ok := DecodeBDS20(mb)
	if !ok {
		return 0
	}
	// A real callsign is left justified without embedded spaces.
	end := 8
	for end > 0 && cs[end-1] == ' ' {
		end--
	}
	if end == 0 {
		return 0.5
	}
	for i := 0; i < end; i++ {
		if cs[i] == ' ' {
			return 0.7
		}
	}
	return 0.95
}

// scoreBDS30: ACAS active resolution advisory.
func scoreBDS30(mb uint64) float32 {
	if mb>>48 != 0x30 {
		return 0
	}
	// Register definition (Doc 9871, BDS 3,0): threat type 3 is not
	// assigned, and ARA bits 16-22 are reserved for ACAS III.
	tti := field(mb, 29, 30)
	if tti == 3 || field(mb, 16, 22) >= 48 {
		return 0
	}
	// A false resolution advisory is worse than a missed one, so the threat identity data must fit the declared threat type...
	switch tti {
	case 0: // no identity data
		if field(mb, 31, 56) != 0 {
			return 0.3
		}
	case 1: // 24-bit address in bits 31-54, bits 55-56 unused
		if field(mb, 55, 56) != 0 {
			return 0
		}
	case 2: // altitude, range, bearing
		if _, kind := decodeAC13(uint16(field(mb, 31, 43))); kind == altInvalid || kind == altMetric {
			return 0
		}
		if field(mb, 51, 56) > 60 {
			return 0
		}
	}
	// ...and without an advisory (first ARA bit and MTE clear) the other ARA
	// bits carry nothing, except in the "RA terminated" tail, which repeats
	// the last advisory for 18 s.
	if !flag(mb, 9) && !flag(mb, 28) && !flag(mb, 27) && field(mb, 10, 15) != 0 {
		return 0.3
	}
	return 0.85
}

// scoreBDS40: selected vertical intention.
func scoreBDS40(mb uint64) float32 {
	if wrongStatus(mb, 1, 2, 13) || wrongStatus(mb, 14, 15, 26) || wrongStatus(mb, 27, 28, 39) ||
		field(mb, 40, 47) != 0 || field(mb, 52, 53) != 0 ||
		wrongStatus(mb, 48, 49, 51) || wrongStatus(mb, 54, 55, 56) {
		return 0
	}
	v := DecodeBDS40(mb)
	n := 0
	conf := 0.6
	checkAlt := func(alt int32) {
		n++
		switch {
		case alt > 50000:
			conf -= 0.35
		case alt == 0:
			conf -= 0.2
		default:
			// Selected altitudes are dialled in 100 ft steps and sent with
			// 16 ft resolution.
			if r := alt % 100; r <= 8 || r >= 92 {
				conf += 0.05
			} else {
				conf -= 0.1
			}
		}
	}
	if v.MCPAltOK {
		checkAlt(v.MCPAlt)
	}
	if v.FMSAltOK {
		checkAlt(v.FMSAlt)
	}
	if v.BaroOK {
		n++
		if v.Baro >= 900 && v.Baro <= 1090 {
			conf += 0.05
		} else {
			conf -= 0.25
		}
	}
	if n == 0 {
		return 0 // mode bits alone are not enough evidence
	}
	return clampConf(conf + 0.1*float64(n-1))
}

// scoreBDS50: track and turn report.
func scoreBDS50(mb uint64) float32 {
	if wrongStatus(mb, 1, 2, 11) || wrongStatus(mb, 12, 13, 23) || wrongStatus(mb, 24, 25, 34) ||
		wrongStatus(mb, 35, 36, 45) || wrongStatus(mb, 46, 47, 56) {
		return 0
	}
	v := DecodeBDS50(mb)
	n := 0
	conf := 0.3
	if v.RollOK {
		n++
		a := math.Abs(v.Roll)
		if a > 50 {
			return 0
		}
		if a > 35 {
			conf -= 0.2
		}
	}
	if v.TrackOK {
		n++
	}
	if v.GSOK {
		n++
		if v.GS > 600 {
			return 0
		}
		if v.GS < 40 {
			conf -= 0.1
		}
	}
	if v.TrackRateOK {
		n++
	}
	if v.TASOK {
		n++
		if v.TAS > 600 {
			return 0
		}
		if v.TAS > 500 || v.TAS < 40 {
			conf -= 0.15
		}
	}
	if n == 0 {
		return 0
	}
	if v.GSOK && v.TASOK {
		d := math.Abs(v.TAS - v.GS)
		if d > 200 {
			return 0
		}
		if d > 150 {
			conf -= 0.1
		} else {
			conf += 0.05
		}
	}
	// Coordinated turn: rate of turn [deg/s] = 1091 * tan(roll) / TAS [kt].
	if v.RollOK && v.TrackRateOK && v.TASOK && v.TAS >= 40 {
		want := 1091 * math.Tan(v.Roll*(math.Pi/180)) / v.TAS
		d := math.Abs(v.TrackRate - want)
		switch {
		case d <= 0.5+0.25*math.Abs(want):
			conf += 0.1
		case d > 2:
			conf -= 0.2
		}
	}
	return clampConf(conf + 0.1*float64(n))
}

// scoreBDS60: heading and speed report.
func scoreBDS60(mb uint64, altFt float64) float32 {
	if wrongStatus(mb, 1, 2, 12) || wrongStatus(mb, 13, 14, 23) || wrongStatus(mb, 24, 25, 34) ||
		wrongStatus(mb, 35, 36, 45) || wrongStatus(mb, 46, 47, 56) {
		return 0
	}
	v := DecodeBDS60(mb)
	n := 0
	conf := 0.3
	if v.HeadingOK {
		n++
	}
	if v.IASOK {
		n++
		if v.IAS > 500 {
			return 0
		}
		if v.IAS < 50 {
			conf -= 0.15
		}
	}
	if v.MachOK {
		n++
		if v.Mach > 1 {
			return 0
		}
	}
	if v.BaroRateOK {
		n++
		if v.BaroRate > 6000 || v.BaroRate < -6000 {
			return 0
		}
	}
	if v.InertialRateOK {
		n++
		if v.InertialRate > 6000 || v.InertialRate < -6000 {
			return 0
		}
	}
	if n == 0 {
		return 0
	}
	if v.IASOK && v.MachOK {
		if known(altFt) {
			// Mach and IAS must describe the same dynamic pressure.
			d := math.Abs(v.IAS - machToCAS(v.Mach, altFt))
			if d > 20 {
				return 0
			}
			if d <= 10 {
				conf += 0.15
			} else {
				conf += 0.05
			}
		} else {
			// Without altitude the pair must at least fit somewhere between
			// 1000 ft below sea level and 60000 ft.
			if v.IAS > machToCAS(v.Mach, -1000)+20 || v.IAS < machToCAS(v.Mach, 60000)-20 {
				return 0
			}
			conf += 0.05
		}
	}
	if v.BaroRateOK && v.InertialRateOK {
		d := v.BaroRate - v.InertialRate
		if d < 0 {
			d = -d
		}
		switch {
		case d <= 800:
			conf += 0.05
		case d > 2500:
			conf -= 0.15
		}
	}
	return clampConf(conf + 0.1*float64(n))
}

// scoreBDS44: meteorological routine air report. Rare in practice, so the
// base confidence is low unless the temperature fits the altitude.
func scoreBDS44(mb uint64, altFt float64) float32 {
	fom := field(mb, 1, 4)
	if fom > 4 || !flag(mb, 5) ||
		wrongStatus(mb, 35, 36, 46) || wrongStatus(mb, 47, 48, 49) || wrongStatus(mb, 50, 51, 56) {
		return 0
	}
	v := DecodeBDS44(mb)
	if v.WindSpeed > 250 || v.Temp > 60 || v.Temp < -80 {
		return 0
	}
	if v.WindSpeed == 0 && v.WindDir == 0 && field(mb, 24, 34) == 0 {
		return 0
	}
	conf := 0.45
	if fom == 0 {
		conf -= 0.15
	}
	if known(altFt) {
		switch d := math.Abs(v.Temp - isaTempC(altFt)); {
		case d <= 20:
			conf += 0.3
		case d <= 30:
			conf += 0.1
		case d > 40:
			conf -= 0.3
		}
		if v.PressureOK {
			switch d := math.Abs(v.Pressure - 1013.25*isaDelta(altFt)); {
			case d <= 15:
				conf += 0.1
			case d > 60:
				conf -= 0.3
			}
		}
	}
	return clampConf(conf)
}

// scoreBDS45: meteorological hazard report. Very rare; never scored above
// "plausible" on its own.
func scoreBDS45(mb uint64, altFt float64) float32 {
	if field(mb, 52, 56) != 0 ||
		wrongStatus(mb, 1, 2, 3) || wrongStatus(mb, 4, 5, 6) || wrongStatus(mb, 7, 8, 9) ||
		wrongStatus(mb, 10, 11, 12) || wrongStatus(mb, 13, 14, 15) || wrongStatus(mb, 16, 17, 26) ||
		wrongStatus(mb, 27, 28, 38) || wrongStatus(mb, 39, 40, 51) {
		return 0
	}
	v := DecodeBDS45(mb)
	if v.TempOK && (v.Temp > 60 || v.Temp < -80) {
		return 0
	}
	conf := 0.3
	severe := 0
	for _, h := range [...]struct {
		ok    bool
		level uint8
	}{
		{v.TurbulenceOK, v.Turbulence}, {v.WindShearOK, v.WindShear}, {v.MicroburstOK, v.Microburst},
		{v.IcingOK, v.Icing}, {v.WakeVortexOK, v.WakeVortex},
	} {
		if h.ok && h.level == 3 {
			severe++
		}
	}
	if severe > 2 {
		conf -= 0.15
	}
	if known(altFt) {
		if v.TempOK {
			switch d := math.Abs(v.Temp - isaTempC(altFt)); {
			case d <= 20:
				conf += 0.25
			case d > 40:
				conf -= 0.2
			}
		}
		if v.PressureOK {
			switch d := math.Abs(v.Pressure - 1013.25*isaDelta(altFt)); {
			case d <= 15:
				conf += 0.1
			case d > 60:
				conf -= 0.2
			}
		}
		if v.RadioHeightOK && float64(v.RadioHeight) > altFt+2000 {
			conf -= 0.2
		}
	}
	if conf > 0.6 {
		conf = 0.6
	}
	return clampConf(conf)
}

// Disambiguation thresholds.
const (
	bdsAcceptConfidence = 0.5  // minimum confidence of the winner
	bdsAcceptMargin     = 0.15 // minimum lead over the runner-up
)

// BestBDS returns the most likely register of a DF20/21 reply using only the
// information in the message itself. ok is false when there is no candidate,
// the best one is weak or two candidates are too close to call; use
// DisambiguateBDS with tracker state in that case.
func (m *Message) BestBDS() (b BDS, confidence float32, ok bool) {
	return pickBDS(&m.BDS, int(m.NumBDS))
}

func pickBDS(c *[MaxBDS]BDSCandidate, n int) (BDS, float32, bool) {
	if n == 0 {
		return BDSNone, 0, false
	}
	best := c[0]
	if best.Confidence < bdsAcceptConfidence {
		return best.BDS, best.Confidence, false
	}
	if n > 1 && best.Confidence-c[1].Confidence < bdsAcceptMargin {
		return best.BDS, best.Confidence, false
	}
	return best.BDS, best.Confidence, true
}

// DisambiguateBDS decides which register a DF20/21 reply carries, using what
// the tracker already knows about the aircraft from other messages:
//
//   - gs, track: ground speed (kt) and true track (degrees), normally from
//     ADS-B velocity messages no more than a few seconds old;
//   - altFt: barometric altitude (ft). When NaN the altitude of the reply
//     itself is used if it has one (DF20).
//
// Pass NaN for anything unknown. The checks compare each interpretation
// with the physics of the known state (the reasoning the 1090 MHz Riddle
// describes for telling BDS 5,0 and 6,0 apart):
//
//   - BDS 5,0 must reproduce the known ground velocity vector; a close match
//     confirms it, a large mismatch rules it out.
//   - BDS 6,0 must have Mach and IAS that agree at the known altitude, and
//     the wind implied by its airspeed/heading against the known ground
//     velocity must be believable.
//   - If the payload is still both a valid BDS 5,0 and a valid BDS 6,0, the
//     one whose velocity vector is nearer to the known ground velocity wins.
//   - BDS 4,4 / 4,5 temperatures are compared with the standard atmosphere.
//
// It returns the winning register and its confidence. ok is false when no
// candidate is convincing (confidence below 0.5) or the two best are within
// 0.15 of each other; the reply should then be ignored. The message is not
// modified.
func DisambiguateBDS(m *Message, gs, track, altFt float64) (b BDS, confidence float32, ok bool) {
	c, n := RankBDS(m, gs, track, altFt)
	return pickBDS(&c, n)
}

// RankBDS is DisambiguateBDS without the final decision: it returns all
// candidates re-scored against the known state, best first.
func RankBDS(m *Message, gs, track, altFt float64) (c [MaxBDS]BDSCandidate, n int) {
	if m.Valid&FCommB == 0 {
		return c, 0
	}
	mb := m.payload()
	if !known(altFt) && m.Valid&FAltBaro != 0 {
		altFt = float64(m.AltBaro)
	}
	n = inferBDS(mb, altFt, &c)
	if n == 0 {
		return c, 0
	}
	for i := 0; i < n; i++ {
		conf := float64(c[i].Confidence)
		switch c[i].BDS {
		case BDS50:
			conf = rescoreBDS50(mb, conf, gs, track)
		case BDS60:
			conf = rescoreBDS60(mb, conf, gs, track, altFt)
		}
		if conf <= 0 {
			c[i].Confidence = 0
		} else {
			c[i].Confidence = clampConf(conf)
		}
	}
	preferNearestVelocity(mb, &c, n, gs, track, altFt)
	// Re-sort (stable insertion sort) and drop eliminated candidates.
	for i := 1; i < n; i++ {
		for j := i; j > 0 && c[j-1].Confidence < c[j].Confidence; j-- {
			c[j-1], c[j] = c[j], c[j-1]
		}
	}
	for n > 0 && c[n-1].Confidence <= 0 {
		n--
		c[n] = BDSCandidate{}
	}
	return c, n
}

// bds5060Gap is the minimum confidence lead given to the register whose
// velocity vector is nearer to the known ground velocity when both BDS 5,0
// and BDS 6,0 survive every other test. It exceeds bdsAcceptMargin so that
// the comparison settles the question.
const bds5060Gap = 0.2

// preferNearestVelocity settles a payload that is still both a valid BDS 5,0
// and a valid BDS 6,0: an aircraft's air vector differs from its ground
// vector only by the wind, so the interpretation
// whose horizontal velocity vector (ground speed/track for 5,0, true
// airspeed/heading for 6,0) lies nearer to the known ground velocity wins.
// It needs the ground velocity, and for 6,0 the altitude to turn Mach or IAS
// into true airspeed; without them the confidences are left alone.
func preferNearestVelocity(mb uint64, c *[MaxBDS]BDSCandidate, n int, gs, track, altFt float64) {
	if !known(gs) || !known(track) || !known(altFt) {
		return
	}
	i50, i60 := -1, -1
	for i := 0; i < n; i++ {
		if c[i].Confidence <= 0 {
			continue
		}
		switch c[i].BDS {
		case BDS50:
			i50 = i
		case BDS60:
			i60 = i
		}
	}
	if i50 < 0 || i60 < 0 {
		return
	}
	v50 := DecodeBDS50(mb)
	v60 := DecodeBDS60(mb)
	if !v50.GSOK || !v50.TrackOK || !v60.HeadingOK {
		return
	}
	var tas float64
	switch {
	case v60.MachOK:
		tas = machToTAS(v60.Mach, altFt)
	case v60.IASOK:
		tas = machToTAS(casToMach(v60.IAS, altFt), altFt)
	default:
		return
	}
	d50 := velocityDiff(v50.GS, v50.Track, gs, track)
	d60 := velocityDiff(tas, v60.MagHeading, gs, track)
	win, lose := i50, i60
	if d60 < d50 {
		win, lose = i60, i50
	}
	if limit := c[win].Confidence - bds5060Gap; c[lose].Confidence > limit {
		if limit < 0.05 {
			limit = 0.05
		}
		c[lose].Confidence = limit
	}
}

// velocityDiff returns the magnitude of the difference of two horizontal
// velocity vectors given as speed (kt) and direction (degrees).
func velocityDiff(v1, dir1, v2, dir2 float64) float64 {
	s1, c1 := math.Sincos(dir1 * (math.Pi / 180))
	s2, c2 := math.Sincos(dir2 * (math.Pi / 180))
	return math.Hypot(v1*s1-v2*s2, v1*c1-v2*c2)
}

// angleDiff returns the absolute difference of two directions in degrees.
func angleDiff(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	if d > 180 {
		d = 360 - d
	}
	return d
}

func rescoreBDS50(mb uint64, conf, gs, track float64) float64 {
	v := DecodeBDS50(mb)
	switch {
	case known(gs) && v.GSOK:
		// Both values come from the same navigation source, so they agree to
		// a few knots; allow for staleness during acceleration and turns.
		d := math.Abs(v.GS - gs)
		if known(track) && v.TrackOK && gs >= 50 {
			d = velocityDiff(v.GS, v.Track, gs, track)
		}
		switch {
		case d <= 25:
			conf += 0.25
		case d <= 50:
			// neither confirmed nor refuted
		case d <= 100:
			conf -= 0.4
		default:
			return 0
		}
	case known(track) && v.TrackOK:
		switch d := angleDiff(v.Track, track); {
		case d <= 10:
			conf += 0.1
		case d > 45:
			conf -= 0.3
		}
	}
	return conf
}

func rescoreBDS60(mb uint64, conf, gs, track, altFt float64) float64 {
	if !known(gs) {
		return conf
	}
	v := DecodeBDS60(mb)
	// Best estimate of true airspeed from the register.
	tas := unknown
	switch {
	case known(altFt) && v.MachOK:
		tas = machToTAS(v.Mach, altFt)
	case known(altFt) && v.IASOK:
		tas = machToTAS(casToMach(v.IAS, altFt), altFt)
	case v.IASOK:
		// IAS never exceeds TAS (above sea level), and TAS cannot exceed
		// ground speed by more than the strongest winds.
		if v.IAS > gs+200 {
			return conf - 0.4
		}
		return conf
	}
	if !known(tas) {
		return conf
	}
	// The difference between air and ground velocity is the wind. Magnetic
	// variation and non-standard temperature blur the estimate, so only
	// clearly impossible winds count against the register.
	wind := math.Abs(tas - gs)
	if known(track) && v.HeadingOK && gs >= 50 {
		wind = velocityDiff(tas, v.MagHeading, gs, track)
	}
	switch {
	case wind > 300:
		return 0
	case wind > 220:
		conf -= 0.4
	case wind <= 120:
		conf += 0.1
	}
	return conf
}
