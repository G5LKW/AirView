# AVF/1 — AirView Feed protocol, version 1 (normative)

**Status:** normative wire specification for WP-P (`internal/avf`), WP-U (`internal/uplink`) and WP-I (`internal/ingest/avfsrv`). Derived from `docs/aggregator/DESIGN.md` §4. Where this document is more precise than DESIGN §4, this document wins; additions to DESIGN §4 are marked **(added)**.

**Owner:** WP0-contract. Changes after phase 0 go through the integrator (DESIGN §14).

**Clean room:** AVF/1 is AirView's own protocol. Nothing here is taken from readsb, mlat-client, mlat-server or any GPL/AGPL source.

The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are used as in RFC 2119.

Shared Go types and constants live in `internal/model` (`ClockClass`, `ClockEventReason`, `MLATFix`, `NBucket*`, `Grade*`, `FixFlag*`, `Pin*`, `FeederIDFromPub`, `GPSTicks`, `Unwrap48`, `Unwrap24`, `NormalizeUUID`, `Admission*`). Record type codes and the codec live in `internal/avf`.

---

## 1. Conventions

| Item | Encoding |
|---|---|
| `u8`, `u16`, `u32`, `u64` | unsigned, fixed width, **big-endian** |
| `i64` | two's complement, 8 bytes, big-endian |
| `f32`, `f64` | IEEE-754 binary32 / binary64, big-endian |
| `uvarint` | unsigned LEB128 (Go `binary.AppendUvarint` / `binary.Uvarint`), at most 10 bytes; a reader MUST reject longer or overflowing encodings |
| `svarint` | zig-zag then LEB128 (Go `binary.AppendVarint` / `binary.Varint`) |
| `u24` | 3 bytes big-endian (ICAO addresses) |
| `b64` | standard base64 alphabet **with** padding (RFC 4648 §4) inside JSON strings |
| JSON | UTF-8, RFC 8259. 64-bit integers are JSON numbers and MUST be decoded as 64-bit integers, not doubles. Unknown members MUST be ignored. |
| ICAO in JSON | 6 upper-case hex digits; a non-ICAO address is prefixed with `~` |
| Time | `…Ns` = Unix nanoseconds UTC (host clock unless stated); `…Ms` = Unix milliseconds |

**Forward compatibility (applies to every binary payload):** a reader MUST ignore bytes that follow the last field it knows. A writer MAY append new fields only at the end of a payload, and only in a later revision listed in §12.

---

## 2. Transport

### 2.1 TCP and TLS
- Default port **30160/tcp** (`model.DefaultAVFPort`).
- **TLS 1.3 only.** Both sides MUST refuse lower versions. There is no plaintext mode.
- **ALPN** `avf/1` is mandatory. A server MUST abort the handshake if the client does not offer it: with the TLS alert `no_application_protocol` when the client offers other protocols, and with any fatal alert when it offers no ALPN at all (Go's TLS server cannot send `no_application_protocol` in that case). A client MUST abort if the server did not select it.
- The client SHOULD send SNI with the configured host name when it is a name (not an IP literal).
- No TLS client certificate is used. Feeder identity is proven inside the channel (§3.3).
- A TCP passthrough proxy (no TLS termination) MAY sit in front of the server. TLS always terminates in AirView, because the identity proof is bound to the TLS session (§3.3).

### 2.2 Server certificate and pinning
The server presents one of:
1. **Operator certificate** (`Aggregator.TLSCert` / `TLSKey`). The server reloads it when either file's modification time changes. Clients with an empty `Uplink.ServerPin` verify it against the system roots and the configured host name.
2. **Self-signed** (default). An ECDSA P-256 key and certificate are generated on first start in `<data>/avf-tls/` (key file mode 0600). Clients verify it by pin.

**Pin format:** `sha256/<b64(SHA-256(SubjectPublicKeyInfo DER))>`, 44 base64 characters with padding. Readers MUST also accept the unpadded and URL-safe forms (`model.ValidServerPin`).

**Client verification rule:**
- `ServerPin == ""`: normal WebPKI verification (roots + host name).
- `ServerPin != ""`: the client MUST skip chain and name verification and instead, in `VerifyPeerCertificate`, require that the SPKI hash of the **leaf** certificate equals the pin. A mismatch aborts the connection and MUST NOT be retried faster than the normal backoff (§9.1).

The pin covers the key, not the certificate, so re-issuing a certificate for the same key keeps the pin valid.

---

## 3. Handshake

### 3.1 Framing
After the TLS handshake, both peers exchange exactly three **handshake messages**, each framed as:

```
u32 length | length bytes of JSON (one object)
```

- `length` MUST be 1..65536. A larger value is a protocol error (close without a reply).
- Messages are not compressed.
- Order: `HELLO_S` (server→client), `HELLO_C` (client→server), `WELCOME` (server→client).
- The whole handshake (TCP accept to WELCOME sent) MUST complete within `Aggregator.Limits.HandshakeTimeoutSec` (default 10 s), otherwise the server closes the connection silently.

### 3.2 HELLO_S (server → client)

```json
{"proto":1,"protoMin":1,"serverId":"k3x2…","nonce":"<b64 32 bytes>","timeNs":1763000000000000000,
 "features":["flate","backfill","selective","results","claim","gnss","decoded","minute_stats"],
 "admission":"invite","name":"Club Network"}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `proto` | int | yes | Highest AVF/1 revision the server speaks (this document: 1) |
| `protoMin` | int | no **(added)** | Lowest revision it accepts; default = `proto` |
| `serverId` | string | yes | The server's stable network ID: 16 lower-case base32 characters (`model.ValidID`), generated randomly once and kept in `stations.db` |
| `nonce` | b64 | yes | 32 fresh random bytes (crypto/rand), unique per connection |
| `timeNs` | int64 | yes | Server UTC clock at send time. Clients MAY use it for a coarse clock-sanity warning; never for timing |
| `features` | string[] | yes | Features the server supports (§3.6) |
| `admission` | string | yes | `open` \| `approve` \| `invite` (`model.Admission*`) |
| `name` | string | no **(added)** | Network display name, shown by feeder UIs |

### 3.3 HELLO_C (client → server)

```json
{"body":"<b64 of the body JSON bytes>","sig":"<b64 64-byte Ed25519 signature>"}
```

`body` decodes to these JSON bytes (signed **as transmitted**, so no JSON canonicalisation is needed):

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `pub` | b64 | yes | Ed25519 public key, 32 bytes. Feeder ID = `model.FeederIDFromPub(pub)` |
| `uuid` | string | no | The feeder's UUID file content, normalised by `model.NormalizeUUID`; used for support and to join compat links |
| `software` | string | yes | `airview-feeder` or `airview` |
| `version` | string | yes | Software version |
| `os`, `arch` | string | yes | `runtime.GOOS`, `runtime.GOARCH` |
| `features` | string[] | yes | Features the client supports (§3.6) |
| `seqEpoch` | string | yes **(added)** | 16 lower-case hex digits identifying the client's sequence space (§5.2) |
| `resumeToken` | string | no | Token from the previous WELCOME, at most 10 minutes old |
| `enrolToken` | string | no | Admin-minted enrol token (invite admission) |

**Signature.** The client computes

```
ekm  = TLS ExportKeyingMaterial(label = "EXPORTER-AirView-AVF1", context = empty, length = 32)
msg  = "AVF1" (4 ASCII bytes)
     ‖ serverId (its 16 ASCII bytes, as received)
     ‖ nonce (32 raw bytes)
     ‖ ekm (32 bytes)
     ‖ SHA-256(bodyBytes) (32 bytes)
sig  = Ed25519.Sign(priv, msg)            // 116-byte message, 64-byte signature
```

The server recomputes `msg` with its own `serverId`, `nonce` and exporter value and MUST verify `sig` with `pub` before it looks at any other body field.

- A relay (MITM) terminates two different TLS sessions, so its exporter values differ, and a relayed signature fails.
- A captured HELLO_C cannot be replayed, because `nonce` is fresh.
- A signature is required on **every** connection, including resumed ones.

The private key lives in `identity.key` (mode 0600) on the feeder. `airview-feeder new-identity` replaces it.

### 3.4 WELCOME (server → client)

```json
{"status":"approved","feederId":"mzuhvlpymk6xo3ep","sessionId":"7f3c…","resumeToken":"…",
 "ackSeq":1234,"config":{"batchMs":50,"mode":"full","mlat":true,"returnResults":true},
 "features":["flate","backfill","results","claim"],"motd":"Welcome to the club network"}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `status` | string | yes | `approved` \| `pending` \| `denied` |
| `feederId` | string | if not denied | Derived from `pub` |
| `sessionId` | string | if not denied | Opaque, for logs and support |
| `resumeToken` | string | if not denied | Opaque bearer token, valid 10 minutes, memory only on both sides |
| `ackSeq` | uint64 | if not denied | Highest seq of this `seqEpoch` the server already holds (0 = none) (§5.2) |
| `config.batchMs` | int | yes | Client flush interval, ms (default 50) |
| `config.mode` | string | yes | `full` \| `selective` (§7) |
| `config.mlat` | bool | yes | The server uses this feeder's timestamps for MLAT |
| `config.returnResults` | bool | yes | The server will send MLAT_RESULTS |
| `features` | string[] | no **(added)** | Effective features = intersection of both lists. Absent = intersection computed by the client |
| `motd` | string | no | Message to show to the feeder owner |
| `reconnectIn` | int | no | Seconds the client MUST wait before reconnecting after this connection closes |
| `code` | string | if denied | Error code (§10) |
| `reason` | string | if denied | Human-readable text |

- `approved`: full participation.
- `pending` (approve admission): the session stays open and data is accepted, but it goes to the station's own lane only, never to the public map or MLAT. The status can later change to approved via a NOTICE `approved` (§6.2), without reconnecting.
- `denied`: the server closes the connection right after WELCOME. The client MUST honour `reconnectIn` (or back off at least 10 minutes when it is absent) and MUST show `reason` to the owner.

### 3.5 Switch to records
Immediately after WELCOME (not denied), both directions switch to the record stream (§4). The client MUST send a RECEIVER record (§5.4) for each receiver before any other data of that receiver in this session. Exception: records replayed unchanged under §5.2 strategy (a) may precede the RECEIVER records, because the server already knows those receivers from the earlier session of the same `seqEpoch` (it keeps that session's receiver table with the watermark).

### 3.6 Features
Unknown feature names MUST be ignored.

| Feature | Meaning when both sides have it |
|---|---|
| `flate` | Mandatory in AVF/1. A peer without it is denied with `proto_version`. |
| `backfill` | BACKFILL records and late frames are allowed |
| `selective` | The server may assign `selective` mode and send INTEREST |
| `results` | MLAT_RESULTS may be sent |
| `claim` | CLAIM / CLAIM_CODE may be used |
| `gnss` | GNSS_POS may be sent |
| `decoded` | DECODED may be sent |
| `minute_stats` | MINUTE_STATS may be sent |

---

## 4. Record stream

### 4.1 Compression
- Each direction is **one** raw DEFLATE stream (RFC 1951, no zlib or gzip header), Go `compress/flate` at level 1 (`flate.BestSpeed`). The stream starts at the first byte after WELCOME. Readers must accept every valid DEFLATE block type: the AirView server sends its direction (PING, ACK, INTEREST, FLOW, results: a few small records per second) as **stored (uncompressed) blocks**, each sync flush ending with the usual empty stored block `00 00 00 FF FF`, so it keeps no compressor state per session.
- The writer performs a **sync flush** (Go `(*flate.Writer).Flush`) when the first of these happens:
  - `batchMs` has elapsed since the first unflushed record;
  - 32 KiB of uncompressed data are pending;
  - a control record (ACK, PING, PONG, FLOW, INTEREST, NOTICE, GOODBYE, CLAIM, CLAIM_CODE) was written.
- **Decompression guards (server side; the client applies the same with its own limits):**
  - inflated output at most 2 MB/s per connection, averaged over 10 s;
  - inflated/wire byte ratio at most 100:1 over any 1 MiB of output.
  - A violation closes the connection with GOODBYE `protocol_error`.

### 4.2 Record framing

```
u8 type | uvarint len | len bytes of payload
```

- `len` MUST be ≤ 65536 (64 KiB). A larger value is a protocol error.
- Types `0x01-0x7F` travel client→server, types `0x81-0xFE` server→client. `0x00`, `0x80` and `0xFF` are reserved and are protocol errors.
- **Unknown types MUST be skipped** using `len`. A known type arriving in the wrong direction is a protocol error.
- A payload shorter than its mandatory fields is a protocol error. Trailing bytes are ignored (§1).

### 4.3 Record type registry

**Client → server.** "seq" = the payload starts with `uvarint seq` (§5.2).

| Type | Name | seq | Encoding | Section |
|---|---|---|---|---|
| 0x01 | RECEIVER | yes | JSON | §5.4 |
| 0x02 | FRAMES | yes | binary | §5.5 |
| 0x03 | CLOCK_STATUS | yes | binary | §5.6 |
| 0x04 | CLOCK_PAIR | yes | binary | §5.7 |
| 0x05 | CLOCK_EVENT | yes | JSON | §5.8 |
| 0x06 | INPUT_STATUS | yes | JSON | §5.9 |
| 0x07 | GNSS_POS | yes | JSON | §5.10 |
| 0x08 | DECODED | yes | binary head + JSON | §5.11 |
| 0x09 | MINUTE_STATS | yes | JSON | §5.12 |
| 0x0A | PONG | no | binary | §5.13 |
| 0x0B | BACKFILL | yes | JSON | §5.14 |
| 0x0C | CLAIM **(added)** | no | JSON | §5.15 |
| 0x0D | GOODBYE **(added)** | no | JSON | §5.16 |

**Server → client.**

| Type | Name | Encoding | Section |
|---|---|---|---|
| 0x81 | PING | binary | §6.1 |
| 0x82 | ACK | binary | §6.1 |
| 0x83 | INTEREST | JSON | §7.2 |
| 0x84 | FLOW | JSON | §7.3 |
| 0x85 | MLAT_RESULTS | binary | §6.3 |
| 0x86 | FEEDBACK | JSON | §6.4 |
| 0x87 | CLAIM_CODE | JSON | §5.15 |
| 0x88 | NOTICE | JSON | §6.2 |
| 0x89 | GOODBYE **(added)** | JSON | §5.16 |

For records with a seq, the JSON or binary body follows directly after the seq varint.

---

## 5. Client → server records

### 5.1 Receivers and stations
One feeder can announce several receivers, each identified by `rx_no` (1..255, `FeederReceiver.RxNo`). The server maps (feederId, rx_no) to one station. A receiver is one timestamp clock; a two-channel receiver is one `rx_no` with `channels: 2`.

### 5.2 Sequence numbers, ACK and retransmission
- Every seq-carrying record has a `seq` that is **exactly one more** than the previous seq-carrying record of the same `seqEpoch`. The first record of a new `seqEpoch` has seq 1.
- `seqEpoch` (HELLO_C) is chosen randomly by the client when it creates a new ring/spool (for example at process start without a spool, or when the spool is discarded). It is kept as long as the ring/spool lives, across reconnects.
- The server keeps, per (feederId, seqEpoch), a **watermark**: the highest seq it has processed with no gaps. It sends `ACK{seq = watermark}` at least once per second while records arrive, and it reports it as `WELCOME.ackSeq` when the same `seqEpoch` reconnects (0 for an unknown `seqEpoch`). Watermarks are kept for at least 10 minutes after a session ends.
- The server MUST drop any record whose seq is ≤ the watermark (duplicate), and MUST treat a gap (seq > watermark+1) within one connection as a protocol error.
- The client trims its ring up to and including the acknowledged seq.
- **Retransmission on reconnect** (corrected after Wave 1A: the earlier text sent RECEIVER records with new seqs *before* replaying held seqs, which the server must reject as a gap). The only wire rule is: **the first seq-carrying record after WELCOME has seq `ackSeq+1`, and seqs stay gap-free.** Two client strategies satisfy it, and servers MUST accept both:
  - (a) **replay:** resend unchanged, in order, every held record with seq > `ackSeq` whose data is younger than the resume window (30 s); the RECEIVER records follow with the next new seqs (`internal/avf` `SendRing.Resume`).
  - (b) **renumber:** send one RECEIVER record per active receiver starting at seq `ackSeq+1`, then re-encode the unacknowledged young records with the following seqs (`internal/uplink`). The server never processed a seq above its watermark, so renumbering cannot duplicate data. When queued young data still belongs to an older clock epoch, the RECEIVER announces that older epoch and the queued CLOCK_EVENT records follow before the newer epoch's FRAMES.
- Unacknowledged data older than the resume window is no longer live. It MAY be sent later as **backfill** (§5.14): re-encoded as new records with new seqs, with the late bit set on every frame.
- If `ackSeq` is larger than the client's highest seq, or (strategy (a)) the client no longer holds `ackSeq+1` or the oldest unacknowledged record has left the resume window, the client MUST close, start a new `seqEpoch`, reconnect at once and send the unacknowledged records as backfill.

### 5.3 Timestamps (DESIGN §4.3)
The feeder **unwraps but never converts** receiver time:

| Class (`model.ClockClass`) | Tick unit | Tick value |
|---|---|---|
| 1-3 (GPS) | 1 ns | `model.GPSTicks(nsOfDay, hostUTCNs)` = dayIndex·86 400·10⁹ + nsOfDay, with dayIndex chosen within ±12 h of host UTC. The legacy "second of day 1..86400" quirk is corrected first (`feed.Timestamper`). |
| 4-6 (12 MHz) | 1/12 µs | 48-bit counter unwrapped with `model.Unwrap48` |
| 7 (SBS 20 MHz) | 50 ns | 24-bit counter unwrapped with `model.Unwrap24`, bridged by host time across gaps longer than 0.4 s |
| 0 | — | no ticks; every frame has `tsInvalid` set |

Ticks are monotonic within one (rx_no, epochId). A new epoch (new `epochId`, with a CLOCK_EVENT) MUST be started on: a process or receiver restart, a step of more than 90 s against host time (`model.EpochStepLimit`), any backwards step, a declared sample drop, and any timestamp-mode or class change. `epochId` is a u32 chosen by the client and MUST differ from the previous epoch of the same receiver; a random start value followed by increments is recommended.

Host times (`…HostNs`) are the feeder's UTC clock (NTP-corrected where available) and are used only for grouping, latency statistics and day resolution.

### 5.4 RECEIVER (0x01) — JSON
Sent first on every connection for every receiver, and again whenever a field changes.

```json
{"rxNo":1,"name":"evo-north","clockClass":"gps_utc","nominalHz":1000000000,"epochId":17,
 "channels":2,"decoder":"radarcape-evo 2.10",
 "location":{"lat":51.5,"lon":-0.1,"alt":45.0,"altRef":"msl","source":"manual","sigmaM":10},
 "antennas":[{"id":"vertical","kind":"collinear","aglM":8},{"id":"yagi","kind":"yagi","aglM":8}],
 "privacy":{"pin":"hidden","publicStats":false,"showName":false},"timestamps":true}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `rxNo` | int | yes | 1..255 |
| `name` | string | no | Display name, `[A-Za-z0-9 _.-]{0,32}` recommended |
| `clockClass` | string | yes | `model.ClockClass.String()` name. Unknown names are treated as `none` |
| `nominalHz` | number | yes | Tick rate (must match the class) |
| `epochId` | uint32 | yes | Current epoch |
| `channels` | int | yes | RF channels (1..15) |
| `decoder` | string | no | Free text: receiver or decoder model and version |
| `location` | object | only if `Uplink.SendLocation` | `lat`, `lon` (WGS-84 deg), `alt` (m), `altRef` `hae`\|`msl`, `source` `manual`\|`gnss`\|`estimated`, `sigmaM` (1σ horizontal, m) |
| `antennas` | array | no | `id`, `kind` (`model.Ant*`), `aglM` |
| `privacy` | object | yes | `pin` (`model.Pin*`), `publicStats`, `showName` |
| `timestamps` | bool | yes **(added)** | `Uplink.Timestamps`; false = no MLAT, no timing-based self-location |

**MUST NOT** contain host names, LAN IP addresses, MAC addresses or user account names.

### 5.5 FRAMES (0x02) — binary

```
uvarint seq
uvarint rx_no
uvarint epochId            (u32)
u64     baseTicks          ticks of the reference point
i64     baseHostNs         host UTC ns of the reference point
uvarint n                  number of frames (1..4096)
n × frame:
  u8      hdr
  uvarint dTicks           ticks − previous frame's ticks (first frame: − baseTicks)
  svarint dHostUs          host µs − previous frame's host µs (first frame: − baseHostNs/1000)
  u8      sig              raw Beast signal byte (0 = unknown)
  bytes   payload          2, 7 or 14 bytes according to hdr.kind
```

`hdr` bits:

| Bits | Meaning |
|---|---|
| 0-1 | kind: 0 = Mode A/C (2 bytes), 1 = Mode S short (7 bytes), 2 = Mode S long (14 bytes), 3 = reserved (protocol error) |
| 2 | late: backfilled frame (§5.14) |
| 3 | tsInvalid: the frame has no valid receiver timestamp |
| 4-7 | channel + 1 (0 = channel unknown, 1..15 = RF channel 0..14) |

Rules:
- Within a FRAMES record, frames with a valid timestamp MUST be in non-decreasing tick order, so `dTicks` is never negative.
- A frame with `tsInvalid` MUST carry `dTicks = 0` and does not change the running tick value. Its `dHostUs` still applies.
- Host time is reconstructed as `hostNs = baseHostNs + 1000·Σ dHostUs` (µs resolution).
- `baseTicks` SHOULD be the first valid frame's ticks (then its `dTicks` is 0). For class 0 it MUST be 0.
- Frames that carry the MLAT magic timestamp (`feed.IsMLATTimestamp`), frames from feeds with `MLATOnly` or `NoForward`, and frames whose 48-bit timestamp is 0 as timed frames MUST NOT be sent. (A 0 timestamp MAY be sent with `tsInvalid`.)
- All frames in a record share `rx_no` and `epochId`. An epoch change closes the current record.
- The server MAY reject a record whose ticks go backwards relative to the previous FRAMES record of the same (rx, epoch) by more than 1 s.

**Test vector.** seq 1, rx 1, epoch 7, baseTicks 1 000 000, baseHostNs 1 763 000 000 000 000 000; frame 1 = long `8D4840D6202CC371C32CE0576098`, channel 0, sig 0x80, dTicks 0, dHostUs 0; frame 2 = short `5D4840D6A1B2C3`, channel unknown, sig 0x40, dTicks 12 000, dHostUs −250. The complete record (type, len, payload) is:

```
02 33 01 01 07 00000000000f4240 18776f28c3038000 02
   12 00 00 80 8d4840d6202cc371c32ce0576098
   01 e05d f303 40 5d4840d6a1b2c3
```

### 5.6 CLOCK_STATUS (0x03) — binary
Radarcape type-'4' status, sent at 1 Hz while the receiver produces it.

```
uvarint seq
uvarint rx_no
i64     hostNs
u8      settings           first settings byte of the type-'4' frame
u8      gpsStatus          GPS status byte (lock, UTC OK, sync OK bits)
svarint ppsOffsetTicks     NCO versus 1PPS offset, signed ticks
uvarint rawLen             0..64
bytes   raw                the complete type-'4' payload, for forward compatibility
```

The status bits are authoritative for clock-class detection and for anchor probation condition (a) (DESIGN §9.2.5).

### 5.7 CLOCK_PAIR (0x04) — binary
Sent every 5 s per receiver with a timed clock.

```
uvarint seq
uvarint rx_no
uvarint epochId
u64     ticks
i64     hostUtcNs
uvarint hostErrEstNs       the feeder's estimate of its host-clock error (NTP), ns
```

### 5.8 CLOCK_EVENT (0x05) — JSON

```json
{"rxNo":1,"oldEpoch":16,"newEpoch":17,"reason":"jump","samplesLost":0,"hostNs":1763000000000000000}
```

`reason` is a `model.ClockEventReason`: `restart` \| `sample_drop` \| `jump` \| `backwards` \| `mode_change`. `samplesLost` is optional. The record MUST precede the first FRAMES record of `newEpoch`.

### 5.9 INPUT_STATUS (0x06) — JSON
Sent on change and at least every 60 s.

```json
{"rxNo":1,"connected":true,"fps":1830.5,"crcErrors":12,"type":"beast","clockClass":"gps_utc","sinceMs":1763000000000}
```

### 5.10 GNSS_POS (0x07) — JSON
Only if the `gnss` feature is effective, `Uplink.ShareGNSS` is true and the receiver's alias flag is clear.

```json
{"rxNo":1,"lat":51.50012,"lon":-0.10034,"altM":47.2,"altRef":"hae","tMs":1763000000000}
```

### 5.11 DECODED (0x08)
Untimed updates from SBS or JSON inputs; map only. They never feed MLAT or clock sync.

```
uvarint seq
uvarint rx_no
bytes   JSON array of update objects (to the end of the payload)
```

Update object members: `icao` (required), `tMs` (required), `cs`, `sq`, `lat`, `lon`, `altBaro` (ft), `altGeom` (ft), `gs` (kt), `trk` (deg), `vr` (ft/min), `gnd` (bool), `sig` (dBFS), `n` (message count, default 1). Updates that are MLAT results MUST NOT be sent.

### 5.12 MINUTE_STATS (0x09) — JSON
Exact per-minute counters computed at the feeder, used for the station's statistics in selective mode.

```json
{"rxNo":1,"minute":1763000040,"df":{"0":120,"4":800,"5":300,"11":2400,"17":9100,"20":400,"21":150},
 "modeAC":0,"icaos":143,"positions":4200,"crcFail":310,"rssi":{"lo":-50,"counts":[0,3,7,…]}}
```

`minute` is the Unix second at the start of the minute. `rssi.counts[i]` counts frames with signal in [lo+i, lo+i+1) dBFS.

### 5.13 PONG (0x0A) — binary

```
uvarint id                 the PING id
i64     rxNs               feeder host UTC when the PING was read
i64     txNs               feeder host UTC when the PONG was written
```

### 5.14 BACKFILL (0x0B) — JSON

```json
{"phase":"begin","fromSeq":5001,"toSeq":9200,"spanNs":1800000000000}
```

- `phase` is `begin` or `end`. `fromSeq` / `toSeq` name the **original** seq range of the data being replayed (informational); `spanNs` is the time span covered.
- FRAMES records between `begin` and `end` carry their original ticks and host times and have the late bit set on every frame. Other record types carried in backfill (DECODED, MINUTE_STATS) are treated as late.
- Live data always goes first. Backfill MUST use at most 25 % of the link rate.
- **Late data feeds only lanes and statistics (with the original timestamps), `flight_station` and history. It never feeds the live map, the clock filter or live MLAT.**

### 5.15 CLAIM (0x0C) and CLAIM_CODE (0x87) — JSON
The feeder asks for a claim code so its owner can claim the station on the website.

```json
CLAIM       {"rxNo":1}
CLAIM_CODE  {"rxNo":1,"code":"ABCDOIEB","expiresMs":1763086400000}
```

The code is 8 base32 characters (`model.NewClaimCode`), single use, valid 24 h, and stored hashed. The server MAY rate-limit CLAIM and then replies with NOTICE `rate_limited`.

### 5.16 GOODBYE (0x0D client→server, 0x89 server→client) — JSON
An orderly close. The sender flushes, sends GOODBYE, then closes the TLS connection.

```json
{"code":"shutting_down","reason":"server restart","reconnectIn":15}
```

`reconnectIn` (seconds) is meaningful only server→client, and the client MUST honour it.

---

## 6. Server → client records

### 6.1 PING (0x81) and ACK (0x82) — binary

```
PING: uvarint id | i64 serverSendNs           every 5 s
ACK:  uvarint seq                             at least 1/s while records arrive
```

The server keeps the minimum-RTT PONG of a sliding window to estimate the RTT and the feeder host-clock offset:

```
offset ≈ ((rxNs − serverSendNs) + (txNs − serverRecvNs)) / 2
```

Test vector: ACK of seq 300 = `82 02 ac 02`.

### 6.2 NOTICE (0x88) — JSON

```json
{"code":"approved","level":"info","text":"Your station was approved"}
```

`level` is `info` \| `warn` \| `error`. Defined codes: `approved` (pending → approved), `pending`, `rate_limited`, `location_suspect`, `clock_class_downgraded`, `spool_ignored`, `update_available`, and any error code of §10. Unknown codes are shown as text.

### 6.3 MLAT_RESULTS (0x85) — binary
Sent only to stations that contributed to the aircraft's track in the last 60 s **and** sent at least 10 timed frames per minute while synced (DESIGN §8.4). **Results carry no station identities.**

```
uvarint n                  number of fixes (1..1000)
n × fix:
  u24   icao
  i64   tUnixNs            transmit time
  f64×3 ecef               metres
  f32×6 cov                m², upper triangle xx xy xz yy yz zz
  u8    nBucket            model.NBucket*  (0: 3, 1: 4, 2: 5-8, 3: 9+)
  u8    grade              model.Grade*    (1 A … 4 D)
  u8    flags              model.FixFlag*
  u8    opt                bit 0: velocity present; other bits reserved (0)
  [f32×3 velENU]           m/s, only when opt bit 0 is set
```

A fix without velocity is 63 bytes (3+8+24+24+4); with velocity, 75 bytes. Feeders publish results locally (Beast with the MLAT magic timestamp on port 30105) and MUST NOT forward them upstream.

Test vector (one fix, ICAO 4840D6, t = 1 763 000 000 123 456 789, ecef (3980000.5, −10000.25, 4970000.75), cov (400, 10, −5, 400, 3, 900), nBucket 1, grade 1, flags 0x02, no velocity):

```
85 40 01 4840d6 18776f28ca5f4d15
   414e5d7040000000 c0c3882000000000 4152f58430000000
   43c80000 41200000 c0a00000 43c80000 40400000 44610000
   01 01 02 00
```

### 6.4 FEEDBACK (0x86) — JSON
Own station only; sent every 10-60 s.

```json
{"rxNo":1,"sync":"synced","anchor":"probation","sigmaThetaNs":42.1,"driftPpb":3.2,"outlierPct":0.8,
 "peers":{"<50":3,"50-150":5,">150":1},"location":{"status":"declared","deltaM":12},"warnings":["…"]}
```

`peers` gives counts per distance band (`model.PeerBand`) only, never identities. `sync` is `unsynced` \| `syncing` \| `synced`. `anchor` is `none` \| `probation` \| `anchor` \| `demoted` \| `self_asserted`.

---

## 7. Traffic modes and flow control

### 7.1 Modes
- `full`: every timed frame (default while approved online stations ≤ `Limits.SelectiveAbove`).
- `selective`: frames of aircraft in the station's `syncSet` and `mlatSet` in full; everything else reduced to one frame per aircraft per `reduceMs` for each DF. MINUTE_STATS keep the counters exact.

### 7.2 INTEREST (0x83) — JSON

```json
{"rxNo":1,"mode":"selective","reduceMs":500,"reset":false,
 "syncAdd":["4840D6"],"syncDel":[],"mlatAdd":["400F01"],"mlatDel":["3C6586"]}
```

`reset:true` clears both sets before applying the adds. The client applies INTEREST at the next batch boundary.

### 7.3 FLOW (0x84) — JSON

```json
{"batchMs":200,"pause":false,"slowFactor":0.5}
```

- `pause:true`: send only control records, CLOCK_PAIR and INPUT_STATUS until the next FLOW with `pause:false`; buffer the rest in the ring.
- `slowFactor` in (0,1]: limit the data rate to that fraction of the current rate (by moving to reduce-style traffic first).
- The server sends FLOW when its per-station queue stays above 50 % for 5 s. If that persists, it moves the station to selective. Above `Limits.MaxFPSPerStation` or `MaxKBpsPerStation` it sends FLOW, then GOODBYE `rate_limited` with `reconnectIn: 300`.

---

## 8. Session limits (server)
All values come from `model.AggLimits`; the defaults are shown.

| Limit | Default |
|---|---|
| Handshake timeout | 10 s |
| Unauthenticated connections per IP | 4 |
| Connections per IP / per IPv4 /24 or IPv6 /48 | 16 / 32 |
| Stations per IP (unless in `AllowCIDRs`) | 8 |
| Per station | 6000 fps, 200 kB/s compressed |
| Global ingest | `IngestFPS` (3 000 000 fps); selective-eligible traffic is shed first |
| Max record | 64 KiB |
| Max handshake message | 64 KiB |

---

## 9. Liveness and reconnect

### 9.1 Client
- Backoff from 1 s doubling to 60 s, ±20 % jitter. Reset after a session that stayed up for 60 s. `reconnectIn` overrides the next delay.
- Reconnect after 30 s with neither PING nor ACK received.
- A resume token less than 10 minutes old is sent in HELLO_C. It lets the server keep the station "online" through gaps under 30 s.
- Buffering: a RAM ring of 10 minutes, plus an optional disk spool (`Uplink.SpoolMB`, `model.ResolveSpoolMB`) of 4 MiB segment files written **only while disconnected**.

### 9.2 Server
- Drop a session after 60 s without any record.
- A second session with the same feeder key from a **different IP** while the first is alive is refused with `cloned_feeder`. From the same IP, the new session replaces the old one (the old one gets GOODBYE `replaced`).

---

## 10. Error codes
Used in `WELCOME.code` (denied), GOODBYE `code` and NOTICE `code`.

| Code | Where | Meaning | Client action |
|---|---|---|---|
| `proto_version` | WELCOME | No common revision or missing `flate` | Stop; tell the owner to update |
| `bad_hello` | WELCOME | HELLO_C malformed or a required field missing | Back off |
| `bad_signature` | WELCOME | Signature invalid (or a relay in the path) | Back off; check the pin and proxies |
| `enrol_required` | WELCOME | Invite admission and no token | Stop; ask the owner for a token |
| `enrol_invalid` | WELCOME | Token unknown, expired, revoked or used up | Stop |
| `denied` | WELCOME | Refused by the admin | Honour `reconnectIn` |
| `revoked` | WELCOME, GOODBYE | Station or feeder revoked | Stop |
| `cloned_feeder` | WELCOME | Same key live from another IP | Stop; suggest `airview-feeder new-identity` |
| `limit_ip` | WELCOME | Per-IP or per-subnet limit | Back off |
| `limit_stations` | WELCOME | `MaxStations` or `PerIPStations` reached | Back off ≥ 10 min |
| `server_full` | WELCOME, GOODBYE | Global capacity | Honour `reconnectIn` |
| `rate_limited` | GOODBYE, NOTICE | Station exceeded its limits | Honour `reconnectIn` (300 s) |
| `protocol_error` | GOODBYE | Malformed record, bad seq, guard violated | Back off; report in logs |
| `replaced` | GOODBYE | A newer session of the same feeder took over | Do not reconnect this connection |
| `shutting_down` | GOODBYE | Server restart | Honour `reconnectIn` |
| `internal` | any | Server error | Back off |

Unknown codes MUST be treated like `internal`.

---

## 11. Security and privacy requirements
- The server MUST NOT send any other station's identity, name, location, IP or timestamps to a feeder. FEEDBACK and MLAT_RESULTS are aggregate or bucketed by construction.
- The client MUST NOT send host names, LAN IPs or account names. Location is sent only with `SendLocation`, GNSS only with `ShareGNSS`, timestamps only with `Timestamps`.
- Raw IPs stay in memory only on the server. `conn_log` stores HMAC(IP) with a daily-rotating salt.
- Enrol tokens, resume tokens and claim codes are bearer secrets. Logs MUST NOT contain them. The server stores enrol tokens and claim codes hashed.
- The feeder key file MUST be mode 0600 (on Windows, ACL-limited to the service account).
- Consent text (DESIGN §10.5) is shown before the first connection that sends timestamps.

---

## 12. Versioning
- **Major version** = ALPN. AVF/1 is `avf/1`. An incompatible change uses a new ALPN (`avf/2`); a server MAY offer several, and the client lists the ones it speaks.
- **Revision** = `proto` in HELLO_S, an integer that increases with compatible additions: new record types, new JSON members, new features, fields appended to binary payloads. Peers MUST tolerate higher revisions by ignoring what they do not know.
- Clock class codes (§5.3), `hdr` bit assignments, record type codes, NBucket, Grade and FixFlag values are frozen within AVF/1; new values are only ever added.

| Revision | Changes |
|---|---|
| 1 | Initial version (this document) |
