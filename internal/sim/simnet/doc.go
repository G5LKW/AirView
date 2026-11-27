// Package simnet simulates a network of ADS-B / Mode S receivers for the
// AirView aggregator, its clock synchronisation, MLAT and self-location
// (docs/aggregator/DESIGN.md §13.1).
//
// One deterministic world (internal/sim, used through its exported API
// only: perfect reception at a virtual station in the middle of the
// network, so every transmission arrives exactly once) supplies the
// aircraft and their bit-exact frames. simnet recovers each frame's
// transmitter, transmit instant and position, and then for every receiver:
//
//   - propagation: WGS-84 slant range plus a refraction excess path from an
//     exponential refractivity profile N(h) = N0·exp(−h/H), scaled by k_atm
//     (Atmosphere);
//   - reception: link budget (aircraft EIRP, free-space loss at 1090 MHz,
//     antenna elevation pattern and cable loss), a soft radio horizon with
//     the k-factor, terrain masks, log-normal fading, a logistic detection
//     curve and random losses (ReceptionSpec);
//   - timing impairments: multipath delays (more at low elevation) and gross
//     outliers;
//   - the receiver clock (ClockSpec, Device): GPS-disciplined Radarcape
//     nanoseconds with 1PPS jitter, cable bias, lock loss and a GPS-time
//     boot; 12 MHz Beast/Radarcape counters with a frequency random walk;
//     RTL-SDRs at 2.4 MS/s with tens of ppm, thermal ramps, jitter and
//     silent or declared sample drops; Airspys at 20 MS/s in 12 MHz units;
//     SBS 20 MHz 24-bit counters; per-class frame reference points and a
//     short-versus-long reference error; scheduled steps and restarts; host
//     clock error and host latency;
//   - per airframe: position latency 0..300 ms and a NACp-consistent
//     position error, applied by re-encoding the CPR position of airborne
//     position squitters; the share of Mode S-only aircraft and the DF11 /
//     DF0/4/5/20/21 traffic come from internal/sim.
//
// Every receiver uses only its own random streams, derived from the seed,
// so a run is reproducible and independent of Config.Workers.
//
// # Outputs
//
// Observations (truth plus what the receiver reports) go to a network-wide
// Sink (merged in arrival order) and to per-receiver RxSinks. The emitters
// are RxSinks:
//
//   - BeastEmitter: Beast binary (BeastFull) with Radarcape GPS or 12 MHz /
//     20 MHz timestamps, type '4' status frames and optional type '5'
//     positions; or readsb beast_reduce_plus (0x1A 0xE4 + 36-byte UUID,
//     'W' 'O', reduced frames);
//   - MLATClientEmitter: an mlat-client-like JSON message stream (sync,
//     mlat, seen/lost, rate_report, clock_reset, heartbeat);
//   - AVFEmitter: AVF/1 records (RECEIVER, FRAMES, CLOCK_STATUS,
//     CLOCK_PAIR, CLOCK_EVENT, BACKFILL) handed to an AVFWriter;
//   - Link: a ByteSink between an emitter and the server that models
//     latency, jitter and disconnections.
//
// TruthLog writes a JSON-lines truth log; Receivers, ClockAt, ClockEvents,
// Aircraft and AircraftAt are the truth API.
//
// # Usage
//
//	specs := simnet.RandomReceivers(1, 200, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: 400})
//	n, _ := simnet.New(simnet.Config{Seed: 1, Aircraft: 800, Receivers: specs})
//	var buf bytes.Buffer
//	n.NewBeastEmitter(0, &simnet.WriterSink{W: &buf}, simnet.BeastOptions{})
//	n.AddSink(mySink)
//	n.Advance(ctx, 10*time.Minute)
//
// Times are float64 seconds since Config.Start (sub-nanosecond resolution
// over days); Network.Time and Network.UnixNs convert.
package simnet
