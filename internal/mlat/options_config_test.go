package mlat

import (
	"testing"
	"time"

	"airview/internal/model"
)

func TestOptionsFromEngineBlock(t *testing.T) {
	var o Options
	o.Config = model.DefaultAggregator().MLAT
	d := o.withDefaults()
	if d.QueueLen != 4096 || d.PublishInterval != 500*time.Millisecond || d.VerifyEvery != 5*time.Second ||
		d.SyncWarmup != time.Minute || d.LatencyFeedback {
		t.Fatalf("defaults %+v", d)
	}
	o.Config.Engine = model.AggMLATEngine{Workers: 3, QueueLen: 100, PublishIntervalMs: 250, MaxSolveSites: 8,
		ContribWindowSec: 30, MinTimedPerMin: 4, HoldoutRotateMin: 5, LatencyFeedback: true, JitterFeedback: true,
		ApplyKAtm: true, VerifyEverySec: -1, SyncWarmupSec: -1}
	c := o.withDefaults()
	if c.Workers != 3 || c.QueueLen != 100 || c.PublishInterval != 250*time.Millisecond || c.MaxSolveSites != 8 ||
		c.ContribWindow != 30*time.Second || c.MinTimedPerMin != 4 || c.HoldoutRotate != 5*time.Minute ||
		!c.LatencyFeedback || !c.JitterFeedback || !c.ApplyKAtm || c.VerifyEvery >= 0 || c.SyncWarmup != 0 {
		t.Fatalf("mapped %+v", c)
	}
	// Explicit options win over the file.
	o.Workers, o.QueueLen = 2, 50
	if e := o.withDefaults(); e.Workers != 2 || e.QueueLen != 50 {
		t.Fatalf("explicit %+v", e)
	}
}
