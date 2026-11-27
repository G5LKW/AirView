// Package pi holds no Go code of its own: deploy/pi is the Raspberry Pi
// station bundle (airview-pi-<version>.tar.gz, built by deploy/build.ps1):
// install.sh (versioned, idempotent install/upgrade with automatic roll-back
// and a --base-url download mode), update.sh, uninstall.sh, the systemd unit
// and the README. The SD-card image (cmd/airview-piimage) reuses install.sh
// and airview.service unchanged.
//
// The tests run the scripts in a scratch root (AIRVIEW_DESTDIR) with stub
// system commands and a fake airview binary. They need a POSIX sh and tar
// (on Windows: Git for Windows) and are skipped where these are missing.
package pi
