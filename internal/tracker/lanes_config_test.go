package tracker

import (
	"testing"
	"time"

	"airview/internal/model"
)

func TestLaneOptionsFromConfig(t *testing.T) {
	a := model.DefaultAggregator()
	o := LaneOptionsFromConfig(a)
	if o.MaxRangeKm != 800 || o.FlushEvery != 10*time.Second || o.SlowFlushEvery != 5*time.Minute || o.EvictAfter != 24*time.Hour {
		t.Fatalf("defaults %+v", o)
	}
	a.Lanes = model.AggLanes{FlushSec: 5, SlowFlushSec: 120, EvictHours: 2, MaxRangeKm: 400}
	o = LaneOptionsFromConfig(a)
	if o.MaxRangeKm != 400 || o.FlushEvery != 5*time.Second || o.SlowFlushEvery != 2*time.Minute || o.EvictAfter != 2*time.Hour {
		t.Fatalf("custom %+v", o)
	}
}
