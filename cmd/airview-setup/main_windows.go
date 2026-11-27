//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"airview/internal/wininstall"
)

func init() {
	// The wizard's window and message loop live on the main thread.
	runtime.LockOSThread()
}

func main() { os.Exit(run()) }

// app is one run of the installer or the uninstaller.
type app struct {
	exe        string
	args       *wininstall.Args
	sys        wininstall.System
	payload    *wininstall.Payload // nil for the uninstaller
	product    *wininstall.Product
	receipt    *wininstall.Receipt // uninstaller
	dir        string              // uninstaller: the install directory
	defaultDir string
	existing   string // version already installed (upgrade), else ""
	licence    string
	logPath    string
	// installedDir / scope of the finished install (for "launch").
	installedDir string
	scope        wininstall.Scope

	logMu sync.Mutex
	log   io.WriteCloser
}

func run() int {
	a := &app{sys: wininstall.NewSystem()}
	var err error
	if a.exe, err = os.Executable(); err != nil {
		return fail(nil, "cannot locate the setup program: "+err.Error(), wininstall.ExitFailed)
	}
	a.args, err = wininstall.ParseArgs(os.Args[1:])
	if err != nil {
		return fail(nil, err.Error()+"\n\n"+wininstall.Usage(nil, false), wininstall.ExitUsage)
	}

	a.payload, err = wininstall.OpenPayload(a.exe)
	switch {
	case errors.Is(err, wininstall.ErrNoPayload):
		return a.runUninstaller()
	case err != nil:
		return fail(a.args, "This setup program is damaged ("+err.Error()+"). Download it again.", wininstall.ExitFailed)
	}
	defer a.payload.Close()
	a.product = a.payload.Product
	if a.args.Uninstall {
		return fail(a.args, "Run uninstall.exe in the installation folder (or use Settings > Apps) to remove "+a.product.Name+".", wininstall.ExitUsage)
	}
	return a.runInstaller()
}

// fail reports an error before the log exists.
func fail(args *wininstall.Args, text string, code int) int {
	if args == nil || !args.Silent {
		messageBox(0, text, "Setup", mbOK|mbIconError)
	}
	fmt.Fprintln(os.Stderr, text)
	return code
}

func (a *app) title() string {
	if a.receipt != nil {
		return a.product.Name + " Uninstall"
	}
	return a.product.Name + " Setup"
}

func (a *app) openLog(kind string) {
	a.logPath = a.args.Log
	if a.logPath == "" {
		a.logPath = filepath.Join(os.TempDir(), a.product.ID+"-"+kind+".log")
	}
	f, err := os.OpenFile(a.logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	a.log = f
	a.logf("%s %s %s (%s), %s", a.product.Name, a.product.Version, kind, runtime.GOARCH, time.Now().Format(time.RFC3339))
	a.logf("Command line: %s", strings.Join(wininstall.RedactCommandLine(os.Args), " ")) // SEC-R10
}

func (a *app) logf(format string, v ...any) {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.log != nil {
		fmt.Fprintf(a.log, "%s %s\r\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, v...))
	}
}

func (a *app) closeLog() {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.log != nil {
		a.log.Close()
		a.log = nil
	}
}

func (a *app) installer(progress func(string, float64)) *wininstall.Installer {
	return &wininstall.Installer{Sys: a.sys, Logf: a.logf, Progress: progress}
}

// singleInstance keeps two setups of the same product apart.
func singleInstance(id string) (func(), bool) {
	h, err := windows.CreateMutex(nil, false, u16("Local\\AirViewSetup-"+id))
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return nil, false
	}
	return func() {
		if h != 0 {
			windows.CloseHandle(h)
		}
	}, true
}

// ---- installer ---------------------------------------------------------------

func (a *app) runInstaller() int {
	p := a.product
	if a.args.Help {
		return a.help(false)
	}
	if a.args.Shots != "" {
		release, ok := singleInstance(p.ID)
		if !ok {
			return fail(a.args, p.Name+" Setup is already running.", wininstall.ExitFailed)
		}
		defer release()
		return a.screenshots(a.args.Shots)
	}
	scope := wininstall.PerMachine
	if a.args.ScopeSet {
		scope = a.args.Scope
	}
	// The release stub is as-invoker (DLV-06): a per-user install needs no
	// elevation; a per-machine one asks for it the way Windows does for any
	// installer (one UAC prompt, also with /S).
	//
	// The single-instance lock is taken only AFTER this decision: the
	// unelevated parent waits for its elevated child, so holding the lock
	// across the relaunch made the child always report "already running".
	switch wininstall.NeedElevation(scope, a.sys.IsElevated(), a.args) {
	case wininstall.ElevationRelaunch:
		return relaunchElevated(a.exe, wininstall.ElevatedArgs(os.Args[1:]), a.args.Silent)
	case wininstall.ElevationFail:
		return fail(a.args, wininstall.ErrNeedsAdmin.Error(), wininstall.ExitNeedsAdmin)
	}
	release, ok := singleInstance(p.ID)
	if !ok {
		return fail(a.args, p.Name+" Setup is already running.", wininstall.ExitFailed)
	}
	defer release()
	a.openLog("setup")
	defer a.closeLog()

	o := wininstall.DefaultOptions(p, scope)
	dir, err := wininstall.DefaultDir(a.sys, p, scope)
	if err != nil {
		a.logf("default folder: %v", err)
		return fail(a.args, err.Error(), wininstall.ExitFailed)
	}
	a.defaultDir = dir
	if info, ok, _ := a.sys.ReadUninstall(scope, p.ID); ok {
		a.existing = info.DisplayVersion
		if a.existing == "" {
			a.existing = "(unknown version)"
		}
	}
	if err := o.ApplyArgs(p, a.args); err != nil {
		a.logf("%v", err)
		return fail(a.args, err.Error()+"\n\n"+wininstall.Usage(p, false), wininstall.ExitUsage)
	}
	if o.Dir == "" {
		o.Dir = dir
	} else {
		a.defaultDir = o.Dir
	}
	o.Stub, o.StubSize = a.exe, a.payload.StubSize
	if p.Licence != "" {
		if rc, err := a.payload.Open(p.Licence); err == nil {
			b, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			a.licence = toCRLF(string(b))
		}
	}

	if a.args.Silent {
		res, err := a.doInstall(context.Background(), o, nil)
		code := wininstall.ExitCode(res, err)
		if err != nil {
			a.logf("FAILED: %v", err)
		}
		a.logf("Exit code %d", code)
		return code
	}
	w := newWizard(a, false)
	w.opts = o
	w.options = a.optionList(o.Scope)
	return w.run()
}

// optionList is the Options page for this product.
func (a *app) optionList(scope wininstall.Scope) []*option {
	p := a.product
	var l []*option
	l = append(l, &option{label: "Create &Start menu shortcuts", get: func(o *wininstall.Options) *bool { return &o.StartMenu }})
	for _, s := range p.Shortcuts {
		if s.Desktop {
			l = append(l, &option{label: "Create a &desktop shortcut", get: func(o *wininstall.Options) *bool { return &o.Desktop }})
			break
		}
	}
	if p.Service != nil && scope == wininstall.PerMachine {
		l = append(l, &option{label: "Run as a Windows &service (starts with Windows, works without anyone logged on)",
			get: func(o *wininstall.Options) *bool { return &o.Service }})
	}
	if p.Tray != nil {
		l = append(l, &option{label: "Show the &tray icon when users log on", get: func(o *wininstall.Options) *bool { return &o.Tray }})
	}
	if p.Firewall != nil && scope == wininstall.PerMachine {
		l = append(l, &option{label: "Allow other computers on the local network to connect (&firewall rule for this program)",
			get:  func(o *wininstall.Options) *bool { return &o.Firewall },
			note: fmt.Sprintf("Private and domain networks only. The rule follows the program, so it keeps working if you change its port (default %d) later.", p.Firewall.Port)})
	}
	return l
}

func (a *app) doInstall(ctx context.Context, o wininstall.Options, progress func(string, float64)) (*wininstall.Result, error) {
	a.installedDir, a.scope = o.Dir, o.Scope
	res, err := a.installer(progress).Install(ctx, a.payload, o)
	if res != nil {
		for _, e := range res.StepErrors {
			a.logf("ATTENTION: %s", e)
		}
	}
	return res, err
}

func (a *app) needBytes() int64 { return a.payload.TotalSize() + a.payload.StubSize }

// checkDir validates the destination chosen on the wizard page.
func (a *app) checkDir(dir string) error {
	if dir == "" {
		return errors.New("Enter a destination folder.")
	}
	f, err := a.sys.Folders(wininstall.PerMachine)
	if err != nil {
		return err
	}
	var protected []string
	for _, v := range f {
		protected = append(protected, v, filepath.Dir(v))
	}
	if err := wininstall.CheckInstallDir(dir, protected); err != nil {
		return err
	}
	if r, err := wininstall.ReadReceipt(dir); err == nil && !strings.EqualFold(r.Product.ID, a.product.ID) {
		return fmt.Errorf("%s already holds %s. Choose another folder.", dir, r.Product.Name)
	}
	// Free space on the volume of the nearest existing parent.
	probe := dir
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(u16(probe), &free, nil, nil); err == nil && free < uint64(a.needBytes()) {
		return fmt.Errorf("There is not enough free disk space on %s: %.1f MB needed, %.1f MB free.", filepath.VolumeName(dir), float64(a.needBytes())/(1<<20), float64(free)/(1<<20))
	}
	return nil
}

// launch starts the product after an interactive install, never elevated:
// an elevated first start would create the data folder as administrator.
func (a *app) launch() {
	l := a.product.Launch
	if l == nil {
		return
	}
	f, err := a.sys.Folders(a.scope)
	if err != nil {
		return
	}
	e := wininstall.Expander{Folders: f, App: a.installedDir, Product: a.product}
	target, err := e.Expand(l.Target)
	if err != nil {
		a.logf("launch: %v", err)
		return
	}
	if a.sys.IsElevated() {
		// Explorer runs the program in the desktop user's own, unelevated session.
		explorer := filepath.Join(os.Getenv("SystemRoot"), "explorer.exe")
		cmd := exec.Command(explorer, target)
		if err := cmd.Start(); err != nil {
			a.logf("launch: %v", err)
		}
		return
	}
	cmd := exec.Command(target, l.Args...)
	cmd.Dir = filepath.Dir(target)
	if err := cmd.Start(); err != nil {
		a.logf("launch: %v", err)
	}
}

func (a *app) help(uninstaller bool) int {
	text := wininstall.Usage(a.product, uninstaller)
	if !a.args.Silent {
		messageBox(0, text, a.title(), mbOK|mbIconInfo)
	}
	fmt.Println(text)
	return wininstall.ExitOK
}

// ---- uninstaller -------------------------------------------------------------

func (a *app) runUninstaller() int {
	a.dir = filepath.Dir(a.exe)
	if a.args.Dir != "" {
		a.dir = a.args.Dir
	}
	r, err := wininstall.ReadReceipt(a.dir)
	if err != nil {
		return fail(a.args, "No installation was found in "+a.dir+" ("+err.Error()+").", wininstall.ExitFailed)
	}
	a.receipt, a.product = r, &r.Product
	if a.args.Help {
		return a.help(true)
	}
	if a.args.Shots != "" {
		return a.screenshots(a.args.Shots)
	}
	// Elevate first, then take the lock (see runInstaller).
	switch wininstall.NeedElevation(r.ScopeValue(), a.sys.IsElevated(), a.args) {
	case wininstall.ElevationRelaunch:
		return relaunchElevated(a.exe, wininstall.ElevatedArgs(os.Args[1:]), a.args.Silent)
	case wininstall.ElevationFail:
		return fail(a.args, wininstall.ErrNeedsAdmin.Error(), wininstall.ExitNeedsAdmin)
	}
	release, ok := singleInstance(a.product.ID)
	if !ok {
		return fail(a.args, a.product.Name+" Setup is already running.", wininstall.ExitFailed)
	}
	defer release()
	a.openLog("uninstall")
	defer a.closeLog()
	if a.args.Silent {
		res, err := a.doUninstall(context.Background(), a.args.RemoveData, nil)
		code := wininstall.ExitCode(res, err)
		if err != nil {
			a.logf("FAILED: %v", err)
		}
		a.logf("Exit code %d", code)
		return code
	}
	w := newWizard(a, true)
	return w.run()
}

func (a *app) doUninstall(ctx context.Context, removeData bool, progress func(string, float64)) (*wininstall.Result, error) {
	res, err := a.installer(progress).Uninstall(ctx, a.dir, wininstall.UninstallOptions{RemoveData: removeData, Self: a.exe})
	if err == nil {
		a.scheduleSelfDelete()
	}
	return res, err
}

// removalText says what the uninstaller removes.
func (a *app) removalText() string { return wininstall.RemovalText(a.receipt) }
func (a *app) dataText() string {
	if a.receipt == nil || len(a.receipt.DataDirs) == 0 {
		return "This product keeps no separate data."
	}
	return "Configuration and data are kept unless you tick the box, so a later reinstall continues where you left off:\r\n      " +
		strings.Join(a.receipt.DataDirs, "\r\n      ")
}

// scheduleSelfDelete removes uninstall.exe and the (then empty) install
// folder after this process exits: a running program cannot delete its
// own file. A hidden cmd.exe retries for up to a minute.
func (a *app) scheduleSelfDelete() {
	self := a.exe
	if !strings.EqualFold(filepath.Dir(self), filepath.Clean(a.dir)) {
		return // run from elsewhere (/D=): nothing of ours to delete
	}
	if strings.ContainsAny(self, `%"`) {
		if a.sys.DeleteOnReboot(self) == nil {
			_ = a.sys.DeleteOnReboot(a.dir)
		}
		return
	}
	script := fmt.Sprintf(`/d /q /c "for /l %%i in (1,1,60) do @(ping -n 2 127.0.0.1 >nul & del /f /q "%s" 2>nul & if not exist "%s" (rmdir "%s" 2>nul & exit /b 0))"`,
		self, self, a.dir)
	comspec := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	cmd := exec.Command(comspec)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       `"` + comspec + `" ` + script,
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Dir = os.TempDir()
	if err := cmd.Start(); err != nil {
		a.logf("self-delete: %v", err)
		_ = a.sys.DeleteOnReboot(self)
		return
	}
	a.logf("%s is removed when this program exits", self)
	_ = cmd.Process.Release()
}

// relaunchElevated runs this program again with "runas" (UAC prompt) and
// returns its exit code. silent suppresses the error message box.
func relaunchElevated(exe string, args []string, silent bool) int {
	params := wininstall.ComposeArgs(args)
	sei := shellExecuteInfo{fMask: 0x40 /*SEE_MASK_NOCLOSEPROCESS*/, lpVerb: u16("runas"), lpFile: u16(exe), lpParameters: u16(params), nShow: swShowNormal}
	sei.cbSize = uint32(unsafe.Sizeof(sei))
	if r, _, err := pShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei))); r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return wininstall.ExitCancelled
		}
		if !silent {
			messageBox(0, "Administrator rights are required: "+err.Error(), "Setup", mbOK|mbIconError)
		}
		return wininstall.ExitNeedsAdmin
	}
	defer windows.CloseHandle(sei.hProcess)
	_, _ = windows.WaitForSingleObject(sei.hProcess, windows.INFINITE)
	var code uint32
	if err := windows.GetExitCodeProcess(sei.hProcess, &code); err != nil {
		return wininstall.ExitFailed
	}
	return int(code)
}

func toCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
