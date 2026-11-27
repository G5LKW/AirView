package tracker

import (
	"math"
	"sort"
	"time"

	lchain "airview/internal/latency"
	"airview/internal/model"
	"airview/internal/modes"
)

// counters are the lifetime totals since New / Reset (owned by mu).
type counters struct {
	Frames, Accepted, Duplicates, CRCBad, Fixed, Rejected, Unsupported, BadLength uint64
	ModeAC, Status                                                                uint64
	Updates, UpdateMsgs                                                           uint64
	Positions, MLATPositions, PosRejected, PosPending, PosJumps                   uint64
	DF                                                                            [modes.NumDFIndex]uint64
	BDS40, BDS50, BDS60, BDS44                                                    uint64
	Flights, Emergencies                                                          uint64
}

// AntennaCounters are the totals of one antenna ("" = all combined).
type AntennaCounters struct {
	ID        string  `json:"id"`
	Frames    uint64  `json:"frames"`
	Positions uint64  `json:"positions"`
	MsgRate   float64 `json:"msgRate"`
}

// FeedCounters are the totals of one feed.
type FeedCounters struct {
	ID         string  `json:"id"`
	AntennaID  string  `json:"antennaId,omitempty"`
	Frames     uint64  `json:"frames"`  // raw Mode S frames
	Updates    uint64  `json:"updates"` // pre-decoded updates (SBS / JSON)
	UpdateMsgs uint64  `json:"updateMsgs"`
	MsgRate    float64 `json:"msgRate"`
	LastFrame  int64   `json:"lastFrame"` // Unix ms, 0 = never
}

// Counters is the ingest summary for the statistics page. All counts are
// totals since the tracker was created or reset.
type Counters struct {
	Now     int64   `json:"now"`     // data time, Unix ms (0 before the first frame)
	Started int64   `json:"started"` // wall clock, Unix ms
	UptimeS float64 `json:"uptimeS"`

	Frames      uint64 `json:"frames"`      // Mode S frames received (every copy)
	Accepted    uint64 `json:"accepted"`    // decoded and applied (duplicates excluded)
	Duplicates  uint64 `json:"duplicates"`  // second copies from another antenna / feed
	CRCBad      uint64 `json:"crcBad"`      // parity failures
	Fixed       uint64 `json:"fixed"`       // accepted after a 1-bit repair
	Rejected    uint64 `json:"rejected"`    // parity-address frames of unconfirmed addresses
	Unsupported uint64 `json:"unsupported"` // downlink formats without a decoder
	BadLength   uint64 `json:"badLength"`
	ModeAC      uint64 `json:"modeAC"`
	Status      uint64 `json:"status"` // receiver status frames
	Updates     uint64 `json:"updates"`
	UpdateMsgs  uint64 `json:"updateMsgs"`

	Positions     uint64 `json:"positions"`
	MLATPositions uint64 `json:"mlatPositions"`
	PosRejected   uint64 `json:"posRejected"` // failed the range / speed filters
	PosPending    uint64 `json:"posPending"`  // first fixes that waited for confirmation
	PosJumps      uint64 `json:"posJumps"`    // tracks restarted by two consistent outliers

	DF  map[string]uint64 `json:"df"`  // accepted frames per downlink format ("df17", "df_other" ...)
	BDS map[string]uint64 `json:"bds"` // identified Comm-B registers ("4,0", "5,0", "6,0", "4,4")

	Flights     uint64 `json:"flights"`
	Emergencies uint64 `json:"emergencies"`

	MsgRate float64 `json:"msgRate"` // frames/s, smoothed
	PosRate float64 `json:"posRate"`

	Aircraft int `json:"aircraft"` // shown in the live list
	Tracked  int `json:"tracked"`  // in the table, including dormant flights

	Antennas []AntennaCounters `json:"antennas"`
	Feeds    []FeedCounters    `json:"feeds"`
}

// Counters returns the ingest counters and rates.
func (t *Tracker) Counters() Counters {
	if s := t.sh.Load(); s != nil {
		return s.counters(t)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &t.c
	out := Counters{
		Now: t.nowMs, Started: t.started.UnixMilli(), UptimeS: round1(time.Since(t.started).Seconds()),
		Frames: c.Frames, Accepted: c.Accepted, Duplicates: c.Duplicates, CRCBad: c.CRCBad, Fixed: c.Fixed,
		Rejected: c.Rejected, Unsupported: c.Unsupported, BadLength: c.BadLength, ModeAC: c.ModeAC,
		Status: c.Status, Updates: c.Updates, UpdateMsgs: c.UpdateMsgs,
		Positions: c.Positions, MLATPositions: c.MLATPositions, PosRejected: c.PosRejected,
		PosPending: c.PosPending, PosJumps: c.PosJumps,
		DF:      make(map[string]uint64, modes.NumDFIndex),
		BDS:     map[string]uint64{"4,0": c.BDS40, "5,0": c.BDS50, "6,0": c.BDS60, "4,4": c.BDS44},
		Flights: c.Flights, Emergencies: c.Emergencies,
		MsgRate: round1(t.msgRate), PosRate: round1(t.posRate),
		Aircraft: t.live, Tracked: len(t.ac),
		Antennas: make([]AntennaCounters, 0, len(t.slots)),
		Feeds:    make([]FeedCounters, 0, len(t.feedList)),
	}
	for i, n := range c.DF {
		out.DF[modes.DFIndexLabel(i)] = n
	}
	for _, s := range t.slots {
		out.Antennas = append(out.Antennas, AntennaCounters{
			ID: s.id, Frames: s.totFrames, Positions: s.totPositions, MsgRate: round1(s.rate),
		})
	}
	for _, fs := range t.feedList {
		fc := FeedCounters{
			ID: fs.id, Frames: fs.frames, Updates: fs.updates, UpdateMsgs: fs.updateMsgs,
			MsgRate: round1(fs.rate), LastFrame: fs.lastMs,
		}
		if fs.slot > 0 && fs.slot < len(t.slots) {
			fc.AntennaID = t.slots[fs.slot].id
		}
		out.Feeds = append(out.Feeds, fc)
	}
	return out
}

// ----------------------------------------------------------------- timing

const (
	latRing       = 2048 // samples kept per feed
	latSampleMs   = 40   // at most one sample per feed in this time (25 Hz)
	latHistoryMax = 180  // per-minute history points
	latHistBins   = 40
	dayNs         = 86400 * int64(time.Second)
)

// latency is the rolling GPS-timestamp latency state of one feed.
type latency struct {
	ms      []float32 // ring, allocated on the first GPS-stamped frame
	at      []int64   // data time of each sample
	n, pos  int
	lastAt  int64
	total   uint64
	last    float64
	history []LatencyPoint
}

// LatencyBin is one bar of the latency histogram.
type LatencyBin struct {
	LoMs  float64 `json:"loMs"`
	HiMs  float64 `json:"hiMs"`
	Count int     `json:"count"`
}

// LatencyPoint is the latency summary of one minute.
type LatencyPoint struct {
	T       int64   `json:"t"` // start of the minute, Unix ms
	Samples int     `json:"samples"`
	MinMs   float64 `json:"minMs"`
	P05Ms   float64 `json:"p05Ms"`
	P50Ms   float64 `json:"p50Ms"`
	P95Ms   float64 `json:"p95Ms"`
}

// FeedLatency describes the delay between a feed's GPS frame timestamps and
// the (NTP-corrected) host clock at reception:
//
//	latency = (Recv + clock offset) time of day - GPS time of day
//
// It contains the receiver's processing time, the network transit and any
// error of the host clock. Only feeds that deliver GPS timestamps (Radarcape
// in GPS mode, the simulator) have samples. The statistics cover the rolling
// sample window (about the last 80 s of traffic); History has one point per
// minute.
type FeedLatency struct {
	FeedID  string  `json:"feedId"`
	Total   uint64  `json:"total"`   // GPS-stamped frames seen
	Samples int     `json:"samples"` // samples in the rolling window
	WindowS float64 `json:"windowS"` // time span of the window
	LastMs  float64 `json:"lastMs"`
	MeanMs  float64 `json:"meanMs"`
	StdMs   float64 `json:"stdMs"`
	MinMs   float64 `json:"minMs"`
	MaxMs   float64 `json:"maxMs"`
	P01Ms   float64 `json:"p01Ms"`
	P05Ms   float64 `json:"p05Ms"`
	P25Ms   float64 `json:"p25Ms"`
	P50Ms   float64 `json:"p50Ms"`
	P75Ms   float64 `json:"p75Ms"`
	P95Ms   float64 `json:"p95Ms"`
	P99Ms   float64 `json:"p99Ms"`

	Hist    []LatencyBin   `json:"hist"`
	History []LatencyPoint `json:"history"`

	ClockOffsetMs float64 `json:"clockOffsetMs"` // NTP correction applied
	ClockSynced   bool    `json:"clockSynced"`
	// Ref is the instant the delays are measured from: always
	// "startOfFrame" (latency.RefStartOfFrame) - the GPS stamp (end of the
	// frame) minus the frame duration, the same reference as the receiver
	// stage of the latency analysis.
	Ref string `json:"ref"`
}

// sampleLatency records the latency of a GPS-stamped frame (rate limited).
func (t *Tracker) sampleLatency(fs *feedState, f *model.RawFrame, tMs int64) {
	l := &fs.lat
	l.total++
	if !fs.latOwn {
		return // a shard that does not sample this feed (shard.go)
	}
	if d := tMs - l.lastAt; d >= 0 && d < latSampleMs {
		return
	}
	if f.Recv.IsZero() || f.GPSNs >= dayNs {
		return
	}
	l.lastAt = tMs
	var off time.Duration
	if t.clock != nil {
		off = t.clock.Offset()
	}
	tod := mod(f.Recv.UnixNano()+int64(off), dayNs)
	// from the start of the frame (the GPS stamp marks its end), like the
	// receiver stage of the latency analysis
	var df uint8
	if len(f.Data) > 0 {
		df = f.Data[0] >> 3
	}
	d := tod - (f.GPSNs - lchain.FrameStampOffsetNs(f.Kind, df))
	if d > dayNs/2 { // midnight wrap
		d -= dayNs
	} else if d < -dayNs/2 {
		d += dayNs
	}
	ms := float64(d) / 1e6
	l.last = ms
	if l.ms == nil {
		l.ms = make([]float32, latRing)
		l.at = make([]int64, latRing)
	}
	l.ms[l.pos], l.at[l.pos] = float32(ms), tMs
	l.pos = (l.pos + 1) % latRing
	if l.n < latRing {
		l.n++
	}
}

// samples returns the sorted samples taken at or after fromMs and the time
// span they cover.
func (l *latency) samples(fromMs int64) (v []float64, spanMs int64) {
	if l.n == 0 {
		return nil, 0
	}
	v = make([]float64, 0, l.n)
	lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
	for i := 0; i < l.n; i++ {
		if l.at[i] < fromMs {
			continue
		}
		v = append(v, float64(l.ms[i]))
		lo, hi = min(lo, l.at[i]), max(hi, l.at[i])
	}
	if len(v) == 0 {
		return nil, 0
	}
	sort.Float64s(v)
	return v, hi - lo
}

// quantile of a sorted slice (linear interpolation).
func quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	x := q * float64(len(v)-1)
	i := int(x)
	if i >= len(v)-1 {
		return v[len(v)-1]
	}
	return v[i] + (x-float64(i))*(v[i+1]-v[i])
}

// closeMinute appends the summary of the minute that started at minStart.
func (l *latency) closeMinute(minStart int64) {
	if l.n == 0 {
		return
	}
	v, _ := l.samples(minStart * 1000)
	if len(v) < 5 {
		return
	}
	if len(l.history) >= latHistoryMax {
		copy(l.history, l.history[1:])
		l.history = l.history[:len(l.history)-1]
	}
	l.history = append(l.history, LatencyPoint{
		T: minStart * 1000, Samples: len(v), MinMs: round3(v[0]),
		P05Ms: round3(quantile(v, 0.05)), P50Ms: round3(quantile(v, 0.50)), P95Ms: round3(quantile(v, 0.95)),
	})
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// FeedLatency returns the timing statistics of every feed that delivers GPS
// timestamps.
func (t *Tracker) FeedLatency() []FeedLatency {
	if s := t.sh.Load(); s != nil {
		return s.feedLatency()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var offMs float64
	synced := false
	if t.clock != nil {
		offMs = float64(t.clock.Offset()) / 1e6
		synced = t.clock.Synced()
	}
	out := make([]FeedLatency, 0, len(t.feedList))
	for _, fs := range t.feedList {
		l := &fs.lat
		if l.total == 0 {
			continue
		}
		fl := FeedLatency{
			FeedID: fs.id, Total: l.total, LastMs: round3(l.last),
			Hist:          []LatencyBin{},
			History:       append([]LatencyPoint{}, l.history...),
			ClockOffsetMs: round3(offMs), ClockSynced: synced,
			Ref: lchain.RefStartOfFrame,
		}
		v, span := l.samples(math.MinInt64)
		if n := len(v); n > 0 {
			fl.Samples, fl.WindowS = n, round1(float64(span)/1000)
			var sum, sq float64
			for _, x := range v {
				sum += x
			}
			mean := sum / float64(n)
			for _, x := range v {
				sq += (x - mean) * (x - mean)
			}
			fl.MeanMs, fl.StdMs = round3(mean), round3(math.Sqrt(sq/float64(n)))
			fl.MinMs, fl.MaxMs = round3(v[0]), round3(v[n-1])
			fl.P01Ms, fl.P05Ms, fl.P25Ms = round3(quantile(v, 0.01)), round3(quantile(v, 0.05)), round3(quantile(v, 0.25))
			fl.P50Ms, fl.P75Ms = round3(quantile(v, 0.50)), round3(quantile(v, 0.75))
			fl.P95Ms, fl.P99Ms = round3(quantile(v, 0.95)), round3(quantile(v, 0.99))
			// Histogram between the 0.5 % and 99.5 % points; the tails are
			// folded into the outer bins.
			lo, hi := quantile(v, 0.005), quantile(v, 0.995)
			if hi-lo < 1e-3 {
				lo, hi = lo-0.5, hi+0.5
			}
			w := (hi - lo) / latHistBins
			fl.Hist = make([]LatencyBin, latHistBins)
			for i := range fl.Hist {
				fl.Hist[i].LoMs = round3(lo + float64(i)*w)
				fl.Hist[i].HiMs = round3(lo + float64(i+1)*w)
			}
			for _, x := range v {
				i := int((x - lo) / w)
				if i < 0 {
					i = 0
				}
				if i >= latHistBins {
					i = latHistBins - 1
				}
				fl.Hist[i].Count++
			}
		}
		out = append(out, fl)
	}
	return out
}
