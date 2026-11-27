package analytics

import (
	"context"
	"math"
	"math/rand"
	"testing"

	"airview/internal/geo"
	"airview/internal/terrain"
)

// l2World is a synthetic reception world for the Locate Me v2 tests. It is
// deliberately built from other code than the estimator: visibility comes
// from internal/terrain's panorama (or geo.ElevationDeg on a smooth earth),
// the hourly refraction factor k varies, the edge of the radio horizon is
// soft, levels follow a link budget with per-aircraft transmitter power, an
// antenna pattern with an azimuth asymmetry and log-normal fading, and the
// tracker's "one sample per aircraft per 10 s" is modelled as "at least one
// of the 20 position messages of the window was decoded".
type l2World struct {
	Lat, Lon  float64 // true receiver
	GroundM   float64 // terrain under the antenna (m MSL)
	AGL       float64
	Terrain   terrain.Source // nil = smooth earth at sea level
	pano      *terrain.Panorama
	Airports  [][3]float64 // lat, lon, elevation m
	PatternN  float64      // gain = PatternN·10·log10(cos² el)
	AzGainDB  float64      // + AzGainDB·cos(az - AzDeg)
	AzDeg     float64
	SensDBFS  float64 // 50 % decode level of one message
	MaxRangeK float64 // world radius, km
	// Impairments the estimator does not model: ground-reflection lobing of
	// the elevation pattern, per-10° sector shadowing and gain compression
	// of strong signals (AGC / front-end).
	LobeDB, LobePeriodDeg, LobePhase float64
	Sector                           [36]float64
	CompressAboveDBFS                float64
}

// l2Hills is a smooth synthetic terrain: a few Gaussian hills and a ridge.
type l2Hills struct {
	base  float64
	hills [][4]float64 // lat, lon, height m, sigma km
}

func (h l2Hills) ElevationM(lat, lon float64) (float64, bool) {
	z := h.base
	for _, q := range h.hills {
		d := geo.FastDistanceKm(lat, lon, q[0], q[1])
		z += q[2] * math.Exp(-0.5*d*d/(q[3]*q[3]))
	}
	return z, true
}

func (h l2Hills) ResolutionM(float64) float64 { return 150 }

func newL2World(t testing.TB, seed int64, withTerrain bool) *l2World {
	rng := rand.New(rand.NewSource(seed))
	w := &l2World{
		Lat: 35 + rng.Float64()*25, Lon: -10 + rng.Float64()*40,
		AGL: 4 + rng.Float64()*20, PatternN: []float64{1, 3}[rng.Intn(2)],
		AzGainDB: rng.Float64() * 2, AzDeg: rng.Float64() * 360,
		SensDBFS: -36 + rng.Float64()*4, MaxRangeK: 450,
		LobeDB: 1 + rng.Float64()*3, LobePeriodDeg: 3 + rng.Float64()*8, LobePhase: rng.Float64() * 2 * math.Pi,
		CompressAboveDBFS: -14 + rng.Float64()*6,
	}
	for i := range w.Sector {
		w.Sector[i] = rng.NormFloat64() * 2
	}
	if withTerrain {
		h := l2Hills{base: 50 + rng.Float64()*300}
		for i := 0; i < 6; i++ {
			la, lo := geo.Destination(w.Lat, w.Lon, rng.Float64()*360, 3+rng.Float64()*25)
			h.hills = append(h.hills, [4]float64{la, lo, 100 + rng.Float64()*600, 1.5 + rng.Float64()*5})
		}
		w.Terrain = h
		w.GroundM, _ = h.ElevationM(w.Lat, w.Lon)
		pt, err := terrain.SamplePolar(context.Background(), h, terrain.PolarSpec{Lat: w.Lat, Lon: w.Lon, RadiusKm: 120, BearingStepDeg: 1, SampleKm: 0.25}, nil)
		if err != nil {
			t.Fatal(err)
		}
		w.pano = terrain.NewPanorama(pt, w.GroundM+w.AGL, 0, -1)
	}
	nAp := 3 + rng.Intn(3)
	for i := 0; i < nAp; i++ {
		la, lo := geo.Destination(w.Lat, w.Lon, rng.Float64()*360, 8+rng.Float64()*110)
		el := 0.0
		if w.Terrain != nil {
			el, _ = w.Terrain.ElevationM(la, lo)
		}
		w.Airports = append(w.Airports, [3]float64{la, lo, el})
	}
	return w
}

// marginDeg is the elevation of the aircraft above the receiver's horizon
// (terrain panorama or smooth sea-level horizon) for refraction factor k.
func (w *l2World) marginDeg(lat, lon, hM, k float64) (margin, dKm, elevDeg float64) {
	dKm = geo.HaversineKm(w.Lat, w.Lon, lat, lon)
	ant := w.GroundM + w.AGL
	elevDeg = geo.ElevationDeg(dKm, ant, hM, k)
	if w.pano != nil {
		m := w.pano.MarginDeg(lat, lon, geo.MetresToFeet(hM))
		// the panorama uses k = 4/3; correct the curvature for this hour
		m += (dKm * 1000 / (2 * earthM)) * (1/geo.DefaultK - 1/k) * rad2deg
		return m, dKm, elevDeg
	}
	dip := -math.Acos(k*earthM/(k*earthM+ant)) * rad2deg
	return elevDeg - dip, dKm, elevDeg
}

type l2Flight struct {
	icao   uint32
	t0     float64 // s
	off    float64 // transmitter level offset, dB
	segs   []l2Seg
	ground int // number of leading/trailing ground segments: handled per segment
}

type l2Seg struct {
	dur              float64 // s
	lat, lon         float64 // start
	brg, v, vz, h    float64 // bearing, m/s, m/s, start height m MSL
	ground, reversed bool
}

// generate returns the samples of hours of traffic with flightsPerHour new
// flights per hour.
func (w *l2World) generate(seed int64, hours float64, flightsPerHour int) []L2Sample {
	rng := rand.New(rand.NewSource(seed*7919 + 1))
	nF := int(hours * float64(flightsPerHour))
	var out []L2Sample
	nHours := int(math.Ceil(hours)) + 2
	kHour := make([]float64, nHours)
	baroHour := make([]float64, nHours)
	for i := range kHour {
		kHour[i] = 1.2 + rng.Float64()*0.35
		baroHour[i] = rng.NormFloat64() * 80
	}
	t0ms := int64(1_780_000_000_000)
	for f := 0; f < nF; f++ {
		fl := w.flight(rng, uint32(0x400000+f), rng.Float64()*hours*3600)
		var tAcc float64
		for _, sg := range fl.segs {
			for ts := 0.0; ts < sg.dur; ts += 10 {
				t := fl.t0 + tAcc + ts + rng.Float64()*2
				if t > hours*3600 {
					break
				}
				var la, lo, h float64
				dist := sg.v * ts / 1000
				la, lo = geo.Destination(sg.lat, sg.lon, sg.brg, dist)
				h = sg.h + sg.vz*ts
				hr := int(t / 3600)
				k := kHour[hr]
				m, dKm, el := w.marginDeg(la, lo, h, k)
				if dKm > w.MaxRangeK {
					continue
				}
				// soft horizon: diffraction lets a little through below it
				pVis := 1 / (1 + math.Exp(-(m+0.05)/0.06))
				ant := w.GroundM + w.AGL
				slant := geo.SlantRangeKm(dKm, ant, h)
				az := geo.InitialBearing(w.Lat, w.Lon, la, lo)
				c := math.Cos(el / rad2deg)
				gain := w.PatternN*10*math.Log10(math.Max(c*c, 1e-3)) + w.AzGainDB*math.Cos((az-w.AzDeg)/rad2deg) +
					w.LobeDB*math.Sin(2*math.Pi*el/w.LobePeriodDeg+w.LobePhase) + w.Sector[int(az/10)%36]
				mean := fl.off - 20*math.Log10(math.Max(slant, 0.05)) + gain
				// per-message decode probability, 20 messages per window
				pMsg := pVis / (1 + math.Exp(-(mean-w.SensDBFS)/1.8))
				pWin := 1 - math.Pow(1-pMsg*0.95, 20)
				if rng.Float64() > pWin {
					continue
				}
				// the reported level: a decoded message (fading, biased up
				// near the threshold by the selection)
				var lvl float64
				for try := 0; try < 20; try++ {
					lvl = mean + rng.NormFloat64()*2.5
					if lvl > w.SensDBFS-2 {
						break
					}
				}
				if lvl > w.CompressAboveDBFS {
					lvl = w.CompressAboveDBFS + 0.5*(lvl-w.CompressAboveDBFS)
				}
				lvl = math.Min(lvl, -1)
				s := L2Sample{T: t0ms + int64(t*1000), ICAO: fl.icao, Lat: la, Lon: lo,
					AltFt: geo.MetresToFeet(h - baroHour[hr]), RSSI: math.Round(lvl*10) / 10, Ch: 0, Ground: sg.ground}
				if sg.ground {
					s.AltFt = math.NaN()
				}
				out = append(out, s)
			}
			tAcc += sg.dur
		}
	}
	return out
}

func (w *l2World) flight(rng *rand.Rand, icao uint32, t0 float64) l2Flight {
	fl := l2Flight{icao: icao, t0: t0, off: 8 + rng.NormFloat64()*3}
	u := rng.Float64()
	switch {
	case u < 0.5: // en-route overflight
		r := 260 * math.Sqrt(rng.Float64())
		pla, plo := geo.Destination(w.Lat, w.Lon, rng.Float64()*360, r)
		hdg := rng.Float64() * 360
		sla, slo := geo.Destination(pla, plo, hdg+180, 420)
		h := geo.FeetToMetres(25000 + rng.Float64()*16000)
		v := 220 + rng.Float64()*30
		fl.segs = []l2Seg{{dur: 840000 / v, lat: sla, lon: slo, brg: hdg, v: v, h: h}}
	case u < 0.85 && len(w.Airports) > 0: // departure or arrival
		ap := w.Airports[rng.Intn(len(w.Airports))]
		dep := l2Departure(rng, ap)
		if rng.Intn(2) == 0 {
			fl.segs = dep
		} else {
			fl.segs = l2Reverse(dep)
		}
	default: // low general aviation
		r := 160 * math.Sqrt(rng.Float64())
		pla, plo := geo.Destination(w.Lat, w.Lon, rng.Float64()*360, r)
		hdg := rng.Float64() * 360
		sla, slo := geo.Destination(pla, plo, hdg+180, 150)
		h := 300 + rng.Float64()*1300
		fl.segs = []l2Seg{{dur: 300000 / 55.0, lat: sla, lon: slo, brg: hdg, v: 55, h: h}}
	}
	return fl
}

// l2Departure: taxi, take-off and climb out to cruise.
func l2Departure(rng *rand.Rand, ap [3]float64) []l2Seg {
	hdg := rng.Float64() * 360
	var segs []l2Seg
	// taxi: two legs of ~1.5 km
	la, lo := geo.Destination(ap[0], ap[1], rng.Float64()*360, 1.2)
	tb := geo.InitialBearing(la, lo, ap[0], ap[1])
	segs = append(segs, l2Seg{dur: 1200 / 8.0, lat: la, lon: lo, brg: tb, v: 8, h: ap[2] + 4, ground: true})
	segs = append(segs, l2Seg{dur: 1500 / 30.0, lat: ap[0], lon: ap[1], brg: hdg, v: 30, h: ap[2] + 4, ground: true})
	la, lo = geo.Destination(ap[0], ap[1], hdg, 1.5)
	// initial climb 10 m/s to 3000 m, then 7 m/s to cruise
	h := ap[2] + 15
	d1 := (3000 - h) / 10
	segs = append(segs, l2Seg{dur: d1, lat: la, lon: lo, brg: hdg, v: 110, vz: 10, h: h})
	la, lo = geo.Destination(la, lo, hdg, 110*d1/1000)
	cr := geo.FeetToMetres(30000 + rng.Float64()*8000)
	d2 := (cr - 3000) / 7
	segs = append(segs, l2Seg{dur: d2, lat: la, lon: lo, brg: hdg, v: 190, vz: 7, h: 3000})
	la, lo = geo.Destination(la, lo, hdg, 190*d2/1000)
	segs = append(segs, l2Seg{dur: 300000 / 230.0, lat: la, lon: lo, brg: hdg, v: 230, h: cr})
	return segs
}

// l2Reverse turns a departure into an arrival (same path, reversed).
func l2Reverse(in []l2Seg) []l2Seg {
	out := make([]l2Seg, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		s := in[i]
		ela, elo := geo.Destination(s.lat, s.lon, s.brg, s.v*s.dur/1000)
		eh := s.h + s.vz*s.dur
		out = append(out, l2Seg{dur: s.dur, lat: ela, lon: elo, brg: geo.InitialBearing(ela, elo, s.lat, s.lon),
			v: s.v, vz: -s.vz, h: eh, ground: s.ground})
	}
	return out
}
