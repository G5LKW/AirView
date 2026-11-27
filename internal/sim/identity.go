package sim

// Identity generation: ICAO 24-bit addresses drawn from the real state
// allocation blocks (ICAO Annex 10 Vol III), airline-style callsigns,
// emitter categories and squawks.

// operator is an airline: ICAO callsign prefix and the address sub-block its
// fleet is drawn from (inside the block of the state of registry).
type operator struct {
	prefix string
	lo, hi uint32
	heavy  float64 // probability of emitter category A5 (heavy) instead of A3
	eu, na float64 // selection weight for a European / North American station
}

var airlines = []operator{
	// United Kingdom 400000-43FFFF
	{"BAW", 0x400400, 0x407FFF, 0.35, 8, 2},
	{"EZY", 0x400400, 0x407FFF, 0, 9, 0},
	{"VIR", 0x400400, 0x407FFF, 1, 1.5, 1.5},
	{"EXS", 0x400400, 0x407FFF, 0, 3, 0},
	{"TOM", 0x400400, 0x407FFF, 0.2, 2.5, 0},
	// Ireland 4CA000-4CAFFF
	{"RYR", 0x4CA000, 0x4CAFFF, 0, 12, 0},
	{"EIN", 0x4CA000, 0x4CAFFF, 0.2, 2, 1},
	// Germany 3C0000-3FFFFF
	{"DLH", 0x3C4000, 0x3C7FFF, 0.3, 8, 2},
	{"EWG", 0x3C4000, 0x3C7FFF, 0, 4, 0},
	{"CFG", 0x3C4000, 0x3C7FFF, 0.3, 1.5, 0.5},
	{"GEC", 0x3C4000, 0x3C7FFF, 1, 0.7, 0.3},
	// Netherlands 480000-487FFF
	{"KLM", 0x484000, 0x485FFF, 0.3, 6, 2},
	{"TRA", 0x484000, 0x485FFF, 0, 2.5, 0},
	// France 380000-3BFFFF
	{"AFR", 0x390000, 0x39FFFF, 0.3, 6, 2},
	// Spain 340000-37FFFF
	{"IBE", 0x340000, 0x347FFF, 0.3, 3, 1},
	{"VLG", 0x340000, 0x347FFF, 0, 4, 0},
	// Italy 300000-33FFFF
	{"ITY", 0x300000, 0x303FFF, 0.2, 2, 0.5},
	// Switzerland 4B0000-4B7FFF
	{"SWR", 0x4B0000, 0x4B3FFF, 0.3, 3, 1},
	// Austria 440000-447FFF
	{"AUA", 0x440000, 0x443FFF, 0.15, 2, 0.5},
	{"EJU", 0x440000, 0x447FFF, 0, 5, 0},
	// Hungary 470000-477FFF
	{"WZZ", 0x470000, 0x473FFF, 0, 6, 0},
	// Sweden 4A8000-4AFFFF, Norway 478000-47FFFF, Finland 460000-467FFF
	{"SAS", 0x4A8000, 0x4ABFFF, 0.1, 3, 0.5},
	{"NOZ", 0x478000, 0x47BFFF, 0, 2.5, 0},
	{"FIN", 0x460000, 0x463FFF, 0.3, 1.5, 0.5},
	// Poland 488000-48FFFF, Portugal 490000-497FFF
	{"LOT", 0x488000, 0x48BFFF, 0.15, 2, 0.5},
	{"TAP", 0x490000, 0x493FFF, 0.2, 2, 0.5},
	// Turkey 4B8000-4BFFFF
	{"THY", 0x4B8000, 0x4BBFFF, 0.5, 4, 1},
	{"PGT", 0x4B8000, 0x4BBFFF, 0, 2, 0},
	// Belgium 448000-44FFFF, Iceland 4CC000-4CCFFF, Luxembourg 4D0000-4D03FF
	{"BEL", 0x448000, 0x44BFFF, 0.2, 1.5, 0.3},
	{"ICE", 0x4CC000, 0x4CC7FF, 0.1, 1, 1},
	{"CLX", 0x4D0000, 0x4D03BF, 1, 1, 0.5},
	// United Arab Emirates 896000-896FFF, Qatar 06A000-06A3FF
	{"UAE", 0x896000, 0x896FFF, 1, 2.5, 1.5},
	{"ETD", 0x896000, 0x896FFF, 1, 1, 0.5},
	{"QTR", 0x06A000, 0x06A3FF, 1, 2.5, 1},
	// Singapore 768000-76FFFF, China 780000-7BFFFF, Japan 840000-87FFFF
	{"SIA", 0x768000, 0x76FFFF, 1, 1, 0.5},
	{"CCA", 0x780000, 0x78FFFF, 1, 1, 1},
	{"JAL", 0x840000, 0x86FFFF, 1, 0.5, 1},
	// Canada C00000-C3FFFF
	{"ACA", 0xC00000, 0xC0FFFF, 0.6, 1.5, 6},
	// United States A00000-AFFFFF (civil N-numbers A00001-ADF7C7)
	{"UAL", 0xA00001, 0xADF7C7, 0.5, 2.5, 14},
	{"AAL", 0xA00001, 0xADF7C7, 0.35, 2, 14},
	{"DAL", 0xA00001, 0xADF7C7, 0.35, 2, 14},
	{"SWA", 0xA00001, 0xADF7C7, 0, 0, 12},
	{"JBU", 0xA00001, 0xADF7C7, 0, 0.2, 5},
	{"FDX", 0xA00001, 0xADF7C7, 1, 1.5, 5},
	{"UPS", 0xA00001, 0xADF7C7, 1, 1.5, 4},
}

// regPattern is a general-aviation registration scheme used as callsign.
type regPattern struct {
	prefix  string
	letters int // random letters after the prefix
	nNumber bool
	lo, hi  uint32
	eu, na  float64
}

var gaRegs = []regPattern{
	{"G", 4, false, 0x401000, 0x407FFF, 6, 0},
	{"DE", 3, false, 0x3D0000, 0x3D7FFF, 5, 0},
	{"DM", 3, false, 0x3D8000, 0x3DFFFF, 1, 0},
	{"PH", 3, false, 0x484000, 0x487FFF, 2, 0},
	{"FG", 3, false, 0x380000, 0x39FFFF, 3, 0},
	{"OO", 3, false, 0x448000, 0x44FFFF, 1, 0},
	{"HB", 3, false, 0x4B0000, 0x4B7FFF, 1, 0},
	{"OE", 3, false, 0x440000, 0x447FFF, 1, 0},
	{"N", 0, true, 0xA00001, 0xADF7C7, 0.5, 10},
	{"CG", 3, false, 0xC00000, 0xC3FFFF, 0, 2},
}

// milOp is a military operator: callsign prefix and a military address block.
type milOp struct {
	prefix string
	lo, hi uint32
	eu, na float64
}

var milOps = []milOp{
	{"RRR", 0x43C000, 0x43CFFF, 4, 0.3},  // Royal Air Force
	{"GAF", 0x3F4000, 0x3F7FFF, 3, 0.2},  // German Air Force
	{"FAF", 0x3B7000, 0x3BFFFF, 2, 0.2},  // French Air Force
	{"BAF", 0x44F000, 0x44FFFF, 1, 0},    // Belgian Air Force
	{"NAF", 0x480000, 0x480FFF, 1, 0},    // Royal Netherlands Air Force
	{"NATO", 0x4D03C0, 0x4D03FF, 1, 0.1}, // NATO AEW (Luxembourg registry)
	{"RCH", 0xAE0000, 0xAE7FFF, 2, 6},    // USAF Air Mobility Command
	{"PAT", 0xAE0000, 0xAE7FFF, 0.3, 2},  // US Army
}

var heliCalls = []string{"HLE", "NPAS", "CHX", "LIFE", "RESQ"}

const regLetters = "ABCDEFGHJKLMNPRSTUVWXYZ" // no I, O, Q

func (s *Sim) weight(eu, na float64) float64 {
	if s.regionNA {
		return na
	}
	return eu
}

// pickIndex draws an index with probability proportional to w(i).
func (s *Sim) pickIndex(n int, w func(i int) float64) int {
	var total float64
	for i := range n {
		total += w(i)
	}
	r := s.rng.Float64() * total
	for i := range n {
		r -= w(i)
		if r < 0 {
			return i
		}
	}
	return n - 1
}

// newICAO draws an unused address from [lo, hi].
func (s *Sim) newICAO(lo, hi uint32) uint32 {
	for range 64 {
		v := lo + s.rng.Uint32N(hi-lo+1)
		if _, dup := s.used[v]; !dup {
			s.used[v] = struct{}{}
			return v
		}
	}
	// Block exhausted (tiny blocks with many aircraft): linear probe.
	for v := lo; v <= hi; v++ {
		if _, dup := s.used[v]; !dup {
			s.used[v] = struct{}{}
			return v
		}
	}
	// Fall back to any free address inside the widest block we use.
	for {
		v := 0xA00001 + s.rng.Uint32N(0xDF7C6)
		if _, dup := s.used[v]; !dup {
			s.used[v] = struct{}{}
			return v
		}
	}
}

type csBuf struct {
	b [8]byte
	n int
}

func (c *csBuf) str(s string) {
	for i := 0; i < len(s) && c.n < 8; i++ {
		c.b[c.n] = s[i]
		c.n++
	}
}

func (c *csBuf) ch(b byte) {
	if c.n < 8 {
		c.b[c.n] = b
		c.n++
	}
}

func (c *csBuf) num(v, minDigits int) {
	var tmp [6]byte
	i := len(tmp)
	for v > 0 || minDigits > 0 {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
		minDigits--
		if i == 0 {
			break
		}
	}
	for ; i < len(tmp); i++ {
		c.ch(tmp[i])
	}
}

func (c *csBuf) done() [8]byte {
	for c.n < 8 {
		c.b[c.n] = ' '
		c.n++
	}
	return c.b
}

func (s *Sim) letter() byte { return regLetters[s.rng.IntN(len(regLetters))] }

// airlineCallsign builds e.g. "BAW117", "EZY82KM", "RYR4TK".
func (s *Sim) airlineCallsign(prefix string) [8]byte {
	var c csBuf
	c.str(prefix)
	switch r := s.rng.Float64(); {
	case r < 0.62:
		c.num(1+s.rng.IntN(1999), 1)
	case r < 0.75:
		c.num(2000+s.rng.IntN(7000), 1)
	case r < 0.90:
		c.num(1+s.rng.IntN(99), 1)
		c.ch(s.letter())
		c.ch(s.letter())
	default:
		c.num(1+s.rng.IntN(9), 1)
		c.ch(s.letter())
		c.ch(s.letter())
	}
	return c.done()
}

// regCallsign builds a registration-style callsign such as "GBXYZ", "DEKLM", "N512PA".
func (s *Sim) regCallsign(p *regPattern) [8]byte {
	var c csBuf
	c.str(p.prefix)
	if p.nNumber {
		c.num(10+s.rng.IntN(9989), 2)
		for range s.rng.IntN(3) {
			c.ch(s.letter())
		}
	} else {
		for range p.letters {
			c.ch(s.letter())
		}
	}
	return c.done()
}

// discreteSquawk draws an ATC-assigned Mode A code (four octal digits packed
// into nibbles), avoiding the special-purpose codes.
func (s *Sim) discreteSquawk() uint16 {
	for {
		a := uint16(s.rng.IntN(8))
		b := uint16(s.rng.IntN(8))
		c := uint16(s.rng.IntN(8))
		d := uint16(s.rng.IntN(8))
		v := a<<12 | b<<8 | c<<4 | d
		if c == 0 && d == 0 { // non-discrete xx00 codes
			continue
		}
		switch v {
		case 0x7500, 0x7600, 0x7700, 0x7000, 0x7777, 0x1200, 0x2000, 0x1000:
			continue
		}
		return v
	}
}

func (s *Sim) vfrSquawk() uint16 {
	if s.regionNA {
		return 0x1200
	}
	return 0x7000
}

func squawkString(sq uint16) string {
	return string([]byte{
		'0' + byte(sq>>12&7), '0' + byte(sq>>8&7), '0' + byte(sq>>4&7), '0' + byte(sq&7),
	})
}

// identify fills in the identity and equipage of a new aircraft of a.kind.
func (s *Sim) identify(a *aircraft) {
	r := s.rng
	a.version = 2
	a.adsb = true
	a.catTC = 4 // category set A
	a.posTC = 11
	a.nacp = 9
	a.nacv = 2
	a.sil = 3
	a.sqChanged = -1e9
	switch x := r.Float64(); {
	case x < 0.15:
		a.posTC, a.nacp = 12, 8
	case x < 0.22:
		a.posTC, a.nacp = 13, 7
		a.nacv = 1
	case x < 0.32:
		a.posTC, a.nacp = 10, 10
	}

	switch a.kind {
	case kOverflight, kArrival, kDeparture:
		i := s.pickIndex(len(airlines), func(i int) float64 { return s.weight(airlines[i].eu, airlines[i].na) })
		op := &airlines[i]
		a.icao = s.newICAO(op.lo, op.hi)
		a.callsign = s.airlineCallsign(op.prefix)
		a.cat = 3 // large
		a.lenWidth = 6
		if r.Float64() < op.heavy {
			a.cat = 5 // heavy
			a.lenWidth = 12
		} else if r.Float64() < 0.04 {
			a.cat = 4 // high vortex large (B757)
			a.lenWidth = 8
		} else if r.Float64() < 0.06 {
			a.cat = 2 // small (regional / business jet)
			a.lenWidth = 3
		}
		a.squawk = s.discreteSquawk()
		if r.Float64() < 0.15 {
			a.squawk = 0x1000 // Mode S conspicuity code
		}
		a.eirp = clamp(54+1.5*r.NormFloat64(), 50, 57)
		a.diversity = true
		a.acas = true
		a.ehs = true
		a.mrar = r.Float64() < 0.12
		a.vrBaro = r.Float64() < 0.7
		a.survRate = 0.8 + 1.7*r.Float64()
		a.acasRate = 0.3 + 0.9*r.Float64()
		a.longFrac = 0.5
		if r.Float64() < 0.12 {
			a.version = 0
		}
	case kGA, kHeli:
		i := s.pickIndex(len(gaRegs), func(i int) float64 { return s.weight(gaRegs[i].eu, gaRegs[i].na) })
		p := &gaRegs[i]
		a.icao = s.newICAO(p.lo, p.hi)
		a.callsign = s.regCallsign(p)
		a.cat = 1 // light
		a.lenWidth = 1
		if a.kind == kHeli {
			a.cat = 7 // rotorcraft
			if r.Float64() < 0.5 {
				var c csBuf
				c.str(heliCalls[r.IntN(len(heliCalls))])
				c.num(1+r.IntN(98), 2)
				a.callsign = c.done()
			}
		} else if x := r.Float64(); x < 0.06 {
			a.catTC, a.cat = 3, 1 // set B: glider / sailplane
		} else if x < 0.10 {
			a.catTC, a.cat = 3, 4 // set B: ultralight
		}
		a.squawk = s.vfrSquawk()
		if r.Float64() < 0.25 {
			a.squawk = s.discreteSquawk()
		}
		a.eirp = clamp(48+1.5*r.NormFloat64(), 44, 52)
		a.singleAnt = true
		a.vrBaro = true
		a.survRate = 0.2 + 0.5*r.Float64()
		a.longFrac = 0.12
		a.nacp, a.posTC = 8, 12
		if r.Float64() < 0.3 {
			a.version = 0
		}
	case kMil:
		i := s.pickIndex(len(milOps), func(i int) float64 { return s.weight(milOps[i].eu, milOps[i].na) })
		op := &milOps[i]
		a.icao = s.newICAO(op.lo, op.hi)
		var c csBuf
		c.str(op.prefix)
		c.num(1+r.IntN(9998), 2)
		a.callsign = c.done()
		a.cat = 6
		a.adsb = false
		a.squawk = s.discreteSquawk()
		a.eirp = clamp(53+2*r.NormFloat64(), 48, 57)
		a.diversity = true
		a.ehs = r.Float64() < 0.4
		a.survRate = 0.8 + 1.2*r.Float64()
		a.acasRate = 0
		if r.Float64() < 0.4 {
			a.acas = true
			a.acasRate = 0.2 + 0.5*r.Float64()
		}
		a.longFrac = 0.35
	}

	if a.kind != kMil {
		if r.Float64() < s.cfg.MLATFraction {
			// Mode S only airframe, positioned by the MLAT network.
			a.adsb = false
			a.mlat = true
		} else if r.Float64() < s.cfg.GNSSAltFraction {
			a.gnssPos = true
		}
	}
	if a.version == 0 {
		a.mrar = false
	}
	s.utcCoupling(a)
}

// utcEvery: every utcEvery-th ADS-B aircraft created has a UTC-coupled
// position source (T=1), 12.5 % of the ADS-B fleet or about 10 % of all
// aircraft (MLAT-only and military airframes never set it). Field studies
// report about 1 %; the demo uses more so that the timing page has
// something to show. Counting instead of drawing keeps the share right
// in small demo fleets (50 aircraft: 5 or 6 senders, not 2..8).
const utcEvery = 8

// utcCoupling decides - deterministically from the seed and the creation
// order, without touching the world's random sequence - whether a sets the
// TIME bit, and its register lead. T=1 is only allowed with the two best
// precision categories, so such airframes report NIC 11 / NIC 10 (TC 9 /
// TC 10, or TC 20 for GNSS height). Caller holds s.mu (or owns s).
func (s *Sim) utcCoupling(a *aircraft) {
	h := mix64(s.seed ^ uint64(a.icao)*0x9E3779B97F4A7C15)
	a.utcT = false
	if a.adsb {
		a.utcT = (s.utcSeq+s.seed)%utcEvery == 0
		s.utcSeq++
	}
	if !a.utcT {
		return
	}
	a.t1Lead = int64(50e6) + int64((h>>20)%100)*1e6 // 50..149 ms
	if h>>40&1 == 0 {
		a.posTC, a.nacp = 9, 11
	} else {
		a.posTC, a.nacp = 10, 10
	}
}

// mix64 is the SplitMix64 finaliser.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	return x ^ x>>31
}

// floorDivI is floor(a / b) for b > 0.
func floorDivI(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}
