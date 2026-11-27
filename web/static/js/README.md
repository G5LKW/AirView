# AirView front-end: page modules and shared helpers

No bundler, no framework: plain ES modules served from `web/static`. `index.html` loads `js/app.js`, which builds
the shell (sidebar, status bar, banners), owns the hash router and lazily imports one module per page.

```
js/app.js        shell, router, page lifecycle           js/api.js     fetch wrapper (JSON, errors, timeouts)
js/live.js       shared SSE store (+ polling fallback)    js/units.js   unit systems + all number/time formatting
js/ui.js         DOM helpers + component kit              js/charts.js  ECharts wrapper, theme, presets
js/config.js     model.Config store                       js/geo.js     client-side geodesy + sun position
js/icons.js      icon set                                 js/pages/<id>.js   one module per page
js/admin.js      admin login-and-retry helper (401)       js/alertsound.js   app-wide alert chime + mute switch
js/map/*.js      internals of the Live Map page           css/*.css     tokens, base, shell, components, map
js/rf/advice.js  RF / terrain advice sentences in the display units (localise, itemText; uses code + data)
js/timing/ref.js frame reference (start / end of frame) of receiver delays, host-clock findings
js/<page>/*.js   internals of one page (aircraft, alerts, antenna, reports, rf, settings, stats, timing, weather)
js/net/*.js      network (aggregator) mode: model.js rules, store.js state, endpoints.js routes + normalisers,
                 auth.js login dialogs, view.js page helpers, qr.js QR encoder      js/map/network.js  map View selector
```

Dev loop without the Go backend: `node web/dev/mock-server.mjs --port=8099` (flags: `--n=400`, `--unlocated`,
`--demo`, `--no-sse`, `--no-tilecache`, `--aggregator [--setup]` for network mode, see `web/dev/mock-network.mjs`
for the fixture accounts) then open `http://127.0.0.1:8099/`. Unit tests of the browser-independent modules
(net rules, normalisers, QR encoder, replay start): `node --test "web/dev/test/*.test.mjs"`. Public-view privacy
check of a running server: `node web/dev/privacy-crawl.mjs http://host:port [--coords=lat,lon]`.

## 1. Page-module contract

Fixed page ids (route `#/<id>`): `map aircraft reports alerts stats antenna rf weather forecast timing guess settings`
(sidebar order). A page is `js/pages/<id>.js` with a default export:

| id | Title (icon) | Content / API (see `docs/API.md`) |
|---|---|---|
| `reports` | Reports & Replay (`replay`) | VRS-style flight report `/api/reports` (+ `/api/reports.csv`), history replay `/api/replay`, flight trace `/api/history/flights/{id}/trace`, exports `/api/export/flight/{id}.kml\|.geojson\|.csv`, `/api/export/live.kml\|.geojson`, `/api/export/aircraft.csv`. Sub-routes suggested: `#/reports/replay`, `#/reports/flight/<id>`. |
| `alerts` | Alerts (`bell`) | Rule list / editor / test (`/api/alerts/rules`, `/api/alerts/test`), recent alerts (`/api/alerts/recent`), watch-lists (`/api/alerts/watchlists...`), import / export, webhook test, engine stats. |
| `rf` | RF Lab (`wave`) | `/api/rf/summary`, `signal`, `detection`, `intervals`, `channel`, `interrogators`, `timing`, `fleet`, `gnss`, `health`, `history`. |

Prediction, terrain and navigation data have no page of their own: they belong on the Live Map (coasting icons,
predicted path, theoretical coverage / coverage floor layers, airports overlay, route line) and in the aircraft
details (`/api/prediction/intent/{icao}`, `/api/terrain/profile`, `/api/navdata/route/{callsign}`, photo), the
Antenna Lab (terrain horizon / site advice, `/api/terrain/*`) and Settings (`/api/navdata/status` + import,
terrain status / prefetch, config sections `prediction`, `terrain`, `navData`, `alerts`, `rfStat`).

Live picture fields added for these features (in every `aircraft` snapshot entry, all optional): `predicted`
(true = shown at the filter's predicted position because no position arrived for more than 4 s; draw it
distinctly, e.g. dashed/glowing, opacity falling with `coastS`), `coastS`, `predSigmaM` (1-sigma uncertainty
circle), `lastRealLat` / `lastRealLon`, `cpaKm` / `cpaInS` (closest approach to the station), `terrainVisible` /
`losMarginDeg`, `route` (`{from, to, fromIata, toIata, fromName, toName, via, plausible, checked, source}`),
`highlight` (`{ruleId, color, label, severity}` while an alert rule highlights the aircraft; note `alert` stays the
Mode S alert flag). `counts.predicted` counts the coasting aircraft (not included in `counts.withPos`).

Alert notifications: `live.js` forwards the SSE `notice` events, which include `kind: "alert"` notices for user
rules and system alerts (feed down, range collapse, NTP). The full alert objects arrive as SSE `event: alert`,
which `live.js` forwards as `live.on('alert')`. A page that needs every alert (history list) uses
`live.followAlerts({ signal: ctx.signal, initial, onAlerts, onReset, onError })`: live alerts from the stream plus a
`/api/alerts/recent?after=` catch-up on mount, on every reconnect and while the stream is down (Alerts page).

**Toasts and sound are the shell's job, on every page.** `app.js` follows the alerts itself and shows exactly one
toast per alert whose rule has `notify`, with a **Show** action that opens `#/map/<ICAO>`; the built-in emergency /
military / record rules are announced through the tracker's own notice (also toasted once, with Show when the
notice has an `icao`), and the engine's `kind: "alert"` notices are skipped because the stream already carries them.
A batch of alerts with `sound` plays one chime (`alertsound.js`, severity of the loudest) when
`config.alerts.sound` is on, the tab is visible and this browser is not muted. Pages must not toast or beep alerts
themselves. (Legacy: while a page sets `ctx.shared` `alertToasts`, the shell shows no alert toasts and plays no
alert sound.)

```js
export default {
  id: 'stats',
  title: 'Statistics',          // optional: overrides the shell title
  fullBleed: false,             // optional: true = no padding, no page scroll (the map uses it)

  async mount(container, ctx) { // container: empty <div class="page">; may be async; throw = error placeholder
    const { api, live, units, ui, charts, config } = ctx;
    const { h } = ui;
    const tile = ui.stat({ label: 'Max range', icon: 'radar', accent: 'cyan' });
    const chartEl = h('div.chart');
    container.append(h('div.page-inner',
      h('div.page-head', h('div', h('h2', 'Statistics'), h('div.sub', 'Last 24 hours'))),
      h('div.grid.cols-4', tile),
      ui.card({ title: 'Message rate', icon: 'signal', body: chartEl })));

    const chart = charts.create(chartEl);                       // disposed automatically on unmount
    ctx.subscribe('aircraft', (snap) => tile.set(units.format('dist', snap.counts.maxRangeKm, { unit: false }), units.label('dist')));
    const data = await api.get('/api/stats/rates', { params: { hours: 24 }, signal: ctx.signal });
    chart.set(charts.area({ series: [{ name: 'msg/s', data: data.points }] }));   // points: [[tMs, value], ...]
    ctx.onCleanup(units.onChange(() => { /* re-render unit-dependent text */ }));
  },

  onRoute(route) {},            // optional: called instead of a remount when only the sub-route changes (#/stats/day -> #/stats/week)
  unmount() {},                 // optional: release what ctx does not track for you
};
```

A page file that does not exist (HTTP 404) renders the "module not installed" placeholder; a module that throws
while loading or mounting renders an error placeholder with the message.

### `ctx`

| Member | What it is |
|---|---|
| `ctx.api` | `api.js` (below) |
| `ctx.live` | `live.js` shared store |
| `ctx.units` | `units.js` |
| `ctx.ui` | `ui.js` component kit (`ctx.ui.h` is the element factory) |
| `ctx.charts` | per-page chart scope: same API as `charts.js`, everything created through it is disposed on unmount |
| `ctx.config` | `config.js` store: `.current` (model.Config, treat as read-only), `.get('ui.trailMinutes', 15)`, `.draft()`, `await .save(cfgOrMutator)`, `.subscribe(fn)`, `.load()` |
| `ctx.geo` | `geo.js` |
| `ctx.route` | `{ id, args: [...], query: {...}, path }` for `#/id/arg0/arg1?x=1` |
| `ctx.navigate(to, {replace})` | go to `'map/4CA1FA'` / `'#/settings'` |
| `ctx.signal` | `AbortSignal` aborted on unmount; pass to `ctx.api` calls |
| `ctx.subscribe(type, fn, opts)` | `live.on(...)` that unsubscribes itself on unmount |
| `ctx.every(ms, fn, {immediate})` | `setInterval` that is cleared on unmount |
| `ctx.onCleanup(fn)` | run `fn` on unmount (use for `units.onChange`, `config.subscribe`, DOM listeners) |
| `ctx.setTitle(text)` | top-bar + tab title |
| `ctx.status` | latest `model.Status` or `null` |
| `ctx.shared` | app-level key/value store that survives navigation: `.get(key)`, `.set(key, value)`, `.on(key, fn)`. Legacy key `alertToasts`: while set, the shell shows no alert toasts / sound (a page that still shows them itself). No current page sets it: the Live Map does so only when `alertsound.js` is missing (an old shell) |
| `ctx.page` | `{ id, title, icon }` |
| `ctx.shell` | shell controls, also `import { shell } from '../app.js'`: `shell.setKiosk(on)`, `shell.kiosk`, `shell.onKiosk(fn) -> off` (below) |

**Kiosk mode** (`ctx.shell.setKiosk(true)`): adds class `kiosk` to `<html>`; `css/shell.css` then hides the
sidebar, the top bar and the banners and gives the whole window to the page (toasts move to the corner). It works
on any page, survives navigation, is never persisted (a reload starts normal) and is left with **Escape** (unless a
dialog / menu / popover takes the key) or the small round exit button at the top centre. The shell dispatches a
window `resize` on every change; subscribe with `shell.onKiosk(fn)` to update a page's own toggle button.

**Admin calls** (`js/admin.js`): every write that may need the admin password goes through it, never ad-hoc 401
handling:

```js
import { createAdmin } from '../admin.js';
const admin = createAdmin(ctx, { onLocked() {}, onLogin(session) {} });      // hooks optional
await admin.run(() => api.post('/api/alerts/import', body), 'Importing rules needs the admin password.');
if (await admin.prompt('Log in to change the rules.')) { /* now admin */ }
```
`run(fn, what)` calls `fn()`; on 401 it shows the login dialog (one shared dialog for concurrent calls; `POST
/api/login` sets the session cookie) and retries once. It rejects with the original error for anything else, and
with an error carrying `.cancelled = true` when the user dismisses the dialog - show nothing for that. While a
`run()` is in flight the shell's generic "needs the admin password" toast is suppressed. `aircraft/admin.js` is a
re-export kept for old imports.

Shared keys in use: `map.coverage` - GeoJSON (Polygon / MultiPolygon / LineString Feature or FeatureCollection,
optional feature properties `color`, `fillOpacity`) drawn by the Live Map as the coverage overlay. Open the map
on an aircraft with `ctx.navigate('map/<ICAO>')`.

Rules of the road: units and timestamps arrive in API base units (ft, kt, ft/min, km, deg, dBFS, Unix ms, degC,
hPa, km/h, mm) - never print them raw, always go through `units`. Use `ctx.subscribe`/`ctx.every`/`ctx.onCleanup`
so nothing leaks. Keep 1 Hz updates cheap: `ui.setText`, `ui.dataTable`, `stat.set`, `kv.set`.
Give charts a sized host (`<div class="chart">`, height via `--h`).

## 2. `api.js`

```js
api.get(path, { params, signal, timeout })   api.post(path, body, opts)   api.put(path, body, opts)   api.del(path, opts)
api.status()  api.config()  api.saveConfig(cfg)  api.aircraft()  api.aircraftDetail(icao)  api.trails(minutes)  api.factoryReset()
api.on('unauthorized' | 'error', fn) -> off       api.headers = {...}  api.timeout = 15000  api.base = ''
```
Resolves with parsed JSON (or `null` for an empty body). Rejects with `ApiError`: `.message` (the server's
`{"error": "..."}` text), `.status` (0 = network / timeout / aborted), `.network`, `.timeout`, `.aborted`, `.body`.
Every non-GET request carries `X-CSRF-Token: api.csrfToken` (the `csrfToken` of the replies of `/api/session`,
`/api/login`, `/api/logout`, kept automatically; before the first write without a token, `/api/session` is read once).
Required in aggregator mode, checked when present in standalone mode.

## 3. `live.js`

One `EventSource('/api/events?delta=1')` for the whole app; falls back to polling `/api/aircraft` (1 s) and `/api/status`
(5 s) while it retries the stream with back-off. `delta=1` matters only for public viewers in aggregator mode: they get
one keyframe (`aircraft` with `seq`) and then `delta` events, which `live.js` applies and re-emits as a full
`aircraft` snapshot, so subscribers see no difference. A gap in the delta chain reopens the stream.

```js
live.on('aircraft', (snap) => {})   // model.LiveSnapshot, 1 Hz; fires immediately with the last value
live.on('status', (st) => {})       // model.Status, every 5 s
live.on('notice', ({ level, text, t }) => {})     live.on('state', (s) => {})   // 'connecting' | 'live' | 'polling' | 'offline'
live.on('alert', (a) => {})        // alerts.Alert from SSE `event: alert` (stream only: nothing while polling)
live.followAlerts({ signal, initial: 0, onAlerts(list, { initial, live }) {}, onReset() {}, onError(e) {} })
                                    // every alert once: stream + /api/alerts/recent catch-up (mount, reconnect, polling); -> { ready, stop(), sync() }
live.snapshot  live.status  live.aircraft /* Map icao -> Aircraft */  live.get('4CA1FA')  live.state
live.serverNow()                    // server clock estimate in Unix ms (use instead of Date.now() for ages)
await live.refreshStatus()
```
`live.on` returns an unsubscribe function; pass `{ immediate: false }` to skip the initial call.

## 4. `units.js`

Display systems: `aviation` (ft, kt, nm) / `metric` (m, km/h, km) / `imperial` (ft, mph, mi); stored per browser,
default from `config.ui.units`. `units.system`, `units.set(sys)`, `units.cycle()`, `units.onChange(fn) -> off`.

| Call | Input (base unit) | Example output |
|---|---|---|
| `units.alt(ft, {fl})` | feet | `35,000 ft` / `10,668 m` / `FL350` |
| `units.speed(kt)` | knots | `451 kt` / `835 km/h` |
| `units.dist(km)` | km | `100 nm` / `185 km` / `115 mi` |
| `units.vrate(fpm)` | ft/min | `+1,200 ft/min` / `+6.1 m/s` |
| `units.temp(c)` `units.pressure(hPa)` `units.wind(kmh)` `units.precip(mm)` `units.height(m)` `units.visibility(m)` | weather / site units | `12.4 °C`, `1,013.2 hPa`, `18 kt`, ... |
| `units.area(km2)` | km² | `292 nm²` / `1,000 km²` / `386 mi²` (square of the distance unit; 1 decimal below 10) |
| `units.format(kind, v, {digits, unit:false, sign})` | generic form of the above | kinds: `alt speed dist vrate temp pressure wind precip height visibility area` |
| `units.value(kind, v)` / `units.toBase(kind, v)` / `units.label(kind)` | numbers for charts and form inputs | `units.label('dist')` -> `nm` |
| `units.fl(ft)` `units.deg(v)` `units.bearing(v)` `units.cardinal(v)` | | `FL350`, `273°`, `273° W`, `W` |
| `units.dbfs(v)` `units.db(v)` `units.rate(v)` `units.pct(v)` `units.num(v, digits)` `units.int(v)` `units.si(v)` | | `−12.3 dBFS`, `1,842/s`, `1.2k` |
| `units.latlon(lat, lon)` `units.bytes(n)` | | `51.4775°N 0.4614°W`, `12.4 MB` |
| `units.age(sec)` `units.duration(ms)` `units.ago(ms)` | | `2m 10s`, `5 min ago` |
| `units.clock(ms, {utc, seconds, z})` `units.date(ms)` `units.dateTime(ms)` | Unix ms | `23:41:07Z`, `2026-11-11` |

Missing values (`null`, `undefined`, `NaN`) format as `—` (`units.DASH`).

## 5. `ui.js`

DOM: `h('div.card#id', { class, style, dataset, onclick, tip, html, text, ...attrs }, ...children)`,
`append`, `mount(el, ...children)` (replace), `clear`, `setText(el, text)` (writes only on change), `setClass`,
`escapeHtml`, `debounce`, `throttle`, `rafThrottle` (never fires while the tab is hidden), `onResize(el, fn) -> off`, `copy(text)`, `loadCss(href)`,
`prefs(key, defaults)` (localStorage object: `.get() .set(patch) .reset() .on(fn)`), `icon(name)` (see `icons.js`:
`ui.iconNames`; added for the new pages: `wave`, `bell`, `replay`, `report`, `mountain`). Any element with `data-tip="text"` (or the `tip` prop) gets a tooltip; `data-tip-place` = `top|bottom|left|right`.

| Component | Signature -> returns |
|---|---|
| Card | `ui.card({ title, sub, icon, actions, body, foot, flush, accent })` -> `<section>` with `.body`, `.setTitle()`, `.setSub()` |
| Stat tile | `ui.stat({ label, value, unit, sub, icon, accent, tip, spark })` -> el with `.set(value, unit)`, `.setSub()`, `.spark` |
| Buttons | `ui.button({ label, icon, variant: 'primary'|'ghost'|'danger', size: 'sm'|'lg', onClick, tip, href, pressed, disabled })` -> el with `.setLoading(bool)`, `.setLabel()`, `.setPressed()`; `ui.iconButton(icon, tip, onClick)` |
| Badges | `ui.badge(text, kind)` (kinds: `blue cyan amber violet teal green red solid-red adsb mlat modes tisb other`), `ui.sourceBadge(src)`, `ui.sourceLabel(src)` |
| Small bits | `ui.flag(cc, name)`, `ui.signalBar(rssi)` (`.set(rssi)`), `ui.meter(pct)` (`.set`), `ui.kv([[label, value, {text}], ...])` (`.set(indexOrLabel, value)`), `ui.section(title, icon)`, `ui.legend([{label, color}])` |
| Form | `ui.input({ type, value, placeholder, icon, suffix, mono, size, clearable, onInput, onChange, onEnter })` -> wrapper with `.input`, `.value`; `ui.select({ options, value, onChange })`; `ui.toggle({ label, checked, onChange })` (`.checked`); `ui.checkbox(...)`; `ui.slider({ min, max, step, value, format, onInput, onChange })` (`.value`); `ui.segmented({ options, value, onChange, block })` (`.value`); `ui.field({ label, hint, control, row })` (`.setError(msg)`, `.setHint()`) |
| Tabs | `ui.tabs({ tabs: [{ id, label, icon, badge, render(panelEl) | content }], active, onChange })` -> el with `.select(id)`, `.active`, `.panel(id)`, `.setBadge(id, text)` |
| States | `ui.skeleton({ lines, title, block })`, `ui.empty({ icon, title, text, action })`, `ui.loading(text)`, `ui.error(err, { retry })`, `ui.load(container, loader, render) -> reload()` (`render` must return a single DOM Node or nothing; an array is not mounted) |
| Toasts | `ui.toast(msg, { type, title, timeout, action })`, `ui.toast.success/.warn/.error/.info(msg)` |
| Dialogs | `ui.modal({ title, body, actions: [{ label, variant, value, onClick }], width, danger }) -> { el, body, close(v), closed }`; `await ui.confirm({ title, message, confirmLabel, danger, requireText: 'RESET' }) -> boolean`; `ui.alert(title, message)` |
| Drawer / popover | `ui.drawer({ title, body, foot, side, width, onClose }) -> { el, body, close }`; `ui.popover(anchor, content, { placement }) -> { el, close, reposition }`; `ui.menu(anchor, [{ label, icon, checked, onClick } | 'sep'])` |
| Static table | `ui.table({ columns: [{ key, label, align, num, render, sort, width }], rows, sort, onRowClick, empty, fixed })` -> el with `.setRows()`, `.setSort()`. `width`: number (px) or CSS length, written to a `<colgroup>`; `fixed: true` = `table-layout: fixed` (exact widths, ellipsis) |
| Live table | `ui.dataTable({...})` - virtualised + sortable, see below |

```js
const table = ui.dataTable({
  columns: [
    { key: 'callsign', label: 'Callsign', width: 'minmax(90px,1fr)', sort: true },
    { key: 'alt', label: 'Alt', width: '72px', align: 'right', num: true, sortDir: 'desc',
      sort: (a) => a.altBaro ?? null, render: (a) => units.format('alt', a.altBaro, { unit: false }) },
    { key: 'src', label: 'Src', width: '64px', update(td, a) { if (td.__s !== a.source) { td.__s = a.source; ui.mount(td, ui.sourceBadge(a.source)); } } },
  ],
  rowKey: (a) => a.icao, rowHeight: 28, minWidth: 420, sort: { key: 'alt', dir: 'desc' },
  onRowClick: (a) => ctx.navigate('map/' + a.icao), onRowHover: (a) => {}, rowClass: (a) => (a.emergency ? 'alert' : ''),
  empty: 'No aircraft',
});
host.append(table.el);                       // host needs a height (flex child / fixed)
ctx.subscribe('aircraft', (snap) => table.setRows(snap.aircraft));
// table.setSelected(key) .setHover(key) .scrollToKey(key) .setSort(key, dir) .getSort() .rows() .refresh() .destroy()
```
`render(row)` returns text (cheapest); `update(td, row)` lets you own the cell DOM (create children once, then
mutate). Missing sort values always sort last. Only visible rows exist in the DOM.

Layout/utility classes (css/base.css, css/components.css): `.page-inner .page-head .grid .grid-auto .cols-2/3/4
.span-2/3/4 .row .col .grow .spacer .num .unit .label .muted .chart .chart--sm/--lg .panel .table .kv .legend`.
Design tokens (colours, spacing, radii, fonts) are CSS variables in `css/tokens.css` - use them, do not hard-code.

## 6. `charts.js`

ECharts loads lazily on first use; handles work immediately (options are queued).

```js
const c = ctx.charts.create(el, option?, { renderer: 'canvas' | 'svg' });   // -> Chart
c.set(option, { replace })   c.on('click', fn)   c.loading(true)   c.resize()   c.dispose()   c.instance   c.whenReady
const echarts = await ctx.charts.ready();                                    // raw namespace when needed
ctx.charts.sparkline(el, data, { color, area, min, max, tooltip: (v, x) => '...' })
```
Option presets (plain ECharts options - tweak before passing to `create`/`set`):

| Preset | Input |
|---|---|
| `charts.line(o)` / `charts.area(o)` | `{ x?: [...], series: [{ name, data, color, area, smooth, step, dashed, y2, stack }], yName, yMin, yMax, yFormat, y2Name, legend, zoom, valueFormat }` - omit `x` for a time axis with `data: [[tMs, v], ...]`. The legend (top row, scrollable) and the y-axis names (`yName` / `y2Name`, a row of their own just above the plot, growing inwards) never overlap: the grid top is reserved for both |
| `charts.bar(o)` | `{ x, series, stack, horizontal, ... }` |
| `charts.heatmap(o)` | `{ x, y, data: [[xi, yi, v], ...], min, max, ramp, valueFormat }` |
| `charts.polar(o)` | north-up, clockwise: `{ series: [{ name, data: [[radius, bearingDeg], ...], type: 'line'|'scatter'|'bar', area, color }], maxR, rFormat, bearingStep, angleFormat }`; `angleFormat`: `'mixed'` (default: N / E / S / W, degrees elsewhere), `'compass'` (N, NNE, NE ... on multiples of 22.5°), `'degrees'`, or `fn(deg) -> label`. Same labels standalone: `charts.bearingLabel(deg, mode)` |
| `charts.scatter(o)` | `{ series: [{ name, data: [[x, y, v?], ...], size }], xName, yName, visual: { min, max, ramp } }` |
| `charts.donut(o)` | `{ data: [{ name, value, color }], centerText }` |
| `charts.sparklineOption(o)` | `{ data, color, area, min, max, tooltip }` |

Also: `charts.palette`, `charts.colors` (`adsb mlat modes tisb blue cyan amber ...`), `charts.ramps`
(`blue heat signal diverging`), `charts.fade(color)`, `charts.theme`.

## 7. `geo.js`

`distanceKm(lat1, lon1, lat2, lon2)`, `bearing(...)`, `destination(lat, lon, brgDeg, km) -> [lon, lat]`,
`circle(lat, lon, km, steps)`, `arc(...)`, `sector(lat, lon, km, azDeg, widthDeg)` (GeoJSON rings, `[lon, lat]`),
`sun(date) -> { lat, lon }` (sub-solar point), `sunElevation(lat, lon, date)`, `dayPhase(elevDeg)`,
`nightPolygon(date, depressionDeg)`.

## 8. `alertsound.js`

```js
import { play, unlock, isMuted, setMuted, onMuteChange } from '../alertsound.js';
play('critical');     // 'info' | 'warning' | 'critical' - synthesised chime (Web Audio, no files); ignores the mute switch
unlock();             // call from a user gesture; the shell does this on the first pointer / key press
isMuted() / setMuted(bool) / onMuteChange(fn) -> off      // per-browser mute (localStorage, in-memory fallback)
```
The shell plays the chime for alerts (see section 1); a page only calls `play()` to preview a sound (rule editor,
the Alerts page's Sound switch, which is the UI for `setMuted`). `alerts/sound.js` re-exports it for old imports.

## 9. Network (aggregator) mode

`app.js` asks `GET /api/session` first (`net.init()`): `aggregator: true` switches the shell to network mode;
anything else (or no answer) keeps the standalone shell exactly as before. In network mode:

- **Scope** (`net.scope`): `public` (no session), `owner`, `admin` (admin scope with the second factor). The
  sidebar is `net/model.js navFor(scope)`; every route goes through `pageAccess(id, net)` (`PAGE_ACCESS`): the
  merged map, Network, Feed Check, claim and setup are public; My stations / Account need a login; the
  single-station pages (Statistics, Antenna Lab, RF Lab, Timing, Weather, Aircraft, Reports, Alerts, Settings)
  show the host station and are admin-only, like on the server. Pages that are not allowed show a placeholder
  with Log in / Create account.
- **Request guard**: while public, `api.guard` refuses (in the browser, status 403, `.blocked`) every path that
  is not in `PUBLIC_ALLOWED` - the public UI never even asks for receiver data. `/api/config` is loaded only
  for the admin.
- **ctx.station** (`net.station`): `{ sid, name, scope, located, lat?, lon?, antennas, local }`. Standalone:
  the local station from the configuration (`sid ''`). Network: the station chosen in the top-bar picker
  (owner: own stations, admin: all). `api.station` adds `station=<sid>` to the station-scoped paths
  (`/api/events`, `/api/aircraft[/icao]`, `/api/trails`, `/api/me/aircraft`).
- **Changes**: `net.on(reason)` with `session` | `station` | `stations` | `view`. The shell restarts the live
  stream on `session` / `station` and calls the page's optional `onNet(reason)`; pages without it are mounted
  again. Login / logout go through `net/auth.js` (`loginDialog`, `registerDialog`, `logout`); `admin.js`
  uses the account login in network mode.
- **Live Map**: `map/network.js` adds the View selector (`net.view`: Network / My stations / Station / Admin),
  station markers per view (public: opt-in pins only), MLAT uncertainty circles coloured by grade, the
  "heard by mine" ring and the "only mine" filter. Receiver-centred overlays (rings, beams, compass, coverage,
  range, terrain, heat) exist only in the Station view (`overlayConfig`).
- **Pages**: `network` (public numbers and opt-in stations), `check` (`#/me/check`, Feed Check), `claim`
  (`#/me/claim`), `me` (`#/me/stations[/sid[/tab]]`), `account` (`#/me/account`: password, authenticator
  with QR code, recovery codes, sessions, export, delete), `admin` (`#/admin/stations|tokens|audit|policy`),
  `setup` (`#/setup`, first-run administrator). Routes and reply shapes: `net/endpoints.js` (normalisers turn
  the server DTOs into page models; change only that file when the server changes).
- **Settings**: Network uplinks section (`settings/sections-uplinks.js`, `uplinks[]` + `GET/POST /api/uplinks`,
  consent text before timestamps); settings fixed by the operator file or `AIRVIEW_*` (`locked` in
  `GET /api/config`) are disabled with their origin (`settings/form.js markLocked`).

## Phase 4-5 modules (WP-W45-web)

| Module | What |
|---|---|
| `net/pager.js` | `pagerBar` (server-paged lists: `GET /api/admin/stations?offset=&limit=&sort=&dir=`, admin share links) and `pagedTable` (client-side search, whole-list sort, paging; one page in the DOM) for 1,000+ station tables. Sorting lives in the pure `net/sort.js` (node test `web/dev/test/pager.test.mjs`). |
| `net/shares.js` | Share links: `#/me/shares` (owner list, create dialog with the location warning that must be confirmed, link shown once with Copy and QR), the admin tab "Share links". |
| `pages/share.js` | `#/share/<token>`: the read-only shared view (map with short browser-side trails, aircraft list, optional station statistics). Public page, never in a sidebar. |
| `net/fedadmin.js` | Admin tab "Federation": node ID, peers, out-feeds, and the peer list with per-peer policy saved through `PUT /api/config` (locked settings respected). |
| `mlat/phase45.js` | MLAT console cards: ADS-B verification alerts (with a banner while aircraft are suspect), "Where to add a station" (CRLB gain), Mode A/C targets. |
| `net/locadmin.js` | Locations tab: paged survey table and the network bundle adjustment card. |
| `map/network.js` | MLAT quality menu: new admin mode `improve` (benefit per candidate cell, best sites ranked `#1..`). |

Owner Location tab: estimates of method `bundle` are labelled and can be confirmed like survey estimates;
the Privacy tab has "Share this station".