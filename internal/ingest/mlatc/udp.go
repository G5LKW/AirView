package mlatc

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// The mlat-client UDP transport (MLATC.md §9; DESIGN §5.2 phase 5 option
// aggregator.mlat.udp).
//
// A client whose handshake offers udp_transport 2 is answered with
// [host, port, key]; from then on it sends its timed frames (sync pairs,
// single syncs and mlat frames) as datagrams to host:port and everything
// else (seen/lost, rate_report, clock events, heartbeats) on TCP. The
// server never answers a datagram, so the socket cannot be used for
// amplification.
//
// Datagram layout (all integers big-endian; wire facts, MLATC.md §9):
//
//	header   u32 key, u16 sequence number, u64 base timestamp   (14 bytes)
//	then messages until the end of the datagram, each starting with a type byte:
//	  1 sync        i32 even delta, i32 odd delta, 14-byte even frame, 14-byte odd frame (37)
//	  2 mlat short  i32 delta, 7-byte frame                                             (12)
//	  3 mlat long   i32 delta, 14-byte frame                                            (19)
//	  4 ssync       i32 delta, 14-byte frame                                            (19)
//	  5 rebase      u64 new base timestamp                                              (9)
//	  6 abs sync    u64 even timestamp, u64 odd timestamp, 14-byte even, 14-byte odd    (45)
//
// A timestamp is base + delta: exactly the value the client would have put
// into the JSON "t" / "et" / "ot" field, so the TCP tick normalisation
// applies unchanged. A Mode A/C reply (2 bytes) has no message type of its
// own; the client packs it as a long frame padded with 12 zero bytes, which
// no valid Mode S long frame can be (its last 3 bytes are parity), so such
// a payload is taken as Mode A/C (to confirm against a capture).
//
// Packets that cannot be attributed are dropped and counted: shorter than
// the header, an unknown key, a source address other than the TCP
// connection's (unless Options.UDPAnySource), or a duplicate sequence
// number within the last 64. A message of unknown type ends the parse of
// its datagram (its length is unknown); a datagram with such a message or
// a truncated one counts as malformed, and a client whose malformed
// datagrams exceed Policy.MaxBadLines and half of its datagrams is
// disconnected like a client sending garbage lines.

const (
	udpHeaderLen = 14

	udpSync      = 1
	udpMLATShort = 2
	udpMLATLong  = 3
	udpSSync     = 4
	udpRebase    = 5
	udpAbsSync   = 6

	// udpVersion is the datagram format the server speaks
	// (handshake "udp_transport": 2).
	udpVersion = 2

	// udpMaxDatagram is the receive buffer: the reference client flushes
	// at about 1,400 bytes; anything up to the IPv4 maximum is accepted.
	udpMaxDatagram = 65535
)

// seqWindow tracks a client's datagram sequence numbers (16 bit, wrapping)
// for loss and duplicate accounting.
type seqWindow struct {
	init bool
	hi   uint16 // highest sequence number seen (modulo 2^16)
	bits uint64 // bit i set: hi-i was seen
}

// note records seq. gap is the change of the loss estimate (sequence
// numbers skipped, -1 when a late datagram fills an earlier gap); dup
// reports a repeat within the window; late an out-of-order arrival.
func (w *seqWindow) note(seq uint16) (gap int, dup, late bool) {
	if !w.init {
		w.init, w.hi, w.bits = true, seq, 1
		return 0, false, false
	}
	d := int(int16(seq - w.hi))
	if d > 0 {
		if d >= 64 {
			w.bits = 0
		} else {
			w.bits <<= uint(d)
		}
		w.bits |= 1
		w.hi = seq
		return d - 1, false, false
	}
	i := uint(-d)
	if i >= 64 {
		return 0, false, true // too old to tell: accept
	}
	if w.bits&(1<<i) != 0 {
		return 0, true, false
	}
	w.bits |= 1 << i
	return -1, false, true
}

// ------------------------------------------------------------ socket

// startUDP opens the datagram socket next to the TCP listener ln
// (Options.UDPAddr, default the same address and port). A failure is
// logged and kept for UDPStatus; clients then stay on TCP.
func (s *Server) startUDP(ln net.Listener) {
	addr := s.o.UDPAddr
	if addr == "" {
		addr = ln.Addr().String()
	}
	ua, err := net.ResolveUDPAddr("udp", addr)
	var pc *net.UDPConn
	if err == nil {
		pc, err = net.ListenUDP("udp", ua)
	}
	if err == nil {
		err = s.ServeUDP(pc)
		if err != nil {
			pc.Close()
		}
	}
	if err != nil {
		s.mu.Lock()
		s.udpErr = err.Error()
		s.mu.Unlock()
		s.o.Logger.Error("mlat-client UDP transport not available; clients stay on TCP", "addr", addr, "err", err)
	}
}

// ServeUDP accepts mlat-client datagrams on pc until Close; it returns at
// once (the readers run in the background). Serve calls it itself when
// Options.UDP is set; call it directly only to supply a socket of your
// own. Only one socket is served.
func (s *Server) ServeUDP(pc *net.UDPConn) error {
	if pc == nil {
		return errors.New("mlatc: nil UDP socket")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return errors.New("mlatc: server closed")
	}
	if s.udpPC != nil {
		return errors.New("mlatc: UDP socket already served")
	}
	s.udpPC = pc
	s.udpErr = ""
	if ua, ok := pc.LocalAddr().(*net.UDPAddr); ok {
		s.udpPort = uint16(ua.Port)
	}
	_ = pc.SetReadBuffer(4 << 20) // bursts of 1,000+ clients; best effort
	for range s.o.UDPReaders {
		s.wg.Add(1)
		go s.udpReader(pc)
	}
	s.o.Logger.Info("mlat-client UDP transport listening", "addr", pc.LocalAddr().String())
	return nil
}

// UDPAddr returns the address of the datagram socket (nil when none).
func (s *Server) UDPAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.udpPC == nil {
		return nil
	}
	return s.udpPC.LocalAddr()
}

// UDPStatus reports whether datagrams are accepted, and the bind error
// when Options.UDP is set but the socket could not be opened.
func (s *Server) UDPStatus() (active bool, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.udpPC != nil && !s.closing, s.udpErr
}

func (s *Server) udpActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.udpPC != nil && !s.closing
}

// udpReader reads datagrams until the socket is closed. Each reader owns
// its buffer; datagrams of one client may be handled by different readers
// concurrently (the client's processing lock orders them).
func (s *Server) udpReader(pc *net.UDPConn) {
	defer s.wg.Done()
	buf := make([]byte, udpMaxDatagram)
	errs := 0
	for {
		n, from, err := pc.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || s.isClosing() {
				return
			}
			// Transient (an ICMP error reported on the socket, buffer
			// exhaustion): back off a little when it repeats.
			if errs++; errs > 10 {
				time.Sleep(min(time.Duration(errs)*time.Millisecond, 100*time.Millisecond))
			}
			continue
		}
		errs = 0
		s.handleDatagram(buf[:n], from.Addr(), s.o.Now().UnixNano())
	}
}

// handleDatagram attributes one datagram to its client and processes it.
func (s *Server) handleDatagram(b []byte, from netip.Addr, nowNs int64) {
	s.c.udpPackets.Add(1)
	s.c.udpBytes.Add(uint64(len(b)))
	if len(b) < udpHeaderLen {
		s.c.udpBad.Add(1)
		return
	}
	key := binary.BigEndian.Uint32(b)
	s.udpMu.RLock()
	c := s.udpKeys[key]
	s.udpMu.RUnlock()
	if c == nil {
		s.c.udpUnknown.Add(1)
		return
	}
	if !s.o.UDPAnySource && from.Unmap() != c.ip {
		s.c.udpBadSrc.Add(1)
		return
	}
	c.datagram(b, nowNs)
}

// ------------------------------------------------------------ keys

// udpReserve picks a fresh non-zero key that no live client holds and
// reserves it (no datagram is attributed to it until udpPublish, so none
// can overtake the epoch start at the end of the handshake).
func (s *Server) udpReserve() (uint32, error) {
	var b [4]byte
	s.udpMu.Lock()
	defer s.udpMu.Unlock()
	for range 64 {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		k := binary.BigEndian.Uint32(b[:])
		if _, taken := s.udpKeys[k]; k == 0 || taken {
			continue
		}
		s.udpKeys[k] = nil
		return k, nil
	}
	return 0, errors.New("mlatc: no free UDP key")
}

// udpPublish makes c's reserved key live (end of the handshake).
func (s *Server) udpPublish(c *conn) {
	if c.udpKey == 0 || c.offline {
		return
	}
	s.udpMu.Lock()
	if v, ok := s.udpKeys[c.udpKey]; ok && v == nil {
		s.udpKeys[c.udpKey] = c
	}
	s.udpMu.Unlock()
}

// udpUnregister forgets c's key (finish): later datagrams with it are
// unknown.
func (s *Server) udpUnregister(c *conn) {
	if c.udpKey == 0 || c.offline {
		return
	}
	s.udpMu.Lock()
	if v, ok := s.udpKeys[c.udpKey]; ok && (v == c || v == nil) {
		delete(s.udpKeys, c.udpKey)
	}
	s.udpMu.Unlock()
}

// udpOffer decides the handshake's udp_transport value for c: false, or
// [host, port, key] (registering the key). Offline sessions get a key when
// Options.UDP is set (Offline.Datagram), without a socket.
func (s *Server) udpOffer(c *conn) any {
	if !s.o.UDP || c.hello.udp != udpVersion {
		return false
	}
	port := s.o.UDPPort
	if !c.offline {
		if !s.udpActive() {
			return false
		}
		if port == 0 {
			s.mu.Lock()
			port = int(s.udpPort)
			s.mu.Unlock()
		}
	} else if port == 0 {
		port = 31090
	}
	host := s.o.UDPHost
	if host == "" {
		if ta, ok := c.nc.LocalAddr().(*net.TCPAddr); ok {
			if a, ok := netip.AddrFromSlice(ta.IP); ok {
				host = a.Unmap().WithZone("").String()
			}
		}
	}
	if host == "" || port <= 0 {
		return false
	}
	if c.offline {
		k, err := randomKey()
		if err != nil {
			return false
		}
		c.udpKey = k
	} else {
		k, err := s.udpReserve()
		if err != nil {
			s.o.Logger.Warn("mlat-client UDP key", "err", err)
			return false
		}
		c.udpKey = k
	}
	return []any{host, port, c.udpKey}
}

func randomKey() (uint32, error) {
	var b [4]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		if k := binary.BigEndian.Uint32(b[:]); k != 0 {
			return k, nil
		}
	}
}

// ------------------------------------------------------------ per client

// datagram processes one datagram of c (any goroutine): the client's
// processing lock orders it with the TCP reader, so epochs, de-duplication
// and batching see one sequence of frames.
func (c *conn) datagram(b []byte, nowNs int64) {
	c.proc.Lock()
	defer c.proc.Unlock()
	if c.procClosed {
		return
	}
	s := c.s
	c.wire.n.Add(uint64(len(b)))
	c.lastReadNs.Store(nowNs)
	c.udpPkts.Add(1)
	saved := c.nowNs
	c.nowNs = nowNs
	c.datagramLocked(b)
	c.flushBatch()
	c.nowNs = saved
	if bad := c.udpBadN.Load(); int(bad) > s.policy().MaxBadLines && bad*2 > c.udpPkts.Load() {
		c.shutdown(ReasonGarbage)
	}
}

func (c *conn) datagramLocked(b []byte) {
	s := c.s
	if !c.rateTick() {
		return
	}
	gap, dup, late := c.udpSeq.note(binary.BigEndian.Uint16(b[4:]))
	switch {
	case dup:
		c.udpDupN.Add(1)
		s.c.udpDup.Add(1)
		return
	case late:
		s.c.udpLate.Add(1)
	}
	if gap != 0 {
		c.udpLost += int64(gap)
		c.udpLostN.Store(uint64(max(c.udpLost, 0)))
		if gap > 0 {
			s.c.udpLost.Add(uint64(gap))
		}
	}
	if !c.udpMessages(b[udpHeaderLen:], binary.BigEndian.Uint64(b[6:])) {
		c.udpBadN.Add(1)
		s.c.udpBad.Add(1)
	}
}

// udpMessages decodes the messages of one datagram and hands them to the
// same path as JSON lines. false = malformed (truncated or unknown type);
// the messages before the fault are kept.
func (c *conn) udpMessages(p []byte, base uint64) bool {
	m := &c.um
	for len(p) > 0 {
		select {
		case <-c.done:
			return true
		default:
		}
		m.n = 1
		m.tok[1], m.m[1] = false, nil
		switch p[0] {
		case udpSync:
			if len(p) < 37 {
				return false
			}
			m.kind, m.n = kSync, 2
			m.t[0], m.tok[0] = addDelta(base, p[1:5])
			m.t[1], m.tok[1] = addDelta(base, p[5:9])
			m.m[0], m.m[1] = p[9:23], p[23:37]
			p = p[37:]
		case udpMLATShort:
			if len(p) < 12 {
				return false
			}
			m.kind = kMLAT
			m.t[0], m.tok[0] = addDelta(base, p[1:5])
			m.m[0] = p[5:12]
			p = p[12:]
		case udpMLATLong, udpSSync:
			if len(p) < 19 {
				return false
			}
			m.kind = kMLAT
			if p[0] == udpSSync {
				m.kind = kSSync
			}
			m.t[0], m.tok[0] = addDelta(base, p[1:5])
			m.m[0] = p[5:19]
			if m.kind == kMLAT && paddedModeAC(m.m[0]) {
				m.m[0] = m.m[0][:2]
			}
			p = p[19:]
		case udpRebase:
			if len(p) < 9 {
				return false
			}
			base = binary.BigEndian.Uint64(p[1:9])
			p = p[9:]
			continue
		case udpAbsSync:
			if len(p) < 45 {
				return false
			}
			m.kind, m.n = kSync, 2
			m.t[0], m.tok[0] = absTime(p[1:9])
			m.t[1], m.tok[1] = absTime(p[9:17])
			m.m[0], m.m[1] = p[17:31], p[31:45]
			p = p[45:]
		default:
			return false
		}
		c.handleMsg(m)
		if c.split && !c.checkSplit() {
			return true
		}
	}
	return true
}

// addDelta returns base + the signed 32-bit delta in d; ok false when the
// result would be negative (the JSON path rejects negative timestamps too).
func addDelta(base uint64, d []byte) (uint64, bool) {
	delta := int64(int32(binary.BigEndian.Uint32(d)))
	if delta < 0 && base < uint64(-delta) {
		return 0, false
	}
	return base + uint64(delta), true
}

// absTime reads an absolute 64-bit timestamp (abs sync); values that do not
// fit a non-negative int64 are rejected like a negative JSON timestamp.
func absTime(d []byte) (uint64, bool) {
	t := binary.BigEndian.Uint64(d)
	return t, t <= 1<<63-1
}

// paddedModeAC reports a 14-byte payload that is a 2-byte Mode A/C reply
// followed by 12 zero bytes (see the package comment above).
func paddedModeAC(b []byte) bool {
	if len(b) != 14 {
		return false
	}
	for _, x := range b[2:] {
		if x != 0 {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------ client side

// AppendUDPHeader / AppendUDP* build datagrams in the client's format. They
// exist for AirView's simulator, load generator and tests (the server does
// not send datagrams).
func AppendUDPHeader(dst []byte, key uint32, seq uint16, base uint64) []byte {
	dst = binary.BigEndian.AppendUint32(dst, key)
	dst = binary.BigEndian.AppendUint16(dst, seq)
	return binary.BigEndian.AppendUint64(dst, base)
}

// AppendUDPMLAT appends an mlat message (7- or 14-byte Mode S frame, or a
// 2-byte Mode A/C reply, which is padded to 14 bytes as the client does).
func AppendUDPMLAT(dst []byte, delta int32, frame []byte) ([]byte, error) {
	switch len(frame) {
	case 7:
		dst = append(dst, udpMLATShort)
		dst = binary.BigEndian.AppendUint32(dst, uint32(delta))
		return append(dst, frame...), nil
	case 2, 14:
		dst = append(dst, udpMLATLong)
		dst = binary.BigEndian.AppendUint32(dst, uint32(delta))
		dst = append(dst, frame...)
		return append(dst, make([]byte, 14-len(frame))...), nil
	}
	return dst, fmt.Errorf("mlatc: frame of %d bytes", len(frame))
}

// AppendUDPSSync appends a single-sync message (14-byte DF17 frame).
func AppendUDPSSync(dst []byte, delta int32, frame []byte) ([]byte, error) {
	if len(frame) != 14 {
		return dst, fmt.Errorf("mlatc: ssync frame of %d bytes", len(frame))
	}
	dst = append(dst, udpSSync)
	dst = binary.BigEndian.AppendUint32(dst, uint32(delta))
	return append(dst, frame...), nil
}

// AppendUDPSync appends a sync pair with deltas from the datagram's base.
func AppendUDPSync(dst []byte, eDelta, oDelta int32, even, odd []byte) ([]byte, error) {
	if len(even) != 14 || len(odd) != 14 {
		return dst, errors.New("mlatc: sync frames must be 14 bytes")
	}
	dst = append(dst, udpSync)
	dst = binary.BigEndian.AppendUint32(dst, uint32(eDelta))
	dst = binary.BigEndian.AppendUint32(dst, uint32(oDelta))
	dst = append(dst, even...)
	return append(dst, odd...), nil
}

// AppendUDPAbsSync appends a sync pair with absolute timestamps.
func AppendUDPAbsSync(dst []byte, et, ot uint64, even, odd []byte) ([]byte, error) {
	if len(even) != 14 || len(odd) != 14 {
		return dst, errors.New("mlatc: sync frames must be 14 bytes")
	}
	dst = append(dst, udpAbsSync)
	dst = binary.BigEndian.AppendUint64(dst, et)
	dst = binary.BigEndian.AppendUint64(dst, ot)
	dst = append(dst, even...)
	return append(dst, odd...), nil
}

// AppendUDPRebase appends a rebase message: later deltas count from base.
func AppendUDPRebase(dst []byte, base uint64) []byte {
	dst = append(dst, udpRebase)
	return binary.BigEndian.AppendUint64(dst, base)
}
