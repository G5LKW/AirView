package simnet

import (
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
	"airview/internal/sim"
)

// Observation is one frame as received by one receiver: the truth of how it
// got there and what the receiver reports.
type Observation struct {
	Rx int // receiver index (Config.Receivers)
	// Tx is the transmission; valid only during the sink call.
	Tx   *Transmission
	Seq  uint64 // Tx.Seq
	ICAO uint32
	Kind model.FrameKind
	Data []byte // the frame (read-only, may outlive the call)

	// Truth.
	ArrivalS    float64 // leading edge of the preamble at the antenna, s since Start
	RangeM      float64 // geometric slant range at TxS
	ExcessM     float64 // refraction excess path
	MultipathNs float64 // extra delay of the stamped pulse
	OutlierNs   float64 // gross timing error (0 = none)
	ElevDeg     float64
	AzDeg       float64
	PowerDBm    float64 // at the receiver input
	// ClockOffsetNs is θ (local minus true time) at the stamping instant,
	// NoiseNs the white jitter added; RefNs the frame reference offset
	// (where in the frame the receiver stamps, incl. ShortLongErrorNs).
	ClockOffsetNs float64
	NoiseNs       float64
	RefNs         float64

	// As reported by the receiver.
	SignalDBFS float64
	SignalByte byte
	Class      model.ClockClass
	Epoch      uint32
	Ticks      uint64 // unwrapped counter (GPS classes: Unix / GPS-time ns)
	BeastTS    uint64 // raw timestamp as in the Beast output (48 bit; 24 bit for SBS)
	HostNs     int64  // feeder host clock (Unix ns) when the frame was read
}

// Sink receives the observations of every receiver, merged in arrival order
// (ties broken by receiver index and transmission). Calls come from one
// goroutine; the slice and Tx pointers are valid only during the call.
type Sink interface {
	Observe(obs []Observation)
	// Advance reports that every observation with ArrivalS <= horizonS has
	// been delivered.
	Advance(horizonS float64)
}

// RxSink receives the observations of one receiver in arrival order. The
// calls for one receiver are sequential; different receivers' sinks may be
// called concurrently.
type RxSink interface {
	Observe(o *Observation)
	Advance(horizonS float64)
}

// Stats are network counters.
type Stats struct {
	NowS          float64
	Steps         uint64
	Aircraft      int
	Transmissions uint64
	Candidates    uint64 // receiver/transmission pairs evaluated in detail
	Observations  uint64 // delivered observations
	Dropped       uint64 // world frames without an attributable aircraft
}

type receiver struct {
	idx      int
	spec     *ReceiverSpec
	pos, up  Vec3
	east     Vec3
	north    Vec3
	rxR      float64 // ellipsoid radius under the antenna
	hz       float64 // radio horizon term of the antenna
	maxRange float64
	clk      *clockModel
	rng      *rand.Rand // reception and per-frame noise
	hrng     *rand.Rand // host timing
	pending  []Observation
	out      []Observation
	lastHost int64
	sinks    []RxSink
	cand     uint64
	obs      uint64
}

const txRingN = 16

// Network is a simulated receiver network. Create it with New and drive it
// with Step / Advance. It is not safe for concurrent use.
type Network struct {
	cfg     Config
	w       *world
	rx      []*receiver
	startNs int64
	k       int64
	nowS    float64
	seq     uint64
	txRing  [txRingN][]Transmission
	sinks   []Sink
	merged  []Observation
	stats   Stats
	horizon float64
}

// New builds a network. The configuration is copied and normalised; invalid
// values are reported.
func New(cfg Config) (*Network, error) {
	cfg.Receivers = slices.Clone(cfg.Receivers)
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	n := &Network{cfg: cfg, startNs: cfg.Start.UnixNano(), horizon: -cfg.HoldbackS}
	n.w = newWorld(&n.cfg, cfg.CenterLat, cfg.CenterLon, cfg.RangeKm)
	atm := &n.cfg.Atmosphere
	for i := range n.cfg.Receivers {
		s := &n.cfg.Receivers[i]
		r := &receiver{idx: i, spec: s}
		hae := s.GroundHAE + s.AntennaAGL
		r.pos = ECEF(s.Lat, s.Lon, hae)
		r.east, r.north, r.up = enuBasis(s.Lat, s.Lon)
		r.rxR = r.pos.Norm() - hae
		r.hz = atm.horizonM(s.Reception.HorizonHeightM)
		rs := &s.Reception
		// link-budget range for the strongest transmitter (EIRP 60 dBm)
		budget := 60 + rs.AntennaGainDBi - rs.CableLossDB - rs.SensitivityDBm + 4*rs.DetectWidthDB + 3*rs.FadingDB
		r.maxRange = math.Pow(10, (budget-fsplConst)/20)
		if rs.MaxRangeKm > 0 {
			r.maxRange = math.Min(r.maxRange, rs.MaxRangeKm*1000)
		}
		r.clk = newClockModel(i, s.Clock, n.startNs, cfg.Seed)
		r.rng = newRand(cfg.Seed, streamRecv, uint64(i))
		r.hrng = newRand(cfg.Seed, streamHost, uint64(i))
		n.rx = append(n.rx, r)
	}
	return n, nil
}

// Config returns the effective configuration.
func (n *Network) Config() Config { return n.cfg }

// Sim returns the underlying single-station world simulator (read-only use:
// Truth, Airports, Stats).
func (n *Network) Sim() *sim.Sim { return n.w.s }

// Start returns the virtual start time.
func (n *Network) Start() time.Time { return n.cfg.Start }

// NowS returns the world time in seconds since Start. Observations are
// delivered up to HorizonS (NowS - HoldbackS).
func (n *Network) NowS() float64 { return n.nowS }

// HorizonS returns the time up to which observations have been delivered.
func (n *Network) HorizonS() float64 { return n.horizon }

// Time converts seconds since Start to a UTC time (ns resolution).
func (n *Network) Time(s float64) time.Time {
	return time.Unix(0, n.startNs+int64(math.Round(s*1e9))).UTC()
}

// UnixNs converts seconds since Start to Unix nanoseconds as float64
// (sub-ns resolution is kept relative to Start: use for differences).
func (n *Network) UnixNs(s float64) int64 { return n.startNs + int64(math.Round(s*1e9)) }

// Seconds converts a time to seconds since Start.
func (n *Network) Seconds(t time.Time) float64 { return float64(t.UnixNano()-n.startNs) / 1e9 }

// NumReceivers returns the number of receivers.
func (n *Network) NumReceivers() int { return len(n.rx) }

// AddSink attaches a network-wide sink.
func (n *Network) AddSink(s Sink) { n.sinks = append(n.sinks, s) }

// Attach attaches a per-receiver sink (emitters).
func (n *Network) Attach(rx int, s RxSink) { n.rx[rx].sinks = append(n.rx[rx].sinks, s) }

// Stats returns the counters.
func (n *Network) Stats() Stats {
	s := n.stats
	s.NowS = n.nowS
	s.Dropped = n.w.dropped
	s.Aircraft = len(n.w.latest().ac)
	for _, r := range n.rx {
		s.Candidates += r.cand
		s.Observations += r.obs
	}
	return s
}

// Advance runs the network for d of virtual time (whole 100 ms steps).
func (n *Network) Advance(ctx context.Context, d time.Duration) error {
	for i := (int64(d) + 99_999_999) / 100_000_000; i > 0; i-- {
		if err := n.Step(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Step advances the network by one 100 ms step and delivers the
// observations that became final.
func (n *Network) Step(ctx context.Context) error {
	slot := &n.txRing[n.k%txRingN]
	txs, err := n.w.step(ctx, &n.seq, (*slot)[:0])
	*slot = txs
	if err != nil {
		return err
	}
	n.k++
	n.nowS = n.w.latest().tS
	n.stats.Steps++
	n.stats.Transmissions += uint64(len(txs))
	for i := range txs {
		txs[i].hz = n.cfg.Atmosphere.horizonM(txs[i].HAE)
	}
	horizon := n.nowS - n.cfg.HoldbackS
	n.horizon = horizon

	// Evaluate receivers in parallel. Each receiver only uses its own
	// random streams, so the result does not depend on the scheduling.
	var next atomic.Int64
	work := func() {
		for {
			i := int(next.Add(8)) - 8
			if i >= len(n.rx) {
				return
			}
			for j := i; j < min(i+8, len(n.rx)); j++ {
				n.runReceiver(n.rx[j], txs, horizon)
			}
		}
	}
	if w := min(n.cfg.Workers, (len(n.rx)+7)/8); w > 1 {
		var wg sync.WaitGroup
		for range w {
			wg.Go(work)
		}
		wg.Wait()
	} else {
		work()
	}

	if len(n.sinks) > 0 {
		n.merged = n.merged[:0]
		for _, r := range n.rx {
			n.merged = append(n.merged, r.out...)
		}
		slices.SortFunc(n.merged, func(a, b Observation) int {
			switch {
			case a.ArrivalS < b.ArrivalS:
				return -1
			case a.ArrivalS > b.ArrivalS:
				return 1
			case a.Rx != b.Rx:
				return a.Rx - b.Rx
			}
			return int(int64(a.Seq) - int64(b.Seq))
		})
		for _, s := range n.sinks {
			s.Observe(n.merged)
			s.Advance(horizon)
		}
	}
	return nil
}

// runReceiver evaluates one receiver for the new transmissions, then
// releases its observations up to the horizon in arrival order.
func (n *Network) runReceiver(r *receiver, txs []Transmission, horizon float64) {
	r.clk.advanceTo(n.nowS + 0.2)
	for i := range txs {
		n.receive(r, &txs[i])
	}
	slices.SortFunc(r.pending, func(a, b Observation) int {
		switch {
		case a.ArrivalS < b.ArrivalS:
			return -1
		case a.ArrivalS > b.ArrivalS:
			return 1
		}
		return int(int64(a.Seq) - int64(b.Seq))
	})
	cut := 0
	for cut < len(r.pending) && r.pending[cut].ArrivalS <= horizon {
		cut++
	}
	r.out = append(r.out[:0], r.pending[:cut]...)
	r.pending = append(r.pending[:0], r.pending[cut:]...)
	cs := &r.spec.Clock
	for i := range r.out {
		o := &r.out[i]
		// host time: block processing, latency, host clock error; reads are
		// sequential, so host time never goes backwards
		t := o.ArrivalS
		if cs.HostBlockMs > 0 {
			b := cs.HostBlockMs / 1000
			t = (math.Floor(t/b) + 1) * b
		}
		t += cs.HostDelayMs / 1000
		if cs.HostDelayJitterMs > 0 {
			t += expRand(r.hrng, cs.HostDelayJitterMs/1000)
		}
		t += (cs.HostOffsetMs + cs.HostWanderMs*math.Sin(2*math.Pi*o.ArrivalS/7200+float64(r.idx))) / 1000
		h := n.startNs + int64(math.Round(t*1e9))
		if h < r.lastHost {
			h = r.lastHost
		}
		r.lastHost = h
		o.HostNs = h
		for _, s := range r.sinks {
			s.Observe(o)
		}
	}
	r.obs += uint64(len(r.out))
	for _, s := range r.sinks {
		s.Advance(horizon)
	}
}

// receive evaluates one transmission at one receiver.
func (n *Network) receive(r *receiver, tx *Transmission) {
	d := tx.Pos.Sub(r.pos)
	d2 := d.Dot(d)
	lim := (r.hz + tx.hz) * 1.25
	if lim > r.maxRange {
		lim = r.maxRange
	}
	if d2 > lim*lim {
		return
	}
	r.cand++
	rs := &r.spec.Reception
	rng := r.rng
	dist := math.Sqrt(d2)
	sinEl := d.Dot(r.up) / dist
	elev := math.Asin(clamp(sinEl, -1, 1)) / deg
	az := 0.0
	if rs.hasMask() {
		az = math.Atan2(d.Dot(r.east), d.Dot(r.north)) / deg
		if az < 0 {
			az += 360
		}
		if elev < rs.maskAt(az) {
			return
		}
	}
	// link budget
	p := tx.EIRPdBm - (20*math.Log10(dist) + fsplConst) + rs.antennaGain(sinEl) - rs.CableLossDB
	// soft radio horizon (diffraction edge)
	hz := r.hz + tx.hz
	if u := (dist - hz) / (0.03 * hz); u > -1 {
		p -= 6 * (u + 1)
	}
	if rs.FadingDB > 0 {
		p += rng.NormFloat64() * rs.FadingDB
	}
	thr := rs.SensitivityDBm
	if tx.Kind != model.FrameModeSLong {
		thr--
	}
	if rng.Float64() >= 1/(1+math.Exp(-(p-thr)/rs.DetectWidthDB)) {
		return
	}
	if rs.LossProb > 0 && rng.Float64() < rs.LossProb {
		return
	}
	// timing
	excess := n.cfg.Atmosphere.ExcessPathM(r.pos, tx.Pos, r.rxR, dist)
	arr := tx.TxS + (dist+excess)/SpeedOfLight
	var mp, outl float64
	if rs.MultipathProb > 0 {
		pm := rs.MultipathProb
		if elev < 3 {
			pm = math.Min(1, 4*pm)
		}
		if rng.Float64() < pm {
			mp = expRand(rng, rs.MultipathMeanNs)
		}
	}
	if rs.OutlierProb > 0 && rng.Float64() < rs.OutlierProb {
		outl = uniform(rng, -1, 1) * rs.OutlierMaxNs
		if outl == 0 {
			outl = rs.OutlierMaxNs
		}
	}
	cs := &r.spec.Clock
	var ref float64
	switch tx.Kind {
	case model.FrameModeAC:
		ref = cs.FrameRefACNs
	case model.FrameModeSShort:
		ref = cs.FrameRefShortNs + cs.ShortLongErrorNs
	default:
		ref = cs.FrameRefLongNs
	}
	ts, ok := r.clk.stamp(arr+(ref+mp+outl)*1e-9, rng)
	if !ok {
		return
	}
	dbfs := clamp(p-rs.FullScaleDBm, -48.1, 0)
	r.pending = append(r.pending, Observation{
		Rx: r.idx, Tx: tx, Seq: tx.Seq, ICAO: tx.ICAO, Kind: tx.Kind, Data: tx.Data,
		ArrivalS: arr, RangeM: dist, ExcessM: excess, MultipathNs: mp, OutlierNs: outl,
		ElevDeg: elev, AzDeg: az, PowerDBm: p,
		ClockOffsetNs: ts.offsetNs, NoiseNs: ts.noiseNs, RefNs: ref,
		SignalDBFS: dbfs, SignalByte: signalByte(dbfs),
		Class: ts.class, Epoch: ts.epoch, Ticks: ts.ticks, BeastTS: ts.beastTS,
	})
}

// RawFrame converts an observation into the model.RawFrame a network
// ingest would produce for it (station index rx+2, so 0 = local and 1 =
// host stay free).
func (n *Network) RawFrame(o *Observation) model.RawFrame {
	f := model.RawFrame{
		Recv:     time.Unix(0, o.HostNs).UTC(),
		GPSNs:    -1,
		Signal:   o.SignalDBFS,
		Kind:     o.Kind,
		Data:     o.Data,
		FeedID:   n.cfg.Receivers[o.Rx].ID,
		Channel:  -1,
		Station:  model.StationIdx(o.Rx + 2),
		Epoch:    o.Epoch,
		ClockCls: o.Class,
		Ticks:    o.Ticks,
		HostNs:   o.HostNs,
	}
	if o.Class.GPS() {
		f.GPSNs = int64(o.Ticks % uint64(model.NsPerDay))
	} else {
		f.Clock12MHz = o.BeastTS
	}
	return f
}

// beastType returns the Beast frame type of a frame kind.
func beastType(k model.FrameKind) byte {
	switch k {
	case model.FrameModeAC:
		return feed.BeastModeAC
	case model.FrameModeSShort:
		return feed.BeastModeSShort
	}
	return feed.BeastModeSLong
}
