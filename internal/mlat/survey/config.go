package survey

import (
	"math"
	"time"

	"airview/internal/model"
)

// Config holds the survey's tunables. The zero value of every field means
// "the documented default" (Normalize fills them in); ConfigFromModel maps
// the aggregator configuration (aggregator.mlat.autoUseSurvey,
// autoUseMaxSigmaM, surveyIntervalH; the other fields have no file keys
// yet, see WIRING.md).
type Config struct {
	// AutoUse and AutoUseMaxSigmaM mirror aggregator.mlat.autoUseSurvey /
	// autoUseMaxSigmaM (DESIGN §19.3, defaults true / 50 m; AutoUse is a
	// plain bool: DefaultConfig and ConfigFromModel set it). The engine
	// applies the rule (mlat.Engine.SetSurvey); the survey only reports in
	// its results whether one qualifies.
	AutoUse          bool
	AutoUseMaxSigmaM float64

	// MinPeers is the minimum number of synced, located peers whose
	// arrivals fix a transmission's emission time (default 2).
	MinPeers int
	// MaxPeerSigmaNs drops peers whose network-time 1σ (clock + jitter)
	// is larger (default 600 ns).
	MaxPeerSigmaNs float64
	// MinInterval thins the observations: at most one per aircraft per
	// MinInterval and target (default 1 s). When a job holds MaxObs
	// observations every second one is dropped and the interval doubles.
	MinInterval time.Duration
	// MaxObs bounds a job's memory (default 15000 observations, ~2 MB).
	MaxObs int
	// MaxJobs bounds the number of concurrently collecting jobs (default 64).
	// MaxCheckFrac bounds the scheduler's periodic checks of located
	// stations to this fraction of the known stations (default 0.1, at
	// least one): a station under survey is not a peer of other surveys.
	MaxJobs      int
	MaxCheckFrac float64

	// MinDuration is the collection time before the first evaluation
	// (default 3 min); EvalEvery the evaluation period after it (default
	// 1 min); MaxDuration ends a job (default 30 min); NoDataAfter ends a
	// job that has collected fewer than 20 observations (default 10 min).
	MinDuration time.Duration
	EvalEvery   time.Duration
	MaxDuration time.Duration
	NoDataAfter time.Duration
	// CheckEvery is the period of the declared-position check (DESIGN
	// §10.3, default 6 h). RetryAfter is the pause after an unsuccessful
	// job of an unlocated station (default 30 min, doubling up to
	// CheckEvery). ScanEvery is the scheduler's station scan (default 1 min).
	// OwnerMinInterval rate-limits owner-started jobs per station
	// (default 1 h).
	CheckEvery       time.Duration
	RetryAfter       time.Duration
	ScanEvery        time.Duration
	OwnerMinInterval time.Duration

	// MaskDeg is the elevation mask (default 2.5°).
	MaskDeg float64
	// HeightSigmaM is the 1σ of the height prior DEM + AGL (default 20 m,
	// DESIGN §10.2); DefaultAGLM the antenna height when none is declared
	// (default 10 m).
	HeightSigmaM float64
	DefaultAGLM  float64
	// SearchRadiusM is the seed grid's radius around a declared or
	// Locate Me position (default 50 km).
	SearchRadiusM float64

	// StableSigmaGPSM / StableSigmaM: the stability limits on σ_h (DESIGN
	// §10.2: 30 m for GPS classes, 200 m otherwise); MinAzSpreadDeg the
	// azimuth spread (default 180°); MinAircraft the distinct aircraft
	// (default 10).
	StableSigmaGPSM float64
	StableSigmaM    float64
	MinAzSpreadDeg  float64
	MinAircraft     int

	// SuspectMinM and SuspectK: a declared position is suspect when it is
	// more than max(SuspectMinM, SuspectK·σ) from the survey (DESIGN
	// §10.3: 100 m, 3).
	SuspectMinM float64
	SuspectK    float64

	// JackknifeGroups is the largest number of aircraft groups of the
	// delete-one jackknife that gives the honest covariance (default 24);
	// the solver uses about three aircraft per group, at least 10 groups.
	JackknifeGroups int
	// SysFloorAnchoredM / SysFloorRelativeM are added in quadrature to
	// every horizontal and vertical σ: errors no resampling can see
	// (constant peer biases, refraction model). Defaults 3 m in UTC
	// (anchored) time domains and 10 m in relative ones.
	SysFloorAnchoredM float64
	SysFloorRelativeM float64

	// HuberK and RejectK are the robust weights' thresholds in σ (default
	// 2 and 5); BreakZ the clock-break detector's threshold on a
	// normalised second difference (default 8).
	HuberK  float64
	RejectK float64
	BreakZ  float64
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	var c Config
	c.AutoUse = true
	c.Normalize()
	return c
}

// ConfigFromModel maps the aggregator MLAT block (autoUseSurvey,
// autoUseMaxSigmaM, surveyIntervalH → CheckEvery) and its survey block
// (aggregator.mlat.survey, zero = default) onto a normalised Config. The
// solver internals (JackknifeGroups, SysFloor*, HuberK, RejectK, BreakZ,
// ScanEvery) have no file keys and keep their defaults.
func ConfigFromModel(m model.AggMLAT) Config {
	c := Config{AutoUse: m.AutoUseSurvey, AutoUseMaxSigmaM: m.AutoUseMaxSigmaM}
	if m.SurveyIntervalH > 0 {
		c.CheckEvery = time.Duration(m.SurveyIntervalH) * time.Hour
	}
	s := m.Survey
	sec := func(v int) time.Duration { return time.Duration(v) * time.Second }
	mins := func(v int) time.Duration { return time.Duration(v) * time.Minute }
	c.MaxJobs, c.MaxCheckFrac = s.MaxJobs, s.MaxCheckFrac
	c.MinDuration, c.EvalEvery = sec(s.MinDurationSec), sec(s.EvalEverySec)
	c.MaxDuration, c.NoDataAfter, c.RetryAfter = mins(s.MaxDurationMin), mins(s.NoDataAfterMin), mins(s.RetryAfterMin)
	switch {
	case s.OwnerMinIntervalMin < 0:
		c.OwnerMinInterval = time.Nanosecond // no limit
	default:
		c.OwnerMinInterval = mins(s.OwnerMinIntervalMin)
	}
	c.MaxObs, c.MinInterval = s.MaxObs, time.Duration(s.MinIntervalMs)*time.Millisecond
	c.MinPeers, c.MaxPeerSigmaNs = s.MinPeers, s.MaxPeerSigmaNs
	c.MaskDeg, c.HeightSigmaM, c.DefaultAGLM = s.MaskDeg, s.HeightSigmaM, s.DefaultAglM
	c.SearchRadiusM = s.SearchRadiusKm * 1000
	c.StableSigmaGPSM, c.StableSigmaM = s.StableSigmaGpsM, s.StableSigmaM
	c.MinAzSpreadDeg, c.MinAircraft = s.MinAzSpreadDeg, s.MinAircraft
	c.SuspectMinM, c.SuspectK = s.SuspectMinM, s.SuspectK
	c.Normalize()
	return c
}

func defF(v *float64, d, lo, hi float64) {
	if !(*v > 0) || math.IsNaN(*v) {
		*v = d
	}
	*v = math.Min(math.Max(*v, lo), hi)
}

func defD(v *time.Duration, d, lo, hi time.Duration) {
	if *v <= 0 {
		*v = d
	}
	*v = min(max(*v, lo), hi)
}

func defI(v *int, d, lo, hi int) {
	if *v <= 0 {
		*v = d
	}
	*v = min(max(*v, lo), hi)
}

// Normalize fills in defaults and clamps every field to its range. It is
// idempotent.
func (c *Config) Normalize() {
	defF(&c.AutoUseMaxSigmaM, 50, 5, 500)
	defI(&c.MinPeers, 2, 1, 16)
	defF(&c.MaxPeerSigmaNs, 600, 20, 10000)
	defD(&c.MinInterval, time.Second, 100*time.Millisecond, time.Minute)
	defI(&c.MaxObs, 15000, 500, 200000)
	defI(&c.MaxJobs, 64, 1, 4096)
	defF(&c.MaxCheckFrac, 0.1, 0.001, 1)
	defD(&c.MinDuration, 3*time.Minute, 30*time.Second, 6*time.Hour)
	defD(&c.EvalEvery, time.Minute, 5*time.Second, time.Hour)
	defD(&c.MaxDuration, 30*time.Minute, time.Minute, 24*time.Hour)
	c.MaxDuration = max(c.MaxDuration, c.MinDuration)
	defD(&c.NoDataAfter, 10*time.Minute, time.Minute, 24*time.Hour)
	defD(&c.CheckEvery, 6*time.Hour, 10*time.Minute, 30*24*time.Hour)
	defD(&c.RetryAfter, 30*time.Minute, time.Minute, 30*24*time.Hour)
	defD(&c.ScanEvery, time.Minute, time.Second, time.Hour)
	defD(&c.OwnerMinInterval, time.Hour, 0, 7*24*time.Hour)
	defF(&c.MaskDeg, 2.5, 0, 15)
	defF(&c.HeightSigmaM, 20, 1, 1000)
	defF(&c.DefaultAGLM, 10, 0, 500)
	defF(&c.SearchRadiusM, 50e3, 1e3, 1000e3)
	defF(&c.StableSigmaGPSM, 30, 1, 1000)
	defF(&c.StableSigmaM, 200, 1, 5000)
	defF(&c.MinAzSpreadDeg, 180, 0, 360)
	defI(&c.MinAircraft, 10, 2, 10000)
	defF(&c.SuspectMinM, 100, 1, 100e3)
	defF(&c.SuspectK, 3, 1, 20)
	defI(&c.JackknifeGroups, 24, 4, 64)
	defF(&c.SysFloorAnchoredM, 3, 0.01, 1000)
	defF(&c.SysFloorRelativeM, 10, 0.01, 1000)
	defF(&c.HuberK, 2, 0.5, 20)
	defF(&c.RejectK, 5, 2, 100)
	c.RejectK = math.Max(c.RejectK, c.HuberK)
	defF(&c.BreakZ, 8, 3, 1000)
}

// ClockModel is the survey's model of a target clock relative to network
// time (DESIGN §10.2 "clock knots"): a continuous piecewise-linear offset
// with knots every KnotS seconds, whose second differences have the prior
// 1σ CurvNs (a frequency random walk plus thermal ramps), and the prior
// timestamp jitter JitterNs (learned per survey).
type ClockModel struct {
	KnotS    float64
	CurvNs   float64
	JitterNs float64
}

// ClockModelFor returns the clock model of a class; relative is set when
// the peers' time domain has no GPS anchor (its network time wanders like
// the free clocks it is made of).
//
// Derivation (knot spacing Δ): a frequency random walk with intensity q
// gives a second-difference variance ≈ (2/3)·q·Δ³; a thermal ramp of
// slope a (s/s²) a second difference a·Δ². The defaults take the larger of
// the two for the documented device ranges (simnet DefaultClockSpec): GPS
// 30 s / 5 ns; GPS emulated 20 s / 20 ns; hw12 10 s / 10 ns (2e-4 ppm/√s);
// sdr12 4 s / 200 ns (0.01 ppm/s ramps); sdr12_hires 4 s / 80 ns
// (0.003 ppm/s); sbs20 4 s / 120 ns (0.005 ppm/s).
func ClockModelFor(c model.ClockClass, relative bool) ClockModel {
	m := ClockModel{KnotS: 4, CurvNs: 200, JitterNs: c.PriorJitterNs()}
	switch c {
	case model.ClockGPSUTC, model.ClockGPSGPST:
		m.KnotS, m.CurvNs = 30, 5
	case model.ClockGPSEmul:
		m.KnotS, m.CurvNs = 20, 20
	case model.ClockHW12:
		m.KnotS, m.CurvNs = 10, 10
	case model.ClockSDR12HiRes:
		m.KnotS, m.CurvNs = 4, 80
	case model.ClockSBS20:
		m.KnotS, m.CurvNs = 4, 120
	}
	if m.JitterNs <= 0 {
		m.JitterNs = 400
	}
	if relative {
		// The network time of an anchor-less domain is the mean of free
		// clocks: give the offset at least SDR-like freedom.
		if m.KnotS > 4 {
			m.CurvNs = m.CurvNs * math.Pow(4/m.KnotS, 2)
			m.KnotS = 4
		}
		m.CurvNs = math.Max(m.CurvNs, 100)
	}
	return m
}
