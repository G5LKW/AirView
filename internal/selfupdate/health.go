package selfupdate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProbeResult is one health observation of the restarted program.
type ProbeResult struct {
	// Version the program reports ("" = the probe cannot tell).
	Version string
	// Instance identifies the running process (PID, start counter...);
	// a change during the health window means the program restarted,
	// which counts as a failure. "" = unknown.
	Instance string
	// Uptime as reported by the program; a decrease means a restart.
	// Negative = unknown.
	Uptime time.Duration
}

// Probe observes the restarted program. An error means "not healthy now".
type Probe interface {
	Probe(ctx context.Context) (ProbeResult, error)
}

// ProbeFunc adapts a function to Probe.
type ProbeFunc func(ctx context.Context) (ProbeResult, error)

// Probe implements Probe.
func (f ProbeFunc) Probe(ctx context.Context) (ProbeResult, error) { return f(ctx) }

// HTTPProbe GETs a health URL. The reply must have one of the accepted
// status codes; if it is a JSON object, its "version" field is the
// reported version and "uptimeS" (seconds) the uptime. This matches
// airview's /healthz and airview-feeder's /api/health (which answers 503
// while an uplink is not connected: accept 503 there, the process itself is
// up and reporting its version).
type HTTPProbe struct {
	URL    string
	Client *http.Client // default: 5 s timeout
	// Accept lists the status codes that count as alive (default 200).
	Accept []int
	// Header is added to the request (e.g. X-AirView-Feeder).
	Header http.Header
}

// Probe implements Probe.
func (p HTTPProbe) Probe(ctx context.Context) (ProbeResult, error) {
	res := ProbeResult{Uptime: -1}
	c := p.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return res, err
	}
	for k, v := range p.Header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	accept := p.Accept
	if len(accept) == 0 {
		accept = []int{http.StatusOK}
	}
	ok := false
	for _, c := range accept {
		ok = ok || c == resp.StatusCode
	}
	if !ok {
		return res, fmt.Errorf("health %s: HTTP %d", redactURL(p.URL), resp.StatusCode)
	}
	var doc struct {
		Version string   `json:"version"`
		UptimeS *float64 `json:"uptimeS"`
		PID     *int     `json:"pid"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &doc) == nil {
		res.Version = doc.Version
		if doc.UptimeS != nil {
			res.Uptime = time.Duration(*doc.UptimeS * float64(time.Second))
		}
		if doc.PID != nil {
			res.Instance = strconv.Itoa(*doc.PID)
		}
	}
	return res, nil
}

// Runner runs a command and returns its standard output (tests fake it).
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs real commands.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// SystemdProbe checks that a systemd unit is active and has not been
// restarted (MainPID and NRestarts are the instance).
type SystemdProbe struct {
	Unit string
	Run  Runner // default ExecRunner
}

// Probe implements Probe.
func (p SystemdProbe) Probe(ctx context.Context) (ProbeResult, error) {
	res := ProbeResult{Uptime: -1}
	run := p.Run
	if run == nil {
		run = ExecRunner
	}
	out, err := run(ctx, "systemctl", "show", p.Unit, "-p", "ActiveState", "-p", "SubState", "-p", "MainPID", "-p", "NRestarts")
	if err != nil {
		return res, err
	}
	kv := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			kv[k] = strings.TrimSpace(v)
		}
	}
	if kv["ActiveState"] != "active" {
		return res, fmt.Errorf("%s is %s/%s", p.Unit, kv["ActiveState"], kv["SubState"])
	}
	if kv["MainPID"] == "" || kv["MainPID"] == "0" {
		return res, fmt.Errorf("%s has no main process", p.Unit)
	}
	res.Instance = "pid=" + kv["MainPID"] + ",restarts=" + kv["NRestarts"]
	return res, nil
}

// AllProbes combines probes: all must succeed; the first non-empty version
// wins, instances are concatenated, the smallest known uptime is kept.
type AllProbes []Probe

// Probe implements Probe.
func (ps AllProbes) Probe(ctx context.Context) (ProbeResult, error) {
	out := ProbeResult{Uptime: -1}
	var inst []string
	for _, p := range ps {
		r, err := p.Probe(ctx)
		if err != nil {
			return out, err
		}
		if out.Version == "" {
			out.Version = r.Version
		}
		if r.Instance != "" {
			inst = append(inst, r.Instance)
		}
		if r.Uptime >= 0 && (out.Uptime < 0 || r.Uptime < out.Uptime) {
			out.Uptime = r.Uptime
		}
	}
	out.Instance = strings.Join(inst, ";")
	return out, nil
}

// HealthPolicy says when a restarted program counts as healthy.
type HealthPolicy struct {
	// Window is the whole observation period after the restart (default
	// 60 s). The program must answer within StartTimeout and then stay
	// healthy until the window ends.
	Window time.Duration
	// StartTimeout bounds the wait for the first healthy answer (default
	// Window/2).
	StartTimeout time.Duration
	// Interval between probes (default Window/10, at most 2 s).
	Interval time.Duration
	// MaxFailures is the number of consecutive failed probes tolerated
	// after the first healthy one (default 2: a probe can time out under
	// load without a real failure).
	MaxFailures int
	// ExpectVersion, when set, must equal the reported version (when the
	// probe reports one).
	ExpectVersion string
}

func (h HealthPolicy) normalized() HealthPolicy {
	if h.Window <= 0 {
		h.Window = DefaultHealthWindowS * time.Second
	}
	if h.StartTimeout <= 0 || h.StartTimeout > h.Window {
		h.StartTimeout = h.Window / 2
	}
	if h.Interval <= 0 {
		h.Interval = min(2*time.Second, max(h.Window/10, 10*time.Millisecond))
	}
	if h.MaxFailures <= 0 {
		h.MaxFailures = 2
	}
	return h
}

// ErrUnhealthy wraps every health-check failure.
var ErrUnhealthy = errors.New("health check failed")

// WaitHealthy observes p according to the policy and returns nil when the
// program was healthy for the whole window.
func WaitHealthy(ctx context.Context, p Probe, pol HealthPolicy) error {
	pol = pol.normalized()
	start := time.Now()
	end := start.Add(pol.Window)
	var (
		first     bool
		instance  string
		lastUp    time.Duration = -1
		fails     int
		lastErr   error
		successes int
	)
	tick := time.NewTicker(pol.Interval)
	defer tick.Stop()
	for {
		pctx, cancel := context.WithTimeout(ctx, pol.Interval*2+time.Second)
		r, err := p.Probe(pctx)
		cancel()
		if err == nil && pol.ExpectVersion != "" && r.Version != "" && r.Version != pol.ExpectVersion {
			err = fmt.Errorf("the program reports version %s, expected %s", r.Version, pol.ExpectVersion)
		}
		if err == nil && first {
			if instance != "" && r.Instance != "" && r.Instance != instance {
				return fmt.Errorf("%w: the program restarted during the health window (%s -> %s)", ErrUnhealthy, instance, r.Instance)
			}
			if lastUp >= 0 && r.Uptime >= 0 && r.Uptime+time.Second < lastUp {
				return fmt.Errorf("%w: the program restarted during the health window (uptime %v -> %v)", ErrUnhealthy, lastUp.Round(time.Second), r.Uptime.Round(time.Second))
			}
		}
		now := time.Now()
		if err != nil {
			lastErr = err
			if first {
				fails++
				if fails > pol.MaxFailures {
					return fmt.Errorf("%w: %v", ErrUnhealthy, err)
				}
			} else if now.Sub(start) >= pol.StartTimeout {
				return fmt.Errorf("%w: no healthy answer within %v: %v", ErrUnhealthy, pol.StartTimeout, err)
			}
		} else {
			if !first {
				first = true
			}
			fails = 0
			successes++
			if r.Instance != "" {
				instance = r.Instance
			}
			if r.Uptime >= 0 {
				lastUp = r.Uptime
			}
		}
		if !now.Before(end) {
			if !first {
				return fmt.Errorf("%w: never healthy: %v", ErrUnhealthy, lastErr)
			}
			if successes < 2 {
				return fmt.Errorf("%w: too few healthy answers", ErrUnhealthy)
			}
			if fails > 0 {
				return fmt.Errorf("%w: failing at the end of the window: %v", ErrUnhealthy, lastErr)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
