package modes

import (
	"math"
	"testing"
)

// Benchmarks. The package target is a Radarcape delivering up to a few
// thousand frames per second on a Raspberry Pi 4, so every decode path must
// stay far below a microsecond on a desktop CPU and must not allocate.
//
//	go test ./internal/modes -bench . -benchmem

var benchSink Message

func benchDecode(b *testing.B, hex string, o Options) {
	frame := hexFrame(b, hex)
	if len(frame) == 14 && frame[0]>>3 >= 17 && frame[0]>>3 <= 18 {
		SetParity(frame, 0)
	}
	var m Message
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := DecodeOpts(frame, &m, o); err != nil {
			b.Fatal(err)
		}
	}
	benchSink = m
}

func BenchmarkDecodeDF17Position(b *testing.B) {
	benchDecode(b, bookPosEven, Options{})
}

func BenchmarkDecodeDF17SurfacePosition(b *testing.B) {
	benchDecode(b, bookSurf, Options{})
}

func BenchmarkDecodeDF17Velocity(b *testing.B) {
	benchDecode(b, bookVelGS, Options{})
}

func BenchmarkDecodeDF17Ident(b *testing.B) {
	benchDecode(b, bookIdent, Options{})
}

func BenchmarkDecodeDF17TargetState(b *testing.B) {
	benchDecode(b, ownTSS, Options{})
}

func BenchmarkDecodeDF17OpStatus(b *testing.B) {
	benchDecode(b, ownOpStatus, Options{})
}

func BenchmarkDecodeDF11(b *testing.B) {
	benchDecode(b, bookAllCall, Options{})
}

func BenchmarkDecodeDF4(b *testing.B) {
	benchDecode(b, bookDF4, Options{})
}

func BenchmarkDecodeDF0Gillham(b *testing.B) {
	benchDecode(b, ownDF0Gillham, Options{})
}

func BenchmarkDecodeDF20BDS50(b *testing.B) {
	benchDecode(b, ownBDS50, Options{})
}

func BenchmarkDecodeDF21Ambiguous(b *testing.B) {
	benchDecode(b, ownAmbigDF21, Options{})
}

func BenchmarkDecodeDF20NoCommB(b *testing.B) {
	benchDecode(b, ownBDS50, Options{NoCommB: true})
}

func BenchmarkDecodeDF17OneBitRepair(b *testing.B) {
	frame := hexFrame(b, bookPosEven)
	flipBit(frame, 60)
	var m Message
	if err := DecodeOpts(frame, &m, Options{FixErrors: 2}); err != nil { // builds the syndrome table
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := DecodeOpts(frame, &m, Options{FixErrors: 2}); err != nil {
			b.Fatal(err)
		}
	}
	benchSink = m
}

func BenchmarkDisambiguateBDS(b *testing.B) {
	m := mustDecode(b, ownAmbigDF21)
	b.ReportAllocs()
	b.ResetTimer()
	var got BDS
	for i := 0; i < b.N; i++ {
		got, _, _ = DisambiguateBDS(&m, 320, 250, 14000)
	}
	if got != BDS50 {
		b.Fatal(got)
	}
}

func BenchmarkResidual(b *testing.B) {
	frame := hexFrame(b, bookPosEven)
	b.ReportAllocs()
	var r uint32
	for i := 0; i < b.N; i++ {
		r |= Residual(frame)
	}
	if r != 0 {
		b.Fatal(r)
	}
}

func BenchmarkCPRAirborneGlobal(b *testing.B) {
	b.ReportAllocs()
	var lat, lon float64
	var ok bool
	for i := 0; i < b.N; i++ {
		lat, lon, ok = CPRAirborneGlobal(93000, 51372, 74158, 50194, i&1 == 0)
	}
	if !ok || math.Abs(lat-52.26) > 0.01 || math.Abs(lon-3.93) > 0.02 {
		b.Fatal(lat, lon, ok)
	}
}

func BenchmarkCPRAirborneLocal(b *testing.B) {
	b.ReportAllocs()
	var lat, lon float64
	var ok bool
	for i := 0; i < b.N; i++ {
		lat, lon, ok = CPRAirborneLocal(52.258, 3.918, 93000, 51372, false)
	}
	if !ok || math.Abs(lat-52.2572) > 1e-4 || math.Abs(lon-3.9194) > 1e-4 {
		b.Fatal(lat, lon, ok)
	}
}

// TestNoAllocations pins the zero-allocation property for every decode path
// and helper that runs per frame.
func TestNoAllocations(t *testing.T) {
	frames := []string{
		bookPosEven,  // DF17 airborne position
		bookSurf,     // DF17 surface position
		bookVelGS,    // DF17 velocity
		bookIdent,    // DF17 identification
		ownStatus,    // DF17 aircraft status
		ownTSS,       // DF17 target state
		ownOpStatus,  // DF17 operational status
		ownADSRIdent, // DF18 ADS-R
		bookAllCall,  // DF11
		ownDF0,       // DF0
		bookDF4,      // DF4
		bookDF5,      // DF5
		ownDF16,      // DF16
		ownBDS50,     // DF20 BDS 5,0
		ownAmbigDF21, // DF21 BDS 5,0 / 6,0
		ownBDS20c,    // DF20 BDS 2,0
		bookBDS44,    // DF20 BDS 4,4
		ownDF24,      // DF24
	}
	var m Message
	for _, s := range frames {
		f := hexFrame(t, s)
		if n := testing.AllocsPerRun(100, func() {
			if err := Decode(f, &m); err != nil {
				t.Fatal(err)
			}
		}); n != 0 {
			t.Errorf("Decode(%s) allocates %v times per call", s, n)
		}
	}

	// Error correction (after the one-off table construction) and failures.
	damaged := hexFrame(t, bookPosEven)
	flipBit(damaged, 44)
	flipBit(damaged, 90)
	DecodeOpts(damaged, &m, Options{FixErrors: 2})
	if n := testing.AllocsPerRun(100, func() {
		if err := DecodeOpts(damaged, &m, Options{FixErrors: 2}); err != nil {
			t.Fatal(err)
		}
		if err := Decode(damaged, &m); err == nil {
			t.Fatal("damaged frame accepted")
		}
	}); n != 0 {
		t.Errorf("error correction allocates %v times per call", n)
	}

	// Per-frame helpers.
	c := mustDecode(t, ownAmbigDF21)
	p := mustDecode(t, bookPosEven)
	var buf [16]byte
	if n := testing.AllocsPerRun(100, func() {
		DisambiguateBDS(&c, 320, 250, 14000)
		RankBDS(&c, 320, 250, 14000)
		c.BestBDS()
		InferBDS(c.MB(), 14000)
		DecodeBDS50(c.MB())
		DecodeBDS60(c.MB())
		DecodeBDS40(c.MB())
		DecodeBDS44(c.MB())
		DecodeBDS45(c.MB())
		DecodeBDS10(c.MB())
		DecodeACASRA(c.MB())
		CPRAirborneGlobal(93000, 51372, 74158, 50194, true)
		CPRAirborneLocal(52.258, 3.918, 93000, 51372, false)
		CPRSurfaceGlobal(51.99, 4.375, 115609, 116941, 39199, 110269, true)
		CPRSurfaceLocal(51.99, 4.375, 39195, 110320, true)
		p.NICRc(2, true, false)
		p.AddrKey()
		AppendICAO(buf[:0], p.ICAO)
		AppendSquawk(buf[:0], 0x7700)
		_ = CategoryName(0xA3)
		_ = EmergencyName(1)
		_ = DFIndex(p.DF)
		_ = p.AddrType.Label()
		_ = p.Source.Label()
		Residual(p.Raw[:14])
	}); n != 0 {
		t.Errorf("per-frame helpers allocate %v times per call", n)
	}
}
