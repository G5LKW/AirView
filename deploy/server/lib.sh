# shellcheck shell=sh
# Shared helpers of the AirView server scripts (install.sh, upgrade.sh,
# uninstall.sh, backup.sh, restore.sh). Sourced, never run on its own.
#
# Layout (DESIGN §20.1):
#   /opt/airview/airview            the binary (+ airview.prev after an upgrade)
#   /opt/airview/*.sh               these scripts, lib.sh, docs and proxy examples
#   /etc/airview/airview.jsonc      operator configuration (root:airview 0640)
#   /etc/airview/airview.example.jsonc  every setting with default and range
#   /var/lib/airview                data directory (airview:airview 0750)
#   /var/backups/airview            backups (root 0700)
#   /etc/systemd/system/airview.service, /etc/logrotate.d/airview,
#   /usr/local/bin/airview          wrapper that sets AIRVIEW_DATA/AIRVIEW_CONFIG
#
# Test hooks: AIRVIEW_DESTDIR=<dir> installs below <dir> instead of / and
# skips the root check (the test suite puts stub systemctl, useradd, chown,
# runuser ... first in PATH). AIRVIEW_ARCH overrides the CPU detection.
# Neither is meant for production use.

R="${AIRVIEW_DESTDIR:-}"
AV_PREFIX="$R/opt/airview"
AV_BIN="$AV_PREFIX/airview"
AV_ETC="$R/etc/airview"
AV_CONFIG="$AV_ETC/airview.jsonc"
AV_DATA="$R/var/lib/airview"
AV_BACKUPS="$R/var/backups/airview"
AV_UNIT="$R/etc/systemd/system/airview.service"
AV_LOGROTATE="$R/etc/logrotate.d/airview"
AV_WRAPPER="$R/usr/local/bin/airview"
AV_SERVICE=airview
AV_USER=airview
AV_GROUP=airview

# Paths as the running service sees them (no DESTDIR): used in messages and
# in the environment of commands that the binary itself evaluates.
AV_REAL_CONFIG=/etc/airview/airview.jsonc
AV_REAL_DATA=/var/lib/airview

info() { printf '==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
warn() { printf '!!  %s\n' "$*" >&2; }
die() {
    printf 'ERROR: %s\n' "$*" >&2
    exit 1
}

# need_root exits unless running as root (skipped in DESTDIR test mode).
need_root() {
    [ -n "$R" ] && return 0
    [ "$(id -u)" -eq 0 ] || die "please run as root, e.g. sudo $0 $*"
}

# need_systemd exits unless systemd is the init system.
need_systemd() {
    [ -n "$R" ] && return 0
    command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ] ||
        die "systemd is required (for Docker see the Dockerfile and docker-compose.example.yml)"
}

# detect_arch prints the AirView architecture name of this machine.
detect_arch() {
    m="${AIRVIEW_ARCH:-$(uname -m)}"
    case "$m" in
        x86_64 | amd64) echo amd64 ;;
        aarch64 | arm64) echo arm64 ;;
        armv7l | armv8l | armhf | armv7)
            echo "AirView server bundles exist for amd64 and arm64. On a 32-bit Raspberry Pi OS use the Pi bundle (airview-pi.tar.gz)." >&2
            return 1
            ;;
        *)
            echo "unsupported CPU architecture: $m (supported: x86_64, aarch64)" >&2
            return 1
            ;;
    esac
}

# binary_version prints the version of the AirView binary $1 ("" if it
# does not run here, e.g. the wrong architecture).
binary_version() {
    "$1" -version 2>/dev/null | awk 'NR==1{print $2}'
}

# as_airview runs a command as the service account, so that any file it
# creates in the data directory belongs to that account (and the operator
# file's 0640 permissions are exercised exactly as the service sees them).
as_airview() {
    if [ -n "$R" ] || [ "$(id -u)" -ne 0 ]; then
        "$@"
    elif command -v runuser >/dev/null 2>&1; then
        runuser -u "$AV_USER" -- "$@"
    elif command -v setpriv >/dev/null 2>&1; then
        setpriv --reuid="$AV_USER" --regid="$AV_GROUP" --init-groups -- "$@"
    else
        # su needs a shell for -c; quote every argument for it.
        cmd=""
        for a in "$@"; do
            cmd="$cmd '$(printf '%s' "$a" | sed "s/'/'\\\\''/g")'"
        done
        su -s /bin/sh "$AV_USER" -c "$cmd"
    fi
}

# Paths handed to the binary in AIRVIEW_CONFIG / AIRVIEW_DATA by these
# scripts. In DESTDIR test mode the binary must see the DESTDIR paths.
if [ -n "$R" ]; then
    AV_REAL_CONFIG_PATH="$AV_CONFIG"
    AV_REAL_DATA_PATH="$AV_DATA"
else
    AV_REAL_CONFIG_PATH="$AV_REAL_CONFIG"
    AV_REAL_DATA_PATH="$AV_REAL_DATA"
fi

# config_check validates the configuration with binary $1 (default: the
# installed one). Exit status as "airview config check" (0 = valid).
config_check() {
    bin="${1:-$AV_BIN}"
    as_airview env AIRVIEW_CONFIG="$AV_REAL_CONFIG_PATH" AIRVIEW_DATA="$AV_REAL_DATA_PATH" \
        "$bin" config check -q
}

svc_active() { systemctl is-active --quiet "$AV_SERVICE" 2>/dev/null; }
svc_exists() { [ -f "$AV_UNIT" ]; }

svc_stop() {
    if svc_active; then
        info "Stopping $AV_SERVICE"
        systemctl stop "$AV_SERVICE"
    fi
}

# fix_permissions (re)applies ownership and modes of the installed tree.
fix_permissions() {
    mkdir -p "$AV_DATA" "$AV_ETC" "$AV_BACKUPS"
    chown "$AV_USER:$AV_GROUP" "$AV_DATA"
    chmod 0750 "$AV_DATA"
    chown "root:$AV_GROUP" "$AV_ETC"
    chmod 0750 "$AV_ETC"
    if [ -f "$AV_CONFIG" ]; then
        chown "root:$AV_GROUP" "$AV_CONFIG"
        chmod 0640 "$AV_CONFIG"
    fi
    chmod 0700 "$AV_BACKUPS"
}

# http_url prints the base URL ("http://host:port/") where the service
# answers: the address it logged after this start (journal), else the one
# derived from the effective configuration.
http_url() {
    since="${1:-}"
    if [ -n "$since" ] && command -v journalctl >/dev/null 2>&1; then
        u="$(journalctl -u "$AV_SERVICE" --since "@$since" -o cat 2>/dev/null |
            sed -n 's/.*is running: \(https\{0,1\}:\/\/[^ ]*\).*/\1/p' | tail -n 1)"
        if [ -n "$u" ]; then
            case "$u" in */) ;; *) u="$u/" ;; esac
            echo "$u"
            return 0
        fi
    fi
    cfg="$(as_airview env AIRVIEW_CONFIG="$AV_REAL_CONFIG_PATH" AIRVIEW_DATA="$AV_REAL_DATA_PATH" \
        "$AV_BIN" config print 2>/dev/null || true)"
    port="$(printf '%s\n' "$cfg" | awk '
        /^  "http": \{/ { h = 1; next }
        h && /^  \}/ { h = 0 }
        h && /^    "port":/ { gsub(/[^0-9]/, ""); print; exit }')"
    bind="$(printf '%s\n' "$cfg" | awk '
        /^  "http": \{/ { h = 1; next }
        h && /^  \}/ { h = 0 }
        h && /^    "bindAddr":/ { sub(/^[^:]*: *"/, ""); sub(/".*$/, ""); print; exit }')"
    [ -n "$port" ] || port=80
    case "$bind" in
        "" | 0.0.0.0 | "::" | "[::]") host=127.0.0.1 ;;
        *:*) host="[$bind]" ;;
        *) host="$bind" ;;
    esac
    echo "http://$host:$port/"
}

# http_ok succeeds when GET $1 answers 2xx. Without curl or wget it
# succeeds (the caller then relies on the service state alone).
http_ok() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsS -o /dev/null --max-time 5 "$1" 2>/dev/null
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O /dev/null -T 5 "$1" 2>/dev/null
    else
        return 0
    fi
}

# wait_healthy START_EPOCH [TIMEOUT_S]: waits until the service is active
# and /healthz answers. Prints the URL on success.
wait_healthy() {
    since="$1"
    limit="${2:-90}"
    waited=0
    stable=0
    while [ "$waited" -lt "$limit" ]; do
        if svc_active; then
            stable=$((stable + 2))
            url="$(http_url "$since")"
            if [ "$stable" -ge 4 ] && http_ok "${url}healthz"; then
                HEALTHY_URL="$url"
                return 0
            fi
        else
            stable=0
            if systemctl is-failed --quiet "$AV_SERVICE" 2>/dev/null && [ "$waited" -ge 10 ]; then
                return 1
            fi
        fi
        sleep 2
        waited=$((waited + 2))
    done
    return 1
}

# now_epoch prints the current UNIX time.
now_epoch() { date +%s; }

# timestamp prints a UTC timestamp usable in file names.
timestamp() { date -u +%Y%m%dT%H%M%SZ; }
