package feederui

import (
	"math"
	"sync"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
	"airview/internal/modes"
)

// Stats is a cheap local view of what each receiver hears, for the status
// page only (DESIGN §12.1: "local decode ... only for MINUTE_STATS, the
// selective filter and the status page"). It never stores anything on disk
// and never leaves the host. It implements model.FrameSink so the feeder can
// put it into its feed tee next to the uplink manager.
//
// Per frame it costs one map lookup per feed and, for frames that carry a
// verifiable address (DF11/17/18), one CRC residual and one map write.
type Stats struct {
	now func() time.Time

	mu    sync.Mutex
	feeds map[string]*rxAcc
}

// AircraftWindow is how long an address stays in the "aircraft heard" count.
const AircraftWindow = 60 * time.Second

// rateSlots is the length of the per-second frame-count ring (FPS is
// averaged over the last rateSlots-1 complete seconds).
const rateSlots = 11

type rxAcc struct {
	frames, modeAC, short, long, status uint64
	adsb, positions, badCRC, mlat       uint64
	decoded                             uint64
	df                                  [32]uint64
	lastFrame                           time.Time

	// per-second ring for the frame rate
	slotSec [rateSlots]int64
	slotCnt [rateSlots]uint32

	// address -> last heard (Unix seconds); verified addresses only
	seen      map[uint32]int64
	adsbSeen  map[uint32]int64
	pruneTick int64

	// signal: exponentially weighted mean of dBFS and of the strong share
	sigMean   float64
	sigN      uint64
	strongEW  float64
	hasSignal bool
}

// NewStats returns an empty collector. now defaults to time.Now.
func NewStats(now func() time.Time) *Stats {
	if now == nil {
		now = time.Now
	}
	return &Stats{now: now, feeds: map[string]*rxAcc{}}
}

func (s *Stats) acc(id string) *rxAcc {
	a := s.feeds[id]
	if a == nil {
		a = &rxAcc{seen: map[uint32]int64{}, adsbSeen: map[uint32]int64{}}
		s.feeds[id] = a
	}
	return a
}

// HandleFrame implements model.FrameSink.
func (s *Stats) HandleFrame(f model.RawFrame) {
	now := s.now()
	sec := now.Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acc(f.FeedID)
	if f.Kind == model.FrameStatus {
		a.status++
		return
	}
	// MLAT results coming back on the input (magic timestamp) are not
	// something this receiver heard.
	if f.MLAT || feed.IsMLATTimestamp(f.Clock12MHz) {
		a.mlat++
		return
	}
	a.frames++
	a.lastFrame = now
	i := sec % rateSlots
	if a.slotSec[i] != sec {
		a.slotSec[i], a.slotCnt[i] = sec, 0
	}
	a.slotCnt[i]++

	if !math.IsNaN(f.Signal) && f.Signal <= 0 && f.Signal > -100 {
		const alpha = 0.02
		strong := 0.0
		if f.Signal > -3 {
			strong = 1
		}
		if !a.hasSignal {
			a.sigMean, a.strongEW, a.hasSignal = f.Signal, strong, true
		} else {
			a.sigMean += alpha * (f.Signal - a.sigMean)
			a.strongEW += alpha * (strong - a.strongEW)
		}
		a.sigN++
	}

	switch f.Kind {
	case model.FrameModeAC:
		a.modeAC++
		return
	case model.FrameModeSShort:
		a.short++
	case model.FrameModeSLong:
		a.long++
	}
	if len(f.Data) != 7 && len(f.Data) != 14 {
		return
	}
	df := f.Data[0] >> 3
	if df > 24 {
		df = 24 // DF24..31 are all Comm-D
	}
	a.df[df]++
	icao, ok, adsb := verifiedAddress(f.Data, df)
	if !ok {
		if df == 17 || df == 18 {
			a.badCRC++
		}
		return
	}
	a.seen[icao] = sec
	if adsb {
		a.adsb++
		a.adsbSeen[icao] = sec
		if tc := f.Data[4] >> 3; (tc >= 9 && tc <= 18) || (tc >= 20 && tc <= 22) || (tc >= 5 && tc <= 8) {
			a.positions++
		}
	}
	if sec-a.pruneTick >= 10 || len(a.seen) > 50000 {
		a.prune(sec)
	}
}

// verifiedAddress returns the address of frames whose parity proves it:
// DF17/18 with a zero residual and DF11 with a residual below 80 (an
// interrogator code overlay).
func verifiedAddress(b []byte, df uint8) (icao uint32, ok, adsb bool) {
	switch df {
	case 11:
		if len(b) != 7 || modes.Residual(b) >= 80 {
			return 0, false, false
		}
	case 17, 18:
		if len(b) != 14 || modes.Residual(b) != 0 {
			return 0, false, false
		}
		if df == 18 && b[0]&7 > 1 { // CF 0/1 only: ADS-B, not TIS-B/ADS-R
			return uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), true, false
		}
		adsb = true
	default:
		return 0, false, false
	}
	return uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), true, adsb
}

func (a *rxAcc) prune(sec int64) {
	a.pruneTick = sec
	lim := sec - int64(AircraftWindow/time.Second)
	for k, t := range a.seen {
		if t < lim {
			delete(a.seen, k)
		}
	}
	for k, t := range a.adsbSeen {
		if t < lim {
			delete(a.adsbSeen, k)
		}
	}
}

// HandleUpdate implements model.FrameSink (untimed SBS / JSON inputs).
func (s *Stats) HandleUpdate(u model.DecodedUpdate) {
	if u.MLAT {
		return
	}
	now := s.now()
	sec := now.Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acc(u.FeedID)
	a.decoded++
	a.lastFrame = now
	i := sec % rateSlots
	if a.slotSec[i] != sec {
		a.slotSec[i], a.slotCnt[i] = sec, 0
	}
	a.slotCnt[i]++
	if u.ICAO != 0 && u.ICAO < 1<<24 {
		a.seen[u.ICAO] = sec
		if u.Lat != nil {
			a.positions++
		}
	}
	if sec-a.pruneTick >= 10 {
		a.prune(sec)
	}
}

// RxStats is the snapshot of one receiver's local statistics.
type RxStats struct {
	FeedID string `json:"feedId"`
	// Frames counts received frames (Mode S and Mode A/C, not status or
	// returned MLAT results) since the start; Decoded counts untimed
	// updates from SBS / JSON inputs.
	Frames  uint64 `json:"frames"`
	Decoded uint64 `json:"decoded,omitempty"`
	// FPS is the mean message rate of the last ten seconds.
	FPS          float64        `json:"fps"`
	ModeAC       uint64         `json:"modeAc"`
	ModeSShort   uint64         `json:"modeSShort"`
	ModeSLong    uint64         `json:"modeSLong"`
	ADSB         uint64         `json:"adsb"`      // DF17 / DF18 ADS-B messages with good parity
	Positions    uint64         `json:"positions"` // of which position messages
	BadCRC       uint64         `json:"badCrc"`    // DF17/18 with bad parity
	StatusFrames uint64         `json:"statusFrames,omitempty"`
	MLATIgnored  uint64         `json:"mlatIgnored,omitempty"` // MLAT results seen on the input (ignored)
	Aircraft     int            `json:"aircraft"`              // addresses heard in the last minute
	AircraftADSB int            `json:"aircraftAdsb"`          // of which with ADS-B
	SignalDBFS   float64        `json:"signalDbfs"`            // mean RSSI (EWMA), NaN-free: 0 when unknown
	HasSignal    bool           `json:"hasSignal"`
	StrongPct    float64        `json:"strongPct"` // share of messages above -3 dBFS, %
	LastFrameMs  int64          `json:"lastFrameMs,omitempty"`
	DF           map[int]uint64 `json:"df,omitempty"`
}

// Snapshot returns the statistics of every feed seen so far.
func (s *Stats) Snapshot() map[string]RxStats {
	now := s.now()
	sec := now.Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]RxStats, len(s.feeds))
	for id, a := range s.feeds {
		a.prune(sec)
		var n uint32
		for i := 0; i < rateSlots; i++ {
			if d := sec - a.slotSec[i]; d >= 1 && d < rateSlots {
				n += a.slotCnt[i]
			}
		}
		r := RxStats{
			FeedID: id, Frames: a.frames, Decoded: a.decoded, FPS: float64(n) / (rateSlots - 1),
			ModeAC: a.modeAC, ModeSShort: a.short, ModeSLong: a.long, ADSB: a.adsb, Positions: a.positions,
			BadCRC: a.badCRC, StatusFrames: a.status, MLATIgnored: a.mlat,
			Aircraft: len(a.seen), AircraftADSB: len(a.adsbSeen),
		}
		if a.hasSignal {
			r.HasSignal = true
			r.SignalDBFS = math.Round(a.sigMean*10) / 10
			r.StrongPct = math.Round(a.strongEW*1000) / 10
		}
		if !a.lastFrame.IsZero() {
			r.LastFrameMs = a.lastFrame.UnixMilli()
		}
		for df, c := range a.df {
			if c > 0 {
				if r.DF == nil {
					r.DF = map[int]uint64{}
				}
				r.DF[df] = c
			}
		}
		out[id] = r
	}
	return out
}

// Forget drops the statistics of a feed (for example after it was removed
// from the configuration).
func (s *Stats) Forget(feedID string) {
	s.mu.Lock()
	delete(s.feeds, feedID)
	s.mu.Unlock()
}

var _ model.FrameSink = (*Stats)(nil)
