// map/measure.js - distance / bearing measuring tool of the Live Map.
//
// Two modes: "Two points" (click A, then B) and "From receiver" (click one point; also fetches the
// terrain line-of-sight profile from the station's antenna to that point at a chosen altitude via
// /api/terrain/profile). The line is the great circle; the label shows distance and initial bearing.

import { ui, h, setText } from '../ui.js';
import { units } from '../units.js';
import { geo } from '../geo.js';
import { sameWorld } from './predict.js';

const fc = (features = []) => ({ type: 'FeatureCollection', features });

/** Great-circle polyline [[lon, lat], ...] between two points (longitudes unwrapped). */
export function greatCircle(lat1, lon1, lat2, lon2) {
  const d = geo.distanceKm(lat1, lon1, lat2, lon2);
  const brg = geo.bearing(lat1, lon1, lat2, lon2);
  const steps = Math.max(2, Math.min(128, Math.ceil(d / 20)));
  const pts = [[lon1, lat1]];
  for (let i = 1; i < steps; i++) pts.push(sameWorld(geo.destination(lat1, lon1, brg, (d * i) / steps), pts[pts.length - 1][0]));
  pts.push(sameWorld([lon2, lat2], pts[pts.length - 1][0]));
  return pts;
}

/**
 * @param {object} ctx page context
 * @param {{ host: HTMLElement, setData(id, data), rxPos(): Promise<{lat, lon}|null>, onClose(), setCursor(c) }} hooks
 */
export function createMeasure(ctx, hooks) {
  const prefs = ui.prefs('airview.map.measure', { mode: 'two', altFt: 10000 });
  let mode = prefs.get().mode === 'rx' ? 'rx' : 'two';
  let a = null; // {lat, lon}
  let b = null;
  let hover = null;
  let rx = null;
  let chart = null;
  let seq = 0;
  let active = false;

  const resultEl = h('div.mapv-measure-result');
  const hintEl = h('div.mapv-measure-hint');
  const profileEl = h('div.mapv-measure-profile.hidden');
  const altInput = ui.input({ type: 'number', size: 'sm', value: Math.round(units.value('alt', prefs.get().altFt)), suffix: units.label('alt'), min: 0, step: 100, onChange: (v) => {
    const ft = units.toBase('alt', v);
    if (typeof ft === 'number' && ft >= 0 && ft <= 60000) { prefs.set({ altFt: ft }); loadProfile(); }
  } });
  const altField = ui.field({ label: 'Target altitude', row: true, control: altInput });
  const modeSeg = ui.segmented({ block: true, value: mode, options: [{ value: 'two', label: 'Two points' }, { value: 'rx', label: 'From receiver' }], onChange: (v) => { mode = v; prefs.set({ mode: v }); reset(); } });
  const el = h('div.mapv-measure.hidden', { role: 'dialog', 'aria-label': 'Measure' },
    h('div.mapv-measure-head', h('b', 'Measure'), h('div.spacer'),
      ui.button({ icon: 'refresh', size: 'sm', variant: 'ghost', tip: 'Start again', onClick: () => reset() }),
      ui.button({ icon: 'x', size: 'sm', variant: 'ghost', tip: 'Close (Esc)', onClick: () => hooks.onClose() })),
    modeSeg, hintEl, resultEl, altField, profileEl);
  hooks.host.appendChild(el);

  function draw() {
    const feats = [];
    const pts = [];
    if (a) pts.push(a);
    const end = b || (a && hover && mode === 'two' ? hover : null);
    if (a) feats.push({ type: 'Feature', properties: { kind: 'pt', text: mode === 'rx' ? '' : 'A' }, geometry: { type: 'Point', coordinates: [a.lon, a.lat] } });
    if (a && end) {
      const line = greatCircle(a.lat, a.lon, end.lat, end.lon);
      feats.push({ type: 'Feature', properties: { kind: 'line', tmp: b ? 0 : 1 }, geometry: { type: 'LineString', coordinates: line } });
      const d = geo.distanceKm(a.lat, a.lon, end.lat, end.lon);
      const brg = geo.bearing(a.lat, a.lon, end.lat, end.lon);
      const mid = line[Math.floor(line.length / 2)];
      feats.push({ type: 'Feature', properties: { kind: 'label', text: `${units.dist(d)}\n${units.deg(brg)}` }, geometry: { type: 'Point', coordinates: mid } });
      if (b) feats.push({ type: 'Feature', properties: { kind: 'pt', text: mode === 'rx' ? '' : 'B' }, geometry: { type: 'Point', coordinates: line[line.length - 1] } });
    }
    hooks.setData('measure', fc(feats));
  }

  function renderResult() {
    const end = b;
    if (mode === 'rx' && !rx) { setText(hintEl, 'The receiver location is not known yet.'); ui.clear(resultEl); return; }
    if (!a || !end) {
      setText(hintEl, mode === 'rx' ? 'Click a point on the map to measure from the receiver and show the terrain profile.' : a ? 'Click the second point.' : 'Click the first point on the map.');
      ui.clear(resultEl);
      return;
    }
    setText(hintEl, mode === 'rx' ? 'Click elsewhere to measure another point.' : 'Click again to start a new measurement.');
    const d = geo.distanceKm(a.lat, a.lon, end.lat, end.lon);
    const brg = geo.bearing(a.lat, a.lon, end.lat, end.lon);
    const back = geo.bearing(end.lat, end.lon, a.lat, a.lon);
    ui.mount(resultEl, ui.kv([
      ['Distance', units.dist(d)],
      ['Bearing', units.bearing(brg)],
      ['Reverse bearing', units.bearing(back)],
      [mode === 'rx' ? 'Point' : 'B', units.latlon(end.lat, end.lon)],
    ]));
  }

  async function loadProfile() {
    const my = ++seq;
    if (mode !== 'rx' || !a || !b) { profileEl.classList.add('hidden'); return; }
    profileEl.classList.remove('hidden');
    if (chart) { chart.dispose(); chart = null; }
    ui.mount(profileEl, ui.loading('Loading terrain profile…', { compact: true }));
    try {
      const r = await ctx.api.get('/api/terrain/profile', { params: { lat: b.lat.toFixed(5), lon: b.lon.toFixed(5), alt: Math.round(prefs.get().altFt) }, signal: ctx.signal, timeout: 70000 });
      if (my !== seq) return;
      const prof = Array.isArray(r?.profile) ? r.profile : [];
      if (prof.length < 2) { ui.mount(profileEl, ui.empty({ icon: 'mountain', title: 'No profile', text: 'Not enough terrain data for this path yet.', compact: true })); return; }
      renderProfile(r, prof);
    } catch (e) {
      if (e.aborted || my !== seq) return;
      const text = e.status === 503 ? 'The terrain analysis has not run yet.' : e.status === 409 ? 'Terrain analysis is switched off in Settings.' : e.message;
      ui.mount(profileEl, ui.error(text, { title: 'Terrain profile unavailable', compact: true, retry: () => loadProfile() }));
    }
  }

  function renderProfile(r, prof) {
    const chartEl = h('div.chart.mapv-measure-chart');
    const hv = (m) => units.value('height', m);
    const dv = (km) => units.value('dist', km);
    const terrain = prof.map((p) => [dv(p.distKm), hv((p.terrainM || 0) + (p.bulgeM || 0))]);
    const ray = prof.map((p) => [dv(p.distKm), hv(p.rayM)]);
    const fres = prof.map((p) => [dv(p.distKm), hv(p.rayM - 0.6 * (p.fresnelM || 0))]);
    const clear = !!r.clear;
    const summary = h('div.mapv-measure-los',
      ui.badge(clear ? 'Line of sight clear' : 'Blocked by terrain', clear ? 'green' : 'red'),
      h('span.muted', `elev ${units.num(r.elevationDeg, 2, 2)}° · clearance ${units.height(r.minClearanceM)}${r.diffractionDb > 0 ? ` · diffraction ${units.db(r.diffractionDb)}` : ''}`));
    ui.mount(profileEl, summary, chartEl);
    const fmtH = (v) => units.num(v, 0) + ' ' + units.label('height');
    chart = ctx.charts.create(chartEl, {
      grid: { left: 46, right: 10, top: 14, bottom: 26 },
      tooltip: { trigger: 'axis', valueFormatter: (v) => fmtH(v) },
      xAxis: { type: 'value', min: 0, max: terrain[terrain.length - 1][0], axisLabel: { formatter: (v) => units.num(v, 0) }, name: units.label('dist'), nameLocation: 'end', nameGap: 4 },
      yAxis: { type: 'value', scale: true, axisLabel: { formatter: (v) => units.num(v, 0) } },
      series: [
        { name: 'Terrain + earth bulge', type: 'line', data: terrain, showSymbol: false, lineStyle: { width: 1, color: '#c08a4a' }, itemStyle: { color: '#c08a4a' }, areaStyle: { color: 'rgba(192,138,74,0.35)' } },
        { name: 'Line of sight', type: 'line', data: ray, showSymbol: false, lineStyle: { width: 1.6, color: clear ? '#6aa8ff' : '#ff4d5e' }, itemStyle: { color: clear ? '#6aa8ff' : '#ff4d5e' } },
        { name: '60 % Fresnel', type: 'line', data: fres, showSymbol: false, lineStyle: { width: 1, type: 'dashed', color: '#22d3ee' }, itemStyle: { color: '#22d3ee' } },
      ],
    });
  }

  function reset() {
    seq++;
    b = null;
    hover = null;
    a = mode === 'rx' && rx ? { ...rx } : null;
    altField.classList.toggle('hidden', mode !== 'rx');
    profileEl.classList.add('hidden');
    if (chart) { chart.dispose(); chart = null; }
    ui.clear(profileEl);
    draw();
    renderResult();
  }

  return {
    el,
    get active() { return active; },
    async open() {
      active = true;
      el.classList.remove('hidden');
      hooks.setCursor('crosshair');
      reset();
      rx = await hooks.rxPos();
      if (!active) return;
      reset();
    },
    close() {
      active = false;
      seq++;
      el.classList.add('hidden');
      hooks.setCursor('');
      if (chart) { chart.dispose(); chart = null; }
      a = b = hover = null;
      hooks.setData('measure', fc());
    },
    /** Map click while active. */
    click(lngLat) {
      const p = { lat: lngLat.lat, lon: ((((lngLat.lng + 180) % 360) + 360) % 360) - 180 };
      if (mode === 'rx') {
        if (!rx) return;
        a = { ...rx };
        b = p;
        loadProfile();
      } else if (!a || b) { a = p; b = null; } else { b = p; }
      draw();
      renderResult();
    },
    move(lngLat) {
      if (!active || mode !== 'two' || !a || b) return;
      hover = { lat: lngLat.lat, lon: lngLat.lng };
      draw();
    },
    refresh() {
      altInput.input.value = Math.round(units.value('alt', prefs.get().altFt));
      const sfx = altInput.querySelector('.input-suffix');
      if (sfx) sfx.textContent = units.label('alt');
      draw();
      renderResult();
      if (b && mode === 'rx') loadProfile();
    },
    destroy() { if (chart) chart.dispose(); el.remove(); },
  };
}
