package wininstall

import (
	"errors"
	"time"
)

// System is every side effect of an install outside the install directory
// (and the uninstaller copy). sys_windows.go implements it with the
// registry, COM, netsh and the service control manager; the tests use a
// fake that records the calls.
type System interface {
	// Folders resolves the folder tokens (FolderProgramFiles, ...) for a scope.
	Folders(scope Scope) (map[string]string, error)
	// IsElevated reports whether the process has administrator rights.
	IsElevated() bool

	ReadUninstall(scope Scope, id string) (UninstallInfo, bool, error)
	WriteUninstall(scope Scope, id string, info UninstallInfo) error
	DeleteUninstall(scope Scope, id string) error

	// SetRun / DeleteRun manage a value under Software\Microsoft\Windows\
	// CurrentVersion\Run (HKLM for PerMachine, HKCU for PerUser).
	SetRun(scope Scope, name, command string) error
	DeleteRun(scope Scope, name string) error
	RunValue(scope Scope, name string) (string, bool)

	// CreateShortcut writes a .lnk file.
	CreateShortcut(lnk string, s ShortcutTarget) error

	AddFirewallRule(rule, program string, port int, profiles string) error
	DeleteFirewallRule(rule string) error

	ServiceExists(name string) (exists, running bool, err error)
	StopService(name string, timeout time.Duration) error
	// DeleteService stops (if needed) and deletes a service; the fallback
	// when the product's own remove command is unavailable or fails.
	DeleteService(name string, timeout time.Duration) error

	// Exec runs a program without a window and returns its combined output.
	Exec(exe string, args []string, timeout time.Duration) (output string, exitCode int, err error)

	// DeleteOnReboot schedules a file or empty directory for deletion at
	// the next restart (MoveFileEx MOVEFILE_DELAY_UNTIL_REBOOT).
	DeleteOnReboot(path string) error
}

// ShortcutTarget is what a .lnk points at.
type ShortcutTarget struct {
	Target      string
	Args        string // already a command-line string
	WorkDir     string
	Icon        string
	IconIndex   int
	Description string
}

// UninstallInfo are the values of an Uninstall registry key.
type UninstallInfo struct {
	DisplayName          string
	DisplayIcon          string
	DisplayVersion       string
	Publisher            string
	UninstallString      string
	QuietUninstallString string
	InstallLocation      string
	InstallDate          string // yyyymmdd
	HelpLink             string
	Comments             string
	EstimatedSizeKB      uint32
	NoModify, NoRepair   bool
}

// ErrNeedsAdmin: a per-machine install or uninstall without administrator
// rights.
var ErrNeedsAdmin = errors.New("administrator rights are required for an install for all users (run elevated, or use /CURRENTUSER)")

// ErrUnsupported is returned by the System of non-Windows platforms.
var ErrUnsupported = errors.New("only supported on Windows")
