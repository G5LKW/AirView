package mlatc

import (
	"testing"
	"time"

	"airview/internal/model"
)

func TestClassifyClock(t *testing.T) {
	for typ, want := range map[string]model.ClockClass{
		"radarcape_gps": model.ClockGPSUTC, "beast": model.ClockHW12, "radarcape_12mhz": model.ClockHW12,
		"dump1090": model.ClockSDR12, "unknown": model.ClockSDR12, "auto": model.ClockSDR12, "sbs": model.ClockSBS20,
		"": model.ClockSDR12, "RADARCAPE_GPS": model.ClockGPSUTC,
	} {
		if c, _ := classifyClock(typ); c != want {
			t.Errorf("%q -> %v, want %v", typ, c, want)
		}
		// must agree with the model's contract mapping
		if c, _ := classifyClock(typ); typ != "RADARCAPE_GPS" && c != model.ClockFromMLATClientType(typ) {
			t.Errorf("%q disagrees with model.ClockFromMLATClientType", typ)
		}
	}
}

func TestModeChange(t *testing.T) {
	type tc struct {
		hs, mode string
		freq     float64
		epoch    string
		want     model.ClockClass
		ok       bool
	}
	for _, c := range []tc{
		{"radarcape_gps", "RADARCAPE", 1e9, "utc_midnight", model.ClockGPSUTC, true},
		{"radarcape_gps", "RADARCAPE_EMULATED", 1e9, "", model.ClockGPSEmul, true},
		{"radarcape_gps", "BEAST", 12e6, "", model.ClockHW12, true},
		{"dump1090", "BEAST", 12e6, "", model.ClockSDR12, true},
		{"beast", "", 12e6, "", model.ClockHW12, true},
		{"sbs", "SBS", 20e6, "", model.ClockSBS20, true},
		{"dump1090", "", 1e9, "utc_midnight", model.ClockGPSUTC, true},
		{"dump1090", "", 1e9, "", model.ClockGPSEmul, true},
		{"dump1090", "sdr12", 0, "", model.ClockSDR12, true},
		{"dump1090", "gps_gpst", 0, "", model.ClockGPSGPST, true},
		{"dump1090", "", 0, "", 0, false},
		{"dump1090", "weird", 5, "", 0, false},
	} {
		got, _, ok := modeChange(c.hs, c.mode, c.freq, c.epoch)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%+v -> %v %v", c, got, ok)
		}
	}
}

func TestNormalise12MHz(t *testing.T) {
	var c clockState
	c.setClass(model.ClockSDR12, base12)
	now := time.Now().UnixNano()
	raw := uint64(1<<48 - 1000) // near the wrap
	// the client subtracts 768 (Mode S) or 244 (Mode A/C)
	tk, step, ok := c.normalise(raw-768, 14, now)
	if !ok || step != stepNone || tk != raw {
		t.Fatalf("long: %d %v %v", tk, step, ok)
	}
	tk, _, _ = c.normalise(raw-768, 7, now)
	if tk != raw {
		t.Fatal("short")
	}
	tk, _, _ = c.normalise(raw-244, 2, now)
	if tk != raw {
		t.Fatal("mode a/c")
	}
	// across the 48-bit wrap: unwrapped
	next := uint64(5000) // = raw + 6000 mod 2^48
	tk, step, ok = c.normalise(next-768, 14, now)
	if !ok || step != stepNone || tk != 1<<48+5000 {
		t.Fatalf("wrap: %d %v", tk, step)
	}
	// value 0 = no timestamp
	if _, _, ok := c.normalise(0, 14, now); ok {
		t.Fatal("zero accepted")
	}
	// a sync half from 4 s earlier is fine, 31 s back is a new epoch
	tk0 := c.max
	if _, step, _ := c.normalise((tk0-4*12e6)&mask48-768, 14, now); step != stepNone {
		t.Fatal("4 s back flagged")
	}
	if _, step, _ := c.normalise((tk0-31*12e6)&mask48-768, 14, now); step != stepBack {
		t.Fatal("31 s back not flagged")
	}
	// forward: 60 s of clock in 1 s of server time is fine (< 90 s), 200 s is a jump
	if _, step, _ := c.normalise((tk0+60*12e6)&mask48-768, 14, now+1e9); step != stepNone {
		t.Fatal("60 s forward flagged")
	}
	if _, step, _ := c.normalise((c.max+200*12e6)&mask48-768, 14, now+2e9); step != stepJump {
		t.Fatal("200 s jump not flagged")
	}
}

func TestNormaliseGPS(t *testing.T) {
	var c clockState
	c.setClass(model.ClockGPSUTC, baseGPS)
	day := int64(20000) * model.NsPerDay
	// a long frame stamped at 23:59:59.99995 UTC (end of frame); the
	// client reports start of frame = stamp - 120 µs
	stamp := model.NsPerDay - 50000
	tClient := uint64(stamp - offGPSLong)
	// the server receives it 0.2 s later, already on the next day
	srv := day + model.NsPerDay + 150e6
	tk, _, ok := c.normalise(tClient, 14, srv)
	if !ok || tk != uint64(day+stamp) {
		t.Fatalf("got %d want %d", tk, day+stamp)
	}
	// a short frame stamped 20 µs after midnight: the client wrapped its
	// start-of-frame value to the previous day (86400 s - 44 µs)
	stamp2 := int64(20000)
	tClient2 := uint64(model.NsPerDay + stamp2 - offGPSShort)
	tk, _, ok = c.normalise(tClient2, 7, srv)
	if !ok || tk != uint64(day+model.NsPerDay+stamp2) {
		t.Fatalf("midnight wrap: got %d want %d", tk, day+model.NsPerDay+stamp2)
	}
	// absolute values are not a time of day for a real client
	if _, _, ok := c.normalise(uint64(day), 14, srv); ok {
		t.Fatal("absolute accepted without RawTicks")
	}
	c.raw = true
	if tk, _, ok := c.normalise(uint64(day+5), 14, srv); !ok || tk != uint64(day+5) {
		t.Fatal("raw absolute")
	}
}

func TestNormaliseSBS(t *testing.T) {
	var c clockState
	c.setClass(model.ClockSBS20, baseSBS)
	now := time.Now().UnixNano()
	w := uint64(5 << 24) // a widened counter
	if tk, _, ok := c.normalise(w+offSBSShort, 7, now); !ok || tk != w {
		t.Fatal("short")
	}
	if tk, _, ok := c.normalise(w, 14, now); !ok || tk != w {
		t.Fatal("long")
	}
	if tk, _, ok := c.normalise(w+offSBSAC, 2, now); !ok || tk != w {
		t.Fatal("a/c")
	}
	if _, _, ok := c.normalise(5, 7, now); ok {
		t.Fatal("underflow accepted")
	}
}

// Finding ROB-6: monotonic SBS values near 2^64 overflowed the backward
// step test (an epoch break on every other frame). They are now rejected,
// and the comparison itself no longer wraps.
func TestNormaliseSBSNoOverflow(t *testing.T) {
	var c clockState
	c.setClass(model.ClockSBS20, baseSBS)
	now := time.Now().UnixNano()
	for i := uint64(0); i < 100; i++ {
		if _, step, ok := c.normalise(1<<64-1000+i*5, 14, now); ok || step != stepNone {
			t.Fatalf("value near 2^64 accepted (ok=%v step=%v)", ok, step)
		}
	}
	// largest plausible values: monotonic frames never step back
	breaks := 0
	base := uint64(maxSBSTicks - 100000)
	for i := uint64(0); i < 100; i++ {
		_, step, ok := c.normalise(base+i*500, 14, now+int64(i)*25000)
		if !ok {
			t.Fatalf("frame %d rejected", i)
		}
		if step != stepNone {
			breaks++
		}
	}
	if breaks != 0 {
		t.Fatalf("%d epoch breaks on a monotonic counter", breaks)
	}
	// the 12 MHz path near its own maximum is unchanged
	var d clockState
	d.setClass(model.ClockHW12, base12)
	d.raw = true
	if _, step, _ := d.normalise(1<<48-10, 14, now); step != stepNone {
		t.Fatal("first frame stepped")
	}
	if _, step, _ := d.normalise(1<<48-10-31*12e6, 14, now); step != stepBack {
		t.Fatal("a real 31 s step back was not detected")
	}
}
