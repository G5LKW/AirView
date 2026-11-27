package ref

import (
	"math"
	"testing"

	"airview/internal/modes"
)

// altReplyFrame encodes a DF4 surveillance altitude reply (airborne, AC13
// altitude in ft) with the address overlaid on the parity.
func altReplyFrame(icao uint32, df uint8, altFt float64) []byte {
	n := 7
	if df >= 16 {
		n = 14
	}
	f := make([]byte, n)
	f[0] = df << 3 // FS / VS 0: airborne
	code, _ := modes.EncodeAC13(int32(math.Round(altFt/25) * 25))
	f[2] = byte(code>>8) & 0x1F
	f[3] = byte(code)
	modes.SetParity(f, icao)
	return f
}

// TestAltReplies: Mode S altitude replies of a tracked aircraft feed its
// vertical channel (throttled), a reply far off the track's height is
// rejected, and replies of unknown addresses are ignored.
func TestAltReplies(t *testing.T) {
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x3C1234, 50, 9, 230, 45)
	fd.k.vu = 8
	fd.run(0, 20)
	alt := func(t float64) float64 { _, llh, _, _, _ := fd.k.truth(t); return llh.H / ftM }
	if r := fd.obs(altReplyFrame(fd.icao, 4, alt(20.1)), 20.1); r != Accepted {
		t.Fatalf("DF4 reply: %v", r)
	}
	if r := fd.obs(altReplyFrame(fd.icao, 20, alt(20.6)), 20.6); r != Noted {
		t.Fatalf("second reply within AltReplyMinS: %v, want noted", r)
	}
	if r := fd.obs(altReplyFrame(fd.icao, 0, alt(22.5)+3000), 22.5); r != Rejected {
		t.Fatalf("3000 ft off: %v, want rejected", r)
	}
	if r := fd.obs(altReplyFrame(0x777777, 4, 30000), 22.6); r != Ignored {
		t.Fatalf("unknown address: %v, want ignored", r)
	}
	if r := fd.obs(altReplyFrame(fd.icao, 16, alt(23)), 23); r != Accepted {
		t.Fatalf("DF16 reply: %v", r)
	}
	st := s.Stats()
	if st.AltReplies != 3 || st.AltReplyRejected != 1 {
		t.Fatalf("stats %+v", st)
	}
	// the reply node keeps its exact tag through a latency re-tag
	s.SetLatency(fd.icao, 0.2, 0.01)
	sh := s.shard(fd.icao)
	sh.mu.Lock()
	tr := sh.m[fd.icao]
	found := false
	for i := 0; i < tr.n; i++ {
		if nd := tr.at(i); nd.kind&nExact != 0 {
			found = true
			if nd.t != nd.rawT {
				t.Errorf("reply node re-tagged to %.3f", nd.t)
			}
		}
	}
	sh.mu.Unlock()
	if !found {
		t.Fatal("no reply node in the window")
	}
	var stt State
	if !s.At(fd.icao, fd.ns(23), 0, &stt) {
		t.Fatal("no state")
	}
	_, llh, _, _, _ := fd.k.truth(23)
	want, _, _ := s.HAEFromBaro(fd.icao, llh.Lat, llh.Lon, llh.H/ftM, fd.ns(23))
	if d := stt.HAE - want; math.Abs(d) > 30 {
		t.Errorf("height after replies off by %.1f m", d)
	}
}

// TestGNSSBaroPairing: an aircraft that reports GNSS heights (TC 20-22) and
// answers Mode S altitude interrogations gets its own GNSS−baro difference
// (vertical-rate compensated) and feeds the regional field with it.
func TestGNSSBaroPairing(t *testing.T) {
	const deltaM = 310.0 // GNSS height − pressure altitude in this world
	s := New(Options{}, nil)
	fd := newFeeder(s, 0x4CA111, 50, 9, 230, 120)
	fd.k.vu = -6
	for tt := 0.0; tt < 75; tt += 0.25 {
		k := int(math.Round(tt / 0.25))
		_, llh, ve, vn, vu := fd.k.truth(tt)
		switch {
		case k%20 == 0:
			fd.obs(opStatusFrame(fd.icao, fd.nacp), tt)
		case k%2 == 0:
			fd.obs(posFrame(fd.icao, 20, llh.H/ftM, false, fd.odd, llh.Lat, llh.Lon), tt)
			fd.odd = !fd.odd
		default:
			fd.obs(velFrame(fd.icao, ve, vn, vu, math.NaN(), 2), tt)
			// a reply 0.1 s after the velocity, every second
			if k%4 == 1 {
				ta := tt + 0.1
				_, l2, _, _, _ := fd.k.truth(ta)
				fd.obs(altReplyFrame(fd.icao, 4, (l2.H-deltaM)/ftM), ta)
			}
		}
	}
	st := s.Stats()
	if st.GNSSPairs == 0 {
		t.Fatalf("no GNSS−baro pairs: %+v", st)
	}
	_, llh, _, _, _ := fd.k.truth(75)
	baro := (llh.H - deltaM) / ftM
	h, sig, src := s.HAEFromBaro(fd.icao, llh.Lat, llh.Lon, baro, fd.ns(74.9))
	if src != AltOwnDelta || math.Abs(h-llh.H) > 15 {
		t.Fatalf("own delta: HAE %.1f (truth %.1f) σ %.1f src %v", h, llh.H, sig, src)
	}
	s.Expire(fd.ns(75) + 1e12) // flush the per-aircraft field accumulator
	fs := s.Field().Stats()
	if fs.Observations == 0 {
		t.Fatalf("field got no observation: %+v", fs)
	}
	d, _, fsrc := s.Field().Offset(llh.Lat, llh.Lon, baro, fd.ns(75))
	if fsrc != AltField || math.Abs(d-deltaM) > 25 {
		t.Fatalf("field offset %.1f m (%v), want %.0f", d, fsrc, deltaM)
	}
	t.Logf("pairs %d, own-delta HAE error %.1f m, field offset %.1f m from %d observations", st.GNSSPairs, h-llh.H, d, fs.Observations)
}
