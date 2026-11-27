package ref

import (
	"math"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/modes"
)

type benchMsg struct {
	m  modes.Message
	ns int64
}

// benchStream returns the decoded messages of a synthetic aircraft for
// dur seconds (positions and velocities at 2 Hz each, status every 5 s).
func benchStream(icao uint32, dur float64) []benchMsg {
	fd := newFeeder(nil, icao, 50, 8, 230, 80)
	fd.k.turnA, fd.k.turnB, fd.k.omega = 100, 160, 2*math.Pi/180
	var out []benchMsg
	odd := false
	for t := 0.0; t < dur; t += 0.25 {
		k := int(math.Round(t / 0.25))
		var f []byte
		switch {
		case k%20 == 0:
			f = opStatusFrame(icao, 9)
		case k%2 == 0:
			_, llh, _, _, _ := fd.k.truth(t)
			f = posFrame(icao, 11, llh.H/ftM, false, odd, llh.Lat, llh.Lon)
			odd = !odd
		default:
			_, _, ve, vn, vu := fd.k.truth(t)
			f = velFrame(icao, ve, vn, vu, 0, 2)
		}
		var m modes.Message
		if err := modes.Decode(f, &m); err != nil {
			panic(err)
		}
		out = append(out, benchMsg{m, fd.ns(t)})
	}
	return out
}

// BenchmarkObserve: one ADS-B message (mix of positions, velocities and
// status) into a track.
func BenchmarkObserve(b *testing.B) {
	msgs := benchStream(0x3c0101, 600)
	s := New(Options{}, nil)
	b.ReportAllocs()
	b.ResetTimer()
	j := 0
	for i := 0; i < b.N; i++ {
		if j == len(msgs) {
			b.StopTimer()
			s = New(Options{}, nil)
			j = 0
			b.StartTimer()
		}
		s.Observe(&msgs[j].m, msgs[j].ns, 0)
		j++
	}
}

func benchStore(b *testing.B, n int) (*Store, []benchMsg) {
	msgs := benchStream(0x3c0102, 60)
	s := New(Options{}, nil)
	for i := range msgs {
		s.Observe(&msgs[i].m, msgs[i].ns, 0)
	}
	_ = n
	return s, msgs
}

// BenchmarkAtInterpolated: the state 0.4 s before the newest report (the
// usual case after the grouper's flush): a short RTS backward pass plus
// the pseudo-node.
func BenchmarkAtInterpolated(b *testing.B) {
	s, msgs := benchStore(b, 1)
	last := msgs[len(msgs)-1].ns
	var st State
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t := last - 400_000_000 + int64(i%100)*1_000_000
		if !s.At(0x3c0102, t, 0, &st) {
			b.Fatal("no state")
		}
	}
}

// BenchmarkAtOldest: the worst case, an instant near the oldest node
// (backward pass over the whole window).
func BenchmarkAtOldest(b *testing.B) {
	s, msgs := benchStore(b, 1)
	last := msgs[len(msgs)-1].ns
	var st State
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !s.At(0x3c0102, last-3_600_000_000, 0, &st) {
			b.Fatal("no state")
		}
	}
}

// BenchmarkAtExtrapolated: an instant after the newest report.
func BenchmarkAtExtrapolated(b *testing.B) {
	s, msgs := benchStore(b, 1)
	last := msgs[len(msgs)-1].ns
	var st State
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !s.At(0x3c0102, last+100_000_000, 0, &st) {
			b.Fatal("no state")
		}
	}
}

// BenchmarkAtParallel: concurrent queries over 1000 aircraft.
func BenchmarkAtParallel(b *testing.B) {
	s := New(Options{}, nil)
	msgs := benchStream(0x3c0000, 30)
	var keys []uint32
	for a := uint32(0); a < 1000; a++ {
		key := 0x400000 + a
		for i := range msgs {
			m := msgs[i].m
			m.ICAO = key
			s.Observe(&m, msgs[i].ns, 0)
		}
		keys = append(keys, key)
	}
	last := msgs[len(msgs)-1].ns
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var st State
		i := 0
		for pb.Next() {
			s.At(keys[i%len(keys)], last-400_000_000, 0, &st)
			i++
		}
	})
}

// BenchmarkTransmitInstant: τ from four arrivals with refraction.
func BenchmarkTransmitInstant(b *testing.B) {
	s, msgs := benchStore(b, 1)
	last := msgs[len(msgs)-1].ns
	var st State
	s.At(0x3c0102, last-400_000_000, 0, &st)
	atm := geo.DefaultAtmosphere()
	rx := []geo.Vec3{geo.ToECEF(50.5, 8, 100), geo.ToECEF(49.6, 8.4, 200), geo.ToECEF(50.1, 9.2, 50), geo.ToECEF(49.9, 7.1, 300)}
	arr := make([]Arrival, len(rx))
	for i, r := range rx {
		arr[i] = Arrival{Station: r, DtNs: atm.PropagationNs(r, st.Pos), SigmaNs: 50}
	}
	base := last - 400_000_000
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, ok := s.TransmitInstant(0x3c0102, base, arr, &atm, &st); !ok {
			b.Fatal("no τ")
		}
	}
}

// BenchmarkFieldOffset: one pressure-altitude conversion (eight cells).
func BenchmarkFieldOffset(b *testing.B) {
	f := NewField(FieldOptions{}, nil)
	for i := 0; i < 100; i++ {
		f.Update(50+float64(i%10)*0.3, 8+float64(i/10)*0.4, 35000, 50, 12, 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Offset(51.1, 9.3, 34100, 1e9)
	}
}
