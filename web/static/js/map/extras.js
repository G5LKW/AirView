// map/extras.js - data loaders for the Live Map's optional overlays (tar1090 / VRS parity):
//   range   - measured range outline      /api/antenna/coverage.geojson   (all altitudes, max + robust)
//   terrain - theoretical coverage rings  /api/terrain/coverage.geojson   (one ring per altitude, los|radio)
//   airports- airports, runways, extended centrelines  /api/navdata/airports?bbox&format=geojson (by view)
//   heat    - all-time position heat map  /api/antenna/heat
//   prox    - proximity pairs             /api/prediction/proximity       (lines are drawn by map.js)
//   acas    - TCAS resolution advisories  /api/acas/events?hours=24       (where each RA started)
//   winds   - winds / temperature aloft   /api/met/grid?level=FL&minutes=60 (Mode S EHS, one flight level)
// Every loader keeps a status note { state: 'off'|'loading'|'ok'|'empty'|'error', text } that the
// layers panel and the legend show, so each overlay has loading / empty / error states.

import { units } from '../units.js';
import { net } from '../net/store.js';

const fc = (features = []) => ({ type: 'FeatureCollection', features });
export const EMPTY_FC = fc();

const RANGE_REFRESH_MS = 5 * 60000;
const TERRAIN_REFRESH_MS = 10 * 60000;
const HEAT_REFRESH_MS = 10 * 60000;
const PROX_REFRESH_MS = 5000;
const ACAS_REFRESH_MS = 60000;
const WINDS_REFRESH_MS = 120000;

/** Airport size threshold for a zoom level (fewer, bigger airports when zoomed out). */
export function airportMinSize(z) { return z < 6.5 ? 'large' : z < 8.5 ? 'medium' : 'small'; }
export const AIRPORT_MIN_ZOOM = 5;

/**
 * @param {object} ctx page context
 * @param {{ setData(id, data), map: () => any, on(key): boolean, onNote(): void }} host
 */
export function createExtras(ctx, host) {
  const notes = new Map();
  const timers = new Map();
  const seqs = new Map();
  let terrainRaw = null;
  let prox = { pairs: [], note: '' };
  let stopped = false;

  const note = (key, state, text = '') => {
    const prev = notes.get(key);
    if (prev && prev.state === state && prev.text === text) return;
    notes.set(key, { state, text });
    host.onNote(key);
  };
  const schedule = (key, ms, fn) => { clearTimeout(timers.get(key)); if (!stopped) timers.set(key, setTimeout(fn, ms)); };
  const nextSeq = (key) => { const s = (seqs.get(key) || 0) + 1; seqs.set(key, s); return s; };
  const current = (key, s) => !stopped && seqs.get(key) === s;
  const errText = (e) => (e.status === 503 ? 'Not computed yet' : e.status === 409 ? 'Disabled in Settings' : e.network ? 'Server not reachable' : e.message);

  // ---- measured range outline ----
  async function loadRange() {
    const s = nextSeq('range');
    if (!host.on('range')) { host.setData('range', EMPTY_FC); note('range', 'off'); return; }
    if (!notes.get('range') || notes.get('range').state === 'off') note('range', 'loading', 'Loading…');
    try {
      const j = await ctx.api.get('/api/antenna/coverage.geojson', { signal: ctx.signal, timeout: 30000 });
      if (!current('range', s)) return;
      const feats = (j?.features || []).filter((f) => f?.properties?.band === -1 && f.geometry && hasExtent(f.geometry)).map(stripCentre).filter(Boolean);
      host.setData('range', fc(feats));
      note('range', feats.length ? 'ok' : 'empty', feats.length ? '' : 'Not enough data yet');
    } catch (e) {
      if (e.aborted || !current('range', s)) return;
      note('range', 'error', errText(e));
    }
    schedule('range', RANGE_REFRESH_MS, loadRange);
  }

  // ---- terrain theoretical coverage ----
  function labelTerrain() {
    if (!terrainRaw) return EMPTY_FC;
    return fc(terrainRaw.map((f) => ({ ...f, properties: { ...f.properties, label: units.alt(f.properties.altFt, { fl: true }) } })));
  }
  async function loadTerrain(kind) {
    const s = nextSeq('terrain');
    if (!host.on('terrain')) { host.setData('terrain', EMPTY_FC); note('terrain', 'off'); return; }
    note('terrain', 'loading', 'Loading…');
    try {
      const j = await ctx.api.get('/api/terrain/coverage.geojson', { params: { kind: kind || 'los' }, signal: ctx.signal, timeout: 30000 });
      if (!current('terrain', s)) return;
      terrainRaw = (j?.features || []).filter((f) => f?.geometry && f.properties);
      host.setData('terrain', labelTerrain());
      note('terrain', terrainRaw.length ? 'ok' : 'empty', terrainRaw.length ? '' : 'No coverage rings');
    } catch (e) {
      if (e.aborted || !current('terrain', s)) return;
      terrainRaw = null;
      host.setData('terrain', EMPTY_FC);
      note('terrain', e.status === 503 ? 'empty' : 'error', e.status === 503 ? 'Terrain analysis not ready yet' : errText(e));
      if (e.status === 503) { schedule('terrain', 60000, () => loadTerrain(kind)); return; }
    }
    schedule('terrain', TERRAIN_REFRESH_MS, () => loadTerrain(kind));
  }

  // ---- airports (by view) ----
  let airportsNoData = null; // null = unknown, true = nothing imported
  async function loadAirports() {
    const s = nextSeq('airports');
    const map = host.map();
    if (!host.on('airports') || !map) { host.setData('airports', EMPTY_FC); note('airports', 'off'); return; }
    const z = map.getZoom();
    if (z < AIRPORT_MIN_ZOOM) { host.setData('airports', EMPTY_FC); note('airports', 'ok', 'Zoom in to see airports'); return; }
    const b = map.getBounds();
    let w = b.getWest();
    let e = b.getEast();
    if (e - w >= 360) { w = -180; e = 180; }
    const wrap = (x) => ((((x + 180) % 360) + 360) % 360) - 180;
    const bbox = [Math.max(-90, b.getSouth()), wrap(w), Math.min(90, b.getNorth()), wrap(e)].map((v) => v.toFixed(3)).join(',');
    try {
      const j = await ctx.api.get('/api/navdata/airports', {
        params: { bbox, minSize: airportMinSize(z), runways: z >= 8 ? 1 : undefined, format: 'geojson', limit: 1500 }, signal: ctx.signal, timeout: 20000,
      });
      if (!current('airports', s)) return;
      const feats = j?.features || [];
      host.setData('airports', fc(feats));
      // the dataset status is an admin route in network mode: other viewers simply see "No airports in view"
      if (!feats.length && airportsNoData === null && !(net.aggregator && net.scope !== 'admin')) {
        try {
          const st = await ctx.api.get('/api/navdata/status', { signal: ctx.signal, timeout: 8000 });
          const ds = (st?.datasets || []).find((d) => d.dataset === 'airports');
          airportsNoData = !ds || !ds.rows;
        } catch { airportsNoData = false; }
      } else if (feats.length) airportsNoData = false;
      if (!current('airports', s)) return;
      note('airports', feats.length ? 'ok' : 'empty', feats.length ? '' : airportsNoData ? 'Airport data not imported (Settings › Navigation data)' : 'No airports in view');
    } catch (err) {
      if (err.aborted || !current('airports', s)) return;
      note('airports', 'error', errText(err));
    }
  }

  // ---- heat map ----
  async function loadHeat() {
    const s = nextSeq('heat');
    if (!host.on('heat')) { host.setData('heat', EMPTY_FC); note('heat', 'off'); return; }
    if (!notes.get('heat') || notes.get('heat').state === 'off') note('heat', 'loading', 'Loading…');
    try {
      const j = await ctx.api.get('/api/antenna/heat', { params: { maxCells: 20000 }, signal: ctx.signal, timeout: 30000 });
      if (!current('heat', s)) return;
      const len = Array.isArray(j?.lat) ? j.lat.length : 0;
      const max = Math.max(1, j?.maxCount || 1);
      const lmax = Math.log1p(max);
      const feats = [];
      for (let i = 0; i < len; i++) {
        const c = j.count[i];
        if (!(c > 0)) continue;
        feats.push({ type: 'Feature', properties: { w: +(Math.log1p(c) / lmax).toFixed(3) }, geometry: { type: 'Point', coordinates: [j.lon[i], j.lat[i]] } });
      }
      host.setData('heat', fc(feats));
      note('heat', feats.length ? 'ok' : 'empty', feats.length ? `${units.si(j.positions)} positions` : 'Not enough data yet');
    } catch (e) {
      if (e.aborted || !current('heat', s)) return;
      note('heat', 'error', errText(e));
    }
    schedule('heat', HEAT_REFRESH_MS, loadHeat);
  }

  // ---- TCAS resolution advisories ----
  async function loadACAS() {
    const s = nextSeq('acas');
    if (!host.on('acas')) { host.setData('acas', EMPTY_FC); note('acas', 'off'); return; }
    if (!notes.get('acas') || notes.get('acas').state === 'off') note('acas', 'loading', 'Loading…');
    try {
      const j = await ctx.api.get('/api/acas/events', { params: { hours: 24, limit: 2000 }, signal: ctx.signal, timeout: 20000 });
      if (!current('acas', s)) return;
      const feats = (j?.events || []).filter((e) => typeof e.lat === 'number' && typeof e.lon === 'number').map((e) => ({
        type: 'Feature', geometry: { type: 'Point', coordinates: [e.lon, e.lat] },
        properties: { icao: e.icao, t: `${e.callsign || e.icao} ${units.clock(e.start, { seconds: false })}\n${(e.advisories || []).join(' › ')}` },
      }));
      host.setData('acas', fc(feats));
      note('acas', feats.length ? 'ok' : 'empty', feats.length ? `${feats.length} in 24 h` : 'No resolution advisories in the last 24 h');
    } catch (e) {
      if (e.aborted || !current('acas', s)) return;
      note('acas', 'error', e.status === 401 || e.status === 403 ? 'Log in to see them' : errText(e));
    }
    schedule('acas', ACAS_REFRESH_MS, loadACAS);
  }

  // ---- winds / temperature aloft (gridded EHS observations) ----
  async function loadWinds(fl) {
    const s = nextSeq('winds');
    if (!host.on('winds')) { host.setData('winds', EMPTY_FC); note('winds', 'off'); return; }
    const level = fl || 300;
    if (!notes.get('winds') || notes.get('winds').state === 'off') note('winds', 'loading', 'Loading…');
    try {
      const j = await ctx.api.get('/api/met/grid', { params: { level, minutes: 60, cell: 0.5 }, signal: ctx.signal, timeout: 30000 });
      if (!current('winds', s)) return;
      const feats = [];
      for (const c of j?.cells || []) {
        const kt = typeof c.windKt === 'number' ? c.windKt : null;
        const label = [kt !== null ? `${String(Math.round(c.windDir)).padStart(3, '0')}/${Math.round(kt)}` : '', typeof c.tempC === 'number' ? `${Math.round(c.tempC)}°` : ''].filter(Boolean).join(' ');
        feats.push({ type: 'Feature', geometry: { type: 'Point', coordinates: [c.lon, c.lat] },
          properties: { t: label, d: typeof c.isaDevC === 'number' ? c.isaDevC : 0, n: c.nAc } });
        if (kt !== null && kt >= 3) {
          // shaft towards where the wind blows (dir + 180), length grows with speed up to the cell size
          const len = (j.cellDeg || 0.5) * 0.45 * Math.min(1, 0.25 + kt / 120);
          const r = ((c.windDir + 180) * Math.PI) / 180;
          const k = Math.cos((c.lat * Math.PI) / 180) || 1;
          const tip = [c.lon + (len * Math.sin(r)) / k, c.lat + len * Math.cos(r)];
          const wing = (a) => [tip[0] - (len * 0.3 * Math.sin(r + a)) / k, tip[1] - len * 0.3 * Math.cos(r + a)];
          feats.push({ type: 'Feature', geometry: { type: 'MultiLineString', coordinates: [[[c.lon, c.lat], tip], [wing(0.5), tip, wing(-0.5)]] }, properties: { kt } });
        }
      }
      host.setData('winds', fc(feats));
      const n = (j?.cells || []).length;
      note('winds', n ? 'ok' : 'empty', n ? `FL${level}: ${n} cells, ${j.samples} observations (60 min)` : `No observations near FL${level} in the last hour`);
    } catch (e) {
      if (e.aborted || !current('winds', s)) return;
      note('winds', 'error', e.status === 401 || e.status === 403 ? 'Log in to see them' : errText(e));
    }
    schedule('winds', WINDS_REFRESH_MS, () => loadWinds(fl));
  }

  // ---- proximity pairs ----
  async function loadProx() {
    const s = nextSeq('prox');
    if (!host.on('prox')) { prox = { pairs: [], note: '' }; note('prox', 'off'); host.onNote('prox-data'); return; }
    if (document.visibilityState === 'visible') {
      try {
        const j = await ctx.api.get('/api/prediction/proximity', { params: { km: 40, horizon: 120 }, signal: ctx.signal, timeout: 10000 });
        if (!current('prox', s)) return;
        prox = { pairs: Array.isArray(j?.pairs) ? j.pairs : [], note: j?.note || '' };
        note('prox', 'ok', prox.pairs.length ? `${prox.pairs.length} pair${prox.pairs.length === 1 ? '' : 's'}` : 'No close pairs right now');
        host.onNote('prox-data');
      } catch (e) {
        if (e.aborted || !current('prox', s)) return;
        prox = { pairs: [], note: '' };
        note('prox', 'error', errText(e));
        host.onNote('prox-data');
      }
    }
    schedule('prox', PROX_REFRESH_MS, loadProx);
  }

  return {
    notes,
    get proximity() { return prox; },
    range: loadRange,
    terrain: loadTerrain,
    airports: loadAirports,
    heat: loadHeat,
    prox: loadProx,
    acas: loadACAS,
    winds: loadWinds,
    /** Re-label unit-dependent data (terrain altitude labels). */
    relabel() { if (terrainRaw) host.setData('terrain', labelTerrain()); },
    /** Push every layer's last data again (after a base-map switch dropped the sources). */
    reloadAll(kind) { loadRange(); loadTerrain(kind); loadAirports(); loadHeat(); loadProx(); loadACAS(); loadWinds(host.windsFL ? host.windsFL() : 300); },
    stop() { stopped = true; for (const t of timers.values()) clearTimeout(t); timers.clear(); },
  };
}

/**
 * Bearings without any received position come back as vertices on the receiver itself, which draws
 * spokes to the centre. Drop those vertices (the most frequent coordinate when it repeats) so the
 * outline only joins the bearings that have data.
 */
function stripCentre(f) {
  const ring = f.geometry.type === 'Polygon' ? f.geometry.coordinates[0] : null;
  if (!ring) return f;
  const counts = new Map();
  let best = null;
  let bestN = 0;
  for (const p of ring) {
    const k = p[0].toFixed(5) + ',' + p[1].toFixed(5);
    const c = (counts.get(k) || 0) + 1;
    counts.set(k, c);
    if (c > bestN) { bestN = c; best = k; }
  }
  if (bestN < 3) return f;
  const kept = ring.filter((p) => p[0].toFixed(5) + ',' + p[1].toFixed(5) !== best);
  if (kept.length < 3) return null;
  kept.push([kept[0][0], kept[0][1]]);
  return { ...f, geometry: { type: 'Polygon', coordinates: [kept] } };
}

/** True when a polygon is not degenerate (the backend returns rings collapsed onto the receiver before data exists). */
function hasExtent(g) {
  const ring = g.type === 'Polygon' ? g.coordinates?.[0] : g.type === 'MultiPolygon' ? g.coordinates?.[0]?.[0] : null;
  if (!Array.isArray(ring) || ring.length < 4) return false;
  const [x0, y0] = ring[0];
  return ring.some((p) => Math.abs(p[0] - x0) > 1e-4 || Math.abs(p[1] - y0) > 1e-4);
}
