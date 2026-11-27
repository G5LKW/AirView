package wininstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// fakeSystem records every side effect; folders live in a temp directory.
type fakeSystem struct {
	mu        sync.Mutex
	root      string
	elevated  bool
	uninst    map[string]UninstallInfo // scope/id
	run       map[string]string        // scope/name
	firewall  map[string]string        // rule -> program:port
	services  map[string]bool          // name -> running
	execs     []string
	execFn    func(exe string, args []string) (string, int, error)
	reboot    []string
	shortcuts map[string]ShortcutTarget
}

func newFake(root string) *fakeSystem {
	return &fakeSystem{root: root, elevated: true, uninst: map[string]UninstallInfo{}, run: map[string]string{},
		firewall: map[string]string{}, services: map[string]bool{}, shortcuts: map[string]ShortcutTarget{}}
}

func (f *fakeSystem) Folders(scope Scope) (map[string]string, error) {
	s := scope.String()
	m := map[string]string{
		FolderProgramFiles:  filepath.Join(f.root, s, "pf"),
		FolderCommonAppData: filepath.Join(f.root, "programdata"),
		FolderUserAppData:   filepath.Join(f.root, "appdata"),
		FolderLocalAppData:  filepath.Join(f.root, "localappdata"),
		FolderPrograms:      filepath.Join(f.root, s, "programs"),
		FolderDesktop:       filepath.Join(f.root, s, "desktop"),
		FolderTemp:          filepath.Join(f.root, "tmp"),
		FolderSystem:        filepath.Join(f.root, "sys"),
	}
	return m, nil
}

func (f *fakeSystem) IsElevated() bool { return f.elevated }

func key(scope Scope, s string) string { return scope.String() + "/" + s }

func (f *fakeSystem) ReadUninstall(scope Scope, id string) (UninstallInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.uninst[key(scope, id)]
	return i, ok, nil
}
func (f *fakeSystem) WriteUninstall(scope Scope, id string, i UninstallInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uninst[key(scope, id)] = i
	return nil
}
func (f *fakeSystem) DeleteUninstall(scope Scope, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.uninst, key(scope, id))
	return nil
}
func (f *fakeSystem) SetRun(scope Scope, name, cmd string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.run[key(scope, name)] = cmd
	return nil
}
func (f *fakeSystem) DeleteRun(scope Scope, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.run, key(scope, name))
	return nil
}
func (f *fakeSystem) RunValue(scope Scope, name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.run[key(scope, name)]
	return v, ok
}
func (f *fakeSystem) CreateShortcut(lnk string, s ShortcutTarget) error {
	if err := os.MkdirAll(filepath.Dir(lnk), 0o755); err != nil {
		return err
	}
	f.mu.Lock()
	f.shortcuts[lnk] = s
	f.mu.Unlock()
	return os.WriteFile(lnk, []byte("lnk:"+s.Target), 0o644)
}
func (f *fakeSystem) AddFirewallRule(rule, program string, port int, profiles string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.firewall[rule] = fmt.Sprintf("%s:%d:%s", program, port, profiles)
	return nil
}
func (f *fakeSystem) DeleteFirewallRule(rule string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.firewall, rule)
	return nil
}
func (f *fakeSystem) ServiceExists(name string) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.services[name]
	return ok, r, nil
}
func (f *fakeSystem) StopService(name string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.services[name]; ok {
		f.services[name] = false
	}
	return nil
}
func (f *fakeSystem) DeleteService(name string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.services, name)
	return nil
}

// Exec emulates the feeder's service commands.
func (f *fakeSystem) Exec(exe string, args []string, _ time.Duration) (string, int, error) {
	f.mu.Lock()
	f.execs = append(f.execs, filepath.Base(exe)+" "+strings.Join(args, " "))
	fn := f.execFn
	f.mu.Unlock()
	if fn != nil {
		return fn(exe, args)
	}
	if len(args) >= 4 && args[0] == "service" && args[2] == "-name" {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch args[1] {
		case "install":
			f.services[args[3]] = true
		case "remove":
			delete(f.services, args[3])
		}
	}
	return "ok", 0, nil
}
func (f *fakeSystem) DeleteOnReboot(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reboot = append(f.reboot, p)
	return nil
}
