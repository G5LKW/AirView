// settings/links.js - pure (no DOM) routing of shell notices, banners and toasts to the place where the
// problem is fixed: a Settings section (#/settings/<section>), the map (#/map/<ICAO>) or another page.
// Also the receiver-agnostic helpers that decide when Radarcape-specific copy is shown.
// Used by app.js (banners + notice toasts) and the Settings sections; checked by
// web/dev/test/links.test.mjs (node --test).

import { SECTIONS } from './model.js';

/** Hash of a Settings section; no section = the Settings page (it opens the last / first section). */
export function settingsHref(section) {
  return '#/settings' + (section && SECTIONS.some((s) => s.id === section) ? '/' + section : '');
}

/**
 * The Settings section that holds a config path ("feeds[0].host", "feeds.0.host", "receiver.lat", "ntp").
 * null when no section owns it.
 */
export function sectionForPath(path) {
  const top = String(path || '').trim().split(/[.[\s:]/)[0];
  if (!top) return null;
  const s = SECTIONS.find((x) => x.keys.includes(top));
  return s ? s.id : null;
}

/** Banner id -> where its link goes (app.js renderBanners). Every banner link is listed here. */
/** First-start text about the online route look-up (ROUTES-DESIGN binding decision 2: on by default). */
export const ROUTES_ONLINE_NOTE = 'With the factory settings, AirView looks up the route of airline callsigns it has no offline route for at adsbdb.com: the callsign is sent, the receiver location is not. Switch it off in ';

export const BANNER_LINKS = {
  setup: { label: 'Finish the first-run setup', href: '#/setup' },
  demo: { label: 'Turn off in Settings', href: settingsHref('demo') },
  loc: { label: 'Set location', href: settingsHref('station') },
  locGuess: { label: 'estimate it', href: '#/guess' },
  feeds: { label: 'Check feeds', href: settingsHref('feeds') },
  nofeeds: { label: 'Add a receiver', href: settingsHref('feeds') },
  nofeedsDemo: { label: 'enable demo mode', href: settingsHref('demo') },
  routesOnline: { label: 'Aviation data settings', href: settingsHref('navdata') },
};

// Free-text notices of the engine (internal/engine Notify) -> Settings section.
const TEXT_RULES = [
  [/^aircraft database import/i, 'navdata', 'Aviation data'],
  [/^reference data import/i, 'navdata', 'Aviation data'],
  [/^uplinks? /i, 'uplinks', 'Uplinks'],
  [/^data port|port \d+ could not be started/i, 'connection', 'Connection'],
  [/^ground elevation/i, 'station', 'Station'],
  [/^weather/i, 'weather', 'Weather'],
  [/^factory reset/i, 'station', 'Settings'],
];

/**
 * The action of a notice toast (SSE "notice": {level, text, kind?, icao?}), or null.
 * opts.settings = false (a viewer without access to the configuration): no Settings links.
 * Returns { label, href }.
 */
export function noticeLink(n, opts = {}) {
  if (!n) return null;
  const settings = opts.settings !== false;
  if (n.icao) return { label: 'Show', href: '#/map/' + String(n.icao).toUpperCase() };
  if (!settings) return null;
  if (n.kind === 'feed_down') return { label: 'Check feeds', href: settingsHref('feeds') };
  const text = String(n.text || '');
  const cfg = /^configuration:\s*(.*)$/i.exec(text);
  if (cfg) {
    const sec = sectionForPath(cfg[1]);
    return { label: 'Open settings', href: settingsHref(sec) };
  }
  for (const [re, sec, label] of TEXT_RULES) if (re.test(text)) return { label, href: settingsHref(sec) };
  return null;
}

// ---- receiver-agnostic copy ----------------------------------------------------------------------

/** The unchanged factory default feed (model.IsFactoryDefaultFeed): a Beast feed to host "radarcape". */
export function isFactoryDefaultFeed(f) {
  if (!f || f.type !== 'beast') return false;
  const port = Number(f.port);
  return (f.id === 'beast' && f.host === 'localhost' && port === 30005) || (f.id === 'radarcape' && f.host === 'radarcape' && port === 10003);
}

const RADARCAPE_RE = /radarcape/i;

/**
 * True when the configuration really has a Radarcape: a feed that names one (name, host or URL) and is
 * not merely the never-connected factory placeholder, or any feed that reports GPS-timestamped frames.
 * status (GET /api/status) is optional.
 */
export function hasRadarcape(cfg, status) {
  const live = new Map(((status && status.feeds) || []).map((f) => [f.id, f]));
  for (const f of (cfg && cfg.feeds) || []) {
    const st = live.get(f.id);
    if (st && st.gpsTime && st.enabled !== false) return true;
    if (!RADARCAPE_RE.test(`${f.name || ''} ${f.host || ''} ${f.url || ''}`)) continue;
    if (isFactoryDefaultFeed(f) && !(st && st.connected)) continue;
    return true;
  }
  if (status && status.receiverGps) return true;
  return false;
}

/** The factory placeholder is the only enabled feed and it is not connected (first start on a Pi / PC). */
export function onlyFactoryPlaceholder(feeds) {
  const on = (feeds || []).filter((f) => f.enabled);
  // status feeds carry addr "host:port", config feeds host + port
  const placeholder = (f) => isFactoryDefaultFeed(f) || (f.type === 'beast' && ((f.id === 'radarcape' && f.addr === 'radarcape:10003') || (f.id === 'beast' && f.addr === 'localhost:30005')));
  return on.length > 0 && on.every((f) => placeholder(f) && !f.connected);
}

/** The feed a local RTL-SDR decoder (readsb / dump1090) delivers on: Beast 127.0.0.1:30005. */
export const LOCAL_DECODER_FEED = { id: 'readsb', name: 'readsb / dump1090 (Beast 30005)', type: 'beast', host: '127.0.0.1', port: 30005 };
