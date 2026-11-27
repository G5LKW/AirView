package radio

import (
	"encoding/json"
	"math"
	"testing"

	"airview/internal/model"
)

var halfPowerDB = 10 * math.Log10(2) // 3.0103 dB

func TestBeamwidthGainRelations(t *testing.T) {
	// Kraus: D = 41253/(θh·θv); with η = 0.7 a 30° × 30° beam has
	// 10·log10(0.7·41253/900) = 15.06 dBi.
	near(t, "30x30", DirectionalGainForBeamwidths(30, 30), 15.06, 0.01)
	near(t, "40x40", DirectionalGainForBeamwidths(40, 40), 12.56, 0.01)
	near(t, "product", BeamwidthProductForGain(15.06), 900, 2)
	near(t, "product round trip", DirectionalGainForBeamwidths(60, BeamwidthProductForGain(11)/60), 11, 1e-9)

	// McDonald: a half-wave dipole (78°) has ~2.15 dBi; inverse round trip.
	near(t, "dipole gain", OmniGainForBeamwidth(78), 2.15, 0.05)
	for _, g := range []float64{2.5, 5, 6, 8, 10} {
		bw := OmniBeamwidthForGain(g)
		near(t, "omni round trip", OmniGainForBeamwidth(bw), g, 1e-9)
	}
	if a, b := OmniBeamwidthForGain(5), OmniBeamwidthForGain(8); !(b < a && a < 60 && b > 10) {
		t.Errorf("omni beamwidths: 5 dBi %v°, 8 dBi %v°", a, b)
	}
	near(t, "low gain cap", OmniBeamwidthForGain(-3), 120, 0)

	// Yagi rule of thumb: 10·log10(N) + 3.
	near(t, "3 el", YagiGainForElements(3), 7.77, 0.01)
	near(t, "9 el", YagiGainForElements(9), 12.54, 0.01)
	near(t, "clamp low", YagiGainForElements(0), YagiGainForElements(2), 0)
	near(t, "clamp high", YagiGainForElements(500), YagiGainForElements(40), 0)

	// cos^n exponent: the pattern is exactly 3 dB down at half the beamwidth.
	for _, bw := range []float64{5, 30, 78, 120} {
		n := cosExponent(bw)
		near(t, "cos^n half power", 10*n*math.Log10(math.Cos(bw/2*degToRad)), -halfPowerDB, 1e-9)
	}
}

func TestOmni(t *testing.T) {
	o := NewOmni(5, 30, 0, 0, 0)
	near(t, "peak", o.GainDBi(0, 0), 5, 1e-12)
	near(t, "−3 dB up", o.GainDBi(123, 15), 5-halfPowerDB, 1e-9)
	near(t, "−3 dB down", o.GainDBi(321, -15), 5-halfPowerDB, 1e-9)
	near(t, "floor overhead", o.GainDBi(0, 90), 5-30, 1e-9)
	for az := 0.0; az < 360; az += 37 {
		near(t, "azimuth independent", o.GainDBi(az, 7), o.GainDBi(0, 7), 0)
	}
	if !math.IsNaN(o.GainDBi(0, math.NaN())) {
		t.Error("NaN elevation must give NaN")
	}
	near(t, "elevation clamp", o.GainDBi(0, 120), o.GainDBi(0, 90), 0)

	// Up-tilt moves the peak.
	ot := NewOmni(6, 20, 4, 0, 0)
	near(t, "tilted peak", ot.GainDBi(0, 4), 6, 1e-12)
	near(t, "tilted −3 dB", ot.GainDBi(0, 14), 6-halfPowerDB, 1e-9)
	near(t, "tilted −3 dB low", ot.GainDBi(0, -6), 6-halfPowerDB, 1e-9)

	// Collinear: compressed lobe, sidelobes no higher than −13.3 dB, and a
	// null straight up.
	c := Collinear(8)
	near(t, "collinear peak", c.GainDBi(0, 0), 8, 1e-12)
	bw := c.VBeamwidthDeg
	if bw < 10 || bw > 25 {
		t.Errorf("8 dBi collinear beamwidth %v°", bw)
	}
	near(t, "collinear −3 dB", c.GainDBi(0, bw/2), 8-halfPowerDB, 1e-9)
	near(t, "collinear zenith", c.GainDBi(0, 90), 8-30, 1e-9)
	prev := c.GainDBi(0, 0)
	for el := 0.5; el <= 90; el += 0.5 {
		g := c.GainDBi(0, el)
		if g > prev+1e-9 {
			t.Fatalf("collinear gain rises with elevation at %v°: %v after %v", el, g, prev)
		}
		if el > 1.2*bw && g > 8-uniformArraySidelobeDB+1e-9 {
			t.Fatalf("collinear sidelobe at %v° is %v dBi, above the −13.3 dB envelope", el, g)
		}
		if g < 8-30-1e-9 {
			t.Fatalf("collinear gain below the floor at %v°: %v", el, g)
		}
		prev = g
	}
	// The sidelobe envelope keeps a collinear well above its bare cos^n lobe
	// at medium elevations (aircraft passing nearby are still heard).
	bare := NewOmni(8, bw, 0, 0, 0)
	if c.GainDBi(0, 45) < bare.GainDBi(0, 45)+3 {
		t.Errorf("sidelobe envelope missing: %v vs %v", c.GainDBi(0, 45), bare.GainDBi(0, 45))
	}
	if Collinear(0).PeakDBi != 6 {
		t.Error("default collinear gain is 6 dBi")
	}

	h := HalfWaveVertical()
	near(t, "dipole peak", h.GainDBi(0, 0), 2.15, 1e-12)
	// Exact dipole pattern cos(π/2·sin el)/cos el at 30° elevation: −1.76 dB.
	exact := 20 * math.Log10(math.Cos(math.Pi/2*math.Sin(30*degToRad))/math.Cos(30*degToRad))
	near(t, "dipole shape at 30°", h.GainDBi(0, 30)-2.15, exact, 0.3)
	q := QuarterWaveVertical()
	near(t, "quarter-wave peak", q.GainDBi(0, 0), 2.0, 1e-12)
	if q.GainDBi(0, 40) > 2-halfPowerDB+0.01 || q.GainDBi(0, 39) < 2-halfPowerDB-0.2 {
		t.Error("quarter-wave vertical should be ~3 dB down at 40°")
	}
	near(t, "isotropic", Isotropic{GainDB: 1.5}.GainDBi(77, -12), 1.5, 0)
}

func TestDirectional(t *testing.T) {
	d := NewDirectional(12, 90, 0, 40, 30, 18, 13)
	near(t, "boresight", d.GainDBi(90, 0), 12, 1e-12)
	near(t, "H −3 dB right", d.GainDBi(110, 0), 12-halfPowerDB, 1e-9)
	near(t, "H −3 dB left", d.GainDBi(70, 0), 12-halfPowerDB, 1e-9)
	near(t, "V −3 dB up", d.GainDBi(90, 15), 12-halfPowerDB, 1e-9)
	near(t, "V −3 dB down", d.GainDBi(90, -15), 12-halfPowerDB, 1e-9)
	near(t, "directly behind", d.GainDBi(270, 0), 12-18, 1e-9)
	near(t, "abeam", d.GainDBi(180, 0), 12-(13+5.0/2), 1e-9) // envelope half-way between SLL and F/B
	near(t, "straight up", d.GainDBi(90, 90), 12-(13+5.0/2), 1e-9)
	near(t, "azimuth wraps", d.GainDBi(450, 0), 12, 1e-9)
	near(t, "azimuth wraps negative", d.GainDBi(-270, 0), 12, 1e-9)

	// Never above the peak, never below the front-to-back floor, and
	// symmetric left/right of boresight.
	for az := 0.0; az < 360; az += 5 {
		for el := -90.0; el <= 90; el += 5 {
			g := d.GainDBi(az, el)
			if g > 12+1e-9 || g < 12-18-1e-9 || math.IsNaN(g) {
				t.Fatalf("gain %v at az %v el %v outside [−6, 12]", g, az, el)
			}
			near(t, "mirror symmetry", d.GainDBi(180-az, el), g, 1e-9)
		}
	}
	// Monotonic fall-off from boresight to the back in the horizontal plane.
	prev := d.GainDBi(90, 0)
	for off := 1.0; off <= 180; off++ {
		g := d.GainDBi(90+off, 0)
		if g > prev+1e-9 {
			t.Fatalf("gain rises at %v° off boresight: %v after %v", off, g, prev)
		}
		prev = g
	}

	// Tilt: the beam peak follows the boresight elevation, and "behind" is
	// the opposite azimuth at the negated elevation.
	dt := NewDirectional(10, 0, 10, 50, 50, 20, 0)
	near(t, "tilted boresight", dt.GainDBi(0, 10), 10, 1e-12)
	near(t, "tilted −3 dB", dt.GainDBi(0, 35), 10-halfPowerDB, 1e-9)
	near(t, "tilted back", dt.GainDBi(180, -10), 10-20, 1e-9)
	if dt.SidelobeDB != 20 {
		t.Errorf("sidelobe level should default to the F/B ratio, got %v", dt.SidelobeDB)
	}
	// Defaults and clamps.
	dd := NewDirectional(9, 725, 0, 0.1, 500, 0, 99)
	if dd.AzimuthDeg != 5 || dd.HBeamwidthDeg != 2 || dd.VBeamwidthDeg != 170 || dd.FrontToBackDB != 15 || dd.SidelobeDB != 15 {
		t.Errorf("clamps/defaults: %+v", dd)
	}
}

func TestResolve(t *testing.T) {
	def := model.ExampleRadarcapeConfig()

	// Factory vertical: 5 dBi, 30° elevation beamwidth, fully specified.
	v := Resolve(def.Antennas[0])
	if v.Directional || v.Kind != model.AntVertical || v.PeakGainDBi != 5 || v.VBeamwidthDeg != 30 ||
		v.HBeamwidthDeg != 360 || len(v.Estimated) != 0 || v.SidelobeDB != uniformArraySidelobeDB {
		t.Errorf("default vertical: %+v", v)
	}
	if _, ok := FromAntenna(def.Antennas[0]).(Omni); !ok {
		t.Error("vertical should give an Omni pattern")
	}

	// Factory yagi: 12 dBi, 40° × 40°, F/B 18 dB, fully specified.
	y := Resolve(def.Antennas[1])
	if !y.Directional || y.Kind != model.AntYagi || y.PeakGainDBi != 12 || y.HBeamwidthDeg != 40 || y.VBeamwidthDeg != 40 ||
		y.FrontToBackDB != 18 || y.SidelobeDB != 13 || len(y.Estimated) != 0 {
		t.Errorf("default yagi: %+v", y)
	}
	yp, ok := FromAntenna(def.Antennas[1]).(Directional)
	if !ok {
		t.Fatal("yagi should give a Directional pattern")
	}
	near(t, "yagi boresight", yp.GainDBi(0, 0), 12, 1e-12)
	near(t, "yagi back", yp.GainDBi(180, 0), 12-18, 1e-9)

	// Yagi with only a gain: θh·θv = 0.7·41253/10^1.2 = 1822 deg², split with
	// the 9-element aspect ratio 1.1875, wider in the horizontal plane for a
	// vertically polarised antenna.
	a := model.Antenna{Type: model.AntYagi, GainDBi: 12, Elements: 9, AzimuthDeg: 45, Polarization: "vertical"}
	r := Resolve(a)
	near(t, "product", r.HBeamwidthDeg*r.VBeamwidthDeg, 1822, 1)
	near(t, "aspect", r.HBeamwidthDeg/r.VBeamwidthDeg, 1.1875, 1e-9)
	near(t, "azimuth", r.AzimuthDeg, 45, 0)
	near(t, "default F/B", r.FrontToBackDB, 15, 0)
	if !sameSet(r.Estimated, "hBeamwidthDeg", "vBeamwidthDeg", "frontToBackDb") {
		t.Errorf("estimated: %v", r.Estimated)
	}
	a.Polarization = "horizontal"
	rh := Resolve(a)
	near(t, "horizontal polarisation swaps the planes", rh.HBeamwidthDeg, r.VBeamwidthDeg, 1e-9)
	near(t, "horizontal polarisation swaps the planes", rh.VBeamwidthDeg, r.HBeamwidthDeg, 1e-9)

	// Only the element count: gain from the rule of thumb.
	r = Resolve(model.Antenna{Type: model.AntYagi, Elements: 9})
	near(t, "gain from elements", r.PeakGainDBi, 12.54, 0.01)
	if !sameSet(r.Estimated, "gainDbi", "hBeamwidthDeg", "vBeamwidthDeg", "frontToBackDb") {
		t.Errorf("estimated: %v", r.Estimated)
	}
	// Nothing at all: 10 dBi yagi.
	near(t, "bare yagi", Resolve(model.Antenna{Type: model.AntYagi}).PeakGainDBi, 10, 0)

	// Both beamwidths, no gain: G = η·41253/(θh·θv).
	r = Resolve(model.Antenna{Type: model.AntYagi, HBeamwidthDeg: 40, VBeamwidthDeg: 40, FrontToBackDB: 20})
	near(t, "gain from beamwidths", r.PeakGainDBi, 12.56, 0.01)
	if !sameSet(r.Estimated, "gainDbi") {
		t.Errorf("estimated: %v", r.Estimated)
	}
	// Gain and one beamwidth: the other follows from the product.
	r = Resolve(model.Antenna{Type: model.AntYagi, GainDBi: 12, HBeamwidthDeg: 50, FrontToBackDB: 20})
	near(t, "v from gain and h", r.VBeamwidthDeg, 1822.0/50, 0.05)
	r = Resolve(model.Antenna{Type: model.AntYagi, GainDBi: 12, VBeamwidthDeg: 30, FrontToBackDB: 20})
	near(t, "h from gain and v", r.HBeamwidthDeg, 1822.0/30, 0.05)

	// Patch: square beam, 20 dB default F/B and no separate sidelobe level.
	r = Resolve(model.Antenna{Type: model.AntPatch, GainDBi: 8})
	near(t, "patch square beam", r.HBeamwidthDeg, r.VBeamwidthDeg, 1e-9)
	near(t, "patch beamwidth", r.HBeamwidthDeg, math.Sqrt(0.7*41253/FromDB(8)), 1e-6)
	if r.FrontToBackDB != 20 || r.SidelobeDB != 20 || !r.Directional {
		t.Errorf("patch: %+v", r)
	}
	near(t, "bare patch", Resolve(model.Antenna{Type: model.AntPatch}).PeakGainDBi, 8, 0)

	// Verticals with missing data.
	r = Resolve(model.Antenna{Type: model.AntVertical})
	if r.PeakGainDBi != 2.15 || r.VBeamwidthDeg != 78 || r.SidelobeDB != 0 || !sameSet(r.Estimated, "gainDbi", "vBeamwidthDeg") {
		t.Errorf("bare vertical: %+v", r)
	}
	r = Resolve(model.Antenna{Type: model.AntCollinear})
	if r.PeakGainDBi != 6 || r.SidelobeDB != uniformArraySidelobeDB || r.VBeamwidthDeg > 40 {
		t.Errorf("bare collinear: %+v", r)
	}
	r = Resolve(model.Antenna{Type: model.AntCollinear, GainDBi: 9})
	near(t, "collinear beamwidth from gain", r.VBeamwidthDeg, OmniBeamwidthForGain(9), 1e-9)
	if !sameSet(r.Estimated, "vBeamwidthDeg") {
		t.Errorf("estimated: %v", r.Estimated)
	}
	r = Resolve(model.Antenna{Type: model.AntVertical, VBeamwidthDeg: 60})
	near(t, "vertical gain from beamwidth", r.PeakGainDBi, OmniGainForBeamwidth(60), 1e-9)

	// "other" / unknown types: omni unless a horizontal beamwidth is given.
	if Resolve(model.Antenna{Type: "discone", GainDBi: 3}).Directional {
		t.Error("unknown type without beamwidth should be an omni")
	}
	r = Resolve(model.Antenna{Type: model.AntOther, GainDBi: 14, HBeamwidthDeg: 30})
	if !r.Directional || r.Kind != model.AntOther {
		t.Errorf("other with beamwidth: %+v", r)
	}

	// The info rebuilds the same pattern, and is JSON-encodable.
	for _, ant := range append(def.Antennas, a, model.Antenna{Type: model.AntCollinear, GainDBi: 7, TiltDeg: 2}) {
		info := Resolve(ant)
		p1, p2 := FromAntenna(ant), info.Pattern()
		for _, dir := range [][2]float64{{0, 0}, {45, 3}, {180, -5}, {270, 60}, {10, 90}} {
			near(t, "info.Pattern == FromAntenna", p2.GainDBi(dir[0], dir[1]), p1.GainDBi(dir[0], dir[1]), 0)
		}
		b, err := json.Marshal(info)
		if err != nil || len(b) == 0 {
			t.Errorf("json: %v", err)
		}
	}
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	m := map[string]bool{}
	for _, g := range got {
		m[g] = true
	}
	for _, w := range want {
		if !m[w] {
			return false
		}
	}
	return true
}

func TestCuts(t *testing.T) {
	d := NewDirectional(12, 120, 5, 40, 30, 18, 13)

	az := AzimuthCut(d, 5, 0)
	if len(az.AngleDeg) != 360 || len(az.GainDBi) != 360 || !az.Wrap || az.AngleDeg[0] != 0 || az.AngleDeg[359] != 359 {
		t.Fatalf("azimuth cut shape: %d %v", len(az.AngleDeg), az.Wrap)
	}
	ang, g := az.Peak()
	near(t, "azimuth peak angle", ang, 120, 0)
	near(t, "azimuth peak gain", g, 12, 1e-9)
	bw, ok := az.Beamwidth(3.0103)
	if !ok {
		t.Fatal("azimuth beamwidth not found")
	}
	// The cut is taken on a cone of 5° elevation, so its width in azimuth is
	// a touch more than the 40° measured in the tilted plane.
	near(t, "azimuth −3 dB width", bw, 40, 0.5)

	// A beam across north wraps correctly.
	north := AzimuthCut(NewDirectional(10, 0, 0, 60, 60, 20, 0), 0, 2)
	if len(north.AngleDeg) != 180 {
		t.Fatalf("2° cut length %d", len(north.AngleDeg))
	}
	bw, ok = north.Beamwidth(3.0103)
	if !ok {
		t.Fatal("wrapped beamwidth not found")
	}
	near(t, "wrapped beamwidth", bw, 60, 0.5)

	el := ElevationCut(d, 120, 1)
	if len(el.AngleDeg) != 181 || el.Wrap || el.AngleDeg[0] != -90 || el.AngleDeg[180] != 90 {
		t.Fatalf("elevation cut shape: %d", len(el.AngleDeg))
	}
	ang, g = el.Peak()
	near(t, "elevation peak angle", ang, 5, 0)
	near(t, "elevation peak gain", g, 12, 1e-9)
	bw, ok = el.Beamwidth(3.0103)
	if !ok {
		t.Fatal("elevation beamwidth not found")
	}
	near(t, "elevation −3 dB width", bw, 30, 0.3)

	// Histogram-matched range: −2…+90 in 0.5° steps = 185 samples.
	r := ElevationCutRange(d, 120, -2, 90, 0.5)
	if len(r.AngleDeg) != 185 || r.AngleDeg[0] != -2 || r.AngleDeg[184] != 90 {
		t.Fatalf("range cut: %d [%v … %v]", len(r.AngleDeg), r.AngleDeg[0], r.AngleDeg[len(r.AngleDeg)-1])
	}
	near(t, "range cut step", r.AngleDeg[1]-r.AngleDeg[0], 0.5, 1e-12)

	// An omni has no azimuth beamwidth; its elevation cut does.
	o := NewOmni(5, 30, 0, 0, 0)
	if _, ok := AzimuthCut(o, 0, 5).Beamwidth(3); ok {
		t.Error("an omni's azimuth cut has no −3 dB points")
	}
	bw, ok = ElevationCut(o, 0, 0.5).Beamwidth(3.0103)
	if !ok {
		t.Fatal("omni elevation beamwidth not found")
	}
	near(t, "omni elevation beamwidth", bw, 30, 0.1)

	// Degenerate requests stay bounded and well-formed.
	if n := len(AzimuthCut(o, 0, 1e-9).AngleDeg); n != maxCutPoints-1 {
		t.Errorf("tiny step: %d samples", n)
	}
	if c := ElevationCutRange(o, 0, 10, 10, 1); len(c.AngleDeg) != 2 || c.AngleDeg[0] != 10 || c.AngleDeg[1] != 10 {
		t.Errorf("empty span: %+v", c)
	}
	if c := ElevationCutRange(o, 0, 50, -20, 1); c.AngleDeg[0] != 50 || c.AngleDeg[len(c.AngleDeg)-1] != 50 {
		t.Errorf("inverted span: %+v", c.AngleDeg)
	}
	if _, ok := (Cut{}).Beamwidth(3); ok {
		t.Error("empty cut has no beamwidth")
	}
	if a, g := (Cut{}).Peak(); !math.IsNaN(a) || !math.IsInf(g, -1) {
		t.Error("empty cut peak")
	}
	if _, err := json.Marshal(az); err != nil {
		t.Errorf("json: %v", err)
	}
}
