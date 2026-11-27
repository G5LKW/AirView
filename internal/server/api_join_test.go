package server

import (
	"strings"
	"testing"
)

// TestPublicJoin: the feeder page's facts in aggregator mode, for anonymous
// viewers, without any station data and without echoing request headers.
func TestPublicJoin(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	rec := f.anon("GET", "/api/public/join", "203.0.113.200:4000", "")
	must(t, rec, 200, "join")
	var d joinDTO
	jsonOf(t, rec, &d)
	a := f.srv.eng.EffectiveConfig().Aggregator
	if !d.Network || d.AVFPort != a.AVFPort || d.BeastPort != a.BeastPort || d.Admission != a.Admission ||
		d.BackupDays != a.Retention.BackupDays || d.Registration == "" {
		t.Fatalf("join %+v (config %+v)", d, a)
	}
	if a.MLAT.Enabled && d.MLATPort != a.MLATPort || !a.MLAT.Enabled && d.MLATPort != 0 {
		t.Fatalf("mlatPort %d (engine on %v, port %d)", d.MLATPort, a.MLAT.Enabled, a.MLATPort)
	}
	if d.Host != strings.TrimSpace(a.PublicHost) {
		t.Fatalf("host %q echoed instead of aggregator.publicHost %q", d.Host, a.PublicHost)
	}
	body := rec.Body.String()
	for _, k := range []string{"203.0.113", `"sid"`, `"lat"`, `"uuid"`} {
		if strings.Contains(strings.ToLower(body), k) {
			t.Fatalf("join answer contains %q: %s", k, body)
		}
	}
}
