//go:build windows

package wininstall

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestInstallDirSDDL(t *testing.T) {
	sd, err := windows.SecurityDescriptorFromString(installDirSDDL)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sd.DACL(); err != nil {
		t.Fatal(err)
	}
	ctl, _, err := sd.Control()
	if err != nil || ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL not protected: %v %v", ctl, err)
	}
	if trustedOwner(nil) {
		t.Fatal("nil owner trusted")
	}
	sys, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	users, _ := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if !trustedOwner(sys) || trustedOwner(users) {
		t.Fatal("owner check")
	}
}
