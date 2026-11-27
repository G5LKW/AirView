package engine

// Owner notification of MLAT position-source changes (DESIGN §19.3,
// H-track). The MLAT engine reports every change of a station's effective
// position source through mlat.Options.Notifier; changes that involve an
// automatically used survey are what the owner must be told about ("your
// station's survey position is now used for MLAT; reject it here"). The
// position itself is owner/admin data: the notice keeps it in memory for
// the owner's station view, and the audit row holds no coordinates.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"airview/internal/mlat"
	"airview/internal/model"
	"airview/internal/registry"
)

// PositionNotice is the last MLAT position-source change of a station.
type PositionNotice struct {
	Change mlat.PositionChange
	At     time.Time
}

// posNotices implements mlat.PositionNotifier.
type posNotices struct {
	reg *registry.Registry
	log *slog.Logger
	now func() time.Time

	mu   sync.Mutex
	last map[model.StationIdx]PositionNotice
}

func newPosNotices(reg *registry.Registry, log *slog.Logger, now func() time.Time) *posNotices {
	return &posNotices{reg: reg, log: log, now: now, last: map[model.StationIdx]PositionNotice{}}
}

// PositionChanged implements mlat.PositionNotifier. It is called
// synchronously from the engine and must not block: the audit row is
// written on its own goroutine.
func (p *posNotices) PositionChanged(c mlat.PositionChange) {
	survey := c.AutoSurvey || c.Old == mlat.PosSurvey || c.New == mlat.PosSurvey
	p.log.Info("MLAT position source changed", "station", int(c.Station), "from", c.Old.String(),
		"to", c.New.String(), "reason", c.Reason, "autoSurvey", c.AutoSurvey)
	if !survey {
		return // registry-driven changes are the owner's own actions
	}
	p.mu.Lock()
	if len(p.last) >= 100_000 {
		clear(p.last)
	}
	p.last[c.Station] = PositionNotice{Change: c, At: p.now()}
	p.mu.Unlock()
	if p.reg == nil {
		return
	}
	st, ok := p.reg.Station(c.Station)
	if !ok {
		return
	}
	detail := fmt.Sprintf(`{"from":%q,"to":%q,"autoSurvey":%t,"reason":%q,"sigmaM":%.0f}`,
		c.Old.String(), c.New.String(), c.AutoSurvey, c.Reason, c.SigmaM)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := p.reg.Audit(ctx, registry.SystemActor, "station.mlat.position", st.SID, detail); err != nil {
			p.log.Warn("MLAT position notice not stored", "station", int(c.Station), "err", err)
		}
	}()
}

// get returns the last survey-related change of st.
func (p *posNotices) get(st model.StationIdx) (PositionNotice, bool) {
	if p == nil {
		return PositionNotice{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.last[st]
	return n, ok
}

// MLATPositionNotice returns the last change of station st's MLAT position
// source that involved a survey (owner notice, DESIGN §19.3).
func (e *Engine) MLATPositionNotice(st model.StationIdx) (PositionNotice, bool) {
	if e.net == nil {
		return PositionNotice{}, false
	}
	return e.net.posNotices.get(st)
}
