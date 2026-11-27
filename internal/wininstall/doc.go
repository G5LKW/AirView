// Package wininstall is the engine of AirView's own Windows installer
// (cmd/airview-setup): payload format, builder, install / upgrade /
// uninstall logic, and the Windows side effects (registry, shell links,
// firewall, services). Pure Go, CGO_ENABLED=0, x/sys/windows only.
//
// # Installer file
//
// An installer is the setup stub (a normal PE) followed by an overlay: a ZIP
// payload (product.json and files/<install path>), padding to 8 bytes and a
// 16-byte trailer (payload length, padding length, "AVSETUP1"). Windows
// ignores the overlay; the stub finds it at the end of its own file. When an
// Authenticode signature is appended later, the PE security directory says
// where the overlay ends, so signed installers open too. Build writes
// installers deterministically (fixed timestamps, sorted entries); OpenPayload
// reads them and rejects unsafe names (zip slip).
//
// # Product definitions
//
// Product (deploy/installer/products/*.json) describes what to install and
// which system integration to perform: Start-menu / desktop shortcuts (.lnk
// through IShellLinkW, .url files), the uninstall entry, an optional firewall
// rule, a Windows service registered by the product's own command (the
// feeder's "service install", which also sets ACLs and recovery actions), a
// tray Run value, a configuration command fed from installer switches, a
// purge command and the data folders. Strings use tokens ({app}, {pf},
// {commonappdata}, {userappdata}, {programs}, {desktop}, {port}, ...) and
// product Vars, so one definition serves the release and scratch test
// identities.
//
// # Install, upgrade, uninstall
//
// Installer.Install copies the files transactionally (new file beside the
// old one, old one renamed away, rollback on error or cancel), removes
// files an earlier version had and this one has not, writes
// install-receipt.json (exactly what was installed), uninstall.exe (the bare
// stub), shortcuts and the uninstall entry, then the firewall rule, the
// configuration, the service and the tray autostart. Steps after the files
// that fail are reported as Result.StepErrors (exit code 1) without undoing
// the install. Data folders are never touched by install or upgrade.
//
// Installer.Uninstall reverses the receipt: closes the tray, removes the
// service (product command, then the SCM directly), the Run value, the
// firewall rule, shortcuts, files, the uninstall entry, and with
// RemoveData the data folders (after the purge command). Files in use are
// renamed and deleted at the next restart (exit code 3010). Files the
// installer did not create are kept, and so is the folder holding them.
//
// Per-machine installs (Program Files, HKLM, service, firewall) need
// administrator rights; per-user installs (/CURRENTUSER: %LOCALAPPDATA%\
// Programs, HKCU, no service or firewall rule) do not.
//
// See WIRING.md for the command line, the files and the test procedure.
package wininstall
