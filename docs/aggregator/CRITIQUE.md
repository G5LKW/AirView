## Judge's critique of Design A (compatibility-first) and Design B (precision-first)

Both designs agree on most of the architecture: three modes plus uplinks, a TLS 1.3 native protocol with Ed25519 identity bound to the channel by a TLS exporter, compat listeners on 30004/31090, one global tracker with per-station lanes, a joint network clock filter anchored on GPS Radarcapes, LM/RAIM/CRLB, a reverse-MLAT survey, typed public DTOs and golden privacy tests. So the judging is mostly about the details. I checked the dependency claims against `D:\AirView\go.mod`: Go 1.27.3, so `crypto/pbkdf2` and `crypto/hkdf` are in stdlib. `ARCHITECTURE.md` lines 16-17 forbid adding third-party modules. That makes A correct to reject gonum and the systray libraries, and the research note's gonum/fyne suggestions are out of bounds.

### 1. MLAT and clock maths

**Errors and gaps both designs share**

1. **Short/long frame reference offset.** Design A says a constant per-type offset "is absorbed by the clock state". It is not. θᵢ is one scalar per station. Sync uses long DF17 frames, but most MLAT targets are short DF4/5/11 replies. On Radarcape GPS stamps the short and long offsets differ by 56 µs, which is 17 km. Any table error for 12 MHz decoders, whose stamping instant differs by implementation (open question in the clock research), becomes a constant bias on every short-frame fix. Design B leans on δ, but δ also cannot separate short from long.
   - **Fix:** a per-station β_short state. It is observable because DF11/DF0/4/5 replies from ADS-B aircraft have known positions and can be used as sync observations.
2. **Dedup and repeated transmissions.**
   - Both designs key on the payload hash with a 1 s host-time window. DF11 acquisition squitters repeat with an identical payload every 0.8-2.4 s, and surface or identification DF17 can repeat too. A 1 s host-time window therefore merges distinct transmissions.
   - Design A's "≤3 ms when both are GPS" rule helps only anchors.
   - **Fix:** a single station's own receiver clock is directly comparable with itself. A second copy from the same station (any channel) more than 1 ms of receiver time from its first copy is a new transmission, so open a new entry. This is cheap, exact and works for every clock class.
3. **Solver weighting.** Both designs give the solver σ per station, but normalised TOAs are correlated through the clock covariance. That holds always once there is a common reference, and when two stations' estimates are correlated.
   - Design B notes the cross-covariance for conversion but still describes a diagonal W.
   - **Fix:** the solver must use the full m×m covariance (clock block + jitter), which is cheap at m ≤ 16.
4. **Gauge for anchors with δ.** If every anchor carries an unknown δ, a common shift of all δ is unobservable from TDOAs.
   - Design A gives each anchor a prior of 0 ± 100 ns, which pins it softly, but does not say why.
   - Design B gives no gauge at all.
   - **Fix:** a mean(δ_anchor) = 0 constraint.
5. **Filter cost claims.**
   - Design B claims "7×10⁷ flops, 30 ms at 4 Hz" for N = 300. That counts one Cholesky only. A square-root information predict needs a QR of the full state (n = 600-900), O(n³) ≈ 2-7×10⁸ flops per epoch. At 4 Hz that is several GFLOP/s in pure Go, which is a whole core or more.
   - Design A's 0.5 s epoch and 50-150 ms estimate are closer.
   - **Decision:** 0.5 s epochs, covariance form with block-diagonal F, information accumulated per epoch, one Cholesky. Dense up to 200 stations per component, with a benchmark gate.
6. **Refraction scale k_atm as a fast filter state (Design B).** It is weakly observable and couples into every clock. Estimate it in a slow hourly regression on anchor-pair residuals versus elevation instead.
7. **Altitude convention of compat feeders.** Design A treats mlat-client altitude as MSL, which is the community installers' wording. mlat-server historically uses it as an ellipsoidal height. Neither design resolves the ambiguity.
   - **Fix:** the survey tests both interpretations, keeps the one with the lower residual, and tells the owner.
8. **Sound in both:** using every DF17 frame (and, under my fix, DF11/DF4/5 from ADS-B aircraft), the T=1 transmit-instant correction, the NIS gates, Bancroft and the 3+alt grid with two hypotheses, RAIM/FDE and the hold-out validation.
   - Design B's anchor probation with a "first anchor self-asserted" bootstrap is better than Design A's, because A cannot validate the very first anchor.
   - Design B's k·1 s offset check catches both the GPS-time base and the legacy +1 s quirk.
   - Design B's airframe position-latency calibration is good but unproven. It goes in a later phase behind a hold-out A/B test.
   - Design B's minimum-contribution rule for MLAT results is a real anti-harvesting measure.

### 2. Privacy and inference

**Design A leaks more than it needs to**

- **Public `seenBy` buckets.** A count of 1 at low altitude reveals a coverage island. Design A only suppresses below 3000 ft, and a bucket on a cruising aircraft still lets an attacker map single-station coverage edges over time.
  - **Decision:** Design B's rule, no public receiver information at all.
- **Public CRLB map built from public_pin geometry.** It is still a function of station density, and the admin version uses exact geometry. A public "expected accuracy" surface is a coverage map of the network.
  - **Decision:** the CRLB map is admin only. The public sees only empirical hold-out error, aggregated per 1° cell with at least 5 contributing stations.
- **Peer distance bands for owners (both designs).** An owner with three stations can trilaterate a peer from bands if the peer tag is the same across their stations.
  - **Decision:** peer tags are HMAC(serverKey, viewerSID ‖ peerSID ‖ ISO week), so they cannot be linked across a viewer's own stations. Bands are coarse: <50, 50-150, >150 km.
- **Hub 30154 serving role-scoped public data in aggregator mode (Design A).** This is an unnecessary second public surface. **Decision:** admin only (Design B).

**Design B's weak points**

- **Claim code = HMAC(feederKey, serverId‖day).** It is deterministic within a day and cannot be single-use.
  - **Decision:** Design A's server-minted, hashed, single-use code, valid 24 h.
- **Share links.** They are useful, but phase them after the core.

**What both get right**

- Hidden pins by default, a keyed one-time PRF offset, owner/admin-only coverage products and Locate Me.
- Feed Check by IP shows only non-locating data.
- Typed DTOs, deny-by-default scopes and golden tests.
- Audit logging and the retention schedule.
- Both fix the confirmed loopback-is-admin hazard.

**Neither design addresses**

- Single-station low-altitude aircraft on the public map (the coverage-island residual risk). Here this is an admin setting, off by default, and stated in the notice.
- Exposure through timing logs. I add a rule that `conn_log` and debug logs never record per-station TOAs or positions.

### 3. Compatibility

**Design A is stronger.**

- **No MLAT from 30004 timestamps**, because beast_reduce thins the copies. Design B implies a survey on reduced Beast data, which is sparse and biased. Compat feeders get MLAT and survey only through 31090.
- **The external mlat-server bridge** on localhost SBS-in. It gives OARC-style communities a no-risk migration path and needs no AGPL code in AirView.
- **tar1090-shaped `/data/aircraft.json` and `/data/receiver.json`** as a first-class feature, not "optional".
- **Compat ingest early.** The user's own example community runs readsb plus mlat-client, so compat cannot wait until Design B's phase 5.

Both designs correctly leave the readsb 'P' ping direction and the mlat-client timestamp conventions to be confirmed from packet captures of the user's own equipment. Design B's choice of 30157 as the local results port clashes with adsb.fi's mlat-client default. I use 30105.

### 4. Pure-Go feasibility

Everything both designs propose is achievable with stdlib plus x/sys:

- TLS 1.3, the exporter, Ed25519, x509 self-signing, flate/zlib, PBKDF2/HKDF, and TOTP over HMAC-SHA1;
- `windows/svc`, plus a Shell_NotifyIconW tray built on raw syscalls, about 600 lines;
- the hand-written small linear algebra.

Design B's mention of zstd and ACME is correctly rejected by B itself.

**Storage.** Design A's per-station SQLite files would run every registered migration (predict, navdata, met, …) in each file and start N writer goroutines. Design B's single `stations.db` with namespaced `ant` keys is cheaper, friendlier to a Pi or SD card, and changes only an analytics prefix. **I choose B.**

**Lane accounting.** Unique-aircraft counting in today's tracker relies on per-aircraft uint32 slot masks, which cannot scale to stations. Neither design noticed this. Lanes need their own per-minute and per-hour ICAO sets.

**Credit path.** Design B's grouper in front of the tracker is right. Credits must not take the tracker mutex at 300k copies/s, so lanes get their own sharded locks, and the grouper caches the first copy's decode result.

### 5. Effort and plan quality

- **Design A** has the more honest estimate (45-55 aw) and a sounder ordering: compat in phase 1, clean-room records.
- **Design B** (30-40 aw) underestimates the UI and per-station engines that the codebase track sized at 4-6 aw and 4-6 aw.
- **Both** have WP ownership overlaps:
  - A: WP26 "engine (owner continues) and web (owner continues)"; WP32 touching avfsrv.
  - B: R1 owning both feed and hub; I2 versus W4 on analytics.

The final plan below makes each wave's ownership strictly disjoint, freezes `internal/model` after phase 0, and accounts for the concurrent workflow now editing the tree. Phase 0 starts from whatever tree that workflow leaves.

### Verdict

- **From A:** compat-first ordering, the bridge, tar1090 JSON, server-minted claim codes, the RAM backfill ring, `--no-timestamps`, the clean-room record.
- **From B:** the record set with seq/ACK, a JSON handshake inside TLS, the explicit clock-class enum, anchor probation and leap handling, `stations.db` with namespaced keys, the grouper in ingest, no public receiver information, the contribution rule for results, and the IMM raw-TOA tracker.
- **New fixes:** β_short, the same-station repeat rule, full-covariance solving, the δ gauge, a realistic filter cost, the MSL/HAE survey test, unlinkable peer tags, and an admin-only CRLB map.