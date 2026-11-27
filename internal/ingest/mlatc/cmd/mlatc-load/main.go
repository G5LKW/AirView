// Command mlatc-load runs the mlat-client compat server
// (internal/ingest/mlatc) on its own, or a load generator of simulated
// mlat-clients against it, so CPU and memory can be measured in separate
// processes (DESIGN §20.2: 1,000+ feeders).
//
//	mlatc-load -serve 127.0.0.1:18606 -duration 80s -report 10s [-results 1]
//	mlatc-load -load 127.0.0.1:18606 -clients 1000 -rate 300 -duration 60s
//
// With -udp on both sides the server accepts the UDP transport on the same
// port and the load clients send their timed frames as datagrams (about
// 1,400 bytes each), keeping seen lists and heartbeats on TCP.
//
// The server uses an in-memory registry (every UUID is a station) and a sink
// that only counts frames; with -results N it sends N synthetic results per
// second to every attached station. It prints a JSON report line every
// -report interval and a final one. It never touches a data directory.
//
// A load client speaks the reference client's protocol: a space-padded
// JSON handshake offering zlib2, zlib-compressed batches every 200 ms
// (sync flush, tail stripped, 2-byte length), mlat and sync messages with
// 12 MHz timestamps, a seen list, rate reports and heartbeats; it inflates
// the server direction and counts what it receives.
package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/ingest"
	"airview/internal/ingest/mlatc"
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

type countSink struct {
	frames, batches atomic.Int64
	mu              sync.Mutex
	stations        map[model.StationIdx]bool
}

func (c *countSink) Submit(f []model.RawFrame) {
	c.frames.Add(int64(len(f)))
	c.batches.Add(1)
}
func (c *countSink) ClockEvent(model.StationIdx, ingest.ClockEvent) {}
func (c *countSink) SetTimestamps(st model.StationIdx, _ bool) {
	c.mu.Lock()
	c.stations[st] = true
	c.mu.Unlock()
}
func (c *countSink) OpenSession(ingest.SessionInfo) mlatc.SessionHandle { return nil }

func main() {
	serve := flag.String("serve", "", "run the server on this address")
	load := flag.String("load", "", "run the load generator against this address")
	clients := flag.Int("clients", 1000, "load: concurrent clients")
	rate := flag.Int("rate", 300, "load: frames per second per client")
	compress := flag.String("compress", "zlib2", "load: offered compression (zlib2, zlib, none)")
	results := flag.Int("results", 0, "server: synthetic results per second per station")
	udp := flag.Bool("udp", false, "server: accept the UDP transport; load: offer it and send timed frames as datagrams")
	duration := flag.Duration("duration", 60*time.Second, "run time")
	report := flag.Duration("report", 10*time.Second, "server: report interval")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	switch {
	case *serve != "":
		runServer(ctx, *serve, *report, *results, *udp)
	case *load != "":
		runLoad(ctx, *load, *clients, *rate, *compress, *udp)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func cpuSeconds() float64 { return processCPU() }

func runServer(ctx context.Context, addr string, every time.Duration, results int, udp bool) {
	sink := &countSink{stations: map[model.StationIdx]bool{}}
	pol := mlatc.DefaultPolicy()
	pol.MaxClients = 5000
	pol.MaxConnsPerIP, pol.MaxConnsPerSubnet, pol.MaxUnauthPerIP, pol.PerIPStations = 5000, 5000, 5000, 5000
	srv, err := mlatc.New(mlatc.Options{Addr: addr, Policy: pol, UDP: udp, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))},
		mlatc.NewMemRegistry(), sink)
	if err != nil {
		log.Fatal(err)
	}
	if results > 0 {
		go func() {
			p := geo.ToECEF(51, 9, 10000)
			t := time.NewTicker(time.Second / time.Duration(results))
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				sink.mu.Lock()
				sts := make([]model.StationIdx, 0, len(sink.stations))
				for st := range sink.stations {
					sts = append(sts, st)
				}
				sink.mu.Unlock()
				fix := model.MLATFix{ICAO: 0x4840d6, TUnixNs: time.Now().UnixNano(), ECEF: [3]float64{p.X, p.Y, p.Z},
					Cov: [6]float32{900, 0, 0, 900, 0, 2500}, NBucket: model.NBucket5to8}
				for _, st := range sts {
					srv.SendResult(st, mlatc.Result{MLATFix: fix})
				}
			}
		}()
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		lastF, lastCPU, lastT := int64(0), cpuSeconds(), time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			now, cpu, f := time.Now(), cpuSeconds(), sink.frames.Load()
			el := now.Sub(lastT).Seconds()
			printReport(srv, f, float64(f-lastF)/el, (cpu-lastCPU)/el)
			lastF, lastCPU, lastT = f, cpu, now
		}
	}()
	t0, cpu0 := time.Now(), cpuSeconds()
	if err := srv.ListenAndServe(ctx); err != nil {
		log.Fatal(err)
	}
	el := time.Since(t0).Seconds()
	printReport(srv, sink.frames.Load(), float64(sink.frames.Load())/el, (cpuSeconds()-cpu0)/el)
}

func printReport(srv *mlatc.Server, frames int64, fps, cores float64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := srv.Stats()
	b, _ := json.Marshal(map[string]any{
		"time": time.Now().Format(time.RFC3339), "clients": st.Clients, "attached": st.Attached,
		"frames": frames, "framesPerSec": int(fps), "cpuCores": fmt.Sprintf("%.3f", cores),
		"heapMB": ms.HeapAlloc >> 20, "sysMB": ms.Sys >> 20, "goroutines": runtime.NumGoroutine(),
		"lines": st.Lines, "badLines": st.BadLines, "dropRate": st.DroppedRate, "dropDup": st.DroppedDup,
		"results": st.Results, "resultsDropped": st.ResultsDropped, "wireMB": st.WireBytes >> 20, "reasons": st.Reasons,
		"udpPackets": st.UDPPackets, "udpLost": st.UDPLost, "udpBad": st.UDPBad, "udpUnknownKey": st.UDPUnknownKey,
	})
	fmt.Println(string(b))
}

// ------------------------------------------------------------ load

type loadStats struct {
	connected, failed, sentFrames, results, starts, heartbeats atomic.Int64
	udpClients, datagrams                                      atomic.Int64
}

func runLoad(ctx context.Context, addr string, n, rate int, compress string, udp bool) {
	var st loadStats
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// spread the connects over two seconds
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(i) * 2 * time.Second / time.Duration(max(n, 1))):
			}
			if err := client(ctx, addr, i, rate, compress, udp, &st); err != nil && ctx.Err() == nil {
				st.failed.Add(1)
				log.Printf("client %d: %v", i, err)
			}
		}(i)
	}
	t0 := time.Now()
	wg.Wait()
	el := time.Since(t0).Seconds()
	b, _ := json.Marshal(map[string]any{"clients": n, "connected": st.connected.Load(), "failed": st.failed.Load(),
		"sentFrames": st.sentFrames.Load(), "sentPerSec": int(float64(st.sentFrames.Load()) / el),
		"resultsReceived": st.results.Load(), "startSending": st.starts.Load(), "heartbeats": st.heartbeats.Load(),
		"udpClients": st.udpClients.Load(), "datagrams": st.datagrams.Load()})
	fmt.Println(string(b))
}

func client(ctx context.Context, addr string, id, rate int, compress string, udp bool, st *loadStats) error {
	nc, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer nc.Close()
	context.AfterFunc(ctx, func() { nc.Close() })
	offer := []string{"none"}
	switch compress {
	case "zlib2":
		offer = []string{"none", "zlib", "zlib2"}
	case "zlib":
		offer = []string{"none", "zlib"}
	}
	hello := map[string]any{"version": 3, "client_version": "mlatc-load", "compress": offer,
		"selective_traffic": true, "heartbeat": true, "return_results": true, "return_stats": true,
		"udp_transport": map[bool]any{false: false, true: 2}[udp], "return_result_format": "ecef", "uuid": fmt.Sprintf("%08x-0000-4000-8000-%012x", id, id),
		"user": fmt.Sprintf("load%d", id), "lat": 50 + float64(id%100)/50, "lon": 8 + float64(id/100)/50, "alt": 100,
		"clock_type": "dump1090", "privacy": true}
	hb, _ := json.Marshal(hello)
	nc.Write(append(append(hb, strings.Repeat(" ", 128)...), '\n'))
	br := bufio.NewReader(nc)
	nc.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	nc.SetReadDeadline(time.Time{})
	var reply struct {
		Compress string          `json:"compress"`
		Deny     []string        `json:"deny"`
		UDP      json.RawMessage `json:"udp_transport"`
	}
	json.Unmarshal(line, &reply)
	if reply.Deny != nil {
		return fmt.Errorf("denied: %v", reply.Deny)
	}
	st.connected.Add(1)
	go readServer(br, reply.Compress, st)
	dg, err := dialUDP(reply.UDP)
	if err != nil {
		return err
	}
	if dg != nil {
		defer dg.pc.Close()
		st.udpClients.Add(1)
	}

	var zbuf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&zbuf, 1)
	r := rand.New(rand.NewPCG(uint64(id), 7))
	aircraft := make([]uint32, 60)
	for i := range aircraft {
		aircraft[i] = r.Uint32() & 0xffffff
	}
	var lines bytes.Buffer
	lines.WriteString(`{"input_connected":"connected"}` + "\n")
	lines.WriteString(`{"seen":[`)
	for i, a := range aircraft {
		if i > 0 {
			lines.WriteByte(',')
		}
		fmt.Fprintf(&lines, `"%06x"`, a)
	}
	lines.WriteString("]}\n")
	ticks := r.Uint64() >> 20
	const batch = 200 * time.Millisecond
	tick := time.NewTicker(batch)
	defer tick.Stop()
	perBatch := max(rate*int(batch/time.Millisecond)/1000, 1)
	lastHB := time.Now()
	short := make([]byte, 7)
	long := make([]byte, 14)
	for {
		for k := 0; k < perBatch; {
			ticks += uint64(12e6*batch.Seconds()) / uint64(perBatch)
			a := aircraft[r.IntN(len(aircraft))]
			if r.IntN(5) == 0 && k+2 <= perBatch {
				long[0] = 0x8d
				long[1], long[2], long[3] = byte(a>>16), byte(a>>8), byte(a)
				for j := 4; j < 14; j++ {
					long[j] = byte(r.Uint32())
				}
				if dg != nil {
					dg.sync(ticks-6000000, ticks, long, st)
				} else {
					fmt.Fprintf(&lines, `{"sync":{"et":%d,"em":"%x","ot":%d,"om":"%x"}}`+"\n", ticks-6000000, long, ticks, long)
				}
				k += 2
				continue
			}
			short[0] = 0x5d
			short[1], short[2], short[3] = byte(a>>16), byte(a>>8), byte(a)
			for j := 4; j < 7; j++ {
				short[j] = byte(r.Uint32())
			}
			if dg != nil {
				dg.mlat(ticks, short, st)
			} else {
				fmt.Fprintf(&lines, `{"mlat":{"t":%d,"m":"%x"}}`+"\n", ticks, short)
			}
			k++
		}
		st.sentFrames.Add(int64(perBatch))
		if dg != nil {
			dg.flush(st)
		}
		if time.Since(lastHB) > 30*time.Second {
			lastHB = time.Now()
			fmt.Fprintf(&lines, `{"heartbeat":{"client_time":%.3f}}`+"\n", float64(time.Now().UnixNano())/1e9)
		}
		var payload []byte
		if reply.Compress == "none" {
			payload = lines.Bytes()
		} else {
			zw.Write(lines.Bytes())
			zw.Flush()
			b := zbuf.Bytes()
			b = b[:len(b)-4]
			for len(b) > 0 {
				m := min(len(b), 32000)
				payload = binary.BigEndian.AppendUint16(payload, uint16(m))
				payload = append(payload, b[:m]...)
				b = b[m:]
			}
			zbuf.Reset()
		}
		lines.Reset()
		nc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := nc.Write(payload); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func readServer(br *bufio.Reader, compress string, st *loadStats) {
	var r io.Reader = br
	if compress == "zlib2" {
		pr, pw := io.Pipe()
		go func() {
			for {
				var hdr [2]byte
				if _, err := io.ReadFull(br, hdr[:]); err != nil {
					pw.CloseWithError(err)
					return
				}
				n := int(binary.BigEndian.Uint16(hdr[:]))
				buf := make([]byte, n+4)
				if _, err := io.ReadFull(br, buf[:n]); err != nil {
					pw.CloseWithError(err)
					return
				}
				copy(buf[n:], []byte{0, 0, 0xff, 0xff})
				if _, err := pw.Write(buf); err != nil {
					return
				}
			}
		}()
		zr, err := zlib.NewReader(pr)
		if err != nil {
			return
		}
		r = zr
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		l := sc.Bytes()
		switch {
		case bytes.HasPrefix(l, []byte(`{"result"`)):
			st.results.Add(1)
		case bytes.HasPrefix(l, []byte(`{"start_sending"`)):
			st.starts.Add(1)
		case bytes.HasPrefix(l, []byte(`{"heartbeat"`)):
			st.heartbeats.Add(1)
		}
	}
}
