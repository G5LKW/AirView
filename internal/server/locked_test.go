package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// GET /api/config lists the settings fixed by the operator file (path and
// origin only) for the admin; the public view and a plain install do not
// have the member (HOOKS WP-CF-configfiles -> internal/server).
func TestConfigLockedSettings(t *testing.T) {
	type reply struct {
		Locked []struct{ Path, Origin string } `json:"locked"`
		NTP    struct {
			IntervalSec int `json:"intervalSec"`
		} `json:"ntp"`
	}
	plain := newRig(t, rigOptions{})
	var p reply
	if rec := plain.get("/api/config"); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &p) != nil || p.Locked != nil {
		t.Fatalf("plain install: %d %s", rec.Code, rec.Body.String())
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "airview.jsonc"), []byte(`{"ntp": {"intervalSec": 128}} // operator`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newRigIn(t, dir, rigOptions{})
	t.Cleanup(r.close)
	var got reply
	rec := r.get("/api/config")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if got.NTP.IntervalSec != 128 || len(got.Locked) != 1 || got.Locked[0].Path != "ntp.intervalSec" || got.Locked[0].Origin == "" {
		t.Fatalf("locked: %+v ntp %d", got.Locked, got.NTP.IntervalSec)
	}
	// The override stays in force across a PUT.
	if rec := r.do("PUT", "/api/config", loopbackAddr, `{"ntp":{"enabled":true,"server":"","intervalSec":64}}`); rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if v := r.eng.Config().Get().NTP.IntervalSec; v != 128 {
		t.Fatalf("override lost: %d", v)
	}
}
