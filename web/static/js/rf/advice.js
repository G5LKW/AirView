// rf/advice.js - RF health items / advice and terrain advice sentences in the user's display units.
//
// The analytics side writes plain-language sentences with fixed units (km, km², ft, m). Newer servers
// add machine-readable fields next to the prose: `code` (stable identifier) and `data` (numbers keyed by
// what they are: distKm, altFt, heightM, areaKm2, rateHz, db, pct ...). localise() rewrites the
// unit-bearing numbers of the sentence in the current units.js system:
//   - every "<number> km|km²|ft|m" in the text is converted (km -> nm / km / mi, ft -> ft / m, ...);
//   - when `data` carries the same quantity, its exact value replaces the rounded one from the prose.
// Numbers without a unit-dependent unit (%, dB, Hz, °) are left as written. Anything that is not
// recognised stays exactly as the server wrote it, so `text` is always the fallback.
//
//   import { localise, itemText } from '../rf/advice.js';
//   itemText(item, units)        // item: string | { text, code?, data? }

const NUM = '(-?\\d{1,3}(?:,\\d{3})+(?:\\.\\d+)?|-?\\d+(?:\\.\\d+)?)';
// order matters: km² before km; "km/h", "ft/min", "m/s" are speeds / rates and are left alone
const UNIT_RE = new RegExp(`${NUM}(\\s|\\u00a0)?(km²|km2|km|ft|m)(?![\\w²/])`, 'g');

/** data key suffix -> quantity of that unit */
const KEY_KIND = [
  [/km2$|Km2$|areaKm/i, 'area'],
  [/Km$|^km$/i, 'dist'],
  [/Ft$|^ft$/i, 'alt'],
  [/(?:^|[a-z])M$|^m$|Metres?$|Meters?$/, 'height'],
];
const UNIT_KIND = { km: 'dist', km2: 'area', 'km²': 'area', ft: 'alt', m: 'height' };

function kindOfKey(k) {
  for (const [re, kind] of KEY_KIND) if (re.test(k)) return kind;
  return null;
}

/** Numbers of `data` grouped by quantity (only unit-dependent ones). */
function dataByKind(data) {
  const out = {};
  if (!data || typeof data !== 'object') return out;
  for (const [k, v] of Object.entries(data)) {
    const list = Array.isArray(v) ? v : [v];
    const kind = kindOfKey(k);
    if (!kind) continue;
    for (const x of list) if (typeof x === 'number' && Number.isFinite(x)) (out[kind] ||= []).push(x);
  }
  return out;
}

function fmt(units, kind, v) {
  switch (kind) {
    case 'dist': return units.dist(v);
    case 'area': return units.area(v);
    case 'alt': return units.alt(v);
    case 'height': return units.height(v);
    default: return null;
  }
}

/**
 * Rewrite the unit-bearing numbers of a server sentence in the display units.
 * @param {string} text  server prose (fixed km / km² / ft / m)
 * @param {object} units the units.js singleton
 * @param {object} [data] machine-readable numbers of the sentence (exact values preferred over the prose)
 */
export function localise(text, units, data) {
  if (typeof text !== 'string' || !text) return text || '';
  const exact = dataByKind(data);
  return text.replace(UNIT_RE, (m, numStr, _sp, unit) => {
    const kind = UNIT_KIND[unit];
    const v = Number(numStr.replace(/,/g, ''));
    if (!kind || !Number.isFinite(v)) return m;
    // the prose rounds: take a data value that rounds to the same number, if there is one
    const dec = (numStr.split('.')[1] || '').length;
    const tol = 0.5 * 10 ** -dec + 1e-9;
    const cand = (exact[kind] || []).find((x) => Math.abs(x - v) <= tol || Math.abs(Math.abs(x) - Math.abs(v)) <= tol);
    const val = cand !== undefined ? (Math.sign(v) === Math.sign(cand) || v === 0 ? cand : -cand) : v;
    const s = fmt(units, kind, val);
    return s && s !== units.DASH ? s : m;
  });
}

/** Display text of a health item / advice entry: a plain string, or { text, code?, data? }. */
export function itemText(item, units) {
  if (item === null || item === undefined) return '';
  if (typeof item === 'string') return localise(item, units);
  return localise(item.text || '', units, item.data);
}
