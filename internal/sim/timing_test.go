package sim

import (
	"context"
	"math"
	"sort"
	"testing"
	"time"

	"airview/internal/model"
)

// TestUTCCoupledShare: about 10 % of the ADS-B aircraft send airborne
// positions with the TIME bit set, chosen deterministically from seed and
// address; their positions apply to the 0.2 s UTC epoch of the CPR format
// parity and are sent within ±150 ms of it (DO-260A), only with the type
// codes T=1 is allowed with (9, 10, 20, 21).
func TestUTCCoupledShare(t *testing.T) {
	run := func() (map[uint32]bool, []float64, int) {
		s := New(Config{Aircraft: 400, Seed: 11, Start: testStart, NoDiurnal: true, PerfectReception: true, MLATFraction: -1})
		ac := map[uint32]bool{} // airborne-position senders -> T=1
		var lat []float64       // transmit - epoch, ms
		badTC := 0
		sink := funcSink(func(f model.RawFrame) {
			if f.MLAT || len(f.Data) != 14 || refDF(f.Data) != 17 || f.Channel != 0 {
				return
			}
			me := f.Data[4:11]
			tc := rb(me, 1, 5)
			if !(tc >= 9 && tc <= 18 || tc >= 20 && tc <= 22) {
				return
			}
			icao := refICAO(f.Data)
			t1 := rb(me, 21, 21) == 1
			if prev, seen := ac[icao]; seen && prev != t1 {
				t.Errorf("%06X changes its T bit", icao)
			}
			ac[icao] = t1
			if !t1 {
				return
			}
			if tc != 9 && tc != 10 && tc != 20 && tc != 21 {
				badTC++
			}
			// transmit time from the GPS stamp, minus the propagation delay
			tr, ok := s.TruthOf(icao, TransmitTime(f))
			if !ok {
				return
			}
			slant := math.Hypot(tr.DistKm, tr.AltBaro*0.3048/1000)
			tx := TransmitTime(f).Add(-time.Duration(slant * 1e9 / radioKmPerS)).UnixNano()
			odd := rb(me, 22, 22) == 1
			const ep = int64(200 * time.Millisecond)
			k := (tx + ep/2) / ep
			if (k&1 == 1) != odd {
				if tx >= k*ep {
					k++
				} else {
					k--
				}
			}
			lat = append(lat, float64(tx-k*ep)/1e6)
		})
		if err := s.Advance(context.Background(), 60*time.Second, sink); err != nil {
			t.Fatal(err)
		}
		return ac, lat, badTC
	}
	ac, lat, badTC := run()
	n1 := 0
	for _, t1 := range ac {
		if t1 {
			n1++
		}
	}
	share := float64(n1) / float64(len(ac))
	sort.Float64s(lat)
	t.Logf("%d of %d airborne ADS-B aircraft (%.1f %%) send T=1; transponder latency min %.1f p50 %.1f max %.1f ms (n=%d)",
		n1, len(ac), 100*share, lat[0], lat[len(lat)/2], lat[len(lat)-1], len(lat))
	if share < 0.06 || share > 0.14 {
		t.Errorf("T=1 share %.1f %%, want about 10 %%", 100*share)
	}
	if badTC != 0 {
		t.Errorf("%d T=1 positions with a type code that does not allow T=1", badTC)
	}
	if lat[0] < -150 || lat[len(lat)-1] > 150 {
		t.Errorf("transponder latency outside ±150 ms: %.1f .. %.1f ms", lat[0], lat[len(lat)-1])
	}
	// deterministic
	ac2, _, _ := run()
	for icao, t1 := range ac {
		if v, ok := ac2[icao]; ok && v != t1 {
			t.Fatalf("%06X: T bit differs between identical runs", icao)
		}
	}
}

// TestRecvLikeARealFeed: Recv is stamped per frame (GPS arrival + a
// receiver -> host delay with jitter), monotone over the whole stream, GPS
// time stamps come in arrival order, and no frame waits for its 100 ms step.
func TestRecvLikeARealFeed(t *testing.T) {
	s := New(Config{Aircraft: 200, Seed: 5, Start: testStart, NoDiurnal: true})
	var last time.Time
	var lastGPS int64 = -1
	var d []float64
	nonMono, gpsBack := 0, 0
	sink := funcSink(func(f model.RawFrame) {
		if f.Recv.Before(last) {
			nonMono++
		}
		last = f.Recv
		if f.GPSNs < 0 {
			return
		}
		if f.GPSNs < lastGPS {
			gpsBack++
		}
		lastGPS = f.GPSNs
		sod := int64(f.Recv.Sub(f.Recv.Truncate(24 * time.Hour)))
		d = append(d, float64(sod-f.GPSNs)/1e6)
	})
	if err := s.Advance(context.Background(), 2*time.Minute, sink); err != nil {
		t.Fatal(err)
	}
	sort.Float64s(d)
	q := func(p float64) float64 { return d[int(p*float64(len(d)-1))] }
	t.Logf("receiver -> host delay over %d frames: min %.2f p50 %.2f p99 %.2f max %.2f ms", len(d), d[0], q(0.5), q(0.99), d[len(d)-1])
	if nonMono != 0 || gpsBack != 0 {
		t.Errorf("Recv went backwards %d times, GPS time stamps %d times", nonMono, gpsBack)
	}
	if d[0] <= 0 || d[len(d)-1] >= 100 {
		t.Errorf("receiver delay %.2f .. %.2f ms, want 0 < d < 100 ms", d[0], d[len(d)-1])
	}
	if p50 := q(0.5); p50 < 1 || p50 > 10 {
		t.Errorf("median receiver delay %.2f ms", p50)
	}
	if q(0.99)-d[0] < 0.5 {
		t.Error("receiver delay has no jitter")
	}
}
