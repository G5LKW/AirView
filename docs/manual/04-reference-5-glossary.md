# 4.5 Glossary

Terms as they are used in AirView's interface, settings and this manual. *Italic* names in the definitions refer to other entries.

| Term | Meaning |
|---|---|
| **ADS-B** | Automatic Dependent Surveillance – Broadcast: aircraft broadcast their own GNSS position, altitude, speed and identity on 1090 MHz (extended squitter). The main source of positions in AirView (position source code 0). |
| **ADS-R, TIS-B** | Rebroadcasts by ground stations: ADS-R relays UAT traffic, TIS-B relays radar tracks. Shown as position source 2. |
| **Admin** | Whoever may change settings. Standalone: the machine itself, private-network clients while no password is set, or anyone logged in with the admin password. Aggregator: accounts with the admin role (password plus authenticator code). |
| **Admission** | How an aggregator accepts new stations (`aggregator.admission`): `open`, `approve` (held as *pending* until an admin approves) or `invite` (default; an *enrol token* is required). |
| **AGL** | Above ground level: the antenna's height above the ground beneath it. |
| **Aggregator** | AirView in `mode: aggregator`: a network server that receives data from many *stations*, merges it, runs *MLAT* and serves a public map. |
| **Aircraft database** | Optional offline table of registrations, types and operators, imported on request (tar1090-db/Mictronics, OpenSky). Not shipped in the program. |
| **airview.jsonc** | The default name of the *operator file* in the data directory. |
| **Also feed others** | A *Beast* output from a feeder or station to a third-party aggregator (`uplinks[].type = "beast"`). |
| **Altitude bands** | The seven altitude classes used in statistics: < 1000 ft, 1–5k, 5–10k, 10–20k, 20–30k, 30–40k, ≥ 40k ft. |
| **Antenna Lab** | Page with coverage, antenna patterns and per-antenna performance. |
| **AVF/1** | AirView Feed protocol, version 1: AirView's own feeder protocol over TLS 1.3 (port 30160, ALPN `avf/1`). Carries timestamped frames, receiver records and MLAT results, with buffering and backfill. |
| **AVFED/1** | AirView's federation protocol between aggregators (TLS 1.3, port 30170 when listening). |
| **AVR** | A text format of raw Mode S frames (`*8D…;`, with timestamps as `@…;`), readsb/dump1090 port 30002. |
| **Backfill** | Data buffered while a feeder was offline and sent after it reconnects, at a limited rate. |
| **Beast** | The binary Mode S frame format with a 48-bit timestamp and signal level, originally of the Mode-S Beast receiver; spoken by Radarcape (port 10003) and readsb/dump1090 (port 30005). `beast_reduce_out` / `beast_reduce_plus_out` are readsb's reduced-rate variants used for feeding aggregators (port 30004). |
| **Bias tee** | A switch on some RTL-SDR dongles that powers an antenna amplifier through the coax cable. |
| **Claim, claim code** | Linking a feeder's station to your account on an aggregator's website, with a short-lived code from `airview-feeder claim` or the feeder's status page. |
| **Clock class** | The kind of timestamp clock a receiver has: `gps_utc`, `gps_gpst` (GPS-disciplined Radarcape), `gps_emul`, `hw12` (hardware 12 MHz counter), `sdr12` (readsb/dump1090 at 2–2.4 MS/s), `sdr12_hires` (airspy_adsb ≥ 12 MS/s), `sbs20` (SBS 20 MHz) or `none` (no usable timestamps: map only, no MLAT). |
| **Cloned feeder** | Two machines using the same identity key (typically a copied SD card). Fixed with `airview-feeder new-identity`. |
| **Coasting** | Showing an aircraft that sent no new position at its predicted position for up to `prediction.coastSec` seconds. |
| **config.json** | The settings file AirView writes itself (data directory). See *operator file*. |
| **Coverage** | The area (by bearing and altitude) in which the station receives aircraft; drawn as an outline on the map and analysed in the Antenna Lab. |
| **Data directory** | The folder with `config.json`, the databases, logs and identity (`-data`, `AIRVIEW_DATA`). |
| **dBFS** | Signal level relative to the receiver's full scale; negative, 0 = strongest possible. |
| **Demo mode** | The built-in traffic simulator (`demo.enabled`, `-demo`); no receiver needed. |
| **EGM96, geoid** | The model of mean sea level used to convert between *MSL* and *HAE* heights. Embedded in AirView. |
| **Enrol token** | A single-use secret an aggregator admin creates so a new station can join an invite-only network (`uplinks[].enrolToken`, `--token`). Expires after 7 days by default. |
| **Feed** | One data input of a station (`feeds[]`): a receiver or decoder output, another AirView, or the demo simulator. |
| **Feeder** | The `airview-feeder` program, or any station that sends data to an aggregator. |
| **Federation** | Exchange of data between AirView aggregators of different networks under explicit policies (`aggregator.federation`). |
| **HAE** | Height above the WGS-84 ellipsoid, what GPS reports. Differs from *MSL* by the geoid height (tens of metres). |
| **Health window** | After a self-update, the time the new version must stay healthy before it is kept; otherwise the previous version is restored. |
| **Hub port** | AirView's own data port (default 30154) that another AirView in *remote-data* mode logs in to with an HMAC challenge. |
| **ICAO address** | The aircraft's unique 24-bit Mode S address, shown as six hexadecimal digits (e.g. `4CA1FA`). |
| **Interrogator** | A secondary surveillance radar (or other Mode S/SSR ground station) whose interrogations make aircraft reply. Shown in the RF Lab; used by the experimental *SSR self-location*. |
| **Island guard** | Optional aggregator privacy feature (`aggregator.islandGuard`) that coarsens low aircraft heard only by hidden stations, so their position does not reveal where the station is. |
| **Locate Me** | Estimating the receiver's own position from the traffic it receives (`locate`). |
| **Locked setting** | A setting set by the operator file, an `AIRVIEW_*` variable or `-set`: shown with a padlock and "Set by …", not changeable in the web interface. |
| **Maidenhead locator** | The radio-amateur grid notation of a position (e.g. `JO22ki`); also accepted when you enter the station location. |
| **MLAT** | Multilateration: locating an aircraft that does not broadcast its position from the differences in arrival time of the same reply at several synchronised stations. Needs timestamps (*clock class* other than `none`). Position source code 1. |
| **mlat-client** | A third-party program that sends timing data to an MLAT server; an AirView aggregator accepts it on port 31090. |
| **Mode A/C** | The older transponder replies carrying only a 4-digit squawk code (Mode A) or a pressure altitude (Mode C). |
| **Mode S** | The selective transponder protocol on 1090 MHz with a 24-bit *ICAO address*; ADS-B is its extended squitter. |
| **MSL** | Height above mean sea level (*EGM96* geoid). The default altitude reference for a declared antenna position. |
| **NACp** | Navigation accuracy category for position, broadcast in ADS-B; tells how accurate the reported position is. |
| **NTP reference** | An optional time server AirView samples to measure the host clock offset for timing analysis (`ntp`). |
| **Operator file** | The read-only configuration file (`-config`, `AIRVIEW_CONFIG`, default `airview.jsonc` in the data directory) whose settings override `config.json` and are *locked*. |
| **Pending** | A station waiting for the aggregator admin's approval. |
| **Pin** | A station's marker on the public network map: `hidden` (default, no marker), `region` (0.5° area) or `cell01` (0.1° area). The exact position is never public. |
| **ppm** | The frequency error of an RTL-SDR's oscillator in parts per million (`sdr.ppm`). |
| **Radarcape** | A receiver with a GPS-disciplined clock; Beast output on port 10003. AirView's default feed. |
| **Range** | The distance of the farthest aircraft currently tracked (top bar), and the range statistics per bearing and altitude band. |
| **Remote-data, remote-web** | Desktop modes showing another AirView: fed from its *hub port*, or showing its website. |
| **Reverse-proxy** | A web server (Caddy, nginx) in front of an aggregator that terminates HTTPS; listed in `aggregator.trustedProxies`. |
| **rtl_tcp** | The third-party program that serves an RTL-SDR dongle's samples over TCP (port 1234); AirView's feed type `rtltcp` demodulates them itself. AirView never ships or downloads rtl_tcp. |
| **SBS (BaseStation)** | A text format with decoded aircraft data (port 30003). Map only: it carries no usable timestamps. |
| **Self-update** | Installing a newer release from a signed manifest (`update`, `airview update`). |
| **Server pin** | The SHA-256 of an aggregator's TLS public key (`sha256/…`), used by feeders instead of a certificate authority. |
| **Share link** | A secret link an owner creates to show a read-only view of their station(s). |
| **Spool** | A feeder's on-disk buffer used while the aggregator is unreachable (`spoolMb`). |
| **Squawk** | The 4-digit Mode A code set by the pilot (e.g. 7000; 7500/7600/7700 are emergencies). |
| **SSR self-location** | Experimental: estimating the station's position from the timing of replies to known radar interrogations (`ssr`, off by default). Its result is shown to the owner only and never sets a station position. |
| **Standalone** | AirView in its default mode: one station with its own receivers, map and statistics. |
| **Station** | One receiver with one timestamp clock, as seen by an aggregator. A feeder with two receivers is two stations. |
| **Survey (reverse MLAT)** | The aggregator measuring a station's position from aircraft with known positions. Used for MLAT without the owner's confirmation when its horizontal uncertainty σh is at most `aggregator.mlat.autoUseMaxSigmaM` (default 50 m) and it passes the *suspect* checks (`aggregator.mlat.autoUseSurvey`); the result is shown to the owner and admins only. |
| **Suspect** | A station whose declared position is more than max(100 m, 3σ) from the survey result. |
| **Timestamps** | The receiver's arrival time of every frame; needed for MLAT. A feeder can turn them off (`timestamps: false`). |
| **Tile cache** | AirView's disk cache of map tiles (`http.tileCache`, `/tilecache/`), 512 MB by default. |
| **Trail** | The track drawn behind an aircraft (`ui.trailMinutes`) and the stored position history. |
| **Uplink** | A connection from a station or feeder to an aggregator (`uplinks[]`). |
| **UUID (feeder)** | A station's identifier at an aggregator. Not a password, but never public: it is shown to the owner, and Feed Check shows only its last 6 characters. |
