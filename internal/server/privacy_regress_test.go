package server

import (
	"context"
	"strings"
	"testing"

	"airview/internal/registry"
)

// Privacy regression tests (findings PRIV-1, PRIV-2, PRIV-3).

// moveStation applies the mutation engine.applyReceiverLocation performs for
// a feeder-announced position.
func (f *netFixture) moveStation(t *testing.T, k string, lat, lon float64) {
	t.Helper()
	if _, err := f.reg.Update(context.Background(), f.stations[k].st.Idx, func(s *registry.Station) error {
		s.Located = true
		s.Effective.Lat, s.Effective.Lon = lat, lon
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *netFixture) ownPeers(t *testing.T, s session4, k string) (map[string]mlatPeerDTO, int) {
	t.Helper()
	rec := f.as(s, "GET", "/api/me/stations/"+f.stations[k].st.SID+"/mlat", "")
	if rec.Code != 200 {
		return nil, rec.Code
	}
	var own mlatStationDTO
	jsonOf(t, rec, &own)
	out := map[string]mlatPeerDTO{}
	for _, p := range own.Clock.Peers {
		out[p.Tag] = p
	}
	return out, rec.Code
}

// PRIV-1: moving the own station within a tag period must not change the
// peer bands (they were an exact distance oracle: 22,042 requests located
// every hidden peer to the metre).
func TestPeerBandsFrozenWithinTagPeriod(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.srv.testMLAT = newFakeMLAT(f)
	first, _ := f.ownPeers(t, f.alice, "A")
	if len(first) == 0 {
		t.Fatal("no peers")
	}
	// move right next to each hidden peer, and far away
	for _, k := range []string{"B", "C", "N"} {
		f.moveStation(t, "A", f.stations[k].lat+0.01, f.stations[k].lon)
		got, code := f.ownPeers(t, f.alice, "A")
		if code != 200 {
			t.Fatalf("status %d", code)
		}
		for tag, p := range got {
			if first[tag].Band != p.Band {
				t.Errorf("moved next to %s: band of %s changed %q -> %q", k, tag, first[tag].Band, p.Band)
			}
		}
	}
	// the admin sees the live geometry
	var adm mlatStationDTO
	jsonOf(t, f.as(f.admin, "GET", "/api/admin/stations/"+f.stations["A"].st.SID+"/mlat", ""), &adm)
	near := false
	for _, p := range adm.Clock.Peers {
		if p.SID == f.stations["N"].st.SID && p.Band == "near" {
			near = true
		}
	}
	if !near {
		t.Error("admin view does not follow the live position")
	}
}

// PRIV-1: the owner MLAT view is rate-limited per account.
func TestOwnerMLATRateLimited(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.srv.testMLAT = newFakeMLAT(f)
	limited := false
	for i := 0; i < 40; i++ {
		if _, code := f.ownPeers(t, f.alice, "A"); code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("40 owner requests in a row were not rate-limited")
	}
	// another account and the admin are not affected
	if _, code := f.ownPeers(t, f.bob, "B"); code != 200 {
		t.Fatalf("bob: %d", code)
	}
	for i := 0; i < 40; i++ {
		if rec := f.as(f.admin, "GET", "/api/admin/stations/"+f.stations["A"].st.SID+"/mlat", ""); rec.Code != 200 {
			t.Fatalf("admin request %d: %d", i, rec.Code)
		}
	}
}

// PRIV-2: the owner view carries no per-peer relative frequency and only a
// coarse own drift, so no numeric field joins the tags of one peer across
// viewers or weeks; the admin keeps both.
func TestOwnerPeersCarryNoClockFingerprint(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.withMLAT()
	rec := f.as(f.alice, "GET", "/api/me/stations/"+f.stations["A"].st.SID+"/mlat", "")
	must(t, rec, 200, "owner mlat")
	body := rec.Body.String()
	if strings.Contains(body, "relPpb") {
		t.Errorf("owner view carries relPpb: %.400s", body)
	}
	var own mlatStationDTO
	jsonOf(t, rec, &own)
	if d := own.Clock.DriftPPM; d != float64(int64(d)) {
		t.Errorf("owner drift %v is not whole ppm", d)
	}
	if own.History != nil {
		for _, v := range own.History.DriftPPM {
			if v != nil && *v != float64(int64(*v)) {
				t.Errorf("owner drift history %v is not whole ppm", *v)
			}
		}
	}
	// the same peer seen from two viewers: only count, RMS and band remain
	// (none is a property of the peer's clock alone)
	for _, p := range own.Clock.Peers {
		if p.RelPPB != nil {
			t.Errorf("tag %s has relPpb", p.Tag)
		}
	}
	adm := f.as(f.admin, "GET", "/api/admin/stations/"+f.stations["A"].st.SID+"/mlat", "")
	if !strings.Contains(adm.Body.String(), `"relPpb":12.5`) || !strings.Contains(adm.Body.String(), `"driftPpm":0.0012`) {
		t.Errorf("admin view lost the clock detail: %.400s", adm.Body.String())
	}
	lt := f.as(f.alice, "GET", "/api/me/stations/"+f.stations["A"].st.SID+"/mlat/longterm", "")
	if lt.Code == 200 && strings.Contains(lt.Body.String(), "relPpb") {
		t.Error("long-term owner view carries relPpb")
	}
}

// PRIV-3: with the public gates off, an answer computed for a signed-in
// viewer must not be served to anonymous viewers from the shared cache.
func TestPublicGateNotBypassedByCache(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.withMLAT()
	cfg := f.eng.EffectiveConfig()
	cfg.Aggregator.PublicStats = false
	cfg.Aggregator.PublicMap = false
	if _, _, err := f.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"/api/public/mlat", "/api/public/stats", "/api/public/summary", "/api/public/stations", "/api/public/aircraft"} {
		before := f.anon("GET", u, "192.0.2.41:4000", "").Code
		if before == 200 {
			t.Fatalf("%s: gate off but anonymous 200", u)
		}
		f.as(f.bob, "GET", u, "")
		f.as(f.admin, "GET", u, "")
		if after := f.anon("GET", u, "192.0.2.42:4000", ""); after.Code != before {
			t.Errorf("%s: anonymous %d before, %d after a signed-in request", u, before, after.Code)
		}
	}
}
