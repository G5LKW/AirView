//go:build windows && !setupadmin

package main

// Icon, version information and an asInvoker manifest: the release stub.
// A per-user install (/CURRENTUSER) runs without elevation; a per-machine
// install or uninstall relaunches itself elevated (UAC prompt) when needed
// (DLV-06).
import _ "airview/cmd/airview-setup/res/user"
