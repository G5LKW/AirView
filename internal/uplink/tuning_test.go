package uplink

import (
	"testing"

	"airview/internal/model"
)

func TestApplyTuningDefaultsMatch(t *testing.T) {
	var o Options
	o.ApplyTuning(model.DefaultUplinkTuning())
	var d Options
	d.normalize()
	o.normalize()
	if o.RingAge != d.RingAge || o.RingBytes != d.RingBytes || o.BackfillBytes != d.BackfillBytes ||
		o.ResumeWindow != d.ResumeWindow || o.BackfillMinBps != d.BackfillMinBps || o.BackoffMin != d.BackoffMin ||
		o.BackoffMax != d.BackoffMax || o.Liveness != d.Liveness || o.SegmentBytes != d.SegmentBytes {
		t.Fatalf("model defaults differ from package defaults:\n%+v\n%+v", o, d)
	}
	var z Options
	z.ApplyTuning(model.UplinkTuning{RingMB: 3})
	z.normalize()
	if z.RingBytes != 3<<20 || z.BackfillBytes != 6<<20 || z.RingAge != DefaultRingAge {
		t.Fatalf("partial tuning %+v", z)
	}
}
