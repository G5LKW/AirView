package avfsrv

import (
	"net/netip"
	"sync"
)

// limiter applies the per-IP and per-subnet limits of AVF.md §8. Raw
// addresses stay in this in-memory table only.
type limiter struct {
	mu     sync.Mutex
	ips    map[netip.Addr]*ipCount
	nets   map[netip.Prefix]int
	total  int
	allow  []netip.Prefix
	policy Policy
}

type ipCount struct {
	unauth, conns, stations int
}

func newLimiter() *limiter {
	return &limiter{ips: map[netip.Addr]*ipCount{}, nets: map[netip.Prefix]int{}}
}

// setPolicy installs new limits (existing connections are kept).
func (l *limiter) setPolicy(p Policy) {
	var allow []netip.Prefix
	for _, s := range p.Limits.AllowCIDRs {
		if pf, err := netip.ParsePrefix(s); err == nil {
			allow = append(allow, pf.Masked())
		} else if a, err := netip.ParseAddr(s); err == nil {
			allow = append(allow, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		}
	}
	l.mu.Lock()
	l.policy, l.allow = p, allow
	l.mu.Unlock()
}

// subnet returns the /24 (IPv4) or /48 (IPv6) of ip.
func subnet(ip netip.Addr) netip.Prefix {
	bits := 48
	if ip.Is4() {
		bits = 24
	}
	p, _ := ip.Prefix(bits)
	return p
}

func (l *limiter) allowed(ip netip.Addr) bool {
	for _, p := range l.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Rejection reasons of acquire.
const (
	rejectNone    = ""
	rejectUnauth  = "unauth_per_ip"
	rejectConns   = "conns_per_ip"
	rejectSubnet  = "conns_per_subnet"
	rejectTotal   = "max_sessions"
	rejectStation = "stations_per_ip"
)

// acquire registers a new, not yet authenticated connection. It returns a
// rejection reason ("" = admitted).
func (l *limiter) acquire(ip netip.Addr, maxTotal int) string {
	ip = ip.Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= maxTotal {
		return rejectTotal
	}
	c := l.ips[ip]
	if c == nil {
		c = &ipCount{}
	}
	lim := l.policy.Limits
	if !l.allowed(ip) {
		switch {
		case c.unauth >= lim.MaxUnauthPerIP:
			return rejectUnauth
		case c.conns >= lim.MaxConnsPerIP:
			return rejectConns
		case l.nets[subnet(ip)] >= lim.MaxConnsPerSubnet:
			return rejectSubnet
		}
	}
	c.unauth++
	c.conns++
	l.ips[ip] = c
	l.nets[subnet(ip)]++
	l.total++
	return rejectNone
}

// authenticated marks a connection of ip as past the handshake.
func (l *limiter) authenticated(ip netip.Addr) {
	ip = ip.Unmap()
	l.mu.Lock()
	if c := l.ips[ip]; c != nil && c.unauth > 0 {
		c.unauth--
	}
	l.mu.Unlock()
}

// release ends a connection of ip (unauth: it never authenticated).
func (l *limiter) release(ip netip.Addr, unauth bool) {
	ip = ip.Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	if c := l.ips[ip]; c != nil {
		c.conns--
		if unauth && c.unauth > 0 {
			c.unauth--
		}
		if c.conns <= 0 && c.stations <= 0 {
			delete(l.ips, ip)
		}
	}
	sn := subnet(ip)
	if l.nets[sn]--; l.nets[sn] <= 0 {
		delete(l.nets, sn)
	}
	l.total--
}

// addStation counts a station attached from ip; false when PerIPStations is
// reached (unless allow-listed).
func (l *limiter) addStation(ip netip.Addr) bool {
	ip = ip.Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.ips[ip]
	if c == nil {
		c = &ipCount{}
		l.ips[ip] = c
	}
	if !l.allowed(ip) && c.stations >= l.policy.Limits.PerIPStations {
		return false
	}
	c.stations++
	return true
}

// dropStation undoes addStation.
func (l *limiter) dropStation(ip netip.Addr) {
	ip = ip.Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	if c := l.ips[ip]; c != nil {
		c.stations--
		if c.conns <= 0 && c.stations <= 0 {
			delete(l.ips, ip)
		}
	}
}
