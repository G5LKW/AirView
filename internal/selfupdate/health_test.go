package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func seqProbe(results ...func() (ProbeResult, error)) Probe {
	var i atomic.Int64
	return ProbeFunc(func(context.Context) (ProbeResult, error) {
		n := int(i.Add(1)) - 1
		if n >= len(results) {
			n = len(results) - 1
		}
		return results[n]()
	})
}

func ok(v, inst string, up time.Duration) func() (ProbeResult, error) {
	return func() (ProbeResult, error) { return ProbeResult{Version: v, Instance: inst, Uptime: up}, nil }
}

func fail(msg string) func() (ProbeResult, error) {
	return func() (ProbeResult, error) { return ProbeResult{}, errors.New(msg) }
}

func fastPolicy(v string) HealthPolicy {
	return HealthPolicy{Window: 200 * time.Millisecond, Interval: 10 * time.Millisecond, ExpectVersion: v}
}

func TestWaitHealthy(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		probe Probe
		want  string // "" = healthy
	}{
		{"healthy", seqProbe(ok("2", "a", -1)), ""},
		{"slow start", seqProbe(fail("refused"), fail("refused"), ok("2", "", -1)), ""},
		{"never", seqProbe(fail("refused")), "no healthy answer"},
		{"wrong version", seqProbe(ok("1", "", -1)), "expected 2"},
		{"instance change", seqProbe(ok("2", "pid=1", -1), ok("2", "pid=1", -1), ok("2", "pid=2", -1)), "restarted"},
		{"uptime drop", seqProbe(ok("2", "", 5*time.Second), ok("2", "", 6*time.Second), ok("2", "", time.Second)), "restarted"},
		{"dies", seqProbe(ok("2", "", -1), ok("2", "", -1), fail("refused")), "refused"},
		{"one blip tolerated", seqProbe(ok("2", "", -1), fail("timeout"), ok("2", "", -1)), ""},
		{"unknown version accepted", seqProbe(ok("", "", -1)), ""},
	}
	for _, c := range cases {
		err := WaitHealthy(ctx, c.probe, fastPolicy("2"))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !errors.Is(err, ErrUnhealthy) || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := WaitHealthy(cctx, seqProbe(fail("x")), HealthPolicy{Window: time.Second, Interval: 10 * time.Millisecond}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}
}

func TestHTTPProbe(t *testing.T) {
	code := atomic.Int64{}
	code.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-AirView-Feeder") != "1" {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(int(code.Load()))
		fmt.Fprint(w, `{"ok":true,"version":"1.2.3","uptimeS":12.5,"pid":42}`)
	}))
	defer srv.Close()
	p := HTTPProbe{URL: srv.URL, Header: http.Header{"X-Airview-Feeder": {"1"}}}
	r, err := p.Probe(context.Background())
	if err != nil || r.Version != "1.2.3" || r.Uptime != 12500*time.Millisecond || r.Instance != "42" {
		t.Fatalf("%+v %v", r, err)
	}
	code.Store(503)
	if _, err := p.Probe(context.Background()); err == nil {
		t.Error("503 accepted by default")
	}
	p.Accept = []int{200, 503}
	if r, err := p.Probe(context.Background()); err != nil || r.Version != "1.2.3" {
		t.Errorf("503 accepted explicitly: %v", err)
	}
	if _, err := (HTTPProbe{URL: "http://127.0.0.1:1/x"}).Probe(context.Background()); err == nil {
		t.Error("closed port")
	}
}

func TestSystemdProbeAndRestarter(t *testing.T) {
	var calls []string
	state := "ActiveState=active\nSubState=running\nMainPID=1234\nNRestarts=0\n"
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte(state), nil
	}
	p := SystemdProbe{Unit: "airview-feeder", Run: run}
	r, err := p.Probe(context.Background())
	if err != nil || r.Instance != "pid=1234,restarts=0" {
		t.Fatalf("%+v %v", r, err)
	}
	state = "ActiveState=activating\nSubState=auto-restart\nMainPID=0\nNRestarts=3\n"
	if _, err := p.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "activating/auto-restart") {
		t.Errorf("%v", err)
	}
	if err := (SystemdRestarter{Unit: "airview-feeder", Run: run}).Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := calls[len(calls)-1]; last != "systemctl restart airview-feeder" {
		t.Errorf("%q", last)
	}
	// Combined probe.
	all := AllProbes{ProbeFunc(func(context.Context) (ProbeResult, error) {
		return ProbeResult{Instance: "pid=1", Uptime: -1}, nil
	}), ProbeFunc(func(context.Context) (ProbeResult, error) {
		return ProbeResult{Version: "2", Uptime: 3 * time.Second}, nil
	})}
	r, err = all.Probe(context.Background())
	if err != nil || r.Version != "2" || r.Instance != "pid=1" || r.Uptime != 3*time.Second {
		t.Fatalf("%+v %v", r, err)
	}
}
