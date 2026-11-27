## Privacy track: AirView aggregator mode

Scope: how ADS-B aggregators and MLAT networks protect feeder locations, what each party can see, GDPR for a UK/EU community operator, location fuzzing, inference attacks, and a concrete design for AirView. I treated `D:\AirView` as read-only and copied no GPL code; algorithms are described in my own words.

---

### 1. How existing networks handle feeder privacy

**mlat-server (wiedehopf fork, AGPL-3.0)** and **mlat-client (GPL-3.0+)**
- Source: https://github.com/wiedehopf/mlat-server and https://github.com/wiedehopf/mlat-client
- The client has an optional `--privacy` switch, off by default. It sends a privacy setting during the handshake. Per the help text, it "currently" only hides the receiver's marker on the coverage/sync maps.
- The server's coordinator rewrites its state files every 15 s, using an atomic replace (write a temp file, then rename):
  - `sync.json` is keyed by receiver name. It holds the peer sync state, a bad-sync score and a map position.
  - `clients.json` holds UID/UUID, source IP and port, message rate, and the exact lat/lon/alt as a 6-decimal coordinate string.
  - `aircraft.json` is the third file.
- How the map position is fuzzed:
  - Privacy on: the map position is set to null.
  - Privacy off: lat/lon are snapped to a 0.05° grid. A uniform random offset of up to 0.05° is then added, and the result is rounded to 2 decimals. Altitude is rounded to 50 m.
  - The random offset is drawn again on every location update, so averaging the reports only recovers the 0.05° cell (about 5.5 x 3.4 km at 52°N).
- **Important gap:** `clients.json` always contains the exact position and the IP address, whatever the privacy flag says. Operators who serve the state directory publicly leak everything. AirView must never have a public equivalent of this file.
- The sync map/table lists each receiver's peers. Peers share synchronisation aircraft only when their coverage overlaps, so the peer list is location evidence in its own right.
- Per-receiver health stats exposed: `peer_count`, `bad_syncs` (0–6), `outlier_percent`, `message_rate`.
- The AGPL means a networked deployment must offer its source. AirView should implement its own design rather than port this code.

**OARC (https://adsb.oarc.uk/)**
- The OARC site is tar1090 plus the readsb aggregator and an mlat-server, with a public "MLAT Sync Map" (`/map/`) and "MLAT Sync Table" (`/sync/`).
- Its "Feed Check" page (https://adsb.oarc.uk/myip/) identifies the feeder by the requester's IP plus the client UUID. It warns when Beast and MLAT arrive under different UUIDs, or when a null UUID is sent.
- In practice, a feeder's own stats are keyed by "same IP as the feeder".

**ADSBExchange**
- The feeder map is "approximate for privacy"; the method is not published.
- https://www.adsbexchange.com/myip/ only works from the feeder's own network, so it is keyed by IP. Its link to a map of only your feeder's aircraft can be bookmarked and used anywhere.
- Per-feeder stats are at `/api/feeders/?feed=<UUID>`. The UUID therefore acts as a bearer secret: anyone who has it sees the stats.
- Users report that the public feeder-map position can be wildly off (https://adsbx.discourse.group/t/map-not-showing-my-location/125).

**adsb.lol**
- The Docker client turns the MLAT `--privacy` flag on by default.
- "My Map" is reached by IP and redirects to a per-feeder subdomain. If several feeders share one IP, the hashes are combined, which can break the subdomain.
- The location is stated to be "always approximate", possibly off by several hundred km.
- Source: https://www.adsb.lol/docs/feeders-only/my-map/

**TheAirTraffic**
- Policy: https://theairtraffic.com/privacy
- Collects IP, UUID, an optional name, the location and the aircraft data.
- The public map shows UUID, name and an approximate location; IP, exact location and per-feeder aircraft lists stay private.
- The MLAT map uses "accurate positions randomized for privacy".
- The policy gives no retention period and no legal basis. It is a useful example of what a community policy should improve on.

**FlightAware**
- Owners logged in see about 5-decimal coordinates; logged-out viewers see about 2 decimals.
- A site setting obscures the location.
- MLAT needs an accurate position, around 100 m by forum reports.
- One user reported that a public coverage map leaked exact coordinates.
- Feeders are linked to accounts by a "claim" flow.
- Source: https://discussions.flightaware.com/t/location-privacy/94104

**Flightradar24**
- Applicants mark a precise location when applying.
- Owners may only move a registered feeder within 100 NM and not to another country.
- I found no published fuzzing method.
- Source: https://support.fr24.com/support/solutions/articles/3000115030-how-to-update-feeder-coordinates

**OpenSky**
- Promises not to reveal the antenna location on request.
- Academic work shows the promise is weak (section 4).

**Pattern common to all of them**
- The operator always knows the exact location; MLAT needs it.
- Owners see precise data about themselves.
- The public sees either nothing or a coarsened point.
- Self-service identity is usually an IP match, or a UUID used as a bearer token. Both are weak:
  - with NAT/CGNAT, many people share one IP;
  - UUIDs get pasted into forum posts and then act as passwords.

---

### 2. GDPR / UK GDPR for a community operator

**What counts as personal data**
- **Home location:** a feeder is usually at a home, so its exact location is personal data. Combined with a callsign or display name it directly identifies a person. Amateur callsigns are easy to look up, so callsign plus a coarse location is close to identification.
- **IP addresses:** the ICO lists IP addresses as online identifiers that *may* be personal data (https://ico.org.uk/for-organisations/uk-gdpr-guidance-and-resources/personal-information-what-is-it/what-is-personal-data/what-are-identifiers-and-related-factors/).
  - CJEU *Breyer* C-582/14: a dynamic IP is personal data for a website operator that has a legal, reasonably likely means of attributing it.
  - CJEU *EDPS v SRB* C-413/23 P (4 Sep 2025): pseudonymised data stays personal for the controller that holds the key, but may not be personal for a recipient who cannot re-identify (https://www.cliffordchance.com/insights/resources/blogs/talking-tech/en/articles/2025/09/pseudonymized-data-after-edps-v-srb.html).
  - Consequence: hashed IPs and feeder IDs held by the operator are still personal data. Fuzzed public output is a safer category, but only if re-identification is not reasonably likely, which section 4 shows can be hard.

**Exemptions**
- The domestic-purposes exemption does not cover a club operating a service for other people.
- ICO fee: the not-for-profit exemption is narrow. The body must be set up as not-for-profit and only process data needed for membership/support (https://ico.org.uk/for-organisations/data-protection-fee/data-protection-fee/exemptions). Use the ICO self-assessment.
- All other duties still apply even when no fee is due.

**Lawful basis**
- Legitimate interests (with a written LIA), or contract/membership terms, fit better than consent for "you must give an exact location for MLAT".
- Use consent (opt-in, withdrawable) for optional public display: map pin, name, public stats.

**Operator obligations**
- **Transparency:** a privacy notice stating:
  - who the controller is;
  - what is collected (IP, feeder key, exact location, Beast and MLAT timing data, stats);
  - who sees what (the matrix below);
  - retention periods;
  - rights, and the ICO complaint route.
- **Rights:**
  - Access/export: an owner download of everything held about their feeder.
  - Erasure: deleting a feeder purges its location, stats and logs.
  - Aircraft data already merged into the shared picture cannot be un-merged; say so in the notice.
- **Security (Art. 32):** access control, an audit log for admin views of exact locations, and encrypted backups.
- **Aircraft data:** ICAO addresses of privately owned aircraft can themselves be personal data. That is out of scope for this track but should be noted in the notice.

**Also to check**
- UK Wireless Telegraphy Act 2006 s.48 (interception/disclosure; amended 2016–2018, see s.48(3A)). There is no Ofcom position specific to ADS-B: https://www.legislation.gov.uk/ukpga/2006/36/section/48
- The Data (Use and Access) Act 2025 amendments to UK GDPR, e.g. recognised legitimate interests. I did not research these in detail.

---

### 3. Location fuzzing techniques

- **Rounding / grid snapping** (FlightAware 2 decimals, mlat-server 0.05°)
  - Deterministic and cannot be averaged out, but the grid is public.
  - A point near a cell edge is still known to within one cell.
  - The cell size should scale with how sparsely populated the area is.
- **Random offset per publication** (mlat-server)
  - Repeated independent draws average out.
  - Two different fuzzers (e.g. sync map and coverage map) can be intersected.
  - Rule: compute **one** public position per feeder, persist it, and reuse it everywhere.
- **Keyed deterministic offset**
  - offset = PRF(server secret, feeder ID) inside the snapped cell.
  - Stable, looks random, cannot be averaged.
  - Rotate it only if the feeder moves to a new cell.
- **Geo-indistinguishability / planar Laplace** (Andrés et al., https://arxiv.org/abs/1212.1984)
  - Gives formal ε-DP for a single report.
  - Independent noise on n reports only gives about nε, so the budget erodes (Chatzikokolakis et al., PETS 2014, https://www.petsymposium.org/2014/papers/Chatzikokolakis.pdf).
  - Use it once, then persist the result, the same as the keyed offset.
- **k-anonymity / region display**
  - Show the feeder only at region level (a county or 0.25–0.5° cell) when few homes or few feeders are in the cell.
  - k-anonymity over *feeders* does not protect a lone house. What matters is the number of plausible dwellings in the cell, which suggests coarser cells in rural areas or a user-chosen precision.
- **Suppression**
  - Hide the feeder entirely. This should be the default, as on adsb.lol.

---

### 4. Inference attacks: fuzzing the pin is not enough

Each item is a way to recover a hidden feeder location.

1. **Coverage geometry.**
   - A per-feeder polar range plot, range outline or heatmap is centred on the receiver.
   - AirView's own `internal/analytics/guess.go` locates a receiver from the all-time 0.05° heat grid alone, fusing centroid, RSSI inverse-square fit, coverage floor and edge symmetry. The user reports about 30-mile accuracy.
   - Any per-feeder coverage product published to others is a locator, and it improves with more data.
2. **Coverage floor / low-altitude and surface detections.**
   - The minimum altitude seen per bearing follows the radio horizon of the receiver site.
   - Seeing aircraft taxiing at an airfield implies line of sight to it.
3. **RSSI per aircraft.** Signal strength against range fits the inverse-square law and points to the site.
4. **MLAT and timing data.**
   - Sciancalepore, Alhazbi & Di Pietro (2019), "Receivers location privacy in avionic crowdsourced networks", show that hidden OpenSky receivers can be located from timestamps plus the positions of non-hidden receivers. Reported accuracy is roughly 0.9–2.6 km. Their countermeasures (corrupting timestamps, nulling them, or sharing one identifier across several receivers) each cost data usefulness.
   - With GPS-timestamped Radarcape-class receivers and ADS-B aircraft as reference beacons, a TDOA solver can put a receiver within tens of metres. Our planned MLAT engine is exactly this capability.
   - Raw per-receiver timestamped frames, per-pair clock offsets, propagation delays and peer residuals must never leave the master.
5. **Sync peer lists.** "A syncs with B, C, D" bounds A to the intersection of their coverages. Combined with fuzzed peer positions, this is trilateration-lite.
6. **Per-feeder aircraft lists or maps** (ADSBx "only my feeder" map, adsb.lol My Map). These reveal coverage, and so the location, to anyone holding the link.
7. **Records** (farthest contact plus bearing, `max_dist_brg`) and the `flights.min_dist_km` closest-approach data. These point at the site.
8. **Linkage.**
   - Display name or callsign, combined with a coarse cell, identifies a person.
   - Feed up/down times reveal power cuts and when someone is home.
   - The IP's geolocation can also be linked.
9. **Inherent residual risk.**
   - The combined public map still shows low-altitude aircraft only where some feeder has coverage.
   - A lone rural feeder therefore makes an "island" of coverage.
   - This cannot be removed without degrading the service. Document it in the notice.

**What this means for AirView's own Locate Me**
- The improved self-location (heat grid plus MLAT/TDOA against GPS-timed peers) is a privacy attack when pointed at someone else's data.
- In aggregator mode it must run only for:
  - the owner, against their own feeder (to fill in a missing location);
  - the master admin.
- Its result is personal data and is stored like a self-reported location.
- The honest promise to feeders is: "the operator can always know where you are; other users and the public cannot." No design that supports MLAT can hide a location from the operator.

---

### 5. Findings in the current codebase (read-only)

1. **Loopback is always treated as admin** (`internal/server/auth.go` `isAdmin`: `if isLoopback(r) { return true }`).
   - If an aggregator runs behind a reverse proxy on the same host (nginx/Caddy for TLS, the usual setup), every request looks like loopback, so **everyone becomes admin**.
   - Also, when no `adminPasswordHash` is set, every client is admin.
   - Aggregator mode needs:
     - an explicit trusted-proxy setting, honouring X-Forwarded-For only from configured proxy IPs (`clientIP` currently ignores XFF, which is correct for standalone);
     - loopback-is-admin turned off, or tied to a local socket;
     - a mandatory admin credential.
2. **GET endpoints are open to any client.**
   - Only mutating endpoints check `isAdmin` (about 15 call sites).
   - `/api/guess`, `/api/stats/*`, `/api/antenna/*`, `/api/timing/*`, `/api/config` (receiver position), `/api/history/*` and `/api/export/*` are readable by anyone who can reach the port.
   - That is fine for a personal station. In aggregator mode, reads must be scoped per role by default (deny unless public).
3. **The hub data port sends station metadata** (`model.Receiver` = exact position) plus raw frames with receiver timestamps to any logged-in client, and it uses one shared password.
   - In aggregator mode, a viewer stream must carry only merged, public-class data.
   - Raw timestamped per-feeder frames must stay inside the master.
4. **Existing tables keyed by antenna** (`heat`, `coverage`, `range_hour`, `records`, `signal_hist`, `elev_hist`) are all location-revealing.
   - In aggregator mode, key them by feeder ID and classify them as owner-only.

---

### 6. Recommended privacy design

**Identities**
- **Feeder identity: a keypair, not an IP.**
  - The installer (RPi script / Windows feeder) generates an Ed25519 keypair. Go has this in the standard library (`crypto/ed25519`), so it stays CGO-free.
  - Feeder ID = base32 of the first 16 bytes of SHA-256(pubkey).
  - On connect, the server sends a nonce and the feeder signs it plus the session parameters. This mirrors the existing hub HMAC challenge, but per feeder, and it cannot be replayed.
  - The private key never leaves the feeder.
  - Do not use a UUID as a bearer secret.
- **Claiming: an owner account linked to feeders.**
  - The feeder's local UI or installer output shows a short one-time claim code (e.g. 8 base32 characters, 24 h TTL, single use, rate-limited).
  - The owner signs in to the aggregator web UI (password plus optional TOTP, or a magic link) and enters the code. The feeder is now linked.
  - One owner can hold several feeders.
  - Transfer and unclaim are available, with an audit log.
- **IP fallback ("Feed Check").**
  - A visitor whose IP matches a currently connected feeder sees only non-locating health data for every feeder on that IP: connected, protocol, message rate, aircraft count, MLAT sync on/off, clock status.
  - Match on the exact IPv4 address or the IPv6 /64.
  - Under CGNAT this can show other people's feeders, which is why it is limited to non-locating fields.
  - It shows a "Claim this feeder" prompt that still needs the claim code from the device.
  - Never grant location, coverage or per-feeder maps by IP.

**Roles and visibility**

| Data | Public | Feeder owner (own feeders) | Master admin |
|---|---|---|---|
| Merged live aircraft map | yes | yes | yes |
| Which feeders saw an aircraft | count only, if any | "seen by my feeder" flag | full list |
| Feeder pin on public map | hidden by default; opt-in fuzzed (region / ~10 km) | exact | exact |
| Display name / callsign | opt-in only | yes | yes |
| Per-feeder stats (rate, uptime, aircraft, positions, MLAT health) | opt-in, non-locating fields only | all | all |
| Coverage, range outline, polar, heatmap, records with bearing, RSSI, min-altitude, Locate Me | never | yes | yes |
| Per-feeder "my aircraft" map | never public; owner session only, no bearer link | yes | yes |
| MLAT sync peers and residuals | never | own peers as fuzzed IDs/positions plus health scores | full |
| Raw timestamped frames, clock offsets | never | own feed's latency/clock stats | full |
| IP address | never | own current IP | yes, audit-logged |

**Public position algorithm**
- Snap to a cell of user-chosen size: hidden (default), 0.5° region, or 0.1°.
- Place the point at cell centre + PRF(server secret, feeder ID) inside the cell.
- Compute it once and store it as `public_lat/lon`.
- Every public view, including the sync map, uses only this stored value.
- Recompute only when the owner moves to another cell.

**Safeguards**
1. **Separate API surfaces and types.**
   - Namespaces: `/api/public/*`, `/api/me/*`, `/api/admin/*`.
   - Public DTO structs have no exact coordinate fields, so a leak cannot happen through serialisation.
   - Golden tests assert that public JSON never contains exact coordinates, IPs or raw timestamps.
2. **Deny by default** in aggregator mode, with an explicit role check per handler.
3. **Admin hardening:**
   - mandatory admin password plus TOTP (HMAC-SHA1 is in the Go standard library);
   - no loopback-equals-admin behind a proxy;
   - a trusted-proxy list;
   - an audit log of admin views of exact locations and IPs.
4. **Data minimisation and retention** (suggested defaults, to be set in the privacy notice):
   - Raw IP: kept in memory for the live connection only.
   - Connection logs: store HMAC(daily-rotating salt, IP) and drop it after 14–30 days.
   - Per-feeder minute stats: 30 days.
   - Hourly rollups: 13 months.
   - Feeder deletion purges location, stats and keys within 30 days, backups included.
   - Unclaimed feeders that stay offline for 90 days are auto-purged.
5. **Locate Me / TDOA self-location:**
   - Owner-only for their own feeder, and admin.
   - The result is stored as `location_source = estimated`.
   - It is used for MLAT only after the owner confirms it, or with a visible "estimated" flag.
   - It is never offered for someone else's feeder.
6. **MLAT outputs:** public aircraft records carry no receiver list. Sync data is aggregated before display.
7. **Transport:**
   - Feeder links should be TLS (or at least signed sessions); the Beast payload is not secret, but the identity must be authenticated.
   - Rate-limit claim attempts and logins.
8. **Owner controls:**
   - export everything held about the feeder;
   - delete the feeder;
   - choose map precision;
   - toggle public stats and name.
9. **Privacy notice:**
   - a short notice in the installers and web UI, plus a feeder-facing statement of residual risks (coverage islands, operator knowledge);
   - a template LIA;
   - a retention schedule.
10. **Standalone mode is unchanged:** the local view keeps its current single-owner behaviour.

**Licenses of referenced software**
- mlat-server (wiedehopf/mutability): AGPL-3.0
- mlat-client: GPL-3.0+
- readsb and tar1090: GPL
- AirView should reimplement from protocol and algorithm descriptions only.

Recommendations:
- Use a per-feeder Ed25519 keypair as the identity, generated by the RPi/Windows installer; feeder ID = SHA-256(pubkey) prefix; authenticate on connect by signing a server nonce. Do not use a UUID as a bearer secret.
- Link feeders to owner accounts with a one-time claim code shown on the feeder (short TTL, single use, rate-limited). Use IP matching only for a non-locating 'Feed Check' view that prompts to claim, and match IPv6 on the /64.
- Make public map pins hidden by default and opt-in. If shown, snap to a cell of the owner's chosen size (region or 0.1 degree), place the point with a keyed deterministic offset, compute it once, and reuse that stored value in every public view, sync map included.
- Classify all per-feeder coverage products as owner/admin only: range outline, polar, heat grid, records with bearing, RSSI, min-altitude floor, Locate Me, per-feeder aircraft map. Public per-feeder stats are opt-in and limited to non-locating fields.
- Never publish a clients.json equivalent, raw per-receiver timestamped frames, per-pair clock offsets, MLAT residuals or receiver lists on aircraft. Show sync peers to owners only as fuzzed IDs.
- Restrict Locate Me and the TDOA-based self-location to the owner's own feeder and the master admin. Store the result as an 'estimated' location and use it for MLAT only after the owner confirms it or with a visible flag.
- In aggregator mode, fix the auth model: drop loopback-equals-admin when behind a reverse proxy, add a trusted-proxy list for X-Forwarded-For, require an admin password plus TOTP, and make all GET endpoints deny-by-default with explicit role checks under /api/public, /api/me and /api/admin.
- Use separate Go DTO types for public responses that have no exact-coordinate or IP fields, and add golden tests asserting that public JSON never contains them.
- Scope the hub data port per role in aggregator mode: viewers get only merged public-class data, and raw timestamped per-feeder frames plus station metadata stay inside the master.
- Data minimisation and retention: raw IPs in memory only; logs keep a salted daily HMAC of the IP for 14-30 days; minute stats 30 days; hourly rollups 13 months; purge on feeder deletion and after 90 days offline when unclaimed; audit-log admin views of exact locations and IPs.
- Ship a privacy notice template (controller, data collected, visibility matrix, retention, rights, ICO complaint route, residual risks such as coverage islands and operator knowledge), a template legitimate-interests assessment, and owner self-service export and delete.
- Implement all MLAT and privacy logic from scratch: mlat-server is AGPL-3.0, mlat-client GPL-3.0+, readsb and tar1090 GPL.

---

# Research track: installers for an AirView Feeder (Raspberry Pi and Windows), and staying compatible with existing feeders

No file under D:\AirView was changed. I only read the code to understand how it is built today: `docs/ARCHITECTURE.md` (hub data port, runtime modes) and `deploy/pi/install.sh` / `uninstall.sh` (a tarball with `sudo ./install.sh`, a system user `airview`, `/opt/airview` + `/var/lib/airview`, a systemd unit, `--purge` on uninstall).

---

## 1. How the existing feeder installers work

### 1.1 ADS-B Exchange feedclient (MIT license)
Source: https://github.com/ADSBexchange/feedclient (files `setup.sh`, `configure.sh`, `update.sh`, `scripts/adsbexchange-feed.sh`)
- **Install:** the user downloads `https://adsbexchange.com/feed.sh` to /tmp and runs it with `sudo bash`. They do not pipe it straight into bash, so they can look at it first. The script exits unless it runs as root. It also refuses to run on the ADSBx prebuilt image, which it detects by `/boot/adsb-config.txt`.
- **Prompts (whiptail):**
  - A confirmation dialog.
  - A feeder name for the MLAT map. Entering `0` turns MLAT off.
  - A reminder that MLAT needs exact antenna coordinates.
  - Latitude and longitude, asked again until they are in range.
  - Altitude as a number with a unit, e.g. `120m` or `400ft`. It is skipped if MLAT is off.
  - The name is cleaned to letters, digits, `_`, `-` and spaces.
- **Receiver auto-detect:** the default input is `127.0.0.1:30005` with type `dump1090`. If the hostname is `radarcape`, or an `rcd` process is running, it switches to `127.0.0.1:10003` with type `radarcape_gps`. This is the GPS-timestamped Beast port, the same source AirView uses.
- **Config file `/etc/default/adsbexchange`:**
  - INPUT and INPUT_TYPE
  - REDUCE_INTERVAL=0.5
  - USER, LATITUDE, LONGITUDE, ALTITUDE
  - UAT_INPUT 127.0.0.1:30978
  - PRIVACY
  - MLATSERVER `feed.adsbexchange.com:31090`
  - TARGET: `feed1.adsbexchange.com:30004` plus a second target `feed2.adsbexchange.com:64004`
  - MLAT result outputs: RESULTS beast connect 127.0.0.1:30104, RESULTS2 basestation listen 31003, RESULTS3 beast listen 30157, **RESULTS4 beast connect 127.0.0.1:30154**
- **Components:**
  - Their own readsb fork is built from git as `feed-adsbx`. It runs `--net --net-only` with `beast_in` and `uat_in` connectors marked `silent_fail`, `--net-beast-reduce-interval`, `--lat/--lon`, `--write-json` into /run, and a UUID file.
  - Their mlat-client fork is installed into a Python venv under `/usr/local/share/adsbexchange/venv`.
  - Installed versions are written to `*_version` files and compared with `git ls-remote`, so a component is only rebuilt when it changed.
  - If the install fails, the previous venv is restored from a backup.
- **Services and user:** two systemd units, `adsbexchange-feed` and `adsbexchange-mlat`. If lat, lon or user is 0, the MLAT unit is disabled. A system user `adsbexchange` is created. The UUID comes from `create-uuid.sh`.
- **Update, reconfigure, uninstall:**
  - Update: `feed-update.sh`. Reconfigure: run `feed.sh` again.
  - Uninstall: `/usr/local/share/adsbexchange/uninstall.sh`, or `systemctl disable --now` on both units.
- **Status pages:** https://www.adsbexchange.com/myip/ finds the feeder **by the public IP of the browser**. It shows ADS-B status, MLAT status, the feed UID and stats. There is also https://map.adsbexchange.com/mlat-map. An optional stats package and an optional local tar1090 at `/adsbx` exist.
- **Design rule:** it leaves existing feed clients alone.

### 1.2 adsb.fi and airplanes.live (both copied from the ADSBx layout)
Sources: https://github.com/adsbfi/adsb-fi-scripts and https://github.com/airplanes-live/feed. airplanes.live is MIT; adsb.fi's LICENSE.md was not checked.
- **Bootstrap:** installs git, wget, unzip and whiptail. Does a shallow clone into `/usr/local/share/<name>/git`. If the clone is already there it does fetch + hard reset. If git fails it falls back to downloading a zip. Then it runs `configure.sh` and `update.sh`.
- **Services and config:** `<name>-feed` and `<name>-mlat`, config in `/etc/default/<name>`, MLAT results sent back to 127.0.0.1:30104.
- **Ports:** feed 30004, MLAT 31090.
- **Update:** `update.sh` updates without asking the questions again.
- **Uninstall:** a script, with `systemctl disable --now` as the fallback.
- **Prerequisite:** a decoder must already be installed. These scripts are feed-only.

### 1.3 wiedehopf readsb-install and tar1090
Sources: https://github.com/wiedehopf/adsb-scripts/wiki/Automatic-installation-for-readsb and https://github.com/wiedehopf/tar1090. Check each repo's LICENSE; the readsb lineage is GPL.
- **Install:** `sudo bash -c "$(wget -O - …/readsb-install.sh)"`. Running it again is also the update.
- **What it does to the system:**
  - Installs the readsb decoder.
  - Removes competing decoders (dump1090-fa or -mutability), because only one program can own the SDR.
  - Points fr24feed and rbfeeder at readsb.
  - Installs tar1090 at `/tar1090`.
  - A reboot is needed the first time so the DVB kernel driver is blacklisted.
  - The RTL-SDR v4 needs a newer driver.
- **Helper commands:** `readsb-set-location <lat> <lon>` and `readsb-gain auto|<dB>`. Settings live in `/etc/default/readsb`.
- **Network-only mode:** `--net-only --net-connector <ip>,30005,beast_in`. The docs warn against connecting to 127.0.0.1:30005, which makes a loop.
- **tar1090 instances:** listed one per line in `/etc/default/tar1090_instances` as source directory plus name. Each instance gets its own config, html folder, service and URL path. There is a per-instance uninstall.

### 1.4 readsb aggregator features (these are the compatibility target)
Source: https://github.com/wiedehopf/readsb (README and net_io.c, read for the protocol only)
- **Connector types:** `beast_out`, `beast_reduce_out`, `beast_reduce_plus_out`, `sbs_out`, `json_out`, `raw_out`, `vrs_out`, and the matching `*_in` types.
- **What "reduce" does:** removes duplicates within `--net-beast-reduce-interval` (default 0.25 s; data that does not affect position is sent at 4x the interval).
- **What "plus" adds:**
  - When the TCP connection opens, the feeder sends a Beast escape message of **type 0xE4 followed by a fixed 36-byte ASCII UUID**. A shorter UUID is padded with 'f', and the minimum is about 28 characters.
  - Then `0x1A 'W' 'O'`, which turns on the ping and high-resolution timing options.
- **Per-message receiver ID:** with `--net-receiver-id`, a message of **type 0xE3 with an 8-byte big-endian receiver ID** is sent whenever the receiver changes. Normal Beast frames follow: type '1', '2' or '3', a 6-byte timestamp, 1 byte signal, then the payload. 0x1A is escaped by doubling it.
- **Heartbeat:** an all-zero Mode A/C frame.
- **Latency check:**
  - `0x1A 'P'` carries a 24-bit millisecond counter, about every 5 s.
  - Round-trip time is kept as a histogram and a smoothed average.
  - If the delay is high, `0x1A 'W' 'S'` tells the sender to slow down.
  - An ingest server can drop data that arrives too late, or disconnect the client.
  - I only skimmed which side sends the ping; read net_io.c again before coding it.
- **Server side:** aggregators run readsb with `--net-receiver-id --net-ingest`, plus `--write-receiver-id-json` (receivers.json), `--net-api-port` and `--write-json-globe-index`. `--net-garbage` sends out messages from receivers that timed out. `--forward-mlat` passes MLAT results on.

### 1.5 mlat-client and mlat-server
Sources: https://github.com/wiedehopf/mlat-client (GPL-3.0-or-later) and https://github.com/wiedehopf/mlat-server (**AGPL-3.0**: anyone who can reach a modified server over the network is entitled to its source). AirView must not reuse their code. Implementing the wire protocol from scratch for compatibility is fine.
- **Inputs:** any Beast source with a 12 MHz clock (readsb, dump1090-fa, Mode-S Beast, airspy_adsb) and Radarcape in 12 MHz or GPS mode. FR24's Radarcape timestamps are deliberately degraded and are not supported.
- **Handshake:**
  - TCP, one JSON line padded with spaces.
  - The client sends: version 3; client_version; a list of compression methods (`none`, `zlib`, `zlib2`); selective_traffic; heartbeat; return_results; udp_transport (2 or false); a request for ECEF results; a UUID. Station fields are added on top of that: user, lat, lon, alt, clock_type/frequency/epoch, privacy.
  - The server replies with: an optional `deny` with reasons, a `motd`, the chosen `compress`, optional `udp_transport` {host, port, key}, `split_sync`, and `reconnect_in`.
- **Client to server messages:**
  - sync (two timestamped frames) or split sync; mlat (one timestamped frame).
  - `seen` / `lost` lists of ICAO addresses; per-ICAO rate reports; input connected / disconnected.
  - `clock_jump`, `clock_reset` (with frequency, epoch, mode).
  - A heartbeat every 120 s. The client reconnects if it hears nothing for 60 s.
- **Server to client messages:**
  - Results: timestamp, ICAO, an ECEF position with covariance and station count (or a legacy lat/lon/alt with GDOP), plus optional velocity, vertical rate, callsign and squawk.
  - `start_sending` / `stop_sending` ICAO lists. This is how selective traffic works: the server tells each station which aircraft to send.
  - Statistics: peers, outlier percentage, bad-sync timeout.
- **zlib framing:** one zlib stream, flushed in chunks of up to about 32 KB. Each chunk has a 2-byte big-endian length, and the 4-byte sync-flush tail is removed before sending. `zlib` compresses only client to server; `zlib2` compresses both directions.
- **UDP framing:** a header with key, a 16-bit sequence number and a 64-bit base timestamp, all big-endian. Then sub-messages:
  - 1 = paired sync (two 32-bit deltas and two 14-byte frames)
  - 2 and 3 = mlat with a short or long frame
  - 4 = split sync
  - 5 = rebase
  - 6 = absolute sync
  - Packets are flushed at about 1400 bytes.
- **Results flow back:** the client writes results to local readsb (beast connect to :30104), so the feeder's own tar1090 shows MLAT aircraft.
- **How the server works:** it models each receiver's clock using DF17 position messages as reference beacons, so feeders do not need synchronised clocks.

### 1.6 sdr-enthusiasts Ultrafeeder (GPL-3.0; the names are trademarks of Ramon F. Kolb)
Source: https://github.com/sdr-enthusiasts/docker-adsb-ultrafeeder
- **One environment string** lists every destination, separated by `;`:
  - `adsb,host,port,beast_reduce_plus_out[,uuid=…]`
  - `mlat,host,port[,return_port][,uuid=…]`
  - `mlathub,host,port,beast_in`
- **Other settings:**
  - Location is mandatory: READSB_LAT, READSB_LON, READSB_ALT (in m or ft).
  - The UUID can be set per connector, or globally with `UUID`. The README suggests `/proc/sys/kernel/random/uuid` to create one.
  - MLAT_USER only allows letters, digits, `-` and `_`.
  - GPSD can supply the location for mobile stations.
  - Prometheus and InfluxDB outputs, tar1090, and graphs1090.
- **Known aggregator endpoints** (useful defaults for an "also feed X" option):
  - airplanes.live, adsb.fi, adsb.lol (`in.adsb.lol`) and planespotters: 30004 for data, 31090 for MLAT
  - ADSBx: feed1:30004 for data, feed.adsbexchange.com:31090 for MLAT
  - Several others are listed on non-standard ports.

### 1.7 ADSB.im feeder image (GPL-3.0)
Source: https://github.com/dirkhh/adsb-feeder-image
- **Platforms:** Pi images and other SBCs, x86 VM images and ISO, and an install script for any Debian system (web UI on port 1099).
- **Setup flow in the browser:** go to `adsb-feeder.local` or the `my.adsb.im` redirector, then enter lat, lon, elevation and name. Tick the aggregators that need no account. Accounts for the others are set up on a separate page.
- **Status:** the UI shows each aggregator's status and links to that aggregator's own status page.
- **Warning in its docs:** USB timing inside VMs hurts MLAT.

This is the best UX model for "no command line" users.

### 1.8 FlightAware PiAware (New BSD) and FR24 fr24feed (closed source)
- **PiAware:**
  - Uses an apt repository installed by the `flightaware-apt-repository` package.
  - The feeder ID is stored in `/var/cache/piaware/feeder_id`.
  - Users **claim** a feeder by matching public IP from the same network. They can also claim by feeder ID directly with `/adsb/piaware/claim/<id>`. Two feeders behind one IP is a known problem.
  - Forum threads show apt signing keys and new Debian releases (trixie) repeatedly breaking installs: https://discussions.flightaware.com/t/repository-no-longer-signed/99638
- **FR24:**
  - Install: `wget -qO- https://fr24.com/install.sh | sudo bash -s` (older docs use `repo-feed…/install_fr24_rpi.sh`).
  - The wizard asks for email, sharing key and lat/lon. Settings are in `/etc/fr24feed.ini` (receiver=beast-tcp, host 127.0.0.1:30005, fr24key). One key is allowed per active feed.
  - **FR24 stopped its Windows feeder in May 2019:** https://forum.flightradar24.com/forum/radar-forums/flightradar24-feeding-data-to-flightradar24/12155-windows-feeder-discontinued-7-may-19

### 1.9 Feeding from Windows today
- There is no current mainstream Windows feeder.
- People chain closed or free tools instead: dump1090 Windows builds, ModeSMixer2 (closed-source freeware), Virtual Radar Server rebroadcast, and PlanePlotter.
- Some aggregators accept a "VRS legacy push feed": https://wiki.ads-b.pro/books/how-to-become-a-feeder/page/en-virtual-radar-server-legacy-push-feed/export/html
- I found no ready-made mlat-client .exe. Only a community guide for installing it with Python exists.

Sources: https://www.rtl-sdr.com/tutorial-on-combining-and-rebroadcasting-ads-b-feeds-with-modesmixer2/, https://discussions.flightaware.com/t/does-anyone-use-dump1090-on-windows/17050

A Go single-EXE Windows feeder therefore fills a real gap.

### 1.10 Other references
- 1090MHz UK peering (a UK community network): peers are identified by a UUID sent at session start over Beast Reduce Plus, set up with `--net-beast-reduce-interval=0.5 --uuid-file --net-receiver-id`. https://1090mhz.uk/peering.html
- Radarcape setup and MLAT notes: https://wiki.jetvision.de/wiki/Radarcape:Planeplotter

---

## 2. Things in the AirView code that affect this
- **Port 30154 clash:** the hub data port defaults to **TCP 30154**, but the ADSBx feedclient's mlat-client is set to `--results beast,connect,127.0.0.1:30154`. On a Pi running both, mlat-client would keep connecting to AirView's hub and send Beast frames. The HMAC login would reject it, but the logs fill with junk. Before choosing aggregator ports, move the default hub port or detect a non-protocol client and close it quietly.
- **Hub login:** it uses one shared password with HMAC challenge-response. An aggregator needs **one credential per feeder** that can be revoked.
- **What can be reused:**
  - The Beast, AVR and SBS input clients (`internal/feed`) and the GPS timestamp handling become the input side of the feeder.
  - `internal/sim` is a ready-made Beast source for end-to-end installer tests.
  - The Pi `install.sh` layout (system user, /opt, /var/lib, unit file, `--purge`) is already the right pattern; it needs a `curl | bash` bootstrap in front of it.
  - `internal/ntp` matters on Windows, where the system clock (w32time) is poor.
  - `golang.org/x/sys` is already a dependency and includes `windows/svc` for running as a service.

---

## 3. Recommended design

### 3.1 One Go binary: `airview-feeder` (CGO off; linux arm64/armv7/amd64 and windows amd64/arm64)
- **Inputs (any number):**
  - `beast_in host:port`. Radarcape port 10003 GPS mode is auto-detected by asking the Radarcape over HTTP or by checking the timestamp format.
  - `beast_in` for local readsb / dump1090-fa on 30005.
  - `avr_in`, `sbs_in`, and `rtltcp_in` (see Windows below).
  - The original 48-bit timestamp and the clock type (12 MHz, or GPS seconds-of-day plus nanoseconds) are kept on every frame.
- **Outputs:**
  1. **AirView native uplink** (recommended):
     - TLS 1.3 on one port, e.g. 30160. Go's crypto/tls has no CGO.
     - Each feeder authenticates with a token issued when it is claimed.
     - Messages are length-framed and sent with flate or zstd compression.
     - It carries: station metadata (name, location, privacy flag, clock type, software version); a de-duplicated ADS-B stream (the same idea as reduce_plus); and full-timestamp frames **only for aircraft the server asks for** (like start_sending/stop_sending). Bandwidth stays low but central MLAT still gets what it needs.
     - Heartbeats with round-trip time, plus a back-pressure / slow-down signal.
  2. **Compatibility outputs:** `beast_reduce_plus_out` and an mlat-client-protocol client, so the same binary can also feed adsb.lol, airplanes.live or ADSBx if the user ticks them. This is the Ultrafeeder idea, written in our own code.
- **Offline buffering:**
  - A bounded on-disk ring buffer, e.g. 1 hour or 256 MB, cleared oldest first.
  - When the link comes back, buffered data is replayed with a `late` flag. The aggregator stores it for history and stats but keeps it out of live MLAT and clock sync, because MLAT data is useless after a few seconds.
- **Local status page:** about :8099, so it does not clash with tar1090/lighttpd on :80 or :8504. It shows input state, message rate, uplink round-trip time, the server's MLAT sync status for this station, buffer depth, version, and a "claim this feeder" link.
- **Identity:**
  - A UUIDv4 is created once on first run and stored in `/etc/airview-feeder/uuid` (or `%ProgramData%\AirView Feeder\uuid` on Windows).
  - The installer warns when a cloned SD card is detected, i.e. the same UUID shows up from two IPs, and offers `airview-feeder --new-uuid`.
- **Self-update:**
  - The feeder checks a JSON release manifest daily. The manifest is signed with Ed25519, using the Go standard library.
  - It downloads the new binary, checks its SHA-256 and the signature, swaps the files atomically, restarts, and rolls back if the health check fails within 60 s.
  - This avoids the apt signing-key problems PiAware users hit.
  - Channels: stable or beta. Updates can be turned off.

### 3.2 (a) Raspberry Pi installer
`curl -fsSL https://<airview-host>/feeder/install.sh -o /tmp/avf.sh && sudo bash /tmp/avf.sh`. Downloading first, as ADSBx and adsb.fi do, lets the user read the script. It also works non-interactively with `--server --lat --lon --alt --name --input` flags. Steps:
1. **Pre-checks:** must be root; detect architecture (aarch64 or armv7l); refuse ARMv6 / Pi Zero W unless a build exists; check systemd; check `timedatectl` NTP sync, and offer chrony.
2. **Detect the receiver**, in this order:
   - A process listening on 30005 (readsb, dump1090-fa, piaware): reuse it and change nothing.
   - Radarcape: hostname or `rcd` process locally, or an IP the user enters, using 10003 and radarcape_gps.
   - An RTL-SDR on USB (`lsusb` IDs 0bda:2838 or 0bda:2832) and no decoder running: offer to run wiedehopf's readsb-install as a separate GPL program. Do not bundle its code. Then connect with beast_in to 127.0.0.1:30005.
   - Other SDRs: Airspy 1d50:60a1 and SDRplay 1df7:* are pointed to their own decoders.
   - A Mode-S Beast on FTDI 0403:6001 (serial input).
3. **Ask (whiptail):**
   - AirView server address and a claim code (optional).
   - Station name, limited to letters, digits, `-` and `_`.
   - Lat, lon and altitude with an m or ft suffix. Offer "use AirView Locate-Me / leave blank". Make clear that MLAT needs an accurate antenna position to about 10 m.
   - Privacy: hide the station on the public map, or show a rounded location.
   - Also feed public aggregators: a tick list.
4. **Set up the system:**
   - A system user `airview-feeder`, the binary in `/opt/airview-feeder`, config in `/etc/airview-feeder/config.json` (mode 0640), state in `/var/lib/airview-feeder`.
   - A hardened systemd unit: `DynamicUser` or a dedicated user, `ProtectSystem=strict`, `NoNewPrivileges`, `Restart=always`.
   - A `airview-feeder-update.timer` unit.
5. **Finish:** print the local status URL, the server-side "my station" URL, `journalctl -u airview-feeder`, and the update and uninstall commands. Running the script again reconfigures.
6. **Uninstall:** `sudo /opt/airview-feeder/uninstall.sh [--purge]`, the same as AirView's existing uninstall. It never removes readsb, which other feeders may use.
7. **Later:** a pre-built Pi image with a first-boot web setup (the ADSB.im model). The AirView Pi build could also offer "feeder mode" in its Settings page, since it already contains every input client.

### 3.3 (b) Windows feeder
- **Shape:**
  - The same Go binary, installed as a **Windows service** (`golang.org/x/sys/windows/svc`) so it runs before anyone logs in.
  - A small **tray controller**. `fyne.io/systray` and `getlantern/systray` (both Apache-2.0) are pure Go on Windows; `lxn/walk` (BSD-3) is an option for native dialogs.
  - Settings and status are the same local web page, opened in the default browser, so **no WebView is needed**. If the AirView EXE is installed it can show the page in WebView2 instead.
- **Installer:**
  - Inno Setup (free, own license) or WiX (MS-RL) builds a signed per-machine installer. Authenticode signing matters to avoid SmartScreen warnings.
  - A portable ZIP is also offered.
  - Self-update uses the same signed manifest. Windows allows renaming a running EXE, so the swap is: rename the old file, put the new one in place, restart the service.
- **Network receivers** (Radarcape, Beast, AVR, SBS, or another AirView): fully supported. MLAT accuracy comes from the receiver's own timestamps, so Windows OS timing does not matter. Only the wall clock needs to be right, and AirView's `internal/ntp` can correct it.
- **Local RTL-SDR on Windows:**
  - readsb does not build natively on Windows.
  - Practical route: the user installs the WinUSB driver with Zadig (GPL-3) and we run osmocom **rtl_tcp.exe** (GPL-2+) as a separate child process. We publish its source link and keep it separate, i.e. aggregation, not linking. Our own Go demodulator then reads 2.4 MS/s I/Q.
  - Timestamps come from the sample counter scaled to a 12 MHz clock, the same approach dump1090 uses.
  - Warn that dropped USB samples break MLAT. Detect resets in the sample count where possible and send `clock_reset` to the server.
  - A pure-Go WinUSB driver for RTL2832U/R820T is possible later, but it must be written clean-room because librtlsdr is GPL.
  - Also accept airspy_adsb or dump1090-for-Windows output as `beast_in`.
- **Legacy Windows setups:** accept Virtual Radar Server and ModeSMixer2 rebroadcast as SBS or Beast input. Without timestamps these give positions only, no MLAT.

### 3.4 Letting existing readsb / mlat-client feeders connect without our software
The AirView aggregator should listen on the de-facto standard ports:
- **TCP 30004, Beast ingest:**
  - Accept plain `beast_out`, `beast_reduce_out` and `beast_reduce_plus_out`.
  - Read the 0xE4 UUID at connect and 0xE3 receiver IDs.
  - Handle `0x1A 'P'` pings and `'W'` options the same way readsb does.
  - Ignore any escape type we do not recognise rather than dropping the connection.
  - If no UUID arrives, identify the feeder by IP:port.
  - Community users then only add one line, e.g. `--net-connector adsb.<host>,30004,beast_reduce_plus_out,uuid=…`, or an Ultrafeeder `adsb,…` entry.
- **TCP 31090, mlat-client protocol server:**
  - Our own implementation of the JSON handshake, zlib and zlib2 framing, optional UDP with the sub-message types above, selective traffic (start/stop_sending), and results in ECEF or legacy format.
  - This takes the handshake's lat/lon/alt, user, uuid and privacy as the station's metadata.
  - Plain Beast feeders send no location, which is exactly where the planned Locate-Me estimate is needed.
  - **Licensing:** implement from the protocol description only. mlat-server is AGPL and must not be used as a code base.
- **Other ingest:** SBS on 30003 and a VRS-JSON push input for Windows users. These have no timestamps, so no MLAT.
- **Per-feeder view without accounts:**
  - Like ADSBx `/myip`: a `/me` page matches the browser's public IP to the IPs of active connections and shows only that station's stats.
  - This fails for several feeders behind one IP, behind CGNAT, or when the browser is on IPv6 and the feeder on IPv4. So add a claim flow, as PiAware does: enter the UUID, or a short code shown in the feeder's log or status page, to bind the station to a login.
  - The master admin sees every station.
  - On the public map, a feeder's location is hidden or rounded unless it opts in. The mlat-client `privacy` flag is honoured.
- **Redundancy:** like ADSBx feed1/feed2, publish a second ingest host or port.

### 3.5 Licenses of everything referenced
| Item | License |
|---|---|
| ADSBx feedclient | MIT |
| airplanes.live feed | MIT |
| adsb.fi scripts | LICENSE.md present, terms not checked |
| wiedehopf readsb, tar1090 | GPL lineage; check each LICENSE file |
| mlat-client | GPL-3.0+ |
| mlat-server | AGPL-3.0 |
| Ultrafeeder | GPL-3.0 (plus trademarks) |
| ADSB.im | GPL-3.0 |
| PiAware | New BSD |
| fr24feed | Proprietary |
| ModeSMixer2 | Closed freeware |
| rtl-sdr / rtl_tcp | GPL-2.0+ |
| Zadig | GPL-3.0 |
| fyne/getlantern systray | Apache-2.0 |
| lxn/walk | BSD-3 |
| Inno Setup | Own permissive license |
| WiX | MS-RL |

Running GPL tools as separate processes or calling their installers is fine. Copying their code into AirView is not.

Recommendations:
- Build one pure-Go 'airview-feeder' binary for Pi and Windows. Reuse AirView's internal/feed input clients and keep the original Beast/Radarcape-GPS timestamps and clock type on every frame.
- Give the native uplink to the aggregator TLS 1.3, one token per feeder issued at claim, compression, and server-driven selective traffic (start/stop sending per ICAO) for MLAT frames.
- Add a bounded on-disk ring buffer for offline periods. Replay its data flagged 'late' so it goes into history and stats but never into live MLAT or clock sync.
- Pi install: download the script, then 'sudo bash' it (ADSBx/adsb.fi pattern), with whiptail prompts or non-interactive flags. Auto-detect an existing decoder on 30005, a Radarcape (rcd/hostname, port 10003 radarcape_gps), or an RTL-SDR (lsusb 0bda:2838/2832). Never remove existing decoders or feeders. Optionally hand off to wiedehopf readsb-install rather than bundling GPL code.
- Use a hardened systemd unit, config in /etc/airview-feeder, state in /var/lib/airview-feeder, a local status page on a non-80 port (e.g. 8099), uninstall.sh with --purge, and running the installer again to reconfigure.
- Self-update from a daily, Ed25519-signed release manifest: check SHA-256 and the signature, swap the file atomically, and roll back if the health check fails. Avoid an apt repository because of the signing-key breakage PiAware users hit.
- Windows: the same binary as a Windows service (x/sys/windows/svc), plus a pure-Go tray controller (fyne.io/systray, Apache-2.0) and a browser-based settings and status page (no WebView needed). Ship a signed Inno Setup or WiX installer and a portable ZIP.
- Windows RTL-SDR: run osmocom rtl_tcp.exe as a separate process (WinUSB driver installed with Zadig) and demodulate in Go, with timestamps from the sample counter scaled to 12 MHz. Warn that dropped USB samples break MLAT. Also accept VRS and ModeSMixer2 SBS/Beast rebroadcasts (no MLAT).
- Compatibility for existing feeders: the aggregator listens on TCP 30004 for Beast (beast_reduce_plus_out with the 0xE4 UUID at connect, 0xE3 receiver IDs, 0x1A 'P'/'W' messages, unknown escape types ignored) and on TCP 31090 with a clean-room implementation of the mlat-client JSON/zlib/UDP protocol. Never use AGPL mlat-server code.
- Per-feeder privacy: a '/me' page that matches the browser's public IP (like ADSBx myip), plus a claim-by-UUID/code flow for CGNAT and several feeders behind one IP. The admin sees all. Public-map locations are hidden or rounded by default and the mlat-client privacy flag is honoured.
- When a feeder sends no location (plain Beast feeders never do), mark it 'location unknown' and run the improved Locate-Me estimate on it. Keep such stations out of MLAT until a position is confirmed.
- Fix the port clash: the ADSBx feedclient's mlat-client connects to 127.0.0.1:30154 (beast), which is AirView's hub default port. Change the hub default port, or detect and quietly close non-protocol clients.

---

# AirView codebase map for an aggregator mode (track: codebase)

All findings come from reading D:\AirView. Nothing was edited. Paths are under D:\AirView. External references are listed at the end with their licences.

## 0. Executive summary

AirView is built around one station. That assumption is fixed in four places:

1. **The configuration.** There is one `model.Receiver` and one antenna list (`internal/model/config.go:15-35, 104-146`).
2. **The tracker.** It holds a single `*geo.Station`, and every aircraft's distance, bearing, elevation and slant range is computed against it (`internal/tracker/tracker.go:154, 326-349`; `position.go:120-127, 184-201`).
3. **The database.** The only dimension is `ant` TEXT, where `''` means combined (`internal/store/schema.go:131-206`).
4. **The HTTP layer.** Every GET endpoint is public. "Admin" only controls mutating endpoints, and loopback is always admin (`internal/server/auth.go:88-98`; `routes.go:28-30`).

Several parts can be reused as they are:

- The binary record codecs in `internal/hub/proto.go` already carry everything MLAT needs: GPS ns-of-day or the 48-bit 12 MHz counter, the signal level, the MLAT flag and a feed index.
- The tracker's de-duplication cache already credits the second and later copies of a transmission to other sources (`ingest.go:44-73, 414-434`).
- `internal/analytics` is parameterised by a store plus a `cfg func() model.Config` (`analytics.go:75`), so one analyzer per station is cheap.
- `internal/latency` already has ECEF conversion, a refractive propagation-delay model, the Radarcape frame-end-to-frame-start correction and GPS ns-of-day to Unix conversion (`timebase.go`). These are the core numerical primitives of an MLAT/TDOA engine.

The **hub protocol has the wrong direction for aggregation**. The server pushes and clients pull, there is one shared password, a client limit of 8 and no client identity (`hub/server.go:31, 265-275, 603-701`; `hub/doc.go:109-111`). An aggregator needs feeders that dial out and push (NAT-friendly), each with its own identity.

## 1. Package-by-package map

### 1.1 internal/hub (data port): protocol, auth, framing

- **Handshake** (`doc.go:19-46`, `server.go:603-701`): the server sends a greeting `AIRVIEW-HUB 1 <hmac-sha256|open> <nonce>`. The client replies `AUTH <cnonce> HMAC(pw, snonce)`, and the server proves itself back with `HMAC(pw, cnonce||snonce)`. Comparisons are constant-time. Failed logins are throttled per IP: 5 free, then 1 s doubling to 60 s, with at most 4096 keys (`server.go:464-518`).
- **Framing.** Records are `uint16 len | uint8 type | payload`, server to client only. The types are:
  - 1 Station: JSON of Receiver, Antennas and Feeds. **This includes the exact lat/lon.**
  - 2 FeedDef: a feed index of at most 255.
  - 3 Frame: flags, kind, feed, channel, recv ns, then optional gpsNs, 48-bit clock and float64 signal, then data.
  - 4 Update: a SBS/JSON-style decoded update.
  - 5 Heartbeat.

  The codecs live in `proto.go:180-423` and do not depend on direction, so they can be reused for an uplink.
- **Fan-out** (`server.go:313-371, 540-559`): each client has a buffer of up to 4 MiB and is dropped when it overflows. There is no acknowledgement and no replay. The client turns host receive times into local time using the minimum heartbeat offset (`client.go:332-343, 412-425`).
- **Limits that matter for an aggregator:** `MaxClients` defaults to 8, there is a single password, and the Station record is published to every client. There is no notion of "who is this client". Clients send nothing after AUTH, and anything they do send is discarded (`server.go:590-598`).
- **Needed:**
  - A protocol v2 uplink in which the feeder connects and sends `HELLO <station-uuid> <version>`.
  - Per-station credentials: an HMAC key per station, or a first-contact claim token that the master approves.
  - The same record types sent client to server.
  - New records: StationPrivate (location, antennas, sent only to the master), ReceiverStatus, MLATResult (server to feeder) and an optional ConfigPush.
  - Optional `compress/flate` and `crypto/tls` (both stdlib).
- **Bandwidth.** A long frame record is about 3+4+8+8+8+14 = 45 B, against about 23 B for Beast. At 1500 frames/s that is roughly 70 KB/s, about 0.5 Mbit/s per feeder, so compression is worth having. Do not use readsb-style "beast_reduce" filtering if the aggregator's own MLAT needs every Mode S reply.
- **Effort:** about 2-3 weeks. That covers the new server role (ingest), the feeder uplink client, the per-station key store and tests. The existing pull port stays for remote-data mode.

### 1.2 internal/feed: Beast parser, timestamps, manager

- **BeastParser** (`beast.go:68-197`): streaming, allocation-free, resynchronises after errors, and handles types 1/2/3/4/5. It **skips the readsb receiver-ID extension 0xE3** (8 bytes, `beast.go:23, 182-184`) and ping 'P'. Readsb and ultrafeeder-style aggregator connectors (`beast_reduce_plus_out` with a UUID, plus `--net-receiver-id` on the server) identify the feeder with exactly this frame. A readsb-compatible ingest port therefore only needs this frame surfaced instead of skipped. That is a small change.
- **Timestamper** (`beast.go:373-488`):
  - A Radarcape status frame is authoritative for GPS versus 12 MHz mode.
  - Otherwise the mode is detected statistically over windows of at least 5 s and 16 frames.
  - The legacy 1..86400 day-second range is corrected.
  - MLAT synthetic frames are recognised by the magic timestamp `0xFF004D4C4154..+10` (`beast.go:27-36`).
  - Output is `RawFrame.GPSNs` (ns since UTC midnight, or -1) or `Clock12MHz`.

  This is the input MLAT needs. Each feeder's timestamp mode must travel with its frames, and it already does through hub flags bits 1 and 2.
- **Type '5' receiver position** (`beast.go:321-342`): Radarcapes report their own GPS position. For an aggregator this gives **exact location for Radarcape feeders without any guessing**.
- **Manager** (`manager.go`):
  - It only **dials out** (`dialLoop`, `manager.go:746-773`). There is no inbound Beast listener anywhere in `internal/`; the only listeners are the hub, the HTTP server and the simulator.
  - Feeds are reconciled from `[]model.Feed`. `config.MaxFeeds` is 32 (`config/validate.go:20`).
  - An aggregator needs a new kind of **listener** feed source that accepts many connections and creates one Session per connection or station, plus a Session-level `StationID`.
- **Session** (`manager.go:432-503`): fills in FeedID, applies MLATOnly, and stamps Recv with `ntp.Now()` when it is missing.
- **Effort:** about 1.5 weeks. That covers the Beast ingest listener with 0xE3 identity and per-connection sessions, the uplink-runner feed type for feeder mode, and moving per-station status out of the 32-feed config list.

### 1.3 internal/tracker: single-station assumptions

The tracker is one mutex around everything (`tracker.go:143-247`). Benchmarks give about 590 ns per frame with all extensions and zero allocations (ARCHITECTURE.md:79). Time is "data time", taken from `RawFrame.Recv` (`tracker.go:512-545`).

**Single-station assumptions:**

- **Receiver geometry.** `t.station` comes from `cfg.Receiver` plus the mean antenna AGL (`tracker.go:326-349`). It is used for the range gate `maxRangeKm` = 800 (`position.go:120-127`), the surface-CPR reference fallback (`position.go:73-82`), per-aircraft `dist/brg/elev/slant` (`position.go:184-201`), flight `MaxDist/MinDist/MaxDistBrg` (`position.go:238-245`), the distance records (`position.go:265-272`), CPA and terrain enrichment (`extend.go:279, 330`) and the snapshot DTO fields `distKm/bearing/elevation/slantKm` (`model/aircraft.go:73-77`).

  In a multi-station world:
  - **"Distance from the station" must be computed per viewing station at snapshot time, not stored per aircraft.**
  - The 800 km range gate must use the nearest station that heard the frame.
  - The CPR surface fallback must use the hearing station.
- **Statistics slots.** `slots []*antStats` has slot 0 for combined and slots 1..16 for antennas (`tracker.go:74, 156, 351-367`). Each aircraft carries fixed arrays `antSeen [maxSlots]int64` and uint32 masks `minMask/minPosMask/hourMask` (`aircraft.go:143`, `stats.go:345-374`).
  - One antStats is roughly 70-100 KB plus a heat map: coverage 180x7 accumulators, signal histogram, elevation histogram, 120-bin distance histogram, 36 sectors (`stats.go:37-81`).
  - The slot index is limited to 32 by the mask width.
  - Per-station statistics therefore need either per-station slot sets, keyed by station rather than a bit index, or one tracker per station (see §2).
- **Range statistics are station-relative.** `creditPosition` → `addRange(dist, brg, elev, slant)` (`position.go:276-304`, `stats.go:158-212`). A combined global slot cannot hold range, coverage or elevation statistics. Only counters such as frames, DF mix and positions make sense globally.
- **De-duplication** (`ingest.go:27-73, 236-251`): a 4096-set by 2-way cache with a 100 ms window in *host receive time*. Copies from a different source are counted as duplicates, and `creditDuplicate` credits that source's antenna slot, including the position (`ingest.go:414-434`). That is the right pattern for crediting each station that heard a position. Problems at aggregator scale:
  - The source bitmask is `1 << ((fs.idx*4 + ch + 1) & 63)` (`ingest.go:246`). Beyond about 16 feeds the bits wrap, so a copy from a colliding station is taken as a new transmission.
  - The 100 ms window is too short for internet feeders. TCP and OS buffering, readsb flush intervals and Wi-Fi jitter often exceed it, so duplicates get decoded twice and message counts are inflated.
  - Needed: a per-entry station list (or a small bitmap keyed by station index) and a window of about 0.5-1 s, ideally driven by receiver timestamps when both copies are GPS-stamped.
- **Feed table.** `maxFeeds` is 64, and beyond that all unknown IDs share the last entry (`tracker.go:73`, `ingest.go:129-144`). The DTO lists `Feeds []string` of feeds that heard the aircraft (`aircraft.go:405-409`, `model/aircraft.go:84-85`). **That list is a privacy leak in a public map:** intersecting coverage areas reveals where a station is.
- **Late frames.** Frames more than 5 s behind data time are clamped (`tracker.go:539-540`), and more than 30 s behind triggers clock-step logic. A feeder with a buffered link degrades instead of failing. This is acceptable.
- **Throughput.** One lock for everything. With 100 feeders at 1-2 k frames/s, de-duplication avoids most decodes, but every frame still takes the lock. Realistic limit: low tens of busy feeders on one core, fewer on a Pi.
- **Other single-station items:** `receiver_status` rows per feed (`ingest.go:100-119`) are fine with station-namespaced feed IDs. Records `recs [numRecs]` are global (`tracker.go:207`). Notices go to all viewers.
- **Effort:** about 3-4 weeks for global de-duplication with station identity, per-station accumulators (or lanes), per-viewer geometry in snapshots, DTO redaction hooks and benchmarks.

### 1.4 internal/store: schema

- One SQLite file, one writer goroutine, batched every ~2 s, with a read pool (`store/doc.go:6-27`). Migrations can be registered by other packages: `predict`, `navdata`, `rfstat`, `met`, `latency`, `alerts` (`schema.go:356-399`).
- **Per-antenna, not per-station.** `stats_minute`, `stats_hour`, `range_hour`, `coverage`, `signal_hist`, `elev_hist` and `heat` all have PK `(…, ant)` with `ant ''` = combined (`schema.go:131-206`).
- **No dimension at all:** `aircraft`, `flights` (station-relative `max_dist_km`, `max_dist_brg`, `min_dist_km`), `positions`, `records`, `events`, `weather_*` (one location), `met_*` ("above the station"), `ntp_samples`, `receiver_status(feed)`.
- **Option S1, namespaced ant key** (for example `ant = '<station>'` and `'<station>/<antenna>'`):
  - No DDL change, and `queryStats` and `RangeHours` already take `ant`.
  - Combined `''` keeps only station-free counters.
  - Cheapest. Weaker isolation: per-station deletion and export become harder.
- **Option S2, one SQLite file per station** (`data/stations/<id>/airview.db`) plus a global DB (aircraft, flights, positions, events, MLAT):
  - The existing Store, migrations, analytics, prune and rollup run unchanged per file.
  - Gives isolation, easy "delete my data" and privacy by construction. Costs N writer goroutines.
  - Fine on a server, heavy on a Pi with an SD card.
- **Option S3, a station column:** SQLite cannot change a PK with ALTER, so each stats table must be rebuilt. This is the most invasive option.
- **Per-station "my flights" in either case:** add a link table, for example `flight_station(flight_id, station, first, last, msgs, positions, max_dist_km, rssi_min/max/avg)`, written from the de-duplication credits.
- **Effort:** S1 about 1 week; S2 about 1.5-2 weeks (a registry of stores plus a global DB); S3 about 3 weeks.

### 1.5 internal/server: auth model and exposure

- **Auth today** (`auth.go:18-98`):
  - Loopback is always admin.
  - Without `http.adminPasswordHash`, everyone is admin.
  - With it, a cookie session from `POST /api/login` (in memory, 30-day TTL, at most 512 sessions, throttled per IP).
  - `X-Forwarded-For` is ignored on purpose (`auth.go:64-72`).

  **Hazard in an aggregator deployment:** a reverse proxy such as nginx or Caddy for TLS on the same host makes every client loopback, and therefore admin. Aggregator mode needs explicit trusted-proxy handling, or must refuse loopback-admin when it is enabled.
- **Route table** (`routes.go:17-48, 98-462`): a single declarative table with `Admin bool`, `Cache` and `Exclusive`. This is the natural place to add `Scope: public | station | master` and a `station` resolver per route. The shared response cache (`Cache: ttl…`) must include the scope and station in its key, or one station's data will be served to another viewer.
- **Every GET is public today.** Endpoints that leak a station's location or identity in aggregate mode:
  - `/api/config`: receiver lat/lon, feed hosts.
  - `/api/status`: `station`, `receiverGps`, feed addresses.
  - `/api/feeds`: hub client IPs (`hub.ClientInfo.Addr`).
  - `/api/aircraft` and SSE: `distKm`, `bearing`, `elevation`, `slantKm`, `cpaKm`, `feeds`, `antennas`. One aircraft's lat/lon together with distance and bearing gives the station's exact position.
  - `/api/stats/records`: detail with lat, lon, distKm and bearing.
  - All `/api/antenna/*`, coverage GeoJSON, heat, `/api/guess`, `/api/astro/*` (receiver location), `/api/weather/*`, `/api/met/*`, `/api/timing/receiver`.
  - `/api/db/export/*`: a CSV of every table, flights included.
  - `/api/replay`, alert geofences.
- **Per-feeder identity "by unique IP"**, as asked: map the HTTP client IP to the station whose uplink comes from the same IP. This is the model behind ADSBexchange's "myip" page. It breaks under CGNAT, shared households, dynamic IPv6 and mobile viewing. Recommendation: use IP matching as the zero-config default, plus a per-station secret "claim link" or cookie issued by the feeder program, so owners can view from anywhere.
- **Effort:** about 3 weeks for scopes and roles, station resolution, redaction of DTO and stats outputs, cache keys, trusted proxy and tests. `docgen_test.go` regenerates API.md from the table, which helps.

### 1.6 Web UI (web/static/js): one-station assumptions

- 110 references to receiver, station or antennas across 26 files. Most read `ctx.config.current.receiver` (`config.js`), for example `pages/map.js:274-282, 893-914, 1414-1477` for receiver overlays, fit and "location not set".
- The page contract (`app.js`, `js/README.md`) passes a `ctx` with `api`, `live`, `units`, `ui`, `charts` and `config`.
- **Needed:**
  - A `ctx.station` object (`{id, name, scope, located, lat?, lon?, antennas}`) plus a station selector in the shell.
  - An `api.js` that appends `station=` to every call.
  - Pages that switch on `scope`. The shared map shows no receiver marker, or a fuzzed one. The map, stats, antenna, RF, timing, guess, weather and forecast pages become per-station. Settings splits into "my station" (owner) and server (master).
- Heavy pages: `pages/antenna.js` (995 lines), `stats.js` (1019), `map.js` (1609), `settings/*`.
- **Effort:** about 4-6 weeks.

### 1.7 internal/latency

- One Analyzer with **one receiver** (`SetReceiver(lat, lon, h)`, `engine/latency.go:86-105`). Per-feed statistics already exist (receiver delay, clock filter, `latency_minute` per feed).
- Per-station work means one analyzer per GPS-stamped station, or keying the receiver geometry by feed and station. The 15-minute window and memory usage are bounded per analyzer.
- **Reusable for MLAT:**
  - `ecef` (`timebase.go:140-149`).
  - `propagationNs`: a bent-ray, refractive path delay, accurate to tens of ns at 300-500 km (`timebase.go:209-250`).
  - `FrameStampOffsetNs` (Radarcape stamps the frame end; subtract 64/120/20.3 µs, `timebase.go:58-81`).
  - `GPSToUnixNs` (`timebase.go:95-112`).
  - The host-clock-versus-GPS minimum filter (`timebase.go:260-330`).
- **Effort:** about 1 week to run per station. MLAT itself is separate (see §3).

### 1.8 Guess coordinates ("Locate Me"): method and why it is about 30 miles

Source: `internal/analytics/guess.go`. Input is **only the all-time heat grid** `heat(cell, ant)` for ant `''` (combined antennas): 0.05° cells (about 5.6 km by 3.5 km at 52°N) with count, mean RSSI and min/max altitude (`coverage.go:105-133`). There is no time, aircraft or per-frame information. If there are more than 2500 cells, the grid is merged n x n with n in {1,2,3,4,6,8,12,16} (`guess.go:155-165`). The `cellKm` value feeds the uncertainty floors.

Four estimators and a fusion:

1. **Centroid:** log(count)-weighted centroid on the unit sphere. Uncertainty is max(15 km, 0.3 × rms radius) (`guess.go:271-299`). Biased towards busy airways and away from blocked sectors.
2. **Signal fit:** minimise the weighted L1 deviation of `rssi + 10·log10(slant²)` from its weighted median, which is the inverse-square law with a free offset. Uses Nelder-Mead from several starts (centroid, 4 offsets, loudest cell). Needs 25 cells with at least 3 RSSI samples (`guess.go:326-362, 402-421`).
3. **Floor fit:** a pinball loss (τ = 0.1) of `min_alt − d²/(2kR)` with k = 4/3. Aircraft first appear along the radio-horizon cone (`guess.go:364-400`).
4. **Symmetry:** in 72 sectors, take the 97th-percentile distance point, Kasa-fit a circle, iterate 4 times. Uncertainty is max(8, …) (`guess.go:445-527`).
5. **Fusion:** inverse-variance weights on the sphere. An outlier is dropped when it is more than 3 combined σ + 20 km away. Reported σ = sqrt(σ² + scatter²/n) (`guess.go:529-591`). Uncertainty radii are heuristic (`profileRadius`, `guess.go:423-443`).

Why the result is only about 50 km (30 miles) good:

- **Weak signal gradient.** The predicted RSSI changes by 8.7/r dB per km. At 150-250 km, moving the receiver 10 km changes the prediction by only 0.35-0.6 dB. That is far below the spread between aircraft, which comes from transponder power classes (several dB, see `/api/radio/transponders`), airframe antenna patterns and multipath. Cell means average many aircraft and altitudes with a crude mid-altitude height (`guess.go:178-183`).
- **A Yagi plus a vertical, combined (`ant=''`).** Coverage and RSSI are strongly asymmetric in azimuth. That biases the centroid and symmetry estimators towards boresight and corrupts the inverse-square fit, which assumes an isotropic antenna.
- **Traffic geography.** Airways and coastlines bias the centroid and the edge circle. Terrain shadows bias the floor fit and the edge.
- **Coarse input.** 0.05° cells, merged up to 16x on a large coverage (cellKm/2 enters the uncertainty floor). All-time heat also mixes ducting episodes.
- **The fusion is dominated by the centroid and symmetry estimators,** whose floors of 15 and 8 km and real biases of tens of km set the result.

Improvements, in order of payoff:

- **(a) Use the receiver's own GPS.** Radarcape type-5 frames are already parsed (`feed/beast.go:321-342`, `engine/latency.go:76-82`). This is exact.
- **(b) Aggregator TDOA self-survey.** For an unlocated station U that shares DF17 position messages with located stations K:
  - Each common message gives an equation `t_U − t_K = (|p_ac − U| − |p_ac − K|)/c + offset_UK(t)`. The aircraft position comes from ADS-B, corrected for the position-age, CPR and altitude uncertainties that `internal/latency` already models.
  - When both stations are GPS-stamped, the offset is about 0 ± 100 ns. Thousands of messages fix U to roughly metres to tens of metres, including height.
  - For 12 MHz clocks, solve for U jointly with a piecewise-linear clock offset and drift.
  - This is the "unknown receiver position" variant of the mlat-server clock-sync problem, and the same machinery the aggregator's own MLAT needs.
- **(c) Single-station, better statistics.** Fit a per-aircraft EIRP offset (a mixed model: `rssi_ij = a_i − 20·log10(slant_ij(U)) + G_ant(az, el)`) using only aircraft under about 80 km, where the gradient is large. Add the measured azimuth pattern from the antenna analysis as a nuisance term. Use per-antenna heat (yagi versus vertical separately) and weight the floor fit by a terrain-aware horizon. This needs a small per-aircraft sample table instead of the heat grid. Realistic gain: a few km, not metres.
- **(d)** When the aggregator falls back to a guess for a feeder without a location, show the guess only to the master and the owner, and never use it for MLAT geometry until (b) has converged.

### 1.9 Configuration model

- One `Config` (`model/config.go`) with `Mode` ∈ {standalone, remote-data, remote-web}. Add `aggregator` (server) and `feeder` (uplink only) modes.
- Add `Aggregator{listenPort, beastIngestPort, stations: registry file?, publicMap bool, locationFuzzKm, retention, mlat {enabled}}` and `Uplink{host, port, stationUUID, key, sendLocation bool, shareMLAT bool}`.
- The station registry should not live in `config.json`. It is mutable data with secrets per station, so it belongs in the DB or its own file.
- `config.MaxAntennas` is 16 and `MaxFeeds` is 32 (`config/validate.go:19-20`).
- The hub password is redacted with `"set"` (`config/secrets.go`). Per-station keys need the same treatment.
- **Effort:** about 1 week.

### 1.10 Engine wiring

- Today's chain:
  - `Engine.Start` builds one of each component: store, tracker, met, ntp, weather, analytics, predict, latency, ext (trajectory, rfstat, terrain, navdata, alerts) (`engine/engine.go:190-261`).
  - The feed manager's sink is `tee{tracker, hub}` (`engine.go:356-375`).
  - The effective configuration derives the mode (`wiring.go:44-129`).
  - `Station()` is one effective position, config else GPS (`engine/latency.go:61-84`).
  - Weather, terrain, astro and forecast all follow `eff.cfg.Receiver` (`wiring.go:311`).
- **Aggregator needs:**
  - A `StationRegistry` (id, owner IP set, key, name, location, private flag, antennas, status).
  - A global tracker plus either per-station lanes inside it, or a per-station `StationRuntime` holding a store or ant namespace, an analytics.Analyzer with its own `cfg func()`, a latency analyzer, rfstat and optionally terrain.
  - Weather, forecast and met per station only on demand (Open-Meteo quota), or switched off in aggregator mode.
  - Lifecycle for stations appearing and disappearing.
  - A feeder mode that runs feeds into a hub uplink without a local tracker or DB, so it suits a small Pi or a Windows tray.
- **Effort:** about 3 weeks.

### 1.11 Other single-station packages

- `rfstat`: one Stats per antenna set. Per station means one instance per station (~1 week).
- `terrain`: one site, about 35-40 MB of tiles per 450 km. Per station, only when requested and with a shared tile cache (~1 week).
- `weather`, `predict`, `met`: one location each. Per station on demand, or off (~1-2 weeks).
- `alerts`: geofences and distance rules use the station. Make them global for the master and per station for owners (~1 week).
- `trajectory`: CPA to the station (`extend.go:330`). Compute per viewer or drop in the public view.
- `sim`: extend it to simulate N stations with GPS-stamped TOAs. This is essential for testing MLAT and de-duplication (~1-2 weeks).

## 2. What must become multi-station (station_id dimension)

| Item | Where today | Multi-station change |
|---|---|---|
| Receiver position, antennas | `model.Config.Receiver/Antennas` | Station registry; config describes this host only |
| Frame identity | `RawFrame.FeedID` | Add `StationID` (or a namespaced FeedID `st/<uuid>/<feed>`) from the hub HELLO or readsb 0xE3 |
| Geometry dist/brg/elev/slant | tracker aircraft fields | Computed per viewer at snapshot time; range gate against the hearing station |
| De-duplication sources | 64-bit mask, 100 ms | Station index list, longer window |
| Stats slots | `antStats` per antenna, `[17]` fixed arrays | Per station (and antenna); combined keeps counters only |
| Stats tables | `ant` column | Namespaced ant (S1) or per-station DB (S2) |
| Flights/positions | global | Global, plus a `flight_station` link |
| Records/events | global | Global plus per station |
| Latency, rfstat, receiver_status | per feed / one receiver | Per station |
| Weather/terrain/forecast/met | one location | Per station on demand |
| HTTP | public GETs, admin = mutate | Scopes public / station / master, redaction, cache key, trusted proxy |
| UI | `config.receiver` | `ctx.station` plus selector |

**Recommended architecture: "lanes".**

- One global tracker does de-duplication, the merged map, flights and MLAT input.
- Each frame copy credits the hearing station's lane: counters, range, coverage, heat and signal accumulators keyed by station, reusing `antStats` unchanged with id `<station>`.
- Storage is S2 (per-station SQLite) on a server-class aggregator, or S1 (namespaced ant) on small Pi deployments.
- Analytics runs one `analytics.New(storeOfStation, cfgOfStation)` per station, so almost no analytics code changes.

The alternative of N full trackers (one per station plus one global) reuses even more code. It costs N decodes per frame and N copies of the aircraft tables. That is acceptable for about 10-30 feeders on a VPS, not on a Pi.

## 3. Own MLAT: where it plugs in

- **Inputs exist:** GPS ns or the 12 MHz counter per frame, the MLAT flag, signal, and frame-start correction (`latency.FrameStampOffsetNs`).
- **Clock sync:** for every DF17 position message heard by two or more stations, compute the expected TDOA from the decoded position and the known station positions, and fit an offset and drift per station pair or per station.
  - GPS-stamped stations need only a sanity offset.
  - 12 MHz stations need a Kalman or piecewise-linear clock model.
  - This is the mlat-server approach, described here in my own words. mlat-server is AGPL-3.0, so do not copy it.
- **Solve:** for Mode S-only aircraft (DF4/5/11/20/21 with no position) heard by four or more synced stations, or three plus barometric altitude, run a Gauss-Newton or Levenberg-Marquardt solve in ECEF with altitude as a soft constraint. Track with the existing `internal/trajectory` IMM filter. `stat.NelderMead` and the linear algebra in `internal/stat` exist, but a dedicated LM solver is better.
- **Outputs:** synthetic positions into the global tracker with `MLAT=true` (`store.SrcMLAT`, already supported), and MLATResult records back to the contributing feeders.
- **Policy:** keep excluding MLAT-flagged frames from other networks (already flagged) to avoid feedback loops.
- **Effort:** 6-10+ weeks for a robust implementation with the simulator-based tests.

## 4. Feeder software

The pure-Go, CGO-free cross-compile already produces Windows amd64, linux arm64 and armv7 (`deploy/build.ps1`, README:260-273). A **feeder mode of the same binary** gives both the requested RPi and Windows feeders:

- A local Beast, AVR or SBS input, or a Radarcape.
- A hub v2 uplink with a station UUID stored in the data directory and a key.
- An optional local mini-UI.

A separate small `cmd/airview-feeder` is also possible. The Pi installer in `deploy/pi` (systemd, unprivileged user) is the template. ADSBexchange's feedclient (MIT) installs two systemd services (feed and mlat), keeps its configuration in `/etc/default/adsbexchange`, has a UUID script and points users to a "myip" status page. That is a good user-experience reference. Readsb (GPL-3.0) defines the de-facto aggregator ingest: `--net-bi-port`, the connector `beast_reduce_plus_out` with a UUID, and `--net-receiver-id` / `--net-ingest` on the server. Supporting that ingest means existing readsb or ultrafeeder feeders can join without installing anything.

## 5. Effort summary (focused developer-weeks, rough)

| Package | Weeks |
|---|---|
| model | 0.5 |
| hub v2 | 2-3 |
| feed | 1.5 |
| tracker | 3-4 |
| store | 1-2 |
| analytics | ~1 (plumbing and redaction) |
| server | 3 |
| engine | 3 |
| config | 1 |
| latency | 1 |
| rfstat/terrain/weather/predict/met/alerts | 4-6 |
| web UI | 4-6 |
| sim multi-station | 1-2 |
| MLAT engine | 6-10+ |
| feeder mode, installers, Windows tray | 2-3 |

**Total: roughly 35-50 weeks.** An aggregator MVP without own MLAT is about 15-20 weeks: hub v2 uplink, Beast 0xE3 ingest, lanes, per-station stats, scopes and redaction, a station selector, GPS or TDOA-free location, and a feeder mode.

## Sources (fetched or searched)

- readsb (wiedehopf), GPL-3.0: aggregator options `--net-bi-port`, `--net-receiver-id`, `--net-ingest`, `--uuid-file`, BeastReduce, `--write-receiver-id-json`. https://github.com/wiedehopf/readsb
- mlat-server (wiedehopf fork of mutability), AGPL-3.0: uses ADS-B DF17 position messages as reference beacons to model receiver clocks. Network users must be offered the source. https://github.com/wiedehopf/mlat-server
- ADSBexchange feedclient, MIT: `feed.sh` installer, `adsbexchange-feed` and `adsbexchange-mlat` services, `/etc/default/adsbexchange`, `create-uuid.sh`, adsbexchange.com/myip. https://github.com/ADSBexchange/feedclient
- Background on TDOA with unknown receivers and clock synchronisation:
  - Figuet et al. 2020, combined multilateration with machine learning (a competition round without known receiver positions or synced clocks): https://srv-clst-301-data66.zhaw.ch/bitstream/11475/21210/2/2020_Figuet-etal_Combined-multilateration-with-machine-learning_Proceedings.pdf
  - TU Delft repository: https://repository.tudelft.nl/record/uuid:94205e7b-a9ab-47ac-a3c6-a71aec2aa516
  - Aireon RefTrack: https://aireon.com/wp-content/uploads/2025/04/RefTrack-Full-paper.pdf
  - MDPI (OpenSky-style per-sensor ns timestamps): https://mdpi.com/2504-3900/59/1/2
- I did not fetch adsb.oarc.uk, FR24, tar1090 or mlat-client in this track. Other research tracks should verify their behaviour and licences.

Recommendations:
- Add a hub protocol v2 uplink: the feeder dials the aggregator and sends HELLO with a station UUID and a per-station HMAC key. Reuse the existing Frame/Update/Station/FeedDef/Heartbeat codecs in internal/hub/proto.go in the client-to-server direction. Add StationPrivate and MLATResult records and optional flate/TLS (both stdlib). Keep v1 pull mode for remote-data.
- Also accept readsb-style Beast pushes: surface the 0xE3 receiver-ID frame that internal/feed/beast.go currently skips (lines 23, 182-184) as the station identity, behind a new inbound listener feed source. Existing readsb/ultrafeeder feeders can then join without new software.
- Add StationID to model.RawFrame/DecodedUpdate, or namespace FeedID as st/<uuid>/<feed>. Rework tracker de-duplication: replace the 64-bit source mask (ingest.go:246), which collides beyond about 16 feeds, with a per-entry station list, and widen the 100 ms window to about 0.5-1 s for internet feeders.
- Use a 'lanes' design: one global tracker for de-duplication, the merged map, flights and MLAT input. Per-station antStats accumulators are credited through the existing creditDuplicate path. Compute geometry (distKm/bearing/elevation/slant/CPA) per viewer at snapshot time and never store it per aircraft. A global combined slot keeps only station-free counters.
- Storage: one SQLite file per station plus a global DB on server-class aggregators, so the existing store/analytics/prune code runs unchanged per station and 'delete my data' is trivial. Use namespaced ant keys as the cheap option on a Pi. Add a flight_station link table for per-station flight history.
- Run one analytics.Analyzer per station (analytics.New takes a store and a cfg func), so analytics needs almost no changes.
- Add Scope (public/station/master) to the server route table, include scope and station in the response cache key, and redact by default in public scope: /api/config, /api/status station and receiverGps, /api/feeds client IPs, aircraft distKm/bearing/elevation/slant/cpa/feeds/antennas, record details, antenna/coverage/heat/guess/astro/weather/met/timing receiver, and db export.
- Fix the reverse-proxy hazard before any aggregator deployment: behind nginx/Caddy on the same host every client is loopback and therefore admin (auth.go:75-98). Add an explicit trusted-proxy option, or disable loopback-admin in aggregator mode.
- Feeder identity: use the matching uplink IP as the zero-config way for an owner to see their station, plus a per-station claim link or cookie issued by the feeder program. Plain IP matching fails under CGNAT, shared NAT and dynamic IPv6.
- Location for unlocated feeders: first use Radarcape type-5 GPS frames (already parsed). Then do an aggregator TDOA self-survey against located stations using common DF17 messages, which can reach metre to tens-of-metre accuracy. Keep the heat-grid guesser only as a coarse fallback, visible to master and owner only. Improve the single-station guesser with per-aircraft EIRP offsets, close-range samples only and per-antenna heat, because the combined yagi+vertical heat grid is a main bias source.
- Build own MLAT on primitives that already exist in internal/latency (ecef, propagationNs, FrameStampOffsetNs, GPSToUnixNs): pairwise clock sync from DF17 beacons, an LM solver in ECEF with a barometric altitude constraint, and IMM tracking through internal/trajectory. Describe the mlat-server approach in your own words only, since it is AGPL-3.0.
- Ship feeders as a 'feeder' mode of the same pure-Go binary: Windows amd64 and Pi arm64/armv7 builds already exist, and deploy/pi/install.sh plus systemd is the installer template. Model the user experience on the ADSBexchange feedclient (MIT): a UUID file, separate feed and mlat services, and a status page.
- Extend internal/sim to simulate N stations with GPS-stamped TOAs and network jitter, so de-duplication, lanes, TDOA self-survey and MLAT can be tested end to end.

---

## Research track: how ADS-B aggregators are built and operated

I made no edits under D:\AirView. I read docs/ARCHITECTURE.md only to map the findings onto the hub (TCP 30154 with HMAC login) and the remote-data and remote-web modes. Everything below describes protocols and algorithms in my own words. No GPL or AGPL code is copied.

### 1. The community stack is the same everywhere
Almost every non-commercial aggregator runs wiedehopf's stack. That includes adsb.fi, adsb.lol, airplanes.live (which absorbed adsb.one), theairtraffic, planespotters and OARC. The stack has these parts:
- **readsb** acts as both the feed client on the station and the ingest/hub on the server. https://github.com/wiedehopf/readsb (GPL; descends from dump1090).
- **tar1090** is the map, with globe mode and history. https://github.com/wiedehopf/tar1090 (GPL).
- **mlat-server** is in Python, Cython and NumPy/SciPy. https://github.com/wiedehopf/mlat-server. It is **AGPL-3.0**: anyone running a modified copy must publish the source to its users, and the server advertises its source URL to clients.
- **mlat-client**, the station side. https://github.com/wiedehopf/mlat-client (GPL).
- **mlat-server-sync-map**, the sync map and sync table. https://github.com/wiedehopf/mlat-server-sync-map (MIT).
- **graphs1090** and **tar1090-db** (aircraft.csv.gz).
- **Feeder install scripts**, which are MIT-licensed bash:
  - adsbexchange: https://github.com/ADSBexchange/feedclient
  - adsb.fi: https://github.com/adsbfi/adsb-fi-scripts
  - airplanes.live: https://github.com/airplanes-live/feed (MIT)
  - OARC fork: https://github.com/mpentler/oarc-adsb-scripts
  - adsb.lol: https://github.com/adsblol/feed (MIT)

### 2. Ingest endpoints (concrete)
| Aggregator | Beast in | MLAT in | Notes |
|---|---|---|---|
| ADSBexchange | feed1.adsbexchange.com:30004 (`beast_reduce_out`) | feed2.adsbexchange.com:64004 | Units `adsbexchange-feed` and `adsbexchange-mlat`; config in /etc/default/adsbexchange; files in /usr/local/share/adsbexchange |
| adsb.fi | feed.adsb.fi:30004 `beast_reduce_plus_out` (connector also names 64004) | feed.adsb.fi:31090 | /etc/default/adsbfi; reduce interval 0.5 s; heartbeat 60 s; ro-size 1280; ro-interval 0.2 |
| adsb.lol | feed.adsb.lol:30004 `beast_reduce_plus_out` (also in.adsb.lol:1337) | feed.adsb.lol:31090 | Results example: `--results beast,listen,32005`, then a `beast_in` connector back into readsb |
| airplanes.live | 78.46.234.18:30004 | :31090 | Verified with `netstat` grep on 30004 and 31090 |
| OARC (adsb.oarc.uk) | 44.31.91.230:30004 | :31090 | Units `oarc-adsb-feed` and `oarc-adsb-mlat`; config in /etc/default/oarc-adsb; "only feed us if you are part of OARC" |
| FlightAware | piaware.flightaware.com:1200 (TLS "adept" control and data) | MLAT goes over **UDP** to FA servers (forum: UDP 4999-9999) | Feeder ID from `piaware-config -show feeder-id`; MLAT results come back over the piaware connection to localhost:30104 |
| Flightradar24 | fr24feed, outbound UDP/TCP 8099 plus 80/443/19788 | MLAT is built into fr24feed | Sharing key FR24KEY; FR24 asks you to set `mlat="no"` if you also share with other networks |
| OpenSky | collector port 10004 | No MLAT returned to feeders | A serial is issued by the collector on first connect and must be kept; Radarcape has a built-in OpenSky feeder |

**Radarcape detail (adsb.fi configure.sh):** if the host is called "radarcape" or `rcd` is running, the mlat-client input becomes `127.0.0.1:10003` with type `radarcape_gps` instead of `dump1090` on 30005. The client then uses the GPS-disciplined absolute timestamps, not a 12 MHz free-running counter. This applies directly to the user's EVO.

**Default mlat-client result outputs (adsb.fi):**
- beast connect to 30104 (into the local readsb)
- basestation listen on 31009
- beast listen on 30157
- beast connect to 30169

### 3. Feeder identification: UUID first, IP as fallback
- **UUID creation (adsb.fi create-uuid.sh, paraphrased).** The script reads /proc/sys/kernel/random/uuid after a short random sleep. It validates the 8-4-4-4-12 hex format, optionally in braces. It stores the result at /usr/local/share/adsbfi/adsbfi-uuid, or at /boot/adsbfi-uuid on the image. A bad file is regenerated. The same UUID goes to readsb (`--uuid-file`) and to mlat-client (`--uuid-file` or `--uuid`).
- **beast_reduce_plus_out wire extensions (readsb net_io.c, described not copied).**
  - **UUID message.** Right after connecting, the client sends one message: escape 0x1A, type **0xE4**, then 36 ASCII bytes. The UUID is padded with 'f' and must be at least 28 characters.
  - **Ping enable.** The client then sends `0x1A 'W' 'O'` to turn on high-resolution ping.
  - **Ping and RTT.** The ingest side sends `0x1A 'P'` plus a 24-bit millisecond counter about every 5 s and computes RTT from the echo. Clients whose RTT exceeds the reduce threshold get `0x1A 'W' 'S'` (slow down). Clients over the disconnect threshold are dropped. This RTT is the "latency ms" shown on feeder status pages.
  - **Receiver ID.** When the hub runs with `--net-receiver-id`, a message `0x1A` **0xE3** plus an 8-byte big-endian 64-bit receiver ID is inserted before frames whenever the source changes. This is how ingest forwards frames to the hub without losing per-feeder attribution. With no UUID, readsb hashes the string "host port port" (fasthash64) into the ID, so the ID is effectively based on IP and port.
  - **Plain beast_reduce_out** never sends a UUID. BeastReduce resends a field only when it has changed or the interval has passed (default about 0.125-0.25 s, adsb.fi uses 0.5 s). Data not tied to aircraft state goes out every 500 ms.
- **mlat-client handshake (paraphrased from jsonclient.py).** The client sends one JSON line with `version`:3, `client_version`, `compress` (none, zlib or zlib2), `selective_traffic`, `heartbeat`, `return_results`, `udp_transport`, `return_result_format`:"ecef", `return_stats`, `uuid`, user, lat/lon/alt, clock_type and privacy. The server replies with `compress`, `udp_transport` [host, port, key], `split_sync`, `reconnect_in`, `deny` and `motd`.
- **mlat-client message types.** Traffic is newline JSON:
  - `mlat` {t, m}
  - `sync` {et, em, ot, om}, or `ssync` when split sync is negotiated
  - `seen` and `lost`
  - `rate_report`
  - `clock_reset` and `clock_jump`
  - `input_connected`
  - `heartbeat`
- **Compression and UDP.** zlib2 uses length-prefixed deflate frames with a 2-byte BE length and the 00 00 FF FF sync marker stripped. The optional UDP path carries mlat and sync data in packets of about 1400 bytes, each with a key, a sequence number and a base timestamp.
- **Server-to-client messages:** `start_sending` and `stop_sending`, which select which ICAO addresses the station should forward (this is how server load is controlled); `result`; and `stats` (sync quality).

### 4. Per-feeder status and "own view" pages
- **ADSBexchange myip** (https://www.adsbexchange.com/myip/). You open it from a PC on the same public IP as the feeder. It shows the beast and mlat connections and the feed UID. The "ADSBx Anywhere" link is a bookmarkable map of only your aircraft. It needs the `adsbexchange-stats` package, which reads local readsb JSON and uploads it keyed by UUID (beta API pattern `/api/feeders/?feed=<uuid>`).
- **adsb.lol is the best-documented design** (BSD-3: https://github.com/adsblol/api, https://github.com/adsblol/infra).
  - Every 5 s the API fetches `clients.json` and `receivers.json` from each readsb ingest pod over HTTP, plus `sync.json` and `clients.json` from each mlat-server (port 150). The results go into Redis with 15-60 s TTLs.
  - `/api/0/me` and `/0/me` match the caller's IP against the beast clients. Per client this returns the truncated UUID, kbps, connected seconds, msg/s, pos/s, positions and RTT ms. Per mlat client it returns user, privacy, peer_count, bad_sync_timeout and outlier_percent. It also raises warnings when ms = -1 or bad_sync_timeout > 0.
  - `my.adsb.lol` redirects (307) to `https://<four-words>.my.adsb.lol`. The slug is SHA3-256 of (the first 18 characters of the UUID + a secret salt), turned into a UUID and passed through humanhash as 4 words. It is deterministic and unguessable, but not tied to the IP.
  - The subdomain serves a tar1090 whose `data/aircraft.json` contains only aircraft that receiver heard. The `recentReceiverIds` field is stripped. Its `receiver.json` gives the station lat/lon rounded to 0.1° (about 11 km).
  - MLAT peer IDs in the public sync data are salted hashes.
  - There is also `/0/h3_latency`, which gives beast latency per H3 res-1 cell.
  - Data flow: user → ingest (scalable readsb) → hub (readsb) → planes (public readsb + tar1090); ingest → mlat-server → hub.
- **OARC** (https://adsb.oarc.uk).
  - Feed check is at /myip/: a JS page looks up the visitor's IP through an API and lists the beast and MLAT feeds for that IP. It flags beast and mlat UUIDs that differ, and a `null` UUID. It says latency only works with `beast_reduce_plus_out`.
  - MLAT sync map is at /map/ and the sync table at /sync/. Station positions are "somewhat hidden" on the map for privacy.
  - Server write-up: https://wiki.oarc.uk/flight:adsb-server-writeup. It runs on one donated Debian VPS with no containers:
    - readsb dev, built as a .deb: `--net-bi-port 30004`, `--net-receiver-id`, `--write-receiver-id-json`, `--write-globe-history /var/globe_history`, `--heatmap 30`, and the API on unix socket /run/readsb/api.sock.
    - tar1090: INTERVAL=8, PTRACKS=24.
    - mlat-server in a venv on 31090. It outputs to readsb with `--basestation-connect localhost:<port>`, and readsb has an `--net-sbs-in-port` listener for it.
    - nginx and graphs1090.
    - The author warns it "will not scale easily", so it suits one country or region.
- **FlightAware and FR24** have stats pages behind an account login, claimed through the feeder ID or sharing key.

### 5. How MLAT results reach feeders, and the policies
- The mlat-server computes a position and sends a `result` message (ECEF plus covariance, nd = station count, optional velocities) **only to the clients that contributed to it**. mlat-client then re-emits it locally as Beast, SBS or ext_basestation through `--results` (usually beast,connect,localhost:30104 into readsb).
- For the aggregate map, the server also publishes every position to a local feed (basestation-connect → readsb sbs-in → hub).
- **FlightAware policy.** Results are licensed for personal use only, with no redistribution. Fewer than 1% are anonymised: those aircraft are sent with a flag and turned into non-ICAO DF18 so the fake hex is not mistaken for a real one. FA quoted about 7,500 ground stations at the time. tar1090 labels these as "anonymized MLAT".
- **FR24** asks you to disable its MLAT if you share with other networks.
- MLAT needs a Mode S transponder and at least 4 receivers. Sync between non-GPS receivers comes from ADS-B DF17 aircraft heard by both, used as reference beacons.

### 6. Backend, APIs and scale
- **Aggregator readsb settings:** `--net-only` (now `--net`), `--net-bi-port`, `--net-ingest` (marks the primary ingest node), `--net-receiver-id`, `--write-json-globe-index`, `--write-globe-history` (one gzip JSON per airframe per day, which means thousands of files a day), `--tar1090-use-api`, `--net-api-port`, and `--write-receiver-id-json` (receivers.json).
- **Public API limits:**
  - adsb.fi opendata: https://opendata.adsb.fi/api/, about 1 req/s, personal and non-commercial use only; feeder IPs get extra access automatically.
  - adsb.lol: "dynamic based on load"; drop-in compatible with the ADSBx RapidAPI.
  - ADS-B One: 1 req/s.
- **Scale:**
  - FR24: over 50,000 receivers (later "over 55,000"), about 7,000 of them its own kits.
  - FlightAware: over 40,000 receivers (30,000 in Oct 2020).
  - ADSBx: no published count.
  - adsb.fi: "hundreds" or more (a dated, approximate source).
  - OARC: one VPS for a regional club.

### 7. What this means for AirView (my analysis)
- **Interoperability.** If an AirView aggregator accepts Beast and beast_reduce_plus_out on 30004 (0xE4 UUID, 0xE3 receiver ID, 'P'/'W' ping) and mlat-client protocol v3 on 31090, existing community feeders work unchanged, including OARC's script fork and Radarcape `radarcape_gps`. A Go reimplementation written from these protocol descriptions avoids the AGPL and GPL obligations. Reusing or shipping GPL mlat-client inside AirView's own Windows feeder would impose GPL obligations.
- **Hub design.** The hub on 30154 is a bidirectional HMAC-authenticated stream. Aggregator ingest is a different trust model: many anonymous feeders identified by UUID with an IP fallback.
- **Per-feeder privacy pattern.**
  - The IP is used only as a lookup key, and never shown to others.
  - Each feeder gets a salted-hash slug view filtered by receiver ID.
  - Public station positions are rounded to 0.1° or less, and public peer IDs are salted hashes.
  - Admins see everything.
  - Add a claim token, because IP matching fails behind CGNAT or when the browser and feeder have different IPs (a failure mode the community status pages warn about).

Sources:
- https://adsb.oarc.uk/
- https://adsb.oarc.uk/myip/
- https://adsb.oarc.uk/sync/
- https://wiki.oarc.uk/flight:adsb
- https://wiki.oarc.uk/flight:adsb-server-writeup
- https://wiki.oarc.uk/_export/raw/flight:adsb-service-list
- https://github.com/mpentler/oarc-adsb-scripts
- https://github.com/ADSBexchange/feedclient
- https://raw.githubusercontent.com/ADSBexchange/feedclient/master/update.sh
- https://github.com/adsbfi/adsb-fi-scripts
- https://raw.githubusercontent.com/adsbfi/adsb-fi-scripts/master/configure.sh
- https://raw.githubusercontent.com/adsbfi/adsb-fi-scripts/master/update.sh
- https://raw.githubusercontent.com/adsbfi/adsb-fi-scripts/master/create-uuid.sh
- https://raw.githubusercontent.com/adsbfi/adsb-fi-scripts/master/scripts/adsbfi-feed.sh
- https://raw.githubusercontent.com/adsbfi/adsb-fi-scripts/master/scripts/adsbfi-mlat.sh
- https://github.com/adsblol/feed
- https://github.com/adsblol/infra
- https://github.com/adsblol/api
- https://raw.githubusercontent.com/adsblol/api/main/src/adsb_api/app.py
- https://raw.githubusercontent.com/adsblol/api/main/src/adsb_api/utils/provider.py
- https://github.com/airplanes-live/feed
- https://github.com/wiedehopf/readsb/blob/dev/README.md
- https://raw.githubusercontent.com/wiedehopf/readsb/dev/net_io.c
- https://github.com/wiedehopf/mlat-server
- https://raw.githubusercontent.com/wiedehopf/mlat-client/master/mlat/client/jsonclient.py
- https://github.com/wiedehopf/mlat-server-sync-map
- https://www.adsbexchange.com/myip/
- https://github.com/TanerH/adsbexchange-stats
- https://discussions.flightaware.com/t/flightaware-mlat-network-announcement/18874
- https://discussions.flightaware.com/t/ip-and-ports-for-flight-aware/37348/11
- https://support.fr24.com/support/solutions/articles/3000115324-firewall
- https://forum.flightradar24.com/forum/radar-forums/flightradar24-feeding-data-to-flightradar24/213550-disable-mlat-if-sharing-with-other-networks
- https://github.com/sdr-enthusiasts/docker-opensky-network
- https://github.com/adsbfi/opendata/blob/main/README.md
- https://www.flightradar24.com/blog/inside-flightradar24/flightradar24-now-has-over-50000-active-data-sharers-around-the-world/
- https://blog.flightaware.com/ads-b-network-news-september-2025
- https://flightaware.com/news/article/FlightAware-Reaches-30000-Receivers/1500

Recommendations:
- Aggregator ingest: open a Beast-in listener on TCP 30004 that understands beast_reduce_plus_out (0x1A 0xE4 36-byte UUID at connect; 0x1A 'W' 'O' to enable ping; 0x1A 'P' 24-bit ms ping with echo for RTT; 'W' 'S' to slow down; 0x1A 0xE3 8-byte receiver-ID prefix). Existing adsb.fi/OARC-style feeder scripts then work with only a hostname change.
- MLAT ingest: write a clean-room Go server that speaks mlat-client protocol v3 on TCP 31090. That covers the JSON handshake, none/zlib/zlib2 framing, mlat/sync/ssync/seen/lost/rate_report/clock_reset messages, start_sending/stop_sending for load control, and per-contributor 'result' messages in ECEF plus covariance. Do not derive it from the AGPL mlat-server code.
- Treat clock_type radarcape_gps (Radarcape port 10003, GPS-absolute timestamps) as a first-class case. GPS-timed stations can be multilaterated directly without beacon-based clock pairing, and should anchor sync for the free-running dongle stations around them.
- Identify feeders by UUID (36-character, 8-4-4-4-12, kept stable on disk and shared by the beast and MLAT links). Use a hash of IP and port only as a fallback ID. Warn on the feeder status page when the beast and MLAT UUIDs differ or the UUID is null, as OARC's myip page does.
- Per-feeder view: copy adsb.lol's design. Look the visitor up by IP (used only as a key), redirect to an unguessable slug (salted SHA-256 of the UUID turned into words), and serve a map filtered to that receiver ID. Also issue a claim token or QR code from the feeder installer so the view works behind CGNAT or when browsing from a different network.
- Privacy: never show other feeders' IPs or exact positions. Round public station positions to 0.1 degrees (or apply a random offset of 5-10 km), salt-hash peer IDs in the public sync table, strip per-aircraft receiver-ID lists from public JSON, and leave full detail on admin-only endpoints.
- Return MLAT results only to the stations that contributed, as the community and FA networks do. Mark them as MLAT so they are never re-forwarded upstream as if they were ADS-B. Document that FlightAware and FR24 MLAT results may not be redistributed, and have the AirView feeder refuse to forward 30104-sourced MLAT frames to the aggregator.
- Feeder installers: model the Pi installer on the MIT-licensed adsb.fi/airplanes.live scripts (whiptail prompts for lat/lon/altitude with unit validation, /etc/default/<app> config file, separate -feed and -mlat systemd units, update.sh vs install.sh, uninstall.sh, a netstat check on 30004/31090). Note the adsb.fi altitude-conversion bug, where positive 'ft' values are not converted, and do not repeat it.
- Server data path: run a separate ingest stage (scalable, per-feeder stats such as kbps, msg/s, pos/s, RTT ms and connected seconds, published every 5 s) feeding a hub engine, as adsb.lol does with ingest, hub and planes. Put per-feeder stats in a short-TTL in-memory table rather than SQLite hot paths. Rate-limit the public API at about 1 req/s per IP, as adsb.fi and adsb.one do.
- Licensing: if AirView itself stays non-GPL, do not bundle readsb, tar1090, mlat-client or mlat-server. If the user wants to offer them optionally, install them as separate upstream packages. The MIT scripts (ADSBx feedclient, airplanes.live feed) and BSD-3 adsb.lol code can be adapted with attribution.

---

## Clock-sync research for AirView aggregator MLAT

Scope: receiver timestamp formats, clock types, jitter, drift, and synchronisation for crowdsourced MLAT, plus how to timestamp at a Windows feeder. I made no edits under D:\AirView. Where I describe GPL/AGPL code below, I describe the behaviour in my own words and copied nothing.

### 0. Rules of thumb
- Radio waves travel about 0.2998 m per ns, so 1 ns of timing error is about 0.3 m of range-difference error and 1 µs is about 300 m. A 12 MHz tick is 83.3 ns, about 25 m. Uniform quantisation alone adds tick/√12 ≈ 24 ns rms.
- mlat-server keeps a clock pair only when its rms prediction error is at most about 4 µs (variance < 16e-12 s²). That is a validity gate, not the accuracy you get. Healthy RTL-SDR pairs are well under 1 µs, Beast pairs about 0.1 µs, and GPS Radarcape pairs a few tens of ns.
- A host clock read at socket time (NTP, OS timestamps) is useless for MLAT. USB, TCP and scheduler jitter is 0.1 to 10 ms, which is 30 to 3000 km. MLAT only works with timestamps taken in the receiver or derived from the sample count. Oliver Jowett (obj), mlat-client's original author, says mlat "doesn't care what you do with your system clock"; what matters is USB sample drops ([FlightAware thread, "Clock unstable"](https://discussions.flightaware.com/t/clock-unstable/20036)).

### 1. Timestamp formats on the wire (Beast binary, port 30005 / Radarcape 10003)
Framing: 0x1A then a type byte. Types:
- '1' (0x31): Mode A/C, 2 bytes
- '2' (0x32): Mode S short, 7 bytes
- '3' (0x33): Mode S long, 14 bytes
- '4' (0x34): Radarcape status, 14 bytes
- '5' (0x35): Radarcape position, 21 bytes, with no timestamp or signal byte

Types 1 to 4 carry a 6-byte big-endian timestamp and a 1-byte signal level before the payload. A 0x1A inside the frame is doubled. Sources: [Beast data formats](https://wiki.jetvision.de/wiki/Mode-S_Beast:Data_Output_Formats), [mlat-client modes_reader.c (GPLv3+)](https://github.com/mutability/mlat-client/blob/master/modes_reader.c).

**a) 12 MHz free-running counter.** Used by the Mode-S Beast, Radarcape in legacy "TS12M" mode, and dump1090, readsb and airspy_adsb.
- 48 bits wide, so it wraps every 2^48 / 12e6 ≈ 271 days. The epoch is arbitrary.
- In SDR decoders it is not a real oscillator count. It is the sample index scaled to 12 MHz. dump1090-fa computes each block's start time as sampleCounter × 12e6 / sample_rate, truncated ([sdr_rtlsdr.c, GPL](https://raw.githubusercontent.com/flightaware/dump1090/master/sdr_rtlsdr.c)). At 2.4 MS/s one sample is 5 ticks (417 ns); at 2 MS/s it is 6 ticks (500 ns).
- obj describes it as "a 12MHz value but the resolution is worse than 12MHz" ([FlightAware](https://discussions.flightaware.com/t/question-about-dump1090-timestamp-frequency/20037)).
- airspy_adsb at 12, 20 or 24 MS/s rescales its ADC sample count to 12 MHz (multiply by 12/20 at 20 MS/s). Its output is still a "12 MHz" Beast stream, so a separate "airspy" clock format is not needed on the wire.
- Forum reports say 20 MS/s often gives "clock unstable" on Raspberry Pis because of lost samples, and that 12 MS/s is the stable choice for MLAT ([airspy-conf wiki](https://github.com/wiedehopf/airspy-conf/wiki/Manual-airspy-configuration-to-work-with-dump1090-fa-and-piaware), [FA Airspy thread](https://discussions.flightaware.com/t/howto-airspy-mini-and-airspy-r2-piaware-dump1090-fa-configuration/44343?page=153)).
- **SDRplay:** sdrplay_dump1090 and beast1090 builds also emit 12 MHz-scaled Beast. Community reports show "clock unstable" and out-of-order timestamps with some SDRplay builds ([SDRplay forum](https://www.sdrplay.com/community/viewtopic.php?p=7335), [docker-sdrplay-beast1090](https://github.com/sdr-enthusiasts/docker-sdrplay-beast1090)). Treat SDRplay as a dump1090-class clock until it is shown to be stable.

**b) Radarcape GPS format ("TSgps", DIP#5 on).**
- 48 bits = (second-of-day << 30) | nanoseconds. The low 30 bits hold 0 to 999,999,999; the high 18 bits hold the second of the UTC day.
- The FPGA clock (64 or 96 MHz) is stretched or compressed to 1e9 counts per second, and the rollover to 0 is aligned to the 1PPS edge. Resolution is therefore about 15 ns (1/64 MHz = 15.625 ns).
- Worked example from Jetvision: 0x244bbb9ac9f0 = 10:19:26.999999984 UTC ([Firmware versions](https://wiki.jetvision.de/wiki/Radarcape:Firmware_Versions)).
- Mode S frames are stamped at the end of the frame, Mode A/C at 20.3 µs. mlat-client subtracts 64,000 ns (short), 120,000 ns (long) and 20,300 ns (A/C) to get the frame start. A published OpenSky analysis saw a systematic "≈120,000 ns Radarcape lag", which is exactly the uncorrected end-of-frame stamp on long frames ([MDPI 2504-3900/59/1/2](https://mdpi.com/2504-3900/59/1/2)).
- **Legacy +1 s quirk:** "Daysec+1" firmware counts seconds 1 to 86400; the "TrueUTC" range is 0 to 86399 ([Software features](https://wiki.jetvision.de/wiki/Radarcape:Software_Features)). Status byte 2 bit 0x80 is the "UTC bugfix" flag. When it is clear, mlat-client subtracts 1 s and wraps 0 to 86399.
- **GPS vs UTC time base:** status byte 2 bit 0x08 (`utc_offset_ok`) says whether the time base is UTC. In GPS time the clock runs 18 s ahead of UTC until the offset arrives from the almanac, which can take up to about 5 minutes after power-on.
- **Status frame '4':** byte 0 holds the settings (0x10 selects GPS timestamps). Byte 1 is a signed int8 NCO-vs-1PPS error in 15 ns ticks, typically about ±3 ticks. Byte 2 holds GPS status bits: 0x01 antenna OK, 0x02 tracking, 0x04 three or more satellites, 0x08 UTC offset OK, 0x10 sync OK, 0x20 timestamp NOT ok, 0x80 UTC bugfix.
- **"Emulated" mode:** with GPS timestamps selected and byte 2 bit 0x20 set, mlat-client switches to RADARCAPE_EMULATED. That is a 1 GHz format with no UTC epoch, so it must be treated as a free-running clock.
- The FR24-branded Radarcape outputs a deliberately crippled timestamp and cannot be used for MLAT ([wiedehopf/mlat-client README](https://github.com/wiedehopf/mlat-client)).

**c) Kinetic SBS-1.** 20 MHz counter, only 24 bits, so it wraps every 2^24 / 20e6 = 0.839 s. It must be unwrapped against host time.

**d) Synthetic timestamps.** Values 0xFF004D4C4154 to 0xFF004D4C415E (ASCII "MLAT") mark MLAT results injected back into a Beast stream. Zero is "no timestamp". Exclude both from sync and solving.

**e) Other mlat-client decoder behaviour:**
- It flags a timestamp jump when consecutive stamps differ by more than 90 s of clock.
- It flags a day rollover when the last stamp was at or after 86,340 s and the new one is at or before 60 s.
- For 12 MHz streams it also applies fixed frame offsets (244 ticks for A/C, 768 ticks = 64 µs for Mode S). Only consistency per message type matters, because pairwise sync cancels any constant offset.

**AirView status (read-only check):**
- `D:\AirView\internal\feed\beast.go` already parses the '4' status bits. It has a Timestamper that auto-detects GPS vs 12 MHz and handles the legacy 1 to 86400 day range.
- `D:\AirView\internal\latency\timebase.go` already applies the end-of-frame offsets and the midnight wrap.
- Gaps for aggregator use:
  - A Radarcape running in emulated mode or with the GPS time base (UTC bit clear) should be routed to a free-running or offset-corrected clock model, not treated as UTC.
  - Feeders must forward the raw 48-bit stamp plus the clock type rather than a converted value.
  - BeastStatus labels the 1PPS tick as 15.625 ns; Jetvision's wiki says "15 ns". This is cosmetic.

### 2. Clock types and parameters used by mlat-server
mlat-client maps input types to clock types ([options.py](https://raw.githubusercontent.com/wiedehopf/mlat-client/master/mlat/client/options.py)):

| Input type | Clock type |
|---|---|
| dump1090 (default) | dump1090 |
| beast | beast |
| radarcape_12mhz | radarcape_12mhz |
| radarcape / radarcape_gps | radarcape_gps |
| sbs | sbs |
| auto, avrmlat | unknown |

Clock parameters ([mutability clocksync.py](https://raw.githubusercontent.com/mutability/mlat-server/master/mlat/server/clocksync.py) and [wiedehopf clocktrack.pyx](https://raw.githubusercontent.com/wiedehopf/mlat-server/master/mlat/clocktrack.pyx), both AGPL-3.0):

| Clock type | Frequency | Max frequency error | Assumed jitter (1σ) |
|---|---|---|---|
| radarcape_gps | 1 GHz, epoch UTC midnight | 1 ppm | 15 ns |
| beast / radarcape_12mhz | 12 MHz | 5 ppm | 83 ns |
| dump1090 / unknown | 12 MHz | 100 ppm | 500 ns |
| sbs | 20 MHz | 100 ppm | 500 ns |

360radar's sync page rejects pairs whose relative frequency differs by more than 200 ppm ([360radar sync](https://mlat.360radar.co.uk/rx1/sync)).

### 3. How pairwise sync works in mlat-server

**Sync events (reference messages).**
- A sync event is a DF17 airborne-position even/odd pair from one aircraft, with valid CRC.
- Both messages must have NUCp ≥ 6, arrive within 5 s of each other on the receiver clock, differ in altitude by ≤ 5000 ft, and decode to positions ≤ 10 km apart (MAX_INTERMESSAGE_RANGE).
- The aircraft must be within 500 km of the receiver (wiedehopf).
- The pair's two decoded positions give exact transmitter locations. Each receiver's two arrival times are corrected by its own propagation delay (distance / c_air). The corrected interval measured on clock A is then compared with the same interval on clock B, which gives one offset and drift sample for the pair (A, B).
- Copies of the same message pair are matched when their intervals agree within 1 ms (original) or 0.75 ms (wiedehopf). A sync point lives 2 s.

**Filter in the original mutability server:**
- PI-style filter with KP = 0.05 and KI = 0.01.
- Drift limit is the sum of both clocks' max frequency error; a single update may move drift by at most 1/10 of that.
- Outlier gate is 5σ of the combined jitter. Five consecutive outliers are accepted as a genuine clock step.
- Samples are kept 30 s. A pair is valid with ≥ 2 samples and variance < 16e-12 s², and expires 120 s after its last accepted update.

**wiedehopf fork (used by adsb.fi / adsb.lol-style networks):**
- Outlier threshold 1.5 µs, doubled to 3 µs while a pair has fewer than 4 samples.
- Drift limit is 0.75 × the summed max frequency error; per-update drift changes are limited to 1/20 of that.
- KP = 0.06 and KI = 0.008, with a boosted gain until 12 drift samples have been seen.
- A pair is valid only with at least 5 offset samples and 5 drift samples, variance < 16e-12, an update in the last 35 s, and no cooldown.
- The buffer holds up to 32 samples and pruning keeps 20 or fewer, none older than 45 s. Offset averaging uses a 20 s window.
- Predictions are interpolated piecewise-linearly between stored sync points, not fitted globally.
- Prediction variance is inflated by (1 + stale/60) and for pairs with fewer than 10 samples.
- Per-receiver peer caps are 32 (under 100 km apart) and 16 (further). MAX_SYNC_AC is 15 aircraft per receiver, reselected every 15 s; MAX_SYNC_RATE is 12.
- Before solving, clock pairs are joined into one timebase with a minimum spanning tree over pair variances. MLAT_DELAY is 0.9 s ([config.py](https://raw.githubusercontent.com/wiedehopf/mlat-server/master/mlat/config.py)).

**How many reference messages?**
- Two sync events give a first offset and drift estimate.
- In practice validity needs 5 or more samples (wiedehopf) and stays good with a fresh update every few seconds.
- Each DF17 position pair contributes one sample to every receiver pair that heard both messages. One aircraft gives roughly 1 to 2 samples per second; capping at 15 aircraft and 12/s keeps the server's work bounded.
- **Drift precision (my calculation):** for N points spread over window W, slope σ ≈ σ_t·√12 / (W·√N). For σ_t = 300 ns, N = 20 and W = 20 s, that is about 0.012 ppm (12 ns/s). The limit is not the count of messages. It is the dongle's frequency changing within the window.
- **Temperature effect:** a cheap XO's temperature coefficient is about 0.5 to 1 ppm/°C. While a dongle is warming at about 0.01 °C/s, the frequency changes by about 0.01 ppm/s. Over a 10 s half-window that adds a quadratic error of 0.5 × 1e-8 × 10² ≈ 500 ns. This is why mlat-server interpolates locally between dense sync points instead of fitting a long linear model.
- Recommendation: use a 3-state model (offset, frequency, frequency-rate) with process noise, or short piecewise windows.

### 4. Jitter and drift by hardware

| Hardware | Timestamp source | Resolution | Typical jitter (1σ) | Frequency error | Main failure mode |
|---|---|---|---|---|---|
| RTL-SDR + dump1090/readsb | Sample counter, 2.0 or 2.4 MS/s | 417 to 500 ns | about 300 to 500 ns after demodulation | XO about 30 to 100 ppm; TCXO dongles (RTL-SDR Blog V3/V4) about 1 ppm initial and 0.5 to 1 ppm over temperature ([rtl-sdr.com](https://www.rtl-sdr.com/buy-rtl-sdr-dvb/)) | Silent sample loss in USB, libusb or the kernel |
| Airspy (airspy_adsb, 12/20/24 MS/s) | ADC sample counter scaled to 12 MHz | 83 ns output, finer internally | roughly 100 to 200 ns | XO/TCXO ppm-class | Lost samples at 20/24 MS/s on Pi, CPU throttling |
| Mode-S Beast / Radarcape 12 MHz mode | FPGA counter | 83 ns | 83 ns modelled | ≤ 5 ppm modelled | — |
| Radarcape GPS mode | FPGA NCO disciplined to 1PPS | about 15 ns | 15 ns modelled; NCO vs 1PPS error typically ±3 ticks (±45 ns) | 1 ppm modelled, effectively 0 long-term | Unsynchronised at boot, GPS time base vs UTC |
| SBS-1 | 20 MHz, 24 bits | 50 ns | 500 ns modelled | 100 ppm | 0.84 s wrap |

For the RTL-SDR row:
- About 125 to 150 m of range per sample period.
- An SDR has no hardware timestamp, so latency only matters if samples go missing. dump1090-fa keeps counting the samples it knows it dropped, so its timestamp stays continuous.
- Loss inside libusb or the kernel is invisible and causes a step. obj's example is a 104-byte loss (about 22 µs) desynchronising every peer and inhibiting the receiver for about 2 minutes.
- A "clock unstable" status means more than 25% of the peers synced in the last 30 s were unstable.
- Most dongles are good enough (forum estimate: over 95%).

Mixed RTL-SDR and Radarcape networks: OpenSky literature contrasts about 2.4 MS/s RTL-SDR with about 60 MHz-class Radarcape timestamping, and found only about 15% of sensors time-synchronised ([arXiv 2108.00922](https://arxiv.org/pdf/2108.00922v1.pdf), [LocaRDS arXiv 2012.00116](https://arxiv.org/abs/2012.00116)). In the AIcrowd/OpenSky localisation dataset, about 45 stations were synchronised and more than 200 were drifting.

GPS timing references:
- A u-blox ZED-F9T time pulse is 5 ns 1σ (2.5 ns differential) ([ZED-F9T datasheet](https://cdn.sparkfun.com/assets/2/6/5/f/e/ZED-F9T-00B_DataSheet.pdf)).
- M8T-class modules are about 20 ns. I could not confirm this figure from the [NEO/LEA-M8T datasheet](https://www.mouser.com/datasheet/2/1025/NEO_LEA_M8T_FW3_DataSheet_UBX_15025193-3180171.pdf); see also the [N8UR measurements](https://hamsci.org/sites/default/files/publications/2020_TAPR_DCC/N8UR_GPS_Evaluation_August2020.pdf).
- Jetvision says the Radarcape's timing GPS is steadier than the GNSS in its Air!Squitter or with a u-blox M8W.

### 5. Why GPS-disciplined receivers do not need pairwise sync, and what still does
- Two receivers in GPS mode both stamp UTC directly. The time difference of arrival is just the difference of the stamps, with no pairing, no drift estimate, and no need for ADS-B reference aircraft. MLAT works even when no common ADS-B aircraft is in view.
- Residual errors remain:
  - 1PPS accuracy: about 15 to 50 ns.
  - A constant per-site bias from coax and RF-chain delay. At a velocity factor of 0.66 to 0.85, coax adds about 4 to 5 ns per metre, so a 25 m feeder run is about 120 ns, about 36 m. The GPS antenna lead is a separate delay.
  - The end-of-frame offset (64 or 120 µs).
- Calibrate the per-site bias once and slowly from ADS-B reference transmissions: a robust median of (measured − predicted) over hours.
- Watch for sites whose status says GPS time base, not synchronised, or timestamp bad. Treat those as free-running.
- In a mixed network, GPS sites should anchor the common timebase. Model each free-running clock against "network UTC" with a strong prior on the GPS sites: offset 0 ± 50 ns, frequency 0 ± 1e-9. mlat-server treats the GPS clock as only 1 ppm better than a Beast and does not exploit its absolute UTC; anchoring on it is an improvement.

### 6. Timestamping at a Windows (or Pi) feeder: design guidance
1. **Never use host time as the MLAT timestamp.** The feeder forwards the raw 48-bit receiver stamp, the clock type (12 MHz, GPS 1 GHz UTC, emulated 1 GHz, 20 MHz/24-bit), the frame-type offset convention, and the status-frame bits.
2. **Also send a host-UTC receive time,** for coarse alignment only: grouping copies of the same message across feeders to within ms, latency stats and the midnight wrap.
   - On Windows, Go's time.Now follows the 0.5 to 15.6 ms timer tick ([golang/go#67066](https://github.com/golang/go/issues/67066)). Use GetSystemTimePreciseAsFileTime (100 ns units, Windows 8+), as `D:\AirView\internal\ntp\clock_windows.go` already does, without cgo, via syscall.NewLazyDLL.
   - W32Time is officially within 1 s by default and reaches 1 ms only under strict high-accuracy configuration ([Microsoft support boundary](https://learn.microsoft.com/windows-server/networking/windows-time-service/support-boundary), [high-accuracy config](https://learn.microsoft.com/en-us/windows-server/networking/windows-time-service/configuring-systems-for-high-accuracy)). The server should therefore estimate each feeder's host-to-UTC offset itself, either with its own NTP-style exchange on the feeder link or from GPS-site message pairs.
   - Every few seconds the feeder should send pairs of (receiver counter, host UTC) so the server can keep a coarse ms-level map from counter to UTC.
3. **Choosing a source on Windows:**
   - (a) An existing networked receiver (Radarcape, Beast, airspy_adsb, a dump1090 Windows build) over TCP Beast. Network latency is irrelevant because the stamps are hardware or sample based.
   - (b) A built-in RTL-SDR demodulator. Pure Go with CGO off can call rtlsdr.dll on Windows through syscall, using windows.NewCallback for async reads. Timestamp = sample index × 12e6 / fs. Keep a drop counter and declare a clock reset (new epoch ID) on any detected gap so the server re-syncs instead of throwing outliers.
   - Avoid rtl_tcp for MLAT. It drops buffers silently when the client is slow, which produces invisible timestamp steps.
   - Use 2.4 MS/s on RTL-SDR, and 12 MS/s for Airspy on weak CPUs.
4. **Send clock resets explicitly** (process restart, sample drop, Radarcape mode change) so the server discards the pairings for that clock.
5. **Pi feeder:** readsb or dump1090-fa with Beast output on 30005 works as-is. The feeder adds the clock-type metadata and per-feeder authentication.

### 7. Licenses of referenced projects
- mlat-client: GPL-3.0-or-later (Oliver Jowett 2015; wiedehopf fork 2019–2025)
- mlat-server: AGPL-3.0. Network use triggers source disclosure, so do not embed it; reimplement from these behavioural descriptions.
- dump1090-fa and readsb: GPL. I did not check the exact version (likely GPL-2.0+ for dump1090-fa, GPL-3.0 for readsb).
- airspy_adsb: closed binary.
- Jetvision wiki: documentation only.
- Academic papers are cited, not copied.

Recommendations:
- Define a feeder wire message that carries the raw 48-bit timestamp, an explicit clock type (12MHz-free, 20MHz-24bit, 1GHz-UTC-GPS, 1GHz-emulated), a clock epoch/reset ID, the Radarcape status bits and a precise host-UTC receive time (GetSystemTimePreciseAsFileTime on Windows); never let the server infer MLAT time from host time.
- Server clock model: give each receiver a state against a single network timebase (offset, frequency, frequency-rate) with a Kalman filter or short piecewise windows, rather than only pairwise PI filters. Use GPS Radarcapes as strong anchors (offset 0±50 ns, freq 0±1e-9) and solve the rest by weighted least squares over the receiver graph.
- Start from mlat-server's measured priors for jitter and max frequency error: dump1090 500 ns/100 ppm, beast 83 ns/5 ppm, radarcape_gps 15 ns/1 ppm, sbs 500 ns/100 ppm. Learn per-receiver jitter online from residuals and gate outliers at about 5σ, with a step-detection rule like 'N consecutive outliers means a clock step: reset'.
- Sync events: use DF17 even/odd airborne-position pairs within 5 s, NUCp ≥ 6 (or NACp/NIC equivalents for DO-260B), even and odd positions ≤ 10 km apart and receiver range ≤ 500 km. Require ≥ 5 samples before a pair or clock is valid, cap at about 15 aircraft per receiver, and refresh every 15 s. Consider adding single-message sync against already-synced GPS anchors, which needs only one position message.
- For GPS sites, estimate a slow per-site constant bias (coax and RF chain, about 4–5 ns per metre of cable) from ADS-B residuals over hours. Handle the GPS-vs-UTC time base (18 s) and the 'timestamp bad/emulated' status. Keep AirView's existing legacy-day and end-of-frame (64/120/20.3 µs) corrections.
- Windows feeder: accept Beast over TCP from existing receivers first. If a built-in RTL-SDR path is added, call rtlsdr.dll via pure-Go syscall (no CGO), timestamp by sample count at 2.4 MS/s, count drops and emit a clock-reset on any gap. Avoid rtl_tcp for MLAT because it drops buffers silently.
- Expose per-feeder sync health in the feeder's private stats view, like the 360radar sync matrix: sync count per 30 s, rms error in µs and relative ppm per peer, with the peers' identities and locations anonymised for non-admin viewers.
- Do not copy or link AGPL mlat-server or GPL mlat-client code; reimplement from the behaviour described in the report.

---


# MLAT algorithms: how mlat-server works, the state of the art, and a design for a better Go engine

**Scope:** this is a research note for the AirView aggregator and MLAT proposal. I edited no AirView files. No GPL or AGPL code is reproduced here. Every algorithm is described in my own words, with sources cited.

---

## 1. How mlat-server works, end to end

**Repositories**
- Original: https://github.com/mutability/mlat-server (Oliver Jowett, 2015). Licence: **AGPL-3.0-or-later**.
- Maintained fork: https://github.com/wiedehopf/mlat-server. Also **AGPL-3.0**.
  - The config requires `AGPL_SERVER_CODE_URL`, and the server sends that link to every client.
  - Because of the AGPL, any derivative network service must publish its source.
- Python with Cython. Dependencies: numpy, scipy, pykalman (BSD) and python-graph.
- The README says it is "poorly documented" and gives **no accuracy figures**.

### 1.1 Feeder to server protocol (mlat-client, wiedehopf `jsonclient.py`)

**Transport**
- TCP carrying newline-delimited JSON. Optional zlib/"zlib2" framing: a 2-byte length followed by a deflate stream.
- Optional UDP side channel for timing records. Each packet carries a 32-bit client key and a sequence number, then binary records:
  - sync pairs, short and long Mode S frames, timestamp rebase, absolute sync.

**Handshake fields:** version 2 or 3, user, uuid, lat, lon, alt (−1000…10000 m), `clock_type` (default dump1090), privacy flag, `return_results`, result format ("old" or "ecef"), `return_stats`, compression list, `udp_transport`.

**Client to server messages**
- `sync`: two timestamps plus the raw even and odd DF17 frames.
- `mlat`: one timestamp plus one raw frame.
- `seen` / `lost`: ICAO sets the receiver has started or stopped tracking.
- `rate_report`.
- `clock_reset`, `clock_jump`, `input_connected` / `input_disconnected`.
- `heartbeat`: the server sends one every 30 s and drops a client after 150 s of silence.

**Traffic selection:** the server sends `start_sending` / `stop_sending` ICAO lists. The client forwards only the frames the server needs, which keeps bandwidth low.

**Results:** sent only to feeders that asked for them.
- The "old" format carries time, ICAO, lat/lon/alt, callsign, squawk and receiver count.
- The "ecef" format adds the ECEF position, an upper-triangular covariance, and optional east/north velocity and vertical rate.
- Results contain **no receiver identities**.
- Feeders inject results into readsb as Beast frames with a magic timestamp. AirView already recognises this with `IsMLATTimestamp` in `D:\AirView\internal\feed\beast.go`.

### 1.2 Clock types (`clocksync`)

| Clock type | Frequency | Max frequency error | Jitter |
|---|---|---|---|
| `radarcape_gps` | 1 GHz, epoch = GPS midnight | 1 ppm | 15 ns |
| `beast` | 12 MHz | 5 ppm | 83 ns |
| `sbs` | 20 MHz | 100 ppm | 500 ns |
| `dump1090` | 12 MHz | 100 ppm | 500 ns |

### 1.3 Sync points (`clocktrack`)

**Which messages qualify**
- A receiver reports an **even and odd DF17 airborne position pair** with both arrival times. The two must be ≤5 s apart.
- Both must have a good CRC and come from the same ICAO.
- NUCp ≥ 6, valid altitudes, altitude difference ≤ 5000 ft.
- A CPR global decode must succeed.
- Each position must be within 500 km of the receiver. Even and odd positions must be ≤ 10 km apart.

**Grouping across receivers**
- The key is the byte content of the A and B frames.
- Another receiver joins the same sync point if its A→B interval matches within 1 ms (0.75 ms in the wiedehopf fork).

**Pair update**
- For each receiver pair, propagation delay is removed from both arrivals (distance ÷ c_air). This gives transmitter-side times and intervals for both receivers.
- The pairing is then updated with (base time, peer time, base interval, peer interval).

**Peer limits in the wiedehopf fork**
- Peers are bucketed by distance in 50 km bands, with a cap of 32 or 16 peers per receiver.
- At most 15 sync aircraft per receiver (`MAX_SYNC_AC`), re-chosen every 15 s.

### 1.4 Clock pairing model (per receiver pair)

- **Drift** is the fractional frequency difference. It comes from comparing the two receivers' corrected A→B intervals after scaling by the nominal frequency ratio.
- **Drift gating:** a sample is rejected if |drift| exceeds the sum of both clocks' maximum frequency errors, or if it jumps more than one tenth of that.
- **Drift smoothing:** a PI-like loop. Gains are Kp 0.05 and Ki 0.01 in mutability, 0.06 and 0.008 in wiedehopf. The integral is clamped to ±50 µs.
- **Offset:** comes from a short history of (base, peer) timestamp points.
  - mutability keeps 30 s. wiedehopf uses a 32-entry ring pruned to about 20 points or 45 s.
  - Prediction interpolates linearly between stored points, or extrapolates from the nearest point with the drift.
- **Outliers**
  - mutability: an error must exceed 5σ of the combined jitter *and* 5× the current standard error.
  - wiedehopf: a fixed 1.5 µs threshold, doubled for new pairs, plus a penalty counter.
  - A run of 5 outliers forces acceptance, which counts as a step.
- **Validity:** at least 2 points, variance < 16·10⁻¹² s² (about 4 µs RMS), and updated within the last 30–35 s.
- A receiver whose clock goes backwards has its history reset.

### 1.5 Clock normalisation (`clocknorm`)

- Builds a graph with receivers as nodes and valid pairings as edges, weighted by variance.
- Receivers that share an absolute epoch (for example two Radarcape GPS units) get a direct edge.
- Computes a **minimum spanning tree per connected component**.
- Picks the **tree centre**, the midpoint of the longest weighted path, to minimise the worst-case hop count.
- Converts every timestamp into the centre receiver's timebase. Variance is summed along the path, so error grows with each hop.

### 1.6 Message grouping and clustering (`mlattrack`)

- Frames are grouped by raw content for `MLAT_DELAY`: 2.5 s in mutability, **0.9 s** in wiedehopf. At most 15 copies are kept.
- After normalisation, timestamps are sorted. The sorted list is split at gaps over 2 ms.
- A cluster grows backwards from the newest timestamp. A receiver is admitted only if, for every member already in the cluster, |Δt| ≤ (1.05·distance + 1 km)/c.
- Receivers within 1 km of each other count once.
- Minimum receivers: **4 distinct**, or **3 if an altitude is known**.

### 1.7 Solver (`solver.py`)

**Formulation**
- Unknowns: ECEF x, y, z and an offset (the transmission time expressed as a distance).
- Residual per receiver: (measured pseudorange − |x − rᵢ| + offset) / σᵢ.
- Optional altitude residual: (geodetic height(x) − reported altitude) / σ_alt.
- `scipy.optimize.leastsq` (MINPACK Levenberg–Marquardt), `maxfev` 50 or 165.
- The initial guess comes from the caller: the last position, or the centre of the receivers.
- Covariance is the top-left 3×3 block of the LM covariance.

**Altitude handling**
- σ_alt = 250 ft if the frame carries altitude, otherwise 250 ft + 70 ft per second of age.
- Pressure altitude is used as if it were geometric height, with no correction.

**Rejection rules**
- Offset < 0 or > 500 km, or the fix more than 500 km from any receiver.
- trace(P) > 10⁸ m², i.e. about 10 km.
- Within 2 s of the last fix, the new variance is more than 1.1× the previous one.

**Rate limits:** the per-aircraft rate-limit and backoff thresholds are listed in section 6.1.

**No GDOP gating** is performed.

### 1.8 Tracking (`kalman.py`)

- pykalman **unscented** Kalman filter in ECEF, constant velocity (6 states) or constant acceleration (9 states).
- The measurement vector is pseudoranges relative to the first receiver, plus altitude. In this one place the filter is tightly coupled to the raw timing.
- Mahalanobis outlier gate at 15.
- Track states: "acquiring" until σ_pos < 3 km and σ_vel < 50 m/s; "lost" when σ_pos > 5 km or σ_vel > 75 m/s.
- The filter is used for velocity and heading output. Positions shown on maps are mostly the per-epoch least-squares fixes.

### 1.9 Achieved accuracy and known weaknesses

**Accuracy**
- No official figures exist.
- Flightradar24 reports typical MLAT accuracy of **10–100 m, worst case about 1 km**, and has used 3 receivers since January 2025: https://www.flightradar24.com/blog/inside-flightradar24/how-we-track-flights-with-mlat/
- Community experience with mlat-server is similar: tens to hundreds of metres, with jumps and "ghost" or "ragged" tracks, especially where aggregators' results disagree (FlightAware forum threads).

**Weaknesses (what we can beat)**
1. **Pairwise, not network-consistent.** Independent PI loops per pair, plus an MST with variance growing per hop, means triangle inconsistencies. The many redundant pairs are thrown away instead of being solved jointly.
2. **No anchor concept.** GPS units are tied together only through shared-epoch edges. There is no absolute time reference, and non-GPS receivers are not anchored to the GPS ones.
3. **Solve-then-filter.** Each epoch needs ≥4, or 3 + altitude. Two-receiver and partial observations are discarded.
4. **Pressure altitude treated as geometric** (σ 76 m). It ignores QNH and temperature, which can bias height by hundreds of feet. There is no geoid handling for receiver altitude: users often enter MSL, and the UK geoid is about +45 to +55 m.
5. **Fixed propagation speed.** c/1.0003 with no altitude-dependent refraction. The 2022 OpenSky competition winner gained by estimating altitude-dependent signal velocity.
6. **No GDOP or χ² integrity test, no leave-one-out fault exclusion.** One bad receiver can bend a fix.
7. **Latency of 0.9–2.5 s** from the grouping delay.
8. **Feeder position errors.** A wrong or deliberately fuzzed position, a common privacy habit, poisons sync. Mitigations came later as bad-sync counters and "karma" (e.g. https://github.com/TanerH/mlat-server).
9. **No use of Comm-B velocity or heading** (BDS 5,0 / 6,0) or DF17 velocity-only messages as track measurements.
10. **No built-in accuracy self-measurement.**

---

## 2. Measurement physics and timing budget

### 2.1 Arrival-time model

For receiver i, transmission at true time T from position x:

tᵢ = T + (|x − rᵢ| + Δᵢ_atm)/c + bᵢ(t) + δᵢ + nᵢ

- bᵢ(t): clock offset, a stochastic process.
- δᵢ: fixed hardware and cable delay (about 5 ns per metre of coax, plus filter group delay).
- nᵢ: timestamp noise.

**Timestamp noise by receiver type**
- **Radarcape GPS:** vendor σ < 50 ns (1σ). The 1PPS tracking error is reported in Beast type-'4' status frames, typically ±3 ticks of about 15 ns (https://wiki.jetvision.de/wiki/Mode-S_Beast:Data_Output_Formats). AirView already parses `PPSOffset` and `GPSLocked()`.
  - In GPS mode the timestamp is nanoseconds since UTC or GPS midnight.
  - Legacy mode is a 12 MHz counter: 83.3 ns tick, quantisation σ = 83.3/√12 ≈ 24 ns.
- **RTL-SDR with readsb/dump1090:** 2.4 MS/s sampling, 417 ns per sample, rescaled to 12 MHz. Effective σ is roughly 50–500 ns, depending on phase interpolation and signal level. mlat-server assumes 500 ns.
- Rule of thumb: 1 ns ≈ 0.3 m of range. 50 ns ≈ 15 m, 500 ns ≈ 150 m of pseudorange noise before geometry dilution.

### 2.2 Atmospheric excess delay (new; cheap, closed form)

Exponential refractivity model:

N(h) = N₀·e^(−h/H), with N₀ ≈ 315 and H ≈ 7 km (ITU-R P.453 standard atmosphere). Better: estimate N₀ from local METAR (AirView has a met module).

For a straight path of length D between heights h₁ and h₂:

excess = 10⁻⁶·D·N₀·H·(e^(−h₁/H) − e^(−h₂/H))/(h₂ − h₁)

When h₁ ≈ h₂, use the limit 10⁻⁶·D·N(h̄).

- A 300 km near-horizontal path at low altitude gives about 60–90 m of excess.
- The differences between receivers are tens of metres. That is far above the GPS timing noise, so this model matters for Radarcape-class accuracy.
- Add a per-network scale factor k_atm as a slowly varying estimated state, following the competition winner's approach.

### 2.3 Altitude

- Mode S altitude (DF0/4/16/17/20) is **pressure altitude referenced to 1013.25 hPa**. Convert it to geometric height above the ellipsoid (HAE).
- **Best approach:** learn a regional "GNSS minus baro" field from ADS-B aircraft that report it. In the DF17 velocity message (TC 19), the GNSS-height-difference field has 25 ft resolution, and TC 20–22 frames carry GNSS height.
  - Fit a smooth field in latitude, longitude and altitude, e.g. a bilinear grid over 50 km cells × altitude bands with a Kalman update per cell.
  - Typical residual is about 50 ft, versus 250 ft assumed by mlat-server.
- **Fallback:** METAR QNH and temperature with the ISA relation. AirView's simulator already models gnssMinusBaro as QNH offset + temperature + geoid in `D:\AirView\internal\sim\physics.go`.
- Receiver heights must be HAE. Apply a geoid model if users enter MSL: EGM96/EGM2008 are NGA, public domain. A 15′ grid is about 2 MB, or bundle a coarse grid for the coverage region.

---

## 3. State of the art beyond mlat-server

### 3.1 Joint network clock estimation instead of pairwise PI loops

- **Sonnleitner & Hobiger (2025).** "Wide-area Multilateration Airspace Surveillance with Unsynchronized Low-Cost ADS-B Receivers Using TDOA Observations", NAVIGATION 72(3), DOI 10.33012/navi.704, **CC BY 4.0**: https://navi.ion.org/content/72/3/navi.704
  - A single EKF holds every aircraft's position and velocity plus the relative clock offsets of all receivers (6·Nₐ + N_g − 1 states). Clocks are random walks; drift is left as future work.
  - Inputs are TDOAs and ADS-B positions used directly as position observations.
  - Results, simulation only: TOA noise 100 ns, 42-station grid. Horizontal WRMSE < 30 m and median 3D error 43 m, falling to **24 m** when ADS-B positions are fused in.
  - Normalised innovation squared (NIS) flagged spoofed messages.
- **Sallouha, Chiumento & Pollin (2021).** "Aerial Vehicles Tracking Using Noncoherent Crowdsourced Wireless Networks", IEEE TVT, arXiv 2108.00922.
  - A few trusted (GPS) sensors anchor a large unsynchronised network. AR and LSTM models correct offsets, followed by MLAT and a Kalman filter.
  - Reported accuracy is "comparable to GPS-synchronized sensors".
- **Kaune et al. (2012), Information Fusion.** Direct TOA processing in WAM instead of first forming TDOAs.
- **Patents (useful ideas only; check before implementing):** US 9377524 (time synchronisation via over-determined measurements), US 8791861 / US 9255804 (ADS-B-derived timing and state).

### 3.2 OpenSky open localisation competition (Strohmeier et al., arXiv 2209.13669)

https://arxiv.org/abs/2209.13669

**Results**
- Best result with **GPS-synchronised receivers: 25 m** TRMSE (90 % truncated, 50 % coverage).
- With **mostly unsynchronised receivers: 78 m** at 70 % coverage.
- OpenSky's classic baseline: 682 m. It used pairwise alpha-beta sync and plain least squares.

**Winner's unsynchronised method (offline, not real-time)**
- Fit station positions, offsets and **two signal-velocity parameters including altitude dependence** with BFGS, using the **L1 norm** because timing noise is heavy-tailed.
- Clock drift modelled as linear plus random walk (cubic spline).
- Synchronise well-behaved stations first, then the rest iteratively.
- Per-message MLAT for any message heard by ≥3 receivers.
- Outlier removal on a track graph (implied speed > 300 m/s).
- Local polynomial and spline smoothing; filling gaps of 60 s or less.

**Lesson:** robust norms, refraction modelling, careful sync and track-level smoothing give multi-× gains.

### 3.3 Initialisation, closed forms and ambiguity

**Linearised TOA (5+ receivers)**
- Squaring |x − rᵢ| = c(tᵢ − T) and subtracting the equation for a reference receiver 0 gives a linear system in (x, T):

  2(rᵢ − r₀)·x − 2c²(tᵢ − t₀)·T = |rᵢ|² − |r₀|² − c²(tᵢ² − t₀²)

  Here tᵢ is the arrival time after removing the receiver's estimated clock offset bᵢ.
- Use times relative to a local epoch (subtract a local epoch) to avoid cancellation.
- Solve by weighted least squares.

**Bancroft (1985, GPS)**
- Uses a Lorentz inner product to give a quadratic in one scalar with exactly 4 receivers. Pick the root with plausible range and altitude.

**Chan & Ho (1994)**
- "A simple and efficient estimator for hyperbolic location", IEEE TSP: a two-stage weighted least squares estimator that is closed-form and approaches the CRLB at moderate noise.

**3 receivers + altitude**
- Two TDOA hyperboloids intersected with the height surface usually give **two solutions**.
- Robust practical method: evaluate the cost on a coarse 2D grid in an ENU plane at the known height, e.g. 5 km cells over ±400 km. That is about 25 k evaluations × N receivers, roughly 1 ms in Go.
- Run LM from the best 2–3 minima. Keep the alternates and let the tracker choose the branch.

**Always seed from the track prediction when one exists.**

### 3.4 Robust estimation and integrity

- Errors are dominated by heavy tails: multipath, garbled frames, wrong clock state, a wrong receiver position.
- **Huber IRLS** (k ≈ 1.345σ) or **Cauchy/Geman-McClure** inside LM; the L1 norm also performs well (competition winner).
- **RANSAC** with minimal subsets of 4, or 3 + altitude, when N ≥ 6. Inliers are those with |residual| < 3σᵢ.
- **GNSS-RAIM-style FDE**
  - Test statistic: weighted SSE ~ χ²(N + n_alt − 4).
  - If it fails at p < 10⁻³, exclude the receiver with the largest normalised (studentised) residual, re-solve, and repeat while DOF ≥ 1.
- Robust M-estimation in multilateration EKFs: 2024 airport-surface paper (Huber-EKF) and the TDOA outlier work from Tel Aviv University (https://cris.tau.ac.il/en/publications/time-difference-localization-in-the-presence-of-outliers/).

### 3.5 CRLB and DOP (use for gating, reported error and siting advice)

Linearised TOA model, unknowns θ = (x, cT):
- Hᵢ = [ −uᵢᵀ , 1 ], where uᵢ = (rᵢ − x)/|rᵢ − x|.
- Altitude row: H_h = [ n̂ᵀ , 0 ], where n̂ is the local ellipsoid normal.
- W = diag(1/σᵢ², 1/σ_h²).

Fisher information F = HᵀWH; CRLB = F⁻¹. Take the 3×3 position block and rotate it to ENU:
- HDOP-equivalent error: sqrt(P_EE + P_NN).
- Gate fixes on sqrt(P_EE + P_NN) < 1–2 km and on the condition number.

**Geometric intuition:** for a target at range R outside a receiver cluster of baseline B, cross-range error ∝ σ·R/B and radial error grows roughly ∝ σ·(R/B)². Long baselines and receivers on both sides of the aircraft matter far more than receiver count.

**Community feature:** a CRLB map ("expected MLAT accuracy at FL100") recomputed when feeders join. It shows where a new feeder would help most.

References: Galati/Leonardi/Tosti CRLB studies for Mode S MLAT (Tor Vergata), and the Puričer PhD thesis, CTU Prague 2019 (TDOA CRLB, arbitrary 3D geometry): https://dspace.cvut.cz/bitstream/handle/10467/85526/

### 3.6 Tracking with raw measurements (tightly coupled)

**Filter**
- An EKF or UKF whose update uses the cluster's TOAs directly, with the transmission time T eliminated by differencing against a reference or by a nuisance state.
- An **IMM** with constant velocity and coordinated turn models. The vertical channel is driven by Mode S altitude.

**Benefits**
- **2-receiver updates:** one TDOA plus altitude constrains a curve, and the dynamics resolve the position along it. Use these only to *maintain* a track initiated by a full fix, never to start one.
- **Velocity observations**
  - DF17 TC 19 velocity, for aircraft that send velocity without position.
  - **Comm-B BDS 5,0** (track angle, ground speed, roll) and **BDS 6,0** (heading, IAS, Mach, vertical rate), which UK radars elicit heavily.
  - AirView already infers BDS registers (`D:\AirView\internal\modes\commb.go`, `infer.go`).
  - Ground speed and track are a direct velocity measurement and remove most MLAT track jitter. Heading + TAS + wind is weaker; AirView has a wind source.
- **Gating:** Mahalanobis χ² with m DOF at 99.9 %. Initiate on M-of-N confirmation (2 consistent fixes in 10 s).
- **Smoothing:** an RTS smoother produces display or replay trails. AirView already has an RTS smoother in `internal/trajectory/smooth.go`.

### 3.7 Mode A/C MLAT

- No address and no parity. Mode A and Mode C replies are indistinguishable without knowing the interrogation mode. Codes such as 7000 or 2000 are shared by many aircraft, and FRUIT and garbling are common.
  - ICAO APANPIRG material: https://www.icao.int/sites/default/files/APAC/Documents/edocs/CNS/grpt_atcmulti_adsbdata.pdf
- Literature: Frisch & Hanebeck (KIT), "Association-Free Multilateration Based on Times of Arrival" (https://isas.iar.kit.edu/pdf/ICRA20_Frisch.pdf); degarbling by signal separation (Univ. Reims, HAL hal-01693283).
- Radarcape can emit Mode A/C with timestamps (Beast type '1'). Jetvision's commercial MLAT claims 3–5 m with Mode S + A/C; that is a vendor claim.
- **Practical approach:** only among GPS-anchored receivers within about 150 km.
  - Associate (code, time) tuples whose pairwise |Δt| ≤ dᵢₖ/c + 2 µs.
  - Solve 4-receiver fixes with no altitude prior.
  - Disambiguate A versus C by track continuity and Gillham validity.
- **Phase 3, low priority.**

### 3.8 Commercial WAM benchmarks

- EUROCAE **ED-142** is the WAM MOPS, with ED-142A in consultation in 2026. Accuracy is defined as RMS horizontal error.
- A field trial in Chengdu measured WAM within **6–7 m** of RTK truth, versus about 20 m for ADS-B and about 200 m for Mode S radar.
  - ICAO SURICG-7 IP14: https://www.icao.int/sites/default/files/APAC/Meetings/2022/2022%20SURICG-7/4-Information%20Papers/IP14_CHN-AI.8a-Introduction-to-the-Test-of-the-WAM-Technology-in-the-Final-Approach-Monitoring-v2.pdf
- This is the ceiling with dedicated, surveyed, GPS-synchronised receivers.
- MITRE WAM white paper: https://www.faa.gov/sites/faa.gov/files/about/office_org/headquarters_offices/ato/WAM_WhitePaperFINAL_MITRE_v2.pdf

---

## 4. Recommended design: a better Go MLAT engine (pure Go, CGO off)

Proposed packages under `internal/mlat`. These are new packages; nothing in AirView was edited.

### 4.1 `geo`: geodesy

- WGS84 geodetic↔ECEF conversion. Use a closed-form (Heikkinen/Zhu) ECEF→geodetic, not an iterative one.
- ENU rotation and geoid lookup (EGM96 grid, public domain).
- Refraction excess-delay function (§2.2).
- Fixed-size `[3]float64` / `[4][4]float64` maths, zero allocations.
- gonum (BSD-3, pure Go) only for the larger clock-network matrices.

### 4.2 `clock`: network clock estimator

This replaces pairwise PI loops and the MST.

**State per receiver i:** offset θᵢ [s], fractional frequency φᵢ [s/s], and optionally a fixed delay δᵢ.

**Process model:** the standard two-state clock model:

Q = [[q₁dt + q₂dt³/3, q₂dt²/2], [q₂dt²/2, q₂dt]]

| Receiver class | q₁, q₂ | Anchor role |
|---|---|---|
| Radarcape GPS-locked | tiny | **anchor**: θ prior 0 ± 50 ns, φ = 0 |
| Beast / TCXO | medium | not an anchor |
| RTL-SDR | large | not an anchor |

- q₁ and q₂ are derived from the clock type, then **adapted online** from innovation statistics.
- The anchor bias δᵢ is a random walk with σ about 1 ns/√h, which absorbs cable and filter delay.
- A receiver loses anchor status when Beast '4' frames report no GPS lock or `TimestampBad`.

**Sync observations**
- Any message from an ADS-B aircraft with a good position: NACp ≥ 8 (EPU < 93 m), preferably ≥ 9, and NIC ≥ 7.
- The message must be heard by receivers i and k:

  zᵢₖ = (tᵢ − tₖ) − (ρᵢ − ρₖ)/c = (θᵢ − θₖ) + (δᵢ − δₖ) + ε

  where ρ = geometric range + refraction excess.
- R = σᵢ² + σₖ² + (uᵢ − uₖ)ᵀ Σ_pos (uᵢ − uₖ)/c², where Σ_pos comes from NACp.
- **Position timing:** the reported position is latched up to roughly 0.1–0.2 s before transmission. Interpolate the aircraft's CPR track to the transmission instant using DF17 velocity. Treat the residual as a per-aircraft bias, mitigated by averaging over many aircraft with varied geometry.
- **Use every frame**, not just even/odd pairs: DF17 velocity and identity frames, and DF11, by interpolating the ADS-B track. This gives about 10–50× more sync observations.
- **Filter structure:** batch observations into 0.25–0.5 s epochs and run one information-form Kalman update per connected component.
  - With N ≤ about 300 receivers, a dense 2N state (600×600) is fine at 2–4 Hz.
  - Beyond that, use a sliding-window factor graph with sparse Cholesky, or partition geographically.
- **Gauge:** fixed by the anchors. With no anchor, add a soft constraint Σθ = 0 with a weak prior.
- **Outliers:** NIS gate χ²(1) at 99.9 %, applied per observation.
  - Per-receiver and per-aircraft "bad sync" counters.
  - An aircraft with consistently biased residuals is blacklisted; this catches bad NACp claims and spoofing.
  - A receiver with a step in its innovations gets a **clock-jump reset** of its θ variance only, keeping φ, as the wiedehopf fork does.
- **Receiver position self-survey (big win)**
  - With the network synchronised, each receiver's position rᵢ is observable from many aircraft at known positions. This is GPS in reverse, with aircraft as satellites.
  - Run a slow per-receiver estimator for rᵢ, or just a bias test against the claimed position.
  - Flag "claimed position off by X m" and optionally auto-correct for MLAT use, keeping the displayed public position fuzzed.
  - This also **upgrades AirView's "Locate Me"** from about 30 miles to tens of metres for any feeder that has at least one synced peer.

### 4.3 `group`: frame grouping and clustering

- Convert every timestamp to network time immediately, with its variance from the clock covariance.
- Key: frame bytes. For DF20/21 use the bytes plus the recovered address; strip AP-parity ambiguity by using the decoded ICAO.
- Dedup window: max TDOA (≈ D_max/c ≈ 1.7 ms at 500 km) plus clock uncertainty.
- Identical DF4/DF20 replies to rapid radar interrogations recur every 2.5–10 ms. The pairwise gate |tᵢ − tₖ| ≤ dᵢₖ/c + kσ separates them.
- **Adaptive flush:** wait only until the p99 network latency of the receivers that usually hear this aircraft has elapsed, typically 150–400 ms. mlat-server waits 0.9–2.5 s.
- Late copies may still update the track.
- Co-located receivers (< 1 km) count once for DOF, but are averaged as a single measurement with reduced variance.

### 4.4 `solve`: per-cluster position

- Unknowns: x (ECEF) and T. Solve in a local ENU frame centred on the initial guess for conditioning.
- **Initialisation order**
  1. Track prediction.
  2. Linear TOA least squares when N ≥ 5.
  3. Bancroft when N = 4.
  4. Grid + LM when N = 3 + altitude, keeping 2 hypotheses.
  5. Centroid at the barometric height as a last resort.
- **Optimiser:** LM, 6–10 iterations, analytic Jacobian, at most 4×4 normal equations. That is microseconds per fix and zero allocations.
- **Altitude row:** σ_h from the baro→geo field (about 15–30 m), inflated with altitude age and vertical rate.
- **Robustness:** Huber IRLS, then χ² RAIM test and leave-one-out exclusion; RANSAC when N ≥ 6.
- **Outputs:** CRLB covariance (§3.5), scaled by max(1, χ²/DOF), plus the DOF, the excluded receivers and a quality grade.
- **Rejection rules**
  - Horizontal σ > 1–2 km.
  - Altitude disagreement > 4σ.
  - Implied speed from the track > 600 kt (mach-aware).
  - Below the horizon of any participating receiver, using the terrain/line-of-sight module.

### 4.5 `track`: IMM tracker

- Constant-velocity and coordinated-turn models in ENU (re-centred), with a separate vertical model.
- Measurements: raw TOA clusters of 2 or more receivers, full fixes, DF17 velocity, BDS 5,0 and 6,0, and Mode S altitude.
- Track management: init on a fix with M-of-N confirmation; coast; drop on σ_pos > 5 km or after 60 s with no update.
- **Optional state augmentation:** add the per-aircraft "position latency" bias used in clock sync.
- Display output is the filtered state at about 1 Hz, with per-fix covariance.

### 4.6 `quality`: continuous self-validation (unique selling point)

- **Hold-out:** continuously MLAT a rotating 10 % of ADS-B aircraft. Exclude them from clock sync to avoid circular validation, solve them as if they were Mode S-only, and compare with their reported positions (NACp ≥ 9).
- Publish live error statistics:
  - P50, P95 and P99 horizontal error;
  - error maps per region and altitude band;
  - per-receiver contribution and residual bias.
- This drives automatic tuning of σᵢ per receiver and per-feeder health on the stats pages each feeder sees.
- **CRLB coverage map** for the community, showing where a new feeder would help.

### 4.7 Feeder data and privacy (ties to the aggregator mode)

- The AirView feeder already ships a timestamped Beast stream through the hub data port. The server can therefore do sync and MLAT on the full stream, with no separate mlat-client.
  - Cost: about 1–3 k frames/s × about 22 B ≈ 25–70 kB/s per feeder. Fine for community scale.
  - Optionally adopt selective forwarding (start/stop ICAO lists) when there are more than about 100 feeders.
- **Do not fuzz receiver positions inside the MLAT engine.** Exact positions are required: 100 m of position error is 100 m of range error. Privacy fuzzing belongs at the display and API layer only.
- Results back to feeders: only aircraft positions, never other feeders' identities or positions. This matches mlat-server's approach.
- Store per-receiver statistics keyed by feeder ID for "your own stats" views.
- **Optional mlat-client compatibility** (so existing piaware, readsb and ultrafeeder users can join):
  - Implement the protocol clean-room from the documented behaviour above.
  - Interoperating with a protocol does not require copying AGPL code, but keep the implementation independent and record this choice.
  - Clients sending `clock_type` dump1090 are given non-anchor clock models.

### 4.8 Performance

- Clock filter: about 2–4 Hz per component.
- Solver: microseconds per cluster.
- Hundreds of feeders × a few thousand clusters/s fit easily on one core.
- Shard tracks by ICAO across goroutines. Keep the clock estimator single-threaded per component and publish immutable snapshots via atomic pointer swap for lock-free reads.

### 4.9 Expected accuracy (targets, to be validated by §4.6)

| Network | Expected horizontal accuracy |
|---|---|
| Mostly Radarcape/GNSS-anchored, good geometry | **10–30 m** (competition synced best: 25 m) |
| Mixed RTL-SDR with joint sync and tracking | **30–100 m** (Sonnleitner/Hobiger simulation: median < 50 m) |
| mlat-server (comparison) | typically tens to hundreds of metres |

### 4.10 Testing

- Extend AirView's simulator (`internal/sim`) to emit the same transmissions to N virtual receivers. Each gets its own clock model (offset, drift random walk, jitter), position error and dropout; truth comes from the world model.
- The simulator already models GNSS-minus-baro and geoid, which helps.
- Unit tests:
  - CRLB versus Monte-Carlo error;
  - Bancroft and 3-receiver ambiguity;
  - clock-jump recovery;
  - a mis-positioned receiver is detected;
  - refraction model against a numeric ray-trace.

---

## 5. Licences of referenced work

- **mlat-server** (mutability, wiedehopf): AGPL-3.0-or-later. Do not copy code. A network service built from it must publish source.
- **mlat-client:** GPL-licensed (mutability/wiedehopf; GPLv3 as far as I know, check the repository COPYING). It is a protocol reference only.
- **readsb, dump1090, tar1090:** GPL family (verify per repository). Interoperate via formats only.
- **pykalman:** BSD.
- **gonum:** BSD-3; fine to use.
- **EGM96/EGM2008 geoid:** NGA, public domain.
- **NAVIGATION 2025 paper:** CC BY 4.0.
- **arXiv 2209.13669:** arXiv licence. Cite it; ideas are free to use.
- **US patents** 9377524, 8791861 and 9255804: check status before implementing the specific claimed methods.

---

## Sources

- https://github.com/mutability/mlat-server, files `mlat/server/{clocksync,clocktrack,clocknorm,solver,mlattrack,kalman,config}.py`
- https://github.com/wiedehopf/mlat-server, files `mlat/{clocktrack.pyx,config.py,jsonclient.py}`
- https://arxiv.org/abs/2209.13669
- https://navi.ion.org/content/72/3/navi.704
- https://arxiv.org/abs/2108.00922
- https://www.flightradar24.com/blog/inside-flightradar24/how-we-track-flights-with-mlat/
- https://wiki.jetvision.de/wiki/Mode-S_Beast:Data_Output_Formats
- https://jetvision.de/jetvision-mlat-server-systems/
- https://isas.iar.kit.edu/pdf/ICRA20_Frisch.pdf
- https://www.icao.int/sites/default/files/APAC/Meetings/2022/2022%20SURICG-7/4-Information%20Papers/IP14_CHN-AI.8a-Introduction-to-the-Test-of-the-WAM-Technology-in-the-Final-Approach-Monitoring-v2.pdf
- https://www.faa.gov/sites/faa.gov/files/about/office_org/headquarters_offices/ato/WAM_WhitePaperFINAL_MITRE_v2.pdf
- https://dspace.cvut.cz/bitstream/handle/10467/85526/
- https://www.eurocae.net/working-group/WG-51-SG-5
- https://github.com/TanerH/mlat-server
- https://cris.tau.ac.il/en/publications/time-difference-localization-in-the-presence-of-outliers/
- https://arxiv.org/pdf/2103.01885

## Relevant AirView files (read-only)

- `D:\AirView\internal\feed\beast.go`: Radarcape status, `GPSLocked`, `IsMLATTimestamp`
- `D:\AirView\internal\latency\timebase.go`: GPS ns-of-day to Unix
- `D:\AirView\internal\modes\commb.go` and `D:\AirView\internal\modes\infer.go`: BDS 5,0 / 6,0
- `D:\AirView\internal\trajectory\smooth.go`: RTS smoother
- `D:\AirView\internal\sim\physics.go`: GNSS-minus-baro model
- `D:\AirView\docs\ARCHITECTURE.md`


Recommendations:
- Build internal/mlat as a clean-room pure-Go engine with these packages: geo, clock, group, solve, track, quality. Never copy AGPL code from mlat-server; reference its behaviour only.
- Replace mlat-server's pairwise PI clock loops and spanning tree with one joint network clock Kalman/information filter per connected component. States are offset and frequency, plus a fixed-delay bias for anchors. Radarcape GPS-locked receivers are anchors (prior 0 +/- 50 ns), dropped automatically on loss of GPS lock (Beast type-4 status, existing GPSLocked()).
- Use every ADS-B frame for sync (interpolated to the transmit instant from the decoded ADS-B track), not only even/odd position pairs. Weight observations by NACp/NIC and timestamp jitter; NIS-gate each observation and blacklist aircraft or receivers whose residuals stay biased.
- Model propagation as geometric range plus an exponential-atmosphere refraction excess, with an online-estimated scale factor. Convert Mode S pressure altitude to ellipsoid height using a learned regional GNSS-minus-baro field from DF17 frames, with METAR QNH and temperature as fallback. Apply a geoid correction to feeder altitudes.
- Solver: LM in local ENU, initialised in order by track prediction, linear TOA least squares (N>=5), Bancroft (N=4), or a 2D grid at the known height (N=3+altitude, keeping both ambiguous roots). Then Huber IRLS, a chi-square RAIM test with leave-one-out exclusion, and RANSAC when N>=6. Report the CRLB-based covariance and gate fixes on horizontal sigma.
- Track with an IMM (constant velocity + coordinated turn) EKF/UKF fed raw TOA clusters, so 2-receiver updates maintain existing tracks. Add DF17 velocity and Comm-B BDS 5,0/6,0 as velocity measurements (AirView already infers BDS registers). Use an RTS smoother for trails.
- Cut MLAT latency from 0.9-2.5 s to about 0.2-0.4 s with adaptive grouping windows based on each feeder's measured network latency. Use pairwise distance/c gating to separate rapidly repeated identical DF4/DF20 replies.
- Add continuous self-validation: MLAT a rotating 10% hold-out of ADS-B aircraft (excluded from sync) and publish P50/P95 error maps plus per-feeder residual bias. Also publish a CRLB coverage map showing where a new feeder would help most.
- Use the synced network to self-survey feeder positions (aircraft as reference transmitters). This detects wrong or fuzzed feeder locations and gives tens-of-metres Locate Me for any feeder with one synced peer, replacing the current ~30-mile method.
- Keep exact feeder coordinates inside the MLAT engine and apply privacy fuzzing only in the display and API layer. Send MLAT results back to feeders without other feeders' identities or locations.
- Extend internal/sim to emit each transmission to N virtual receivers, each with its own clock offset, drift, jitter and position error. Use this for regression tests: CRLB versus Monte Carlo, clock-jump recovery, detecting a wrong receiver position, and 3-receiver ambiguity.
- Defer Mode A/C MLAT to a later phase, restricted to GPS-anchored receivers within about 150 km, using association-free TOA methods (Frisch and Hanebeck).

---


# Research track: receiver self-location ("Locate Me" beyond 30 miles)

Scope: precise self-localisation of a 1090 MHz receiver from the traffic it hears, for (a) AirView standalone and (b) feeders in the planned aggregator mode that do not give a location. I edited nothing under D:\AirView; I only read the code.

## 0. What AirView already has (codebase findings)

- `D:\AirView\internal\analytics\guess.go` is the current Locate Me. It fuses four estimators by inverse variance: a centroid, an inverse-square RSSI fit, a coverage-floor (radio-horizon cone) quantile fit, and a circle fit to the coverage edge. All four run on the all-time heat grid, which uses 0.05° cells (~5.5 km). That grid gets thinned again up to 16× for the optimisers. Two things cap the precision:
  - **Input resolution.** Each cell is 5–90 km.
  - **Cell-mean RSSI.** It mixes aircraft whose transmit power differs by about 6 dB (125–500 W transponder classes) and whose installations differ.
- **The Radarcape already reports its own GNSS position.** `D:\AirView\internal\feed\beast.go` (`BeastPosition = '5'`, `ParseBeastPosition`, float32 lat/lon/alt) and `feed.Manager.OnReceiverPosition` handle it, and `model/aircraft.go:239` offers "use receiver GPS position". The jetvision wiki also documents `systemstatus.json` → `position`. Its `alias` flag shows when the owner has overwritten the GNSS position for privacy ([Radarcape:Software_Features](https://wiki.jetvision.de/wiki/Radarcape:Software_Features)). **For this user's own station, self-location is a solved problem at the 2–5 m level.** The research below matters for feeders without GNSS: RTL-SDR on a Pi, the Windows feeder, an Airspy, a Beast in 12 MHz mode.
- **The hub protocol already carries everything reverse-MLAT needs.** `D:\AirView\internal\hub\proto.go` carries `GPSNs` (ns of day) and `Clock12MHz` per frame, and the hub also forwards the station position. This is also a privacy issue: see §6.
- `D:\AirView\internal\latency\analyzer.go` already estimates the transponder's "transmit − time of applicability" for T=1 messages on the 0.2 s UTC epoch grid (tolerance ±150 ms) and a kinematic position-age estimator. This can correct ADS-B position latency for the reverse-MLAT solver in §2.
- `D:\AirView\internal\terrain` (Terrarium DEM, panorama, ITU-R P.526 knife-edge) gives the DEM height prior and the terrain-horizon fit. `internal/engine/airports.go` gives the airport hints. `internal/sim` can be extended into a multi-receiver clock and geometry simulator for tests.

## 1. Notation and the core observability result

- Receiver U has unknown position **u** and a clock model T_U(t) = t + θ_U + δ_U·(t − t₀) (offset θ, drift δ; more terms or a Kalman state if needed).
- Aircraft message m is transmitted at unknown time τ_m from position **p**_m. **p**_m is known from ADS-B after latency correction.
- c is the speed of light, n_m the refractivity excess, ε the noise.
- The arrival time at U is:

  t_{U,m} = τ_m + |**p**_m − **u**|/c + θ_U + δ_U·Δt_m + n_m + ε   (1)

### 1a. Standalone, from squitters alone, the position is not observable at all

**The argument.** Each message brings one equation and one new unknown, τ_m. For any candidate **u**′, setting τ′_m = t_{U,m} − |**p**_m − **u**′|/c − θ makes every residual zero. The likelihood is therefore flat in **u**: the Fisher information about **u** is exactly zero, whatever the clock quality (GPS ns or a 12 MHz crystal).

**Could priors on τ_m rescue it? No.**
- Squitter intervals are randomised: 0.5 ± 0.1 s for airborne position ([ITU-R M.2413](https://www.itu.int/dms_pub/itu-r/opb/rep/R-REP-M.2413-2017-PDF-E.pdf)).
- T=1 makes the *position's time of applicability* UTC-coupled (0.2 s epochs), not the transmit instant. AirView's own analyser gates transmit − epoch at ±150 ms.
- A prior of σ ≈ tens of ms on τ_m is worth about 10⁴ km of range. The ranges themselves are below 1.5 ms (450 km).

**Conclusion:** "reverse GPS" with a single receiver and ADS-B squitters is impossible. It is worth one cheap experiment on the existing data: histogram, per airframe, (GPS TOA − propagation − nearest 0.2 s epoch). If some transponder model turns out to transmit at a sub-µs-stable offset from the UTC epoch, those airframes would act as pseudolites. This is very unlikely given the mandated jitter. Even if they exist, they help only receivers that have GPS time, which in practice also have GPS position.

### 1b. What does break the degeneracy

A second observation of the *same* transmission, with a known receiver position and a known clock relationship. It eliminates τ_m and turns MLAT inside out (§2). Alternatively, a transmission whose emission time is tied to a known event, such as a transponder reply to an SSR interrogation (§3).

## 2. Method A (aggregator): reverse MLAT, also called "aircraft-as-satellites"

**The equations.** Anchor j has a known position **a**_j and a known clock: GPS-timed, or synchronised through AirView's own MLAT clock graph. It hears the same message:

  t_{j,m} = τ_m + |**p**_m − **a**_j|/c + θ_j + ε_j   (2)

Subtracting (2) from (1) and moving known terms to the left gives a pseudorange:

  ρ_{m,j} ≡ c·(t_{U,m} − t_{j,m} − θ_j) + |**p**_m − **a**_j| = |**p**_m − **u**| + c·θ_U + c·δ_U·Δt_m + e_{m,j}   (3)

This is exactly the GPS navigation equation, with aircraft as the satellites. The unknowns are **u** (3), θ_U and δ_U: 5 parameters, plus piecewise clock knots for poor clocks.

**Solving it:**
- **Closed-form start:** Bancroft's algebraic GPS solution (S. Bancroft, IEEE Trans. AES-21(1), 1985). It needs no initial guess. Alternatively, a coarse grid of ±50 km at 1 km seeded by the existing Locate Me result.
- **Refinement:** Gauss-Newton or Levenberg-Marquardt with a robust (Huber or Tukey) loss.
  - Jacobian row: [−**e**_mᵀ, c, c·Δt_m], where **e**_m is the unit vector from **u** to **p**_m.
  - Covariance: σ²·(HᵀWH)⁻¹. Report a 95 % ellipse.
- **Height prior:** add a pseudo-observation h = DEM(lat, lon) + mast (σ ≈ 20 m). This removes the bad vertical/clock coupling explained below.

**Message association:** identical 56/112-bit payload and ICAO, within a few ms after the anchor clock mapping. Use every DF17/18 position message. Other DF17/18 types (velocity, identity) can be used with **p** interpolated from the track, which roughly doubles the samples.

### Geometry and DOP

Aircraft sit at low elevation (≈0–10°). So the vertical component of **e**_m (sin ε) barely varies, and it correlates with the clock θ_U. This gives a large VDOP, which the SkyPos authors also observed (vertical errors well above horizontal). The DEM prior fixes it.

Horizontal precision depends on how the aircraft are spread in azimuth:
- **Uniform azimuths:** σ_E ≈ σ_N ≈ c·σ_t·√(2/N).
- **A directional antenna seeing a ±α sector:** the along-boresight information scales with var(cos φ) and the cross-boresight with var(sin φ). For α = 30°: var(cos φ) ≈ 0.0016 and var(sin φ) ≈ 0.087. The along-axis error is therefore about 25× (≈ 1/√0.0016) the error of an omni receiver with the same N.
- **Design consequence:** solve on omni / all-channel data. For the EVO 2CH (yagi + vertical) that is fine, because the 2CH has no per-frame channel flag anyway.

### Error budget per pseudorange

- **Timestamp jitter.** mlat-server's clock presets (AGPL-3.0; numbers cited as facts, no code copied, [mlat-server](https://github.com/mutability/mlat-server)):

  | Receiver | Clock | Max frequency error | Timestamp jitter |
  |---|---|---|---|
  | Radarcape GPS | 1 GHz | 1 ppm | 15 ns |
  | Beast | 12 MHz | 5 ppm | 83 ns |
  | dump1090 / SBS | 12 / 20 MHz | 100 ppm | 500 ns |

  The same module rejects a pair-clock fit worse than ~4 µs (variance 16e-12 s²), keeps 30 s of points, and uses outlier tests at 5σ.
- **ADS-B position error and latency.** The NACp error (5–30 m typical) and the position-age error (v·Δage, e.g. 250 m/s × 50 ms = 12.5 m) are *common* to U and the anchor. They enter (3) only as Δ·(**e**_{u,m} − **e**_{a,m}), which is small when the U–anchor baseline is short relative to the range. Example: 30 km baseline at 150 km range gives about 0.2 rad, so 12.5 m becomes about 2.5 m. **Prefer the nearest anchors and T=1 messages.** Correct the position to τ_m with **p**(τ) = **p**(epoch) + **v**·(τ − epoch), using the latency analyser.
- **Troposphere.** The excess path grows as ~1/sin(elevation): metres at 5°, ~100 m near 0°. It largely cancels between nearby U and anchor. Apply the same exponential-refractivity model at both ends and use an elevation mask of 2–3°.
- **Constant cable, filter and FPGA delays** are absorbed into θ_U.
- **Multipath at low elevation:** handled by the robust loss and elevation weighting.

### Expected accuracy

| Case | Theory | Real-world reference |
|---|---|---|
| Radarcape target + GPS anchor | σ_t ≈ 30–50 ns (≈ 10–15 m per row). N = 2000 rows gives σ_h ≈ 0.5 m (white-noise floor). | SkyPos (Lizarribar, Giustiniano, Bovet, Lenders, IEEE JSAC 2023, [paper](https://www.lenders.ch/publications/journals/jsac23.pdf), [record](https://www.aminer.org/pub/65793f38939a5f40821a0b8e/skypos-real-world-evaluation-of-self-positioning-with-aircraft-signals-for-iot)) used OpenSky Radarcape anchors (GPSDO, ~30 ns). A Radarcape with randomised position was relocated with a **2D median error of 21 m, worst 41 m**. Even with **1 anchor the median was 15 m**. About **1 % of messages (~1000 messages ≈ 40 s)** already reached that accuracy. Systematics dominate, not N. |
| 12 MHz / RTL-SDR target (dump1090 class) | Per row 100–150 m. Drift of 1–2 ppm (RTL v3) up to tens of ppm (older dongles) has to be modelled with piecewise clocks or a Kalman filter. | SkyPos measured a **median of 450 m (2D) and 2000 m (3D)**. Those sensors also had hand-entered, uncertain anchor positions and no drift discipline. |

For the 12 MHz case, my estimate is **50–200 m** with a careful drift model, a DEM height prior and nearby GPS anchors. That is an estimate to verify, not a published figure.

**Earlier and related work:**
- Eichelberger et al., "Indoor Localization with Aircraft Signals" (ACM SenSys 2017): about 25 m, with reference ground stations estimating transmit times and clock drift ([summary](https://a416485164.github.io/2019/04/16/Indoor-Localization), [ETH record](https://www.research-collection.ethz.ch/items/5f87f74f-7bb5-467e-99ba-96d0409dfa9e)).
- Sciancalepore, Alhazbi, Di Pietro, "Receivers location privacy in avionic crowdsourced networks" (JNCA 2021): they located OpenSky "protected" receivers to 0.9–2.6 km from public data ([TU/e](https://research.tue.nl/en/publications/receivers-location-privacy-in-avionic-crowdsourced-networks-issue/), [ScienceDirect](https://www.sciencedirect.com/science/article/pii/S1084804520303544)).

### Variants

- **A′ Non-GPS anchors.** Anchors whose clocks are known only through AirView's own MLAT pair-clock graph (offset + drift per pair, ADS-B messages as beacons, the mlat-server *approach* re-implemented). Their errors add to (3).
- **A″ Network self-calibration.** Even two receivers with *both* positions unknown are observable from their mutual TDOAs. The ADS-B aircraft positions fix the absolute frame, so there is no gauge freedom. The unknowns are **u**, **v** and θ_U − θ_V (7 parameters) against thousands of rows. A whole-network bundle adjustment therefore (i) locates feeders who gave no location and (ii) flags feeders whose *declared* location is wrong. Wrong declared locations are the classic cause of bad MLAT sync; flag any feeder whose solved position is more than ~200 m from its declared one.

### Data needs (aggregator)

- Timestamped Beast frames from the feeder. The hub already provides `GPSNs` / `Clock12MHz`.
- At least one anchor with overlapping coverage, ideally a GPS Radarcape (the host station itself).
- About 1000 common position messages (≈1 min) for a first fix; 5–10 min with full azimuth spread for a stable one.
- Compute cost: a few thousand rows × 5–10 parameters, trivial in pure Go (CGO off).

## 3. Method C (standalone, experimental): bistatic SSR interrogation–reply timing

Transponder replies are *not* spontaneous. A reply follows the interrogation after a fixed delay d: Mode S 128 µs, with a test-set tolerance window of 127.65–128.35 µs per [TM 43-6625-916-12](https://radar.tpub.com/TM-43-6625-916-12/TM-43-6625-916-120040.htm); Mode A/C 3 µs. Aircraft A1 and A2 are in the same rotating SSR beam, the interrogator R is known, and both answer the same interrogation:

  t₂ − t₁ = [(|**R**−**p**₂| + |**p**₂−**u**|) − (|**R**−**p**₁| + |**p**₁−**u**|)]/c + (d₂ − d₁)   (4)

So |**p**₂−**u**| − |**p**₁−**u**| is measured. That is a hyperbola for **u**, with τ eliminated by the common interrogation instant.

**Properties:**
- **Clock:** only intervals of a few ms are needed. A free-running 12 MHz clock is fine (5 ppm × 3 ms = 15 ps), so this works for any receiver.
- **Radar position error cancels to first order.** Two aircraft in the same ~2.4° beam lie nearly on one bearing from R, so (**e**_{R,p₂} − **e**_{R,p₁})·Δ**R** ≈ 0.04·|Δ**R**|. A radar position good to ~1 km is enough. That can come from public lists, or from DF11 II/SI interrogator codes plus the beam-rotation method of Sarperi et al., "Localization of interrogators with a 1030/1090 MHz spectrum monitoring system" (ICAS 2022, interrogator located to <3.5 km, [paper](https://www.icas.org/icas_archive/ICAS2022/data/papers/ICAS2022_0144_paper.pdf)).
- **Noise:** about 75 m (Mode S) or 150 m (Mode A/C) per pair from reply-delay tolerance, as quoted in the PSSR literature ([US 5198823](https://image-ppubs.uspto.gov/dirsearch-public/print/downloadPdf/5198823)). Per-transponder constant delay biases can be modelled as nuisance parameters.

**Hard parts:**
1. Mode S aircraft are mostly *locked out* of the main radar's all-calls, so DF11 replies to all-calls are sparse. Roll-call replies (DF4/5/20/21) are individually scheduled and useless here. The practical signal is **Mode A/C replies** (Radarcape outputs them when enabled). These have no address, so they must be associated with ADS-B tracks via the squawk and Mode C altitude.
2. Garbling. A reply lasts 20.3 µs, so aircraft within ~6 km slant-range difference overlap.
3. Grouping replies by interrogation. PRF is ~100–450 Hz. Resolve the PRI ambiguity with the coarse location prior (±50 km → ±333 µs) and by matching the whole reply trains across the dwell.
4. Interlace patterns differ by radar.

I found no published *receiver* self-localisation system using this. Related work covers PSSR (passive SSR, slaved to a master radar) and interrogator localisation. My estimate is 100 m–2 km after hours of data; this is an R&D item, not a v1 feature.

## 4. Method D (standalone, no timing): better RSSI, horizon and terrain fitting

1. **Raw samples instead of heat cells.** Fit on per-position samples from `flights` / trace data rather than 0.05° cells. That removes the ~5 km quantisation.
2. **Per-aircraft RSSI normalisation.** Model rssi_{a,k} = P_a − 20·log10 r_k(**u**) − G(az, el) + n, with a free offset P_a per airframe and session. This removes the transponder-power and installation spread. The information then comes from the *within-track* change of log range. Overflights and approaching or departing tracks give the most. Solve jointly with a low-order antenna-gain model in azimuth and elevation, which matters for the yagi; `analytics/antenna.go` currently assumes a known position. The closest-approach time of straight tracks (RSSI peak) gives a line of position perpendicular to each track. Mind the zenith null of vertical antennas. **Estimate: 2–10 km.**
3. **Terrain-aware horizon fit** (terrain-aided-navigation style). For candidate **u**, the DEM panorama gives a horizon elevation H_u(az). The likelihood rewards:
   - observed positions at or above the horizon;
   - first-contact and last-contact points lying on the predicted visibility boundary;
   - penalised gaps: positions well above the horizon that were never seen.

   Search coarse to fine: a 2 km grid, then 200 m. Use a 1°-azimuth panorama per candidate in the background job that `terrain` already offers. Hills give distinctive signatures and a sea horizon gives a clean circle. **Estimate: 0.5–5 km in hilly terrain, 5–15 km over flat land.** This is a generalisation of the existing "floor" fit, which uses a smooth 4/3-earth cone with no terrain.
4. **Grid posterior fusion.** Multiply the method likelihoods on a lat/lon grid instead of inverse-variance averaging (`fuse()`). The constraints in §4–§5 are multimodal or one-sided (discs, horizons) and are not Gaussian.

## 5. Method E: surface, airport and ground-vehicle hints

- **Surface position messages (TC 5–8)** from aircraft at airport X prove line of sight to its surface. The emitter is ~3–5 m high, so d ≤ 4.12·(√h_r + √h_e) km with k = 4/3. For a 10 m mast that is ≈ 13 + 8 = 21 km, perhaps 30–40 km with diffraction or a hilltop. Each airport heard gives a disc. Surface messages have high-NACp positions, so a DEM line-of-sight check to the actual taxiway points sharpens it in hilly areas. RSSI of surface messages adds range information.
- **Departures and arrivals.** The lowest altitude at which a climbing or descending aircraft is first or last heard near airport X bounds the distance with the same horizon formula. This is denser than surface data. Tie it to `engine/airports.go`.
- **DF11 / DF4/5 with on-ground flags (CA=4, FS)**, matched later to ADS-B tracks, give the same kind of airport association.
- **Emitter categories.** C1/C2 surface vehicles are mobile, so they only add airport presence. C3–C5 obstacles, if any are heard, are fixed known-position emitters: useful but rare.
- **Alone this gives about 5–20 km** where several airports are within range. Its real value is as a strong prior that kills wrong modes in the §4 posterior.

## 6. Privacy implications (important for aggregator mode)

- **A feeder who hides their location is still locatable by the server.** If the feeder streams timestamped frames, reverse MLAT (§2) locates them to ~10–40 m (GPS clock) or ~0.1–0.5 km (12 MHz), whatever location they declare. Even without timestamps, coverage, RSSI and terrain fitting give roughly 1–15 km. Sciancalepore et al. showed this in practice against OpenSky's protection (0.9–2.6 km).
- **"Hide my location" can therefore only mean hidden from other users and the public, never from the operator.** The consent text must say so.
- **How public networks present it.** OARC's map marks feeder locations as approximate ([adsb.oarc.uk](https://adsb.oarc.uk/map)). TheAirTraffic randomises MLAT positions on its feeder map and states the exact location is used only for MLAT ([privacy](https://theairtraffic.com/privacy), [feeder map](https://dev.theairtraffic.com/feeder-map)). FlightAware offers location obscuring on its site.
- **Feeder installer rule: Radarcape Beast type '5' frames carry the exact GNSS position.** Forward them only on opt-in, and respect the Radarcape `alias` flag. AirView's hub currently forwards the station position to remote-data clients: fine for "my own PC", but a leak in a multi-tenant aggregator.
- **Server rules:**
  - Compute self-location only for the requesting feeder ("Locate me") or for admin quality control.
  - Store solved positions as sensitive data: owner and admin only, never in public JSON, sync maps or logs.
  - Per-feeder coverage plots and range rings reveal location too, so show them only to the authenticated owner.
  - Use a feeder key or UUID rather than the source IP. CGNAT and dynamic IPs make IP-based identity both wrong and itself personal data.
- **Countermeasures a feeder can choose** (from Sciancalepore et al.): send no or nullified timestamps (no MLAT, no timing-based localisation), corrupt or coarsen timestamps, or group several receivers under one ID.
- **Legal:** a precise home location of an identifiable amateur operator is personal data under UK GDPR. The aggregator operator is a controller and needs a stated purpose (MLAT), retention and an access policy.

## 7. Summary table: expected accuracy and data needs

| Method | Needs | Expected horizontal accuracy | Status |
|---|---|---|---|
| R0 Receiver GNSS (Radarcape type '5' / systemstatus.json) | GNSS receiver | 2–5 m | Already in AirView |
| A Reverse MLAT, GPS-timed target + GPS anchor | Aggregator, ≥1 anchor, ~1 min of common traffic | 10–25 m median, worst ~40 m (SkyPos). Vertical poor without a DEM prior | Recommended v1 for aggregator |
| A Reverse MLAT, 12 MHz / RTL-SDR target | Same, plus drift modelling | 50–500 m (SkyPos 450 m median; 50–200 m is my estimate with a DEM prior and drift model) | v1 |
| A″ Network bundle adjustment | Several feeders with overlapping coverage | Similar to A, slightly worse | v2; also catches mis-declared feeders |
| B Standalone squitter timing | – | Not observable (proved above) | Do not build |
| C Bistatic SSR reply timing | Mode A/C output, interrogator positions, hours of data | 0.1–2 km (estimate) | Research |
| D Per-aircraft RSSI + raw samples + terrain-horizon fit | Hours to days of own data, DEM | 0.5–10 km (estimate) | Upgrade of the current Locate Me |
| E Airport and surface hints | Airport DB, surface traffic | 5–20 km alone; strong prior | Cheap add-on |
| Current Locate Me | Heat grid | ~30 miles (~48 km) | Existing |

Validation: this user's Radarcape GNSS position is a ground truth. Every standalone method can be scored automatically (`ErrorKm` already exists in `GuessEstimate`). For method A, a randomised-position Radarcape can be relocated the same way SkyPos did.

## 8. Licences of referenced material

| Item | Licence | How it is used |
|---|---|---|
| mlat-server (mutability / wiedehopf) | AGPL-3.0, network-use clause | Do not copy. Approach and parameters are cited as facts only. |
| mlat-client | GPL-3.0 | Not copied. |
| SkyPos (IEEE JSAC), SenSys 2017, JNCA 2021, ICAS 2022 papers | Copyrighted | Cited, not reproduced. |
| Bancroft 1985 | Published algorithm | Free to implement from the description. |
| ITU-R reports | Free to read | Cited. |
| Mapzen / Tilezen terrain tiles | Open data, attribution required | Already used and attributed by AirView. |
| jetvision wiki | Documentation, facts cited | Cited. |

Scratch extracts I made of the two PDFs (for reference only, not deliverables):
- `C:\Users\chris\AppData\Local\Temp\claude\7cf8c843ccba07dc\559b9eaf-9013-47b7-bf37-8e45b676bc65\scratchpad\sk1.txt`
- `C:\Users\chris\AppData\Local\Temp\claude\7cf8c843ccba07dc\559b9eaf-9013-47b7-bf37-8e45b676bc65\scratchpad\icas.txt`


Recommendations:
- For any Radarcape (including the user's own), always prefer the receiver's GNSS position from Beast type '5' frames, which AirView already parses, or from systemstatus.json; show the solved estimates only as a cross-check. Respect the Radarcape 'alias' privacy flag.
- Build reverse MLAT ('aircraft as satellites') as the aggregator's self-location engine: pseudorange rho = c*(t_U - t_anchor - theta_anchor) + |p - a_anchor|. Start with a Bancroft closed-form solution or a coarse grid seeded by the current Locate Me, refine with robust Gauss-Newton over position, clock offset, drift and piecewise clock knots, and add a DEM height pseudo-observation (sigma ~20 m) to remove the vertical/clock degeneracy. Target accuracy: 10-25 m for GPS-timestamped feeders and 50-500 m for 12 MHz feeders after ~1-10 min of common traffic.
- Correct ADS-B positions to the transmit instant (T=1 epoch plus velocity, using the existing internal/latency analyser), prefer the nearest anchors (position errors cancel as baseline/range), apply the same troposphere model at both ends with a 2-3 degree elevation mask, and match messages by ICAO + full payload + time window.
- Solve on omni / all-antenna data. A directional-only sector of +/-30 degrees inflates the along-boresight error about 25x (var(cos phi) ~ 0.0016).
- Re-implement MLAT pair-clock sync in AirView's own code (offset + drift per receiver pair, ADS-B beacons, outlier gating). Use mlat-server only as a published-parameter reference and do not copy its AGPL code. Reuse the same clock graph for non-GPS anchors in reverse MLAT.
- Add a network-wide bundle-adjustment job that solves every feeder's position from mutual TDOAs and flags feeders whose declared position is more than ~200 m from the solved one; this protects MLAT quality.
- Do not build standalone 'reverse GPS' from squitters: it is provably unobservable because each message adds an unknown transmit time and squitter jitter is 0.5 +/- 0.1 s. Optionally run a cheap experiment histogramming per-airframe (TOA - propagation - 0.2 s epoch) to rule out deterministic UTC-coupled transmitters.
- Upgrade standalone Locate Me: (1) fit on raw position samples, not 0.05-degree heat cells; (2) per-aircraft RSSI offsets with a joint antenna-gain model; (3) DEM-panorama horizon likelihood searched coarse-to-fine (2 km, then 200 m); (4) surface-message, departure and arrival airport line-of-sight discs; (5) fuse on a lat/lon grid posterior instead of inverse-variance averaging. Score every method automatically against the user's Radarcape GNSS position.
- Treat bistatic SSR interrogation-reply timing (Mode A/C replies to the same interrogation, interrogator position cancelling to first order) as an R&D track, not v1.
- Privacy by design for the aggregator: identify feeders by key/UUID, not IP; make solved positions, per-feeder coverage plots and range rings visible only to the owning feeder and the admin; never publish them in sync maps or logs; strip type '5' GNSS frames in the feeder installers unless the user opts in; state plainly in the consent text that the operator can always locate a timestamp-sending feeder; offer a 'no timestamps (no MLAT)' mode for feeders who want to stay unlocatable by timing.