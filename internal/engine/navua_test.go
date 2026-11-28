package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/navdata"
	"airview/internal/store"
)

// TestRouteLookupUserAgent: with the factory defaults the engine's online route look-up is on and
// identifies itself as "AirView/<version> (+<project URL>)", never with a personal address.
func TestRouteLookupUserAgent(t *testing.T) {
	var ua atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"response":"unknown callsign"}`))
	}))
	defer srv.Close()

	e := &Engine{opts: Options{Version: "1.1.0-test"}, log: quietLogger()}
	cfg := model.DefaultConfig()
	o := e.navOptions(cfg)
	if !o.Routes.Online {
		t.Fatal("online route look-ups must be on by default (binding decision 2)")
	}
	o.Routes.BaseURL = srv.URL + "/v0/callsign/"
	o.Routes.MinInterval = time.Millisecond
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	nd := navdata.New(st, o)
	if _, _, err := nd.OnlineRoute(context.Background(), "BAW123"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	want := "AirView/1.1.0-test (+" + model.ProjectURL + ")"
	if got, _ := ua.Load().(string); got != want {
		t.Fatalf("User-Agent %q, want %q", got, want)
	}
}
