package mlatc

import (
	"math"
	"time"

	"airview/internal/model"
)

// Split-sync fallback (SplitSyncAuto). The reply flag is a reading of the
// reference client's behaviour that no capture has confirmed yet
// (CLEANROOM.md, WP-B3): if a client that was offered split_sync answers
// with single-sync messages the server cannot use, or with no sync data at
// all, the fallback restores the paired protocol for that identity.
const (
	// splitBadMin malformed "ssync" messages without a single valid one
	// trigger the fallback.
	splitBadMin = 16
	// splitSilentAfter: a client that was offered split_sync, has sent
	// timed mlat frames and at least splitUnknownMin messages of unknown
	// kind, but neither a sync nor a valid ssync within this time, falls
	// back too (it may be sending its single syncs under another key).
	splitSilentAfter = 90 * time.Second
	splitUnknownMin  = 32
	// splitFallbackFor is how long an identity keeps the paired protocol.
	splitFallbackFor = 7 * 24 * time.Hour
	// maxNoSplit bounds the fallback table.
	maxNoSplit = 100_000
)

// offerSplit decides the reply's split_sync flag for an identity key.
func (s *Server) offerSplit(key string, now time.Time) bool {
	switch s.o.SplitSync {
	case SplitSyncOn:
		return true
	case SplitSyncOff:
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.noSplit[key]
	if !ok {
		return true
	}
	if now.Before(until) {
		return false
	}
	delete(s.noSplit, key)
	return true
}

// splitFallback remembers that key must not be offered split_sync.
func (s *Server) splitFallback(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.noSplit) >= maxNoSplit {
		for k, t := range s.noSplit {
			if now.After(t) {
				delete(s.noSplit, k)
			}
		}
		if len(s.noSplit) >= maxNoSplit {
			return // full of live entries: the client keeps split sync
		}
	}
	s.noSplit[key] = now.Add(splitFallbackFor)
	s.c.splitFallbacks.Add(1)
}

// SplitSyncFallbacks returns the identities currently answered without
// split_sync (admin diagnostics; keys as Identity.Key).
func (s *Server) SplitSyncFallbacks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.noSplit))
	for k := range s.noSplit {
		out = append(out, k)
	}
	return out
}

// checkSplit runs after every message of a client that was offered split
// sync (reader goroutine); false = the client was disconnected for the
// fallback.
func (c *conn) checkSplit() bool {
	if !c.split || c.s.o.SplitSync != SplitSyncAuto || c.ssyncOK.Load() > 0 || c.syncs.Load() > 0 {
		return true
	}
	bad := c.ssyncBad >= splitBadMin
	silent := c.unknownMsgs >= splitUnknownMin && c.mlats.Load() > 0 &&
		c.nowNs-c.since.UnixNano() > int64(splitSilentAfter)
	if !bad && !silent {
		return true
	}
	now := time.Unix(0, c.nowNs)
	c.s.splitFallback(c.key, now)
	c.s.o.Logger.Info("mlat-client: split_sync not usable, falling back to sync pairs on reconnect",
		"link", c.id, "station", uint32(c.att.Station), "bad_ssync", c.ssyncBad, "unknown", c.unknownMsgs)
	c.shutdown(ReasonProtocol)
	return false
}

// backdateSyncPair sets the host time of the older half of a sync pair
// (the batch entries from i on) to its reception time: the arrival time
// minus the tick difference to the newest half at the class's nominal
// rate. Paired clients hold the first half until the second arrives, so
// its arrival time is not its reception time; the ingest's host-time
// consumers (receive times, arrival envelope) then see the copy as what
// it is: an old copy, not a fresh one with odd ticks.
func (c *conn) backdateSyncPair(i int) {
	if len(c.batch)-i != 2 {
		return
	}
	a, b := &c.batch[i], &c.batch[i+1]
	if a.Epoch != b.Epoch {
		return
	}
	hz := c.clk.cls.NominalHz()
	if hz <= 0 {
		return
	}
	older, newer := a, b
	if b.Ticks < a.Ticks {
		older, newer = b, a
	}
	d := float64(newer.Ticks-older.Ticks) / hz * 1e9
	if d <= 0 || d > float64(maxPairAge) {
		return
	}
	older.HostNs = newer.HostNs - int64(d)
}

// maxPairAge bounds the back-dating (the reference client pairs halves at
// most 5 s apart; a little slack for clock rate errors).
const maxPairAge = 6 * time.Second

// ---------------------------------------------------------------- rates

// maxRates bounds the rate table of one client.
const maxRates = 4096

// noteRates stores a rate_report (reader goroutine): the client's ADS-B
// position rates per aircraft. A report replaces the previous one. For a
// station in timing quarantine the highest-rate aircraft become the
// selection's probe set (Options.Trust).
func (c *conn) noteRates(rates []AircraftRate) {
	sum := 0.0
	c.sel.mu.Lock()
	clear(c.sel.rates)
	for _, r := range rates {
		if len(c.sel.rates) >= maxRates {
			break
		}
		c.sel.rates[r.Addr] = r.Rate
		sum += float64(r.Rate)
	}
	c.adsbN.Store(uint64(len(c.sel.rates)))
	c.adsbRate.Store(math.Float64bits(sum))
	k := 0
	if t := c.s.o.Trust; t != nil && c.s.o.ProbeAircraft > 0 && t.Quarantined(c.att.Station) {
		k = c.s.o.ProbeAircraft
	}
	c.sel.setProbe(k)
	c.sel.mu.Unlock()
}

// ADSBRates appends the station's last rate_report (ADS-B position rates per
// aircraft, messages/s) to dst. ok is false when the station has no client
// or the client sent none.
func (s *Server) ADSBRates(st model.StationIdx, dst []AircraftRate) ([]AircraftRate, bool) {
	s.mu.Lock()
	c := s.byStation[st]
	s.mu.Unlock()
	if c == nil || c.sel == nil {
		return dst, false
	}
	c.sel.mu.Lock()
	defer c.sel.mu.Unlock()
	if len(c.sel.rates) == 0 {
		return dst, false
	}
	for a, r := range c.sel.rates {
		dst = append(dst, AircraftRate{Addr: a, Rate: r})
	}
	return dst, true
}

// ADSBRate returns one aircraft's rate from the station's last rate_report
// (ok false when unknown). Cheap: one map lookup under the client's lock.
func (s *Server) ADSBRate(st model.StationIdx, addr uint32) (float32, bool) {
	s.mu.Lock()
	c := s.byStation[st]
	s.mu.Unlock()
	if c == nil || c.sel == nil {
		return 0, false
	}
	c.sel.mu.Lock()
	r, ok := c.sel.rates[addr]
	c.sel.mu.Unlock()
	return r, ok
}
