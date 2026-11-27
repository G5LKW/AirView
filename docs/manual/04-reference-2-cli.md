# 4.2 Command-line reference

AirView ships two programs and a few helpers:

| Program | Where | What it is |
|---|---|---|
| `airview` (`airview.exe`) | Windows, Linux, Raspberry Pi, Docker | The station, the network aggregator and every maintenance command. |
| `airview-feeder` (`airview-feeder.exe`) | Linux, Raspberry Pi, Windows | The lightweight feeder that sends a receiver's data to an AirView aggregator. |
| `airview-healthcheck` | Docker image | Health probe for the container `HEALTHCHECK`. |
| `install.sh` scripts, `AirView-Setup-*.exe` | Linux, Pi, Windows | Installers; their options are listed in 4.2.4 – 4.2.6. |

The option texts below are the programs' own `-h` output of version 1.0.0. Every program prints them with `-h` (Windows installers: `/?`). Options take one dash or two (`-data` = `--data`); a value follows with a space or `=`.

---

## 4.2.1 `airview`

```text
airview [options]                 run the station (desktop window on Windows unless -headless)
airview serve [options]           run headless (servers, systemd, Docker); same options
airview config check|print|example|env [options]
airview admin init|reset [-data dir] [-config file] [-user name]
airview backup [-data dir] [-o file.zip]
airview restore [-data dir] [-force] file.zip
airview db snapshot [-data dir] -o outdir
airview update [-data dir] [-config file] [-check] ...
airview federation id [-data dir] [-config file]
airview version [-json]
```

Run `airview <command> -h` for the options of a command.

### Running the station or aggregator

`airview` without a command runs AirView. On Windows it opens the AirView window; with `-headless`, or as `airview serve`, it runs without a window (a server). Outside Windows it is always headless.

| Option | Meaning |
|---|---|
| `-config file` | Operator configuration file (JSONC). Default: `$AIRVIEW_CONFIG`, else `airview.jsonc` in the data directory when present. |
| `-data directory` | Data directory. Default: `$AIRVIEW_DATA`, else `data` next to the executable (when writable), else the user configuration directory. |
| `-set path=value` | Override one setting (repeatable), e.g. `-set http.port=8080`. See 4.1. |
| `-port port` | Override the HTTP port from the configuration (same as `-set http.port=…`). |
| `-demo` | Force the built-in traffic simulator feed (standalone). |
| `-headless` | Run without a window (always the case outside Windows). |
| `-open` | Open the default browser on the web interface (headless). |
| `-listen-host address` | Aggregator mode: address the feeder listeners bind to. Default: `$AIRVIEW_LISTEN_HOST`, else every interface. |
| `-version` | Print the version and exit. |

When it starts, AirView prints the address of the web interface (`AirView is running: http://…/`). In standalone mode, when `http.port` is in use it falls back to 8080, 8081 … 8089 and then to a free port, and logs a warning. A headless **aggregator** never falls back: it stops with an error so that systemd and your reverse proxy never point at the wrong service.

Examples:

```sh
airview serve -data /var/lib/airview -config /etc/airview/airview.jsonc
airview -headless -demo -port 8080 -open              # try it without a receiver
airview.exe -data D:\AirView-test\data -set http.port=18080
```

### `airview config`

| Command and options | Meaning |
|---|---|
| `config check` | Validate every configuration layer without changing anything. Exit status 0 = valid (warnings allowed), 1 = errors, 2 = usage error. |
| `config print` | Effective configuration, secrets redacted. |
| `config example [-o file]` | Fully commented example file (every setting at its default); `-o` writes to a file and refuses to overwrite. |
| `config env [-json]` | Every `AIRVIEW_*` environment variable with its setting and type. |

Options of `config check` and `config print`:

| Option | Meaning |
|---|---|
| `-config file` | Operator configuration file (JSON with comments). |
| `-data directory` | Data directory holding `config.json`. |
| `-set path=value` | Override a setting (repeatable). |
| `-no-config` | Ignore the operator configuration file. |
| `-no-env` | Ignore `AIRVIEW_*` environment variables. |
| `-json` | Machine-readable JSON output. |
| `-q` | (`check` only) Print errors only. |
| `-sources` | (`print` only) Annotate overridden settings with their origin (JSON with comments). |

### `airview admin init` / `airview admin reset`

Create the first administrator account of an aggregator (`init`), or reset an administrator's password and authenticator (`reset`, the break-glass procedure). Run on the server, as the service account, while AirView may be running.

| Option | Meaning |
|---|---|
| `-config file` | Operator configuration file. Default: `$AIRVIEW_CONFIG`. |
| `-data directory` | Data directory. Default: `$AIRVIEW_DATA`, else the default data directory. |
| `-user name` | Admin user name (default `admin`). |

### `airview backup`

Writes a consistent backup (databases, settings, uplink identity, AVF TLS certificate). Safe while AirView is running.

| Option | Meaning |
|---|---|
| `-data directory` | Data directory. |
| `-o file` | Output file. Default: `airview-backup-<UTC time>.zip` in the current directory; `-` = standard output. |
| `-tmp directory` | Where the database snapshots are staged while the archive is written. Default: the data directory; it needs free space for a copy of `airview.db` and `stations.db`. |

### `airview restore`

Restores a backup made by `airview backup`. **Stop AirView first.** Every file is checked against the archive's manifest; files that are replaced are moved to `<data>/pre-restore-<time>/`, nothing is deleted.

| Option | Meaning |
|---|---|
| `-data directory` | Data directory to restore into. |
| `-force` | Restore even if the data directory looks in use (a stale `airview.run`). |

### `airview db snapshot`

Writes a consistent copy of every SQLite database (`*.db`) of the data directory into an existing output directory (with `VACUUM INTO`). Safe while AirView is running.

| Option | Meaning |
|---|---|
| `-data directory` | Data directory. |
| `-o directory` | Output directory (must exist; one `<name>.db` per database; the files must not exist yet). |

### `airview update`

Checks for and installs a signed release (see the `update` settings in 4.1.9).

| Option | Meaning |
|---|---|
| `-check` | Only check whether a newer version exists (exit status **10** if so). |
| `-status` | Print the state of the last check and update. |
| `-channel stable\|beta` | Release channel (default: `update.channel`). |
| `-force` | Install the channel's version even if it is not newer (reinstall / downgrade). |
| `-manifest-url string` | Manifest URL (default: `update.manifestUrl`). |
| `-key value` | Trusted release public key, base64 (repeatable; default: `update.keys`). |
| `-service string` | Service to restart and supervise (systemd unit on Linux, service name on Windows). |
| `-health-url string` | Health URL polled after the restart. |
| `-health-window duration` | How long the new version must stay healthy (default: `update.healthWindowS`). |
| `-json` | Machine-readable output. |
| `-data`, `-config` | As above. |

### `airview federation id`

Prints this aggregator's federation identity, which you give to the operator of a peer network (see the operator guide, federation). Options `-data` and `-config`.

### `airview version`

```text
$ airview version
AirView 1.0.0 (windows/amd64, go1.27.3)
$ airview version -json
{ "app": "AirView", "version": "1.0.0", "os": "windows", "arch": "amd64", "go": "go1.27.3", "gui": true }
```

---

## 4.2.2 `airview-feeder`

```text
airview-feeder <command> [options]

  claim             get a claim code to link this feeder to your account
  config            check|print|get|set|env|example|path: work with feeder.json
  detect            look for receivers on this computer or a given host
  help              show this help
  new-identity      create a new feeder key (after cloning an SD card); keeps the UUID
  run               run the feeder (default)
  service           (Windows) install|remove|start|stop|pause|continue|restart|status|run
  setup             create or change feeder.json (interactive, or --non-interactive)
  status            show the state of the running feeder
  tray              (Windows) the notification-area icon (status, pause, logs, settings)
  uninstall-helper  list (or with -purge -yes delete) the feeder's own files
  update            check for and install a signed update published by the aggregator
  version           print the version
```

**Common options** of most commands:

| Option | Meaning |
|---|---|
| `-config file` | Configuration file. Default: `$AIRVIEW_FEEDER_CONFIG`, else the platform default (4.1.10). |
| `-data directory` | Data directory for the identity, state and spool. Default: `$AIRVIEW_FEEDER_DATA`, else the platform default. |

### `run`

Runs the feeder (also what `airview-feeder` does without a command).

| Option | Meaning |
|---|---|
| `-set path=value` | Override one setting (repeatable), e.g. `-set ui.addr=127.0.0.1:8100`. |
| `-ui address` | Status page listen address (overrides `ui.addr` for this run). |

### `setup`

Creates or changes `feeder.json`. Without `-non-interactive` it asks for every value; with it, the options and the current values are used.

| Option | Meaning |
|---|---|
| `-server host[:port]` | Aggregator address (default port 30160). |
| `-pin sha256/...` | Aggregator server pin (from the aggregator's install command). |
| `-enrol token` | Enrol token from the aggregator operator. |
| `-uplink-id id` | Id of the aggregator entry to create or change (default: the first). |
| `-name name` | Station name (`[A-Za-z0-9_-]`, at most 32). |
| `-lat degrees`, `-lon degrees` | Antenna position, decimal degrees. |
| `-alt 120m` | Antenna altitude with unit, e.g. `120m` or `400ft`. |
| `-alt-ref msl` | Altitude reference: `msl` (default) or `hae`. |
| `-agl 8m` | Antenna height above ground, e.g. `8m` or `25ft`. |
| `-no-location` | Do not declare a position (the network locates the station). |
| `-no-timestamps` / `-timestamps` | Send no timestamps (no MLAT, no timing-based location) / send them again. |
| `-privacy hidden` | Public map pin: `hidden` (default), `region` or `cell01`. |
| `-public-stats` | Publish non-locating statistics of the station. |
| `-show-name` | Show the station name publicly. |
| `-input auto` | Receiver: `auto` (default), or `host[:port]` of its Beast/AVR/SBS output. |
| `-input-type type` | Input type for `host[:port]`: `beast`, `avr` or `sbs` (default: detect). |
| `-results-port port` | Local Beast port for MLAT results (0 = off; default 30105). |
| `-readsb-push host:port` | Also push MLAT results into readsb at `host:port` (e.g. `127.0.0.1:30104`; `off` to stop). |
| `-beast-out host[:port],...` | Also feed other aggregators: Beast output(s) (default port 30004; `off` removes all). |
| `-beast-format format` | `beast_reduce_plus_out` (default), `beast_reduce_out` or `beast_out`. |
| `-beast-uuid UUID` | Station UUID sent to the Beast aggregator(s) (default: a random one per output). |
| `-sdr-gain max` | RTL-SDR gain: `max` (default), `auto` or dB such as `40.2` (input type `rtltcp`). |
| `-sdr-ppm ppm` | RTL-SDR oscillator correction (−500..500). |
| `-sdr-sample-rate rate` | Samples/s (2000000..3200000, default 2400000). |
| `-sdr-bias-tee` | Switch on the dongle's antenna supply. |
| `-sdr-spawn` | Start your own `rtl_tcp` as a child process (never shipped or downloaded by AirView). |
| `-sdr-rtltcp-path executable` | `rtl_tcp` for `-sdr-spawn` (default: search PATH and the program folder). |
| `-sdr-device index` | Dongle index for a spawned `rtl_tcp` (0..31). |
| `-sdr-accept-unverified-ap` | Pass address/parity replies of unconfirmed addresses (not recommended). |
| `-ui-password password` | Status page password (needed only for a LAN address). |
| `-non-interactive` | Do not ask. |

### `status`

Shows the state of the running feeder, read from its status page.

| Option | Meaning |
|---|---|
| `-json` | Print the raw status JSON. |
| `-url URL` | Status page URL (default: from `ui.addr`). |

Exit status: 0 = every aggregator connected, 1 = at least one is not connected, 3 = the feeder is not running (or its status page is not reachable).

### `claim`

Gets a claim code to link this feeder's station to your account on the aggregator's website.

| Option | Meaning |
|---|---|
| `-uplink id` | Aggregator (uplink id; default: the first). |
| `-rx number` | Receiver number (default: the first). |
| `-timeout duration` | How long to wait for the aggregator (default 30s). |
| `-json` | Print `{"code","expiresMs"}` as JSON. |

### `detect`

Looks for receivers on this computer or a given host.

| Option | Meaning |
|---|---|
| `-host host` | Receiver host (default 127.0.0.1). |
| `-port port` | Probe only this port. |
| `-wait duration` | How long to listen to each port for data (default 2s). |
| `-sdr` | Also look for an RTL-SDR: `rtl_tcp` on the host, USB dongles and their driver. |
| `-sdr-port port` | `rtl_tcp` port for `-sdr` (default 1234). |
| `-json` | Print JSON. |

### `config`

`airview-feeder config check|print|get <path>|set <path=value>...|env|example|path [-config file] [-data dir]` – see 4.1.10. `config print -show-secrets` prints enrol tokens and the password hash.

### `new-identity`

Creates a new feeder key after an SD card was cloned; the UUID is kept. `-yes` skips the confirmation. Afterwards claim the station again.

### `update`

Same options as `airview update` (4.2.1); `-service` defaults to `AirViewFeeder`. `-check` exits with status 10 when a newer version exists. On Windows the daily "AirView Feeder update" task installs updates; on a Raspberry Pi installed with the installer script, its daily timer does (`sudo bash install.sh --update`).

### `uninstall-helper`

Lists the feeder's own files (configuration, identity, state, spool). `-purge -yes` deletes them; `-json` prints JSON.

### `service` (Windows)

```text
airview-feeder service install|remove|start|stop|pause|continue|restart|status|run|sid [options]
```

| Command | Meaning |
|---|---|
| `install` | Register (or update) the Windows service; needs administrator rights. |
| `remove` | Stop and unregister the service (keeps configuration and identity). `-keep-eventlog` keeps the Event Log source. |
| `start`, `stop`, `restart` | Control the service (`-wait duration`, default 1m0s). |
| `pause`, `continue` | Stop feeding until `continue` (the service keeps running). |
| `status` | Show the service state (`-json` for JSON). |
| `run` | The service body, started by the service control manager; `-console` runs it in this console for testing. |
| `sid` | Print the SID of the service's virtual account (for `icacls`). |

Every service command takes `-name name` (default `AirViewFeeder`; one per feeder instance). Options of `install`:

| Option | Meaning |
|---|---|
| `-account account` | `virtual` (default, `NT SERVICE\<name>`), `localsystem`, `localservice` or `networkservice`. |
| `-start type` | `auto` (default), `delayed` or `manual`. |
| `-now` | Start the service after installing it. |
| `-display name` | Display name in services.msc (default "AirView Feeder"). |
| `-exe executable` | Feeder executable the service runs (default: `airview-feeder.exe` next to this program). |
| `-acl` | Restrict the data directory to SYSTEM, Administrators and the service account (default true; `-acl=false` to skip). |
| `-eventlog` | Register the Windows Event Log source (default true). |
| `-user-control` | Let interactive users pause, resume and restart the service without a UAC prompt (default true). |
| `-log-dir`, `-log-max-mb`, `-log-keep` | Log folder (default `<data>\logs`), rotation size (default 10 MB), old files kept (default 5); also `AIRVIEW_FEEDER_LOG_*`. |

### `tray` (Windows)

The notification-area icon: status, pause, logs, settings.

| Option | Meaning |
|---|---|
| `-autostart on\|off` | Start the tray when you log on (this user). |
| `-embedded mode` | `auto` (default) runs the feeder inside the tray when the service is not installed; `off` never does. |
| `-log-dir directory` | Log directory of the tray-hosted feeder (default `<data>\logs`). |
| `-name name` | Feeder service name (default `AirViewFeeder`). |
| `-signal quit\|menu\|refresh` | Send a signal to a running tray and exit. |
| `-url URL` | Status page URL (default: found from the service or the configuration). |

---

## 4.2.3 `airview-healthcheck` (Docker)

```text
airview-healthcheck [-url URL] [-timeout 5s]
```

Fetches the URL and exits 0 on a 2xx answer within the timeout, 1 otherwise, 2 on a usage error. Without `-url` it uses `$AIRVIEW_HEALTHCHECK_URL`, else `http://127.0.0.1:$AIRVIEW_HTTP_PORT/healthz` (port 80 when unset). The image needs no shell, curl or wget for its `HEALTHCHECK`.

---

## 4.2.4 Linux server installer (`airview-server-install.sh`)

```text
install.sh [options]

First installation (ignored on upgrades; edit /etc/airview/airview.jsonc instead):
  --mode MODE          aggregator (default) or standalone
  --name NAME          network display name (aggregator.name)
  --public-host HOST   host name feeders use to reach this server (aggregator.publicHost)
  --proxy              a reverse proxy on this host terminates HTTPS:
                       AirView listens on 127.0.0.1:8080 and trusts its X-Forwarded-For
  --http-port N        HTTP port (default 80, or 8080 with --proxy)
Download:
  --base-url URL       fetch airview-server-linux-<arch>.tar.gz and SHA256SUMS from URL,
                       verify the checksum and install from it
Upgrade (also: upgrade.sh):
  --upgrade            require an existing installation
  --no-backup          skip the backup taken before an upgrade
  --no-rollback        keep the new binary even if it does not become healthy
  --force              upgrade even if the new version rejects the configuration
General:
  --no-start           install but do not (re)start the service
  -h, --help           this help
```

It installs the binary to `/opt/airview/airview`, the operator file to `/etc/airview/airview.jsonc`, data to `/var/lib/airview`, and the `airview` systemd unit (which runs `airview config check -q` before every start).

## 4.2.5 Raspberry Pi station installer (`airview-pi-install.sh`)

```text
sudo ./install.sh [options]          (from the unpacked airview-pi-<version>.tar.gz)
curl -fsSL https://<release-site>/airview-pi-install.sh | sudo sh -s -- --base-url https://<release-site>

  --base-url URL   download airview-pi-*.tar.gz + SHA256SUMS from URL, verify, install
  --key KEY        release public key (base64 Ed25519, from the release notes): SHA256SUMS.sig
                   must verify against it; the key is pinned in /etc/airview/update.env
  --allow-unsigned accept a download without a verified signature (not recommended)
  --upgrade        require an existing installation (update.sh passes this)
  --force          reinstall even when the downloaded version is already installed
  --no-start       install but do not (re)start the service
  -h, --help       this help
```

Running it again upgrades in place; `/var/lib/airview` and `/etc/airview/airview.jsonc` are kept, and the previous binary (`/opt/airview/airview.prev`) is restored automatically when the new one does not start.

## 4.2.6 Feeder installer for Raspberry Pi and Linux (`airview-feeder-install.sh`)

```text
sudo bash install.sh [options]                 install or reconfigure
sudo bash install.sh --update [--force]        update the feeder binary now
sudo bash install.sh --update --check          only check for a newer version (exit 10 if one exists)
sudo bash install.sh --update --channel beta   switch the release channel (stable|beta) and update
sudo bash install.sh --status                  show service, config and link state
sudo bash install.sh --claim                   print a claim code for this station
sudo bash install.sh --uninstall [--purge]     remove the feeder (readsb is never touched)
```

| Group | Options |
|---|---|
| Connection | `--server HOST[:PORT]` (port 30160 by default), `--pin sha256/BASE64`, `--token TOKEN` (alias `--enrol`), `--url https://HOST` (download base, default `https://` + the server host), `--manifest-key BASE64`, `--http-pin sha256//B64` (pin the download server's key), `--binary FILE` (install this binary instead of downloading) |
| Station | `--name NAME` (default: host name), `--lat DEG --lon DEG`, `--alt 120m\|400ft` (unit required), `--alt-ref msl\|hae`, `--agl 8m\|25ft` (default 10m), `--no-location`, `--input auto\|HOST:PORT\|beast:HOST:PORT\|avr:HOST:PORT\|sbs:HOST:PORT\|radarcape:HOST`, `--privacy hidden\|region\|cell01`, `--no-timestamps`, `--readsb-results` (push results into readsb at 127.0.0.1:30104), `--no-auto-update`, `--channel stable\|beta` |
| Behaviour | `--non-interactive` / `-y`, `--install-chrony` (when the clock is not NTP-synchronised), `--allow-unsigned`, `--no-start`, `--arch arm64\|armhf\|amd64`, `--dry-run`, `--dry-run-root DIR`, `--quiet`, `--help`, `--version` |

An aggregator serves this script with its own presets at `https://<aggregator>/feeder/install.sh`.

## 4.2.7 Windows installers (`AirView-Setup-*.exe`, `AirView-Feeder-Setup-*.exe`)

```text
Setup.exe [/S] [/ALLUSERS | /CURRENTUSER] [/NODESKTOP] [/NOSTARTMENU]
          [/FIREWALL [/PORT=n]] [/NOLAUNCH] [/LOG=file] [/D=folder]
```

| Switch | Meaning |
|---|---|
| `/S` (also `/SILENT`, `/VERYSILENT`) | Silent install with the default options. A per-machine install still shows one UAC prompt. |
| `/ALLUSERS` \| `/CURRENTUSER` | Per-machine install (default) or per-user install (no service, no firewall rule, no UAC). |
| `/DESKTOP` \| `/NODESKTOP` | Desktop shortcut on or off. |
| `/STARTMENU` \| `/NOSTARTMENU` | Start-menu shortcuts on or off. |
| `/FIREWALL` \| `/NOFIREWALL`, `/PORT=n` | Inbound Windows Firewall rule for the HTTP port (the web interface, or the feeder status page), and its port. |
| `/NOLAUNCH` | Do not offer to start the program at the end. |
| `/LOG=file` | Write the setup log to this file (default `%TEMP%\<id>-setup.log`; secrets on the command line are masked). |
| `/D=folder` | Destination; must be last, no quotes, spaces allowed. |
| `/?` | Show the switches of this installer. |

Switches are case-insensitive, and `-` may be used instead of `/`.

The **feeder** installer additionally has `/SERVICE` or `/NOSERVICE` (the Windows service), `/TRAY` or `/NOTRAY` (tray icon at log-on) and the unattended configuration `/SERVER=host[:port]` with `/PIN=…`, `/ENROL=…`, `/NAME=…`, `/LAT=…`, `/LON=…`, `/ALT=…`, `/AGL=…`, `/PRIVACY=…`, `/TIMESTAMPS`, `/NOTIMESTAMPS`, `/NOLOCATION` (passed to `airview-feeder setup -non-interactive`).

Uninstall: `uninstall.exe [/S] [/REMOVEDATA] [/LOG=file]` in the installation folder (or Settings → Apps). `/REMOVEDATA` also deletes the configuration and data folders, which are kept by default (`/KEEPDATA`).

Exit codes: 0 done, 1 failed, 2 cancelled, 3 needs administrator rights, 4 bad command line, 3010 done (restart to finish).
