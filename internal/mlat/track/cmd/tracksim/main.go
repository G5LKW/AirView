// Command tracksim drives the MLAT tracker with a simulated receiver
// network and reports its error against the simulation truth: simnet
// world and receivers → per-transmission TOA clusters in network time
// (truth arrival + the receiver's real tick error + a modelled clock-filter
// residual) → mlat/solve fixes (until a track is confirmed) → mlat/track.
// Live counters are served as JSON on -listen (/stats).
//
//	go run ./internal/mlat/track/cmd/tracksim -stations 20 -device sdr -minutes 5
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/solve"
	"airview/internal/mlat/track"
	"airview/internal/sim/simnet"
)

type group struct {
	txS  float64
	icao uint32
	pos  geo.Vec3
	hae  float64
	alt  bool
	toas []track.TOA
}

type stats struct {
	mu        sync.Mutex
	SimS      float64        `json:"simS"`
	Groups    int            `json:"groups"`
	Solves    int            `json:"solves"`
	SolveOK   int            `json:"solveOk"`
	Results   map[string]int `json:"results"`
	Tracker   track.Stats    `json:"tracker"`
	ErrInside []float64      `json:"-"`
	ErrOut    []float64      `json:"-"`
	CovIn     int            `json:"-"`
	CovOut    int            `json:"-"`
	Summary   any            `json:"summary"`
	UpdateNs  float64        `json:"meanUpdateNs"`
	nUpd      float64
}

func pct(v []float64, q float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	return c[max(0, min(len(c)-1, int(math.Ceil(q*float64(len(c))))-1))]
}

func (s *stats) summary() map[string]any {
	f := func(e []float64, cov int) map[string]any {
		if len(e) == 0 {
			return map[string]any{"n": 0}
		}
		return map[string]any{"n": len(e), "p50M": pct(e, .5), "p95M": pct(e, .95), "p99M": pct(e, .99),
			"within2SigmaFrac": float64(cov) / float64(len(e))}
	}
	return map[string]any{"insideFootprint": f(s.ErrInside, s.CovIn), "outside": f(s.ErrOut, s.CovOut)}
}

func main() {
	nst := flag.Int("stations", 8, "receivers")
	dev := flag.String("device", "radarcape_gps", "device: radarcape_gps, beast, sdr, airspy")
	nac := flag.Int("aircraft", 150, "aircraft")
	minutes := flag.Float64("minutes", 3, "simulated minutes")
	radius := flag.Float64("radius", 150, "receiver placement radius, km")
	seed := flag.Uint64("seed", 1, "seed")
	listen := flag.String("listen", "127.0.0.1:18605", "stats listener (empty = none)")
	hold := flag.Duration("hold", 0, "keep serving /stats this long after the run")
	flag.Parse()

	const lat, lon = 50.5, 8.5
	specs := simnet.RandomReceivers(*seed, *nst, simnet.Placement{CenterLat: lat, CenterLon: lon, RadiusKm: *radius,
		Mix: map[simnet.Device]float64{simnet.Device(*dev): 1}})
	n, err := simnet.New(simnet.Config{Seed: *seed, Aircraft: *nac, Receivers: specs, RangeKm: 2 * *radius})
	if err != nil {
		log.Fatal(err)
	}
	st := &stats{Results: map[string]int{}}
	if *listen != "" {
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
			st.mu.Lock()
			st.Summary = st.summary()
			b, _ := json.MarshalIndent(st, "", "  ")
			st.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go srv.Serve(ln)
		defer srv.Close()
		log.Printf("stats on http://%s/stats", ln.Addr())
	}

	cfg := n.Config()
	rng := rand.New(rand.NewPCG(*seed, 77))
	pos := make([]geo.Vec3, len(cfg.Receivers))
	white := make([]float64, len(pos))
	syncS := make([]float64, len(pos))
	syncV := make([]float64, len(pos))
	syncT := make([]float64, len(pos))
	for i, r := range cfg.Receivers {
		rt := n.Receiver(i)
		pos[i] = geo.Vec3{X: rt.Pos[0], Y: rt.Pos[1], Z: rt.Pos[2]}
		rate := r.Clock.SampleRateHz
		if rate <= 0 {
			rate = map[simnet.Device]float64{simnet.DeviceRadarcapeGPS: 1e9, simnet.DeviceSDR: 2.4e6, simnet.DeviceAirspy: 20e6, simnet.DeviceSBS: 20e6}[r.Clock.Device]
			if rate == 0 {
				rate = 12e6
			}
		}
		q := 1 / rate
		white[i] = math.Sqrt(math.Pow(r.Clock.JitterNs*1e-9, 2) + q*q/12)
		syncS[i] = map[simnet.Device]float64{simnet.DeviceRadarcapeGPS: 30e-9, simnet.DeviceBeast: 50e-9, simnet.DeviceRadarcape12: 50e-9, simnet.DeviceAirspy: 80e-9}[r.Clock.Device]
		if syncS[i] == 0 {
			syncS[i] = 120e-9
		}
		syncV[i] = rng.NormFloat64() * syncS[i]
	}
	cen := geo.ToECEF(lat, lon, 0)
	cf := geo.NewFrameECEF(cen)
	atm := geo.DefaultAtmosphere()
	tr := track.New(track.Params{Atmosphere: atm})
	sv := solve.NewSolver(solve.Options{Atmosphere: atm})
	lastAlt := map[uint32][2]float64{}
	open := map[uint64]*group{}
	var sst []solve.Station

	handle := func(g *group) {
		st.Groups++
		base := math.Floor(g.txS)
		t0 := n.UnixNs(base)
		c := track.Cluster{ICAO: g.icao, T0Ns: t0}
		for _, m := range g.toas {
			m.T -= base
			c.TOAs = append(c.TOAs, m)
		}
		var est track.Estimate
		if g.alt {
			c.AltHAE, c.AltSigma = g.hae+25*rng.NormFloat64(), 25
			lastAlt[g.icao] = [2]float64{g.txS, c.AltHAE}
		} else if la, ok := lastAlt[g.icao]; ok && g.txS-la[0] < 10 {
			vz := 15.0
			if tr.Get(g.icao, &est) {
				vz = math.Abs(est.VelENU[2]) + 2
			}
			c.AltHAE, c.AltSigma = la[1], math.Hypot(25, (g.txS-la[0])*vz)
		}
		confirmed := tr.Get(g.icao, &est) && est.Status == track.StatusConfirmed
		var r track.Result
		t1 := time.Now()
		if confirmed {
			r = tr.Cluster(&c, &est)
		} else {
			if len(c.TOAs) < 3 || (len(c.TOAs) == 3 && c.AltSigma <= 0) {
				return
			}
			sst = sst[:0]
			for _, m := range c.TOAs {
				sst = append(sst, solve.Station{Pos: m.Pos, TOA: m.T, Sigma: math.Hypot(m.Sigma, m.SyncSigma), ID: int32(m.Station)})
			}
			pr := solve.Problem{EpochNs: t0, Stations: sst}
			if c.AltSigma > 0 {
				pr.Alt = solve.Altitude{Valid: true, HAE: c.AltHAE, Sigma: c.AltSigma}
			}
			if est.Status == track.StatusTentative {
				pr.Prior = solve.Prior{Valid: true, Pos: est.ECEF, Cov: est.Cov}
			}
			var f solve.Fix
			st.Solves++
			if err := sv.Solve(&pr, &f); err != nil || !f.OK() {
				return
			}
			st.SolveOK++
			tf := track.Fix{ICAO: g.icao, TNs: t0 + int64(math.Round(f.TxS*1e9)), NHyp: 1, NStations: f.NStations, DOF: f.DOF,
				Hyp: [2]track.FixHyp{{ECEF: f.ECEF, Cov: f.CovECEF}}}
			if f.Flags&solve.FlagAmbiguous != 0 && f.NAlt > 0 {
				tf.Hyp[1] = track.FixHyp{ECEF: f.Alt[0].ECEF, Cov: f.CovECEF}
				tf.NHyp = 2
			}
			r = tr.Fix(&tf, &est)
		}
		st.UpdateNs += float64(time.Since(t1).Nanoseconds())
		st.nUpd++
		st.Results[r.String()]++
		if r != track.ResApplied || est.Status != track.StatusConfirmed || est.Updates <= 10 {
			return
		}
		lf := geo.NewFrame(est.LLH)
		d := lf.Rotate(g.pos.Sub(est.ECEF))
		pl := lf.CovToENU(est.Cov)
		e := math.Hypot(d.X, d.Y)
		det := pl[0]*pl[4] - pl[1]*pl[3]
		nees := (pl[4]*d.X*d.X - 2*pl[1]*d.X*d.Y + pl[0]*d.Y*d.Y) / det
		in := cf.ToENU(g.pos)
		if math.Hypot(in.X, in.Y) <= *radius*1e3 {
			st.ErrInside = append(st.ErrInside, e)
			if nees <= 6.18 {
				st.CovIn++
			}
		} else {
			st.ErrOut = append(st.ErrOut, e)
			if nees <= 6.18 {
				st.CovOut++
			}
		}
	}
	n.AddSink(sinkFunc{
		obs: func(obs []simnet.Observation) {
			for i := range obs {
				o := &obs[i]
				g := open[o.Seq]
				if g == nil {
					tx := o.Tx
					alt := false
					if len(o.Data) > 0 {
						switch df := o.Data[0] >> 3; df {
						case 0, 4, 16, 20:
							alt = true
						case 17, 18:
							alt = len(o.Data) > 4 && o.Data[4]>>3 >= 9 && o.Data[4]>>3 <= 18
						}
					}
					g = &group{txS: tx.TxS, icao: o.ICAO, pos: geo.Vec3{X: tx.Pos[0], Y: tx.Pos[1], Z: tx.Pos[2]}, hae: tx.HAE, alt: alt}
					open[o.Seq] = g
				}
				tick := 0.0
				if hz := o.Class.NominalHz(); hz > 0 {
					tick = n.TickError(o) / hz
				}
				rx := o.Rx
				a := math.Exp(-math.Max(o.ArrivalS-syncT[rx], 0) / 20)
				syncV[rx] = a*syncV[rx] + math.Sqrt(1-a*a)*syncS[rx]*rng.NormFloat64()
				syncT[rx] = o.ArrivalS
				t := o.ArrivalS + (o.MultipathNs+o.OutlierNs)*1e-9 + tick + syncV[rx]
				g.toas = append(g.toas, track.TOA{Station: uint32(rx), Pos: pos[rx], T: t, Sigma: white[rx], SyncSigma: syncS[rx]})
			}
		},
		adv: func(h float64) {
			var done []*group
			for k, g := range open {
				if g.txS < h-0.05 {
					done = append(done, g)
					delete(open, k)
				}
			}
			sort.Slice(done, func(i, j int) bool { return done[i].txS < done[j].txS })
			st.mu.Lock()
			for _, g := range done {
				handle(g)
			}
			st.SimS = h
			st.mu.Unlock()
		},
	})
	ctx := context.Background()
	steps := int(*minutes * 60)
	for k := range steps {
		if err := n.Advance(ctx, time.Second); err != nil {
			log.Fatal(err)
		}
		if k%10 == 9 {
			st.mu.Lock()
			tr.Sweep(n.UnixNs(n.NowS()), nil)
			st.Tracker = tr.Stats()
			st.mu.Unlock()
		}
	}
	st.mu.Lock()
	st.Tracker = tr.Stats()
	st.Summary = st.summary()
	if st.nUpd > 0 {
		st.UpdateNs /= st.nUpd
		st.nUpd = 1
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	st.mu.Unlock()
	fmt.Fprintln(os.Stdout, string(b))
	if *hold > 0 {
		time.Sleep(*hold)
	}
}

type sinkFunc struct {
	obs func([]simnet.Observation)
	adv func(float64)
}

func (s sinkFunc) Observe(o []simnet.Observation) { s.obs(o) }
func (s sinkFunc) Advance(h float64)              { s.adv(h) }
