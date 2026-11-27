# 4 Reference

This part of the manual is for looking things up. Every statement in it was checked against AirView 1.0.0: the setting tables and the endpoint index are generated from the program's own sources, the command options are the programs' own `-h` output, and the screenshots come from a demo station.

| Section | Contents |
|---|---|
| [4.1 Configuration reference](04-reference-1-configuration.md) | Where settings come from (defaults, `config.json`, operator file, `AIRVIEW_*` variables, `-set`), file format, secrets, every setting of `airview` with its environment variable, and the feeder's `feeder.json` and `feeder-setup.txt`. |
| [4.2 Command-line reference](04-reference-2-cli.md) | `airview` and its commands (`serve`, `config`, `admin`, `backup`, `restore`, `db snapshot`, `update`, `federation id`, `version`), `airview-feeder` and its commands, the health-check helper and the options of every installer. |
| [4.3 HTTP API overview](04-reference-3-api.md) | Conventions, access control, the endpoints most useful for monitoring and integration, and an index of all endpoints. |
| [4.4 Troubleshooting and FAQ](04-reference-4-troubleshooting.md) | Symptoms and fixes for the station, the feeder and the aggregator, and frequently asked questions. |
| [4.5 Glossary](04-reference-5-glossary.md) | The terms used in the interface, the settings and this manual. |
| [4.6 Licences and clean-room statement](04-reference-6-licences.md) | AirView's licence terms, the third-party software and data it uses, and how AirView stays free of GPL code. |

## Ports at a glance

| Port | Program | Direction | Purpose | Setting |
|---|---|---|---|---|
| 80 (fallback 8080–8089) | airview | in | web interface and API | `http.port` |
| 30154 | airview | in | hub data port for remote-data clients (admin login only in aggregator mode) | `hub.port`, `hub.enabled` |
| 30160 | airview (aggregator) | in | AVF/1 feeders, TLS 1.3 | `aggregator.avfPort` |
| 30004 | airview (aggregator) | in | Beast feeds from readsb / Ultrafeeder (no encryption) | `aggregator.beastPort` |
| 31090 | airview (aggregator) | in | mlat-client (no encryption; also UDP with `aggregator.mlat.udp`) | `aggregator.mlatPort` |
| e.g. 30170 | airview (aggregator) | in | AVFED/1 federation; no default, only when a listen address is configured | `aggregator.federation.listen` |
| 10003 / 30005 / 30002 / 30003 | your receiver or decoder | out from AirView | Beast (Radarcape / readsb), AVR, SBS input | `feeds[]`, feeder `receivers[].input` |
| 1234 | your `rtl_tcp` | out from AirView | RTL-SDR samples | `feeds[].sdr`, feeder input type `rtltcp` |
| 8099 | airview-feeder | in (127.0.0.1 by default) | feeder status page | `ui.addr` |
| 30105 | airview-feeder | in (127.0.0.1 by default) | MLAT results as Beast for your own map | `results.beastPort` |
| 30104 | readsb | out from the feeder | optional push of MLAT results into readsb | `results.readsbAddr` |
