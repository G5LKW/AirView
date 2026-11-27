package survey

import (
	"sync"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Obs is one survey observation: a long Mode S frame of an ADS-B reference
// aircraft, heard by the survey target (its raw receiver ticks) and by
// MinPeers or more synced, located peers, which fix the emission time.
//
// The pseudorange of DESIGN §10.2 follows from it:
//
//	ρ = c·(s_U − τ) = |p − u| + c·θ_U(τ)
//
// with s_U = Ticks / NominalHz (the target's local time), τ = TauNs (the
// emission time in the peers' network time), p = P (the aircraft at τ)
// and θ_U the target clock's offset (piecewise linear, DESIGN §10.2 clock
// knots). Obs holds no identity of the peers.
type Obs struct {
	// TauNs is the emission time (network ns of the peers' time domain:
	// UTC when Anchored).
	TauNs int64
	// Ticks and Epoch are the target's receiver timestamp; Class its clock
	// class (Ticks are in units of Class.NominalHz()).
	Ticks uint64
	Epoch uint32
	Class model.ClockClass
	// Domain is the peers' time domain; Anchored: it has GPS anchors.
	Domain   uint32
	Anchored bool
	// Peers is the number of peers that fixed τ.
	Peers uint8
	// Key is the aircraft (tracker key).
	Key uint32
	// P is the aircraft's ECEF position at τ; CovP its covariance (upper
	// triangle xx, xy, xz, yy, yz, zz; m²).
	P    geo.Vec3
	CovP [6]float64
	// Ebar is the weighted mean unit vector from the peers to the aircraft:
	// an aircraft position error δp moves the pseudorange by Ebar·δp
	// (through τ) and the model by e_U·δp, so (Ebar − e_U)·δp is the error
	// it leaves (with Ebar ≈ e_U for nearby peers it cancels).
	Ebar geo.Vec3
	// VarTauM2 is the variance of c·τ from the peers' clocks and jitter
	// (m²).
	VarTauM2 float64
}

// quad returns (Ebar − e)ᵀ·CovP·(Ebar − e).
func (o *Obs) quad(e geo.Vec3) float64 {
	d := [3]float64{o.Ebar.X - e.X, o.Ebar.Y - e.Y, o.Ebar.Z - e.Z}
	c := &o.CovP
	return c[0]*d[0]*d[0] + c[3]*d[1]*d[1] + c[5]*d[2]*d[2] +
		2*(c[1]*d[0]*d[1]+c[2]*d[0]*d[2]+c[4]*d[1]*d[2])
}

// collector accumulates a job's observations with per-aircraft thinning
// and a memory bound. Safe for concurrent use.
type collector struct {
	mu       sync.Mutex
	obs      []Obs
	last     map[uint32]int64 // aircraft → τ of its last accepted observation
	interval int64            // ns
	max      int
	// peer centroid (ECEF sum and count) for the unlocated seed
	peerSum geo.Vec3
	peerN   int
	// peer heights for the height prior of unlocated stations
	peerH    []float64
	firstNs  int64
	lastNs   int64
	offered  uint64
	accepted uint64
	dropped  uint64 // dropped by decimation
}

func newCollector(interval int64, max int) *collector {
	return &collector{last: map[uint32]int64{}, interval: interval, max: max}
}

// wants reports cheaply whether an observation of aircraft key at tNs
// would be accepted (pre-check before the expensive emission time).
func (c *collector) wants(key uint32, tNs int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.last[key]
	return !ok || tNs-l >= c.interval || tNs < l
}

// add stores o unless thinning rejects it; peers are the ECEF positions of
// the peers that fixed its τ.
func (c *collector) add(o *Obs, peers []geo.Vec3) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offered++
	if l, ok := c.last[o.Key]; ok && o.TauNs-l < c.interval && o.TauNs >= l {
		return false
	}
	c.last[o.Key] = o.TauNs
	if len(c.obs) >= c.max {
		// decimate: keep every second observation, double the interval
		k := 0
		for i := 0; i < len(c.obs); i += 2 {
			c.obs[k] = c.obs[i]
			k++
		}
		c.dropped += uint64(len(c.obs) - k)
		c.obs = c.obs[:k]
		c.interval *= 2
	}
	c.obs = append(c.obs, *o)
	c.accepted++
	if c.firstNs == 0 || o.TauNs < c.firstNs {
		c.firstNs = o.TauNs
	}
	c.lastNs = max(c.lastNs, o.TauNs)
	for _, p := range peers {
		if c.peerN < 1<<20 {
			c.peerSum = c.peerSum.Add(p)
			c.peerN++
		}
		if len(c.peerH) < 4096 {
			c.peerH = append(c.peerH, geo.FromECEF(p).H)
		}
	}
	if len(c.last) > 4*c.max {
		// forget aircraft not seen for a while (bounded map)
		for k, t := range c.last {
			if o.TauNs-t > 60*c.interval {
				delete(c.last, k)
			}
		}
	}
	return true
}

// snapshot returns a copy of the observations and the collection stats.
func (c *collector) snapshot() (obs []Obs, peerCentroid geo.Vec3, peerOK bool, peerH []float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obs = append([]Obs(nil), c.obs...)
	if c.peerN > 0 {
		peerCentroid, peerOK = c.peerSum.Scale(1/float64(c.peerN)), true
	}
	return obs, peerCentroid, peerOK, append([]float64(nil), c.peerH...)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.obs)
}
