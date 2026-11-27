// Package admin carries the Windows resources of the opt-in
// requireAdministrator setup program (go build -tags setupadmin): icon,
// version information and a manifest requesting administrator rights. rsrc_windows_*.syso are generated from winres.json by
// deploy/installer/build-installers.ps1 -Resources (go-winres); they are checked in.
package admin
