package solve

import (
	"testing"

	"airview/internal/model"
)

func TestOptionsFromConfig(t *testing.T) {
	if got, want := OptionsFromConfig(model.AggMLATSolve{}).Defaults(), (Options{}).Defaults(); got != want {
		t.Fatalf("zero block: %+v", got)
	}
	o := OptionsFromConfig(model.AggMLATSolve{MinHAEM: -800, MaxIter: 15, AlwaysAlternates: true}).Defaults()
	if o.MinHAE != -800 || o.MaxIter != 15 || !o.AlwaysAlternates || o.HuberK != 1.345 {
		t.Fatalf("%+v", o)
	}
}
