# AirView 1.1 release notes (storage efficiency; not released yet)

The 1.1 storage layout makes the station database several times smaller and writes far less to disk, so a
Raspberry Pi can run from an SD card for years. Nothing was removed: every page, API answer and export shows
the same data as 1.0. Measured numbers: docs/perf/DB-RESULTS.md.

### What changes for you
- **Smaller database, fewer writes, on every platform.** Trails are stored as compressed chunks (about
  7-10 bytes per point instead of 38), meteorological observations and range statistics in compact tables,
  the heat map in tile order; commits are grouped, statistics are kept in memory a little longer (pages
  still show them at once) and the write-ahead log is checkpointed on a timer instead of every 4 MiB.
- **Storage profiles** (`storage.profile`, docs/CONFIG.md "Storage profiles"): `auto` picks `low-wear` on an
  SD card or a small ARM board, `server` for an aggregator, `standard` elsewhere. Single values can be
  changed under `storage.tuning`. `airview db profile` shows what is in force.
- **What a power cut can lose** depends on the profile (at most 15 minutes of heat-map increments with
  `low-wear`); a clean stop always writes everything. The database file is never damaged. Table: docs/CONFIG.md.
- **Logging under systemd:** the journal receives warnings and errors only (`process.journalLevel`, default
  `warn` when systemd runs AirView); `airview.log` in the data directory still has every line. The server kit
  keeps `info` in the journal (`AIRVIEW_PROCESS_JOURNAL_LEVEL=info` in its unit).
- **Tile cache:** 128 MB cap with the `low-wear` profile (512 MB elsewhere).
- **Raspberry Pi:** `install.sh --low-wear-journal` (opt-in) keeps the systemd journal in RAM; the unit allows
  120 s for the final write at a stop. The server kit writes `"storage": {"profile": "server"}` for a new
  aggregator.

### Automatic migration
- The first start of 1.1 converts an existing database **in the background**. The station keeps receiving,
  showing and recording while it runs (minutes on a PC, up to about an hour for a large database on a Pi);
  every step is its own transaction, so a crash or power cut resumes where it stopped and never loses data.
  The migration pauses while the disk has too little free space.
- **Backup first:** before the first step AirView writes `<data>/backup/airview-pre-1.1-<date>.db` (and
  `stations-pre-1.1-<date>.db` on an aggregator) when the disk has room for a copy
  (`storage.backupBeforeMigrate`: `auto`; `always` = no migration without a backup; `never` = no backup).
  This automatic backup is deleted 14 days after the migration completed.
- Headless installs: `airview db migrate --status` (progress, safe while running), `--backup` (a copy of
  every database now, kept until you delete it), `--now` (finish at once, service stopped). The freed space
  goes back to the disk over the next hours; `airview db vacuum` (service stopped) does it at once.
- **Downgrade to 1.0 only by restoring the backup.** 1.0 does not read the new trail chunks or compact
  tables: stop AirView, put the backup in place of `airview.db` (delete `airview.db-wal` / `-shm`), then
  install 1.0. Data recorded after the backup is not in it. The installers' automatic roll-back (a new
  version that does not start) restores the previous program, not the previous data layout.

# AirView 1.0.0 release notes

Released 2026-11-14. The first complete release: the standalone station, the network aggregator with AirView's
own multilateration (MLAT), the feeder program, and installers for Windows, Raspberry Pi and Linux servers.

## What is in 1.0.0

### Standalone station (unchanged behaviour, now versioned)
- ADS-B / Mode S station for a Radarcape or any Beast, AVR, SBS or JSON receiver: live map with trajectory
  prediction, long-term statistics, antenna and terrain line-of-sight analysis, RF lab, alerts, flight reports
  and replay, weather and propagation views, timing diagnostics and Locate Me.
- Windows desktop app (WebView2 window plus web server) and headless Linux / Raspberry Pi service.

### Network aggregator (`mode: "aggregator"`)
- Merges many feeders on a headless Linux VPS. Designed and load-tested in simulation for 1,000+ feeders and
  10,000+ aircraft (see docs/aggregator/OPERATOR-GUIDE.md §2.3 for sizing).
- Ingest: native AVF/1 (TLS 1.3, Ed25519 feeder identity, TCP 30160), Beast / beast_reduce_plus compat
  (map and statistics only, TCP 30004), and a clean-room mlat-client protocol listener (TCP 31090, optional UDP).
- Own MLAT engine: joint clock filter per connected component, solver with ambiguity handling, tracker,
  timing quarantine for new stations, hold-out quality measurement, receiver self-survey (a survey with
  σh ≤ 50 m that passes the suspect checks is used automatically), network bundle adjustment, SSR
  self-location.
- Accounts: owners (password + optional TOTP), admins (password + mandatory TOTP), claims, enrol tokens,
  share links, audit log. Admin bootstrap with `airview admin init` or a generated password (Docker).
- Privacy by default (UK GDPR templates in docs/aggregator): station pins hidden by default, no receiver
  details on public surfaces, coverage products owner/admin only, exact-data admin views audited.
- Federation with other AirView networks, MLAT result outputs for other tools, configurable limits.
- Every setting can be set in the operator file (`airview.jsonc`) and through `AIRVIEW_*` environment
  variables; `airview config check` validates a configuration. Reference: docs/CONFIG.md.

### Feeder (`airview-feeder`)
- Headless feeder for Raspberry Pi / Linux and Windows (service plus optional tray), status page, one or more
  uplinks, MLAT results back as a Beast output, signed self-update.

## Downloads

| File | For |
|---|---|
| `AirView-Setup-1.0.0.exe` | Windows station installer (per-machine, or `/CURRENTUSER` without admin rights) |
| `airview.exe` | Windows station, portable (no installer) |
| `AirView-Feeder-Setup-1.0.0.exe` / `-arm64.exe` | Windows feeder installer (x64 / Windows on Arm) |
| `AirView-Feeder-1.0.0-windows-{amd64,arm64}.zip` | Windows feeder, portable |
| `airview-pi-1.0.0.tar.gz` + `airview-pi-install.sh` | Raspberry Pi station app with `install.sh`, `update.sh`, `uninstall.sh` |
| `airview-feeder-pi-1.0.0.tar.gz` + `airview-feeder-install.sh` | Raspberry Pi / Linux feeder with its installer |
| `AirView-Station-Pi-1.0.0.img.zip` | Ready Raspberry Pi SD-card image, station (Raspberry Pi OS Lite 64-bit, trixie 2026-10-06) |
| `AirView-Feeder-Pi-1.0.0.img.zip` | Ready Raspberry Pi SD-card image, feeder |
| `airview-server-linux-{amd64,arm64}.tar.gz` + `airview-server-install.sh` | Linux VPS server kit (systemd, logrotate, backup/restore, proxy examples) |
| `airview-docker-1.0.0.tar.gz` | Docker build context with the prebuilt binaries |
| `airview-linux-*`, `airview-feeder-linux-*`, `airview-feeder-windows-*.exe` | Plain binaries (also the self-update payloads) |
| `AirView-Manual-1.0.pdf` | The user and operator manual (229 pages, A4): installation, station, network, reference |
| `SHA256SUMS`, `SHA256SUMS.sig` | Checksums of every file (27, the manual included), signed with the release key |
| `release/airview/`, `release/feeder/` | Signed update manifests for self-update |

Check a download with `sha256sum -c SHA256SUMS --ignore-missing` (Linux) or
`Get-FileHash <file>` (Windows) against `SHA256SUMS`.

## Installing
- **Windows:** run `AirView-Setup-1.0.0.exe`. The installers are not Authenticode-signed: SmartScreen shows
  "Windows protected your PC"; check the SHA-256, then "More info" > "Run anyway". Silent switches:
  deploy/installer/README.md.
- **Raspberry Pi:** write an `.img.zip` with Raspberry Pi Imager and boot, or unpack `airview-pi-1.0.0.tar.gz`
  and run `sudo ./install.sh` (README "Raspberry Pi install").
- **Linux VPS (aggregator):** docs/aggregator/OPERATOR-GUIDE.md.
- **Feeders:** docs/aggregator/FEEDER-GUIDE.md.

## Upgrading from 0.1.0
- Station: install over the old version (installer, `install.sh` again, or replace `airview.exe`); data and
  settings are kept.
- The configuration layout is unchanged (config version 1).

## Rebuild of 14 November 2026
The 1.0.0 artifacts were rebuilt after the soak memory follow-up below; the files published as 1.0.0 are the
rebuilt ones (built 2026-11-14T23:38Z, `BUILD-INFO.txt`). Changes against the first 1.0.0 build:
- **Memory fixes** in the aggregator: MLAT late-copy rings sized per worker instead of per CPU thread, one
  shared AVF/1 send-buffer pool, uncompressed mlat-client connections no longer cut off by the compressed
  byte budget, and `/healthz` reports the MLAT start-up sync as "ok (start-up sync ...)" instead of "degraded".
- **Sizing:** OPERATOR-GUIDE §2.3 "Memory" has the measured curve and the planning formula
  resident memory ≈ 2.4 × (180 MB + 1.2 MB per busy feeder + 0.25 MB per aircraft): about 1.2 GB for
  85 feeders and 800 aircraft (measured), about 9 GB projected for 1,000 feeders and 10,000 aircraft.
- The PDF manual `AirView-Manual-1.0.pdf` is part of the release and of `SHA256SUMS`.

Checked for the rebuild: `gofmt`, `go vet ./...` clean and the full `go test ./...` passing (every package,
in parallel); `deploy\build.ps1` with the same Pi base image and the same scratch signing key: every artifact
verified by the release tool, source tree unchanged during the build; `SHA256SUMS.sig` verified against the
release public key and every hash re-checked; a copy of `airview.exe` in headless demo mode (healthz ok,
1.0.0, demo traffic); silent per-user install and uninstall of `AirView-Setup-1.0.0.exe` in scratch folders
(installed `airview.exe` identical to the release file; shortcuts, uninstall entry and files removed); both
Pi images unzipped and re-opened with the FAT verifier (`airview-piimage inspect -base`): MBR and root file
system identical to the base, boot file system consistent, payload binaries equal the release binaries.
The feeder installers were smoke-tested on the first 1.0.0 build (below), not again on the rebuild.

## Verified for this release
- `gofmt`, `go vet ./...` clean; `go test ./...` passes (the clock filter's 200-station epoch budget
  `TestEpochCostGate` is a wall-clock gate that may fail only while every package runs in parallel; it passes
  when run alone, 57 s).
- Every artifact checked by the release tool: static ELF for the right CPU, PE machine type, tar owners and
  modes, ZIP CRCs, VERSION files, SHA256SUMS; update manifests signed and verified.
- Silent per-user install and uninstall of both Windows installers (scratch folders); the installed feeder
  connected to a test aggregator.
- Windows app: GUI window and headless demo mode.
- Both Pi images re-opened: partition table, boot file system consistent, AirView payload matches the
  release binaries.
- 30-minute soak of an aggregator (Windows build) with 85 simulated feeders (60 AVF, 15 Beast, 10
  mlat-client), one real `airview-feeder` and 800 aircraft, with MLAT: every link stayed up, CPU about 2.4 %
  of a 32-thread machine, private memory 2.6 GB after 30 minutes and levelling off, no errors. Health showed
  "degraded" for about 3 minutes during the MLAT start-up sync.
- Memory follow-up (heap profiles at 5/15/30 minutes of the same soak): no unbounded growth; the memory was
  bounded stores filling up plus garbage-collector headroom. Three fixes: the MLAT late-copy rings no longer
  scale with the CPU count (515 MB on 32 threads, now 64 MB), the AVF/1 send buffers are pooled across
  connections instead of per connection (about 40 MB with 61 links, still growing at 30 minutes), and an
  mlat-client without compression is no longer cut off by the compressed byte limit (one simulated client was
  refused every 5 minutes). Re-run for 45 minutes: private memory 0.71 GB at 5 min, 1.08 GB at 30 min,
  1.16 GB from minute 42 on (flat), live heap 482 MB; all 10 mlat-clients connected throughout. `/healthz` now
  reports MLAT muting in the first 10 minutes after verification starts as "ok (start-up sync ...)" instead of
  "degraded". Sizing formula: OPERATOR-GUIDE §2.3 "Memory".

## Known limitations
- **Not run on real hardware for this release:** Raspberry Pi boot of the SD-card images, the Pi installers on
  a Pi, Docker and systemd on Linux, an elevated per-machine Windows install (service and firewall rule), and
  an RTL-SDR receiver. These were checked statically and by unit tests only.
- **Release key:** the update manifests and `SHA256SUMS.sig` of this build are signed with a key generated
  on the build machine for this release. Before publishing, generate the real release key offline
  (`go run ./deploy/release keygen`), keep it with a backup, and rebuild with it; feeders and Pi stations trust
  only the key baked into their installers and images.
- **Scale:** the 1,000-station MLAT quality row and a live 1,000-feeder memory measurement were not repeated
  at full size; mitigations are in place and smaller simulations pass.
- Windows installers are unsigned (see above).
- The items listed under "Known limitations" in README.md still apply.
