# 4.6 Licences and clean-room statement

## 4.6.1 AirView's own licence terms

The Windows installers show the AirView licence agreement before installing (`LICENSE.txt` in the installation folder). In short:

- You may install and use AirView and the AirView Feeder on any number of computers you own or control, privately or commercially, and copy the installer for that purpose.
- AirView keeps its configuration, history and identity on your own computer. The feeder sends receiver data only to the aggregators you configure; by default your station is not shown on any public map.
- You are responsible for complying with your country's laws on receiving aeronautical radio signals and sharing the data received.
- **Not for navigation or safety.** AirView is for information and hobby use. It must not be used for navigation, air traffic control, collision avoidance or any purpose where its failure could cause injury, death or damage.
- The software is provided "as is", without warranty of any kind.
- Open-source components keep their own licences (4.6.2); nothing in AirView's terms limits your rights under them.

## 4.6.2 Third-party software included in AirView

AirView is written in Go and is a single, statically linked program; its web interface is served from the program itself (nothing is loaded from a CDN). It contains these third-party components, all under permissive licences. None is under the GPL, LGPL or AGPL. Their full licence texts are in `THIRD-PARTY-NOTICES.txt` in every release package and installation folder, and the bundled web libraries are also listed in **Settings → About & diagnostics** (figure 4.3).

### Go modules

| Component | Version | Licence | Used in |
|---|---|---|---|
| The Go standard library and runtime | 1.27.3 | BSD-3-Clause | every program |
| golang.org/x/sys | 0.50.0 | BSD-3-Clause | airview, airview-feeder |
| modernc.org/sqlite (pure-Go SQLite) | 1.62.0 | BSD-3-Clause (SQLite itself: public domain) | airview |
| modernc.org/libc, modernc.org/mathutil, modernc.org/memory | 1.80.1, 1.7.1, 1.12.1 | BSD-3-Clause | airview |
| github.com/remyoudompheng/bigfft | 2023-01-29 | BSD-3-Clause | airview |
| github.com/google/uuid | 1.6.0 | BSD-3-Clause | airview (Linux builds) |
| github.com/dustin/go-humanize | 1.1.0 | MIT | airview |
| github.com/jchv/go-webview2 | 2026-02-05 | MIT | airview (Windows desktop window) |
| github.com/jchv/go-winloader | 2025-04-06 | ISC | airview (Windows) |
| github.com/mattn/go-isatty | 0.0.24 | MIT | airview (Windows) |
| github.com/ncruces/go-strftime | 1.1.0 | MIT | airview (Windows) |

`airview-feeder` links only the Go standard library and golang.org/x/sys.

### Web interface

| Component | Version | Licence | Used for |
|---|---|---|---|
| MapLibre GL JS | 6.18.0 | BSD-3-Clause | map rendering |
| Apache ECharts (bundles zrender and d3-derived code, BSD-3-Clause) | 6.1.0 | Apache-2.0 | charts |
| Inter (variable font) | 5.3.0 | SIL Open Font License 1.1 | interface typeface |
| JetBrains Mono (variable font) | 5.3.0 | SIL Open Font License 1.1 | numeric typeface |

## 4.6.3 Data sources and services

AirView uses only free data and services that need no account or API key. Some are fetched only when you turn the feature on or ask for an import.

| Source | Used for | Licence / terms | When |
|---|---|---|---|
| OpenFreeMap (© OpenMapTiles, data from OpenStreetMap) | vector map tiles, fonts, sprites | free; attribution shown on the map | when the map is shown |
| OpenStreetMap | map data, optional raster base map | © OpenStreetMap contributors, ODbL; OSMF tile usage policy (interactive viewing only) | optional base map |
| Open-Meteo | weather, upper-air profiles, terrain elevation (Copernicus DEM) | CC BY 4.0 | `weather.enabled` (default on) |
| RainViewer | precipitation radar overlay | personal / educational use with attribution | `weather.radarOverlay` (default off) |
| AWS Terrain Tiles (Mapzen / Tilezen) | terrain line-of-sight analysis | open data, attribution required (shown in the Terrain section) | `terrain.autoPrefetch` or on request |
| OurAirports | airports, runways, navaids, countries | public domain | import on request |
| Virtual Radar Server standing data | callsign routes (offline) | CC0 1.0 | import on request |
| adsbdb.com | online route lookup (sends the callsign) | free API, used as is | `navData.routesOnline` (default off) |
| planespotters.net | aircraft photos (sends the ICAO address) | each photo © its photographer, shown with credit and link | `navData.photos` (default off) |
| tar1090-db / Mictronics | aircraft database | ODC-By 1.0 | import on request |
| OpenSky Network | aircraft database | provided as is | import on request |
| EGM96 geoid (NGA) | MSL ↔ ellipsoid heights | public domain | embedded |
| World Magnetic Model 2025 (NOAA/NCEI and BGS) | magnetic declination | public domain | embedded |

Be a good citizen: keep the tile cache on (`http.tileCache`) and the weather interval moderate.

## 4.6.4 Clean-room statement

Much of the ADS-B software ecosystem that AirView works with is licensed under the GPL or AGPL: readsb, dump1090 and its forks, tar1090, mlat-client, mlat-server, pyModeS, and rtl-sdr / rtl_tcp. **AirView contains no code from these programs.** Its decoders, Mode S and ADS-B parsing, multilateration server, clock synchronisation, RTL-SDR demodulator, feeder protocol (AVF/1) and installers are its own implementations.

The rules AirView is developed under:

1. **No copying, translating or paraphrasing** of GPL, LGPL, AGPL or licence-less code, including its structure, comments, distinctive constant tables and test vectors.
2. **Preferred sources of knowledge**, in order: official specifications and standards (ICAO Annex 10 and Doc 9871, RTCA DO-260B summaries, RFCs, the Jetvision documentation of the Beast/Radarcape formats); peer-reviewed papers and textbooks; black-box observation of the user's own equipment and of unmodified programs (their documentation and command-line help); permissively licensed code (MIT, BSD, ISC, Apache-2.0, public domain) with attribution.
3. **Interoperability facts** (a port number, a wire field's meaning) that only a GPL program documents may be confirmed by reading its source as a last resort; the fact is restated in AirView's own words and the reading is logged.
4. **Test data** comes from AirView's own simulators, from standards and papers, or from the user's own receivers, never from a GPL test suite.
5. **Data is not code, but it has licences too:** every dataset is recorded with its licence and the attribution AirView shows (4.6.3).

Where each component's knowledge came from is recorded, component by component, in the provenance log `docs/aggregator/CLEANROOM.md` that is part of the AirView source.

### Programs AirView works with but never ships

| Program | Licence | Relationship |
|---|---|---|
| readsb, dump1090 | GPL | Your decoder; AirView reads its Beast (30005), AVR, SBS or `aircraft.json` output, and an aggregator accepts readsb's Beast feeds (30004). |
| mlat-client | GPL-3.0 | Your MLAT client may connect to an AirView aggregator (port 31090); AirView speaks its protocol from its own clean-room implementation. |
| mlat-server | AGPL-3.0 | Behaviour reference only. An operator may run it as a separate process during a migration, feeding AirView's local SBS bridge. |
| tar1090 | GPL | A separately installed tar1090 can read AirView's tar1090-shaped `/data/aircraft.json` and `receiver.json`. |
| rtl_tcp (rtl-sdr) | GPL-2.0+ | Your own `rtl_tcp` serves an RTL-SDR dongle's samples; AirView demodulates them itself (feed type `rtltcp`). AirView can start your `rtl_tcp` as a child process (`sdr.spawn`), but never ships or downloads it. |

The feeder installer for Raspberry Pi may *offer* to run the upstream readsb installer as a separate program that you confirm; it never installs it silently.
