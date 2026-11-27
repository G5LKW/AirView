package simnet

import (
	"math"
	"testing"

	"airview/internal/modes"
)

func setAP(f []byte, addr uint32) { modes.SetParity(f, addr) }

func TestGeodeticRoundTrip(t *testing.T) {
	for _, c := range [][3]float64{{0, 0, 0}, {51.5, -0.1, 45}, {-33.9, 151.2, 11000}, {69.9, 179.99, 12500}, {-70, -179.5, -50}} {
		p := ECEF(c[0], c[1], c[2])
		lat, lon, h := Geodetic(p)
		if math.Abs(lat-c[0]) > 1e-9 || math.Abs(wrapLon(lon-c[1])) > 1e-9 || math.Abs(h-c[2]) > 1e-4 {
			t.Fatalf("%v -> %v %v %v", c, lat, lon, h)
		}
	}
	// a known point: equator / prime meridian at the surface
	if p := ECEF(0, 0, 0); math.Abs(p[0]-wgsA) > 1e-6 || math.Abs(p[1]) > 1e-6 || math.Abs(p[2]) > 1e-6 {
		t.Fatal(p)
	}
}

func TestRefractionExcess(t *testing.T) {
	a := Atmosphere{}
	a.normalize()
	rx := ECEF(50, 8, 100)
	rxR := rx.Norm() - 100
	// vertical path from 100 m to 10.1 km: N0·H·(e^{-h1/H} - e^{-h2/H})·1e-6
	up := ECEF(50, 8, 10100)
	got := a.ExcessPathM(rx, up, rxR, up.Sub(rx).Norm())
	want := 315e-6 * 7000 * (math.Exp(-100.0/7000) - math.Exp(-10100.0/7000))
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("zenith excess %v, want %v", got, want)
	}
	// a long, low path has a much larger excess (tens of metres)
	lat, lon := destination(50, 8, 90, 350e3)
	far := ECEF(lat, lon, 10000)
	low := a.ExcessPathM(rx, far, rxR, far.Sub(rx).Norm())
	if low < 30 || low > 120 {
		t.Fatalf("low-elevation excess %v m", low)
	}
	// k_atm scales linearly
	a.KAtm = 1.1
	if s := a.ExcessPathM(rx, far, rxR, far.Sub(rx).Norm()); math.Abs(s-1.1*low) > 1e-9 {
		t.Fatal("KAtm does not scale the excess")
	}
}

func TestSignalByte(t *testing.T) {
	if signalByte(0) != 255 || signalByte(-60) != 1 {
		t.Fatal("clamp")
	}
	b := signalByte(-20)
	if d := 20*math.Log10(float64(b)/255) + 20; math.Abs(d) > 0.4 {
		t.Fatalf("-20 dBFS -> %d", b)
	}
}

func TestMakeUUID(t *testing.T) {
	u := makeUUID(1, 2)
	if len(u) != 36 || u[14] != '4' || (u[19] != '8' && u[19] != '9' && u[19] != 'a' && u[19] != 'b') {
		t.Fatalf("uuid %s", u)
	}
	if makeUUID(1, 2) != u || makeUUID(1, 3) == u {
		t.Fatal("not deterministic / not distinct")
	}
}
