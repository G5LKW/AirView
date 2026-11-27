#!/bin/sh
# Removes the AirView station service and program (/opt/airview). Data in /var/lib/airview and the
# operator configuration in /etc/airview are kept unless you pass --purge. Safe to run again.
#
#   sudo ./uninstall.sh            keep data and configuration
#   sudo ./uninstall.sh --purge    also delete /var/lib/airview, /etc/airview and the airview user
set -eu

PURGE=0
for a in "$@"; do
    case "$a" in
        --purge) PURGE=1 ;;
        -h | --help) sed -n '2,7p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "unknown option: $a" >&2; exit 2 ;;
    esac
done

R="${AIRVIEW_DESTDIR:-}"
if [ -z "$R" ] && [ "$(id -u)" -ne 0 ]; then
    echo "Please run as root: sudo $0 [--purge]" >&2
    exit 1
fi

systemctl stop airview 2>/dev/null || true
systemctl disable airview 2>/dev/null || true
rm -f "$R/etc/systemd/system/airview.service"
systemctl daemon-reload 2>/dev/null || true
rm -rf "$R/opt/airview"

if [ "$PURGE" -eq 1 ]; then
    rm -rf "$R/var/lib/airview" "$R/etc/airview"
    userdel airview 2>/dev/null || true
    echo "AirView removed, including all data."
else
    echo "AirView removed. Data kept in /var/lib/airview, configuration in /etc/airview (use --purge to delete them)."
fi
