// Package server holds no Go code of its own: deploy/server is the Linux
// server installation kit (DESIGN §20.1) - install.sh, upgrade.sh,
// uninstall.sh, backup.sh, restore.sh, the systemd unit, the logrotate rule
// and reverse-proxy examples - and this package's tests exercise those
// scripts in a scratch root (AIRVIEW_DESTDIR) with stub system commands.
// The tests also check that the Docker build context of the repository's
// Dockerfile contains every package the binaries need.
//
// The tests need a POSIX sh and GNU tar (on Windows: Git for Windows); they
// are skipped where these are missing.
package server
