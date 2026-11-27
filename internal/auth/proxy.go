package auth

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ProxyTrust decides whose forwarding headers are believed.
//
// Rules (DESIGN §7 "Auth fixes in aggregator mode"):
//
//   - X-Forwarded-For and X-Forwarded-Proto are honoured only when the TCP
//     peer is inside one of the configured CIDRs. Everybody else's headers
//     are ignored and the peer address is the client.
//   - The client is found by walking X-Forwarded-For from the right (the
//     entry appended by the nearest proxy) and skipping entries that are
//     themselves trusted proxies; the first untrusted entry is the client.
//     Entries further left were written by the client and can be forged.
//   - A request that carries any forwarding header (X-Forwarded-For,
//     Forwarded, X-Real-IP) is "proxied". A proxied request is never
//     treated as coming from this machine, even when the TCP peer is
//     loopback: a reverse proxy on the same host would otherwise turn every
//     internet client into a local one.
//
// The zero value (and nil) trusts nobody.
type ProxyTrust struct {
	nets []netip.Prefix
}

// ParseTrustedProxies parses CIDRs or single addresses ("10.0.0.0/8",
// "127.0.0.1", "::1/128"). Empty entries are skipped; any invalid entry is
// an error naming it.
func ParseTrustedProxies(list []string) (*ProxyTrust, error) {
	p := &ProxyTrust{}
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		var pr netip.Prefix
		var err error
		if strings.Contains(s, "/") {
			pr, err = netip.ParsePrefix(s)
		} else {
			var a netip.Addr
			a, err = netip.ParseAddr(s)
			if err == nil {
				a = a.Unmap()
				pr = netip.PrefixFrom(a, a.BitLen())
			}
		}
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: not an address or CIDR", s)
		}
		if pr.Addr().Is4In6() {
			pr = netip.PrefixFrom(pr.Addr().Unmap(), max(pr.Bits()-96, 0))
		}
		p.nets = append(p.nets, pr.Masked())
	}
	return p, nil
}

// MustTrustedProxies is ParseTrustedProxies that drops invalid entries
// (config validation already reported them).
func MustTrustedProxies(list []string) *ProxyTrust {
	p := &ProxyTrust{}
	for _, s := range list {
		if one, err := ParseTrustedProxies([]string{s}); err == nil {
			p.nets = append(p.nets, one.nets...)
		}
	}
	return p
}

// Empty reports whether no proxy is trusted.
func (p *ProxyTrust) Empty() bool { return p == nil || len(p.nets) == 0 }

// Trusted reports whether a is a trusted proxy address.
func (p *ProxyTrust) Trusted(a netip.Addr) bool {
	if p == nil || !a.IsValid() {
		return false
	}
	a = a.Unmap()
	for _, n := range p.nets {
		if n.Contains(a) {
			return true
		}
	}
	return false
}

// Client is the resolved origin of a request.
type Client struct {
	// IP is the client address: the peer, or the address the trusted
	// proxy chain reported. Invalid only when RemoteAddr is unparsable.
	IP netip.Addr
	// Peer is the TCP peer address (RemoteAddr).
	Peer netip.Addr
	// Proxied is true when the request carries any forwarding header,
	// whether or not the peer is trusted.
	Proxied bool
	// ViaTrustedProxy is true when IP was taken from X-Forwarded-For.
	ViaTrustedProxy bool
	// Secure is true for TLS on the socket, or X-Forwarded-Proto https
	// set by a trusted proxy.
	Secure bool
}

// String returns the client IP as text ("" if unknown).
func (c Client) String() string {
	if !c.IP.IsValid() {
		return ""
	}
	return c.IP.String()
}

// Local reports whether the request may be treated as coming from this
// machine: the peer is loopback and nothing proxied it.
func (c Client) Local() bool {
	return c.Peer.IsValid() && c.Peer.IsLoopback() && !c.Proxied
}

// PeerAddr parses an http.Request.RemoteAddr ("ip:port", "[v6]:port" or a
// bare address).
func PeerAddr(remote string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return ap.Addr().Unmap()
	}
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if i := strings.IndexByte(host, '%'); i >= 0 { // zone
		host = host[:i]
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap()
	}
	return netip.Addr{}
}

// HasForwardingHeaders reports whether r carries a header that proxies use
// to name the original client.
func HasForwardingHeaders(r *http.Request) bool {
	h := r.Header
	return len(h.Values("X-Forwarded-For")) > 0 || len(h.Values("Forwarded")) > 0 ||
		len(h.Values("X-Real-Ip")) > 0 || len(h.Values("X-Forwarded-Host")) > 0
}

// parseXFFEntry parses one X-Forwarded-For element ("1.2.3.4",
// "1.2.3.4:5678", "[2001:db8::1]:443", "2001:db8::1").
func parseXFFEntry(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

// maxXFFEntries bounds the work spent on one header (hostile chains).
const maxXFFEntries = 32

// Resolve determines the client of r.
func (p *ProxyTrust) Resolve(r *http.Request) Client {
	c := Client{Peer: PeerAddr(r.RemoteAddr), Proxied: HasForwardingHeaders(r), Secure: r.TLS != nil}
	c.IP = c.Peer
	if !p.Trusted(c.Peer) {
		return c
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	if len(hops) > maxXFFEntries {
		hops = hops[len(hops)-maxXFFEntries:]
	}
	if len(hops) > 0 {
		client := c.Peer
		for i := len(hops) - 1; i >= 0; i-- {
			a, ok := parseXFFEntry(hops[i])
			if !ok {
				break // garbage: keep the last hop we could trust
			}
			client = a
			if !p.Trusted(a) {
				break
			}
		}
		if client != c.Peer {
			c.IP, c.ViaTrustedProxy = client, true
		}
	}
	if !c.Secure {
		// The nearest proxy appends its value last.
		vals := r.Header.Values("X-Forwarded-Proto")
		if len(vals) > 0 {
			parts := strings.Split(vals[len(vals)-1], ",")
			c.Secure = strings.EqualFold(strings.TrimSpace(parts[len(parts)-1]), "https")
		}
	}
	return c
}

// ClientKey is the rate-limit identity of an address: the IPv4 address, or
// the IPv6 /64 (one subscriber typically holds a whole /64, so per-address
// keys would give an attacker 2^64 fresh identities). "" for an invalid
// address.
func ClientKey(a netip.Addr) string {
	a = a.Unmap()
	if !a.IsValid() {
		return ""
	}
	if a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64)
	return p.String()
}

// ClientKeyString is ClientKey for an address in text form; text that is
// not an address is returned unchanged.
func ClientKeyString(s string) string {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return s
	}
	return ClientKey(a)
}

// SameClient reports whether a and b are the same client for feed-check
// and compat-claim purposes (DESIGN §7): equal IPv4 addresses, or IPv6
// addresses in the same /64.
func SameClient(a, b netip.Addr) bool {
	a, b = a.Unmap(), b.Unmap()
	if !a.IsValid() || !b.IsValid() || a.Is4() != b.Is4() {
		return false
	}
	if a.Is4() {
		return a == b
	}
	pa, _ := a.Prefix(64)
	return pa.Contains(b)
}
