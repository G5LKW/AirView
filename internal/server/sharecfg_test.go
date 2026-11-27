package server

import (
	"encoding/json"
	"testing"
)

// TestShareLinksConfig: aggregator.shareLinks limits reach the routes, and
// disabled answers 404 on the owner and public share routes.
func TestShareLinksConfig(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	sidA := f.stations["A"].st.SID
	cfg := f.eng.EffectiveConfig()
	cfg.Aggregator.ShareLinks.MaxDays = 10
	cfg.Aggregator.ShareLinks.DefaultDays = 3
	cfg.Aggregator.ShareLinks.MaxPerOwner = 1
	cfg.Aggregator.ShareLinks.ExactLowDefault = true
	if _, _, err := f.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var lst struct {
		Max, MaxDays, DefaultDays int
		CoarseLowDefault          bool
	}
	rec := f.as(f.alice, "GET", "/api/me/shares", "")
	must(t, rec, 200, "list")
	if err := json.Unmarshal(rec.Body.Bytes(), &lst); err != nil {
		t.Fatal(err)
	}
	if lst.Max != 1 || lst.MaxDays != 10 || lst.DefaultDays != 3 || lst.CoarseLowDefault {
		t.Fatalf("limits not from the config: %+v", lst)
	}
	must(t, f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidA+`"],"acknowledge":true,"days":11}`), 400, "above maxDays")
	rec = f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidA+`"],"acknowledge":true}`)
	must(t, rec, 200, "create")
	var c shareCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.Link.CoarseLow || c.Link.Expires-c.Link.Created != 3*24*3600*1000 {
		t.Fatalf("defaults not from the config: %+v", c.Link)
	}
	must(t, f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidA+`"],"acknowledge":true}`), 429, "over maxPerOwner")
	must(t, f.anon("GET", "/api/share/"+c.Token, "192.0.2.50:4000", ""), 200, "enabled link")

	cfg = f.eng.EffectiveConfig()
	cfg.Aggregator.ShareLinks.Disabled = true
	if _, _, err := f.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	must(t, f.as(f.alice, "GET", "/api/me/shares", ""), 404, "list when disabled")
	must(t, f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidA+`"],"acknowledge":true}`), 404, "create when disabled")
	must(t, f.anon("GET", "/api/share/"+c.Token, "192.0.2.51:4000", ""), 404, "link when disabled")
	must(t, f.anon("GET", "/api/share/"+c.Token+"/aircraft", "192.0.2.52:4000", ""), 404, "aircraft when disabled")
	must(t, f.as(f.admin, "GET", "/api/admin/shares", ""), 200, "admin list stays")
}
