package analytics

import (
	"math"
	"sort"
	"sync"

	"airview/internal/geo"
	"airview/internal/latency"
)

// L2TimingCollector gathers T=1 timing tracks for Locate Me v2 from the
// observations of a GPS-stamping receiver (Radarcape, or any feed with
// model.RawFrame.GPSNs). A T=1 airborne position applies to the 0.2 s UTC
// epoch whose parity the CPR format bit gives (DO-260 "time
// synchronisation"; see internal/latency/doc.go): the time of arrival minus
// that epoch is the transmitter's latency plus the propagation time. When
// the latency is stable the propagation part locates the receiver (range
// differences along the track). In practice few transmitters are stable
// enough (internal/latency measures them); Locate2 tests every track
// (l2UsableTrack) and ignores the rest.
//
// Observe is cheap and safe for concurrent use; memory is bounded by
// MaxAircraft × MaxPoints.
type L2TimingCollector struct {
	MaxAircraft int // 0 = 2000
	MaxPoints   int // per aircraft, 0 = 120

	mu  sync.Mutex
	acs map[uint32]*l2TimingAC
}

type l2TimingAC struct {
	pts   []L2TimingPoint
	lastT int64 // GPS ns of the last point
	seen  int64 // ordering for eviction
}

const l2EpochNs = 200_000_000

// Observe takes one tracker observation (latency.Obs as built for the
// latency analyser).
func (c *L2TimingCollector) Observe(o *latency.Obs) {
	if o == nil || o.MLAT || !o.IsPosition || !o.TFlag || o.GPSNs < 0 {
		return
	}
	if !geo.ValidLatLon(o.Lat, o.Lon) || !isNum(o.AltFt) {
		return
	}
	// the epoch with the parity of the F bit nearest to the arrival
	k := int64(math.Round(float64(o.GPSNs) / l2EpochNs))
	want := int64(0)
	if o.CPROdd {
		want = 1
	}
	if k&1 != want {
		if float64(o.GPSNs) >= float64(k)*l2EpochNs {
			k++
		} else {
			k--
		}
	}
	m := float64(o.GPSNs - k*l2EpochNs)
	c.mu.Lock()
	defer c.mu.Unlock()
	maxAC, maxPts := c.MaxAircraft, c.MaxPoints
	if maxAC <= 0 {
		maxAC = 2000
	}
	if maxPts <= 0 {
		maxPts = 120
	}
	if c.acs == nil {
		c.acs = map[uint32]*l2TimingAC{}
	}
	ac := c.acs[o.ICAO]
	if ac == nil {
		if len(c.acs) >= maxAC {
			c.evict()
		}
		ac = &l2TimingAC{}
		c.acs[o.ICAO] = ac
	}
	// one point per 2 s per aircraft is plenty; a day wrap starts afresh
	if ac.lastT != 0 && o.GPSNs > ac.lastT && o.GPSNs-ac.lastT < 2e9 {
		return
	}
	if o.GPSNs < ac.lastT {
		ac.pts = ac.pts[:0]
	}
	ac.lastT = o.GPSNs
	ac.seen = o.GPSNs
	if len(ac.pts) >= maxPts {
		copy(ac.pts, ac.pts[1:])
		ac.pts = ac.pts[:len(ac.pts)-1]
	}
	ac.pts = append(ac.pts, L2TimingPoint{Lat: o.Lat, Lon: o.Lon, AltM: geo.FeetToMetres(o.AltFt), MNs: m})
}

// evict drops the least recently updated half of the aircraft. Caller holds mu.
func (c *L2TimingCollector) evict() {
	type kv struct {
		k uint32
		t int64
	}
	all := make([]kv, 0, len(c.acs))
	for k, v := range c.acs {
		all = append(all, kv{k, v.seen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t < all[j].t })
	for _, e := range all[:len(all)/2] {
		delete(c.acs, e.k)
	}
}

// Tracks returns a copy of the tracks with at least 20 points.
func (c *L2TimingCollector) Tracks() []L2TimingTrack {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []L2TimingTrack
	for k, v := range c.acs {
		if len(v.pts) >= 20 {
			out = append(out, L2TimingTrack{ICAO: k, Points: append([]L2TimingPoint(nil), v.pts...)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ICAO < out[j].ICAO })
	return out
}
