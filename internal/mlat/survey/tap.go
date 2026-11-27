package survey

import (
	"math"
	"sync"
	"sync/atomic"

	"airview/internal/ingest"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/ref"
	"airview/internal/model"
)

// ClockSource gives the clock filter's current snapshot (*clock.Filter
// implements it: mlat.Engine.Filter()).
type ClockSource interface {
	Snapshot() *clock.Snapshot
}

// RefSource is the reference store's transmit-instant estimator
// (*ref.Store implements it: mlat.Engine.Refs()).
type RefSource interface {
	TransmitInstant(key uint32, baseNs int64, arr []ref.Arrival, atm *geo.Atmosphere, st *ref.State) (tauDtNs, sigmaNs float64, ok bool)
}

// maxCopies bounds the copies of one group the tap looks at.
const maxCopies = 64

// active is the immutable set of survey targets, read lock-free on the
// hot path.
type active struct {
	bits []uint64
	jobs map[model.StationIdx]*job
	n    int
}

func (a *active) has(st model.StationIdx) bool {
	i := int(st) >> 6
	return a != nil && i < len(a.bits) && a.bits[i]&(1<<(uint(st)&63)) != 0
}

// tapScratch is the per-call working memory (pooled: Observe may be called
// from several goroutines).
type tapScratch struct {
	peers   [maxCopies]peerArr
	arr     [maxCopies]ref.Arrival
	pos     [maxCopies]geo.Vec3
	targets [8]int
}

type peerArr struct {
	st   model.StationIdx
	dom  uint64
	rel  bool
	pos  geo.Vec3
	ns   int64
	frac float64
	sig  float64 // ns
}

var scratchPool = sync.Pool{New: func() any { return new(tapScratch) }}

// tapCounters are the tap's statistics.
type tapCounters struct {
	groups, matched, thinned, fewPeers, noRef, inconsistent, accepted, masked atomic.Uint64
}

// Observe implements ingest.MLATSink for the survey: groups whose copies
// include an active survey target become observations (see Obs). It is
// cheap for every other group (one bitmap test per copy) and safe for
// concurrent use. Late deliveries carry only the new copies and are not
// used. Copies from map-only sources (Beast 30004), untimed copies (the
// owner withheld timestamps: the consent gate of DESIGN §10.5) and second
// channels of a station are never used.
func (s *Surveyor) Observe(g *ingest.Group) {
	if g.Kind != model.FrameModeSLong || len(g.Copies) < 2 {
		return
	}
	act := s.active.Load()
	if act == nil || act.n == 0 {
		return
	}
	s.tap.groups.Add(1)
	sc := scratchPool.Get().(*tapScratch)
	defer scratchPool.Put(sc)
	nt := 0
	for i := range g.Copies {
		c := &g.Copies[i]
		if c.Timed && !c.MapOnly && !c.SameStation && act.has(c.Station) && nt < len(sc.targets) {
			sc.targets[nt] = i
			nt++
		}
	}
	if nt == 0 {
		return
	}
	s.tap.matched.Add(1)
	// cheap thinning pre-check: does any target want this aircraft now?
	want := false
	for _, ti := range sc.targets[:nt] {
		if j := act.jobs[g.Copies[ti].Station]; j != nil && j.col.wants(g.Key, g.FirstNs) {
			want = true
			break
		}
	}
	if !want {
		s.tap.thinned.Add(1)
		return
	}
	snap := s.o.Clock.Snapshot()
	if snap == nil {
		return
	}
	excl := s.excluded.Load()
	// peers: synced, located, not under survey themselves, not suspect
	np := 0
	for i := range g.Copies {
		c := &g.Copies[i]
		if np == maxCopies {
			break
		}
		if !c.Timed || c.MapOnly || c.SameStation || act.has(c.Station) || excl.has(c.Station) {
			continue
		}
		stc := snap.Station(c.Station)
		if stc == nil || !stc.Synced || stc.Suspect {
			continue
		}
		pos, ok := snap.Position(c.Station)
		if !ok {
			continue
		}
		ns, frac, cv, jv, ok := snap.NetTimeUnix(clock.Query{Station: c.Station, Epoch: c.Epoch, Ticks: c.Ticks, Kind: g.Kind})
		if !ok {
			continue
		}
		sig := math.Sqrt(cv + jv)
		if !(sig <= s.cfg.MaxPeerSigmaNs) {
			continue
		}
		dom, _ := snap.Domain(c.Station)
		sc.peers[np] = peerArr{st: c.Station, dom: dom, rel: stc.RelativeTime, pos: pos, ns: ns, frac: frac, sig: sig}
		np++
	}
	// the time domain with the most peers (anchored on ties)
	bestDom, bestN, bestRel := uint64(0), 0, true
	for i := 0; i < np; i++ {
		d := sc.peers[i].dom
		n := 0
		for j := 0; j < np; j++ {
			if sc.peers[j].dom == d {
				n++
			}
		}
		if n > bestN || (n == bestN && bestRel && !sc.peers[i].rel) {
			bestDom, bestN, bestRel = d, n, sc.peers[i].rel
		}
	}
	if bestN < s.cfg.MinPeers {
		s.tap.fewPeers.Add(1)
		return
	}
	base := g.FirstNs
	na := 0
	for i := 0; i < np; i++ {
		pa := &sc.peers[i]
		if pa.dom != bestDom {
			continue
		}
		sc.arr[na] = ref.Arrival{Station: pa.pos, DtNs: float64(pa.ns-base) + pa.frac, SigmaNs: pa.sig}
		sc.pos[na] = pa.pos
		na++
	}
	atm := s.atmosphere()
	var st ref.State
	_, _, ok := s.o.Refs.TransmitInstant(g.Key, base, sc.arr[:na], &atm, &st)
	if !ok || !st.Usable() {
		s.tap.noRef.Add(1)
		return
	}
	// consistency of the peers with the reference position: each peer's
	// emission estimate e_j = t_j − ρ_j/c must agree with the others;
	// one bad peer (clock fault, wrong position) is removed.
	tau, varTau, eb, n, ok := s.emission(sc.arr[:na], &st, &atm)
	if !ok || n < s.cfg.MinPeers {
		s.tap.inconsistent.Add(1)
		return
	}
	cov := st.Cov
	o := Obs{TauNs: base + int64(math.Round(tau)), Domain: uint32(bestDom), Anchored: !bestRel, Peers: uint8(min(n, 255)),
		Key: g.Key, P: st.Pos, CovP: [6]float64{cov[0], cov[1], cov[2], cov[4], cov[5], cov[8]}, Ebar: eb,
		VarTauM2: varTau * 1e-18 * cLight * cLight}
	for _, ti := range sc.targets[:nt] {
		c := &g.Copies[ti]
		j := act.jobs[c.Station]
		if j == nil {
			continue
		}
		if ap := j.approx.Load(); ap != nil && ap.belowMask(o.P) {
			// below the elevation mask at the approximate position: the
			// solver would not use it, keep the memory for useful ones
			s.tap.masked.Add(1)
			continue
		}
		o.Ticks, o.Epoch, o.Class = c.Ticks, c.Epoch, c.ClockCls
		if j.col.add(&o, sc.pos[:na]) {
			s.tap.accepted.Add(1)
		}
	}
}

// emission computes the weighted emission time (ns relative to the group
// base) from the arrivals with the reference position, removes the worst
// peer while it is inconsistent (> 5σ and > 300 ns), and returns τ, its
// variance (ns²), the weighted mean unit vector peers → aircraft and the
// number of peers kept.
func (s *Surveyor) emission(arr []ref.Arrival, st *ref.State, atm *geo.Atmosphere) (tau, varTau float64, eb geo.Vec3, n int, ok bool) {
	var e [maxCopies]float64
	var keep [maxCopies]bool
	for i := range arr {
		e[i] = arr[i].DtNs - atm.PropagationNs(arr[i].Station, st.Pos)
		keep[i] = true
	}
	n = len(arr)
	for n >= s.cfg.MinPeers {
		var sw, swe float64
		for i := range arr {
			if keep[i] {
				w := 1 / (arr[i].SigmaNs * arr[i].SigmaNs)
				sw += w
				swe += w * e[i]
			}
		}
		tau = swe / sw
		varTau = 1 / sw
		if n < 3 {
			// two peers: no majority to decide which one is wrong
			if n == 2 {
				var d, sg [2]float64
				m := 0
				for i := range arr {
					if keep[i] {
						d[m], sg[m] = e[i], arr[i].SigmaNs
						m++
					}
				}
				if math.Abs(d[0]-d[1]) > 5*math.Hypot(sg[0], sg[1])+300 {
					return 0, 0, eb, n, false
				}
			}
			break
		}
		worst, wi := 0.0, -1
		for i := range arr {
			if !keep[i] {
				continue
			}
			z := math.Abs(e[i] - tau)
			if z > 5*arr[i].SigmaNs+300 && z > worst {
				worst, wi = z, i
			}
		}
		if wi < 0 {
			break
		}
		keep[wi] = false
		n--
	}
	if n < s.cfg.MinPeers {
		return 0, 0, eb, n, false
	}
	var sw float64
	for i := range arr {
		if keep[i] {
			w := 1 / (arr[i].SigmaNs * arr[i].SigmaNs)
			eb = eb.Add(st.Pos.Sub(arr[i].Station).Unit().Scale(w))
			sw += w
		}
	}
	eb = eb.Scale(1 / sw)
	return tau, varTau, eb, n, true
}

// Tee passes every group to the survey and then to Next (the MLAT engine
// or the timing quarantine in front of it). Put it before the timing
// quarantine: a station under survey is often one whose timing is not
// proven yet (it has no position to prove it with).
type Tee struct {
	Next ingest.MLATSink
	S    *Surveyor
}

// Observe implements ingest.MLATSink.
func (t Tee) Observe(g *ingest.Group) {
	if t.S != nil {
		t.S.Observe(g)
	}
	if t.Next != nil {
		t.Next.Observe(g)
	}
}

// ObserveLate implements ingest.LateObserver (late copies go to Next
// only).
func (t Tee) ObserveLate(g *ingest.Group) {
	if lo, ok := t.Next.(ingest.LateObserver); ok {
		lo.ObserveLate(g)
	}
}

// approxPos is a job's approximate target position (declared, hint or the
// last solution) for the tap's elevation pre-filter.
type approxPos struct {
	u, up geo.Vec3
	// sinMin is the sine of the pre-filter's elevation limit (the solver's
	// mask less a margin for the position's uncertainty).
	sinMin float64
}

func newApprox(u geo.Vec3, maskDeg, marginDeg float64) *approxPos {
	fr := geo.NewFrameECEF(u)
	return &approxPos{u: u, up: fr.Up(), sinMin: math.Sin((maskDeg - marginDeg) * math.Pi / 180)}
}

func (a *approxPos) belowMask(p geo.Vec3) bool {
	d := p.Sub(a.u)
	n := d.Norm()
	return n > 0 && d.Dot(a.up) < a.sinMin*n
}
