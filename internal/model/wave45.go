package model

import (
	"math"
	"slices"
	"strconv"
	"strings"
)

// Settings added by WP-WIRE-45 (phase 4-5 wiring): the airframe-latency
// calibration of the MLAT engine and the experimental bistatic SSR
// self-location. DESIGN §20.3: every tunable is file-configurable, has an
// AIRVIEW_* variable and is checked by `airview config check`.

// AggMLATLatencyCal configures aggregator.mlat.latencyCal: the per-airframe
// ADS-B position-latency calibration of the reference tracks with its own A/B
// gate (internal/mlat/quality LatencyCal, DESIGN §9.6). Zero = default.
type AggMLATLatencyCal struct {
	// Enabled runs the calibration (default false). Even when on, the
	// built-in A/B test applies learned latencies only after they measurably
	// reduce the along-track error of the treated half of the airframes.
	Enabled bool `json:"enabled"`
	// MinGain is the relative error reduction the A/B test needs before the
	// calibration is applied (default 0.05, 0.001..1).
	MinGain float64 `json:"minGain"`
	// ZEnable is the significance needed to enable it (default 3, 0.5..20).
	ZEnable float64 `json:"zEnable"`
	// ZDisable is the significance below which an enabled calibration is
	// switched off again (default 2, 0.1..20).
	ZDisable float64 `json:"zDisable"`
	// TreatFraction is the share of calibrated airframes that get their
	// learned latency (the rest are the control arm; default 0.5,
	// 0.05..0.95).
	TreatFraction float64 `json:"treatFraction"`
	// MinCalSamples: an airframe counts as calibrated after this many
	// samples (default 20, 1..100000).
	MinCalSamples int `json:"minCalSamples"`
	// MinArmSamples is the samples each A/B arm needs (default 500,
	// 10..10000000).
	MinArmSamples int `json:"minArmSamples"`
	// MinArmAircraft is the airframes each A/B arm needs (default 20,
	// 2..100000).
	MinArmAircraft int `json:"minArmAircraft"`
	// WindowH is the A/B window in hours (default 6, 1..168).
	WindowH int `json:"windowH"`
	// PriorSigmaS is the prior 1-sigma of an airframe latency in seconds
	// (default 0.15, 0.001..2).
	PriorSigmaS float64 `json:"priorSigmaS"`
	// CalibratedSigmaS: an airframe counts as calibrated below this
	// posterior 1-sigma in seconds (default 0.04, 0.001..1).
	CalibratedSigmaS float64 `json:"calibratedSigmaS"`
}

// normalizeLatencyCal checks aggregator.mlat.latencyCal.
func (a *Aggregator) normalizeLatencyCal(n *notes) {
	c := &a.MLAT.LatencyCal
	p := "aggregator.mlat.latencyCal."
	zeroOrRangeF(n, p+"minGain", &c.MinGain, 0.001, 1)
	zeroOrRangeF(n, p+"zEnable", &c.ZEnable, 0.5, 20)
	zeroOrRangeF(n, p+"zDisable", &c.ZDisable, 0.1, 20)
	zeroOrRangeF(n, p+"treatFraction", &c.TreatFraction, 0.05, 0.95)
	zeroOrRangeI(n, p+"minCalSamples", &c.MinCalSamples, 1, 100000)
	zeroOrRangeI(n, p+"minArmSamples", &c.MinArmSamples, 10, 10_000_000)
	zeroOrRangeI(n, p+"minArmAircraft", &c.MinArmAircraft, 2, 100000)
	zeroOrRangeI(n, p+"windowH", &c.WindowH, 1, 168)
	zeroOrRangeF(n, p+"priorSigmaS", &c.PriorSigmaS, 0.001, 2)
	zeroOrRangeF(n, p+"calibratedSigmaS", &c.CalibratedSigmaS, 0.001, 1)
	if c.ZEnable != 0 && c.ZDisable != 0 && c.ZDisable > c.ZEnable {
		n.addf("%szDisable %g is above zEnable %g; the default is used", p, c.ZDisable, c.ZEnable)
		c.ZDisable = 0
	}
}

// SSRConfig configures the EXPERIMENTAL bistatic SSR self-location of this
// receiver (top-level "ssr" block; internal/mlat/ssr,
// docs/aggregator/SSR-RESEARCH.md). Off by default. Its result is shown to
// the station owner / admin only and is never used to set, confirm or
// replace a station position (DESIGN §8, §19.3); at most it may seed a
// survey search. Every numeric field: 0 = the package default.
//
// The Go field names equal those of ssr.Config; the engine maps them by
// name (internal/engine/ssrwire.go, tested).
type SSRConfig struct {
	// Enabled turns the estimator on (default false, experimental).
	Enabled bool `json:"enabled"`
	// FeedID is the local feed whose receiver timestamps are used ("" = the
	// first feed that delivers timestamped frames). One receiver clock
	// only: replies of other feeds are ignored.
	FeedID string `json:"feedId,omitempty"`
	// SolveEveryMin is how often the position is solved in the background
	// (default 10, 1..1440 minutes; a solve takes about 0.3 s).
	SolveEveryMin int `json:"solveEveryMin"`
	// Sites are known interrogator (radar) positions. Empty = learn them
	// from the data.
	Sites []SSRSite `json:"sites,omitempty"`
	// ConspicuityCodes are Mode A codes (4 octal digits) shared by many
	// aircraft and never used to identify a Mode A/C reply (default 7000,
	// 2000, 1200, 0000, 7777, 1000).
	ConspicuityCodes []string `json:"conspicuityCodes,omitempty"`
	// ModeACCodeFormat is the layout of the receiver's 2-byte Mode A/C
	// payloads: "" or "pulses" (reply pulses in transmission order, the
	// Beast default) or "nibbles" (one octal digit per nibble).
	ModeACCodeFormat string `json:"modeAcCodeFormat,omitempty"`

	// DwellGapS is the silence that ends a reply burst (default 0.05,
	// 0.001..1 s).
	DwellGapS float64 `json:"dwellGapS"`
	// ProcessLagS: replies are processed this far behind the newest input
	// (default 2, 0.1..30 s).
	ProcessLagS float64 `json:"processLagS"`
	// MaxDwellS caps a reply burst (default 0.5, 0.01..5 s).
	MaxDwellS float64 `json:"maxDwellS"`
	// MatchTolNs is the half width of the reply-train match window
	// (default 400, 10..5000 ns).
	MatchTolNs float64 `json:"matchTolNs"`
	// MinMatches is the minimum common interrogations of a pair dwell
	// (default 4, 2..100).
	MinMatches int `json:"minMatches"`
	// AmbiguityRatio rejects a pair whose alias peak reaches this fraction
	// of the best peak (default 0.6, 0.01..1).
	AmbiguityRatio float64 `json:"ambiguityRatio"`
	// MaxPairSepM bounds the separation of two paired aircraft (default
	// 250000, 1000..1000000 m).
	MaxPairSepM float64 `json:"maxPairSepM"`
	// TimingSigmaACNs is the per-reply timing noise of Mode A/C replies
	// (default 120, 1..5000 ns; about 180 for RTL-SDR receivers).
	TimingSigmaACNs float64 `json:"timingSigmaACNs"`
	// TimingSigmaSNs is the per-reply timing noise of Mode S replies
	// (default 60, 1..5000 ns).
	TimingSigmaSNs float64 `json:"timingSigmaSNs"`
	// TrackSigmaM is the position noise of a reference aircraft without
	// NACp (default 20, 0.1..10000 m).
	TrackSigmaM float64 `json:"trackSigmaM"`
	// FloorSigmaM is added to every observation (default 5, 0.01..1000 m).
	FloorSigmaM float64 `json:"floorSigmaM"`
	// BiasSigmaACNs is the prior of a Mode A/C reply-delay offset (default
	// 300, 1..10000 ns).
	BiasSigmaACNs float64 `json:"biasSigmaACNs"`
	// BiasSigmaSNs is the prior of a Mode S reply-delay offset (default
	// 150, 1..10000 ns).
	BiasSigmaSNs float64 `json:"biasSigmaSNs"`
	// MaxObs bounds the stored pair observations (default 60000,
	// 1000..10000000).
	MaxObs int `json:"maxObs"`
	// MaxAircraft bounds the solved airframe biases (default 600,
	// 10..100000).
	MaxAircraft int `json:"maxAircraft"`
	// MinObs gates a result: minimum observations (default 50, 1..1000000).
	MinObs int `json:"minObs"`
	// MinAircraft gates a result: minimum airframes (default 10, 1..10000).
	MinAircraft int `json:"minAircraft"`
	// MinRadarObs gates a result: observations per interrogator (default
	// 20, 1..1000000).
	MinRadarObs int `json:"minRadarObs"`
	// RadarRegionM is the half size of the interrogator search square
	// (default 500000, 10000..2000000 m).
	RadarRegionM float64 `json:"radarRegionM"`
	// RadarCellM is the Hough cell of the site learning (default 4000,
	// 100..100000 m).
	RadarCellM float64 `json:"radarCellM"`
	// MinLineSepM is the minimum pair separation for a bearing line
	// (default 15000, 100..1000000 m).
	MinLineSepM float64 `json:"minLineSepM"`
	// MinRadarLines is the minimum bearing lines of a learned interrogator
	// (default 25, 2..100000).
	MinRadarLines int `json:"minRadarLines"`
	// MinRadarSpreadDeg is the minimum azimuth spread of those lines
	// (default 10, 0.1..180 degrees).
	MinRadarSpreadDeg float64 `json:"minRadarSpreadDeg"`
	// MaxRadars bounds the learned interrogators (default 16, 1..256).
	MaxRadars int `json:"maxRadars"`
	// RadarNMSM is the peak suppression radius (default 30000,
	// 100..1000000 m).
	RadarNMSM float64 `json:"radarNmsM"`
	// BeamwidthDeg is the assumed beam width incl. margin (default 3,
	// 0.1..20 degrees).
	BeamwidthDeg float64 `json:"beamwidthDeg"`
	// AssignMinTolM is the minimum assignment tolerance (default 3000,
	// 1..1000000 m).
	AssignMinTolM float64 `json:"assignMinTolM"`
	// RadarHeightM is the assumed height of a learned interrogator above
	// the ellipsoid (default 60, -500..9000 m).
	RadarHeightM float64 `json:"radarHeightM"`
	// RadarHeightSigmaM is its prior 1-sigma (default 300, 1..10000 m).
	RadarHeightSigmaM float64 `json:"radarHeightSigmaM"`
	// LearnedSiteMinSigmaM floors a learned site's horizontal prior
	// (default 1500, 1..1000000 m).
	LearnedSiteMinSigmaM float64 `json:"learnedSiteMinSigmaM"`
	// MaxLineSigmaM: bearing lines vote only below this expected miss
	// (default 10000, 10..1000000 m).
	MaxLineSigmaM float64 `json:"maxLineSigmaM"`
	// RotWindowS is the rotation-check phase window (default 60,
	// 1..3600 s).
	RotWindowS float64 `json:"rotWindowS"`
	// MinPeriodS is the shortest radar rotation period searched (default
	// 2, 0.5..60 s).
	MinPeriodS float64 `json:"minPeriodS"`
	// MaxPeriodS is the longest one (default 15, 1..120 s).
	MaxPeriodS float64 `json:"maxPeriodS"`
	// MinCoherence is the rotation coherence a learned interrogator needs
	// (default 0.8, 0.01..1).
	MinCoherence float64 `json:"minCoherence"`
	// PhaseTolDeg is the beam-phase allowance (default 2, 0.01..90 degrees).
	PhaseTolDeg float64 `json:"phaseTolDeg"`
	// LatencySigmaS is the prior of a per-airframe ADS-B latency (default
	// 0.3, 0.001..10 s).
	LatencySigmaS float64 `json:"latencySigmaS"`
	// NoLatency disables the per-airframe latency parameters.
	NoLatency bool `json:"noLatency"`
	// SearchRadiusM is the seed grid radius without a hint (default 300000,
	// 1000..2000000 m).
	SearchRadiusM float64 `json:"searchRadiusM"`
	// SeedMaxObs is the observations used by the seed grid (default 4000,
	// 100..1000000).
	SeedMaxObs int `json:"seedMaxObs"`
	// HeightSigmaM is the height prior without a hint (default 1000,
	// 1..100000 m).
	HeightSigmaM float64 `json:"heightSigmaM"`
	// HuberK is the robust-solve threshold (default 2.5, 0.1..100).
	HuberK float64 `json:"huberK"`
	// OutlierZ rejects observations beyond this normalised residual
	// (default 5, 1..100).
	OutlierZ float64 `json:"outlierZ"`
	// MaxIter bounds the solver iterations (default 25, 1..1000).
	MaxIter int `json:"maxIter"`
	// MaxSigmaHM: a solution with a larger horizontal 1-sigma is reported
	// but not OK (default 500, 1..1000000 m).
	MaxSigmaHM float64 `json:"maxSigmaHM"`
}

// SSRSite is one known interrogator (ssr.sites[]).
type SSRSite struct {
	// Name is a label.
	Name string `json:"name"`
	// Lat is the WGS-84 latitude in degrees.
	Lat float64 `json:"lat"`
	// Lon is the WGS-84 longitude in degrees.
	Lon float64 `json:"lon"`
	// HeightM is the antenna height above the ellipsoid (-500..9000 m).
	HeightM float64 `json:"heightM"`
	// SigmaHM is the horizontal 1-sigma of the position (0 = 50 m).
	SigmaHM float64 `json:"sigmaHM,omitempty"`
	// SigmaUM is the vertical 1-sigma (0 = radarHeightSigmaM).
	SigmaUM float64 `json:"sigmaUM,omitempty"`
}

// Clone deep-copies the lists.
func (s SSRConfig) Clone() SSRConfig {
	s.Sites = slices.Clone(s.Sites)
	s.ConspicuityCodes = slices.Clone(s.ConspicuityCodes)
	return s
}

// ParseOctalCode parses a 4-digit octal Mode A code ("7700").
func ParseOctalCode(s string) (uint16, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 8, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

// Normalize corrects out-of-range values in place and returns notes.
func (s *SSRConfig) Normalize() []string {
	var n notes
	p := "ssr."
	s.FeedID = strings.TrimSpace(s.FeedID)
	switch f := strings.ToLower(strings.TrimSpace(s.ModeACCodeFormat)); f {
	case "", "pulses", "nibbles":
		s.ModeACCodeFormat = f
	default:
		n.addf("%smodeAcCodeFormat %q is not pulses or nibbles; pulses is used", p, s.ModeACCodeFormat)
		s.ModeACCodeFormat = ""
	}
	zeroOrRangeI(&n, p+"solveEveryMin", &s.SolveEveryMin, 1, 1440)
	zeroOrRangeF(&n, p+"dwellGapS", &s.DwellGapS, 0.001, 1)
	zeroOrRangeF(&n, p+"processLagS", &s.ProcessLagS, 0.1, 30)
	zeroOrRangeF(&n, p+"maxDwellS", &s.MaxDwellS, 0.01, 5)
	zeroOrRangeF(&n, p+"matchTolNs", &s.MatchTolNs, 10, 5000)
	zeroOrRangeI(&n, p+"minMatches", &s.MinMatches, 2, 100)
	zeroOrRangeF(&n, p+"ambiguityRatio", &s.AmbiguityRatio, 0.01, 1)
	zeroOrRangeF(&n, p+"maxPairSepM", &s.MaxPairSepM, 1000, 1e6)
	zeroOrRangeF(&n, p+"timingSigmaACNs", &s.TimingSigmaACNs, 1, 5000)
	zeroOrRangeF(&n, p+"timingSigmaSNs", &s.TimingSigmaSNs, 1, 5000)
	zeroOrRangeF(&n, p+"trackSigmaM", &s.TrackSigmaM, 0.1, 10000)
	zeroOrRangeF(&n, p+"floorSigmaM", &s.FloorSigmaM, 0.01, 1000)
	zeroOrRangeF(&n, p+"biasSigmaACNs", &s.BiasSigmaACNs, 1, 10000)
	zeroOrRangeF(&n, p+"biasSigmaSNs", &s.BiasSigmaSNs, 1, 10000)
	zeroOrRangeI(&n, p+"maxObs", &s.MaxObs, 1000, 10_000_000)
	zeroOrRangeI(&n, p+"maxAircraft", &s.MaxAircraft, 10, 100000)
	zeroOrRangeI(&n, p+"minObs", &s.MinObs, 1, 1_000_000)
	zeroOrRangeI(&n, p+"minAircraft", &s.MinAircraft, 1, 10000)
	zeroOrRangeI(&n, p+"minRadarObs", &s.MinRadarObs, 1, 1_000_000)
	zeroOrRangeF(&n, p+"radarRegionM", &s.RadarRegionM, 10000, 2e6)
	zeroOrRangeF(&n, p+"radarCellM", &s.RadarCellM, 100, 100000)
	zeroOrRangeF(&n, p+"minLineSepM", &s.MinLineSepM, 100, 1e6)
	zeroOrRangeI(&n, p+"minRadarLines", &s.MinRadarLines, 2, 100000)
	zeroOrRangeF(&n, p+"minRadarSpreadDeg", &s.MinRadarSpreadDeg, 0.1, 180)
	zeroOrRangeI(&n, p+"maxRadars", &s.MaxRadars, 1, 256)
	zeroOrRangeF(&n, p+"radarNmsM", &s.RadarNMSM, 100, 1e6)
	zeroOrRangeF(&n, p+"beamwidthDeg", &s.BeamwidthDeg, 0.1, 20)
	zeroOrRangeF(&n, p+"assignMinTolM", &s.AssignMinTolM, 1, 1e6)
	zeroOrRangeF(&n, p+"radarHeightM", &s.RadarHeightM, -500, 9000)
	zeroOrRangeF(&n, p+"radarHeightSigmaM", &s.RadarHeightSigmaM, 1, 10000)
	zeroOrRangeF(&n, p+"learnedSiteMinSigmaM", &s.LearnedSiteMinSigmaM, 1, 1e6)
	zeroOrRangeF(&n, p+"maxLineSigmaM", &s.MaxLineSigmaM, 10, 1e6)
	zeroOrRangeF(&n, p+"rotWindowS", &s.RotWindowS, 1, 3600)
	zeroOrRangeF(&n, p+"minPeriodS", &s.MinPeriodS, 0.5, 60)
	zeroOrRangeF(&n, p+"maxPeriodS", &s.MaxPeriodS, 1, 120)
	zeroOrRangeF(&n, p+"minCoherence", &s.MinCoherence, 0.01, 1)
	zeroOrRangeF(&n, p+"phaseTolDeg", &s.PhaseTolDeg, 0.01, 90)
	zeroOrRangeF(&n, p+"latencySigmaS", &s.LatencySigmaS, 0.001, 10)
	zeroOrRangeF(&n, p+"searchRadiusM", &s.SearchRadiusM, 1000, 2e6)
	zeroOrRangeI(&n, p+"seedMaxObs", &s.SeedMaxObs, 100, 1_000_000)
	zeroOrRangeF(&n, p+"heightSigmaM", &s.HeightSigmaM, 1, 100000)
	zeroOrRangeF(&n, p+"huberK", &s.HuberK, 0.1, 100)
	zeroOrRangeF(&n, p+"outlierZ", &s.OutlierZ, 1, 100)
	zeroOrRangeI(&n, p+"maxIter", &s.MaxIter, 1, 1000)
	zeroOrRangeF(&n, p+"maxSigmaHM", &s.MaxSigmaHM, 1, 1e6)
	if s.MinPeriodS != 0 && s.MaxPeriodS != 0 && s.MaxPeriodS <= s.MinPeriodS {
		n.addf("%smaxPeriodS %g is not above minPeriodS %g; the default is used", p, s.MaxPeriodS, s.MinPeriodS)
		s.MaxPeriodS = 0
	}

	var sites []SSRSite
	for i, x := range s.Sites {
		x.Name = strings.TrimSpace(x.Name)
		bad := math.IsNaN(x.Lat) || math.IsNaN(x.Lon) || x.Lat < -90 || x.Lat > 90 || x.Lon < -180 || x.Lon > 180 || (x.Lat == 0 && x.Lon == 0)
		if bad {
			n.addf("%ssites[%d] position %g, %g is not a valid latitude/longitude; the site is ignored", p, i, x.Lat, x.Lon)
			continue
		}
		if math.IsNaN(x.HeightM) || x.HeightM < -500 || x.HeightM > 9000 {
			n.addf("%ssites[%d].heightM %g is out of range -500..9000; 0 is used", p, i, x.HeightM)
			x.HeightM = 0
		}
		if x.SigmaHM < 0 || x.SigmaHM > 1e6 || math.IsNaN(x.SigmaHM) {
			n.addf("%ssites[%d].sigmaHM %g is out of range 0..1000000; the default is used", p, i, x.SigmaHM)
			x.SigmaHM = 0
		}
		if x.SigmaUM < 0 || x.SigmaUM > 1e6 || math.IsNaN(x.SigmaUM) {
			n.addf("%ssites[%d].sigmaUM %g is out of range 0..1000000; the default is used", p, i, x.SigmaUM)
			x.SigmaUM = 0
		}
		sites = append(sites, x)
	}
	s.Sites = sites

	var codes []string
	for i, c := range s.ConspicuityCodes {
		v, ok := ParseOctalCode(c)
		if !ok {
			n.addf("%sconspicuityCodes[%d] %q is not a 4-digit octal code; ignored", p, i, c)
			continue
		}
		codes = append(codes, strconv.FormatUint(uint64(v)|0o10000, 8)[1:])
	}
	s.ConspicuityCodes = codes
	return n
}
