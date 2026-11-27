// Command mlatsim runs the complete MLAT pipeline on a simulated network:
// simnet receivers → frames delivered with network latency → the real
// ingest grouper → the MLAT engine (internal/mlat, asynchronous workers) →
// published positions, compared with the simulator's truth. It serves the
// engine's admin and public JSON while it runs, and prints a summary.
//
//	mlatsim -rx 100 -aircraft 600 -dur 10m -listen 127.0.0.1:18611
//
// GET /stats, /quality, /public, /syncmatrix, /station?st=N on -listen.
// Time is virtual (as fast as the machine allows); nothing is written to
// disk.
package main

import (
	"container/heap"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/tracker"
)

type handler struct{}

func (handler) Handle(f model.RawFrame) tracker.Result {
	k, _ := tracker.RouteKey(f.Data)
	var df uint8
	if len(f.Data) > 0 {
		df = f.Data[0] >> 3
	}
	r := tracker.Result{Status: tracker.ResultOK, Key: k, DF: df}
	if (df == 17 || (df == 18 && f.Data[0]&7 < 2)) && len(f.Data) == 14 {
		if tc := f.Data[4] >> 3; (tc >= 9 && tc <= 18) || (tc >= 20 && tc <= 22) {
			r.Pos = true
		}
	}
	return r
}
func (handler) HandleUpdate(model.DecodedUpdate) {}
func (handler) Lanes() *tracker.Lanes            { return nil }

type item struct {
	at, seq int64
	f       model.RawFrame
}
type pq []item

func (h pq) Len() int { return len(h) }
func (h pq) Less(i, j int) bool {
	return h[i].at < h[j].at || (h[i].at == h[j].at && h[i].seq < h[j].seq)
}
func (h pq) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pq) Push(x any)   { *h = append(*h, x.(item)) }
func (h *pq) Pop() any {
	o := *h
	it := o[len(o)-1]
	*h = o[:len(o)-1]
	return it
}

type driver struct {
	n      *simnet.Network
	ing    *ingest.Ingest
	e      *mlat.Engine
	q      pq
	seq    int64
	mu     sync.Mutex // guards now
	now    int64
	sweep  int64
	nextAd int64
	lastAt map[int]int64
	epoch  map[int]uint32
	gps    []int
	nextG  float64
	rng    *rand.Rand
	centre geo.Vec3
	radius float64
	errs   []float64
	in1    int
	frames int
}

func (d *driver) clock() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Unix(0, d.now)
}

func (d *driver) setNow(t int64) {
	d.mu.Lock()
	d.now = max(d.now, t)
	d.mu.Unlock()
}

func (d *driver) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		if o.Kind != model.FrameModeSShort && o.Kind != model.FrameModeSLong {
			continue
		}
		f := d.n.RawFrame(o)
		f.Data = append([]byte(nil), f.Data...)
		if ep, ok := d.epoch[o.Rx]; ok && ep != o.Epoch {
			var reason model.ClockEventReason
			for _, ev := range d.n.ClockEvents(o.Rx) {
				if ev.NewEpoch == o.Epoch {
					reason = ev.Reason
				}
			}
			d.ing.ClockEvent(f.Station, ingest.ClockEvent{OldEpoch: ep, NewEpoch: o.Epoch, Reason: reason, HostNs: o.HostNs})
		}
		d.epoch[o.Rx] = o.Epoch
		at := max(o.HostNs+int64((20+60*d.rng.Float64())*1e6), d.lastAt[o.Rx])
		d.lastAt[o.Rx] = at
		d.seq++
		heap.Push(&d.q, item{at: at, seq: d.seq, f: f})
		d.frames++
	}
}

func (d *driver) Advance(hz float64) {
	until := d.n.UnixNs(hz)
	for len(d.q) > 0 && d.q[0].at <= until {
		it := heap.Pop(&d.q).(item)
		d.step(it.at)
		d.ing.Submit([]model.RawFrame{it.f}, false)
	}
	d.step(until)
	for ; d.nextG <= hz; d.nextG++ {
		for _, i := range d.gps {
			ct, _ := d.n.ClockAt(i, d.nextG)
			d.e.Filter().GPS(model.StationIdx(i+2), clock.GPSStatus{HostNs: d.n.UnixNs(d.nextG), Locked: ct.Locked, UTC: !ct.GPSTime})
		}
	}
}

func (d *driver) step(t int64) {
	if d.sweep == 0 {
		d.sweep = t
	}
	for d.sweep+10e6 <= t {
		d.sweep += 10e6
		d.setNow(d.sweep)
		d.ing.Sweep(time.Unix(0, d.sweep))
		if d.sweep >= d.nextAd {
			d.e.Advance(d.sweep)
			d.nextAd = d.sweep + 100e6
		}
		d.collect()
	}
	d.setNow(t)
}

// collect compares the published positions with the truth (the truth
// history is short, so this runs often).
func (d *driver) collect() {
	for {
		select {
		case f := <-d.e.Fixes():
			if f.Flags&model.FixFlagHoldout != 0 {
				continue
			}
			tr, ok := d.truth(f.ICAO, f.TUnixNs)
			if !ok {
				continue
			}
			p := geo.Vec3{X: f.ECEF[0], Y: f.ECEF[1], Z: f.ECEF[2]}
			fr := geo.NewFrame(geo.LLH{Lat: tr.Lat, Lon: tr.Lon, H: tr.HAE})
			dv := fr.Rotate(p.Sub(geo.Vec3{X: tr.Pos[0], Y: tr.Pos[1], Z: tr.Pos[2]}))
			c := f.Cov
			ce := fr.CovToENU([9]float64{float64(c[0]), float64(c[1]), float64(c[2]), float64(c[1]), float64(c[3]), float64(c[4]),
				float64(c[2]), float64(c[4]), float64(c[5])})
			cf := geo.NewFrameECEF(d.centre)
			ic := cf.ToENU(geo.Vec3{X: tr.Pos[0], Y: tr.Pos[1], Z: tr.Pos[2]})
			if math.Hypot(ic.X, ic.Y) > d.radius {
				continue
			}
			e := math.Hypot(dv.X, dv.Y)
			d.errs = append(d.errs, e)
			if e <= math.Sqrt(ce[0]+ce[4]) {
				d.in1++
			}
		default:
			return
		}
	}
}

func (d *driver) truth(icao uint32, tNs int64) (at simnet.AircraftTruth, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return d.n.AircraftAt(icao, float64(tNs-d.n.UnixNs(0))/1e9)
}

func main() {
	rx := flag.Int("rx", 100, "receivers")
	ac := flag.Int("aircraft", 600, "aircraft")
	radius := flag.Float64("radius", 300, "network radius, km")
	dur := flag.Duration("dur", 10*time.Minute, "simulated duration")
	seed := flag.Uint64("seed", 1, "seed")
	workers := flag.Int("workers", 0, "engine workers (0 = GOMAXPROCS)")
	holdout := flag.Float64("holdout", 0.1, "hold-out fraction")
	listen := flag.String("listen", "127.0.0.1:18611", "HTTP listener for /stats ('' = none)")
	mix := flag.String("mix", "default", "device mix: default, gps, sdr")
	flag.Parse()

	pl := simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: *radius}
	switch *mix {
	case "gps":
		pl.Mix = map[simnet.Device]float64{simnet.DeviceRadarcapeGPS: 1}
	case "sdr":
		pl.Mix = map[simnet.Device]float64{simnet.DeviceSDR: 1}
	}
	specs := simnet.RandomReceivers(*seed, *rx, pl)
	n, err := simnet.New(simnet.Config{Seed: *seed, Aircraft: *ac, Receivers: specs, ModeSOnlyFraction: 0.3, CenterLat: 51, CenterLon: 9})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	d := &driver{n: n, lastAt: map[int]int64{}, epoch: map[int]uint32{}, rng: rand.New(rand.NewPCG(*seed, 5)),
		centre: geo.ToECEF(51, 9, 0), radius: *radius * 1000, now: n.UnixNs(0)}
	cfg := model.DefaultAggregator().MLAT
	cfg.Holdout = *holdout
	a := n.Config().Atmosphere
	e, err := mlat.New(mlat.Options{Config: cfg, OriginUnixNs: n.UnixNs(0), Now: d.clock, Workers: *workers,
		Atmosphere: geo.Atmosphere{N0: a.N0, H: a.ScaleHeightM, K: a.KAtm},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	d.e = e
	d.ing, err = ingest.New(ingest.Options{Handler: handler{}, MLAT: e, Clock: e.ClockView(), ClockObserver: e, Now: d.clock,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for i, r := range n.Receivers() {
		e.SetStation(model.StationIdx(i+2), mlat.StationInfo{Class: r.Class,
			Declared: mlat.Position{Valid: true, ECEF: geo.Vec3{X: r.Pos[0], Y: r.Pos[1], Z: r.Pos[2]}, SigmaM: 5}})
		if r.Device == simnet.DeviceRadarcapeGPS {
			d.gps = append(d.gps, i)
		}
	}
	n.AddSink(d)

	if *listen != "" {
		mux := http.NewServeMux()
		js := func(w http.ResponseWriter, v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) { js(w, e.Stats()) })
		mux.HandleFunc("/quality", func(w http.ResponseWriter, r *http.Request) { js(w, e.QualityReport(60, true)) })
		mux.HandleFunc("/public", func(w http.ResponseWriter, r *http.Request) { js(w, e.PublicQuality()) })
		mux.HandleFunc("/syncmatrix", func(w http.ResponseWriter, r *http.Request) { js(w, e.SyncMatrix()) })
		mux.HandleFunc("/station", func(w http.ResponseWriter, r *http.Request) {
			st, _ := strconv.Atoi(r.URL.Query().Get("st"))
			rep, ok := e.Station(model.StationIdx(st))
			if !ok {
				http.NotFound(w, r)
				return
			}
			js(w, rep)
		})
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
		fmt.Printf("listening on %s\n", ln.Addr())
	}

	start := time.Now()
	ctx := context.Background()
	step := time.Minute
	for el := time.Duration(0); el < *dur; el += step {
		if err := n.Advance(ctx, min(step, *dur-el)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		st := e.Stats()
		q := e.PublicQuality()
		fmt.Printf("t=%v wall=%v groups=%d clusters=%d solves=%d published=%d dropped=%d proc=%.1fµs/group synced-anchors=%d hold-out P50=%.1f m (n=%d)\n",
			el+step, time.Since(start).Round(time.Second), st.Groups, st.Clusters, st.Solves, st.Published, st.Dropped, st.ProcUsPerGroup,
			anchors(e), q.P50M, q.Samples)
	}
	_ = e.Drain(ctx)
	d.collect()
	st := e.Stats()
	slices.Sort(d.errs)
	qv := func(p float64) float64 {
		if len(d.errs) == 0 {
			return math.NaN()
		}
		return d.errs[min(len(d.errs)-1, int(p*float64(len(d.errs))))]
	}
	cpu := st.ProcUsPerGroup * float64(st.Groups+st.Late) / 1e6
	fmt.Printf("done: %d receivers, %d aircraft, %v simulated in %v (%d frames)\n", *rx, *ac, *dur, time.Since(start).Round(time.Second), d.frames)
	fmt.Printf("published inside %.0f km vs truth: n=%d P50 %.1f m P95 %.1f m P99 %.1f m, within 1σ %.1f %%\n",
		*radius, len(d.errs), qv(0.5), qv(0.95), qv(0.99), 100*float64(d.in1)/math.Max(1, float64(len(d.errs))))
	q := e.QualityReport(60, false)
	fmt.Printf("hold-out (engine, vs ADS-B reference, total): track n=%d P50 %.1f P95 %.1f cross-track P50 %.1f; fixes n=%d P50 %.1f\n",
		q.TrackTotal.N, q.TrackTotal.P50, q.TrackTotal.P95, q.TrackTotal.CrossP50, q.FixTotal.N, q.FixTotal.P50)
	fmt.Printf("engine CPU %.2f s for %.0f s simulated (%.3f cores), %.1f µs/group, dropped %d, workers %d, clock last epoch %.1f ms\n",
		cpu, dur.Seconds(), cpu/dur.Seconds(), st.ProcUsPerGroup, st.Dropped, st.Workers, float64(st.Clock.LastEpochNs)/1e6)
	e.Close()
}

func anchors(e *mlat.Engine) int {
	n := 0
	for _, sc := range e.Filter().Snapshot().Stations() {
		if sc.Anchor {
			n++
		}
	}
	return n
}
