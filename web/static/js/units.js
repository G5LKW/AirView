// units.js - unit systems and number/time formatting.
//
// The API always speaks base units: altitude ft, speed kt, vertical rate ft/min, distance km,
// angles deg, signal dBFS, timestamps Unix ms, weather degC / hPa / km/h / mm, heights m.
// Everything shown to the user goes through this module so the three display systems stay
// consistent:   aviation (ft, kt, nm)  |  metric (m, km/h, km)  |  imperial (ft, mph, mi)
//
//   units.alt(35000)        -> "35,000 ft" | "10,668 m"
//   units.dist(185.2)       -> "100 nm" | "185 km" | "115 mi"
//   units.area(1000)        -> "292 nm²" | "1,000 km²" | "386 mi²"   (label('area') -> "nm²")
//   units.value('dist', km) -> number in the user's unit;  units.label('dist') -> "nm"
//   units.toBase('dist', v) -> km (for form inputs typed in the user's unit)
//   units.onChange(fn)      -> re-render when the user switches system
//
// The choice is stored per browser in localStorage; until the user picks one, the server
// default (config.ui.units) applies via units.setDefault().

const KEY = 'airview.units';
export const DASH = '—';
const NBSP = ' ';
const MINUS = '−';

const SYSTEMS = ['aviation', 'metric', 'imperial'];
const SYSTEM_LABELS = { aviation: 'Aviation (ft, kt, nm)', metric: 'Metric (m, km/h, km)', imperial: 'Imperial (ft, mph, mi)' };
const SYSTEM_SHORT = { aviation: 'ft · kt · nm', metric: 'm · km/h · km', imperial: 'ft · mph · mi' };

// kind -> per-system [unit label, factor from base, default fraction digits, offset]
const lin = (label, k, digits = 0, off = 0) => ({ label, k, digits, off });
const Q = {
  alt: { aviation: lin('ft', 1), metric: lin('m', 0.3048), imperial: lin('ft', 1) },
  speed: { aviation: lin('kt', 1), metric: lin('km/h', 1.852), imperial: lin('mph', 1.150779) },
  dist: { aviation: lin('nm', 1 / 1.852, 1), metric: lin('km', 1, 1), imperial: lin('mi', 0.621371, 1) },
  vrate: { aviation: lin('ft/min', 1), metric: lin('m/s', 0.00508, 1), imperial: lin('ft/min', 1) },
  temp: { aviation: lin('°C', 1, 1), metric: lin('°C', 1, 1), imperial: lin('°F', 1.8, 1, 32) },
  pressure: { aviation: lin('hPa', 1, 1), metric: lin('hPa', 1, 1), imperial: lin('inHg', 0.0295299875, 2) },
  wind: { aviation: lin('kt', 1 / 1.852), metric: lin('km/h', 1), imperial: lin('mph', 0.621371) }, // base km/h (weather API)
  precip: { aviation: lin('mm', 1, 1), metric: lin('mm', 1, 1), imperial: lin('in', 1 / 25.4, 2) },
  height: { aviation: lin('m', 1, 1), metric: lin('m', 1, 1), imperial: lin('ft', 3.28084, 0) }, // base m (antenna, terrain)
  visibility: { aviation: lin('km', 0.001, 1), metric: lin('km', 0.001, 1), imperial: lin('mi', 0.000621371, 1) }, // base m
  // base km², follows the distance unit (nm² | km² | mi²)
  area: { aviation: lin('nm²', 1 / (1.852 * 1.852)), metric: lin('km²', 1), imperial: lin('mi²', 0.621371 * 0.621371) },
};

let stored = null;
try { stored = localStorage.getItem(KEY); } catch { /* storage unavailable */ }
let system = SYSTEMS.includes(stored) ? stored : 'aviation';
let userChosen = SYSTEMS.includes(stored);
const subs = new Set();

const nfCache = new Map();
function nf(min, max) {
  const k = min * 16 + max;
  let f = nfCache.get(k);
  if (!f) { f = new Intl.NumberFormat('en-US', { minimumFractionDigits: min, maximumFractionDigits: max }); nfCache.set(k, f); }
  return f;
}
const ok = (v) => typeof v === 'number' && Number.isFinite(v);
const pad2 = (n) => (n < 10 ? '0' + n : '' + n);

/** Format a number with thousands separators. Missing -> "—". */
function num(v, digits = 0, minDigits = digits) {
  if (!ok(v)) return DASH;
  const s = nf(Math.min(minDigits, digits), digits).format(v);
  return s === '-0' ? '0' : s.replace('-', MINUS);
}

function def(kind) {
  const q = Q[kind];
  if (!q) throw new Error(`units: unknown quantity "${kind}"`);
  return q[system];
}

/** Convert a base-unit value to the user's unit (number, or null when missing). */
function value(kind, v) {
  if (!ok(v)) return null;
  const d = def(kind);
  return v * d.k + d.off;
}
/** Convert a value in the user's unit back to the API base unit. */
function toBase(kind, v) {
  if (!ok(v)) return null;
  const d = def(kind);
  return (v - d.off) / d.k;
}
/** Unit label for a quantity in the current system, e.g. label('dist') -> "nm". */
function label(kind) { return def(kind).label; }

/**
 * Format a base-unit value with its unit.
 * @param {string} kind alt|speed|dist|vrate|temp|pressure|wind|precip|height|visibility|area
 * @param {number|null|undefined} v base-unit value
 * @param {{digits?: number, unit?: boolean, sign?: boolean}} [o] unit=false omits the label
 */
function format(kind, v, o = {}) {
  if (!ok(v)) return DASH;
  const d = def(kind);
  const x = v * d.k + d.off;
  let s = num(x, o.digits ?? d.digits);
  if (o.sign && x > 0) s = '+' + s;
  return o.unit === false ? s : s + NBSP + d.label;
}

function set(next, { silent = false } = {}) {
  if (!SYSTEMS.includes(next)) return;
  userChosen = true;
  try { localStorage.setItem(KEY, next); } catch { /* ignore */ }
  if (next === system) return;
  system = next;
  if (!silent) for (const fn of subs) { try { fn(system); } catch (e) { console.error('[units] listener failed', e); } }
}

const CARD = ['N', 'NNE', 'NE', 'ENE', 'E', 'ESE', 'SE', 'SSE', 'S', 'SSW', 'SW', 'WSW', 'W', 'WNW', 'NW', 'NNW'];

export const units = {
  DASH,
  systems: SYSTEMS,
  systemLabels: SYSTEM_LABELS,
  systemShort: SYSTEM_SHORT,
  /** Current system: 'aviation' | 'metric' | 'imperial'. */
  get system() { return system; },
  /** Switch system (persists in localStorage, notifies subscribers). */
  set,
  /** Advance to the next system; returns the new one. */
  cycle() { set(SYSTEMS[(SYSTEMS.indexOf(system) + 1) % SYSTEMS.length]); return system; },
  /** Apply the server default (config.ui.units) unless this browser already chose a system. */
  setDefault(sys) {
    if (userChosen || !SYSTEMS.includes(sys) || sys === system) return;
    system = sys;
    for (const fn of subs) { try { fn(system); } catch (e) { console.error('[units] listener failed', e); } }
  },
  /** Subscribe to system changes. Returns an unsubscribe function. */
  onChange(fn) { subs.add(fn); return () => subs.delete(fn); },

  value, toBase, label, format, num,

  // ---- quantity shorthands (base unit in, formatted string out) ----
  /** Altitude from feet. opts.fl: show flight level at/above 18,000 ft in aviation mode. */
  alt(ft, o = {}) {
    if (!ok(ft)) return DASH;
    if (o.fl && system === 'aviation' && ft >= 18000) return units.fl(ft);
    return format('alt', ft, o);
  },
  /** Speed from knots. */
  speed: (kt, o) => format('speed', kt, o),
  /** Area from km², in the square of the distance unit: "12,345 km²" | "3,599 nm²" | "4,766 mi²" (1 decimal below 10). */
  area(km2, o = {}) {
    const x = value('area', km2);
    return format('area', km2, { digits: x !== null && Math.abs(x) < 10 ? 1 : 0, ...o });
  },
  /** Distance from kilometres (1 decimal below 100 units, none above unless digits given). */
  dist(km, o = {}) {
    if (!ok(km)) return DASH;
    const x = value('dist', km);
    return format('dist', km, { digits: o.digits ?? (Math.abs(x) >= 100 ? 0 : 1), ...o });
  },
  /** Vertical rate from ft/min, signed. */
  vrate: (fpm, o) => format('vrate', fpm, { sign: true, ...o }),
  /** Temperature from degC. */
  temp: (c, o) => format('temp', c, o),
  /** Pressure from hPa. */
  pressure: (hpa, o) => format('pressure', hpa, o),
  /** Wind speed from km/h (weather data). Aircraft-derived wind in kt: use units.speed(). */
  wind: (kmh, o) => format('wind', kmh, o),
  /** Precipitation from mm. */
  precip: (mm, o) => format('precip', mm, o),
  /** Height (antenna, terrain) from metres. */
  height: (m, o) => format('height', m, o),
  /** Visibility from metres. */
  visibility: (m, o) => format('visibility', m, o),
  /** Flight level from feet: 35000 -> "FL350". */
  fl(ft) { return ok(ft) ? 'FL' + String(Math.max(0, Math.round(ft / 100))).padStart(3, '0') : DASH; },

  // ---- unit-less formatting ----
  /** Integer with separators. */
  int: (v) => num(v, 0),
  /** Compact count: 1234 -> "1.2k", 3400000 -> "3.4M". */
  si(v, digits = 1) {
    if (!ok(v)) return DASH;
    const a = Math.abs(v);
    if (a >= 1e9) return num(v / 1e9, digits) + 'G';
    if (a >= 1e6) return num(v / 1e6, digits) + 'M';
    if (a >= 1e4) return num(v / 1e3, digits) + 'k';
    return num(v, 0);
  },
  /** Percentage from a 0..100 value. */
  pct: (v, digits = 0) => (ok(v) ? num(v, digits) + '%' : DASH),
  /** Angle: 273.4 -> "273°". */
  deg: (v, digits = 0) => (ok(v) ? num(((v % 360) + 360) % 360, digits) + '°' : DASH),
  /** 16-point compass name for a bearing. */
  cardinal: (v) => (ok(v) ? CARD[Math.round((((v % 360) + 360) % 360) / 22.5) % 16] : ''),
  /** Bearing with compass point: "273° W". */
  bearing: (v) => (ok(v) ? units.deg(v) + NBSP + units.cardinal(v) : DASH),
  /** Signal level: "-12.3 dBFS". */
  dbfs: (v, o = {}) => (ok(v) ? num(v, o.digits ?? 1, o.digits ?? 1) + (o.unit === false ? '' : NBSP + 'dBFS') : DASH),
  /** dB quantity. */
  db: (v, digits = 1) => (ok(v) ? num(v, digits, digits) + NBSP + 'dB' : DASH),
  /** Message / frame rate: "1,842/s". */
  rate: (v, digits) => (ok(v) ? num(v, digits ?? (v < 10 ? 1 : 0)) + '/s' : DASH),
  /** Latitude / longitude pair: "51.4775°N 0.4614°W". */
  latlon(lat, lon, digits = 4) {
    if (!ok(lat) || !ok(lon)) return DASH;
    return `${Math.abs(lat).toFixed(digits)}°${lat >= 0 ? 'N' : 'S'} ${Math.abs(lon).toFixed(digits)}°${lon >= 0 ? 'E' : 'W'}`;
  },
  /** Byte size: "12.4 MB". */
  bytes(n) {
    if (!ok(n)) return DASH;
    const u = ['B', 'kB', 'MB', 'GB', 'TB'];
    let i = 0;
    let x = n;
    while (x >= 1024 && i < u.length - 1) { x /= 1024; i++; }
    return num(x, i === 0 || x >= 100 ? 0 : 1) + NBSP + u[i];
  },
  /** Short age from seconds: "3s", "2m 10s", "1h 04m", "3d 2h". */
  age(sec) {
    if (!ok(sec)) return DASH;
    const s = Math.max(0, Math.round(sec));
    if (s < 60) return s + 's';
    if (s < 3600) return Math.floor(s / 60) + 'm ' + pad2(s % 60) + 's';
    if (s < 86400) return Math.floor(s / 3600) + 'h ' + pad2(Math.floor((s % 3600) / 60)) + 'm';
    return Math.floor(s / 86400) + 'd ' + Math.floor((s % 86400) / 3600) + 'h';
  },
  /** Duration from milliseconds (same style as age). */
  duration: (ms) => (ok(ms) ? units.age(ms / 1000) : DASH),
  /** "5 min ago" style relative time from a Unix-ms timestamp. */
  ago(ms, now = Date.now()) {
    if (!ok(ms) || ms <= 0) return DASH;
    const s = Math.round((now - ms) / 1000);
    if (s < 5) return 'just now';
    if (s < 60) return s + ' s ago';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 86400) return Math.floor(s / 3600) + ' h ago';
    return Math.floor(s / 86400) + ' d ago';
  },
  /** Clock time from Unix ms. opts: { utc = true, seconds = true, z = true } -> "23:41:07Z". */
  clock(ms, o = {}) {
    if (!ok(ms) || ms <= 0) return DASH;
    const d = new Date(ms);
    const utc = o.utc !== false;
    const h = utc ? d.getUTCHours() : d.getHours();
    const m = utc ? d.getUTCMinutes() : d.getMinutes();
    const s = utc ? d.getUTCSeconds() : d.getSeconds();
    return pad2(h) + ':' + pad2(m) + (o.seconds === false ? '' : ':' + pad2(s)) + (utc && o.z !== false ? 'Z' : '');
  },
  /** ISO date from Unix ms: "2026-11-11" (UTC unless utc:false). */
  date(ms, o = {}) {
    if (!ok(ms) || ms <= 0) return DASH;
    const d = new Date(ms);
    return o.utc === false
      ? `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`
      : `${d.getUTCFullYear()}-${pad2(d.getUTCMonth() + 1)}-${pad2(d.getUTCDate())}`;
  },
  /** "2026-11-11 23:41Z". */
  dateTime(ms, o = {}) {
    if (!ok(ms) || ms <= 0) return DASH;
    return units.date(ms, o) + ' ' + units.clock(ms, { seconds: false, ...o });
  },
  /** All-time record in display units: ('max_dist_km', 355.3) -> "farthest position 191.8 nm"; null for unknown keys. */
  recordText(key, value) {
    const f = RECORD_FMT[key];
    return f && ok(value) ? f[0] + ' ' + f[1](value) : null;
  },
  /**
   * Text of a live notice in the display units. Record notices (kind "record", key, value) are rebuilt from their
   * fields; the server text (fixed km / ft / kt / s) is the fallback.
   */
  noticeText(n) {
    const text = (n && n.text) || '';
    if (n && n.kind === 'record') {
      const r = units.recordText(n.key, n.value);
      if (r) return 'New record: ' + r + whoSuffix(text);
    }
    return text;
  },
  /**
   * Text of an alerts.Alert in the display units. Only alerts whose text came from the default template
   * (`auto`) are rebuilt from the structured fields (same layout as the server's template); a rule's own
   * message is shown as the server expanded it.
   */
  alertText(al) {
    const text = (al && al.text) || '';
    if (!al || !al.auto) return text;
    if (al.kind === 'system') {
      if (al.event === 'record') {
        const r = units.recordText(al.eventKey, al.value);
        if (r) return `${al.ruleName}: ${r}${whoSuffix(text)}`;
      }
      return text;
    }
    let s = `${al.ruleName}: ${al.callsign || al.reg || al.icao || '?'}`;
    if (al.type) s += ` (${al.type})`;
    if (al.icao) s += ` [${al.icao}]`;
    if (al.squawk) s += ` sq ${al.squawk}`;
    if (ok(al.altFt)) s += ' ' + units.alt(al.altFt);
    if (ok(al.distKm)) s += ' ' + units.dist(al.distKm);
    return s;
  },
};

/** Labels and formatters of the tracker's record keys (internal/tracker/flights.go). */
const RECORD_FMT = {
  max_dist_km: ['farthest position', (v) => units.dist(v)],
  max_alt_ft: ['highest aircraft', (v) => units.alt(v)],
  max_gs_kt: ['fastest aircraft', (v) => units.speed(v)],
  min_rssi_pos_dbfs: ['weakest decoded position', (v) => units.dbfs(v)],
  max_aircraft: ['most aircraft at once', (v) => units.int(v)],
  max_msg_rate: ['peak message rate', (v) => units.rate(v)],
  max_flight_s: ['longest tracked flight', (v) => units.age(v)],
};

/** " - RYR1928 (4CA123)" part of a server record text, or "". */
function whoSuffix(text) {
  const i = text.indexOf(' - ');
  return i >= 0 ? text.slice(i) : '';
}

export default units;
