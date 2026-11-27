# airview-feeder

Headless feeder for AirView networks (docs/aggregator/DESIGN.md §12.1). It reads local
receivers, forwards their frames with receiver timestamps to one or more AirView aggregators
over AVF/1 (`internal/uplink`), delivers the MLAT results it gets back to local programs and
serves a status page on `127.0.0.1:8099` (`internal/feederui`). No SQLite, no GUI.

Builds: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64|amd64 go build ./cmd/airview-feeder`,
`GOARCH=arm GOARM=7` for 32-bit Pi OS, `GOOS=windows`. About 9 MB stripped (`-ldflags=-s -w`).

## Commands

| Command | What it does | Exit codes |
|---|---|---|
| `run` (default) | runs the feeder until SIGINT/SIGTERM; SIGHUP re-reads the configuration | 0 stopped, 1 failed to start |
| `setup` | creates or changes `feeder.json`; interactive, or `-non-interactive` with options | 0, 1 write error, 2 bad option |
| `status [-json] [-url u]` | asks the running feeder (status page API) | 0 all aggregators connected, 1 some not, 3 not running |
| `claim [-uplink id] [-rx n] [-json] [-timeout d]` | prints a claim code. Uses the running feeder; when none runs, opens a short session with the feeder identity itself | 0, 1 |
| `config check` | validates file + environment without changing anything | 0 ok, 1 errors |
| `config print [-show-secrets]` / `get <path>` / `set <path=value>...` / `env` / `example` / `path` | inspect and edit the configuration | |
| `detect [-host h] [-port p] [-json] [-sdr [-sdr-port p]]` | probes 10003, 30005, 30002, 30003 (or one port) and shows what it would use; `-sdr` also probes rtl_tcp (default port 1234), lists RTL-SDR USB dongles with their driver (Windows: WinUSB bound or not) and whether an rtl_tcp program is installed. rtl_tcp is never selected automatically | 0 found (or rtl_tcp found with -sdr), 1 none |
| `new-identity [-yes]` | new feeder key (after cloning an SD card; `cloned_feeder` refusal); keeps the UUID | |
| `uninstall-helper [-json] [-purge -yes]` | lists the feeder's own files (config, `identity.key`, `uuid`, `uplinks/`, lock); `-purge -yes` deletes them. Never touches readsb or other feeders | |
| `update [-check] [-status] [-json] [-channel stable\|beta] [-force] [-manifest-url u] [-key k]... [-health-url u] [-health-window d] [-service name] [-retrust]` | checks for and installs a signed release published by the aggregator (`update.manifestUrl`, `update.keys`), restarts the service and rolls back when the new version does not come up healthy. `-check` only checks; `-status` prints the last check and update. Windows: the release trust anchors are kept under `HKLM\SOFTWARE\AirView Feeder\Update` (only administrators can change them); the first elevated run copies them from `feeder.json`, and `-retrust` (administrator) copies them again after a deliberate change. The status page never changes the manifest URL, the keys or the health URL (the channel and automatic updates remain page settings) | 0 updated or up to date, 1 failed (nothing changed), 2 bad option, 3 not configured, 4 rolled back, 10 newer version available (`-check`) |
| `version`, `--version` | | |
| `help`, `-h`, `--help` | the command overview (`airview-feeder <command> -h` shows the options of a command) | |

Every command takes `-config <file>` and `-data <dir>`.

### setup options (same names as the Pi installer, DESIGN §12.2)

`-server host[:port]` (port 30160), `-pin sha256/...`, `-enrol TOKEN`, `-name NAME`
(`[A-Za-z0-9_-]{1,32}`), `-lat`, `-lon`, `-alt 120m|400ft` (unit mandatory), `-alt-ref msl|hae`,
`-agl 8m`, `-input auto|host[:port]`, `-input-type beast|avr|sbs`, `-privacy hidden|region|cell01`,
`-no-location`, `-no-timestamps` / `-timestamps`, `-public-stats`, `-show-name`,
`-results-port N` (0 = off), `-readsb-push host:port|off`, `-ui-password PW`, `-uplink-id ID`,
`-non-interactive`. Re-running keeps the current values for everything not given.

RTL-SDR (input type `rtltcp`, the user's own rtl_tcp; AirView never ships or downloads it):
`-input 127.0.0.1:1234 -input-type rtltcp`, `-sdr-gain max|auto|<dB>`, `-sdr-ppm N` (-500..500),
`-sdr-sample-rate N` (2000000..3200000), `-sdr-bias-tee`, `-sdr-spawn`, `-sdr-rtltcp-path FILE`,
`-sdr-device N`, `-sdr-accept-unverified-ap`. They need an rtltcp input.

Also feed other aggregators (Beast outputs, `uplinks[].type = "beast"`): `-beast-out host[:port],...`
(port 30004; the same address again updates the entry, `off` removes every Beast output),
`-beast-format beast_reduce_plus_out|beast_reduce_out|beast_out`, `-beast-uuid UUID`; `-timestamps` /
`-no-timestamps` apply to the given outputs too. `-server` always changes an AirView (AVF/1) entry, never a
Beast output. The
interactive mode shows the consent text of DESIGN §10.5 before asking about timestamps and
when the position is left blank.

## Files and defaults

| | Linux (system install or root) | Linux (user) | Windows |
|---|---|---|---|
| config | `/etc/airview-feeder/feeder.json` | `~/.config/airview-feeder/feeder.json` | `%ProgramData%\AirView Feeder\feeder.json` |
| data | `/var/lib/airview-feeder` | `~/.config/airview-feeder` | `%ProgramData%\AirView Feeder` |

`AIRVIEW_FEEDER_CONFIG` and `AIRVIEW_FEEDER_DATA` override the defaults, `-config`/`-data`
override those. The data directory holds `identity.key` (0600), `uuid`, `uplinks/<id>/`
(state, spool) and `feeder.lock` (Linux: an flock that stops a second process with the same
identity). `feeder.json` is written 0640 (an existing file keeps its mode and group).

## Configuration

`model.FeederConfig` (`airview-feeder config example` prints every setting with its default).
Layers, in increasing priority: defaults < `feeder.json` (JSON; comments accepted on read) <
`AIRVIEW_FEEDER_*` environment variables < `-set path=value` (run). Names: the JSON path in
upper snake case, list elements by index: `AIRVIEW_FEEDER_UPLINKS_0_HOST`,
`AIRVIEW_FEEDER_UPLINK_TUNING_RING_SEC`, `AIRVIEW_FEEDER_UI_ADDR`. `<NAME>_FILE` reads the value
from a file (secrets such as `..._ENROL_TOKEN_FILE`). An index one past the end appends an
element (`AIRVIEW_FEEDER_UPLINKS_0_HOST` works without an uplink in the file). Environment
and `-set` values are "locked": the status page and `config set` write only the file.
`ui.passwordHash` accepts a plain password in `config set` and on the page (it is hashed).

Inputs: `receivers[].input.type` is `auto` (default), `beast`, `avr`, `sbs` or `json`. `auto`
probes 127.0.0.1 (or `input.host`, or only `input.port` when set) every 5 s at first, then
every 15 s; a detected receiver that is unreachable for 2 minutes is looked for again.
Preference: Beast data, AVR data, a listening quiet port (not SBS), SBS (map only). The feed
detects the clock class itself (Radarcape status frames, else timestamp statistics; readsb
gives `sdr12`); `clockClass` overrides it.

## Status page and API (`internal/feederui`)

`GET /` page, `GET /api/status`, `GET /api/health` (200 when every uplink is connected,
else 503; for installer post-checks), `POST /api/claim {"id","rxNo"}`, `POST /api/retry {"id"}`,
`GET|PUT /api/settings`. Mutating requests need the header `X-AirView-Feeder: 1`; host names
other than IP literals and `localhost` are refused (DNS rebinding); non-loopback clients need
`ui.passwordHash` (HTTP Basic, any user name). The page cannot change `ui.*` itself.

## MLAT results

Results are re-encoded as DF17 messages (even + odd airborne position with GNSS height, type
code 20/21/22 by uncertainty, plus airborne velocity when known) in Beast format with the MLAT
magic timestamp, and served on `127.0.0.1:<results.beastPort>` (default 30105; readsb:
`--net-connector 127.0.0.1,30105,beast_in`) and, with `results.readsbAddr`, pushed into a readsb
Beast input. They are never uploaded again.

## Linux service notes

Runs in the foreground, logs to stderr without timestamps under systemd (journald adds them),
supports `Type=notify` (READY/STOPPING/WATCHDOG over `$NOTIFY_SOCKET`) and `ExecReload=kill -HUP
$MAINPID`. Needs no capabilities (no privileged ports). On SIGTERM the uplinks flush, send
GOODBYE and spill unsent data to the spool.

## Tests

`go test ./cmd/airview-feeder ./internal/feederui` (end-to-end against
`internal/uplink/avftest`: detect, connect, frames, status page, CLI status/claim, results
output, settings save, offline claim). Live: `internal/uplink/cmd/avfstub` +
`internal/sim/simnet/cmd/simnet-serve` + the built binary.
