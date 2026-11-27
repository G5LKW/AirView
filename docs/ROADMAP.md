# AirView roadmap and improvement cycle

This file is the single source of truth for unattended work. Cloud sessions and scheduled routines read it,
work on the top unfinished item, and update it before they finish.

## Rules for every automated run
- Work on a branch named `auto/<topic>` off the latest `main`, or the newest open `auto/*` branch; never push to `main` directly.
- Open (or update) a pull request per feature; the owner merges.
- `gofmt`, `go vet` and `go test ./...` must be green (CGO_ENABLED=0, no new Go modules). Flaky timing tests: rerun once, then fix.
- Clean-room only: no GPL/AGPL code. Honest physics: never claim NTP ranging, or any accuracy that was not measured.
- Receiver-agnostic defaults: one antenna, RTL-SDR/readsb; Radarcape is optional.
- Never build release artifacts, Pi images or installers, and never create or commit signing keys. Those happen on the owner's PC.
- Commit author: `G5LKW <G5LKW@users.noreply.github.com>`.
- Before finishing a run: tick off what was done here, add any new ideas to "Improvement backlog", and note what was not verified.

## The cycle (repeat until every item is done)
1. **Pick** the top unchecked item below (or the oldest open review finding).
2. **Build** it with tests.
3. **Test** it: unit tests, plus a `-demo` run for UI features.
4. **Review** it critically: correctness, performance on a Pi 4, UI at 375 px and desktop, docs.
5. **Improve**: fix what the review found, and add follow-up ideas to the backlog.
6. **Record** progress in this file and in `docs/V1.1-PROGRESS.md`.

## 1.1 feature list (priority order)
> Note (2026-11-27): items 2-7 below (routes, TCAS, EHS weather, 8K heat map, feeding networks, learned prediction) are implemented on the unmerged branch `claude/zen-davinci-sbmb33` (interactive session, no PR yet). Automated runs skip them until the owner merges or drops that branch, to avoid duplicate, conflicting work.
- [x] Small fixes from `docs/aggregator/HOOKS.md` (branch `auto/small-fixes`, 2026-11-27):
  - flaky tests `TestMetSamplePruner` and `TestEpochCostGate` (plus `hub.TestLoopbackRoundTrip`);
  - `timing.js:125` suggests the Radarcape NTP service even without a Radarcape (also `timing/tdoa.js`);
  - `guess.js` text says "four" methods while it lists five;
  - show the registration/type source in the detail panel (server `regSource` / `typeSource` / `acdbAttribution`).
- [ ] Route lookups, per `docs/ROUTES-DESIGN.md`. Binding decisions: auto data download on; online adsbdb lookups on by default; learned routes published under CC0.
- [ ] TCAS RA events (DF16/DF0, BDS 3,0) with an events list and map markers.
- [ ] Mode-S EHS weather (BDS 4,4/4,5; winds and temperatures from 5,0/6,0), with a gridded layer, then fronts and short-term forecasting.
- [ ] 8K JPEG heat-map export.
- [ ] Feeding external networks (ADSBx, OARC, adsb.lol…) through the uplinks code, plus our own mlat-client output.
- [ ] Learned flight prediction from surrounding traffic, self-correcting against observed positions.
- [x] ACARS/VDL2 integration (acarsdec/dumpvdl2 JSON input) (branch `auto/acars`, 2026-11-27: `internal/acars`, `acars` config block, `/api/acars`, map detail panel group, Settings card).
- [ ] Interrogator radar locating (SSR bistatic; Ofcom data as a reference).
- [ ] On-device AI (Pi 4 class) with a daily summary.
- [ ] Information architecture and navigation overhaul, and chart explanations.
- [ ] Better Locate Me: reverse MLAT, statistics, and honest error bars.
- [ ] Better terrain model for coverage prediction.
- [ ] Remaining database-efficiency targets (`docs/perf/DB-EFFICIENCY-DESIGN.md`).

## Owner-only actions (do not automate)
- Create the real release signing key offline (`go run ./deploy/release keygen`); the 1.0.x test key is not for publication.
- Build releases with `deploy\build.ps1` on the Windows PC (needs the Pi OS base image).
- Test on real Pi, Linux and Docker hardware; choose a licence before making the repository public.

## Improvement backlog
_(Automated runs append ideas here.)_
- Commit the `cmd/airview/testdata/airview-1.0.0.db` fixture (owner: it only exists on the owner's PC; `.gitignore` now lets it in). Until then `TestDBMigrateOldFormat` and `TestDBVacuum` fail on every fresh clone.
- `internal/analytics` `TestLocate2Synthetic` takes minutes per sub-test on a 4-core VM and pushes the package past the default 10-minute `go test` timeout under a full parallel run; trim the 24 h synthetic cases or gate the slowest behind `-short`/an env var, or document `go test -timeout 20m ./...`.
- Show the registration/type source on the Aircraft page too (it still uses the client-side "(derived)" rule) using the new `regSource` / `typeSource` fields.
- ECharts "scroll" legend cuts off the next item's label on 375 px cards (HOOKS r101-verifier, charts.js).
- ACARS: persist messages (a small `acars_msg` table with retention) so they survive a restart; today they are memory-only.
- ACARS: an "ACARS" page or Reports tab with the full message list, search and label filter (the API already supports it); fits the information-architecture item.
- ACARS: decode common structured payloads (H1 position / weather reports, OOOI times, libacars-decoded fields from dumpvdl2/acarsdec `libacars` objects) into fields; show a badge on aircraft that sent messages in the last minutes.
- ACARS: verify both parsers against real decoder captures (acarsdec 4.x, dumpvdl2 2.x) and add them as test fixtures.
- `uplink.TestSimnetOutage30Min/spool` failed once under the full parallel suite ("pre-outage frame marked late"; passes 3x alone): investigate whether an in-flight frame acknowledged late is resent as backfill.
