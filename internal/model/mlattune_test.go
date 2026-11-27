package model

import (
	"encoding/json"
	"testing"
)

func TestMLATTuningBlocks(t *testing.T) {
	a := DefaultAggregator()
	if a.MLAT.Ref != (AggMLATRef{}) || a.MLAT.Track != (AggMLATTrack{}) || a.MLATC != (AggMLATC{}) {
		t.Fatal("tuning blocks must default to zero (= package defaults)")
	}
	if a.Storage.TrailFullDays != 2 || a.Storage.TrailThinSec != 30 {
		t.Fatalf("trail tiers %+v", a.Storage)
	}
	var b Aggregator
	if err := json.Unmarshal([]byte(`{"storage":{"trailFullDays":0,"trailThinSec":1},
		"mlat":{"ref":{"window":20,"minNacp":12,"field":{"cellKm":-5}},"solve":{"minHaeM":-800,"pfa":0.7},
		"track":{"maxExclude":-1,"gateProb":1.5},"plan":{"maxChurn":-2}},"mlatc":{"splitSync":true,"statsSec":-1}}`), &b); err != nil {
		t.Fatal(err)
	}
	n := b.Normalize()
	if b.MLAT.Ref.Window != 20 || b.MLAT.Ref.MinNACp != 0 || b.MLAT.Ref.Field.CellKm != 0 {
		t.Fatalf("ref %+v", b.MLAT.Ref)
	}
	if b.MLAT.Solve.MinHAEM != -800 || b.MLAT.Solve.PFA != 0 || b.MLAT.Track.MaxExclude != -1 || b.MLAT.Track.GateProb != 0 {
		t.Fatalf("solve %+v track %+v", b.MLAT.Solve, b.MLAT.Track)
	}
	if b.MLAT.Plan.MaxChurn != 0 || b.MLATC.SplitSync != SplitSyncOnSetting || b.MLATC.StatsSec != 0 {
		t.Fatalf("plan %+v mlatc %+v", b.MLAT.Plan, b.MLATC)
	}
	if b.Storage.TrailFullDays != 0 || b.Storage.TrailThinSec != 30 {
		t.Fatalf("trail %+v", b.Storage)
	}
	// minNacp, cellKm, pfa, gateProb, maxChurn, statsSec, trailThinSec.
	if len(n) != 7 {
		t.Fatalf("notes %d: %v", len(n), n)
	}
}

func TestMLATEngineBlock(t *testing.T) {
	if DefaultAggregator().MLAT.Engine != (AggMLATEngine{}) {
		t.Fatal("engine block must default to zero")
	}
	var b Aggregator
	if err := json.Unmarshal([]byte(`{"mlat":{"engine":{"workers":200,"queueLen":-3,"publishIntervalMs":250,
		"verifyEverySec":-1,"syncWarmupSec":-7,"latencyFeedback":true}}}`), &b); err != nil {
		t.Fatal(err)
	}
	n := b.Normalize()
	e := b.MLAT.Engine
	if e.Workers != 64 || e.QueueLen != 0 || e.PublishIntervalMs != 250 || e.VerifyEverySec != -1 ||
		e.SyncWarmupSec != 0 || !e.LatencyFeedback {
		t.Fatalf("engine %+v", e)
	}
	if len(n) != 3 { // workers, queueLen, syncWarmupSec
		t.Fatalf("notes %d: %v", len(n), n)
	}
}

func TestMLATClockBlock(t *testing.T) {
	if DefaultAggregator().MLAT.Clock != (AggMLATClock{}) {
		t.Fatal("clock block must default to zero")
	}
	var b Aggregator
	if err := json.Unmarshal([]byte(`{"mlat":{"clock":{"hostSigmaS":0.1,"ppsCorrelatedNs":-1,"maxMembers":2,
		"gateProb":1,"blacklistMaxFrac":1,"stationBadOutlier":1.5,"syncRatePerStation":150,"workers":99,
		"stepCount":-4,"probationS":-1,"syncedMinObs":7,"noJitterLearning":true}}}`), &b); err != nil {
		t.Fatal(err)
	}
	n := b.Normalize()
	c := b.MLAT.Clock
	if c.HostSigmaS != 0.1 || c.PPSCorrelatedNs != -1 || c.MaxMembers != 0 || c.GateProb != 0 || c.BlacklistMaxFrac != 1 ||
		c.StationBadOutlier != 0 || c.SyncRatePerStation != 0 || c.Workers != 64 || c.StepCount != 0 || c.ProbationS != 0 ||
		c.SyncedMinObs != 7 || !c.NoJitterLearning {
		t.Fatalf("clock %+v", c)
	}
	// maxMembers, gateProb, stationBadOutlier, syncRatePerStation, workers, stepCount, probationS.
	if len(n) != 7 {
		t.Fatalf("notes %d: %v", len(n), n)
	}
}
