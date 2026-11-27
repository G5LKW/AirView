# AirView Feeder for Windows: packaging

DESIGN §12.3, work package WP-WF. The program itself is `cmd/airview-feeder` (Windows parts: `service_windows.go`, `tray_windows.go`, `winacl_windows.go`, `winlock_windows.go`, `logfile_windows.go`, `winmain_windows.go`); the tray library is `internal/wintray`. This folder holds what turns it into something a Windows user installs.

| File | What it is |
|---|---|
| `build.ps1` | Builds amd64 and arm64: `airview-feeder.exe` (console), `airview-feeder-tray.exe` (the same program linked with `-H windowsgui`), the icon, the portable ZIPs and, when Inno Setup 6.3+ is installed, the installers. Optional Authenticode signing. Writes to `deploy\feeder\windows\out` unless `-OutDir` is given; refuses the repository's `dist` folder. |
| `airview-feeder.iss` | Inno Setup script (per-machine installer). |
| `installer/README.txt` | Installed next to the programs. |
| `portable/` | Added to the portable ZIP: `README.txt`, `Install service.cmd`, `Remove service.cmd`. |
| `mkicon/` | Writes the multi-size `.ico` from `internal/wintray`'s icon drawing (no image files in the repository). |
| `iss_test.go` | Checks the packaging against the program: every command and flag the installer and scripts use exists and works, C1/C2 consent texts are word for word, shared names (service, tray autostart value, data folder) match. |

```powershell
powershell -ExecutionPolicy Bypass -File deploy\feeder\windows\build.ps1 [-Version 1.2.3] [-Arch amd64,arm64]
           [-OutDir dir] [-NoInstaller] [-Iscc 'C:\...\ISCC.exe'] [-CertThumbprint <sha1> [-SignTool path] [-TimestampUrl url]]
```

Output (`<v>` = version, `<a>` = amd64 | arm64):

- `AirView-Feeder-<v>-windows-<a>.zip`: one folder `AirView-Feeder-<v>-windows-<a>\` with `airview-feeder.exe`, `airview-feeder-tray.exe`, `airview-feeder.ico`, `README.txt`, `Install service.cmd`, `Remove service.cmd`.
- `AirView-Feeder-Setup-<v>-windows-<a>.exe` (only with Inno Setup).
- `stage-<a>\`: the files the installer packs.

Sizes (0.1.0): each exe 9.9 MB (budget 15 MB), ZIP 7.8 MB (amd64) / 7.0 MB (arm64).

## The two programs

`airview-feeder-tray.exe` is byte-for-byte the same package as `airview-feeder.exe`, linked as a GUI program so that starting it at log-on opens no console window. A program whose name contains `-tray` starts the tray when run without a command; the service and the installer always use the console `airview-feeder.exe` (`siblingExe`).

## Installer

Per-machine, administrator rights, Windows 10 or newer, amd64 (`x64compatible`, which also runs on arm64 under emulation) or arm64 (native).

Pages after the usual ones:

1. **Tasks:** Windows service (default on), tray icon at log-on for every user (default on, `HKLM\...\Run`, value `AirView Feeder Tray`), automatic signed updates (default on, with the service: scheduled task *AirView Feeder update*, SYSTEM, daily at a random time 01:00-05:59, running `airview-feeder.exe update -service AirViewFeeder`; it does nothing (exit 3) until `update.keys` exists; without `update.manifestUrl` the feeder uses `https://<aggregator host>/feeder/manifest.json`, and the per-machine installer seals both in `HKLMSOFTWAREAirView FeederPDATE`), status page on the LAN (default off).
2. **Aggregator:** server, server pin, enrol token, station name. Leaving the server empty skips the next pages: the tray then opens the status page's Settings on first run (browser-based setup). On an upgrade an empty server keeps the existing configuration unchanged.
3. **Antenna position:** latitude, longitude, altitude with unit, height above ground, or "Leave blank, let the network locate me". C1 is shown on this page.
4. **Timestamps:** C1, then *Send timestamps (MLAT)* / *No timestamps (no MLAT)*.
5. **Public map:** C2 word for word, Hidden (default) / Region / Area.
6. **Status page password** (only with the LAN task): at least 8 characters, stored hashed (`config set ui.passwordHash=…`); `ui.addr` becomes `0.0.0.0:8099` and a firewall rule *AirView Feeder status page* (TCP 8099, private and domain profiles, this program only) is added. Nothing is opened in the firewall otherwise.

When the user leaves the public-map page, the installer runs the real `airview-feeder setup -non-interactive …` against a scratch configuration in its temp folder, so a mistyped value is reported while it can still be fixed (setup exits 2 and says why).

After copying the files:

1. `airview-feeder setup -non-interactive -config "%ProgramData%\AirView Feeder\feeder.json" -data "%ProgramData%\AirView Feeder" -server … -privacy …` (only when a server was entered).
2. LAN task: `config set` and the firewall rule.
3. Service task: `airview-feeder service install -name AirViewFeeder -now`: account `NT SERVICE\AirViewFeeder`, automatic start, restart on failure (5 s, 5 s, 60 s), interactive users may pause, resume and restart it without UAC, the data folder is restricted to SYSTEM, Administrators and the service (logs also readable by users), Event Log source registered. Without the service task an existing service is removed, so the tray can run the feeder itself.
4. Optional: start the tray now, open the status page.

Upgrade: before replacing files the installer closes the trays of the current session (`tray -signal quit`) and stops the service; step 3 starts it again. Configuration, identity, spool and logs stay.

Silent install, for deploying many feeders:

```
AirView-Feeder-Setup-<v>-windows-amd64.exe /VERYSILENT /SUPPRESSMSGBOXES /SERVER=feed.example.org /PIN=sha256/... /ENROL=TOKEN /NAME=home1 /LAT=51.4775 /LON=-0.4614 /ALT=35m /AGL=8m /PRIVACY=hidden [/NOTIMESTAMPS] [/NOLOCATION] [/TASKS="service,trayautostart"] [/LOG="setup.log"]
```

Bad values make the silent install stop before anything is changed (PrepareToInstall runs the same validation). The status page password is never written to the setup log.

Uninstall: closes the tray, removes the service and the firewall rule, removes the autostart, and asks whether to delete the configuration, identity and logs (default **No**: a reinstall continues as the same station; a silent uninstall always keeps them). Program files a tray of another logged-on user still holds are deleted at the next restart.

### Inno Setup is not part of the build machine

`ISCC.exe` (Inno Setup 6.3 or newer) was not installed where this was written, so the script has been checked by `iss_test.go` (names, texts, every command line it runs, executed against the real program) but **not compiled**. To produce the installers: install Inno Setup 6.3+ from its official site, then run `build.ps1` again (or `ISCC.exe /DArch=amd64 /DAppVersion=<v> /DSrcDir=<out>\stage-amd64 airview-feeder.iss`). The script stops with a clear error on an older Inno Setup.

## Portable ZIP

No administrator rights needed. Unzip, start `airview-feeder-tray.exe`: with no service installed the tray runs the feeder itself (in the tray process, as the logged-on user, data in `%ProgramData%\AirView Feeder`; a data folder it creates is made private to that user). The first time, the settings page opens in the browser. The tray menu offers *Install as Windows service…* (UAC prompt), which moves the same configuration and identity into the service; `Install service.cmd` does the same from Explorer. Other locations: `airview-feeder-tray.exe -config <file> -data <dir>` (the tray's *Start with Windows* keeps those flags).

## Tray

`airview-feeder-tray.exe` (or `airview-feeder tray`): one per user session and service name. Menu: Open status page (default, double-click), Settings…, Get claim code… (copied to the clipboard), Pause feeding / Resume feeding / Start feeder, Restart feeder, Install as Windows service… (only without the service), Open logs folder, Check for updates…, Start with Windows, Quit tray. The icon's badge is green (feeding), amber (starting, connecting, receiver missing, no aggregator configured), red (refused by the aggregator, service failed; also a notification) or grey (paused, stopped, not installed); the tooltip shows the state and msg/s and aircraft. `-signal quit|menu|refresh` talks to a running tray (installers, tests).

## SmartScreen and signing

Unsigned programs and installers downloaded from the internet get the SmartScreen warning "Windows protected your PC" until they have built up reputation. Users then click *More info* → *Run anyway*; the READMEs say so, and that they should first check the file came from their network's own page. For a ZIP, *Properties* → *Unblock* before unzipping clears the mark of the web for every file inside.

- With a code-signing certificate, `build.ps1 -CertThumbprint <sha1>` signs both executables before they are zipped or packed, and the installer and uninstaller through Inno Setup's `SignTool` (`/DSign=1`). An OV certificate still needs reputation; an EV certificate is trusted by SmartScreen from the first download.
- Hashes: `build.ps1` prints the SHA-256 of every ZIP and installer; publish them next to the downloads.
- The release decision for this project (DESIGN §19) is **unsigned**.

## Not done here

- The executables carry no embedded icon or version resource: generating a `.syso` needs a resource compiler or a third-party module, and the `.syso` would live in `cmd/airview-feeder` (HOOKS.md, WP-WF → WP-HL). Shortcuts and *Apps & features* use `airview-feeder.ico`.
- `deploy/build.ps1` (the release build) does not call this script yet (HOOKS.md).
- Self-update (`airview-feeder update`) belongs to WP-UP/WP-FB and is a stub in this build. The tray's *Check for updates…* already runs it elevated for the service and reads its exit code; until it is wired the tray explains how to update by hand. The updater must also refresh `airview-feeder-tray.exe` (HOOKS.md).
