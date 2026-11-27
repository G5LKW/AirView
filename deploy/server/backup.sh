#!/bin/sh
# Backs up an AirView server installed with install.sh: the data directory
# (/var/lib/airview: airview.db, stations.db with stations/accounts/keys,
# config.json, AVF/1 TLS key, uplink spool, ...) and the operator
# configuration (/etc/airview). Re-downloadable caches (map tiles, terrain)
# and logs are left out unless asked for.
#
#   sudo airview backup                       (= sudo /opt/airview/backup.sh)
#   sudo /opt/airview/backup.sh --online -o /mnt/backup --keep 30
#
# Default is a cold backup: the service is stopped for the duration of the
# copy and started again afterwards (consistent by construction). --online
# keeps it running and copies each database with SQLite's online backup
# (needs the sqlite3 command-line tool: apt install sqlite3).
#
# Result: <dir>/airview-backup-<host>-<UTC time>[-<tag>].tar.gz (mode 0600; it
# contains password hashes, TOTP secrets and private keys - store it safely).
# The archive path is printed as the last line. Restore with restore.sh.
set -eu

usage() {
    cat <<'EOF'
Usage: backup.sh [options]
  -o, --output DIR      where to write the archive (default /var/backups/airview)
  --online              do not stop the service (needs sqlite3)
  --tag TAG             add TAG to the file name ([A-Za-z0-9._-])
  --keep N              keep only the newest N backups in DIR (default 10, 0 = all)
  --include-logs        include airview.log*
  --include-cache       include map-tile and terrain caches
  --no-service-control  do not stop/start the service (caller has stopped it)
  -q, --quiet           print only the archive path
  -h, --help            this help
EOF
}

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"

OUT=""
ONLINE=0
TAG=""
KEEP=10
LOGS=0
CACHE=0
NOSVC=0
QUIET=0
while [ $# -gt 0 ]; do
    case "$1" in
        -o | --output) OUT="${2:?$1 needs a directory}"; shift 2 ;;
        --online) ONLINE=1; shift ;;
        --tag) TAG="${2:?--tag needs a value}"; shift 2 ;;
        --keep) KEEP="${2:?--keep needs a number}"; shift 2 ;;
        --include-logs) LOGS=1; shift ;;
        --include-cache) CACHE=1; shift ;;
        --no-service-control) NOSVC=1; shift ;;
        -q | --quiet) QUIET=1; shift ;;
        -h | --help) usage; exit 0 ;;
        *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
    esac
done
# The archive holds secrets: never readable by others, not even while written.
umask 077
case "$KEEP" in *[!0-9]* | "") die "--keep must be a number" ;; esac
case "$TAG" in *[!A-Za-z0-9._-]*) die "--tag may contain only A-Z a-z 0-9 . _ -" ;; esac

say() { [ "$QUIET" -eq 1 ] || info "$@"; }

need_root
[ -d "$AV_DATA" ] || die "no data directory $AV_DATA (is AirView installed with install.sh?)"
command -v tar >/dev/null 2>&1 || die "tar is required"
OUT="${OUT:-$AV_BACKUPS}"
mkdir -p "$OUT"
[ "$OUT" != "$AV_BACKUPS" ] || chmod 0700 "$OUT"

host="$(hostname 2>/dev/null | head -n 1 | tr -c 'A-Za-z0-9.-' '_' | sed 's/_*$//' || true)"
name="airview-backup-${host:-host}-$(timestamp)${TAG:+-$TAG}.tar.gz"
archive="$OUT/$name"
partial="$archive.partial"

# Disk space: the compressed archive is normally much smaller than the data,
# but warn when even the raw size does not fit.
need_kb="$(du -sk "$AV_DATA" 2>/dev/null | awk '{print $1}')"
free_kb="$(df -Pk "$OUT" 2>/dev/null | awk 'NR==2{print $4}')"
if [ -n "$need_kb" ] && [ -n "$free_kb" ] && [ "$free_kb" -lt "$need_kb" ]; then
    warn "only $((free_kb / 1024)) MiB free in $OUT for $((need_kb / 1024)) MiB of data; the backup may not fit"
fi

# Staging (database snapshots) next to the output, not in /tmp: /tmp is a
# RAM-backed tmpfs on current Debian and other systemd distributions, too
# small for a copy of a busy aggregator's databases. AV_BACKUP_TMP
# overrides the place.
TMP="$(mktemp -d "${AV_BACKUP_TMP:-$OUT}/.airview-backup.XXXXXX")"
RESTART=0
cleanup() {
    rc=$?
    rm -rf "$TMP"
    rm -f "$partial"
    if [ "$RESTART" -eq 1 ]; then
        systemctl start "$AV_SERVICE" || warn "could not start $AV_SERVICE again: systemctl start $AV_SERVICE"
    fi
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

version="$(binary_version "$AV_BIN" || true)"
mkdir -p "$TMP/airview-backup"
{
    echo "format=1"
    echo "created=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "host=$host"
    echo "version=${version:-unknown}"
    echo "arch=$(uname -m)"
    if [ "$ONLINE" -eq 1 ]; then echo "method=online"; else echo "method=cold"; fi
    echo "tag=$TAG"
} >"$TMP/airview-backup/MANIFEST"

# Member names below the archive root ("/" or $AIRVIEW_DESTDIR).
d="var/lib/airview"
e="etc/airview"
set -- --anchored
if [ "$CACHE" -eq 0 ]; then
    for x in tilecache tiles terrain terrarium webview2 acdb-staging.tmp; do
        set -- "$@" "--exclude=$d/$x"
    done
fi
[ "$LOGS" -eq 1 ] || set -- "$@" "--exclude=$d/airview.log*"
# Never: SQLite sidecar files of an online copy are replaced by snapshots.
SNAP_FILES=""
if [ "$ONLINE" -eq 1 ]; then
    # A built-in "airview db snapshot" (consistent copies via VACUUM INTO, see
    # docs/aggregator/HOOKS.md) is preferred; else the sqlite3 tool.
    USE_BUILTIN=0
    if "$AV_BIN" db snapshot -h >/dev/null 2>&1; then
        USE_BUILTIN=1
    elif ! command -v sqlite3 >/dev/null 2>&1; then
        die "--online needs the sqlite3 tool (apt install sqlite3), or run without --online"
    fi
    for x in "*.db" "*.db-wal" "*.db-shm" "*.db-journal"; do
        set -- "$@" "--exclude=$d/$x"
    done
    snapdir="$TMP/airview-backup/data"
    mkdir -p "$snapdir"
    [ -n "$R" ] || chown "$AV_USER:$AV_GROUP" "$TMP" "$TMP/airview-backup" "$snapdir"
    if [ "$USE_BUILTIN" -eq 1 ]; then
        say "Copying the databases (online, airview db snapshot)"
        as_airview env AIRVIEW_CONFIG="$AV_REAL_CONFIG_PATH" AIRVIEW_DATA="$AV_REAL_DATA_PATH" \
            "$AV_BIN" db snapshot -data "$AV_REAL_DATA_PATH" -o "$snapdir" ||
            die "online copy of the databases failed"
    else
        for db in "$AV_DATA"/*.db; do
            [ -f "$db" ] || continue
            b="$(basename "$db")"
            say "Copying $b (online)"
            as_airview sqlite3 "$db" ".timeout 30000" ".backup '$snapdir/$b'" ||
                die "online copy of $b failed"
        done
    fi
    for s in "$snapdir"/*.db; do
        [ -f "$s" ] || continue
        SNAP_FILES="$SNAP_FILES airview-backup/data/$(basename "$s")"
    done
    # Every live database must have its snapshot.
    for db in "$AV_DATA"/*.db; do
        [ -f "$db" ] || continue
        [ -f "$snapdir/$(basename "$db")" ] || die "no online copy of $(basename "$db")"
    done
else
    if [ "$NOSVC" -eq 0 ] && svc_active; then
        say "Stopping $AV_SERVICE for a consistent copy (use --online to avoid this)"
        systemctl stop "$AV_SERVICE"
        RESTART=1
    fi
fi
set -- "$@" \
    "--transform=s,^$d,airview-backup/data," \
    "--transform=s,^$e,airview-backup/etc,"

root="${R:-/}"
members="$d"
[ -d "$root/$e" ] && members="$members $e"

say "Writing $archive"
# shellcheck disable=SC2086 # members and SNAP_FILES are simple relative paths
# Written through stdout: GNU tar would read a name with a colon as host:file.
tar -czf - "$@" \
    -C "$TMP" airview-backup/MANIFEST \
    -C "$root" $members \
    ${SNAP_FILES:+-C "$TMP"} $SNAP_FILES >"$partial"
chmod 0600 "$partial"
mv -f "$partial" "$archive"

# The service can run again while old backups are pruned.
if [ "$RESTART" -eq 1 ]; then
    RESTART=0
    systemctl start "$AV_SERVICE" || warn "could not start $AV_SERVICE again: systemctl start $AV_SERVICE"
fi

if [ "$KEEP" -gt 0 ]; then
    # Newest first; everything after the first KEEP goes.
    # shellcheck disable=SC2012 # names are ours, without newlines
    ls -1t "$OUT"/airview-backup-*.tar.gz 2>/dev/null | awk -v k="$KEEP" 'NR > k' |
        while IFS= read -r old; do
            rm -f "$old" && { [ "$QUIET" -eq 1 ] || note "removed old backup $(basename "$old")"; }
        done
fi

size="$(du -h "$archive" 2>/dev/null | awk '{print $1}')"
[ "$QUIET" -eq 1 ] || info "Backup complete (${size:-?}). Restore with: sudo airview restore $archive"
echo "$archive"
