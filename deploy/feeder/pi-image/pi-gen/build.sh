#!/bin/bash
# AirView Raspberry Pi images with pi-gen (maintainers, Linux only).
#
# The Windows route (cmd/airview-piimage build) is the normal way to make the
# images. This is the alternative for maintainers who build Raspberry Pi OS
# themselves with pi-gen (https://github.com/RPi-Distro/pi-gen, branch for
# arm64 Trixie; fetch it yourself). Both routes put the same payload on the
# boot partition, so the first boot behaves identically.
#
#   build.sh station|feeder PI_GEN_DIR PAYLOAD_DIR [VERSION]
#
# PAYLOAD_DIR comes from:  airview-piimage payload -kind station|feeder -out DIR ...
# (it holds vendor-data and the airview/ directory). The script writes
# PI_GEN_DIR/config.airview, links stage-airview into PI_GEN_DIR, marks stage2
# SKIP_IMAGES and runs pi-gen's build-docker.sh (or build.sh with --no-docker).
set -euo pipefail

usage() { echo "usage: $0 station|feeder PI_GEN_DIR PAYLOAD_DIR [VERSION] [--no-docker]" >&2; exit 2; }
[ $# -ge 3 ] || usage
KIND=$1 PIGEN=$(cd "$2" && pwd) PAYLOAD=$(cd "$3" && pwd) VERSION=${4:-dev}
DOCKER=1
for a in "$@"; do [ "$a" = --no-docker ] && DOCKER=0; done
case $KIND in
station) TITLE=Station HOST=airview ;;
feeder) TITLE=Feeder HOST=airview-feeder ;;
*) usage ;;
esac
HERE=$(cd "$(dirname "$0")" && pwd)

[ -x "$PIGEN/build.sh" ] || { echo "$PIGEN is not a pi-gen checkout" >&2; exit 1; }
[ -f "$PAYLOAD/vendor-data" ] && [ -f "$PAYLOAD/airview/image.conf" ] && [ -f "$PAYLOAD/airview/SHA256SUMS" ] ||
	{ echo "$PAYLOAD is not an airview-piimage payload directory" >&2; exit 1; }
grep -q "^AIRVIEW_IMAGE_KIND=$KIND\$" "$PAYLOAD/airview/image.conf" ||
	{ echo "$PAYLOAD was made for another image kind" >&2; exit 1; }
(cd "$PAYLOAD/airview" && sha256sum -c --quiet SHA256SUMS)

rm -rf "$PIGEN/stage-airview"
cp -a "$HERE/stage-airview" "$PIGEN/stage-airview"
mkdir -p "$PIGEN/stage-airview/00-airview/files"
cp -a "$PAYLOAD/." "$PIGEN/stage-airview/00-airview/files/"
chmod +x "$PIGEN/stage-airview/prerun.sh" "$PIGEN/stage-airview/00-airview/"*.sh
printf 'IMG_SUFFIX=""\n' >"$PIGEN/stage-airview/EXPORT_IMAGE"
touch "$PIGEN/stage2/SKIP_IMAGES"

cat >"$PIGEN/config.airview" <<CONF
IMG_NAME="AirView-$TITLE-Pi-$VERSION"
ARCH=arm64
TARGET_HOSTNAME=$HOST
STAGE_LIST="stage0 stage1 stage2 stage-airview"
DEPLOY_COMPRESSION=zip
CONF
echo "==> pi-gen config: $PIGEN/config.airview"
cd "$PIGEN"
if [ "$DOCKER" = 1 ]; then
	./build-docker.sh -c config.airview
else
	sudo ./build.sh -c config.airview
fi
echo "==> images: $PIGEN/deploy/"
