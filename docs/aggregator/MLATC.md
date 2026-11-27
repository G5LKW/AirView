# The mlat-client protocol (as implemented by AirView on TCP 31090)

**Status:** implemented by `internal/ingest/mlatc` (WP-B3). This is AirView's own description of the wire
protocol spoken by the community **mlat-client** program, written for interoperability. It was compiled from
the protocol's documented behaviour, the facts listed in RESEARCH.md §1.5/§3, and — under CLEANROOM rule 4 —
a reading of the client's protocol handling for individual wire facts (recorded in CLEANROOM.md §4, WP-B3 row).
No code was copied; field names and constants below are interoperability facts. Items marked **(to confirm)**
are inferred and should be checked against a capture of the user's own client (DESIGN §13.3).

---

## 1. Roles

- **Client:** runs next to a receiver, reads its Beast (or SBS/AVR) stream, decodes enough to pick
  synchronisation and multilateration candidates, and forwards them with the receiver's timestamps.
- **Server:** groups the same transmission across clients, keeps the receivers' clocks synchronised, solves
  positions and sends results back to the clients that contributed.
- One TCP connection per receiver. Default port **31090**. Optional UDP side channel (not offered by AirView;
  §9).

## 2. Handshake

### 2.1 Client hello
The first line the client sends is one JSON object followed by about 128 spaces and `\n`. AirView reads it
with a 64 KiB limit and decodes every field tolerantly (booleans as `true`/`1`/`"true"`, numbers as numbers or
numeric strings).

| Field | Type | Meaning |
|---|---|---|
| `version` | int | protocol version; the current client sends 3 (AirView accepts 2 and 3) |
| `client_version` | string | software version (shown to the admin) |
| `compress` | list of strings (or one string) | compression methods the client accepts: `none`, `zlib`, `zlib2` |
| `selective_traffic` | bool | the client honours start/stop_sending |
| `heartbeat` | bool | the client sends heartbeats |
| `return_results` | bool | the client wants results |
| `return_result_format` | string | `ecef` (current) or anything else (legacy lat/lon format) |
| `return_stats` | bool | the client wants `stats` messages |
| `udp_transport` | false or int | `2` = the client can send timing data over UDP |
| `uuid` | string or null | feeder UUID (28-32 hex digits after removing dashes and braces) |
| `user` | string | display name chosen by the feeder owner |
| `lat`, `lon`, `alt` | number | declared receiver position; `alt` in metres, reference (MSL or ellipsoid) unspecified |
| `clock_type` | string | `dump1090` (default), `beast`, `radarcape_12mhz`, `radarcape_gps`, `sbs`, `unknown`/`auto` |
| `privacy` | bool | hide the receiver from public views |

AirView maps `clock_type` to a clock class per DESIGN §5.2 (`radarcape_gps` → 1, `beast`/`radarcape_12mhz` →
4, `sbs` → 7, everything else → 5), stores the altitude with reference `ambiguous`, treats `0,0` as "no
position", and turns `privacy:false` into the region-pin consent of DESIGN §19.2 (absent = neither).

### 2.2 Server reply
One JSON line, uncompressed:

| Field | AirView sends | Client behaviour |
|---|---|---|
| `compress` | `zlib2` if offered, else `zlib`, else `none` | any value it did not offer is fatal |
| `udp_transport` | `false`, or `[host, port, key]` when `aggregator.mlat.udp` is on and the client offered `2` (§9; host/port from `aggregator.mlatc.udpHost` / `udpPort` behind NAT) | the triple switches sync, ssync and mlat data to UDP |
| `split_sync` | `true` by default (option `SplitSync` = `aggregator.mlatc.splitSync`: `"auto"` default with a per-client fallback to `false`, `"on"`, `"off"`) | `true` makes the client send single sync messages (`ssync`) |
| `motd` | aggregator name, if set | shown in the client's log (at most daily) |
| `deny` | list of reason strings, instead of the above | the client logs them and disconnects |
| `reconnect_in` | seconds, with `deny` | overrides the client's reconnect delay |

## 3. Framing and compression

- **none:** newline-delimited JSON in both directions.
- **zlib:** the client's direction is compressed, the server's is plain.
- **zlib2:** both directions are compressed.

A compressed direction is **one continuous zlib stream** (zlib header once, deflate level 1 at the client)
cut into chunks. After each batch of lines the sender performs a sync flush, removes the 4 bytes
`00 00 FF FF` that end every sync flush, and sends `u16 big-endian length` + chunk (chunks stay below 64 KiB;
the client starts a new one at about 32 KiB). The receiver appends `00 00 FF FF` to each chunk and feeds the
result to one persistent inflater; the inflated bytes are newline-delimited JSON. Lines may span chunks.

AirView's server direction uses **stored** deflate blocks (zlib header `78 01`; per chunk a non-final stored
block `00 LEN NLEN data`, then the first byte `00` of an empty stored block whose remaining `00 00 FF FF` is
stripped). Any inflater decodes it; it needs no compressor state (DESIGN §20 scale). AirView also accepts a
raw deflate stream without the zlib header from a client.

## 4. Client messages

Every line is one JSON object with (normally) one key.

| Message | Content | AirView |
|---|---|---|
| `{"sync":{"et":T,"em":"hex","ot":T,"om":"hex"}}` | an even (`e`) and odd (`o`) DF17 airborne position of one aircraft with their timestamps; sent when the second half arrives (within 5 s of the first) | two timed frames |
| `{"ssync":{"t":T,"m":"hex"}}` | one sync candidate (only after `split_sync:true`) | one timed frame |
| `{"mlat":{"t":T,"m":"hex"}}` | one Mode S frame (DF 0/4/5/11/16/20/21) of a requested aircraft without a good ADS-B position | one timed frame |
| `{"seen":["abcdef",…]}` / `{"lost":[…]}` | aircraft the client started / stopped tracking (6 hex digits) | selection set, planner hook |
| `{"rate_report":{"ABCDEF":1.25,…}}` | ADS-B position message rate per aircraft (msgs/s); an empty one after connecting | planner hook |
| `{"clock_reset":{"reason":"…"[,"frequency":F,"epoch":"utc_midnight"\|null,"mode":"…"]}}` | the timestamp source changed; with `mode`/`frequency` after a decoder mode change, reason only after a day rollover | new clock epoch (§5.3) |
| `{"clock_jump":true}` | the client saw its receiver's timestamps jump | new clock epoch |
| `{"input_connected":"connected"}` / `{"input_disconnected":"disconnected"}` | the client's link to its receiver | reconnect = new clock epoch |
| `{"heartbeat":{"client_time":S}}` | liveness, every ≤ 120 s; wall-clock seconds | liveness only |
| `{"position_update":{…}}` **(to confirm)** | a Radarcape's own GNSS position | lat/lon/alt passed to a hook if present |

Hex payloads are 14 or 28 digits (Mode S short/long; 4 for Mode A/C). Timestamps `T` are non-negative
integers (AirView also accepts integral floats). Anything unknown is skipped; a line that is not valid JSON
counts as malformed.

## 5. Timestamps

### 5.1 Units per clock type
The client reports the receiver's time of the **start of the frame**, derived from its raw stamp:

| clock_type | Units | Client value t from the receiver stamp s |
|---|---|---|
| `dump1090`, `beast`, `radarcape_12mhz`, `unknown` | 12 MHz counter (48 bit) | t = s − 768 (Mode S), s − 244 (Mode A/C); 0 if negative |
| `radarcape_gps` | nanoseconds of the UTC day | s is `seconds<<30 \| nanoseconds`; the client fixes the legacy "day second + 1" firmware (status bit), then t = (sec·10⁹ + ns − 64 000 / 120 000 / 20 300) mod 86 400·10⁹ |
| `sbs` | 20 MHz counter | 24-bit stamp **plus** 1 120 (short) / 0 (long) / 1 920 (Mode A/C), masked to 24 bit, then widened by keeping the previous value's upper bits and adding 2²⁴ when the result went down |

### 5.2 AirView normalisation
`internal/ingest/mlatc` adds the offsets back, so a 31090 timestamp equals the receiver stamp an AirView feeder
would send on AVF/1 (12 MHz: unwrapped 48-bit counter; GPS: Unix nanoseconds, the day chosen within ±12 h of
the server clock; SBS: the widened counter at the stamping point). The clock filter's frame-reference table
then applies to both protocols. Proven against simulator truth for all device types in
`TestSimnetRealClientConvention`.

### 5.3 Epochs
A new epoch starts at every connection, after `clock_reset` (except the day rollover of a GPS clock, which is
continuous in Unix nanoseconds), `clock_jump`, a receiver reconnect, and server-side when a tick is > 30 s
behind the epoch's maximum or > (elapsed + 90 s) ahead. A `clock_reset` naming the emulated Radarcape mode (no
GPS lock) switches the class to `gps_emul`; returning to the GPS mode switches back to `gps_utc`.

## 6. Server messages

| Message | Content |
|---|---|
| `{"start_sending":["abcdef",…]}` / `{"stop_sending":[…]}` | aircraft the client should (no longer) forward; the client sends nothing for an aircraft that was never requested |
| `{"result":{…}}` | one MLAT position (§6.1) |
| `{"stats":{"peer_count":N,"outlier_percent":P,"bad_sync_timeout":S}}` | the station's sync quality (never a peer list) |
| `{"heartbeat":{"server_time":S}}` | every 30 s; the client disconnects after 60 s without any data |

### 6.1 Results
Common fields: `"@"` transmit time in Unix seconds (float), `"addr"` 6 hex digits; optional `"ewvel"`,
`"nsvel"` (knots) and `"vrate"` (ft/min) — the client passes them unchanged into SBS speed/track/vertical-rate
fields; optional `"callsign"`, `"squawk"`.

- **ecef:** `"ecef":[x,y,z]` metres, `"cov":[xx,xy,xz,yy,yz,zz]` m² (the client's error estimate is
  √(xx+yy+zz)), `"nd"` station count.
- **legacy:** `"lat"`, `"lon"` degrees, `"alt"` feet, `"gdop"` (the client's error estimate is gdop × 300 m;
  AirView sends √trace/300), `"nstations"`.

AirView sends results only to the stations that contributed (DESIGN §8.4); the station count is the lower bound
of the fix's count bucket unless the engine gives the exact count.

## 7. Selection

In AirView's full traffic mode every aircraft in a client's `seen` set is requested (capped per client); in
selective mode only the MLAT planner's sync and MLAT interest sets are. A `lost` aircraft is forgotten by the
client, so no `stop_sending` is needed for it.

A station in timing quarantine (new stations, `internal/registry/trust`; `aggregator.mlat.quarantine`) is also asked
for a small probe set of ADS-B aircraft (`Options.Trust`), so the gate can compare its timing with trusted stations
before its copies reach MLAT. `rate_report` values (ADS-B position rates per aircraft) are kept per client
(`Server.ADSBRate` / `ADSBRates`, `LinkStats.ADSBRate`); the engine passes the aircraft with a non-zero rate to the
planner as heard by that station (`Hooks.OnRates`), so the station can be ranked for sync sets before any group of
the aircraft arrives.

## 8. Measured behaviour and open points

- **split_sync.** With pairs, the first half of a pair reaches the server only when the second arrives (up to
  5 s later) and may be sent twice. In the simulator end-to-end test (10 stations, 60 s) 86 % of multi-station
  sync transmissions reached the MLAT grouper with all copies in paired mode versus 99.98 % with split sync;
  Mode S `mlat` frames were 99.8 % complete either way. Split sync is therefore offered by default
  (`"auto"`, H-mlatc): half the sync traffic of clients that pair overlapping messages, hold-out unchanged within
  noise (2 worlds × 3 seeds), clients that ignore the flag keep working. A client whose `ssync` messages are
  unusable (malformed, or no sync data while unknown messages arrive) is disconnected once and answered without
  `split_sync` (per-identity fallback, `Server.SplitSyncFallbacks`). `"off"` restores the reference default. A
  capture of the reference client is still needed to confirm its `ssync` timing.
- **(to confirm)** the key and fields of the Radarcape position update; the result flags for anonymised and
  Mode A/C results (AirView never sends either).
- The client drops its own frames whose timestamps go backwards; SBS widening can produce false 2²⁴ steps if
  frames arrive out of order (not seen with real SBS-1 output, which is in stamp order).

## 9. UDP transport

After `[host, port, key]` the client sends sync, ssync and mlat data as datagrams; everything else stays on TCP.
Datagram: header u32 key, u16 sequence number, u64 base timestamp (big-endian, 14 bytes), then messages until
the end of the datagram:

| Type | Message | Body | Size |
|---|---|---|---|
| 1 | sync | i32 even delta, i32 odd delta, 14 + 14 bytes | 37 |
| 2 | mlat short | i32 delta, 7 bytes | 12 |
| 3 | mlat long | i32 delta, 14 bytes | 19 |
| 4 | ssync | i32 delta, 14 bytes | 19 |
| 5 | rebase | u64 new base | 9 |
| 6 | absolute sync | u64 et, u64 ot, 14 + 14 bytes | 45 |

Timestamp = base + delta = the JSON value. Datagrams are flushed at about 1,400 bytes. A Mode A/C reply travels
as type 3 padded with 12 zero bytes (to be confirmed against a real client capture). AirView: a random key is
live after the handshake; the source address must equal the TCP client's (`aggregator.mlatc.udpAnySource` lifts
this for carrier-grade NAT); duplicates within 64 sequence numbers are dropped; loss is estimated from gaps; a
datagram is never answered; idle = no data on either path for 150 s. `TestUDPInterop` proves the path
equivalent to the JSON lines. Settings: `aggregator.mlat.udp` (on/off), `aggregator.mlatc.udpHost`, `udpPort`,
`udpListen`, `udpAnySource`, `udpReaders`; a change restarts the mlat-client listener.

## 10. Mode A/C requests (AirView extension)

`{"start_sending_modeac":["0a12",...]}` / `{"stop_sending_modeac":[...]}`; entries are the 2-byte reply value as
4 hex digits (as in `m`), `"*"` = every reply. They are sent only to clients whose handshake has `"modeac": true`
(or to every client with `aggregator.mlatc.modeAcAllClients`). Replies are `mlat` messages with a 4-digit `m`
(TCP) or the padded long frame (UDP). With `aggregator.mlat.modeAc` the MLAT planner asks exactly its Mode A/C
stations for every reply (DESIGN §9.9).
