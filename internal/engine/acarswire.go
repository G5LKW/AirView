package engine

// ACARS / VDL2 message input (internal/acars). Off by default (acars.enabled).
// When on, the listeners accept the JSON output of an external decoder
// (acarsdec, dumpvdl2) and keep the newest messages in memory. Messages
// without an aircraft address are linked to the live Mode S picture by
// registration. Demo mode runs the store with synthetic messages (station
// "DEMO") for the aircraft on the simulated picture, and also the
// listeners when acars.enabled is set.
//
// Nothing here touches the frame path, the database or public views.

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"airview/internal/acars"
	"airview/internal/model"
)

// demoACARSEvery is the mean interval of synthetic demo messages.
const demoACARSEvery = 6 * time.Second

type acarsWiring struct {
	cfg    model.ACARSConfig
	demo   bool
	st     *acars.Store
	l      *acars.Listener // nil when only the demo runs
	cancel context.CancelFunc
	done   chan struct{}

	regMu  sync.Mutex
	regIdx map[string]string // NormReg -> ICAO
	regAt  time.Time
}

func (w *acarsWiring) close() {
	w.cancel()
	<-w.done
}

// ACARSStatus is the state of the ACARS input (GET /api/acars/status).
type ACARSStatus struct {
	Enabled bool               `json:"enabled"`
	Demo    bool               `json:"demo"`
	Input   *acars.InputStatus `json:"input,omitempty"`
	Stats   *acars.Stats       `json:"stats,omitempty"`
}

// syncACARS starts, rebuilds or stops the input after a configuration
// change. The stored messages survive a change of the listen addresses.
func (e *Engine) syncACARS(cfg model.Config, demo bool) {
	want := cfg.ACARS
	cur := e.acars.Load()
	if !want.Enabled && !demo {
		if cur != nil {
			e.acars.Store(nil)
			cur.close()
			e.log.Info("ACARS input stopped")
		}
		return
	}
	if cur != nil && cur.cfg == want && cur.demo == demo {
		return
	}
	var st *acars.Store
	if cur != nil && cur.cfg.Keep == want.Keep {
		st = cur.st
	} else {
		st = acars.NewStore(want.Keep)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &acarsWiring{cfg: want, demo: demo, st: st, cancel: cancel, done: make(chan struct{})}
	st.SetResolver(w.resolver(e))
	if want.Enabled {
		w.l = acars.NewListener(st, acars.ListenConfig{UDP: want.UDPAddr(), TCP: want.TCPListen}, e.log)
	}
	if old := e.acars.Swap(w); old != nil {
		old.close() // releases the ports before the new listener binds
	}
	go func() {
		defer close(w.done)
		var wg sync.WaitGroup
		if w.l != nil {
			wg.Add(1)
			go func() { defer wg.Done(); _ = w.l.Run(ctx) }()
		}
		if demo {
			wg.Add(1)
			go func() { defer wg.Done(); e.demoACARS(ctx, st) }()
		}
		wg.Wait()
	}()
	e.log.Info("ACARS input running", "udp", want.UDPAddr(), "tcp", want.TCPListen, "demo", demo)
}

// resolver links a registration to the live picture. The index is rebuilt
// from the (cached) snapshot at most every 5 s.
func (w *acarsWiring) resolver(e *Engine) acars.Resolver {
	return func(reg string) string {
		w.regMu.Lock()
		defer w.regMu.Unlock()
		if now := time.Now(); w.regIdx == nil || now.Sub(w.regAt) > 5*time.Second {
			snap := e.tr.Snapshot()
			idx := make(map[string]string, len(snap.Aircraft))
			for i := range snap.Aircraft {
				a := &snap.Aircraft[i]
				if a.Registration != "" && a.AddrType != "modeac" {
					idx[acars.NormReg(a.Registration)] = a.ICAO
				}
			}
			w.regIdx, w.regAt = idx, now
		}
		return w.regIdx[reg]
	}
}

// demoACARS adds synthetic messages for random aircraft of the live
// picture.
func (e *Engine) demoACARS(ctx context.Context, st *acars.Store) {
	rnd := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0xAC))
	t := time.NewTimer(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(time.Duration(float64(demoACARSEvery) * (0.3 + 1.4*rnd.Float64())))
		snap := e.tr.Snapshot()
		var cand []*model.Aircraft
		for i := range snap.Aircraft {
			a := &snap.Aircraft[i]
			if a.Callsign != "" && (a.AddrType == "" || a.AddrType == "icao") {
				cand = append(cand, a)
			}
		}
		if len(cand) == 0 {
			continue
		}
		a := cand[rnd.IntN(len(cand))]
		now := time.Now()
		st.Add(acars.DemoMessage(rnd, a.ICAO, a.Registration, a.Callsign, now), now)
	}
}

// closeACARS stops the input (Stop).
func (e *Engine) closeACARS() {
	if w := e.acars.Swap(nil); w != nil {
		w.close()
	}
}

// ACARSMessages returns stored messages (ok false when the input is off).
func (e *Engine) ACARSMessages(f acars.Filter) ([]acars.Message, bool) {
	w := e.acars.Load()
	if w == nil {
		return nil, false
	}
	return w.st.List(f), true
}

// ACARSStatus returns the state of the input.
func (e *Engine) ACARSStatus() ACARSStatus {
	w := e.acars.Load()
	if w == nil {
		return ACARSStatus{}
	}
	s := ACARSStatus{Enabled: w.cfg.Enabled, Demo: w.demo}
	if w.l != nil {
		in := w.l.Status()
		s.Input = &in
	}
	st := w.st.Stats()
	s.Stats = &st
	return s
}
