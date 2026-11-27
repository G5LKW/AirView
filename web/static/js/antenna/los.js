// antenna/los.js - client-side line-of-sight maths for the Antenna Lab "Terrain & line of sight" tab.
//
// The server's /api/terrain/los profile is computed with the configured effective-earth factor
// (config.terrain.kFactor). To let the user explore refraction without touching the settings, the profile is
// re-evaluated here for any k, using the same textbook model as internal/terrain:
//
//   earth bulge        b(d) = d1 d2 / (2 k R)              (added to the terrain, the ray stays straight)
//   first Fresnel zone r1(d) = sqrt(lambda d1 d2 / (d1 + d2)), lambda = c / 1090 MHz
//   knife edge         v = h sqrt(2 (d1 + d2) / (lambda d1 d2)),  J(v) = 6.9 + 20 log10(sqrt((v-0.1)^2+1) + v - 0.1)
//                      for v > -0.78 (ITU-R P.526), else 0
//   several edges      Deygout: the main edge over the whole path, then one level of sub-edges on either side.
//
// Everything works in metres; distances in the input profile are km (API base unit).

export const R_EARTH_M = 6371008.8;
export const LAMBDA_M = 299792458 / 1090e6;
const V_MIN = -0.78;
const NEAR_SKIP_M = 300; // obstacles closer than this to the antenna are its own mounting (same as the server's minObstacleKm)

const isNum = (v) => typeof v === 'number' && Number.isFinite(v);

/** Earth bulge in metres at d1 / d2 metres from the path ends for factor k. */
export function bulgeM(d1, d2, k) {
  return (d1 * d2) / (2 * k * R_EARTH_M);
}

/** First Fresnel zone radius in metres. */
export function fresnelM(d1, d2) {
  const d = d1 + d2;
  return d > 0 && d1 > 0 && d2 > 0 ? Math.sqrt((LAMBDA_M * d1 * d2) / d) : 0;
}

/** ITU-R P.526 single knife-edge diffraction loss in dB for the parameter v. */
export function knifeEdgeDb(v) {
  if (!isNum(v) || v <= V_MIN) return 0;
  return 6.9 + 20 * Math.log10(Math.sqrt((v - 0.1) ** 2 + 1) + v - 0.1);
}

/** Smooth-earth radio horizon distance in km for a height in metres above the (sea-level) surface. */
export function horizonKm(hM, k) {
  return hM > 0 ? Math.sqrt(2 * k * R_EARTH_M * hM) / 1000 : 0;
}

/** Effective earth radius in km. */
export function effectiveRadiusKm(k) {
  return (k * R_EARTH_M) / 1000;
}

/**
 * Find the dominant edge between indices a and b (exclusive) of the effective profile.
 * @returns {{ i: number, v: number, h: number } | null}
 */
function mainEdge(xs, eff, a, b, ya, yb, guard = 0) {
  const da = xs[a];
  const span = xs[b] - da;
  if (span <= 0 || b - a < 2) return null;
  let best = null;
  for (let i = a + 1 + guard; i < b - guard; i++) {
    if (xs[i] < NEAR_SKIP_M) continue;
    const d1 = xs[i] - da;
    const d2 = xs[b] - xs[i];
    if (d1 <= 0 || d2 <= 0) continue;
    const ray = ya + ((yb - ya) * d1) / span;
    const h = eff[i] - ray;
    const v = h * Math.sqrt((2 * (d1 + d2)) / (LAMBDA_M * d1 * d2));
    if (!best || v > best.v) best = { i, v, h };
  }
  return best;
}

/**
 * Re-evaluate a /api/terrain/los reply for another k.
 * @param {object} los server reply with profile[] ({distKm, terrainM, ...}), rxAntennaM, tgtAltM, groundKm
 * @param {number} k effective earth radius factor
 * @returns {null | { k, points: Array<{x, terrain, bulge, eff, ray, fres}>, clear, minClearanceM, minClearanceDistKm,
 *   fresnelRatio, fresnel60Clear, obstruction: null | {distKm, lat, lon, terrainM, aboveRayM}, edges: Array<{distKm, lat, lon,
 *   terrainM, heightM, v, lossDb, level}>, diffractionDb }}  x in km, heights in metres AMSL
 */
export function evaluate(los, k) {
  const prof = los && Array.isArray(los.profile) ? los.profile : null;
  if (!prof || prof.length < 2 || !isNum(los.rxAntennaM) || !isNum(los.tgtAltM) || !(k > 0)) return null;
  const n = prof.length;
  const D = prof[n - 1].distKm * 1000;
  if (!(D > 0)) return null;
  const y0 = los.rxAntennaM;
  const y1 = los.tgtAltM;
  const xs = new Float64Array(n);
  const eff = new Float64Array(n);
  const points = new Array(n);
  let minC = Infinity; let minCi = -1; let minRatio = Infinity; let minRatioI = -1;
  for (let i = 0; i < n; i++) {
    const p = prof[i];
    const d = p.distKm * 1000;
    const t = isNum(p.terrainM) ? p.terrainM : 0;
    const b = bulgeM(d, D - d, k);
    const ray = y0 + ((y1 - y0) * d) / D;
    const fr = fresnelM(d, D - d);
    xs[i] = d;
    eff[i] = t + b;
    points[i] = { x: p.distKm, terrain: t, bulge: b, eff: t + b, ray, fres: fr, lat: p.lat, lon: p.lon };
    if (i === 0 || i === n - 1 || d < NEAR_SKIP_M) continue;
    const c = ray - (t + b);
    if (c < minC) { minC = c; minCi = i; }
    if (fr > 0) { const r = c / fr; if (r < minRatio) { minRatio = r; minRatioI = i; } }
  }
  const clear = !(minC < 0);
  let obstruction = null;
  if (!clear && minCi >= 0) {
    // the edge that cuts the ray deepest is the one the server reports as the obstruction
    const p = points[minCi];
    obstruction = { distKm: p.x, lat: p.lat, lon: p.lon, terrainM: p.terrain, aboveRayM: -minC };
  }

  // Deygout: main edge + one level of sub-edges
  const edges = [];
  const top = mainEdge(xs, eff, 0, n - 1, y0, y1);
  let loss = 0;
  if (top && top.v > V_MIN) {
    const add = (e, level) => {
      const l = knifeEdgeDb(e.v);
      const p = points[e.i];
      edges.push({ distKm: p.x, lat: p.lat, lon: p.lon, terrainM: p.terrain, heightM: e.h, v: e.v, lossDb: l, level });
      return l;
    };
    loss += add(top, 0);
    // a few samples either side of the main edge belong to the same ridge, not to a separate edge
    const guard = Math.max(2, Math.round(n / 400));
    const left = mainEdge(xs, eff, 0, top.i, y0, eff[top.i], guard);
    if (left && left.v > V_MIN) loss += add(left, 1);
    const right = mainEdge(xs, eff, top.i, n - 1, eff[top.i], y1, guard);
    if (right && right.v > V_MIN) loss += add(right, 1);
  }
  const ratioPt = minRatioI >= 0 ? points[minRatioI] : null;
  return {
    k, points, clear,
    minClearanceM: isNum(minC) ? minC : null,
    minClearanceDistKm: minCi >= 0 ? points[minCi].x : null,
    fresnelRatio: isNum(minRatio) ? minRatio : null,
    fresnelRatioDistKm: ratioPt ? ratioPt.x : null,
    fresnel60Clear: isNum(minRatio) ? minRatio >= 0.6 : true,
    obstruction, edges, diffractionDb: loss,
  };
}

/**
 * Pick about `max` points of a profile for drawing, always keeping the extremes that matter (the start, the
 * end, the obstruction and every local terrain maximum inside each bucket).
 */
export function decimate(points, max = 700) {
  const n = points.length;
  if (n <= max) return points;
  const step = n / max;
  const out = [points[0]];
  for (let b = 1; b < max - 1; b++) {
    const a = Math.floor(b * step);
    const e = Math.min(n - 1, Math.floor((b + 1) * step));
    let best = a;
    for (let i = a; i < e; i++) if (points[i].eff - points[i].ray > points[best].eff - points[best].ray) best = i;
    out.push(points[best]);
  }
  out.push(points[n - 1]);
  return out;
}
