package survey

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sort"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// This file is the network bundle adjustment (DESIGN §10.3 phase 4,
// RESEARCH "A″ network self-calibration"): the positions of many stations
// and their clocks are refined jointly from the raw arrival times of ADS-B
// reference transmissions they heard together. The package doc (doc.go,
// "Network bundle adjustment") describes the model; bundle_solve.go has
// the normal equations, bundle_seed.go the grid seed of unlocated
// stations and bundler.go the live collector.

// PriorKind is what a bundle station's position prior is.
type PriorKind uint8

// Prior kinds.
const (
	// PriorNone: no position at all (seeded by a grid search against the
	// other stations).
	PriorNone PriorKind = iota
	// PriorHint: a coarse position (Locate Me v2, kilometres); used as the
	// seed grid's centre and as a weak prior.
	PriorHint
	// PriorDeclared: an owner-declared position. It is tested against the
	// data (a wrong declaration is flagged and released) and, when it
	// passes, gauges the network with DeclaredSigmaHM.
	PriorDeclared
	// PriorAnchor: a GNSS-reported or owner-confirmed position (metres).
	// Anchors gauge the network tightly; an anchor the data contradicts is
	// flagged and released (data snooping, worst first).
	PriorAnchor
)

func (k PriorKind) String() string {
	switch k {
	case PriorHint:
		return "hint"
	case PriorDeclared:
		return "declared"
	case PriorAnchor:
		return "anchor"
	}
	return "none"
}

// BundleStation is one station of a bundle adjustment.
type BundleStation struct {
	// ID is the caller's identity of the station (returned in the result).
	ID    model.StationIdx
	Class model.ClockClass
	// Prior is the kind of Pos; Pos the prior (or seed) position (ECEF,
	// heights HAE). SigmaHM / SigmaVM are its horizontal / vertical 1σ
	// (0 = the configured default of the kind).
	Prior            PriorKind
	Pos              geo.Vec3
	SigmaHM, SigmaVM float64
	// HeightHAE / HeightSigmaM is an independent height prior (DEM + antenna
	// height); used instead of Pos's height for PriorNone and PriorHint and
	// after a released prior. HeightSigmaM 0 = none (the median height of
	// the seeded stations with UnlocatedHeightSigmaM).
	HeightHAE, HeightSigmaM float64
}

// BundleRec is one reception: station St (an index into
// BundleInput.Stations) heard the frame at its raw receiver time Ticks
// (units of the station class's NominalHz) in receiver epoch Epoch.
type BundleRec struct {
	St    int32
	Epoch uint32
	Ticks uint64
}

// BundleFrame is one long Mode S transmission of an ADS-B reference
// aircraft and its receptions Recs[R0 : R0+NR] of BundleInput.Recs.
type BundleFrame struct {
	// Key is the aircraft; TNs the approximate emission time (Unix ns of
	// the server's network time: only used to place the frame in a clock
	// window and as the clock polynomial's argument, ms errors are
	// harmless).
	Key uint32
	TNs int64
	// P is the aircraft's ECEF position at TNs and CovP its covariance (upper
	// triangle xx, xy, xz, yy, yz, zz; m²).
	P    geo.Vec3
	CovP [6]float32
	R0   int32
	NR   int32
}

// BundleInput is one bundle adjustment problem.
type BundleInput struct {
	Stations []BundleStation
	Frames   []BundleFrame
	Recs     []BundleRec
	// Atm is the propagation model (the engine's; zero = default).
	Atm geo.Atmosphere
}

// BundleConfig tunes the bundle adjustment. The zero value of every field
// means the documented default (Normalize).
type BundleConfig struct {
	// WindowS is the clock window: every station's clock is a polynomial
	// in time per window (offset + drift for GPS and hw12 classes, plus a
	// quadratic term for the others), eliminated in closed form (default
	// 10 s, 2..120 s).
	WindowS float64
	// MinBlockRecs is the minimum number of receptions of one station in
	// one window (fewer: its clock would absorb them; default 6, 4..1000).
	MinBlockRecs int
	// MinStationRecs: a station with fewer usable receptions is left out
	// (default 100, 10..1e6).
	MinStationRecs int
	// MaxStations bounds one adjustment (the cost grows with the cube of
	// the stations per window; default 64, 2..512). The Bundler selects
	// the stations; Bundle refuses larger inputs.
	MaxStations int
	// RobustRounds of reweighting (learned jitter per station, Huber
	// weights HuberK·σ, rejection beyond RejectK·σ; default 4 / 2 / 5).
	RobustRounds int
	HuberK       float64
	RejectK      float64
	// MaxIter Gauss-Newton iterations per round (default 12); MaxStepM
	// bounds one step of a station (default 5000 m).
	MaxIter  int
	MaxStepM float64
	// AnchorSigmaHM / AnchorSigmaVM are the default prior 1σ of anchors
	// (default 5 / 10 m).
	AnchorSigmaHM, AnchorSigmaVM float64
	// DeclaredLooseM is the horizontal 1σ of declared positions in the
	// first stage, where every declaration is tested against the data
	// (default 2000 m); DeclaredSigmaHM / DeclaredSigmaVM the prior of the
	// declarations that passed, in the second stage (default 30 / 30 m).
	DeclaredLooseM                   float64
	DeclaredSigmaHM, DeclaredSigmaVM float64
	// HintSigmaM is the default horizontal 1σ of a hint (default 5000 m);
	// UnlocatedHeightSigmaM the height 1σ of stations without a height
	// (default 300 m).
	HintSigmaM            float64
	UnlocatedHeightSigmaM float64
	// OutlierMinM / OutlierK: a prior position is contradicted when the
	// data-only position is more than max(OutlierMinM, OutlierK·σ_h)
	// away horizontally (default 100 m / 3, the survey's declared check).
	// MaxReleases bounds the priors released (default 16).
	OutlierMinM float64
	OutlierK    float64
	MaxReleases int
	// NoisyFactor / NoisyMinNs: a station whose learned jitter exceeds
	// NoisyFactor × its class prior and NoisyMinNs is flagged noisy
	// (default 4 / 150 ns). MaxRejectFrac flags a station with more of its
	// receptions rejected (default 0.25).
	NoisyFactor   float64
	NoisyMinNs    float64
	MaxRejectFrac float64
	// MaxSigmaHM: a station whose horizontal σ is larger is reported as
	// undetermined (default 1000 m).
	MaxSigmaHM float64
	// JackknifeGroups of contiguous windows for the delete-one-group
	// jackknife covariance (default 10, 0 = off below 4); SysFloorM a
	// systematic floor added in quadrature for GPS and hardware 12 MHz
	// clocks (default 3 m), SysFloorFreeM for the other classes, whose
	// timing has level-dependent biases the noise model does not carry
	// (default 10 m; simnet SDR errors reach 2-3 times the formal σ).
	JackknifeGroups int
	SysFloorM       float64
	SysFloorFreeM   float64
	// SearchRadiusM is the seed grid's radius around a hint or the
	// centroid of the co-observing stations (default 150 km);
	// SeedSigmaAboveM: a prior with a horizontal σ above this is seeded by
	// the grid (default 2000 m). MaxSeedPairs bounds the station pairs
	// one grid evaluation uses (default 30000).
	SearchRadiusM   float64
	SeedSigmaAboveM float64
	MaxSeedPairs    int
	// Workers is the number of goroutines for the window passes (default
	// min(GOMAXPROCS, 8)).
	Workers int

	// The live Bundler: Every is the adjustment period (DESIGN §10.3
	// "hourly"; default 1 h, 1 min..24 h), Span the data it uses (default
	// 1 h, 5 min..6 h), ThinInterval keeps at most one frame per aircraft
	// per interval (default 8 s, 1..60 s), MaxFrames bounds the memory
	// (default 200000 frames, ~60 MB with 10 receptions each; when full,
	// frames older than Span go, then every second frame and the interval
	// doubles).
	Every        time.Duration
	Span         time.Duration
	ThinInterval time.Duration
	MaxFrames    int
}

// DefaultBundleConfig returns the documented defaults.
func DefaultBundleConfig() BundleConfig {
	var c BundleConfig
	c.Normalize()
	return c
}

// Normalize fills in defaults and clamps every field. Idempotent.
func (c *BundleConfig) Normalize() {
	defF(&c.WindowS, 10, 2, 120)
	defI(&c.MinBlockRecs, 6, 4, 1000)
	defI(&c.MinStationRecs, 100, 10, 1_000_000)
	defI(&c.MaxStations, 64, 2, 512)
	defI(&c.RobustRounds, 4, 1, 20)
	defF(&c.HuberK, 2, 1, 10)
	defF(&c.RejectK, 5, 2, 50)
	c.RejectK = math.Max(c.RejectK, c.HuberK)
	defI(&c.MaxIter, 12, 2, 100)
	defF(&c.MaxStepM, 5000, 100, 100e3)
	defF(&c.AnchorSigmaHM, 5, 0.1, 1000)
	defF(&c.AnchorSigmaVM, 10, 0.1, 1000)
	defF(&c.DeclaredLooseM, 2000, 100, 100e3)
	defF(&c.DeclaredSigmaHM, 30, 1, 10e3)
	defF(&c.DeclaredSigmaVM, 30, 1, 10e3)
	defF(&c.HintSigmaM, 5000, 100, 500e3)
	defF(&c.UnlocatedHeightSigmaM, 300, 5, 5000)
	defF(&c.OutlierMinM, 100, 1, 100e3)
	defF(&c.OutlierK, 3, 1, 100)
	defI(&c.MaxReleases, 16, 1, 512)
	defF(&c.NoisyFactor, 4, 1, 1000)
	defF(&c.NoisyMinNs, 150, 1, 1e6)
	defF(&c.MaxRejectFrac, 0.25, 0.01, 1)
	defF(&c.MaxSigmaHM, 1000, 1, 1e6)
	defI(&c.JackknifeGroups, 10, 1, 100)
	defF(&c.SysFloorM, 3, 0.01, 1000)
	defF(&c.SysFloorFreeM, 10, 0.01, 1000)
	defF(&c.SearchRadiusM, 150e3, 1e3, 1000e3)
	defF(&c.SeedSigmaAboveM, 2000, 10, 1e6)
	defI(&c.MaxSeedPairs, 30000, 100, 10_000_000)
	defI(&c.Workers, min(runtime.GOMAXPROCS(0), 8), 1, 256)
	defD(&c.Every, time.Hour, time.Minute, 24*time.Hour)
	defD(&c.Span, time.Hour, 5*time.Minute, 6*time.Hour)
	defD(&c.ThinInterval, 8*time.Second, time.Second, time.Minute)
	defI(&c.MaxFrames, 200_000, 1000, 5_000_000)
}

// BundleStationResult is one station's outcome. Positions are exact:
// owner/admin data only (DESIGN §8), never public, never logged.
type BundleStationResult struct {
	ID    model.StationIdx
	Class model.ClockClass
	Prior PriorKind
	// Used: the station had enough data and is in the solution; Why says
	// why not ("few receptions", "unseeded", "singular").
	Used bool
	Why  string
	// Determined: Used and σ_h ≤ MaxSigmaHM.
	Determined bool
	// ECEF / LLH is the adjusted position; CovENU its final covariance
	// (east, north, up; m²): the larger of the formal and the inflated
	// jackknife covariance plus SysFloorM². FormalENU and JackknifeENU are
	// the ingredients.
	ECEF         geo.Vec3
	LLH          geo.LLH
	CovENU       [9]float64
	FormalENU    [9]float64
	JackknifeENU [9]float64
	SigmaHM      float64
	SigmaVM      float64
	// ShiftHM is the horizontal distance from the prior (or seed) position.
	ShiftHM float64
	// Receptions used, Rejected as outliers, clock Blocks (station
	// windows) and the learned timing noise.
	Receptions, Rejected, Blocks int
	JitterNs                     float64
	// Prior test (data snooping): DataOnly is the position the data give
	// with this station's own prior removed (every other prior kept),
	// DataOnlySigmaHM its σ_h; PriorDistHM the horizontal distance of the
	// prior from it and PriorLimitM the limit max(OutlierMinM,
	// OutlierK·σ_h). PriorTested is false when the data alone do not
	// determine the station (or it has no horizontal prior).
	PriorTested     bool
	DataOnly        geo.Vec3
	DataOnlySigmaHM float64
	PriorDistHM     float64
	PriorLimitM     float64
	// PriorReleased: the prior was contradicted and is not used in the
	// solution.
	PriorReleased bool
	// Outlier: the station failed a check; OutlierWhy lists them:
	// "declared" / "anchor" (the prior is contradicted), "noisy" (timing
	// noise far above its class), "rejects" (too many receptions
	// rejected).
	Outlier    bool
	OutlierWhy []string
	// Seeded: the start position came from the grid search.
	Seeded bool
}

// BundleResult is a bundle adjustment's outcome.
type BundleResult struct {
	Stations []BundleStationResult
	// Frames given and used, receptions used and rejected, clock windows
	// and blocks.
	Frames, FramesUsed, Receptions, Rejected, Windows, Blocks int
	// Iterations of Gauss-Newton (all stages); Released priors.
	Iterations, Released int
	// VarianceFactor is the weighted cost per degree of freedom (≈ 1 when
	// the learned noise fits).
	VarianceFactor float64
	// JackknifeGroups used (0 = formal covariance only).
	JackknifeGroups int
	// DurationS is the time span of the frames used; Elapsed the compute
	// time.
	DurationS float64
	Elapsed   time.Duration
}

// Bundle errors.
var (
	ErrBundleTooFew  = errors.New("survey: bundle: fewer than two usable stations")
	ErrBundleTooMany = errors.New("survey: bundle: too many stations")
	ErrBundleInput   = errors.New("survey: bundle: invalid input")
)

// Bundle runs a network bundle adjustment: all stations' positions and
// clocks are refined jointly from the frames (see the package doc,
// "Network bundle adjustment"). ctx cancels it between passes.
func Bundle(ctx context.Context, in BundleInput, cfg BundleConfig) (BundleResult, error) {
	t0 := time.Now()
	cfg.Normalize()
	if len(in.Stations) > cfg.MaxStations {
		return BundleResult{}, fmt.Errorf("%w: %d > %d", ErrBundleTooMany, len(in.Stations), cfg.MaxStations)
	}
	for i := range in.Frames {
		f := &in.Frames[i]
		if f.R0 < 0 || f.NR < 0 || int(f.R0)+int(f.NR) > len(in.Recs) {
			return BundleResult{}, fmt.Errorf("%w: frame %d receptions out of range", ErrBundleInput, i)
		}
	}
	for i := range in.Recs {
		if s := in.Recs[i].St; s < 0 || int(s) >= len(in.Stations) {
			return BundleResult{}, fmt.Errorf("%w: reception %d station out of range", ErrBundleInput, i)
		}
	}
	b := &bundle{cfg: &cfg, in: &in, atm: in.Atm.Normalize(), ctx: ctx}
	if err := b.prepare(); err != nil {
		return b.result(t0), err
	}
	if err := b.seedAll(); err != nil {
		return b.result(t0), err
	}
	if err := b.run(); err != nil {
		return b.result(t0), err
	}
	return b.result(t0), nil
}

// bst is the solver's state of one station.
type bst struct {
	in    *BundleStation
	deg   int // clock polynomial degree
	u     geo.Vec3
	start geo.Vec3 // the prior or seed position (ShiftHM)
	slot  int      // position parameter index /3 (−1: not used)
	why   string
	sig2  float64 // timing noise variance (m²)
	sig0  float64 // class prior jitter (m)

	// prior: horizontal part (hOn: mean m, 1σ hSig) and a height prior
	// (always: mH, 1σ vSig).
	m          geo.Vec3
	mH         float64
	hOn        bool
	hSig, vSig float64
	released   bool
	seeded     bool

	nrec, nrej, nblk int

	// snooping
	tested           bool
	dataOnly         geo.Vec3
	dataSigH, distH  float64
	limit            float64
	flaggedPrior     bool
	jk               [9]float64 // jackknife ECEF covariance
	formal, final    [9]float64 // ECEF
	sigH, sigV       float64
	jkOK, determined bool
}

// brec is one reception in solver order (frames sorted by window).
type brec struct {
	st  int32
	blk int32 // global block index (−1: dropped)
	y   float64
	w   float32 // weight (0: rejected)
	z   float32 // standardised residual at the last pass (base σ)
}

type bframe struct {
	win  int32
	r0   int32
	nr   int32
	x    float64 // time within the window, scaled to [−1, 1]
	p    geo.Vec3
	cov  [6]float64
	used bool
}

type bblock struct {
	st  int32
	win int32
	deg int
	n   int // receptions
}

type bwin struct {
	f0, nf int32   // frames
	blocks []int32 // global block indices
	group  int     // jackknife group
}

type bundle struct {
	cfg *BundleConfig
	in  *BundleInput
	atm geo.Atmosphere
	ctx context.Context

	st     []bst
	frames []bframe
	recs   []brec
	blocks []bblock
	wins   []bwin
	nWin   int
	nUsed  int // stations with a slot
	slots  []int32
	t0, t1 int64

	iters    int
	released int
	vf       float64
	jkGroups int
	final    *passOut
	stage    int
	// scratch is the workers' memory, reused across passes (scratchFor)
	scratch []*scratch
}

// clockDeg is the clock polynomial degree of a class per window: GPS-
// disciplined and hardware 12 MHz clocks are linear over a window, the
// others get a quadratic term (thermal ramps, ClockModelFor).
func clockDeg(c model.ClockClass) int {
	switch c {
	case model.ClockGPSUTC, model.ClockGPSGPST, model.ClockGPSEmul, model.ClockHW12:
		return 1
	}
	return 2
}

// prepare indexes the frames into windows and clock blocks and drops what
// cannot contribute: receptions of unknown or untimed stations, second
// receptions of a station, blocks with fewer than MinBlockRecs receptions,
// stations with fewer than MinStationRecs and frames with fewer than two
// receptions (repeated until stable).
func (b *bundle) prepare() error {
	cfg := b.cfg
	in := b.in
	b.st = make([]bst, len(in.Stations))
	for i := range in.Stations {
		s := &in.Stations[i]
		st := &b.st[i]
		st.in = s
		st.deg = clockDeg(s.Class)
		st.slot = -1
		j := ClockModelFor(s.Class, false).JitterNs
		st.sig0 = j * 1e-9 * cLight
		// start with the prior jitter plus a window-fit allowance
		st.sig2 = st.sig0*st.sig0 + 15*15
		if !s.Class.Timed() || s.Class.NominalHz() == 0 {
			st.why = "untimed"
		}
	}
	// frames: time range
	b.t0, b.t1 = math.MaxInt64, math.MinInt64
	for i := range in.Frames {
		f := &in.Frames[i]
		if f.NR < 2 {
			continue
		}
		b.t0 = min(b.t0, f.TNs)
		b.t1 = max(b.t1, f.TNs)
	}
	if b.t0 > b.t1 {
		return ErrBundleTooFew
	}
	winNs := int64(cfg.WindowS * 1e9)
	nWin := int((b.t1-b.t0)/winNs) + 1
	order := make([]int, 0, len(in.Frames))
	for i := range in.Frames {
		if in.Frames[i].NR >= 2 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, c int) bool { return in.Frames[order[a]].TNs < in.Frames[order[c]].TNs })
	// per (station, epoch) tick base, for local times of bounded magnitude
	type sk struct {
		st    int32
		epoch uint32
	}
	base := map[sk]uint64{}
	b.frames = make([]bframe, 0, len(order))
	b.recs = make([]brec, 0, len(in.Recs))
	var seen []int32
	for _, fi := range order {
		f := &in.Frames[fi]
		w := int32((f.TNs - b.t0) / winNs)
		ws := float64(b.t0+int64(w)*winNs) / 1e9
		x := (float64(f.TNs)/1e9 - ws - cfg.WindowS/2) / (cfg.WindowS / 2)
		bf := bframe{win: w, r0: int32(len(b.recs)), x: x, p: f.P}
		for k := range 6 {
			bf.cov[k] = float64(f.CovP[k])
		}
		seen = seen[:0]
		tSec := float64(f.TNs-b.t0) / 1e9
		for _, r := range in.Recs[f.R0 : f.R0+f.NR] {
			st := &b.st[r.St]
			if st.why != "" || slices.Contains(seen, r.St) {
				continue
			}
			seen = append(seen, r.St)
			k := sk{r.St, r.Epoch}
			t0, ok := base[k]
			if !ok {
				base[k] = r.Ticks
				t0 = r.Ticks
			}
			local := float64(int64(r.Ticks-t0)) / st.in.Class.NominalHz()
			// y = c·(local − frame time): the frame time removes the common
			// part, the rest is range + clock (bounded magnitude).
			b.recs = append(b.recs, brec{st: r.St, blk: -1, y: cLight * (local - tSec), w: 1})
		}
		bf.nr = int32(len(b.recs)) - bf.r0
		if bf.nr < 2 {
			b.recs = b.recs[:bf.r0]
			continue
		}
		b.frames = append(b.frames, bf)
	}
	b.nWin = nWin
	if err := b.reindex(true); err != nil {
		return err
	}
	// clock offsets: subtract each block's median of y (keeps magnitudes
	// small; the clock parameters absorb any constant per block)
	byBlk := make([][]float64, len(b.blocks))
	for fi := range b.frames {
		f := &b.frames[fi]
		if !f.used {
			continue
		}
		for ri := f.r0; ri < f.r0+f.nr; ri++ {
			if r := &b.recs[ri]; r.blk >= 0 {
				byBlk[r.blk] = append(byBlk[r.blk], r.y)
			}
		}
	}
	med := make([]float64, len(b.blocks))
	for i, v := range byBlk {
		if len(v) > 0 {
			med[i] = median(v)
		}
	}
	for ri := range b.recs {
		if r := &b.recs[ri]; r.blk >= 0 {
			r.y -= med[r.blk]
		}
	}
	return nil
}

// reindex prunes (blocks with fewer than MinBlockRecs receptions, stations
// with fewer than MinStationRecs or a reason not to be used, frames with
// fewer than two receptions; repeated until stable) and numbers the clock
// blocks, windows and position slots. first: every reception is a
// candidate (afterwards pruning only removes).
func (b *bundle) reindex(first bool) error {
	cfg := b.cfg
	for pass := 0; pass < 16; pass++ {
		all := first && pass == 0
		blockOf := map[[2]int32]int32{}
		b.blocks = b.blocks[:0]
		for i := range b.st {
			b.st[i].nrec = 0
		}
		for fi := range b.frames {
			f := &b.frames[fi]
			if !f.used && !all {
				continue
			}
			for ri := f.r0; ri < f.r0+f.nr; ri++ {
				r := &b.recs[ri]
				if !all && r.blk < 0 {
					continue
				}
				key := [2]int32{r.st, f.win}
				bi, ok := blockOf[key]
				if !ok {
					bi = int32(len(b.blocks))
					blockOf[key] = bi
					b.blocks = append(b.blocks, bblock{st: r.st, win: f.win, deg: b.st[r.st].deg})
				}
				b.blocks[bi].n++
				r.blk = bi
			}
			f.used = true
		}
		// drop small blocks
		changed := false
		for fi := range b.frames {
			f := &b.frames[fi]
			if !f.used {
				continue
			}
			n := 0
			for ri := f.r0; ri < f.r0+f.nr; ri++ {
				r := &b.recs[ri]
				if r.blk < 0 {
					continue
				}
				if b.blocks[r.blk].n < cfg.MinBlockRecs || b.st[r.st].why != "" {
					r.blk = -1
					changed = true
					continue
				}
				n++
			}
			if n < 2 {
				f.used = false
				for ri := f.r0; ri < f.r0+f.nr; ri++ {
					b.recs[ri].blk = -1
				}
				changed = true
			}
		}
		// station counts
		for fi := range b.frames {
			f := &b.frames[fi]
			if !f.used {
				continue
			}
			for ri := f.r0; ri < f.r0+f.nr; ri++ {
				if r := &b.recs[ri]; r.blk >= 0 {
					b.st[r.st].nrec++
				}
			}
		}
		for i := range b.st {
			st := &b.st[i]
			if st.why == "" && st.nrec < cfg.MinStationRecs {
				st.why = "few receptions"
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	// final block numbering over used receptions only
	blockOf := map[[2]int32]int32{}
	b.blocks = b.blocks[:0]
	b.nUsed = 0
	b.slots = b.slots[:0]
	for i := range b.st {
		b.st[i].nblk = 0
		b.st[i].slot = -1
	}
	for fi := range b.frames {
		f := &b.frames[fi]
		if !f.used {
			continue
		}
		for ri := f.r0; ri < f.r0+f.nr; ri++ {
			r := &b.recs[ri]
			if r.blk < 0 {
				continue
			}
			key := [2]int32{r.st, f.win}
			bi, ok := blockOf[key]
			if !ok {
				bi = int32(len(b.blocks))
				blockOf[key] = bi
				b.blocks = append(b.blocks, bblock{st: r.st, win: f.win, deg: b.st[r.st].deg})
				b.st[r.st].nblk++
			}
			b.blocks[bi].n++
			r.blk = bi
		}
	}
	// windows
	b.wins = make([]bwin, b.nWin)
	for i := range b.wins {
		b.wins[i].f0 = -1
	}
	for fi := range b.frames {
		f := &b.frames[fi]
		w := &b.wins[f.win]
		if w.f0 < 0 {
			w.f0 = int32(fi)
		}
		w.nf = int32(fi) - w.f0 + 1
	}
	for bi := range b.blocks {
		w := &b.wins[b.blocks[bi].win]
		w.blocks = append(w.blocks, int32(bi))
	}
	// keep windows with blocks only
	k := 0
	for _, w := range b.wins {
		if len(w.blocks) > 0 {
			b.wins[k] = w
			k++
		}
	}
	b.wins = b.wins[:k]
	for i := range b.st {
		st := &b.st[i]
		if st.why == "" {
			st.slot = b.nUsed
			b.nUsed++
			b.slots = append(b.slots, int32(i))
		}
	}
	if b.nUsed < 2 {
		return ErrBundleTooFew
	}
	return nil
}

// median returns the median of v (reordered).
func median(v []float64) float64 {
	slices.Sort(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// setPriors sets every station's prior for the stage: 1 = declared loose,
// 2 = declared at DeclaredSigmaHM. Released priors stay released (only a
// height prior is kept).
func (b *bundle) setPriors(stage int) {
	cfg := b.cfg
	// median height of the stations with a position (unlocated height prior)
	var hs []float64
	for i := range b.st {
		st := &b.st[i]
		if st.slot >= 0 && (st.in.Prior == PriorAnchor || st.in.Prior == PriorDeclared) {
			hs = append(hs, geo.FromECEF(st.in.Pos).H)
		}
	}
	medH := 0.0
	if len(hs) > 0 {
		medH = median(hs)
	}
	for i := range b.st {
		st := &b.st[i]
		in := st.in
		st.hOn = false
		// height prior without a trusted position
		st.mH, st.vSig = medH, cfg.UnlocatedHeightSigmaM
		switch {
		case in.HeightSigmaM > 0:
			st.mH, st.vSig = in.HeightHAE, in.HeightSigmaM
		case in.Prior == PriorDeclared || in.Prior == PriorAnchor:
			st.mH = geo.FromECEF(in.Pos).H
		}
		if st.released {
			continue
		}
		switch in.Prior {
		case PriorAnchor:
			st.hOn = true
			st.m = in.Pos
			st.hSig = pos(in.SigmaHM, cfg.AnchorSigmaHM)
			st.mH, st.vSig = geo.FromECEF(in.Pos).H, pos(in.SigmaVM, cfg.AnchorSigmaVM)
		case PriorDeclared:
			st.hOn = true
			st.m = in.Pos
			st.mH = geo.FromECEF(in.Pos).H
			if stage == 1 {
				st.hSig = math.Max(cfg.DeclaredLooseM, in.SigmaHM)
				st.vSig = math.Max(pos(in.SigmaVM, cfg.DeclaredSigmaVM), cfg.UnlocatedHeightSigmaM/3)
			} else {
				st.hSig = pos(in.SigmaHM, cfg.DeclaredSigmaHM)
				st.vSig = pos(in.SigmaVM, cfg.DeclaredSigmaVM)
			}
			if in.HeightSigmaM > 0 && in.HeightSigmaM < st.vSig {
				st.mH, st.vSig = in.HeightHAE, in.HeightSigmaM
			}
		case PriorHint:
			st.hOn = true
			st.m = in.Pos
			st.hSig = pos(in.SigmaHM, cfg.HintSigmaM)
		}
	}
}

func pos(v, d float64) float64 {
	if v > 0 {
		return v
	}
	return d
}

// withHeight returns p moved to height h (HAE).
func withHeight(p geo.Vec3, h float64) geo.Vec3 {
	l := geo.FromECEF(p)
	l.H = h
	return l.ECEF()
}

// run is the staged adjustment: stage 1 with every declared position
// loose (each one tested against the data), the contradicted priors
// released; stage 2 with the remaining declared positions as gauge
// (DeclaredSigmaHM), then data snooping (worst first) until no prior is
// contradicted; the final pass gives the covariance.
func (b *bundle) run() error {
	b.stage = 1
	b.setPriors(1)
	if err := b.solve(b.cfg.RobustRounds); err != nil {
		return err
	}
	if err := b.snoop(true); err != nil {
		return err
	}
	b.stage = 2
	b.setPriors(2)
	if err := b.solve(2); err != nil {
		return err
	}
	for b.released < b.cfg.MaxReleases {
		any, err := b.snoopOne()
		if err != nil {
			return err
		}
		if !any {
			break
		}
		b.setPriors(2)
		if err := b.solve(2); err != nil {
			return err
		}
	}
	return b.finish()
}

func (b *bundle) result(t0 time.Time) BundleResult {
	cfg := b.cfg
	r := BundleResult{Frames: len(b.in.Frames), Windows: len(b.wins), Blocks: len(b.blocks), Iterations: b.iters,
		Released: b.released, VarianceFactor: b.vf, JackknifeGroups: b.jkGroups, Elapsed: time.Since(t0)}
	var tmin, tmax int64 = math.MaxInt64, math.MinInt64
	for fi := range b.frames {
		f := &b.frames[fi]
		if f.used {
			r.FramesUsed++
			t := b.t0 + int64((float64(f.win)*cfg.WindowS+(f.x+1)*cfg.WindowS/2)*1e9)
			tmin, tmax = min(tmin, t), max(tmax, t)
		}
	}
	if tmax > tmin {
		r.DurationS = float64(tmax-tmin) / 1e9
	}
	for i := range b.st {
		st := &b.st[i]
		in := st.in
		o := BundleStationResult{ID: in.ID, Class: in.Class, Prior: in.Prior, Why: st.why, Used: st.slot >= 0 && b.final != nil,
			Seeded: st.seeded}
		if o.Used {
			o.ECEF = st.u
			o.LLH = geo.FromECEF(st.u)
			fr := geo.NewFrameECEF(st.u)
			o.CovENU = fr.CovToENU(st.final)
			o.FormalENU = fr.CovToENU(st.formal)
			if st.jkOK {
				o.JackknifeENU = fr.CovToENU(st.jk)
			}
			o.SigmaHM, o.SigmaVM = st.sigH, st.sigV
			o.Determined = st.determined
			d := fr.ToENU(st.start)
			o.ShiftHM = math.Hypot(d.X, d.Y)
			o.Receptions, o.Rejected, o.Blocks = st.nrec-st.nrej, st.nrej, st.nblk
			o.JitterNs = math.Sqrt(st.sig2) / cLight * 1e9
			o.PriorTested, o.DataOnly, o.DataOnlySigmaHM = st.tested, st.dataOnly, st.dataSigH
			o.PriorDistHM, o.PriorLimitM = st.distH, st.limit
			o.PriorReleased = st.released
			if st.flaggedPrior {
				if in.Prior == PriorAnchor {
					o.OutlierWhy = append(o.OutlierWhy, "anchor")
				} else {
					o.OutlierWhy = append(o.OutlierWhy, "declared")
				}
			}
			if o.JitterNs > cfg.NoisyFactor*ClockModelFor(in.Class, false).JitterNs && o.JitterNs > cfg.NoisyMinNs {
				o.OutlierWhy = append(o.OutlierWhy, "noisy")
			}
			if st.nrec > 0 && float64(st.nrej) > cfg.MaxRejectFrac*float64(st.nrec) {
				o.OutlierWhy = append(o.OutlierWhy, "rejects")
			}
			o.Outlier = len(o.OutlierWhy) > 0
			r.Receptions += o.Receptions
			r.Rejected += o.Rejected
		} else if o.Why == "" {
			o.Why = "no solution"
		}
		r.Stations = append(r.Stations, o)
	}
	return r
}
