package terrain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/radio"
)

var (
	errNoAnalysis = errors.New("terrain: no terrain analysis available yet")
	errNoAntenna  = errors.New("terrain: unknown antenna")
)

// Ground elevation sources.
const (
	GroundDEM    = "dem"    // site ground from the DEM (consistent with the terrain data; default)
	GroundConfig = "config" // model.Receiver.GroundElevM
)

// Request describes a terrain analysis. Zero values select defaults.
type Request struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
	// GroundElevM is the configured site elevation (model.Receiver.GroundElevM).
	GroundElevM float64 `json:"groundElevM"`
	// GroundSource is GroundDEM (default; falls back to GroundElevM when the
	// DEM has no data) or GroundConfig.
	GroundSource string `json:"groundSource"`
	// Antennas analysed (ID, Name, HeightAGLM and, for Radio, the RF chain).
	// Empty = one 10 m vertical.
	Antennas []model.Antenna `json:"antennas"`
	// K is the effective-earth factor (0 = 4/3).
	K float64 `json:"k"`
	// RadiusKm of the analysis (default 450).
	RadiusKm float64 `json:"radiusKm"`
	// BearingStepDeg (default 0.5).
	BearingStepDeg float64 `json:"bearingStepDeg"`
	// SampleKm along each ray (0 = one DEM pixel, at least 0.1 km).
	SampleKm float64 `json:"sampleKm"`
	// AltitudesFt for the coverage rings (default DefaultAltitudesFt).
	AltitudesFt []float64 `json:"altitudesFt"`
	// Zoom of the DEM tiles (0 = ChooseZoom with MaxTiles).
	Zoom     int `json:"zoom"`
	MaxTiles int `json:"maxTiles"`
	// MinObstacleKm (0 = DefaultMinObstacleKm).
	MinObstacleKm float64 `json:"minObstacleKm"`
	// Radio enables the link-budget (diffraction-extended) ranges with Link.
	Radio bool             `json:"radio"`
	Link  radio.LinkParams `json:"link"`
	// AdviceExtraM: extra antenna heights compared (default 0, 2, 5, 10).
	AdviceExtraM []float64 `json:"adviceExtraM"`
}

// RequestFromConfig builds a Request for the configured receiver and
// antennas. k = 0 selects 4/3 (pass the measured/forecast k-factor only if
// you want the coverage to follow the weather; every change recomputes).
func RequestFromConfig(rx model.Receiver, ants []model.Antenna, k float64) Request {
	return Request{Lat: rx.Lat, Lon: rx.Lon, GroundElevM: rx.GroundElevM, Antennas: ants, K: k, Radio: true}
}

func (r Request) normalized() Request {
	if r.GroundSource != GroundConfig {
		r.GroundSource = GroundDEM
	}
	if len(r.Antennas) == 0 {
		r.Antennas = []model.Antenna{{ID: "default", Name: "Antenna", Type: model.AntVertical, HeightAGLM: 10}}
	}
	ants := make([]model.Antenna, len(r.Antennas))
	copy(ants, r.Antennas)
	for i := range ants {
		if ants[i].ID == "" {
			ants[i].ID = fmt.Sprintf("ant%d", i+1)
		}
		if !(ants[i].HeightAGLM >= 0) {
			ants[i].HeightAGLM = 0
		}
	}
	r.Antennas = ants
	r.K = geo.EffectiveK(r.K)
	if !(r.RadiusKm > 0) {
		r.RadiusKm = 450
	}
	if r.RadiusKm > 1500 {
		r.RadiusKm = 1500
	}
	if !(r.BearingStepDeg > 0) {
		r.BearingStepDeg = 0.5
	}
	if len(r.AltitudesFt) == 0 {
		r.AltitudesFt = DefaultAltitudesFt
	}
	alts := append([]float64(nil), r.AltitudesFt...)
	sort.Float64s(alts)
	r.AltitudesFt = alts
	if r.MaxTiles <= 0 {
		r.MaxTiles = 300
	}
	if r.Zoom <= 0 {
		r.Zoom = ChooseZoom(r.RadiusKm, r.Lat, r.MaxTiles, 0, 0)
	}
	if r.Zoom > MaxTerrariumZoom {
		r.Zoom = MaxTerrariumZoom
	}
	if r.MinObstacleKm == 0 {
		r.MinObstacleKm = DefaultMinObstacleKm
	}
	if len(r.AdviceExtraM) == 0 {
		r.AdviceExtraM = DefaultAdviceExtraM
	}
	return r
}

// Key identifies the analysis result: it changes whenever the station
// position, antenna heights/RF chain, k or any analysis parameter changes.
func (r Request) Key() string {
	n := r.normalized()
	n.Lat, n.Lon = math.Round(n.Lat*1e5)/1e5, math.Round(n.Lon*1e5)/1e5
	b, _ := json.Marshal(n)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// polarKey identifies the polar terrain (independent of heights and k).
func polarKey(spec PolarSpec) string {
	s := fmt.Sprintf("v1|%.5f|%.5f|%.3f|%.6f|%.5f|%d", spec.Lat, spec.Lon, spec.RadiusKm, spec.BearingStepDeg, spec.SampleKm, spec.Zoom)
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// Job states.
const (
	StateIdle      = "idle"
	StateRunning   = "running"
	StateDone      = "done"
	StateError     = "error"
	StateCancelled = "cancelled"
)

// Job phases.
const (
	PhaseTiles    = "tiles"
	PhaseTerrain  = "terrain"
	PhaseCoverage = "coverage"
)

// JobStatus is the background job state (JSON-ready).
type JobStatus struct {
	State         string           `json:"state"`
	Phase         string           `json:"phase"`
	Progress      float64          `json:"progress"`      // 0..1 overall
	PhaseProgress float64          `json:"phaseProgress"` // 0..1 within the phase
	Tiles         PrefetchProgress `json:"tiles"`
	Message       string           `json:"message"`
	Error         string           `json:"error"`
	Key           string           `json:"key"`
	StartedMs     int64            `json:"startedMs"`
	FinishedMs    int64            `json:"finishedMs"`
	FromCache     bool             `json:"fromCache"` // polar terrain loaded from the disk cache
}

// ServiceOptions configure a Service.
type ServiceOptions struct {
	// Dir stores the polar-terrain cache (polar_<key>.bin.gz). Empty = no
	// result cache.
	Dir string
	// Provider supplies (and downloads) the DEM tiles. Required unless
	// Source is set.
	Provider *Provider
	// Source overrides the DEM source (tests, demo); no tiles are fetched.
	Source Source
	// KeepPolarFiles is the number of polar cache files kept (default 6).
	KeepPolarFiles int
	// KeepMemTiles is the number of decoded DEM tiles left in the
	// provider's memory cache after a job (default 0: all released; tiles
	// are re-read from disk by later LOS / elevation queries).
	KeepMemTiles int
	// OnJobDone, when set, is called (outside any lock, on the job
	// goroutine) after every job ended, successfully or not, once the job's
	// intermediates (polar terrain, extra panoramas, decoded tiles) have
	// been dropped. The job runner can return the memory to the OS here,
	// e.g. func(JobStatus) { debug.FreeOSMemory() } on a Raspberry Pi; the
	// library itself never forces a GC.
	OnJobDone func(JobStatus)
	Logger    *slog.Logger
}

type jobReq struct {
	req   Request
	force bool
}

// Service runs the terrain analysis as a cancellable background job and
// serves the latest result. Create with NewService, start Run(ctx) in a
// goroutine, then Submit requests whenever the station configuration changes.
type Service struct {
	opt ServiceOptions
	log *slog.Logger

	reqCh chan jobReq

	mu         sync.Mutex
	status     JobStatus
	runningKey string
	cancelJob  context.CancelFunc
	lastReq    *Request

	analysis atomic.Pointer[Analysis]

	// DEM downloads triggered by HTTP requests (http.go).
	httpOnce sync.Once
	httpSem  chan struct{}
}

// NewService creates a service.
func NewService(o ServiceOptions) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.KeepPolarFiles <= 0 {
		o.KeepPolarFiles = 6
	}
	return &Service{opt: o, log: o.Logger.With("component", "terrain"), reqCh: make(chan jobReq, 1),
		status: JobStatus{State: StateIdle}}
}

// Provider returns the tile provider (may be nil).
func (s *Service) Provider() *Provider { return s.opt.Provider }

// Submit queues an analysis. It is a no-op (returns false) when the same
// request is already running or its result is current, unless force is set.
// A newer request replaces a queued one and cancels a running job.
func (s *Service) Submit(req Request, force bool) bool {
	key := req.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := req
	s.lastReq = &r
	if !force {
		if s.runningKey == key {
			return false
		}
		if a := s.analysis.Load(); a != nil && a.Key == key && s.runningKey == "" {
			return false
		}
	}
	select {
	case <-s.reqCh:
	default:
	}
	s.reqCh <- jobReq{req: req, force: force}
	if s.cancelJob != nil {
		s.cancelJob()
	}
	return true
}

// Cancel stops the running job (the previous result stays available).
func (s *Service) Cancel() {
	s.mu.Lock()
	select {
	case <-s.reqCh:
	default:
	}
	if s.cancelJob != nil {
		s.cancelJob()
	}
	s.mu.Unlock()
}

// Status returns the job status.
func (s *Service) Status() JobStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Analysis returns the latest result (nil until the first job finished).
func (s *Service) Analysis() *Analysis { return s.analysis.Load() }

// LastRequest returns the last submitted request.
func (s *Service) LastRequest() (Request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastReq == nil {
		return Request{}, false
	}
	return *s.lastReq, true
}

// Visible is Analysis().Visible, nil-safe.
func (s *Service) Visible(antID string, lat, lon, altFt float64) Visibility {
	return s.analysis.Load().Visible(antID, lat, lon, altFt)
}

// Forecast is Analysis().Forecast, nil-safe.
func (s *Service) Forecast(antID string, path []PathPoint) VisibilityForecast {
	return s.analysis.Load().Forecast(antID, path)
}

// Run processes submitted requests until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	var done chan struct{}
	for {
		select {
		case <-ctx.Done():
			s.Cancel()
			if done != nil {
				<-done
			}
			return ctx.Err()
		case jr := <-s.reqCh:
			if done != nil {
				<-done // the job was cancelled by Submit
			}
			key := jr.req.Key()
			if a := s.analysis.Load(); !jr.force && a != nil && a.Key == key {
				done = nil
				continue
			}
			jctx, cancel := context.WithCancel(ctx)
			s.mu.Lock()
			s.runningKey, s.cancelJob = key, cancel
			s.status = JobStatus{State: StateRunning, Key: key, StartedMs: time.Now().UnixMilli()}
			s.mu.Unlock()
			done = make(chan struct{})
			go func(req Request, force bool, done chan struct{}) {
				defer close(done)
				defer cancel()
				a, err := s.run(jctx, req, force)
				s.releaseJobMemory()
				final := s.finishJob(key, a, err)
				if s.opt.OnJobDone != nil {
					s.opt.OnJobDone(final)
				}
			}(jr.req, jr.force, done)
		}
	}
}

// releaseJobMemory drops what an analysis job leaves behind besides its
// result: the decoded DEM tiles in the provider's memory cache (the polar
// terrain and the advice panoramas are locals of run and already
// unreachable).
func (s *Service) releaseJobMemory() {
	if p := s.opt.Provider; p != nil && s.opt.Source == nil {
		if freed := p.TrimMemory(s.opt.KeepMemTiles); freed > 0 {
			s.log.Debug("terrain: released decoded tiles", "bytes", freed)
		}
	}
}

// finishJob records the outcome of a job and returns the final status.
func (s *Service) finishJob(key string, a *Analysis, err error) JobStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runningKey == key {
		s.runningKey, s.cancelJob = "", nil
	}
	s.status.FinishedMs = time.Now().UnixMilli()
	switch {
	case err == nil:
		s.analysis.Store(a)
		s.status.State, s.status.Progress, s.status.PhaseProgress, s.status.Message = StateDone, 1, 1, "terrain analysis complete"
		s.log.Info("terrain: analysis complete", "key", key, "ms", a.ComputeMs, "noData", a.NoDataFraction)
	case errors.Is(err, context.Canceled):
		s.status.State, s.status.Message = StateCancelled, "cancelled"
	default:
		s.status.State, s.status.Error = StateError, err.Error()
		s.log.Warn("terrain: analysis failed", "err", err)
	}
	return s.status
}

func (s *Service) setPhase(phase string, overall0, overall1, frac float64, msg string) {
	s.mu.Lock()
	s.status.Phase = phase
	s.status.PhaseProgress = math.Max(0, math.Min(1, frac))
	s.status.Progress = overall0 + (overall1-overall0)*s.status.PhaseProgress
	if msg != "" {
		s.status.Message = msg
	}
	s.mu.Unlock()
}

// source returns the DEM source at zoom z and whether tiles are fetched.
func (s *Service) source(z int) Source {
	if s.opt.Source != nil {
		return s.opt.Source
	}
	if s.opt.Provider != nil {
		return s.opt.Provider.At(z)
	}
	return NoDataSource{}
}

// siteZoom is the zoom used for the DEM ground elevation at the site.
func siteZoom(z int) int {
	sz := z + 3
	if sz > 13 {
		sz = 13
	}
	if sz < z {
		sz = z
	}
	return sz
}

// run executes one analysis.
func (s *Service) run(ctx context.Context, req Request, force bool) (*Analysis, error) {
	t0 := time.Now()
	req = req.normalized()
	if !geo.ValidLatLon(req.Lat, req.Lon) {
		return nil, errors.New("terrain: invalid station position")
	}
	key := req.Key()
	p := s.opt.Provider
	useTiles := s.opt.Source == nil && p != nil

	// Phase 1: tiles.
	if useTiles {
		s.setPhase(PhaseTiles, 0, 0.5, 0, "downloading terrain tiles")
		_, _, _ = p.Fetch(ctx, TileAt(req.Lat, req.Lon, siteZoom(req.Zoom)))
		pr, err := p.Prefetch(ctx, req.Lat, req.Lon, req.RadiusKm, req.Zoom, func(pp PrefetchProgress) {
			s.mu.Lock()
			s.status.Tiles = pp
			s.mu.Unlock()
			if pp.Total > 0 {
				s.setPhase(PhaseTiles, 0, 0.5, float64(pp.Done)/float64(pp.Total), "")
			}
		})
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.status.Tiles = pr
		s.mu.Unlock()
		if pr.Failed > 0 {
			s.log.Warn("terrain: some tiles unavailable, using sea level there", "failed", pr.Failed, "total", pr.Total)
		}
	}
	src := s.source(req.Zoom)

	// Site ground.
	a := &Analysis{Key: key, Lat: req.Lat, Lon: req.Lon, K: req.K, Zoom: req.Zoom, AltitudesFt: req.AltitudesFt,
		Attribution: AttributionShort, AttributionURL: AttributionURL, GroundM: req.GroundElevM, GroundSource: GroundConfig}
	if e, ok := s.source(siteZoom(req.Zoom)).ElevationM(req.Lat, req.Lon); ok {
		a.DEMGroundM, a.DEMGroundOK = round(e, 1), true
		if req.GroundSource == GroundDEM {
			a.GroundM, a.GroundSource = e, GroundDEM
		}
	}

	// Phase 2: polar terrain (cached on disk).
	s.setPhase(PhaseTerrain, 0.5, 0.9, 0, "sampling terrain")
	spec := PolarSpec{Lat: req.Lat, Lon: req.Lon, RadiusKm: req.RadiusKm, BearingStepDeg: req.BearingStepDeg,
		SampleKm: req.SampleKm, Zoom: req.Zoom}.normalize(src)
	pk := polarKey(spec)
	var pt *PolarTerrain
	if !force {
		pt = s.loadPolar(pk)
	}
	if pt != nil {
		s.mu.Lock()
		s.status.FromCache = true
		s.mu.Unlock()
	} else {
		var err error
		pt, err = SamplePolar(ctx, src, spec, func(done, total int) {
			s.setPhase(PhaseTerrain, 0.5, 0.9, float64(done)/float64(total), "")
		})
		if err != nil {
			return nil, err
		}
		if pt.NoData == 0 {
			s.savePolar(pk, pt)
		}
	}
	a.RadiusKm, a.StepDeg, a.SampleKm = round(pt.Spec.RadiusKm, 3), pt.Spec.BearingStepDeg, round(pt.Spec.SampleKm, 4)
	a.NoDataFraction = round(pt.NoDataFraction(), 5)
	a.DataComplete = pt.NoData == 0

	// Phase 3: panoramas, ranges, advice.
	s.setPhase(PhaseCoverage, 0.9, 1, 0, "computing coverage")
	n := len(req.Antennas)
	for i, ant := range req.Antennas {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hm := a.GroundM + ant.HeightAGLM
		pano := NewPanorama(pt, hm, req.K, req.MinObstacleKm)
		c := &AntennaCoverage{ID: ant.ID, Name: ant.Name, HeightAGLM: ant.HeightAGLM, AntennaM: round(hm, 2), pano: pano}
		c.Horizon = pano.Horizon()
		c.Horizon.AntennaID = ant.ID
		c.LOS = pano.LOSRanges(req.AltitudesFt)
		if req.Radio {
			lp := req.Link
			lp.K = req.K
			link := radio.NewLink(ant, hm, lp)
			c.Radio = pano.RadioRanges(link, req.AltitudesFt)
			c.SensitivityDBm = round(link.SensitivityDBm, 2)
		}
		a.Antennas = append(a.Antennas, c)
		s.setPhase(PhaseCoverage, 0.9, 1, float64(i+1)/float64(n+1), "")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Site advice per antenna; antennas at the same height share one
	// computation (the advice depends only on the feed-point height).
	for i, c := range a.Antennas {
		for _, prev := range a.Antennas[:i] {
			if prev.AntennaM == c.AntennaM && prev.advice != nil {
				cp := *prev.advice
				cp.AntennaID = c.ID
				c.advice = &cp
				break
			}
		}
		if c.advice == nil {
			c.advice = computeAdvice(pt, c, req.K, req.MinObstacleKm, req.AltitudesFt, req.AdviceExtraM)
		}
	}
	a.Advice = a.Antennas[0].advice
	a.GroundM = round(a.GroundM, 1)
	a.ComputedMs = time.Now().UnixMilli()
	a.ComputeMs = time.Since(t0).Milliseconds()
	return a, nil
}

func (s *Service) polarPath(key string) string {
	return filepath.Join(s.opt.Dir, "polar_"+key+".bin.gz")
}

func (s *Service) loadPolar(key string) *PolarTerrain {
	if s.opt.Dir == "" {
		return nil
	}
	f, err := os.Open(s.polarPath(key))
	if err != nil {
		return nil
	}
	defer f.Close()
	pt, err := ReadPolarTerrain(f)
	if err != nil {
		s.log.Warn("terrain: polar cache unreadable, recomputing", "err", err)
		return nil
	}
	now := time.Now()
	_ = os.Chtimes(s.polarPath(key), now, now)
	return pt
}

func (s *Service) savePolar(key string, pt *PolarTerrain) {
	if s.opt.Dir == "" {
		return
	}
	if err := os.MkdirAll(s.opt.Dir, 0o755); err != nil {
		s.log.Warn("terrain: cannot create cache dir", "err", err)
		return
	}
	path := s.polarPath(key)
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		s.log.Warn("terrain: cannot write polar cache", "err", err)
		return
	}
	_, werr := pt.WriteTo(f)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	s.prunePolar()
}

func (s *Service) prunePolar() {
	ents, err := os.ReadDir(s.opt.Dir)
	if err != nil {
		return
	}
	type fi struct {
		name string
		t    time.Time
	}
	var files []fi
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "polar_") || !strings.HasSuffix(e.Name(), ".bin.gz") {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, fi{e.Name(), info.ModTime()})
		}
	}
	if len(files) <= s.opt.KeepPolarFiles {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].t.After(files[j].t) })
	for _, f := range files[s.opt.KeepPolarFiles:] {
		_ = os.Remove(filepath.Join(s.opt.Dir, f.name))
	}
}

// ---------------------------------------------------------------------------
// On-demand helpers

// ElevationM returns the DEM elevation at a point, fetching the tile if
// needed (zoom 0 = 12).
func (s *Service) ElevationM(ctx context.Context, lat, lon float64, z int) (float64, bool, int) {
	return s.elevationM(ctx, lat, lon, z, true)
}

// elevationM is ElevationM; fetch=false uses only tiles already available.
func (s *Service) elevationM(ctx context.Context, lat, lon float64, z int, fetch bool) (float64, bool, int) {
	if z <= 0 {
		z = 12
	}
	if z > MaxTerrariumZoom {
		z = MaxTerrariumZoom
	}
	if fetch && s.opt.Source == nil && s.opt.Provider != nil {
		_, _, _ = s.opt.Provider.Fetch(ctx, TileAt(lat, lon, z))
	}
	e, ok := s.source(z).ElevationM(lat, lon)
	return e, ok, z
}

// LOS analyses the path from antenna antID ("" = first) of the current
// analysis to a target at altFt AMSL, fetching the DEM tiles along the path
// when needed.
func (s *Service) LOS(ctx context.Context, antID string, lat, lon, altFt float64, keepProfile bool) (LOSResult, error) {
	return s.LOSWithK(ctx, antID, lat, lon, altFt, 0, keepProfile)
}

// Effective earth radius factors accepted by LOSWithK.
const (
	MinLOSK = 0.5
	MaxLOSK = 10
)

// LOSWithK is LOS with another effective earth radius factor k (clamped to
// MinLOSK..MaxLOSK; 0 or NaN = the analysis' k).
func (s *Service) LOSWithK(ctx context.Context, antID string, lat, lon, altFt, k float64, keepProfile bool) (LOSResult, error) {
	a := s.analysis.Load()
	if a == nil {
		return LOSResult{}, errNoAnalysis
	}
	c := a.Antenna(antID)
	if c == nil {
		return LOSResult{}, errNoAntenna
	}
	if !geo.ValidLatLon(lat, lon) || math.IsNaN(altFt) {
		return LOSResult{}, errors.New("terrain: invalid target position")
	}
	z := a.Zoom
	if sphereKm(a.Lat, a.Lon, lat, lon) < 150 && z < 11 {
		z = 11
	}
	if s.opt.Source == nil && s.opt.Provider != nil {
		if _, err := s.opt.Provider.PrefetchPath(ctx, a.Lat, a.Lon, lat, lon, z); err != nil {
			return LOSResult{}, err
		}
	}
	req, _ := s.LastRequest()
	var gas *radio.GasModel
	if req.Link.Gas != (radio.GasModel{}) {
		g := req.Link.Gas
		gas = &g
	}
	if k == 0 || math.IsNaN(k) {
		k = a.K
	} else {
		k = math.Max(MinLOSK, math.Min(MaxLOSK, k))
	}
	return LOS(s.source(z), LOSInput{
		RxLat: a.Lat, RxLon: a.Lon, RxGroundM: a.GroundM, RxAGLM: c.HeightAGLM,
		TgtLat: lat, TgtLon: lon, TgtAltM: altFt * geo.MetresPerFoot, K: k,
		MinObstacleKm: c.pano.MinObstacleKm, Gas: gas, KeepProfile: keepProfile,
	}), nil
}
