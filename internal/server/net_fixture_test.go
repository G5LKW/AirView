package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	avauth "airview/internal/auth"
	"airview/internal/model"
	"airview/internal/privacy"
	"airview/internal/registry"
	"airview/internal/sim"
)

// The aggregator fixture of the network and golden privacy tests: a demo
// engine (whose tracker is NOT in network mode, so its live picture still
// carries every receiver field - the worst case for the projections), the
// aggregator rules, a registry with stations at known exact positions,
// live links from known addresses, two owners and an admin.

type fixStation struct {
	name     string
	uuid     string
	lat, lon float64
	pin      string
	ip       string
	st       registry.Station
}

type netFixture struct {
	*rig
	reg      *registry.Registry
	stations map[string]*fixStation // "A" (alice, hidden), "B" (bob, region pin), "C" (unclaimed, hidden), "N" (native, unclaimed)
	links    *fakeLinks
	alice    session4
	bob      session4
	admin    session4
	gAdmin   avauth.TOTP
}

// session4 is a logged-in client: cookie, CSRF token, account ID and the
// address it calls from.
type session4 struct {
	cookie, csrf string
	id           int64
	addr         string
}

func (s session4) hdr() []string {
	return []string{"Cookie", s.cookie, avauth.CSRFHeader, s.csrf}
}

type fakeLinks struct {
	mu sync.Mutex
	ls []LiveLink
}

func (f *fakeLinks) LiveLinks() []LiveLink {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LiveLink(nil), f.ls...)
}

func newNetFixture(t *testing.T, o rigOptions) *netFixture {
	t.Helper()
	r := newRig(t, o)
	r.srv.auth.net.testMode = model.ModeAggregator
	r.srv.testRate = &rateSpec{} // the crawls are not rate-limited; see TestPublicRateLimit
	reg, err := registry.OpenDir(t.TempDir(), registry.Options{Policy: registry.Policy{Admission: model.AdmissionOpen}, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	r.srv.SetAccountService(avauth.NewService(reg.Accounts(), reg.Sessions(), avauth.Options{PasswordIterations: 1000}))
	f := &netFixture{rig: r, reg: reg, links: &fakeLinks{}, stations: map[string]*fixStation{
		"A": {name: "Alice Home", uuid: "a1b2c3d4e5f60718293a4b5c6d7e8f90", lat: -41.28713, lon: 174.77621, pin: model.PinHidden, ip: "203.0.113.5"},
		"B": {name: "Bob Hill", uuid: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", lat: -33.86882, lon: 151.20929, pin: model.PinRegion, ip: "2001:db8:aa:bb::17"},
		"C": {name: "Carol Shed", uuid: "77665544332211009988aabbccddeeff", lat: 64.13548, lon: -21.89541, pin: model.PinHidden, ip: "198.51.100.9"},
		"N": {name: "Native Pi", lat: -12.46344, lon: 130.84565, pin: model.PinHidden, ip: "198.51.100.77"},
	}}
	ctx := context.Background()
	for _, k := range []string{"A", "B", "C", "N"} {
		fs := f.stations[k]
		req := registry.AdmitRequest{Kind: model.StationKindBeast, UUID: fs.uuid, Name: fs.name}
		if k == "N" {
			seed := sha256.Sum256([]byte("server-test-feeder"))
			req = registry.AdmitRequest{Kind: model.StationKindAVF, FeederPub: ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey), Name: fs.name}
		}
		res, err := reg.Admit(ctx, req)
		if err != nil || res.Status != registry.StatusApproved {
			t.Fatalf("admit %s: %v %+v", k, err, res)
		}
		idx := res.Station.Idx
		if _, err := reg.SetEffective(ctx, idx, model.StationGeo{Lat: fs.lat, Lon: fs.lon, HAE: 123.4, Source: model.LocSrcManual}, true, model.LocDeclared); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.SetDeclared(ctx, idx, registry.Position{Lat: fs.lat, Lon: fs.lon, Alt: 80, Valid: true}, model.AltRefMSL); err != nil {
			t.Fatal(err)
		}
		if fs.st, err = reg.SetPrivacy(ctx, idx, fs.pin, k == "A", k == "A"); err != nil {
			t.Fatal(err)
		}
		reg.Seen(idx, time.Now())
		proto := "beast"
		if k == "N" {
			proto = "avf"
		}
		f.links.ls = append(f.links.ls, LiveLink{Proto: proto, Station: idx, UUID: fs.uuid, IP: netip.MustParseAddr(fs.ip),
			Since: time.Now().Add(-time.Hour), MsgRate: 812.5, KBps: 14.2, RTTMs: 31, ClockClass: "radarcape-gps"})
	}
	r.srv.SetNetwork(Network{Registry: reg, Links: []LinkSource{f.links}})

	h := r.srv.Handler()
	f.gAdmin = makeAdmin(t, r, "root", "admin-password-1")
	rec := serve(h, "POST", "/api/login", "192.0.2.200:4000", `{"username":"root","password":"admin-password-1","code":"`+f.gAdmin.Code(time.Now())+`"}`)
	if rec.Code != 200 {
		t.Fatalf("admin login: %d %s", rec.Code, rec.Body.String())
	}
	f.admin = session4{cookie: cookieOf(rec), csrf: decodeInfo(t, rec).CSRFToken, addr: "192.0.2.200:4000"}
	f.alice = f.register(t, "alice", "203.0.113.5:5000")
	f.bob = f.register(t, "bob", "[2001:db8:aa:bb::99]:5000")
	for k, owner := range map[string]int64{"A": f.alice.id, "B": f.bob.id} {
		st, err := reg.AssignOwner(ctx, registry.SystemActor, f.stations[k].st.Idx, owner)
		if err != nil {
			t.Fatal(err)
		}
		f.stations[k].st = st
	}
	for _, k := range []string{"A", "B", "C", "N"} {
		st, _ := reg.Station(f.stations[k].st.Idx)
		f.stations[k].st = st
	}
	if !f.stations["B"].st.HasPublic || f.stations["A"].st.HasPublic {
		t.Fatalf("public pins: B %+v, A %+v", f.stations["B"].st.Public, f.stations["A"].st.Public)
	}
	return f
}

func (f *netFixture) register(t *testing.T, name, addr string) session4 {
	t.Helper()
	rec := serve(f.srv.Handler(), "POST", "/api/auth/register", addr, `{"username":"`+name+`","password":"`+name+`-password"}`)
	if rec.Code != 200 {
		t.Fatalf("register %s: %d %s", name, rec.Code, rec.Body.String())
	}
	acct, err := f.reg.Accounts().ByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return session4{cookie: cookieOf(rec), csrf: decodeInfo(t, rec).CSRFToken, id: acct.ID, addr: addr}
}

// as sends a request as session s (anonymous when s is zero; then from addr).
func (f *netFixture) as(s session4, method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	addr := s.addr
	if addr == "" {
		addr = "192.0.2.10:4000"
	}
	if s.cookie != "" {
		hdr = append(s.hdr(), hdr...)
	}
	return serve(f.srv.Handler(), method, target, addr, body, hdr...)
}

func (f *netFixture) anon(method, target, addr, body string) *httptest.ResponseRecorder {
	return serve(f.srv.Handler(), method, target, addr, body)
}

// publicFixture is what no public surface may contain.
func (f *netFixture) publicFixture() *privacy.Fixture {
	fx := &privacy.Fixture{
		Points: []privacy.Point{{Lat: sim.DefaultLat, Lon: sim.DefaultLon, Label: "host / demo receiver"}},
	}
	for k, s := range f.stations {
		fx.Points = append(fx.Points, privacy.Point{Lat: s.lat, Lon: s.lon, Strict: true, Label: "station " + k})
		fx.IPs = append(fx.IPs, s.ip)
		if s.pin == model.PinHidden {
			fx.SIDs = append(fx.SIDs, s.st.SID)
		}
		if s.uuid != "" {
			fx.Secrets = append(fx.Secrets, s.uuid, s.uuid[len(s.uuid)-6:])
		}
		if s.st.FeederID != "" {
			fx.Secrets = append(fx.Secrets, s.st.FeederID)
		}
	}
	return fx
}

// ownerFixture is what owner `own` may not see: every other station.
func (f *netFixture) ownerFixture(own string) *privacy.Fixture {
	fx := &privacy.Fixture{NoKeyCheck: true,
		Points: []privacy.Point{{Lat: sim.DefaultLat, Lon: sim.DefaultLon, Label: "host / demo receiver"}}}
	for k, s := range f.stations {
		if k == own {
			fx.AllowIPs = append(fx.AllowIPs, s.ip) // the owner sees the own current address
			continue
		}
		fx.IPs = append(fx.IPs, s.ip)
		fx.Points = append(fx.Points, privacy.Point{Lat: s.lat, Lon: s.lon, Strict: true, Label: "station " + k})
		if s.pin == model.PinHidden {
			fx.SIDs = append(fx.SIDs, s.st.SID)
		}
		if s.uuid != "" {
			fx.Secrets = append(fx.Secrets, s.uuid)
		}
		if s.st.FeederID != "" {
			fx.Secrets = append(fx.Secrets, s.st.FeederID)
		}
	}
	return fx
}

var placeholderRE = regexp.MustCompile(`\{[a-z]+(\.\.\.)?\}`)

// netSample resolves a route's sample URL for the crawls without touching
// stored data: placeholders of routes the crawler may not call resolve to
// dummies (the gate answers before the handler).
func (f *netFixture) netSample(rt Route, sid string) string {
	u := rt.Sample
	if u == "" {
		u = rt.Path
	}
	live := "3C6444"
	for _, a := range f.eng.Tracker().Snapshot().Aircraft {
		if a.Lat != nil && !strings.HasPrefix(a.ICAO, "~") {
			live = a.ICAO
			break
		}
	}
	rep := strings.NewReplacer("{live}", live, "{icao}", live, "{seen}", "3C6444", "{flight}", "1", "{airport}", "EDDF",
		"{route}", "DLH400?lat=50.2&lon=7.9", "lat=50.05&lon=8.6", "lat=47.123&lon=9.321", "{watchlist}", "demo", "{sid}", sid, "{link}", "1")
	u = rep.Replace(u)
	return placeholderRE.ReplaceAllString(u, "x")
}

func scanOrFail(t *testing.T, fx *privacy.Fixture, name string, body []byte) {
	t.Helper()
	for _, l := range fx.Scan(body) {
		t.Errorf("%s leaks %s\n  body: %.400s", name, l, snippet(body, l.Offset))
	}
}

func snippet(b []byte, off int) string {
	lo, hi := max(0, off-120), min(len(b), off+120)
	return string(b[lo:hi])
}

// readSSE runs one live stream for d and returns what it sent.
func (f *netFixture) readSSE(t *testing.T, target, addr string, d time.Duration, hdr ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req := httptest.NewRequest("GET", target, nil).WithContext(ctx)
	req.RemoteAddr = addr
	req.Host = "192.168.1.10:8080"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func jsonOf(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %d %.300s: %v", rec.Code, rec.Body.String(), err)
	}
}

func must(t *testing.T, rec *httptest.ResponseRecorder, code int, what string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("%s: status %d, want %d: %.400s", what, rec.Code, code, rec.Body.String())
	}
}
