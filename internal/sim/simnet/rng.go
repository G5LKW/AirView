package simnet

import (
	"math"
	"math/rand/v2"
)

// splitmix64 is the SplitMix64 finaliser, used to derive independent,
// reproducible sub-seeds from the network seed.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// subSeed derives a seed for one purpose (stream id) of one entity.
func subSeed(seed uint64, parts ...uint64) uint64 {
	h := splitmix64(seed)
	for _, p := range parts {
		h = splitmix64(h ^ splitmix64(p+0x632BE59BD9B4E019))
	}
	return h
}

func newRand(seed uint64, parts ...uint64) *rand.Rand {
	s := subSeed(seed, parts...)
	return rand.New(rand.NewPCG(s, splitmix64(s^0xD1B54A32D192ED03)))
}

// Stream identifiers for subSeed.
const (
	streamClock uint64 = iota + 1
	streamRecv
	streamHost
	streamSpec
	streamAirframe
	streamLink
	streamUUID
	streamEmit
)

func uniform(r *rand.Rand, lo, hi float64) float64 { return lo + (hi-lo)*r.Float64() }

// expRand returns an exponentially distributed value with the given mean.
func expRand(r *rand.Rand, mean float64) float64 { return r.ExpFloat64() * mean }

// poissonHit reports whether an event with the given rate (per second)
// happens within dt seconds.
func poissonHit(r *rand.Rand, ratePerS, dt float64) bool {
	if ratePerS <= 0 {
		return false
	}
	return r.Float64() < -math.Expm1(-ratePerS*dt)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
