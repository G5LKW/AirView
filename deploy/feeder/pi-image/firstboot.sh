#!/bin/sh
# AirView Raspberry Pi image: first-boot provisioning.
#
# Started once by cloud-init on the first boot of an AirView image, through
# the "vendor-data" script on the boot partition (cloud-init's scripts_vendor
# module, final stage: after Raspberry Pi Imager's user, SSH, Wi-Fi and host
# name settings from user-data/network-config were applied). Raspberry Pi
# Imager rewrites user-data, meta-data and network-config but not vendor-data,
# so this works with and without Imager customisation.
#
# It reads /boot/firmware/airview/image.conf (KEY=VALUE, never executed),
# verifies the payload against SHA256SUMS, copies it to /opt/airview-image and:
#   station: runs the standard AirView installer (deploy/pi/install.sh):
#            user "airview", /opt/airview/airview, airview.service on port 80;
#   feeder:  installs and starts airview-pisetup.service, the setup page on
#            port 8099, which runs the standard feeder installer
#            (deploy/feeder/pi/install.sh) once the owner filled in the form.
# The host name is set to the image default (airview / airview-feeder) only
# while it is still the Raspberry Pi OS default "raspberrypi"; a name chosen in
# Raspberry Pi Imager is kept. SSH is left exactly as Imager configured it.
#
# Safe to run again by hand (sudo sh /boot/firmware/airview/firstboot.sh): it
# stops at once when /var/lib/airview-image/firstboot.done exists, and the
# payload stays on the boot partition until provisioning succeeded.
# Log: /var/log/airview-firstboot.log.
#
# Test hooks: AVI_ROOT prefixes every path, AVI_DRYRUN=1 prints system
# commands instead of running them (deploy/feeder/pi-image/piimage_test.go).
set -eu
umask 022
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH

R=${AVI_ROOT:-}
DRY=${AVI_DRYRUN:-0}
BOOT=$R/boot/firmware
[ -d "$BOOT/airview" ] || BOOT=$R/boot
SRC=$BOOT/airview
DEST=$R/opt/airview-image
STATE=$R/var/lib/airview-image
LOG=$R/var/log/airview-firstboot.log
UNITS=$R/etc/systemd/system

log() { printf '%s airview-firstboot: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() {
	log "ERROR: $*"
	log "The payload stays in $SRC; fix the problem and run: sudo sh $SRC/firstboot.sh"
	console "AirView first-boot setup FAILED: $* (see $LOG)"
	exit 1
}
run() {
	log "+ $*"
	[ "$DRY" = 1 ] && return 0
	"$@"
}
console() {
	[ "$DRY" = 1 ] && return 0
	for t in /dev/console /dev/tty1; do
		[ -w "$t" ] && printf '\n*** %s ***\n\n' "$*" >"$t" 2>/dev/null || true
	done
}

# conf KEY DEFAULT: the last KEY=VALUE line of image.conf (CR stripped).
conf() {
	v=$(sed -n "s/^$1=//p" "$SRC/image.conf" 2>/dev/null | tr -d '\r' | tail -n 1)
	printf '%s' "${v:-$2}"
}

set_hostname() {
	want=$1
	[ -n "$want" ] || return 0
	case $want in
	*[!a-z0-9-]* | -* | *-) die "invalid AIRVIEW_HOSTNAME '$want' in image.conf" ;;
	esac
	[ ${#want} -le 63 ] || die "AIRVIEW_HOSTNAME is longer than 63 characters"
	cur=$(tr -d ' \t\r\n' <"$R/etc/hostname" 2>/dev/null || true)
	case $cur in
	"" | raspberrypi | localhost) ;;
	*)
		log "keeping the host name '$cur' (chosen in Raspberry Pi Imager or by the user)"
		HOST=$cur
		return 0
		;;
	esac
	log "setting the host name to '$want'"
	printf '%s\n' "$want" >"$R/etc/hostname"
	if [ -f "$R/etc/hosts" ] && grep -q '^127\.0\.1\.1[[:space:]]' "$R/etc/hosts"; then
		sed -i "s/^127\.0\.1\.1[[:space:]].*/127.0.1.1\t$want/" "$R/etc/hosts"
	else
		printf '127.0.1.1\t%s\n' "$want" >>"$R/etc/hosts"
	fi
	# cloud-init would otherwise re-apply its idea of the host name on later boots.
	if [ -d "$R/etc/cloud/cloud.cfg.d" ]; then
		printf '%s\n' "# Written by the AirView image first boot (firstboot.sh)." "preserve_hostname: true" \
			>"$R/etc/cloud/cloud.cfg.d/90-airview-hostname.cfg"
	fi
	if command -v hostnamectl >/dev/null 2>&1; then
		run hostnamectl set-hostname "$want" || run hostname "$want"
	else
		run hostname "$want"
	fi
	# Announce the new name over mDNS (airview.local / airview-feeder.local).
	run systemctl try-restart --no-block avahi-daemon.service || true
	HOST=$want
}

# stage_payload verifies SHA256SUMS on the boot partition and copies every
# listed file to $DEST.
stage_payload() {
	[ -f "$SRC/SHA256SUMS" ] || die "$SRC/SHA256SUMS is missing"
	log "verifying the payload"
	(cd "$SRC" && tr -d '\r' <SHA256SUMS | sha256sum -c --strict --quiet -) ||
		die "the files on the SD card are damaged (SHA-256 mismatch); write the image again"
	mkdir -p "$DEST" "$STATE"
	chmod 0755 "$DEST"
	chmod 0700 "$STATE"
	# A plain file, not a pipe: die must leave this shell, not a subshell.
	tr -d '\r' <"$SRC/SHA256SUMS" >"$STATE/payload.list"
	while read -r _ name; do
		name=${name#\*}
		case $name in */* | "" | .*) die "unexpected payload name '$name'" ;; esac
		cp "$SRC/$name" "$DEST/$name.tmp" || die "copying $name failed"
		case $name in
		*.sh | *-arm64 | airview-pisetup) chmod 0755 "$DEST/$name.tmp" ;;
		*) chmod 0644 "$DEST/$name.tmp" ;;
		esac
		mv -f "$DEST/$name.tmp" "$DEST/$name" || die "copying $name failed"
	done <"$STATE/payload.list"
	log "payload copied to $DEST"
}

install_station() {
	[ -f "$DEST/install.sh" ] && [ -f "$DEST/airview-linux-arm64" ] && [ -f "$DEST/airview.service" ] ||
		die "the station payload is incomplete"
	if [ -f "$SRC/airview.jsonc" ] && [ ! -f "$R/etc/airview/airview.jsonc" ]; then
		log "using the operator configuration airview.jsonc from the SD card"
		mkdir -p "$R/etc/airview"
		tr -d '\r' <"$SRC/airview.jsonc" >"$R/etc/airview/airview.jsonc"
		chmod 0640 "$R/etc/airview/airview.jsonc"
	fi
	run sh "$DEST/install.sh" || die "the AirView installer failed"
	URL="http://$HOST.local/"
}

install_feeder() {
	for f in airview-feeder-linux-arm64 install-feeder.sh airview-pisetup airview-pisetup.service; do
		[ -f "$DEST/$f" ] || die "the feeder payload is incomplete ($f missing)"
	done
	if [ -f "$SRC/feeder-setup.txt" ] && grep -q '^[[:space:]]*SERVER[[:space:]]*=[[:space:]]*[^[:space:]]' "$SRC/feeder-setup.txt"; then
		log "feeder-setup.txt has an aggregator: the feeder will be configured without the setup page"
	fi
	if [ -f "$SRC/feeder-setup.txt" ]; then
		cp "$SRC/feeder-setup.txt" "$STATE/feeder-setup.txt"
		chmod 0600 "$STATE/feeder-setup.txt"
	fi
	mkdir -p "$UNITS"
	cp "$DEST/airview-pisetup.service" "$UNITS/airview-pisetup.service"
	chmod 0644 "$UNITS/airview-pisetup.service"
	run systemctl daemon-reload
	run systemctl enable airview-pisetup.service
	# --no-block: we run inside cloud-init's final stage.
	run systemctl start --no-block airview-pisetup.service
	URL="http://$HOST.local:8099/"
}

# clean_boot removes the binaries and any secrets (enrol token, passwords)
# from the boot partition, which every local user can read.
clean_boot() {
	while read -r _ name; do
		name=${name#\*}
		case $name in *-arm64 | airview-pisetup | install*.sh | *.service) rm -f "$SRC/$name" ;; esac
	done <"$STATE/payload.list"
	rm -f "$SRC/SHA256SUMS" "$SRC/feeder-setup.txt" "$SRC/airview.jsonc"
	printf '%s\n' "AirView was installed from this SD card on $(date -u +%Y-%m-%d) (log: $LOG)." \
		"The installation files were removed from this partition; the feeder/station is in /opt." \
		>"$SRC/INSTALLED.txt"
	sync
}

main() {
	if [ "$DRY" != 1 ] && [ "$(id -u)" != 0 ]; then
		echo "airview-firstboot: run as root" >&2
		exit 1
	fi
	mkdir -p "$(dirname "$LOG")"
	if [ "$DRY" != 1 ]; then
		exec >>"$LOG" 2>&1
		chmod 0640 "$LOG" 2>/dev/null || true
	fi
	if [ -f "$STATE/firstboot.done" ]; then
		log "already provisioned ($(cat "$STATE/firstboot.done")); nothing to do"
		exit 0
	fi
	[ -f "$SRC/image.conf" ] || die "$SRC/image.conf not found: this is not an AirView image"
	KIND=$(conf AIRVIEW_IMAGE_KIND "")
	VERSION=$(conf AIRVIEW_IMAGE_VERSION unknown)
	log "AirView $KIND image $VERSION: first-boot provisioning starts"
	HOST=$(tr -d ' \t\r\n' <"$R/etc/hostname" 2>/dev/null || echo raspberrypi)
	set_hostname "$(conf AIRVIEW_HOSTNAME "")"
	stage_payload
	case $KIND in
	station) install_station ;;
	feeder) install_feeder ;;
	*) die "unknown AIRVIEW_IMAGE_KIND '$KIND' in image.conf" ;;
	esac
	clean_boot
	mkdir -p "$STATE"
	printf '%s %s %s\n' "$KIND" "$VERSION" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$STATE/firstboot.done"
	log "done: open $URL"
	console "AirView is ready: open $URL"
}

main "$@"
