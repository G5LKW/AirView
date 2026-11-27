package terrain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"airview/internal/geo"
)

// Route is one HTTP endpoint of the terrain API. Admin routes mutate state
// and must be wrapped with the server's admin-auth middleware.
type Route struct {
	Method  string
	Path    string
	Admin   bool
	Handler http.HandlerFunc
}

// Routes returns the terrain HTTP API (see WIRING.md). All responses are
// JSON except floor.png; errors are {"error": "..."} with an HTTP status.
func (s *Service) Routes() []Route {
	return []Route{
		{http.MethodGet, "/api/terrain/status", false, s.handleStatus},
		{http.MethodGet, "/api/terrain/coverage", false, s.handleCoverage},
		{http.MethodGet, "/api/terrain/coverage.geojson", false, s.handleGeoJSON},
		{http.MethodGet, "/api/terrain/horizon", false, s.handleHorizon},
		{http.MethodGet, "/api/terrain/advice", false, s.handleAdvice},
		{http.MethodGet, "/api/terrain/floor", false, s.handleFloor},
		{http.MethodGet, "/api/terrain/floor.png", false, s.handleFloorPNG},
		{http.MethodGet, "/api/terrain/floor-image", false, s.handleFloorImage},
		{http.MethodGet, "/api/terrain/visible", false, s.handleVisible},
		{http.MethodGet, "/api/terrain/los", false, s.handleLOS},
		{http.MethodGet, "/api/terrain/elevation", false, s.handleElevation},
		{http.MethodPost, "/api/terrain/recompute", true, s.handleRecompute},
		{http.MethodPost, "/api/terrain/cancel", true, s.handleCancel},
	}
}

// Mount registers all routes on mux using Go 1.22 method patterns. admin
// wraps the mutating routes (nil = no wrapping).
func (s *Service) Mount(mux *http.ServeMux, admin func(http.Handler) http.Handler) {
	for _, r := range s.Routes() {
		var h http.Handler = r.Handler
		if r.Admin && admin != nil {
			h = admin(h)
		}
		mux.Handle(r.Method+" "+r.Path, h)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func qFloat(r *http.Request, name string) (float64, bool) {
	v, err := strconv.ParseFloat(r.URL.Query().Get(name), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// StatusResponse is the body of GET /api/terrain/status.
type StatusResponse struct {
	Job            JobStatus      `json:"job"`
	Available      bool           `json:"available"` // an analysis result exists
	Key            string         `json:"key"`       // its key
	ComputedMs     int64          `json:"computedMs"`
	DataComplete   bool           `json:"dataComplete"`
	Provider       *ProviderStats `json:"provider"`
	Attribution    string         `json:"attribution"`
	AttributionURL string         `json:"attributionUrl"`
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := StatusResponse{Job: s.Status(), Attribution: AttributionShort, AttributionURL: AttributionURL}
	if a := s.Analysis(); a != nil {
		resp.Available, resp.Key, resp.ComputedMs, resp.DataComplete = true, a.Key, a.ComputedMs, a.DataComplete
	}
	if p := s.opt.Provider; p != nil {
		st := p.Stats()
		resp.Provider = &st
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Service) needAnalysis(w http.ResponseWriter) *Analysis {
	a := s.Analysis()
	if a == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoAnalysis)
	}
	return a
}

// coverageView is the Analysis with the advice of a chosen antenna.
type coverageView struct {
	*Analysis
	Advice *SiteAdvice `json:"advice"`
}

func (s *Service) handleCoverage(w http.ResponseWriter, r *http.Request) {
	a := s.needAnalysis(w)
	if a == nil {
		return
	}
	ant := r.URL.Query().Get("ant")
	if ant == "" {
		writeJSON(w, http.StatusOK, a)
		return
	}
	adv := a.AdviceFor(ant)
	if adv == nil {
		writeErr(w, http.StatusNotFound, errNoAntenna)
		return
	}
	writeJSON(w, http.StatusOK, coverageView{Analysis: a, Advice: adv})
}

func (s *Service) handleGeoJSON(w http.ResponseWriter, r *http.Request) {
	a := s.needAnalysis(w)
	if a == nil {
		return
	}
	ant := r.URL.Query().Get("ant")
	if a.Antenna(ant) == nil {
		writeErr(w, http.StatusNotFound, errNoAntenna)
		return
	}
	w.Header().Set("Content-Type", "application/geo+json")
	fc := a.GeoJSON(ant, r.URL.Query().Get("kind"))
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(fc)
}

func (s *Service) handleHorizon(w http.ResponseWriter, r *http.Request) {
	a := s.needAnalysis(w)
	if a == nil {
		return
	}
	c := a.Antenna(r.URL.Query().Get("ant"))
	if c == nil {
		writeErr(w, http.StatusNotFound, errNoAntenna)
		return
	}
	writeJSON(w, http.StatusOK, c.Horizon)
}

func (s *Service) handleAdvice(w http.ResponseWriter, r *http.Request) {
	a := s.needAnalysis(w)
	if a == nil {
		return
	}
	adv := a.AdviceFor(r.URL.Query().Get("ant"))
	if adv == nil {
		writeErr(w, http.StatusNotFound, errNoAntenna)
		return
	}
	writeJSON(w, http.StatusOK, adv)
}

func (s *Service) handleFloor(w http.ResponseWriter, r *http.Request) {
	a := s.needAnalysis(w)
	if a == nil {
		return
	}
	ant := r.URL.Query().Get("ant")
	if ant != "" && a.Antenna(ant) == nil {
		writeErr(w, http.StatusNotFound, errNoAntenna)
		return
	}
	cell, _ := qFloat(r, "cell")
	writeJSON(w, http.StatusOK, a.FloorGrid(ant, cell))
}

func (s *Service) floorImage(w http.ResponseWriter, r *http.Request) *FloorImage {
	a := s.needAnalysis(w)
	if a == nil {
		return nil
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	im, err := a.FloorImage(r.URL.Query().Get("ant"), width)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil
	}
	return im
}

func (s *Service) handleFloorPNG(w http.ResponseWriter, r *http.Request) {
	if im := s.floorImage(w, r); im != nil {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(im.PNG)
	}
}

// FloorImageResponse is the body of GET /api/terrain/floor-image.
type FloorImageResponse struct {
	URL string `json:"url"` // floor.png URL with the same parameters
	*FloorImage
	Key string `json:"key"`
}

func (s *Service) handleFloorImage(w http.ResponseWriter, r *http.Request) {
	im := s.floorImage(w, r)
	if im == nil {
		return
	}
	a := s.Analysis()
	q := r.URL.Query()
	q.Set("k", a.Key) // cache buster
	writeJSON(w, http.StatusOK, FloorImageResponse{URL: "/api/terrain/floor.png?" + q.Encode(), FloorImage: im, Key: a.Key})
}

func (s *Service) handleVisible(w http.ResponseWriter, r *http.Request) {
	lat, ok1 := qFloat(r, "lat")
	lon, ok2 := qFloat(r, "lon")
	alt, ok3 := qFloat(r, "altFt")
	if !ok1 || !ok2 || !ok3 || !geo.ValidLatLon(lat, lon) {
		writeErr(w, http.StatusBadRequest, errors.New("lat, lon and altFt are required"))
		return
	}
	if s.Analysis() == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoAnalysis)
		return
	}
	writeJSON(w, http.StatusOK, s.Visible(r.URL.Query().Get("ant"), lat, lon, alt))
}

func (s *Service) handleLOS(w http.ResponseWriter, r *http.Request) {
	lat, ok1 := qFloat(r, "lat")
	lon, ok2 := qFloat(r, "lon")
	alt, ok3 := qFloat(r, "altFt")
	if !ok1 || !ok2 || !ok3 || !geo.ValidLatLon(lat, lon) {
		writeErr(w, http.StatusBadRequest, errors.New("lat, lon and altFt are required"))
		return
	}
	k := 0.0 // the analysis' k
	if r.URL.Query().Get("k") != "" {
		v, ok := qFloat(r, "k")
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			writeErr(w, http.StatusBadRequest, errors.New("k must be a number (0.5..10)"))
			return
		}
		k = math.Max(MinLOSK, math.Min(MaxLOSK, v))
	}
	if a := s.Analysis(); a != nil && sphereKm(a.Lat, a.Lon, lat, lon) > a.RadiusKm+HTTPMarginKm {
		// Each request downloads the DEM tiles along the path: keep requests
		// from the network to the analysed area.
		writeErr(w, http.StatusBadRequest, fmt.Errorf("target is beyond the terrain analysis radius (%.0f km)", a.RadiusKm+HTTPMarginKm))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if !s.acquireHTTPFetch(ctx) {
		writeErr(w, http.StatusServiceUnavailable, errors.New("terrain service busy, try again"))
		return
	}
	defer s.releaseHTTPFetch()
	res, err := s.LOSWithK(ctx, r.URL.Query().Get("ant"), lat, lon, alt, k, r.URL.Query().Get("profile") != "0")
	switch {
	case errors.Is(err, errNoAnalysis):
		writeErr(w, http.StatusServiceUnavailable, err)
	case errors.Is(err, errNoAntenna):
		writeErr(w, http.StatusNotFound, err)
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// ElevationResponse is the body of GET /api/terrain/elevation.
type ElevationResponse struct {
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	ElevM float64 `json:"elevM"`
	OK    bool    `json:"ok"` // false = no data (sea level assumed)
	Zoom  int     `json:"zoom"`
}

func (s *Service) handleElevation(w http.ResponseWriter, r *http.Request) {
	lat, ok1 := qFloat(r, "lat")
	lon, ok2 := qFloat(r, "lon")
	if !ok1 || !ok2 || !geo.ValidLatLon(lat, lon) {
		writeErr(w, http.StatusBadRequest, errors.New("lat and lon are required"))
		return
	}
	z, _ := strconv.Atoi(r.URL.Query().Get("z"))
	if z > HTTPMaxElevationZoom {
		z = HTTPMaxElevationZoom
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// Tiles are downloaded only inside the analysed area; elsewhere the
	// answer comes from tiles already on disk (ok=false when none).
	a := s.Analysis()
	fetch := a != nil && sphereKm(a.Lat, a.Lon, lat, lon) <= a.RadiusKm+HTTPMarginKm
	if fetch {
		if !s.acquireHTTPFetch(ctx) {
			writeErr(w, http.StatusServiceUnavailable, errors.New("terrain service busy, try again"))
			return
		}
		defer s.releaseHTTPFetch()
	}
	e, ok, zz := s.elevationM(ctx, lat, lon, z, fetch)
	writeJSON(w, http.StatusOK, ElevationResponse{Lat: lat, Lon: lon, ElevM: round(e, 1), OK: ok, Zoom: zz})
}

// Limits for DEM downloads triggered by the HTTP API (any LAN client can
// call /api/terrain/los and /api/terrain/elevation).
const (
	// HTTPMarginKm is added to the analysis radius: LOS targets and
	// elevation look-ups that download tiles must lie inside it.
	HTTPMarginKm = 100
	// HTTPMaxElevationZoom caps ?z= of /api/terrain/elevation.
	HTTPMaxElevationZoom = 12
	// httpFetchSlots is how many such requests may download at once.
	httpFetchSlots = 2
)

func (s *Service) acquireHTTPFetch(ctx context.Context) bool {
	s.httpOnce.Do(func() { s.httpSem = make(chan struct{}, httpFetchSlots) })
	select {
	case s.httpSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Service) releaseHTTPFetch() { <-s.httpSem }

func (s *Service) handleRecompute(w http.ResponseWriter, r *http.Request) {
	req, ok := s.LastRequest()
	if !ok {
		writeErr(w, http.StatusConflict, errors.New("no terrain request has been submitted yet"))
		return
	}
	if s.opt.Provider != nil {
		s.opt.Provider.ResetBackoff()
	}
	started := s.Submit(req, true)
	writeJSON(w, http.StatusAccepted, map[string]any{"started": started, "job": s.Status()})
}

func (s *Service) handleCancel(w http.ResponseWriter, r *http.Request) {
	s.Cancel()
	writeJSON(w, http.StatusOK, s.Status())
}
