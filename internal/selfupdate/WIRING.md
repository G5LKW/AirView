# internal/selfupdate - wiring

Signed self-update for `airview` and `airview-feeder` (WP-UP). Package
overview: `doc.go`. Release tool and key handling: `deploy/release/README.md`.

Nothing outside this package calls it yet. The sections below say what each
owner needs to add; the requests are also filed in
`docs/aggregator/HOOKS.md` ("WP-UP-selfupdate -> ...").

## 1. Files and formats

| File | Where | Content |
|---|---|---|
| `manifest.json` | release dir (served at `/feeder/manifest.json`), beta below `beta/` | `{"schema":1,"product","version","channel","published","expires"?,"files":[{"target","url"?,"sha256","size"}],"notes"?}`, canonical layout from `Manifest.Marshal` (one flat object per file, `version` before `files`, `{`/`}` escaped in strings) |
| `manifest.json.sig` | next to it | base64 (standard, padded) of the 64-byte Ed25519 signature over the exact manifest bytes, trailing newline |
| `dl/<target>` | next to it | the artifact; targets `linux-amd64`, `linux-arm64`, `linux-armv7`, `windows-amd64`, `windows-arm64`, ... |
| public key | configs, installers | base64 of the raw 32-byte key (44 chars), e.g. `update.keys`, `--manifest-key`, `PRESET_MANIFEST_KEY` |
| `selfupdate.json` | StateDir (the data dir) | journal of the last transaction (`phase`: `swapped` / `committed` / `rolledback`) |
| `selfupdate-status.json` | StateDir | last check, error, available version, newest accepted publication time per channel (anti-replay) |
| `selfupdate.lock`, `selfupdate.boots`, `download/` | StateDir | transaction lock, trial start-up counter, staging |
| `<exe>.old`, `<exe>.failed`, `<exe>.*.del` | next to the program | previous binary during the trial, rejected binary after a rollback, files Windows could not delete yet |

File URLs in the manifest are relative to the manifest (`dl/<target>`) by
default, so the same release directory works at `/feeder/` and below
`/feeder/beta/`, and the Pi installer resolves them the same way.

## 2. Configuration (`update` block) - owner: internal/model + internal/config

`selfupdate.Settings` is the block, JSON names as they should appear in the
file:

```jsonc
"update": {
  "enabled": false,            // automatic check-and-install (opt-in for airview)
  "channel": "stable",         // stable | beta
  "manifestUrl": "https://aggregator.example/feeder/manifest.json", // "{channel}" allowed
  "keys": ["<base64 32-byte Ed25519 public key>"],                  // several = rotation
  "checkIntervalMin": 1440,    // 15..44640, jittered +-25 %
  "healthWindowS": 60,         // 10..900
  "healthUrl": "",             // default chosen by the program (see below)
  "maxDownloadMB": 256         // 1..4096
}
```

- model is a leaf package, so it mirrors the struct (`model.UpdateSettings`
  with the same JSON tags) in `Config.Update` and `FeederConfig.Update`; the
  wiring converts with a field-by-field copy.
- Environment: `Settings.ApplyEnv("AIRVIEW_UPDATE_", os.Getenv)` for airview,
  `ApplyEnv("AIRVIEW_FEEDER_UPDATE_", ...)` for the feeder (names in
  `selfupdate.EnvNames`: ENABLED, CHANNEL, MANIFEST_URL, KEYS,
  CHECK_INTERVAL_MIN, HEALTH_WINDOW_S, HEALTH_URL, MAX_DOWNLOAD_MB). If the
  generic config layer already maps `AIRVIEW_*` onto JSON paths, that is
  enough and ApplyEnv is not needed.
- `config check`: append `Settings.Check()` (messages are prefixed
  `update.`).
- Feeder: `FeederConfig.AutoUpdate` (default true) maps onto `Enabled`, but
  only when `manifestUrl` and `keys` are set (otherwise Check would fail);
  the Pi/Windows installers write both (they already know the aggregator URL
  and `PRESET_MANIFEST_KEY`).

## 3. airview-feeder - owner: WP-FB (cmd/airview-feeder)

**`update` subcommand** (replace the stub `cmdUpdate`):

```go
code := selfupdate.RunCommand(args, stdout, stderr, selfupdate.CommandOptions{
	Product: "airview-feeder", Name: "airview-feeder", Version: version,
	Settings: settings,            // from feeder.json + env
	StateDir: dataDir,             // /var/lib/airview-feeder, %ProgramData%\AirView Feeder
	Service:  defaultService,      // "airview-feeder" (systemd) / "AirViewFeeder" (Windows)
	HealthURL:    "http://127.0.0.1:8099/api/health",   // from ui.addr
	HealthAccept: []int{200, 503}, // 503 = up, an uplink not connected yet
	HealthHeader: http.Header{"X-AirView-Feeder": {"1"}},
})
```

Exit codes: 0 updated / up to date, 1 failed (nothing changed), 2 usage,
3 not configured (same as the stub today, so `feeder_test.go` line ~250 stays
valid until the config carries a URL), 4 rolled back, 10 `-check` found a
newer version. Options: `-check -status -json -force -channel -manifest-url
-key -service -health-url -health-window`. It must run as root/administrator
(it replaces the program file and restarts the service).

**`run` / the Windows service body** (in-process; this is what the tray's
"Check for updates" and Windows auto-update use):

```go
att, err := selfupdate.Attach(selfupdate.Options{
	Product: "airview-feeder", CurrentVersion: version, Settings: settings,
	StateDir: dataDir, Logf: logf,
	Restarter: restarter, // see below
})
if errors.Is(err, selfupdate.ErrRolledBack) { return exit(75) } // supervisor starts the restored binary
if err != nil { logf("self-update unavailable: %v", err) }      // never fatal
// ... start the feeder ...
go func() {
	err := att.Run(ctx, func(ctx context.Context) error { return f.selfCheck(ctx) })
	if errors.Is(err, selfupdate.ErrRolledBack) || errors.Is(err, selfupdate.ErrExitRequested) {
		f.requestExit(1) // non-zero: systemd Restart= / SCM recovery starts the program file again
	}
}()
```

- Call `Attach` before listeners open (on Windows it waits for the previous
  process, `AIRVIEW_SELFUPDATE_WAIT_PID`).
- `selfCheck` should fail only when the program is really broken (the UI
  listener cannot answer, the uplink goroutines died); "aggregator
  unreachable" is not a reason to roll back.
- Restarter for the in-process loop:
  - Windows service: `selfupdate.WindowsServiceRestarter{Name: svcName}`
    cannot be used from inside the service it stops. Use
    `SelfRestarter{Exe, Exit: stopService}` with the SCM recovery action
    "restart on failure" and exit non-zero, or (simpler) leave
    `update.enabled` to a scheduled task that runs `airview-feeder update`.
  - Linux under systemd: the service user cannot write `/opt/airview-feeder`,
    so automatic installs come from the root timer (below). Keep `Attach` +
    `Run` in `run` anyway: it confirms a version installed by the timer and
    rolls back a crash loop. Set `Restarter` to `SelfRestarter{Exe: exe}`
    (execve keeps the PID, systemd does not notice) for installs where the
    directory is writable.
- Status page: `att.Status()` (product, version, channel, enabled, last
  check/error, available version, last transaction) for `/api/status` and the
  tray's "Check for updates" item (`att.Updater().Check(ctx)`).

## 4. airview (main program) - owner: WP-E (cmd/airview) / integrator

Same pattern with `Product: "airview"`, `StateDir: <data dir>`, `Service:
"airview"` (systemd unit of deploy/server), `HealthURL:
"http://127.0.0.1:<http port>/healthz"` (200 = up; the JSON carries
`version` and `uptimeS`, which the probe uses). Register `airview update` as a
subcommand calling `RunCommand`. `update.enabled` stays false by default
(opt-in, DESIGN §19/§20). On the desktop (Windows, no service) the in-process
loop uses `SelfRestarter{Exe, Exit: cancelMain}`: Restart starts the new
process and returns `ErrExitRequested`; the program must then return from
`main` promptly so the new process gets the ports.

## 5. Aggregator `/feeder/*` routes - owner: WP-H (internal/server)

```go
h := http.StripPrefix("/feeder", selfupdate.ReleaseHandler{Dir: releaseDir})
// GET|HEAD /feeder/manifest.json, /feeder/manifest.json.sig, /feeder/dl/<target>,
//          /feeder/beta/manifest.json(.sig), /feeder/beta/dl/<target>
```

- Public, not rate-limited (artifacts are large; `http.ServeContent` gives
  Range/resume, `Cache-Control: no-cache`, `nosniff`). Only those names are
  served; anything else is 404, and a manifest without its `.sig` is 404.
- `releaseDir`: suggested setting `aggregator.releaseDir` (default
  `<data>/releases/feeder`). The operator fills it with
  `release publish -product airview-feeder -out <releaseDir> ...` on the
  release machine and copies it (rsync) to the server; the server never
  holds the private key.
- `install.sh` preset `PRESET_MANIFEST_KEY`: the first entry of the
  aggregator's own `update.keys` (same release key signs airview and
  airview-feeder), or a dedicated `aggregator.releaseKey` if the operator
  publishes the feeder with a different key.

## 6. Installers

- Pi (WP-PI): `update.sh` re-runs the installer with `--update`. Once the
  feeder's `update` subcommand is wired, the timer can run
  `airview-feeder update -service airview-feeder` instead, which adds the
  60 s health check and automatic rollback; the installer should write
  `update.manifestUrl` (`<URL>/feeder/manifest.json`) and `update.keys`
  into feeder.json.
- Windows feeder (WP-WF): the installer writes the same two settings; a
  scheduled task (SYSTEM, daily, random delay) running
  `airview-feeder.exe update -service AirViewFeeder` gives the
  rename-aside swap, SCM restart, health check and rollback.

## 7. Tests that pin the contract

- `TestTamperedManifestRejected`, `TestSignVerifyAndTamper`,
  `TestParseManifestStrict`: tampered manifests / signatures are refused and
  nothing changes on disk.
- `TestIntegrationRollback`: real binaries (testdata/scratchapp) under a
  supervisor; crash, flaky and sick builds are rolled back and the previous
  version runs again, a good build is kept (supervised mode).
- `TestIntegrationSelfConfirm`: real binaries (testdata/selfapp, using
  Attach/Run) under a restarting supervisor; a crash-looping build is rolled
  back by its own Recover, a build whose self check fails by Trial.Confirm, a
  good one confirms itself (self-confirm mode).
- `TestManifestInstallerContract`, `deploy/release TestInstallerCompat`: the
  Pi installer's awk/OpenSSL code accepts our manifests and rejects a
  tampered one.
