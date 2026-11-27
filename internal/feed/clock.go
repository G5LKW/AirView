package feed

import (
	"math/rand/v2"

	"airview/internal/model"
)

// Receiver clock epochs and unwrapped ticks (docs/aggregator/DESIGN.md §4.3,
// docs/aggregator/AVF.md §5.3).
//
// A receiver timestamp is turned into a monotonic 64-bit tick value per
// (receiver, epoch), in units of the clock class's nominal rate:
//
//   - GPS classes (1 GHz): dayIndex*model.NsPerDay + nsOfDay. The day of the
//     first frame of an epoch comes from host UTC (model.GPSTicks, ±12 h);
//     later frames keep continuity with the previous frame, so midnight
//     rolls the day over and a host clock step does not move the ticks.
//   - 12 MHz classes: the 48-bit counter unwrapped (model.Unwrap48).
//   - SBS 20 MHz: the 24-bit counter unwrapped (model.Unwrap24); gaps longer
//     than 0.4 s of host time are bridged by predicting the counter from
//     host time.
//
// Ticks are never converted into another time base: the server learns each
// receiver's offset and drift. A new epoch is started (and reported as a
// ClockEvent) whenever ticks of the new frames would no longer be
// comparable with the old ones.

// Clock event details: finer-grained causes behind a model.ClockEventReason.
// They are informational (logs, status pages); the wire format only carries
// the reason.
const (
	ClockDetailStart       = "start"        // first epoch of a feed in this process (reason restart)
	ClockDetailReconnect   = "reconnect"    // first epoch after the input reconnected (reason restart)
	ClockDetailStep        = "step"         // forward step of more than the step limit (reason jump)
	ClockDetailBackwards   = "backwards"    // receiver time went backwards, e.g. counter reset (reason backwards)
	ClockDetailGPSLost     = "gps_lost"     // GPS lock lost: gps_utc/gps_gpst -> gps_emul (reason mode_change)
	ClockDetailGPSRegained = "gps_regained" // GPS lock regained: gps_emul -> gps_utc/gps_gpst (reason mode_change)
	ClockDetailTimeBase    = "time_base"    // UTC <-> GPS-time base switch (reason mode_change)
	ClockDetailDayRange    = "day_range"    // second-of-day range changed (legacy 1..86400 vs 0..86399) (reason mode_change)
	ClockDetailClassChange = "class_change" // timestamp mode or clock class changed otherwise (reason mode_change)
	ClockDetailOverride    = "override"     // the configured class override changed (reason mode_change)
	ClockDetailSampleDrop  = "sample_drop"  // the input declared lost samples (reason sample_drop)
)

// ClockEvent reports the start of a new receiver clock epoch. It maps 1:1
// onto the AVF CLOCK_EVENT record and is always delivered before the first
// frame that carries NewEpoch.
type ClockEvent struct {
	OldEpoch uint32 // previous epoch; 0 when this feed had none before
	NewEpoch uint32 // the epoch frames carry from now on (never 0)
	Reason   model.ClockEventReason
	Detail   string           // ClockDetail* constant
	OldClass model.ClockClass // class of the previous epoch (ClockNone when none)
	NewClass model.ClockClass // class of the new epoch
	// StepNs is the observed step of receiver time against host time in
	// nanoseconds for reasons jump and backwards (receiver minus expected),
	// else 0.
	StepNs int64
	// SamplesLost is the number of samples the input declared lost (reason
	// sample_drop), else 0.
	SamplesLost uint64
	// HostNs is the host UTC time (Unix ns) of the frame that opened the
	// new epoch.
	HostNs int64
}

// Defaults of ClockTracker.
const (
	DefaultBackwardsTolerance = int64(100e6) // 100 ms of receiver time
	DefaultStepConfirmFrames  = 3
	sbs20BridgeNs             = int64(400e6) // gaps above this use host-time prediction
)

// ClockTracker turns raw receiver timestamps of one receiver into unwrapped
// ticks and manages its clock epochs. It is the class-independent half of
// Timestamper and can be used on its own by inputs that already know their
// class (for example an SBS 20 MHz source).
//
// Every timed frame is checked against the previous accepted frame: receiver
// time may not go back by more than BackwardsTolerance, and may not run
// ahead of host time by more than StepLimitNs. A frame that fails either
// check is not trusted on its own: it opens a candidate step, and only when
// ConfirmFrames consecutive frames are consistent with each other (and not
// with the old epoch) is a new epoch started. Frames of an unconfirmed
// candidate are reported as untimed; a lone corrupt timestamp therefore
// costs one frame's timing and never an epoch.
//
// The zero value is ready to use. It is not safe for concurrent use.
type ClockTracker struct {
	// StepLimitNs is the largest forward step of receiver time against host
	// time that stays in the epoch (default model.EpochStepLimit).
	StepLimitNs int64
	// BackwardsTolerance is how far (ns of receiver time) a frame may lie
	// before the newest accepted frame without being a backwards step
	// (default 100 ms). It absorbs reordering inside an input; sorting is
	// the uplink's job.
	BackwardsTolerance int64
	// ConfirmFrames is the number of consecutive consistent frames that
	// confirm a step (default 3; 1 = react to the first frame).
	ConfirmFrames int
	// OnEvent, when set, receives every new epoch before the frame that
	// opened it is returned.
	OnEvent func(ClockEvent)

	cls      model.ClockClass // class frames are stamped with
	epochCls model.ClockClass // class of the current / last epoch
	epoch    uint32           // current / last epoch, 0 = none yet
	next     uint32           // next epoch ID, 0 = not seeded
	running  bool             // an epoch is open (ref is valid)
	ref      clockRef         // newest accepted frame of the open epoch
	cand     clockRef         // candidate step, valid when candN > 0
	candN    int
	candStep int64

	pending     bool // the next epoch has a recorded cause
	pendReason  model.ClockEventReason
	pendDetail  string
	pendSamples uint64

	// Counters (monotonic over the tracker's lifetime).
	Epochs    uint64 // epochs started
	Steps     uint64 // confirmed forward steps
	Backwards uint64 // confirmed backwards steps
	Glitches  uint64 // abnormal frames that were not confirmed
}

type clockRef struct {
	ticks uint64
	host  int64
}

func (c *ClockTracker) stepLimit() int64 {
	if c.StepLimitNs > 0 {
		return c.StepLimitNs
	}
	return int64(model.EpochStepLimit)
}

func (c *ClockTracker) backTol() int64 {
	if c.BackwardsTolerance > 0 {
		return c.BackwardsTolerance
	}
	return DefaultBackwardsTolerance
}

func (c *ClockTracker) confirm() int {
	if c.ConfirmFrames > 0 {
		return c.ConfirmFrames
	}
	return DefaultStepConfirmFrames
}

// Class returns the class frames are currently stamped with.
func (c *ClockTracker) Class() model.ClockClass { return c.cls }

// Epoch returns the current epoch, or the last one when no epoch is open
// (0 when none was ever started).
func (c *ClockTracker) Epoch() uint32 { return c.epoch }

// Running reports whether an epoch is open, i.e. the next consistent frame
// continues it.
func (c *ClockTracker) Running() bool { return c.running }

// LastTicks returns the ticks of the newest accepted frame of the open
// epoch (0 when no epoch is open).
func (c *ClockTracker) LastTicks() uint64 {
	if !c.running {
		return 0
	}
	return c.ref.ticks
}

// SetEpochBase makes the next epoch prev+1 (skipping 0) so that epoch IDs
// of a receiver keep increasing across reconnects, and records the next
// epoch's cause as a reconnect. Call it before the first Stamp. prev = 0
// means "no previous epoch" and is a no-op: the first epoch ID is then
// random, so a restarted process does not repeat its predecessor's IDs.
func (c *ClockTracker) SetEpochBase(prev uint32) {
	if prev == 0 {
		return
	}
	c.next = prev + 1
	if c.next == 0 {
		c.next = 1
	}
	c.epoch = prev
	c.setPending(model.ClockEvRestart, ClockDetailReconnect, 0)
}

func (c *ClockTracker) newEpochID() uint32 {
	if c.next == 0 {
		c.next = rand.Uint32()
		if c.next == 0 {
			c.next = 1
		}
	}
	id := c.next
	c.next++
	if c.next == 0 {
		c.next = 1
	}
	if id == c.epoch { // never reuse the previous ID (possible only after wrapping)
		id = c.next
		c.next++
		if c.next == 0 {
			c.next = 1
		}
	}
	return id
}

func (c *ClockTracker) setPending(r model.ClockEventReason, detail string, samples uint64) {
	c.pending, c.pendReason, c.pendDetail, c.pendSamples = true, r, detail, samples
}

// SetClass sets the class following frames are stamped with. A change ends
// the open epoch; the next timed frame opens a new one with reason
// mode_change and the given detail (a ClockDetail* constant).
func (c *ClockTracker) SetClass(cls model.ClockClass, detail string) {
	if cls == c.cls {
		return
	}
	c.cls = cls
	switch {
	case c.running:
		c.running, c.candN = false, 0
		c.setPending(model.ClockEvModeChange, detail, 0)
	case c.epoch != 0 && (!c.pending || c.pendReason == model.ClockEvModeChange):
		// Between epochs: the newest class change explains the next one,
		// but a recorded restart or sample drop takes precedence.
		c.setPending(model.ClockEvModeChange, detail, 0)
	}
}

// ModeChange ends the open epoch because the meaning of the timestamps
// changed without a class change (e.g. the second-of-day range): the next
// timed frame opens a new epoch with reason mode_change and detail.
func (c *ClockTracker) ModeChange(detail string) {
	if !c.running {
		if c.epoch != 0 && (!c.pending || c.pendReason == model.ClockEvModeChange) {
			c.setPending(model.ClockEvModeChange, detail, 0)
		}
		return
	}
	c.running, c.candN = false, 0
	c.setPending(model.ClockEvModeChange, detail, 0)
}

// SampleDrop declares that the input lost samples (or frames with timing
// continuity): the open epoch ends and the next timed frame opens a new
// one with reason sample_drop.
func (c *ClockTracker) SampleDrop(lost uint64) {
	if !c.running && c.epoch == 0 {
		return
	}
	c.running, c.candN = false, 0
	c.setPending(model.ClockEvSampleDrop, ClockDetailSampleDrop, lost)
}

// Restart ends the open epoch as if the input had restarted (reason
// restart, detail ClockDetailReconnect). Use it when one tracker outlives a
// reconnect.
func (c *ClockTracker) Restart() {
	if !c.running && c.epoch == 0 {
		return
	}
	c.running, c.candN = false, 0
	c.setPending(model.ClockEvRestart, ClockDetailReconnect, 0)
}

// Stamp converts one raw receiver timestamp into ticks of the current
// epoch. raw is, by class: nanoseconds of the day (GPS classes, already
// corrected for the legacy day range), the 48-bit counter (12 MHz classes)
// or the 24-bit counter (SBS 20 MHz). hostNs is the host UTC receive time in
// Unix nanoseconds. ok is false for untimed classes, an invalid raw value
// and frames of an unconfirmed step.
func (c *ClockTracker) Stamp(raw uint64, hostNs int64) (ticks uint64, ok bool) {
	if !c.cls.Timed() {
		return 0, false
	}
	if c.cls.GPS() && raw >= uint64(model.NsPerDay) {
		return 0, false
	}
	if !c.running {
		t, ok := c.initial(raw, hostNs)
		if !ok {
			return 0, false
		}
		c.begin(clockRef{t, hostNs}, 0)
		return t, true
	}

	t, step, normal := c.check(c.ref, raw, hostNs)
	if normal {
		if c.candN > 0 {
			c.Glitches += uint64(c.candN)
			c.candN = 0
		}
		if t > c.ref.ticks {
			c.ref.ticks = t
		}
		c.ref.host = hostNs
		return t, true
	}

	if c.candN > 0 {
		if t2, _, ok2 := c.check(c.cand, raw, hostNs); ok2 {
			c.candN++
			if t2 > c.cand.ticks {
				c.cand.ticks = t2
			}
			c.cand.host = hostNs
			if c.candN >= c.confirm() {
				c.confirmStep()
				return t2, true
			}
			return 0, false
		}
		c.Glitches += uint64(c.candN)
	}
	// Open a new candidate, mapped afresh (a reset counter or a new day
	// starts from the raw value, not from the old epoch's unwrapping).
	t0, ok0 := c.initial(raw, hostNs)
	if !ok0 {
		c.candN = 0
		return 0, false
	}
	c.cand, c.candN, c.candStep = clockRef{t0, hostNs}, 1, step
	if c.candN >= c.confirm() {
		c.confirmStep()
		return t0, true
	}
	return 0, false
}

func (c *ClockTracker) confirmStep() {
	if c.candStep > 0 {
		c.Steps++
		c.setPending(model.ClockEvJump, ClockDetailStep, 0)
	} else {
		c.Backwards++
		c.setPending(model.ClockEvBackwards, ClockDetailBackwards, 0)
	}
	ref, step := c.cand, c.candStep
	c.candN = 0
	c.begin(ref, step)
}

// begin opens a new epoch at ref and reports it.
func (c *ClockTracker) begin(ref clockRef, step int64) {
	ev := ClockEvent{
		OldEpoch: c.epoch,
		OldClass: c.epochCls,
		NewClass: c.cls,
		HostNs:   ref.host,
		Reason:   model.ClockEvRestart,
		Detail:   ClockDetailStart,
	}
	if c.epoch == 0 {
		ev.OldClass = model.ClockNone
	}
	if c.pending {
		ev.Reason, ev.Detail, ev.SamplesLost = c.pendReason, c.pendDetail, c.pendSamples
		if ev.Reason == model.ClockEvJump || ev.Reason == model.ClockEvBackwards {
			ev.StepNs = step
		}
	}
	c.pending = false
	c.epoch = c.newEpochID()
	ev.NewEpoch = c.epoch
	c.epochCls = c.cls
	c.running, c.ref, c.candN = true, ref, 0
	c.Epochs++
	if c.OnEvent != nil {
		c.OnEvent(ev)
	}
}

// initial maps a raw value without reference to a previous frame.
func (c *ClockTracker) initial(raw uint64, hostNs int64) (uint64, bool) {
	switch {
	case c.cls.GPS():
		return model.GPSTicks(int64(raw), hostNs)
	case c.cls == model.ClockSBS20:
		return raw & (1<<24 - 1), true
	default:
		return raw & (1<<48 - 1), true
	}
}

// check resolves raw against ref and reports whether it continues ref's
// epoch. step is receiver-minus-expected time in ns (positive = ahead).
func (c *ClockTracker) check(ref clockRef, raw uint64, hostNs int64) (ticks uint64, step int64, normal bool) {
	hz := c.cls.NominalHz()
	dH := hostNs - ref.host
	pred := addTicks(ref.ticks, float64(dH)*hz/1e9)
	switch {
	case c.cls.GPS():
		// Continuity: the day is the one that puts the frame within ±12 h
		// of where ref plus elapsed host time predicts it.
		if pred > uint64(1<<63-1) {
			return 0, 0, false
		}
		t, ok := model.GPSTicks(int64(raw), int64(pred))
		if !ok {
			return 0, 0, false
		}
		ticks = t
	case c.cls == model.ClockSBS20:
		if dH >= 0 && dH < sbs20BridgeNs {
			ticks = model.Unwrap24(ref.ticks, raw)
		} else {
			ticks = model.Unwrap24(pred, raw)
		}
	default:
		ticks = model.Unwrap48(pred, raw)
	}
	dT := ticksToNs(ticks, ref.ticks, hz)
	step = dT - dH
	if dT < -c.backTol() {
		return ticks, step, false
	}
	if step > c.stepLimit() {
		return ticks, step, false
	}
	return ticks, step, true
}

// addTicks returns base+delta clamped to the uint64 range.
func addTicks(base uint64, delta float64) uint64 {
	if delta >= 0 {
		if delta >= float64(^uint64(0)-base) {
			return ^uint64(0)
		}
		return base + uint64(delta)
	}
	d := -delta
	if d >= float64(base) {
		return 0
	}
	return base - uint64(d)
}

// ticksToNs returns (a-b) in nanoseconds at rate hz, saturated to ±2^62.
func ticksToNs(a, b uint64, hz float64) int64 {
	const lim = float64(1 << 62)
	var d float64
	if a >= b {
		d = float64(a-b) * 1e9 / hz
	} else {
		d = -float64(b-a) * 1e9 / hz
	}
	switch {
	case d > lim:
		return 1 << 62
	case d < -lim:
		return -(1 << 62)
	}
	return int64(d)
}
