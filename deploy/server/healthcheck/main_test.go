package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaultURL(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{nil, "http://127.0.0.1:80/healthz"},
		{map[string]string{"AIRVIEW_HTTP_PORT": "8080"}, "http://127.0.0.1:8080/healthz"},
		{map[string]string{"AIRVIEW_HTTP_PORT": " 9000 "}, "http://127.0.0.1:9000/healthz"},
		{map[string]string{"AIRVIEW_HTTP_PORT": "nope"}, "http://127.0.0.1:80/healthz"},
		{map[string]string{"AIRVIEW_HTTP_PORT": "70000"}, "http://127.0.0.1:80/healthz"},
		{map[string]string{"AIRVIEW_HTTP_PORT": "8080", "AIRVIEW_HEALTHCHECK_URL": "http://x:1/y"}, "http://x:1/y"},
	}
	for _, c := range cases {
		if got := defaultURL(envOf(c.env)); got != c.want {
			t.Errorf("defaultURL(%v) = %q, want %q", c.env, got, c.want)
		}
	}
}

func TestRun(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer ok.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()

	var errs strings.Builder
	if code := run([]string{"-url", ok.URL + "/healthz"}, envOf(nil), &errs); code != 0 {
		t.Fatalf("healthy server: exit %d (%s)", code, errs.String())
	}
	if code := run(nil, envOf(map[string]string{"AIRVIEW_HEALTHCHECK_URL": ok.URL + "/healthz"}), &errs); code != 0 {
		t.Fatalf("healthy server via env: exit %d (%s)", code, errs.String())
	}
	errs.Reset()
	if code := run([]string{"-url", failing.URL}, envOf(nil), &errs); code != 1 || !strings.Contains(errs.String(), "503") {
		t.Fatalf("503: exit %d, stderr %q", code, errs.String())
	}
	if code := run([]string{"-url", ok.URL + "/missing"}, envOf(nil), &errs); code != 1 {
		t.Fatalf("404: exit %d", code)
	}
	if code := run([]string{"-url", slow.URL, "-timeout", "100ms"}, envOf(nil), &errs); code != 1 {
		t.Fatalf("timeout: exit %d", code)
	}
	// Nothing listening.
	if code := run([]string{"-url", "http://127.0.0.1:1/healthz", "-timeout", "1s"}, envOf(nil), &errs); code != 1 {
		t.Fatalf("closed port: exit %d", code)
	}
	if code := run([]string{"-bogus"}, envOf(nil), io.Discard); code != 2 {
		t.Fatalf("bad flag: exit %d", code)
	}
}
