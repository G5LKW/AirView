# AirView on a Raspberry Pi

Works on 64-bit (recommended) and 32-bit Raspberry Pi OS (Raspberry Pi 3/4/5, Zero 2 W). No dependencies:
one static binary. For a ready SD card use the image `AirView-Station-Pi-<version>.img.zip` instead
(deploy/feeder/pi-image); it runs this same installer on first boot.

## Install

**One line** (downloads the newest `airview-pi-<version>.tar.gz` listed in the release site's
`SHA256SUMS`, checks its SHA-256 and installs it; the site is remembered for updates):

```sh
curl -fsSL https://<release-site>/airview-pi-install.sh | sudo sh -s -- --base-url https://<release-site>
```

**From the bundle** (copy `airview-pi-<version>.tar.gz` to the Pi, for example with `scp`):

```sh
tar xzf airview-pi-1.2.3.tar.gz
cd airview-pi-1.2.3
sudo ./install.sh
```

Open `http://<pi-address>/` in a browser. Set your receiver address (the Radarcape's host name or IP),
station coordinates and antennas under **Settings**.

`install.sh` is idempotent and versioned: run it again with the same bundle to repair an installation,
or with a newer one to upgrade. It prints what it does (`Installing 1.2.3`, `Upgrading 1.2.2 -> 1.2.3`,
`Reinstalling 1.2.3`), keeps the data and the operator configuration, keeps the previous binary as
`/opt/airview/airview.prev`, and **rolls back automatically** when the new version does not stay running.
Options: `--no-start` (install only), `--base-url URL` (download mode), `--force` (reinstall a version that is
already installed in download mode), `--low-wear-journal` (keep the systemd journal in RAM, see
[SD card wear](#sd-card-wear)).

## Update

```sh
sudo /opt/airview/update.sh                                   # from the remembered release site
sudo /opt/airview/update.sh --base-url https://<release-site> # from another site
sudo ./airview-pi-1.2.4/update.sh                             # from an unpacked newer bundle
```

Nothing happens when the newest version is already installed (`--force` reinstalls it). Take a backup of
`/var/lib/airview` first if you want to be able to go back to the old data (`airview backup`).

## Where things live

| What | Where |
|---|---|
| Binary | `/opt/airview/airview` (previous one: `airview.prev`), version in `/opt/airview/VERSION` |
| Scripts | `/opt/airview/install.sh`, `update.sh`, `uninstall.sh` |
| Database + config | `/var/lib/airview` (`airview.db`, `config.json`) |
| Service | `systemctl status airview` |
| Logs | `/var/lib/airview/airview.log` (everything); `journalctl -u airview` (warnings and errors) |
| Operator configuration | `/etc/airview/airview.jsonc` (created once, never overwritten; wins over the web UI) |
| Reference of every setting | `/etc/airview/airview.example.jsonc` (refreshed on every install) |
| Release site for updates | `/etc/airview/update.env` (written by a download install) |
| Licences | `/opt/airview/LICENSE.txt`, `/opt/airview/THIRD-PARTY-NOTICES.txt` |

Configuration can also come from environment variables (`AIRVIEW_HTTP_PORT=8080`, list them with
`/opt/airview/airview config env`; `AIRVIEW_<NAME>_FILE=/path` reads a secret from a file), e.g. in a drop-in made
with `sudo systemctl edit airview`. Check everything with
`sudo -u airview AIRVIEW_CONFIG=/etc/airview/airview.jsonc AIRVIEW_DATA=/var/lib/airview /opt/airview/airview config check`.
Settings fixed this way keep their value when changed in the web interface (`GET /api/config` lists them under `locked`). Full reference: `docs/CONFIG.md`.

## SD card wear

AirView is built to run for years from an SD card. Nothing needs to be set up for that:

- **Storage profile.** `storage.profile` is `auto`: when the data directory is on an SD/MMC card (or the
  board is a small ARM device) AirView picks the `low-wear` profile - the database commits every 30 s,
  keeps the statistics of the current day and the heat map in memory for up to 5 / 15 minutes and
  checkpoints its write-ahead log every 10 minutes, with small caches. Pages always show the data still
  held in memory, so nothing on screen changes. `sudo -u airview /opt/airview/airview db profile -data
  /var/lib/airview` shows the profile in force and why; `storage.profile = "standard"` in
  `/etc/airview/airview.jsonc` restores the more frequent writes (for example on an SSD).
- **What a power cut can lose.** A clean stop (`systemctl stop`, `reboot`, `shutdown`) writes everything
  first. Pulling the power loses at most the last minute of trail points, 5 minutes of coverage / signal
  statistics, 15 minutes of heat-map increments and 30 minutes of the open hour's range statistics, plus
  what Linux had not written back yet (about 30 s). The database file itself is never damaged. Full
  table: `docs/CONFIG.md`, "Storage profiles".
- **Log lines.** Under systemd AirView copies only warnings and errors to the journal; the complete log is
  `airview.log` in the data directory (4 MB, one rotated copy). `process.journalLevel = "info"` sends
  everything to the journal again.
- **Map tiles.** The tile cache (`/var/lib/airview/tilecache`, filled only while someone pans the map) is
  capped at 128 MB with the `low-wear` profile (512 MB elsewhere; `AIRVIEW_TILECACHE_MB` overrides it).

Optional operator steps, never done by the installer unless asked:

- **Journal in RAM.** `sudo ./install.sh --low-wear-journal` writes
  `/etc/systemd/journald.conf.d/airview-low-wear.conf` (`Storage=volatile`, 32 MB) and restarts journald.
  The journal of *every* service is then lost at a reboot (AirView's own `airview.log` is not affected).
  Undo: delete the file and run `sudo systemctl restart systemd-journald`.
- **`noatime`.** Raspberry Pi OS mounts the root file system with `relatime`, which already writes access
  times rarely. Adding `noatime` to the root entry of `/etc/fstab` (then reboot) removes the rest; this is
  a system-wide choice and has no effect on AirView's correctness.
- **A USB SSD** for `/var/lib/airview` removes the wear question altogether; AirView then picks the
  `standard` profile by itself.

## Connecting the Windows app to the Pi

1. On the Pi's web interface: **Settings -> Connection / remote -> Data port (hub) of this station** -> enable it and set a password (default TCP port 30154).
2. In the Windows app: **Settings -> Connection / remote** -> choose either
   - **Remote data** - the app logs in to the Pi's data port and runs its own local statistics on the live stream, or
   - **Remote website** - the app window simply shows the Pi's hosted website.

## Notes

- The Pi build always runs as a standalone station ("mode" in config.json is ignored). To make a headless
  instance follow another AirView host's data port instead, set `Environment=AIRVIEW_REMOTE=1` in the service unit
  and configure **Remote data** mode. For a network aggregator on a VPS use the server kit
  (`airview-server-linux-<arch>.tar.gz`, deploy/server).
- `Environment=AIRVIEW_DEBUG=1` enables debug logging (in the journal too). AirView keeps a size-capped log file
  `airview.log` (+ `airview.log.1`, 4 MB each) in the data directory; the journal receives warnings and errors
  only (`process.journalLevel`).
- Upgrading from 1.0.x converts the database to the 1.1 storage layout in the background (the station keeps
  working; `airview db migrate --status` shows the progress). A backup is written to
  `/var/lib/airview/backup/` first when the card has room for it. Going back to 1.0.x is only possible by
  restoring that backup (see the release notes).
- Try it without a receiver: `/opt/airview/airview -demo -data /tmp/airview-demo -port 8080`.

## Uninstall

```sh
sudo /opt/airview/uninstall.sh          # keep data and /etc/airview
sudo /opt/airview/uninstall.sh --purge  # delete data, /etc/airview and the airview user too
```
