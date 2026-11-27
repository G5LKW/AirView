// Command sdrbridge turns an rtl_tcp stream into Beast binary on a TCP port,
// using AirView's own demodulator (internal/sdr). It is a diagnostic and
// bring-up tool for the Windows RTL-SDR path until the feeder wiring
// exists, and a way to compare the demodulator with other decoders on the
// same dongle.
//
//	sdrbridge -rtltcp 127.0.0.1:1234 -beast 127.0.0.1:30005
//	sdrbridge -spawn "C:\rtl-sdr\rtl_tcp.exe" -beast 127.0.0.1:30005
//	sdrbridge -fake -beast 127.0.0.1:18737 -for 30s   (simulated sky, no hardware)
//
// Every 10 s it prints the demodulator counters.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"sort"
	"sync"
	"time"

	"airview/internal/model"
	"airview/internal/sdr"
	"airview/internal/sim"
)

func main() {
	var (
		rtlAddr = flag.String("rtltcp", "127.0.0.1:1234", "rtl_tcp server address")
		spawn   = flag.String("spawn", "", "start this rtl_tcp executable (listening on -rtltcp) and restart it when it exits")
		device  = flag.Int("device", 0, "dongle index for -spawn")
		beast   = flag.String("beast", "127.0.0.1:30005", "Beast output listen address")
		gain    = flag.String("gain", "max", "tuner gain: max, auto or dB")
		ppm     = flag.Int("ppm", 0, "frequency correction (ppm)")
		rate    = flag.Uint("rate", 2400000, "sample rate (2000000-3200000)")
		bias    = flag.Bool("biastee", false, "switch on the bias tee")
		fake    = flag.Bool("fake", false, "serve a simulated sky from a built-in fake rtl_tcp (no hardware)")
		dur     = flag.Duration("for", 0, "stop after this long (0 = until interrupted)")
	)
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}
	logf := func(f string, a ...any) { log.Printf(f, a...) }

	if *fake {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
		*rtlAddr = ln.Addr().String()
		f := &sdr.FakeRTLTCP{Tuner: sdr.TunerR820T, GainCount: 29, Rate: float64(*rate), Source: simSource(float64(*rate))}
		go f.Serve(ctx, ln)
		logf("fake rtl_tcp with simulated traffic on %s", *rtlAddr)
	} else if *spawn != "" {
		go func() {
			err := sdr.RunChild(ctx, sdr.ChildOptions{Path: *spawn, Addr: *rtlAddr, Device: *device, SampleRate: uint32(*rate), Logf: logf}, nil)
			if err != nil && ctx.Err() == nil {
				log.Fatal(err)
			}
		}()
	}

	out := sdr.NewBeastServer()
	if err := out.Listen(ctx, *beast); err != nil {
		log.Fatal(err)
	}
	logf("Beast output on %s", out.Addr())
	opts := sdr.Options{Addr: *rtlAddr, Gain: *gain, PPM: *ppm, SampleRate: uint32(*rate), BiasTee: *bias, FeedID: "rtl", Logf: logf,
		OnClockEvent: func(e uint32, why model.ClockEventReason) { logf("clock epoch %d (%s)", e, why) }}
	if err := opts.Validate(); err != nil {
		log.Fatal(err)
	}
	rx := sdr.NewReceiver(opts)
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		var last sdr.Stats
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s := rx.Stats()
			d := s.Demod
			logf("connected=%v tuner=%s frames=%d (+%d, %.0f/s) repaired=%d noise=%.1f dBFS clipped=%d drops(server/local)=%d/%d beast clients=%d",
				s.Connected, s.Tuner, d.Frames, d.Frames-last.Demod.Frames, float64(d.Frames-last.Demod.Frames)/10,
				d.Corrected, d.NoiseDBFS, d.Clipped, s.SampleDrops, s.LocalDrops, out.Clients())
			last = s
		}
	}()
	err := rx.Run(ctx, out)
	s := rx.Stats()
	fmt.Printf("frames %d, repaired %d, candidates %d, samples %d, by DF %v\n", s.Demod.Frames, s.Demod.Corrected, s.Demod.Candidates, s.Demod.Samples, s.Demod.ByDF)
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

// simSource renders the AirView simulator's sky as an endless 8-bit I/Q
// stream: frames at their simulated times and levels, 2.5 LSB noise.
func simSource(fs float64) func(n int) []byte {
	s := sim.New(sim.Config{Seed: 7, Start: time.Now().UTC(), Aircraft: 200, NoDiurnal: true})
	var (
		mu      sync.Mutex
		pending []sdr.SynthFrame // absolute sample positions
		simT    float64          // seconds simulated so far
		pos     int              // samples produced so far
		t0      int64            = -1
		rng                      = rand.New(rand.NewPCG(1, 2))
		seed    uint64
	)
	sink := frameSink(func(f model.RawFrame) {
		if f.Channel > 0 || (f.Kind != model.FrameModeSShort && f.Kind != model.FrameModeSLong) || f.GPSNs < 0 {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if t0 < 0 {
			t0 = f.GPSNs
		}
		dur := float64(8+len(f.Data)*8) * 1e-6
		start := (float64(f.GPSNs-t0)*1e-9 - dur) * fs
		amp := 128 * math.Pow(10, f.Signal/20)
		pending = append(pending, sdr.SynthFrame{Data: append([]byte(nil), f.Data...), Start: start,
			SNRDB: 20 * math.Log10(amp/(2.5*math.Sqrt2)), FreqHz: (rng.Float64() - 0.5) * 2e5, Phase: math.NaN()})
	})
	return func(n int) []byte {
		end := float64(pos+n) / fs
		for simT < end+0.1 {
			_ = s.Advance(context.Background(), 100*time.Millisecond, sink)
			simT += 0.1
		}
		mu.Lock()
		sort.Slice(pending, func(i, j int) bool { return pending[i].Start < pending[j].Start })
		var in []sdr.SynthFrame
		keep := pending[:0]
		for _, f := range pending {
			if f.Start < float64(pos+n) && f.Start+300 > float64(pos) {
				g := f
				g.Start -= float64(pos)
				in = append(in, g)
			}
			if f.Start+300 > float64(pos+n) {
				keep = append(keep, f)
			}
		}
		pending = keep
		mu.Unlock()
		seed++
		out := sdr.Synthesize(sdr.SynthConfig{SampleRate: fs, NoiseLSB: 2.5, Seed: seed}, n, in)
		pos += n
		return out
	}
}

type frameSink func(model.RawFrame)

func (f frameSink) HandleFrame(r model.RawFrame)     { f(r) }
func (f frameSink) HandleUpdate(model.DecodedUpdate) {}
