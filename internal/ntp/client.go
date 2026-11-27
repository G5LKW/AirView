package ntp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// Errors returned by Query. Test with errors.Is.
var (
	// ErrTimeout: no valid reply arrived before the deadline.
	ErrTimeout = errors.New("ntp: timeout waiting for reply")
	// ErrUnsynchronized: the server answered but says its own clock is not
	// synchronised (leap indicator 3, stratum 16, or root distance >= 16 s).
	ErrUnsynchronized = errors.New("ntp: server clock is not synchronised")
	// ErrInvalidResponse: the reply is malformed or fails the RFC sanity checks.
	ErrInvalidResponse = errors.New("ntp: invalid response")
	// ErrKissOfDeath matches any *KissError.
	ErrKissOfDeath = errors.New("ntp: kiss-o'-death")
	// ErrClockStepped: the local clock was stepped while the exchange was in
	// flight, so the measurement is meaningless.
	ErrClockStepped = errors.New("ntp: local clock stepped during measurement")
	// ErrNoServer: the server string is empty or unusable.
	ErrNoServer = errors.New("ntp: no server configured")
)

// KissError is a Kiss-o'-Death packet (stratum 0): the server refuses service
// and Code says why, e.g. "RATE" (polling too fast), "DENY"/"RSTR" (access
// denied), "INIT"/"STEP" (server not ready yet).
type KissError struct {
	Code string
}

func (e *KissError) Error() string { return "ntp: kiss-o'-death from server: " + e.Code }

// Is makes errors.Is(err, ErrKissOfDeath) true for every KissError.
func (e *KissError) Is(target error) bool { return target == ErrKissOfDeath }

// Response is the result of one successful client/server exchange.
type Response struct {
	Server string // normalised "host:port" that was queried
	Addr   string // resolved remote address that answered

	// Time is the host system clock when the reply arrived (T4).
	Time time.Time
	// Offset = server time - host system clock: add it to time.Now() to get UTC.
	Offset time.Duration
	// Delay is the network round-trip time excluding server processing.
	Delay time.Duration

	Stratum   int    // 1 = primary reference, 2..15 = secondary
	RefID     string // "GPS", "PPS", ... for stratum 1; upstream IPv4 for stratum 2+
	RefIDRaw  uint32 //
	Leap      int    // Leap* constants (never LeapNotInSync in a successful response)
	Version   int    // version number the server replied with
	Poll      int    // server's poll exponent (log2 seconds)
	Precision int    // server clock precision exponent (log2 seconds)

	PrecisionDur   time.Duration // 2^Precision seconds
	RootDelay      time.Duration // server's round trip to its primary reference
	RootDispersion time.Duration // server's dispersion to its primary reference
	// RootDistance is the maximum error bound of this measurement:
	// RootDelay/2 + RootDispersion + Delay/2.
	RootDistance time.Duration

	ReferenceTime time.Time // when the server clock was last set/corrected (zero if not reported)
	ReceiveTime   time.Time // T2, server clock
	TransmitTime  time.Time // T3, server clock
}

// Client performs SNTP queries. The zero value is ready to use.
type Client struct {
	// Timeout bounds one query (DNS + exchange) when the context carries no
	// earlier deadline. Default 3 s.
	Timeout time.Duration
	// Version is the NTP version number sent in requests (3 or 4). Default 4.
	Version int
}

// DefaultTimeout is the per-query timeout used when Client.Timeout is zero.
const DefaultTimeout = 3 * time.Second

var defaultClient Client

// Query performs one SNTP exchange with server using default settings.
// server is "host", "host:port", an IPv4/IPv6 literal, or "[v6]:port";
// the default port is 123.
func Query(ctx context.Context, server string) (*Response, error) {
	return defaultClient.Query(ctx, server)
}

// NormalizeServer turns a user-entered server string into "host:port",
// adding the default NTP port when none is given. It accepts host names,
// IPv4 and IPv6 literals (bare, bracketed, with zone), each with or without
// a port.
func NormalizeServer(server string) (string, error) {
	s := strings.TrimSpace(server)
	if s == "" {
		return "", ErrNoServer
	}
	if strings.ContainsAny(s, " \t\r\n/") {
		return "", fmt.Errorf("ntp: invalid server %q", server)
	}
	// Bare IPv6 literal (two or more colons, no brackets): never has a port.
	if strings.Count(s, ":") >= 2 && !strings.HasPrefix(s, "[") {
		if _, err := netip.ParseAddr(s); err != nil {
			return "", fmt.Errorf("ntp: invalid server %q: %v", server, err)
		}
		return net.JoinHostPort(s, DefaultPort), nil
	}
	// "[v6]" without a port.
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		h := s[1 : len(s)-1]
		if _, err := netip.ParseAddr(h); err != nil {
			return "", fmt.Errorf("ntp: invalid server %q: %v", server, err)
		}
		return net.JoinHostPort(h, DefaultPort), nil
	}
	if host, port, err := net.SplitHostPort(s); err == nil {
		if host == "" {
			return "", fmt.Errorf("ntp: invalid server %q: empty host", server)
		}
		if port == "" {
			port = DefaultPort
		}
		if _, err := net.LookupPort("udp", port); err != nil {
			return "", fmt.Errorf("ntp: invalid server %q: bad port", server)
		}
		return net.JoinHostPort(host, port), nil
	}
	if strings.ContainsAny(s, "[]:") {
		return "", fmt.Errorf("ntp: invalid server %q", server)
	}
	return net.JoinHostPort(s, DefaultPort), nil
}

// Query performs one SNTP exchange. It sends a client-mode packet whose
// transmit timestamp is a random nonce (so nothing about the local clock is
// disclosed and off-path spoofing is impractical), waits for the reply whose
// origin timestamp echoes that nonce, and validates it.
//
// Errors: ErrNoServer, ErrTimeout, ErrUnsynchronized, ErrInvalidResponse,
// ErrClockStepped, *KissError (errors.Is(err, ErrKissOfDeath)), or a network
// error (DNS failure, port unreachable). ctx cancellation returns ctx.Err().
func (c *Client) Query(ctx context.Context, server string) (*Response, error) {
	addr, err := NormalizeServer(server)
	if err != nil {
		return nil, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w (resolving %s)", ErrTimeout, addr)
		}
		return nil, fmt.Errorf("ntp: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// Unblock the read promptly if the caller cancels.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	version := c.Version
	if version != 3 {
		version = currentVersion
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("ntp: nonce: %w", err)
	}
	req := Packet{
		Version:  uint8(version),
		Mode:     ModeClient,
		Transmit: Timestamp(binary.BigEndian.Uint64(nonce[:])),
	}
	if req.Transmit == 0 {
		req.Transmit = 1 // zero means "unknown" on the wire
	}
	out := req.Marshal()

	mono := time.Now()
	t1 := preciseNow()
	if _, err := conn.Write(out[:]); err != nil {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		return nil, fmt.Errorf("ntp: send to %s: %w", addr, err)
	}

	buf := make([]byte, 1024)
	var lastBad error
	for {
		n, err := conn.Read(buf)
		t4 := preciseNow()
		monoElapsed := time.Since(mono)
		if err != nil {
			if parent.Err() != nil {
				return nil, parent.Err()
			}
			var ne net.Error
			if ctx.Err() != nil || (errors.As(err, &ne) && ne.Timeout()) {
				if lastBad != nil {
					return nil, fmt.Errorf("%w from %s (%v)", ErrTimeout, addr, lastBad)
				}
				return nil, fmt.Errorf("%w from %s", ErrTimeout, addr)
			}
			return nil, fmt.Errorf("ntp: receive from %s: %w", addr, err)
		}
		pkt, perr := ParsePacket(buf[:n])
		if perr != nil {
			lastBad = perr
			continue
		}
		if pkt.Origin != req.Transmit {
			// Late duplicate of an earlier exchange or a spoofed packet: ignore
			// it and keep waiting for the real answer.
			lastBad = errors.New("origin timestamp mismatch")
			continue
		}
		// The local clock must not have been stepped mid-exchange: compare the
		// wall-clock elapsed time with the monotonic one (coarse on Windows).
		if diff := t4.Sub(t1) - monoElapsed; diff > 250*time.Millisecond || diff < -250*time.Millisecond {
			return nil, ErrClockStepped
		}
		resp, verr := buildResponse(&pkt, t1, t4)
		if verr != nil {
			return nil, verr
		}
		resp.Server = addr
		resp.Addr = conn.RemoteAddr().String()
		return resp, nil
	}
}

// buildResponse validates a server packet whose origin timestamp has already
// been matched and computes the measurement. t1/t4 are local wall-clock times
// without monotonic readings.
func buildResponse(p *Packet, t1, t4 time.Time) (*Response, error) {
	if p.Mode != ModeServer {
		return nil, fmt.Errorf("%w: mode %d, want server (4)", ErrInvalidResponse, p.Mode)
	}
	if p.Version < minServerVersion || p.Version > currentVersion {
		return nil, fmt.Errorf("%w: version %d", ErrInvalidResponse, p.Version)
	}
	if p.Stratum == 0 {
		code := FormatRefID(0, p.RefID)
		if code == "" {
			code = "unspecified"
		}
		return nil, &KissError{Code: code}
	}
	if p.Leap == LeapNotInSync || p.Stratum > maxStratum {
		return nil, fmt.Errorf("%w (leap %d, stratum %d)", ErrUnsynchronized, p.Leap, p.Stratum)
	}
	if p.Transmit == 0 || p.Receive == 0 {
		return nil, fmt.Errorf("%w: zero server timestamp", ErrInvalidResponse)
	}
	rootDelay := p.RootDelay.Duration()
	rootDisp := p.RootDispersion.Duration()
	if rootDelay/2+rootDisp >= maxRootDistance {
		return nil, fmt.Errorf("%w (root distance %v)", ErrUnsynchronized, rootDelay/2+rootDisp)
	}
	t2 := p.Receive.Time(t4)
	t3 := p.Transmit.Time(t4)
	if t3.Before(t2) {
		return nil, fmt.Errorf("%w: transmit before receive", ErrInvalidResponse)
	}
	offset, delay := offsetDelay(t1, t2, t3, t4)
	if delay < 0 {
		// Possible when the path is faster than the clock resolution of
		// either side; a negative round trip is meaningless.
		delay = 0
	}
	r := &Response{
		Time:           t4,
		Offset:         offset,
		Delay:          delay,
		Stratum:        int(p.Stratum),
		RefID:          FormatRefID(int(p.Stratum), p.RefID),
		RefIDRaw:       p.RefID,
		Leap:           int(p.Leap),
		Version:        int(p.Version),
		Poll:           int(p.Poll),
		Precision:      int(p.Precision),
		PrecisionDur:   log2Duration(p.Precision),
		RootDelay:      rootDelay,
		RootDispersion: rootDisp,
		RootDistance:   rootDelay/2 + rootDisp + delay/2,
		ReceiveTime:    t2,
		TransmitTime:   t3,
	}
	if p.Reference != 0 {
		r.ReferenceTime = p.Reference.Time(t4)
	}
	return r, nil
}
