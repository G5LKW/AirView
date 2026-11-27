package group

import (
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// MaxMembers is the most copies one group contributes (= ingest.MaxCopies;
// the clustering works on 64-bit sets).
const MaxMembers = 64

// Use says what a cluster is good for (DESIGN §9.3 "Minimum to solve").
type Use uint8

// Cluster uses, in increasing order of value.
const (
	// UseNone: fewer than two sites; never emitted.
	UseNone Use = iota
	// UseTrack: two or more sites but not enough to solve (or the planner's
	// budget said no): a TDOA update for a confirmed track only.
	UseTrack
	// UseSolveAlt: exactly enough sites to solve with the altitude row
	// (MinSitesAlt, default 3 + altitude).
	UseSolveAlt
	// UseSolve: MinSites (default 4) or more independent sites.
	UseSolve
)

// String returns "none", "track", "solve_alt" or "solve".
func (u Use) String() string {
	switch u {
	case UseTrack:
		return "track"
	case UseSolveAlt:
		return "solve_alt"
	case UseSolve:
		return "solve"
	}
	return "none"
}

// Altitude sources (Cluster.AltSrc).
const (
	AltNone      uint8 = 0 // no altitude
	AltFrameBaro uint8 = 1 // Mode S AC13 / ES AC12 pressure altitude in this frame
	AltFrameGNSS uint8 = 2 // ES TC 20-22 GNSS height (HAE) in this frame
	AltHint      uint8 = 3 // Hints.AltitudeFt (age and accuracy are the hint's)
)

// Member is one station's time of arrival in a cluster.
type Member struct {
	Station model.StationIdx
	Channel int8
	// Site is the index into Cluster.Sites of the site this member belongs to;
	// W its weight in that site's fused time (the weights of a site sum to 1).
	Site uint8
	// Late marks a copy that arrived after the group was flushed.
	Late bool
	// Epoch and Ticks are the receiver timestamp the time came from.
	Epoch uint32
	Ticks uint64
	// T is the network time of arrival at the antenna; ClockVar and JitterVar
	// its clock-state and per-frame variances (ns², see Clock.NetTime).
	T         Time
	ClockVar  float64
	JitterVar float64
	// Pos is the station's antenna (ECEF, m) and PosSigmaM its 1-sigma.
	Pos       geo.Vec3
	PosSigmaM float64
	// Signal is the RSSI in dBFS (NaN = unknown).
	Signal float32
	W      float64
}

// Var returns the member's total timing variance (ns²).
func (m *Member) Var() float64 { return m.ClockVar + m.JitterVar }

// Site is one independent viewpoint: a station, or several co-located
// stations (closer than Config.ColocatedM) fused into one measurement
// (DESIGN §9.3 "Co-located stations"). Members[First:First+Count] are its
// members.
type Site struct {
	// Pos is the weighted centroid of the members' antennas and PosSigmaM its
	// 1-sigma (from the members' position sigmas).
	Pos       geo.Vec3
	PosSigmaM float64
	// T is the weighted mean of the members' times. Because Pos is the
	// centroid with the same weights, T is the arrival time at Pos to first
	// order (package documentation, "Co-located stations").
	T Time
	// Var is the variance of T (ns²): the inverse-variance combination of the
	// members, their clock cross-covariances when known, and a bound on the
	// second-order geometry term. Equal to the member's variance for a
	// single-station site.
	Var float64
	// First and Count index the site's members in Cluster.Members.
	First, Count uint8
}

// Cluster is a set of times of arrival of one transmission at distinct
// stations, consistent with a single emitter (DESIGN §9.3).
type Cluster struct {
	// Seq numbers clusters in emission order (late re-emissions keep the
	// original Seq).
	Seq uint64
	// Hash, Key, DF and Kind identify the transmission (from the ingest
	// group); Data[:DataLen] is the payload.
	Hash    uint64
	Key     uint32
	DF      uint8
	Kind    model.FrameKind
	Data    [14]byte
	DataLen uint8
	// FirstNs is the corrected server time of the group's earliest copy.
	FirstNs int64
	// Members, grouped by site and inside a site by increasing variance.
	Members []Member
	// Sites are the independent viewpoints; Ref is the index of the site with
	// the smallest variance (the natural TDOA reference).
	Sites []Site
	Ref   int
	// AltFt is the altitude (ft) and AltSrc where it came from (Alt* consts).
	AltFt  float64
	AltSrc uint8
	// Use is what the cluster may be used for.
	Use Use
	// Part and Parts: this is cluster Part (0-based) of the Parts clusters
	// taken from one ingest group. Parts > 1 means the group mixed several
	// transmissions of an identical payload (a repeat burst).
	Part, Parts uint8
	// Hinted is set when a position hint tightened the gate; HintConflict
	// when the hint was rejected because it fragmented a cluster the
	// baseline gate keeps whole (the hint may be stale or a wrong branch).
	Hinted       bool
	HintConflict bool
	// MaxRatio is the largest |Δt| / gate over member pairs (<= 1): how close
	// to the gate edge the cluster is.
	MaxRatio float64
	// Late is set when the cluster is re-emitted with copies that arrived
	// after the flush (Member.Late marks them). Its Seq is the original's;
	// consumers that already used the original should treat it as a
	// replacement measurement, not an independent one.
	Late bool
}

// NSites returns the number of independent sites.
func (c *Cluster) NSites() int { return len(c.Sites) }

// Long reports whether the frame is a 112-bit one.
func (c *Cluster) Long() bool { return c.Kind == model.FrameModeSLong }

// Payload returns the frame bytes.
func (c *Cluster) Payload() []byte { return c.Data[:c.DataLen] }

// Clone returns a deep copy of c that may be retained.
func (c *Cluster) Clone() *Cluster {
	d := *c
	d.Members = append([]Member(nil), c.Members...)
	d.Sites = append([]Site(nil), c.Sites...)
	return &d
}
