// map/route-split.js - splits the selected aircraft's great-circle route path at its current position
// (docs/ROUTES-DESIGN.md §6.2): the flown part is drawn faint and solid, the remaining part dashed, both
// coloured by the route state. Pure functions (no DOM), tested in web/dev/test/route-split.test.mjs.

/** Line colour per route state: normal magenta, uncertain grey, doubtful / reversed amber. */
export const ROUTE_STATE_COLOR = { fits: '#e879f9', uncertain: '#94a3b8', doubtful: '#fbbf24', reversed: '#fbbf24', unchecked: '#94a3b8' };

/** Maps the live route state (or the lazy check of /api/navdata/route) to a colour. */
export function routeColor(state, check) {
  if (state && ROUTE_STATE_COLOR[state]) return ROUTE_STATE_COLOR[state];
  if (check && check.doubtful) return ROUTE_STATE_COLOR.doubtful;
  return ROUTE_STATE_COLOR.fits;
}

/**
 * Splits path ([[lon, lat], ...], longitudes possibly continuous across the antimeridian) at the point nearest
 * to pos ([lon, lat]). Returns { flown, remaining } (each null when it is a single point), or null when the
 * aircraft is further than maxDeg (equirectangular degrees) from the path: then the whole path is "remaining".
 */
export function splitPath(path, pos, maxDeg = 3) {
  if (!Array.isArray(path) || path.length < 2 || !pos) return null;
  let best = null;
  for (let i = 0; i < path.length - 1; i++) {
    const [ax, ay] = path[i];
    const [bx0, by] = path[i + 1];
    // keep the aircraft and the segment in the same world copy as the segment's start
    let px = pos[0];
    while (px - ax > 180) px -= 360;
    while (px - ax < -180) px += 360;
    const k = Math.cos(((ay + by) / 2) * Math.PI / 180) || 1e-6;
    const bx = bx0;
    const dx = (bx - ax) * k;
    const dy = by - ay;
    const len2 = dx * dx + dy * dy;
    let t = len2 > 0 ? (((px - ax) * k) * dx + (pos[1] - ay) * dy) / len2 : 0;
    t = Math.max(0, Math.min(1, t));
    const qx = ax + (bx - ax) * t;
    const qy = ay + dy * t;
    const d = Math.hypot((px - qx) * k, pos[1] - qy);
    if (!best || d < best.d) best = { d, i, q: [qx, qy] };
  }
  if (!best || best.d > maxDeg) return null;
  const flown = path.slice(0, best.i + 1).concat([best.q]);
  const remaining = [best.q].concat(path.slice(best.i + 1));
  const line = (c) => (c.length >= 2 && !(c.length === 2 && c[0][0] === c[1][0] && c[0][1] === c[1][1]) ? c : null);
  return { flown: line(flown), remaining: line(remaining) };
}

/**
 * GeoJSON line features for the route: { kind: 'line', part: 'flown' | 'remaining', color }.
 */
export function routeLineFeatures(path, pos, color) {
  const line = (coords, part) => ({ type: 'Feature', properties: { kind: 'line', part, color }, geometry: { type: 'LineString', coordinates: coords } });
  const s = pos ? splitPath(path, pos) : null;
  if (!s) return [line(path, 'remaining')];
  return [s.flown && line(s.flown, 'flown'), s.remaining && line(s.remaining, 'remaining')].filter(Boolean);
}
