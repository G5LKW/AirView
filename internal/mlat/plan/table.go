package plan

import (
	"sync"

	"airview/internal/ingest"
	"airview/internal/model"
)

// MaxHearers is the most stations remembered per aircraft (the stalest is
// replaced). In a dense network of 1,000 feeders a high aircraft is heard by
// well over 100 stations, and every one of them needs to know it hears the
// aircraft to choose its sync set.
const MaxHearers = 256

const (
	nShards      = 64
	noteEveryNs  = int64(1e9) // hearer lists are refreshed at most once a second per aircraft
	rateTauNs    = 10e9       // time constant of the frame-rate EWMA
	shardKeyMask = nShards - 1
)

// hearer is a station that heard an aircraft recently.
type hearer struct {
	st     model.StationIdx
	lastDs uint32 // deciseconds since the planner's base time
	n      uint16 // notes (saturating)
	sig    int8   // last RSSI, dBFS (rounded)
	_      int8
}

// acEntry is one aircraft.
type acEntry struct {
	key          uint32
	adsbPoor     bool
	lastSeen     int64
	lastNote     int64
	lastADSB     int64
	suspectUntil int64
	rate         float32 // groups/s (EWMA)
	tok          float32 // solve tokens
	tokAt        int64
	h            []hearer // sorted by station
}

type shard struct {
	mu   sync.Mutex
	idx  map[uint32]int32
	recs []acEntry
	free []int32
	// max bounds len(idx): beyond it the least recently seen of a sample
	// of entries is evicted (approximate LRU).
	max     int
	evicted uint64
	// pend holds aircraft reported only by visibility lists (mlat-client
	// "seen") from one station so far: a second station or a frame
	// creates the entry (finding ROB-3). Bounded by max.
	pend map[uint32]pendHear
}

// pendHear is the first visibility report of an unknown aircraft.
type pendHear struct {
	st model.StationIdx
	ns int64
}

type table struct {
	sh     [nShards]shard
	baseNs int64
}

func (t *table) init(baseNs int64, maxAircraft int) {
	t.baseNs = baseNs
	per := max((maxAircraft+nShards-1)/nShards, 16)
	for i := range t.sh {
		t.sh[i].idx = map[uint32]int32{}
		t.sh[i].pend = map[uint32]pendHear{}
		t.sh[i].max = per
	}
}

// evictSample is the number of entries an eviction compares.
const evictSample = 8

// evictOne frees the least recently seen entry of a sample (caller holds
// mu) and returns its slot.
func (s *shard) evictOne() int32 {
	victim, vk, best := int32(-1), uint32(0), int64(0)
	n := 0
	for k, j := range s.idx {
		if ls := s.recs[j].lastSeen; victim < 0 || ls < best {
			victim, vk, best = j, k, ls
		}
		if n++; n >= evictSample {
			break
		}
	}
	delete(s.idx, vk)
	s.evicted++
	return victim
}

func shardOf(key uint32) int { return int((key ^ key>>6 ^ key>>12) & shardKeyMask) }

// get returns the entry of key, creating it. Caller holds the shard lock.
func (s *shard) get(key uint32) *acEntry {
	if i, ok := s.idx[key]; ok {
		return &s.recs[i]
	}
	var i int32
	if n := len(s.free); n > 0 {
		i = s.free[n-1]
		s.free = s.free[:n-1]
	} else if s.max > 0 && len(s.idx) >= s.max {
		i = s.evictOne()
	} else {
		s.recs = append(s.recs, acEntry{})
		i = int32(len(s.recs) - 1)
	}
	s.idx[key] = i
	e := &s.recs[i]
	h := e.h[:0]
	*e = acEntry{key: key, h: h}
	return e
}

func (s *shard) lookup(key uint32) *acEntry {
	if i, ok := s.idx[key]; ok {
		return &s.recs[i]
	}
	return nil
}

func (t *table) ds(nowNs int64) uint32 {
	d := (nowNs - t.baseNs) / 1e8
	if d < 0 {
		return 0
	}
	return uint32(min(d, 1<<32-1))
}

// stSig is a station and its signal (dBFS, rounded).
type stSig struct {
	st  model.StationIdx
	sig int8
}

func sigByte(sig float32) int8 {
	if sig != sig { // NaN
		return -127
	}
	return int8(max(min(sig, 0), -127))
}

// noteHearers merges stations (sorted by station, distinct) into the
// aircraft's sorted hearer list. Caller holds the lock.
func (e *acEntry) noteHearers(in []stSig, ds uint32) {
	h := e.h
	i := 0
	for _, x := range in {
		for i < len(h) && h[i].st < x.st {
			i++
		}
		if i < len(h) && h[i].st == x.st {
			h[i].lastDs, h[i].sig = ds, x.sig
			if h[i].n < 0xFFFF {
				h[i].n++
			}
			continue
		}
		if len(h) >= MaxHearers {
			// Full: drop the stalest (it is before or after i).
			w := 0
			for k := range h {
				if h[k].lastDs < h[w].lastDs {
					w = k
				}
			}
			if h[w].lastDs >= ds {
				continue // all current: keep them
			}
			h = append(h[:w], h[w+1:]...)
			if w < i {
				i--
			}
		}
		h = append(h, hearer{})
		copy(h[i+1:], h[i:])
		h[i] = hearer{st: x.st, lastDs: ds, n: 1, sig: x.sig}
		i++
	}
	e.h = h
}

// ObserveGroup records which stations hear the group's aircraft and, for a
// DF17/18 frame that produced a tracker position, that the aircraft has a
// current ADS-B position. Call it for every group handed to the MLAT engine
// (before the assembler, from the same goroutine or any other).
func (p *Planner) ObserveGroup(g *ingest.Group) {
	if g.Key == 0 {
		return
	}
	now := g.FirstNs
	s := &p.t.sh[shardOf(g.Key)]
	s.mu.Lock()
	e := s.get(g.Key)
	if e.lastSeen > 0 {
		dt := float32(max(now-e.lastSeen, 0)) / rateTauNs
		e.rate = e.rate*max(0, 1-dt) + 1e9/rateTauNs
	}
	e.lastSeen = max(e.lastSeen, now)
	if g.HasResult && g.Result.Pos && (g.DF == 17 || (g.DF == 18 && len(g.Data) > 0 && g.Data[0]&7 < 2)) {
		e.lastADSB = max(e.lastADSB, now)
	}
	if now-e.lastNote >= noteEveryNs {
		e.lastNote = now
		var buf [64]stSig
		n := 0
		for i := range g.Copies {
			c := &g.Copies[i]
			if !c.Timed || c.SameStation || c.MapOnly || n == len(buf) {
				continue
			}
			x := stSig{st: c.Station, sig: sigByte(c.Signal)}
			j := n
			for j > 0 && buf[j-1].st > x.st {
				buf[j] = buf[j-1]
				j--
			}
			if j > 0 && buf[j-1].st == x.st {
				copy(buf[j:n], buf[j+1:n+1])
				continue
			}
			buf[j] = x
			n++
		}
		e.noteHearers(buf[:n], p.t.ds(now))
	}
	s.mu.Unlock()
}

// NoteHearer records that station st heard aircraft key at nowNs (for links
// that report visibility without timed groups, e.g. mlat-client seen/lost).
//
// A visibility report alone does not create an aircraft: an unknown key is
// held as pending until a second station reports it within HeardTTL (or a
// frame of it arrives through ObserveGroup), so one client's address list
// cannot fill the table (finding ROB-3).
func (p *Planner) NoteHearer(key uint32, st model.StationIdx, nowNs int64) {
	s := &p.t.sh[shardOf(key)]
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.lookup(key)
	if e == nil {
		pd, ok := s.pend[key]
		if !ok || pd.st == st || nowNs-pd.ns > int64(p.cfg.HeardTTL) {
			if ok || len(s.pend) < s.max {
				s.pend[key] = pendHear{st: st, ns: nowNs}
			}
			return
		}
		delete(s.pend, key)
		e = s.get(key)
		e.lastSeen = max(e.lastSeen, pd.ns)
		e.noteHearers([]stSig{{st: pd.st, sig: -127}}, p.t.ds(pd.ns))
	}
	e.lastSeen = max(e.lastSeen, nowNs)
	e.noteHearers([]stSig{{st: st, sig: -127}}, p.t.ds(nowNs))
}

// NoteADSB records an ADS-B position of aircraft key at nowNs and whether
// it is good enough to make MLAT unnecessary (the reference track's NACp ≥ 8,
// NIC ≥ 7, DESIGN §9.2.2). A poor one keeps the aircraft an MLAT candidate.
func (p *Planner) NoteADSB(key uint32, nowNs int64, good bool) {
	s := &p.t.sh[shardOf(key)]
	s.mu.Lock()
	e := s.get(key)
	e.lastSeen = max(e.lastSeen, nowNs)
	e.adsbPoor = !good
	if good {
		e.lastADSB = max(e.lastADSB, nowNs)
	}
	s.mu.Unlock()
}

// MarkSuspect makes an ADS-B aircraft an MLAT candidate until untilNs
// (positions contradicted by TDOA, DESIGN §9.6 "ADS-B verification").
func (p *Planner) MarkSuspect(key uint32, untilNs int64) {
	s := &p.t.sh[shardOf(key)]
	s.mu.Lock()
	e := s.get(key)
	e.suspectUntil = max(e.suspectUntil, untilNs)
	s.mu.Unlock()
}

// prio returns the aircraft's class. Caller holds the lock (or owns e).
func (p *Planner) prio(e *acEntry, nowNs int64) Prio {
	adsb := !e.adsbPoor && e.lastADSB > 0 && nowNs-e.lastADSB <= int64(p.cfg.ADSBGoodAge)
	if !adsb {
		if p.tracked(e.key) {
			return PrioTracked
		}
		return PrioAcquire
	}
	if p.holdOut != nil && p.holdOut(e.key, nowNs) {
		return PrioHoldOut
	}
	if e.suspectUntil > nowNs {
		return PrioSuspect
	}
	return PrioNone
}

func (p *Planner) tracked(key uint32) bool {
	if p.pos == nil {
		return false
	}
	_, sig, ok := p.pos.Approx(key)
	return ok && sig <= p.cfg.TrackedSigmaM
}

// Prio returns the current priority class of an aircraft (PrioAcquire for
// one never seen: it has no ADS-B position).
func (p *Planner) Prio(key uint32, nowNs int64) Prio {
	s := &p.t.sh[shardOf(key)]
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.lookup(key)
	if e == nil {
		if p.tracked(key) {
			return PrioTracked
		}
		return PrioAcquire
	}
	return p.prio(e, nowNs)
}

// forget removes aircraft not seen since cutoffNs. Caller holds no lock.
func (t *table) forget(cutoffNs int64) int {
	n := 0
	for i := range t.sh {
		s := &t.sh[i]
		s.mu.Lock()
		for k, j := range s.idx {
			if s.recs[j].lastSeen < cutoffNs {
				delete(s.idx, k)
				s.free = append(s.free, j)
				n++
			}
		}
		for k, pd := range s.pend {
			if pd.ns < cutoffNs {
				delete(s.pend, k)
			}
		}
		s.mu.Unlock()
	}
	return n
}
