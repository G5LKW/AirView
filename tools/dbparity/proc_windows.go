package main

import (
	"io"
	"os"
	"os/exec"
)

// Stopping an instance on Windows.
//
// "airview serve" stops gracefully on SIGTERM / Ctrl+C. Windows has no
// SIGTERM, and airview.exe is a GUI-subsystem program: it has a console
// only when it attaches to its parent's, and it registers its control
// handler (Go runtime start) before attaching, so a console Ctrl+C /
// Ctrl+Break terminates it instead of reaching the handler (verified with
// GenerateConsoleCtrlEvent through a launcher console). The harness
// therefore stops Windows instances the way a crash or "taskkill /F" does:
// after a quiet period (input stopped, the writer's batches committed) the
// process is terminated. SQLite in WAL mode is crash-safe, and every
// comparison only reads rows up to the fixture cutoff or answers given
// before the stop, so nothing compared depends on the shutdown path. On
// Linux (Raspberry Pi, aggregator) instances get SIGTERM and stop
// gracefully.

func ensureConsole() {}

func launchCommand(exe string, args []string, log io.Writer) (*exec.Cmd, func() (int, error), error) {
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = log, log
	return cmd, func() (int, error) { return cmd.Process.Pid, nil }, nil
}

func interruptProcess(*os.Process, int) error { return errNoGraceful }

func helperMain([]string) bool { return false }
