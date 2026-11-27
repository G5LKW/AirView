package main

// The consent texts the feeder setup shows (docs/aggregator/CONSENT.md, word
// for word; TestFeederConsentTextsMatchDoc compares them). C1 is consentText
// in setup.go. C2-C4 ask for the opt-in public display, so the interactive
// setup shows each one before its question; the command-line flags
// (-privacy, -show-name, -public-stats) are the non-interactive equivalent.

// consentPin is C2 (public pin).
const consentPin = `Show my station on the public map?
  - Hidden (default): no pin.
  - Region: a point placed at random inside a 0.5-degree area around your station, about 55 by 35 km in the UK.
  - Area: the same inside a 0.1-degree area, about 11 by 7 km. Where fewer than 3 stations are nearby it is shown as Region.
The point is chosen once and stays put, so it cannot be averaged out. Your exact position is never public. You can change this at any time.`

// consentName is C3 (public name).
const consentName = `Show my station name publicly? Others will see the name next to the station's public statistics and, if you chose a pin, next to the pin. Do not use your callsign or real name if you want to stay anonymous: a name together with even a rough area can identify you.`

// consentStats is C4 (public statistics).
const consentStats = `Publish my station's statistics? Only figures that do not reveal where your station is: message rate, uptime, aircraft per day, positions per day and MLAT contribution. Range, coverage, polar plots and records stay private to you.`
