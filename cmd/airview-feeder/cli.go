package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"airview/internal/avf"
	"airview/internal/feederui"
	"airview/internal/model"
	"airview/internal/uplink"
)

// ---- talking to the running feeder ------------------------------------------

// baseURL turns ui.addr into the URL a local client uses (a wildcard
// listener is reached on loopback).
func baseURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/"
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// localClient is the HTTP client of status / claim (no proxy: the page is
// local).
var localClient = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 30 * time.Second}

func localCall(ctx context.Context, method, url string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(feederui.CSRFHeader, "1")
	}
	resp, err := localClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusServiceUnavailable {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return &apiError{resp.StatusCode, e.Error}
		}
		return &apiError{resp.StatusCode, strings.TrimSpace(string(b))}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

// loadForCLI loads the configuration with the process environment.
func loadForCLI(c common, stderr io.Writer) *loadedConfig {
	lc, err := loadConfig(loadOptions{File: c.config, Environ: os.Environ()})
	if err != nil && lc.Exists {
		fmt.Fprintf(stderr, "airview-feeder: %v\n", err)
	}
	return lc
}

// ---- status ------------------------------------------------------------------

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	var c common
	var asJSON bool
	var url string
	fs := newFlagSet("status", stderr)
	c.register(fs)
	fs.BoolVar(&asJSON, "json", false, "print the raw status JSON")
	fs.StringVar(&url, "url", "", "status page `URL` (default: from ui.addr)")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	c.resolve()
	lc := loadForCLI(c, stderr)
	if url == "" {
		url = baseURL(lc.Eff.UI.Addr)
	}
	url = strings.TrimSuffix(url, "/") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var s feederui.Snapshot
	if err := localCall(ctx, http.MethodGet, url+"api/status", nil, &s); err != nil {
		fmt.Fprintf(stdout, "airview-feeder is not running (or its status page %s is not reachable: %v)\n", url, err)
		fmt.Fprintf(stdout, "config: %s (%s)\n", c.config, map[bool]string{true: "present", false: "missing"}[lc.Exists])
		for _, u := range lc.Eff.Uplinks {
			fmt.Fprintf(stdout, "aggregator %s: %s:%d%s\n", u.ID, u.Host, u.Port, map[bool]string{true: " (disabled)"}[u.Disabled])
		}
		return 3
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s)
		return 0
	}
	printStatus(stdout, s, url)
	for _, u := range s.Uplinks {
		if u.State != uplink.StateConnected {
			return 1
		}
	}
	return 0
}

func printStatus(w io.Writer, s feederui.Snapshot, url string) {
	fmt.Fprintf(w, "airview-feeder %s  feeder %s  up %s\n", s.Version, s.FeederID, time.Since(time.UnixMilli(s.StartedMs)).Round(time.Second))
	fmt.Fprintf(w, "status page: %s\n", url)
	for _, r := range s.Receivers {
		state := "not connected"
		if r.Connected {
			state = "connected"
		}
		if r.InputType == model.FeedAuto && r.Detected == "" {
			state = "searching (auto-detect)"
		}
		src := r.Detected
		if src == "" {
			src = r.InputType
		}
		fmt.Fprintf(w, "receiver %d %q: %s, %s %s, %.1f msg/s, %d aircraft, clock %s\n",
			r.RxNo, r.Name, state, src, r.Addr, r.Stats.FPS, r.Stats.Aircraft, orDash(r.ClockClass))
		if r.LastError != "" && !r.Connected {
			fmt.Fprintf(w, "  last error: %s\n", r.LastError)
		}
	}
	if len(s.Uplinks) == 0 {
		fmt.Fprintln(w, "no aggregator configured (run airview-feeder setup)")
	}
	for _, u := range s.Uplinks {
		state := u.State
		if u.Approval == avf.StatusPending {
			state += ", waiting for approval"
		}
		fmt.Fprintf(w, "aggregator %s (%s:%d): %s, mode %s, frames sent %d (late %d), results %d\n",
			u.ID, u.Host, u.Port, state, orDash(u.Mode), u.Counters.FramesSent, u.Counters.FramesLate, u.Counters.ResultsIn)
		if u.LastError != "" && u.State != uplink.StateConnected {
			fmt.Fprintf(w, "  last error: %s", u.LastError)
			if u.ErrorCode != "" {
				fmt.Fprintf(w, " (%s)", u.ErrorCode)
			}
			fmt.Fprintln(w)
		}
	}
	if s.Results.BeastPort > 0 {
		fmt.Fprintf(w, "MLAT results: Beast on 127.0.0.1:%d (%d clients), %d received\n", s.Results.BeastPort, s.Results.Clients, s.Results.Received)
	}
	for _, n := range s.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- claim -------------------------------------------------------------------

func cmdClaim(args []string, stdout, stderr io.Writer) int {
	var c common
	var id string
	var rx int
	var asJSON bool
	var timeout time.Duration
	fs := newFlagSet("claim", stderr)
	c.register(fs)
	fs.StringVar(&id, "uplink", "", "aggregator (uplink `id`; default: the first)")
	fs.IntVar(&rx, "rx", 0, "receiver `number` (default: the first)")
	fs.BoolVar(&asJSON, "json", false, "print {\"code\",\"expiresMs\"} as JSON")
	fs.DurationVar(&timeout, "timeout", 30*time.Second, "how long to wait for the aggregator")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	c.resolve()
	lc := loadForCLI(c, stderr)
	if len(lc.Eff.Uplinks) == 0 {
		fmt.Fprintln(stderr, "airview-feeder: no aggregator is configured (run airview-feeder setup)")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var res feederui.ClaimResponse
	url := baseURL(lc.Eff.UI.Addr)
	err := localCall(ctx, http.MethodPost, url+"api/claim", feederui.ClaimRequest{ID: id, RxNo: rx}, &res)
	var ae *apiError
	if err != nil && !errors.As(err, &ae) {
		// The feeder is not running (or its page is unreachable): connect
		// with the feeder identity for this one request, unless a running
		// process holds the identity (a second session would be refused as
		// a clone).
		if dataDirLocked(c.data) {
			fmt.Fprintf(stderr, "airview-feeder: the running feeder's status page %s is not reachable (%v)\n", url, err)
			return 1
		}
		fmt.Fprintln(stderr, "the feeder is not running; connecting to the aggregator directly...")
		res, err = offlineClaim(ctx, c, lc.Eff, id, rx)
	}
	if err != nil {
		fmt.Fprintf(stderr, "airview-feeder: no claim code: %v\n", err)
		return 1
	}
	if asJSON {
		_ = json.NewEncoder(stdout).Encode(res)
		return 0
	}
	host := lc.Eff.Uplinks[0].Host
	for _, u := range lc.Eff.Uplinks {
		if u.ID == id {
			host = u.Host
		}
	}
	fmt.Fprintf(stdout, "Claim code: %s\nValid until: %s\n\nEnter it on the aggregator's claim page (for example https://%s/claim) while logged in to your account.\nThe code works once. Do not share it.\n",
		res.Code, time.UnixMilli(res.ExpiresMs).Format("2006-01-02 15:04 MST"), host)
	return 0
}

// offlineClaim opens a temporary uplink session with the feeder identity
// and asks for a code.
func offlineClaim(ctx context.Context, c common, cfg model.FeederConfig, id string, rx int) (feederui.ClaimResponse, error) {
	release, err := lockDataDir(c.data)
	if err != nil {
		return feederui.ClaimResponse{}, err
	}
	defer release()
	if rx == 0 && len(cfg.Receivers) > 0 {
		rx = cfg.Receivers[0].RxNo
	}
	m, err := uplink.NewManager(uplink.Options{Software: avf.SoftwareFeeder, Version: version, StateDir: uplinkStateDir(c.data)})
	if err != nil {
		return feederui.ClaimResponse{}, err
	}
	defer m.Close()
	var ups []model.Uplink
	for _, u := range cfg.Uplinks {
		if id == "" || u.ID == id {
			u.SpoolMB = 0 // never touch the spool from here
			ups = append(ups, u)
			if id == "" {
				id = u.ID
				break
			}
		}
	}
	if len(ups) == 0 {
		return feederui.ClaimResponse{}, uplink.ErrNoUplink
	}
	m.Apply(ups)
	for {
		st := m.Status()
		if len(st) > 0 {
			switch st[0].State {
			case uplink.StateConnected:
				code, exp, err := m.Claim(ctx, id, rx)
				return feederui.ClaimResponse{Code: code, ExpiresMs: exp.UnixMilli()}, err
			case uplink.StateStopped:
				return feederui.ClaimResponse{}, fmt.Errorf("aggregator refused the feeder: %s (%s)", st[0].LastError, st[0].ErrorCode)
			}
		}
		select {
		case <-ctx.Done():
			msg := "timed out"
			if len(st) > 0 && st[0].LastError != "" {
				msg += ": " + st[0].LastError
			}
			return feederui.ClaimResponse{}, errors.New(msg)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ---- detect ------------------------------------------------------------------

func cmdDetect(args []string, stdout, stderr io.Writer) int {
	var host string
	var port int
	var asJSON bool
	var wait time.Duration
	fs := newFlagSet("detect", stderr)
	fs.StringVar(&host, "host", "", "receiver `host` (default 127.0.0.1)")
	fs.IntVar(&port, "port", 0, "probe only this `port`")
	fs.BoolVar(&asJSON, "json", false, "print JSON")
	fs.DurationVar(&wait, "wait", 2*time.Second, "how long to listen to each port for data")
	var withSDR bool
	var sdrPort int
	fs.BoolVar(&withSDR, "sdr", false, "also look for an RTL-SDR: rtl_tcp on the host, USB dongles and their driver")
	fs.IntVar(&sdrPort, "sdr-port", 0, "rtl_tcp `port` for -sdr (default 1234)")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	ds := detect(context.Background(), host, port, wait)
	b, ok := best(ds)
	var sd *sdrDetection
	if withSDR {
		x := detectSDR(context.Background(), host, sdrPort)
		sd = &x
	}
	if asJSON {
		doc := map[string]any{"candidates": ds, "best": b, "found": ok}
		if sd != nil {
			doc["sdr"] = sd
		}
		_ = json.NewEncoder(stdout).Encode(doc)
	} else {
		for _, d := range ds {
			state := d.Err
			switch {
			case d.ok() && d.Format != "":
				state = "receiving " + d.Format
			case d.ok():
				state = "listening, no data yet"
			}
			fmt.Fprintf(stdout, "%-22s %-32s %s\n", d.Addr(), d.Label, state)
		}
		if ok {
			fmt.Fprintf(stdout, "\nwould use: %s at %s\n", b.Label, b.Addr())
		} else {
			fmt.Fprintln(stdout, "\nno receiver found")
		}
		if sd != nil {
			printSDRDetection(stdout, *sd)
		}
	}
	if !ok && (sd == nil || !sd.RTLTCP.Found) {
		return 1
	}
	return 0
}

func printSDRDetection(w io.Writer, d sdrDetection) {
	fmt.Fprintln(w, "\nRTL-SDR:")
	if d.RTLTCP.Found {
		fmt.Fprintf(w, "  rtl_tcp at %s: tuner %s, %d gain steps\n", d.RTLTCP.Addr, d.RTLTCP.Tuner, d.RTLTCP.GainSteps)
		fmt.Fprintf(w, "  use it with: airview-feeder setup -input %s -input-type rtltcp\n", d.RTLTCP.Addr)
	} else {
		fmt.Fprintf(w, "  rtl_tcp at %s: %s\n", d.RTLTCP.Addr, d.RTLTCP.Err)
	}
	for _, x := range d.Dongles {
		state := "WinUSB driver bound (ready for rtl_tcp)"
		if !x.Ready {
			state = "driver " + x.Driver + ": bind the WinUSB driver (e.g. with Zadig) before rtl_tcp can use it"
		}
		fmt.Fprintf(w, "  USB %s: %s\n", x.Device, state)
	}
	if d.USBNote != "" {
		fmt.Fprintf(w, "  %s\n", d.USBNote)
	}
	if d.Executable != "" {
		fmt.Fprintf(w, "  rtl_tcp program: %s (-sdr-spawn can start it)\n", d.Executable)
	} else {
		fmt.Fprintln(w, "  rtl_tcp program: not found (install your own rtl_tcp to use -sdr-spawn; AirView never ships it)")
	}
}

// ---- new-identity ------------------------------------------------------------

func cmdNewIdentity(args []string, stdout, stderr io.Writer) int {
	var c common
	var yes bool
	fs := newFlagSet("new-identity", stderr)
	c.register(fs)
	fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	c.resolve()
	if !yes {
		fmt.Fprintf(stdout, "This replaces the feeder key in %s. The aggregator will see a new feeder that must be claimed again.\nType \"yes\" to continue: ", c.data)
		var ans string
		_, _ = fmt.Fscanln(os.Stdin, &ans)
		if ans != "yes" {
			fmt.Fprintln(stdout, "cancelled")
			return 1
		}
	}
	release, err := lockDataDir(c.data)
	if err != nil {
		fmt.Fprintf(stderr, "airview-feeder: %v; stop it first\n", err)
		return 1
	}
	defer release()
	id, err := uplink.NewIdentity(uplinkStateDir(c.data))
	if err != nil {
		fmt.Fprintf(stderr, "airview-feeder: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "new feeder ID: %s (UUID %s kept)\nRestart the feeder, then claim it again.\n", id.FeederID, id.UUID)
	return 0
}

// ---- uninstall-helper ---------------------------------------------------------

// ownFiles are the names the feeder creates in its data directory.
var ownFiles = []string{uplink.IdentityFile, uplink.UUIDFile, "feeder.lock", "uplinks", "state.json"}

type uninstallInfo struct {
	Binary     string   `json:"binary"`
	ConfigFile string   `json:"configFile"`
	DataDir    string   `json:"dataDir"`
	Files      []string `json:"files"` // existing files and directories the feeder owns
	Running    bool     `json:"running"`
}

func cmdUninstallHelper(args []string, stdout, stderr io.Writer) int {
	var c common
	var asJSON, purge, yes bool
	fs := newFlagSet("uninstall-helper", stderr)
	c.register(fs)
	fs.BoolVar(&asJSON, "json", false, "print JSON")
	fs.BoolVar(&purge, "purge", false, "delete the configuration, identity, state and spool (needs -yes)")
	fs.BoolVar(&yes, "yes", false, "confirm -purge")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	c.resolve()
	info := uninstallInfo{ConfigFile: c.config, DataDir: c.data, Running: dataDirLocked(c.data)}
	info.Binary, _ = os.Executable()
	if _, err := os.Stat(c.config); err == nil {
		info.Files = append(info.Files, c.config)
	}
	for _, n := range ownFiles {
		p := filepath.Join(c.data, n)
		if _, err := os.Lstat(p); err == nil {
			info.Files = append(info.Files, p)
		}
	}
	if !purge {
		if asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(info)
		} else {
			fmt.Fprintf(stdout, "binary:      %s\nconfig file: %s\ndata dir:    %s\nrunning:     %v\nfiles owned by the feeder:\n", info.Binary, info.ConfigFile, info.DataDir, info.Running)
			for _, f := range info.Files {
				fmt.Fprintf(stdout, "  %s\n", f)
			}
			fmt.Fprintln(stdout, "\nThe feeder never changes readsb, dump1090 or other feeders; removing it leaves them as they are.\n\"airview-feeder uninstall-helper -purge -yes\" deletes the files listed above (stop the service first).")
		}
		return 0
	}
	if !yes {
		fmt.Fprintln(stderr, "airview-feeder: -purge deletes the feeder identity and configuration; add -yes to confirm")
		return 2
	}
	if info.Running {
		fmt.Fprintln(stderr, "airview-feeder: the feeder is running; stop it first")
		return 1
	}
	failed := 0
	for _, f := range info.Files {
		if err := os.RemoveAll(f); err != nil {
			fmt.Fprintf(stderr, "cannot remove %s: %v\n", f, err)
			failed++
		} else if !asJSON {
			fmt.Fprintf(stdout, "removed %s\n", f)
		}
	}
	// Remove the directories only when empty and clearly ours.
	for _, d := range []string{c.data, filepath.Dir(c.config)} {
		if strings.Contains(strings.ToLower(filepath.Base(d)), "airview-feeder") || strings.EqualFold(filepath.Base(d), "AirView Feeder") {
			if os.Remove(d) == nil && !asJSON {
				fmt.Fprintf(stdout, "removed %s\n", d)
			}
		}
	}
	if asJSON {
		_ = json.NewEncoder(stdout).Encode(map[string]any{"removed": info.Files, "failed": failed})
	}
	if failed > 0 {
		return 1
	}
	return 0
}
