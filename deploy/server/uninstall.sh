#!/bin/sh
# Removes an AirView server installed with install.sh.
#
#   sudo airview uninstall            service, binary, scripts, unit, logrotate rule, wrapper
#   sudo airview uninstall --purge    ... and the data (/var/lib/airview), the
#                                     configuration (/etc/airview) and the airview account
#
# Backups in /var/backups/airview are never deleted by this script.
set -eu

usage() {
    cat <<'EOF'
Usage: uninstall.sh [--purge] [--yes]
  --purge   also delete /var/lib/airview, /etc/airview and the airview account
  --yes     do not ask before --purge deletes data
EOF
}

HERE="$(cd "$(dirname "$0")" && pwd)"
# This script may run from /opt/airview, which it removes below; the shell
# keeps reading the already-open (unlinked) file, so that is safe.
# shellcheck source=lib.sh
. "$HERE/lib.sh"

PURGE=0
YES=0
while [ $# -gt 0 ]; do
    case "$1" in
        --purge) PURGE=1; shift ;;
        --yes | -y) YES=1; shift ;;
        -h | --help) usage; exit 0 ;;
        *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
    esac
done

need_root

if [ "$PURGE" -eq 1 ] && [ "$YES" -eq 0 ]; then
    if [ -t 0 ]; then
        printf 'Delete ALL AirView data in %s and %s? This cannot be undone. [y/N] ' "$AV_REAL_DATA" /etc/airview
        read -r answer
        case "$answer" in y | Y | yes | YES) ;; *) echo "Aborted; nothing changed."; exit 1 ;; esac
    else
        die "not a terminal: pass --yes together with --purge"
    fi
fi

info "Stopping and removing the service"
systemctl stop "$AV_SERVICE" 2>/dev/null || true
systemctl disable "$AV_SERVICE" 2>/dev/null || true
rm -f "$AV_UNIT"
rm -rf "$AV_UNIT.d"
systemctl daemon-reload 2>/dev/null || true
systemctl reset-failed "$AV_SERVICE" 2>/dev/null || true

rm -f "$AV_LOGROTATE"
# Only remove the wrapper if it is ours.
if [ -f "$AV_WRAPPER" ] && grep -q 'deploy/server/install.sh' "$AV_WRAPPER" 2>/dev/null; then
    rm -f "$AV_WRAPPER"
fi
rm -rf "$AV_PREFIX"

if [ "$PURGE" -eq 1 ]; then
    rm -rf "$AV_DATA" "$AV_ETC"
    if id "$AV_USER" >/dev/null 2>&1; then
        userdel "$AV_USER" 2>/dev/null || warn "could not remove the account $AV_USER"
    fi
    info "AirView removed, including all data and configuration."
else
    info "AirView removed. Kept: $AV_REAL_DATA (data), /etc/airview (configuration), the airview account."
    note "Reinstalling with install.sh picks them up again; remove them with: uninstall.sh --purge"
fi
if [ -d "$AV_BACKUPS" ]; then
    note "Backups are kept in /var/backups/airview."
fi
