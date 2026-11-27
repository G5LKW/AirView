# internal/wininstall + cmd/airview-setup — wiring (WP-SETUP-exe)

AirView's own Windows installer. No Inno Setup, no NSIS, nothing downloaded at
build or install time; pure Go with `golang.org/x/sys/windows`.

## Pieces

| Path | Role |
| --- | --- |
| `internal/wininstall/payload.go` | installer file format (stub + ZIP overlay + trailer; signature-aware) |
| `internal/wininstall/build.go` | `Build`: assemble an installer (deterministic) |
| `internal/wininstall/product.go` | product definition (JSON), validation |
| `internal/wininstall/paths.go` | `{token}` expansion, install-folder safety checks |
| `internal/wininstall/args.go` | NSIS/Inno-style command line, exit codes, `/?` text |
| `internal/wininstall/install.go` | install / upgrade / rollback / uninstall engine |
| `internal/wininstall/receipt.go` | `install-receipt.json` |
| `internal/wininstall/system.go` | `System` interface (all side effects outside the install folder) |
| `internal/wininstall/sys_windows.go` | registry, IShellLinkW, netsh, SCM, MoveFileEx |
| `internal/wininstall/lnk.go` | pure-Go `.lnk` reader ([MS-SHLLINK]) for verification |
| `cmd/airview-setup` | the stub: Win32 wizard, silent mode, uninstaller, `/SHOTS=` |
| `cmd/airview-setup/res/{admin,user}` | icon + version + manifest (`.syso`, go-winres) |
| `cmd/airview-setup/mkinstaller` | builder CLI (any OS) |
| `deploy/installer/build-installers.ps1` | builds the release installers |
| `deploy/installer/test-installers.ps1` | acceptance test (scratch identity, no admin) |
| `deploy/installer/products/*.json` | station and feeder definitions |

## Outputs

`build-installers.ps1` writes (default `deploy\installer\out`):

- `AirView-Setup-<ver>.exe` — station (`airview.exe`, README, licence, notices, docs)
- `AirView-Feeder-Setup-<ver>.exe`, `AirView-Feeder-Setup-<ver>-arm64.exe` — feeder
  (`airview-feeder.exe`, `airview-feeder-tray.exe`, icon, README, licence, notices, docs)

The release stub's manifest is `asInvoker` (DLV-06): `/CURRENTUSER` installs
without UAC, and a per-machine install (the default) or uninstall relaunches
itself elevated (UAC prompt, also with `/S`). `-AdminStub` links the old
`requireAdministrator` variant (`-tags setupadmin`).

Feeder update settings (DLV-03): `build-installers.ps1 -ManifestKey <base64>
[-UpdateUrl https://.../feeder/manifest.json]` bakes `-var manifestkey=` /
`-var updateurl=` into the feeder installers; the product's `postInstall`
commands then run `airview-feeder config set update.keys=... update.manifestUrl=...`.

## What an install does

| Item | Per machine | Per user (`/CURRENTUSER`) |
| --- | --- | --- |
| Files | `%ProgramFiles%\<Name>` | `%LOCALAPPDATA%\Programs\<Name>` |
| Uninstall entry | `HKLM\...\Uninstall\<id>` | `HKCU\...\Uninstall\<id>` |
| Start menu / desktop | All Users | current user |
| Firewall rule (opt-in) | `netsh advfirewall`, private+domain, program + TCP port | — |
| Service (feeder, default on) | `airview-feeder service install -name AirViewFeeder -now -data ... -config ...` | — |
| Tray autostart (feeder) | `HKLM\...\Run` "AirView Feeder Tray" | `HKCU\...\Run` |
| Receipt / uninstaller | `install-receipt.json`, `uninstall.exe` in the folder | same |

Uninstall entry values: DisplayName, DisplayIcon (`<app>\<mainExe>,0`),
DisplayVersion, Publisher, UninstallString, QuietUninstallString (`/S`),
InstallLocation, InstallDate, EstimatedSize (KB), NoModify, NoRepair,
VersionMajor/Minor, Comments, HelpLink.

Data folders (never touched on install/upgrade; deleted only with "remove data"):
station `{app}\data` and `%APPDATA%\AirView` (where `airview.exe` keeps its
data when it cannot write next to itself, i.e. in Program Files); feeder
`%ProgramData%\AirView Feeder` (after `airview-feeder uninstall-helper -purge -yes`).

Upgrade: the installer goes to the folder of the existing install
(`InstallLocation`), closes the trays (`tray -signal quit -name <service>`),
stops the service, replaces files transactionally, deletes files the new
version no longer has, re-creates shortcuts, re-runs `service install`
(updates the configuration and starts the service).

The installer never starts the program elevated: "Launch" on the last page
goes through Explorer when Setup runs as administrator, so the first start
creates the data folder as the desktop user.

## Command line

Installer: `/S`, `/D=<dir>` (last, unquoted), `/ALLUSERS`, `/CURRENTUSER`,
`/[NO]DESKTOP`, `/[NO]STARTMENU`, `/[NO]FIREWALL`, `/PORT=n`, `/[NO]SERVICE`,
`/[NO]TRAY`, `/NOLAUNCH`, `/LOG=file`, `/SHOTS=dir`, `/?`; feeder
configuration: `/SERVER= /PIN= /ENROL= /NAME= /LAT= /LON= /ALT= /AGL=
/PRIVACY= /TIMESTAMPS /NOTIMESTAMPS /NOLOCATION` (forwarded to
`airview-feeder setup -non-interactive`; the enrol token is masked in the log).

Uninstaller: `uninstall.exe [/S] [/REMOVEDATA] [/LOG=file]`.

Exit codes: 0 done, 1 failed (or a step after the files failed), 2 cancelled,
3 needs administrator rights, 4 bad command line, 3010 done but restart needed.

Log: `%TEMP%\<id>-setup.log` / `%TEMP%\<id>-uninstall.log` (or `/LOG=`).

Test-only folder overrides: `AIRVIEW_SETUP_FOLDER_<TOKEN>` (e.g.
`AIRVIEW_SETUP_FOLDER_PROGRAMS`, `_DESKTOP`) redirect a folder token, so
automated tests leave the real Start menu and desktop alone.

## Tests

- `go test ./internal/wininstall/` — format (incl. signed images, zip slip,
  determinism), product validation of the shipped definitions, argument
  parsing, quoting, token expansion, folder safety, the full engine against a
  fake `System` (install, upgrade with stale-file removal and data kept,
  rollback on a failed copy, cancel, per-user vs per-machine, refusal of
  foreign/system folders, configuration switches, step errors, uninstall with
  and without data), and on Windows the real IShellLinkW round trip and
  registry/SCM/exec basics. `BenchmarkOpenPayload`.
- `deploy\installer\test-installers.ps1 -Scratch <dir> [-Shots]` — real
  installers with a scratch identity, per-user, no admin: silent install,
  verification (SHA-256 vs receipt, shortcuts via WScript.Shell, registry,
  Run value), upgrade keeping data, silent uninstall (keep / remove data),
  clean removal, refusals (no admin → 3, bad switch → 4, drive root → 1),
  the wizard driven through its buttons (install and uninstall), and
  `/SHOTS` screenshots of all pages.

Not covered automatically (needs an elevated session): the per-machine path
(Program Files, HKLM, the feeder service and the firewall rule). The engine
code for it is the same as per-user apart from the `System` calls, which are
unit-tested against the fake; run the release installer once in an
administrator session before publishing (checklist in deploy/installer/README.md).
