// settings/model.js - pure logic of the Settings page (no DOM): section list, path helpers, client-side
// validation mirroring internal/config/validate.go, the diff that reveals server corrections, feed presets,
// and a port of the antenna pattern model (internal/radio/pattern.go) plus the radio horizon for the live preview.

export const SECTIONS = [
  { id: 'station', label: 'Station location', icon: 'pin', keys: ['receiver'] },
  { id: 'antennas', label: 'Antennas', icon: 'antenna', keys: ['antennas'] },
  { id: 'feeds', label: 'Receiver feeds', icon: 'signal', keys: ['feeds'] },
  { id: 'time', label: 'Time (NTP)', icon: 'timer', keys: ['ntp'] },
  { id: 'prediction', label: 'Prediction', icon: 'target', keys: ['prediction'] },
  { id: 'terrain', label: 'Terrain', icon: 'mountain', keys: ['terrain'] },
  { id: 'navdata', label: 'Aviation data', icon: 'plane', keys: ['navData'] },
  { id: 'alerts', label: 'Alerts', icon: 'bell', keys: ['alerts'] },
  { id: 'rf', label: 'RF statistics', icon: 'wave', keys: ['rfStat'] },
  { id: 'weather', label: 'Weather', icon: 'cloud', keys: ['weather'] },
  { id: 'web', label: 'Web server', icon: 'globe', keys: ['http'] },
  { id: 'connection', label: 'Connection / remote', icon: 'link', keys: ['mode', 'remote', 'hub'] },
  { id: 'uplinks', label: 'Network uplinks', icon: 'globe', keys: ['uplinks'] },
  { id: 'display', label: 'Display', icon: 'eye', keys: ['ui'] },
  { id: 'storage', label: 'Storage & database', icon: 'database', keys: ['storage'] },
  { id: 'demo', label: 'Demo mode', icon: 'play', keys: ['demo'] },
  { id: 'updates', label: 'Updates', icon: 'download', keys: ['update'] },
  { id: 'ssr', label: 'SSR self-location', icon: 'wave', keys: ['ssr'] },
  { id: 'acars', label: 'ACARS / VDL2', icon: 'list', keys: ['acars'] },
  { id: 'danger', label: 'Danger zone', icon: 'alert', keys: [] },
  { id: 'about', label: 'About & diagnostics', icon: 'info', keys: [] },
];

export const ANT_TYPES = [
  { value: 'vertical', label: 'Vertical', hint: 'Simple 1/4 or 1/2 wave omni: one broad lobe towards the horizon.' },
  { value: 'collinear', label: 'Collinear', hint: 'Stacked omni: more gain, squeezed into a flat disc around the horizon.' },
  { value: 'yagi', label: 'Yagi', hint: 'Directional beam: high gain in one direction, little behind.' },
  { value: 'patch', label: 'Patch', hint: 'Directional flat panel: a wide beam in front, shielded behind.' },
  { value: 'other', label: 'Other', hint: 'Anything else. Treated as directional when a horizontal beamwidth is entered.' },
];

export const FEED_TYPES = [
  { value: 'beast', label: 'Beast binary (TCP)', port: 10003, tcp: true },
  { value: 'avr', label: 'AVR / raw hex (TCP)', port: 10002, tcp: true },
  { value: 'sbs', label: 'SBS BaseStation (TCP)', port: 30003, tcp: true },
  { value: 'json', label: 'JSON aircraft list (HTTP polling)', port: 0, tcp: false },
  { value: 'hub', label: 'AirView data port (hub)', port: 30154, tcp: true },
  { value: 'rtltcp', label: 'RTL-SDR dongle (rtl_tcp)', port: 1234, tcp: true },
];

// Receiver-agnostic order: the common RTL-SDR decoders (readsb / dump1090) and the generic protocols first,
// the Radarcape presets after them (flag `radarcape`).
export const FEED_PRESETS = [
  { label: 'RTL-SDR: readsb / dump1090 Beast 30005', hint: 'Beast output of readsb, dump1090 or any other RTL-SDR decoder: raw frames with signal levels and timestamps. 127.0.0.1 when it runs on this machine.',
    feed: { name: 'readsb (Beast 30005)', type: 'beast', host: '127.0.0.1', port: 30005 } },
  { label: 'readsb / dump1090 SBS 30003', hint: 'BaseStation text output: decoded positions only, no signal level, no raw frames.',
    feed: { name: 'readsb (SBS 30003)', type: 'sbs', host: '127.0.0.1', port: 30003 } },
  { label: 'readsb / dump1090 aircraft.json', hint: 'Polls the aircraft list (aircraft.json) of the decoder\'s web server once per second.',
    feed: { name: 'readsb (aircraft.json)', type: 'json', url: 'http://127.0.0.1:8080/data/aircraft.json' } },
  { label: 'RTL-SDR dongle via rtl_tcp 1234', hint: 'Raw I/Q samples from rtl_tcp; AirView decodes them itself.',
    feed: { name: 'RTL-SDR (rtl_tcp)', type: 'rtltcp', host: '127.0.0.1', port: 1234 } },
  { label: 'Radarcape - Beast 10003', radarcape: true, hint: 'GPS-timestamped Beast binary: the best Radarcape input, includes signal levels and MLAT timing.',
    feed: { name: 'Radarcape (Beast 10003)', type: 'beast', host: 'radarcape', port: 10003 } },
  { label: 'Radarcape SBS 30003', radarcape: true, hint: 'BaseStation text: decoded positions only, no signal level, no raw frames.',
    feed: { name: 'Radarcape (SBS 30003)', type: 'sbs', host: 'radarcape', port: 30003 } },
  { label: 'Radarcape JSON aircraft list', radarcape: true, hint: 'Polls the aircraft list of the Radarcape web server once per second.',
    feed: { name: 'Radarcape (JSON aircraft list)', type: 'json', url: 'http://radarcape/aircraftlist.json' } },
  { label: 'Empty feed', hint: 'Start from scratch.', feed: { name: 'New feed', type: 'beast', host: '', port: 30005 } },
];

export const SECRET_PATHS = ['http.adminPasswordHash', 'hub.password', 'remote.password'];
export const MAX_ANTENNAS = 16;
export const MAX_FEEDS = 32;
export const MAX_RINGS = 12;
export const ANT_PALETTE = ['#38bdf8', '#f59e0b', '#34d399', '#f472b6', '#a78bfa', '#fb7185', '#facc15', '#22d3ee'];

// ---------------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------------

export const clone = (o) => (typeof structuredClone === 'function' ? structuredClone(o) : JSON.parse(JSON.stringify(o)));

export function getPath(obj, path) {
  let o = obj;
  for (const k of String(path).split('.')) {
    if (o === null || o === undefined) return undefined;
    o = o[k];
  }
  return o;
}

export function setPath(obj, path, value) {
  const keys = String(path).split('.');
  let o = obj;
  for (let i = 0; i < keys.length - 1; i++) {
    if (o[keys[i]] === null || typeof o[keys[i]] !== 'object') o[keys[i]] = {};
    o = o[keys[i]];
  }
  o[keys[keys.length - 1]] = value;
}

/** Same rules as config.Slug: lower-case [a-z0-9_-], runs of other characters become '-', max 32 chars. */
export function slug(s) {
  let out = '';
  let lastDash = true;
  for (const ch of String(s ?? '').toLowerCase()) {
    if ((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch === '_') { out += ch; lastDash = false; }
    else if (!lastDash) { out += '-'; lastDash = true; }
    if (out.length >= 32) break;
  }
  return out.replace(/^[-_]+|[-_]+$/g, '');
}

/** A slug that is not in `used` (appends -2, -3 ...). */
export function uniqueId(base, used, fallback) {
  let s = slug(base) || fallback;
  const root = s;
  for (let n = 2; used.includes(s); n++) {
    const suffix = '-' + n;
    s = root.slice(0, 32 - suffix.length).replace(/[-_]+$/, '') + suffix;
  }
  return s;
}

const isNum = (v) => typeof v === 'number' && Number.isFinite(v);

/** Mirrors config.splitHost: accepts host, host:port, [v6]:port, URLs; returns { ok, host, port }. */
export function splitHost(input) {
  let s = String(input ?? '').trim();
  let i = s.indexOf('://');
  if (i >= 0) s = s.slice(i + 3);
  i = s.search(/[/?#]/);
  if (i >= 0) s = s.slice(0, i);
  i = s.lastIndexOf('@');
  if (i >= 0) s = s.slice(i + 1);
  if (s === '') return { ok: true, host: '', port: 0 };
  if (s.length > 261) return { ok: false };
  let host = s;
  let portStr = '';
  if (s.startsWith('[')) {
    const end = s.indexOf(']');
    if (end < 0) return { ok: false };
    host = s.slice(1, end);
    const rest = s.slice(end + 1);
    if (rest) { if (rest[0] !== ':') return { ok: false }; portStr = rest.slice(1); }
  } else if (s.split(':').length === 2) {
    [host, portStr] = s.split(':');
  }
  let port = 0;
  if (portStr !== '') {
    if (!/^\d+$/.test(portStr)) return { ok: false };
    port = Number(portStr);
    if (port < 1 || port > 65535) return { ok: false };
  }
  if (!host || host.length > 253) return { ok: false };
  if (host.includes(':')) return /^[0-9a-fA-F:.]+(%[\w.-]+)?$/.test(host) ? { ok: true, host, port } : { ok: false };
  if (!/^[\p{L}\p{N}\p{M}._-]+$/u.test(host)) return { ok: false };
  return { ok: true, host, port };
}

const IPV4 = /^(25[0-5]|2[0-4]\d|1?\d?\d)(\.(25[0-5]|2[0-4]\d|1?\d?\d)){3}$/;
export function isIP(s) {
  return IPV4.test(s) || (s.includes(':') && /^[0-9a-fA-F:.]+$/.test(s));
}

export function isHexColor(s) {
  return /^#([0-9a-f]{3}|[0-9a-f]{6}|[0-9a-f]{8})$/i.test(String(s ?? '').trim());
}

/** What a secret field of the draft means compared with the stored state. */
export function secretState(savedValue, draftValue) {
  const stored = savedValue === 'set';
  if (draftValue === 'clear') return { stored, pending: stored ? 'clear' : null };
  if (typeof draftValue === 'string' && draftValue.startsWith('plain:') && draftValue.length > 6) return { stored, pending: 'set' };
  return { stored, pending: null };
}

// ---------------------------------------------------------------------------------------------
// validation (mirrors internal/config/validate.go; the server corrects silently, the UI refuses to send)
// ---------------------------------------------------------------------------------------------

/**
 * @returns {Array<{path: string, msg: string, section: string}>} problems that block saving
 */
export function validate(cfg) {
  const out = [];
  const add = (section, path, msg) => out.push({ section, path, msg });
  const range = (section, path, lo, hi, what, o = {}) => {
    const v = getPath(cfg, path);
    if (v === undefined && o.optional) return;
    if (!isNum(v)) return add(section, path, 'Enter a number.');
    if (o.int && !Number.isInteger(v)) return add(section, path, 'Enter a whole number.');
    if (v < lo || v > hi) add(section, path, `${what || 'Value'} must be between ${lo} and ${hi}${o.unit ? ' ' + o.unit : ''}.`);
  };
  const text = (section, path, max) => {
    const v = getPath(cfg, path);
    if (typeof v === 'string' && [...v.trim()].length > max) add(section, path, `At most ${max} characters.`);
  };
  const port = (section, path) => range(section, path, 1, 65535, 'Port', { int: true });
  const oneOf = (section, path, list) => { if (!list.includes(getPath(cfg, path))) add(section, path, 'Choose one of the options.'); };

  // web server
  port('web', 'http.port');
  const bind = String(cfg.http?.bindAddr ?? '').trim();
  if (bind && bind !== '*' && bind.toLowerCase() !== 'localhost' && !isIP(bind)) add('web', 'http.bindAddr', 'Enter an IP address of this computer, or leave empty for all interfaces.');
  const adm = cfg.http?.adminPasswordHash ?? '';
  if (adm.startsWith('plain:') && adm.length - 6 > 256) add('web', 'http.adminPasswordHash', 'At most 256 characters.');

  // station
  text('station', 'receiver.name', 80);
  text('station', 'receiver.model', 80);
  range('station', 'receiver.lat', -90, 90, 'Latitude', { unit: 'degrees' });
  range('station', 'receiver.lon', -180, 180, 'Longitude', { unit: 'degrees' });
  range('station', 'receiver.groundElevM', -500, 9000, 'Ground elevation', { unit: 'm' });

  // antennas
  const ants = Array.isArray(cfg.antennas) ? cfg.antennas : [];
  if (ants.length > MAX_ANTENNAS) add('antennas', 'antennas', `At most ${MAX_ANTENNAS} antennas.`);
  const antIds = new Map();
  ants.forEach((a, i) => {
    const p = `antennas.${i}.`;
    const s = 'antennas';
    text(s, p + 'name', 80);
    if (!String(a.name ?? '').trim()) add(s, p + 'name', 'Give the antenna a name.');
    const id = String(a.id ?? '');
    if (!id) add(s, p + 'id', 'An ID is required.');
    else if (slug(id) !== id) add(s, p + 'id', 'Lower-case letters, digits, "-" and "_" only (max 32).');
    else if (antIds.has(id)) add(s, p + 'id', `Already used by antenna "${ants[antIds.get(id)].name}".`);
    else antIds.set(id, i);
    oneOf(s, p + 'type', ANT_TYPES.map((t) => t.value));
    range(s, p + 'channel', -1, 15, 'Channel', { int: true });
    range(s, p + 'gainDbi', -20, 40, 'Gain', { unit: 'dBi' });
    range(s, p + 'heightAglM', 0, 1000, 'Height', { unit: 'm' });
    range(s, p + 'azimuthDeg', 0, 360, 'Azimuth', { unit: 'degrees' });
    range(s, p + 'tiltDeg', -90, 90, 'Tilt', { unit: 'degrees' });
    range(s, p + 'hBeamwidthDeg', 0, 360, 'Beamwidth', { unit: 'degrees' });
    range(s, p + 'vBeamwidthDeg', 0, 180, 'Beamwidth', { unit: 'degrees' });
    range(s, p + 'frontToBackDb', 0, 60, 'Front-to-back ratio', { unit: 'dB' });
    range(s, p + 'elements', 0, 100, 'Element count', { int: true });
    range(s, p + 'cableLossDb', 0, 60, 'Loss', { unit: 'dB' });
    range(s, p + 'lnaGainDb', 0, 60, 'Gain', { unit: 'dB' });
    range(s, p + 'lnaNoiseFigDb', 0, 30, 'Noise figure', { unit: 'dB' });
    range(s, p + 'filterLossDb', 0, 30, 'Loss', { unit: 'dB' });
    oneOf(s, p + 'polarization', ['vertical', 'horizontal']);
    text(s, p + 'notes', 2000);
    if (!isHexColor(a.color)) add(s, p + 'color', 'Use a hex colour such as #38bdf8.');
  });

  // feeds
  const feeds = Array.isArray(cfg.feeds) ? cfg.feeds : [];
  if (feeds.length > MAX_FEEDS) add('feeds', 'feeds', `At most ${MAX_FEEDS} feeds.`);
  const feedIds = new Map();
  feeds.forEach((f, i) => {
    const p = `feeds.${i}.`;
    const s = 'feeds';
    text(s, p + 'name', 80);
    if (!String(f.name ?? '').trim()) add(s, p + 'name', 'Give the feed a name.');
    const id = String(f.id ?? '');
    if (!id) add(s, p + 'id', 'An ID is required.');
    else if (slug(id) !== id) add(s, p + 'id', 'Lower-case letters, digits, "-" and "_" only (max 32).');
    else if (feedIds.has(id)) add(s, p + 'id', `Already used by feed "${feeds[feedIds.get(id)].name}".`);
    else feedIds.set(id, i);
    const ft = FEED_TYPES.find((t) => t.value === f.type) || (f.type === 'demo' ? { value: 'demo', tcp: false } : null);
    if (!ft) add(s, p + 'type', 'Choose a feed type.');
    const hp = splitHost(f.host);
    if (!hp.ok) add(s, p + 'host', 'Not a valid host name or IP address.');
    if (ft?.tcp) {
      if (hp.ok && !hp.host && f.enabled) add(s, p + 'host', 'Enter the host name or IP address of the receiver.');
      port(s, p + 'port');
    } else if (isNum(f.port) && (f.port < 0 || f.port > 65535)) add(s, p + 'port', 'Port must be between 0 and 65535.');
    const url = String(f.url ?? '').trim();
    if (f.type === 'json') {
      if (!url) { if (f.enabled) add(s, p + 'url', 'Enter the URL of the aircraft list (http://host/path).'); }
      else if (!validFeedURL(url)) add(s, p + 'url', 'Enter an http:// or https:// URL without spaces.');
    } else if (url && !validFeedURL(url)) add(s, p + 'url', 'Enter an http:// or https:// URL without spaces.');
    if (f.antennaId && !ants.some((a) => a.id === f.antennaId)) add(s, p + 'antennaId', 'This antenna no longer exists.');
    text(s, p + 'notes', 2000);
  });

  // NTP
  const ntpHost = splitHost(cfg.ntp?.server);
  if (!ntpHost.ok) add('time', 'ntp.server', 'Not a valid host name or address (host or host:port).');
  else if (cfg.ntp?.enabled && !ntpHost.host) add('time', 'ntp.server', 'Enter the address of your NTP server, or switch NTP off.');
  range('time', 'ntp.intervalSec', 2, 86400, 'Interval', { int: true, unit: 's' });

  // weather
  range('weather', 'weather.intervalMin', 5, 1440, 'Interval', { int: true, unit: 'min' });

  // hub + remote
  port('connection', 'hub.port');
  oneOf('connection', 'mode', ['standalone', 'remote-data', 'remote-web']);
  const rh = splitHost(cfg.remote?.host);
  if (!rh.ok) add('connection', 'remote.host', 'Not a valid host name or IP address.');
  else if (cfg.mode !== 'standalone' && !rh.host) add('connection', 'remote.host', 'This mode needs the address of the AirView host.');
  port('connection', 'remote.dataPort');
  port('connection', 'remote.httpPort');
  for (const p of ['hub.password', 'remote.password']) {
    const v = String(getPath(cfg, p) ?? '');
    if (v.replace(/^plain:/, '').length > 256) add('connection', p, 'At most 256 characters.');
  }

  // storage
  range('storage', 'storage.trailIntervalSec', 1, 3600, 'Interval', { int: true, unit: 's' });
  range('storage', 'storage.trailRetentionDays', 1, 3650, 'Retention', { int: true, unit: 'days' });
  range('storage', 'storage.flightRetentionDays', 0, 36500, 'Retention', { int: true, unit: 'days' });
  range('storage', 'storage.minuteRetentionDays', 1, 3650, 'Retention', { int: true, unit: 'days' });

  // display
  oneOf('display', 'ui.units', ['aviation', 'metric', 'imperial']);
  range('display', 'ui.trailMinutes', 1, 1440, 'Trail length', { int: true, unit: 'min' });
  range('display', 'ui.aircraftTtlS', 5, 3600, 'Timeout', { int: true, unit: 's' });
  oneOf('display', 'ui.colorTrailsBy', ['altitude', 'speed', 'signal', 'source']);
  const rings = cfg.ui?.rangeRingsKm;
  if (!Array.isArray(rings)) add('display', 'ui.rangeRingsKm', 'Range rings are missing.');
  else {
    if (rings.length > MAX_RINGS) add('display', 'ui.rangeRingsKm', `At most ${MAX_RINGS} rings.`);
    if (rings.some((r) => !Number.isInteger(r) || r < 1 || r > 5000)) add('display', 'ui.rangeRingsKm', 'Each ring must be between 1 and 5000 km.');
    if (new Set(rings).size !== rings.length) add('display', 'ui.rangeRingsKm', 'Two rings have the same radius.');
  }

  // demo
  range('demo', 'demo.aircraft', 1, 1000, 'Aircraft count', { int: true });

  // Newer blocks: a config written by an older AirView (or an import) may lack them entirely; the server then
  // fills in the defaults, so a missing block or field is not a problem, a wrong value is.
  const bool = (section, path) => {
    const v = getPath(cfg, path);
    if (v !== undefined && typeof v !== 'boolean') add(section, path, 'Choose on or off.');
  };

  // prediction (the coast minimum stays 5 s: "prediction off" is the way to never coast)
  if (cfg.prediction) {
    bool('prediction', 'prediction.enabled');
    bool('prediction', 'prediction.showPaths');
    range('prediction', 'prediction.coastSec', 5, 120, 'Coasting time', { int: true, unit: 's', optional: true });
    range('prediction', 'prediction.pathSec', 10, 900, 'Path length', { int: true, unit: 's', optional: true });
  }

  // terrain
  if (cfg.terrain) {
    bool('terrain', 'terrain.enabled');
    bool('terrain', 'terrain.autoPrefetch');
    range('terrain', 'terrain.radiusKm', 50, 1000, 'Radius', { int: true, unit: 'km', optional: true });
    const z = cfg.terrain.zoom;
    if (z !== undefined && (!Number.isInteger(z) || (z !== 0 && (z < 6 || z > 12)))) add('terrain', 'terrain.zoom', 'Choose automatic or a zoom level between 6 and 12.');
    // 0 means "default" on the server (validate.go replaces it with the default k-factor)
    if (cfg.terrain.kFactor !== 0) range('terrain', 'terrain.kFactor', 0.5, 10, 'k-factor', { optional: true });
  }

  // aviation data
  if (cfg.navData) {
    for (const k of ['airportsOverlay', 'routesOnline', 'photos']) bool('navdata', 'navData.' + k);
    const c = cfg.navData.photoContact;
    if (c !== undefined && typeof c !== 'string') add('navdata', 'navData.photoContact', 'Enter text.');
    const ct = String(c ?? '').trim();
    if ([...ct].length > 200) add('navdata', 'navData.photoContact', 'At most 200 characters.');
    if (cfg.navData.photos && !ct) add('navdata', 'navData.photoContact', 'planespotters.net asks every client for a contact: enter an e-mail address or a website, or switch photos off.');
  }

  // alerts + RF statistics
  if (cfg.alerts) for (const k of ['enabled', 'sound', 'webhooks']) bool('alerts', 'alerts.' + k);
  if (cfg.rfStat) bool('rf', 'rfStat.enabled');

  // self-update (internal/selfupdate Settings.Check)
  if (cfg.update && typeof cfg.update === 'object') {
    const u = cfg.update;
    bool('updates', 'update.enabled');
    if (u.channel !== undefined && u.channel !== '') oneOf('updates', 'update.channel', ['stable', 'beta']);
    const mu = String(u.manifestUrl ?? '').trim();
    if (mu && !validUpdateURL(mu)) add('updates', 'update.manifestUrl', 'Enter an http(s) URL of the release manifest.');
    const ks = Array.isArray(u.keys) ? u.keys : [];
    if (u.keys !== undefined && u.keys !== null && !Array.isArray(u.keys)) add('updates', 'update.keys', 'Enter one key per line.');
    if (ks.some((k) => !isValidUpdateKey(k))) add('updates', 'update.keys', 'Every key is the base64 of a 32-byte Ed25519 public key (44 characters ending in "=").');
    if (ks.length > 16) add('updates', 'update.keys', 'At most 16 keys.');
    if (u.enabled && (!mu || !ks.length)) add('updates', 'update.enabled', 'Automatic updates need the manifest URL and at least one release key.');
    if (u.checkIntervalMin) range('updates', 'update.checkIntervalMin', 15, 44640, 'Check interval', { int: true, unit: 'min' });
    if (u.healthWindowS) range('updates', 'update.healthWindowS', 10, 900, 'Health window', { int: true, unit: 's' });
    if (u.maxDownloadMB) range('updates', 'update.maxDownloadMB', 1, 4096, 'Largest download', { int: true, unit: 'MB' });
  }
  return out;
}

/** A release public key: base64 of 32 bytes (44 characters, one "=" of padding). */
export function isValidUpdateKey(k) {
  return typeof k === 'string' && /^[A-Za-z0-9+/]{43}=$/.test(k.trim());
}

function validUpdateURL(raw) {
  if (raw.length > 2048 || /\s/.test(raw)) return false;
  try {
    const u = new URL(raw);
    return (u.protocol === 'http:' || u.protocol === 'https:') && !!u.hostname;
  } catch { return false; }
}

function validFeedURL(raw) {
  if (raw.length > 2048 || /\s/.test(raw)) return false;
  try {
    const u = new URL(raw.includes('://') ? raw : 'http://' + raw);
    return (u.protocol === 'http:' || u.protocol === 'https:') && !!u.hostname;
  } catch { return false; }
}

/** Structural check of an imported config.json before it is validated field by field. */
export function checkImportShape(obj) {
  const errs = [];
  if (obj === null || typeof obj !== 'object' || Array.isArray(obj)) return ['The file does not contain a JSON object.'];
  const objs = ['http', 'receiver', 'ntp', 'weather', 'hub', 'remote', 'storage', 'ui', 'demo', 'prediction', 'terrain', 'navData', 'alerts', 'rfStat', 'update'];
  const known = new Set(['version', 'mode', 'antennas', 'feeds', ...objs]);
  if (!Object.keys(obj).some((k) => known.has(k))) errs.push('None of the AirView configuration sections were found in the file.');
  for (const k of objs) if (k in obj && (obj[k] === null || typeof obj[k] !== 'object' || Array.isArray(obj[k]))) errs.push(`"${k}" must be an object.`);
  for (const k of ['antennas', 'feeds']) if (k in obj && !Array.isArray(obj[k])) errs.push(`"${k}" must be a list.`);
  if ('version' in obj && typeof obj.version === 'number' && obj.version > 1) errs.push(`The file was written by a newer AirView (config version ${obj.version}).`);
  return errs;
}

/**
 * Leaf-by-leaf comparison of what was sent and what the server stored: every difference is a correction
 * the server made. Secret fields and the version stamp are skipped.
 * @returns {Array<{path: string, sent: any, stored: any}>}
 */
export function diffConfig(sent, stored) {
  const out = [];
  const walk = (a, b, path) => {
    if (path === 'version' || SECRET_PATHS.includes(path)) return;
    if (Array.isArray(a) || Array.isArray(b)) {
      const aa = Array.isArray(a) ? a : [];
      const bb = Array.isArray(b) ? b : [];
      const leaf = (x) => x === null || typeof x !== 'object';
      if (aa.every(leaf) && bb.every(leaf)) { if (JSON.stringify(aa) !== JSON.stringify(bb)) out.push({ path, sent: aa, stored: bb }); return; }
      if (aa.length !== bb.length) out.push({ path, sent: `${aa.length} entries`, stored: `${bb.length} entries` });
      for (let i = 0; i < Math.min(aa.length, bb.length); i++) walk(aa[i], bb[i], `${path}.${i}`);
      return;
    }
    if (a !== null && typeof a === 'object' && b !== null && typeof b === 'object') {
      for (const k of Object.keys(a)) walk(a[k], b[k], path ? `${path}.${k}` : k);
      return;
    }
    if (a === undefined) return;
    const same = a === b || (typeof a === 'string' && typeof b === 'string' && a.trim() === b);
    if (!same) out.push({ path, sent: a, stored: b });
  };
  walk(sent, stored, '');
  return out;
}

/** Top-level keys whose value differs between two configs. */
export function changedKeys(a, b) {
  const keys = new Set([...Object.keys(a || {}), ...Object.keys(b || {})]);
  return [...keys].filter((k) => JSON.stringify(a?.[k]) !== JSON.stringify(b?.[k]));
}

export function newAntenna(existing) {
  const used = existing.map((a) => a.id);
  const n = existing.length;
  return {
    id: uniqueId('antenna', used, 'antenna'), name: 'New antenna', type: 'vertical', channel: -1, gainDbi: 5, heightAglM: 8,
    azimuthDeg: 0, tiltDeg: 0, hBeamwidthDeg: 0, vBeamwidthDeg: 0, frontToBackDb: 0, elements: 0, polarization: 'vertical',
    cableLossDb: 0, lnaGainDb: 0, lnaNoiseFigDb: 0, filterLossDb: 0, notes: '', color: ANT_PALETTE[n % ANT_PALETTE.length],
  };
}

export function newFeed(existing, preset) {
  const base = { id: '', name: 'New feed', enabled: true, type: 'beast', host: '', port: 10003, url: '', antennaId: '', mlatOnly: false, notes: '', ...preset };
  base.id = uniqueId(base.name, existing.map((f) => f.id), 'feed');
  return base;
}

// ---------------------------------------------------------------------------------------------
// antenna pattern model - a port of internal/radio/pattern.go (Resolve, Omni, Directional)
// ---------------------------------------------------------------------------------------------

const D2R = Math.PI / 180;
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
const fromDB = (db) => 10 ** (db / 10);
const toDB = (x) => 10 * Math.log10(x);
const cosExponent = (bw) => Math.LN2 / -Math.log(Math.cos((clamp(bw, 0.5, 170) / 2) * D2R));
const omniBeamwidthForGain = (g) => {
  const x = (4 * 0.00272 * 101.5) / fromDB(g);
  return x >= 1 ? 120 : clamp((1 - Math.sqrt(1 - x)) / (2 * 0.00272), 2, 120);
};
const omniGainForBeamwidth = (bw) => { const t = clamp(bw, 2, 120); return toDB(101.5 / (t - 0.00272 * t * t)); };

/**
 * Resolve an antenna configuration into the full parameter set of its pattern model, estimating what is 0.
 * @returns {{kind, directional, peakGainDbi, azimuthDeg, tiltDeg, hBeamwidthDeg, vBeamwidthDeg, frontToBackDb, sidelobeDb, floorDb, estimated: string[]}}
 */
export function resolvePattern(a) {
  const n = (v) => (isNum(v) ? v : 0);
  let kind = a.type;
  if (!['vertical', 'collinear', 'yagi', 'patch'].includes(kind)) kind = 'other';
  const hIn = n(a.hBeamwidthDeg);
  const vIn = n(a.vBeamwidthDeg);
  const directional = kind === 'yagi' || kind === 'patch' || (kind === 'other' && hIn > 0 && hIn < 360);
  const est = [];
  let gain = n(a.gainDbi);
  if (!directional) {
    let vbw = vIn;
    const haveGain = gain !== 0;
    const haveBW = vbw > 0 && vbw <= 180;
    if (haveGain && !haveBW) { vbw = omniBeamwidthForGain(gain); est.push('vBeamwidthDeg'); }
    else if (!haveGain && haveBW) { gain = omniGainForBeamwidth(vbw); est.push('gainDbi'); }
    else if (!haveGain && !haveBW) {
      if (kind === 'collinear') { gain = 6; vbw = omniBeamwidthForGain(gain); } else { gain = 2.15; vbw = 78; }
      est.push('gainDbi', 'vBeamwidthDeg');
    }
    vbw = clamp(vbw, 2, 120);
    const sidelobeDb = kind === 'collinear' || vbw < 45 ? 13.3 : 0;
    return { kind, directional: false, peakGainDbi: gain, azimuthDeg: 0, tiltDeg: clamp(n(a.tiltDeg), -45, 45), hBeamwidthDeg: 360, vBeamwidthDeg: vbw, frontToBackDb: 0, sidelobeDb, floorDb: 30, estimated: est };
  }
  let h = hIn;
  let v = vIn;
  const haveH = h > 0 && h < 360;
  const haveV = v > 0 && v <= 180;
  if (gain === 0) {
    if (haveH && haveV) gain = toDB((0.7 * 41253) / (clamp(h, 2, 170) * clamp(v, 2, 170)));
    else if (kind === 'yagi' && n(a.elements) >= 2) gain = 10 * Math.log10(clamp(n(a.elements), 2, 40)) + 3;
    else gain = kind === 'yagi' ? 10 : 8;
    est.push('gainDbi');
  }
  if (!haveH || !haveV) {
    const prod = (0.7 * 41253) / fromDB(gain);
    if (haveH) { v = prod / h; est.push('vBeamwidthDeg'); }
    else if (haveV) { h = prod / v; est.push('hBeamwidthDeg'); }
    else {
      const el = n(a.elements);
      const r = kind === 'yagi' ? (el < 2 ? 1.2 : 1 + 1.5 / (el - 1)) : 1;
      const narrow = Math.sqrt(prod / r);
      const wide = r * narrow;
      if (a.polarization === 'horizontal') { h = narrow; v = wide; } else { h = wide; v = narrow; }
      est.push('hBeamwidthDeg', 'vBeamwidthDeg');
    }
  }
  let ftb = n(a.frontToBackDb);
  if (!(ftb > 0)) { ftb = kind === 'patch' ? 20 : 15; est.push('frontToBackDb'); }
  const sll = kind === 'patch' ? ftb : Math.min(13, ftb);
  return {
    kind, directional: true, peakGainDbi: gain, azimuthDeg: (((n(a.azimuthDeg) % 360) + 360) % 360), tiltDeg: clamp(n(a.tiltDeg), -90, 90),
    hBeamwidthDeg: clamp(h, 2, 170), vBeamwidthDeg: clamp(v, 2, 170), frontToBackDb: ftb, sidelobeDb: sll, floorDb: 0, estimated: est,
  };
}

/** Gain in dBi of a resolved pattern towards azimuth / elevation (degrees). */
export function patternGain(p, azDeg, elDeg) {
  if (!p.directional) {
    const el = clamp(elDeg, -90, 90);
    const d = Math.abs(el - p.tiltDeg) * D2R;
    let rel = -p.floorDb;
    if (d < Math.PI / 2) rel = Math.max(rel, 10 * cosExponent(p.vBeamwidthDeg) * Math.log10(Math.cos(d)));
    if (p.sidelobeDb > 0) {
      const ce = Math.cos(el * D2R);
      if (ce > 1e-9) {
        const sinSL1 = Math.sin(Math.min(1.61 * p.vBeamwidthDeg, 90) * D2R);
        let env = -p.sidelobeDb + 20 * Math.log10(ce);
        const s = Math.abs(Math.sin(d));
        if (s > sinSL1) env -= 20 * Math.log10(s / sinSL1);
        rel = Math.max(rel, env);
      }
    }
    return p.peakGainDbi + rel;
  }
  const sinEl = Math.sin(elDeg * D2R);
  const cosEl = Math.cos(elDeg * D2R);
  const da = (azDeg - p.azimuthDeg) * D2R;
  const sinT = Math.sin(p.tiltDeg * D2R);
  const cosT = Math.cos(p.tiltDeg * D2R);
  const tx = clamp(sinEl * sinT + cosEl * cosT * Math.cos(da), -1, 1);
  const ty = cosEl * Math.sin(da);
  const tz = sinEl * cosT - cosEl * sinT * Math.cos(da);
  let rel = -(p.sidelobeDb + ((p.frontToBackDb - p.sidelobeDb) * (1 - tx)) / 2);
  if (tx > 0) {
    const nh = cosExponent(p.hBeamwidthDeg);
    const nv = cosExponent(p.vBeamwidthDeg);
    const s = ty * ty + tz * tz;
    const n = s > 1e-18 ? (nh * ty * ty + nv * tz * tz) / s : nh;
    rel = Math.max(rel, 10 * n * Math.log10(tx));
  }
  return p.peakGainDbi + rel;
}

/** Azimuth cut at `elevDeg`: [[gainDbi, bearingDeg], ...] for charts.polar. */
export function azimuthCut(p, elevDeg = 3, step = 2) {
  const out = [];
  for (let az = 0; az < 360; az += step) out.push([patternGain(p, az, elevDeg), az]);
  return out;
}

/** Elevation cut through the boresight from -10 to +90 degrees: [[elevDeg, gainDbi], ...]. */
export function elevationCut(p, step = 1) {
  const out = [];
  for (let el = -10; el <= 90; el += step) out.push([el, patternGain(p, p.azimuthDeg, el)]);
  return out;
}

const EARTH_KM = 6371.0088;
/** Radio horizon distance in km of a point `hM` metres above a smooth earth (k = 4/3 standard atmosphere). */
export function horizonKm(hM, k = 4 / 3) {
  if (!(hM > 0)) return 0;
  const re = k * EARTH_KM;
  return re * Math.acos(re / (re + hM / 1000));
}
/** Radio line of sight between an antenna (m AMSL) and an aircraft (ft AMSL). */
export function lineOfSightKm(antennaM, aircraftFt, k = 4 / 3) {
  return horizonKm(antennaM, k) + horizonKm(aircraftFt * 0.3048, k);
}
