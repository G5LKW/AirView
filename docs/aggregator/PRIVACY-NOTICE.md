# Privacy notice template (UK GDPR)

**How to use this template.** This is a template for the operator of an AirView aggregator. Replace everything in `[[double brackets]]`, delete the guidance paragraphs in *italics* (they start with "Operator:"), and publish the result on your site, for example at `/privacy`, linked from the feeder page, the claim page, the registration form and the site footer. Date it. It is written for the UK GDPR and the Data Protection Act 2018 as amended by the Data (Use and Access) Act 2025; EU feeders are covered by the same notice, and no EU representative is assumed (DESIGN §16). This template is not legal advice.

*Operator: the numbers in sections 5 and 6 are AirView's defaults. If you changed any setting listed in [RETENTION.md](RETENTION.md) or the visibility options in [OPERATOR-GUIDE.md](OPERATOR-GUIDE.md) §2.1, change this notice to match. Keep the wording of the quoted text in section 4 exactly as it is: the installers show the same sentence ([CONSENT.md](CONSENT.md) C1).*

---

# Privacy notice: [[NETWORK NAME]]

Last updated: [[DATE]]. Version [[n]].

## 1. Who we are

[[NETWORK NAME]] is an ADS-B and Mode S receiver network run by [[CONTROLLER: club / association / individual name]], [[postal address]]. We are the **controller** of the personal data described here.

Contact for anything about your data: [[contact e-mail or contact form URL]]. [[If you appointed one: Our data protection lead is NAME.]]

[[ICO registration number ZA123456 / We are exempt from the ICO data protection fee because REASON.]]

*Operator: a data protection officer is not normally required for a club network, but you must give a working contact. Use the ICO's fee self-assessment; the not-for-profit exemption is narrow.*

## 2. What this notice covers

It covers people who:
- **feed** the network with a receiver ("feeder owners"), whether they have an account or not;
- **have an account** on this site;
- **visit** this site and its public map.

It does not cover the aircraft data itself in detail (section 10).

## 3. What we collect

| Category | What exactly | From |
|---|---|---|
| **Station identity** | the feeder's public identity key and the ID derived from it; the feeder's random UUID; for readsb/mlat-client feeders their UUID; station name; receiver type, software name and version, operating system and architecture | your feeder |
| **Station location** | the position and antenna height you enter; the receiver's own GNSS position if you chose to share it; positions we **calculate** from your receiver's timing (reverse multilateration) or from its reception pattern; the uncertainty of each | your feeder, and our calculations |
| **Reception data** | the aircraft messages your receiver hears, with signal strength and precise receiver timestamps; per-minute counters your feeder computes | your feeder |
| **Timing and clock data** | the state of your receiver's clock as seen by the network (offset, drift, quality) | our calculations |
| **Statistics about your station** | message rates, range, coverage, signal and elevation statistics, records, uptime, MLAT contribution | our calculations from the reception data |
| **Connection data** | your IP address while connected (held in memory only); a connection log of connect and disconnect times, protocol and bytes, with a hash of the IP that cannot be linked across days | your connection |
| **Account data** | user name; one-way password hash; authenticator secret and hashed recovery codes if you enable two-factor sign-in; account creation and last sign-in time; which stations you own | you |
| **Session data** | a session cookie and an anti-forgery token; session start, last use and expiry (no IP address) | your browser |
| **Choices and audit** | your public-display choices and when you made them; a log of administrator actions concerning your station, including every time an administrator viewed its exact location | you, our administrators |
| **Website visits** | [[If your reverse proxy keeps access logs: IP address, time, page requested, browser type, kept for 14 days.]] | your browser |

We do **not** ask for your e-mail address, real name, phone number or payment details. We do not collect special category data.

## 4. Your receiver's location: please read

> "If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this."

Multilateration only works with precise receiver positions, so:
- we use the **exact** position of your receiver (never a rounded one) inside our MLAT engine;
- we check the position you enter against what we measure from your receiver's timing every [[6]] hours. If they differ by more than 100 m (or three times the measurement's uncertainty), we mark the station "suspect", stop using it for MLAT and tell you;
- if we measure your position to 50 m or better and it passes these checks, we use the measured position for MLAT **automatically**, even if you entered none or a different one. We tell you on your station page, and you can correct or override it. *(Operator: delete this bullet if you set `aggregator.mlat.autoUseSurvey` to false, and say instead that a measured position is used only after you confirm it.)*

The calculated position is shown only to you and to our administrators.

## 5. Who can see what

| Data | The public | Other feeder owners | You (your own stations) | Our administrators |
|---|---|---|---|---|
| Live map of aircraft, trails, MLAT positions with their accuracy grade | yes | yes | yes | yes |
| Which stations heard an aircraft | no (not even how many) | no | whether **your** stations heard it | yes |
| Your station's map pin | only if you opt in, and then only a point placed at random in a 0.5° or 0.1° area (never your position) | the same as the public | exact | exact |
| Station name | only if you opt in | only if you opt in | yes | yes |
| Station statistics | only if you opt in, and only figures that do not reveal your location (rates, uptime, aircraft per day, MLAT contribution) | the same as the public | all | all |
| Range, coverage, polar plots, heat maps, records, signal statistics, your calculated positions | never | never | yes | yes |
| Timing and clock data | never | an anonymous tag per peer that changes weekly, a distance band and timing quality; no name, position or direction | yes | yes |
| IP address | never | never | your current IP while connected | yes, while connected; each view is logged |

We put a public network coverage map together from all stations; in areas with fewer than 3 contributing stations it leaves out low-altitude coverage. We also publish network-wide MLAT accuracy figures, only for areas with at least 5 contributing stations.

**Results sent back to feeders.** Stations that helped calculate an aircraft's position in the last minute can receive that position. These results never say which other stations took part; only a rough count (3, 4, 5-8, 9 or more).

## 6. How long we keep it

| Data | Kept for |
|---|---|
| IP addresses | memory only, while connected |
| Connection log | 30 days |
| Per-minute statistics of your station | 30 days |
| Hourly statistics, range and coverage of your station | 13 months |
| Calculated positions of your station | the latest 5 of each method |
| A station that has no owner and has been offline | deleted after 90 days |
| Your account and claimed stations | until you delete them; deletion is immediate |
| Backups | up to [[30]] days after deletion |
| Audit log | 24 months (pseudonymised at once if you delete your account or station) |
| Merged aircraft positions and trails | 30 days |
| Flights (including which stations heard them) | 2 years |
| [[Web server access logs]] | [[14 days]] |

The full schedule is at [[link to your published retention schedule, from RETENTION.md]].

## 7. Who we share it with

- **Nobody buys or receives your data from us.** We do not use advertising or analytics trackers.
- **Hosting:** our server is run by [[HOSTING PROVIDER]] in [[COUNTRY]], acting as our processor under its data processing terms. *(Operator: if the country is outside the UK and not covered by UK adequacy regulations, name the safeguard here, e.g. the UK International Data Transfer Addendum.)*
- **The public and other feeders** see only what section 5 lists.
- **Weather and terrain services:** when you ask for weather or terrain analysis for your own station, your station's approximate position is sent to [[Open-Meteo / the elevation tile provider]] to fetch the data. *(Operator: delete this bullet if you have not enabled these features for network stations.)*
- **[[Other networks]]:** [[If you federate with another aggregator: what is forwarded, under which agreement. Otherwise delete.]]
- **Authorities:** only where the law requires us to.

## 8. Why we use your data, and our lawful basis

| Purpose | Lawful basis |
|---|---|
| Merging station data into the network picture, multilateration, measuring and checking station positions, clock synchronisation, returning results to contributing stations | **Legitimate interests** (Art. 6(1)(f)): running an accurate shared receiver network that its members join for this purpose. Our assessment is available on request. |
| Your account, claiming and managing your stations, exporting and deleting your data | **Legitimate interests** (Art. 6(1)(f)) [[or, for members of a club with membership terms: **contract** (Art. 6(1)(b))]] |
| Showing your pin, name or statistics publicly | **Consent** (Art. 6(1)(a)). Off unless you switch it on; you can switch it off at any time, with immediate effect. |
| Security: rate limits, connection log, audit log, abuse prevention | **Legitimate interests** (Art. 6(1)(f)) in keeping the service and your data secure |
| Answering requests, keeping records the law requires | **Legal obligation** (Art. 6(1)(c)) |

**Automated decisions.** Our software decides automatically whether a station's position looks wrong ("suspect") and whether a measured position is precise enough to use. These decisions only affect whether your station is used for MLAT; they have no legal or similarly significant effect on you. You can always override them on your station page or ask us to review them.

## 9. Things we cannot fully protect against

We want you to decide with the full picture:
- **We, the operator, can always locate a station that sends timestamps** (section 4).
- **Coverage islands.** An aircraft flying low that only your station hears will appear on the public map, and its position hints at the area your station covers. [[We do / We do not]] coarsen such aircraft on the public map. *(Operator: "do" if `aggregator.islandGuard` is on.)*
- **readsb/mlat-client connections are not encrypted.** If you feed through ports 30004 or 31090, your timing data, UUID and entered position cross the internet in clear text. Our own AirView feeder encrypts everything.
- **Merged data cannot be un-merged.** Aircraft positions your station contributed to the shared picture are not removed when you delete your station; only the link to your station is.
- **Names.** If you publish a station name that is your callsign or real name, someone who also sees your Region pin may identify you.

## 10. Aircraft data

The network receives what aircraft broadcast: their ICAO address, callsign, position, altitude and speed. For privately owned aircraft this can be personal data of the owner or pilot. [[We show / We hide]] aircraft that are on the [[PIA / LADD]] lists. To ask us about an aircraft, contact us (section 1).

## 11. Your rights

You have the right to:
- **access** your data: use **Export** on your account page (a ZIP of JSON files), or ask us;
- **portability**: the export is machine-readable JSON;
- **rectification**: change your station name and position yourself, or ask us;
- **erasure**: use **Delete account** (with or without your stations), or ask us; it is immediate, backups follow within [[30]] days;
- **restriction** and **objection**: in particular you can object to our use of your receiver for MLAT. The simplest way is to switch your feeder to **No timestamps**; we will also act on a request;
- **withdraw consent** for public display at any time, on your station page; it does not affect what was shown before;
- **complain**: please tell us first at [[contact]]; we will acknowledge your complaint within [[30]] days and reply without undue delay. You can also complain to the Information Commissioner's Office: ico.org.uk/make-a-complaint, telephone 0303 123 1113.

We answer requests within one month. If you have no account, we may ask you to prove the station is yours, for example by connecting from the feeder's internet connection or with a claim code.

## 12. Cookies and local storage

This site sets one session cookie when you sign in and an anti-forgery token for your session. They are strictly necessary for signing in. The map stores your display preferences (units, layers) in your browser's local storage; they never leave your browser. We use no tracking or analytics cookies. [[If your map tiles load directly from a third-party tile server in the visitor's browser, name it here: that server sees the visitor's IP address.]]

## 13. Children

The network is not intended for children under 13. If you are under 13, ask a parent or guardian to run the feeder and the account.

## 14. Security

Feeder connections to our own feeder port use TLS 1.3 with a key unique to each feeder. Passwords are stored as slow one-way hashes; administrators must use two-factor sign-in; every administrator view of an exact station location is logged; backups are encrypted. *(Operator: keep this sentence true: encrypted backups, OPERATOR-GUIDE.md §13.3.)* No system is perfectly secure; if a breach is likely to put you at risk, we will tell you.

## 15. Changes

We will post any change here with a new date and, for changes that affect how your receiver's location is used, tell feeder owners on their station page before the change takes effect.
