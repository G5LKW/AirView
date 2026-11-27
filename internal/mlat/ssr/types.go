package ssr

import (
	"math"
	"sort"

	"airview/internal/mlat/geo"
)

// ReplyClass is the kind of SSR reply heard on 1090 MHz.
type ReplyClass uint8

const (
	// ClassModeAC is a Mode A or Mode C reply (F1 ... F2 bracket, 20.3 µs,
	// nominal reply delay 3 µs after P3).
	ClassModeAC ReplyClass = 1
	// ClassModeS is a Mode S all-call reply (DF11, 64 µs, nominal reply
	// delay 128 µs after the P6 sync phase reversal).
	ClassModeS ReplyClass = 2
)

func (c ReplyClass) String() string {
	switch c {
	case ClassModeAC:
		return "modeAC"
	case ClassModeS:
		return "modeS"
	}
	return "unknown"
}

// Reply is one SSR reply received by the station.
type Reply struct {
	// T is the receiver-local time of arrival in seconds. It must be the
	// same time base as the Position reports (the station's own sample
	// clock is ideal; the method only differences times less than a
	// millisecond apart, so any clock that is stable to a few ns over
	// 1 ms works).
	T     float64
	Class ReplyClass
	// Code is the 12-bit Mode A/C code as its octal identity value
	// (A4A2A1 B4B2B1 C4C2C1 D4D2D1, e.g. 0o7700); ModeAC only.
	Code uint16
	// ICAO is the Mode S address (ModeS only; DF11 carries it).
	ICAO uint32
	// IID is the DF11 interrogator identifier (II 1-15, SI 16+N), or 0
	// when unknown / II = 0. ModeS only.
	IID uint8
}

// Position is one decoded position report of a reference aircraft, in the
// station's local time base (Reply.T).
type Position struct {
	T      float64
	P      geo.Vec3 // ECEF, m
	SigmaM float64  // 1σ horizontal error, 0 = Config.TrackSigmaM
	// Squawk is the Mode A code (octal value) or -1 when unknown.
	Squawk int32
	// AltBaroFt is the barometric (pressure) altitude in feet as the
	// transponder encodes it in Mode C, NaN when unknown.
	AltBaroFt float64
}

// AircraftKey identifies an airframe and reply class: the transponder
// reply delay offset is solved per key.
type AircraftKey struct {
	ICAO  uint32
	Class ReplyClass
}

// PairObs is the elementary observation: two aircraft K and J answered the
// same interrogation sequence of one interrogator. Delta is the mean
// arrival time of J's replies minus K's, over N matched interrogations.
type PairObs struct {
	K, J   AircraftKey
	TK, TJ float64  // mean local times of the matched replies, s
	PK, PJ geo.Vec3 // aircraft ECEF positions at TK, TJ
	SK, SJ float64  // their 1σ position errors, m
	VK, VJ geo.Vec3 // their ECEF velocities, m/s (zero when unknown)
	Delta  float64  // s
	N      int      // matched interrogations
	IID    uint8    // DF11 interrogator identifier (0 unknown / Mode A/C)
	Spread float64  // RMS of the individual differences about Delta, s
	Radar  int      // assigned interrogator index (-1 none); set by the solver
}

// ModeCCode returns the Mode C (Gillham) code of a pressure altitude in
// feet, as the 12-bit octal identity value used by Reply.Code, and false
// outside -1200 .. 126700 ft. Encoding (ICAO Annex 10 Vol IV): N = number
// of 100 ft increments above -1200 ft; the 500 ft count N/5 is reflected
// binary (Gray) coded on D2 D4 A1 A2 A4 B1 B2 B4 (MSB first, D1 unused);
// the 100 ft remainder uses the five-state code C1C2C4 = 001, 011, 010,
// 110, 100, run backwards when the 500 ft count is odd. Adjacent 100 ft
// levels therefore differ in exactly one pulse.
func ModeCCode(altFt float64) (uint16, bool) {
	if math.IsNaN(altFt) {
		return 0, false
	}
	n := int(math.Floor((altFt+1200)/100 + 0.5))
	if n < 0 || n >= 1280 {
		return 0, false
	}
	n500, r := n/5, n%5
	if n500&1 == 1 {
		r = 4 - r
	}
	g := n500 ^ (n500 >> 1)
	bit := func(i int) uint16 { return uint16(g>>i) & 1 }
	// g bit7..bit0 = D2 D4 A1 A2 A4 B1 B2 B4
	d2, d4 := bit(7), bit(6)
	a1, a2, a4 := bit(5), bit(4), bit(3)
	b1, b2, b4 := bit(2), bit(1), bit(0)
	c124 := [5][3]uint16{{0, 0, 1}, {0, 1, 1}, {0, 1, 0}, {1, 1, 0}, {1, 0, 0}}[r]
	c1, c2, c4 := c124[0], c124[1], c124[2]
	A := a4<<2 | a2<<1 | a1
	B := b4<<2 | b2<<1 | b1
	C := c4<<2 | c2<<1 | c1
	D := d4<<2 | d2<<1
	return A<<9 | B<<6 | C<<3 | D, true
}

// track is the bounded position history of one aircraft.
type track struct {
	pos []Position // time-sorted
}

// TrackStore holds recent position reports and interpolates them. It is
// not safe for concurrent use (the Estimator serialises access).
type TrackStore struct {
	tracks   map[uint32]*track
	retainS  float64
	maxGapS  float64
	extrapS  float64
	squawk   map[uint16]map[uint32]float64 // code -> icao -> last time
	modeC    map[uint16]map[uint32]float64 // Gillham code -> icao -> last time
	lastPrun float64
}

// NewTrackStore keeps retainS seconds of positions, interpolates across
// gaps up to maxGapS and extrapolates up to extrapS past the ends.
func NewTrackStore(retainS, maxGapS, extrapS float64) *TrackStore {
	return &TrackStore{
		tracks: map[uint32]*track{}, retainS: retainS, maxGapS: maxGapS, extrapS: extrapS,
		squawk: map[uint16]map[uint32]float64{}, modeC: map[uint16]map[uint32]float64{},
	}
}

func addIdx(m map[uint16]map[uint32]float64, code uint16, icao uint32, t float64) {
	s := m[code]
	if s == nil {
		s = map[uint32]float64{}
		m[code] = s
	}
	s[icao] = t
}

// Add stores a position report.
func (s *TrackStore) Add(icao uint32, p Position) {
	tr := s.tracks[icao]
	if tr == nil {
		tr = &track{}
		s.tracks[icao] = tr
	}
	n := len(tr.pos)
	if n > 0 && p.T < tr.pos[n-1].T {
		i := sort.Search(n, func(i int) bool { return tr.pos[i].T > p.T })
		tr.pos = append(tr.pos, Position{})
		copy(tr.pos[i+1:], tr.pos[i:])
		tr.pos[i] = p
	} else {
		tr.pos = append(tr.pos, p)
	}
	if p.Squawk >= 0 && p.Squawk <= 0o7777 {
		addIdx(s.squawk, uint16(p.Squawk), icao, p.T)
	}
	if !math.IsNaN(p.AltBaroFt) {
		for _, d := range [...]float64{-150, -50, 50, 150} {
			if c, ok := ModeCCode(p.AltBaroFt + d); ok {
				addIdx(s.modeC, c, icao, p.T)
			}
		}
	}
	if p.T-s.lastPrun > 10 {
		s.prune(p.T)
	}
}

func (s *TrackStore) prune(now float64) {
	s.lastPrun = now
	lim := now - s.retainS
	for id, tr := range s.tracks {
		i := sort.Search(len(tr.pos), func(i int) bool { return tr.pos[i].T >= lim })
		if i == len(tr.pos) {
			delete(s.tracks, id)
			continue
		}
		if i > 0 {
			tr.pos = append(tr.pos[:0], tr.pos[i:]...)
		}
	}
	for _, m := range [2]map[uint16]map[uint32]float64{s.squawk, s.modeC} {
		for code, set := range m {
			for id, t := range set {
				if t < lim {
					delete(set, id)
				}
			}
			if len(set) == 0 {
				delete(m, code)
			}
		}
	}
}

// Len returns the number of tracked aircraft.
func (s *TrackStore) Len() int { return len(s.tracks) }

// At returns the interpolated position of icao at time t.
func (s *TrackStore) At(icao uint32, t float64) (p geo.Vec3, sigma float64, ok bool) {
	tr := s.tracks[icao]
	if tr == nil || len(tr.pos) == 0 {
		return geo.Vec3{}, 0, false
	}
	ps := tr.pos
	i := sort.Search(len(ps), func(i int) bool { return ps[i].T >= t })
	var a, b Position
	switch {
	case i == 0:
		if ps[0].T-t > s.extrapS || len(ps) < 2 {
			if len(ps) == 1 && math.Abs(ps[0].T-t) <= 0.05 {
				return ps[0].P, ps[0].SigmaM, true
			}
			return geo.Vec3{}, 0, false
		}
		a, b = ps[0], ps[1]
	case i == len(ps):
		if t-ps[i-1].T > s.extrapS || len(ps) < 2 {
			if len(ps) == 1 && math.Abs(ps[0].T-t) <= 0.05 {
				return ps[0].P, ps[0].SigmaM, true
			}
			return geo.Vec3{}, 0, false
		}
		a, b = ps[i-2], ps[i-1]
	default:
		a, b = ps[i-1], ps[i]
		if b.T-a.T > s.maxGapS {
			return geo.Vec3{}, 0, false
		}
	}
	dt := b.T - a.T
	if dt <= 0 {
		return b.P, b.SigmaM, true
	}
	u := (t - a.T) / dt
	p = a.P.Add(b.P.Sub(a.P).Scale(u))
	sigma = math.Max(a.SigmaM, b.SigmaM)
	// Extrapolation and long gaps grow the error.
	if u < 0 || u > 1 {
		ex := math.Max(-u, u-1) * dt
		sigma = math.Hypot(sigma, 20*ex)
	}
	return p, sigma, true
}

// VelAt returns the mean velocity of icao over about ±2.5 s around t
// (a long baseline keeps position noise out of it).
func (s *TrackStore) VelAt(icao uint32, t float64) (geo.Vec3, bool) {
	tr := s.tracks[icao]
	if tr == nil || len(tr.pos) < 2 {
		return geo.Vec3{}, false
	}
	ps := tr.pos
	near := func(tt float64) int {
		i := sort.Search(len(ps), func(i int) bool { return ps[i].T >= tt })
		if i == len(ps) || (i > 0 && tt-ps[i-1].T < ps[i].T-tt) {
			i--
		}
		return i
	}
	a, b := near(t-2.5), near(t+2.5)
	dt := ps[b].T - ps[a].T
	if a >= b || dt < 0.5 || dt > 20 {
		return geo.Vec3{}, false
	}
	return ps[b].P.Sub(ps[a].P).Scale(1 / dt), true
}

// altAt returns the barometric altitude of icao near t.
func (s *TrackStore) altAt(icao uint32, t float64) (float64, bool) {
	tr := s.tracks[icao]
	if tr == nil {
		return 0, false
	}
	ps := tr.pos
	i := sort.Search(len(ps), func(i int) bool { return ps[i].T >= t })
	best, bd := math.NaN(), math.Inf(1)
	for _, k := range [...]int{i - 1, i} {
		if k >= 0 && k < len(ps) && !math.IsNaN(ps[k].AltBaroFt) {
			if d := math.Abs(ps[k].T - t); d < bd {
				best, bd = ps[k].AltBaroFt, d
			}
		}
	}
	if bd > 10 {
		return 0, false
	}
	return best, true
}

// Resolve identifies the aircraft that sent the Mode A/C code at time t:
// an aircraft whose current squawk is code, or whose barometric altitude
// encodes to code (±50 ft encoder rounding). It returns false when no
// aircraft or more than one matches (ambiguous replies are dropped).
func (s *TrackStore) Resolve(code uint16, t float64, conspicuity []uint16) (uint32, bool) {
	id, n := s.ResolveN(code, t, conspicuity)
	return id, n == 1
}

// ResolveN is Resolve returning the number of candidates (the identity is
// valid only when it is 1).
func (s *TrackStore) ResolveN(code uint16, t float64, conspicuity []uint16) (uint32, int) {
	var found uint32
	n := 0
	seen := func(id uint32) {
		switch {
		case n == 0:
			found, n = id, 1
		case id != found:
			n++
		}
	}
	squawkOK := true
	for _, c := range conspicuity {
		if c == code {
			squawkOK = false
		}
	}
	if squawkOK {
		for id, last := range s.squawk[code] {
			if t-last < 60 && last-t < 60 {
				seen(id)
			}
		}
	}
	for id, last := range s.modeC[code] {
		if math.Abs(t-last) > 30 {
			continue
		}
		alt, ok := s.altAt(id, t)
		if !ok {
			continue
		}
		c1, ok1 := ModeCCode(alt - 50)
		c2, ok2 := ModeCCode(alt + 50)
		c0, ok0 := ModeCCode(alt)
		if (ok0 && c0 == code) || (ok1 && c1 == code) || (ok2 && c2 == code) {
			seen(id)
		}
	}
	if n != 1 {
		return 0, n
	}
	return found, 1
}
