package sdr

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

// FakeRTLTCP is an rtl_tcp server for tests, demos and the feeder's
// "no hardware" checks: it speaks the rtl_tcp protocol (header, 5-byte
// commands) and streams the I/Q bytes produced by Source. It serves one
// client at a time, like the real server.
type FakeRTLTCP struct {
	Tuner     TunerType
	GainCount uint32
	// Source returns the next n complex samples (2n bytes). It is called
	// from the serving goroutine only.
	Source func(n int) []byte
	// Rate paces the stream in samples per second (0 = as fast as the
	// client reads).
	Rate float64
	// ChunkSamples is the number of samples per write (default 16384).
	ChunkSamples int
	// Drop, when set, is asked before each chunk; true discards the chunk
	// without sending it (time still advances), like a real server whose
	// client reads too slowly.
	Drop func(chunk int) bool
	// CloseAfter closes the connection after this many chunks (0 = never).
	CloseAfter int

	mu   sync.Mutex
	cmds []FakeCommand
}

// FakeCommand is one command received by FakeRTLTCP.
type FakeCommand struct {
	Cmd   byte
	Param uint32
}

// Commands returns the commands received so far.
func (f *FakeRTLTCP) Commands() []FakeCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCommand(nil), f.cmds...)
}

// Serve accepts clients on ln until ctx is cancelled or ln is closed.
func (f *FakeRTLTCP) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		f.serveConn(ctx, conn)
	}
}

func (f *FakeRTLTCP) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	var h [12]byte
	copy(h[:], "RTL0")
	binary.BigEndian.PutUint32(h[4:], uint32(f.Tuner))
	binary.BigEndian.PutUint32(h[8:], f.GainCount)
	if _, err := conn.Write(h[:]); err != nil {
		return
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		var b [5]byte
		for {
			if _, err := io.ReadFull(conn, b[:]); err != nil {
				return
			}
			f.mu.Lock()
			f.cmds = append(f.cmds, FakeCommand{b[0], binary.BigEndian.Uint32(b[1:])})
			f.mu.Unlock()
		}
	}()
	n := f.ChunkSamples
	if n <= 0 {
		n = 16384
	}
	start := time.Now()
	sent := 0
	for i := 0; ; i++ {
		if ctx.Err() != nil || (f.CloseAfter > 0 && i >= f.CloseAfter) {
			return
		}
		data := f.Source(n)
		if f.Rate > 0 {
			due := start.Add(time.Duration(float64(sent) / f.Rate * float64(time.Second)))
			if d := time.Until(due); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return
				}
			}
		}
		sent += len(data) / 2
		if f.Drop != nil && f.Drop(i) {
			continue
		}
		if _, err := conn.Write(data); err != nil {
			return
		}
	}
}
