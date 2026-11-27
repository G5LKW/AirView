// reports/replay.js - time-lapse playback of stored traffic (/api/replay) on a full-size MapLibre map.
//
// The reply's frames are turned into one time-sorted track per flight once; playback then interpolates each
// visible aircraft between its stored points (gaps above staleSec are not bridged), so motion is smooth at any
// speed (1x - 600x) and independent of the frame step. Icons, altitude colours, base map, range rings and the
// tile-cache rewrite come from the Live Map's helpers (read-only imports). Positions redraw at most ~30 times a
// second, trails ~6 times a second; nothing runs while paused except on scrubbing.

import { basemapById, styleFor, makeTransformRequest, LABEL_FONT } from '../map/basemaps.js';
import { altColor, legend as legendFor, SOURCE_COLORS } from '../map/palette.js';
import { SHAPES, shapeFor, imageName, addAircraftImages } from '../map/aircraft-icons.js';
import { receiverOverlays, EMPTY } from '../map/overlays.js';
import { toInputUTC, fromInputUTC, HOUR, fin, firstDataTime } from './common.js';

const DURATIONS = [
  { value: 15 * 60e3, label: '15 min' }, { value: 30 * 60e3, label: '30 min' }, { value: HOUR, label: '1 h' }, { value: 2 * HOUR, label: '2 h' },
  { value: 3 * HOUR, label: '3 h' }, { value: 6 * HOUR, label: '6 h' }, { value: 12 * HOUR, label: '12 h' }, { value: 24 * HOUR, label: '24 h' },
];
const SPEEDS = [1, 2, 5, 10, 30, 60, 120, 300, 600];
const TRAILS = [{ value: 2, label: '2 min' }, { value: 5, label: '5 min' }, { value: 10, label: '10 min' }, { value: 30, label: '30 min' }, { value: 0, label: 'Whole' }];
const SRC_NAMES = ['ADS-B', 'MLAT', 'TIS-B', 'Other'];
const BLANK_STYLE = { version: 8, glyphs: 'https://tiles.openfreemap.org/fonts/{fontstack}/{range}.pbf', sources: {}, layers: [{ id: 'bg', type: 'background', paint: { 'background-color': '#050c1b' } }] };

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

/** Index of the last element of sorted array `a` that is <= x (-1 if none). */
function floorIndex(a, x) {
  let lo = 0; let hi = a.length - 1; let r = -1;
  while (lo <= hi) { const m = (lo + hi) >> 1; if (a[m] <= x) { r = m; lo = m + 1; } else hi = m - 1; }
  return r;
}

/** Frames -> per-flight tracks (time-sorted, de-duplicated stored points) + density per frame. */
function buildTracks(R) {
  const n = R.aircraft.length;
  const tr = Array.from({ length: n }, () => ({ t: [], lat: [], lon: [], alt: [], gs: [], trk: [], src: [] }));
  const density = new Float32Array(R.frames.length);
  R.frames.forEach((f, fi) => {
    density[fi] = f.p.length;
    for (const row of f.p) {
      const k = row[0];
      const x = tr[k];
      if (!x) continue;
      const t = f.t - (row[7] || 0) * 1000;
      const last = x.t.length ? x.t[x.t.length - 1] : -Infinity;
      if (t <= last) continue;
      x.t.push(t); x.lat.push(row[1]); x.lon.push(row[2]); x.alt.push(row[3]); x.gs.push(row[4]); x.trk.push(row[5]); x.src.push(row[6]);
    }
  });
  for (const x of tr) {
    x.first = x.t.length ? x.t[0] : Infinity;
    x.last = x.t.length ? x.t[x.t.length - 1] : -Infinity;
  }
  return { tracks: tr, density };
}

/**
 * @param {HTMLElement} host full-bleed container
 * @param ctx page context
 * @param scope view scope
 * @param {HTMLElement} nav the page's view switcher (placed in the top panel)
 */
export function mountReplay(host, ctx, scope, nav) {
  const { ui, units, api, config, live } = ctx;
  const { h } = ui;
  const prefs = ui.prefs('airview.reports.replay', { dur: HOUR, speed: 60, trails: true, trailMin: 10, labels: true });
  const P = prefs.get();
  const S = {
    R: null, tracks: null, density: null, from: 0, to: 0, staleMs: 0, dataFrom: 0,
    t: 0, playing: false, speed: SPEEDS.includes(P.speed) ? P.speed : 60, sel: -1,
    map: null, styleReady: false, raf: 0, lastTick: 0, lastPos: 0, lastTrail: 0, seq: 0, visible: 0,
    dur: DURATIONS.some((d) => d.value === P.dur) ? P.dur : HOUR, start: null,
  };

  // ---------------------------------------------------------------------------------------------
  // DOM
  // ---------------------------------------------------------------------------------------------
  const canvasEl = h('div.rpr-canvas');
  const stateEl = h('div.rpr-state.hidden');
  const legendEl = h('div.rpr-legend');
  const infoEl = h('div.rpr-info.hidden');
  const now0 = live.serverNow();
  S.start = Math.floor((now0 - S.dur) / 60e3) * 60e3;
  const startIn = ui.input({ type: 'datetime-local', size: 'sm', value: toInputUTC(S.start), 'aria-label': 'Window start (UTC)' });
  const durSel = ui.select({ size: 'sm', options: DURATIONS.map((d) => ({ value: String(d.value), label: d.label })), value: String(S.dur), 'aria-label': 'Window length' });
  const loadBtn = ui.button({ label: 'Load', icon: 'refresh', size: 'sm', variant: 'primary', onClick: () => { readWindow(); load(); } });
  const lastBtn = ui.button({ label: 'Latest', size: 'sm', variant: 'ghost', tip: 'The window that ends now', onClick: () => { S.dur = +durSel.value; S.start = live.serverNow() - S.dur; startIn.value = toInputUTC(S.start); load(); } });
  const winInfo = h('div.rpr-wininfo.muted');
  const panel = h('div.rpr-panel',
    h('div.rpr-panel-nav', nav),
    h('div.rpr-window', h('label.rpr-lab', 'Start (UTC)'), startIn, h('label.rpr-lab', 'Length'), durSel, h('div.rpr-window-btns', lastBtn, loadBtn)),
    winInfo);
  const collapse = ui.iconButton('minimize', 'Hide the window panel', () => { const on = !panel.classList.contains('is-collapsed'); panel.classList.toggle('is-collapsed', on); ui.mount(collapse, ui.icon(on ? 'maximize' : 'minimize')); }, { variant: 'ghost', size: 'sm', class: 'rpr-collapse' });
  panel.prepend(collapse);

  const playBtn = ui.button({ icon: 'play', variant: 'primary', class: 'rpr-play', tip: 'Play / pause (space)', onClick: () => setPlaying(!S.playing) });
  const speedSel = ui.select({ size: 'sm', options: SPEEDS.map((s) => ({ value: String(s), label: s + '×' })), value: String(S.speed), 'aria-label': 'Playback speed',
    onChange: (v) => { S.speed = +v; prefs.set({ speed: S.speed }); } });
  const timeEl = h('div.rpr-time.num');
  const dateEl = h('div.rpr-date.num.muted');
  const countEl = h('div.rpr-count.num');
  const spark = h('canvas.rpr-spark');
  const range = h('input.rpr-range', { type: 'range', min: 0, max: 1000, step: 1, value: 0, 'aria-label': 'Replay position' });
  const startLab = h('span.rpr-tl-lab.num');
  const endLab = h('span.rpr-tl-lab.num');
  const trailsT = ui.toggle({ label: 'Trails', checked: !!P.trails, onChange: (v) => { prefs.set({ trails: v }); drawTrails(true); } });
  const trailSel = ui.select({ size: 'sm', options: TRAILS.map((t) => ({ value: String(t.value), label: t.label })), value: String(P.trailMin), 'aria-label': 'Trail length',
    onChange: (v) => { prefs.set({ trailMin: +v }); drawTrails(true); } });
  const labelsT = ui.toggle({ label: 'Labels', checked: P.labels !== false, onChange: (v) => { prefs.set({ labels: v }); applyLabels(); } });
  const bar = h('div.rpr-bar',
    h('div.rpr-bar-left', playBtn, h('div.rpr-clock', timeEl, dateEl)),
    h('div.rpr-timeline', h('div.rpr-track', spark, range), h('div.rpr-tl-labs', startLab, countEl, endLab)),
    h('div.rpr-bar-right', speedSel, h('div.rpr-opts', trailsT, trailSel, labelsT)));
  const root = h('div.rpr', h('div.rpr-map', canvasEl, panel, legendEl, infoEl, stateEl), bar);
  host.append(root);

  function readWindow() {
    const st = fromInputUTC(startIn.value);
    S.dur = +durSel.value;
    prefs.set({ dur: S.dur });
    if (fin(st)) S.start = st;
    else startIn.value = toInputUTC(S.start);
  }

  function showState(kind, o = {}) {
    if (!kind) { stateEl.classList.add('hidden'); ui.clear(stateEl); return; }
    stateEl.classList.remove('hidden');
    if (kind === 'loading') ui.mount(stateEl, h('div.rpr-state-card', ui.loading(o.text || 'Loading replay…')));
    else if (kind === 'error') ui.mount(stateEl, h('div.rpr-state-card', ui.error(o.error, { title: 'The replay could not be loaded', retry: load })));
    else ui.mount(stateEl, h('div.rpr-state-card', ui.empty({ icon: 'replay', title: o.title || 'Not enough data yet', text: o.text })));
  }

  // ---------------------------------------------------------------------------------------------
  // data
  // ---------------------------------------------------------------------------------------------
  async function load() {
    const my = ++S.seq;
    setPlaying(false);
    readWindow();
    const now = live.serverNow();
    let from = S.start;
    let to = Math.min(from + S.dur, now);
    if (to - from < 60e3) { from = now - S.dur; to = now; S.start = from; startIn.value = toInputUTC(from); }
    const spanS = (to - from) / 1000;
    const step = Math.max(5, Math.ceil(spanS / 1000));
    showState('loading', { text: `Loading ${units.age(spanS)} of stored traffic…` });
    loadBtn.setLoading(true);
    try {
      const R = await api.get('/api/replay', { params: { from: Math.floor(from), to: Math.floor(to), step }, signal: scope.signal, timeout: 90000 });
      if (my !== S.seq) return;
      ingest(R);
    } catch (e) {
      if (e.aborted || my !== S.seq) return;
      S.R = null;
      clearMap();
      showState('error', { error: e });
    } finally { if (my === S.seq) loadBtn.setLoading(false); }
  }

  function ingest(R) {
    S.R = R;
    const { tracks, density } = buildTracks(R);
    S.tracks = tracks;
    S.density = density;
    S.from = R.from;
    S.to = R.to;
    S.staleMs = (R.staleSec || 60) * 1000;
    S.sel = -1;
    infoEl.classList.add('hidden');
    S.t = S.dataFrom = S.from;
    ui.setText(startLab, units.clock(S.from, { seconds: false }));
    ui.setText(endLab, units.clock(S.to, { seconds: false }));
    const flights = tracks.filter((x) => x.t.length).length;
    ui.mount(winInfo, `${units.int(flights)} flights · ${units.si(R.points)} positions · step ${units.age(R.step)}`,
      R.truncated ? h('span.c-red', ' · truncated (point budget reached) - choose a shorter window') : null);
    paintSpark();
    if (!flights) {
      clearMap();
      showState('empty', { title: 'Not enough data yet', text: 'No positions are stored for this window. Stored trails are kept for the retention period in Settings > Storage; try a more recent window.' });
      render(true);
      return;
    }
    showState(null);
    fitToData();
    // Skip a leading stretch without traffic.
    S.t = S.dataFrom = firstDataTime(tracks, S.from, S.to, R.step);
    if (window.matchMedia('(max-width: 640px)').matches && !panel.classList.contains('is-collapsed')) collapse.click();
    render(true);
    // Start right away at a pleasant speed.
    setPlaying(true);
  }

  // ---------------------------------------------------------------------------------------------
  // map
  // ---------------------------------------------------------------------------------------------
  const bm = basemapById(config.current?.ui?.mapStyle || 'airview-dark');
  root.classList.toggle('light-map', !bm.dark);
  async function initMap() {
    let maplibregl;
    try { maplibregl = await loadLib(ui); } catch (e) { showState('error', { error: new Error('The map library could not be loaded: ' + e.message) }); return; }
    if (!scope.alive) return;
    const rx = config.current?.receiver;
    let map;
    try {
      map = new maplibregl.Map({
        container: canvasEl, style: styleFor(bm), center: rx?.located ? [rx.lon, rx.lat] : [8, 48], zoom: rx?.located ? 6 : 4,
        minZoom: 1.5, maxZoom: 15, maxPitch: 0, dragRotate: false, pitchWithRotate: false, touchPitch: false, fadeDuration: 0, attributionControl: false,
        transformRequest: makeTransformRequest(() => !!live.status?.tileCache),
      });
    } catch (e) {
      showState('error', { error: new Error('The map could not be started (' + (e?.message || 'WebGL unavailable') + ').') });
      return;
    }
    S.map = map;
    scope.onCleanup(() => { S.map = null; map.remove(); });
    map.touchZoomRotate.disableRotation();
    map.keyboard.disableRotation();
    map.addControl(new maplibregl.AttributionControl({ compact: true }), 'bottom-right');
    map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');
    let warned = 0;
    map.on('error', (e) => { if (warned++ < 2) console.warn('[replay map]', e?.error?.message || e); });
    const fallback = setTimeout(() => { if (!S.styleReady && scope.alive) map.setStyle(BLANK_STYLE); }, 8000);
    scope.onCleanup(() => clearTimeout(fallback));
    map.on('style.load', () => { if (!scope.alive) return; S.styleReady = true; install(); render(true); });
    map.on('click', (e) => {
      const pad = 8;
      const f = S.styleReady ? map.queryRenderedFeatures([[e.point.x - pad, e.point.y - pad], [e.point.x + pad, e.point.y + pad]], { layers: ['rp-ac'] }) : [];
      select(f.length ? f[0].properties.k : -1);
    });
    map.on('mousemove', ui.rafThrottle((e) => {
      if (!S.styleReady || !scope.alive) return;
      const f = map.queryRenderedFeatures([[e.point.x - 6, e.point.y - 6], [e.point.x + 6, e.point.y + 6]], { layers: ['rp-ac'] });
      map.getCanvas().style.cursor = f.length ? 'pointer' : '';
    }));
  }

  function install() {
    const map = S.map;
    const dark = bm.dark;
    const halo = dark ? 'rgba(1,4,11,0.9)' : 'rgba(255,255,255,0.9)';
    addAircraftImages(map);
    const src = (id) => { if (!map.getSource(id)) map.addSource(id, { type: 'geojson', data: EMPTY, tolerance: 0 }); };
    const add = (l) => { if (!map.getLayer(l.id)) map.addLayer(l); };
    for (const id of ['rp-rings', 'rp-rx', 'rp-trails', 'rp-sel', 'rp-ac']) src(id);
    const ov = receiverOverlays(config.current, units);
    map.getSource('rp-rings').setData(ov.rings);
    map.getSource('rp-rx').setData(ov.receiver);
    const ring = dark ? '#78a8ff' : '#1c4fb8';
    add({ id: 'rp-rings', type: 'line', source: 'rp-rings', paint: { 'line-color': ring, 'line-opacity': 0.4, 'line-width': 1, 'line-dasharray': [2, 4] } });
    add({ id: 'rp-trails', type: 'line', source: 'rp-trails', layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': ['get', 'c'], 'line-width': ['interpolate', ['linear'], ['zoom'], 3, 1, 7, 1.7, 11, 2.6], 'line-opacity': 0.78 } });
    add({ id: 'rp-sel-casing', type: 'line', source: 'rp-sel', layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': dark ? '#ffffff' : '#01040b', 'line-width': 6, 'line-opacity': dark ? 0.2 : 0.45 } });
    add({ id: 'rp-sel', type: 'line', source: 'rp-sel', layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': ['get', 'c'], 'line-width': 3 } });
    add({ id: 'rp-rx-glow', type: 'circle', source: 'rp-rx', paint: { 'circle-radius': 16, 'circle-color': '#2f7dff', 'circle-opacity': 0.28, 'circle-blur': 0.7 } });
    add({ id: 'rp-rx', type: 'circle', source: 'rp-rx', paint: { 'circle-radius': 4.5, 'circle-color': '#ffffff', 'circle-stroke-color': '#2f7dff', 'circle-stroke-width': 3 } });
    add({ id: 'rp-ac-sel', type: 'circle', source: 'rp-ac', filter: ['==', ['get', 'sel'], 1], paint: { 'circle-radius': 20, 'circle-color': 'rgba(47,125,255,0.12)', 'circle-stroke-color': '#8cc2ff', 'circle-stroke-width': 2 } });
    add({
      id: 'rp-ac', type: 'symbol', source: 'rp-ac',
      layout: {
        'icon-image': ['get', 'shape'], 'icon-size': ['interpolate', ['linear'], ['zoom'], 3, ['*', 0.5, ['get', 's']], 6, ['*', 0.72, ['get', 's']], 9, ['get', 's'], 13, ['*', 1.25, ['get', 's']]],
        'icon-rotate': ['get', 'rot'], 'icon-rotation-alignment': 'map', 'icon-allow-overlap': true, 'icon-ignore-placement': true,
        'text-field': prefs.get().labels === false ? '' : ['get', 'label'], 'text-font': LABEL_FONT, 'text-size': 10.5, 'text-variable-anchor': ['left', 'right', 'top', 'bottom'],
        'text-radial-offset': 1.2, 'text-optional': true, 'text-padding': 1,
      },
      paint: {
        'icon-color': ['get', 'c'], 'icon-halo-color': dark ? 'rgba(1,4,11,0.95)' : 'rgba(6,16,40,0.95)', 'icon-halo-width': 1.4,
        'icon-opacity': ['case', ['==', ['get', 'hold'], 1], 0.45, 1],
        'text-color': dark ? '#e3edff' : '#0b1b3a', 'text-halo-color': halo, 'text-halo-width': 1.5,
      },
    });
    paintLegend();
  }

  function applyLabels() {
    if (S.map && S.styleReady && S.map.getLayer('rp-ac')) S.map.setLayoutProperty('rp-ac', 'text-field', prefs.get().labels === false ? '' : ['get', 'label']);
  }

  function clearMap() {
    if (!S.map || !S.styleReady) return;
    for (const id of ['rp-trails', 'rp-sel', 'rp-ac']) S.map.getSource(id)?.setData(EMPTY);
  }

  function fitToData() {
    if (!S.map) return;
    let w = Infinity; let s = Infinity; let e = -Infinity; let n = -Infinity;
    for (const x of S.tracks) {
      for (let i = 0; i < x.t.length; i += 4) {
        const lo = x.lon[i]; const la = x.lat[i];
        if (lo < w) w = lo; if (lo > e) e = lo; if (la < s) s = la; if (la > n) n = la;
      }
    }
    if (Number.isFinite(w)) S.map.fitBounds([[w, s], [e, n]], { padding: { top: 60, bottom: 30, left: 40, right: 40 }, maxZoom: 10, duration: 0 });
  }

  function paintLegend() {
    const lg = legendFor('altitude', units);
    ui.mount(legendEl, h('div.rpr-legend-title', lg.title), h('div.rpr-legend-bar', { style: { background: lg.gradient } }),
      h('div.rpr-legend-ticks', lg.ticks.map((t) => h('span.num', { style: { left: (t.pos * 100).toFixed(1) + '%' } }, t.label))));
  }

  // ---------------------------------------------------------------------------------------------
  // playback
  // ---------------------------------------------------------------------------------------------
  /** Interpolated state of track x at time t, or null when not visible. */
  function stateAt(x, t) {
    if (t < x.first || t > x.last + S.staleMs) return null;
    const j = floorIndex(x.t, t);
    if (j < 0) return null;
    const dt = t - x.t[j];
    if (dt > S.staleMs) return null;
    let lat = x.lat[j]; let lon = x.lon[j]; let alt = x.alt[j]; let gs = x.gs[j]; let trk = x.trk[j];
    let hold = 0;
    if (j + 1 < x.t.length) {
      const gap = x.t[j + 1] - x.t[j];
      if (gap <= S.staleMs && gap > 0) {
        const f = dt / gap;
        lat += (x.lat[j + 1] - lat) * f;
        lon += (x.lon[j + 1] - lon) * f;
        if (fin(alt) && fin(x.alt[j + 1])) alt += (x.alt[j + 1] - alt) * f;
        if (!fin(trk)) trk = ctx.geo.bearing(x.lat[j], x.lon[j], x.lat[j + 1], x.lon[j + 1]);
      } else hold = 1;
    } else hold = dt > 30e3 ? 1 : 0;
    return { lat, lon, alt, gs, trk, src: x.src[j], hold, j };
  }

  function acFeature(k, st) {
    const a = S.R.aircraft[k];
    let shape = a.__shape;
    // emitter category (ADS-B, e.g. "A3") first, then the ICAO type, as on the Live Map
    if (!shape) { shape = shapeFor({ category: a.category, type: a.type, track: 0 }); a.__shape = shape; }
    return {
      type: 'Feature',
      properties: { k, shape: imageName(shape), s: SHAPES[shape]?.scale || 1, rot: fin(st.trk) ? st.trk : 0, c: altColor(st.alt), label: a.callsign || a.reg || a.icao, sel: k === S.sel ? 1 : 0, hold: st.hold },
      geometry: { type: 'Point', coordinates: [st.lon, st.lat] },
    };
  }

  function drawAircraft() {
    if (!S.map || !S.styleReady || !S.tracks) return;
    const feats = [];
    const t = S.t;
    for (let k = 0; k < S.tracks.length; k++) {
      const x = S.tracks[k];
      if (t < x.first || t > x.last + S.staleMs) continue;
      const st = stateAt(x, t);
      if (st) feats.push(acFeature(k, st));
    }
    S.visible = feats.length;
    S.map.getSource('rp-ac').setData({ type: 'FeatureCollection', features: feats });
  }

  /** Line features of one track between indices [i0, j] plus the current point, split into runs of equal colour. */
  function trackLines(x, i0, j, tail, out) {
    let run = null;
    let prev = null;
    for (let i = i0; i <= j; i++) {
      // break the line across gaps the replay would not bridge
      if (i > i0 && x.t[i] - x.t[i - 1] > S.staleMs) { run = null; prev = null; }
      const c = altColor(x.alt[i]);
      const pt = [x.lon[i], x.lat[i]];
      if (!run || run.c !== c) { run = { c, coords: prev ? [prev, pt] : [pt] }; out.push(run); } else run.coords.push(pt);
      prev = pt;
    }
    if (tail && run) run.coords.push([tail.lon, tail.lat]);
  }

  function drawTrails(force) {
    if (!S.map || !S.styleReady || !S.tracks) return;
    const runs = [];
    if (prefs.get().trails) {
      const t = S.t;
      const win = (prefs.get().trailMin || 0) * 60e3;
      for (let k = 0; k < S.tracks.length; k++) {
        const x = S.tracks[k];
        if (t < x.first || t > x.last + S.staleMs) continue;
        const st = stateAt(x, t);
        if (!st) continue;
        const i0 = win ? Math.max(0, floorIndex(x.t, t - win) + 1) : 0;
        if (st.j < i0) continue;
        trackLines(x, i0, st.j, st, runs);
      }
    }
    S.map.getSource('rp-trails').setData({ type: 'FeatureCollection', features: runs.filter((r) => r.coords.length > 1).map((r) => ({ type: 'Feature', properties: { c: r.c }, geometry: { type: 'LineString', coordinates: r.coords } })) });
    if (force) drawSelected();
  }

  function drawSelected() {
    if (!S.map || !S.styleReady) return;
    const runs = [];
    if (S.sel >= 0 && S.tracks?.[S.sel]?.t.length) { const x = S.tracks[S.sel]; trackLines(x, 0, x.t.length - 1, null, runs); }
    S.map.getSource('rp-sel').setData({ type: 'FeatureCollection', features: runs.filter((r) => r.coords.length > 1).map((r) => ({ type: 'Feature', properties: { c: r.c }, geometry: { type: 'LineString', coordinates: r.coords } })) });
  }

  // ---- selected aircraft info -----------------------------------------------------------------
  let info = null;
  function select(k) {
    S.sel = Number.isInteger(k) && k >= 0 && S.R && k < S.R.aircraft.length ? k : -1;
    drawSelected();
    if (S.sel < 0) { infoEl.classList.add('hidden'); info = null; render(true); return; }
    const a = S.R.aircraft[S.sel];
    const x = S.tracks[S.sel];
    const kv = ui.kv([['Altitude', units.DASH], ['Speed', units.DASH], ['Track', units.DASH], ['Source', units.DASH], ['Seen', `${units.clock(x.first)} – ${units.clock(x.last)}`]]);
    info = { kv };
    ui.mount(infoEl,
      h('div.rpr-info-head',
        h('div.grow', h('div.rpr-info-title.num', a.callsign || a.reg || a.icao), h('div.rpr-info-sub.muted', [a.icao, a.reg, a.type, a.operator].filter(Boolean).join(' · '))),
        a.military ? ui.badge('Military', 'amber') : null,
        ui.iconButton('x', 'Close', () => select(-1), { variant: 'ghost', size: 'sm' })),
      kv,
      h('div.rpr-info-actions',
        ui.button({ label: 'Flight details', icon: 'report', size: 'sm', href: '#/reports/flight/' + a.flightId }),
        ui.button({ label: 'Live Map', icon: 'map', size: 'sm', variant: 'ghost', href: '#/map/' + a.icao })));
    infoEl.classList.remove('hidden');
    render(true);
  }
  function paintInfo() {
    if (!info || S.sel < 0) return;
    const st = stateAt(S.tracks[S.sel], S.t);
    info.kv.set('Altitude', st ? units.alt(st.alt, { fl: true }) : 'not visible now');
    info.kv.set('Speed', st ? units.speed(st.gs) : units.DASH);
    info.kv.set('Track', st ? units.bearing(st.trk) : units.DASH);
    info.kv.set('Source', st ? h('span', h('i.rpr-dot', { style: { background: [SOURCE_COLORS.adsb, SOURCE_COLORS.mlat, SOURCE_COLORS.tisb, SOURCE_COLORS.other][st.src] || SOURCE_COLORS.other } }), SRC_NAMES[st.src] || 'Other') : units.DASH);
  }

  // ---- timeline ----------------------------------------------------------------------------------
  function paintSpark() {
    const dpr = Math.min(2, window.devicePixelRatio || 1);
    const w = spark.clientWidth || 300;
    const hgt = spark.clientHeight || 34;
    if (spark.width !== Math.round(w * dpr) || spark.height !== Math.round(hgt * dpr)) { spark.width = Math.round(w * dpr); spark.height = Math.round(hgt * dpr); }
    const g = spark.getContext('2d');
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, w, hgt);
    const d = S.density;
    if (!d || !d.length) return;
    let max = 1;
    for (const v of d) if (v > max) max = v;
    const frac = S.to > S.from ? (S.t - S.from) / (S.to - S.from) : 0;
    const C = ctx.charts.colors;
    const cols = Math.min(d.length, Math.max(20, Math.floor(w / 2)));
    const bw = w / cols;
    for (let c = 0; c < cols; c++) {
      const i0 = Math.floor((c * d.length) / cols);
      const i1 = Math.max(i0 + 1, Math.floor(((c + 1) * d.length) / cols));
      let v = 0;
      for (let i = i0; i < i1; i++) if (d[i] > v) v = d[i];
      const bh = Math.max(v > 0 ? 1.5 : 0, (v / max) * (hgt - 4));
      g.fillStyle = (c + 0.5) / cols <= frac ? C.cyan : 'rgba(120,165,255,0.32)';
      g.fillRect(c * bw + 0.5, hgt - bh, Math.max(1, bw - 1), bh);
    }
  }

  let lastSparkFrac = -1;
  function render(force) {
    const span = S.to - S.from;
    const frac = span > 0 ? (S.t - S.from) / span : 0;
    if (S.R) {
      ui.setText(timeEl, units.clock(S.t));
      ui.setText(dateEl, units.date(S.t));
    } else { ui.setText(timeEl, '--:--:--'); ui.setText(dateEl, ''); }
    if (document.activeElement !== range || force) range.value = String(Math.round(frac * 1000));
    const now = performance.now();
    if (force || now - S.lastPos > 33) { S.lastPos = now; drawAircraft(); ui.setText(countEl, S.R ? `${units.int(S.visible)} aircraft` : ''); paintInfo(); }
    if (force || now - S.lastTrail > 160) { S.lastTrail = now; drawTrails(force); }
    if (force || Math.abs(frac - lastSparkFrac) > 0.002) { lastSparkFrac = frac; paintSpark(); }
  }

  function tick(ts) {
    S.raf = 0;
    if (!S.playing || !scope.alive) return;
    const dt = S.lastTick ? Math.min(250, ts - S.lastTick) : 16;
    S.lastTick = ts;
    S.t += dt * S.speed;
    if (S.t >= S.to) { S.t = S.to; render(true); setPlaying(false); return; }
    render(false);
    S.raf = requestAnimationFrame(tick);
  }

  function setPlaying(on) {
    on = !!on && !!S.R && S.tracks?.length > 0;
    // Play from the first recorded data, never from an empty lead-in of the window (also after the end).
    if (on && (S.t >= S.to || S.t < S.dataFrom)) S.t = S.dataFrom;
    S.playing = on;
    ui.mount(playBtn, ui.icon(on ? 'pause' : 'play'));
    playBtn.setAttribute('aria-label', on ? 'Pause' : 'Play');
    if (on && !S.raf) { S.lastTick = 0; S.raf = requestAnimationFrame(tick); }
    if (!on && S.raf) { cancelAnimationFrame(S.raf); S.raf = 0; }
  }
  // A background tab gets no animation frames: pause cleanly instead of jumping when it comes back.
  const onVis = () => { if (document.hidden && S.playing) setPlaying(false); };
  document.addEventListener('visibilitychange', onVis);
  scope.onCleanup(() => document.removeEventListener('visibilitychange', onVis));
  scope.onCleanup(() => { if (S.raf) cancelAnimationFrame(S.raf); S.raf = 0; S.playing = false; });

  range.addEventListener('input', () => {
    if (!S.R) return;
    S.t = S.from + (Number(range.value) / 1000) * (S.to - S.from);
    render(true);
  });
  const stepFrame = (n) => { if (!S.R) return; setPlaying(false); S.t = Math.max(S.from, Math.min(S.to, S.t + n * S.R.step * 1000)); render(true); };
  const onKey = (e) => {
    if (e.target.closest('input:not([type=range]),select,textarea') || e.ctrlKey || e.metaKey || e.altKey) return;
    if (e.key === ' ') { e.preventDefault(); setPlaying(!S.playing); }
    else if (e.key === 'ArrowRight' && e.target !== range) { e.preventDefault(); stepFrame(e.shiftKey ? 10 : 1); }
    else if (e.key === 'ArrowLeft' && e.target !== range) { e.preventDefault(); stepFrame(e.shiftKey ? -10 : -1); }
    else if (e.key === 'Escape' && S.sel >= 0) select(-1);
  };
  root.tabIndex = -1;
  root.addEventListener('keydown', onKey);
  scope.onCleanup(ui.onResize(spark, () => paintSpark()));
  scope.onCleanup(units.onChange(() => { paintLegend(); if (S.map && S.styleReady) { const ov = receiverOverlays(config.current, units); S.map.getSource('rp-rings')?.setData(ov.rings); } if (S.R) { ingestLabels(); render(true); } }));
  function ingestLabels() { ui.setText(startLab, units.clock(S.from, { seconds: false })); ui.setText(endLab, units.clock(S.to, { seconds: false })); }

  render(true);
  paintLegend();
  initMap();
  load();
}
