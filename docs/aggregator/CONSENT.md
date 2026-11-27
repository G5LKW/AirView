# Consent and notice texts

These are the texts AirView shows feeder owners at the moments that matter: when they install a feeder, when they choose what is public, when they claim a station and when the network locates their receiver. The installers, the claim page and the station page must use them **word for word** (the operator may translate them, and may add the network name and the privacy-notice link). They are kept here so the privacy notice, the UI and the installers cannot drift apart.

**Two different things are going on, and the texts keep them apart** (see [LIA.md](LIA.md)):

| Processing | Lawful basis (UK GDPR Art. 6(1)) | What the text does |
|---|---|---|
| Using the station's data, timestamps and exact position to run the network and MLAT | (f) legitimate interests | **Informs.** It is not a consent request: the owner's choice is whether to send timestamps at all. |
| Showing a pin, the station name or statistics to the public | (a) consent | **Asks.** Off by default, opt-in, withdrawable at any time with immediate effect. |

Each text has an ID. Code and installers refer to the ID; tests check that the canonical wording below matches the design (DESIGN §10.5) and the privacy notice.

---

## C1. Timestamps and location (installers, claim page, privacy notice)

Shown: on the installer's position screen and before the "leave blank" choice; on the Windows installer's privacy page; on the claim page; in the privacy notice §4; in the feeder guide §1.

> "If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this."

Controls next to it: **Send timestamps (MLAT)** (default on) / **No timestamps (no MLAT)**. Command line: `--no-timestamps`. Configuration: `uplinks[].timestamps`.

## C2. Public pin (opt-in)

Shown with the pin control on the installer and the station's *Privacy & data* page.

> **Show my station on the public map?**
> - **Hidden** (default): no pin.
> - **Region**: a point placed at random inside a 0.5-degree area around your station, about 55 by 35 km in the UK.
> - **Area**: the same inside a 0.1-degree area, about 11 by 7 km. Where fewer than 3 stations are nearby it is shown as Region.
>
> The point is chosen once and stays put, so it cannot be averaged out. Your exact position is never public. You can change this at any time.

Configuration: `uplinks[].pin` / installer `--privacy hidden|region|cell01` (the UI label *Area* is the value `cell01`).

## C3. Public name (opt-in)

> **Show my station name publicly?** Others will see the name next to the station's public statistics and, if you chose a pin, next to the pin. Do not use your callsign or real name if you want to stay anonymous: a name together with even a rough area can identify you.

Configuration: `uplinks[].showName`.

## C4. Public statistics (opt-in)

> **Publish my station's statistics?** Only figures that do not reveal where your station is: message rate, uptime, aircraft per day, positions per day and MLAT contribution. Range, coverage, polar plots and records stay private to you.

Configuration: `uplinks[].publicStats`.

## C5. readsb / mlat-client feeders (compat ports)

Shown on the network's `/feeder` page next to the configuration lines, and on the claim page for compat stations.

> These connections are not encrypted: your receiver's timing data, your UUID and, for MLAT, the position you enter cross the internet in clear text. Your UUID identifies your station; keep it private. If your mlat-client does not set privacy mode, the network shows a Region pin and your mlat-client user name, as other networks do. You can change both after you claim the station.

## C6. Automatic use of a surveyed position (§19.3)

Shown on the claim page and, as a notice, on the station page when it happens.

> The network can measure your receiver's position from its timing. If that measurement is precise (50 m or better) and consistent, it is used for MLAT automatically, even if you entered a different position or none. Only you and the network admins can see it. You can correct it or override it on your station page at any time.

Notice when it happens:

> We measured your station's position to about {σ} m and now use it for MLAT. It differs from the position you entered by {Δ} m. [Review location]

## C7. Account creation (notice, not consent)

Under the registration form.

> We store your user name, a one-way hash of your password, your authenticator secret if you turn on two-factor sign-in, and the times you sign in. We ask for no e-mail address, so we cannot reset your password by e-mail: keep your recovery codes. How we use your data: [privacy notice].

## C8. Withdrawal and deletion (station page)

> You can switch off public display at any time; it takes effect immediately. **Export** downloads everything we hold about you. **Delete account** removes your account and, if you choose, your stations, their positions, statistics and logs at once; backups expire within {backup days} days. Aircraft positions already merged into the shared map cannot be separated out again.

## C9. Feeding another aggregator (Beast, "also feed others")

Shown next to the uplink type choice when an owner adds a Beast output (`uplinks[]` with `"type":"beast"`) in the AirView settings or on the feeder status page.

> Beast connections are not encrypted. Your receiver data and this UUID go to that aggregator's operator under their terms. AirView never forwards MLAT results to them.

Configuration: `uplinks[].type`, `uplinks[].uuid`.

---

## Record keeping

- Changes of pin, name and public-statistics settings are written to the audit log with the time and the account (§8.4), which is the record of consent and withdrawal. Owners see these entries in their export.
- For compat feeders, the mlat-client `privacy` flag received at connection is recorded as the source of the initial pin level.
- Keep the version of this file (and of the privacy notice) that was in force when a station was admitted. Date your published notice.

## Changing these texts

If you change C1 or C6, update [PRIVACY-NOTICE.md](PRIVACY-NOTICE.md) and tell existing feeder owners, because they joined on the old wording. The test `TestConsentTextMatchesDesign` fails when C1 no longer matches DESIGN §10.5 or the privacy notice.
