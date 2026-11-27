package model

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// SplitSyncSetting is the file form of the mlat-client split_sync policy
// (aggregator.mlatc.splitSync): "auto", "on" or "off" ("" = auto). The
// boolean form of earlier versions is still accepted when decoding: true
// means "on", false "auto" (false could never be told apart from unset).
type SplitSyncSetting string

// Split-sync settings.
const (
	SplitSyncAutoSetting SplitSyncSetting = "auto"
	SplitSyncOnSetting   SplitSyncSetting = "on"
	SplitSyncOffSetting  SplitSyncSetting = "off"
)

// UnmarshalJSON accepts a string or the legacy boolean.
func (s *SplitSyncSetting) UnmarshalJSON(b []byte) error {
	var v bool
	if err := json.Unmarshal(b, &v); err == nil {
		if v {
			*s = SplitSyncOnSetting
		} else {
			*s = SplitSyncAutoSetting
		}
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	*s = SplitSyncSetting(strings.ToLower(strings.TrimSpace(str)))
	return nil
}

// AggMLATQuarantine is the timing quarantine of new stations
// (internal/registry/trust, H-mlatc): a new station's copies reach MLAT
// only after its timing agreed with trusted references. Zero = default.
type AggMLATQuarantine struct {
	// Disabled admits new stations to MLAT without proving their timing
	// (default false: new stations start in timing quarantine).
	Disabled bool `json:"disabled"`
	// MinRefs is the number of distinct reference stations a station's
	// timing must agree with (default 2).
	MinRefs int `json:"minRefs"`
	// WindowSec is the length of one fit window per station pair (default
	// 30 s).
	WindowSec int `json:"windowSec"`
	// MaxSigmaNs is the floor of the per-pair residual threshold (default
	// 1500 ns).
	MaxSigmaNs float64 `json:"maxSigmaNs"`
	// GoodWindows good windows prove the timing (default 8).
	GoodWindows int `json:"goodWindows"`
	// MaxBadFrac is the largest share of judged references with as many
	// bad windows as good ones (default 0.2, up to 1).
	MaxBadFrac float64 `json:"maxBadFrac"`
	// MinAgeSec is the time from the first comparison before a proof
	// (default 180 s).
	MinAgeSec int `json:"minAgeSec"`
}

// AggPositions rate-limits and holds the station positions that feeders
// announce themselves (AVF RECEIVER records, mlat-client handshakes;
// finding PRIV-1). Zero = default.
type AggPositions struct {
	// JitterM: changes smaller than this are ignored (default 30 m).
	JitterM float64 `json:"jitterM"`
	// MinIntervalHours: at most one applied change per station per this
	// interval (default 24 h).
	MinIntervalHours float64 `json:"minIntervalHours"`
	// HoldBeyondM: moves at least this large wait for the survey, an owner
	// confirmation or holdMaxDays (default 1000 m).
	HoldBeyondM float64 `json:"holdBeyondM"`
	// HoldMaxDays releases a held move announced continuously this long
	// (default 7; negative = never).
	HoldMaxDays float64 `json:"holdMaxDays"`
	// SurveyMatchM: a survey estimate within max(surveyMatchM, 3σ) of a held
	// position confirms it (default 250 m).
	SurveyMatchM float64 `json:"surveyMatchM"`
}

// normalizeHardenTuning checks the settings added after the hardening wave.
func (a *Aggregator) normalizeHardenTuning(n *notes) {
	if k := strings.TrimSpace(a.FeederManifestKey); k != "" {
		if b, err := base64.StdEncoding.DecodeString(k); err != nil || len(b) != 32 {
			n.addf("aggregator.feederManifestKey is not the base64 of a 32-byte Ed25519 public key; it is ignored")
			a.FeederManifestKey = ""
		} else {
			a.FeederManifestKey = k
		}
	}
	a.FeederReleaseDir = strings.TrimSpace(a.FeederReleaseDir)

	c := &a.MLATC
	switch c.SplitSync {
	case "", SplitSyncAutoSetting, SplitSyncOnSetting, SplitSyncOffSetting:
	case "true":
		c.SplitSync = SplitSyncOnSetting
	case "false":
		c.SplitSync = SplitSyncAutoSetting
	default:
		n.addf("aggregator.mlatc.splitSync %q is not auto, on or off; auto is used", string(c.SplitSync))
		c.SplitSync = SplitSyncAutoSetting
	}

	q := &a.MLAT.Quarantine
	p := "aggregator.mlat.quarantine."
	nonNegI(n, p+"minRefs", &q.MinRefs)
	nonNegI(n, p+"windowSec", &q.WindowSec)
	nonNegI(n, p+"goodWindows", &q.GoodWindows)
	nonNegI(n, p+"minAgeSec", &q.MinAgeSec)
	nonNegF(n, p+"maxSigmaNs", &q.MaxSigmaNs)
	nonNegF(n, p+"maxBadFrac", &q.MaxBadFrac)
	if q.MaxBadFrac > 1 {
		n.addf("%smaxBadFrac %g is out of range 0..1; the default is used", p, q.MaxBadFrac)
		q.MaxBadFrac = 0
	}

	ps := &a.Positions
	p = "aggregator.positions."
	nonNegF(n, p+"jitterM", &ps.JitterM)
	nonNegF(n, p+"minIntervalHours", &ps.MinIntervalHours)
	nonNegF(n, p+"holdBeyondM", &ps.HoldBeyondM)
	nonNegF(n, p+"surveyMatchM", &ps.SurveyMatchM)

	ig := &a.Ingest
	p = "aggregator.ingest."
	if ig.ClockGateFloorUs < -1 || ig.ClockGateFloorUs > 100000 {
		n.addf("%sclockGateFloorUs %d is out of range -1..100000; the default is used", p, ig.ClockGateFloorUs)
		ig.ClockGateFloorUs = 0
	}
	if ig.FlushSampleEvery < 0 || ig.FlushSampleEvery > 1000 {
		n.addf("%sflushSampleEvery %d is out of range 0..1000; the default is used", p, ig.FlushSampleEvery)
		ig.FlushSampleEvery = 0
	}
	if ig.AVFHostSlackMs < -1 || ig.AVFHostSlackMs > 60000 {
		n.addf("%savfHostSlackMs %d is out of range -1..60000; the default is used", p, ig.AVFHostSlackMs)
		ig.AVFHostSlackMs = 0
	}

	ck := &a.MLAT.Clock
	p = "aggregator.mlat.clock."
	if ck.StepRunCount < -1 {
		n.addf("%sstepRunCount %d is below -1 (off); the default is used", p, ck.StepRunCount)
		ck.StepRunCount = 0
	}
	if ck.RelGaugeBetaSigmaNs < 0 {
		ck.RelGaugeBetaSigmaNs = -1 // any negative value = off
	}
	nonNegF(n, p+"reoriginS", &ck.ReoriginS)
	nonNegF(n, p+"externalInflate", &ck.ExternalInflate)
	if ck.ReoriginS != 0 && ck.ReoriginS < 10 {
		n.addf("%sreoriginS %g is below 10; 10 is used", p, ck.ReoriginS)
		ck.ReoriginS = 10
	}
	if ck.ExternalInflate != 0 && ck.ExternalInflate < 1 {
		n.addf("%sexternalInflate %g is below 1; 1 is used", p, ck.ExternalInflate)
		ck.ExternalInflate = 1
	}

	r := &a.MLAT.Ref
	p = "aggregator.mlat.ref."
	nonNegF(n, p+"altReplyMinS", &r.AltReplyMinS)
	nonNegF(n, p+"pairBaroS", &r.PairBaroS)
	nonNegF(n, p+"field.profileSigmaM", &r.Field.ProfileSigmaM)
	nonNegF(n, p+"field.profileSigmaTK", &r.Field.ProfileSigmaTK)
}
