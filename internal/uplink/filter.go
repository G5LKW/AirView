package uplink

import (
	"airview/internal/avf"
	"airview/internal/modes"
)

// frameAddr returns the aircraft address and downlink format of a frame.
// Mode A/C frames return the code with bit 32 set and df 255. For
// address/parity formats the address is recovered from the parity.
func frameAddr(data []byte) (addr uint64, df uint8) {
	switch len(data) {
	case 2:
		return 1<<32 | uint64(data[0])<<8 | uint64(data[1]), 255
	case 7, 14:
	default:
		return 0, 254
	}
	df = data[0] >> 3
	if df > 24 {
		df = 24
	}
	switch df {
	case 11, 17, 18:
		return uint64(data[1])<<16 | uint64(data[2])<<8 | uint64(data[3]), df
	}
	return uint64(modes.Residual(data)), df
}

// selFilter implements selective traffic (AVF §7) and the reduce-style part
// of FLOW slowFactor for one receiver. Frames of aircraft in the sync or
// mlat set pass; every other aircraft passes at most once per reduce
// interval per DF.
type selFilter struct {
	selective bool
	reduceNs  int64
	slow      float64 // FLOW slowFactor (1 = none)
	sync      map[uint32]struct{}
	mlat      map[uint32]struct{}
	last      map[uint64]int64 // addr<<8|df -> host ns last passed
	pruneAt   int64

	// token bucket of the slow factor
	tokens   float64
	tokTime  int64
	fpsEWMA  float64
	fpsCount int
	fpsStart int64
}

func newSelFilter() *selFilter {
	return &selFilter{reduceNs: 500e6, slow: 1, sync: map[uint32]struct{}{}, mlat: map[uint32]struct{}{}, last: map[uint64]int64{}}
}

// clone copies the settings (mode, interval, slow factor, sets) of s.
func (s *selFilter) clone() *selFilter {
	n := newSelFilter()
	n.selective, n.reduceNs, n.slow = s.selective, s.reduceNs, s.slow
	for k := range s.sync {
		n.sync[k] = struct{}{}
	}
	for k := range s.mlat {
		n.mlat[k] = struct{}{}
	}
	return n
}

// active reports whether the filter drops anything at all.
func (s *selFilter) active() bool { return s.selective || s.slow < 1 }

// apply applies an INTEREST record.
func (s *selFilter) apply(in *avf.Interest) {
	switch in.Mode {
	case avf.ModeSelective:
		s.selective = true
	case avf.ModeFull:
		s.selective = false
	}
	if in.ReduceMs > 0 {
		s.reduceNs = int64(in.ReduceMs) * 1e6
	}
	if in.Reset {
		clear(s.sync)
		clear(s.mlat)
	}
	edit := func(m map[uint32]struct{}, add, del []string) {
		for _, a := range del {
			if v, err := modes.ParseICAO(a); err == nil {
				delete(m, v)
			}
		}
		for _, a := range add {
			if v, err := modes.ParseICAO(a); err == nil {
				m[v] = struct{}{}
			}
		}
	}
	edit(s.sync, in.SyncAdd, in.SyncDel)
	edit(s.mlat, in.MLATAdd, in.MLATDel)
}

// observe measures the input frame rate (for the slow factor's budget).
func (s *selFilter) observe(hostNs int64) {
	if s.fpsStart == 0 {
		s.fpsStart = hostNs
	}
	s.fpsCount++
	if d := hostNs - s.fpsStart; d >= 1e9 {
		r := float64(s.fpsCount) / (float64(d) / 1e9)
		if s.fpsEWMA == 0 {
			s.fpsEWMA = r
		} else {
			s.fpsEWMA += 0.2 * (r - s.fpsEWMA)
		}
		s.fpsCount, s.fpsStart = 0, hostNs
	}
}

// pass decides whether a frame is sent.
func (s *selFilter) pass(data []byte, hostNs int64) bool {
	addr, df := frameAddr(data)
	if addr < 1<<24 {
		a := uint32(addr)
		if _, ok := s.sync[a]; ok {
			return s.budget(hostNs)
		}
		if _, ok := s.mlat[a]; ok {
			return s.budget(hostNs)
		}
	}
	red := s.reduceNs
	if s.slow < 1 && s.slow > 0 {
		red = int64(float64(red) / s.slow)
	}
	k := addr<<8 | uint64(df)
	if t, ok := s.last[k]; ok && hostNs-t < red && hostNs >= t {
		return false
	}
	if !s.budget(hostNs) {
		return false
	}
	s.last[k] = hostNs
	if hostNs > s.pruneAt {
		s.pruneAt = hostNs + 60e9
		for key, t := range s.last {
			if hostNs-t > 120e9 {
				delete(s.last, key)
			}
		}
	}
	return true
}

// budget applies the slow factor's frame budget (slowFactor x the input
// rate measured before the FLOW).
func (s *selFilter) budget(hostNs int64) bool {
	if s.slow >= 1 || s.fpsEWMA <= 0 {
		return true
	}
	rate := s.slow * s.fpsEWMA
	if s.tokTime != 0 {
		s.tokens = min(rate, s.tokens+float64(hostNs-s.tokTime)/1e9*rate)
	} else {
		s.tokens = rate
	}
	s.tokTime = hostNs
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

// Filter is the feeder-side selective-traffic and FLOW filter of one
// receiver (AVF §7), exported for load generators that must behave like a
// real feeder (internal/ingest/avfsrv/cmd/avfsrv-load, SO-12). It is not
// safe for concurrent use.
type Filter struct{ f *selFilter }

// NewFilter returns a filter in full mode.
func NewFilter() *Filter { return &Filter{f: newSelFilter()} }

// Apply applies an INTEREST record.
func (x *Filter) Apply(in *avf.Interest) { x.f.apply(in) }

// SetSlow applies a FLOW slowFactor (values outside (0, 1] mean none).
func (x *Filter) SetSlow(s float64) {
	if !(s > 0 && s <= 1) {
		s = 1
	}
	x.f.slow = s
}

// Selective reports whether the filter is in selective mode.
func (x *Filter) Selective() bool { return x.f.selective }

// Pass reports whether a frame received at hostNs is sent.
func (x *Filter) Pass(data []byte, hostNs int64) bool {
	x.f.observe(hostNs)
	return !x.f.active() || x.f.pass(data, hostNs)
}
