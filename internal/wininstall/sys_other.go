//go:build !windows

package wininstall

import "time"

// otherSystem lets the package (and the builder that uses it) compile on
// every platform; nothing can be installed outside Windows.
type otherSystem struct{}

// NewSystem returns the System of this platform.
func NewSystem() System { return otherSystem{} }

func (otherSystem) Folders(Scope) (map[string]string, error) { return nil, ErrUnsupported }
func (otherSystem) IsElevated() bool                         { return false }
func (otherSystem) ReadUninstall(Scope, string) (UninstallInfo, bool, error) {
	return UninstallInfo{}, false, ErrUnsupported
}
func (otherSystem) WriteUninstall(Scope, string, UninstallInfo) error { return ErrUnsupported }
func (otherSystem) DeleteUninstall(Scope, string) error               { return ErrUnsupported }
func (otherSystem) SetRun(Scope, string, string) error                { return ErrUnsupported }
func (otherSystem) DeleteRun(Scope, string) error                     { return ErrUnsupported }
func (otherSystem) RunValue(Scope, string) (string, bool)             { return "", false }
func (otherSystem) CreateShortcut(string, ShortcutTarget) error       { return ErrUnsupported }
func (otherSystem) AddFirewallRule(string, string, int, string) error { return ErrUnsupported }
func (otherSystem) DeleteFirewallRule(string) error                   { return ErrUnsupported }
func (otherSystem) ServiceExists(string) (bool, bool, error)          { return false, false, ErrUnsupported }
func (otherSystem) StopService(string, time.Duration) error           { return ErrUnsupported }
func (otherSystem) DeleteService(string, time.Duration) error         { return ErrUnsupported }
func (otherSystem) Exec(string, []string, time.Duration) (string, int, error) {
	return "", -1, ErrUnsupported
}
func (otherSystem) DeleteOnReboot(string) error { return ErrUnsupported }
