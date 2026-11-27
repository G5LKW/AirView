package uplink

import (
	"time"

	"airview/internal/model"
)

// ApplyTuning copies the file-configurable tunables (config key
// "uplinkTuning", DESIGN §20.3) into o. Zero fields keep the package
// defaults, so a zero model.UplinkTuning changes nothing.
func (o *Options) ApplyTuning(t model.UplinkTuning) {
	sec := func(v int) time.Duration { return time.Duration(v) * time.Second }
	o.RingAge = sec(t.RingSec)
	o.RingBytes = t.RingMB << 20
	o.BackfillBytes = t.BackfillQueueMB << 20
	o.ResumeWindow = sec(t.ResumeWindowSec)
	o.BackfillMinBps = t.BackfillMinKBps << 10
	o.BackoffMin = sec(t.BackoffMinSec)
	o.BackoffMax = sec(t.BackoffMaxSec)
	o.Liveness = sec(t.LivenessSec)
	o.SegmentBytes = int64(t.SpoolSegmentMB) << 20
}
