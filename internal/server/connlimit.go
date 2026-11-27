package server

import (
	"net"
	"net/netip"
	"sync"

	avauth "airview/internal/auth"
	"airview/internal/model"
)

// connLimiter caps open web connections in aggregator mode (SEC-05): a
// global cap (aggregator.limits.maxHttpConns) and a per-client cap
// (maxHttpConnsPerIp, per IPv4 address or IPv6 /64; loopback and trusted
// proxies exempt). A connection over a cap is closed at once, so a
// slow-body or connection flood cannot exhaust the process's descriptors
// and starve the feeder listeners. Standalone mode is not limited.
type connLimiter struct {
	net.Listener
	s *Server

	mu    sync.Mutex
	total int
	perIP map[string]int
}

func (s *Server) limitListener(ln net.Listener) net.Listener {
	return &connLimiter{Listener: ln, s: s, perIP: map[string]int{}}
}

// limits returns the caps (0 = unlimited).
func (l *connLimiter) limits() (total, perIP int) {
	if !l.s.aggregatorMode() || l.s.eng == nil {
		return 0, 0
	}
	lim := l.s.eng.EffectiveConfig().Aggregator.Limits
	d := model.DefaultAggregator().Limits
	total, perIP = lim.MaxHTTPConns, lim.MaxHTTPConnsPerIP
	if total <= 0 {
		total = d.MaxHTTPConns
	}
	if perIP <= 0 {
		perIP = d.MaxHTTPConnsPerIP
	}
	return total, perIP
}

func (l *connLimiter) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if lc, ok := l.admit(c); ok {
			return lc, nil
		}
		_ = c.Close()
	}
}

// admit counts c, or reports false when a cap is reached.
func (l *connLimiter) admit(c net.Conn) (net.Conn, bool) {
	total, perIP := l.limits()
	if total == 0 {
		return c, true
	}
	key := ""
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		a := ap.Addr().Unmap()
		if !a.IsLoopback() && !l.s.proxyTrust().Trusted(a) {
			key = avauth.ClientKey(a)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= total || (key != "" && l.perIP[key] >= perIP) {
		return nil, false
	}
	l.total++
	if key != "" {
		l.perIP[key]++
	}
	return &limitedConn{Conn: c, l: l, key: key}, true
}

func (l *connLimiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if key != "" {
		if l.perIP[key]--; l.perIP[key] <= 0 {
			delete(l.perIP, key)
		}
	}
}

// open returns the number of counted connections (tests).
func (l *connLimiter) open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

type limitedConn struct {
	net.Conn
	l    *connLimiter
	key  string
	once sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.release(c.key) })
	return err
}
