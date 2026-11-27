// net/location.js - pure helpers of the station Location section (DESIGN §10, §11; WP-W3-wire): the
// declared-versus-measured comparison, the local plot of declared / surveyed / approximate positions with
// the survey's 95 % ellipse, and the wording of survey states. No DOM: covered by web/dev/test/location.test.mjs.

const R_EARTH = 6371008.8; // mean Earth radius (m)
const rad = (d) => (d * Math.PI) / 180;
const num = (x) => typeof x === 'number' && Number.isFinite(x);

/** Has p a usable latitude / longitude? */
export const hasPos = (p) => !!p && num(p.lat) && num(p.lon) && Math.abs(p.lat) <= 90 && Math.abs(p.lon) <= 180;

/** East / north offset (m) of b from a (local tangent plane; exact enough below ~100 km). */
export function enuOffset(a, b) {
  const lat0 = rad(a.lat);
  let dLon = b.lon - a.lon;
  if (dLon > 180) dLon -= 360;
  if (dLon < -180) dLon += 360;
  return { e: rad(dLon) * R_EARTH * Math.cos(lat0), n: rad(b.lat - a.lat) * R_EARTH };
}

/** Great-circle distance (m). */
export function distanceM(a, b) {
  const p1 = rad(a.lat), p2 = rad(b.lat);
  const dp = p2 - p1, dl = rad(b.lon - a.lon);
  const h = Math.sin(dp / 2) ** 2 + Math.cos(p1) * Math.cos(p2) * Math.sin(dl / 2) ** 2;
  return 2 * R_EARTH * Math.asin(Math.min(1, Math.sqrt(h)));
}

/** Initial bearing from a to b (degrees, 0 = north, clockwise). */
export function bearingDeg(a, b) {
  const p1 = rad(a.lat), p2 = rad(b.lat), dl = rad(b.lon - a.lon);
  const y = Math.sin(dl) * Math.cos(p2);
  const x = Math.cos(p1) * Math.sin(p2) - Math.sin(p1) * Math.cos(p2) * Math.cos(dl);
  return ((Math.atan2(y, x) * 180) / Math.PI + 360) % 360;
}

const DIRS = ['N', 'NE', 'E', 'SE', 'S', 'SW', 'W', 'NW'];
/** Compass point of a bearing (8 directions). */
export const compass = (deg) => DIRS[Math.round((((deg % 360) + 360) % 360) / 45) % 8];

/** "12 m", "850 m", "1.3 km", "24 km". */
export function fmtDistance(m) {
  if (!num(m)) return '—';
  if (m >= 10000) return `${Math.round(m / 1000)} km`;
  if (m >= 1000) return `${(m / 1000).toFixed(1)} km`;
  return `${Math.round(m)} m`;
}

/**
 * Declared position versus the network's survey (DESIGN §10.3): distance, direction and the verdict with
 * the survey's own limit max(100 m, 3σ) when the result does not carry one.
 * Returns null without both positions.
 */
export function compareDeclared(declared, survey) {
  if (!hasPos(declared) || !hasPos(survey)) return null;
  const d = distanceM(declared, survey);
  const sigma = num(survey.sigmaHM) ? survey.sigmaHM : num(survey.sigmaM) ? survey.sigmaM : 0;
  const limit = num(survey.declaredLimitM) && survey.declaredLimitM > 0 ? survey.declaredLimitM : Math.max(100, 3 * sigma);
  const brg = bearingDeg(survey, declared);
  return { distM: d, limitM: limit, agrees: d <= limit, bearingDeg: brg, dir: compass(brg) };
}

/**
 * Layout of the comparison plot: every point relative to the centre (the survey, else the first position
 * given), scaled into a square of size px with a margin, north up. Circles (r in m) and the ellipse (a, b
 * in m, azimuth of a in degrees from north) are scaled alike.
 *
 * items: [{ id, pos, r?, ellipse?: { a, b, azDeg } }]
 * Returns { size, scale (px per m), spanM (m across the square), items: [{ id, x, y, r?, ellipse?: { rx, ry, rotDeg } }] }
 * or null when no item has a position.
 */
export function plotLayout(items, size = 240, margin = 18) {
  const ok = (items || []).filter((it) => it && hasPos(it.pos));
  if (!ok.length) return null;
  const centre = ok[0].pos;
  let extent = 0;
  const rel = ok.map((it) => {
    const o = enuOffset(centre, it.pos);
    let reach = Math.hypot(o.e, o.n);
    if (num(it.r) && it.r > 0) reach += it.r;
    if (it.ellipse && num(it.ellipse.a)) reach += Math.max(it.ellipse.a, it.ellipse.b || 0);
    extent = Math.max(extent, reach);
    return { it, o };
  });
  extent = Math.max(extent, 25); // at least ±25 m, so a lone point does not fill the plot
  const half = size / 2 - margin;
  const scale = half / extent;
  return {
    size,
    scale,
    spanM: (2 * (size / 2)) / scale,
    items: rel.map(({ it, o }) => {
      const out = { id: it.id, x: size / 2 + o.e * scale, y: size / 2 - o.n * scale };
      if (num(it.r) && it.r > 0) out.r = it.r * scale;
      if (it.ellipse && num(it.ellipse.a) && it.ellipse.a > 0) {
        // SVG rotation is clockwise from +x (east); the ellipse azimuth is clockwise from north.
        out.ellipse = { rx: it.ellipse.a * scale, ry: (num(it.ellipse.b) ? it.ellipse.b : it.ellipse.a) * scale, rotDeg: (num(it.ellipse.azDeg) ? it.ellipse.azDeg : 0) - 90 };
      }
      return out;
    }),
  };
}

/** A "nice" scale-bar length (1, 2, 5 × 10^n m) of at most maxM metres. */
export function niceLength(maxM) {
  if (!(maxM > 0)) return 0;
  const p = 10 ** Math.floor(Math.log10(maxM));
  for (const f of [5, 2, 1]) if (f * p <= maxM) return f * p;
  return p;
}

const REASONS = {
  unlocated: 'the station has no position', check: 'the regular check of the declared position', altref: 'the altitude reference test (MSL or ellipsoid)',
  bias: 'MLAT residuals pointed at this station', owner: 'you asked', admin: 'the network operator asked',
};
/** Why a survey ran, in words. */
export const reasonText = (r) => REASONS[r] || r || '';

/**
 * One line for a survey status (GET .../survey): running, last outcome or nothing yet.
 * s: { enabled, running, observed, reason, last: { state, valid, stable, suspect, sigmaHM } }
 */
export function surveyStatusText(s) {
  if (!s || !s.enabled) return 'The network survey is not running on this aggregator.';
  if (s.running) return `Measuring: ${s.observed || 0} observations so far (${reasonText(s.reason)}).`;
  const l = s.last;
  if (!l) return 'Not surveyed yet. A station that sends timestamps is surveyed automatically once it shares aircraft with located stations.';
  return lastResultText(l);
}

/** The outcome of one survey result in words. */
export function lastResultText(l) {
  if (!l) return '';
  const sig = num(l.sigmaHM) ? ` (±${fmtDistance(l.sigmaHM)})` : '';
  if (!l.valid) {
    return l.state === 'cancelled' ? 'The last survey was cancelled.' : 'The last survey ended without enough data; it will be retried.';
  }
  if (l.suspect) return `The last survey${sig} failed its consistency checks and is not used.`;
  if (l.stable) return `Measured${sig}.`;
  return `The last survey found no stable position${sig}; it will be repeated.`;
}

/** Badge kind and label of a location status. */
export function locBadge(status) {
  switch (status) {
    case 'gnss': case 'confirmed': return { kind: 'green', label: status === 'gnss' ? 'GNSS' : 'Confirmed' };
    case 'estimated': return { kind: 'cyan', label: 'Estimated by the network' };
    case 'declared': return { kind: 'blue', label: 'Declared' };
    case 'suspect': return { kind: 'amber', label: 'Suspect' };
    case 'estimating': return { kind: 'violet', label: 'Being measured' };
    default: return { kind: 'other', label: 'Not located' };
  }
}

/**
 * The survey estimate the owner can confirm: the newest survey estimate with status "stable" (estimates
 * from GET /api/me/stations/{sid} location.estimates). Null when there is none.
 */
export function confirmableSurvey(estimates) {
  const list = (estimates || []).filter((e) => e && e.method === 'survey' && e.status === 'stable' && hasPos(e));
  list.sort((a, b) => (b.t || 0) - (a.t || 0));
  return list[0] || null;
}

/** Histogram rows of the admin σ bins ([0,10), [10,30), [30,50), [50,200), ≥200 m). */
export const SIGMA_BINS = ['< 10 m', '10-30 m', '30-50 m', '50-200 m', '≥ 200 m'];
