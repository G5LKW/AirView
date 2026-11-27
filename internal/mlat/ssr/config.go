package ssr

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Site is a configured interrogator (SSR radar) position. When
// Config.Sites is non-empty the interrogators are not learned from the
// data (LocateInterrogators is skipped) and every pair observation is
// assigned to one of these sites.
type Site struct {
	Name    string
	Lat     float64 // degrees
	Lon     float64 // degrees
	HeightM float64 // antenna height above the WGS-84 ellipsoid, m
	// SigmaHM is the 1σ horizontal uncertainty of the configured position
	// (0 = 50 m). SigmaUM the vertical one (0 = Config.RadarHeightSigmaM).
	SigmaHM float64
	SigmaUM float64
}

// Config holds the tunables of the experimental bistatic SSR
// self-location. The zero value of every numeric field means "the
// documented default" (Normalize fills it in). Enabled defaults to false:
// the estimator is research grade (docs/aggregator/SSR-RESEARCH.md) and is
// never used to locate a station automatically.
type Config struct {
	// Enabled turns the estimator on (default false, experimental).
	Enabled bool

	// DwellGapS separates the reply bursts ("dwells") of one aircraft: a
	// silence longer than this closes the dwell (default 0.05 s; SSR
	// interrogation periods are 2-20 ms).
	DwellGapS float64
	// ProcessLagS is how far behind the newest position report replies
	// are processed, so that every reply has positions on both sides
	// (default 2 s).
	ProcessLagS float64
	// MaxDwellS caps a dwell (default 0.5 s; a beam dwell is 20-80 ms).
	MaxDwellS float64

	// MatchTolNs is the half width of the reply-train match window
	// (default 400 ns: transponder jitter + receiver jitter, both ends).
	MatchTolNs float64
	// MinMatches is the minimum number of common interrogations of a pair
	// dwell (default 4).
	MinMatches int
	// AmbiguityRatio rejects a pair dwell whose PRI alias peak has at
	// least this fraction of the best peak's matches (default 0.6).
	AmbiguityRatio float64
	// MaxPairSepM bounds the separation of two paired aircraft (default
	// 250 km).
	MaxPairSepM float64

	// TimingSigmaACNs / TimingSigmaSNs are the 1σ per-reply timing noise
	// (transponder jitter + receiver time of arrival) of Mode A/C and Mode
	// S replies (defaults 120 ns / 60 ns).
	TimingSigmaACNs float64
	TimingSigmaSNs  float64
	// TrackSigmaM is the 1σ position error of a reference aircraft when
	// the position source gives none (default 20 m).
	TrackSigmaM float64
	// FloorSigmaM is added in quadrature to every observation (default 5 m).
	FloorSigmaM float64
	// BiasSigmaACNs / BiasSigmaSNs are the prior 1σ of the per-airframe
	// transponder reply-delay offset (Annex 10 tolerances ±0.5 µs Mode A/C
	// and ±0.25 µs Mode S; defaults 300 ns / 150 ns).
	BiasSigmaACNs float64
	BiasSigmaSNs  float64

	// MaxObs bounds the stored pair observations (default 60000, ~9 MB);
	// when full every second one is dropped.
	MaxObs int
	// MaxAircraft bounds the solved airframe biases: only the most
	// observed airframes are kept (default 600).
	MaxAircraft int
	// MinObs / MinAircraft / MinRadarObs gate a result (defaults 50 / 10 /
	// 20 observations per interrogator).
	MinObs      int
	MinAircraft int
	MinRadarObs int

	// Interrogator learning (when Sites is empty).
	RadarRegionM      float64 // half size of the search square (default 500 km)
	RadarCellM        float64 // Hough cell (default 4 km)
	MinLineSepM       float64 // minimum pair separation for a bearing line (default 15 km)
	MinRadarLines     int     // minimum lines of an interrogator (default 25)
	MinRadarSpreadDeg float64 // minimum azimuth spread of its lines (default 10°)
	MaxRadars         int     // default 16
	RadarNMSM         float64 // peak suppression radius (default 30 km)
	BeamwidthDeg      float64 // assumed 3 dB beam width incl. margin (default 3°)
	AssignMinTolM     float64 // minimum assignment tolerance (default 3 km)
	RadarHeightM      float64 // assumed height of a learned interrogator, HAE (default 60 m)
	RadarHeightSigmaM float64 // its prior 1σ (default 300 m)
	Sites             []Site
	// LearnedSiteMinSigmaM floors the horizontal 1σ of a learned site's
	// prior in the joint solve (default 1500 m), so that the Δ data can
	// move a badly learned site.
	LearnedSiteMinSigmaM float64
	// MaxLineSigmaM: bearing lines vote only where their expected miss
	// distance is below this (default 10 km).
	MaxLineSigmaM float64

	// Rotation check: an interrogator's pairs must follow one rotating
	// beam. RotWindowS is the phase window (default 60 s), MinPeriodS /
	// MaxPeriodS the period search range (defaults 2 / 15 s),
	// MinCoherence the mean resultant length a learned interrogator needs
	// (default 0.8), PhaseTolDeg the allowance beyond 1.5 beam widths when
	// observations are gated by the beam phase (default 2°).
	RotWindowS   float64
	MinPeriodS   float64
	MaxPeriodS   float64
	MinCoherence float64
	PhaseTolDeg  float64

	// LatencySigmaS is the prior 1σ of a per-airframe position latency
	// (uncompensated ADS-B time of applicability error, solved along the
	// track; default 0.3 s). NoLatency disables those parameters.
	LatencySigmaS float64
	NoLatency     bool

	// Solve.
	SearchRadiusM float64 // seed grid radius without a hint (default 300 km)
	SeedMaxObs    int     // observations used by the seed grid (default 4000)
	HeightSigmaM  float64 // height prior 1σ without a hint (default 1000 m)
	HuberK        float64 // default 2.5
	OutlierZ      float64 // default 5
	MaxIter       int     // default 25
	// MaxSigmaHM: a solution with a larger σ_h is reported but not OK
	// (default 500 m).
	MaxSigmaHM float64

	// ConspicuityCodes are Mode A codes shared by many aircraft and never
	// used to identify a Mode A/C reply (default 7000, 2000, 1200, 0000,
	// 7777, 1000 octal).
	ConspicuityCodes []uint16
}

// DefaultConfig returns the documented defaults (Enabled false).
func DefaultConfig() Config {
	var c Config
	c.Normalize()
	return c
}

func defF(p *float64, v float64) {
	if !(*p > 0) {
		*p = v
	}
}

func defI(p *int, v int) {
	if *p <= 0 {
		*p = v
	}
}

// Normalize fills zero fields with their defaults and clamps nonsense.
func (c *Config) Normalize() {
	defF(&c.DwellGapS, 0.05)
	defF(&c.ProcessLagS, 2)
	defF(&c.MaxDwellS, 0.5)
	defF(&c.MatchTolNs, 400)
	defI(&c.MinMatches, 4)
	defF(&c.AmbiguityRatio, 0.6)
	defF(&c.MaxPairSepM, 250e3)
	defF(&c.TimingSigmaACNs, 120)
	defF(&c.TimingSigmaSNs, 60)
	defF(&c.TrackSigmaM, 20)
	defF(&c.FloorSigmaM, 5)
	defF(&c.BiasSigmaACNs, 300)
	defF(&c.BiasSigmaSNs, 150)
	defI(&c.MaxObs, 60000)
	defI(&c.MaxAircraft, 600)
	defI(&c.MinObs, 50)
	defI(&c.MinAircraft, 10)
	defI(&c.MinRadarObs, 20)
	defF(&c.RadarRegionM, 500e3)
	defF(&c.RadarCellM, 4e3)
	defF(&c.MinLineSepM, 15e3)
	defI(&c.MinRadarLines, 25)
	defF(&c.MinRadarSpreadDeg, 10)
	defI(&c.MaxRadars, 16)
	defF(&c.RadarNMSM, 30e3)
	defF(&c.BeamwidthDeg, 3)
	defF(&c.AssignMinTolM, 3e3)
	if c.RadarHeightM == 0 {
		c.RadarHeightM = 60
	}
	defF(&c.RadarHeightSigmaM, 300)
	defF(&c.MaxLineSigmaM, 10e3)
	defF(&c.LearnedSiteMinSigmaM, 1500)
	defF(&c.RotWindowS, 60)
	defF(&c.MinPeriodS, 2)
	defF(&c.MaxPeriodS, 15)
	defF(&c.MinCoherence, 0.8)
	defF(&c.PhaseTolDeg, 2)
	defF(&c.LatencySigmaS, 0.3)
	if c.MaxPeriodS <= c.MinPeriodS {
		c.MaxPeriodS = c.MinPeriodS + 1
	}
	c.MinCoherence = math.Min(c.MinCoherence, 1)
	defF(&c.SearchRadiusM, 300e3)
	defI(&c.SeedMaxObs, 4000)
	defF(&c.HeightSigmaM, 1000)
	defF(&c.HuberK, 2.5)
	defF(&c.OutlierZ, 5)
	defI(&c.MaxIter, 25)
	defF(&c.MaxSigmaHM, 500)
	if c.ConspicuityCodes == nil {
		c.ConspicuityCodes = []uint16{0o7000, 0o2000, 0o1200, 0o0000, 0o7777, 0o1000}
	}
	if c.MinMatches < 2 {
		c.MinMatches = 2
	}
	c.AmbiguityRatio = math.Min(c.AmbiguityRatio, 1)
	c.MaxDwellS = math.Max(c.MaxDwellS, c.DwellGapS)
	if c.MaxObs < 1000 {
		c.MaxObs = 1000
	}
	if c.MaxAircraft < 10 {
		c.MaxAircraft = 10
	}
}

// Validate reports settings that Normalize cannot repair.
func (c *Config) Validate() error {
	var errs []error
	for i, s := range c.Sites {
		if s.Lat < -90 || s.Lat > 90 || s.Lon < -180 || s.Lon > 180 || math.IsNaN(s.Lat+s.Lon) {
			errs = append(errs, fmt.Errorf("ssr: sites[%d] (%q): latitude/longitude out of range", i, s.Name))
		}
		if math.Abs(s.HeightM) > 9000 {
			errs = append(errs, fmt.Errorf("ssr: sites[%d] (%q): height %g m out of range", i, s.Name, s.HeightM))
		}
	}
	if c.BeamwidthDeg > 20 {
		errs = append(errs, fmt.Errorf("ssr: beamwidthDeg %g > 20", c.BeamwidthDeg))
	}
	if c.MatchTolNs > 5000 {
		errs = append(errs, fmt.Errorf("ssr: matchTolNs %g > 5000", c.MatchTolNs))
	}
	return errors.Join(errs...)
}

// envField is one AIRVIEW_SSR_* override.
type envField struct {
	key string
	f   *float64
	i   *int
	b   *bool
}

func (c *Config) envFields() []envField {
	return []envField{
		{key: "ENABLED", b: &c.Enabled},
		{key: "DWELL_GAP_S", f: &c.DwellGapS},
		{key: "PROCESS_LAG_S", f: &c.ProcessLagS},
		{key: "MAX_DWELL_S", f: &c.MaxDwellS},
		{key: "MATCH_TOL_NS", f: &c.MatchTolNs},
		{key: "MIN_MATCHES", i: &c.MinMatches},
		{key: "AMBIGUITY_RATIO", f: &c.AmbiguityRatio},
		{key: "MAX_PAIR_SEP_M", f: &c.MaxPairSepM},
		{key: "TIMING_SIGMA_AC_NS", f: &c.TimingSigmaACNs},
		{key: "TIMING_SIGMA_S_NS", f: &c.TimingSigmaSNs},
		{key: "TRACK_SIGMA_M", f: &c.TrackSigmaM},
		{key: "FLOOR_SIGMA_M", f: &c.FloorSigmaM},
		{key: "BIAS_SIGMA_AC_NS", f: &c.BiasSigmaACNs},
		{key: "BIAS_SIGMA_S_NS", f: &c.BiasSigmaSNs},
		{key: "MAX_OBS", i: &c.MaxObs},
		{key: "MAX_AIRCRAFT", i: &c.MaxAircraft},
		{key: "MIN_OBS", i: &c.MinObs},
		{key: "MIN_AIRCRAFT", i: &c.MinAircraft},
		{key: "MIN_RADAR_OBS", i: &c.MinRadarObs},
		{key: "RADAR_REGION_M", f: &c.RadarRegionM},
		{key: "RADAR_CELL_M", f: &c.RadarCellM},
		{key: "MIN_LINE_SEP_M", f: &c.MinLineSepM},
		{key: "MIN_RADAR_LINES", i: &c.MinRadarLines},
		{key: "MIN_RADAR_SPREAD_DEG", f: &c.MinRadarSpreadDeg},
		{key: "MAX_RADARS", i: &c.MaxRadars},
		{key: "RADAR_NMS_M", f: &c.RadarNMSM},
		{key: "BEAMWIDTH_DEG", f: &c.BeamwidthDeg},
		{key: "ASSIGN_MIN_TOL_M", f: &c.AssignMinTolM},
		{key: "RADAR_HEIGHT_M", f: &c.RadarHeightM},
		{key: "RADAR_HEIGHT_SIGMA_M", f: &c.RadarHeightSigmaM},
		{key: "MAX_LINE_SIGMA_M", f: &c.MaxLineSigmaM},
		{key: "LEARNED_SITE_MIN_SIGMA_M", f: &c.LearnedSiteMinSigmaM},
		{key: "ROT_WINDOW_S", f: &c.RotWindowS},
		{key: "MIN_PERIOD_S", f: &c.MinPeriodS},
		{key: "MAX_PERIOD_S", f: &c.MaxPeriodS},
		{key: "MIN_COHERENCE", f: &c.MinCoherence},
		{key: "PHASE_TOL_DEG", f: &c.PhaseTolDeg},
		{key: "LATENCY_SIGMA_S", f: &c.LatencySigmaS},
		{key: "NO_LATENCY", b: &c.NoLatency},
		{key: "SEARCH_RADIUS_M", f: &c.SearchRadiusM},
		{key: "SEED_MAX_OBS", i: &c.SeedMaxObs},
		{key: "HEIGHT_SIGMA_M", f: &c.HeightSigmaM},
		{key: "HUBER_K", f: &c.HuberK},
		{key: "OUTLIER_Z", f: &c.OutlierZ},
		{key: "MAX_ITER", i: &c.MaxIter},
		{key: "MAX_SIGMA_H_M", f: &c.MaxSigmaHM},
	}
}

// EnvKeys lists the environment variables ApplyEnv reads (for config
// check output and documentation).
func EnvKeys() []string {
	var c Config
	fs := c.envFields()
	out := make([]string, 0, len(fs)+1)
	for _, f := range fs {
		out = append(out, EnvPrefix+f.key)
	}
	return append(out, EnvPrefix+"SITES")
}

// EnvPrefix is the prefix of the environment overrides.
const EnvPrefix = "AIRVIEW_SSR_"

// ApplyEnv applies AIRVIEW_SSR_* overrides read through lookup (normally
// os.LookupEnv). AIRVIEW_SSR_SITES is a ';'-separated list of
// "name,lat,lon,heightM" entries and replaces Sites. Unparsable values are
// reported together; the fields that parsed are applied.
func (c *Config) ApplyEnv(lookup func(string) (string, bool)) error {
	var errs []error
	for _, f := range c.envFields() {
		v, ok := lookup(EnvPrefix + f.key)
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case f.b != nil:
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s: %w", EnvPrefix, f.key, err))
				continue
			}
			*f.b = b
		case f.i != nil:
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s: %w", EnvPrefix, f.key, err))
				continue
			}
			*f.i = n
		default:
			x, err := strconv.ParseFloat(v, 64)
			if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
				errs = append(errs, fmt.Errorf("%s%s: invalid number %q", EnvPrefix, f.key, v))
				continue
			}
			*f.f = x
		}
	}
	if v, ok := lookup(EnvPrefix + "SITES"); ok {
		sites, err := ParseSites(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%sSITES: %w", EnvPrefix, err))
		} else {
			c.Sites = sites
		}
	}
	return errors.Join(errs...)
}

// ParseSites parses "name,lat,lon,heightM;name,lat,lon,heightM". The
// height may be omitted (0).
func ParseSites(s string) ([]Site, error) {
	var out []Site
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f := strings.Split(part, ",")
		if len(f) < 3 || len(f) > 4 {
			return nil, fmt.Errorf("site %q: want name,lat,lon[,heightM]", part)
		}
		var v [3]float64
		for i := 1; i < len(f); i++ {
			x, err := strconv.ParseFloat(strings.TrimSpace(f[i]), 64)
			if err != nil {
				return nil, fmt.Errorf("site %q: %w", part, err)
			}
			v[i-1] = x
		}
		out = append(out, Site{Name: strings.TrimSpace(f[0]), Lat: v[0], Lon: v[1], HeightM: v[2]})
	}
	c := Config{Sites: out}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}
