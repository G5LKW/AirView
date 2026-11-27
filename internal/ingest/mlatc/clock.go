package mlatc

import (
	"strings"

	"airview/internal/model"
)

// tickBase says how the client's "t" values are counted (MLATC.md §5).
type tickBase uint8

const (
	base12  tickBase = iota // 12 MHz counter (48 bit)
	baseGPS                 // 1 GHz, nanoseconds of the UTC day
	baseSBS                 // 20 MHz counter, widened by the client
)

// Frame reference offsets the reference client removes from every
// timestamp so that it points at the start of the frame (MLATC.md §5,
// facts confirmed under CLEANROOM rule 4). The server adds them back, so a
// timestamp from 31090 means exactly what the same receiver's raw stamp
// means on AVF/1: the clock filter applies one FrameRef table to both.
const (
	off12ModeS  = 768    // 12 MHz ticks (64 µs), short and long Mode S
	off12ModeAC = 244    // 12 MHz ticks (≈ 20.3 µs), Mode A/C
	offGPSShort = 64000  // ns
	offGPSLong  = 120000 // ns
	offGPSAC    = 20300  // ns
	// The SBS reader adds instead of subtracting.
	offSBSShort = 1120 // 20 MHz ticks (7 × 160)
	offSBSAC    = 1920 // 20 MHz ticks (12 × 160)
)

const mask48 = 1<<48 - 1

// maxSBSTicks bounds the client's widened SBS counter: 2^56 ticks of
// 20 MHz are 114 years, far beyond any real uptime, and keep the
// plausibility arithmetic of normalise away from the uint64 limit.
const maxSBSTicks = 1 << 56

// classifyClock maps a handshake clock_type to a class and tick base
// (DESIGN §5.2 table). Unknown, "auto" and "dump1090" are free-running SDR
// 12 MHz clocks.
func classifyClock(clockType string) (model.ClockClass, tickBase) {
	switch strings.ToLower(clockType) {
	case "radarcape_gps":
		return model.ClockGPSUTC, baseGPS
	case "beast", "radarcape_12mhz":
		return model.ClockHW12, base12
	case "sbs":
		return model.ClockSBS20, baseSBS
	}
	return model.ClockSDR12, base12
}

// modeChange works out the class after a clock_reset that names a decoder
// mode and/or a frequency. handshakeType is the client's clock_type (it
// decides whether a 12 MHz mode is hardware or SDR). ok is false when the
// message says nothing usable.
func modeChange(handshakeType, mode string, freq float64, epoch string) (model.ClockClass, tickBase, bool) {
	m := strings.ToLower(mode)
	hw := false
	switch strings.ToLower(handshakeType) {
	case "beast", "radarcape_12mhz", "radarcape_gps":
		hw = true
	}
	twelve := func() (model.ClockClass, tickBase, bool) {
		if hw {
			return model.ClockHW12, base12, true
		}
		return model.ClockSDR12, base12, true
	}
	// AirView's own class names (simnet and AirView feeders) are exact.
	if c, ok := model.ParseClockClass(m); ok && c.Timed() {
		switch {
		case c.GPS():
			return c, baseGPS, true
		case c == model.ClockSBS20:
			return c, baseSBS, true
		}
		return c, base12, true
	}
	switch {
	case strings.Contains(m, "emulated"):
		return model.ClockGPSEmul, baseGPS, true
	case strings.Contains(m, "12mhz"):
		return twelve()
	case strings.Contains(m, "radarcape"), strings.Contains(m, "gps"):
		return model.ClockGPSUTC, baseGPS, true
	case strings.Contains(m, "sbs"):
		return model.ClockSBS20, baseSBS, true
	case strings.Contains(m, "beast"), strings.Contains(m, "dump1090"), strings.Contains(m, "avr"):
		return twelve()
	}
	switch {
	case freq > 0.99e9 && freq < 1.01e9:
		if strings.Contains(strings.ToLower(epoch), "utc") {
			return model.ClockGPSUTC, baseGPS, true
		}
		return model.ClockGPSEmul, baseGPS, true
	case freq > 11.9e6 && freq < 12.1e6:
		return twelve()
	case freq > 19.9e6 && freq < 20.1e6:
		return model.ClockSBS20, baseSBS, true
	}
	return 0, 0, false
}

// Epoch-break thresholds of the server's own plausibility check (the client
// reports jumps itself; this catches clients that do not). Sync messages
// carry the earlier half of a pair, so ticks legitimately go back by up to
// the pairing window (5 s in the reference client).
const (
	backStepS = 30 // a step back of more than this many seconds of clock
	fwdStepS  = 90 // a step forward of more than elapsed + this (model.EpochStepLimit)
)

// clockState turns one client's "t" values into AirView ticks (MLATC.md §5).
type clockState struct {
	cls  model.ClockClass
	base tickBase
	hz   float64
	// raw: the client sends receiver stamps unchanged (AirView's simulator
	// and test clients), no frame-start offsets to undo.
	raw   bool
	epoch uint32

	have     bool   // max is valid in this epoch
	max      uint64 // largest normalised tick of the epoch
	maxSrvNs int64  // server time when max was seen
}

func (c *clockState) setClass(cls model.ClockClass, base tickBase) {
	c.cls, c.base, c.hz = cls, base, cls.NominalHz()
	c.have = false
}

// stepReason classifies an epoch break found by normalise.
type stepReason uint8

const (
	stepNone stepReason = iota
	stepBack
	stepJump
)

// normalise converts the client value t of a frame of n payload bytes,
// received at server time srvNs, into ticks of the AirView convention:
// 12 MHz: the raw 48-bit counter unwrapped; GPS: dayIndex·86400e9 +
// nanoseconds of the day at the receiver's stamping point (model.GPSTicks
// with the server clock choosing the day); SBS: the client's widened 20 MHz
// counter at the stamping point. ok is false for unusable values (0, or an
// offset underflow). step reports an implausible step: the caller starts a
// new epoch and calls normalise again.
func (c *clockState) normalise(t uint64, n int, srvNs int64) (ticks uint64, step stepReason, ok bool) {
	if t == 0 {
		return 0, stepNone, false
	}
	switch c.base {
	case base12:
		if c.raw {
			ticks = t // already unwrapped (AirView convention)
			break
		}
		v := t + off12ModeS
		if n == 2 {
			v = t + off12ModeAC
		}
		v &= mask48
		if c.have {
			v = model.Unwrap48(c.max, v)
		}
		ticks = v
	case baseGPS:
		if t >= uint64(model.NsPerDay) {
			if !c.raw {
				return 0, stepNone, false // not a time of day
			}
			ticks = t // already absolute (AirView convention)
			break
		}
		v := int64(t)
		if !c.raw {
			switch n {
			case 2:
				v += offGPSAC
			case 7:
				v += offGPSShort
			default:
				v += offGPSLong
			}
			if v >= model.NsPerDay {
				v -= model.NsPerDay
			}
		}
		g, gok := model.GPSTicks(v, srvNs)
		if !gok {
			return 0, stepNone, false
		}
		ticks = g
	case baseSBS:
		v := t
		if !c.raw {
			var off uint64
			switch n {
			case 2:
				off = offSBSAC
			case 7:
				off = offSBSShort
			}
			if v < off {
				return 0, stepNone, false
			}
			v -= off
		}
		if v > maxSBSTicks {
			return 0, stepNone, false // beyond any plausible widened counter
		}
		ticks = v
	default:
		return 0, stepNone, false
	}
	if c.have {
		switch {
		// (no unsigned sum: ticks near 2^64 would wrap it)
		case ticks < c.max && c.max-ticks > uint64(backStepS*c.hz):
			return ticks, stepBack, true
		case ticks > c.max:
			el := float64(srvNs-c.maxSrvNs) / 1e9
			if el < 0 {
				el = 0
			}
			if float64(ticks-c.max) > (el+fwdStepS)*c.hz {
				return ticks, stepJump, true
			}
		}
	}
	if !c.have || ticks > c.max {
		c.max, c.maxSrvNs, c.have = ticks, srvNs, true
	}
	return ticks, stepNone, true
}

// frameKind returns the model kind of a payload length.
func frameKind(n int) model.FrameKind {
	switch n {
	case 2:
		return model.FrameModeAC
	case 7:
		return model.FrameModeSShort
	}
	return model.FrameModeSLong
}
