package plan

import (
	"slices"

	"airview/internal/model"
)

// ModeACOut is told when a station starts (on) or stops taking part in
// Mode A/C MLAT (DESIGN §9.9). Calls come from the Replan goroutine, in
// station order, only on changes.
type ModeACOut interface {
	ModeAC(st model.StationIdx, on bool)
}

// ModeACFunc adapts a function to ModeACOut.
type ModeACFunc func(st model.StationIdx, on bool)

// ModeAC implements ModeACOut.
func (f ModeACFunc) ModeAC(st model.StationIdx, on bool) { f(st, on) }

// MLATCModeAC forwards Mode A/C participation to an mlat-client session
// server (internal/ingest/mlatc: SetModeACAll asks the station's client for
// every Mode A/C reply).
type MLATCModeAC struct {
	C interface {
		SetModeACAll(st model.StationIdx, on bool) bool
	}
}

// ModeAC implements ModeACOut.
func (o MLATCModeAC) ModeAC(st model.StationIdx, on bool) { o.C.SetModeACAll(st, on) }

// ModeACFanout tells several outputs.
type ModeACFanout []ModeACOut

// ModeAC implements ModeACOut.
func (f ModeACFanout) ModeAC(st model.StationIdx, on bool) {
	for _, o := range f {
		o.ModeAC(st, on)
	}
}

// modeACNotify reports the difference between the previous Mode A/C set
// and next (nil = none) to the ModeACOut. Called with p.rp held.
func (p *Planner) modeACNotify(next map[model.StationIdx]struct{}) {
	if p.acOut == nil {
		return
	}
	var prev map[model.StationIdx]struct{}
	if m := p.modeA.Load(); m != nil {
		prev = *m
	}
	var ch []model.StationIdx
	for st := range prev {
		if _, ok := next[st]; !ok {
			ch = append(ch, st)
		}
	}
	for st := range next {
		if _, ok := prev[st]; !ok {
			ch = append(ch, st)
		}
	}
	slices.Sort(ch)
	for _, st := range ch {
		_, on := next[st]
		p.acOut.ModeAC(st, on)
	}
}

// ModeACStations returns the stations taking part in Mode A/C MLAT, in
// order (admin scope).
func (p *Planner) ModeACStations() []model.StationIdx {
	m := p.modeA.Load()
	if m == nil {
		return nil
	}
	out := make([]model.StationIdx, 0, len(*m))
	for st := range *m {
		out = append(out, st)
	}
	slices.Sort(out)
	return out
}
