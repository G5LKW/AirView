package avfsrv

import (
	"strconv"
	"sync"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
)

// Selective-mode MINUTE_STATS merge (DESIGN §4.4, §14 phase 4 WP-I2).
//
// In selective mode (and under FLOW slowFactor) a feeder sends only part of
// its frames, so counters derived from the frames that reach the server
// undercount the station. The feeder therefore sends exact per-minute
// counters (MINUTE_STATS, counted before any filtering). The server counts,
// per station and minute of feeder host time, what actually reached the
// ingest, and when a MINUTE_STATS record arrives it hands the engine one
// StationMinute holding both, so statistics can add exactly what the server
// did not see (StationMinute.Shed) or use the merged maximum
// (StationMinute.Merged).
//
// Rules:
//   - The feeder sends a minute's frames before its MINUTE_STATS (internal/
//     uplink closes the open batch first) and the worker handles records in
//     order, so the received counts are complete when the record arrives.
//   - Several records for one (station, minute) are added up: a feeder that
//     restarts or reconnects mid-minute sends the part before and the part
//     after. Each new record emits the cumulative StationMinute again with
//     Records incremented: consumers upsert by (Station, Minute). ICAOs is
//     then a lower bound (the larger of the parts).
//   - Received counts are kept for the last minuteSlots minutes per station.
//     Records for older minutes (backfill after a long outage) come with
//     ReceivedKnown false and are not merged with each other.

// minuteSlots is the per-station window of received counts (minutes).
const minuteSlots = 16

// MinuteCounts are per-minute frame counters.
type MinuteCounts struct {
	Frames    uint64     `json:"frames"`    // all frames (Mode S + Mode A/C)
	ModeAC    uint64     `json:"modeAC"`    // Mode A/C replies
	Positions uint64     `json:"positions"` // DF17/18 airborne/surface position messages
	DF        [25]uint64 `json:"df"`        // Mode S frames by downlink format (24 = 24 and above)
}

// add adds o to c.
func (c *MinuteCounts) add(o *MinuteCounts) {
	c.Frames += o.Frames
	c.ModeAC += o.ModeAC
	c.Positions += o.Positions
	for i := range c.DF {
		c.DF[i] += o.DF[i]
	}
}

// countFrame counts one frame payload.
func (c *MinuteCounts) countFrame(d []byte) {
	c.Frames++
	switch len(d) {
	case 2:
		c.ModeAC++
		return
	case 7, 14:
	default:
		return
	}
	df := d[0] >> 3
	if df > 24 {
		df = 24
	}
	c.DF[df]++
	if (df == 17 || df == 18) && len(d) == 14 {
		if tc := d[4] >> 3; (tc >= 5 && tc <= 18) || (tc >= 20 && tc <= 22) {
			c.Positions++
		}
	}
}

// ExactCounts maps a MINUTE_STATS record onto MinuteCounts (Frames is the
// sum of the DF counters and Mode A/C, the feeder's frame total).
func ExactCounts(m *avf.MinuteStats) MinuteCounts {
	c := MinuteCounts{ModeAC: m.ModeAC, Positions: m.Positions}
	for k, n := range m.DF {
		df, err := strconv.Atoi(k)
		if err != nil || df < 0 {
			continue
		}
		c.DF[min(df, 24)] += n
		c.Frames += n
	}
	c.Frames += m.ModeAC
	return c
}

// StationMinute is the merged statistics of one station for one minute
// (Hooks.OnStationMinute).
type StationMinute struct {
	Station model.StationIdx `json:"station"`
	// Minute is the Unix second of the minute start (feeder host time).
	Minute int64 `json:"minute"`
	// Exact is the feeder's MINUTE_STATS (summed over Records records; RxNo
	// of the last one). Authoritative for ICAOs, CRCFail and RSSI, which the
	// server cannot count from a filtered stream.
	Exact avf.MinuteStats `json:"exact"`
	// Received counts the frames of that minute the server handed to the
	// ingest (late ones included). Valid only when ReceivedKnown.
	Received      MinuteCounts `json:"received"`
	ReceivedKnown bool         `json:"receivedKnown"`
	// Selective is set when some of the minute's frames arrived while the
	// station was in selective mode.
	Selective bool `json:"selective"`
	// Records is the number of MINUTE_STATS records merged (> 1 after a
	// restart or reconnect inside the minute; ICAOs is then a lower bound).
	Records int `json:"records"`
	// Late is set when the record arrived inside BACKFILL or more than two
	// minutes after the minute ended.
	Late bool `json:"late"`
}

// Merged returns the best per-minute counts: per field the larger of the
// feeder's exact counters and what the server received (they differ only
// when frames were filtered at the feeder, or for records that cover part
// of a minute).
func (m *StationMinute) Merged() MinuteCounts {
	e := ExactCounts(&m.Exact)
	if !m.ReceivedKnown {
		return e
	}
	r := &m.Received
	out := MinuteCounts{Frames: max(e.Frames, r.Frames), ModeAC: max(e.ModeAC, r.ModeAC), Positions: max(e.Positions, r.Positions)}
	for i := range out.DF {
		out.DF[i] = max(e.DF[i], r.DF[i])
	}
	return out
}

// Shed returns what the feeder counted but the server did not receive
// (selective reduce, FLOW, budget shedding): add it to statistics that
// were built from the received frames. All zero when ReceivedKnown is
// false.
func (m *StationMinute) Shed() MinuteCounts {
	if !m.ReceivedKnown {
		return MinuteCounts{}
	}
	e := ExactCounts(&m.Exact)
	r := &m.Received
	sub := func(a, b uint64) uint64 {
		if a > b {
			return a - b
		}
		return 0
	}
	out := MinuteCounts{ModeAC: sub(e.ModeAC, r.ModeAC), Positions: sub(e.Positions, r.Positions)}
	for i := range out.DF {
		out.DF[i] = sub(e.DF[i], r.DF[i])
		out.Frames += out.DF[i]
	}
	out.Frames += out.ModeAC
	return out
}

// minuteSlot is one minute of one station.
type minuteSlot struct {
	minute    int64 // Unix second of the minute start (0 = empty)
	recv      MinuteCounts
	selective bool
	exact     *avf.MinuteStats // merged MINUTE_STATS so far
	records   int
}

// stationMinutes is the per-station window. It is shared by every
// connection that carries the station (reconnects, a moved receiver).
type stationMinutes struct {
	mu    sync.Mutex
	slots [minuteSlots]minuteSlot
	used  int64 // Unix ns of the last use (janitor)
}

// slot returns the slot of minute, recycling an older one; nil when minute
// is older than the window's content allows.
func (s *stationMinutes) slot(minute int64) *minuteSlot {
	i := (minute / 60) % minuteSlots
	if i < 0 {
		i += minuteSlots
	}
	sl := &s.slots[i]
	switch {
	case sl.minute == minute:
		return sl
	case sl.minute > minute:
		return nil // the slot already holds a newer minute
	}
	*sl = minuteSlot{minute: minute}
	return sl
}

// countFrames counts a decoded FRAMES record (called by the worker with the
// frames handed to the ingest).
func (s *stationMinutes) countFrames(raw []model.RawFrame, selective bool, nowNs int64) {
	if len(raw) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used = nowNs
	var cur *minuteSlot
	var curMin int64 = -1
	for i := range raw {
		h := raw[i].HostNs
		if h <= 0 {
			h = nowNs
		}
		mn := floorMinute(h)
		if mn != curMin {
			curMin, cur = mn, s.slot(mn)
			if cur != nil && selective {
				cur.selective = true
			}
		}
		if cur != nil {
			cur.recv.countFrame(raw[i].Data)
		}
	}
}

// addExact merges a MINUTE_STATS record and returns the station minute to
// publish.
func (s *stationMinutes) addExact(st model.StationIdx, m avf.MinuteStats, late bool, nowNs int64) StationMinute {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used = nowNs
	out := StationMinute{Station: st, Minute: m.Minute, Late: late}
	mn := floorMinute(m.Minute * 1e9)
	// Only minutes inside the window can be merged; a minute too far in
	// the future (bad feeder clock) must not wipe the window either.
	var sl *minuteSlot
	if newest := s.newest(); mn > newest-minuteSlots*60 && mn <= max(newest, floorMinute(nowNs))+120 {
		sl = s.slot(mn)
	}
	if sl == nil {
		out.Exact, out.Records = cloneMinuteStats(&m), 1
		return out
	}
	if sl.exact == nil {
		e := cloneMinuteStats(&m)
		sl.exact = &e
	} else {
		mergeMinuteStats(sl.exact, &m)
	}
	sl.records++
	out.Exact = cloneMinuteStats(sl.exact)
	out.Received, out.ReceivedKnown, out.Selective, out.Records = sl.recv, true, sl.selective, sl.records
	return out
}

// newest returns the newest minute in the window (0 when empty).
func (s *stationMinutes) newest() int64 {
	var n int64
	for i := range s.slots {
		n = max(n, s.slots[i].minute)
	}
	return n
}

func cloneMinuteStats(m *avf.MinuteStats) avf.MinuteStats {
	c := *m
	c.DF = make(map[string]uint64, len(m.DF))
	for k, v := range m.DF {
		c.DF[k] = v
	}
	if m.RSSI != nil {
		r := avf.RSSIHist{Lo: m.RSSI.Lo, Counts: append([]uint64(nil), m.RSSI.Counts...)}
		c.RSSI = &r
	}
	return c
}

// mergeMinuteStats adds b to a (two parts of one minute). ICAOs cannot be
// added (the same aircraft is usually in both parts): the larger count is
// kept as a lower bound.
func mergeMinuteStats(a, b *avf.MinuteStats) {
	a.RxNo = b.RxNo
	if a.DF == nil {
		a.DF = map[string]uint64{}
	}
	for k, v := range b.DF {
		a.DF[k] += v
	}
	a.ModeAC += b.ModeAC
	a.ICAOs = max(a.ICAOs, b.ICAOs)
	a.Positions += b.Positions
	a.CRCFail += b.CRCFail
	switch {
	case b.RSSI == nil:
	case a.RSSI == nil:
		r := avf.RSSIHist{Lo: b.RSSI.Lo, Counts: append([]uint64(nil), b.RSSI.Counts...)}
		a.RSSI = &r
	default:
		// align on a.RSSI.Lo; bins outside a's range go to its end bins
		for i, n := range b.RSSI.Counts {
			if len(a.RSSI.Counts) == 0 {
				break
			}
			j := min(max(b.RSSI.Lo+i-a.RSSI.Lo, 0), len(a.RSSI.Counts)-1)
			a.RSSI.Counts[j] += n
		}
	}
}

func floorMinute(ns int64) int64 {
	s := ns / 1e9
	if ns < 0 && ns%1e9 != 0 {
		s--
	}
	return s - ((s%60)+60)%60
}

// minuteBook holds the windows of every station seen recently.
type minuteBook struct {
	mu sync.Mutex
	st map[model.StationIdx]*stationMinutes
}

func newMinuteBook() *minuteBook {
	return &minuteBook{st: map[model.StationIdx]*stationMinutes{}}
}

// get returns the window of st, creating it.
func (b *minuteBook) get(st model.StationIdx) *stationMinutes {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.st[st]
	if s == nil {
		s = &stationMinutes{}
		b.st[st] = s
	}
	return s
}

// sweep forgets stations unused for longer than the window plus a margin.
func (b *minuteBook) sweep(now time.Time) {
	limit := now.Add(-(minuteSlots + 5) * time.Minute).UnixNano()
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, s := range b.st {
		s.mu.Lock()
		old := s.used < limit
		s.mu.Unlock()
		if old {
			delete(b.st, k)
		}
	}
}

// size returns the number of stations held (tests, stats).
func (b *minuteBook) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.st)
}
