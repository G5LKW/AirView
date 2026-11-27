package routes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"airview/internal/model"
	"airview/internal/navdata"
	"airview/internal/store"
)

// Nav is the part of navdata the resolver uses (an interface for tests).
type Nav interface {
	StoredRoute(ctx context.Context, cs string) (navdata.RouteRecord, bool, error)
	CachedRoute(ctx context.Context, cs string) (navdata.RouteRecord, bool, bool, error)
	OnlineRoute(ctx context.Context, cs string) (navdata.RouteRecord, bool, error)
	AirportByCode(ctx context.Context, code string, runways bool) (navdata.Airport, bool, error)
}

// Options configure a Service.
type Options struct {
	Store  *store.Store // nil: no overrides, learning or exports (tests)
	Nav    Nav
	Logger *slog.Logger
	// Config returns the current route settings (called often; must be cheap).
	Config func() model.RoutesConfig
	// OverridesFile is the path of routes.overrides.csv ("" = none).
	OverridesFile string
	Now           func() time.Time
}

// Service resolves, scores and learns routes (docs/ROUTES-DESIGN.md §2).
// Live is the 1 Hz path: map reads and arithmetic only; look-ups run on
// the worker goroutines started by Run.
type Service struct {
	o   Options
	log *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry // by callsign
	tracks  map[string]*track // by ICAO
	sweep   time.Time

	ovMu      sync.RWMutex
	dbOv      map[string]Override
	fileOv    map[string]Override
	fileMod   time.Time
	fileBad   []string
	airMu     sync.Mutex
	airports  map[string]Stop // code -> resolved stop (bounded)
	resolveQ  chan string
	onlineQ   chan string
	statsMu   sync.Mutex
	stats     Stats
	learnMu   sync.Mutex // serialises CloseLeg rebuilds
	lastPrune time.Time
}

// Stats are counters for the status endpoint.
type Stats struct {
	Entries        int    `json:"entries"`
	Resolved       uint64 `json:"resolved"`
	OnlineQueries  uint64 `json:"onlineQueries"`
	OnlineFound    uint64 `json:"onlineFound"`
	OnlineErrors   uint64 `json:"onlineErrors"`
	LastOnlineErr  string `json:"lastOnlineError,omitempty"`
	LegsRecorded   uint64 `json:"legsRecorded"`
	ObsAdded       uint64 `json:"obsAdded"`
	LearnedUpdates uint64 `json:"learnedUpdates"`
}

type entry struct {
	hyps      []Hyp
	block     bool
	fixed     bool
	ready     bool
	exp       time.Time
	firstSeen time.Time
	online    bool // an online look-up was queued for it
}

// track is the per-aircraft hysteresis state.
type track struct {
	cs       string
	key      string // displayed hypothesis
	state    string // displayed state
	pend     string
	pendN    int
	swKey    string
	swN      int
	next     time.Time
	seen     time.Time
	lastBest float64

	out    *model.RouteShort // last DTO (immutable once returned)
	ent    *entry            // entry it was built from
	outAt  time.Time
	outExp time.Time
}

// Tunables.
const (
	maxEntries   = 65536
	foundTTL     = time.Hour
	missTTL      = 10 * time.Minute
	pendingTTL   = 30 * time.Second
	evalEvery    = 30 * time.Second
	trackIdle    = 10 * time.Minute
	switchLead   = 1.0
	lOverrideFix = 8.0
	maxAirports  = 20000
	liveRefresh  = 5 * time.Second
)

// New creates a Service; call Run to start its workers.
func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Config == nil {
		def := model.DefaultConfig().NavData.Routes
		o.Config = func() model.RoutesConfig { return def }
	}
	return &Service{o: o, log: o.Logger, entries: map[string]*entry{}, tracks: map[string]*track{},
		dbOv: map[string]Override{}, fileOv: map[string]Override{}, airports: map[string]Stop{},
		resolveQ: make(chan string, 1024), onlineQ: make(chan string, 256)}
}

// Run loads the overrides and runs the resolver and online workers until
// ctx ends.
func (s *Service) Run(ctx context.Context) {
	s.loadOverrides(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.resolveLoop(ctx) }()
	go func() { defer wg.Done(); s.onlineLoop(ctx) }()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-tick.C:
			s.reloadFile()
			s.maybePrune(ctx)
		}
	}
}

// ---------------------------------------------------------------- live path

// Live returns the route of an aircraft for the live picture (1 Hz, no I/O).
func (s *Service) Live(a *model.Aircraft, now time.Time) *model.RouteShort {
	cfg := s.o.Config()
	if !cfg.Enabled || a == nil {
		return nil
	}
	cs := Normalize(a.Callsign)
	if cs == "" || !Lookupable(cs, a.Registration) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.sweep) > time.Minute || now.Before(s.sweep) {
		s.sweepLocked(now)
	}
	e := s.entries[cs]
	if e == nil || now.After(e.exp) {
		first := now
		if e != nil {
			first = e.firstSeen
		}
		if len(s.entries) >= maxEntries {
			s.evictLocked(now)
		}
		select {
		case s.resolveQ <- cs:
			s.entries[cs] = &entry{exp: now.Add(pendingTTL), firstSeen: first}
		default:
		}
		return nil
	}
	if !e.ready || e.block {
		return nil
	}
	if len(e.hyps) == 0 {
		if cfg.Online && !e.online && a.Lat != nil && now.Sub(e.firstSeen) >= time.Duration(cfg.StableSec)*time.Second {
			select {
			case s.onlineQ <- cs:
				e.online = true
			default:
			}
		}
		return nil
	}
	tr := s.trackLocked(a.ICAO, cs, now)
	// The DTO is rebuilt every liveRefresh (and at once when the entry was
	// re-resolved); in between the same immutable value is returned, so the
	// hit path is a few map reads.
	if tr.out != nil && tr.ent == e && now.Before(tr.outExp) && !now.Before(tr.outAt) {
		return tr.out
	}
	o := obsOf(a)
	f := Fuse(e.hyps, o)
	if f.Best < 0 {
		return nil
	}
	bi := s.hysteresisLocked(tr, e, f, now)
	h, sc := e.hyps[bi], f.Scores[bi]
	p := probOf(f, bi)
	if e.fixed {
		p = 1
	}
	tr.out = s.short(h, sc, tr.state, p, o, cfg, now)
	tr.ent, tr.outAt, tr.outExp = e, now, now.Add(liveRefresh)
	return tr.out
}

func obsOf(a *model.Aircraft) Obs {
	o := Obs{Lat: math.NaN(), Lon: math.NaN(), AltFt: math.NaN(), VR: math.NaN(), Track: math.NaN(), GS: math.NaN(), Category: a.Category}
	if a.Lat != nil && a.Lon != nil {
		o.Lat, o.Lon = *a.Lat, *a.Lon
	}
	switch {
	case a.AltBaro != nil:
		o.AltFt = *a.AltBaro
	case a.AltGeom != nil:
		o.AltFt = *a.AltGeom
	}
	if a.VertRate != nil {
		o.VR = *a.VertRate
	}
	if a.Track != nil {
		o.Track = *a.Track
	}
	if a.GS != nil {
		o.GS = *a.GS
	}
	if a.OnGround != nil {
		o.OnGround = *a.OnGround
	}
	return o
}

// probOf is the softmax probability of hypothesis i.
func probOf(f Fused, i int) float64 {
	maxL := 0.0
	for _, l := range f.Logits {
		maxL = math.Max(maxL, l)
	}
	sum := math.Exp(-maxL)
	for _, l := range f.Logits {
		sum += math.Exp(l - maxL)
	}
	return math.Exp(f.Logits[i]-maxL) / sum
}

func (s *Service) trackLocked(icao, cs string, now time.Time) *track {
	tr := s.tracks[icao]
	if tr == nil || tr.cs != cs {
		tr = &track{cs: cs}
		s.tracks[icao] = tr
	}
	tr.seen = now
	return tr
}

// hysteresisLocked applies the switching rules every evalEvery: the shown
// hypothesis changes only when another leads by switchLead logits on two
// consecutive evaluations, and the shown state only when two consecutive
// evaluations agree. Returns the index of the hypothesis to show.
func (s *Service) hysteresisLocked(tr *track, e *entry, f Fused, now time.Time) int {
	cur := -1
	for i := range e.hyps {
		if e.hyps[i].Key == tr.key {
			cur = i
		}
	}
	if cur < 0 { // first evaluation, or the entry was re-resolved
		tr.key, tr.state, tr.pend, tr.pendN, tr.swKey, tr.swN = e.hyps[f.Best].Key, f.Scores[f.Best].State, "", 0, "", 0
		tr.next = now.Add(evalEvery)
		return f.Best
	}
	if now.Before(tr.next) {
		return cur
	}
	tr.next = now.Add(evalEvery)
	if f.Best != cur && f.Logits[f.Best]-f.Logits[cur] >= switchLead {
		if tr.swKey == e.hyps[f.Best].Key {
			tr.swN++
		} else {
			tr.swKey, tr.swN = e.hyps[f.Best].Key, 1
		}
		if tr.swN >= 2 {
			cur = f.Best
			tr.key, tr.state, tr.swKey, tr.swN, tr.pend, tr.pendN = e.hyps[cur].Key, f.Scores[cur].State, "", 0, "", 0
			return cur
		}
	} else {
		tr.swKey, tr.swN = "", 0
	}
	st := f.Scores[cur].State
	switch {
	case st == tr.state:
		tr.pend, tr.pendN = "", 0
	case st == tr.pend:
		tr.pendN++
		if tr.pendN >= 2 {
			tr.state, tr.pend, tr.pendN = st, "", 0
		}
	default:
		tr.pend, tr.pendN = st, 1
	}
	return cur
}

// short builds the live DTO of hypothesis h.
func (s *Service) short(h Hyp, sc Score, state string, p float64, o Obs, cfg model.RoutesConfig, now time.Time) *model.RouteShort {
	src := primary(h)
	r := &model.RouteShort{Source: src.Source, Class: ClassOf(src.Source), State: state, Conf: round(p, 2)}
	// The most permissive class among agreeing sources is what may be shown.
	for _, c := range h.Claims {
		if c.Source != src.Source && classRank(ClassOf(c.Source)) < classRank(r.Class) {
			r.Class, r.Source = ClassOf(c.Source), c.Source
		}
		if c.Variant {
			r.Flags = appendOnce(r.Flags, "variant")
		}
	}
	if h.Stale {
		r.Flags = appendOnce(r.Flags, "stale-standing")
	}
	leg := sc.Leg
	known := knownIdx(h.Stops)
	if leg < 0 || leg+1 >= len(known) {
		// not checkable: first -> last as given
		r.From, r.To = h.Stops[0].Code, h.Stops[len(h.Stops)-1].Code
		fillStop(h.Stops[0], &r.From, &r.FromIATA, &r.FromName)
		fillStop(h.Stops[len(h.Stops)-1], &r.To, &r.ToIATA, &r.ToName)
		r.State = StateUnchecked
		r.Plausible = true
	} else {
		a, b := h.Stops[known[leg]], h.Stops[known[leg+1]]
		fillStop(a, &r.From, &r.FromIATA, &r.FromName)
		fillStop(b, &r.To, &r.ToIATA, &r.ToName)
		r.Checked = true
		r.Plausible = state == StateFits || state == StateUncertain
		r.AlongPct, r.DistRem = sc.AlongPct, sc.DistRemKm
		if cfg.ETA && p >= 0.8 && (state == StateFits || state == StateUncertain) {
			if eta, sig, ok := ETA(b, o, now); ok {
				r.ETA = (eta.Unix() + 15) / 30 * 30
				r.ETASigma = int(sig.Seconds()+15) / 30 * 30
			}
		}
	}
	if len(h.Stops) > 2 {
		r.Via = len(h.Stops) - 2
		for _, st := range h.Stops {
			r.Stops = append(r.Stops, st.Code)
		}
		if r.Checked {
			r.Leg, r.Legs = leg+1, len(known)-1
		}
	}
	r.Reason = Reason(h, sc)
	return r
}

func classRank(c string) int {
	switch c {
	case ClassOpen:
		return 0
	case ClassOwn:
		return 1
	}
	return 2
}

func primary(h Hyp) Claim {
	best := h.Claims[0]
	for _, c := range h.Claims[1:] {
		if c.Logit > best.Logit {
			best = c
		}
	}
	return best
}

func appendOnce(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func knownIdx(st []Stop) []int {
	var out []int
	for i := range st {
		if st[i].Known {
			out = append(out, i)
		}
	}
	return out
}

func fillStop(st Stop, code, iata, name *string) {
	*code, *iata, *name = st.Code, st.IATA, st.Name
}

func (s *Service) sweepLocked(now time.Time) {
	s.sweep = now
	for k, tr := range s.tracks {
		if now.Sub(tr.seen) > trackIdle || now.Before(tr.seen) {
			delete(s.tracks, k)
		}
	}
	for k, e := range s.entries {
		if now.Sub(e.exp) > time.Hour {
			delete(s.entries, k)
		}
	}
}

func (s *Service) evictLocked(now time.Time) {
	for k, e := range s.entries {
		if now.After(e.exp) {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= maxEntries {
		// still full: drop an arbitrary eighth (map order is random)
		n := len(s.entries) / 8
		for k := range s.entries {
			if n == 0 {
				break
			}
			delete(s.entries, k)
			n--
		}
	}
}

// Invalidate forgets the cached resolution of a callsign (all when cs is
// "") so that the next Live call resolves it again.
func (s *Service) Invalidate(cs string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cs == "" {
		for _, e := range s.entries {
			e.exp = time.Time{}
		}
		return
	}
	if e := s.entries[Normalize(cs)]; e != nil {
		e.exp = time.Time{}
	}
}

// RecheckStanding marks the entries that use standing data for a new
// look-up (after a reference-data import), without wiping the others.
func (s *Service) RecheckStanding() {
	s.airMu.Lock()
	clear(s.airports)
	s.airMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if len(e.hyps) == 0 {
			e.exp = time.Time{}
			continue
		}
		for _, h := range e.hyps {
			for _, c := range h.Claims {
				if c.Source == SrcStanding {
					e.exp = time.Time{}
				}
			}
		}
	}
}

// ---------------------------------------------------------------- resolver

func (s *Service) resolveLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cs := <-s.resolveQ:
			e, err := s.resolve(ctx, cs)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				s.log.Debug("routes: resolving", "callsign", cs, "err", err)
			}
			s.put(cs, e)
		}
	}
}

func (s *Service) put(cs string, e *entry) {
	now := s.o.Now()
	s.mu.Lock()
	if old := s.entries[cs]; old != nil {
		e.firstSeen = old.firstSeen
		e.online = e.online || old.online
	}
	if e.firstSeen.IsZero() {
		e.firstSeen = now
	}
	s.entries[cs] = e
	s.mu.Unlock()
	s.statsMu.Lock()
	s.stats.Resolved++
	s.statsMu.Unlock()
}

// override returns the effective override of a callsign (file rows win).
func (s *Service) override(cs string, now time.Time) (Override, bool) {
	s.ovMu.RLock()
	defer s.ovMu.RUnlock()
	o, ok := s.fileOv[cs]
	if !ok {
		o, ok = s.dbOv[cs]
	}
	if ok && o.Expires > 0 && now.Unix() >= o.Expires {
		return Override{}, false
	}
	return o, ok
}

// resolve gathers every offline claim (and a cached online answer) for a
// callsign. Never sends a request.
func (s *Service) resolve(ctx context.Context, cs string) (*entry, error) {
	now := s.o.Now()
	e := &entry{ready: true, exp: now.Add(missTTL)}
	hyps := map[string]*Hyp{}
	var order []string
	add := func(codes []string, c Claim) error {
		if len(codes) < 2 {
			return nil
		}
		key := strings.Join(codes, "-")
		h := hyps[key]
		if h == nil {
			stops, err := s.stops(ctx, codes)
			if err != nil {
				return err
			}
			h = &Hyp{Key: key, Stops: stops}
			hyps[key] = h
			order = append(order, key)
		}
		h.Claims = append(h.Claims, c)
		return nil
	}
	if ov, ok := s.override(cs, now); ok {
		switch ov.Mode {
		case ModeBlock:
			e.block, e.exp = true, now.Add(foundTTL)
			return e, nil
		case ModeFixed:
			e.fixed = true
			if err := add(strings.Split(ov.Airports, "-"), Claim{Source: SrcOverride, Logit: lOverrideFix, Why: "set by the operator"}); err != nil {
				return e, err
			}
			e.hyps, e.exp = []Hyp{*hyps[order[0]]}, now.Add(foundTTL)
			return e, nil
		case ModeVerify:
			if err := add(strings.Split(ov.Airports, "-"), Claim{Source: SrcOverride, Logit: lVerify, Why: "operator's route"}); err != nil {
				return e, err
			}
		}
	}
	stale := false
	if s.o.Store != nil {
		ls, err := learnedFor(ctx, s.o.Store.DB(), cs)
		if err != nil {
			return e, err
		}
		for _, l := range ls {
			age := now.Sub(time.Unix(l.LastT, 0))
			why := fmt.Sprintf("learned %d days", l.Days)
			if err := add(strings.Split(l.Airports, "-"), Claim{Source: SrcLearned, Logit: LearnedLogit(l.Days, age), Why: why}); err != nil {
				return e, err
			}
			stale = stale || l.StaleStanding
		}
	}
	if s.o.Nav != nil {
		rr, ok, err := s.o.Nav.StoredRoute(ctx, cs)
		if err != nil {
			return e, err
		}
		if ok {
			if err := add(rr.Codes, Claim{Source: SrcStanding, Logit: lStanding, Why: "standing data"}); err != nil {
				return e, err
			}
			if stale {
				hyps[strings.Join(rr.Codes, "-")].Stale = true
			}
		}
		if !ok {
			for _, v := range Variants(cs) {
				vr, vok, err := s.o.Nav.StoredRoute(ctx, v)
				if err != nil {
					return e, err
				}
				if vok {
					if err := add(vr.Codes, Claim{Source: SrcStanding, Logit: lStanding + lVariant, Why: "standing data for " + v, Variant: true}); err != nil {
						return e, err
					}
				}
			}
		}
		if orr, found, cached, err := s.o.Nav.CachedRoute(ctx, cs); err != nil {
			return e, err
		} else if cached {
			e.online = true // answered (or unknown) already: no new request
			if found {
				if err := add(orr.Codes, Claim{Source: SrcAdsbdb, Logit: lOnline, Why: "adsbdb.com"}); err != nil {
					return e, err
				}
			}
		}
	}
	for _, k := range order {
		e.hyps = append(e.hyps, *hyps[k])
	}
	if len(e.hyps) > 0 {
		e.exp = now.Add(foundTTL)
	}
	return e, nil
}

// stops resolves airport codes (cached).
func (s *Service) stops(ctx context.Context, codes []string) ([]Stop, error) {
	out := make([]Stop, 0, len(codes))
	for _, c := range codes {
		c = strings.ToUpper(strings.TrimSpace(c))
		s.airMu.Lock()
		st, ok := s.airports[c]
		s.airMu.Unlock()
		if !ok {
			st = Stop{Code: c, ElevFt: math.NaN()}
			if s.o.Nav != nil {
				a, found, err := s.o.Nav.AirportByCode(ctx, c, false)
				if err != nil {
					return nil, err
				}
				if found {
					st = StopOf(a)
				}
			}
			s.airMu.Lock()
			if len(s.airports) >= maxAirports {
				clear(s.airports)
			}
			s.airports[c] = st
			s.airMu.Unlock()
		}
		out = append(out, st)
	}
	return out, nil
}

// StopOf converts an airport.
func StopOf(a navdata.Airport) Stop {
	st := Stop{Code: a.Code(), IATA: a.IATA, Name: a.Municipality, Lat: a.Lat, Lon: a.Lon, ElevFt: math.NaN(), Size: int(a.Size), Known: true}
	if st.Name == "" {
		st.Name = a.Name
	}
	if a.ElevFt != nil {
		st.ElevFt = float64(*a.ElevFt)
	}
	return st
}

// ---------------------------------------------------------------- online

func (s *Service) onlineLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cs := <-s.onlineQ:
			if !s.o.Config().Online || s.o.Nav == nil {
				continue
			}
			s.statsMu.Lock()
			s.stats.OnlineQueries++
			s.statsMu.Unlock()
			_, found, err := s.o.Nav.OnlineRoute(ctx, cs)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				s.statsMu.Lock()
				s.stats.OnlineErrors++
				s.stats.LastOnlineErr = err.Error()
				s.statsMu.Unlock()
				// allow a retry after the entry expires
				s.mu.Lock()
				if e := s.entries[cs]; e != nil {
					e.online = false
				}
				s.mu.Unlock()
				if errors.Is(err, navdata.ErrRateLimited) {
					t := time.NewTimer(30 * time.Second)
					select {
					case <-ctx.Done():
						t.Stop()
						return
					case <-t.C:
					}
				}
				continue
			}
			if found {
				s.statsMu.Lock()
				s.stats.OnlineFound++
				s.statsMu.Unlock()
			}
			s.Invalidate(cs)
		}
	}
}

// Stats returns the counters.
func (s *Service) Stats() Stats {
	s.mu.Lock()
	n := len(s.entries)
	s.mu.Unlock()
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	st := s.stats
	st.Entries = n
	return st
}

// ---------------------------------------------------------------- detail

// HypView is one hypothesis in the detail view.
type HypView struct {
	Airports string  `json:"airports"`
	Stops    []Stop  `json:"stops"`
	Sources  []Claim `json:"sources"`
	Logit    float64 `json:"logit"`
	P        float64 `json:"p"`
	Score    Score   `json:"score"`
	Class    string  `json:"lic"`
}

// Detail is the full route view of a callsign.
type Detail struct {
	Callsign   string            `json:"callsign"`
	Lookupable bool              `json:"lookupable"`
	Blocked    bool              `json:"blocked,omitempty"`
	Override   *Override         `json:"override,omitempty"`
	Route      *model.RouteShort `json:"route,omitempty"`
	Hyps       []HypView         `json:"hypotheses"`
	Path       [][2]float64      `json:"path,omitempty"` // great circle [lon,lat] of the shown route (lon unwrapped)
	Hidden     int               `json:"hidden,omitempty"`
	Learned    []Learned         `json:"learned,omitempty"`
	Online     string            `json:"online,omitempty"` // "", "queried", "not found", error text
}

// Claim JSON: lowercase keys.
func (c Claim) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, `{"source":%q,"logit":%.2f,"why":%q,"variant":%t}`, c.Source, c.Logit, c.Why, c.Variant), nil
}

// DetailOptions select what a caller may see and do.
type DetailOptions struct {
	Aircraft          *model.Aircraft // current state (optional) for the score
	AllowOnline       bool            // may spend the online budget (owner/admin only)
	IncludeRestricted bool            // may see restricted-class answers (the station's own users)
}

// Detail resolves a callsign for the detail panel. It reads the offline
// data and the cache; only with AllowOnline (and online look-ups enabled)
// does it send a request.
func (s *Service) Detail(ctx context.Context, callsign string, o DetailOptions) (*Detail, error) {
	cs := Normalize(callsign)
	if cs == "" {
		return nil, fmt.Errorf("%w: callsign", ErrBadOverride)
	}
	d := &Detail{Callsign: cs, Lookupable: Lookupable(cs, "")}
	if ov, ok := s.override(cs, s.o.Now()); ok {
		d.Override = &ov
	}
	e, err := s.resolve(ctx, cs)
	if err != nil {
		return nil, err
	}
	if len(e.hyps) == 0 && !e.block && o.AllowOnline && s.o.Config().Online && s.o.Nav != nil && d.Lookupable {
		if _, found, err := s.o.Nav.OnlineRoute(ctx, cs); err != nil {
			d.Online = err.Error()
		} else {
			d.Online = "not found"
			if found {
				d.Online = "found"
				if e, err = s.resolve(ctx, cs); err != nil {
					return nil, err
				}
				s.put(cs, e)
			}
		}
	}
	d.Blocked = e.block
	if s.o.Store != nil {
		d.Learned, _ = learnedFor(ctx, s.o.Store.DB(), cs)
	}
	hs := e.hyps
	if !o.IncludeRestricted {
		hs = nil
		for _, h := range e.hyps {
			var cl []Claim
			for _, c := range h.Claims {
				if ClassOf(c.Source) != ClassRestricted {
					cl = append(cl, c)
				}
			}
			if len(cl) == 0 {
				d.Hidden++
				continue
			}
			h.Claims = cl
			hs = append(hs, h)
		}
	}
	var ob Obs
	if o.Aircraft != nil {
		ob = obsOf(o.Aircraft)
	} else {
		ob = Obs{Lat: math.NaN(), Lon: math.NaN(), AltFt: math.NaN(), VR: math.NaN(), Track: math.NaN(), GS: math.NaN()}
	}
	f := Fuse(hs, ob)
	for i, h := range hs {
		cls := ClassRestricted
		for _, c := range h.Claims {
			if classRank(ClassOf(c.Source)) < classRank(cls) {
				cls = ClassOf(c.Source)
			}
		}
		d.Hyps = append(d.Hyps, HypView{Airports: h.Key, Stops: h.Stops, Sources: h.Claims, Logit: round(f.Logits[i], 2),
			P: round(probOf(f, i), 3), Score: f.Scores[i], Class: cls})
	}
	sort.SliceStable(d.Hyps, func(i, j int) bool { return d.Hyps[i].P > d.Hyps[j].P })
	if f.Best >= 0 && !e.block {
		p := probOf(f, f.Best)
		if e.fixed {
			p = 1
		}
		d.Route = s.short(hs[f.Best], f.Scores[f.Best], f.Scores[f.Best].State, p, ob, s.o.Config(), s.o.Now())
		d.Path = path(hs[f.Best].Stops)
	}
	return d, nil
}

// path is the great-circle polyline over the known stops.
func path(st []Stop) [][2]float64 {
	var pts [][2]float64
	idx := knownIdx(st)
	for i := 0; i+1 < len(idx); i++ {
		a, b := st[idx[i]], st[idx[i+1]]
		seg := navdata.GreatCircle(a.Lat, a.Lon, b.Lat, b.Lon, 50)
		if len(pts) > 0 && len(seg) > 0 {
			seg = seg[1:]
		}
		pts = append(pts, seg...)
	}
	// unwrap longitudes so the line does not jump across the antimeridian
	for i := 1; i < len(pts); i++ {
		for pts[i][0]-pts[i-1][0] > 180 {
			pts[i][0] -= 360
		}
		for pts[i][0]-pts[i-1][0] < -180 {
			pts[i][0] += 360
		}
	}
	return pts
}

// ---------------------------------------------------------------- overrides

func (s *Service) loadOverrides(ctx context.Context) {
	if s.o.Store != nil {
		m, err := loadOverrides(ctx, s.o.Store.DB())
		if err != nil {
			s.log.Warn("routes: loading the overrides", "err", err)
		} else {
			s.ovMu.Lock()
			s.dbOv = m
			s.ovMu.Unlock()
		}
	}
	s.reloadFile()
}

func (s *Service) reloadFile() {
	s.ovMu.RLock()
	mod := s.fileMod
	s.ovMu.RUnlock()
	list, bad, newMod, changed := fileOverrides(s.o.OverridesFile, mod)
	if !changed {
		return
	}
	m := make(map[string]Override, len(list))
	for _, o := range list {
		m[o.Callsign] = o
	}
	s.ovMu.Lock()
	s.fileOv, s.fileMod, s.fileBad = m, newMod, bad
	s.ovMu.Unlock()
	if len(bad) > 0 {
		s.log.Warn("routes: overrides file has invalid rows", "file", filepath.Base(s.o.OverridesFile), "rows", strings.Join(bad, "; "))
	}
	if len(m) > 0 || !mod.IsZero() {
		s.log.Info("routes: overrides file loaded", "rows", len(m))
	}
	s.Invalidate("")
}

// Overrides lists every override (file rows marked locked), by callsign.
func (s *Service) Overrides() []Override {
	s.ovMu.RLock()
	defer s.ovMu.RUnlock()
	out := make([]Override, 0, len(s.dbOv)+len(s.fileOv))
	for cs, o := range s.dbOv {
		if _, locked := s.fileOv[cs]; !locked {
			out = append(out, o)
		}
	}
	for _, o := range s.fileOv {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Callsign < out[j].Callsign })
	return out
}

// FileProblems returns the invalid rows of the overrides file.
func (s *Service) FileProblems() []string {
	s.ovMu.RLock()
	defer s.ovMu.RUnlock()
	return append([]string(nil), s.fileBad...)
}

// PutOverride stores (creates or replaces) an override.
func (s *Service) PutOverride(ctx context.Context, o Override, author string) (Override, error) {
	if s.o.Store == nil {
		return Override{}, errors.New("routes: no store")
	}
	if err := normOverride(&o); err != nil {
		return Override{}, err
	}
	s.ovMu.RLock()
	_, locked := s.fileOv[o.Callsign]
	before := s.dbOv[o.Callsign]
	s.ovMu.RUnlock()
	if locked {
		return Override{}, ErrLocked
	}
	o.Author, o.Created, o.Locked = author, s.o.Now().Unix(), false
	var exp any
	if o.Expires > 0 {
		exp = o.Expires
	}
	err := s.o.Store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO routes_override(callsign, airports, mode, note, author, created, expires) VALUES(?,?,?,?,?,?,?)`,
			o.Callsign, o.Airports, o.Mode, o.Note, o.Author, o.Created, exp); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO routes_override_log(callsign, before, after, author, t) VALUES(?,?,?,?,?)`,
			o.Callsign, before.summary(), o.summary(), author, o.Created)
		return err
	})
	if err != nil {
		return Override{}, err
	}
	s.ovMu.Lock()
	s.dbOv[o.Callsign] = o
	s.ovMu.Unlock()
	s.Invalidate(o.Callsign)
	return o, nil
}

// DeleteOverride removes an override.
func (s *Service) DeleteOverride(ctx context.Context, callsign, author string) error {
	if s.o.Store == nil {
		return errors.New("routes: no store")
	}
	cs := Normalize(callsign)
	s.ovMu.RLock()
	_, locked := s.fileOv[cs]
	before, ok := s.dbOv[cs]
	s.ovMu.RUnlock()
	if locked {
		return ErrLocked
	}
	if !ok {
		return ErrNotFound
	}
	err := s.o.Store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM routes_override WHERE callsign = ?`, cs); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO routes_override_log(callsign, before, after, author, t) VALUES(?,?,?,?,?)`,
			cs, before.summary(), "", author, s.o.Now().Unix())
		return err
	})
	if err != nil {
		return err
	}
	s.ovMu.Lock()
	delete(s.dbOv, cs)
	s.ovMu.Unlock()
	s.Invalidate(cs)
	return nil
}

// OverrideLog returns the newest audit entries.
func (s *Service) OverrideLog(ctx context.Context, limit int) ([]OverrideLog, error) {
	if s.o.Store == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.o.Store.DB().QueryContext(ctx, `SELECT id, callsign, before, after, author, t FROM routes_override_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OverrideLog
	for rows.Next() {
		var l OverrideLog
		if err := rows.Scan(&l.ID, &l.Callsign, &l.Before, &l.After, &l.Author, &l.T); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- learning

// CloseLeg records a closed leg and updates the learned routes of its
// callsign (called by the engine when a flight closes; not on the 1 Hz path).
// The leg's Route must be an open/own-class value (or empty).
func (s *Service) CloseLeg(ctx context.Context, l Leg) error {
	if s.o.Store == nil {
		return nil
	}
	l.Callsign = Normalize(l.Callsign)
	if l.Callsign != "" && !Lookupable(l.Callsign, "") {
		l.Callsign = ""
	}
	learn := s.o.Config().Learn
	var standing string
	if l.Callsign != "" && learn && s.o.Nav != nil {
		if rr, ok, err := s.o.Nav.StoredRoute(ctx, l.Callsign); err == nil && ok {
			standing = strings.Join(rr.Codes, "-")
		}
	}
	s.learnMu.Lock()
	defer s.learnMu.Unlock()
	added, updated := false, false
	err := s.o.Store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if added, err = recordLeg(ctx, tx, l); err != nil {
			return err
		}
		if added && learn {
			if _, err = rebuild(ctx, tx, l.Callsign, standing, s.o.Now()); err != nil {
				return err
			}
			updated = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.statsMu.Lock()
	s.stats.LegsRecorded++
	if added {
		s.stats.ObsAdded++
	}
	if updated {
		s.stats.LearnedUpdates++
	}
	s.statsMu.Unlock()
	if updated {
		s.Invalidate(l.Callsign)
	}
	return nil
}

// RouteForLeg returns the open/own-class route currently resolved for a
// callsign ("" when none), for the leg record.
func (s *Service) RouteForLeg(ctx context.Context, cs string) (route, src string) {
	cs = Normalize(cs)
	if cs == "" {
		return "", ""
	}
	e, err := s.resolve(ctx, cs)
	if err != nil || e.block {
		return "", ""
	}
	bestL := math.Inf(-1)
	for _, h := range e.hyps {
		l := 0.0
		var top Claim
		for _, c := range h.Claims {
			if ClassOf(c.Source) == ClassRestricted {
				continue
			}
			l += c.Logit
			if top.Source == "" || c.Logit > top.Logit {
				top = c
			}
		}
		if top.Source != "" && l > bestL {
			bestL, route, src = l, h.Key, top.Source
		}
	}
	return route, src
}

func (s *Service) maybePrune(ctx context.Context) {
	now := s.o.Now()
	if s.o.Store == nil || now.Sub(s.lastPrune) < 24*time.Hour {
		return
	}
	s.lastPrune = now
	err := s.o.Store.Write(ctx, func(tx *sql.Tx) error {
		_, err := pruneObs(ctx, tx, now)
		return err
	})
	if err != nil && ctx.Err() == nil {
		s.log.Debug("routes: pruning observations", "err", err)
	}
}

// ---------------------------------------------------------------- exports

// LearnedLicence is the licence of the learned-routes export.
const LearnedLicence = "CC0-1.0 (https://creativecommons.org/publicdomain/zero/1.0/): routes learned by AirView from observed take-offs and landings"

// Export kinds.
const (
	ExportLearned   = "learned.csv"
	ExportOverrides = "overrides.csv"
	ExportLegs      = "legs.csv"
)

// Export writes one export (learned routes in VRS standing-data format,
// released CC0; the overrides file format; or the observed legs).
func (s *Service) Export(ctx context.Context, w io.Writer, kind string, from, to int64) error {
	switch kind {
	case ExportOverrides:
		var list []Override
		for _, o := range s.Overrides() {
			if !o.Locked {
				list = append(list, o)
			}
		}
		return WriteOverridesCSV(w, list)
	case ExportLearned:
		return s.exportLearned(ctx, w)
	case ExportLegs:
		return s.exportLegs(ctx, w, from, to)
	}
	return ErrNotFound
}

func (s *Service) exportLearned(ctx context.Context, w io.Writer) error {
	// Plain VRS routes.csv (no comment lines: the format has none); the
	// licence (CC0 1.0) is stated by the HTTP endpoint (X-License header)
	// and in the documentation.
	fmt.Fprintf(w, "Callsign,Code,Number,AirlineCode,AirportCodes\n")
	if s.o.Store == nil {
		return nil
	}
	rows, err := s.o.Store.DB().QueryContext(ctx, `SELECT callsign, airports FROM routes_learned WHERE p >= 0.9 AND n_days >= 3 AND rank = 0 ORDER BY callsign`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cs, ap string
		if err := rows.Scan(&cs, &ap); err != nil {
			return err
		}
		// VRS: Code = airline + number for ICAO callsigns.
		airline, number := cs[:3], cs[3:]
		if _, err := fmt.Fprintf(w, "%s,%s,%s,%s,%s\n", cs, airline, number, airline, ap); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Service) exportLegs(ctx context.Context, w io.Writer, from, to int64) error {
	fmt.Fprintf(w, "flight,start,end,callsign,dep,dep_conf,dep_tier,arr,arr_conf,arr_tier,route,route_src\n")
	if s.o.Store == nil {
		return nil
	}
	if to <= 0 {
		to = math.MaxInt64
	}
	rows, err := s.o.Store.DB().QueryContext(ctx, `SELECT flight, start, "end", callsign, dep, dep_conf, dep_tier, arr, arr_conf, arr_tier, route, route_src
  FROM routes_leg WHERE start >= ? AND start < ? ORDER BY start LIMIT 1000000`, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var fl, st, en int64
		var cs, dep, dt, arr, at, rt, rs string
		var dc, ac float64
		if err := rows.Scan(&fl, &st, &en, &cs, &dep, &dc, &dt, &arr, &ac, &at, &rt, &rs); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%d,%d,%d,%s,%s,%.2f,%s,%s,%.2f,%s,%s,%s\n", fl, st, en, cs, dep, dc, dt, arr, ac, at, rt, rs); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Attribution lists the attribution lines of the route sources in use.
func (s *Service) Attribution() []string {
	out := []string{"Routes: Virtual Radar Server standing data (CC0)", "Learned routes: AirView (CC0)"}
	if s.o.Config().Online {
		out = append(out, "Online routes: adsbdb.com (shown to this station's users only)")
	}
	return out
}
