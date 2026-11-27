//go:build windows && setupadmin

package main

// Icon, version information and a requireAdministrator manifest (opt-in:
// go build -tags setupadmin). Every start then raises a UAC prompt, also
// for a per-user install (/CURRENTUSER).
import _ "airview/cmd/airview-setup/res/admin"
