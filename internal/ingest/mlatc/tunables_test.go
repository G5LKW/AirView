package mlatc

import (
	"testing"
	"time"

	"airview/internal/model"
)

func TestOptionsFromConfigMLATCBlock(t *testing.T) {
	a := model.DefaultAggregator()
	o := OptionsFromConfig(a).normalized()
	if o.IdleTimeout != 150*time.Second || o.MaxOutQueue != 256<<10 || o.SplitSync != SplitSyncAuto || o.Policy.normalized().MaxRequested != 2000 {
		t.Fatalf("defaults: %+v", o)
	}
	a.MLATC = model.AggMLATC{SplitSync: model.SplitSyncOnSetting, IdleTimeoutSec: 200, StatsSec: 30, MaxOutQueueKB: 512, MaxRequested: 500, CoolDownSec: 60, MaxClients: 10}
	o = OptionsFromConfig(a).normalized()
	p := o.Policy.normalized()
	if o.SplitSync != SplitSyncOn || o.IdleTimeout != 200*time.Second || o.StatsEvery != 30*time.Second || o.MaxOutQueue != 512<<10 ||
		o.HeartbeatEvery != 30*time.Second || p.MaxRequested != 500 || p.CoolDown != time.Minute || p.MaxClients != 10 {
		t.Fatalf("mapped: %+v %+v", o, p)
	}
}

// TestOptionsFromConfigSplitSyncOff: the tri-state file setting can turn
// split sync off, and the legacy boolean still decodes.
func TestOptionsFromConfigSplitSyncOff(t *testing.T) {
	a := model.DefaultAggregator()
	a.MLATC.SplitSync = model.SplitSyncOffSetting
	if o := OptionsFromConfig(a); o.SplitSync != SplitSyncOff {
		t.Fatalf("off: %v", o.SplitSync)
	}
	for in, want := range map[string]SplitSyncMode{`true`: SplitSyncOn, `false`: SplitSyncAuto, `"off"`: SplitSyncOff, `"auto"`: SplitSyncAuto} {
		var b model.Aggregator
		if err := b.UnmarshalJSON([]byte(`{"mlatc":{"splitSync":` + in + `}}`)); err != nil {
			t.Fatal(err)
		}
		b.Normalize()
		if o := OptionsFromConfig(b); o.SplitSync != want {
			t.Fatalf("%s: %v, want %v", in, o.SplitSync, want)
		}
	}
}
