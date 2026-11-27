package wininstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options are the choices of one install (the wizard pages or switches).
type Options struct {
	Dir       string
	Scope     Scope
	StartMenu bool
	Desktop   bool
	Firewall  bool
	Port      int
	// PortFixed (set by /PORT=) limits the firewall rule to Port; without
	// it the rule allows the program on any TCP port (UX-09: a port
	// changed later in the program's settings keeps working).
	PortFixed bool
	Service   bool
	Tray      bool
	// Params are the product configuration switches (/SERVER=...).
	Params map[string]string
	// Stub is the running installer: the first StubSize bytes become
	// uninstall.exe. Empty skips writing an uninstaller (tests).
	Stub     string
	StubSize int64
}

// DefaultOptions are the product defaults for a scope (Dir is left empty:
// see DefaultDir).
func DefaultOptions(p *Product, scope Scope) Options {
	o := Options{Scope: scope, StartMenu: true, Desktop: hasDesktopShortcut(p), Params: map[string]string{}}
	if p.Firewall != nil {
		o.Firewall = p.Firewall.Default && scope == PerMachine
		o.Port = p.Firewall.Port
	}
	if p.Service != nil {
		o.Service = p.Service.Default && scope == PerMachine
	}
	if p.Tray != nil {
		o.Tray = p.Tray.Default
	}
	return o
}

func hasDesktopShortcut(p *Product) bool {
	for _, s := range p.Shortcuts {
		if s.Desktop {
			return true
		}
	}
	return false
}

// ApplyArgs overrides options with command-line switches.
func (o *Options) ApplyArgs(p *Product, a *Args) error {
	set := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	set(&o.Desktop, a.Desktop)
	set(&o.StartMenu, a.StartMenu)
	set(&o.Firewall, a.Firewall)
	set(&o.Service, a.Service)
	set(&o.Tray, a.Tray)
	if a.Port != 0 {
		o.Port, o.PortFixed = a.Port, true
	}
	if a.Dir != "" {
		o.Dir = a.Dir
	}
	if p.Service == nil && a.Service != nil && *a.Service {
		return fmt.Errorf("%s has no Windows service (/SERVICE)", p.Name)
	}
	if p.Tray == nil && a.Tray != nil && *a.Tray {
		return fmt.Errorf("%s has no tray icon (/TRAY)", p.Name)
	}
	if p.Firewall == nil && a.Firewall != nil && *a.Firewall {
		return fmt.Errorf("%s has no firewall rule (/FIREWALL)", p.Name)
	}
	if o.Scope == PerUser {
		if a.Service != nil && *a.Service {
			return fmt.Errorf("/SERVICE needs an install for all users (not /CURRENTUSER)")
		}
		if a.Firewall != nil && *a.Firewall {
			return fmt.Errorf("/FIREWALL needs an install for all users (not /CURRENTUSER)")
		}
		o.Service, o.Firewall = false, false
	}
	known := map[string]Param{}
	if p.Configure != nil {
		known[p.Configure.Trigger] = Param{Switch: p.Configure.Trigger}
		for _, prm := range p.Configure.Params {
			known[prm.Switch] = prm
		}
	}
	for k, v := range a.Params {
		prm, ok := known[k]
		if !ok {
			return fmt.Errorf("unknown switch /%s", k)
		}
		if prm.Flag != "" && !prm.Bool && v == "" {
			return fmt.Errorf("/%s needs a value (/%s=...)", k, k)
		}
		if o.Params == nil {
			o.Params = map[string]string{}
		}
		o.Params[k] = v
	}
	if p.Configure != nil && len(o.Params) > 0 && o.Params[p.Configure.Trigger] == "" {
		return fmt.Errorf("configuration switches need /%s=", p.Configure.Trigger)
	}
	return nil
}

// Result reports what happened beyond success / failure.
type Result struct {
	Upgraded       bool
	RebootRequired bool
	// StepErrors are failures after the files were installed (service,
	// firewall, configuration): the product is installed but needs
	// attention. The installer exits 1 when there are any.
	StepErrors []string
	// Kept lists what the uninstaller left in place (data, foreign files).
	Kept []string
}

// Installer performs installs and uninstalls.
type Installer struct {
	Sys System
	// Logf receives the setup log (nil: discarded).
	Logf func(format string, a ...any)
	// Progress receives a status line and the completed fraction 0..1.
	Progress func(status string, frac float64)
	// Now is the clock (tests).
	Now func() time.Time
}

func (in *Installer) logf(format string, a ...any) {
	if in.Logf != nil {
		in.Logf(format, a...)
	}
}

func (in *Installer) progress(s string, f float64) {
	if in.Progress != nil {
		in.Progress(s, f)
	}
}

func (in *Installer) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

// DefaultDir is the folder of an existing install of the product in this
// scope (an upgrade goes where the product already is), else the product's
// DefaultDir expanded.
func DefaultDir(sys System, p *Product, scope Scope) (string, error) {
	if info, ok, err := sys.ReadUninstall(scope, p.ID); err == nil && ok && info.InstallLocation != "" {
		return info.InstallLocation, nil
	}
	f, err := sys.Folders(scope)
	if err != nil {
		return "", err
	}
	e := Expander{Folders: f, Product: p}
	return e.Expand(p.DefaultDir)
}

// protectedDirs are folders an install directory may not be.
func protectedDirs(f map[string]string) []string {
	var out []string
	for _, k := range []string{FolderProgramFiles, FolderCommonAppData, FolderUserAppData, FolderLocalAppData, FolderPrograms, FolderDesktop, FolderTemp, FolderSystem} {
		if v := f[k]; v != "" {
			out = append(out, v, filepath.Dir(v))
		}
	}
	return out
}

// Install installs (or upgrades) the payload's product.
func (in *Installer) Install(ctx context.Context, pl *Payload, o Options) (*Result, error) {
	p := pl.Product
	res := &Result{}
	if o.Scope == PerMachine && !in.Sys.IsElevated() {
		return nil, ErrNeedsAdmin
	}
	folders, err := in.Sys.Folders(o.Scope)
	if err != nil {
		return nil, err
	}
	dir := cleanWin(o.Dir)
	if err := CheckInstallDir(dir, protectedDirs(folders)); err != nil {
		return nil, err
	}
	if o.Port == 0 && p.Firewall != nil {
		o.Port = p.Firewall.Port
	}
	exp := &Expander{Folders: folders, App: dir, Port: o.Port, Product: p}
	if p, err = ResolveNames(p, exp); err != nil {
		return nil, err
	}
	exp.Product = p
	in.logf("Installing %s %s for %s into %s", p.Name, p.Version, o.Scope, dir)

	old, err := ReadReceipt(dir)
	switch {
	case err == nil:
		if !strings.EqualFold(old.Product.ID, p.ID) {
			return nil, fmt.Errorf("%s already holds another program (%s); choose another folder", dir, old.Product.Name)
		}
		if old.ScopeValue() != o.Scope {
			return nil, fmt.Errorf("%s is installed there for %s; uninstall it first or choose the same scope", old.Product.Name, old.Scope)
		}
		res.Upgraded = true
		in.logf("Upgrading %s %s (installed %s)", old.Product.Name, old.Product.Version, old.Installed.Format(time.RFC3339))
	case errors.Is(err, os.ErrNotExist):
		old = nil
	default:
		return nil, fmt.Errorf("reading the existing installation: %w", err)
	}

	// Close what holds the files: trays, then the service.
	in.progress("Preparing...", 0.01)
	serviceWasRunning := false
	if res.Upgraded {
		in.quitTray(exp, p)
		if p.Service != nil {
			if exists, running, _ := in.Sys.ServiceExists(p.Service.Name); exists {
				serviceWasRunning = running
				if running {
					in.logf("Stopping service %s", p.Service.Name)
					if err := in.Sys.StopService(p.Service.Name, 90*time.Second); err != nil {
						in.logf("  could not stop: %v", err)
					}
				}
			}
		}
	}

	// Files.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	if ds, ok := in.Sys.(dirSecurer); ok && o.Scope == PerMachine {
		// SEC-R5: a per-machine install folder (a service runs from it as
		// SYSTEM) must not be writable by standard users.
		if err := ds.SecureInstallDir(dir); err != nil {
			return nil, err
		}
	}
	tx := &fileTxn{dir: dir, logf: in.logf}
	files, err := in.copyFiles(ctx, pl, o, tx)
	if err != nil {
		in.logf("Rolling back: %v", err)
		tx.rollback()
		if !res.Upgraded {
			removeEmptyDirs(dir, dir)
			_ = os.Remove(dir)
		} else if serviceWasRunning {
			_, _, _ = in.execCmd(exp, Command{Exe: p.Service.Install.Exe, Args: []string{"service", "start", "-name", p.Service.Name}})
		}
		return nil, err
	}
	tx.commit(in.Sys, res)

	// Files of the previous version that this one no longer has.
	if old != nil {
		keep := map[string]bool{}
		for _, f := range files {
			keep[strings.ToLower(f.Path)] = true
		}
		for _, f := range old.Files {
			if !keep[strings.ToLower(f.Path)] {
				in.removeFile(joinRel(dir, f.Path), res)
			}
		}
		removeEmptyDirs(dir, dir)
	}

	rec := &Receipt{
		Product: *p, Scope: o.Scope.String(), Dir: dir, Installed: in.now().UTC().Truncate(time.Second),
		Files: files, Desktop: o.Desktop, StartMenu: o.StartMenu, Port: o.Port,
	}
	for _, d := range p.DataDirs {
		if v, err := exp.Expand(d); err == nil {
			rec.DataDirs = append(rec.DataDirs, v)
		} else {
			in.logf("data folder %s: %v", d, err)
		}
	}
	if p.Service != nil {
		rec.ServiceName = p.Service.Name
		rec.Service = o.Service
	}
	if p.Tray != nil {
		rec.TrayRunValue = p.Tray.RunValue
	}
	if p.Firewall != nil {
		rec.Firewall = p.Firewall.Rule
	}

	// Shortcuts (the old ones first: names may have changed).
	in.progress("Creating shortcuts...", 0.9)
	if old != nil {
		for _, s := range old.Shortcuts {
			_ = os.Remove(s)
		}
	}
	if err := in.shortcuts(exp, p, o, rec); err != nil {
		res.StepErrors = append(res.StepErrors, err.Error())
	}
	// The receipt before the registry and system changes: from here on the
	// uninstaller can undo everything.
	if err := WriteReceipt(dir, rec); err != nil {
		return nil, fmt.Errorf("writing %s: %w", ReceiptName, err)
	}

	in.progress("Registering...", 0.93)
	var size int64
	for _, f := range files {
		size += f.Size
	}
	if st, err := os.Stat(filepath.Join(dir, UninstallerName)); err == nil {
		size += st.Size()
	}
	info := UninstallInfo{
		DisplayName:          p.Name,
		DisplayIcon:          joinRel(dir, p.MainExe) + ",0",
		DisplayVersion:       p.Version,
		Publisher:            p.Publisher,
		UninstallString:      quoteArg(filepath.Join(dir, UninstallerName)),
		QuietUninstallString: quoteArg(filepath.Join(dir, UninstallerName)) + " /S",
		InstallLocation:      dir,
		InstallDate:          in.now().Format("20060102"),
		HelpLink:             p.HelpLink,
		Comments:             p.Comments,
		EstimatedSizeKB:      uint32((size + 1023) / 1024),
		NoModify:             true,
		NoRepair:             true,
	}
	if err := in.Sys.WriteUninstall(o.Scope, p.ID, info); err != nil {
		return nil, fmt.Errorf("registering the uninstaller: %w", err)
	}

	// Firewall.
	if p.Firewall != nil && o.Scope == PerMachine {
		_ = in.Sys.DeleteFirewallRule(p.Firewall.Rule)
		if o.Firewall {
			prog, err := exp.Expand(p.Firewall.Program)
			if err == nil {
				port := 0 // any port of the program
				if o.PortFixed {
					port = o.Port
				}
				in.logf("Firewall rule %q: TCP %s for %s", p.Firewall.Rule, portText(port), prog)
				err = in.Sys.AddFirewallRule(p.Firewall.Rule, prog, port, profilesOr(p.Firewall.Profiles))
			}
			if err != nil {
				res.StepErrors = append(res.StepErrors, fmt.Sprintf("firewall rule: %v", err))
			}
		}
	}

	// Configuration (before the service starts with it).
	if p.Configure != nil && o.Params[p.Configure.Trigger] != "" {
		in.progress("Configuring...", 0.95)
		c := p.Configure.Command
		for _, prm := range p.Configure.Params {
			v, ok := o.Params[prm.Switch]
			if !ok {
				continue
			}
			if prm.Bool {
				c.Args = append(c.Args, prm.Flag)
			} else {
				c.Args = append(c.Args, prm.Flag, v)
			}
		}
		if out, code, err := in.execCmd(exp, c); err != nil || code != 0 {
			res.StepErrors = append(res.StepErrors, fmt.Sprintf("the configuration was not accepted (exit %d, %v): %s", code, err, strings.TrimSpace(out)))
		}
	}

	// Post-install commands (e.g. the feeder's update settings).
	for _, c := range p.PostInstall {
		if !postWhen(exp, c.When) || (c.MachineOnly && o.Scope != PerMachine) {
			continue
		}
		what := c.What
		if what == "" {
			what = "post-install step"
		}
		in.logf("Running %s", what)
		if out, code, err := in.execCmd(exp, c.Command); err != nil || code != 0 {
			res.StepErrors = append(res.StepErrors, fmt.Sprintf("%s failed (exit %d, %v): %s", what, code, err, strings.TrimSpace(out)))
		}
	}

	// Service.
	if p.Service != nil {
		in.progress("Registering the service...", 0.97)
		if o.Service && o.Scope == PerMachine {
			if out, code, err := in.execCmd(exp, p.Service.Install); err != nil || code != 0 {
				res.StepErrors = append(res.StepErrors, fmt.Sprintf("the Windows service could not be installed or started (exit %d, %v): %s", code, err, strings.TrimSpace(out)))
			}
		} else if exists, _, _ := in.Sys.ServiceExists(p.Service.Name); exists && o.Scope == PerMachine {
			// Without the service the tray runs the program; a service from an
			// earlier install would hold the same data, so it goes.
			in.removeService(exp, p, res)
		}
	}

	// Tray autostart.
	if p.Tray != nil {
		if o.Tray {
			exe, err := exp.Expand(p.Tray.Exe)
			if err == nil {
				err = in.Sys.SetRun(o.Scope, p.Tray.RunValue, quoteArg(exe))
			}
			if err != nil {
				res.StepErrors = append(res.StepErrors, fmt.Sprintf("tray autostart: %v", err))
			}
		} else {
			_ = in.Sys.DeleteRun(o.Scope, p.Tray.RunValue)
		}
	}
	in.progress("Done", 1)
	in.logf("Installed %s %s (%d files, %d KB)%s", p.Name, p.Version, len(files), info.EstimatedSizeKB, map[bool]string{true: "; restart Windows to finish", false: ""}[res.RebootRequired])
	return res, nil
}

// postWhen reports whether every condition expands to a non-empty string
// (an expansion error counts as empty).
func postWhen(exp *Expander, when []string) bool {
	for _, w := range when {
		v, err := exp.Expand(w)
		if err != nil || strings.TrimSpace(v) == "" {
			return false
		}
	}
	return true
}

// ResolveNames returns a copy of p with the {var} tokens of the service
// name, tray Run value and firewall rule resolved.
func ResolveNames(p *Product, exp *Expander) (*Product, error) {
	c := *p
	var err error
	if p.Service != nil {
		s := *p.Service
		if s.Name, err = exp.Expand(s.Name); err != nil {
			return nil, err
		}
		if !idRE.MatchString(s.Name) {
			return nil, fmt.Errorf("service name %q: letters, digits, '.', '_' or '-'", s.Name)
		}
		c.Service = &s
	}
	if p.Tray != nil {
		t := *p.Tray
		if t.RunValue, err = exp.Expand(t.RunValue); err != nil {
			return nil, err
		}
		c.Tray = &t
	}
	if p.Firewall != nil {
		f := *p.Firewall
		if f.Rule, err = exp.Expand(f.Rule); err != nil {
			return nil, err
		}
		c.Firewall = &f
	}
	return &c, nil
}

func profilesOr(p string) string {
	if p == "" {
		return "private,domain"
	}
	return p
}

func (in *Installer) copyFiles(ctx context.Context, pl *Payload, o Options, tx *fileTxn) ([]ReceiptFile, error) {
	total := pl.TotalSize() + o.StubSize
	if total <= 0 {
		total = 1
	}
	var done int64
	var files []ReceiptFile
	for _, zf := range pl.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel := RelName(zf)
		in.progress("Installing "+rel, 0.02+0.86*float64(done)/float64(total))
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		size, sum, err := tx.put(rel, &ctxReader{ctx: ctx, r: rc})
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("installing %s: %w", rel, err)
		}
		files = append(files, ReceiptFile{Path: rel, Size: size, SHA256: sum})
		done += size
	}
	if o.Stub != "" && o.StubSize > 0 {
		f, err := os.Open(o.Stub)
		if err != nil {
			return nil, fmt.Errorf("uninstaller: %w", err)
		}
		_, _, err = tx.put(UninstallerName, io.LimitReader(f, o.StubSize))
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("installing %s: %w", UninstallerName, err)
		}
	}
	return files, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}

// shortcuts creates the Start-menu and desktop shortcuts and records them.
func (in *Installer) shortcuts(exp *Expander, p *Product, o Options, rec *Receipt) error {
	var errs []error
	programs := exp.Folders[FolderPrograms]
	desktop := exp.Folders[FolderDesktop]
	if o.StartMenu && programs != "" {
		smDir := programs
		if p.StartMenuFolder != "" {
			smDir = filepath.Join(programs, p.StartMenuFolder)
			rec.StartMenuDir = smDir
		}
		if err := os.MkdirAll(smDir, 0o755); err != nil {
			return err
		}
		for _, s := range p.Shortcuts {
			path := filepath.Join(smDir, s.Name+".lnk")
			if err := in.makeShortcut(exp, path, s); err != nil {
				errs = append(errs, err)
				continue
			}
			rec.Shortcuts = append(rec.Shortcuts, path)
		}
		for _, u := range p.URLShortcuts {
			path := filepath.Join(smDir, u.Name+".url")
			if err := writeURLShortcut(exp, path, u); err != nil {
				errs = append(errs, err)
				continue
			}
			rec.Shortcuts = append(rec.Shortcuts, path)
		}
		un := filepath.Join(smDir, "Uninstall "+p.Name+".lnk")
		if o.Stub != "" {
			if err := in.Sys.CreateShortcut(un, ShortcutTarget{
				Target: filepath.Join(exp.App, UninstallerName), WorkDir: exp.App,
				Description: "Remove " + p.Name,
			}); err != nil {
				errs = append(errs, err)
			} else {
				rec.Shortcuts = append(rec.Shortcuts, un)
			}
		}
	}
	if o.Desktop && desktop != "" {
		for _, s := range p.Shortcuts {
			if !s.Desktop {
				continue
			}
			path := filepath.Join(desktop, s.Name+".lnk")
			if err := in.makeShortcut(exp, path, s); err != nil {
				errs = append(errs, err)
				continue
			}
			rec.Shortcuts = append(rec.Shortcuts, path)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("shortcuts: %w", errors.Join(errs...))
	}
	return nil
}

func (in *Installer) makeShortcut(exp *Expander, path string, s Shortcut) error {
	t, err := exp.Expand(s.Target)
	if err != nil {
		return err
	}
	args, err := exp.ExpandAll(s.Args)
	if err != nil {
		return err
	}
	wd := filepath.Dir(t)
	if s.WorkDir != "" {
		if wd, err = exp.Expand(s.WorkDir); err != nil {
			return err
		}
	}
	icon := t
	if s.Icon != "" {
		if icon, err = exp.Expand(s.Icon); err != nil {
			return err
		}
	}
	in.logf("Shortcut %s -> %s", path, t)
	return in.Sys.CreateShortcut(path, ShortcutTarget{
		Target: t, Args: ComposeArgs(args), WorkDir: wd, Icon: icon, Description: s.Description,
	})
}

func writeURLShortcut(exp *Expander, path string, u URLShortcut) error {
	url, err := exp.Expand(u.URL)
	if err != nil {
		return err
	}
	s := "[InternetShortcut]\r\nURL=" + url + "\r\n"
	if u.Icon != "" {
		icon, err := exp.Expand(u.Icon)
		if err != nil {
			return err
		}
		s += "IconFile=" + icon + "\r\nIconIndex=0\r\n"
	}
	return os.WriteFile(path, []byte(s), 0o644)
}

func (in *Installer) execCmd(exp *Expander, c Command) (string, int, error) {
	exe, err := exp.Expand(c.Exe)
	if err != nil {
		return "", -1, err
	}
	args, err := exp.ExpandAll(c.Args)
	if err != nil {
		return "", -1, err
	}
	t := time.Duration(c.TimeoutSec) * time.Second
	if t <= 0 {
		t = 120 * time.Second
	}
	in.logf("Running %s %s", exe, ComposeArgs(redactArgs(args)))
	out, code, err := in.Sys.Exec(exe, args, t)
	in.logf("  exit %d%s", code, map[bool]string{true: " (" + fmt.Sprint(err) + ")", false: ""}[err != nil])
	if s := strings.TrimSpace(out); s != "" {
		in.logf("  %s", strings.ReplaceAll(s, "\n", "\n  "))
	}
	return out, code, err
}

// redactArgs hides the values of secret-looking flags in the log.
func redactArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		f := strings.ToLower(strings.TrimLeft(out[i], "-"))
		if strings.Contains(f, "password") || strings.Contains(f, "token") || f == "enrol" || f == "enroll" || strings.Contains(f, "secret") {
			out[i+1] = "***"
		}
	}
	return out
}

func (in *Installer) quitTray(exp *Expander, p *Product) {
	if p.Tray == nil || p.Tray.Quit == nil {
		return
	}
	exe, err := exp.Expand(p.Tray.Quit.Exe)
	if err != nil {
		return
	}
	if _, err := os.Stat(exe); err != nil {
		return
	}
	_, _, _ = in.execCmd(exp, *p.Tray.Quit)
}

func (in *Installer) removeService(exp *Expander, p *Product, res *Result) {
	name := p.Service.Name
	if exe, err := exp.Expand(p.Service.Remove.Exe); err == nil {
		if _, err := os.Stat(exe); err == nil {
			_, _, _ = in.execCmd(exp, p.Service.Remove)
		}
	}
	if exists, _, _ := in.Sys.ServiceExists(name); exists {
		in.logf("Deleting service %s", name)
		if err := in.Sys.DeleteService(name, 60*time.Second); err != nil {
			res.StepErrors = append(res.StepErrors, fmt.Sprintf("service %s: %v", name, err))
		}
	}
}

// removeFile deletes a file, or schedules it for deletion at the next
// restart when it is in use.
func (in *Installer) removeFile(path string, res *Result) {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return
	}
	// A running program cannot be deleted but can be renamed out of the way.
	tmp := path + ".avdel"
	_ = os.Remove(tmp)
	if os.Rename(path, tmp) == nil {
		path = tmp
	}
	if derr := in.Sys.DeleteOnReboot(path); derr == nil {
		in.logf("%s is in use: deleted at the next restart", path)
		res.RebootRequired = true
	} else {
		in.logf("could not delete %s: %v", path, err)
		res.Kept = append(res.Kept, path)
	}
}

// removeEmptyDirs removes empty directories below dir (not dir itself).
func removeEmptyDirs(dir, root string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			sub := filepath.Join(dir, e.Name())
			removeEmptyDirs(sub, root)
			_ = os.Remove(sub) // fails unless empty
		}
	}
}

// ---- file transaction ---------------------------------------------------------

// fileTxn replaces files so an error part-way restores the previous state:
// the new file is written beside the old one (.avnew), the old one is
// renamed out of the way (.avold; also works for a running program) and
// the new one renamed into place.
type fileTxn struct {
	dir     string
	logf    func(string, ...any)
	entries []txnEntry
	madeDir []string
}

type txnEntry struct {
	dest, old string
}

func (t *fileTxn) put(rel string, r io.Reader) (int64, string, error) {
	dest := joinRel(t.dir, rel)
	if err := t.mkdirAll(filepath.Dir(dest)); err != nil {
		return 0, "", err
	}
	tmp := dest + ".avnew"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, "", err
	}
	e := txnEntry{dest: dest}
	if _, err := os.Lstat(dest); err == nil {
		e.old = dest + ".avold"
		_ = os.Remove(e.old)
		if err := os.Rename(dest, e.old); err != nil {
			_ = os.Remove(tmp)
			return 0, "", fmt.Errorf("%s is in use (close the program and try again): %w", dest, err)
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		if e.old != "" {
			_ = os.Rename(e.old, dest)
		}
		_ = os.Remove(tmp)
		return 0, "", err
	}
	t.entries = append(t.entries, e)
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func (t *fileTxn) mkdirAll(d string) error {
	if _, err := os.Stat(d); err == nil {
		return nil
	}
	if err := t.mkdirAll(filepath.Dir(d)); err != nil {
		return err
	}
	if err := os.Mkdir(d, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	t.madeDir = append(t.madeDir, d)
	return nil
}

func (t *fileTxn) rollback() {
	for i := len(t.entries) - 1; i >= 0; i-- {
		e := t.entries[i]
		_ = os.Remove(e.dest)
		if e.old != "" {
			if err := os.Rename(e.old, e.dest); err != nil && t.logf != nil {
				t.logf("rollback: could not restore %s: %v", e.dest, err)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(t.madeDir)))
	for _, d := range t.madeDir {
		_ = os.Remove(d)
	}
	t.entries, t.madeDir = nil, nil
}

func (t *fileTxn) commit(sys System, res *Result) {
	for _, e := range t.entries {
		if e.old == "" {
			continue
		}
		if err := os.Remove(e.old); err != nil && !errors.Is(err, os.ErrNotExist) {
			if sys.DeleteOnReboot(e.old) == nil {
				res.RebootRequired = true
			}
		}
	}
	t.entries = nil
}

// ---- uninstall ---------------------------------------------------------------

// UninstallOptions are the choices of an uninstall.
type UninstallOptions struct {
	RemoveData bool
	// Self is the running uninstaller: left in place for the caller to
	// delete after exit (a running program cannot delete itself).
	Self string
}

// Uninstall removes the installation in dir as its receipt describes.
func (in *Installer) Uninstall(ctx context.Context, dir string, o UninstallOptions) (*Result, error) {
	_ = ctx
	dir = cleanWin(dir)
	rec, err := ReadReceipt(dir)
	if err != nil {
		return nil, fmt.Errorf("no installation found in %s: %w", dir, err)
	}
	p := &rec.Product
	scope := rec.ScopeValue()
	if scope == PerMachine && !in.Sys.IsElevated() {
		return nil, ErrNeedsAdmin
	}
	folders, err := in.Sys.Folders(scope)
	if err != nil {
		return nil, err
	}
	if err := CheckInstallDir(dir, protectedDirs(folders)); err != nil {
		return nil, err
	}
	exp := &Expander{Folders: folders, App: dir, Port: rec.Port, Product: p}
	res := &Result{}
	in.logf("Uninstalling %s %s from %s (%s)", p.Name, p.Version, dir, rec.Scope)

	in.progress("Stopping "+p.Name+"...", 0.05)
	in.quitTray(exp, p)
	if p.Service != nil && scope == PerMachine {
		if exists, _, _ := in.Sys.ServiceExists(p.Service.Name); exists {
			in.removeService(exp, p, res)
		}
	}
	if rec.TrayRunValue != "" {
		if err := in.Sys.DeleteRun(scope, rec.TrayRunValue); err != nil {
			in.logf("Run value %s: %v", rec.TrayRunValue, err)
		}
		if scope == PerMachine {
			// The tray's own "Start with Windows" uses the same name under HKCU.
			_ = in.Sys.DeleteRun(PerUser, rec.TrayRunValue)
		}
	}
	if rec.Firewall != "" && scope == PerMachine {
		if err := in.Sys.DeleteFirewallRule(rec.Firewall); err != nil {
			in.logf("firewall rule %s: %v", rec.Firewall, err)
		}
	}

	in.progress("Removing shortcuts...", 0.2)
	for _, s := range rec.Shortcuts {
		if err := os.Remove(s); err != nil && !errors.Is(err, os.ErrNotExist) {
			in.logf("shortcut %s: %v", s, err)
		}
	}
	if rec.StartMenuDir != "" {
		_ = os.Remove(rec.StartMenuDir) // only when empty
	}

	if o.RemoveData && p.PurgeData != nil {
		if exe, err := exp.Expand(p.PurgeData.Exe); err == nil {
			if _, err := os.Stat(exe); err == nil {
				_, _, _ = in.execCmd(exp, *p.PurgeData)
			}
		}
	}

	in.progress("Removing files...", 0.3)
	for i, f := range rec.Files {
		in.progress("Removing "+f.Path, 0.3+0.5*float64(i)/float64(len(rec.Files)+1))
		in.removeFile(joinRel(dir, f.Path), res)
	}
	removeEmptyDirs(dir, dir)

	if o.RemoveData {
		in.progress("Removing data...", 0.85)
		for _, d := range rec.DataDirs {
			if err := CheckInstallDir(d, protectedDirs(folders)); err != nil {
				in.logf("not removing data folder %s: %v", d, err)
				res.Kept = append(res.Kept, d)
				continue
			}
			in.logf("Removing data folder %s", d)
			if err := os.RemoveAll(d); err != nil {
				in.logf("  %v", err)
				res.Kept = append(res.Kept, d)
			}
		}
	} else {
		for _, d := range rec.DataDirs {
			if _, err := os.Stat(d); err == nil {
				in.logf("Keeping data folder %s", d)
				res.Kept = append(res.Kept, d)
			}
		}
	}

	if err := in.Sys.DeleteUninstall(scope, p.ID); err != nil {
		in.logf("uninstall registry key: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, ReceiptName))
	un := filepath.Join(dir, UninstallerName)
	if o.Self == "" || !strings.EqualFold(filepath.Clean(o.Self), filepath.Clean(un)) {
		in.removeFile(un, res)
	}
	removeEmptyDirs(dir, dir)
	_ = os.Remove(dir) // fails while the uninstaller or foreign files are in it
	in.progress("Done", 1)
	in.logf("Uninstalled %s", p.Name)
	return res, nil
}

// ---- command lines ----------------------------------------------------------

// quoteArg quotes one argument for a Windows command line (the rules of
// CommandLineToArgvW: backslashes are literal except before a quote).
func quoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			b.WriteByte('"')
			slashes = 0
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteByte(c)
			slashes = 0
		}
	}
	// Backslashes before the closing quote are doubled.
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

// ComposeArgs joins arguments into a Windows command-line string.
func ComposeArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = quoteArg(a)
	}
	return strings.Join(q, " ")
}

// ExitCode maps an outcome to the process exit code of the installer and
// the uninstaller.
func ExitCode(res *Result, err error) int {
	switch {
	case errors.Is(err, context.Canceled):
		return ExitCancelled
	case errors.Is(err, ErrNeedsAdmin):
		return ExitNeedsAdmin
	case err != nil:
		return ExitFailed
	case res != nil && len(res.StepErrors) > 0:
		return ExitFailed
	case res != nil && res.RebootRequired:
		return ExitRebootRequired
	}
	return ExitOK
}

// RemovalText is the uninstaller's sentence on what it removes.
func RemovalText(r *Receipt) string {
	parts := []string{"the program files", "the shortcuts", "the uninstall entry"}
	if r != nil {
		if r.Service && r.ServiceName != "" {
			parts = append(parts, "the Windows service "+r.ServiceName)
		}
		if r.TrayRunValue != "" {
			parts = append(parts, "the tray autostart")
		}
		if r.Firewall != "" && r.ScopeValue() == PerMachine {
			parts = append(parts, "the firewall rule")
		}
	}
	s := strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1] + " are removed."
	return strings.ToUpper(s[:1]) + s[1:]
}

// portText is a firewall port for the log ("any" for 0).
func portText(port int) string {
	if port == 0 {
		return "any port"
	}
	return fmt.Sprintf("port %d", port)
}

// dirSecurer is implemented by systems that can restrict the install
// folder's access list (Windows: WindowsSystem.SecureInstallDir).
type dirSecurer interface {
	SecureInstallDir(dir string) error
}
