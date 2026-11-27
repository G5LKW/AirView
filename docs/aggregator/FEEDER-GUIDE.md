# Feeding an AirView network: guide for feeder owners

You have an ADS-B receiver and want to share its data with an AirView network (for example your club's). This guide explains what you share, what others can see, how to install the feeder, and how to claim and manage your station.

The network's own **Feed this network** page (`https://adsb.example.org/#/feeder`, linked in the sidebar) shows the exact commands and configuration lines for that network: its host name, the ports that are open and whether you need an enrol token.

> **Operators:** copy this file, replace `adsb.example.org` with your host name and add the link to your privacy notice. Check [OPERATOR-GUIDE.md](OPERATOR-GUIDE.md) §0 for the state of your build.

---

## 1. What you share, and who can see it

Your feeder sends the aircraft messages your receiver hears, with the receiver's precise timestamps, plus (if you enter one) your station's position. The network merges everyone's data into one live map and works out the position of aircraft that do not broadcast it themselves (multilateration, MLAT). MLAT needs precise receiver positions and timestamps.

**Please read this before you install:**

> "If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this."

| Who | What they can see about your station |
|---|---|
| **The public** | Nothing that locates it. The merged map does not say which receivers heard an aircraft. Your pin is **hidden** unless you opt in. Your name and statistics are shown only if you opt in, and then only figures that do not reveal where you are (message rate, uptime, aircraft per day). |
| **Other feeders** | Through the sync view, only an anonymous tag per peer that changes weekly, a distance band (under 50 km, 50-150 km, over 150 km) and timing quality. No name, position or bearing. |
| **You** (after claiming) | Everything about your own station: exact position, range and coverage, polar plots, signal and heat maps, records, timing state, MLAT contribution, connection history and your current IP. |
| **The network admin(s)** | Everything, including your exact position and IP while you are connected. Views of exact positions are recorded in an audit log that you can see in your data export. |

**Your choices:**

| Choice | Effect |
|---|---|
| Public pin: `hidden` (default) | No pin on the public map. |
| Public pin: `region` | A point placed at random inside a 0.5° cell (roughly 55 × 35 km in the UK) around you. It is computed once and does not move unless you move to another cell. |
| Public pin: `cell01` | The same in a 0.1° cell (roughly 11 × 7 km). In sparsely fed areas, where fewer than 3 stations share the surrounding 1° cell, it is shown as `region`. |
| Show name / public statistics | Off by default. |
| **No timestamps (no MLAT)** | Your feeder sends only decoded data without timing. You still appear in the merged map's data, but you do not help MLAT and the network cannot locate your receiver by timing. |
| Do not send a location | The network will try to locate your receiver from its timing (reverse MLAT), unless you also chose *No timestamps*. |

**Automatic use of surveyed positions.** If you leave the location blank, or your entered position looks wrong, the network may compute your receiver's position from its timing. If that estimate is precise (uncertainty 50 m or better) and passes consistency checks, the network uses it for MLAT straight away; you are notified and can correct or override it on your station page. The estimate is only ever shown to you and the admins.

The full details, the legal basis and your rights are in the network's privacy notice: **[link to your operator's privacy notice]**.

---

## 2. Before you install

- **Receiver and decoder.** One of: an RTL-SDR (or Airspy, SDRplay) with readsb or dump1090 running, a Mode-S Beast, or a Radarcape. AirView's feeder does not demodulate radio itself on Linux; it reads your decoder's Beast output (normally `127.0.0.1:30005`) or the Radarcape's port 10003.
- **Accurate clock.** Keep the computer's clock synchronised (chrony or systemd-timesyncd). The Pi installer checks this and offers chrony.
- **Enrol token.** Most networks are invite-only: ask the operator for a token. It is a one-time secret; do not post it.
- **Your position.** For MLAT, the antenna position matters to within about 10-20 m. Read latitude and longitude from a map in decimal degrees, and know the antenna height: either above mean sea level (MSL, what maps show) or above the ellipsoid (HAE, what GPS receivers show raw). Altitude needs a unit: `120m` or `400ft`.
- **Data volume.** A busy station sends about 15-35 kB/s (roughly 40-90 GB a month) in full mode; large networks switch stations to a selective mode of about 3-5 kB/s. Check this against any data cap on your connection.

---

## 3. Choose how to feed

| You have | Use | Section |
|---|---|---|
| A Raspberry Pi or other Linux box with readsb/dump1090 or a Radarcape on the network | `airview-feeder` with the Pi installer or the Pi SD-card image | §4 |
| A Windows PC connected to a receiver | AirView Feeder for Windows | §5 |
| AirView already running as your station (Windows app or Pi) | Its **Uplinks** setting: no extra program | §6 |
| readsb / Ultrafeeder / adsb.im and you want to keep your stack | Two lines of readsb/mlat-client configuration | §7 |

The native AirView feeder (§4-§6) encrypts everything (TLS 1.3), keeps an identity key so your station survives IP changes, buffers data during outages, gets MLAT results back and lets you claim your station with a code. The readsb/mlat-client route (§7) sends data **unencrypted** and identifies you by your UUID.

---

## 4. Raspberry Pi and Linux: `airview-feeder`

Supported: Raspberry Pi OS / Debian / Ubuntu on 64-bit ARM (aarch64), 32-bit ARMv7 and x86-64, with systemd. ARMv6 (Pi Zero/1) is not supported.

### 4.1 Install

The network's `/feeder` page shows the exact command with the server and its key pin filled in. It looks like this:

```sh
curl -fsSL https://adsb.example.org/feeder/install.sh -o /tmp/avf.sh && sudo bash /tmp/avf.sh
```

The installer:
1. checks your system (root, systemd, architecture, disk space, clock synchronisation);
2. finds your receiver: a decoder already listening on 30005, a Radarcape, an FTDI Beast, or an RTL-SDR without a decoder (then it offers to run the upstream readsb installer as a separate program). **It never removes or reconfigures your existing software or other feeders;**
3. asks for the enrol token, a station name (letters, digits, `_` and `-`, up to 32), your position and altitude (with `m` or `ft`), MSL or HAE, the antenna height above ground, the public pin level, and whether to send timestamps. Leaving the position blank shows the timestamp notice of §1 again;
4. downloads the feeder for your architecture from the network and checks its signature;
5. installs it as a service under its own user;
6. prints the local status page address, your **claim code**, and the commands for logs, updates and uninstalling, then checks that the connection works.

Unattended installs use options instead of questions:

```sh
sudo bash /tmp/avf.sh --server adsb.example.org:30160 --pin sha256/... --enrol TOKEN --name MyStation \
  --lat 51.47 --lon -0.45 --alt 30m --agl 8m --input auto --privacy hidden --non-interactive
```

| Option | Meaning |
|---|---|
| `--server host:port` / `--pin sha256/...` | the aggregator and its certificate pin (pre-filled by the network's page) |
| `--enrol TOKEN` | enrol token from the operator |
| `--name NAME` | station name |
| `--lat`, `--lon` | decimal degrees, north and east positive |
| `--alt 120m` or `--alt 400ft` | antenna altitude, unit required; MSL unless you choose HAE in the dialog |
| `--agl 8m` | antenna height above ground |
| `--input auto` or `--input host:port` | where your Beast data comes from |
| `--privacy hidden\|region\|cell01` | public pin level |
| `--no-location` | do not send a position |
| `--no-timestamps` | no MLAT; see §1 |
| `--non-interactive` | no dialogs |

Running the installer again changes the settings, with your current values as defaults.

### 4.2 Where things live

| What | Where |
|---|---|
| Program | `/opt/airview-feeder/` |
| Settings | `/etc/airview-feeder/feeder.json` |
| Identity key (keep private) and UUID | `/var/lib/airview-feeder/identity.key`, `/var/lib/airview-feeder/uuid` |
| Logs | `journalctl -u airview-feeder -f` |
| Status page | `http://127.0.0.1:8099/` on the feeder itself |

The status page listens only on the feeder itself. To open it from another computer, either use an SSH tunnel (`ssh -L 8099:127.0.0.1:8099 pi@yourpi`) or enable LAN access with a password in the settings.

### 4.3 Commands

| Command | Does |
|---|---|
| `airview-feeder status` | connection state, rates, clock class, MLAT sync |
| `airview-feeder claim` | fetches a new claim code from the network |
| `airview-feeder config set path=value` | changes one setting, e.g. `'uplinks[0].timestamps=false'` |
| `airview-feeder new-identity` | creates a new identity key (needed after cloning an SD card; see §10) |
| `airview-feeder update` | installs a signed update now |
| `airview-feeder --version` | version |

Updates are checked daily at a random time and are installed only if their signature verifies.

### 4.4 MLAT results on your own map

The network sends you positions it calculated for aircraft your station helped to locate. The feeder offers them as Beast data with the MLAT timestamp on `127.0.0.1:30105`, and can push them into readsb's results input (`127.0.0.1:30104`) so they show on your local tar1090 map. They are never sent back to the network or forwarded to other networks.

### 4.5 SD cards

On a Pi booting from an SD card the feeder keeps its outage buffer in memory only (about 10 minutes), to spare the card. On an SSD or other Linux hosts it also writes a disk buffer (256 MB by default) while the network is unreachable, and sends it later at a reduced rate.

### 4.6 Uninstall

```sh
sudo bash /opt/airview-feeder/uninstall.sh           # keeps settings and identity
sudo bash /opt/airview-feeder/uninstall.sh --purge   # removes them too
```

It never touches readsb or other feeders. Uninstalling does not delete your data at the network; see §9.

---

## 5. Windows: AirView Feeder

Two downloads from the network's `/feeder` page: an installer (recommended) and a portable ZIP.

**Windows SmartScreen.** The installer is not code-signed, so Windows may show *"Windows protected your PC"*. Check first that you downloaded it from your network's HTTPS page and, if the operator publishes checksums, compare:

```powershell
Get-FileHash .\AirViewFeederSetup.exe -Algorithm SHA256
```

Then click **More info → Run anyway**. If your browser blocks the download, choose **Keep** in the download list. For the ZIP, right-click it → **Properties** → tick **Unblock** before extracting.

The installer registers a background service (*AirView Feeder*) and a tray icon that starts when you log in. The tray menu has **Open status**, **Claim code**, **Pause**, **Restart service**, **Check for updates** and **Quit tray**. Settings and the identity key are in `%ProgramData%\AirView Feeder`, readable only by the service and administrators.

Inputs: a networked Radarcape or Beast, AVR or SBS output (dump1090-win, airspy_adsb, ModeSMixer2 or Virtual Radar Server rebroadcasts). SBS and rebroadcast inputs carry no usable timing: map only, no MLAT.

---

## 6. AirView station: Uplinks

If you already run AirView for your receiver, open **Settings → Uplinks → Add** and enter the server, port 30160, the pin shown on the network's `/feeder` page (empty if the network uses a public CA certificate) and your enrol token. Choose your pin level, name and statistics options, and whether to send your location and timestamps. Your local AirView keeps working exactly as before; the uplink only adds a copy of the data.

The same as a configuration file entry (see [../CONFIG.md](../CONFIG.md), section *Uplinks*):

```jsonc
"uplinks": [
  { "id": "club", "host": "adsb.example.org", "port": 30160,
    "serverPin": "sha256/...", "enrolToken": "...",
    "pin": "hidden", "sendLocation": true, "timestamps": true, "shareGnss": false }
]
```

`shareGnss` (default `false`) would upload a Radarcape's own GNSS position; leave it off and enter your position by hand to about 10 m instead. Results received from the network are never forwarded to other uplinks.

---

## 6a. Also feed other aggregators

AirView (station and `airview-feeder`) can feed other aggregators next to your AirView network: an `uplinks[]`
entry with `"type": "beast"` sends your receiver's data in Beast format, the way readsb's
`beast_reduce_plus_out` connector does:

```json
{ "id": "other", "type": "beast", "host": "feed.example.org", "port": 30004 }
```

- **Format.** `format` is `beast_reduce_plus_out` (default: reduced data plus a station UUID), `beast_reduce_out`
  or `beast_out` (every frame); `reduceMs` sets the reduce interval (250 ms by default).
- **UUID.** Each Beast destination gets its own random UUID by default, so your AirView identity is never shared
  with a third party. To let that aggregator join this link with your mlat-client into one station, set `uuid`
  to the UUID your mlat-client uses (`--uuid`).
- **Running mlat-client alongside.** Keep running it as before: it connects to your receiver, and its results
  go back into readsb (port 30104). AirView refuses MLAT-stamped frames on every uplink, so results are never
  forwarded anywhere.
- **Live only.** Beast outputs are not spooled: nothing is sent for the time a link was down.
- **Privacy.** Beast connections are not encrypted; your receiver data and the UUID go to that aggregator's
  operator under their terms (consent text C9). AirView never forwards MLAT results to them.
- **Check.** The status page lists each Beast output (state, frames sent, reduced, refused MLAT results, pings);
  `uplinksim -beast` (internal/uplink/cmd) is a live check against any Beast sink.

## 6b. RTL-SDR dongle without readsb

`airview-feeder` and the AirView station can read an RTL-SDR dongle directly through your own `rtl_tcp` (from
the rtl-sdr package; AirView does not ship it because it is GPL software): set a receiver input to
`"type": "rtltcp"` (default address 127.0.0.1:1234; keep rtl_tcp on loopback, it has no authentication). The
demodulator is AirView's own. Options under `input.sdr`: `gain` (`max`, `auto` or dB), `ppm`, `biasTee`,
`sampleRate`, `spawn` (start rtl_tcp for you; `rtltcpPath`, `device`). Timestamps are 12 MHz sample-clock
stamps (clock class `sdr12`), good enough for MLAT.

## 7. readsb, Ultrafeeder and adsb.im users

Keep your existing setup and add the network as one more destination. The network's `/feeder` page shows these two lines with its host filled in:

- **Map data (Beast):** readsb `--net-connector adsb.example.org,30004,beast_reduce_plus_out,uuid=<your UUID>` (Ultrafeeder: add `adsb,adsb.example.org,30004,beast_reduce_plus_out,uuid=<your UUID>` to `ULTRAFEEDER_CONFIG`).
- **MLAT:** mlat-client `--server adsb.example.org:31090 --user <name> --lat .. --lon .. --alt .. --uuid <your UUID>` (Ultrafeeder: `mlat,adsb.example.org,31090,uuid=<your UUID>`).

Things to know:
- Use **the same UUID** on both lines, so the two links are one station. Without a UUID your station is "unidentified" and you cannot claim it. Treat your UUID like a password: do not post it.
- This traffic is **not encrypted**. Your timing data, UUID and (for MLAT) declared position cross the internet in clear text.
- If your mlat-client is set to `privacy: false` (no `--privacy`), the network treats that as consent to a `region` pin and to showing your `--user` name, as other networks do. You can change both after claiming.
- On an invite-only network, a new readsb station waits for the admin's approval.
- **Claiming:** sign in to the network from a browser on the same internet connection as the feeder and enter your full UUID under **Claim**.

---

## 8. Claim your station

1. Create an account on the network's site (user name and password; no e-mail is asked for). Turn on two-factor authentication if you can, and store the recovery codes.
2. Open **Claim** and enter the code the installer printed (or `airview-feeder claim`, or the status page's **Claim** button). Codes are 8 characters, valid 24 hours, single use.
3. Your station appears under **My stations**.

Before claiming, the **Feed check** page (`/me/check`) shows whether your connection is working, without an account, when you visit from the same internet connection as the feeder.

There is no e-mail recovery. If you lose your password and recovery codes, create a new account, request a new claim code from the feeder and claim the station again; or ask the admin.

---

## 9. Your station page and your data

**My stations → your station** has: connection timeline and quality; your reception statistics, range, polar and heat maps and records; timing and MLAT state; location (declared, GNSS, surveyed with its error ellipse; **Use this location** / adjust); and **Privacy & data**, where you change the pin level, name and public statistics and see a preview of exactly what the public sees.

| You want to | Do |
|---|---|
| See everything held about you | **Export**: a ZIP of JSON files (account, stations with exact positions, location estimates, connection log, audit entries about you). |
| Stop public display | Set the pin to `hidden` and switch name and statistics off. Immediate. |
| Stop MLAT use of your receiver | Switch the feeder to **No timestamps** (`airview-feeder config set 'uplinks[0].timestamps=false'`, the installer, or the Uplinks setting). |
| Give the station up | **Unclaim**. The station keeps feeding; you lose access to its data. |
| Delete everything | **Delete account**, optionally with your stations. Station data, positions, logs and keys are removed straight away; backups expire within the period stated in the privacy notice. Aircraft positions already merged into the shared map history cannot be separated out again. |

To complain, contact the operator first (see the privacy notice). You can also complain to the Information Commissioner's Office (ico.org.uk).

---

## 10. Troubleshooting

| Problem | Try |
|---|---|
| Installer says no receiver found | Check your decoder runs and offers Beast on 30005 (`ss -ltn | grep 30005`), or give `--input host:port`. |
| "certificate pin mismatch" | The network changed its certificate. Copy the new pin from its `/feeder` page or re-run the installer. |
| "enrol token invalid or used" | Tokens are single-use and expire (7 days by default). Ask the operator for a new one. |
| "cloned feeder" | Two machines use the same identity key, usually after copying an SD card. On the copy, run `sudo airview-feeder new-identity` and claim the new station. |
| Station shows `pending` | The operator has not approved it yet. |
| Station shows `suspect` | Your entered position differs from what the network measures by more than 100 m. Check latitude/longitude, the altitude unit and MSL vs HAE. |
| MLAT never syncs | Is *No timestamps* on? Is the clock class `none` (SBS input)? Are other stations nearby? See the Timing section of your station page. |
| No MLAT results on your local map | Results go only to stations that contributed to that aircraft in the last minute and send enough timed frames. Check `results.beastPort` / `results.readsbAddr` in `feeder.json`. |
