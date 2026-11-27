// Package sdr receives Mode S directly from an RTL-SDR dongle: an rtl_tcp
// client, a pure-Go 1090 MHz demodulator and the glue that turns the two
// into model.RawFrame values with 12 MHz sample-clock timestamps (DESIGN
// §12.3, phase 5: Windows RTL-SDR without a third-party decoder).
//
// # Pieces
//
//   - Client (rtltcp.go): the rtl_tcp protocol. The server sends a 12-byte
//     header ("RTL0", tuner type, gain step count, big-endian) and then raw
//     interleaved unsigned 8-bit I/Q; the client sends 5-byte commands (one
//     command byte, a big-endian 32-bit parameter) for frequency, sample
//     rate, gain mode, gain, ppm, AGC, bias tee and so on.
//   - Demodulator (demod.go, viterbi.go, crc.go): magnitude, a preamble
//     screen against a running noise estimate, a per-frame fit of the
//     pulse amplitude, sub-sample start phase and the front end's pulse
//     spread, Viterbi bit decisions over that model, CRC-24 parity with
//     single-bit repair of DF11/17/18 restricted to the least certain bits,
//     and address/parity replies accepted only for recently confirmed
//     addresses. Each frame carries a 12 MHz-equivalent timestamp derived
//     from the sample count (sample × 12e6 / rate, refined to a fraction of
//     a sample), the signal level in dBFS and the SNR.
//   - Receiver (receiver.go): dials rtl_tcp, configures it, feeds the
//     demodulator from a buffer pool, reconnects with back-off, detects
//     stalls and server-side sample drops (samples falling behind the wall
//     clock) and starts a new clock epoch for them, accounts for local drops
//     without breaking the timestamp sequence, and delivers model.RawFrame
//     with ClockCls = model.ClockSDR12 to any model.FrameSink.
//   - RunChild (child.go): starts the user's own rtl_tcp as a child process
//     and restarts it; AirView never ships or downloads rtl_tcp (GPL).
//   - BeastServer (beast.go): re-serves frames as Beast binary, so any
//     Beast consumer (AirView's own feed parser included) can use them.
//   - Synthesize (synth.go) and FakeRTLTCP (fake.go): an I/Q synthesiser
//     for known frames at a chosen SNR, carrier offset and phase, and a
//     protocol-faithful fake rtl_tcp server; used by the tests, the
//     sdrbridge tool and the feeder's "no hardware" self-test.
//   - winusb (subpackage): a WinUSB transport and device enumeration for a
//     future direct-USB driver; see its package documentation for why the
//     RTL2832U/tuner programming is not implemented (clean-room rule).
//   - sdrbridge (subpackage, a command): rtl_tcp → Beast on a TCP port,
//     for bring-up and comparisons until the feeder wiring exists.
//
// # Performance (2.4 MS/s, i9-13900K, one core)
//
// Noise only: about 5 % of a core. A busy synthetic stream of 2,000
// frames/s with random overlaps: about 20 % (≈ 82 ns/sample), dominated by
// the Viterbi decisions of the examined candidates. A Raspberry Pi 4 class
// CPU is roughly 6-8× slower per core, so the busy case there is around one
// core (an estimate, not measured on a Pi). This path is aimed at Windows
// PCs.
//
// # Sensitivity (TestDemodSNRSweep, SNR = pulse power / noise power per
// complex sample, random carrier offset ±50 kHz, random sub-sample phase)
//
//	 8 dB   0.2 %     12 dB  88 %     16 dB  100 %
//	10 dB  28.5 %     14 dB  99.8 %   20+ dB 100 %
//
// with no false frames in any run and 10-11 ns median timing error. Four
// seconds of noise alone produce no frames.
//
// See WIRING.md for how the feeder is meant to use the package.
package sdr
