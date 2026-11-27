// alerts/model.js - condition catalogue, normalisation, client-side validation and plain-language summaries
// of alert rules (alerts.Rule / alerts.Cond, see docs/API.md "Alerts"). No DOM here.

/** Severity levels with their badge kinds. */
export const SEVERITIES = [
  { value: 'info', label: 'Info', badge: 'blue' },
  { value: 'warning', label: 'Warning', badge: 'amber' },
  { value: 'critical', label: 'Critical', badge: 'solid-red' },
];
export const severityBadge = (s) => (SEVERITIES.find((x) => x.value === s) || SEVERITIES[0]).badge;

export const SOURCES = [
  { value: 'adsb', label: 'ADS-B' }, { value: 'mlat', label: 'MLAT' }, { value: 'tisb', label: 'TIS-B' },
  { value: 'adsr', label: 'ADS-R' }, { value: 'modes', label: 'Mode S' }, { value: 'other', label: 'Other' },
];
export const EMERGENCIES = [
  { value: 'general', label: 'General' }, { value: 'medical', label: 'Medical' }, { value: 'minfuel', label: 'Minimum fuel' },
  { value: 'nordo', label: 'No comms' }, { value: 'unlawful', label: 'Unlawful interference' }, { value: 'downed', label: 'Downed' },
];
export const SYSTEM_EVENTS = [
  { value: 'record', label: 'New record' }, { value: 'feed_down', label: 'Feed down' }, { value: 'feed_up', label: 'Feed up' },
  { value: 'ntp_unsync', label: 'NTP lost sync' }, { value: 'ntp_sync', label: 'NTP synced' },
  { value: 'range_collapse', label: 'Range collapse' }, { value: 'range_restored', label: 'Range restored' },
  { value: 'new_airframe', label: 'New airframe' }, { value: 'custom', label: 'Custom' },
];
export const CATEGORIES = [
  ['A1', 'Light'], ['A2', 'Small'], ['A3', 'Large'], ['A4', 'B757 class'], ['A5', 'Heavy'], ['A6', 'High performance'],
  ['A7', 'Rotorcraft'], ['B1', 'Glider'], ['B2', 'Balloon'], ['B3', 'Parachutist'], ['B4', 'Ultralight'], ['B6', 'UAV'],
  ['B7', 'Space vehicle'], ['C1', 'Emergency vehicle'], ['C2', 'Service vehicle'], ['C3', 'Obstacle'],
].map(([value, label]) => ({ value, label: `${value} · ${label}` }));

/**
 * Leaf condition catalogue. kind:
 *   values  - list of globs typed as text (comma / newline separated), optional `choices` shown as chips
 *   choice  - multi-select chips (`choices`), optional free text (`free`)
 *   bool    - `value` flag (default true)
 *   range   - min / max; `unit` = units.js kind (converted for display) or a plain label
 *   bearing - from / to degrees
 *   geofence - circle or polygon on a map
 */
export const CONDS = [
  { type: 'icao', group: 'Identity', label: 'ICAO address', kind: 'values', ph: '3C6444, 3C*, 3C0000-3CFFFF', hint: 'Hex addresses, * / ? patterns or ranges.' },
  { type: 'callsign', group: 'Identity', label: 'Callsign', kind: 'values', ph: 'DLH*, BAW1??', hint: 'Patterns with * and ?, case-insensitive.' },
  { type: 'reg', group: 'Identity', label: 'Registration', kind: 'values', ph: 'D-A*, G-EUU?' },
  { type: 'operator', group: 'Identity', label: 'Operator', kind: 'values', ph: '*Lufthansa*, *POLICE*' },
  { type: 'actype', group: 'Identity', label: 'Aircraft type', kind: 'values', ph: 'A38*, B74?, C17', hint: 'ICAO type designators.' },
  { type: 'category', group: 'Identity', label: 'Emitter category', kind: 'values', ph: 'A5, A7, B*', choices: CATEGORIES },
  { type: 'country', group: 'Identity', label: 'Country', kind: 'values', ph: 'DE, United Kingdom, *land', hint: 'ISO codes or country-name patterns.' },
  { type: 'military', group: 'Identity', label: 'Military', kind: 'bool', yes: 'is military', no: 'is not military' },
  { type: 'watchlist', group: 'Identity', label: 'On a watch-list', kind: 'choice', free: true, hint: 'No list selected = on any watch-list.' },
  { type: 'newAirframe', group: 'Identity', label: 'New airframe', kind: 'bool', yes: 'never seen before', no: 'seen before' },
  { type: 'squawk', group: 'Transponder', label: 'Squawk', kind: 'values', ph: '7700, 76*, 1200' },
  { type: 'emergency', group: 'Transponder', label: 'Emergency', kind: 'choice', choices: EMERGENCIES, hint: 'Nothing selected = any emergency state.' },
  { type: 'source', group: 'Transponder', label: 'Position source', kind: 'choice', choices: SOURCES, required: true },
  { type: 'adsbVersion', group: 'Transponder', label: 'ADS-B version', kind: 'range', unit: '', lo: 0, hi: 7, step: 1 },
  { type: 'nic', group: 'Transponder', label: 'NIC', kind: 'range', unit: '', lo: 0, hi: 11, step: 1 },
  { type: 'rssi', group: 'Transponder', label: 'Signal (RSSI)', kind: 'range', unit: 'dBFS', lo: -100, hi: 10, step: 0.5 },
  { type: 'altitude', group: 'Flight', label: 'Altitude', kind: 'range', unit: 'alt', lo: -2000, hi: 100000 },
  { type: 'speed', group: 'Flight', label: 'Ground speed', kind: 'range', unit: 'speed', lo: 0, hi: 5000 },
  { type: 'vrate', group: 'Flight', label: 'Vertical rate', kind: 'range', unit: 'vrate', lo: -30000, hi: 30000 },
  { type: 'onGround', group: 'Flight', label: 'On the ground', kind: 'bool', yes: 'on the ground', no: 'airborne' },
  { type: 'distance', group: 'Position', label: 'Distance from station', kind: 'range', unit: 'dist', lo: 0, hi: 20000 },
  { type: 'bearing', group: 'Position', label: 'Bearing sector', kind: 'bearing' },
  { type: 'geofence', group: 'Position', label: 'Geofence', kind: 'geofence' },
  { type: 'system', group: 'System', label: 'System event', kind: 'choice', choices: SYSTEM_EVENTS, free: true, required: true, hint: 'Extra entries as kind:key patterns, e.g. record:max_dist*.' },
];
export const condDef = (type) => CONDS.find((c) => c.type === type);
export const COND_GROUPS = ['Identity', 'Transponder', 'Flight', 'Position', 'System'];

const isGroup = (c) => !!c && (c.op !== undefined || Array.isArray(c.conds)) && !c.type;
export { isGroup };

export const clone = (x) => (x === undefined ? undefined : JSON.parse(JSON.stringify(x)));

/** New leaf with sensible defaults. */
export function newLeaf(type, rx) {
  const c = { type };
  const d = condDef(type);
  if (!d) return c;
  if (d.kind === 'values') c.values = [];
  else if (d.kind === 'choice') c.values = [];
  else if (d.kind === 'bool') c.value = true;
  else if (d.kind === 'bearing') { c.from = 0; c.to = 90; }
  else if (d.kind === 'geofence') {
    c.lat = rx?.located ? round(rx.lat, 5) : 50;
    c.lon = rx?.located ? round(rx.lon, 5) : 8;
    c.radiusKm = 25;
    c.inside = true;
  } else if (d.kind === 'range') {
    if (type === 'altitude') c.max = 10000;
    else if (type === 'distance') c.max = 25;
    else if (type === 'speed') c.min = 500;
    else if (type === 'vrate') c.max = -2500;
    else if (type === 'rssi') c.min = -10;
    else c.min = d.lo;
  }
  return c;
}

/** Editor form of a rule: match is always a group. */
export function toDraft(rule) {
  const r = clone(rule) || {};
  r.name = r.name || '';
  r.enabled = r.enabled !== false;
  r.severity = r.severity || 'info';
  r.cooldownSec = r.cooldownSec || 900;
  r.actions = { notify: true, sound: false, log: true, ...(r.actions || {}) };
  let m = r.match;
  if (!m || (!isGroup(m) && !m.type)) m = { op: 'and', conds: [] };
  else if (!isGroup(m)) m = { op: 'and', conds: [m] };
  if (!m.op) m.op = 'and';
  if (!Array.isArray(m.conds)) m.conds = [];
  r.match = m;
  return r;
}

/** Strip editor-only fields and simplify a single-child root group for the API. */
/**
 * Condition types that use the aircraft position. A rule with one of them skips aircraft whose
 * position is predicted (coasting) unless rule.includePredicted is true (server: internal/alerts).
 */
export const POSITION_CONDS = new Set(['altitude', 'distance', 'geofence', 'bearing']);

/** True when the match tree contains a position condition. */
export function usesPosition(m) {
  if (!m) return false;
  if (isGroup(m)) return m.conds.some(usesPosition);
  return POSITION_CONDS.has(m.type);
}

export function fromDraft(draft) {
  const r = clone(draft);
  const strip = (c) => {
    if (isGroup(c)) {
      const out = { op: c.op === 'or' ? 'or' : 'and', conds: c.conds.map(strip) };
      if (c.negate) out.negate = true;
      return out;
    }
    const out = { type: c.type };
    for (const k of ['values', 'min', 'max', 'value', 'lat', 'lon', 'radiusKm', 'polygon', 'inside', 'from', 'to']) {
      const v = c[k];
      if (v === undefined || v === null || v === '') continue;
      if (Array.isArray(v) && !v.length) continue;
      out[k] = v;
    }
    if (c.negate) out.negate = true;
    // A geofence is either a circle or a polygon.
    if (c.type === 'geofence') {
      if (c.shape === 'polygon') { delete out.lat; delete out.lon; delete out.radiusKm; } else delete out.polygon;
    }
    return out;
  };
  let m = strip(r.match);
  if (isGroup(m) && m.conds.length === 1 && !m.negate) m = m.conds[0];
  r.match = m;
  if (!r.actions.webhook) delete r.actions.webhook;
  if (!r.actions.highlight) delete r.actions.highlight;
  if (!r.description) delete r.description;
  if (!r.message) delete r.message;
  if (!r.ruleCooldownSec) delete r.ruleCooldownSec;
  if (r.includePredicted) r.includePredicted = true; else delete r.includePredicted;
  for (const k of ['created', 'updated', 'builtin']) delete r[k];
  return r;
}

const fin = (v) => typeof v === 'number' && Number.isFinite(v);
function round(v, d) { const k = 10 ** d; return Math.round(v * k) / k; }
export { round };

/**
 * Client-side validation mirroring internal/alerts/rule.go.
 * @returns {Array<{path: string, msg: string}>} path = 'name' | 'match' | 'cond:<id>' | 'webhook' ...
 */
export function validate(draft, idOf) {
  const out = [];
  if (!String(draft.name || '').trim()) out.push({ path: 'name', msg: 'Give the rule a name.' });
  else if (draft.name.length > 100) out.push({ path: 'name', msg: 'At most 100 characters.' });
  if ((draft.message || '').length > 500) out.push({ path: 'message', msg: 'At most 500 characters.' });
  const cd = Number(draft.cooldownSec);
  if (!fin(cd) || cd < 0 || cd > 604800) out.push({ path: 'cooldown', msg: 'Between 0 and 7 days.' });
  const wh = draft.actions?.webhook;
  if (wh && !/^https?:\/\/[^\s/]+\S*$/i.test(wh)) out.push({ path: 'webhook', msg: 'An absolute http:// or https:// URL.' });
  const hl = draft.actions?.highlight;
  if (hl && !/^#(?:[0-9a-f]{3}|[0-9a-f]{6})$/i.test(hl)) out.push({ path: 'highlight', msg: 'A #rrggbb colour.' });
  let nodes = 0;
  const walk = (c, depth) => {
    nodes++;
    const id = idOf ? idOf(c) : '';
    const bad = (msg) => out.push({ path: 'cond:' + id, msg });
    if (depth > 8) { bad('Nested deeper than 8 levels.'); return; }
    if (isGroup(c)) {
      if (!c.conds.length) bad(depth === 1 ? 'Add at least one condition.' : 'This group is empty.');
      c.conds.forEach((x) => walk(x, depth + 1));
      return;
    }
    const d = condDef(c.type);
    if (!d) { bad(`Unknown condition "${c.type}".`); return; }
    if (d.kind === 'values' && !(c.values || []).length) bad('Enter at least one value.');
    if (d.kind === 'choice' && d.required && !(c.values || []).length) bad('Pick at least one.');
    if (d.kind === 'values' && c.type === 'icao') {
      for (const v of c.values || []) {
        const u = v.toUpperCase().replace(/^~/, '');
        if (!(/^[0-9A-F*?]{1,6}$/.test(u) || /^[0-9A-F]{6}-[0-9A-F]{6}$/.test(u))) { bad(`"${v}" is not a hex address, pattern or range.`); break; }
      }
    }
    if (d.kind === 'choice' && c.type === 'watchlist') {
      for (const v of c.values || []) if (!/^[A-Za-z0-9 _.-]{1,40}$/.test(v)) { bad(`"${v}" is not a valid list name.`); break; }
    }
    if (d.kind === 'range') {
      if (!fin(c.min) && !fin(c.max)) bad('Enter a minimum and/or a maximum.');
      else if (fin(c.min) && fin(c.max) && c.min > c.max) bad('The minimum is above the maximum.');
      else if ((fin(c.min) && (c.min < d.lo || c.min > d.hi)) || (fin(c.max) && (c.max < d.lo || c.max > d.hi))) bad('Outside the allowed range.');
    }
    if (d.kind === 'bearing' && (!fin(c.from) || !fin(c.to) || c.from < 0 || c.from > 360 || c.to < 0 || c.to > 360)) bad('From and to between 0 and 360°.');
    if (d.kind === 'geofence') {
      if (c.shape === 'polygon') {
        if (!Array.isArray(c.polygon) || c.polygon.length < 3) bad('Draw at least 3 points on the map.');
      } else if (!fin(c.lat) || !fin(c.lon) || !fin(c.radiusKm) || c.radiusKm <= 0 || c.radiusKm > 5000 || Math.abs(c.lat) > 90 || Math.abs(c.lon) > 180) {
        bad('Set a centre and a radius (up to 5000 km).');
      }
    }
  };
  walk(draft.match, 1);
  if (nodes > 200) out.push({ path: 'match', msg: 'At most 200 conditions.' });
  return out;
}

/** Prepare a stored geofence leaf for the editor (shape flag). */
export function prepareTree(c) {
  if (isGroup(c)) { c.conds.forEach(prepareTree); return c; }
  if (c.type === 'geofence') {
    c.shape = Array.isArray(c.polygon) && c.polygon.length ? 'polygon' : 'circle';
    if (c.inside === undefined) c.inside = true;
  }
  if (condDef(c.type)?.kind === 'bool' && c.value === undefined) c.value = true;
  return c;
}

const list = (vals, n = 4) => {
  const v = vals || [];
  return v.length <= n ? v.join(', ') : v.slice(0, n).join(', ') + ` +${v.length - n}`;
};
const labelOf = (choices, v) => choices?.find((c) => c.value === v)?.label || v;

/** One-line, human description of a condition tree (numbers through units). */
export function describe(c, units, depth = 0) {
  if (!c) return '—';
  const neg = c.negate ? 'NOT ' : '';
  if (isGroup(c)) {
    if (!c.conds.length) return neg + '(nothing)';
    const parts = c.conds.map((x) => describe(x, units, depth + 1));
    const s = parts.join(c.op === 'or' ? ' or ' : ' and ');
    return neg ? `${neg}(${s})` : depth > 0 && c.conds.length > 1 ? `(${s})` : s;
  }
  const d = condDef(c.type);
  if (!d) return neg + c.type;
  const L = d.label.toLowerCase();
  const fmtR = (v) => {
    if (d.unit && units && ['alt', 'speed', 'vrate', 'dist'].includes(d.unit)) return units.format(d.unit, v);
    return units ? units.num(v, d.step && d.step < 1 ? 1 : 0) + (d.unit ? ' ' + d.unit : '') : String(v);
  };
  switch (d.kind) {
    case 'values': return `${neg}${L} ${list(c.values)}`;
    case 'choice':
      if (!(c.values || []).length) return c.type === 'watchlist' ? `${neg}on any watch-list` : c.type === 'emergency' ? `${neg}any emergency` : `${neg}${L}`;
      return `${neg}${c.type === 'watchlist' ? 'on watch-list' : L} ${list((c.values || []).map((v) => labelOf(d.choices, v)))}`;
    case 'bool': return neg + (c.value === false ? d.no : d.yes);
    case 'range': {
      const lo = fin(c.min); const hi = fin(c.max);
      if (lo && hi) return `${neg}${L} ${fmtR(c.min)} – ${fmtR(c.max)}`;
      if (lo) return `${neg}${L} ≥ ${fmtR(c.min)}`;
      if (hi) return `${neg}${L} ≤ ${fmtR(c.max)}`;
      return neg + L;
    }
    case 'bearing': return `${neg}bearing ${units ? units.deg(c.from) : c.from} → ${units ? units.deg(c.to) : c.to}`;
    case 'geofence': {
      const where = c.inside === false ? 'outside' : 'inside';
      if (Array.isArray(c.polygon) && c.polygon.length && c.shape !== 'circle') return `${neg}${where} polygon (${c.polygon.length} points)`;
      if (!fin(c.lat) || !fin(c.lon)) return `${neg}${where} a polygon (not drawn yet)`;
      return `${neg}${where} ${units ? units.dist(c.radiusKm) : c.radiusKm + ' km'} of ${units ? units.latlon(c.lat, c.lon, 2) : c.lat + ',' + c.lon}`;
    }
    default: return neg + L;
  }
}

/** True when the rule reacts to system events rather than aircraft. */
export function isSystemRule(match) {
  let sys = false;
  const walk = (c) => { if (!c) return; if (isGroup(c)) c.conds.forEach(walk); else if (c.type === 'system') sys = true; };
  walk(match);
  return sys;
}

/** Split typed text into values (comma, semicolon or newline separated). */
export const splitValues = (text) => String(text || '').split(/[,;\n]+/).map((s) => s.trim()).filter(Boolean);

/** Parse a "hex list with notes" paste / CSV into entries (client preview only; the server parses again). */
export function parseWatchText(text) {
  const ok = [];
  const bad = [];
  for (const raw of String(text || '').split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const m = line.match(/^"?([0-9A-Fa-f]{6})"?\s*(?:[,;\t ]\s*(.*))?$/);
    if (m) ok.push({ icao: m[1].toUpperCase(), note: (m[2] || '').replace(/^"|"$/g, '').trim() });
    else if (!/^icao\b/i.test(line)) bad.push(line);
  }
  return { ok, bad };
}
