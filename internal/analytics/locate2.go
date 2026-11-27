package analytics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"time"

	"airview/internal/geo"
	"airview/internal/terrain"
)

// L2Sample is one reception sample for Locate Me v2: an ADS-B position the
// station received off-air with the signal level it was received at
// (store.GuessSample without the station).
type L2Sample struct {
	T      int64   // Unix ms
	ICAO   uint32  // address key
	Lat    float64 // aircraft position, degrees
	Lon    float64
	AltFt  float64 // barometric altitude ft (NaN = unknown)
	RSSI   float64 // dBFS (NaN = unknown)
	Ch     int     // RF channel (-1 = unknown)
	Ground bool    // surface position
}

// L2TimingPoint is one T=1 airborne position received with a GPS time
// stamp: MNs is the time of arrival minus the 0.2 s UTC epoch the position
// applies to (ns), i.e. the transmitter's latency plus the propagation time.
type L2TimingPoint struct {
	Lat, Lon float64
	AltM     float64 // aircraft height, m MSL
	MNs      float64
}

// L2TimingTrack is the T=1 timing history of one aircraft.
type L2TimingTrack struct {
	ICAO   uint32
	Points []L2TimingPoint
}

// Locate2Options configures Locate2.
type Locate2Options struct {
	// Terrain is the elevation model (nil = smooth earth). Only data that is
	// already available is read (terrain.Provider reads its caches only);
	// when too much of the search area has no data the terrain term is
	// switched off and the result says so.
	Terrain terrain.Source
	// Timing holds T=1 timing tracks of a GPS-stamping receiver (optional).
	Timing []L2TimingTrack
	// PriorLat/PriorLon centre the search (both 0 = derived from the data).
	PriorLat, PriorLon float64
	// SearchKm is the radius of the first search level (0 = 200 km).
	SearchKm float64
	// K is the effective earth radius factor (0 = 4/3).
	K float64
	// Workers bounds the goroutines (0 = GOMAXPROCS).
	Workers int
}

// L2Term describes one likelihood term of a Locate Me v2 result.
type L2Term struct {
	Name   string `json:"name"` // horizon, endpoints, signal, timing
	Used   bool   `json:"used"`
	N      int    `json:"n"` // samples / sessions / aircraft it used at the final level
	Detail string `json:"detail"`
	// DeltaLL is the term's log-likelihood at the estimate minus its value
	// at the search centre: how strongly it pulled the estimate.
	DeltaLL Num `json:"deltaLL"`
}

// L2Level is one level of the grid search.
type L2Level struct {
	StepKm   float64 `json:"stepKm"`
	Nodes    int     `json:"nodes"`
	HalfKm   float64 `json:"halfKm"` // half width of the searched box
	Horizon  int     `json:"horizon"`
	Signal   int     `json:"signal"`
	Endpoint int     `json:"endpoints"`
}

// Locate2Result is a Locate Me v2 position estimate.
type Locate2Result struct {
	OK     bool    `json:"ok"`
	Reason string  `json:"reason,omitempty"`
	Lat    float64 `json:"lat"` // posterior mean
	Lon    float64 `json:"lon"`
	MAPLat float64 `json:"mapLat"` // highest posterior node
	MAPLon float64 `json:"mapLon"`
	// SigmaKm is the 1-sigma radial uncertainty sqrt((aÂ²+bÂ²)/2) of the
	// posterior; R95Km the radius of the accuracy circle (95 % of the
	// posterior mass for its major axis); MajorKm/MinorKm/AxisDeg the 1-sigma
	// ellipse (axis = bearing of the major axis).
	SigmaKm Num `json:"sigmaKm"`
	R95Km   Num `json:"r95Km"`
	MajorKm Num `json:"majorKm"`
	MinorKm Num `json:"minorKm"`
	AxisDeg Num `json:"axisDeg"`
	// AntennaM is the antenna height that explains the data best: above
	// ground with terrain data, above the surrounding terrain without.
	AntennaM Num `json:"antennaM"`

	Samples  int  `json:"samples"`
	Sessions int  `json:"sessions"`
	Aircraft int  `json:"aircraft"`
	Terrain  bool `json:"terrain"`
	// Source names where the samples came from ("guess_samples" or
	// "positions"); set by the Analyzer.
	Source    string       `json:"source,omitempty"`
	Terms     []L2Term     `json:"terms"`
	Levels    []L2Level    `json:"levels"`
	Surface   *CostSurface `json:"surface,omitempty"`
	ElapsedMs int64        `json:"elapsedMs"`
	Notes     []string     `json:"notes"`
	// ErrorKm is the distance to the reference position (GNSS or configured;
	// NaN = none), set by the Analyzer.
	ErrorKm Num `json:"errorKm"`
	// SkylineRaised is the share of directions (10° sectors with session
	// ends) whose skyline the fit moved off the model horizon: terrain or
	// obstructions the model does not know (null = too few sectors).
	SkylineRaised Num `json:"skylineRaised"`
	// RefSource names the reference of ErrorKm (gnss, configured, survey).
	RefSource string `json:"refSource,omitempty"`
}

// Tuning of the likelihoods. Every term is a log-likelihood divided by a
// "temperature" that accounts for the correlation between samples (one
// aircraft session, the same airway, the same hour of tropospheric
// conditions). Together with l2Inflation they make the 95 % circle cover
// the truth in most simulated sites (TestLocate2Eval, locate2_doc.go).
type l2Tuning struct {
	sessionGapMs  int64
	offlineGapMs  int64
	endGuardMs    int64
	violFreeDeg   float64 // reception this far below the horizon costs nothing (diffraction, refraction)
	violScaleDeg  float64 // horizon violation scale beyond that
	violCap       float64 // per-sample penalty cap
	fixedSkyline  bool    // endpoint term without the skyline fit
	skyPen        float64 // tempered prior cost of a sector skyline off the model horizon
	altErrM       float64 // altitude error allowed as elevation slack (barometric vs geometric)
	tauHorizon    float64
	endPi         float64 // share of session ends caused by the horizon
	endSigmaDeg   float64
	tauEnd        float64
	endMaxAltFt   float64
	rssiMaxKm     float64
	rssiMaxN      int
	tauRSSI       float64
	tauTiming     float64
	timingSigmaNs float64
	horizonMaxN   int
}

var l2Default = l2Tuning{
	sessionGapMs:  300_000,
	offlineGapMs:  180_000,
	endGuardMs:    120_000,
	violFreeDeg:   0.2,
	violScaleDeg:  0.5,
	violCap:       2,
	altErrM:       200,
	skyPen:        2,
	tauHorizon:    4,
	endPi:         0.5,
	endSigmaDeg:   0.35,
	tauEnd:        3,
	endMaxAltFt:   25000,
	rssiMaxKm:     120,
	rssiMaxN:      30000,
	tauRSSI:       12,
	tauTiming:     8,
	timingSigmaNs: 5000,
	horizonMaxN:   60000,
}

const (
	l2MinSamples  = 300
	l2MinSessions = 15
	l2PanoKm      = 30.0
	l2PanoRays    = 120
	l2PanoD0Km    = 0.2
	l2PanoQ       = 1.1
	l2DEMStepKm   = 0.25
	l2DEMCoarseKm = 1.0
	lightMPerNs   = 0.299792458 / 1.0003 // radio speed in air, m/ns
	rad2deg       = 180 / math.Pi
	earthM        = geo.EarthRadiusKm * 1000
)

// Margin histogram of the endpoint term: l2HistBins bins of l2HistW
// degrees from l2HistLo (margins outside fall into the end bins).
const (
	l2HistLo   = -3.0
	l2HistW    = 0.1
	l2HistBins = 150
)

func l2HistBin(u float64) int {
	b := int(math.Floor((u - l2HistLo) / l2HistW))
	if b < 0 {
		return 0
	}
	if b >= l2HistBins {
		return l2HistBins - 1
	}
	return b
}

// errL2Cancelled is returned when the context ends during the search.
var errL2Cancelled = errors.New("locate2: cancelled")

// Locate2 estimates the station position from reception samples alone
// (DESIGN Â§10.4): a grid posterior that multiplies
//
//   - horizon: every heard aircraft must be above the radio horizon /
//     terrain panorama of the candidate position (soft, bounded penalty);
//     surface positions make airport line-of-sight discs;
//   - endpoints: low aircraft that appear or vanish in mid-air do so near
//     the skyline (mixture of "horizon" and "other cause", the latter with
//     the margin density of all heard low aircraft as reference); the
//     skyline of every 10° sector is fitted around the terrain panorama or
//     the smooth horizon (locate2_skyline.go);
//   - signal: within one aircraft session the level follows
//     -20Â·log10(slant range) plus a low-order antenna gain G(el, az), with
//     the per-session transmitter offset profiled out (samples < 120 km);
//   - timing: T=1 transmissions of transmitters whose latency is stable
//     to microseconds (GPS-stamping receivers only; in practice rare).
//
// It searches at 8 km, 2 km, 500 m and 200 m and reports the posterior
// mean with its uncertainty ellipse and calibrated 95 % circle. See
// locate2_doc.go for the model, the evaluation and the wiring.
func Locate2(ctx context.Context, samples []L2Sample, opt Locate2Options) (*Locate2Result, error) {
	t0 := time.Now()
	res := &Locate2Result{Terms: []L2Term{}, Levels: []L2Level{}, Notes: []string{},
		SkylineRaised: nan(), SigmaKm: nan(), R95Km: nan(), MajorKm: nan(), MinorKm: nan(), AxisDeg: nan(), AntennaM: nan(), ErrorKm: nan()}
	p, err := newL2Problem(samples, opt, l2Default, res)
	if err != nil || p == nil {
		res.ElapsedMs = time.Since(t0).Milliseconds()
		return res, err
	}
	if err := p.search(ctx, res); err != nil {
		return nil, err
	}
	res.ElapsedMs = time.Since(t0).Milliseconds()
	return res, nil
}

func nan() Num { return Num(math.NaN()) }

// l2RaisedGain widens the posterior by 1 + gain·SkylineRaised.
const l2RaisedGain = 14.0

// l2ModelFloorKm is the 1-sigma (per axis) model error that no amount of
// data averages away on a real station: refraction that differs from the
// model, buildings and trees the DEM does not know, DEM height errors, the
// antenna's own pattern and mounting, multipath on the signal levels, and
// the correlation between samples of the same airways and hours (the
// tempered posterior treats thousands of samples as more independent than
// they are). The synthetic worlds and simnet draw their terrain from the
// same model the estimator uses, so they cannot measure it; the value comes
// from the field: a real station with a DEM and signal levels was 4.8 km
// (2.6 nm) off while the floor of then 0.5 km gave sigma 0.56 km and a 1.3 km
// circle (8.6 sigma). With 2.0 km the 95 % circle is at least 4.9 km, which
// holds that case, and simnet / the synthetic worlds stay covered
// (TestLocate2Simnet, TestLocate2Eval, TestL2CalibrateFieldCase).
const l2ModelFloorKm = 2.0

// l2Ellipse is a calibrated uncertainty ellipse (km; axis = bearing of the
// major axis, degrees).
type l2Ellipse struct {
	major, minor, axisDeg, sigma, r95 float64
}

// l2Calibrate turns the posterior second moments (km²) of a grid level of
// step stepKm into the reported, calibrated uncertainty. The tempered
// posterior is widened by l2Inflation (where the model is known to be
// incomplete), by 1 + l2RaisedGain·raised (raised = share of sectors whose
// skyline the fit moved; NaN = unknown) and then the grid quantisation and
// the model-error floor l2ModelFloorKm are added in quadrature.
func l2Calibrate(cxx, cyy, cxy, stepKm float64, signal, terrain bool, raised float64) l2Ellipse {
	inf := l2Inflation(signal, terrain)
	if isNum(raised) && raised > 0 {
		inf *= 1 + l2RaisedGain*raised
	}
	floor := stepKm*stepKm/12 + l2ModelFloorKm*l2ModelFloorKm
	cxx = cxx*inf*inf + floor
	cyy = cyy*inf*inf + floor
	cxy *= inf * inf
	tr, det := cxx+cyy, cxx*cyy-cxy*cxy
	disc := math.Sqrt(math.Max(0, tr*tr/4-det))
	l1, l2 := tr/2+disc, math.Max(tr/2-disc, 1e-9)
	axis := 0.5 * math.Atan2(2*cxy, cxx-cyy) // from east, counter-clockwise
	return l2Ellipse{
		major:   math.Sqrt(l1),
		minor:   math.Sqrt(l2),
		axisDeg: geo.NormalizeDeg(90 - axis*rad2deg),
		sigma:   math.Sqrt((l1 + l2) / 2),
		// 95 % of a 2-D normal along the major axis: chi² (2 dof) = 5.991
		r95: math.Sqrt(5.991 * l1),
	}
}

// l2Inflation is the calibration factor of the posterior's spread. Signal
// levels with a DEM get 1.5 rather than 1: the synthetic worlds that
// calibrated the other factors use the estimator's own terrain and link
// models, a real site never matches them that well.
func l2Inflation(signal, terrain bool) float64 {
	switch {
	case signal && terrain:
		return 1.5
	case signal:
		return 1.5
	case terrain:
		return 3
	default:
		return 1.5
	}
}

// ---------------------------------------------------------------- problem

type l2pt struct {
	x, y, z float64 // unit vector
	hM      float64 // aircraft height m MSL
	icao    uint32
	sess    int32
	ground  bool
}

type l2rs struct {
	x, y, z float64
	hM      float64
	rssi    float64
	sess    int32
}

type l2track struct {
	x, y, z []float64
	hM, m   []float64
}

// Antenna heights the posterior is profiled over, with their prior
// penalties: above ground with a terrain model, MSL (the ground included)
// without one.
var (
	l2NuisTerrain = []l2nuis{{5, 0.5}, {12, 0}, {30, 0.7}, {70, 2.5}}
	l2NuisSmooth  = []l2nuis{{10, 0.3}, {30, 0}, {80, 0.5}, {200, 1.5}, {500, 3}}
)

func l2NuisFor(terrain bool) []l2nuis {
	if terrain {
		return l2NuisTerrain
	}
	return l2NuisSmooth
}

type l2nuis struct {
	h   float64 // antenna height: AGL with terrain, effective MSL without
	pen float64 // prior penalty (log units)
}

type l2Problem struct {
	tun     l2Tuning
	k       float64
	kR2     float64 // 2Â·kÂ·R (m)
	c0      [2]float64
	workers int

	all    []l2pt // every usable sample with a height (horizon candidates)
	ends   []l2pt // session ends near the horizon candidates
	rssiIn []l2rs
	tracks []l2track
	src    terrain.Source
	dem    *l2DEM
	nSess  int
	nAC    int
	sumT   string
}

// newL2Problem prepares the data. It returns nil (and fills res.Reason)
// when there is not enough of it.
func newL2Problem(samples []L2Sample, opt Locate2Options, tun l2Tuning, res *Locate2Result) (*l2Problem, error) {
	p := &l2Problem{tun: tun, k: opt.K, src: opt.Terrain, workers: opt.Workers}
	if !(p.k > 0) {
		p.k = geo.DefaultK
	}
	p.kR2 = 2 * p.k * earthM
	if p.workers <= 0 {
		p.workers = runtime.GOMAXPROCS(0)
	}
	// valid samples, sorted by aircraft and time
	ss := make([]L2Sample, 0, len(samples))
	for _, s := range samples {
		if !geo.ValidLatLon(s.Lat, s.Lon) || s.T <= 0 {
			continue
		}
		if !s.Ground && !isNum(s.AltFt) {
			continue
		}
		ss = append(ss, s)
	}
	res.Samples = len(ss)
	if len(ss) < l2MinSamples {
		res.Reason = fmt.Sprintf("not enough reception samples yet: %d (at least %d are needed)", len(ss), l2MinSamples)
		return nil, nil
	}
	sort.Slice(ss, func(i, j int) bool {
		if ss[i].ICAO != ss[j].ICAO {
			return ss[i].ICAO < ss[j].ICAO
		}
		return ss[i].T < ss[j].T
	})
	// sessions
	sess := make([]int32, len(ss))
	var nSess int32 = -1
	acs := 0
	for i := range ss {
		if i == 0 || ss[i].ICAO != ss[i-1].ICAO {
			acs++
		}
		if i == 0 || ss[i].ICAO != ss[i-1].ICAO || ss[i].T-ss[i-1].T > tun.sessionGapMs {
			nSess++
		}
		sess[i] = nSess
	}
	p.nSess, p.nAC = int(nSess)+1, acs
	res.Sessions, res.Aircraft = p.nSess, p.nAC
	if p.nSess < l2MinSessions {
		res.Reason = fmt.Sprintf("not enough aircraft yet: %d sessions (at least %d are needed)", p.nSess, l2MinSessions)
		return nil, nil
	}

	// search centre
	if opt.PriorLat != 0 || opt.PriorLon != 0 {
		p.c0 = [2]float64{opt.PriorLat, geo.NormalizeLon(opt.PriorLon)}
	} else {
		p.c0 = l2RobustCentre(ss)
	}

	// heights of surface positions
	groundH := l2GroundHeights(ss, sess, opt.Terrain)

	// receiver-offline intervals (no samples at all for a while)
	times := make([]int64, len(ss))
	for i := range ss {
		times[i] = ss[i].T
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	var gaps [][2]int64 // receiver offline (no samples at all) intervals
	for j := 1; j < len(times); j++ {
		if times[j]-times[j-1] > tun.offlineGapMs {
			gaps = append(gaps, [2]int64{times[j-1], times[j]})
		}
	}
	nearGap := func(t int64) bool {
		if t-times[0] < tun.endGuardMs || times[len(times)-1]-t < tun.endGuardMs {
			return true
		}
		// first gap that ends after t - guard
		j := sort.Search(len(gaps), func(j int) bool { return gaps[j][1] >= t-tun.endGuardMs })
		return j < len(gaps) && gaps[j][0] <= t+tun.endGuardMs
	}
	// horizon candidates and session ends
	for i := range ss {
		s := &ss[i]
		var h float64
		switch {
		case s.Ground:
			h = groundH[i]
		default:
			h = geo.FeetToMetres(s.AltFt)
		}
		if !isNum(h) {
			continue
		}
		x, y, z := unitVec(s.Lat, s.Lon)
		pt := l2pt{x: x, y: y, z: z, hM: h, icao: s.ICAO, sess: sess[i], ground: s.Ground}
		p.all = append(p.all, pt)
		first := i == 0 || sess[i-1] != sess[i]
		last := i == len(ss)-1 || sess[i+1] != sess[i]
		if (first || last) && !s.Ground && s.AltFt < tun.endMaxAltFt {
			// a session that starts on / ends at the ground is a take-off
			// or landing, not a horizon crossing
			if first && i+1 < len(ss) && sess[i+1] == sess[i] && ss[i+1].Ground {
				continue
			}
			if last && i > 0 && sess[i-1] == sess[i] && ss[i-1].Ground {
				continue
			}
			if !nearGap(s.T) {
				p.ends = append(p.ends, pt)
			}
		}
	}
	p.all = l2Dedupe(p.all, tun.horizonMaxN)

	// signal samples: dominant channel, plausible levels
	p.rssiIn = l2SignalSet(ss, sess)

	// timing tracks
	usable, total := 0, 0
	for _, tr := range opt.Timing {
		total++
		if t, ok := l2UsableTrack(tr); ok {
			p.tracks = append(p.tracks, t)
			usable++
		}
	}
	if total > 0 {
		p.sumT = fmt.Sprintf("%d of %d T=1 aircraft have a transmit latency stable to microseconds", usable, total)
	}

	return p, nil
}

// l2RobustCentre is the weighted median position of the samples, low
// aircraft weighing more (they are heard only nearby).
func l2RobustCentre(ss []L2Sample) [2]float64 {
	n := len(ss)
	la, lo, w := make([]float64, n), make([]float64, n), make([]float64, n)
	ref := ss[0].Lon
	for i := range ss {
		la[i] = ss[i].Lat
		lo[i] = ref + geo.AngleDiff(ss[i].Lon, ref)
		a := 1.0
		if !ss[i].Ground {
			a = 1 + math.Max(0, ss[i].AltFt)/3000
		}
		w[i] = 1 / (a * a)
	}
	w2 := append([]float64(nil), w...)
	lat := weightedQuantileInPlace(la, w, 0.5)
	lon := weightedQuantileInPlace(lo, w2, 0.5)
	return [2]float64{lat, geo.NormalizeLon(lon)}
}

// l2GroundHeights returns the antenna height (m MSL) of every surface
// sample (NaN when unknown): terrain + 4 m with an elevation model, else
// the lowest airborne altitude reported within 4 km (the runway level as
// seen by landing and departing aircraft) minus 50 ft.
func l2GroundHeights(ss []L2Sample, sess []int32, src terrain.Source) []float64 {
	out := make([]float64, len(ss))
	for i := range out {
		out[i] = math.NaN()
	}
	any := false
	for i := range ss {
		if ss[i].Ground {
			any = true
			if src != nil {
				if z, ok := src.ElevationM(ss[i].Lat, ss[i].Lon); ok {
					out[i] = z + 4
				}
			}
		}
	}
	if !any {
		return out
	}
	// lowest low-level airborne altitude per 0.02Â° cell
	type key struct{ a, b int32 }
	low := map[key]float64{}
	cell := func(lat, lon float64) key {
		return key{int32(math.Floor(lat / 0.02)), int32(math.Floor(lon / 0.02))}
	}
	for i := range ss {
		s := &ss[i]
		if s.Ground || !(s.AltFt < 3000) {
			continue
		}
		k := cell(s.Lat, s.Lon)
		if v, ok := low[k]; !ok || s.AltFt < v {
			low[k] = s.AltFt
		}
	}
	for i := range ss {
		if !ss[i].Ground || isNum(out[i]) {
			continue
		}
		k := cell(ss[i].Lat, ss[i].Lon)
		best := math.Inf(1)
		for da := int32(-2); da <= 2; da++ {
			for db := int32(-2); db <= 2; db++ {
				if v, ok := low[key{k.a + da, k.b + db}]; ok && v < best {
					best = v
				}
			}
		}
		if !math.IsInf(best, 1) {
			out[i] = geo.FeetToMetres(best-50) + 4
		}
	}
	return out
}

// l2Dedupe keeps, per 0.02Â° cell and 250 ft band, the lowest sample (the
// one the horizon constrains most), and then at most maxN samples
// (preferring the lowest).
func l2Dedupe(pts []l2pt, maxN int) []l2pt {
	type key struct {
		a, b, h int32
		g       bool
	}
	best := map[key]int{}
	for i := range pts {
		q := &pts[i]
		lat := math.Asin(clamp(q.z, -1, 1)) * rad2deg
		lon := math.Atan2(q.y, q.x) * rad2deg
		k := key{int32(math.Floor(lat / 0.02)), int32(math.Floor(lon / 0.02)), int32(math.Floor(q.hM / 76)), q.ground}
		if j, ok := best[k]; !ok || q.hM < pts[j].hM {
			best[k] = i
		}
	}
	out := make([]l2pt, 0, len(best))
	for _, i := range best {
		out = append(out, pts[i])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].hM != out[j].hM {
			return out[i].hM < out[j].hM
		}
		return out[i].icao < out[j].icao
	})
	if len(out) > maxN {
		out = out[:maxN]
	}
	return out
}

// l2SignalSet selects the samples of the signal term: the channel with the
// most levels, levels between the noise floor + 6 dB and -2 dBFS (no
// saturation), sessions with at least 4 of them.
func l2SignalSet(ss []L2Sample, sess []int32) []l2rs {
	chN := map[int]int{}
	var lv []float64
	for i := range ss {
		if isNum(ss[i].RSSI) && !ss[i].Ground {
			chN[ss[i].Ch]++
			lv = append(lv, ss[i].RSSI)
		}
	}
	if len(lv) < 50 {
		return nil
	}
	ch, n := 0, -1
	for c, k := range chN {
		if k > n || (k == n && c < ch) {
			ch, n = c, k
		}
	}
	sort.Float64s(lv)
	floor := lv[len(lv)/100] + 6
	var out []l2rs
	start := 0
	for i := 0; i <= len(ss); i++ {
		if i < len(ss) && (i == 0 || sess[i] == sess[i-1]) {
			continue
		}
		// session [start, i)
		var tmp []l2rs
		for j := start; j < i; j++ {
			s := &ss[j]
			if s.Ground || s.Ch != ch || !isNum(s.RSSI) || s.RSSI < floor || s.RSSI > -2 {
				continue
			}
			x, y, z := unitVec(s.Lat, s.Lon)
			tmp = append(tmp, l2rs{x: x, y: y, z: z, hM: geo.FeetToMetres(s.AltFt), rssi: s.RSSI, sess: sess[j]})
		}
		if len(tmp) >= 4 {
			out = append(out, tmp...)
		}
		start = i
	}
	return out
}

// l2UsableTrack tests whether a T=1 track's latency is stable: between
// consecutive points the epoch offset may only change by the change of the
// propagation time (at most the aircraft's displacement / c) plus 30 Âµs.
// Transmitters that jitter their transmissions (the norm: squitters are
// randomised and T=1 transmitters are rarely synchronised, see
// internal/latency) fail this test and carry no information.
func l2UsableTrack(tr L2TimingTrack) (l2track, bool) {
	var t l2track
	if len(tr.Points) < 20 {
		return t, false
	}
	ok, pairs := 0, 0
	for i := 1; i < len(tr.Points); i++ {
		a, b := &tr.Points[i-1], &tr.Points[i]
		disp := geo.HaversineKm(a.Lat, a.Lon, b.Lat, b.Lon)*1000 + math.Abs(a.AltM-b.AltM)
		if disp > 60_000 {
			continue
		}
		pairs++
		if math.Abs(b.MNs-a.MNs) <= disp/lightMPerNs+30_000 {
			ok++
		}
	}
	if pairs < 15 || float64(ok) < 0.8*float64(pairs) {
		return t, false
	}
	for _, q := range tr.Points {
		x, y, z := unitVec(q.Lat, q.Lon)
		t.x, t.y, t.z = append(t.x, x), append(t.y, y), append(t.z, z)
		t.hM, t.m = append(t.hM, q.AltM), append(t.m, q.MNs)
	}
	return t, true
}

// ------------------------------------------------------------- the search

// l2Node is one candidate position.
type l2Node struct {
	px, py   float64 // plane coordinates around c0, km (east, north)
	lat, lon float64
	ux       [3]float64
	e, n     [3]float64
	g        float64 // ground elevation (terrain) or NaN
	lp       float64
	parts    [4]float64
	nuis     int
}

type l2Level struct {
	step, half float64
	cx, cy     float64 // box centre (plane)
	nx         int
	nodes      []l2Node
	max        float64
}

// planeToLatLon converts plane (east, north) km around c0 to lat/lon.
func (p *l2Problem) planeToLatLon(x, y float64) (float64, float64) {
	d := math.Hypot(x, y)
	if d < 1e-9 {
		return p.c0[0], p.c0[1]
	}
	la, lo := geo.Destination(p.c0[0], p.c0[1], math.Atan2(x, y)*rad2deg, d)
	return la, geo.NormalizeLon(lo)
}

func (p *l2Problem) latLonToPlane(lat, lon float64) (float64, float64) {
	d := geo.HaversineKm(p.c0[0], p.c0[1], lat, lon)
	b := geo.InitialBearing(p.c0[0], p.c0[1], lat, lon) / rad2deg
	return d * math.Sin(b), d * math.Cos(b)
}

func (p *l2Problem) makeNode(x, y float64) l2Node {
	la, lo := p.planeToLatLon(x, y)
	nd := l2Node{px: x, py: y, lat: la, lon: lo, g: math.NaN()}
	sl, cl := math.Sincos(la / rad2deg)
	so, co := math.Sincos(lo / rad2deg)
	nd.ux = [3]float64{cl * co, cl * so, sl}
	nd.e = [3]float64{-so, co, 0}
	nd.n = [3]float64{-sl * co, -sl * so, cl}
	if p.dem != nil {
		nd.g = p.dem.at(x, y)
	}
	return nd
}

// levelData is the data of one level (selected for its box).
type levelData struct {
	hor, end []l2pt
	rs       []l2rs
	rsSess   []int32 // start index of every session in rs, plus len(rs)
	terrain  bool
	nuis     []l2nuis // antenna heights profiled over
}

func (p *l2Problem) search(ctx context.Context, res *Locate2Result) error {
	type spec struct {
		step, capHalf, keep float64
		terrain             bool
	}
	r0 := 200.0
	// The first level reads the terrain on a 1 km grid (it only has to
	// tell regions apart), the others on 250 m.
	specs := []spec{{8, r0, 40, true}, {2, 60, 25, true}, {0.5, 15, 15, true}, {0.2, 5, 12, true}}
	var levels []*l2Level
	cx, cy, half := 0.0, 0.0, r0
	var mapNode l2Node
	for li, sp := range specs {
		useTerrain := false
		if sp.terrain && p.src != nil {
			demStep := l2DEMStepKm
			if li == 0 {
				demStep = l2DEMCoarseKm
			}
			need := half + l2PanoKm + sp.step
			if p.dem == nil || p.dem.step != demStep || !p.dem.covers(cx, cy, need) {
				p.dem = newL2DEM(p, cx, cy, need, demStep)
			}
			switch {
			case p.dem.noData <= 0.3:
				useTerrain = true
				res.Terrain = true
			case li == 0:
				// a wide area without data: search it on the smooth earth
				p.dem = nil
			default:
				res.Notes = append(res.Notes, fmt.Sprintf("Terrain data covers only %.0f %% of the search area; the terrain panorama was not used (open the terrain page once to download the tiles).", 100*(1-p.dem.noData)))
				p.dem = nil
				p.src = nil
				res.Terrain = false
			}
		}
		lv := &l2Level{step: sp.step, half: half, cx: cx, cy: cy}
		ld := p.levelData(cx, cy, half, useTerrain, li == 0)
		if err := p.evalLevel(ctx, lv, ld, li == 0); err != nil {
			return err
		}
		levels = append(levels, lv)
		res.Levels = append(res.Levels, L2Level{StepKm: sp.step, Nodes: len(lv.nodes), HalfKm: round(half, 2),
			Horizon: len(ld.hor), Signal: len(ld.rs), Endpoint: len(ld.end)})
		mapNode = lv.best()
		if li+1 == len(specs) {
			break
		}
		// next box: the nodes within keep of the maximum
		x0, x1, y0, y1 := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
		for i := range lv.nodes {
			nd := &lv.nodes[i]
			if nd.lp > lv.max-sp.keep {
				x0, x1 = math.Min(x0, nd.px), math.Max(x1, nd.px)
				y0, y1 = math.Min(y0, nd.py), math.Max(y1, nd.py)
			}
		}
		nh := specs[li+1].capHalf
		cx, cy = (x0+x1)/2, (y0+y1)/2
		half = math.Max(x1-x0, y1-y0)/2 + sp.step
		if half > nh {
			// too wide for the next step: centre on the best node
			cx, cy, half = mapNode.px, mapNode.py, nh
		}
		half = math.Max(half, 2*specs[li+1].step)
	}

	// posterior moments from the finest level whose box holds the mass
	use := len(levels) - 1
	for use > 0 && levels[use].truncated(12) {
		use--
	}
	lv := levels[use]

	// term report: final-level contributions at the MAP vs the search centre
	last := levels[len(levels)-1]
	ld := p.levelData(last.cx, last.cy, last.half, p.dem != nil, false)
	centre := p.makeNode(0, 0)
	sc := newL2Scratch(p)
	p.eval(&centre, ld, sc)
	best := mapNode
	p.eval(&best, ld, sc)
	if bn := best.nuis; bn >= 0 && bn < len(sc.secN) && sc.secN[bn] >= 6 {
		res.SkylineRaised = Num(round(float64(sc.raised[bn])/float64(sc.secN[bn]), 3))
	}
	names := []string{"horizon", "endpoints", "signal", "timing"}
	ns := []int{len(ld.hor), len(ld.end), len(ld.rs), len(p.tracks)}
	details := []string{
		"aircraft must be above the radio horizon / terrain panorama; surface positions draw airport line-of-sight discs",
		"low aircraft that appear or vanish in mid-air do so at the skyline (fitted per 10° sector)",
		"levels within each aircraft pass follow the inverse-square law plus a low-order antenna pattern",
		"T=1 transmissions with microsecond-stable latency give range differences",
	}
	if p.sumT != "" {
		details[3] = p.sumT
	}
	for i := range names {
		used := ns[i] > 0
		res.Terms = append(res.Terms, L2Term{Name: names[i], Used: used, N: ns[i], Detail: details[i],
			DeltaLL: Num(round(best.parts[i]-centre.parts[i], 2))})
	}

	mx, my, cxx, cyy, cxy := lv.moments()
	signal := len(p.rssiIn) > 0
	raised := math.NaN()
	if isNum(float64(res.SkylineRaised)) {
		raised = float64(res.SkylineRaised)
	}
	el := l2Calibrate(cxx, cyy, cxy, lv.step, signal, p.dem != nil, raised)
	res.MajorKm, res.MinorKm = Num(round(el.major, 3)), Num(round(el.minor, 3))
	res.AxisDeg = Num(round(el.axisDeg, 1))
	res.SigmaKm = Num(round(el.sigma, 3))
	res.R95Km = Num(round(el.r95, 3))
	res.Lat, res.Lon = p.planeToLatLon(mx, my)
	res.Lat, res.Lon = round(res.Lat, 5), round(res.Lon, 5)
	res.MAPLat, res.MAPLon = round(mapNode.lat, 5), round(mapNode.lon, 5)
	if nu := l2NuisFor(p.dem != nil); mapNode.nuis >= 0 && mapNode.nuis < len(nu) {
		res.AntennaM = Num(nu[mapNode.nuis].h)
	}
	res.OK = true
	res.Surface = p.surface(levels, res.Lat, res.Lon, float64(res.R95Km))
	if res.R95Km > 25 {
		res.Notes = append(res.Notes, "The data still allow a wide area; low traffic (approaches, departures, aircraft on the ground) in several directions narrows it down.")
	}
	return nil
}

// truncated reports whether posterior mass within keep of the maximum
// touches the box edge (the box was capped).
func (lv *l2Level) truncated(keep float64) bool {
	for i := range lv.nodes {
		nd := &lv.nodes[i]
		if nd.lp > lv.max-keep {
			if math.Abs(nd.px-lv.cx) > lv.half-lv.step*0.99 || math.Abs(nd.py-lv.cy) > lv.half-lv.step*0.99 {
				return true
			}
		}
	}
	return false
}

func (lv *l2Level) best() l2Node {
	b := 0
	for i := range lv.nodes {
		if lv.nodes[i].lp > lv.nodes[b].lp {
			b = i
		}
	}
	return lv.nodes[b]
}

func (lv *l2Level) moments() (mx, my, cxx, cyy, cxy float64) {
	var sw float64
	for i := range lv.nodes {
		nd := &lv.nodes[i]
		w := math.Exp(nd.lp - lv.max)
		sw += w
		mx += w * nd.px
		my += w * nd.py
	}
	mx /= sw
	my /= sw
	for i := range lv.nodes {
		nd := &lv.nodes[i]
		w := math.Exp(nd.lp - lv.max)
		dx, dy := nd.px-mx, nd.py-my
		cxx += w * dx * dx
		cyy += w * dy * dy
		cxy += w * dx * dy
	}
	return mx, my, cxx / sw, cyy / sw, cxy / sw
}

// levelData selects the samples that can matter inside the box: those whose
// horizon margin at the box centre could turn negative somewhere in it.
func (p *l2Problem) levelData(cx, cy, half float64, withTerrain, first bool) *levelData {
	ld := &levelData{terrain: withTerrain, nuis: l2NuisFor(withTerrain)}
	c := p.makeNode(cx, cy)
	hd := half * math.Sqrt2
	g0 := 0.0
	if withTerrain && isNum(c.g) {
		g0 = c.g
	}
	slack := 0.0
	if withTerrain {
		slack = 4
	}
	pick := func(q *l2pt, extra float64) bool {
		dot := c.ux[0]*q.x + c.ux[1]*q.y + c.ux[2]*q.z
		dKm := arcKm(dot)
		if dKm <= hd+1 {
			return true
		}
		// The margin (elevation above the smooth horizon) changes across
		// the box by at most (|Δh|/D² + 1/(2kR))·distance moved; keep the
		// sample when its smallest margin over the antenna heights could
		// fall below zero (plus the penalty scale and the terrain slack).
		D := dKm * 1000
		Dn := (dKm - hd) * 1000
		for _, nu := range ld.nuis {
			hr := g0 + nu.h
			tanE := (q.hM-hr)/D - D/p.kR2
			margin := (tanE + math.Sqrt(2*nu.h/(p.k*earthM))) * rad2deg
			lim := rad2deg*1.2*(math.Abs(q.hM-hr)/(Dn*Dn)+1/p.kR2)*hd*1000 + slack + extra
			if margin < lim {
				return true
			}
		}
		return false
	}
	for i := range p.all {
		if pick(&p.all[i], 1.5) {
			ld.hor = append(ld.hor, p.all[i])
		}
	}
	for i := range p.ends {
		if pick(&p.ends[i], 3) {
			ld.end = append(ld.end, p.ends[i])
		}
	}
	maxN := p.tun.rssiMaxN
	if first {
		maxN /= 3
	}
	limKm := p.tun.rssiMaxKm + hd
	var sel []l2rs
	for i := range p.rssiIn {
		q := &p.rssiIn[i]
		if arcKm(c.ux[0]*q.x+c.ux[1]*q.y+c.ux[2]*q.z) < limKm {
			sel = append(sel, *q)
		}
	}
	// thin evenly to maxN, keeping whole-session structure
	if len(sel) > maxN {
		stride := float64(len(sel)) / float64(maxN)
		var th []l2rs
		for f := 0.0; int(f) < len(sel); f += stride {
			th = append(th, sel[int(f)])
		}
		sel = th
	}
	// sessions with >= 3 samples
	for i := 0; i < len(sel); {
		j := i
		for j < len(sel) && sel[j].sess == sel[i].sess {
			j++
		}
		if j-i >= 3 {
			ld.rsSess = append(ld.rsSess, int32(len(ld.rs)))
			ld.rs = append(ld.rs, sel[i:j]...)
		}
		i = j
	}
	ld.rsSess = append(ld.rsSess, int32(len(ld.rs)))
	if len(ld.rsSess) < 21 { // fewer than 20 sessions: no signal term
		ld.rs, ld.rsSess = nil, nil
	}
	return ld
}

func arcKm(dot float64) float64 {
	c2 := 2 * (1 - dot)
	if c2 < 0 {
		c2 = 0
	}
	h := math.Sqrt(c2) / 2
	if h > 1 {
		h = 1
	}
	return 2 * geo.EarthRadiusKm * math.Asin(h)
}

// evalLevel builds the level's grid (a circle for the first level) and
// evaluates every node in parallel.
func (p *l2Problem) evalLevel(ctx context.Context, lv *l2Level, ld *levelData, circle bool) error {
	n := int(math.Round(2*lv.half/lv.step)) + 1
	lv.nx = n
	for r := 0; r < n; r++ {
		for c := 0; c < n; c++ {
			x := lv.cx - lv.half + float64(c)*lv.step
			y := lv.cy - lv.half + float64(r)*lv.step
			if circle && math.Hypot(x-lv.cx, y-lv.cy) > lv.half+1e-9 {
				continue
			}
			lv.nodes = append(lv.nodes, p.makeNode(x, y))
		}
	}
	var wg sync.WaitGroup
	next := make(chan int, len(lv.nodes))
	for i := range lv.nodes {
		next <- i
	}
	close(next)
	var cancelled bool
	var mu sync.Mutex
	for w := 0; w < p.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc := newL2Scratch(p)
			k := 0
			for i := range next {
				if k++; k%32 == 0 && ctx.Err() != nil {
					mu.Lock()
					cancelled = true
					mu.Unlock()
					return
				}
				p.eval(&lv.nodes[i], ld, sc)
			}
		}()
	}
	wg.Wait()
	if cancelled || ctx.Err() != nil {
		return errL2Cancelled
	}
	lv.max = math.Inf(-1)
	for i := range lv.nodes {
		lv.max = math.Max(lv.max, lv.nodes[i].lp)
	}
	return nil
}

// l2Scratch holds per-worker buffers.
type l2Scratch struct {
	pano  [][]float32 // per nuisance: rays Ã— steps cumulative max tan
	pen   []float64
	endL  []float64
	ys    []float64
	xs    [][3]float64
	steps []float64 // panorama step distances, m
	kR2   float64
	prev  []float32
	hr    []float64
	dip   []float64
	tres  []float64
	// hist[i] counts the margins of the heard low aircraft for antenna
	// height i: the reference density of the endpoint term
	hist [][]float64
	// far[i][ray]: tangent of the smooth-earth horizon dip beyond the
	// panorama, over the lowest terrain at the end of the ray
	far [][]float32
	// secH / secE[i][sector·l2HistBins + bin]: margins of the heard low
	// aircraft / of the session ends per azimuth sector (skyline fit)
	secH, secE [][]float64
	nzb        []int
	// secN / raised: sectors with session ends / with a skyline fitted off
	// the model horizon, per antenna height (last eval)
	secN, raised []int
}

func newL2Scratch(p *l2Problem) *l2Scratch {
	nn := len(l2NuisSmooth)
	sc := &l2Scratch{pen: make([]float64, nn), endL: make([]float64, nn), kR2: p.kR2,
		prev: make([]float32, nn), hr: make([]float64, nn), dip: make([]float64, nn)}
	for d := l2PanoD0Km; d <= l2PanoKm; d *= l2PanoQ {
		sc.steps = append(sc.steps, d*1000)
	}
	sc.far = make([][]float32, nn)
	for i := range sc.far {
		sc.far[i] = make([]float32, l2PanoRays)
	}
	sc.secN, sc.raised = make([]int, nn), make([]int, nn)
	sc.secH, sc.secE = make([][]float64, nn), make([][]float64, nn)
	for i := range sc.secH {
		sc.secH[i] = make([]float64, l2Sectors*l2HistBins)
		sc.secE[i] = make([]float64, l2Sectors*l2HistBins)
	}
	sc.hist = make([][]float64, nn)
	for i := range sc.hist {
		sc.hist[i] = make([]float64, l2HistBins)
	}
	sc.pano = make([][]float32, nn)
	for i := range sc.pano {
		sc.pano[i] = make([]float32, l2PanoRays*len(sc.steps))
	}
	return sc
}

// eval computes the log posterior of one node (profiled over the antenna
// height) and its parts.
func (p *l2Problem) eval(nd *l2Node, ld *levelData, sc *l2Scratch) {
	tun := &p.tun
	nuis := ld.nuis
	nn := len(nuis)
	hr := sc.hr[:nn] // antenna height MSL per nuisance
	dip := sc.dip[:nn]
	g := 0.0
	if ld.terrain && isNum(nd.g) {
		g = nd.g
	}
	for i := range nuis {
		hr[i] = g + nuis[i].h
		dip[i] = -math.Sqrt(2 * nuis[i].h / (p.k * earthM))
		sc.pen[i], sc.endL[i] = 0, 0
		sc.secN[i], sc.raised[i] = 0, 0
	}
	nSteps := len(sc.steps)
	usePano := ld.terrain && p.dem != nil
	if usePano {
		p.dem.panorama(nd, hr, sc)
	}
	lnQ := math.Log(l2PanoQ)
	geom := func(q *l2pt) (D float64, ray, step int, ok bool) {
		dot := nd.ux[0]*q.x + nd.ux[1]*q.y + nd.ux[2]*q.z
		D = arcKm(dot) * 1000
		if D < 300 {
			return 0, 0, 0, false
		}
		if usePano {
			e := nd.e[0]*q.x + nd.e[1]*q.y + nd.e[2]*q.z
			n := nd.n[0]*q.x + nd.n[1]*q.y + nd.n[2]*q.z
			az := math.Atan2(e, n)
			if az < 0 {
				az += 2 * math.Pi
			}
			ray = int(az/(2*math.Pi)*l2PanoRays+0.5) % l2PanoRays
			if D >= sc.steps[nSteps-1] {
				step = nSteps // beyond the panorama
			} else {
				step = int(math.Log(D/sc.steps[0]) / lnQ)
				if step < 0 {
					step = -1
				}
			}
		}
		return D, ray, step, true
	}
	// req is the tangent of the elevation an aircraft at (ray, step) must
	// exceed. Without terrain it is the smooth-earth dip of the horizon.
	// With terrain it is the panorama (the highest terrain angle up to the
	// aircraft's distance), beyond the panorama also the smooth-earth dip
	// over the lowest terrain at the end of the ray. Each is the lowest of
	// the ray and its two neighbours: a ray that grazes a hill's flank must
	// not condemn an aircraft seen just past it (the benefit of the doubt
	// goes to the data).
	req := func(i, ray, step int) float64 {
		if !usePano {
			return dip[i]
		}
		rl := (ray + l2PanoRays - 1) % l2PanoRays
		rr := (ray + 1) % l2PanoRays
		pi := sc.pano[i]
		if step < 0 {
			return math.Inf(-1)
		}
		if step < nSteps {
			return float64(min(pi[ray*nSteps+step], pi[rl*nSteps+step], pi[rr*nSteps+step]))
		}
		j := nSteps - 1
		v := float64(min(pi[ray*nSteps+j], pi[rl*nSteps+j], pi[rr*nSteps+j]))
		fa := sc.far[i]
		return math.Max(v, float64(min(fa[ray], fa[rl], fa[rr])))
	}
	s2 := tun.violScaleDeg * tun.violScaleDeg
	useEnds := len(ld.end) > 0
	if useEnds {
		for i := 0; i < nn; i++ {
			clear(sc.hist[i])
		}
	}
	// The skyline is fitted: per azimuth sector the endpoint term may raise
	// (or lower) the model horizon - smooth earth or terrain panorama - see
	// l2SkylineLL. tun.fixedSkyline selects the plain variant (the model
	// horizon only; kept for comparisons).
	fit := useEnds && !tun.fixedSkyline
	if fit {
		for i := 0; i < nn; i++ {
			clear(sc.secH[i])
			clear(sc.secE[i])
		}
	}
	lowM := geo.FeetToMetres(tun.endMaxAltFt)
	nLow := 0
	for k := range ld.hor {
		q := &ld.hor[k]
		D, ray, step, ok := geom(q)
		if !ok {
			continue
		}
		curv := D / p.kR2
		// the reported altitude is barometric (and a surface position's
		// height comes from a model): allow its error as elevation slack
		altSlack := tun.altErrM / D * rad2deg
		low := useEnds && !q.ground && q.hM < lowM
		sec := 0
		if low {
			nLow++
			if fit {
				sec = nd.sector(q)
			}
		}
		for i := 0; i < nn; i++ {
			tanE := (q.hM-hr[i])/D - curv
			v := (req(i, ray, step)-tanE)*rad2deg - tun.violFreeDeg - altSlack
			if v > 0 {
				sc.pen[i] += math.Min(tun.violCap, 0.5*v*v/s2)
			}
			if low {
				b := l2HistBin(-v - tun.violFreeDeg - altSlack)
				sc.hist[i][b]++
				if fit {
					sc.secH[i][sec*l2HistBins+b]++
				}
			}
		}
	}
	if fit && nLow >= 200 {
		for k := range ld.end {
			q := &ld.end[k]
			D, ray, step, ok := geom(q)
			if !ok {
				continue
			}
			sec := nd.sector(q)
			for i := 0; i < nn; i++ {
				tanE := (q.hM-hr[i])/D - D/p.kR2
				u := (tanE - req(i, ray, step)) * rad2deg
				sc.secE[i][sec*l2HistBins+l2HistBin(u)]++
			}
		}
		for i := 0; i < nn; i++ {
			ll, ns, nr := l2SkylineLL(tun, sc.secE[i], sc.secH[i], sc.hist[i], nLow, &sc.nzb)
			sc.endL[i] = ll * tun.tauEnd
			sc.secN[i], sc.raised[i] = ns, nr
		}
	} else if useEnds && nLow >= 200 {
		// Endpoint term: the margin u of a session end is a mixture of
		// "lost at the horizon" (u ~ N(0, σ)) and "lost for another reason",
		// whose density is that of the margins of all heard low aircraft
		// at this node. The reference density matters: seen from far away
		// every low aircraft is near the horizon, so ends are informative
		// only when they cluster at the horizon more than the rest does.
		es2 := tun.endSigmaDeg * tun.endSigmaDeg
		gn := 1 / (tun.endSigmaDeg * math.Sqrt(2*math.Pi))
		for i := 0; i < nn; i++ {
			norm := 1 / ((float64(nLow) + 0.5*l2HistBins) * l2HistW)
			hi := sc.hist[i]
			for k := range ld.end {
				q := &ld.end[k]
				D, ray, step, ok := geom(q)
				if !ok {
					continue
				}
				tanE := (q.hM-hr[i])/D - D/p.kR2
				u := (tanE - req(i, ray, step)) * rad2deg
				other := (hi[l2HistBin(u)] + 0.5) * norm
				ref := other // the end explained by "another reason" only
				sc.endL[i] += math.Log((tun.endPi*gn*math.Exp(-0.5*u*u/es2) + (1-tun.endPi)*other) / ref)
			}
		}
	}
	bestI, bestV := 0, math.Inf(-1)
	var bh, be float64
	for i := 0; i < nn; i++ {
		h := -sc.pen[i] / tun.tauHorizon
		e := sc.endL[i] / tun.tauEnd
		if v := h + e - nuis[i].pen; v > bestV {
			bestI, bestV, bh, be = i, v, h, e
		}
	}
	hNom := g + nuis[0].h
	if nn > 1 {
		hNom = g + nuis[1].h
	}
	rs := p.signalLL(nd, ld, hNom, sc)
	tm := p.timingLL(nd, hNom, sc)
	nd.parts = [4]float64{bh, be, rs, tm}
	nd.nuis = bestI
	nd.lp = bestV + rs + tm
}

// signalLL is the profile log-likelihood of the signal term: per session
// demeaned levels regressed on the demeaned antenna-gain basis
// [10Â·log10 cosÂ² el, cos az, sin az]; Ïƒ is profiled out.
func (p *l2Problem) signalLL(nd *l2Node, ld *levelData, hr float64, sc *l2Scratch) float64 {
	if len(ld.rs) == 0 {
		return 0
	}
	var A [3][3]float64
	var b [3]float64
	var yy float64
	nTot := 0
	for s := 0; s+1 < len(ld.rsSess); s++ {
		lo, hi := ld.rsSess[s], ld.rsSess[s+1]
		m := int(hi - lo)
		if cap(sc.ys) < m {
			sc.ys = make([]float64, m)
			sc.xs = make([][3]float64, m)
		}
		ys, xs := sc.ys[:m], sc.xs[:m]
		var my float64
		var mx [3]float64
		for j := 0; j < m; j++ {
			q := &ld.rs[int(lo)+j]
			dot := nd.ux[0]*q.x + nd.ux[1]*q.y + nd.ux[2]*q.z
			D := arcKm(dot)
			dh := (q.hM - hr) / 1000
			// the aircraft drops below the tangent plane with distance
			dh -= D * D * 1000 / p.kR2
			slant2 := D*D + dh*dh
			if slant2 < 0.01 {
				slant2 = 0.01
			}
			ls := math.Log10(slant2)
			hz := D * D
			if hz < 1e-6 {
				hz = 1e-6
			}
			e := nd.e[0]*q.x + nd.e[1]*q.y + nd.e[2]*q.z
			n := nd.n[0]*q.x + nd.n[1]*q.y + nd.n[2]*q.z
			hn := math.Hypot(e, n)
			ca, sa := 1.0, 0.0
			if hn > 0 {
				ca, sa = n/hn, e/hn
			}
			ys[j] = q.rssi + 10*ls
			xs[j] = [3]float64{10 * (math.Log10(hz) - ls), ca, sa}
			my += ys[j]
			mx[0] += xs[j][0]
			mx[1] += xs[j][1]
			mx[2] += xs[j][2]
		}
		fm := float64(m)
		my /= fm
		for k := range mx {
			mx[k] /= fm
		}
		for j := 0; j < m; j++ {
			y := ys[j] - my
			x := [3]float64{xs[j][0] - mx[0], xs[j][1] - mx[1], xs[j][2] - mx[2]}
			for r := 0; r < 3; r++ {
				b[r] += x[r] * y
				for c := 0; c < 3; c++ {
					A[r][c] += x[r] * x[c]
				}
			}
			yy += y * y
		}
		nTot += m - 1
	}
	if nTot < 30 {
		return 0
	}
	// ridge-regularised solve (the basis can be nearly collinear)
	for r := 0; r < 3; r++ {
		A[r][r] += 1e-6*A[r][r] + 1e-9
	}
	beta, ok := solve3(A, b)
	ssr := yy
	if ok {
		ssr = yy - (beta[0]*b[0] + beta[1]*b[1] + beta[2]*b[2])
	}
	if ssr < 1e-6*float64(nTot) {
		ssr = 1e-6 * float64(nTot)
	}
	return -0.5 * float64(nTot) / p.tun.tauRSSI * math.Log(ssr/float64(nTot))
}

func solve3(A [3][3]float64, b [3]float64) ([3]float64, bool) {
	det := func(m [3][3]float64) float64 {
		return m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) - m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) + m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	}
	d := det(A)
	if !(math.Abs(d) > 1e-300) {
		return [3]float64{}, false
	}
	var x [3]float64
	for c := 0; c < 3; c++ {
		m := A
		for r := 0; r < 3; r++ {
			m[r][c] = b[r]
		}
		x[c] = det(m) / d
	}
	return x, true
}

// timingLL is the timing term: per usable T=1 track the epoch offsets minus
// the propagation time from the node must be constant (Huber loss around
// the track's mean).
func (p *l2Problem) timingLL(nd *l2Node, hr float64, sc *l2Scratch) float64 {
	if len(p.tracks) == 0 {
		return 0
	}
	sig := p.tun.timingSigmaNs
	var ll float64
	rr := earthM + hr
	res := sc.tres[:0]
	defer func() { sc.tres = res[:0] }()
	for ti := range p.tracks {
		t := &p.tracks[ti]
		res = res[:0]
		var mean float64
		for k := range t.m {
			dot := nd.ux[0]*t.x[k] + nd.ux[1]*t.y[k] + nd.ux[2]*t.z[k]
			ra := earthM + t.hM[k]
			slant := math.Sqrt(math.Max(0, ra*ra+rr*rr-2*ra*rr*dot))
			r := t.m[k] - slant/lightMPerNs
			res = append(res, r)
			mean += r
		}
		mean /= float64(len(res))
		// two Huber re-weighting steps for the offset
		for it := 0; it < 2; it++ {
			var sw, s float64
			for _, r := range res {
				w := 1.0
				if a := math.Abs(r - mean); a > 2*sig {
					w = 2 * sig / a
				}
				sw += w
				s += w * r
			}
			mean = s / sw
		}
		for _, r := range res {
			u := math.Abs(r-mean) / sig
			if u <= 2 {
				ll -= 0.5 * u * u
			} else {
				ll -= 2*u - 2
			}
		}
	}
	return ll / p.tun.tauTiming
}

// surface renders the posterior around the estimate as a CostSurface
// (0 = most likely ... 1 = excluded) for the map.
func (p *l2Problem) surface(levels []*l2Level, lat0, lon0, r95 float64) *CostSurface {
	half := clamp(4*r95, 15, 250)
	const kmPerDeg = geo.EarthRadiusKm * math.Pi / 180
	cl := math.Max(0.1, math.Cos(lat0*math.Pi/180))
	n := guessSurfaceN
	s := &CostSurface{Method: "locate2", Rows: n, Cols: n, Cost: make(Nums, n*n)}
	s.DLat = 2 * half / kmPerDeg / float64(n-1)
	s.DLon = 2 * half / (kmPerDeg * cl) / float64(n-1)
	s.South = lat0 - half/kmPerDeg
	s.West = lon0 - half/(kmPerDeg*cl)
	gmax := levels[len(levels)-1].max
	bestV := math.Inf(-1)
	for r := 0; r < n; r++ {
		for c := 0; c < n; c++ {
			la, lo := s.South+float64(r)*s.DLat, s.West+float64(c)*s.DLon
			x, y := p.latLonToPlane(la, lo)
			v := math.NaN()
			for li := len(levels) - 1; li >= 0; li-- {
				if lp, ok := levels[li].lookup(x, y); ok {
					// levels differ by the data they use; align on the max
					v = lp - levels[li].max + gmax
					break
				}
			}
			cost := 1.0
			if isNum(v) {
				cost = clamp(math.Sqrt(math.Max(0, gmax-v)/25), 0, 1)
				if v > bestV {
					bestV, s.MinLat, s.MinLon = v, round(la, 5), round(geo.NormalizeLon(lo), 5)
				}
			}
			s.Cost[r*n+c] = round(cost, 4)
		}
	}
	s.South, s.West, s.DLat, s.DLon = round(s.South, 5), round(s.West, 5), round(s.DLat, 6), round(s.DLon, 6)
	return s
}

// lookup returns the log posterior of the node nearest to (x, y) when it
// lies inside the level's grid.
func (lv *l2Level) lookup(x, y float64) (float64, bool) {
	if math.Abs(x-lv.cx) > lv.half+lv.step/2 || math.Abs(y-lv.cy) > lv.half+lv.step/2 {
		return 0, false
	}
	// nodes are in row-major order over the full square except for the
	// circular first level: search by coordinates
	c := int(math.Round((x - (lv.cx - lv.half)) / lv.step))
	r := int(math.Round((y - (lv.cy - lv.half)) / lv.step))
	if c < 0 || r < 0 || c >= lv.nx || r >= lv.nx {
		return 0, false
	}
	if len(lv.nodes) == lv.nx*lv.nx {
		return lv.nodes[r*lv.nx+c].lp, true
	}
	tx := lv.cx - lv.half + float64(c)*lv.step
	ty := lv.cy - lv.half + float64(r)*lv.step
	i := sort.Search(len(lv.nodes), func(i int) bool {
		nd := &lv.nodes[i]
		return nd.py > ty+1e-9 || (math.Abs(nd.py-ty) <= 1e-9 && nd.px >= tx-1e-9)
	})
	if i < len(lv.nodes) && math.Abs(lv.nodes[i].px-tx) < 1e-6 && math.Abs(lv.nodes[i].py-ty) < 1e-6 {
		return lv.nodes[i].lp, true
	}
	return 0, false
}
