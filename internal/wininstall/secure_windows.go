//go:build windows

package wininstall

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// installDirSDDL is the DACL of a per-machine install folder (SEC-R5):
// SYSTEM and Administrators full control, Users read and execute, no
// inherited entries (a custom folder such as D:\Apps would otherwise
// inherit "Authenticated Users: modify" from the drive root, letting a
// standard user replace the service binary that runs as SYSTEM).
const installDirSDDL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)"

// trustedInstallerSID owns the Windows-shipped folders.
const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// SecureInstallDir implements dirSecurer: it makes Administrators the
// owner of dir when someone else owns it and applies installDirSDDL.
func (WindowsSystem) SecureInstallDir(dir string) error {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading the owner of %s: %w", dir, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("reading the owner of %s: %w", dir, err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var newOwner *windows.SID
	if !trustedOwner(owner) {
		info |= windows.OWNER_SECURITY_INFORMATION
		newOwner = admins
	}
	want, err := windows.SecurityDescriptorFromString(installDirSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := want.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, info, newOwner, nil, dacl, nil); err != nil {
		return fmt.Errorf("securing %s: %w", dir, err)
	}
	return nil
}

func trustedOwner(s *windows.SID) bool {
	if s == nil {
		return false
	}
	if s.IsWellKnown(windows.WinBuiltinAdministratorsSid) || s.IsWellKnown(windows.WinLocalSystemSid) {
		return true
	}
	return s.String() == trustedInstallerSID
}
