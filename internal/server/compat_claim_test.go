package server

import (
	"net/netip"
	"testing"
	"time"
)

// SEC-04: the compat claim's same-address proof needs a long-standing link
// and no concurrent link with the UUID from another client.
func TestCompatClaimLink(t *testing.T) {
	now := time.Now()
	u := "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	home, evil := netip.MustParseAddr("203.0.113.5"), netip.MustParseAddr("198.51.100.66")
	old := LiveLink{Proto: "beast", UUID: u, IP: home, Since: now.Add(-time.Hour)}
	fresh := LiveLink{Proto: "beast", UUID: u, IP: evil, Since: now.Add(-10 * time.Second)}
	if !compatClaimLink([]LiveLink{old}, u, home, now) {
		t.Error("owner at home with an hour-old link refused")
	}
	if compatClaimLink([]LiveLink{fresh}, u, evil, now) {
		t.Error("a just-opened spoofed link is accepted")
	}
	if compatClaimLink([]LiveLink{old, fresh}, u, evil, now) || compatClaimLink([]LiveLink{old, fresh}, u, home, now) {
		t.Error("UUID live from two clients is accepted")
	}
	if compatClaimLink([]LiveLink{old}, u, evil, now) {
		t.Error("other address accepted")
	}
}
