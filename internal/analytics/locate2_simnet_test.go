package analytics

import (
	"context"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"airview/internal/geo"
	"airview/internal/sim/simnet"
)

// Locate Me v2 on internal/sim/simnet: an independent simulation (written
// for the MLAT work, not for this estimator) with its own traffic model
// (internal/sim: overflights, arrivals, departures, taxiing), link budget,
// soft horizon with refraction, antenna pattern, fading and detection
// curve. The station is placed away from the centre of the simulated
// world so that the traffic is not centred on it.

// l2SimSink turns one receiver's observations into guess samples the way
// the tracker does: ADS-B position squitters, at most one per aircraft per
// 10 s, with the reported signal level.
type l2SimSink struct {
	n    *simnet.Network
	last map[uint32]float64
	out  []L2Sample
}

func (s *l2SimSink) Observe(o *simnet.Observation) {
	if len(o.Data) < 11 || o.Data[0]>>3 != 17 {
		return
	}
	tc := o.Data[4] >> 3
	if !((tc >= 5 && tc <= 18) || (tc >= 20 && tc <= 22)) {
		return
	}
	if t, ok := s.last[o.ICAO]; ok && o.ArrivalS-t < 10 {
		return
	}
	a, ok := s.n.AircraftAt(o.ICAO, o.ArrivalS)
	if !ok {
		return
	}
	s.last[o.ICAO] = o.ArrivalS
	alt := a.Sim.AltBaro
	if a.Sim.GNSSAlt {
		alt = a.Sim.AltGeom
	}
	smp := L2Sample{T: s.n.UnixNs(o.ArrivalS) / 1e6, ICAO: o.ICAO, Lat: a.Sim.Lat, Lon: a.Sim.Lon,
		AltFt: alt, RSSI: math.Round(o.SignalDBFS*10) / 10, Ch: 0, Ground: tc <= 8}
	if smp.Ground {
		smp.AltFt = math.NaN()
	}
	s.out = append(s.out, smp)
}

func (s *l2SimSink) Advance(float64) {}

func l2SimRun(t *testing.T, seed uint64, hours float64, aircraft int) (float64, *Locate2Result) {
	t.Helper()
	specs := simnet.RandomReceivers(seed, 1, simnet.Placement{CenterLat: 48 + float64(seed%7), CenterLon: 2 + float64(seed%11), RadiusKm: 10})
	rx := specs[0]
	// the world centre 40..80 km away from the station
	cla, clo := geo.Destination(rx.Lat, rx.Lon, float64(seed*67%360), 40+float64(seed*13%40))
	n, err := simnet.New(simnet.Config{Seed: seed, Aircraft: aircraft, Receivers: specs, CenterLat: cla, CenterLon: clo, RangeKm: 450})
	if err != nil {
		t.Fatal(err)
	}
	sink := &l2SimSink{n: n, last: map[uint32]float64{}}
	n.Attach(0, sink)
	if err := n.Advance(context.Background(), time.Duration(hours*float64(time.Hour))); err != nil {
		t.Fatal(err)
	}
	r, err := Locate2(context.Background(), sink.out, Locate2Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Fatalf("seed %d: %s", seed, r.Reason)
	}
	truth := n.Receiver(0)
	e := geo.HaversineKm(truth.Lat, truth.Lon, r.Lat, r.Lon)
	t.Logf("simnet seed %d: %d samples, %d aircraft: error %.2f km  r95 %.2f km  %d ms", seed, r.Samples, r.Aircraft, e, float64(r.R95Km), r.ElapsedMs)
	return e, r
}

func TestLocate2Simnet(t *testing.T) {
	if testing.Short() {
		t.Skip("simulates hours of traffic")
	}
	n, _ := strconv.Atoi(os.Getenv("AIRVIEW_LOCATE2_SIMNET"))
	if n <= 0 {
		n = 2
	}
	var errs []float64
	cover := 0
	for seed := uint64(1); seed <= uint64(n); seed++ {
		e, r := l2SimRun(t, seed, 3, 300)
		errs = append(errs, e)
		if e <= float64(r.R95Km) {
			cover++
		}
	}
	q := l2Report(t, "simnet 3h", errs)
	t.Logf("truth inside the 95 %% circle in %d of %d", cover, n)
	if q(0.5) > 10 {
		t.Errorf("median error %.2f km > 10 km", q(0.5))
	}
}
