# 1.3 Raspberry Pi station

On a Raspberry Pi AirView runs as a headless service (`airview.service`); you use it from any browser on
your network. There are three ways to install it:

| Way | File | Best for |
|---|---|---|
| [SD-card image](#131-sd-card-image) | `AirView-Station-Pi-<version>.img.zip` | A new Pi: write the card, boot, done. |
| [App bundle](#install-from-the-app-bundle) | `airview-pi-<version>.tar.gz` | An existing Raspberry Pi OS (64-bit or 32-bit). |
| [One line](#one-line-install-from-a-release-site) | `airview-pi-install.sh` | An existing Pi with internet access, when the release files are on a web site. |

All three end with the same installation: the binary in `/opt/airview`, data in `/var/lib/airview`, the
service running as the unprivileged user `airview` on port 80.

## 1.3.1 SD-card image

The image is the official **Raspberry Pi OS Lite (64-bit)** release (Trixie, 2026-10-06 for AirView 1.0.1),
byte for byte, plus a few files on the boot partition that install AirView on the first boot. It runs on every
64-bit Pi supported by Raspberry Pi OS Trixie: Pi 3, 4, 5, 400, 500, Zero 2 W, CM3/4/5.

### Writing the card with Raspberry Pi Imager

1. Check the download: `AirView-Station-Pi-1.0.1.img.zip.sha256` holds the SHA-256 of the zip
   (`Get-FileHash -Algorithm SHA256 <file>` on Windows, `sha256sum -c <file>.sha256` on Linux).
2. Raspberry Pi Imager → **Choose Device** → your Pi.
3. **Choose OS** → scroll down → **Use custom** → pick `AirView-Station-Pi-1.0.1.img.zip`. Imager reads the
   .zip directly.
4. **Choose Storage** → the SD card (or a USB / NVMe drive).
5. When Imager offers **OS customisation**, use it as for any Raspberry Pi OS image:
   - user name and password, Wi-Fi (SSID, password, country), time zone and keyboard;
   - on the *Services* tab, **Enable SSH** with a password or your public key;
   - the host name: leave it at the default to get `airview`. A name you choose is kept; then open
     `http://<your-name>.local/`.

   Without customisation there is no login user and no SSH, but the AirView web interface still works over
   Ethernet.
6. Write the card, put it in the Pi and switch on.

Other writers (balenaEtcher, `dd`) work too, without the customisation step.

### First boot

The first start takes a few minutes: Raspberry Pi OS grows the root partition, cloud-init applies your
Imager settings, and then AirView's first-boot step runs (log: `/var/log/airview-firstboot.log`):

1. it sets the host name to `airview`, but only while it is still `raspberrypi`, so that `airview.local`
   resolves;
2. it checks the AirView files on the card against their `SHA256SUMS` (a damaged card stops here with a
   message on the console);
3. it copies an optional operator file `airview/airview.jsonc` from the card to `/etc/airview/airview.jsonc`
   (only when none exists) and runs the standard installer `install.sh` (the same as in the app bundle);
4. it removes the program files and `airview.jsonc` from the boot partition (every local user can read it),
   writes `airview/INSTALLED.txt` and prints the address on the console.

Then open **`http://airview.local/`** (or `http://<the Pi's IP address>/`) and continue with the
[first-run setup](01-install-06-first-run.md).

**Pre-configure the station.** To start with your own settings, copy an operator file to the card's boot
partition as `airview/airview.jsonc` before the first boot. Its format is described in
[4.1](04-reference-1-configuration.md); every setting it contains is shown as locked in the web interface.

**If something fails**, the files stay on the card. Log in (keyboard or SSH), read
`/var/log/airview-firstboot.log` and `journalctl -u airview`, fix the cause and run
`sudo sh /boot/firmware/airview/firstboot.sh` again.

Raspberry Pi OS updates are not applied when the image is built. After the first boot run:

```sh
sudo apt update && sudo apt full-upgrade
```

## Install from the app bundle

The bundle `airview-pi-<version>.tar.gz` contains both Linux builds (arm64 and armv7), the installer
`install.sh`, `update.sh`, `uninstall.sh`, the systemd unit and the licences. The installer picks the right
binary for the board. It runs on 64-bit and 32-bit Raspberry Pi OS (Pi 3/4/5, Zero 2 W).

```sh
scp airview-pi-1.0.1.tar.gz pi@raspberrypi.local:
ssh pi@raspberrypi.local
tar xzf airview-pi-1.0.1.tar.gz
cd airview-pi-1.0.1
sudo ./install.sh
```

The installer prints what it does (`Installing 1.0.1`, `Upgrading 0.1.0 -> 1.0.1` or `Reinstalling 1.0.1`)
and ends with the address of the web interface (`http://<pi-address>/`, port 80).

`install.sh` is idempotent: run it again with the same bundle to repair the installation, or with a newer one to
upgrade. It keeps the data and the operator file, keeps the previous binary as `/opt/airview/airview.prev`,
and rolls back automatically when the new version does not stay running.

| Option | Meaning |
|---|---|
| `--no-start` | Install but do not (re)start the service. |
| `--base-url URL` | Download mode, see below. |
| `--key KEY` | Release public key (base64 Ed25519): `SHA256SUMS.sig` must verify against it. The key is pinned in `/etc/airview/update.env`. |
| `--allow-unsigned` | Accept a download without a verified signature (not recommended). |
| `--force` | Download mode: reinstall a version that is already installed. |
| `-h`, `--help` | Help. |

## One-line install from a release site

When the release files are published on a web server (the files `SHA256SUMS`, `SHA256SUMS.sig`,
`airview-pi-<version>.tar.gz` and `airview-pi-install.sh` in one folder), one command downloads the newest Pi
bundle listed in `SHA256SUMS`, checks its SHA-256 and installs it:

```sh
curl -fsSL https://<release-site>/airview-pi-install.sh | sudo sh -s -- --base-url https://<release-site> --key <release key>
```

- With `--key`, the installer also verifies `SHA256SUMS.sig` (this needs OpenSSL 3.0 or newer, which Raspberry
  Pi OS has). Without it, it prints a warning: the check is then only as trustworthy as the HTTPS connection.
- Plain `http://` is refused unless you give `--key` (or `--allow-unsigned`).
- The site (and the key) are remembered in `/etc/airview/update.env` for `update.sh` ([1.8](01-install-07-updates.md)).

## 1.3.2 Where things live

| What | Where |
|---|---|
| Program | `/opt/airview/airview` (previous one: `airview.prev`), version in `/opt/airview/VERSION` |
| Scripts | `/opt/airview/install.sh`, `update.sh`, `uninstall.sh` |
| Database and web settings | `/var/lib/airview` (`airview.db`, `config.json`, `airview.log`) |
| Operator file | `/etc/airview/airview.jsonc` (created once, never overwritten; wins over the web interface) |
| Reference of every setting | `/etc/airview/airview.example.jsonc` (refreshed on every install) |
| Release site for updates | `/etc/airview/update.env` (written by a download install) |
| Service | `systemctl status airview`; logs `journalctl -u airview -f` |
| Licences | `/opt/airview/LICENSE.txt`, `/opt/airview/THIRD-PARTY-NOTICES.txt` |

The service runs `/opt/airview/airview -headless -data /var/lib/airview` as user `airview` with only the
capability to bind port 80 and a read-only view of the rest of the system. Put local changes in a drop-in
(`sudo systemctl edit airview`), never in the unit file, because an upgrade replaces it.

### Configuration from files and environment variables

Settings come from three layers, each overriding the one before:

1. `/var/lib/airview/config.json`, edited by **Settings** in the web interface;
2. the operator file `/etc/airview/airview.jsonc`; settings fixed here are shown as locked in the web interface;
3. `AIRVIEW_*` environment variables, for example in a drop-in:

   ```ini
   # sudo systemctl edit airview
   [Service]
   Environment=AIRVIEW_HTTP_PORT=8080
   ```

   `/opt/airview/airview config env` lists the variable of every setting; `AIRVIEW_<NAME>_FILE=/path` reads a
   secret from a file.

Check everything with:

```sh
sudo -u airview AIRVIEW_CONFIG=/etc/airview/airview.jsonc AIRVIEW_DATA=/var/lib/airview /opt/airview/airview config check
```

Try AirView without a receiver, next to the service: `/opt/airview/airview -demo -data /tmp/airview-demo -port 8080`.

## 1.3.3 Uninstall

```sh
sudo /opt/airview/uninstall.sh          # removes the service and program; keeps data and /etc/airview
sudo /opt/airview/uninstall.sh --purge  # also deletes the data, /etc/airview and the airview user
```
