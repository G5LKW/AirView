# internal/alerts — wiring notes for the integration engineer

Rules engine for aircraft and system alerts (VirtualRadar / tar1090 style, plus AND/OR/NOT trees,
geofences, watch-lists, system conditions, webhooks, map highlights). Imports only `model`,
`geo`, `store`. Owns the tables `alerts_rule` and `alerts_watch` (migration owner `"alerts"`)
and the kv key `alerts.seeded`. The "log" action writes `store.Event{Kind:"alert", ICAO, Detail: <Alert JSON>}`
through the store's batched writer (no direct DB writes on the hot path).

## 1. Construction & lifecycle (engine)

```go
al := alerts.New(alerts.Options{Store: st})   // other options have sane defaults
if err := al.Load(ctx); err != nil { … }      // reads rules + watch-lists; seeds defaults on first run
go al.Run(ctx)                                 // webhook sender; stops when ctx is cancelled
```

Without `Load` (or without a store) the built-in defaults run in memory. Built-in defaults:
**enabled** – Emergency (7500/7600/7700 or ADS-B emergency state, critical, sound, red highlight),
Military aircraft (amber highlight), New all-time record (system). **Disabled templates** – Feed
down/up, NTP not synchronised, Range collapse/restored, New airframe, Watch-list aircraft,
Low overhead (<3000 ft within 5 km), Unusual category (B1–B7).

## 2. Hooks (how often, with what)

| Hook | Caller / frequency | Data |
|---|---|---|
| `al.Evaluate(aircraft []model.Aircraft, now time.Time)` | tracker/engine, **1 Hz**, right after building the `LiveSnapshot` | the same `[]model.Aircraft` slice (read only, not retained). Needs `DistKm`/`Bearing` (receiver geometry), `Military`, `CountryCode`, `Operator`, `TypeCode`, `Registration` filled from refdata/acdb for those conditions to work. Cost ≈ 7 µs for 300 aircraft with the default rules on a desktop CPU; **0 allocations** in steady state (tested with `testing.AllocsPerRun`). |
| `al.NoteNewAirframe(icao uint32, now)` | tracker, when it inserts a **new row into the `aircraft` table** (first time ever seen) | enables the `newAirframe` condition for 15 min |
| `al.HandleSystem(alerts.SystemEvent{Kind: alerts.SysRecord, Key: "<record key>", Text: "...", Value: &km, Aircraft: &ac})` | analytics/tracker, whenever a new **all-time record** is stored (`records` table) | `Key` = record key (e.g. `farthest_km`), `Value` = new value, optional `Aircraft` = the record holder (rules may combine `system` with aircraft conditions) |
| `al.HandleSystem(alerts.SystemEvent{Kind: alerts.SysCustom, Key, Text})` | anything else worth alerting on (optional) | |
| `al.CheckStatus(status model.Status, now)` | engine, **every 5–10 s** with the same `model.Status` sent as SSE `status` | derives `feed_down` (enabled feed disconnected > 60 s), `feed_up`, `ntp_unsync`/`ntp_sync` (NTP enabled and `Synced` flips), `range_collapse` (15-min max range < 35 % of the 24 h median hourly max, with ≥ 6 h history, ≥ 10 recent minutes, ≥ 3 positioned aircraft) / `range_restored` (≥ 60 %). Needs `Counts.MaxRangeKm` and `Counts.WithPos`. |
| `ch, cancel := al.Subscribe(64)` | server SSE hub, once | forward each `alerts.Alert` as SSE `event: alert` (JSON). Slow subscribers lose alerts (never block). Also emit a `notice` (`{level: severity, text}`) if you prefer the existing notice channel. |
| numeric fields | every `alerts.Alert` | besides the formatted `text` (default template: fixed ft / km): `altFt`, `distKm`, `gsKt`, `vrFpm`, `trackDeg`, `brgDeg`, `rssi` (aircraft alerts), and for system events `value`, `refValue` (range collapse: the typical range) and `unit` (`km`, `ft`, `kt`, `dBFS`, `msg/s`, `s`, `""` = count; a record's unit comes from its key suffix, `RecordUnit`). `auto` = the text came from the default template, so a client may rebuild it in its own units. `feed_down` carries `value` = seconds down. |
| `al.Highlights(now)` | server, include in the 1 Hz SSE `aircraft` payload or serve via endpoint | `[]alerts.Highlight{icao,color,ruleId,severity,until}` |

## 3. HTTP endpoints (server)

JSON; errors: `errors.Is(err, alerts.ErrInvalid)` → 400 (message is user-readable),
`alerts.ErrNotFound` → 404, `alerts.ErrTooMany` → 409. Mutating endpoints are **admin only**.

| Method & path | Body / params | Call | Response |
|---|---|---|---|
| `GET /api/alerts/rules` | – | `al.Rules()` | `[]alerts.Rule` |
| `GET /api/alerts/rules/{id}` | – | `al.Rule(id)` | `alerts.Rule` / 404 |
| `POST /api/alerts/rules` (admin) | `alerts.Rule` JSON (no id) — decode with `alerts.ParseRule(body)` (size-limited, unknown fields rejected) | `al.PutRule(ctx, r)` | stored `alerts.Rule` (id `r-…`) |
| `PUT /api/alerts/rules/{id}` (admin) | `alerts.Rule`; set `r.ID = id` | `al.PutRule` | stored rule |
| `DELETE /api/alerts/rules/{id}` (admin) | – | `al.DeleteRule` | `{"ok":true}` |
| `POST /api/alerts/rules/{id}/enabled` (admin) | `{"enabled":bool}` | `al.SetEnabled` | rule |
| `POST /api/alerts/rules/reset-defaults` (admin) | – | `al.ResetDefaults` | `{"ok":true}` |
| `POST /api/alerts/test` | `{"rule": Rule, "event": SystemEvent?}` | `al.TestRule(rule, snapshot.Aircraft, ev, now)` with the **current live picture** | `[]alerts.TestMatch{icao,callsign,reg,type,text}` (≤ 500) |
| `GET /api/alerts/export` | – | `al.ExportRules()` | JSON array (download) |
| `POST /api/alerts/import` (admin) | JSON array (≤ 4 MB) | `al.ImportRules(ctx, body)` | `{"imported":n}`; all-or-nothing validation |
| `GET /api/alerts/recent` | `limit` (default 100), `after` (alert id) | `al.Recent(limit, after)` | `[]alerts.Alert`, newest first (in-memory ring of 500; older ones: `GET /api/events?kind=alert` from the store) |
| `GET /api/alerts/highlights` | – | `al.Highlights(time.Now())` | `[]alerts.Highlight` |
| `GET /api/alerts/stats` | – | `al.Stats()` | `alerts.Stats` (counters, eval time, webhook sent/failed/dropped, range baseline) |
| `POST /api/alerts/webhook-test` (admin) | `{"url":"https://…"}` | `al.TestWebhook(ctx, url)` | `{"ok":true}` or 502 with the error |
| `GET /api/alerts/placeholders` | – | `alerts.PlaceholderHelp()` | string |
| `GET /api/alerts/watchlists` | – | `al.Watchlists()` | `[]alerts.WatchlistInfo{name,entries}` |
| `GET /api/alerts/watchlists/{name}` | `format=json\|txt\|csv` | `al.Watchlist(name)` / `al.ExportWatchlist(w, name, format)` | `[]alerts.WatchEntry{icao,note,added}` or file download |
| `POST /api/alerts/watchlists/{name}/import` (admin) | raw text/CSV body (≤ 16 MB), `replace=1` | `al.ImportWatchlist(ctx, name, r.Body, replace)` | `{"imported":n,"rejected":k}` |
| `PUT /api/alerts/watchlists/{name}/{icao}` (admin) | `{"note":"…"}` | `al.AddWatch(ctx, name, icao, note)` | `{"ok":true}` |
| `DELETE /api/alerts/watchlists/{name}/{icao}` (admin) | – | `al.RemoveWatch` | `{"ok":true}` |
| `DELETE /api/alerts/watchlists/{name}` (admin) | – | `al.DeleteWatchlist` | `{"ok":true}` |

Also useful: in `GET /api/aircraft/{icao}` add `al.WatchNote(icao)` → `{list, note}` when on a list.

## 4. Rule JSON (what the UI edits)

```json
{"id":"r-1a2b3c4d5e6f","name":"Heavies over the house","enabled":true,"severity":"info",
 "match":{"op":"and","conds":[
   {"type":"category","values":["A5"]},
   {"type":"geofence","lat":50.1,"lon":8.6,"radiusKm":15},
   {"type":"altitude","max":10000},
   {"type":"callsign","values":["DLH*","CFG*"],"negate":true}]},
 "cooldownSec":900,"ruleCooldownSec":0,
 "message":"{ident} ({type}) at {alt} ft, {dist} km",
 "actions":{"notify":true,"sound":false,"log":true,"webhook":"https://example.com/hook","highlight":"#0a84ff"}}
```

Condition types (`type`): `icao` (values: `3C6444`, `3C*`, `3C0000-3CFFFF`), `reg`, `callsign`,
`operator`, `actype`, `category`, `country` (ISO2 or name), `squawk` (globs `*` `?`, case-insensitive),
`military` / `onGround` / `newAirframe` (`value`, default true), `emergency` (values
`general,medical,minfuel,nordo,unlawful,downed,any`; empty = any), `altitude` (ft), `speed` (kt),
`vrate` (ft/min), `distance` (km), `adsbVersion`, `nic`, `rssi` (dBFS) — all `min`/`max`;
`bearing` (`from`,`to` clockwise sector, wraps through north), `geofence` (circle `lat,lon,radiusKm`
or `polygon` [[lon,lat]…] in GeoJSON order, antimeridian-safe; `inside` default true), `source`
(`adsb,mlat,tisb,adsr,modes,other`), `watchlist` (list names; empty = any list), `system` (event
kinds `record, feed_down, feed_up, ntp_unsync, ntp_sync, range_collapse, range_restored, custom,
new_airframe`, optionally `kind:KEY*` globs). Groups: `{"op":"and"|"or","conds":[…]}`; any node
may carry `"negate":true`. A rule containing a `system` leaf is evaluated only on system events.
Predicted positions: a rule whose tree contains a position-dependent leaf (`altitude`, `distance`,
`geofence`, `bearing`) is not evaluated for an aircraft shown at a predicted (coasting) position
(`model.Aircraft.predicted`), so it cannot fire on an extrapolated track. Set the rule boolean
`"includePredicted":true` to opt in. Rules without such leaves are unaffected.
Unknown values (no registration, no altitude…) never match a positive condition.

Semantics: an **episode** is a run of consecutive matches of one rule for one aircraft (a gap
> 10 s ends it). An alert fires at most once per episode and only if `cooldownSec` (per aircraft,
per system event key) has passed since that rule last fired for it; `ruleCooldownSec` limits the
rule as a whole; a global guard allows ≤ 120 alerts/min. Highlights stay on while the rule matches
plus 15 s (highest severity wins).

Webhook: `POST` JSON `{"app":"AirView","version":"…","type":"alert"|"test","alert":{…Alert…}}`,
`Content-Type: application/json`, `User-Agent: AirView/<version>`, 5 s timeout, **no retries, no
redirects**, ≤ 30 posts/min globally, queue of 64 (overflow dropped and counted). Errors are logged
with the URL redacted to scheme+host.

## 5. UI

- New page or Settings section **Alerts**: rule list (enable toggles, severity chip, built-in badge,
  last fired from `/api/alerts/recent`), rule editor with a condition-tree builder (AND/OR groups,
  NOT toggle, per-type inputs; geofence drawn on a small map: circle or polygon; bearing sector
  widget), actions (toast, sound, log, webhook URL + "test" button, highlight colour picker),
  cooldowns, message template with the placeholder list, and a **"Test against current traffic"**
  button showing `/api/alerts/test` matches. Import/export buttons for rules (JSON) and
  watch-lists (txt/CSV), watch-list editor (hex + note).
- Global: on SSE `alert` show a toast coloured by severity (`critical` red, `warning` amber,
  `info` blue); play a sound when `sound` is true (respect a user mute); clicking selects
  `icao` on the map. Alert history drawer from `/api/alerts/recent`.
- Map: draw a halo/outline in `color` around aircraft listed in the highlights.
- Status/health: show `/api/alerts/stats` (eval µs, webhook failures/last error) in diagnostics.
