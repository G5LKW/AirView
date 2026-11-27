// Command simnet-serve runs a simnet receiver network in real time and
// serves each receiver's Beast stream over TCP, for live tests of Beast
// ingest (DESIGN §13.1). The n-th accepted connection gets receiver n mod N.
//
//	simnet-serve -listen 127.0.0.1:18501 -receivers 20 -aircraft 300 -format reduce_plus
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"time"

	"airview/internal/sim/simnet"
)

// connSink queues bytes for one connection; a slow client loses data
// instead of stalling the simulation.
type connSink struct{ ch chan []byte }

func (c *connSink) Write(_ int64, p []byte) {
	select {
	case c.ch <- append([]byte(nil), p...):
	default:
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18501", "TCP listen address")
	nrx := flag.Int("receivers", 20, "number of receivers")
	nac := flag.Int("aircraft", 300, "number of aircraft")
	seed := flag.Uint64("seed", 1, "random seed")
	lat := flag.Float64("lat", 50, "network centre latitude")
	lon := flag.Float64("lon", 9, "network centre longitude")
	radius := flag.Float64("radius", 300, "receiver spread radius, km")
	format := flag.String("format", "beast", "beast | reduce_plus")
	flag.Parse()

	specs := simnet.RandomReceivers(*seed, *nrx, simnet.Placement{CenterLat: *lat, CenterLon: *lon, RadiusKm: *radius})
	start := time.Now().UTC().Truncate(time.Second)
	n, err := simnet.New(simnet.Config{Seed: *seed, Start: start, Aircraft: *nac, Receivers: specs})
	if err != nil {
		log.Fatal(err)
	}
	opt := simnet.BeastOptions{}
	if *format == "reduce_plus" {
		opt.Format = simnet.BeastReducePlus
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() { <-ctx.Done(); ln.Close() }()

	attach := make(chan net.Conn)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			attach <- c
		}
	}()
	log.Printf("simnet-serve: %d receivers, %d aircraft, listening on %s", *nrx, *nac, ln.Addr())
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	next := 0
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-attach:
			rx := next % *nrx
			next++
			cs := &connSink{ch: make(chan []byte, 1024)}
			n.NewBeastEmitter(rx, cs, opt) // attached between steps: no race
			log.Printf("connection %s -> receiver %s", c.RemoteAddr(), n.Receiver(rx).ID)
			go func() {
				defer c.Close()
				for p := range cs.ch {
					c.SetWriteDeadline(time.Now().Add(5 * time.Second))
					if _, err := c.Write(p); err != nil {
						log.Printf("connection %s closed: %v", c.RemoteAddr(), err)
						// keep draining so the emitter never blocks
						for range cs.ch {
						}
						return
					}
				}
			}()
		case <-tick.C:
			if err := n.Step(ctx); err != nil {
				return
			}
		}
	}
}
