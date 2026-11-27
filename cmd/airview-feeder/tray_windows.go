package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"

	"airview/internal/feederui"
	"airview/internal/wintray"
)

// The tray (DESIGN §12.3): "airview-feeder-tray.exe" (or "airview-feeder
// tray"), a per-user process in the notification area.
//
//   - With the service installed it shows the service state and the local
//     status page's summary, and pauses, resumes, starts and restarts the
//     service (without a UAC prompt when the service was installed with
//     -user-control, the default; otherwise Windows asks).
//   - Without the service (the portable ZIP, no administrator rights) it
//     runs the feeder itself, in the tray process, until the user quits;
//     "Install as Windows service" moves it into a service.
//   - First run: when no aggregator is configured yet it opens the settings
//     on the local status page once (browser-based setup).
//
//	airview-feeder tray [-name AirViewFeeder] [-config f] [-data d] [-url http://127.0.0.1:8099/]
//	                    [-embedded auto|off] [-signal quit|menu|refresh] [-autostart on|off]

const (
	trayRunKey     = `Software\Microsoft\Windows\CurrentVersion\Run`
	trayRunValue   = "AirView Feeder Tray"
	trayStateKey   = `Software\AirView\Feeder Tray`
	trayPollEvery  = 3 * time.Second
	trayHTTPTimout = 3 * time.Second
)

func trayClass(name string) string { return "AirViewFeederTray-" + name }

type trayOptions struct {
	c        common
	logs     logSettings
	name     string
	url      string
	embedded string // auto | off
}

func cmdTray(args []string, stdout, stderr io.Writer) int {
	var o trayOptions
	var signal, autostart string
	fs := newFlagSet("tray", stderr)
	o.c.register(fs)
	fs.StringVar(&o.name, "name", defaultServiceName, "feeder service `name`")
	fs.StringVar(&o.url, "url", "", "status page `URL` (default: found from the service or the configuration)")
	fs.StringVar(&o.embedded, "embedded", "auto", "`mode`: auto runs the feeder inside the tray when the service is not installed; off never does")
	fs.StringVar(&signal, "signal", "", "send `quit`, menu or refresh to a running tray and exit")
	fs.StringVar(&autostart, "autostart", "", "`on` or off: start the tray when you log on (this user)")
	fs.StringVar(&o.logs.Dir, "log-dir", "", "log `directory` of the tray-hosted feeder (default <data>\\logs)")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	switch signal {
	case "":
	case "quit", "menu", "refresh":
		sig := map[string]wintray.Signal{"quit": wintray.SignalQuit, "menu": wintray.SignalMenu, "refresh": wintray.SignalRefresh}[signal]
		n := wintray.SignalRunning(trayClass(o.name), sig)
		if signal == "quit" {
			// Wait until they are gone (installers replace the program next).
			for i := 0; i < 100 && n > 0 && wintray.SignalRunning(trayClass(o.name), 0) > 0; i++ {
				time.Sleep(100 * time.Millisecond)
			}
		}
		fmt.Fprintf(stdout, "%d tray(s) signalled\n", n)
		return 0
	default:
		fmt.Fprintf(stderr, "airview-feeder tray: -signal must be quit, menu or refresh\n")
		return 2
	}
	if autostart != "" {
		on := autostart == "on" || autostart == "true" || autostart == "1"
		if err := setTrayAutostart(on, trayCommand(o)); err != nil {
			fmt.Fprintf(stderr, "airview-feeder: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Tray autostart %s for this user.\n", map[bool]string{true: "on", false: "off"}[on])
		return 0
	}
	if o.embedded != "auto" && o.embedded != "off" {
		fmt.Fprintln(stderr, "airview-feeder tray: -embedded must be auto or off")
		return 2
	}

	// One tray per feeder service and user session.
	mu, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(`Local\`+trayClass(o.name)))
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if mu != 0 {
			windows.CloseHandle(mu)
		}
		wintray.SignalRunning(trayClass(o.name), wintray.SignalMenu)
		return 0
	}
	if mu != 0 {
		defer windows.CloseHandle(mu)
	}

	app := newTrayApp(o)
	if err := app.run(); err != nil {
		fmt.Fprintf(stderr, "airview-feeder tray: %v\n", err)
		wintray.MessageBox("AirView Feeder", "The tray icon could not start: "+err.Error(), wintray.MBIconError)
		return 1
	}
	return 0
}

// trayCommand is the command line that starts this tray (autostart).
func trayCommand(o trayOptions) string {
	args := []string{exePath()}
	if !strings.Contains(strings.ToLower(filepath.Base(args[0])), "-tray") {
		args = append(args, "tray")
	}
	if !strings.EqualFold(o.name, defaultServiceName) {
		args = append(args, "-name", o.name)
	}
	if o.c.config != "" {
		args = append(args, "-config", absPath(o.c.config))
	}
	if o.c.data != "" {
		args = append(args, "-data", absPath(o.c.data))
	}
	return windows.ComposeCommandLine(args)
}

// ---- state ------------------------------------------------------------------

// trayState is one poll result.
type trayState struct {
	svc       serviceInfo
	embedded  bool   // the tray hosts the feeder (no service)
	embPaused bool   // the user paused the tray-hosted feeder
	embErr    string // why the tray-hosted feeder is not running
	url       string
	snap      *feederui.Snapshot
	snapErr   string
	logDir    string
}

// running reports whether a feeder should be answering on the status page.
func (s trayState) running() bool {
	if s.embedded {
		return !s.embPaused && s.embErr == ""
	}
	return s.svc.Installed && s.svc.state == svc.Running
}

// summarize turns the state into the icon level, the tooltip and the
// status line of the menu.
func summarize(s trayState) (wintray.Level, string, string) {
	level, line := summarizeLine(s)
	tip := "AirView Feeder: " + line
	if s.snap != nil && s.running() {
		var fps float64
		var ac int
		for _, r := range s.snap.Receivers {
			fps += r.Stats.FPS
			ac += r.Stats.Aircraft
		}
		tip += fmt.Sprintf("\n%.0f msg/s, %d aircraft", fps, ac)
	}
	if r := []rune(tip); len(r) > 127 {
		tip = string(r[:126]) + "…"
	}
	return level, tip, line
}

func summarizeLine(s trayState) (wintray.Level, string) {
	switch {
	case s.embedded && s.embPaused:
		return wintray.LevelIdle, "paused"
	case s.embedded && s.embErr != "":
		return wintray.LevelError, "not running: " + s.embErr
	case !s.embedded && !s.svc.Installed:
		if s.svc.Error != "" {
			return wintray.LevelError, "service: " + s.svc.Error
		}
		return wintray.LevelIdle, "service not installed"
	case !s.embedded:
		switch s.svc.state {
		case svc.Stopped:
			if s.svc.ExitCode != 0 || s.svc.SvcCode != 0 {
				return wintray.LevelError, fmt.Sprintf("service stopped with an error (code %d/%d), see the logs", s.svc.ExitCode, s.svc.SvcCode)
			}
			return wintray.LevelIdle, "service stopped"
		case svc.Paused, svc.PausePending:
			return wintray.LevelIdle, "paused"
		case svc.StartPending, svc.ContinuePending:
			return wintray.LevelWarn, "starting"
		case svc.StopPending:
			return wintray.LevelIdle, "stopping"
		}
	}
	// Running: judge by the status page.
	if s.snap == nil {
		if s.snapErr != "" {
			return wintray.LevelWarn, "running; status page not reachable"
		}
		return wintray.LevelWarn, "starting"
	}
	return summarizeSnapshot(s.snap)
}

func summarizeSnapshot(sn *feederui.Snapshot) (wintray.Level, string) {
	if len(sn.Uplinks) == 0 {
		return wintray.LevelWarn, "no aggregator configured: open Settings"
	}
	rxUp := 0
	for _, r := range sn.Receivers {
		if r.Connected {
			rxUp++
		}
	}
	active, up, refused := 0, 0, 0
	var lastErr string
	for _, u := range sn.Uplinks {
		if u.Disabled {
			continue
		}
		active++
		switch {
		case u.State == "connected":
			up++
		case u.ErrorCode != "":
			refused++
			lastErr = u.LastError
		case u.LastError != "" && lastErr == "":
			lastErr = u.LastError
		}
	}
	lastErr = shortError(lastErr)
	switch {
	case active == 0:
		return wintray.LevelWarn, "all aggregators are disabled"
	case up == 0 && refused > 0:
		return wintray.LevelError, "refused by the aggregator: " + orDash(lastErr)
	case up == 0:
		if lastErr != "" {
			return wintray.LevelWarn, "connecting: " + lastErr
		}
		return wintray.LevelWarn, "connecting to the aggregator"
	case len(sn.Receivers) > 0 && rxUp == 0:
		return wintray.LevelWarn, "receiver not connected"
	case up < active:
		return wintray.LevelWarn, fmt.Sprintf("%d of %d aggregators connected", up, active)
	}
	if active == 1 {
		return wintray.LevelOK, "feeding"
	}
	return wintray.LevelOK, fmt.Sprintf("feeding %d aggregators", active)
}

// shortError turns a Go network error into a few words for the menu and the
// tooltip (the status page shows the full text): the last part after ": ",
// with the long Windows socket messages replaced, at most 60 characters.
func shortError(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	low := strings.ToLower(s)
	for _, m := range []struct{ has, say string }{
		{"actively refused", "connection refused"},
		{"connection refused", "connection refused"},
		{"forcibly closed", "connection reset"},
		{"connection reset", "connection reset"},
		{"i/o timeout", "timed out"},
		{"did not properly respond", "timed out"},
		{"no such host", "unknown host"},
		{"network is unreachable", "network unreachable"},
		{"unreachable network", "network unreachable"},
		{"certificate", "certificate not accepted"},
	} {
		if strings.Contains(low, m.has) {
			return m.say
		}
	}
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		s = s[i+2:]
	}
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}

// ---- the app ----------------------------------------------------------------

type trayApp struct {
	o     trayOptions
	tray  *wintray.Tray
	http  *http.Client
	kick  chan struct{}
	quit  chan struct{}
	level wintray.Level

	mu    sync.Mutex
	st    trayState
	first bool // first poll done

	embMu   sync.Mutex
	emb     *feeder
	embLock func()
	embLog  *rotatingLog
	paused  bool // embedded: paused by the user
}

func newTrayApp(o trayOptions) *trayApp {
	return &trayApp{
		o:     o,
		http:  &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: trayHTTPTimout},
		kick:  make(chan struct{}, 1),
		quit:  make(chan struct{}),
		level: -1,
	}
}

func (a *trayApp) run() error {
	defer a.stopEmbedded()
	return wintray.Run(wintray.Options{
		ClassName: trayClass(a.o.name),
		Tooltip:   "AirView Feeder",
		Icon:      func(size int) []byte { return wintray.PNG(size, wintray.LevelIdle) },
		Menu:      a.menu,
		OnBalloonClick: func() {
			a.openStatus("")
		},
		OnSignal: func(s wintray.Signal) {
			if s == wintray.SignalRefresh {
				a.refresh()
			}
		},
		OnEndSession: a.stopEmbedded,
		Ready: func(t *wintray.Tray) {
			a.tray = t
			go a.pollLoop()
		},
	})
}

func (a *trayApp) refresh() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func (a *trayApp) pollLoop() {
	t := time.NewTicker(trayPollEvery)
	defer t.Stop()
	for {
		a.poll()
		select {
		case <-t.C:
		case <-a.kick:
		case <-a.quit:
			return
		}
	}
}

// state returns a copy of the last poll.
func (a *trayApp) state() trayState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st
}

// paths returns the configuration, data and log locations in effect: the
// service's own when it is installed, else the flags and defaults.
func (a *trayApp) paths(si serviceInfo) (common, logSettings) {
	c, logs := a.o.c, a.o.logs
	if si.Installed && si.DataDir != "" {
		if c.data == "" {
			c.data = si.DataDir
		}
		if c.config == "" {
			c.config = si.Config
		}
		if logs.Dir == "" {
			logs.Dir = si.LogDir
		}
	}
	c.resolve()
	logs.resolve(c.data, os.Getenv)
	return c, logs
}

func (a *trayApp) poll() {
	si := queryService(a.o.name)
	c, logs := a.paths(si)
	st := trayState{svc: si, logDir: logs.Dir}

	if !si.Installed && si.Error == "" && a.o.embedded == "auto" {
		st.embedded = true
		a.embMu.Lock()
		paused := a.paused
		a.embMu.Unlock()
		if !paused {
			if err := a.startEmbedded(c, logs); err != nil {
				st.embErr = err.Error()
			}
		}
		st.embPaused = paused
	} else if si.Installed {
		// The service took over (installed from the menu or by an
		// installer): the tray must not keep a second feeder.
		a.stopEmbedded()
	}

	st.url = a.statusURL(st, c, logs)
	if st.running() {
		var sn feederui.Snapshot
		ctx, cancel := context.WithTimeout(context.Background(), trayHTTPTimout)
		err := getJSON(ctx, a.http, st.url+"api/status", &sn)
		cancel()
		if err != nil {
			st.snapErr = err.Error()
		} else {
			st.snap = &sn
		}
	}

	a.mu.Lock()
	first := !a.first
	a.st, a.first = st, true
	a.mu.Unlock()

	level, tip, line := summarize(st)
	if a.tray != nil {
		if level != a.level {
			l := level
			a.tray.SetIcon(func(size int) []byte { return wintray.PNG(size, l) })
			if level == wintray.LevelError && !first {
				a.tray.Notify("AirView Feeder", line, wintray.NotifyWarning)
			}
			a.level = level
		}
		a.tray.SetTooltip(tip)
	}
	a.maybeFirstRun(st)
}

// statusURL finds the status page: -url, the URL the service or the
// tray-hosted feeder recorded, the configuration's ui.addr, the default.
func (a *trayApp) statusURL(st trayState, c common, logs logSettings) string {
	if a.o.url != "" {
		return withSlash(a.o.url)
	}
	if st.embedded {
		a.embMu.Lock()
		f := a.emb
		a.embMu.Unlock()
		if f != nil {
			if u := f.statusURL(); u != "" {
				return u
			}
		}
	} else if b, err := os.ReadFile(filepath.Join(logs.Dir, statusURLFile)); err == nil {
		if u := strings.TrimSpace(string(b)); strings.HasPrefix(u, "http://") {
			return withSlash(u)
		}
	}
	if lc, err := loadConfig(loadOptions{File: c.config, Environ: os.Environ()}); err == nil || !lc.Exists {
		return baseURL(lc.Eff.UI.Addr)
	}
	return baseURL("127.0.0.1:8099")
}

func withSlash(u string) string {
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}

func getJSON(ctx context.Context, cl *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// ---- tray-hosted feeder (portable mode) --------------------------------------

func (a *trayApp) startEmbedded(c common, logs logSettings) error {
	a.embMu.Lock()
	defer a.embMu.Unlock()
	if a.emb != nil {
		return nil
	}
	_, statErr := os.Stat(c.data)
	if err := os.MkdirAll(c.data, 0o750); err != nil {
		return err
	}
	if os.IsNotExist(statErr) {
		// A data directory the tray created: keep the identity private.
		_ = secureDirForUser(c.data)
	}
	release, err := lockDataDirWin(c.data)
	if err != nil {
		if errors.Is(err, errLocked) {
			return errors.New("another feeder uses " + c.data)
		}
		return err
	}
	var out io.Writer = io.Discard
	rl, err := openRotatingLog(logs.Dir, int64(logs.MaxMB)<<20, logs.Keep)
	if err == nil {
		out = rl
	}
	lg := log.New(out, "", log.LstdFlags|log.Lmicroseconds)
	logf := func(format string, args ...any) { lg.Printf(format, args...) }
	f, err := startFeeder(runOptions{ConfigFile: c.config, DataDir: c.data, Environ: os.Environ(), Logf: logf,
		Logger: slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))})
	if err != nil {
		release()
		if rl != nil {
			rl.Close()
		}
		return err
	}
	logf("tray: feeder running inside the tray (no Windows service installed)")
	a.emb, a.embLock, a.embLog = f, release, rl
	return nil
}

func (a *trayApp) stopEmbedded() {
	a.embMu.Lock()
	defer a.embMu.Unlock()
	if a.emb == nil {
		return
	}
	a.emb.stop()
	a.embLock()
	if a.embLog != nil {
		a.embLog.Close()
	}
	a.emb, a.embLock, a.embLog = nil, nil, nil
}

// ---- menu -------------------------------------------------------------------

// trayActions are the things the menu does (a struct so tests can check
// the menu without performing them).
type trayActions struct {
	openStatus, settings, claim, pause, resume, start, restart, install,
	logs, updates, toggleAutostart, quit func()
}

func (a *trayApp) menu() []wintray.Item {
	st := a.state()
	act := trayActions{
		openStatus:      func() { a.openStatus("") },
		settings:        func() { a.openStatus("#settings") },
		claim:           a.claim,
		pause:           func() { a.control("pause") },
		resume:          func() { a.control("continue") },
		start:           func() { a.control("start") },
		restart:         func() { a.control("restart") },
		install:         a.installService,
		logs:            func() { a.openLogs(st.logDir) },
		updates:         a.checkUpdates,
		toggleAutostart: a.toggleAutostart,
		quit:            a.quitTray,
	}
	user, machine := trayAutostart()
	return buildTrayMenu(st, act, user, machine)
}

// buildTrayMenu is the menu for a state (pure, tested).
func buildTrayMenu(st trayState, act trayActions, autoUser, autoMachine bool) []wintray.Item {
	_, _, line := summarize(st)
	items := []wintray.Item{
		{Title: wintray.Escape("AirView Feeder " + version), Disabled: true},
		{Title: wintray.Escape("Status: " + line), Disabled: true},
		{Separator: true},
		{Title: "&Open status page", Default: true, Disabled: !st.running(), OnClick: act.openStatus},
		{Title: "&Settings…", Disabled: !st.running(), OnClick: act.settings},
		{Title: "Get &claim code…", Disabled: !st.running() || st.snap == nil || len(st.snap.Uplinks) == 0, OnClick: act.claim},
		{Separator: true},
	}
	paused := (st.embedded && st.embPaused) || (!st.embedded && (st.svc.state == svc.Paused || st.svc.state == svc.PausePending))
	stopped := !st.embedded && st.svc.Installed && st.svc.state == svc.Stopped
	switch {
	case paused:
		items = append(items, wintray.Item{Title: "&Resume feeding", OnClick: act.resume})
	case stopped:
		items = append(items, wintray.Item{Title: "S&tart feeder", OnClick: act.start})
	case st.embedded || st.svc.Installed:
		items = append(items, wintray.Item{Title: "&Pause feeding", Disabled: !st.running(), OnClick: act.pause})
	}
	if st.embedded || (st.svc.Installed && !stopped) {
		items = append(items, wintray.Item{Title: "R&estart feeder", OnClick: act.restart})
	}
	if !st.svc.Installed {
		items = append(items, wintray.Item{Title: "&Install as Windows service…", OnClick: act.install})
	}
	items = append(items,
		wintray.Item{Separator: true},
		wintray.Item{Title: "Open &logs folder", OnClick: act.logs},
		wintray.Item{Title: "Check for &updates…", OnClick: act.updates},
	)
	if autoMachine {
		items = append(items, wintray.Item{Title: "Start with Windows (set by the installer)", Checked: true, Disabled: true})
	} else {
		items = append(items, wintray.Item{Title: "Start with &Windows", Checked: autoUser, OnClick: act.toggleAutostart})
	}
	quit := "&Quit tray"
	if st.embedded && !st.embPaused && st.embErr == "" {
		quit = "&Quit (stops feeding)"
	}
	items = append(items, wintray.Item{Separator: true}, wintray.Item{Title: quit, OnClick: act.quit})
	return items
}

// ---- actions ----------------------------------------------------------------

func (a *trayApp) openStatus(fragment string) {
	st := a.state()
	if st.url == "" {
		return
	}
	if err := wintray.Open(st.url + fragment); err != nil {
		wintray.MessageBox("AirView Feeder", "Cannot open "+st.url+": "+err.Error(), wintray.MBIconError)
	}
}

func (a *trayApp) openLogs(dir string) {
	if dir == "" {
		return
	}
	if err := wintray.Open(dir); err != nil {
		wintray.MessageBox("AirView Feeder", "Cannot open "+dir+": "+err.Error(), wintray.MBIconError)
	}
}

func (a *trayApp) claim() {
	st := a.state()
	var res feederui.ClaimResponse
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	err := localCall(ctx, http.MethodPost, st.url+"api/claim", feederui.ClaimRequest{}, &res)
	if err != nil {
		wintray.MessageBox("AirView Feeder", "No claim code: "+err.Error(), wintray.MBIconWarning)
		return
	}
	host := ""
	if st.snap != nil && len(st.snap.Uplinks) > 0 {
		host = st.snap.Uplinks[0].Host
	}
	copied := ""
	if wintray.SetClipboardText(res.Code) == nil {
		copied = " (copied to the clipboard)"
	}
	wintray.MessageBox("AirView Feeder: claim code", claimMessage(res, host, copied), wintray.MBIconInfo)
}

func claimMessage(res feederui.ClaimResponse, host, copied string) string {
	where := "on the aggregator's claim page"
	if host != "" {
		where = "on the claim page of " + host + " (for example https://" + host + "/claim)"
	}
	return fmt.Sprintf("Claim code: %s%s\nValid until: %s\n\nEnter it %s while logged in to your account. The code works once. Do not share it.",
		res.Code, copied, time.UnixMilli(res.ExpiresMs).Format("2006-01-02 15:04"), where)
}

// control pauses, resumes, starts or restarts the service, or the
// tray-hosted feeder.
func (a *trayApp) control(action string) {
	defer a.refresh()
	st := a.state()
	if st.embedded {
		switch action {
		case "pause":
			a.embMu.Lock()
			a.paused = true
			a.embMu.Unlock()
			a.stopEmbedded()
		case "continue", "start":
			a.embMu.Lock()
			a.paused = false
			a.embMu.Unlock()
		case "restart":
			a.stopEmbedded() // the next poll starts it again
		}
		return
	}
	_, err := controlService(a.o.name, action, 60*time.Second)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// Installed without -user-control: ask Windows for elevation.
		var code uint32
		code, err = wintray.RunElevated(siblingExe(), windows.ComposeCommandLine([]string{"service", action, "-name", a.o.name}), 2*time.Minute)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit code %d", code)
		}
	}
	if err != nil && !errors.Is(err, wintray.ErrCancelled) {
		wintray.MessageBox("AirView Feeder", fmt.Sprintf("Could not %s the feeder service: %v", action, err), wintray.MBIconError)
	}
}

func (a *trayApp) installService() {
	if wintray.MessageBox("AirView Feeder",
		"Install the feeder as a Windows service?\n\nIt then starts with Windows and feeds even when nobody is logged in. "+
			"The configuration and identity stay where they are. Windows asks for administrator rights.",
		wintray.MBOKCancel|wintray.MBIconInfo) != wintray.IDOK {
		return
	}
	a.embMu.Lock()
	a.paused = true // keep the poll from restarting the tray-hosted feeder meanwhile
	a.embMu.Unlock()
	a.stopEmbedded()
	args := []string{"service", "install", "-name", a.o.name, "-now"}
	if a.o.c.config != "" {
		args = append(args, "-config", absPath(a.o.c.config))
	}
	if a.o.c.data != "" {
		args = append(args, "-data", absPath(a.o.c.data))
	}
	code, err := wintray.RunElevated(siblingExe(), windows.ComposeCommandLine(args), 3*time.Minute)
	if err == nil && code != 0 {
		err = fmt.Errorf("the installer exited with code %d", code)
	}
	a.embMu.Lock()
	a.paused = false
	a.embMu.Unlock()
	if err != nil && !errors.Is(err, wintray.ErrCancelled) {
		wintray.MessageBox("AirView Feeder", "The service was not installed: "+err.Error(), wintray.MBIconError)
	}
	a.refresh()
}

// Exit codes of "airview-feeder update" (internal/selfupdate.RunCommand).
const (
	updOK            = 0
	updUsage         = 2
	updNotConfigured = 3
	updRolledBack    = 4
)

// checkUpdates runs "airview-feeder update -service <name>" elevated (it
// replaces the program in Program Files and restarts the service, with a
// health check and rollback) and reports the outcome. Without the service,
// or when this build or configuration has no signed updates, it explains
// the manual way.
func (a *trayApp) checkUpdates() {
	st := a.state()
	where := "your network's \"Feed this network\" page"
	if st.snap != nil && len(st.snap.Uplinks) > 0 && st.snap.Uplinks[0].Host != "" {
		where = "the \"Feed this network\" page of " + st.snap.Uplinks[0].Host
	}
	manual := "Download the newest installer or ZIP from " + where + " and run it: the configuration and identity are kept."
	if st.embedded || !st.svc.Installed {
		wintray.MessageBox("AirView Feeder", "This is AirView Feeder "+version+".\n\n"+manual, wintray.MBIconInfo)
		return
	}
	if wintray.MessageBox("AirView Feeder", "This is AirView Feeder "+version+".\n\nCheck for a signed update and install it now? "+
		"Windows asks for administrator rights. If an update is installed the feeder restarts, and goes back to this version by itself if the new one does not work.",
		wintray.MBOKCancel|wintray.MBIconInfo) != wintray.IDOK {
		return
	}
	code, err := wintray.RunElevated(siblingExe(), windows.ComposeCommandLine(updateArgs(a.o.name, st.svc)), 35*time.Minute)
	if errors.Is(err, wintray.ErrCancelled) {
		return
	}
	defer a.refresh()
	if err != nil {
		wintray.MessageBox("AirView Feeder", "The update could not be started: "+err.Error(), wintray.MBIconError)
		return
	}
	now := installedVersion()
	msg, icon := updateMessage(int(code), now, manual)
	wintray.MessageBox("AirView Feeder", msg, icon)
}

// updateArgs is the elevated update command for the service.
func updateArgs(name string, si serviceInfo) []string {
	args := []string{"update", "-service", name}
	if si.Config != "" {
		args = append(args, "-config", si.Config)
	}
	if si.DataDir != "" {
		args = append(args, "-data", si.DataDir)
	}
	return args
}

// updateMessage explains an update exit code (pure, tested).
func updateMessage(code int, now, manual string) (string, uint32) {
	switch code {
	case updOK:
		if now != "" && now != version {
			return "AirView Feeder was updated to " + now + " and runs normally. The tray icon uses the new version after you log on again.", wintray.MBIconInfo
		}
		return "AirView Feeder is up to date.", wintray.MBIconInfo
	case updRolledBack:
		return "An update was installed but did not work on this computer, so the previous version runs again. Nothing else changed; see the logs folder for details.", wintray.MBIconWarning
	case updNotConfigured, updUsage:
		return "Signed updates are not set up for this feeder.\n\n" + manual, wintray.MBIconInfo
	}
	return fmt.Sprintf("The update failed (code %d); the installed version keeps running. See the logs folder.\n\n%s", code, manual), wintray.MBIconWarning
}

// installedVersion asks the console program next to the tray for its
// version ("" if it cannot be run).
func installedVersion() string {
	cmd := exec.Command(siblingExe(), "version")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

func (a *trayApp) toggleAutostart() {
	user, _ := trayAutostart()
	if err := setTrayAutostart(!user, trayCommand(a.o)); err != nil {
		wintray.MessageBox("AirView Feeder", "Cannot change the autostart: "+err.Error(), wintray.MBIconError)
	}
}

func (a *trayApp) quitTray() {
	st := a.state()
	if st.embedded && !st.embPaused && st.embErr == "" {
		if wintray.MessageBox("AirView Feeder", "The feeder runs inside this tray because the Windows service is not installed. Quitting stops feeding.\n\nQuit anyway?",
			wintray.MBOKCancel|wintray.MBIconWarning|wintray.MBDefButton2) != wintray.IDOK {
			return
		}
	}
	close(a.quit)
	a.stopEmbedded()
	if a.tray != nil {
		a.tray.Quit()
	}
}

// maybeFirstRun opens the settings once when no aggregator is configured.
func (a *trayApp) maybeFirstRun(st trayState) {
	if st.snap == nil || len(st.snap.Uplinks) > 0 || a.tray == nil {
		return
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, trayStateKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	if v, _, err := k.GetIntegerValue("SetupPrompted-" + a.o.name); err == nil && v != 0 {
		return
	}
	_ = k.SetDWordValue("SetupPrompted-"+a.o.name, 1)
	a.tray.Notify("AirView Feeder", "Finish the setup: add your aggregator on the settings page.", wintray.NotifyInfo)
	a.openStatus("#settings")
}

// ---- autostart --------------------------------------------------------------

// trayAutostart reports the per-user and per-machine (installer) Run
// entries.
func trayAutostart() (user, machine bool) {
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		k, err := registry.OpenKey(root, trayRunKey, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		_, _, err = k.GetStringValue(trayRunValue)
		k.Close()
		if err == nil {
			if root == registry.CURRENT_USER {
				user = true
			} else {
				machine = true
			}
		}
	}
	return
}

func setTrayAutostart(on bool, command string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, trayRunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if on {
		return k.SetStringValue(trayRunValue, command)
	}
	if err := k.DeleteValue(trayRunValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}
