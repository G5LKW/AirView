package rfstat

// acState is the per-aircraft state. It lives in a fixed array that is
// allocated once (Config.MaxAircraft) and indexed by an open-addressing hash
// table, so Observe never allocates.
type acState struct {
	icao       uint32
	used       bool
	flags      uint32
	first      int64 // ns
	last       int64 // ns
	frames     uint32
	lastSurv   int64 // ns of last DF4/5/20/21
	survN      uint32
	df11N      uint32
	acasN      uint32 // DF0 + DF16
	raN        uint32
	lastADSB   int64
	lastPosT   int64 // ns, last airborne position (update gaps)
	lastTypeT  [nPdTypes]int64
	posTMinute int64 // ns, last position for GNSS index

	// equipage
	version, nic, nacp, sil int8
	maxNIC, maxNACp         int8
	category                uint8
	degraded                bool
	jumps                   uint16
	jumpsMin                uint16 // jumps in the current minute
	lastLat, lastLon        float64
	lastLatT                int64
	lastJumpT               int64

	// geometry (last known)
	dist, brg, elev, alt, track float32
	rssiEWMA                    float32
	residEWMA                   float32
	residN                      uint32

	// probability of detection window
	pdStart int64
	pdWarm  bool
	pdSeen  uint8
	pdCnt   [nPdTypes]uint16
	pdRSum  float32
	pdRN    uint16
	lifeExp [nPdTypes]float32
	lifeGot [nPdTypes]uint32

	// fading window
	fadeStart          int64
	fadeN              uint16
	fP, fP2, fDb, fDb2 float64
	kEWMA, stdEWMA     float32
	fadeWins           uint16

	// GPS timing
	gpsFeed     int8
	lastGPS     [nTimeKind]int64
	lastOdd     bool
	tN, tBad    uint32
	t1N         uint32
	t1Sum       float64
	t1Sq        float64
	altN, altOK uint32

	// interrogators that elicited DF11 replies (IID+1, 0 = empty)
	codes    [8]uint8
	codeLast [8]int64
}

// Flags.
const (
	fADSB      = 1 << iota // DF17 seen
	fSurv                  // DF4/5/20/21 seen
	fACAS                  // DF0/16 seen
	fTC28                  // aircraft status
	fTC29                  // target state
	fTC31                  // operational status
	fGNSSAlt               // GNSS altitude (TC20-22) or GNSS delta seen
	fGround                // on ground (last)
	fPos                   // airborne position seen
	fTimingBad             // GPS interval compliance flagged
	fDF18                  // DF18 (non-transponder / TIS-B / ADS-R)
)

// table is a fixed-capacity hash table (linear probing, backward-shift delete).
type table struct {
	slots []acState
	idx   []int32 // -1 = empty; else slot index
	free  []int32 // stack of free slot indexes
	mask  uint32
	n     int
}

func newTable(capacity int) *table {
	size := 1
	for size < capacity*2 {
		size <<= 1
	}
	t := &table{
		slots: make([]acState, capacity),
		idx:   make([]int32, size),
		free:  make([]int32, capacity),
		mask:  uint32(size - 1),
	}
	for i := range t.idx {
		t.idx[i] = -1
	}
	for i := range t.free {
		t.free[i] = int32(capacity - 1 - i)
	}
	return t
}

func hash32(x uint32) uint32 {
	x ^= x >> 16
	x *= 0x7feb352d
	x ^= x >> 15
	x *= 0x846ca68b
	x ^= x >> 16
	return x
}

// get returns the state for icao, inserting it when absent (nil when full).
func (t *table) get(icao uint32) (*acState, bool) {
	h := hash32(icao) & t.mask
	for {
		si := t.idx[h]
		if si < 0 {
			break
		}
		if t.slots[si].icao == icao {
			return &t.slots[si], false
		}
		h = (h + 1) & t.mask
	}
	if len(t.free) == 0 {
		return nil, false
	}
	si := t.free[len(t.free)-1]
	t.free = t.free[:len(t.free)-1]
	t.idx[h] = si
	t.n++
	s := &t.slots[si]
	*s = acState{icao: icao, used: true}
	return s, true
}

// remove deletes icao from the table.
func (t *table) remove(icao uint32) {
	h := hash32(icao) & t.mask
	for {
		si := t.idx[h]
		if si < 0 {
			return
		}
		if t.slots[si].icao == icao {
			break
		}
		h = (h + 1) & t.mask
	}
	si := t.idx[h]
	t.slots[si].used = false
	t.free = append(t.free, si) // capacity is preallocated: no allocation
	t.n--
	// backward-shift deletion
	i := h
	j := h
	for {
		j = (j + 1) & t.mask
		sj := t.idx[j]
		if sj < 0 {
			break
		}
		k := hash32(t.slots[sj].icao) & t.mask
		// move j to i if k is cyclically outside (i, j]
		if (i <= j && (k <= i || k > j)) || (i > j && k <= i && k > j) {
			t.idx[i] = sj
			i = j
		}
	}
	t.idx[i] = -1
}
