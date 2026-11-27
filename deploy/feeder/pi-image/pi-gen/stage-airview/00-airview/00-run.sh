#!/bin/bash -e
# Runs on the build host: put the AirView payload on the boot partition,
# exactly where cmd/airview-piimage puts it (vendor-data + airview/).
BOOT="${ROOTFS_DIR}/boot/firmware"
[ -d "$BOOT" ] || BOOT="${ROOTFS_DIR}/boot"
install -m 0644 files/vendor-data "$BOOT/vendor-data"
rm -rf "$BOOT/airview"
mkdir -p "$BOOT/airview"
cp -a files/airview/. "$BOOT/airview/"
echo "AirView payload: $(ls "$BOOT/airview" | tr '\n' ' ')"
