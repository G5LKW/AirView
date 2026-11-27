package store

import (
	"testing"
	"time"

	"airview/internal/model"
)

func TestTierPolicyFromConfig(t *testing.T) {
	a := model.DefaultAggregator()
	p := TierPolicyFromConfig(a)
	d := DefaultTierPolicy()
	// The defaults of the config block reproduce DefaultTierPolicy (minute
	// and hour tiers from aggregator.retention: 30 days, 13 x 30 days).
	d.HourDays = 30 * a.Retention.HourMonths
	// aggregator.storage.optimizeHours: 1 h (1.1, analysis_limit bounds the
	// cost); a config package that still has the 1.0.0 default of 24 h
	// passes it through unchanged.
	if a.Storage.OptimizeHours == 24 {
		d.OptimizeEvery = 24 * time.Hour
	}
	if p != d {
		t.Fatalf("policy %+v, want %+v", p, d)
	}
	a.Storage.MinuteDays, a.Storage.HourDays, a.Storage.WALCheckpointMB, a.Storage.OptimizeHours = 7, 100, 8, 2
	p = TierPolicyFromConfig(a)
	if p.MinuteDays != 7 || p.HourDays != 100 || p.WALCheckpointBytes != 8<<20 || p.OptimizeEvery != 2*time.Hour {
		t.Fatalf("explicit %+v", p)
	}
}
