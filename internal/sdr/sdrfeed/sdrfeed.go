// Package sdrfeed runs an RTL-SDR dongle (through the user's own rtl_tcp)
// as a feed.Manager input of type model.FeedRTLTCP ("rtltcp"), for both
// airview (standalone) and airview-feeder. The demodulator is internal/sdr
// (pure Go, clean room); rtl_tcp itself is never shipped or downloaded.
package sdrfeed

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/sdr"
)

// Options builds the sdr receiver options of an rtltcp feed.
func Options(cfg model.Feed) sdr.Options {
	host, port := cfg.Host, cfg.Port
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = model.DefaultRTLTCPPort
	}
	o := sdr.Options{
		Addr:       net.JoinHostPort(host, strconv.Itoa(port)),
		Gain:       cfg.SDR.Gain,
		PPM:        cfg.SDR.PPM,
		BiasTee:    cfg.SDR.BiasTee,
		SampleRate: uint32(max(cfg.SDR.SampleRate, 0)),
		FeedID:     cfg.ID,
	}
	o.Demod.AcceptUnverifiedAP = cfg.SDR.AcceptUnverifiedAP
	return o
}

// Factory is the feed.Factory of model.FeedRTLTCP. logf may be nil.
func Factory(logf func(format string, args ...any)) feed.Factory {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return func(cfg model.Feed) (feed.Runner, error) {
		o := Options(cfg)
		if err := o.Validate(); err != nil {
			return nil, err
		}
		return feed.RunnerFunc(func(ctx context.Context, s *feed.Session) error {
			return run(ctx, s, cfg, o, logf)
		}), nil
	}
}

func run(ctx context.Context, s *feed.Session, cfg model.Feed, o sdr.Options, logf func(string, ...any)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	if cfg.SDR.Spawn {
		path := cfg.SDR.RTLTCPPath
		if path == "" {
			path = sdr.FindRTLTCP()
		}
		if path == "" {
			err := fmt.Errorf("rtl_tcp was not found on PATH or next to the program (install rtl-sdr; AirView does not ship it)")
			s.SetDisconnected(err)
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sdr.RunChild(ctx, sdr.ChildOptions{Path: path, Addr: o.Addr, Device: cfg.SDR.Device,
				SampleRate: o.SampleRate, Logf: logf}, nil)
		}()
	}
	var mu sync.Mutex
	var last uint32
	o.Logf = logf
	o.OnClockEvent = func(epoch uint32, reason model.ClockEventReason) {
		mu.Lock()
		old := last
		last = epoch
		mu.Unlock()
		oldCls := model.ClockNone
		if old != 0 {
			oldCls = model.ClockSDR12
		}
		s.ReportClockEvent(feed.ClockEvent{OldEpoch: old, NewEpoch: epoch, Reason: reason,
			OldClass: oldCls, NewClass: model.ClockSDR12, HostNs: ntp.Now().UnixNano()})
	}
	r := sdr.NewReceiver(o)
	wg.Add(1)
	go func() { // connection state for the feed status
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		was := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			st := r.Stats()
			switch {
			case st.Connected && !was:
				s.SetConnected(st.Addr)
				info := "RTL-SDR via rtl_tcp"
				if st.Tuner != "" {
					info += ", tuner " + st.Tuner
				}
				s.SetInfo(info)
			case !st.Connected && was:
				var err error
				if st.LastError != "" {
					err = fmt.Errorf("%s", st.LastError)
				}
				s.SetDisconnected(err)
			}
			was = st.Connected
		}
	}()
	err := r.Run(ctx, s)
	cancel()
	return err
}
