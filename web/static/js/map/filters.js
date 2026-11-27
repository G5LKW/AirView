// map/filters.js - the Live Map's advanced aircraft filter: model, predicate, chips, URL form and
// the filter panel (altitude / speed ranges, text fields, category, source, flags).
//
// The filter object is plain JSON (stored in the map prefs and in the #/map?filter= parameter):
//   { altMin, altMax (ft), spdMin, spdMax (kt), callsign, type, operator, cat, src,
//     mil, hideGround, hideVehicles, alerting, predicted }
// Missing / null / '' / false members are inactive.

import { ui, h, setText } from '../ui.js';
import { units } from '../units.js';
import { isEmergency } from './list.js';

export const ALT_MAX_FT = 50000;
export const SPD_MAX_KT = 700;

export const CATEGORIES = [
  { value: '', label: 'Any category' },
  { value: 'A1', label: 'A1 · Light' },
  { value: 'A2', label: 'A2 · Small' },
  { value: 'A3', label: 'A3 · Large' },
  { value: 'A4', label: 'A4 · High vortex (B757)' },
  { value: 'A5', label: 'A5 · Heavy' },
  { value: 'A6', label: 'A6 · High performance' },
  { value: 'A7', label: 'A7 · Rotorcraft' },
  { value: 'B', label: 'B · Glider / balloon / UAV' },
  { value: 'C', label: 'C · Surface vehicle / obstacle' },
];
export const SOURCES = [
  { value: '', label: 'Any' },
  { value: 'adsb', label: 'ADS-B' },
  { value: 'mlat', label: 'MLAT' },
  { value: 'tisb', label: 'TIS-B' },
  { value: 'modes', label: 'Mode S' },
];

const n = (v) => typeof v === 'number' && Number.isFinite(v);
const txt = (v) => (typeof v === 'string' ? v.trim() : '');

/** Normalise anything (prefs, URL) into a clean filter object; unknown members are dropped. */
export function normalize(f) {
  const o = f && typeof f === 'object' ? f : {};
  const num = (v, max) => (n(v) && v >= 0 && v <= max ? Math.round(v) : null);
  const out = {
    altMin: num(o.altMin, ALT_MAX_FT), altMax: num(o.altMax, ALT_MAX_FT),
    spdMin: num(o.spdMin, SPD_MAX_KT), spdMax: num(o.spdMax, SPD_MAX_KT),
    callsign: txt(o.callsign).slice(0, 24), type: txt(o.type).slice(0, 24), operator: txt(o.operator).slice(0, 40),
    cat: CATEGORIES.some((c) => c.value === o.cat) ? o.cat : '',
    src: SOURCES.some((c) => c.value === o.src) ? o.src : '',
    mil: !!o.mil, hideGround: !!o.hideGround, hideVehicles: !!o.hideVehicles, alerting: !!o.alerting, predicted: !!o.predicted,
  };
  if (out.altMin === 0) out.altMin = null;
  if (out.altMax === ALT_MAX_FT) out.altMax = null;
  if (out.spdMin === 0) out.spdMin = null;
  if (out.spdMax === SPD_MAX_KT) out.spdMax = null;
  return out;
}

export const isActive = (f) => chips(f).length > 0;

const alt = (a) => (a.onGround ? 0 : a.altBaro ?? a.altGeom);
const isVehicle = (a) => typeof a.category === 'string' && a.category[0] === 'C';

/** Compile a filter into a fast predicate (a) => boolean, or null when nothing is active. */
export function compile(fIn) {
  const f = normalize(fIn);
  const tests = [];
  if (f.altMin !== null || f.altMax !== null) {
    const lo = f.altMin ?? -Infinity;
    const hi = f.altMax ?? Infinity;
    tests.push((a) => { const v = alt(a); return n(v) && v >= lo && v <= hi; });
  }
  if (f.spdMin !== null || f.spdMax !== null) {
    const lo = f.spdMin ?? -Infinity;
    const hi = f.spdMax ?? Infinity;
    tests.push((a) => n(a.gs) && a.gs >= lo && a.gs <= hi);
  }
  const like = (key, get) => {
    const words = f[key].toLowerCase().split(/[\s,]+/).filter(Boolean);
    if (!words.length) return;
    tests.push((a) => { const v = (get(a) || '').toLowerCase(); return words.some((w) => v.includes(w)); });
  };
  like('callsign', (a) => a.callsign);
  like('type', (a) => `${a.type || ''} ${a.typeDesc || ''}`);
  like('operator', (a) => a.operator);
  if (f.cat) tests.push(f.cat.length === 1 ? (a) => typeof a.category === 'string' && a.category[0] === f.cat : (a) => a.category === f.cat);
  if (f.src) tests.push(f.src === 'tisb' ? (a) => a.source === 'tisb' || a.source === 'adsr' : (a) => a.source === f.src);
  if (f.mil) tests.push((a) => !!a.military);
  if (f.hideGround) tests.push((a) => !a.onGround);
  if (f.hideVehicles) tests.push((a) => !isVehicle(a));
  if (f.alerting) tests.push((a) => !!a.highlight || isEmergency(a));
  if (f.predicted) tests.push((a) => !!a.predicted);
  if (!tests.length) return null;
  return (a) => { for (const t of tests) if (!t(a)) return false; return true; };
}

/** Active-filter chips: [{ key, label }] (key = the member(s) to clear). */
export function chips(fIn) {
  const f = normalize(fIn);
  const out = [];
  const range = (lo, hi, fmt) => (lo !== null && hi !== null ? `${fmt(lo)} – ${fmt(hi)}` : lo !== null ? `≥ ${fmt(lo)}` : `≤ ${fmt(hi)}`);
  if (f.altMin !== null || f.altMax !== null) out.push({ key: ['altMin', 'altMax'], label: 'Alt ' + range(f.altMin, f.altMax, (v) => units.alt(v)) });
  if (f.spdMin !== null || f.spdMax !== null) out.push({ key: ['spdMin', 'spdMax'], label: 'Speed ' + range(f.spdMin, f.spdMax, (v) => units.speed(v)) });
  if (f.callsign) out.push({ key: ['callsign'], label: `Callsign “${f.callsign}”` });
  if (f.type) out.push({ key: ['type'], label: `Type “${f.type}”` });
  if (f.operator) out.push({ key: ['operator'], label: `Operator “${f.operator}”` });
  if (f.cat) out.push({ key: ['cat'], label: CATEGORIES.find((c) => c.value === f.cat).label });
  if (f.src) out.push({ key: ['src'], label: SOURCES.find((c) => c.value === f.src).label + ' only' });
  if (f.mil) out.push({ key: ['mil'], label: 'Military' });
  if (f.hideGround) out.push({ key: ['hideGround'], label: 'No ground traffic' });
  if (f.hideVehicles) out.push({ key: ['hideVehicles'], label: 'No vehicles' });
  if (f.alerting) out.push({ key: ['alerting'], label: 'Alerting' });
  if (f.predicted) out.push({ key: ['predicted'], label: 'Predicted only' });
  return out;
}

/**
 * Compact URL form: "alt:0-20000|spd:100-|cs:DLH|mil|pred". Text values are written raw except that
 * "\", "|" and "%" are escaped with a backslash; the caller's URLSearchParams then percent-encodes the
 * whole parameter exactly once (older links encoded the text values a second time, see fromParam).
 */
export function toParam(fIn) {
  const f = normalize(fIn);
  const p = [];
  const r = (lo, hi) => `${lo ?? ''}-${hi ?? ''}`;
  const esc = (v) => String(v).replace(/[\\|%]/g, (c) => '\\' + c);
  if (f.altMin !== null || f.altMax !== null) p.push('alt:' + r(f.altMin, f.altMax));
  if (f.spdMin !== null || f.spdMax !== null) p.push('spd:' + r(f.spdMin, f.spdMax));
  if (f.callsign) p.push('cs:' + esc(f.callsign));
  if (f.type) p.push('type:' + esc(f.type));
  if (f.operator) p.push('op:' + esc(f.operator));
  if (f.cat) p.push('cat:' + f.cat);
  if (f.src) p.push('src:' + f.src);
  for (const [k, s] of [['mil', 'mil'], ['hideGround', 'nognd'], ['hideVehicles', 'noveh'], ['alerting', 'alert'], ['predicted', 'pred']]) if (f[k]) p.push(s);
  return p.join('|');
}

/** Split on "|" outside backslash escapes; each part keeps its escapes (and a flag whether it had any). */
function splitParts(s) {
  const out = [];
  let cur = '';
  let escaped = false;
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (c === '\\' && i + 1 < s.length) { cur += c + s[++i]; escaped = true; continue; }
    if (c === '|') { out.push({ part: cur, escaped }); cur = ''; escaped = false; continue; }
    cur += c;
  }
  out.push({ part: cur, escaped });
  return out;
}

/**
 * Parse the URL form back (tolerant: unknown parts are ignored). Text values of the current form are
 * backslash-escaped; a value with a bare "%" comes from an older, double-encoded link and is
 * percent-decoded instead (the current form never writes a bare "%").
 */
export function fromParam(s) {
  const f = {};
  const dec = (v, escaped) => {
    if (escaped || v.includes('\\')) return v.replace(/\\(.)/g, '$1');
    if (/%[0-9A-Fa-f]{2}/.test(v)) { try { return decodeURIComponent(v); } catch { return v; } }
    return v;
  };
  const rng = (v) => v.split('-').map((x) => (x === '' ? null : Number(x)));
  for (const { part, escaped } of splitParts(String(s || ''))) {
    const i = part.indexOf(':');
    const k = i < 0 ? part : part.slice(0, i);
    const v = i < 0 ? '' : part.slice(i + 1);
    switch (k) {
      case 'alt': [f.altMin, f.altMax] = rng(v); break;
      case 'spd': [f.spdMin, f.spdMax] = rng(v); break;
      case 'cs': f.callsign = dec(v, escaped); break;
      case 'type': f.type = dec(v, escaped); break;
      case 'op': f.operator = dec(v, escaped); break;
      case 'cat': f.cat = v; break;
      case 'src': f.src = v; break;
      case 'mil': f.mil = true; break;
      case 'nognd': f.hideGround = true; break;
      case 'noveh': f.hideVehicles = true; break;
      case 'alert': f.alerting = true; break;
      case 'pred': f.predicted = true; break;
      default: break;
    }
  }
  return normalize(f);
}

/**
 * Two-handle range slider working in the user's display unit.
 * @param {{kind: 'alt'|'speed', maxBase: number, step: number, lo: number|null, hi: number|null, onChange(lo, hi)}} o
 * lo / hi and the callback are in base units (null = open end).
 */
function rangeSlider(o) {
  const max = Math.round(units.value(o.kind, o.maxBase));
  const step = o.step;
  const toUser = (v, def) => (v === null ? def : Math.round(units.value(o.kind, v) / step) * step);
  const a = h('input', { type: 'range', min: 0, max, step, 'aria-label': 'Minimum' });
  const b = h('input', { type: 'range', min: 0, max, step, 'aria-label': 'Maximum' });
  a.value = toUser(o.lo, 0);
  b.value = toUser(o.hi, max);
  const out = h('span.mapv-range-out.num');
  const track = h('div.mapv-range-track');
  const fmt = (v, open) => (open ? (o.kind === 'alt' ? (v === 0 ? 'GND' : '∞') : '∞') : units.format(o.kind, units.toBase(o.kind, v), { unit: false }));
  const sync = (emit) => {
    let lo = Number(a.value);
    let hi = Number(b.value);
    if (lo > hi) { if (document.activeElement === a) { hi = lo; b.value = hi; } else { lo = hi; a.value = lo; } }
    track.style.setProperty('--lo', (lo / max) * 100 + '%');
    track.style.setProperty('--hi', (hi / max) * 100 + '%');
    setText(out, `${lo === 0 ? (o.kind === 'alt' ? 'GND' : '0') : fmt(lo)} – ${hi >= max ? '∞' : fmt(hi)} ${units.label(o.kind)}`);
    if (emit) o.onChange(lo <= 0 ? null : units.toBase(o.kind, lo), hi >= max ? null : units.toBase(o.kind, hi));
  };
  const emitter = ui.debounce(() => sync(true), 150);
  for (const inp of [a, b]) inp.addEventListener('input', () => { sync(false); emitter(); });
  sync(false);
  const el = h('div.mapv-range', h('div.mapv-range-box', track, a, b), out);
  el.reset = () => { a.value = 0; b.value = max; sync(false); };
  return el;
}

/**
 * Open the filter panel anchored to `anchor`.
 * @param {HTMLElement} anchor
 * @param {object} current filter
 * @param {(f: object) => void} onChange called with the new normalised filter (debounced for text)
 * @returns popover controller
 */
export function openFilterPanel(anchor, current, onChange, opts = {}) {
  let f = normalize(current);
  const commit = () => onChange(normalize(f));
  const set = (patch) => { f = { ...f, ...patch }; commit(); };
  const text = (key, placeholder) => ui.input({ size: 'sm', value: f[key], placeholder, clearable: true, spellcheck: 'false', autocomplete: 'off', onInput: ui.debounce((v) => set({ [key]: v }), 250) });
  const tgl = (key, label, hint) => ui.field({ label, hint, row: true, control: ui.toggle({ checked: f[key], onChange: (v) => set({ [key]: v }) }) });

  const altR = rangeSlider({ kind: 'alt', maxBase: ALT_MAX_FT, step: units.system === 'metric' ? 100 : 500, lo: f.altMin, hi: f.altMax, onChange: (lo, hi) => set({ altMin: lo, altMax: hi }) });
  const spdR = rangeSlider({ kind: 'speed', maxBase: SPD_MAX_KT, step: 10, lo: f.spdMin, hi: f.spdMax, onChange: (lo, hi) => set({ spdMin: lo, spdMax: hi }) });

  const body = h('div.mapv-panel.mapv-filter-panel',
    ui.section('Altitude', 'arrow-up'), altR,
    ui.section('Ground speed', 'bolt'), spdR,
    ui.section('Identity', 'search'),
    ui.field({ label: 'Callsign', control: text('callsign', 'e.g. DLH, BAW12') }),
    ui.field({ label: 'Type', control: text('type', 'e.g. A32, B77, Cessna') }),
    ui.field({ label: 'Operator', control: text('operator', 'e.g. Lufthansa') }),
    ui.field({ label: 'Category', control: ui.select({ size: 'sm', options: CATEGORIES, value: f.cat, onChange: (v) => set({ cat: v }) }) }),
    ui.field({ label: 'Position source', control: ui.segmented({ block: true, options: SOURCES, value: f.src, onChange: (v) => set({ src: v }) }) }),
    ui.section('Show', 'eye'),
    tgl('mil', 'Military only'),
    tgl('hideGround', 'Hide aircraft on the ground'),
    tgl('hideVehicles', 'Hide surface vehicles'),
    tgl('alerting', 'Alerting only', 'Alert-rule highlight or emergency'),
    tgl('predicted', 'Predicted only', 'Aircraft coasting on a predicted position'),
    h('div.mapv-panel-foot', ui.button({ label: 'Clear all filters', size: 'sm', variant: 'ghost', icon: 'x', onClick: () => { f = normalize({}); commit(); pop.close(); } })));
  const pop = ui.popover(anchor, body, { placement: opts.placement || 'left-start', onClose: opts.onClose });
  return pop;
}
