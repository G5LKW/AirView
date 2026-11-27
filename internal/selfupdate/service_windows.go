//go:build windows

package selfupdate

// serviceControl returns the restarter and liveness probe for a Windows
// service.
func serviceControl(name string) (Restarter, Probe) {
	return WindowsServiceRestarter{Name: name}, WindowsServiceProbe{Name: name}
}
