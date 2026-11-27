// rf/gnssmap.js - small MapLibre map of the GNSS interference index per polar cell (10 deg x 50 km) around the
// station. MapLibre loads lazily; the base map and tile-cache rewrite are the Live Map's.

import { BASEMAPS, styleFor, makeTransformRequest } from '../map/basemaps.js';

const BLANK_STYLE = { version: 8, sources: {}, layers: [{ id: 'bg', type: 'background', paint: { 'background-color': '#050c1b' } }] };
const EMPTY = { type: 'FeatureCollection', features: [] };
const SECTOR_DEG = 10;
const RING_KM = 50;
export const INDEX_RAMP = [[0, '#34d399'], [5, '#a3e635'], [10, '#fde047'], [20, '#fb923c'], [35, '#ff4d5e']];

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

const okNum = (v) => typeof v === 'number' && Number.isFinite(v);

/** Annular sector polygon of one cell, [lon, lat] ring. */
function cellRing(geo, lat, lon, sector, ring) {
  const a0 = sector * SECTOR_DEG;
  const r0 = ring * RING_KM;
  const r1 = r0 + RING_KM;
  const pts = [];
  for (let a = a0; a <= a0 + SECTOR_DEG + 1e-9; a += 2) pts.push(geo.destination(lat, lon, a, r1));
  for (let a = a0 + SECTOR_DEG; a >= a0 - 1e-9; a -= 2) pts.push(r0 > 0 ? geo.destination(lat, lon, a, r0) : [lon, lat]);
  pts.push(pts[0]);
  return pts;
}

/**
 * @param {HTMLElement} host sized element
 * @param ctx page context
 * @param {{ describe: (cell) => string }} opts
 * @returns {Promise<{ set(receiver, cells), resize(), remove() }>}
 */
export async function createGnssMap(host, ctx, opts = {}) {
  const maplibregl = await loadLib(ctx.ui);
  const map = new maplibregl.Map({
    container: host, style: styleFor(BASEMAPS[0]), center: [8, 50], zoom: 5, minZoom: 2, maxZoom: 11, maxPitch: 0,
    dragRotate: false, pitchWithRotate: false, touchPitch: false, fadeDuration: 0, attributionControl: false,
    transformRequest: makeTransformRequest(() => !!ctx.live.status?.tileCache),
  });
  map.touchZoomRotate.disableRotation();
  map.keyboard.disableRotation();
  map.addControl(new maplibregl.AttributionControl({ compact: true }), 'bottom-right');
  map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');

  let ready = false;
  let removed = false;
  let current = null;
  let fittedKey = '';
  const popup = new maplibregl.Popup({ closeButton: false, closeOnClick: false, offset: 8, className: 'ac-popup' });

  function install() {
    if (removed) return;
    for (const id of ['rf-cells', 'rf-rx']) if (!map.getSource(id)) map.addSource(id, { type: 'geojson', data: EMPTY });
    const add = (layer) => { if (!map.getLayer(layer.id)) map.addLayer(layer); };
    const ramp = ['interpolate', ['linear'], ['get', 'index']];
    for (const [v, c] of INDEX_RAMP) ramp.push(v, c);
    add({ id: 'rf-cells-fill', type: 'fill', source: 'rf-cells', paint: { 'fill-color': ramp, 'fill-opacity': ['get', 'op'] } });
    add({ id: 'rf-cells-line', type: 'line', source: 'rf-cells', paint: { 'line-color': '#02040a', 'line-width': 0.6, 'line-opacity': 0.7 } });
    add({ id: 'rf-rx-glow', type: 'circle', source: 'rf-rx', paint: { 'circle-radius': 14, 'circle-color': '#2f7dff', 'circle-opacity': 0.3, 'circle-blur': 0.7 } });
    add({ id: 'rf-rx', type: 'circle', source: 'rf-rx', paint: { 'circle-radius': 4.5, 'circle-color': '#ffffff', 'circle-stroke-color': '#2f7dff', 'circle-stroke-width': 3 } });
    ready = true;
    if (current) apply();
  }
  map.on('style.load', install);
  const fallback = setTimeout(() => { if (!ready && !removed) map.setStyle(BLANK_STYLE); }, 6000);
  map.on('error', (e) => { if (!ready) console.warn('[rf] gnss map:', e && e.error ? e.error.message : e); });

  map.on('mousemove', 'rf-cells-fill', (e) => {
    const feat = e.features && e.features[0];
    if (!feat || !current || !opts.describe) return;
    const c = current.cells[feat.properties.i];
    if (!c) return;
    popup.setLngLat(e.lngLat).setText(opts.describe(c)).addTo(map);
  });
  map.on('mouseleave', 'rf-cells-fill', () => popup.remove());

  function apply() {
    const { rx, cells } = current;
    const maxTot = Math.max(1, ...cells.map((c) => c.tot || 0));
    const feats = [];
    cells.forEach((c, i) => {
      if (!Number.isInteger(c.sector) || !Number.isInteger(c.ring)) return;
      const op = 0.25 + 0.5 * Math.sqrt((c.tot || 0) / maxTot);
      feats.push({ type: 'Feature', properties: { i, index: okNum(c.index) ? c.index : 0, op }, geometry: { type: 'Polygon', coordinates: [cellRing(ctx.geo, rx.lat, rx.lon, c.sector, c.ring)] } });
    });
    map.getSource('rf-cells').setData({ type: 'FeatureCollection', features: feats });
    map.getSource('rf-rx').setData({ type: 'FeatureCollection', features: [{ type: 'Feature', properties: {}, geometry: { type: 'Point', coordinates: [rx.lon, rx.lat] } }] });
    const maxRing = Math.max(2, ...cells.map((c) => (c.ring || 0) + 1));
    const key = `${rx.lat},${rx.lon},${maxRing}`;
    if (key !== fittedKey) {
      fittedKey = key;
      const km = maxRing * RING_KM;
      const sw = ctx.geo.destination(rx.lat, rx.lon, 225, km * Math.SQRT2);
      const ne = ctx.geo.destination(rx.lat, rx.lon, 45, km * Math.SQRT2);
      map.fitBounds([sw, ne], { padding: 16, animate: false });
    }
  }

  return {
    set(rx, cells) {
      current = { rx, cells: cells || [] };
      if (ready) apply();
    },
    resize() { if (!removed) map.resize(); },
    remove() {
      if (removed) return;
      removed = true;
      clearTimeout(fallback);
      popup.remove();
      map.remove();
    },
  };
}
