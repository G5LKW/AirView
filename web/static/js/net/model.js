// net/model.js - pure rules of the network (aggregator) UI: modes, scopes, views, page access,
// the ctx.station shape, the station= request parameter and the defensive public-payload scrubber.
//
// No DOM and no imports: the module is unit-tested with `node --test web/dev/test` and used by
// net/store.js (state), app.js (navigation) and the network pages. DESIGN §8.2 (visibility matrix) and
// §11 (views) are the source of every rule here; the server enforces them, this module keeps the UI from
// asking for, or showing, what a viewer may not see.

/** Operating modes the UI distinguishes. remote-data / remote-web behave like standalone in the browser. */
export const MODE_STANDALONE = 'standalone';
export const MODE_AGGREGATOR = 'aggregator';

/** Viewer scopes. 'local' = standalone mode (the station's own UI; admin rules of today apply). */
export const SCOPES = ['public', 'owner', 'admin'];

/** Map views (DESIGN §11), in selector order. */
export const VIEWS = [
  { id: 'network', label: 'Network', icon: 'globe', tip: 'Merged traffic of the whole network. No receiver information.' },
  { id: 'mine', label: 'My stations', icon: 'antenna', tip: 'Aircraft your stations hear, your exact station markers and range.' },
  { id: 'station', label: 'Station', icon: 'radar', tip: 'Full single-station map centred on the selected station.' },
  { id: 'admin', label: 'Admin', icon: 'shield', tip: 'Every station exact, receivers per aircraft (audited).' },
];

/**
 * The mode of the server from GET /api/session (sessionInfo.aggregator) and, as a fallback, model.Status.mode.
 * @param {object|null} session  @param {object|null} status
 */
export function detectMode(session, status) {
  if (session && session.aggregator === true) return MODE_AGGREGATOR;
  if (status && status.mode === MODE_AGGREGATOR) return MODE_AGGREGATOR;
  return MODE_STANDALONE;
}

/**
 * Viewer scope of a session in aggregator mode: 'public' | 'owner' | 'admin'.
 * An admin session without a confirmed second factor (sessionInfo.admin false) is treated as owner,
 * like the server does (admin = scope admin + MFA).
 */
export function scopeOf(session) {
  if (!session || !session.loggedIn) return 'public';
  if (session.scope === 'admin') return session.admin === false ? 'owner' : 'admin';
  if (session.scope === 'owner') return 'owner';
  return 'public';
}

/** Map views a scope may use. */
export function viewsFor(scope) {
  switch (scope) {
    case 'admin': return ['network', 'mine', 'station', 'admin'];
    case 'owner': return ['network', 'mine', 'station'];
    default: return ['network'];
  }
}

/**
 * The view to show: the requested one when allowed (and, for 'station', when a station is selected),
 * else the network view.
 */
export function normalizeView(view, scope, hasStation) {
  const allowed = viewsFor(scope);
  if (!allowed.includes(view)) return 'network';
  if (view === 'station' && !hasStation) return allowed.includes('mine') ? 'mine' : 'network';
  return view;
}

/** True when the map view must not show any receiver-centred information (rings, beams, range, heat ...). */
export function viewHidesReceiver(view) {
  return view === 'network' || view === 'admin' || view === 'mine';
}

/**
 * Page access in aggregator mode. Every page id of the shell plus the network pages.
 *   public:  the merged map and the public network page, Feed Check and the claim / login flows.
 *   owner:   needs a login (any scope above public): My stations (each station's own statistics, connection
 *            log, location and privacy come from /api/me/stations/{sid}*), the account.
 *   station: needs owner or admin scope and a selected station (reserved for pages that the server serves
 *            per station; none today).
 *   admin:   needs the admin scope. The single-station pages (Statistics, Antenna Lab, RF Lab, Timing,
 *            Weather ...) show the host station's data and the server answers them to the admin only
 *            (internal/server WIRING.md, AccessAdmin); also the configuration, alerts and the admin console.
 * Standalone mode never consults this table: every page is available as today.
 */
export const PAGE_ACCESS = {
  map: 'public',
  network: 'public',
  feeder: 'public',
  check: 'public',
  claim: 'public',
  setup: 'public',
  share: 'public', // share links (#/share/<token>): the token is the access
  me: 'owner',
  account: 'owner',
  aircraft: 'admin',
  reports: 'admin',
  stats: 'admin',
  antenna: 'admin',
  rf: 'admin',
  weather: 'admin',
  forecast: 'admin',
  timing: 'admin',
  guess: 'admin',
  alerts: 'admin',
  settings: 'admin',
  admin: 'admin',
};

/**
 * Whether a page may be shown.
 * @returns {{ok: boolean, need?: 'login'|'station'|'admin', reason?: string}}
 */
export function pageAccess(pageId, net) {
  if (!net || net.mode !== MODE_AGGREGATOR) return { ok: true };
  const rule = PAGE_ACCESS[pageId] || 'admin';
  const scope = net.scope || 'public';
  if (rule === 'public') return { ok: true };
  if (scope === 'public') return { ok: false, need: 'login', reason: 'Log in to see this page.' };
  if (rule === 'owner') return { ok: true };
  if (rule === 'admin') return scope === 'admin' ? { ok: true } : { ok: false, need: 'admin', reason: 'Only the network administrator can open this page.' };
  // station-scoped
  if (!net.station || !net.station.sid) return { ok: false, need: 'station', reason: 'Choose one of your stations first.' };
  return { ok: true };
}

/** Sidebar groups of a scope in aggregator mode: ids in display order ('sep' = divider). */
export function navFor(scope) {
  const list = ['map', 'network', 'feeder', 'check'];
  if (scope === 'public') return list;
  list.push('me');
  // the host station's own pages (admin only, see PAGE_ACCESS)
  if (scope === 'admin') list.push('sep', 'aircraft', 'reports', 'stats', 'antenna', 'rf', 'timing', 'weather', 'sep', 'alerts', 'admin', 'settings');
  return list;
}

/**
 * ctx.station: the station the station-scoped pages show.
 *   { sid, name, scope, located, lat?, lon?, antennas, local }
 * Standalone: the local station from model.Config (sid '' = no station= parameter, so every request is
 * exactly as before). Aggregator: an owner/admin station DTO (exact coordinates are owner/admin data).
 */
export function localStation(cfg) {
  const rx = (cfg && cfg.receiver) || {};
  const located = !!rx.located && Number.isFinite(rx.lat) && Number.isFinite(rx.lon);
  return {
    sid: '', local: true, scope: 'local', name: rx.name || '', located,
    lat: located ? rx.lat : undefined, lon: located ? rx.lon : undefined,
    antennas: Array.isArray(cfg?.antennas) ? cfg.antennas : [],
  };
}

/** ctx.station from an owner / admin station DTO (GET /api/me/stations, /api/admin/stations). */
export function networkStation(dto, scope) {
  if (!dto || !dto.sid) return null;
  const loc = dto.location || {};
  const lat = num(dto.lat) ? dto.lat : num(loc.lat) ? loc.lat : undefined;
  const lon = num(dto.lon) ? dto.lon : num(loc.lon) ? loc.lon : undefined;
  return {
    sid: String(dto.sid), local: false, scope: scope || 'owner', name: dto.name || shortSid(dto.sid),
    located: lat !== undefined && lon !== undefined, lat, lon,
    antennas: Array.isArray(dto.antennas) ? dto.antennas : [],
  };
}

/**
 * A model.Config-like object for the receiver-centred map overlays (range rings, beams, compass).
 * Returns the real config in standalone mode (unchanged behaviour), nothing receiver-related in a view that
 * hides the receiver, and the selected station's exact position in the station view.
 */
export function overlayConfig(cfg, net) {
  if (!net || net.mode !== MODE_AGGREGATOR) return cfg;
  const base = { ...(cfg || {}), ui: (cfg && cfg.ui) || {} };
  const st = net.station;
  if (net.view !== 'station' || !st || !st.located) {
    return { ...base, receiver: { name: '', located: false, lat: 0, lon: 0 }, antennas: [] };
  }
  return { ...base, receiver: { name: st.name, located: true, lat: st.lat, lon: st.lon }, antennas: st.antennas || [] };
}

/**
 * Paths that take ?station=<sid> (the viewer's selected station: geometry - distance, bearing, elevation -
 * relative to it). /api/me/aircraft selects the station on the server today; the live picture routes accept
 * the parameter harmlessly and use it once the server projects geometry per station (HOOKS.md).
 */
const STATION_SCOPED = [/^\/api\/(events|aircraft|trails)$/, /^\/api\/aircraft\/[^/]+$/, /^\/api\/me\/aircraft$/];

/**
 * The station= parameter for a request: the selected station's SID for the station-scoped paths in
 * aggregator mode, otherwise nothing (standalone requests stay byte-identical to today's).
 */
export function stationParam(path, sid) {
  if (!sid) return '';
  const p = String(path || '').split('?')[0];
  if (!STATION_SCOPED.some((re) => re.test(p))) return '';
  if (/[?&]station=/.test(String(path))) return '';
  return String(sid);
}

/**
 * Paths the UI may request while the viewer is public in aggregator mode. Everything else is refused in the
 * browser before it reaches the network (api.js guard), so the public UI never even asks for receiver data
 * (station configuration, coverage, terrain, weather at the station, statistics ...). The server is the
 * authority (DESIGN §8.4); this is defence in depth and keeps the network tab clean.
 */
const PUBLIC_ALLOWED = [
  /^\/api\/(session|login|logout|status|aircraft|events|events\/view|trails)$/,
  /^\/api\/aircraft\/[0-9A-Fa-f]{6}$/,
  /^\/api\/public(\/|$)/,
  /^\/api\/me\/check$/,
  /^\/api\/auth\/(setup|setup\/begin|setup\/finish|register)$/,
  /^\/api\/navdata\/route\//,
  /^\/api\/photo\//,
  /^\/api\/history\/flights\/[^/]+\/trace$/,
  /^\/api\/prediction\/(intent|path)\//,
  /^\/api\/export\/live\.(kml|geojson)$/,
];

/** Whether a request path is allowed for a public viewer in aggregator mode. */
export function publicAllowed(path) {
  const p = String(path || '').split('?')[0];
  return PUBLIC_ALLOWED.some((re) => re.test(p));
}

/**
 * Keys that never appear in what a public viewer is shown (DESIGN §8.4 golden-test list plus the geometry of
 * a station-relative aircraft). scrubPublic() removes them from any object before the public UI renders it.
 */
export const PUBLIC_FORBIDDEN_KEYS = new Set([
  'seenBy', 'feeds', 'distKm', 'bearing', 'elevation', 'recentReceiverIds', 'stations', 'mine', 'receivers',
  'rssi', 'signal', 'cpaKm', 'cpaInS', 'terrainVisible', 'losMarginDeg', 'ip', 'ips', 'uuid', 'feederId',
]);

/**
 * Deep copy of `obj` without the forbidden keys. Arrays and nested objects are scrubbed too. Used on
 * everything the public network page renders, so a server that sends too much still shows nothing more.
 */
export function scrubPublic(obj, depth = 0) {
  if (depth > 12 || obj === null || typeof obj !== 'object') return obj;
  if (Array.isArray(obj)) return obj.map((x) => scrubPublic(x, depth + 1));
  const out = {};
  for (const [k, v] of Object.entries(obj)) {
    if (PUBLIC_FORBIDDEN_KEYS.has(k)) continue;
    out[k] = scrubPublic(v, depth + 1);
  }
  return out;
}

/** A public station entry: only a stored pin point is a position, and only when its precision is not hidden. */
export function publicPin(st) {
  const pin = st && st.pin;
  if (!pin || pin.precision === 'hidden' || !num(pin.lat) || !num(pin.lon)) return null;
  return { lat: pin.lat, lon: pin.lon, precision: pin.precision === 'cell01' ? 'cell01' : 'region' };
}

// ---------------------------------------------------------------------------------------------
// labels
// ---------------------------------------------------------------------------------------------

/** Clock classes (model.ClockClass wire names and codes) with a human description. */
export const CLOCK_CLASSES = [
  { code: 0, id: 'none', label: 'No timestamps', mlat: false },
  { code: 1, id: 'gps_utc', label: 'GPS (UTC)', mlat: true },
  { code: 2, id: 'gps_gpst', label: 'GPS (GPS time)', mlat: true },
  { code: 3, id: 'gps_emul', label: 'GPS format, no lock', mlat: true },
  { code: 4, id: 'hw12', label: '12 MHz hardware', mlat: true },
  { code: 5, id: 'sdr12', label: 'SDR 12 MHz', mlat: true },
  { code: 6, id: 'sdr12_hires', label: 'SDR 12 MHz high-rate', mlat: true },
  { code: 7, id: 'sbs20', label: 'SBS 20 MHz', mlat: true },
];

/** Label of a clock class given as wire name ("gps_utc") or code (1). */
export function clockLabel(c) {
  if (c === null || c === undefined || c === '') return 'Unknown';
  const hit = CLOCK_CLASSES.find((x) => x.id === c || x.code === Number(c));
  return hit ? hit.label : String(c);
}

export const PIN_OPTIONS = [
  { value: 'hidden', label: 'Hidden', hint: 'No pin on the public map (default).' },
  { value: 'region', label: 'Region (0.5°)', hint: 'A fixed point somewhere inside a ~50 km cell.' },
  { value: 'cell01', label: 'Area (0.1°)', hint: 'A fixed point inside a ~10 km cell. Shown as region where fewer than 3 stations are nearby.' },
];

export const ADMISSION_LABELS = { pending: 'Pending', approved: 'Approved', denied: 'Denied', revoked: 'Revoked' };
export const KIND_LABELS = { avf: 'AirView feeder', beast: 'Beast (30004)', mlatc: 'mlat-client', host: 'Host station' };
export const LOC_LABELS = {
  unknown: 'Unknown', estimating: 'Estimating', estimated: 'Estimated', confirmed: 'Confirmed',
  declared: 'Declared', gnss: 'GNSS', suspect: 'Suspect',
};
export const PROTO_LABELS = { avf: 'AirView (AVF/1)', beast: 'Beast', mlatc: 'mlat-client' };

/** The last 6 characters of a UUID / feeder ID, the only part Feed Check shows. */
export function idTail(id) {
  const s = String(id || '').replace(/[^0-9a-zA-Z]/g, '');
  return s ? '…' + s.slice(-6).toLowerCase() : '';
}

/** A short form of a SID for labels when a station has no name. */
export function shortSid(sid) {
  const s = String(sid || '');
  return s.length > 8 ? s.slice(0, 4) + '…' + s.slice(-3) : s;
}

/** Normalise a claim code as typed: upper case, no spaces or dashes, look-alikes mapped (0->O, 1->I, 8->B). */
export function normalizeClaimCode(s) {
  return String(s || '').toUpperCase().replace(/[\s-]/g, '').replace(/0/g, 'O').replace(/1/g, 'I').replace(/8/g, 'B');
}

/** A claim code is 8 base32 characters (A-Z, 2-7). */
export function validClaimCode(s) { return /^[A-Z2-7]{8}$/.test(normalizeClaimCode(s)); }

/** A UUID as typed: hex with or without dashes, at least 28 hex digits after stripping. */
export function normalizeUUID(s) { return String(s || '').toLowerCase().replace(/[^0-9a-f]/g, ''); }
export function validUUID(s) { return normalizeUUID(s).length >= 28 && normalizeUUID(s).length <= 32; }

/** Owner user names: 3-32 of a-z 0-9 . _ - (the server lower-cases). */
export function validUsername(s) { return /^[a-z0-9._-]{3,32}$/.test(String(s || '').trim().toLowerCase()); }

/** A TOTP or recovery code as typed: digits only for 6-digit codes, else upper-case letters/digits with dashes kept. */
export function normalizeCode(s) {
  const t = String(s || '').trim();
  const digits = t.replace(/\s/g, '');
  if (/^\d{6,8}$/.test(digits)) return digits;
  return t.toUpperCase().replace(/\s/g, '');
}

/**
 * Parse an otpauth://totp/ URI into its parts (for showing the account label and the issuer next to the QR).
 * @returns {{label: string, issuer: string, secret: string, digits: number, period: number}|null}
 */
export function parseOtpauth(uri) {
  const m = /^otpauth:\/\/totp\/([^?]*)\?(.*)$/i.exec(String(uri || ''));
  if (!m) return null;
  const q = new URLSearchParams(m[2]);
  let label = '';
  try { label = decodeURIComponent(m[1]); } catch { label = m[1]; }
  return {
    label, issuer: q.get('issuer') || (label.includes(':') ? label.split(':')[0] : ''), secret: q.get('secret') || '',
    digits: Number(q.get('digits')) || 6, period: Number(q.get('period')) || 30,
  };
}

/** Group a base32 secret in blocks of 4 for manual entry. */
export function groupSecret(s) { return String(s || '').replace(/\s/g, '').replace(/(.{4})/g, '$1 ').trim(); }

/** Distance band label of an owner peer (DESIGN §8.2: <50, 50-150, >150 km). */
export function bandLabel(b) {
  return { near: '< 50 km', mid: '50–150 km', far: '> 150 km' }[b] || b || '—';
}

function num(v) { return typeof v === 'number' && Number.isFinite(v); }

// ---- feeder page (pages/feeder.js) ----

/** Host for the snippets: the configured public host, else this page's host name. */
export function snippetHost(join, loc = globalThis.location) {
  const h = typeof join?.host === 'string' ? join.host.trim() : '';
  if (h && /^[A-Za-z0-9.:-]+$/.test(h)) return h.includes(':') && !h.startsWith('[') ? `[${h}]` : h;
  const own = loc?.hostname || 'aggregator.example';
  return own.includes(':') && !own.startsWith('[') ? `[${own}]` : own;
}

/** The configuration lines of the page for a join answer (pure; tested in web/dev/test/net-model.test.mjs). */
export function feederSnippets(join, host) {
  const out = {};
  if (join?.avfPort > 0) {
    const tok = join.admission === 'invite' ? ' --enrol <token from the operator>' : '';
    out.avf = [`airview-feeder setup --server ${host}:${join.avfPort}${tok}`, 'airview-feeder claim'];
  }
  if (join?.beastPort > 0) {
    out.readsb = `--net-connector ${host},${join.beastPort},beast_reduce_plus_out,uuid=<your-uuid>`;
    out.ultrafeeder = `adsb,${host},${join.beastPort},beast_reduce_plus_out,uuid=<your-uuid>`;
  }
  if (join?.mlatPort > 0) {
    out.mlatclient = `mlat-client --input-type dump1090 --input-connect 127.0.0.1:30005 --server ${host}:${join.mlatPort} --user <station-name> --lat <lat> --lon <lon> --alt <alt>m --uuid <your-uuid> --results beast,connect,127.0.0.1:30104`;
    out.ultrafeederMlat = `mlat,${host},${join.mlatPort},uuid=<your-uuid>`;
  }
  return out;
}

// ---- consent text C6, notice form (pages/me.js Location tab) ----

/**
 * The C6 notice for a position in use that the network survey measured (source 'estimated'): the template's
 * {σ} and {Δ} filled with whole metres; without a declared position the second sentence ("It differs ...") is
 * left out. Returns '' when the position in use is not a usable survey result. distKm(lat1, lon1, lat2, lon2).
 */
export function surveyNoticeText(eff, declared, template, distKm) {
  if (!eff || eff.source !== 'estimated' || !num(eff.sigmaM) || !num(eff.lat) || !num(eff.lon)) return '';
  const sigma = String(Math.max(1, Math.round(eff.sigmaM)));
  const cut = template.indexOf(' It differs');
  if (declared && num(declared.lat) && num(declared.lon)) {
    const d = String(Math.round(distKm(eff.lat, eff.lon, declared.lat, declared.lon) * 1000));
    return template.replace('{σ}', sigma).replace('{Δ}', d);
  }
  return (cut > 0 ? template.slice(0, cut) : template).replace('{σ}', sigma);
}
