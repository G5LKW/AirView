package quality

import (
	"math"
	"sort"
	"sync"

	"airview/internal/model"
)

// Altitude bands of the error map (DESIGN §9.6 "per 1° cell × altitude
// band").
const (
	BandLow  uint8 = 0 // below 10,000 ft
	BandMid  uint8 = 1 // 10,000-25,000 ft
	BandHigh uint8 = 2 // above 25,000 ft
)

// BandOf returns the band of a height in metres.
func BandOf(altM float64) uint8 {
	ft := altM / 0.3048
	switch {
	case ft < 10000:
		return BandLow
	case ft < 25000:
		return BandMid
	}
	return BandHigh
}

type cellKey struct {
	lat, lon int16 // floor of the degree
	band     uint8
}

// maxCellStations bounds the distinct-station set kept per cell; the count
// saturates there (enough for the public threshold of 5).
const maxCellStations = 64

type cell struct {
	h      Hist
	n      uint64
	st     []model.StationIdx
	lastNs int64
}

// Cells is the error map: per 1° × 1° cell and altitude band, the
// hold-out error distribution and the number of distinct stations that
// contributed to those fixes. Safe for concurrent use.
type Cells struct {
	mu sync.Mutex
	m  map[cellKey]*cell
}

// NewCells returns an empty map.
func NewCells() *Cells { return &Cells{m: map[cellKey]*cell{}} }

// Add records a sample and the stations that contributed to its fix.
func (c *Cells) Add(s *Sample, stations []model.StationIdx) {
	if math.IsNaN(s.Lat) || math.IsNaN(s.Lon) {
		return
	}
	k := cellKey{lat: int16(math.Floor(s.Lat)), lon: int16(math.Floor(s.Lon)), band: BandOf(s.AltM)}
	c.mu.Lock()
	ce := c.m[k]
	if ce == nil {
		ce = &cell{}
		c.m[k] = ce
	}
	ce.h.Add(s.ErrH())
	ce.n++
	ce.lastNs = max(ce.lastNs, s.TNs)
	for _, st := range stations {
		if len(ce.st) >= maxCellStations {
			break
		}
		found := false
		for _, x := range ce.st {
			if x == st {
				found = true
				break
			}
		}
		if !found {
			ce.st = append(ce.st, st)
		}
	}
	c.mu.Unlock()
}

// Cell is one entry of the error map. It carries no station identities.
type Cell struct {
	Lat      int     `json:"lat"` // south-west corner, degrees
	Lon      int     `json:"lon"`
	Band     uint8   `json:"band"`
	N        uint64  `json:"n"`
	P50      float64 `json:"p50M"`
	P95      float64 `json:"p95M"`
	Stations int     `json:"stations"` // distinct contributing stations (saturates at 64)
}

// List returns the cells with at least minN samples and minStations
// distinct contributing stations (the public map uses minStations = 5,
// DESIGN §8.2), sorted by position.
func (c *Cells) List(minN uint64, minStations int) []Cell {
	c.mu.Lock()
	out := make([]Cell, 0, len(c.m))
	for k, ce := range c.m {
		if ce.n < minN || len(ce.st) < minStations {
			continue
		}
		out = append(out, Cell{Lat: int(k.lat), Lon: int(k.lon), Band: k.band, N: ce.n,
			P50: ce.h.Quantile(0.5), P95: ce.h.Quantile(0.95), Stations: len(ce.st)})
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Lat != b.Lat {
			return a.Lat < b.Lat
		}
		if a.Lon != b.Lon {
			return a.Lon < b.Lon
		}
		return a.Band < b.Band
	})
	return out
}

// Prune drops cells without a sample since beforeNs and returns how many.
func (c *Cells) Prune(beforeNs int64) int {
	n := 0
	c.mu.Lock()
	for k, ce := range c.m {
		if ce.lastNs < beforeNs {
			delete(c.m, k)
			n++
		}
	}
	c.mu.Unlock()
	return n
}
