package group

import (
	"testing"

	"airview/internal/model"
)

func TestConfigFromModel(t *testing.T) {
	if got := ConfigFromModel(model.AggMLATGroup{}); got != DefaultConfig().Normalize() {
		t.Fatalf("zero block: %+v", got)
	}
	c := ConfigFromModel(model.AggMLATGroup{NSigma: 5, LateHoldMs: 1500, MinSites: 20})
	if c.NSigma != 5 || c.LateHoldNs != 1.5e9 || c.MinSites != 8 || c.SlackM != 1000 {
		t.Fatalf("%+v", c)
	}
}
