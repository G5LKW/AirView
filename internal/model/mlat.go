package model

import "math"

// MLAT result contract (docs/aggregator/DESIGN.md §9, AVF MLAT_RESULTS).

// MLAT position sources for DecodedUpdate.Src.
const (
	SrcMLATAV  = "mlat-av"  // AirView's own MLAT engine
	SrcMLATExt = "mlat-ext" // an operator's external mlat-server via the SBS-in bridge
)

// MLATFix is one multilateration result. It carries no station identities.
type MLATFix struct {
	ICAO    uint32 // 24-bit address
	TUnixNs int64  // transmit instant, Unix ns (network time; UTC when anchored)
	// ECEF is the WGS-84 earth-centred earth-fixed position in metres.
	ECEF [3]float64
	// Cov is the upper triangle of the 3x3 ECEF position covariance in m²,
	// in the order xx, xy, xz, yy, yz, zz.
	Cov [6]float32
	// Vel is the ENU velocity in m/s (east, north, up) when the tracker has
	// one, else nil.
	Vel *[3]float32
	// NBucket is the contributing station count bucket (NBucket* constants);
	// the exact count is never published.
	NBucket uint8
	// Grade is the quality grade (Grade* constants).
	Grade uint8
	// Flags is a bit set of FixFlag* values.
	Flags uint8
}

// Station count buckets of MLATFix.NBucket.
const (
	NBucket3     uint8 = 0 // 3 stations (+ altitude)
	NBucket4     uint8 = 1 // 4 stations
	NBucket5to8  uint8 = 2 // 5-8 stations
	NBucket9Plus uint8 = 3 // 9 or more stations
)

// NBucketFor returns the bucket of a station count (counts below 3 map to
// NBucket3).
func NBucketFor(n int) uint8 {
	switch {
	case n <= 3:
		return NBucket3
	case n == 4:
		return NBucket4
	case n <= 8:
		return NBucket5to8
	}
	return NBucket9Plus
}

// Fix quality grades (DESIGN §9.4). GradeNone is "not graded".
const (
	GradeNone uint8 = 0
	GradeA    uint8 = 1 // sigma_h <= 50 m and DOF >= 2
	GradeB    uint8 = 2 // sigma_h <= 150 m
	GradeC    uint8 = 3 // sigma_h <= 500 m
	GradeD    uint8 = 4 // sigma_h <= 2 km: track maintenance only, never displayed
)

// GradeFor returns the grade for a horizontal 1-sigma in metres and the
// solution's degrees of freedom. Fixes above 2 km get GradeNone (rejected).
func GradeFor(sigmaHM float64, dof int) uint8 {
	switch {
	case math.IsNaN(sigmaHM) || sigmaHM < 0 || sigmaHM > 2000:
		return GradeNone
	case sigmaHM <= 50 && dof >= 2:
		return GradeA
	case sigmaHM <= 150:
		return GradeB
	case sigmaHM <= 500:
		return GradeC
	}
	return GradeD
}

// GradeString returns "A".."D", or "" for GradeNone / unknown.
func GradeString(g uint8) string {
	if g >= GradeA && g <= GradeD {
		return string(rune('A' + g - 1))
	}
	return ""
}

// Displayable reports whether a fix of grade g may be shown on a map.
func Displayable(g uint8) bool { return g >= GradeA && g <= GradeC }

// MLATFix flags.
const (
	FixFlagAltConstrained uint8 = 1 << 0 // altitude was used as a measurement row (3+alt)
	FixFlagTracked        uint8 = 1 << 1 // output of the tracker (filtered), not a single snapshot fix
	FixFlagRelativeTime   uint8 = 1 << 2 // solved in a component without GPS anchors
	FixFlagHoldout        uint8 = 1 << 3 // hold-out aircraft (also has ADS-B; validation only)
	FixFlagAmbiguous      uint8 = 1 << 4 // a second 3+alt hypothesis is still open
)
