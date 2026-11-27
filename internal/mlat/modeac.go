package mlat

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/group"
	"airview/internal/mlat/solve"
	"airview/internal/mlat/track"
	"airview/internal/model"
)

// Mode A/C multilateration (DESIGN §9.9; phase 5, off by default:
// aggregator.mlat.modeAc). Mode A/C replies carry no address, so they never
// reach the ingest grouper's MLAT path. The links hand them to
// ObserveModeAC / ObserveModeACFrames; the engine converts them to network
// time with the current clock snapshot, holds them for ModeACHoldNs (host
// latency), associates them by code and time among the planner's Mode A/C
// stations (anchors with anchor peers within 150 km; group.ModeAC: maximum
// cliques of the pairwise TDOA gate |Δt| ≤ d/c + 2 µs + nσ), solves every
// cluster of ≥ 4 stations (seeded by a matching target's prediction), and
// feeds the fixes and the 2-3 station clusters to the Mode A/C tracker
// (track.ACTracker: synthetic identities, code bookkeeping, grades). Shown
// targets are published once a second as ModeACTarget records
// (ModeACTargets, ModeACAircraft); every accepted fix also goes to the
// EnableModeAC callback. See doc.go "Mode A/C" and WIRING.md §5.

// ModeACReply is one Mode A/C reply heard by a station.
type ModeACReply struct {
	Station model.StationIdx
	Epoch   uint32
	Ticks   uint64
	// Code is the reply in the raw pulse layout (C1 in bit 12 … D4 in bit
	// 0, X in bit 6: see track.ACSquawk). ModeACReplyFromFrame converts a
	// received 2-byte payload.
	Code uint16
}

// ModeACFix is a Mode A/C multilateration result (one solved reply). It has
// no address; Target is the synthetic identity the tracker associated it
// with (0: none).
type ModeACFix struct {
	Code      uint16
	TUnixNs   int64
	ECEF      [3]float64
	SigmaHM   float64
	NStations int
	Grade     uint8
	Target    uint32
}

// ModeACHoldNs is how long replies are held before association.
const ModeACHoldNs = int64(600e6)

// ModeACJitterNs is the timing noise (1σ) of a Mode A/C reply on top of the
// station's Mode S jitter: the reply has no preamble to correlate on, and
// its framing pulse is short with a slow (0.05-0.1 µs) rise, so receivers
// time it less precisely than a Mode S preamble. Added in quadrature to the
// clock's jitter variance (Options.ModeACJitterNs overrides it).
const ModeACJitterNs = 50.0

// modeACMaxBuf bounds the reply buffer (replies beyond it are dropped and
// counted): 10 s of 50,000 replies/s.
const modeACMaxBuf = 500_000

// Code formats of the 2-byte Mode A/C payload (Options.ModeACCodeFormat).
const (
	// ModeACPulses: the payload's low 13 bits are the reply pulses in
	// transmission order (C1 … D4, X in bit 6). Default.
	ModeACPulses = "pulses"
	// ModeACNibbles: the payload is the 4-digit octal code with one digit
	// per nibble (0xABCD).
	ModeACNibbles = "nibbles"
)

// ModeACReplyFromFrame converts a received Mode A/C frame (kind
// model.FrameModeAC, 2 bytes, with a timestamp) into a reply. format is
// ModeACPulses or ModeACNibbles ("" = pulses). ok is false for other frames,
// untimed frames and payloads that are invalid in the format.
func ModeACReplyFromFrame(f *model.RawFrame, format string) (ModeACReply, bool) {
	if f.Kind != model.FrameModeAC || len(f.Data) != 2 || f.ClockCls == model.ClockNone || f.Ticks == 0 || f.MLAT {
		return ModeACReply{}, false
	}
	v := uint16(f.Data[0])<<8 | uint16(f.Data[1])
	var code uint16
	switch format {
	case ModeACNibbles:
		c, ok := track.ACRawFromNibbles(v)
		if !ok {
			return ModeACReply{}, false
		}
		code = c
	default:
		if v&^0x1FFF != 0 {
			return ModeACReply{}, false
		}
		code = v
	}
	if code&track.ACCodeMask == 0 {
		return ModeACReply{}, false // all-zero: heartbeats and empty replies carry nothing to associate
	}
	return ModeACReply{Station: f.Station, Epoch: f.Epoch, Ticks: f.Ticks, Code: code}, true
}

// ModeACTarget is the map record of one shown Mode A/C target (public
// scope: no station identities). IDs are synthetic and stable for the
// target's lifetime only.
type ModeACTarget struct {
	// ID is "#" + 6 hex digits of the synthetic identity (never a valid
	// ICAO address string); Key the identity (track.ACIDFlag set).
	ID  string `json:"id"`
	Key uint32 `json:"key"`
	// Squawk is the identity code (4 octal digits) when known; AltFt the
	// Mode C pressure altitude.
	Squawk string `json:"squawk,omitempty"`
	AltFt  *int   `json:"altFt,omitempty"`
	// Lat/Lon and the geometric height (HAE, m) of the MLAT track.
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	AltGeoM float64 `json:"altGeomM"`
	// GS (kt), Track (deg true) and VertRate (ft/min) of the track.
	GS       float64 `json:"gs"`
	Track    float64 `json:"track"`
	VertRate float64 `json:"vertRate"`
	// SigmaHM is the horizontal 1σ (m); Grade "A".."D".
	SigmaHM float64 `json:"sigmaHM"`
	Grade   string  `json:"grade"`
	// NBucket is the station-count bucket of the last fix (model.NBucket*).
	NBucket uint8 `json:"nBucket"`
	// Fixes counts the solved replies, Updates all accepted measurements.
	Fixes   uint32 `json:"fixes"`
	Updates uint32 `json:"updates"`
	// AgeS is the time since the last accepted measurement; SeenS the
	// target's age.
	AgeS  float64 `json:"ageS"`
	SeenS float64 `json:"seenS"`
	// Crowded: another target with the same identity code is close by
	// (association by position only; graded one class worse).
	Crowded bool `json:"crowded,omitempty"`
	// TUnixNs is the time of the state (Unix ns).
	TUnixNs int64 `json:"t"`
}

// Aircraft converts the target into the map's aircraft DTO: ICAO is the
// "#"-prefixed synthetic ID, AddrType "modeac", Source mlat.
func (t *ModeACTarget) Aircraft() model.Aircraft {
	lat, lon := t.Lat, t.Lon
	gs, trk, vr := t.GS, t.Track, t.VertRate
	geom := math.Round(t.AltGeoM / 0.3048)
	age := t.AgeS
	a := model.Aircraft{ICAO: t.ID, AddrType: "modeac", Squawk: t.Squawk, Lat: &lat, Lon: &lon,
		Source: model.SrcMLAT, PosAge: &age, GS: &gs, Track: &trk, VertRate: &vr, AltGeom: &geom}
	if t.AltFt != nil {
		b := float64(*t.AltFt)
		a.AltBaro = &b
	}
	return a
}

// ModeACStats are the Mode A/C counters (admin scope).
type ModeACStats struct {
	Enabled  bool   `json:"enabled"`
	Replies  uint64 `json:"replies"`  // replies accepted into the buffer
	Unsynced uint64 `json:"unsynced"` // dropped: station not synced
	Overflow uint64 `json:"overflow"` // dropped: buffer full
	Clusters uint64 `json:"clusters"` // clusters of ≥ 2 stations
	Solves   uint64 `json:"solves"`   // clusters of ≥ 4 stations solved
	SolveOK  uint64 `json:"solveOk"`
	Small    uint64 `json:"small"` // 2-3 station clusters offered to the tracker
	// Stations is the number of stations taking part (planner).
	Stations int `json:"stations"`
	// Shown is the number of targets in the last published snapshot.
	Shown int           `json:"shown"`
	Track track.ACStats `json:"track"`
}

type modeACState struct {
	mu     sync.Mutex
	m      *group.ModeAC
	buf    []group.ACObs
	keep   []group.ACObs
	sv     *solve.Solver
	svAtm  geo.Atmosphere
	clk    clockAdapter
	sites  sitesAdapter
	out    func(ModeACFix)
	sst    []solve.Station
	toas   []track.TOA
	ac     *track.ACTracker
	est    track.Estimate
	fix    solve.Fix
	nextSw int64
	jit2   float64 // ModeACJitterNs²

	replies, unsynced, overflow, clusters, solves, solveOK, small atomic.Uint64

	targets atomic.Pointer[[]ModeACTarget]
}

// ErrModeACDisabled is returned by EnableModeAC when aggregator.mlat.modeAc
// is off.
var ErrModeACDisabled = errors.New("mlat: Mode A/C MLAT is disabled (aggregator.mlat.modeAc)")

// newModeAC builds the Mode A/C path (New calls it when Config.ModeAC is
// set).
func (e *Engine) newModeAC() (*modeACState, error) {
	st := &modeACState{}
	j := e.o.ModeACJitterNs
	if j == 0 {
		j = ModeACJitterNs
	}
	st.jit2 = math.Max(j, 0) * math.Max(j, 0)
	gc := group.ConfigFromModel(e.cfg.Group)
	m, err := group.NewModeAC(gc, &st.sites, e.planner.ModeACEligible)
	if err != nil {
		return nil, err
	}
	st.m = m
	so := solve.OptionsFromConfig(e.cfg.Solve)
	so.Atmosphere = e.Atmosphere()
	st.svAtm = so.Atmosphere
	st.sv = solve.NewSolver(so)
	ap := track.ACParams{Track: track.ParamsFromConfig(e.cfg.Track)}
	// the A/C lifecycle defaults (bursty SSR replies) unless configured
	ap.Track.ConfirmWindowS, ap.Track.ConfirmUpdates, ap.Track.CoastS = 0, 0, 0
	ap.Track.Atmosphere = e.Atmosphere()
	st.ac = track.NewACTracker(ap)
	empty := []ModeACTarget{}
	st.targets.Store(&empty)
	return st, nil
}

// EnableModeAC sets the callback that receives every accepted Mode A/C fix
// (nil = none). It requires Config.ModeAC; the Mode A/C path itself runs
// whenever Config.ModeAC is set (ModeACTargets works without a callback).
func (e *Engine) EnableModeAC(out func(ModeACFix)) error {
	if !e.cfg.ModeAC {
		return ErrModeACDisabled
	}
	st := e.modeAC.Load()
	if st == nil {
		var err error
		if st, err = e.newModeAC(); err != nil {
			return err
		}
		e.modeAC.Store(st)
	}
	st.mu.Lock()
	st.out = out
	st.mu.Unlock()
	return nil
}

// ObserveModeAC queues Mode A/C replies (any goroutine). Replies of
// stations that are not synced are dropped. A no-op while Mode A/C MLAT is
// off.
func (e *Engine) ObserveModeAC(rs []ModeACReply) {
	st := e.modeAC.Load()
	if st == nil || len(rs) == 0 {
		return
	}
	snap := e.filter.Snapshot()
	st.mu.Lock()
	for _, r := range rs {
		if r.Code&track.ACCodeMask == 0 {
			continue
		}
		ns, frac, cv, jv, ok := snap.NetTimeUnix(clock.Query{Station: r.Station, Epoch: r.Epoch, Ticks: r.Ticks, Kind: model.FrameModeAC})
		if !ok {
			st.unsynced.Add(1)
			continue
		}
		if len(st.buf) >= modeACMaxBuf {
			st.overflow.Add(1)
			continue
		}
		st.replies.Add(1)
		jv += st.jit2
		st.buf = append(st.buf, group.ACObs{Station: r.Station, Code: r.Code & track.ACCodeMask, T: group.MakeTime(ns, frac), Var: cv + jv, JVar: jv})
	}
	st.mu.Unlock()
}

// ObserveModeACFrames converts received Mode A/C frames
// (ModeACReplyFromFrame with Options.ModeACCodeFormat) and queues them;
// other frames are ignored. For the links (ingest, AVF, mlat-client).
func (e *Engine) ObserveModeACFrames(fs []model.RawFrame) {
	if e.modeAC.Load() == nil {
		return
	}
	var tmp [64]ModeACReply
	rs := tmp[:0]
	for i := range fs {
		if r, ok := ModeACReplyFromFrame(&fs[i], e.o.ModeACCodeFormat); ok {
			rs = append(rs, r)
		}
	}
	e.ObserveModeAC(rs)
}

// flushModeAC associates and solves the replies older than ModeACHoldNs
// and publishes the targets once a second (called from Advance).
func (e *Engine) flushModeAC(nowNs int64) {
	st := e.modeAC.Load()
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if n := len(st.buf); n > 0 {
		lim := nowNs - ModeACHoldNs
		sort.Slice(st.buf, func(i, j int) bool { return st.buf[i].T.Before(st.buf[j].T) })
		k := sort.Search(n, func(i int) bool { return st.buf[i].T.UnixNs() > lim })
		if k > 0 {
			st.clk.snap = e.filter.Snapshot()
			st.sites.tab = e.st.load()
			if at := e.Atmosphere(); at != st.svAtm {
				st.sv.SetAtmosphere(at)
				st.svAtm = at
			}
			st.m.ClusterMin(st.buf[:k], 2, func(c *group.ACCluster) { e.modeACCluster(st, c) })
			st.keep = append(st.keep[:0], st.buf[k:]...)
			st.buf, st.keep = st.keep, st.buf
		}
	}
	if nowNs >= st.nextSw {
		st.nextSw = nowNs + 1e9
		e.publishModeAC(st, nowNs)
	}
}

// modeACCluster handles one associated cluster: ≥ 4 stations are solved,
// 2-3 station clusters update a matching target through their TDOAs.
func (e *Engine) modeACCluster(st *modeACState, c *group.ACCluster) {
	st.clusters.Add(1)
	if len(c.Members) >= 4 {
		e.solveModeAC(st, c)
		return
	}
	st.small.Add(1)
	ep := c.Members[0].T
	epNs := ep.UnixNs()
	base := group.MakeTime(epNs, 0)
	st.toas = st.toas[:0]
	for _, m := range c.Members {
		jv, cv := m.JVar, math.Max(m.Var-m.JVar, 0)
		if jv <= 0 {
			jv, cv = math.Max(m.Var, 1), 0
		}
		st.toas = append(st.toas, track.TOA{Station: uint32(m.Station), Pos: m.Pos, T: m.T.Sub(base) * 1e-9,
			Sigma: math.Sqrt(jv) * 1e-9, SyncSigma: math.Sqrt(cv) * 1e-9})
	}
	cl := track.Cluster{T0Ns: epNs, TOAs: st.toas}
	st.ac.Cluster(c.Code, &cl)
}

func (e *Engine) solveModeAC(st *modeACState, c *group.ACCluster) {
	if len(c.Members) < 4 {
		return
	}
	n := min(len(c.Members), solve.MaxStations)
	ep := c.Members[0].T
	epNs := ep.UnixNs()
	base := group.MakeTime(epNs, 0)
	st.sst = st.sst[:0]
	for _, m := range c.Members[:n] {
		st.sst = append(st.sst, solve.Station{Pos: m.Pos, TOA: m.T.Sub(base) * 1e-9,
			Sigma: math.Sqrt(math.Max(m.Var, 1)) * 1e-9, ID: int32(m.Station)})
	}
	pr := solve.Problem{EpochNs: epNs, Stations: st.sst}
	if st.ac != nil && st.ac.Hint(c.Code, epNs, geo.Vec3{}, &st.est) && st.est.SigmaH < 2000 {
		pr.Prior = solve.Prior{Valid: true, Pos: st.est.ECEF, Cov: st.est.Cov}
	}
	st.solves.Add(1)
	f := &st.fix
	if err := st.sv.Solve(&pr, f); err != nil || !f.OK() || f.Flags&solve.FlagPriorFused != 0 {
		return
	}
	st.solveOK.Add(1)
	txNs := epNs + int64(math.Round(f.TxS*1e9))
	var id uint32
	if st.ac != nil {
		tf := track.Fix{TNs: txNs, NHyp: 1, NStations: f.NStations, DOF: f.DOF,
			Hyp: [2]track.FixHyp{{ECEF: f.ECEF, Cov: f.CovECEF}}}
		if f.Flags&solve.FlagAmbiguous != 0 && f.NAlt > 0 {
			cov := f.Alt[0].CovECEF
			if cov[0] <= 0 || cov[4] <= 0 || cov[8] <= 0 {
				cov = f.CovECEF
			}
			tf.Hyp[1] = track.FixHyp{ECEF: f.Alt[0].ECEF, Cov: cov}
			tf.NHyp = 2
		}
		r := st.ac.Fix(c.Code, &tf)
		if r.Res == track.ResApplied || r.Res == track.ResCreated {
			id = r.ID
		}
	}
	if st.out != nil {
		st.out(ModeACFix{Code: c.Code, TUnixNs: txNs, ECEF: [3]float64{f.ECEF.X, f.ECEF.Y, f.ECEF.Z},
			SigmaHM: f.SigmaH, NStations: f.NStations, Grade: f.Grade, Target: id})
	}
}

// publishModeAC sweeps the tracker and stores the snapshot of shown
// targets (st.mu held).
func (e *Engine) publishModeAC(st *modeACState, nowNs int64) {
	out := []ModeACTarget{}
	st.ac.Sweep(nowNs, func(a *track.ACEstimate) {
		if !model.Displayable(a.Grade) {
			return // grade D: track maintenance only, like Mode S results
		}
		t := ModeACTarget{ID: fmt.Sprintf("#%06X", a.ICAO&0xFFFFFF), Key: a.ICAO, Lat: a.LLH.Lat, Lon: a.LLH.Lon,
			AltGeoM: a.LLH.H, SigmaHM: a.SigmaH, Grade: model.GradeString(a.Grade), NBucket: model.NBucketFor(a.LastStations),
			Fixes: a.Fixes, Updates: a.Updates, Crowded: a.Crowded, TUnixNs: a.TNs,
			AgeS: math.Max(0, float64(nowNs-a.LastUpdateNs)/1e9), SeenS: math.Max(0, float64(nowNs-a.FirstNs)/1e9)}
		if a.SquawkOK {
			t.Squawk = track.ACSquawkString(a.Squawk)
		}
		if a.AltOK {
			ft := a.AltFt
			t.AltFt = &ft
		}
		t.GS = a.Speed() / 0.514444
		t.Track = math.Mod(math.Atan2(a.VelENU[0], a.VelENU[1])*180/math.Pi+360, 360)
		t.VertRate = a.VelENU[2] / 0.3048 * 60
		out = append(out, t)
	})
	st.targets.Store(&out)
}

// ModeACTargets returns the shown Mode A/C targets of the last publication
// (once a second; nil while Mode A/C MLAT is off). The slice is shared:
// do not modify it.
func (e *Engine) ModeACTargets() []ModeACTarget {
	st := e.modeAC.Load()
	if st == nil {
		return nil
	}
	return *st.targets.Load()
}

// ModeACAircraft returns the shown Mode A/C targets as map aircraft
// records (see ModeACTarget.Aircraft), new slice.
func (e *Engine) ModeACAircraft() []model.Aircraft {
	ts := e.ModeACTargets()
	if ts == nil {
		return nil
	}
	out := make([]model.Aircraft, len(ts))
	for i := range ts {
		out[i] = ts[i].Aircraft()
	}
	return out
}

// ModeACStats returns the Mode A/C counters (zero with Enabled false while
// off).
func (e *Engine) ModeACStats() ModeACStats {
	st := e.modeAC.Load()
	if st == nil {
		return ModeACStats{}
	}
	s := ModeACStats{Enabled: true, Replies: st.replies.Load(), Unsynced: st.unsynced.Load(), Overflow: st.overflow.Load(),
		Clusters: st.clusters.Load(), Solves: st.solves.Load(), SolveOK: st.solveOK.Load(), Small: st.small.Load(),
		Stations: len(e.planner.ModeACStations()), Shown: len(*st.targets.Load())}
	st.mu.Lock()
	s.Track = st.ac.Stats()
	st.mu.Unlock()
	return s
}
