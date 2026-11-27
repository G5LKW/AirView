package trust

import (
	"math"
	"slices"
	"time"

	"airview/internal/geoid"
	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/modes"
)

// Config tunes the gate. Zero fields take the defaults.
type Config struct {
	// Disabled forwards every copy (no quarantine).
	Disabled bool
	// MinRefs is the number of distinct reference stations a station's
	// timing must agree with (default 2).
	MinRefs int
	// Window is the length of one fit window per station pair (default
	// 30 s); a window is judged with at least MinWindowSamples and at most
	// MaxWindowSamples comparisons (defaults 8 and 64).
	Window           time.Duration
	MinWindowSamples int
	MaxWindowSamples int
	// MaxSigmaNs is the floor of the per-pair residual threshold (default
	// 1500 ns); the threshold is max(MaxSigmaNs, 3·√(jitter_a² + jitter_b²))
	// with the class prior jitters.
	MaxSigmaNs float64
	// GoodWindows good windows prove the timing (default 8) when they come
	// from at least MinRefs references with more good than bad windows, at
	// most MaxBadFrac of the trusted references judged so far have as many
	// bad windows as good ones (default 0.2), and the first comparison is
	// at least MinAge old (default 3 min).
	GoodWindows int
	MaxBadFrac  float64
	MinAge      time.Duration
	// RefreshEvery re-reads station states and positions (default 10 s of
	// group time); ForgetAfter drops idle stations (default 30 min).
	RefreshEvery time.Duration
	ForgetAfter  time.Duration
	// RefActiveFor: a trusted station counts as an active reference for
	// this long after its last copy (default 2 min). With fewer than
	// MinRefs active references the gate is in bootstrap: quarantined
	// stations are judged against each other too (a new network starts
	// with no trusted station at all).
	RefActiveFor time.Duration
	// ProvenGrace: after a proof the gate treats the station as trusted for
	// this long even if the station source still says quarantined (the
	// registry write is asynchronous); afterwards the source wins again, so
	// an admin re-quarantine takes effect (default 2 min).
	ProvenGrace time.Duration

	on bool
}

// ConfigFromModel maps the file block aggregator.mlat.quarantine (DESIGN
// §20.3; zero fields keep the defaults). Whether new stations start
// quarantined is the registry's policy (registry.PolicyFromConfig maps
// quarantine.disabled onto Policy.NoTimingQuarantine).
func ConfigFromModel(m model.AggMLAT) Config {
	q := m.Quarantine
	return Config{
		Disabled:    q.Disabled,
		MinRefs:     q.MinRefs,
		Window:      time.Duration(q.WindowSec) * time.Second,
		MaxSigmaNs:  q.MaxSigmaNs,
		GoodWindows: q.GoodWindows,
		MaxBadFrac:  q.MaxBadFrac,
		MinAge:      time.Duration(q.MinAgeSec) * time.Second,
	}
}

func (c Config) normalized() Config {
	c.on = !c.Disabled
	pos := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	dur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	pos(&c.MinRefs, 2)
	dur(&c.Window, 30*time.Second)
	pos(&c.MinWindowSamples, 8)
	pos(&c.MaxWindowSamples, 64)
	c.MaxWindowSamples = max(c.MaxWindowSamples, c.MinWindowSamples)
	if c.MaxSigmaNs <= 0 {
		c.MaxSigmaNs = 1500
	}
	pos(&c.GoodWindows, 8)
	if c.MaxBadFrac <= 0 {
		c.MaxBadFrac = 0.2
	}
	dur(&c.MinAge, 3*time.Minute)
	dur(&c.RefreshEvery, 10*time.Second)
	dur(&c.ForgetAfter, 30*time.Minute)
	dur(&c.RefActiveFor, 2*time.Minute)
	dur(&c.ProvenGrace, 2*time.Minute)
	return c
}

// Report is the judging progress of one quarantined station. It holds no
// peer identities (owner view).
type Report struct {
	State       string    `json:"state"`
	Since       time.Time `json:"since"`       // first comparison
	Samples     uint64    `json:"samples"`     // comparisons with references
	Windows     int       `json:"windows"`     // judged fit windows
	Good        int       `json:"good"`        // good windows
	Bad         int       `json:"bad"`         // bad windows
	Refs        int       `json:"refs"`        // distinct references with a good window
	LastSigmaNs float64   `json:"lastSigmaNs"` // robust residual σ of the last judged window
	Proven      bool      `json:"proven"`
	// Reason says what is missing ("" when proven).
	Reason string `json:"reason,omitempty"`
	// NoTrustedRef: no trusted station has shared a position message with
	// this station for RefActiveFor (a new regional cluster); it is judged
	// against the other new stations it hears with (local bootstrap,
	// finding SCALE-06).
	NoTrustedRef bool `json:"noTrustedRef,omitempty"`
}

// evalState judges one quarantined station.
type evalState struct {
	first   int64 // group time of the first comparison (0 = none)
	samples uint64
	pairs   map[model.StationIdx]*pairWin
	good    int
	bad     int
	perRef  map[model.StationIdx]*refStat
	lastSig float64
	proven  int64 // group time of the proof (0 = not proven)
	stamped time.Time
	// created is the group time judging started; lastTrusted the group time
	// of the last position group shared with a trusted reference (0 =
	// never). Together they decide the local bootstrap (isolated).
	created     int64
	lastTrusted int64
}

func newEvalState(now int64) *evalState {
	return &evalState{pairs: map[model.StationIdx]*pairWin{}, perRef: map[model.StationIdx]*refStat{}, created: now}
}

// isolated reports whether the station has heard no position message
// together with a trusted reference for RefActiveFor (and has been judged
// at least that long): its region has no trusted station yet, so the
// network-wide bootstrap rule is applied locally (finding SCALE-06: a
// second regional cluster was never promoted once another region was
// trusted).
func (g *Gate) isolated(s *evalState, now int64) bool {
	lim := int64(g.cfg.RefActiveFor)
	return now-s.created >= lim && now-s.lastTrusted > lim
}

// refStat counts the judged windows against one reference. trusted is the
// reference's state at its last window: windows against a quarantined
// reference (bootstrap) can be bad through the reference's own fault, so
// only trusted references can make a station "inconsistent".
type refStat struct {
	good, bad int
	trusted   bool
}

// pairWin is the current fit window of (station, reference).
type pairWin struct {
	epochQ, epochR uint32
	t0             int64
	thr            float64
	x, y           []float64
}

// isAirbornePos reports a DF17/18 airborne position message.
func isAirbornePos(d []byte) bool {
	if len(d) != 14 {
		return false
	}
	df := d[0] >> 3
	if df != 17 && !(df == 18 && d[0]&7 < 2) {
		return false
	}
	tc := d[4] >> 3
	return (tc >= 9 && tc <= 18) || (tc >= 20 && tc <= 22)
}

const cAir = 299_792_458.0 / 1.0003 // m/s, a mean tropospheric refractive index

// judge compares the quarantined copies of a DF17 position group with the
// reference copies. Sweeper goroutine.
func (g *Gate) judge(grp *ingest.Group, now int64) {
	if !isAirbornePos(grp.Data) {
		return
	}
	bootstrap := g.activeRefs(now) < g.cfg.MinRefs
	// references (trusted, located, timed), GPS classes first; quarantined
	// stations are references too in the bootstrap, globally (fewer than
	// MinRefs trusted stations in the network) or locally (an isolated
	// quarantined station in this group: no trusted co-observer)
	g.refs = g.refs[:0]
	nq, nt := 0, 0
	local := false
	for i := range grp.Copies {
		c := &grp.Copies[i]
		if !c.Timed || c.SameStation || c.Ticks == 0 {
			continue
		}
		e := g.ent[c.Station]
		if e == nil || !e.info.Located || c.ClockCls.NominalHz() <= 0 {
			continue
		}
		switch e.info.State {
		case Trusted:
			g.refs = append(g.refs, i)
			nt++
		case Quarantined:
			nq++
			g.refs = append(g.refs, i)
		}
	}
	if nq == 0 {
		return
	}
	for _, i := range g.refs {
		if e := g.ent[grp.Copies[i].Station]; e.info.State == Quarantined && e.ref != nil {
			if nt > 0 {
				e.ref.lastTrusted = now
			} else if g.isolated(e.ref, now) {
				local = true
			}
		}
	}
	if !bootstrap && !local {
		// trusted references only
		k := 0
		for _, i := range g.refs {
			if g.ent[grp.Copies[i].Station].info.State == Trusted {
				g.refs[k] = i
				k++
			}
		}
		g.refs = g.refs[:k]
	}
	if len(g.refs) == 0 {
		return
	}
	slices.SortStableFunc(g.refs, func(a, b int) int {
		ga, gb := grp.Copies[a].ClockCls.GPS(), grp.Copies[b].ClockCls.GPS()
		switch {
		case ga && !gb:
			return -1
		case gb && !ga:
			return 1
		}
		return 0
	})
	var m modes.Message
	if modes.Decode(grp.Data, &m) != nil || m.Valid&modes.FCPR == 0 || m.CPRSurface {
		return
	}
	var hM float64
	switch {
	case m.Valid&modes.FAltGNSS != 0:
		hM = float64(m.AltGNSS) * 0.3048
	case m.Valid&modes.FAltBaro != 0:
		hM = float64(m.AltBaro) * 0.3048 // baro ≈ MSL; the geoid is added below
	default:
		return
	}
	r0 := g.ent[grp.Copies[g.refs[0]].Station]
	lat, lon, ok := modes.CPRAirborneLocal(r0.info.Lat, r0.info.Lon, m.CPRLat, m.CPRLon, m.CPROdd)
	if !ok {
		return
	}
	if m.Valid&modes.FAltGNSS == 0 {
		if n, ok := geoid.Undulation(lat, lon); ok {
			hM += n
		}
	}
	P := geo.ToECEF(lat, lon, hM)
	checked := 0
	for i := range grp.Copies {
		cq := &grp.Copies[i]
		eq := g.ent[cq.Station]
		if eq == nil || eq.info.State != Quarantined || eq.ref == nil || !cq.Timed || cq.SameStation ||
			cq.Ticks == 0 || !eq.info.Located || cq.ClockCls.NominalHz() <= 0 {
			continue
		}
		// The local decode must agree from the station's own position too
		// (an unambiguous decode within its reception range).
		if la, lo, ok := modes.CPRAirborneLocal(eq.info.Lat, eq.info.Lon, m.CPRLat, m.CPRLon, m.CPROdd); !ok ||
			math.Abs(la-lat) > 1e-6 || math.Abs(lo-lon) > 1e-6 {
			continue
		}
		tq := g.tickNs(eq, cq)
		dq := P.Sub(eq.ecef).Norm()
		used := 0
		// peers (other quarantined stations) only in the global bootstrap
		// or for an isolated station (local bootstrap)
		peers := bootstrap || g.isolated(eq.ref, now)
		for _, ri := range g.refs {
			if ri == i || used == 3 {
				continue
			}
			cr := &grp.Copies[ri]
			er := g.ent[cr.Station]
			if cr.Station == cq.Station || (!peers && er.info.State != Trusted) {
				continue
			}
			tr := g.tickNs(er, cr)
			y := (tq - tr) - (dq-P.Sub(er.ecef).Norm())/cAir*1e9
			g.addSample(cq.Station, eq, cr.Station, cq, cr, now, y)
			used++
		}
		if checked++; checked == 4 {
			break
		}
	}
}

// tickNs returns a copy's ticks in ns relative to the station's base of
// the current epoch.
func (g *Gate) tickNs(e *entry, c *ingest.Copy) float64 {
	if !e.baseSet || e.baseEpoch != c.Epoch || c.Ticks < e.baseTicks && e.baseTicks-c.Ticks > 1<<40 {
		e.baseSet, e.baseEpoch, e.baseTicks = true, c.Epoch, c.Ticks
	}
	d := int64(c.Ticks - e.baseTicks)
	return float64(d) * 1e9 / c.ClockCls.NominalHz()
}

// addSample adds one comparison to the pair window and judges the window
// when it is complete.
func (g *Gate) addSample(st model.StationIdx, e *entry, ref model.StationIdx, cq, cr *ingest.Copy, now int64, y float64) {
	s := e.ref
	if s == nil {
		return // proven by an earlier comparison of this group
	}
	g.c.samples.Add(1)
	s.samples++
	if s.first == 0 {
		s.first = now
		s.stamped = wallNow()
	}
	w := s.pairs[ref]
	if w == nil || w.epochQ != cq.Epoch || w.epochR != cr.Epoch {
		thr := math.Max(g.cfg.MaxSigmaNs, 3*math.Hypot(cq.ClockCls.PriorJitterNs(), cr.ClockCls.PriorJitterNs()))
		w = &pairWin{epochQ: cq.Epoch, epochR: cr.Epoch, t0: now, thr: thr}
		s.pairs[ref] = w
	}
	w.x = append(w.x, float64(now-w.t0)/1e9)
	w.y = append(w.y, y)
	full := len(w.x) >= g.cfg.MaxWindowSamples || now-w.t0 >= int64(g.cfg.Window)
	if !full {
		return
	}
	if len(w.x) >= g.cfg.MinWindowSamples {
		sig, good := judgeWindow(w.x, w.y, w.thr)
		g.c.windows.Add(1)
		s.lastSig = sig
		rs := s.perRef[ref]
		if rs == nil {
			rs = &refStat{}
			s.perRef[ref] = rs
		}
		er := g.ent[ref]
		rs.trusted = er != nil && er.info.State == Trusted
		if good {
			s.good++
			rs.good++
		} else {
			s.bad++
			rs.bad++
		}
	}
	w.x, w.y, w.t0 = w.x[:0], w.y[:0], now
	rep := g.report(s, now)
	if rep.Proven && s.proven == 0 {
		s.proven = now
		g.c.promoted.Add(1)
		e.provenAt = now
		e.info.State = Trusted
		e.ref = nil
		g.setState(st, Trusted)
		g.dropReport(st)
		if g.promote != nil {
			g.promote(st, rep)
		}
		return
	}
	g.setReport(st, rep)
}

// report summarises an evalState.
func (g *Gate) report(s *evalState, now int64) Report {
	goodRefs, trustedJudged, trustedBad := 0, 0, 0
	for _, rs := range s.perRef {
		if rs.good > rs.bad {
			goodRefs++
		}
		if rs.trusted && rs.good+rs.bad >= 2 {
			trustedJudged++
			if rs.bad >= rs.good {
				trustedBad++
			}
		}
	}
	r := Report{State: Quarantined.String(), Since: s.stamped, Samples: s.samples, Windows: s.good + s.bad,
		Good: s.good, Bad: s.bad, Refs: goodRefs, LastSigmaNs: math.Round(s.lastSig), NoTrustedRef: g.isolated(s, now)}
	switch {
	case trustedJudged > 0 && float64(trustedBad) > g.cfg.MaxBadFrac*float64(trustedJudged):
		r.Reason = "timing inconsistent with the network"
	case s.good < g.cfg.GoodWindows && r.NoTrustedRef:
		r.Reason = "no trusted reference in range (compared with nearby new stations)"
	case s.good < g.cfg.GoodWindows:
		r.Reason = "collecting comparisons"
	case goodRefs < g.cfg.MinRefs:
		r.Reason = "too few reference stations"
	case now-s.first < int64(g.cfg.MinAge):
		r.Reason = "observation time too short"
	default:
		r.Proven = true
		r.State = Trusted.String()
	}
	return r
}

// judgeWindow fits a line to (x, y) — the offset and rate difference of
// the two clocks — robustly (Theil–Sen: the median of the pairwise slopes,
// then the median intercept; simulated and real receivers produce a few
// percent of gross timing errors, which a least-squares fit would spread
// over every residual) and returns the robust residual σ (1.4826 × MAD)
// and whether the window is good: σ ≤ thr, and at most 40 % of the
// residuals beyond 5·thr.
func judgeWindow(x, y []float64, thr float64) (float64, bool) {
	n := len(x)
	slopes := make([]float64, 0, n*(n-1)/2)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if dx := x[j] - x[i]; math.Abs(dx) > 1e-6 {
				slopes = append(slopes, (y[j]-y[i])/dx)
			}
		}
	}
	b := 0.0
	if len(slopes) > 0 {
		b = median(slopes)
	}
	res := make([]float64, n)
	for i := range x {
		res[i] = y[i] - b*x[i]
	}
	a := median(slices.Clone(res))
	for i := range res {
		res[i] -= a
	}
	keep := make([]bool, n)
	for i := range keep {
		keep[i] = true
	}
	sig := 1.4826 * mad(res, keep)
	gross := 0
	for i := range res {
		if math.Abs(res[i]) > 5*thr {
			gross++
		}
	}
	return sig, sig <= thr && float64(gross) <= 0.4*float64(n)
}

// median sorts v and returns its median.
func median(v []float64) float64 {
	slices.Sort(v)
	return v[len(v)/2]
}

// mad is the median absolute deviation of the kept residuals.
func mad(r []float64, keep []bool) float64 {
	v := make([]float64, 0, len(r))
	for i := range r {
		if keep[i] {
			v = append(v, r[i])
		}
	}
	if len(v) == 0 {
		return math.Inf(1)
	}
	slices.Sort(v)
	med := v[len(v)/2]
	for i := range v {
		v[i] = math.Abs(v[i] - med)
	}
	slices.Sort(v)
	return v[len(v)/2]
}
