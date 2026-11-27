package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// buildScratch builds testdata/scratchapp with the given version and mode.
func buildScratch(t *testing.T, dir, version, mode string) string {
	t.Helper()
	out := filepath.Join(dir, fmt.Sprintf("scratch-%s-%s%s", version, mode, exeSuffix()))
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version="+version+" -X main.mode="+mode, "-o", out, "./testdata/scratchapp")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build scratchapp: %v\n%s", err, b)
	}
	return out
}

// freeAddr returns a free ephemeral loopback address. A fixed port made
// the tests fail whenever another process (or a parallel package) held it.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// supervisor plays the service manager for the scratch program: it
// starts the program file and stops the running process by its PID.
type supervisor struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

// testUpTimeout bounds waits for a test program to come up (or come back
// after a rollback). It is generous on purpose: the release gate runs while
// other builds load the machine, and a program that is merely slow to
// start must not fail the test. AIRVIEW_TEST_TIMEOUT_SCALE multiplies it.
func testUpTimeout() time.Duration {
	d := 30 * time.Second
	if f, err := strconv.ParseFloat(os.Getenv("AIRVIEW_TEST_TIMEOUT_SCALE"), 64); err == nil && f > 0 {
		d = time.Duration(float64(d) * f)
	}
	return d
}

// waitUp polls the probe until the program answers healthy with version
// want twice in a row, or testUpTimeout passes.
func waitUp(t *testing.T, p Probe, want string) error {
	t.Helper()
	deadline := time.Now().Add(testUpTimeout())
	var lastErr error
	ok := 0
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		r, err := p.Probe(ctx)
		cancel()
		if err == nil && r.Version != want {
			err = fmt.Errorf("version %q, want %q", r.Version, want)
		}
		if err == nil {
			if ok++; ok >= 2 {
				return nil
			}
		} else {
			ok, lastErr = 0, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("not healthy with version %s within %v: %v", want, testUpTimeout(), lastErr)
}

func (s *supervisor) start(exe, addr string) error {
	cmd := exec.Command(exe, addr)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	s.mu.Lock()
	s.cmd, s.done = cmd, done
	s.mu.Unlock()
	return nil
}

func (s *supervisor) stop(context.Context) error {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.cmd = nil
	s.mu.Unlock()
	if cmd == nil {
		return nil
	}
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		return errors.New("process did not exit")
	}
	return nil
}

func (s *supervisor) restarter(exe, addr string) Restarter {
	return RestarterFunc(func(ctx context.Context) error {
		if err := s.stop(ctx); err != nil {
			return err
		}
		return s.start(exe, addr)
	})
}

// TestIntegrationRollback installs real scratch binaries: a crashing and a
// flaky new version are rolled back (the previous binary is restored and
// running again), a good one is kept. This is the WP-UP acceptance test
// "rollback on a failed health check".
func TestIntegrationRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not found")
	}
	bin := t.TempDir()
	v1 := buildScratch(t, bin, "1.0.0", "good")
	builds := map[string]string{
		"crash": buildScratch(t, bin, "1.1.0", "crash"),
		"flaky": buildScratch(t, bin, "1.1.0", "flaky"),
		"sick":  buildScratch(t, bin, "1.1.0", "sick"),
		"good":  buildScratch(t, bin, "1.1.0", "good"),
	}
	v1Bytes := readFile(t, v1)

	pub, priv, _ := GenerateKey()
	addr := freeAddr(t)
	health := "http://" + addr + "/health"
	noKeepAlive := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	for _, mode := range []string{"crash", "flaky", "sick", "good"} {
		t.Run(mode, func(t *testing.T) {
			relDir := t.TempDir()
			srv := httptest.NewServer(http.StripPrefix("/feeder", ReleaseHandler{Dir: relDir}))
			defer srv.Close()
			if _, err := Publish(PublishOptions{Product: "airview-feeder", Version: "1.1.0", Channel: ChannelStable,
				Artifacts: map[string]string{CurrentTarget(): builds[mode]}, OutDir: relDir, Key: priv}); err != nil {
				t.Fatal(err)
			}
			app := t.TempDir()
			exe := filepath.Join(app, "airview-feeder"+exeSuffix())
			if err := os.WriteFile(exe, v1Bytes, 0o755); err != nil {
				t.Fatal(err)
			}
			sup := &supervisor{}
			if err := sup.start(exe, addr); err != nil {
				t.Fatal(err)
			}
			defer sup.stop(context.Background())
			probe := HTTPProbe{URL: health, Client: noKeepAlive}
			if err := waitUp(t, probe, "1.0.0"); err != nil {
				t.Fatalf("v1 not healthy: %v", err)
			}
			u, err := New(Options{Product: "airview-feeder", CurrentVersion: "1.0.0", Exe: exe, StateDir: filepath.Join(app, "state"),
				Settings:  Settings{ManifestURL: srv.URL + "/feeder/manifest.json", Keys: []string{pub.String()}},
				Restarter: sup.restarter(exe, addr), Probe: probe,
				Health: HealthPolicy{Window: 6 * time.Second, StartTimeout: 4 * time.Second, Interval: 150 * time.Millisecond}, Logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			res, err := u.Update(context.Background(), UpdateOptions{})
			if mode == "good" {
				if err != nil || res.Outcome != OutcomeUpdated {
					t.Fatalf("%+v %v", res, err)
				}
				if !bytes.Equal(readFile(t, exe), readFile(t, builds["good"])) {
					t.Fatal("new binary not in place")
				}
				if perr := waitUp(t, probe, "1.1.0"); perr != nil {
					t.Fatalf("new version not running: %v", perr)
				}
				return
			}
			if !errors.Is(err, ErrRolledBack) || res.Outcome != OutcomeRolledBack {
				t.Fatalf("%+v %v", res, err)
			}
			if !bytes.Equal(readFile(t, exe), v1Bytes) {
				t.Fatal("previous binary not restored")
			}
			if perr := waitUp(t, probe, "1.0.0"); perr != nil {
				t.Fatalf("previous version not running after rollback: %v", perr)
			}
			if j := u.Status().Last; j.Phase != PhaseRolledBack {
				t.Fatalf("journal %+v", j)
			}
			t.Logf("rolled back: %s", res.Detail)
		})
	}
}
