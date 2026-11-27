// settings/sections-station.js - Station location, Antennas and Receiver feeds sections of the Settings page.

import {
  ANT_TYPES, FEED_TYPES, FEED_PRESETS, MAX_ANTENNAS, MAX_FEEDS, clone, uniqueId, newAntenna, newFeed,
  resolvePattern, azimuthCut, elevationCut, horizonKm, lineOfSightKm,
} from './model.js';
import { hasRadarcape, isFactoryDefaultFeed, LOCAL_DECODER_FEED } from './links.js';

const round = (v, d) => Number(Number(v).toFixed(d));
const isNum = (v) => typeof v === 'number' && Number.isFinite(v);

// =============================================================================================
// Station location
// =============================================================================================

export function renderStation(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const scope = S.scope;
  const rx = () => S.draft.receiver;
  const hasPos = () => rx().located && isNum(rx().lat) && isNum(rx().lon) && Math.abs(rx().lat) <= 90 && Math.abs(rx().lon) <= 180;

  // ---- current value in the common notations ----
  const locBadge = h('span');
  const fmtKv = ui.kv([['Decimal degrees', '—'], ['Degrees, minutes, seconds', '—'], ['Degrees, decimal minutes', '—'], ['Maidenhead locator', '—']]);
  const fmtNote = h('div.set-help');
  let fmtSeq = 0;
  const refreshFormats = ui.debounce(async () => {
    const my = ++fmtSeq;
    if (!hasPos()) return;
    try {
      const r = await api.get('/api/geo/format', { params: { lat: rx().lat, lon: rx().lon }, signal: scope.signal });
      if (my !== fmtSeq) return;
      const f = r.formatted || {};
      fmtKv.set(1, f.dms || '—');
      fmtKv.set(2, f.ddm || '—');
      fmtKv.set(3, f.maidenhead8 ? `${f.maidenhead} (${f.maidenhead8})` : f.maidenhead || '—');
    } catch (e) {
      if (e.aborted || my !== fmtSeq) return;
      fmtKv.set(1, '—'); fmtKv.set(2, '—'); fmtKv.set(3, '—');
    }
  }, 300);
  function paintCurrent() {
    const ok = hasPos();
    ui.mount(locBadge, ok ? ui.badge('Located', 'green') : ui.badge('Not located', 'amber'));
    fmtKv.set(0, ok ? units.latlon(rx().lat, rx().lon) : '—');
    if (!ok) { fmtSeq++; fmtKv.set(1, '—'); fmtKv.set(2, '—'); fmtKv.set(3, '—'); }
    ui.setText(fmtNote, ok ? ''
      : S.status?.demo ? 'No position is stored. Demo mode places the station at the simulator centre until you set one.'
        : 'No position is stored yet. Range, bearing, coverage and the antenna analysis all need to know where the antenna is - enter it below.');
    fmtNote.classList.toggle('hidden', ok);
    if (ok) refreshFormats();
  }

  // ---- set the position from any source ----
  let marker = null;
  let map = null;
  function setPosition(lat, lon, opts = {}) {
    const r = rx();
    r.lat = round(lat, 6);
    r.lon = round(lon, 6);
    r.located = true;
    S.notes.delete('receiver.lat');
    S.notes.delete('receiver.lon');
    latF.show(r.lat);
    lonF.show(r.lon);
    S.touch();
    paintCurrent();
    paintGps();
    if (marker && !opts.fromMap) {
      marker.setLngLat([r.lon, r.lat]);
      if (map) { if (!marker._onMap) { marker.addTo(map); marker._onMap = true; } map.easeTo({ center: [r.lon, r.lat], zoom: Math.max(map.getZoom(), 11) }); }
    }
  }
  function onCoordTyped() {
    const r = rx();
    if (isNum(r.lat) && isNum(r.lon) && Math.abs(r.lat) <= 90 && Math.abs(r.lon) <= 180) {
      r.located = true;
      S.touch();
      if (marker && map) { marker.setLngLat([r.lon, r.lat]); if (!marker._onMap) { marker.addTo(map); marker._onMap = true; } }
    }
    paintCurrent();
  }

  // ---- paste box, validated live by /api/geo/parse ----
  const parseOut = h('div.set-parse', h('span.muted', 'Paste or type coordinates in any notation - decimal degrees, 50°02\'00"N 8°34\'13"E, N50 02.000 E008 34.223, a Maidenhead locator (JO40ga), a Plus Code, a geo: link or a Google / OpenStreetMap URL.'));
  let parsed = null;
  let parseSeq = 0;
  const useBtn = ui.button({ label: 'Use this position', icon: 'check', variant: 'primary', size: 'sm', disabled: true, onClick: applyParsed });
  function applyParsed() {
    if (!parsed) return;
    setPosition(parsed.lat, parsed.lon);
    ui.toast.success('Position taken over - remember to save.');
  }
  const runParse = ui.debounce(async (q) => {
    const my = ++parseSeq;
    parsed = null;
    useBtn.disabled = true;
    if (!q.trim()) { ui.mount(parseOut, h('span.muted', 'Paste or type coordinates in any notation.')); return; }
    try {
      const r = await api.get('/api/geo/parse', { params: { q }, signal: scope.signal });
      if (my !== parseSeq) return;
      if (!r.ok) { ui.mount(parseOut, h('span.set-parse-bad', ui.icon('alert'), r.error || 'Not understood yet - keep typing.')); return; }
      parsed = { lat: r.lat, lon: r.lon };
      useBtn.disabled = false;
      const f = r.formatted || {};
      ui.mount(parseOut,
        h('div.set-parse-ok', ui.icon('check-circle'), h('b.num', units.latlon(r.lat, r.lon)), ui.badge(r.format || 'recognised', 'blue')),
        h('div.set-parse-detail.num', [f.dms, f.maidenhead, hasPos() && isNum(r.distKm) ? `${units.dist(r.distKm)} ${units.cardinal(r.bearing)} of the stored position` : null].filter(Boolean).join('  ·  ')));
    } catch (e) {
      if (e.aborted || my !== parseSeq) return;
      ui.mount(parseOut, h('span.set-parse-bad', ui.icon('alert'), 'Could not check the coordinates: ' + e.message));
    }
  }, 280);
  const pasteInp = ui.input({ placeholder: 'e.g. 50.0333, 8.5704  or  JO40ga  or  50°02\'00"N 8°34\'13"E', icon: 'search', mono: true, clearable: true, autocomplete: 'off', spellcheck: 'false', onInput: (v) => runParse(v), onEnter: () => applyParsed() });

  // ---- numeric fields ----
  const latF = F.number('receiver.lat', { label: 'Latitude', suffix: '° N', digits: 6, min: -90, max: 90, hint: 'Decimal degrees, south is negative.', onChange: onCoordTyped });
  const lonF = F.number('receiver.lon', { label: 'Longitude', suffix: '° E', digits: 6, min: -180, max: 180, hint: 'Decimal degrees, west is negative.', onChange: onCoordTyped });

  // ---- map picker ----
  const mapHost = h('div.set-map.hidden');
  const mapBtn = ui.button({ label: 'Pick on map', icon: 'map', onClick: toggleMap });
  async function toggleMap() {
    if (map) { mapHost.classList.toggle('hidden'); mapBtn.setPressed(!mapHost.classList.contains('hidden')); if (!mapHost.classList.contains('hidden')) map.resize(); return; }
    mapHost.classList.remove('hidden');
    mapBtn.setLoading(true);
    const canvas = h('div.set-map-canvas');
    ui.mount(mapHost, canvas, h('div.set-map-hint', S.locked ? 'Read-only: log in to move the marker.' : 'Click the map or drag the marker to the exact antenna position, then save.'));
    try {
      const [maplibregl, bm] = await Promise.all([
        import('../../vendor/maplibre/maplibre-gl.mjs'), import('../map/basemaps.js'),
        ui.loadCss(new URL('../../vendor/maplibre/maplibre-gl.css', import.meta.url)),
      ]);
      if (scope.signal.aborted) return;
      const gps = S.status?.receiverGps;
      const center = hasPos() ? [rx().lon, rx().lat] : gps ? [gps.lon, gps.lat] : [8, 30];
      map = new maplibregl.Map({
        container: canvas, style: bm.styleFor(bm.basemapById(S.draft.ui?.mapStyle || 'airview-dark')), center, zoom: hasPos() ? 13 : gps ? 11 : 1.6,
        maxPitch: 0, dragRotate: false, pitchWithRotate: false, touchPitch: false, attributionControl: false,
        transformRequest: bm.makeTransformRequest(() => !!S.ctx.live.status?.tileCache),
      });
      map.touchZoomRotate.disableRotation();
      map.addControl(new maplibregl.AttributionControl({ compact: true }), 'bottom-right');
      map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');
      marker = new maplibregl.Marker({ draggable: !S.locked, color: '#2f7dff' });
      if (hasPos()) { marker.setLngLat(center).addTo(map); marker._onMap = true; }
      marker.on('dragend', () => { const p = marker.getLngLat(); setPosition(p.lat, p.lng, { fromMap: true }); });
      map.on('click', (e) => {
        if (S.locked) return;
        marker.setLngLat(e.lngLat);
        if (!marker._onMap) { marker.addTo(map); marker._onMap = true; }
        setPosition(e.lngLat.lat, e.lngLat.lng, { fromMap: true });
      });
      scope.onCleanup(() => { try { map.remove(); } catch { /* already gone */ } map = null; marker = null; });
      scope.onCleanup(ui.onResize(canvas, () => map && map.resize()));
      mapBtn.setPressed(true);
    } catch (e) {
      console.error('[settings] map picker failed', e);
      ui.mount(mapHost, ui.error(e, { title: 'The map could not be loaded', compact: true }));
    } finally {
      mapBtn.setLoading(false);
    }
  }

  // ---- receiver GPS ----
  const gpsBtn = ui.button({ label: 'Use receiver GPS position', icon: 'locate', onClick: () => {
    const g = S.status?.receiverGps;
    if (!g) return;
    setPosition(g.lat, g.lon);
    ui.toast.success('Receiver GPS position taken over - remember to save.');
  } });
  const gpsNote = h('div.set-help');
  function paintGps() {
    const g = S.status?.receiverGps;
    gpsBtn.disabled = !g || S.locked;
    // Radarcape-specific: only receivers that report their own GPS position offer this
    const show = !!g || hasRadarcape(S.draft, S.status);
    gpsBtn.classList.toggle('hidden', !show);
    gpsNote.classList.toggle('hidden', !show);
    if (g) {
      ui.setText(gpsNote, `The receiver (feed "${g.feedId}") reports its own GPS position: ${units.latlon(g.lat, g.lon)}, GPS altitude ${units.height(g.altM)}, ${units.ago(g.t)}.`);
    } else {
      ui.setText(gpsNote, 'No receiver reports a GPS position right now. A Radarcape sends one on its Beast port (10003) once its GPS has a fix; other receivers do not.');
    }
  }
  scope.subscribe('status', (st) => { if (st) { S.status = st; paintGps(); } }, { immediate: false });

  const clearBtn = ui.button({ label: 'Clear location', icon: 'x', variant: 'ghost', onClick: () => {
    const r = rx();
    r.lat = 0; r.lon = 0; r.located = false;
    latF.show(0); lonF.show(0);
    if (marker) { marker.remove(); marker._onMap = false; }
    S.touch();
    paintCurrent();
  } });

  // ---- ground elevation ----
  const elevMsg = h('div.set-help');
  const elevF = F.number('receiver.groundElevM', { label: 'Ground elevation', kind: 'height', digits: 1, hint: 'Terrain height above mean sea level at the antenna site (not the antenna height - that is set per antenna).' });
  const elevBtn = ui.button({ label: 'Look up', icon: 'download', tip: 'Fetch the terrain elevation at the entered position (needs the internet)', onClick: async () => {
    if (!hasPos()) { ui.setText(elevMsg, 'Enter the position first.'); elevMsg.className = 'set-help set-help--bad'; return; }
    elevBtn.setLoading(true);
    try {
      const r = await api.get('/api/geo/elevation', { params: { lat: rx().lat, lon: rx().lon }, signal: scope.signal, timeout: 20000 });
      if (!isNum(r.elevationM)) throw new Error('the elevation service returned no value');
      S.set('receiver.groundElevM', round(r.elevationM, 1));
      elevF.show(rx().groundElevM);
      elevMsg.className = 'set-help set-help--ok';
      ui.setText(elevMsg, `${units.height(r.elevationM)} at ${units.latlon(r.lat, r.lon)}. ${r.attribution || ''}`);
    } catch (e) {
      if (e.aborted) return;
      elevMsg.className = 'set-help set-help--bad';
      ui.setText(elevMsg, `Look-up failed: ${e.message}. Enter the elevation by hand (a topographic map or your GPS will do).`);
    } finally {
      elevBtn.setLoading(false);
    }
  } });

  paintCurrent();
  paintGps();

  return [
    ui.card({ title: 'Current position', icon: 'pin', actions: locBadge, body: [fmtKv, fmtNote] }),
    ui.card({ title: 'Set the position', icon: 'target', body: [
      ui.field({ label: 'Coordinates (any notation)', control: pasteInp }),
      h('div.set-parse-row', parseOut, useBtn),
      h('div.set-fields.set-fields--2', latF, lonF),
      h('div.row-wrap.set-actions', mapBtn, gpsBtn,
        ui.button({ label: 'Guess from received data', icon: 'radar', href: '#/guess', tip: 'Estimate the station position from the aircraft positions it receives' }),
        h('span.spacer'), clearBtn),
      gpsNote,
      mapHost,
    ] }),
    ui.card({ title: 'Elevation', icon: 'ruler', body: [
      h('div.set-inline', elevF, elevBtn),
      elevMsg,
      F.help('Ground elevation plus the antenna height gives the antenna altitude that the radio-horizon and coverage calculations use.'),
    ] }),
    ui.card({ title: 'Station', icon: 'radar', body: h('div.set-fields.set-fields--2',
      F.text('receiver.name', { label: 'Station name', maxLength: 80, placeholder: 'My Station', hint: 'Shown in the top bar and sent to remote AirView clients.' }),
      F.text('receiver.model', { label: 'Receiver model', maxLength: 80, placeholder: 'e.g. RTL-SDR + readsb', hint: 'Informational.' })) }),
  ];
}

// =============================================================================================
// Antennas
// =============================================================================================

/** Compass dial: drag (or arrow keys) to set a bearing; draws the beam as a wedge. */
function compassDial(ui, opts) {
  const { h } = ui;
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '-60 -60 120 120');
  svg.setAttribute('class', 'set-dial-svg');
  const mk = (tag, attrs, parent = svg) => { const e = document.createElementNS(NS, tag); for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v); parent.appendChild(e); return e; };
  mk('circle', { r: 46, class: 'set-dial-ring' });
  for (let d = 0; d < 360; d += 10) {
    const major = d % 90 === 0;
    const mid = d % 30 === 0;
    const r1 = major ? 37 : mid ? 40 : 43;
    const a = (d * Math.PI) / 180;
    mk('line', { x1: Math.sin(a) * r1, y1: -Math.cos(a) * r1, x2: Math.sin(a) * 46, y2: -Math.cos(a) * 46, class: 'set-dial-tick' + (major ? ' major' : '') });
  }
  for (const [t, x, y] of [['N', 0, -50], ['E', 53, 3], ['S', 0, 57], ['W', -53, 3]]) { const e = mk('text', { x, y, class: 'set-dial-label' + (t === 'N' ? ' north' : '') }); e.textContent = t; }
  const wedge = mk('path', { class: 'set-dial-wedge' });
  const needle = mk('line', { x1: 0, y1: 0, x2: 0, y2: -40, class: 'set-dial-needle' });
  mk('circle', { r: 3, class: 'set-dial-hub' });
  const tipDot = mk('circle', { r: 4.5, class: 'set-dial-tip' });
  const el = h('div.set-dial', { role: 'slider', tabindex: opts.disabled ? -1 : 0, 'aria-label': 'Azimuth', 'aria-valuemin': 0, 'aria-valuemax': 359 }, svg);
  let value = 0;
  let beam = 0;
  const pt = (deg, r) => `${(Math.sin((deg * Math.PI) / 180) * r).toFixed(2)} ${(-Math.cos((deg * Math.PI) / 180) * r).toFixed(2)}`;
  function draw() {
    const a = (value * Math.PI) / 180;
    needle.setAttribute('x2', Math.sin(a) * 40);
    needle.setAttribute('y2', -Math.cos(a) * 40);
    tipDot.setAttribute('cx', Math.sin(a) * 40);
    tipDot.setAttribute('cy', -Math.cos(a) * 40);
    const bw = Math.max(0, Math.min(359, beam));
    wedge.setAttribute('d', bw > 0 ? `M0 0 L${pt(value - bw / 2, 40)} A40 40 0 ${bw > 180 ? 1 : 0} 1 ${pt(value + bw / 2, 40)} Z` : '');
    el.setAttribute('aria-valuenow', Math.round(value));
    el.setAttribute('aria-valuetext', `${Math.round(value)} degrees`);
  }
  el.set = (v) => { value = isNum(v) ? ((v % 360) + 360) % 360 : 0; draw(); };
  el.setBeam = (b) => { beam = isNum(b) ? b : 0; draw(); };
  if (opts.color) el.style.setProperty('--dial', opts.color);
  const fromEvent = (e) => {
    const r = svg.getBoundingClientRect();
    const dx = e.clientX - (r.left + r.width / 2);
    const dy = e.clientY - (r.top + r.height / 2);
    let deg = (Math.atan2(dx, -dy) * 180) / Math.PI;
    if (deg < 0) deg += 360;
    deg = e.shiftKey ? Math.round(deg) : Math.round(deg / 5) * 5;
    return deg % 360;
  };
  let dragging = false;
  el.addEventListener('pointerdown', (e) => {
    if (opts.disabled()) return;
    dragging = true;
    el.setPointerCapture(e.pointerId);
    el.set(fromEvent(e)); opts.onInput(value);
    e.preventDefault();
    el.focus();
  });
  el.addEventListener('pointermove', (e) => { if (dragging) { el.set(fromEvent(e)); opts.onInput(value); } });
  const end = () => { dragging = false; };
  el.addEventListener('pointerup', end);
  el.addEventListener('pointercancel', end);
  el.addEventListener('keydown', (e) => {
    if (opts.disabled()) return;
    const step = e.shiftKey ? 10 : 1;
    if (e.key === 'ArrowRight' || e.key === 'ArrowUp') { el.set(value + step); opts.onInput(value); e.preventDefault(); }
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowDown') { el.set(value - step); opts.onInput(value); e.preventDefault(); }
  });
  el.set(opts.value);
  el.setBeam(opts.beam);
  return el;
}

/** Shared list-editor chrome: header row with expand toggle and reorder / duplicate / remove buttons. */
function listItem(ui, o) {
  const { h } = ui;
  const head = h('div.set-item-head',
    h('button.set-item-toggle', { type: 'button', 'aria-expanded': String(o.open), onclick: o.onToggle },
      ui.icon(o.open ? 'chevron-down' : 'chevron-right'), o.lead, h('div.set-item-title', o.title, o.sub && h('div.set-item-sub', o.sub)), o.badges),
    h('div.set-item-actions',
      ui.iconButton('arrow-up', 'Move up', o.onUp, { variant: 'ghost', size: 'sm', disabled: o.first }),
      ui.iconButton('arrow-down', 'Move down', o.onDown, { variant: 'ghost', size: 'sm', disabled: o.last }),
      ui.iconButton('copy', 'Duplicate', o.onDuplicate, { variant: 'ghost', size: 'sm', disabled: o.full }),
      ui.iconButton('trash', 'Remove', o.onRemove, { variant: 'ghost', size: 'sm' })));
  return h('div.set-item' + (o.open ? '.open' : '') + (o.bad ? '.bad' : ''), head, o.open ? h('div.set-item-body', o.body()) : null);
}

export function renderAntennas(S, F) {
  const { ui, units } = S.ctx;
  const { h } = ui;
  const scope = S.scope;
  const list = () => S.draft.antennas;
  if (!Array.isArray(S.draft.antennas)) S.draft.antennas = [];
  if (S.mem.open.ant === undefined) S.mem.open.ant = 0;
  const savedIds = new Set((S.saved.antennas || []).map((a) => a.id));
  const structural = () => { S.touch(); S.rerender(); };
  const hasProblem = (i) => S.problems.some((p) => p.path.startsWith(`antennas.${i}.`));

  function body(i) {
    const a = list()[i];
    const p = `antennas.${i}.`;
    const isDir = a.type === 'yagi' || a.type === 'patch' || a.type === 'other';
    let prevId = a.id;
    let autoId = !savedIds.has(a.id);

    // ---- live preview --------------------------------------------------------------------
    const azEl = h('div.chart.set-chart');
    const elEl = h('div.chart.set-chart');
    const azChart = S.ctx.charts.create(azEl);
    const elChart = S.ctx.charts.create(elEl);
    scope.onCleanup(() => { azChart.dispose(); elChart.dispose(); });
    const modelLine = h('div.set-model');
    const horizonKv = ui.kv([['Antenna above sea level', '—'], ['Radio horizon of the antenna itself', '—'], [`Line of sight to ${units.alt(1000)}`, '—'], [`Line of sight to ${units.alt(10000)}`, '—'], [`Line of sight to ${units.alt(35000, { fl: true })}`, '—']]);
    let dial = null;

    // Coalesced with a timer, not requestAnimationFrame: rAF never fires in a background tab, which left
    // the preview empty when the page was opened there.
    let previewPending = false;
    const preview = () => {
      if (previewPending) return;
      previewPending = true;
      setTimeout(() => { previewPending = false; drawPreview(); }, 40);
    };
    const drawPreview = () => {
      if (scope.signal.aborted) return;
      const m = resolvePattern(a);
      const est = (k, text) => (m.estimated.includes(k) ? h('span.set-est', { tip: 'Not entered (0): estimated from the other values' }, text + ' est.') : h('span', text));
      ui.mount(modelLine,
        ui.badge(m.directional ? 'Directional model' : 'Omnidirectional model', m.directional ? 'amber' : 'cyan'),
        est('gainDbi', `peak ${units.num(m.peakGainDbi, 1)} dBi`),
        m.directional ? est('hBeamwidthDeg', `horizontal beam ${units.num(m.hBeamwidthDeg, 0)}°`) : h('span', 'all round'),
        est('vBeamwidthDeg', `vertical beam ${units.num(m.vBeamwidthDeg, 0)}°`),
        m.directional ? est('frontToBackDb', `front-to-back ${units.num(m.frontToBackDb, 0)} dB`) : null);
      const color = a.color && /^#[0-9a-f]{6}$/i.test(a.color) ? a.color : '#38bdf8';
      // Radius = dB below the peak, shifted so that the centre is -30 dB and the outer ring the peak (0 dB).
      const span = 30;
      const az = azimuthCut(m, 3, 2).map(([g, b]) => [round(Math.max(0, g - m.peakGainDbi + span), 2), b]);
      const opt = S.ctx.charts.polar({ series: [{ name: a.name || 'Antenna', data: az, color, area: 0.2 }], minR: 0, maxR: span, rFormat: (v) => `${units.num(v - span, 1)} dB`, legend: false });
      azChart.set(opt, { replace: true });
      const ec = elevationCut(m, 1);
      elChart.set(S.ctx.charts.line({
        x: ec.map(([e]) => `${e}°`), series: [{ name: 'Gain', data: ec.map(([, g]) => round(g, 2)), color, area: true }],
        yName: 'dBi', legend: false, valueFormat: (v) => `${units.num(v, 1)} dBi`,
      }), { replace: true });
      const amsl = (isNum(S.draft.receiver?.groundElevM) ? S.draft.receiver.groundElevM : 0) + (isNum(a.heightAglM) ? a.heightAglM : 0);
      horizonKv.set(0, units.height(amsl));
      horizonKv.set(1, units.dist(horizonKm(amsl)));
      horizonKv.set(2, units.dist(lineOfSightKm(amsl, 1000)));
      horizonKv.set(3, units.dist(lineOfSightKm(amsl, 10000)));
      horizonKv.set(4, units.dist(lineOfSightKm(amsl, 35000)));
      if (dial) { dial.set(a.azimuthDeg); dial.setBeam(m.directional ? m.hBeamwidthDeg : 0); dial.style.setProperty('--dial', color); }
    };
    const upd = () => preview();

    // ---- identity ------------------------------------------------------------------------
    const idF = F.text(p + 'id', { label: 'ID', mono: true, maxLength: 32, hint: 'Feeds and per-antenna statistics refer to this ID. Renaming it detaches statistics already stored under the old ID.',
      onChange: (v) => { autoId = false; renameRefs(prevId, v); prevId = v; } });
    function renameRefs(from, to) {
      if (!from || from === to) return;
      for (const f of S.draft.feeds || []) if (f.antennaId === from) f.antennaId = to;
    }
    const nameF = F.text(p + 'name', { label: 'Name', maxLength: 80, placeholder: 'e.g. Roof collinear', onChange: (v) => {
      if (autoId) {
        const id = uniqueId(v, list().filter((x) => x !== a).map((x) => x.id), 'antenna');
        renameRefs(prevId, id);
        a.id = id; prevId = id;
        idF.control.value = id;
        S.touch();
      }
      const t = S.scopeTitle?.[i];
      if (t) ui.setText(t, v || '(unnamed)');
    } });
    const colorInp = h('input.set-color', { type: 'color', 'aria-label': 'Colour' });
    colorInp.value = /^#[0-9a-f]{6}$/i.test(a.color || '') ? a.color : '#38bdf8';
    const colorText = ui.input({ value: a.color || '', mono: true, maxlength: 9, onInput: (v) => { S.set(p + 'color', v.trim()); if (/^#[0-9a-f]{6}$/i.test(v.trim())) colorInp.value = v.trim(); upd(); } });
    colorInp.addEventListener('input', () => { S.set(p + 'color', colorInp.value); colorText.value = colorInp.value; upd(); });
    const colorF = F.reg(p + 'color', ui.field({ label: 'Colour', hint: 'Used for this antenna\'s overlays and chart series.', control: h('div.row', colorInp, h('div.grow', colorText)) }));

    const typeF = F.seg(p + 'type', { label: 'Type', options: ANT_TYPES.map((t) => ({ value: t.value, label: t.label, tip: t.hint })), onChange: () => structural() });
    typeF.setHint(ANT_TYPES.find((t) => t.value === a.type)?.hint || '');

    const chanOpts = [{ value: -1, label: 'Not applicable / unknown' }];
    for (let c = 0; c < 16; c++) chanOpts.push({ value: c, label: `Channel ${c + 1}  (index ${c})` });

    // ---- direction -----------------------------------------------------------------------
    let dirGroup = null;
    if (isDir) {
      const azF = F.number(p + 'azimuthDeg', { label: 'Azimuth (boresight)', suffix: '° true', min: 0, max: 360, step: 1, hint: 'Compass direction the antenna points to, in degrees true (not magnetic). 0 = north, 90 = east.', onChange: upd });
      dial = compassDial(ui, { value: a.azimuthDeg, beam: a.hBeamwidthDeg, color: a.color, disabled: () => S.locked, onInput: (v) => { S.set(p + 'azimuthDeg', v); azF.show(v); upd(); } });
      dirGroup = h('div.set-group', ui.section('Direction and beam', 'compass'),
        h('div.set-dir', h('div.set-dial-wrap', dial, h('div.set-help', 'Drag the pointer or use the arrow keys. The shaded wedge is the −3 dB beam.')),
          h('div.set-fields.set-fields--2',
            azF,
            F.number(p + 'tiltDeg', { label: 'Tilt', suffix: '°', min: -90, max: 90, step: 0.5, hint: 'Boresight elevation above the horizon. 0 = level; a few degrees up can help when the horizon is blocked.', onChange: upd }),
            F.number(p + 'hBeamwidthDeg', { label: 'Horizontal beamwidth', suffix: '°', min: 0, max: 360, step: 1, hint: 'The −3 dB beam angle left to right, from the data sheet. 0 = estimate from the gain.', onChange: upd }),
            F.number(p + 'vBeamwidthDeg', { label: 'Vertical beamwidth', suffix: '°', min: 0, max: 180, step: 1, hint: 'The −3 dB beam angle up and down. 0 = estimate from the gain.', onChange: upd }),
            F.number(p + 'frontToBackDb', { label: 'Front-to-back ratio', suffix: 'dB', min: 0, max: 60, step: 0.5, hint: 'How much weaker the antenna is directly behind. 0 = typical value (15 dB yagi, 20 dB patch).', onChange: upd }),
            a.type === 'yagi' || a.type === 'other' ? F.number(p + 'elements', { label: 'Elements', min: 0, max: 100, step: 1, hint: 'Yagi element count; used to estimate gain and beam shape when those are 0.', onChange: upd }) : null)),
        a.type === 'other' ? F.help('"Other" is modelled as a directional antenna only when a horizontal beamwidth below 360° is entered; otherwise as an omni.') : null);
    } else {
      dirGroup = h('div.set-group', ui.section('Vertical beam', 'compass'), h('div.set-fields.set-fields--2',
        F.number(p + 'vBeamwidthDeg', { label: 'Vertical beamwidth', suffix: '°', min: 0, max: 180, step: 1, hint: 'The −3 dB elevation beam angle. 0 = estimate from the gain (a higher-gain omni has a flatter beam).', onChange: upd }),
        F.number(p + 'tiltDeg', { label: 'Electrical up-tilt', suffix: '°', min: -90, max: 90, step: 0.5, hint: 'Elevation of the beam peak. Normally 0.', onChange: upd })));
    }

    preview();

    return [
      h('div.set-group', ui.section('Identity', 'antenna'),
        h('div.set-fields.set-fields--2', nameF, idF, colorF,
          F.select(p + 'channel', { label: 'Receiver channel', numeric: true, options: chanOpts, hint: hasRadarcape(S.draft, S.status) ? 'The RF input of the receiver this antenna is plugged into. Informational for a Radarcape 2CH: its data stream does not say which input heard a frame.' : 'The RF input of the receiver this antenna is plugged into. A single-input receiver (one RTL-SDR, one antenna) leaves this at "not applicable".' })),
        typeF),
      h('div.set-group', ui.section('Gain and mounting', 'signal'), h('div.set-fields.set-fields--2',
        F.number(p + 'gainDbi', { label: 'Peak gain', suffix: 'dBi', min: -20, max: 40, step: 0.1, hint: 'From the data sheet. 0 = estimate from type and beamwidth. dBd + 2.15 = dBi.', onChange: upd }),
        F.number(p + 'heightAglM', { label: 'Height above ground', kind: 'height', digits: 1, hint: 'Height of the feed point above the ground at the site. Height is what buys range: it moves the radio horizon out.', onChange: upd }),
        F.seg(p + 'polarization', { label: 'Polarisation', options: [{ value: 'vertical', label: 'Vertical' }, { value: 'horizontal', label: 'Horizontal' }], hint: 'ADS-B is vertically polarised; a horizontally mounted antenna loses about 20 dB.', onChange: upd }))),
      dirGroup,
      h('div.set-group', ui.section('Modelled pattern and range (live preview)', 'radar'),
        modelLine,
        h('div.set-preview',
          h('figure.set-fig', azEl, h('figcaption', 'Azimuth pattern at 3° elevation, north up: relative gain around the compass. An omni is a circle; a beam should point where your traffic is.')),
          h('figure.set-fig', elEl, h('figcaption', 'Elevation pattern through the beam: gain from below the horizon (left) to straight up (right). Good for ADS-B: the peak sits at 0-5°, where distant aircraft are.')),
          h('div.set-fig', horizonKv, h('div.set-help', 'Smooth-earth radio horizon in a standard atmosphere (k = 4/3) for the entered height plus the ground elevation. Hills, buildings and trees are not included - the Antenna page measures the real horizon.')))),
      h('div.set-group', ui.section('RF chain', 'bolt'), h('div.set-fields.set-fields--2',
        F.number(p + 'cableLossDb', { label: 'Cable loss', suffix: 'dB', min: 0, max: 60, step: 0.1, hint: 'Feed-line loss between the antenna and the first amplifier (or the receiver). Every dB here is a dB of lost sensitivity.' }),
        F.number(p + 'filterLossDb', { label: 'Filter loss', suffix: 'dB', min: 0, max: 30, step: 0.1, hint: 'Insertion loss of a 1090 MHz band-pass filter in front of the amplifier. 0 = none.' }),
        F.number(p + 'lnaGainDb', { label: 'LNA gain', suffix: 'dB', min: 0, max: 60, step: 0.5, hint: 'Gain of a mast-head / inline low-noise amplifier. 0 = no amplifier.' }),
        F.number(p + 'lnaNoiseFigDb', { label: 'LNA noise figure', suffix: 'dB', min: 0, max: 30, step: 0.1, hint: 'From the amplifier data sheet (typically 0.5-1.5 dB). Used by the link budget.' }))),
      h('div.set-group', ui.section('Notes', 'edit'), F.area(p + 'notes', { placeholder: 'Make, model, mounting, cable type ...' })),
    ];
  }

  S.scopeTitle = {};
  const items = list().map((a, i) => {
    const title = h('span', a.name || '(unnamed)');
    S.scopeTitle[i] = title;
    const m = resolvePattern(a);
    const feeds = (S.draft.feeds || []).filter((f) => f.antennaId && f.antennaId === a.id).length;
    return listItem(ui, {
      open: S.mem.open.ant === i, bad: hasProblem(i), first: i === 0, last: i === list().length - 1, full: list().length >= MAX_ANTENNAS,
      lead: h('i.set-swatch', { style: { background: a.color || 'var(--grey)' } }),
      title,
      sub: `${ANT_TYPES.find((t) => t.value === a.type)?.label || a.type} · ${units.num(m.peakGainDbi, 1)} dBi · ${units.height(a.heightAglM)} above ground${m.directional ? ` · pointing ${units.bearing(a.azimuthDeg)}` : ''}`,
      badges: [a.channel >= 0 ? ui.badge(`CH ${a.channel + 1}`, 'blue') : null, feeds ? ui.badge(`${feeds} ${feeds === 1 ? 'feed' : 'feeds'}`, 'cyan') : null, !savedIds.has(a.id) ? ui.badge('new', 'amber') : null],
      onToggle: () => { S.mem.open.ant = S.mem.open.ant === i ? -1 : i; S.rerender(); },
      onUp: () => { const l = list(); [l[i - 1], l[i]] = [l[i], l[i - 1]]; S.mem.open.ant = S.mem.open.ant === i ? i - 1 : S.mem.open.ant === i - 1 ? i : S.mem.open.ant; structural(); },
      onDown: () => { const l = list(); [l[i + 1], l[i]] = [l[i], l[i + 1]]; S.mem.open.ant = S.mem.open.ant === i ? i + 1 : S.mem.open.ant === i + 1 ? i : S.mem.open.ant; structural(); },
      onDuplicate: () => {
        const c = clone(a);
        c.name = (a.name || 'Antenna') + ' copy';
        c.id = uniqueId(c.name, list().map((x) => x.id), 'antenna');
        list().splice(i + 1, 0, c);
        S.mem.open.ant = i + 1;
        structural();
      },
      onRemove: async () => {
        const refs = (S.draft.feeds || []).filter((f) => f.antennaId === a.id);
        const ok = await ui.confirm({ title: `Remove antenna "${a.name}"?`, danger: true, confirmLabel: 'Remove',
          message: `The antenna is removed when you save.${refs.length ? ` ${refs.length} ${refs.length === 1 ? 'feed is' : 'feeds are'} assigned to it and will become "unknown / combined".` : ''} Statistics already stored under "${a.id}" stay in the database.` });
        if (!ok) return;
        for (const f of refs) f.antennaId = '';
        list().splice(i, 1);
        S.mem.open.ant = -1;
        structural();
      },
      body: () => body(i),
    });
  });

  const addBtn = ui.button({ label: 'Add antenna', icon: 'plus', variant: 'primary', disabled: list().length >= MAX_ANTENNAS, onClick: () => {
    list().push(newAntenna(list()));
    S.mem.open.ant = list().length - 1;
    structural();
  } });

  return [
    F.callout('info', 'info',
      h('b', 'Why this matters. '), 'AirView compares what it receives with what these antennas should be able to receive: the modelled pattern, the radio horizon for the entered height and the RF chain feed the antenna analysis, the link budget and the coverage forecast. Enter what you know; fields left at 0 are estimated.'),
    items.length ? h('div.set-list', items) : ui.empty({ icon: 'antenna', title: 'No antennas configured', text: 'Add the antenna(s) connected to your receiver so that coverage can be compared with a model.' }),
    h('div.row-wrap', addBtn, h('span.muted', `${list().length} of ${MAX_ANTENNAS}`)),
  ];
}

// =============================================================================================
// Receiver feeds
// =============================================================================================

export function renderFeeds(S, F) {
  const { ui, units } = S.ctx;
  const { h } = ui;
  const scope = S.scope;
  if (!Array.isArray(S.draft.feeds)) S.draft.feeds = [];
  const list = () => S.draft.feeds;
  if (S.mem.open.feed === undefined) S.mem.open.feed = list().length === 1 ? 0 : -1;
  const structural = () => { S.touch(); S.rerender(); };
  const savedById = new Map((S.saved.feeds || []).map((f) => [f.id, f]));
  const hasProblem = (i) => S.problems.some((p) => p.path.startsWith(`feeds.${i}.`));
  const typeLabel = (t) => FEED_TYPES.find((x) => x.value === t)?.label || (t === 'demo' ? 'Built-in simulator' : t);
  const addrOf = (f) => (f.type === 'json' ? f.url || '(no URL)' : f.type === 'demo' ? 'simulator' : `${f.host || '(no host)'}:${f.port}`);
  const radarcape = hasRadarcape(S.draft, S.status);

  // ---- live status, painted into per-feed slots ----
  const slots = new Map();   // feed id -> { dot, line, detail }
  const extra = h('div');
  function paintStatus() {
    const st = S.status;
    const byId = new Map((st?.feeds || []).map((f) => [f.id, f]));
    const now = S.ctx.live.serverNow();
    for (const [id, slot] of slots) {
      const f = list().find((x) => x.id === id);
      const fs = byId.get(id);
      const saved = savedById.get(id);
      let cls = 'off';
      let text;
      if (!saved) { text = 'New - connects after you save'; cls = 'new'; }
      else if (!fs) { text = st ? 'No status from the server yet' : 'Waiting for status…'; }
      else if (!fs.enabled) { text = st.demo && fs.type !== 'demo' ? 'Paused while demo mode is on' : 'Disabled'; }
      else if (fs.connected) { cls = 'ok'; text = `Connected · ${units.rate(fs.msgRate)} · connected ${units.ago(fs.since)}`; }
      else { cls = 'bad'; text = `Not connected · retrying · since ${units.ago(fs.since)}`; }
      if (saved && f && JSON.stringify(saved) !== JSON.stringify(f)) text += '  (status of the stored feed - your changes are not saved yet)';
      slot.dot.className = 'set-dot ' + cls;
      ui.setText(slot.line, text);
      if (slot.detail) {
        if (fs && saved) {
          slot.detail.set(0, fs.addr || '—');
          slot.detail.set(1, units.int(fs.frames));
          slot.detail.set(2, fs.bytes ? units.bytes(fs.bytes) : '—');
          slot.detail.set(3, fs.lastFrame ? units.age(Math.max(0, (now - fs.lastFrame) / 1000)) + ' ago' : 'never');
          // the GPS time row (Radarcape) is only there when a Radarcape is configured
          const gps = slot.detail.__gps;
          if (gps) slot.detail.set(4, fs.gpsTime ? 'yes (GPS-timestamped frames)' : 'no');
          slot.detail.set(gps ? 5 : 4, fs.info || fs.error || '—');
        }
        slot.detail.classList.toggle('hidden', !(fs && saved));
      }
    }
    const others = (st?.feeds || []).filter((f) => !list().some((x) => x.id === f.id) && !savedById.has(f.id));
    ui.mount(extra, others.map((f) => h('div.set-item.set-item--info', h('div.set-item-head', h('div.set-item-toggle',
      h('i.set-dot ' + (f.connected ? 'ok' : 'off')), h('div.set-item-title', f.name, h('div.set-item-sub', `${typeLabel(f.type)} · ${f.info || f.addr} · ${f.connected ? units.rate(f.msgRate) : 'not connected'}`)),
      ui.badge(f.type === 'demo' ? 'demo mode' : 'runtime', 'violet'))))));
  }
  scope.subscribe('status', (st) => { if (st) { S.status = st; paintStatus(); } }, { immediate: false });
  scope.every(5000, paintStatus);

  function body(i) {
    const f = list()[i];
    const p = `feeds.${i}.`;
    const ft = FEED_TYPES.find((t) => t.value === f.type);
    let autoId = !savedById.has(f.id);
    const typeOpts = FEED_TYPES.map((t) => ({ value: t.value, label: t.label }));
    if (f.type === 'demo') typeOpts.push({ value: 'demo', label: 'Built-in simulator' });
    const antOpts = [{ value: '', label: 'Unknown / combined (all antennas)' }, ...(S.draft.antennas || []).map((a) => ({ value: a.id, label: `${a.name} (${a.id})` }))];
    const idF = F.text(p + 'id', { label: 'ID', mono: true, maxLength: 32, hint: 'Per-feed counters and timing statistics are stored under this ID.', onChange: () => { autoId = false; } });
    const detail = ui.kv([['Address', '—'], ['Frames received', '—'], ['Bytes received', '—'], ['Last frame', '—'], ...(radarcape ? [['GPS time', '—']] : []), ['Receiver info', '—', { text: true }]]);
    detail.__gps = radarcape;
    const slot = slots.get(f.id);
    if (slot) slot.detail = detail;
    return [
      h('div.set-group', h('div.set-fields.set-fields--2',
        F.toggle(p + 'enabled', { label: 'Enabled', hint: 'A disabled feed stays configured but is not connected.' }),
        F.toggle(p + 'mlatOnly', { label: 'MLAT results only', hint: 'Switch on for a feed that carries only multilateration results (e.g. an mlat-client output): its positions are marked MLAT and kept out of the antenna analysis.' }),
        F.text(p + 'name', { label: 'Name', maxLength: 80, onChange: (v) => {
          if (autoId) { f.id = uniqueId(v, list().filter((x) => x !== f).map((x) => x.id), 'feed'); idF.control.value = f.id; S.touch(); }
        } }),
        idF,
        F.select(p + 'type', { label: 'Protocol', options: typeOpts, onChange: (v) => {
          const t = FEED_TYPES.find((x) => x.value === v);
          if (t && t.tcp && (!f.port || FEED_TYPES.some((x) => x.port === f.port))) f.port = t.port;
          structural();
        } }),
        F.select(p + 'antennaId', { label: 'Antenna', options: antOpts, hint: 'Attribute this feed\'s frames to one antenna (one receiver per antenna). With a single antenna, pick it or leave "unknown / combined".' + (radarcape ? ' Leave on "unknown / combined" for a Radarcape 2CH.' : '') }))),
      h('div.set-group', ui.section('Address', 'link'), h('div.set-fields.set-fields--2',
        f.type === 'json'
          ? F.text(p + 'url', { label: 'URL', mono: true, placeholder: 'http://127.0.0.1:8080/data/aircraft.json', hint: 'aircraft.json of readsb / dump1090 (http://host/data/aircraft.json)' + (radarcape ? ' or the Radarcape aircraftlist.json' : '') + '. Polled about once per second.' })
          : [
            F.text(p + 'host', { label: 'Host', mono: true, placeholder: '127.0.0.1  or  a host name', hint: 'Host name or IP address of the receiver; 127.0.0.1 when the decoder runs on this machine. "host:port" is accepted too.' }),
            F.number(p + 'port', { label: 'Port', min: 1, max: 65535, step: 1, hint: ft ? `Usual port for this protocol: ${ft.port}.` : '' }),
          ])),
      h('div.set-group', ui.section('Live status', 'signal'), detail, h('div.set-help', 'Updated every 5 seconds from the running engine. A healthy feed shows a steady message rate' + (radarcape ? '; a Radarcape also shows GPS time "yes".' : '.'))),
      h('div.set-group', ui.section('Notes', 'edit'), F.area(p + 'notes', { rows: 2 })),
    ];
  }

  const items = list().map((f, i) => {
    const dot = h('i.set-dot.off');
    const line = h('span');
    slots.set(f.id, { dot, line, detail: null });
    const ant = (S.draft.antennas || []).find((a) => a.id === f.antennaId);
    return listItem(ui, {
      open: S.mem.open.feed === i, bad: hasProblem(i), first: i === 0, last: i === list().length - 1, full: list().length >= MAX_FEEDS,
      lead: dot,
      title: h('span', f.name || '(unnamed)', h('span.set-item-addr.num', ` ${addrOf(f)}`)),
      sub: line,
      badges: [ui.badge(f.type, 'blue'), ant ? ui.badge(ant.name, 'cyan') : null, f.mlatOnly ? ui.badge('MLAT', 'mlat') : null, !f.enabled ? ui.badge('off', 'other') : null],
      onToggle: () => { S.mem.open.feed = S.mem.open.feed === i ? -1 : i; S.rerender(); },
      onUp: () => { const l = list(); [l[i - 1], l[i]] = [l[i], l[i - 1]]; S.mem.open.feed = -1; structural(); },
      onDown: () => { const l = list(); [l[i + 1], l[i]] = [l[i], l[i + 1]]; S.mem.open.feed = -1; structural(); },
      onDuplicate: () => {
        const c = clone(f);
        c.name = (f.name || 'Feed') + ' copy';
        c.id = uniqueId(c.name, list().map((x) => x.id), 'feed');
        list().splice(i + 1, 0, c);
        S.mem.open.feed = i + 1;
        structural();
      },
      onRemove: async () => {
        const ok = await ui.confirm({ title: `Remove feed "${f.name}"?`, danger: true, confirmLabel: 'Remove', message: 'The feed is disconnected and removed when you save. Data already received stays in the database.' });
        if (!ok) return;
        list().splice(i, 1);
        S.mem.open.feed = -1;
        structural();
      },
      body: () => body(i),
    });
  });

  const addBtn = ui.button({ label: 'Add feed', icon: 'plus', iconRight: 'chevron-down', variant: 'primary', disabled: list().length >= MAX_FEEDS, onClick: () => {
    ui.menu(addBtn, FEED_PRESETS.map((pr) => ({ label: pr.label, icon: 'plus', onClick: () => {
      list().push(newFeed(list(), pr.feed));
      S.mem.open.feed = list().length - 1;
      structural();
    } })));
  } });

  paintStatus();

  // First start: the factory placeholder (a Radarcape named "radarcape") never connected. Offer the usual
  // RTL-SDR setup - readsb / dump1090 on this machine - in one click.
  const phIdx = list().findIndex((f) => isFactoryDefaultFeed(f) && f.enabled);
  const phStatus = phIdx >= 0 ? (S.status?.feeds || []).find((x) => x.id === list()[phIdx].id) : null;
  const placeholder = phIdx >= 0 && !phStatus?.connected && !S.status?.demo;
  const useLocal = () => {
    const f = list()[phIdx];
    if (!f) return;
    const others = list().filter((x) => x !== f).map((x) => x.id);
    Object.assign(f, { ...LOCAL_DECODER_FEED, id: uniqueId(LOCAL_DECODER_FEED.id, others, 'feed') });
    S.mem.open.feed = phIdx;
    structural();
    ui.toast.info('Feed changed to readsb / dump1090 on 127.0.0.1:30005 - save to connect.');
  };

  return [
    placeholder ? F.callout('warn', 'antenna', h('div.grow',
      h('b', 'Where does your receiver deliver its data? '),
      'The factory default feed looks for a Radarcape under the host name "radarcape". With an RTL-SDR dongle and readsb or dump1090 on this machine (the usual Raspberry Pi set-up) switch it to their Beast output 127.0.0.1:30005; for another receiver edit the host and port below or add a feed from the presets.',
      h('div.row-wrap.set-actions', ui.button({ label: 'Use readsb / dump1090 on this machine', icon: 'antenna', variant: 'primary', size: 'sm', disabled: S.locked, onClick: useLocal })))) : null,
    S.status?.demo ? F.callout('info', 'play', h('b', 'Demo mode is on. '), 'The built-in simulator supplies the traffic and the configured feeds are paused. Switch demo mode off (Settings > Demo mode) to connect to your receiver.') : null,
    items.length ? h('div.set-list', items) : ui.empty({ icon: 'signal', title: 'No feeds configured', text: 'Add a feed to tell AirView where your receiver delivers its data. Without one only demo mode shows traffic.' }),
    extra,
    h('div.row-wrap', addBtn, h('span.muted', `${list().length} of ${MAX_FEEDS} · presets fill in the usual port`)),
    ui.card({ title: 'Presets', icon: 'list', body: h('dl.set-presets', FEED_PRESETS.filter((pr) => pr.feed.host !== '' && (!pr.radarcape || radarcape)).map((pr) => [h('dt', pr.label), h('dd', pr.hint)])) }),
    radarcape ? F.callout('info', 'antenna',
      h('b', 'Radarcape 2CH and antenna diversity. '),
      'The EVO 2CH receives on two antennas but merges them into one data stream: none of its outputs says which antenna heard a frame. A Radarcape feed therefore always stays "unknown / combined", and AirView infers each antenna\'s share from the combined picture (measured pattern against the modelled yagi and vertical) on the Antenna page. Exact per-antenna statistics need one receiver - and one feed - per antenna; assign each of those feeds to its antenna here.') : null,
  ];
}
