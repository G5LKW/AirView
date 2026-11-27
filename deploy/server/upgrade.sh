#!/bin/sh
# Upgrades an AirView server installed with install.sh.
#
# From a newly unpacked bundle:
#   cd airview-server-linux-amd64 && sudo ./upgrade.sh
# Or download and verify the bundle for this CPU (SHA256SUMS) from a release site:
#   sudo /opt/airview/upgrade.sh --base-url https://<release-site>
#
# Steps: the new binary validates the existing configuration (abort if it is
# rejected, nothing changed) -> service stop -> cold backup to
# /var/backups/airview (airview-backup-*-pre-upgrade-<old version>-*.tar.gz)
# -> binary, unit, scripts replaced (old binary kept as /opt/airview/airview.prev)
# -> start -> health check on /healthz -> automatic rollback of the binary if
# the new version does not become healthy within 90 s.
#
# Options: --base-url URL, --no-backup, --no-rollback, --force, --no-start (see install.sh -h).
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
case "${1:-}" in
    -h | --help)
        sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
esac
# With --base-url the binary next to this script is not needed (install.sh
# downloads the bundle and runs its own install.sh --upgrade).
exec sh "$HERE/install.sh" --upgrade "$@"
