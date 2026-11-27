# AVFED/1: AirView federation protocol

AVFED/1 links two AirView aggregators ("networks"). It is AirView's own protocol, built from the
AVF/1 building blocks (docs/aggregator/AVF.md) with its own ALPN, exporter label and signature domains,
so an AVF feeder session and a federation session can never be mistaken for each other.
Implementation: `internal/federation` (handshake.go, wire.go, link.go).

## 1. Transport

- TCP, suggested port **30170** (`aggregator.federation.listen`). Either side may dial.
- TLS 1.3 only, ALPN **`avfed/1`** mandatory (the server refuses a ClientHello without it).
- Server certificate: an operator certificate (reloaded on change) or a self-signed ECDSA P-256 one kept in
  `<data>/fed-tls`. The dialler checks it according to `peers[].pin`:
  - `""` (default): no certificate check. The peer is authenticated by the mutual Ed25519 handshake (§3),
    whose signatures are bound to this TLS session; a TLS-terminating relay fails it on both sides.
  - `"webpki"`: chain and host name against the system roots.
  - `"sha256/<base64>"`: the leaf's SPKI SHA-256 (the `pin` printed by the peer).

## 2. Identities

Each network has an Ed25519 key (`<data>/federation.key`, PKCS#8 PEM, mode 0600). Its **node ID** is
`base32(SHA-256(pub)[:10])` in lower case, 16 characters (the same derivation as AVF feeder IDs). Peer
operators exchange node IDs out of band; every `peers[]` entry names the node ID that must be proved.

## 3. Handshake

Messages are `u32 length | JSON`, length 1..65536 (AVF.md §3 framing), uncompressed, in this order:

1. **Hello** (server → client): `{"proto":1,"nodeId","name","nonce":<32 bytes b64>,"timeMs","software"}`.
   The client aborts if `nodeId` is not the configured peer ID or `proto` ≠ 1.
2. **Auth** (client → server): `{"body":<bytes>,"sig":<64 bytes>}`, body = JSON
   `{"pub","name","software","version","nonce":<client nonce, 32 bytes>,"peer":<server node ID>}`,
   `sig = Ed25519(clientKey, "AVFED1/C" ‖ serverNodeID ‖ serverNonce ‖ ekm ‖ SHA-256(body))`.
3. **Verdict** (server → client): `{"status":"ok","auth":{body,sig}}` with body
   `{"pub","name","software","version","peer":<client node ID>}` and
   `sig = Ed25519(serverKey, "AVFED1/S" ‖ clientNodeID ‖ clientNonce ‖ ekm ‖ SHA-256(body))`,
   or `{"status":"denied","code","reason"}` and close. Codes: `unknown_peer`, `disabled`, `bad_auth`.

`ekm` = TLS exporter, label `EXPORTER-AirView-AVFED1`, empty context, 32 bytes. The server derives the
client's node ID from `pub` after verifying the signature and admits it only if it is a configured,
enabled peer. The client checks that the server's signed `pub` yields the configured node ID. A client
that was denied waits 5 minutes before dialling again; other failures back off 1 s doubling to 60 s
(±20 %). Handshake timeout: 10 s.

**Duplicate links.** When both sides dial, each side may briefly hold two links to the same peer. Both
keep the link **dialled by the lower node ID** (string order) and close the other with GOODBYE
`duplicate`; a newer link from the same dialler replaces the older one. A side that holds a link does not
dial.

## 4. Records

After the Verdict each direction is one raw DEFLATE stream (AVF.md §4.1: level 1 compressor, sync flush
every 50 ms, at 32 KiB pending and after control records; the reader enforces a decompression guard of
`maxInflateBps` (default 16 MB/s over 10 s) and a 200:1 ratio per MiB). Inside it, records are
`u8 type | uvarint len | payload`, payload ≤ 64 KiB. Unknown types are skipped by length. Both
directions use the same types:

| Type | Name | Payload |
|---|---|---|
| 0x20 | INFO | JSON `{"name","software","version","aircraft","mlat","transit","intervalMs","delayS"}`: what the sender will send (informational) |
| 0x21 | STATE | aircraft batch (§5) |
| 0x22 | MLAT | MLAT result batch (§6) |
| 0x30 | PING | `u64 id | i64 sendNs` (big-endian), every 5 s from both sides |
| 0x31 | PONG | the PING payload echoed (RTT = now − sendNs) |
| 0x3F | GOODBYE | JSON `{"code","reason"}`: `shutting_down`, `duplicate`, `protocol_error`, `disabled` |

A side closes the session after 30 s without any record. A malformed record is a protocol error: the
receiver sends GOODBYE `protocol_error` and closes.

### Path

`u8 n (1..16) | n × 16 ASCII bytes` (node IDs). The **origin** network is first, the **sending** network
last. Receiver rules, in order (each drop is counted in the status):
1. last entry ≠ the authenticated peer → drop the record (`pathSpoofDropped`);
2. own node ID on the path, or a node twice → drop (`loopsDropped`);
3. more than `maxHops` entries → drop (`hopLimitDropped`).

Sender rules for forwarding (transit): only when the sender's `transit` and that peer's `send.transit`
are on; never to a node on the path (split horizon); only when path length + 1 ≤ `maxHops`; never for an
aircraft the sender's own stations hear; the sender appends its own ID.

## 5. STATE

```
uvarint tMs            sender clock, Unix ms, at the snapshot
u8      flags          bit 0: full snapshot (periodic full refresh)
path
uvarint n              ≤ 4096
n × item
```

Item:
```
u32     addr           bits 0-23 address, bit 24 non-ICAO ("~" address)
uvarint mask           field presence bits (below)
u8      source         0 "", 1 adsb, 2 mlat, 3 tisb, 4 adsr, 5 modes, 6 other
uvarint lastAgeMs      tMs − LastSeen
uvarint firstAgeS      (LastSeen − FirstSeen) / 1000
fields in bit order:
  0 callsign str   1 squawk str   2 category str   3 addrType str
  4 position: i32 lat·1e7 | i32 lon·1e7 | uvarint posAgeMs
  5 altBaro svarint ft   6 altGeom svarint ft   7 vertRate svarint ft/min
  8 gs uvarint 0.1 kt   9 track uvarint 0.01°   10 ias uvarint kt   11 tas uvarint kt
  12 mach uvarint 0.001   13 magHeading uvarint 0.01°   14 roll svarint 0.01°   15 trackRate svarint 0.01°/s
  16 selHeading uvarint 0.01°   17 selAlt svarint ft   18 qnh uvarint 0.1 hPa
  19 wind: uvarint dir 0.1° | uvarint speed 0.1 kt   20 oat svarint 0.1 °C
  21 adsbVersion u8   22 nic u8   23 nacp u8   24 nacv u8   25 sil u8   26 rc uvarint m
  27 emergency str   28 navModes uvarint bits (autopilot, vnav, althold, approach, lnav, tcas)
  29 mlat: str grade | uvarint sigma m + 1 (0 = none)   30 onGround u8   31 flags u8 (alert, spi, military, coarse)
str = uvarint len (≤ 64) | UTF-8 bytes
```

All times are relative to `tMs`, so the receiver rebases them to its own clock (`local now − tMs`) and
the two networks' clocks need not agree. A sender sends the aircraft whose LastSeen changed since its last
STATE to that peer, and everything (flag `full`) every `fullRefreshS` (default 30 s) and after a dropped
batch. There is no explicit removal: the receiver expires an aircraft `ttlS` (default 60 s) after its
last report.

The fields are exactly the public map's (publicdto.Aircraft) minus reference data, routes and
prediction: no field for station positions, distances, bearings, receiver lists, signal levels, feeder
IDs, SIDs or addresses exists.

## 6. MLAT

```
uvarint tMs | path | uvarint n (≤ 4096) | n × fix
fix: u32 addr | svarint dtMs (fix time − tMs) | i32 lat·1e7 | i32 lon·1e7 | svarint hae dm
     | uvarint sigmaH dm | uvarint sigmaV dm | u8 grade | u8 nBucket | u8 fixFlags | u8 wflags
     [ wflags bit 0: 3 × svarint ENU velocity cm/s ]
```

Only displayable grades (A-C) are sent and no hold-out fixes. `nBucket` is the station count bucket of
`model.MLATFix` (3, 4, 5-8, 9+); no station identity exists in the record.

## 7. Versioning

`proto` is 1. Additions within revision 1: new record types (skipped by old receivers), new trailing
fields of JSON records. A new STATE field needs a new mask bit, which old receivers cannot skip; such
a change needs revision 2.
