package hub

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"time"
)

// Admin-only (aggregator) mode, DESIGN §5.4.
//
// On an aggregator the data port is for the administrator's own AirView
// instances only. Other software is routinely pointed at port 30154 by
// mistake or by default - most prominently the ADS-B Exchange feed client,
// whose MLAT "results" connector opens an outbound Beast connection to
// localhost:30154 and keeps reconnecting. Such peers do not speak the hub
// protocol, so in admin-only mode the server:
//
//   - always announces "hmac-sha256" (open access is never offered);
//   - closes a connection without any ERR line when its first byte is 0x1A
//     (the Beast frame escape), when no complete AUTH line arrives within
//     Options.ProbeTimeout (2 s) of the connect, or when the first line is
//     not a well-formed AUTH line;
//   - writes at most one log line per remote IP address per
//     Options.QuietLogInterval (1 h) for those closes, reporting how many
//     were suppressed since the previous line;
//   - does not count those closes as failed logins, so a misconfigured
//     program on the same host cannot lock the administrator out.
//
// Peers that do speak the protocol (a well-formed AUTH line) keep receiving
// the usual "ERR auth" / "ERR busy" / "ERR throttled" replies.

// beastEscape is the first byte of every Beast-format frame.
const beastEscape = 0x1A

// maxQuietKeys bounds the per-IP log limiter's memory.
const maxQuietKeys = 4096

// Quiet rejection reasons (log field "reason").
const (
	quietBeast    = "beast stream (first byte 0x1A)"
	quietNoAuth   = "no AUTH line in time"
	quietEOF      = "closed without AUTH"
	quietNotHub   = "not the hub protocol"
	quietNoPasswd = "no data port password set (aggregator mode requires one)"
)

type quietEntry struct {
	last       time.Time // time of the last log line for this key
	suppressed int       // rejections since then that were not logged
}

// probeReason classifies a read error seen before a valid AUTH line.
func probeReason(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return quietNoAuth
	case errors.Is(err, io.EOF):
		return quietEOF
	default:
		return quietNotHub
	}
}

// quietReject counts a quiet close and logs it unless this key was logged
// within the last QuietLogInterval.
func (s *Server) quietReject(key, ip, addr, reason string, now time.Time) {
	s.quietRejects.Add(1)
	s.mu.Lock()
	e := s.quiet[key]
	if e != nil && now.Sub(e.last) < s.opts.QuietLogInterval {
		e.suppressed++
		s.mu.Unlock()
		return
	}
	suppressed := 0
	if e == nil {
		if len(s.quiet) >= maxQuietKeys {
			s.pruneQuietLocked(now)
			for k := range s.quiet { // still full: evict an arbitrary entry
				if len(s.quiet) < maxQuietKeys {
					break
				}
				delete(s.quiet, k)
			}
		}
		e = &quietEntry{}
		s.quiet[key] = e
	} else {
		suppressed = e.suppressed
	}
	e.last = now
	e.suppressed = 0
	s.mu.Unlock()

	args := append(s.peerArgs(addr), "reason", reason,
		"next_log_after", s.opts.QuietLogInterval.String())
	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	if reason == quietNoPasswd {
		s.log.Warn("data port login refused (admin-only mode)", args...)
		return
	}
	s.log.Info("data port: closed non-hub client (admin-only mode)", args...)
}

// pruneQuietLocked forgets limiter entries whose log interval has passed.
func (s *Server) pruneQuietLocked(now time.Time) {
	for k, e := range s.quiet {
		if now.Sub(e.last) >= s.opts.QuietLogInterval {
			delete(s.quiet, k)
		}
	}
}

// peerArgs returns the log attributes naming a client. On an aggregator
// (admin-only mode) raw addresses are not logged (DESIGN §8.4, SEC-10):
// only the network (IPv4 /24, IPv6 /48), which is enough to spot a
// scanner's origin without identifying a person. Standalone mode logs the
// address as before.
func (s *Server) peerArgs(addr string) []any {
	return peerArgsFor(s.AdminOnly(), addr)
}

// peerArgsFor is peerArgs for callers holding s.mu.
func peerArgsFor(adminOnly bool, addr string) []any {
	if !adminOnly {
		return []any{"addr", addr}
	}
	return []any{"net", maskedNet(addr)}
}

// maskedNet returns the /24 (IPv4) or /48 (IPv6) of a host:port or address.
func maskedNet(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		h = addr
	}
	ip, err := netip.ParseAddr(h)
	if err != nil {
		return "?"
	}
	ip = ip.Unmap()
	bits := 48
	if ip.Is4() {
		bits = 24
	}
	p, _ := ip.Prefix(bits)
	return p.String()
}

// SetAdminOnly switches admin-only (aggregator) mode on or off at run time.
// Switching it on while no password is set disconnects every client, since
// open access is not allowed in that mode.
func (s *Server) SetAdminOnly(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adminOnly == on {
		return
	}
	s.adminOnly = on
	if on && s.password == "" {
		for c := range s.clients {
			s.removeLocked(c, "admin-only mode requires a password")
		}
	}
}

// AdminOnly reports whether admin-only (aggregator) mode is active.
func (s *Server) AdminOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adminOnly
}

// QuietRejections returns how many connections were closed quietly in
// admin-only mode (non-hub peers and refused logins without a password).
func (s *Server) QuietRejections() uint64 { return s.quietRejects.Load() }
