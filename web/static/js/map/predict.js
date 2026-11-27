// map/predict.js - drawing helpers for predicted (coasting) aircraft and predicted paths.
//
//  - Coasting aircraft (aircraft.predicted) are drawn from the same `aircraft` source as everything
//    else: per-feature properties `pred` (1), `gi` (glow intensity 1 -> 0 as coastS approaches
//    config.prediction.coastSec), `po` (icon opacity) and `r0` (1-sigma radius in screen pixels at
//    zoom 0), so the glow and the uncertainty circle cost no extra source.
//  - Predicted paths are short runs of dashed line segments whose opacity falls with look-ahead
//    time, plus tick marks (+30 s, +60 s ...). The selected aircraft use the server's filter path
//    (/api/prediction/path/{icao}); "all aircraft" mode dead-reckons client-side from gs / track /
//    trackRate so it costs nothing on the Pi.

import { geo } from '../geo.js';

/** Metres per screen pixel at zoom 0 on the equator (512 px tiles, as MapLibre uses). */
const M_PER_PX_Z0 = 40075016.686 / 512;
const D2R = Math.PI / 180;

/** Pixel radius at zoom 0 of a circle of `m` metres at latitude `lat` (feed into SIGMA_RADIUS). */
export function radiusZ0(m, lat) {
  const c = Math.max(0.05, Math.cos(lat * D2R));
  return m / (M_PER_PX_Z0 * c);
}

/** circle-radius expression that turns the `r0` feature property into true ground metres at every zoom. */
export const SIGMA_RADIUS = ['interpolate', ['exponential', 2], ['zoom'], 0, ['get', 'r0'], 22, ['*', ['get', 'r0'], 4194304]];

const n = (v) => typeof v === 'number' && Number.isFinite(v);

/**
 * Coasting properties for a live aircraft.
 * @returns {{gi: number, po: number, r0: number}|null} null when not predicted
 */
export function coastProps(a, coastSec) {
  if (!a.predicted) return null;
  const lim = Math.max(5, coastSec || 60);
  const f = Math.max(0, Math.min(1, (a.coastS || 0) / lim));
  return {
    gi: +(1 - 0.85 * f).toFixed(3),
    po: +(0.95 - 0.5 * f).toFixed(3),
    r0: n(a.predSigmaM) && a.predSigmaM > 0 ? radiusZ0(a.predSigmaM, a.lat) : 0,
  };
}

/** Keep a [lon, lat] point in the same world copy as `refLon`. */
export function sameWorld(p, refLon) {
  if (p[0] - refLon > 180) p[0] -= 360; else if (p[0] - refLon < -180) p[0] += 360;
  return p;
}

/**
 * Client-side dead-reckoned path from a position. The turn rate decays (time constant 40 s) so a
 * turning aircraft rolls out instead of flying circles.
 * @returns {Array<{dt: number, lon: number, lat: number}>}
 */
export function deadReckonPath(lon, lat, a, horizonS = 60, stepS = 10) {
  if (!n(a.gs) || !n(a.track) || a.gs < 30 || a.onGround) return [];
  const kmPerS = (a.gs * 1.852) / 3600;
  let track = a.track;
  let rate = n(a.trackRate) ? Math.max(-3, Math.min(3, a.trackRate)) : 0;
  let cur = [lon, lat];
  const out = [{ dt: 0, lon, lat }];
  const sub = 2;
  for (let t = sub; t <= horizonS + 1e-6; t += sub) {
    track += rate * sub;
    rate *= Math.exp(-sub / 40);
    cur = sameWorld(geo.destination(cur[1], cur[0], track, kmPerS * sub), cur[0]);
    if (t % stepS < 1e-6 || t >= horizonS) out.push({ dt: t, lon: cur[0], lat: cur[1] });
  }
  return out;
}

/**
 * Tick circles show the server's 1-sigma `sh`, which assumes the aircraft
 * may have turned (standard-rate hypothesis) and grows to tens of km after a
 * couple of minutes. Beyond this radius the circle would only paint a large
 * area without telling anything: the tick is drawn without it.
 */
export const TICK_SIGMA_MAX_M = 5000;

/**
 * Convert a path into line segments with fading opacity and tick points.
 * @param {Array<{dt, lon, lat, sh?}>} pts
 * @param {{color: string, icao: string, ticks?: boolean, tickEvery?: number, startLon?: number, major?: boolean}} o
 * @param {Array} segs output: line features      @param {Array} ticks output: point features
 */
export function pathFeatures(pts, o, segs, ticks) {
  if (!pts || pts.length < 2) return;
  const last = pts[pts.length - 1].dt || 1;
  const every = o.tickEvery || 30;
  let prev = [pts[0].lon, pts[0].lat];
  if (n(o.startLon)) sameWorld(prev, o.startLon);
  for (let i = 1; i < pts.length; i++) {
    const p = sameWorld([pts[i].lon, pts[i].lat], prev[0]);
    const f = pts[i].dt / last;
    segs.push({ type: 'Feature', properties: { c: o.color, o: +(0.95 - 0.8 * f).toFixed(3), w: o.major ? 1 : 0 }, geometry: { type: 'LineString', coordinates: [prev, p] } });
    if (o.ticks && pts[i].dt > 0 && pts[i].dt % every === 0) {
      const sh = pts[i].sh;
      const r0 = n(sh) && sh <= TICK_SIGMA_MAX_M ? radiusZ0(sh, pts[i].lat) : 0;
      ticks.push({ type: 'Feature', properties: { c: o.color, t: `+${pts[i].dt >= 120 && pts[i].dt % 60 === 0 ? pts[i].dt / 60 + ' min' : pts[i].dt + ' s'}`, r0, o: +(0.95 - 0.7 * f).toFixed(3) }, geometry: { type: 'Point', coordinates: p } });
    }
    prev = p;
  }
}

/** Dashed connector from the last real position to the shown (predicted) position. */
export function linkFeature(a, cur) {
  if (!n(a.lastRealLat) || !n(a.lastRealLon)) return null;
  const from = sameWorld([a.lastRealLon, a.lastRealLat], cur[0]);
  return { type: 'Feature', properties: {}, geometry: { type: 'LineString', coordinates: [from, cur] } };
}

/** The last real position as a small hollow marker. */
export function lastRealFeature(a, cur) {
  if (!n(a.lastRealLat) || !n(a.lastRealLon)) return null;
  return { type: 'Feature', properties: { last: 1 }, geometry: { type: 'Point', coordinates: sameWorld([a.lastRealLon, a.lastRealLat], cur[0]) } };
}
