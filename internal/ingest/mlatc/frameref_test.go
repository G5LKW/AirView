package mlatc

import (
	"math"
	"testing"
	"time"

	"airview/internal/mlat/clock"
	"airview/internal/model"
)

// Interop check for findings M1/M2: ticks in the reference client's own
// convention for a short and a long frame of the same transmission
// instant must name the same instant once the server has normalised them
// (normalise) and the clock filter has removed FrameRef(class, kind). The
// client's convention per tick base (MLATC.md §5):
//
//   - 12 MHz (beast, radarcape_12mhz): 768 ticks (64 µs) are removed from
//     short and long frames alike, so client values of one instant are
//     equal for both lengths;
//   - SBS: 56 µs are added to short and 96 µs to A/C stamps, aligning every
//     kind with the end of a long frame: again equal client values;
//   - GPS (radarcape_gps): the per-kind end-of-frame offsets are removed,
//     giving the start of the frame for every kind.
//
// A table that disagrees by tens of µs between the kinds (the bugs) fails
// here by exactly that amount.
func TestFrameRefInterop(t *testing.T) {
	now := time.Now().UnixNano()
	type tc struct {
		name string
		cls  model.ClockClass
		base tickBase
		val  func(kind model.FrameKind) uint64 // client value of one instant
	}
	const t12 = uint64(123456789012)
	const tSBS = uint64(77 << 24)
	day := now % model.NsPerDay
	tGPS := uint64((day + model.NsPerDay - 5e9) % model.NsPerDay) // ns of the UTC day, 5 s ago
	cases := []tc{
		{"hw12", model.ClockHW12, base12, func(model.FrameKind) uint64 { return t12 }},
		{"sbs20", model.ClockSBS20, baseSBS, func(model.FrameKind) uint64 { return tSBS }},
		{"gps", model.ClockGPSUTC, baseGPS, func(model.FrameKind) uint64 { return tGPS }},
	}
	lenOf := map[model.FrameKind]int{model.FrameModeSShort: 7, model.FrameModeSLong: 14}
	for _, c := range cases {
		var cs clockState
		cs.setClass(c.cls, c.base)
		hz := c.cls.NominalHz()
		tks := map[model.FrameKind]uint64{}
		for _, k := range []model.FrameKind{model.FrameModeSShort, model.FrameModeSLong} {
			tk, step, ok := cs.normalise(c.val(k), lenOf[k], now)
			if !ok || step != stepNone {
				t.Fatalf("%s %v: normalise ok=%v step=%v", c.name, k, ok, step)
			}
			tks[k] = tk
		}
		// difference of the instants the clock filter infers (ns):
		// tick difference minus the FrameRef difference
		sh, lg := model.FrameModeSShort, model.FrameModeSLong
		d := float64(int64(tks[sh]-tks[lg]))/hz*1e9 - (clock.DefaultFrameRefNs(c.cls, sh) - clock.DefaultFrameRefNs(c.cls, lg))
		if math.Abs(d) > 1 {
			t.Errorf("%s: short and long frames of one instant disagree by %.0f ns after FrameRef", c.name, d)
		}
	}
}
