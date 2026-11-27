package plan

import (
	"testing"
	"time"

	"airview/internal/model"
)

func TestConfigFromModel(t *testing.T) {
	if got := ConfigFromModel(model.AggMLAT{}); got != DefaultConfig().Normalize() {
		t.Fatalf("zero block: %+v", got)
	}
	c := ConfigFromModel(model.AggMLAT{ModeAC: true, Plan: model.AggMLATPlan{SyncSetMax: 40, HeardTTLS: 90, GlobalSolveHz: 5000}})
	if !c.ModeAC.Enabled || c.SyncSetMax != 40 || c.HeardTTL != 90*time.Second || c.GlobalSolveHz != 5000 || c.SyncSetMin != 15 {
		t.Fatalf("%+v", c)
	}
}
