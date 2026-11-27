# AirView Network documents

Documents for AirView's aggregator mode, feeders and own MLAT.

## For operators and feeder owners

| Document | For | Contents |
|---|---|---|
| [OPERATOR-GUIDE.md](OPERATOR-GUIDE.md) | aggregator operators | headless Linux VPS install, admin bootstrap, reverse proxy and TLS, firewall, running the network, scaling, backup, upgrade, troubleshooting |
| [FEEDER-GUIDE.md](FEEDER-GUIDE.md) | feeder owners | what is shared and who sees it, Pi/Linux, Windows, AirView uplink and readsb/mlat-client set-ups, claiming, privacy choices, data rights |
| [PRIVACY-NOTICE.md](PRIVACY-NOTICE.md) | operators (template to publish) | UK GDPR privacy notice |
| [LIA.md](LIA.md) | operators (template to keep) | legitimate interests assessment and DPIA screening |
| [CONSENT.md](CONSENT.md) | operators, UI and installer authors | the canonical consent and notice texts (C1-C8) |
| [RETENTION.md](RETENTION.md) | operators (published with the notice) | retention schedule, AirView and operator-side |
| [../CONFIG.md](../CONFIG.md) | everyone | generated reference of every setting and `AIRVIEW_*` variable |

## For developers

| Document | Contents |
|---|---|
| [DESIGN.md](DESIGN.md) | the final design; §19-§20 hold the user's decisions and publication requirements |
| [AVF.md](AVF.md) | normative AVF/1 wire specification |
| [MLATC.md](MLATC.md) | AirView's description of the mlat-client protocol on 31090 (clean-room, `internal/ingest/mlatc`) |
| [RESEARCH.md](RESEARCH.md), [CRITIQUE.md](CRITIQUE.md) | background research and design review |
| [CLEANROOM.md](CLEANROOM.md) | clean-room rule, provenance log, open review items |
| [PROGRESS.md](PROGRESS.md), [HOOKS.md](HOOKS.md) | implementation progress and cross-package requests |

## Consistency tests

This directory holds a small Go test package (`docs_test.go`, package `aggregatordocs`) so that the guides cannot silently drift from the code. `go test ./docs/aggregator` checks that:

- every setting path written in backticks (`aggregator.limits.maxStations`, `uplinks[].timestamps`, ...) exists in the generated [../CONFIG.md](../CONFIG.md);
- every "`setting` (default `value`)" statement and every "`setting` = `value`" row matches the default in CONFIG.md;
- the retention defaults in RETENTION.md equal `model.DefaultAggregator()`;
- every `AIRVIEW_*` variable mentioned exists;
- consent text C1 matches DESIGN §10.5 word for word and appears verbatim in the privacy notice and the feeder guide;
- every relative Markdown link resolves.

The user/operator PDF manual (DESIGN §20.4) is produced last, from these documents, once every phase is finished.
