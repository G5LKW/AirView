# Route lookups: design

Status: design, 2026-11-15. Scope: station mode and aggregator (hub) mode.
Inputs: research tracks "sources", "competitors", "inference" and "codebase" (probes made 2026-11-15 with the User-Agent `AirView-research`, about 45 requests in total, none carrying user data).

## 0. Goal and what "beating tar1090 / VRS" means

Today tar1090-based sites (adsb.lol, ADSBx and airplanes.live, where routes are switched off) and VRS show at most a text pair such as "LHR - DOH". tar1090 adds a "??" prefix that almost never fires, and VRS shows From/To/Via columns. None of them:

1. learns routes from its own observed flights;
2. scores a route continuously against the live track and explains the score;
3. shows the active leg of a multi-stop route, progress or ETA;
4. draws origin→destination on the map with flown and remaining parts;
5. offers airport boards, route, airport and airline statistics, or route alerts;
6. lets the operator correct a route;
7. keeps viewers' browsers from sending every callsign and position to a third party.

AirView will do all seven. It works fully offline by default, and the online sources are opt-in and server-side only.

Design principles:

- **Server-side only.** Browsers never call a route provider. On a hub, one shared cache serves 1000+ feeders and every viewer.
- **Offline first.** The CC0 standing data plus AirView-learned routes cover more than 95 % of airline traffic without any online request.
- **Licence hygiene.** Each answer is tagged with a licence class. Only CC0 data and AirView's own data are merged into persistent tables; everything else lives in an expiring cache and is never re-published where its terms forbid it.
- **Honesty.** Every displayed route carries its source, its confidence and a one-line reason. Conflicting routes are shown as doubtful, never silently picked.
- **Pi-friendly.** Lookups are point reads on WITHOUT ROWID tables. The per-second loop does no SQL. Writes are batched and table sizes are bounded.

## 1. What exists today (baseline we build on)

- `internal/navdata`:
  - Standing-data import from the GitHub zip into `navdata_route(callsign PK, airline, number, airports)`, about 629k rows. Import is manual.
  - Opt-in adsbdb lookups stored in `navdata_route_cache`, with a limiter, singleflight and Retry-After handling.
  - `CheckRoute`, a binary corridor test.
  - `GuessDeparture` / `GuessArrival`, which use the first or last 6 points.
  - `RouteInfo` and a great-circle `path`.
- `internal/engine/ext.go`:
  - The live route map (4096 entries, wiped on overflow) is filled by `routeWorker` from offline data only.
  - `routeFor` re-checks plausibility every second.
  - `PutRoute` puts an adsbdb answer into the live picture.
- `model.RouteShort` holds from/to, IATA codes, names, the via count, plausible, source and checked.
- UI: a Route column, a detail-panel badge, a route line from `/api/navdata/route/{cs}`, and settings with a toggle and a cache-clear button.
- Flights store `dep` / `arr` guessed from the track (core migration v2). The filed route is not stored.

Known defects that this design fixes:

- Plausibility is binary. The "within 40 km of any airport" rule passes FL380 hub overflights.
- Multi-stop routes show first→last instead of the active leg.
- The adsbdb User-Agent has no contact.
- `GET /api/navdata/route/{cs}` is AccessPublic, so anonymous hub viewers can use up the shared online budget and inject answers into the live picture.
- adsbdb answers are re-served through the public DTO, which conflicts with adsbdb's terms.
- The standing data is never refreshed.
- `AirportByCode` ranks an ident match above an IATA match.
- The settings privacy text is wrong.
- The endpoint list in WIRING.md has drifted from the code.

## 2. Architecture

A new package, **`internal/routes`**, owns route resolution, learning, overrides, statistics and ETA. `internal/navdata` keeps datasets, airports, geometry and the bulk import. The engine, server and UI talk only to `routes.Service`.

```
                +----------------------- internal/routes -----------------------+
 tracker 1 Hz   |  Resolver (in-memory LRU, keyed (callsign, icao, legStart))   |
 snapshot ----> |    1 override    routes_override           (admin, always wins) |
 (engine ext)   |    2 learned     routes_learned            (AirView-own, CC0)   |
                |    3 standing    navdata_route             (VRS CC0, refreshed) |
                |    4 online      routes_cache  <- Providers (opt-in, batched)   |
                |  Scorer: plausibility + event evidence -> fused hypothesis      |
                |  ETA + progress  |  Learner (closed legs -> routes_obs)         |
                |  Stats (routes_daily) | Board (live in-memory)                  |
                +----------------------------------------------------------------+
                         |                                 ^
          model.RouteShort on Aircraft            leg events (hub upload)
          (public-safe fields only)
```

Lookup cost per aircraft per second: one map read (LRU hit). Misses go to a worker queue. The worker reads SQLite at most every 250 ms per batch of up to 64 callsigns, using one prepared `IN (...)` query per table.

### 2.1 Callsign hygiene (routes/callsign.go)

- Normalise by upper-casing and trimming.
- Do not look up a callsign when it:
  - is blank;
  - equals the registration (with or without the hyphen);
  - does not match `^[A-Z]{3}[0-9][0-9A-Z]{0,3}$`;
  - is on a test or placeholder list (`TEST*`, `00000000`).

  It may still be shown as "no route".
- **Exact match only**, in the order override → learned → standing → online cache.
- Variant hypotheses (strip leading zeros: `BAW0123`→`BAW123`; strip one trailing letter: `BAW101P`→`BAW101`) enter fusion with logit −1.0. They never display without positive track evidence. Never fuzzy-strip EZY- or RYR-style alphanumeric suffixes; they are different routes.
- Positioning and charter labelling: import VRS `airlines.csv` (CC0) and apply its `PositioningFlightPattern` / `CharterFlightPattern` regexes to set `RouteShort.Kind` to `"positioning"` or `"charter"`. tar1090 and VRS web do not show this.
- An operator mismatch is flagged when the airline prefix's country differs from the ICAO-block country *and* the registry operator (where known) differs. The flag is `RouteShort.Flags |= "cs-mismatch"` and it lowers fusion by 0.5.

### 2.2 Sources and licence classes

| # | Source | Access | Licence / terms | Class | Stored in |
|---|--------|--------|-----------------|-------|-----------|
| 1 | VRS standing data, routes + airlines (via the `vrs-standing-data.adsb.lol` mirror, GitHub zip as fallback) | bulk, conditional GET daily | CC0 1.0 | `open` | `navdata_route`, `navdata_airline` |
| 2 | OurAirports (already imported) | bulk | Public domain (Unlicense mirror) | `open` | navdata airports |
| 3 | AirView learned (own observations, station and network) | internal | AirView-owned; published as CC0 | `own` | `routes_learned` |
| 4 | Operator / admin overrides | UI / API / file | operator's own | `own` | `routes_override` |
| 5 | adsbdb `GET /v0/callsign/{cs}` | per callsign, opt-in | data: no copying, publishing or incorporation without D. J. Taylor's permission; code MIT | `restricted` | `routes_cache` (expiring) |
| 6 | hexdb v2 `GET /api/v2/routes/{cs}` | per callsign, opt-in | no published terms; PlanePlotter/Mason lineage, so treated as restricted | `restricted` | `routes_cache` |
| 7 | adsb.im `POST http://adsb.im/api/0/routeset` | batch ≤100 per ≥30 s, opt-in | ODbL 1.0, non-commercial; contact UA, plain http, 30 s batching; other use needs the maintainer's prior approval | `odbl` | `routes_cache` |
| 8 | adsb.lol `POST /api/0/routeset` | batch ≤100, opt-in | ODbL 1.0 (code BSD-3). Returned 201 with an empty body to anonymous probes; an API key is planned | `odbl` | `routes_cache` |
| 9 | Bring-your-own-key paid APIs: FlightAware AeroAPI, AirLabs, aviationstack | per callsign, user key | proprietary, no redistribution | `restricted` | `routes_cache` |
| — | OpenSky routes / flights | — | undocumented and stale (2020); non-profit only | **not used** | — |
| — | Airline schedules / airport boards (OAG, Cirium, scraping) | — | paid, or ToS forbid scraping | **not used** | — |

Licence-class rules (enforced in code, tested):

- `open` and `own` may be persisted, exported, federated and shown publicly.
- `odbl` may be shown publicly with attribution ("Route data © adsb.im / adsb.lol contributors, ODbL"). It is never merged into persistent tables or exports and never used as learning evidence, which avoids share-alike on AirView's database.
- `restricted` is shown only to the station's own authenticated users (owner/admin/viewer scope on a station). It is never placed in the public DTO, federation, exports, stats tables or learning. On a hub's public map such a route is replaced by "route not public". Fusion may still use it internally, but displays it only in non-public scopes.
- Attribution lines for every enabled source appear in `/api/navdata/status`, the Settings "Reference data" section, the About/Credits screen and the manual licences page.

### 2.3 Provider layer (routes/provider*.go)

```go
type Query struct { Callsign string; ICAO uint32; Lat, Lon float64; HasPos bool }
type Answer struct {
    Callsign string; Found bool; Airports []string // ICAO-or-code, ordered
    Airline, Number string; Updated time.Time      // provider's own timestamp if any
    Plausible *bool                                // provider's flag (ignored in fusion, logged)
}
type Provider interface {
    Name() string            // "adsbdb" | "hexdb" | "adsbim" | "adsblol" | "aeroapi" | ...
    Class() LicenceClass     // open | odbl | restricted
    MaxBatch() int           // 1 for per-callsign APIs
    MinInterval() time.Duration
    SendsPosition() bool
    Lookup(ctx context.Context, qs []Query) ([]Answer, error)
}
```

- Implementations:
  - `adsbdb` moves here from navdata/route.go; navdata keeps a thin deprecated wrapper.
  - `hexdb`.
  - `adsbim` and `adsblol`, sharing one routeset codec.
  - `aeroapi`, a BYOK example; others can follow the same template.
- Each provider gets its own limiter (min interval; daily cap, default 1000), singleflight, and error state:
  - **429**: honour `Retry-After` / `X-Rate-Limit-Retry-After-Seconds`.
  - **5xx / network**: exponential backoff with full jitter from 30 s, capped at 1 h.
  - **403**: provider disabled for 24 h. The response body (policy text, at most 1 KB, sanitised) goes to the event log and settings status.
  - **401 / bad key**: disabled until the config changes.
- **Chain order (default)**: adsbdb → hexdb → adsbim → adsblol → BYOK. Only enabled providers take part. The admin can reorder them.
- A query goes online only when:
  - online lookups are enabled; and
  - the callsign has been stable for ≥ 45 s on an aircraft actually received; and
  - it passed hygiene; and
  - it missed override, learned and standing data, **or** its best fused hypothesis is "doubtful" for ≥ 3 min (a "second opinion" query, at most once per callsign per 6 h).
- **Batching**:
  - The Batcher collects pending queries every 30 s, or immediately when ≥ 100 are queued.
  - Routeset providers get one POST of ≤ 100.
  - Per-callsign providers drain the queue at their own interval.
  - On a cold hub start, priority goes to aircraft visible on the most viewers' maps (the SSE view registry count), then to the nearest.
- **Identity**:
  - User-Agent `AirView/<ver> (+<navData.routes.contact or project URL>)`.
  - The default contact is the project URL, never a personal address.
  - adsb.im requires a contact; it is refused to start without `routes.contact` or the project URL, mirroring photos.go's `ErrContactMissing`.
- **Privacy**:
  - Never send the receiver location, a feeder ID or an e-mail.
  - Positions go only to routeset providers that use them, and only when `routes.sendPosition` is true (default false). Without it, routeset queries omit lat/lng.
  - adsb.im is plain http by its own terms. The settings page says so.
- **adsb.im gating**: the provider stays compiled in but shows "requires approval" in settings until `routes.providers.adsbim.approved` is true. This flag documents that the operator has the maintainer's approval or that the use is non-commercial and personal. A hub with 1000+ feeders must not enable it without approval.

### 2.4 Bulk standing data refresh (navdata)

- Change the routes source to `https://vrs-standing-data.adsb.lol/routes.csv.gz` (4.5 MB). Fall back to the GitHub zip on failure.
- Add `airlines.csv` from the GitHub raw path, or from the mirror if it is published there.
- The parser accepts the mirror's flat CSV (same columns).
- **Auto-refresh**:
  - When `navData.autoRefresh` is on (default true), navLoop schedules a conditional GET (If-None-Match / If-Modified-Since) every 24 h at a per-install random time of day, seeded from the install ID hash. 1000+ installs therefore never hit the mirror together.
  - A 304 costs nothing.
  - On 200 the import uses the existing staged atomic swap.
- After an import the live route LRU is **not** wiped. Only entries whose source is `standing` are marked for re-check, which avoids a re-lookup storm.
- First start with no data and auto-refresh on: download airports and routes once, about 6 MB. Until that completes, or when offline, routes show nothing and the UI says "reference data not imported".
- `AirportByCode` fix: for a 3-letter code, try IATA first, then ident. For 4 letters, try ICAO, then ident, then gps. Provider answers in IATA are resolved with `ResolveIATA`.

## 3. Inference: plausibility, events, fusion

### 3.1 Take-off and landing events (routes/events.go; replaces GuessDeparture/GuessArrival)

Detection scans the whole flight, not just its first and last 6 points.

**Height above ground (AGL):**

- Preferred: `(AltGeom − N_geoid) − elev`.
- Otherwise: `AltBaro + 27.3·(QNH−1013.25) − elev`, using the aircraft's own BDS 4,0 QNH, else the network QNH (§3.5).
- Last resort: raw baro with ±300 ft allowance.

**Landing tiers** (event quality q):

- **L1 (0.99)**: an on-ground/surface position, or gs < 80 kt and h < 200 ft, within 1 km of the centreline of a runway long enough for the aircraft.
- **L2 (0.90)**: the last airborne fix has all of:
  - h ≤ 1000 ft;
  - ≤ 8 km to the threshold;
  - an approach fit (|trk−hdg| ≤ 15°, lateral ≤ 0.3+0.08·d km);
  - descending (vr ≤ −300 fpm, or altitude falling over 60 s);
  - gs ≤ 200 kt.
- **L3 (0.75)**: established on the approach for ≥ 30 s and lost at h ≤ 3000 ft, while the terrain coverage model says the station would still see it 1000 ft lower.
- **L4 (0.45)**: lost below the radio horizon. Extrapolate touchdown with `t_g = h/|vr|` (vr clamped to −700…−1500 fpm). Candidate airports lie within 5 km + 0.1·|PT| of the segment.
- **L5 (0.30)**: only "low near an airport", the old behaviour.

**Landing cancellation:**

- Go-around: after an approach fit below 1500 ft, vr > +500 for ≥ 20 s.
- Touch-and-go: on the ground < 60 s. The flight stays one leg, tagged `circuit`.

**Take-off tiers** (mirror image):

- **T1**: surface → airborne transition on a runway.
- **T2**: the first airborne fix has h ≤ 1500 ft, vr ≥ +500, gs between 110 and 220 kt, and is aligned beyond the roll start with d ≤ 10 km.
- **T3**: first seen ≤ 4000 ft AGL and climbing, within 25 km of the airport.
- **T4**: first seen climbing below FL100. Back-extrapolate to the ground.

**Candidate scoring:**

- `S = (d/σd)² + (h/1500ft)² + (Δψ/15°)²·aligned + (lat/latTol)² − 2·ln(prior)`.
- σd = 3 km for L1/L2/T1/T2, 8 km otherwise.
- Size prior: large 1, medium 0.5, small 0.12, heliport 0.02. ×1.5 if the airport has scheduled service.
- Hard filters:
  - categories A3–A5 and jets need a hard runway ≥ 1500 m (A5/heavy ≥ 2400 m);
  - A7 (rotorcraft) may use heliports and has no alignment check.
- Relative confidence: `p_i = softmax(−S_i/2)`, and event confidence = q·p_best. This fixes nearby pairs such as EGLL/EGWU.

**Segmentation into legs** (applied when a flight closes):

- Split at a ground stop: on ground, or gs < 50 kt and h < 300 ft, for ≥ 120 s.
- Split at a landing event (q ≥ 0.75) followed by a take-off.
- Split at a callsign change.
- Split at a gap > 10 min after a landing event.
- Otherwise the existing 30-min gap rule applies.
- The tracker's flight-split rule itself stays unchanged in v1. Legs are stored as rows in `routes_leg` (see §5) that refer to a flight and a time range, so the `flights` table and the tracker remain untouched.

### 3.2 Plausibility score (routes/score.go; replaces binary CheckRoute)

For each leg A→B of length D km at position P:

- **Detour (ellipse) term:**
  - `ε = d(A,P)+d(P,B)−D`, `k = clamp(0.10+60/D, 0.10, 0.60)`, `z = ε/(k·D+20)`.
  - Inside ≤ 1; hard fail > 2.
  - Within 40 km of A or B, the point also needs h ≤ 12 000 ft AGL or matching vertical evidence. This kills the hub-overflight false positive.
- **Heading term** (only when > 150 km from both ends and not holding):
  - Δ = |track − bearing(P,B)|, adding `0.5·(Δ/45°)²`.
  - **Reversed**: for 5 min, closing on A and opening from B (c_B < −0.5·gs, c_A > +0.5·gs, 0.1 < along < 0.9). The flag gives +2 to the swapped hypothesis.
- **Vertical terms:**
  - Climbing near A: +1.
  - Descending below 5000 ft AGL near A: −2.
  - Descending near B: +1.
  - Climbing out of B: −2.
  - SelAlt ≤ 10 000 ft while descending and > 300 km from B: −2.
  - Cruising well past top of descent: −0.5.
- **Time term**: when a departure event is known, the implied gs `along/(t−t_dep)` must lie in 150–600 kt for jets (≤ 300 kt for A1/A2); otherwise −2.
- **Score**: `S = z² + heading − Σ(vertical + time)`.
- **States**:
  - `fits`: S ≤ 1.5.
  - `uncertain`: 1.5 < S < 4.
  - `doubtful`: S ≥ 4.
  - `reversed`.
  - `contradicted`: an observed take-off event (q ≥ 0.75) at an airport more than 30 km from A, or an L-event away from B.
- **Re-evaluation**: every 30 s per aircraft, outside the 1 Hz path; the result is cached in the LRU entry. **Hysteresis**: the state changes only after two consecutive evaluations agree.
- **Active leg**: the argmin of z over legs, tie-broken by observed events and heading towards the leg end. It sets `LegIdx`, `LegCount`, `AlongPct`, `ActiveFrom` and `ActiveTo`.

### 3.3 Hypothesis fusion and confidence (routes/fuse.go)

Hypotheses:

- every (dep, arr[, via]) from override, learned, standing, the online cache, variants and live events;
- "dep=X, arr=?";
- "other" (logit 0).

Starting log-likelihood ratios:

| Evidence | ℓ |
|---|---|
| Override asserts h | +8 (always wins unless the admin set "verify") |
| Standing data asserts h | +1.7, replaced by the per-airline measured accuracy (§3.6) once n ≥ 50 legs |
| Learned | `+min(1.0+0.7·n_days, 4.0)·exp(−age/21 d)` |
| Online cache asserts h | +1.9 (odbl) / +1.9 (restricted, non-public scopes only) |
| Variant callsign | −1.0 |
| Departure event at origin(h) | +3q; at another airport > 30 km away: −3q |
| Arrival event | ±3q |
| fits / uncertain / doubtful / reversed | +0.5 / 0 / −2 / −3 (+2 to the swapped hypothesis) |
| QNH match to B | +1 |
| SelAlt contradiction | −2 |
| Persistent heading away | −1.5 |

- `P = softmax`.
- Displayed tier:
  - **P ≥ 0.8**: the route is shown normally.
  - **0.5–0.8**: shown with "?".
  - **< 0.5 with dep known**: "EGLL → ?", plus an optional "heading towards" candidate list (medium+ airports within 50 km of the forward great circle, ranked by learned (dep, airline) frequency). This list is labelled as a guess, never as the route.
  - **Otherwise**: nothing is shown.
- Switching hypothesis requires the new best to lead by Δlogit ≥ 1.0 on two consecutive evaluations.
- **Reason string**: generated from the top three evidence terms, e.g. "learned 9 days · departed EGLL 27R (observed) · fits track". It is public-safe: it never mentions station identities.

### 3.4 Progress and ETA (routes/eta.go)

- **Remaining distance**: `s = d_gc(P,B) + X_B`. X_B is the learned arrival excess per airport. Defaults are 25 / 12 / 4 NM for large / medium / small airports. When the network-inferred runway in use is known, the path goes via the FAF.
- **Time profile**:
  - Cruise at the current gs until the 3:1 top of descent.
  - Above FL100 at (gs+290)/2 kt.
  - FL100 to 10 NM out at 230 kt.
  - Final 10 NM at V_app by category: A1 90, A2 110, A3 140, A4/A5 150 kt.
  - Hold allowance when `detectHold` is active.
  - On final, `detectApproach.ETASec` is used directly.
- **Learned bias**: the median residual at the 200, 100 and 40 NM milestones, per airport and per hour bucket.
- **Uncertainty**: σ = max(IQR/1.35, 1.5 min + 0.02·t_rem).
- **When shown**: only when P ≥ 0.8 and s < 1500 NM. Otherwise progress % only.
- **Fields**: `ETA` (Unix seconds), `ETASigma` (seconds), `DistRemainKm`, `AlongPct`. The UI shows the ETA in station-local time, with the timezone taken from the browser.

### 3.5 Network QNH (routes/qnh.go)

- Per airport, take the median BDS 4,0 QNH of aircraft < 5000 ft AGL within 30 km over the last 30 min. The data stays in memory.
- It gives destination evidence (+1) and better AGL. No METAR source is needed.

### 3.6 Learning (routes/learn.go)

- Every closed leg with a dep or arr confidence ≥ 0.7 produces an observation:
  - **Upsert** into `routes_obs(callsign, dep, arr, day)`: `n += 1`, `w += dep_conf·arr_conf`, plus a running circular mean of the departure time of day.
  - **One-sided legs** are stored with an empty dep or arr.
- **Weighting** at rebuild time:
  - `exp(−age/21 d)`.
  - ×0.3 for observations before the last IATA season change (last Sunday of March and of October).
- **Posterior**:
  - Dirichlet `P(h) = (n_h + α·π_h)/(Σn + α)` with α = 2.
  - π = 0.8 on the standing route.
- **Promotion and override**:
  - **Promote** to `routes_learned` at ≥ 2 distinct days, P ≥ 0.8 and last seen ≤ 14 days ago.
  - **Override standing** at ≥ 3 contradicting days, P ≥ 0.85 and at least one T1/T2 or L1/L2 event. The standing row is then marked stale (`routes_learned.stale_standing=1`).
- **Same callsign, several routes**: keep up to 3 hypotheses with their time of day. Choose by the observed departure time, or by `now − (d_gc(A,P)+15 NM)/(0.85·gs)`.
- **Rebuild**: incremental per affected callsign when a leg closes, plus a nightly full pass at 03:xx local with jitter. The nightly pass also prunes `routes_obs` older than 120 days.
- **Accuracy book-keeping**: for every leg with T1/T2 and L1/L2 evidence, record whether standing, learned and online agreed. The result goes to `routes_accuracy(source, airline, day, agree, total)`. This calibrates ℓ and drives the "Route accuracy" panel (§6.6), our evidence against tar1090.
- **Never learn from `odbl` or `restricted` answers.**

**Hub (network) learning:**

- Stations upload compact leg events over the existing uplink: `{icao, callsign, type T/L, airport, runway, t, q}`, about 40 B each.
- The hub joins a departure from station X with an arrival from station Y when:
  - the icao matches;
  - the callsign matches (or is blank on one side);
  - `120 ≤ gc/Δt ≤ 560 kt` (≤ 300 for A1/A2);
  - no event lies in between.
- The joined legs feed the hub's own learner.
- Uploads follow federation/uplink privacy policy: a station whose policy hides its position still sends events. Airports are public facts, and the station ID is never stored with the observation.

## 4. Overrides and corrections

- `routes_override(callsign PK, airports TEXT, mode 'fixed'|'verify'|'block', note, author, created, expires)`:
  - `fixed`: always this route.
  - `verify`: a prior of +3 that can still be contradicted.
  - `block`: never show a route for this callsign.
  - `expires` is optional (e.g. a season).
- An audit log goes to `routes_override_log(id, callsign, before, after, author, t)`.
- Overrides are editable from:
  - the detail panel ("Correct route…", admin; owner on a station);
  - Settings → Routes → Overrides (a table with search);
  - the API;
  - an operator file `routes.overrides.csv` in the data dir (`callsign,airports,mode,note`), loaded at start and on SIGHUP or file change. File rows are "locked", shown read-only in the UI, matching the config layering model.
- **Hub**:
  - Hub admins edit network overrides.
  - Feeder owners can submit **suggestions** (`routes_suggest`). Each one is a vote that adds +1.5 per distinct owner (cap +4) as evidence; it is not an override. Admins see a queue with learned evidence alongside and accept or reject.
- **Upstream**: the detail panel shows a "Report to VRS standing data" link to `https://sdm.virtualradarserver.co.uk/Edit`. The link is plain, with no data in the URL beyond what the SDM site supports; we do not prefill personal data. The hub admin can export accepted corrections and confidently learned routes as a VRS-format CSV (§7) to submit manually.

## 5. Storage (SQLite; migration owner `routes`, airview.db only)

`store.RegisterMigration("routes", 1, ...)` holds the tables below. Every table is WITHOUT ROWID where the primary key is natural. Sizes are estimates for a busy station / a 1000-feeder hub.

```sql
CREATE TABLE routes_cache(            -- online answers only (replaces navdata_route_cache; old table left, read once to migrate then ignored)
  callsign TEXT NOT NULL, provider TEXT NOT NULL,
  fetched INTEGER NOT NULL, expires INTEGER NOT NULL,
  found INTEGER NOT NULL, airports TEXT NOT NULL DEFAULT '', airline TEXT NOT NULL DEFAULT '',
  updated INTEGER,                      -- provider's own timestamp
  PRIMARY KEY(callsign, provider)) WITHOUT ROWID;
CREATE INDEX routes_cache_exp ON routes_cache(expires);
-- TTL: found 24 h, not found 12 h, implausible-at-fetch 1 h; size cap 50k rows (LRU by fetched).

CREATE TABLE routes_override(callsign TEXT PRIMARY KEY, airports TEXT NOT NULL, mode TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '', author TEXT NOT NULL, created INTEGER NOT NULL, expires INTEGER) WITHOUT ROWID;
CREATE TABLE routes_override_log(id INTEGER PRIMARY KEY, callsign TEXT, before TEXT, after TEXT, author TEXT, t INTEGER);
CREATE TABLE routes_suggest(callsign TEXT, owner TEXT, airports TEXT, t INTEGER, state TEXT,
  PRIMARY KEY(callsign, owner)) WITHOUT ROWID;                     -- hub only in practice

CREATE TABLE routes_obs(callsign TEXT, dep TEXT, arr TEXT, day INTEGER,
  n INTEGER, w REAL, tod_sin REAL, tod_cos REAL, last_t INTEGER, events INTEGER, -- bitmask tiers seen
  PRIMARY KEY(callsign, dep, arr, day)) WITHOUT ROWID;               -- ~ legs/day; pruned >120 d
CREATE TABLE routes_learned(callsign TEXT, rank INTEGER, airports TEXT, p REAL, n_days INTEGER,
  last_t INTEGER, tod_min INTEGER, stale_standing INTEGER,
  PRIMARY KEY(callsign, rank)) WITHOUT ROWID;                         -- rank 0..2

CREATE TABLE routes_leg(flight INTEGER, seq INTEGER, start INTEGER, "end" INTEGER, callsign TEXT,
  dep TEXT, dep_conf REAL, dep_rwy TEXT, dep_t INTEGER, dep_tier TEXT,
  arr TEXT, arr_conf REAL, arr_rwy TEXT, arr_t INTEGER, arr_tier TEXT,
  route TEXT, route_src TEXT, route_p REAL, diverted INTEGER,         -- route shown at leg close (open/own classes only)
  PRIMARY KEY(flight, seq)) WITHOUT ROWID;
CREATE INDEX routes_leg_route ON routes_leg(route, start);
CREATE INDEX routes_leg_dep ON routes_leg(dep, start);
CREATE INDEX routes_leg_arr ON routes_leg(arr, start);

CREATE TABLE routes_daily(day INTEGER, route TEXT, airline TEXT, n INTEGER,
  PRIMARY KEY(day, route, airline)) WITHOUT ROWID;                    -- stats; route='EGLL-KJFK'
CREATE TABLE routes_seen(route TEXT PRIMARY KEY, first_t INTEGER, last_t INTEGER, n INTEGER) WITHOUT ROWID; -- "new route" detection
CREATE TABLE routes_accuracy(day INTEGER, source TEXT, airline TEXT, agree INTEGER, total INTEGER,
  PRIMARY KEY(day, source, airline)) WITHOUT ROWID;
CREATE TABLE routes_meta(k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID;    -- provider state, last refresh, ETags
```

- **navdata migration**: add `navdata_airline(icao PK, iata, name, country, positioning_re, charter_re) WITHOUT ROWID` with `store.RegisterMigration("navdata", <next>, ...)`. Group A takes the next free navdata version number at implementation time.
- **Pi rules**:
  - All writes from the 1 Hz path are queued and flushed every 10 s in one transaction.
  - Leg and obs writes happen at flight close.
  - Stats are aggregated per day.
  - No table grows without a bound: cache cap 50k rows, obs pruned at 120 d, legs follow the flights retention, daily stats kept 400 d.
- `flights.dep` / `arr` keep being written by engine/airports.go, which now calls `routes.Events` for better guesses.
- `routes_leg` is the rich record, and reports join it on `flight`.
- Exports and federation use only `open`/`own`-class route values. `routes_leg.route` is never written from a restricted or ODbL answer; in that case it stores the dep/arr from events only.

## 6. Product surface

### 6.1 Live model (public-safe), `model.RouteShort` extended

New fields, appended. Every one is omitted when empty.

```go
type RouteShort struct {
    From, To, FromIATA, ToIATA, FromName, ToName string // ACTIVE leg (was first->last)
    Via        int      `json:"via,omitempty"`        // stops in whole route (kept)
    Plausible  bool     `json:"plausible"`            // kept for old clients: state in {fits, uncertain}
    Source     string   `json:"source"`               // "override"|"learned"|"standing-data"|"adsbdb"|"hexdb"|"adsbim"|"adsblol"|"aeroapi"|"observed"
    Checked    bool     `json:"checked,omitempty"`
    // new
    State      string   `json:"state,omitempty"`      // fits|uncertain|doubtful|reversed|contradicted|unchecked
    Conf       float64  `json:"conf,omitempty"`       // fused P, 0..1, 2 dp
    Reason     string   `json:"reason,omitempty"`     // <=120 chars, no station identifiers
    Stops      []string `json:"stops,omitempty"`      // full ordered route codes when >2 airports
    Leg        int      `json:"leg,omitempty"`        // 1-based active leg
    Legs       int      `json:"legs,omitempty"`
    AlongPct   float64  `json:"along,omitempty"`      // 0..100 on active leg
    DistRemKm  float64  `json:"distRem,omitempty"`
    ETA        int64    `json:"eta,omitempty"`        // Unix s
    ETASigma   int      `json:"etaSigma,omitempty"`   // s
    Kind       string   `json:"kind,omitempty"`       // ""|positioning|charter|circuit
    Observed   string   `json:"obsFrom,omitempty"`    // observed departure airport when contradicted
    Class      string   `json:"lic,omitempty"`        // open|own|odbl|restricted (server-side projection uses this)
    Flags      []string `json:"flags,omitempty"`      // "cs-mismatch","stale-standing","variant"
}
```

- **Scope projection**:
  - `publicdto` and federation remove routes whose `Class == "restricted"`. Station-scope SSE keeps them.
  - The `Reason` text never contains station names or SIDs.
  - ETA and progress are derived from the aircraft's public position, so they are public-safe.
- **Bandwidth**: about 120 B per aircraft for typical fields. The SSE delta encoder already sends only changed fields. ETA is rounded to 30 s so it does not churn deltas.

### 6.2 Map

- **Selected aircraft**:
  - The great-circle route path, with flown and remaining parts styled differently: completed leg portion faint solid, remaining portion dashed, other legs of a multi-stop route thin dotted. The live track trail stays as it is.
  - Origin and destination markers (and via markers) with IATA labels.
  - Line colour by state: normal, uncertain grey, doubtful/contradicted amber with a dashed outline.
  - If the route is contradicted and an observed origin exists, a thin line runs from the observed origin instead.
  - The path is computed server-side as today (about 50 km steps, antimeridian-safe), split at the projected position point.
- **"Routes for all"** layer toggle (off by default): draws remaining legs for every aircraft in the current filter, capped at 300 lines, with simplified 200 km steps computed client-side from airport coordinates delivered once per route via `/api/routes/airports?codes=`.
- **Hub spider view**: selecting an airport marker draws all live inbound and outbound lines for that airport.
- **Airport click popup**: "Arrivals / Departures board" link plus inbound count.

### 6.3 Detail panel

- Route block: `EGLL London → (EDDF) → LIRF Rome · leg 2/2`.
- Progress bar with flown / remaining km and `ETA 14:32 ±3 min`.
- State chip (Fits / Uncertain / Doubtful / Reversed? / Contradicted, plus observed origin).
- Source chip with confidence: "Learned · 9 days · 92 %". Hovering shows the reason string and a licence attribution line.
- Kind tag (Positioning / Charter / Circuit).
- Actions:
  - "Correct route…" (admin/owner) opens the override dialog, prefilled.
  - "Suggest correction" (hub feeder owners).
  - "Report to VRS SDM" (link).
  - "Show route flights" opens a report filtered on this route.
- Lazy details come from `GET /api/routes/{cs}`: all hypotheses with P, evidence terms, the provider answers the viewer may see, and the learned history (last 10 days with dep/arr and events).

### 6.4 Aircraft list: columns, filters, search

- **New columns** (hidden by default except Route):
  - Route (active leg IATA, or ICAO by the user's units setting; "?" suffix; amber when doubtful);
  - From, To, Via;
  - Progress %;
  - ETA;
  - Route source;
  - Route conf;
  - Airline (name from `navdata_airline`).
- **Filter grammar**, also used by the search box and kept in the URL (`?f=`):
  - `from:EGLL`, `to:JFK` (IATA and ICAO interchangeable via a client-side airport alias map of about 40 KB gz, loaded lazily);
  - `via:FRA`;
  - `airport:LHR` (from, to or via);
  - `route:LHR-JFK` (either direction with `route:LHR~JFK`);
  - `airline:BAW` or `airline:BA`;
  - `country-from:GB`, `country-to:US`;
  - `state:doubtful`;
  - `src:learned`;
  - `kind:positioning`;
  - `eta<30m`;
  - `noroute`;
  - wildcards `*` and `?`, negation `-to:EGLL`.
- **Fade mode**: fade non-matching aircraft on the map instead of hiding them (an FR24-style toggle).
- `filters.js` gets a "Route / airport" section with chips for these filters. Evaluation is client-side on the live picture, with no server call.

### 6.5 Airport boards (new page `#/airports/{code}` and map popup)

- Built from live data only, so no schedule licence is needed:
  - **Arrivals**: inbound aircraft with P ≥ 0.5 and To = code, sorted by ETA. Each row shows callsign, airline, from, ETA ±σ, distance, state.
  - **Departures**: aircraft whose departure event at this airport happened within the last 60 min, or whose From = code and which are still within 300 km climbing.
  - **Recently landed**: L-events in the last 60 min.
- Served by `GET /api/routes/board/{code}`, computed in memory from the live picture. Cost is O(aircraft), cached for 5 s.
- On a hub it is AccessPublic with restricted-class routes removed.

### 6.6 Reports and statistics (new "Routes" tab in Stats; Reports builder additions)

- **Busiest routes, airports (in, out and both) and airlines**: per day, 7 d, 30 d or custom, read from `routes_daily` and the routes_leg indexes.
- **New routes**: routes whose `routes_seen.first_t` falls in the period. A "new route first seen" feed.
- **Route coverage**:
  - the share of airline-callsign flights with a displayed route (P ≥ 0.8), by source;
  - the share where both ends were observed (T1–T2 and L1–L2).
- **Route accuracy**: standing vs learned vs online agreement with observed legs, per airline (from `routes_accuracy`), plus a reliability diagram and Brier score. This is the public scoreboard showing that AirView beats raw standing data.
- **Diversions**: legs where `arr` ≠ the route's To with arr_conf ≥ 0.75.
- **Origin and destination countries**: a map choropleth that reuses stats/countries.js styling.
- **Reports builder** (reports/builder.js) adds filters and columns:
  - Route;
  - From/To (filed route; the existing Dep/Arr remain observed);
  - Diverted;
  - Route source;
  - Airline.
- A flight report "Airports" card shows filed vs observed, with tiers and runways.

### 6.7 Alerts (new condition types in internal/alerts)

- New conditions:
  - `routeFrom`, `routeTo`, `routeVia`, `routeAirport`: Values are airport code globs, with IATA/ICAO resolved at rule normalise.
  - `route`: Values `"EGLL-KJFK"`, with `*` allowed.
  - `routeState`: Values among fits, uncertain, doubtful, reversed, contradicted.
  - `etaWithin`: Max minutes to ETA at the rule's `routeTo`, else at any destination.
  - `newRoute`: Value bool; the first time this route is seen at the station or network.
  - `routeKind`: Values among positioning, charter, circuit.
- Shipped default rules (disabled) as one-click templates:
  - "Arrivals to my airport within 20 min" (prompts for the airport);
  - "New route seen";
  - "Positioning flight nearby";
  - "Diversion suspected" (contradicted, plus arr event away from To).
- Template variables for messages: `{route}`, `{from}`, `{to}`, `{eta}`, `{routeSource}`.

### 6.8 Exports

- `GET /api/routes/export/legs.csv?from&to`: legs with dep/arr, events, route (open/own only). Admin.
- `GET /api/routes/export/learned.csv`, in VRS format `Callsign,Code,Number,AirlineCode,AirportCodes`: learned routes with P ≥ 0.9 and ≥ 3 days, for submission to the VRS SDM. Released under CC0 with a header comment. Admin.
- `GET /api/routes/export/overrides.csv`: round-trips with `routes.overrides.csv`. Admin.
- Route stats JSON and CSV via the stats endpoints (`?format=csv`).
- `/api/db/export/{table}.csv` already covers raw tables. Restricted-class rows never exist outside `routes_cache`, which is excluded from that export.

## 7. API endpoints (server/routes_ext.go entries; new handlers in server/api_routes.go)

| Method | Path | Access | Purpose |
|---|---|---|---|
| GET | `/api/routes/{callsign}?icao&lat&lon` | Viewer (projected) | Full route view: hypotheses, evidence, path, airports, state, ETA. Uses the cache and offline data only. It **never triggers an online lookup for public or anonymous callers**. Owner and admin calls may enqueue one (`?online=1`), subject to the budget. |
| POST | `/api/routes/batch` `{callsigns:[...≤200]}` | Viewer | Route shorts for the list/filter UI (offline + cache). |
| GET | `/api/routes/board/{airport}` | Viewer | Arrivals, departures and landed. |
| GET | `/api/routes/airports?codes=A,B` | Public | Airport coords, names and IATA/ICAO for client-side drawing and the alias map. |
| GET | `/api/routes/stats?kind=routes|airports|airlines|new|coverage|accuracy&from&to&limit&format` | Viewer | Statistics. |
| GET | `/api/routes/legs?route&from&to&dep&arr&airline&limit` | Owner | Leg search (reports). |
| GET/PUT/DELETE | `/api/routes/overrides[/{callsign}]` | Admin (station: Owner) | Overrides, with audit. |
| GET | `/api/routes/overrides/log` | Admin | Audit log. |
| POST | `/api/routes/suggest/{callsign}` | Owner (hub) | A feeder-owner suggestion. |
| GET/POST | `/api/routes/suggest` | Admin | Review queue; accept or reject. |
| GET | `/api/routes/providers` | Admin | Provider status: enabled, class, budget used, backoff, last error or policy text. |
| POST | `/api/routes/providers/{name}/test` | Admin | One lookup of a fixed public callsign (`BAW123`) for testing. |
| POST | `/api/routes/cache/clear` | Admin | Replaces `/api/navdata/route-cache/clear`, which is kept as an alias. |
| POST | `/api/routes/refresh` | Admin | Forces a standing-data conditional refresh. |
| GET | `/api/routes/export/{legs.csv|learned.csv|overrides.csv}` | Admin | Exports. |

- `/api/navdata/route/{callsign}` stays as a compatibility alias of `/api/routes/{cs}`, with the same no-online rule for public callers. This fixes the budget-drain and injection issue.
- Per-IP public rate limits apply as today.
- Every endpoint is documented in the route table, so docs/API.md is generated or updated from it.

## 8. Configuration (model.NavDataConfig gains `Routes RoutesConfig json:"routes"`)

Environment names follow `config.EnvName` (camelCase → `_`, prefix `AIRVIEW_`). All keys can also be set in `airview.jsonc` and with `-set`.

| Key | Env | Default | Notes |
|---|---|---|---|
| `navData.autoRefresh` | `AIRVIEW_NAV_DATA_AUTO_REFRESH` | `true` | Daily conditional refresh of airports, routes and airlines, with jitter. |
| `navData.routesUrl` | `AIRVIEW_NAV_DATA_ROUTES_URL` | mirror URL | Override or self-host (CC0). |
| `navData.routes.enabled` | `AIRVIEW_NAV_DATA_ROUTES_ENABLED` | `true` | The master switch for the route feature. |
| `navData.routes.online` | `AIRVIEW_NAV_DATA_ROUTES_ONLINE` | `false` | Global online kill switch. The legacy `navData.routesOnline` maps here (read on load, then dropped). |
| `navData.routes.providers` | `AIRVIEW_NAV_DATA_ROUTES_PROVIDERS` | `["adsbdb"]` | Ordered list of providers used when `online` is on. Allowed: adsbdb, hexdb, adsbim, adsblol, aeroapi, airlabs, aviationstack. |
| `navData.routes.dailyCap` | `..._DAILY_CAP` | `1000` | Per provider. |
| `navData.routes.minIntervalSec` | `..._MIN_INTERVAL_SEC` | `2` | Per-callsign providers; routeset providers are fixed at ≥ 30. |
| `navData.routes.stableSec` | `..._STABLE_SEC` | `45` | Callsign stability before an online query. |
| `navData.routes.sendPosition` | `..._SEND_POSITION` | `false` | Aircraft lat/lon to routeset providers. |
| `navData.routes.contact` | `..._CONTACT` | `""` → project URL | User-Agent contact. Redacted for non-admins. |
| `navData.routes.adsbimApproved` | `..._ADSBIM_APPROVED` | `false` | Required before adsbim is used. |
| `navData.routes.keys.aeroapi` / `.airlabs` / `.aviationstack` | `..._KEYS_AEROAPI` (+ `_FILE`) | `""` | Secrets: never sent to the UI, locked when they come from env or a file. |
| `navData.routes.ttlFoundH` / `ttlMissH` / `ttlImplausibleH` | … | `24 / 12 / 1` | Cache TTLs. |
| `navData.routes.learn` | `..._LEARN` | `true` | Learner on. |
| `navData.routes.publicRestricted` | `..._PUBLIC_RESTRICTED` | `false` | Always false on a hub. On a station it allows restricted-class routes in public share links (only when the operator has the provider's permission). |
| `navData.routes.eta` | `..._ETA` | `true` | |
| `navData.routes.overridesFile` | `..._OVERRIDES_FILE` | `routes.overrides.csv` | In the data dir. |
| `navData.routes.hubEvents` | `..._HUB_EVENTS` | `true` | Station→hub leg-event upload (follows uplink policy). |

- `config/validate.go` checks bounds: cap 0–100 000; interval 1–3600 s; TTLs 0.1–720 h; providers from the known list; the contact must be a URL or e-mail (`validContact`).
- `fielddocs_gen.go` gets regenerated docs.
- `secrets.go` redacts `contact` and `keys.*`.
- The settings UI shows the privacy text exactly: "When on, callsigns of received aircraft with no offline route are sent to: <providers>. The receiver location is never sent. Aircraft positions are sent only to adsb.im/adsb.lol, and only if 'send position' is on."

## 9. Aggregator (hub) mode

- **One resolver on the hub** serves every feeder and viewer, so the online budget is shared and batched.
- **Feeders**:
  - Feeders in feeder-only mode do no route lookups.
  - Full stations linked to a hub keep their own resolver. They may optionally pull hub-learned routes (`own` class, CC0) with a daily delta (`GET /api/routes/export/learned.csv?since=`), which is the network effect.
- **Public DTO**: route only when its class is open, own or odbl. ODbL attribution is shown in the public map credits.
- **Federation peers**: `policy.go` currently strips Route. It stays stripped for v1, because peers run their own resolvers.
- **Learning**: the hub learner consumes joined cross-station events. Event uploads are rate-capped at 1 event per aircraft per 30 s per station.
- **Scale budget for 1000 feeders and about 15k aircraft visible**:
  - LRU 64k entries, about 20 MB.
  - Scoring every 30 s means about 500 evaluations per second, each a few microseconds.
  - SQLite reads only on miss.
  - Daily stats write about 50k rows per day.
- **Viewer endpoints**: `/api/routes/{cs}`, `board` and `stats` are served from memory with short caches and per-IP limits. No viewer action can trigger an online query.

## 10. Offline behaviour

- **No network at all**: standing data, if previously imported, plus overrides, the learner, scoring, ETA, boards and stats all work. Auto-refresh retries with backoff and shows "last refreshed N days ago" in settings, with a warning after 30 days.
- **No reference data imported**: the route feature is idle. The list, map and detail panel show a one-line hint linking to Settings → Reference data, and learning still collects events once airports exist.
- **Provider unreachable**: it backs off silently. The status shows in Settings → Routes → Providers.

## 11. Tests (no live network calls; CI blocks outbound HTTP via a test transport)

- **Recorded fixtures** under `internal/routes/testdata/providers/`:
  - one JSON/HTTP transcript per provider (adsbdb, hexdb, adsbim, adsblol, aeroapi);
  - found, not found, 429 with Retry-After, 403 with policy body, 5xx, malformed, and the empty 201 (adsb.lol).
  - The fixtures are hand-written from the documented schemas and the 2026-11-15 probe shapes, using only the public callsigns BAW123, EZY12AB, RYR1234, DLH400 and QFA1.
  - They are replayed through an `http.RoundTripper` fake. Tests assert the User-Agent format and the absence of any receiver location, e-mail or feeder ID in requests.
- **Provider and limiter**: daily cap, min interval, singleflight, backoff and jitter bounds, 24 h disable after 403, batch ≤ 100 and ≥ 30 s spacing for routeset, and position omitted unless `sendPosition`.
- **Licence projection**: a golden test that restricted routes never appear in publicdto, federation, exports, `routes_leg.route`, `routes_obs` or stats. Extend `server/privacy_golden_test.go` fixtures in group B.
- **Scoring**:
  - table tests for the ellipse z at D = 150/300/1000/6000;
  - the hub-overflight case (FL380 over EGLL on EGPH-LIRF → not "fits" via the airport rule);
  - reversed detection;
  - vertical terms;
  - hysteresis;
  - the multi-leg active leg (QFA1 YSSY-WSSS-EGLL).
- **Events**: drive `internal/sim` scenarios (takeoff, final, flare, rollout, taxi-in, go-around, touch-and-go, hold, lost-below-horizon) and check tier, airport and runway. Include closely spaced airports (EGLL vs EGWU, EGKK vs EGKR) and the category filter (an A3 must not land at a 600 m grass strip).
- **Learner**: synthetic obs streams covering promotion at 2 days, override at 3 days with T/L evidence, season down-weighting, time-of-day multi-route, and the 120-day prune.
- **Fusion**: calibration sanity (the probabilities sum to 1); the override wins; the display tiers.
- **ETA**: synthetic straight-in and holding profiles, with tolerance checks.
- **Callsign hygiene**: registration-equals-callsign, BAW101P vs BAW101 kept distinct, EZY alphanumerics never stripped.
- **Import**: the mirror CSV.gz parser, a 304 path, and airlines.csv with its regexes. The tiny fixture zip already exists in `testdata_test.go`.
- **Server**: the route table docgen test, access classes (public callers cannot pass `online=1`), and CSV exports.
- **Web**: filter-grammar unit tests (existing JS test harness if present, else a small node-free test page under `web/static/js/map/filters.test.html` run in the browser pane).
- **Benchmarks**: resolver hit path < 1 µs; scoring 15k aircraft < 50 ms per cycle on a Pi 4 (measured with `go test -bench`, reported in group A's PR).

## 12. Work breakdown (parallel agents, disjoint file ownership)

The contracts are fixed by this document: the `routes.Service` interface (below), `model.RouteShort` (§6.1), the JSON shapes in §7 and the config keys in §8.

```go
// internal/routes/service.go (Group A writes; B consumes)
type Service interface {
    Live(a *model.Aircraft, now time.Time) *model.RouteShort        // 1 Hz, no I/O
    Detail(ctx context.Context, cs string, icao uint32, p *navdata.Point, scope Scope, allowOnline bool) (*Detail, error)
    Batch(cs []string, scope Scope) map[string]*model.RouteShort
    Board(code string, scope Scope) (*Board, error)
    Stats(ctx context.Context, q StatsQuery) (*StatsResult, error)
    Legs(ctx context.Context, q LegQuery) ([]Leg, error)
    CloseFlight(ctx context.Context, f FlightTrack) ([]Leg, error)  // called by engine at flight close
    HubEvent(ev LegEvent)                                           // hub ingest
    Overrides() OverrideStore; Suggestions() SuggestStore
    Providers() []ProviderStatus; ClearCache(ctx context.Context) error; Refresh(ctx context.Context) error
    Export(ctx context.Context, w io.Writer, kind string) error
    Attribution() []string
}
```

**Group A: routes core and data** (owner: `internal/routes/**`, `internal/navdata/importer.go`, `internal/navdata/ourairports.go`, `internal/navdata/db.go`, `internal/navdata/schema.go`, `internal/navdata/route.go`, `internal/navdata/assoc.go`, `internal/navdata/WIRING.md`, a new `internal/navdata/airlines.go`)

- Provider layer and batcher; resolver and LRU; the cache and the `routes` migration.
- Mirror refresh and airlines import; the `AirportByCode` fix.
- Events, scoring, fusion, ETA, network QNH, learner, accuracy, stats aggregation, board, overrides file loader and exports.
- All Go tests and fixtures under `internal/routes/testdata`.
- `navdata/route.go` adsbdb code becomes a deprecated wrapper delegating to `routes`, or stays for its existing callers until B switches.

**Group B: integration (engine, model, config, server, privacy, alerts, hub)** (owner: `internal/engine/ext.go`, `internal/engine/airports.go`, `internal/model/aircraft.go`, `internal/model/config.go`, `internal/config/{validate.go,fielddocs_gen.go,secrets.go,defaults*}`, `internal/server/routes_ext.go`, a new `internal/server/api_routes.go`, `internal/server/api_ext.go` (navdata route alias only), `internal/server/api_replay.go` (route filters), `internal/privacy/publicdto/publicdto.go`, `internal/federation/policy.go`, `internal/alerts/{rule.go,match.go,defaults.go,template.go}`, `internal/hub/**` and `internal/uplink/**` (leg-event message), and server privacy golden fixtures)

- Wires `routes.Service` into the engine 1 Hz enricher and flight close.
- Builds the endpoints in §7, config §8, scope projection and alert conditions.
- Starts against a stub `Service` until A lands.

**Group C: web UI** (owner: `web/static/js/map/{detail.js,list.js,filters.js,extras.js}`, `web/static/js/pages/map.js`, `web/static/js/pages/settings.js` (Reference data / Routes section), a new `web/static/js/pages/airports.js`, a new `web/static/js/stats/routes.js`, `web/static/js/pages/stats.js` (tab registration), `web/static/js/reports/{builder.js,flight.js}`, `web/static/js/alerts/editor.js`, related CSS and the router entry in `web/static/js/app.js`)

- Covers §6.2–6.7 UI: the filter grammar with URL state and fade mode; the route layer with flown/remaining styling and "routes for all"; the detail block; airport boards; the stats tab; the reports additions; the alert condition editors; the provider settings with privacy text and attribution; the override dialog; and the VRS SDM link.
- Builds against the JSON in §6.1 and §7, using mock JSON until B lands.

**Group D: docs and licences** (owner: `docs/API.md`, `docs/CONFIG.md`, `docs/ARCHITECTURE.md` (route section), `docs/manual/02-station-01-map.md`, `02-station-03-stats.md`, `02-station-09-alerts.md`, `02-station-12-settings.md`, `03-network-operator.md`, `04-reference-1-configuration.md`, `04-reference-3-api.md`, the manual licences page, `docs/RELEASE-NOTES.md`, and this file)

- Documentation, licence and attribution table (§2.2), privacy text, and the WIRING drift fixes for server/navdata (WIRING.md in navdata is Group A's).
- The PDF manual is produced only at the very end, per the project goals.

Merge order: A and B in parallel (B against the stub), then C, then D. Each group runs `go test ./...` and `go vet` on its packages before handing off.

## 13. Decisions for the user (recommended defaults in bold)

1. **Reference-data auto-download and refresh.** Should AirView download airports and routes (about 6 MB, CC0 and public domain, no user data sent) automatically at first start and refresh them daily with jitter? **Recommended: yes, on by default** (`navData.autoRefresh=true`); it can be turned off for air-gapped sites.
2. **Online providers by default.** **Recommended: online lookups stay off by default**, with a one-click opt-in in Settings that lists the providers. When turned on, the default chain is adsbdb only. hexdb and adsb.im are offered but need the project maintainer to contact those services first; adsb.im additionally needs `adsbimApproved` for any hub or commercial use.
3. **Publishing learned routes.** **Recommended: release AirView-learned routes as CC0** (hub export in VRS format) and submit high-confidence corrections to the VRS SDM manually, so the whole community benefits. The alternative is to keep them private to the network.

---

## User decisions (2026-11-15) — binding

1. **Auto data: ON by default.** `navData.autoRefresh=true`: airports + VRS standing-data routes (CC0, ~6 MB) download at first start and refresh daily (conditional GET, jitter). Air-gapped sites can switch it off.
2. **Online lookups: ON by default, adsbdb only.** Only for callsigns that miss locally or fail plausibility; server-side only; polite-client rules (cache, batching, backoff, daily cap, project User-Agent without personal data). hexdb / adsb.im stay opt-in. adsbdb answers are cached but never merged into the CC0 learned/route tables (licence). Settings and first-run text must say that unknown airline callsigns are sent to adsbdb.com and how to turn it off.
3. **Learned routes: publish as CC0.** The hub exports learned routes in VRS format; the operator submits high-confidence corrections to the VRS SDM manually.
