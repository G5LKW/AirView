# internal/feed: wiring notes for the network work packages (WP-F)

`internal/feed` runs the configured inputs (Beast, AVR, SBS, JSON, hub, demo) and delivers
`model.RawFrame` / `model.DecodedUpdate` into one sink. WP-F added what the aggregator, the
uplink and the Beast ingest need. Standalone behaviour, the legacy `GPSNs` / `Clock12MHz` fields,
`FeedStatus` and every existing API are unchanged.

## 1. Beast extension messages (for `internal/ingest/beastin`, DESIGN §5.1)

```go
p := feed.NewBeastParser(dec.Handle)          // frames: '1' '2' '3' '4' '5' as before
p.OnEscape = func(typ byte, body []byte) {    // body aliases the parser buffer: copy to keep
    switch typ {
    case feed.BeastUUID:       uuid, ok := feed.ParseBeastUUID(body)      // 36 B, normalised, null UUID rejected
    case feed.BeastReceiverID: id, ok := feed.ParseBeastReceiverID(body)  // 8 B big-endian; honour only for TrustedRelays
    case feed.BeastPing:       v, _ := feed.ParseBeastPing(body)          // 24-bit ms
                               conn.Write(feed.AppendBeastEscape(nil, feed.BeastPing, body)) // echo verbatim
    case feed.BeastOption:     // body[0]: feed.BeastOptPing ('O'), feed.BeastOptSlowDown ('S'), ...
    default:                   // body == nil: unknown escape type, already skipped
    }
}
```

- Nothing a peer sends makes the parser fail. Unknown escape types are counted (`p.Unknown`)
  and skipped; the parser resynchronises on the next frame start. Never drop a link for content.
- Counters: `Frames`, `Skipped` (complete extension messages), `Unknown`, `Garbage`, `Truncated`.
- Encoders: `AppendBeastEscape`, `AppendBeastPing`, `AppendBeastOption` (e.g. `'S'` to slow a
  sender down), `AppendBeastUUID`, `AppendBeastReceiverID`. Own pings: `BeastPingValue(now)` to
  send, `BeastPingRTT(echoed, now)` for the round trip (modulo the 24-bit counter).
- `NormalizeUUID` is the same normalisation for UUIDs from other sources (mlat-client handshake).
- **30004 timestamps are for latency and stats only** (DESIGN §5.1). The decoder still stamps
  `ClockCls`/`Ticks`; beastin must not pass them to clock sync, MLAT or survey.

## 2. Receiver clock: class, epochs and ticks (for the uplink and the ingest, DESIGN §4.3)

Every frame from a Beast or AVR feed now carries, in addition to the legacy fields:

| Field | Meaning |
|---|---|
| `HostNs` | host UTC receive time, Unix ns (`Recv.UnixNano()`; NTP-corrected via `ntp.Now`) |
| `ClockCls` | `model.ClockClass`; `ClockNone` = no usable receiver time for this frame |
| `Epoch` | clock epoch (never 0 when timed) |
| `Ticks` | unwrapped receiver time of the epoch in `ClockCls.NominalHz()` units |

- GPS classes: `dayIndex·86400e9 + nsOfDay` after the legacy 1..86400 correction. The first
  frame of an epoch takes the day from host UTC (±12 h, `model.GPSTicks`); later frames keep
  continuity, so midnight rolls over and a host clock step does not move ticks. For `gps_utc`
  the ticks are UTC Unix ns; for `gps_gpst` GPS-time ns (18 s ahead).
- 12 MHz classes: the 48-bit counter unwrapped. SBS 20 MHz (no input parser yet): use
  `feed.ClockTracker` with `ClockSBS20` directly (24-bit unwrap bridged by host time).
- Untimed frames: MLAT results (magic timestamp or `MLATOnly`), all-zero timestamps, frames
  before the timestamp mode is known, class `none`, invalid GPS fields, and frames of a step
  that is not confirmed yet (see below).

**Classes** (`Timestamper.DetectedClass`): Radarcape status frames are authoritative: locked + UTC
+ FPGA sync = `gps_utc`; locked with the UTC bit clear = `gps_gpst`; otherwise `gps_emul`
(including old firmware that does not report GPS detail). Losing lock is immediate; regaining it
needs `GPSRegainStatuses` (3) consecutive good status frames. 12 MHz with status = `hw12`; 12 MHz
detected statistically = `sdr12`; GPS detected statistically = `gps_emul`. The feed's
`model.Feed.ClockClass` overrides the detected class when it has the same tick unit (or is
`none`); a mismatched override is ignored and shown in `FeedStatus.Info`.

**Epochs.** A new epoch is announced by a `feed.ClockEvent` **before** the first frame that
carries it:

| `Reason` (wire) | `Detail` | When |
|---|---|---|
| `restart` | `start` / `reconnect` | first epoch of a feed in this process / after the input reconnected (IDs continue +1 per feed ID) |
| `jump` | `step` | receiver time ran ahead of host time by more than `model.EpochStepLimit` (90 s) |
| `backwards` | `backwards` | receiver time went back by more than 100 ms (e.g. a counter reset) |
| `mode_change` | `gps_lost`, `gps_regained`, `time_base`, `day_range`, `class_change`, `override` | class or timestamp meaning changed |
| `sample_drop` | `sample_drop` | `ClockTracker.SampleDrop` (for SDR inputs; none of today's inputs declares drops) |

A step is confirmed only after `ConfirmFrames` (3) consecutive frames agree with each other and
not with the old epoch; until then those frames are untimed. A single corrupt timestamp
therefore never costs an epoch (`ClockTracker.Glitches` counts them).

Hooks:

```go
fm.OnClockEvent(func(feedID string, ev feed.ClockEvent) { ... })  // feed goroutine, before the frame
cs, ok := fm.Clock(feedID)  // feed.ClockState{Class, Detected, Epoch, Events, Last, Escapes, UnknownEscapes}
```

The uplink builds `CLOCK_EVENT{rxNo, oldEpoch: ev.OldEpoch, newEpoch: ev.NewEpoch, reason: ev.Reason,
samplesLost: ev.SamplesLost, hostNs: ev.HostNs}` from the event, and `CLOCK_STATUS` from the
`FrameStatus` frames (raw payload in `Data`, decode with `feed.ParseBeastStatus`). Ordering
holds as long as the event and the frames go through one ordered path (both arrive on the feed's
goroutine). Custom runners (`RegisterType`) that produce timed frames report epochs with
`Session.ReportClockEvent`.

## 3. Forwarding rule (uplink tee, DESIGN §1 / AVF §5.5)

```go
if !fm.Forwardable(f.FeedID) { drop }          // configured, not MLATOnly, not NoForward, not demo, not hub
ok, timed := feed.ForwardableFrame(&f)          // MLAT results and status frames: never; untimed: only with tsInvalid
```

`Forwardable` is a lock-free map lookup refreshed on every `Apply`. Unknown feed IDs (e.g. host
feed IDs kept by a hub feed) are not forwardable. Demo (simulator) and hub (another station's
data) feeds are excluded even without `NoForward`.

## 4. Tests and fuzzing

```
go test ./internal/feed
go test ./internal/feed -run=^$ -fuzz=FuzzBeastParser -fuzztime=30s -fuzzminimizetime=50x   # also FuzzBeastDecoderClock,
    # FuzzClockTracker, FuzzAVRParser, FuzzSBSLine, FuzzJSONDecoder, FuzzNormalizeUUID
go test ./internal/feed -run=^$ -bench=BeastDecode -benchmem
```

`-fuzzminimizetime=50x` keeps the progress counter honest: with the default time-based
minimisation the fuzzer reports 0 execs/s while it minimises a new corpus entry.
Benchmarks (i9-13900K): `BeastDecode` 42 µs and `BeastDecodeTimed` (with class, epoch and
ticks) 57 µs per 1000 long frames, one allocation per frame (the owned `Data` slice) in both.
