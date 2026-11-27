# AirView Network: final design for aggregator mode, feeders and own MLAT

**Status:** final design for implementation. It was written read-only: nothing under `D:\AirView` was changed. Every protocol and algorithm here is described in our own words from public behaviour, documentation, papers and the AirView source.

**Clean-room rule:** no GPL or AGPL code (readsb, tar1090, mlat-client, mlat-server) is copied, linked or bundled. Its provenance is recorded in `docs/aggregator/CLEANROOM.md`.

**Build constraints, verified in `go.mod` and `ARCHITECTURE.md`:**
- Go 1.27.3, CGO off.
- No new third-party modules.
- Allowed: the standard library (including `crypto/tls`, `crypto/ed25519`, `crypto/ecdsa`, `crypto/x509`, `crypto/pbkdf2`, `crypto/hkdf`, `crypto/hmac`, `compress/flate`, `compress/zlib`), `golang.org/x/sys` (including `windows/svc` and `windows/svc/mgr`), and `modernc.org/sqlite`.

---

## 0. Decisions at a glance

| Topic | Decision |
|---|---|
| Modes | `standalone` (unchanged; optional `uplinks[]`), `aggregator` (multi-station server; optional host station and uplinks), `feeder` (used only by the `airview-feeder` binary). `remote-data` and `remote-web` are unchanged. |
| Binaries | `airview` (existing GUI/Pi build): standalone, aggregator, remote-*. `airview-feeder` (new, headless): Pi, Windows service and tray. Both share `internal/uplink`. |
| Native protocol | **AVF/1** on TCP **30160**. TLS 1.3 only, ALPN `avf/1`. Ed25519 feeder key, bound to the channel with a TLS exporter. JSON handshake, then binary records in a flate stream with seq/ACK. Frames carry unwrapped receiver ticks, clock class, epoch and host time. |
| Compat ingest | TCP **30004**: Beast, beast_reduce_out, beast_reduce_plus_out (0xE4 UUID, 0xE3 trusted-relay receiver ID, 'P'/'W'). Used for map and stats only, **never MLAT**.<br>TCP **31090**: clean-room mlat-client protocol v3 over TCP, used for MLAT and survey.<br>**localhost SBS-in:** a bridge for an operator's separate, unmodified mlat-server during migration. |
| Identity | Station = one receiver clock. Native feeder ID = base32(SHA-256(pubkey)[:10]). Compat identity = UUID; a missing UUID becomes an "unidentified" IP-keyed station. |
| Accounts | Owners: username + PBKDF2-SHA256 (600k) + optional TOTP. No e-mail. Admin: mandatory password + TOTP. Claims use a server-minted single-use code (native) or UUID + same IP (compat). |
| Storage | `airview.db`: merged network data plus `flight_station`. `stations.db`: registry, owners, audit, and existing stats tables keyed `ant='s<idx>'` / `'s<idx>/<ant>'`. |
| Ingest and tracker | Grouper in `internal/ingest`. The first copy is decoded by the tracker; later copies are credited to lanes without the tracker lock. The same-station repeat rule splits repeated identical transmissions. Geometry is computed per viewer at snapshot time. |
| MLAT | `internal/mlat/*`. One covariance-form joint clock Kalman filter per connected component, 0.5 s epochs. States: θ, φ (+ψ for SDR), β_short; anchors have δ only. GPS anchors go through probation. Sync uses every DF17/18 frame plus DF0/4/5/11 frames from ADS-B aircraft. LM solve with full-covariance weighting, Huber, RAIM, CRLB. IMM raw-TOA tracker. Hold-out self-validation. |
| Self-location | Receiver GNSS → declared (checked) → reverse MLAT (10-25 m GPS-timed, 50-500 m 12 MHz) → Locate Me v2 (2-10 km). Owner and admin only. Used for MLAT after owner confirmation. |
| Privacy | Pins hidden by default. Opt-in pins are a keyed offset in a cell, computed once. No receiver information of any kind on public surfaces. Coverage-type products and the CRLB map are owner/admin (CRLB: admin) only. Peer tags cannot be linked. Every exact-data admin view is audited. |
| Hub 30154 | Aggregator mode: admin login only. Clients that are not speaking the hub protocol (first byte 0x1A, or no AUTH within 2 s) are closed quietly. |

---

## 1. Modes and configuration

| Mode | Local tracker/DB/UI | Ingest listeners | Uplinks | MLAT engine |
|---|---|---|---|---|
| standalone | yes (today) | hub 30154 (as today) | optional | no |
| aggregator | yes, multi-station | 30160, 30004, 31090, localhost SBS-in (each 0 = off) | optional (federation, phase 5) | yes |
| feeder (`airview-feeder`) | no; status page on 127.0.0.1:8099 | none | ≥1 | no |
| remote-data / remote-web | unchanged | – | – | – |

**Host station.** In aggregator mode the host's own feeds register as station `#1` ("host"):
- owned by the admin;
- exact location;
- anchor-eligible;
- they enter `internal/ingest` in-process through the feed manager sink.

**Uplink tee.** In standalone and aggregator modes the sink becomes `tee{tracker or ingest, hub, uplinks...}`. An uplink never forwards:
- frames carrying the MLAT magic timestamp;
- zero timestamps as timed frames;
- frames from any feed marked `noForward`. Feeds receiving MLAT results (for example a 30104-style input) default to `noForward`.

**Config contract** (`internal/model/config.go`, owned by WP0):

```go
const ModeAggregator = "aggregator"; const ModeFeeder = "feeder"

type Uplink struct {
    ID, Host string; Port int        // default port 30160
    ServerPin   string               // "sha256/<b64 SPKI>"; "" = system roots
    EnrolToken  string               // secret; redacted like hub password
    Inputs      []string             // local feed IDs; empty = all timed feeds
    SendLocation bool                // default true
    ShareGNSS   bool                 // forward Radarcape type '5'; default false; never if alias flag set
    Timestamps  bool                 // default true; false = no MLAT, no timing-based self-location
    ReturnMLAT  bool                 // default true
    SpoolMB     int                  // 0 = RAM ring only (default on SD-root Pi); Windows default 1024
    Disabled    bool
}
type Aggregator struct {
    Name string
    AVFPort, BeastPort, MLATPort, SBSInLocalPort int // 30160, 30004, 31090, 0
    Admission string                 // "open" | "approve" | "invite" (default "invite")
    TLSCert, TLSKey string           // empty => self-signed ECDSA P-256 in <data>/avf-tls/
    TrustedProxies []string          // CIDRs whose X-Forwarded-For is honoured
    PublicMap bool; PublicStats bool; TarJSON bool   // defaults true, true, true
    DefaultPin string                // "hidden" (default) | "region" | "cell01"
    IslandGuard bool                 // default false (see §8.6)
    TrustedRelays []string           // UUIDs whose 0xE3 receiver IDs are honoured
    MLAT struct { Enabled bool; ReturnResults bool; Holdout float64; AutoUseSurvey bool }
    Retention struct { ConnLogDays, MinuteDays, HourMonths, OfflinePurgeDays, AuditMonths int }
    Limits struct { MaxStations, SelectiveAbove, PerIPStations, MaxFPSPerStation int }
}
```

The registry, keys, owners, claims and tokens are mutable secrets. They live in `stations.db`, never in `config.json`. `config/secrets.go` redaction is extended to `EnrolToken` and the TLS key path.

---

## 2. Topology

```
 AirView feeders (AVF/1 TLS :30160)   readsb (Beast :30004)   mlat-client (JSON :31090)   host station (in-proc)
            |                                |                          |                           |
            +---------------> internal/ingest: sessions, host-offset estimator, normaliser <--------+
                                   |   GROUPER (payload hash + same-station repeat rule)
             first copy -> tracker.Handle()        later copies -> lanes.Credit()   closed timed groups -> mlat
                                   |                       |                                   |
                         global tracker (merged map,   per-station lanes             internal/mlat engine
                         flights, positions)           (counters, range, coverage,   (clock, group, solve,
                                   ^                    heat, signal, elevation)      track, quality, survey)
                                   +------------- MLAT tracks (src=mlat-av) ----------------+
                                   |                                                        +--> results to contributors
            airview.db (merged) + stations.db (registry + namespaced stats)
            HTTP: /api/public/* | /api/me/* | /api/admin/* | /data/aircraft.json, /data/receiver.json (tar1090-shaped)
            optional bridge: operator's unmodified mlat-server -> 127.0.0.1:SBSIn -> src=mlat-ext
```

---

## 3. Station model and identity

**Station.** A station is one receiver timestamp clock. An EVO 2CH is one station with two channels. A Pi running both readsb and a Radarcape is two stations under one feeder.

`StationIdx uint32` is a dense in-process index. `SID` is a public opaque ID: base32 of 10 random bytes, never derived from location or IP.

**`stations` columns:**

| Group | Columns |
|---|---|
| Identity | idx, sid, feeder_id, rx_no, kind (avf / beast / mlatc / host), uuid, name |
| Clock | clock_class |
| Location | declared lat/lon/hae (+ alt_ref), gnss, estimated, effective + source + σ |
| Status | loc_status (unknown / estimating / estimated / confirmed / declared / gnss / suspect) |
| Privacy and ownership | pin precision + stored public point, owner_id |
| Admission and health | trust (karma), admission state, created, last_seen |

**Native feeders.**
- Ed25519 key in `identity.key` (0600).
- Feeder ID = base32(SHA-256(pubkey)[:10]).
- One feeder can announce several receivers (`rx_no`).
- **Clones:** the same key seen from two IPs at once is detected. The second session is denied with "cloned feeder", and `airview-feeder new-identity` fixes it.

**Compat feeders.**
- UUID from 0xE4 on 30004, or from `uuid` in the 31090 handshake. UUIDs are normalised: hex, dashes stripped, at least 28 characters.
- A 30004 link and a 31090 link with the same UUID are one station.
- A link without a UUID is joined to a station on the same IP within 120 s and flagged `link_guess`.
- No UUID at all: `ip:` + HMAC(serverKey, ip‖port). The station is flagged "unidentified". Its stats merge by IP within 10 minutes.
- **0xE3 receiver IDs** are honoured only from `TrustedRelays`, where each ID becomes a sub-station. From anyone else, 0xE3 is ignored.

**Admission policies** (UUID-keyed for compat feeders):
- `open`: auto-approve.
- `approve`: the station is held pending. Its data goes to its own lane only, never to the public map or MLAT.
- `invite` (default): requires an enrol token. Tokens are minted by the admin, have a configurable maximum use count (default 1), an expiry (default 7 days), are revocable and are stored hashed.

---

## 4. AVF/1 protocol (TCP 30160)

### 4.1 Transport

**TLS.** TLS 1.3 only, ALPN `avf/1`. There is no plaintext mode. The server certificate is one of:
- **(a)** an operator certificate (`TLSCert`/`TLSKey`, reloaded when the file mtime changes), verified against system roots; or
- **(b)** by default, a self-signed ECDSA P-256 certificate generated on first start. Its SPKI pin (`sha256/<b64>`) is shown in the admin UI and embedded in the install one-liner. The client checks it in `VerifyPeerCertificate`.

A TCP passthrough proxy in front is supported; TLS terminates in AirView.

**Handshake** (inside TLS; `u32 BE length + JSON`):
1. S→C `HELLO_S`: `{proto:1, serverId, nonce(b64 32B), timeNs, features[], admission}`.
2. C→S `HELLO_C`: `{body: b64(JSON bytes), sig: b64}`.
   - `body` = `{pub, uuid, software, version, os, arch, features[], resumeToken?, enrolToken?}`.
   - `sig = Ed25519(priv, "AVF1" ‖ serverId ‖ nonce ‖ ExportKeyingMaterial("EXPORTER-AirView-AVF1", nil, 32) ‖ SHA-256(bodyBytes))`.
   - Signing the raw body bytes avoids JSON canonicalisation.
3. S→C `WELCOME`: `{feederId, status: approved|pending|denied, sessionId, resumeToken, ackSeq, config:{batchMs:50, mode:"full"|"selective", mlat:bool, returnResults:bool}, motd, reconnectIn?}`.
   - A deny carries a reason and closes the connection.

**After the handshake.**
- Each direction is one `compress/flate` stream at level 1.
- `Flush` happens every `batchMs`, at 32 KiB, or immediately after a control record.
- **Decompression guards:** inflated output at most 2 MB/s per connection, and an inflate/wire ratio cap of 100:1.

### 4.2 Records
Each record is `u8 type | uvarint len | payload`. The maximum record is 64 KiB. Unknown types are skipped. Varints are LEB128, with zigzag for signed values. Each C→S data record starts with `uvarint seq`. The server sends `ACK` once a second; the client trims its ring up to the ACK, and the server drops seq values at or below its watermark.

**Client to server**

| Type | Name | Payload |
|---|---|---|
| 0x01 | RECEIVER (JSON) | `rx_no`, name, clockClass, nominalHz, epochId, channels, decoder string; location `{lat, lon, alt, altRef:"hae"\|"msl", source:"manual"\|"gnss"\|"estimated", sigmaM}` only if `SendLocation`; antennas `[{id, kind, aglM}]`; privacy `{pin, publicStats, showName}`. Never hostnames or LAN IPs. |
| 0x02 | FRAMES | seq, rx_no, epochId, `baseTicks u64`, `baseHostNs i64`, n; per frame: `hdr u8` (bits 0-1 kind A/C, short, long; bit 2 late; bit 3 tsInvalid; bits 4-7 channel+1), `dTicks uvarint` (frames sorted by ticks within a batch), `dHostUs svarint`, `sig u8` (raw Beast byte), payload 2/7/14 B. |
| 0x03 | CLOCK_STATUS | Radarcape type-'4' fields: settings byte, NCO-vs-1PPS signed ticks, GPS status byte. 1 Hz while present. |
| 0x04 | CLOCK_PAIR | (rx_no, epochId, ticks, hostUtcNs, hostErrEstNs), every 5 s |
| 0x05 | CLOCK_EVENT | {rx_no, oldEpoch, newEpoch, reason: restart\|sample_drop\|jump\|backwards\|mode_change, samplesLost?} |
| 0x06 | INPUT_STATUS | connected, fps, CRC errors, detected type |
| 0x07 | GNSS_POS | Radarcape type '5'. Only if `ShareGNSS` and the alias flag is clear. |
| 0x08 | DECODED | Untimed updates from SBS/JSON inputs. Map only. |
| 0x09 | MINUTE_STATS | Exact per-minute counters computed at the feeder: frames by DF, unique ICAOs, positions, CRC failures, a 1 dB RSSI histogram |
| 0x0A | PONG | Echoes the PING id with the feeder's receive and send host ns |
| 0x0B | BACKFILL | begin/end {fromSeq, toSeq, spanNs}. FRAMES in between have the late bit set. |

**Server to client**

| Type | Name | Payload |
|---|---|---|
| 0x81 | PING | id, serverSendNs, every 5 s. Min-RTT filter gives the RTT and the feeder host-clock offset. |
| 0x82 | ACK | seq |
| 0x83 | INTEREST | mode; syncSet and mlatSet ICAO add/remove deltas; reduceMs |
| 0x84 | FLOW | {batchMs, pause, slowFactor} |
| 0x85 | MLAT_RESULTS | Batch of {icao, tUtcNs, ecef[3] f64, cov[6] f32, velENU[3] f32?, nBucket (3, 4, 5-8, 9+), grade, flags}. **No station identities.** |
| 0x86 | FEEDBACK (JSON) | Own station only: sync state, anchor state, σ_θ (ns), drift (ppb), outlier %, peer counts by band, location status/delta, warnings |
| 0x87 | CLAIM_CODE | Response to a client `claim` request: an 8-character base32 code, valid 24 h |
| 0x88 | NOTICE | code + text |

### 4.3 Timestamps and clock classes
The feeder **unwraps but never converts** receiver time into anything else. `ticks` is a monotonic u64 per (rx, epoch):
- **12 MHz and 20 MHz:** the raw counter unwrapped. 48-bit wrap for 12 MHz. The 24-bit SBS counter is unwrapped against host time.
- **1 GHz GPS classes:** `dayIndex·86400e9 + nsOfDay`, with `dayIndex` taken from host UTC within ±12 h. The legacy Daysec+1 quirk is fixed by the existing `feed.Timestamper`. Classes 1 and 2 are therefore UTC or GPS-time nanoseconds.

| Code | Class | Hz | Prior jitter σ | Prior freq σ | Anchor |
|---|---|---|---|---|---|
| 0 | none | – | – | – | map only |
| 1 | gps_utc (Radarcape GPS, status bits UTC OK + sync OK) | 1e9 | 30 ns | 1e-9 | after probation (§9.2.5) |
| 2 | gps_gpst (UTC-offset bit clear) | 1e9 | 30 ns | 1e-9 | after k·1 s offset confirmed |
| 3 | gps_emul (timestamp-bad / no lock) | 1e9 | 100 ns | 5 ppm | no |
| 4 | hw12 (Beast, Radarcape 12 MHz) | 12e6 | 83 ns | 5 ppm | no |
| 5 | sdr12 (readsb/dump1090 at 2-2.4 MS/s) | 12e6 | 400 ns | 100 ppm | no |
| 6 | sdr12_hires (airspy_adsb ≥ 12 MS/s) | 12e6 | 150 ns | 20 ppm | no |
| 7 | sbs20 | 20e6 | 500 ns | 100 ppm | no |

**Detection.** The feeder detects the class: Radarcape status frames are authoritative, otherwise the `Timestamper` statistics. The user can override it. The server can downgrade a class from behaviour (§9.2.5), but never upgrades one.

**New epoch.** A new `epochId` plus a `CLOCK_EVENT` is sent on:
- a process restart;
- a step of more than 90 s;
- a backwards step;
- a declared sample drop;
- a mode or class change.

**Frame reference.** The server applies a table `FrameRefNs(class, kind)`:
- Radarcape GPS end-of-frame: short 64 µs, long 120 µs, A/C 20.3 µs. This reuses `latency.FrameStampOffsetNs`.
- hw12 (Beast / Radarcape 12 MHz): one fixed point for both Mode S lengths, 64 µs (A/C 20.3 µs). These 12 MHz offsets come from the observed interop convention (the reference client removes 768 ticks from short and long frames alike), **not** from the GPS table: a length-dependent table would put long frames 56 µs early against short ones.
- sbs20 (SBS-3): end of frame, short 64 µs, long 120 µs, A/C 24 µs (the client's +56 µs short / +96 µs A/C corrections line everything up with the end of a long frame).
- SDR-derived classes: preamble start (0).
- `internal/ingest/mlatc` removes the client's offsets again, so a 31090 tick means what the same receiver's raw stamp means on AVF/1; `TestFrameRefInterop` checks that a short and a long frame of one instant agree after FrameRef for every tick base. simnet's devices follow the same conventions.

The residual short-versus-long error is estimated per station as β_short (§9.2).

**Host time** is used only for grouping, latency stats and day resolution:
- Windows: `GetSystemTimePreciseAsFileTime`, via the existing `internal/ntp/clock_windows.go`.
- Linux: `time.Now`, corrected by `internal/ntp`.

### 4.4 Traffic modes
**`full`** (default while approved stations ≤ `SelectiveAbove`, default 100): every timed frame. About 1.5-3k fps per busy UK station, roughly 15-35 kB/s after compression.

**`selective`**, assigned by the server:
- `syncSet`: 15-30 aircraft per station. All their DF17/18 and DF11 frames are sent.
- `mlatSet`: Mode S-only aircraft plus the hold-out set. All their frames are sent.
- Everything else is reduce-style at `reduceMs = 500`.
- `MINUTE_STATS` keeps counters exact. Result: about 3-5 kB/s.

**FLOW back-pressure.** When a station's server queue stays above 50 % for 5 s, the server sends FLOW. If that persists, it moves the station to selective.

### 4.5 Reconnect, buffering and backfill
**Reconnect.** Backoff 1→60 s with ±20 % jitter, honouring `reconnectIn`. The resume token (memory only, 10-minute lifetime) keeps a station "online" through gaps under 30 s. A signature is still required on every connection.

**Buffering.**
- Always a RAM ring of 10 minutes (about 10 MB).
- An on-disk spool (`SpoolMB`) of 4 MiB segment files, written **only while disconnected**.
  - Default 0 when the root filesystem is on `mmcblk`, which protects SD cards.
  - 256 MB on other Linux hosts and 1024 MB on Windows.

**Backfill.** Live data goes first. Backfill is interleaved at no more than 25 % of the link rate inside `BACKFILL`, with the late bit set. **Late data feeds only:**
- lanes and stats with their original timestamps;
- `flight_station`;
- history.

It never feeds the live map, the clock filter or live MLAT.

**Liveness.** The server drops a session after 60 s of silence. The client reconnects after 30 s with no PING or ACK.

### 4.6 Limits
- 10 s handshake timeout.
- Per IP: at most 4 unauthenticated connections and 16 total. Per /24 (IPv4) or /48 (IPv6): at most 32.
- At most 8 stations per IP unless allow-listed.
- Per station: 6000 fps and 200 kB/s compressed. Above that the server sends FLOW, then disconnects with a 5-minute cool-down.
- A global ingest semaphore. Selective-eligible traffic is shed first.

---

## 5. Compat ingest

### 5.1 Beast on 30004 (`internal/ingest/beastin`)
Uses `feed.BeastParser` with the new escape callback (WP-F):

| Escape | Handling |
|---|---|
| 0xE4 + 36 ASCII | UUID |
| 0xE3 + 8 B | receiver ID; trusted relays only |
| `'W'` + byte | options (`'O'` = ping on) |
| `'P'` + 24-bit ms | ping |
| unknown | skipped |

**Ping.** The parser echoes a client-originated 'P' verbatim. When 'W','O' was received, it also originates its own 'P' every 5 s and measures RTT from the echo. The real direction is confirmed from a capture before WP-B2 merges (open item, verified in phase 1).

**Slowdown.** 'W','S' is sent when the per-connection queue exceeds 50 %.

**Use of timestamps.** The class is inferred by `Timestamper`. 30004 timestamps are used for latency and stats only; **never for MLAT, clock sync or survey**, because beast_reduce thins copies and readsb may re-stamp.

**Location.** None is sent on 30004. The station takes its location from the joined 31090 link, from an owner entry, or stays `unknown`. An `unknown` station still gets stats counters; geometry products start once it is located.

### 5.2 mlat-client protocol v3 on 31090 (`internal/ingest/mlatc`)
Clean-room, written from documented behaviour, and verified by captures of the user's own mlat-client.

**Handshake.**
- Read one space-padded JSON line.
- Fields read: version, client_version, compress, selective_traffic, heartbeat, return_results, return_result_format, udp_transport, uuid, user, lat, lon, alt, clock_type, privacy.
- Reply with:
  - `compress`: zlib2 if offered, otherwise zlib, otherwise none;
  - `udp_transport`: `[host, port, key]` when `aggregator.mlat.udp` is on and the client offers `2` (MLATC.md §9), else `false`;
  - `split_sync:true` by default (option `aggregator.mlatc.splitSync`: `"auto"` (default) offers it with a per-client fallback to sync pairs when a client's single syncs are unusable, `"on"` always, `"off"` never; see the measurement below);
  - `motd`;
  - or `deny` with a reason.

**Timestamp convention (WP-B3, from rule-4 readings; to be re-checked with a capture).** The client reports start-of-frame values: 12 MHz counters −768/−244 ticks, GPS nanoseconds of the UTC day −64/−120/−20.3 µs, SBS +1 120/+1 920 ticks (widened). The server adds the offsets back, so 31090 ticks equal the AVF/1 ticks of the same receiver (`internal/ingest/mlatc/doc.go`).

**`split_sync` measurement (simnet, 10 stations, 60 s, through the real ingest).** Paired sync delivers only 86 % of multi-station sync transmissions complete to the MLAT grouper, because the first half of a pair arrives up to 5 s late; split sync delivers 99.98 %. Mode S frames reach 99.8 % either way. Decision (H-mlatc, integrator after the hardening wave): `split_sync` is offered by default (`"auto"`). Further evidence: a client that pairs overlapping messages sends half the sync traffic with split sync (672 k `ssync` frames against 666 k pairs = 1.33 M frames, half of them repeats); hold-out P95 unchanged within noise on two simulated worlds × three seeds; a client that ignores the flag keeps sending pairs, which are still accepted. A client whose single syncs turn out unusable is disconnected once and answered without `split_sync` (per-identity fallback). Still to confirm against a capture of the reference client.

**Framing.**
- zlib2: 2-byte big-endian length plus a deflate chunk with the sync-flush tail removed. It is rebuilt by re-appending `00 00 FF FF`.
- zlib compresses client→server only.
- JSON lines are capped at 64 KB and decoded strictly.

**Client messages.**

| Message | Mapping |
|---|---|
| `sync` {et, em, ot, om}, `ssync` | Two TOA observations each |
| `mlat` {t, m} | One TOA |
| `seen` / `lost` | Visible ICAO set |
| `rate_report` | Rates used by `mlat/plan` |
| `clock_reset` / `clock_jump` | New epoch |
| `input_connected` / `input_disconnected` | Station state |
| `heartbeat` | Liveness |

**Server messages.**
- `start_sending` / `stop_sending` from `mlat/plan`.
- `result` in the `ecef` format, or the legacy format when that is what was requested.
- `stats` with peer_count, outlier_percent and bad_sync_timeout. **Never a peer list.**
- A heartbeat every 30 s. The session is dropped after 150 s of silence.

**clock_type mapping.**

| clock_type | Class |
|---|---|
| `radarcape_gps` | 1, but anchor status only through behavioural probation (no status frames arrive on this link) |
| `beast`, `radarcape_12mhz` | 4 |
| `dump1090`, `unknown`, `auto` | 5 |
| `sbs` | 7 |

**Timestamp convention.** Whether mlat-client sends raw ticks or pre-offset values for each type is **fixed from captures** in WP-B3. β_short absorbs any residual.

**Altitude** is stored with `altRef = "ambiguous"`. The survey (§10.2) resolves MSL versus HAE.

**Privacy and name.**
- `privacy:true` hides the pin and the name.
- `privacy:false` is treated as consent for a `region` pin and the name, matching community norms. The owner can change both after claiming.
- `user` becomes the display name.

### 5.3 Bridge
`SBSInLocalPort` listens on 127.0.0.1 only. An operator's separate, unmodified mlat-server can point its basestation output there. Positions enter the map as `src=mlat-ext`. AirView never runs, links or embeds mlat-server.

### 5.4 Hub 30154
Unchanged in standalone mode. In aggregator mode it is admin login only. Any client whose first byte is 0x1A, or that sends no valid AUTH within 2 s, is closed quietly, with one log line per IP per hour. This fixes the clash with ADSBx feedclient's results connector.

---

## 6. Ingest, grouper and tracker (station dimension)

### 6.1 Contract additions (`internal/model`, WP0)
```go
type StationIdx uint32 // 0 = local/standalone
// RawFrame gains:
Station StationIdx; Epoch uint32; ClockCls uint8; Ticks uint64; HostNs int64; Late bool
// DecodedUpdate gains: Station StationIdx; Late bool
type StationInfo struct{ Idx StationIdx; SID, Name string; ClockCls uint8; Located bool } // no coordinates
type StationGeo  struct{ Lat, Lon, HAE, SigmaM float64; Source string }                 // private
type StationResolver interface{ Info(StationIdx) (StationInfo, bool); Geo(StationIdx) (StationGeo, bool) }
type Scope uint8 // Public, Owner, Admin
type MLATFix struct{ ICAO uint32; TUnixNs int64; ECEF [3]float64; Cov [6]float32; Vel *[3]float32; NBucket, Grade uint8 }
```
`FeedID` stays. In aggregator mode it is namespaced `st/<sid>/<rx>`.

### 6.2 Grouper (`internal/ingest`)
- **Key:** a 64-bit hash of the payload bytes. For DF20/21 the AP field already contains the address.
- **Entry:**
  - first corrected host time (host time − station offset − station median latency);
  - up to 32 copies of `(station, channel, ticks, sig)`;
  - the cached decode result returned by `tracker.Handle` (ICAO, DF, position if any).
- **Window:** 1.0 s of corrected host time.
- **Same-station repeat rule.** If a timed copy from a station already in the entry differs from that station's first copy by more than 32 µs of *that station's* ticks, it is a new transmission and opens a new entry; within 32 µs (a reply lasts at least 64 µs) a copy from the other channel of the same station is the same transmission. The threshold is 32 µs, not 1 ms: identical Mode S replies only 0.74 ms apart occur (ACAS and DF4/DF20 interrogation pairs). Untimed copies: a copy on the same channel is always a new transmission; one on another channel within 1 ms of host time is the same. This separates repeated DF11, DF4 and identical DF17 frames for every clock class. *(Amended after Wave 1B from the WP-I implementation.)*
- **Synced stations.** When both stations are synced (§9.2), a copy is admitted only if |Δt_net| ≤ d/c + 5σ.
- **First copy:** `tracker.Handle(frame)`.
- **Later copies:** `lanes.Credit(station, ch, sig, cachedResult)`. This **does not take the tracker mutex**.
- **On close:** if the group has ≥ 2 timed copies, or ≥ 1 when it is a sync candidate heard by a synced station, it goes to `mlat.Observe`.
- **Late frames** bypass the grouper and go to `lanes.Backfill`.
- **Standalone mode** keeps today's tracker dedup unchanged.

### 6.3 Lanes (in `internal/tracker`, file `lanes.go`)
- `map[StationIdx]*lane`, sharded 16 ways by index, each shard with its own mutex. Created lazily and evicted after 24 h offline.
- A lane has one `antStats` for the station combined and one per antenna channel. It also keeps **its own** per-minute and per-hour ICAO sets, replacing the per-aircraft uint32 masks that cannot scale.
- **Range, coverage, heat, signal and elevation** are credited only when the station is located, using `StationResolver.Geo`. Counters are always credited.
- **Lane flush** writes to `stations.db` every 10 s, round-robin across stations.
- **Global slot 0** keeps only station-free counters.

### 6.4 Tracker changes (aggregator path only)
- **Range gate:** a position is accepted if it is within 800 km of any located station in the group. The surface CPR reference uses the hearing station.
- **Per-aircraft `heardBy`:** stations from the last 60 s, at most 16, kept off the DTO.
- **Geometry fields** (distKm, bearing, elevation, slantKm, cpaKm) are not stored for aircraft in aggregator mode. The DTO projection computes them for the viewer's selected station (owner or admin) and omits them in public scope.
- **`flight_station(flight_id, sid, first, last, msgs, positions, max_dist_km, min_dist_km, rssi_min, rssi_max, rssi_avg)`** in `airview.db`, written by the lanes when a flight closes. In aggregator mode the station-relative columns of `flights` stay NULL.
- **Throughput:** the tracker sees only unique transmissions (30-60k/s for a large UK network, about 0.6 µs each). An ICAO-sharded tracker is a phase 5 item, gated by profiling.

### 6.5 Per-station engines
- `analytics.Analyzer`, `latency.Analyzer` (GPS-class stations only) and `rfstat`: one per active station, created lazily, capped at an LRU of 32, retired after 30 minutes offline.
- Weather, forecast, met and terrain: host station only by default. For other stations they run on owner request only, with a shared tile cache and an Open-Meteo quota guard.
- Alerts: global rules for the admin; owners get rules scoped to their own stations.

### 6.6 Storage
- **`airview.db`** (existing): aircraft, flights, positions, events, records (merged view), plus `flight_station`, `mlat_minute(t, sid, …)` and `mlat_quality_hour(t, cell1deg, altband, n, p50, p95, p99)`.
- **`stations.db`** (a new `store.Store` instance with its own writer goroutine and **only** the station-relevant migrations):

  | Group | Tables |
  |---|---|
  | Registry | `stations`, `feeders` (pubkey, first_seen, state) |
  | Accounts and access | `owners`, `owner_station`, `claims(code_hash, sid, expires, used)`, `enrol_tokens` |
  | Location | `location_estimates(sid, t, method, lat, lon, hae, σE, σN, σU, n, status)`, `station_public(sid, precision, pub_lat, pub_lon)` |
  | Logs | `audit`, `conn_log(sid, on, off, ip_hmac, proto, bytes)` |
  | Health | `station_minute`, `station_clock_minute` |
  | Existing stats | `stats_minute`, `stats_hour`, `range_hour`, `coverage`, `signal_hist`, `elev_hist`, `heat`, `rfstat_*`, `latency_minute`, `receiver_status`, keyed by `ant='s<idx>'` or `'s<idx>/<ch>'` |

- `analytics.New(store, cfg, WithAntPrefix("s<idx>"))`.
- **Deletion** is a background job that runs a key-range `DELETE` per table, then removes the location, keys and owner links, and pseudonymises the audit entries.
- **Pi:** the SSD is required for a Pi aggregator. On an SD card the UI warns and minute retention drops to 7 days.

---

## 7. Accounts, roles and claiming

| Role | How | Notes |
|---|---|---|
| Public | no session | |
| Feed-check visitor | IP matches a live link (exact IPv4 or IPv6 /64) | Non-locating fields only |
| Owner | username + PBKDF2-SHA256 600k + optional TOTP | No e-mail |
| Admin | password + **mandatory** TOTP (RFC 6238) | Recovery codes; `airview admin reset --data <dir>` break-glass CLI |

**Claiming a native feeder.**
1. `airview-feeder claim`, the local status page or the installer requests a code over AVF.
2. The server mints it: 8 base32 characters (40 bits), valid 24 h, single use, stored as a hash.
3. The owner enters it at `/claim`.

**Claiming a compat feeder.** The visitor must come from the station's current IP **and** enter the full UUID. UUID-less stations need admin approval.

**Rate limits.** Claims: 5 per 10 minutes per IP and 20 per day per account. Logins use the existing throttle.

**Auth fixes in aggregator mode** (standalone is unchanged and covered by tests):
- Loopback is not admin.
- `X-Forwarded-For` is honoured only from `TrustedProxies`.
- Startup refuses aggregator mode until an admin password and TOTP exist. A one-time console token is used to set them.
- Session cookies are `HttpOnly; Secure (when TLS); SameSite=Strict`.
- A CSRF token is required on every mutating request.

---

## 8. Privacy

### 8.1 Principles
- **The operator can always locate a station that sends timestamps.** The consent text says this plainly.
- **Other users and the public cannot.** The design defends against inference, not only against the pin.
- **Exact coordinates exist only in:**
  - `stations` and `location_estimates`;
  - the MLAT engine, which always uses exact positions and never fuzzed ones;
  - owner and admin DTOs.

### 8.2 Visibility matrix (enforced per route; deny by default)

| Data | Public | Owner (own stations) | Admin |
|---|---|---|---|
| Merged live map, trails, aircraft info, MLAT tracks with grade | yes | yes | yes |
| Receivers per aircraft | **nothing** (no count, no list) | `mine` flag + which own stations heard it | full list |
| Station pin | hidden (default) or stored fuzzed point | exact | exact |
| Name | opt-in | yes | yes |
| Stats: rate, uptime %, aircraft/day, positions, MLAT contribution count | opt-in, non-locating fields only | all | all |
| Coverage, polar, range outline, heat, records with bearing/position, RSSI vs range, min-altitude floor, CPA, antenna lab, terrain, Locate Me, survey | never | yes | yes |
| "My map" (aircraft my stations heard) | never | session only (share links in phase 4) | any |
| Uptime timeline | never | yes | yes |
| Sync peers | public sync graph only between opt-in stations at their stored points, unweighted edges | per peer: unlinkable tag `HMAC(key, viewerSID‖peerSID‖ISOweek)[:6]`, distance band (<50, 50-150, >150 km) computed from the viewer's position frozen at its first request of the ISO week, sync rate, rms ns. No bearing, no position, no per-peer frequency; the own drift is shown in whole ppm (PRIV-1/PRIV-2). The owner view is rate-limited per account. | full matrix |
| Clock state, residuals | never | own σ_θ, drift, β_short, outliers, residual bias | full |
| CRLB "expected accuracy" map | never | never | yes |
| Empirical MLAT quality | network P50/P95/P99 plus 1° cells with ≥ 5 contributing stations | same | full maps |
| Network coverage union | yes; cells below 3000 ft AGL suppressed where fewer than 3 stations contribute | full own | full |
| IP | never | own current IP | yes, audited |

### 8.3 Public point algorithm
1. Precision options: `hidden` (default), `region` (0.5° cell) or `cell01` (0.1° cell).
2. The point = cell origin + a uniform point inside the cell, derived from `HMAC-SHA256(HKDF(serverKey, "pin"), sid)`.
3. It is computed once, stored in `station_public` and recomputed only when the station moves to another cell.
4. Every public surface uses only this stored point.
5. **Rural guard:** if fewer than 3 stations fall within the surrounding 1° cell, `cell01` is shown as `region`.

### 8.4 Enforcement
- **Separate DTO types.** `internal/privacy/publicdto` types have **no** fields for station coordinates, distance, bearing, receiver lists, IPs or raw timestamps.
- **Route scopes.** Every route in `routes.go` declares a `Scope` and a station resolver. A lint test fails if any route lacks one. The response cache key includes `(scope, ownerID, sid)`.
- **Legacy `/api/*` in aggregator mode** maps to public equivalents or returns 403.
- **Golden tests.** They crawl every public route, the SSE stream and the `/data/*.json` endpoints over fixtures with known exact coordinates, IPs, SIDs and ticks. They fail on:
  - any fixture coordinate to 3 decimals;
  - any IP;
  - any SID of a hidden station;
  - any ns-precision timestamp;
  - the keys `seenBy`, `feeds`, `distKm`, `bearing` or `recentReceiverIds`.
- **tar1090 JSON.**
  - `/data/receiver.json` has no position, or the network centroid when `TarJSON` is on.
  - `/data/aircraft.json` has no receiver fields.
  - Owners get session-scoped `/data/me/aircraft.json`.
- **MLAT results to feeders** are sent only when that station contributed to the aircraft's track in the last 60 s **and** sent at least 10 timed frames per minute while synced. The station count is bucketed, with no identities.
- **Logs.** No per-station TOAs, positions or raw IPs are written. `conn_log` stores HMAC(IP) with a daily-rotating salt.
- **Rate limits.** Public API: 1 req/s per IP, burst 10. One public SSE stream per IP.

### 8.5 Retention defaults (published in the privacy notice)

| Data | Retention |
|---|---|
| Raw IP | memory only |
| `conn_log` | 30 days |
| Minute stats | 30 days |
| Hourly stats | 13 months |
| Location estimates | latest 5 per method |
| Unclaimed and offline | purged after 90 days |
| Owner delete | immediate; backups expire within 30 days |
| Audit log | 24 months |

Owners can export their data (JSON zip) and delete it.

Templates are shipped in `docs/aggregator/`:
- a privacy notice (controller, data, matrix, retention, rights, the ICO route, residual risks: operator knowledge and coverage islands);
- an LIA (the lawful basis for MLAT is legitimate interests; consent covers public display);
- a feeder consent text.

### 8.6 Residual risk: coverage islands
Low-altitude aircraft heard only by one station reveal that station's area on the merged map. `IslandGuard` (default **off**) shows such aircraft below 1000 ft AGL, or on the ground, at 0.05° rounding when the only receivers are hidden-pin stations. The notice documents the risk whether or not the guard is on.

---

## 9. MLAT engine (`internal/mlat/*`)

### 9.1 Packages

| Package | Responsibility |
|---|---|
| `mlat/geo` | WGS-84 ↔ ECEF (closed-form inverse), ENU, refraction excess (exponential N(h), N₀ from METAR else 315, H = 7 km, scale k_atm), 4/3-earth line-of-sight check. Moves `ecef` and `propagationNs` out of `latency`; `latency` re-imports them. |
| `internal/geoid` | Embedded EGM96 1° global grid (≈130 KB, public domain), bilinear interpolation |
| `mlat/linalg` | Dense Cholesky, solve, inverse up to n ≈ 600; fixed 4×4, 6×6 and 9×9 helpers; sparse block accumulator |
| `mlat/ref` | Reference tracks from ADS-B (uses `internal/modes`): CPR positions, velocity, NACp/NIC, T=1 transmit-instant correction (from `latency`), GNSS−baro field |
| `mlat/clock` | Joint clock filter (§9.2) |
| `mlat/group` | Network-time clustering (§9.3) |
| `mlat/solve` | Initialisation, LM, robust stage, RAIM, CRLB (§9.4) |
| `mlat/track` | IMM raw-TOA tracker (§9.5) |
| `mlat/quality` | Hold-out, error maps, σ learning, CRLB map, k_atm regression (§9.6) |
| `mlat/survey` | Reverse MLAT, bundle adjustment, declared-position check (§10) |
| `mlat/plan` | syncSet and mlatSet per station; feeds AVF INTEREST and mlatc start/stop |
| `mlat` (root) | `Engine`: `Observe(Group)`, `Fixes() <-chan MLATFix`, result routing, admin/owner stats DTOs |

### 9.2 Clock synchronisation

**9.2.1 Model.**

Local seconds for station i:

  sᵢ = ticks/f_nom − FrameRef(class, kind)

The measurement model for a transmission at network time τ from position p:

  sᵢ = τ + ρᵢ/c + θᵢ(τ) + β_shortᵢ·[kind = short] + nᵢ

where ρᵢ = |p − rᵢ| + refraction excess(p, rᵢ).

**States per station** (non-anchor):
- θ: offset, in s;
- φ: fractional frequency;
- ψ: frequency rate; only for classes 5 and 7, to model thermal wander;
- β_short: random walk, 1 ns/√h; prior ±1 µs for classes 4-7 and ±50 ns for GPS classes.

Dynamics: θ̇ = φ, φ̇ = ψ (or white noise), ψ̇ = white noise. Process noise uses the standard two- or three-state clock Q, seeded from the class table and adapted online by covariance matching over 10-minute windows, bounded to ×0.25…×10 of the prior.

**Anchors:** θᵢ = δᵢ (random walk, 1 ns/√h) and φ ≡ 0.

**Gauge.**
- A component with anchors: soft constraint mean(δ_anchor) = 0, σ 10 ns. Network time is UTC.
- A component without anchors: soft Σθ = 0 (σ 1 ms), Σφ = 0 (σ 1 ppm). The component is labelled "relative time".

**9.2.2 Sync observations.**

Any group from an aircraft whose reference track has all of:
- NACp ≥ 8 (prefer ≥ 9);
- NIC ≥ 7;
- a valid CRC;
- a position age ≤ 1 s once propagated to the transmit instant.

This covers every DF17/18 frame and **also DF11/DF0/4/5 frames from the same aircraft**; the short frames make β_short observable. For non-position frames, p comes from the reference track with σ grown by σ_v·age + ½·a_max·age².

The transmit time τ is eliminated by differencing against the group member with the lowest σ. The innovation covariance is:

  R = σ_ref²·11ᵀ + diag(σᵢ²) + Gᵀ Σ_pos G / c²

where G has rows (uᵢ − u_ref) and Σ_pos comes from NACp. The H rows include the (t − t_epoch) terms for φ and ψ.

**9.2.3 Estimation (covariance form, per connected component).**
- Components are found by union-find over co-observation counts in the last 60 s.
- Epoch = 0.5 s. Prediction uses block-diagonal F and Q (O(n²)).
- Over the epoch, accumulate J = Σ HᵀR⁻¹H and g = Σ HᵀR⁻¹ν, where innovations ν are computed against the predicted state at each observation time.
- Update:

  P⁺ = (P⁻⁻¹ + J)⁻¹, using Cholesky
  x⁺ = x⁻ + P⁺ g

- One O(n³) cycle per epoch. Dense up to 200 stations per component (n ≤ 700). **Benchmark gate: ≤ 80 ms per epoch at 200 stations on one VPS core.** Partitioning is phase 5.
- Publish an immutable `Snapshot` via `atomic.Pointer`: per-station states, the marginal covariance blocks, cross-covariances for synced pairs, the component ID and flags.

**9.2.4 Integrity.**
- NIS per group: χ²(m−1) at 99.9 %. On failure, drop the member with the largest studentised innovation and retest.
- Per-station and per-aircraft bad-sync EWMAs. An aircraft biased across ≥ 3 station pairs is blacklisted from sync for 10 minutes; this catches bad NACp and spoofing.
- **Step detection:** 5 consecutive same-sign innovations above 4σ, or a `CLOCK_EVENT`, resets var(θ) to (1 ms)². φ is kept unless the reason is `restart`.
- **Influence cap:** a station's information contribution per epoch is limited to 3× the component median.
- **Synced** means marginal σ_θ < 300 ns (classes 5-7), < 100 ns (classes 3-4) or < 50 ns (GPS), with ≥ 5 accepted observations in the last 30 s.

**9.2.5 Anchor probation.** Classes 1 and 2 are free clocks while on probation. A station becomes an anchor when all of these hold:
- **(a)** status bits show GPS lock, UTC OK and sync OK for ≥ 10 minutes. mlatc `radarcape_gps` stations have no status bits and skip (a).
- **(b)** after 10 minutes as a free clock, |φ| < 2e-9 and |θ − median anchor offset| < 200 ns.
- **(c)** the admin has not flagged it (registry flag `neverAnchor`, admin API `neverAnchor`, or a negative trust).

**Server-time check.** Condition (a) is the feeder's own word. The filter also compares every GPS station's clock with the server's corrected time of its sync groups (EWMA of station time − earliest copy's antenna time in the server clock domain, ≥ 20 samples). A station more than 0.4 s off (`AnchorHostMaxS`) can be neither the first nor a self-asserted anchor (finding ROB-1: one fake-GPS feeder used to set network time by whole seconds).

The **first anchor** of an empty component is admitted on (a) plus the server-time check and labelled "self-asserted". It is revalidated automatically once a second anchor exists. A self-asserted anchor is demoted when its own server-time check fails, or when status-bearing candidates that pass the check disagree with it (by k·1 s or by more than 2 × 200 ns).

**Class 2:** a θ offset of exactly k·1 s (±1 µs) against validated (not self-asserted) UTC anchors is accepted as a leap or legacy offset, applied, and the class becomes 1, but only when the leap brings the station to within 0.4 s of server time. Against a self-asserted anchor no leap is applied.

**Demotion:** immediately when the status bits are lost (existing `GPSLocked()`); otherwise when |dδ/dt| > 100 ns/h or φ drift exceeds 1e-8 for 10 minutes. A demoted station falls back to class 3 behaviour. A demotion for a status loss or a receiver epoch event is transient: once (a) holds again for 10 minutes the station's declared class is restored and it re-enters probation (finding ROB-5). Drift, revalidation, step and flag demotions are permanent until the class changes.

**Blacklist.** An aircraft whose sync groups keep failing (≥ 10 groups, > 50 % failed) is blacklisted from sync for 10 minutes. A group counts as failed only when ≥ 4 healthy members took part and ≥ 2 healthy members were dropped (or it failed outright); members whose outlier EWMA is ≥ 0.25 are not healthy and do not count. At most 20 % of the sync aircraft are blacklisted; beyond that the fault is in stations and the filter counts `BlacklistCapped` instead (finding ROB-2).

**9.2.6 Slow terms.**
- **k_atm:** an hourly robust regression of anchor-pair sync residuals against the elevation-dependent excess. Bounded to 0.8…1.2. Lives in `mlat/quality` and is fed back to `geo`.
- **Airframe latency bias** (phase 4, default off until the hold-out shows a gain): a per-ICAO scalar from anchor-pair residuals, estimated by RLS.

### 9.3 Grouping and clustering (`mlat/group`)

**Candidates:**
- all Mode S frames of aircraft without a good ADS-B position in the last 10 s;
- the hold-out set;
- DF17 aircraft whose positions fail TDOA verification.

**Flush:** when the p99 corrected host latency of the stations that usually hear this ICAO has elapsed, clamped to 150-600 ms. Late copies (up to 2 s) still update the track.

**Clustering.**
1. Convert each copy to network time using the snapshot, with variance.
2. Sort and split at gaps over 2 ms.
3. Grow a cluster from the newest copy, admitting k only if |tₖ − tᵢ| ≤ (dᵢₖ + 1 km)/c + 4σᵢₖ for every member i.

**Co-located stations** (under 1 km apart) are fused into one measurement and counted once for degrees of freedom.

**Minimum to solve:** 4 independent stations, or 3 + altitude. 2-station clusters go to the tracker only.

### 9.4 Solver (`mlat/solve`)
**Unknowns:** ENU position about the seed, plus the transmit time.

**Weighting:** W = Σ⁻¹, where **Σ is the full m×m covariance** of the network-time TOAs: the clock covariance blocks from the snapshot plus diag(jitterᵢ²). Altitude is an extra row: Mode S altitude converted to HAE through the GNSS−baro field (§9.6), with σ_h = 15 m or the field cell's σ if larger (fallback: height-dependent, §9.6), inflated by altitude age × |vertical rate|.

**Initialisation**, in order:
1. track prediction, if the track's σ < 2 km;
2. linear TOA weighted least squares (N ≥ 5);
3. Bancroft (N = 4);
4. N = 3 + altitude: a 5 km grid at the known height over ±400 km, then LM from the best 3 minima, keeping 2 hypotheses;
5. receiver centroid at baro height.

**Optimiser:** LM, at most 10 iterations, analytic Jacobian, zero allocations.

**Robust stage:**
- Huber IRLS, k = 1.345.
- RAIM: weighted SSE against χ²(N + n_alt − 4) at p = 1e-3. On failure, exclude the largest studentised residual and re-solve while DOF ≥ 1.
- RANSAC when N ≥ 6 and RAIM has failed twice.

**Covariance:** CRLB (HᵀWH)⁻¹ × max(1, χ²/DOF), in ENU.

**Grades:**

| Grade | Horizontal σ | Condition |
|---|---|---|
| A | ≤ 50 m | DOF ≥ 2 |
| B | ≤ 150 m | |
| C | ≤ 500 m | |
| D | ≤ 2 km | track maintenance only, never displayed |

**Reject a fix if any of these hold:**
- σ_h > 2 km;
- altitude disagreement > 4σ;
- outside the radio horizon of any member;
- implied speed versus the track > max(600 kt, Mach-aware limit);
- τ inconsistent with first-arrival host time by more than 50 ms.

### 9.5 Tracker (`mlat/track`)
**Model:** IMM with constant velocity plus coordinated turn in a re-centred ENU frame, and a separate vertical channel.

**Measurements:**
- raw TOA clusters with m ≥ 2, as an (m−1)-dimensional TDOA update against the reference member plus the altitude row. 2-station updates **maintain** confirmed tracks only;
- full fixes;
- DF17 TC19 velocity;
- BDS 5,0 track and ground speed, and BDS 6,0 heading/IAS/Mach (via `internal/modes` inference) with met wind and inflated σ;
- Mode S altitude.

**Gating:** Mahalanobis at 99.9 %.

**Lifecycle:**
- Initiation: 2 consistent fixes within 10 s.
- Coast up to 60 s.
- Drop when σ_pos > 5 km.
- 3+alt ambiguities are resolved by gating.

**Output:** 1 Hz or on update, as `DecodedUpdate{MLAT:true, Src: mlat-av}` into the global tracker, with σ for map ellipses. The existing RTS smoother is used for trails and replay.

**Concurrency:** tracks are sharded by ICAO across GOMAXPROCS goroutines. The clock snapshot is read lock-free.

### 9.6 Quality (`mlat/quality`)
**Hold-out.** 10 % of ADS-B aircraft with NACp ≥ 9, chosen by hash and rotated every 10 minutes. They are excluded from sync and from any latency calibration, solved as Mode S-only, and compared with their latency-corrected ADS-B positions.

**Outputs:**
- network P50/P95/P99 error;
- per 1° cell × altitude band;
- calibration: the fraction of errors within 1σ and 2σ;
- per-station residual bias and outlier rate.

**Learning:**
- Per-station jitter σᵢ is learned from robust residual variance, bounded to ×0.5…×4 of the class prior.
- A station with residual bias > 3σ for 1 h is flagged and gets a survey.

**GNSS−baro field.**
- Built from DF17 TC19 GNSS-difference fields and TC20-22 GNSS heights.
- Cells of 50 km × 2000 ft, with a scalar Kalman update per cell (1 h time constant).
- Fallback: METAR QNH and temperature with the ISA relation, plus the geoid. Its σ grows with height: σ² = σ_base² + (h_p·σ_T/T_mean(h_p))², σ_base 60 m with surface weather (150 m without), σ_T 6 K with surface temperature (11 K without): about 90 m at FL100 and 290 m at FL400 with weather, 190 m and 540 m without. The 60-100 m of §9.4 hold only at low levels. Cells are initialised and relaxed to the same σ.

**CRLB map** (admin only): expected σ_h at 3000 ft, FL100 and FL300 on a 0.25° grid, recomputed when stations change.

**ADS-B verification.** A DF17 position contradicted by TDOA from ≥ 2 synced stations is flagged `adsbSuspect` (admin view). Positions confirmed by ≥ 3 timed stations are flagged `verified`.

**Alarm:** the admin is alerted when P95 regresses more than 25 % week on week.

### 9.7 Policy
- Any frame flagged MLAT by its source (magic timestamp or SBS MLAT flag) is excluded from tracking, sync and solving.
- Results are always marked MLAT and are never forwarded upstream.

### 9.8 Targets (gates in §13; live numbers come from the hold-out, not from claims)

| Network | Target P50 / P95 horizontal | Latency |
|---|---|---|
| ≥ 4 GPS anchors, good geometry | 15-30 m / 80 m | p95 ≤ 0.6 s |
| Mixed anchors + SDR | 30-100 m / 250 m | |
| SDR only | 80-200 m / 500 m | |
| mlat-server reference | tens to hundreds of m | 0.9-2.5 s |

### 9.9 Mode A/C MLAT
Phase 5, off by default. Only among anchors within 150 km, using association-free clustering on (code, time) with |Δt| ≤ d/c + 2 µs.

---

## 10. Self-location

### 10.1 Resolution order and state machine
States: `unknown → estimating → estimated(σ) → confirmed`, plus `declared`, `gnss` and `suspect`.

| Source | Accuracy | Notes |
|---|---|---|
| GNSS (Radarcape type '5', `ShareGNSS`, or the host station) | 2-5 m | Auto-confirmed |
| Declared | as stated | Used immediately; always cross-checked by the survey |
| Reverse-MLAT survey | 10-25 m GPS-timed; 50-500 m 12 MHz | |
| Locate Me v2 | 2-10 km | Coarse prior only; never used for MLAT geometry |

**Before confirmation**, a station takes part in clock sync **only as a survey target**. It is never used in the solver geometry, and in the filter it carries an inflated position σ.

**Confirmation:**
- the owner clicks "Use this location"; or
- with admin `AutoUseSurvey` on (default off), when σ_h < 50 m for GPS classes or < 200 m otherwise. The station is then badged "estimated".

### 10.2 Reverse MLAT (`mlat/survey`)
**Inputs:** the target U's timed frames (AVF, or 31090 frames the planner requests from U's visible set), plus the same transmissions heard by synced, located stations, nearest anchors first.

**Pseudorange equation:**

  ρ = c·(s_U − t_j,net) + |p − a_j| = |p − u| + c·θ_U + c·φ_U·Δt

with clock knots every 30-60 s for classes 5-7.

**Height prior:** h = DEM(lat, lon) + AGL (if declared, otherwise 10 m), σ 20 m. This breaks the vertical/clock coupling.

**Seed:** Bancroft, or a ±50 km grid at 1 km seeded by the declared position, Locate Me v2, or the centroid of the stations U co-observes with.

**Refinement:** robust Gauss-Newton (Huber).
- Elevation mask 2-3°, with the same refraction model at both ends.
- Positions are corrected to the transmit instant.
- All channels are used together. The UI shows the anisotropic ellipse, because a yagi-only sector inflates the along-boresight error.

**Stable** means σ_h < 30 m (GPS) or < 200 m (other) **and** an azimuth spread > 180°. The result is stored with its 95 % ellipse.

**Altitude-reference test** for `altRef = ambiguous` and declared MSL stations: solve with the declared height as MSL and as HAE, and keep the interpretation whose residual is lower by more than 3σ. The owner is told the result.

### 10.3 Declared-position check and bundle adjustment
- **Every 6 h**, survey each declared station. If |declared − surveyed| > max(100 m, 3σ), mark the station `suspect`:
  - its σ is inflated by the discrepancy, which effectively excludes it from the solver;
  - the owner gets a notice;
  - the admin sees the solution.
- **Phase 4:** an hourly network bundle adjustment solves all unlocated stations jointly. ADS-B positions fix the frame, so even two unlocated peers are observable.

### 10.4 Locate Me v2 (standalone; `internal/analytics/locate2*.go`)
- **New bounded table `guess_samples`** in the station's store: at most one sample per aircraft per 10 s, 7 days, up to 2 M rows. Each sample holds t, icao, lat, lon, alt, rssi, ch, onGround. It is written through a tracker hook.
- **Likelihoods:**
  - per-aircraft-session RSSI offsets plus a low-order antenna gain G(az, el), using samples < 120 km;
  - a DEM-panorama horizon likelihood (`internal/terrain`), searched at 2 km then 200 m;
  - airport line-of-sight discs from surface messages and first/last heard altitudes near airports.
- **Fusion:** a grid posterior that multiplies the likelihoods. `guess.go` is kept for comparison.
- **Scoring:** automatic, against the Radarcape GNSS position (`ErrorKm`).

Standalone timing-only self-location is provably unobservable and is not built.

### 10.5 Consent text (installers, claim page, privacy notice)
> "If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this."

`--no-timestamps` sets `Timestamps=false`. The feeder then sends DECODED or untimed frames only.

---

## 11. User-facing views

**One map** (`pages/map.js`) with a **View** selector:

| View | Who | Shows |
|---|---|---|
| Network | everyone | Merged aircraft; MLAT aircraft with σ ellipse and grade; opt-in public pins; public quality summary |
| My stations | owner | "Heard by me" highlight and an "only mine" filter; own exact markers, range outline and polar; distance/bearing/elevation columns for the selected own station |
| Station X | owner of X, or admin | Full standalone-style map centred on X |
| Admin | admin | All stations exact; per-aircraft receiver list; sync graph coloured by σ; residual inspector; CRLB and hold-out layers |

**UI plumbing:** `ctx.station = {sid, name, scope, located, lat?, lon?, antennas}` plus a station picker in the shell. `api.js` appends `station=`. Standalone mode sets `ctx.station` to the local station, so existing pages work unchanged.

**`/me/check` (Feed Check, IP match):**
- for each link (AVF, Beast, MLAT): protocol, last 6 characters of the UUID, UUID-mismatch and null-UUID warnings, connected since, msg/s, pos/s, kbps, RTT, clock class, MLAT synced yes/no;
- a "Claim" button.

**`/me/stations/<sid>`:**

| Section | Contents |
|---|---|
| Connection | timeline, protocol, version, RTT p50/p95, kbps, fps by DF, late frames, spool depth, host offset |
| Reception | the existing stats, antenna, range, polar, heat, records, signal and elevation pages, fed from the station's namespace; "aircraft only you heard (24 h)" |
| Timing | class, anchor state with reason, σ_θ, drift chart, β_short, clock events, peers (unlinkable tags) |
| MLAT | contributions/h, used versus excluded %, residual bias, outlier % |
| Location | declared / GNSS / surveyed with ellipse, confirm/adjust, the public pin preview exactly as others see it |
| Privacy & data | pin precision, public stats, name, export, delete, unclaim |

**Admin console:** station registry (approve, revoke, trust, rename, audited exact-location reveal), enrol tokens, MLAT console (components, anchors, epoch timing, sync matrix), quality dashboards, CRLB, ingest load, retention jobs.

**Outputs for other tools:** a merged Beast-out TCP listener (MLAT uses the magic timestamp) and an SBS-out listener. Both are admin-configurable and token-protected by default.

---

## 12. Feeder programs

### 12.1 `cmd/airview-feeder` (linux arm64/armv7/amd64, windows amd64/arm64)
- **Shared code:**
  - `internal/feed` inputs and `Timestamper`;
  - `internal/ntp`;
  - `internal/uplink`;
  - `internal/feederui` (status and settings on 127.0.0.1:8099; LAN binding opt-in with a password);
  - `internal/selfupdate`.
- **Subcommands:** `run`, `claim`, `status`, `config set k=v`, `new-identity`, `update`, `service install|remove` (Windows), `tray` (Windows), `--version`.
- **Inputs:**
  - auto-detect 127.0.0.1:30005 (readsb/dump1090), sdr12 by default;
  - a Radarcape on :10003 (status frames are authoritative; `systemstatus.json` is a hint);
  - explicit `beast`, `avr` and `sbs` entries;
  - several receivers, each its own `rx_no`.
- **Local decode** with `internal/modes` is used only for `MINUTE_STATS`, the selective filter and the status page. No SQLite.
- **MLAT results:** listen on Beast **30105** with the magic timestamp, optionally connect to local readsb 30104. Results are never re-uploaded.
- **Identity files:** `identity.key` (0600) and `uuid` (UUIDv4, for compat links and support).
- **Budget:** binary < 15 MB, RSS < 40 MB. ARMv6 is not supported.
- **Phase 5 "also feed others":** a clean-room `beast_reduce_plus_out` client and an mlat-client-protocol client, with the destination list in config.

### 12.2 Raspberry Pi installer (`deploy/feeder/pi/`)
```
curl -fsSL https://<aggregator>/feeder/install.sh -o /tmp/avf.sh && sudo bash /tmp/avf.sh
sudo bash /tmp/avf.sh --server host:30160 --pin sha256/... --enrol TOKEN --name NAME \
  --lat .. --lon .. --alt 120m|400ft --agl 8m --input auto|host:port \
  --privacy hidden|region|cell01 [--no-location] [--no-timestamps] --non-interactive
```
The aggregator serves `install.sh` with `--server` and `--pin` pre-filled.

1. **Pre-checks:**
   - root and systemd;
   - architecture aarch64, armv7l or x86_64;
   - free disk space;
   - `timedatectl` NTP state, offering chrony;
   - warn if `/etc/default/adsbexchange` sends results to 127.0.0.1:30154.
2. **Detect the receiver.** The installer never removes or reconfigures anything:
   - a listener on 30005 is reused;
   - a Radarcape (hostname, `rcd` process, or a user-entered IP) uses :10003;
   - an RTL-SDR (0bda:2838/2832) with no decoder: offer to run upstream readsb-install as a separate GPL program;
   - Airspy or SDRplay: point to their decoders;
   - an FTDI Beast (0403:6001): serial input.
3. **whiptail prompts:**
   - server and enrol token;
   - name, validated to `[A-Za-z0-9_-]{1,32}`;
   - lat and lon with range checks;
   - altitude with a mandatory `m`/`ft` suffix, converted correctly for both units; reference MSL (default) or HAE; antenna AGL;
   - "leave blank, let the network locate me", which shows the consent text;
   - privacy level;
   - no-timestamps option.
4. **Download and verify:** fetch the binary from the aggregator's `/feeder/dl/<arch>` and check it against the Ed25519-signed manifest.
5. **Install:**
   - user `airview-feeder`;
   - `/opt/airview-feeder`, `/etc/airview-feeder/feeder.json` (0640), `/var/lib/airview-feeder`;
   - unit with `Restart=always`, `RestartSec=5`, `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `ReadWritePaths=/var/lib/airview-feeder`, empty `AmbientCapabilities`, `MemoryMax=128M`;
   - `airview-feeder-update.timer`, daily with a random delay.
6. **Finish:**
   - print the local status URL, the aggregator `/me` URL, a **claim code**, the journalctl command, and the update and uninstall commands;
   - a post-check confirms the session is established.
7. **Re-running** reconfigures with the current values as defaults. `uninstall.sh [--purge]` never touches readsb or other feeders.

The aggregator's `/feeder` page also shows the two-line readsb/Ultrafeeder snippets (30004 `beast_reduce_plus_out,uuid=…` and mlat-client to 31090) for people who prefer their existing stack.

### 12.3 Windows feeder (`deploy/feeder/windows/`)
- **Windows service:** `airview-feeder.exe` via `x/sys/windows/svc` and `svc/mgr`. It runs as `NT SERVICE\AirViewFeeder`, with `%ProgramData%\AirView Feeder` ACL-limited.
- **Tray:** `airview-feeder.exe tray`, a per-user autostart process. It uses raw `Shell_NotifyIconW`, `CreateWindowExW` and `TrackPopupMenu` through `x/sys/windows` (`internal/wintray`, no new modules).
  - Menu: Open status, Claim code, Pause, Restart service, Check for updates, Quit tray.
  - The page opens in the default browser, or in AirView's WebView2 if the main app is installed.
- **Installer:** an Inno Setup script for a per-machine install. It registers the service and the tray autostart, and adds a firewall rule only if the LAN UI is enabled. A portable ZIP is also offered. Authenticode signing is used if a certificate exists.
- **Self-update:** download, verify, rename the running EXE to `.old`, place the new one, restart, roll back if the health check fails within 60 s.
- **Inputs:** network Radarcape/Beast/AVR/SBS, another AirView over the hub, dump1090-win, airspy_adsb, and VRS or ModeSMixer2 rebroadcasts (SBS, map only).
- **Phase 5:** RTL-SDR through a user-installed `rtl_tcp.exe` child process (GPL, separate) with a clean-room Go demodulator (`internal/demod`). Timestamps are sample index × 12e6/fs; gap detection triggers `CLOCK_EVENT`; the UI warns that MLAT may be unreliable.

### 12.4 Main AirView
It stays the standalone station and gains an **Uplinks** settings section, so the user's EVO 2CH can feed the club while keeping its full local UI. Aggregator mode lives in the same binary.

---

## 13. Test strategy

### 13.1 `internal/sim/simnet`
N receivers (up to 500) driven by the existing world model, extended with:
- a configurable fraction of Mode S-only aircraft;
- DF4/20 interrogation bursts and DF11 squitters;
- per-airframe position latency of 0-300 ms;
- NACp.

**Receiver models:** declared position error (0-5 km), antenna and horizon, and reception probability.

**Clock models:**
- GPS anchor: PPS jitter 15-45 ns, cable bias 50-300 ns, lock loss, a +18 s boot;
- hw12: 5 ppm random walk;
- sdr12: 2.4 MS/s quantisation, 300-500 ns jitter, 30-100 ppm, 0.01 ppm/s thermal ramps, silent and declared sample drops;
- sbs20: 24-bit wrap.

Every class gets a configurable **short-versus-long reference offset error**.

**Network:** latency distributions, jitter, disconnects, reordering within batches.

**Outputs:** AVF, Beast-30004 and mlatc streams, plus a truth log.

### 13.2 CI gates (fixed seeds)

| Test | Gate |
|---|---|
| 8 GPS anchors, 150 km | P50 < 20 m, P95 < 60 m; 1σ calibration 60-75 % |
| 4 anchors + 20 SDR | P50 < 80 m, P95 < 250 m; ≥ 90 % of Mode S-only aircraft in the 4-station footprint get fixes |
| 20 SDR only | P50 < 200 m |
| 2 µs short/long error on 5 SDR stations | β_short converges within 100 ns in 10 minutes; short-frame fix bias < 30 m |
| DF11 repeat every 0.8 s, 100 stations | Grouper unique count = truth ± 0.1 % |
| CRLB vs Monte Carlo (1000 runs) | within ±20 % |
| 3+alt ambiguity | correct branch > 95 % within 10 s |
| Silent 22 µs step | detected < 5 s, re-synced < 30 s, no fix error > 3σ during recovery |
| False GPS claim / 1 µs anchor fault | demoted < 10 min; network-time shift < 20 ns |
| Declared 300 m off | `suspect` within 15 minutes |
| MSL/HAE ambiguity | correct reference chosen in 30 minutes |
| Survey | GPS target < 25 m in 5 minutes; SDR target < 300 m in 30 minutes |
| Latency | result p95 < 0.6 s at 50 ms network jitter |
| Clock epoch, 200 stations | ≤ 80 ms |
| Grouper / Credit / solver / AVF encode | ≤ 300 ns per copy with 0 allocs / ≤ 120 ns / ≤ 20 µs per 6-station cluster / ≤ 400 ns per frame |
| Privacy | golden crawl clean; route-scope lint passes; the hidden-station SID never appears |
| Protocol | fuzz targets for AVF records, the Beast escape parser, mlatc JSON and zlib2; spool replay is idempotent; resume after a 30-minute outage |
| Load | 500 selective stations + 1000 public SSE clients on 4 vCPU: CPU < 60 %, RSS < 4 GB, no queue drops |

### 13.3 Interop and live checks
- **Fixtures:** binary captures from the **user's own** readsb (`beast_reduce_plus_out`), mlat-client (every `clock_type` the user can produce) and Radarcape, replayed against `beastin` and `mlatc`.
- **`-tags interop` suite:** upstream readsb and mlat-client run as separate processes in a VM.
- **Live:** the hold-out dashboard. Locate Me v2 and the survey are scored against the Radarcape GNSS position, including a "pretend unlocated" run.
- **Installers:** shellcheck; a Debian bookworm/trixie arm64 container running `install.sh --non-interactive` against a simnet aggregator; a Windows portable smoke test plus a manual checklist.

---

## 14. Phased plan with disjoint ownership

**Rules**
- **One owner per directory per wave.** `internal/model` is changed only in WP0 and frozen at the end of phase 0. Any later change is requested from the integrator, who batches them between waves.
- **No direct edits across packages.** A WP that needs a hook in another package files the request in `docs/aggregator/HOOKS.md`. The owning WP in the next wave implements it.
- **Every WP ends green** on `go build ./...`, `go vet` and `go test` for its packages, plus its listed acceptance tests.
- **Starting point.** Phase 0 starts from the tree as it is when the workflow currently editing `D:\AirView` finishes. WP0 first re-reads `model`, `feed`, `tracker` and `server` to rebase these contracts.

**Sizes:** aw = agent-weeks.

### Phase 0: contracts and foundations (≈ 2.5 weeks calendar)

| WP | Owns | Work | Acceptance | aw |
|---|---|---|---|---|
| WP0 Contract | `internal/model/**`, `docs/aggregator/AVF.md` | §6.1 types, config structs §1, clock-class enum, Scope, MLATFix; normative AVF spec | Compiles; doc comments complete; standalone tests unchanged | 1 |
| WP-S Simnet | `internal/sim/simnet/**` (new) | §13.1 world-to-N-receivers, clock models, stream emitters, truth API | Deterministic replay; emits valid Beast parsed by `feed.BeastParser` | 2 |
| WP-G Geo/linalg/geoid | `internal/mlat/geo`, `internal/mlat/linalg`, `internal/geoid` | §9.1 primitives; refraction vs numeric ray trace; EGM96 1° grid | Refraction < 1 m at 400 km; geoid within 2 m of reference points; Cholesky benchmarks | 1.5 |
| WP-F Feed surfacing | `internal/feed/**` | Escape callback (0xE3/0xE4/'P'/'W'); Timestamper exposes class, epoch and unwrapped ticks; CLOCK_EVENT reasons; `noForward` | Fuzz the parser; legacy-day and GPS-time-base unit tests | 1 |
| WP-A Auth | `internal/auth/**` (new), `internal/server/auth*.go` | Trusted proxies, aggregator auth rules, owners (PBKDF2), TOTP, CSRF, scoped sessions | Standalone auth tests unchanged; proxy-loopback test is not admin | 1.5 |

### Phase 1: aggregator MVP with compat map ingest, no own MLAT (≈ 8 weeks)

**Wave 1A**

| WP | Owns | Work | Acceptance | aw |
|---|---|---|---|---|
| WP-P AVF codec | `internal/avf/**` | Records, flate wrapper, handshake (both sides), TLS helpers (self-signed, SPKI pin, exporter), seq/ACK | Round-trip and fuzz; MITM relay test fails the signature | 2 |
| WP-R Registry | `internal/registry/**` | `stations.db` registry tables, admission, enrol tokens, claims, public-point PRF, audit, retention and deletion jobs | Claim single-use/TTL tests; PRF stable across restarts | 2 |
| WP-B2 Beast ingest | `internal/ingest/beastin/**` | §5.1 listener, UUID/relay/ping, limits, per-link stats | Replay of the user's capture; unknown escapes do not drop the link | 1.5 |
| WP-U Uplink | `internal/uplink/**` | Client, backoff/resume, RAM ring + disk spool, backfill, selective filter, result injection, no-MLAT-forward | Simnet 30-minute outage backfills; MLAT-stamped frames are never sent | 2 |
| WP-T Tracker lanes | `internal/tracker/**` | §6.3-6.4: Handle returning a result, lanes + Credit, lane ICAO sets, per-viewer geometry projection API, range gate, heardBy, `flight_station` emission hook, guess-sample hook | Benchmarks ≤ 700 ns per unique frame, ≤ 120 ns per credit; standalone tests unchanged | 3.5 |

**Wave 1B**

| WP | Owns | Work | Acceptance | aw |
|---|---|---|---|---|
| WP-I Ingest core | `internal/ingest` (root), `internal/ingest/avfsrv/**` | Sessions, host-offset estimator, normaliser, grouper with the repeat rule, FLOW/INTEREST plumbing, late routing, budgets | DF11-repeat and 100-station dedup gates | 2.5 |
| WP-D Store/analytics | `internal/store/**`, `internal/analytics/**` except `locate2*` | `stations.db` opening with a migration subset, namespaced ant, `WithAntPrefix`, `flight_station` and `mlat_*` migrations, export | Existing analytics tests pass with prefix ""; prefix tests | 1.5 |
| WP-H Server | `internal/server/**` except auth, `internal/privacy/**` (new) | Scopes, route lint, cache keys, `/api/public|me|admin`, publicdto projection, `/data/*.json`, Feed Check, claim APIs, rate limits, golden tests | Golden crawl clean on fixtures | 3 |
| WP-E Engine/config | `internal/engine/**`, `internal/config/**`, `cmd/airview/**` | Modes, host station, uplink tee, StationRuntime lifecycle, registry ↔ ingest ↔ tracker ↔ store wiring, SBS-in bridge | Simnet 20 AVF + 10 Beast feeders + host on one map for 24 h | 3 |
| WP-HB Hub | `internal/hub/**` | Aggregator admin-only, quiet close | ADSBx-style Beast connect is closed and logged once | 0.5 |
| WP-W Web v1 | `web/static/**` | ctx.station, picker, View selector (Network/My/Station/Admin), `/me/check`, `/me/stations`, claim, admin registry, privacy settings | Manual checklist; no receiver data visible in public view | 4 |
| WP-FB Feeder | `cmd/airview-feeder/**` (non-Windows files), `internal/feederui/**` | CLI, auto-detect, status page, claim, Linux run | Connects to simnet aggregator; `claim` prints a code | 2 |
| WP-PI Pi installer | `deploy/feeder/pi/**` | §12.2 | shellcheck; container non-interactive install passes | 1.5 |
| WP-DOC Docs | `docs/aggregator/**` except AVF.md | Operator guide, privacy notice, LIA, consent text, retention schedule, CLEANROOM.md | Reviewed against §8 | 1 |

**Phase 1 exit:**
- an OARC-style community points readsb at 30004 and keeps its own mlat-server through the bridge;
- AirView feeders join over AVF;
- owners claim and see their station pages;
- golden tests are green;
- simnet with 100 stations runs stable for 24 h.

### Phase 2: own MLAT core and the mlat-client server (≈ 9 weeks)

| WP | Owns | Work | Acceptance | aw |
|---|---|---|---|---|
| WP-RF Reference | `internal/mlat/ref/**` | Reference tracks, transmit-instant interpolation, GNSS−baro field | Interpolation error < 5 m on sim | 2 |
| WP-K Clock | `internal/mlat/clock/**` | §9.2 complete (states incl. β_short, gauge, probation, step detection, adaptive Q, snapshots) | Step, anchor-fault, β_short and epoch-cost gates | 4 |
| WP-GR Group/plan | `internal/mlat/group/**`, `internal/mlat/plan/**` | §9.3 and the planner | Cluster split of DF4 bursts; plan sizes respected | 2 |
| WP-SV Solver | `internal/mlat/solve/**` | §9.4 | CRLB vs MC; RAIM excludes an injected 2 µs fault; 3+alt branches | 2.5 |
| WP-TR Track | `internal/mlat/track/**` | §9.5 | Ambiguity gate; 2-station maintenance holds σ < 300 m | 2.5 |
| WP-M Engine + quality v1 | `internal/mlat` (root), `internal/mlat/quality/**` | Wiring, result routing with the contribution rule, hold-out, metrics | Accuracy gates in §13.2 rows 1-3 | 2 |
| WP-B3 mlatc server | `internal/ingest/mlatc/**` | §5.2 | Replays of user captures; a live interop client receives results | 2.5 |
| WP-E2 / WP-W2 | engine and web (same owners as phase 1) | MLAT on/off, timing and MLAT owner sections, admin MLAT console, sync matrix | End-to-end simnet MLAT on the map with grades | 2 + 2 |

**Phase 2 exit:**
- the §13.2 MLAT gates pass;
- live hold-out P50 is published;
- the user's own mlat-client connects and receives results;
- the bridge can be switched off.

### Phase 3: self-location, Windows feeder, self-update (≈ 6 weeks)

| WP | Owns | Work | Acceptance | aw |
|---|---|---|---|---|
| WP-SU Survey | `internal/mlat/survey/**` | §10.2-10.3 (declared check, MSL/HAE test, consent-gated job API) | Survey and suspect gates | 3 |
| WP-L2 Locate Me v2 | `internal/analytics/locate2*.go` + `guess_samples` migration | §10.4 | Error vs the user's GNSS position < 10 km on recorded data | 3 |
| WP-WF Windows feeder | `cmd/airview-feeder/*_windows.go`, `internal/wintray/**`, `deploy/feeder/windows/**` | Service, tray, Inno Setup | Install, run, uninstall on Windows 11; tray menu works | 2.5 |
| WP-UP Self-update | `internal/selfupdate/**`, `deploy/release/**` | Signed manifest tooling, verify, swap, rollback | Tampered manifest is rejected; rollback on a failed health check | 1.5 |
| WP-W3 Web | `web/static/**` | Location section, survey confirm flow, quality pages | Checklist | 1.5 |

### Phase 4: precision and polish (≈ 5 weeks)
- WP-Q2 (`internal/mlat/quality`): CRLB admin map, k_atm regression, ADS-B verification, airframe-latency calibration behind a hold-out A/B test.
- WP-SU2 (`internal/mlat/survey`): network bundle adjustment.
- WP-H2 (`internal/server`, `internal/registry`): share links, revocable and scoped, with a location warning.
- WP-I2 (`internal/ingest/avfsrv`, `internal/uplink`): selective-mode `MINUTE_STATS` merge.

### Phase 5: as needed
- Partitioned clock filter above 200 stations per component.
- ICAO-sharded tracker.
- mlatc UDP.
- Feeder "also feed others" clients.
- Federation between aggregators.
- Mode A/C MLAT.
- Windows RTL-SDR (rtl_tcp + Go demodulator, then clean-room WinUSB).
- Pi image with first-boot web setup.
- Bistatic SSR self-location (research).

**Total:** about 52 aw. With 6-8 parallel agents, phases 0-3 take about 6-7 calendar months, and the MVP (phase 1) about 2.5 months.

---

## 15. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Wire-compat details (readsb ping direction, mlat-client timestamp convention per type) | Captures from the user's own equipment before WP-B2 and WP-B3 merge; tolerant parsers; interop suite; β_short absorbs residual offsets; the bridge keeps the old path alive |
| GPL/AGPL contamination | Clean-room rule, CLEANROOM.md, review checklist; GPL tools only as separate processes |
| Few GPS anchors | Joint filter degrades gracefully to relative time; the user's EVO anchors the region; admin CRLB map guides where to add GPS hardware |
| Filter cost | 0.5 s epochs, component split, benchmark gate, phase 5 partitioning |
| Privacy inference | Matrix, typed DTOs, deny-by-default, golden tests, no public receiver information, unlinkable peer tags, admin-only CRLB, audit, notice |
| Wrong or fuzzed declared positions | Survey cross-check, `suspect` exclusion, owner notice |
| Tracker mutex at scale | Grouper in front, lock-free credits, profiling gate, phase 5 sharding |
| SD wear | No spool on SD, 10 s lane flushes, SSD required for a Pi aggregator |
| No code-signing certificate | Portable ZIP, documented SmartScreen steps |
| Patents (US 9377524, 8791861, 9255804) | Textbook Kalman/least-squares methods; claim review before phase 2 ships publicly |
| Account recovery without e-mail | TOTP recovery codes, re-claim with a new device code, admin reset |

---

## 16. Default answers to open questions (assumptions this design commits to)

| Question | Default |
|---|---|
| Accept unmodified readsb/mlat-client feeders? | Yes: 30004 in phase 1, 31090 in phase 2 |
| Owner accounts | Username + password + optional TOTP. No e-mail and no magic links. |
| Admission | `invite`, with enrol tokens issued by the admin (OARC-style members-only) |
| Return MLAT results | Contributors only, with the minimum-contribution rule. Never re-forwarded. |
| tar1090-shaped JSON | On, public, with no receiver fields |
| Full vs selective | Full up to 100 stations, selective above |
| Geoid | EGM96 1° global grid embedded |
| Hub 30154 port | Default unchanged; quiet close of non-protocol clients |
| Coverage-island guard | Off; residual risk documented |
| UK only | The privacy notice is written for UK GDPR; EU users are covered by the same notice, with no EU representative assumed |
| Radarcape GNSS position upload | Off (`ShareGNSS=false`); the declared position is sent instead |
| Main app as a feeder | Yes, through `uplinks[]`; the dedicated `airview-feeder` exists for headless boxes |

---

## 17. Decisions for the user (at most 5)

1. **Hosting and scale of the club aggregator.** A VPS (2-4 vCPU, 4-8 GB, SSD) or a Pi 5 with NVMe, and the expected feeder count.
   - **Recommended default:** a 2-4 vCPU VPS sized for up to 100 feeders in full-traffic mode, with the user's EVO 2CH as the host or anchor station feeding it over an uplink.
2. **Public feeder map default.** Pins `hidden` for everyone, or `region` (0.5°) by default with opt-out.
   - **Recommended default:** `hidden` for native feeders. Compat feeders that send mlat-client `privacy:false` get `region`, matching community norms.
3. **Use of surveyed locations.** May the operator auto-use reverse-MLAT locations for MLAT (`AutoUseSurvey`) when σ is small, or only after the owner confirms?
   - **Recommended default:** owner confirmation only. Auto-use stays off.
4. **Radarcape configuration and sharing.** Confirm the EVO 2CH runs GPS timestamps (DIP#5 on, status shows UTC OK and sync OK). Choose whether its GNSS position may be uploaded to the club server (owner and admin only).
   - **Recommended default:** GPS timestamps on, which is required for it to anchor network time. `ShareGNSS` off; a declared position entered to about 10 m is sent instead.
5. **Windows code signing.** Will you obtain an Authenticode certificate for the Windows feeder installer?
   - **Recommended default:** ship unsigned with a portable ZIP and documented SmartScreen steps, and add signing when a certificate is available.

---

## 18. Licences of referenced work

| Item | Licence | Use |
|---|---|---|
| mlat-server (mutability, wiedehopf) | AGPL-3.0 | Behaviour and parameter reference only. May run as the operator's separate unmodified process behind the bridge. Never copied or linked. |
| mlat-client | GPL-3.0+ | Wire-protocol interop, clean-room. Never bundled. |
| readsb, dump1090-fa, tar1090 | GPL family (confirm the COPYING files) | Format interop. readsb-install is offered as a separate program. A separately installed tar1090 may read our JSON. |
| mlat-server-sync-map | MIT | Idea reference |
| ADSBx feedclient, airplanes.live feed, adsb.lol feed | MIT | Installer UX patterns, rewritten |
| adsb.lol api/infra | BSD-3 | Per-feeder view ideas |
| adsb.fi scripts | Licence file not verified | Behaviour reference only |
| Ultrafeeder, ADSB.im | GPL-3.0 | UX reference only |
| PiAware | BSD | Claim-flow idea |
| rtl-sdr / rtl_tcp, Zadig | GPL-2.0+ / GPL-3.0 | Optional separate processes (phase 5) |
| Inno Setup | Own permissive licence | Build tool |
| EGM96 | Public domain (NGA) | Embedded grid |
| Mapzen/Tilezen terrain | Open data, attribution | Already used by AirView |

**Papers cited, algorithms re-implemented from their descriptions:**
- Sonnleitner & Hobiger 2025, NAVIGATION 72(3), CC BY 4.0
- Strohmeier et al., arXiv 2209.13669
- Sallouha et al., arXiv 2108.00922
- SkyPos (Lizarribar et al., IEEE JSAC 2023)
- Sciancalepore et al., JNCA 2021
- Bancroft 1985
- Chan & Ho 1994
- Andrés et al., arXiv 1212.1984
- Frisch & Hanebeck, ICRA 2020

**Key URLs:**
- https://github.com/wiedehopf/readsb
- https://github.com/wiedehopf/mlat-server
- https://github.com/wiedehopf/mlat-client
- https://github.com/ADSBexchange/feedclient
- https://github.com/adsblol/api
- https://wiki.oarc.uk/flight:adsb-server-writeup
- https://adsb.oarc.uk/myip/
- https://wiki.jetvision.de/wiki/Mode-S_Beast:Data_Output_Formats
- https://navi.ion.org/content/72/3/navi.704
- https://arxiv.org/abs/2209.13669
- https://www.lenders.ch/publications/journals/jsac23.pdf
- https://ico.org.uk/for-organisations/uk-gdpr-guidance-and-resources/

**AirView files this design builds on (read-only):**
- `D:\AirView\go.mod`
- `D:\AirView\docs\ARCHITECTURE.md`
- `D:\AirView\README.md`
- `D:\AirView\internal\model\config.go`
- `D:\AirView\internal\model\model.go`
- `D:\AirView\internal\feed\beast.go`
- `D:\AirView\internal\feed\manager.go`
- `D:\AirView\internal\hub\proto.go`
- `D:\AirView\internal\hub\server.go`
- `D:\AirView\internal\tracker\ingest.go`
- `D:\AirView\internal\tracker\stats.go`
- `D:\AirView\internal\latency\timebase.go`
- `D:\AirView\internal\server\auth.go`
- `D:\AirView\internal\server\routes.go`
- `D:\AirView\internal\analytics\guess.go`
- `D:\AirView\internal\ntp\clock_windows.go`
- `D:\AirView\deploy\pi\install.sh`
---

## 19. User decisions (2026-11-13) — these override §16/§17 defaults

1. Hosting: a 2-4 vCPU / 4-8 GB VPS, full-traffic mode up to ~100 feeders; the user's EVO 2CH feeds it as GPS anchor over an uplink.
2. Public feeder pins: `hidden` by default (compat feeders that send mlat-client `privacy:false` get `region`).
3. Surveyed locations: `Aggregator.MLAT.AutoUseSurvey = true` by default — a reverse-MLAT position is used for MLAT automatically when its uncertainty is small (σ_h ≤ 50 m and passes the suspect checks). It remains owner/admin-only, never public, and the owner is notified and can override.
4. Scope: build ALL phases 0-5 in this push (including federation, mlatc UDP, Mode A/C MLAT, Windows RTL-SDR, Pi image/first-boot setup, partitioned clock filter, ICAO-sharded tracker; bistatic SSR self-location as research-grade/experimental).
5. Windows signing: unsigned portable ZIP + Inno Setup installer with documented SmartScreen steps (default).

## 20. Publication requirements (user, 2026-11-13) — binding for all phases

AirView will eventually be published for larger communities to adopt, not just one club.

1. **Headless server installs.** Aggregator mode must run fully headless on Linux VPS hosts: linux/amd64 and linux/arm64 builds, one-line installer + systemd unit (dedicated `airview` user, `/etc/airview/`, `/var/lib/airview/`, logrotate/journald), a Docker image + docker-compose example, reverse-proxy examples (Caddy/nginx with TLS), firewall/port guide, first-run admin bootstrap from the CLI (`airview admin init`), health endpoint, backup/restore and upgrade commands. No GUI or WebView dependency on servers.
2. **Large data handling.** Design targets raise from ~100 to **1,000+ feeders and 10k+ concurrent aircraft** on a mid-size VPS: the phase-5 scaling items (partitioned clock filter, ICAO-sharded tracker, selective/reduced feeds) become required, not optional. Storage must stay bounded: tiered retention/downsampling, partition or rotate large tables, batched writes, VACUUM/maintenance jobs, measured DB growth per feeder per day, and load tests with simnet at 1,000 feeders.
3. **Config files.** Every setting is configurable from a file without the UI: a fully commented example config (`airview.example.yaml`-equivalent kept in JSON-with-comments or TOML via a pure-Go stdlib parser we write — no new modules), environment-variable overrides (`AIRVIEW_*`), CLI flags for the essentials, `airview config check` validation, and a generated config reference document.
4. **Complete PDF manual — LAST.** A full user/operator manual (station, aggregator operator, feeder owner, Pi and Windows feeders, MLAT, privacy/GDPR, API reference, troubleshooting) produced as PDF only after every feature in all phases is finished and verified. Not before.
