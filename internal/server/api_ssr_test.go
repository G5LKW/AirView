package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func init() {
	for _, k := range []string{"GET /api/ssr", "POST /api/ssr/solve"} {
		specialTests[k] = "TestSSRRoutes"
	}
}

// TestSSRRoutes: the experimental SSR routes off and on, and a PUT
// /api/config without the ssr block keeps it.
func TestSSRRoutes(t *testing.T) {
	r := newRig(t, rigOptions{})
	rec := r.get("/api/ssr")
	must(t, rec, 200, "status when off")
	if !strings.Contains(rec.Body.String(), `"experimental":true`) || strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("status when off: %s", rec.Body)
	}
	must(t, r.do("POST", "/api/ssr/solve", loopbackAddr, ""), 409, "solve when off")

	cfg := r.eng.EffectiveConfig()
	cfg.SSR.Enabled = true
	if _, _, err := r.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// A settings save that does not know the ssr block keeps it.
	al, _ := json.Marshal(r.eng.EffectiveConfig().Alerts)
	must(t, r.do("PUT", "/api/config", loopbackAddr, `{"alerts":`+string(al)+`}`), 200, "put without ssr")
	if !r.eng.EffectiveConfig().SSR.Enabled {
		t.Fatal("PUT /api/config without ssr switched it off")
	}
	rec = r.get("/api/ssr")
	must(t, rec, 200, "status when on")
	if !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("status when on: %s", rec.Body)
	}
	must(t, r.do("POST", "/api/ssr/solve", loopbackAddr, ""), 202, "solve when on")
}
