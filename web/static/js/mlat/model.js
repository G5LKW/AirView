// mlat/model.js - pure helpers of the MLAT views (no DOM; unit-tested in web/dev/test/mlat-model.test.mjs):
//   grades          the solver's quality grades (DESIGN §9.4) with their colours and map ring styles
//   normStationMLAT GET /api/(me|admin)/stations/{sid}/mlat -> the owner / admin Timing & MLAT model
//   historyRows     GET /api/admin/mlat/history columns -> chart series [[tMs, v], ...]
//   syncMatrix      GET /api/admin/mlat/sync -> heat-map axes and cells (bounded, ordered by component)
//   crlbRows        GET /api/admin/mlat/crlb summary -> table rows
//   viewport        map bounds -> the `bbox` of a viewport stream (live.js), auto on/off hysteresis

/** Grades of displayable MLAT positions (D is never shown on the map). */
export const GRADES = [
  { id: 'A', label: 'A', max: 50, color: '#34d399', width: 2.2, text: '≤ 50 m, redundant geometry' },
  { id: 'B', label: 'B', max: 150, color: '#fbbf24', width: 1.6, text: '≤ 150 m' },
  { id: 'C', label: 'C', max: 500, color: '#fb923c', width: 1.1, text: '≤ 500 m' },
];
export const GRADE_COLOR = Object.fromEntries(GRADES.map((g) => [g.id, g.color]));
/** Ring colour of an MLAT aircraft without a grade (external MLAT, e.g. an mlat-server bridge). */
export const MLAT_UNGRADED = '#c4b5fd';

/** Grade of a σ_h in metres (as the solver grades; DOF is not known on the client). */
export function gradeOf(sigmaM) {
  if (!Number.isFinite(sigmaM)) return '';
  for (const g of GRADES) if (sigmaM <= g.max) return g.id;
  return sigmaM <= 2000 ? 'D' : '';
}

/** Ring style of a map aircraft: { c, w } (colour, stroke width) or null for non-MLAT aircraft. */
export function mlatRing(a) {
  if (!a || a.source !== 'mlat') return null;
  const g = GRADES.find((x) => x.id === a.mlatGrade);
  return g ? { c: g.color, w: g.width } : { c: MLAT_UNGRADED, w: 1.2 };
}

const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : undefined);

/** Peer distance bands (DESIGN §8.2: owners see a band, never a distance or bearing). */
export const BANDS = { near: '< 50 km', mid: '50–150 km', far: '> 150 km' };

/**
 * Owner / admin station MLAT answer -> page model. Every field is optional; `running` false = no engine.
 * peers: [{ id (tag or SID), band, count, rmsNs, relPpb, sameDomain }] sorted by co-observations.
 */
export function normStationMLAT(r) {
  r = r || {};
  const c = r.clock || null;
  const m = r.mlat || null;
  const s = r.summary || {};
  const peers = (c?.peers || []).map((p) => ({ id: p.tag || p.sid || '?', tag: p.tag || '', sid: p.sid || '', band: p.band || '', count: num(p.count) ?? 0, rmsNs: num(p.rmsNs), relPpb: num(p.relPpb), sameDomain: !!p.sameDomain }))
    .sort((a, b) => b.count - a.count);
  return {
    running: !!r.running,
    known: !!r.known,
    positionSource: r.positionSource || '',
    clock: c ? {
      clockClass: c.clockClass || '', declaredClass: c.declaredClass || '', anchorState: c.anchorState || '', anchorReason: c.anchorReason || '',
      synced: !!c.synced, relativeTime: !!c.relativeTime, sigmaThetaNs: num(c.sigmaThetaNs), driftPpm: num(c.driftPpm),
      betaShortNs: num(c.betaShortNs), jitterNs: num(c.jitterNs), outlierPct: num(c.outlierPct), steps: num(c.steps) ?? 0,
      events: (c.events || []).filter((e) => e && num(e.t) !== undefined).sort((a, b) => b.t - a.t),
    } : null,
    mlat: m ? {
      contribPerHour: num(m.contribPerHour), usedPct: num(m.usedPct), excludedPct: num(m.excludedPct), outlierPct: num(m.outlierPct),
      residualBiasNs: num(m.residualBiasNs), wScale: num(m.wScale), learnedJitterNs: num(m.learnedJitterNs), biasFlag: !!m.biasFlag,
    } : null,
    peers,
    summary: {
      peers: num(s.peers) ?? peers.length,
      syncedPeers: num(s.syncedPeers) ?? num(c?.syncedPeers) ?? 0,
      contribPerHour: num(s.contribPerHour) ?? num(m?.contribPerHour),
      timedPerMin: num(s.timedPerMin) ?? num(r.timedPerMin),
      resultsEligible: s.resultsEligible ?? r.resultsEligible ?? false,
      resultsReceived: num(s.resultsReceived),
    },
    history: r.history && Array.isArray(r.history.t) ? r.history : null,
  };
}

/** Column `key` of a columnar history as [[tMs, v], ...] (null values break the line). */
export function series(hist, key, scale = 1) {
  const t = hist?.t || [];
  const col = hist?.[key] || [];
  const out = new Array(t.length);
  for (let i = 0; i < t.length; i++) {
    const v = col[i];
    out[i] = [t[i], typeof v === 'number' && Number.isFinite(v) ? v * scale : null];
  }
  return out;
}

/** Like series(), but values with fewer than minN samples (column nKey) are null. */
export function seriesMinN(hist, key, nKey, minN) {
  const s = series(hist, key);
  const n = hist?.[nKey] || [];
  for (let i = 0; i < s.length; i++) if (!(n[i] >= minN)) s[i][1] = null;
  return s;
}

/** The newest non-null value of a column, or undefined. */
export function last(hist, key) {
  const col = hist?.[key] || [];
  for (let i = col.length - 1; i >= 0; i--) if (typeof col[i] === 'number' && Number.isFinite(col[i])) return col[i];
  return undefined;
}

/**
 * Sync matrix for a heat map. Stations are grouped into connected components (union-find over the edges) and
 * ordered by component size, then by degree, so synced blocks appear on the diagonal. At most `max` stations
 * (the busiest) are kept; `metric` is 'rmsNs' | 'count' | 'relPpb'.
 * Returns { ids, labels, cells: [[x, y, v]], components, dropped }.
 */
export function syncMatrix(r, { max = 60, metric = 'rmsNs' } = {}) {
  const edges = (r?.edges || []).filter((e) => e && e.a && e.b && e.a !== e.b);
  const names = new Map((r?.stations || []).map((s) => [s.sid, s.name || '']));
  const deg = new Map();
  const parent = new Map();
  const find = (x) => { let p = parent.get(x) ?? x; while (p !== (parent.get(p) ?? p)) p = parent.get(p) ?? p; parent.set(x, p); return p; };
  const unite = (a, b) => { const ra = find(a); const rb = find(b); if (ra !== rb) parent.set(ra, rb); };
  for (const e of edges) {
    deg.set(e.a, (deg.get(e.a) || 0) + (e.count || 0));
    deg.set(e.b, (deg.get(e.b) || 0) + (e.count || 0));
    unite(e.a, e.b);
  }
  for (const s of r?.stations || []) if (!deg.has(s.sid)) deg.set(s.sid, 0);
  let ids = [...deg.keys()];
  const dropped = Math.max(0, ids.length - max);
  if (dropped) ids = ids.sort((a, b) => deg.get(b) - deg.get(a)).slice(0, max);
  const compSize = new Map();
  for (const id of ids) { const c = find(id); compSize.set(c, (compSize.get(c) || 0) + 1); }
  ids.sort((a, b) => {
    const ca = find(a); const cb = find(b);
    if (ca !== cb) return (compSize.get(cb) - compSize.get(ca)) || (ca < cb ? -1 : 1);
    return (deg.get(b) - deg.get(a)) || (a < b ? -1 : 1);
  });
  const pos = new Map(ids.map((id, i) => [id, i]));
  const cells = [];
  for (const e of edges) {
    const x = pos.get(e.a); const y = pos.get(e.b);
    if (x === undefined || y === undefined) continue;
    const v = metric === 'count' ? e.count : metric === 'relPpb' ? Math.abs(e.relPpb) : e.rmsNs;
    if (typeof v === 'number' && Number.isFinite(v)) cells.push([x, y, v]);
  }
  return { ids, labels: ids.map((id) => names.get(id) || shortId(id)), cells, components: new Set(ids.map(find)).size, dropped };
}

const shortId = (sid) => (sid && sid.length > 10 ? sid.slice(0, 8) + '…' : sid || '?');

/** CRLB summary -> rows with the target height in ft and areas per grade. */
export function crlbRows(r) {
  return (r?.summary || []).map((s) => ({ ...s, altFt: Math.round((s.altM || 0) / 0.3048 / 100) * 100, areaKm2Shown: (s.areaKm2A || 0) + (s.areaKm2B || 0) + (s.areaKm2C || 0) }));
}

// ------------------------------------------------------------------ viewport (live.js bbox streams)

/** Aircraft count above which the map switches to a viewport stream in 'auto' mode, and below which it leaves it. */
export const VIEWPORT_ON = 1500;
export const VIEWPORT_OFF = 1000;

/** auto-mode decision with hysteresis. mode 'on' | 'off' | 'auto'. */
export function wantViewport(mode, total, current) {
  if (mode === 'on') return true;
  if (mode === 'off') return false;
  if (!Number.isFinite(total)) return !!current;
  return current ? total > VIEWPORT_OFF : total > VIEWPORT_ON;
}

const clampLat = (v) => Math.max(-90, Math.min(90, v));
const wrapLon = (v) => ((((v + 180) % 360) + 360) % 360) - 180;

/**
 * Map bounds (west, south, east, north in degrees, east may exceed 180 or west be below -180 after panning
 * across the antimeridian) -> [south, west, north, east] padded by `pad` of the size on every side, longitudes
 * wrapped into -180..180 (west > east then means the box crosses the antimeridian); null for the whole world.
 */
export function viewportBox(w, s, e, n, pad = 0.15) {
  if (![w, s, e, n].every(Number.isFinite)) return null;
  const dLon = e - w;
  const dLat = n - s;
  if (dLon <= 0 || dLat <= 0) return null;
  const pw = Math.max(dLon * pad, 0.05);
  const ph = Math.max(dLat * pad, 0.05);
  if (dLon + 2 * pw >= 360) return null;
  const S = clampLat(s - ph);
  const N = clampLat(n + ph);
  if (S <= -90 && N >= 90) return null;
  const r = (v) => Math.round(v * 1000) / 1000;
  let E = wrapLon(r(e + pw));
  if (E === -180) E = 180; // the east edge exactly on the antimeridian
  return [r(S), r(wrapLon(r(w - pw))), r(N), r(E)];
}

/** The `bbox` query value of a box. */
export const bboxParam = (b) => (b ? b.join(',') : '');

// ---- MLAT quality layers of the Live Map (map/network.js; DESIGN §8.2, §11) ----
//   measured  hold-out error per 1° cell and altitude band: public (cells with ≥ 5 contributing stations,
//             GET /api/public/mlat) or admin (every cell, GET /api/admin/mlat/quality?cells=1)
//   expected  CRLB σ_h per grid point at a target height (admin only, GET /api/admin/mlat/crlb)

/** Altitude bands of the quality cells (internal/mlat/quality BandOf). */
export const QUALITY_BANDS = [
  { id: 0, label: 'below 10,000 ft' },
  { id: 1, label: '10,000–25,000 ft' },
  { id: 2, label: 'above 25,000 ft' },
];

/** Colour scale of an error / σ in metres (the grade colours, then red). */
export const QUALITY_SCALE = [
  { max: 50, color: GRADE_COLOR.A, label: '≤ 50 m' },
  { max: 150, color: GRADE_COLOR.B, label: '≤ 150 m' },
  { max: 500, color: GRADE_COLOR.C, label: '≤ 500 m' },
  { max: Infinity, color: '#f87171', label: '> 500 m' },
];

export function qualityColor(m) {
  if (!Number.isFinite(m)) return null;
  return QUALITY_SCALE.find((s) => m <= s.max).color;
}

const square = (s, w, n, e) => [[[w, s], [e, s], [e, n], [w, n], [w, s]]];

/**
 * Measured cells -> GeoJSON polygons of the 1° cells of one altitude band (band < 0 = every band, the best
 * P50 per cell). Properties: c (colour of P50), p50, p95, n. Cells without a finite P50 are skipped.
 */
export function qualityCellsFc(cells, band = -1) {
  const best = new Map();
  for (const c of Array.isArray(cells) ? cells : []) {
    if (!c || !Number.isInteger(c.lat) || !Number.isInteger(c.lon) || !Number.isFinite(c.p50M)) continue;
    if (band >= 0 && c.band !== band) continue;
    if (c.lat < -90 || c.lat > 89 || c.lon < -180 || c.lon > 179) continue;
    const k = c.lat + ':' + c.lon;
    const cur = best.get(k);
    if (!cur || c.p50M < cur.p50M) best.set(k, c);
  }
  const features = [...best.values()].map((c) => ({
    type: 'Feature',
    properties: { c: qualityColor(c.p50M), p50: Math.round(c.p50M), p95: Number.isFinite(c.p95M) ? Math.round(c.p95M) : null, n: c.n ?? null, band: c.band },
    geometry: { type: 'Polygon', coordinates: square(c.lat, c.lon, c.lat + 1, c.lon + 1) },
  }));
  return { type: 'FeatureCollection', features };
}

/** Target heights of the CRLB map (distinct altM values, ascending). */
export function crlbHeights(r) {
  const s = new Set();
  for (const c of r?.cells || []) if (Number.isFinite(c?.altM)) s.add(c.altM);
  return [...s].sort((a, b) => a - b);
}

/**
 * CRLB grid points of one target height -> GeoJSON squares (stepDeg wide, centred on the point).
 * Properties: c (colour of σ_h), s (σ_h m), n (stations).
 */
export function crlbCellsFc(r, altM) {
  const step = Number.isFinite(r?.stepDeg) && r.stepDeg > 0 ? r.stepDeg : 0.25;
  const half = step / 2;
  const features = [];
  for (const c of r?.cells || []) {
    if (!c || c.altM !== altM || !Number.isFinite(c.lat) || !Number.isFinite(c.lon) || !Number.isFinite(c.sigmaHM)) continue;
    features.push({
      type: 'Feature',
      properties: { c: qualityColor(c.sigmaHM), s: Math.round(c.sigmaHM), n: c.n ?? null },
      geometry: { type: 'Polygon', coordinates: square(c.lat - half, c.lon - half, c.lat + half, c.lon + half) },
    });
  }
  return { type: 'FeatureCollection', features };
}

/** Colour ramp of the coverage-improvement layer (share of the best site's gain). */
export const IMPROVE_SCALE = [
  { min: 0.75, color: '#22c55e', label: '≥ 75 %' },
  { min: 0.5, color: '#84cc16', label: '50-75 %' },
  { min: 0.25, color: '#eab308', label: '25-50 %' },
  { min: 0.05, color: '#f97316', label: '5-25 %' },
];

/**
 * Coverage-improvement cells (GET /api/admin/mlat/crlb/improve) -> GeoJSON squares coloured by the benefit of
 * a new station in that cell (score 0..1 of the best site; below 5 % not drawn). The ranked best sites carry
 * their rank as the label ('#1' ...), other cells the benefit in %.
 */
export function improveCellsFc(r) {
  const step = Number.isFinite(r?.stepDeg) && r.stepDeg > 0 ? r.stepDeg : 0.5;
  const half = step / 2;
  const rank = new Map((r?.sites || []).map((s, i) => [`${s.lat}:${s.lon}`, i + 1]));
  const features = [];
  for (const c of r?.cells || []) {
    if (!c || !Number.isFinite(c.lat) || !Number.isFinite(c.lon) || !(c.score >= 0.05)) continue;
    const band = IMPROVE_SCALE.find((b) => c.score >= b.min) || IMPROVE_SCALE[IMPROVE_SCALE.length - 1];
    const k = rank.get(`${c.lat}:${c.lon}`);
    features.push({
      type: 'Feature',
      properties: { c: band.color, g: k ? `#${k}` : `${Math.round(c.score * 100)} %` },
      geometry: { type: 'Polygon', coordinates: square(c.lat - half, c.lon - half, c.lat + half, c.lon + half) },
    });
  }
  return { type: 'FeatureCollection', features };
}

/** Feet label of a CRLB target height. */
export function heightLabel(altM) {
  const ft = Math.round(altM / 0.3048 / 100) * 100;
  return ft >= 10000 && ft % 1000 === 0 ? `FL${String(ft / 100).padStart(3, '0')}` : `${ft.toLocaleString('en-GB')} ft`;
}
