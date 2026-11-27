package feed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"airview/internal/hub"
	"airview/internal/model"
)

// hubRunner is the feed type "hub": a client of another AirView instance's
// data port.
type hubRunner struct {
	m    *Manager
	cfg  model.Feed
	opts HubOptions
}

func (m *Manager) newHubRunner(cfg model.Feed) (Runner, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("hub feed has no host")
	}
	// Called with m.mu held (from reconcileLocked): hubOpts is consistent.
	return &hubRunner{m: m, cfg: cfg, opts: m.hubOpts}, nil
}

// gpsWatch notes whether the relayed frames carry GPS timestamps.
type gpsWatch struct {
	s   *Session
	gps atomic.Bool
}

func (w *gpsWatch) HandleFrame(f model.RawFrame) {
	if f.Kind != model.FrameStatus && !f.MLAT {
		if on := f.GPSNs >= 0; on != w.gps.Load() && (on || f.Clock12MHz != 0) {
			w.gps.Store(on)
			w.s.SetGPSTime(on)
		}
	}
	w.s.HandleFrame(f)
}

func (w *gpsWatch) HandleUpdate(u model.DecodedUpdate) { w.s.HandleUpdate(u) }

// Run implements Runner. The hub client reconnects by itself.
func (r *hubRunner) Run(ctx context.Context, s *Session) error {
	o := r.m.opts
	copts := hub.ClientOptions{
		Host:        strings.TrimSpace(r.cfg.Host),
		Port:        feedPort(r.cfg),
		Password:    r.opts.Password,
		DialTimeout: o.DialTimeout,
		IdleTimeout: o.IdleTimeout,
		MinBackoff:  o.MinBackoff,
		MaxBackoff:  o.MaxBackoff,
		Logger:      o.Logger,
		OnBytes:     s.AddBytes,
		OnState: func(connected bool, addr string, err error) {
			if connected {
				s.SetConnected(addr)
			} else {
				s.SetDisconnected(err)
			}
		},
		OnStation: func(st hub.Station) {
			s.SetInfo(fmt.Sprintf("%s %s host %q (%s), %d antennas, %d feeds",
				st.App, st.Version, st.Receiver.Name, st.Receiver.Model, len(st.Antennas), len(st.Feeds)))
			if fn := r.m.handlers.Load().station; fn != nil {
				fn(r.cfg.ID, st)
			}
		},
	}
	if !r.opts.KeepHostFeedIDs {
		copts.FeedID = r.cfg.ID
	}
	s.SetGPSTime(false)
	return hub.NewClient(copts).Run(ctx, &gpsWatch{s: s})
}
