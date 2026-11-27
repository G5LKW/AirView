// Package latency measures, for every received Mode S / ADS-B message, how
// long it took from the moment the data was valid on the aircraft to the
// moment AirView had it, broken into physically meaningful stages:
//
//	A  time of applicability (ToA) of the position   -- exact when T=1, else estimated
//	   └ position age at transmission                 (transmit − ToA)
//	B  propagation                                    (slant range / radio speed)
//	C  time of arrival at the antenna                 (Radarcape GPS ns since UTC midnight)
//	D  receiver → host                                (host socket read, NTP-corrected, − C)
//	   processing                                     (tracker handled − socket read)
//	E  NTP vs GPS consistency                         (minimum-filtered D, jitter, drift)
//
// # Verified facts (sources fetched and read)
//
// T bit, 0.2 s epochs. RTCA 1090-WP-14-09R1 (DO-260A draft text, Appendix A
// §A.1.4.2.2, §A.1.4.2.3.1-2, §A.1.4.3.4), https://www.anteni.net/adsb/Doc/1090-WP-14-09R1.pdf:
//   - Bit 21 of the airborne (TC 9-18, 20-22) and surface (TC 5-8) position ME
//     is T. T=1: the time of applicability is synchronised to UTC; only used
//     with the top two precision categories (TC 9, 10, 20, 21; surface 5, 6).
//   - With T=1 the F (CPR format) bit also tells the 0.2 s tick: "even-numbered
//     0.2 second epoch" = an even number of 200 ms intervals after an
//     even-numbered UTC second (12.0, 12.4, 12.8, 13.2, 13.6 s); odd = 12.2,
//     12.6, 13.0, 13.4, 13.8 s. So the parity is NOT "even seconds"; 13.0 s is
//     odd. This package counts 200 ms intervals from the Unix epoch, which
//     gives exactly that parity.
//   - The receiver recovers the ToA as the NEAREST even (F=0) / odd (F=1)
//     epoch to the time the message is received - it may lie before or AFTER
//     the reception, because the position register is loaded 150..50 ms
//     BEFORE the time of applicability (ideally 100 ms) with a position
//     extrapolated to that epoch. A compliant transmitter therefore sends
//     within about ±150 ms of the epoch; negative transponder latency (sent
//     before the epoch) is normal. (The task brief said "before TOA" - that
//     is corrected here.)
//   - T=0: the register is reloaded at least every 200 ms with data whose
//     ToA is never more than 200 ms from any time the register holds it;
//     receivers "accept airborne position messages as being current as of
//     the time of receipt". Extrapolation stops after a 2 s navigation
//     timeout.
//   - Decoders: dump1090-fa mode_s.c ("21: T flag (UTC sync or not)", only
//     read, never used for timing); rs1090 bds05.rs cites ICAO Doc 9871
//     §A.2.3.2.2 with the same F/epoch rule. modes.Message.CPRTime already
//     carries the bit (set only when bit 21 is not the TIS-B/ADS-R IMF).
//
// Velocity / TC 29. The airborne velocity (TC 19) layout in the same
// appendix (§A.1.4.5: subtype, intent change, IFR capability, NUCr/NACv,
// velocity fields) has no time field; neither does target state (TC 29) or
// operational status (TC 31). Only position messages carry a time reference.
//
// Latency requirements (context bands):
//   - 14 CFR 91.227(e) (https://www.law.cornell.edu/cfr/text/14/91.227):
//     geometric position transmitted no later than 2.0 s after measurement;
//     at most 0.6 s of that may be uncompensated latency.
//   - George, Tuladhar, Sivananthan, "A study of uncompensated latency in
//     ADS-B reports" (arXiv:2008.06352): DO-260B Appendix U budget for
//     uncompensated latency is -200..+400 ms; measured Chicago means were
//     within ±50 ms; ~1 % of aircraft were UTC-coupled (T=1), and those were
//     found NOT actually synchronised to the epochs (non-compliant) - the
//     same failure the "t1-not-synchronised" flag detects. Their along-track
//     position error method (UL = along-track error / speed) needs an
//     independent truth track; with only the aircraft's own reports a
//     CONSTANT latency is unobservable (it shifts the whole track in time),
//     which is why this package estimates the age jitter and a model mean.
//   - Note: these limits run from the navigation fix (time of measurement),
//     which is not on the air; the T=1 "transponder" stage here is transmit −
//     time of applicability, i.e. the residual after the transmitter's own
//     compensation.
//
// Radarcape timestamps:
//   - jetvision wiki Radarcape:Software_Features: GPS timestamps are "daysec
//     and nanoseconds"; "TrueUTC: daysec starts with 0 at midnight, range is
//     0..86399"; "Daysec+1: Legacy mode: daysec range is 1..86400";
//     "TimeGPS: Timestamp is GPS time" until the UTC offset (+18 s) is known.
//   - jetvision wiki Mode-S_Beast:Data_Output_Formats: 6-byte timestamp; the
//     1PPS NCO error is reported in 15 ns ticks, typically ±3 ticks.
//   - Which instant: not stated by jetvision. mlat-client modes_reader.c
//     (used with Radarcapes by every MLAT network) treats the Radarcape GPS
//     timestamp as the END of the frame and subtracts 64 us (short, 8 us
//     preamble + 56 us) or 120 us (long) to get the start of the preamble; it
//     also subtracts one second when the status frame lacks the "true UTC
//     range" bit. internal/feed already handles the second; this package
//     subtracts the frame duration (Options.NoFrameEndCorrection disables).
//     Mode A/C replies are stamped at F2 (20.3 us after F1) and status
//     frames are taken as is - selected by Obs.Kind (SetFrame fills it).
//
// # Estimated position age (T=0)
//
// For consecutive positions of one aircraft, with GPS arrival times t1, t2,
// ground speed v and track, the along-track displacement d satisfies
// d = v·(t2 − t1) − v·(λ2 − λ1), λ = position age at transmission, so each
// pair yields δ = λ2 − λ1 = (v·Δt − d)/v. Over many pairs var(δ) = 2σλ² +
// 2σpos²/v²; the cross-track residual measures the position noise, so
// σλ = sqrt((var δ − mean (cross/v)²)/2). Under the DO-260A register model
// (age uniform in [0, P], P <= 200 ms) mean age = √3·σλ; that "model mean"
// is labelled an estimate. Per message, E[λk | δk] = mean + κ·(δk − mean δ)
// with κ = σλ²/var δ is the single-message estimate. For T=1 aircraft the
// pair method is cross-checked against the exact ages (AgeCheckMs).
//
// # Uncertainty
//
// Propagation up to ±2 us: barometric (pressure) altitude stands in for
// geometric height, and QNH / temperature deviations of several hundred
// metres at cruise levels enter the slant range as Δh·sin(elevation) - small
// for distant aircraft, largest overhead; CPR and the aircraft's motion
// between epoch and transmit (T=1, <= ~40 m) add < 0.2 us. Radio speed is
// c/(1+N·1e-6) in air, not vacuum: 1090 MHz is non-dispersive in the
// troposphere and the aircraft is below the ionosphere; N≈315 ppm at the
// ground gives ~0.3 us excess over 300 km. GPS TOA ±1 us (frame-end model);
// receiver→host ± (NTP delay/2 + root delay/2 + root dispersion + jitter);
// T=1 transponder latency ±3 us; estimated ages ±σλ plus model doubt.
//
// Limits of the pair model: it assumes ages of consecutive positions are
// independent. A T=0 source whose navigation fix updates slower than the
// message rate (e.g. 1 Hz GNSS, messages every 0.4-0.6 s) gives correlated
// ages; var δ is then not 2σλ² and the jitter and √3·σλ model mean are
// biased - for that example OVER-estimated (~350 vs 289 ms jitter, see
// TestReferenceCorrelatedAgesBias); other lag / fix-period ratios can bias
// either way. A constant age offset is never observable.
//
// Legacy / suspect timestamps: a GPS timestamp one whole second off (legacy
// Daysec+1 not corrected upstream) or grossly off an NTP-synced host is kept
// out of the absolute-time stages - an odd number of seconds flips the
// 0.2 s epoch parity and would bias the T=1 stage by 200 ms. GPS-time
// stamping (+18 s, even) keeps the parity and is still used for T=1.
//
// Wiring: see WIRING.md in this directory.
package latency
