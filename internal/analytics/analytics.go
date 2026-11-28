// Package analytics is AirView's read-side analysis layer: it turns the
// statistics tables filled by the tracker (see the schema reference in
// package store) into JSON-ready results for the HTTP API.
//
//   - traffic.go   overview, rates over time, heat map of the week, daily
//     totals, top lists, altitude distribution, records / events
//   - coverage.go  polar range plots, GeoJSON coverage polygons, radio
//     horizon comparison, measured skyline / obstructions, heat-grid export
//   - antenna.go   RSSI-vs-distance, measured azimuth / elevation patterns
//     against the modelled ones, yagi lobe fit, link-budget table
//   - score.go     station score and plain-language findings
//   - daynight.go  day / twilight / night and hour-of-day effects
//   - weather.go   weather impact: correlations, response curves, ridge
//     regression, ducting episodes
//   - guess.go     receiver-location guesser
//   - locate2*.go  Locate Me v2 (guess_samples, grid posterior, recorder,
//     T=1 timing collector; see locate2_doc.go)
//   - sun.go       sun / terminator passthrough
//
// Every method takes a context and is safe for concurrent use. Results are
// cached per query class (a few seconds for live traffic, minutes for the
// heavy analyses) so pages can poll cheaply on a Raspberry Pi; a cached
// result is shared between callers and MUST be treated as read-only.
//
// Conventions of the results: timestamps are Unix milliseconds, distances
// km, altitudes ft, angles degrees true, signal dBFS. No result ever
// contains NaN: unknown numbers are JSON null (types Num / Nums) or omitted.
package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

// Cache lifetimes per query class.
const (
	ttlLive     = 10 * time.Second // minute-resolution traffic
	ttlTraffic  = 60 * time.Second // hour-resolution traffic, daily totals, top lists
	ttlCoverage = 5 * time.Minute  // coverage, heat grid
	ttlAntenna  = 10 * time.Minute // antenna analysis
	ttlScore    = ttlCoverage      // score: never outlives the coverage it is built from
	ttlWeather  = 10 * time.Minute // weather impact, day/night
	ttlGuess    = 15 * time.Minute // location guesser
	ttlSun      = 30 * time.Second

	maxCacheEntries = 160
)

// Analyzer runs the analyses. Create it with New.
type Analyzer struct {
	st  *store.Store
	cfg func() model.Config

	// Station scope (aggregator mode; see WithAntPrefix / WithNetwork).
	prefix string       // ant key prefix "s<idx>" ("" = standalone)
	net    *store.Store // airview.db when st is stations.db (nil = st)
	sid    string       // station SID for flight_station scoping

	opts Options // wired inputs (WithOptions)

	mu    sync.Mutex
	cache map[string]*cacheEntry
	now   func() time.Time
	loc   *time.Location
	gen   atomic.Uint64
}

// nextGen returns a process-unique stamp for a freshly computed result, so
// a derived result can be keyed by the exact inputs it was built from.
func (a *Analyzer) nextGen() uint64 { return a.gen.Add(1) }

// New returns an Analyzer reading from st. cfg returns the current
// configuration (e.g. config.Holder.Get); nil selects model.DefaultConfig.
// Options select a station of an aggregator (WithAntPrefix, WithNetwork);
// without options the Analyzer is the standalone one.
//
//	analytics.New(airviewDB, cfg)                                     // standalone
//	analytics.New(stationsDB, stationCfg, analytics.WithAntPrefix("s7"),
//	    analytics.WithNetwork(airviewDB, sid))                         // one station
func New(st *store.Store, cfg func() model.Config, opts ...Option) *Analyzer {
	if cfg == nil {
		cfg = model.DefaultConfig
	}
	a := &Analyzer{st: st, cfg: cfg, cache: map[string]*cacheEntry{}, now: time.Now, loc: time.Local}
	for _, o := range opts {
		if o != nil {
			o(a)
		}
	}
	return a
}

// SetLocation sets the time zone used for "local" hour-of-day / weekday /
// day boundaries (default time.Local). The offset in force now is applied
// to the whole range (daylight-saving switches inside it are ignored).
func (a *Analyzer) SetLocation(loc *time.Location) {
	if loc == nil {
		loc = time.Local
	}
	a.mu.Lock()
	a.loc = loc
	a.cache = map[string]*cacheEntry{}
	a.mu.Unlock()
}

// SetClock replaces the time source (tests).
func (a *Analyzer) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	a.mu.Lock()
	a.now = now
	a.cache = map[string]*cacheEntry{}
	a.mu.Unlock()
}

// Invalidate drops every cached result (call after a factory reset, a
// database import or when results must reflect the latest flush at once).
func (a *Analyzer) Invalidate() {
	a.mu.Lock()
	a.cache = map[string]*cacheEntry{}
	a.mu.Unlock()
}

func (a *Analyzer) clock() (time.Time, int64) {
	a.mu.Lock()
	now, loc := a.now, a.loc
	a.mu.Unlock()
	t := now()
	_, off := t.In(loc).Zone()
	return t, int64(off)
}

// ----------------------------------------------------------------- ranges

// Range selects a time span in Unix SECONDS, From inclusive, To exclusive.
// The zero value means "the default span of the query, ending now". To <= 0
// means now; From <= 0 means To - Span (or the query's default span).
type Range struct {
	From int64
	To   int64
	Span int64 // seconds, used when From is not set
}

// RangeMs builds a Range from Unix-millisecond bounds as used by the HTTP
// API (0 = not set).
func RangeMs(fromMs, toMs int64) Range { return Range{From: fromMs / 1000, To: toMs / 1000} }

// Last is the span d ending now.
func Last(d time.Duration) Range { return Range{Span: int64(d / time.Second)} }

// LastHours is the last n hours.
func LastHours(n int) Range { return Range{Span: int64(n) * 3600} }

// LastDays is the last n days.
func LastDays(n int) Range { return Range{Span: int64(n) * 86400} }

// resolve fills the defaults and aligns the bounds outwards to gran seconds
// (so that polls within one granule share a cache entry).
func (r Range) resolve(now int64, defSpan, gran int64) (from, to int64) {
	to = r.To
	if to <= 0 || to > now+gran {
		to = now
	}
	from = r.From
	if from <= 0 {
		span := r.Span
		if span <= 0 {
			span = defSpan
		}
		from = to - span
	}
	if from >= to {
		from = to - gran
	}
	if gran > 1 {
		from = floorDiv(from, gran) * gran
		to = floorDiv(to+gran-1, gran) * gran
	}
	return from, to
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// ------------------------------------------------------------------ cache

type cacheEntry struct {
	mu   sync.Mutex
	done bool
	val  any
	exp  time.Time
}

// cached returns the cached value for key or computes it with fn. Concurrent
// callers of the same key wait for one computation. Errors are not cached.
func cached[T any](ctx context.Context, a *Analyzer, key string, ttl time.Duration, fn func(context.Context) (T, error)) (T, error) {
	return cachedFor(ctx, a, key, ttl, nil, fn)
}

// cachedFor is cached with a lifetime that may depend on the result:
// ttlFor (nil = always ttl) returns the lifetime for a computed value; a
// result with lifetime <= 0 is returned but not kept, so the next call
// recomputes it. Used to keep empty / near-empty answers (a fresh database)
// from hiding the first data for the full ttl.
func cachedFor[T any](ctx context.Context, a *Analyzer, key string, ttl time.Duration, ttlFor func(T) time.Duration, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	a.mu.Lock()
	e := a.cache[key]
	if e == nil {
		if len(a.cache) >= maxCacheEntries {
			a.pruneLocked()
		}
		e = &cacheEntry{}
		a.cache[key] = e
	}
	a.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done && time.Now().Before(e.exp) {
		if v, ok := e.val.(T); ok {
			return v, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	v, err := fn(ctx)
	if err != nil {
		e.done = false
		e.val = nil
		return zero, err
	}
	if ttlFor != nil {
		ttl = min(ttl, ttlFor(v))
	}
	if ttl <= 0 {
		e.done, e.val = false, nil
		return v, nil
	}
	e.val, e.done, e.exp = v, true, time.Now().Add(ttl)
	return v, nil
}

// sparseTTL is the lifetime of a traffic answer built from fewer than
// sparseFlights flights: short, so that a fresh database fills in quickly.
// Answers with no flights at all are not cached (ttl 0).
const (
	sparseTTL     = 10 * time.Second
	sparseFlights = 20
)

func flightsTTL(n int64) time.Duration {
	switch {
	case n <= 0:
		return 0
	case n < sparseFlights:
		return sparseTTL
	}
	return ttlTraffic
}

// pruneLocked removes expired entries, and everything when that is not enough.
func (a *Analyzer) pruneLocked() {
	now := time.Now()
	for k, e := range a.cache {
		if e.mu.TryLock() {
			expired := !e.done || now.After(e.exp)
			e.mu.Unlock()
			if expired {
				delete(a.cache, k)
			}
		}
	}
	if len(a.cache) >= maxCacheEntries {
		a.cache = map[string]*cacheEntry{}
	}
}

// cfgKey fingerprints the parts of the configuration that influence results.
func cfgKey(c model.Config) string {
	h := fnv.New64a()
	b, _ := json.Marshal(struct {
		R model.Receiver
		A []model.Antenna
	}{c.Receiver, c.Antennas})
	h.Write(b)
	return strconv.FormatUint(h.Sum64(), 36)
}

func (a *Analyzer) key(name, ant string, c model.Config, parts ...int64) string {
	k := name + "|" + ant + "|" + cfgKey(c)
	for _, p := range parts {
		k += "|" + strconv.FormatInt(p, 10)
	}
	return k
}

// ------------------------------------------------------------- JSON types

// Num is a float64 that encodes NaN / Inf as JSON null and is rounded to
// four decimals.
type Num float64

// MarshalJSON implements json.Marshaler.
func (n Num) MarshalJSON() ([]byte, error) {
	return appendNum(nil, float64(n), 1e4), nil
}

// Nums is a []float64 that encodes NaN / Inf as null, rounded to three
// decimals (compact arrays for charts).
type Nums []float64

// MarshalJSON implements json.Marshaler.
func (n Nums) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 2+8*len(n))
	b = append(b, '[')
	for i, v := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendNum(b, v, 1e3)
	}
	return append(b, ']'), nil
}

func appendNum(b []byte, v, scale float64) []byte {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return append(b, "null"...)
	}
	if math.Abs(v) < 1e12 {
		v = math.Round(v*scale) / scale
	}
	if v == 0 {
		return append(b, '0') // also -0
	}
	return strconv.AppendFloat(b, v, 'f', -1, 64)
}

func nanSlice(n int) Nums {
	s := make(Nums, n)
	for i := range s {
		s[i] = math.NaN()
	}
	return s
}

func isNum(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func round(v float64, decimals int) float64 {
	if !isNum(v) {
		return 0
	}
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func pct(part, total float64) float64 {
	if !(total > 0) {
		return 0
	}
	return 100 * part / total
}

// LatLon is a WGS84 position.
type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// ------------------------------------------------------------------- site

// site is the station geometry for one antenna selection.
type site struct {
	located bool
	lat     float64
	lon     float64
	groundM float64
	antM    float64 // antenna height AMSL used for horizon geometry
	ants    []model.Antenna
	ant     *model.Antenna // the selected antenna, nil for "" or an unknown id
}

func siteOf(c model.Config, antID string) site {
	s := site{
		located: c.Receiver.Located && isNum(c.Receiver.Lat) && isNum(c.Receiver.Lon) &&
			math.Abs(c.Receiver.Lat) <= 90 && math.Abs(c.Receiver.Lon) <= 180,
		lat: c.Receiver.Lat, lon: c.Receiver.Lon, ants: c.Antennas,
	}
	if isNum(c.Receiver.GroundElevM) {
		s.groundM = c.Receiver.GroundElevM
	}
	agl := 0.0
	for i := range c.Antennas {
		an := &c.Antennas[i]
		if antID != "" && an.ID == antID {
			s.ant = an
		}
		if isNum(an.HeightAGLM) && an.HeightAGLM > agl {
			agl = an.HeightAGLM
		}
	}
	if s.ant != nil && isNum(s.ant.HeightAGLM) && s.ant.HeightAGLM > 0 {
		agl = s.ant.HeightAGLM
	}
	if agl <= 0 {
		agl = 5
	}
	s.antM = s.groundM + agl
	if s.antM < 1 {
		s.antM = 1
	}
	return s
}

func (s site) pos() *LatLon {
	if !s.located {
		return nil
	}
	return &LatLon{Lat: round(s.lat, 5), Lon: round(s.lon, 5)}
}

var compass16 = [...]string{"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"}

// compass returns the 16-point compass name of a bearing.
func compass(deg float64) string {
	if !isNum(deg) {
		return ""
	}
	d := math.Mod(deg, 360)
	if d < 0 {
		d += 360
	}
	return compass16[int(d/22.5+0.5)%16]
}

func fmtDeg(v float64) string { return fmt.Sprintf("%.0f°", v) }

// Now returns the analyzer's current time and the UTC offset (seconds) of
// its "local" time zone in force now: the same clock and offset that the
// day boundaries of Daily use.
func (a *Analyzer) Now() (time.Time, int64) { return a.clock() }
