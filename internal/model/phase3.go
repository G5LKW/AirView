package model

import (
	"slices"
	"strings"
)

// UpdateSettings is the signed self-update block ("update") of airview and
// airview-feeder. It mirrors internal/selfupdate.Settings field for field
// (model is a leaf package; the wiring converts with a field copy).
// Self-update is opt-in for airview; airview-feeder maps its autoUpdate
// switch onto Enabled when a manifest URL and a key are set.
type UpdateSettings struct {
	// Enabled turns on the periodic automatic check-and-install (default
	// false for airview; the manual "update" command works whenever a
	// manifest URL and a key are configured).
	Enabled bool `json:"enabled"`
	// Channel is "stable" (default) or "beta".
	Channel string `json:"channel"`
	// ManifestURL is the URL of the signed release manifest, e.g.
	// https://aggregator.example/feeder/manifest.json ("" = no updates;
	// "{channel}" is replaced by the channel).
	ManifestURL string `json:"manifestUrl"`
	// Keys are the trusted Ed25519 release public keys (base64 of the raw
	// 32 bytes; several allow key rotation). Public keys, not secrets.
	Keys []string `json:"keys"`
	// CheckIntervalMin is the automatic check period in minutes (default
	// 1440, 15..44640; jittered by ±25 %).
	CheckIntervalMin int `json:"checkIntervalMin"`
	// HealthWindowS is how long a new version must stay healthy after the
	// restart before it is kept (default 60, 10..900 s).
	HealthWindowS int `json:"healthWindowS"`
	// MaxDownloadMB bounds a release download (default 256, 1..4096).
	MaxDownloadMB int `json:"maxDownloadMB"`
	// HealthURL is polled during the health window ("" = the program's own
	// health endpoint).
	HealthURL string `json:"healthUrl"`
}

// DefaultUpdateSettings returns the defaults (disabled, stable channel).
func DefaultUpdateSettings() UpdateSettings {
	return UpdateSettings{Channel: "stable", Keys: []string{}, CheckIntervalMin: 1440, HealthWindowS: 60, MaxDownloadMB: 256}
}

// Clone deep-copies the key list.
func (u UpdateSettings) Clone() UpdateSettings {
	if u.Keys != nil {
		u.Keys = slices.Clone(u.Keys)
	}
	return u
}

// Normalize corrects out-of-range values in place (path is the block's
// prefix, "update" or "update" of feeder.json) and returns notes.
func (u *UpdateSettings) Normalize(path string) []string {
	var n notes
	d := DefaultUpdateSettings()
	u.Channel = strings.ToLower(strings.TrimSpace(u.Channel))
	switch u.Channel {
	case "":
		u.Channel = d.Channel
	case "stable", "beta":
	default:
		n.addf("%s.channel %q is not stable or beta; stable is used", path, u.Channel)
		u.Channel = d.Channel
	}
	u.ManifestURL = strings.TrimSpace(u.ManifestURL)
	u.HealthURL = strings.TrimSpace(u.HealthURL)
	keys := make([]string, 0, len(u.Keys))
	for _, k := range u.Keys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	u.Keys = keys
	u.CheckIntervalMin = zeroDefRange(&n, path+".checkIntervalMin", u.CheckIntervalMin, 15, 44640, d.CheckIntervalMin)
	u.HealthWindowS = zeroDefRange(&n, path+".healthWindowS", u.HealthWindowS, 10, 900, d.HealthWindowS)
	u.MaxDownloadMB = zeroDefRange(&n, path+".maxDownloadMB", u.MaxDownloadMB, 1, 4096, d.MaxDownloadMB)
	return n
}

// zeroDefRange: 0 means the default; a value outside lo..hi is reported and
// replaced by the default.
func zeroDefRange(n *notes, path string, v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo || v > hi {
		n.addf("%s %d is out of range %d..%d; %d is used", path, v, lo, hi, def)
		return def
	}
	return v
}

// LocateConfig controls Locate Me v2 (internal/analytics locate2): the
// guess samples the tracker records so the receiver position can be
// estimated from what it hears.
type LocateConfig struct {
	// Enabled records guess samples and runs Locate Me v2 (default true).
	Enabled bool `json:"enabled"`
	// RetentionDays keeps guess samples this long (default 7, 1..30).
	RetentionDays int `json:"retentionDays"`
	// MaxRowsPerStation caps one station's guess samples (default
	// 2000000, 10000..20000000).
	MaxRowsPerStation int `json:"maxRowsPerStation"`
	// MaxRowsTotal caps all guess samples of a database (default
	// 10000000, 100000..200000000).
	MaxRowsTotal int `json:"maxRowsTotal"`
}

// DefaultLocateConfig returns the Locate Me v2 defaults.
func DefaultLocateConfig() LocateConfig {
	return LocateConfig{Enabled: true, RetentionDays: 7, MaxRowsPerStation: 2_000_000, MaxRowsTotal: 10_000_000}
}

// Normalize corrects out-of-range values in place and returns notes.
func (l *LocateConfig) Normalize() []string {
	var n notes
	d := DefaultLocateConfig()
	l.RetentionDays = zeroDefRange(&n, "locate.retentionDays", l.RetentionDays, 1, 30, d.RetentionDays)
	l.MaxRowsPerStation = zeroDefRange(&n, "locate.maxRowsPerStation", l.MaxRowsPerStation, 10_000, 20_000_000, d.MaxRowsPerStation)
	l.MaxRowsTotal = zeroDefRange(&n, "locate.maxRowsTotal", l.MaxRowsTotal, 100_000, 200_000_000, d.MaxRowsTotal)
	return n
}

// AggMLATSurvey tunes the reverse-MLAT station survey
// (internal/mlat/survey, DESIGN §10.2-10.3). Zero = the package default.
type AggMLATSurvey struct {
	// Disabled turns the survey off (default false).
	Disabled bool `json:"disabled"`
	// MaxJobs bounds the concurrently collecting surveys (default 64,
	// 1..4096).
	MaxJobs int `json:"maxJobs"`
	// MaxCheckFrac bounds the periodic checks of located stations to this
	// share of the stations (default 0.1, 0.001..1).
	MaxCheckFrac float64 `json:"maxCheckFrac"`
	// MinDurationSec is the collection time before the first evaluation
	// (default 180, 30..21600 s).
	MinDurationSec int `json:"minDurationSec"`
	// EvalEverySec is the evaluation period after that (default 60,
	// 5..3600 s).
	EvalEverySec int `json:"evalEverySec"`
	// MaxDurationMin ends a survey (default 30, 1..1440 min).
	MaxDurationMin int `json:"maxDurationMin"`
	// NoDataAfterMin ends a survey that collected almost nothing (default
	// 10, 1..1440 min).
	NoDataAfterMin int `json:"noDataAfterMin"`
	// RetryAfterMin is the pause after an unsuccessful survey of an
	// unlocated station (default 30, 1..43200 min; doubles up to
	// surveyIntervalH).
	RetryAfterMin int `json:"retryAfterMin"`
	// OwnerMinIntervalMin rate-limits owner-started surveys per station
	// (default 60, up to 10080 min; negative = no limit).
	OwnerMinIntervalMin int `json:"ownerMinIntervalMin"`
	// MaxObs bounds a survey's observations (default 15000, 500..200000).
	MaxObs int `json:"maxObs"`
	// MinIntervalMs thins observations to one per aircraft per interval
	// (default 1000, 100..60000 ms).
	MinIntervalMs int `json:"minIntervalMs"`
	// MinPeers is the number of synced, located peers that fix a
	// transmission's emission time (default 2, 1..16).
	MinPeers int `json:"minPeers"`
	// MaxPeerSigmaNs drops peers with a noisier network time (default 600,
	// 20..10000 ns).
	MaxPeerSigmaNs float64 `json:"maxPeerSigmaNs"`
	// MaskDeg is the elevation mask (default 2.5, up to 15 degrees).
	MaskDeg float64 `json:"maskDeg"`
	// HeightSigmaM is the 1-sigma of the terrain + antenna height prior
	// (default 20, 1..1000 m).
	HeightSigmaM float64 `json:"heightSigmaM"`
	// DefaultAglM is the antenna height above ground when none is declared
	// (default 10, up to 500 m).
	DefaultAglM float64 `json:"defaultAglM"`
	// SearchRadiusKm is the seed grid's radius around the declared or
	// Locate Me position (default 50, 1..1000 km).
	SearchRadiusKm float64 `json:"searchRadiusKm"`
	// StableSigmaGpsM is the horizontal sigma a GPS-timed station's survey
	// must reach to be stable (default 30, 1..1000 m).
	StableSigmaGpsM float64 `json:"stableSigmaGpsM"`
	// StableSigmaM is the same limit for other clock classes (default 200,
	// 1..5000 m).
	StableSigmaM float64 `json:"stableSigmaM"`
	// MinAzSpreadDeg is the azimuth spread of reference aircraft a stable
	// survey needs (default 180, up to 360 degrees).
	MinAzSpreadDeg float64 `json:"minAzSpreadDeg"`
	// MinAircraft is the number of distinct reference aircraft a stable
	// survey needs (default 10, 2..10000).
	MinAircraft int `json:"minAircraft"`
	// SuspectMinM: a declared position is suspect when it is more than
	// max(suspectMinM, suspectK x sigma) from the survey (default 100,
	// 1..100000 m).
	SuspectMinM float64 `json:"suspectMinM"`
	// SuspectK is that sigma factor (default 3, 1..20).
	SuspectK float64 `json:"suspectK"`
	// Bundle tunes the hourly network bundle adjustment (zero = default).
	Bundle AggMLATBundle `json:"bundle"`
}

func zeroOrRangeI(n *notes, path string, v *int, lo, hi int) {
	if *v != 0 && (*v < lo || *v > hi) {
		n.addf("%s %d is out of range %d..%d; the default is used", path, *v, lo, hi)
		*v = 0
	}
}

func zeroOrRangeF(n *notes, path string, v *float64, lo, hi float64) {
	if *v != 0 && (*v < lo || *v > hi || *v != *v) {
		n.addf("%s %g is out of range %g..%g; the default is used", path, *v, lo, hi)
		*v = 0
	}
}

// normalizeSurveyTuning checks aggregator.mlat.survey.
func (a *Aggregator) normalizeSurveyTuning(n *notes) {
	s := &a.MLAT.Survey
	p := "aggregator.mlat.survey."
	zeroOrRangeI(n, p+"maxJobs", &s.MaxJobs, 1, 4096)
	zeroOrRangeF(n, p+"maxCheckFrac", &s.MaxCheckFrac, 0.001, 1)
	zeroOrRangeI(n, p+"minDurationSec", &s.MinDurationSec, 30, 21600)
	zeroOrRangeI(n, p+"evalEverySec", &s.EvalEverySec, 5, 3600)
	zeroOrRangeI(n, p+"maxDurationMin", &s.MaxDurationMin, 1, 1440)
	zeroOrRangeI(n, p+"noDataAfterMin", &s.NoDataAfterMin, 1, 1440)
	zeroOrRangeI(n, p+"retryAfterMin", &s.RetryAfterMin, 1, 43200)
	if s.OwnerMinIntervalMin > 10080 {
		n.addf("%sownerMinIntervalMin %d is out of range (at most 10080); the default is used", p, s.OwnerMinIntervalMin)
		s.OwnerMinIntervalMin = 0
	}
	if s.OwnerMinIntervalMin < 0 {
		s.OwnerMinIntervalMin = -1 // any negative value = no limit
	}
	zeroOrRangeI(n, p+"maxObs", &s.MaxObs, 500, 200000)
	zeroOrRangeI(n, p+"minIntervalMs", &s.MinIntervalMs, 100, 60000)
	zeroOrRangeI(n, p+"minPeers", &s.MinPeers, 1, 16)
	zeroOrRangeF(n, p+"maxPeerSigmaNs", &s.MaxPeerSigmaNs, 20, 10000)
	zeroOrRangeF(n, p+"maskDeg", &s.MaskDeg, 0, 15)
	zeroOrRangeF(n, p+"heightSigmaM", &s.HeightSigmaM, 1, 1000)
	zeroOrRangeF(n, p+"defaultAglM", &s.DefaultAglM, 0, 500)
	zeroOrRangeF(n, p+"searchRadiusKm", &s.SearchRadiusKm, 1, 1000)
	zeroOrRangeF(n, p+"stableSigmaGpsM", &s.StableSigmaGpsM, 1, 1000)
	zeroOrRangeF(n, p+"stableSigmaM", &s.StableSigmaM, 1, 5000)
	zeroOrRangeF(n, p+"minAzSpreadDeg", &s.MinAzSpreadDeg, 0, 360)
	zeroOrRangeI(n, p+"minAircraft", &s.MinAircraft, 2, 10000)
	zeroOrRangeF(n, p+"suspectMinM", &s.SuspectMinM, 1, 100000)
	zeroOrRangeF(n, p+"suspectK", &s.SuspectK, 1, 20)
}
