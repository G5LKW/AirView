// Command avfsrv-load runs the AVF/1 server with the real ingest, tracker
// and registry (-serve), or a fleet of simulated AVF feeders (-load) that
// connect to it over TLS, so the network ingest can be load-tested with
// server and feeders in separate processes.
//
//	avfsrv-load -serve 127.0.0.1:18521 -data <dir> -duration 90s
//	avfsrv-load -load 127.0.0.1:18521 -data <dir> -feeders 1000 -aircraft 300 -duration 60s
//
// The server writes its TLS pin and server ID to <dir>/avfsrv-load.json;
// the load generator reads them from there. Admission is open. Both print a
// status line every 5 s. Nothing here is used by the application.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/avf"
	"airview/internal/ingest"
	"airview/internal/ingest/avfsrv"
	"airview/internal/model"
	"airview/internal/registry"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
	"airview/internal/uplink"
)

var heapProfile string

type info struct {
	Pin      string `json:"pin"`
	ServerID string `json:"serverId"`
}

func main() {
	serve := flag.String("serve", "", "run the server on this address")
	load := flag.String("load", "", "run feeders against this server address")
	data := flag.String("data", "", "data directory (registry, TLS, server ID)")
	dur := flag.Duration("duration", 60*time.Second, "run time")
	feeders := flag.Int("feeders", 100, "load: number of feeders (stations)")
	aircraft := flag.Int("aircraft", 300, "load: simulated aircraft")
	radius := flag.Float64("radius", 400, "load: radius of the station area, km")
	full := flag.Bool("full", false, "load: do not offer selective traffic (every frame is sent; INTEREST / FLOW are still honoured)")
	flag.StringVar(&heapProfile, "heapprofile", "", "serve: write a heap profile here at the end (live sessions still open)")
	flag.Parse()
	if *data == "" || (*serve == "") == (*load == "") {
		fmt.Fprintln(os.Stderr, "usage: avfsrv-load (-serve addr | -load addr) -data dir [-duration d] [-feeders n] [-aircraft n]")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, *dur)
	defer cancel2()
	var err error
	if *serve != "" {
		err = runServer(ctx, *serve, *data)
	} else {
		err = runLoad(ctx, *load, *data, *feeders, *aircraft, *radius, !*full)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func liveHeap() float64 {
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return float64(s[0].Value.Uint64())
}

func runServer(ctx context.Context, addr, dir string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	reg, err := registry.OpenDir(dir, registry.Options{Policy: registry.Policy{Admission: model.AdmissionOpen, MaxStations: 100000}, Logger: log})
	if err != nil {
		return err
	}
	defer reg.Close()
	go reg.Run(ctx)
	agg := model.DefaultAggregator()
	agg.Admission = model.AdmissionOpen
	agg.Limits.MaxConnsPerIP, agg.Limits.MaxConnsPerSubnet, agg.Limits.MaxUnauthPerIP = 100000, 100000, 100000
	agg.Limits.PerIPStations = 100000
	tlsCfg, pin, err := avfsrv.TLSFromConfig(dir, agg)
	if err != nil {
		return err
	}
	sid, err := avfsrv.LoadOrCreateServerID(dir)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(info{Pin: pin, ServerID: sid})
	if err := os.WriteFile(filepath.Join(dir, "avfsrv-load.json"), b, 0o600); err != nil {
		return err
	}
	cfg := model.Config{}
	lanes := tracker.NewLanes(tracker.LaneOptions{Resolver: reg.Resolver()})
	tr := tracker.New(tracker.Options{Config: &cfg, Logger: log})
	tr.SetNetwork(reg.Resolver(), lanes)
	go tr.Run(ctx)
	io := ingest.OptionsFromConfig(agg)
	io.Handler, io.Resolver, io.Logger = tr, reg.Resolver(), log
	ing, err := ingest.New(io)
	if err != nil {
		return err
	}
	go ing.Run(ctx)
	o := avfsrv.OptionsFromConfig(agg)
	o.Addr, o.TLSConfig, o.ServerID, o.Resolver, o.Logger = addr, tlsCfg, sid, reg.Resolver(), log
	o.Policy.Limits = agg.Limits
	srv, err := avfsrv.New(o, reg, ing)
	if err != nil {
		return err
	}
	fmt.Printf("avfsrv-load server on %s, pin %s, server id %s\n", addr, pin, sid)
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var last ingest.Stats
	lastCPU, lastT := processCPU(), time.Now()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			if heapProfile != "" {
				writeHeap(heapProfile)
			}
			srv.Close()
			st := ing.Stats()
			fmt.Printf("final: %d frames, %d unique, %d joined, %d too old, %d late; server %+v\n",
				st.Frames, st.Groups, st.Joined, st.TooOld, st.Late, srv.Stats())
			return nil
		case now := <-t.C:
			st := ing.Stats()
			dt := now.Sub(lastT).Seconds()
			cpu := processCPU()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			ss := srv.Stats()
			fmt.Printf("%s sessions %d | %.0f frames/s, %.0f unique/s, %.1f copies/unique | tooOld %d late %d | cpu %.0f %% of one core | heap live %.0f MB, sys %.0f MB | goroutines %d | aircraft %d | tracker hold %.0f ns\n",
				now.Format("15:04:05"), ss.Open, float64(st.Frames-last.Frames)/dt, float64(st.Groups-last.Groups)/dt,
				float64(st.Frames-last.Frames)/max(1, float64(st.Groups-last.Groups)), st.TooOld, st.Late,
				100*(cpu-lastCPU)/dt, liveHeap()/1e6, float64(ms.Sys)/1e6, runtime.NumGoroutine(),
				tr.Snapshot().Counts.Aircraft, tr.LockStats().HoldNs)
			last, lastCPU, lastT = st, cpu, now
		}
	}
}

// ------------------------------------------------------------ load

// writer encodes simnet AVF records onto one client session.
type writer struct {
	c      *avf.Conn
	seq    uint64
	frames atomic.Uint64
	held   atomic.Uint64 // frames the INTEREST / FLOW filter kept back
	fr     avf.Frames
	err    error

	mu     sync.Mutex
	filt   *uplink.Filter
	paused bool
}

// control applies the server's INTEREST and FLOW records (reader goroutine).
func (w *writer) control(rec avf.Record) {
	switch rec.Type {
	case avf.TInterest:
		var in avf.Interest
		if avf.DecodeJSON(rec.Payload, &in) == nil {
			w.mu.Lock()
			w.filt.Apply(&in)
			w.mu.Unlock()
		}
	case avf.TFlow:
		var fl avf.Flow
		if avf.DecodeJSON(rec.Payload, &fl) == nil {
			w.mu.Lock()
			w.paused = fl.Pause
			w.filt.SetSlow(fl.SlowFactor)
			w.mu.Unlock()
			if fl.BatchMs >= 10 && fl.BatchMs <= 5000 {
				w.c.SetBatch(time.Duration(fl.BatchMs) * time.Millisecond)
			}
		}
	}
}

// selective reports whether the server put this feeder in selective mode.
func (w *writer) selective() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.filt.Selective()
}

func (w *writer) next() uint64 { w.seq++; return w.seq }

func (w *writer) Connect(int64, int) {}

func (w *writer) Receiver(_ int64, r *simnet.AVFReceiver) {
	if w.err != nil {
		return
	}
	w.err = w.c.WriteSeqJSON(avf.TReceiver, w.next(), &avf.Receiver{RxNo: r.RxNo, Name: r.Name, ClockClass: r.ClockClass.String(),
		NominalHz: r.NominalHz, EpochID: r.EpochID, Channels: max(r.Channels, 1), Decoder: r.Decoder, Timestamps: r.Timestamps})
}

func (w *writer) Frames(_ int64, f *simnet.AVFFrames) {
	if w.err != nil || len(f.Frames) == 0 {
		return
	}
	w.fr = avf.Frames{RxNo: uint8(f.RxNo), EpochID: f.EpochID, BaseTicks: f.BaseTicks, BaseHostNs: f.BaseHostNs, Frames: w.fr.Frames[:0]}
	w.mu.Lock()
	for _, x := range f.Frames {
		if w.paused || !w.filt.Pass(x.Payload, x.HostNs) {
			w.held.Add(1)
			continue
		}
		ch := uint8(0)
		if x.Channel >= 0 {
			ch = uint8(x.Channel + 1)
		}
		w.fr.Frames = append(w.fr.Frames, avf.Frame{Ticks: x.Ticks, HostNs: x.HostNs, Data: x.Payload, Channel: ch,
			Sig: x.Sig, Late: x.Late, TsInvalid: x.TsInvalid})
	}
	w.mu.Unlock()
	if len(w.fr.Frames) == 0 {
		return
	}
	w.fr.Seq = w.next()
	w.err = w.c.WriteFrames(&w.fr)
	w.frames.Add(uint64(len(w.fr.Frames)))
}

func (w *writer) ClockStatus(_ int64, s *simnet.AVFClockStatus) {
	if w.err != nil {
		return
	}
	cs := avf.ClockStatus{Seq: w.next(), RxNo: uint8(s.RxNo), HostNs: s.HostNs, Settings: s.Settings, GPSStatus: s.GPSStatus,
		PPSOffsetTicks: int64(s.PPSOffsetTicks), Raw: s.Raw}
	w.err = w.c.Send(avf.TClockStatus, func(d []byte) ([]byte, error) { return avf.AppendClockStatus(d, &cs) })
}

func (w *writer) ClockPair(_ int64, p *simnet.AVFClockPair) {
	if w.err != nil {
		return
	}
	cp := avf.ClockPair{Seq: w.next(), RxNo: uint8(p.RxNo), EpochID: p.EpochID, Ticks: p.Ticks, HostUTCNs: p.HostUTCNs, HostErrEstNs: p.HostErrEstNs}
	w.err = w.c.Send(avf.TClockPair, func(d []byte) ([]byte, error) { return avf.AppendClockPair(d, &cp), nil })
}

func (w *writer) ClockEvent(_ int64, e *simnet.AVFClockEvent) {
	if w.err != nil {
		return
	}
	w.err = w.c.WriteSeqJSON(avf.TClockEvent, w.next(), &avf.ClockEvent{RxNo: e.RxNo, OldEpoch: e.OldEpoch, NewEpoch: e.NewEpoch,
		Reason: e.Reason, SamplesLost: int64(e.SamplesLost), HostNs: e.HostNs})
}

func (w *writer) Backfill(_ int64, b *simnet.AVFBackfill) {
	if w.err != nil {
		return
	}
	w.err = w.c.WriteSeqJSON(avf.TBackfill, w.next(), &avf.Backfill{Phase: b.Phase, SpanNs: b.SpanNs})
}

func (w *writer) Flush(int64) {}

func runLoad(ctx context.Context, addr, dir string, feeders, aircraft int, radius float64, selective bool) error {
	b, err := os.ReadFile(filepath.Join(dir, "avfsrv-load.json"))
	if err != nil {
		return fmt.Errorf("server info: %w", err)
	}
	var in info
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	specs := simnet.RandomReceivers(41, feeders, simnet.Placement{CenterLat: 51, CenterLon: 5, RadiusKm: radius})
	n, err := simnet.New(simnet.Config{Seed: 41, Start: time.Now(), Aircraft: aircraft, Receivers: specs})
	if err != nil {
		return err
	}
	tlsCfg, err := avf.ClientTLSConfig("127.0.0.1", in.Pin, nil)
	if err != nil {
		return err
	}
	ws := make([]*writer, feeders)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	var fail atomic.Int64
	for i := 0; i < feeders; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			id, _ := avf.NewIdentity()
			feats := avf.AllFeatures
			if !selective {
				feats = slices.DeleteFunc(slices.Clone(avf.AllFeatures), func(f string) bool { return f == avf.FeatSelective })
			}
			c, _, err := avf.Dial(ctx, addr, tlsCfg, avf.ClientConfig{Identity: id, ServerID: in.ServerID,
				Body: avf.HelloBody{Software: avf.SoftwareFeeder, Version: "load", SeqEpoch: avf.NewSeqEpoch(), Features: feats}})
			if err != nil {
				fail.Add(1)
				return
			}
			w := &writer{c: c, filt: uplink.NewFilter()}
			go func() { // PING / ACK are handled by the session; INTEREST / FLOW here
				for {
					rec, err := c.ReadRecord()
					if err != nil {
						return
					}
					w.control(rec)
				}
			}()
			ws[i] = w
		}(i)
	}
	wg.Wait()
	fmt.Printf("connected %d feeders (%d failed)\n", feeders-int(fail.Load()), fail.Load())
	for i, w := range ws {
		if w != nil {
			n.NewAVFEmitter(i, w, simnet.AVFOptions{})
		}
	}
	start := time.Now()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var last, lastHeld uint64
	lastT := start
	for ctx.Err() == nil {
		// pace the simulation to wall time
		if ahead := time.Duration(n.NowS()*1e9) - time.Since(start); ahead > 0 {
			time.Sleep(min(ahead, 100*time.Millisecond))
			continue
		}
		if err := n.Step(ctx); err != nil {
			break
		}
		select {
		case now := <-t.C:
			var tot, held uint64
			alive, sel := 0, 0
			for _, w := range ws {
				if w != nil {
					tot += w.frames.Load()
					held += w.held.Load()
					if w.err == nil {
						alive++
					}
					if w.selective() {
						sel++
					}
				}
			}
			lag := time.Since(start) - time.Duration(n.NowS()*1e9)
			dt := now.Sub(lastT).Seconds()
			fmt.Printf("%s feeders alive %d (selective %d) | sent %.0f frames/s | held back %.0f frames/s | simulation lag %v\n",
				now.Format("15:04:05"), alive, sel, float64(tot-last)/dt, float64(held-lastHeld)/dt, lag.Round(time.Millisecond))
			last, lastHeld, lastT = tot, held, now
		default:
		}
	}
	for _, w := range ws {
		if w != nil {
			_ = w.c.Goodbye("shutting_down", "load test done", 0)
		}
	}
	return nil
}

func writeHeap(path string) {
	runtime.GC()
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "heap profile:", err)
		return
	}
	defer f.Close()
	if err := pprof.WriteHeapProfile(f); err != nil {
		fmt.Fprintln(os.Stderr, "heap profile:", err)
	}
	fmt.Printf("heap profile written (live heap %.0f MB)\n", liveHeap()/1e6)
}
