package group

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/sim/simnet"
)

// ------------------------------------------------------------ fakes

// tickClock decodes synthetic ticks: ticks are 1/1024 ns since base. Clock
// and jitter variances are per station (default 0 / 1).
type tickClock struct {
	base   int64
	cv, jv map[model.StationIdx]float64
	off    map[model.StationIdx]bool // stations reported unsynced
}

const tickScale = 1024

func (c *tickClock) ticks(t Time) uint64 {
	d := t.Sub(Time{Ns: c.base})
	return uint64(math.Round(d * tickScale))
}

func (c *tickClock) NetTime(st model.StationIdx, _ uint32, ticks uint64, _ model.FrameKind) (Time, float64, float64, bool) {
	if c.off[st] {
		return Time{}, 0, 0, false
	}
	whole := int64(ticks / tickScale)
	frac := float64(ticks%tickScale) / tickScale
	jv, ok := c.jv[st]
	if !ok {
		jv = 1
	}
	return Time{Ns: c.base + whole, Frac: frac}, c.cv[st], jv, true
}

// siteMap is a Sites from a map.
type siteMap map[model.StationIdx]geo.Vec3

func (s siteMap) Site(st model.StationIdx) (geo.Vec3, float64, bool) {
	p, ok := s[st]
	return p, 0, ok
}

// fixedHint is a Hints returning a fixed position per key.
type fixedHint struct {
	pos   map[uint32]geo.Vec3
	sigma float64
	altFt float64
}

func (h *fixedHint) Predict(key uint32, _ Time) (geo.Vec3, float64, bool) {
	p, ok := h.pos[key]
	return p, h.sigma, ok
}

func (h *fixedHint) AltitudeFt(uint32, Time) (float64, bool) {
	return h.altFt, h.altFt != 0
}

// collect is a Sink keeping deep copies.
type collect struct{ cl []*Cluster }

func (c *collect) Cluster(cl *Cluster) { c.cl = append(c.cl, cl.Clone()) }

func v3(p simnet.Vec3) geo.Vec3 { return geo.Vec3{X: p[0], Y: p[1], Z: p[2]} }

// ------------------------------------------------------------ geometry from simnet

// world is the station and aircraft geometry of a small simnet run.
type world struct {
	n     *simnet.Network
	sites siteMap
	st    []model.StationIdx
	ac    []simnet.AircraftTruth
}

func newWorld(t testing.TB, seed uint64, rx, aircraft int, radiusKm float64) *world {
	t.Helper()
	specs := simnet.RandomReceivers(seed, rx, simnet.Placement{CenterLat: 52, CenterLon: 0, RadiusKm: radiusKm})
	n, err := simnet.New(simnet.Config{Seed: seed, Aircraft: aircraft, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Advance(context.Background(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	w := &world{n: n, sites: siteMap{}}
	for _, r := range n.Receivers() {
		st := model.StationIdx(r.Index + 2)
		w.sites[st] = v3(r.Pos)
		w.st = append(w.st, st)
	}
	for _, a := range n.Aircraft() {
		if a.HAE > 500 {
			w.ac = append(w.ac, a)
		}
	}
	if len(w.ac) == 0 {
		t.Fatal("no airborne aircraft")
	}
	return w
}

// visible reports whether station st can plausibly hear an aircraft at p
// (radio line of sight and within 450 km).
func (w *world) visible(st model.StationIdx, p geo.Vec3) bool {
	r := w.sites[st]
	return r.Dist(p) < 450e3 && geo.LineOfSight(r, p, 0)
}

// tx is one synthetic transmission.
type tx struct {
	id  int
	pos geo.Vec3
	at  Time // emission instant
}

// toa returns the true arrival time of tx at st plus Gaussian noise of sigma ns.
func toa(w *world, x tx, st model.StationIdx, sigmaNs float64, rng *rand.Rand) Time {
	return x.at.Add(w.sites[st].Dist(x.pos)/mPerNs + rng.NormFloat64()*sigmaNs)
}

// copyOf builds an ingest copy whose ticks encode t for tickClock.
func copyOf(c *tickClock, st model.StationIdx, t Time) ingest.Copy {
	return ingest.Copy{Station: st, Channel: 0, ClockCls: model.ClockGPSUTC, Ticks: c.ticks(t), Timed: true, Signal: -20}
}

// df4 is a DF4 payload with an altitude and an address overlay.
func df4(altFt int32) []byte {
	b := make([]byte, 7)
	b[0] = 4 << 3
	code, _ := modes.EncodeAC13(altFt)
	b[2] = byte(code>>8) & 0x1F
	b[3] = byte(code)
	b[4], b[5], b[6] = 0x12, 0x34, 0x56
	return b
}
