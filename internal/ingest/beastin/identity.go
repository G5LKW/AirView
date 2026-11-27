package beastin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"net/netip"
	"strings"

	"airview/internal/feed"
	"airview/internal/model"
)

// normalizeUUID turns a UUID as sent in the 0xE4 escape (36 ASCII bytes,
// with or without dashes, padded with 'f', NUL or spaces) or as written in
// the config into the canonical station UUID: the first 28..32 lower-case
// hex digits, as accepted by model.NormalizeUUID. readsb pads an undashed
// 32-digit UUID to 36 characters, so digits beyond the 32nd are padding and
// are dropped; this keeps a 30004 link and a 31090 link of the same
// feeder on one key (DESIGN §3).
func normalizeUUID(s string) (string, bool) {
	u, ok := feed.NormalizeUUID(s)
	if !ok {
		return "", false
	}
	if len(u) > 32 {
		u = u[:32]
	}
	return model.NormalizeUUID(u)
}

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ipKey is the identity key of an unidentified link (DESIGN §3):
// "ip:" + base32(HMAC-SHA256(key, ip16 ‖ port)[:10]), lower case. ip16 is
// the 16-byte form of the unmapped address and port the big-endian listen
// port, so keys of the Beast and mlat-client listeners differ.
func ipKey(key []byte, ip netip.Addr, port uint16) string {
	m := hmac.New(sha256.New, key)
	a := ip.Unmap().As16()
	m.Write(a[:])
	m.Write([]byte{byte(port >> 8), byte(port)})
	sum := m.Sum(nil)
	return "ip:" + strings.ToLower(keyEncoding.EncodeToString(sum[:model.IDBytes]))
}

// subnetOf returns the IPv4 /24 or IPv6 /48 containing ip.
func subnetOf(ip netip.Addr) netip.Prefix {
	ip = ip.Unmap()
	bits := 48
	if ip.Is4() {
		bits = 24
	}
	p, _ := ip.Prefix(bits)
	return p
}
