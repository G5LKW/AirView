package group

import (
	"errors"
	"math"
	"math/bits"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// mPerNs is the speed of light in metres per nanosecond.
const mPerNs = geo.C * 1e-9

// Options are the assembler's collaborators.
type Options struct {
	Config Config
	// Clock converts timestamps (required).
	Clock Clock
	// Sites gives the stations' antenna positions (required).
	Sites Sites
	// Hints (optional) tightens the gate with a predicted position.
	Hints Hints
	// Selector (optional) restricts clustering to MLAT candidates and applies
	// the solve budget. Without it every group is clustered and every
	// solvable cluster is admitted.
	Selector Selector
	// Sink receives the clusters (required).
	Sink Sink
}

// cand is one usable copy in the working set.
type cand struct {
	st     model.StationIdx
	ch     int8
	late   bool
	epoch  uint32
	ticks  uint64
	sig    float32
	t      Time
	cv, jv float64
	v      float64 // cv + jv (floored)
	pos    geo.Vec3
	ps     float64
	dom    uint64   // time domain (Domains), 0 without
	rel    float64  // t − t0 (ns)
	e      float64  // hinted: emission time estimate rel − ρ̂/c (ns)
	rho    float64  // hinted: range to the predicted position (m)
	u      geo.Vec3 // hinted: unit vector station → predicted position
}

// Assembler turns ingest groups into clusters (DESIGN §9.3). It is not safe
// for concurrent use: the ingest calls Observe and ObserveLate from its
// sweeper goroutine only. Stats may be called from any goroutine.
type Assembler struct {
	cfg   Config
	clock Clock
	cross CrossCov
	doms  Domains
	sites Sites
	hints Hints
	sel   Selector
	sink  Sink

	c       [MaxMembers]cand
	n       int
	adjB    [MaxMembers]uint64 // baseline gate only
	adjHint [MaxMembers]uint64 // baseline AND hint gates
	ratB    [MaxMembers][MaxMembers]float32
	ratH    [MaxMembers][MaxMembers]float32
	colo    [MaxMembers]uint64 // pairs closer than ColocatedM
	order   [MaxMembers]int8
	srch    cliqueSearch

	// output scratch
	cl      Cluster
	members [MaxMembers]Member
	sitesB  [MaxMembers]Site
	idx     [MaxMembers]int8
	siteOf  [MaxMembers]int8

	// hint of the working set
	hinted bool
	hsig   float64
	hpos   geo.Vec3

	// late copies
	lc   [MaxMembers]cand
	late *lateStore

	seq   uint64
	stats counters
}

var _ ingest.MLATSink = (*Assembler)(nil)
var _ ingest.LateObserver = (*Assembler)(nil)

// New returns an assembler.
func New(o Options) (*Assembler, error) {
	if o.Clock == nil || o.Sites == nil || o.Sink == nil {
		return nil, errors.New("group: Clock, Sites and Sink are required")
	}
	a := &Assembler{cfg: o.Config.Normalize(), clock: o.Clock, sites: o.Sites, hints: o.Hints, sel: o.Selector, sink: o.Sink}
	a.cross, _ = o.Clock.(CrossCov)
	a.doms, _ = o.Clock.(Domains)
	if a.cfg.LateHoldNs > 0 {
		a.late = newLateStore(a.cfg.LateSlots, a.cfg.LateMembers)
	}
	a.useBaseline()
	return a, nil
}

// Config returns the normalised configuration in use.
func (a *Assembler) Config() Config { return a.cfg }

// excluded reports whether a frame can never be multilaterated: Mode A/C
// (handled by ClusterModeAC) and DF18 with CF >= 2 (TIS-B / ADS-R, sent by a
// ground station, not the aircraft).
func excluded(g *ingest.Group) bool {
	if g.Kind != model.FrameModeSShort && g.Kind != model.FrameModeSLong {
		return true
	}
	return len(g.Data) > 0 && g.Data[0]>>3 == 18 && g.Data[0]&7 >= 2
}

// Observe implements ingest.MLATSink: it clusters a flushed group and emits
// the clusters to the sink.
func (a *Assembler) Observe(g *ingest.Group) {
	a.stats.groups.Add(1)
	if excluded(g) {
		a.stats.excluded.Add(1)
		return
	}
	if a.sel != nil && !a.sel.Candidate(g.Key, g.DF, g.FirstNs) {
		a.stats.notCandidate.Add(1)
		return
	}
	a.n = a.load(g, a.c[:], false)
	h := headerOf(g)
	if a.n < 2 {
		if a.n == 1 && a.late != nil {
			a.late.retainSingle(&h, &a.c[0])
		}
		a.stats.tooFew.Add(1)
		return
	}
	a.prepare(g.Key)
	altFt, altSrc := a.altitude(g)
	a.clusterAll(&h, altFt, altSrc, false)
}

// prepare fetches the hint for the working set a.c[:a.n] and builds its
// graphs.
func (a *Assembler) prepare(key uint32) {
	a.hinted, a.hsig = false, 0
	if a.hints != nil && a.cfg.HintMaxSigmaM > 0 {
		p, sig, ok := a.hints.Predict(key, a.c[0].t)
		if ok && sig >= 0 && sig <= a.cfg.HintMaxSigmaM {
			a.hinted, a.hsig, a.hpos = true, sig, p
		}
	}
	a.buildGraph(a.c[:a.n])
}

// clusterAll extracts up to MaxParts clusters from the working set and emits
// them (DESIGN §9.3 "Clustering").
func (a *Assembler) clusterAll(h *header, altFt float64, altSrc uint8, late bool) {
	all := uint64(1)<<a.n - 1
	if a.n == 64 {
		all = ^uint64(0)
	}
	hinted, conflict := a.hinted, false
	if hinted {
		// Use the hint graph unless it fragments what the baseline graph
		// keeps together (stale track or wrong 3+alt branch).
		a.useHint()
		nh := bits.OnesCount64(a.search(all))
		bad := false
		if 2*nh < a.n { // otherwise no baseline clique can be twice as large
			a.useBaseline()
			bad = 2*nh < bits.OnesCount64(a.search(all))
		}
		if bad {
			conflict, hinted = true, false
			a.stats.hintConflict.Add(1)
		} else {
			a.useHint()
			a.stats.hinted.Add(1)
		}
	} else {
		a.useBaseline()
	}
	rem := all
	var parts [8]uint64
	np := 0
	for np < a.cfg.MaxParts && bits.OnesCount64(rem) >= 2 {
		cl := a.search(rem)
		if bits.OnesCount64(cl) < 2 {
			break
		}
		parts[np] = cl
		np++
		rem &^= cl
	}
	a.stats.leftover.Add(uint64(bits.OnesCount64(rem)))
	if np > 1 {
		a.stats.split.Add(1)
	}
	for i := 0; i < np; i++ {
		a.seq++
		a.emit(*h, a.c[:a.n], parts[i], uint8(i), uint8(np), a.seq, altFt, altSrc, hinted, conflict, late)
	}
}

// useBaseline / useHint select the graph the search works on: the baseline
// gate alone, or baseline AND hint gates (both are necessary conditions for
// a single transmission, so their intersection is too).
func (a *Assembler) useBaseline() { a.srch.adj, a.srch.ratio = &a.adjB, &a.ratB }
func (a *Assembler) useHint()     { a.srch.adj, a.srch.ratio = &a.adjHint, &a.ratH }

// load converts the usable copies of g into cs and returns their number.
func (a *Assembler) load(g *ingest.Group, cs []cand, late bool) int {
	n := 0
	var untimed, noClock, noSite uint64
	for i := range g.Copies {
		cp := &g.Copies[i]
		if !cp.Timed || cp.SameStation || cp.MapOnly {
			untimed++
			continue
		}
		if n == len(cs) {
			break
		}
		t, cv, jv, ok := a.clock.NetTime(cp.Station, cp.Epoch, cp.Ticks, g.Kind)
		if !ok {
			noClock++
			continue
		}
		pos, ps, ok := a.sites.Site(cp.Station)
		if !ok {
			noSite++
			continue
		}
		c := &cs[n]
		*c = cand{st: cp.Station, ch: cp.Channel, late: late, epoch: cp.Epoch, ticks: cp.Ticks, sig: cp.Signal,
			t: t, cv: max(cv, 0), jv: max(jv, 0), pos: pos, ps: max(ps, 0)}
		c.v = max(c.cv+c.jv, 1e-6)
		if a.doms != nil {
			c.dom = a.doms.Domain(cp.Station)
		}
		n++
	}
	a.stats.copies.Add(uint64(len(g.Copies)))
	a.stats.untimed.Add(untimed)
	a.stats.noClock.Add(noClock)
	a.stats.noSite.Add(noSite)
	a.stats.usable.Add(uint64(n))
	return n
}

// buildGraph fills the baseline (and with a hint, the hint) adjacency, gate
// ratios and distances for cs.
//
// Baseline gate (package doc, eq. B):
//
//	|t_k − t_i| ≤ (d_ik + SlackM + 3(s_i + s_k))/c + n·sqrt(v_i + v_k − 2 cov_ik)
//
// Hint gate (eq. H), with e = t − ρ̂/c:
//
//	|e_k − e_i| ≤ n·sqrt(σp²‖u_k − u_i‖²/c² + v_ik) + (q_i + q_k + HintSlackM + 3(s_i + s_k))/c
//	q = (nσp)² / (2·max(ρ̂ − nσp, 1 km))
func (a *Assembler) buildGraph(cs []cand) {
	n := len(cs)
	ns := a.cfg.NSigma
	hinted, hsig := a.hinted, a.hsig
	t0 := cs[0].t
	var q [MaxMembers]float64
	r := ns * hsig
	for i := range cs {
		c := &cs[i]
		c.rel = c.t.Sub(t0)
		a.adjB[i], a.adjHint[i], a.colo[i] = 0, 0, 0
		if hinted {
			d := a.hpos.Sub(c.pos)
			c.rho = max(d.Norm(), 1)
			c.u = d.Scale(1 / c.rho)
			c.e = c.rel - c.rho/mPerNs
			q[i] = r * r / (2 * max(c.rho-r, 1000))
		}
	}
	for i := 0; i < n; i++ {
		ci := &cs[i]
		for k := i + 1; k < n; k++ {
			ck := &cs[k]
			d := ci.pos.Dist(ck.pos)
			if d < a.cfg.ColocatedM {
				a.colo[i] |= 1 << k
				a.colo[k] |= 1 << i
			}
			if ci.st == ck.st || ci.dom != ck.dom {
				a.ratB[i][k], a.ratB[k][i] = math.MaxFloat32, math.MaxFloat32
				a.ratH[i][k], a.ratH[k][i] = math.MaxFloat32, math.MaxFloat32
				continue
			}
			v := ci.v + ck.v
			if a.cross != nil {
				v -= 2 * a.cross.CrossCov(ci.st, ck.st)
			}
			v = max(v, 1e-6)
			sp := 3 * (ci.ps + ck.ps)
			gate := (d+a.cfg.SlackM+sp)/mPerNs + ns*math.Sqrt(v)
			rb := math.Abs(ck.rel-ci.rel) / gate
			a.ratB[i][k], a.ratB[k][i] = float32(rb), float32(rb)
			if rb <= 1 {
				a.adjB[i] |= 1 << k
				a.adjB[k] |= 1 << i
			}
			if !hinted {
				continue
			}
			if rb > 1 { // the combined gate fails anyway
				a.ratH[i][k], a.ratH[k][i] = float32(rb), float32(rb)
				continue
			}
			du := ck.u.Sub(ci.u)
			gh := ns*math.Sqrt(hsig*hsig*du.Dot(du)/(mPerNs*mPerNs)+v) + (q[i]+q[k]+a.cfg.HintSlackM+sp)/mPerNs
			rh := max(math.Abs(ck.e-ci.e)/gh, rb)
			a.ratH[i][k], a.ratH[k][i] = float32(rh), float32(rh)
			if rh <= 1 {
				a.adjHint[i] |= 1 << k
				a.adjHint[k] |= 1 << i
			}
		}
	}
	// time order for the greedy fallback (insertion sort, n <= 64)
	for i := 0; i < n; i++ {
		a.order[i] = int8(i)
		for j := i; j > 0 && cs[a.order[j]].rel < cs[a.order[j-1]].rel; j-- {
			a.order[j], a.order[j-1] = a.order[j-1], a.order[j]
		}
	}
}

// search returns the best clique within p of the graph in use.
func (a *Assembler) search(p uint64) uint64 {
	r := a.srch.run(p, a.cfg.SearchBudget)
	if a.srch.exhausted {
		a.stats.budgetOut.Add(1)
		if r == 0 {
			ord := a.order[:0]
			for _, v := range a.order[:a.n] {
				if p&(1<<uint(v)) != 0 {
					ord = append(ord, v)
				}
			}
			r = greedy(a.srch.adj, p, ord)
		}
	}
	return r
}

// altitude returns the altitude to use for the group's clusters.
func (a *Assembler) altitude(g *ingest.Group) (float64, uint8) {
	ft, src := frameAltitude(g.Data)
	if src == AltNone && a.hints != nil && a.n > 0 {
		if h, ok := a.hints.AltitudeFt(g.Key, a.c[0].t); ok {
			return h, AltHint
		}
	}
	return ft, src
}
