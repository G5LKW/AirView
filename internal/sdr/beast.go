package sdr

import (
	"context"
	"errors"
	"math"
	"net"
	"sync"
	"time"

	"airview/internal/model"
)

// Beast binary output (Jetvision/Mode-S Beast format, as documented on the
// Jetvision wiki and already parsed by internal/feed): 0x1A, a type byte
// ('1' Mode A/C, '2' 56-bit, '3' 112-bit), a 48-bit big-endian 12 MHz
// timestamp, a signal byte and the frame; every 0x1A inside the record is
// sent twice. The signal byte is the amplitude relative to full scale × 255,
// which is how internal/feed reads it back (20·log10(b/255) dBFS).
//
// It lets a demodulated RTL-SDR feed any Beast consumer, including AirView's
// own feeder input auto-detection, the uplinks and third-party tools.

// AppendBeast appends the Beast record of a Mode S frame to dst.
func AppendBeast(dst []byte, f model.RawFrame) []byte {
	var typ byte
	switch f.Kind {
	case model.FrameModeAC:
		typ = '1'
	case model.FrameModeSShort:
		typ = '2'
	case model.FrameModeSLong:
		typ = '3'
	default:
		return dst
	}
	ts := f.Clock12MHz
	if f.ClockCls == model.ClockSDR12 || f.ClockCls == model.ClockSDR12HiRes || f.ClockCls == model.ClockHW12 {
		ts = f.Ticks
	}
	sig := byte(0)
	if !math.IsNaN(f.Signal) {
		v := math.Round(255 * math.Pow(10, f.Signal/20))
		sig = byte(math.Max(0, math.Min(255, v)))
	}
	dst = append(dst, 0x1A, typ)
	put := func(b byte) {
		dst = append(dst, b)
		if b == 0x1A {
			dst = append(dst, b)
		}
	}
	for i := 5; i >= 0; i-- {
		put(byte(ts >> (8 * uint(i))))
	}
	put(sig)
	for _, b := range f.Data {
		put(b)
	}
	return dst
}

// BeastServer serves Beast binary to any number of TCP clients. It is a
// model.FrameSink: frames handed to HandleFrame are encoded once and queued
// to every client; a client that falls more than 256 frames behind
// is disconnected rather than slowing the receiver down.
type BeastServer struct {
	mu      sync.Mutex
	clients map[*beastClient]struct{}
	ln      net.Listener
	closed  bool
}

type beastClient struct {
	conn net.Conn
	q    chan []byte
}

// NewBeastServer returns a server that is not yet listening.
func NewBeastServer() *BeastServer {
	return &BeastServer{clients: make(map[*beastClient]struct{})}
}

// Listen binds addr (for example "127.0.0.1:30005"; ":0" picks a port) and
// serves until ctx is cancelled. It returns once listening; Addr reports the
// bound address.
func (s *BeastServer) Listen(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed || s.ln != nil {
		s.mu.Unlock()
		ln.Close()
		return errors.New("sdr: Beast server already used")
	}
	s.ln = ln
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.add(conn)
		}
	}()
	return nil
}

// Addr returns the listening address (nil before Listen).
func (s *BeastServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Clients returns the number of connected clients.
func (s *BeastServer) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

func (s *BeastServer) add(conn net.Conn) {
	c := &beastClient{conn: conn, q: make(chan []byte, 256)}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.Close()
		return
	}
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	go func() {
		defer s.drop(c)
		for b := range c.q {
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := conn.Write(b); err != nil {
				return
			}
		}
	}()
}

func (s *BeastServer) drop(c *beastClient) {
	s.mu.Lock()
	if _, ok := s.clients[c]; ok {
		delete(s.clients, c)
		close(c.q)
	}
	s.mu.Unlock()
	c.conn.Close()
}

// HandleFrame queues one frame to every client.
func (s *BeastServer) HandleFrame(f model.RawFrame) {
	b := AppendBeast(nil, f)
	if len(b) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		select {
		case c.q <- b:
		default: // too slow: disconnect
			delete(s.clients, c)
			close(c.q)
			c.conn.Close()
		}
	}
}

// HandleUpdate ignores decoded updates (Beast carries raw frames only).
func (s *BeastServer) HandleUpdate(model.DecodedUpdate) {}

// Close stops listening and disconnects every client.
func (s *BeastServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for c := range s.clients {
		delete(s.clients, c)
		close(c.q)
		c.conn.Close()
	}
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}
