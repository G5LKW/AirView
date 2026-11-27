package simnet

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/sim"
)

// The world is the single-station simulator of internal/sim, configured for
// perfect reception at a virtual station in the middle of the network: every
// transmission it generates is delivered exactly once, together with the
// time its frame reached that virtual station. simnet turns each of those
// frames back into a transmission (address, transmit instant, position) and
// propagates it to every network receiver itself.

// Transmission is one frame sent by an aircraft.
type Transmission struct {
	Seq  uint64 // network-wide sequence number, in order of generation
	ICAO uint32
	Kind model.FrameKind
	Data []byte // the frame as transmitted (read-only)
	// TxS is the true time the leading edge of the preamble left the
	// aircraft antenna, seconds since Config.Start.
	TxS float64
	// Aircraft state at TxS: ECEF position and velocity, geodetic position
	// (height above the WGS-84 ellipsoid).
	Pos           Vec3
	Vel           Vec3
	Lat, Lon, HAE float64
	EIRPdBm       float64

	// Position is set for airborne position squitters whose CPR position
	// simnet re-encoded to model the airframe's position latency and NACp
	// error. ReportedLat/Lon is what the frame says; the truth is Lat/Lon.
	Position     bool
	ReportedLat  float64
	ReportedLon  float64
	PosLatencyS  float64
	PosErrEastM  float64
	PosErrNorthM float64
	// NACp is the navigation accuracy category the aircraft last announced
	// (0 = not yet seen).
	NACp uint8

	hz float64 // radio horizon contribution of the aircraft height, m
}

// AircraftTruth is the true state of one aircraft.
type AircraftTruth struct {
	ICAO          uint32
	AtS           float64
	Pos           Vec3
	Vel           Vec3
	Lat, Lon, HAE float64
	// PosLatencyS is the airframe's position latency (how old the positions
	// it reports are), NACp the category it announces (0 = unknown).
	PosLatencyS float64
	NACp        uint8
	// Sim is the full truth record of internal/sim (callsign, kind,
	// altitudes in feet, speeds ...). Its distances and bearings are relative
	// to the world centre.
	Sim sim.Truth
}

type acState struct {
	icao      uint32
	pos, vel  Vec3
	lat, lon  float64
	hae, eirp float64
	truth     int // index into snapshot.truth
}

type snapshot struct {
	tS    float64
	valid bool
	idx   map[uint32]int
	ac    []acState
	truth []sim.Truth
}

const snapRing = 24 // 2.4 s of aircraft history

type airframe struct {
	latencyS    float64
	nacp        uint8
	errE, errN  float64
	errT        float64
	errScale    float64
	rng         *rand.Rand
	initialized bool
}

type collector struct{ frames []model.RawFrame }

func (c *collector) HandleFrame(f model.RawFrame)     { c.frames = append(c.frames, f) }
func (c *collector) HandleUpdate(model.DecodedUpdate) {}
func (c *collector) reset()                           { c.frames = c.frames[:0] }
func (c *collector) take() []model.RawFrame           { return c.frames }
func (w *world) secondsOf(t time.Time) float64        { return float64(t.UnixNano()-w.startNs) / 1e9 }
func (w *world) timeOf(s float64) time.Time {
	return time.Unix(0, w.startNs+int64(math.Round(s*1e9))).UTC()
}
func (w *world) snap(i int) *snapshot { return &w.snaps[((i%snapRing)+snapRing)%snapRing] }
func (w *world) latest() *snapshot    { return w.snap(w.nsnap - 1) }

type world struct {
	s        *sim.Sim
	seed     uint64
	startNs  int64
	col      collector
	snaps    [snapRing]snapshot
	nsnap    int
	st       Vec3
	air      map[uint32]*airframe
	latMax   float64 // s
	errScale float64
	slab     []byte
	msg      modes.Message
	dropped  uint64
}

func newWorld(cfg *Config, centerLat, centerLon, rangeKm float64) *world {
	sc := sim.Config{
		Aircraft:         cfg.Aircraft,
		Receiver:         model.Receiver{Name: "simnet world", Located: true, Lat: centerLat, Lon: centerLon},
		Antennas:         []model.Antenna{{ID: "omni", Type: model.AntVertical, Channel: 0, GainDBi: 0, HeightAGLM: 10}},
		FeedID:           "simnet",
		Seed:             cfg.Seed,
		Start:            cfg.Start,
		RangeKm:          rangeKm,
		MLATFraction:     cfg.ModeSOnlyFraction,
		GNSSAltFraction:  cfg.GNSSAltFraction,
		MilitaryFraction: cfg.MilitaryFraction,
		EmergencyPerHour: -1,
		NoDiurnal:        !cfg.Diurnal,
		PerfectReception: true,
		NoStatusFrames:   true,
	}
	w := &world{
		s:       sim.New(sc),
		seed:    cfg.Seed,
		startNs: cfg.Start.UnixNano(),
		air:     make(map[uint32]*airframe),
	}
	st := w.s.Station()
	w.st = ECEF(st.Lat, st.Lon, st.GroundElevM)
	if cfg.PosLatencyMaxMs > 0 {
		w.latMax = cfg.PosLatencyMaxMs / 1000
	}
	if cfg.PosErrorScale > 0 {
		w.errScale = cfg.PosErrorScale
	}
	w.takeSnapshot()
	return w
}

// takeSnapshot records the aircraft state at the sim's current time.
func (w *world) takeSnapshot() {
	now := w.s.Now()
	tr := w.s.TruthAt(now)
	sn := w.snap(w.nsnap)
	w.nsnap++
	sn.tS = w.secondsOf(now)
	sn.valid = true
	sn.truth = tr
	if sn.idx == nil {
		sn.idx = make(map[uint32]int, len(tr))
	} else {
		clear(sn.idx)
	}
	sn.ac = sn.ac[:0]
	for i := range tr {
		t := &tr[i]
		hae := t.AltGeom * ft
		if t.OnGround {
			hae = t.AltBaro * ft
		}
		a := acState{icao: t.ICAO, lat: t.Lat, lon: t.Lon, hae: hae, eirp: t.EIRPdBm, truth: i}
		a.pos = ECEF(t.Lat, t.Lon, hae)
		e, n, u := enuBasis(t.Lat, t.Lon)
		gs := t.GS * 1852 / 3600
		st, ct := math.Sincos(t.Track * deg)
		vr := t.VertRate * ft / 60
		a.vel = e.Scale(gs * st).Add(n.Scale(gs * ct)).Add(u.Scale(vr))
		sn.idx[t.ICAO] = len(sn.ac)
		sn.ac = append(sn.ac, a)
	}
}

// stateAt interpolates (or extrapolates) an aircraft's state at t.
func (w *world) stateAt(icao uint32, t float64) (acState, bool) {
	var before, after *acState
	var tb, ta float64
	for i := w.nsnap - 1; i >= 0 && i >= w.nsnap-snapRing; i-- {
		sn := w.snap(i)
		j, ok := sn.idx[icao]
		if !ok {
			continue
		}
		if sn.tS <= t {
			before, tb = &sn.ac[j], sn.tS
			break
		}
		after, ta = &sn.ac[j], sn.tS
	}
	switch {
	case before != nil && after != nil:
		f := (t - tb) / (ta - tb)
		s := *before
		s.pos = before.pos.Add(after.pos.Sub(before.pos).Scale(f))
		s.vel = before.vel.Add(after.vel.Sub(before.vel).Scale(f))
		s.lat = before.lat + (after.lat-before.lat)*f
		dl := after.lon - before.lon
		if dl > 180 {
			dl -= 360
		} else if dl < -180 {
			dl += 360
		}
		s.lon = wrapLon(before.lon + dl*f)
		s.hae = before.hae + (after.hae-before.hae)*f
		return s, true
	case before != nil:
		return extrapolate(*before, t-tb), true
	case after != nil:
		return extrapolate(*after, t-ta), true
	}
	return acState{}, false
}

func extrapolate(s acState, dt float64) acState {
	if dt == 0 {
		return s
	}
	s.pos = s.pos.Add(s.vel.Scale(dt))
	e, n, u := enuBasis(s.lat, s.lon)
	s.lat, s.lon = offsetLatLon(s.lat, s.lon, s.vel.Dot(e)*dt, s.vel.Dot(n)*dt)
	s.hae += s.vel.Dot(u) * dt
	return s
}

// frameICAO returns the transmitting aircraft's address.
func frameICAO(d []byte) (uint32, bool) {
	if len(d) != 7 && len(d) != 14 {
		return 0, false
	}
	switch df := d[0] >> 3; df {
	case 11, 17, 18:
		return uint32(d[1])<<16 | uint32(d[2])<<8 | uint32(d[3]), true
	case 0, 4, 5, 16, 20, 21:
		return modes.Residual(d), true
	}
	return 0, false
}

// step advances the world by one 100 ms step and appends its transmissions.
func (w *world) step(ctx context.Context, seq *uint64, out []Transmission) ([]Transmission, error) {
	w.col.reset()
	if err := w.s.Advance(ctx, 100*time.Millisecond, &w.col); err != nil {
		return out, err
	}
	w.takeSnapshot()
	for i := range w.col.take() {
		f := &w.col.frames[i]
		if f.MLAT || f.Kind == model.FrameStatus || f.GPSNs < 0 {
			continue
		}
		icao, ok := frameICAO(f.Data)
		if !ok {
			w.dropped++
			continue
		}
		// Arrival of the frame start at the world centre, then one
		// light-time iteration back to the transmit instant.
		tArr := w.secondsOf(sim.TransmitTime(*f))
		st, ok := w.stateAt(icao, tArr)
		if !ok {
			w.dropped++
			continue
		}
		tx := tArr - st.pos.Sub(w.st).Norm()/SpeedOfLight
		st, _ = w.stateAt(icao, tx)
		tx = tArr - st.pos.Sub(w.st).Norm()/SpeedOfLight
		*seq++
		t := Transmission{
			Seq: *seq, ICAO: icao, Kind: f.Kind, Data: f.Data, TxS: tx,
			Pos: st.pos, Vel: st.vel, Lat: st.lat, Lon: st.lon, HAE: st.hae, EIRPdBm: st.eirp,
		}
		w.impair(&t)
		out = append(out, t)
	}
	return out, nil
}

// af returns the per-airframe impairment state.
func (w *world) af(icao uint32) *airframe {
	a := w.air[icao]
	if a == nil {
		r := newRand(w.seed, streamAirframe, uint64(icao))
		a = &airframe{rng: r, latencyS: r.Float64() * w.latMax, errScale: w.errScale}
		w.air[icao] = a
	}
	return a
}

// impair learns the announced NACp and re-encodes airborne positions with
// the airframe's position latency and NACp-consistent error.
func (w *world) impair(t *Transmission) {
	d := t.Data
	if len(d) != 14 || d[0]>>3 != 17 {
		if a := w.air[t.ICAO]; a != nil {
			t.NACp = a.nacp
		}
		return
	}
	a := w.af(t.ICAO)
	tc := d[4] >> 3
	if tc == 29 || tc == 31 {
		if err := modes.Decode(d, &w.msg); err == nil && w.msg.Valid&modes.FNACp != 0 {
			a.nacp = w.msg.NACp
		}
	}
	t.NACp = a.nacp
	if !((tc >= 9 && tc <= 18) || (tc >= 20 && tc <= 22)) {
		return
	}
	if (d[6]>>3)&1 == 1 { // T=1: UTC-coupled, encoded for its epoch; leave alone
		return
	}
	if a.latencyS <= 0 && a.errScale <= 0 {
		return
	}
	// position error: first-order Gauss-Markov, τ = 60 s, σ per axis from
	// the announced NACp's 95 % bound (EPU = 2.45 σ).
	if a.errScale > 0 {
		nacp := a.nacp
		if nacp == 0 {
			nacp = 9
		}
		epu := modes.NACpEPU(nacp)
		if math.IsNaN(epu) || epu <= 0 || math.IsInf(epu, 0) {
			epu = 18520
		}
		sigma := epu / 2.45 * a.errScale
		if !a.initialized {
			a.errE, a.errN = a.rng.NormFloat64()*sigma, a.rng.NormFloat64()*sigma
			a.initialized = true
		} else if dt := t.TxS - a.errT; dt > 0 {
			rho := math.Exp(-dt / 60)
			q := sigma * math.Sqrt(1-rho*rho)
			a.errE = a.errE*rho + q*a.rng.NormFloat64()
			a.errN = a.errN*rho + q*a.rng.NormFloat64()
		}
		a.errT = t.TxS
	}
	lat, lon := t.Lat, t.Lon
	if a.latencyS > 0 {
		if s, ok := w.stateAt(t.ICAO, t.TxS-a.latencyS); ok {
			lat, lon = s.lat, s.lon
		}
	}
	lat, lon = offsetLatLon(lat, lon, a.errE, a.errN)
	odd := (d[6]>>2)&1 == 1
	clat, clon := modes.CPREncode(lat, lon, odd, false)
	// Copies live in chunks that are never reused: transmissions of older
	// steps may still reference theirs.
	if cap(w.slab)-len(w.slab) < 14 {
		w.slab = make([]byte, 0, 64<<10)
	}
	n := len(w.slab)
	w.slab = append(w.slab, d...)
	f := w.slab[n : n+14 : n+14]
	me := uint64(f[4])<<48 | uint64(f[5])<<40 | uint64(f[6])<<32 | uint64(f[7])<<24 | uint64(f[8])<<16 | uint64(f[9])<<8 | uint64(f[10])
	me = me&^(1<<34-1) | uint64(clat)<<17 | uint64(clon)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(me >> (48 - 8*i))
	}
	modes.SetParity(f, 0)
	t.Data = f
	t.Position = true
	t.ReportedLat, t.ReportedLon = lat, lon
	t.PosLatencyS = a.latencyS
	t.PosErrEastM, t.PosErrNorthM = a.errE, a.errN
}
