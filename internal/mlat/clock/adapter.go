package clock

import (
	"airview/internal/feed"
	"airview/internal/ingest"
	"airview/internal/model"
	"airview/internal/tracker"
)

// ObserveGroup turns a closed ingest group into a sync observation when its
// aircraft has a usable reference track (DESIGN §9.2.2) and queues it. It
// returns whether the group was admitted. Only Timed copies are used
// (Copy.Timed already excludes Beast 30004 / MapOnly copies, stations
// without timestamp consent and second channels of a diversity receiver).
// It does not allocate.
func (f *Filter) ObserveGroup(g *ingest.Group, ref RefSource) bool {
	if ref == nil || !g.HasResult || g.Result.Status != tracker.ResultOK || !SyncDF(g.DF) {
		return false
	}
	if g.Kind != model.FrameModeSShort && g.Kind != model.FrameModeSLong {
		return false
	}
	var buf [64]Member
	ms := buf[:0]
	for i := range g.Copies {
		c := &g.Copies[i]
		if !c.Timed || c.MapOnly || c.SameStation || !c.ClockCls.Timed() {
			continue
		}
		if len(ms) == len(buf) {
			break
		}
		ms = append(ms, Member{Station: c.Station, Epoch: c.Epoch, Ticks: c.Ticks})
	}
	if len(ms) < 2 {
		return false
	}
	pos, cov, ok := ref.RefPosition(g.Key, g.DF, g.FirstNs)
	if !ok {
		return false
	}
	o := SyncObs{Key: g.Key, Kind: g.Kind, HostNs: g.FirstNs, Pos: pos, PosCov: cov, Members: ms}
	return f.AddSync(&o)
}

// ClockEvent implements ingest.ClockObserver.
func (f *Filter) ClockEvent(st model.StationIdx, ev ingest.ClockEvent) {
	if ev.MapOnly {
		return
	}
	f.StationEvent(st, Event{OldEpoch: ev.OldEpoch, NewEpoch: ev.NewEpoch, Reason: ev.Reason,
		SamplesLost: ev.SamplesLost, HostNs: ev.HostNs})
}

// ClockPair implements ingest.ClockObserver. The (ticks, host UTC) pairs
// carry no information beyond what the sync observations and their host
// times give the filter; they are ignored.
func (f *Filter) ClockPair(model.StationIdx, ingest.ClockPair) {}

// ClockStatus implements ingest.ClockObserver: Radarcape status frames give
// the GPS state for anchor condition (a) and immediate demotion.
func (f *Filter) ClockStatus(st model.StationIdx, s ingest.ClockStatus) {
	bs, ok := feed.ParseBeastStatus([]byte{s.Settings, 0, s.GPSStatus})
	if !ok || !bs.GPSTimestamps {
		return
	}
	f.GPS(st, GPSStatus{HostNs: s.HostNs, Locked: bs.GPSDetail && bs.GPSLocked() && bs.FPGASync, UTC: bs.UTC})
}

// View implements ingest.ClockView on the filter's latest snapshot.
// NetTime returns nanoseconds since Config.OriginUnixNs (not Unix ns: a
// float64 Unix-ns value would be quantised to 256 ns); values of different
// time domains are not comparable.
type View struct {
	F   *Filter
	Ref RefSource
}

var _ ingest.ClockView = View{}
var _ ingest.ClockObserver = (*Filter)(nil)

// NetTime implements ingest.ClockView (long-frame reference).
func (v View) NetTime(st model.StationIdx, epoch uint32, ticks uint64) (tNs, sigmaNs float64, ok bool) {
	t, sig, ok := v.F.Snapshot().NetTime(Query{Station: st, Epoch: epoch, Ticks: ticks, Kind: model.FrameModeSLong})
	return t * 1e9, sig, ok
}

// BaselineNs implements ingest.ClockView.
func (v View) BaselineNs(a, b model.StationIdx) (float64, bool) {
	s := v.F.Snapshot()
	pa, oka := s.Position(a)
	pb, okb := s.Position(b)
	if !oka || !okb {
		return 0, false
	}
	return pa.Dist(pb) / cMPerNs, true
}

// SyncCandidate implements ingest.ClockView.
func (v View) SyncCandidate(key uint32, df uint8) bool {
	return SyncDF(df) && v.Ref != nil && v.Ref.Usable(key)
}

// GateView is the ingest.ClockView for the synced-station gate as the
// HOOKS request WP-M-engine -> internal/ingest (items 1 and 4) asks for it:
// it also implements ingest.ClockKindView, so the gate compares network
// times of the frame's own kind (frame reference and β_short), with σ
// including the receivers' per-frame jitter (Snapshot.NetTimeTotal). With
// ingest.Options.ClockGateFloor (default 50 µs) the gate then separates
// only identical transmissions (≥ one reply length apart) and never the
// copies of a clock that has drifted by a few σ.
//
// View is kept unchanged (long-frame times, clock-state σ only) because
// internal/mlat embeds it and overrides NetTime; GateView is a separate
// type so that switching is the engine's explicit choice.
type GateView struct {
	F   *Filter
	Ref RefSource
}

var (
	_ ingest.ClockView     = GateView{}
	_ ingest.ClockKindView = GateView{}
)

// NetTime implements ingest.ClockView (long frames, σ with jitter); the
// grouper uses NetTimeKind.
func (v GateView) NetTime(st model.StationIdx, epoch uint32, ticks uint64) (tNs, sigmaNs float64, ok bool) {
	return v.NetTimeKind(st, epoch, ticks, model.FrameModeSLong)
}

// NetTimeKind implements ingest.ClockKindView: ns since Config.OriginUnixNs.
func (v GateView) NetTimeKind(st model.StationIdx, epoch uint32, ticks uint64, kind model.FrameKind) (tNs, sigmaNs float64, ok bool) {
	t, sig, ok := v.F.Snapshot().NetTimeTotal(Query{Station: st, Epoch: epoch, Ticks: ticks, Kind: kind})
	return t * 1e9, sig, ok
}

// BaselineNs implements ingest.ClockView.
func (v GateView) BaselineNs(a, b model.StationIdx) (float64, bool) {
	return View{F: v.F}.BaselineNs(a, b)
}

// SyncCandidate implements ingest.ClockView.
func (v GateView) SyncCandidate(key uint32, df uint8) bool {
	return SyncDF(df) && v.Ref != nil && v.Ref.Usable(key)
}
