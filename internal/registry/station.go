package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"airview/internal/model"
)

// Flags are station attributes (stations.flags).
type Flags uint32

// Station flags.
const (
	// FlagUnidentified: a compat link without a UUID, keyed by IP (§3).
	FlagUnidentified Flags = 1 << iota
	// FlagLinkGuess: a UUID-less link was joined to this station because it
	// came from the same IP within 120 s (§3).
	FlagLinkGuess
	// FlagBeast / FlagMLATC: the station has had a 30004 / 31090 link.
	FlagBeast
	FlagMLATC
	// FlagRelaySub: a sub-station of a trusted relay (0xE3 receiver ID).
	FlagRelaySub
	// FlagNoAnchor: the admin barred the station from being an MLAT time
	// anchor (DESIGN §9.2.5 condition (c)); it reaches the clock filter as
	// StationInfo.Flagged.
	FlagNoAnchor
	// FlagTimingQuarantine: the station's timing is not proven yet; its
	// copies stay out of MLAT until internal/registry/trust has shown its
	// timestamps consistent with the trusted network (set on creation
	// unless Policy.NoTimingQuarantine; cleared by ProveTiming). See
	// timing.go.
	FlagTimingQuarantine
	// FlagTimingTrusted: the admin trusts the station's timing without
	// proof (override; wins over FlagTimingQuarantine).
	FlagTimingTrusted
	// FlagTimingExcluded: the admin keeps the station out of MLAT
	// altogether (override; wins over everything).
	FlagTimingExcluded
	// FlagSurveyRejected: the owner rejected the automatic use of the
	// reverse-MLAT survey position for MLAT (DESIGN §19.3; persisted
	// mlat.Engine.OverrideSurvey).
	FlagSurveyRejected
)

// Position is an optional geographic position.
type Position struct {
	Lat, Lon float64
	// Alt is the height in metres: HAE for GNSS and estimates; for the
	// declared position it is interpreted per Station.DeclaredAltRef.
	Alt   float64
	Valid bool
}

// Station is one registered receiver clock (DESIGN §3). Values returned by
// the Registry are copies; change a station through the Registry methods.
//
// Station holds exact locations. It must only reach the MLAT engine, the
// owner of the station and the admin; public surfaces use Meta and
// PublicPoint.
type Station struct {
	Idx      model.StationIdx
	SID      string
	FeederID string // native feeders ("" for compat and host)
	RxNo     int
	Kind     string // model.StationKind*
	UUID     string // compat UUID, normalised ("" for native / unidentified)
	IdentKey string // "ip:…" for unidentified stations
	Name     string
	ClockCls model.ClockClass

	Declared       Position
	DeclaredAltRef string // model.AltRef*
	GNSS           Position
	Estimated      Position
	EstSigmaM      float64
	// Effective is the position used for geometry and MLAT; Located says
	// whether it is set.
	Effective model.StationGeo
	Located   bool
	LocStatus string // model.Loc*

	Pin         string // model.Pin*
	ShowName    bool
	PublicStats bool
	OwnerID     int64 // 0 = unclaimed
	// OwnedSince is when the current owner was assigned (zero = unknown:
	// assigned before this was recorded, or unowned). Owner views and
	// exports start here.
	OwnedSince time.Time
	Trust      int
	Admission  string // model.Adm*
	Anchor     bool
	Flags      Flags
	TokenID    string // enrol token used to admit the station
	Created    time.Time
	LastSeen   time.Time
	// Deleting is set once the station's deletion is queued; it is then
	// invisible to every lookup except Station(idx).
	Deleting bool

	// Public is the stored public pin (valid only when HasPublic).
	Public    PublicPoint
	HasPublic bool
}

// Meta returns the non-locating description of the station.
func (s *Station) Meta() model.StationMeta {
	return model.StationMeta{
		Idx: s.Idx, SID: s.SID, Name: s.Name, Kind: s.Kind, ClockCls: s.ClockCls,
		Located: s.Located, Admission: s.Admission, Pin: s.Pin,
		ShowName: s.ShowName, PublicStats: s.PublicStats, Anchor: s.Anchor,
	}
}

// Has reports whether all flags in f are set.
func (s *Station) Has(f Flags) bool { return s.Flags&f == f }

// Station lookup errors.
var (
	ErrStationNotFound = errors.New("registry: station not found")
	ErrInvalid         = errors.New("registry: invalid value")
)

// ---------------------------------------------------------------- cache

const chunkBits = 10

type chunk [1 << chunkBits]atomic.Pointer[Station]

type feederRx struct {
	feeder string
	rx     int
}

// stationCache mirrors the stations table. Reads of single stations by idx
// are lock-free; the secondary indices use an RWMutex.
type stationCache struct {
	dir atomic.Pointer[[]*chunk]

	mu       sync.RWMutex
	bySID    map[string]model.StationIdx
	byFeeder map[feederRx]model.StationIdx
	byUUID   map[string]model.StationIdx
	byIdent  map[string]model.StationIdx
	all      map[model.StationIdx]struct{}
}

func (c *stationCache) init() {
	c.bySID = map[string]model.StationIdx{}
	c.byFeeder = map[feederRx]model.StationIdx{}
	c.byUUID = map[string]model.StationIdx{}
	c.byIdent = map[string]model.StationIdx{}
	c.all = map[model.StationIdx]struct{}{}
	empty := []*chunk{}
	c.dir.Store(&empty)
}

func (c *stationCache) load(idx model.StationIdx) *Station {
	d := *c.dir.Load()
	ci := int(idx >> chunkBits)
	if ci >= len(d) || d[ci] == nil {
		return nil
	}
	return d[ci][idx&(1<<chunkBits-1)].Load()
}

// put stores st (callers hold Registry.wmu, which serialises writers).
func (c *stationCache) put(st *Station) {
	ci := int(st.Idx >> chunkBits)
	d := *c.dir.Load()
	if ci >= len(d) {
		nd := make([]*chunk, ci+1)
		copy(nd, d)
		for i := len(d); i <= ci; i++ {
			nd[i] = new(chunk)
		}
		c.dir.Store(&nd)
		d = nd
	}
	old := d[ci][st.Idx&(1<<chunkBits-1)].Swap(st)
	c.mu.Lock()
	if old != nil {
		c.unindex(old)
	}
	c.bySID[st.SID] = st.Idx
	if st.FeederID != "" {
		c.byFeeder[feederRx{st.FeederID, st.RxNo}] = st.Idx
	}
	if st.UUID != "" {
		c.byUUID[st.UUID] = st.Idx
	}
	if st.IdentKey != "" {
		c.byIdent[st.IdentKey] = st.Idx
	}
	c.all[st.Idx] = struct{}{}
	c.mu.Unlock()
}

func (c *stationCache) unindex(st *Station) {
	delete(c.bySID, st.SID)
	if st.FeederID != "" {
		delete(c.byFeeder, feederRx{st.FeederID, st.RxNo})
	}
	if st.UUID != "" {
		delete(c.byUUID, st.UUID)
	}
	if st.IdentKey != "" {
		delete(c.byIdent, st.IdentKey)
	}
	delete(c.all, st.Idx)
}

func (c *stationCache) remove(idx model.StationIdx) {
	d := *c.dir.Load()
	ci := int(idx >> chunkBits)
	if ci >= len(d) {
		return
	}
	old := d[ci][idx&(1<<chunkBits-1)].Swap(nil)
	if old != nil {
		c.mu.Lock()
		c.unindex(old)
		c.mu.Unlock()
	}
}

func (c *stationCache) lookup(m func(*stationCache) (model.StationIdx, bool)) *Station {
	c.mu.RLock()
	idx, ok := m(c)
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	return c.load(idx)
}

// each calls fn for every cached station (unordered).
func (c *stationCache) each(fn func(*Station)) {
	c.mu.RLock()
	ids := make([]model.StationIdx, 0, len(c.all))
	for idx := range c.all {
		ids = append(ids, idx)
	}
	c.mu.RUnlock()
	for _, idx := range ids {
		if st := c.load(idx); st != nil {
			fn(st)
		}
	}
}

func (c *stationCache) count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.all)
}

// ---------------------------------------------------------------- SQL

const stationCols = `s.idx, s.sid, s.feeder_id, s.rx_no, s.kind, s.uuid, s.ident_key, s.name, s.clock_class,
 s.decl_lat, s.decl_lon, s.decl_alt, s.decl_alt_ref, s.gnss_lat, s.gnss_lon, s.gnss_hae,
 s.est_lat, s.est_lon, s.est_hae, s.est_sigma, s.eff_lat, s.eff_lon, s.eff_hae, s.eff_sigma, s.eff_source,
 s.loc_status, s.pin, s.show_name, s.public_stats, s.owner_id, s.trust, s.admission, s.anchor, s.flags,
 s.token_id, s.created, s.last_seen, s.deleting, s.owned_since,
 p.precision, p.effective, p.pub_lat, p.pub_lon`

const stationFrom = ` FROM stations s LEFT JOIN station_public p ON p.idx = s.idx`

type scanner interface{ Scan(dest ...any) error }

func scanStation(sc scanner) (*Station, error) {
	var (
		st                                   Station
		dLat, dLon, dAlt, gLat, gLon, gHae   sql.NullFloat64
		eLat, eLon, eHae, eSig               sql.NullFloat64
		fLat, fLon, fHae, fSig               sql.NullFloat64
		cc                                   int
		showName, pubStats, anchor, deleting int
		flags                                int64
		created, lastSeen, ownedSince        int64
		pPrec, pEff                          sql.NullString
		pLat, pLon                           sql.NullFloat64
	)
	if err := sc.Scan(&st.Idx, &st.SID, &st.FeederID, &st.RxNo, &st.Kind, &st.UUID, &st.IdentKey, &st.Name, &cc,
		&dLat, &dLon, &dAlt, &st.DeclaredAltRef, &gLat, &gLon, &gHae,
		&eLat, &eLon, &eHae, &eSig, &fLat, &fLon, &fHae, &fSig, &st.Effective.Source,
		&st.LocStatus, &st.Pin, &showName, &pubStats, &st.OwnerID, &st.Trust, &st.Admission, &anchor, &flags,
		&st.TokenID, &created, &lastSeen, &deleting, &ownedSince,
		&pPrec, &pEff, &pLat, &pLon); err != nil {
		return nil, err
	}
	st.ClockCls = model.ClockClass(cc)
	if dLat.Valid && dLon.Valid {
		st.Declared = Position{Lat: dLat.Float64, Lon: dLon.Float64, Alt: dAlt.Float64, Valid: true}
	}
	if gLat.Valid && gLon.Valid {
		st.GNSS = Position{Lat: gLat.Float64, Lon: gLon.Float64, Alt: gHae.Float64, Valid: true}
	}
	if eLat.Valid && eLon.Valid {
		st.Estimated = Position{Lat: eLat.Float64, Lon: eLon.Float64, Alt: eHae.Float64, Valid: true}
		st.EstSigmaM = eSig.Float64
	}
	if fLat.Valid && fLon.Valid {
		st.Effective.Lat, st.Effective.Lon, st.Effective.HAE = fLat.Float64, fLon.Float64, fHae.Float64
		st.Effective.SigmaM = fSig.Float64
		st.Located = true
	}
	st.ShowName, st.PublicStats, st.Anchor, st.Deleting = showName != 0, pubStats != 0, anchor != 0, deleting != 0
	st.Flags = Flags(flags)
	st.Created, st.LastSeen = fromS(created), fromS(lastSeen)
	st.OwnedSince = fromS(ownedSince)
	if pPrec.Valid && pLat.Valid {
		st.Public = PublicPoint{Precision: pPrec.String, Effective: pEff.String, Lat: pLat.Float64, Lon: pLon.Float64}
		st.HasPublic = true
	}
	return &st, nil
}

func (r *Registry) loadStations(ctx context.Context) error {
	r.cache.init()
	rows, err := r.wdb.QueryContext(ctx, `SELECT `+stationCols+stationFrom)
	if err != nil {
		return fmt.Errorf("registry: load stations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		st, err := scanStation(rows)
		if err != nil {
			return fmt.Errorf("registry: load stations: %w", err)
		}
		r.cache.put(st)
	}
	return rows.Err()
}

func nullPos(p Position) (lat, lon, alt any) {
	if !p.Valid {
		return nil, nil, nil
	}
	return p.Lat, p.Lon, p.Alt
}

// saveStation writes every mutable column of st.
func saveStation(ctx context.Context, tx *sql.Tx, st *Station) error {
	dLat, dLon, dAlt := nullPos(st.Declared)
	gLat, gLon, gHae := nullPos(st.GNSS)
	eLat, eLon, eHae := nullPos(st.Estimated)
	var eSig any
	if st.Estimated.Valid {
		eSig = st.EstSigmaM
	}
	var fLat, fLon, fHae, fSig any
	if st.Located {
		fLat, fLon, fHae, fSig = st.Effective.Lat, st.Effective.Lon, st.Effective.HAE, st.Effective.SigmaM
	}
	_, err := tx.ExecContext(ctx, `UPDATE stations SET kind=?, uuid=?, name=?, clock_class=?,
 decl_lat=?, decl_lon=?, decl_alt=?, decl_alt_ref=?, gnss_lat=?, gnss_lon=?, gnss_hae=?,
 est_lat=?, est_lon=?, est_hae=?, est_sigma=?, eff_lat=?, eff_lon=?, eff_hae=?, eff_sigma=?, eff_source=?,
 loc_status=?, pin=?, show_name=?, public_stats=?, owner_id=?, trust=?, admission=?, anchor=?, flags=?,
 token_id=?, last_seen=MAX(last_seen, ?), deleting=?, owned_since=? WHERE idx=?`,
		st.Kind, st.UUID, st.Name, int(st.ClockCls),
		dLat, dLon, dAlt, st.DeclaredAltRef, gLat, gLon, gHae,
		eLat, eLon, eHae, eSig, fLat, fLon, fHae, fSig, st.Effective.Source,
		st.LocStatus, st.Pin, b2i(st.ShowName), b2i(st.PublicStats), st.OwnerID, st.Trust, st.Admission,
		b2i(st.Anchor), int64(st.Flags), st.TokenID, unixS(st.LastSeen), b2i(st.Deleting), unixS(st.OwnedSince), st.Idx)
	if err != nil {
		return fmt.Errorf("registry: save station: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- lookups

func cp(st *Station) (Station, bool) {
	if st == nil {
		return Station{}, false
	}
	return *st, true
}

// visible returns st unless it is nil or being deleted.
func visible(st *Station) *Station {
	if st == nil || st.Deleting {
		return nil
	}
	return st
}

// Station returns a station by index (also while its deletion is queued).
func (r *Registry) Station(idx model.StationIdx) (Station, bool) {
	st, ok := cp(r.cache.load(idx))
	if ok {
		r.mergeSeen(&st)
	}
	return st, ok
}

// BySID returns a station by its public ID (case-insensitive).
func (r *Registry) BySID(sid string) (Station, bool) {
	sid, _ = model.CanonicalID(sid)
	st, ok := cp(visible(r.cache.lookup(func(c *stationCache) (model.StationIdx, bool) {
		i, ok := c.bySID[sid]
		return i, ok
	})))
	if ok {
		r.mergeSeen(&st)
	}
	return st, ok
}

// ByFeeder returns receiver rxNo of a native feeder.
func (r *Registry) ByFeeder(feederID string, rxNo int) (Station, bool) {
	return cp(visible(r.cache.lookup(func(c *stationCache) (model.StationIdx, bool) {
		i, ok := c.byFeeder[feederRx{feederID, rxNo}]
		return i, ok
	})))
}

// ByUUID returns the compat station with a (normalised) UUID.
func (r *Registry) ByUUID(uuid string) (Station, bool) {
	u, ok := model.NormalizeUUID(uuid)
	if !ok {
		u = uuid // sub-station keys "uuid/rid" are stored verbatim
	}
	return cp(visible(r.cache.lookup(func(c *stationCache) (model.StationIdx, bool) {
		i, ok := c.byUUID[u]
		return i, ok
	})))
}

// ByIdentKey returns an unidentified station by its UnidentifiedKey.
func (r *Registry) ByIdentKey(key string) (Station, bool) {
	return cp(visible(r.cache.lookup(func(c *stationCache) (model.StationIdx, bool) {
		i, ok := c.byIdent[key]
		return i, ok
	})))
}

// Filter selects stations in List. Zero fields do not filter.
type Filter struct {
	OwnerID   int64
	Admission string
	Kind      string
	// Unclaimed selects stations without an owner.
	Unclaimed bool
	// Query matches the SID, name, feeder ID or UUID (case-insensitive
	// substring).
	Query string
	// After/Limit page through the result ordered by idx (Limit 0 = all).
	After model.StationIdx
	Limit int
}

// List returns the stations matching f, ordered by idx. It is served from
// memory; stations being deleted are skipped.
func (r *Registry) List(f Filter) []Station {
	q := strings.ToLower(strings.TrimSpace(f.Query))
	var out []Station
	r.cache.each(func(st *Station) {
		switch {
		case st.Deleting, st.Idx <= f.After,
			f.OwnerID != 0 && st.OwnerID != f.OwnerID,
			f.Unclaimed && st.OwnerID != 0,
			f.Admission != "" && st.Admission != f.Admission,
			f.Kind != "" && st.Kind != f.Kind:
			return
		}
		if q != "" && !strings.Contains(st.SID, q) && !strings.Contains(strings.ToLower(st.Name), q) &&
			!strings.Contains(st.FeederID, q) && !strings.Contains(st.UUID, q) {
			return
		}
		out = append(out, *st)
	})
	slices.SortFunc(out, func(a, b Station) int { return int(a.Idx) - int(b.Idx) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	for i := range out {
		r.mergeSeen(&out[i])
	}
	return out
}

// Count returns the number of stations per admission state (stations being
// deleted are not counted).
func (r *Registry) Count() map[string]int {
	m := map[string]int{}
	r.cache.each(func(st *Station) {
		if !st.Deleting {
			m[st.Admission]++
		}
	})
	return m
}

// ---------------------------------------------------------------- resolver

// Resolver returns the model.StationResolver view of the registry. Info and
// Geo are lock-free and allocation-free; stations being deleted resolve as
// absent.
func (r *Registry) Resolver() model.StationResolver { return resolver{r} }

type resolver struct{ r *Registry }

func (v resolver) Info(idx model.StationIdx) (model.StationMeta, bool) {
	st := visible(v.r.cache.load(idx))
	if st == nil {
		return model.StationMeta{}, false
	}
	return st.Meta(), true
}

func (v resolver) Geo(idx model.StationIdx) (model.StationGeo, bool) {
	st := visible(v.r.cache.load(idx))
	if st == nil || !st.Located {
		return model.StationGeo{}, false
	}
	return st.Effective, true
}

// PublicPoint returns the stored public pin of an approved station whose
// precision is not hidden (DESIGN §8.3). It is the only station position
// public surfaces may use.
func (r *Registry) PublicPoint(idx model.StationIdx) (PublicPoint, bool) {
	st := visible(r.cache.load(idx))
	if st == nil || !st.HasPublic || st.Admission != model.AdmApproved || st.Pin == model.PinHidden {
		return PublicPoint{}, false
	}
	return st.Public, true
}

// ---------------------------------------------------------------- updates

// Update applies fn to a copy of the station and stores the result. fn
// may change any field except the identity (Idx, SID, FeederID, RxNo,
// IdentKey, Created); the effective location, pin precision and admission
// state drive the public pin, which Update recomputes (including the
// rural guard of neighbouring stations). It is the general form behind the
// Set* helpers.
func (r *Registry) Update(ctx context.Context, idx model.StationIdx, fn func(*Station) error) (Station, error) {
	var out Station
	var changed []*Station
	err := r.write(ctx, func(tx *sql.Tx) error {
		cur := r.cache.load(idx)
		if cur == nil || cur.Deleting {
			return ErrStationNotFound
		}
		next := *cur
		if err := fn(&next); err != nil {
			return err
		}
		next.Idx, next.SID, next.FeederID, next.RxNo = cur.Idx, cur.SID, cur.FeederID, cur.RxNo
		next.IdentKey, next.Created, next.Deleting = cur.IdentKey, cur.Created, cur.Deleting
		next.Public, next.HasPublic = cur.Public, cur.HasPublic
		if err := validStation(&next); err != nil {
			return err
		}
		if err := saveStation(ctx, tx, &next); err != nil {
			return err
		}
		var err error
		changed, err = r.refreshPublic(ctx, tx, cur, &next)
		if err != nil {
			return err
		}
		out = next
		for _, c := range changed {
			if c.Idx == idx {
				out = *c
			}
		}
		return nil
	}, func() {
		for _, c := range changed {
			r.cache.put(c)
		}
	})
	if err == nil {
		r.mergeSeen(&out)
	}
	return out, err
}

func validStation(st *Station) error {
	if !model.ValidPin(st.Pin) {
		return fmt.Errorf("%w: pin %q", ErrInvalid, st.Pin)
	}
	switch st.Admission {
	case model.AdmPending, model.AdmApproved, model.AdmDenied, model.AdmRevoked:
	default:
		return fmt.Errorf("%w: admission %q", ErrInvalid, st.Admission)
	}
	if !st.ClockCls.Valid() {
		return fmt.Errorf("%w: clock class %d", ErrInvalid, st.ClockCls)
	}
	if utf8.RuneCountInString(st.Name) > 64 || !utf8.ValidString(st.Name) {
		return fmt.Errorf("%w: name must be valid UTF-8, at most 64 characters", ErrInvalid)
	}
	for _, p := range []Position{st.Declared, st.GNSS, st.Estimated} {
		if p.Valid && !validLatLon(p.Lat, p.Lon) {
			return fmt.Errorf("%w: position %.5f,%.5f", ErrInvalid, p.Lat, p.Lon)
		}
	}
	if st.Located && !validLatLon(st.Effective.Lat, st.Effective.Lon) {
		return fmt.Errorf("%w: effective position", ErrInvalid)
	}
	return nil
}

func validLatLon(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 && lat == lat && lon == lon
}

// SetName sets the display name (trimmed; at most 64 characters).
func (r *Registry) SetName(ctx context.Context, idx model.StationIdx, name string) (Station, error) {
	return r.Update(ctx, idx, func(s *Station) error { s.Name = strings.TrimSpace(name); return nil })
}

// SetPrivacy sets the owner's public opt-ins.
func (r *Registry) SetPrivacy(ctx context.Context, idx model.StationIdx, pin string, showName, publicStats bool) (Station, error) {
	return r.Update(ctx, idx, func(s *Station) error {
		s.Pin, s.ShowName, s.PublicStats = pin, showName, publicStats
		return nil
	})
}

// SetClockClass records the station's current clock class.
func (r *Registry) SetClockClass(ctx context.Context, idx model.StationIdx, c model.ClockClass) (Station, error) {
	return r.Update(ctx, idx, func(s *Station) error { s.ClockCls = c; return nil })
}

// SetAnchor records whether the station is an accepted MLAT time anchor.
func (r *Registry) SetAnchor(ctx context.Context, idx model.StationIdx, anchor bool) (Station, error) {
	return r.Update(ctx, idx, func(s *Station) error { s.Anchor = anchor; return nil })
}

// SetNoAnchor sets or clears FlagNoAnchor (admin: never a time anchor).
func (r *Registry) SetNoAnchor(ctx context.Context, idx model.StationIdx, never bool) (Station, error) {
	return r.Update(ctx, idx, func(s *Station) error {
		if never {
			s.Flags |= FlagNoAnchor
		} else {
			s.Flags &^= FlagNoAnchor
		}
		return nil
	})
}

// AddTrust adds delta to the station's trust (karma), clamped to ±1000.
func (r *Registry) AddTrust(ctx context.Context, idx model.StationIdx, delta int) (Station, error) {
	return r.Update(ctx, idx, func(s *Station) error { s.Trust = min(max(s.Trust+delta, -1000), 1000); return nil })
}

// SetDeclared stores the owner-declared position (altRef model.AltRef*).
// valid=false clears it. The effective position is not changed: the
// self-location policy (Locate) decides that.
func (r *Registry) SetDeclared(ctx context.Context, idx model.StationIdx, p Position, altRef string) (Station, error) {
	return r.Update(ctx, idx, func(s *Station) error {
		switch altRef {
		case model.AltRefMSL, model.AltRefHAE, model.AltRefAmbiguous, "":
		default:
			return fmt.Errorf("%w: altRef %q", ErrInvalid, altRef)
		}
		s.Declared, s.DeclaredAltRef = p, altRef
		if !p.Valid {
			s.DeclaredAltRef = ""
		}
		return nil
	})
}

// SetGNSS stores the receiver's own GNSS position (HAE).
func (r *Registry) SetGNSS(ctx context.Context, idx model.StationIdx, p Position) (Station, error) {
	return r.Update(ctx, idx, func(s *Station) error { s.GNSS = p; return nil })
}

// SetEstimated stores the latest survey result with its 1-sigma
// horizontal uncertainty.
// A feeder position held pending a survey (position.go) is applied when
// the estimate confirms it.
func (r *Registry) SetEstimated(ctx context.Context, idx model.StationIdx, p Position, sigmaM float64) (Station, error) {
	st, err := r.Update(ctx, idx, func(s *Station) error { s.Estimated, s.EstSigmaM = p, sigmaM; return nil })
	if err != nil || !p.Valid {
		return st, err
	}
	if err := r.releaseBySurvey(ctx, st); err != nil {
		return st, err
	}
	if up, ok := r.Station(idx); ok {
		st = up
	}
	return st, nil
}

// SetEffective sets (located=true) or clears the effective position and
// the location status.
func (r *Registry) SetEffective(ctx context.Context, idx model.StationIdx, g model.StationGeo, located bool, status string) (Station, error) {
	return r.Update(ctx, idx, func(s *Station) error {
		s.Located = located
		s.Effective = g
		if !located {
			s.Effective = model.StationGeo{}
		}
		if status != "" {
			s.LocStatus = status
		}
		return nil
	})
}

// ---------------------------------------------------------------- last seen

// Seen records that a station was active at t. It only updates memory;
// FlushSeen (called by Run every SeenFlushEvery and by Close) writes the
// values in one transaction.
func (r *Registry) Seen(idx model.StationIdx, t time.Time) {
	s := t.Unix()
	r.seenMu.Lock()
	if s > r.seen[idx] {
		r.seen[idx] = s
	}
	r.seenMu.Unlock()
}

func (r *Registry) mergeSeen(st *Station) {
	r.seenMu.Lock()
	s, ok := r.seen[st.Idx]
	r.seenMu.Unlock()
	if ok && s > st.LastSeen.Unix() {
		st.LastSeen = fromS(s)
	}
}

// FlushSeen writes pending last-seen times.
func (r *Registry) FlushSeen(ctx context.Context) error { return r.flushSeen(ctx) }

func (r *Registry) flushSeen(ctx context.Context) error {
	r.seenMu.Lock()
	pending := r.seen
	r.seen = map[model.StationIdx]int64{}
	r.seenMu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	var updated []*Station
	err := r.write(ctx, func(tx *sql.Tx) error {
		updated = updated[:0]
		stmt, err := tx.PrepareContext(ctx, `UPDATE stations SET last_seen=? WHERE idx=? AND last_seen<?`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		fstmt, err := tx.PrepareContext(ctx, `UPDATE feeders SET last_seen=? WHERE feeder_id=? AND last_seen<?`)
		if err != nil {
			return err
		}
		defer fstmt.Close()
		for idx, s := range pending {
			if _, err := stmt.ExecContext(ctx, s, idx, s); err != nil {
				return err
			}
			if cur := r.cache.load(idx); cur != nil {
				if cur.FeederID != "" {
					if _, err := fstmt.ExecContext(ctx, s, cur.FeederID, s); err != nil {
						return err
					}
				}
				if cur.LastSeen.Unix() < s {
					n := *cur
					n.LastSeen = fromS(s)
					updated = append(updated, &n)
				}
			}
		}
		return nil
	}, func() {
		for _, s := range updated {
			if cur := r.cache.load(s.Idx); cur != nil {
				n := *cur
				n.LastSeen = s.LastSeen
				r.cache.put(&n)
			}
		}
	})
	if err != nil {
		// Put the values back so the next flush retries them.
		r.seenMu.Lock()
		for idx, s := range pending {
			if s > r.seen[idx] {
				r.seen[idx] = s
			}
		}
		r.seenMu.Unlock()
	}
	return err
}
