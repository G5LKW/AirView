# cmd/airview-piimage — wiring

Builds the AirView Raspberry Pi images (Phase 5, WP-PIIMG). User documentation:
`deploy/feeder/pi-image/README.md`.

## Pieces

| Piece | Path | Runs on |
|---|---|---|
| Image builder `airview-piimage` (`build`, `inspect`, `decompress`, `payload`) | `cmd/airview-piimage` | build machine (Windows/Linux/macOS) |
| Setup page `airview-pisetup` | `cmd/airview-piimage/pisetup` | the Pi (linux/arm64), feeder image only |
| FAT32 + MBR library | `internal/fatfs` | build machine |
| .xz (LZMA2) decoder | `internal/fatfs/xz` | build machine |
| First-boot scripts and templates (embedded) | `deploy/feeder/pi-image` (package `piimage`) | the Pi |
| Station installer (reused unchanged) | `deploy/pi/install.sh`, `deploy/pi/airview.service` | the Pi |
| Feeder installer (reused unchanged) | `deploy/feeder/pi/install.sh` | the Pi |

## Build flow (`build`)

1. SHA-256 of the base file (recorded in `image.conf`).
2. `.img.xz` → `<out>/.base-*.img` with `xz.DecodeFile` (parallel per xz block; CRC64/CRC32/SHA-256
   block checks, index and footer verified). Deleted at the end.
3. Per kind: `payload()` assembles `/airview` (binaries are checked to be static linux/arm64 ELF; text
   files are converted to LF); copy the base to `<name>.img.part`; `inject()`:
   - `fatfs.ReadMBR`, first FAT partition, `fatfs.Open`;
   - `Check()` of the base (no severe errors), `provisioning()`: `cmdline.txt`, `config.txt`,
     `user-data`, `meta-data` must exist (cloud-init NoCloud seed, Trixie); an existing foreign
     `vendor-data` is refused;
   - free-space check, write `airview/*` (largest first, contiguous) and `vendor-data`;
   - `Check()` again.
4. `verifyAgainst()` re-opens the image read-only and the base: same size, identical MBR sector,
   identical bytes outside the boot partition (SHA-256), every base boot file unchanged, injected files
   equal to the payload, consistent FAT (fsck-like). Output also goes to `<name>.verify.txt`.
5. `<name>.img.zip` (Deflate, zip64), read back completely (CRC-32), `.sha256` files and `SHA256SUMS`.

## On the Pi

`vendor-data` (cloud-init `scripts_vendor`, final stage, once per instance) → `firstboot.sh`
→ station: `install.sh` | feeder: `airview-pisetup.service` → `airview-pisetup`
→ `bash install-feeder.sh --binary … --non-interactive --no-start`
→ `airview-feeder config set -config /etc/airview-feeder/feeder.json ui.addr=0.0.0.0:8099 ui.passwordHash=<hash>`
(owner/group/mode restored afterwards) → `systemctl disable airview-pisetup.service`
→ `systemctl start --no-block airview-feeder.service`.

Contracts relied on (tests break when they change):

- `deploy/feeder/pi/install.sh` options `--binary --server --pin --token --name --lat --lon --alt --alt-ref
  --agl --no-location --input --privacy --no-timestamps --no-auto-update --readsb-results
  --non-interactive --no-start` and its validation rules (`pisetup/installer_test.go` runs the real
  script in `--dry-run`).
- `airview-feeder config set` with `ui.addr` and `ui.passwordHash` (`TestFeederAcceptsStatusPageSettings`
  builds the real feeder).
- `deploy/pi/install.sh` expects `airview-linux-arm64` and `airview.service` next to it.
- Consent texts C1/C2 word for word (`TestConsentTextsMatchDoc`).

## Tests

`go test ./internal/fatfs/... ./cmd/airview-piimage/... ./deploy/feeder/pi-image/`. Opt-in tests on a
real image: `AIRVIEW_XZ_TEST=<img.xz> AIRVIEW_XZ_SHA256=<hex of the decompressed .img>` (internal/fatfs/xz) and
`AIRVIEW_PIIMG_TEST=<img>` (internal/fatfs).

## fix:deliverables (DLV-03, DLV-05)
- Every payload carries `LICENSE.txt` and the program's `THIRD-PARTY-NOTICES.txt`, both in SHA256SUMS (so they reach /opt/airview-image and the installers copy them to /opt/airview or /opt/airview-feeder). `-licence`, `-notices-station`, `-notices-feeder`; without the notices flags the tool generates them with `go run ./deploy/relkit notices` from `-repo`.
- Feeder payloads carry `update.conf` (`MANIFEST_KEY=`, `UPDATE_URL=`) from `-manifest-key` / `-update-url`; airview-pisetup (`-update-conf`, default /opt/airview-image/update.conf) passes them to install-feeder.sh as `--manifest-key` / `--url`. Without a key the setup page shows automatic updates as unavailable and the installer gets `--no-auto-update`. `feeder-setup.txt` accepts `MANIFEST_KEY=` / `UPDATE_URL=` too.
- deploy/build.ps1 passes the release public key of `-SignKey` and `-UpdateBaseUrl` automatically.
