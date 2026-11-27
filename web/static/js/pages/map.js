// pages/map.js - the Live Map.
//
// MapLibre GL renders everything on the GPU: aircraft are one data-driven symbol layer of SDF
// silhouettes (tinted, outlined and rotated in the shader), trails are line layers whose
// features are runs of equal colour, and all receiver overlays are GeoJSON layers. JavaScript
// only prepares small GeoJSON updates (1 Hz, or ~8 Hz with smooth motion), so panning and
// zooming stay fluid with several hundred aircraft.
//
// Prediction: aircraft whose position is predicted (aircraft.predicted) keep gliding along the
// filter's prediction and glow in their own colour (fading as coastS approaches the coast limit),
// with their 1-sigma uncertainty circle and a dashed connector to the last real position. Predicted
// paths (selected aircraft from /api/prediction/path, or every aircraft dead-reckoned client-side)
// are fading dashed lines with time ticks. Between 1 Hz snapshots positions are dead-reckoned and
// corrections are blended in; low-power (ARM) mode keeps smooth motion and animations off.
//
// Other pages can talk to the map through ctx.shared:
//   ctx.shared.set('map.coverage', geojson)   // Polygon/MultiPolygon/LineString Feature(Collection);
//                                             // optional feature properties: color, fillOpacity
//   ctx.navigate('map/4CA1FA')                // open the map with an aircraft selected
// URL: #/map/<ICAO>?zoom=&lat=&lon=&filter=<see map/filters.js>&kiosk=1
//
// Network (aggregator) mode adds the View selector of map/network.js (Network / My stations / Station / Admin).
// Receiver-centred overlays then follow the view: none in Network / My stations / Admin (public and merged
// views carry no receiver data), the selected station's exact position in the Station view. In standalone mode
// rxCfg() is simply the configuration and everything behaves as before.

import * as maplibregl from '../../vendor/maplibre/maplibre-gl.mjs';
import { ui, h, icon, setText } from '../ui.js';
import { units } from '../units.js';
import { geo } from '../geo.js';
import { COLOR_MODES, aircraftColor, trailColor, legend as legendFor } from '../map/palette.js';
import { SHAPES, shapeFor, imageName, addAircraftImages } from '../map/aircraft-icons.js';
import { BASEMAPS, basemapById, styleFor, makeTransformRequest, LABEL_FONT, LABEL_FONT_REGULAR, RAINVIEWER_INDEX, RAINVIEWER_ATTRIBUTION } from '../map/basemaps.js';
import { receiverOverlays, nightOverlay, coverageCollection, bounds, EMPTY } from '../map/overlays.js';
import { createList, isEmergency, isStale, hasPosition } from '../map/list.js';
import { createDetail, download } from '../map/detail.js';
import { coastProps, SIGMA_RADIUS, deadReckonPath, pathFeatures, linkFeature, lastRealFeature, sameWorld } from '../map/predict.js';
import { createExtras, AIRPORT_MIN_ZOOM } from '../map/extras.js';
import { createMeasure } from '../map/measure.js';
import { createAlerts, unlockAudio, loadAlertSound, alertSound } from '../map/alerts.js';
import * as filters from '../map/filters.js';
import { createNetView } from '../map/network.js';
import { GRADES, MLAT_UNGRADED, VIEWPORT_ON, mlatRing, viewportBox, wantViewport } from '../mlat/model.js';
import { net } from '../net/store.js';
import { overlayConfig } from '../net/model.js';

const PREFS_KEY = 'airview.map.v1';
const STATIC_TRAIL_REBUILD_MS = 10000;
const SMOOTH_TICK_MS = 125;
const MAX_TRAIL_POINTS = 900;
const TRAIL_GAP_MS = 120000; // do not join points across a longer silence
const MAX_EXTRAPOLATE_S = 12;
const MAX_EXTRAPOLATE_PRED_S = 3; // a predicted position is for snapshot time; the next snapshot replaces it
const BLEND_MS = 900; // corrections between snapshots are blended in over this time
const BLEND_MAX_DEG = 0.15; // larger jumps snap
const PATH_REFRESH_MS = 5000;
const PATH_GROUP_MAX = 8;
const ALL_PATH_HORIZON_S = 60;
const ANIM_FRAME_MS = 33;

const fc = (features) => ({ type: 'FeatureCollection', features });
const num = (v) => typeof v === 'number' && Number.isFinite(v);

let S = null; // page state while mounted

/** The configuration as the receiver overlays see it: standalone = config; network mode = per view. */
const rxCfg = () => overlayConfig(S.ctx.config.current, net);

// -----------------------------------------------------------------------------------------------
// preferences
// -----------------------------------------------------------------------------------------------
function effective(S) {
  const e = baseEffective(S);
  // network mode: receiver-centred overlays exist only in the Station view (the server has no receiver data
  // for the merged views, and the public must not get any)
  if (S.netv && S.netv.hidesReceiver()) {
    for (const k of ['rings', 'beams', 'compass', 'coverage', 'range', 'terrain', 'heat', 'prox']) e[k] = false;
  }
  return e;
}

function baseEffective(S) {
  const p = S.prefs.get();
  const cfg = S.ctx.config.current || {};
  const uiCfg = cfg.ui || {};
  const lowPower = document.documentElement.classList.contains('lowpower');
  const predOn = cfg.prediction?.enabled !== false;
  return {
    basemap: basemapById(p.basemap || uiCfg.mapStyle || 'airview-dark'),
    colorBy: COLOR_MODES.some((m) => m.value === (p.colorBy || uiCfg.colorTrailsBy)) ? p.colorBy || uiCfg.colorTrailsBy : 'altitude',
    labels: p.labels,
    trails: p.trails,
    trailMinutes: Math.max(1, Math.min(120, p.trailMinutes || uiCfg.trailMinutes || 15)),
    rings: p.rings, beams: p.beams, compass: p.compass, night: p.night, coverage: p.coverage,
    radar: p.radar === null ? !!cfg.weather?.radarOverlay : p.radar,
    smooth: p.smooth === null ? !lowPower : p.smooth,
    lowPower,
    predPaths: ['off', 'selected', 'all'].includes(p.predPaths) ? p.predPaths : predOn && cfg.prediction?.showPaths !== false ? 'selected' : 'off',
    pathSec: Math.max(10, Math.min(900, cfg.prediction?.pathSec || 120)),
    coastSec: Math.max(5, cfg.prediction?.coastSec || 60),
    range: !!p.range,
    terrain: !!p.terrain,
    terrainKind: p.terrainKind === 'radio' ? 'radio' : 'los',
    airports: p.airports === null ? cfg.navData?.airportsOverlay !== false : !!p.airports,
    heat: !!p.heat,
    prox: !!p.prox,
    acas: !!p.acas,
    winds: !!p.winds,
    windsFL: [100, 180, 240, 300, 340, 390].includes(p.windsFL) ? p.windsFL : 300,
    mute: !!p.mute,
    soundAllowed: cfg.alerts?.sound !== false,
  };
}

// -----------------------------------------------------------------------------------------------
// layers
// -----------------------------------------------------------------------------------------------
const SOURCES = ['night', 'heat', 'coverage', 'range', 'terrain', 'rings', 'ring-labels', 'compass', 'compass-labels', 'beams', 'beam-axes',
  'airports', 'acas', 'winds', 'route', 'receiver', 'trail-sel', 'trace', 'focus', 'pred-paths', 'pred-ticks', 'pred-links', 'prox', 'measure'];

function installLayers() {
  const { map } = S;
  const dark = S.eff.basemap.dark;
  const ring = dark ? '#78a8ff' : '#1c4fb8';
  const ringText = dark ? '#9dbcf2' : '#17409a';
  const halo = dark ? 'rgba(1,4,11,0.9)' : 'rgba(255,255,255,0.9)';
  const labelColor = dark ? '#e3edff' : '#0b1b3a';
  const iconScale = (k) => ['interpolate', ['linear'], ['zoom'], 3, 0.55 * k, 9, k, 13, 1.25 * k];

  addAircraftImages(map);
  const src = (id, extra) => { if (!map.getSource(id)) map.addSource(id, { type: 'geojson', data: S.cache.get(id) || EMPTY, ...extra }); };
  const add = (layer) => { if (!map.getLayer(layer.id)) map.addLayer(layer); };

  for (const id of SOURCES) src(id);
  src('trails', { tolerance: 0.25, buffer: 32 });
  src('trails-live', { tolerance: 0, buffer: 32 });
  src('aircraft', { tolerance: 0, buffer: 64 });

  add({ id: 'night', type: 'fill', source: 'night', paint: { 'fill-color': dark ? '#00030c' : '#001033', 'fill-opacity': dark ? 0.16 : 0.1, 'fill-antialias': false } });
  add({ id: 'heat', type: 'heatmap', source: 'heat', maxzoom: 13, paint: {
    'heatmap-weight': ['get', 'w'],
    'heatmap-intensity': ['interpolate', ['linear'], ['zoom'], 3, 0.6, 9, 1.6],
    'heatmap-radius': ['interpolate', ['exponential', 1.6], ['zoom'], 3, 6, 7, 14, 11, 40],
    'heatmap-color': ['interpolate', ['linear'], ['heatmap-density'], 0, 'rgba(10,30,90,0)', 0.15, 'rgba(47,125,255,0.45)', 0.4, 'rgba(34,211,238,0.65)', 0.65, 'rgba(250,204,21,0.75)', 0.9, 'rgba(255,77,94,0.85)', 1, 'rgba(255,240,240,0.9)'],
    'heatmap-opacity': 0.8,
  } });
  add({ id: 'coverage-fill', type: 'fill', source: 'coverage', filter: ['==', ['geometry-type'], 'Polygon'], paint: { 'fill-color': ['coalesce', ['get', 'color'], '#22d3ee'], 'fill-opacity': ['coalesce', ['get', 'fillOpacity'], 0.07] } });
  add({ id: 'coverage-line', type: 'line', source: 'coverage', layout: { 'line-join': 'round' }, paint: { 'line-color': ['coalesce', ['get', 'color'], '#22d3ee'], 'line-width': 1.4, 'line-opacity': 0.85 } });

  // measured range outline (robust = faint fill, max = bright line)
  add({ id: 'range-fill', type: 'fill', source: 'range', filter: ['==', ['get', 'kind'], 'robust'], paint: { 'fill-color': dark ? '#7dd3fc' : '#0369a1', 'fill-opacity': 0.06 } });
  add({ id: 'range-robust', type: 'line', source: 'range', filter: ['==', ['get', 'kind'], 'robust'], layout: { 'line-join': 'round' }, paint: { 'line-color': dark ? '#7dd3fc' : '#0369a1', 'line-width': 1, 'line-opacity': 0.6, 'line-dasharray': [3, 2] } });
  add({ id: 'range-max', type: 'line', source: 'range', filter: ['==', ['get', 'kind'], 'max'], layout: { 'line-join': 'round' }, paint: { 'line-color': dark ? '#bae6fd' : '#0c4a6e', 'line-width': 1.4, 'line-opacity': 0.8 } });
  // terrain theoretical coverage rings
  add({ id: 'terrain-line', type: 'line', source: 'terrain', layout: { 'line-join': 'round' }, paint: { 'line-color': ['coalesce', ['get', 'color'], '#a78bfa'], 'line-width': 1.2, 'line-opacity': 0.8, 'line-dasharray': [4, 3] } });
  add({ id: 'terrain-labels', type: 'symbol', source: 'terrain', layout: { 'symbol-placement': 'line', 'symbol-spacing': 420, 'text-field': ['get', 'label'], 'text-font': LABEL_FONT, 'text-size': 10, 'text-keep-upright': true }, paint: { 'text-color': ['coalesce', ['get', 'color'], '#a78bfa'], 'text-halo-color': halo, 'text-halo-width': 1.6 } });

  add({ id: 'compass', type: 'line', source: 'compass', paint: { 'line-color': ring, 'line-opacity': ['case', ['==', ['get', 'spoke'], 1], 0.13, ['==', ['get', 'major'], 1], 0.9, 0.5], 'line-width': ['case', ['==', ['get', 'major'], 1], 1.5, 0.8] } });
  add({ id: 'rings', type: 'line', source: 'rings', filter: ['==', ['get', 'outer'], 0], paint: { 'line-color': ring, 'line-opacity': 0.42, 'line-width': 1, 'line-dasharray': [2, 4] } });
  add({ id: 'rings-outer', type: 'line', source: 'rings', filter: ['==', ['get', 'outer'], 1], paint: { 'line-color': ring, 'line-opacity': 0.8, 'line-width': 1.4 } });

  add({ id: 'beams-fill', type: 'fill', source: 'beams', paint: { 'fill-color': ['get', 'color'], 'fill-opacity': 0.1 } });
  add({ id: 'beams-line', type: 'line', source: 'beams', paint: { 'line-color': ['get', 'color'], 'line-opacity': 0.6, 'line-width': 1 } });
  add({ id: 'beam-axes', type: 'line', source: 'beam-axes', paint: { 'line-color': ['get', 'color'], 'line-opacity': 0.7, 'line-width': 1, 'line-dasharray': [2, 3] } });
  add({ id: 'beam-labels', type: 'symbol', source: 'beam-axes', layout: { 'symbol-placement': 'line-center', 'text-field': ['get', 'text'], 'text-font': LABEL_FONT, 'text-size': 10.5, 'text-offset': [0, -0.9], 'text-letter-spacing': 0.04, 'text-allow-overlap': true }, paint: { 'text-color': ['get', 'color'], 'text-halo-color': halo, 'text-halo-width': 1.4 } });
  add({ id: 'ring-labels', type: 'symbol', source: 'ring-labels', layout: { 'text-field': ['get', 'text'], 'text-font': LABEL_FONT_REGULAR, 'text-size': 10.5, 'text-padding': 2 }, paint: { 'text-color': ringText, 'text-halo-color': halo, 'text-halo-width': 1.8 } });
  add({ id: 'compass-labels', type: 'symbol', source: 'compass-labels', layout: { 'text-field': ['get', 'text'], 'text-font': LABEL_FONT, 'text-size': ['case', ['==', ['get', 'cardinal'], 1], 13, 9.5], 'text-allow-overlap': true, 'text-ignore-placement': true }, paint: { 'text-color': ['case', ['==', ['get', 'cardinal'], 1], dark ? '#ffffff' : '#0b1b3a', ringText], 'text-halo-color': halo, 'text-halo-width': 1.6 } });

  // airports, runways, extended centrelines
  const apt = dark ? '#c4b5fd' : '#5b21b6';
  add({ id: 'apt-centerline', type: 'line', source: 'airports', minzoom: 8, filter: ['==', ['get', 'kind'], 'centerline'], paint: { 'line-color': apt, 'line-opacity': 0.45, 'line-width': 1, 'line-dasharray': [3, 3] } });
  add({ id: 'apt-runway', type: 'fill', source: 'airports', minzoom: 8, filter: ['==', ['get', 'kind'], 'runway'], paint: { 'fill-color': dark ? '#dbe4f5' : '#334155', 'fill-opacity': 0.55, 'fill-outline-color': dark ? '#ffffff' : '#0b1b3a' } });
  add({ id: 'apt-dot', type: 'circle', source: 'airports', minzoom: AIRPORT_MIN_ZOOM, filter: ['==', ['get', 'kind'], 'airport'], paint: {
    'circle-radius': ['match', ['get', 'size'], 3, 4.5, 2, 3.5, 2.5], 'circle-color': dark ? '#0b1430' : '#ffffff', 'circle-stroke-color': apt, 'circle-stroke-width': 1.6,
  } });
  add({ id: 'apt-label', type: 'symbol', source: 'airports', minzoom: AIRPORT_MIN_ZOOM, filter: ['==', ['get', 'kind'], 'airport'], layout: {
    'text-field': ['step', ['zoom'], ['get', 'code'], 9.5, ['format', ['get', 'code'], {}, '\n', {}, ['get', 'name'], { 'font-scale': 0.82 }]],
    'text-font': LABEL_FONT, 'text-size': ['match', ['get', 'size'], 3, 11, 10], 'text-offset': [0, 0.9], 'text-anchor': 'top', 'text-optional': true, 'text-max-width': 12,
    'symbol-sort-key': ['-', 0, ['get', 'size']],
  }, paint: { 'text-color': apt, 'text-halo-color': halo, 'text-halo-width': 1.5 } });

  // TCAS resolution advisories of the last 24 h (where each started)
  add({ id: 'acas-dot', type: 'circle', source: 'acas', paint: { 'circle-radius': 5, 'circle-color': '#ff453a', 'circle-opacity': 0.85, 'circle-stroke-color': halo, 'circle-stroke-width': 1.2 } });
  add({ id: 'acas-label', type: 'symbol', source: 'acas', minzoom: 6, layout: { 'text-field': ['get', 't'], 'text-font': LABEL_FONT, 'text-size': 10, 'text-offset': [0, 0.9], 'text-anchor': 'top', 'text-optional': true, 'text-max-width': 16 }, paint: { 'text-color': '#ff8a80', 'text-halo-color': halo, 'text-halo-width': 1.5 } });

  // winds / temperature aloft (gridded EHS): arrow towards where the wind blows, dot coloured by ISA deviation
  const isa = ['interpolate', ['linear'], ['get', 'd'], -10, '#60a5fa', 0, dark ? '#e5e7eb' : '#475569', 10, '#f87171'];
  add({ id: 'winds-line', type: 'line', source: 'winds', filter: ['!=', ['geometry-type'], 'Point'], layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': dark ? '#a5f3fc' : '#0e7490', 'line-width': ['interpolate', ['linear'], ['get', 'kt'], 0, 1, 100, 2.4], 'line-opacity': 0.85 } });
  add({ id: 'winds-dot', type: 'circle', source: 'winds', filter: ['==', ['geometry-type'], 'Point'], paint: { 'circle-radius': 3.2, 'circle-color': isa, 'circle-stroke-color': halo, 'circle-stroke-width': 1 } });
  add({ id: 'winds-label', type: 'symbol', source: 'winds', minzoom: 6.5, filter: ['==', ['geometry-type'], 'Point'], layout: { 'text-field': ['get', 't'], 'text-font': LABEL_FONT_REGULAR, 'text-size': 9.5, 'text-offset': [0, 0.8], 'text-anchor': 'top', 'text-optional': true }, paint: { 'text-color': dark ? '#cffafe' : '#155e75', 'text-halo-color': halo, 'text-halo-width': 1.4 } });

  // route of the selected aircraft
  add({ id: 'route-line', type: 'line', source: 'route', filter: ['==', ['get', 'kind'], 'line'], layout: { 'line-join': 'round', 'line-cap': 'round' }, paint: { 'line-color': ['case', ['==', ['get', 'ok'], 0], '#fbbf24', '#e879f9'], 'line-width': 1.6, 'line-opacity': 0.75, 'line-dasharray': [1, 2.4] } });
  add({ id: 'route-ap', type: 'circle', source: 'route', filter: ['==', ['get', 'kind'], 'ap'], paint: { 'circle-radius': 5, 'circle-color': dark ? '#0b1430' : '#ffffff', 'circle-stroke-color': '#e879f9', 'circle-stroke-width': 2 } });
  add({ id: 'route-ap-label', type: 'symbol', source: 'route', filter: ['==', ['get', 'kind'], 'ap'], layout: { 'text-field': ['get', 'text'], 'text-font': LABEL_FONT, 'text-size': 11.5, 'text-offset': [0, 1], 'text-anchor': 'top', 'text-allow-overlap': true }, paint: { 'text-color': '#f5d0fe', 'text-halo-color': halo, 'text-halo-width': 1.6 } });

  const trailWidth = ['interpolate', ['linear'], ['zoom'], 3, 1, 7, 1.7, 11, 2.6];
  add({ id: 'trails', type: 'line', source: 'trails', layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': ['get', 'c'], 'line-width': trailWidth, 'line-opacity': 0.82 } });
  add({ id: 'trails-live', type: 'line', source: 'trails-live', layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': ['get', 'c'], 'line-width': trailWidth, 'line-opacity': 0.82 } });
  add({ id: 'trace', type: 'line', source: 'trace', layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': ['get', 'c'], 'line-width': ['interpolate', ['linear'], ['zoom'], 3, 1.6, 11, 3], 'line-opacity': 0.7 } });
  add({ id: 'trail-sel-casing', type: 'line', source: 'trail-sel', layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': dark ? '#ffffff' : '#01040b', 'line-width': ['interpolate', ['linear'], ['zoom'], 3, 4.5, 11, 7.5], 'line-opacity': dark ? 0.22 : 0.5 } });
  add({ id: 'trail-sel', type: 'line', source: 'trail-sel', layout: { 'line-cap': 'round', 'line-join': 'round' }, paint: { 'line-color': ['get', 'c'], 'line-width': ['interpolate', ['linear'], ['zoom'], 3, 2.4, 11, 4.2] } });

  // predicted paths, time ticks, connectors to the last real position
  add({ id: 'pred-path', type: 'line', source: 'pred-paths', layout: { 'line-cap': 'butt', 'line-join': 'round' }, paint: { 'line-color': ['get', 'c'], 'line-opacity': ['get', 'o'], 'line-width': ['case', ['==', ['get', 'w'], 1], 2, 1.3], 'line-dasharray': [2, 1.6] } });
  add({ id: 'pred-tick-sigma', type: 'circle', source: 'pred-ticks', filter: ['>', ['get', 'r0'], 0], paint: { 'circle-radius': SIGMA_RADIUS, 'circle-color': ['get', 'c'], 'circle-opacity': 0.05, 'circle-stroke-color': ['get', 'c'], 'circle-stroke-width': 0.8, 'circle-stroke-opacity': ['*', 0.5, ['get', 'o']], 'circle-pitch-alignment': 'map' } });
  add({ id: 'pred-tick', type: 'circle', source: 'pred-ticks', paint: { 'circle-radius': 2.6, 'circle-color': ['get', 'c'], 'circle-opacity': ['get', 'o'], 'circle-stroke-color': halo, 'circle-stroke-width': 1 } });
  add({ id: 'pred-tick-label', type: 'symbol', source: 'pred-ticks', minzoom: 6, layout: { 'text-field': ['get', 't'], 'text-font': LABEL_FONT_REGULAR, 'text-size': 9.5, 'text-offset': [0.7, 0], 'text-anchor': 'left', 'text-optional': true, 'text-padding': 1 }, paint: { 'text-color': ['get', 'c'], 'text-opacity': ['get', 'o'], 'text-halo-color': halo, 'text-halo-width': 1.4 } });
  add({ id: 'pred-link', type: 'line', source: 'pred-links', filter: ['==', ['geometry-type'], 'LineString'], paint: { 'line-color': dark ? '#e9d5ff' : '#6d28d9', 'line-width': 1.2, 'line-opacity': 0.75, 'line-dasharray': [1.5, 1.5] } });
  add({ id: 'pred-last', type: 'circle', source: 'pred-links', filter: ['==', ['geometry-type'], 'Point'], paint: { 'circle-radius': 3, 'circle-color': 'rgba(0,0,0,0)', 'circle-stroke-color': dark ? '#e9d5ff' : '#6d28d9', 'circle-stroke-width': 1.3, 'circle-stroke-opacity': 0.85 } });

  // proximity pairs
  const sev = ['case', ['<=', ['get', 's'], 1], '#ff4d5e', ['<=', ['get', 's'], 2], '#fbbf24', '#7dd3fc'];
  add({ id: 'prox-line', type: 'line', source: 'prox', filter: ['==', ['geometry-type'], 'LineString'], layout: { 'line-cap': 'round' }, paint: { 'line-color': sev, 'line-width': 2, 'line-opacity': 0.9, 'line-dasharray': [1, 1.5] } });
  add({ id: 'prox-label', type: 'symbol', source: 'prox', filter: ['==', ['geometry-type'], 'Point'], layout: { 'text-field': ['get', 't'], 'text-font': LABEL_FONT, 'text-size': 10, 'text-allow-overlap': true, 'text-max-width': 14 }, paint: { 'text-color': sev, 'text-halo-color': halo, 'text-halo-width': 1.6 } });

  add({ id: 'receiver-glow', type: 'circle', source: 'receiver', paint: { 'circle-radius': 18, 'circle-color': '#2f7dff', 'circle-opacity': 0.28, 'circle-blur': 0.7 } });
  add({ id: 'receiver', type: 'circle', source: 'receiver', paint: { 'circle-radius': 4.5, 'circle-color': '#ffffff', 'circle-stroke-color': '#2f7dff', 'circle-stroke-width': 3 } });

  // coasting aircraft: uncertainty circle under everything aircraft-related
  add({ id: 'ac-pred-sigma', type: 'circle', source: 'aircraft', filter: ['all', ['==', ['get', 'pred'], 1], ['>', ['get', 'r0'], 0]], paint: { 'circle-radius': SIGMA_RADIUS, 'circle-color': ['get', 'c'], 'circle-opacity': 0.08, 'circle-stroke-color': ['get', 'c'], 'circle-stroke-width': 1, 'circle-stroke-opacity': ['*', 0.55, ['get', 'gi']], 'circle-pitch-alignment': 'map' } });
  add({ id: 'ac-emerg', type: 'circle', source: 'aircraft', filter: ['==', ['get', 'emerg'], 1], paint: { 'circle-radius': 20, 'circle-color': '#ff4d5e', 'circle-opacity': 0.22, 'circle-stroke-color': '#ff4d5e', 'circle-stroke-width': 1.6, 'circle-stroke-opacity': 0.9 } });
  // an active TCAS resolution advisory reported by the aircraft
  add({ id: 'ac-ra', type: 'circle', source: 'aircraft', filter: ['==', ['get', 'ra'], 1], paint: { 'circle-radius': iconScale(21), 'circle-color': 'rgba(255,69,58,0.12)', 'circle-stroke-color': '#ff453a', 'circle-stroke-width': 2.6, 'circle-stroke-opacity': 0.95 } });
  add({ id: 'ac-hl', type: 'circle', source: 'aircraft', filter: ['!=', ['get', 'hl'], ''], paint: { 'circle-radius': iconScale(17), 'circle-color': 'rgba(0,0,0,0)', 'circle-stroke-color': ['get', 'hl'], 'circle-stroke-width': 2.2, 'circle-stroke-opacity': 0.95 } });
  // MLAT positions: a ring around the icon in the colour of the fix grade (A green, B amber, C orange; violet
  // for external MLAT without a grade), thicker for better grades - distinct from ADS-B in every colour mode
  add({ id: 'ac-mlat', type: 'circle', source: 'aircraft', filter: ['all', ['==', ['get', 'mlat'], 1], ['==', ['get', 'pred'], 0]], paint: { 'circle-radius': ['interpolate', ['linear'], ['zoom'], 4, 9, 9, 15], 'circle-color': 'rgba(0,0,0,0)', 'circle-stroke-color': ['case', ['==', ['get', 'mc'], ''], '#fbbf24', ['get', 'mc']], 'circle-stroke-width': ['case', ['>', ['get', 'mw'], 0], ['get', 'mw'], 1.3], 'circle-stroke-opacity': ['case', ['==', ['get', 'stale'], 1], 0.3, 0.9] } });
  add({ id: 'ac-focus', type: 'circle', source: 'focus', paint: {
    'circle-radius': ['match', ['get', 'kind'], 'sel', 21, 'grp', 18, 17],
    'circle-color': ['match', ['get', 'kind'], 'sel', 'rgba(47,125,255,0.12)', 'grp', 'rgba(167,139,250,0.14)', 'rgba(255,255,255,0.06)'],
    'circle-stroke-color': ['match', ['get', 'kind'], 'sel', '#8cc2ff', 'grp', '#c4b5fd', '#ffffff'],
    'circle-stroke-width': ['match', ['get', 'kind'], 'sel', 2, 'grp', 1.8, 1.2],
    'circle-stroke-opacity': 0.95,
  } });
  add({ id: 'ac-pred-glow', type: 'circle', source: 'aircraft', filter: ['==', ['get', 'pred'], 1], paint: {
    'circle-radius': iconScale(17), 'circle-color': ['get', 'c'], 'circle-blur': 0.85, 'circle-opacity': ['*', 0.55, ['get', 'gi']],
  } });
  add({
    id: 'ac-icons', type: 'symbol', source: 'aircraft',
    layout: {
      'icon-image': ['get', 'shape'],
      'icon-size': ['interpolate', ['linear'], ['zoom'], 3, ['*', 0.5, ['get', 's']], 6, ['*', 0.72, ['get', 's']], 9, ['get', 's'], 13, ['*', 1.25, ['get', 's']]],
      'icon-rotate': ['get', 'rot'],
      'icon-rotation-alignment': 'map',
      'icon-allow-overlap': true,
      'icon-ignore-placement': false,
      'icon-padding': 0,
      'symbol-sort-key': ['get', 'sk'],
      'text-field': S.eff.labels === 'none' ? '' : ['get', 'label'],
      'text-font': LABEL_FONT,
      'text-size': 11,
      'text-line-height': 1.18,
      'text-variable-anchor': ['left', 'right', 'top', 'bottom'],
      'text-radial-offset': 1.25,
      'text-justify': 'auto',
      'text-optional': true,
      'text-padding': 1,
      'text-letter-spacing': 0.03,
    },
    paint: {
      'icon-color': ['get', 'c'],
      'icon-halo-color': ['case', ['==', ['get', 'pred'], 1], dark ? 'rgba(40,20,80,0.95)' : 'rgba(60,30,120,0.95)', dark ? 'rgba(1,4,11,0.95)' : 'rgba(6,16,40,0.95)'],
      'icon-halo-width': ['case', ['==', ['get', 'pred'], 1], 1.8, 1.4],
      'icon-opacity': ['case', ['==', ['get', 'stale'], 1], 0.38, ['==', ['get', 'pred'], 1], ['get', 'po'], 1],
      'text-color': labelColor,
      'text-halo-color': halo,
      'text-halo-width': 1.5,
      'text-opacity': ['case', ['==', ['get', 'stale'], 1], 0.5, ['==', ['get', 'pred'], 1], ['get', 'po'], 1],
    },
  });

  // measuring tool on top
  add({ id: 'measure-line', type: 'line', source: 'measure', filter: ['==', ['get', 'kind'], 'line'], layout: { 'line-cap': 'round' }, paint: { 'line-color': dark ? '#ffffff' : '#0b1b3a', 'line-width': 2, 'line-opacity': ['case', ['==', ['get', 'tmp'], 1], 0.55, 0.95], 'line-dasharray': [2, 1.5] } });
  add({ id: 'measure-pt', type: 'circle', source: 'measure', filter: ['==', ['get', 'kind'], 'pt'], paint: { 'circle-radius': 5, 'circle-color': '#ffffff', 'circle-stroke-color': '#2f7dff', 'circle-stroke-width': 2.5 } });
  add({ id: 'measure-label', type: 'symbol', source: 'measure', filter: ['==', ['get', 'kind'], 'label'], layout: { 'text-field': ['get', 'text'], 'text-font': LABEL_FONT, 'text-size': 12.5, 'text-allow-overlap': true, 'text-ignore-placement': true, 'text-offset': [0, -1.2] }, paint: { 'text-color': '#ffffff', 'text-halo-color': 'rgba(5,15,40,0.95)', 'text-halo-width': 2.2 } });
  // network mode: station markers, MLAT sigma circles, "heard by mine" rings - below the aircraft
  S.netv.install(map, 'ac-pred-sigma');
  applyVisibility();
}

function setVisible(ids, on) {
  for (const id of ids) if (S.map.getLayer(id)) S.map.setLayoutProperty(id, 'visibility', on ? 'visible' : 'none');
}

function applyVisibility() {
  const e = S.eff;
  setVisible(['rings', 'rings-outer', 'ring-labels'], e.rings);
  setVisible(['compass', 'compass-labels'], e.compass);
  setVisible(['beams-fill', 'beams-line', 'beam-axes', 'beam-labels'], e.beams);
  setVisible(['night'], e.night);
  setVisible(['coverage-fill', 'coverage-line'], e.coverage);
  setVisible(['trails', 'trails-live'], e.trails);
  setVisible(['radar'], e.radar);
  setVisible(['range-fill', 'range-robust', 'range-max'], e.range);
  setVisible(['terrain-line', 'terrain-labels'], e.terrain);
  setVisible(['apt-centerline', 'apt-runway', 'apt-dot', 'apt-label'], e.airports);
  setVisible(['heat'], e.heat);
  setVisible(['prox-line', 'prox-label'], e.prox);
  setVisible(['acas-dot', 'acas-label'], e.acas);
  setVisible(['winds-line', 'winds-dot', 'winds-label'], e.winds);
  setVisible(['pred-path', 'pred-tick', 'pred-tick-sigma', 'pred-tick-label'], e.predPaths !== 'off');
}

/** Set a GeoJSON source's data (cached so a base-map switch can restore it). */
function setData(id, data) {
  S.cache.set(id, data);
  const s = S.styleReady && S.map.getSource(id);
  if (s) s.setData(data);
}

// -----------------------------------------------------------------------------------------------
// overlays
// -----------------------------------------------------------------------------------------------
function pushOverlays() {
  if (!S.styleReady) return;
  const o = receiverOverlays(rxCfg(), units);
  S.outerKm = o.outerKm;
  setData('rings', o.rings);
  setData('ring-labels', o.ringLabels);
  setData('compass', o.compass);
  setData('compass-labels', o.compassLabels);
  setData('beams', o.beams);
  setData('beam-axes', o.beamAxes);
  setData('receiver', o.receiver);
  setData('coverage', coverageCollection(S.ctx.shared.get('map.coverage')));
  pushNight();
}

function pushNight() {
  if (S.styleReady && S.eff.night) setData('night', nightOverlay(S.ctx.live.serverNow()));
}

async function refreshRadar() {
  const { map } = S;
  clearTimeout(S.radarTimer);
  if (!S.eff.radar) {
    if (map.getLayer('radar')) map.removeLayer('radar');
    if (map.getSource('radar')) map.removeSource('radar');
    S.radarTime = 0;
    renderLegend();
    return;
  }
  const mine = S;
  try {
    const r = await fetch(RAINVIEWER_INDEX, { cache: 'no-store' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const j = await r.json();
    if (S !== mine || !S.eff.radar || !S.styleReady) return;
    const frame = j?.radar?.past?.[j.radar.past.length - 1];
    if (!frame || typeof j.host !== 'string' || !/^https:\/\/[a-z0-9.-]+$/i.test(j.host) || typeof frame.path !== 'string' || !frame.path.startsWith('/')) throw new Error('unexpected index format');
    // {host}{path}/{size}/{z}/{x}/{y}/{color}/{smooth}_{snow}.png - colour scheme 2, smoothed, snow shown
    const tiles = [`${j.host}${frame.path}/256/{z}/{x}/{y}/2/1_1.png`];
    const cur = map.getSource('radar');
    if (cur) cur.setTiles(tiles);
    else {
      map.addSource('radar', { type: 'raster', tiles, tileSize: 256, maxzoom: 7, attribution: RAINVIEWER_ATTRIBUTION });
      map.addLayer({ id: 'radar', type: 'raster', source: 'radar', paint: { 'raster-opacity': 0.6, 'raster-fade-duration': 150 } }, map.getLayer('coverage-fill') ? 'coverage-fill' : undefined);
    }
    S.radarTime = frame.time * 1000;
    S.radarFailed = false;
  } catch (e) {
    if (S !== mine) return;
    if (!S.radarFailed) ui.toast.warn('Rain radar is not available right now (' + e.message + ').');
    S.radarFailed = true;
    S.radarTime = 0;
  }
  renderLegend();
  S.radarTimer = setTimeout(refreshRadar, 5 * 60000);
}

// -----------------------------------------------------------------------------------------------
// aircraft features
// -----------------------------------------------------------------------------------------------
function labelFor(a, mode) {
  const cs = a.callsign || a.reg || a.icao;
  if (mode !== 'full') return cs;
  const alt = a.onGround ? 'GND' : units.alt(a.altBaro ?? a.altGeom, { fl: true });
  const parts = [];
  if (alt !== units.DASH) parts.push(alt.replace(' ', ''));
  if (typeof a.gs === 'number') parts.push(units.speed(a.gs).replace(' ', ''));
  return parts.length ? cs + '\n' + parts.join(' ') : cs;
}

const HL_RE = /^#[0-9a-f]{3,8}$/i;

/** Position of a prepared record at `now` (dead-reckoned when smooth motion is on), without blending. */
function reckon(p, now) {
  if (!S.eff.smooth || p.kmPerSec <= 0) return [p.lon, p.lat];
  const dt = Math.max(0, Math.min(p.maxDt, (now - p.tPos) / 1000));
  return sameWorld(geo.destination(p.lat, p.lon, p.track, p.kmPerSec * dt), p.lon);
}

const focusSet = () => {
  const out = [];
  if (S.selected) out.push(S.selected);
  for (const i of S.group) if (i !== S.selected) out.push(i);
  return out;
};

/** Turn the latest snapshot into per-aircraft render records (done once per snapshot). */
function prepare(snap) {
  const mode = S.eff.colorBy;
  const labels = S.eff.labels;
  const coastSec = S.eff.coastSec;
  const now = S.ctx.live.serverNow();
  const old = S.prepared;
  const prepared = new Map();
  const hidden = new Set();
  const fn = S.filterFn;
  let emergencies = 0;
  let predicted = 0;
  let mlatShown = 0;
  for (const a of snap.aircraft) {
    if (!hasPosition(a)) continue;
    if (((fn && !fn(a)) || !S.netv.filter(a)) && a.icao !== S.selected && !S.group.has(a.icao)) { hidden.add(a.icao); continue; }
    const shape = shapeFor(a);
    const stale = isStale(a);
    const emerg = isEmergency(a);
    if (emerg) emergencies++;
    const cp = coastProps(a, coastSec);
    if (cp) predicted++;
    const alt = a.onGround ? 0 : a.altBaro ?? a.altGeom ?? 0;
    const moving = !stale && typeof a.gs === 'number' && a.gs > 30 && typeof a.track === 'number';
    const hl = a.highlight && HL_RE.test(a.highlight.color || '') ? a.highlight.color : a.highlight ? '#ffd60a' : '';
    const ring = cp ? null : mlatRing(a); // a coasting prediction has no fix grade (glow instead of a ring)
    if (ring) mlatShown++;
    const rec = {
      a,
      lat: a.lat, lon: a.lon,
      // a predicted position is the filter's estimate for snapshot time; a real one is posAge old
      tPos: cp ? snap.now : snap.now - (a.posAge || 0) * 1000,
      maxDt: cp ? MAX_EXTRAPOLATE_PRED_S : MAX_EXTRAPOLATE_S,
      kmPerSec: moving ? (a.gs * 1.852) / 3600 : 0,
      track: a.track || 0,
      cur: [a.lon, a.lat],
      bx: 0, by: 0, bt: 0,
      props: {
        icao: a.icao,
        shape: imageName(shape),
        s: SHAPES[shape].scale,
        c: aircraftColor(mode, a),
        rot: typeof a.track === 'number' ? a.track : 0,
        stale: stale ? 1 : 0,
        mlat: a.source === 'mlat' ? 1 : 0,
        mc: ring ? ring.c : '',
        mw: ring ? ring.w : 0,
        emerg: emerg ? 1 : 0,
        pred: cp ? 1 : 0,
        gi: cp ? cp.gi : 0,
        po: cp ? cp.po : 1,
        r0: cp ? cp.r0 : 0,
        hl,
        ra: a.acas ? 1 : 0,
        mine: a.mine ? 1 : 0,
        label: a.acas ? (labels === 'none' ? a.callsign || a.icao : labelFor(a, labels)) + '\nRA ' + a.acas.advisory : labels === 'none' ? '' : labelFor(a, labels),
        sk: a.icao === S.selected ? -1e7 : emerg ? -1e6 : a.acas ? -9e5 : hl ? -5e5 : -alt,
      },
    };
    // blend the jump between the shown and the newly reported position over the next frames
    const prev = old.get(a.icao);
    if (prev && S.eff.smooth && S.styleReady) {
      const target = reckon(rec, now);
      const dx = prev.cur[0] - target[0];
      const dy = prev.cur[1] - target[1];
      if (Math.abs(dx) < BLEND_MAX_DEG && Math.abs(dy) < BLEND_MAX_DEG) { rec.bx = dx; rec.by = dy; rec.bt = now; }
    }
    // where the icon is drawn right now (the ticker moves it on from here), so paths pushed with this
    // snapshot start at the icon instead of at the raw reported position
    if (S.eff.smooth) rec.cur = rec.bt ? [prev.cur[0], prev.cur[1]] : reckon(rec, now);
    prepared.set(a.icao, rec);
  }
  S.prepared = prepared;
  S.hidden = hidden;
  S.emergencies = emergencies;
  S.predictedCount = predicted;
  S.mlatCount = mlatShown;
}

/** Dead-reckon every aircraft to `now` (when smooth motion is on) and push the aircraft + focus sources. */
function pushAircraft(now) {
  if (!S.styleReady) return;
  const smooth = S.eff.smooth;
  const features = [];
  const links = [];
  for (const p of S.prepared.values()) {
    p.cur = reckon(p, now);
    if (smooth && p.bt) {
      const k = 1 - (now - p.bt) / BLEND_MS;
      if (k > 0) { p.cur = [p.cur[0] + p.bx * k, p.cur[1] + p.by * k]; } else p.bt = 0;
    }
    features.push({ type: 'Feature', properties: p.props, geometry: { type: 'Point', coordinates: p.cur } });
    if (p.props.pred) {
      const l = linkFeature(p.a, p.cur);
      if (l) links.push(l, lastRealFeature(p.a, p.cur));
    }
  }
  setData('aircraft', fc(features));
  if (links.length || S.linksShown) { setData('pred-links', fc(links)); S.linksShown = links.length > 0; }

  const focus = [];
  const sel = S.selected && S.prepared.get(S.selected);
  if (sel) focus.push({ type: 'Feature', properties: { kind: 'sel' }, geometry: { type: 'Point', coordinates: sel.cur } });
  for (const icao of S.group) {
    const g = icao !== S.selected && S.prepared.get(icao);
    if (g) focus.push({ type: 'Feature', properties: { kind: 'grp' }, geometry: { type: 'Point', coordinates: g.cur } });
  }
  const hov = S.hover && S.hover !== S.selected && S.prepared.get(S.hover);
  if (hov) focus.push({ type: 'Feature', properties: { kind: 'hov' }, geometry: { type: 'Point', coordinates: hov.cur } });
  setData('focus', fc(focus));

  pushProximity();

  if (sel && S.following && !S.userMoving) {
    S.map.easeTo({ center: sel.cur, duration: smooth ? SMOOTH_TICK_MS : 950, easing: (t) => t, essential: true, offset: focusOffset() });
  }
}

function pushProximity() {
  const pairs = S.eff.prox ? S.extras.proximity.pairs : [];
  if (!pairs.length) { if (S.proxShown) { setData('prox', EMPTY); S.proxShown = false; } return; }
  const feats = [];
  for (const pr of pairs) {
    const a = S.prepared.get(pr.a);
    const b = S.prepared.get(pr.b);
    if (!a || !b) continue;
    const pb = sameWorld([b.cur[0], b.cur[1]], a.cur[0]);
    feats.push({ type: 'Feature', properties: { s: pr.severity }, geometry: { type: 'LineString', coordinates: [a.cur, pb] } });
    const t = `${units.dist(pr.hDistKm)} · ${units.alt(pr.vSepFt)}\n${pr.tcpa > 1 ? 'in ' + units.age(pr.tcpa) : 'now'}`;
    feats.push({ type: 'Feature', properties: { s: pr.severity, t }, geometry: { type: 'Point', coordinates: [(a.cur[0] + pb[0]) / 2, (a.cur[1] + pb[1]) / 2] } });
  }
  setData('prox', fc(feats));
  S.proxShown = true;
}

// -----------------------------------------------------------------------------------------------
// predicted paths
// -----------------------------------------------------------------------------------------------
/** Interpolate a server path (dt from t0) at `t` seconds after t0. */
function pathAt(pts, t) {
  if (t <= pts[0].dt) return pts[0];
  for (let i = 1; i < pts.length; i++) {
    if (t <= pts[i].dt) {
      const p0 = pts[i - 1];
      const p1 = pts[i];
      const f = (t - p0.dt) / (p1.dt - p0.dt || 1);
      const lon1 = sameWorld([p1.lon, p1.lat], p0.lon)[0];
      return { dt: t, lat: p0.lat + (p1.lat - p0.lat) * f, lon: p0.lon + (lon1 - p0.lon) * f, sh: num(p0.sh) && num(p1.sh) ? p0.sh + (p1.sh - p0.sh) * f : undefined };
    }
  }
  return null;
}

/** Re-anchor a server path at the aircraft's current position and resample it every 10 s from now. */
function serverPathFromNow(sp, p, now) {
  const el = Math.max(0, (now - sp.t0) / 1000);
  const end = sp.points[sp.points.length - 1].dt;
  const out = [{ dt: 0, lon: p.cur[0], lat: p.cur[1] }];
  for (let t = 10; el + t <= end + 1e-6; t += 10) {
    const q = pathAt(sp.points, el + t);
    if (!q) break;
    out.push({ ...q, dt: t });
  }
  return out;
}

function pushPaths() {
  if (!S.styleReady) return;
  const mode = S.eff.predPaths;
  if (mode === 'off') { if (S.pathsShown) { setData('pred-paths', EMPTY); setData('pred-ticks', EMPTY); S.pathsShown = false; } return; }
  const now = S.ctx.live.serverNow();
  const segs = [];
  const ticks = [];
  const focus = new Set(focusSet());
  for (const icao of focus) {
    const p = S.prepared.get(icao);
    if (!p || p.props.stale) continue;
    const sp = S.paths.get(icao);
    let pts = sp && sp.points && sp.points.length > 1 ? serverPathFromNow(sp, p, now) : null;
    if (!pts || pts.length < 2) pts = deadReckonPath(p.cur[0], p.cur[1], p.a, S.eff.pathSec, 10);
    pathFeatures(pts, { color: p.props.c, icao, ticks: true, tickEvery: S.eff.pathSec > 300 ? 60 : 30, major: true }, segs, ticks);
  }
  if (mode === 'all') {
    for (const [icao, p] of S.prepared) {
      if (focus.has(icao) || p.props.stale || p.kmPerSec <= 0) continue;
      // The path shape is dead-reckoned once per snapshot (per render record) and then translated so
      // it always starts at the icon's current, smoothly animated position.
      if (p.relPath === undefined) {
        const raw = deadReckonPath(p.cur[0], p.cur[1], p.a, ALL_PATH_HORIZON_S, 20);
        p.relPath = raw.length > 1 ? raw.map((q) => ({ dt: q.dt, dx: q.lon - raw[0].lon, dy: q.lat - raw[0].lat })) : null;
      }
      if (!p.relPath) continue;
      const pts = p.relPath.map((q) => ({ dt: q.dt, lon: p.cur[0] + q.dx, lat: p.cur[1] + q.dy }));
      pathFeatures(pts, { color: p.props.c, icao }, segs, ticks);
    }
  }
  S.lastPathsPush = performance.now();
  setData('pred-paths', fc(segs));
  setData('pred-ticks', fc(ticks));
  S.pathsShown = true;
}

/** Fetch server-side predicted paths for the selected / grouped aircraft (every 5 s each). */
function refreshServerPaths() {
  if (!S.serverPaths) return;
  const now = Date.now();
  const want = focusSet().slice(0, PATH_GROUP_MAX);
  for (const icao of [...S.paths.keys()]) if (!want.includes(icao)) S.paths.delete(icao);
  for (const icao of want) {
    const cur = S.paths.get(icao);
    if (cur && (cur.loading || now - cur.fetched < (cur.failed ? 15000 : PATH_REFRESH_MS))) continue;
    const p = S.prepared.get(icao);
    if (!p) continue;
    const rec = cur || { points: null, t0: 0 };
    rec.loading = true;
    S.paths.set(icao, rec);
    const mine = S;
    S.ctx.api.get('/api/prediction/path/' + icao, { params: { horizon: S.eff.pathSec, step: 10 }, signal: S.ctx.signal, timeout: 10000 })
      .then((r) => {
        if (S !== mine) return;
        rec.points = Array.isArray(r?.points) ? r.points.filter((q) => num(q.lat) && num(q.lon)) : null;
        rec.t0 = r?.t0 || Date.now();
        rec.terrain = r?.terrain || null;
        rec.failed = false;
        if (icao === S.selected) S.detail.setPath(r);
      })
      .catch((e) => {
        if (S !== mine || e.aborted) return;
        rec.points = null;
        rec.failed = true;
        if (e.status === 409) S.serverPaths = false; // prediction disabled: client-side paths only
        if (icao === S.selected) S.detail.setPath(null);
      })
      .finally(() => { rec.loading = false; rec.fetched = Date.now(); if (S === mine) pushPaths(); });
  }
}

// -----------------------------------------------------------------------------------------------
// trails
// -----------------------------------------------------------------------------------------------
/** Append coloured runs for pts[from..] to `groups` (Map colour -> array of coordinate arrays). */
function addRuns(groups, pts, from, mode, head) {
  let line = null;
  let color = null;
  let prevLon = 0;
  let prevT = 0;
  const start = (c, lon, lat) => {
    line = [[lon, lat]];
    color = c;
    let g = groups.get(c);
    if (!g) { g = []; groups.set(c, g); }
    g.push(line);
  };
  for (let i = Math.max(0, from); i < pts.length; i++) {
    const p = pts[i];
    const c = trailColor(mode, p);
    let lon = p[2];
    if (line) {
      const d = lon - prevLon;
      if (d > 180) lon -= 360; else if (d < -180) lon += 360;
      if (p[0] - prevT > TRAIL_GAP_MS) { line = null; lon = p[2]; }
    }
    if (!line) start(c, lon, p[1]);
    else if (c !== color) { line.push([lon, p[1]]); start(c, lon, p[1]); }
    else line.push([lon, p[1]]);
    prevLon = lon;
    prevT = p[0];
  }
  if (head && line) {
    let lon = head[0];
    const d = lon - prevLon;
    if (d > 180) lon -= 360; else if (d < -180) lon += 360;
    line.push([lon, head[1]]);
  }
}

function groupsToFeatures(groups) {
  const features = [];
  for (const [c, lines] of groups) {
    const coords = lines.filter((l) => l.length > 1);
    if (coords.length) features.push({ type: 'Feature', properties: { c }, geometry: { type: 'MultiLineString', coordinates: coords } });
  }
  return fc(features);
}

/** Rebuild the big, rarely-changing trail source and trim old points. */
function rebuildStaticTrails() {
  if (!S.styleReady) return;
  S.lastStatic = Date.now();
  const cut = S.ctx.live.serverNow() - S.eff.trailMinutes * 60000;
  const mode = S.eff.colorBy;
  const groups = new Map();
  for (const [icao, t] of S.trails) {
    let drop = 0;
    while (drop < t.pts.length && t.pts[drop][0] < cut) drop++;
    if (drop) t.pts.splice(0, drop);
    if (t.pts.length > MAX_TRAIL_POINTS) t.pts.splice(0, t.pts.length - MAX_TRAIL_POINTS);
    if (!t.pts.length && !S.prepared.has(icao)) { S.trails.delete(icao); continue; }
    if ((icao !== S.selected || !S.selTrail) && !S.hidden.has(icao)) addRuns(groups, t.pts, 0, mode, null);
    t.staticLen = t.pts.length;
  }
  if (S.eff.trails) setData('trails', groupsToFeatures(groups));
}

/** Push the small live part of every trail: points since the last static rebuild plus the head. */
function pushLiveTrails() {
  if (!S.styleReady) return;
  const mode = S.eff.colorBy;
  if (S.eff.trails) {
    const groups = new Map();
    for (const [icao, t] of S.trails) {
      const p = S.prepared.get(icao);
      if (!p || !t.pts.length) continue;
      if (icao === S.selected && S.selTrail) continue;
      // a coasting aircraft's trail ends at its last real position (the connector shows the rest)
      addRuns(groups, t.pts, t.staticLen - 1, mode, isStaleRecord(p) || p.props.pred ? null : p.cur);
    }
    setData('trails-live', groupsToFeatures(groups));
  }
  if (S.selected && S.selTrail) {
    const groups = new Map();
    const p = S.prepared.get(S.selected);
    addRuns(groups, S.selTrail, 0, mode, p && !isStaleRecord(p) && !p.props.pred ? p.cur : null);
    setData('trail-sel', groupsToFeatures(groups));
  } else {
    setData('trail-sel', EMPTY);
  }
}

const isStaleRecord = (p) => p.props.stale === 1;

function appendPoint(pts, p, a) {
  const last = pts[pts.length - 1];
  if (last && (p.tPos - last[0] < 1900 || (last[1] === a.lat && last[2] === a.lon))) return false;
  pts.push([p.tPos, a.lat, a.lon, a.onGround ? 0 : a.altBaro ?? a.altGeom ?? null, a.gs ?? null, a.rssi ?? null, a.source === 'adsb' ? 0 : a.source === 'mlat' ? 1 : a.source === 'tisb' || a.source === 'adsr' ? 2 : 3]);
  return true;
}

function extendTrails() {
  for (const [icao, p] of S.prepared) {
    // predicted positions never enter trails (same rule as on the server)
    if (p.props.stale || p.props.pred) continue;
    let t = S.trails.get(icao);
    if (!t) { t = { pts: [], staticLen: 0 }; S.trails.set(icao, t); }
    appendPoint(t.pts, p, p.a);
    if (icao === S.selected && S.selTrail) appendPoint(S.selTrail, p, p.a);
  }
  // an aircraft that left the live list takes its trail with it
  for (const icao of S.trails.keys()) {
    if (!S.prepared.has(icao) && !S.ctx.live.aircraft.has(icao)) { S.trails.delete(icao); S.staticDirty = true; }
  }
}

async function seedTrails() {
  const mine = S;
  const seq = ++S.seedSeq;
  try {
    const res = await S.ctx.api.trails(S.eff.trailMinutes, { signal: S.ctx.signal });
    if (S !== mine || seq !== S.seedSeq) return;
    for (const [icao, pts] of Object.entries(res.trails || {})) {
      if (!Array.isArray(pts) || !pts.length) continue;
      const cur = S.trails.get(icao);
      const lastT = pts[pts.length - 1][0];
      const tail = cur ? cur.pts.filter((p) => p[0] > lastT + 1500) : [];
      S.trails.set(icao, { pts: pts.concat(tail), staticLen: 0 });
    }
    rebuildStaticTrails();
    pushLiveTrails();
  } catch (e) {
    if (!e.aborted) console.warn('[map] could not load trails:', e.message);
  }
}

// -----------------------------------------------------------------------------------------------
// full stored trace (today) and route line of the selected aircraft
// -----------------------------------------------------------------------------------------------
async function loadTrace(on) {
  const mine = S;
  const seq = ++S.traceSeq;
  if (!on || !S.selected) { S.trace = null; setData('trace', EMPTY); return null; }
  const icao = S.selected;
  try {
    let flights = S.detail.flights;
    if (!flights.length) {
      const d = await S.ctx.api.get('/api/history/aircraft/' + icao, { params: { limit: 20 }, signal: S.ctx.signal });
      flights = Array.isArray(d?.flights) ? d.flights : [];
    }
    const midnight = new Date();
    midnight.setHours(0, 0, 0, 0);
    const today = flights.filter((f) => (f.end || f.start || 0) >= midnight.getTime()).slice(0, 8);
    const traces = await Promise.all(today.map((f) => S.ctx.api.get(`/api/history/flights/${f.id}/trace`, { signal: S.ctx.signal, timeout: 30000 }).catch(() => null)));
    if (S !== mine || seq !== S.traceSeq || S.selected !== icao) return null;
    const pts = [];
    for (const t of traces) for (const q of t?.points || []) if (num(q[1]) && num(q[2])) pts.push([q[0], q[1], q[2], q[3], q[4], q[6], q[7]]);
    pts.sort((x, y) => x[0] - y[0]);
    S.trace = { icao, pts };
    const groups = new Map();
    addRuns(groups, pts, 0, S.eff.colorBy, null);
    setData('trace', groupsToFeatures(groups));
    if (pts.length > 1 && !S.following) {
      const b = bounds(pts.map((q) => [q[2], q[1]]));
      if (b) S.map.fitBounds([[b[0], b[1]], [b[2], b[3]]], { padding: viewPadding(), maxZoom: 10, duration: 600 });
    }
    return { points: pts.length, flights: today.length };
  } catch (e) {
    if (S !== mine || e.aborted) return null;
    ui.toast.error('Could not load the stored trace: ' + e.message);
    return null;
  }
}

function showRoute(info) {
  if (!S) return;
  const p = S.selected && S.prepared.get(S.selected);
  if (!info || !Array.isArray(info.path) || info.path.length < 2) { setData('route', EMPTY); return; }
  const feats = [{ type: 'Feature', properties: { kind: 'line', ok: info.check && info.check.doubtful ? 0 : 1 }, geometry: { type: 'LineString', coordinates: info.path } }];
  const aps = info.airports || [];
  for (const [i, ap] of aps.entries()) {
    const a = ap.airport;
    if (!a || !num(a.lat) || !num(a.lon)) continue;
    const ref = i === 0 ? info.path[0][0] : info.path[info.path.length - 1][0];
    feats.push({ type: 'Feature', properties: { kind: 'ap', text: a.iata || a.icao || ap.code }, geometry: { type: 'Point', coordinates: sameWorld([a.lon, a.lat], p ? p.cur[0] : ref) } });
  }
  setData('route', fc(feats));
}

// -----------------------------------------------------------------------------------------------
// selection, hover, view
// -----------------------------------------------------------------------------------------------
function select(icao, opts = {}) {
  icao = icao ? String(icao).toUpperCase() : null;
  if (icao && !/^[0-9A-F]{6}$/.test(icao)) icao = null;
  if (icao === S.selected) {
    if (icao && opts.center) centerOn(icao);
    return;
  }
  if (!opts.keepGroup) S.group.clear();
  S.selected = icao;
  S.selTrail = null;
  S.following = false;
  S.trace = null;
  S.traceSeq++;
  setData('trace', EMPTY);
  setData('route', EMPTY);
  S.root.classList.toggle('has-detail', !!icao);
  S.list.setSelected(icao);
  if (!opts.fromRoute) S.ctx.navigate(icao ? 'map/' + icao : 'map');
  if (!icao) {
    S.detail.hide();
    S.staticDirty = true;
  } else {
    const mine = S;
    if (opts.reveal !== false) S.list.reveal(icao);
    S.detail.show(icao).then((d) => {
      if (S !== mine || S.selected !== icao || !d) return;
      const live = S.trails.get(icao);
      const base = Array.isArray(d.trail) ? d.trail.slice() : [];
      const lastT = base.length ? base[base.length - 1][0] : 0;
      S.selTrail = base.concat(live ? live.pts.filter((p) => p[0] > lastT + 1500) : []);
      S.staticDirty = true;
      refreshNow();
    });
    if (opts.center) centerOn(icao);
  }
  if (S.snapshot) prepare(S.snapshot);
  refreshServerPaths();
  renderGroupBar();
  S.list.refresh();
  refreshNow();
}

/** Shift / Ctrl / Cmd click: add or remove an aircraft from the multi-selection. */
function toggleGroup(icao) {
  icao = String(icao).toUpperCase();
  if (!S.selected) { select(icao, { reveal: true }); return; }
  if (icao === S.selected) {
    const next = [...S.group].find((i) => i !== icao) || null;
    if (next) S.group.delete(next);
    select(next, { keepGroup: true });
    return;
  }
  if (S.group.has(icao)) S.group.delete(icao); else S.group.add(icao);
  if (S.snapshot) prepare(S.snapshot);
  refreshServerPaths();
  renderGroupBar();
  S.list.refresh();
  refreshNow();
  pushPaths();
}

function clearGroup() {
  if (!S.group.size) return false;
  S.group.clear();
  renderGroupBar();
  S.list.refresh();
  if (S.snapshot) prepare(S.snapshot);
  refreshNow();
  pushPaths();
  return true;
}

function renderGroupBar() {
  const n = S.group.size + (S.selected && !S.group.has(S.selected) ? 1 : 0);
  S.el.group.classList.toggle('hidden', S.group.size === 0);
  setText(S.el.groupCount, `${n} aircraft selected`);
}

function fitGroup() {
  const pts = focusSet().map((i) => S.prepared.get(i)).filter(Boolean).map((p) => p.cur);
  const b = bounds(pts);
  if (!b) return;
  const pad = 0.05;
  S.map.fitBounds([[b[0] - pad, b[1] - pad], [b[2] + pad, b[3] + pad]], { padding: viewPadding(), maxZoom: 10, duration: 600 });
}

function refreshNow() {
  if (!S.styleReady) return;
  if (S.staticDirty) { S.staticDirty = false; rebuildStaticTrails(); }
  pushAircraft(S.ctx.live.serverNow());
  pushLiveTrails();
}

/** Pixel offset that keeps a centred aircraft clear of the floating detail panel on wide layouts. */
function focusOffset() {
  const wide = S.root.clientWidth > 900;
  if (!S.selected) return [0, 0];
  // wide: the panel floats on the left; narrow: it is a bottom sheet
  return wide ? [Math.round((S.detail.el.offsetWidth + 12) / 2), 0] : [0, -Math.round(S.detail.el.offsetHeight / 2)];
}

function centerOn(icao) {
  const p = S.prepared.get(icao);
  if (p) S.map.easeTo({ center: p.cur, zoom: Math.max(S.map.getZoom(), 7), duration: 600, offset: focusOffset() });
}

function viewPadding() {
  const wide = S.root.clientWidth > 900;
  return { top: 56, bottom: 56, left: wide && S.selected ? 400 : 48, right: 64 };
}

function fitTraffic(animate = true) {
  const pts = [];
  for (const p of S.prepared.values()) pts.push([p.lon, p.lat]);
  const rx = rxCfg()?.receiver;
  if (rx?.located) pts.push([rx.lon, rx.lat]);
  const b = bounds(pts);
  if (!b) { ui.toast('No aircraft positions to fit yet.'); return false; }
  const pad = 0.05;
  S.map.fitBounds([[b[0] - pad, Math.max(-85, b[1] - pad)], [b[2] + pad, Math.min(85, b[3] + pad)]], { padding: viewPadding(), maxZoom: 10, duration: animate ? 700 : 0 });
  return true;
}

function fitReceiver(animate = true) {
  const rx = rxCfg()?.receiver;
  if (!rx?.located) return false;
  const km = (S.outerKm || 300) * 1.16;
  const ring = geo.circle(rx.lat, rx.lon, km, 16);
  const b = bounds(ring);
  S.map.fitBounds([[b[0], Math.max(-85, b[1])], [b[2], Math.min(85, b[3])]], { padding: 24, duration: animate ? 700 : 0 });
  return true;
}

/** Receiver position: the configured one, else the terrain analysis' station (demo / GPS). */
async function rxPos() {
  const rx = rxCfg()?.receiver;
  if (rx?.located && num(rx.lat) && num(rx.lon)) return { lat: rx.lat, lon: rx.lon };
  if (net.aggregator) return null; // no host-station fallback in network mode
  if (S.rxFallback) return S.rxFallback;
  try {
    const r = await S.ctx.api.get('/api/terrain/coverage', { signal: S.ctx.signal, timeout: 15000 });
    if (S && num(r?.lat) && num(r?.lon)) { S.rxFallback = { lat: r.lat, lon: r.lon }; return S.rxFallback; }
  } catch { /* not available */ }
  return null;
}

function pickAt(point) {
  const r = 9;
  const hits = S.map.queryRenderedFeatures([[point.x - r, point.y - r], [point.x + r, point.y + r]], { layers: ['ac-icons'] });
  if (!hits.length) return null;
  let best = null;
  let bestD = Infinity;
  for (const f of hits) {
    const p = S.prepared.get(f.properties.icao);
    if (!p) continue;
    const s = S.map.project(p.cur);
    const d = (s.x - point.x) ** 2 + (s.y - point.y) ** 2;
    if (d < bestD) { bestD = d; best = f.properties.icao; }
  }
  return best;
}

function setHover(icao, fromList) {
  if (S.hover === icao) return;
  S.hover = icao;
  if (!fromList) S.list.setHover(icao);
  S.map.getCanvas().style.cursor = S.measure.active ? 'crosshair' : icao && !fromList ? 'pointer' : '';
  const p = icao && S.prepared.get(icao);
  if (p && !fromList) {
    const a = p.a;
    const alt = a.onGround ? 'GND' : units.alt(a.altBaro ?? a.altGeom, { fl: true });
    setText(S.el.tipTitle, a.callsign || a.icao);
    setText(S.el.tipSub, [a.predicted ? `predicted · ${units.age(a.coastS)}` : null, a.type, alt !== units.DASH ? alt : null, typeof a.gs === 'number' ? units.speed(a.gs) : null, typeof a.distKm === 'number' ? units.dist(a.distKm) : null].filter(Boolean).join(' · '));
    const s = S.map.project(p.cur);
    S.el.tip.style.transform = `translate(${Math.round(s.x + 16)}px, ${Math.round(s.y - 14)}px)`;
    S.el.tip.classList.remove('hidden');
  } else {
    S.el.tip.classList.add('hidden');
  }
  if (S.styleReady) pushAircraft(S.ctx.live.serverNow());
}

// -----------------------------------------------------------------------------------------------
// filters, URL
// -----------------------------------------------------------------------------------------------
function setFilter(f, opts = {}) {
  S.filter = filters.normalize(f);
  S.filterFn = filters.compile(S.filter);
  if (opts.persist !== false) S.prefs.set({ filter: S.filter });
  renderChips();
  S.filterBtn.classList.toggle('has-dot', filters.isActive(S.filter));
  if (S.snapshot) prepare(S.snapshot);
  S.list.reapply();
  S.staticDirty = true;
  refreshNow();
  pushPaths();
  writeUrl();
}

function renderChips() {
  const list = filters.chips(S.filter);
  S.el.chips.classList.toggle('hidden', !list.length);
  if (!list.length) { ui.clear(S.el.chips); return; }
  ui.mount(S.el.chips,
    h('span.mapv-chips-ic', icon('filter')),
    list.map((c) => h('button.mapv-chip', { type: 'button', tip: 'Remove this filter', onclick: () => {
      const f = { ...S.filter };
      for (const k of c.key) f[k] = k.endsWith('Min') || k.endsWith('Max') ? null : typeof f[k] === 'boolean' ? false : '';
      setFilter(f);
    } }, c.label, icon('x'))),
    list.length > 1 ? h('button.mapv-chip.mapv-chip--clear', { type: 'button', onclick: () => setFilter({}) }, 'Clear all') : null);
}

/** Keep #/map[/ICAO]?zoom&lat&lon&filter&kiosk in sync without adding history entries. */
function writeUrl() {
  if (!S || !S.map || !location.hash.startsWith('#/map')) return;
  const c = S.map.getCenter();
  const q = new URLSearchParams();
  q.set('zoom', S.map.getZoom().toFixed(2));
  q.set('lat', c.lat.toFixed(4));
  q.set('lon', c.lng.toFixed(4));
  const fp = filters.toParam(S.filter);
  if (fp) q.set('filter', fp);
  if (S.kiosk) q.set('kiosk', '1');
  const hash = '#/map' + (S.selected ? '/' + S.selected : '') + '?' + q.toString();
  if (location.hash !== hash) history.replaceState(history.state, '', hash);
}

/** Apply zoom / lat / lon / filter / kiosk from a route's query. Returns true when the view was set. */
function applyQuery(query, opts = {}) {
  let viewSet = false;
  const z = Number(query.zoom);
  const lat = Number(query.lat);
  const lon = Number(query.lon);
  if (query.lat !== undefined && query.lon !== undefined && num(lat) && num(lon) && Math.abs(lat) <= 85 && Math.abs(lon) <= 540) {
    const zoom = query.zoom !== undefined && num(z) ? Math.max(1.2, Math.min(17, z)) : S.map.getZoom();
    S.map.jumpTo({ center: [lon, lat], zoom });
    viewSet = true;
  } else if (query.zoom !== undefined && num(z)) {
    S.map.jumpTo({ zoom: Math.max(1.2, Math.min(17, z)) });
    viewSet = true;
  }
  if (typeof query.filter === 'string') setFilter(filters.fromParam(query.filter));
  if (query.kiosk !== undefined) setKiosk(query.kiosk !== '0', opts);
  return viewSet;
}

/** Alert sound muted: the app-wide switch when js/alertsound.js exists, else the map's own preference. */
function isMuted() {
  const snd = alertSound();
  if (snd) { try { return !!snd.isMuted(); } catch { /* fall through */ } }
  return !!(S && S.eff.mute);
}

/** The shell's kiosk control (ctx.shell.setKiosk / .kiosk, `kiosk` class on <html>) when this shell has one. */
const shellKiosk = () => (S && S.ctx.shell && typeof S.ctx.shell.setKiosk === 'function' ? S.ctx.shell : null);

/**
 * Kiosk mode. With a shell that supports it, the shell hides its own chrome (sidebar, top bar) and the
 * map only hides its controls; otherwise (older shell) the map covers the window as a fixed overlay.
 */
function setKiosk(on) {
  S.kiosk = !!on;
  const shell = shellKiosk();
  if (shell) {
    if (!!shell.kiosk !== S.kiosk) shell.setKiosk(S.kiosk);
  }
  S.root.classList.toggle('kiosk', S.kiosk);
  S.root.classList.toggle('kiosk-shell', !!shell);
  if (S.kiosk && S.panel) S.panel.close();
  setTimeout(() => { if (S) S.map.resize(); }, 30);
  writeUrl();
}

/** Follow kiosk changes made by the shell itself (its exit button, Escape). Returns an unsubscribe function. */
function watchShellKiosk() {
  const shell = shellKiosk();
  if (!shell) return null;
  const apply = (on) => {
    if (!S || !!on === S.kiosk) return;
    S.kiosk = !!on;
    S.root.classList.toggle('kiosk', S.kiosk);
    S.root.classList.add('kiosk-shell');
    if (S.kiosk && S.panel) S.panel.close();
    setTimeout(() => { if (S) S.map.resize(); }, 30);
    writeUrl();
  };
  if (typeof shell.onKiosk === 'function') return shell.onKiosk(apply);
  if (typeof MutationObserver !== 'function') return null;
  const html = document.documentElement;
  const mo = new MutationObserver(() => apply(html.classList.contains('kiosk')));
  mo.observe(html, { attributes: true, attributeFilter: ['class'] });
  return () => mo.disconnect();
}

// -----------------------------------------------------------------------------------------------
// UI: controls, legend, call to action
// -----------------------------------------------------------------------------------------------
function renderLegend() {
  const lg = legendFor(S.eff.colorBy, units);
  const body = lg.items
    ? h('div.lg-items', lg.items.map((it) => h('span.lg-item', h('i', { style: { background: it.color } }), it.label)))
    : h('div.lg-ramp', h('div.lg-bar', { style: { background: lg.gradient } }), h('div.lg-ticks', lg.ticks.map((t) => h('span', { style: { left: t.pos * 100 + '%' } }, t.label))));
  const sym = [];
  if (S.predictedCount || S.eff.predPaths !== 'off') sym.push(h('span.lg-sym', { tip: 'No position for a few seconds: shown where the tracking filter predicts it; the glow fades as the prediction ages, the circle is the 1σ uncertainty' }, h('i.lg-glow'), 'Predicted (glow)'));
  if (S.eff.predPaths !== 'off') sym.push(h('span.lg-sym', h('i.lg-dash'), 'Predicted path'));
  if (S.hasHighlights) sym.push(h('span.lg-sym', h('i.lg-ring'), 'Alert rule'));
  if (S.hasRA) sym.push(h('span.lg-sym', { tip: 'The aircraft reports an active TCAS resolution advisory (its label shows the advisory)' }, h('i.lg-ring', { style: { borderColor: '#ff453a' } }), 'TCAS RA'));
  if (S.mlatCount && S.legendMlat) {
    sym.push(h('span.lg-sym.lg-mlat-title', { tip: 'Multilateration: positions the network computes from the arrival times at several stations. The ring colour is the quality grade (1σ horizontal error).' }, 'MLAT'));
    if (S.legendMlat & 1) for (const g of GRADES) sym.push(h('span.lg-sym', { tip: `Grade ${g.label}: ${g.text}` }, h('i.lg-mring', { style: { borderColor: g.color, borderWidth: g.width + 'px' } }), g.label));
    if (S.mlatUngraded) sym.push(h('span.lg-sym', { tip: 'MLAT from an external server, without a grade' }, h('i.lg-mring', { style: { borderColor: MLAT_UNGRADED } }), 'ext.'));
  }
  const notes = [];
  for (const [key, label] of [['range', 'Range'], ['terrain', 'Terrain'], ['airports', 'Airports'], ['heat', 'Heat map'], ['prox', 'Proximity'], ['acas', 'TCAS RAs'], ['winds', 'Winds aloft']]) {
    if (!S.eff[key]) continue;
    const nt = S.extras.notes.get(key);
    if (nt && nt.text && nt.state !== 'ok') notes.push(h('div.lg-note' + (nt.state === 'error' ? '.err' : ''), icon(nt.state === 'error' ? 'alert' : nt.state === 'loading' ? 'refresh' : 'info'), `${label}: ${nt.text}`));
  }
  ui.mount(S.el.legend,
    h('div.lg-title', lg.title),
    body,
    sym.length ? h('div.lg-syms', sym) : null,
    S.eff.radar && S.radarTime ? h('div.lg-note', icon('rain'), `Radar ${units.clock(S.radarTime, { seconds: false })}`) : null,
    notes);
}

function openLayersPanel(anchor) {
  if (S.panel) { S.panel.close(); return; }
  const e = S.eff;
  const set = (patch, after) => { S.prefs.set(patch); S.eff = effective(S); if (after) after(); };
  const row = (label, control, hint) => ui.field({ label, control, row: true, hint });
  const noteOf = (key) => { const nt = S.extras.notes.get(key); return nt && nt.text ? nt.text : null; };
  const tgl = (key, label, after, hint) => row(label, ui.toggle({ checked: e[key], onChange: (v) => set({ [key]: v }, () => { applyVisibility(); renderLegend(); if (after) after(); }) }), hint);
  const hasCoverage = !!S.ctx.shared.get('map.coverage');
  const rxOk = !S.netv.hidesReceiver();

  const body = h('div.mapv-panel',
    ui.section('Base map', 'globe'),
    ui.select({ options: BASEMAPS.map((b) => ({ value: b.id, label: b.label })), value: e.basemap.id, onChange: (v) => set({ basemap: v }, switchBasemap) }),

    ui.section('Colour by', 'layers'),
    ui.segmented({ block: true, value: e.colorBy, options: COLOR_MODES, onChange: (v) => set({ colorBy: v }, () => { renderLegend(); recolour(); }) }),

    ui.section('Labels', 'plane'),
    ui.segmented({ block: true, value: e.labels, options: [{ value: 'none', label: 'Off' }, { value: 'callsign', label: 'Callsign' }, { value: 'full', label: 'Detailed' }], onChange: (v) => set({ labels: v }, applyLabels) }),

    ui.section('Trails', 'route'),
    row('Show trails', ui.toggle({ checked: e.trails, onChange: (v) => set({ trails: v }, () => { applyVisibility(); S.staticDirty = true; refreshNow(); }) })),
    ui.field({ label: 'Trail length', control: ui.slider({ min: 1, max: 120, step: 1, value: e.trailMinutes, format: (v) => v + ' min', onInput: (v) => set({ trailMinutes: v }, S.reseed) }) }),

    ui.section('Prediction', 'target'),
    ui.field({ label: 'Predicted paths (P)', hint: 'Dashed lines ahead of the aircraft with time ticks', control: ui.segmented({ block: true, value: e.predPaths, options: [{ value: 'off', label: 'Off' }, { value: 'selected', label: 'Selected' }, { value: 'all', label: 'All' }], onChange: (v) => set({ predPaths: v }, () => { applyVisibility(); renderLegend(); pushPaths(); }) }) }),
    tgl('prox', 'Proximity pairs', () => S.extras.prox(), noteOf('prox') || 'Predicted close approaches (informational)'),

    ui.section('Overlays', 'radar'),
    rxOk ? [
      tgl('rings', 'Range rings'),
      tgl('beams', 'Antenna beams'),
      tgl('compass', 'Compass rose'),
    ] : null,
    tgl('night', 'Day / night', pushNight),
    tgl('radar', 'Rain radar', refreshRadar, 'RainViewer, needs internet'),
    rxOk ? [
      row('Coverage', ui.toggle({ checked: e.coverage, disabled: !hasCoverage, onChange: (v) => set({ coverage: v }, applyVisibility) }), hasCoverage ? null : 'Published by the Antenna page'),
      tgl('range', 'Measured range', () => S.extras.range(), noteOf('range') || 'Actual coverage outline from received positions'),
      tgl('terrain', 'Terrain coverage', () => S.extras.terrain(S.eff.terrainKind), noteOf('terrain') || 'Theoretical range per altitude'),
      ui.segmented({ block: true, value: e.terrainKind, options: [{ value: 'los', label: 'Line of sight', tip: 'Geometric line of sight with refraction' }, { value: 'radio', label: 'Radio', tip: 'Link budget incl. diffraction' }], onChange: (v) => set({ terrainKind: v }, () => S.extras.terrain(v)) }),
    ] : null,
    tgl('airports', 'Airports & runways', () => S.extras.airports(), noteOf('airports') || 'From zoom 5; runways and centrelines from zoom 8'),
    rxOk ? tgl('heat', 'Heat map (all time)', () => S.extras.heat(), noteOf('heat') || 'Every position ever received') : null,
    rxOk ? row('Heat map picture', ui.button({ label: '8K JPEG', icon: 'download', size: 'sm', href: '/api/antenna/heat.jpg?download=1', tip: '7680 x 4320 picture of the all-time heat map with range rings and scale (takes a few seconds)' }), 'For printing and sharing') : null,
    tgl('acas', 'TCAS RAs (24 h)', () => S.extras.acas(), noteOf('acas') || 'Where aircraft reported a resolution advisory'),
    tgl('winds', 'Winds aloft', () => S.extras.winds(S.eff.windsFL), noteOf('winds') || 'Wind and temperature from aircraft (Mode S EHS), last hour'),
    ui.segmented({ block: true, value: e.windsFL, options: [100, 180, 240, 300, 340, 390].map((v) => ({ value: v, label: 'FL' + v })), onChange: (v) => set({ windsFL: +v }, () => S.extras.winds(+v)) }),
    net.aggregator ? ui.field({ label: 'Load aircraft', hint: `"Visible area" saves bandwidth on large networks; the list then shows the aircraft in view only. Automatic switches to it above ${VIEWPORT_ON} aircraft.`,
      control: ui.segmented({ block: true, value: S.prefs.get().viewport || 'auto', options: [{ value: 'auto', label: 'Automatic' }, { value: 'on', label: 'Visible area' }, { value: 'off', label: 'Everything' }],
        onChange: (v) => { S.prefs.set({ viewport: v }); syncViewport(true); } }) }) : null,
    net.aggregator ? row('MLAT uncertainty', ui.toggle({ checked: S.netv.sigma, onChange: (v) => { S.netv.setSigma(v); if (S.snapshot) S.netv.update(S.prepared); } }), 'Circle of the 1σ position error, coloured by grade') : null,
    !rxOk ? h('p.mapv-panel-note', 'Range rings, beams, coverage and the heat map belong to one station: choose the Station view.') : null,

    ui.section('Motion', 'bolt'),
    row('Smooth motion', ui.toggle({ checked: e.smooth, onChange: (v) => set({ smooth: v }, restartTicker) }), e.lowPower ? 'Off by default in low-power mode' : 'Glide between 1 s updates'),
    h('div.mapv-panel-foot', ui.button({ label: 'Reset map settings', size: 'sm', variant: 'ghost', icon: 'refresh', onClick: () => {
      const keepFilter = S.prefs.get().filter;
      S.prefs.reset();
      S.prefs.set({ filter: keepFilter });
      S.eff = effective(S);
      S.panel.close();
      switchBasemap();
      renderLegend();
      restartTicker();
      S.reseed();
      S.extras.reloadAll(S.eff.terrainKind);
    } })));

  const owner = S;
  S.panel = ui.popover(anchor, h('div.mapv-panel-scroll', body), { placement: 'left-start', onClose: () => { owner.panel = null; anchor.setAttribute('aria-pressed', 'false'); } });
  anchor.setAttribute('aria-pressed', 'true');
}

function openFilters(anchor) {
  if (S.filterPop) { S.filterPop.close(); return; }
  const owner = S;
  anchor.setAttribute('aria-pressed', 'true');
  S.filterPop = filters.openFilterPanel(anchor, S.filter, (f) => setFilter(f), { onClose: () => { owner.filterPop = null; anchor.setAttribute('aria-pressed', 'false'); } });
}

function openMore(anchor) {
  const mins = S.eff.trailMinutes;
  ui.menu(anchor, [
    { label: 'Export picture as KML', icon: 'download', onClick: () => download(`/api/export/live.kml?minutes=${mins}`) },
    { label: 'Export picture as GeoJSON', icon: 'download', onClick: () => download(`/api/export/live.geojson?minutes=${mins}`) },
    'sep',
    { label: 'Replay history', icon: 'replay', onClick: () => S.ctx.navigate('reports/replay') },
    { label: S.kiosk ? 'Leave kiosk mode' : 'Kiosk mode (K)', icon: 'maximize', onClick: () => setKiosk(!S.kiosk) },
    { label: 'Copy link to this view', icon: 'link', onClick: async () => { writeUrl(); (await ui.copy(location.href)) ? ui.toast.success('Link copied') : ui.toast.warn('Could not copy the link'); } },
    { label: 'Keyboard shortcuts (?)', icon: 'help', onClick: () => openHelp() },
  ], { placement: 'left-start' });
}

const SHORTCUTS = [
  ['L', 'Cycle labels (off / callsign / detailed)'],
  ['T', 'Trails on / off'],
  ['P', 'Predicted paths: off / selected / all'],
  ['F', 'Follow the selected aircraft'],
  ['R', 'Reset the view (receiver, else all traffic)'],
  ['M', 'Measuring tool'],
  ['K', 'Kiosk mode (hide all chrome)'],
  ['/', 'Search the aircraft list'],
  ['Esc', 'Close tool / clear multi-selection / deselect / leave kiosk'],
  ['Shift / Ctrl + click', 'Add or remove an aircraft from the multi-selection'],
  ['?', 'This help'],
];

function openHelp() {
  if (S.helpOpen) return;
  S.helpOpen = true;
  const m = ui.modal({
    title: 'Live Map shortcuts', icon: 'help', width: 460,
    body: h('table.mapv-keys', h('tbody', SHORTCUTS.map(([k, d]) => h('tr', h('td', k.split(' / ').map((x, i) => [i ? ' / ' : null, h('kbd', x)])), h('td', d))))),
    actions: [{ label: 'Close', variant: 'primary' }],
    onClose: () => { if (S) S.helpOpen = false; },
  });
  return m;
}

function recolour() {
  if (S.snapshot) prepare(S.snapshot);
  S.staticDirty = true;
  refreshNow();
  pushPaths();
  if (S.trace) { const g = new Map(); addRuns(g, S.trace.pts, 0, S.eff.colorBy, null); setData('trace', groupsToFeatures(g)); }
  S.list.refresh();
  S.detail.refresh();
}

function applyLabels() {
  if (S.map.getLayer('ac-icons')) S.map.setLayoutProperty('ac-icons', 'text-field', S.eff.labels === 'none' ? '' : ['get', 'label']);
  if (S.snapshot) prepare(S.snapshot);
  refreshNow();
}

function switchBasemap() {
  S.styleReady = false;
  S.map.setStyle(styleFor(S.eff.basemap), { diff: false });
  S.root.classList.toggle('light-map', !S.eff.basemap.dark);
}

function renderCta() {
  const st = S.ctx.status;
  const show = st && !st.located && !S.ctaDismissed && !net.aggregator;
  S.el.cta.classList.toggle('hidden', !show);
}

function restartTicker() {
  clearInterval(S.ticker);
  S.ticker = 0;
  if (S.eff.smooth) {
    S.ticker = setInterval(() => {
      if (document.visibilityState !== 'visible' || !S.styleReady || !S.prepared.size) return;
      const now = S.ctx.live.serverNow();
      pushAircraft(now);
      pushLiveTrails();
      // predicted paths follow the gliding icons: the few focused ones every frame, "all" throttled
      // by traffic (and harder in low-power mode) since it rebuilds a feature per path segment
      if (S.eff.predPaths === 'selected' && (S.selected || S.group.size)) pushPaths();
      else if (S.eff.predPaths === 'all') {
        const every = S.eff.lowPower ? 500 : S.prepared.size > 250 ? 375 : S.prepared.size > 120 ? 250 : 0;
        if (performance.now() - (S.lastPathsPush || 0) >= every - 5) pushPaths();
      }
    }, SMOOTH_TICK_MS);
  } else if (S.snapshot) {
    refreshNow();
  }
}

/** One animation loop for the emergency pulse and the coasting glow (~30 fps, off in low-power mode). */
function animate(t) {
  if (!S) return;
  S.animRaf = 0;
  const glow = S.predictedCount > 0 && !S.eff.lowPower;
  const emerg = S.emergencies > 0;
  if ((!glow && !emerg) || !S.styleReady || document.visibilityState !== 'visible') { S.animating = false; resetGlow(); return; }
  if (t - S.animLast >= ANIM_FRAME_MS) {
    S.animLast = t;
    if (emerg && S.map.getLayer('ac-emerg')) {
      const ph = (t % 1400) / 1400;
      S.map.setPaintProperty('ac-emerg', 'circle-radius', 12 + 22 * ph);
      S.map.setPaintProperty('ac-emerg', 'circle-opacity', 0.3 * (1 - ph));
      S.map.setPaintProperty('ac-emerg', 'circle-stroke-opacity', 1 - ph * ph);
    }
    if (glow && S.map.getLayer('ac-pred-glow')) {
      const s = 0.5 + 0.5 * Math.sin((t / 1800) * Math.PI * 2);
      const r = 16 + 9 * s;
      S.map.setPaintProperty('ac-pred-glow', 'circle-radius', ['interpolate', ['linear'], ['zoom'], 3, 0.55 * r, 9, r, 13, 1.25 * r]);
      S.map.setPaintProperty('ac-pred-glow', 'circle-opacity', ['*', 0.45 + 0.45 * s, ['get', 'gi']]);
      S.glowAnimated = true;
    }
  }
  S.animRaf = requestAnimationFrame(animate);
}

function resetGlow() {
  if (!S.glowAnimated || !S.styleReady || !S.map.getLayer('ac-pred-glow')) return;
  S.glowAnimated = false;
  S.map.setPaintProperty('ac-pred-glow', 'circle-radius', ['interpolate', ['linear'], ['zoom'], 3, 0.55 * 17, 9, 17, 13, 1.25 * 17]);
  S.map.setPaintProperty('ac-pred-glow', 'circle-opacity', ['*', 0.55, ['get', 'gi']]);
}

function ensureAnim() {
  if (S.animating) return;
  const glow = S.predictedCount > 0 && !S.eff.lowPower;
  const emerg = S.emergencies > 0 && !S.eff.lowPower;
  if (!glow && !emerg) return;
  S.animating = true;
  S.animRaf = requestAnimationFrame(animate);
}

// -----------------------------------------------------------------------------------------------
// live data
// -----------------------------------------------------------------------------------------------
function onSnapshot(snap) {
  S.snapshot = snap;
  prepare(snap);
  extendTrails();
  S.list.update(snap);
  S.detail.update(snap.now);
  const hl = snap.aircraft.some((a) => a.highlight);
  const ra = snap.aircraft.some((a) => a.acas);
  if (ra !== !!S.hasRA) { S.hasRA = ra; renderLegend(); }
  const predNow = S.predictedCount > 0;
  let mlatKey = 0;
  if (S.mlatCount) for (const a of snap.aircraft) if (a.source === 'mlat' && typeof a.lat === 'number' && !a.predicted) mlatKey |= a.mlatGrade ? 1 : 2;
  if (hl !== S.hasHighlights || predNow !== S.legendPred || mlatKey !== S.legendMlat) {
    S.hasHighlights = hl; S.legendPred = predNow; S.legendMlat = mlatKey; S.mlatUngraded = (mlatKey & 2) !== 0;
    renderLegend();
  }
  syncViewport();
  if (S.styleReady) {
    if (S.staticDirty || Date.now() - S.lastStatic > STATIC_TRAIL_REBUILD_MS) { S.staticDirty = false; rebuildStaticTrails(); }
    if (!S.eff.smooth || !S.ticker || document.visibilityState !== 'visible') pushAircraft(S.ctx.live.serverNow());
    S.netv.update(S.prepared);
    pushLiveTrails();
    refreshServerPaths();
    pushPaths();
    ensureAnim();
    if (S.pendingFit && S.prepared.size) { S.pendingFit = false; fitTraffic(false); }
  }
  if (S.pendingSelect && S.ctx.live.aircraft.has(S.pendingSelect)) {
    const icao = S.pendingSelect;
    S.pendingSelect = null;
    if (S.selected === icao && !S.viewFromUrl) centerOn(icao);
  }
}

/**
 * Viewport stream (aggregator mode, DESIGN §20.2): with many aircraft the map asks the live store for only the
 * aircraft inside the visible area (padded) and sends the new box after each pan / zoom. 'auto' switches on
 * above VIEWPORT_ON aircraft in the whole picture (`counts` stay global) and off below VIEWPORT_OFF.
 * moved: the map moved (send the new box); otherwise only the on / off decision is re-evaluated.
 */
function syncViewport(moved = false) {
  if (!S || !S.map || !S.styleReady) return;
  const total = S.snapshot?.counts?.aircraft;
  const on = net.aggregator && wantViewport(S.prefs.get().viewport || 'auto', total, S.vpOn);
  if (!on) {
    if (S.vpOn) { S.vpOn = false; S.ctx.live.setViewport(null); }
    return;
  }
  if (S.vpOn && !moved) return;
  const b = S.map.getBounds();
  S.vpOn = true;
  S.ctx.live.setViewport(viewportBox(b.getWest(), b.getSouth(), b.getEast(), b.getNorth()) || [-90, -180, 90, 180]);
}

/** Network mode: re-apply the view (overlays, layers panel, station markers, filters) after a change. */
function applyNet(reason) {
  const before = S.netView;
  S.netView = net.view;
  S.eff = effective(S);
  S.netv.renderBar();
  if (S.panel) S.panel.close();
  if (S.styleReady) {
    applyVisibility();
    S.netv.applyVisibility();
    pushOverlays();
  }
  if (reason !== 'filter') S.netv.refresh();
  renderLegend();
  if (S.snapshot) prepare(S.snapshot);
  S.list.reapply();
  S.staticDirty = true;
  refreshNow();
  if (reason !== 'filter') S.extras.reloadAll(S.eff.terrainKind);
  if (net.view === 'station' && (before !== 'station' || reason === 'station')) fitReceiver();
}

// -----------------------------------------------------------------------------------------------
// page module
// -----------------------------------------------------------------------------------------------
export default {
  id: 'map',
  title: 'Live Map',
  fullBleed: true,

  async mount(container, ctx) {
    await Promise.all([
      ui.loadCss(new URL('../../vendor/maplibre/maplibre-gl.css', import.meta.url)),
      ui.loadCss(new URL('../../css/map.css', import.meta.url)),
    ]);

    const prefs = ui.prefs(PREFS_KEY, {
      basemap: null, colorBy: null, labels: 'callsign', trails: true, trailMinutes: null,
      rings: true, beams: true, compass: true, night: true, radar: null, coverage: true, smooth: null,
      list: true, view: null,
      predPaths: null, range: false, terrain: false, terrainKind: 'los', airports: null, heat: false, prox: false, acas: false, winds: false, windsFL: 300, mute: false, filter: {},
      viewport: 'auto',
    });

    const state = {
      ctx, prefs, map: null, root: null, el: {}, eff: null, cache: new Map(),
      styleReady: false, snapshot: null, prepared: new Map(), hidden: new Set(), trails: new Map(), selTrail: null,
      selected: null, hover: null, following: false, userMoving: false, group: new Set(),
      emergencies: 0, predictedCount: 0, animating: false, animRaf: 0, animLast: 0, glowAnimated: false,
      ticker: 0, radarTimer: 0, radarTime: 0, radarFailed: false,
      staticDirty: true, lastStatic: 0, seedSeq: 0, outerKm: 0, pendingFit: false, pendingSelect: null, viewFromUrl: false,
      panel: null, filterPop: null, ctaDismissed: false, list: null, detail: null, reseed: null, nightTimer: 0,
      filter: filters.normalize(prefs.get().filter), filterFn: null, filterBtn: null,
      paths: new Map(), serverPaths: true, pathsShown: false, linksShown: false, proxShown: false,
      trace: null, traceSeq: 0, rxFallback: null, kiosk: false, kioskObs: null, lastPathsPush: 0, helpOpen: false, hasHighlights: false, legendPred: false,
      extras: null, measure: null, alerts: null, netv: null, netView: net.view,
      mlatCount: 0, mlatUngraded: false, legendMlat: 0, vpOn: false,
    };
    S = state;
    // development aid: http://host/?mapdebug=1#/map exposes the page state for the browser console
    if (new URLSearchParams(location.search).has('mapdebug')) window.__airviewMap = state;
    S.netv = createNetView(ctx, {
      onChange: () => { if (S === state) applyNet('filter'); },
      pickStation: (anchor) => import('../app.js').then((m) => m.openStationPicker?.(anchor)),
    });
    S.eff = effective(S);
    S.filterFn = filters.compile(S.filter);
    S.reseed = ui.debounce(() => { if (S === state) { S.staticDirty = true; seedTrails(); } }, 400);

    // ---- DOM ----
    const colorOf = (a) => aircraftColor(S.eff.colorBy, a);
    S.list = createList(ctx, {
      colorOf,
      onSelect: (icao, e) => {
        if (e && (e.shiftKey || e.ctrlKey || e.metaKey)) toggleGroup(icao);
        else select(icao === S.selected && !S.group.size ? null : icao, { center: true, reveal: false });
      },
      onHover: (icao) => setHover(icao, true),
      onClose: () => toggleList(false),
      filter: () => S.filterFn,
      isGrouped: (icao) => S.group.has(icao),
    });
    S.detail = createDetail(ctx, {
      colorOf,
      onClose: () => select(null),
      onFollow: (on) => { S.following = on; S.userMoving = false; if (on && S.selected) centerOn(S.selected); },
      onCenter: () => S.selected && centerOn(S.selected),
      onTrace: (on) => (S === state ? loadTrace(on) : Promise.resolve(null)),
      onRouteInfo: (info) => { if (S === state) showRoute(info); },
    });

    const btn = (name, tip, onClick, extra) => { const b = ui.button({ icon: name, tip, onClick, class: 'mapv-btn', ...extra }); b.dataset.tipPlace = 'left'; return b; };
    const layersBtn = btn('layers', 'Layers & display', () => openLayersPanel(layersBtn), { pressed: false });
    const filterBtn = btn('filter', 'Filter aircraft', () => openFilters(filterBtn), { pressed: false });
    S.filterBtn = filterBtn;
    const fsBtn = btn('maximize', 'Fullscreen', () => toggleFullscreen());
    const listBtn = btn('list', 'Aircraft list', () => toggleList(), { pressed: prefs.get().list });
    const measureBtn = btn('ruler', 'Measure distance / bearing (M)', () => toggleMeasure(), { pressed: false });
    // the bell drives the app-wide alert sound (js/alertsound.js) when the shell has it, else the map's own beep
    const muteBtn = btn('bell', '', () => {
      const snd = alertSound();
      if (snd) { try { snd.setMuted(!snd.isMuted()); } catch (e) { console.warn('[map] setMuted failed', e); } } else prefs.set({ mute: !prefs.get().mute });
      S.eff = effective(S); unlockAudio(); renderMute();
    }, { pressed: false });
    const moreBtn = btn('more', 'Export, replay, kiosk, shortcuts', () => openMore(moreBtn));
    S.el.canvas = h('div.mapv-canvas');
    S.el.legend = h('div.mapv-legend');
    S.el.tipTitle = h('b');
    S.el.tipSub = h('span');
    S.el.tip = h('div.mapv-tip.hidden', S.el.tipTitle, S.el.tipSub);
    S.el.chips = h('div.mapv-chips.hidden');
    S.el.groupCount = h('b');
    S.el.group = h('div.mapv-group.hidden', icon('list'), S.el.groupCount,
      ui.button({ label: 'Fit', icon: 'fit', size: 'sm', variant: 'ghost', onClick: () => fitGroup() }),
      ui.button({ label: 'Clear', icon: 'x', size: 'sm', variant: 'ghost', tip: 'Clear the multi-selection (Esc)', onClick: () => clearGroup() }));
    S.el.cta = h('div.mapv-cta.hidden',
      icon('pin'),
      h('div.grow', h('b', 'Receiver location not set'), h('span', 'Range rings, distances and beam overlays appear once AirView knows where the antenna is.')),
      ui.button({ label: 'Set location', variant: 'primary', size: 'sm', href: '#/settings' }),
      ui.button({ icon: 'x', variant: 'ghost', size: 'sm', tip: 'Dismiss', onClick: () => { S.ctaDismissed = true; renderCta(); } }));
    S.el.kioskExit = ui.button({ icon: 'minimize', label: 'Exit kiosk', size: 'sm', class: 'mapv-kiosk-exit', onClick: () => setKiosk(false) });
    const ctl = h('div.mapv-ctl',
      h('div.mapv-ctl-group', layersBtn, filterBtn),
      h('div.mapv-ctl-group.mapv-zoom', btn('plus', 'Zoom in', () => S.map.zoomIn()), btn('minus', 'Zoom out', () => S.map.zoomOut())),
      h('div.mapv-ctl-group',
        btn('locate', net.aggregator ? 'Centre on the selected station' : 'Centre on receiver', () => {
          if (fitReceiver()) return;
          if (!net.aggregator) ui.toast('Set the receiver location in Settings first.');
          else if (net.loggedIn && net.station?.located) net.setView('station');
          else ui.toast(net.loggedIn ? 'Choose one of your stations with a known position.' : 'Station positions are not public.');
        }),
        btn('fit', 'Fit to traffic', () => fitTraffic()),
        fsBtn),
      h('div.mapv-ctl-group', measureBtn, muteBtn, moreBtn),
      h('div.mapv-ctl-group', listBtn));
    const mapBox = h('div.mapv-map', S.el.canvas, ctl, S.netv.el, S.el.chips, S.el.legend, S.el.tip, S.el.cta, S.el.group, S.el.kioskExit, S.detail.el);
    S.root = h('div.mapv', mapBox, S.list.el);
    S.kioskObs = watchShellKiosk();
    if (shellKiosk()?.kiosk) { S.kiosk = true; S.root.classList.add('kiosk', 'kiosk-shell'); }
    container.appendChild(S.root);
    S.root.addEventListener('pointerdown', () => unlockAudio(), { once: true });

    function renderMute() {
      const muted = isMuted();
      const allowed = S.eff.soundAllowed;
      muteBtn.setPressed(!muted && allowed);
      muteBtn.classList.toggle('is-muted', muted || !allowed);
      ui.tip(muteBtn, !allowed ? 'Alert sounds are switched off in Settings › Alerts' : muted ? 'Alert sound muted (click to unmute)' : 'Alert sound on (click to mute)', 'left');
    }
    renderMute();
    // switch the bell over to the app-wide mute state once the module is there
    loadAlertSound().then((m) => {
      if (S !== state) return;
      renderMute();
      // follow mute changes made elsewhere (Settings, another tab of the shell)
      if (m && typeof m.onMuteChange === 'function') {
        const off = m.onMuteChange(() => { if (S === state) renderMute(); });
        if (typeof off === 'function') ctx.onCleanup(off);
      }
    });

    const phoneQuery = window.matchMedia('(max-width: 900px)');
    const isPhone = () => phoneQuery.matches;
    function toggleList(on, persist = true) {
      const open = on === undefined ? !S.root.classList.contains('list-open') : on;
      S.root.classList.toggle('list-open', open);
      listBtn.setPressed(open);
      if (persist && !isPhone()) prefs.set({ list: open });
    }
    // Side by side on wide screens (remembered); a bottom sheet that starts closed on narrow ones.
    const syncListToViewport = () => { if (S === state) toggleList(isPhone() ? false : prefs.get().list, false); };
    syncListToViewport();
    phoneQuery.addEventListener('change', syncListToViewport);
    ctx.onCleanup(() => phoneQuery.removeEventListener('change', syncListToViewport));
    S.root.classList.toggle('light-map', !S.eff.basemap.dark);

    function toggleFullscreen() {
      if (document.fullscreenElement) document.exitFullscreen();
      else if (S.root.requestFullscreen) S.root.requestFullscreen().catch(() => ui.toast.warn('Fullscreen is not available here.'));
    }
    const onFs = () => { const on = document.fullscreenElement === S.root; ui.mount(fsBtn, icon(on ? 'minimize' : 'maximize')); ui.tip(fsBtn, on ? 'Exit fullscreen' : 'Fullscreen'); };
    document.addEventListener('fullscreenchange', onFs);
    ctx.onCleanup(() => document.removeEventListener('fullscreenchange', onFs));

    // ---- map ----
    const rx = rxCfg()?.receiver;
    const rxSig = rx?.located ? `${rx.lat.toFixed(3)},${rx.lon.toFixed(3)}` : 'none';
    const saved = prefs.get().view;
    const useSaved = saved && saved.rx === rxSig && Array.isArray(saved.c) && typeof saved.z === 'number';
    const map = new maplibregl.Map({
      container: S.el.canvas,
      style: styleFor(S.eff.basemap),
      center: useSaved ? saved.c : rx?.located ? [rx.lon, rx.lat] : [8, 30],
      zoom: useSaved ? saved.z : rx?.located ? 6 : 1.6,
      minZoom: 1.2,
      maxZoom: 17,
      maxPitch: 0,
      dragRotate: false,
      pitchWithRotate: false,
      touchPitch: false,
      fadeDuration: 0,
      attributionControl: false,
      renderWorldCopies: true,
      transformRequest: makeTransformRequest(() => !!ctx.live.status?.tileCache),
    });
    S.map = map;
    map.touchZoomRotate.disableRotation();
    map.keyboard.disableRotation();
    // Attribution stays visible (required by OpenFreeMap / OSM / RainViewer); MapLibre only folds it on very narrow maps.
    map.addControl(new maplibregl.AttributionControl({}), 'bottom-right');
    S.scale = new maplibregl.ScaleControl({ maxWidth: 110, unit: units.system === 'metric' ? 'metric' : units.system === 'imperial' ? 'imperial' : 'nautical' });
    map.addControl(S.scale, 'bottom-right');

    // ---- overlay loaders, measuring tool, alerts ----
    S.extras = createExtras(ctx, {
      setData: (id, data) => { if (S === state) setData(id, data); },
      map: () => (S === state ? map : null),
      on: (key) => S === state && !!S.eff[key],
      windsFL: () => S.eff.windsFL,
      onNote: (key) => {
        if (S !== state) return;
        if (key === 'prox-data') { if (S.styleReady) pushProximity(); return; }
        renderLegend();
      },
    });
    S.measure = createMeasure(ctx, {
      host: mapBox,
      setData: (id, data) => { if (S === state) setData(id, data); },
      rxPos,
      onClose: () => toggleMeasure(false),
      setCursor: (c) => { map.getCanvas().style.cursor = c; },
    });
    function toggleMeasure(on) {
      const open = on === undefined ? !S.measure.active : on;
      if (open === S.measure.active) return;
      if (open) { S.measure.open(); setHover(null, false); } else S.measure.close();
      measureBtn.setPressed(open);
      S.root.classList.toggle('measuring', open);
    }
    S.toggleMeasure = toggleMeasure;
    S.alerts = createAlerts(ctx, {
      host: mapBox,
      onSelect: (icao) => { if (S === state) { if (S.kiosk) setKiosk(false); select(icao, { center: true, reveal: true }); } },
      soundAllowed: () => S === state && S.eff.soundAllowed,
      muted: () => S === state && isMuted(),
    });

    let firstStyle = true;
    map.on('style.load', () => {
      if (S !== state) return;
      S.styleReady = true;
      S.glowAnimated = false;
      installLayers();
      pushOverlays();
      S.staticDirty = true;
      if (S.snapshot) prepare(S.snapshot);
      refreshNow();
      pushPaths();
      refreshRadar();
      if (firstStyle) {
        firstStyle = false;
        if (!S.viewFromUrl && !useSaved) { if (!fitReceiver(false)) S.pendingFit = !fitTrafficQuiet(); }
        S.extras.reloadAll(S.eff.terrainKind);
      }
    });
    const fitTrafficQuiet = () => (S.prepared.size ? fitTraffic(false) : false);

    let errShown = 0;
    map.on('error', (e) => {
      // Tile/glyph fetch failures are expected on an isolated LAN; keep the console readable.
      if (errShown++ < 3) console.warn('[map]', e?.error?.message || e);
    });
    map.on('click', (e) => {
      if (S.measure.active) { S.measure.click(e.lngLat); return; }
      const icao = pickAt(e.point);
      const oe = e.originalEvent;
      if (icao && oe && (oe.shiftKey || oe.ctrlKey || oe.metaKey)) { toggleGroup(icao); return; }
      select(icao, { reveal: true });
    });
    map.on('mousemove', ui.rafThrottle((e) => {
      if (S !== state || S.userMoving) return;
      if (S.measure.active) { S.measure.move(e.lngLat); return; }
      setHover(pickAt(e.point), false);
    }));
    map.on('mouseout', () => { if (S === state) setHover(null, false); });
    map.on('dragstart', () => { S.userMoving = true; S.el.tip.classList.add('hidden'); if (S.following) { S.following = false; S.detail.setFollowing(false); } });
    map.on('dragend', () => { S.userMoving = false; });
    const airportsMove = ui.debounce(() => { if (S === state && S.eff.airports) S.extras.airports(); }, 400);
    map.on('moveend', ui.debounce(() => {
      if (S !== state) return;
      const c = map.getCenter();
      prefs.set({ view: { c: [+c.lng.toFixed(4), +c.lat.toFixed(4)], z: +map.getZoom().toFixed(2), rx: rxSig } });
      writeUrl();
    }, 600));
    map.on('moveend', airportsMove);
    map.on('moveend', ui.debounce(() => { if (S === state) syncViewport(true); }, 500));

    // ---- wiring ----
    renderLegend();
    renderCta();
    renderChips();
    filterBtn.classList.toggle('has-dot', filters.isActive(S.filter));
    restartTicker();
    ctx.subscribe('aircraft', (snap) => { if (S === state) onSnapshot(snap); });
    ctx.subscribe('status', () => { if (S === state) renderCta(); });
    // network mode: the View selector and the station markers follow net (view / stations)
    S.netv.renderBar();
    S.netv.refresh();
    ctx.onCleanup(net.on((reason) => { if (S === state && (reason === 'view' || reason === 'stations')) applyNet(reason); }));
    ctx.onCleanup(units.onChange(() => {
      if (S !== state) return;
      pushOverlays();
      renderLegend();
      renderChips();
      if (S.snapshot) prepare(S.snapshot);
      refreshNow();
      S.list.refresh();
      S.detail.refresh();
      S.extras.relabel();
      S.measure.refresh();
      if (S.scale) S.scale.setUnit(units.system === 'metric' ? 'metric' : units.system === 'imperial' ? 'imperial' : 'nautical');
    }));
    ctx.onCleanup(ctx.config.subscribe(() => {
      if (S !== state) return;
      const before = S.eff;
      S.eff = effective(S);
      pushOverlays();
      if (before.basemap.id !== S.eff.basemap.id) switchBasemap();
      if (before.colorBy !== S.eff.colorBy) { renderLegend(); recolour(); }
      if (before.trailMinutes !== S.eff.trailMinutes) S.reseed();
      if (before.radar !== S.eff.radar) { applyVisibility(); refreshRadar(); }
      if (before.predPaths !== S.eff.predPaths || before.airports !== S.eff.airports) { applyVisibility(); pushPaths(); S.extras.airports(); }
      renderMute();
    }));
    ctx.onCleanup(ctx.shared.on('map.coverage', () => { if (S === state && S.styleReady) setData('coverage', coverageCollection(ctx.shared.get('map.coverage'))); }));
    S.nightTimer = setInterval(pushNight, 60000);

    // ---- keyboard ----
    const cyclePaths = () => {
      const order = ['off', 'selected', 'all'];
      const next = order[(order.indexOf(S.eff.predPaths) + 1) % order.length];
      prefs.set({ predPaths: next });
      S.eff = effective(S);
      applyVisibility();
      renderLegend();
      pushPaths();
      ui.toast(`Predicted paths: ${next === 'off' ? 'off' : next === 'selected' ? 'selected aircraft' : 'all aircraft'}`, { timeout: 1800 });
    };
    const onKey = (e) => {
      if (S !== state || e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
      const t = e.target;
      if (t instanceof Element && t.closest('input,textarea,select,[contenteditable],.modal,.popover,.drawer')) return;
      const k = e.key;
      if (k === 'Escape') {
        if (S.measure.active) toggleMeasure(false);
        else if (clearGroup()) { /* cleared */ }
        else if (S.selected) select(null);
        else if (S.kiosk) setKiosk(false);
        else return;
        e.preventDefault();
        return;
      }
      switch (k.length === 1 ? k.toLowerCase() : k) {
        case 'l': {
          const order = ['none', 'callsign', 'full'];
          prefs.set({ labels: order[(order.indexOf(S.eff.labels) + 1) % order.length] });
          S.eff = effective(S);
          applyLabels();
          break;
        }
        case 't': prefs.set({ trails: !S.eff.trails }); S.eff = effective(S); applyVisibility(); S.staticDirty = true; refreshNow(); break;
        case 'p': cyclePaths(); break;
        case 'f':
          if (!S.selected) return;
          S.following = !S.following; S.userMoving = false; S.detail.setFollowing(S.following);
          if (S.following) centerOn(S.selected);
          break;
        case 'r': if (!fitReceiver()) fitTraffic(); break;
        case 'm': toggleMeasure(); break;
        case 'k': setKiosk(!S.kiosk); break;
        case '/':
          if (S.kiosk) setKiosk(false);
          toggleList(true);
          setTimeout(() => S.list.focusSearch(), 50);
          break;
        case '?': openHelp(); break;
        default: return;
      }
      e.preventDefault();
    };
    document.addEventListener('keydown', onKey);
    ctx.onCleanup(() => document.removeEventListener('keydown', onKey));
    const onVis = () => { if (S === state && document.visibilityState === 'visible') { refreshNow(); ensureAnim(); } };
    document.addEventListener('visibilitychange', onVis);
    ctx.onCleanup(() => document.removeEventListener('visibilitychange', onVis));

    seedTrails();
    S.viewFromUrl = applyQuery(ctx.route.query || {}, { initial: true });
    if (ctx.route.args[0]) {
      S.pendingSelect = String(ctx.route.args[0]).toUpperCase();
      select(ctx.route.args[0], { fromRoute: true });
    }
  },

  /** Network mode: the session or the selected station changed (the shell restarted the live stream). */
  onNet(reason) { if (S) applyNet(reason); },

  /** Called by the router when only the part after #/map changes (deep links, back/forward). */
  onRoute(route) {
    if (!S) return;
    const viewSet = applyQuery(route.query || {});
    const icao = route.args[0] ? String(route.args[0]).toUpperCase() : null;
    if (icao !== S.selected) {
      S.pendingSelect = icao;
      select(icao, { fromRoute: true, center: !viewSet });
    }
  },

  unmount() {
    const s = S;
    if (!s) return;
    S = null;
    if (window.__airviewMap === s) delete window.__airviewMap;
    if (s.vpOn) s.ctx.live.setViewport(null); // the other pages need the full picture
    clearInterval(s.ticker);
    clearInterval(s.nightTimer);
    clearTimeout(s.radarTimer);
    cancelAnimationFrame(s.animRaf);
    if (s.reseed) s.reseed.cancel();
    if (s.panel) s.panel.close();
    if (s.filterPop) s.filterPop.close();
    if (document.fullscreenElement === s.root) document.exitFullscreen().catch(() => {});
    // shell kiosk deliberately stays on when navigating away (the shell's own rule); only stop listening
    if (s.kioskObs) s.kioskObs();
    s.extras.stop();
    s.alerts.stop();
    s.measure.destroy();
    s.detail.destroy();
    s.list.destroy();
    try { s.map.remove(); } catch (e) { console.warn('[map] remove failed', e); }
  },
};
