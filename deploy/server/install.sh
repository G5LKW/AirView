#!/bin/sh
# AirView server installer for Linux hosts with systemd (amd64, arm64).
#
# From an unpacked bundle:
#   tar xzf airview-server-linux-amd64.tar.gz
#   cd airview-server-linux-amd64 && sudo ./install.sh [options]
#
# One line, downloading the bundle for this CPU and checking it against the
# SHA256SUMS file published next to it:
#   curl -fsSL https://<release-site>/airview-server-install.sh | sudo sh -s -- --base-url https://<release-site>
#
# Running it again on an installed system upgrades (same as upgrade.sh):
# the binary, unit and scripts are replaced; configuration and data are kept.
# See README.md (installed as /opt/airview/README.md) for everything else.
set -eu
#
# Everything below runs inside main(), called on the last line: if a piped
# download (curl | sh) is cut short, the shell sees an incomplete function
# and runs nothing, instead of a prefix of the commands as root.

usage() {
    cat <<'EOF'
Usage: install.sh [options]

First installation (ignored on upgrades; edit /etc/airview/airview.jsonc instead):
  --mode MODE          aggregator (default) or standalone
  --name NAME          network display name (aggregator.name)
  --public-host HOST   host name feeders use to reach this server (aggregator.publicHost)
  --proxy              a reverse proxy on this host terminates HTTPS (see proxy/):
                       AirView listens on 127.0.0.1:8080 and trusts its X-Forwarded-For
  --http-port N        HTTP port (default 80, or 8080 with --proxy)

Download:
  --base-url URL       fetch airview-server-linux-<arch>.tar.gz and SHA256SUMS from URL,
                       verify the checksum and install from it

Upgrade (also: upgrade.sh):
  --upgrade            require an existing installation
  --no-backup          skip the backup taken before an upgrade
  --no-rollback        keep the new binary even if it does not become healthy
  --force              upgrade even if the new version rejects the configuration

General:
  --no-start           install but do not (re)start the service
  -h, --help           this help
EOF
}

main() {
    MODE=aggregator
    NAME=""
    PUBLIC_HOST=""
    PROXY=0
    HTTP_PORT=""
    BASE_URL=""
    UPGRADE=0
    NO_BACKUP=0
    NO_ROLLBACK=0
    FORCE=0
    NO_START=0
    FIRST_ONLY_SET=""
    ORIG_ARGS=""

    while [ $# -gt 0 ]; do
        case "$1" in
            --mode) MODE="${2:?--mode needs a value}"; FIRST_ONLY_SET="$FIRST_ONLY_SET --mode"; shift 2 ;;
            --name) NAME="${2:?--name needs a value}"; FIRST_ONLY_SET="$FIRST_ONLY_SET --name"; shift 2 ;;
            --public-host) PUBLIC_HOST="${2:?--public-host needs a value}"; FIRST_ONLY_SET="$FIRST_ONLY_SET --public-host"; shift 2 ;;
            --proxy) PROXY=1; FIRST_ONLY_SET="$FIRST_ONLY_SET --proxy"; shift ;;
            --http-port) HTTP_PORT="${2:?--http-port needs a value}"; FIRST_ONLY_SET="$FIRST_ONLY_SET --http-port"; shift 2 ;;
            --base-url) BASE_URL="${2:?--base-url needs a value}"; shift 2; continue ;;
            --upgrade) UPGRADE=1; shift ;;
            --no-backup) NO_BACKUP=1; shift ;;
            --no-rollback) NO_ROLLBACK=1; shift ;;
            --force) FORCE=1; shift ;;
            --no-start) NO_START=1; shift ;;
            -h | --help) usage; exit 0 ;;
            *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
        esac
    done
    # Arguments to hand to the install.sh of a downloaded bundle (without --base-url).
    [ "$MODE" = aggregator ] || ORIG_ARGS="$ORIG_ARGS --mode $MODE"
    [ "$PROXY" -eq 0 ] || ORIG_ARGS="$ORIG_ARGS --proxy"
    [ -z "$HTTP_PORT" ] || ORIG_ARGS="$ORIG_ARGS --http-port $HTTP_PORT"
    [ "$UPGRADE" -eq 0 ] || ORIG_ARGS="$ORIG_ARGS --upgrade"
    [ "$NO_BACKUP" -eq 0 ] || ORIG_ARGS="$ORIG_ARGS --no-backup"
    [ "$NO_ROLLBACK" -eq 0 ] || ORIG_ARGS="$ORIG_ARGS --no-rollback"
    [ "$FORCE" -eq 0 ] || ORIG_ARGS="$ORIG_ARGS --force"
    [ "$NO_START" -eq 0 ] || ORIG_ARGS="$ORIG_ARGS --no-start"

    # ---------------------------------------------------------------------------
    # Download mode: runs without the rest of the bundle (piped from curl).
    # ---------------------------------------------------------------------------
    if [ -n "$BASE_URL" ]; then
        if [ -z "${AIRVIEW_DESTDIR:-}" ] && [ "$(id -u)" -ne 0 ]; then
            echo "ERROR: please run as root (curl ... | sudo sh -s -- --base-url URL)" >&2
            exit 1
        fi
        case "${AIRVIEW_ARCH:-$(uname -m)}" in
            x86_64 | amd64) ARCH=amd64 ;;
            aarch64 | arm64) ARCH=arm64 ;;
            *) echo "ERROR: unsupported CPU architecture $(uname -m) (supported: x86_64, aarch64)" >&2; exit 1 ;;
        esac
        case "$BASE_URL" in
            https://*) ;;
            http://*) echo "!!  $BASE_URL is not HTTPS; the download is only as trustworthy as the SHA256SUMS fetched with it" >&2 ;;
            *) echo "ERROR: --base-url must be an http(s) URL" >&2; exit 2 ;;
        esac
        BASE_URL="${BASE_URL%/}"
        TARBALL="airview-server-linux-$ARCH.tar.gz"
        TMP="$(mktemp -d)"
        trap 'rm -rf "$TMP"' EXIT INT TERM
        fetch() {
            if command -v curl >/dev/null 2>&1; then
                curl -fsSL --proto '=https,http' --retry 3 -o "$2" "$1"
            elif command -v wget >/dev/null 2>&1; then
                wget -q -O "$2" "$1"
            else
                echo "ERROR: curl or wget is required to download" >&2
                exit 1
            fi
        }
        echo "==> Downloading $TARBALL"
        fetch "$BASE_URL/$TARBALL" "$TMP/$TARBALL"
        fetch "$BASE_URL/SHA256SUMS" "$TMP/SHA256SUMS"
        want="$(awk -v f="$TARBALL" '{n=$2; sub(/^\*/, "", n)} n==f {print $1}' "$TMP/SHA256SUMS")"
        [ -n "$want" ] || { echo "ERROR: $TARBALL is not listed in SHA256SUMS" >&2; exit 1; }
        got="$(sha256sum "$TMP/$TARBALL" | awk '{print $1}')"
        [ "$want" = "$got" ] || { echo "ERROR: checksum mismatch for $TARBALL (want $want, got $got)" >&2; exit 1; }
        echo "==> Checksum OK"
        tar -xzf - -C "$TMP" --no-same-owner <"$TMP/$TARBALL"
        [ -x "$TMP/airview-server-linux-$ARCH/install.sh" ] || { echo "ERROR: the bundle has no install.sh" >&2; exit 1; }
        # Name and public host may contain spaces: pass them as separate arguments.
        set -- "$TMP/airview-server-linux-$ARCH/install.sh"
        [ -z "$NAME" ] || set -- "$@" --name "$NAME"
        [ -z "$PUBLIC_HOST" ] || set -- "$@" --public-host "$PUBLIC_HOST"
        # shellcheck disable=SC2086 # ORIG_ARGS holds simple flags only
        sh "$@" $ORIG_ARGS
        exit $?
    fi

    HERE="$(cd "$(dirname "$0")" && pwd)"
    [ -f "$HERE/lib.sh" ] || {
        echo "ERROR: lib.sh is missing next to install.sh. Run install.sh from an unpacked bundle, or use --base-url." >&2
        exit 1
    }
    # shellcheck source=lib.sh
    . "$HERE/lib.sh"

    need_root
    need_systemd
    ARCH="$(detect_arch)" || exit 1

    if [ -d "$AV_PREFIX" ] && [ "$HERE" = "$(cd "$AV_PREFIX" && pwd)" ]; then
        die "this is the installed copy in $AV_PREFIX; run upgrade.sh from a newly unpacked bundle, or pass --base-url URL"
    fi

    NEW_BIN="$HERE/airview"
    [ -f "$NEW_BIN" ] || die "the binary 'airview' is missing next to install.sh"
    NEW_VERSION="$(binary_version "$NEW_BIN")"
    [ -n "$NEW_VERSION" ] || die "$NEW_BIN does not run on this machine (wrong architecture? this host is $ARCH)"

    EXISTING=0
    OLD_VERSION=""
    if [ -f "$AV_BIN" ]; then
        EXISTING=1
        OLD_VERSION="$(binary_version "$AV_BIN" || true)"
    fi
    if [ "$UPGRADE" -eq 1 ] && [ "$EXISTING" -eq 0 ]; then
        die "no installation found in $AV_PREFIX; run install.sh without --upgrade first"
    fi

    # --- validate first-install options ----------------------------------------
    case "$MODE" in aggregator | standalone) ;; *) die "--mode must be aggregator or standalone" ;; esac
    if [ -n "$NAME" ]; then
        case "$NAME" in *'"'* | *'\'*) die "--name must not contain quotes or backslashes" ;; esac
        [ "${#NAME}" -le 64 ] || die "--name is longer than 64 characters"
    fi
    if [ -n "$PUBLIC_HOST" ]; then
        case "$PUBLIC_HOST" in
            *[!A-Za-z0-9.:-]* | "") die "--public-host must be a host name or address (no scheme, path or port)" ;;
        esac
    fi
    if [ -n "$HTTP_PORT" ]; then
        case "$HTTP_PORT" in *[!0-9]* | "") die "--http-port must be a number" ;; esac
        [ "$HTTP_PORT" -ge 1 ] && [ "$HTTP_PORT" -le 65535 ] || die "--http-port must be 1..65535"
    fi
    if [ -z "$HTTP_PORT" ]; then
        if [ "$PROXY" -eq 1 ]; then HTTP_PORT=8080; else HTTP_PORT=80; fi
    fi

    # install_files copies the binary (as airview.new), scripts, docs, unit,
    # logrotate rule and wrapper. The binary is activated by the caller.
    install_files() {
        mkdir -p "$AV_PREFIX" "$AV_PREFIX/proxy" "$(dirname "$AV_UNIT")" "$(dirname "$AV_LOGROTATE")" "$(dirname "$AV_WRAPPER")"
        chmod 0755 "$AV_PREFIX"
        install -m 0755 "$NEW_BIN" "$AV_PREFIX/airview.new"
        for f in install.sh upgrade.sh uninstall.sh backup.sh restore.sh avf-cert-sync.sh; do
            [ -f "$HERE/$f" ] && install -m 0755 "$HERE/$f" "$AV_PREFIX/$f"
        done
        for f in lib.sh README.md FIREWALL.md; do
            [ -f "$HERE/$f" ] && install -m 0644 "$HERE/$f" "$AV_PREFIX/$f"
        done
        if [ -d "$HERE/proxy" ]; then
            for f in "$HERE"/proxy/*; do
                [ -f "$f" ] && install -m 0644 "$f" "$AV_PREFIX/proxy/$(basename "$f")"
            done
        fi
        install -m 0644 "$HERE/airview.service" "$AV_UNIT"
        if [ -d "$(dirname "$AV_LOGROTATE")" ]; then
            install -m 0644 "$HERE/airview.logrotate" "$AV_LOGROTATE"
        fi
        install -m 0755 "$HERE/airview-wrapper.sh" "$AV_WRAPPER"
    }

    # write_example refreshes the commented reference of every setting.
    write_example() {
        rm -f "$AV_ETC/airview.example.jsonc"
        if "$AV_PREFIX/airview.new" config example -o "$AV_ETC/airview.example.jsonc"; then
            chmod 0644 "$AV_ETC/airview.example.jsonc"
        else
            warn "could not write $AV_ETC/airview.example.jsonc"
        fi
    }

    # write_operator_file creates /etc/airview/airview.jsonc once.
    write_operator_file() {
        [ -f "$AV_CONFIG" ] && return 0
        {
            echo "// AirView operator configuration (JSON with comments), created by install.sh"
            echo "// on $(date -u +%Y-%m-%d). Settings listed here win over the web interface and"
            echo "// config.json (the web UI shows them as locked); AIRVIEW_* environment variables"
            echo "// win over this file. Every setting with its default and range:"
            echo "//   /etc/airview/airview.example.jsonc"
            echo "// Check after editing:  sudo airview config check   then: sudo systemctl restart airview"
            echo "{"
            echo "  \"mode\": \"$MODE\","
            if [ "$PROXY" -eq 1 ]; then
                echo "  // Behind the reverse proxy on this host (see /opt/airview/proxy/):"
                echo "  // only the proxy reaches the web interface."
                echo "  \"http\": { \"port\": $HTTP_PORT, \"bindAddr\": \"127.0.0.1\" },"
            else
                echo "  \"http\": { \"port\": $HTTP_PORT },"
            fi
            if [ "$MODE" = "aggregator" ]; then
                echo "  // Database profile for a server (large caches, short checkpoint interval);"
                echo "  // \"auto\" picks it in aggregator mode as well (docs/CONFIG.md, storage.profile)."
                echo "  \"storage\": { \"profile\": \"server\" },"
            fi
            echo "  \"aggregator\": {"
            if [ -n "$NAME" ]; then
                echo "    \"name\": \"$NAME\","
            fi
            if [ -n "$PUBLIC_HOST" ]; then
                echo "    \"publicHost\": \"$PUBLIC_HOST\","
            fi
            echo "    // Feeder listeners (0 = off): AVF/1 (AirView feeders), Beast compat, mlat-client."
            echo "    \"avfPort\": 30160,"
            echo "    \"beastPort\": 30004,"
            echo "    \"mlatPort\": 31090,"
            if [ "$PROXY" -eq 1 ]; then
                echo "    // X-Forwarded-For / -Proto are honoured only from these addresses."
                echo "    \"trustedProxies\": [\"127.0.0.1/32\", \"::1/128\"],"
            else
                echo "    // Add the proxy's address here when a reverse proxy is put in front."
                echo "    \"trustedProxies\": [],"
            fi
            echo "    // How new stations are admitted: \"invite\" (enrol token), \"approve\" or \"open\"."
            echo "    \"admission\": \"invite\""
            echo "  }"
            echo "}"
        } >"$AV_CONFIG.tmp"
        mv "$AV_CONFIG.tmp" "$AV_CONFIG"
        CREATED_CONFIG=1
    }

    ensure_user() {
        if ! id "$AV_USER" >/dev/null 2>&1; then
            info "Creating the system account $AV_USER"
            nologin="$(command -v nologin 2>/dev/null || echo /usr/sbin/nologin)"
            useradd --system --user-group --home-dir "$AV_REAL_DATA" --no-create-home \
                --shell "$nologin" --comment "AirView server" "$AV_USER"
        fi
    }

    print_summary() {
        url="${HEALTHY_URL:-}"
        ip="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
        echo ""
        info "AirView $NEW_VERSION is installed."
        [ -z "$url" ] || note "Answering at     : $url (on this host)"
        if [ "$PROXY" -eq 1 ] && [ "${CREATED_CONFIG:-0}" -eq 1 ]; then
            note "Web interface    : via your reverse proxy (examples: $AV_PREFIX/proxy/)"
        else
            note "Web interface    : http://${ip:-<server>}:$HTTP_PORT/"
            if [ "$MODE" = aggregator ]; then
                echo ""
                warn "PLAIN HTTP: this aggregator has no HTTPS. Passwords and admin sessions"
                warn "would cross the network in clear text, so AirView accepts logins only"
                warn "from this machine (sudo airview admin init) until an HTTPS reverse"
                warn "proxy is in front (install with --proxy; examples in $AV_PREFIX/proxy/)."
            fi
        fi
        note "Configuration    : $AV_REAL_CONFIG  (reference: /etc/airview/airview.example.jsonc)"
        note "Data             : $AV_REAL_DATA"
        note "Logs             : journalctl -u airview -f   (and $AV_REAL_DATA/airview.log)"
        note "Commands         : sudo airview config check | backup | restore FILE | upgrade | uninstall"
        note "Firewall         : $AV_PREFIX/FIREWALL.md"
        if [ "$MODE" = aggregator ] && [ "$EXISTING" -eq 0 ]; then
            echo ""
            info "Next: create the admin account (password + authenticator app):"
            note "sudo airview admin init"
            note "(or open the web interface over HTTPS and use the one-time setup token: sudo cat $AV_REAL_DATA/setup-token)"
        fi
    }

    # effective_mode prints the configured run mode of the installed binary.
    effective_mode() {
        m="$(as_airview env AIRVIEW_CONFIG="$AV_REAL_CONFIG_PATH" AIRVIEW_DATA="$AV_REAL_DATA_PATH" \
            "$AV_BIN" config print 2>/dev/null | sed -n 's/^  "mode": "\([a-z-]*\)".*/\1/p' | head -n 1)"
        echo "${m:-standalone}"
    }

    CREATED_CONFIG=0
    START_EPOCH="$(now_epoch)"

    if [ "$EXISTING" -eq 0 ]; then
        # ------------------------------------------------------------------ install
        info "Installing AirView $NEW_VERSION ($ARCH) as a $MODE server"
        ensure_user
        install_files
        fix_permissions
        write_example
        write_operator_file
        fix_permissions
        mv -f "$AV_PREFIX/airview.new" "$AV_BIN"
        info "Checking the configuration"
        config_check || die "the configuration is invalid (see above); fix $AV_REAL_CONFIG and run: sudo systemctl restart airview"
        [ "$CREATED_CONFIG" -eq 1 ] || MODE="$(effective_mode)"
        systemctl daemon-reload
        systemctl enable "$AV_SERVICE" >/dev/null 2>&1 || systemctl enable "$AV_SERVICE"
        if [ "$NO_START" -eq 1 ]; then
            info "Not starting (--no-start). Start with: sudo systemctl start airview"
            print_summary
            exit 0
        fi
        START_EPOCH="$(now_epoch)"
        systemctl restart "$AV_SERVICE"
        if wait_healthy "$START_EPOCH" 90; then
            print_summary
            exit 0
        fi
        warn "AirView did not become healthy. Check: journalctl -u airview -n 100 --no-pager"
        exit 1
    fi

    # ---------------------------------------------------------------------- upgrade
    [ -z "$FIRST_ONLY_SET" ] || warn "ignored on an upgrade:$FIRST_ONLY_SET (edit $AV_REAL_CONFIG instead)"
    MODE="$(effective_mode)"
    info "Upgrading AirView ${OLD_VERSION:-(unknown)} -> $NEW_VERSION ($ARCH)"
    ensure_user
    install -m 0755 "$NEW_BIN" "$AV_PREFIX/airview.new"
    fix_permissions

    info "Checking the configuration with the new version"
    if ! config_check "$AV_PREFIX/airview.new"; then
        if [ "$FORCE" -eq 1 ]; then
            warn "the new version reports configuration errors; continuing because of --force"
        else
            rm -f "$AV_PREFIX/airview.new"
            die "the new version rejects the configuration (see above). Fix $AV_REAL_CONFIG, or pass --force. Nothing was changed."
        fi
    fi

    svc_stop

    if [ "$NO_BACKUP" -eq 0 ]; then
        info "Backing up before the upgrade"
        "$HERE/backup.sh" --tag "pre-upgrade-${OLD_VERSION:-unknown}" --no-service-control --quiet ||
            die "the backup failed; nothing was changed (start again with: sudo systemctl start airview, or pass --no-backup)"
    fi

    cp -p "$AV_BIN" "$AV_PREFIX/airview.prev"
    install_files
    write_example
    fix_permissions
    mv -f "$AV_PREFIX/airview.new" "$AV_BIN"
    systemctl daemon-reload
    systemctl enable "$AV_SERVICE" >/dev/null 2>&1 || true

    if [ "$NO_START" -eq 1 ]; then
        info "Upgraded; not starting (--no-start). Previous binary: $AV_PREFIX/airview.prev"
        exit 0
    fi
    START_EPOCH="$(now_epoch)"
    systemctl start "$AV_SERVICE"
    if wait_healthy "$START_EPOCH" 90; then
        print_summary
        note "Previous binary  : $AV_PREFIX/airview.prev"
        exit 0
    fi

    warn "AirView $NEW_VERSION did not become healthy (journalctl -u airview -n 100 --no-pager)."
    if [ "$NO_ROLLBACK" -eq 1 ]; then
        warn "--no-rollback: leaving $NEW_VERSION in place. The previous binary is $AV_PREFIX/airview.prev"
        exit 1
    fi
    info "Rolling back to ${OLD_VERSION:-the previous binary}"
    systemctl stop "$AV_SERVICE" || true
    mv -f "$AV_PREFIX/airview.prev" "$AV_BIN"
    START_EPOCH="$(now_epoch)"
    systemctl start "$AV_SERVICE"
    if wait_healthy "$START_EPOCH" 90; then
        warn "rolled back: AirView ${OLD_VERSION:-} is running again. The upgrade was not applied."
    else
        warn "the previous binary does not start either. The new version may have changed the database;"
        warn "restore the pre-upgrade backup with: sudo $AV_PREFIX/restore.sh <file in $AV_BACKUPS>"
    fi
    exit 1
}

main "$@"
