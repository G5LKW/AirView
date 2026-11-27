package mlat

import (
	"cmp"
	"context"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/group"
	"airview/internal/mlat/plan"
	"airview/internal/mlat/quality"
	"airview/internal/mlat/ref"
	"airview/internal/mlat/solve"
	"airview/internal/mlat/track"
	"airview/internal/model"
	"airview/internal/modes"
)

// job is one queued group (or a control message).
type job struct {
	g      ingest.Group
	copies []ingest.Copy
	data   [14]byte
	late   bool
	tick   int64         // > 0: run the 1 Hz tick at this time
	done   chan struct{} // drain barrier
}

// maxContrib is the number of contributing stations remembered per
// aircraft (the most recent ones).
const maxContrib = 32

type contribRec struct {
	st  model.StationIdx
	tNs int64
}

// acState is the engine's per-aircraft state (worker-local).
type acState struct {
	contrib  [maxContrib]contribRec
	nContrib int

	lastSeen     int64
	lastPubNs    int64 // last publication of a measurement update (event time: the group's FirstNs)
	lastCoastNs  int64 // last coasting publication (server time of the tick)
	lastScoreNs  int64
	lastVerifyNs int64

	pending bool
	lastFix model.MLATFix
	vel     [3]float32
	hasVel  bool

	lastFixPos   geo.Vec3
	lastFixNs    int64
	lastFixSigma float64
	holdout      bool
	relative     bool
	// verified: the current track has accepted at least one position
	// update with redundancy (DOF >= 1), so its branch was checked. Tracks
	// born from minimal (3 + altitude, DOF 0) fixes are not displayed or
	// used as priors / hints until then (see doc.go "Verification").
	verified bool
	redund   int // redundancy (sum of DOF) accepted by the current track
	contra   int // redundancy of consecutive fixes contradicting the track
	// gateFail is the EWMA (weight gateFailW) of gate failures among the
	// redundant clusters (DOF >= 1) offered to the confirmed track, gateN
	// their count (checkGated)
	gateFail float64
	gateN    int
	// minSets are the station sets of the DOF-1 fixes credited to the
	// current track (redundancy)
	minSets  [2]uint64
	nMinSets int
}

func (a *acState) addContrib(st model.StationIdx, tNs int64) {
	oldest := 0
	for i := 0; i < a.nContrib; i++ {
		if a.contrib[i].st == st {
			a.contrib[i].tNs = max(a.contrib[i].tNs, tNs)
			return
		}
		if a.contrib[i].tNs < a.contrib[oldest].tNs {
			oldest = i
		}
	}
	if a.nContrib < maxContrib {
		a.contrib[a.nContrib] = contribRec{st, tNs}
		a.nContrib++
		return
	}
	a.contrib[oldest] = contribRec{st, tNs}
}

// seqRing remembers which cluster sequence numbers were applied, so a late
// re-emission (group.Cluster.Late) is not applied a second time.
type seqRing struct {
	s [4096]uint64
}

func (r *seqRing) mark(seq uint64)         { r.s[seq%uint64(len(r.s))] = seq + 1 }
func (r *seqRing) applied(seq uint64) bool { return r.s[seq%uint64(len(r.s))] == seq+1 }

// worker owns one partition of the aircraft: its assembler, solver and
// tracker are used by this goroutine only.
type worker struct {
	e  *Engine
	id int

	jobs   chan *job
	free   chan *job
	nalloc atomic.Int32
	quit   chan struct{}

	asm   *group.Assembler
	sv    *solve.Solver
	svAtm geo.Atmosphere
	tr    *track.Tracker

	clk   clockAdapter
	sites sitesAdapter
	hints hintsAdapter

	alt    map[uint32]altRec
	ac     map[uint32]*acState
	verAt  map[uint32]int64
	domOff map[uint64]float64
	// firstSynced is the copy index of the first arrival syncedArrivals
	// collected.
	firstSynced int
	offN        map[uint64]int
	dom         uint64
	timed       map[model.StationIdx]int64
	seqs        seqRing
	msg         modes.Message
	now         int64

	rangeFn func(rx, ac geo.Vec3) float64
	sweepFn func(*track.Estimate)

	// scratch (no allocation on the per-cluster path)
	sm    [group.MaxMembers]syncMem
	smm   [group.MaxMembers]clock.Member
	qs    [group.MaxMembers]clock.Query
	tv    [group.MaxMembers]float64
	mcov  [group.MaxMembers * group.MaxMembers]float64
	okv   [group.MaxMembers]bool
	midx  [group.MaxMembers]int // member index -> query row (or -1)
	sel   [group.MaxMembers]int
	siteJ [group.MaxMembers]float64 // jitter part of each site's variance (ns²)
	sst   [solve.MaxStations]solve.Station
	scov  [solve.MaxStations * solve.MaxStations]float64
	toas  [group.MaxMembers]track.TOA
	arr   [group.MaxMembers]ref.Arrival
	arrSt [group.MaxMembers]model.StationIdx // station of each arr entry
	vtoa  [16]quality.TOA
	vsig  [16]float64
	resid [solve.MaxStations]quality.Residual
	stb   [solve.MaxStations]model.StationIdx
	ex1   [solve.MaxStations]float64
	exr   [solve.MaxStations]float64
	exs   [solve.MaxStations]float64
	// katmFix is the reusable k_atm sample (Stations backed by katmPos).
	katmFix quality.KAtmFix
	fix     solve.Fix
	est     track.Estimate
	est2    track.Estimate
	emit    []track.Estimate
	keys    []uint32 // tick's sorted aircraft list (reused)
	katmN   int
	// trStats is the tracker's counters, published at every tick (the
	// tracker itself is not safe for other goroutines).
	trStats atomic.Pointer[track.Stats]
	dbgFix  func(key uint32, epochNs int64, f *solve.Fix, holdout bool)
	// checking: solve runs for checkGated (no hold-out scoring: those
	// clusters are a biased sample)
	checking bool
	dbgCl    func(c *group.Cluster, solved, confirmed bool, res track.Result, est *track.Estimate)
}

// lateSlotsPerWorker sizes one worker's late-copy ring (group.Config
// LateSlots, about 1 kB per slot). Aircraft are spread over the workers, so
// each ring sees 1/workers of the clusters: the configured size is kept up
// to 4 workers and shared out above (at least 2048 slots each). Without it
// the rings cost 16 MB per CPU thread whatever the load (release soak, 32
// threads: 515 MB of a 0.9 GB live heap for 85 feeders).
func lateSlotsPerWorker(slots, workers int) int {
	if workers <= 4 {
		return slots
	}
	return min(slots, max(slots*4/workers, 2048))
}

func newWorker(e *Engine, id int) (*worker, error) {
	w := &worker{e: e, id: id, alt: map[uint32]altRec{}, ac: map[uint32]*acState{},
		verAt: map[uint32]int64{}, domOff: map[uint64]float64{}, offN: map[uint64]int{}, timed: map[model.StationIdx]int64{}}
	if !e.sync {
		w.jobs = make(chan *job, e.o.QueueLen)
		w.free = make(chan *job, e.o.QueueLen)
		w.quit = make(chan struct{})
	}
	atm := e.Atmosphere()
	so := solve.OptionsFromConfig(e.cfg.Solve)
	so.Atmosphere = atm
	w.sv = solve.NewSolver(so)
	w.svAtm = atm
	tp := track.ParamsFromConfig(e.cfg.Track)
	tp.Atmosphere = atm
	w.tr = track.New(tp)
	w.hints.w = w
	var err error
	gc := group.ConfigFromModel(e.cfg.Group)
	gc.LateSlots = lateSlotsPerWorker(gc.LateSlots, e.o.Workers)
	w.asm, err = group.New(group.Options{Config: gc, Clock: &w.clk, Sites: &w.sites,
		Hints: &w.hints, Selector: e.planner, Sink: group.SinkFunc(w.onCluster)})
	if err != nil {
		return nil, err
	}
	w.rangeFn = func(rx, ac geo.Vec3) float64 { a := w.e.atm.Load(); return a.Range(rx, ac) }
	w.sweepFn = func(est *track.Estimate) { w.emit = append(w.emit, *est) }
	return w, nil
}

// --- queue ---

func (w *worker) submit(g *ingest.Group, late bool) bool {
	var j *job
	select {
	case j = <-w.free:
	default:
		if int(w.nalloc.Load()) >= cap(w.jobs) {
			return false
		}
		w.nalloc.Add(1)
		j = &job{}
	}
	j.g = *g
	j.copies = append(j.copies[:0], g.Copies...)
	n := copy(j.data[:], g.Data)
	j.g.Data = j.data[:n]
	j.g.Copies = j.copies
	j.late, j.tick, j.done = late, 0, nil
	select {
	case w.jobs <- j:
		return true
	default:
		w.release(j)
		return false
	}
}

func (w *worker) release(j *job) {
	select {
	case w.free <- j:
	default:
		w.nalloc.Add(-1)
	}
}

// postTick queues the 1 Hz tick (blocking briefly at most: ticks must not
// be lost, but a full queue means the worker is busy and will get to it).
func (w *worker) postTick(nowNs int64) {
	j := &job{tick: nowNs}
	select {
	case w.jobs <- j:
	case <-w.quit:
	case <-time.After(500 * time.Millisecond):
	}
}

func (w *worker) drain(ctx context.Context) error {
	j := &job{done: make(chan struct{})}
	select {
	case w.jobs <- j:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-j.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *worker) loop(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-w.quit:
			return
		case j := <-w.jobs:
			switch {
			case j.done != nil:
				close(j.done)
			case j.tick > 0:
				w.tick(j.tick)
			default:
				w.process(&j.g, j.late)
				w.release(j)
			}
		}
	}
}

// --- per group ---

// process handles one ingest group: reference tracks, clock sync, planner
// bookkeeping, then the cluster assembler (whose sink, onCluster, solves
// and tracks).
func (w *worker) process(g *ingest.Group, late bool) {
	t0 := time.Now()
	e := w.e
	w.now = g.FirstNs
	w.clk.snap = e.filter.Snapshot()
	w.sites.tab = e.st.load()
	if late {
		w.asm.ObserveLate(g)
		e.ctr.procNs.Add(int64(time.Since(t0)))
		return
	}
	if g.Redelivery {
		// A sync candidate flushed early with one synced copy, delivered
		// again with every copy (Options.IngestGate): the frame was already
		// counted and seen by the reference store; only the clock filter
		// and the assembler need the full set of copies.
		if w.now >= e.syncFrom.Load() && w.sync(g) {
			e.ctr.syncAdmitted.Add(1)
		}
		w.asm.Observe(g)
		e.ctr.procNs.Add(int64(time.Since(t0)))
		return
	}
	w.countTimed(g)
	holdout := e.hold.In(g.Key, w.now)
	if len(g.Data) > 0 && modes.Decode(g.Data, &w.msg) == nil {
		w.observeMessage(g, holdout)
	} else {
		w.msg = modes.Message{}
	}
	e.planner.ObserveGroup(g)
	if w.now >= e.syncFrom.Load() && w.sync(g) {
		e.ctr.syncAdmitted.Add(1)
	}
	w.asm.Observe(g)
	e.ctr.procNs.Add(int64(time.Since(t0)))
}

// syncWindowNs is the width of the emission-time window that selects the
// members of one transmission in a short-frame sync group.
const syncWindowNs = 20e3

// sync hands a group to the clock filter as a sync observation (DESIGN
// §9.2.2). Long frames (DF17/18) carry unique payloads and go as they are
// (clock.Filter.ObserveGroup). Short frames (DF0/4/5/11) repeat with
// identical payloads: replies to interrogators milliseconds apart (ACAS /
// radar pairs, 0.74 ms) land in one ingest group, and a sync observation
// that mixes two transmissions corrupts the clocks (simnet, 20 SDR: from
// 0 to 100-300 false steps per minute, see doc.go). For them the engine
// keeps only synced stations whose emission-time estimates
// e_i = t_net,i − ρ_i(p_ref)/c fall into the densest 20 µs window (one
// transmission: e_i agree to clock + jitter + reference error, < 2 µs;
// two transmissions: hundreds of µs apart), and passes that set to
// AddSync. Unsynced stations converge on long frames.
//
// Short frames are used only in components with GPS anchors. In a
// relative-time (anchor-less) component the common mode of the stations'
// β_short is not observable, and on simnet SDR-only networks short-frame
// sync drove half of the seeds into a cascade of false steps and
// blacklists (seeds 16 and 19: 85-280 steps per minute; with long frames
// only: 0-5 in 16 minutes, P50 25-33 m). The cost is that β_short is not
// estimated there, so short-frame fixes of SDR-only networks keep the
// receivers' short/long reference error (filed in HOOKS.md for mlat/clock).
func (w *worker) sync(g *ingest.Group) bool {
	e := w.e
	if g.Kind == model.FrameModeSLong {
		return e.filter.ObserveGroup(g, e.href)
	}
	if g.Kind != model.FrameModeSShort || !g.HasResult || !clock.SyncDF(g.DF) {
		return false
	}
	pos, cov, ok := e.href.RefPosition(g.Key, g.DF, g.FirstNs)
	if !ok {
		return false
	}
	snap := w.clk.snap
	n := 0
	for i := range g.Copies {
		c := &g.Copies[i]
		if !c.Timed || c.MapOnly || c.SameStation || n == len(w.sm) {
			continue
		}
		if sc := snap.Station(c.Station); sc == nil || sc.RelativeTime {
			continue // see below: short frames only in anchored components
		}
		t, _, ok := snap.NetTime(clock.Query{Station: c.Station, Epoch: c.Epoch, Ticks: c.Ticks, Kind: g.Kind})
		if !ok {
			continue
		}
		prop, ok := snap.PropagationNs(c.Station, pos)
		if !ok {
			continue
		}
		w.sm[n] = syncMem{m: clock.Member{Station: c.Station, Epoch: c.Epoch, Ticks: c.Ticks}, e: t*1e9 - prop}
		n++
	}
	if n < 2 {
		return false
	}
	ms := w.sm[:n]
	slices.SortFunc(ms, func(a, b syncMem) int { return cmp.Compare(a.e, b.e) })
	best, bi := 0, 0
	for i, j := 0, 0; i < n; i++ {
		for j < n && ms[j].e-ms[i].e <= syncWindowNs {
			j++
		}
		if j-i > best {
			best, bi = j-i, i
		}
	}
	if best < 2 {
		return false
	}
	for k := 0; k < best; k++ {
		w.smm[k] = ms[bi+k].m
	}
	o := clock.SyncObs{Key: g.Key, Kind: g.Kind, HostNs: g.FirstNs, Pos: pos, PosCov: cov, Members: w.smm[:best]}
	return e.filter.AddSync(&o)
}

type syncMem struct {
	m clock.Member
	e float64 // emission-time estimate (ns since the clock origin)
}

// countTimed counts the timed copies of synced stations (minimum
// contribution rule, DESIGN §8.4); flushed to the engine once a second.
func (w *worker) countTimed(g *ingest.Group) {
	for i := range g.Copies {
		c := &g.Copies[i]
		if !c.Timed || c.SameStation || c.MapOnly {
			continue
		}
		if sc := w.clk.snap.Station(c.Station); sc != nil && sc.Synced {
			w.timed[c.Station]++
		}
	}
}

// observeMessage feeds the decoded frame to the reference store, the
// hold-out selector, the altitude store, ADS-B verification and the
// tracker's velocity input.
func (w *worker) observeMessage(g *ingest.Group, holdout bool) {
	e := w.e
	m := &w.msg
	key := g.Key
	if m.Valid&modes.FAltBaro != 0 && m.Valid&modes.FAltMetric == 0 {
		w.alt[key] = altRec{ft: float64(m.AltBaro), tNs: w.now, gillham: m.AltBaroGillham}
	}
	if m.Valid&modes.FNACp != 0 {
		e.hold.NoteNACp(key, m.NACp, w.now)
	}
	adsb := (m.DF == 17 || (m.DF == 18 && m.CF < 2)) && m.CRC == modes.CRCOK
	if adsb {
		tau, sig := w.transmitInstant(g)
		r := e.refs.Observe(m, tau, sig)
		if r == ref.Accepted || r == ref.Started {
			e.ctr.refObs.Add(1)
			if m.Valid&modes.FCPR != 0 && !m.CPRSurface {
				e.planner.NoteADSB(key, w.now, e.refs.Usable(key))
				if !holdout {
					w.verifyADSB(g, key, tau)
				}
			}
		}
		if m.TC == 19 && !holdout && m.Valid&(modes.FGroundSpeed|modes.FTrack) == modes.FGroundSpeed|modes.FTrack {
			sig := 5.0
			if m.Valid&modes.FNACv != 0 {
				sig = [...]float64{10, 10, 3, 1, 0.3, 0.3, 0.3, 0.3}[m.NACv&7]
			}
			w.velocity(key, tau, m.GS, m.Track, sig, m.Valid&modes.FVertRate != 0, float64(m.VertRate))
		}
		return
	}
	if m.DF == 0 || m.DF == 4 || m.DF == 16 || m.DF == 20 {
		// Mode S altitude replies: the reference store uses them only for
		// aircraft that already have a reference track (vertical channel,
		// TC 20-22 GNSS−baro pairing; ref doc.go "Mode S altitude replies").
		if m.Valid&modes.FAltBaro != 0 && e.refs.Has(key) {
			tau, sig := w.transmitInstant(g)
			e.refs.Observe(m, tau, sig) // counted in ref.Stats
		}
	}
	if (m.DF == 20 || m.DF == 21) && m.NumBDS > 0 && m.BDS[0].BDS == modes.BDS50 && m.BDS[0].Confidence >= 0.8 {
		v := modes.DecodeBDS50(m.MB())
		if v.GSOK && v.TrackOK {
			w.velocity(key, g.FirstNs, v.GS, v.Track, 8, false, 0)
		}
	}
}

// velocity feeds a ground velocity (kt, degrees true) to an existing track.
func (w *worker) velocity(key uint32, tNs int64, gsKt, trkDeg, sigma float64, vrOK bool, vrFpm float64) {
	const kt = 1852.0 / 3600
	s, c := math.Sincos(trkDeg * math.Pi / 180)
	v := track.Velocity{ICAO: key, TNs: tNs, East: gsKt * kt * s, North: gsKt * kt * c, SigmaH: sigma}
	if vrOK {
		v.Up, v.SigmaUp = vrFpm*0.3048/60, 1
	}
	if w.tr.Velocity(&v, nil) == track.ResApplied {
		w.e.ctr.velocities.Add(1)
	}
}

// syncedArrivals collects the network times of the group's copies at
// synced, located stations of one time domain (relative to baseNs).
// anchoredOnly skips relative-time domains (their network time is not
// UTC). It returns the count.
func (w *worker) syncedArrivals(g *ingest.Group, baseNs int64, anchoredOnly bool, max int) int {
	snap := w.clk.snap
	n := 0
	var dom uint64
	for i := range g.Copies {
		c := &g.Copies[i]
		if n == max || n == len(w.arr) {
			break
		}
		if !c.Timed || c.SameStation || c.MapOnly {
			continue
		}
		sc := snap.Station(c.Station)
		if sc == nil || !sc.Synced || (anchoredOnly && sc.RelativeTime) {
			continue
		}
		d, _ := snap.Domain(c.Station)
		if n > 0 && d != dom {
			continue
		}
		pos, _, ok := w.sites.Site(c.Station)
		if !ok {
			continue
		}
		ns, frac, cv, jv, ok := snap.NetTimeUnix(clock.Query{Station: c.Station, Epoch: c.Epoch, Ticks: c.Ticks, Kind: g.Kind})
		if !ok {
			continue
		}
		if n == 0 {
			w.firstSynced = i
		}
		dom = d
		w.arrSt[n] = c.Station
		w.arr[n] = ref.Arrival{Station: pos, DtNs: float64(ns-baseNs) + frac, SigmaNs: math.Sqrt(cv + jv)}
		n++
	}
	return n
}

// transmitInstant estimates a message's transmit instant (UTC network
// time): from the synced copies through the reference track when possible
// (ref.Store.TransmitInstant), else the first corrected arrival minus 1 ms
// of propagation with σ 5 ms.
func (w *worker) transmitInstant(g *ingest.Group) (int64, float64) {
	base := g.FirstNs
	if n := w.syncedArrivals(g, base, true, len(w.arr)); n > 0 {
		atm := w.e.Atmosphere()
		var st ref.State
		if dt, s, ok := w.e.refs.TransmitInstant(g.Key, base, w.arr[:n], &atm, &st); ok {
			return base + int64(math.Round(dt)), s
		}
	}
	return base - 1e6, 5e6
}

// verifyADSB checks an ADS-B position against the TDOAs of synced stations
// (DESIGN §9.6), at most every VerifyEvery per aircraft.
func (w *worker) verifyADSB(g *ingest.Group, key uint32, tau int64) {
	e := w.e
	if e.o.VerifyEvery < 0 || w.now-w.verAt[key] < int64(e.o.VerifyEvery) {
		return
	}
	// at least 3 stations: with 2 a single clock error contradicts
	n := w.syncedArrivals(g, g.FirstNs, false, len(w.vtoa))
	if n < 3 {
		return
	}
	var st ref.State
	if !e.refs.At(key, tau, 0, &st) || st.Flags&ref.FlagConfirmed == 0 || st.Flags&(ref.FlagStale|ref.FlagBackward) != 0 {
		return
	}
	w.verAt[key] = w.now
	// Declared positions the survey flagged suspect never take part; the
	// other declared-only stations are remembered for the attribution below.
	tab := w.sites.tab
	m, nDecl := 0, 0
	var declIdx [blameMaxTries]int
	for i := 0; i < n; i++ {
		ef, _ := tab.get(w.arrSt[i])
		if ef.suspect {
			continue
		}
		if ef.src == PosDeclared && nDecl < len(declIdx) {
			declIdx[nDecl] = m
			nDecl++
		}
		w.vtoa[m] = quality.TOA{Pos: w.arr[i].Station, T: w.arr[i].DtNs * 1e-9}
		w.vsig[m] = w.arr[i].SigmaNs * 1e-9
		w.arrSt[m] = w.arrSt[i]
		m++
	}
	n = m
	if n < 3 {
		return
	}
	// Phase-4 verification (quality.Verify): χ² with the full covariance, a
	// minimal-detectable-offset gate before confirming, and leave-one-out
	// attribution: a single bad row (Culprit with VerdictNone) is a station
	// problem, not the aircraft's.
	res := quality.Verify(st.Pos, st.Cov, w.vtoa[:n], nil, w.vsig[:n], w.rangeFn, quality.DefaultVerifyParams())
	v := res.Verdict
	if v == quality.VerdictNone && res.Culprit >= 0 && res.Culprit < n {
		e.ctr.verifyCulprit.Add(1)
		if slices.Contains(declIdx[:nDecl], res.Culprit) {
			e.blameStation(w.arrSt[res.Culprit], w.now)
		}
	}
	if v == quality.VerdictContradicted && n >= 4 && nDecl > 0 {
		// Charge the station only when it is the one culprit: dropping it
		// (and no other row) resolves the contradiction. When several
		// single removals do, nothing points at one station (clocks still
		// settling, a bad ADS-B position) and the aircraft is charged.
		culprit, resolved := -1, 0
		for i := 0; i < n && resolved < 2; i++ {
			if w.verifyWithout(&st, n, i) {
				culprit = i
				resolved++
			}
		}
		if resolved == 1 && slices.Contains(declIdx[:nDecl], culprit) {
			e.blameStation(w.arrSt[culprit], w.now)
			v = quality.VerdictNone // charged to the station, not the aircraft
		}
	}
	switch v {
	case quality.VerdictConfirmed:
		e.ctr.verified.Add(1)
	case quality.VerdictContradicted:
		e.ctr.contradicted.Add(1)
	}
	if !e.vbrk.note(v) {
		// the network's TDOAs contradict most ADS-B positions: a clock
		// problem, not spoofing (finding SCALE-05)
		e.ctr.verifyMuted.Add(1)
		v = quality.VerdictNone
	}
	if e.verify.Record(key, w.now, v) {
		e.ctr.suspect.Add(1)
		until := w.now + int64(10*time.Minute)
		e.planner.MarkSuspect(key, until)
		// Stop offering it as a clock reference only on evidence from UTC
		// (anchored) time: in a relative-time component a contradiction can
		// come from the clocks themselves, and suspending references there
		// would starve the clock filter of the very observations it needs
		// to converge. The clock filter's own per-aircraft blacklist
		// (§9.2.4) covers bad references in every component.
		if sc := w.clk.snap.Station(g.Copies[w.firstSynced].Station); sc != nil && !sc.RelativeTime {
			e.refs.Suspend(key, until)
		}
		e.o.Logger.Debug("mlat: ADS-B position contradicted by TDOA", "icao", key)
	}
}

// verifyWithout re-runs the TDOA check of the first n verification rows
// without row skip and reports whether the contradiction goes away.
func (w *worker) verifyWithout(st *ref.State, n, skip int) bool {
	var toa [16]quality.TOA
	var sig [16]float64
	m := 0
	for i := 0; i < n; i++ {
		if i != skip {
			toa[m], sig[m] = w.vtoa[i], w.vsig[i]
			m++
		}
	}
	r := quality.Verify(st.Pos, st.Cov, toa[:m], nil, sig[:m], w.rangeFn, quality.DefaultVerifyParams())
	return r.Verdict == quality.VerdictConfirmed || r.Verdict == quality.VerdictConsistent
}

func (w *worker) state(key uint32) *acState {
	a := w.ac[key]
	if a == nil {
		a = &acState{}
		w.ac[key] = a
	}
	return a
}

// --- per cluster ---

// onCluster is the assembler's sink: solve (when admitted), update the
// tracker, publish.
func (w *worker) onCluster(c *group.Cluster) {
	e := w.e
	e.ctr.clusters.Add(1)
	if c.Late && w.seqs.applied(c.Seq) {
		e.ctr.lateSkipped.Add(1)
		return
	}
	if len(c.Sites) < 2 {
		return
	}
	key := c.Key
	holdout := e.hold.In(key, w.now)
	a := w.state(key)
	a.lastSeen = w.now
	a.holdout = holdout

	sc0 := w.clk.snap.Station(c.Members[0].Station)
	relative := sc0 == nil || sc0.RelativeTime
	epochNs := c.Sites[c.Ref].T.UnixNs()
	epochT := group.MakeTime(epochNs, 0)
	// server time − network time of this domain: shifts relative-time
	// domains to (approximately) UTC and removes the ingest's corrected-time
	// bias from the solver's host-time check
	off := w.domainOffset(c)
	var shift int64
	if relative {
		shift = off
	}
	a.relative = relative
	k := w.selectSites(c)
	if !w.siteCov(c, k) {
		return
	}
	confirmed := w.tr.Get(key, &w.est) && w.est.Status == track.StatusConfirmed
	alt := w.altitude(c, key, holdout, epochNs+shift, confirmed)

	var res track.Result
	var tf track.Fix
	solved := false
	if c.Use >= group.UseSolveAlt {
		solved = w.solve(c, k, epochT, shift, off, alt, holdout, relative, a, &tf)
	}
	// A redundant, unambiguous fix is an independent check of the track's
	// branch: when fixes with DOF >= 2 contradict a confirmed track with
	// verifyDOF of redundancy in a row (one such fix is enough), the track
	// locked onto a wrong 3+alt branch (or a stale state); it is dropped and
	// restarted from the fix. A contradicting DOF-1 fix restarts nothing:
	// far outside the stations it is itself often the wrong (near) branch of
	// a 4-station or 4+alt problem, and two of them, minutes apart, used to
	// replace a right track by a wrong one (H-track, live-like netsim: a track
	// 0.1 km from its aircraft restarted 133 km off).
	agree := false
	if solved && confirmed && tf.NHyp == 1 && w.fix.DOF >= 1 {
		if w.fixAgrees(&tf) {
			agree = true
			a.contra = 0
		} else if w.fix.DOF >= 2 {
			a.contra += w.fix.DOF
			if a.contra >= verifyDOF {
				w.tr.Delete(key)
				e.ctr.restarts.Add(1)
				a.contra, a.verified, a.redund, a.gateFail, a.gateN, a.nMinSets = 0, false, 0, 0, 0, 0
				confirmed = false
			}
		}
	}
	switch {
	case confirmed:
		w.buildCluster(c, k, epochT, shift, alt)
		res = w.tr.Cluster(&track.Cluster{ICAO: key, T0Ns: epochNs + shift, TOAs: w.toas[:min(k, len(w.toas))],
			AltHAE: alt.HAE, AltSigma: altSigma(alt)}, &w.est)
		e.ctr.clusterTrack.Add(1)
	case solved:
		res = w.tr.Fix(&tf, &w.est)
		e.ctr.fixToTrack.Add(1)
	default:
		return
	}
	if w.dbgCl != nil {
		w.dbgCl(c, solved, confirmed, res, &w.est)
	}
	switch res {
	case track.ResApplied, track.ResCreated:
		e.ctr.applied.Add(1)
		if res == track.ResCreated {
			a.verified, a.redund, a.gateFail, a.gateN, a.nMinSets = false, 0, 0, 0, 0
		}
		if solved && (agree || !confirmed) && w.fix.Flags&solve.FlagAmbiguous == 0 {
			a.redund += w.redundancy(c, k, a)
		}
		if a.redund >= verifyDOF {
			a.verified = true
		}
	case track.ResGated:
		e.ctr.gated.Add(1)
		if confirmed {
			w.checkGated(c, k, epochT, shift, off, alt, holdout, relative, a, solved, &tf)
		}
		return
	default:
		return
	}
	if confirmed && (k >= 5 || (k >= 4 && alt.Valid)) {
		a.gateFail *= 1 - gateFailW // a redundant cluster fitted the track
		a.gateN++
	}
	if c.Late {
		e.ctr.lateAfterAccept.Add(1)
	}
	w.seqs.mark(c.Seq)
	for i := 0; i < k; i++ {
		s := &c.Sites[w.sel[i]]
		for j := int(s.First); j < int(s.First)+int(s.Count); j++ {
			a.addContrib(c.Members[j].Station, w.now)
		}
	}
	if w.est.Status == track.StatusConfirmed && a.verified {
		e.pos.put(key, w.est.ECEF, w.est.SigmaH, w.now)
		w.publish(key, a, &w.est, false)
	}
}

// A confirmed track is restarted when the EWMA (weight gateFailW) of gate
// failures among its redundant clusters exceeds gateFailMax after at least
// gateFailMinN of them (checkGated). A right track fails the 99.9 % gate on
// one cluster in a thousand plus its outliers (a few per cent); a wrong
// branch or a run-away track on most of them.
const (
	gateFailW    = 0.15
	gateFailMax  = 0.5
	gateFailMinN = 6
)

// checkGated handles a redundant cluster (DOF >= 1) that a confirmed track
// gated away. Such a cluster is evidence that the track itself is wrong (a
// 3+alt branch confirmed from minimal fixes, or a track that ran away on
// 2-station maintenance): seen on the live-like netsim reproduction, a
// track 100-170 km off stayed confirmed for minutes while every
// 4-6-station cluster of its aircraft failed its gate, because the planner
// had not admitted those clusters for solving and so the fix contradiction
// test (fixAgrees) never ran. The cluster is solved here (outside the
// planner's budget: gate failures are a few per cent of the clusters); an
// unambiguous, redundant fix that contradicts the track counts towards the
// restart rule of onCluster, and a high rate of gated redundant clusters
// (EWMA above gateFailMax) restarts the track on its own.
// A restart starts the new track from the fix when there is one.
func (w *worker) checkGated(c *group.Cluster, k int, epochT group.Time, shift, off int64, alt solve.Altitude,
	holdout, relative bool, a *acState, solved bool, tf *track.Fix) {
	if !(k >= 5 || (k >= 4 && alt.Valid)) {
		return
	}
	e := w.e
	a.gateFail = a.gateFail*(1-gateFailW) + gateFailW
	a.gateN++
	if !solved {
		w.checking = true
		solved = w.solve(c, k, epochT, shift, off, alt, holdout, relative, a, tf)
		w.checking = false
		e.ctr.gateSolves.Add(1)
	}
	if solved && tf.NHyp == 1 && w.fix.DOF >= 1 {
		if w.fixAgrees(tf) {
			a.contra = 0
		} else if w.fix.DOF >= 2 { // see onCluster: DOF-1 fixes restart nothing
			a.contra += w.fix.DOF
		}
	}
	if a.contra < verifyDOF && (a.gateFail < gateFailMax || a.gateN < gateFailMinN) {
		return
	}
	w.tr.Delete(c.Key)
	e.pos.del(c.Key)
	e.ctr.restarts.Add(1)
	a.contra, a.verified, a.redund, a.gateFail, a.gateN, a.nMinSets = 0, false, 0, 0, 0, 0
	if solved && tf.NHyp == 1 && w.fix.DOF >= 2 && w.tr.Fix(tf, &w.est) == track.ResCreated && w.fix.Flags&solve.FlagAmbiguous == 0 {
		a.redund = w.redundancy(c, k, a)
		a.verified = a.redund >= verifyDOF
	}
}

// redundancy returns the verification credit of an accepted, unambiguous
// fix (doc.go "Verification"): its DOF when DOF >= 2. A DOF-1 fix counts
// only when its station set differs from the DOF-1 fixes already credited
// to the track: the near/far branch ambiguity of a minimal problem is a
// property of its station geometry, so the same four stations repeating
// the same (possibly wrong) branch verify nothing. Seen on the live-like
// netsim reproduction: a 4-station DOF-1 fix 133 km from its aircraft
// started a track that two more fixes of the same stations "verified".
func (w *worker) redundancy(c *group.Cluster, k int, a *acState) int {
	d := w.fix.DOF
	if d != 1 {
		return max(d, 0)
	}
	var hs uint64 = 1469598103934665603
	for i := 0; i < k && i < solve.MaxStations; i++ {
		if w.fix.Used&(1<<i) == 0 {
			continue
		}
		s := &c.Sites[w.sel[i]]
		hs ^= uint64(c.Members[s.First].Station) + 0x9e3779b97f4a7c15 + hs<<6 + hs>>2
	}
	for _, x := range a.minSets[:a.nMinSets] {
		if x == hs {
			return 0
		}
	}
	if a.nMinSets < len(a.minSets) {
		a.minSets[a.nMinSets] = hs
		a.nMinSets++
	}
	return 1
}

// verifyDOF is the accumulated redundancy a track needs before it is
// displayed: two degrees of freedom, i.e. one update with DOF >= 2 or two
// with DOF 1 (doc.go "Verification").
const verifyDOF = 2

// fixAgrees tests a snapshot fix against the track predicted to the fix
// time: Mahalanobis² of the difference under the sum of both covariances
// against the χ²(3) 99.99 % quantile (21.1), with a 1 km floor for the
// distance so that a slightly optimistic covariance does not restart good
// tracks.
func (w *worker) fixAgrees(tf *track.Fix) bool {
	if !w.tr.Predict(tf.ICAO, tf.TNs, &w.est2) {
		return true
	}
	d := tf.Hyp[0].ECEF.Sub(w.est2.ECEF)
	if d.Norm() < 1000 {
		return true
	}
	var S [9]float64
	for i := range S {
		S[i] = tf.Hyp[0].Cov[i] + w.est2.Cov[i]
	}
	m2, ok := mahal3(&S, d)
	return !ok || m2 <= 21.1
}

// mahal3 returns dᵀS⁻¹d for a symmetric 3×3 S.
func mahal3(S *[9]float64, d geo.Vec3) (float64, bool) {
	a, b, c := S[0], S[1], S[2]
	e, f, i := S[4], S[5], S[8]
	A := e*i - f*f
	B := -(b*i - c*f)
	C := b*f - c*e
	det := a*A + b*B + c*C
	if !(det > 0) {
		return 0, false
	}
	E := a*i - c*c
	F := -(a*f - b*c)
	I := a*e - b*b
	x, y, z := d.X, d.Y, d.Z
	q := A*x*x + E*y*y + I*z*z + 2*(B*x*y+C*x*z+F*y*z)
	return q / det, true
}

func altSigma(a solve.Altitude) float64 {
	if !a.Valid {
		return 0
	}
	return a.Sigma
}

// domainOffset estimates (server time − network time) of a relative-time
// domain from the first arrivals: an EWMA of FirstNs minus the earliest
// site time. Tracks, published times and reference lookups of relative
// domains are shifted by it so that they live in (approximately) UTC.
func (w *worker) domainOffset(c *group.Cluster) int64 {
	first := c.Sites[0].T
	for i := 1; i < len(c.Sites); i++ {
		if c.Sites[i].T.Before(first) {
			first = c.Sites[i].T
		}
	}
	d, _ := w.clk.snap.Domain(c.Members[0].Station)
	w.dom = d
	smp := float64(c.FirstNs-first.UnixNs()) - first.Frac
	o, ok := w.domOff[d]
	if !ok || math.Abs(smp-o) > 1e9 {
		o, w.offN[d] = smp, 0
	} else {
		o += 0.02 * (smp - o)
	}
	w.offN[d]++
	w.domOff[d] = o
	return int64(math.Round(o))
}

// selectSites picks at most MaxSolveSites sites (lowest variance first)
// into w.sel and returns the count.
func (w *worker) selectSites(c *group.Cluster) int {
	n := len(c.Sites)
	for i := 0; i < n; i++ {
		w.sel[i] = i
	}
	lim := w.e.o.MaxSolveSites
	if n <= lim {
		return n
	}
	// insertion sort by variance (n <= 64)
	for i := 1; i < n; i++ {
		x := w.sel[i]
		j := i
		for j > 0 && c.Sites[w.sel[j-1]].Var > c.Sites[x].Var {
			w.sel[j] = w.sel[j-1]
			j--
		}
		w.sel[j] = x
	}
	return lim
}

// siteCov fills w.scov (k×k, s²) with the covariance of the selected
// sites' times and w.siteJ with the per-frame (jitter) part of each site's
// variance. Off the diagonal it is the clock-state covariance of the
// stations, combined with the sites' member weights:
//
//	S_ab = Σ_{i∈a} Σ_{j∈b} w_i w_j C_ij
//
// (C from clock.Snapshot.Convert; per-frame jitter is independent between
// stations). On the diagonal it is the site's own variance (members'
// clock, jitter, cross-covariance and the fusion geometry bound).
func (w *worker) siteCov(c *group.Cluster, k int) bool {
	m := 0
	for i := range c.Members {
		w.midx[i] = -1
	}
	for a := 0; a < k; a++ {
		s := &c.Sites[w.sel[a]]
		for j := int(s.First); j < int(s.First)+int(s.Count); j++ {
			mb := &c.Members[j]
			w.qs[m] = clock.Query{Station: mb.Station, Epoch: mb.Epoch, Ticks: mb.Ticks, Kind: c.Kind}
			w.midx[j] = m
			m++
		}
	}
	w.clk.snap.Convert(w.qs[:m], w.tv[:m], w.mcov[:m*m], w.okv[:m])
	for a := 0; a < k; a++ {
		sa := &c.Sites[w.sel[a]]
		var jv float64
		for i := int(sa.First); i < int(sa.First)+int(sa.Count); i++ {
			mb := &c.Members[i]
			jv += mb.W * mb.W * mb.JitterVar
		}
		w.siteJ[a] = math.Max(jv, 1)
		va := math.Max(sa.Var, 1)
		w.scov[a*k+a] = va * 1e-18
		for b := 0; b < a; b++ {
			sb := &c.Sites[w.sel[b]]
			var v float64
			for i := int(sa.First); i < int(sa.First)+int(sa.Count); i++ {
				ri := w.midx[i]
				if ri < 0 || !w.okv[ri] {
					continue
				}
				for j := int(sb.First); j < int(sb.First)+int(sb.Count); j++ {
					rj := w.midx[j]
					if rj < 0 || !w.okv[rj] {
						continue
					}
					v += c.Members[i].W * c.Members[j].W * w.mcov[ri*m+rj]
				}
			}
			// keep the matrix positive definite: |ρ| <= 0.98
			lim := 0.98 * math.Sqrt(va*math.Max(sb.Var, 1))
			v = math.Max(-lim, math.Min(lim, v))
			w.scov[a*k+b], w.scov[b*k+a] = v*1e-18, v*1e-18
		}
	}
	return true
}

// approxLLH returns an approximate aircraft position for the altitude
// conversion: the track prediction, else the centroid of the sites.
func (w *worker) approxLLH(c *group.Cluster, confirmed bool) geo.LLH {
	if confirmed {
		return w.est.LLH
	}
	var s geo.Vec3
	for i := range c.Sites {
		s = s.Add(c.Sites[i].Pos)
	}
	return geo.FromECEF(s.Scale(1 / float64(len(c.Sites))))
}

// altitude converts the cluster's altitude to a solver row (DESIGN §9.4):
// Mode S pressure altitude through the GNSS−baro field (the aircraft's own
// GNSS−baro offset when it is an ADS-B aircraft outside the hold-out set),
// quantisation (25 or 100 ft) and age × vertical rate inflation for an
// altitude taken from an earlier frame. GNSS heights in the frame are used
// directly (not for hold-out aircraft, which are solved as Mode S-only).
func (w *worker) altitude(c *group.Cluster, key uint32, holdout bool, tNs int64, confirmed bool) solve.Altitude {
	const ft = 0.3048
	switch c.AltSrc {
	case group.AltFrameGNSS:
		if holdout {
			return solve.Altitude{}
		}
		return solve.AltitudeFromHAE(c.AltFt*ft, 15, solve.AltitudeInput{QuantFt: 25})
	case group.AltFrameBaro, group.AltHint:
	default:
		return solve.Altitude{}
	}
	in := solve.AltitudeInput{BaroFt: c.AltFt, QuantFt: 25}
	if r, ok := w.alt[key]; ok {
		if r.gillham {
			in.QuantFt = 100
		}
		if c.AltSrc == group.AltHint {
			in.AgeS = math.Max(float64(w.now-r.tNs)/1e9, 0)
			in.VRateMps = 10
			if confirmed {
				in.VRateMps = math.Abs(w.est.VelENU[2]) + 1
			}
		}
	}
	p := w.approxLLH(c, confirmed)
	var hae, sig float64
	if holdout {
		hae, sig, _ = w.e.refs.Field().BaroToHAE(p.Lat, p.Lon, c.AltFt, tNs)
	} else {
		hae, sig, _ = w.e.refs.HAEFromBaro(key, p.Lat, p.Lon, c.AltFt, tNs)
	}
	return solve.AltitudeFromHAE(hae, sig, in)
}

// buildCluster fills w.toas for a tracker update: per site, the per-frame
// jitter as TOA.Sigma and the clock part as TOA.SyncSigma (the tracker
// models the latter as correlated, track WIRING).
func (w *worker) buildCluster(c *group.Cluster, k int, epochT group.Time, shift int64, _ solve.Altitude) {
	for a := 0; a < k && a < len(w.toas); a++ {
		s := &c.Sites[w.sel[a]]
		jv := w.siteJ[a]
		cv := math.Max(s.Var-jv, 0)
		w.toas[a] = track.TOA{Station: uint32(c.Members[s.First].Station), Pos: s.Pos,
			T: s.T.Sub(epochT) * 1e-9, Sigma: math.Sqrt(jv) * 1e-9, SyncSigma: math.Sqrt(cv) * 1e-9}
	}
}

// solve runs the solver on the selected sites; on success it fills tf for
// the tracker and records quality data.
func (w *worker) solve(c *group.Cluster, k int, epochT group.Time, shift, off int64, alt solve.Altitude,
	holdout, relative bool, a *acState, tf *track.Fix) bool {
	e := w.e
	e.ctr.solves.Add(1)
	if k > solve.MaxStations {
		k = solve.MaxStations
	}
	epochNs := epochT.UnixNs()
	for i := 0; i < k; i++ {
		s := &c.Sites[w.sel[i]]
		w.sst[i] = solve.Station{Pos: s.Pos, TOA: s.T.Sub(epochT) * 1e-9, Sigma: math.Sqrt(math.Max(s.Var, 1)) * 1e-9, ID: int32(w.sel[i])}
	}
	if at := e.Atmosphere(); at != w.svAtm {
		w.sv.SetAtmosphere(at)
		w.svAtm = at
	}
	pr := solve.Problem{EpochNs: epochNs, Stations: w.sst[:k], Cov: w.scov[:k*k], Alt: alt}
	if a.verified && w.tr.Predict(c.Key, epochNs+shift, &w.est2) && w.est2.SigmaH < 2000 {
		pr.Prior = solve.Prior{Valid: true, Pos: w.est2.ECEF, Cov: w.est2.Cov}
	}
	if a.lastFixNs > 0 {
		if dt := float64(epochNs+shift-a.lastFixNs) / 1e9; dt > 0.05 && dt < 60 {
			pr.Last = solve.Motion{Valid: true, Pos: a.lastFixPos, SigmaM: a.lastFixSigma, DtS: dt}
		}
	}
	if w.offN[w.dom] >= 50 {
		pr.HostFirstS, pr.HostValid = float64(c.FirstNs-off-epochNs)/1e9, true
	}
	f := &w.fix
	if err := w.sv.Solve(&pr, f); err != nil {
		e.ctr.solveErr.Add(1)
		return false
	}
	if !f.OK() {
		e.ctr.rejected.Add(1)
		for b := 0; b < len(e.ctr.rejects); b++ {
			if f.Reject&(1<<b) != 0 {
				e.ctr.rejects[b].Add(1)
			}
		}
		return false
	}
	if f.Flags&solve.FlagPriorFused != 0 {
		return false // a posterior, not an independent measurement
	}
	e.ctr.solveOK.Add(1)
	if f.Grade < 5 {
		e.ctr.grades[f.Grade].Add(1)
	}
	txNs := epochNs + shift + int64(math.Round(f.TxS*1e9))
	if f.Flags&solve.FlagAmbiguous == 0 {
		a.lastFixPos, a.lastFixNs, a.lastFixSigma = f.ECEF, txNs, f.SigmaH
	}
	if w.dbgFix != nil {
		w.dbgFix(c.Key, epochNs+shift, f, holdout)
	}
	w.residuals(c, k, f, relative)
	if holdout && !w.checking && f.Flags&solve.FlagAmbiguous == 0 && model.Displayable(f.Grade) {
		w.scoreFix(c, k, f, txNs)
	}
	*tf = track.Fix{ICAO: c.Key, TNs: txNs, NHyp: 1, NStations: f.NStations, DOF: f.DOF,
		Hyp: [2]track.FixHyp{{ECEF: f.ECEF, Cov: f.CovECEF}}}
	if f.Flags&solve.FlagAmbiguous != 0 && f.NAlt > 0 {
		// the alternative with its own covariance (the far branch of a
		// 3+alt pair is often ten times less certain; with the chosen
		// branch's covariance its tracker hypothesis gated its own later
		// fixes away and the wrong branch won, H-track)
		cov := f.Alt[0].CovECEF
		if cov[0] <= 0 || cov[4] <= 0 || cov[8] <= 0 {
			cov = f.CovECEF
		}
		tf.Hyp[1] = track.FixHyp{ECEF: f.Alt[0].ECEF, Cov: cov}
		tf.NHyp = 2
	}
	return true
}

// residuals feeds the per-station residual statistics (single-station
// sites of fixes with redundancy) and, for anchored fixes, the k_atm
// regression.
func (w *worker) residuals(c *group.Cluster, k int, f *solve.Fix, relative bool) {
	if f.DOF < 2 || f.Flags&solve.FlagAmbiguous != 0 {
		return
	}
	n := 0
	for i := 0; i < k; i++ {
		s := &c.Sites[w.sel[i]]
		if s.Count != 1 {
			continue
		}
		mb := &c.Members[s.First]
		jv := w.siteJ[i]
		r := quality.Residual{Station: mb.Station, ResidNs: float64(f.Resid[i]) / geo.C * 1e9, W: float64(f.WTest[i]),
			ClockVarNs2: math.Max(s.Var-jv, 0), JitterVarNs2: jv, Excluded: f.Excluded&(1<<i) != 0}
		w.resid[n] = r
		n++
	}
	if n > 0 {
		w.e.stq.Observe(w.now, w.resid[:n], w.priorJitter)
	}
	// k_atm: every 4th anchored fix with 3+ redundant rows
	if relative || f.DOF < 3 {
		return
	}
	if w.katmN++; w.katmN%4 != 0 {
		return
	}
	a1 := w.e.Atmosphere()
	a1.K = 1
	m := 0
	for i := 0; i < k; i++ {
		if f.Used&(1<<i) == 0 {
			continue
		}
		s := &c.Sites[w.sel[i]]
		w.ex1[m] = a1.Excess(s.Pos, f.ECEF)
		w.exr[m] = float64(f.Resid[i])
		w.exs[m] = math.Sqrt(math.Max(s.Var, 1)) * 1e-9 * geo.C
		m++
	}
	// Projected estimator (phase 4, quality WIRING.md §2): the used rows'
	// antenna positions, residuals (m) and σ (m), with the model k they were
	// solved with.
	kf := &w.katmFix
	kf.Target, kf.ModelK = f.ECEF, w.e.Atmosphere().K
	kf.Stations, kf.ResidM, kf.SigmaM = kf.Stations[:0], w.exr[:m], w.exs[:m]
	for i := 0; i < k; i++ {
		if f.Used&(1<<i) != 0 {
			kf.Stations = append(kf.Stations, c.Sites[w.sel[i]].Pos)
		}
	}
	kf.AltSigmaM, kf.AltResidM = 0, 0
	if f.AltUsed && !f.AltExcluded {
		kf.AltSigmaM, kf.AltResidM = solve.AltSigmaFallback, f.AltResid
	}
	w.e.katm.AddSolved(w.now, kf, w.e.Atmosphere())
}

func (w *worker) priorJitter(st model.StationIdx) float64 {
	if sc := w.clk.snap.Station(st); sc != nil {
		return sc.Class.PriorJitterNs()
	}
	if e, ok := w.sites.tab.get(st); ok {
		return e.class.PriorJitterNs()
	}
	return 0
}

// refState returns the hold-out reference state at tNs when it is fit for
// scoring (confirmed, not stale, not in probation, not suspended).
func (w *worker) refState(key uint32, tNs int64, st *ref.State) bool {
	if !w.e.refs.At(key, tNs, 0, st) {
		return false
	}
	bad := ref.FlagStale | ref.FlagProbation | ref.FlagUnstable | ref.FlagSuspended | ref.FlagOnGround | ref.FlagBackward
	return st.Flags&ref.FlagConfirmed != 0 && st.Flags&bad == 0
}

func sampleFrom(st *ref.State, pos geo.Vec3, covECEF [9]float64, tNs int64, grade uint8) quality.Sample {
	fr := geo.NewFrame(geo.LLH{Lat: st.Lat, Lon: st.Lon, H: st.HAE})
	d := fr.Rotate(pos.Sub(st.Pos))
	ce := fr.CovToENU(covECEF)
	v := fr.Rotate(st.Vel)
	return quality.Sample{TNs: tNs, Lat: st.Lat, Lon: st.Lon, AltM: st.HAE,
		EastM: d.X, NorthM: d.Y, UpM: d.Z, VelE: v.X, VelN: v.Y,
		CovEE: ce[0], CovEN: ce[1], CovNN: ce[4],
		RefCovEE: st.CovENU[0], RefCovEN: st.CovENU[1], RefCovNN: st.CovENU[4], Grade: grade}
}

// scoreFix compares a hold-out snapshot fix with the reference position.
func (w *worker) scoreFix(c *group.Cluster, k int, f *solve.Fix, txNs int64) {
	var st ref.State
	if !w.refState(c.Key, txNs, &st) {
		return
	}
	s := sampleFrom(&st, f.ECEF, f.CovECEF, txNs, f.Grade)
	w.e.qFix.Add(&s)
	n := 0
	for i := 0; i < k && n < len(w.stb); i++ {
		if f.Used&(1<<i) != 0 {
			w.stb[n] = c.Members[c.Sites[w.sel[i]].First].Station
			n++
		}
	}
	w.e.cells.Add(&s, w.stb[:n])
	w.e.ctr.holdoutFix.Add(1)
	w.e.observeLatency(c.Key, true, txNs, &st, f.ECEF, f.CovECEF)
	if w.e.o.LatencyFeedback {
		w.e.refs.ObserveFix(c.Key, txNs, f.ECEF, f.CovECEF)
	}
}

// publish sends a confirmed track estimate to the map (rate-limited per
// aircraft), remembers it for the feeders' results and scores hold-out
// aircraft. Hold-out aircraft are never published (they have ADS-B).
// Track times are already in the server/UTC base (relative domains are
// shifted before they reach the tracker).
func (w *worker) publish(key uint32, a *acState, est *track.Estimate, coast bool) {
	e := w.e
	if !coast && w.now-a.lastPubNs < int64(e.o.PublishInterval) {
		return
	}
	f := est.MLATFix()
	if a.relative {
		f.Flags |= model.FixFlagRelativeTime
	}
	if f.Vel != nil {
		a.vel, a.hasVel = *f.Vel, true
		f.Vel = &a.vel
	}
	// Rate limits per time base (H-track): measurement updates run on event
	// time (the group's corrected antenna time), coasting publications on the
	// tick's server time, which leads it by the network and grouping latency
	// (≈ 0.5 s). One shared stamp let every coasting publication block the
	// measurement updates of the next second, so live hold-out tracks were
	// almost never scored (4 samples in 16 minutes) and the map showed
	// predictions instead of updates.
	if coast {
		a.lastCoastNs = w.now
	} else {
		a.lastPubNs = w.now
	}
	if a.holdout {
		if coast {
			return // hold-out tracks are scored on measurement updates only
		}
		f.Flags |= model.FixFlagHoldout
		if w.now-a.lastScoreNs >= 1e9 && model.Displayable(f.Grade) {
			var st ref.State
			if w.refState(key, f.TUnixNs, &st) {
				a.lastScoreNs = w.now
				s := sampleFrom(&st, est.ECEF, est.Cov, f.TUnixNs, f.Grade)
				e.qTrack.Add(&s)
				e.ctr.holdoutTrack.Add(1)
			}
		}
		e.emitFix(f)
		return
	}
	if !model.Displayable(f.Grade) {
		return // grade D: track maintenance only
	}
	a.lastFix, a.pending = f, true
	if e.o.Updates != nil {
		e.o.Updates.HandleUpdate(updateFrom(&f, est, f.TUnixNs))
	}
	e.ctr.published.Add(1)
	e.emitFix(f)
}

// updateFrom converts a published fix into the global tracker's update.
func updateFrom(f *model.MLATFix, est *track.Estimate, tNs int64) model.DecodedUpdate {
	const ft = 0.3048
	u := model.DecodedUpdate{Recv: time.Unix(0, tNs).UTC(), ICAO: f.ICAO, MLAT: true, Src: model.SrcMLATAV,
		Msgs: 1, Grade: f.Grade}
	lat, lon, hae := est.LLH.Lat, est.LLH.Lon, est.LLH.H/ft
	u.Lat, u.Lon, u.AltGeom = &lat, &lon, &hae
	sig := est.SigmaH
	u.SigmaM = &sig
	if est.Speed() > 1 {
		gs := est.Speed() * 3600 / 1852
		trk := math.Mod(math.Atan2(est.VelENU[0], est.VelENU[1])*180/math.Pi+360, 360)
		vr := est.VelENU[2] / ft * 60
		u.GS, u.Track, u.VertRate = &gs, &trk, &vr
	}
	return u
}

// --- 1 Hz tick ---

// coastPublishNs: a confirmed track without measurements is still
// published (its prediction, with its growing σ and grade) for this long.
const coastPublishNs = int64(5e9)

// coastAfterNs: a track is coasting (its prediction is published at the
// tick) only when its last measurement update is older than this, in the
// tick's server time: the measurement updates themselves lag the server
// clock by the network and grouping latency (0.4-0.6 s).
const coastAfterNs = int64(1500e6)

func (w *worker) tick(nowNs int64) {
	e := w.e
	w.now = nowNs
	for st, n := range w.timed {
		e.rates.add(st, n)
		delete(w.timed, st)
	}
	w.emit = w.emit[:0]
	w.tr.Sweep(nowNs, w.sweepFn)
	live := make(map[uint32]bool, len(w.emit))
	for i := range w.emit {
		est := &w.emit[i]
		live[est.ICAO] = true
		a := w.ac[est.ICAO]
		if a == nil || !a.verified || nowNs-a.lastCoastNs < 1e9 || nowNs-est.LastUpdateNs < coastAfterNs || nowNs-est.LastUpdateNs > coastPublishNs {
			continue
		}
		w.publish(est.ICAO, a, est, true)
	}
	// address order, not map order: route / delete sequences (and with
	// them every output) are reproducible (H-track)
	w.keys = w.keys[:0]
	for key := range w.ac {
		w.keys = append(w.keys, key)
	}
	slices.Sort(w.keys)
	for _, key := range w.keys {
		a := w.ac[key]
		if a.pending {
			a.pending = false
			w.route(a, nowNs)
		}
		a.holdout = e.hold.In(key, nowNs)
		if live[key] && e.planner.Prio(key, nowNs) == plan.PrioNone && !a.holdout {
			// good ADS-B again: the MLAT track is not needed (DESIGN §9.5)
			w.tr.Delete(key)
			e.pos.del(key)
			delete(w.ac, key)
			e.ctr.deleted.Add(1)
			continue
		}
		if !live[key] && nowNs-a.lastSeen > 120e9 && !w.tr.Get(key, &w.est2) {
			delete(w.ac, key)
		}
	}
	ts := w.tr.Stats()
	w.trStats.Store(&ts)
	for k, r := range w.alt {
		if nowNs-r.tNs > 60e9 {
			delete(w.alt, k)
		}
	}
	for k, t := range w.verAt {
		if nowNs-t > 120e9 {
			delete(w.verAt, k)
		}
	}
}

// route queues an aircraft's latest result for every station that passes
// the minimum-contribution rule (DESIGN §8.4).
func (w *worker) route(a *acState, nowNs int64) {
	e := w.e
	if !e.cfg.ReturnResults || len(e.o.Results) == 0 {
		return
	}
	win := int64(e.o.ContribWindow)
	for i := 0; i < a.nContrib; i++ {
		c := a.contrib[i]
		if nowNs-c.tNs > win {
			continue
		}
		if !e.rates.eligible(c.st, int64(e.o.MinTimedPerMin)) {
			e.ctr.resultsRefused.Add(1)
			continue
		}
		e.route.add(c.st, &a.lastFix)
	}
}
