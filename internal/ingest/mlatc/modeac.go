package mlatc

import (
	"slices"

	"airview/internal/model"
)

// Mode A/C requests (MLATC.md §10; DESIGN §9.9, phase 5).
//
// Mode A/C replies carry no address, so the server cannot ask for them by
// aircraft. AirView asks by reply code, or for every reply:
//
//	{"start_sending_modeac":["0a12","1c40"]}   // these 16-bit reply values
//	{"start_sending_modeac":["*"]}             // every Mode A/C reply
//	{"stop_sending_modeac":[...]}              // the reverse
//
// A code is the 2-byte Mode A/C payload as the client puts it into the "m"
// field of an mlat message (the same two bytes as a Beast type '1' frame),
// written as 4 hex digits. The replies come back as ordinary mlat messages
// with a 4-digit "m" (TCP) or as a zero-padded long frame (UDP) and reach
// the ingest as model.FrameModeAC frames with the station's ticks.
//
// The reference client keeps an internal set of requested Mode A/C codes,
// but its JSON protocol has no message that fills it (a reading of the
// protocol, to confirm against a capture). These two messages are
// therefore an AirView extension: they are sent only to a client that
// announced it with "modeac": true in its handshake (or to every client
// with Options.ModeACAllClients). The request state is kept for every
// client either way, so Links and the planner see what was asked.

// maxModeACCodes bounds the requested code set of one client (the 13 code
// bits give 8,192 distinct replies).
const maxModeACCodes = 8192

// acSelection is a client's Mode A/C request state. Guarded by the
// selection's mutex.
type acSelection struct {
	all, sentAll bool
	want, sent   map[uint16]struct{}
}

func (a *acSelection) init() {
	if a.want == nil {
		a.want, a.sent = map[uint16]struct{}{}, map[uint16]struct{}{}
	}
}

// take returns the pending start / stop code lists and the change of the
// "every reply" flag (+1 start, -1 stop, 0 none), and marks them sent.
func (a *acSelection) take(start, stop []uint16) ([]uint16, []uint16, int) {
	a.init()
	allChange := 0
	if a.all != a.sentAll {
		allChange = 1
		if !a.all {
			allChange = -1
		}
		a.sentAll = a.all
	}
	for code := range a.want {
		if _, ok := a.sent[code]; !ok {
			start = append(start, code)
			a.sent[code] = struct{}{}
		}
	}
	for code := range a.sent {
		if _, ok := a.want[code]; !ok {
			stop = append(stop, code)
			delete(a.sent, code)
		}
	}
	slices.Sort(start)
	slices.Sort(stop)
	return start, stop, allChange
}

// appendModeAC appends the pending Mode A/C request lines of c (writer or
// Offline.Poll). Nothing is written for a client that cannot take them;
// their state is then left pending.
func (c *conn) appendModeAC(dst []byte, start, stop []uint16) ([]byte, []uint16, []uint16) {
	if c.sel == nil || !c.modeACWire() {
		return dst, start, stop
	}
	c.sel.mu.Lock()
	start, stop, all := c.sel.ac.take(start[:0], stop[:0])
	c.sel.mu.Unlock()
	if all < 0 {
		dst = append(dst, `{"stop_sending_modeac":["*"]}`+"\n"...)
	}
	for len(stop) > 0 {
		n := min(len(stop), 1000)
		dst = appendCodeList(dst, "stop_sending_modeac", stop[:n])
		stop = stop[n:]
	}
	if all > 0 {
		dst = append(dst, `{"start_sending_modeac":["*"]}`+"\n"...)
	}
	for len(start) > 0 {
		n := min(len(start), 1000)
		dst = appendCodeList(dst, "start_sending_modeac", start[:n])
		start = start[n:]
	}
	return dst, start, stop
}

// modeACWire reports whether Mode A/C requests are sent to c.
func (c *conn) modeACWire() bool { return c.hello.modeAC || c.s.o.ModeACAllClients }

func appendCodeList(dst []byte, key string, codes []uint16) []byte {
	const hexd = "0123456789abcdef"
	dst = append(dst, `{"`...)
	dst = append(dst, key...)
	dst = append(dst, `":[`...)
	for i, v := range codes {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '"', hexd[v>>12&15], hexd[v>>8&15], hexd[v>>4&15], hexd[v&15], '"')
	}
	return append(dst, "]}\n"...)
}

// StartSendingModeAC asks the client of station st for the Mode A/C
// replies with the given codes (2-byte payload values). It returns false
// when the station has no client or the client cannot receive Mode A/C
// requests (the request is still remembered for it).
func (s *Server) StartSendingModeAC(st model.StationIdx, codes []uint16) bool {
	return s.modeACCodes(st, codes, true)
}

// StopSendingModeAC withdraws Mode A/C code requests of station st.
func (s *Server) StopSendingModeAC(st model.StationIdx, codes []uint16) bool {
	return s.modeACCodes(st, codes, false)
}

func (s *Server) modeACCodes(st model.StationIdx, codes []uint16, on bool) bool {
	c := s.connOf(st)
	if c == nil || c.sel == nil {
		return false
	}
	c.sel.mu.Lock()
	a := &c.sel.ac
	a.init()
	for _, code := range codes {
		if on {
			if len(a.want) < maxModeACCodes {
				a.want[code] = struct{}{}
			}
		} else {
			delete(a.want, code)
		}
	}
	c.sel.mu.Unlock()
	return c.modeACWire()
}

// SetModeACAll asks the client of station st for every Mode A/C reply (on)
// or withdraws that request: the request for a station taking part in
// Mode A/C MLAT (plan.Planner.ModeACEligible). Same result as
// StartSendingModeAC.
func (s *Server) SetModeACAll(st model.StationIdx, on bool) bool {
	c := s.connOf(st)
	if c == nil || c.sel == nil {
		return false
	}
	c.sel.mu.Lock()
	c.sel.ac.all = on
	c.sel.mu.Unlock()
	return c.modeACWire()
}

// ModeACCapable reports whether station st has a client that receives Mode
// A/C requests.
func (s *Server) ModeACCapable(st model.StationIdx) bool {
	c := s.connOf(st)
	return c != nil && c.modeACWire()
}
