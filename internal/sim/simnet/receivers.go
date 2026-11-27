package simnet

import (
	"fmt"
	"math"
	"sort"
)

// Placement describes a random receiver layout for RandomReceivers.
type Placement struct {
	// CenterLat / CenterLon / RadiusKm: receivers are spread uniformly over
	// the disc (default radius 300 km).
	CenterLat, CenterLon float64
	RadiusKm             float64
	// Mix gives the relative weights of the devices (nil = DefaultMix).
	Mix map[Device]float64
	// DeclaredErrorMaxM spreads declared position errors uniformly over
	// 0..max (0 = declared positions are exact).
	DeclaredErrorMaxM float64
}

// DefaultMix is a community-network-like device mix: mostly RTL-SDRs, some
// Beasts and Airspys, a few GPS Radarcapes and SBSs.
var DefaultMix = map[Device]float64{
	DeviceSDR:          0.62,
	DeviceAirspy:       0.10,
	DeviceBeast:        0.10,
	DeviceRadarcape12:  0.04,
	DeviceRadarcapeGPS: 0.10,
	DeviceSBS:          0.04,
}

// RandomReceivers returns n reproducible receivers (DefaultClockSpec and
// DefaultReception for their device) for a seed and placement.
func RandomReceivers(seed uint64, n int, p Placement) []ReceiverSpec {
	if !(p.RadiusKm > 0) {
		p.RadiusKm = 300
	}
	mix := p.Mix
	if len(mix) == 0 {
		mix = DefaultMix
	}
	devs := make([]Device, 0, len(mix))
	total := 0.0
	for d, w := range mix {
		if w > 0 {
			devs = append(devs, d)
			total += w
		}
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i] < devs[j] })
	r := newRand(seed, streamSpec, uint64(n))
	out := make([]ReceiverSpec, n)
	for i := range out {
		x := r.Float64() * total
		dev := devs[len(devs)-1]
		for _, d := range devs {
			if x < mix[d] {
				dev = d
				break
			}
			x -= mix[d]
		}
		dist := p.RadiusKm * 1000 * math.Sqrt(r.Float64())
		lat, lon := destination(p.CenterLat, p.CenterLon, r.Float64()*360, dist)
		s := ReceiverSpec{
			ID:         fmt.Sprintf("rx%04d", i),
			Lat:        lat,
			Lon:        lon,
			GroundHAE:  uniform(r, 40, 400),
			AntennaAGL: uniform(r, 4, 30),
			Clock:      DefaultClockSpec(dev, r),
			Reception:  DefaultReception(dev, r),
			Link:       LinkSpec{LatencyMs: uniform(r, 5, 60), JitterMs: uniform(r, 0, 10)},
		}
		if p.DeclaredErrorMaxM > 0 {
			s.DeclaredErrorM = r.Float64() * p.DeclaredErrorMaxM
		}
		out[i] = s
	}
	return out
}
