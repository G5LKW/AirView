// pages/guess.js - Locate Me: estimate the station position from the received traffic alone and
// reveal it step by step on a map.
//
// Endpoints: POST /api/guess (run now on the newest data; admin) with GET /api/guess (cached result)
// as the fallback for locked clients and for the "is there enough data" readiness line;
// PUT /api/config through ctx.config.save for "use this as my station location";
// status.receiverGps (model.Status) for "use receiver GPS position".

import { createAdmin } from '../admin.js';
import { basemapById, styleFor, makeTransformRequest } from '../map/basemaps.js';

const METHODS = {
  centroid: {
    color: '#fbbf24', short: 'Centroid', icon: 'plane',
    how: 'Aircraft are heard in every direction around the antenna, so the centre of mass of all received positions (weighted by how often each map cell was hit) lies close to it. It is the quickest method and the easiest to fool: a busy airway on one side or a hill on the other pulls it sideways.',
    improve: 'Let it collect for a few days so that traffic from all directions evens out.',
  },
  signal: {
    color: '#22d3ee', short: 'Signal fit', icon: 'signal',
    how: 'Radio signals fade with the square of the distance. The estimator searches for the one point from which the average signal level of every map cell is best explained by that law - closer cells must be louder, farther ones weaker. The glowing overlay on the map is this search: bright means "the antenna could well be here".',
    improve: 'Needs a feed that reports signal levels (Beast / Radarcape). Receiver overload and strongly directional antennas bend the law and shift the result.',
  },
  floor: {
    color: '#a78bfa', short: 'Coverage floor', icon: 'globe',
    how: 'The curvature of the earth hides low aircraft that are far away: the farther out you look, the higher the lowest aircraft you can still hear. That "floor" forms a bowl whose lowest point sits exactly above the antenna. The estimator fits the theoretical radio-horizon bowl to the lowest altitude seen in every map cell.',
    improve: 'Needs low-flying traffic in several directions - nearby airports help a lot. It sharpens over days as rare low flights fill the floor in.',
  },
  symmetry: {
    color: '#2dd4bf', short: 'Coverage edge', icon: 'radar',
    how: 'With a free view the outer edge of the coverage is roughly a circle around the antenna (the radio horizon for high-flying traffic). The estimator fits a circle through the farthest positions in every direction and takes its centre.',
    improve: 'Needs coverage in most directions out to the horizon. Obstructed sectors make the edge lopsided and push the centre away from them.',
  },
  locate2: {
    color: '#f97316', short: 'v2', icon: 'radar',
    how: 'Locate Me v2 scores every candidate position on a fine grid against three things it can predict: where low aircraft appear and vanish at the radio horizon and behind the local skyline (per 10-degree sector, with a terrain panorama when elevation tiles are cached), how the signal level of each aircraft pass falls with distance, and - for a GPS-stamping receiver - the timing of T=1 positions. The solid circle around its estimate holds the station with about 95 % probability.',
    improve: 'An hour of traffic with signal levels (Beast / Radarcape) is usually enough; low traffic near airports and cached elevation tiles sharpen it. Feeds without signal levels (SBS) are much less precise.',
  },
  fused: {
    color: '#ffffff', short: 'Fused', icon: 'target',
    how: 'The estimates are combined into one, each weighted by how sharply it pins the position down (inverse variance, 1/σ²). Once Locate Me v2 has enough traffic its σ is a few km against the tens of km of the classic methods, so it carries well over 90 % of the weight and the classic methods only nudge the result; a classic method that disagrees with v2 by more than 3 combined σ is left out (0 %). The 95 % circle stays that of v2: the classic methods see the same traffic, so they cannot make it smaller.',
    improve: 'More data, from more directions and altitudes. The configured station position is never used as an input - only to show how far off each estimate is.',
  },
};
const ORDER = ['centroid', 'signal', 'floor', 'symmetry', 'locate2'];
const CONFIGURED_COLOR = '#34d399';
const GPS_COLOR = '#f472b6';
const READY_POLL_MS = 60000;

const isNum = (v) => typeof v === 'number' && Number.isFinite(v);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const easeOut = (t) => 1 - (1 - t) ** 3;
const fc = (features = []) => ({ type: 'FeatureCollection', features });

export default {
  id: 'guess',
  title: 'Locate Me',

  async mount(container, ctx) {
    const { api, units, ui, config, geo } = ctx;
    const { h } = ui;
    const admin = createAdmin(ctx);
    await Promise.all([
      ui.loadCss(new URL('../../css/guess.css', import.meta.url)),
      ui.loadCss(new URL('../../vendor/maplibre/maplibre-gl.css', import.meta.url)),
    ]);
    const reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

    const S = {
      alive: true, phase: 'idle', // idle | running | revealing | done | nodata | error
      guess: null, peek: null, map: null, maplibre: null, mapOk: false, mapReady: null,
      markers: [], run: 0, shown: new Set(), cached: false,
    };
    ctx.onCleanup(() => {
      S.alive = false;
      S.run++;
      for (const m of S.markers) m.remove();
      if (S.map) { try { S.map.remove(); } catch { /* already gone */ } S.map = null; }
    });
    /** Wait `ms` unless the run was superseded or motion is reduced; false = cancelled. */
    const pause = async (ms, run) => { if (!reduced) await sleep(ms); return S.alive && run === S.run; };

    // ---- DOM ------------------------------------------------------------------------------------
    const mapEl = h('div.gm-map');
    const readyEl = h('div.gm-ready', 'Checking how much traffic has been collected…');
    const goBtn = h('button.gm-go', { type: 'button', onclick: () => start() },
      h('span.gm-go-rings', h('i'), h('i'), h('i')), h('span.gm-go-sweep'),
      h('span.gm-go-face', ui.icon('target'), h('b', 'Locate me'), h('small', 'guess my coordinates')));
    const launch = h('div.gm-launch',
      goBtn,
      h('p.gm-pitch', 'AirView never asked where you are - but every aircraft it hears gives your position away a little. Press the button and four independent methods will work out where this antenna must be standing.'),
      readyEl);
    const stepEl = h('div.gm-step.hidden');
    const againBtn = ui.button({ label: 'Run again', icon: 'refresh', size: 'sm', class: 'gm-again hidden', onClick: () => start() });
    const legendEl = h('div.gm-legend.hidden');
    const stage = h('div.gm-stage', mapEl, h('div.gm-vignette'), launch, stepEl, againBtn, legendEl);

    const resultBody = h('div.gm-result');
    const resultCard = ui.card({ title: 'The verdict', icon: 'target', body: resultBody, class: 'card--accent gm-resultcard' });
    const tableHost = h('div.gm-tablehost');
    const tableCard = ui.card({ title: 'Methods and their estimates', icon: 'list', sub: 'Four independent estimators and their fusion', body: tableHost, flush: false });
    const howHost = h('div.gm-how');
    const howCard = ui.card({ title: 'How it works', icon: 'help', sub: 'What each method looks at, how much data it had and how to make it better', body: howHost });
    const notesEl = h('div.gm-notes');

    container.append(h('div.page-inner.gm',
      h('div.page-head', h('div', h('h2', 'Locate Me'), h('div.sub', 'Where is this antenna? Estimated from nothing but the aircraft it hears.'))),
      h('div.gm-top', stage, resultCard),
      h('div.gm-bottom', tableCard, howCard),
      notesEl));

    // ---- map ------------------------------------------------------------------------------------
    S.mapReady = (async () => {
      try {
        const maplibregl = await import('../../vendor/maplibre/maplibre-gl.mjs');
        if (!S.alive) return false;
        S.maplibre = maplibregl;
        const basemap = basemapById(config.current?.ui?.mapStyle || 'airview-dark');
        const map = new maplibregl.Map({
          container: mapEl, style: styleFor(basemap), center: [10, 28], zoom: 1.1, minZoom: 0.8, maxZoom: 14,
          dragRotate: false, pitchWithRotate: false, touchPitch: false, attributionControl: false, fadeDuration: 0, cooperativeGestures: true,
          transformRequest: makeTransformRequest(() => !!ctx.live.status?.tileCache),
        });
        S.map = map;
        map.touchZoomRotate.disableRotation();
        map.addControl(new maplibregl.AttributionControl({ compact: true }), 'bottom-right');
        map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'bottom-right');
        let warned = 0;
        map.on('error', (ev) => { if (warned++ < 2) console.warn('[guess map]', ev?.error?.message || ev); });
        // No timeout here: a map in a background tab only loads once the tab is shown again.
        await new Promise((resolve) => { map.once('load', resolve); });
        if (!S.alive) return false;
        map.addSource('gm-surface', { type: 'geojson', data: fc(), tolerance: 0 });
        map.addSource('gm-unc', { type: 'geojson', data: fc() });
        map.addSource('gm-line', { type: 'geojson', data: fc() });
        map.addLayer({ id: 'gm-surface', type: 'fill', source: 'gm-surface', paint: {
          'fill-color': ['interpolate', ['linear'], ['get', 'c'], 0, '#eaffff', 0.06, '#67e8f9', 0.2, '#2f7dff', 0.5, '#1a3fa8', 1, '#06122e'],
          'fill-opacity': 0, 'fill-antialias': false,
        } });
        map.addLayer({ id: 'gm-unc-fill', type: 'fill', source: 'gm-unc', filter: ['!=', ['get', 'm'], 'fused95'], paint: { 'fill-color': ['get', 'color'], 'fill-opacity': ['case', ['==', ['get', 'm'], 'fused'], 0.13, 0.045] } });
        // 1-sigma circles dashed; the 95 % accuracy circle of the result (fused.r95Km) solid.
        map.addLayer({ id: 'gm-unc-line', type: 'line', source: 'gm-unc', filter: ['!=', ['get', 'm'], 'fused95'], paint: { 'line-color': ['get', 'color'], 'line-width': 1.2, 'line-opacity': 0.9, 'line-dasharray': [3, 2] } });
        map.addLayer({ id: 'gm-unc-line-fused', type: 'line', source: 'gm-unc', filter: ['==', ['get', 'm'], 'fused95'], paint: { 'line-color': ['get', 'color'], 'line-width': 2.2, 'line-opacity': 0.95 } });
        map.addLayer({ id: 'gm-line', type: 'line', source: 'gm-line', paint: { 'line-color': CONFIGURED_COLOR, 'line-width': 1.6, 'line-dasharray': [2, 2] } });
        S.mapOk = true;
        if (S.phase === 'done') redrawDone(true); // the estimate finished before the map did
        return true;
      } catch (e) {
        console.error('[guess] map unavailable', e);
        if (S.alive) mapEl.replaceChildren(h('div.gm-nomap', ui.icon('map'), h('span', 'The map could not be started here (' + (e?.message || 'WebGL unavailable') + '). The estimate still works - see the table below.')));
        return false;
      }
    })();

    function surfaceGeoJson(sf) {
      if (!sf || !Array.isArray(sf.cost)) return fc();
      const feats = [];
      const hl = sf.dLat / 2;
      const hw = sf.dLon / 2;
      for (let r = 0; r < sf.rows; r++) {
        for (let c = 0; c < sf.cols; c++) {
          const cost = sf.cost[r * sf.cols + c];
          if (!isNum(cost)) continue;
          const la = sf.south + r * sf.dLat;
          const lo = sf.west + c * sf.dLon;
          feats.push({ type: 'Feature', properties: { c: cost }, geometry: { type: 'Polygon', coordinates: [[[lo - hw, la - hl], [lo + hw, la - hl], [lo + hw, la + hl], [lo - hw, la + hl], [lo - hw, la - hl]]] } });
        }
      }
      return fc(feats);
    }
    const surfaceOpacity = (f) => ['*', f, ['interpolate', ['linear'], ['get', 'c'], 0, 0.78, 0.15, 0.55, 0.5, 0.3, 1, 0.05]];
    async function animate(ms, run, fn) {
      if (reduced) { fn(1); return S.alive && run === S.run; }
      const t0 = performance.now();
      return new Promise((resolve) => {
        let done = false;
        const end = (ok) => { if (done) return; done = true; clearTimeout(guard); if (ok) fn(1); resolve(ok); };
        // requestAnimationFrame stops in a background tab: finish the step anyway so the reveal never hangs
        const guard = setTimeout(() => end(S.alive && run === S.run), ms + 1200);
        const tick = (now) => {
          if (done) return;
          if (!S.alive || run !== S.run) { end(false); return; }
          const t = Math.min(1, (now - t0) / ms);
          if (t < 1) { fn(easeOut(t)); requestAnimationFrame(tick); } else end(true);
        };
        requestAnimationFrame(tick);
      });
    }
    const uncFeatures = new Map(); // method -> { est, f (0..1) }
    function pushUnc() {
      if (!S.mapOk) return;
      const feats = [];
      for (const [m, u] of uncFeatures) {
        const km = (u.est.uncertaintyKm || 0) * u.f;
        if (km < 0.05) continue;
        feats.push({ type: 'Feature', properties: { m, color: m === 'fused' ? '#8cc2ff' : METHODS[m]?.color || '#fff' }, geometry: { type: 'Polygon', coordinates: [geo.circle(u.est.lat, u.est.lon, km, 96)] } });
        if (m === 'fused' && isNum(u.est.r95Km) && u.est.r95Km * u.f >= 0.05) {
          feats.push({ type: 'Feature', properties: { m: 'fused95', color: '#8cc2ff' }, geometry: { type: 'Polygon', coordinates: [geo.circle(u.est.lat, u.est.lon, u.est.r95Km * u.f, 96)] } });
        }
      }
      S.map.getSource('gm-unc').setData(fc(feats));
    }
    function addMarker(kind, lat, lon, label, sub) {
      if (!S.mapOk) return null;
      const el = h('div.gm-pin.gm-pin--' + kind, { style: { '--c': kind === 'configured' ? CONFIGURED_COLOR : kind === 'gps' ? GPS_COLOR : METHODS[kind]?.color || '#fff' } },
        h('span.gm-pin-dot'), h('span.gm-pin-label', h('b', label), sub ? h('small', sub) : null));
      const mk = new S.maplibre.Marker({ element: el, anchor: 'center' }).setLngLat([lon, lat]).addTo(S.map);
      S.markers.push(mk);
      return mk;
    }
    function clearMap() {
      for (const m of S.markers) m.remove();
      S.markers = [];
      uncFeatures.clear();
      if (!S.mapOk) return;
      S.map.getSource('gm-unc').setData(fc());
      S.map.getSource('gm-line').setData(fc());
      S.map.getSource('gm-surface').setData(fc());
      S.map.setPaintProperty('gm-surface', 'fill-opacity', 0);
    }
    function boundsOf(g) {
      let w = Infinity; let s = Infinity; let e = -Infinity; let n = -Infinity;
      const add = (lon, lat) => { w = Math.min(w, lon); e = Math.max(e, lon); s = Math.min(s, lat); n = Math.max(n, lat); };
      for (const est of [...(g.estimates || []), g.fused].filter((x) => x && x.ok)) {
        for (const p of geo.circle(est.lat, est.lon, Math.max(5, est.uncertaintyKm || 5), 16)) add(p[0], p[1]);
      }
      if (g.configured) add(g.configured.lon, g.configured.lat);
      return Number.isFinite(w) ? [[w, s], [e, n]] : null;
    }
    const mapPad = () => (stage.clientWidth < 560 ? 36 : 80);
    async function flyTo(bounds, run, ms = 2200) {
      if (!S.mapOk || !bounds) return S.alive && run === S.run;
      S.map.fitBounds(bounds, { padding: mapPad(), maxZoom: 11, duration: reduced ? 0 : ms, essential: true });
      return pause(ms + 100, run);
    }

    // ---- readiness ------------------------------------------------------------------------------
    function describeData(g) {
      return `${units.int(g.positions)} positions in ${units.int(g.cells)} map cells`;
    }
    function renderReady() {
      const g = S.peek;
      if (S.phase !== 'idle') return;
      if (!g) { ui.setText(readyEl, 'Checking how much traffic has been collected…'); readyEl.className = 'gm-ready'; return; }
      if (g.error) { ui.mount(readyEl, ui.icon('alert'), ` Could not check the data: ${g.error}`); readyEl.className = 'gm-ready warn'; return; }
      if (g.ok || g.cells >= 12) {
        ui.mount(readyEl, ui.icon('check-circle'), ` Ready: ${describeData(g)} collected so far.`, ctx.status?.demo ? h('span.gm-demo', ' Demo mode - the traffic is simulated around a pretend station.') : null);
        readyEl.className = 'gm-ready ok';
      } else {
        ui.mount(readyEl, ui.icon('info'), ` Still collecting: ${describeData(g)} so far. The estimators need positions in at least 12 map cells - with live traffic that takes a few minutes. You can try anyway.`);
        readyEl.className = 'gm-ready warn';
      }
    }
    async function peek() {
      if (S.phase !== 'idle' || document.visibilityState !== 'visible') return;
      try {
        const g = await api.get('/api/guess', { signal: ctx.signal, timeout: 30000 });
        if (S.alive) S.peek = g;
      } catch (e) {
        if (e.aborted || !S.alive) return;
        S.peek = { error: e.message || String(e) };
      }
      renderReady();
    }

    // ---- result card ----------------------------------------------------------------------------
    function verdictFor(g) {
      const f = g.fused;
      if (isNum(f.errorKm)) {
        const e = f.errorKm;
        if (e < 2) return ['Bullseye!', 'green', 'That is practically on top of the configured position.'];
        if (e < 10) return ['Very close', 'green', 'Good enough to find the right part of town.'];
        if (e < 30) return ['Right neighbourhood', 'cyan', 'Close - a few more days of traffic usually tighten this.'];
        if (e < 100) return ['Right region', 'amber', 'The methods have the area but not yet the spot; they need more traffic from more directions.'];
        return ['Still guessing', 'red', 'Too little or too one-sided data so far - let the station run and try again.'];
      }
      const u = f.uncertaintyKm;
      if (!isNum(u)) return ['Estimate ready', 'blue', ''];
      if (u < 5) return ['Pinned down', 'green', 'The methods agree closely.'];
      if (u < 20) return ['Fairly sure', 'cyan', 'A solid estimate; more data will sharpen it.'];
      if (u < 60) return ['Rough fix', 'amber', 'Right area, wide margin - keep collecting.'];
      return ['Very rough', 'red', 'Treat this as a hint only; the data is still thin.'];
    }
    function renderIdleResult() {
      ui.mount(resultBody, ui.empty({ icon: 'target', title: 'No guess yet', text: 'Press "Locate me" on the map. The result, its uncertainty and - if a station position is configured - how far off it is will appear here.', compact: true }), gpsBlock());
    }
    function gpsBlock() {
      const gps = ctx.status?.receiverGps;
      if (!gps || !isNum(gps.lat) || !isNum(gps.lon)) return null;
      return h('div.gm-gps',
        h('div.gm-gps-text', ui.icon('pin'), h('div',
          h('b', 'The receiver knows where it is'),
          h('span', `Its own GPS reports ${units.latlon(gps.lat, gps.lon, 5)}${isNum(gps.altM) ? ', ' + units.height(gps.altM, { digits: 0 }) : ''} (${units.ago(gps.t, ctx.live.serverNow())}). That beats any guess.`))),
        ui.button({ label: 'Use receiver GPS position', icon: 'pin', size: 'sm', onClick: () => applyLocation(gps.lat, gps.lon, 'the receiver\'s own GPS position', 'gps') }));
    }
    function renderResult(final = true) {
      const g = S.guess;
      if (!g || !g.ok || !g.fused) return;
      const f = g.fused;
      const [title, kind, text] = verdictFor(g);
      const coord = h('div.gm-coord.num', units.latlon(f.lat, f.lon, 4));
      const rows = [
        [isNum(f.r95Km) ? 'Uncertainty (1σ)' : 'Uncertainty (1σ, heuristic)', isNum(f.uncertaintyKm) ? '± ' + units.dist(f.uncertaintyKm) : '—', { tip: isNum(f.r95Km) ? 'Calibrated 1-sigma radius of Locate Me v2, including an allowance for model errors on a real site (buildings, trees, refraction, the antenna)' : 'How sharply the methods pin the position down - not a guaranteed error bound' }],
        ...(isNum(f.r95Km) ? [['Accuracy circle (95 %)', units.dist(f.r95Km), { tip: 'The station lies inside this circle (solid on the map) with about 95 % probability (Locate Me v2, calibrated)' }]] : []),
        ['Maidenhead locator', g.maidenhead || '—'],
        ['Decimal degrees', g.formatted || `${f.lat.toFixed(4)}, ${f.lon.toFixed(4)}`],
        ['Based on', describeData(g), { text: true }],
      ];
      const l2 = g.locate2;
      if (l2 && isNum(l2.errorKm)) {
        const ref = { gnss: 'receiver GPS', gps: 'receiver GPS', config: 'configured position', configured: 'configured position', survey: 'survey' }[l2.refSource] || l2.refSource || 'reference';
        rows.push(['Locate Me v2 off by', `${units.dist(l2.errorKm)} (vs ${ref})`, { tip: 'Distance between the Locate Me v2 estimate and the reference position' }]);
      }
      if (g.configured) {
        rows.push(['Configured position', units.latlon(g.configured.lat, g.configured.lon, 4)]);
        rows.push(['Off by', isNum(f.errorKm) ? units.dist(f.errorKm) + (isNum(f.uncertaintyKm) && f.uncertaintyKm > 0 ? ` (${units.num(f.errorKm / f.uncertaintyKm, 1)} σ)` : '') : '—', { tip: 'Distance between the fused estimate and the configured station position' }]);
      }
      const useBtn = ui.button({ label: 'Use this as my station location', icon: 'check', variant: 'primary', onClick: () => applyLocation(f.lat, f.lon, 'the fused estimate', 'guess') });
      const copyBtn = ui.button({ label: 'Copy', icon: 'copy', variant: 'ghost', tip: 'Copy the coordinates', onClick: async () => { const ok = await ui.copy(g.formatted || `${f.lat.toFixed(5)}, ${f.lon.toFixed(5)}`); if (ok !== false) ui.toast.success('Coordinates copied'); } });
      ui.mount(resultBody,
        h('div.gm-verdict', ui.badge(title, kind, { large: true }), S.cached ? ui.badge('cached result', 'other', { tip: 'This client is not logged in as admin, so the last cached estimate (up to a minute old) is shown instead of a fresh run' }) : null),
        coord,
        text ? h('p.gm-verdict-text', text) : null,
        ui.kv(rows),
        g.configured ? null : h('p.gm-hint', 'No station position is configured yet, so there is nothing to compare with. If the estimate looks plausible on the map you can adopt it - or better, enter the exact coordinates in Settings.'),
        h('div.gm-actions', useBtn, copyBtn),
        h('p.gm-hint', isNum(f.uncertaintyKm) && f.uncertaintyKm > 10
          ? `Mind the ± ${units.dist(f.uncertaintyKm)}: for range rings and coverage analysis an estimate this rough will skew the results. Exact coordinates (or the receiver's GPS) are always better.`
          : 'An estimate from traffic is never as good as real coordinates - use it to get started, replace it when you can.'),
        gpsBlock());
      if (final || reduced) return;
      // slot-machine reveal of the coordinates
      const t0 = performance.now();
      const run = S.run;
      const spin = (now) => {
        if (!S.alive || run !== S.run || !coord.isConnected) return;
        const t = Math.min(1, (now - t0) / 1400);
        const k = (1 - easeOut(t)) * 4;
        ui.setText(coord, units.latlon(f.lat + (Math.random() - 0.5) * k, f.lon + (Math.random() - 0.5) * k, 4));
        if (t < 1) requestAnimationFrame(spin); else ui.setText(coord, units.latlon(f.lat, f.lon, 4));
      };
      requestAnimationFrame(spin);
    }

    // ---- table + explanations -------------------------------------------------------------------
    function estimatesOf(g) {
      const by = new Map((g?.estimates || []).map((e) => [e.method, e]));
      const list = ORDER.map((m) => by.get(m)).filter(Boolean);
      for (const e of g?.estimates || []) if (!ORDER.includes(e.method)) list.push(e);
      return list;
    }
    function renderTable() {
      const g = S.guess;
      if (!g || !g.ok) {
        ui.mount(tableHost, ui.empty({ icon: 'list', title: 'Nothing to compare yet', text: 'Each method\'s estimate, its uncertainty and its share in the fused result will be listed here after a run.', compact: true }));
        return;
      }
      const hasCfg = !!g.configured;
      const rows = [...estimatesOf(g), g.fused].filter(Boolean);
      const visible = (r) => S.phase === 'done' || S.shown.has(r.method);
      const cols = [
        { key: 'label', label: 'Method', render: (r) => h('span.gm-method' + (r.method === 'fused' ? '.fused' : ''), h('i', { style: { '--c': METHODS[r.method]?.color || '#fff' } }), r.label || METHODS[r.method]?.short || r.method) },
        { key: 'pos', label: 'Estimate', num: true, render: (r) => (!visible(r) ? '· · ·' : r.ok ? units.latlon(r.lat, r.lon, 4) : h('span.muted', 'no estimate')) },
        { key: 'uncertaintyKm', label: 'Uncertainty', align: 'right', num: true, render: (r) => (visible(r) && r.ok && isNum(r.uncertaintyKm) ? '± ' + units.dist(r.uncertaintyKm) : '—'), tip: 'Heuristic 1-sigma radius - drawn as the circle on the map' },
      ];
      if (hasCfg) cols.push({ key: 'errorKm', label: 'Off by', align: 'right', num: true, render: (r) => (visible(r) && r.ok ? units.dist(r.errorKm) : '—'), tip: 'Distance to the configured station position' });
      cols.push(
        { key: 'weight', label: 'Weight', align: 'right', render: (r) => {
          if (!visible(r) || !r.ok || !isNum(r.weight) || r.method === 'fused') return h('span.muted', r.method === 'fused' ? 'result' : '—');
          return h('div.gm-weight', h('span.num', units.pct(r.weight * 100, r.weight > 0 && r.weight < 0.1 ? 1 : 0)), ui.meter(r.weight * 100));
        }, tip: 'Share of this method in the fused estimate (inverse variance, 1/σ²). With Locate Me v2 available it dominates because its σ is much smaller; 0 % = left out because it disagrees with the others by more than its uncertainty allows' },
        { key: 'detail', label: 'Data used', render: (r) => h('span.gm-detail', !visible(r) ? '' : r.ok ? r.detail || '' : r.reason || r.detail || 'not available') });
      ui.mount(tableHost, ui.table({
        columns: cols, rows,
        rowClass: (r) => (r.method === 'fused' ? 'gm-row-fused' : '') + (visible(r) ? '' : ' gm-row-pending'),
        onRowClick: (r) => { if (r.ok && S.mapOk && visible(r)) S.map.flyTo({ center: [r.lon, r.lat], zoom: Math.max(S.map.getZoom(), 7.5), duration: reduced ? 0 : 900 }); },
      }));
    }
    function renderHow() {
      const g = S.guess;
      const by = new Map((g?.estimates || []).map((e) => [e.method, e]));
      if (g?.fused) by.set('fused', g.fused);
      const done = S.phase === 'done';
      ui.mount(howHost, [...ORDER, 'fused'].map((m) => {
        const meta = METHODS[m];
        const e = done ? by.get(m) : null;
        let data = null;
        if (e) {
          data = e.ok
            ? h('div.gm-how-data', h('span.label', 'This run'), h('span', `${e.detail || 'estimate available'}${isNum(e.uncertaintyKm) ? ' · ± ' + units.dist(e.uncertaintyKm) : ''}${m !== 'fused' && isNum(e.weight) ? ' · weight ' + units.pct(e.weight * 100, 0) : ''}`))
            : h('div.gm-how-data.fail', h('span.label', 'This run'), h('span', `No estimate: ${e.reason || 'not enough data'}.`));
        }
        return h('div.gm-how-item', { style: { '--c': meta.color } },
          h('div.gm-how-head', h('span.gm-how-icon', ui.icon(meta.icon)), h('h4', e?.label || { centroid: 'Centroid of all positions', signal: 'Signal-strength fit', floor: 'Coverage-floor (radio horizon) fit', symmetry: 'Centre of the coverage edge', locate2: 'Locate Me v2 (horizon, skyline, signal)', fused: 'Fused estimate' }[m])),
          h('p', meta.how),
          data,
          h('div.gm-how-improve', ui.icon('bolt'), h('span', meta.improve)));
      }));
      const notes = done ? g?.notes || [] : [];
      ui.mount(notesEl, notes.map((t) => h('p.gm-note', ui.icon('info'), ' ', t)));
    }
    function renderLegend() {
      const g = S.guess;
      if (!g?.ok) { legendEl.classList.add('hidden'); return; }
      const items = estimatesOf(g).filter((e) => e.ok).map((e) => ({ label: METHODS[e.method]?.short || e.method, color: METHODS[e.method]?.color || '#fff' }));
      items.push({ label: 'Fused', color: '#8cc2ff' });
      if (g.configured) items.push({ label: 'Configured', color: CONFIGURED_COLOR });
      ui.mount(legendEl, ui.legend(items), g.surface ? h('span.gm-legend-note', `Glow: where the ${METHODS[g.surface.method]?.short.toLowerCase() || g.surface.method} method thinks the antenna can be`) : null);
      legendEl.classList.remove('hidden');
    }
    const step = (text) => { if (text) { ui.setText(stepEl, text); stepEl.classList.remove('hidden'); } else stepEl.classList.add('hidden'); };

    // ---- run + reveal ---------------------------------------------------------------------------
    async function fetchGuess() {
      // A fresh run is an admin action: a locked client is offered the login (and the call retried);
      // cancelling it falls back to the public cached estimate (up to a minute old).
      try {
        const g = await admin.run(() => api.post('/api/guess', undefined, { signal: ctx.signal, timeout: 90000 }),
          'A fresh location estimate runs the analysis on the newest data and needs the admin password. Cancel to see the last cached estimate instead.');
        return { g, cached: false };
      } catch (e) {
        if (!e.cancelled && e.status !== 401 && e.status !== 403) throw e;
      }
      return { g: await api.get('/api/guess', { signal: ctx.signal, timeout: 60000 }), cached: true };
    }

    async function start() {
      if (S.phase === 'running' || S.phase === 'revealing') return;
      const run = ++S.run;
      S.phase = 'running';
      S.shown.clear();
      stage.classList.add('running');
      stage.classList.remove('revealed', 'failed');
      launch.classList.remove('gm-off');
      againBtn.classList.add('hidden');
      legendEl.classList.add('hidden');
      goBtn.disabled = true;
      clearMap();
      ui.setText(readyEl, 'Listening to the traffic and crunching the numbers…');
      readyEl.className = 'gm-ready';
      step(null);
      let res;
      try {
        // give the map a moment to come up, but never let it hold the estimate back
        [res] = await Promise.all([fetchGuess(), reduced ? null : sleep(1600), Promise.race([S.mapReady, sleep(6000)])]);
      } catch (e) {
        if (e.aborted || !S.alive || run !== S.run) return;
        S.phase = 'error';
        stage.classList.remove('running');
        stage.classList.add('failed');
        goBtn.disabled = false;
        ui.mount(readyEl, ui.icon('alert'), ` The estimator could not be run: ${e.message || e}. Try again in a moment.`);
        readyEl.className = 'gm-ready warn';
        return;
      }
      if (!S.alive || run !== S.run) return;
      const g = res.g;
      S.guess = g;
      S.cached = res.cached;
      S.peek = g;
      goBtn.disabled = false;
      stage.classList.remove('running');
      if (!g || !g.ok || !g.fused) {
        S.phase = 'nodata';
        stage.classList.add('failed');
        const reason = g?.reason || 'no estimate could be made';
        ui.mount(readyEl, ui.icon('info'),
          h('span', ` Not enough data yet: ${reason.replace(/^not enough data yet:\s*/i, '')}. `),
          h('span', `So far there are ${describeData(g || { positions: 0, cells: 0 })}. Positions are written to the statistics every few minutes; with live traffic around you a first guess is possible after about ten minutes, a good one after a day or two.`));
        readyEl.className = 'gm-ready warn';
        ui.mount(resultBody, ui.empty({ icon: 'radar', title: 'Not enough data yet', text: `${reason}. Keep the station running and try again a little later.`, compact: true }), gpsBlock());
        renderTable(); renderHow();
        return;
      }
      await reveal(g, run);
    }

    async function reveal(g, run) {
      S.phase = 'revealing';
      launch.classList.add('gm-off');
      stage.classList.add('revealed');
      ui.mount(resultBody, ui.loading('Working it out…', { compact: true }));
      renderTable(); renderHow(); renderLegend();
      const ests = estimatesOf(g).filter((e) => e.ok).sort((a, b) => (b.uncertaintyKm || 0) - (a.uncertaintyKm || 0));

      // 1. fly from the whole world to the search area
      step(`Searching… ${describeData(g)} to work with`);
      if (!(await flyTo(boundsOf(g), run))) return;

      // 2. cost surface
      if (S.mapOk && g.surface) {
        step(`Scanning the area: where would the ${METHODS[g.surface.method]?.short.toLowerCase() || g.surface.method} method put the antenna?`);
        S.map.getSource('gm-surface').setData(surfaceGeoJson(g.surface));
        if (!(await animate(1100, run, (t) => S.map.setPaintProperty('gm-surface', 'fill-opacity', surfaceOpacity(t))))) return;
        if (!(await pause(500, run))) return;
      }

      // 3. the methods, roughest first
      for (let i = 0; i < ests.length; i++) {
        const e = ests[i];
        const meta = METHODS[e.method] || { short: e.method };
        step(`Method ${i + 1} of ${ests.length} · ${e.label || meta.short}: ± ${units.dist(e.uncertaintyKm)}`);
        addMarker(e.method, e.lat, e.lon, meta.short, isNum(e.uncertaintyKm) ? '± ' + units.dist(e.uncertaintyKm) : null);
        uncFeatures.set(e.method, { est: e, f: 0 });
        S.shown.add(e.method);
        renderTable();
        if (!(await animate(750, run, (t) => { uncFeatures.get(e.method).f = t; pushUnc(); }))) return;
        if (!(await pause(650, run))) return;
      }
      for (const e of estimatesOf(g).filter((x) => !x.ok)) S.shown.add(e.method);

      // 4. the fused estimate
      step('Combining the methods…');
      if (!(await pause(500, run))) return;
      const f = g.fused;
      if (S.mapOk) {
        const ring = geo.circle(f.lat, f.lon, Math.max(8, (f.uncertaintyKm || 8) * 1.8), 24);
        let w = Infinity; let s = Infinity; let e2 = -Infinity; let n = -Infinity;
        for (const p of ring) { w = Math.min(w, p[0]); e2 = Math.max(e2, p[0]); s = Math.min(s, p[1]); n = Math.max(n, p[1]); }
        if (g.configured) { w = Math.min(w, g.configured.lon); e2 = Math.max(e2, g.configured.lon); s = Math.min(s, g.configured.lat); n = Math.max(n, g.configured.lat); }
        S.map.fitBounds([[w, s], [e2, n]], { padding: mapPad(), maxZoom: 12, duration: reduced ? 0 : 1800, essential: true });
      }
      addMarker('fused', f.lat, f.lon, 'You are here?', units.latlon(f.lat, f.lon, 3));
      uncFeatures.set('fused', { est: f, f: 0 });
      S.shown.add('fused');
      renderTable();
      renderResult(false);
      step(`Fused estimate: ${units.latlon(f.lat, f.lon, 4)} ± ${units.dist(f.uncertaintyKm)}`);
      if (!(await animate(900, run, (t) => { uncFeatures.get('fused').f = t; pushUnc(); }))) return;
      if (!(await pause(900, run))) return;

      // 5. the truth, if known
      if (g.configured) {
        addMarker('configured', g.configured.lat, g.configured.lon, 'Configured', isNum(f.errorKm) ? units.dist(f.errorKm) + ' away' : null);
        if (S.mapOk) S.map.getSource('gm-line').setData(fc([{ type: 'Feature', properties: {}, geometry: { type: 'LineString', coordinates: [[f.lon, f.lat], [g.configured.lon, g.configured.lat]] } }]));
        step(`Configured station position: ${isNum(f.errorKm) ? 'the guess is off by ' + units.dist(f.errorKm) : 'shown in green'}`);
      }
      const gps = ctx.status?.receiverGps;
      if (gps && isNum(gps.lat) && isNum(gps.lon)) addMarker('gps', gps.lat, gps.lon, 'Receiver GPS', null);
      finish();
    }

    function finish() {
      S.phase = 'done';
      againBtn.classList.remove('hidden');
      renderResult(true);
      renderTable(); renderHow(); renderLegend();
    }

    /** Redraw the finished state without animation (unit change, map that became ready late). */
    function redrawDone(fit = false) {
      const g = S.guess;
      if (S.phase !== 'done' || !g?.ok) return;
      for (const m of S.markers) m.remove();
      S.markers = [];
      if (S.mapOk) {
        uncFeatures.clear();
        for (const e of [...estimatesOf(g).filter((x) => x.ok), g.fused]) uncFeatures.set(e.method, { est: e, f: 1 });
        pushUnc();
        S.map.getSource('gm-surface').setData(surfaceGeoJson(g.surface));
        S.map.setPaintProperty('gm-surface', 'fill-opacity', surfaceOpacity(1));
        S.map.getSource('gm-line').setData(fc(g.configured ? [{ type: 'Feature', properties: {}, geometry: { type: 'LineString', coordinates: [[g.fused.lon, g.fused.lat], [g.configured.lon, g.configured.lat]] } }] : []));
        if (fit) { const b = boundsOf(g); if (b) S.map.fitBounds(b, { padding: mapPad(), maxZoom: 11, duration: 0 }); }
      }
      for (const e of estimatesOf(g).filter((x) => x.ok)) addMarker(e.method, e.lat, e.lon, METHODS[e.method]?.short || e.method, isNum(e.uncertaintyKm) ? '± ' + units.dist(e.uncertaintyKm) : null);
      addMarker('fused', g.fused.lat, g.fused.lon, 'You are here?', units.latlon(g.fused.lat, g.fused.lon, 3));
      if (g.configured) addMarker('configured', g.configured.lat, g.configured.lon, 'Configured', isNum(g.fused.errorKm) ? units.dist(g.fused.errorKm) + ' away' : null);
      const gps = ctx.status?.receiverGps;
      if (gps && isNum(gps.lat) && isNum(gps.lon)) addMarker('gps', gps.lat, gps.lon, 'Receiver GPS', null);
      step(`Fused estimate: ${units.latlon(g.fused.lat, g.fused.lon, 4)} ± ${units.dist(g.fused.uncertaintyKm)}`);
      renderResult(true); renderTable(); renderHow();
    }

    // ---- adopt a position -----------------------------------------------------------------------
    async function applyLocation(lat, lon, what, source) {
      const la = Math.round(lat * 1e5) / 1e5;
      const lo = Math.round(lon * 1e5) / 1e5;
      const g = S.guess;
      // In demo mode the saved config may have no position while the simulator runs around a pretend one
      // (reported by the guess as `configured`); show that rather than "not set".
      const saved = config.current?.receiver;
      const cur = saved?.located ? saved : g?.configured && isNum(g.configured.lat) ? { ...g.configured, located: true } : saved;
      const rough = source === 'guess' && isNum(g?.fused?.uncertaintyKm) && g.fused.uncertaintyKm > 10;
      const ok = await ui.confirm({
        title: 'Change the station location?',
        confirmLabel: 'Set station location',
        message: h('div.gm-confirm',
          h('p', `AirView will use ${what} as the position of this station. Range rings, distances, coverage and antenna analyses are all computed from it.`),
          ui.kv([
            ['New position', units.latlon(la, lo, 5)],
            ['Current position', cur?.located ? units.latlon(cur.lat, cur.lon, 5) : 'not set', { text: !cur?.located }],
            cur?.located ? ['Moves the station by', units.dist(geo.distanceKm(cur.lat, cur.lon, la, lo))] : null,
          ]),
          rough ? h('p.gm-warn', ui.icon('alert'), ` This estimate is only good to about ± ${units.dist(g.fused.uncertaintyKm)}. It is fine to get started, but enter exact coordinates when you can.`) : null,
          ctx.status?.demo ? h('p.gm-warn', ui.icon('info'), ' Demo mode is on: the traffic is simulated around a pretend station, so this position describes the simulation, not your real antenna.') : null,
          h('p.muted', 'Ground elevation and antenna heights are not changed. You can edit everything under Settings.')),
      });
      if (!ok || !S.alive) return;
      try {
        await admin.run(() => config.save((c) => { c.receiver = { ...(c.receiver || {}), lat: la, lon: lo, located: true }; }),
          'Changing the station location needs the admin password.');
        ui.toast.success(`Station location set to ${units.latlon(la, lo, 4)}.`, { title: 'Saved' });
        ctx.live.refreshStatus?.().catch?.(() => {});
        // the error distances now refer to the new position
        try {
          const fresh = await api.get('/api/guess', { signal: ctx.signal, timeout: 60000 });
          if (S.alive && fresh?.ok && S.phase === 'done') { S.guess = fresh; if (S.mapOk) S.map.getSource('gm-line').setData(fc(fresh.configured ? [{ type: 'Feature', properties: {}, geometry: { type: 'LineString', coordinates: [[fresh.fused.lon, fresh.fused.lat], [fresh.configured.lon, fresh.configured.lat]] } }] : [])); redrawDone(); }
        } catch { /* the saved position is what matters */ }
      } catch (e) {
        if (e.aborted || e.cancelled) return;
        if (e.status === 401) ui.toast.error('The server still refused the change after the login.', { title: 'Could not save the location' });
        else if (e.status === 403) ui.alert('Not allowed from here', 'The server refused the change because this page was not opened from the AirView address itself (cross-origin request). Open AirView directly and try again.');
        else ui.toast.error(e.message || String(e), { title: 'Could not save the location' });
      }
    }
    // ---- wiring ---------------------------------------------------------------------------------
    renderIdleResult();
    renderTable();
    renderHow();
    peek();
    ctx.every(READY_POLL_MS, peek);
    ctx.subscribe('status', () => { if (S.phase === 'idle') { renderReady(); renderIdleResult(); } }, { immediate: false });
    ctx.onCleanup(units.onChange(() => { if (S.phase === 'done') redrawDone(); else if (S.phase === 'idle') { renderReady(); renderIdleResult(); } }));
  },
};
