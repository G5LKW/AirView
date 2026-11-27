package plan

import (
	"math"
	"time"
)

// Config tunes the planner. Start from DefaultConfig; Normalize clamps every
// field to its documented range.
type Config struct {
	// SyncSetMin / SyncSetMax: aircraft per station in its syncSet (DESIGN
	// §4.4: 15-30). Ranges 0..200 and SyncSetMin..200.
	SyncSetMin, SyncSetMax int
	// MLATSetMax caps a station's mlatSet (aircraft). Range 0..5000.
	MLATSetMax int
	// StationFPSBudget caps the estimated frame rate a selective station is
	// asked to send for its two sets together (frames/s). Range 10..6000.
	StationFPSBudget float64
	// StationsPerAircraft is the most stations asked to send frames of one
	// MLAT aircraft; MinStationsPerAircraft the number always asked for when
	// available (more than the 4 needed, for robustness: RAIM needs ≥ 5).
	// TargetSigmaM: above the minimum, stations are only added while the
	// expected horizontal CRLB is above this. Ranges 4..64, 4..Stations,
	// 1..1000 m.
	StationsPerAircraft    int
	MinStationsPerAircraft int
	TargetSigmaM           float64
	// HeardTTL: a station counts as hearing an aircraft for this long after
	// its last copy (DESIGN §4.4 sets are about live traffic). 5 s..10 min.
	HeardTTL time.Duration
	// ADSBGoodAge: an aircraft with a good ADS-B position within this is not
	// an MLAT candidate (DESIGN §9.3: 10 s). 1 s..2 min.
	ADSBGoodAge time.Duration
	// TrackedSigmaM: an aircraft whose MLAT/ADS-B position (Positions) is
	// better than this is "tracked" (lower solve rate). 100 m..20 km.
	TrackedSigmaM float64
	// MinDwell keeps a set member at least this long; SwapMargin is the
	// factor a newcomer's gain must beat a current member's by; MaxChurn the
	// most adds plus removals per station and replan. 0..10 min, 1..4, 1..1000.
	MinDwell   time.Duration
	SwapMargin float64
	MaxChurn   int
	// ReplanEvery is the replan period Run uses. 1 s..60 s.
	ReplanEvery time.Duration
	// Solve rates per aircraft (solves/s) by priority, and the token burst.
	// Ranges 0.05..50 and 1..20.
	AcquireHz, TrackedHz, HoldOutHz, SuspectHz float64
	Burst                                      float64
	// GlobalSolveHz is the solver budget for the whole network (solves/s)
	// and Reserve the fraction of the global bucket that must remain for
	// each priority class to be admitted (P0 acquire .. P3 suspect): lower
	// classes back off first when the solver is saturated. 10..1e6; 0..0.9.
	GlobalSolveHz float64
	Reserve       [NumPrio]float64
	// ModeAC: Mode A/C MLAT among anchors (DESIGN §9.9; phase 5, off).
	ModeAC ModeACConfig
	// MaxAircraft bounds the aircraft table (default 40000; the engine
	// sets it from aggregator.limits.maxAircraft). Beyond it the least
	// recently seen aircraft are evicted. 1000..1,000,000.
	MaxAircraft int
}

// ModeACConfig selects the stations that take part in Mode A/C MLAT.
type ModeACConfig struct {
	Enabled bool
	// MaxBaselineKm: anchors this close count as peers (150). 10..500.
	MaxBaselineKm float64
	// MinPeers: an anchor takes part when it has at least this many anchor
	// peers (3, for 4-station fixes). 2..20.
	MinPeers int
}

// DefaultConfig returns the DESIGN §4.4 / §9.3 values.
func DefaultConfig() Config {
	return Config{
		SyncSetMin: 15, SyncSetMax: 30,
		MLATSetMax:             400,
		StationFPSBudget:       400,
		StationsPerAircraft:    12,
		MinStationsPerAircraft: 6,
		TargetSigmaM:           30,
		HeardTTL:               60 * time.Second,
		ADSBGoodAge:            10 * time.Second,
		TrackedSigmaM:          2000,
		MinDwell:               30 * time.Second,
		SwapMargin:             1.25,
		MaxChurn:               10,
		ReplanEvery:            5 * time.Second,
		AcquireHz:              4, TrackedHz: 2, HoldOutHz: 1, SuspectHz: 0.5,
		Burst:         3,
		GlobalSolveHz: 20000,
		Reserve:       [NumPrio]float64{0, 0.1, 0.3, 0.5},
		ModeAC:        ModeACConfig{Enabled: false, MaxBaselineKm: 150, MinPeers: 3},
		MaxAircraft:   40000,
	}
}

// Normalize clamps the fields to their ranges and returns the result.
func (c Config) Normalize() Config {
	d := DefaultConfig()
	fl := func(v, def, lo, hi float64) float64 {
		if math.IsNaN(v) || v == 0 {
			v = def
		}
		return min(max(v, lo), hi)
	}
	dur := func(v, def, lo, hi time.Duration) time.Duration {
		if v == 0 {
			v = def
		}
		return min(max(v, lo), hi)
	}
	c.SyncSetMin = min(max(c.SyncSetMin, 0), 200)
	c.SyncSetMax = min(max(c.SyncSetMax, c.SyncSetMin), 200)
	c.MLATSetMax = min(max(c.MLATSetMax, 0), 5000)
	c.StationFPSBudget = fl(c.StationFPSBudget, d.StationFPSBudget, 10, 6000)
	c.StationsPerAircraft = min(max(c.StationsPerAircraft, 4), 64)
	c.MinStationsPerAircraft = min(max(c.MinStationsPerAircraft, 4), c.StationsPerAircraft)
	c.TargetSigmaM = fl(c.TargetSigmaM, d.TargetSigmaM, 1, 1000)
	c.HeardTTL = dur(c.HeardTTL, d.HeardTTL, 5*time.Second, 10*time.Minute)
	c.ADSBGoodAge = dur(c.ADSBGoodAge, d.ADSBGoodAge, time.Second, 2*time.Minute)
	c.TrackedSigmaM = fl(c.TrackedSigmaM, d.TrackedSigmaM, 100, 20000)
	c.MinDwell = min(max(c.MinDwell, 0), 10*time.Minute)
	c.SwapMargin = fl(c.SwapMargin, d.SwapMargin, 1, 4)
	c.MaxChurn = min(max(c.MaxChurn, 1), 1000)
	c.ReplanEvery = dur(c.ReplanEvery, d.ReplanEvery, time.Second, time.Minute)
	c.AcquireHz = fl(c.AcquireHz, d.AcquireHz, 0.05, 50)
	c.TrackedHz = fl(c.TrackedHz, d.TrackedHz, 0.05, 50)
	c.HoldOutHz = fl(c.HoldOutHz, d.HoldOutHz, 0.05, 50)
	c.SuspectHz = fl(c.SuspectHz, d.SuspectHz, 0.05, 50)
	c.Burst = fl(c.Burst, d.Burst, 1, 20)
	c.GlobalSolveHz = fl(c.GlobalSolveHz, d.GlobalSolveHz, 10, 1e6)
	for i := range c.Reserve {
		if math.IsNaN(c.Reserve[i]) {
			c.Reserve[i] = d.Reserve[i]
		}
		c.Reserve[i] = min(max(c.Reserve[i], 0), 0.9)
	}
	c.ModeAC.MaxBaselineKm = fl(c.ModeAC.MaxBaselineKm, d.ModeAC.MaxBaselineKm, 10, 500)
	if c.ModeAC.MinPeers == 0 {
		c.ModeAC.MinPeers = d.ModeAC.MinPeers
	}
	c.ModeAC.MinPeers = min(max(c.ModeAC.MinPeers, 2), 20)
	if c.MaxAircraft == 0 {
		c.MaxAircraft = d.MaxAircraft
	}
	c.MaxAircraft = min(max(c.MaxAircraft, 1000), 1_000_000)
	return c
}
