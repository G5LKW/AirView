//go:build !windows

package selfupdate

// serviceControl returns the restarter and liveness probe for a systemd
// unit.
func serviceControl(unit string) (Restarter, Probe) {
	return SystemdRestarter{Unit: unit}, SystemdProbe{Unit: unit}
}
