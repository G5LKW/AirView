package sdr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// RTL-SDR access on Windows (DESIGN §12.3, phase 5) goes through rtl_tcp, a
// separate program of the GPL-licensed rtl-sdr project that the user
// installs. AirView never ships, links or downloads it; it only starts the
// user's copy as a child process, unmodified, and talks to it over TCP. The
// options below are rtl_tcp's documented command-line flags (its -h text):
// -a listen address, -p port, -d device index, -s sample rate, -f frequency.
// Gain, ppm and the rest are set over the protocol by the Receiver, so they
// do not depend on the version's flags.

// ChildOptions describe how to run the user's rtl_tcp.
type ChildOptions struct {
	// Path is the rtl_tcp executable. Empty: FindRTLTCP's result.
	Path string
	// Addr is the listen address host:port (default "127.0.0.1:1234";
	// keep it on loopback: rtl_tcp has no authentication).
	Addr string
	// Device is the dongle index (-d, default 0).
	Device int
	// SampleRate and FrequencyHz are passed at start-up as well.
	SampleRate  uint32
	FrequencyHz uint32
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
	// RestartMin / RestartMax bound the restart back-off (1 s / 60 s).
	RestartMin, RestartMax time.Duration
	// Logf receives the child's output lines and lifecycle messages.
	Logf func(format string, args ...any)
}

// FindRTLTCP looks for an rtl_tcp executable on PATH and next to the
// running program. It returns "" when there is none.
func FindRTLTCP() string {
	name := "rtl_tcp"
	if runtime.GOOS == "windows" {
		name = "rtl_tcp.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		for _, dir := range []string{filepath.Dir(exe), filepath.Join(filepath.Dir(exe), "rtl-sdr")} {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

// Args returns the command line arguments for the options.
func (o ChildOptions) Args() ([]string, error) {
	addr := o.Addr
	if addr == "" {
		addr = "127.0.0.1:1234"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("sdr: rtl_tcp address %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	sr, f := o.SampleRate, o.FrequencyHz
	if sr == 0 {
		sr = 2400000
	}
	if f == 0 {
		f = 1090000000
	}
	args := []string{"-a", host, "-p", port, "-d", strconv.Itoa(o.Device),
		"-s", strconv.FormatUint(uint64(sr), 10), "-f", strconv.FormatUint(uint64(f), 10)}
	return append(args, o.ExtraArgs...), nil
}

// RunChild runs rtl_tcp and restarts it when it exits, until ctx is
// cancelled (the child is then killed). ready, when not nil, is called each
// time the child is accepting connections.
func RunChild(ctx context.Context, o ChildOptions, ready func()) error {
	path := o.Path
	if path == "" {
		path = FindRTLTCP()
	}
	if path == "" {
		return errors.New("sdr: rtl_tcp not found; install the rtl-sdr tools (a separate GPL program) and set its path")
	}
	args, err := o.Args()
	if err != nil {
		return err
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	minB, maxB := o.RestartMin, o.RestartMax
	if minB == 0 {
		minB = time.Second
	}
	if maxB == 0 {
		maxB = time.Minute
	}
	addr := o.Addr
	if addr == "" {
		addr = "127.0.0.1:1234"
	}
	backoff := minB
	for {
		start := time.Now()
		cmd := exec.CommandContext(ctx, path, args...)
		configureChild(cmd)
		out, _ := cmd.StdoutPipe()
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("sdr: starting %s: %w", path, err)
		}
		logf("sdr: started %s %v (pid %d)", path, args, cmd.Process.Pid)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyLines(out, func(l string) { logf("rtl_tcp: %s", l) })
		}()
		if ready != nil {
			go waitListening(ctx, addr, ready)
		}
		wg.Wait()
		err := cmd.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logf("sdr: rtl_tcp exited (%v); restarting in %s", err, backoff)
		if time.Since(start) > 2*time.Minute {
			backoff = minB
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxB {
			backoff = maxB
		}
	}
}

func copyLines(r io.Reader, fn func(string)) {
	if r == nil {
		return
	}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fn(sc.Text())
	}
}

// waitListening polls the port until the child accepts a connection. It
// does not connect for real (rtl_tcp serves one client at a time and would
// start streaming), it only checks with a dial that is closed at once; the
// server treats that as a client that left.
func waitListening(ctx context.Context, addr string, ready func()) {
	for i := 0; i < 100; i++ {
		if ctx.Err() != nil {
			return
		}
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			ready()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
