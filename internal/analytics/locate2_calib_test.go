package analytics

import (
	"math"
	"testing"
)

// Field case r101: a real station with a DEM and signal levels. Locate Me v2
// reported ±0.3 nm (95 %: 0.7 nm) and was 2.6 nm (4.82 km) off: the
// posterior was as sharp as the 200 m grid and the 0.5 km DEM floor made
// the whole uncertainty. The calibrated circle must now hold that error and
// the error must be an ordinary (< 3 sigma) miss.
func TestL2CalibrateFieldCase(t *testing.T) {
	const fieldErrKm = 2.6 * 1.852
	for _, c := range []struct {
		name            string
		signal, terrain bool
	}{{"signal+DEM", true, true}, {"signal", true, false}, {"DEM", false, true}, {"neither", false, false}} {
		el := l2Calibrate(0.01, 0.008, 0.001, 0.2, c.signal, c.terrain, 0)
		if el.r95 < fieldErrKm {
			t.Errorf("%s: r95 %.2f km does not hold the field error %.2f km", c.name, el.r95, fieldErrKm)
		}
		if z := fieldErrKm / el.sigma; z > 3 {
			t.Errorf("%s: field error is %.1f sigma (sigma %.2f km)", c.name, z, el.sigma)
		}
		if !(el.minor <= el.major) || !(el.sigma >= el.minor && el.sigma <= el.major) {
			t.Errorf("%s: inconsistent ellipse %+v", c.name, el)
		}
	}
}

func TestL2CalibrateInflation(t *testing.T) {
	// a wide posterior is widened by the inflation factors, not just the floor
	base := l2Calibrate(100, 25, 0, 2, true, true, 0)
	raised := l2Calibrate(100, 25, 0, 2, true, true, 0.2)
	if !(raised.r95 > base.r95) {
		t.Errorf("skyline raised did not widen: %.2f vs %.2f", raised.r95, base.r95)
	}
	noSig := l2Calibrate(100, 25, 0, 2, false, true, 0)
	if !(noSig.r95 > base.r95) {
		t.Errorf("no signal did not widen: %.2f vs %.2f", noSig.r95, base.r95)
	}
	// major axis north-south for cyy > cxx
	ns := l2Calibrate(1, 100, 0, 0.2, true, true, math.NaN())
	if d := math.Abs(math.Mod(ns.axisDeg+360, 180)); d > 1 && d < 179 {
		t.Errorf("axis %.1f, want 0/180", ns.axisDeg)
	}
	want := math.Sqrt(5.991 * (100*1.5*1.5 + 0.04/12 + l2ModelFloorKm*l2ModelFloorKm))
	if math.Abs(ns.r95-want) > 1e-9 {
		t.Errorf("r95 %.4f want %.4f", ns.r95, want)
	}
}
