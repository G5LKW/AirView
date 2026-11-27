// pages/share.js - the read-only view behind a share link (#/share/<token>; WP-H2, DESIGN §8.2 "My map …
// share links in phase 4"). Anyone with the link sees, without an account:
//
//   - the aircraft the owner's chosen stations heard in the last minute (map and list), in the public shape:
//     no receiver list, distance, bearing or signal; low aircraft may be rounded ("approximate");
//   - for links that include them, the stations' non-locating statistics (message rate, aircraft, uptime).
//
// Never shown: the station position or pin, its ID, range, coverage or polar data (the server does not send
// them: internal/server/api_share.go). The token lives only in the URL fragment, so it is not sent to the
// server with the page itself, only with the API calls of this page.
//
// API: GET /api/share/{token}, /api/share/{token}/aircraft (every 3 s while visible), /api/share/{token}/stats
// (every 30 s, scope map+stats). Short trails are kept in the browser only.

import { EP } from '../net/endpoints.js';
import { scrubPublic } from '../net/model.js';
import { BASEMAPS, LABEL_FONT, styleFor, makeTransformRequest } from '../map/basemaps.js';
import { altColor, GROUND } from '../map/palette.js';

const REFRESH_MS = 3000;
const STATS_MS = 30000;
const TRAIL_MS = 3 * 60 * 1000;
const EMPTY = { type: 'FeatureCollection', features: [] };
const okNum = (v) => typeof v === 'number' && Number.isFinite(v);

let libPromise = null;
function loadLib(ui) {
  if (!libPromise) {
    libPromise = Promise.all([
      import('../../vendor/maplibre/maplibre-gl.mjs'),
      ui.loadCss(new URL('../../vendor/maplibre/maplibre-gl.css', import.meta.url)),
    ]).then(([m]) => m);
    libPromise.catch(() => { libPromise = null; });
  }
  return libPromise;
}

let P = null;

export default {
  id: 'share',
  title: 'Shared view',

  async mount(container, ctx) {
    const { ui, api } = ctx;
    const { h } = ui;
    const token = ctx.route.args[0] || '';
    P = { ctx, map: null, timers: [], trails: new Map(), fitted: false, ready: false };
    const head = h('div.page-head', h('div', h('h2', 'Shared live view'), h('div.sub', 'Loading…')), h('div.actions', ui.badge('read-only', 'other')));
    const setSub = (t) => ui.setText(head.querySelector('.sub'), t);
    const root = h('div.page-inner.share-page', head);
    container.append(root);
    if (!/^[A-Za-z0-9_-]{16,64}$/.test(token)) { root.append(gone(ui)); setSub(''); return; }
    let meta;
    try {
      meta = scrubPublic(await api.get(EP.share(token), { signal: ctx.signal }));
    } catch (e) {
      if (e.aborted) return;
      setSub('');
      root.append(e.status === 404 ? gone(ui) : ui.error(e, { title: 'The shared view could not be loaded' }));
      return;
    }
    head.querySelector('h2').textContent = meta.label || 'Shared live view';
    setSub([meta.network, meta.stationCount === 1 ? 'one station' : `${ctx.units.int(meta.stationCount || 0)} stations`, meta.expires ? `link valid until ${ctx.units.dateTime(meta.expires)}` : null].filter(Boolean).join(' · '));

    const mapEl = h('div.share-map');
    const counts = h('div.row.gap-3.share-counts');
    const listHost = h('div.share-list');
    const statsHost = h('div');
    root.append(
      h('div.grid.cols-3.net-grid.share-grid',
        ui.card({ title: 'Live map', icon: 'map', class: 'span-2 share-map-card', flush: true, body: mapEl }),
        ui.card({ title: 'Aircraft', icon: 'plane', sub: counts, body: listHost })),
      meta.scope === 'map+stats' ? ui.card({ title: 'Stations', icon: 'antenna', sub: 'Non-locating statistics', body: statsHost }) : null,
      h('p.muted.net-privacy-note', ui.icon('shield'), 'A read-only view its owner shared with you. It shows the aircraft the owner\'s stations receive, never where the stations are.'
        + (meta.coarseLow ? ' Low aircraft and aircraft on the ground are shown approximately.' : '')));

    const table = ui.table({ rows: [], sort: { key: 'alt', dir: 'desc' }, empty: 'No aircraft right now', fixed: false, columns: [
      { key: 'id', label: 'Aircraft', sort: (a) => (a.callsign || a.icao), render: (a) => h('span.col', h('b', a.callsign || a.icao), h('span.muted.small', [a.type, a.reg].filter(Boolean).join(' · ') || a.icao)) },
      { key: 'alt', label: 'Altitude', align: 'right', num: true, sort: (a) => (a.onGround ? -1 : a.altBaro ?? a.altGeom ?? null), sortDir: 'desc', render: (a) => (a.onGround ? 'ground' : okNum(a.altBaro ?? a.altGeom) ? ctx.units.alt(a.altBaro ?? a.altGeom) : '—') },
      { key: 'gs', label: 'Speed', align: 'right', num: true, sort: (a) => a.gs ?? null, render: (a) => (okNum(a.gs) ? ctx.units.speed(a.gs) : '—') },
      { key: 'src', label: '', render: (a) => h('span.row.gap-1', ui.sourceBadge(String(a.source || '').startsWith('mlat') ? 'mlat' : a.source), a.coarse ? ui.badge('approx.', 'other', { tip: 'Position rounded: low aircraft reveal where receivers are' }) : null) },
    ] });
    ui.mount(listHost, table);

    try {
      await initMap(ctx, mapEl);
    } catch (e) {
      ui.mount(mapEl, ui.empty({ icon: 'map', title: 'The map could not be shown', text: e.message || '' }));
    }

    const refresh = async () => {
      let snap;
      try {
        snap = scrubPublic(await api.get(EP.shareAircraft(token), { signal: ctx.signal, timeout: 10000 }));
      } catch (e) {
        if (e.aborted) return;
        if (e.status === 404) { stop(); root.replaceChildren(head, gone(ui)); setSub(''); }
        return;
      }
      const list = (snap?.aircraft || []).filter((a) => a && a.icao);
      table.setRows(list);
      const withPos = list.filter((a) => okNum(a.lat) && okNum(a.lon));
      ui.mount(counts, h('span', `${ctx.units.int(list.length)} aircraft`), h('span.muted', `${ctx.units.int(withPos.length)} with position`));
      drawAircraft(withPos, snap?.now || Date.now());
    };
    const refreshStats = async () => {
      if (meta.scope !== 'map+stats') return;
      try {
        const r = scrubPublic(await api.get(EP.shareStats(token), { signal: ctx.signal }));
        ui.mount(statsHost, ui.table({ rows: r?.entries || [], columns: [
          { key: 'name', label: 'Station', render: (s) => h('span.row.gap-2', h('span.dot.' + (s.online ? 'ok' : 'off')), s.name) },
          { key: 'rate', label: 'Messages', align: 'right', num: true, render: (s) => (okNum(s.stats?.msgRate) ? ctx.units.num(s.stats.msgRate, 0) + ' /s' : '—') },
          { key: 'ac', label: 'Aircraft', align: 'right', num: true, render: (s) => (okNum(s.stats?.aircraftDay ?? s.stats?.aircraft) ? ctx.units.int(s.stats.aircraftDay ?? s.stats.aircraft) : '—') },
          { key: 'pos', label: 'Positions', align: 'right', num: true, render: (s) => (okNum(s.stats?.positions) ? ctx.units.si(s.stats.positions) : '—') },
          { key: 'up', label: 'Uptime', align: 'right', num: true, render: (s) => (okNum(s.stats?.uptimePct) ? ctx.units.num(s.stats.uptimePct, 1) + ' %' : '—') },
        ] }));
      } catch (e) { if (!e.aborted && e.status !== 404) ui.mount(statsHost, ui.error(e)); }
    };
    const stop = () => { for (const t of P?.timers || []) clearInterval(t); };
    await refresh();
    refreshStats();
    P.timers.push(setInterval(() => { if (!document.hidden) refresh(); }, REFRESH_MS));
    P.timers.push(setInterval(() => { if (!document.hidden) refreshStats(); }, STATS_MS));
  },

  onRoute(route) { if (P && route.args[0] && route.args[0] !== P.ctx.route.args[0]) P.ctx.navigate('share/' + route.args[0], { replace: true }); },
  onNet() {},
  unmount() {
    if (!P) return;
    for (const t of P.timers) clearInterval(t);
    try { P.map?.remove(); } catch { /* already gone */ }
    P = null;
  },
};

function gone(ui) {
  return ui.empty({ icon: 'lock', title: 'This link does not work (any more)', text: 'It may have expired, or its owner revoked it. Ask the person who sent it for a new one.' });
}

async function initMap(ctx, el) {
  const maplibregl = await loadLib(ctx.ui);
  if (!P) return;
  const map = new maplibregl.Map({
    container: el, style: styleFor(BASEMAPS[0]), center: [8, 30], zoom: 1.6, minZoom: 1, maxZoom: 15, maxPitch: 0,
    dragRotate: false, pitchWithRotate: false, touchPitch: false, fadeDuration: 0, attributionControl: false,
    transformRequest: makeTransformRequest(() => !!ctx.live?.status?.tileCache),
  });
  map.touchZoomRotate.disableRotation();
  map.keyboard.disableRotation();
  map.addControl(new maplibregl.AttributionControl({ compact: true }), 'bottom-right');
  map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');
  P.map = map;
  const install = () => {
    if (map.getSource('sh-ac')) return;
    map.addSource('sh-trail', { type: 'geojson', data: EMPTY });
    map.addSource('sh-ac', { type: 'geojson', data: EMPTY });
    map.addLayer({ id: 'sh-trail', type: 'line', source: 'sh-trail', paint: { 'line-color': ['get', 'c'], 'line-width': 1.6, 'line-opacity': 0.7 } });
    map.addLayer({ id: 'sh-ac', type: 'circle', source: 'sh-ac', paint: {
      'circle-radius': ['interpolate', ['linear'], ['zoom'], 3, 3.5, 10, 6], 'circle-color': ['get', 'c'],
      'circle-stroke-color': ['case', ['==', ['get', 'coarse'], 1], '#94a3b8', '#0b1220'], 'circle-stroke-width': ['case', ['==', ['get', 'coarse'], 1], 2, 1] } });
    if (!P.noText) map.addLayer({ id: 'sh-ac-dir', type: 'symbol', source: 'sh-ac', filter: ['has', 'trk'], layout: {
      'text-field': '▲', 'text-size': 9, 'text-rotate': ['get', 'trk'], 'text-rotation-alignment': 'map', 'text-allow-overlap': true, 'text-ignore-placement': true,
      'text-font': LABEL_FONT, 'text-offset': [0, -1.1] }, paint: { 'text-color': ['get', 'c'] } });
    if (!P.noText) map.addLayer({ id: 'sh-ac-label', type: 'symbol', source: 'sh-ac', minzoom: 6, layout: {
      'text-field': ['get', 'label'], 'text-font': LABEL_FONT, 'text-size': 10.5, 'text-offset': [0, 1.1], 'text-anchor': 'top', 'text-optional': true },
    paint: { 'text-color': '#d8e4ff', 'text-halo-color': 'rgba(1,4,11,0.9)', 'text-halo-width': 1.4 } });
    const popup = new maplibregl.Popup({ closeButton: false, closeOnClick: false, offset: 10, className: 'ac-popup' });
    map.on('mousemove', 'sh-ac', (e) => {
      const f = e.features?.[0];
      if (!f) return;
      map.getCanvas().style.cursor = 'pointer';
      popup.setLngLat(f.geometry.coordinates).setText(f.properties.tip || '').addTo(map);
    });
    map.on('mouseleave', 'sh-ac', () => { map.getCanvas().style.cursor = ''; popup.remove(); });
    P.ready = true;
    if (P.last) drawAircraft(P.last.list, P.last.now);
  };
  map.on('load', install);
  map.on('error', (e) => {
    // An unreachable vector style (offline network): fall back to a plain background so aircraft still show.
    if (!map.isStyleLoaded() && e?.error && !P?.ready) {
      P.noText = true;
      map.setStyle({ version: 8, sources: {}, layers: [{ id: 'bg', type: 'background', paint: { 'background-color': '#050c1b' } }] });
      map.once('styledata', install);
    }
  });
}

function drawAircraft(list, now) {
  if (!P) return;
  P.last = { list, now };
  const { units } = P.ctx;
  for (const a of list) {
    const t = P.trails.get(a.icao) || [];
    const lastPt = t[t.length - 1];
    if (!lastPt || lastPt[1] !== a.lat || lastPt[2] !== a.lon) t.push([now, a.lat, a.lon, a.onGround ? null : a.altBaro ?? a.altGeom ?? null, !!a.coarse]);
    P.trails.set(a.icao, t);
  }
  const seen = new Set(list.map((a) => a.icao));
  for (const [k, t] of P.trails) {
    while (t.length && t[0][0] < now - TRAIL_MS) t.shift();
    if (!t.length || (!seen.has(k) && t[t.length - 1][0] < now - 60000)) P.trails.delete(k);
  }
  if (!P.ready || !P.map) return;
  const colorOf = (a) => (a.onGround ? GROUND : altColor(a.altBaro ?? a.altGeom));
  const feats = list.map((a) => ({ type: 'Feature', geometry: { type: 'Point', coordinates: [a.lon, a.lat] }, properties: {
    c: colorOf(a), coarse: a.coarse ? 1 : 0, label: a.callsign || a.icao, ...(okNum(a.track) && !a.coarse ? { trk: a.track } : {}),
    tip: [a.callsign || a.icao, a.onGround ? 'on the ground' : okNum(a.altBaro ?? a.altGeom) ? units.alt(a.altBaro ?? a.altGeom) : null, okNum(a.gs) ? units.speed(a.gs) : null, a.typeDesc || a.type, a.coarse ? 'approximate position' : null].filter(Boolean).join(' · '),
  } }));
  const lines = [];
  for (const t of P.trails.values()) {
    const pts = t.filter((p) => !p[4]);
    if (pts.length < 2) continue;
    lines.push({ type: 'Feature', properties: { c: altColor(pts[pts.length - 1][3]) }, geometry: { type: 'LineString', coordinates: pts.map((p) => [p[2], p[1]]) } });
  }
  P.map.getSource('sh-ac')?.setData({ type: 'FeatureCollection', features: feats });
  P.map.getSource('sh-trail')?.setData({ type: 'FeatureCollection', features: lines });
  if (!P.fitted && feats.length) {
    P.fitted = true;
    let s = 90; let w = 180; let n = -90; let e = -180;
    for (const a of list) { s = Math.min(s, a.lat); n = Math.max(n, a.lat); w = Math.min(w, a.lon); e = Math.max(e, a.lon); }
    if (n - s < 0.5) { s -= 0.5; n += 0.5; }
    if (e - w < 0.5) { w -= 0.5; e += 0.5; }
    P.map.fitBounds([[w, s], [e, n]], { padding: 40, duration: 0, maxZoom: 9 });
  }
}
