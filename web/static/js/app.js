// app.js - AirView shell: sidebar, top status bar, banners, hash router and page lifecycle.
// Page modules live in ./pages/<id>.js and are imported on demand (see js/README.md).

import { api } from './api.js';
import { live } from './live.js';
import { units } from './units.js';
import { ui, h, icon, setText, mount } from './ui.js';
import { charts } from './charts.js';
import { config } from './config.js';
import { geo } from './geo.js';
import * as alertSound from './alertsound.js';
import { adminCallActive } from './admin.js';
import { net } from './net/store.js';
import { pageAccess, navFor, VIEWS } from './net/model.js';
import { loginDialog, registerDialog, logout } from './net/auth.js';
import { BANNER_LINKS, noticeLink, onlyFactoryPlaceholder } from './settings/links.js';

/** The fixed page ids (docs/ARCHITECTURE.md), with titles and icons. */
export const PAGES = [
  { id: 'map', title: 'Live Map', icon: 'map', blurb: 'Real-time traffic, trails and coverage overlays.' },
  { id: 'aircraft', title: 'Aircraft', icon: 'plane', blurb: 'Every airframe and flight this station has logged.' },
  { id: 'reports', title: 'Reports & Replay', icon: 'replay', blurb: 'Search flights, replay history and export KML / GeoJSON / CSV.' },
  { id: 'alerts', title: 'Alerts', icon: 'bell', blurb: 'Alert rules, watch-lists, geofences and webhooks.' },
  { id: 'stats', title: 'Statistics', icon: 'chart', blurb: 'Message rates, range, traffic levels and records.' },
  { id: 'antenna', title: 'Antenna Lab', icon: 'antenna', blurb: 'Coverage, patterns and per-antenna performance.' },
  { id: 'rf', title: 'RF Lab', icon: 'wave', blurb: 'Signal, noise floor, detection probability, channel load and interrogators.' },
  { id: 'weather', title: 'Weather', icon: 'cloud', blurb: 'Conditions, refractivity and their effect on reception.' },
  { id: 'forecast', title: 'Forecast', icon: 'forecast', blurb: 'Predicted traffic, range and receiving conditions.' },
  { id: 'timing', title: 'Timing', icon: 'timer', blurb: 'NTP reference, receiver clock and timestamp quality.' },
  { id: 'guess', title: 'Locate Me', icon: 'target', blurb: 'Estimate the receiver position from received traffic.' },
  { id: 'settings', title: 'Settings', icon: 'settings', blurb: 'Station, antennas, feeds, storage and connections.' },
  // network (aggregator) mode only - never in the standalone sidebar (docs/aggregator/DESIGN.md §11)
  { id: 'network', net: true, title: 'Network', icon: 'globe', blurb: 'The network at a glance: stations, traffic and MLAT quality.' },
  { id: 'feeder', net: true, title: 'Feed this network', icon: 'antenna', blurb: 'Connect your receiver: AirView feeder, readsb or mlat-client.' },
  { id: 'check', net: true, href: '#/me/check', title: 'Feed Check', icon: 'signal', blurb: 'Is my feeder connected? Checks the links from your address.' },
  { id: 'claim', net: true, href: '#/me/claim', title: 'Claim a feeder', icon: 'claim', blurb: 'Link a feeder to your account with its claim code or UUID.' },
  { id: 'me', net: true, href: '#/me/stations', title: 'My stations', icon: 'antenna', blurb: 'Your stations: connection, reception, timing, location and privacy.' },
  { id: 'account', net: true, href: '#/me/account', title: 'Account', icon: 'user', blurb: 'Password, authenticator, recovery codes, sessions and your data.' },
  { id: 'admin', net: true, title: 'Network admin', icon: 'shield', blurb: 'Station registry, enrol tokens, admission and the audit log.' },
  { id: 'setup', net: true, title: 'First-run setup', icon: 'key', blurb: 'Create the network administrator.' },
  { id: 'share', net: true, title: 'Shared view', icon: 'share', blurb: 'A read-only live view an owner shared with a link.' },
];
/** #/me/<x> routes that are pages of their own (DESIGN §11 names them /me/check, /me/claim ...). */
const ROUTE_ALIASES = { check: 'check', claim: 'claim', account: 'account' };
const DEFAULT_PAGE = 'map';

// ---------------------------------------------------------------------------------------------
// app-level shared store (survives page navigation): ctx.shared
// ---------------------------------------------------------------------------------------------
function makeShared() {
  const data = new Map();
  const subs = new Map();
  return {
    /** Read a shared value. */
    get: (key) => data.get(key),
    /** Publish a shared value (undefined/null deletes) and notify subscribers. */
    set(key, value) {
      if (value === undefined || value === null) data.delete(key); else data.set(key, value);
      for (const fn of subs.get(key) || []) { try { fn(value ?? undefined); } catch (e) { console.error('[shared] subscriber failed', e); } }
    },
    /** Subscribe to a key. Returns an unsubscribe function. */
    on(key, fn) {
      if (!subs.has(key)) subs.set(key, new Set());
      subs.get(key).add(fn);
      return () => subs.get(key)?.delete(fn);
    },
  };
}
const shared = makeShared();

// ---------------------------------------------------------------------------------------------
// router
// ---------------------------------------------------------------------------------------------
function parseHash() {
  const raw = location.hash.replace(/^#\/?/, '');
  const [pathPart, queryPart = ''] = raw.split('?');
  let segs = pathPart.split('/').filter(Boolean).map((s) => { try { return decodeURIComponent(s); } catch { return s; } });
  const path = '#/' + segs.map(encodeURIComponent).join('/');
  if (segs[0] === 'me' && ROUTE_ALIASES[segs[1]]) segs = [ROUTE_ALIASES[segs[1]], ...segs.slice(2)];
  const id = segs[0] || DEFAULT_PAGE;
  return { id, args: segs.slice(1), query: Object.fromEntries(new URLSearchParams(queryPart)), path: segs.length ? path : '#/' + id };
}

/** Navigate to a route: navigate('map/4CA1FA'), navigate('#/settings'), { replace: true } keeps history clean. */
export function navigate(to, opts = {}) {
  let hash = String(to || '');
  if (!hash.startsWith('#')) hash = '#/' + hash.replace(/^\/+/, '');
  if (location.hash === hash) return;
  if (opts.replace) history.replaceState(null, '', hash);
  else location.hash = hash;
  if (opts.replace) route(); // replaceState does not fire hashchange
}

// ---------------------------------------------------------------------------------------------
// shell API (ctx.shell): kiosk mode
// ---------------------------------------------------------------------------------------------
const kioskSubs = new Set();
let kioskOn = false;

/**
 * Shell controls a page may use (also passed as ctx.shell).
 *   shell.setKiosk(true)   hide sidebar, top bar and banners; the page fills the window (Escape leaves)
 *   shell.kiosk            current state
 *   shell.onKiosk(fn)      fn(on) on every change (also when Escape or the exit button left it); returns unsubscribe
 * Kiosk mode is not persisted: a reload always starts with the normal shell. Navigating to another page keeps it.
 */
export const shell = {
  setKiosk(on) {
    on = !!on;
    if (on === kioskOn) return;
    kioskOn = on;
    document.documentElement.classList.toggle('kiosk', on);
    if (el.kioskExit) el.kioskExit.hidden = !on;
    window.dispatchEvent(new Event('resize')); // maps and charts re-measure their containers
    for (const fn of kioskSubs) { try { fn(on); } catch (e) { console.error('[shell] kiosk subscriber failed', e); } }
  },
  get kiosk() { return kioskOn; },
  onKiosk(fn) { kioskSubs.add(fn); return () => kioskSubs.delete(fn); },
};

document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape' || !kioskOn || e.defaultPrevented) return;
  // an open dialog / menu / drawer takes the Escape first
  if (document.querySelector('.modal-backdrop, .drawer-backdrop, dialog[open], .menu, .popover')) return;
  shell.setKiosk(false);
});

// ---------------------------------------------------------------------------------------------
// shell
// ---------------------------------------------------------------------------------------------
const el = {};
const NAV_KEY = 'airview.nav.open';

function buildShell() {
  const app = document.getElementById('app');
  let navOpen = false;
  try { navOpen = localStorage.getItem(NAV_KEY) === '1'; } catch { /* ignore */ }

  el.navItems = new Map();
  const nav = h('nav.nav', { 'aria-label': 'Pages' });
  el.nav = nav;
  const toggleBtn = h('button.nav-item.nav-toggle', { type: 'button', 'aria-label': 'Toggle sidebar', tip: 'Expand sidebar', 'data-tip-place': 'right' }, icon('chevrons-right'), h('span', 'Collapse'));
  el.version = h('div.sidebar-ver');
  const setNav = (open) => {
    navOpen = open;
    app.classList.toggle('nav-open', open);
    for (const a of [...el.navItems.values(), toggleBtn]) { if (open) delete a.dataset.tip; else a.dataset.tip = a === toggleBtn ? 'Expand sidebar' : a.textContent; }
    try { localStorage.setItem(NAV_KEY, open ? '1' : '0'); } catch { /* ignore */ }
  };
  el.refreshNavTips = () => setNav(navOpen);
  renderNav();
  toggleBtn.addEventListener('click', () => setNav(!navOpen));

  const sidebar = h('aside.sidebar',
    h('a.brand', { href: '#/map', 'aria-label': 'AirView' }, h('img.brand-logo', { src: 'img/logo.svg', alt: '', width: 32, height: 32 }), h('span.brand-name', { html: 'Air<b>View</b>' })),
    nav,
    h('div.sidebar-foot', toggleBtn, el.version));

  // --- top bar ---
  el.title = h('h1', '');
  el.station = h('span.station', '');
  el.linkDot = h('span.dot');
  el.acCount = h('span.num', '—');
  el.msgRate = h('span.num', '—');
  el.range = h('span.num', '—');
  el.rangeUnit = h('span.unit', '');
  el.feedDots = h('span.dots');
  el.ntpText = h('span.num', '—');
  el.ntpDot = h('span.dot.off');
  el.dayIcon = h('span.row', icon('sun'));
  el.dayText = h('span', '—');
  el.unitsText = h('span', units.systemShort[units.system]);
  el.clock = h('span.num', '--:--:--');

  el.chipAc = h('a.chip.chip--primary.chip-btn', { href: '#/map' }, el.linkDot, icon('plane'), el.acCount);
  el.chipRate = h('span.chip.chip--opt0', icon('signal'), el.msgRate, h('span.unit', 'msg/s'));
  el.chipRange = h('span.chip.chip--opt1', { tip: 'Farthest aircraft currently tracked' }, icon('radar'), el.range, el.rangeUnit);
  el.chipFeeds = h('a.chip.chip-btn', { href: '#/settings' }, h('span.label', 'Feeds'), el.feedDots);
  el.chipNtp = h('a.chip.chip-btn.chip--opt1', { href: '#/timing' }, el.ntpDot, h('span.label', 'NTP'), el.ntpText);
  el.chipDay = h('span.chip.chip--opt2', el.dayIcon, el.dayText);
  el.chipUnits = h('button.chip.chip-btn.chip--opt2', { type: 'button', tip: 'Switch display units', onclick: () => units.cycle() }, icon('ruler'), el.unitsText);
  el.chipClock = h('span.chip.chip--clock', { tip: 'Coordinated Universal Time (server clock)' }, el.clock, h('span.unit', 'UTC'));
  // network mode: station picker and account chip (hidden in standalone mode)
  el.pickName = h('span.chip-text', '');
  el.chipPick = h('button.chip.chip-btn.chip--pick.hidden', { type: 'button', tip: 'Station shown on the station pages', onclick: () => openStationPicker(el.chipPick) }, icon('antenna'), el.pickName, icon('chevron-down'));
  el.userName = h('span.chip-text', '');
  el.chipUser = h('button.chip.chip-btn.chip--user.hidden', { type: 'button', onclick: () => openUserMenu(el.chipUser) }, icon('user'), el.userName);

  const topbar = h('header.topbar',
    h('a.topbar-brand', { href: '#/map' }, h('img', { src: 'img/logo.svg', alt: '', width: 26, height: 26 }), h('span.topbar-brand-name', 'AirView')),
    h('div.topbar-title', el.title, el.station),
    h('div.topbar-stats', el.chipAc, el.chipRate, el.chipRange, el.chipFeeds, el.chipNtp, el.chipDay, el.chipUnits, el.chipPick, el.chipUser, el.chipClock));

  el.banners = h('div.banners');
  el.main = h('main.main#main');
  el.kioskExit = h('button.kiosk-exit', { type: 'button', hidden: true, tip: 'Leave full screen (Esc)', 'aria-label': 'Leave kiosk mode', 'data-tip-place': 'bottom', onclick: () => shell.setKiosk(false) }, icon('x'));
  mount(app, sidebar, topbar, el.banners, el.main, el.kioskExit);
  setNav(navOpen);
  app.hidden = false;
  document.getElementById('boot')?.remove();
}

// ---------------------------------------------------------------------------------------------
// network (aggregator) mode: sidebar per scope, station picker, account chip
// ---------------------------------------------------------------------------------------------
const pageById = (id) => PAGES.find((p) => p.id === id);

/** (Re)build the sidebar: the standalone pages as always, or the pages of the viewer's scope in network mode. */
function renderNav() {
  const ids = net.aggregator
    ? navFor(net.scope)
    : PAGES.filter((p) => !p.net).flatMap((p) => (p.id === 'settings' ? ['sep', p.id] : [p.id]));
  const sig = ids.join(',');
  if (el.nav.__sig === sig) return;
  el.nav.__sig = sig;
  el.navItems = new Map();
  const items = ids.map((id) => {
    if (id === 'sep') return h('div.nav-sep');
    const p = pageById(id);
    if (!p) return null;
    const a = h('a.nav-item', { href: p.href || '#/' + p.id, tip: p.title, 'data-tip-place': 'right' }, icon(p.icon), h('span', p.title));
    el.navItems.set(p.id, a);
    return a;
  });
  mount(el.nav, items);
  el.refreshNavTips?.();
  markActiveNav(current?.id || parseHash().id);
}

function markActiveNav(id) {
  for (const [pid, a] of el.navItems) {
    const on = pid === id;
    a.classList.toggle('active', on);
    if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  }
}

/** Top-bar chips that depend on the mode / scope. */
function renderNetChrome() {
  const agg = net.aggregator;
  const hostInfo = !agg || net.scope === 'admin';
  el.chipFeeds.classList.toggle('hidden', !hostInfo);
  el.chipNtp.classList.toggle('hidden', !hostInfo);
  el.chipRange.classList.toggle('hidden', agg && !net.station);
  el.chipPick.classList.toggle('hidden', !agg || !net.loggedIn);
  el.chipUser.classList.toggle('hidden', !agg);
  if (!agg) return;
  setText(el.pickName, net.station ? net.station.name : net.stations.length ? 'Choose station' : 'No stations');
  ui.tip(el.chipPick, net.station ? `Station pages show: ${net.station.name}` : 'Choose the station the station pages show');
  setText(el.userName, net.loggedIn ? net.username || 'Account' : 'Log in');
  el.chipUser.classList.toggle('chip--primary', !net.loggedIn);
  ui.tip(el.chipUser, net.loggedIn ? `Logged in as ${net.username} (${net.scope})` : 'Log in or create an account');
  setText(el.station, net.station ? net.station.name : '');
}

/** Station picker (network mode): a searchable list of the viewer's stations (admins: every station). */
export function openStationPicker(anchor) {
  if (!net.stations.length) {
    ui.menu(anchor, [
      { label: net.stationsError ? 'Stations could not be loaded' : 'You have no stations yet', icon: 'info', onClick: () => {} },
      { label: 'Claim a feeder', icon: 'claim', onClick: () => navigate('me/claim') },
      { label: 'Feed Check', icon: 'signal', onClick: () => navigate('me/check') },
    ]);
    return;
  }
  const listEl = h('div.net-pick-list');
  let pop = null;
  const draw = (q) => {
    const needle = String(q || '').trim().toLowerCase();
    const rows = net.stations.filter((s) => !needle || `${s.name || ''} ${s.sid}`.toLowerCase().includes(needle)).slice(0, 200);
    const own = rows.filter((s) => s.own);
    const other = rows.filter((s) => !s.own);
    const row = (s) => h('button.net-pick-row' + (net.station?.sid === s.sid ? '.on' : ''), { type: 'button', onclick: () => { pop?.close(); net.setStation(s.sid); } },
      h('span.dot.' + (s.online ? 'ok' : 'off')), h('span.grow', s.name || s.sid), h('code.muted', String(s.sid).slice(0, 6)));
    mount(listEl,
      own.length ? [h('div.net-pick-head', 'My stations'), own.map(row)] : null,
      other.length ? [h('div.net-pick-head', 'All stations'), other.map(row)] : null,
      rows.length ? null : h('div.muted.net-pick-empty', 'No match'));
  };
  const many = net.stations.length > 8;
  const search = ui.input({ placeholder: 'Search stations', icon: 'search', size: 'sm', clearable: true, onInput: (v) => draw(v) });
  const body = h('div.net-pick', many ? search : null, listEl,
    h('div.net-pick-foot', ui.button({ label: 'My stations', icon: 'antenna', size: 'sm', variant: 'ghost', onClick: () => { pop?.close(); navigate('me/stations'); } })));
  draw('');
  pop = ui.popover(anchor, body, { placement: 'bottom-end' });
  if (many) setTimeout(() => search.input.focus(), 0);
}

function openUserMenu(anchor) {
  if (!net.loggedIn) {
    ui.menu(anchor, [
      { label: 'Log in', icon: 'lock', onClick: () => loginDialog() },
      { label: 'Create an account', icon: 'user', onClick: () => registerDialog() },
      'sep',
      { label: 'Feed Check', icon: 'signal', onClick: () => navigate('me/check') },
      net.setupRequired ? { label: 'First-run setup', icon: 'key', onClick: () => navigate('setup') } : null,
    ].filter(Boolean));
    return;
  }
  ui.menu(anchor, [
    { label: 'My stations', icon: 'antenna', onClick: () => navigate('me/stations') },
    { label: 'Claim a feeder', icon: 'claim', onClick: () => navigate('me/claim') },
    { label: 'Account & security', icon: 'user', onClick: () => navigate('me/account') },
    net.scope === 'admin' ? { label: 'Network admin', icon: 'shield', onClick: () => navigate('admin') } : null,
    'sep',
    { label: 'Log out', icon: 'logout', onClick: () => logout() },
  ].filter(Boolean));
}

/** Placeholder for a page the viewer may not open (yet). */
function accessPlaceholder(page, acc) {
  const actions = [];
  if (acc.need === 'login') {
    actions.push(ui.button({ label: 'Log in', icon: 'lock', variant: 'primary', onClick: () => loginDialog() }));
    actions.push(ui.button({ label: 'Create account', icon: 'user', onClick: () => registerDialog() }));
  } else if (acc.need === 'station') {
    const pickBtn = ui.button({ label: net.stations.length ? 'Choose a station' : 'Claim a feeder', icon: net.stations.length ? 'antenna' : 'claim', variant: 'primary',
      onClick: () => (net.stations.length ? openStationPicker(pickBtn) : navigate('me/claim')) });
    actions.push(pickBtn);
  }
  actions.push(ui.button({ label: 'Open Live Map', icon: 'map', href: '#/map' }));
  return h('div.placeholder', h('div.placeholder-card',
    h('div.placeholder-icon', icon(acc.need === 'login' ? 'lock' : acc.need === 'admin' ? 'shield' : page.icon)),
    h('h2', page.title), h('p', page.blurb), h('p', acc.reason),
    h('div.row.gap-3', { style: 'justify-content:center;margin-top:20px;flex-wrap:wrap' }, actions)));
}

/** Pages hand station / session changes to `onNet(reason)` when they have it; others are mounted again. */
function onNetChange(reason) {
  renderNav();
  renderNetChrome();
  renderBanners();
  if (reason === 'view' || reason === 'stations') return;
  live.restart();
  if (reason === 'session') {
    if (configAllowed()) { config.load().catch(() => {}); if (!alertsStarted) startAlerts(); } else config.clear();
  }
  if (current && current.mounted && typeof current.mod.onNet === 'function' && pageAccess(current.id, net).ok) {
    try { current.mod.onNet(reason); } catch (e) { console.error(`[app] ${current.id}.onNet failed`, e); }
    return;
  }
  remount();
}

/** Standalone, or the network administrator: the station configuration (GET /api/config) may be read. */
function configAllowed() { return !net.aggregator || net.scope === 'admin'; }

// ---------------------------------------------------------------------------------------------
// status bar + banners
// ---------------------------------------------------------------------------------------------
const LINK_STATE = {
  live: ['live', 'Live stream connected'],
  polling: ['warn', 'Live stream unavailable – polling'],
  connecting: ['off', 'Connecting…'],
  offline: ['bad', 'Server unreachable'],
};
const dismissed = new Set();

function renderCounts(counts) {
  if (!counts) return;
  setText(el.acCount, units.int(counts.aircraft));
  setText(el.msgRate, units.num(counts.msgRate, counts.msgRate < 100 ? 1 : 0));
  const located = live.status ? live.status.located : true;
  setText(el.range, located && counts.maxRangeKm > 0 ? units.format('dist', counts.maxRangeKm, { unit: false, digits: 0 }) : '—');
  setText(el.rangeUnit, units.label('dist'));
  ui.tip(el.chipAc, `${counts.aircraft} aircraft · ${counts.withPos} with position\nADS-B ${counts.adsb} · MLAT ${counts.mlat} · TIS-B ${counts.tisb} · Mode S ${counts.modes}\n${LINK_STATE[live.state]?.[1] || ''}`);
}

function renderStatus(st) {
  if (!st) return;
  setText(el.version, `v${st.version || '?'} · ${st.platform || ''}`);
  // feeds
  const feeds = (st.feeds || []).filter((f) => f.enabled);
  const sig = feeds.map((f) => f.id + (f.connected ? 1 : 0)).join('|');
  if (el.feedDots.__sig !== sig) {
    el.feedDots.__sig = sig;
    mount(el.feedDots, feeds.length ? feeds.map((f) => h('span.dot.' + (f.connected ? 'ok' : 'bad'))) : h('span.dot.off'));
  }
  ui.tip(el.chipFeeds, feeds.length
    ? feeds.map((f) => `${f.connected ? '●' : '○'} ${f.name || f.id} – ${f.connected ? units.rate(f.msgRate) + (f.gpsTime ? ' · GPS time' : '') : f.lastError || 'disconnected'}`).join('\n')
    : 'No feeds enabled');
  el.chipFeeds.classList.toggle('chip--bad', feeds.length > 0 && feeds.every((f) => !f.connected) && !st.demo);
  // NTP
  const n = st.ntp || {};
  if (!n.enabled) {
    el.ntpDot.className = 'dot off';
    setText(el.ntpText, 'off');
    ui.tip(el.chipNtp, 'NTP reference disabled – set a server in Settings');
  } else if (n.synced) {
    el.ntpDot.className = 'dot ok';
    const off = Number(n.offsetMs) || 0;
    const a = Math.abs(off);
    setText(el.ntpText, (off >= 0 ? '+' : '−') + (a < 10 ? a.toFixed(2) : a < 100 ? a.toFixed(1) : Math.round(a)) + ' ms');
    ui.tip(el.chipNtp, `${n.server} · stratum ${n.stratum}\noffset ${off.toFixed(3)} ms · delay ${(Number(n.delayMs) || 0).toFixed(2)} ms · jitter ${(Number(n.jitterMs) || 0).toFixed(2)} ms`);
  } else {
    el.ntpDot.className = 'dot bad';
    setText(el.ntpText, 'no sync');
    ui.tip(el.chipNtp, `${n.server || 'NTP'}: ${n.lastError || 'no recent sample'}`);
  }
  renderCounts(live.snapshot ? live.snapshot.counts : st.counts);
  renderBanners();
  renderDayNight();
}

function renderDayNight() {
  const rx = config.current?.receiver;
  if (!rx || !rx.located) {
    el.chipDay.classList.add('hidden');
    return;
  }
  el.chipDay.classList.remove('hidden');
  const elev = geo.sunElevation(rx.lat, rx.lon, live.serverNow());
  const ph = geo.dayPhase(elev);
  if (el.dayIcon.__p !== ph.icon) { el.dayIcon.__p = ph.icon; mount(el.dayIcon, icon(ph.icon)); el.dayIcon.firstChild.style.color = ph.id === 'day' ? 'var(--amber)' : ph.id === 'night' ? 'var(--blue-hi)' : 'var(--violet)'; }
  setText(el.dayText, ph.label);
  ui.tip(el.chipDay, `Sun elevation at the receiver: ${elev.toFixed(1)}°`);
}

function renderClock() {
  setText(el.clock, units.clock(live.serverNow(), { z: false }));
}

function renderLink(state) {
  const [cls] = LINK_STATE[state] || LINK_STATE.connecting;
  el.linkDot.className = 'dot ' + cls;
  if (live.snapshot) renderCounts(live.snapshot.counts);
  renderBanners();
}

/** A banner's link (settings/links.js BANNER_LINKS): a real <a href> so that it also works with the keyboard and middle-click. */
function bannerLink(key) {
  const l = BANNER_LINKS[key];
  return h('a.banner-link', { href: l.href, 'data-banner-link': key }, l.label);
}

function banner(id, kind, iconName, content, dismissable = true) {
  if (dismissed.has(id)) return null;
  const b = h('div.banner' + (kind ? '.banner--' + kind : ''), { role: kind === 'bad' ? 'alert' : 'status' }, icon(iconName), h('div.banner-text', content));
  if (dismissable) b.appendChild(ui.button({ icon: 'x', variant: 'ghost', size: 'sm', class: 'banner-close', tip: 'Dismiss', onClick: () => { dismissed.add(id); renderBanners(); } }));
  return b;
}

let bannerSig = '';
function renderBanners() {
  const st = live.status;
  const list = [];
  if (live.state === 'offline') {
    list.push(['offline', () => banner('offline', 'bad', 'alert', 'Connection to the AirView server lost – retrying…', false)]);
  }
  if (net.aggregator && net.setupRequired) {
    list.push(['setup', () => banner('setup', 'warn', 'key', ['This network has no administrator yet. ', bannerLink('setup'), ' with the token from the setup-token file in the data directory, or run ', h('code', 'airview admin init'), ' on the server.'], false)]);
  }
  if (st && (!net.aggregator || net.scope === 'admin')) {
    if (st.demo) list.push(['demo', () => banner('demo', 'demo', 'bolt', ['Demo mode – the traffic shown is simulated. ', bannerLink('demo')])]);
    if (!st.located) {
      list.push(['loc', () => banner('loc', 'warn', 'pin', ['Receiver location not set – distances, range and antenna analysis need it. ',
        bannerLink('loc'), ' or let AirView ', bannerLink('locGuess'), ' from received traffic.'])]);
    }
    const feeds = (st.feeds || []).filter((f) => f.enabled);
    if (!st.demo && live.state !== 'offline' && feeds.length && feeds.every((f) => !f.connected) && Date.now() - (st.started || 0) > 15000) {
      // first start (Pi / PC): only the factory placeholder feed to a Radarcape named "radarcape" exists
      const text = onlyFactoryPlaceholder(st.feeds)
        ? 'No receiver feed is connected yet. Tell AirView where your receiver delivers its data (RTL-SDR with readsb or dump1090, Beast, SBS or a Radarcape). '
        : 'No receiver feed is connected. ';
      list.push(['feeds', () => banner('feeds', 'warn', 'antenna', [text, bannerLink('feeds')])]);
    }
    if (!feeds.length && !st.demo) {
      list.push(['nofeeds', () => banner('nofeeds', 'warn', 'antenna', ['No feeds are enabled. ', bannerLink('nofeeds'), ' or ', bannerLink('nofeedsDemo'), '.'])]);
    }
  }
  const sig = list.map((x) => x[0]).filter((id) => !dismissed.has(id)).join(',');
  if (sig === bannerSig) return;
  bannerSig = sig;
  mount(el.banners, list.map((x) => x[1]()));
}

// ---------------------------------------------------------------------------------------------
// alerts: one toast and one chime per alert, app-wide
// ---------------------------------------------------------------------------------------------
// The shell follows every alert (live.followAlerts) on every page. Alerts whose rule asks for a notification
// get exactly one toast with a "Show" action (#/map/<ICAO>); a batch with sound plays one chime (severity of
// the loudest) when config.alerts.sound is on, the tab is visible and this browser is not muted
// (alertsound.isMuted()). The built-in emergency / military / record rules are toasted through the tracker's
// own notice (which also fires when those rules are off), so their alerts are not toasted a second time.
// The engine's "alert" notices duplicate the alert stream and are skipped while the stream is followed.
// Legacy: a page that still shows alert toasts itself sets ctx.shared 'alertToasts'; the shell then stays quiet.
const SHELL_NOTICE_RULES = new Set(['builtin-emergency', 'builtin-military', 'builtin-record']);
const SEV_RANK = { info: 0, warning: 1, critical: 2 };
const SEV_TOAST = { critical: 'error', warning: 'warn', info: 'info' };
let alertsFollowed = false;
let lastChime = 0;

const showAction = (icao) => (icao ? { label: 'Show', onClick: () => navigate('map/' + String(icao).toUpperCase()) } : undefined);

function alertToast(al) {
  const meta = [
    typeof al.altFt === 'number' ? units.alt(al.altFt, { fl: true }) : null,
    typeof al.distKm === 'number' ? units.dist(al.distKm) : null,
  ].filter(Boolean).join(' · ');
  // a default-template text already names altitude and distance; a rule's own message may not
  const text = units.alertText(al) + (meta && !al.auto ? ` (${meta})` : '');
  ui.toast(text, { type: SEV_TOAST[al.severity] || 'info', title: al.ruleName || 'Alert', action: showAction(al.icao),
    timeout: al.severity === 'critical' ? 20000 : 9000 });
}

function onAlerts(list) {
  if (shared.get('alertToasts')) return;
  // a long catch-up (e.g. after the laptop slept) shows only the newest few
  for (const al of list.filter((a) => a.notify && !SHELL_NOTICE_RULES.has(a.ruleId)).slice(-4)) alertToast(al);
  let sev = null;
  for (const al of list) if (al.sound && (sev === null || (SEV_RANK[al.severity] ?? 0) > (SEV_RANK[sev] ?? 0))) sev = al.severity || 'info';
  if (sev === null || document.visibilityState !== 'visible' || config.current?.alerts?.sound === false || alertSound.isMuted()) return;
  if (Date.now() - lastChime < 1500) return; // bursts: one chime
  lastChime = Date.now();
  alertSound.play(sev);
}

let alertsStarted = false;
function startAlerts() {
  alertsStarted = true;
  alertsFollowed = true;
  const f = live.followAlerts({ initial: 0, onAlerts, onOk() { alertsFollowed = true; } });
  // alerts engine missing (a 404 stops the follower): fall back to the engine's "alert" notices
  f.ready.then(() => { if (f.lastId === null) alertsFollowed = false; });
  // unlock the audio context on the first gesture anywhere in the app
  const unlock = () => {
    alertSound.unlock();
    window.removeEventListener('pointerdown', unlock, true);
    window.removeEventListener('keydown', unlock, true);
  };
  window.addEventListener('pointerdown', unlock, true);
  window.addEventListener('keydown', unlock, true);
}

function onNotice(n) {
  if (n.kind === 'alert' && (alertsFollowed || shared.get('alertToasts'))) return;
  const type = n.level === 'error' || n.level === 'alert' ? 'error' : n.level === 'warn' || n.level === 'warning' ? 'warn' : n.level === 'success' ? 'success' : 'info';
  const timeout = n.kind === 'emergency' ? 20000 : n.icao ? 9000 : undefined;
  const link = noticeLink(n, { settings: configAllowed() });
  ui.toast(units.noticeText(n), { type, action: link ? { label: link.label, href: link.href } : undefined, timeout });
}

// ---------------------------------------------------------------------------------------------
// page lifecycle
// ---------------------------------------------------------------------------------------------
let current = null; // { id, mod, container, ctx, cleanup: [], scope, abort, mounted }
let navSeq = 0;

function pageURL(id) { return new URL(`./pages/${id}.js`, import.meta.url).href; }

const pageModules = new Map(); // id -> module (successfully imported)

async function loadPage(id) {
  if (pageModules.has(id)) return pageModules.get(id);
  // Probe first so that "module not installed" (404, or a non-script fallback page) is told apart
  // from a module that exists but fails to load or evaluate - and without a failed import in the console.
  let missing = false;
  try {
    const r = await fetch(pageURL(id), { method: 'HEAD', cache: 'no-store' });
    const ct = r.headers.get('content-type') || '';
    missing = r.status === 404 || (r.ok && !/javascript|ecmascript/i.test(ct));
  } catch { /* server unreachable: let the import report it */ }
  if (missing) {
    const e = new Error(`pages/${id}.js is not installed`);
    e.missing = true;
    throw e;
  }
  const m = await import(pageURL(id));
  const mod = m.default;
  if (!mod || typeof mod.mount !== 'function') throw new Error(`pages/${id}.js has no default export with mount()`);
  pageModules.set(id, mod);
  return mod;
}

function teardown() {
  if (!current) return;
  const c = current;
  current = null;
  try { if (c.mounted && typeof c.mod.unmount === 'function') c.mod.unmount(); } catch (e) { console.error(`[app] ${c.id}.unmount failed`, e); }
  for (const fn of c.cleanup.splice(0).reverse()) { try { fn(); } catch (e) { console.error('[app] cleanup failed', e); } }
  c.scope.disposeAll();
  c.abort.abort();
}

function makeCtx(page, r, entry) {
  const ctx = {
    api, live, units, ui, charts: entry.scope, config, geo, shared,
    /** Current route: { id, args: [...], query: {...}, path } */
    route: r,
    /** Navigate to another route, e.g. ctx.navigate('map/4CA1FA'). */
    navigate,
    /** AbortSignal that fires when the page unmounts - pass to ctx.api calls. */
    signal: entry.abort.signal,
    /** Register a function to run on unmount (returns it). */
    onCleanup(fn) { if (typeof fn === 'function') entry.cleanup.push(fn); return fn; },
    /** live.on(...) that unsubscribes automatically on unmount. */
    subscribe(type, fn, opts) { const off = live.on(type, fn, opts); entry.cleanup.push(off); return off; },
    /** setInterval that is cleared on unmount. opts.immediate runs fn once right away. */
    every(ms, fn, opts = {}) {
      const id = setInterval(fn, ms);
      const stop = () => clearInterval(id);
      entry.cleanup.push(stop);
      if (opts.immediate) fn();
      return stop;
    },
    /** Change the title shown in the top bar and the browser tab. */
    setTitle(text) { setTitle(text || page.title); },
    /** Latest model.Status (or null). */
    get status() { return live.status; },
    /** The page descriptor { id, title, icon }. */
    page,
    /** Shell controls: kiosk mode (see `shell`). */
    shell,
    /** Network state (net/store.js): mode, scope, stations, view. */
    net,
    /** The station the page shows: { sid, name, scope, located, lat?, lon?, antennas, local } (DESIGN §11). */
    get station() { return net.station; },
  };
  return ctx;
}

function setTitle(text) {
  setText(el.title, text);
  document.title = text ? `${text} · AirView` : 'AirView';
}

function placeholder(page, kind, err) {
  const missing = kind === 'missing';
  return h('div.placeholder', h('div.placeholder-card',
    h('div.placeholder-icon', icon(missing ? page.icon : 'alert')),
    h('h2', page.title),
    missing
      ? [h('p', page.blurb), h('p', 'This module is not installed in this build (', h('code', `js/pages/${page.id}.js`), ').')]
      : [h('p', 'This page failed to load.'), h('p', h('code', String(err?.message || err)))],
    h('div.row.gap-3', { style: 'justify-content:center;margin-top:20px' },
      ui.button({ label: 'Open Live Map', icon: 'map', variant: 'primary', href: '#/map' }),
      !missing && ui.button({ label: 'Reload', icon: 'refresh', onClick: () => location.reload() }))));
}

async function route(force = false) {
  const r = parseHash();
  const seq = ++navSeq;
  const page = PAGES.find((p) => p.id === r.id);
  markActiveNav(r.id);

  if (!force && current && current.id === r.id && current.mounted) {
    current.ctx.route = r;
    if (typeof current.mod.onRoute === 'function') {
      try { current.mod.onRoute(r); } catch (e) { console.error(`[app] ${r.id}.onRoute failed`, e); }
      return;
    }
  }

  teardown();
  const container = h('div.page');
  mount(el.main, container);

  if (!page) {
    setTitle('Not found');
    mount(container, h('div.placeholder', h('div.placeholder-card', h('div.placeholder-icon', icon('help')), h('h2', 'Page not found'),
      h('p', 'There is no page at ', h('code', r.path), '.'), h('div', { style: 'margin-top:20px' }, ui.button({ label: 'Open Live Map', icon: 'map', variant: 'primary', href: '#/map' })))));
    return;
  }
  setTitle(page.title);
  if (page.net && !net.aggregator) {
    mount(container, h('div.placeholder', h('div.placeholder-card', h('div.placeholder-icon', icon(page.icon)), h('h2', page.title),
      h('p', page.blurb), h('p', 'This page belongs to network (aggregator) mode. This AirView runs as a single station.'),
      h('div', { style: 'margin-top:20px' }, ui.button({ label: 'Open Live Map', icon: 'map', variant: 'primary', href: '#/map' })))));
    return;
  }
  const acc = pageAccess(r.id, net);
  if (!acc.ok) { mount(container, accessPlaceholder(page, acc)); return; }
  const spinnerTimer = setTimeout(() => { if (seq === navSeq && !container.firstChild) mount(container, ui.loading('Loading ' + page.title + '…')); }, 180);

  let mod;
  try {
    mod = await loadPage(r.id);
  } catch (e) {
    clearTimeout(spinnerTimer);
    if (seq !== navSeq) return;
    if (!e.missing) console.error(`[app] failed to load page "${r.id}"`, e);
    mount(container, placeholder(page, e.missing ? 'missing' : 'error', e));
    return;
  }
  clearTimeout(spinnerTimer);
  if (seq !== navSeq) return;

  const entry = { id: r.id, mod, container, cleanup: [], scope: charts.scope(), abort: new AbortController(), mounted: false, ctx: null };
  entry.ctx = makeCtx(page, r, entry);
  current = entry;
  mount(container);
  container.classList.toggle('page--bleed', !!mod.fullBleed);
  container.dataset.page = r.id;
  if (mod.title) setTitle(mod.title);
  try {
    await mod.mount(container, entry.ctx);
    entry.mounted = true;
    if (seq !== navSeq && current === entry) { /* superseded while mounting: the newer route() tears it down */ }
  } catch (e) {
    console.error(`[app] ${r.id}.mount failed`, e);
    if (current === entry) {
      entry.mounted = true; // let unmount() release whatever was set up
      teardown();
      mount(container, placeholder(page, 'error', e));
    }
  }
}

/** Mount the current route again (after a login, logout or a change of the selected station). */
function remount() { route(true); }

// ---------------------------------------------------------------------------------------------
// boot
// ---------------------------------------------------------------------------------------------
function detectLowPower() {
  const q = new URLSearchParams(location.search);
  let pref = null;
  try { pref = localStorage.getItem('airview.lowpower'); } catch { /* ignore */ }
  if (q.has('lowpower')) return q.get('lowpower') !== '0';
  if (pref !== null) return pref === '1';
  const ua = navigator.userAgent || '';
  return /\b(armv\d|aarch64|Raspbian)\b/i.test(ua) && !/Android|iPhone|iPad/i.test(ua);
}

/** If this desktop window is configured as a thin client for another host, hand over to the remote shell. */
function maybeEnterRemoteShell() {
  const cfg = config.current;
  const st = live.status;
  if (!cfg || !st || !st.gui || cfg.mode !== 'remote-web' || window.top !== window) return;
  const host = (cfg.remote?.host || '').trim();
  if (!host) return;
  const port = cfg.remote.httpPort || 80;
  const target = `http://${host.includes(':') && !host.startsWith('[') ? `[${host}]` : host}${port === 80 ? '' : ':' + port}/`;
  location.replace('remote.html?url=' + encodeURIComponent(target));
}

function boot() {
  document.documentElement.classList.toggle('lowpower', detectLowPower());
  buildShell();

  units.onChange(() => {
    setText(el.unitsText, units.systemShort[units.system]);
    if (live.snapshot) renderCounts(live.snapshot.counts);
  });
  config.subscribe((cfg) => {
    units.setDefault(cfg?.ui?.units);
    if (!net.aggregator) setText(el.station, cfg?.receiver?.name || '');
    renderDayNight();
    maybeEnterRemoteShell();
  });

  live.on('aircraft', (snap) => renderCounts(snap.counts));
  let lastFlags = '';
  live.on('status', (st) => {
    renderStatus(st);
    // Another client (or the engine) may have changed the configuration: refresh our copy when key flags move.
    const flags = `${st.located}|${st.demo}|${st.mode}|${st.tileCache}`;
    if (lastFlags && flags !== lastFlags) config.load().catch(() => {});
    lastFlags = flags;
    maybeEnterRemoteShell();
  });
  live.on('state', renderLink);
  live.on('notice', onNotice);
  // admin.run() calls open the login dialog themselves: only unhandled 401s get the generic toast
  api.on('unauthorized', (e) => {
    if (adminCallActive() || e?.blocked) return;
    // network mode: pages explain refusals themselves; standalone keeps today's hint
    if (!net.aggregator) ui.toast.warn('This action needs the admin password.', { title: 'Locked' });
  });

  setInterval(renderClock, 1000);
  setInterval(renderDayNight, 60000);
  renderClock();

  window.addEventListener('hashchange', () => route());
  // The mode decides what the shell may ask for: GET /api/session first (network mode sets the request guard
  // and the station parameter before anything else goes out). Standalone: the same calls as always.
  net.init().then(() => {
    renderNav();
    renderNetChrome();
    renderBanners();
    net.on(onNetChange);
    live.start();
    if (configAllowed()) startAlerts();
    // Pages may rely on ctx.config.current being loaded; give it a moment, but never block the shell on it.
    const cfgReady = configAllowed() ? config.load().catch((e) => { console.warn('[app] config not available yet', e.message); }) : Promise.resolve();
    Promise.race([cfgReady, new Promise((r) => setTimeout(r, 2500))]).then(() => route());
    // Retry the config quietly if the first attempt failed (server still starting).
    const retry = setInterval(() => { if (config.current) clearInterval(retry); else if (configAllowed()) config.load().catch(() => {}); }, 5000);
  });
}

boot();
