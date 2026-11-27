// map/overlays.js - GeoJSON builders for the receiver-centred overlays (range rings, compass rose,
// antenna beam wedges) and the day/night terminator.

import { geo } from '../geo.js';

const fc = (features = []) => ({ type: 'FeatureCollection', features });
export const EMPTY = fc();

const DIRECTIONAL = new Set(['yagi', 'patch']);

/** Typical -3 dB horizontal beamwidth for a yagi from its element count, used when none is configured. */
export function estimateYagiBeamwidth(elements) {
  if (!elements || elements < 2) return 60;
  // Rule of thumb: boom gain grows ~ with element count; HPBW ~ sqrt(41253 / G) for a roughly symmetric beam.
  const gainDbi = Math.min(18, 4 + 10 * Math.log10(elements) * 1.05);
  const g = 10 ** (gainDbi / 10);
  return Math.max(18, Math.min(90, Math.sqrt(41253 / g)));
}

/**
 * Build all receiver-centred overlays.
 * @param {object} cfg model.Config
 * @param {object} units units module (labels in the user's units)
 * @returns {{rings: object, ringLabels: object, compass: object, compassLabels: object, beams: object, beamAxes: object, receiver: object, outerKm: number}}
 */
export function receiverOverlays(cfg, units) {
  const rx = cfg?.receiver;
  if (!rx || !rx.located) {
    return { rings: EMPTY, ringLabels: EMPTY, compass: EMPTY, compassLabels: EMPTY, beams: EMPTY, beamAxes: EMPTY, receiver: EMPTY, outerKm: 0 };
  }
  const { lat, lon } = rx;
  const ringKm = [...new Set((cfg.ui?.rangeRingsKm || []).map(Number).filter((k) => k > 0 && k < 5000))].sort((a, b) => a - b);
  const outerKm = ringKm.length ? ringKm[ringKm.length - 1] : 300;

  const rings = [];
  const ringLabels = [];
  ringKm.forEach((km, i) => {
    rings.push({ type: 'Feature', properties: { km, outer: i === ringKm.length - 1 ? 1 : 0 }, geometry: { type: 'LineString', coordinates: geo.circle(lat, lon, km, 180) } });
    const text = units.dist(km, { digits: 0 });
    for (const brg of [0, 180]) {
      ringLabels.push({ type: 'Feature', properties: { text }, geometry: { type: 'Point', coordinates: geo.destination(lat, lon, brg, km) } });
    }
  });

  // compass rose just outside the outer ring: ticks every 5 deg, longer every 10 / 30, labels every 30
  const compass = [];
  const compassLabels = [];
  for (let b = 0; b < 360; b += 5) {
    const major = b % 30 === 0;
    const mid = b % 10 === 0;
    const len = outerKm * (major ? 0.045 : mid ? 0.028 : 0.016);
    compass.push({ type: 'Feature', properties: { major: major ? 1 : 0 }, geometry: { type: 'LineString', coordinates: geo.unwrap([geo.destination(lat, lon, b, outerKm), geo.destination(lat, lon, b, outerKm + len)]) } });
    if (major) {
      const text = b === 0 ? 'N' : b === 90 ? 'E' : b === 180 ? 'S' : b === 270 ? 'W' : String(b).padStart(3, '0');
      compassLabels.push({ type: 'Feature', properties: { text, cardinal: b % 90 === 0 ? 1 : 0 }, geometry: { type: 'Point', coordinates: geo.destination(lat, lon, b, outerKm * 1.085) } });
    }
  }
  // spokes every 30 deg from the first ring outwards, very faint
  const innerKm = ringKm.length ? ringKm[0] : outerKm / 4;
  for (let b = 0; b < 360; b += 30) {
    compass.push({ type: 'Feature', properties: { spoke: 1, major: 0 }, geometry: { type: 'LineString', coordinates: geo.unwrap([geo.destination(lat, lon, b, innerKm * 0.5), ...[0.25, 0.5, 0.75, 1].map((f) => geo.destination(lat, lon, b, innerKm * 0.5 + (outerKm - innerKm * 0.5) * f))]) } });
  }

  const beams = [];
  const beamAxes = [];
  for (const ant of cfg.antennas || []) {
    if (!DIRECTIONAL.has(ant.type)) continue;
    const width = ant.hBeamwidthDeg > 0 ? ant.hBeamwidthDeg : ant.type === 'yagi' ? estimateYagiBeamwidth(ant.elements) : 65;
    const az = ((ant.azimuthDeg % 360) + 360) % 360;
    const color = /^#[0-9a-f]{6}$/i.test(ant.color || '') ? ant.color : '#f59e0b';
    const props = { id: ant.id, name: ant.name || ant.id, color, az, width };
    beams.push({ type: 'Feature', properties: props, geometry: { type: 'Polygon', coordinates: [geo.sector(lat, lon, outerKm, az, width, 1)] } });
    beamAxes.push({ type: 'Feature', properties: { ...props, text: `${ant.name || ant.id} · ${Math.round(az)}° · ${Math.round(width)}°` }, geometry: { type: 'LineString', coordinates: geo.unwrap([[lon, lat], ...[0.25, 0.5, 0.75, 1].map((f) => geo.destination(lat, lon, az, outerKm * f))]) } });
  }

  const receiver = fc([{ type: 'Feature', properties: { name: rx.name || 'Receiver' }, geometry: { type: 'Point', coordinates: [lon, lat] } }]);
  return { rings: fc(rings), ringLabels: fc(ringLabels), compass: fc(compass), compassLabels: fc(compassLabels), beams: fc(beams), beamAxes: fc(beamAxes), receiver, outerKm };
}

/**
 * Night-side overlay: four nested polygons (sun below 0 / 6 / 12 / 18 deg) that are drawn with the
 * same low opacity, so the shade deepens through civil, nautical and astronomical twilight.
 */
export function nightOverlay(date = Date.now()) {
  return fc([0, 6, 12, 18].map((dep) => ({ type: 'Feature', properties: { depression: dep }, geometry: geo.nightPolygon(date, dep) })));
}

/** Normalise whatever another page published as coverage into a FeatureCollection. */
export function coverageCollection(data) {
  if (!data) return EMPTY;
  if (data.type === 'FeatureCollection') return data;
  if (data.type === 'Feature') return fc([data]);
  if (data.type && data.coordinates) return fc([{ type: 'Feature', properties: {}, geometry: data }]);
  return EMPTY;
}

/** [west, south, east, north] of a set of [lon, lat] points, or null. */
export function bounds(points) {
  let w = Infinity;
  let s = Infinity;
  let e = -Infinity;
  let n = -Infinity;
  for (const p of points) {
    if (p[0] < w) w = p[0];
    if (p[0] > e) e = p[0];
    if (p[1] < s) s = p[1];
    if (p[1] > n) n = p[1];
  }
  return Number.isFinite(w) ? [w, s, e, n] : null;
}
