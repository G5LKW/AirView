// Command beastin-load runs the Beast ingest (internal/ingest/beastin) as a
// standalone server, or a load generator of simulated readsb
// beast_reduce_plus_out feeders against it, so CPU and memory of the
// ingest can be measured in its own process (DESIGN §20.2: 1,000+ feeders).
//
//	beastin-load -serve 127.0.0.1:18513 -duration 70s -report 10s
//	beastin-load -load 127.0.0.1:18513 -links 1000 -duration 60s
//
// The server uses an in-memory registry (every UUID is a station) and a
// sink that only counts frames. It prints a JSON report line every -report
// interval and a final one on exit. It never touches a data directory.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/feed"
	"airview/internal/ingest/beastin"
	"airview/internal/ingest/beastin/internal/cpustat"
	"airview/internal/sim/simnet"
)

type countSink struct{ frames, batches atomic.Int64 }

func (c *countSink) HandleBeast(b *beastin.Batch) {
	c.frames.Add(int64(len(b.Frames)))
	c.batches.Add(1)
}

func main() {
	serve := flag.String("serve", "", "run the ingest server on this address")
	load := flag.String("load", "", "run the load generator against this address")
	links := flag.Int("links", 1000, "load: number of concurrent links")
	receivers := flag.Int("receivers", 50, "load: distinct simulated receivers (links share their streams)")
	aircraft := flag.Int("aircraft", 400, "load: simulated aircraft")
	speed := flag.Float64("speed", 1, "load: replay speed (1 = real time)")
	duration := flag.Duration("duration", 60*time.Second, "run time")
	report := flag.Duration("report", 10*time.Second, "server: report interval")
	verbose := flag.Bool("v", false, "server: log link events")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	switch {
	case *serve != "":
		runServer(ctx, *serve, *report, *verbose)
	case *load != "":
		runLoad(ctx, *load, *links, *receivers, *aircraft, *speed)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

type serverReport struct {
	T          string         `json:"t"`
	Links      int            `json:"links"`
	Identified int            `json:"identified"`
	CPUPercent float64        `json:"cpuPercentOfOneCore"`
	HeapMB     float64        `json:"heapMB"`
	SysMB      float64        `json:"sysMB"`
	Goroutines int            `json:"goroutines"`
	FPS        float64        `json:"framesPerSec"`
	MBps       float64        `json:"MBps"`
	Delivered  int64          `json:"delivered"`
	Stats      beastin.Stats  `json:"stats"`
	Final      bool           `json:"final,omitempty"`
	Extra      map[string]any `json:"extra,omitempty"`
}

func runServer(ctx context.Context, addr string, every time.Duration, verbose bool) {
	sink := &countSink{}
	lvl := slog.LevelWarn
	if verbose {
		lvl = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	opt := beastin.Options{
		Addr:   addr,
		Logger: logger,
		Policy: beastin.Policy{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}},
	}
	s, err := beastin.New(opt, beastin.NewMemRegistry(), sink)
	if err != nil {
		log.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()

	enc := json.NewEncoder(os.Stdout)
	prev, prevStats := cpustat.Take(), s.Stats()
	emit := func(final bool) {
		now := cpustat.Take()
		st := s.Stats()
		dt := now.Wall.Sub(prev.Wall).Seconds()
		r := serverReport{
			T: now.Wall.UTC().Format(time.RFC3339), Links: st.Links, Identified: st.Identified,
			CPUPercent: cpustat.CPUPercent(prev, now), HeapMB: now.HeapMB, SysMB: now.SysMB, Goroutines: now.Goroutines,
			Delivered: sink.frames.Load(), Stats: st, Final: final,
		}
		if dt > 0 {
			r.FPS = float64(st.Frames-prevStats.Frames) / dt
			r.MBps = float64(st.Bytes-prevStats.Bytes) / dt / 1e6
		}
		enc.Encode(r)
		prev, prevStats = now, st
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			emit(false)
		case err := <-done:
			emit(true)
			if err != nil {
				log.Fatal(err)
			}
			return
		}
	}
}

func runLoad(ctx context.Context, addr string, links, receivers, aircraft int, speed float64) {
	log.Printf("generating %d simulated receiver streams (30 s, %d aircraft)", receivers, aircraft)
	specs := simnet.RandomReceivers(7, receivers, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: 250})
	n, err := simnet.New(simnet.Config{Seed: 7, Aircraft: aircraft, Receivers: specs})
	if err != nil {
		log.Fatal(err)
	}
	bufs := make([]*bytes.Buffer, receivers)
	for i := range bufs {
		bufs[i] = &bytes.Buffer{}
		n.NewBeastEmitter(i, &simnet.WriterSink{W: bufs[i]}, simnet.BeastOptions{Format: simnet.BeastReducePlus, NoStatus: true})
	}
	if err := n.Advance(ctx, 30*time.Second); err != nil {
		log.Fatal(err)
	}
	streams := make([][]byte, receivers)
	var total int
	for i, b := range bufs {
		raw := b.Bytes()
		if len(raw) > 41 && raw[0] == 0x1A && raw[1] == 0xE4 {
			raw = raw[41:] // each link sends its own UUID
		}
		streams[i] = raw
		total += len(raw)
	}
	log.Printf("average stream %.1f kB/s per link; starting %d links", float64(total)/float64(receivers)/30/1e3*speed, links)

	var wg sync.WaitGroup
	var sent, failed, connected atomic.Int64
	dialSem := make(chan struct{}, 32)
	for i := 0; i < links; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var c net.Conn
			var err error
			dialSem <- struct{}{} // a burst of dials overflows the listen backlog
			for try := 0; try < 30; try++ {
				if c, err = net.DialTimeout("tcp", addr, 5*time.Second); err == nil {
					break
				}
				time.Sleep(time.Duration(20*(try+1)) * time.Millisecond)
			}
			<-dialSem
			if err != nil {
				failed.Add(1)
				return
			}
			defer c.Close()
			connected.Add(1)
			uuid := strconv.FormatUint(0x20000000_00000000+uint64(i), 16) + "0123456789abcdef"
			hdr := feed.AppendBeastUUID(nil, uuid)
			hdr = feed.AppendBeastOption(hdr, feed.BeastOptPing)
			if _, err := c.Write(hdr); err != nil {
				return
			}
			// Echo the server's pings like a feeder would.
			go func() {
				p := feed.NewBeastParser(func(*feed.BeastFrame) {})
				p.OnEscape = func(typ byte, body []byte) {
					if typ == feed.BeastPing {
						c.Write(feed.AppendBeastEscape(nil, feed.BeastPing, body))
					}
				}
				b := make([]byte, 512)
				for {
					k, err := c.Read(b)
					p.Write(b[:k])
					if err != nil {
						return
					}
				}
			}()
			src := streams[i%len(streams)]
			per := max(1, int(float64(len(src))*speed/300))
			off := (i * 7919) % len(src)
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				end := off + per
				var chunk []byte
				if end <= len(src) {
					chunk = src[off:end]
					off = end
				} else {
					chunk = append(append([]byte(nil), src[off:]...), src[:end-len(src)]...)
					off = end - len(src)
				}
				if _, err := c.Write(chunk); err != nil {
					failed.Add(1)
					return
				}
				sent.Add(int64(len(chunk)))
			}
		}(i)
	}
	start := time.Now()
	wg.Wait()
	el := time.Since(start).Seconds()
	fmt.Printf(`{"links":%d,"connected":%d,"failed":%d,"sentMB":%.1f,"MBps":%.2f}`+"\n",
		links, connected.Load(), failed.Load(), float64(sent.Load())/1e6, float64(sent.Load())/el/1e6)
}
