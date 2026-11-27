// Command airview-netsim drives an AirView aggregator with a simulated
// receiver network in real time (DESIGN §13.1, WP-E acceptance): one
// simnet world, heard by
//
//   - the host station: a plain Beast stream (Radarcape GPS timestamps)
//     served on -host-listen, which the aggregator's own feed connects to;
//   - -beast compat feeders: readsb beast_reduce_plus links (with UUIDs)
//     to the aggregator's Beast port;
//   - -avf native feeders: each its own feeder identity (internal/uplink,
//     in memory) feeding the aggregator's AVF/1 port;
//   - -mlatc mlat-clients: protocol v3 on the aggregator's mlat-client port
//     with the reference client's timestamp convention (simfeed);
//   - with -mix gps the timed feeders are GPS Radarcapes (fast, anchored
//     MLAT); the default keeps simnet's community device mix.
//
// It prints a status line every -report and reconnects lost links.
//
//	airview-netsim -beast 127.0.0.1:30004 -avf 127.0.0.1:30160 -pin sha256/... \
//	    -host-listen 127.0.0.1:30005 -avf-feeders 20 -beast-feeders 10 -duration 24h
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"airview/internal/engine/simfeed"
	"airview/internal/sim/simnet"
)

func main() {
	beastAddr := flag.String("beast", "", "aggregator Beast compat address (host:port; empty = no Beast feeders)")
	avfAddr := flag.String("avf", "", "aggregator AVF/1 address (host:port; empty = no AVF feeders)")
	mlatcAddr := flag.String("mlatc", "", "aggregator mlat-client address (host:port; empty = no mlat-clients)")
	pin := flag.String("pin", "", "AVF server SPKI pin (sha256/...; empty = system roots)")
	token := flag.String("token", "", "AVF enrol token (invite admission)")
	hostListen := flag.String("host-listen", "", "serve the host station's Beast stream here (empty = no host stream)")
	nAVF := flag.Int("avf-feeders", 20, "native AVF feeders")
	nBeast := flag.Int("beast-feeders", 10, "compat Beast feeders")
	nMLATC := flag.Int("mlatc-feeders", 0, "mlat-clients")
	nUnloc := flag.Int("unlocated", 0, "the last N AVF feeders declare no position (the aggregator must survey them)")
	mix := flag.String("mix", "default", `device mix of the timed feeders: "default" (simnet community mix) or "gps" (GPS Radarcapes)`)
	aircraft := flag.Int("aircraft", 300, "simulated aircraft")
	modeS := flag.Float64("modes-only", 0, "fraction of Mode S-only aircraft (0 = simnet default)")
	seed := flag.Uint64("seed", 1, "random seed")
	lat := flag.Float64("lat", 51.0, "network centre latitude")
	lon := flag.Float64("lon", 9.0, "network centre longitude")
	radius := flag.Float64("radius", 250, "receiver spread radius, km")
	dur := flag.Duration("duration", 0, "stop after this long (0 = until interrupted)")
	report := flag.Duration("report", time.Minute, "status line interval")
	flag.Parse()

	// Receiver order: 0 host, then Beast, AVF, mlat-client feeders.
	nrx := 1 + *nBeast + *nAVF + *nMLATC
	specs := simnet.RandomReceivers(*seed, nrx, simnet.Placement{CenterLat: *lat, CenterLon: *lon, RadiusKm: *radius})
	rr := rand.New(rand.NewPCG(*seed, 9))
	setDev := func(i int, d simnet.Device) {
		specs[i].Clock = simnet.DefaultClockSpec(d, rr)
		specs[i].Reception = simnet.DefaultReception(d, rr)
	}
	setDev(0, simnet.DeviceRadarcapeGPS)
	for i := 1 + *nBeast; i < nrx; i++ {
		switch {
		case *mix == "gps":
			setDev(i, simnet.DeviceRadarcapeGPS)
		case i >= 1+*nBeast+*nAVF && specs[i].Clock.Device == simnet.DeviceSBS:
			setDev(i, simnet.DeviceBeast) // SBS is not supported as an mlat-client
		}
	}
	n, err := simnet.New(simnet.Config{Seed: *seed, Start: time.Now().UTC(), Aircraft: *aircraft, Receivers: specs,
		ModeSOnlyFraction: *modeS})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}

	var wg sync.WaitGroup
	attach := make(chan func(), 16) // emitters are attached between steps only

	// Host station stream.
	var hostConns atomic.Int64
	if *hostListen != "" {
		ln, err := net.Listen("tcp", *hostListen)
		if err != nil {
			log.Fatal(err)
		}
		go func() { <-ctx.Done(); ln.Close() }()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				cs := newConnSink(ctx, c, &hostConns)
				attach <- func() { n.NewBeastEmitter(0, cs, simnet.BeastOptions{}) }
			}
		}()
	}

	var beasts []*simfeed.BeastFeeder
	if *beastAddr != "" {
		for i := 0; i < *nBeast; i++ {
			b := simfeed.NewBeastFeeder(n, 1+i, *beastAddr)
			beasts = append(beasts, b)
			wg.Add(1)
			go func() { defer wg.Done(); b.Run(ctx) }()
		}
	}
	var avfs []*simfeed.AVFFeeder
	if *avfAddr != "" {
		for i := 0; i < *nAVF; i++ {
			f, err := simfeed.NewAVFFeeder(n, 1+*nBeast+i, simfeed.AVFOptions{Addr: *avfAddr, Pin: *pin, Token: *token, Unlocated: i >= *nAVF-*nUnloc})
			if err != nil {
				log.Fatal(err)
			}
			avfs = append(avfs, f)
		}
	}
	var clients []*simfeed.MLATClient
	if *mlatcAddr != "" {
		for i := 0; i < *nMLATC; i++ {
			c, err := simfeed.NewMLATClient(n, 1+*nBeast+*nAVF+i, simfeed.MLATClientOptions{Addr: *mlatcAddr})
			if err != nil {
				log.Fatal(err)
			}
			clients = append(clients, c)
			wg.Add(1)
			go func() { defer wg.Done(); c.Run(ctx) }()
		}
	}
	log.Printf("airview-netsim: %d aircraft; host stream %q, %d Beast feeders -> %q, %d AVF feeders -> %q, %d mlat-clients -> %q",
		*aircraft, *hostListen, len(beasts), *beastAddr, len(avfs), *avfAddr, len(clients), *mlatcAddr)

	start := time.Now()
	nextReport := start.Add(*report)
	status := func() string {
		up := 0
		for _, b := range beasts {
			if b.Up() {
				up++
			}
		}
		avfUp, avfRes := 0, 0
		for _, f := range avfs {
			if f.Connected() {
				avfUp++
			}
			avfRes += f.Results.Count()
		}
		cUp, cRes := 0, 0
		var down []string
		for _, c := range clients {
			if c.Up() {
				cUp++
			} else {
				d := c.Denied()
				if d == "" {
					d = "not connected"
				}
				down = append(down, fmt.Sprintf("rx%d: %s", c.Rx, d))
			}
			cRes += c.Results.Count()
		}
		s := fmt.Sprintf("host conns %d, beast %d/%d, avf %d/%d (results %d), mlatc %d/%d (results %d)",
			hostConns.Load(), up, len(beasts), avfUp, len(avfs), avfRes, cUp, len(clients), cRes)
		if len(down) > 0 {
			s += " [mlatc down: " + strings.Join(down, "; ") + "]"
		}
		return s
	}
	err = simfeed.RealTime(ctx, n, 100*time.Millisecond, func() {
		select {
		case fn := <-attach:
			fn()
		default:
		}
		if now := time.Now(); now.After(nextReport) {
			nextReport = now.Add(*report)
			log.Printf("status: up %s, %s", now.Sub(start).Round(time.Second), status())
		}
	})
	if err != nil {
		log.Print(err)
	}
	for _, f := range avfs {
		f.Close()
	}
	wg.Wait()
	log.Printf("airview-netsim: stopped after %s; %s", time.Since(start).Round(time.Second), status())
}

// connSink writes an emitter's bytes to one accepted connection; a slow
// client loses data instead of stalling the simulation.
type connSink struct{ ch chan []byte }

func newConnSink(ctx context.Context, c net.Conn, live *atomic.Int64) *connSink {
	cs := &connSink{ch: make(chan []byte, 4096)}
	live.Add(1)
	go func() {
		defer live.Add(-1)
		defer c.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-cs.ch:
				_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, err := c.Write(p); err != nil {
					// Keep the emitter running: drop everything from now on.
					go func() {
						for range cs.ch {
						}
					}()
					return
				}
			}
		}
	}()
	return cs
}

func (c *connSink) Write(_ int64, p []byte) {
	select {
	case c.ch <- append([]byte(nil), p...):
	default:
	}
}
