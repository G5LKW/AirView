// Command airview-setup is AirView's own Windows installer (no Inno Setup,
// no NSIS, nothing downloaded): the same executable is the installer stub
// and, without a payload, the uninstaller.
//
// An installer is this program followed by a payload (product.json plus the
// files to install; see internal/wininstall). It is assembled by
// ./cmd/airview-setup/mkinstaller, normally through
// deploy/installer/build-installers.ps1, which produces
// AirView-Setup-<ver>-windows-<arch>.exe and
// AirView-Feeder-Setup-<ver>-windows-<arch>.exe.
//
// Started with a payload it shows a wizard (welcome, licence, destination,
// options, ready, progress, finished) or, with /S, installs silently; with
// no payload it reads install-receipt.json next to itself and uninstalls.
// The command-line switches are documented on wininstall.Args and shown by
// /?.
//
// Windows resources: the release build links res/user (icon, version info
// and a manifest with requestedExecutionLevel asInvoker). A per-user
// install (/CURRENTUSER) therefore runs without UAC, also for a standard
// user; a per-machine install or uninstall relaunches itself with "runas"
// (one UAC prompt, also in silent mode /S). With -tags setupadmin it links
// res/admin instead (requireAdministrator: UAC at every start). DLV-06.
//
// Build (Windows GUI subsystem):
//
//	go build -trimpath -ldflags "-H windowsgui -s -w" -o airview-setup.exe ./cmd/airview-setup
package main
