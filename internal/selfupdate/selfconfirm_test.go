package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// buildSelfApp builds testdata/selfapp (a program using Attach/Run).
func buildSelfApp(t *testing.T, dir, version, mode string) string {
	t.Helper()
	out := filepath.Join(dir, fmt.Sprintf("selfapp-%s-%s%s", version, mode, exeSuffix()))
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version="+version+" -X main.mode="+mode, "-o", out, "./testdata/selfapp")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build selfapp: %v\n%s", err, b)
	}
	return out
}

// restartingSupervisor plays systemd with Restart=always (or the Windows
// service recovery actions): whenever the program exits on its own it is
// started again from the program file, up to maxStarts times.
type restartingSupervisor struct {
	exe, addr, state string
	maxStarts        int

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{} // closed when cmd has exited
	stopped bool
	starts  int
	exits   []int
	wg      sync.WaitGroup
}

func (s *restartingSupervisor) startLocked() error {
	if s.starts >= s.maxStarts {
		return fmt.Errorf("start limit %d reached", s.maxStarts)
	}
	cmd := exec.Command(s.exe, s.addr, s.state)
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	s.starts++
	done := make(chan struct{})
	s.cmd, s.done = cmd, done
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = cmd.Wait()
		close(done)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.exits = append(s.exits, cmd.ProcessState.ExitCode())
		if s.cmd != cmd || s.stopped {
			return // killed on purpose
		}
		s.cmd = nil
		time.Sleep(100 * time.Millisecond)
		_ = s.startLocked()
	}()
	return nil
}

func (s *restartingSupervisor) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked()
}

// restart kills the running instance (by its PID) and starts the program
// file again: what "systemctl restart" does.
func (s *restartingSupervisor) restart(context.Context) error {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.cmd = nil
	s.mu.Unlock()
	if cmd != nil {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("pid %d did not exit", cmd.Process.Pid)
		}
	}
	return s.start()
}

func (s *restartingSupervisor) close() {
	s.mu.Lock()
	s.stopped = true
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil {
		_ = cmd.Process.Kill()
	}
	s.wg.Wait()
}

func (s *restartingSupervisor) exitCodes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.exits...)
}

// TestIntegrationSelfConfirm exercises the in-process path (Attach + Run,
// no external probe) with real binaries under a restarting supervisor: a
// new version that crash-loops is rolled back by its own Recover after
// MaxTrialBoots start-ups, one whose self check fails is rolled back by
// Trial.Confirm, and a good one confirms itself.
func TestIntegrationSelfConfirm(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not found")
	}
	bin := t.TempDir()
	v1 := buildSelfApp(t, bin, "1.0.0", "good")
	builds := map[string]string{
		"crash": buildSelfApp(t, bin, "1.1.0", "crash"),
		"sick":  buildSelfApp(t, bin, "1.1.0", "sick"),
		"good":  buildSelfApp(t, bin, "1.1.0", "good"),
	}
	v1Bytes := readFile(t, v1)
	pub, priv, _ := GenerateKey()
	addr := freeAddr(t)
	probe := HTTPProbe{URL: "http://" + addr + "/health",
		Client: &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}}

	for _, mode := range []string{"crash", "sick", "good"} {
		t.Run(mode, func(t *testing.T) {
			relDir := t.TempDir()
			srv := httptest.NewServer(http.StripPrefix("/feeder", ReleaseHandler{Dir: relDir}))
			defer srv.Close()
			if _, err := Publish(PublishOptions{Product: "airview-feeder", Version: "1.1.0", Channel: ChannelStable,
				Artifacts: map[string]string{CurrentTarget(): builds[mode]}, OutDir: relDir, Key: priv}); err != nil {
				t.Fatal(err)
			}
			app := t.TempDir()
			if r, err := filepath.EvalSymlinks(app); err == nil {
				app = r
			}
			exe := filepath.Join(app, "airview-feeder"+exeSuffix())
			if err := os.WriteFile(exe, v1Bytes, 0o755); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(app, "state")
			sup := &restartingSupervisor{exe: exe, addr: addr, state: state, maxStarts: 12}
			if err := sup.start(); err != nil {
				t.Fatal(err)
			}
			defer sup.close()
			if err := waitUp(t, probe, "1.0.0"); err != nil {
				t.Fatalf("v1 not healthy: %v", err)
			}

			u, err := New(Options{Product: "airview-feeder", CurrentVersion: "1.0.0", Exe: exe, StateDir: state,
				Settings:  Settings{ManifestURL: srv.URL + "/feeder/manifest.json", Keys: []string{pub.String()}},
				Restarter: RestarterFunc(sup.restart), Logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			res, err := u.Update(context.Background(), UpdateOptions{})
			if err != nil || res.Outcome != OutcomePending {
				t.Fatalf("update: %+v %v", res, err)
			}

			// Wait for the trial to settle.
			deadline := time.Now().Add(2 * testUpTimeout())
			var j Journal
			for time.Now().Before(deadline) {
				if j, err = loadJournal(state); err == nil && j.Phase != PhaseSwapped {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			want, wantBytes, wantPhase := "1.0.0", v1Bytes, PhaseRolledBack
			if mode == "good" {
				want, wantBytes, wantPhase = "1.1.0", readFile(t, builds["good"]), PhaseCommitted
			}
			if j.Phase != wantPhase {
				t.Fatalf("journal phase %q (%s), want %q; exits %v", j.Phase, j.Result, wantPhase, sup.exitCodes())
			}
			if !bytes.Equal(readFile(t, exe), wantBytes) {
				t.Fatal("wrong binary in place")
			}
			if err := waitUp(t, probe, want); err != nil {
				t.Fatalf("version %s not running: %v (exits %v)", want, err, sup.exitCodes())
			}
			t.Logf("%s: %s; exit codes %v", j.Phase, j.Result, sup.exitCodes())
		})
	}
}
