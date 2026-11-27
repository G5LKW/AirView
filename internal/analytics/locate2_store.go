package analytics

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/store"
	"airview/internal/terrain"
)

// Locate Me v2 on an Analyzer: loading the reception samples of the
// Analyzer's station, the process-wide inputs (terrain, the receiver's own
// GNSS position, T=1 timing) and the integration into the guess game.

const (
	l2WindowDays   = 7       // samples older than this are not used
	l2LoadMax      = 400_000 // samples loaded per run (thinned beyond)
	l2RunTimeout   = 90 * time.Second
	l2ThinBucketMs = 10_000
)

// LocateSources are the process-wide inputs of Locate Me v2. Every field is
// optional; nil functions mean "not available".
type LocateSources struct {
	// Terrain is the elevation model (terrain.Provider: cached tiles only).
	Terrain terrain.Source
	// GNSS returns the receiver's own GNSS position (Radarcape / GPS feed)
	// for scoring a standalone station (Locate2Result.ErrorKm). It is never
	// an input of the estimate.
	GNSS func() (lat, lon float64, ok bool)
	// Timing returns the T=1 timing tracks of a GPS-stamping receiver
	// (L2TimingCollector.Tracks).
	Timing func() []L2TimingTrack
}

var l2Src struct {
	mu sync.RWMutex
	s  LocateSources
}

// SetLocateSources installs the process-wide Locate Me v2 inputs (call once
// at start-up; later calls replace them). They apply to the standalone
// Analyzer; a station Analyzer of an aggregator uses only Terrain (its GNSS
// and timing belong to the host, not to the station).
func SetLocateSources(s LocateSources) {
	l2Src.mu.Lock()
	l2Src.s = s
	l2Src.mu.Unlock()
}

func locateSources() LocateSources {
	l2Src.mu.RLock()
	defer l2Src.mu.RUnlock()
	return l2Src.s
}

// LocateRequest selects the inputs of Analyzer.LocateMe. The zero value
// uses the process-wide sources and the configured position as reference.
type LocateRequest struct {
	// Terrain overrides LocateSources.Terrain (nil = the process-wide one).
	Terrain terrain.Source
	// Timing overrides LocateSources.Timing (nil = the process-wide one on
	// a standalone Analyzer, none on a station Analyzer).
	Timing []L2TimingTrack
	// Ref is the position the estimate is scored against (nil = the
	// receiver's GNSS position, else the configured position).
	Ref       *LatLon
	RefSource string // label of Ref ("survey", "gnss", ...)
	// PriorLat/PriorLon centre the search (0, 0 = derived from the data).
	PriorLat, PriorLon float64
}

// stationIdx is the station index of the Analyzer's guess samples (0 =
// standalone).
func (a *Analyzer) stationIdx() (int, error) {
	if a.prefix == "" {
		return 0, nil
	}
	p := strings.TrimPrefix(a.prefix, "s")
	n, err := strconv.ParseUint(p, 10, 32)
	if err != nil || p == a.prefix {
		return 0, fmt.Errorf("analytics: locate: station prefix %q is not s<idx>", a.prefix)
	}
	return int(n), nil
}

// LocateMe runs Locate Me v2 (DESIGN §10.4) on the station's reception
// samples of the last 7 days (table guess_samples; a standalone station
// without enough of them falls back to its stored ADS-B trails). The result
// is cached like GuessLocation; Invalidate forces a new run. Owner/admin
// data: it locates the station (§8.2).
func (a *Analyzer) LocateMe(ctx context.Context, req LocateRequest) (*Locate2Result, error) {
	cfg := a.cfg()
	src := locateSources()
	if req.Terrain == nil {
		req.Terrain = src.Terrain
	}
	if req.Timing == nil && a.prefix == "" && src.Timing != nil {
		req.Timing = src.Timing()
	}
	refLat, refLon, refSrc, hasRef := 0.0, 0.0, "", false
	switch {
	case req.Ref != nil:
		refLat, refLon, refSrc, hasRef = req.Ref.Lat, req.Ref.Lon, req.RefSource, true
	case a.prefix == "" && src.GNSS != nil:
		if la, lo, ok := src.GNSS(); ok && geo.ValidLatLon(la, lo) {
			refLat, refLon, refSrc, hasRef = la, lo, "gnss", true
		}
	}
	if !hasRef {
		if s := siteOf(cfg, ""); s.located {
			refLat, refLon, refSrc, hasRef = s.lat, s.lon, "configured", true
		}
	}
	var refKey int64
	if hasRef {
		refKey = int64(math.Round(refLat*1e4))*1_000_000_000 + int64(math.Round(refLon*1e4))
	}
	key := a.key("locate2", "", cfg, refKey, boolInt(len(req.Timing) > 0), boolInt(req.Terrain != nil),
		int64(math.Round(req.PriorLat*1e3)), int64(math.Round(req.PriorLon*1e3)))
	return cached(ctx, a, key, ttlGuess, func(ctx context.Context) (*Locate2Result, error) {
		ctx, cancel := context.WithTimeout(ctx, l2RunTimeout)
		defer cancel()
		ss, source, err := a.locateSamples(ctx)
		if err != nil {
			return nil, err
		}
		r, err := Locate2(ctx, ss, Locate2Options{Terrain: req.Terrain, Timing: req.Timing,
			PriorLat: req.PriorLat, PriorLon: req.PriorLon})
		if errors.Is(err, errL2Cancelled) && ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("analytics: locate: no result within %v", l2RunTimeout)
		}
		if err != nil {
			return nil, err
		}
		r.Source = source
		if hasRef && r.OK {
			r.ErrorKm = Num(round(geo.HaversineKm(refLat, refLon, r.Lat, r.Lon), 3))
			r.RefSource = refSrc
		}
		return r, nil
	})
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// locateSamples loads the samples of the last l2WindowDays.
func (a *Analyzer) locateSamples(ctx context.Context) ([]L2Sample, string, error) {
	stn, err := a.stationIdx()
	if err != nil {
		return nil, "", err
	}
	now, _ := a.clock()
	fromMs := now.Add(-l2WindowDays * 24 * time.Hour).UnixMilli()
	n, err := a.st.GuessSampleCount(ctx, stn, fromMs)
	if err != nil && !isNoTable(err) {
		return nil, "", err
	}
	if n >= l2MinSamples || a.prefix != "" {
		keep := l2Keep(n)
		out := make([]L2Sample, 0, min(n, l2LoadMax))
		if n > 0 {
			err = a.st.ScanGuessSamples(ctx, stn, fromMs, 0, func(g store.GuessSample) error {
				if !keep(g.ICAO, g.T) {
					return nil
				}
				out = append(out, L2Sample{T: g.T, ICAO: g.ICAO, Lat: g.Lat, Lon: g.Lon, AltFt: g.AltFt,
					RSSI: g.RSSI, Ch: g.Ch, Ground: g.Ground})
				return nil
			})
			if err != nil {
				return nil, "", err
			}
		}
		return out, store.GuessSamplesTable, nil
	}
	// Standalone without enough guess samples yet (a new install or one
	// upgraded from a version without the table): the stored trails. They
	// hold ADS-B positions with signal levels but no surface flag, and are
	// down-sampled, so the endpoint term sees coarser session ends.
	var out []L2Sample
	err = a.st.ForEachPosition(ctx, fromMs/1000, now.Unix(), func(icao uint32, p store.Position) error {
		if p.Src != store.SrcADSB || !isNum(p.Alt) {
			return nil
		}
		if len(out) >= 4*l2LoadMax {
			return errL2Enough
		}
		rssi := p.RSSI
		if rssi == 0 { // NULL reads as 0
			rssi = math.NaN()
		}
		out = append(out, L2Sample{T: p.T * 1000, ICAO: icao, Lat: p.Lat, Lon: p.Lon, AltFt: p.Alt,
			RSSI: rssi, Ch: 0})
		return nil
	})
	if err != nil && !errors.Is(err, errL2Enough) {
		return nil, "", err
	}
	if len(out) > l2LoadMax {
		keep := l2Keep(int64(len(out)))
		th := out[:0]
		for _, s := range out {
			if keep(s.ICAO, s.T) {
				th = append(th, s)
			}
		}
		out = th
	}
	return out, "positions", nil
}

var errL2Enough = errors.New("enough samples")

func isNoTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// l2Keep returns a deterministic thinning filter that keeps about
// l2LoadMax of n samples: whole 10 s buckets of one aircraft are kept or
// dropped together, so sessions stay sessions (with longer gaps).
func l2Keep(n int64) func(icao uint32, tMs int64) bool {
	if n <= l2LoadMax {
		return func(uint32, int64) bool { return true }
	}
	frac := float64(l2LoadMax) / float64(n)
	lim := uint32(frac * math.MaxUint32)
	return func(icao uint32, tMs int64) bool {
		h := fnv.New32a()
		var b [12]byte
		b[0], b[1], b[2], b[3] = byte(icao), byte(icao>>8), byte(icao>>16), byte(icao>>24)
		k := tMs / l2ThinBucketMs
		for i := 0; i < 8; i++ {
			b[4+i] = byte(k >> (8 * i))
		}
		h.Write(b[:])
		return h.Sum32() <= lim
	}
}

// ---------------------------------------------------------------- recorder

// GuessRecorderOptions bounds the guess_samples table.
type GuessRecorderOptions struct {
	// Retention: samples older than this go (0 = 7 days).
	Retention time.Duration
	// PerStation caps one station's rows (0 = 2,000,000).
	PerStation int64
	// Total caps all rows (0 = 10,000,000; aggregator hosts record only the
	// stations Want selects).
	Total int64
	// PruneEvery is the interval of the bound enforcement (0 = 10 min).
	PruneEvery time.Duration
	// Want selects the stations whose samples are recorded (nil = all).
	// An aggregator passes "has no confirmed location yet".
	Want func(model.StationIdx) bool
}

// GuessRecorder writes the tracker's guess samples (tracker.SetGuessSampleHook)
// to guess_samples and keeps the table bounded.
//
//	rec := analytics.NewGuessRecorder(st, analytics.GuessRecorderOptions{})
//	tr.SetGuessSampleHook(rec.Hook)
//	defer rec.Close()
type GuessRecorder struct {
	st  *store.Store
	opt GuessRecorderOptions
	now func() time.Time

	mu        sync.Mutex
	lastPrune time.Time
	pruning   bool
	closed    bool
	wg        sync.WaitGroup
	written   int64
	pruned    int64
}

// NewGuessRecorder returns a recorder writing to st.
func NewGuessRecorder(st *store.Store, opt GuessRecorderOptions) *GuessRecorder {
	if opt.Retention <= 0 {
		opt.Retention = l2WindowDays * 24 * time.Hour
	}
	if opt.PerStation <= 0 {
		opt.PerStation = 2_000_000
	}
	if opt.Total <= 0 {
		opt.Total = 10_000_000
	}
	if opt.PruneEvery <= 0 {
		opt.PruneEvery = 10 * time.Minute
	}
	// lastPrune zero: the first Record enforces the bounds (a restart after
	// a long stop finds old rows)
	return &GuessRecorder{st: st, opt: opt, now: time.Now}
}

// GuessInput is one sample handed to Record (the fields of
// tracker.GuessSample that are stored).
type GuessInput struct {
	T        int64
	Station  model.StationIdx
	ICAO     uint32
	Lat, Lon float64
	AltFt    float64
	RSSI     float64
	Channel  int
	OnGround bool
}

// Record queues samples for the store (the slice may be reused after the
// call) and runs the bound enforcement when it is due. Safe for concurrent
// use; it never blocks on the database.
func (r *GuessRecorder) Record(in []GuessInput) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	buf := make([]store.GuessSample, 0, len(in))
	for i := range in {
		g := &in[i]
		if r.opt.Want != nil && !r.opt.Want(g.Station) {
			continue
		}
		buf = append(buf, store.GuessSample{Stn: int(g.Station), T: g.T, ICAO: g.ICAO, Lat: g.Lat, Lon: g.Lon,
			AltFt: g.AltFt, RSSI: g.RSSI, Ch: g.Channel, Ground: g.OnGround})
	}
	due := !r.pruning && r.now().Sub(r.lastPrune) >= r.opt.PruneEvery
	if due {
		r.pruning = true
		r.lastPrune = r.now()
		r.wg.Add(1)
	}
	r.written += int64(len(buf))
	r.mu.Unlock()
	if len(buf) > 0 {
		r.st.AddGuessSamples(buf) // copies
	}
	if due {
		go func() {
			defer r.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			n, _ := r.Prune(ctx)
			cancel()
			r.mu.Lock()
			r.pruning = false
			r.pruned += n
			r.mu.Unlock()
		}()
	}
}

// Prune enforces the bounds now and returns the number of rows deleted.
func (r *GuessRecorder) Prune(ctx context.Context) (int64, error) {
	return r.st.PruneGuessSamples(ctx, store.GuessPruneLimits{
		CutoffMs:   r.now().Add(-r.opt.Retention).UnixMilli(),
		PerStation: r.opt.PerStation,
		Total:      r.opt.Total,
	})
}

// Stats returns the rows handed to the store and the rows pruned so far.
func (r *GuessRecorder) Stats() (written, pruned int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.written, r.pruned
}

// Close stops recording and waits for a running prune.
func (r *GuessRecorder) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.wg.Wait()
}
