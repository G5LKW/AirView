# internal/uplink — wiring notes (WP-U)

AVF/1 feeding client (docs/aggregator/AVF.md, DESIGN §4.5, §12). Built on `internal/avf`
(codec, TLS, handshake, record stream); imports `internal/feed` (clock events, magic timestamp,
Radarcape status), `internal/model`, `internal/modes`, `internal/mlat/geo`. Consumers:
`internal/engine` (airview standalone / aggregator with `Config.Uplinks`), `cmd/airview-feeder`,
`internal/feederui`, `internal/server` (status route).

## 1. Lifecycle

```go
m, err := uplink.NewManager(uplink.Options{
    Software: "airview",                 // or avf.SoftwareFeeder in airview-feeder
    Version:  model.Version,
    StateDir: filepath.Join(dataDir, "uplink"), // identity.key (0600), uuid, uplinks/<id>/{state.json,spool/}
    Source:   src,                       // see §2
    Inject:   trackerSink,               // MLAT results -> local tracker (model.FrameSink); nil = not injected
    OnResults: nil,                      // airview-feeder: republish as Beast with the MLAT magic timestamp on 30105
    Logf:     log.Printf,
    Now:      ntp.Now,                   // host UTC (NTP corrected)
    HostErrNs: func() uint64 { ... },    // NTP error estimate for CLOCK_PAIR (default 2 ms)
})
m.Apply(cfg.Uplinks)                     // on start and on every config change (normalised copies are taken)
...
m.Close()                                // shutdown: flush, up to 2 s for ACKs, GOODBYE, rest -> spool
```

`Apply` restarts an uplink only when a connection setting changes (host, port, pin, enrol token,
spool size, shareGNSS, returnMLAT, disabled); inputs, location, privacy and timestamps apply in
place (a fresh RECEIVER is sent). The feeder identity is shared by all uplinks
(`m.Identity().FeederID`, `.UUID` are public; the key never leaves the package).
`uplink.NewIdentity(stateDir)` implements `airview-feeder new-identity` (keeps the UUID).

## 2. Inputs

- **Frames:** the Manager is a `model.FrameSink`; add it to the feed manager's tee
  (`tee{tracker or ingest, hub, uplinkManager}`, DESIGN §1). `HandleFrame` costs ~110 ns per
  frame (i9-13900K, 0 allocs) and never blocks on the network.
- **Clock epochs:** `feedManager.OnClockEvent(m.HandleClockEvent)` (events arrive before the
  first frame of the new epoch; a missing event is synthesised from the frame's epoch).
- **GNSS:** `feedManager.OnReceiverPosition(func(id string, p feed.ReceiverPosition) {
  m.HandleGNSS(id, p.Lat, p.Lon, p.AltM, model.AltRefHAE /* or MSL, per receiver */, ntp.Now()) })`.
  Sent only with `Uplink.ShareGNSS`, at most every 10 s, never for `ReceiverInfo.AliasGNSS`.
- **Decoded updates:** `HandleUpdate` forwards untimed SBS/JSON updates (DECODED) only from feeds
  listed explicitly in `Uplink.Inputs`; MLAT updates and injected results are always refused.
- **Source** (`uplink.Source` / `uplink.SourceFuncs`):
  - `Forwardable(feedID)` → `feedManager.Forwardable(feedID)` (configured, not MLATOnly, not
    NoForward, not demo, not hub). Called per frame: it must be cheap.
  - `Receiver(feedID)` → `ReceiverInfo` built from the feed config and station settings:
    `RxNo` (stable 1..255; 0 lets the uplink assign and persist one), name, decoder string,
    channels, `Location` (`avf.Location`, sent only with `SendLocation`), antennas
    (`avf.Antenna{ID, Kind, AglM}`), input status (type, connected, fps, CRC errors, since),
    `AliasGNSS`. Called on first sight and every 60 s; must not block. Never put host names,
    LAN IPs or account names into it (AVF §5.4).

## 3. Status API for UIs

`Manager.ServeHTTP` — mount behind admin auth (airview: e.g. `GET/POST /api/uplinks`; feeder
page: `/api/status/uplinks`):

- `GET` → `{"feederId","uuid","uplinks":[Status...]}`. `Status` (status.go) carries state
  (`idle|disabled|connecting|handshake|connected|backoff|stopped`), last error + AVF error code,
  next retry, server name, admission, approval (`approved|pending`), mode, MLAT/results flags,
  MOTD, features, FLOW pause/slow factor, counters (frames in/queued/sent/late, untimed,
  mlatRefused, filtered, dropped, spilled, results in/injected, wire bytes, ackSeq), buffer
  (ring records/bytes/span, backfill queue, spool size/bytes) and per receiver (rxNo, feed,
  clock class, epoch, mode, last FEEDBACK) plus the last 10 NOTICEs. No secrets.
- `POST {"action":"claim","id":"<uplink>","rxNo":0}` → `{"code","expiresMs"}` (bearer secret:
  owner eyes only, never logged). `POST {"action":"retry","id":...}` reconnects now / leaves
  `stopped`. Go API: `m.Status()`, `m.Claim(ctx, id, rxNo)`, `m.Retry(id)`.

## 4. Buffering numbers

| Item | Default | Option |
|---|---|---|
| RAM ring | 10 min, 12 MiB | `RingAge`, `RingBytes` |
| In-memory backfill queue | 24 MiB | `BackfillBytes` |
| Disk spool | `model.ResolveSpoolMB` (0 on SD root, 256 MB Linux, 1024 MB Windows), 4 MiB segments, written only while disconnected | `Uplink.SpoolMB`, `SegmentBytes` |
| Resume window (resent live) | 30 s | `ResumeWindow` |
| Backfill rate | max(live/3, 64 KiB/s) uncompressed = ≤ 25 % of the link when busy | `BackfillMinBps` |
| Backoff | 1 s → 60 s ±20 %, reset after 60 s up | `BackoffMin`, `BackoffMax` |
| Liveness | 30 s without PING/ACK | `Liveness` |

Seq handling: records are held as bodies without seq and numbered when written; after WELCOME
everything above `ackSeq` is renumbered from `ackSeq+1` (a RECEIVER per receiver first, announcing
the epoch of its oldest queued record). This satisfies AVF §5.2's "RECEIVER first" and "no gap"
rules together (WP-P noted that replaying records unchanged after a fresh RECEIVER would leave a
gap). `avf.SendRing` is therefore not used: the uplink's queue also has to feed the disk spool.

## 5. Tools and tests

- `internal/uplink/avftest`: in-process AVF/1 server stub (real wire format via `internal/avf`)
  that records everything and reports protocol violations.
- `go run ./internal/uplink/cmd/avfstub -listen 127.0.0.1:30160 [-token T] [-outage-at 30s -outage-for 1m]`
- `go run ./internal/uplink/cmd/uplinksim -server 127.0.0.1:30160 -pin sha256/... -state DIR -spool 64`
  (simnet receivers through the uplink in real time; status on `-status`).
- Acceptance: `go test -run TestSimnetOutage30Min ./internal/uplink` (30-minute outage, spool:
  everything backfilled exactly once; RAM only: exactly the last 10 minutes).

## Integrator additions (after Wave 1A)

- Tunables are file-configurable under the top-level `uplinkTuning` block (`model.UplinkTuning`; also in feeder.json). Call `opts.ApplyTuning(cfg.UplinkTuning)` before `NewManager`. The key is `uplinkTuning` (not `uplink`) so that it cannot be confused with the `uplinks` list; env overrides are `AIRVIEW_UPLINK_TUNING_*`.

## 6. Beast outputs: "also feed others" (WP-UPLINK-multi, DESIGN §12.1 phase 5)

A station (airview standalone/aggregator or airview-feeder) can feed third-party aggregators that take
readsb-style Beast on TCP (for example a community readsb at `host:30004` with a UUID) **in addition to**
its AVF/1 uplinks. Code: `beast.go`, `beast_reduce.go`, `manager_beast.go`.

```go
notes := m.ApplyBeast([]uplink.BeastUplink{{
    ID: "community", Host: "in.example.org", Port: 30004,   // port default 30004
    Format: uplink.BeastReducePlus,  // "beast_reduce_plus_out" (default) | "beast_reduce_out" | "beast_out"
    UUID: "",                        // "" = per-uplink random UUID kept in <StateDir>/uplinks/<id>/uuid
    Inputs: nil,                     // local feed IDs; empty = every forwardable feed
    ReduceMs: 250,                   // 50..5000, default 250 (non-position data at 4x)
    NoTimestamps: false,             // true = all-zero Beast timestamps
}})
m.BeastStatus()                      // []BeastStatus, also "beast" in the GET status body
m.Retry("community")                 // works for Beast IDs too
```

- **Lifecycle:** like `Apply`. Call `ApplyBeast` on start and on every config change. A change of host, port,
  format, uuid or disabled reconnects; inputs, reduceMs and noTimestamps apply in place. IDs must be valid
  slugs and must not clash with an AVF uplink ID (such entries are skipped and returned as notes, which a
  config check can show). `Manager.Close` stops everything.
- **Wire (clean room, see CLEANROOM.md):** for `beast_reduce_plus_out`: `0x1A 0xE4` + the 36-character UUID
  (as written, padded with `f`), then `0x1A 'W' 'O'`. Then Beast frames `'1'`/`'2'`/`'3'` carrying the
  receiver's own 48-bit timestamp (12 MHz counter, Radarcape GPS second/ns, or the 12 MHz equivalent of a
  20 MHz SBS counter) and the signal byte. Reduce: per address, DF, type code and CPR format, one frame per
  interval for position/velocity data and one per 4 intervals for everything else. A `'P'` from the server is
  echoed verbatim. `'W' 'S'` doubles the interval for 60 s (up to 8x). An all-zero Mode A/C frame is sent as a
  keep-alive after 30 s of silence. DF11/17/18 frames with a CRC error are not forwarded.
- **Live only.** While disconnected, frames are dropped and counted (`dropped`). Nothing is spooled or
  backfilled, because third-party aggregators expect live data. Reconnects use the same 1 s → 60 s ±20 %
  backoff as AVF.
- **Never forwarded** (as for AVF): MLAT results (MLAT flag or magic timestamp, `mlatRefused`), late
  frames, frames of feeds that are not `Source.Forwardable` (MLATOnly, NoForward, demo, hub), and the
  uplinks' own injected results (`uplink/<id>` feed IDs).
- **Privacy:** the UUID is the station's identity at that aggregator. By default each Beast output gets its
  own random UUID, so the native feeder UUID (`Identity.UUID`) is never given to third parties. The UUID
  appears in `BeastStatus` (status API: owner/admin eyes only, like the rest of it). The connection is
  plain TCP, as with readsb itself. The UI should say so next to the setting: the data and the UUID go
  unencrypted to an operator outside this network, under that operator's privacy terms. A new consent text,
  modelled on CONSENT.md C5, is requested in HOOKS.md.
- **Running alongside an external mlat-client:** nothing to do in AirView. mlat-client connects to the same
  receiver (readsb 30005 or the Radarcape 10003) as before. Set its `--uuid` to the UUID of the Beast output
  to the same aggregator (copy it from the status page, or configure `uuid` explicitly), so that aggregator
  joins the two links into one station. Its results go back to readsb (30104). If the feeder's input is that
  readsb, results reappear on 30005 with the MLAT magic timestamp, and every uplink refuses them.
- **Cost:** `BenchmarkBeastHandleFrame` is about 33 ns per frame, with 0 allocations, on top of AVF's ~110 ns.
- **Tests:** `TestDualUplinkAVFAndBeast` (AVF stub plus AirView's own `beastin` 30004 server, exact reduced
  set against an independent replay, UUID attach, ping echo, never-forwarded rules), `TestBeastPreambleSlowDownReconnect`,
  `TestBeastFormatsAndApply`, `TestBeastReducer`, `TestBeastTimestamp`, `TestBeastNormalize`.
- **Live check:** `uplinksim -server <avf> -pin <pin> -beast 127.0.0.1:<port> [-beast-format ...] [-beast-uuid ...]`
  against `beastin-load -serve 127.0.0.1:<port>`. `-server ""` runs the Beast output alone.

### Config shape (requested in HOOKS.md; not wired yet)

`uplinks[]` entries get `"type": "avf"` (the default, so existing files are unchanged) or `"beast"`. A Beast
entry uses `id`, `host`, `port` (default 30004), `inputs`, `timestamps` (default true, mapped to
`!NoTimestamps`) and `disabled` from the existing fields, plus `format`, `uuid` and `reduceMs`. The engine and
the feeder split the list: AVF entries go to `Apply`, Beast entries (converted to `uplink.BeastUplink`) go to
`ApplyBeast`.

## 7. MINUTE_STATS and the selective-mode merge (WP-I2)

The feeder side of the merge implemented in `internal/ingest/avfsrv/minutes.go`:

- `MINUTE_STATS` counts every forwardable frame before the selective/FLOW filter (unchanged).
- When a minute ends, the receiver's open FRAMES batch is closed **before** the MINUTE_STATS record is
  queued. The server has then seen all of that minute's frames when the record arrives (`TestMinuteStatsOrderAndCloseFlush`
  fails if this is removed).
- `Client.close` (shutdown, or a restart after a connection-setting change) sends the minute in progress as
  it stands. The next run sends the rest of that minute, and the server adds the parts up (ICAOs becomes a
  lower bound).
