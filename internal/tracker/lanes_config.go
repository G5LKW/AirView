package tracker

import (
	"time"

	"airview/internal/model"
)

// LaneOptionsFromConfig maps the aggregator.lanes block (DESIGN §20.3) onto
// LaneOptions. Resolver, Store and OnFlightStation are left for the caller;
// zero fields keep the package defaults. The same MaxRangeKm should be passed
// as Options.MaxRangeKm (the aggregator range gate).
func LaneOptionsFromConfig(a model.Aggregator) LaneOptions {
	l := a.Lanes
	return LaneOptions{
		MaxRangeKm:     l.MaxRangeKm,
		FlushEvery:     time.Duration(l.FlushSec) * time.Second,
		SlowFlushEvery: time.Duration(l.SlowFlushSec) * time.Second,
		EvictAfter:     time.Duration(l.EvictHours) * time.Hour,
	}
}
