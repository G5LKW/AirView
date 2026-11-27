package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

// Secrets in feed URLs, webhook URLs, the photo contact and host details are
// not served to clients that are not admin (GET routes are open to the LAN
// and to any web page via CORS *).
func TestNonAdminRedaction(t *testing.T) {
	r := newRig(t, rigOptions{})
	var cfg model.Config
	if err := json.Unmarshal(r.get("/api/config").Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.HTTP.AdminPasswordHash = "plain:admin-pw"
	cfg.NavData.PhotoContact = "owner@example.org"
	cfg.Feeds = append(cfg.Feeds, model.Feed{ID: "j1", Name: "json", Type: model.FeedJSON,
		URL: "http://feeduser:FeedPassw0rd@127.0.0.1:1/data/aircraft.json?apikey=SECRETKEY123"})
	body, _ := json.Marshal(cfg)
	if rec := r.do("PUT", "/api/config", loopbackAddr, string(body)); rec.Code != 200 {
		t.Fatalf("PUT config: %d %s", rec.Code, rec.Body.String())
	}
	rule := `{"name":"hook","enabled":true,"severity":"info","match":{"type":"squawk","values":["7700"]},` +
		`"actions":{"notify":true,"webhook":"https://hooks.example.com/services/T000/B000/XXXXSECRETTOKENXXXX"}}`
	rec := r.do("POST", "/api/alerts/rules", loopbackAddr, rule)
	if rec.Code != 200 {
		t.Fatalf("POST rule: %d %s", rec.Code, rec.Body.String())
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	secrets := []string{"FeedPassw0rd", "SECRETKEY123", "owner@example.org", "XXXXSECRETTOKENXXXX", r.dir}
	for _, path := range []string{"/api/config", "/api/feeds", "/api/status", "/api/system", "/api/db/stats",
		"/api/tilecache", "/api/alerts/rules", "/api/alerts/rules/" + created.ID, "/api/alerts/export"} {
		rec := r.do("GET", path, lanAddr, "", "Origin", "https://evil.example")
		if rec.Code != 200 {
			t.Errorf("LAN GET %s: %d", path, rec.Code)
			continue
		}
		b := rec.Body.String()
		for _, s := range secrets {
			if strings.Contains(b, s) || strings.Contains(b, strings.ReplaceAll(s, `\`, `\\`)) {
				t.Errorf("LAN GET %s leaks %q", path, s)
			}
		}
	}
	// The webhook host is still visible, so the UI can show that one is set.
	if b := r.do("GET", "/api/alerts/rules/"+created.ID, lanAddr, "").Body.String(); !strings.Contains(b, "https://hooks.example.com/...") {
		t.Errorf("redacted webhook: %s", b)
	}
	// Admins (loopback) still get everything they need to edit.
	if b := r.do("GET", "/api/config", loopbackAddr, "").Body.String(); !strings.Contains(b, "SECRETKEY123") || !strings.Contains(b, "owner@example.org") {
		t.Error("admin config lost the feed URL / photo contact")
	}
	if b := r.do("GET", "/api/alerts/rules", loopbackAddr, "").Body.String(); !strings.Contains(b, "XXXXSECRETTOKENXXXX") {
		t.Error("admin rules lost the webhook URL")
	}
}

// One LAN address cannot take every live-stream slot, and loopback / admin
// clients are admitted even when the regular slots are full.
func TestSSEAdmission(t *testing.T) {
	h := newLiveHub(nil)
	add := func(ip string, loop, priv bool) string {
		c := &liveClient{kick: make(chan struct{}), id: h.seq.Add(1), ip: ip, loopback: loop}
		msg := h.admitLocked(c, priv)
		if msg == "" {
			h.clients[c] = struct{}{}
		}
		return msg
	}
	for i := 0; i < sseMaxPerIP; i++ {
		if msg := add("192.168.0.149", false, false); msg != "" {
			t.Fatalf("stream %d refused: %s", i, msg)
		}
	}
	if add("192.168.0.149", false, false) == "" {
		t.Fatal("per-IP cap not enforced")
	}
	// Fill the regular slots from many addresses.
	for i := 0; len(h.clients) < sseMaxClients; i++ {
		if msg := add("10.0.0."+string(rune('a'+i/sseMaxPerIP)), false, false); msg != "" {
			t.Fatalf("refused while filling: %s", msg)
		}
	}
	if add("10.1.1.1", false, false) == "" {
		t.Fatal("regular client admitted beyond sseMaxClients")
	}
	for i := 0; i < sseReserve; i++ {
		if msg := add("127.0.0.1", true, true); msg != "" {
			t.Fatalf("loopback refused at %d clients: %s", len(h.clients), msg)
		}
	}
	// Reserve full: a privileged client evicts the oldest stream of the
	// address holding the most.
	n := len(h.clients)
	if msg := add("127.0.0.1", true, true); msg != "" {
		t.Fatalf("loopback refused with the reserve full: %s", msg)
	}
	if len(h.clients) != n {
		t.Fatalf("no eviction: %d -> %d clients", n, len(h.clients))
	}
}

// A factory reset (exclusive route) aborts exports that hold the reset read
// lock instead of waiting for slow clients.
func TestExclusiveAbortsStreams(t *testing.T) {
	s := &Server{}
	s.resetMu.RLock()
	aborted := make(chan struct{})
	var once bool
	id := s.streams.add(func() {
		if !once {
			once = true
			close(aborted)
		}
	})
	go func() {
		<-aborted
		s.streams.remove(id)
		s.resetMu.RUnlock()
	}()
	done := make(chan struct{})
	go func() {
		s.lockExclusive()
		s.resetMu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("exclusive lock waited for the streaming export")
	}
}
