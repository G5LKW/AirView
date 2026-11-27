package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/quality"
	"airview/internal/model"
)

// Phase 4-5 web/API routes (WP-W45-web): share links (WP-H2), ADS-B
// verification alerts, the CRLB coverage-improvement analysis and the
// sortable admin station table.

func init() {
	for _, k := range []string{"GET /api/me/shares", "POST /api/me/shares", "DELETE /api/me/shares/{id}",
		"GET /api/admin/shares", "DELETE /api/admin/shares/{id}",
		"GET /api/share/{token}", "GET /api/share/{token}/aircraft", "GET /api/share/{token}/stats"} {
		specialTests[k] = "TestShareLinks, TestGoldenPublicCrawl, TestGoldenOwnerCrawl"
	}
	specialTests["GET /api/admin/mlat/verification"] = "TestVerificationRoute"
	specialTests["GET /api/admin/mlat/crlb/improve"] = "TestCRLBImproveRoute, TestImproveAnalysis"
}

type shareCreated struct {
	Link  shareLinkDTO `json:"link"`
	Token string       `json:"token"`
	Path  string       `json:"path"`
}

func TestShareLinks(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.waitTraffic(2)
	sidA, sidB := f.stations["A"].st.SID, f.stations["B"].st.SID
	fx := f.publicFixture()

	// The location warning must be acknowledged.
	rec := f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidA+`"],"label":"For Dad"}`)
	must(t, rec, 400, "share without acknowledge")
	if !strings.Contains(rec.Body.String(), "roughly where your antenna is") {
		t.Fatalf("no location warning: %s", rec.Body)
	}
	// Foreign and unknown stations answer alike.
	must(t, f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidB+`"],"acknowledge":true}`), 404, "foreign station")
	must(t, f.as(f.alice, "POST", "/api/me/shares", `{"stations":["nope"],"acknowledge":true}`), 404, "unknown station")
	must(t, f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidA+`"],"acknowledge":true,"days":400}`), 400, "too long")
	must(t, f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidA+`"],"acknowledge":true,"scope":"all"}`), 400, "bad scope")
	must(t, f.anon("POST", "/api/me/shares", "192.0.2.10:4000", `{"stations":["`+sidA+`"],"acknowledge":true}`), 401, "anonymous create")

	rec = f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidA+`"],"label":"For Dad","scope":"map+stats","days":7,"acknowledge":true}`)
	must(t, rec, 200, "create")
	var c shareCreated
	jsonOf(t, rec, &c)
	if len(c.Token) != 32 || c.Path != "/#/share/"+c.Token || c.Link.State != "active" || !c.Link.CoarseLow || len(c.Link.Stations) != 1 || !c.Link.Stations[0].Owned {
		t.Fatalf("created: %+v", c)
	}
	if d := time.Until(time.UnixMilli(c.Link.Expires)); d < 6*24*time.Hour || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("expiry in %v", d)
	}
	// The secret is never listed again; the stored file holds only its hash.
	rec = f.as(f.alice, "GET", "/api/me/shares", "")
	must(t, rec, 200, "list")
	if strings.Contains(rec.Body.String(), c.Token) || !strings.Contains(rec.Body.String(), c.Link.ID) {
		t.Fatalf("list: %s", rec.Body)
	}
	ls, _ := f.srv.shareStore().list(nil)
	if len(ls) != 1 || ls[0].Hash != hashShareSecret(c.Token) {
		t.Fatalf("stored: %+v", ls)
	}
	// Bob sees nothing of it and cannot revoke it.
	rec = f.as(f.bob, "GET", "/api/me/shares", "")
	if strings.Contains(rec.Body.String(), c.Link.ID) {
		t.Fatalf("bob sees alice's link: %s", rec.Body)
	}
	must(t, f.as(f.bob, "DELETE", "/api/me/shares/"+c.Link.ID, ""), 404, "bob revokes")

	// Anyone with the link: meta, aircraft, stats. Nothing locating, no SID.
	for _, u := range []string{"/api/share/" + c.Token, "/api/share/" + c.Token + "/aircraft", "/api/share/" + c.Token + "/stats"} {
		rec := f.anon("GET", u, "192.0.2.30:4000", "")
		must(t, rec, 200, u)
		scanOrFail(t, fx, "share "+u, rec.Body.Bytes())
		for _, bad := range []string{sidA, `"sid"`, `"lat":-41`, `"seenBy"`, `"stations"`} {
			if strings.Contains(rec.Body.String(), bad) {
				t.Errorf("%s contains %q: %.300s", u, bad, rec.Body.String())
			}
		}
	}
	var st struct {
		Stations []shareStationDTO `json:"entries"`
	}
	jsonOf(t, f.anon("GET", "/api/share/"+c.Token+"/stats", "192.0.2.31:4000", ""), &st)
	// A publishes its name (showName), so the link names it; without that
	// the link says "Station 1".
	if len(st.Stations) != 1 || st.Stations[0].Name != "Alice Home" || st.Stations[0].Stats == nil {
		t.Fatalf("stats: %+v", st)
	}
	if _, err := f.reg.SetPrivacy(context.Background(), f.stations["A"].st.Idx, model.PinHidden, false, true); err != nil {
		t.Fatal(err)
	}
	jsonOf(t, f.anon("GET", "/api/share/"+c.Token+"/stats", "192.0.2.31:4001", ""), &st)
	if len(st.Stations) != 1 || st.Stations[0].Name != "Station 1" {
		t.Fatalf("stats without showName: %+v", st)
	}
	// Junk and near-miss tokens: the same 404.
	bad := []byte(c.Token)
	bad[0] ^= 1
	for _, tok := range []string{"x", string(bad), strings.Repeat("A", 32)} {
		must(t, f.anon("GET", "/api/share/"+tok, "192.0.2.32:4000", ""), 404, "bad token "+tok)
	}

	// A "map" link has no statistics.
	rec = f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidA+`"],"acknowledge":true,"coarseLow":false}`)
	must(t, rec, 200, "create map")
	var c2 shareCreated
	jsonOf(t, rec, &c2)
	if c2.Link.CoarseLow || c2.Link.Scope != "map" || c2.Link.Expires-c2.Link.Created != int64(shareDefaultDays)*24*3600*1000 {
		t.Fatalf("map link: %+v", c2.Link)
	}
	must(t, f.anon("GET", "/api/share/"+c2.Token+"/stats", "192.0.2.33:4000", ""), 404, "map link stats")

	// The admin lists both with the owner, filters and revokes.
	rec = f.as(f.admin, "GET", "/api/admin/shares?q=dad", "")
	must(t, rec, 200, "admin list")
	var al struct {
		Links  []shareLinkDTO `json:"links"`
		Total  int            `json:"total"`
		Counts map[string]int `json:"counts"`
	}
	jsonOf(t, rec, &al)
	if al.Total != 1 || len(al.Links) != 1 || al.Links[0].Owner != "alice" || al.Counts["active"] != 2 {
		t.Fatalf("admin list: %+v", al)
	}
	must(t, f.as(f.alice, "GET", "/api/admin/shares", ""), 403, "owner on admin list")
	must(t, f.as(f.admin, "GET", "/api/admin/shares?state=bogus", ""), 400, "bad state")
	must(t, f.as(f.admin, "DELETE", "/api/admin/shares/"+c2.Link.ID, ""), 200, "admin revoke")
	must(t, f.anon("GET", "/api/share/"+c2.Token, "192.0.2.34:4000", ""), 404, "revoked link")

	// The owner revokes; the link dies at once and stays listed as revoked.
	must(t, f.as(f.alice, "DELETE", "/api/me/shares/"+c.Link.ID, ""), 200, "owner revoke")
	must(t, f.anon("GET", "/api/share/"+c.Token+"/aircraft", "192.0.2.35:4000", ""), 404, "revoked aircraft")
	rec = f.as(f.alice, "GET", "/api/me/shares", "")
	if !strings.Contains(rec.Body.String(), `"state":"revoked"`) {
		t.Fatalf("revoked not listed: %s", rec.Body)
	}
	// Audit trail.
	rec = f.as(f.admin, "GET", "/api/admin/audit", "")
	for _, a := range []string{"share.create", "share.revoke"} {
		if !strings.Contains(rec.Body.String(), a) {
			t.Errorf("audit lacks %s", a)
		}
	}

	// A link stops working when the station leaves the account.
	rec = f.as(f.alice, "POST", "/api/me/shares", `{"stations":["`+sidA+`"],"acknowledge":true}`)
	var c3 shareCreated
	jsonOf(t, rec, &c3)
	must(t, f.anon("GET", "/api/share/"+c3.Token, "192.0.2.36:4000", ""), 200, "before unclaim")
	must(t, f.as(f.alice, "POST", "/api/me/stations/"+sidA+"/unclaim", ""), 200, "unclaim")
	must(t, f.anon("GET", "/api/share/"+c3.Token, "192.0.2.37:4000", ""), 404, "after unclaim")
}

func TestShareStoreLimitsAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, shareFile)
	st := newShareStore(path)
	now := time.Now()
	var secrets []string
	for i := 0; i < shareMaxPerOwner; i++ {
		_, sec, err := st.create(shareLink{OwnerID: 7, SIDs: []string{"s1"}, Scope: shareScopeMap, Expires: now.Add(time.Hour).UnixMilli()}, 0)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, sec)
	}
	if _, _, err := st.create(shareLink{OwnerID: 7, SIDs: []string{"s1"}, Scope: shareScopeMap, Expires: now.Add(time.Hour).UnixMilli()}, 0); err != errShareLimit {
		t.Fatalf("limit: %v", err)
	}
	// Another owner is not affected.
	if _, _, err := st.create(shareLink{OwnerID: 8, SIDs: []string{"s2"}, Scope: shareScopeMap, Expires: now.Add(time.Hour).UnixMilli()}, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.resolve(secrets[3]); !ok {
		t.Fatal("resolve")
	}
	st.flush()
	// A fresh store reads the file back; the use counter survived.
	st2 := newShareStore(path)
	l, ok := st2.resolve(secrets[3])
	if !ok || l.Uses != 2 || l.OwnerID != 7 {
		t.Fatalf("reloaded: %+v %v", l, ok)
	}
	// Expired links do not resolve and are pruned after the keep period.
	st2.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, ok := st2.resolve(secrets[0]); ok {
		t.Fatal("expired link resolved")
	}
	st2.now = func() time.Time { return now.Add(shareKeepRevoked + 2*time.Hour) }
	ls, _ := st2.list(nil)
	if len(ls) != 0 {
		t.Fatalf("not pruned: %d", len(ls))
	}
	if n := newShareStore(path); n != nil {
		if ls, _ := n.list(nil); len(ls) != 0 {
			t.Fatalf("prune not saved: %d", len(ls))
		}
	}
	// Owner revocation (account deletion).
	st3 := newShareStore("")
	_, sec, _ := st3.create(shareLink{OwnerID: 9, SIDs: []string{"x"}, Scope: shareScopeMap, Expires: now.Add(time.Hour).UnixMilli()}, 0)
	if n := st3.revokeOwner(9, "test"); n != 1 {
		t.Fatalf("revokeOwner %d", n)
	}
	if _, ok := st3.resolve(sec); ok {
		t.Fatal("revoked owner link resolved")
	}
	// A damaged file is reported, not overwritten.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st4 := newShareStore(bad)
	if _, _, err := st4.create(shareLink{OwnerID: 1, SIDs: []string{"x"}, Expires: now.Add(time.Hour).UnixMilli()}, 0); err == nil {
		t.Fatal("damaged file overwritten")
	}
}

// suspectMLAT is the console fake with a settable suspect set.
type suspectMLAT struct {
	*fakeMLAT
	mu  sync.Mutex
	sus map[uint32]bool
}

func (m *suspectMLAT) Suspect(key uint32, _ int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sus[key]
}

func TestVerificationRoute(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.waitTraffic(2)
	must(t, f.as(f.admin, "GET", "/api/admin/mlat/verification", ""), 200, "off")
	fake := &suspectMLAT{fakeMLAT: newFakeMLAT(f), sus: map[uint32]bool{}}
	fake.stats.ADSBConfirmed, fake.stats.ADSBContradicted, fake.stats.ADSBSuspects = 990, 10, 1
	f.srv.testMLAT = fake
	var icao string
	for _, a := range f.eng.Tracker().Snapshot().Aircraft {
		if a.Lat != nil && !strings.HasPrefix(a.ICAO, "~") {
			icao = a.ICAO
			break
		}
	}
	if icao == "" {
		t.Fatal("no live aircraft")
	}
	key, _ := parseICAOParam(icao)
	fake.mu.Lock()
	fake.sus[key] = true
	fake.mu.Unlock()
	type reply struct {
		Running  bool               `json:"running"`
		Counters map[string]float64 `json:"counters"`
		Current  []verifyAircraft   `json:"current"`
		Events   []verifyEvent      `json:"events"`
	}
	var r reply
	jsonOf(t, f.as(f.admin, "GET", "/api/admin/mlat/verification", ""), &r)
	if !r.Running || len(r.Current) != 1 || r.Current[0].ICAO != icao || r.Current[0].Lat == nil || len(r.Events) != 1 || r.Events[0].Kind != "suspect" ||
		r.Counters["contradicted"] != 10 || r.Counters["contradictedShare"] != 0.01 {
		t.Fatalf("suspect: %+v", r)
	}
	fake.mu.Lock()
	delete(fake.sus, key)
	fake.mu.Unlock()
	f.srv.scanVerify(time.Now().Add(time.Minute), true)
	jsonOf(t, f.as(f.admin, "GET", "/api/admin/mlat/verification", ""), &r)
	if len(r.Current) != 0 || len(r.Events) != 2 || r.Events[0].Kind != "cleared" || r.Events[0].ICAO != icao {
		t.Fatalf("cleared: %+v", r)
	}
	must(t, f.as(f.alice, "GET", "/api/admin/mlat/verification", ""), 403, "owner")
	if rec := f.anon("GET", "/api/admin/mlat/verification", "192.0.2.9:1", ""); rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
}

// TestImproveAnalysis checks the coverage-improvement ranking against the
// geometry: four GPS stations in a 1Â° square; the best new site must lie
// outside the square (where Ïƒ_h is poor or missing), every reported site
// must improve something, and the score ordering must hold.
func TestImproveAnalysis(t *testing.T) {
	var sts []quality.CRLBStation
	for _, p := range [][2]float64{{50, 8}, {50, 9}, {51, 8}, {51, 9}} {
		sts = append(sts, quality.CRLBStation{Pos: geo.ToECEF(p[0], p[1], 200), SigmaS: 30e-9})
	}
	t0 := time.Now()
	res := improveAnalysis(context.Background(), sts, 3048, 0.5, 30, 4)
	el := time.Since(t0)
	if res.Reason != "" || len(res.Sites) == 0 || res.Candidates == 0 || res.Baseline.Cells == 0 {
		t.Fatalf("result: %+v", res)
	}
	t.Logf("%d candidates, %d sites, baseline %d cells P50 %.0f m, best %+v (%v)", res.Candidates, len(res.Sites), res.Baseline.Cells, res.Baseline.P50M, res.Sites[0], el)
	best := res.Sites[0]
	if best.Lat > 50 && best.Lat < 51 && best.Lon > 8 && best.Lon < 9 {
		t.Errorf("best site inside the square: %+v", best)
	}
	if best.Score != 1 || best.Gain <= 0 || best.NewCells+best.Better == 0 {
		t.Errorf("best site: %+v", best)
	}
	for i := 1; i < len(res.Sites); i++ {
		if res.Sites[i].Gain > res.Sites[i-1].Gain {
			t.Errorf("order: %+v after %+v", res.Sites[i], res.Sites[i-1])
		}
	}
	// Sites inside the square (already well covered) improve far less than
	// the best one.
	inner := 0
	for _, c := range res.Cells {
		if c.Lat > 50 && c.Lat < 51 && c.Lon > 8 && c.Lon < 9 {
			inner++
			if c.Score > 0.5 {
				t.Errorf("inner cell scores high: %+v", c)
			}
		}
	}
	if inner == 0 {
		t.Errorf("no inner candidate cells")
	}
	// Too few stations.
	if r := improveAnalysis(context.Background(), sts[:2], 3048, 0.5, 30, 1); r.Reason == "" || len(r.Sites) != 0 {
		t.Errorf("2 stations: %+v", r)
	}
	if el > 20*time.Second {
		t.Errorf("analysis took %v", el)
	}
}

// BenchmarkImprove100 is the analysis for 100 stations on a 0.25Â° grid
// (budget: a few seconds; the result is cached for 10 minutes).
func BenchmarkImprove100(b *testing.B) {
	var sts []quality.CRLBStation
	for i := 0; i < 100; i++ {
		lat, lon := 48+float64(i%10)*0.4, 6+float64(i/10)*0.5
		sts = append(sts, quality.CRLBStation{Pos: geo.ToECEF(lat, lon, 200), SigmaS: 100e-9})
	}
	for b.Loop() {
		improveAnalysis(context.Background(), sts, 3048, 0.5, 30, 8)
	}
}

func TestCRLBImproveRoute(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	must(t, f.as(f.admin, "GET", "/api/admin/mlat/crlb/improve", ""), 200, "off")
	f.srv.testMLAT = newFakeMLAT(f)
	rec := f.as(f.admin, "GET", "/api/admin/mlat/crlb/improve?alt=3048&step=1", "")
	must(t, rec, 200, "improve")
	var r improveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Running || r.AltM != 3048 || r.StepDeg < 1 || r.Sites == nil || r.Cells == nil {
		t.Fatalf("improve: %+v", r)
	}
	must(t, f.as(f.admin, "GET", "/api/admin/mlat/crlb/improve?alt=10", ""), 400, "bad alt")
	must(t, f.as(f.admin, "GET", "/api/admin/mlat/crlb/improve?sigmaNs=0", ""), 400, "bad sigma")
	must(t, f.as(f.alice, "GET", "/api/admin/mlat/crlb/improve", ""), 403, "owner")
}

func TestAdminStationsSort(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	var r adminStationsReply
	for _, dir := range []string{"asc", "desc"} {
		jsonOf(t, f.as(f.admin, "GET", "/api/admin/stations?sort=name&dir="+dir+"&limit=2", ""), &r)
		if r.Total < 4 || len(r.Stations) != 2 {
			t.Fatalf("paging: %+v", r)
		}
		var all adminStationsReply
		jsonOf(t, f.as(f.admin, "GET", "/api/admin/stations?sort=name&dir="+dir, ""), &all)
		for i := 1; i < len(all.Stations); i++ {
			a, b := strings.ToLower(all.Stations[i-1].Name), strings.ToLower(all.Stations[i].Name)
			if dir == "asc" && a > b || dir == "desc" && a < b {
				t.Fatalf("%s order: %q before %q", dir, a, b)
			}
		}
		// The second page continues the first.
		var p2 adminStationsReply
		jsonOf(t, f.as(f.admin, "GET", "/api/admin/stations?sort=name&dir="+dir+"&limit=2&offset=2", ""), &p2)
		if len(p2.Stations) == 0 || p2.Stations[0].SID != all.Stations[2].SID {
			t.Fatalf("page 2: %+v vs %+v", p2.Stations, all.Stations)
		}
	}
	must(t, f.as(f.admin, "GET", "/api/admin/stations?sort=ip", ""), 400, "bad sort")
}
