package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Restarter stops the running instance of the program and starts the
// program file again (after a swap: the new binary; after a rollback: the
// previous one).
type Restarter interface {
	Restart(ctx context.Context) error
}

// RestarterFunc adapts a function to Restarter.
type RestarterFunc func(ctx context.Context) error

// Restart implements Restarter.
func (f RestarterFunc) Restart(ctx context.Context) error { return f(ctx) }

// SystemdRestarter runs "systemctl restart <unit>" (needs root, or a polkit
// rule). Used by the root-run "update" command and update timers on Linux.
type SystemdRestarter struct {
	Unit string
	Run  Runner // default ExecRunner
}

// Restart implements Restarter.
func (r SystemdRestarter) Restart(ctx context.Context) error {
	run := r.Run
	if run == nil {
		run = ExecRunner
	}
	_, err := run(ctx, "systemctl", "restart", r.Unit)
	return err
}

// ProcessRestarter restarts a program that no service manager supervises:
// Stop ends the current instance, then Path is started with Args, Env and
// Dir; Started receives the new process (its owner must Wait for it).
type ProcessRestarter struct {
	Path    string
	Args    []string
	Env     []string
	Dir     string
	Stop    func(ctx context.Context) error
	Started func(cmd *exec.Cmd)
}

// Restart implements Restarter.
func (r ProcessRestarter) Restart(ctx context.Context) error {
	if r.Stop != nil {
		if err := r.Stop(ctx); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
	}
	cmd := exec.Command(r.Path, r.Args...)
	cmd.Env = r.Env
	cmd.Dir = r.Dir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", r.Path, err)
	}
	if r.Started != nil {
		r.Started(cmd)
	} else {
		go cmd.Wait()
	}
	return nil
}

// EnvWaitPID is set by SelfRestarter on Windows for the new process: Recover
// waits (up to 30 s) for that PID to exit, so the new instance does not
// race the old one for listening ports and lock files.
const EnvWaitPID = "AIRVIEW_SELFUPDATE_WAIT_PID"

// SelfRestarter restarts the calling process in place. On POSIX it
// execve(2)s the program file (same PID, so systemd, a supervisor or a
// terminal session do not notice); Restart does not return on success.
// On Windows it starts the program file as a new process with the same
// arguments and calls Exit, which must end this process (for example by
// cancelling the program's main context and returning from main).
//
// Before is called right before the exec/start (flush logs, close
// listeners, release locks). Args defaults to os.Args, Env to os.Environ().
type SelfRestarter struct {
	Exe    string
	Args   []string
	Env    []string
	Before func()
	Exit   func()
}

// ErrExitRequested is returned by SelfRestarter.Restart on Windows after
// the new process was started and Exit was called.
var ErrExitRequested = errors.New("restart: new process started, this one must exit")

func (r SelfRestarter) argsEnv() ([]string, []string) {
	args, env := r.Args, r.Env
	if args == nil {
		args = os.Args
	}
	if env == nil {
		env = os.Environ()
	}
	return args, env
}

// waitPIDFromEnv waits for the process named by EnvWaitPID to exit.
func waitPIDFromEnv(getenv func(string) string, max time.Duration) {
	s := getenv(EnvWaitPID)
	if s == "" {
		return
	}
	pid, err := strconv.Atoi(s)
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return
	}
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(100 * time.Millisecond)
	}
}
