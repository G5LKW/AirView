package clock

import (
	"reflect"
	"testing"

	"airview/internal/model"
)

// TestConfigFromModel: a non-zero value of every field of the file block
// arrives in the clock.Config field of the same name; zero keeps base.
func TestConfigFromModel(t *testing.T) {
	var m model.AggMLATClock
	mv := reflect.ValueOf(&m).Elem()
	for i := 0; i < mv.NumField(); i++ {
		switch fv := mv.Field(i); fv.Kind() {
		case reflect.Float64:
			fv.SetFloat(float64(i) + 1.25)
		case reflect.Int:
			fv.SetInt(int64(i) + 3)
		case reflect.Bool:
			fv.SetBool(true)
		}
	}
	c := ConfigFromModel(m, Config{EpochS: 0.5})
	cv := reflect.ValueOf(c)
	for i := 0; i < mv.NumField(); i++ {
		name := mv.Type().Field(i).Name
		got := cv.FieldByName(name)
		if !got.IsValid() {
			t.Errorf("%s: no clock.Config field of that name", name)
			continue
		}
		if !reflect.DeepEqual(got.Interface(), mv.Field(i).Interface()) {
			t.Errorf("%s: got %v, want %v", name, got.Interface(), mv.Field(i).Interface())
		}
	}
	if c.EpochS != 0.5 {
		t.Fatal("base field lost")
	}
	z := ConfigFromModel(model.AggMLATClock{}, Config{HostSigmaS: 0.07})
	if z.HostSigmaS != 0.07 || z.StepRunCount != 0 {
		t.Fatalf("zero block changed base: %+v", z)
	}
	d := Config{StepRunCount: -1, OriginUnixNs: 1}
	if err := d.normalize(); err != nil || d.StepRunCount != -1 {
		t.Fatalf("StepRunCount -1 (off) became %d", d.StepRunCount)
	}
}
