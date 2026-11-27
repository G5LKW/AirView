package model

import (
	"strconv"
	"time"
)

// Receiver clock classes, epochs and tick conventions of the AirView network
// (docs/aggregator/DESIGN.md §4.3, docs/aggregator/AVF.md §6).

// ClockClass is the kind of timestamp clock a receiver has. The numeric
// codes are part of the AVF/1 wire format and must never be renumbered.
type ClockClass uint8

// Clock classes.
const (
	ClockNone       ClockClass = 0 // no usable timestamps: map only
	ClockGPSUTC     ClockClass = 1 // Radarcape GPS, status UTC OK + sync OK: ns of UTC day
	ClockGPSGPST    ClockClass = 2 // Radarcape GPS with the UTC-offset bit clear: GPS-time ns
	ClockGPSEmul    ClockClass = 3 // GPS timestamp format without lock (emulated)
	ClockHW12       ClockClass = 4 // hardware 12 MHz counter (Beast, Radarcape 12 MHz)
	ClockSDR12      ClockClass = 5 // readsb/dump1090 at 2-2.4 MS/s, 12 MHz units
	ClockSDR12HiRes ClockClass = 6 // airspy_adsb >= 12 MS/s, 12 MHz units
	ClockSBS20      ClockClass = 7 // SBS 20 MHz (24-bit counter, unwrapped)
)

// NumClockClasses is one past the highest defined class code.
const NumClockClasses = 8

var clockNames = [NumClockClasses]string{
	"none", "gps_utc", "gps_gpst", "gps_emul", "hw12", "sdr12", "sdr12_hires", "sbs20",
}

// String returns the class's wire/config name ("gps_utc", "sdr12", ...), or
// "class<N>" for an undefined code.
func (c ClockClass) String() string {
	if c < NumClockClasses {
		return clockNames[c]
	}
	return "class" + strconv.Itoa(int(c))
}

// ParseClockClass parses a name produced by String.
func ParseClockClass(s string) (ClockClass, bool) {
	for i, n := range clockNames {
		if n == s {
			return ClockClass(i), true
		}
	}
	return ClockNone, false
}

// Valid reports whether c is a defined class.
func (c ClockClass) Valid() bool { return c < NumClockClasses }

// Timed reports whether frames of this class carry usable receiver time.
func (c ClockClass) Timed() bool { return c != ClockNone && c.Valid() }

// GPS reports whether the class uses the 1 GHz GPS tick convention
// (classes 1-3).
func (c ClockClass) GPS() bool { return c >= ClockGPSUTC && c <= ClockGPSEmul }

// AnchorCapable reports whether a station of this class may become an MLAT
// time anchor (after probation, DESIGN §9.2.5): classes 1 and 2 only.
func (c ClockClass) AnchorCapable() bool { return c == ClockGPSUTC || c == ClockGPSGPST }

// NominalHz returns the tick rate of the class (0 for none / unknown).
func (c ClockClass) NominalHz() float64 {
	switch {
	case c.GPS():
		return 1e9
	case c == ClockHW12, c == ClockSDR12, c == ClockSDR12HiRes:
		return 12e6
	case c == ClockSBS20:
		return 20e6
	}
	return 0
}

// PriorJitterNs returns the prior 1-sigma timestamp jitter of the class in
// nanoseconds (0 for none / unknown). The MLAT engine learns the actual
// value per station within x0.5..x4 of this prior.
func (c ClockClass) PriorJitterNs() float64 {
	switch c {
	case ClockGPSUTC, ClockGPSGPST:
		return 30
	case ClockGPSEmul:
		return 100
	case ClockHW12:
		return 83
	case ClockSDR12:
		return 400
	case ClockSDR12HiRes:
		return 150
	case ClockSBS20:
		return 500
	}
	return 0
}

// PriorFreqSigma returns the prior 1-sigma fractional frequency error of the
// class (dimensionless; 1e-9 = 1 ppb), 0 for none / unknown.
func (c ClockClass) PriorFreqSigma() float64 {
	switch c {
	case ClockGPSUTC, ClockGPSGPST:
		return 1e-9
	case ClockGPSEmul, ClockHW12:
		return 5e-6
	case ClockSDR12, ClockSBS20:
		return 100e-6
	case ClockSDR12HiRes:
		return 20e-6
	}
	return 0
}

// SyncedSigmaNs returns the marginal clock-offset sigma below which a
// station of this class counts as synced (DESIGN §9.2.4).
func (c ClockClass) SyncedSigmaNs() float64 {
	switch {
	case c == ClockGPSUTC, c == ClockGPSGPST:
		return 50
	case c == ClockGPSEmul, c == ClockHW12:
		return 100
	case c.Timed():
		return 300
	}
	return 0
}

// ClockFromMLATClientType maps an mlat-client "clock_type" string to a class
// (DESIGN §5.2). Unknown strings map to ClockSDR12.
func ClockFromMLATClientType(t string) ClockClass {
	switch t {
	case "radarcape_gps":
		return ClockGPSUTC
	case "beast", "radarcape_12mhz":
		return ClockHW12
	case "sbs":
		return ClockSBS20
	}
	return ClockSDR12
}

// ClockEventReason says why a receiver started a new clock epoch (AVF
// CLOCK_EVENT, DESIGN §4.3). The names are part of the wire format.
type ClockEventReason string

// Clock epoch reasons.
const (
	ClockEvRestart    ClockEventReason = "restart"     // feeder process or receiver restart
	ClockEvSampleDrop ClockEventReason = "sample_drop" // declared sample drop (SDR)
	ClockEvJump       ClockEventReason = "jump"        // forward step larger than EpochStepLimit
	ClockEvBackwards  ClockEventReason = "backwards"   // backwards step
	ClockEvModeChange ClockEventReason = "mode_change" // timestamp mode or clock class change
)

// Valid reports whether r is a defined reason.
func (r ClockEventReason) Valid() bool {
	switch r {
	case ClockEvRestart, ClockEvSampleDrop, ClockEvJump, ClockEvBackwards, ClockEvModeChange:
		return true
	}
	return false
}

// EpochStepLimit is the forward step of receiver time (against host time)
// above which a new epoch is started.
const EpochStepLimit = 90 * time.Second

// NsPerDay is the number of nanoseconds in a (UTC, leap-second-free) day.
const NsPerDay = int64(86400) * 1e9

// GPSTicks turns a GPS-class timestamp (nanoseconds of the day, 0 <= nsOfDay
// < NsPerDay) into the AVF tick value dayIndex*NsPerDay + nsOfDay, where
// dayIndex (days since 1970-01-01) is chosen so the result is within ±12 h of
// hostUTCNs (host UTC in Unix nanoseconds). For class 1 the result is
// therefore UTC Unix nanoseconds. ok is false for an out-of-range nsOfDay.
func GPSTicks(nsOfDay, hostUTCNs int64) (ticks uint64, ok bool) {
	if nsOfDay < 0 || nsOfDay >= NsPerDay {
		return 0, false
	}
	day := floorDiv(hostUTCNs, NsPerDay)
	t := day*NsPerDay + nsOfDay
	switch d := t - hostUTCNs; {
	case d > NsPerDay/2:
		t -= NsPerDay
	case d <= -NsPerDay/2:
		t += NsPerDay
	}
	if t < 0 {
		return 0, false
	}
	return uint64(t), true
}

// Unwrap48 extends a 48-bit counter value to 64 bits given the previous
// unwrapped value: the result is the value congruent to raw (mod 2^48)
// closest to prev, never below 0. Use it for 12 MHz receivers (the counter
// wraps every 271 days at 12 MHz, but receivers restart it at power-on).
func Unwrap48(prev, raw uint64) uint64 { return unwrapBits(prev, raw, 48) }

// Unwrap24 is Unwrap48 for the 24-bit SBS 20 MHz counter, which wraps every
// 0.84 s; callers must call it at least every 0.4 s of receiver time (the
// feeder uses host time to bridge longer gaps).
func Unwrap24(prev, raw uint64) uint64 { return unwrapBits(prev, raw, 24) }

func unwrapBits(prev, raw uint64, bits uint) uint64 {
	mod := uint64(1) << bits
	mask := mod - 1
	raw &= mask
	cand := (prev &^ mask) | raw
	half := mod / 2
	switch {
	case cand > prev && cand-prev > half && cand >= mod:
		cand -= mod
	case cand < prev && prev-cand > half:
		cand += mod
	}
	return cand
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
