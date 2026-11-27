#!/bin/sh
# Restores an AirView server backup made by backup.sh.
#
#   sudo airview restore /var/backups/airview/airview-backup-<host>-<time>.tar.gz
#
# The service is stopped; the current data directory and /etc/airview are
# moved aside (/var/lib/airview.pre-restore-<time>, /etc/airview.pre-restore-<time>)
# - nothing is deleted - and the backup takes their place. Then the service
# is started and checked. Delete the .pre-restore-* copies once satisfied.
#
# A backup can be restored on another machine (move a server): install
# AirView there with install.sh first, then restore. Feeders reconnect to the
# new address once DNS points there; the AVF/1 TLS key travels with the data,
# so pinned feeders keep working.
set -eu

usage() {
    cat <<'EOF'
Usage: restore.sh FILE [options]
  --yes            do not ask for confirmation
  --keep-config    keep the current /etc/airview (restore only the data directory)
  --no-start       leave the service stopped afterwards
  -h, --help       this help
EOF
}

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"

FILE=""
YES=0
KEEPCFG=0
NOSTART=0
while [ $# -gt 0 ]; do
    case "$1" in
        --yes | -y) YES=1; shift ;;
        --keep-config) KEEPCFG=1; shift ;;
        --no-start) NOSTART=1; shift ;;
        -h | --help) usage; exit 0 ;;
        -*) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
        *)
            [ -z "$FILE" ] || { echo "only one backup file, please" >&2; exit 2; }
            FILE="$1"
            shift
            ;;
    esac
done
[ -n "$FILE" ] || { usage >&2; exit 2; }
[ -f "$FILE" ] || die "no such file: $FILE"

need_root
[ -f "$AV_BIN" ] || die "AirView is not installed here; run install.sh first, then restore"

info "Checking $FILE"
LIST="$(tar -tzf - <"$FILE")" || die "$FILE is not a readable .tar.gz archive"
bad="$(printf '%s\n' "$LIST" | awk '
    $0 == "" { next }
    $0 !~ /^airview-backup\// || $0 ~ /(^|\/)\.\.(\/|$)/ { print; exit }')"
[ -z "$bad" ] || die "unexpected member in the archive: $bad (not an AirView backup?)"
printf '%s\n' "$LIST" | grep -qx 'airview-backup/MANIFEST' || die "the archive has no MANIFEST (not an AirView backup?)"
printf '%s\n' "$LIST" | grep -q '^airview-backup/data/' || die "the archive contains no data directory"
HAS_ETC=0
printf '%s\n' "$LIST" | grep -q '^airview-backup/etc/' && HAS_ETC=1

MANIFEST="$(tar -xzOf - airview-backup/MANIFEST <"$FILE")"
printf '%s\n' "$MANIFEST" | sed 's/^/    /'
bver="$(printf '%s\n' "$MANIFEST" | sed -n 's/^version=//p')"
iver="$(binary_version "$AV_BIN" || true)"
if [ -n "$bver" ] && [ "$bver" != "unknown" ] && [ -n "$iver" ] && [ "$bver" != "$iver" ]; then
    warn "the backup was made by AirView $bver; installed is $iver."
    warn "A backup from a NEWER version may not open with an older binary (upgrade first)."
fi

if [ "$YES" -eq 0 ]; then
    if [ -t 0 ]; then
        printf 'Replace the current data%s with this backup? [y/N] ' "$([ "$KEEPCFG" -eq 1 ] || [ "$HAS_ETC" -eq 0 ] || echo ' and configuration')"
        read -r answer
        case "$answer" in y | Y | yes | YES) ;; *) echo "Aborted; nothing changed."; exit 1 ;; esac
    else
        die "not a terminal: pass --yes to confirm"
    fi
fi

ts="$(timestamp)"
parent="$(dirname "$AV_DATA")"
stage="$parent/airview.restore-$ts"
mkdir -p "$stage"
chmod 0700 "$stage"
trap 'rm -rf "$stage"' EXIT

info "Unpacking"
tar -xzf - -C "$stage" --no-same-owner <"$FILE"
[ -d "$stage/airview-backup/data" ] || die "unpacking failed"

svc_stop

old_data="$AV_DATA.pre-restore-$ts"
mv "$AV_DATA" "$old_data"
mv "$stage/airview-backup/data" "$AV_DATA"
note "previous data moved to $old_data"
if [ "$HAS_ETC" -eq 1 ] && [ "$KEEPCFG" -eq 0 ]; then
    old_etc="$AV_ETC.pre-restore-$ts"
    if [ -d "$AV_ETC" ]; then
        mv "$AV_ETC" "$old_etc"
        note "previous configuration moved to $old_etc"
    fi
    mv "$stage/airview-backup/etc" "$AV_ETC"
    # Unpacked as root:root; the service reads its files through the group.
    chown -R "root:$AV_GROUP" "$AV_ETC"
fi

chown -R "$AV_USER:$AV_GROUP" "$AV_DATA"
fix_permissions
# The reference file follows the installed binary, not the backup.
"$AV_BIN" config example -o "$AV_ETC/airview.example.jsonc.new" 2>/dev/null &&
    mv -f "$AV_ETC/airview.example.jsonc.new" "$AV_ETC/airview.example.jsonc" &&
    chmod 0644 "$AV_ETC/airview.example.jsonc" || rm -f "$AV_ETC/airview.example.jsonc.new"

info "Checking the restored configuration"
config_check || warn "the restored configuration has errors (see above)"

if [ "$NOSTART" -eq 1 ]; then
    info "Restored. Start with: sudo systemctl start $AV_SERVICE"
    exit 0
fi
since="$(now_epoch)"
systemctl start "$AV_SERVICE"
if wait_healthy "$since" 120; then
    info "Restored and running at $HEALTHY_URL"
    note "When everything looks right: sudo rm -rf $old_data${old_etc:+ $old_etc}"
    exit 0
fi
warn "AirView did not become healthy after the restore (journalctl -u airview -n 100 --no-pager)."
warn "To go back: sudo systemctl stop airview && sudo mv $AV_DATA $AV_DATA.failed-$ts && sudo mv $old_data $AV_DATA${old_etc:+ (and $old_etc -> $AV_ETC)}"
exit 1
