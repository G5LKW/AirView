#!/bin/sh
# AirView station installer for Raspberry Pi OS (and other systemd Linux on arm64 / armv7).
#
# From the unpacked bundle:
#   tar xzf airview-pi-<version>.tar.gz && cd airview-pi-<version> && sudo ./install.sh
#
# One line, downloading the newest Pi bundle listed in SHA256SUMS on a release site and
# verifying it (the site is remembered for update.sh):
#   curl -fsSL https://<release-site>/airview-pi-install.sh | sudo sh -s -- --base-url https://<release-site>
#
# Idempotent: running it again (same or newer bundle) upgrades in place. Data in /var/lib/airview
# and the operator file /etc/airview/airview.jsonc are kept; the previous binary is kept as
# /opt/airview/airview.prev and restored automatically when the new one does not start.
#
# Options:
#   --base-url URL   download airview-pi-*.tar.gz + SHA256SUMS from URL, verify, install
#   --key KEY        release public key (base64 Ed25519, from the release notes): SHA256SUMS.sig
#                    must verify against it; the key is pinned in /etc/airview/update.env
#   --allow-unsigned accept a download without a verified signature (not recommended)
#   --upgrade        require an existing installation (update.sh passes this)
#   --force          reinstall even when the downloaded version is already installed
#   --no-start       install but do not (re)start the service
#   --low-wear-journal  keep the systemd journal in RAM only (Storage=volatile drop-in
#                    /etc/systemd/journald.conf.d/airview-low-wear.conf): fewer SD-card writes,
#                    but the journal of every service is lost at a reboot. Opt-in; see README.md
#   -h, --help       this help
#
# Test hooks: AIRVIEW_DESTDIR=<dir> installs below <dir> (no root needed, system commands are
# whatever PATH provides); AIRVIEW_ARCH overrides uname -m.
set -eu

usage() {
    sed -n '2,29p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' || true
}

die() {
    echo "ERROR: $*" >&2
    exit 1
}

# fetch URL FILE: curl or wget, HTTPS only unless the URL is plain http.
fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1"
    else
        die "neither curl nor wget is installed"
    fi
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        openssl dgst -sha256 "$1" | awk '{print $NF}'
    fi
}

# ed25519_verify KEY FILE SIGFILE: 0 = valid, 1 = invalid, 2 = no OpenSSL >= 3.0 (SEC-R6).
# KEY is the base64 raw Ed25519 public key; SIGFILE holds the base64 signature
# ("go run ./deploy/release sign -raw").
ed25519_verify() {
    command -v openssl >/dev/null 2>&1 || return 2
    case "$(openssl version 2>/dev/null)" in
        "OpenSSL 0."* | "OpenSSL 1."* | "") return 2 ;;
    esac
    vdir="$(mktemp -d "${TMPDIR:-/tmp}/airview-sig.XXXXXX")"
    vrc=0
    # SubjectPublicKeyInfo DER for Ed25519 = fixed 12-byte prefix + 32-byte key.
    printf '\060\052\060\005\006\003\053\145\160\003\041\000' >"$vdir/key.der"
    printf '%s' "$1" | base64 -d >>"$vdir/key.der" 2>/dev/null || vrc=1
    tr -d ' \r\n\t' <"$3" | base64 -d >"$vdir/sig.bin" 2>/dev/null || vrc=1
    if [ "$vrc" = 0 ] && [ "$(wc -c <"$vdir/key.der" | tr -d ' ')" = 44 ] && [ "$(wc -c <"$vdir/sig.bin" | tr -d ' ')" = 64 ]; then
        openssl pkeyutl -verify -pubin -keyform DER -inkey "$vdir/key.der" -rawin \
            -in "$2" -sigfile "$vdir/sig.bin" >/dev/null 2>&1 || vrc=1
    else
        vrc=1
    fi
    rm -rf "$vdir"
    return "$vrc"
}

# version_of_bundle NAME: airview-pi-1.2.3.tar.gz -> 1.2.3
version_of_bundle() {
    printf '%s\n' "$1" | sed 's/^airview-pi-//; s/\.tar\.gz$//'
}

main() {
    BASE_URL=""
    FROM_URL=""
    KEY=""
    FROM_KEY=""
    ALLOW_UNSIGNED=0
    UPGRADE=0
    FORCE=0
    NO_START=0
    LOW_WEAR_JOURNAL=0
    while [ $# -gt 0 ]; do
        case "$1" in
            --base-url) BASE_URL="${2:?--base-url needs a value}"; shift 2 ;;
            --key) KEY="${2:?--key needs a value}"; shift 2 ;;
            --allow-unsigned) ALLOW_UNSIGNED=1; shift ;;
            --from-url) FROM_URL="${2:?--from-url needs a value}"; shift 2 ;; # internal: set by download mode
            --from-key) FROM_KEY="${2:?--from-key needs a value}"; shift 2 ;; # internal: set by download mode
            --upgrade) UPGRADE=1; shift ;;
            --force) FORCE=1; shift ;;
            --no-start) NO_START=1; shift ;;
            --low-wear-journal) LOW_WEAR_JOURNAL=1; shift ;;
            -h | --help) usage; exit 0 ;;
            *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
        esac
    done

    R="${AIRVIEW_DESTDIR:-}"
    if [ -z "$R" ] && [ "$(id -u)" -ne 0 ]; then
        die "please run as root: sudo $0 $*"
    fi

    PREFIX=/opt/airview
    DATA=/var/lib/airview
    ETC=/etc/airview
    UNIT=/etc/systemd/system/airview.service
    INSTALLED_VERSION="$(cat "$R$PREFIX/VERSION" 2>/dev/null || true)"

    if [ "$UPGRADE" -eq 1 ] && [ ! -x "$R$PREFIX/airview" ]; then
        die "AirView is not installed in $PREFIX (run install.sh first)"
    fi

    case "${AIRVIEW_ARCH:-$(uname -m)}" in
        aarch64 | arm64) BIN="airview-linux-arm64" ;;
        armv7l | armv8l | armv7) BIN="airview-linux-armv7" ;;
        *)
            echo "Unsupported CPU architecture: ${AIRVIEW_ARCH:-$(uname -m)}" >&2
            echo "AirView ships Pi builds for aarch64 (64-bit) and armv7l (32-bit); for x86_64 use the server bundle." >&2
            exit 1
            ;;
    esac

    # -----------------------------------------------------------------------
    # Download mode: fetch, verify and run the install.sh of the bundle.
    # -----------------------------------------------------------------------
    if [ -n "$BASE_URL" ]; then
        # SEC-R6: the release key pinned by an earlier install (update.sh) or given now.
        if [ -z "$KEY" ] && [ -f "$R$ETC/update.env" ]; then
            KEY="$(sed -n 's/^RELEASE_KEY=//p' "$R$ETC/update.env" | tail -n 1)"
        fi
        case "$BASE_URL" in
            https://*) ;;
            http://*)
                if [ -z "$KEY" ] && [ "$ALLOW_UNSIGNED" -eq 0 ]; then
                    die "refusing an unsigned download over plain HTTP ($BASE_URL); use https, --key <release key>, or --allow-unsigned"
                fi
                ;;
            *) echo "ERROR: --base-url must be an http(s) URL" >&2; exit 2 ;;
        esac
        BASE_URL="${BASE_URL%/}"
        TMP="$(mktemp -d "${TMPDIR:-/tmp}/airview-pi.XXXXXX")"
        trap 'rm -rf "$TMP"' EXIT INT TERM
        echo "==> Fetching $BASE_URL/SHA256SUMS"
        fetch "$BASE_URL/SHA256SUMS" "$TMP/SHA256SUMS"
        if [ -n "$KEY" ]; then
            fetch "$BASE_URL/SHA256SUMS.sig" "$TMP/SHA256SUMS.sig" || die "could not download $BASE_URL/SHA256SUMS.sig"
            vr=0
            ed25519_verify "$KEY" "$TMP/SHA256SUMS" "$TMP/SHA256SUMS.sig" || vr=$?
            case "$vr" in
                0) echo "    SHA256SUMS signature verified (Ed25519)" ;;
                2)
                    [ "$ALLOW_UNSIGNED" -eq 1 ] || die "OpenSSL >= 3.0 is needed to check the release signature (apt-get install openssl), or pass --allow-unsigned"
                    echo "!!  OpenSSL >= 3.0 missing: signature NOT checked (--allow-unsigned)" >&2
                    ;;
                *) die "the SHA256SUMS signature is INVALID - nothing was installed" ;;
            esac
        elif [ "$ALLOW_UNSIGNED" -eq 0 ]; then
            echo "!!  no --key: the bundle is checked against SHA256SUMS, which is only as trustworthy as the HTTPS connection" >&2
        fi
        TARBALL="$(tr -d '\r' <"$TMP/SHA256SUMS" | awk '{n=$2; sub(/^\*/, "", n)} n ~ /^airview-pi-[0-9][0-9A-Za-z.+-]*\.tar\.gz$/ {print n}' | tail -n 1)"
        [ -n "$TARBALL" ] || die "no airview-pi-<version>.tar.gz listed in $BASE_URL/SHA256SUMS"
        NEW="$(version_of_bundle "$TARBALL")"
        if [ "$NEW" = "$INSTALLED_VERSION" ] && [ "$FORCE" -eq 0 ]; then
            echo "==> AirView $NEW is already installed (use --force to reinstall)."
            exit 0
        fi
        want="$(tr -d '\r' <"$TMP/SHA256SUMS" | awk -v f="$TARBALL" '{n=$2; sub(/^\*/, "", n)} n==f {print $1}')"
        echo "==> Downloading $TARBALL"
        fetch "$BASE_URL/$TARBALL" "$TMP/$TARBALL"
        got="$(sha256_of "$TMP/$TARBALL")"
        [ "$got" = "$want" ] || die "checksum mismatch for $TARBALL (got $got, want $want)"
        echo "    SHA-256 OK"
        (cd "$TMP" && tar xzf "$TARBALL")
        INNER="$TMP/${TARBALL%.tar.gz}/install.sh"
        [ -f "$INNER" ] || die "the bundle has no install.sh"
        set -- --from-url "$BASE_URL"
        [ -z "$KEY" ] || set -- "$@" --from-key "$KEY"
        [ "$UPGRADE" -eq 0 ] || set -- "$@" --upgrade
        [ "$NO_START" -eq 0 ] || set -- "$@" --no-start
        [ "$LOW_WEAR_JOURNAL" -eq 0 ] || set -- "$@" --low-wear-journal
        sh "$INNER" "$@"
        exit $?
    fi

    # -----------------------------------------------------------------------
    # Install from this directory.
    # -----------------------------------------------------------------------
    HERE="$(cd "$(dirname "$0")" && pwd)"
    [ -f "$HERE/$BIN" ] || die "missing $BIN next to install.sh"
    [ -f "$HERE/airview.service" ] || die "missing airview.service next to install.sh"
    NEW_VERSION="$(tr -d '\r\n' <"$HERE/VERSION" 2>/dev/null || true)"
    if [ -z "$NEW_VERSION" ]; then
        # No VERSION file (e.g. the SD-card image payload): ask the binary ("AirView 1.2.3 (...)").
        NEW_VERSION="$("$HERE/$BIN" -version 2>/dev/null | awk 'NR==1 {print $2}' || true)"
    fi
    [ -n "$NEW_VERSION" ] || NEW_VERSION="unknown"

    if [ -z "$INSTALLED_VERSION" ] && [ ! -x "$R$PREFIX/airview" ]; then
        echo "==> Installing AirView $NEW_VERSION ($BIN)"
    elif [ "$INSTALLED_VERSION" = "$NEW_VERSION" ]; then
        echo "==> Reinstalling AirView $NEW_VERSION ($BIN); data and settings are kept"
    else
        echo "==> Upgrading AirView ${INSTALLED_VERSION:-unknown} -> $NEW_VERSION ($BIN); data and settings are kept"
    fi

    if ! id airview >/dev/null 2>&1; then
        useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin airview
    fi

    mkdir -p "$R$PREFIX" "$R$DATA" "$R$ETC" "$R$(dirname "$UNIT")"
    chown airview:airview "$R$DATA"
    chmod 750 "$R$DATA"

    WAS_ACTIVE=0
    if systemctl is-active --quiet airview 2>/dev/null; then
        WAS_ACTIVE=1
        echo "==> Stopping the running service"
        systemctl stop airview
    fi

    # Keep the previous binary for the automatic roll-back (and a manual one).
    rm -f "$R$PREFIX/airview.new"
    if [ -f "$R$PREFIX/airview" ]; then
        cp -p "$R$PREFIX/airview" "$R$PREFIX/airview.prev"
        cp -p "$R$PREFIX/VERSION" "$R$PREFIX/VERSION.prev" 2>/dev/null || rm -f "$R$PREFIX/VERSION.prev"
    fi
    # Write next to the target and rename: never a half-written binary.
    install -m 0755 "$HERE/$BIN" "$R$PREFIX/airview.new"
    mv -f "$R$PREFIX/airview.new" "$R$PREFIX/airview"
    install -m 0644 "$HERE/airview.service" "$R$UNIT"
    printf '%s\n' "$NEW_VERSION" >"$R$PREFIX/VERSION"
    for f in install.sh uninstall.sh update.sh; do
        [ ! -f "$HERE/$f" ] || install -m 0755 "$HERE/$f" "$R$PREFIX/$f"
    done
    for f in README.md THIRD-PARTY-NOTICES.txt LICENSE.txt; do
        [ ! -f "$HERE/$f" ] || install -m 0644 "$HERE/$f" "$R$PREFIX/$f"
    done
    if [ -n "$FROM_URL" ]; then
        {
            printf 'BASE_URL=%s\n' "$FROM_URL"
            # SEC-R6: pin the release key for update.sh (root-owned file).
            [ -z "$FROM_KEY" ] || printf 'RELEASE_KEY=%s\n' "$FROM_KEY"
        } >"$R$ETC/update.env"
        chmod 0644 "$R$ETC/update.env"
    fi

    # Operator configuration (docs/CONFIG.md): the commented reference of every setting is refreshed
    # on every install; the operator file itself is created only once and never overwritten.
    rm -f "$R$ETC/airview.example.jsonc"
    "$R$PREFIX/airview" config example -o "$R$ETC/airview.example.jsonc" ||
        echo "!! could not write $ETC/airview.example.jsonc" >&2
    chmod 0644 "$R$ETC/airview.example.jsonc" 2>/dev/null || true
    if [ ! -f "$R$ETC/airview.jsonc" ]; then
        cat >"$R$ETC/airview.jsonc" <<'JSONC'
// AirView operator configuration (JSON with comments).
// Settings here win over the web interface and config.json; AIRVIEW_* environment
// variables win over this file. Every setting with its default and range:
// /etc/airview/airview.example.jsonc  -  check with: airview config check
{
}
JSONC
    fi
    chown root:airview "$R$ETC/airview.jsonc"
    chmod 0640 "$R$ETC/airview.jsonc"

    # Optional (operator choice, never by default): the systemd journal in RAM only. AirView
    # itself already writes only warnings and errors to the journal (process.journalLevel) and
    # keeps its full log in $DATA/airview.log.
    if [ "$LOW_WEAR_JOURNAL" -eq 1 ]; then
        JDIR="$R/etc/systemd/journald.conf.d"
        mkdir -p "$JDIR"
        cat >"$JDIR/airview-low-wear.conf" <<'JOURNALD'
# Written by the AirView installer (--low-wear-journal): keep the journal in RAM only so the
# SD card is not written for log lines. The journal of every service is lost at a reboot.
# Undo: delete this file and run "systemctl restart systemd-journald".
[Journal]
Storage=volatile
RuntimeMaxUse=32M
JOURNALD
        chmod 0644 "$JDIR/airview-low-wear.conf"
        systemctl restart systemd-journald 2>/dev/null ||
            echo "!! could not restart systemd-journald; the journal setting applies after the next reboot" >&2
        echo "==> Journal kept in RAM only (/etc/systemd/journald.conf.d/airview-low-wear.conf)"
    fi

    echo "==> Checking the configuration"
    AIRVIEW_CONFIG="$R$ETC/airview.jsonc" AIRVIEW_DATA="$R$DATA" "$R$PREFIX/airview" config check -q ||
        echo "!! The configuration has errors (see above); AirView repairs config.json itself, fix $ETC/airview.jsonc by hand." >&2

    systemctl daemon-reload
    systemctl enable airview >/dev/null
    if [ "$NO_START" -eq 1 ]; then
        echo "==> Installed AirView $NEW_VERSION; not started (--no-start). Start it with: systemctl start airview"
        [ "$WAS_ACTIVE" -eq 0 ] || echo "    The service was running before and is stopped now."
        exit 0
    fi
    systemctl restart airview

    # Health: the unit must stay active for a few seconds.
    ok=0
    i=0
    while [ $i -lt 10 ]; do
        sleep 1
        if systemctl is-active --quiet airview; then
            ok=$((ok + 1))
            [ $ok -lt 3 ] || break
        else
            ok=0
        fi
        i=$((i + 1))
    done
    if [ $ok -lt 3 ]; then
        echo "!! AirView $NEW_VERSION failed to start. Last log lines:" >&2
        journalctl -u airview -n 30 --no-pager >&2 2>/dev/null || true
        if [ -f "$R$PREFIX/airview.prev" ]; then
            echo "!! Rolling back to the previous binary (${INSTALLED_VERSION:-unknown})" >&2
            systemctl stop airview 2>/dev/null || true
            mv -f "$R$PREFIX/airview.prev" "$R$PREFIX/airview"
            if [ -f "$R$PREFIX/VERSION.prev" ]; then mv -f "$R$PREFIX/VERSION.prev" "$R$PREFIX/VERSION"; else rm -f "$R$PREFIX/VERSION"; fi
            systemctl restart airview || true
        fi
        exit 1
    fi

    IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
    echo "==> AirView $NEW_VERSION is running."
    echo "    Web interface : http://${IP:-<this-pi>}/   (port 80 by default; change it in Settings)"
    echo "    Data directory: $DATA"
    echo "    Configuration : $ETC/airview.jsonc (reference: $ETC/airview.example.jsonc)"
    echo "    Logs          : $DATA/airview.log (the journal, journalctl -u airview, keeps warnings and errors)"
    echo "    Update        : sudo $PREFIX/update.sh [--base-url https://<release-site>]"
    echo "    If port 80 is taken AirView falls back to 8080+ - check the log for the actual port."
}

main "$@"
