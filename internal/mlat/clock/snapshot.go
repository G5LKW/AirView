package clock

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Snapshot is an immutable view of every station clock after one epoch,
// published with an atomic pointer (DESIGN §9.2.3). Readers never lock.
// Times are seconds since OriginUnixNs ("network seconds"); offsets are in
// nanoseconds.
type Snapshot struct {
	Seq          uint64
	T            float64 // epoch time of the states (network seconds)
	OriginUnixNs int64
	EpochS       float64

	stations   []*StationClock // indexed by StationIdx
	Components []*ComponentInfo
	shiftS     int64 // internal time origin = OriginUnixNs + shiftS s (Config.ReoriginS)
	frameRef   func(model.ClockClass, model.FrameKind) float64
	prop       func(rx, ac geo.Vec3) float64
	pos        map[model.StationIdx]geo.Vec3
}

// ComponentInfo describes one jointly estimated component (or partition).
type ComponentInfo struct {
	ID       uint32
	Domain   uint32 // time domain: components of one domain share network time
	Relative bool   // no GPS anchor: network time is the mean of the free clocks
	Anchors  int
	Stations []model.StationIdx
	// N is the state dimension and P the full state covariance at T
	// (row-major N×N; units ns, ppb, ppb/s). Shared, read-only.
	N int
	P []float64
}

// StationClock is the published state of one station clock.
type StationClock struct {
	Station      model.StationIdx
	Class        model.ClockClass // effective class
	Declared     model.ClockClass // class the station declared
	Epoch        uint32
	BaseTicks    uint64
	BaseS        float64 // local seconds at BaseTicks (since OriginUnixNs)
	baseS        float64 // the same in internal seconds (since OriginUnixNs + shiftS)
	shiftS       int64
	Hz           float64
	Comp, Domain uint32
	comp         *ComponentInfo
	off          int
	layout       [numKinds]int8
	t            float64 // epoch time of the state (internal seconds)
	mode         mode
	psi          bool
	qScale       float64
	prior        classPrior
	ppsVar       float64

	// State at T: θ (ns), φ (ppb), ψ (ppb/s), β_short (ns). For anchors θ
	// is δ and φ = ψ = 0.
	Theta, Phi, Psi, Beta float64
	// Cov is the marginal covariance of (θ, φ, ψ, β) (zero rows for absent
	// states).
	Cov [4][4]float64

	Anchor       bool
	AnchorState  string
	AnchorReason string
	Synced       bool
	Suspect      bool
	RelativeTime bool

	SigmaThetaNs float64
	JitterNs     float64
	QScale       float64
	JitterScale  float64
	Accepted30s  int
	OutlierPct   float64 // EWMA of rejected members, %
	Steps        int
	Events       []EventRecord
	Peers        []PeerStat
}

// PeerStat are the sync statistics of a station against one peer.
type PeerStat struct {
	Peer    model.StationIdx
	Count   int     // accepted co-observations in the last 30 s window
	RMSNs   float64 // RMS of the member's leave-one-out residual against this reference peer
	RelPPB  float64 // relative frequency φ_self − φ_peer (ppb)
	SameDom bool
}

// Station returns a station's clock, or nil.
func (s *Snapshot) Station(st model.StationIdx) *StationClock {
	if s == nil || int(st) >= len(s.stations) {
		return nil
	}
	return s.stations[st]
}

// Stations returns every station clock (read-only).
func (s *Snapshot) Stations() []*StationClock {
	out := make([]*StationClock, 0, len(s.stations))
	for _, sc := range s.stations {
		if sc != nil {
			out = append(out, sc)
		}
	}
	return out
}

// Local converts ticks of the station's current epoch to local seconds
// (seconds since the origin on the station's nominal clock), with the frame
// reference of kind removed.
func (sc *StationClock) Local(ticks uint64, ref float64) float64 {
	return sc.localInt(ticks, ref) + float64(sc.shiftS)
}

// localInt is Local in internal seconds.
func (sc *StationClock) localInt(ticks uint64, ref float64) float64 {
	return float64(int64(ticks-sc.BaseTicks))/sc.Hz + sc.baseS - ref*1e-9
}

// offsetAt returns θ(t) (+ β for short frames) in ns and its variance
// including the process noise accumulated since the epoch time.
func (sc *StationClock) offsetAt(t float64, short bool) (mean, vr float64) {
	dl := t - sc.t
	h := sc.hrow(dl, short)
	mean = sc.Theta + sc.Phi*dl + sc.Psi*dl*dl/2
	if short {
		mean += sc.Beta
	}
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			vr += h[i] * sc.Cov[i][j] * h[j]
		}
	}
	vr += sc.procVar(dl)
	return mean, vr
}

func (sc *StationClock) hrow(dl float64, short bool) [4]float64 {
	h := [4]float64{1, dl, dl * dl / 2, 0}
	if sc.mode == modeAnchor {
		h[1], h[2] = 0, 0
	}
	if !sc.psi {
		h[2] = 0
	}
	if short {
		h[3] = 1
	}
	return h
}

func (sc *StationClock) procVar(dt float64) float64 {
	dt = math.Abs(dt)
	if sc.mode == modeAnchor {
		return sc.qScale * qDelta * dt
	}
	v := sc.qScale * (sc.prior.qTheta*dt + sc.prior.qPhi*dt*dt*dt/3)
	if sc.psi {
		v += sc.qScale * sc.prior.qPsi * dt * dt * dt * dt * dt / 20
	}
	return v
}

// NetFromLocal converts a local time y (seconds since the origin, frame
// reference removed) to network time t solving t + θ(t) + β·short = y. It
// returns t (network seconds) and its variance (ns²).
func (sc *StationClock) NetFromLocal(y float64, short bool) (t, varNs2 float64) {
	sh := float64(sc.shiftS)
	t, varNs2 = sc.netInt(y-sh, short)
	return t + sh, varNs2
}

// netInt is NetFromLocal in internal seconds.
func (sc *StationClock) netInt(y float64, short bool) (t, varNs2 float64) {
	t = y - sc.Theta*1e-9
	for i := 0; i < 3; i++ {
		m, _ := sc.offsetAt(t, short)
		t = y - m*1e-9
	}
	_, varNs2 = sc.offsetAt(t, short)
	return t, varNs2
}

// Query is one timestamp for NetTime / Convert.
type Query struct {
	Station model.StationIdx
	Epoch   uint32
	Ticks   uint64
	Kind    model.FrameKind
}

// NetTime converts a station timestamp to network time (seconds since the
// origin) with its 1-sigma uncertainty (ns). ok is false when the station
// is unknown, the epoch is not the current one, or the station is not
// synced.
func (s *Snapshot) NetTime(q Query) (t, sigmaNs float64, ok bool) {
	sc := s.Station(q.Station)
	if sc == nil || sc.Epoch != q.Epoch || sc.Hz == 0 || !sc.Synced {
		return 0, 0, false
	}
	short := q.Kind == model.FrameModeSShort
	y := sc.localInt(q.Ticks, s.frameRef(sc.Class, q.Kind))
	t, v := sc.netInt(y, short)
	t += float64(sc.shiftS)
	return t, math.Sqrt(math.Max(v, 0)), true
}

// NetTimeTotal is NetTime with the station's per-frame timestamp noise (its
// jitter and, for the GPS classes, the 1PPS error) included in sigmaNs: the
// uncertainty of one frame's network time, as a consistency test between
// two copies needs it (ingest's synced-station gate).
func (s *Snapshot) NetTimeTotal(q Query) (t, sigmaNs float64, ok bool) {
	sc := s.Station(q.Station)
	if sc == nil || sc.Epoch != q.Epoch || sc.Hz == 0 || !sc.Synced {
		return 0, 0, false
	}
	short := q.Kind == model.FrameModeSShort
	y := sc.localInt(q.Ticks, s.frameRef(sc.Class, q.Kind))
	t, v := sc.netInt(y, short)
	t += float64(sc.shiftS)
	v += sc.JitterNs * sc.JitterNs
	if sc.Class == model.ClockGPSUTC || sc.Class == model.ClockGPSGPST {
		v += sc.ppsVar
	}
	return t, math.Sqrt(math.Max(v, 0)), true
}

// Convert converts m timestamps to network time for the solver: t[i] gets
// the network seconds and cov (m×m, row-major) the full covariance in ns²
// of the conversions (marginal blocks and cross-covariances of stations in
// the same component; the receivers' own jitter is not included). ok[i] is
// false for unusable timestamps (their rows are zero). Timestamps of
// stations in different time domains are not comparable: callers must use
// one domain per solve (StationClock.Domain). Convert does not allocate.
func (s *Snapshot) Convert(qs []Query, t, cov []float64, ok []bool) {
	m := len(qs)
	var hs [64][4]float64
	for i := 0; i < m; i++ {
		t[i], ok[i] = 0, false
		sc := s.Station(qs[i].Station)
		if sc == nil || sc.Epoch != qs[i].Epoch || !sc.Synced {
			continue
		}
		short := qs[i].Kind == model.FrameModeSShort
		y := sc.localInt(qs[i].Ticks, s.frameRef(sc.Class, qs[i].Kind))
		ti, _ := sc.netInt(y, short)
		t[i], ok[i] = ti+float64(sc.shiftS), true
		if i < len(hs) {
			hs[i] = sc.hrow(ti-sc.t, short)
		}
	}
	for i := 0; i < m; i++ {
		for j := 0; j < m; j++ {
			cov[i*m+j] = 0
		}
	}
	for i := 0; i < m && i < len(hs); i++ {
		if !ok[i] {
			continue
		}
		a := s.Station(qs[i].Station)
		for j := 0; j <= i && j < len(hs); j++ {
			if !ok[j] {
				continue
			}
			b := s.Station(qs[j].Station)
			if a.comp != b.comp || a.comp == nil {
				continue
			}
			n := a.comp.N
			P := a.comp.P
			v := 0.0
			for p := 0; p < 4; p++ {
				lp := a.layout[p]
				if lp < 0 || hs[i][p] == 0 {
					continue
				}
				row := P[(a.off+int(lp))*n+b.off:]
				for q := 0; q < 4; q++ {
					lq := b.layout[q]
					if lq < 0 || hs[j][q] == 0 {
						continue
					}
					v += hs[i][p] * row[lq] * hs[j][q]
				}
			}
			if i == j {
				v += a.procVar(t[i] - float64(a.shiftS) - a.t)
			}
			cov[i*m+j], cov[j*m+i] = v, v
		}
	}
}

// Position returns a station's position as known to the filter.
func (s *Snapshot) Position(st model.StationIdx) (geo.Vec3, bool) {
	p, ok := s.pos[st]
	return p, ok
}

// PropagationNs returns the propagation time from ac to station st (ns).
func (s *Snapshot) PropagationNs(st model.StationIdx, ac geo.Vec3) (float64, bool) {
	p, ok := s.pos[st]
	if !ok {
		return 0, false
	}
	return s.prop(p, ac), true
}

// NetTimeUnix is NetTime in the form of internal/mlat/group.Clock: the
// network time as whole Unix nanoseconds plus a fraction (a float64 Unix-ns
// value would only resolve 256 ns), the variance of the clock-state part of
// the conversion and the station's per-frame timestamp noise variance (its
// jitter plus, for GPS classes, the 1PPS error), both in ns².
//
// The conversion is carried out in split form (whole seconds as an integer
// plus a small float64 remainder), so its resolution does not depend on
// the uptime: a float64 "seconds since the origin" has an ulp of 0.93 ns
// after 48.5 days and 3.7 ns after 194 days, which every TOA would
// otherwise pick up (finding M6).
func (s *Snapshot) NetTimeUnix(q Query) (ns int64, frac, clockVar, jitterVar float64, ok bool) {
	sc := s.Station(q.Station)
	if sc == nil || sc.Epoch != q.Epoch || sc.Hz == 0 || !sc.Synced {
		return 0, 0, 0, 0, false
	}
	short := q.Kind == model.FrameModeSShort
	w, f := sc.localSplit(q.Ticks, s.frameRef(sc.Class, q.Kind))
	sec, rem, v := sc.netFromLocalSplit(w, f, short)
	tn := rem * 1e9
	fl := math.Floor(tn)
	jit := sc.JitterNs * sc.JitterNs
	if sc.Class == model.ClockGPSUTC || sc.Class == model.ClockGPSGPST {
		jit += sc.ppsVar
	}
	return s.OriginUnixNs + (sec+sc.shiftS)*1e9 + int64(fl), tn - fl, math.Max(v, 0), jit, true
}

// localSplit is localInt in split form: internal local seconds = w + f with w whole
// seconds and |f| small. The tick count is divided exactly when the
// nominal rate is an integer (every timed class).
func (sc *StationClock) localSplit(ticks uint64, ref float64) (w int64, f float64) {
	n := int64(ticks - sc.BaseTicks)
	if hz := int64(sc.Hz); hz > 0 && float64(hz) == sc.Hz {
		w = n / hz
		f = float64(n-w*hz) / sc.Hz
	} else {
		x := float64(n) / sc.Hz
		w = int64(math.Floor(x))
		f = x - float64(w)
	}
	bw := math.Floor(sc.baseS)
	w += int64(bw)
	f += sc.baseS - bw - ref*1e-9
	return w, f
}

// netFromLocalSplit is NetFromLocal in split form: the offset θ(t) (+ β) is
// evaluated at the approximate network time (its slope is ppm at most, so
// the approximation does not matter) and subtracted from the small
// remainder only. It returns network time = sec + rem seconds and the
// variance (ns²).
func (sc *StationClock) netFromLocalSplit(w int64, f float64, short bool) (sec int64, rem, varNs2 float64) {
	t, _ := sc.netInt(float64(w)+f, short)
	m, v := sc.offsetAt(t, short)
	rem = f - m*1e-9
	r := math.Floor(rem)
	return w + int64(r), rem - r, v
}

// CrossCov returns the covariance (ns²) of the clock offsets θ of two
// synced stations of the same component at the snapshot time (long
// frames), 0 otherwise.
func (s *Snapshot) CrossCov(a, b model.StationIdx) float64 {
	sa, sb := s.Station(a), s.Station(b)
	if sa == nil || sb == nil || sa.comp == nil || sa.comp != sb.comp || !sa.Synced || !sb.Synced {
		return 0
	}
	n := sa.comp.N
	return sa.comp.P[(sa.off+int(sa.layout[kTheta]))*n+sb.off+int(sb.layout[kTheta])]
}

// Domain returns the time domain of a station (network times of different
// domains are not comparable).
func (s *Snapshot) Domain(st model.StationIdx) (uint64, bool) {
	sc := s.Station(st)
	if sc == nil || sc.comp == nil {
		return 0, false
	}
	return uint64(sc.Domain), true
}

// SyncedPeers returns the number of peers the station was synchronised
// against in the last 30 s window (the mlat-client "peer_count").
func (sc *StationClock) SyncedPeers() int {
	n := 0
	for _, p := range sc.Peers {
		if p.Count > 0 && p.SameDom {
			n++
		}
	}
	return n
}
