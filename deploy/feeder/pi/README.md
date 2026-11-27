# AirView Network feeder: Raspberry Pi / Debian / Ubuntu installer

`install.sh` installs `airview-feeder` (DESIGN §12.1) as a hardened systemd service on Raspberry Pi OS,
Debian and Ubuntu. It supports **arm64** (aarch64), **armhf** (armv7l, including a 64-bit kernel with a
32-bit userland) and **amd64** (x86_64). ARMv6 (Pi 1, Zero, Zero W) is not supported.

It reuses the receiver you already have and never removes, stops or reconfigures readsb, dump1090,
a Radarcape or other feeders. It never bundles or runs GPL software for you; where a decoder is
missing it explains how to install one yourself.

| File | Purpose |
|---|---|
| `install.sh` | Installer and the update, uninstall, status and claim actions. Bash 4+, self-contained. |
| `update.sh`, `uninstall.sh`, `status.sh` | Thin wrappers around `install.sh --update` / `--uninstall` / `--status`. |
| `pi_test.go` | Dry-run tests (Go test that drives the script through bash; works with Git Bash on Windows). |

## Install

The aggregator serves this script at `https://<aggregator>/feeder/install.sh` with the server address,
server key pin, download URL and release-signing key already filled in. Download it, read it, run it:

```sh
curl -fsSL https://<aggregator>/feeder/install.sh -o /tmp/avf.sh
sudo bash /tmp/avf.sh
```

Or as one line (prompts still work because they read `/dev/tty`):

```sh
curl -fsSL https://<aggregator>/feeder/install.sh | sudo bash
curl -fsSL https://<aggregator>/feeder/install.sh | sudo bash -s -- --token ABCD-1234 --name my-pi --non-interactive
```

Interactive runs use `whiptail` dialogs when available (plain prompts otherwise) and ask for:
the aggregator and enrol token, the station name, the antenna position (or "let the network locate
me"), altitude with a mandatory `m`/`ft` unit and its reference (MSL/HAE), antenna height above ground,
the receiver input, the public pin precision and whether to send timestamps. The consent text of
DESIGN §10.5 is shown with the location and timestamp questions.

### Options

| Option | Meaning |
|---|---|
| `--server HOST[:PORT]` | Aggregator AVF/1 address; port 30160 by default. IPv6: `[2001:db8::1]:30160`. |
| `--pin sha256/BASE64` | Expected AVF server key (SPKI SHA-256). Empty: system CA roots. |
| `--token TOKEN` (`--enrol`) | Enrol token minted by the aggregator admin (needed with admission `invite`). |
| `--url https://HOST` | Download base URL. Default: `https://` + the server host. |
| `--manifest-key BASE64` | Ed25519 public key (raw 32 bytes, base64) that signs `manifest.json`. |
| `--http-pin sha256//BASE64` | Pin the download server's key (`curl --pinnedpubkey`; allows a self-signed certificate). |
| `--binary FILE` | Install a local `airview-feeder` build instead of downloading one (offline installs, development). |
| `--name NAME` | Station name, `[A-Za-z0-9_-]{1,32}`. Default: the host name. |
| `--lat DEG --lon DEG` | Antenna position in decimal degrees (−90..90, −180..180). |
| `--alt 120m` / `--alt 400ft` | Antenna altitude; the unit is mandatory (1 ft = 0.3048 m). −500..9000 m. |
| `--alt-ref msl\|hae` | Altitude reference: mean sea level (default) or GPS ellipsoid height. |
| `--agl 8m` / `--agl 25ft` | Antenna height above ground (default 10 m); used as the survey height prior. |
| `--no-location` | Send no position; the network locates the station (owner/admin-only result). |
| `--input SPEC` | `auto` (default: 127.0.0.1:30005 or a Radarcape on :10003), `HOST:PORT` (Beast), `beast:`/`avr:`/`sbs:HOST:PORT`, `radarcape:HOST`. |
| `--privacy hidden\|region\|cell01` | Public map pin: none (default), within 0.5°, within 0.1°. |
| `--no-timestamps` | Send no timestamps: no MLAT and no timing-based self-location. |
| `--readsb-results` | Also push returned MLAT results into readsb's Beast input 127.0.0.1:30104. |
| `--no-auto-update` | Do not enable the daily update timer (sets `autoUpdate: false`). |
| `--non-interactive`, `-y` | Never prompt: flags, saved answers, presets and defaults only. |
| `--install-chrony` | Install chrony when the clock is not NTP-synchronised (asked interactively otherwise). |
| `--allow-unsigned` | Accept a download without a verifiable manifest signature (not recommended). |
| `--no-start` | Install and enable, but do not start the service. |
| `--arch arm64\|armhf\|amd64` | Override the detected build. |
| `--dry-run`, `--dry-run-root DIR` | Change nothing; print each command; stage written files under DIR. |
| `--update [--force]`, `--status`, `--claim`, `--uninstall [--purge]` | The other actions (see below). |

Values come from, lowest priority first: the aggregator's presets in the script, the answers saved by the
previous run (`/etc/airview-feeder/install.env`, root-only), then command-line flags; interactive
answers override all of them. **Re-running the installer reconfigures** with the current values as
defaults.

### What it checks and detects

- root, systemd, CPU architecture (`dpkg --print-architecture` distinguishes armhf on a 64-bit kernel), 50 MB free disk;
- NTP state from `timedatectl`, offering chrony when the clock is not synchronised;
- a warning when `/etc/default/adsbexchange` sends results to 127.0.0.1:30154 (the AirView hub port), and when ports 8099 or 30105 are taken;
- receivers: a Beast listener on 30005 (readsb, dump1090-fa, ...) is reused as is; a Radarcape (`rcd` process, `radarcape*` host name or :10003); an RTL-SDR (0bda:2838/2832) without a decoder gets instructions to install readsb yourself (a separate GPL program, not bundled); Airspy (1d50:60a1) and SDRplay (1df7) owners are pointed to their decoders; an FTDI Mode-S Beast (0403:6001) needs a Beast-to-TCP bridge because the feeder reads network inputs only.

### Download verification

1. `GET <url>/feeder/manifest.json` and, when a manifest key is known, `<url>/feeder/manifest.json.sig`
   (base64 Ed25519 signature over the exact manifest bytes), verified with OpenSSL ≥ 3.0.
2. The manifest entry for the target (`linux-arm64`, `linux-armv7`, `linux-amd64`) gives the SHA-256,
   optional size and optional URL (default `<url>/feeder/dl/<target>`).
3. The binary must match the SHA-256 (and size) and be an ELF executable; otherwise nothing is installed.

Without a manifest key the hash check is only as strong as the HTTPS connection (a warning says so).
Plain `http://` without a key is refused unless `--allow-unsigned` is given. The manifest format is
specified in `docs/aggregator/HOOKS.md` (WP-PI-installer -> WP-UP).

### What gets installed

| Path | Mode / owner | Content |
|---|---|---|
| `/opt/airview-feeder/airview-feeder` | 0755 root | the binary |
| `/opt/airview-feeder/install.sh` (+ `/usr/local/sbin/airview-feeder-setup` link) | 0755 root | this script (regenerated from its functions when it was piped into bash) |
| `/opt/airview-feeder/{update,uninstall,status}.sh` | 0755 root | wrappers |
| `/etc/airview-feeder/feeder.json` | 0640 root:airview-feeder | feeder configuration (`model.FeederConfig`) |
| `/etc/airview-feeder/install.env` | 0600 root | saved installer answers (includes the enrol token) |
| `/var/lib/airview-feeder/` | 0750 airview-feeder | identity key, UUID, spool (the only writable path) |
| `/etc/systemd/system/airview-feeder.service` | 0644 | the service |
| `/etc/systemd/system/airview-feeder-update.{service,timer}` | 0644 | daily update check, random delay up to 6 h |

The system user `airview-feeder` has no login shell and no home directory of its own.

When `feeder.json` already exists, a re-run updates only the settings the installer manages (receiver 1's
name, input and position, the uplink with id `aggregator`, `results.readsbAddr`, `autoUpdate`) and keeps
everything else, such as more receivers, more uplinks and UI settings. This merge uses `python3`
(present on Raspberry Pi OS). Without it the file is rewritten and the old one is kept as
`feeder.json.bak-<timestamp>`.

### Service hardening

`Restart=always`, `RestartSec=5`, `NoNewPrivileges`, empty `AmbientCapabilities` and
`CapabilityBoundingSet`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`,
`ProtectKernelTunables/Modules/Logs`, `ProtectControlGroups`, `ProtectClock`, `ProtectHostname`,
`RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`, `LockPersonality`,
`MemoryDenyWriteExecute`, `SystemCallArchitectures=native`,
`RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`, `ReadWritePaths=/var/lib/airview-feeder`,
`MemoryMax=128M`, `TasksMax=128`. The configuration comes from `AIRVIEW_CONFIG`/`AIRVIEW_DATA`.
Use `systemctl edit airview-feeder` for local changes, because re-running the installer rewrites the unit.

## After installation

The installer prints the local status page (`http://127.0.0.1:8099/`, reachable remotely with
`ssh -L 8099:127.0.0.1:8099 pi@<host>`), the aggregator's `/me` page, a **claim code** (once the session
is up), and the log, update and uninstall commands. A post-check waits up to 30 s for the aggregator
session and reports refusals such as `enrol_required` or `cloned_feeder`.

```sh
sudo airview-feeder-setup --status       # service, settings (token hidden), link state
sudo airview-feeder-setup --claim        # a new claim code for the aggregator's /me page
sudo airview-feeder-setup                # reconfigure (current values are the defaults)
journalctl -u airview-feeder -f          # logs
```

## Update

`airview-feeder-update.timer` runs `update.sh --quiet` daily (it does nothing when `autoUpdate` is
`false` in `feeder.json`). Run it by hand with `sudo /opt/airview-feeder/update.sh` (`--force`
reinstalls). It downloads and verifies the binary as described above. When the SHA-256 differs from the
installed binary, it keeps the old one as `airview-feeder.old`, swaps the file atomically and restarts.
If the service stops or restarts within 60 s, it **rolls back**. Updates run from this root timer
because the service itself cannot write `/opt`; the unit sets `AIRVIEW_FEEDER_UPDATER=external`.
To refresh the installer scripts, re-run the installer from the aggregator.

## Uninstall

```sh
sudo /opt/airview-feeder/uninstall.sh            # keeps /etc/airview-feeder and /var/lib/airview-feeder (same feeder identity on reinstall)
sudo /opt/airview-feeder/uninstall.sh --purge    # also deletes configuration, identity key and the airview-feeder user
```

This stops and removes the service, the timer, `/opt/airview-feeder` and the `airview-feeder-setup`
link. It never touches readsb, dump1090 or other feeders.

## Testing

`--dry-run` executes nothing that changes the system. It prints every command and stages every file it
would write under `--dry-run-root` (a temp dir by default). A later dry run against the same root reads the
staged files as if they were installed, so re-runs, updates and status can be tested too. The Go tests
drive these dry runs:

```sh
go test ./deploy/feeder/pi/            # needs bash (Git Bash on Windows); OpenSSL 3 + curl for the download tests
```

They cover the following:
- `feeder.json` decodes strictly into `model.FeederConfig` and normalises without corrections;
- unit hardening, wrappers and the installed script copy are identical to the source tree;
- re-runs keep the saved values, and the Python merge keeps unmanaged settings;
- every validation error, and the m/ft conversion;
- signed downloads for all three targets from a local release server (port 18528 when free), and rejection of bad signatures, wrong keys, hash mismatches, non-ELF payloads and missing targets;
- update and roll-back steps, with `autoUpdate=false` respected;
- uninstall with and without `--purge`;
- status without printing the token;
- piped (`curl | bash`) runs;
- aggregator presets.

`shellcheck` was not available on the development machine. The script was reviewed by hand against the
usual shellcheck rules: quoting, `local` with command substitution, `read -r`, no `eval`, `set -Eeuo
pipefail`, and saved answers read back without `source`/`eval`. Running `shellcheck -x install.sh` on a
Linux host is still recommended.
