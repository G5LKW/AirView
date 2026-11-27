#!/usr/bin/env bash
# AirView Network feeder installer for Raspberry Pi OS, Debian and Ubuntu
# (arm64, armhf/armv7, amd64). DESIGN §12.2; documentation: README.md next to
# this file.
#
#   curl -fsSL https://<aggregator>/feeder/install.sh -o /tmp/avf.sh && sudo bash /tmp/avf.sh
#   curl -fsSL https://<aggregator>/feeder/install.sh | sudo bash -s -- --server host:30160 ...
#
# The same script, installed as /opt/airview-feeder/install.sh (and linked as
# /usr/local/sbin/airview-feeder-setup), also runs the update, uninstall,
# status and claim actions:
#
#   airview-feeder-setup --status | --update | --uninstall [--purge] | --claim
#
# What it never does: remove, stop or reconfigure readsb, dump1090, a
# Radarcape or any other feeder; install GPL decoders by itself; send
# anything anywhere except to the aggregator you name.
#
# Everything is inside functions and the last line calls main, so the script
# also works when piped into bash (prompts read /dev/tty, never stdin).

# The aggregator rewrites these lines when it serves the script (one
# assignment per line, exactly `NAME=""`), so a user can run it without
# flags. Command-line flags win over them.
avf_presets() {
	PRESET_SERVER=""
	PRESET_PIN=""
	PRESET_URL=""
	PRESET_MANIFEST_KEY=""
	PRESET_HTTP_PIN=""
}

avf_constants() {
	INSTALLER_VERSION="1.0.0"
	PREFIX=/opt/airview-feeder
	ETC=/etc/airview-feeder
	DATA=/var/lib/airview-feeder
	SVC_USER=airview-feeder
	UNIT_DIR=/etc/systemd/system
	SERVICE=airview-feeder
	CONF="$ETC/feeder.json"
	STATE="$ETC/install.env"
	BIN="$PREFIX/airview-feeder"
	SETUP_LINK=/usr/local/sbin/airview-feeder-setup
	UI_ADDR=127.0.0.1:8099
	DEFAULT_AVF_PORT=30160
	RESULTS_PORT=30105
	MIN_FREE_KB=51200
	UPLINK_ID=aggregator
	CONSENT_TEXT="If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this."
	# C2 (docs/aggregator/CONSENT.md, word for word; pi_test.go TestConsentPinMatchesDoc).
	CONSENT_PIN="Show my station on the public map?
  - Hidden (default): no pin.
  - Region: a point placed at random inside a 0.5-degree area around your station, about 55 by 35 km in the UK.
  - Area: the same inside a 0.1-degree area, about 11 by 7 km. Where fewer than 3 stations are nearby it is shown as Region.
The point is chosen once and stays put, so it cannot be averaged out. Your exact position is never public. You can change this at any time."
}

# ---------------------------------------------------------------- output ----

avf_init_output() {
	if [ -t 2 ] && [ -z "${NO_COLOR:-}" ]; then
		C_B=$'\033[1m' C_Y=$'\033[33m' C_R=$'\033[31m' C_G=$'\033[32m' C_0=$'\033[0m'
	else
		C_B='' C_Y='' C_R='' C_G='' C_0=''
	fi
}

say() { [ "${QUIET:-0}" = 1 ] || printf '%s==>%s %s\n' "$C_B" "$C_0" "$*"; }
info() { [ "${QUIET:-0}" = 1 ] || printf '    %s\n' "$*"; }
ok() { [ "${QUIET:-0}" = 1 ] || printf '    %sOK%s %s\n' "$C_G" "$C_0" "$*"; }
warn() { printf '%s!!%s %s\n' "$C_Y" "$C_0" "$*" >&2; }
die() {
	printf '%sERROR:%s %s\n' "$C_R" "$C_0" "$*" >&2
	exit 1
}

# quote_cmd prints its arguments shell-quoted, for dry-run output.
quote_cmd() {
	local out='' a
	for a in "$@"; do
		out+="$(printf '%q' "$a") "
	done
	printf '%s' "${out% }"
}

# run executes a command that changes the system; in dry-run mode it only
# prints it.
run() {
	if [ "$DRY_RUN" = 1 ]; then
		printf '[dry-run] %s\n' "$(quote_cmd "$@")"
		return 0
	fi
	"$@"
}

# rpath maps a system path for reading. In dry-run mode a file staged by an
# earlier dry run wins, so re-running against the same staging root behaves
# like re-running on an installed system.
rpath() {
	if [ "$DRY_RUN" = 1 ] && [ -e "$DRY_ROOT$1" ]; then
		printf '%s' "$DRY_ROOT$1"
	else
		printf '%s' "$1"
	fi
}

# put_file DEST MODE OWNER:GROUP writes stdin to DEST atomically.
put_file() {
	local dest=$1 mode=$2 owner=$3 tmp
	if [ "$DRY_RUN" = 1 ]; then
		mkdir -p "$DRY_ROOT$(dirname "$dest")"
		cat >"$DRY_ROOT$dest"
		chmod "$mode" "$DRY_ROOT$dest" 2>/dev/null || true
		printf '[dry-run] write %s (mode %s, owner %s) -> %s\n' "$dest" "$mode" "$owner" "$DRY_ROOT$dest"
		return 0
	fi
	tmp=$(mktemp "$(dirname "$dest")/.$(basename "$dest").XXXXXX")
	cat >"$tmp"
	chmod "$mode" "$tmp"
	chown "$owner" "$tmp"
	mv -f "$tmp" "$dest"
}

# put_copy SRC DEST MODE OWNER:GROUP copies a file atomically.
put_copy() {
	put_file "$2" "$3" "$4" <"$1"
}

# install_notices copies the licence texts that ship next to a local
# --binary (the unpacked airview-feeder-pi bundle, the SD-card image's
# /opt/airview-image) into $PREFIX: the Go standard library's BSD licence
# must travel with the binary (DLV-05).
install_notices() {
	local d f
	[ -n "${BINARY_FILE:-}" ] || return 0
	d=$(dirname "$BINARY_FILE")
	for f in LICENSE.txt THIRD-PARTY-NOTICES.txt; do
		if [ -f "$d/$f" ]; then
			put_copy "$d/$f" "$PREFIX/$f" 0644 root:root
		fi
	done
}

# ----------------------------------------------------------- arguments ----

usage() {
	cat <<'EOF'
AirView Network feeder installer (Raspberry Pi OS / Debian / Ubuntu; arm64, armhf, amd64)

Usage:
  sudo bash install.sh [options]                 install or reconfigure
  sudo bash install.sh --update [--force]        update the feeder binary now
  sudo bash install.sh --update --check          only check for a newer version (exit 10 if one exists)
  sudo bash install.sh --update --channel beta   switch the release channel (stable|beta) and update
  sudo bash install.sh --status                  show service, config and link state
  sudo bash install.sh --claim                   print a claim code for this station
  sudo bash install.sh --uninstall [--purge]     remove the feeder (readsb is never touched)

Connection:
  --server HOST[:PORT]     aggregator AVF/1 address (port 30160 by default)
  --pin sha256/BASE64      expected server key (SPKI pin); empty = system CA roots
  --token TOKEN            enrol token from the aggregator admin (alias: --enrol)
  --url https://HOST       download base URL (default https://HOST of --server)
  --manifest-key BASE64    Ed25519 public key that signs the release manifest
  --http-pin sha256//B64   pin the download server's key (curl --pinnedpubkey)
  --binary FILE            install this airview-feeder binary instead of downloading

Station:
  --name NAME              station name, [A-Za-z0-9_-]{1,32} (default: host name)
  --lat DEG --lon DEG      antenna position (decimal degrees)
  --alt 120m|400ft         antenna altitude, unit suffix mandatory
  --alt-ref msl|hae        altitude reference (default msl)
  --agl 8m|25ft            antenna height above ground (default 10m)
  --no-location            send no position; the network locates the station
  --input SPEC             auto (default) | HOST:PORT (Beast) | beast:HOST:PORT |
                           avr:HOST:PORT | sbs:HOST:PORT | radarcape:HOST
  --privacy LEVEL          public map pin: hidden (default) | region | cell01
  --no-timestamps          send no timestamps (no MLAT, no timing self-location)
  --readsb-results         also push returned MLAT results into readsb (127.0.0.1:30104)
  --no-auto-update         do not install the daily update timer
  --channel stable|beta    release channel for updates (default stable)

Behaviour:
  --non-interactive, -y    never prompt; use flags, saved values and defaults
  --install-chrony         install chrony when the clock is not NTP-synchronised
  --allow-unsigned         accept a manifest without a verifiable signature
  --no-start               install but do not start the service
  --arch arm64|armhf|amd64 override the detected architecture
  --dry-run                change nothing; print every command and stage files
  --dry-run-root DIR       where --dry-run stages files (default: a temp dir)
  --quiet                  less output (used by the update timer)
  --help, --version
EOF
}

parse_args() {
	ACTION=install
	DRY_RUN=0 DRY_ROOT='' NONINTERACTIVE=0 QUIET=0 FORCE=0 PURGE=0 NO_START=0
	INSTALL_CHRONY=0 ALLOW_UNSIGNED=0 ARCH_OVERRIDE='' BINARY_FILE=''
	F_SERVER='' F_PIN='' F_TOKEN='' F_URL='' F_MKEY='' F_HTTP_PIN='' F_NAME=''
	F_LAT='' F_LON='' F_ALT='' F_ALT_REF='' F_AGL='' F_INPUT='' F_PRIVACY=''
	F_NOLOC='' F_NOTS='' F_READSB='' F_AUTOUPD='' F_CHANNEL='' CHECK_ONLY=0
	SET_PIN=0 SET_TOKEN=0
	while [ $# -gt 0 ]; do
		case $1 in
		--server) need_val "$@"; F_SERVER=$2; shift ;;
		--pin) need_val0 "$@"; F_PIN=$2; SET_PIN=1; shift ;;
		--token | --enrol) need_val0 "$@"; F_TOKEN=$2; SET_TOKEN=1; shift ;;
		--url) need_val "$@"; F_URL=$2; shift ;;
		--manifest-key) need_val "$@"; F_MKEY=$2; shift ;;
		--http-pin) need_val "$@"; F_HTTP_PIN=$2; shift ;;
		--binary) need_val "$@"; BINARY_FILE=$2; shift ;;
		--name) need_val "$@"; F_NAME=$2; shift ;;
		--lat) need_val "$@"; F_LAT=$2; shift ;;
		--lon) need_val "$@"; F_LON=$2; shift ;;
		--alt) need_val "$@"; F_ALT=$2; shift ;;
		--alt-ref) need_val "$@"; F_ALT_REF=$2; shift ;;
		--agl) need_val "$@"; F_AGL=$2; shift ;;
		--no-location) F_NOLOC=1 ;;
		--input) need_val "$@"; F_INPUT=$2; shift ;;
		--privacy) need_val "$@"; F_PRIVACY=$2; shift ;;
		--no-timestamps) F_NOTS=1 ;;
		--timestamps) F_NOTS=0 ;;
		--readsb-results) F_READSB=1 ;;
		--no-readsb-results) F_READSB=0 ;;
		--no-auto-update) F_AUTOUPD=0 ;;
		--auto-update) F_AUTOUPD=1 ;;
		--channel) need_val "$@"; F_CHANNEL=$2; shift ;;
		--check) CHECK_ONLY=1 ;;
		--non-interactive | -y | --yes) NONINTERACTIVE=1 ;;
		--install-chrony) INSTALL_CHRONY=1 ;;
		--allow-unsigned) ALLOW_UNSIGNED=1 ;;
		--no-start) NO_START=1 ;;
		--arch) need_val "$@"; ARCH_OVERRIDE=$2; shift ;;
		--dry-run) DRY_RUN=1 ;;
		--dry-run-root) need_val "$@"; DRY_ROOT=$2; shift ;;
		--quiet | -q) QUIET=1 ;;
		--force) FORCE=1 ;;
		--purge) PURGE=1 ;;
		--update) ACTION=update ;;
		--uninstall) ACTION=uninstall ;;
		--status) ACTION=status ;;
		--claim) ACTION=claim ;;
		--help | -h) usage; exit 0 ;;
		--version) printf 'airview-feeder installer %s\n' "$INSTALLER_VERSION"; exit 0 ;;
		--*=*) set -- "${1%%=*}" "${1#*=}" "${@:2}"; continue ;;
		*) die "unknown option: $1 (see --help)" ;;
		esac
		shift
	done
	if [ "$ACTION" = update ] && [ "$QUIET" = 1 ]; then NONINTERACTIVE=1; fi
	if [ -n "$F_CHANNEL" ] && ! valid_channel "$F_CHANNEL"; then die "invalid --channel '$F_CHANNEL' (stable or beta)"; fi
	if [ "$CHECK_ONLY" = 1 ] && [ "$ACTION" != update ]; then die "--check goes with --update"; fi
}

need_val() { [ $# -ge 2 ] && [ -n "$2" ] || die "option $1 needs a value"; }
need_val0() { [ $# -ge 2 ] || die "option $1 needs a value"; }

# ------------------------------------------------------------ validation ----

valid_name() { [[ $1 =~ ^[A-Za-z0-9_-]{1,32}$ ]]; }
valid_host() { [[ $1 =~ ^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})$ ]] || [[ $1 =~ ^\[[0-9A-Fa-f:.]+\]$ ]]; }
valid_port() { [[ $1 =~ ^[0-9]{1,5}$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }
valid_pin() { [ -z "$1" ] || [[ $1 =~ ^sha256/[A-Za-z0-9+/]{43}=$ ]]; }
# (Interval bounds stay <= 255: RE_DUP_MAX on glibc and musl.)
valid_token() { [ -z "$1" ] || [[ $1 =~ ^[A-Za-z0-9._~+/=-]{4,255}$ ]]; }
valid_channel() { case $1 in stable | beta) return 0 ;; esac; return 1; }
valid_privacy() { case $1 in hidden | region | cell01) return 0 ;; esac; return 1; }
valid_url() {
	local re='^https?://[][A-Za-z0-9.:-]+(/[A-Za-z0-9._~/-]*)?$'
	[[ $1 =~ $re ]]
}
valid_b64key() { [[ $1 =~ ^[A-Za-z0-9+/]{43}=$ ]]; }
valid_http_pin() {
	local re='^sha256//[A-Za-z0-9+/]{43}=(;sha256//[A-Za-z0-9+/]{43}=)*$'
	[ -z "$1" ] || [[ $1 =~ $re ]]
}

# num_in VALUE LO HI: VALUE is a decimal number within [LO, HI].
num_in() {
	[[ $1 =~ ^[+-]?[0-9]+(\.[0-9]+)?$ ]] || return 1
	awk -v v="$1" -v lo="$2" -v hi="$3" 'BEGIN { exit !(v + 0 >= lo + 0 && v + 0 <= hi + 0) }'
}

# to_metres "120m" | "400 ft" | "8.5M": prints metres with one decimal.
# The unit suffix is mandatory (DESIGN §12.2); 1 ft = 0.3048 m exactly.
to_metres() {
	local s=${1// /} num unit
	s=$(printf '%s' "$s" | tr 'A-Z' 'a-z')
	[[ $s =~ ^([+-]?[0-9]+(\.[0-9]+)?)(m|ft|feet|foot)$ ]] || return 1
	num=${BASH_REMATCH[1]} unit=${BASH_REMATCH[3]}
	case $unit in
	m) awk -v v="$num" 'BEGIN { printf "%.1f\n", v + 0 }' ;;
	*) awk -v v="$num" 'BEGIN { printf "%.1f\n", v * 0.3048 }' ;;
	esac
}

# split_server "host[:port]" -> SRV_HOST SRV_PORT
split_server() {
	local s=$1
	SRV_HOST='' SRV_PORT=$DEFAULT_AVF_PORT
	if [[ $s =~ ^\[([0-9A-Fa-f:.]+)\](:([0-9]+))?$ ]]; then
		SRV_HOST="[${BASH_REMATCH[1]}]"
		[ -z "${BASH_REMATCH[3]}" ] || SRV_PORT=${BASH_REMATCH[3]}
	elif [[ $s =~ ^([^:]+)(:([0-9]+))?$ ]]; then
		SRV_HOST=${BASH_REMATCH[1]}
		[ -z "${BASH_REMATCH[3]}" ] || SRV_PORT=${BASH_REMATCH[3]}
	else
		return 1
	fi
	valid_host "$SRV_HOST" && valid_port "$SRV_PORT"
}

# parse_input SPEC -> IN_TYPE IN_HOST IN_PORT IN_LABEL
parse_input() {
	local s=$1 kind rest
	IN_TYPE='' IN_HOST='' IN_PORT=0 IN_LABEL=''
	case $s in
	auto | '')
		IN_TYPE=auto IN_LABEL="Auto-detected receiver (127.0.0.1:30005 or Radarcape :10003)"
		return 0
		;;
	radarcape:*)
		IN_TYPE=beast IN_HOST=${s#radarcape:} IN_PORT=10003
		valid_host "$IN_HOST" || return 1
		IN_LABEL="Radarcape $IN_HOST:10003"
		return 0
		;;
	beast:* | avr:* | sbs:*) kind=${s%%:*} rest=${s#*:} ;;
	*) kind=beast rest=$s ;;
	esac
	if [[ $rest =~ ^(\[[0-9A-Fa-f:.]+\]|[^:]+):([0-9]+)$ ]]; then
		IN_HOST=${BASH_REMATCH[1]} IN_PORT=${BASH_REMATCH[2]}
	else
		return 1
	fi
	valid_host "$IN_HOST" && valid_port "$IN_PORT" || return 1
	IN_TYPE=$kind
	IN_LABEL="$kind $IN_HOST:$IN_PORT"
}

# json_str escapes a string for JSON (inputs are validated, this is a guard).
json_str() {
	local s=$1
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	printf '"%s"' "$s"
}

# ---------------------------------------------------------- environment ----

have() { command -v "$1" >/dev/null 2>&1; }

# port_listening PORT: something listens on TCP PORT on this host.
port_listening() {
	local p=$1 hex
	if have ss; then
		ss -Hltn 2>/dev/null | awk -v p=":$p" '{ n = split($4, a, ":"); if (":" a[n] == p) f = 1 } END { exit !f }'
		return
	fi
	hex=$(printf '%04X' "$p")
	if [ -r /proc/net/tcp ]; then
		awk -v h=":$hex" 'FNR > 1 && $4 == "0A" && substr($2, length($2) - 4) == h { f = 1 } END { exit !f }' \
			/proc/net/tcp /proc/net/tcp6 2>/dev/null
		return
	fi
	return 1
}

proc_running() {
	if have pgrep; then
		pgrep -x "$1" >/dev/null 2>&1
	else
		return 1
	fi
}

# usb_present VENDOR[:PRODUCT] (lower-case hex)
usb_present() {
	local want=$1 d v p
	if have lsusb; then
		lsusb 2>/dev/null | tr 'A-F' 'a-f' | grep -q " ${want}"
		return
	fi
	for d in /sys/bus/usb/devices/*; do
		[ -r "$d/idVendor" ] || continue
		v=$(cat "$d/idVendor" 2>/dev/null) p=$(cat "$d/idProduct" 2>/dev/null)
		case $want in
		*:*) [ "$v:$p" = "$want" ] && return 0 ;;
		*) [ "$v" = "$want" ] && return 0 ;;
		esac
	done
	return 1
}

detect_target() {
	local m dpkg_arch=''
	if [ -n "$ARCH_OVERRIDE" ]; then
		case $ARCH_OVERRIDE in
		arm64 | aarch64) TARGET=linux-arm64 ;;
		armhf | armv7 | armv7l) TARGET=linux-armv7 ;;
		amd64 | x86_64) TARGET=linux-amd64 ;;
		*) die "--arch must be arm64, armhf or amd64" ;;
		esac
		return
	fi
	m=$(uname -m)
	if have dpkg; then dpkg_arch=$(dpkg --print-architecture 2>/dev/null || true); fi
	case $m in
	aarch64 | arm64)
		# A 64-bit kernel under a 32-bit (armhf) userland: use the armv7 build.
		if [ "$dpkg_arch" = armhf ]; then TARGET=linux-armv7; else TARGET=linux-arm64; fi
		;;
	armv7l | armv8l) TARGET=linux-armv7 ;;
	x86_64 | amd64)
		[ "$dpkg_arch" != i386 ] || die "32-bit x86 userland is not supported"
		TARGET=linux-amd64
		;;
	armv6l | armv6*) die "ARMv6 (Raspberry Pi 1, Zero, Zero W) is not supported; use a Pi 2/3/4/5 or Zero 2 W" ;;
	*) die "unsupported CPU architecture: $m (supported: aarch64, armv7l, x86_64)" ;;
	esac
}

sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | awk '{ print $1 }'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	else
		openssl dgst -sha256 -r "$1" | awk '{ print $1 }'
	fi
}

# fetch URL OUT: download with curl (or wget). HTTP_PIN pins the server key.
fetch() {
	local url=$1 out=$2
	if have curl; then
		local opts=(-fsSL --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 600 --proto =https,http -o "$out")
		if [ -n "$HTTP_PIN" ]; then
			# --pinnedpubkey is still enforced with --insecure, so a
			# self-signed aggregator certificate works when it is pinned.
			opts+=(--insecure "--pinnedpubkey" "$HTTP_PIN")
		fi
		curl "${opts[@]}" "$url"
	elif have wget; then
		[ -z "$HTTP_PIN" ] || die "--http-pin needs curl (apt-get install curl)"
		wget -q -T 30 -t 3 -O "$out" "$url"
	else
		die "neither curl nor wget is installed (apt-get install curl)"
	fi
}

# ed25519_verify KEY_B64 FILE SIG_FILE(base64): needs OpenSSL >= 3.0.
ed25519_verify() {
	local key=$1 file=$2 sigb64=$3 dir rc=0
	have openssl || return 2
	case $(openssl version 2>/dev/null) in
	"OpenSSL 0."* | "OpenSSL 1."* | "") return 2 ;;
	esac
	dir=$(mktemp -d)
	# SubjectPublicKeyInfo DER for Ed25519 = fixed 12-byte prefix + 32-byte key.
	printf '\x30\x2a\x30\x05\x06\x03\x2b\x65\x70\x03\x21\x00' >"$dir/key.der"
	printf '%s' "$key" | base64 -d >>"$dir/key.der" 2>/dev/null || rc=3
	tr -d ' \r\n\t' <"$sigb64" | base64 -d >"$dir/sig.bin" 2>/dev/null || rc=3
	if [ $rc = 0 ] && [ "$(wc -c <"$dir/key.der" | tr -d ' ')" = 44 ] && [ "$(wc -c <"$dir/sig.bin" | tr -d ' ')" = 64 ]; then
		openssl pkeyutl -verify -pubin -keyform DER -inkey "$dir/key.der" -rawin \
			-in "$file" -sigfile "$dir/sig.bin" >/dev/null 2>&1 || rc=1
	else
		rc=1
	fi
	rm -rf "$dir"
	return $rc
}

# manifest_field FILE TARGET KEY: value of KEY ("sha256", "url", "size") in
# the flat JSON object of the manifest that has TARGET as one of its values.
manifest_field() {
	awk -v tgt="$2" -v key="$3" '
		BEGIN { RS = "[{}]" }
		{
			gsub(/[\r\n\t]/, " ")
			if ($0 !~ ("\"" tgt "\"") || $0 !~ (":[ ]*\"" tgt "\"")) next
			s = $0
			if (match(s, "\"" key "\"[ ]*:[ ]*\"[^\"]*\"")) {
				v = substr(s, RSTART, RLENGTH); sub(/^"[^"]*"[ ]*:[ ]*"/, "", v); sub(/"$/, "", v)
				print v; exit
			}
			if (match(s, "\"" key "\"[ ]*:[ ]*[0-9]+")) {
				v = substr(s, RSTART, RLENGTH); sub(/^"[^"]*"[ ]*:[ ]*/, "", v)
				print v; exit
			}
		}' "$1"
}

manifest_version() {
	awk '{ gsub(/[\r\n\t]/, " ") } match($0, /"version"[ ]*:[ ]*"[^"]*"/) {
		v = substr($0, RSTART, RLENGTH); sub(/^"version"[ ]*:[ ]*"/, "", v); sub(/"$/, "", v); print v; exit }' "$1"
}

is_elf() {
	[ "$(head -c 4 "$1" | od -An -tx1 | tr -d ' \n')" = 7f454c46 ]
}

# obtain_binary OUT: put a verified airview-feeder binary for TARGET at OUT.
# Sets BIN_VERSION and BIN_SHA.
obtain_binary() {
	local out=$1 work url sha size got here
	BIN_VERSION=''
	# Run from the unpacked airview-feeder-pi.tar.gz (no --binary, no --url,
	# not the preset script served by an aggregator):
	# use the bundled binary for this CPU (deploy/build.ps1 names them
	# airview-feeder-<target>; the bundle is checked with SHA256SUMS).
	if [ -z "$BINARY_FILE" ] && [ -z "${F_URL:-}" ] && [ -z "${PRESET_URL:-}" ]; then
		here=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd) || here=''
		if [ -n "$here" ] && [ -f "$here/airview-feeder-$TARGET" ]; then
			BINARY_FILE="$here/airview-feeder-$TARGET"
			say "Using the bundled binary $BINARY_FILE"
		fi
	fi
	if [ -n "$BINARY_FILE" ]; then
		[ -f "$BINARY_FILE" ] || die "--binary $BINARY_FILE: no such file"
		cp "$BINARY_FILE" "$out"
		is_elf "$out" || die "--binary $BINARY_FILE is not a Linux (ELF) executable"
		BIN_SHA=$(sha256_of "$out")
		BIN_VERSION=local
		warn "installing a local binary; its signature is not checked (sha256 $BIN_SHA)"
		return 0
	fi
	[ -n "$URL" ] || die "no download URL: pass --url https://<aggregator> or --binary FILE"
	case $URL in
	http://*)
		if [ -z "$MKEY" ] && [ "$ALLOW_UNSIGNED" != 1 ]; then
			die "refusing an unsigned download over plain HTTP ($URL); use https, --manifest-key, or --allow-unsigned"
		fi
		;;
	esac
	work=$(mktemp -d)
	say "Downloading the release manifest from $URL/feeder/manifest.json"
	fetch "$URL/feeder/manifest.json" "$work/manifest.json" || {
		rm -rf "$work"
		die "could not download $URL/feeder/manifest.json"
	}
	if [ -n "$MKEY" ]; then
		fetch "$URL/feeder/manifest.json.sig" "$work/manifest.sig" || {
			rm -rf "$work"
			die "could not download the manifest signature ($URL/feeder/manifest.json.sig)"
		}
		local vrc=0
		ed25519_verify "$MKEY" "$work/manifest.json" "$work/manifest.sig" || vrc=$?
		case $vrc in
		0) ok "manifest signature verified (Ed25519)" ;;
		2)
			if [ "$ALLOW_UNSIGNED" = 1 ]; then
				warn "OpenSSL >= 3.0 is needed to check the Ed25519 signature; continuing because of --allow-unsigned"
			else
				rm -rf "$work"
				die "OpenSSL >= 3.0 is needed to check the manifest signature (apt-get install openssl), or pass --allow-unsigned"
			fi
			;;
		*)
			rm -rf "$work"
			die "the release manifest signature is INVALID - nothing was installed"
			;;
		esac
	elif [ "$ALLOW_UNSIGNED" != 1 ]; then
		warn "no --manifest-key: the binary is checked against the manifest hash, which is only as trustworthy as the HTTPS connection"
	fi
	sha=$(manifest_field "$work/manifest.json" "$TARGET" sha256 | tr 'A-F' 'a-f')
	url=$(manifest_field "$work/manifest.json" "$TARGET" url)
	size=$(manifest_field "$work/manifest.json" "$TARGET" size)
	BIN_VERSION=$(manifest_version "$work/manifest.json")
	[[ $sha =~ ^[0-9a-f]+$ ]] && [ ${#sha} = 64 ] || {
		rm -rf "$work"
		die "the manifest has no sha256 for $TARGET"
	}
	case $url in
	'') url="$URL/feeder/dl/$TARGET" ;;
	http://* | https://*) ;;
	/*) url="$URL$url" ;;
	*) url="$URL/feeder/$url" ;;
	esac
	say "Downloading airview-feeder ${BIN_VERSION:+$BIN_VERSION }for $TARGET"
	fetch "$url" "$work/bin" || {
		rm -rf "$work"
		die "could not download $url"
	}
	got=$(sha256_of "$work/bin")
	if [ "$got" != "$sha" ]; then
		rm -rf "$work"
		die "SHA-256 mismatch for $url (expected $sha, got $got) - nothing was installed"
	fi
	if [ -n "$size" ] && [ "$(wc -c <"$work/bin" | tr -d ' ')" != "$size" ]; then
		rm -rf "$work"
		die "size mismatch for $url"
	fi
	is_elf "$work/bin" || {
		rm -rf "$work"
		die "the download is not a Linux executable"
	}
	ok "SHA-256 matches the manifest ($sha)"
	mv -f "$work/bin" "$out"
	rm -rf "$work"
	BIN_SHA=$sha
}

# --------------------------------------------------------------- prompts ----

can_prompt() {
	[ "$NONINTERACTIVE" = 0 ] && { : </dev/tty >/dev/tty; } 2>/dev/null
}

use_whiptail() { have whiptail && [ -z "${AVF_NO_WHIPTAIL:-}" ]; }

# ask VAR "question" DEFAULT
ask() {
	local __var=$1 q=$2 def=$3 ans
	if use_whiptail; then
		ans=$(whiptail --title "AirView feeder" --inputbox "$q" 12 74 "$def" 3>&1 1>/dev/tty 2>&3 </dev/tty) ||
			die "cancelled"
	else
		printf '%s [%s]: ' "$q" "$def" >/dev/tty
		IFS= read -r ans </dev/tty || ans=''
		[ -n "$ans" ] || ans=$def
	fi
	printf -v "$__var" '%s' "$ans"
}

# ask_secret VAR "question": empty answer keeps the old value.
ask_secret() {
	local __var=$1 q=$2 ans
	if use_whiptail; then
		ans=$(whiptail --title "AirView feeder" --passwordbox "$q" 12 74 3>&1 1>/dev/tty 2>&3 </dev/tty) || die "cancelled"
	else
		printf '%s: ' "$q" >/dev/tty
		IFS= read -rs ans </dev/tty || ans=''
		printf '\n' >/dev/tty
	fi
	printf -v "$__var" '%s' "$ans"
}

# ask_yesno "question" DEFAULT(y|n): returns 0 for yes.
ask_yesno() {
	local q=$1 def=$2 ans
	if use_whiptail; then
		local extra=()
		[ "$def" = y ] || extra=(--defaultno)
		whiptail --title "AirView feeder" "${extra[@]}" --yesno "$q" 14 74 </dev/tty >/dev/tty 2>&1
		return
	fi
	printf '%s [%s]: ' "$q" "$([ "$def" = y ] && echo Y/n || echo y/N)" >/dev/tty
	IFS= read -r ans </dev/tty || ans=''
	case ${ans:-$def} in y | Y | yes | YES | Yes) return 0 ;; esac
	return 1
}

show_text() {
	if use_whiptail; then
		whiptail --title "AirView feeder" --msgbox "$1" 16 74 </dev/tty >/dev/tty 2>&1 || true
	else
		printf '\n%s\n\n' "$1" >/dev/tty
	fi
}

# prompt_valid VAR "question" DEFAULT validator [error]
prompt_valid() {
	local __var=$1 q=$2 def=$3 check=$4 err=${5:-"invalid value"} v
	while :; do
		ask v "$q" "$def"
		if "$check" "$v"; then
			printf -v "$__var" '%s' "$v"
			return 0
		fi
		if use_whiptail; then show_text "$err"; else printf '  %s\n' "$err" >/dev/tty; fi
	done
}

chk_server() { split_server "$1"; }
chk_lat() { num_in "$1" -90 90; }
chk_lon() { num_in "$1" -180 180; }
chk_alt() { local m; m=$(to_metres "$1") && num_in "$m" -500 9000; }
chk_agl() { local m; m=$(to_metres "$1") && num_in "$m" 0 1000; }
chk_input() { parse_input "$1"; }
chk_altref() { case $1 in msl | hae) return 0 ;; esac; return 1; }

# ---------------------------------------------------------------- state ----

# load_state: saved answers of the last run (root-only shell file of
# NAME=quoted-value lines written by save_state).
load_state() {
	S_SERVER='' S_PIN='' S_TOKEN='' S_URL='' S_MKEY='' S_HTTP_PIN='' S_NAME=''
	S_LOCATED='' S_LAT='' S_LON='' S_ALT='' S_ALT_REF='' S_AGL='' S_INPUT=''
	S_PRIVACY='' S_TIMESTAMPS='' S_READSB='' S_AUTOUPD='' S_TARGET='' S_CHANNEL=''
	local f line key val
	f=$(rpath "$STATE")
	[ -r "$f" ] || return 0
	HAVE_STATE=1
	while IFS= read -r line || [ -n "$line" ]; do
		case $line in '#'* | '') continue ;; esac
		key=${line%%=*} val=${line#*=}
		[[ $key =~ ^S_[A-Z_]+$ ]] || continue
		# Values are written with printf %q; undo the quoting without eval.
		val=$(unquote "$val")
		printf -v "$key" '%s' "$val"
	done <"$f"
}

# unquote reverses printf %q for the character set the validators allow
# (letters, digits, . _ ~ + / = - : [ ] and spaces in names never occur).
unquote() {
	local v=$1
	case $v in
	\'*\') v=${v#\'} v=${v%\'} ;;
	*) v=${v//\\/} ;;
	esac
	printf '%s' "$v"
}

save_state() {
	local k
	{
		printf '# AirView feeder installer answers (re-running the installer offers them as defaults).\n'
		printf '# Written by install.sh %s; root-only because it holds the enrol token.\n' "$INSTALLER_VERSION"
		for k in S_SERVER S_PIN S_TOKEN S_URL S_MKEY S_HTTP_PIN S_NAME S_LOCATED S_LAT S_LON S_ALT S_ALT_REF \
			S_AGL S_INPUT S_PRIVACY S_TIMESTAMPS S_READSB S_AUTOUPD S_TARGET S_CHANNEL; do
			printf '%s=%q\n' "$k" "${!k}"
		done
	} | put_file "$STATE" 0600 root:root
}

# ------------------------------------------------------------ prechecks ----

prechecks() {
	say "Checking this system"
	if [ "$(id -u)" != 0 ]; then
		if [ "$DRY_RUN" = 1 ]; then warn "not root (ignored in --dry-run)"; else die "please run as root: sudo bash $0"; fi
	fi
	if [ ! -d /run/systemd/system ]; then
		if [ "$DRY_RUN" = 1 ]; then warn "systemd is not running (ignored in --dry-run)"; else die "systemd is required"; fi
	fi
	detect_target
	ok "architecture $(uname -m) -> $TARGET"
	local d=$PREFIX avail
	while [ ! -d "$d" ]; do d=$(dirname "$d"); done
	avail=$(df -Pk "$d" 2>/dev/null | awk 'NR == 2 { print $4 }')
	if [ -n "$avail" ] && [ "$avail" -lt "$MIN_FREE_KB" ] 2>/dev/null; then
		die "less than $((MIN_FREE_KB / 1024)) MB free on $d"
	fi
	ok "disk space (${avail:-?} KB free on $d)"
	check_ntp
	if [ -r /etc/default/adsbexchange ] && grep -Eq '(127\.0\.0\.1|localhost)[:,]30154' /etc/default/adsbexchange; then
		warn "/etc/default/adsbexchange sends results to 127.0.0.1:30154, the AirView hub port; if the AirView app also runs here, move one of them"
	fi
	if port_listening 8099 && ! svc_active; then
		warn "port 8099 is already in use; the feeder status page will not start (set ui.addr in $CONF)"
	fi
	if port_listening "$RESULTS_PORT" && ! svc_active; then
		warn "port $RESULTS_PORT is already in use; set results.beastPort in $CONF"
	fi
}

check_ntp() {
	local synced=''
	if have timedatectl; then
		synced=$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)
		if [ -z "$synced" ]; then
			if timedatectl status 2>/dev/null | grep -Eq 'synchronized: yes|NTP synchronized: yes'; then synced=yes; else synced=no; fi
		fi
	fi
	case $synced in
	yes) ok "system clock is NTP-synchronised" ;;
	'') info "timedatectl not available; cannot check NTP synchronisation" ;;
	*)
		warn "the system clock is not NTP-synchronised; MLAT needs correct host time"
		if have chronyd; then
			info "chrony is installed but not synchronised yet; check: chronyc tracking"
		elif [ "$INSTALL_CHRONY" = 1 ] || { can_prompt && ask_yesno "The clock is not NTP-synchronised. Install chrony (recommended)?" y; }; then
			run env DEBIAN_FRONTEND=noninteractive apt-get install -y chrony </dev/null || warn "chrony installation failed"
		else
			info "install it later with: sudo apt-get install chrony"
		fi
		;;
	esac
}

svc_active() {
	have systemctl && systemctl is-active --quiet "$SERVICE" 2>/dev/null
}

# detect_receiver prints what was found and sets DETECTED_INPUT.
detect_receiver() {
	say "Looking for receivers (nothing is changed)"
	DETECTED_INPUT=auto
	local found=0 p
	for p in readsb dump1090-fa dump1090 dump1090-mutability piaware; do
		if proc_running "$p"; then
			info "found running decoder: $p"
			found=1
		fi
	done
	if port_listening 30005; then
		ok "Beast output on 127.0.0.1:30005 (readsb/dump1090) - it will be reused as is"
		found=1
	fi
	if port_listening 10003 || proc_running rcd || [[ $(hostname 2>/dev/null) == radarcape* ]]; then
		ok "Radarcape on this host (:10003) - it will be used"
		found=1
	fi
	if usb_present 0bda:2838 || usb_present 0bda:2832; then
		if [ $found = 0 ]; then
			warn "an RTL-SDR stick is plugged in but no decoder is running."
			info "AirView does not bundle a decoder. readsb is a separate GPL program; to install it, run its"
			info "upstream installer yourself (read it first):"
			info "    https://github.com/wiedehopf/adsb-scripts/wiki/Automatic-installation-for-readsb"
			info "then re-run this installer; it will find readsb on 127.0.0.1:30005."
		else
			info "RTL-SDR stick present and a decoder is running"
		fi
	fi
	if usb_present 1d50:60a1; then
		info "Airspy found: install airspy_adsb (see https://airspy.com) feeding readsb, then use 127.0.0.1:30005"
	fi
	if usb_present 1df7; then
		info "SDRplay found: use SDRplay's dump1090/readsb build, then use 127.0.0.1:30005"
	fi
	if usb_present 0403:6001; then
		warn "FTDI serial device found (possibly a Mode-S Beast). airview-feeder reads network inputs only:"
		info "run a Beast-to-TCP bridge (for example beast-splitter or readsb --net-only with a serial input)"
		info "and point --input at its Beast TCP port."
	fi
	if [ $found = 0 ]; then
		info "no local receiver found; for a network receiver use --input HOST:PORT or radarcape:HOST"
	fi
}

# ------------------------------------------------------------- settings ----

# resolve_settings merges presets < saved state < flags, prompts in
# interactive mode, validates everything and fills the V_* variables.
resolve_settings() {
	local def
	V_SERVER=${F_SERVER:-${S_SERVER:-$PRESET_SERVER}}
	if [ "$SET_PIN" = 1 ]; then V_PIN=$F_PIN; else V_PIN=${S_PIN:-$PRESET_PIN}; fi
	if [ "$SET_TOKEN" = 1 ]; then V_TOKEN=$F_TOKEN; else V_TOKEN=$S_TOKEN; fi
	V_URL=${F_URL:-${S_URL:-$PRESET_URL}}
	V_MKEY=${F_MKEY:-${S_MKEY:-$PRESET_MANIFEST_KEY}}
	V_HTTP_PIN=${F_HTTP_PIN:-${S_HTTP_PIN:-$PRESET_HTTP_PIN}}
	def=$(hostname 2>/dev/null | tr -c 'A-Za-z0-9_-' '-' | cut -c1-32)
	def=${def%-}
	V_NAME=${F_NAME:-${S_NAME:-${def:-feeder}}}
	V_LAT=${F_LAT:-$S_LAT} V_LON=${F_LON:-$S_LON}
	V_ALT=${F_ALT:-$S_ALT} V_ALT_REF=${F_ALT_REF:-${S_ALT_REF:-msl}} V_AGL=${F_AGL:-${S_AGL:-10m}}
	V_LOCATED=${S_LOCATED:-}
	if [ "$F_NOLOC" = 1 ]; then
		V_LOCATED=0 V_LAT='' V_LON='' V_ALT=''
	elif [ -n "$F_LAT$F_LON" ]; then
		V_LOCATED=1
	fi
	V_INPUT=${F_INPUT:-${S_INPUT:-$DETECTED_INPUT}}
	V_PRIVACY=${F_PRIVACY:-${S_PRIVACY:-hidden}}
	if [ -n "$F_NOTS" ]; then
		if [ "$F_NOTS" = 1 ]; then V_TIMESTAMPS=0; else V_TIMESTAMPS=1; fi
	else
		V_TIMESTAMPS=${S_TIMESTAMPS:-1}
	fi
	V_READSB=${F_READSB:-${S_READSB:-0}}
	V_AUTOUPD=${F_AUTOUPD:-${S_AUTOUPD:-1}}
	V_CHANNEL=${F_CHANNEL:-${S_CHANNEL:-stable}}

	if can_prompt; then
		interactive_settings
	fi
	validate_settings
}

interactive_settings() {
	local ans
	prompt_valid V_SERVER "AirView aggregator address (host or host:port, AVF/1 port 30160)" "$V_SERVER" chk_server \
		"Enter a host name or IP address, optionally with :port."
	if [ -n "$V_TOKEN" ]; then
		ask_secret ans "Enrol token from the aggregator admin (leave empty to keep the saved token)"
		[ -z "$ans" ] || V_TOKEN=$ans
	else
		ask_secret ans "Enrol token from the aggregator admin (leave empty if the aggregator does not need one)"
		V_TOKEN=$ans
	fi
	while ! valid_token "$V_TOKEN"; do
		ask_secret V_TOKEN "That token contains characters a token cannot have. Enrol token"
	done
	prompt_valid V_NAME "Station name (letters, digits, - and _, at most 32)" "$V_NAME" valid_name \
		"Use 1-32 letters, digits, '-' or '_'."
	if ask_yesno "Enter the antenna position now?

MLAT needs it to about 10 m. Choose No to leave it blank and let the network locate the station." \
		"$([ "$V_LOCATED" = 0 ] && echo n || echo y)"; then
		V_LOCATED=1
		prompt_valid V_LAT "Antenna latitude (decimal degrees, north positive, e.g. 52.3731)" "$V_LAT" chk_lat \
			"Latitude must be a decimal number from -90 to 90."
		prompt_valid V_LON "Antenna longitude (decimal degrees, east positive, e.g. 4.8922)" "$V_LON" chk_lon \
			"Longitude must be a decimal number from -180 to 180."
		prompt_valid V_ALT "Antenna altitude with unit, e.g. 120m or 400ft" "$V_ALT" chk_alt \
			"Give a number followed by m or ft (from -500 m to 9000 m)."
		prompt_valid V_ALT_REF "Altitude reference: msl (mean sea level, usual) or hae (GPS ellipsoid height)" \
			"$V_ALT_REF" chk_altref "Type msl or hae."
		prompt_valid V_AGL "Antenna height above ground with unit, e.g. 8m or 25ft" "$V_AGL" chk_agl \
			"Give a number followed by m or ft (0 to 1000 m)."
	else
		V_LOCATED=0 V_LAT='' V_LON='' V_ALT=''
		show_text "The network will locate this station. $CONSENT_TEXT"
	fi
	prompt_valid V_INPUT "Receiver input: auto, HOST:PORT (Beast), avr:HOST:PORT, sbs:HOST:PORT or radarcape:HOST" \
		"$V_INPUT" chk_input "Examples: auto, 127.0.0.1:30005, radarcape:192.168.1.50"
	show_text "$CONSENT_PIN"
	if use_whiptail; then
		V_PRIVACY=$(whiptail --title "AirView feeder" --default-item "$V_PRIVACY" --menu \
			"Public map pin for this station (only the aggregator operator sees the exact position)" 14 74 3 \
			hidden "No pin on the public map (default)" \
			region "A pin somewhere in a 0.5 degree area" \
			cell01 "A pin somewhere in a 0.1 degree area" 3>&1 1>/dev/tty 2>&3 </dev/tty) || die "cancelled"
	else
		prompt_valid V_PRIVACY "Public map pin: hidden, region (0.5 degree area) or cell01 (0.1 degree area)" \
			"$V_PRIVACY" valid_privacy "Type hidden, region or cell01."
	fi
	if ask_yesno "Send receiver timestamps (needed for MLAT)?

$CONSENT_TEXT" "$([ "$V_TIMESTAMPS" = 0 ] && echo n || echo y)"; then
		V_TIMESTAMPS=1
	else
		V_TIMESTAMPS=0
	fi
}

validate_settings() {
	[ -n "$V_SERVER" ] || die "no aggregator given: use --server HOST[:PORT]"
	split_server "$V_SERVER" || die "invalid --server '$V_SERVER' (use host or host:port)"
	V_SERVER="$SRV_HOST:$SRV_PORT"
	valid_pin "$V_PIN" || die "invalid --pin (expected sha256/<44 base64 characters>)"
	valid_token "$V_TOKEN" || die "invalid --token (4-255 characters of A-Z a-z 0-9 . _ ~ + / = -)"
	valid_name "$V_NAME" || die "invalid --name '$V_NAME' (1-32 of A-Z a-z 0-9 _ -)"
	if [ "$V_LOCATED" = 1 ]; then
		[ -n "$V_LAT" ] && [ -n "$V_LON" ] || die "--lat and --lon are both needed (or use --no-location)"
		num_in "$V_LAT" -90 90 || die "invalid --lat '$V_LAT' (decimal degrees, -90..90)"
		num_in "$V_LON" -180 180 || die "invalid --lon '$V_LON' (decimal degrees, -180..180)"
		[ -n "$V_ALT" ] || die "--alt is needed with --lat/--lon (for example --alt 120m or --alt 400ft)"
		V_ALT_M=$(to_metres "$V_ALT") || die "invalid --alt '$V_ALT': a number with the unit m or ft, e.g. 120m"
		num_in "$V_ALT_M" -500 9000 || die "--alt $V_ALT is out of range (-500 m .. 9000 m)"
	else
		V_LOCATED=0 V_LAT='' V_LON='' V_ALT='' V_ALT_M=''
	fi
	chk_altref "$V_ALT_REF" || die "invalid --alt-ref '$V_ALT_REF' (msl or hae)"
	V_AGL_M=$(to_metres "$V_AGL") || die "invalid --agl '$V_AGL': a number with the unit m or ft, e.g. 8m"
	num_in "$V_AGL_M" 0 1000 || die "--agl $V_AGL is out of range (0..1000 m)"
	parse_input "$V_INPUT" || die "invalid --input '$V_INPUT' (auto, HOST:PORT, beast:/avr:/sbs:HOST:PORT, radarcape:HOST)"
	valid_privacy "$V_PRIVACY" || die "invalid --privacy '$V_PRIVACY' (hidden, region or cell01)"
	if [ -z "$V_URL" ]; then V_URL="https://${SRV_HOST}"; fi
	V_URL=${V_URL%/}
	valid_url "$V_URL" || die "invalid --url '$V_URL'"
	[ -z "$V_MKEY" ] || valid_b64key "$V_MKEY" || die "invalid --manifest-key (base64 of a 32-byte Ed25519 key)"
	valid_http_pin "$V_HTTP_PIN" || die "invalid --http-pin (sha256//<base64>)"
	if [ "$V_READSB" = 1 ] && ! port_listening 30104; then
		warn "--readsb-results: nothing listens on 127.0.0.1:30104 right now; the feeder will keep retrying"
	fi
	URL=$V_URL MKEY=$V_MKEY HTTP_PIN=$V_HTTP_PIN
}

# ---------------------------------------------------------- generators ----

gen_feeder_json() {
	local loc lat=0 lon=0 alt=0 sendloc=true ts=true autoupd=true readsb='""' upd_url='' upd_keys='[]'
	if [ "$V_LOCATED" = 1 ]; then loc=true lat=$V_LAT lon=$V_LON alt=$V_ALT_M; else loc=false; fi
	if [ -n "${URL:-}" ] && [ -n "${MKEY:-}" ]; then
		upd_url="${URL%/}/feeder/manifest.json"
		upd_keys="[$(json_str "$MKEY")]"
	fi
	[ "$V_TIMESTAMPS" = 1 ] || ts=false
	[ "$V_AUTOUPD" = 1 ] || autoupd=false
	[ "$V_READSB" = 1 ] && readsb='"127.0.0.1:30104"'
	cat <<EOF
{
  "version": 1,
  "mode": "feeder",
  "receivers": [
    {
      "rxNo": 1,
      "name": $(json_str "$V_NAME"),
      "input": {
        "id": "input-1",
        "name": $(json_str "$IN_LABEL"),
        "enabled": true,
        "type": $(json_str "$IN_TYPE"),
        "host": $(json_str "$IN_HOST"),
        "port": $IN_PORT
      },
      "located": $loc,
      "lat": $lat,
      "lon": $lon,
      "altM": $alt,
      "altRef": $(json_str "$V_ALT_REF"),
      "aglM": $V_AGL_M
    }
  ],
  "uplinks": [
    {
      "id": "$UPLINK_ID",
      "host": $(json_str "$SRV_HOST"),
      "port": $SRV_PORT,
      "serverPin": $(json_str "$V_PIN"),
      "enrolToken": $(json_str "$V_TOKEN"),
      "sendLocation": $sendloc,
      "timestamps": $ts,
      "returnMlat": true,
      "pin": $(json_str "$V_PRIVACY")
    }
  ],
  "ui": { "addr": "$UI_ADDR" },
  "results": { "beastPort": $RESULTS_PORT, "readsbAddr": $readsb },
  "update": { "manifestUrl": $(json_str "$upd_url"), "keys": $upd_keys, "channel": $(json_str "${V_CHANNEL:-stable}") },
  "autoUpdate": $autoupd
}
EOF
}

# merge_feeder_json OLD NEW_OUT: update only the installer-managed settings
# in an existing feeder.json (keeps other receivers, uplinks and settings).
# Needs python3; returns 1 when it is not available.
merge_feeder_json() {
	local py=${AVF_PYTHON-python3}
	[ -n "$py" ] && "$py" -c 'import json, sys' >/dev/null 2>&1 </dev/null || return 1
	AVF_NAME=$V_NAME AVF_LOCATED=$V_LOCATED AVF_LAT=${V_LAT:-0} AVF_LON=${V_LON:-0} AVF_ALT=${V_ALT_M:-0} \
		AVF_ALTREF=$V_ALT_REF AVF_AGL=$V_AGL_M AVF_INTYPE=$IN_TYPE AVF_INHOST=$IN_HOST AVF_INPORT=$IN_PORT \
		AVF_INLABEL=$IN_LABEL AVF_HOST=$SRV_HOST AVF_PORT=$SRV_PORT AVF_PIN=$V_PIN AVF_TOKEN=$V_TOKEN \
		AVF_TS=$V_TIMESTAMPS AVF_PRIV=$V_PRIVACY AVF_AUTOUPD=$V_AUTOUPD AVF_READSB=$V_READSB AVF_UPID=$UPLINK_ID \
		AVF_UPDURL=${URL:+${URL%/}/feeder/manifest.json} AVF_MKEY=${MKEY:-} AVF_CHANNEL=${V_CHANNEL:-stable} \
		"$py" - "$1" "$2" 2>/dev/null <<'PY'
import json, os, sys
e = os.environ
with open(sys.argv[1], encoding="utf-8") as f:
    cfg = json.load(f)
if not isinstance(cfg, dict):
    sys.exit(1)
cfg["mode"] = "feeder"
rxs = cfg.get("receivers")
if not isinstance(rxs, list) or not rxs:
    rxs = [{"rxNo": 1}]
rx = next((r for r in rxs if isinstance(r, dict) and r.get("rxNo") == 1), rxs[0])
loc = e["AVF_LOCATED"] == "1"
inp = rx.get("input") if isinstance(rx.get("input"), dict) else {}
inp.update({"id": inp.get("id") or "input-1", "name": e["AVF_INLABEL"], "enabled": True,
            "type": e["AVF_INTYPE"], "host": e["AVF_INHOST"], "port": int(e["AVF_INPORT"])})
rx.update({"rxNo": rx.get("rxNo", 1), "name": e["AVF_NAME"], "input": inp, "located": loc,
           "lat": float(e["AVF_LAT"]) if loc else 0, "lon": float(e["AVF_LON"]) if loc else 0,
           "altM": float(e["AVF_ALT"]) if loc else 0, "altRef": e["AVF_ALTREF"], "aglM": float(e["AVF_AGL"])})
cfg["receivers"] = rxs
ups = cfg.get("uplinks") if isinstance(cfg.get("uplinks"), list) else []
up = next((u for u in ups if isinstance(u, dict) and u.get("id") == e["AVF_UPID"]), None)
if up is None:
    up = {"id": e["AVF_UPID"], "returnMlat": True, "sendLocation": True}
    ups.append(up)
up.update({"host": e["AVF_HOST"], "port": int(e["AVF_PORT"]), "serverPin": e["AVF_PIN"],
           "enrolToken": e["AVF_TOKEN"], "timestamps": e["AVF_TS"] == "1", "pin": e["AVF_PRIV"],
           "disabled": False})
cfg["uplinks"] = ups
res = cfg.get("results") if isinstance(cfg.get("results"), dict) else {"beastPort": 30105}
res["readsbAddr"] = "127.0.0.1:30104" if e["AVF_READSB"] == "1" else ""
cfg["results"] = res
cfg["autoUpdate"] = e["AVF_AUTOUPD"] == "1"
if e.get("AVF_UPDURL") and e.get("AVF_MKEY"):
    upd = cfg.get("update") if isinstance(cfg.get("update"), dict) else {}
    keys = upd.get("keys") if isinstance(upd.get("keys"), list) else []
    if e["AVF_MKEY"] not in keys:
        keys.insert(0, e["AVF_MKEY"])
    upd.update({"manifestUrl": e["AVF_UPDURL"], "keys": keys})
    cfg["update"] = upd
if e.get("AVF_CHANNEL") in ("stable", "beta"):
    upd = cfg.get("update") if isinstance(cfg.get("update"), dict) else {}
    upd["channel"] = e["AVF_CHANNEL"]
    cfg["update"] = upd
with open(sys.argv[2], "w", encoding="utf-8", newline="\n") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
}

gen_service_unit() {
	cat <<EOF
# AirView Network feeder - generated by install.sh $INSTALLER_VERSION; re-running the
# installer rewrites this file. Local changes: systemctl edit $SERVICE (drop-in).
[Unit]
Description=AirView Network feeder (forwards receiver data to an AirView aggregator)
Documentation=http://$UI_ADDR/
After=network-online.target time-sync.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=$SVC_USER
Group=$SVC_USER
Environment=AIRVIEW_CONFIG=$CONF
Environment=AIRVIEW_DATA=$DATA
# Updates are installed by $SERVICE-update.timer (root), not by the service itself.
Environment=AIRVIEW_FEEDER_UPDATER=external
ExecStart=$BIN run
WorkingDirectory=$DATA
Restart=always
RestartSec=5
UMask=0027

# Hardening: no privileges, read-only system, writable state directory only.
NoNewPrivileges=true
AmbientCapabilities=
CapabilityBoundingSet=
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
ReadWritePaths=$DATA
MemoryMax=128M
TasksMax=128
LimitNOFILE=4096

[Install]
WantedBy=multi-user.target
EOF
}

gen_update_service() {
	cat <<EOF
# Generated by the AirView feeder installer $INSTALLER_VERSION.
[Unit]
Description=AirView Network feeder update check
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=$PREFIX/update.sh --quiet
Nice=10
IOSchedulingClass=idle
EOF
}

gen_update_timer() {
	cat <<EOF
# Generated by the AirView feeder installer $INSTALLER_VERSION.
[Unit]
Description=Daily AirView Network feeder update check

[Timer]
OnCalendar=daily
RandomizedDelaySec=6h
Persistent=true

[Install]
WantedBy=timers.target
EOF
}

# gen_wrapper ACTION: the small update.sh / uninstall.sh / status.sh scripts.
# They are identical to the files next to install.sh in the source tree.
gen_wrapper() {
	cat <<EOF
#!/usr/bin/env bash
# AirView Network feeder: $1 (thin wrapper; the logic is in install.sh next to this file).
# Usage: sudo $2 [options]   - see: install.sh --help
set -euo pipefail
here=\$(cd "\$(dirname "\${BASH_SOURCE[0]}")" && pwd)
exec bash "\$here/install.sh" --$1 "\$@"
EOF
}

# self_source prints this script (the file it was started from, or, when it
# was piped into bash, a regenerated copy of all its functions).
self_source() {
	if [ -n "${AVF_SELF:-}" ] && [ -f "$AVF_SELF" ] && grep -q '^avf_presets()' "$AVF_SELF" 2>/dev/null; then
		cat "$AVF_SELF"
		return
	fi
	printf '#!/usr/bin/env bash\n# AirView Network feeder installer %s (regenerated from a piped run; see README.md).\n' "$INSTALLER_VERSION"
	declare -f
	# shellcheck disable=SC2016 # literal text for the generated script
	printf 'AVF_SELF=${BASH_SOURCE[0]:-}\nmain "$@"\n'
}

# ------------------------------------------------------------- actions ----

ensure_user() {
	if id "$SVC_USER" >/dev/null 2>&1; then
		ok "user $SVC_USER exists"
		return
	fi
	local nologin=/usr/sbin/nologin
	[ -x "$nologin" ] || nologin=/sbin/nologin
	run useradd --system --user-group --home-dir "$DATA" --no-create-home --shell "$nologin" \
		--comment "AirView Network feeder" "$SVC_USER"
}

do_install() {
	prechecks
	detect_receiver
	load_state
	resolve_settings

	say "Settings"
	info "aggregator   : $V_SERVER${V_PIN:+ (pinned key)}"
	info "enrol token  : $([ -n "$V_TOKEN" ] && echo "set (hidden)" || echo none)"
	info "station name : $V_NAME"
	if [ "$V_LOCATED" = 1 ]; then
		info "position     : $V_LAT, $V_LON, ${V_ALT_M} m ${V_ALT_REF^^}, antenna ${V_AGL_M} m above ground"
	else
		info "position     : not given - the network will locate the station"
	fi
	info "input        : $IN_LABEL"
	info "public pin   : $V_PRIVACY"
	info "timestamps   : $([ "$V_TIMESTAMPS" = 1 ] && echo "yes (MLAT)" || echo "no (no MLAT)")"
	info "auto update  : $([ "$V_AUTOUPD" = 1 ] && echo daily || echo off) (channel $V_CHANNEL)"
	if [ "$V_TIMESTAMPS" = 1 ]; then info "note: $CONSENT_TEXT"; fi

	WORK=$(mktemp -d)
	trap 'rm -rf "$WORK"' EXIT
	local work=$WORK
	obtain_binary "$work/airview-feeder"

	say "Installing"
	ensure_user
	run install -d -m 0755 -o root -g root "$PREFIX"
	run install -d -m 0750 -o root -g "$SVC_USER" "$ETC"
	run install -d -m 0750 -o "$SVC_USER" -g "$SVC_USER" "$DATA"

	local was_active=0
	if svc_active; then
		was_active=1
	fi
	if [ -e "$(rpath "$BIN")" ] && [ "$DRY_RUN" = 0 ]; then
		cp -p "$BIN" "$BIN.old"
	fi
	put_copy "$work/airview-feeder" "$BIN" 0755 root:root
	install_notices

	self_source >"$work/install.sh"
	if [ "${AVF_SELF:-}" != "$PREFIX/install.sh" ]; then
		put_copy "$work/install.sh" "$PREFIX/install.sh" 0755 root:root
	fi
	gen_wrapper update "$PREFIX/update.sh" | put_file "$PREFIX/update.sh" 0755 root:root
	gen_wrapper uninstall "$PREFIX/uninstall.sh" | put_file "$PREFIX/uninstall.sh" 0755 root:root
	gen_wrapper status "$PREFIX/status.sh" | put_file "$PREFIX/status.sh" 0755 root:root
	run install -d -m 0755 "$(dirname "$SETUP_LINK")"
	run ln -sfn "$PREFIX/install.sh" "$SETUP_LINK"

	# Configuration: merge into an existing feeder.json (python3), otherwise
	# write a fresh one and keep a backup of the old file.
	local old
	old=$(rpath "$CONF")
	gen_feeder_json >"$work/feeder.json"
	if [ -f "$old" ]; then
		if merge_feeder_json "$old" "$work/merged.json"; then
			mv -f "$work/merged.json" "$work/feeder.json"
			ok "updated the installer-managed settings in $CONF (other settings kept)"
		elif ! cmp -s "$old" "$work/feeder.json"; then
			local bak
			bak="$CONF.bak-$(date +%Y%m%d%H%M%S)"
			run cp -p "$old" "$bak"
			warn "python3 is not available to merge settings: $CONF was rewritten, the old file is $bak"
		fi
	fi
	put_copy "$work/feeder.json" "$CONF" 0640 "root:$SVC_USER"

	S_SERVER=$V_SERVER S_PIN=$V_PIN S_TOKEN=$V_TOKEN S_URL=$V_URL S_MKEY=$V_MKEY S_HTTP_PIN=$V_HTTP_PIN
	S_NAME=$V_NAME S_LOCATED=$V_LOCATED S_LAT=$V_LAT S_LON=$V_LON S_ALT=$V_ALT S_ALT_REF=$V_ALT_REF
	S_AGL=$V_AGL S_INPUT=$V_INPUT S_PRIVACY=$V_PRIVACY S_TIMESTAMPS=$V_TIMESTAMPS S_READSB=$V_READSB
	S_AUTOUPD=$V_AUTOUPD S_TARGET=$TARGET S_CHANNEL=$V_CHANNEL
	save_state

	gen_service_unit | put_file "$UNIT_DIR/$SERVICE.service" 0644 root:root
	gen_update_service | put_file "$UNIT_DIR/$SERVICE-update.service" 0644 root:root
	gen_update_timer | put_file "$UNIT_DIR/$SERVICE-update.timer" 0644 root:root
	run systemctl daemon-reload
	if [ "$V_AUTOUPD" = 1 ]; then
		run systemctl enable --now "$SERVICE-update.timer"
	else
		run systemctl disable --now "$SERVICE-update.timer" || true
	fi
	run systemctl enable "$SERVICE"
	if [ "$NO_START" = 1 ]; then
		info "not starting the service (--no-start); start it with: sudo systemctl start $SERVICE"
	else
		if [ $was_active = 1 ]; then run systemctl restart "$SERVICE"; else run systemctl start "$SERVICE"; fi
		post_check
	fi
	if [ "$DRY_RUN" = 1 ] || svc_active; then
		rm -f "$(rpath "$BIN").old" 2>/dev/null || true
	elif [ -e "$BIN.old" ]; then
		info "the previous binary is kept as $BIN.old"
	fi
	finish_message
}

# feeder_cli SUBCOMMAND...: run the feeder binary as the service user.
feeder_cli() {
	local envs=(AIRVIEW_CONFIG="$CONF" AIRVIEW_DATA="$DATA" AIRVIEW_FEEDER_UPDATER=external)
	if have runuser; then
		timeout 30 runuser -u "$SVC_USER" -- env "${envs[@]}" "$BIN" "$@" </dev/null
	else
		timeout 30 env "${envs[@]}" "$BIN" "$@" </dev/null
	fi
}

post_check() {
	CONNECTED=0
	if [ "$DRY_RUN" = 1 ]; then
		printf '[dry-run] post-check: wait for the service and the aggregator session\n'
		return
	fi
	say "Checking the service"
	local i out
	for i in $(seq 1 30); do
		if ! systemctl is-active --quiet "$SERVICE"; then
			sleep 1
			continue
		fi
		out=$(feeder_cli status 2>/dev/null || true)
		if printf '%s' "$out" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"connected"|state[: =]+connected'; then
			CONNECTED=1
			break
		fi
		if printf '%s' "$out" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"stopped"'; then
			warn "the aggregator refused the session: $(printf '%s' "$out" | grep -Eo '"errorCode"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1)"
			info "enrol_required/enrol_invalid: re-run with --token; cloned_feeder: sudo -u $SVC_USER $BIN new-identity"
			break
		fi
		sleep 1
		: "$i"
	done
	if ! systemctl is-active --quiet "$SERVICE"; then
		warn "the service is not running. Check: journalctl -u $SERVICE -n 50"
	elif [ $CONNECTED = 1 ]; then
		ok "connected to the aggregator"
	else
		warn "the service runs but no aggregator session is established yet; check: $SETUP_LINK --status"
	fi
}

finish_message() {
	local me_url claim=''
	me_url="$V_URL/me"
	if [ "$DRY_RUN" = 0 ] && [ "${CONNECTED:-0}" = 1 ]; then
		claim=$(feeder_cli claim 2>/dev/null | sed -n '/[^[:space:]]/{p;q;}' || true)
	fi
	say "Done. AirView feeder is installed."
	info "Local status page : http://$UI_ADDR/  (on this machine; ssh -L 8099:127.0.0.1:8099 pi@<host>)"
	info "Your stations     : $me_url"
	if [ -n "$claim" ]; then
		info "Claim code        : $claim   (enter it on $me_url to link this station to your account)"
	else
		info "Claim code        : sudo $SETUP_LINK --claim   (once the feeder is connected)"
	fi
	info "Logs              : journalctl -u $SERVICE -f"
	info "Status            : sudo $SETUP_LINK --status"
	info "Update now        : sudo $PREFIX/update.sh   (check only: --check; channel: --channel beta)"
	info "Reconfigure       : sudo $SETUP_LINK   (current values are the defaults)"
	info "Uninstall         : sudo $PREFIX/uninstall.sh [--purge]"
	info "Configuration     : $CONF"
}

# set_conf_channel CHANNEL: write update.channel into feeder.json (python3;
# without it the channel applies to this run only and a warning says how
# to make it stick) and into the saved answers.
set_conf_channel() {
	local ch=$1 f py=${AVF_PYTHON-python3}
	S_CHANNEL=$ch
	if [ "${HAVE_STATE:-0}" = 1 ]; then save_state; fi
	f=$(rpath "$CONF")
	if [ ! -f "$f" ]; then
		warn "no $CONF: the channel '$ch' applies to this run only"
		return 0
	fi
	if [ -z "$py" ] || ! "$py" -c 'import json' >/dev/null 2>&1 </dev/null; then
		warn "python3 is not available: the channel '$ch' applies to this run only (re-run the installer with --channel $ch to keep it)"
		return 0
	fi
	if [ "$DRY_RUN" = 1 ]; then
		printf '[dry-run] set update.channel=%s in %s\n' "$ch" "$CONF"
	fi
	AVF_CHANNEL=$ch "$py" - "$f" "$f.tmp" 2>/dev/null <<'PY' || { warn "could not write the channel into $CONF"; return 0; }
import json, os, sys
with open(sys.argv[1], encoding="utf-8") as f:
    cfg = json.load(f)
upd = cfg.get("update") if isinstance(cfg.get("update"), dict) else {}
upd["channel"] = os.environ["AVF_CHANNEL"]
cfg["update"] = upd
with open(sys.argv[2], "w", encoding="utf-8", newline="\n") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
	if [ "$DRY_RUN" = 1 ]; then
		mv -f "$f.tmp" "$f"
	else
		chmod 0640 "$f.tmp" && chown "root:$SVC_USER" "$f.tmp" 2>/dev/null || true
		mv -f "$f.tmp" "$f"
	fi
	ok "release channel: $ch"
}

do_update() {
	load_state
	[ "${HAVE_STATE:-0}" = 1 ] || die "the feeder is not installed (no $STATE); run install.sh first"
	if [ "$(id -u)" != 0 ] && [ "$DRY_RUN" = 0 ]; then die "please run as root"; fi
	if [ "$FORCE" = 0 ] && [ "$QUIET" = 1 ] && grep -Eq '"autoUpdate"[[:space:]]*:[[:space:]]*false' "$(rpath "$CONF")" 2>/dev/null; then
		exit 0
	fi
	# --channel: store the release channel (feeder.json update.channel and
	# the saved answers) so the daily timer follows it too.
	local chan_args=()
	if [ -n "$F_CHANNEL" ]; then
		set_conf_channel "$F_CHANNEL"
		chan_args=(-channel "$F_CHANNEL")
	fi
	# --check: ask the feeder's updater only (signed manifest, nothing is
	# installed). Exit 10 = a newer version exists, 0 = up to date.
	if [ "$CHECK_ONLY" = 1 ]; then
		if [ "$DRY_RUN" = 1 ]; then
			printf '[dry-run] %s\n' "$(quote_cmd "$BIN" update -check ${chan_args[@]+"${chan_args[@]}"} -config "$CONF" -data "$DATA")"
			return 0
		fi
		[ -x "$BIN" ] || die "airview-feeder is not installed ($BIN)"
		local crc=0
		"$BIN" update -check ${chan_args[@]+"${chan_args[@]}"} -config "$CONF" -data "$DATA" </dev/null || crc=$?
		case $crc in
		0) ok "airview-feeder is up to date" ;;
		10) info "a newer airview-feeder is available; install it with: sudo $PREFIX/update.sh" ;;
		3) warn "updates are not set up (no update.manifestUrl / update.keys in $CONF); re-run the installer with --url and --manifest-key" ;;
		esac
		exit "$crc"
	fi
	# The feeder's own updater first (signed manifest from update.manifestUrl,
	# service restart, 60 s health check, automatic rollback). Exit 0 =
	# updated or up to date, 4 = rolled back; anything else (3/2 = not set
	# up, an older binary without the command) falls back to the download
	# below.
	if [ "$FORCE" = 0 ] && [ -z "$ARCH_OVERRIDE" ] && [ "$DRY_RUN" = 0 ] && [ -x "$BIN" ]; then
		local urc=0
		if [ "$QUIET" = 1 ]; then
			"$BIN" update -service "$SERVICE" ${chan_args[@]+"${chan_args[@]}"} -config "$CONF" -data "$DATA" </dev/null >/dev/null 2>&1 || urc=$?
		else
			"$BIN" update -service "$SERVICE" ${chan_args[@]+"${chan_args[@]}"} -config "$CONF" -data "$DATA" </dev/null || urc=$?
		fi
		case $urc in
		0) return 0 ;;
		4)
			warn "the new airview-feeder failed its health check and was rolled back"
			return 0
			;;
		esac
	fi
	URL=$S_URL MKEY=$S_MKEY HTTP_PIN=$S_HTTP_PIN
	if [ -n "$ARCH_OVERRIDE" ] || [ -z "$S_TARGET" ]; then detect_target; else TARGET=$S_TARGET; fi
	local work cur
	WORK=$(mktemp -d)
	trap 'rm -rf "$WORK"' EXIT
	work=$WORK
	obtain_binary "$work/airview-feeder"
	cur=''
	if [ -f "$(rpath "$BIN")" ]; then cur=$(sha256_of "$(rpath "$BIN")"); fi
	if [ "$cur" = "$BIN_SHA" ] && [ "$FORCE" = 0 ]; then
		say "airview-feeder is up to date${BIN_VERSION:+ ($BIN_VERSION)}"
		return 0
	fi
	say "Installing airview-feeder ${BIN_VERSION:-update}"
	run cp -p "$BIN" "$BIN.old"
	put_copy "$work/airview-feeder" "$BIN" 0755 root:root
	run systemctl restart "$SERVICE"
	if [ "$DRY_RUN" = 1 ]; then
		printf '[dry-run] health check for 60 s; roll back to %s.old on failure\n' "$BIN"
		return 0
	fi
	# Health check: the service must stay active without restarts for 60 s.
	local i healthy=1 restarts restarts_before
	restarts_before=$(systemctl show -p NRestarts --value "$SERVICE" 2>/dev/null || echo 0)
	for i in $(seq 1 12); do
		sleep 5
		restarts=$(systemctl show -p NRestarts --value "$SERVICE" 2>/dev/null || echo 0)
		if ! systemctl is-active --quiet "$SERVICE" || [ "$restarts" != "$restarts_before" ]; then
			healthy=0
			break
		fi
		: "$i"
	done
	if [ $healthy = 1 ]; then
		rm -f "$BIN.old"
		ok "updated and healthy"
	else
		warn "the new version failed its health check; rolling back"
		mv -f "$BIN.old" "$BIN"
		systemctl restart "$SERVICE"
		die "update rolled back. Logs: journalctl -u $SERVICE -n 100"
	fi
}

do_uninstall() {
	if [ "$(id -u)" != 0 ] && [ "$DRY_RUN" = 0 ]; then die "please run as root"; fi
	say "Uninstalling the AirView feeder"
	info "readsb, dump1090, Radarcape and other feeders are not touched."
	if [ "$PURGE" = 1 ]; then
		info "--purge: the configuration, the feeder identity (identity.key) and all state are deleted too."
	else
		info "The configuration ($ETC) and identity ($DATA) are kept; use --purge to delete them."
	fi
	if can_prompt && ! ask_yesno "Remove the AirView feeder$([ "$PURGE" = 1 ] && echo " and all its data")?" n; then
		die "cancelled"
	fi
	local u
	for u in "$SERVICE-update.timer" "$SERVICE-update.service" "$SERVICE.service"; do
		if have systemctl || [ "$DRY_RUN" = 1 ]; then run systemctl disable --now "$u" 2>/dev/null || true; fi
		run rm -f "$UNIT_DIR/$u"
	done
	run rm -rf "$UNIT_DIR/$SERVICE.service.d"
	if have systemctl || [ "$DRY_RUN" = 1 ]; then run systemctl daemon-reload || true; fi
	run rm -f "$SETUP_LINK"
	run rm -rf "$PREFIX"
	if [ "$PURGE" = 1 ]; then
		run rm -rf "$ETC" "$DATA"
		if id "$SVC_USER" >/dev/null 2>&1 || [ "$DRY_RUN" = 1 ]; then run userdel "$SVC_USER" || true; fi
		ok "removed, including configuration, identity and the $SVC_USER user"
	else
		ok "removed; kept $ETC and $DATA (re-installing keeps the same feeder identity)"
	fi
}

do_status() {
	load_state
	printf 'AirView Network feeder - installer %s\n\n' "$INSTALLER_VERSION"
	if [ "${HAVE_STATE:-0}" != 1 ] && [ ! -e "$(rpath "$BIN")" ]; then
		printf 'Not installed (no %s).\n' "$BIN"
		return 1
	fi
	printf 'Aggregator   : %s%s\n' "${S_SERVER:-?}" "$([ -n "$S_PIN" ] && echo " (pinned key)")"
	printf 'Station      : %s\n' "${S_NAME:-?}"
	if [ "$S_LOCATED" = 1 ]; then
		printf 'Position     : %s, %s, %s %s\n' "$S_LAT" "$S_LON" "$S_ALT" "${S_ALT_REF^^}"
	else
		printf 'Position     : not given (located by the network)\n'
	fi
	printf 'Input        : %s\n' "${S_INPUT:-auto}"
	printf 'Public pin   : %s\n' "${S_PRIVACY:-hidden}"
	printf 'Timestamps   : %s\n' "$([ "${S_TIMESTAMPS:-1}" = 1 ] && echo yes || echo no)"
	printf 'Build        : %s\n' "${S_TARGET:-?}"
	if [ -x "$BIN" ] && [ "$DRY_RUN" = 0 ]; then
		printf 'Version      : %s\n' "$("$BIN" --version 2>/dev/null </dev/null | head -1 || echo '?')"
	fi
	if have systemctl; then
		printf 'Service      : %s (%s)\n' "$(systemctl is-active "$SERVICE" 2>/dev/null || true)" \
			"$(systemctl is-enabled "$SERVICE" 2>/dev/null || true)"
		printf 'Restarts     : %s\n' "$(systemctl show -p NRestarts --value "$SERVICE" 2>/dev/null || echo '?')"
		printf 'Update timer : %s\n' "$(systemctl is-active "$SERVICE-update.timer" 2>/dev/null || true)"
	fi
	printf 'Channel      : %s\n' "${S_CHANNEL:-stable}"
	if [ -x "$BIN" ] && [ "$DRY_RUN" = 0 ]; then
		printf 'Updates      : %s\n' "$("$BIN" update -status -config "$CONF" -data "$DATA" 2>/dev/null </dev/null | head -3 | tr '\n' ' ' || echo '?')"
	fi
	printf 'Status page  : http://%s/\n' "$UI_ADDR"
	if [ "$DRY_RUN" = 0 ] && [ -x "$BIN" ] && svc_active; then
		printf '\nLink state (airview-feeder status):\n'
		feeder_cli status 2>&1 | sed 's/^/  /' || printf '  (not available)\n'
	fi
	printf '\nLogs: journalctl -u %s -f\n' "$SERVICE"
}

do_claim() {
	if [ "$(id -u)" != 0 ] && [ "$DRY_RUN" = 0 ]; then die "please run as root"; fi
	if [ "$DRY_RUN" = 1 ]; then
		printf '[dry-run] %s\n' "$(quote_cmd runuser -u "$SVC_USER" -- "$BIN" claim)"
		return 0
	fi
	svc_active || die "the feeder is not running (systemctl start $SERVICE)"
	feeder_cli claim || die "could not get a claim code; is the feeder connected? ($SETUP_LINK --status)"
}

main() {
	set -Eeuo pipefail
	umask 022
	avf_presets
	avf_constants
	avf_init_output
	WORK='' HAVE_STATE=0 TARGET='' URL='' MKEY='' HTTP_PIN='' BIN_SHA='' BIN_VERSION='' CONNECTED=0
	DETECTED_INPUT=auto
	parse_args "$@"
	if [ "$DRY_RUN" = 1 ]; then
		if [ -z "$DRY_ROOT" ]; then DRY_ROOT=$(mktemp -d); fi
		mkdir -p "$DRY_ROOT"
		DRY_ROOT=$(cd "$DRY_ROOT" && pwd)
		printf '[dry-run] nothing on this system is changed; files are staged under %s\n' "$DRY_ROOT"
	elif [ -n "$DRY_ROOT" ]; then
		die "--dry-run-root needs --dry-run"
	fi
	if [ "$DRY_RUN" = 0 ] && [ "$ACTION" != status ] && have flock; then
		exec 9>/run/airview-feeder-setup.lock
		flock -n 9 || die "another airview-feeder installer is running"
	fi
	case $ACTION in
	install) do_install ;;
	update) do_update ;;
	uninstall) do_uninstall ;;
	status) do_status ;;
	claim) do_claim ;;
	esac
}

AVF_SELF=${BASH_SOURCE[0]:-}
main "$@"
exit $?
