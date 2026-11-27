//go:build windows

package wininstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\`
	runKey       = `Software\Microsoft\Windows\CurrentVersion\Run`
	// EnvFolderPrefix + token (upper case) overrides a folder token, e.g.
	// AIRVIEW_SETUP_FOLDER_DESKTOP=D:\scratch\desktop. Meant for automated
	// tests of the installers, so they leave the real desktop and Start
	// menu alone; documented in WIRING.md.
	EnvFolderPrefix = "AIRVIEW_SETUP_FOLDER_"
)

// WindowsSystem is the real System.
type WindowsSystem struct{}

// NewSystem returns the System of this platform.
func NewSystem() System { return WindowsSystem{} }

func knownFolder(id *windows.KNOWNFOLDERID) string {
	p, err := windows.KnownFolderPath(id, 0)
	if err != nil {
		return ""
	}
	return p
}

// Folders implements System.
func (WindowsSystem) Folders(scope Scope) (map[string]string, error) {
	f := map[string]string{
		FolderCommonAppData: knownFolder(windows.FOLDERID_ProgramData),
		FolderUserAppData:   knownFolder(windows.FOLDERID_RoamingAppData),
		FolderLocalAppData:  knownFolder(windows.FOLDERID_LocalAppData),
		FolderTemp:          filepath.Clean(os.TempDir()),
		FolderSystem:        knownFolder(windows.FOLDERID_System),
	}
	if scope == PerMachine {
		f[FolderProgramFiles] = knownFolder(windows.FOLDERID_ProgramFiles)
		f[FolderPrograms] = knownFolder(windows.FOLDERID_CommonPrograms)
		f[FolderDesktop] = knownFolder(windows.FOLDERID_PublicDesktop)
	} else {
		f[FolderProgramFiles] = knownFolder(windows.FOLDERID_UserProgramFiles)
		if f[FolderProgramFiles] == "" && f[FolderLocalAppData] != "" {
			f[FolderProgramFiles] = filepath.Join(f[FolderLocalAppData], "Programs")
		}
		f[FolderPrograms] = knownFolder(windows.FOLDERID_Programs)
		f[FolderDesktop] = knownFolder(windows.FOLDERID_Desktop)
	}
	for k := range builtinTokens {
		if v := os.Getenv(EnvFolderPrefix + strings.ToUpper(k)); v != "" {
			if _, isFolder := f[k]; isFolder || k == FolderProgramFiles {
				f[k] = filepath.Clean(v)
			}
		}
	}
	if f[FolderProgramFiles] == "" || f[FolderCommonAppData] == "" {
		return nil, errors.New("the Program Files / ProgramData folders could not be determined")
	}
	return f, nil
}

// IsElevated implements System.
func (WindowsSystem) IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

func root(scope Scope) registry.Key {
	if scope == PerUser {
		return registry.CURRENT_USER
	}
	return registry.LOCAL_MACHINE
}

// ReadUninstall implements System.
func (WindowsSystem) ReadUninstall(scope Scope, id string) (UninstallInfo, bool, error) {
	k, err := registry.OpenKey(root(scope), uninstallKey+id, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return UninstallInfo{}, false, nil
	}
	if err != nil {
		return UninstallInfo{}, false, err
	}
	defer k.Close()
	s := func(n string) string { v, _, _ := k.GetStringValue(n); return v }
	i := UninstallInfo{
		DisplayName: s("DisplayName"), DisplayIcon: s("DisplayIcon"), DisplayVersion: s("DisplayVersion"),
		Publisher: s("Publisher"), UninstallString: s("UninstallString"), QuietUninstallString: s("QuietUninstallString"),
		InstallLocation: s("InstallLocation"), InstallDate: s("InstallDate"), HelpLink: s("HelpLink"), Comments: s("Comments"),
	}
	if v, _, err := k.GetIntegerValue("EstimatedSize"); err == nil {
		i.EstimatedSizeKB = uint32(v)
	}
	if v, _, err := k.GetIntegerValue("NoModify"); err == nil {
		i.NoModify = v != 0
	}
	if v, _, err := k.GetIntegerValue("NoRepair"); err == nil {
		i.NoRepair = v != 0
	}
	return i, true, nil
}

// WriteUninstall implements System.
func (WindowsSystem) WriteUninstall(scope Scope, id string, i UninstallInfo) error {
	k, _, err := registry.CreateKey(root(scope), uninstallKey+id, registry.SET_VALUE|registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return err
	}
	defer k.Close()
	str := map[string]string{
		"DisplayName": i.DisplayName, "DisplayIcon": i.DisplayIcon, "DisplayVersion": i.DisplayVersion,
		"Publisher": i.Publisher, "UninstallString": i.UninstallString, "QuietUninstallString": i.QuietUninstallString,
		"InstallLocation": i.InstallLocation, "InstallDate": i.InstallDate, "HelpLink": i.HelpLink, "Comments": i.Comments,
	}
	for n, v := range str {
		if v == "" {
			_ = k.DeleteValue(n)
			continue
		}
		if err := k.SetStringValue(n, v); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	dw := map[string]uint32{"EstimatedSize": i.EstimatedSizeKB, "NoModify": b2u(i.NoModify), "NoRepair": b2u(i.NoRepair)}
	for n, v := range dw {
		if err := k.SetDWordValue(n, v); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	if maj, min, ok := splitVersion(i.DisplayVersion); ok {
		_ = k.SetDWordValue("VersionMajor", maj)
		_ = k.SetDWordValue("VersionMinor", min)
	}
	return nil
}

func b2u(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func splitVersion(v string) (uint32, uint32, bool) {
	var maj, min uint32
	n, _ := fmt.Sscanf(v, "%d.%d", &maj, &min)
	return maj, min, n >= 1
}

// DeleteUninstall implements System.
func (WindowsSystem) DeleteUninstall(scope Scope, id string) error {
	err := registry.DeleteKey(root(scope), uninstallKey+id)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

// SetRun implements System.
func (WindowsSystem) SetRun(scope Scope, name, command string) error {
	k, _, err := registry.CreateKey(root(scope), runKey, registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, command)
}

// DeleteRun implements System.
func (WindowsSystem) DeleteRun(scope Scope, name string) error {
	k, err := registry.OpenKey(root(scope), runKey, registry.SET_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// RunValue implements System.
func (WindowsSystem) RunValue(scope Scope, name string) (string, bool) {
	k, err := registry.OpenKey(root(scope), runKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	return v, err == nil
}

// CreateShortcut implements System.
func (WindowsSystem) CreateShortcut(lnk string, s ShortcutTarget) error {
	if err := os.MkdirAll(filepath.Dir(lnk), 0o755); err != nil {
		return err
	}
	return createShellLink(lnk, s)
}

func netsh() string {
	return filepath.Join(knownFolderOr(windows.FOLDERID_System, `C:\Windows\System32`), "netsh.exe")
}

func knownFolderOr(id *windows.KNOWNFOLDERID, def string) string {
	if v := knownFolder(id); v != "" {
		return v
	}
	return def
}

// AddFirewallRule implements System.
func (s WindowsSystem) AddFirewallRule(rule, program string, port int, profiles string) error {
	args := []string{"advfirewall", "firewall", "add", "rule", "name=" + rule, "dir=in", "action=allow",
		"protocol=TCP", "profile=" + profiles, "program=" + program, "enable=yes"}
	if port > 0 { // 0: every port the program listens on
		args = append(args, fmt.Sprintf("localport=%d", port))
	}
	out, code, err := s.Exec(netsh(), args, 30*time.Second)
	if err != nil || code != 0 {
		return fmt.Errorf("netsh exit %d: %v %s", code, err, strings.TrimSpace(out))
	}
	return nil
}

// DeleteFirewallRule implements System. A missing rule is not an error.
func (s WindowsSystem) DeleteFirewallRule(rule string) error {
	out, code, err := s.Exec(netsh(), []string{"advfirewall", "firewall", "delete", "rule", "name=" + rule}, 30*time.Second)
	if err != nil {
		return err
	}
	if code != 0 && !strings.Contains(strings.ToLower(out), "no rules match") {
		return fmt.Errorf("netsh exit %d: %s", code, strings.TrimSpace(out))
	}
	return nil
}

func openService(name string, access uint32) (windows.Handle, windows.Handle, error) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, 0, err
	}
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		windows.CloseServiceHandle(m)
		return 0, 0, err
	}
	s, err := windows.OpenService(m, n, access)
	if err != nil {
		windows.CloseServiceHandle(m)
		return 0, 0, err
	}
	return m, s, nil
}

// ServiceExists implements System (works without administrator rights).
func (WindowsSystem) ServiceExists(name string) (bool, bool, error) {
	m, s, err := openService(name, windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	defer windows.CloseServiceHandle(m)
	defer windows.CloseServiceHandle(s)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(s, &st); err != nil {
		return true, false, err
	}
	return true, st.CurrentState != windows.SERVICE_STOPPED, nil
}

func stopAndWait(s windows.Handle, timeout time.Duration) error {
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(s, &st); err != nil {
		return err
	}
	if st.CurrentState == windows.SERVICE_STOPPED {
		return nil
	}
	if st.CurrentState != windows.SERVICE_STOP_PENDING {
		if err := windows.ControlService(s, windows.SERVICE_CONTROL_STOP, &st); err != nil &&
			!errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := windows.QueryServiceStatus(s, &st); err != nil {
			return err
		}
		if st.CurrentState == windows.SERVICE_STOPPED {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the service did not stop within %s", timeout)
}

// StopService implements System.
func (WindowsSystem) StopService(name string, timeout time.Duration) error {
	m, s, err := openService(name, windows.SERVICE_STOP|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(m)
	defer windows.CloseServiceHandle(s)
	return stopAndWait(s, timeout)
}

// DeleteService implements System.
func (WindowsSystem) DeleteService(name string, timeout time.Duration) error {
	m, s, err := openService(name, windows.SERVICE_STOP|windows.SERVICE_QUERY_STATUS|windows.DELETE)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(m)
	defer windows.CloseServiceHandle(s)
	if err := stopAndWait(s, timeout); err != nil {
		return err
	}
	if err := windows.DeleteService(s); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return err
	}
	return nil
}

// Exec implements System.
func (WindowsSystem) Exec(exe string, args []string, timeout time.Duration) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.Dir = filepath.Dir(exe)
	out, err := cmd.CombinedOutput()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		err = nil // a non-zero exit is reported through code
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	return string(out), code, err
}

// DeleteOnReboot implements System.
func (WindowsSystem) DeleteOnReboot(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(p, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

// ---- IShellLinkW ----------------------------------------------------------------

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")

	clsidShellLink  = windows.GUID{Data1: 0x00021401, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIShellLinkW  = windows.GUID{Data1: 0x000214F9, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIPersistFile = windows.GUID{Data1: 0x0000010b, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
)

// vtable slots (IUnknown: 0 QueryInterface, 1 AddRef, 2 Release).
const (
	slQueryInterface      = 0
	slRelease             = 2
	slSetDescription      = 7
	slSetWorkingDirectory = 9
	slSetArguments        = 11
	slSetShowCmd          = 15
	slSetIconLocation     = 17
	slSetPath             = 20
	pfSave                = 6 // IPersistFile::Save
)

type comObj struct{ p unsafe.Pointer }

func (o comObj) call(slot int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(o.p)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(o.p)}, args...)...)
	return r
}

func hresult(what string, r uintptr) error {
	if int32(r) < 0 {
		return fmt.Errorf("%s: HRESULT 0x%08X", what, uint32(r))
	}
	return nil
}

func createShellLink(lnk string, s ShortcutTarget) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const coinitApartmentThreaded = 0x2
	err := windows.CoInitializeEx(0, coinitApartmentThreaded)
	var e syscall.Errno
	switch {
	case err == nil, errors.As(err, &e) && e == 1: // S_OK, S_FALSE (already initialised): balanced below
		defer windows.CoUninitialize()
	case errors.As(err, &e) && uint32(e) == 0x80010106: // RPC_E_CHANGED_MODE: COM is usable as it is
	default:
		return fmt.Errorf("CoInitializeEx: %w", err)
	}
	var link unsafe.Pointer
	const clsctxInprocServer = 1
	r, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidShellLink)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIShellLinkW)), uintptr(unsafe.Pointer(&link)))
	if err := hresult("CoCreateInstance(ShellLink)", r); err != nil {
		return err
	}
	sl := comObj{link}
	defer sl.call(slRelease)
	str := func(v string) uintptr {
		p, _ := windows.UTF16PtrFromString(v)
		return uintptr(unsafe.Pointer(p))
	}
	if err := hresult("SetPath", sl.call(slSetPath, str(s.Target))); err != nil {
		return err
	}
	if s.Args != "" {
		if err := hresult("SetArguments", sl.call(slSetArguments, str(s.Args))); err != nil {
			return err
		}
	}
	if s.WorkDir != "" {
		if err := hresult("SetWorkingDirectory", sl.call(slSetWorkingDirectory, str(s.WorkDir))); err != nil {
			return err
		}
	}
	if s.Description != "" {
		d := s.Description
		if len(d) > 259 {
			d = d[:259]
		}
		if err := hresult("SetDescription", sl.call(slSetDescription, str(d))); err != nil {
			return err
		}
	}
	if s.Icon != "" {
		if err := hresult("SetIconLocation", sl.call(slSetIconLocation, str(s.Icon), uintptr(s.IconIndex))); err != nil {
			return err
		}
	}
	const swShowNormal = 1
	sl.call(slSetShowCmd, swShowNormal)
	var pf unsafe.Pointer
	if err := hresult("QueryInterface(IPersistFile)", sl.call(slQueryInterface, uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&pf)))); err != nil {
		return err
	}
	pfo := comObj{pf}
	defer pfo.call(slRelease)
	return hresult("IPersistFile.Save "+lnk, pfo.call(pfSave, str(lnk), 1))
}
