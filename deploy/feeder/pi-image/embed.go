// Package piimage holds the files that cmd/airview-piimage writes onto the
// boot partition of an AirView Raspberry Pi image, and the maintainers'
// pi-gen alternative (pi-gen/). See README.md.
//
// Boot partition layout written by the image builder:
//
//	vendor-data                  cloud-init vendor script: exec airview/firstboot.sh
//	airview/firstboot.sh         first-boot provisioning (this directory)
//	airview/image.conf           KEY=VALUE: kind, version, default host name
//	airview/README.txt           boot-readme.txt rendered for the image
//	airview/SHA256SUMS           checksums of the payload, verified on the Pi
//	station: airview-linux-arm64, airview.service, install.sh (deploy/pi)
//	feeder:  airview-feeder-linux-arm64, install-feeder.sh (deploy/feeder/pi),
//	         airview-pisetup (setup page), airview-pisetup.service,
//	         feeder-setup.txt (optional unattended answers)
package piimage

import "embed"

// Files are the scripts and templates of the image.
//
//go:embed firstboot.sh vendor-data airview-pisetup.service feeder-setup.txt boot-readme.txt
var Files embed.FS
