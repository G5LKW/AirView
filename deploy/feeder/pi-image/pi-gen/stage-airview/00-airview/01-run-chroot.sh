#!/bin/bash -e
# Runs inside the image: the first-boot mechanism needs cloud-init (NoCloud
# seed on the boot partition), avahi for <host>.local and sha256sum.
for c in cloud-init sha256sum avahi-daemon; do
	command -v "$c" >/dev/null 2>&1 || { echo "pi-gen image lacks $c; the AirView first boot needs it" >&2; exit 1; }
done
