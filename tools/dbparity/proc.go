package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// instance is one AirView process under test: a binary, its own scratch
// data directory and HTTP port. Only processes started here are ever
// stopped (by the PID this harness got from exec).
type instance struct {
	Label string // "old" / "new" (report and log names)
	Exe   string
	Dir   string // data directory (scratch)
	Port  int

	cmd    *exec.Cmd // the process started (on Windows: the launcher)
	pid    int       // the AirView process
	logf   *os.File
	done   chan error
	exited error
	began  time.Time // when the current process was started
}

// errNoGraceful: the platform cannot stop AirView gracefully (Windows, see
// proc_windows.go); Stop then waits quiesce and terminates the process.
var errNoGraceful = errors.New("no graceful stop for a windowless airview.exe on Windows")

// quiesce covers one tracker flush period (15 s, internal/tracker
// flushEveryMs) plus the store writer's commit.
const quiesce = 20 * time.Second

// childEnv is the environment of an instance: the harness' own minus every
// AIRVIEW_* variable (an operator's AIRVIEW_HTTP_PORT, AIRVIEW_DATA or
// AIRVIEW_GENERATE_ADMIN_PASSWORD would change what is compared) and
// GOMEMLIMIT.
func childEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k := strings.ToUpper(kv[:max(0, strings.IndexByte(kv, '='))])
		if strings.HasPrefix(k, "AIRVIEW_") || k == "GOMEMLIMIT" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Start launches "airview serve -data dir -port n" with its console output
// appended to <dir>/../<label>.console.log.
func (in *instance) Start() error {
	if in.cmd != nil {
		return fmt.Errorf("%s: already started", in.Label)
	}
	if err := os.MkdirAll(in.Dir, 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(filepath.Dir(in.Dir), in.Label+".console.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintf(lf, "\n==== %s start %s -data %s -port %d\n", time.Now().Format(time.RFC3339), in.Exe, in.Dir, in.Port)
	args := []string{"serve", "-data", in.Dir, "-port", strconv.Itoa(in.Port)}
	cmd, pidOf, err := launchCommand(in.Exe, args, lf)
	if err != nil {
		lf.Close()
		return err
	}
	cmd.Env = childEnv()
	cmd.Dir = in.Dir
	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("%s: %w", in.Label, err)
	}
	in.cmd, in.logf, in.done, in.exited = cmd, lf, make(chan error, 1), nil
	in.began = time.Now()
	pid, err := pidOf()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		in.cleanup()
		return fmt.Errorf("%s: %w", in.Label, err)
	}
	in.pid = pid
	go func() { in.done <- cmd.Wait() }()
	logf("%s: started pid %d (%s, port %d)", in.Label, pid, filepath.Base(in.Exe), in.Port)
	return nil
}

// PID of the AirView process (0 when not running). On Windows this is the
// child of the launcher (proc_windows.go), not cmd.Process.
func (in *instance) PID() int {
	if in.cmd == nil {
		return 0
	}
	return in.pid
}

// URL of the instance's web server.
func (in *instance) URL() string { return fmt.Sprintf("http://127.0.0.1:%d", in.Port) }

// WaitHealthy polls /healthz until it answers 200.
func (in *instance) WaitHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	c := &http.Client{Timeout: 3 * time.Second}
	for {
		select {
		case err := <-in.done:
			in.done <- err
			return fmt.Errorf("%s: exited before it became healthy: %v (see %s.console.log)", in.Label, err, in.Label)
		default:
		}
		resp, err := c.Get(in.URL() + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: not healthy after %s (last error %v)", in.Label, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Stop ends the process gracefully (console break / SIGTERM: the server
// flushes the tracker and closes the databases) and kills it when it has
// not exited within grace.
func (in *instance) Stop(grace time.Duration) error {
	if in.cmd == nil {
		return nil
	}
	defer in.cleanup()
	if err := interruptProcess(in.cmd.Process, in.pid); err != nil {
		if errors.Is(err, errNoGraceful) {
			// Windows (proc_windows.go): let the tracker's periodic flush
			// (every 15 s) and the store writer commit, then terminate.
			time.Sleep(quiesce)
			logf("%s: quiesced %s, terminating pid %d (%v)", in.Label, quiesce, in.PID(), err)
			return in.Kill()
		}
		logf("%s: graceful stop not possible (%v); killing pid %d", in.Label, err, in.PID())
		return in.Kill()
	}
	select {
	case err := <-in.done:
		logf("%s: stopped (pid %d, %s)", in.Label, in.PID(), exitText(err))
		return nil
	case <-time.After(grace):
		logf("%s: did not stop within %s; killing pid %d", in.Label, grace, in.PID())
		_ = killPID(in.pid)
		_ = in.cmd.Process.Kill()
		<-in.done
		return errors.New(in.Label + ": graceful stop timed out")
	}
}

// Kill terminates the process at once (crash simulation).
func (in *instance) Kill() error {
	if in.cmd == nil {
		return nil
	}
	defer in.cleanup()
	pid := in.PID()
	_ = killPID(pid) // fails only when the process is already gone; done tells
	if in.cmd.Process.Pid != pid {
		select {
		case <-in.done:
		case <-time.After(10 * time.Second):
			_ = in.cmd.Process.Kill()
			<-in.done
		}
	} else {
		<-in.done
	}
	logf("%s: killed pid %d", in.Label, pid)
	return nil
}

func (in *instance) cleanup() {
	if in.logf != nil {
		in.logf.Close()
	}
	in.cmd, in.logf, in.pid = nil, nil, 0
}

// killPID terminates a process at once (TerminateProcess / SIGKILL).
func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func exitText(err error) string {
	if err == nil {
		return "exit 0"
	}
	return err.Error()
}

// freePort returns a currently free TCP port on 127.0.0.1.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// portFree reports whether port can be bound on 127.0.0.1 and on all
// interfaces right now.
func portFree(port int) bool {
	for _, h := range []string{"127.0.0.1", ""} {
		ln, err := net.Listen("tcp", net.JoinHostPort(h, strconv.Itoa(port)))
		if err != nil {
			return false
		}
		ln.Close()
	}
	return true
}
