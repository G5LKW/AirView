package feederui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/config"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/uplink"
)

type fakeBackend struct {
	mu       sync.Mutex
	claims   int
	retries  []string
	saved    *model.FeederConfig
	claimErr error
}

func (b *fakeBackend) Snapshot() Snapshot {
	return Snapshot{Software: "airview-feeder", Version: "test", FeederID: "abc",
		Receivers: []ReceiverStatus{{RxNo: 1, Name: "rx", Connected: true}},
		Uplinks:   []uplink.Status{{ID: "u", Host: "h", State: uplink.StateConnected}}}
}

func (b *fakeBackend) Claim(ctx context.Context, id string, rx int) (string, time.Time, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.claims++
	if b.claimErr != nil {
		return "", time.Time{}, b.claimErr
	}
	return "ABCD2345", time.UnixMilli(1e12), nil
}

func (b *fakeBackend) Retry(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id == "missing" {
		return uplink.ErrNoUplink
	}
	b.retries = append(b.retries, id)
	return nil
}

func (b *fakeBackend) Settings() SettingsDoc {
	c := model.DefaultFeederConfig()
	c.Uplinks = []model.Uplink{{ID: "u", Host: "h", EnrolToken: Redacted}}
	return SettingsDoc{Config: c, File: "/x/feeder.json"}
}

func (b *fakeBackend) SaveSettings(c model.FeederConfig) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.saved = &c
	return []string{"ok"}, nil
}

func do(t *testing.T, h http.Handler, method, target, remote, host string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	r.RemoteAddr = remote
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const local = "127.0.0.1:5555"

func TestPageAndStatus(t *testing.T) {
	s := New(Options{Backend: &fakeBackend{}})
	w := do(t, s, "GET", "/", local, "127.0.0.1:8099", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "AirView Feeder") {
		t.Fatalf("page: %d %q", w.Code, w.Body.String()[:min(80, w.Body.Len())])
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing security headers")
	}
	for _, p := range []string{"/app.js", "/style.css", "/favicon.svg"} {
		if w := do(t, s, "GET", p, local, "localhost:8099", nil, ""); w.Code != 200 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	w = do(t, s, "GET", "/api/status", local, "[::1]:8099", nil, "")
	var snap Snapshot
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &snap) != nil || snap.FeederID != "abc" {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	w = do(t, s, "GET", "/api/health", local, "127.0.0.1", nil, "")
	var h Health
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &h) != nil || !h.OK || h.Connected != 1 || h.Inputs != 1 {
		t.Fatalf("health: %d %s", w.Code, w.Body)
	}
}

func TestHostAndRemoteChecks(t *testing.T) {
	s := New(Options{Backend: &fakeBackend{}})
	if w := do(t, s, "GET", "/api/status", local, "evil.example.com:8099", nil, ""); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding host accepted: %d", w.Code)
	}
	if w := do(t, s, "GET", "/api/status", "192.168.1.5:3333", "192.168.1.2:8099", nil, ""); w.Code != http.StatusForbidden {
		t.Fatalf("LAN client without password configured: %d", w.Code)
	}
	hash := config.HashPassword("secret")
	s = New(Options{Backend: &fakeBackend{}, PasswordHash: func() string { return hash }})
	if w := do(t, s, "GET", "/api/status", "192.168.1.5:3333", "192.168.1.2:8099", nil, ""); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("LAN client without credentials: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.RemoteAddr, r.Host = "192.168.1.5:3333", "192.168.1.2:8099"
	r.SetBasicAuth("any", "secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("LAN client with password: %d", w.Code)
	}
	r.SetBasicAuth("any", "wrong")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", w.Code)
	}
	// loopback never needs it
	if w := do(t, s, "GET", "/api/status", local, "127.0.0.1:8099", nil, ""); w.Code != 200 {
		t.Fatalf("loopback: %d", w.Code)
	}
}

func TestCSRFAndActions(t *testing.T) {
	b := &fakeBackend{}
	s := New(Options{Backend: b})
	if w := do(t, s, "POST", "/api/claim", local, "127.0.0.1:8099", nil, `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("claim without header: %d", w.Code)
	}
	hdr := map[string]string{CSRFHeader: "1", "Content-Type": "application/json", "Origin": "http://evil.example"}
	if w := do(t, s, "POST", "/api/claim", local, "127.0.0.1:8099", hdr, `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site claim: %d", w.Code)
	}
	hdr["Origin"] = "http://127.0.0.1:8099"
	w := do(t, s, "POST", "/api/claim", local, "127.0.0.1:8099", hdr, `{"id":"u","rxNo":1}`)
	var cr ClaimResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &cr) != nil || cr.Code != "ABCD2345" || cr.ExpiresMs != 1e12 {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	b.claimErr = errors.New("uplink is not connected")
	if w := do(t, s, "POST", "/api/claim", local, "127.0.0.1:8099", hdr, `{}`); w.Code != http.StatusConflict {
		t.Fatalf("claim error: %d", w.Code)
	}
	b.claimErr = uplink.ErrNoUplink
	if w := do(t, s, "POST", "/api/claim", local, "127.0.0.1:8099", hdr, `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("claim unknown uplink: %d", w.Code)
	}
	if w := do(t, s, "POST", "/api/retry", local, "127.0.0.1:8099", hdr, `{"id":"u"}`); w.Code != 200 || len(b.retries) != 1 {
		t.Fatalf("retry: %d", w.Code)
	}
	if w := do(t, s, "POST", "/api/retry", local, "127.0.0.1:8099", hdr, `{"id":"missing"}`); w.Code != 404 {
		t.Fatalf("retry missing: %d", w.Code)
	}
	if w := do(t, s, "POST", "/api/retry", local, "127.0.0.1:8099", map[string]string{CSRFHeader: "1", "Content-Type": "text/plain"}, `{"id":"u"}`); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain body: %d", w.Code)
	}
	// settings round trip
	w = do(t, s, "GET", "/api/settings", local, "127.0.0.1:8099", nil, "")
	var doc SettingsDoc
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &doc) != nil || doc.Config.Uplinks[0].EnrolToken != Redacted {
		t.Fatalf("settings: %d %s", w.Code, w.Body)
	}
	body, _ := json.Marshal(doc.Config)
	w = do(t, s, "PUT", "/api/settings", local, "127.0.0.1:8099", map[string]string{CSRFHeader: "1"}, string(body))
	if w.Code != 200 || b.saved == nil || b.saved.Uplinks[0].Host != "h" {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
}

func TestHostAllowed(t *testing.T) {
	for h, want := range map[string]bool{
		"": true, "127.0.0.1": true, "127.0.0.1:8099": true, "[::1]:8099": true, "localhost:8099": true,
		"LOCALHOST.": true, "pi.localhost": true, "10.0.0.2:8099": true, "attacker.com": false, "raspberrypi:8099": false,
	} {
		if got := hostAllowed(h); got != want {
			t.Errorf("hostAllowed(%q) = %v", h, got)
		}
	}
}

func esFrame(icao uint32, me uint64, df byte) []byte {
	f := make([]byte, 14)
	f[0] = df<<3 | 5
	f[1], f[2], f[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(me >> (8 * (6 - i)))
	}
	modes.SetParity(f, 0)
	return f
}

func TestStats(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := NewStats(func() time.Time { return now })
	pos := esFrame(0x4ca123, uint64(11)<<51, 17)
	ident := esFrame(0x4ca124, uint64(4)<<51, 17)
	bad := append([]byte(nil), pos...)
	bad[5] ^= 0x10
	for i := 0; i < 30; i++ {
		now = now.Add(100 * time.Millisecond)
		s.HandleFrame(model.RawFrame{FeedID: "rx1", Kind: model.FrameModeSLong, Data: pos, Signal: -10})
		s.HandleFrame(model.RawFrame{FeedID: "rx1", Kind: model.FrameModeSLong, Data: ident, Signal: -2})
	}
	s.HandleFrame(model.RawFrame{FeedID: "rx1", Kind: model.FrameModeSLong, Data: bad, Signal: math.NaN()})
	s.HandleFrame(model.RawFrame{FeedID: "rx1", Kind: model.FrameModeAC, Data: []byte{1, 2}, Signal: math.NaN()})
	s.HandleFrame(model.RawFrame{FeedID: "rx1", Kind: model.FrameModeSLong, Data: pos, MLAT: true})
	s.HandleFrame(model.RawFrame{FeedID: "rx1", Kind: model.FrameStatus, Data: []byte{0}})
	lat := 51.0
	s.HandleUpdate(model.DecodedUpdate{FeedID: "sbs", ICAO: 0x111111, Lat: &lat})
	now = now.Add(time.Second)
	snap := s.Snapshot()
	r := snap["rx1"]
	if r.Frames != 62 || r.ADSB != 60 || r.Positions != 30 || r.BadCRC != 1 || r.ModeAC != 1 || r.MLATIgnored != 1 || r.StatusFrames != 1 {
		t.Fatalf("counters: %+v", r)
	}
	if r.Aircraft != 2 || r.AircraftADSB != 2 {
		t.Fatalf("aircraft: %+v", r)
	}
	if !r.HasSignal || r.SignalDBFS > -2 || r.SignalDBFS < -10 || r.StrongPct <= 0 {
		t.Fatalf("signal: %+v", r)
	}
	if r.FPS < 5 || r.FPS > 8 {
		t.Fatalf("fps %.2f", r.FPS)
	}
	if r.DF[17] != 61 {
		t.Fatalf("df: %v", r.DF)
	}
	if sb := snap["sbs"]; sb.Decoded != 1 || sb.Aircraft != 1 || sb.Positions != 1 {
		t.Fatalf("sbs: %+v", sb)
	}
	now = now.Add(2 * AircraftWindow)
	if r := s.Snapshot()["rx1"]; r.Aircraft != 0 || r.FPS != 0 {
		t.Fatalf("after window: %+v", r)
	}
	s.Forget("sbs")
	if _, ok := s.Snapshot()["sbs"]; ok {
		t.Fatal("forget")
	}
}

func BenchmarkStatsHandleFrame(b *testing.B) {
	s := NewStats(nil)
	f := model.RawFrame{FeedID: "rx1", Kind: model.FrameModeSLong, Data: esFrame(0x4ca123, uint64(11)<<51, 17), Signal: -10}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.HandleFrame(f)
	}
}

// SEC-08: wrong Basic-auth passwords are throttled per client; the right
// password from another client still works.
func TestBasicAuthThrottled(t *testing.T) {
	hash := config.HashPassword("secret")
	s := New(Options{Backend: &fakeBackend{}, PasswordHash: func() string { return hash }})
	try := func(remote, pw string) int {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.RemoteAddr, r.Host = remote, "192.168.1.2:8099"
		r.SetBasicAuth("any", pw)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	code := 0
	for range 6 {
		code = try("192.168.1.66:1", "guess")
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("guessing not throttled: %d", code)
	}
	if c := try("192.168.1.66:2", "secret"); c != http.StatusTooManyRequests {
		t.Fatalf("throttled client not blocked: %d", c)
	}
	if c := try("192.168.1.5:3", "secret"); c != 200 {
		t.Fatalf("other client: %d", c)
	}
	if c := try("192.168.1.5:3", "secret"); c != 200 { // from the cache
		t.Fatalf("cached: %d", c)
	}
}
