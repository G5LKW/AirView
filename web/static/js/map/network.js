// map/network.js - the Live Map's network (aggregator) layer: the View selector (Network / My stations /
// Station / Admin), station markers per view, MLAT uncertainty circles with the fix grade, the "heard by my
// stations" highlight and the "only mine" filter (DESIGN §11). In standalone mode nothing of this is shown
// and map.js behaves exactly as before.
//
// Station markers per view (DESIGN §8.2):
//   network  opt-in public pins only (GET /api/public/stations; a deliberately inexact stored point)
//   mine     the viewer's own stations at their exact positions (owner data)
//   station  the selected station (exact), drawn by the receiver overlays of map.js
//   admin    every station with a position in the admin registry list (exact)
//
//   const nv = createNetView(ctx, { onChange(reason) {} });
//   nv.el                      // the selector bar (append to the map box)
//   nv.install(map, beforeId)  // sources + layers (call from installLayers, before the aircraft layers)
//   nv.update(prepared)        // after each snapshot: MLAT sigma circles
//   nv.hidesReceiver()         // true when receiver overlays (rings, beams, range, heat ...) must stay off
//   nv.filter(a)               // false = hide this aircraft ("only mine")
//   nv.refresh()               // reload the station markers (view / station / session changed)
//
// MLAT quality layer (button MLAT quality in the bar; DESIGN §8.2, §11):
//   measured  hold-out error per 1° cell (P50 colour, P50/P95 label) for one altitude band or the best band:
//             public and owner views use GET /api/public/mlat (cells with ≥ 5 contributing stations only),
//             the admin view GET /api/admin/mlat/quality?cells=1 (every cell)
//   expected  CRLB σ_h per 0.25° grid point at a target height, admin view only (GET /api/admin/mlat/crlb)
//   improve   where one more (GPS) station would improve the CRLB most, per candidate cell, best sites
//             ranked #1.. (admin view only, GET /api/admin/mlat/crlb/improve)
// Refreshed every minute while shown; switched off when the view no longer allows the chosen layer.

import { ui, h, icon } from '../ui.js';
import { geo } from '../geo.js';
import { net } from '../net/store.js';
import { EP, normPublicStations } from '../net/endpoints.js';
import { VIEWS, viewHidesReceiver, publicPin, scrubPublic, shortSid } from '../net/model.js';
import { LABEL_FONT } from './basemaps.js';
import { GRADE_COLOR, MLAT_UNGRADED, QUALITY_BANDS, QUALITY_SCALE, IMPROVE_SCALE, qualityCellsFc, crlbCellsFc, crlbHeights, heightLabel, improveCellsFc } from '../mlat/model.js';

const fc = (features) => ({ type: 'FeatureCollection', features });
const EMPTY = fc([]);
const SIGMA_MIN_M = 30;     // smaller circles are not drawn (invisible at map scales)
const SIGMA_MAX_M = 5000;   // larger values are not displayable fixes anyway

export function createNetView(ctx, hooks = {}) {
  const prefs = ui.prefs('airview.map.net', { onlyMine: false, sigma: true, mlatq: 'off', mlatqBand: -1, crlbAlt: 3048 });
  let map = null;
  let stationsFc = EMPTY;
  let sigmaFc = EMPTY;
  let loadSeq = 0;
  let qualityFc = EMPTY;
  let qualitySeq = 0;
  let qualityTimer = null;
  let crlbAlts = [914.4, 3048, 9144];

  // ---- selector ----
  const seg = h('div.seg.mapv-views', { role: 'group', 'aria-label': 'Map view' });
  const onlyMine = ui.toggle({ label: 'Only mine', checked: prefs.get().onlyMine, onChange: (v) => { prefs.set({ onlyMine: v }); hooks.onChange?.('filter'); } });
  onlyMine.classList.add('mapv-onlymine');
  const stationName = h('button.mapv-viewst', { type: 'button', tip: 'Choose the station' }, icon('antenna'), h('span'));
  stationName.addEventListener('click', () => hooks.pickStation?.(stationName));
  const qBtn = h('button.mapv-mlatq', { type: 'button', tip: 'MLAT accuracy layer', 'aria-haspopup': 'menu' }, icon('target'), h('span', 'MLAT quality'));
  qBtn.addEventListener('click', () => openQualityMenu());
  const qLegend = h('div.mapv-mlatq-legend.hidden', { 'aria-live': 'polite' });
  const el = h('div.mapv-netbar.hidden', seg, stationName, onlyMine, qBtn, qLegend);

  function renderBar() {
    const agg = net.aggregator;
    el.classList.toggle('hidden', !agg);
    if (!agg) return;
    const allowed = net.views;
    ui.mount(seg, VIEWS.filter((v) => allowed.includes(v.id)).map((v) => {
      const b = h('button', { type: 'button', 'aria-pressed': String(net.view === v.id), tip: v.tip, onclick: () => { if (v.id === 'station' && !net.station) { hooks.pickStation?.(b); return; } net.setView(v.id); } },
        icon(v.icon), h('span', v.id === 'station' && net.station ? net.station.name : v.label));
      return b;
    }));
    seg.classList.toggle('hidden', allowed.length < 2);
    onlyMine.classList.toggle('hidden', net.view !== 'mine');
    stationName.classList.add('hidden');
    qBtn.classList.toggle('hidden', !qualityViews().length);
    qBtn.setAttribute('aria-pressed', String(qualityMode() !== 'off'));
  }

  // ---- layers ----
  function install(m, beforeId) {
    map = m;
    const dark = !document.querySelector('.mapv.light-map');
    const src = (id, data) => { if (!map.getSource(id)) map.addSource(id, { type: 'geojson', data }); };
    const add = (layer) => { if (!map.getLayer(layer.id)) map.addLayer(layer, map.getLayer(beforeId) ? beforeId : undefined); };
    src('net-mlatq', qualityFc);
    src('net-sigma', sigmaFc);
    src('net-stations', stationsFc);
    add({ id: 'net-mlatq-fill', type: 'fill', source: 'net-mlatq', paint: { 'fill-color': ['get', 'c'], 'fill-opacity': 0.22, 'fill-outline-color': ['get', 'c'] } });
    add({ id: 'net-mlatq-label', type: 'symbol', source: 'net-mlatq', minzoom: 6, layout: {
      'text-field': ['case', ['has', 'g'], ['get', 'g'], ['has', 's'], ['concat', 'σ ', ['to-string', ['get', 's']], ' m'], ['concat', ['to-string', ['get', 'p50']], ' / ', ['coalesce', ['to-string', ['get', 'p95']], '–'], ' m']],
      'text-font': LABEL_FONT, 'text-size': 10, 'text-allow-overlap': false, 'text-optional': true },
    paint: { 'text-color': dark ? '#d8e4ff' : '#0b1b3a', 'text-halo-color': dark ? 'rgba(1,4,11,0.85)' : 'rgba(255,255,255,0.85)', 'text-halo-width': 1.2 } });
    add({ id: 'net-sigma-fill', type: 'fill', source: 'net-sigma', paint: { 'fill-color': ['get', 'c'], 'fill-opacity': 0.08 } });
    add({ id: 'net-sigma-line', type: 'line', source: 'net-sigma', paint: { 'line-color': ['get', 'c'], 'line-width': 1, 'line-opacity': 0.7, 'line-dasharray': [2, 2] } });
    add({ id: 'net-st-area', type: 'circle', source: 'net-stations', filter: ['==', ['get', 'fuzzy'], 1], paint: {
      'circle-radius': ['interpolate', ['linear'], ['zoom'], 4, 6, 9, 14], 'circle-color': '#a78bfa', 'circle-opacity': 0.14,
      'circle-stroke-color': '#a78bfa', 'circle-stroke-width': 1, 'circle-stroke-opacity': 0.6 } });
    add({ id: 'net-st', type: 'circle', source: 'net-stations', paint: {
      'circle-radius': ['case', ['==', ['get', 'sel'], 1], 6, 4.2],
      'circle-color': ['case', ['==', ['get', 'fuzzy'], 1], '#c4b5fd', ['==', ['get', 'online'], 1], '#ffffff', '#7388ad'],
      'circle-stroke-color': ['case', ['==', ['get', 'own'], 1], '#22d3ee', ['==', ['get', 'fuzzy'], 1], '#7c3aed', '#2f7dff'],
      'circle-stroke-width': ['case', ['==', ['get', 'sel'], 1], 3, 2] } });
    add({ id: 'net-st-label', type: 'symbol', source: 'net-stations', minzoom: 5, layout: {
      'text-field': ['get', 'name'], 'text-font': LABEL_FONT, 'text-size': 10.5, 'text-offset': [0, 1.1], 'text-anchor': 'top', 'text-optional': true },
    paint: { 'text-color': dark ? '#d8e4ff' : '#0b1b3a', 'text-halo-color': dark ? 'rgba(1,4,11,0.9)' : 'rgba(255,255,255,0.9)', 'text-halo-width': 1.5 } });
    // "heard by my stations": a cyan ring under the icon (aircraft source, property `mine`)
    add({ id: 'ac-mine', type: 'circle', source: 'aircraft', filter: ['==', ['get', 'mine'], 1], paint: {
      'circle-radius': ['interpolate', ['linear'], ['zoom'], 3, 8, 9, 15], 'circle-color': 'rgba(34,211,238,0.10)',
      'circle-stroke-color': '#22d3ee', 'circle-stroke-width': 1.4, 'circle-stroke-opacity': 0.9 } });
    applyVisibility();
    if (qualityMode() !== 'off') loadQuality(); // a refresh() before install found no map
  }

  function applyVisibility() {
    if (!map) return;
    const agg = net.aggregator;
    const set = (id, on) => { if (map.getLayer(id)) map.setLayoutProperty(id, 'visibility', on ? 'visible' : 'none'); };
    set('net-mlatq-fill', agg && qualityMode() !== 'off');
    set('net-mlatq-label', agg && qualityMode() !== 'off');
    set('net-sigma-fill', agg && prefs.get().sigma);
    set('net-sigma-line', agg && prefs.get().sigma);
    for (const id of ['net-st-area', 'net-st', 'net-st-label']) set(id, agg);
    set('ac-mine', agg && net.view === 'mine');
  }

  function setSource(id, data) {
    if (id === 'net-stations') stationsFc = data; else if (id === 'net-mlatq') qualityFc = data; else sigmaFc = data;
    const s = map && map.getSource(id);
    if (s) s.setData(data);
  }

  // ---- station markers ----
  const point = (s, props) => ({ type: 'Feature', properties: props, geometry: { type: 'Point', coordinates: [s.lon, s.lat] } });
  const exact = (s) => {
    const lat = Number.isFinite(s.lat) ? s.lat : s.location?.lat;
    const lon = Number.isFinite(s.lon) ? s.lon : s.location?.lon;
    return Number.isFinite(lat) && Number.isFinite(lon) ? { lat, lon } : null;
  };

  async function refresh() {
    const my = ++loadSeq;
    if (!net.aggregator) { setSource('net-stations', EMPTY); return; }
    let feats = [];
    try {
      if (net.view === 'network') {
        const r = await ctx.api.get(EP.publicStations(), { signal: ctx.signal, timeout: 15000 });
        for (const s of scrubPublic(normPublicStations(r))) {
          const p = publicPin(s);
          if (p) feats.push(point(p, { name: s.name || '', fuzzy: 1, online: s.online ? 1 : 0, own: 0, sel: 0 }));
        }
      } else if (net.view === 'mine' || net.view === 'station') {
        for (const s of net.stations.filter((x) => x.own)) {
          const p = exact(s);
          if (p && !(net.view === 'station' && net.station?.sid === s.sid)) feats.push(point(p, { name: s.name || shortSid(s.sid), fuzzy: 0, online: s.online ? 1 : 0, own: 1, sel: net.station?.sid === s.sid ? 1 : 0, sid: s.sid }));
        }
      } else if (net.view === 'admin') {
        for (const s of net.stations) {
          const p = exact(s);
          if (p) feats.push(point(p, { name: s.name || shortSid(s.sid), fuzzy: 0, online: s.online ? 1 : 0, own: s.own ? 1 : 0, sel: net.station?.sid === s.sid ? 1 : 0, sid: s.sid }));
        }
      }
    } catch (e) {
      if (e.aborted) return;
      feats = [];
    }
    if (my !== loadSeq) return;
    setSource('net-stations', fc(feats));
    renderBar();
    loadQuality();
  }

  // ---- MLAT quality layer ----
  /** The quality layers the current view may show ('measured' for everyone, 'expected' for the admin). */
  function qualityViews() {
    if (!net.aggregator) return [];
    return net.view === 'admin' && net.scope === 'admin' ? ['measured', 'expected', 'improve'] : ['measured'];
  }
  function qualityMode() {
    const m = prefs.get().mlatq;
    return qualityViews().includes(m) ? m : 'off';
  }
  function openQualityMenu() {
    const p = prefs.get();
    const mode = qualityMode();
    const pick = (patch) => { prefs.set(patch); renderBar(); applyVisibility(); loadQuality(); };
    const items = [{ label: 'Off', checked: mode === 'off', onClick: () => pick({ mlatq: 'off' }) }, 'sep',
      { label: 'Measured accuracy: best band', checked: mode === 'measured' && p.mlatqBand < 0, onClick: () => pick({ mlatq: 'measured', mlatqBand: -1 }) },
      ...QUALITY_BANDS.map((b) => ({ label: 'Measured accuracy: ' + b.label, checked: mode === 'measured' && p.mlatqBand === b.id, onClick: () => pick({ mlatq: 'measured', mlatqBand: b.id }) }))];
    if (qualityViews().includes('expected')) {
      items.push('sep', ...crlbAlts.map((a) => ({ label: 'Expected σ (CRLB) at ' + heightLabel(a), checked: mode === 'expected' && p.crlbAlt === a, onClick: () => pick({ mlatq: 'expected', crlbAlt: a }) })));
    }
    if (qualityViews().includes('improve')) {
      items.push('sep', ...crlbAlts.map((a) => ({ label: 'Where to add a station (' + heightLabel(a) + ')', checked: mode === 'improve' && p.crlbAlt === a, onClick: () => pick({ mlatq: 'improve', crlbAlt: a }) })));
    }
    ui.menu(qBtn, items, { placement: 'bottom-start' });
  }
  function legend(text, scale = QUALITY_SCALE) {
    if (!text) { qLegend.classList.add('hidden'); ui.clear(qLegend); return; }
    ui.mount(qLegend, h('span.mapv-mlatq-title', text), ...scale.map((s) => h('span', h('i', { style: { background: s.color } }), s.label)));
    qLegend.classList.remove('hidden');
  }
  async function loadQuality() {
    const my = ++qualitySeq;
    clearInterval(qualityTimer);
    qualityTimer = null;
    const mode = qualityMode();
    if (mode === 'off' || !map) { setSource('net-mlatq', EMPTY); legend(''); applyVisibility(); return; }
    const p = prefs.get();
    let data = EMPTY;
    let text = '';
    let failed = false;
    try {
      if (mode === 'improve') {
        // Where one more station helps MLAT most (admin): benefit per candidate cell, the best sites ranked.
        const r = await ctx.api.get(EP.adminCRLBImprove(), { params: { alt: p.crlbAlt }, signal: ctx.signal, timeout: 120000 });
        data = improveCellsFc(r);
        text = 'Benefit of one more GPS station at ' + heightLabel(r?.altM ?? p.crlbAlt) + (r?.sites?.length ? ' (#1 = best site)' : r?.reason ? ': ' + r.reason : '');
      } else if (mode === 'expected') {
        const r = await ctx.api.get('/api/admin/mlat/crlb', { signal: ctx.signal, timeout: 60000 });
        const hs = crlbHeights(r);
        if (hs.length) crlbAlts = hs;
        const alt = hs.includes(p.crlbAlt) ? p.crlbAlt : (hs[0] ?? p.crlbAlt);
        data = crlbCellsFc(r, alt);
        text = 'Expected σ (CRLB) at ' + heightLabel(alt);
      } else if (net.view === 'admin' && net.scope === 'admin') {
        const r = await ctx.api.get('/api/admin/mlat/quality', { params: { window: 60, cells: 1 }, signal: ctx.signal, timeout: 30000 });
        data = qualityCellsFc(r?.cells, p.mlatqBand);
        text = 'Measured error P50 / P95 (hold-out, last hour)';
      } else {
        const r = await ctx.api.get(EP.publicMLAT(), { signal: ctx.signal, timeout: 30000 });
        data = qualityCellsFc(r?.cells, p.mlatqBand);
        text = 'Measured error P50 / P95 (hold-out, last hour; cells with ≥ 5 stations)';
      }
    } catch (e) {
      if (e.aborted) return;
      data = EMPTY;
      failed = true;
      text = e.status === 403 ? 'MLAT quality is not public on this network' : 'MLAT quality could not be loaded';
    }
    if (my !== qualitySeq) return;
    setSource('net-mlatq', data);
    legend(failed || data.features.length || mode !== 'measured' ? text : text + ': no cells yet', mode === 'improve' && !failed ? IMPROVE_SCALE : QUALITY_SCALE);
    applyVisibility();
    if (!ctx.signal?.aborted) qualityTimer = setInterval(() => { if (!document.hidden) loadQuality(); }, 60000);
  }
  ctx.signal?.addEventListener('abort', () => clearInterval(qualityTimer));

  // ---- MLAT sigma circles ----
  function update(prepared) {
    if (!net.aggregator || !map || !prefs.get().sigma) { if (sigmaFc !== EMPTY) setSource('net-sigma', EMPTY); return; }
    const feats = [];
    for (const p of prepared.values()) {
      const a = p.a;
      const sig = a.mlatSigmaM;
      if (!Number.isFinite(sig) || sig < SIGMA_MIN_M || sig > SIGMA_MAX_M || p.props.stale) continue;
      const ring = geo.circle(a.lat, a.lon, sig / 1000, 32);
      feats.push({ type: 'Feature', properties: { c: GRADE_COLOR[a.mlatGrade] || MLAT_UNGRADED }, geometry: { type: 'Polygon', coordinates: [ring] } });
    }
    if (feats.length || sigmaFc.features.length) setSource('net-sigma', fc(feats));
  }

  return {
    el,
    install,
    update,
    refresh,
    renderBar,
    applyVisibility,
    /** Receiver-centred overlays must stay off in this view. */
    hidesReceiver: () => net.aggregator && viewHidesReceiver(net.view),
    /** Aircraft filter of the view ("only mine"). */
    filter: (a) => !(net.aggregator && net.view === 'mine' && prefs.get().onlyMine && !a.mine),
    get sigma() { return prefs.get().sigma; },
    setSigma(on) { prefs.set({ sigma: !!on }); applyVisibility(); },
  };
}
