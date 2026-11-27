# internal/sdr — wiring

Owner: WP-SDR-rtl. Status: package complete and tested; the feeder CLI
wiring is requested in docs/aggregator/HOOKS.md ("WP-SDR-rtl ->
cmd/airview-feeder").

## 1. Data path

```
dongle --USB/WinUSB--> rtl_tcp (user's own, GPL, child process or separate)
       --TCP 127.0.0.1:1234--> sdr.Client --> sdr.Demodulator
       --> sdr.Receiver --> model.FrameSink (feeder uplink, BeastServer, ...)
```

Frames leave the Receiver as `model.RawFrame` with

| field | value |
|---|---|
| `ClockCls` | `model.ClockSDR12` |
| `Clock12MHz` | `Ticks & (2^48-1)` (Beast-compatible) |
| `Ticks` | full 64-bit sample-clock ticks of the epoch |
| `Epoch` | new on every (re)connect and on server-side sample drops |
| `Signal` | mean pulse amplitude, dBFS (≤ 0) |
| `GPSNs` | -1 (no GPS time) |
| `HostNs` | wall time of the frame, estimated from the read time |
| `Channel` | -1 |

A new epoch is reported through `Options.OnClockEvent`; consumers that
unwrap timestamps (MLAT) must not difference ticks across epochs. Local
drops (the demodulator could not keep up) do **not** start an epoch: the
sample counter is advanced over the discarded bytes.

## 2. Feeder use (requested of cmd/airview-feeder, next wave)

```go
r := sdr.NewReceiver(sdr.Options{
    Addr: cfg.SDR.RTLTCP,        // "127.0.0.1:1234"
    Gain: cfg.SDR.Gain,          // "max" | "auto" | "40.2"
    PPM: cfg.SDR.PPM, BiasTee: cfg.SDR.BiasTee,
    SampleRate: cfg.SDR.SampleRate, // 2400000
    FeedID: feedID, Logf: logf,
    OnClockEvent: func(e uint32, why model.ClockEventReason) { ... },
})
go r.Run(ctx, sink)              // sink = the existing uplink FrameSink
```

Optionally `sdr.RunChild(ctx, sdr.ChildOptions{Path: cfg.SDR.RTLTCPPath,
Addr: cfg.SDR.RTLTCP, Device: cfg.SDR.Device}, nil)` in its own goroutine
when the user wants AirView to start rtl_tcp; `sdr.FindRTLTCP()` finds an
rtl_tcp.exe on PATH or next to the feeder. `Options.Validate`,
`sdr.ParseGain` and `DemodConfig.Validate` are the config-check hooks.
`Receiver.Stats()` is the status-page payload (connected, tuner, gain steps,
frames, repaired, noise dBFS, clipped samples, drops).

Suggested settings (file + `AIRVIEW_FEEDER_SDR_*` env, per DESIGN §20):
`sdr.enabled`, `sdr.rtltcp` (address), `sdr.spawn` (bool), `sdr.rtltcp_path`,
`sdr.device`, `sdr.gain`, `sdr.ppm`, `sdr.bias_tee`, `sdr.sample_rate`,
`sdr.accept_unverified_ap` (default false).

`winusb.Enumerate(0x0bda, 0)` + `DeviceInfo.IsRTLSDR()/WinUSBReady()` tell a
set-up page whether a dongle is present and whether the WinUSB driver
(Zadig) has been bound, which rtl_tcp needs.

## 3. Direct WinUSB (no rtl_tcp)

`internal/sdr/winusb` implements the transport (enumeration, open, control
and bulk transfers, pipe policies). The RTL2832U and tuner register
programming is not implemented: the only complete public descriptions are
GPL code, which the clean-room rule excludes. A future driver would sit
behind an `io.Reader` of interleaved u8 I/Q — exactly what the Receiver reads
from rtl_tcp — so the demodulator and feeder wiring stay unchanged. Details:
winusb/doc.go.

## 4. Tests

`go test ./internal/sdr/...`: SNR sweep vs truth, sample rates 2.0-3.2 MS/s,
noise-only false-frame check, single-bit repair, odd chunking, the rtl_tcp
client and receiver against `FakeRTLTCP` (header, commands, reconnect with a
new epoch, server drops, local drops, stalls), Beast round trip through
`internal/feed`, and simulated traffic from `internal/sim`. Benchmarks:
`go test ./internal/sdr -run X -bench .` (budget: busy stream < 104
ns/sample on the development PC).

Manual end-to-end without hardware:
`go run ./internal/sdr/sdrbridge -fake -beast 127.0.0.1:18737 -for 30s`
(measured: about 135 frames/s of simulated traffic, all parsed as Beast).
