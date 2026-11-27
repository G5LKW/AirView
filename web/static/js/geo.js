// geo.js - small client-side geodesy and sun helpers (map overlays, status bar day/night).
// Coordinates follow GeoJSON order in arrays: [lon, lat]; function arguments are (lat, lon).

const R_KM = 6371.0088;
const D2R = Math.PI / 180;
const R2D = 180 / Math.PI;
const MERC_MAX_LAT = 85.0511;

const normLon = (lon) => ((((lon + 180) % 360) + 360) % 360) - 180;
const norm360 = (d) => ((d % 360) + 360) % 360;

/** Great-circle distance in km. */
function distanceKm(lat1, lon1, lat2, lon2) {
  const p1 = lat1 * D2R;
  const p2 = lat2 * D2R;
  const dp = p2 - p1;
  const dl = (lon2 - lon1) * D2R;
  const a = Math.sin(dp / 2) ** 2 + Math.cos(p1) * Math.cos(p2) * Math.sin(dl / 2) ** 2;
  return 2 * R_KM * Math.asin(Math.min(1, Math.sqrt(a)));
}

/** Initial bearing in degrees true from point 1 to point 2. */
function bearing(lat1, lon1, lat2, lon2) {
  const p1 = lat1 * D2R;
  const p2 = lat2 * D2R;
  const dl = (lon2 - lon1) * D2R;
  const y = Math.sin(dl) * Math.cos(p2);
  const x = Math.cos(p1) * Math.sin(p2) - Math.sin(p1) * Math.cos(p2) * Math.cos(dl);
  return norm360(Math.atan2(y, x) * R2D);
}

/** Point reached from (lat, lon) going `km` along `brgDeg`. Returns [lon, lat] (lon not normalised to the start's world copy). */
function destination(lat, lon, brgDeg, km) {
  return destinationAngular(lat, lon, brgDeg, km / R_KM);
}

function destinationAngular(lat, lon, brgDeg, ang) {
  const p1 = lat * D2R;
  const b = brgDeg * D2R;
  const sinP2 = Math.sin(p1) * Math.cos(ang) + Math.cos(p1) * Math.sin(ang) * Math.cos(b);
  const p2 = Math.asin(Math.max(-1, Math.min(1, sinP2)));
  const l2 = lon * D2R + Math.atan2(Math.sin(b) * Math.sin(ang) * Math.cos(p1), Math.cos(ang) - Math.sin(p1) * sinP2);
  return [normLon(l2 * R2D), p2 * R2D];
}

/** Make a ring's longitudes continuous (no +-360 jumps) so it renders across the antimeridian. */
function unwrap(ring) {
  for (let i = 1; i < ring.length; i++) {
    let d = ring[i][0] - ring[i - 1][0];
    if (d > 180) ring[i][0] -= 360 * Math.round(d / 360);
    else if (d < -180) ring[i][0] += 360 * Math.round(-d / 360);
  }
  return ring;
}

/** Closed geodesic circle ring [[lon,lat],...] of radius km around a point. */
function circle(lat, lon, km, steps = 128) {
  const ring = [];
  for (let i = 0; i <= steps; i++) ring.push(destination(lat, lon, (i * 360) / steps, km));
  return unwrap(ring);
}

/** Open arc [[lon,lat],...] at radius km from bearing a0 to a1 (clockwise). */
function arc(lat, lon, km, a0, a1, stepDeg = 2) {
  const span = norm360(a1 - a0) || 360;
  const n = Math.max(2, Math.ceil(span / stepDeg));
  const pts = [];
  for (let i = 0; i <= n; i++) pts.push(destination(lat, lon, a0 + (span * i) / n, km));
  return unwrap(pts);
}

/** Closed pie-slice ring centred on azimuth `az` with total angular width `width` out to `km`. */
function sector(lat, lon, km, az, width, stepDeg = 2) {
  const w = Math.max(0.5, Math.min(360, width));
  const ring = [[lon, lat], ...arc(lat, lon, km, az - w / 2, az + w / 2, stepDeg), [lon, lat]];
  return unwrap(ring);
}

/**
 * Low-precision solar position (Astronomical Almanac, ~0.01 deg): the sub-solar point.
 * @param {Date|number} [date]
 * @returns {{lat: number, lon: number}} declination and sub-solar longitude in degrees
 */
function sun(date = Date.now()) {
  const ms = date instanceof Date ? date.getTime() : date;
  const n = ms / 86400000 + 2440587.5 - 2451545.0;
  const L = norm360(280.46 + 0.9856474 * n);
  const g = norm360(357.528 + 0.9856003 * n) * D2R;
  const lambda = (L + 1.915 * Math.sin(g) + 0.02 * Math.sin(2 * g)) * D2R;
  const eps = (23.439 - 0.0000004 * n) * D2R;
  const ra = Math.atan2(Math.cos(eps) * Math.sin(lambda), Math.cos(lambda)) * R2D;
  const dec = Math.asin(Math.sin(eps) * Math.sin(lambda)) * R2D;
  const gmst = norm360(280.46061837 + 360.98564736629 * n);
  return { lat: dec, lon: normLon(ra - gmst) };
}

/** Sun elevation above the horizon in degrees (geometric, no refraction). */
function sunElevation(lat, lon, date = Date.now()) {
  const s = sun(date);
  const h = (lon - s.lon) * D2R;
  const sinEl = Math.sin(lat * D2R) * Math.sin(s.lat * D2R) + Math.cos(lat * D2R) * Math.cos(s.lat * D2R) * Math.cos(h);
  return Math.asin(Math.max(-1, Math.min(1, sinEl))) * R2D;
}

/**
 * Classify a sun elevation.
 * @returns {{id: 'day'|'civil'|'nautical'|'astronomical'|'night', label: string, icon: string}}
 */
function dayPhase(elevDeg) {
  if (elevDeg > -0.833) return { id: 'day', label: 'Day', icon: 'sun' };
  if (elevDeg > -6) return { id: 'civil', label: 'Civil twilight', icon: 'twilight' };
  if (elevDeg > -12) return { id: 'nautical', label: 'Nautical twilight', icon: 'twilight' };
  if (elevDeg > -18) return { id: 'astronomical', label: 'Astro twilight', icon: 'moon' };
  return { id: 'night', label: 'Night', icon: 'moon' };
}

/**
 * Polygon covering the part of the Earth where the sun is more than `depressionDeg` below the
 * horizon (0 = night side, 6 / 12 / 18 = end of civil / nautical / astronomical twilight).
 * The region is a spherical cap around the anti-solar point; the ring is unwrapped and, when the
 * cap contains a pole, closed along that pole so it fills correctly in Web Mercator.
 * @returns {{type: 'Polygon', coordinates: number[][][]}}
 */
function nightPolygon(date = Date.now(), depressionDeg = 0) {
  const s = sun(date);
  let cLat = -s.lat;
  if (Math.abs(cLat) < 0.05) cLat = cLat < 0 ? -0.05 : 0.05; // avoid the degenerate through-both-poles case
  const cLon = normLon(s.lon + 180);
  const ang = (90 - Math.max(0, Math.min(89, depressionDeg))) * D2R;
  const ring = [];
  const steps = 180;
  for (let i = 0; i <= steps; i++) {
    const p = destinationAngular(cLat, cLon, (i * 360) / steps, ang);
    p[1] = Math.max(-MERC_MAX_LAT, Math.min(MERC_MAX_LAT, p[1]));
    ring.push(p);
  }
  unwrap(ring);
  const winding = ring[ring.length - 1][0] - ring[0][0];
  if (Math.abs(winding) > 180) {
    // The cap contains a pole: the boundary runs once around the globe; close it along the pole.
    const pole = cLat > 0 ? MERC_MAX_LAT : -MERC_MAX_LAT;
    ring.push([ring[ring.length - 1][0], pole], [ring[0][0], pole], [ring[0][0], ring[0][1]]);
  } else {
    ring[ring.length - 1] = [ring[0][0], ring[0][1]];
  }
  // Keep the ring within one world copy either side so the renderer can wrap it.
  let min = Infinity;
  let max = -Infinity;
  for (const p of ring) { if (p[0] < min) min = p[0]; if (p[0] > max) max = p[0]; }
  const shift = -360 * Math.round((min + max) / 2 / 360);
  if (shift) for (const p of ring) p[0] += shift;
  return { type: 'Polygon', coordinates: [ring] };
}

export const geo = {
  R_KM, D2R, R2D, normLon, norm360,
  distanceKm, bearing, destination, circle, arc, sector, unwrap,
  sun, sunElevation, dayPhase, nightPolygon,
};

export default geo;
