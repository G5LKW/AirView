package acars

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Listener limits.
const (
	// MaxLineBytes bounds one JSON object (dumpvdl2 lines with decoded
	// ACARS applications can reach a few kB).
	MaxLineBytes = 64 << 10
	// MaxConns bounds concurrent TCP senders.
	MaxConns = 8
	// RatePerSec / RateBurst bound the accepted lines (all inputs
	// together); a busy VDL2 frequency set produces a few lines per
	// second.
	RatePerSec = 50
	RateBurst  = 200
	// idleTimeout closes a silent TCP sender.
	idleTimeout = 15 * time.Minute
)

// ListenConfig selects the inputs.
type ListenConfig struct {
	UDP string // "host:port" for JSON datagrams ("" = off)
	TCP string // "host:port" for newline-separated JSON over TCP ("" = off)
}

// InputStatus is the state of the listeners.
type InputStatus struct {
	UDP      string `json:"udp,omitempty"`
	UDPError string `json:"udpError,omitempty"`
	TCP      string `json:"tcp,omitempty"`
	TCPError string `json:"tcpError,omitempty"`
	TCPConns int    `json:"tcpConns"`
}

// Listener feeds a Store from UDP and TCP inputs.
type Listener struct {
	st  *Store
	cfg ListenConfig
	log *slog.Logger

	mu     sync.Mutex
	status InputStatus
	conns  atomic.Int32

	bucket struct {
		sync.Mutex
		tokens float64
		last   time.Time
	}
	now func() time.Time
}

// NewListener returns a listener for st.
func NewListener(st *Store, cfg ListenConfig, log *slog.Logger) *Listener {
	if log == nil {
		log = slog.Default()
	}
	l := &Listener{st: st, cfg: cfg, log: log, now: time.Now}
	l.bucket.tokens = RateBurst
	return l
}

// Status returns the listener state.
func (l *Listener) Status() InputStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.status
	s.TCPConns = int(l.conns.Load())
	return s
}

// Run listens until ctx is cancelled. A listener that cannot bind is
// reported in Status; Run then keeps serving the other one.
func (l *Listener) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	if l.cfg.UDP != "" {
		pc, err := net.ListenPacket("udp", l.cfg.UDP)
		l.mu.Lock()
		if err != nil {
			l.status.UDPError = err.Error()
		} else {
			l.status.UDP = pc.LocalAddr().String()
		}
		l.mu.Unlock()
		if err != nil {
			l.log.Warn("ACARS UDP input not started", "addr", l.cfg.UDP, "err", err)
		} else {
			wg.Add(1)
			go func() { defer wg.Done(); l.serveUDP(ctx, pc) }()
		}
	}
	if l.cfg.TCP != "" {
		ln, err := net.Listen("tcp", l.cfg.TCP)
		l.mu.Lock()
		if err != nil {
			l.status.TCPError = err.Error()
		} else {
			l.status.TCP = ln.Addr().String()
		}
		l.mu.Unlock()
		if err != nil {
			l.log.Warn("ACARS TCP input not started", "addr", l.cfg.TCP, "err", err)
		} else {
			wg.Add(1)
			go func() { defer wg.Done(); l.serveTCP(ctx, ln) }()
		}
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (l *Listener) serveUDP(ctx context.Context, pc net.PacketConn) {
	go func() { <-ctx.Done(); _ = pc.Close() }()
	buf := make([]byte, 65536)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		// One datagram usually holds one object; split on newlines in
		// case a sender batches them.
		for line := range bytes.SplitSeq(buf[:n], []byte{'\n'}) {
			l.Ingest(line)
		}
	}
}

func (l *Listener) serveTCP(ctx context.Context, ln net.Listener) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	open := map[net.Conn]struct{}{}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		mu.Lock()
		for c := range open {
			_ = c.Close()
		}
		mu.Unlock()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if int(l.conns.Load()) >= MaxConns {
			_ = c.Close()
			continue
		}
		l.conns.Add(1)
		mu.Lock()
		open[c] = struct{}{}
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer l.conns.Add(-1)
			defer func() { mu.Lock(); delete(open, c); mu.Unlock(); _ = c.Close() }()
			l.readStream(c)
		}()
	}
	wg.Wait()
}

func (l *Listener) readStream(c net.Conn) {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 4096), MaxLineBytes)
	for {
		_ = c.SetReadDeadline(time.Now().Add(idleTimeout))
		if !sc.Scan() {
			return // EOF, idle, closed or an over-long line
		}
		l.Ingest(sc.Bytes())
	}
}

// Ingest parses and stores one line (blank lines are ignored). It applies
// the input rate limit.
func (l *Listener) Ingest(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	now := l.now()
	if !l.take(now) {
		l.st.CountDropped(1)
		return
	}
	m, err := Parse(line, now)
	if err != nil {
		l.st.CountError(err)
		return
	}
	l.st.Add(m, now)
}

func (l *Listener) take(now time.Time) bool {
	b := &l.bucket
	b.Lock()
	defer b.Unlock()
	if !b.last.IsZero() {
		b.tokens = min(RateBurst, b.tokens+now.Sub(b.last).Seconds()*RatePerSec)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
