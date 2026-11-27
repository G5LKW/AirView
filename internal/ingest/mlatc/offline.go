package mlatc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Offline is one mlat-client session without a network connection: the
// handshake, every message line and the server's answers go through the
// same code as a TCP client's (admission, limits, tick normalisation,
// epochs, de-duplication, selection, results), but the caller supplies the
// lines and the time of their arrival. It exists for the replay of
// recorded client streams (DESIGN §13.3) and for virtual-time simulations
// that drive the ingest and the MLAT engine faster than real time.
//
// The session is uncompressed (the handshake's compress offer is ignored).
// Lines, Poll and Close may be called from any goroutine, one at a time;
// Server.Close ends an open session like any connection.
type Offline struct {
	mu   sync.Mutex
	c    *conn
	nc   *bufConn
	done chan struct{} // closed once the session was detached
}

// ErrOfflineClosed is returned by Offline.Lines after the session ended.
var ErrOfflineClosed = errors.New("mlatc: offline session closed")

// Offline runs a client handshake (one JSON line, as the client would send
// it) from remote at the server time now. It returns the session and the
// server's reply line (the deny line for a refusal, then with a nil
// session and an error).
func (s *Server) Offline(handshake []byte, remote netip.Addr, now time.Time) (*Offline, []byte, error) {
	remote = remote.Unmap()
	nc := &bufConn{remote: &net.TCPAddr{IP: remote.AsSlice(), Port: 30005}}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, nil, errors.New("mlatc: server closed")
	}
	st := s.ips[remote]
	if st == nil {
		st = &ipState{}
		s.ips[remote] = st
	}
	st.conns++
	st.unauth++
	s.nets[subnetOf(remote)]++
	s.nextID++
	c := newConn(s, nc, s.nextID, remote)
	c.offline = true
	c.since = now
	c.lastReadNs.Store(now.UnixNano())
	s.conns[c.id] = c
	s.wg.Add(1)
	s.mu.Unlock()
	s.c.accepted.Add(1)

	o := &Offline{c: c, nc: nc, done: make(chan struct{})}
	line := append(bytes.TrimRight(handshake, "\r\n"), '\n')
	if !c.handshake(newLineReader(bytes.NewReader(line))) {
		c.finish()
		s.wg.Done()
		close(o.done)
		return nil, nc.take(), errors.New("mlatc: handshake refused")
	}
	c.nowNs = now.UnixNano()
	go func() {
		<-c.done // Close, Server.Close, or a disconnect from processing
		o.mu.Lock()
		c.finish()
		o.mu.Unlock()
		s.wg.Done()
		close(o.done)
	}()
	return o, nc.take(), nil
}

// Station returns the session's station.
func (o *Offline) Station() Attachment { return o.c.att }

// Lines processes newline-separated JSON message lines that arrived at the
// server time now (a missing final newline is tolerated). Frames are handed
// to the Sink before it returns. It returns ErrOfflineClosed once the
// session ended (for example after a rate-limit or garbage disconnect).
func (o *Offline) Lines(data []byte, now time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.c
	if o.closed() {
		return ErrOfflineClosed
	}
	c.nowNs = now.UnixNano()
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		too := len(line) > maxLine
		if too {
			line = nil
		}
		if !c.processLine(line, too) || o.closed() {
			c.flushBatch()
			return ErrOfflineClosed
		}
	}
	c.flushBatch()
	return nil
}

// UDPKey returns the datagram key the handshake reply gave the session (0
// when the client did not offer udp_transport 2 or Options.UDP is off).
func (o *Offline) UDPKey() uint32 { return o.c.udpKey }

// Datagram processes one UDP datagram of the session (MLATC.md §9) that
// arrived at the server time now, as the UDP socket would. A datagram with
// another key is counted as unknown and ignored. It returns
// ErrOfflineClosed once the session ended.
func (o *Offline) Datagram(b []byte, now time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.c
	if o.closed() {
		return ErrOfflineClosed
	}
	s := c.s
	s.c.udpPackets.Add(1)
	s.c.udpBytes.Add(uint64(len(b)))
	switch {
	case len(b) < udpHeaderLen:
		s.c.udpBad.Add(1)
	case c.udpKey == 0 || binary.BigEndian.Uint32(b) != c.udpKey:
		s.c.udpUnknown.Add(1)
	default:
		c.datagram(b, now.UnixNano())
	}
	if o.closed() {
		return ErrOfflineClosed
	}
	return nil
}

// Poll returns what the server has queued for the client since the last
// call: start_sending / stop_sending updates, results and anything written
// during the handshake. now is the server time (unused by the selection,
// kept for symmetry with Lines).
func (o *Offline) Poll(time.Time) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.c
	out := o.nc.take()
	if o.closed() {
		return out
	}
	var start, stop []uint32
	c.sel.mu.Lock()
	start, stop = c.sel.take(start, stop)
	c.sel.mu.Unlock()
	for len(stop) > 0 {
		n := min(len(stop), 1000)
		out = appendAddrList(out, "stop_sending", stop[:n])
		stop = stop[n:]
	}
	for len(start) > 0 {
		n := min(len(start), 1000)
		out = appendAddrList(out, "start_sending", start[:n])
		start = start[n:]
	}
	out, _, _ = c.appendModeAC(out, nil, nil)
	c.outMu.Lock()
	out = append(out, c.out...)
	c.out = c.out[:0]
	c.outMu.Unlock()
	return out
}

// Close ends the session and waits until its station was detached.
func (o *Offline) Close() {
	o.c.shutdown(ReasonEOF)
	<-o.done
}

func (o *Offline) closed() bool {
	select {
	case <-o.c.done:
		return true
	default:
		return false
	}
}

// bufConn is the net.Conn of an Offline session: writes are collected,
// reads report EOF.
type bufConn struct {
	mu     sync.Mutex
	buf    []byte
	remote net.Addr
}

func (b *bufConn) take() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.buf
	b.buf = nil
	return out
}

func (b *bufConn) Read([]byte) (int, error) { return 0, net.ErrClosed }
func (b *bufConn) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	b.mu.Unlock()
	return len(p), nil
}
func (b *bufConn) Close() error                     { return nil }
func (b *bufConn) LocalAddr() net.Addr              { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 31090} }
func (b *bufConn) RemoteAddr() net.Addr             { return b.remote }
func (b *bufConn) SetDeadline(time.Time) error      { return nil }
func (b *bufConn) SetReadDeadline(time.Time) error  { return nil }
func (b *bufConn) SetWriteDeadline(time.Time) error { return nil }
