# AirView server on Linux (headless)

This guide installs AirView as a headless network server (aggregator) on a Linux VPS or a home server with systemd. It works the same for a standalone station without a screen.

- **Architectures:** linux/amd64 (x86_64) and linux/arm64 (aarch64). For 32-bit Raspberry Pi OS use the Pi bundle (`airview-pi.tar.gz`).
- **Sizing:**
  - about 100 feeders: 2-4 vCPU and 4-8 GB RAM;
  - the design target of 1,000+ feeders and 10k+ aircraft: a mid-size VPS (8 vCPU, 16 GB) with SSD storage.
- **Requirements:** systemd, `tar`, `sha256sum`, and `curl` or `wget` for the one-line install and for health checks. The binary has no other dependencies: it is static Go with an embedded SQLite.
- **Docker:** see `Dockerfile` and `docker-compose.example.yml` in the source tree.

## 1. Install

### From a downloaded bundle

```sh
tar xzf airview-server-linux-amd64.tar.gz
cd airview-server-linux-amd64
sudo ./install.sh --name "My AirView Network" --public-host feed.example.org --proxy
```

### One line, with the checksum verified

```sh
curl -fsSL https://<release-site>/airview-server-install.sh | sudo sh -s -- --base-url https://<release-site> --proxy
```

The installer downloads `airview-server-linux-<arch>.tar.gz` for this CPU and checks it against `SHA256SUMS` from the same place. Then it runs the `install.sh` inside the bundle with your options. The release site must serve the files that `deploy/build.ps1` produces: the bundles, `SHA256SUMS` and `airview-server-install.sh` (a copy of `install.sh`).

### Installer options

These options apply to the first installation only. On later runs, edit `/etc/airview/airview.jsonc` instead.

| Option | Effect |
|---|---|
| `--mode aggregator` (default) or `standalone` | Run mode, written to the operator file. |
| `--name NAME` | Network display name (`aggregator.name`). |
| `--public-host HOST` | Host name that feeders use (`aggregator.publicHost`). It appears in the install one-liners that the server hands out. |
| `--proxy` | A reverse proxy on this host terminates HTTPS. AirView listens on `127.0.0.1:8080` and trusts `X-Forwarded-*` from loopback only. |
| `--http-port N` | HTTP port. The default is 80, or 8080 with `--proxy`. |
| `--no-start` | Install, but do not start the service. |

### What the installer does

1. Checks for root, systemd and the CPU type, and checks that the binary runs on this machine.
2. Creates the system account `airview`, which has no login shell and no home directory of its own.
3. Installs:

   | Path | Contents |
   |---|---|
   | `/opt/airview/airview` | the binary |
   | `/opt/airview/*.sh` | the scripts |
   | `/opt/airview/README.md` | this guide |
   | `/opt/airview/FIREWALL.md` | firewall guide |
   | `/opt/airview/proxy/` | Caddy and nginx examples |
   | `/etc/systemd/system/airview.service` | hardened unit |
   | `/etc/logrotate.d/airview` | logrotate rule |
   | `/usr/local/bin/airview` | command wrapper (see §3) |

4. Writes `/etc/airview/airview.example.jsonc`, a commented reference of every setting. It is refreshed on every upgrade.
5. Creates `/etc/airview/airview.jsonc` (`root:airview`, mode 0640) from your options. This happens only if the file does not exist yet.
6. Creates the data directory `/var/lib/airview` (`airview:airview`, mode 0750).
7. Runs `airview config check` as the service account.
8. Enables and starts the service, then waits until `/healthz` answers.

### Re-running the installer

Re-running `install.sh` on an installed system performs an **upgrade** (see §5).

## 2. First start: create the admin account (aggregator mode)

Aggregator mode keeps the feeder listeners closed until an admin account exists. That account needs a password and an authenticator app (TOTP). You can create it in either of two ways:

- **On the command line:**

  ```sh
  sudo airview admin init            # asks for the password, shows the TOTP secret/URI, asks for a code,
                                     # prints recovery codes (store them offline)
  ```

- **In the browser:** open the web interface over HTTPS (behind the reverse proxy; plain-HTTP logins from the network are refused). The one-time setup token is in a file only root and the service account can read; it is valid for 24 hours and renewed while no admin exists:

  ```sh
  sudo cat /var/lib/airview/setup-token
  ```

If you lose access to the authenticator, the break-glass reset is `sudo airview admin reset`. It needs console access to the server.

Then:

1. Mint enrol tokens for your feeders in the web interface. The default admission policy is `invite`.
2. Open the firewall: see `FIREWALL.md`, or §7 below.

## 3. Daily operation

`/usr/local/bin/airview` runs the binary with the service's settings (`AIRVIEW_CONFIG=/etc/airview/airview.jsonc`, `AIRVIEW_DATA=/var/lib/airview`) as the `airview` account. Files it creates therefore belong to the service.

| Command | Purpose |
|---|---|
| `sudo airview status` | `systemctl status airview` |
| `sudo airview logs` | follow the journal (`journalctl -u airview -f`) |
| `sudo airview config check` | validate config.json, the operator file and `AIRVIEW_*` variables |
| `sudo airview config print -sources` | show the effective settings and the origin of each override |
| `sudo airview config env` | list the environment variable of every setting |
| `sudo airview backup [--online]` | back up the data and configuration (§4) |
| `sudo airview restore FILE` | restore a backup (§4) |
| `sudo airview upgrade [--base-url URL]` | upgrade (§5) |
| `sudo airview uninstall [--purge]` | remove AirView (§6) |
| `sudo systemctl restart airview` | apply edits to `/etc/airview/airview.jsonc` |

### Configuration

AirView reads its settings in layers. Each layer below overrides the one before it:

1. `config.json` in the data directory. The web interface edits this file.
2. `/etc/airview/airview.jsonc`. The web interface shows these settings as locked.
3. `AIRVIEW_*` environment variables.

For variables and secrets, use a systemd drop-in:

```sh
sudo systemctl edit airview
# [Service]
# Environment=AIRVIEW_AGGREGATOR_PUBLIC_HOST=feed.example.org
# Environment=AIRVIEW_AGGREGATOR_OUTPUT_TOKEN_FILE=/etc/airview/output-token
```

Any variable can take a `_FILE` suffix, which reads its value from a file. Keep such files `root:airview` with mode 0640. The full reference is `docs/CONFIG.md`, or `/etc/airview/airview.example.jsonc` on the server.

### Health

`GET /healthz` answers 200 while the server runs (503 while it starts or stops; no login, no database query). Use it for uptime monitoring through the proxy, or locally:

```sh
curl -fsS http://127.0.0.1:8080/healthz >/dev/null && echo up
```

### Database profile

The installer writes `"storage": { "profile": "server" }` into `/etc/airview/airview.jsonc` for an aggregator (`auto` would pick the same in aggregator mode). The `server` profile commits every 5 s, checkpoints the write-ahead log every 60 s or at 64 MiB, and gives `airview.db` and `stations.db` large caches (64 MiB writer, 8 x 16 MiB readers, 1 GiB memory map). `sudo airview db profile` prints the values in force. Single values can be changed under `storage.tuning` (docs/CONFIG.md); a standalone server install keeps `auto` (the `standard` profile).

### Logs

- **The journal** (`journalctl -u airview`) receives everything the service writes to stdout and stderr. The unit sets `AIRVIEW_PROCESS_JOURNAL_LEVEL=info`, so every log line goes there (without it AirView sends only warnings and errors to the journal when systemd runs it, the default meant for SD-card stations). Its size is set system-wide. On a busy aggregator, limit it with a drop-in:

  ```ini
  # /etc/systemd/journald.conf.d/size.conf
  [Journal]
  SystemMaxUse=1G
  MaxRetentionSec=1month
  ```

  Then run `sudo systemctl restart systemd-journald`.

- **`/var/lib/airview/airview.log`** is AirView's own log file:
  - AirView moves it to `airview.log.1` itself at 4 MiB;
  - `/etc/logrotate.d/airview` also keeps 14 dated, compressed copies (`copytruncate`, so the service is not interrupted).

In aggregator mode AirView logs no client IP addresses (DESIGN §8.5). Keep your proxy's access logs off or short-lived for the same reason. The examples in `proxy/` turn them off.

## 4. Backup and restore

```sh
sudo airview backup                         # cold: stops the service for the copy, then starts it again
sudo airview backup --online                # no downtime; needs sqlite3 (apt install sqlite3)
sudo airview backup -o /mnt/offsite --keep 30
```

- **Contents:** the backup holds the whole data directory and `/etc/airview`:
  - `airview.db`;
  - `stations.db` with the station registry, accounts and audit log;
  - `config.json`;
  - the AVF/1 TLS key in `avf-tls/`, which pinned feeders depend on;
  - the uplink spool.
- **Left out:** map-tile and terrain caches (they are downloaded again; use `--include-cache` to keep them) and logs (`--include-logs`).
- **Archive file:** `/var/backups/airview/airview-backup-<host>-<UTC time>.tar.gz`, mode 0600. It contains password hashes, TOTP secrets and private keys, so store copies encrypted.
- **Retention:** the newest 10 archives are kept (`--keep N`, 0 = all).

Example nightly cron entry (`/etc/cron.d/airview-backup`):

```
17 3 * * * root /opt/airview/backup.sh --online --quiet --keep 14 >/dev/null
```

Restore a backup on the same machine or a new one. On a new machine, run `install.sh` first:

```sh
sudo airview restore /var/backups/airview/airview-backup-...tar.gz
```

The restore:

1. checks the archive;
2. stops the service;
3. moves the current data and configuration aside to `*.pre-restore-<time>` (nothing is deleted);
4. unpacks the backup and fixes ownership;
5. starts the service and checks its health.

Delete the `.pre-restore-*` copies once you are satisfied. `--keep-config` restores only the data directory.

## 5. Upgrade

```sh
tar xzf airview-server-linux-amd64.tar.gz && cd airview-server-linux-amd64 && sudo ./upgrade.sh
# or
sudo airview upgrade --base-url https://<release-site>
```

The upgrade runs these steps:

1. **The new binary validates your configuration first.** If it rejects the configuration, nothing changes. Fix the file, or pass `--force`.
2. Stops the service.
3. Takes a cold backup to `/var/backups/airview` (`--no-backup` skips it).
4. Replaces the binary, unit and scripts. The previous binary stays as `/opt/airview/airview.prev`. The configuration and data are not touched, except that `airview.example.jsonc` is refreshed.
5. Starts the service and waits up to 90 s for `/healthz`.
6. If the new version does not become healthy, it **rolls the binary back** and starts the old version (`--no-rollback` disables this). A newer version may already have migrated the database. If the old binary then fails as well, the script prints the restore command for the pre-upgrade backup.

## 6. Uninstall

```sh
sudo airview uninstall            # removes the service and program; keeps data, config, account, backups
sudo airview uninstall --purge    # also deletes /var/lib/airview, /etc/airview and the airview account
```

Backups in `/var/backups/airview` are never removed by the uninstaller.

## 7. Network: ports, TLS and the reverse proxy

| Port | Purpose |
|---|---|
| 80/443 | web interface and API, through the reverse proxy |
| 30160 | AVF/1 (AirView feeders, TLS 1.3 terminated by AirView itself) |
| 30004 | Beast compat (readsb `beast_reduce_plus_out`, map only) |
| 31090 | mlat-client |

`FIREWALL.md` has ready-made ufw, nftables and firewalld rules.

**Reverse proxy (HTTPS for the web interface):** `proxy/Caddyfile` sets up automatic certificates in one block. `proxy/nginx.conf` is for nginx with certbot. Either way, AirView must:

- listen on loopback only (`"http": {"port": 8080, "bindAddr": "127.0.0.1"}`);
- trust only the proxy's address (`"aggregator": {"trustedProxies": ["127.0.0.1/32", "::1/128"]}`).

`install.sh --proxy` writes both settings. AirView honours `X-Forwarded-For` and `X-Forwarded-Proto` only from `trustedProxies`. It walks the chain from the right, so clients cannot spoof their address. It marks session cookies `Secure` when the proxy reports HTTPS.

**AVF/1 certificate:** port 30160 is not HTTP and cannot go through the proxy. By default AirView creates a self-signed ECDSA certificate in `/var/lib/airview/avf-tls/`. Feeders pin it: the server's install one-liner carries the pin.

To use the proxy's CA certificate for 30160 as well:

1. Copy the certificate with `avf-cert-sync.sh`. For certbot, use it as the `--deploy-hook`. For Caddy, run it daily.
2. Set `"aggregator": {"tlsCert": "/etc/airview/tls/avf.crt", "tlsKey": "/etc/airview/tls/avf.key"}`.

Feeders that pinned the old self-signed certificate need the new pin, or an empty pin to use system roots.

## 8. Hardening built into the unit

The service runs as `airview` with only `CAP_NET_BIND_SERVICE`, which allows port 80. The unit also sets:

- `ProtectSystem=strict`; only `/var/lib/airview` is writable;
- `ProtectHome`, `PrivateTmp` and `PrivateDevices`;
- kernel tunables, modules, logs and clock protected;
- `MemoryDenyWriteExecute`;
- a `@system-service` system-call filter;
- address families limited to IPv4, IPv6, Unix sockets and netlink;
- `LimitNOFILE=65536` for 1,000+ feeder connections.

`systemd-analyze security airview` rates the unit.

Put local changes in a drop-in (`systemctl edit airview`), never in the unit file: upgrades replace it. Two examples:

- `MemoryMax=6G` together with `Environment=GOMEMLIMIT=5GiB` caps memory. The unit sets `GOMEMLIMIT=off` by default. Without it, AirView would apply its Raspberry Pi heap target (384 MiB) on arm64 servers.
- `Nice=` changes the CPU priority.

## 9. Troubleshooting

| Symptom | Check |
|---|---|
| Service does not start | `journalctl -u airview -n 100 --no-pager`; `sudo airview config check` |
| "operator configuration file" error | JSONC syntax in `/etc/airview/airview.jsonc` (`sudo airview config check` shows the line) |
| Web interface not reachable | `sudo ss -ltnp \| grep airview`; port 80 busy, so AirView fell back to 8080+ (the journal shows the URL); proxy upstream port |
| Feeders cannot connect | firewall and cloud security group (`FIREWALL.md`); no admin account yet (§2); admission policy and enrol tokens |
| Every client has the same IP in the UI or limits | `trustedProxies` missing for your proxy, or a Docker userland proxy in between |
| Disk filling up | `aggregator.retention.*` settings; `sudo du -sh /var/lib/airview/*`; journald limits (§3) |
