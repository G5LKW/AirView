package met

import (
	"context"
	"database/sql"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"airview/internal/store"
)

// Geometry of the accumulation grids.
const (
	// LevelFt is the thickness of a profile layer. Level L covers
	// [L*1000, (L+1)*1000) ft of pressure altitude (store.MetLevel).
	LevelFt = 1000.0

	minLevel = -1
	maxLevel = 60
	nLevels  = maxLevel - minLevel + 1

	slotSec = 600 // the current profile is kept in 10-minute slots
	nSlots  = 16  // 160 minutes of slots

	hourSec       = 3600
	memHours      = 96    // hourly profiles kept in memory
	ringSize      = 4096  // recent observations for the map layer
	maxPendingObs = 8000  // met_sample rows waiting for the next flush
	maxBiasTable  = 20000 // learned per-aircraft heading corrections kept
)

// Heading-bias learning parameters.
const (
	biasMinTASKt     = 180.0 // residuals of slow aircraft are dominated by wind error
	biasMaxResidDeg  = 12.0  // larger residuals are wind-field mismatches, not heading bias
	biasMinConsensus = 12    // wind samples needed around the level
	biasMinAircraft  = 3     // ... from at least this many other aircraft
	biasMinSamples   = 12    // residuals before a correction is applied
	biasShrink       = 6.0   // correction = mean * n/(n+biasShrink)
	biasDeadBandDeg  = 0.5   // smaller corrections are not applied: below the noise of the method
	biasMemory       = 300   // effective averaging length (samples)
	biasMaxDeg       = 8.0   // corrections are clamped to this
)

func init() {
	// Learned heading corrections survive restarts in a table of their own.
	_ = store.RegisterMigration("met", 1, `
CREATE TABLE IF NOT EXISTS met_bias(
  icao    INTEGER PRIMARY KEY,
  bias    REAL NOT NULL,
  n       INTEGER NOT NULL,
  updated INTEGER NOT NULL
) WITHOUT ROWID;`)
}

// Options tune a Collector; the zero value gives the defaults.
type Options struct {
	// MinInterval is the per-aircraft rate limit: at most one observation
	// per aircraft is derived and accumulated in this time, calls in
	// between return the previous result. Default 5 s.
	MinInterval time.Duration
	// StoreInterval thins the raw observations written to met_sample (per
	// aircraft). Default 60 s. Negative disables met_sample.
	StoreInterval time.Duration
	// MapInterval thins the in-memory observations kept for the map layer
	// (per aircraft). Default 20 s.
	MapInterval time.Duration
	// Window is the length of the "current" profile. Default 75 min,
	// limited to 10 .. 150 min.
	Window time.Duration
	// MinSamples is the number of observations a level needs before its
	// mean is reported. Default 3.
	MinSamples int
	// MinQuality is the lowest Detail.WindQuality / TempQuality that is
	// accumulated into the profiles. Default 0.3.
	MinQuality float64
	// FlushInterval is how often Run hands accumulated increments to the
	// store. Default 15 s.
	FlushInterval time.Duration
	// MaxAircraft bounds the per-aircraft state. Default 4000.
	MaxAircraft int
	// NoBiasLearning switches the per-aircraft heading correction off.
	NoBiasLearning bool
	Logger         *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.MinInterval <= 0 {
		o.MinInterval = 5 * time.Second
	}
	if o.StoreInterval == 0 {
		o.StoreInterval = time.Minute
	}
	if o.MapInterval <= 0 {
		o.MapInterval = 20 * time.Second
	}
	if o.Window <= 0 {
		o.Window = 75 * time.Minute
	}
	o.Window = min(max(o.Window, 10*time.Minute), 150*time.Minute)
	if o.MinSamples <= 0 {
		o.MinSamples = 3
	}
	if o.MinQuality <= 0 {
		o.MinQuality = 0.3
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 15 * time.Second
	}
	if o.MaxAircraft <= 0 {
		o.MaxAircraft = 4000
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// cell accumulates the observations of one level in one 10-minute slot.
type cell struct {
	u, v, t    float64 // sums
	uu, vv, tt float64 // sums of squares (spread)
	alt        float64 // sum of observation altitudes
	nw, nt, na int32
	ic         [4]uint32 // last distinct contributing aircraft (wind)
	icPos      uint8
}

type slot struct {
	id    int64 // unix / slotSec; 0 = empty
	cells [nLevels]cell
}

type hcell struct {
	u, v, t float64
	nw, nt  int32
}

type hourProf [nLevels]hcell

type acState struct {
	last      Derived
	lastT     int64 // unix ms of the last accepted observation (0 = none)
	seen      int64 // unix ms of the last call
	lastStore int64
	lastRing  int64

	// cached magnetic declination
	mv           float64
	mvLat, mvLon float64
	mvT          int64
}

type biasEntry struct {
	mean    float64 // running mean heading residual, degrees
	sq      float64 // running mean squared residual
	n       int32
	updated int64 // unix s
	dirty   bool
}

func (b *biasEntry) correction() float64 {
	if b == nil || b.n < biasMinSamples {
		return 0
	}
	c := b.mean * float64(b.n) / (float64(b.n) + biasShrink)
	if math.Abs(c) < biasDeadBandDeg {
		// The consensus wind is an area and layer average; residuals this
		// small cannot be told from its representativeness error.
		return 0
	}
	return math.Max(-biasMaxDeg, math.Min(biasMaxDeg, c))
}

type ringObs struct {
	t        int64 // unix ms
	icao     uint32
	lat, lon float32
	alt      float32
	dir, spd float32 // NaN = none
	oat      float32 // NaN = none
	q        float32
	src      uint8
}

// Stats are the Collector's counters (JSON-ready).
type Stats struct {
	Calls        int64            `json:"calls"`        // Observe calls
	Derived      int64            `json:"derived"`      // calls that ran a derivation (not rate-limited)
	Wind         int64            `json:"wind"`         // wind observations accumulated
	Temp         int64            `json:"temp"`         // temperature observations accumulated
	MRARWind     int64            `json:"mrarWind"`     // of which direct BDS 4,4
	MRARTemp     int64            `json:"mrarTemp"`     //
	LowQuality   int64            `json:"lowQuality"`   // derived but below MinQuality
	Rejected     map[string]int64 `json:"rejected"`     // by reason
	Aircraft     int              `json:"aircraft"`     // aircraft currently tracked
	Biases       int              `json:"biases"`       // heading corrections known
	BiasesActive int              `json:"biasesActive"` // ... of which are being applied
	LastObs      int64            `json:"lastObs"`      // Unix ms of the newest accumulated observation (0 = none)
	Dropped      int64            `json:"dropped"`      // met_sample rows dropped because the queue was full
}

// Collector turns a stream of aircraft states into meteorological
// observations and vertical profiles. All methods are safe for concurrent
// use. A nil *store.Store is allowed: everything then lives in memory only.
type Collector struct {
	st  *store.Store
	opt Options
	log *slog.Logger

	mu     sync.Mutex
	ac     map[uint32]*acState
	bias   map[uint32]*biasEntry
	slots  [nSlots]slot
	hours  map[int64]*hourProf
	pend   map[int64]*store.MetProfileInc
	obsQ   []store.MetObs
	ring   []ringObs
	ringN  int // total ever written
	newest int64
	loaded bool
	wrote  bool // something was handed to the store already

	stats   Stats
	rejects [len(rejectNames)]int64
}

// New creates a Collector. Call Load (or Run, which loads first) before
// the first Observe to continue the profiles and heading corrections of the
// previous run.
func New(st *store.Store, opt Options) *Collector {
	opt = opt.withDefaults()
	return &Collector{
		st:    st,
		opt:   opt,
		log:   opt.Logger,
		ac:    make(map[uint32]*acState, 256),
		bias:  make(map[uint32]*biasEntry, 256),
		hours: make(map[int64]*hourProf, memHours+2),
		pend:  make(map[int64]*store.MetProfileInc, 128),
		ring:  make([]ringObs, ringSize),
	}
}

func levelIndex(altFt float64) int { return store.MetLevel(altFt) - minLevel }

// Observe derives wind and temperature from one aircraft state, applies
// the learned heading correction of that airframe, accumulates the result
// into the profiles and returns it. It is cheap enough to be called for
// every tracker update: each aircraft is rate-limited to one derivation per
// Options.MinInterval, calls in between return the previous result.
func (c *Collector) Observe(in Input) Derived {
	if in.T.IsZero() {
		in.T = time.Now()
	}
	tms := in.T.UnixMilli()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Calls++

	a := c.ac[in.ICAO]
	if a != nil && a.lastT != 0 {
		if dt := tms - a.lastT; dt >= 0 && dt < c.opt.MinInterval.Milliseconds() {
			a.seen = tms
			return a.last
		}
	}
	if a == nil && len(c.ac) < c.opt.MaxAircraft {
		a = &acState{mv: math.NaN()}
		c.ac[in.ICAO] = a
	}

	o := deriveOpts{magVar: math.NaN(), tempHintC: math.NaN()}
	if a != nil {
		a.seen = tms
		// Declination changes slowly: re-evaluate the model only after
		// the aircraft has moved about 15 km or six hours have passed.
		if finite(in.Lat) && finite(in.Lon) {
			if !finite(a.mv) || math.Abs(in.Lat-a.mvLat) > 0.15 || math.Abs(in.Lon-a.mvLon) > 0.25 ||
				tms-a.mvT > 6*3600*1000 || tms < a.mvT {
				altKm := 0.0
				if finite(in.AltFt) {
					altKm = in.AltFt * 0.0003048
				}
				a.mv = MagVar(in.Lat, in.Lon, altKm, in.T)
				a.mvLat, a.mvLon, a.mvT = in.Lat, in.Lon, tms
			}
			o.magVar = a.mv
		}
	}

	idx := -1
	if finite(in.AltFt) && in.AltFt >= MinAltFt && in.AltFt <= MaxAltFt {
		idx = levelIndex(in.AltFt)
	}
	slotID := in.T.Unix() / slotSec
	if idx >= 0 {
		o.tempHintC = c.tempHint(idx, slotID)
	}
	var be *biasEntry
	if !c.opt.NoBiasLearning {
		be = c.bias[in.ICAO]
		o.headingCorr = be.correction()
	}

	d := derive(&in, o)
	c.stats.Derived++
	for i := range c.rejects {
		if d.Reject&(1<<i) != 0 {
			c.rejects[i]++
		}
	}

	if !d.HasWind && !d.HasOAT {
		if a != nil {
			a.last = d.Derived
		}
		return d.Derived
	}

	// Learn the airframe's heading bias from the residual against the
	// consensus wind of the other aircraft around this level.
	if !c.opt.NoBiasLearning && idx >= 0 && finite(d.EHSU) && !d.TASDerived && d.TAS >= biasMinTASKt &&
		finite(in.Roll) && math.Abs(in.Roll) <= 2.5 {
		if cu, cv, ok := c.consensus(idx, slotID, in.ICAO); ok {
			st, ct := math.Sincos(in.Track * math.Pi / 180)
			implied := math.Atan2(in.GS*st-cu, in.GS*ct-cv) * 180 / math.Pi
			resid := angDiff(implied, in.MagHeading+d.MagVar)
			if math.Abs(resid) <= biasMaxResidDeg {
				if be == nil && len(c.bias) < maxBiasTable {
					be = &biasEntry{}
					c.bias[in.ICAO] = be
				}
				if be != nil {
					be.n++
					g := 1 / float64(min(be.n, biasMemory))
					be.mean += g * (resid - be.mean)
					be.sq += g * (resid*resid - be.sq)
					be.updated = in.T.Unix()
					be.dirty = true
				}
			}
		}
	}

	useWind := d.HasWind && d.WindQuality >= c.opt.MinQuality
	useTemp := d.HasOAT && d.TempQuality >= c.opt.MinQuality
	if !useWind && !useTemp {
		c.stats.LowQuality++
	}
	if idx >= 0 && (useWind || useTemp) {
		c.accumulate(&in, &d, idx, slotID, useWind, useTemp)
	}
	if a != nil {
		a.last = d.Derived
		a.lastT = tms
		if useWind || useTemp {
			c.record(a, &in, &d, tms, useWind, useTemp)
		}
	}
	return d.Derived
}

// slotFor returns the slot for id, recycling the ring entry when id is
// newer than what it holds; nil when id is too old to be kept.
func (c *Collector) slotFor(id int64) *slot {
	s := &c.slots[int(id%nSlots+nSlots)%nSlots]
	if s.id == id {
		return s
	}
	if id > s.id {
		*s = slot{id: id}
		return s
	}
	return nil
}

func (c *Collector) accumulate(in *Input, d *Detail, idx int, slotID int64, useWind, useTemp bool) {
	hour := in.T.Unix() / hourSec * hourSec
	hp := c.hours[hour]
	if hp == nil {
		hp = new(hourProf)
		c.hours[hour] = hp
		if len(c.hours) > memHours+1 {
			c.trimHours()
		}
	}
	key := hour*64 + int64(idx)
	inc := c.pend[key]
	if inc == nil {
		inc = &store.MetProfileInc{Hour: hour, Level: idx + minLevel}
		c.pend[key] = inc
	}
	h := &hp[idx]
	var cl *cell
	if s := c.slotFor(slotID); s != nil {
		cl = &s.cells[idx]
		cl.alt += in.AltFt
		cl.na++
	}
	if useWind {
		h.u += d.U
		h.v += d.V
		h.nw++
		inc.USum += d.U
		inc.VSum += d.V
		inc.NWind++
		if cl != nil {
			cl.u += d.U
			cl.v += d.V
			cl.uu += d.U * d.U
			cl.vv += d.V * d.V
			cl.nw++
			known := false
			for _, ic := range cl.ic {
				if ic == in.ICAO {
					known = true
					break
				}
			}
			if !known {
				cl.ic[cl.icPos&3] = in.ICAO
				cl.icPos++
			}
		}
		c.stats.Wind++
		if d.WindSource == SourceMRAR {
			c.stats.MRARWind++
		}
	}
	if useTemp {
		h.t += d.OAT
		h.nt++
		inc.TSum += d.OAT
		inc.NTemp++
		if cl != nil {
			cl.t += d.OAT
			cl.tt += d.OAT * d.OAT
			cl.nt++
		}
		c.stats.Temp++
		if d.TempSource == SourceMRAR {
			c.stats.MRARTemp++
		}
	}
	if t := in.T.UnixMilli(); t > c.newest {
		c.newest = t
	}
}

// record thins the observation into the map ring and the met_sample queue.
func (c *Collector) record(a *acState, in *Input, d *Detail, tms int64, useWind, useTemp bool) {
	nan32 := float32(math.NaN())
	var src uint8
	if useWind && d.WindSource == SourceMRAR {
		src |= ObsSrcMRARWind
	}
	if useTemp && d.TempSource == SourceMRAR {
		src |= ObsSrcMRARTemp
	}
	if finite(in.Lat) && finite(in.Lon) && finite(in.AltFt) && (a.lastRing == 0 || tms-a.lastRing >= c.opt.MapInterval.Milliseconds() || tms < a.lastRing) {
		a.lastRing = tms
		r := &c.ring[c.ringN%ringSize]
		c.ringN++
		*r = ringObs{t: tms, icao: in.ICAO, lat: float32(in.Lat), lon: float32(in.Lon), alt: float32(in.AltFt),
			dir: nan32, spd: nan32, oat: nan32, q: float32(d.Quality), src: src}
		if useWind {
			r.dir, r.spd = float32(d.WindDir), float32(d.WindSpd)
		}
		if useTemp {
			r.oat = float32(d.OAT)
		}
	}
	if c.st != nil && c.opt.StoreInterval > 0 &&
		(a.lastStore == 0 || tms-a.lastStore >= c.opt.StoreInterval.Milliseconds() || tms < a.lastStore) {
		a.lastStore = tms
		if len(c.obsQ) >= maxPendingObs {
			c.stats.Dropped++
			return
		}
		m := store.MetObs{T: in.T.Unix(), ICAO: in.ICAO, Lat: in.Lat, Lon: in.Lon, Alt: in.AltFt,
			WindDir: math.NaN(), WindKt: math.NaN(), OATC: math.NaN(), Src: src}
		if useWind {
			m.WindDir, m.WindKt = round1(d.WindDir), round1(d.WindSpd)
		}
		if useTemp {
			m.OATC = round1(d.OAT)
		}
		c.obsQ = append(c.obsQ, m)
	}
}

// Source bits of store.MetObs.Src / Obs.Src: 0 = everything computed from
// EHS.
const (
	ObsSrcMRARWind uint8 = 1 << iota // the wind is a direct BDS 4,4 report
	ObsSrcMRARTemp                   // the temperature is a direct BDS 4,4 report
)

// windowSlots returns the lowest slot id of the profile window ending at
// slot `now`.
func (c *Collector) windowSlots(now int64) int64 {
	n := int64((c.opt.Window + slotSec*time.Second - 1) / (slotSec * time.Second))
	return now - n + 1
}

// tempHint is the current mean temperature at a level (NaN when unknown),
// used to turn Mach into TAS for aircraft that do not report TAS.
func (c *Collector) tempHint(idx int, now int64) float64 {
	lo := c.windowSlots(now)
	var sum float64
	var n int32
	for i := range c.slots {
		s := &c.slots[i]
		if s.id < lo || s.id > now {
			continue
		}
		sum += s.cells[idx].t
		n += s.cells[idx].nt
	}
	if int(n) < max(c.opt.MinSamples, 5) {
		return math.NaN()
	}
	return sum / float64(n)
}

// consensus is the vector-mean wind at a level over the profile window,
// widened to the neighbouring levels when the level itself is thin. ok
// needs enough samples from enough aircraft other than `self`.
func (c *Collector) consensus(idx int, now int64, self uint32) (u, v float64, ok bool) {
	if u, v, ok = c.consensusSpan(idx, idx, now, self); ok {
		return u, v, true
	}
	return c.consensusSpan(max(idx-1, 0), min(idx+1, nLevels-1), now, self)
}

func (c *Collector) consensusSpan(l0, l1 int, now int64, self uint32) (u, v float64, ok bool) {
	lo := c.windowSlots(now)
	var n int32
	var others [8]uint32
	nOthers := 0
	for i := range c.slots {
		s := &c.slots[i]
		if s.id < lo || s.id > now {
			continue
		}
		for l := l0; l <= l1; l++ {
			cl := &s.cells[l]
			if cl.nw == 0 {
				continue
			}
			u += cl.u
			v += cl.v
			n += cl.nw
			for _, ic := range cl.ic {
				if ic == 0 || ic == self || nOthers == len(others) {
					continue
				}
				dup := false
				for _, o := range others[:nOthers] {
					if o == ic {
						dup = true
						break
					}
				}
				if !dup {
					others[nOthers] = ic
					nOthers++
				}
			}
		}
	}
	if n < biasMinConsensus || nOthers < biasMinAircraft {
		return 0, 0, false
	}
	return u / float64(n), v / float64(n), true
}

func (c *Collector) trimHours() {
	keys := make([]int64, 0, len(c.hours))
	for k := range c.hours {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys[:len(keys)-memHours] {
		// Unflushed increments of such an hour live in c.pend and still
		// reach the store.
		delete(c.hours, k)
	}
}

// Flush hands every accumulated profile increment (also those of the open
// hour) and the thinned observations to the store's write queue (no disk
// access on the calling goroutine). Run calls it at shutdown; in between
// Run hands over the observations every FlushInterval and the profile
// increments of an hour once, after the hour has closed.
func (c *Collector) Flush() { c.flush(true, time.Time{}) }

// FlushOpenHour hands the open hour's profile increments (and the pending
// observations) to the store's write queue now, for a table export that
// must contain everything collected so far (server flushForExport; the
// caller commits with Store.Flush). The increments are additive UPSERTs:
// the rest of the hour is merged into the same met_profile rows when the
// hour closes. What History / HourProfile / Current answer is unchanged:
// the in-memory hours still hold the open hour, and without Load the
// table plus the remaining pending increments sum to the same values.
func (c *Collector) FlushOpenHour(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.flush(true, time.Time{})
	return nil
}

// flush hands the thinned observations and the profile increments to the
// store: all of them (all), or those of the hours that closed before now
// (the newer of now and the newest observation time, so replayed and
// simulated time work). One met_profile UPSERT per (hour, level) and hour,
// instead of one per FlushInterval (~7,500 an hour in 1.0.0): the open
// hour is served from memory (History, HourProfile, Current) meanwhile.
func (c *Collector) flush(all bool, now time.Time) {
	if c.st == nil {
		c.mu.Lock()
		clear(c.pend)
		c.obsQ = c.obsQ[:0]
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	ref := max(now.Unix(), c.newest/1000)
	var incs []store.MetProfileInc
	for k, p := range c.pend {
		if all || p.Hour+hourSec <= ref {
			incs = append(incs, *p)
			delete(c.pend, k)
		}
	}
	sort.Slice(incs, func(i, j int) bool {
		if incs[i].Hour != incs[j].Hour {
			return incs[i].Hour < incs[j].Hour
		}
		return incs[i].Level < incs[j].Level
	})
	obs := c.obsQ
	c.obsQ = nil
	if len(incs) > 0 || len(obs) > 0 {
		c.wrote = true
	}
	c.mu.Unlock()

	if len(incs) == 0 && len(obs) == 0 {
		c.mu.Lock()
		if c.obsQ == nil {
			c.obsQ = obs[:0]
		}
		c.mu.Unlock()
		return
	}
	b := store.NewBatch()
	if len(incs) > 0 {
		b.MergeMetProfile(incs)
	}
	if len(obs) > 0 {
		b.AddMetSamples(obs)
	}
	c.st.Submit(b)

	c.mu.Lock()
	if c.obsQ == nil {
		c.obsQ = obs[:0] // reuse the buffer (the batch copied the rows)
	}
	c.mu.Unlock()
}

// SaveBiases queues the heading corrections that changed since the last
// call for the met_bias table (they are committed with the store's next
// group commit; Store.Flush waits for it).
func (c *Collector) SaveBiases(ctx context.Context) error {
	if c.st == nil {
		return nil
	}
	type row struct {
		icao    uint32
		mean    float64
		n       int32
		updated int64
	}
	c.mu.Lock()
	var rows []row
	for icao, b := range c.bias {
		if b.dirty {
			rows = append(rows, row{icao, b.mean, b.n, b.updated})
			b.dirty = false
		}
	}
	c.mu.Unlock()
	if len(rows) == 0 {
		return nil
	}
	// Sorted by key: the UPSERTs walk the table's pages in order.
	sort.Slice(rows, func(i, j int) bool { return rows[i].icao < rows[j].icao })
	// Queued: the rows ride the store's next group commit instead of a
	// commit of their own (and the forced flush of the queue before it).
	c.st.QueueTx("met_bias", len(rows), func(ctx context.Context, tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `INSERT INTO met_bias(icao, bias, n, updated) VALUES(?,?,?,?)
ON CONFLICT(icao) DO UPDATE SET bias=excluded.bias, n=excluded.n, updated=excluded.updated`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, r := range rows {
			if _, err := st.ExecContext(ctx, int64(r.icao), r.mean, int64(r.n), r.updated); err != nil {
				return err
			}
		}
		return nil
	})
	return ctx.Err()
}

// Load restores state from the store: the hourly profiles of the last four
// days, the current profile (rebuilt from the thinned met_sample rows of
// the last window) and the learned heading corrections. It is a no-op
// without a store or when called a second time.
func (c *Collector) Load(ctx context.Context) error {
	c.mu.Lock()
	if c.loaded || c.st == nil {
		c.loaded = true
		c.mu.Unlock()
		return nil
	}
	c.loaded = true
	c.mu.Unlock()

	db := c.st.DB()
	now := time.Now()

	// Hourly profiles.
	type hrow struct {
		hour    int64
		level   int
		u, v, t float64
		nw, nt  int64
	}
	var hrows []hrow
	rs, err := db.QueryContext(ctx, `SELECT hour, level, u_sum, v_sum, t_sum, n_wind, n_temp FROM met_profile WHERE hour >= ?`,
		now.Unix()/hourSec*hourSec-(memHours-1)*hourSec)
	if err != nil {
		return err
	}
	for rs.Next() {
		var r hrow
		if err := rs.Scan(&r.hour, &r.level, &r.u, &r.v, &r.t, &r.nw, &r.nt); err != nil {
			rs.Close()
			return err
		}
		hrows = append(hrows, r)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}

	// Recent raw samples for the current profile.
	orows, err := c.st.MetSamplesSince(ctx, now.Add(-c.opt.Window).Unix(), 50000)
	if err != nil {
		return err
	}

	// Heading corrections.
	type brow struct {
		icao    uint32
		mean    float64
		n       int32
		updated int64
	}
	var brows []brow
	rs, err = db.QueryContext(ctx, `SELECT icao, bias, n, updated FROM met_bias ORDER BY updated DESC LIMIT ?`, maxBiasTable)
	if err != nil {
		return err
	}
	for rs.Next() {
		var r brow
		var icao, n int64
		if err := rs.Scan(&icao, &r.mean, &n, &r.updated); err != nil {
			rs.Close()
			return err
		}
		r.icao, r.n = uint32(icao), int32(min(n, math.MaxInt32))
		brows = append(brows, r)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.wrote {
		for _, r := range hrows {
			idx := r.level - minLevel
			if idx < 0 || idx >= nLevels {
				continue
			}
			hp := c.hours[r.hour]
			if hp == nil {
				hp = new(hourProf)
				c.hours[r.hour] = hp
			}
			h := &hp[idx]
			h.u += r.u
			h.v += r.v
			h.t += r.t
			h.nw += int32(min(r.nw, math.MaxInt32/2))
			h.nt += int32(min(r.nt, math.MaxInt32/2))
		}
		for _, r := range orows {
			if !finite(r.Alt) {
				continue
			}
			idx := levelIndex(r.Alt)
			s := c.slotFor(r.T / slotSec)
			if s == nil || idx < 0 || idx >= nLevels {
				continue
			}
			cl := &s.cells[idx]
			cl.alt += r.Alt
			cl.na++
			if finite(r.WindDir) && finite(r.WindKt) {
				sn, cs := math.Sincos(r.WindDir * math.Pi / 180)
				u, v := -r.WindKt*sn, -r.WindKt*cs
				cl.u += u
				cl.v += v
				cl.uu += u * u
				cl.vv += v * v
				cl.nw++
			}
			if finite(r.OATC) {
				cl.t += r.OATC
				cl.tt += r.OATC * r.OATC
				cl.nt++
			}
			if t := r.T * 1000; t > c.newest {
				c.newest = t
			}
		}
	}
	for _, r := range brows {
		if _, ok := c.bias[r.icao]; !ok && finite(r.mean) && r.n > 0 {
			c.bias[r.icao] = &biasEntry{mean: r.mean, sq: r.mean * r.mean, n: r.n, updated: r.updated}
		}
	}
	return nil
}

// Run loads the persisted state, then periodically flushes increments to
// the store, persists heading corrections and prunes idle aircraft until
// ctx is cancelled. It always returns nil.
func (c *Collector) Run(ctx context.Context) error {
	if err := c.Load(ctx); err != nil && ctx.Err() == nil {
		c.log.Warn("met: loading persisted profiles failed", "err", err)
	}
	tick := time.NewTicker(c.opt.FlushInterval)
	defer tick.Stop()
	lastBias := time.Now()
	for {
		select {
		case <-ctx.Done():
			c.Flush()
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := c.SaveBiases(sctx); err != nil {
				c.log.Warn("met: saving heading corrections failed", "err", err)
			}
			cancel()
			return nil
		case now := <-tick.C:
			c.flush(false, now)
			c.prune(now)
			if now.Sub(lastBias) >= 5*time.Minute {
				lastBias = now
				if err := c.SaveBiases(ctx); err != nil && ctx.Err() == nil {
					c.log.Warn("met: saving heading corrections failed", "err", err)
				}
			}
		}
	}
}

// prune forgets aircraft that have not been heard for 20 minutes. The
// reference is the newest input time, so replayed or simulated time works.
func (c *Collector) prune(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ref := now.UnixMilli()
	var newest int64
	for _, a := range c.ac {
		newest = max(newest, a.seen)
	}
	if newest != 0 && newest < ref {
		ref = newest
	}
	for k, a := range c.ac {
		if ref-a.seen > 20*60*1000 {
			delete(c.ac, k)
		}
	}
}

// Reset drops all in-memory state (after a factory reset of the store).
func (c *Collector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.ac)
	clear(c.bias)
	clear(c.hours)
	clear(c.pend)
	c.slots = [nSlots]slot{}
	c.obsQ = c.obsQ[:0]
	c.ringN = 0
	c.newest = 0
	c.wrote = false
	c.stats = Stats{}
	c.rejects = [len(rejectNames)]int64{}
}

// Stats returns the counters.
func (c *Collector) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Rejected = make(map[string]int64, len(rejectNames))
	for i, n := range c.rejects {
		if n != 0 {
			s.Rejected[rejectNames[i]] = n
		}
	}
	s.Aircraft = len(c.ac)
	s.Biases = len(c.bias)
	for _, b := range c.bias {
		if b.correction() != 0 {
			s.BiasesActive++
		}
	}
	s.LastObs = c.newest
	return s
}

// HeadingBias is the learned heading correction of one airframe.
type HeadingBias struct {
	ICAO       string  `json:"icao"`
	MeanDeg    float64 `json:"meanDeg"`    // mean residual: true heading minus reported heading
	AppliedDeg float64 `json:"appliedDeg"` // correction currently added to the heading (0 until enough samples)
	StdDeg     float64 `json:"stdDeg"`     // scatter of the residuals
	N          int     `json:"n"`
	Updated    int64   `json:"updated"` // Unix ms
}

// HeadingBiases lists the learned corrections, largest |mean| first, at
// most limit entries (0 = 200).
func (c *Collector) HeadingBiases(limit int) []HeadingBias {
	if limit <= 0 {
		limit = 200
	}
	c.mu.Lock()
	out := make([]HeadingBias, 0, len(c.bias))
	for icao, b := range c.bias {
		out = append(out, HeadingBias{
			ICAO:       store.HexICAO(icao),
			MeanDeg:    round2(b.mean),
			AppliedDeg: round2(b.correction()),
			StdDeg:     round2(math.Sqrt(math.Max(b.sq-b.mean*b.mean, 0))),
			N:          int(b.n),
			Updated:    b.updated * 1000,
		})
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		ai, aj := math.Abs(out[i].MeanDeg), math.Abs(out[j].MeanDeg)
		if ai != aj {
			return ai > aj
		}
		return out[i].ICAO < out[j].ICAO
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// HeadingCorrection returns the correction currently applied to an
// aircraft's heading in degrees (0 when none has been learned yet).
func (c *Collector) HeadingCorrection(icao uint32) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bias[icao].correction()
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
