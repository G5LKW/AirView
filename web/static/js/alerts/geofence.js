// alerts/geofence.js - small MapLibre map to draw a geofence condition: a circle (click = centre, radius typed)
// or a polygon (click = add a point). The condition object is edited in place; onChange() is called after
// every edit. MapLibre loads lazily; the base map and tile-cache rewrite are the Live Map's (read-only import).

import { basemapById, styleFor, makeTransformRequest } from '../map/basemaps.js';

const BLANK_STYLE = { version: 8, sources: {}, layers: [{ id: 'bg', type: 'background', paint: { 'background-color': '#050c1b' } }] };
const EMPTY = { type: 'FeatureCollection', features: [] };
const fin = (v) => typeof v === 'number' && Number.isFinite(v);
const r5 = (v) => Math.round(v * 1e5) / 1e5;

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

/**
 * @param {HTMLElement} host sized element
 * @param ctx page context
 * @param {object} cond geofence leaf { shape: 'circle'|'polygon', lat, lon, radiusKm, polygon, inside }
 * @param {() => void} onChange
 * @returns {Promise<{ refresh(fit?: boolean), remove() }>}
 */
export async function createGeofenceMap(host, ctx, cond, onChange) {
  const maplibregl = await loadLib(ctx.ui);
  const rx = ctx.config.current?.receiver;
  const bm = basemapById(ctx.config.current?.ui?.mapStyle || 'airview-dark');
  const start = fin(cond.lat) && fin(cond.lon) ? [cond.lon, cond.lat] : rx?.located ? [rx.lon, rx.lat] : [8, 50];
  const map = new maplibregl.Map({
    container: host, style: styleFor(bm), center: start, zoom: 7, minZoom: 1, maxZoom: 15, maxPitch: 0,
    dragRotate: false, pitchWithRotate: false, touchPitch: false, fadeDuration: 0, attributionControl: false,
    doubleClickZoom: false, cooperativeGestures: false,
    transformRequest: makeTransformRequest(() => !!ctx.live.status?.tileCache),
  });
  map.touchZoomRotate.disableRotation();
  map.keyboard.disableRotation();
  map.addControl(new maplibregl.AttributionControl({ compact: true }), 'bottom-right');
  map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');

  let ready = false;
  let removed = false;
  const fallback = setTimeout(() => { if (!ready && !removed) map.setStyle(BLANK_STYLE); }, 6000);
  map.on('error', (e) => { if (!ready) console.warn('[alerts] geofence map:', e?.error?.message || e); });

  function features() {
    const feats = [];
    const col = cond.inside === false ? '#fbbf24' : '#22d3ee';
    if (cond.shape === 'polygon') {
      const pts = Array.isArray(cond.polygon) ? cond.polygon : [];
      if (pts.length >= 3) feats.push({ type: 'Feature', properties: { col }, geometry: { type: 'Polygon', coordinates: [[...pts, pts[0]]] } });
      else if (pts.length === 2) feats.push({ type: 'Feature', properties: { col }, geometry: { type: 'LineString', coordinates: pts } });
      pts.forEach((p, i) => feats.push({ type: 'Feature', properties: { col, v: i === 0 ? 2 : 1 }, geometry: { type: 'Point', coordinates: p } }));
    } else if (fin(cond.lat) && fin(cond.lon)) {
      if (fin(cond.radiusKm) && cond.radiusKm > 0) {
        feats.push({ type: 'Feature', properties: { col }, geometry: { type: 'Polygon', coordinates: [ctx.geo.circle(cond.lat, cond.lon, Math.min(cond.radiusKm, 5000), 96)] } });
      }
      feats.push({ type: 'Feature', properties: { col, v: 2 }, geometry: { type: 'Point', coordinates: [cond.lon, cond.lat] } });
    }
    return { type: 'FeatureCollection', features: feats };
  }

  function fit() {
    const fc = features();
    let w = Infinity; let s = Infinity; let e = -Infinity; let n = -Infinity;
    const eat = (c) => { if (typeof c[0] === 'number') { w = Math.min(w, c[0]); e = Math.max(e, c[0]); s = Math.min(s, c[1]); n = Math.max(n, c[1]); } else c.forEach(eat); };
    fc.features.forEach((f) => eat(f.geometry.coordinates));
    if (!Number.isFinite(w)) return;
    if (e - w < 1e-3 && n - s < 1e-3) map.jumpTo({ center: [w, s], zoom: 9 });
    else map.fitBounds([[w, s], [e, n]], { padding: 28, maxZoom: 12, animate: false });
  }

  function install() {
    if (removed) return;
    if (!map.getSource('gf')) map.addSource('gf', { type: 'geojson', data: EMPTY });
    if (!map.getSource('gf-rx')) map.addSource('gf-rx', { type: 'geojson', data: rx?.located ? { type: 'Feature', properties: {}, geometry: { type: 'Point', coordinates: [rx.lon, rx.lat] } } : EMPTY });
    const add = (l) => { if (!map.getLayer(l.id)) map.addLayer(l); };
    add({ id: 'gf-fill', type: 'fill', source: 'gf', filter: ['==', ['geometry-type'], 'Polygon'], paint: { 'fill-color': ['get', 'col'], 'fill-opacity': 0.14 } });
    add({ id: 'gf-line', type: 'line', source: 'gf', filter: ['!=', ['geometry-type'], 'Point'], layout: { 'line-join': 'round' }, paint: { 'line-color': ['get', 'col'], 'line-width': 2, 'line-opacity': 0.95 } });
    add({ id: 'gf-rx', type: 'circle', source: 'gf-rx', paint: { 'circle-radius': 4, 'circle-color': '#ffffff', 'circle-stroke-color': '#2f7dff', 'circle-stroke-width': 2.5 } });
    add({ id: 'gf-pts', type: 'circle', source: 'gf', filter: ['==', ['geometry-type'], 'Point'], paint: { 'circle-radius': ['case', ['==', ['get', 'v'], 2], 5.5, 4], 'circle-color': ['get', 'col'], 'circle-stroke-color': '#01040b', 'circle-stroke-width': 1.5 } });
    ready = true;
    map.getSource('gf').setData(features());
    fit();
  }
  map.on('style.load', install);

  map.on('click', (ev) => {
    const { lng, lat } = ev.lngLat.wrap();
    if (cond.shape === 'polygon') {
      if (!Array.isArray(cond.polygon)) cond.polygon = [];
      if (cond.polygon.length >= 1000) return;
      cond.polygon.push([r5(lng), r5(lat)]);
    } else {
      cond.lat = r5(lat);
      cond.lon = r5(lng);
    }
    if (ready) map.getSource('gf').setData(features());
    onChange();
  });
  map.getCanvas().style.cursor = 'crosshair';

  return {
    refresh(refit) {
      if (!ready || removed) return;
      map.getSource('gf').setData(features());
      if (refit) fit();
    },
    resize() { if (!removed) map.resize(); },
    remove() {
      if (removed) return;
      removed = true;
      clearTimeout(fallback);
      map.remove();
    },
  };
}
