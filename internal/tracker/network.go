package tracker

import (
	"math"
	"slices"
	"sort"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/ntp"
)

// Aggregator mode (docs/aggregator/DESIGN.md §6.2-6.4).
//
// In aggregator mode the grouper in internal/ingest de-duplicates the copies
// of a transmission heard by many stations. The first copy goes to
// Tracker.Handle, which applies it to the merged live picture, credits the
// hearing station's lane and returns a Result; later copies go straight to
// Lanes.Credit with that Result and never take the tracker mutex.
//
// Differences from standalone mode, all confined to aggregator mode:
//   - no geometry is stored for aircraft (distance, bearing, elevation,
//     slant range, record distances, flight min/max distance): there is no
//     single receiver. SnapshotView / AircraftView compute it for the
//     viewer's selected station when the picture is read;
//   - the range gate accepts a position within MaxRangeKm of any located
//     station that heard the aircraft (the hearing station first, then the
//     stations in its heardBy set); with no located station at all it is
//     open (the speed / pairing filters still apply);
//   - the surface CPR reference is the hearing station's position;
//   - each aircraft keeps heardBy: the stations of the last 60 s, at most
//     16, never part of the DTO;
//   - the tracker's own statistics keep only the combined slot (station-free
//     counters); everything per station lives in the lanes;
//   - flight closes are handed to the lanes, which emit flight_station rows.

// netState is the aggregator-mode state of the tracker. Guarded by mu.
type netState struct {
	res   model.StationResolver
	lanes *Lanes
	geo   map[model.StationIdx]*netGeo
}

// netGeo caches the geometry station of a network station.
type netGeo struct {
	st   *geo.Station // nil = not located
	g    model.StationGeo
	atMs int64
}

// netGeoRefreshMs is how long a cached station position is used before the
// resolver is asked again; netGeoMax bounds the cache.
const (
	netGeoRefreshMs = 30_000
	netGeoMax       = 1 << 16
)

// netSide is the aggregator-only part of one aircraft of a shared snapshot:
// the stations that heard it (kept off the DTO).
type netSide struct {
	n  uint8
	st [heardMax]model.StationIdx
}

func (s *netSide) list() []model.StationIdx { return s.st[:s.n] }

// SetNetwork switches the tracker to aggregator mode (res != nil or lanes
// != nil) or back to standalone mode (both nil). res resolves station
// positions for the range gate, the surface CPR reference and the per-viewer
// projections; lanes receives every frame's station credit and the flight
// closes. Call it at start-up, before frames arrive: switching flushes the
// open statistics and rebuilds the statistics slots.
func (t *Tracker) SetNetwork(res model.StationResolver, lanes *Lanes) {
	if s := t.sh.Load(); s != nil {
		s.setNetwork(t, res, lanes)
		return
	}
	t.setNetwork(res, lanes)
	// Aggregator mode on a fresh tracker: split it into ICAO shards when
	// configured (shard.go).
	t.mu.Lock()
	n := 0
	if t.net != nil && t.nowMs == 0 && len(t.ac) == 0 {
		n = t.shardCount()
	}
	t.mu.Unlock()
	if n > 1 {
		t.startShards(n, res, lanes)
	}
}

// setNetwork is SetNetwork of one (unsharded) tracker.
func (t *Tracker) setNetwork(res model.StationResolver, lanes *Lanes) {
	t.mu.Lock()
	if t.nowMs != 0 {
		t.flush(true)
	}
	if res == nil && lanes == nil {
		t.net = nil
	} else {
		t.net = &netState{res: res, lanes: lanes, geo: make(map[model.StationIdx]*netGeo)}
	}
	t.lanesP.Store(lanes)
	t.slots = nil
	t.applyConfig(t.cfg)
	for _, ac := range t.ac {
		ac.geoValid = false
	}
	t.snap, t.snapSide = nil, nil
	t.mu.Unlock()
}

// Network reports whether the tracker is in aggregator mode.
func (t *Tracker) Network() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.net != nil
}

// Lanes returns the lanes installed with SetNetwork (nil = none).
func (t *Tracker) Lanes() *Lanes { return t.lanesP.Load() }

// Handle applies the first copy of a transmission and returns what it
// learned (DESIGN §6.2). Unlike HandleFrame it does not de-duplicate: the
// caller (the grouper) has already decided that f is a new transmission.
// In aggregator mode the copy is also credited to f.Station's lane; later
// copies of the same transmission go to Lanes().Credit with the returned
// Result. A late (backfilled) frame only reaches the lanes and returns
// Status ResultLate.
func (t *Tracker) Handle(f model.RawFrame) (r Result) {
	if f.Late {
		if ls := t.lanesP.Load(); ls != nil {
			ls.Backfill(f)
		}
		r.Status = ResultLate
		return r
	}
	if s := t.sh.Load(); s != nil {
		return s.route(f.Data).Handle(f)
	}
	t.mu.Lock()
	ok := false
	defer t.unlockOnPanic(&ok)
	t0, sampled := t.holdStart()
	t.ingest(&f, &r, true)
	t.holdEnd(t0, sampled)
	ok = true
	var ls *Lanes
	if t.net != nil {
		ls = t.net.lanes
	}
	if !t.slowPending() {
		t.mu.Unlock()
	} else {
		t.finishSlow()
	}
	if ls != nil {
		ls.Credit(f.Station, f.Channel, f.Signal, &r)
	}
	return r
}

// netGeoOf returns the geometry station of st (nil = not located). Caller
// holds mu; t.net != nil.
func (t *Tracker) netGeoOf(st model.StationIdx, tMs int64) *geo.Station {
	n := t.net
	g := n.geo[st]
	if g != nil && tMs >= g.atMs && tMs-g.atMs < netGeoRefreshMs {
		return g.st
	}
	if g == nil {
		if len(n.geo) >= netGeoMax {
			clear(n.geo)
		}
		g = &netGeo{}
		n.geo[st] = g
	}
	g.atMs = tMs
	if n.res == nil {
		g.st = nil
		return nil
	}
	sg, ok := n.res.Geo(st)
	if !ok || !geo.ValidLatLon(sg.Lat, sg.Lon) {
		g.st = nil
		return nil
	}
	if g.st == nil || sg != g.g {
		g.g, g.st = sg, stationFromGeo(sg)
	}
	return g.st
}

// netInRange is the aggregator range gate: (lat, lon) must be within
// maxRangeKm of a located station that heard the aircraft - the station of
// the current frame first, then the aircraft's heardBy set. With no located
// station to check the gate is open. Caller holds mu; t.net != nil.
func (t *Tracker) netInRange(ac *aircraft, lat, lon float64, tMs int64) bool {
	checked := false
	if g := t.netGeoOf(t.cur, tMs); g != nil {
		if g.DistanceKm(lat, lon) <= t.maxRangeKm {
			return true
		}
		checked = true
	}
	var buf [heardMax]model.StationIdx
	for _, st := range ac.heard.list(tMs/1000, buf[:0]) {
		if st == t.cur {
			continue
		}
		if g := t.netGeoOf(st, tMs); g != nil {
			if g.DistanceKm(lat, lon) <= t.maxRangeKm {
				return true
			}
			checked = true
		}
	}
	return !checked
}

// netSurfaceRef returns the reference for a surface CPR decode in
// aggregator mode: the hearing station, else a located station of heardBy.
func (t *Tracker) netSurfaceRef(ac *aircraft, tMs int64) (lat, lon float64, ok bool) {
	if g := t.netGeoOf(t.cur, tMs); g != nil {
		return g.Lat, g.Lon, true
	}
	var buf [heardMax]model.StationIdx
	for _, st := range ac.heard.list(tMs/1000, buf[:0]) {
		if g := t.netGeoOf(st, tMs); g != nil {
			return g.Lat, g.Lon, true
		}
	}
	return 0, 0, false
}

// ------------------------------------------------------------- projection

// View selects the per-viewer projection of the live picture in aggregator
// mode (DESIGN §6.4, §8.2).
type View struct {
	// Scope of the request. Public sees the merged picture only: no
	// geometry, receiver lists, RSSI, feeds or antennas.
	Scope model.Scope
	// Station is the station whose geometry (distKm, bearing, elevation,
	// slantKm, cpaKm) is computed; 0 = none. An owner gets it only for one
	// of Own; an admin for any located station.
	Station model.StationIdx
	// Own lists the viewer's own stations (owner scope): they decide Mine
	// and which SIDs Stations shows.
	Own []model.StationIdx
}

// SnapshotView returns the live picture projected for one viewer. In
// standalone mode it is Snapshot unchanged. In aggregator mode it copies the
// shared 1 Hz snapshot and, per aircraft, removes what the scope may not see
// and fills the viewer's own fields:
//   - public: no distance, bearing, elevation, slant range, CPA, terrain
//     line of sight, RSSI, feeds, antennas, Mine or Stations;
//   - owner: Mine (one of Own heard it in the last 60 s), Stations (the SIDs
//     of those own stations) and the geometry of View.Station when it is
//     one of Own;
//   - admin: everything, Stations lists every station of the last 60 s,
//     geometry for any located View.Station.
//
// Terrain line of sight and the trajectory-based CPA of the enrichment hook
// refer to the configured receiver (the host station) and are kept only
// when View.Station is model.HostStation; otherwise CPA is a straight-line
// prediction from the selected station (within 30 min).
func (t *Tracker) SnapshotView(v View) model.LiveSnapshot {
	s, side := t.snapshotSide()
	t.mu.Lock()
	net := t.net
	var res model.StationResolver
	if net != nil {
		res = net.res
	}
	t.mu.Unlock()
	if net == nil {
		return s
	}
	p := newProjector(v, res)
	out := s
	out.Aircraft = slices.Clone(s.Aircraft)
	out.Counts.MaxRangeKm = 0
	for i := range out.Aircraft {
		var heard []model.StationIdx
		if i < len(side) {
			heard = side[i].list()
		}
		a := &out.Aircraft[i]
		p.apply(a, heard)
		if !a.Predicted && a.DistKm != nil && *a.DistKm > out.Counts.MaxRangeKm {
			out.Counts.MaxRangeKm = *a.DistKm
		}
	}
	out.Counts.MaxRangeKm = round1(out.Counts.MaxRangeKm)
	return out
}

// AircraftView is Aircraft projected for one viewer (see SnapshotView).
func (t *Tracker) AircraftView(icao uint32, v View) (model.Aircraft, bool) {
	if s := t.sh.Load(); s != nil {
		return s.parts[ShardOf(icao, s.n)].AircraftView(icao, v)
	}
	t.mu.Lock()
	ac := t.ac[icao]
	if ac == nil || !ac.shown() {
		t.mu.Unlock()
		return model.Aircraft{}, false
	}
	now := t.nowMs
	a := t.dto(ac, now, &dtoAlloc{})
	net := t.net
	var heard []model.StationIdx
	var res model.StationResolver
	if net != nil {
		heard = ac.heard.list(now/1000, nil)
		res = net.res
	}
	t.mu.Unlock()
	if net == nil {
		return a, true
	}
	p := newProjector(v, res)
	p.apply(&a, heard)
	return a, true
}

// HeardBy returns the stations that heard aircraft icao in the last 60 s of
// data time (aggregator mode; nil otherwise). It is for operator views and
// the MLAT result-return rule, never for public output.
func (t *Tracker) HeardBy(icao uint32) []model.StationIdx {
	if s := t.sh.Load(); s != nil {
		return s.parts[ShardOf(icao, s.n)].HeardBy(icao)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ac := t.ac[icao]
	if ac == nil || t.net == nil {
		return nil
	}
	return ac.heard.list(t.nowMs/1000, nil)
}

// projector applies one View to DTOs.
type projector struct {
	v    View
	res  model.StationResolver
	st   *geo.Station // geometry reference (nil = none)
	host bool         // st is the host station: keep the enrichment's receiver fields
	sids map[model.StationIdx]string
	al   dtoAlloc
}

func newProjector(v View, res model.StationResolver) *projector {
	p := &projector{v: v, res: res}
	if v.Station != 0 && res != nil {
		allowed := v.Scope == model.ScopeAdmin || (v.Scope == model.ScopeOwner && slices.Contains(v.Own, v.Station))
		if allowed {
			if g, ok := res.Geo(v.Station); ok && geo.ValidLatLon(g.Lat, g.Lon) {
				p.st = stationFromGeo(g)
				p.host = v.Station == model.HostStation
			}
		}
	}
	return p
}

// sid returns the SID of station st ("" = unknown).
func (p *projector) sid(st model.StationIdx) string {
	if s, ok := p.sids[st]; ok {
		return s
	}
	s := ""
	if p.res != nil {
		if m, ok := p.res.Info(st); ok {
			s = m.SID
		}
	}
	if p.sids == nil {
		p.sids = make(map[model.StationIdx]string)
	}
	p.sids[st] = s
	return s
}

// apply projects one aircraft. a is a private copy; its pointer fields may
// be shared with the base snapshot and are replaced, never written through.
func (p *projector) apply(a *model.Aircraft, heard []model.StationIdx) {
	scope := p.v.Scope
	if !scope.Valid() {
		scope = model.ScopePublic
	}
	a.DistKm, a.Bearing, a.Elevation, a.SlantKm = nil, nil, nil, nil
	a.Mine, a.Stations = false, nil
	if scope != model.ScopeAdmin {
		a.Feeds, a.Antennas = nil, nil
		a.RSSI, a.RSSIAvg = nil, nil
	}
	if p.st == nil || !p.host {
		a.TerrainVisible, a.LOSMarginDeg = nil, nil
		a.CPAKm, a.CPAInS = nil, nil
	}
	if scope == model.ScopePublic {
		return
	}
	for _, st := range heard {
		own := slices.Contains(p.v.Own, st)
		if own {
			a.Mine = true
		}
		if own || scope == model.ScopeAdmin {
			if sid := p.sid(st); sid != "" {
				a.Stations = append(a.Stations, sid)
			}
		}
	}
	sort.Strings(a.Stations)
	if p.st == nil || a.Lat == nil || a.Lon == nil {
		return
	}
	km, brg := p.st.DistanceBearing(*a.Lat, *a.Lon)
	a.DistKm, a.Bearing = p.al.fp(round2(km)), p.al.fp(round1(brg))
	alt := math.NaN()
	switch {
	case a.AltBaro != nil:
		alt = *a.AltBaro
	case a.AltGeom != nil:
		alt = *a.AltGeom
	}
	if finite(alt) {
		altM := alt * geo.MetresPerFoot
		a.SlantKm = p.al.fp(round2(geo.SlantRangeKm(km, p.st.AntennaAMSLM, altM)))
		a.Elevation = p.al.fp(round2(geo.ElevationDeg(km, p.st.AntennaAMSLM, altM, p.st.K)))
	}
	if !p.host && a.GS != nil && a.Track != nil {
		if cpa, in, ok := straightCPA(km, brg, *a.GS, *a.Track); ok {
			a.CPAKm, a.CPAInS = p.al.fp(round2(cpa)), p.al.fp(math.Round(in))
		}
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// cpaHorizonS is how far ahead a straight-line closest approach is reported.
const cpaHorizonS = 1800

// straightCPA predicts the closest approach of an aircraft km away on
// bearing brg (from the station) flying gsKt on track trkDeg, in a local
// flat frame: the distance and the seconds until it (0 = now: moving away or
// stationary). ok is false when it lies more than 30 minutes ahead.
func straightCPA(km, brg, gsKt, trkDeg float64) (cpaKm, inS float64, ok bool) {
	if !finite(km) || !finite(brg) || !finite(gsKt) || !finite(trkDeg) {
		return 0, 0, false
	}
	sb, cb := math.Sincos(brg * math.Pi / 180)
	px, py := km*sb, km*cb
	v := gsKt * ktToKmPerS
	st, ct := math.Sincos(trkDeg * math.Pi / 180)
	vx, vy := v*st, v*ct
	vv := vx*vx + vy*vy
	if vv < 1e-12 {
		return km, 0, true
	}
	tt := -(px*vx + py*vy) / vv
	if tt <= 0 {
		return km, 0, true
	}
	if tt > cpaHorizonS {
		return 0, 0, false
	}
	x, y := px+vx*tt, py+vy*tt
	return math.Hypot(x, y), tt, true
}

// ------------------------------------------------------ ICAO sharding seam

// NetHandler is what aggregator ingest (the grouper, late routing, the SBS
// bridge) needs from the tracker. *Tracker implements it, sharded or not:
// an ICAO-sharded tracker (shard.go; docs/aggregator/DESIGN.md §14, §20.2)
// routes every frame with RouteKey to one of N shard Trackers that share one
// Lanes, behind the same *Tracker, so ingest code does not change.
type NetHandler interface {
	Handle(f model.RawFrame) Result
	HandleUpdate(u model.DecodedUpdate)
	Lanes() *Lanes
}

var _ NetHandler = (*Tracker)(nil)

// RouteKey returns the 24-bit address a frame belongs to without a full
// decode, for routing frames to ICAO shards: the clear address field of
// DF11/17/18 and the address recovered from the address/parity field (CRC
// residual) of DF0/4/5/16/20/21. ok is false for other formats and lengths.
// For frames with bit errors, or a Comm-B overlay of the parity field, the
// recovered value is not a real address; such frames are rejected by the
// shard they reach, exactly as an unsharded tracker rejects them.
func RouteKey(data []byte) (addr uint32, ok bool) {
	if len(data) != 7 && len(data) != 14 {
		return 0, false
	}
	switch df := data[0] >> 3; df {
	case 11, 17, 18:
		return uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3]), true
	case 0, 4, 5, 16, 20, 21:
		if (df >= 16) != (len(data) == 14) {
			return 0, false
		}
		return modes.Residual(data) & 0xFFFFFF, true
	}
	return 0, false
}

// ShardOf maps an address to one of n shards (n >= 1), spreading the
// structured ICAO allocation blocks evenly.
func ShardOf(addr uint32, n int) int {
	if n <= 1 {
		return 0
	}
	return int((uint64((addr&0xFFFFFF)*0x9E3779B1) * uint64(n)) >> 32)
}

// LockStats describes how busy the tracker mutex is, from a 1/1024 sample
// of frame handlings: the sharding gate (WIRING.md "Scaling").
type LockStats struct {
	// HoldNs is the mean time one frame or update holds the mutex.
	HoldNs float64
	// Samples is the number of timed handlings behind HoldNs.
	Samples int64
	// Utilization is HoldNs times the smoothed message rate: the fraction
	// of one CPU the tracker spends inside its mutex (summed over the
	// shards of a sharded tracker).
	Utilization float64
	// Shards is the number of ICAO shards (1 = not sharded).
	Shards int
	// MaxShardUtilization is the Utilization of the busiest shard (equal to
	// Utilization when not sharded): one shard saturates at 1.
	MaxShardUtilization float64
}

// LockStats returns the sampled mutex statistics.
func (t *Tracker) LockStats() LockStats {
	if s := t.sh.Load(); s != nil {
		return s.lockStats()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := LockStats{Samples: t.holdK, Shards: 1}
	if t.holdK > 0 {
		s.HoldNs = float64(t.holdNs) / float64(t.holdK)
		s.Utilization = s.HoldNs * t.msgRate / 1e9
	}
	s.MaxShardUtilization = s.Utilization
	return s
}

// holdStart starts a hold-time sample for every 1024th handling. It reads
// ntp.Now, the platform's finest clock (time.Now advances only once per
// timer tick on Windows; ntp.Now has 100 ns steps there, which average out
// over many samples). Caller holds mu.
func (t *Tracker) holdStart() (time.Time, bool) {
	t.lockN++
	if t.lockN&1023 != 0 {
		return time.Time{}, false
	}
	return ntp.Now(), true
}

// holdEnd completes a sample; samples spoilt by a wall-clock step are
// dropped. Caller holds mu.
func (t *Tracker) holdEnd(t0 time.Time, sampled bool) {
	if !sampled {
		return
	}
	if d := ntp.Now().Sub(t0); d >= 0 && d < 10*time.Millisecond {
		t.holdNs += int64(d)
		t.holdK++
	}
}

// netSorter sorts a snapshot's aircraft by ICAO together with their side
// data.
type netSorter struct {
	a    []model.Aircraft
	side []netSide
}

func (s netSorter) Len() int           { return len(s.a) }
func (s netSorter) Less(i, j int) bool { return s.a[i].ICAO < s.a[j].ICAO }
func (s netSorter) Swap(i, j int) {
	s.a[i], s.a[j] = s.a[j], s.a[i]
	s.side[i], s.side[j] = s.side[j], s.side[i]
}
