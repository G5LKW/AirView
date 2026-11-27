package server

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"airview/internal/engine"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/privacy"
	"airview/internal/privacy/publicdto"
	"airview/internal/store"
	"airview/internal/tracker"
	"airview/internal/trajectory"
)

// Per-viewer projections of the legacy live routes (/api/status,
// /api/config, /api/aircraft, /api/aircraft/{icao}, /api/trails,
// /api/notices, the live stream) in aggregator mode (DESIGN §8.2):
//
//   - admin: the full answer (the tracker's admin projection);
//   - owner: the tracker's owner projection (own stations only) with every
//     receiver field of other stations removed;
//   - public: the publicdto shapes.
//
// In standalone mode every route answers exactly as before.

// nonICAO marks non-ICAO addresses in tracker keys.
const nonICAO = modes.NonICAOFlag

// netRestricted reports whether r is in aggregator mode and below admin
// scope: the answer must be projected.
func (s *Server) netRestricted(r *http.Request) bool {
	return s.aggregatorMode() && !s.viewerOf(r).Scope.Allows(model.ScopeAdmin)
}

// netPublic reports whether r is a public-scope request in aggregator mode.
func (s *Server) netPublic(r *http.Request) bool {
	return s.aggregatorMode() && s.viewerOf(r).Scope == model.ScopePublic
}

// ownStations returns the station indices of an owner.
func (s *Server) ownStations(ownerID int64) []model.StationIdx {
	reg := s.registry()
	if reg == nil || ownerID == 0 {
		return nil
	}
	sts := reg.OwnerStations(ownerID)
	out := make([]model.StationIdx, 0, len(sts))
	for _, st := range sts {
		out = append(out, st.Idx)
	}
	return out
}

// trackerView builds the tracker projection for a viewer. sel is the
// selected station (hasSel false = none: the host station for an admin).
func (s *Server) trackerView(v model.Viewer, sel model.StationIdx, hasSel bool) tracker.View {
	tv := tracker.View{Scope: v.Scope}
	switch {
	case hasSel:
		tv.Station = sel
	case v.Scope == model.ScopeAdmin:
		tv.Station = model.HostStation
	}
	if v.Scope == model.ScopeOwner {
		tv.Own = s.ownStations(v.OwnerID)
	}
	return tv
}

// viewSnapshot returns the live picture for r: the full snapshot in
// standalone mode, else the viewer's projection.
func (s *Server) viewSnapshot(r *http.Request) any {
	if !s.aggregatorMode() {
		return s.eng.Tracker().Snapshot()
	}
	st, ok := selectedStation(r)
	return s.snapshotFor(s.viewerOf(r), st.Idx, ok)
}

// snapshotFor returns the live picture projected for viewer v in
// aggregator mode (see viewSnapshot).
func (s *Server) snapshotFor(v model.Viewer, sel model.StationIdx, hasSel bool) any {
	tr := s.eng.Tracker()
	switch v.Scope {
	case model.ScopeAdmin:
		snap := tr.SnapshotView(s.trackerView(v, sel, hasSel))
		s.appendModeAC(&snap)
		return snap
	case model.ScopeOwner:
		snap := tr.SnapshotView(s.trackerView(v, sel, hasSel))
		s.appendModeAC(&snap)
		if !tr.Network() {
			// The tracker is not in network mode (no projection happened):
			// its receiver fields are the host station's.
			snap = privacy.StripSnapshot(snap, false)
		}
		s.guardOwnerSnapshot(&snap)
		return snap
	}
	p := s.publicSnapshot()
	return &p
}

// pubCache shares the public picture between the public routes, the
// public live stream and /data/aircraft.json: built at most once per
// second.
type pubCache struct {
	mu   sync.Mutex
	at   time.Time
	snap publicdto.Snapshot
	msgs uint64
	gen  uint64 // incremented with every rebuilt picture
	// frame is the encoded "aircraft" event of generation frameGen: every
	// full public stream gets the same bytes (SO-1).
	frame    []byte
	frameGen uint64
}

const pubSnapshotTTL = 900 * time.Millisecond

// publicSnapshot returns the public live picture (DESIGN §8.2: merged map,
// MLAT tracks with grade; no receiver data). The island guard (§8.6) is
// applied when aggregator.islandGuard is on.
func (s *Server) publicSnapshot() publicdto.Snapshot {
	snap, _ := s.publicSnapshotMsgs()
	return snap
}

func (s *Server) publicSnapshotMsgs() (publicdto.Snapshot, uint64) {
	snap, msgs, _ := s.publicSnapshotGen()
	return snap, msgs
}

// publicSnapshotGen is publicSnapshotMsgs with the picture's generation
// (equal generations = the same picture).
func (s *Server) publicSnapshotGen() (publicdto.Snapshot, uint64, uint64) {
	c := &s.pub
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < pubSnapshotTTL && c.snap.Aircraft != nil {
		return c.snap, c.msgs, c.gen
	}
	tr := s.eng.Tracker()
	raw := tr.SnapshotView(tracker.View{Scope: model.ScopePublic})
	s.appendModeAC(&raw)
	c.snap = publicdto.FromSnapshot(&raw, s.islandGuard(), privacy.IslandGridDeg)
	if n := s.eng.Federation(); n != nil && !s.eng.EffectiveConfig().Aggregator.Federation.HideRemote {
		n.Overlay(&c.snap, nil)
	}
	c.msgs = tr.Counters().Frames
	c.at = time.Now()
	c.gen++
	return c.snap, c.msgs, c.gen
}

// publicFrame returns the full public "aircraft" event and its generation,
// encoded once per generation and shared by every full public stream.
func (s *Server) publicFrame() ([]byte, uint64) {
	snap, _, gen := s.publicSnapshotGen()
	c := &s.pub
	c.mu.Lock()
	if c.frameGen == gen && c.frame != nil {
		f := c.frame
		c.mu.Unlock()
		return f, gen
	}
	c.mu.Unlock()
	b, err := marshal(&snap)
	if err != nil {
		return nil, gen
	}
	f := sseFrame("aircraft", b)
	c.mu.Lock()
	if gen >= c.frameGen {
		c.frame, c.frameGen = f, gen
	}
	c.mu.Unlock()
	return f, gen
}

// islandGuard returns the island-guard predicate, nil when the guard is off
// or there is no registry to tell hidden stations apart. "Low" is measured
// above the ground (DESIGN §8.6: below 1000 ft AGL): see islandGroundFt.
func (s *Server) islandGuard() publicdto.Guard {
	if s.testGuard != nil {
		return s.testGuard
	}
	if !s.aggregatorMode() || !s.eng.EffectiveConfig().Aggregator.IslandGuard {
		return nil
	}
	reg := s.registry()
	if reg == nil {
		return nil
	}
	res := reg.Resolver()
	tr := s.eng.Tracker()
	return func(a *model.Aircraft) bool {
		if a.Lat == nil || a.Lon == nil {
			return false
		}
		// Cheap pre-check: nothing above the highest terrain on earth is low.
		if !privacy.IslandLowAGL(a.OnGround, a.AltBaro, a.AltGeom, privacy.IslandMaxGroundFt) {
			return false
		}
		key, err := parseICAOParam(a.ICAO)
		if err != nil {
			// PRIV-A6: Mode A/C targets ("#xxxxxx", AddrType "modeac")
			// carry no anchor list, so it cannot be told whether only
			// hidden stations solved them: a low one is always coarsened.
			// Anything else without a parseable address is coarsened when
			// low too (fail closed).
			return privacy.IslandLowAGL(a.OnGround, a.AltBaro, a.AltGeom, s.islandGroundFt(*a.Lat, *a.Lon, true))
		}
		heard := tr.HeardBy(key)
		if !privacy.OnlyHidden(heard, res) {
			return false
		}
		return privacy.IslandLowAGL(a.OnGround, a.AltBaro, a.AltGeom, s.islandGroundFt(*a.Lat, *a.Lon, true))
	}
}

// ftPerM converts metres to feet.
const ftPerM = 3.28084

// islandGroundFt estimates the ground elevation (feet AMSL) for the island
// guard from the elevation model at (lat, lon) only (cached tiles; never
// fetched on this path). With unknown terrain it is
// privacy.IslandUnknownGroundFt, so the guard errs on the side of
// coarsening. PRIV-A5: station antenna heights are never used here. The
// verdict is public (the "coarse" flag), and the altitude at which it flips
// for a climbing aircraft would give away a hidden station's antenna height.
func (s *Server) islandGroundFt(lat, lon float64, hasPos bool) float64 {
	if hasPos && s.eng != nil {
		if ts := s.eng.Terrain(); ts != nil {
			if p := ts.Provider(); p != nil {
				if e, ok := p.ElevationM(lat, lon); ok {
					return e * ftPerM
				}
			}
		}
	}
	return privacy.IslandUnknownGroundFt
}

// guardOwnerSnapshot applies the island guard to an owner's live picture
// (DESIGN §8.6): an owner keeps exact positions only for aircraft one of
// their own stations heard (Mine); every other guarded aircraft is shown on
// the public grid, exactly as on the public map. Self-registered owners
// must not see more than the public about other people's hidden stations.
func (s *Server) guardOwnerSnapshot(snap *model.LiveSnapshot) {
	guard := s.islandGuard()
	if guard == nil {
		return
	}
	for i := range snap.Aircraft {
		a := &snap.Aircraft[i]
		if a.Mine || !guard(a) {
			continue
		}
		coarsenAircraft(a)
	}
}

// coarsenAircraft rounds every position field of a (a copy: the pointers
// are replaced, never written through) to the island-guard grid and drops
// the fields from which the exact position could be recomputed.
func coarsenAircraft(a *model.Aircraft) {
	r := func(p *float64) *float64 {
		if p == nil {
			return nil
		}
		v := privacy.RoundGrid(*p)
		return &v
	}
	a.Lat, a.Lon = r(a.Lat), r(a.Lon)
	a.LastRealLat, a.LastRealLon = r(a.LastRealLat), r(a.LastRealLon)
	a.DistKm, a.Bearing, a.Elevation, a.SlantKm = nil, nil, nil, nil
	a.CPAKm, a.CPAInS = nil, nil
	a.TerrainVisible, a.LOSMarginDeg = nil, nil
}

// publicStatus is the status of a public viewer.
func (s *Server) publicStatus() publicdto.Status {
	st := s.eng.Status()
	cfg := s.eng.EffectiveConfig()
	out := publicdto.Status{
		App: st.App, Version: s.opts.Version, Mode: st.Mode, Now: st.Now, Started: st.Started,
		TileCache: st.TileCache, Counts: publicdto.FromCounts(st.Counts), AdminLocked: true,
	}
	if s.aggregatorMode() {
		out.Network = cfg.Aggregator.Name
		out.PublicMap, out.PublicStats, out.TarJSON = cfg.Aggregator.PublicMap, cfg.Aggregator.PublicStats, cfg.Aggregator.TarJSON
	}
	if st.TileCache {
		out.TileCacheHosts = s.tiles.hosts()
	}
	return out
}

// viewStatus is GET /api/status: the full status in standalone mode and for
// the admin; below admin in aggregator mode the public status (an owner's
// own stations are on /api/me).
func (s *Server) viewStatus(r *http.Request) any {
	if !s.netRestricted(r) {
		return s.status(s.isAdmin(r))
	}
	return s.publicStatus()
}

// publicConfig is GET /api/config for non-admin viewers in aggregator
// mode: the display settings only (no receiver, antennas, feeds, ...).
type publicConfig struct {
	Version    int              `json:"version"`
	Mode       string           `json:"mode"`
	UI         model.UIConfig   `json:"ui"`
	Prediction publicPrediction `json:"prediction"`
	Network    publicNetwork    `json:"aggregator"`
}

type publicPrediction struct {
	Enabled  bool `json:"enabled"`
	CoastSec int  `json:"coastSec"`
}

type publicNetwork struct {
	Name        string `json:"name"`
	PublicMap   bool   `json:"publicMap"`
	PublicStats bool   `json:"publicStats"`
	TarJSON     bool   `json:"tarJson"`
}

func (s *Server) publicConfig() publicConfig {
	cfg := s.eng.Config().Get()
	return publicConfig{
		Version: cfg.Version, Mode: cfg.Mode, UI: cfg.UI,
		Prediction: publicPrediction{Enabled: cfg.Prediction.Enabled, CoastSec: cfg.Prediction.CoastSec},
		Network: publicNetwork{Name: cfg.Aggregator.Name, PublicMap: cfg.Aggregator.PublicMap,
			PublicStats: cfg.Aggregator.PublicStats, TarJSON: cfg.Aggregator.TarJSON},
	}
}

// publicAircraftDetail is GET /api/aircraft/{icao} (and
// /api/public/aircraft/{icao}) below admin scope in aggregator mode: the
// live state, the live trail without signal levels, and reference data.
// Stored totals and flights (with distances) are not included.
type publicAircraftDetail struct {
	Aircraft *publicdto.Aircraft    `json:"aircraft"`
	Trail    []publicdto.TrailPoint `json:"trail"`
	Info     aircraftInfo           `json:"info"`
	ACDB     *store.ACDBEntry       `json:"acdb"`
}

func (s *Server) publicDetail(r *http.Request, key uint32) (*publicAircraftDetail, error) {
	out := &publicAircraftDetail{Trail: []publicdto.TrailPoint{}}
	tr := s.eng.Tracker()
	var live *model.Aircraft
	if ac, ok := tr.AircraftView(key, tracker.View{Scope: model.ScopePublic}); ok {
		live = &ac
		p := publicdto.FromAircraft(&ac)
		guard := s.islandGuard()
		coarse := guard != nil && guard(&ac)
		if coarse {
			p.Coarsen(privacy.IslandGridDeg)
		}
		out.Aircraft = &p
		pts := tr.Trail(key)
		if qBool(r, "smooth", false) && len(pts) > 2 {
			pts = trajectory.SmoothTrail(pts, trajectory.SmoothOptions{})
		}
		ground := s.trailGroundFt(pts)
		for _, pt := range pts {
			tp := publicdto.FromTrailPoint(pt)
			if coarse || s.coarseTrail(pt, ground) {
				tp.Lat, tp.Lon = privacy.RoundGrid(tp.Lat), privacy.RoundGrid(tp.Lon)
			}
			out.Trail = append(out.Trail, tp)
		}
	}
	if e, ok, err := s.eng.Store().ACDBLookup(r.Context(), key&^nonICAO); err != nil {
		return nil, err
	} else if ok && key&nonICAO == 0 {
		out.ACDB = &e
	}
	out.Info = buildInfo(key, live, nil, out.ACDB)
	s.attributeACDB(r.Context(), &out.Info)
	return out, nil
}

// coarseTrail reports whether a public trail point is coarsened by the
// island guard: with the guard on, every point less than IslandAltFt above
// the ground (or of unknown altitude) is shown on the 0.05° grid, because
// which stations heard an old point is not kept. groundFt is the ground
// estimate for the trail (trailGroundFt).
func (s *Server) coarseTrail(p model.TrailPoint, groundFt float64) bool {
	if !s.islandGuardOn() {
		return false
	}
	return privacy.AltLowAGL(p.Alt, groundFt)
}

// islandGuardOn reports whether the island guard is configured on.
func (s *Server) islandGuardOn() bool {
	return s.aggregatorMode() && s.eng.EffectiveConfig().Aggregator.IslandGuard
}

// trailGroundFt is the ground estimate applied to every point of one
// aircraft's trail: the elevation model at the last point (islandGroundFt;
// never station heights, PRIV-A5).
func (s *Server) trailGroundFt(pts []model.TrailPoint) float64 {
	if !s.islandGuardOn() {
		return 0
	}
	lat, lon, has := 0.0, 0.0, len(pts) > 0
	if has {
		lat, lon = pts[len(pts)-1].Lat, pts[len(pts)-1].Lon
	}
	return s.islandGroundFt(lat, lon, has)
}

// Public trail bounds (SO-7): the full in-memory trails of every aircraft
// grew to megabytes per anonymous request. Public answers cover at most
// publicTrailMaxMinutes and carry at most publicTrailMaxPoints points per
// aircraft (evenly thinned, first and last kept); ?bbox= limits them to
// the aircraft in view.
const (
	publicTrailMaxMinutes = 10
	publicTrailMaxPoints  = 120
)

// geoBox is a ?bbox=south,west,north,east filter (west > east crosses the
// antimeridian).
type geoBox struct{ s, w, n, e float64 }

func (b *geoBox) contains(lat, lon float64) bool {
	if b == nil {
		return true
	}
	if lat < b.s || lat > b.n {
		return false
	}
	if b.w <= b.e {
		return lon >= b.w && lon <= b.e
	}
	return lon >= b.w || lon <= b.e
}

// parseTrailBox reads ?bbox= (nil when absent).
func parseTrailBox(r *http.Request) (*geoBox, error) {
	v := strings.TrimSpace(r.URL.Query().Get("bbox"))
	if v == "" {
		return nil, nil
	}
	parts := strings.Split(v, ",")
	if len(parts) != 4 {
		return nil, badRequest("bbox must be south,west,north,east")
	}
	var f [4]float64
	for i, p := range parts {
		x, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, badRequest("bbox must be south,west,north,east")
		}
		f[i] = x
	}
	b := &geoBox{s: f[0], w: f[1], n: f[2], e: f[3]}
	if b.s < -90 || b.n > 90 || b.s > b.n || b.w < -180 || b.w > 180 || b.e < -180 || b.e > 180 {
		return nil, badRequest("bbox out of range")
	}
	return b, nil
}

// thinTrail keeps at most max points, evenly spaced, first and last kept.
func thinTrail(pts []model.TrailPoint, max int) []model.TrailPoint {
	if len(pts) <= max || max < 2 {
		return pts
	}
	out := make([]model.TrailPoint, 0, max)
	step := float64(len(pts)-1) / float64(max-1)
	for i := 0; i < max; i++ {
		out = append(out, pts[int(math.Round(float64(i)*step))])
	}
	return out
}

// publicTrails is GET /api/trails below admin scope and
// /api/public/trails.
func (s *Server) publicTrails(minutes int, bb *geoBox) publicdto.Trails {
	minutes = min(minutes, publicTrailMaxMinutes)
	src := s.eng.Tracker().Trails(minutes)
	out := publicdto.Trails{Now: time.Now().UnixMilli(), Minutes: minutes, Trails: make(map[string][]publicdto.TrailPoint, len(src))}
	guard := s.islandGuardOn()
	for k, pts := range src {
		if len(pts) == 0 || !bb.contains(pts[len(pts)-1].Lat, pts[len(pts)-1].Lon) {
			continue
		}
		pts = thinTrail(pts, publicTrailMaxPoints)
		ground := 0.0
		if guard {
			ground = s.trailGroundFt(pts)
		}
		tp := make([]publicdto.TrailPoint, len(pts))
		for i, p := range pts {
			tp[i] = publicdto.FromTrailPoint(p)
			if guard && privacy.AltLowAGL(p.Alt, ground) {
				tp[i].Lat, tp[i].Lon = privacy.RoundGrid(tp[i].Lat), privacy.RoundGrid(tp[i].Lon)
			}
		}
		out.Trails[k] = tp
	}
	return out
}

// publicNotices filters notices for a public or owner viewer.
func publicNotices(n []engine.Notice) []publicdto.Notice {
	out := []publicdto.Notice{}
	for _, x := range n {
		if publicdto.PublicNoticeKind(x.Kind) {
			out = append(out, publicdto.Notice{Level: x.Level, Text: x.Text, T: x.T, Kind: x.Kind, ICAO: x.ICAO})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}
