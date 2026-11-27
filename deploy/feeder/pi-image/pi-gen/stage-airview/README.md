# stage-airview (pi-gen)

Custom pi-gen stage used by `../build.sh`. It runs after `stage2` (Raspberry Pi OS Lite) and only adds
`vendor-data` and the `airview/` payload to the boot partition, the same files `cmd/airview-piimage`
writes on Windows. `01-run-chroot.sh` checks that the image has cloud-init, avahi and sha256sum.
