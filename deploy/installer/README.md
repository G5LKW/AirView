# AirView Windows installers

`AirView-Setup-<ver>.exe` (station) and `AirView-Feeder-Setup-<ver>.exe`
(feeder; `-arm64` for Windows on ARM) are built by AirView's own installer,
`cmd/airview-setup` — no Inno Setup, nothing downloaded. Engine and file
format: `internal/wininstall` (see its WIRING.md).

## Build

```powershell
powershell -ExecutionPolicy Bypass -File deploy\installer\build-installers.ps1 [-OutDir dir] [-Version x.y.z]
```

Builds the stub, the programs (or takes them from `-BinDir`), generates
THIRD-PARTY-NOTICES.txt from the linked modules' licence files, and writes
the installers to `deploy\installer\out` (never `dist\` unless the Release
stage passes `-ReleaseStage`). `-Resources` regenerates the stub's icon /
version / manifest resources with go-winres.

## Install

Double-click: welcome, licence, destination, options (Start menu, desktop,
service and tray for the feeder, firewall rule), ready, progress, finished.
The setup program itself starts without elevation (asInvoker manifest). For
the default per-machine install to Program Files it relaunches itself and
Windows asks for administrator rights once (also with `/S`); `/CURRENTUSER`
installs into `%LOCALAPPDATA%\Programs` without any UAC prompt, also for a
standard user (no service, no firewall rule). `build-installers.ps1
-AdminStub` builds the old always-elevated variant.

Feeder updates: release builds pass `-ManifestKey` (and `-UpdateUrl`) so the
feeder installer writes `update.keys` / `update.manifestUrl` into
`feeder.json`; without them installed feeders do not update automatically.

Silent:

```
AirView-Setup-1.0.0.exe /S [/NODESKTOP] [/FIREWALL /PORT=8080] [/D=D:\Apps\AirView]
AirView-Feeder-Setup-1.0.0.exe /S /SERVER=feed.example.org [/PIN=sha256/...] [/ENROL=TOKEN] [/NAME=home1]
    [/LAT=51.5 /LON=-0.1 /ALT=35m [/AGL=8m] | /NOLOCATION] [/PRIVACY=hidden|region|cell01] [/NOTIMESTAMPS]
    [/NOSERVICE] [/NOTRAY] [/FIREWALL]
"C:\Program Files\AirView\uninstall.exe" /S [/REMOVEDATA]
```

`/?` lists every switch. Exit codes: 0 done, 1 failed, 2 cancelled, 3 needs
administrator rights, 4 bad command line, 3010 restart needed. Log:
`%TEMP%\AirView-setup.log` (or `/LOG=`).

Upgrades: run the newer installer; it finds the existing folder, closes the
tray, stops the service, replaces the files and keeps every setting. The
uninstaller keeps configuration and data unless "Also delete the
configuration and data" (or `/REMOVEDATA`) is chosen.

## Unsigned (SmartScreen)

The installers are not Authenticode-signed (DESIGN section 19, decision 5).
Windows SmartScreen shows "Windows protected your PC": click "More info",
check the publisher line says "Unknown publisher" and the file name is the one
you downloaded, compare its SHA-256 with the published SHA256SUMS
(`Get-FileHash AirView-Setup-1.0.0.exe`), then "Run anyway". The installer
format already supports signing (the payload is found through the PE security
directory), so a later signed release needs no change.

## Acceptance test

```powershell
powershell -ExecutionPolicy Bypass -File deploy\installer\test-installers.ps1 -Scratch <empty dir> -Shots
```

Builds as-invoker installers with a scratch identity (`AirViewSetupTest`,
`AirViewFeederSetupTest`, scratch data and Start-menu folders) and checks
install, upgrade, uninstall and clean removal without administrator rights;
`-Shots` renders every wizard page to PNG (`/SHOTS=`, nothing installed).

### Manual checklist (administrator session, before a release)

1. `AirView-Setup-<ver>.exe` → wizard → defaults → Finish with "Launch":
   AirView opens; `%APPDATA%\AirView` is created (not `Program Files\AirView\data`).
2. Settings > Apps lists AirView with version, publisher, size and icon.
3. `AirView-Feeder-Setup-<ver>.exe /S`: `sc query AirViewFeeder` RUNNING,
   `HKLM\...\Run` has "AirView Feeder Tray", Start menu "AirView Feeder".
4. Re-run the same installer (upgrade): service restarted, configuration kept.
5. Uninstall both from Settings > Apps (data kept), then reinstall the feeder
   and uninstall with "Also delete the configuration and data":
   `%ProgramData%\AirView Feeder` gone, `sc query AirViewFeeder` fails,
   `netsh advfirewall firewall show rule name="AirView Feeder status page"` finds nothing.
