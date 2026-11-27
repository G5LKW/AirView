//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
)

// guiAvailable: there is no desktop window outside Windows.
const guiAvailable = false

func platformInit(wantConsole bool) {}

// fatalMessage reports a start-up failure.
func fatalMessage(text string, headless bool) {
	fmt.Fprintln(os.Stderr, "airview: "+text)
}

// acquireInstance: no single-instance guard here (the Pi runs AirView as one
// systemd service).
func acquireInstance(dir string) (release func(), already bool) {
	return func() {}, false
}

func activateRunningInstance(dir string, show bool) {}

// runUI runs headless until SIGINT / SIGTERM.
func runUI(a *app, gui bool) int {
	fmt.Printf("%s is running: %s\n", "AirView", a.url)
	if a.flags.open {
		if err := openBrowser(a.startURL()); err != nil {
			a.log.Warn("could not open a browser", "err", err)
		}
	}
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var s os.Signal
	select {
	case s = <-sig:
	case code := <-a.exitReq:
		signal.Stop(sig)
		return code
	}
	a.log.Info("signal received", "signal", s.String())
	// A second signal during shutdown kills the process the default way.
	signal.Stop(sig)
	return 0
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
