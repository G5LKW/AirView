package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"airview/internal/alerts"
	"airview/internal/modes"
	"airview/internal/navdata"
	"airview/internal/rfstat"
	"airview/internal/terrain"
	"airview/internal/trajectory"
)

// Handlers of the trajectory, RF statistics, terrain, navigation data and
// alert endpoints (see the WIRING.md of each package).

// ---------------------------------------------------------------------------
// Prediction (internal/trajectory)
// ---------------------------------------------------------------------------

func (s *Server) trajectory() (*trajectory.Manager, error) {
	tj := s.eng.Trajectory()
	if tj == nil || !s.eng.PredictionEnabled() {
		return nil, errStatus(http.StatusConflict, "prediction is disabled (config.prediction.enabled)")
	}
	return tj, nil
}

// predictionLive is GET /api/prediction/live.
type predictionLive struct {
	Now      int64                  `json:"now"`      // Unix ms
	CoastSec int                    `json:"coastSec"` // config.prediction.coastSec
	Aircraft []trajectory.Predicted `json:"aircraft"` // every aircraft with a usable estimate; coast=true when its last position is older than 4 s
}

func apiPredictionLive(s *Server, r *http.Request) (any, error) {
	tj, err := s.trajectory()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := predictionLive{Now: now.UnixMilli(), CoastSec: s.eng.EffectiveConfig().Prediction.CoastSec}
	coast := float64(out.CoastSec)
	for _, p := range tj.Predicted(now, nil) {
		if p.AgeSec <= coast {
			out.Aircraft = append(out.Aircraft, p)
		}
	}
	if out.Aircraft == nil {
		out.Aircraft = []trajectory.Predicted{}
	}
	sort.Slice(out.Aircraft, func(i, j int) bool { return out.Aircraft[i].ICAO < out.Aircraft[j].ICAO })
	return out, nil
}

func apiPredictionStats(s *Server, r *http.Request) (any, error) {
	tj, err := s.trajectory()
	if err != nil {
		return nil, err
	}
	return tj.Stats(time.Now()), nil
}

// predictedPath is GET /api/prediction/path/{icao}.
type predictedPath struct {
	trajectory.Path
	// Terrain tells when the aircraft is expected to drop below (or rise
	// above) the terrain radio horizon along the path; null until the terrain
	// analysis exists.
	Terrain *terrain.VisibilityForecast `json:"terrain"`
}

func apiPredictionPath(s *Server, r *http.Request) (any, error) {
	tj, err := s.trajectory()
	if err != nil {
		return nil, err
	}
	key, err := parseICAOParam(r.PathValue("icao"))
	if err != nil {
		return nil, err
	}
	def := s.eng.EffectiveConfig().Prediction.PathSec
	horizon, err := qInt(r, "horizon", def, 10, 900)
	if err != nil {
		return nil, err
	}
	step, err := qInt(r, "step", 10, 1, 300)
	if err != nil {
		return nil, err
	}
	p, ok := tj.PathFor(key, time.Now(), time.Duration(horizon)*time.Second, time.Duration(step)*time.Second)
	if !ok {
		return nil, notFound("no prediction for %s (unknown, or no position for more than %d s)", modes.FormatICAO(key), int(maxCoastSec))
	}
	out := predictedPath{Path: p}
	if ts := s.eng.Terrain(); ts != nil && ts.Analysis() != nil && p.AltOK && len(p.Points) > 0 {
		pts := make([]terrain.PathPoint, len(p.Points))
		for i, q := range p.Points {
			pts[i] = terrain.PathPoint{TMs: p.T0 + int64(q.DT*1000), Lat: q.Lat, Lon: q.Lon, AltFt: q.AltFt}
		}
		f := ts.Forecast("", pts)
		if f.Known {
			out.Terrain = &f
		}
	}
	return out, nil
}

const maxCoastSec = 120.0

func apiPredictionIntent(s *Server, r *http.Request) (any, error) {
	tj, err := s.trajectory()
	if err != nil {
		return nil, err
	}
	key, err := parseICAOParam(r.PathValue("icao"))
	if err != nil {
		return nil, err
	}
	in, ok := tj.Intent(key, time.Now())
	if !ok {
		return nil, notFound("no prediction for %s", modes.FormatICAO(key))
	}
	return in, nil
}

// proximityReply is GET /api/prediction/proximity.
type proximityReply struct {
	Now   int64                      `json:"now"`
	Note  string                     `json:"note"`
	Pairs []trajectory.ProximityPair `json:"pairs"` // sorted by severity (<= 1: both thresholds violated)
}

func apiPredictionProximity(s *Server, r *http.Request) (any, error) {
	tj, err := s.trajectory()
	if err != nil {
		return nil, err
	}
	var o trajectory.ProximityOptions
	if o.SearchKm, err = qFloat(r, "km", 40); err != nil {
		return nil, err
	}
	h, err := qInt(r, "horizon", 120, 10, 600)
	if err != nil {
		return nil, err
	}
	o.HorizonSec = float64(h)
	if o.HThreshKm, err = qFloat(r, "hkm", 9.26); err != nil {
		return nil, err
	}
	if o.VThreshFt, err = qFloat(r, "vft", 1000); err != nil {
		return nil, err
	}
	if o.MaxPairs, err = qInt(r, "max", 200, 1, 1000); err != nil {
		return nil, err
	}
	o.SearchKm = math.Min(math.Max(o.SearchKm, 1), 200)
	now := time.Now()
	pairs := tj.Proximity(now, o)
	if pairs == nil {
		pairs = []trajectory.ProximityPair{}
	}
	return proximityReply{Now: now.UnixMilli(), Pairs: pairs,
		Note: "Predicted from received data only; informational, not an ATC or collision-avoidance function."}, nil
}

// ---------------------------------------------------------------------------
// RF statistics (internal/rfstat)
// ---------------------------------------------------------------------------

// rfReport returns the shared Report (computed at most every 2 s).
func (s *Server) rfReport() (*rfstat.Report, error) {
	rs := s.eng.RFStats()
	if rs == nil {
		return nil, errStatus(http.StatusConflict, "RF statistics are disabled (config.rfStat.enabled)")
	}
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	if s.rfRep != nil && s.rfFrom == rs && time.Since(s.rfAt) < 2*time.Second {
		return s.rfRep, nil
	}
	rep := rs.Report()
	s.rfRep, s.rfFrom, s.rfAt = &rep, rs, time.Now()
	return s.rfRep, nil
}

// rfSummary is GET /api/rf/summary: the headline numbers of every section.
type rfSummary struct {
	T            int64      `json:"t"`         // Unix ms
	WindowSec    rfstat.Num `json:"windowSec"` // sliding window of the statistics (about 1 h)
	Health       int        `json:"health"`    // 0..100, -1 = no data
	HealthStatus string     `json:"healthStatus"`
	Advice       []string   `json:"advice"`
	// AdviceItems are the Advice sentences with their code and numbers
	// (rfstat.AdviceItem), so a UI can re-format them in its own units.
	AdviceItems []rfstat.AdviceItem `json:"adviceItems"`

	FPS          rfstat.Num `json:"fps"`              // frames per second
	OccupancyPct rfstat.Num `json:"occupancyPct"`     // 1090 MHz channel occupancy by received replies
	PeakOccPct   rfstat.Num `json:"peakOccupancyPct"` // 1 s peak, last 10 min
	CRCBadFrac   rfstat.Num `json:"crcBadFrac"`
	FailFrac     rfstat.Num `json:"failFrac"`

	DecodeFloorDbfs rfstat.Num `json:"decodeFloorDbfs"`
	NoiseFloorDbfs  rfstat.Num `json:"noiseFloorEstDbfs"`
	P50Dbfs         rfstat.Num `json:"p50Dbfs"`
	StrongFrac      rfstat.Num `json:"strongFrac"`
	SNRMarginDb     rfstat.Num `json:"snrMarginDb"`

	PdPos     rfstat.Num `json:"pdPos"` // position squitter reception probability
	Range50Km rfstat.Num `json:"range50Km"`
	Range90Km rfstat.Num `json:"range90Km"`

	ActiveRadars int        `json:"activeRadars"`
	ADSBAircraft int        `json:"adsbAircraft"`
	GNSSIndexNow rfstat.Num `json:"gnssIndexNow"` // % of ADS-B aircraft with degraded GNSS integrity
	TimingOK     bool       `json:"timingAvailable"`
}

func apiRFSummary(s *Server, r *http.Request) (any, error) {
	rep, err := s.rfReport()
	if err != nil {
		return nil, err
	}
	out := rfSummary{
		T: rep.T, WindowSec: rep.WindowSec, Health: rep.Health.Score, HealthStatus: rep.Health.Status, Advice: rep.Health.Advice,
		AdviceItems: rep.Health.AdviceItems,
		FPS:         rep.Channel.FPS, OccupancyPct: rep.Channel.OccupancyPct, PeakOccPct: rep.Channel.PeakOccPct,
		CRCBadFrac: rep.Channel.BadFrac, FailFrac: rep.Channel.FailFrac,
		DecodeFloorDbfs: rep.Signal.DecodeFloor, NoiseFloorDbfs: rep.Signal.NoiseFloorEst, P50Dbfs: rep.Signal.P50,
		StrongFrac: rep.Signal.StrongFrac, SNRMarginDb: rep.Signal.SNRMargin,
		Range50Km: rep.Detection.Range50Km, Range90Km: rep.Detection.Range90Km, PdPos: rfstat.Num(math.NaN()),
		ActiveRadars: rep.Interrogation.ActiveRadars, ADSBAircraft: rep.Census.ADSB, GNSSIndexNow: rep.Census.GNSSIndexNow,
		TimingOK: rep.Timing.Available,
	}
	for _, t := range rep.Detection.Types {
		if t.Type == "position" || t.Type == "pos" {
			out.PdPos = t.Pd
		}
	}
	if out.Advice == nil {
		out.Advice = []string{}
	}
	if out.AdviceItems == nil {
		out.AdviceItems = []rfstat.AdviceItem{}
	}
	return out, nil
}

func rfPart(pick func(*rfstat.Report) any) func(s *Server, r *http.Request) (any, error) {
	return func(s *Server, r *http.Request) (any, error) {
		rep, err := s.rfReport()
		if err != nil {
			return nil, err
		}
		return pick(rep), nil
	}
}

// rfIntervals is GET /api/rf/intervals: position update intervals.
type rfIntervals struct {
	Gaps      []rfstat.GapBand `json:"gaps"` // per range band
	Types     []rfstat.PdType  `json:"types"`
	Range50Km rfstat.Num       `json:"range50Km"`
	Range90Km rfstat.Num       `json:"range90Km"`
}

// rfInterrogators is GET /api/rf/interrogators: the interrogation part of
// the RF report with the report time, so the UI can place the per-minute
// timelines on the server's clock.
type rfInterrogators struct {
	T             int64                      `json:"t"` // RF report time (Unix ms)
	Interrogators *rfstat.InterrogationStats `json:"interrogators"`
}

// rfGNSS is GET /api/rf/gnss: the GNSS interference part of the census.
type rfGNSS struct {
	IndexNow       rfstat.Num            `json:"indexNow"`
	Series         []rfstat.GNSSPoint    `json:"series"` // per minute, 24 h
	Cells          []rfstat.GNSSCell     `json:"cells"`  // polar map cells around the station
	Degraded       int                   `json:"degradedNow"`
	DegradedList   []string              `json:"degradedList"`
	JumpAircraft   int                   `json:"jumpAircraft"`
	GNSSDelta      rfstat.Hist           `json:"gnssDelta"`
	GNSSDeltaMean  rfstat.Num            `json:"gnssDeltaMeanFt"`
	GNSSDeltaByAlt []rfstat.AltBandDelta `json:"gnssDeltaByAlt"`
}

var (
	apiRFReport        = rfPart(func(r *rfstat.Report) any { return r })
	apiRFSignal        = rfPart(func(r *rfstat.Report) any { return r.Signal })
	apiRFDetection     = rfPart(func(r *rfstat.Report) any { return r.Detection })
	apiRFChannel       = rfPart(func(r *rfstat.Report) any { return r.Channel })
	apiRFInterrogators = rfPart(func(r *rfstat.Report) any { return rfInterrogators{T: r.T, Interrogators: &r.Interrogation} })
	apiRFTiming        = rfPart(func(r *rfstat.Report) any { return r.Timing })
	apiRFFleet         = rfPart(func(r *rfstat.Report) any { return r.Census })
	apiRFHealth        = rfPart(func(r *rfstat.Report) any { return r.Health })
	apiRFIntervals     = rfPart(func(r *rfstat.Report) any {
		return rfIntervals{Gaps: r.Detection.Gaps, Types: r.Detection.Types, Range50Km: r.Detection.Range50Km, Range90Km: r.Detection.Range90Km}
	})
	apiRFGNSS = rfPart(func(r *rfstat.Report) any {
		c := &r.Census
		return rfGNSS{IndexNow: c.GNSSIndexNow, Series: c.GNSSSeries, Cells: c.GNSSCells, Degraded: c.Degraded,
			DegradedList: c.DegradedList, JumpAircraft: c.JumpAircraft, GNSSDelta: c.GNSSDelta,
			GNSSDeltaMean: c.GNSSDeltaMean, GNSSDeltaByAlt: c.GNSSDeltaByAlt}
	})
)

func apiRFHistory(s *Server, r *http.Request) (any, error) {
	from, to, err := qSpanDefault(r, 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	out, err := rfstat.LoadHistory(r.Context(), s.eng.Store().DB(), from, to)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		if rs := s.eng.RFStats(); rs != nil {
			for _, h := range rs.History() {
				if h.T >= from.UnixMilli() && h.T < to.UnixMilli() {
					out = append(out, h)
				}
			}
		}
	}
	if out == nil {
		out = []rfstat.HourSummary{}
	}
	return map[string]any{"hours": out}, nil
}

// ---------------------------------------------------------------------------
// Terrain (internal/terrain)
// ---------------------------------------------------------------------------

// terrainStatus is GET /api/terrain/status.
type terrainStatus struct {
	terrain.StatusResponse
	Enabled      bool `json:"enabled"`      // config.terrain.enabled
	AutoPrefetch bool `json:"autoPrefetch"` // config.terrain.autoPrefetch
	Located      bool `json:"located"`      // the (effective) receiver position is known
}

func apiTerrainStatus(s *Server, r *http.Request) (any, error) {
	ts := s.eng.Terrain()
	cfg := s.eng.EffectiveConfig()
	out := terrainStatus{Enabled: cfg.Terrain.Enabled, AutoPrefetch: cfg.Terrain.AutoPrefetch, Located: cfg.Receiver.Located}
	out.Job = ts.Status()
	out.Attribution, out.AttributionURL = terrain.AttributionShort, terrain.AttributionURL
	if a := ts.Analysis(); a != nil {
		out.Available, out.Key, out.ComputedMs, out.DataComplete = true, a.Key, a.ComputedMs, a.DataComplete
	}
	if p := ts.Provider(); p != nil {
		st := p.Stats()
		out.Provider = &st
	}
	return out, nil
}

func apiTerrainPrefetch(s *Server, r *http.Request) (any, error) {
	force := qBool(r, "force", false)
	started, ok := s.eng.TerrainPrefetch(force)
	if !ok {
		return nil, errStatus(http.StatusConflict, "terrain analysis needs config.terrain.enabled and a located receiver")
	}
	return map[string]any{"started": started, "job": s.eng.Terrain().Status()}, nil
}

// noStoreWriter forces the API cache headers on the terrain package's handlers.
type noStoreWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *noStoreWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.ResponseWriter.WriteHeader(code)
}

func (w *noStoreWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// terrainRaw adapts a handler of the terrain package to the route table.
// alias renames query parameters (e.g. alt -> altFt) before the call.
func terrainRaw(path string, alias map[string]string) func(s *Server, w http.ResponseWriter, r *http.Request) {
	return func(s *Server, w http.ResponseWriter, r *http.Request) {
		s.resetMu.RLock()
		defer s.resetMu.RUnlock()
		// Analyses and panoramas can take a while and stream large
		// documents: a factory reset aborts them instead of waiting.
		r, end := s.beginAbortable(w, r, exportMaxDuration)
		defer end()
		ts := s.eng.Terrain()
		if ts == nil || !s.eng.Running() {
			writeError(w, http.StatusServiceUnavailable, "engine is not running")
			return
		}
		var h http.HandlerFunc
		for _, rt := range ts.Routes() {
			if rt.Path == path && rt.Method == r.Method {
				h = rt.Handler
				break
			}
		}
		if h == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if len(alias) > 0 {
			q := r.URL.Query()
			for from, to := range alias {
				if v := q.Get(from); v != "" && q.Get(to) == "" {
					q.Set(to, v)
				}
			}
			r2 := r.Clone(r.Context())
			r2.URL.RawQuery = q.Encode()
			r = r2
		}
		h(&noStoreWriter{ResponseWriter: w}, r)
	}
}

// ---------------------------------------------------------------------------
// Navigation data (internal/navdata)
// ---------------------------------------------------------------------------

func navErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, navdata.ErrBadInput):
		return badRequest("%s", err.Error())
	case errors.Is(err, navdata.ErrNoStore):
		return errStatus(http.StatusServiceUnavailable, "%s", err.Error())
	case errors.Is(err, navdata.ErrImportRunning), errors.Is(err, navdata.ErrDisabled), errors.Is(err, navdata.ErrContactMissing):
		return errStatus(http.StatusConflict, "%s", err.Error())
	case errors.Is(err, navdata.ErrRateLimited):
		return errStatus(http.StatusTooManyRequests, "%s", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return errStatus(http.StatusBadGateway, "%s", err.Error())
}

// navStatus is GET /api/navdata/status.
type navStatus struct {
	Datasets       []navdata.DatasetInfo `json:"datasets"`       // imported datasets (rows, time, source with licence)
	Sources        []navdata.Source      `json:"sources"`        // downloadable datasets
	Import         navdata.ImportStatus  `json:"import"`         // current / last import
	RegionAirports int                   `json:"regionAirports"` // airports within 800 km of the receiver loaded for the live picture
	RoutesOnline   bool                  `json:"routesOnline"`   // config.navData.routesOnline
	Photos         bool                  `json:"photos"`         // config.navData.photos (and a contact is set)
	Overlay        bool                  `json:"airportsOverlay"`
	Attribution    []string              `json:"attribution"` // lines to show where the data is used
}

func apiNavStatus(s *Server, r *http.Request) (any, error) {
	nd := s.eng.NavData()
	ds, err := nd.Datasets(r.Context())
	if err != nil {
		return nil, navErr(err)
	}
	if ds == nil {
		ds = []navdata.DatasetInfo{}
	}
	cfg := s.eng.EffectiveConfig().NavData
	out := navStatus{Datasets: ds, Sources: navdata.Sources(), Import: nd.ImportStatus(),
		RoutesOnline: cfg.RoutesOnline, Photos: cfg.Photos && cfg.PhotoContact != "", Overlay: cfg.AirportsOverlay,
		Attribution: []string{"Airport data: OurAirports (ourairports.com)", "Routes: Virtual Radar Server standing data"}}
	if reg := s.eng.Region(); reg != nil {
		out.RegionAirports = reg.Len()
	}
	if cfg.RoutesOnline {
		out.Attribution = append(out.Attribution, "Online routes: adsbdb.com")
	}
	if out.Photos {
		out.Attribution = append(out.Attribution, "Photos: planespotters.net")
	}
	return out, nil
}

func apiNavImport(s *Server, r *http.Request) (any, error) {
	var in struct {
		Datasets []navdata.Dataset `json:"datasets"`
		Force    bool              `json:"force"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(nil, r, &in, 16<<10); err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") {
			return nil, err
		}
	}
	nd := s.eng.NavData()
	if err := nd.StartImport(s.eng.RuntimeContext(), navdata.ImportOptions{Datasets: in.Datasets, Force: in.Force}); err != nil {
		return nil, navErr(err)
	}
	return map[string]any{"started": true, "import": nd.ImportStatus()}, nil
}

func apiNavProgress(s *Server, r *http.Request) (any, error) {
	return s.eng.NavData().ImportStatus(), nil
}

func apiNavCancel(s *Server, r *http.Request) (any, error) {
	nd := s.eng.NavData()
	nd.CancelImport()
	return map[string]any{"cancelled": true, "import": nd.ImportStatus()}, nil
}

func apiNavRouteCacheClear(s *Server, r *http.Request) (any, error) {
	if err := s.eng.NavData().ClearRouteCache(r.Context()); err != nil {
		return nil, navErr(err)
	}
	return map[string]any{"ok": true}, nil
}

// parseBBox parses "S,W,N,E".
func parseBBox(v string) (navdata.BBox, error) {
	var b navdata.BBox
	parts := strings.Split(v, ",")
	if len(parts) != 4 {
		return b, badRequest("parameter bbox: S,W,N,E expected")
	}
	var f [4]float64
	for i, p := range parts {
		x, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return b, badRequest("parameter bbox: not a number")
		}
		f[i] = x
	}
	b = navdata.BBox{S: f[0], W: f[1], N: f[2], E: f[3]}
	if !b.Valid() {
		return b, badRequest("parameter bbox: invalid box")
	}
	return b, nil
}

func apiNavAirports(s *Server, r *http.Request) (any, error) {
	nd := s.eng.NavData()
	minSize := navdata.SizeSmall
	if v := qStr(r, "minSize"); v != "" {
		sz, ok := navdata.ParseSize(v)
		if !ok {
			return nil, badRequest("parameter minSize: large | medium | small | other | all")
		}
		minSize = sz
	}
	limit, err := qInt(r, "limit", 500, 1, 5000)
	if err != nil {
		return nil, err
	}
	runways := qBool(r, "runways", false)
	var as []navdata.Airport
	if v := qStr(r, "bbox"); v != "" {
		b, err := parseBBox(v)
		if err != nil {
			return nil, err
		}
		as, err = nd.AirportsInBBox(r.Context(), navdata.AirportQuery{BBox: b, MinSize: minSize, Limit: limit, Runways: runways})
		if err != nil {
			return nil, navErr(err)
		}
	} else {
		rx := s.eng.EffectiveConfig().Receiver
		lat, err := qFloat(r, "lat", rx.Lat)
		if err != nil {
			return nil, err
		}
		lon, err := qFloat(r, "lon", rx.Lon)
		if err != nil {
			return nil, err
		}
		if qStr(r, "lat") == "" && s.netRestricted(r) {
			// Aggregator mode below admin: the receiver position is not public.
			return nil, badRequest("bbox or lat, lon are required")
		}
		if qStr(r, "lat") == "" && !rx.Located {
			return nil, badRequest("bbox or lat, lon are required (the receiver is not located)")
		}
		radius, err := qFloat(r, "radiusKm", 100)
		if err != nil {
			return nil, err
		}
		if radius <= 0 || radius > 5000 {
			return nil, badRequest("parameter radiusKm: 0..5000")
		}
		as, err = nd.AirportsNear(r.Context(), lat, lon, radius, minSize, limit, runways)
		if err != nil {
			return nil, navErr(err)
		}
	}
	if as == nil {
		as = []navdata.Airport{}
	}
	if qStr(r, "format") == "geojson" {
		return navdata.AirportOverlay(as, navdata.OverlayOptions{}), nil
	}
	return as, nil
}

func apiNavSearch(s *Server, r *http.Request) (any, error) {
	q := qStr(r, "q")
	if q == "" {
		return nil, badRequest("parameter q is required")
	}
	limit, err := qInt(r, "limit", 20, 1, 200)
	if err != nil {
		return nil, err
	}
	as, err := s.eng.NavData().SearchAirports(r.Context(), q, limit)
	if err != nil {
		return nil, navErr(err)
	}
	if as == nil {
		as = []navdata.Airport{}
	}
	return as, nil
}

func apiNavNavaids(s *Server, r *http.Request) (any, error) {
	b, err := parseBBox(qStr(r, "bbox"))
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 500, 1, 5000)
	if err != nil {
		return nil, err
	}
	var types []string
	if v := qStr(r, "types"); v != "" {
		types = strings.Split(strings.ToUpper(v), ",")
	}
	ns, err := s.eng.NavData().NavaidsInBBox(r.Context(), navdata.NavaidQuery{BBox: b, Types: types, Limit: limit})
	if err != nil {
		return nil, navErr(err)
	}
	if ns == nil {
		ns = []navdata.Navaid{}
	}
	return ns, nil
}

// navAirportReply is GET /api/navdata/airport/{code}.
type navAirportReply struct {
	Airport navdata.Airport         `json:"airport"` // with runways
	Ends    []navdata.RunwayEndInfo `json:"ends"`    // usable landing directions
}

func apiNavAirport(s *Server, r *http.Request) (any, error) {
	code := strings.ToUpper(strings.TrimSpace(r.PathValue("code")))
	if code == "" || len(code) > 16 {
		return nil, badRequest("invalid airport code")
	}
	a, ok, err := s.eng.NavData().AirportByCode(r.Context(), code, true)
	if err != nil {
		return nil, navErr(err)
	}
	if !ok {
		return nil, notFound("airport %s not found (import the airport data first)", code)
	}
	ends := navdata.RunwayEnds(&a, nil)
	if ends == nil {
		ends = []navdata.RunwayEndInfo{}
	}
	return navAirportReply{Airport: a, Ends: ends}, nil
}

func apiNavRoute(s *Server, r *http.Request) (any, error) {
	cs := r.PathValue("callsign")
	var pos *navdata.Point
	if qStr(r, "lat") != "" || qStr(r, "lon") != "" {
		lat, lon, err := qLatLon(r)
		if err != nil {
			return nil, err
		}
		pos = &navdata.Point{Lat: lat, Lon: lon}
	}
	info, err := s.eng.NavData().Route(r.Context(), cs, pos)
	if err != nil {
		return nil, navErr(err)
	}
	s.eng.PutRoute(info)
	return info, nil
}

func apiNavPhoto(s *Server, r *http.Request) (any, error) {
	key, err := parseICAOParam(r.PathValue("icao"))
	if err != nil {
		return nil, err
	}
	if key&modes.NonICAOFlag != 0 {
		return nil, badRequest("photos exist only for ICAO addresses")
	}
	p, err := s.eng.NavData().Photo(r.Context(), key, qStr(r, "reg"))
	if err != nil {
		return nil, navErr(err)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Alerts (internal/alerts)
// ---------------------------------------------------------------------------

func alertErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, alerts.ErrInvalid):
		return badRequest("%s", err.Error())
	case errors.Is(err, alerts.ErrNotFound):
		return notFound("%s", err.Error())
	case errors.Is(err, alerts.ErrTooMany):
		return errStatus(http.StatusConflict, "%s", err.Error())
	}
	return err
}

func apiAlertRules(s *Server, r *http.Request) (any, error) {
	rules := s.eng.Alerts().Rules()
	if rules == nil {
		rules = []alerts.Rule{}
	}
	if !s.isAdmin(r) {
		rules = alerts.RedactRules(rules) // webhook URLs embed tokens
	}
	return rules, nil
}

func apiAlertRule(s *Server, r *http.Request) (any, error) {
	rule, ok := s.eng.Alerts().Rule(r.PathValue("id"))
	if !ok {
		return nil, notFound("rule %q not found", r.PathValue("id"))
	}
	if !s.isAdmin(r) {
		rule = alerts.RedactRules([]alerts.Rule{rule})[0]
	}
	return rule, nil
}

func readRule(r *http.Request) (alerts.Rule, error) {
	raw, err := readBody(r, alerts.MaxRuleBytes+1024)
	if err != nil {
		return alerts.Rule{}, err
	}
	rule, err := alerts.ParseRule(raw)
	if err != nil {
		return alerts.Rule{}, alertErr(err)
	}
	return rule, nil
}

func apiAlertRuleCreate(s *Server, r *http.Request) (any, error) {
	rule, err := readRule(r)
	if err != nil {
		return nil, err
	}
	rule.ID = ""
	out, err := s.eng.Alerts().PutRule(r.Context(), rule)
	return out, alertErr(err)
}

func apiAlertRuleUpdate(s *Server, r *http.Request) (any, error) {
	rule, err := readRule(r)
	if err != nil {
		return nil, err
	}
	rule.ID = r.PathValue("id")
	if _, ok := s.eng.Alerts().Rule(rule.ID); !ok {
		return nil, notFound("rule %q not found", rule.ID)
	}
	out, err := s.eng.Alerts().PutRule(r.Context(), rule)
	return out, alertErr(err)
}

func apiAlertRuleDelete(s *Server, r *http.Request) (any, error) {
	if err := s.eng.Alerts().DeleteRule(r.Context(), r.PathValue("id")); err != nil {
		return nil, alertErr(err)
	}
	return map[string]any{"ok": true}, nil
}

func apiAlertRuleEnabled(s *Server, r *http.Request) (any, error) {
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := readJSON(nil, r, &in, 4<<10); err != nil {
		return nil, err
	}
	if in.Enabled == nil {
		return nil, badRequest("body {\"enabled\":true|false} expected")
	}
	out, err := s.eng.Alerts().SetEnabled(r.Context(), r.PathValue("id"), *in.Enabled)
	return out, alertErr(err)
}

func apiAlertResetDefaults(s *Server, r *http.Request) (any, error) {
	if err := s.eng.Alerts().ResetDefaults(r.Context()); err != nil {
		return nil, alertErr(err)
	}
	return map[string]any{"ok": true}, nil
}

// alertTestReply is POST /api/alerts/test.
type alertTestReply struct {
	Aircraft int                `json:"aircraft"` // aircraft in the live picture tested
	Matches  []alerts.TestMatch `json:"matches"`
}

func apiAlertTest(s *Server, r *http.Request) (any, error) {
	raw, err := readBody(r, alerts.MaxRuleBytes+4096)
	if err != nil {
		return nil, err
	}
	var in struct {
		Rule  json.RawMessage     `json:"rule"`
		Event *alerts.SystemEvent `json:"event"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || len(in.Rule) == 0 {
		return nil, badRequest("body {\"rule\": Rule, \"event\": SystemEvent?} expected")
	}
	rule, err := alerts.ParseRule(in.Rule)
	if err != nil {
		return nil, alertErr(err)
	}
	snap := s.eng.Tracker().Snapshot()
	m, err := s.eng.Alerts().TestRule(rule, snap.Aircraft, in.Event, time.UnixMilli(snap.Now))
	if err != nil {
		return nil, alertErr(err)
	}
	if m == nil {
		m = []alerts.TestMatch{}
	}
	return alertTestReply{Aircraft: len(snap.Aircraft), Matches: m}, nil
}

func apiAlertRecent(s *Server, r *http.Request) (any, error) {
	limit, err := qInt(r, "limit", 100, 1, 500)
	if err != nil {
		return nil, err
	}
	after, err := qInt64(r, "after", 0)
	if err != nil {
		return nil, err
	}
	out := s.eng.Alerts().Recent(limit, after)
	if out == nil {
		out = []alerts.Alert{}
	}
	return out, nil
}

func apiAlertHighlights(s *Server, r *http.Request) (any, error) {
	return s.eng.Alerts().Highlights(time.Now()), nil
}

func apiAlertStats(s *Server, r *http.Request) (any, error) {
	return s.eng.Alerts().Stats(), nil
}

func apiAlertPlaceholders(s *Server, r *http.Request) (any, error) {
	return map[string]any{"text": alerts.PlaceholderHelp()}, nil
}

func apiAlertWebhookTest(s *Server, r *http.Request) (any, error) {
	var in struct {
		URL string `json:"url"`
	}
	if err := readJSON(nil, r, &in, 8<<10); err != nil {
		return nil, err
	}
	if err := alerts.ValidateWebhookURL(in.URL); err != nil {
		return nil, badRequest("%s", err.Error())
	}
	if !s.eng.EffectiveConfig().Alerts.Webhooks {
		return nil, errStatus(http.StatusConflict, "webhooks are switched off (config.alerts.webhooks)")
	}
	if err := s.eng.Alerts().TestWebhook(r.Context(), in.URL); err != nil {
		return nil, errStatus(http.StatusBadGateway, "%s", err.Error())
	}
	return map[string]any{"ok": true}, nil
}

func rawAlertExport(s *Server, w http.ResponseWriter, r *http.Request) {
	b, err := s.eng.Alerts().ExportRules()
	if !s.isAdmin(r) && err == nil {
		b, err = json.MarshalIndent(alerts.RedactRules(s.eng.Alerts().Rules()), "", "  ")
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="airview-alert-rules.json"`)
	writeBody(w, r, http.StatusOK, b, nil, "")
}

func apiAlertImport(s *Server, r *http.Request) (any, error) {
	raw, err := readBody(r, 4<<20)
	if err != nil {
		return nil, err
	}
	n, err := s.eng.Alerts().ImportRules(r.Context(), raw)
	if err != nil {
		return nil, alertErr(err)
	}
	return map[string]any{"imported": n}, nil
}

func apiAlertWatchlists(s *Server, r *http.Request) (any, error) {
	out := s.eng.Alerts().Watchlists()
	if out == nil {
		out = []alerts.WatchlistInfo{}
	}
	return out, nil
}

// rawAlertWatchlist is GET /api/alerts/watchlists/{name}: JSON entries, or a
// text / CSV download with format=txt|csv.
func rawAlertWatchlist(s *Server, w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	al := s.eng.Alerts()
	entries, ok := al.Watchlist(name)
	if !ok {
		writeError(w, http.StatusNotFound, "watch-list not found")
		return
	}
	switch f := qStr(r, "format"); f {
	case "", "json":
		if entries == nil {
			entries = []alerts.WatchEntry{}
		}
		b, err := marshal(entries)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeBody(w, r, http.StatusOK, b, nil, "")
	case "txt", "csv":
		var buf bytes.Buffer
		if err := al.ExportWatchlist(&buf, name, f); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ct := "text/plain; charset=utf-8"
		if f == "csv" {
			ct = "text/csv; charset=utf-8"
		}
		w.Header().Set("Content-Disposition", `attachment; filename="airview-watchlist-`+safeFileName(name)+`.`+f+`"`)
		writeBody(w, r, http.StatusOK, buf.Bytes(), nil, ct)
	default:
		writeError(w, http.StatusBadRequest, "parameter format: json | txt | csv")
	}
}

func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

func apiAlertWatchlistImport(s *Server, r *http.Request) (any, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 16<<20)
	n, rej, err := s.eng.Alerts().ImportWatchlist(r.Context(), r.PathValue("name"), r.Body, qBool(r, "replace", false))
	if err != nil {
		return nil, alertErr(err)
	}
	return map[string]any{"imported": n, "rejected": rej}, nil
}

func watchICAO(r *http.Request) (uint32, error) {
	key, err := parseICAOParam(r.PathValue("icao"))
	if err != nil {
		return 0, err
	}
	if key&modes.NonICAOFlag != 0 {
		return 0, badRequest("watch-lists hold ICAO addresses only")
	}
	return key, nil
}

func apiAlertWatchAdd(s *Server, r *http.Request) (any, error) {
	key, err := watchICAO(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(nil, r, &in, 8<<10); err != nil {
			return nil, err
		}
	}
	if err := s.eng.Alerts().AddWatch(r.Context(), r.PathValue("name"), key, in.Note); err != nil {
		return nil, alertErr(err)
	}
	return map[string]any{"ok": true}, nil
}

func apiAlertWatchRemove(s *Server, r *http.Request) (any, error) {
	key, err := watchICAO(r)
	if err != nil {
		return nil, err
	}
	if err := s.eng.Alerts().RemoveWatch(r.Context(), r.PathValue("name"), key); err != nil {
		return nil, alertErr(err)
	}
	return map[string]any{"ok": true}, nil
}

func apiAlertWatchlistDelete(s *Server, r *http.Request) (any, error) {
	if err := s.eng.Alerts().DeleteWatchlist(r.Context(), r.PathValue("name")); err != nil {
		return nil, alertErr(err)
	}
	return map[string]any{"ok": true}, nil
}
