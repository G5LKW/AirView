package engine

// Experimental bistatic SSR self-location (internal/mlat/ssr,
// docs/aggregator/SSR-RESEARCH.md, DESIGN §19.3). Off by default
// (ssr.enabled). When on, every frame of ONE local feed (ssr.feedId, else
// the first feed that delivers timestamped frames) is decoded here on the
// receiver's own time base: ADS-B airborne positions (CPR, global then
// locally relative to the aircraft's last position), Mode A identities
// from DF5/21, Mode A/C replies (Beast type '1') and DF11 all-call replies.
// The estimator runs Process once a second and Solve every
// ssr.solveEveryMin minutes (or on request) on its own goroutine.
//
// The result is advisory: it is returned by SSRStatus for the owner/admin
// views only and is never written to the receiver position, the station
// registry or anything public. Nothing here runs while ssr.enabled is
// false: the feed tee pays one atomic load per frame.

import (
	"context"
	"math"
	"reflect"
	"sync"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/ssr"
	"airview/internal/mlat/track"
	"airview/internal/model"
	"airview/internal/modes"
)

// ssrConfigFromModel maps the file block onto ssr.Config. Fields with the
// same Go name and kind are copied by reflection (TestSSRConfigFromModel
// keeps the two in step); sites and conspicuity codes are converted.
func ssrConfigFromModel(m model.SSRConfig) ssr.Config {
	var c ssr.Config
	dst := reflect.ValueOf(&c).Elem()
	src := reflect.ValueOf(m)
	for i := 0; i < dst.NumField(); i++ {
		f := dst.Type().Field(i)
		sv := src.FieldByName(f.Name)
		if !sv.IsValid() || sv.Kind() != f.Type.Kind() {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Bool, reflect.Int, reflect.Float64:
			dst.Field(i).Set(sv)
		}
	}
	for _, s := range m.Sites {
		c.Sites = append(c.Sites, ssr.Site{Name: s.Name, Lat: s.Lat, Lon: s.Lon, HeightM: s.HeightM, SigmaHM: s.SigmaHM, SigmaUM: s.SigmaUM})
	}
	for _, s := range m.ConspicuityCodes {
		if v, ok := model.ParseOctalCode(s); ok {
			c.ConspicuityCodes = append(c.ConspicuityCodes, v)
		}
	}
	c.Normalize()
	return c
}

// ssrAircraft is the per-aircraft decoder state.
type ssrAircraft struct {
	even, odd   ssrCPR
	lat, lon    float64 // last decoded position
	posT        float64 // its local time (0 = none)
	squawk      int32   // octal value, -1 unknown
	altBaroFt   float64
	gnssDeltaFt float64 // GNSS height minus baro altitude (TC19), NaN unknown
	sigmaM      float64
	lastSeenLoc float64
}

type ssrCPR struct {
	lat, lon uint32
	t        float64 // local time, 0 = none
}

// ssrClock unwraps one feed's receiver timestamps into seconds.
type ssrClock struct {
	kind   int // 0 none, 1 GPS ns of day, 2 12 MHz counter, 3 network ticks
	last   float64
	dayOff float64
	acc    uint64 // unwrapped 12 MHz counter
	raw    uint64
	init   bool
}

// local returns the receiver time of f in seconds, or false.
func (c *ssrClock) local(f *model.RawFrame) (float64, bool) {
	var t float64
	switch {
	case f.GPSNs >= 0 && (c.kind == 0 || c.kind == 1):
		c.kind = 1
		t = float64(f.GPSNs)/1e9 + c.dayOff
		if c.init && t < c.last-43200 {
			c.dayOff += 86400 // across UTC midnight
			t += 86400
		}
	case f.Clock12MHz != 0 && (c.kind == 0 || c.kind == 2):
		c.kind = 2
		const mask = 1<<48 - 1
		v := f.Clock12MHz & mask
		if !c.init {
			c.acc, c.raw = v, v
		} else {
			d := (v - c.raw) & mask
			if d < 1<<47 {
				c.acc += d
				c.raw = v
			} else {
				// a slightly older frame (reordering): do not move the base
				back := (c.raw - v) & mask
				if back > c.acc {
					return 0, false
				}
				return float64(c.acc-back) / 12e6, true
			}
		}
		t = float64(c.acc) / 12e6
	case f.ClockCls.Timed() && f.Ticks != 0 && (c.kind == 0 || c.kind == 3):
		c.kind = 3
		hz := f.ClockCls.NominalHz()
		if hz <= 0 {
			return 0, false
		}
		t = float64(f.Ticks) / hz
	default:
		return 0, false
	}
	if t > c.last || !c.init {
		c.last = t
	}
	c.init = true
	return t, true
}

// ssrWiring is the running estimator.
type ssrWiring struct {
	cfgM   model.SSRConfig
	est    *ssr.Estimator
	hint   func() ssr.Hint
	cancel context.CancelFunc
	done   chan struct{}
	solveC chan struct{}

	mu      sync.Mutex // guards the decoder state below
	feedID  string
	clk     ssrClock
	now     float64 // newest local time
	ac      map[uint32]*ssrAircraft
	msg     modes.Message
	frames  uint64
	ignored uint64 // frames of other feeds / untimed
	resets  uint64
	pos     uint64
	replies uint64

	resMu     sync.Mutex
	last      *ssr.Result
	lastAt    time.Time
	radars    []ssr.SweepRadar // interrogators located from beam sweeps (last solve)
	solving   bool
	solveErr  string
	solveRuns uint64
}

const ssrMaxAircraft = 4096

func newSSRWiring(m model.SSRConfig, hint func() ssr.Hint) *ssrWiring {
	ctx, cancel := context.WithCancel(context.Background())
	w := &ssrWiring{
		cfgM:   m.Clone(),
		est:    ssr.New(ssrConfigFromModel(m)),
		hint:   hint,
		cancel: cancel,
		done:   make(chan struct{}),
		solveC: make(chan struct{}, 1),
		feedID: m.FeedID,
		ac:     map[uint32]*ssrAircraft{},
	}
	go w.loop(ctx)
	return w
}

func (w *ssrWiring) close() {
	w.cancel()
	<-w.done
}

// observe handles one frame from the feed tee (any goroutine).
func (w *ssrWiring) observe(f *model.RawFrame) {
	if f.MLAT || len(f.Data) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.feedID == "" {
		if f.GPSNs < 0 && f.Clock12MHz == 0 && !(f.ClockCls.Timed() && f.Ticks != 0) {
			w.ignored++
			return
		}
		w.feedID = f.FeedID // the first timestamped feed
	}
	if f.FeedID != w.feedID {
		w.ignored++
		return
	}
	t, ok := w.clk.local(f)
	if !ok {
		w.ignored++
		return
	}
	// A receiver restart or epoch change: the time base jumped. Start over.
	if w.now != 0 && (t > w.now+60 || t < w.now-60) {
		w.restartLocked(f)
		var ok bool
		if t, ok = w.clk.local(f); !ok {
			return
		}
	}
	if t > w.now {
		w.now = t
	}
	w.frames++
	if f.Kind == model.FrameModeAC {
		w.modeAC(f, t)
		return
	}
	if modes.Decode(f.Data, &w.msg) != nil {
		return
	}
	m := &w.msg
	switch m.DF {
	case 11:
		if m.CRC != modes.CRCOK && m.CRC != modes.CRCInterrogator {
			return
		}
		code, si, ok := m.Interrogator()
		if !ok {
			return
		}
		iid := code
		if si {
			iid = code + 16
		}
		w.est.AddReply(ssr.Reply{T: t, Class: ssr.ClassModeS, ICAO: m.ICAO, IID: iid})
		w.replies++
	case 5, 21:
		if m.Valid&modes.FSquawk != 0 {
			if a := w.ac[m.ICAO]; a != nil { // address/parity: only known aircraft
				a.squawk = octalOfNibbles(m.Squawk)
			}
		}
	case 17, 18:
		if m.CRC != modes.CRCOK || !m.AddrType.IsICAO() {
			return
		}
		a := w.aircraft(m.ICAO, t)
		if a == nil {
			return
		}
		if m.Valid&modes.FSquawk != 0 {
			a.squawk = octalOfNibbles(m.Squawk)
		}
		if m.Valid&modes.FNACp != 0 {
			if epu := modes.NACpEPU(m.NACp); epu > 0 && !math.IsInf(epu, 0) {
				a.sigmaM = epu / 2.45 // 95 % radius -> 1σ
			}
		}
		if m.Valid&modes.FAltBaro != 0 {
			a.altBaroFt = float64(m.AltBaro)
		}
		if m.Valid&modes.FGNSSDelta != 0 {
			a.gnssDeltaFt = float64(m.GNSSDelta)
		}
		if m.Valid&modes.FCPR == 0 || m.CPRSurface || m.Valid&(modes.FAltBaro|modes.FAltGNSS) == 0 {
			return
		}
		w.position(m, a, t)
	}
}

// restartLocked forgets the time base and the estimator's data.
func (w *ssrWiring) restartLocked(f *model.RawFrame) {
	w.clk = ssrClock{}
	w.now = 0
	w.ac = map[uint32]*ssrAircraft{}
	w.resets++
	w.est = ssr.New(w.est.Config())
	_ = f
}

func (w *ssrWiring) aircraft(icao uint32, t float64) *ssrAircraft {
	a := w.ac[icao]
	if a == nil {
		if len(w.ac) >= ssrMaxAircraft {
			return nil
		}
		a = &ssrAircraft{squawk: -1, altBaroFt: math.NaN(), gnssDeltaFt: math.NaN()}
		w.ac[icao] = a
	}
	a.lastSeenLoc = t
	return a
}

// position decodes an airborne CPR position and hands it to the estimator.
func (w *ssrWiring) position(m *modes.Message, a *ssrAircraft, t float64) {
	cur := ssrCPR{lat: m.CPRLat, lon: m.CPRLon, t: t}
	if m.CPROdd {
		a.odd = cur
	} else {
		a.even = cur
	}
	var lat, lon float64
	ok := false
	if a.posT != 0 && t-a.posT < 30 {
		lat, lon, ok = modes.CPRAirborneLocal(a.lat, a.lon, m.CPRLat, m.CPRLon, m.CPROdd)
		if ok && geo.ToECEF(lat, lon, 0).Sub(geo.ToECEF(a.lat, a.lon, 0)).Norm() > 20e3 {
			ok = false // implausible jump: fall back to a global decode
		}
	}
	if !ok && a.even.t != 0 && a.odd.t != 0 && math.Abs(a.even.t-a.odd.t) < 10 {
		lat, lon, ok = modes.CPRAirborneGlobal(a.even.lat, a.even.lon, a.odd.lat, a.odd.lon, m.CPROdd)
	}
	if !ok {
		return
	}
	a.lat, a.lon, a.posT = lat, lon, t
	// Height above the ellipsoid: the barometric altitude corrected by the
	// GNSS-baro difference of the velocity messages when known.
	h := a.altBaroFt * 0.3048
	if !math.IsNaN(a.gnssDeltaFt) {
		h = (a.altBaroFt + a.gnssDeltaFt) * 0.3048
	}
	if m.Valid&modes.FAltGNSS != 0 {
		h = float64(m.AltGNSS) * 0.3048
	}
	w.est.AddPosition(m.ICAO, ssr.Position{
		T: t, P: geo.ToECEF(lat, lon, h), SigmaM: a.sigmaM, Squawk: a.squawk, AltBaroFt: a.altBaroFt,
	})
	w.pos++
}

// modeAC hands a Mode A/C reply to the estimator.
func (w *ssrWiring) modeAC(f *model.RawFrame, t float64) {
	if len(f.Data) != 2 {
		return
	}
	v := uint16(f.Data[0])<<8 | uint16(f.Data[1])
	var raw uint16
	if w.cfgM.ModeACCodeFormat == "nibbles" {
		r, ok := track.ACRawFromNibbles(v)
		if !ok {
			return
		}
		raw = r
	} else {
		if v&^0x1FFF != 0 {
			return
		}
		raw = v
	}
	if raw&track.ACCodeMask == 0 {
		return
	}
	w.est.AddReply(ssr.Reply{T: t, Class: ssr.ClassModeAC, Code: track.ACSquawk(raw)})
	w.replies++
}

// octalOfNibbles converts modes.Message.Squawk (one octal digit per hex
// nibble, 0x7700) to its octal value (0o7700).
func octalOfNibbles(v uint16) int32 {
	return int32(v>>12&7)<<9 | int32(v>>8&7)<<6 | int32(v>>4&7)<<3 | int32(v&7)
}

// tick runs Process and prunes the aircraft table.
func (w *ssrWiring) tick() {
	w.mu.Lock()
	now := w.now
	est := w.est
	if now != 0 {
		for k, a := range w.ac {
			if now-a.lastSeenLoc > 120 {
				delete(w.ac, k)
			}
		}
	}
	w.mu.Unlock()
	if now != 0 {
		est.Process(now)
	}
}

func (w *ssrWiring) loop(ctx context.Context) {
	defer close(w.done)
	every := time.Duration(w.cfgM.SolveEveryMin) * time.Minute
	if every <= 0 {
		every = 10 * time.Minute
	}
	sec := time.NewTicker(time.Second)
	defer sec.Stop()
	solveT := time.NewTicker(every)
	defer solveT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sec.C:
			w.tick()
		case <-solveT.C:
			w.solve()
		case <-w.solveC:
			w.solve()
		}
	}
}

// solve runs one solve (on the wiring's goroutine).
func (w *ssrWiring) solve() {
	w.resMu.Lock()
	w.solving = true
	w.resMu.Unlock()
	w.tick()
	w.mu.Lock()
	est := w.est
	w.mu.Unlock()
	var h ssr.Hint
	if w.hint != nil {
		h = w.hint()
	}
	res := est.Solve(h)
	radars := est.LocateRadars(h)
	w.resMu.Lock()
	w.last, w.lastAt, w.solving = &res, time.Now(), false
	w.radars = radars
	w.solveRuns++
	w.resMu.Unlock()
}

// requestSolve asks the loop for a solve now (false: one is already queued).
func (w *ssrWiring) requestSolve() bool {
	select {
	case w.solveC <- struct{}{}:
		return true
	default:
		return false
	}
}

// SSRStatus is the owner/admin view of the experimental SSR
// self-location (never public; DESIGN §8).
type SSRStatus struct {
	Enabled      bool   `json:"enabled"`
	Experimental bool   `json:"experimental"` // always true
	FeedID       string `json:"feedId,omitempty"`
	Frames       uint64 `json:"frames"`
	Ignored      uint64 `json:"ignored"`
	Positions    uint64 `json:"positions"`
	Replies      uint64 `json:"replies"`
	Resets       uint64 `json:"resets"`
	Aircraft     int    `json:"aircraft"`
	// Estimator counters (ssr.Stats): PairMatches is the number to watch
	// before trusting a solve (SSR-RESEARCH.md §5).
	Estimator ssrStatsDTO `json:"estimator"`
	Solving   bool        `json:"solving"`
	SolveRuns uint64      `json:"solveRuns"`
	// Last is the most recent solve (nil before the first one).
	Last   *SSRResultDTO `json:"last,omitempty"`
	LastAt int64         `json:"lastAt,omitempty"` // Unix ms
	// Radars are the interrogators located from the timing of their beam
	// sweeps (DF11 all-call bursts with an II/SI code; ssr.LocateBySweep),
	// independent of the self-location above. Formal errors only.
	Radars []SSRSweepRadarDTO `json:"radars,omitempty"`
}

// SSRSweepRadarDTO is one interrogator located from its beam sweeps.
type SSRSweepRadarDTO struct {
	Code   string `json:"code"` // "II 5" / "SI 12"
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// Lat / Lon are omitted when no fit was possible.
	Lat *float64 `json:"lat,omitempty"`
	Lon *float64 `json:"lon,omitempty"`
	// Formal 1σ error ellipse (m) and the azimuth of its major axis.
	SemiMajorM float64 `json:"semiMajorM,omitempty"`
	SemiMinorM float64 `json:"semiMinorM,omitempty"`
	OrientDeg  float64 `json:"orientDeg,omitempty"`
	// PeriodS: antenna rotation period, negative when anticlockwise.
	PeriodS      float64 `json:"periodS,omitempty"`
	PeriodSigmaS float64 `json:"periodSigmaS,omitempty"`
	Coherence    float64 `json:"coherence"`
	RMSDeg       float64 `json:"rmsDeg,omitempty"`
	NObs         int     `json:"nObs"`
	NUsed        int     `json:"nUsed"`
	NAircraft    int     `json:"nAircraft"`
	SpreadDeg    float64 `json:"spreadDeg,omitempty"`
	SpanS        float64 `json:"spanS,omitempty"`
	RangeMinM    float64 `json:"rangeMinM,omitempty"`
	RangeMaxM    float64 `json:"rangeMaxM,omitempty"`
	// Ref / RefDistM: the nearest configured ssr.sites entry.
	Ref      string   `json:"ref,omitempty"`
	RefDistM *float64 `json:"refDistM,omitempty"`
}

func ssrSweepDTO(rs []ssr.SweepRadar) []SSRSweepRadarDTO {
	out := make([]SSRSweepRadarDTO, 0, len(rs))
	for i := range rs {
		r := &rs[i]
		d := SSRSweepRadarDTO{
			Code: r.Code, OK: r.OK, Reason: r.Reason, Coherence: fin(r.Coherence), NObs: r.NObs, NUsed: r.NUsed, NAircraft: r.NAircraft,
		}
		if r.NUsed > 0 && !math.IsNaN(r.LLH.Lat) && !math.IsNaN(r.LLH.Lon) {
			lat, lon := r.LLH.Lat, r.LLH.Lon
			d.Lat, d.Lon = &lat, &lon
			d.SemiMajorM, d.SemiMinorM, d.OrientDeg = fin(r.SemiMajorM), fin(r.SemiMinorM), fin(r.OrientDeg)
			d.PeriodS, d.PeriodSigmaS = fin(r.PeriodS), fin(r.PeriodSigmaS)
			d.RMSDeg, d.SpreadDeg, d.SpanS = fin(r.RMSDeg), fin(r.SpreadDeg), fin(r.SpanS)
			d.RangeMinM, d.RangeMaxM = fin(r.RangeMinM), fin(r.RangeMaxM)
			if r.RefDistM >= 0 {
				v := fin(r.RefDistM)
				d.Ref, d.RefDistM = r.Ref, &v
			}
		}
		out = append(out, d)
	}
	return out
}

type ssrStatsDTO struct {
	Replies       uint64 `json:"replies"`
	ResolvedAC    uint64 `json:"resolvedAc"`
	UnresolvedAC  uint64 `json:"unresolvedAc"`
	AmbiguousAC   uint64 `json:"ambiguousAc"`
	Dwells        uint64 `json:"dwells"`
	DwellsNoTrack uint64 `json:"dwellsNoTrack"`
	PairsTried    uint64 `json:"pairsTried"`
	PairMatches   uint64 `json:"pairMatches"`
	Observations  int    `json:"observations"`
	Tracks        int    `json:"tracks"`
	SweepBursts   int    `json:"sweepBursts"` // DF11 bursts with an II/SI code kept for the radar locator
}

// SSRResultDTO is a JSON-safe ssr.Result (NaN/Inf become 0 / omitted).
type SSRResultDTO struct {
	OK         bool             `json:"ok"`
	Reason     string           `json:"reason,omitempty"`
	Lat        float64          `json:"lat"`
	Lon        float64          `json:"lon"`
	HaeM       float64          `json:"haeM"`
	SigmaHM    float64          `json:"sigmaHM"`
	SigmaUM    float64          `json:"sigmaUM"`
	SemiMajorM float64          `json:"semiMajorM"`
	SemiMinorM float64          `json:"semiMinorM"`
	NObs       int              `json:"nObs"`
	NUsed      int              `json:"nUsed"`
	NAircraft  int              `json:"nAircraft"`
	NRadars    int              `json:"nRadars"`
	RMSNs      float64          `json:"rmsNs"`
	Converged  bool             `json:"converged"`
	Radars     []SSRInterrogDTO `json:"radars,omitempty"`
}

// SSRInterrogDTO is one interrogator of a result.
type SSRInterrogDTO struct {
	Name       string  `json:"name,omitempty"`
	Configured bool    `json:"configured"`
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	SigmaHM    float64 `json:"sigmaHM"`
	Obs        int     `json:"obs"`
	PeriodS    float64 `json:"periodS,omitempty"`
}

func fin(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func ssrResultDTO(r *ssr.Result) *SSRResultDTO {
	d := &SSRResultDTO{
		OK: r.OK, Reason: r.Reason, Lat: fin(r.LLH.Lat), Lon: fin(r.LLH.Lon), HaeM: fin(r.LLH.H),
		SigmaHM: fin(r.SigmaHM), SigmaUM: fin(r.SigmaUM), SemiMajorM: fin(r.SemiMajorM), SemiMinorM: fin(r.SemiMinorM),
		NObs: r.NObs, NUsed: r.NUsed, NAircraft: r.NAircraft, NRadars: r.NRadars, RMSNs: fin(r.RMSNs), Converged: r.Converged,
	}
	for i := range r.Radars {
		x := &r.Radars[i]
		d.Radars = append(d.Radars, SSRInterrogDTO{
			Name: x.Name, Configured: x.Configured, Lat: fin(x.LLH.Lat), Lon: fin(x.LLH.Lon),
			SigmaHM: fin(x.SigmaHM()), Obs: x.Obs, PeriodS: fin(x.Rotation.PeriodS),
		})
	}
	return d
}

func (w *ssrWiring) status() SSRStatus {
	w.mu.Lock()
	s := SSRStatus{Enabled: true, Experimental: true, FeedID: w.feedID, Frames: w.frames, Ignored: w.ignored,
		Positions: w.pos, Replies: w.replies, Resets: w.resets, Aircraft: len(w.ac)}
	est := w.est
	w.mu.Unlock()
	st := est.Stats()
	s.Estimator = ssrStatsDTO{Replies: st.Replies, ResolvedAC: st.ResolvedAC, UnresolvedAC: st.UnresolvedAC,
		AmbiguousAC: st.AmbiguousAC, Dwells: st.Dwells, DwellsNoTrack: st.DwellsNoTrack, PairsTried: st.PairsTried,
		PairMatches: st.PairMatches, Observations: st.Observations, Tracks: st.Tracks,
		SweepBursts: st.SweepObs}
	w.resMu.Lock()
	s.Solving, s.SolveRuns = w.solving, w.solveRuns
	if w.last != nil {
		s.Last = ssrResultDTO(w.last)
		s.LastAt = w.lastAt.UnixMilli()
	}
	if w.radars != nil {
		s.Radars = ssrSweepDTO(w.radars)
	}
	w.resMu.Unlock()
	return s
}

// syncSSR starts, rebuilds or stops the estimator after a configuration
// change. Caller holds opMu.
func (e *Engine) syncSSR(cfg model.Config) {
	cur := e.ssr.Load()
	want := cfg.SSR
	if !want.Enabled {
		if cur != nil {
			e.ssr.Store(nil)
			cur.close()
			e.log.Info("experimental SSR self-location stopped")
		}
		return
	}
	if cur != nil && reflect.DeepEqual(cur.cfgM, want) {
		return
	}
	w := newSSRWiring(want, e.ssrHint)
	if old := e.ssr.Swap(w); old != nil {
		old.close()
	}
	e.log.Info("experimental SSR self-location running (advisory only, never applied)", "feed", want.FeedID)
}

// ssrHint is the search centre of a solve: the configured receiver
// position when there is one.
func (e *Engine) ssrHint() ssr.Hint {
	eff := e.eff.Load()
	if eff == nil || !eff.cfg.Receiver.Located {
		return ssr.Hint{}
	}
	r := eff.cfg.Receiver
	return ssr.Hint{Valid: true, Pos: geo.LLH{Lat: r.Lat, Lon: r.Lon, H: r.GroundElevM}, RadiusM: 50e3}
}

// closeSSR stops the estimator (Stop).
func (e *Engine) closeSSR() {
	if w := e.ssr.Swap(nil); w != nil {
		w.close()
	}
}

// SSRStatus returns the experimental SSR self-location state (owner/admin
// only; Enabled false when ssr.enabled is off).
func (e *Engine) SSRStatus() SSRStatus {
	if w := e.ssr.Load(); w != nil {
		return w.status()
	}
	return SSRStatus{Experimental: true}
}

// SSRSolveNow queues a solve on the estimator's goroutine (about 0.3 s).
// It returns false when SSR is off or a solve is already queued.
func (e *Engine) SSRSolveNow() bool {
	if w := e.ssr.Load(); w != nil {
		return w.requestSolve()
	}
	return false
}
