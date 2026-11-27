// aircraft/trailmap.js - small MapLibre map that draws one stored flight trail (Aircraft page drawer).
// MapLibre and its stylesheet load lazily on first use; the base map and tile-cache rewrite are the Live Map's.

import { BASEMAPS, styleFor, makeTransformRequest } from '../map/basemaps.js';
import { altColor, signalColor } from '../map/palette.js';

// Used when the vector style cannot be fetched (offline station): the trail is still drawn on a plain canvas.
const BLANK_STYLE = { version: 8, sources: {}, layers: [{ id: 'bg', type: 'background', paint: { 'background-color': '#050c1b' } }] };
const EMPTY = { type: 'FeatureCollection', features: [] };

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

/** Trail points [tMs, lat, lon, altFt, gsKt, rssi, src] -> line features, one per run of equal colour. */
function trailLines(trail, colorOf) {
  const feats = [];
  let run = null;
  let prev = null;
  for (const p of trail) {
    if (!okNum(p[1]) || !okNum(p[2])) continue;
    const c = colorOf(p);
    const pt = [p[2], p[1]];
    if (!run || run.color !== c) {
      run = { color: c, coords: prev ? [prev, pt] : [pt] };
      feats.push(run);
    } else {
      run.coords.push(pt);
    }
    prev = pt;
  }
  return feats.filter((r) => r.coords.length > 1).map((r) => ({ type: 'Feature', properties: { color: r.color }, geometry: { type: 'LineString', coordinates: r.coords } }));
}

/**
 * @param {HTMLElement} host sized element
 * @param ctx page context
 * @param {{ describe?: (point) => string }} opts describe() returns the hover text of a trail point
 * @returns {Promise<{ set(data), resize(), remove() }>}
 *   set({ trail, mode: 'altitude'|'signal', receiver: {lat, lon}|null, far: {lat, lon}|null })
 */
export async function createTrailMap(host, ctx, opts = {}) {
  const maplibregl = await loadLib(ctx.ui);
  const map = new maplibregl.Map({
    container: host,
    style: styleFor(BASEMAPS[0]),
    center: [8, 30],
    zoom: 1.4,
    minZoom: 1,
    maxZoom: 16,
    maxPitch: 0,
    dragRotate: false,
    pitchWithRotate: false,
    touchPitch: false,
    fadeDuration: 0,
    attributionControl: false,
    transformRequest: makeTransformRequest(() => !!ctx.live.status?.tileCache),
  });
  map.touchZoomRotate.disableRotation();
  map.keyboard.disableRotation();
  map.addControl(new maplibregl.AttributionControl({ compact: true }), 'bottom-right');
  map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');

  let ready = false;
  let removed = false;
  let current = null;
  let fitted = null;
  const popup = new maplibregl.Popup({ closeButton: false, closeOnClick: false, offset: 10, className: 'ac-popup' });

  function install() {
    if (removed) return;
    for (const id of ['ac-trail', 'ac-points', 'ac-marks']) if (!map.getSource(id)) map.addSource(id, { type: 'geojson', data: EMPTY });
    const add = (layer) => { if (!map.getLayer(layer.id)) map.addLayer(layer); };
    add({ id: 'ac-trail-casing', type: 'line', source: 'ac-trail', layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': '#01030a', 'line-width': 6, 'line-opacity': 0.55 } });
    add({ id: 'ac-trail', type: 'line', source: 'ac-trail', layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': ['get', 'color'], 'line-width': 3 } });
    add({ id: 'ac-points', type: 'circle', source: 'ac-points', paint: { 'circle-radius': ['interpolate', ['linear'], ['zoom'], 4, 1.5, 9, 3.5], 'circle-color': ['get', 'color'], 'circle-stroke-color': '#01030a', 'circle-stroke-width': 0.8 } });
    add({ id: 'ac-marks-glow', type: 'circle', source: 'ac-marks', filter: ['==', ['get', 'kind'], 'rx'], paint: { 'circle-radius': 16, 'circle-color': '#2f7dff', 'circle-opacity': 0.28, 'circle-blur': 0.7 } });
    add({
      id: 'ac-marks', type: 'circle', source: 'ac-marks',
      paint: {
        'circle-radius': ['match', ['get', 'kind'], 'rx', 4.5, 'far', 5, 5.5],
        'circle-color': ['match', ['get', 'kind'], 'rx', '#ffffff', 'start', '#34d399', 'end', '#ffffff', 'far', '#fbbf24', '#ffffff'],
        'circle-stroke-color': ['match', ['get', 'kind'], 'rx', '#2f7dff', 'end', '#ff4d5e', '#01030a'],
        'circle-stroke-width': ['match', ['get', 'kind'], 'rx', 3, 'end', 2.5, 1.5],
      },
    });
    ready = true;
    if (current) apply(current, false);
  }
  map.on('style.load', install);
  const fallback = setTimeout(() => { if (!ready && !removed) map.setStyle(BLANK_STYLE); }, 6000);
  map.on('error', (e) => { if (!ready) console.warn('[aircraft] trail map:', e && e.error ? e.error.message : e); });

  map.on('mousemove', 'ac-points', (e) => {
    const f = e.features && e.features[0];
    if (!f || !opts.describe || !current) return;
    const p = current.trail[f.properties.i];
    if (!p) return;
    map.getCanvas().style.cursor = 'crosshair';
    popup.setLngLat([p[2], p[1]]).setText(opts.describe(p)).addTo(map);
  });
  map.on('mouseleave', 'ac-points', () => { map.getCanvas().style.cursor = ''; popup.remove(); });

  function apply(data, refit) {
    const trail = data.trail || [];
    const colorOf = data.mode === 'signal' ? (p) => signalColor(p[5]) : (p) => altColor(p[3]);
    const pts = [];
    trail.forEach((p, i) => { if (okNum(p[1]) && okNum(p[2])) pts.push({ type: 'Feature', properties: { i, color: colorOf(p) }, geometry: { type: 'Point', coordinates: [p[2], p[1]] } }); });
    const marks = [];
    const mark = (kind, lat, lon) => { if (okNum(lat) && okNum(lon)) marks.push({ type: 'Feature', properties: { kind }, geometry: { type: 'Point', coordinates: [lon, lat] } }); };
    if (data.receiver) mark('rx', data.receiver.lat, data.receiver.lon);
    if (data.far) mark('far', data.far.lat, data.far.lon);
    if (pts.length) {
      const a = pts[0].geometry.coordinates;
      const b = pts[pts.length - 1].geometry.coordinates;
      mark('start', a[1], a[0]);
      mark('end', b[1], b[0]);
    }
    map.getSource('ac-trail').setData({ type: 'FeatureCollection', features: trailLines(trail, colorOf) });
    map.getSource('ac-points').setData({ type: 'FeatureCollection', features: pts });
    map.getSource('ac-marks').setData({ type: 'FeatureCollection', features: marks });
    if (!refit && fitted === data.key) return;
    fitted = data.key;
    const all = pts.map((f) => f.geometry.coordinates);
    if (data.receiver && okNum(data.receiver.lat) && okNum(data.receiver.lon)) all.push([data.receiver.lon, data.receiver.lat]);
    if (!all.length) return;
    let w = Infinity; let s = Infinity; let e = -Infinity; let n = -Infinity;
    for (const [x, y] of all) { w = Math.min(w, x); e = Math.max(e, x); s = Math.min(s, y); n = Math.max(n, y); }
    if (e - w < 1e-4 && n - s < 1e-4) map.jumpTo({ center: [w, s], zoom: 8 });
    else map.fitBounds([[w, s], [e, n]], { padding: 36, maxZoom: 11, animate: false });
  }

  return {
    /** Draw a flight. `key` identifies it: the view is refitted only when the key changes. */
    set(data) {
      current = data;
      if (ready) apply(data, false);
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
