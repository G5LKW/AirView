package engine

import (
	"sort"
	"sync"

	"airview/internal/ingest/avfsrv"
	"airview/internal/model"
)

// Selective-mode MINUTE_STATS merge (WP-I2; internal/ingest/avfsrv
// WIRING.md): the AVF/1 server merges each station's MINUTE_STATS with
// what it received in that minute. The engine keeps the last
// stationMinuteKeep minutes per station in memory, upserted by (station,
// minute) (a second record for one minute carries the cumulative sum), for
// the owner/admin station page: exact counts, what was received, what the
// feeder shed (selective reduce, FLOW, budget) and whether the station was
// in selective mode.
const stationMinuteKeep = 16

type stationMinutes struct {
	mu sync.Mutex
	m  map[model.StationIdx][]avfsrv.StationMinute
}

func (s *stationMinutes) add(sm avfsrv.StationMinute) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[model.StationIdx][]avfsrv.StationMinute{}
	}
	list := s.m[sm.Station]
	for i := range list {
		if list[i].Minute == sm.Minute {
			list[i] = sm
			return
		}
	}
	list = append(list, sm)
	sort.Slice(list, func(i, j int) bool { return list[i].Minute < list[j].Minute })
	if len(list) > stationMinuteKeep {
		list = append(list[:0:0], list[len(list)-stationMinuteKeep:]...)
	}
	s.m[sm.Station] = list
}

func (s *stationMinutes) get(st model.StationIdx) []avfsrv.StationMinute {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]avfsrv.StationMinute(nil), s.m[st]...)
}

func (s *stationMinutes) remove(st model.StationIdx) {
	s.mu.Lock()
	delete(s.m, st)
	s.mu.Unlock()
}

// StationMinute is one minute of a station's MINUTE_STATS merge
// (StationMinutes): the feeder's exact counters, what the server received,
// the merged best counts and what the feeder shed.
type StationMinute struct {
	avfsrv.StationMinute
	Merged avfsrv.MinuteCounts `json:"merged"`
	Shed   avfsrv.MinuteCounts `json:"shed"`
}

// StationMinutes returns the last minutes of st's MINUTE_STATS merge,
// oldest first (aggregator mode, AVF/1 stations only; owner/admin data).
func (e *Engine) StationMinutes(st model.StationIdx) []StationMinute {
	nc := e.net
	if nc == nil {
		return nil
	}
	list := nc.minutes.get(st)
	out := make([]StationMinute, 0, len(list))
	for i := range list {
		m := list[i]
		out = append(out, StationMinute{StationMinute: m, Merged: m.Merged(), Shed: m.Shed()})
	}
	return out
}
