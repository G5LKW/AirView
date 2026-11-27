# web - API notes (from the package author)

## Go: package web (import "airview/web")
- `var FS embed.FS` — `//go:embed all:static`; names carry the `static/` prefix.
- `func Static() fs.FS` — rooted at the site root (`index.html`, `js/app.js`, `vendor/...`).
- `func ContentType(name string) string` — MIME by extension (.js/.mjs -> `text/javascript; charset=utf-8`, .woff2, .svg, .json, ...; no extension -> text/plain; unknown -> octet-stream).
- `func Handler() http.Handler` — serves Static() at "/": "/" -> index.html, GET/HEAD only, correct MIME + `nosniff`, strong ETag + `Cache-Control: no-cache` (304 on If-None-Match), gzip for text assets (compressed once, cached), 404 for unknown paths and directories. Server usage: mount `/api/...`, `/tilecache/...` first, then `mux.Handle("/", web.Handler())`.
- Site URLs: `/` (app, hash routes `#/<page>[/arg...]`), `/remote.html?url=http://host:port/` (remote-web shell; Disconnect does GET+PUT /api/config with mode="standalone" on the LOCAL server, then opens `./#/settings`).

## Page-module contract (full text: web/static/js/README.md)
Page ids: map aircraft stats antenna weather forecast timing guess settings. File `web/static/js/pages/<id>.js`:
```js
export default {
  id, title?, fullBleed?,              // fullBleed: no padding/scroll (map uses it)
  async mount(container, ctx) {},      // container = empty <div class="page">; throw -> error placeholder
  onRoute(route) {},                   // optional: sub-route change (#/id/a -> #/id/b) without remount
  unmount() {},                        // optional
};
```
Missing file (404) -> "module not installed" placeholder (app.js probes with HEAD first).

ctx: `api, live, units, ui, charts` (per-page scope, auto-disposed), `config` (store), `geo`, `shared`, `route {id,args,query,path}`, `navigate(to,{replace})`, `signal` (AbortSignal on unmount), `subscribe(type,fn,opts)` (live.on + auto-off), `every(ms,fn,{immediate})`, `onCleanup(fn)`, `setTitle(text)`, `status` (model.Status|null), `page {id,title,icon}`.
- `ctx.config`: `.current` (model.Config), `.get('ui.trailMinutes', 15)`, `.draft()`, `await .save(cfg | (draft)=>{...})`, `.subscribe(fn)`, `.load()`.
- `ctx.shared`: `.get(key) .set(key,value) .on(key,fn)`; survives navigation. Key in use: `map.coverage` = GeoJSON Polygon/MultiPolygon/LineString Feature(Collection), optional props `color`, `fillOpacity` -> drawn by the Live Map. Open map on an aircraft: `ctx.navigate('map/<ICAO>')`.

## api.js
`api.get(path,{params,signal,timeout})`, `post(path,body,opts)`, `put`, `del`; typed: `status() config() saveConfig(cfg) aircraft() aircraftDetail(icao) trails(minutes) factoryReset()`; `api.on('unauthorized'|'error', fn)`; `api.headers`, `api.timeout`, `api.base`. Rejects with `ApiError {message (server {"error"}), status (0 = network/timeout/abort), network, timeout, aborted, body}`.

## live.js
One EventSource on /api/events (events aircraft/status/notice/alert); fallback polling /api/aircraft 1 s + /api/status 5 s with stream retry back-off and 8 s silence watchdog (alerts are stream-only). `live.on('aircraft'|'status'|'notice'|'alert'|'state', fn, {immediate=true}) -> off`; `live.snapshot`, `live.status`, `live.aircraft` (Map icao->Aircraft), `live.get(icao)`, `live.state` ('connecting'|'live'|'polling'|'offline'), `live.serverNow()`, `live.refreshStatus()`.

## units.js
Systems aviation|metric|imperial (localStorage `airview.units`, default config.ui.units). `units.system`, `set(sys)`, `cycle()`, `onChange(fn)->off`. Formatters from API base units: `alt(ft,{fl})`, `speed(kt)`, `dist(km)`, `vrate(fpm)`, `temp(c)`, `pressure(hPa)`, `wind(kmh)`, `precip(mm)`, `height(m)`, `visibility(m)`, generic `format(kind,v,{digits,unit:false,sign})`, `value(kind,v)`, `toBase(kind,v)`, `label(kind)`; plus `fl deg bearing cardinal dbfs db rate pct num int si latlon bytes age duration ago clock date dateTime`. Missing -> "—".

## ui.js (ctx.ui)
DOM: `h('tag.cls#id', props, ...children)`, `append mount clear setText setClass escapeHtml debounce throttle rafThrottle onResize copy loadCss prefs(key,defaults) icon(name) tip(el,text,place)`; `data-tip` attribute = tooltip.
Components: `card({title,sub,icon,actions,body,foot,flush,accent})` (.body .setTitle), `stat({label,value,unit,sub,icon,accent,spark})` (.set(value,unit) .setSub .spark), `button({label,icon,variant,size,onClick,tip,href,pressed})` (.setLoading .setLabel .setPressed), `iconButton`, `badge(text,kind)`, `sourceBadge(src)`, `sourceLabel`, `flag(cc,name)`, `signalBar(rssi)`, `meter(pct)`, `kv(rows)` (.set), `section`, `legend`, `input({...})` (.input .value), `select`, `toggle` (.checked), `checkbox`, `slider` (.value), `segmented` (.value), `field({label,hint,control,row})` (.setError), `tabs({tabs,active,onChange})` (.select .panel .setBadge), `skeleton empty loading error load(container, loader, render)->reload`, `toast(msg,{type,title,timeout,action})` + `.success/.warn/.error/.info`, `modal({...})->{el,body,close,closed}`, `await confirm({title,message,danger,requireText})`, `alert`, `drawer({...})->{el,body,close}`, `popover(anchor,content,{placement})`, `menu(anchor,items)`, `table({columns,rows,sort,onRowClick})` (.setRows), `dataTable({columns:[{key,label,width,align,num,render(row)|update(td,row),cls,sort,sortDir,tip}],rowKey,rowHeight,minWidth,sort,onRowClick,onRowHover,rowClass,onSort,empty}) -> {el,setRows,rows,setSort,getSort,setSelected,setHover,scrollToKey,refresh,destroy}` (virtualised; host needs a height).
CSS classes: `.page-inner .page-head .grid .grid-auto .cols-2/3/4 .span-N .row .col .grow .num .unit .label .muted .chart(--sm/--lg) .panel .table .kv`; tokens in css/tokens.css.

## charts.js (ctx.charts)
Lazy ECharts. `create(el, option?, {renderer}) -> Chart {set(option,{replace}), on, loading, resize, dispose, instance, whenReady}`; `ready()`; `sparkline(el,data,{color,tooltip})`; presets returning ECharts options: `line(o) area(o) bar(o) heatmap(o) polar(o) scatter(o) donut(o) sparklineOption(o)`; `palette colors ramps fade theme`; `scope()`/`disposeAll()`.

## geo.js
`distanceKm bearing destination(lat,lon,brg,km)->[lon,lat] circle arc sector unwrap sun(date) sunElevation(lat,lon,date) dayPhase(elev) nightPolygon(date,depressionDeg)`.

## Mock server
`node web/dev/mock-server.mjs [--port=8099] [--n=80] [--unlocated] [--demo] [--no-sse] [--no-tilecache] [--no-emergency] [--lat= --lon=]` — serves web/static plus status, config GET/PUT, aircraft, aircraft/{icao}, trails, events (SSE), factory-reset, /tilecache proxy. `node web/dev/make-map-style.mjs [stock-dark-style]` regenerates map/airview-dark.json.

## localStorage keys
`airview.units`, `airview.nav.open`, `airview.lowpower`, `airview.map.v1`, `airview.map.list`.
