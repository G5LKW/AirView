# Legitimate interests assessment (template) and DPIA screening

**How to use this template.** Complete it before you admit the first feeder, sign and date it, and keep it with your records. It is not published, but you must be able to show it to the ICO or to a feeder owner who objects. Replace `[[...]]`; the pre-filled answers describe what AirView does with its default settings (DESIGN §8, §19). If you change a privacy-relevant setting, revisit the balancing test. This template follows the structure of the ICO's legitimate interests guidance (purpose, necessity, balancing) and is not legal advice.

| | |
|---|---|
| Controller | [[NETWORK NAME / organisation]] |
| Completed by | [[name, role]] |
| Date | [[date]] |
| Next review | [[date + 12 months, or earlier on a change of settings]] |
| Related | [[link to published privacy notice]], [RETENTION.md](RETENTION.md), [CONSENT.md](CONSENT.md) |

---

## Part A. Scope: what this assessment covers

Processing based on **legitimate interests** (UK GDPR Art. 6(1)(f)):

1. **Network operation.** Receiving feeders' Mode S/ADS-B data with receiver timestamps, merging it into one network picture, keeping per-station statistics.
2. **Multilateration (MLAT).** Using every timestamping station's **exact** position and clock data to compute positions of aircraft that do not broadcast them, and returning results to contributing stations.
3. **Self-location and position checking.** Calculating a station's position from its timing (reverse MLAT) or its reception pattern; checking declared positions every 6 hours; automatically using a calculated position for MLAT when σh ≤ 50 m and the checks pass (`aggregator.mlat.autoUseSurvey`, DESIGN §19.3).
4. **Security and integrity.** Connection log with day-salted IP hashes, rate limits, audit log of administrator actions, clone detection.
5. **Accounts.** Owner accounts used to claim and manage stations. *(If your network has membership terms, you may rely on contract, Art. 6(1)(b), for this item instead; then delete it here.)*

**Not covered here** (other bases): public display of pins, names and statistics (consent, Art. 6(1)(a), [CONSENT.md](CONSENT.md) C2-C4); responding to rights requests (legal obligation, Art. 6(1)(c)).

---

## Part B. Purpose test: is there a legitimate interest?

| Question | Answer |
|---|---|
| Why do we want to process the data? | To run a community receiver network that shows a more complete and accurate picture of air traffic than any single receiver, including aircraft without ADS-B, which requires MLAT. |
| Who benefits? | Feeder owners (merged map, MLAT results for their own map, statistics, help locating and checking their receiver), the club or community, and the public (public map). |
| Are there wider public benefits? | Aviation interest and education; independent observation of air traffic; research into receiver timing and coverage. [[add yours]] |
| How important are the benefits? | MLAT is the main reason people join an MLAT network; without exact receiver positions and timestamps it cannot work at all. |
| What would happen if we could not process? | No MLAT, no network-wide accuracy checks, no protection against wrong declared positions (which degrade everyone's results). |
| Do we comply with other relevant laws? | Data protection: this assessment, the notice, retention schedule. Wireless Telegraphy Act 2006 s.48: we process only signals aircraft transmit for reception by others and do not disclose message content beyond what the public aviation-tracking community customarily does. [[confirm for your network]] |
| Any ethical issues? | Precise home locations are sensitive; the design keeps them from everyone except the operator (Part D). |

**Conclusion:** [[There is a legitimate interest. / Explain if not.]]

---

## Part C. Necessity test: is the processing necessary?

| Question | Answer |
|---|---|
| Does the processing achieve the purpose? | Yes. MLAT solves for aircraft position from time differences between known receiver positions; position errors translate directly into MLAT errors. |
| Is it proportionate? | Exact positions are used only inside the MLAT engine and shown only to the owner and administrators. The public sees no receiver locations, receiver lists or coverage products (DESIGN §8.2). |
| Could we achieve the purpose without it, or with less? | **Fuzzed positions:** no; a 0.5° error ruins MLAT. **Owner-declared positions only:** they are often wrong by hundreds of metres (map misreads, MSL/ellipsoid confusion, deliberate offsets), which makes position checking necessary. **Not locating stations that send timestamps:** not possible; anyone running MLAT can solve for a receiver's position from its timing, so the honest course is to say so (C1) and offer *No timestamps*. |
| Is the IP address necessary? | Only while connected (to deliver data, enforce per-IP limits and match the Feed Check page). It is never written to disk; the log keeps a day-salted hash for 30 days to investigate abuse. |
| Is the retention necessary? | See [RETENTION.md](RETENTION.md): minute statistics 30 days (diagnosing feeder problems), hourly 13 months (seasonal comparison), audit 24 months (accountability for admin access). |
| Automatic use of measured positions (§19.3) | Necessary for stations that give no or wrong positions to contribute; limited to precise results (≤ 50 m) that pass consistency checks; the owner is told and can override. *If you consider this not necessary, set `aggregator.mlat.autoUseSurvey` to false; measured positions are then used only after the owner confirms.* |

**Conclusion:** [[The processing is necessary and there is no less intrusive way to achieve the purpose. / Explain.]]

---

## Part D. Balancing test: do the individuals' interests override ours?

### D1. Nature of the data

| Question | Answer |
|---|---|
| Is it special category or criminal offence data? | No. |
| Is it data people consider particularly private? | Yes: the exact location of a receiver usually is a home address. Combined with a callsign or name it identifies a person. |
| Children or vulnerable people? | Not targeted; under-13s are asked to use a parent's account. |
| Is it about people in a personal or professional capacity? | Personal (hobby). |

### D2. Reasonable expectations

| Question | Answer |
|---|---|
| Relationship with the individuals | They chose to join the network and install a feeder (or point readsb/mlat-client at us). |
| Would they expect this processing? | Yes for merging and MLAT: it is the reason they joined, and all comparable networks use exact positions internally. Possibly **not** for being located from timing when they entered no position, which is why C1 is shown on the installer and claim page **before** data is sent, and why C6 is shown when a measured position is used. |
| What did we tell them? | Privacy notice §3-§9; consent/notice texts C1, C5, C6 at the point of collection. |
| Is anything innovative or unexpected? | Reverse-MLAT self-location and automatic use of its result. Mitigated by prominent notice and owner override. |

### D3. Likely impact

| Risk | Likelihood / severity (before safeguards) | Safeguards in AirView | Residual |
|---|---|---|---|
| Public or another feeder learns a home location | possible / high | Pins hidden by default; opt-in pins are a fixed random point in a 0.5° or 0.1° cell, computed once (cannot be averaged out) and coarsened when fewer than 3 stations share the 1° cell; public DTOs have no fields for coordinates, distances, bearings, receiver lists or IPs; golden tests crawl every public route for fixture coordinates, IPs and IDs; peer information uses weekly unlinkable tags and distance bands; coverage, range, polar and CRLB maps are never public | low |
| Location inferred from low-altitude "coverage islands" on the public map | possible / medium | Low-altitude public coverage suppressed where fewer than 3 stations contribute; optional island guard; documented in the notice | low-medium (documented) |
| Operator misuse (browsing exact locations) | unlikely / high | Mandatory two-factor admin login; every exact-location view audited (owners see entries about them in their export); audit kept 24 months | low |
| Breach of the server or a backup | unlikely / high | Raw IPs never stored; hashed tokens and claim codes; slow password hashes; encrypted backups rotated at 30 days; TLS 1.3 for native feeders | low-medium |
| Interception on the network path | possible for compat feeders / medium | Native feeders encrypted; compat feeders warned (C5) | medium for compat feeders (their choice) |
| Wrong automated "suspect" decision | possible / low | Affects MLAT use only; owner notified and can correct; admin review | low |
| Retained data outlives its purpose | possible / low | Automatic retention jobs; offline unclaimed stations purged after 90 days; immediate owner deletion | low |

### D4. Can individuals control it?

- **Opt out of locating:** *No timestamps* at install or later ([CONSENT.md](CONSENT.md) C1).
- **Object:** at any time, by switching off timestamps or asking us; we stop MLAT use of that station.
- **Public display:** opt-in only, withdrawable at once.
- **Export and delete:** self-service, immediate.

### D5. Conclusion

[[Having weighed the network's interest against the impact on feeder owners, with the safeguards above, we conclude that our legitimate interests are not overridden by the interests or rights of feeder owners. / Explain if not and what changes.]]

Signed: [[name]] Date: [[date]]

---

## Part E. DPIA screening

A data protection impact assessment is required when processing is likely to result in a high risk. The ICO's list of processing that requires a DPIA includes **tracking** an individual's geolocation and **invisible processing**; locating a receiver from its timing without the owner entering a position comes close to both, and a network may grow large. Answer the screening questions:

| Screening question | Answer |
|---|---|
| Large-scale processing of location data? | [[Yes if you expect hundreds of feeders or more (§20 targets 1,000+).]] |
| Systematic monitoring of individuals? | No: the station is monitored, not the person. But its location is personal data. |
| Combining or matching datasets? | Declared position, GNSS, survey and reception-pattern estimates are combined for each station. |
| Data obtained without the individual's direct involvement (invisible processing)? | Survey positions are calculated, not provided. Mitigated by C1 before collection and C6 when used. |
| Innovative technology? | Reverse MLAT for self-location. |
| Could it prevent people from using a service? | No. |

**Recommendation:** treat a public network, or any network aiming at more than [[100]] feeders, as **requiring a DPIA**. Parts B-D of this document, together with [RETENTION.md](RETENTION.md) and the privacy notice, cover most of its content: add a description of data flows (DESIGN §2), the consultation you did with members (e.g. a club meeting or forum post on the privacy settings), and sign-off. For a small private club, record the screening result here.

Screening result: [[DPIA required / not required, because ...]] Date: [[date]]
