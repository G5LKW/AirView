package survey

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/geoid"
	"airview/internal/ingest"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/tracker"
)

// RefPositioner gives the transmitter position of a reference aircraft
// (*ref.Store implements it: mlat.Engine.Refs()).
type RefPositioner interface {
	RefPosition(key uint32, df uint8, tUnixNs int64) (pos geo.Vec3, cov [6]float64, ok bool)
}

// BundleNotifier receives every finished bundle adjustment (admin view;
// owner notices for flagged stations). Called from the bundler's
// goroutine without locks held; must not block and must not call back
// into the bundler. The result holds exact positions: owner/admin data
// only (DESIGN §8).
type BundleNotifier interface {
	BundleFinished(r BundleResult)
}

// BundleMember is what the bundler knows about a station (the registry's
// view, pushed with SetStation).
type BundleMember struct {
	// Class is the clock class; Timestamps the owner's consent (DESIGN
	// §10.5): without it the station is never used.
	Class      model.ClockClass
	Timestamps bool
	// Anchor is a GNSS-reported or owner-confirmed position (ECEF, HAE)
	// with its horizontal 1σ (0 = BundleConfig.AnchorSigmaHM). Anchors
	// gauge the network and are tested like every prior.
	Anchor       *geo.Vec3
	AnchorSigmaM float64
	// Declared is the owner's declared position (used when there is no
	// Anchor); Hint a coarse Locate Me position.
	Declared *Declared
	Hint     *geo.LLH
}

// MemberOf converts the survey's view of a station into a BundleMember
// (the same registry sync loop feeds both). anchor is the station's GNSS
// or owner-confirmed position (ECEF, heights HAE) and anchorSigmaM its
// horizontal 1σ (0 = BundleConfig.AnchorSigmaHM); it is used only when
// s.GNSS or s.Confirmed is set, otherwise the declared position (tested)
// or the Locate Me hint is the prior.
func MemberOf(s Station, anchor *geo.Vec3, anchorSigmaM float64) BundleMember {
	m := BundleMember{Class: s.Class, Timestamps: s.Timestamps, Declared: s.Declared, Hint: s.Hint}
	if anchor != nil && (s.GNSS || s.Confirmed) {
		a := *anchor
		m.Anchor, m.AnchorSigmaM = &a, anchorSigmaM
	}
	return m
}

// subject reports whether the bundle should solve the station (it has no
// anchor position).
func (m BundleMember) subject() bool { return m.Timestamps && m.Class.Timed() && m.Anchor == nil }

// BundlerOptions configure a Bundler.
type BundlerOptions struct {
	Config BundleConfig
	// Refs is the reference store (required).
	Refs RefPositioner
	// Atmosphere returns the engine's propagation model (nil = default).
	Atmosphere func() geo.Atmosphere
	// Undulation returns the geoid height N (default internal/geoid), for
	// declared MSL heights.
	Undulation func(lat, lon float64) (float64, bool)
	// Notifier receives every result; Declared is told about declared
	// positions the bundle contradicts (and clears only its own flags).
	// Both optional.
	Notifier BundleNotifier
	Declared DeclaredFlagger
	// Now is the server clock (default time.Now); Logger (default
	// slog.Default(): counts and σ only, never positions).
	Now    func() time.Time
	Logger *slog.Logger
}

// BundlerStats are the bundler's counters (admin).
type BundlerStats struct {
	Members, Subjects, Frames, Recs int
	Groups, Matched, Thinned, NoRef uint64
	Accepted, Decimated             uint64
	Runs, Failures                  uint64
	LastRun                         time.Time
	LastStations, LastReleased      int
	LastOutliers                    int
	LastElapsed                     time.Duration
	LastError                       string
}

// bset is the immutable member/subject bitmap read by Observe.
type bset struct {
	member, subject []uint64
	nSubjects       int
}

func (s *bset) has(bits []uint64, st model.StationIdx) bool {
	i := int(st) >> 6
	return s != nil && i < len(bits) && bits[i]&(1<<(uint(st)&63)) != 0
}

type bframeRec struct {
	key uint32
	tNs int64
	p   geo.Vec3
	cov [6]float32
	r0  int32
	nr  int32
}

type brecRaw struct {
	st    model.StationIdx
	epoch uint32
	ticks uint64
}

// Bundler runs the network bundle adjustment on live data (DESIGN §10.3
// phase 4): it collects the reference frames heard by the stations
// without an anchor position and their co-observers (Observe, lock-free
// for every other group), and every Config.Every adjusts the last
// Config.Span of them (Run or RunOnce).
type Bundler struct {
	o   BundlerOptions
	cfg BundleConfig
	log *slog.Logger

	set atomic.Pointer[bset]

	mu       sync.Mutex
	members  map[model.StationIdx]BundleMember
	frames   []bframeRec
	recs     []brecRaw
	last     map[uint32]int64
	interval int64
	lastDone map[model.StationIdx]time.Time
	flagged  map[model.StationIdx]bool
	// focus is the subjects collected for the next run (at most
	// MaxStations/2, rotated after every run)
	focus  map[model.StationIdx]bool
	result *BundleResult
	stats  BundlerStats

	runMu sync.Mutex

	groups, matched, thinned, noRef, accepted, decimated atomic.Uint64
}

// NewBundler returns a Bundler.
func NewBundler(o BundlerOptions) (*Bundler, error) {
	if o.Refs == nil {
		return nil, errors.New("survey: bundler: Refs is required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Undulation == nil {
		o.Undulation = geoid.Undulation
	}
	if o.Atmosphere == nil {
		o.Atmosphere = geo.DefaultAtmosphere
	}
	cfg := o.Config
	cfg.Normalize()
	b := &Bundler{o: o, cfg: cfg, log: o.Logger, members: map[model.StationIdx]BundleMember{}, last: map[uint32]int64{},
		interval: int64(cfg.ThinInterval), lastDone: map[model.StationIdx]time.Time{}, flagged: map[model.StationIdx]bool{},
		focus: map[model.StationIdx]bool{}}
	b.publish()
	return b, nil
}

// Config returns the normalised configuration.
func (b *Bundler) Config() BundleConfig { return b.cfg }

// SetStation adds or updates a station.
func (b *Bundler) SetStation(st model.StationIdx, m BundleMember) {
	b.mu.Lock()
	b.members[st] = m
	switch {
	case !m.subject():
		delete(b.focus, st)
	case !b.focus[st] && len(b.focus) < b.focusCap():
		b.focus[st] = true
	}
	b.publishLocked()
	b.mu.Unlock()
}

// RemoveStation forgets a station (its stored receptions are no longer
// used).
func (b *Bundler) RemoveStation(st model.StationIdx) {
	b.mu.Lock()
	delete(b.members, st)
	delete(b.lastDone, st)
	delete(b.flagged, st)
	delete(b.focus, st)
	b.publishLocked()
	b.mu.Unlock()
}

func (b *Bundler) publish() {
	b.mu.Lock()
	b.publishLocked()
	b.mu.Unlock()
}

func (b *Bundler) publishLocked() {
	s := &bset{}
	for st, m := range b.members {
		if !m.Timestamps || !m.Class.Timed() {
			continue
		}
		i := int(st) >> 6
		for len(s.member) <= i {
			s.member = append(s.member, 0)
			s.subject = append(s.subject, 0)
		}
		s.member[i] |= 1 << (uint(st) & 63)
		if m.subject() && b.focus[st] {
			s.subject[i] |= 1 << (uint(st) & 63)
			s.nSubjects++
		}
	}
	b.set.Store(s)
}

// Observe implements ingest.MLATSink: long Mode S frames of usable
// reference aircraft (clock.SyncDF) heard by a subject (a station without
// an anchor position) and at least one other member are kept, at most one
// per aircraft per ThinInterval. Untimed, map-only (Beast 30004) and
// second-channel copies are never used (consent gate, DESIGN §10.5).
// Cheap for every other group; safe for concurrent use.
func (b *Bundler) Observe(g *ingest.Group) {
	if g.Kind != model.FrameModeSLong || len(g.Copies) < 2 {
		return
	}
	s := b.set.Load()
	if s == nil || s.nSubjects == 0 {
		return
	}
	b.groups.Add(1)
	nm, ns := 0, 0
	for i := range g.Copies {
		c := &g.Copies[i]
		if !c.Timed || c.MapOnly || c.SameStation || !s.has(s.member, c.Station) {
			continue
		}
		nm++
		if s.has(s.subject, c.Station) {
			ns++
		}
	}
	if ns == 0 || nm < 2 {
		return
	}
	b.matched.Add(1)
	if !g.HasResult || g.Result.Status != tracker.ResultOK || !clock.SyncDF(g.DF) {
		b.noRef.Add(1)
		return
	}
	b.mu.Lock()
	if l, ok := b.last[g.Key]; ok && g.FirstNs-l < b.interval && g.FirstNs >= l {
		b.mu.Unlock()
		b.thinned.Add(1)
		return
	}
	b.mu.Unlock()
	// the transmitter at the (approximate) emission time: FirstNs is the
	// earliest arrival, ≤ 1.5 ms late, i.e. ≤ 0.4 m of aircraft motion
	p, cov, ok := b.o.Refs.RefPosition(g.Key, g.DF, g.FirstNs)
	if !ok {
		b.noRef.Add(1)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if l, ok := b.last[g.Key]; ok && g.FirstNs-l < b.interval && g.FirstNs >= l {
		b.thinned.Add(1)
		return
	}
	b.last[g.Key] = g.FirstNs
	if len(b.frames) >= b.cfg.MaxFrames {
		b.compactLocked(g.FirstNs)
	}
	f := bframeRec{key: g.Key, tNs: g.FirstNs, p: p, r0: int32(len(b.recs))}
	for k := range 6 {
		f.cov[k] = float32(cov[k])
	}
	for i := range g.Copies {
		c := &g.Copies[i]
		if !c.Timed || c.MapOnly || c.SameStation || !s.has(s.member, c.Station) {
			continue
		}
		b.recs = append(b.recs, brecRaw{st: c.Station, epoch: c.Epoch, ticks: c.Ticks})
	}
	f.nr = int32(len(b.recs)) - f.r0
	b.frames = append(b.frames, f)
	b.accepted.Add(1)
	if len(b.last) > 4*b.cfg.MaxFrames/16+1024 {
		for k, t := range b.last {
			if g.FirstNs-t > 60*b.interval {
				delete(b.last, k)
			}
		}
	}
}

// compactLocked drops frames older than Span; if the store is still more
// than half full, every second frame is dropped and the thinning interval
// doubles (the memory bound MaxFrames).
func (b *Bundler) compactLocked(nowNs int64) {
	cut := nowNs - int64(b.cfg.Span)
	keepEvery := 1
	n := 0
	for i := range b.frames {
		if b.frames[i].tNs >= cut {
			n++
		}
	}
	if n > b.cfg.MaxFrames/2 {
		keepEvery = 2
		b.interval *= 2
		b.decimated.Add(1)
	}
	var frames []bframeRec
	var recs []brecRaw
	k := 0
	for i := range b.frames {
		f := b.frames[i]
		if f.tNs < cut {
			continue
		}
		k++
		if k%keepEvery != 0 {
			continue
		}
		r0 := int32(len(recs))
		recs = append(recs, b.recs[f.r0:f.r0+f.nr]...)
		f.r0 = r0
		frames = append(frames, f)
	}
	b.frames, b.recs = frames, recs
}

func (b *Bundler) focusCap() int { return max(b.cfg.MaxStations/2, 1) }

// rotate chooses the next focus: stations without any position (or only
// a hint) first, then declarations this bundler flagged, then the other
// declared stations; within each, the least recently adjusted first. At
// most MaxStations/2 subjects are collected for at a time, so a network
// of any size is covered in turns of Every.
func (b *Bundler) rotate() {
	b.mu.Lock()
	defer b.mu.Unlock()
	var cand []model.StationIdx
	for st, m := range b.members {
		if m.subject() {
			cand = append(cand, st)
		}
	}
	rank := func(st model.StationIdx) int {
		m := b.members[st]
		switch {
		case m.Declared == nil:
			return 0
		case b.flagged[st]:
			return 1
		}
		return 2
	}
	slices.SortFunc(cand, func(x, y model.StationIdx) int {
		if rx, ry := rank(x), rank(y); rx != ry {
			return rx - ry
		}
		tx, ty := b.lastDone[x], b.lastDone[y]
		switch {
		case tx.Before(ty):
			return -1
		case ty.Before(tx):
			return 1
		}
		return int(x) - int(y)
	})
	clear(b.focus)
	for _, st := range cand[:min(len(cand), b.focusCap())] {
		b.focus[st] = true
	}
	b.publishLocked()
}

// pruneLocked drops the frames older than cut.
func (b *Bundler) pruneLocked(cut int64) {
	k, nr := 0, int32(0)
	for i := range b.frames {
		f := b.frames[i]
		if f.tNs < cut {
			continue
		}
		copy(b.recs[nr:], b.recs[f.r0:f.r0+f.nr])
		f.r0 = nr
		nr += f.nr
		b.frames[k] = f
		k++
	}
	b.frames = b.frames[:k]
	b.recs = b.recs[:nr]
}

// Run adjusts every Config.Every until ctx ends.
func (b *Bundler) Run(ctx context.Context) {
	t := time.NewTicker(b.cfg.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := b.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				b.log.Info("bundle adjustment skipped", "err", err)
			}
		}
	}
}

// ErrBundleNoSubjects: nothing to adjust (no station without an anchor
// has enough data).
var ErrBundleNoSubjects = errors.New("survey: bundle: no station to adjust")

// RunOnce adjusts the frames of the last Span now and publishes the
// result. One run at a time.
func (b *Bundler) RunOnce(ctx context.Context) (BundleResult, error) {
	b.runMu.Lock()
	defer b.runMu.Unlock()
	now := b.o.Now()
	in, err := b.input(now)
	defer b.rotate()
	if err != nil {
		b.mu.Lock()
		b.stats.LastError = err.Error()
		b.mu.Unlock()
		return BundleResult{}, err
	}
	in.Atm = b.o.Atmosphere()
	res, err := Bundle(ctx, in, b.cfg)
	b.mu.Lock()
	b.stats.Runs++
	b.stats.LastRun = now
	b.stats.LastElapsed = res.Elapsed
	if err != nil {
		b.stats.Failures++
		b.stats.LastError = err.Error()
		b.mu.Unlock()
		return res, err
	}
	b.stats.LastError = ""
	b.stats.LastStations = len(res.Stations)
	b.stats.LastReleased = res.Released
	outliers := 0
	type flag struct {
		st      model.StationIdx
		suspect bool
		d       float64
	}
	var flags []flag
	for _, s := range res.Stations {
		b.lastDone[s.ID] = now
		if s.Outlier {
			outliers++
		}
		m, ok := b.members[s.ID]
		if !ok || m.Anchor != nil || m.Declared == nil || s.Prior != PriorDeclared {
			continue
		}
		switch {
		case s.PriorReleased && s.Determined:
			flags = append(flags, flag{s.ID, true, s.PriorDistHM})
			b.flagged[s.ID] = true
		case b.flagged[s.ID] && s.PriorTested && !s.PriorReleased && s.Determined:
			flags = append(flags, flag{s.ID, false, s.PriorDistHM})
			delete(b.flagged, s.ID)
		}
	}
	b.stats.LastOutliers = outliers
	r := res
	b.result = &r
	b.mu.Unlock()
	b.log.Info("bundle adjustment", "stations", len(res.Stations), "frames", res.FramesUsed, "released", res.Released,
		"outliers", outliers, "vf", math.Round(res.VarianceFactor*100)/100, "elapsed", res.Elapsed.Round(time.Millisecond))
	if b.o.Declared != nil {
		for _, f := range flags {
			b.o.Declared.SetDeclaredSuspect(f.st, f.suspect, f.d)
		}
	}
	if b.o.Notifier != nil {
		b.o.Notifier.BundleFinished(res)
	}
	return res, nil
}

// input selects the stations and frames of a run: the focus subjects
// with enough receptions (least recently adjusted first, most receptions
// next) up to half of MaxStations, then the members that co-observe most
// with them (anchors and other subjects, whose priors are tested too),
// then further focus subjects; frames of the last Span heard by a
// selected focus subject and at least one other selected station.
func (b *Bundler) input(now time.Time) (BundleInput, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cut := now.UnixNano() - int64(b.cfg.Span)
	b.pruneLocked(cut)
	count := map[model.StationIdx]int{}
	for i := range b.frames {
		f := &b.frames[i]
		if f.tNs < cut {
			continue
		}
		for _, r := range b.recs[f.r0 : f.r0+f.nr] {
			if m, ok := b.members[r.st]; ok && m.Timestamps {
				count[r.st]++
			}
		}
	}
	var subj, others []model.StationIdx
	for st, n := range count {
		if n < b.cfg.MinStationRecs {
			continue
		}
		m := b.members[st]
		if m.subject() && b.focus[st] {
			subj = append(subj, st)
		} else {
			others = append(others, st)
		}
	}
	if len(subj) == 0 {
		return BundleInput{}, ErrBundleNoSubjects
	}
	slices.SortFunc(subj, func(x, y model.StationIdx) int {
		tx, ty := b.lastDone[x], b.lastDone[y]
		switch {
		case tx.Before(ty):
			return -1
		case ty.Before(tx):
			return 1
		case count[x] != count[y]:
			return count[y] - count[x]
		}
		return int(x) - int(y)
	})
	maxS := b.cfg.MaxStations
	sel := map[model.StationIdx]bool{}
	var order []model.StationIdx
	for _, st := range subj[:min(len(subj), max(maxS/2, 1))] {
		sel[st] = true
		order = append(order, st)
	}
	// co-observation counts of the others with the selected subjects
	co := map[model.StationIdx]int{}
	for i := range b.frames {
		f := &b.frames[i]
		if f.tNs < cut {
			continue
		}
		rs := b.recs[f.r0 : f.r0+f.nr]
		hit := false
		for _, r := range rs {
			if sel[r.st] {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		for _, r := range rs {
			if !sel[r.st] {
				co[r.st]++
			}
		}
	}
	slices.SortFunc(others, func(x, y model.StationIdx) int {
		if co[x] != co[y] {
			return co[y] - co[x]
		}
		return int(x) - int(y)
	})
	for _, st := range others {
		if len(order) >= maxS {
			break
		}
		if co[st] > 0 {
			sel[st] = true
			order = append(order, st)
		}
	}
	for _, st := range subj {
		if len(order) >= maxS {
			break
		}
		if !sel[st] {
			sel[st] = true
			order = append(order, st)
		}
	}
	idx := map[model.StationIdx]int32{}
	var in BundleInput
	for _, st := range order {
		idx[st] = int32(len(in.Stations))
		in.Stations = append(in.Stations, b.stationLocked(st))
	}
	for i := range b.frames {
		f := &b.frames[i]
		if f.tNs < cut {
			continue
		}
		r0 := int32(len(in.Recs))
		subjects := 0
		for _, r := range b.recs[f.r0 : f.r0+f.nr] {
			k, ok := idx[r.st]
			if !ok {
				continue
			}
			if b.focus[r.st] && b.members[r.st].subject() {
				subjects++
			}
			in.Recs = append(in.Recs, BundleRec{St: k, Epoch: r.epoch, Ticks: r.ticks})
		}
		nr := int32(len(in.Recs)) - r0
		if nr < 2 || subjects == 0 {
			in.Recs = in.Recs[:r0]
			continue
		}
		in.Frames = append(in.Frames, BundleFrame{Key: f.key, TNs: f.tNs, P: f.p, CovP: f.cov, R0: r0, NR: nr})
	}
	return in, nil
}

// stationLocked converts a member into a bundle station (declared MSL
// heights to HAE; an ambiguous reference as MSL with a wider σ).
func (b *Bundler) stationLocked(st model.StationIdx) BundleStation {
	m := b.members[st]
	s := BundleStation{ID: st, Class: m.Class}
	switch {
	case m.Anchor != nil:
		s.Prior, s.Pos, s.SigmaHM = PriorAnchor, *m.Anchor, m.AnchorSigmaM
	case m.Declared != nil:
		d := m.Declared
		h := d.Alt
		s.Prior = PriorDeclared
		if d.AltRef != model.AltRefHAE {
			n, ok := b.o.Undulation(d.Lat, d.Lon)
			if ok {
				h += n
			}
			if d.AltRef != model.AltRefMSL || !ok {
				s.SigmaVM = math.Max(b.cfg.DeclaredSigmaVM, 60)
			}
		}
		s.Pos = geo.ToECEF(d.Lat, d.Lon, h)
	case m.Hint != nil:
		s.Prior, s.Pos = PriorHint, m.Hint.ECEF()
	}
	return s
}

// Last returns the last result (nil = none). Owner/admin data only.
func (b *Bundler) Last() *BundleResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.result == nil {
		return nil
	}
	r := *b.result
	r.Stations = slices.Clone(r.Stations)
	return &r
}

// Stats returns the counters.
func (b *Bundler) Stats() BundlerStats {
	b.mu.Lock()
	s := b.stats
	s.Members = len(b.members)
	if set := b.set.Load(); set != nil {
		s.Subjects = set.nSubjects
	}
	s.Frames, s.Recs = len(b.frames), len(b.recs)
	b.mu.Unlock()
	s.Groups, s.Matched, s.Thinned, s.NoRef = b.groups.Load(), b.matched.Load(), b.thinned.Load(), b.noRef.Load()
	s.Accepted, s.Decimated = b.accepted.Load(), b.decimated.Load()
	return s
}
