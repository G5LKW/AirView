# 1.4 Linux server: the aggregator

This section installs AirView as a headless **aggregator** (the server of an AirView Network) on a Linux VPS or
a home server. The same kit also installs a headless standalone station (`--mode standalone`).

| Way | Files |
|---|---|
| [Server kit](#142-install-with-the-server-kit) (recommended) | `airview-server-linux-{amd64,arm64}.tar.gz`, or the one-line installer `airview-server-install.sh` |
| [Docker](#145-docker) | `airview-docker-<version>.tar.gz` (build context with the prebuilt binaries) |

Running the network once it is installed (enrol tokens, claims, MLAT, federation) is described in
[chapter 3](03-network.md); every setting is in [4.1](04-reference-1-configuration.md).

## 1.4.1 Before you start

**Server.** Any 64-bit Linux with systemd on amd64 (x86_64) or arm64 (aarch64). AirView is one static binary
with an embedded SQLite database; the kit needs only `tar`, `sha256sum`, and `curl` or `wget`.

| Network | vCPU | RAM | Disk | Inbound traffic |
|---|---|---|---|---|
| up to about 100 feeders | 2-4 | 4-8 GB | 40 GB SSD | 15-35 kB/s per busy station |
| 1,000+ feeders, 10,000+ aircraft | 8+ | 16-32 GB | 250+ GB SSD/NVMe | about 3-5 kB/s per station once stations switch to selective traffic (above `aggregator.limits.selectiveAbove`, default 100 stations) |

The operator guide (`docs/aggregator/OPERATOR-GUIDE.md`, §2.3) breaks the disk figures down by table and
retention setting.

**A host name.** Feeders connect to a name such as `feed.example.org`; browsers reach the same name over
HTTPS. Point its DNS A/AAAA records at the server before you set up the reverse proxy.

**Paperwork.** A feeder is usually at someone's home, so its position is personal data and the network
operator is its controller. Before you admit the first feeder, complete the privacy templates in
`docs/aggregator/` (`PRIVACY-NOTICE.md`, `LIA.md`, `CONSENT.md`, `RETENTION.md`).

**Decisions for the first start** (all can be changed later):

| Setting | Default | Meaning |
|---|---|---|
| `aggregator.admission` | `invite` | `invite`: a feeder needs an enrol token you mint. `approve`: you approve each new station. `open`: automatic. |
| `aggregator.auth.registration` | `open` | Whether feeder owners may create their own accounts. |
| `aggregator.defaultPin` | `hidden` | Public map pin of new stations (owners may opt in to `region` or `cell01`). |
| `aggregator.mlat.autoUseSurvey` | `true` | A surveyed station position with σh ≤ 50 m that passes the suspect checks is used for MLAT automatically. |

## 1.4.2 Install with the server kit

### From a downloaded bundle

```sh
tar xzf airview-server-linux-amd64.tar.gz
cd airview-server-linux-amd64
sudo ./install.sh --name "My AirView Network" --public-host feed.example.org --proxy
```

### One line, with the checksum verified

When the release files are on a web site (`airview-server-linux-<arch>.tar.gz`, `SHA256SUMS` and
`airview-server-install.sh` in one folder):

```sh
curl -fsSL https://<release-site>/airview-server-install.sh | sudo sh -s -- --base-url https://<release-site> --proxy
```

The script downloads the bundle for this CPU, checks it against `SHA256SUMS` from the same place, and runs the
`install.sh` inside the bundle with your options.

### Installer options

These options apply to the first installation only; later, edit `/etc/airview/airview.jsonc`.

| Option | Effect |
|---|---|
| `--mode aggregator` (default) or `standalone` | Run mode, written to the operator file. |
| `--name NAME` | Network display name (`aggregator.name`). |
| `--public-host HOST` | The host name feeders use (`aggregator.publicHost`); it appears in the install commands the server hands out. |
| `--proxy` | A reverse proxy on this host terminates HTTPS: AirView listens on `127.0.0.1:8080` and trusts `X-Forwarded-*` from loopback only. **Use it on every server reachable from the internet.** |
| `--http-port N` | HTTP port (default 80, or 8080 with `--proxy`). |
| `--no-start` | Install, but do not start the service. |

### What the installer does

1. Checks for root, systemd and the CPU type, and that the binary runs on this machine.
2. Creates the system account `airview` (no login shell).
3. Installs the binary `/opt/airview/airview`, the scripts, `README.md`, `FIREWALL.md` and the proxy examples
   (`/opt/airview/proxy/`), the hardened unit `/etc/systemd/system/airview.service`, a logrotate rule and the
   command wrapper `/usr/local/bin/airview`.
4. Writes `/etc/airview/airview.example.jsonc`, a commented reference of every setting (refreshed on every upgrade).
5. Creates the operator file `/etc/airview/airview.jsonc` (`root:airview`, mode 0640) from your options, only
   if it does not exist yet.
6. Creates the data directory `/var/lib/airview` (`airview:airview`, mode 0750).
7. Runs `airview config check` as the service account.
8. Enables and starts the service and waits until `/healthz` answers.

Running `install.sh` again on an installed system upgrades it ([1.8](01-install-07-updates.md)).

### Create the first admin

Aggregator mode keeps the feeder ports closed until an admin account exists. An admin has a password
(12 characters or more) **and** an authenticator app (TOTP). Create it on the command line:

```sh
sudo airview admin init
```

It asks for the password twice, shows the TOTP secret and `otpauth://` URI for your authenticator app, asks for
a current code, and prints 10 recovery codes: store them offline. The alternative in the browser is described
in [1.7](01-install-06-first-run.md#174-first-run-of-an-aggregator).

If you lose the authenticator, `sudo airview admin reset` sets a new password and authenticator (it needs
shell access to the server).

## 1.4.3 Reverse proxy and HTTPS

AirView speaks plain HTTP. In aggregator mode it **refuses passwords, authenticator codes and the setup token
from the network unless they arrive over HTTPS** through a proxy listed in `aggregator.trustedProxies` (logins
from the server itself are accepted). So put a reverse proxy in front of the web interface. `install.sh
--proxy` already set the AirView side:

```jsonc
"http":       { "port": 8080, "bindAddr": "127.0.0.1" },
"aggregator": { "trustedProxies": ["127.0.0.1/32", "::1/128"] }
```

### Caddy (simplest: automatic certificates)

Copy `/opt/airview/proxy/Caddyfile` to `/etc/caddy/Caddyfile` (or import it), replace `feed.example.org`, and
reload: `sudo systemctl reload caddy`. Caddy obtains and renews the certificate, redirects HTTP to HTTPS,
streams AirView's live updates (`/api/events`) without buffering and, in this example, discards its access log.

### nginx with certbot

```sh
sudo cp /opt/airview/proxy/nginx.conf /etc/nginx/sites-available/airview
sudo ln -s ../sites-available/airview /etc/nginx/sites-enabled/airview
# replace feed.example.org everywhere in the file, then:
sudo certbot certonly --webroot -w /var/www/html -d feed.example.org \
     --deploy-hook "systemctl reload nginx; /opt/airview/avf-cert-sync.sh"
sudo nginx -t && sudo systemctl reload nginx
```

The example sets `X-Forwarded-For` / `X-Forwarded-Proto`, turns buffering off for `/api/events`, allows uploads
up to 650 MB (aircraft database) and switches the access log off.

**Privacy.** In aggregator mode AirView itself logs no client IP addresses. Keep the proxy's access logs off or
short-lived for the same reason; both examples turn them off.

### The feeder port certificate (30160)

Port 30160 (AVF/1) is not HTTP and does not go through the proxy: AirView terminates its TLS itself. By default it
creates a self-signed certificate in `/var/lib/airview/avf-tls/`, and feeders *pin* it: the install command the
server hands out carries the pin (`sha256/...`). To use your proxy's CA certificate on 30160 instead, copy it
with `/opt/airview/avf-cert-sync.sh` (as the certbot deploy hook above, or daily for Caddy) and set
`aggregator.tlsCert` / `aggregator.tlsKey` to the copied files (`/etc/airview/tls/avf.crt`, `avf.key`). Feeders
that pinned the old self-signed certificate then need the new pin, or an empty pin to use the system roots.

## 1.4.4 Firewall

Open only these ports (`/opt/airview/FIREWALL.md` has the full guide):

| Port | Who connects | Open to |
|---|---|---|
| 22/tcp | you (SSH); **allow it before you enable a firewall** | your addresses if possible |
| 80/tcp, 443/tcp (+443/udp for HTTP/3 with Caddy) | browsers, certificate challenges | everyone |
| 30160/tcp | AirView feeders (AVF/1, TLS) | everyone |
| 30004/tcp | readsb / Ultrafeeder `beast_reduce_plus_out` | everyone, or known feeder addresses |
| 31090/tcp | mlat-client | everyone, or known feeder addresses |
| 31090/udp | mlat-client UDP, only with `aggregator.mlat.udp: true` | as 31090/tcp |

Do **not** open 8080 (AirView behind the proxy), 30154 (the hub; in aggregator mode it accepts only the admin
login), or the merged outputs (`aggregator.beastOutPort`, `aggregator.sbsOutPort`) except to specific consumers.

With ufw (Ubuntu, Debian):

```sh
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow OpenSSH
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 443/udp
sudo ufw allow 30160/tcp comment 'AirView AVF/1 feeders'
sudo ufw allow 30004/tcp comment 'AirView Beast compat'
sudo ufw allow 31090/tcp comment 'AirView mlat-client'
sudo ufw enable
```

With firewalld (RHEL, Fedora, openSUSE):

```sh
sudo firewall-cmd --permanent --add-service=http --add-service=https
sudo firewall-cmd --permanent --add-port=30160/tcp --add-port=30004/tcp --add-port=31090/tcp
sudo firewall-cmd --reload
```

`FIREWALL.md` also has an nftables rule set and an optional rate limit for new feeder connections.

**Cloud firewalls.** Most VPS providers (security groups, cloud firewalls) filter in front of the machine as
well: open the same ports there, or feeders time out although the host firewall allows them. Some images
(for example Oracle Cloud) also ship iptables rules that block everything but SSH.

**Check from another machine:**

```sh
nc -vz feed.example.org 30160
nc -vz feed.example.org 30004
nc -vz feed.example.org 31090
curl -sI https://feed.example.org/ | head -n 1
```

## 1.4.5 Docker

`airview-docker-<version>.tar.gz` is a ready Docker build context: a `Dockerfile` (distroless image, runs as
uid 65532), `docker-compose.example.yml` (aggregator plus an optional Caddy HTTPS front end), the static
binaries for amd64 and arm64, and a health-check helper. No image is published to a registry: you build it
yourself (BuildKit is needed, the default since Docker 23).

```sh
tar xzf airview-docker-1.0.1.tar.gz && cd airview-docker-1.0.1
docker build -t airview:1.0.1 --build-arg VERSION=1.0.1 .
cp docker-compose.example.yml docker-compose.yml   # edit the values marked "<- edit"
docker compose run --rm airview config check       # validate the AIRVIEW_* settings
docker compose up -d
```

- **Configuration** is done with `AIRVIEW_*` variables in `docker-compose.yml` (one per setting; secrets from
  files with `AIRVIEW_<NAME>_FILE`), optionally plus an operator file mounted at `/etc/airview/airview.jsonc`.
  The example sets `AIRVIEW_MODE=aggregator`, the network name, the public host, admission `invite` and the
  trusted proxy network `172.30.160.0/24`.
- **HTTPS:** either start the bundled Caddy (needs ports 80/443 and DNS for the domain):
  `AIRVIEW_DOMAIN=feed.example.org docker compose --profile proxy up -d`, or use a reverse proxy on the host
  against `127.0.0.1:8080` (section 1.4.3).
- **First admin:** `docker compose exec airview airview admin init`.
- **Data** lives in the volume `airview-data` (`/data` in the container). The container's HTTP port is 8080 and
  it answers `GET /healthz`.
- **Firewall:** Docker publishes ports through its own iptables rules, past ufw. Publish only what must be
  public; the example binds 8080 to `127.0.0.1` and publishes 30160, 30004 and 31090.

Backup, restore and upgrade commands are in the comments at the top of `docker-compose.example.yml`.

## 1.4.6 Serving the feeder installer and feeder updates

The aggregator serves the Raspberry Pi feeder installer at `https://<host>/feeder/install.sh`, with the
network's address (from `aggregator.publicHost` and `aggregator.avfPort`), the AVF certificate pin and the
release key already filled in ([1.5](01-install-04-pi-feeder.md)). The installer downloads the feeder binary
from the same server, so publish the signed feeder release there:

1. Copy the folder `release/feeder/` of the release (`manifest.json`, `manifest.json.sig`, `dl/<target>`) to the
   server, for example to `/var/lib/airview/feeder-release/`, readable by the `airview` account.
2. Set in `/etc/airview/airview.jsonc`:

   ```jsonc
   "aggregator": {
     "publicHost": "feed.example.org",
     "feederReleaseDir": "/var/lib/airview/feeder-release",
     "feederManifestKey": "<release public key, base64>"
   }
   ```

3. `sudo airview config check && sudo systemctl restart airview`.

The server then answers `/feeder/manifest.json`, `/feeder/manifest.json.sig` and `/feeder/dl/<target>`; installed
feeders check the same URLs for updates. Without `feederReleaseDir` these URLs answer 404 and the installer needs a
local binary (`--binary`, or the feeder Pi bundle).

## 1.4.7 Daily commands and file locations

`/usr/local/bin/airview` runs the binary with the service's settings (`AIRVIEW_CONFIG=/etc/airview/airview.jsonc`,
`AIRVIEW_DATA=/var/lib/airview`) as the `airview` account:

| Command | Purpose |
|---|---|
| `sudo airview status` / `sudo airview logs` | service status / follow the journal |
| `sudo airview config check` | validate `config.json`, the operator file and `AIRVIEW_*` variables (exit 0 = valid) |
| `sudo airview config print -sources` | effective settings and where each override comes from (secrets redacted) |
| `sudo airview config env` | the environment variable of every setting |
| `sudo airview backup [--online]` / `sudo airview restore FILE` | backup and restore (`/var/backups/airview`) |
| `sudo airview upgrade [--base-url URL]` | upgrade ([1.8](01-install-07-updates.md)) |
| `sudo airview uninstall [--purge]` | remove AirView |
| `sudo systemctl restart airview` | apply edits of `/etc/airview/airview.jsonc` |

| What | Where |
|---|---|
| Binary, scripts, proxy examples | `/opt/airview/` |
| Operator file / reference | `/etc/airview/airview.jsonc` / `/etc/airview/airview.example.jsonc` |
| Network data (aircraft, flights, positions) | `/var/lib/airview/airview.db` |
| Registry, accounts, claims, tokens, station statistics, audit log | `/var/lib/airview/stations.db` |
| Settings saved from the web interface | `/var/lib/airview/config.json` |
| AVF/1 certificate (self-signed default) | `/var/lib/airview/avf-tls/` |
| Logs | `journalctl -u airview`, `/var/lib/airview/airview.log` |
| Health | `curl -fsS http://127.0.0.1:8080/healthz` (200 while running) |

`stations.db` holds password hashes, TOTP secrets and keys: treat the data directory and every backup as
confidential. Settings layers, the hardening of the unit and troubleshooting are in
`/opt/airview/README.md` and [4.4](04-reference-4-troubleshooting.md).

## 1.4.8 Uninstall

```sh
sudo airview uninstall            # removes the service and program; keeps data, config, account, backups
sudo airview uninstall --purge    # also deletes /var/lib/airview, /etc/airview and the airview account
```

Backups in `/var/backups/airview` are never removed by the uninstaller.
