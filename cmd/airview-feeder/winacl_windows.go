package main

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// Access control of the Windows feeder (DESIGN §12.3: "%ProgramData%\AirView
// Feeder ACL-limited").
//
// The data directory holds identity.key (the feeder's private key), the
// configuration (enrol tokens, the status page password hash) and the
// spool. After "service install" it is readable by SYSTEM, Administrators
// and the service's own account only; the logs subdirectory is also
// readable by local users, so the tray's "Open logs" works without
// elevation. Nothing else is changed: directories that do not look like the
// feeder's are refused (aclSafeDir), so "-data C:\" can never re-permission
// a drive.

// fileModify is FILE_GENERIC_READ|WRITE|EXECUTE|DELETE ("Modify" in the
// Explorer dialog), without WRITE_DAC / WRITE_OWNER.
const fileModify = "0x1301bf"

// serviceSID returns the SID of the virtual account NT SERVICE\<name>:
// S-1-5-80 followed by the SHA-1 of the upper-case service name in UTF-16LE
// as five little-endian 32-bit sub-authorities (the documented derivation;
// TestServiceSIDMatchesLookup checks it against LookupAccountName).
func serviceSID(name string) string {
	u := utf16.Encode([]rune(strings.ToUpper(name)))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	sum := sha1.Sum(b)
	var sb strings.Builder
	sb.WriteString("S-1-5-80")
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&sb, "-%d", binary.LittleEndian.Uint32(sum[4*i:]))
	}
	return sb.String()
}

// dataDirSDDL is the protected DACL of the data directory: SYSTEM and
// Administrators full control, the service account (if any) modify.
func dataDirSDDL(accountSID string) string {
	s := "D:(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	if accountSID != "" {
		s += "(A;OICI;" + fileModify + ";;;" + accountSID + ")"
	}
	return s
}

// logDirExtraSDDL is added (inheriting the rest) to the logs directory:
// local users may read the logs. They never contain secrets (Logf callers
// redact them).
const logDirExtraSDDL = "D:(A;OICI;0x1200a9;;;BU)"

// serviceDACL is the service object's DACL: the Windows default plus, with
// userControl, start/stop/pause for interactive users so the tray can
// pause and restart the feeder without a UAC prompt.
func serviceDACL(userControl bool) string {
	iu := "CCLCSWLOCRRC"
	if userControl {
		iu = "CCLCSWRPWPDTLOCRRC"
	}
	return "D:(A;;CCLCSWRPWPDTLOCRRC;;;SY)(A;;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;BA)(A;;" + iu + ";;;IU)(A;;CCLCSWLOCRRC;;;SU)(A;;CCLCSWLOCRRC;;;AU)"
}

// aclSafeDir refuses directories whose permissions the feeder must not
// replace: drive roots, system and profile folders and their parents, and
// existing non-empty directories that hold nothing of the feeder's and
// whose name does not mention AirView.
func aclSafeDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	if vol := filepath.VolumeName(abs); abs == vol || abs == vol+`\` {
		return fmt.Errorf("%s is a drive root", abs)
	}
	for _, k := range []string{"ProgramData", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "SystemRoot", "windir",
		"USERPROFILE", "PUBLIC", "ALLUSERSPROFILE", "LOCALAPPDATA", "APPDATA", "TEMP", "TMP"} {
		v := os.Getenv(k)
		if v == "" {
			continue
		}
		sys := filepath.Clean(v)
		if strings.EqualFold(abs, sys) || isParentDir(abs, sys) {
			return fmt.Errorf("%s is a system or profile folder", abs)
		}
	}
	ents, err := os.ReadDir(abs)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(ents) == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToLower(filepath.Base(abs)), "airview") {
		return nil
	}
	known := map[string]bool{strings.ToLower(configFileName): true}
	for _, n := range ownFiles {
		known[strings.ToLower(n)] = true
	}
	for _, e := range ents {
		if known[strings.ToLower(e.Name())] {
			return nil
		}
	}
	return fmt.Errorf("%s already contains other files and is not an AirView directory; use a dedicated directory or -acl=false", abs)
}

// isParentDir reports whether parent is a strict ancestor of child.
func isParentDir(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// setDirDACL applies sddl's DACL to dir (protected: inherited entries are
// dropped) and lets Windows propagate it to the existing contents.
func setDirDACL(dir, sddl string, protected bool) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("SDDL %q: %w", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protected {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("set permissions of %s: %w", dir, err)
	}
	return nil
}

// secureDataDir creates dataDir (and logDir) and restricts them: see the
// file comment. accountSID is the service account ("" for LocalSystem,
// which SYSTEM covers).
func secureDataDir(dataDir, logDir, accountSID string) error {
	if err := aclSafeDir(dataDir); err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	if err := setDirDACL(dataDir, dataDirSDDL(accountSID), true); err != nil {
		return err
	}
	if logDir == "" {
		return nil
	}
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return err
	}
	if isParentDir(dataDir, logDir) {
		return setDirDACL(logDir, logDirExtraSDDL, false)
	}
	if err := aclSafeDir(logDir); err != nil {
		return err
	}
	return setDirDACL(logDir, strings.Replace(dataDirSDDL(accountSID), "D:", "D:(A;OICI;0x1200a9;;;BU)", 1), true)
}

// secureDirForUser restricts a data directory the tray creates for its own
// feeder (portable mode, no service): SYSTEM, Administrators and the current
// user. A later "service install" replaces it with the service layout.
func secureDirForUser(dir string) error {
	if err := aclSafeDir(dir); err != nil {
		return err
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return setDirDACL(dir, "D:(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;"+tu.User.Sid.String()+")", true)
}

// setServiceDACL replaces the DACL of an opened service.
func setServiceDACL(h windows.Handle, userControl bool) error {
	sd, err := windows.SecurityDescriptorFromString(serviceDACL(userControl))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(h, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
