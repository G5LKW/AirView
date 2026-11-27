# AirView Raspberry Pi SD-card images

Two ready-to-flash images, built on Windows (or any OS) from the official **Raspberry Pi OS Lite (64-bit)**
release without Linux tools:

| Image | What runs after the first boot | Open |
|---|---|---|
| `AirView-Station-Pi-<ver>.img.zip` | The full AirView station/aggregator web app (`deploy/pi/install.sh`: user `airview`, `/opt/airview/airview`, `airview.service`, port 80) | `http://airview.local/` |
| `AirView-Feeder-Pi-<ver>.img.zip` | A setup page on port 8099 that installs `airview-feeder` with the standard feeder installer (`deploy/feeder/pi/install.sh`: user `airview-feeder`, `/opt/airview-feeder`, `airview-feeder.service`) | `http://airview-feeder.local:8099/` |

Supported boards: every 64-bit Raspberry Pi that runs Raspberry Pi OS Trixie (Pi 3, 4, 5, 400, 500, Zero 2 W,
CM3/4/5). Each image is the Raspberry Pi OS release byte for byte plus a few files on the boot (FAT)
partition; the partition table, the partition sizes and the root file system are unchanged
(`<name>.verify.txt` records the proof).

## Flashing with Raspberry Pi Imager

1. Raspberry Pi Imager → **Choose Device** → your Pi.
2. **Choose OS** → scroll down → **Use custom** → pick `AirView-Station-Pi-<ver>.img.zip` or
   `AirView-Feeder-Pi-<ver>.img.zip` (Imager reads the .zip directly; no need to unpack).
3. **Choose Storage** → the SD card (or USB/NVMe drive).
4. When Imager offers **OS customisation**, use it as for any Raspberry Pi OS image: host name, user name
   and password, Wi-Fi (SSID, password, country), time zone/keyboard, and on the *Services* tab **Enable
   SSH** with a password or your public key. All of it keeps working: Imager writes these settings to
   `user-data`/`network-config` (or, older Imager versions, `firstrun.sh`), which Raspberry Pi OS applies
   before AirView's own first-boot step runs, and Imager never touches AirView's `vendor-data`.
   - Leave the host name empty/default to get `airview` / `airview-feeder`. A name you set is kept
     (then use `http://<your-name>.local/`).
   - Without customisation there is no login user and no SSH; the AirView web pages still work over
     Ethernet. Use customisation if you want Wi-Fi or a shell.
5. Write, put the card in the Pi, power on. The first start takes a few minutes (Raspberry Pi OS grows
   the root partition, then cloud-init applies your settings, then AirView installs itself).

Other writers (balenaEtcher, `dd`) work as well, without the customisation step.

The `.sha256` next to each `.img.zip` is the SHA-256 of the zip (`Get-FileHash -Algorithm SHA256 <file>` on
Windows, `sha256sum -c <file>.sha256` on Linux).

## First boot, step by step

1. Raspberry Pi OS boots; cloud-init (NoCloud seed on the boot partition) applies `user-data` and
   `network-config` from Imager.
2. In its final stage cloud-init runs **`vendor-data`**, a two-line shell script that runs
   **`/boot/firmware/airview/firstboot.sh`** (log: `/var/log/airview-firstboot.log`):
   - stops at once if `/var/lib/airview-image/firstboot.done` exists (runs once);
   - sets the host name to `airview`/`airview-feeder` **only** while it is still `raspberrypi`, updates
     `/etc/hosts`, tells cloud-init to preserve it and restarts avahi so `<name>.local` resolves;
   - verifies the payload against `airview/SHA256SUMS` (a damaged card stops here with a console message);
   - copies the payload to `/opt/airview-image`;
   - **station:** copies an optional `airview/airview.jsonc` to `/etc/airview/airview.jsonc` (operator
     file, only if none exists) and runs the standard installer `install.sh`;
   - **feeder:** installs and starts `airview-pisetup.service` (the setup page) and hands over an
     optional `airview/feeder-setup.txt` (root-only copy in `/var/lib/airview-image`);
   - removes the binaries, `feeder-setup.txt` and `airview.jsonc` from the boot partition (every local
     user can read it) and writes `airview/INSTALLED.txt`;
   - prints the URL on the console.
3. SSH is not touched: it is exactly as Imager configured it.

If something fails, the payload stays on the card; log in, read the log, fix the cause and run
`sudo sh /boot/firmware/airview/firstboot.sh` again.

### The feeder setup page (port 8099)

`airview-pisetup` (cmd/airview-piimage/pisetup) serves a one-page form on port 8099 until the feeder is
configured: aggregator address, server key and enrol token, station name, receiver input (`auto` finds
readsb/dump1090 on port 30005 or a Radarcape), antenna position (or "let the network locate me"), the
timestamp choice with consent text C1, the public pin choice with text C2 (docs/aggregator/CONSENT.md,
word for word) and a status-page password. On submit it runs `install-feeder.sh --binary
… --non-interactive --no-start`, sets `ui.addr` to `0.0.0.0:8099` with the password's hash
(`airview-feeder config set`), disables itself and starts `airview-feeder.service`, whose status page
takes over port 8099 (open it by IP address; it refuses host names to defeat DNS rebinding, see HOOKS.md).

Security of the setup page while it exists: only clients on loopback/private/link-local/CGNAT addresses;
Host header must be an IP, `localhost` or the Pi's own name (optionally `.local`, `.lan`, `.home`,
`.home.arpa`, `.localdomain`, `.internal`); a per-boot form token and same-origin check against CSRF;
strict CSP; one setup at a time. Anyone on your LAN could fill in the form first: if that matters, use
the unattended file instead.

**Unattended feeder setup:** before the first boot, open the SD card's boot partition on your PC, edit
`airview/feeder-setup.txt` (commented template; at least `SERVER=`), save. The feeder is then configured
without the page; the file is deleted after use. Errors fall back to the page with the values filled in.

The feeder image does not contain a decoder (readsb, dump1090 are GPL programs and are not bundled);
install one yourself or point `INPUT=` at a receiver on the network.

## Building the images (Windows, no Linux needed)

```powershell
# 1. Base image: the official Raspberry Pi OS Lite (64-bit) release and its published checksum
#    https://downloads.raspberrypi.com/raspios_lite_arm64_latest  (+ ".sha256" of the resolved file)
# 2. Build (from the repository root):
powershell -File deploy\feeder\pi-image\build.ps1 -Base C:\path\2026-10-06-raspios-trixie-arm64-lite.img.xz -OutDir C:\path\out
```

`build.ps1` cross-compiles `airview`, `airview-feeder` and `airview-pisetup` for linux/arm64 from the
current tree (CGO_ENABLED=0), builds `airview-piimage` and runs:

```
airview-piimage build -base <img.xz> -out <dir> -airview <bin> -feeder <bin> -pisetup <bin> [-version V]
```

which decompresses the .xz with the built-in decoder (all block checks and the index verified; parallel,
about 10 s), copies it per image, injects the files into the boot partition, re-opens each image and
verifies it (`<name>.verify.txt`), and writes `<name>.img`, `<name>.img.zip`, `.sha256` files and
`SHA256SUMS`. The base must be a Raspberry Pi OS release that provisions with cloud-init from the boot
partition (Trixie, 2025-10 and later); older releases (Bookworm's `firstrun.sh`) are refused.

Check an image at any time:

```
airview-piimage inspect AirView-Feeder-Pi-<ver>.img                 # partition table, AirView files, fsck
airview-piimage inspect -base raspios.img AirView-Feeder-Pi-<ver>.img  # + byte comparison with the base
```

## pi-gen alternative (maintainers, Linux)

`pi-gen/build.sh` builds the same images with [pi-gen](https://github.com/RPi-Distro/pi-gen) (fetch it
yourself): `airview-piimage payload -kind feeder -out payload …` writes the boot files to a directory,
then `pi-gen/build.sh feeder /path/to/pi-gen payload <ver>` adds stage `stage-airview` after `stage2`,
which copies `vendor-data` and `airview/` onto the boot partition. The first boot is identical.

## Files

| File | Purpose |
|---|---|
| `firstboot.sh` | First-boot provisioning (POSIX sh; test hooks `AVI_ROOT`, `AVI_DRYRUN`) |
| `vendor-data` | cloud-init vendor script that runs `firstboot.sh` |
| `airview-pisetup.service` | systemd unit of the feeder setup page |
| `feeder-setup.txt` | Template for the unattended feeder setup |
| `boot-readme.txt` | Template of `airview/README.txt` on the card |
| `embed.go` | Embeds the above for `cmd/airview-piimage` |
| `build.ps1` | Windows build of binaries and images |
| `pi-gen/` | pi-gen stage and wrapper |
| `piimage_test.go` | Line endings, shell syntax, `firstboot.sh` end to end in a fake root |

## Limits and honest notes

- A Raspberry Pi cannot be booted on the build machine: the first-boot path is tested in a fake root
  with a POSIX shell (`piimage_test.go`), the setup page and the installer arguments against the real
  installer's `--dry-run`, and the image structure by reading it back with our FAT reader and with 7-Zip.
  The boot on real hardware is still to be confirmed.
- Raspberry Pi OS updates are not applied at build time; run `sudo apt update && sudo apt full-upgrade`
  after the first boot, or rebuild from a newer release.
- The station image puts the database on the SD card; for an aggregator use an SSD (DESIGN: storage on a Pi).
