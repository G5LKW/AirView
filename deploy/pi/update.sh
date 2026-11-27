#!/bin/sh
# AirView station update (Raspberry Pi bundle).
#
# Installed as /opt/airview/update.sh. Two ways to use it:
#
#   sudo /opt/airview/update.sh [--base-url https://<release-site>] [--key <release key>] [--force]
#       downloads the newest airview-pi-<version>.tar.gz listed in the site's SHA256SUMS,
#       verifies it and upgrades (nothing happens when that version is already installed).
#       Without --base-url the site remembered by the last download install is used
#       (/etc/airview/update.env).
#
#   tar xzf airview-pi-<version>.tar.gz && sudo ./airview-pi-<version>/update.sh
#       upgrades from an unpacked bundle (same as its install.sh --upgrade).
#
# Data, settings and the operator file are kept; a new binary that does not start is rolled
# back automatically (see install.sh).
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
R="${AIRVIEW_DESTDIR:-}"

has_url=0
for a in "$@"; do
    [ "$a" != --base-url ] || has_url=1
done

# Inside an unpacked bundle (binaries next to this script): upgrade from it.
if [ "$has_url" -eq 0 ] && { [ -f "$HERE/airview-linux-arm64" ] || [ -f "$HERE/airview-linux-armv7" ]; }; then
    exec sh "$HERE/install.sh" --upgrade "$@"
fi

if [ "$has_url" -eq 0 ]; then
    BASE_URL=""
    if [ -f "$R/etc/airview/update.env" ]; then
        BASE_URL="$(sed -n 's/^BASE_URL=//p' "$R/etc/airview/update.env" | tail -n 1)"
    fi
    if [ -z "$BASE_URL" ]; then
        echo "ERROR: no release site known: pass --base-url https://<release-site>" >&2
        echo "       (or run update.sh from an unpacked airview-pi-<version>.tar.gz)" >&2
        exit 2
    fi
    set -- --base-url "$BASE_URL" "$@"
fi
exec sh "$HERE/install.sh" --upgrade "$@"
