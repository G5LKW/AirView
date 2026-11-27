# deploy/ - release build and installation kits (WP-BUILD)

One command builds every deliverable and verifies it:

```powershell
powershell -ExecutionPolicy Bypass -File deploy\build.ps1 -OutDir D:\release\1.2.3 -Version 1.2.3 `
    -PiImages D:\images\2026-10-06-raspios-trixie-arm64-lite.img.xz -PiBaseSha256 <published sha256> `
    -SignKey D:\offline\airview-release.key
```

Without `-OutDir` the output goes to `dist\` (the Release stage only: it renames a running `dist\airview.exe` to
`airview.exe.old`). The artifact list is in the header of `build.ps1`; the run ends with `relkit check`, which
prints every artifact with what was verified and fails the build on any problem.

## Pieces

| Path | What | Built into |
|---|---|---|
| `build.ps1` | release orchestration (this WP) | everything below |
| `relkit/` | Go helper: `pack` (root-owned, mode-exact, reproducible tar.gz), `sums`, `notices`, `check` | used by build.ps1 |
| `pi/` | Pi station kit: `install.sh` (idempotent, versioned, roll-back, `--base-url`), `update.sh`, `uninstall.sh`, unit | `airview-pi-<v>.tar.gz`, `airview-pi-install.sh`; SD image reuses install.sh + unit |
| `server/` | Linux server kit (VPS): install/upgrade/uninstall/backup/restore, hardened unit, logrotate, proxy examples, healthcheck | `airview-server-linux-{amd64,arm64}.tar.gz`, `airview-server-install.sh` |
| `docker/` | Dockerfile for the prebuilt binaries + README | `airview-docker-<v>.tar.gz` (with `docker-compose.example.yml`, binaries, healthcheck) |
| `../Dockerfile`, `../docker-compose.example.yml` | source build of the image | (repository) |
| `feeder/pi/` | feeder installer (WP-PI) | `airview-feeder-pi-<v>.tar.gz`, `airview-feeder-install.sh` |
| `feeder/windows/` | portable feeder ZIP (WP-WF) | `AirView-Feeder-<v>-windows-<arch>.zip` |
| `installer/` | own Windows installers (WP-SETUP-exe) | `AirView-Setup-<v>.exe`, `AirView-Feeder-Setup-<v>[-arm64].exe` |
| `feeder/pi-image/`, `../cmd/airview-piimage` | SD-card images (WP-PIIMG) | `AirView-{Station,Feeder}-Pi-<v>.img.zip` (`-PiImages`) |
| `release/` | update signing tool (WP-UP) | `release/{airview,feeder}/manifest.json(.sig)` + `dl/` (`-SignKey`) |

## Contracts

- **Bundle layout**: `NAME.tar.gz` holds exactly one top-level directory `NAME/`; every entry root:root;
  directories and executables (ELF, `#!` scripts) 0755, everything else 0644; LF line ends; `VERSION`,
  `LICENSE.txt`, `THIRD-PARTY-NOTICES.txt` in every bundle. `relkit check` enforces all of it.
- **Names fetched by scripts** (do not rename without changing the scripts):
  `airview-server-linux-<arch>.tar.gz` + `SHA256SUMS` (server `install.sh --base-url`),
  `airview-pi-<version>.tar.gz` + `SHA256SUMS` (Pi `install.sh`/`update.sh --base-url`: the last
  `airview-pi-*.tar.gz` line of SHA256SUMS), `airview-feeder-linux-<arch>` / `airview-feeder-windows-<arch>.exe`
  (self-update manifests, `selfupdate.ScanArtifacts`), `airview-feeder-<target>` inside the feeder bundle
  (feeder `install.sh` picks its own CPU).
- **Release site**: serve the output directory as is (all top-level files plus `SHA256SUMS`). An aggregator serves
  `release/feeder/` under `/feeder/` (`internal/selfupdate.ReleaseHandler`).
- **Signing key**: only a path parameter; the build refuses a key inside the repository. The public key printed at
  the end goes into `update.keys` / `--manifest-key` / `/MANIFESTKEY=`.
- **Pi images**: the base image is a download the operator makes and verifies (`-PiBaseSha256`); the build never
  downloads. Only `.img.zip` (+ `.sha256`, `.verify.txt`) ship unless `-KeepRawImages`.

## Tests

- `go test ./deploy/relkit` - packing rules, reproducibility, every check rule against a deliberately broken input.
- `go test ./deploy/pi` - install / reinstall / upgrade / roll-back / download + update / corrupted download /
  uninstall / purge in a scratch root with stub system commands (needs sh + tar: Git for Windows).
- `go test ./deploy/server` - the server kit (same technique).
- Acceptance: a full `build.ps1` run into a scratch `-OutDir` (all artifacts, `relkit check` clean).

## fix:deliverables (DLV-01, DLV-03..06)
- build.ps1 reads the public key of `-SignKey` first and bakes it into the feeder installers (`build-installers.ps1 -ManifestKey/-UpdateUrl` -> product `postInstall`: `airview-feeder config set update.keys=... update.manifestUrl=...`) and the feeder Pi image (`-manifest-key/-update-url` -> update.conf). `-UpdateBaseUrl https://...` is where `release\` is published (feeder manifest at `<base>/feeder/manifest.json`).
- `-SignKey` under %TEMP%/%TMP% or with test/throwaway/scratch/dummy in its name is refused unless `-AllowTestKey`.
- The build fails when sources under cmd/, internal/, web/, deploy/ or go.mod/go.sum change while it runs; it writes BUILD-INFO.txt.
- relkit check: every archive holding programs (ELF in a tarball, .exe in a ZIP) must hold LICENSE.txt and THIRD-PARTY-NOTICES.txt; the portable feeder ZIPs and the Pi images now do.
- The setup stub is asInvoker by default (`-tags setupadmin` / `build-installers.ps1 -AdminStub` for requireAdministrator); per-machine work relaunches elevated, also with /S.
