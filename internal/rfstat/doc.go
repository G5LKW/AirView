// Package rfstat computes advanced RF / radio / protocol statistics online
// from the received Mode S / ADS-B frame stream: signal distributions,
// noise floor and overload, fading (Rician K) and multipath lobing, aspect
// (airframe shadowing), probability of detection of ADS-B squitters versus
// distance / elevation / bearing / RSSI, position update intervals,
// 1090 MHz channel occupancy and garble, the Mode S interrogator census
// (II/SI codes, lockout, Comm-B register mix, ACAS activity), GPS-timestamp
// squitter timing compliance, the ADS-B equipage census and a GNSS
// interference index, plus a scored "RF health" summary with advice.
//
// Usage:
//
//	rs := rfstat.New(rfstat.Config{Antennas: []string{"yagi", "vertical"}})
//	go rs.Run(ctx, st)          // 1 Hz Tick + hourly summaries into the store (st may be nil)
//	rs.Observe(frame)           // per frame, from the tracker (no allocation)
//	rep := rs.Report()          // JSON-ready snapshot over a sliding ~1 h window
//
// Memory is bounded: one flat accumulator per clock hour (current and
// previous, ~120 kB each), fixed rings for per-second / per-minute series,
// and a fixed-capacity aircraft table (Config.MaxAircraft, ~0.5 kB each).
// Nothing is written per frame; Run persists one row per hour.
//
// Reference facts used (verified, see WIRING.md): nominal ADS-B rates
// airborne position 2/s, velocity 2/s, identification 0.2/s; position and
// velocity squitters jittered 0.4-0.6 s; Mode S replies = 8 us preamble +
// 56/112 us data at 1 bit/us (64 / 120 us on air); lambda = c/1090 MHz.
package rfstat
