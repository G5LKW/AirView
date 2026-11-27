#!/bin/sh
# /usr/local/bin/airview - command-line front end of an AirView server install
# (deploy/server/install.sh). It runs /opt/airview/airview with the service's
# configuration (AIRVIEW_CONFIG=/etc/airview/airview.jsonc,
# AIRVIEW_DATA=/var/lib/airview unless already set) as the airview account, so
# files it creates in the data directory belong to the service. Examples:
#
#   sudo airview config check          validate the configuration
#   sudo airview config print -sources effective settings and where they come from
#   sudo airview admin init            create the first aggregator admin
#   sudo airview backup                back up data + configuration
#   sudo airview restore FILE          restore a backup
#   sudo airview db profile            database profile in force (storage.profile)
#   sudo airview db migrate --status   progress of a storage-layout migration
#   sudo airview upgrade [...]         upgrade (see /opt/airview/upgrade.sh -h)
#   sudo airview uninstall [--purge]   remove AirView
#   sudo airview status | logs         systemctl status / journalctl -f
set -eu

PREFIX=/opt/airview
: "${AIRVIEW_CONFIG:=/etc/airview/airview.jsonc}"
: "${AIRVIEW_DATA:=/var/lib/airview}"
export AIRVIEW_CONFIG AIRVIEW_DATA

case "${1:-}" in
    backup | restore | upgrade | uninstall)
        sub="$1"
        shift
        exec "$PREFIX/$sub.sh" "$@"
        ;;
    status)
        exec systemctl status airview --no-pager
        ;;
    logs)
        shift
        exec journalctl -u airview -f "$@"
        ;;
esac

uid="$(id -u)"
if [ "$uid" -eq 0 ]; then
    if command -v runuser >/dev/null 2>&1; then
        exec runuser -u airview -- "$PREFIX/airview" "$@"
    fi
    exec setpriv --reuid=airview --regid=airview --init-groups -- "$PREFIX/airview" "$@"
fi
if [ "$(id -un)" != airview ] && [ ! -r "$AIRVIEW_CONFIG" ]; then
    echo "airview: $AIRVIEW_CONFIG is not readable by $(id -un); run with sudo" >&2
    exit 1
fi
exec "$PREFIX/airview" "$@"
