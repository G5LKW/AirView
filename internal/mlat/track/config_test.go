package track

import (
	"testing"

	"airview/internal/model"
)

func TestParamsFromConfig(t *testing.T) {
	if got, want := ParamsFromConfig(model.AggMLATTrack{}).Normalize(), DefaultParams().Normalize(); got != want {
		t.Fatalf("zero block: %+v", got)
	}
	p := ParamsFromConfig(model.AggMLATTrack{MaxExclude: -1, SyncTauS: 30, MaxHyp: 2}).Normalize()
	if p.MaxExclude != 0 || p.SyncTauS != 30 || p.MaxHyp != 2 || p.QCV != 0.3 {
		t.Fatalf("%+v", p)
	}
}
