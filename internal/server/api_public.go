package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"airview/internal/model"
	"airview/internal/privacy/publicdto"
	"airview/internal/registry"
)

// Public network surfaces (routes_net.go, DESIGN §8.2). Every answer is
// built from internal/privacy/publicdto types only.

// publicGate refuses a public viewer when the operator switched the surface
// off (aggregator.publicMap / publicStats / tarJson). Owners and admins and
// standalone mode always pass.
func (s *Server) publicGate(r *http.Request, enabled func(model.Aggregator) bool, status int) error {
	if !s.netPublic(r) {
		return nil
	}
	if enabled(s.eng.EffectiveConfig().Aggregator) {
		return nil
	}
	if status == http.StatusNotFound {
		return notFound("not available on this network")
	}
	return errStatus(status, "not public on this network")
}

func mapOn(a model.Aggregator) bool   { return a.PublicMap }
func statsOn(a model.Aggregator) bool { return a.PublicStats }
func tarOn(a model.Aggregator) bool   { return a.TarJSON && a.PublicMap }

func apiPublicStatus(s *Server, r *http.Request) (any, error) {
	return s.publicStatus(), nil
}

func apiPublicAircraft(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, mapOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	p := s.publicSnapshot()
	return &p, nil
}

func apiPublicAircraftDetail(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, mapOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	key, err := parseICAOParam(r.PathValue("icao"))
	if err != nil {
		return nil, err
	}
	return s.publicDetail(r, key)
}

func (s *Server) trailMinutes(r *http.Request) (int, error) {
	def := s.eng.EffectiveConfig().UI.TrailMinutes
	if def <= 0 {
		def = 15
	}
	return qInt(r, "minutes", def, 1, 240)
}

func apiPublicTrails(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, mapOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	minutes, err := s.trailMinutes(r)
	if err != nil {
		return nil, err
	}
	bb, err := parseTrailBox(r)
	if err != nil {
		return nil, err
	}
	return s.publicTrails(minutes, bb), nil
}

// onlineWindow: a station with data within this window counts as online.
const onlineWindow = 2 * time.Minute

// laneRates keeps the last counters of every lane, so the station lists can
// report recent rates (difference between two refreshes at least 5 s
// apart) instead of lifetime averages.
type laneRates struct {
	mu   sync.Mutex
	last map[model.StationIdx]laneSample
}

type laneSample struct {
	frames, positions uint64
	at                time.Time
	rate, posRate     float64
}

// rates returns the frame and position rates of station idx.
func (l *laneRates) rates(idx model.StationIdx, frames, positions uint64, now time.Time) (fps, pps float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[model.StationIdx]laneSample{}
	}
	p, ok := l.last[idx]
	if !ok || frames < p.frames || positions < p.positions {
		l.last[idx] = laneSample{frames: frames, positions: positions, at: now}
		return 0, 0
	}
	if dt := now.Sub(p.at).Seconds(); dt >= 5 {
		p.rate = float64(frames-p.frames) / dt
		p.posRate = float64(positions-p.positions) / dt
		p.frames, p.positions, p.at = frames, positions, now
		l.last[idx] = p
	}
	return math.Round(p.rate*10) / 10, math.Round(p.posRate*10) / 10
}

// stationOnline reports whether st had data within onlineWindow.
func (s *Server) stationOnline(st *registry.Station, now time.Time) bool {
	if !st.LastSeen.IsZero() && now.Sub(st.LastSeen) < onlineWindow {
		return true
	}
	if lanes := s.eng.Tracker().Lanes(); lanes != nil {
		if li, ok := lanes.Info(st.Idx); ok && li.LastMs > 0 && now.UnixMilli()-li.LastMs < onlineWindow.Milliseconds() {
			return true
		}
	}
	return false
}

// stationStats returns the non-locating statistics of st (shown publicly
// only when the owner opted in).
func (s *Server) stationStats(st *registry.Station, now time.Time) *publicdto.StationStats {
	out := &publicdto.StationStats{}
	if lanes := s.eng.Tracker().Lanes(); lanes != nil {
		if li, ok := lanes.Info(st.Idx); ok {
			out.MsgRate, out.PosRate = s.rates.rates(st.Idx, li.Frames, li.Positions, now)
			out.Aircraft = li.Aircraft
			out.Positions = li.Positions
		}
	}
	return out
}

// hasPublicPin reports whether st has a stored public pin to show.
func hasPublicPin(st *registry.Station) bool {
	return st.HasPublic && st.Pin != model.PinHidden && st.Public.Effective != "" && st.Public.Effective != model.PinHidden
}

func apiPublicStations(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, statsOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	out := publicdto.StationList{Stations: []publicdto.StationEntry{}}
	reg := s.registry()
	if reg == nil || !s.aggregatorMode() {
		return out, nil
	}
	now := time.Now()
	var anon []anonEntry
	for _, st := range reg.List(registry.Filter{Admission: model.AdmApproved}) {
		out.Approved++
		online := s.stationOnline(&st, now)
		if online {
			out.Online++
		}
		e := publicdto.StationEntry{Online: online}
		switch {
		case hasPublicPin(&st):
			e.SID = st.SID
			e.Pin = &publicdto.Pin{Lat: st.Public.Lat, Lon: st.Public.Lon, Precision: st.Public.Effective}
		case st.PublicStats:
		default:
			continue
		}
		if st.ShowName {
			e.Name = st.Name
		}
		if st.PublicStats {
			e.Stats = s.stationStats(&st, now)
		}
		if e.SID == "" {
			// PRIV-A7: no lifetime counter and no stable position in the
			// list, so the entry cannot be followed across polls.
			e.Stats.Unlinkable()
			anon = append(anon, anonEntry{key: anonOrderKey(st.SID, now), e: e})
			continue
		}
		out.Stations = append(out.Stations, e)
	}
	sort.Slice(anon, func(i, j int) bool { return anon[i].key < anon[j].key })
	for _, a := range anon {
		out.Stations = append(out.Stations, a.e)
	}
	return out, nil
}

// anonEntry is a public station entry without SID with its order key.
type anonEntry struct {
	key string
	e   publicdto.StationEntry
}

// anonSecret keys the order of the anonymous entries; it never leaves the
// process.
var anonSecret = func() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}()

// anonOrderKey is the per-day order key of an anonymous station entry
// (HMAC of the UTC day and the SID under a process secret): the order
// changes every day and with every restart and says nothing about the
// registry order.
func anonOrderKey(sid string, now time.Time) string {
	m := hmac.New(sha256.New, anonSecret)
	m.Write([]byte(now.UTC().Format("2006-01-02")))
	m.Write([]byte{0})
	m.Write([]byte(sid))
	return string(m.Sum(nil))
}

// stationCounts counts the approved stations: all, online, with a public pin.
func (s *Server) stationCounts() publicdto.StationCounts {
	var c publicdto.StationCounts
	reg := s.registry()
	if reg == nil || !s.aggregatorMode() {
		return c
	}
	now := time.Now()
	for _, st := range reg.List(registry.Filter{Admission: model.AdmApproved}) {
		c.Total++
		if s.stationOnline(&st, now) {
			c.Online++
		}
		if hasPublicPin(&st) {
			c.Public++
		}
	}
	return c
}

func apiPublicStats(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, statsOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	snap := s.publicSnapshot()
	c := s.stationCounts()
	out := publicdto.NetworkStats{Now: time.Now().UnixMilli(), Counts: snap.Counts, Approved: c.Total, Online: c.Online}
	if s.aggregatorMode() {
		out.Name = s.eng.EffectiveConfig().Aggregator.Name
	}
	return out, nil
}

func apiPublicSummary(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, statsOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	snap := s.publicSnapshot()
	out := publicdto.Summary{StationCounts: s.stationCounts(), Aircraft: snap.Counts.Aircraft, MLATAircraft: snap.Counts.MLAT,
		MessagesPerSec: snap.Counts.MsgRate, Updated: snap.Now, MLAT: s.publicMLATQuality()}
	if s.aggregatorMode() {
		out.Name = s.eng.EffectiveConfig().Aggregator.Name
	}
	return out, nil
}

func apiTarAircraft(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, tarOn, http.StatusNotFound); err != nil {
		return nil, err
	}
	p, msgs := s.publicSnapshotMsgs()
	f := publicdto.TarFromSnapshot(&p, msgs)
	return &f, nil
}

func apiTarReceiver(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, tarOn, http.StatusNotFound); err != nil {
		return nil, err
	}
	out := publicdto.TarReceiver{Version: "AirView " + s.opts.Version, Refresh: 1000}
	if !s.aggregatorMode() {
		if st := s.eng.Status().Station; st.Lat != nil && st.Lon != nil {
			out.Lat, out.Lon = st.Lat, st.Lon
		}
		return out, nil
	}
	reg := s.registry()
	if reg == nil {
		return out, nil
	}
	var sLat, sLon float64
	n := 0
	for _, st := range reg.List(registry.Filter{Admission: model.AdmApproved}) {
		if hasPublicPin(&st) {
			sLat += st.Public.Lat
			sLon += st.Public.Lon
			n++
		}
	}
	if n >= 3 {
		lat, lon := math.Round(sLat/float64(n)*10)/10, math.Round(sLon/float64(n)*10)/10
		out.Lat, out.Lon = &lat, &lon
	}
	return out, nil
}
