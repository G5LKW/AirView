package rfstat

import (
	"container/heap"
	"math"
	"math/rand/v2"
	"time"
)

// Synthetic ADS-B squitter generator for tests: every aircraft transmits
// position (alternating even/odd), velocity and identification squitters
// with uniform jitter; each squitter is decoded with probability pd.

var simBase = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

type simAC struct {
	icao      uint32
	dist, brg float64
	elev, alt float64
	track     float64
	pd        float64 // decode probability
	rssi      float64
	odd       bool
	// interval generator: jitter uniform [lo,hi] unless fixed > 0
	posLo, posHi float64
	fixed        float64
}

type simEv struct {
	t   float64 // seconds since simBase
	ac  int
	typ int // 0 pos, 1 vel, 2 ident
}

type evHeap []simEv

func (h evHeap) Len() int           { return len(h) }
func (h evHeap) Less(i, j int) bool { return h[i].t < h[j].t }
func (h evHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *evHeap) Push(x any)        { *h = append(*h, x.(simEv)) }
func (h *evHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type sim struct {
	rng  *rand.Rand
	acs  []simAC
	h    evHeap
	gps  bool
	lost [3]int
	sent [3]int
}

func newSim(seed uint64, acs []simAC, gps bool) *sim {
	s := &sim{rng: rand.New(rand.NewPCG(seed, seed^0x9e37)), acs: acs, gps: gps}
	for i := range acs {
		if acs[i].posLo == 0 {
			acs[i].posLo, acs[i].posHi = 0.4, 0.6
		}
		heap.Push(&s.h, simEv{t: s.rng.Float64() * 0.6, ac: i, typ: 0})
		heap.Push(&s.h, simEv{t: s.rng.Float64() * 0.6, ac: i, typ: 1})
		heap.Push(&s.h, simEv{t: s.rng.Float64() * 5, ac: i, typ: 2})
	}
	return s
}

func (s *sim) interval(a *simAC, typ int) float64 {
	if typ == 2 {
		return 4.8 + 0.4*s.rng.Float64()
	}
	if a.fixed > 0 {
		return a.fixed
	}
	return a.posLo + (a.posHi-a.posLo)*s.rng.Float64()
}

// run emits frames up to time end (s), calling Tick each simulated second.
func (s *sim) run(st *Stats, end float64) {
	nextTick := 1.0
	for s.h.Len() > 0 && s.h[0].t < end {
		ev := heap.Pop(&s.h).(simEv)
		for ev.t >= nextTick {
			st.Tick(simBase.Add(time.Duration(nextTick * 1e9)))
			nextTick++
		}
		a := &s.acs[ev.ac]
		heap.Push(&s.h, simEv{t: ev.t + s.interval(a, ev.typ), ac: ev.ac, typ: ev.typ})
		kind := KindVelocity
		switch ev.typ {
		case 0:
			kind = KindPosEven
			if a.odd {
				kind = KindPosOdd
			}
			a.odd = !a.odd
		case 2:
			kind = KindIdent
		}
		s.sent[ev.typ]++
		if s.rng.Float64() >= a.pd {
			s.lost[ev.typ]++
			continue
		}
		f := NewFrame(simBase.Add(time.Duration(ev.t * 1e9)))
		if s.gps {
			f.GPSNs = int64(math.Round((ev.t + 36000) * 1e9))
		}
		f.Signal = a.rssi
		f.DF, f.CRC, f.ICAO, f.Kind = 17, CRCOK, a.icao, kind
		f.TC = 11
		f.DistKm, f.BearingDeg, f.ElevDeg, f.AltFt, f.TrackDeg = a.dist, a.brg, a.elev, a.alt, a.track
		f.Version, f.NIC, f.NACp, f.SIL = 2, 8, 9, 3
		f.Feed, f.Antenna = 0, 0
		st.Observe(f)
	}
	for nextTick <= end {
		st.Tick(simBase.Add(time.Duration(nextTick * 1e9)))
		nextTick++
	}
}
