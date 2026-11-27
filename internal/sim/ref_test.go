package sim

// Independent reference implementations used by the tests. Nothing in this
// file calls the simulator's encoder: the CRC is a bit-serial polynomial
// division (the encoder uses a byte table), fields are read with a generic
// bit extractor (the encoder packs with shifts), and the CPR decoder follows
// the published global decoding algorithm ("The 1090 Megahertz Riddle",
// https://mode-s.org/1090mhz/).

import (
	"math"
	"strings"
)

// refRemainder divides the complete message (data + parity) by the Mode S
// generator polynomial 0x1FFF409, one bit at a time, and returns the 24-bit
// remainder: 0 for an intact DF11 (II = 0) / DF17 / DF18 message, the ICAO
// address for an intact address-parity message (DF0/4/5/16/20/21).
func refRemainder(msg []byte) uint32 {
	const generator = 0x1FFF409 // 25 bits
	buf := append([]byte(nil), msg...)
	nbits := len(buf) * 8
	for i := 0; i < nbits-24; i++ {
		if buf[i/8]&(0x80>>(i%8)) == 0 {
			continue
		}
		for j := 0; j < 25; j++ {
			if generator&(1<<(24-j)) != 0 {
				k := i + j
				buf[k/8] ^= 0x80 >> (k % 8)
			}
		}
	}
	n := len(buf)
	return uint32(buf[n-3])<<16 | uint32(buf[n-2])<<8 | uint32(buf[n-1])
}

// rb returns bits first..last (1-based, inclusive, bit 1 = MSB of byte 0).
func rb(b []byte, first, last int) uint64 {
	var v uint64
	for i := first; i <= last; i++ {
		v <<= 1
		if b[(i-1)/8]&(0x80>>((i-1)%8)) != 0 {
			v |= 1
		}
	}
	return v
}

// rsigned interprets bits first..last as a two's complement number.
func rsigned(b []byte, first, last int) float64 {
	n := last - first + 1
	v := int64(rb(b, first, last))
	if v&(1<<(n-1)) != 0 {
		v -= 1 << n
	}
	return float64(v)
}

func refDF(msg []byte) int { return int(msg[0] >> 3) }

func refICAO(msg []byte) uint32 { return uint32(rb(msg, 9, 32)) }

// refAlt12 decodes the 12-bit altitude field of an airborne position message.
func refAlt12(code uint64) (ft float64, ok bool) {
	if code == 0 || code&0x10 == 0 {
		return 0, false // not available / Gillham coded
	}
	n := (code&0xFE0)>>1 | code&0x00F
	return float64(n)*25 - 1000, true
}

// refAlt13 decodes the 13-bit altitude code of DF0/4/16/20.
func refAlt13(code uint64) (ft float64, ok bool) {
	if code == 0 || code&0x40 != 0 || code&0x10 == 0 {
		return 0, false // not available / metric / Gillham coded
	}
	n := (code&0x1F80)>>2 | (code&0x0020)>>1 | code&0x000F
	return float64(n)*25 - 1000, true
}

// refSquawk decodes the 13-bit identity code C1 A1 C2 A2 C4 A4 X B1 D1 B2 D2 B4 D4.
func refSquawk(id uint64) string {
	bit := func(n int) byte { return byte(id >> (13 - n) & 1) } // n = 1..13 from the MSB
	a := bit(6)<<2 | bit(4)<<1 | bit(2)
	b := bit(12)<<2 | bit(10)<<1 | bit(8)
	c := bit(5)<<2 | bit(3)<<1 | bit(1)
	d := bit(13)<<2 | bit(11)<<1 | bit(9)
	return string([]byte{'0' + a, '0' + b, '0' + c, '0' + d})
}

const refCharset = "#ABCDEFGHIJKLMNOPQRSTUVWXYZ##### ###############0123456789######"

// refCallsign decodes eight 6-bit characters starting at bit first.
func refCallsign(b []byte, first int) string {
	var sb strings.Builder
	for i := 0; i < 8; i++ {
		sb.WriteByte(refCharset[rb(b, first+6*i, first+6*i+5)])
	}
	return strings.TrimRight(sb.String(), " ")
}

func refNL(lat float64) int {
	lat = math.Abs(lat)
	switch {
	case lat == 0:
		return 59
	case lat == 87:
		return 2
	case lat > 87:
		return 1
	}
	const nz = 15.0
	a := 1 - math.Cos(math.Pi/(2*nz))
	b := math.Pow(math.Cos(math.Pi/180*lat), 2)
	return int(math.Floor(2 * math.Pi / math.Acos(1-a/b)))
}

func fmod(a, b float64) float64 { return a - b*math.Floor(a/b) }

// refCPRAirborne is the globally unambiguous airborne CPR decode of an even
// and an odd 17-bit pair; oddNewest selects which message the result is for.
// ok is false when the pair straddles a longitude zone boundary.
func refCPRAirborne(latE, lonE, latO, lonO uint64, oddNewest bool) (lat, lon float64, ok bool) {
	const scale = 131072.0
	ce, co := float64(latE)/scale, float64(latO)/scale
	xe, xo := float64(lonE)/scale, float64(lonO)/scale
	j := math.Floor(59*ce - 60*co + 0.5)
	le := 360.0 / 60 * (fmod(j, 60) + ce)
	lo := 360.0 / 59 * (fmod(j, 59) + co)
	if le >= 270 {
		le -= 360
	}
	if lo >= 270 {
		lo -= 360
	}
	if le < -90 || le > 90 || lo < -90 || lo > 90 || refNL(le) != refNL(lo) {
		return 0, 0, false
	}
	lat = le
	if oddNewest {
		lat = lo
	}
	nl := float64(refNL(lat))
	m := math.Floor(xe*(nl-1) - xo*nl + 0.5)
	if oddNewest {
		n := math.Max(nl-1, 1)
		lon = 360 / n * (fmod(m, n) + xo)
	} else {
		n := math.Max(nl, 1)
		lon = 360 / n * (fmod(m, n) + xe)
	}
	if lon >= 180 {
		lon -= 360
	}
	return lat, lon, true
}

// refCPRSurface is the global surface CPR decode. Surface positions are only
// unambiguous within a quarter of the globe, so the solution closest to the
// reference position (the receiver) is chosen.
func refCPRSurface(latE, lonE, latO, lonO uint64, oddNewest bool, refLat, refLon float64) (lat, lon float64, ok bool) {
	const scale = 131072.0
	ce, co := float64(latE)/scale, float64(latO)/scale
	xe, xo := float64(lonE)/scale, float64(lonO)/scale
	j := math.Floor(59*ce - 60*co + 0.5)
	le := 90.0 / 60 * (fmod(j, 60) + ce)
	lo := 90.0 / 59 * (fmod(j, 59) + co)
	// northern or southern solution, whichever is closer to the reference
	if math.Abs(le-90-refLat) < math.Abs(le-refLat) {
		le -= 90
	}
	if math.Abs(lo-90-refLat) < math.Abs(lo-refLat) {
		lo -= 90
	}
	if refNL(le) != refNL(lo) {
		return 0, 0, false
	}
	lat = le
	if oddNewest {
		lat = lo
	}
	nl := float64(refNL(lat))
	m := math.Floor(xe*(nl-1) - xo*nl + 0.5)
	if oddNewest {
		n := math.Max(nl-1, 1)
		lon = 90 / n * (fmod(m, n) + xo)
	} else {
		n := math.Max(nl, 1)
		lon = 90 / n * (fmod(m, n) + xe)
	}
	best, bestD := lon, math.Inf(1)
	for k := 0; k < 4; k++ {
		c := lon + 90*float64(k)
		d := math.Abs(math.Mod(c-refLon+540, 360) - 180)
		if d < bestD {
			best, bestD = c, d
		}
	}
	lon = best
	if lon >= 180 {
		lon -= 360
	}
	return lat, lon, true
}

// refDistM is the great-circle distance in metres (haversine, mean earth
// radius), written independently of the simulator's geometry helpers.
func refDistM(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371008.8
	rad := math.Pi / 180
	dp := (lat2 - lat1) * rad
	dl := (lon2 - lon1) * rad
	h := math.Pow(math.Sin(dp/2), 2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Pow(math.Sin(dl/2), 2)
	return 2 * r * math.Asin(math.Sqrt(math.Min(1, h)))
}

// refBearing is the initial great-circle bearing from point 1 to point 2.
func refBearing(lat1, lon1, lat2, lon2 float64) float64 {
	rad := math.Pi / 180
	y := math.Sin((lon2-lon1)*rad) * math.Cos(lat2*rad)
	x := math.Cos(lat1*rad)*math.Sin(lat2*rad) - math.Sin(lat1*rad)*math.Cos(lat2*rad)*math.Cos((lon2-lon1)*rad)
	return fmod(math.Atan2(y, x)/rad, 360)
}

// refVelocity decodes an airborne velocity ME field (TC 19 subtype 1).
type refVel struct {
	ew, ns   float64 // kt, + east / + north
	vr       float64 // ft/min
	vrBaro   bool
	nacv     int
	hasDelta bool
	delta    float64 // GNSS minus baro, ft
}

func refVelocity(me []byte) (v refVel, ok bool) {
	if rb(me, 1, 5) != 19 || rb(me, 6, 8) != 1 {
		return v, false
	}
	vew, vns := rb(me, 15, 24), rb(me, 26, 35)
	if vew == 0 || vns == 0 {
		return v, false
	}
	v.nacv = int(rb(me, 11, 13))
	v.ew = float64(vew - 1)
	if rb(me, 14, 14) == 1 {
		v.ew = -v.ew
	}
	v.ns = float64(vns - 1)
	if rb(me, 25, 25) == 1 {
		v.ns = -v.ns
	}
	v.vrBaro = rb(me, 36, 36) == 1
	if r := rb(me, 38, 46); r != 0 {
		v.vr = float64(r-1) * 64
		if rb(me, 37, 37) == 1 {
			v.vr = -v.vr
		}
	}
	if d := rb(me, 50, 56); d != 0 {
		v.hasDelta = true
		v.delta = float64(d-1) * 25
		if rb(me, 49, 49) == 1 {
			v.delta = -v.delta
		}
	}
	return v, true
}

// refMovement returns the ground speed range [lo, hi) in knots of a surface
// movement code.
func refMovement(code uint64) (lo, hi float64, ok bool) {
	switch {
	case code == 1:
		return 0, 0.125, true
	case code >= 2 && code <= 8:
		lo = 0.125 + float64(code-2)*0.125
		return lo, lo + 0.125, true
	case code >= 9 && code <= 12:
		lo = 1 + float64(code-9)*0.25
		return lo, lo + 0.25, true
	case code >= 13 && code <= 38:
		lo = 2 + float64(code-13)*0.5
		return lo, lo + 0.5, true
	case code >= 39 && code <= 93:
		lo = 15 + float64(code-39)
		return lo, lo + 1, true
	case code >= 94 && code <= 108:
		lo = 70 + float64(code-94)*2
		return lo, lo + 2, true
	case code >= 109 && code <= 123:
		lo = 100 + float64(code-109)*5
		return lo, lo + 5, true
	case code == 124:
		return 175, math.Inf(1), true
	}
	return 0, 0, false
}

// angDiff is the absolute difference of two angles in degrees (0..180).
func angDiff(a, b float64) float64 {
	return math.Abs(math.Mod(a-b+540, 360) - 180)
}

// Comm-B registers, decoded per ICAO Doc 9871 as tabulated in the 1090 MHz
// Riddle. Each decoder reports ok = false when a status bit is missing or a
// reserved bit is set.

type refBDS40 struct {
	mcpAlt, baro            float64
	vnav, altHold, approach bool
	source                  int
}

func refDecode40(mb []byte) (r refBDS40, ok bool) {
	if rb(mb, 1, 1) != 1 || rb(mb, 14, 14) != 0 || rb(mb, 15, 26) != 0 || rb(mb, 27, 27) != 1 ||
		rb(mb, 40, 47) != 0 || rb(mb, 48, 48) != 1 || rb(mb, 52, 53) != 0 || rb(mb, 54, 54) != 1 {
		return r, false
	}
	r.mcpAlt = float64(rb(mb, 2, 13)) * 16
	r.baro = float64(rb(mb, 28, 39))*0.1 + 800
	r.vnav = rb(mb, 49, 49) == 1
	r.altHold = rb(mb, 50, 50) == 1
	r.approach = rb(mb, 51, 51) == 1
	r.source = int(rb(mb, 55, 56))
	return r, true
}

type refBDS50 struct{ roll, track, gs, trackRate, tas float64 }

func refDecode50(mb []byte) (r refBDS50, ok bool) {
	if rb(mb, 1, 1) != 1 || rb(mb, 12, 12) != 1 || rb(mb, 24, 24) != 1 || rb(mb, 35, 35) != 1 || rb(mb, 46, 46) != 1 {
		return r, false
	}
	r.roll = rsigned(mb, 2, 11) * 45 / 256
	r.track = fmod(rsigned(mb, 13, 23)*90/512, 360)
	r.gs = float64(rb(mb, 25, 34)) * 2
	r.trackRate = rsigned(mb, 36, 45) * 8 / 256
	r.tas = float64(rb(mb, 47, 56)) * 2
	return r, true
}

type refBDS60 struct{ hdg, ias, mach, baroRate, inertialRate float64 }

func refDecode60(mb []byte) (r refBDS60, ok bool) {
	if rb(mb, 1, 1) != 1 || rb(mb, 13, 13) != 1 || rb(mb, 24, 24) != 1 || rb(mb, 35, 35) != 1 || rb(mb, 46, 46) != 1 {
		return r, false
	}
	r.hdg = fmod(rsigned(mb, 2, 12)*90/512, 360)
	r.ias = float64(rb(mb, 14, 23))
	r.mach = float64(rb(mb, 25, 34)) * 0.004
	r.baroRate = rsigned(mb, 36, 45) * 32
	r.inertialRate = rsigned(mb, 47, 56) * 32
	return r, true
}

type refBDS44 struct{ windKt, windDir, sat, press float64 }

func refDecode44(mb []byte) (r refBDS44, ok bool) {
	if rb(mb, 1, 4) != 1 || rb(mb, 5, 5) != 1 || rb(mb, 35, 35) != 1 || rb(mb, 47, 56) != 0 {
		return r, false
	}
	r.windKt = float64(rb(mb, 6, 14))
	r.windDir = float64(rb(mb, 15, 23)) * 180 / 256
	r.sat = rsigned(mb, 24, 34) * 0.25
	r.press = float64(rb(mb, 36, 46))
	return r, true
}

// refBeastFrame is one frame of a Beast binary stream.
type refBeastFrame struct {
	typ  byte
	ts   uint64 // 48-bit timestamp (types 1-4)
	sig  byte
	data []byte
}

// refParseBeast is an independent Beast stream parser: 0x1A, type, body with
// doubled 0x1A. It returns the complete frames and the unparsed tail.
func refParseBeast(buf []byte) (frames []refBeastFrame, rest []byte, bad int) {
	i := 0
	for i < len(buf) {
		if buf[i] != 0x1A {
			bad++
			i++
			continue
		}
		if i+1 >= len(buf) {
			break
		}
		var n int
		switch buf[i+1] {
		case '1':
			n = 6 + 1 + 2
		case '2':
			n = 6 + 1 + 7
		case '3', '4', '5':
			n = 6 + 1 + 14
		default:
			bad++
			i++
			continue
		}
		body := make([]byte, 0, n)
		k := i + 2
		complete := true
		for len(body) < n {
			if k >= len(buf) {
				complete = false
				break
			}
			c := buf[k]
			if c == 0x1A {
				if k+1 >= len(buf) {
					complete = false
					break
				}
				if buf[k+1] != 0x1A {
					break // unescaped 0x1A inside a body: framing error
				}
				k++
			}
			body = append(body, c)
			k++
		}
		if !complete {
			break
		}
		if len(body) < n {
			bad++
			i = k
			continue
		}
		f := refBeastFrame{typ: buf[i+1]}
		if f.typ == '5' {
			f.data = body
		} else {
			for _, c := range body[:6] {
				f.ts = f.ts<<8 | uint64(c)
			}
			f.sig = body[6]
			f.data = body[7:]
		}
		frames = append(frames, f)
		i = k
	}
	return frames, buf[i:], bad
}
