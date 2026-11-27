package simnet

import (
	"fmt"
	"math"
	"math/rand/v2"

	"airview/internal/feed"
	"airview/internal/model"
)

// Device is the kind of receiver hardware / decoder a simulated station
// runs. It selects the clock behaviour, the timestamp format of the Beast
// output and the defaults of DefaultClockSpec and DefaultReception.
type Device string

// Devices.
const (
	// DeviceRadarcapeGPS is a Radarcape / EVO in GPS timestamp mode: a 1 GHz
	// nanosecond-of-day counter disciplined by the GPS 1PPS. Class gps_utc
	// (gps_gpst while it still runs on GPS time after boot, gps_emul while
	// the GPS lock is lost). Beast timestamps use the Radarcape GPS format
	// (second of day << 30 | nanoseconds), and a type '4' status frame is
	// sent every second.
	DeviceRadarcapeGPS Device = "radarcape_gps"
	// DeviceRadarcape12 is a Radarcape in legacy 12 MHz mode (class hw12).
	DeviceRadarcape12 Device = "radarcape_12mhz"
	// DeviceBeast is a Mode-S Beast: free-running 12 MHz counter (class hw12).
	DeviceBeast Device = "beast"
	// DeviceSDR is an RTL-SDR with readsb/dump1090 at 2.4 MS/s: 12 MHz
	// timestamp units quantised to 5 ticks, hundreds of ns of jitter, tens
	// of ppm of frequency error with thermal wander and USB sample drops
	// (class sdr12).
	DeviceSDR Device = "sdr"
	// DeviceAirspy is an Airspy with airspy_adsb: sampled at 20 MS/s
	// (quantum 50 ns), timestamps reported in 12 MHz units (class
	// sdr12_hires).
	DeviceAirspy Device = "airspy"
	// DeviceSBS is a Kinetic SBS: 20 MHz counter output as 24 bits that wrap
	// every 0.84 s (class sbs20).
	DeviceSBS Device = "sbs"
)

// Devices lists every device kind.
var Devices = []Device{DeviceRadarcapeGPS, DeviceRadarcape12, DeviceBeast, DeviceSDR, DeviceAirspy, DeviceSBS}

// Class returns the clock class a device reports when nothing is wrong.
func (d Device) Class() model.ClockClass {
	switch d {
	case DeviceRadarcapeGPS:
		return model.ClockGPSUTC
	case DeviceRadarcape12, DeviceBeast:
		return model.ClockHW12
	case DeviceSDR:
		return model.ClockSDR12
	case DeviceAirspy:
		return model.ClockSDR12HiRes
	case DeviceSBS:
		return model.ClockSBS20
	}
	return model.ClockNone
}

// sampleRate is the default timestamp quantisation rate of a device.
func (d Device) sampleRate() float64 {
	switch d {
	case DeviceRadarcapeGPS:
		return 1e9
	case DeviceRadarcape12, DeviceBeast:
		return 12e6
	case DeviceSDR:
		return 2.4e6
	case DeviceAirspy, DeviceSBS:
		return 20e6
	}
	return 12e6
}

// MLATClientType is the mlat-client "clock_type" a feeder of this device
// would announce.
func (d Device) MLATClientType() string {
	switch d {
	case DeviceRadarcapeGPS:
		return "radarcape_gps"
	case DeviceRadarcape12:
		return "radarcape_12mhz"
	case DeviceBeast:
		return "beast"
	case DeviceSBS:
		return "sbs"
	}
	return "dump1090"
}

func (d Device) valid() bool {
	for _, x := range Devices {
		if x == d {
			return true
		}
	}
	return false
}

// ClockStep is a scheduled step of the receiver clock offset (applied at the
// first 100 ms clock-grid point at or after AtS). A step with an empty
// Reason is silent: the feeder does not notice it (the "silent 22 µs step"
// CI scenario). With a Reason the receiver starts a new epoch.
type ClockStep struct {
	AtS    float64                // seconds since Config.Start
	StepNs float64                // offset change (local minus true time), ns
	Reason model.ClockEventReason // "" = silent
}

// SampleDrop is a scheduled loss of samples (SDR USB overflow): during
// Samples/SampleRate seconds from AtS (rounded up to the 100 ms grid)
// nothing is received, and the sample counter falls behind by that much.
// Declared drops start a new epoch with reason sample_drop.
type SampleDrop struct {
	AtS      float64
	Samples  int
	Declared bool
}

// Interval is a time span in seconds since Config.Start, [FromS, ToS).
type Interval struct{ FromS, ToS float64 }

func (iv Interval) contains(t float64) bool { return t >= iv.FromS && t < iv.ToS }

// ClockSpec configures the clock of one receiver. Every field is used as
// given (zero means zero, i.e. "no such effect"), except Class and
// SampleRateHz, which default from Device. DefaultClockSpec returns a
// realistic random specification for a device.
type ClockSpec struct {
	Device Device
	// Class overrides the class the receiver reports (0 = Device.Class()).
	Class model.ClockClass
	// SampleRateHz is the timestamp quantisation (0 = device default: 1 GHz
	// GPS, 12 MHz Beast/Radarcape, 2.4 MS/s SDR, 20 MS/s Airspy/SBS).
	SampleRateHz float64

	// InitialPPM is the fractional frequency error at Start, ppm (local
	// clock runs fast for positive values).
	InitialPPM float64
	// RandomWalkPPM is the frequency random walk, ppm per sqrt(second).
	RandomWalkPPM float64
	// ThermalRampPPMPerS is the largest frequency slope of the thermal
	// wander: the frequency follows piecewise-linear ramps whose slope is
	// drawn uniformly from ±ThermalRampPPMPerS every ~ThermalSegmentS seconds
	// and which stay within InitialPPM ± ThermalBandPPM.
	ThermalRampPPMPerS float64
	ThermalBandPPM     float64
	ThermalSegmentS    float64 // mean segment length (0 = 600 s)

	// JitterNs is the white timestamp noise (1σ), on top of quantisation.
	JitterNs float64
	// ShortLongErrorNs is added to the timestamp of short (56-bit) frames:
	// the receiver's reference point for short frames is off by this much
	// relative to long ones (the β_short the MLAT engine estimates).
	ShortLongErrorNs float64
	// FrameRef*Ns say where in the frame the receiver stamps, as ns after
	// the leading edge of the preamble (0 = preamble start; Radarcape GPS
	// and SBS: end of frame, 20 300 (SBS 24 000) / 64 000 / 120 000; Beast
	// and Radarcape 12 MHz: a fixed 64 µs for both Mode S lengths).
	FrameRefACNs, FrameRefShortNs, FrameRefLongNs float64

	// Steps and Drops are scheduled events; Restarts are times (s) at which
	// the receiver restarts (counter reset, new epoch, reason restart).
	Steps    []ClockStep
	Drops    []SampleDrop
	Restarts []float64
	// Random events (Poisson rates per hour).
	SilentStepsPerHour   float64
	SilentStepMaxNs      float64 // uniform ±max
	SilentDropsPerHour   float64
	DeclaredDropsPerHour float64
	DropMaxSamples       int // uniform 1..max samples (0 = 262144)

	// CounterStart is the 12/20 MHz counter value at Start (0 = random).
	CounterStart uint64
	// EpochStart is the first epoch id (0 = random).
	EpochStart uint32

	// GPS (DeviceRadarcapeGPS) only.
	CableBiasNs float64    // constant offset: antenna cable and receiver delay
	PPSJitterNs float64    // 1σ of the per-second 1PPS discipline error
	LockLoss    []Interval // GPS lock lost: free-running, class gps_emul
	FreeRunPPB  float64    // frequency error while the lock is lost, ppb
	// BootGPSTimeS: the receiver reports GPS time (UTC + LeapSeconds, UTC
	// status bit clear, class gps_gpst) until this many seconds after Start.
	BootGPSTimeS float64
	LeapSeconds  int // 0 = 18

	// Host (feeder computer) timing.
	HostOffsetMs      float64 // constant host clock error (NTP), ms
	HostWanderMs      float64 // amplitude of a slow (~2 h) host clock wander, ms
	HostDelayMs       float64 // receiver -> host latency, ms
	HostDelayJitterMs float64 // exponential extra latency, mean ms
	HostBlockMs       float64 // samples are processed in blocks of this length (SDR), ms
}

// DefaultClockSpec returns a realistic, randomised clock for a device (r
// supplies the randomness).
func DefaultClockSpec(d Device, r *rand.Rand) ClockSpec {
	c := ClockSpec{Device: d}
	sign := func() float64 {
		if r.IntN(2) == 0 {
			return -1
		}
		return 1
	}
	switch d {
	case DeviceRadarcapeGPS:
		c.JitterNs = 8
		c.PPSJitterNs = uniform(r, 15, 45)
		c.CableBiasNs = uniform(r, 50, 300)
		c.FreeRunPPB = uniform(r, -200, 200)
		c.FrameRefACNs, c.FrameRefShortNs, c.FrameRefLongNs = 20300, 64000, 120000
		c.HostDelayMs, c.HostDelayJitterMs = 2, 1
	case DeviceRadarcape12, DeviceBeast:
		c.InitialPPM = clamp(r.NormFloat64()*1.5, -5, 5)
		c.RandomWalkPPM = 2e-4
		c.JitterNs = 30
		// 12 MHz stamps sit at one fixed point of the frame, independent
		// of its length (the observed Beast / Radarcape 12 MHz interop
		// convention, not the GPS end-of-frame table).
		c.FrameRefACNs, c.FrameRefShortNs, c.FrameRefLongNs = 20300, 64000, 64000
		c.HostDelayMs, c.HostDelayJitterMs = 2, 1
	case DeviceSDR:
		c.InitialPPM = sign() * uniform(r, 30, 100)
		c.RandomWalkPPM = 1e-3
		c.ThermalRampPPMPerS = 0.01
		c.ThermalBandPPM = 2
		c.ThermalSegmentS = 600
		c.JitterNs = uniform(r, 300, 500)
		c.ShortLongErrorNs = uniform(r, -200, 200)
		c.SilentDropsPerHour = 0.05
		c.DeclaredDropsPerHour = 0.1
		c.HostDelayMs, c.HostDelayJitterMs, c.HostBlockMs = 5, 3, 54.6
	case DeviceAirspy:
		c.InitialPPM = uniform(r, -20, 20)
		c.RandomWalkPPM = 5e-4
		c.ThermalRampPPMPerS = 0.003
		c.ThermalBandPPM = 1
		c.ThermalSegmentS = 900
		c.JitterNs = 150
		c.ShortLongErrorNs = uniform(r, -100, 100)
		c.HostDelayMs, c.HostDelayJitterMs, c.HostBlockMs = 5, 2, 10
	case DeviceSBS:
		c.InitialPPM = sign() * uniform(r, 10, 100)
		c.RandomWalkPPM = 1e-3
		c.ThermalRampPPMPerS = 0.005
		c.ThermalBandPPM = 2
		c.ThermalSegmentS = 600
		c.JitterNs = 500
		// SBS-3 stamps the end of the frame (the reference client's
		// +56 µs short / +96 µs A/C corrections align everything with the
		// end of a long frame).
		c.FrameRefACNs, c.FrameRefShortNs, c.FrameRefLongNs = 24000, 64000, 120000
		c.HostDelayMs, c.HostDelayJitterMs = 5, 2
	}
	c.HostOffsetMs = r.NormFloat64() * 2
	c.HostWanderMs = r.Float64()
	return c
}

func (c *ClockSpec) normalize() error {
	if c.Device == "" {
		c.Device = DeviceBeast
	}
	if !c.Device.valid() {
		return fmt.Errorf("unknown device %q", c.Device)
	}
	if c.Class == model.ClockNone {
		c.Class = c.Device.Class()
	}
	if !c.Class.Timed() {
		return fmt.Errorf("clock class %v is not a timed class", c.Class)
	}
	if c.Class.GPS() != (c.Device == DeviceRadarcapeGPS) {
		return fmt.Errorf("clock class %v does not fit device %q", c.Class, c.Device)
	}
	if !(c.SampleRateHz > 0) {
		c.SampleRateHz = c.Device.sampleRate()
	}
	if !(c.ThermalSegmentS > 0) {
		c.ThermalSegmentS = 600
	}
	if c.DropMaxSamples <= 0 {
		c.DropMaxSamples = 262144
	}
	if c.LeapSeconds == 0 {
		c.LeapSeconds = 18
	}
	for _, v := range []float64{c.InitialPPM, c.RandomWalkPPM, c.JitterNs, c.ShortLongErrorNs, c.CableBiasNs, c.PPSJitterNs, c.HostOffsetMs, c.HostDelayMs} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("clock parameter is not finite")
		}
	}
	return nil
}

// ClockEvent is the start of a new clock epoch of a receiver (truth, and
// the AVF CLOCK_EVENT / mlat-client clock_reset the feeder would send).
type ClockEvent struct {
	Rx          int
	AtS         float64 // seconds since Start (grid point)
	OldEpoch    uint32
	NewEpoch    uint32
	Reason      model.ClockEventReason
	SamplesLost int
	Class       model.ClockClass // class from AtS on
}

// ClockTruth is the true state of a receiver clock at one instant.
type ClockTruth struct {
	Rx    int
	AtS   float64
	Epoch uint32
	Class model.ClockClass
	// OffsetNs is local minus true time (θ), excluding per-frame jitter,
	// quantisation and the frame reference offsets. For gps_gpst it
	// includes the GPS-UTC leap seconds.
	OffsetNs float64
	// FreqPPM is the fractional frequency error (φ) in ppm.
	FreqPPM float64
	// LocalS is the receiver's local time at AtS: AtS + OffsetNs·1e-9.
	LocalS float64
	// Ticks is the ideal (noise free) unwrapped counter value at AtS in
	// units of Class.NominalHz(); for GPS classes Unix nanoseconds (as a
	// float64 only good to ~256 ns there: use LocalS for precise work).
	Ticks float64
	// GPS status (DeviceRadarcapeGPS).
	Locked   bool
	GPSTime  bool // reporting GPS time instead of UTC
	PPSErrNs float64
}

// gridS is the clock evolution grid; equal to the world step.
const gridS = 0.1

const clockRing = 128 // 12.8 s of clock history for lookups

type clockNode struct {
	k        int64 // grid index: interval [k*gridS, (k+1)*gridS)
	theta    float64
	phi      float64
	c0       float64 // counter offset of this epoch (ticks at local time 0)
	epoch    uint32
	class    model.ClockClass
	locked   bool
	gpsTime  bool
	ppsErr   float64 // s, GPS 1PPS discipline error of the current second
	dropEnd  float64 // nothing is received before this time (s); 0 = none
	thermSlp float64 // current thermal slope, 1/s
	thermEnd float64 // end of the thermal segment, s
}

// clockModel is the runtime state of one receiver clock. It evolves on a
// fixed 100 ms grid with its own random stream, so it does not depend on
// which frames are received.
type clockModel struct {
	rx      int
	spec    ClockSpec
	f       float64 // nominal Hz
	quant   float64 // seconds per sample
	startNs int64   // Unix ns of Start
	rng     *rand.Rand
	ring    [clockRing]clockNode
	last    int64 // grid index of the newest node
	events  []ClockEvent
	steps   []ClockStep
	drops   []SampleDrop
	restart []float64
	bias    float64 // GPS cable bias, s
	phi0    float64
}

func newClockModel(rx int, spec ClockSpec, startNs int64, seed uint64) *clockModel {
	c := &clockModel{rx: rx, spec: spec, startNs: startNs, rng: newRand(seed, streamClock, uint64(rx))}
	c.f = spec.Class.NominalHz()
	c.quant = 1 / spec.SampleRateHz
	c.steps = sortedSteps(spec.Steps)
	c.drops = sortedDrops(spec.Drops)
	c.restart = sortedFloats(spec.Restarts)
	c.bias = spec.CableBiasNs * 1e-9
	c.phi0 = spec.InitialPPM * 1e-6
	n := clockNode{k: 0, phi: c.phi0, class: spec.Class, locked: true}
	n.epoch = spec.EpochStart
	if n.epoch == 0 {
		n.epoch = uint32(c.rng.Uint64()>>40) + 1
	}
	if c.f != 1e9 {
		start := float64(spec.CounterStart)
		if spec.CounterStart == 0 {
			start = math.Floor(uniform(c.rng, 1e9, 1<<40))
		}
		n.c0 = start
	}
	if spec.Class.GPS() {
		n.phi = 0
		n.theta = c.bias
		n.ppsErr = c.rng.NormFloat64() * spec.PPSJitterNs * 1e-9
		n.theta += n.ppsErr
		if spec.BootGPSTimeS > 0 {
			n.gpsTime = true
			n.class = model.ClockGPSGPST
			n.theta += float64(spec.LeapSeconds)
		}
		if c.inLockLoss(0) {
			n.locked = false
			n.class = model.ClockGPSEmul
		}
	}
	n.thermSlp, n.thermEnd = c.newThermal(0)
	c.ring[0] = n
	c.last = 0
	return c
}

func (c *clockModel) newThermal(t float64) (slope, end float64) {
	if c.spec.ThermalRampPPMPerS <= 0 {
		return 0, math.Inf(1)
	}
	return uniform(c.rng, -1, 1) * c.spec.ThermalRampPPMPerS * 1e-6, t + expRand(c.rng, c.spec.ThermalSegmentS)
}

func (c *clockModel) inLockLoss(t float64) bool {
	for _, iv := range c.spec.LockLoss {
		if iv.contains(t) {
			return true
		}
	}
	return false
}

func (c *clockModel) slot(k int64) *clockNode { return &c.ring[int(k%clockRing)] }

// advanceTo creates grid nodes up to the one containing t.
func (c *clockModel) advanceTo(t float64) {
	kEnd := int64(math.Floor(t / gridS))
	for c.last < kEnd {
		c.next()
	}
}

func (c *clockModel) newEpoch(n *clockNode, t float64, reason model.ClockEventReason, lost int) {
	old := n.epoch
	n.epoch++
	if n.epoch == 0 {
		n.epoch = 1
	}
	c.events = append(c.events, ClockEvent{Rx: c.rx, AtS: t, OldEpoch: old, NewEpoch: n.epoch, Reason: reason, SamplesLost: lost, Class: n.class})
}

// next creates the node after c.last.
func (c *clockModel) next() {
	p := *c.slot(c.last)
	k := c.last + 1
	t := float64(k) * gridS
	n := p
	n.k = k
	n.dropEnd = 0
	n.theta = p.theta + p.phi*gridS
	sp := &c.spec
	if sp.Class.GPS() {
		c.nextGPS(&p, &n, t)
	} else {
		// frequency: random walk + thermal ramps
		if sp.RandomWalkPPM > 0 {
			n.phi += c.rng.NormFloat64() * sp.RandomWalkPPM * 1e-6 * math.Sqrt(gridS)
		}
		if sp.ThermalRampPPMPerS > 0 {
			if t >= n.thermEnd {
				n.thermSlp, n.thermEnd = c.newThermal(t)
			}
			band := sp.ThermalBandPPM * 1e-6
			if d := n.phi - c.phi0; band > 0 && math.Abs(d) > band && math.Signbit(d) == math.Signbit(n.thermSlp) {
				n.thermSlp = -n.thermSlp // turn back towards the centre of the band
			}
			n.phi += n.thermSlp * gridS
		}
	}
	// silent random steps
	if poissonHit(c.rng, sp.SilentStepsPerHour/3600, gridS) {
		n.theta += uniform(c.rng, -1, 1) * sp.SilentStepMaxNs * 1e-9
	}
	// scheduled steps
	for len(c.steps) > 0 && c.steps[0].AtS <= t {
		s := c.steps[0]
		c.steps = c.steps[1:]
		n.theta += s.StepNs * 1e-9
		if s.Reason != "" {
			c.newEpoch(&n, t, s.Reason, 0)
		}
	}
	// sample drops (counter-based clocks only)
	if !sp.Class.GPS() {
		drop := func(samples int, declared bool) {
			d := float64(samples) / sp.SampleRateHz
			n.theta -= d
			if e := t + d; e > n.dropEnd {
				n.dropEnd = e
			}
			if declared {
				c.newEpoch(&n, t, model.ClockEvSampleDrop, samples)
			}
		}
		for len(c.drops) > 0 && c.drops[0].AtS <= t {
			d := c.drops[0]
			c.drops = c.drops[1:]
			drop(max(d.Samples, 1), d.Declared)
		}
		if poissonHit(c.rng, sp.SilentDropsPerHour/3600, gridS) {
			drop(1+c.rng.IntN(sp.DropMaxSamples), false)
		}
		if poissonHit(c.rng, sp.DeclaredDropsPerHour/3600, gridS) {
			drop(1+c.rng.IntN(sp.DropMaxSamples), true)
		}
	}
	// restarts
	for len(c.restart) > 0 && c.restart[0] <= t {
		c.restart = c.restart[1:]
		if !sp.Class.GPS() {
			// the counter restarts near zero
			n.c0 = math.Floor(uniform(c.rng, 1e6, 1e9)) - (t+n.theta)*c.f
		}
		c.newEpoch(&n, t, model.ClockEvRestart, 0)
	}
	*c.slot(k) = n
	c.last = k
}

func (c *clockModel) nextGPS(p, n *clockNode, t float64) {
	sp := &c.spec
	lost := c.inLockLoss(t)
	gpst := sp.BootGPSTimeS > 0 && t < sp.BootGPSTimeS
	switch {
	case lost && p.locked:
		// lock lost: free-run from here
		n.locked = false
		n.phi = sp.FreeRunPPB * 1e-9
		n.class = model.ClockGPSEmul
		c.newEpoch(n, t, model.ClockEvModeChange, 0)
	case !lost && !p.locked:
		n.locked = true
		n.phi = 0
		n.class = sp.Class
		if gpst {
			n.class = model.ClockGPSGPST
		}
		n.ppsErr = c.rng.NormFloat64() * sp.PPSJitterNs * 1e-9
		n.theta = c.bias + n.ppsErr
		if gpst {
			n.theta += float64(sp.LeapSeconds)
		}
		c.newEpoch(n, t, model.ClockEvModeChange, 0)
	case !lost:
		// disciplined: a new 1PPS error every whole second
		if math.Floor(t+1e-9) != math.Floor(float64(p.k)*gridS+1e-9) {
			n.ppsErr = c.rng.NormFloat64() * sp.PPSJitterNs * 1e-9
		}
		n.theta = c.bias + n.ppsErr
		if p.gpsTime {
			n.theta += float64(sp.LeapSeconds)
		}
		n.phi = 0
	default:
		// free running
		if sp.RandomWalkPPM > 0 {
			n.phi += c.rng.NormFloat64() * sp.RandomWalkPPM * 1e-6 * math.Sqrt(gridS)
		}
	}
	if p.gpsTime && !gpst {
		// switched from GPS time to UTC
		n.gpsTime = false
		n.theta -= float64(sp.LeapSeconds)
		if n.locked {
			n.class = sp.Class
		}
		c.newEpoch(n, t, model.ClockEvModeChange, 0)
	}
}

// node returns the grid node containing t. ok is false when t is outside the
// kept history or not generated yet (the nearest node is returned).
func (c *clockModel) node(t float64) (*clockNode, bool) {
	k := int64(math.Floor(t / gridS))
	switch {
	case k > c.last:
		return c.slot(c.last), false
	case k < 0 || k <= c.last-clockRing:
		lo := max(c.last-clockRing+1, 0)
		return c.slot(lo), false
	}
	return c.slot(k), true
}

// localS returns the receiver's local time (seconds since Start on its own
// clock) at true time t: t + θ(t).
func (n *clockNode) localS(t float64) float64 {
	return t + n.theta + n.phi*(t-float64(n.k)*gridS)
}

// timestamp is what a receiver reports for one frame.
type timestamp struct {
	ticks    uint64
	beastTS  uint64
	epoch    uint32
	class    model.ClockClass
	offsetNs float64 // θ at the stamping instant
	noiseNs  float64 // jitter added
}

// stamp produces the receiver timestamp of a frame whose stamping reference
// point reaches the antenna at true time t. ok is false when the receiver
// was not sampling (sample drop).
func (c *clockModel) stamp(t float64, r *rand.Rand) (ts timestamp, ok bool) {
	n, _ := c.node(t)
	if n.dropEnd > 0 && t < n.dropEnd {
		return ts, false
	}
	theta := n.theta + n.phi*(t-float64(n.k)*gridS)
	noise := 0.0
	if c.spec.JitterNs > 0 {
		noise = r.NormFloat64() * c.spec.JitterNs * 1e-9
	}
	local := t + theta + noise
	ts.epoch, ts.class = n.epoch, n.class
	ts.offsetNs, ts.noiseNs = theta*1e9, noise*1e9
	if c.f == 1e9 {
		ns := c.startNs + int64(math.Floor(local*1e9))
		if ns < 0 {
			ns = 0
		}
		ts.ticks = uint64(ns)
		day := ns % model.NsPerDay
		ts.beastTS = feed.GPSTimestamp(int(day/1e9), int(day%1e9))
		return ts, true
	}
	lq := math.Floor(local/c.quant) * c.quant
	v := n.c0 + lq*c.f
	if v < 0 {
		v = 0
	}
	ts.ticks = uint64(math.Floor(v + 1e-6)) // guard against 4.9999999 after quantisation
	if c.f == 20e6 && c.spec.Class == model.ClockSBS20 {
		ts.beastTS = ts.ticks & 0xFFFFFF
	} else {
		ts.beastTS = ts.ticks & (1<<48 - 1)
	}
	if ts.beastTS == 0 {
		ts.beastTS = 1 // 0 means "no timestamp" in Beast
	}
	return ts, true
}

// truth returns the clock truth at t (within the kept history).
func (c *clockModel) truth(t float64) (ClockTruth, bool) {
	n, ok := c.node(t)
	theta := n.theta + n.phi*(t-float64(n.k)*gridS)
	ct := ClockTruth{Rx: c.rx, AtS: t, Epoch: n.epoch, Class: n.class, OffsetNs: theta * 1e9,
		FreqPPM: n.phi * 1e6, Locked: n.locked, GPSTime: n.gpsTime, PPSErrNs: n.ppsErr * 1e9, LocalS: t + theta}
	if c.f == 1e9 {
		ct.Ticks = float64(c.startNs) + (t+theta)*1e9
	} else {
		ct.Ticks = n.c0 + (t+theta)*c.f
	}
	return ct, ok
}

// Radarcape status bytes (Beast type '4').
const (
	statusSettingsGPS = 0x01 | 0x10 // binary output, GPS timestamps
	statusSettings12  = 0x01        // binary output, 12 MHz timestamps
	gpsAntennaOK      = 0x01
	gpsTracking       = 0x02
	gpsSats3          = 0x04
	gpsUTC            = 0x08
	gpsSync           = 0x10
	gpsTSBad          = 0x20
	gpsTrueUTC        = 0x80
)

// status returns the Radarcape status payload (14 bytes) at local second t.
func (c *clockModel) status(t float64) [14]byte {
	var b [14]byte
	n, _ := c.node(t)
	if c.spec.Device != DeviceRadarcapeGPS {
		b[0] = statusSettings12
		b[2] = gpsAntennaOK | gpsTracking | gpsSats3 | gpsUTC | gpsSync | gpsTrueUTC
		return b
	}
	b[0] = statusSettingsGPS
	b[1] = byte(int8(clamp(math.Round(n.ppsErr*1e9/15.625), -127, 127)))
	switch {
	case !n.locked:
		b[2] = gpsAntennaOK | gpsTracking | gpsTSBad | gpsTrueUTC
	case n.gpsTime:
		b[2] = gpsAntennaOK | gpsTracking | gpsSats3 | gpsSync | gpsTrueUTC
	default:
		b[2] = gpsAntennaOK | gpsTracking | gpsSats3 | gpsUTC | gpsSync | gpsTrueUTC
	}
	return b
}

func sortedSteps(in []ClockStep) []ClockStep {
	out := append([]ClockStep(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].AtS < out[j-1].AtS; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func sortedDrops(in []SampleDrop) []SampleDrop {
	out := append([]SampleDrop(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].AtS < out[j-1].AtS; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func sortedFloats(in []float64) []float64 {
	out := append([]float64(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
