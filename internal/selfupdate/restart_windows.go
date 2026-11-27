//go:build windows

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Restart implements Restarter: start the program file as a new process
// (it waits for this one to exit, see EnvWaitPID) and call Exit.
func (r SelfRestarter) Restart(context.Context) error {
	args, env := r.argsEnv()
	if r.Exit == nil {
		return errors.New("SelfRestarter on Windows needs Exit")
	}
	env = append(append([]string(nil), env...), EnvWaitPID+"="+strconv.Itoa(os.Getpid()))
	cmd := exec.Command(r.Exe, args[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if r.Before != nil {
		r.Before()
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", r.Exe, err)
	}
	_ = cmd.Process.Release()
	r.Exit()
	return ErrExitRequested
}

// processAlive reports whether pid is a running process.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// WindowsServiceRestarter stops and starts a Windows service through the
// service control manager (needs the rights to control the service:
// administrators, or LocalSystem).
type WindowsServiceRestarter struct {
	Name string
	// StopTimeout bounds the wait for the service to stop (default 60 s).
	StopTimeout time.Duration
}

// Restart implements Restarter.
func (r WindowsServiceRestarter) Restart(ctx context.Context) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(r.Name)
	if err != nil {
		return fmt.Errorf("service %s: %w", r.Name, err)
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("service %s: %w", r.Name, err)
	}
	if st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil && st.State != svc.StopPending {
			return fmt.Errorf("stop %s: %w", r.Name, err)
		}
		timeout := r.StopTimeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		deadline := time.Now().Add(timeout)
		for {
			st, err = s.Query()
			if err != nil {
				return fmt.Errorf("service %s: %w", r.Name, err)
			}
			if st.State == svc.Stopped {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("service %s did not stop within %v", r.Name, timeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start %s: %w", r.Name, err)
	}
	return nil
}

// WindowsServiceProbe checks that a service is running; its process ID is
// the instance (a restart by the recovery actions changes it).
type WindowsServiceProbe struct{ Name string }

// Probe implements Probe.
func (p WindowsServiceProbe) Probe(context.Context) (ProbeResult, error) {
	res := ProbeResult{Uptime: -1}
	m, err := mgr.Connect()
	if err != nil {
		return res, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(p.Name)
	if err != nil {
		return res, err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return res, err
	}
	if st.State != svc.Running {
		return res, fmt.Errorf("service %s is not running (state %d)", p.Name, st.State)
	}
	res.Instance = "pid=" + strconv.FormatUint(uint64(st.ProcessId), 10)
	return res, nil
}
