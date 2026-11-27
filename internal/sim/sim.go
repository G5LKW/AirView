package sim

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"airview/internal/model"
)

// Simulation step. The world advances in fixed steps; every transmission
// inside a step gets its own nanosecond timestamp.
const (
	stepNs = 100_000_000
	stepDt = float64(stepNs) / 1e9
	// maxAnt is the maximum number of receive antennas (RF channels) modelled.
	maxAnt = 4
	// maxLagWall is how far (wall time) Run may fall behind its schedule
	// before it stops trying to catch up (host suspended or overloaded).
	maxLagWall = 3 * time.Second
	// utcEpochNs is the ADS-B time-of-applicability grid of T=1 positions.
	utcEpochNs = 200_000_000
	// paceSlackNs groups frames due within this much of each other into one
	// paced delivery.
	paceSlackNs = 1_000_000
)

// Defaults used when the configuration leaves a value at zero.
const (
	DefaultAircraft = 60
	DefaultFeedID   = "demo"
	DefaultRangeKm  = 460.0
	// DefaultLat / DefaultLon / DefaultGroundElevM is the station used when
	// the receiver is not located (Rhine-Main area, Germany).
	DefaultLat         = 50.05
	DefaultLon         = 8.60
	DefaultGroundElevM = 110.0

	defaultMLATFraction     = 0.08
	defaultGNSSAltFraction  = 0.04
	defaultMilitaryFraction = 0.07
	defaultEmergencyPerHour = 0.0004
	maxAircraft             = 20000 // simnet.MaxAircraft (SCALE-11)
	// maxStationLat keeps the simulated world clear of the poles, where the
	// flat lat/lon integration of the aircraft motion breaks down.
	maxStationLat = 70.0
)

// Config configures a simulator. The zero value is usable: every zero field
// takes the documented default.
type Config struct {
	// Aircraft is the number of simultaneously simulated aircraft at the
	// daily traffic peak (default 60). The diurnal curve scales it down to
	// about 20 % in the small hours unless NoDiurnal is set.
	Aircraft int
	// Receiver is the station. Its position is used when Located is true
	// (and the latitude is within +-70 degrees), otherwise DefaultLat /
	// DefaultLon / DefaultGroundElevM.
	Receiver model.Receiver
	// Antennas are the receive antennas, one RF channel each (at most 4 are
	// used). Empty = model.ExampleRadarcapeConfig().Antennas: an omni vertical on
	// channel 0 and a yagi on channel 1.
	Antennas []model.Antenna
	// FeedID is stamped on every RawFrame (default "demo").
	FeedID string

	// Seed makes the simulation reproducible: the same Seed, Start and
	// configuration produce the identical frame sequence. 0 = random seed.
	Seed uint64
	// Start is the virtual UTC time at which the simulation begins. The
	// zero value means "live": time.Now() when New is called, and in real
	// time (Speed 1) the virtual clock is then kept on the wall clock, so
	// RawFrame.Recv is the current time.
	Start time.Time
	// Speed is the pacing of Run and ServeBeast in virtual seconds per wall
	// second (default 1 = real time, 600 = ten minutes per second). Advance
	// and CatchUp ignore it and run as fast as the CPU allows.
	Speed float64

	// RangeKm is the radius of the simulated world around the station:
	// traffic enters and leaves at this distance (default 460).
	RangeKm float64
	// MLATFraction is the share of civil aircraft that are Mode S only and
	// positioned by synthetic MLAT results (default 0.08; negative = none).
	MLATFraction float64
	// GNSSAltFraction is the share of ADS-B aircraft that report GNSS height
	// positions (TC 20) instead of barometric ones (default 0.04; negative = none).
	GNSSAltFraction float64
	// MilitaryFraction is the share of traffic that is military: Mode S
	// only, no position (default 0.07; negative = none).
	MilitaryFraction float64
	// EmergencyPerHour is the probability per aircraft and flight hour of
	// declaring an emergency (7700 / 7600 / 7500). Default 0.0004, i.e. about
	// one per two days with 60 aircraft; negative = never.
	EmergencyPerHour float64

	// NoDiurnal keeps the traffic density constant at the peak value.
	NoDiurnal bool
	// PerfectReception disables the reception model: every transmission is
	// received on every antenna at its mean level (no horizon, fading,
	// garbling or detection losses). Meant for decoder tests.
	PerfectReception bool
	// NoStatusFrames suppresses the Radarcape-style status frame that is
	// otherwise generated on every virtual full second.
	NoStatusFrames bool
	// MagVarDeg is the magnetic variation at the station (east positive)
	// used for magnetic headings. 0 = a crude estimate from the longitude.
	MagVarDeg float64
}

// FromModel builds a simulator configuration from the application config:
// receiver position, antennas and Demo.Aircraft. feedID is the ID of the
// demo feed the frames are attributed to.
func FromModel(c model.Config, feedID string) Config {
	return Config{
		Aircraft: c.Demo.Aircraft,
		Receiver: c.Receiver,
		Antennas: c.Antennas,
		FeedID:   feedID,
	}
}

func fraction(v, def float64) float64 {
	switch {
	case v == 0 || math.IsNaN(v):
		return def
	case v < 0:
		return 0
	}
	return math.Min(v, 1)
}

// Sim is a traffic simulator. Create one with New, then use Run, ServeBeast
// and/or Advance. All methods are safe for concurrent use.
type Sim struct {
	cfg  Config
	seed uint64

	// runMu serialises complete step + delivery cycles (Run's loop, Advance).
	runMu sync.Mutex

	// mu guards the world state below.
	mu       sync.Mutex
	rng      *rand.Rand
	st       station
	ants     []antModel
	antCfg   []model.Antenna
	apts     []airport
	ac       []aircraft
	used     map[uint32]struct{}
	utcSeq   uint64 // ADS-B aircraft created so far (utcCoupling)
	maxAC    int
	regionNA bool
	geoidFt  float64
	dT0      float64
	ph       [8]float64
	env      env
	epochNs  int64 // virtual Unix time (ns) of world time 0
	worldNs  int64 // world time: ns simulated since New
	secNs    int64 // world time of the next once-per-second update
	events   []txEvent
	seq      uint32
	slab     []byte
	batch    []model.RawFrame
	carry    []model.RawFrame // received frames whose Recv falls into a later step
	pendArr  []model.RawFrame // frames arriving after the step end (not yet stamped)
	work     []model.RawFrame // scratch for stampRecv
	spare    []model.RawFrame // second batch buffer (batches ping-pong)
	rxRng    *rand.Rand       // receiver -> host delays (separate: keeps the world sequence)
	lastRecv int64            // Recv of the latest frame (Unix ns): the stream is monotone
	txCount  uint64
	rxCount  uint64
	spawned  uint64
	skipped  int64 // virtual ns the clock was moved forward to follow the wall clock
	live     bool  // in real time the virtual clock follows the wall clock

	// outMu guards the attached outputs (copy-on-write slices).
	outMu   sync.Mutex
	sinks   []*sinkEntry
	clients []*beastClient

	// engMu guards the pacing loop shared by Run and ServeBeast.
	engMu   sync.Mutex
	engRefs int
	engStop context.CancelFunc
	engDone chan struct{}
}

type sinkEntry struct{ sink model.FrameSink }

// New creates a simulator and seeds the picture with traffic that is already
// in flight. It never fails: invalid configuration values fall back to the
// defaults.
func New(cfg Config) *Sim {
	if cfg.Aircraft <= 0 {
		cfg.Aircraft = DefaultAircraft
	}
	if cfg.Aircraft > maxAircraft {
		cfg.Aircraft = maxAircraft
	}
	if cfg.FeedID == "" {
		cfg.FeedID = DefaultFeedID
	}
	if !(cfg.Speed > 0) || math.IsInf(cfg.Speed, 0) {
		cfg.Speed = 1
	}
	if !(cfg.RangeKm >= 50) || math.IsInf(cfg.RangeKm, 0) {
		cfg.RangeKm = DefaultRangeKm
	}
	cfg.RangeKm = math.Min(cfg.RangeKm, 1500)
	cfg.MLATFraction = fraction(cfg.MLATFraction, defaultMLATFraction)
	cfg.GNSSAltFraction = fraction(cfg.GNSSAltFraction, defaultGNSSAltFraction)
	cfg.MilitaryFraction = math.Min(fraction(cfg.MilitaryFraction, defaultMilitaryFraction), 0.9)
	switch {
	case cfg.EmergencyPerHour == 0 || math.IsNaN(cfg.EmergencyPerHour):
		cfg.EmergencyPerHour = defaultEmergencyPerHour
	case cfg.EmergencyPerHour < 0:
		cfg.EmergencyPerHour = 0
	}
	live := cfg.Start.IsZero()
	if live {
		cfg.Start = time.Now()
	}
	cfg.Start = cfg.Start.UTC()

	s := &Sim{seed: cfg.Seed, live: live}
	for s.seed == 0 {
		s.seed = rand.Uint64()
	}
	s.rng = rand.New(rand.NewPCG(s.seed, s.seed^0x9E3779B97F4A7C15))
	s.rxRng = rand.New(rand.NewPCG(s.seed^0xD1B54A32D192ED03, s.seed+1))

	s.st = station{lat: DefaultLat, lon: DefaultLon, groundM: DefaultGroundElevM}
	r := cfg.Receiver
	if r.Located && math.Abs(r.Lat) <= maxStationLat && r.Lon >= -180 && r.Lon <= 180 {
		s.st = station{lat: r.Lat, lon: r.Lon, groundM: clamp(r.GroundElevM, -400, 6000)}
		if math.IsNaN(s.st.groundM) {
			s.st.groundM = 0
		}
	}
	s.regionNA = s.st.lon < -50 && s.st.lon > -170 && s.st.lat > 10
	if cfg.MagVarDeg == 0 || math.IsNaN(cfg.MagVarDeg) {
		if s.regionNA {
			cfg.MagVarDeg = clamp((-90-s.st.lon)*0.45, -18, 18)
		} else {
			cfg.MagVarDeg = clamp(1+0.2*s.st.lon, -8, 10)
		}
	}
	// Geoid undulation (height of the geoid above the ellipsoid), roughly.
	s.geoidFt = 150
	if s.regionNA {
		s.geoidFt = -100
	}

	ants := cfg.Antennas
	if len(ants) == 0 {
		ants = model.ExampleRadarcapeConfig().Antennas
	}
	if len(ants) > maxAnt {
		slog.Warn("sim: more antennas than supported, using the first ones", "configured", len(ants), "used", maxAnt)
		ants = ants[:maxAnt]
	}
	s.antCfg = append([]model.Antenna(nil), ants...)
	cfg.Antennas = s.antCfg
	for i, a := range s.antCfg {
		m := newAntModel(a, i)
		s.antCfg[i].Channel = m.channel
		s.ants = append(s.ants, m)
	}
	s.cfg = cfg

	s.epochNs = cfg.Start.UnixNano()
	s.used = make(map[uint32]struct{}, cfg.Aircraft*2)
	s.maxAC = cfg.Aircraft + cfg.Aircraft/4 + 4
	s.ac = make([]aircraft, 0, s.maxAC)
	s.dT0 = s.uni(-5, 8)
	for i := range s.ph {
		s.ph[i] = s.uni(0, 2*math.Pi)
	}
	s.buildAirports()
	s.updateEnv(0)
	s.secNs = 1e9
	target := int(float64(cfg.Aircraft)*s.env.density + 0.5)
	for len(s.ac) < max(target, 1) {
		s.spawn(s.randomKind(), true, 0)
	}
	return s
}

// Seed returns the random seed in use (the configured one, or the one that
// was drawn when Config.Seed was 0).
func (s *Sim) Seed() uint64 { return s.seed }

// Config returns the effective configuration (defaults filled in).
func (s *Sim) Config() Config {
	c := s.cfg
	c.Seed = s.seed
	c.Antennas = append([]model.Antenna(nil), s.antCfg...)
	return c
}

// Station returns the simulated receiver site.
func (s *Sim) Station() model.Receiver {
	r := s.cfg.Receiver
	if r.Name == "" {
		r.Name = "AirView simulator"
	}
	r.Located = true
	r.Lat, r.Lon, r.GroundElevM = s.st.lat, s.st.lon, s.st.groundM
	return r
}

// Now returns the current virtual time: every frame generated so far is
// older, every future frame is at or after it.
func (s *Sim) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Unix(0, s.epochNs+s.worldNs).UTC()
}

// step advances the world by one step and leaves the received frames in
// s.batch. The caller holds s.mu.
func (s *Sim) step() {
	t0 := float64(s.worldNs) / 1e9
	if s.worldNs >= s.secNs {
		s.secNs += 1e9
		s.updateEnv(t0)
		s.populate(t0)
	}
	s.events = s.events[:0]
	s.seq = 0
	t1 := t0 + stepDt
	for i := range s.ac {
		s.emit(&s.ac[i], int32(i), t0, t1)
	}
	if !s.cfg.NoStatusFrames {
		s.emitStatus()
	}
	s.receive()
	for i := range s.ac {
		s.move(&s.ac[i], t1)
	}
	s.reap()
	s.worldNs += stepNs
}

// Radarcape status frame content (Beast type '4'), see the package comment:
// byte 0 = settings, byte 1 = signed timestamp offset at the last 1PPS edge
// (15 ns units), byte 2 = GPS status.
const (
	// binary output format (0x01), GPS timestamps (0x10)
	statusSettings = 0x01 | 0x10
	// antenna OK (0x01), tracking satellites (0x02), enough good satellites
	// (0x04), time is UTC rather than GPS time (0x08), degradation <= 45 ms (0x10), new style
	// status with the timestamp in UTC (0x80). 0x20 clear = the FPGA
	// timestamp is disciplined by GPS.
	statusGPS = 0x01 | 0x02 | 0x04 | 0x08 | 0x10 | 0x80
)

// emitStatus generates the receiver status frame when a full virtual UTC
// second (the receiver's 1PPS) falls inside the current step.
func (s *Sim) emitStatus() {
	ns0 := s.epochNs + s.worldNs
	pps := (ns0 + 1e9 - 1) / 1e9 * 1e9
	if pps >= ns0+stepNs {
		return
	}
	e := s.newEvent(-1, float64(pps-ns0)/1e9, 14)
	e.flags = evStatus
	e.msg[0] = statusSettings
	e.msg[1] = byte(int8(s.rng.IntN(5) - 2)) // 1PPS offset of the nanosecond counter, 15 ns ticks
	e.msg[2] = statusGPS
}

// runStep performs one step and delivers its frames to extra (may be nil)
// and to every attached output. With pace != nil every frame is handed over
// only once pace(its Recv in Unix ns) has returned true (the live loop waits
// for the wall clock); pace returning false stops the delivery.
func (s *Sim) runStep(ctx context.Context, extra model.FrameSink, block bool, pace func(int64) bool) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.mu.Lock()
	s.step()
	batch := s.batch
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	s.outMu.Lock()
	sinks, clients := s.sinks, s.clients
	s.outMu.Unlock()
	deliver := func(part []model.RawFrame) {
		if extra != nil || len(sinks) > 0 {
			for i := range part {
				if extra != nil {
					extra.HandleFrame(part[i])
				}
				for _, e := range sinks {
					e.sink.HandleFrame(part[i])
				}
			}
		}
		for _, c := range clients {
			c.push(ctx, part, block)
		}
	}
	if pace == nil {
		deliver(batch)
		return
	}
	for i := 0; i < len(batch); {
		due := batch[i].Recv.UnixNano()
		j := i + 1
		for j < len(batch) && batch[j].Recv.UnixNano() <= due+paceSlackNs {
			j++
		}
		if !pace(due) {
			return
		}
		deliver(batch[i:j])
		i = j
	}
}

// Advance runs the simulation forward by d of virtual time as fast as the CPU
// allows (rounded up to whole 100 ms steps), delivering the frames to sink
// (which may be nil) and to every output attached with Run or ServeBeast.
// It is the deterministic way to generate hours of traffic for tests and for
// pre-filling statistics; it does not look at the wall clock. It returns
// ctx.Err() when cancelled early.
func (s *Sim) Advance(ctx context.Context, d time.Duration, sink model.FrameSink) error {
	for n := (int64(d) + stepNs - 1) / stepNs; n > 0; n-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.runStep(ctx, sink, true, nil)
	}
	return nil
}

// CatchUp advances the simulation as fast as possible until the virtual clock
// has reached the wall clock; from then on the simulation is "live" (see
// Config.Start). Typical use: New with Start a day in the past, CatchUp to
// pre-fill statistics, then Run in real time.
func (s *Sim) CatchUp(ctx context.Context, sink model.FrameSink) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		behind := time.Now().UnixNano() - (s.epochNs + s.worldNs)
		s.mu.Unlock()
		if behind < stepNs {
			// from here on the simulation is live
			s.mu.Lock()
			s.live = true
			s.mu.Unlock()
			return nil
		}
		s.runStep(ctx, sink, true, nil)
	}
}

// Run simulates in paced mode (Config.Speed virtual seconds per wall second)
// and delivers every received frame to sink until ctx is cancelled; it then
// returns nil. Frames arrive in time order from a single goroutine, each one
// when the (scaled) wall clock reaches its Recv.
//
// RawFrame fields: Recv = virtual UTC time the host read the frame (the
// current time in a live simulation, see Config.Start): the GPS time of
// arrival plus a receiver -> host delay of ~1.6-5 ms with occasional bursts
// (< 60 ms), monotone over the stream. GPSNs = nanoseconds since UTC
// midnight of the arrival at the antenna, stamped at the END of the frame
// like a Radarcape (transmit time + slant range / c + 64 or 120 us); frames
// come in arrival order. Signal in dBFS, Channel = RF channel of the
// receiving antenna, FeedID from the configuration. A transmission heard by
// two antennas is delivered twice (same Data, different Channel and Signal).
// MLAT results have MLAT = true, GPSNs = -1, Signal = NaN, Channel = -1;
// status frames have Kind = FrameStatus and the 14-byte Radarcape status
// payload. Data must be treated as read-only; it stays valid indefinitely.
//
// Run may be called several times and together with ServeBeast; all of them
// share one simulation, which advances while at least one is active. Once
// Run has returned, its sink receives no further frames. HandleFrame is called
// from the simulation goroutine and must not wait for Run to return.
func (s *Sim) Run(ctx context.Context, sink model.FrameSink) error {
	if sink != nil {
		e := &sinkEntry{sink: sink}
		s.outMu.Lock()
		s.sinks = append(append([]*sinkEntry(nil), s.sinks...), e)
		s.outMu.Unlock()
		defer func() {
			// Holding runMu waits for a step that is being delivered right
			// now, so the sink sees no frame after Run has returned.
			s.runMu.Lock()
			s.outMu.Lock()
			out := make([]*sinkEntry, 0, len(s.sinks))
			for _, x := range s.sinks {
				if x != e {
					out = append(out, x)
				}
			}
			s.sinks = out
			s.outMu.Unlock()
			s.runMu.Unlock()
		}()
	}
	s.acquire()
	defer s.release()
	<-ctx.Done()
	return nil
}

// acquire starts the pacing loop unless it is already running.
func (s *Sim) acquire() {
	s.engMu.Lock()
	defer s.engMu.Unlock()
	s.engRefs++
	if s.engRefs > 1 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.engStop = cancel
	s.engDone = make(chan struct{})
	go s.loop(ctx, s.engDone)
}

// release stops the pacing loop when its last user is gone.
func (s *Sim) release() {
	s.engMu.Lock()
	defer s.engMu.Unlock()
	s.engRefs--
	if s.engRefs > 0 {
		return
	}
	s.engStop()
	<-s.engDone
	s.engStop, s.engDone = nil, nil
}

// loop is the pacing loop: it runs the steps that are due according to the
// wall clock and Config.Speed.
func (s *Sim) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	speed := s.cfg.Speed
	start := time.Now()
	var simulated int64 // virtual ns accounted for since start
	s.mu.Lock()
	follow := s.live && speed == 1
	if follow {
		// Put the virtual clock on the wall clock (it only ever moves
		// forward). Each step below is computed when the wall clock reaches
		// its start and its frames are handed over one by one as the wall
		// clock reaches their Recv, like a real feed: Recv is never in the
		// future and no frame waits for the rest of its step.
		if d := start.UnixNano() - (s.epochNs + s.worldNs); d > 0 {
			s.epochNs += d
			s.skipped += d
		}
	}
	s.mu.Unlock()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	pace := func(base, vs, recv int64) bool {
		due := start.Add(time.Duration(float64(base+recv-vs) / speed))
		if d := time.Until(due); d > 0 {
			timer.Reset(d)
			select {
			case <-ctx.Done():
				return false
			case <-timer.C:
			}
		}
		return ctx.Err() == nil
	}
	for ctx.Err() == nil {
		lag := int64(float64(time.Since(start))*speed) - simulated
		if lag < 0 {
			wait := max(time.Duration(float64(-lag)/speed), time.Millisecond)
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			continue
		}
		if float64(lag)/speed > float64(maxLagWall) {
			// The host was suspended or cannot keep up: do not replay the
			// gap. A live simulation moves its virtual clock forward so that
			// it keeps following the wall clock; otherwise it simply runs late.
			skip := lag
			simulated += skip
			if follow {
				s.mu.Lock()
				s.epochNs += skip
				s.skipped += skip
				s.mu.Unlock()
			}
			slog.Debug("sim: fell behind schedule, skipping ahead", "virtual", time.Duration(skip))
			continue
		}
		s.mu.Lock()
		vs := s.epochNs + s.worldNs
		s.mu.Unlock()
		base := simulated
		s.runStep(ctx, nil, false, func(recv int64) bool { return pace(base, vs, recv) })
		simulated += stepNs
	}
}

// TriggerEmergency makes one airborne aircraft declare a general emergency
// (squawk 7700) right now, preferring ADS-B equipped airliners. It reports
// the aircraft's address, or false when nothing is airborne. For testing
// alerts and for demos.
func (s *Sim) TriggerEmergency() (icao uint32, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best, score := -1, -1
	for i := range s.ac {
		a := &s.ac[i]
		if a.onGround || a.emerg != 0 || a.dead {
			continue
		}
		sc := 0
		if a.adsb {
			sc += 2
		}
		if a.jet() && a.kind != kMil {
			sc++
		}
		if a.stDist < s.cfg.RangeKm*0.6 {
			sc += 4
		}
		if sc > score {
			best, score = i, sc
		}
	}
	if best < 0 {
		return 0, false
	}
	a := &s.ac[best]
	a.emerg, a.squawk = 1, 0x7700
	a.sqChanged = float64(s.worldNs) / 1e9
	return a.icao, true
}

// Stats is a snapshot of the simulator's counters and environment.
type Stats struct {
	Now          time.Time // virtual time
	Aircraft     int       // aircraft currently simulated
	Spawned      uint64    // aircraft created since New
	Transmitted  uint64    // off-air transmissions generated
	Received     uint64    // frames delivered (all channels, including MLAT results)
	BeastClients int       // connected Beast TCP clients
	// ClockSkipped is how far the virtual clock of a live simulation was
	// moved forward to keep following the wall clock (start of Run, host
	// suspended or overloaded).
	ClockSkipped time.Duration

	TrafficDensity float64 // diurnal traffic factor 0..1
	KFactor        float64 // effective earth radius factor (4/3 = standard)
	QNH            float64 // hPa
	ISADeviation   float64 // deg C
	SurfaceWindDir float64 // deg true, blowing from
	SurfaceWindKt  float64
}

// Stats returns the current counters and environment.
func (s *Sim) Stats() Stats {
	s.outMu.Lock()
	nc := len(s.clients)
	s.outMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, kt := s.windAt(0)
	return Stats{
		Now:            time.Unix(0, s.epochNs+s.worldNs).UTC(),
		Aircraft:       len(s.ac),
		Spawned:        s.spawned,
		Transmitted:    s.txCount,
		Received:       s.rxCount,
		BeastClients:   nc,
		ClockSkipped:   time.Duration(s.skipped),
		TrafficDensity: s.env.density,
		KFactor:        s.env.kFactor,
		QNH:            s.env.qnh,
		ISADeviation:   s.env.dT,
		SurfaceWindDir: dir,
		SurfaceWindKt:  kt,
	}
}

// errAntenna is returned by ServeBeastOn for an unknown antenna ID.
var errAntenna = errors.New("sim: unknown antenna ID")
