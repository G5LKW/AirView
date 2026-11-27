package group

import "math"

// Config tunes the assembler. The zero value is not useful; start from
// DefaultConfig. Normalize clamps every field to its documented range.
type Config struct {
	// NSigma is the gate width in standard deviations of the timing noise
	// (DESIGN §9.3: 4). Range 2..8.
	NSigma float64
	// SlackM is the geometric slack added to a station baseline in the
	// baseline gate (DESIGN §9.3: 1 km). It covers refraction-excess
	// differences, multipath and small position errors. Range 0..10000.
	SlackM float64
	// HintSlackM is the geometric slack of the hint gate (refraction-excess
	// differences, multipath, hint model error). Range 0..5000.
	HintSlackM float64
	// HintMaxSigmaM: hints with a larger 1-sigma are ignored (the baseline
	// gate is then at least as tight). Range 0 (hints off)..50000.
	HintMaxSigmaM float64
	// ColocatedM: stations closer than this are one site (DESIGN §9.3:
	// 1 km). Range 0 (never fuse)..5000.
	ColocatedM float64
	// FuseMinRangeM is the smallest aircraft range assumed when bounding the
	// second-order error of a fused site (used without a hint). Range
	// 1000..100000.
	FuseMinRangeM float64
	// MinSites / MinSitesAlt: sites needed to solve without / with an
	// altitude (DESIGN §9.3: 4 and 3). Fewer (but >= 2) go to the tracker
	// only. MinSites range 3..8, MinSitesAlt 3..MinSites.
	MinSites    int
	MinSitesAlt int
	// MaxParts is the most clusters taken from one ingest group (repeat
	// bursts). Range 1..8.
	MaxParts int
	// SearchBudget bounds the clique search per group (search nodes); when
	// it runs out the best clique found so far is used. Range 64..1e6.
	SearchBudget int
	// LateHoldNs is how long emitted clusters are kept for late copies
	// (DESIGN §9.3: up to 2 s). 0 disables late handling. Range 0..5e9.
	LateHoldNs int64
	// LateSlots is the size of the late-copy retention ring; LateMembers the
	// copies kept per retained cluster (the lowest-variance ones). Ranges
	// 256..1<<20 and 4..64.
	LateSlots   int
	LateMembers int
	// ModeACMaxBaselineM and ModeACGateNs: Mode A/C association (DESIGN
	// §9.9): only station pairs within the baseline (150 km), with |Δt| <=
	// d/c + ModeACGateNs (2 µs) + NSigma·σ.
	ModeACMaxBaselineM float64
	ModeACGateNs       float64
	// ModeACMinStations is the smallest Mode A/C cluster emitted. Range 3..8.
	ModeACMinStations int
}

// DefaultConfig returns the DESIGN §9.3 / §9.9 values.
func DefaultConfig() Config {
	return Config{
		NSigma:             4,
		SlackM:             1000,
		HintSlackM:         300,
		HintMaxSigmaM:      5000,
		ColocatedM:         1000,
		FuseMinRangeM:      20000,
		MinSites:           4,
		MinSitesAlt:        3,
		MaxParts:           4,
		SearchBudget:       4096,
		LateHoldNs:         2e9,
		LateSlots:          16384,
		LateMembers:        16,
		ModeACMaxBaselineM: 150000,
		ModeACGateNs:       2000,
		ModeACMinStations:  4,
	}
}

// Normalize clamps every field to its range (NaN takes the default) and
// returns the result.
func (c Config) Normalize() Config {
	d := DefaultConfig()
	f := func(v, def, lo, hi float64) float64 {
		if math.IsNaN(v) {
			return def
		}
		return min(max(v, lo), hi)
	}
	c.NSigma = f(c.NSigma, d.NSigma, 2, 8)
	c.SlackM = f(c.SlackM, d.SlackM, 0, 10000)
	c.HintSlackM = f(c.HintSlackM, d.HintSlackM, 0, 5000)
	c.HintMaxSigmaM = f(c.HintMaxSigmaM, d.HintMaxSigmaM, 0, 50000)
	c.ColocatedM = f(c.ColocatedM, d.ColocatedM, 0, 5000)
	c.FuseMinRangeM = f(c.FuseMinRangeM, d.FuseMinRangeM, 1000, 100000)
	c.MinSites = min(max(c.MinSites, 3), 8)
	c.MinSitesAlt = min(max(c.MinSitesAlt, 3), c.MinSites)
	c.MaxParts = min(max(c.MaxParts, 1), 8)
	c.SearchBudget = min(max(c.SearchBudget, 64), 1_000_000)
	c.LateHoldNs = min(max(c.LateHoldNs, 0), 5e9)
	c.LateSlots = min(max(c.LateSlots, 256), 1<<20)
	c.LateMembers = min(max(c.LateMembers, 4), 64)
	c.ModeACMaxBaselineM = f(c.ModeACMaxBaselineM, d.ModeACMaxBaselineM, 1000, 500000)
	c.ModeACGateNs = f(c.ModeACGateNs, d.ModeACGateNs, 0, 20000)
	c.ModeACMinStations = min(max(c.ModeACMinStations, 3), 8)
	return c
}
