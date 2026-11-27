package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
	"airview/internal/sim"
)

// stream is one Beast TCP stream an instance connects to as one feed.
type stream struct {
	ID      string // feed id suffix and Feed.AntennaID ("" = combined)
	Channel int    // RF channel of the simulator; -1 = combined receiver output
}

// splitStreams: one stream per RF channel of the simulator's default
// antennas (model.DefaultConfig: the vertical on channel 0, the yagi on
// channel 1), attributed to those antennas, so the per-antenna statistics
// exist the way demo mode produces them. Synthetic MLAT results go to the
// vertical stream only; status frames to both. Used for the fixture: the
// order in which an instance reads two TCP connections is not
// deterministic (which antenna's copy of a transmission it sees first
// decides the stored RSSI), which does not matter for a fixture recorded
// by one instance.
var splitStreams = []stream{{"vertical", 0}, {"yagi", 1}}

// combinedStreams: a single stream with the receiver's combined output
// (each transmission once, from the channel that heard it best - what a
// multi-channel receiver reports), so two instances see the same frames in
// the same order. Used for the live phase.
var combinedStreams = []stream{{"", -1}}

// fanout serves one deterministic simulation as Beast TCP streams to N
// instances. The simulation starts only when every instance has connected
// every stream, and each chunk of bytes is queued to all connections of a
// stream in the same step, so all instances receive byte-identical input
// (apart from microseconds of scheduling).
type fanout struct {
	seed      uint64
	aircraft  int
	clients   int // instances expected per stream
	streams   []stream
	lns       []net.Listener
	mu        sync.Mutex
	bufs      [][]byte // pending bytes per stream
	pend      *model.RawFrame
	peers     [][]*peer
	ready     chan struct{}
	readyOnce sync.Once
	frames    atomic.Int64
	bytes     atomic.Int64
	dropped   atomic.Int64 // connections lost after the start (input diverged)
	sim       *sim.Sim
	started   bool
}

type peer struct {
	conn net.Conn
	q    chan []byte
	dead atomic.Bool
}

// newFanout opens one listener per stream on 127.0.0.1.
func newFanout(seed uint64, aircraft, clients int, streams []stream) (*fanout, error) {
	f := &fanout{seed: seed, aircraft: aircraft, clients: clients, streams: streams, ready: make(chan struct{})}
	for range streams {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			f.Close()
			return nil, err
		}
		f.lns = append(f.lns, ln)
	}
	f.bufs = make([][]byte, len(streams))
	f.peers = make([][]*peer, len(streams))
	return f, nil
}

// Ports returns the TCP port of each stream.
func (f *fanout) Ports() []int {
	out := make([]int, len(f.lns))
	for i, ln := range f.lns {
		out[i] = ln.Addr().(*net.TCPAddr).Port
	}
	return out
}

// Run accepts the connections, waits until all are there (or ctx ends),
// then runs the simulation in real time until ctx ends. The connections
// stay open afterwards (no "feed disconnected" events while the instances
// are compared) until Close.
func (f *fanout) Run(ctx context.Context, receiver model.Receiver) error {
	for i, ln := range f.lns {
		go f.accept(i, ln)
	}
	select {
	case <-f.ready:
	case <-ctx.Done():
		return fmt.Errorf("feed: not every instance connected (%s)", f.connText())
	}
	// Live simulation: the virtual clock is the wall clock (RawFrame.Recv =
	// now), with the fixed seed and constant traffic density. The world is
	// seeded with traffic that is already under way when New is called.
	f.mu.Lock()
	f.sim = sim.New(sim.Config{Seed: f.seed, Aircraft: f.aircraft, Receiver: receiver, FeedID: "parity", NoDiurnal: true})
	f.started = true
	f.mu.Unlock()
	done := make(chan struct{})
	go func() { f.flushLoop(ctx); close(done) }()
	err := f.sim.Run(ctx, f)
	<-done
	return err
}

func (f *fanout) connText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := ""
	for i, p := range f.peers {
		s += fmt.Sprintf("stream %d: %d/%d ", i, len(p), f.clients)
	}
	return s
}

func (f *fanout) accept(idx int, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		p := &peer{conn: c, q: make(chan []byte, 1<<16)}
		f.mu.Lock()
		started := f.started
		f.peers[idx] = append(f.peers[idx], p)
		all := true
		for _, ps := range f.peers {
			if len(ps) < f.clients {
				all = false
			}
		}
		f.mu.Unlock()
		if started {
			// A reconnect after the start: this instance misses data the
			// others got, so the comparison is no longer meaningful.
			f.dropped.Add(1)
			logf("feed: WARNING late (re)connection on stream %d from %s", idx, c.RemoteAddr())
		}
		go p.write()
		if all {
			f.readyOnce.Do(func() { close(f.ready) })
		}
	}
}

func (p *peer) write() {
	for b := range p.q {
		if p.dead.Load() {
			continue
		}
		_ = p.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := p.conn.Write(b); err != nil {
			p.dead.Store(true)
		}
	}
	p.conn.Close()
}

// sameTx reports whether b is another channel's copy of transmission a (the
// simulator delivers them consecutively, sharing the Data array).
func sameTx(a, b *model.RawFrame) bool {
	return !a.MLAT && !b.MLAT && a.Kind == b.Kind && len(a.Data) > 0 && len(b.Data) > 0 && &a.Data[0] == &b.Data[0]
}

// HandleFrame implements model.FrameSink (called by the simulation).
func (f *fanout) HandleFrame(fr model.RawFrame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames.Add(1)
	for i, s := range f.streams {
		switch {
		case s.Channel < 0:
			// Combined: hold the frame until the next one shows whether
			// another channel heard the same transmission better.
			if fr.Kind == model.FrameStatus || fr.MLAT {
				f.emitPending(i)
				f.bufs[i] = sim.AppendBeastFrame(f.bufs[i], &fr)
				continue
			}
			if f.pend != nil && sameTx(f.pend, &fr) {
				if fr.Signal > f.pend.Signal {
					c := fr
					f.pend = &c
				}
				continue
			}
			f.emitPending(i)
			c := fr
			f.pend = &c
		case fr.Kind == model.FrameStatus:
			f.bufs[i] = sim.AppendBeastFrame(f.bufs[i], &fr)
		case fr.MLAT:
			if i == 0 {
				f.bufs[i] = sim.AppendBeastFrame(f.bufs[i], &fr)
			}
		case fr.Channel == s.Channel:
			f.bufs[i] = sim.AppendBeastFrame(f.bufs[i], &fr)
		}
	}
}

func (f *fanout) emitPending(i int) {
	if f.pend != nil {
		f.bufs[i] = sim.AppendBeastFrame(f.bufs[i], f.pend)
		f.pend = nil
	}
}

// HandleUpdate implements model.FrameSink (the simulator emits frames only).
func (f *fanout) HandleUpdate(model.DecodedUpdate) {}

func (f *fanout) flushLoop(ctx context.Context) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			f.mu.Lock()
			for i, s := range f.streams {
				if s.Channel < 0 {
					f.emitPending(i)
				}
			}
			f.mu.Unlock()
			f.flush()
			return
		case <-t.C:
			f.flush()
		}
	}
}

func (f *fanout) flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, b := range f.bufs {
		if len(b) == 0 {
			continue
		}
		chunk := append([]byte(nil), b...)
		f.bufs[i] = b[:0]
		f.bytes.Add(int64(len(chunk)))
		for _, p := range f.peers[i] {
			select {
			case p.q <- chunk:
			default:
				p.dead.Store(true) // a stuck reader: its instance diverges
				f.dropped.Add(1)
			}
		}
	}
}

// Close stops listening and closes every connection.
func (f *fanout) Close() {
	for _, ln := range f.lns {
		ln.Close()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ps := range f.peers {
		for _, p := range ps {
			closeOnce(p)
		}
	}
	f.peers = make([][]*peer, len(f.streams))
}

func closeOnce(p *peer) {
	defer func() { _ = recover() }()
	close(p.q)
}

// Diverged reports whether any instance lost or re-established a stream
// after the start.
func (f *fanout) Diverged() bool {
	if f.dropped.Load() > 0 {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ps := range f.peers {
		for _, p := range ps {
			if p.dead.Load() {
				return true
			}
		}
	}
	return false
}
