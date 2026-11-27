package ref

import (
	"testing"

	"airview/internal/model"
)

func TestOptionsFromConfig(t *testing.T) {
	if got, want := OptionsFromConfig(model.AggMLATRef{}).withDefaults(), (Options{}).withDefaults(); got != want {
		t.Fatalf("zero block: %+v", got)
	}
	o := OptionsFromConfig(model.AggMLATRef{Window: 24, MinNACp: 9, Field: model.AggMLATField{CellKm: 25}}).withDefaults()
	if o.Window != 24 || o.MinNACp != 9 || o.MinNIC != 7 || o.Field.CellKm != 25 || o.Field.BandFt != 2000 {
		t.Fatalf("%+v", o)
	}
}
