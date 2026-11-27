package feederui

import "testing"

// TestHostAllowedOwnName: the machine's own name and name.local are
// accepted (Pi image mDNS), other names are not (DNS rebinding).
func TestHostAllowedOwnName(t *testing.T) {
	old := osHostname
	defer func() { osHostname = old }()
	osHostname = func() (string, error) { return "AirView-Feeder", nil }
	for h, want := range map[string]bool{
		"airview-feeder.local:8099": true, "airview-feeder": true, "AIRVIEW-FEEDER.LOCAL.": true,
		"127.0.0.1:8099": true, "localhost": true, "evil.example": false, "airview-feeder.example": false,
	} {
		if got := hostAllowed(h); got != want {
			t.Errorf("%s: %v, want %v", h, got, want)
		}
	}
}
