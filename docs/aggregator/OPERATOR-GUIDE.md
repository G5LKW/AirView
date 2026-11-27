# AirView Network: operator guide

This guide is for the person who runs an AirView **aggregator**: the server that feeders connect to, which merges their data into one live map, runs multilateration (MLAT) and keeps per-station statistics. It covers a headless Linux VPS from first install to day-to-day operation.

Related documents:
- [FEEDER-GUIDE.md](FEEDER-GUIDE.md): what you send to your feeder owners.
- [PRIVACY-NOTICE.md](PRIVACY-NOTICE.md), [LIA.md](LIA.md), [CONSENT.md](CONSENT.md), [RETENTION.md](RETENTION.md): the UK GDPR paperwork you must complete **before** you admit the first feeder.
- [../CONFIG.md](../CONFIG.md): every setting, its default, its range and its `AIRVIEW_*` variable. It is generated from the source, so it always matches your build.
- [AVF.md](AVF.md): the native feeder protocol, for developers.
- [DESIGN.md](DESIGN.md): the full design. Section numbers in this guide (§n) refer to it.

---

## 0. Status of this guide

The aggregator is being built in phases. This guide describes the finished product, so a few commands and packages it mentions do not exist in every build yet. Check the table against [PROGRESS.md](PROGRESS.md) and the release notes of your build. Where a planned tool is missing, the guide gives a manual procedure that works with the current binary.

| Item | State when this guide was written | What to do until it exists |
|---|---|---|
| `airview` binary for linux/amd64 and linux/arm64, aggregator mode, `config check/print/example/env`, `admin init/reset` | available | – |
| Operator file `/etc/airview/airview.jsonc`, `AIRVIEW_*` variables, `-set` | available | – |
| Server install kit `deploy/server/` (`install.sh`, `upgrade.sh`, `backup.sh`, `restore.sh`, systemd unit, proxy examples), released as `airview-server-linux-{amd64,arm64}.tar.gz` with the one-line installer `airview-server-install.sh` (`curl -fsSL <site>/airview-server-install.sh \| sudo sh -s -- --base-url <site>`) | available | §3 shows the same steps by hand |
| Docker kit (`Dockerfile`, `docker-compose.example.yml`) and the release build context `airview-docker-<version>.tar.gz` (prebuilt binaries, `deploy/docker`) | available (build the image yourself with `docker build`; no image is published to a registry) | §13.6 |
| Health endpoint `GET /healthz` | available | §13.1 |
| `airview backup` / `airview restore` / `airview db snapshot` | available | §13.3, §13.4 |
| Feeder listeners (AVF 30160, Beast 30004, mlat-client 31090 incl. UDP) started by the engine | available | – |
| `airview-feeder`, Pi bundle `airview-feeder-pi-<version>.tar.gz` and `airview-feeder-install.sh`, Pi SD-card images, Windows installer `AirView-Feeder-Setup-<version>.exe` (unsigned) | available | [FEEDER-GUIDE.md](FEEDER-GUIDE.md) |
| Own MLAT engine, self-location (survey, bundle adjustment), federation, share links, Mode A/C | available (phases 2-5) | – |

Commands in this guide that are **planned** are marked *(planned)*.

---

## 1. What you are setting up

```
 feeders (Pi, Windows, AirView stations)          aggregator VPS
 ---------------------------------------          ----------------------------------------------
 airview-feeder / AirView uplink  --AVF/1 TLS-->  :30160  ┐
 readsb beast_reduce_plus_out     --Beast------>  :30004  ├─ airview (mode "aggregator")
 mlat-client                      --mlat v3---->  :31090  ┘    data: /var/lib/airview
                                                                config: /etc/airview/airview.jsonc
 browsers  --HTTPS 443-->  Caddy / nginx  --HTTP-->  127.0.0.1:8080 (web UI + API)
```

**Roles.**

| Role | Who | How they sign in |
|---|---|---|
| Admin | you (and co-operators you trust) | user name + password (12+ characters) + **mandatory** authenticator code (TOTP) |
| Owner | a feeder owner who claimed their station | user name + password (8+ characters), optional TOTP; no e-mail |
| Public | everyone else | no account |

What each role can see is fixed in the visibility matrix (§8.2); [PRIVACY-NOTICE.md](PRIVACY-NOTICE.md) §5 restates it for your users. You cannot widen public visibility of receiver locations by configuration: no public surface ever carries exact station coordinates, receiver lists per aircraft, IP addresses or coverage products.

**Station.** One receiver clock is one station. A Radarcape EVO 2CH is one station with two channels; a Pi with both an RTL-SDR and a Radarcape is two stations.

---

## 2. Before you start

### 2.1 Decisions

| Decision | Setting | Default | Notes |
|---|---|---|---|
| Who may connect a feeder | `aggregator.admission` (default `invite`) | invite | `invite`: a feeder needs an enrol token you mint. `approve`: anyone may connect; you approve each station before its data reaches the map or MLAT. `open`: automatic. Start with `invite`. |
| Who may create an owner account | `aggregator.auth.registration` (default `open`) | open | `closed` = you create accounts. For a club, `open` is fine because an account is useless without a claim code. |
| Public pins for new stations | `aggregator.defaultPin` (default `hidden`) | hidden | Owners can opt in to `region` (0.5° cell) or `cell01` (0.1° cell). mlat-client feeders that send `privacy:false` start at `region` (§19.2). |
| Surveyed positions used for MLAT automatically | `aggregator.mlat.autoUseSurvey` (default `true`) | on, σh ≤ 50 m | §19.3. The position stays owner/admin-only; the owner is notified and can override. Your privacy notice must say this ([PRIVACY-NOTICE.md](PRIVACY-NOTICE.md) §4). |
| Public live map, statistics, tar1090-style JSON | `aggregator.publicMap`, `aggregator.publicStats`, `aggregator.tarJson` | all on | Turn off for a members-only network. |
| Coverage-island guard | `aggregator.islandGuard` (default `false`) | off | See §8.6 and [PRIVACY-NOTICE.md](PRIVACY-NOTICE.md) §9. |

### 2.2 Legal paperwork (UK GDPR)

A feeder is usually at someone's home, so its location is personal data and you are the **controller**. Before the first feeder connects:

1. Fill in [PRIVACY-NOTICE.md](PRIVACY-NOTICE.md) and publish it on your site (for example at `https://<your host>/privacy`, linked from the claim and feeder pages).
2. Complete and date [LIA.md](LIA.md) (legitimate interests assessment) and the DPIA screening in it. Keep them; you do not publish them, but the ICO may ask for them.
3. Check whether you must pay the ICO data protection fee (ICO self-assessment; the not-for-profit exemption is narrow).
4. Configure the retention settings and your backup rotation to match [RETENTION.md](RETENTION.md), including the reverse-proxy logs (§5.4) and journald (§13.2).
5. Make sure the consent texts of [CONSENT.md](CONSENT.md) appear where feeders are installed and claimed.

These templates are not legal advice. If your network is large or commercial, have them reviewed.

### 2.3 Sizing

| Network | vCPU | RAM | Disk | Inbound traffic |
|---|---|---|---|---|
| up to ~100 feeders, full traffic (§19.1) | 2-4 | 4-8 GB | 40 GB SSD | 15-35 kB/s per busy station in `full` mode, so 1.5-3.5 MB/s (12-28 Mbit/s) for 100 stations |
| 1,000+ feeders, 10k+ aircraft (§20.2) | 8+ | 16-32 GB | 250+ GB SSD/NVMe | stations above `aggregator.limits.selectiveAbove` (default `100`) switch to `selective` traffic, about 3-5 kB/s each: 3-5 MB/s for 1,000 |

- **Transfer volume.** One busy station in full mode sends roughly 40-90 GB a month. 100 stations are several TB a month inbound. Many VPS plans count only outbound traffic, but check yours.
- **Disk.** Use an SSD. Measured on a 1,000-feeder simulation with about 3,000 aircraft:
  - `airview.db` grows by about **0.6 GB per 1,000 aircraft and day**, almost all of it the stored position trails (`positions`; one point per aircraft every `storage.trailIntervalSec` = 5 s, about 40 bytes with its index). The heat map adds a few per cent. In aggregator mode trails are kept for `aggregator.storage.trailDays` (default **7**). They stay at full resolution for `aggregator.storage.trailFullDays` (default **2**) days after the flight ended, then keep one point per `aggregator.storage.trailThinSec` (default **30** s; thinned trails take about 15 % of the bytes). The steady state is therefore about **1.7 GB per 1,000 aircraft** (5.1 GB at 3,000, 16.9 GB at 10,000; without thinning 4.3, 12.8 and 42.8 GB). Daily inflow is unchanged (about 6 GB a day at 10,000 aircraft). Set either tier field to 0 to keep full trails.
  - Per-station statistics (measured with the opt-in store growth test `TestGrowth` in `internal/store`, 100 busy feeders, 800 aircraft, re-run in the integration wave after the SCALE-04 finding; the earlier "3 MB per station" covered `stats_minute` only):

    | Table (database) | per busy station and day | kept at the defaults (per station) |
    |---|---|---|
    | `flight_station` (`airview.db`): which stations heard each flight | about 1.1 MB | about 100 MB |
    | `heat` (`stations.db`): per-station heat-map cells | 0.67 MB on the first day, then it saturates (cells are merged) | about 2.7 MB |
    | `stats_minute`, `stats_hour`, `stats_day` | 0.17 MB | about 6.5 MB |
    | `range_hour`, `signal_hist`, `coverage`, `elev_hist` | 0.18 MB | about 13 MB |

    So `stations.db` settles at roughly **22 MB per busy station** (about 22 GB for 1,000) and the station rows of `flight_station` in `airview.db` at roughly **100 MB per busy station** (about 100 GB for 1,000; it follows the flight retention). Quiet rural stations need a fraction of this. The first day is the most expensive for `heat`: plan the write rate (§2.3 MLAT and the `stations.db` writer below), not only the size.
  - Leave free space for one copy of both databases for `airview backup` (it stages consistent copies in the data directory, §13.3).
  - So 1,000 busy feeders with 3,000-10,000 aircraft need about **140-160 GB** with the defaults (trails 5-17 GB, `stations.db` about 22 GB, `flight_station` about 100 GB); 250 GB leaves room for growth and backups. Shorter retention (CONFIG.md `aggregator.storage.*`) scales these figures down linearly.
  - The `stations.db` writer takes bursts of millions of rows when stations flush their day tables; lanes now spread these flushes over the whole interval by station (SCALE-03), and `aggregator.storage.queueRows` (default 2,000,000) bounds the queue. `/healthz` reports `store.stations` with the rows dropped since start; if that number grows, use faster storage or raise `queueRows` (about 100 bytes of RAM per queued row).
- **Outbound traffic to viewers.** The web UI uses the delta live stream (`/api/public/events?delta=1`): one picture, then only what changed, about 70-80 bytes per moving aircraft per second (measured: 0.8 MB/s for 10,000 aircraft, against 3.5 MB/s for full pictures). Plan for that per open map at the busiest time, or cap viewers with `aggregator.limits.maxPublicSse`. A client that opens a viewport stream (`bbox=`, see docs/API.md) receives only the aircraft in its map view: at 10,000 aircraft a zoom-8 view costs about 54 kB/s (10.5 kB/s gzip), zoom 9 about 13 kB/s (2.5 kB/s gzip) and zoom 7 about 185 kB/s (36 kB/s gzip), against 712 kB/s (139 kB/s gzip) for the unfiltered delta stream. The bundled Live Map opens a viewport stream by itself when the network has more than 1,500 aircraft and returns to the full stream below 1,000 (map setting **Load aircraft**: Automatic / Visible area / Everything); the lists, tables and statistics pages keep the full stream, because a viewport stream never carries aircraft without a position. Public trails are limited to the last 10 minutes and 120 points per aircraft.
- **MLAT (CPU and memory).** Measured on simulated networks (docs/aggregator/MLAT-RESULTS.md "CPU and memory"): 100 stations need about 0.25 cores and 150 MB for ingest plus engine, 300 stations about 1 core and 410 MB, with a clock-filter epoch of 13 ms and 77 ms every 0.5 s. A 1,000-station run was not executed; the projection is 3-4 cores and 1.5-2 GB for ingest plus engine at 1,000 stations and 10,000 aircraft. On top of that the server keeps the MLAT console history in memory (about 3.4 MB at 1,000 stations) and writes it to `mlat-history.gob.gz` in the data directory every 10 minutes and at shutdown (a few MB at 1,000 stations), so the admin charts and the owners' 2-hour clock charts survive a restart. `aggregator.mlat.engine.workers` and `aggregator.mlat.clock.workers` bound the goroutines (default: one per CPU).
- **Memory (whole process, steady state).** Measured in a 45-minute soak of the Windows build (`airview-netsim`: 60 AVF, 15 Beast and 10 mlat-client feeders plus one `airview-feeder`, 800 aircraft, MLAT on, 32 CPU threads) with heap profiles at 5, 15, 30 and 45 minutes: the live Go heap was 310, 415, 457 and 482 MB and the process's private memory 0.71, 0.97, 1.08 and 1.16 GB, flat over the last 5 minutes; goroutines (396) and threads (40) stayed flat. Nothing grows without bound: what still grows in the first hour are stores that fill up to a fixed size and then stay there:
  - the MLAT late-copy rings, about 16 MB per MLAT worker up to 4 workers and 64 MB in total above that (`aggregator.mlat.group.lateSlots`; in the first 1.0.0 build, replaced by the rebuild of 14 November 2026, they cost 16 MB per CPU thread, 515 MB on this machine);
  - the station survey's bundle-adjustment store, filled over `aggregator.mlat.survey.bundle.spanMin` (60 min) and capped by `bundle.maxFrames` (200,000 transmissions, about 300 bytes each with this mix: 65 MB at 45 minutes);
  - per station: the statistics and heat-map accumulators between flushes and the `stations.db` write queue, about 1.2 MB per busy feeder;
  - per aircraft: the ingest's frame copies, live trails (capped per aircraft), tracker tables and trajectories, about 0.25 MB per aircraft at about 10 receiving stations per aircraft.

  For planning: **resident memory ≈ 2.4 × (180 MB + 1.2 MB per busy feeder + 0.25 MB per aircraft)**, which gives the measured 1.16 GB for 85 feeders and 800 aircraft. The factor 2.4 is the garbage collector's headroom (the heap may grow to twice the live data before a collection) plus runtime overhead; a soft limit (`process.memoryLimitMb`, on Linux by default 80 % of the RAM or 85 % of a cgroup cap) makes the collector work harder instead of growing further. Extrapolated to 1,000 feeders and 10,000 aircraft the formula gives about 9 GB, inside the 16-32 GB of the table above; the per-aircraft term rises with the number of stations that hear each aircraft, so that figure is a projection, not a measurement.
- **Hosting location.** If the VPS is outside the UK, your privacy notice must name the country and the transfer safeguard ([PRIVACY-NOTICE.md](PRIVACY-NOTICE.md) §7).
- A Raspberry Pi 5 can host a small aggregator only with an NVMe or USB SSD; on an SD card AirView warns and shortens minute retention to 7 days.

---

## 3. Install on a Linux VPS

Supported: any 64-bit Linux with systemd on amd64 or arm64. AirView is one static binary with no runtime dependencies and no GUI or WebView on Linux.

> The server kit `deploy/server/install.sh` does the steps of this section for you (`sudo ./install.sh --proxy` from an unpacked bundle, or `curl -fsSL <release-site>/airview-server-install.sh | sudo sh -s -- --base-url <release-site>`; see `deploy/server/README.md`). Use `--proxy` on any server reachable from the internet: AirView itself speaks plain HTTP, and in aggregator mode it refuses passwords, TOTP codes and the setup token from the network unless they arrive over HTTPS through a proxy listed in `aggregator.trustedProxies` (or `http.allowInsecureLogin` is set, which you should not do on the internet). This section shows the same steps by hand.

### 3.1 Get the binary

Build it on any machine with Go installed (no C compiler needed):

```sh
# in the AirView source tree
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o airview-linux-amd64 ./cmd/airview
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o airview-linux-arm64 ./cmd/airview
```

On Windows, `deploy\build.ps1` builds the release set into `dist\`. Copy the right binary to the server (for example with `scp`) and verify its checksum against the one you computed locally (`sha256sum`).

### 3.2 User, directories and binary

```sh
sudo useradd --system --home-dir /var/lib/airview --shell /usr/sbin/nologin airview
sudo install -d -m 0750 -o airview -g airview /var/lib/airview
sudo install -d -m 0755 /opt/airview /etc/airview
sudo install -m 0755 airview-linux-amd64 /opt/airview/airview
/opt/airview/airview -version
```

### 3.3 Operator configuration file

The operator file is JSON with comments. It is read-only for AirView and **wins over the web UI**: anything you set here is locked in the UI (shown as "set by the operator file"). Write the full commented reference next to it:

```sh
sudo /opt/airview/airview config example -o /etc/airview/airview.example.jsonc
```

Then create `/etc/airview/airview.jsonc` with only the settings you want to pin. A good starting point for a club VPS:

```jsonc
// /etc/airview/airview.jsonc - AirView aggregator operator file.
// Reference of every setting: /etc/airview/airview.example.jsonc and docs/CONFIG.md.
{
  "mode": "aggregator",

  // Web UI and API: local only, published through the reverse proxy (section 5).
  "http": { "bindAddr": "127.0.0.1", "port": 8080 },

  "aggregator": {
    "name": "Example ADS-B Club",
    "publicHost": "adsb.example.org",      // embedded in feeder install one-liners

    // Feeder listeners (0 = off). 30160 carries its own TLS; never proxy it through HTTP.
    "avfPort": 30160,
    "beastPort": 30004,
    "mlatPort": 31090,

    "admission": "invite",
    "defaultPin": "hidden",

    // The reverse proxy on this host. Only these peers may set X-Forwarded-For/-Proto.
    "trustedProxies": ["127.0.0.1/32", "::1/128"],

    "auth": { "registration": "open" },

    "mlat": { "enabled": true, "autoUseSurvey": true, "autoUseMaxSigmaM": 50 },

    // Retention schedule published in the privacy notice (RETENTION.md).
    "retention": { "connLogDays": 30, "minuteDays": 30, "hourMonths": 13,
                   "offlinePurgeDays": 90, "auditMonths": 24 }
  },

  // Database profile (CONFIG.md "Storage profiles"); "auto" picks the same in aggregator mode.
  "storage": { "profile": "server" }
}
```

```sh
sudo chown root:airview /etc/airview/airview.jsonc
sudo chmod 0640 /etc/airview/airview.jsonc
sudo -u airview AIRVIEW_CONFIG=/etc/airview/airview.jsonc AIRVIEW_DATA=/var/lib/airview \
     /opt/airview/airview config check
```

`config check` reports syntax errors, unknown or misspelt settings (with a suggestion), wrong types and out-of-range values with file, line and column. Exit status 0 = valid, 1 = errors. `config print -sources` shows the effective configuration and where each overridden value came from (secrets are redacted).

**Secrets** (`aggregator.outputToken`, `aggregator.tlsKey`, `hub.password`, ...) are better kept out of the file: use `AIRVIEW_<NAME>_FILE=/path` in a systemd drop-in, pointing at a file readable only by `airview`.

### 3.4 systemd unit

`/etc/systemd/system/airview.service`:

```ini
[Unit]
Description=AirView aggregator
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=airview
Group=airview
Environment=AIRVIEW_CONFIG=/etc/airview/airview.jsonc
Environment=AIRVIEW_DATA=/var/lib/airview
# Every log line in the journal too (the default under systemd is warnings and errors only).
Environment=AIRVIEW_PROCESS_JOURNAL_LEVEL=info
# Report configuration problems in the journal before the start. Errors in the
# operator file also stop the start itself.
ExecStartPre=-/opt/airview/airview config check -q
ExecStart=/opt/airview/airview -headless -data /var/lib/airview
WorkingDirectory=/var/lib/airview
Restart=always
RestartSec=5
# Leave time to write the batched statistics on stop.
TimeoutStopSec=60

# The web UI listens on 8080 behind the proxy and the feeder ports are above
# 1024, so no capabilities are needed.
AmbientCapabilities=
CapabilityBoundingSet=
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
ReadWritePaths=/var/lib/airview
# Feeder connections: each needs a file descriptor.
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

Do not start it yet: create the admin first (§4).

### 3.5 Where things live

| What | Where |
|---|---|
| Binary | `/opt/airview/airview` |
| Operator file / reference | `/etc/airview/airview.jsonc` / `/etc/airview/airview.example.jsonc` |
| Merged network data (aircraft, flights, positions, `flight_station`, MLAT quality) | `/var/lib/airview/airview.db` |
| Registry, accounts, sessions, claims, enrol tokens, station statistics, audit log, connection log | `/var/lib/airview/stations.db` |
| Settings saved from the web UI | `/var/lib/airview/config.json` (written by AirView) |
| AVF TLS certificate (self-signed default) | `/var/lib/airview/avf-tls/` |
| Application log (rotated by AirView at `process.logMaxMb`, 64 MB in aggregator mode, keeping `process.logFiles`, 10 by default) | `/var/lib/airview/airview.log`, `airview.log.1` ... |
| Service log | `journalctl -u airview` |

`stations.db` holds secrets (password hashes, TOTP secrets, hashed tokens, keys). Treat the data directory and every backup of it as confidential.

---

## 4. Create the first admin

Aggregator mode refuses to admit feeders until an admin with a password **and** an authenticator exists (§7).

**Recommended: from the CLI, before the first start.**

```sh
sudo -u airview AIRVIEW_CONFIG=/etc/airview/airview.jsonc /opt/airview/airview admin init -data /var/lib/airview -user admin
```

1. Type the password twice (12 characters or more; not echoed).
2. Add the shown secret or `otpauth://` URI to an authenticator app (any RFC 6238 app).
3. Type the current 6-digit code to confirm.
4. **Write down the 10 recovery codes** and store them offline. Each works once, in place of an authenticator code.

Then start the service:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now airview
journalctl -u airview -n 50
```

**Alternative: from the browser.** If you start the service without an admin, AirView writes a one-time setup token to the file `/var/lib/airview/setup-token` (mode 0600, readable only by the service account and root: `sudo cat /var/lib/airview/setup-token`); it is never written to the journal or `airview.log`. Open the site and complete the setup page with it. The token is valid until used or until the next restart. Because the journal is readable by administrators of the host, prefer the CLI on shared machines.

**Lost access (break-glass).** Anyone with shell access to the data directory can reset an admin:

```sh
sudo -u airview /opt/airview/airview admin reset -data /var/lib/airview -user admin
```

It sets a new password and authenticator, ends that admin's sessions and prints new recovery codes. It is safe to run while the server is up.

---

## 5. Web access: reverse proxy and TLS

Run the web UI on `127.0.0.1:8080` and publish it through a reverse proxy that terminates HTTPS. AirView then:
- honours `X-Forwarded-For` and `X-Forwarded-Proto` **only** from `aggregator.trustedProxies`; from anyone else they are ignored;
- marks session cookies `Secure` when the request came over HTTPS;
- requires a CSRF token on every state-changing request (the web UI handles this).

**Never put the feeder port 30160 behind an HTTP proxy.** AVF/1 terminates TLS inside AirView and binds the feeder's signature to that TLS session. A plain TCP pass-through (for example nginx `stream` without `ssl`) works; TLS termination in front breaks every feeder.

### 5.1 Caddy (simplest)

`/etc/caddy/Caddyfile`:

```
adsb.example.org {
	encode gzip
	reverse_proxy 127.0.0.1:8080 {
		# Live updates are a server-sent event stream: deliver it unbuffered.
		flush_interval -1
	}
	log {
		output file /var/log/caddy/airview-access.log {
			roll_size 10MiB
			roll_keep 5
			roll_keep_for 336h   # 14 days - see RETENTION.md
		}
	}
}
```

Caddy obtains and renews the certificate itself and sends `X-Forwarded-For` / `X-Forwarded-Proto`.

### 5.2 nginx

```nginx
server {
    listen 80;
    listen [::]:80;
    server_name adsb.example.org;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;               # nginx < 1.25.1: write "listen 443 ssl http2;" instead
    server_name adsb.example.org;

    ssl_certificate     /etc/letsencrypt/live/adsb.example.org/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/adsb.example.org/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;

    # See RETENTION.md: access logs hold IP addresses.
    access_log /var/log/nginx/airview-access.log;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # Server-sent events: no buffering, long reads.
    location = /api/events {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 1h;
    }
}
```

Get the certificate with certbot (`certbot --nginx -d adsb.example.org`) or any ACME client.

### 5.3 Proxy on another host

If the proxy runs on a different machine, put **its** address in `aggregator.trustedProxies` (for example `["10.0.0.5/32"]`), bind AirView to the private interface (`http.bindAddr`), and firewall port 8080 so only the proxy reaches it. Never list a public range: anyone inside it could forge their client address.

### 5.4 Proxy logs and privacy

Proxy access logs contain visitors' and owners' IP addresses. AirView itself never stores raw IPs (§8.4), so the proxy log would be the only place they persist. Either keep access logs short (14 days is the default in [RETENTION.md](RETENTION.md)), or turn them off, or log a truncated address. Whatever you choose must match your privacy notice.

---

## 6. The feeder port certificate (AVF/1 on 30160)

AVF/1 is TLS 1.3 only. Two choices:

**(a) Self-signed (default).** On first start AirView creates an ECDSA P-256 certificate in `/var/lib/airview/avf-tls/`. Feeders do not check it against a CA; they check its **pin**, `sha256/<base64>`, which the admin console shows and which AirView embeds in the install one-liner it serves. This is secure as long as feeders get the pin over HTTPS from your site (or from you). Back up `avf-tls/`: if the key is lost, every feeder must be given the new pin.

**(b) Your own certificate.** Set `aggregator.tlsCert` and `aggregator.tlsKey` to PEM files (for example the files your ACME client renews). AirView reloads them when their modification time changes. Feeders then verify against the system CA roots and need no pin. The `airview` user must be able to read both files; with certbot, copy them in a deploy hook rather than loosening the permissions of `/etc/letsencrypt`.

If you switch from (a) to (b), feeders that still carry the old pin will refuse the new certificate until their pin is removed or updated (`airview-feeder config set 'uplinks[0].serverPin='` on the feeder, or a re-run of the installer).

---

## 7. Firewall and ports

| Port | Protocol | Open to | Purpose |
|---|---|---|---|
| 22 | TCP | your admin addresses | SSH |
| 80, 443 | TCP | everyone | reverse proxy: web UI, API, feeder installer downloads |
| 30160 | TCP | feeders (everyone, or your members' ranges) | AVF/1, TLS 1.3 (`aggregator.avfPort`) |
| 30004 | TCP | feeders running readsb / Ultrafeeder | Beast compat ingest, map and statistics only (`aggregator.beastPort`) |
| 31090 | TCP (+UDP if `aggregator.mlat.udp`) | feeders running mlat-client | mlat-client protocol v3, MLAT and survey (`aggregator.mlatPort`) |
| 8080 | TCP | localhost only | web UI behind the proxy |
| 30154 | TCP | closed, or your own address | hub port: admin login only in aggregator mode (`hub.enabled`, `hub.port`) |
| `aggregator.beastOutPort`, `aggregator.sbsOutPort` | TCP | specific consumers only | merged outputs; refused unless `aggregator.outputToken` is set |
| `aggregator.sbsInLocalPort` | TCP | 127.0.0.1 (enforced) | bridge for an operator-run mlat-server during migration (§5.3) |
| 30170 | TCP | your federation peers only | AVFED/1 federation listener, TLS 1.3, inbound only when `aggregator.federation.listen` is set (§8.8) |
| out-feed addresses (listen mode) | TCP | the partner network only | federation-safe out-feeds (Beast / JSON), see §8.8 |

**Ports 30004 and 31090 carry no encryption.** That is how readsb and mlat-client work. Timing data, the station UUID and (on 31090) the declared position cross the internet in clear text. Recommend native AirView feeders (AVF/1, encrypted) to anyone who cares, and say so in your privacy notice.

ufw example:

```sh
sudo ufw default deny incoming
sudo ufw allow from 203.0.113.10 to any port 22 proto tcp   # your admin address
sudo ufw allow 80,443/tcp
sudo ufw allow 30160/tcp
sudo ufw allow 30004/tcp
sudo ufw allow 31090/tcp
sudo ufw enable
```

nftables equivalent (inside your `inet filter input` chain):

```
tcp dport { 80, 443, 30160, 30004, 31090 } accept
ip saddr 203.0.113.10 tcp dport 22 accept
```

AirView has its own per-IP and per-subnet connection limits (`aggregator.limits.*`): at most 4 unauthenticated and 16 total connections per IP, 32 per IPv4 /24 or IPv6 /48, 8 stations per IP. A club relay host that legitimately carries many stations goes into `aggregator.limits.allowCidrs`.

---

## 8. Running the network

### 8.1 Enrol tokens (admission `invite`)

In the admin console, **Enrol tokens → New**: give it a note ("Bob's Pi"), a use count (default 1) and a lifetime (default 7 days). The token is shown **once**; it is stored only as a hash. Send it to the feeder owner over a private channel. Revoke unused tokens. For a group install day, mint one token with a higher use count and a short lifetime.

mlat-client and readsb feeders cannot present a token. Under `invite`, a new UUID on 30004 is held **pending** until you approve it in the station registry; a link without a UUID is refused. Under `approve`, every new station waits for you.

### 8.2 Claims and owners

Owners link their account to their station with a **claim code** (8 characters, valid 24 hours, single use), which the feeder requests from your server (`airview-feeder claim`, the feeder's status page or the installer's last screen). Compat feeders claim from the station's current IP address by entering the full UUID. Stations without a UUID need you to assign them. Claims are rate-limited (5 per 10 minutes per IP, 20 per day per account).

### 8.3 What you see and what is audited

The admin console shows every station exactly, the per-aircraft receiver lists, the sync graph, the MLAT console, the CRLB accuracy map, connection data and the current IP of a connected station. Viewing an exact station location, and every other exact-data admin view, is written to the **audit log** with your account and the time (§8.4). The audit log is kept 24 months by default and owners see entries about their own stations in their data export. Do not browse exact locations without a reason; the audit log will show it.

### 8.4 Your own station as anchor (§19.1)

Your best MLAT anchor is a GPS-timed receiver such as a Radarcape EVO 2CH with GPS timestamps on (status page shows UTC OK and sync OK). Keep running it as a normal standalone AirView at home and add an **uplink** to the aggregator (Settings → Uplinks in that AirView, or in its `config.json`/operator file):

```jsonc
"uplinks": [
  { "id": "club", "host": "adsb.example.org", "enrolToken": "<token>",
    "sendLocation": true, "shareGnss": false, "timestamps": true }
]
```

With `shareGnss` off (default) the Radarcape's own GNSS position is not uploaded; the declared position, entered to about 10 m, is sent instead. An aggregator can also host a receiver directly (station `#1`, "host"), but on a VPS there is no receiver.

### 8.5 MLAT settings worth knowing

| Setting | Default | Meaning |
|---|---|---|
| `aggregator.mlat.enabled` | `true` | Runs the own MLAT engine. |
| `aggregator.mlat.returnResults` | `true` | Sends results to stations that contributed (minimum-contribution rule always applies; no station identities). |
| `aggregator.mlat.holdout` | `0.1` | 10 % of well-tracked ADS-B aircraft are solved as if they had no ADS-B, to measure real MLAT accuracy. |
| `aggregator.mlat.autoUseSurvey` / `autoUseMaxSigmaM` | `true` / `50` | Use a surveyed station position automatically when σh ≤ 50 m and it passes the suspect checks. |
| `aggregator.mlat.surveyIntervalH` | `6` | Declared positions are re-checked by survey this often; a station more than max(100 m, 3σ) off is marked `suspect` and excluded. |
| `aggregator.mlat.modeAc` | `false` | Mode A/C multilateration among anchors. |
| `aggregator.mlat.udp` | `false` | Accept the mlat-client UDP transport (open UDP 31090 in the firewall too). |

### 8.6 Outputs for other tools

`aggregator.beastOutPort` and `aggregator.sbsOutPort` publish the merged picture (MLAT positions carry the MLAT magic timestamp on Beast). They refuse every client unless `aggregator.outputToken` is set, and the token must be presented by each client. Only open them to consumers you control, and remember that the output itself carries no station information.

### 8.7 Migrating from an existing mlat-server

If you already run an mlat-server (an AGPL program), keep it as a separate, unmodified process and point its basestation output at `127.0.0.1:<aggregator.sbsInLocalPort>`. Its positions appear on the map as `mlat-ext`. AirView never runs, links or embeds it. Switch it off once the own engine's hold-out accuracy is acceptable.

### 8.8 Federation with other AirView networks

Federation (`aggregator.federation.enabled`, off by default) links this network with other AirView aggregators over
AVFED/1: TLS 1.3 with mutual Ed25519 identities bound to the TLS session, so a relay that terminates TLS fails.

- **Identity.** `airview federation id` prints this network's node ID (and, with `listen` set, the TLS pin).
  Exchange node IDs with the other operator out of band; each side lists the other in `peers[]` with its `id`
  and, on the side that dials, `addr`.
- **What peers receive.** At most what your public map shows: aircraft (optionally without MLAT positions,
  inside boxes, above a floor, delayed by `send.delayS`), and MLAT results only with `send.mlat`. Island-guard
  aircraft are coarsened or dropped (`send.island`). Never feeder identities, station positions, receiver lists,
  signal levels or IP addresses: the protocol has no fields for them. Data received from one peer goes to
  another only with `transit` on both sides, bounded by `maxHops`.
- **What you show.** Remote aircraft are drawn on the public map as an overlay unless `hideRemote` is set; they
  never enter your statistics or records (`inject` stays off).
- **Out-feeds.** `feeds[]` are federation-safe Beast (timestamps and signal zeroed, de-duplicated) or JSON
  feeds of your own aircraft for other networks, in `connect` or `listen` mode (listen feeds accept only the
  `allow` networks, loopback by default).
- **Status.** Admin console / `GET /api/admin/federation`: peers, state, direction, RTT, counters, errors.
- **Ports.** 30170/tcp inbound only when `listen` is set (§7).

Every setting is in the generated config reference (`docs/CONFIG.md`, `aggregator.federation.*`) and has an
`AIRVIEW_AGGREGATOR_FEDERATION_*` variable; `airview config check` runs the federation checks.

### 8.9 mlat-client UDP and Mode A/C

`aggregator.mlat.udp` lets mlat-clients send their timing data over UDP on the mlat port (31090/udp). Behind NAT
or in a port-mapped container set `aggregator.mlatc.udpHost` / `udpPort` to the externally reachable address;
behind carrier-grade NAT `udpAnySource` accepts datagrams from any source. `aggregator.mlat.modeAc` turns on
Mode A/C multilateration among the anchors: targets without a Mode S address appear on the map as "Mode A/C"
(synthetic `#xxxxxx` ids, no registration or photo lookups); `GET /api/admin/mlat/modeac` lists them.

### 8.10 Share links, verification alerts and where to add stations

- **Share links.** Owners can give people without an account a read-only live view of their stations
  (`#/me/shares`, or "Share this station" on the station's Privacy tab). Before a link is made the owner must
  confirm that the aircraft a station hears reveal roughly where it is. A link shows the aircraft the chosen
  stations heard in the last minute (public shape: no receiver data, low aircraft rounded to about 5 km unless
  the owner switches that off) and optionally the stations' public-style statistics; never a station position,
  pin, ID, range or coverage. Links last 1 to 365 days (default 30); an owner has at most 20 active links. Only a
  hash of each link is stored (`<data>/sharelinks.json`; until `airview backup` copies it, include it in your
  own backup if links should survive a restore); the full link is shown once. A link stops working when it is revoked, expires, or a listed station leaves the owner's account. You
  can list, search and revoke every link in Network admin > Share links; creating and revoking are audited
  (`share.create`, `share.revoke`).
- **ADS-B verification alerts.** Network admin > MLAT shows how many broadcast ADS-B positions the network's
  timing confirmed or contradicted, the aircraft that are "suspect" now (possible spoofing or a faulty
  transponder; the network multilaterates them for 10 minutes) and the last 200 changes. Admin only.
- **Where to add a station.** The same tab ranks 0.5° areas by how much one more receiver (GPS, Beast or
  RTL-SDR timing) would improve the expected MLAT accuracy (Cramér-Rao bound) at 3000 ft, FL100 or FL300; the
  Live Map's Admin view shows it as the MLAT quality layer "Where to add a station". Admin only, because it is
  derived from exact station positions.
- **Large networks.** The admin tables (stations, MLAT stations, surveys, bundle adjustment, share links,
  federation peers) are paged and searchable; the station table is paged and sorted on the server
  (`GET /api/admin/stations?offset=&limit=&sort=&dir=`).
- **Federation** has its own tab (node ID to give partners, peer state, out-feeds, peer list with sharing
  policy; settings fixed in the operator file are shown locked).

---

## 9. Scaling to 1,000+ feeders

The defaults are sized for §20.2. When you grow:

1. **Traffic mode.** Above `aggregator.limits.selectiveAbove` (default `100`) approved online stations, feeders switch to `selective` traffic (about 3-5 kB/s each). Statistics stay exact through per-minute counters sent by the feeder.
2. **Limits.** `aggregator.limits.maxStations` (default `2000`), `aggregator.limits.maxAircraft` (default `30000`) and `aggregator.limits.ingestFps` (default `3000000`) are hard caps; raise them deliberately. `aggregator.limits.trackerShards` (default `0`, automatic from the CPU count) splits the tracker.
3. **Clock filter.** `aggregator.mlat.maxComponentStations` (default `200`) partitions large clock-sync components.
4. **Beast ingest.** `aggregator.beast.maxLinks` (default `4096`) caps concurrent 30004 links.
5. **File descriptors and web connections.** Keep `LimitNOFILE` well above the number of feeder links plus web clients. Web connections are capped at `aggregator.limits.maxHttpConns` (default `20000`) in total and `aggregator.limits.maxHttpConnsPerIp` (default `64`) per IPv4 address or IPv6 /64; loopback and `aggregator.trustedProxies` are exempt (behind a proxy, cap per client in the proxy). A request body must arrive within 30 s, so slow-body clients cannot hold connections open.
6. **Disk.** The position trails dominate `airview.db`: `aggregator.storage.trailDays` (default `7`) bounds them; per-station minute statistics dominate `stations.db`. See §2.3 for measured growth. The registry purges unclaimed stations offline for `aggregator.retention.offlinePurgeDays` (default `90`).
7. **Viewers.** The public live stream sends one picture and then deltas (§2.3); public trails are bounded. `aggregator.limits.maxPublicSse` caps public streams.
8. **Trusted Beast relays.** An entry of `aggregator.trustedRelays` is `uuid@cidr` (for example `"1234abcd-...@203.0.113.7"`): the relay's 0xE3 receiver IDs are honoured only from those addresses, because the 0xE4 UUID is self-declared and travels in clear text. A plain `uuid` is honoured only from `aggregator.limits.allowCidrs`. New sub-stations of one relay link are registered at most 20 per second.

---

## 10. Changing settings

- **Operator file** (`/etc/airview/airview.jsonc`): edit, run `config check`, then `sudo systemctl restart airview`.
- **Environment** (`sudo systemctl edit airview`):
  ```ini
  [Service]
  Environment=AIRVIEW_AGGREGATOR_LIMITS_SELECTIVE_ABOVE=150
  Environment=AIRVIEW_AGGREGATOR_OUTPUT_TOKEN_FILE=/etc/airview/output-token
  ```
  `airview config env` lists every variable.
- **Web UI** (admin): changes are saved to `config.json`. Settings pinned by the operator file or environment are shown as locked and cannot be changed there.

Layers, later wins: defaults < `config.json` < operator file < `AIRVIEW_*` < `-set path=value`.

---

## 11. Feeder onboarding checklist

1. Mint an enrol token (§8.1).
2. Send the owner [FEEDER-GUIDE.md](FEEDER-GUIDE.md) (or your own copy with your host name filled in), the token, and the link to your privacy notice.
3. The owner runs the installer; it prints a claim code.
4. The owner creates an account on your site and enters the claim code under **Claim**.
5. Check in the admin console that the station is `approved`, receiving and (for timestamped feeders) synced.

For readsb / Ultrafeeder users, your site's `/feeder` page shows the two configuration lines with your host filled in (Beast `beast_reduce_plus_out` to 30004 with their UUID, and mlat-client to 31090).

---

## 12. Responding to data-protection requests

| Request | How |
|---|---|
| Access / portability | The owner downloads **Export** (ZIP of JSON) from their account page. For a person without an account, export the station from the admin console. |
| Erasure | The owner uses **Delete account** (with or without their stations). Admin: delete the station in the registry. Deletion is immediate in the live system; backups expire within your backup rotation (30 days in [RETENTION.md](RETENTION.md)). |
| Rectification | Owners edit name and declared position themselves; admins can rename. |
| Objection to MLAT use | The owner switches the feeder to **No timestamps** (`--no-timestamps`), or you delete the station. |
| Withdraw consent for public display | The owner sets pin `hidden`, public stats and name off; it takes effect at once. |

Respond within one month. Record what you did. See [PRIVACY-NOTICE.md](PRIVACY-NOTICE.md) §8.

---

## 13. Operations

### 13.1 Monitoring

`GET /healthz` is the health endpoint for uptime monitors, load balancers, Docker `HEALTHCHECK` and systemd (it is cheap, never rate-limited and says nothing about stations):

```sh
curl -fsS http://127.0.0.1:8080/healthz
# {"ok":true,"status":"ok","mode":"aggregator","version":"...","uptimeS":86400,
#  "checks":{"engine":"ok","registry":"ok","listener.avf":"listening","listener.beast":"listening",
#            "listener.sbsIn":"off","listener.mlatc":"not built","store.main":"ok","store.stations":"ok"},
#  "adminReady":true}
```

- **200** with `status` `ok` or `degraded` (a non-essential part is missing).
- **503** with `status` `failing`: a configured feeder port could not be opened (`listener.*` says `failed: ...`, for example "address already in use"), or a database writer has not written for 2 minutes while rows are waiting (`store.*`). Feeders cannot deliver data: act on it.
- **503** with `status` `unavailable`: the engine is not running (starting, stopping or a factory reset).
- Before the admin exists, the feeder listeners say `closed until the admin exists` (§4); that is not a failure.

```sh
# Feeder port answers TLS 1.3 with ALPN avf/1
openssl s_client -connect 127.0.0.1:30160 -tls1_3 -alpn avf/1 </dev/null 2>/dev/null | grep -E 'ALPN|Protocol'
systemctl is-active airview
```

A headless aggregator does not move to another web port when its `http.port` is taken: it stops with an error, so systemd and your proxy never point at the wrong service. `airview config check` reports `http.port` or `hub.port` colliding with a feeder port.

The admin console shows ingest load, per-station health and MLAT state.

### 13.2 Logs

- `journalctl -u airview -f`: start-up, configuration corrections, errors. Under systemd AirView copies only warnings and errors to standard error unless `process.journalLevel` (or `AIRVIEW_PROCESS_JOURNAL_LEVEL`) says otherwise; the server kit's unit and the unit of §3.4 set `info`, so the journal has every line.
- `/var/lib/airview/airview.log`: every line at `process.logLevel`, rotated by AirView at `process.logMaxMb` (64 MB in aggregator mode) keeping `process.logFiles` (10) older files; `process.logLevel` sets the level (CONFIG.md).
- In aggregator mode AirView logs no raw IP addresses, positions or per-station timing data.
- Cap journald so its retention matches your notice, for example in `/etc/systemd/journald.conf`: `MaxRetentionSec=30day` and `SystemMaxUse=1G`.
- The one-time admin setup token for the browser setup is **not** written to the journal (anyone in the `adm` / `systemd-journal` groups can read that). It is in `/var/lib/airview/setup-token` (mode 0600, readable by root and the service account only), valid for 24 hours and renewed while no admin exists; the file is removed once the admin is set up. The token is printed on the console only when AirView runs in an interactive terminal.
- Feeder disconnects are logged with their cause, e.g. `avfsrv: feeder disconnected feeder=... reason="server: rate_limited: ..." reconnectIn=5m0s`, `reason="closed by the feeder (EOF)"`, `reason="idle timeout"` or `reason="feeder goodbye: shutting_down"`. A feeder sent away for exceeding its rate limit is also logged once at WARN.
- In aggregator mode the data port (hub) logs a client's network (`net=203.0.113.0/24`, IPv6 /48), never its address.
- Debug logging: `Environment=AIRVIEW_DEBUG=1` in a drop-in. Turn it off again; debug output is more detailed.

### 13.3 Backup

`airview backup` writes a consistent archive (databases, settings, uplink identity, AVF TLS certificate) while the service runs:

```sh
sudo -u airview AIRVIEW_DATA=/var/lib/airview /opt/airview/airview backup -o /var/backups/airview/airview-$(date +%F).zip
```

It takes consistent copies of `airview.db` and `stations.db` first (`VACUUM INTO`), staged in the data directory by default, so the data directory's file system needs free space for one copy of both databases. It deliberately does not use `/tmp`, which is a RAM-backed tmpfs on current Debian and other systemd distributions; `-tmp DIR` stages elsewhere. `airview db snapshot -o DIR` writes only the database copies.

The kit's `/opt/airview/backup.sh` wraps this for nightly use (`--online -o /var/backups/airview --keep 30`) and adds `/etc/airview`; it stages next to the output directory (`AV_BACKUP_TMP` overrides). Encrypt backups before they leave the host (they contain exact station locations, password hashes and TOTP secrets) and keep at most 30 days (RETENTION.md: deleted data must leave backups within 30 days).

### 13.4 Restore

```sh
sudo systemctl stop airview
sudo -u airview AIRVIEW_DATA=/var/lib/airview /opt/airview/airview restore /var/backups/airview/airview-2026-11-12.zip
sudo systemctl start airview
```

`airview restore` refuses to run while AirView is running, checks every file against the archive's manifest, and moves the files it replaces to `<data>/pre-restore-<time>/` (nothing is deleted). The kit's `/opt/airview/restore.sh` does the stop, restore and start for you.

After a restore, re-apply erasure requests you completed after the backup was taken (keep a list of them for that purpose).

### 13.5 Upgrade

```sh
sudo install -m 0755 airview-linux-amd64 /opt/airview/airview.new
sudo -u airview AIRVIEW_CONFIG=/etc/airview/airview.jsonc AIRVIEW_DATA=/var/lib/airview \
     /opt/airview/airview.new config check
/usr/local/sbin/airview-backup                 # always back up first
sudo mv /opt/airview/airview.new /opt/airview/airview
sudo systemctl restart airview
journalctl -u airview -n 50
sudo /opt/airview/airview config example -o /etc/airview/airview.example.jsonc   # refresh the reference
```

Database migrations run on start and are forward-only. An older binary refuses a newer `stations.db` ("schema version ... is newer than this program"); to downgrade, restore the backup taken before the upgrade.

The upgrade to the 1.1 storage layout also converts existing rows of `airview.db` and `stations.db` in the background (trails into compressed chunks, compact statistics tables); the network keeps running meanwhile. Before the first step AirView writes `/var/lib/airview/backup/airview-pre-1.1-<date>.db` and `stations-pre-1.1-<date>.db` when the disk has room for copies of both (`storage.backupBeforeMigrate`), and deletes them 14 days after the migration completed. Progress: `sudo airview db migrate --status` or `GET /api/db/stats` (`migration`); `sudo systemctl stop airview && sudo airview db migrate --now` finishes it at once. Going back to 1.0 is only possible by restoring those backups (or the backup of the upgrade step above). Feeders reconnect by themselves; their buffers cover a short restart (10 minutes in RAM, more with a disk spool).

### 13.6 Docker

The repository ships a `Dockerfile` and `docker-compose.example.yml` (no published image yet: `docker compose build`). Configure the container exactly like the service: mount `/var/lib/airview` as a volume, pass settings as `AIRVIEW_*` variables, and pass secrets as files with the `_FILE` suffix (Docker secrets). Publish 30160, 30004 and 31090 directly, and the web port only to the reverse proxy (with the proxy's address in `AIRVIEW_AGGREGATOR_TRUSTED_PROXIES`). The image's `HEALTHCHECK` uses `/healthz` (§13.1). Run `airview admin init` with `docker exec -it`.

### 13.7 Database maintenance

Both databases run the `server` storage profile (`storage.profile`, CONFIG.md "Storage profiles"): `airview.db` and `stations.db` commit queued rows every 5 s, a checkpointer back-fills the write-ahead log every 60 s or at 64 MiB (hard cap 512 MiB, truncated to 128 MiB after a reset) outside the writer's transactions, and each gets a 64 MiB writer cache, eight 16 MiB readers and a 1 GiB memory map. The registry's own connection to `stations.db` uses the same hard cap (and an 8 MiB writer / 4 MiB reader cache) while the store has the file open, so only the store's checkpointer back-fills it; tools that open the registry alone fall back to SQLite's default checkpoint. `sudo airview db profile` prints the values in force; `storage.tuning.*` changes single values (for example `"storage": {"tuning": {"readConns": 16}}` on a host with many cores and many web users). The hourly job also runs `PRAGMA optimize`, so query plans stay good on a server that is never restarted.

Retention jobs run inside AirView (minute statistics, connection log, audit log, offline purge, location estimates). Watch the database size in the admin console or with `du`. If a large deletion (many stations purged) leaves the files much larger than their content, stop the service and run `sqlite3 /var/lib/airview/stations.db 'VACUUM;'` during a quiet hour, after a backup.

---

## 14. Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| Service exits at once; journal shows a configuration error with file:line:col | Fix `/etc/airview/airview.jsonc`; check with `airview config check`. An explicitly named operator file that is missing or invalid stops the start. |
| Log says aggregator setup is required; feeders cannot connect | No admin yet: §4. |
| Every feeder fails the TLS handshake | Port 30160 is behind a TLS-terminating proxy, or you changed the certificate and feeders still pin the old one (§6). |
| Logins work but are lost immediately, or every POST fails with 403 | The proxy does not send `X-Forwarded-Proto https` from an address in `trustedProxies`, so cookies or CSRF fail; check §5. |
| All visitors appear to come from 127.0.0.1 (shared rate limits) | `aggregator.trustedProxies` does not list the proxy. |
| Live map does not update through nginx | Missing `proxy_buffering off` for `/api/events` (§5.2). |
| A compat feeder shows as "unidentified" | It sends no UUID; ask the owner to set one in readsb/Ultrafeeder. |
| A readsb feeder stays "pending" | Admission `invite`/`approve`: approve it in the registry. |
| Feeder denied with "cloned feeder" | The same identity key is connected from two places (a copied SD card). The owner runs `airview-feeder new-identity` on the copy. |
| A station is marked `suspect` | Its declared position disagrees with the survey by more than max(100 m, 3σ). The owner should check the coordinates and altitude reference (MSL vs ellipsoid). |
| `stations.db schema version ... is newer than this program` | You started an older binary; reinstall the newer one or restore the pre-upgrade backup. |
| Station connects but never syncs for MLAT | No timestamps (feeder set to *No timestamps*), clock class `none`, or no other synced station in common view. See the station's Timing section. |

---

## 15. Command reference

| Command | Purpose |
|---|---|
| `airview -headless -data <dir> [-config <file>] [-set path=value]...` | run the server |
| `airview config check [-data dir] [-config file] [-json] [-q]` | validate every configuration layer |
| `airview config print [-sources] [-json]` | effective configuration, secrets redacted |
| `airview config example [-o file]` | fully commented reference file |
| `airview config env` | every `AIRVIEW_*` variable |
| `airview admin init [-data dir] [-config file] [-user name]` | create the first admin |
| `airview admin reset [-data dir] [-config file] [-user name]` | break-glass admin reset |
| `airview serve [options]` | run headless (systemd, Docker); same options as `airview -headless` |
| `airview backup [-data dir] [-o file.zip] [-tmp dir]` | consistent online backup (§13.3) |
| `airview restore [-data dir] [-force] file.zip` | restore a backup; AirView must be stopped (§13.4) |
| `airview db snapshot [-data dir] -o dir` | consistent copies of the databases, online |
| `airview db profile [-data dir] [-json]` | storage profile of `airview.db` and `stations.db` (§13.7) |
| `airview db migrate [-data dir] --status \| --backup \| --now` | storage-layout migration: progress, backup, finish now (§13.5) |
| `airview db vacuum [-data dir]` | rewrite the databases without free pages (service stopped; needs free space of about the file size) |
| `curl http://127.0.0.1:8080/healthz` | health endpoint (§13.1) |
| `airview -version` / `airview version [-json]` | version, platform and Go version |
