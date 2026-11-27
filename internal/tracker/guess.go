package tracker

import (
	"airview/internal/model"
)

// GuessSample is one reception sample for Locate Me v2
// (docs/aggregator/DESIGN.md §10.4, table guess_samples): an ADS-B position
// the station received off-air, with the signal it was received at. The
// tracker emits at most one per aircraft every 10 s of data time.
type GuessSample struct {
	T        int64            // Unix ms (data time)
	Station  model.StationIdx // receiving station (0 = standalone)
	ICAO     uint32           // address key
	Lat, Lon float64          // aircraft position
	AltFt    float64          // best altitude, ft (NaN = unknown)
	RSSI     float64          // dBFS (NaN = unknown)
	Channel  int              // RF channel of the frame (-1 = unknown)
	Antenna  string           // configured antenna ID of the feed / channel ("" = none)
	OnGround bool
}

const (
	guessEveryMs = 10_000 // per aircraft
	guessQMax    = 8192   // queued samples between deliveries
)

// SetGuessSampleHook installs fn, which receives the queued guess samples
// about once per second of data time. It is called without tracker locks,
// never concurrently with itself, and the slice is reused after it returns
// (copy what you keep). Samples are taken only from ADS-B positions received
// off-air (not MLAT, TIS-B / ADS-R or decoded feeds). nil removes it.
func (t *Tracker) SetGuessSampleHook(fn func([]GuessSample)) {
	t.mu.Lock()
	t.guessFn = fn
	t.guessQ = t.guessQ[:0]
	t.guessDue = false
	t.mu.Unlock()
	if s := t.sh.Load(); s != nil {
		s.setHooks(t)
	}
}

// guessSample queues a sample for ac's accepted position. Caller holds mu
// and has checked t.guessFn != nil.
func (t *Tracker) guessSample(ac *aircraft, f *model.RawFrame, slot int, rssi float64, tMs int64) {
	if ac.guessMs != 0 && tMs-ac.guessMs < guessEveryMs && tMs >= ac.guessMs {
		return
	}
	if len(t.guessQ) >= guessQMax {
		return
	}
	ac.guessMs = tMs
	s := GuessSample{
		T: tMs, Station: f.Station, ICAO: ac.key, Lat: ac.lat, Lon: ac.lon,
		AltFt: ac.bestAlt(tMs), RSSI: rssi, Channel: f.Channel,
		OnGround: ac.onGround && fresh(ac.groundMs, tMs, fieldTTLMs),
	}
	if slot > 0 && slot < len(t.slots) {
		s.Antenna = t.slots[slot].id
	}
	t.guessQ = append(t.guessQ, s)
}

// deliverGuess hands a batch of samples to the hook and returns the buffer.
// Called without mu.
func (t *Tracker) deliverGuess(fn func([]GuessSample), q []GuessSample) {
	t.guessMu.Lock()
	fn(q)
	t.guessMu.Unlock()
	t.mu.Lock()
	if t.guessSpare == nil {
		t.guessSpare = q[:0]
	}
	t.mu.Unlock()
}
