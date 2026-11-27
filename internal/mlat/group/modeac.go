package group

import (
	"cmp"
	"errors"
	"math"
	"math/bits"
	"slices"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Mode A/C association (DESIGN §9.9, phase 5, off by default). Mode A/C
// replies carry no address and no parity: the only keys are the 12-bit code
// and the time. Following the association-free approach (Frisch & Hanebeck,
// "Association-Free Multilateration Based on Times of Arrival"; written here
// from the idea, no code consulted), replies with the same code whose
// pairwise arrival times are consistent with one emitter are grouped:
//
//	|t_k − t_i| ≤ d_ik/c + ModeACGateNs + n·sqrt(v_i + v_k)
//
// Only stations within ModeACMaxBaselineM of each other (150 km) take part,
// and by default only time anchors (GPS classes after probation), because the
// replies are short (20.3 µs), FRUIT-heavy and garbled often, so only the
// best clocks give clusters worth solving. Codes shared by many aircraft
// (7000, 2000, 1200) produce chance coincidences; the maximum-clique rule and
// ModeACMinStations (4: a 3D fix without an altitude prior) keep them rare,
// and the solver's RAIM and the tracker's continuity remove the rest.

// ACObs is one Mode A/C reply heard by a station, already in network time.
type ACObs struct {
	Station model.StationIdx
	// Code is the 13-bit reply code as received (bits C1 A1 C2 A2 C4 A4 X B1
	// D1 B2 D2 B4 D4); Mode A and Mode C replies are not distinguished.
	Code uint16
	T    Time
	// Var is the time variance (ns²): clock plus jitter; JVar is its
	// per-reply jitter part (0 = unknown), which the tracker treats as
	// independent between replies (the rest is correlated clock error).
	Var  float64
	JVar float64
}

// ACMember is one station of a Mode A/C cluster.
type ACMember struct {
	Station   model.StationIdx
	T         Time
	Var       float64
	JVar      float64
	Pos       geo.Vec3
	PosSigmaM float64
}

// ACCluster is a set of Mode A/C replies consistent with one emitter.
type ACCluster struct {
	Code    uint16
	Members []ACMember
	// MaxRatio is the largest |Δt| / gate over member pairs.
	MaxRatio float64
}

// ModeAC clusters Mode A/C replies. It is not safe for concurrent use.
type ModeAC struct {
	cfg    Config
	sites  Sites
	anchor func(model.StationIdx) bool

	obs  []acRef
	adj  [MaxMembers]uint64
	rat  [MaxMembers][MaxMembers]float32
	srch cliqueSearch
	out  [MaxMembers]ACMember
	cl   ACCluster
}

type acRef struct {
	o   ACObs
	pos geo.Vec3
	ps  float64
}

// NewModeAC returns a Mode A/C associator. anchor reports whether a station
// may take part (nil: every located station; production wiring passes the
// clock filter's anchor test).
func NewModeAC(cfg Config, sites Sites, anchor func(model.StationIdx) bool) (*ModeAC, error) {
	if sites == nil {
		return nil, errors.New("group: Sites is required")
	}
	m := &ModeAC{cfg: cfg.Normalize(), sites: sites, anchor: anchor}
	m.srch.adj, m.srch.ratio = &m.adj, &m.rat
	return m, nil
}

// Cluster associates the replies in obs (any order; the slice is not
// modified) and calls emit for every cluster of at least ModeACMinStations
// stations. The *ACCluster is valid only during the call. It returns the
// number of clusters.
func (m *ModeAC) Cluster(obs []ACObs, emit func(*ACCluster)) int {
	return m.ClusterMin(obs, m.cfg.ModeACMinStations, emit)
}

// ClusterMin is Cluster with an explicit smallest cluster size minSt
// (clamped to 2..ModeACMinStations). The engine asks for clusters down to
// two stations: those of 2-3 stations cannot be solved on their own but
// update an established Mode A/C track through its time differences
// (mlat/track ACTracker.Cluster).
func (m *ModeAC) ClusterMin(obs []ACObs, minSt int, emit func(*ACCluster)) int {
	minSt = min(max(minSt, 2), m.cfg.ModeACMinStations)
	m.obs = m.obs[:0]
	maxVar := 0.0
	for i := range obs {
		o := &obs[i]
		if m.anchor != nil && !m.anchor(o.Station) {
			continue
		}
		pos, ps, ok := m.sites.Site(o.Station)
		if !ok {
			continue
		}
		m.obs = append(m.obs, acRef{o: *o, pos: pos, ps: ps})
		maxVar = max(maxVar, o.Var)
	}
	slices.SortFunc(m.obs, func(a, b acRef) int {
		if c := cmp.Compare(a.o.Code&0x1FBF, b.o.Code&0x1FBF); c != 0 {
			return c
		}
		return cmp.Compare(a.o.T.Sub(b.o.T), 0)
	})
	// Upper bound of any pair's gate: no pair across a larger gap can match.
	maxGate := (m.cfg.ModeACMaxBaselineM+m.cfg.SlackM)/mPerNs + m.cfg.ModeACGateNs + m.cfg.NSigma*math.Sqrt(2*maxVar)
	count := 0
	for i := 0; i < len(m.obs); {
		j := i + 1
		for j < len(m.obs) && j-i < MaxMembers && m.obs[j].o.Code&0x1FBF == m.obs[i].o.Code&0x1FBF &&
			m.obs[j].o.T.Sub(m.obs[j-1].o.T) <= maxGate {
			j++
		}
		if j-i >= minSt {
			count += m.segment(m.obs[i:j], minSt, emit)
		}
		i = j
	}
	return count
}

func (m *ModeAC) segment(s []acRef, minSt int, emit func(*ACCluster)) int {
	n := len(s)
	ns := m.cfg.NSigma
	for i := 0; i < n; i++ {
		m.adj[i] = 0
	}
	for i := 0; i < n; i++ {
		for k := i + 1; k < n; k++ {
			m.rat[i][k], m.rat[k][i] = math.MaxFloat32, math.MaxFloat32
			if s[i].o.Station == s[k].o.Station {
				continue
			}
			d := s[i].pos.Dist(s[k].pos)
			if d > m.cfg.ModeACMaxBaselineM {
				continue
			}
			gate := (d+3*(s[i].ps+s[k].ps))/mPerNs + m.cfg.ModeACGateNs + ns*math.Sqrt(max(s[i].o.Var+s[k].o.Var, 1e-6))
			r := math.Abs(s[k].o.T.Sub(s[i].o.T)) / gate
			m.rat[i][k], m.rat[k][i] = float32(r), float32(r)
			if r <= 1 {
				m.adj[i] |= 1 << k
				m.adj[k] |= 1 << i
			}
		}
	}
	rem := uint64(1)<<n - 1
	if n == 64 {
		rem = ^uint64(0)
	}
	count := 0
	for bits.OnesCount64(rem) >= minSt {
		c := m.srch.run(rem, m.cfg.SearchBudget)
		if bits.OnesCount64(c) < minSt {
			break
		}
		rem &^= c
		k := 0
		var mr float64
		for a := c; a != 0; a &= a - 1 {
			i := bits.TrailingZeros64(a)
			for b := a & (a - 1); b != 0; b &= b - 1 {
				mr = max(mr, float64(m.rat[i][bits.TrailingZeros64(b)]))
			}
			m.out[k] = ACMember{Station: s[i].o.Station, T: s[i].o.T, Var: s[i].o.Var, JVar: s[i].o.JVar, Pos: s[i].pos, PosSigmaM: s[i].ps}
			k++
		}
		m.cl = ACCluster{Code: s[bits.TrailingZeros64(c)].o.Code, Members: m.out[:k], MaxRatio: mr}
		emit(&m.cl)
		count++
	}
	return count
}
