package simnet

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"time"
)

// DefaultStart is the virtual start time used when Config.Start is zero.
// A fixed instant keeps runs reproducible.
var DefaultStart = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

// Limits.
const (
	MaxReceivers = 5000
	// MaxAircraft is the aircraft limit of the underlying internal/sim world
	// (20,000: one network of 1,000+ feeders and 10k+ aircraft, DESIGN
	// §20.2; finding SCALE-11).
	MaxAircraft = 20000
)

// Config configures a simulated network.
type Config struct {
	// Seed makes the run reproducible: the same Seed and Config produce the
	// identical observation sequence, independent of Workers. 0 = 1.
	Seed uint64
	// Start is the virtual UTC start time (zero = DefaultStart).
	Start time.Time

	// Aircraft is the number of simulated aircraft (default 300, at most
	// MaxAircraft). Diurnal enables the internal/sim traffic curve; by
	// default the density is constant.
	Aircraft int
	Diurnal  bool
	// CenterLat / CenterLon is the centre of the simulated world (zero =
	// centroid of the receivers). RangeKm is its radius: traffic enters and
	// leaves there (zero = the farthest receiver + 350 km, 200..1500).
	CenterLat, CenterLon float64
	RangeKm              float64
	// ModeSOnlyFraction, MilitaryFraction and GNSSAltFraction are passed to
	// internal/sim (its MLATFraction, MilitaryFraction, GNSSAltFraction):
	// 0 = sim default, negative = none.
	ModeSOnlyFraction float64
	MilitaryFraction  float64
	GNSSAltFraction   float64

	// PosLatencyMaxMs: every ADS-B airframe gets a fixed position latency
	// drawn uniformly from 0..PosLatencyMaxMs; its airborne position
	// squitters are re-encoded with the position that old (0 = 300 ms,
	// negative = none).
	PosLatencyMaxMs float64
	// PosErrorScale scales the NACp-consistent position error added to
	// re-encoded positions (0 = 1, negative = none).
	PosErrorScale float64

	// Receivers are the stations of the network (1..MaxReceivers).
	Receivers []ReceiverSpec
	// Atmosphere is the refraction model (zero fields = standard values).
	Atmosphere Atmosphere
	// GeoidUndulationM is the geoid height above the ellipsoid used to turn
	// declared MSL altitudes into HAE and back (0 = 47 m, Central Europe).
	GeoidUndulationM float64

	// HoldbackS is how long observations are held back so that every
	// receiver's output is in arrival order (0 = 0.3 s; at least 0.2 s).
	HoldbackS float64
	// Workers is the number of goroutines that evaluate receivers (0 =
	// GOMAXPROCS). It does not change the results.
	Workers int
}

// ReceiverSpec describes one receiving station.
type ReceiverSpec struct {
	// ID is a unique station name (default "rx<index>"). UUID is the
	// feeder UUID sent by beast_reduce_plus and mlat-client (default:
	// derived from Seed and index). Name is the display name. RxNo is the
	// AVF rx_no (default 1).
	ID, UUID, Name string
	RxNo           int

	// True position: WGS-84 latitude/longitude, terrain height above the
	// ellipsoid and antenna height above the terrain.
	Lat, Lon   float64
	GroundHAE  float64
	AntennaAGL float64

	// Declared position (what the owner entered): with DeclaredSet the
	// explicit values, otherwise the true position moved DeclaredErrorM
	// metres in a random direction and DeclaredAltErrM up/down.
	// DeclaredAltRef is "hae" or "msl" (default "msl"; MSL = HAE -
	// Config.GeoidUndulationM).
	DeclaredSet              bool
	DeclaredLat, DeclaredLon float64
	DeclaredAlt              float64
	DeclaredErrorM           float64
	DeclaredAltErrM          float64
	DeclaredAltRef           string
	declaredLat, declaredLon float64
	declaredAlt              float64

	Clock     ClockSpec
	Reception ReceptionSpec
	// Link is the network path from this feeder to the aggregator (used by
	// emitters wrapped in a Link).
	Link LinkSpec
}

func (c *Config) normalize() error {
	if c.Seed == 0 {
		c.Seed = 1
	}
	if c.Start.IsZero() {
		c.Start = DefaultStart
	}
	c.Start = c.Start.UTC()
	if c.Aircraft <= 0 {
		c.Aircraft = 300
	}
	if c.Aircraft > MaxAircraft {
		return fmt.Errorf("simnet: %d aircraft exceed the limit of %d", c.Aircraft, MaxAircraft)
	}
	n := len(c.Receivers)
	if n == 0 {
		return errors.New("simnet: no receivers")
	}
	if n > MaxReceivers {
		return fmt.Errorf("simnet: %d receivers exceed the limit of %d", n, MaxReceivers)
	}
	switch {
	case c.PosLatencyMaxMs == 0:
		c.PosLatencyMaxMs = 300
	case c.PosLatencyMaxMs < 0:
		c.PosLatencyMaxMs = 0
	}
	switch {
	case c.PosErrorScale == 0:
		c.PosErrorScale = 1
	case c.PosErrorScale < 0:
		c.PosErrorScale = 0
	}
	if c.GeoidUndulationM == 0 {
		c.GeoidUndulationM = 47
	}
	if !(c.HoldbackS > 0) {
		c.HoldbackS = 0.3
	}
	c.HoldbackS = math.Max(c.HoldbackS, 0.2)
	if c.Workers <= 0 {
		c.Workers = runtime.GOMAXPROCS(0)
	}
	c.Atmosphere.normalize()

	ids := make(map[string]int, n)
	declR := newRand(c.Seed, streamSpec, 0xDEC)
	for i := range c.Receivers {
		r := &c.Receivers[i]
		if r.ID == "" {
			r.ID = fmt.Sprintf("rx%04d", i)
		}
		if j, dup := ids[r.ID]; dup {
			return fmt.Errorf("simnet: receivers %d and %d share the ID %q", j, i, r.ID)
		}
		ids[r.ID] = i
		if r.UUID == "" {
			r.UUID = makeUUID(c.Seed, uint64(i))
		}
		if r.Name == "" {
			r.Name = r.ID
		}
		if r.RxNo <= 0 {
			r.RxNo = 1
		}
		if math.Abs(r.Lat) > 70 || math.Abs(r.Lon) > 180 || math.IsNaN(r.Lat) || math.IsNaN(r.Lon) {
			return fmt.Errorf("simnet: receiver %q: position %.4f,%.4f out of range (|lat| <= 70)", r.ID, r.Lat, r.Lon)
		}
		if r.AntennaAGL <= 0 {
			r.AntennaAGL = 10
		}
		if err := r.Clock.normalize(); err != nil {
			return fmt.Errorf("simnet: receiver %q: %w", r.ID, err)
		}
		if err := r.Reception.normalize(r.AntennaAGL); err != nil {
			return fmt.Errorf("simnet: receiver %q: %w", r.ID, err)
		}
		r.Link.normalize()
		switch r.DeclaredAltRef {
		case "":
			r.DeclaredAltRef = "msl"
		case "msl", "hae":
		default:
			return fmt.Errorf("simnet: receiver %q: declared altitude reference %q is not hae or msl", r.ID, r.DeclaredAltRef)
		}
		// declared position (drawn for every receiver, so adding an error
		// to one does not change the others)
		brg, u := declR.Float64()*360, declR.NormFloat64()
		hae := r.GroundHAE + r.AntennaAGL
		if r.DeclaredSet {
			r.declaredLat, r.declaredLon, r.declaredAlt = r.DeclaredLat, r.DeclaredLon, r.DeclaredAlt
		} else {
			r.declaredLat, r.declaredLon = r.Lat, r.Lon
			if r.DeclaredErrorM > 0 {
				r.declaredLat, r.declaredLon = destination(r.Lat, r.Lon, brg, r.DeclaredErrorM)
			}
			r.declaredAlt = hae + u*r.DeclaredAltErrM
			if r.DeclaredAltRef == "msl" {
				r.declaredAlt -= c.GeoidUndulationM
			}
		}
	}
	if c.CenterLat == 0 && c.CenterLon == 0 {
		c.CenterLat, c.CenterLon = centroid(c.Receivers)
	}
	if c.RangeKm == 0 {
		far := 0.0
		for i := range c.Receivers {
			far = math.Max(far, groundDistance(c.CenterLat, c.CenterLon, c.Receivers[i].Lat, c.Receivers[i].Lon))
		}
		c.RangeKm = far/1000 + 350
	}
	c.RangeKm = clamp(c.RangeKm, 200, 1500)
	return nil
}

// centroid returns the mean position of the receivers (via ECEF, so it
// works across the antimeridian).
func centroid(rs []ReceiverSpec) (lat, lon float64) {
	var s Vec3
	for i := range rs {
		s = s.Add(ECEF(rs[i].Lat, rs[i].Lon, 0))
	}
	lat, lon, _ = Geodetic(s.Scale(1 / float64(len(rs))))
	return clamp(lat, -70, 70), lon
}

// makeUUID derives a version-4 style UUID string from the seed.
func makeUUID(seed, i uint64) string {
	a, b := subSeed(seed, streamUUID, i), subSeed(seed, streamUUID, i, 1)
	a = a&^(0xF<<12) | 4<<12   // version 4
	b = b&^(0xC<<60) | 0x8<<60 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", a>>32, (a>>16)&0xFFFF, a&0xFFFF, b>>48, b&0xFFFFFFFFFFFF)
}
