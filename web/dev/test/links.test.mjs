// Shell notice / banner / toast links and the receiver-agnostic helpers (settings/links.js).
import test from 'node:test';
import assert from 'node:assert/strict';
import * as L from '../../static/js/settings/links.js';
import * as M from '../../static/js/settings/model.js';

const sectionIds = new Set(M.SECTIONS.map((s) => s.id));
const PAGE_IDS = new Set(['map', 'guess', 'setup', 'settings']);

/** Every hash a link produces must resolve to a real page and, for Settings, a real section. */
function assertRoutable(href) {
  assert.match(href, /^#\/[a-z]/, href);
  const segs = href.slice(2).split('/');
  assert.ok(PAGE_IDS.has(segs[0]), `unknown page in ${href}`);
  if (segs[0] === 'settings') assert.ok(segs[1] === undefined || sectionIds.has(segs[1]), `unknown settings section in ${href}`);
}

test('every banner link targets a routable page / settings section', () => {
  for (const [k, l] of Object.entries(L.BANNER_LINKS)) {
    assert.ok(l.label, k);
    assertRoutable(l.href);
  }
  assert.equal(L.BANNER_LINKS.feeds.href, '#/settings/feeds');
  assert.equal(L.BANNER_LINKS.nofeeds.href, '#/settings/feeds');
  assert.equal(L.BANNER_LINKS.loc.href, '#/settings/station');
  assert.equal(L.BANNER_LINKS.demo.href, '#/settings/demo');
  assert.equal(L.BANNER_LINKS.locGuess.href, '#/guess');
  assert.equal(L.BANNER_LINKS.routesOnline.href, '#/settings/navdata');
  assert.match(L.ROUTES_ONLINE_NOTE, /adsbdb\.com/);
  assert.match(L.ROUTES_ONLINE_NOTE, /location is not/);
});

test('settingsHref ignores unknown sections', () => {
  assert.equal(L.settingsHref('feeds'), '#/settings/feeds');
  assert.equal(L.settingsHref('nope'), '#/settings');
  assert.equal(L.settingsHref(), '#/settings');
});

test('sectionForPath maps config paths to sections', () => {
  assert.equal(L.sectionForPath('feeds[0].host: bad'), 'feeds');
  assert.equal(L.sectionForPath('feeds.0.port'), 'feeds');
  assert.equal(L.sectionForPath('receiver.lat out of range'), 'station');
  assert.equal(L.sectionForPath('ntp.server'), 'time');
  assert.equal(L.sectionForPath('http.port'), 'web');
  assert.equal(L.sectionForPath('airview.jsonc contains comments'), null);
  assert.equal(L.sectionForPath(''), null);
});

test('noticeLink: every notice kind gets a working action', () => {
  const cases = [
    [{ level: 'alert', kind: 'emergency', icao: '4ca1fa', text: 'Squawk 7700' }, 'Show', '#/map/4CA1FA'],
    [{ level: 'info', kind: 'record', icao: '3c6444', text: 'New record' }, 'Show', '#/map/3C6444'],
    [{ level: 'warn', kind: 'alert', icao: 'abc123', text: 'Rule hit' }, 'Show', '#/map/ABC123'],
    [{ level: 'warn', kind: 'feed_down', text: 'Feed "radarcape" disconnected: lookup radarcape: no such host' }, 'Check feeds', '#/settings/feeds'],
    [{ level: 'warn', text: 'Configuration: feeds[0].port: out of range' }, 'Open settings', '#/settings/feeds'],
    [{ level: 'warn', text: 'Configuration: airview.jsonc contains comments' }, 'Open settings', '#/settings'],
    [{ level: 'warn', text: 'Aircraft database import failed: bad file' }, 'Aviation data', '#/settings/navdata'],
    [{ level: 'warn', text: 'Reference data import failed: x' }, 'Aviation data', '#/settings/navdata'],
    [{ level: 'warn', text: 'Uplinks cannot start: x' }, 'Uplinks', '#/settings/uplinks'],
    [{ level: 'warn', text: 'Data port 30154 could not be started: in use' }, 'Connection', '#/settings/connection'],
    [{ level: 'info', text: 'Ground elevation set to 12 m from the terrain model.' }, 'Station', '#/settings/station'],
  ];
  for (const [n, label, href] of cases) {
    const l = L.noticeLink(n);
    assert.ok(l, n.text);
    assert.equal(l.label, label, n.text);
    assert.equal(l.href, href, n.text);
    assertRoutable(l.href);
  }
  assert.equal(L.noticeLink({ level: 'info', kind: 'feed_up', text: 'Feed "x" connected (a:1)' }), null);
  assert.equal(L.noticeLink(null), null);
});

test('noticeLink: no Settings links for viewers without configuration access', () => {
  assert.equal(L.noticeLink({ kind: 'feed_down', text: 'Feed down' }, { settings: false }), null);
  assert.equal(L.noticeLink({ text: 'Configuration: ntp.server' }, { settings: false }), null);
  assert.equal(L.noticeLink({ kind: 'emergency', icao: 'abc123' }, { settings: false }).href, '#/map/ABC123');
});

const placeholder = { id: 'radarcape', name: 'Radarcape EVO (Beast 10003)', enabled: true, type: 'beast', host: 'radarcape', port: 10003 };

test('hasRadarcape: the never-connected factory placeholder does not count', () => {
  assert.equal(L.hasRadarcape({ feeds: [placeholder] }), false);
  assert.equal(L.hasRadarcape({ feeds: [placeholder] }, { feeds: [{ id: 'radarcape', connected: false }] }), false);
  assert.equal(L.hasRadarcape({ feeds: [placeholder] }, { feeds: [{ id: 'radarcape', connected: true }] }), true);
  assert.equal(L.hasRadarcape({ feeds: [{ ...placeholder, host: '10.0.0.5', name: 'Radarcape' }] }), true);
  assert.equal(L.hasRadarcape({ feeds: [{ id: 'r', name: 'x', type: 'json', url: 'http://radarcape/aircraftlist.json' }] }), true);
  assert.equal(L.hasRadarcape({ feeds: [{ id: 'readsb', name: 'readsb', type: 'beast', host: '127.0.0.1', port: 30005 }] }), false);
  assert.equal(L.hasRadarcape({ feeds: [{ id: 'x', name: 'x', host: 'h' }] }, { feeds: [{ id: 'x', gpsTime: true }] }), true);
  assert.equal(L.hasRadarcape({ feeds: [] }, { receiverGps: { lat: 1, lon: 2 } }), true);
  assert.equal(L.hasRadarcape(null, null), false);
});

test('onlyFactoryPlaceholder spots the first-start state from /api/status feeds', () => {
  const st = { id: 'radarcape', name: 'Radarcape EVO (Beast 10003)', type: 'beast', addr: 'radarcape:10003', enabled: true, connected: false };
  assert.equal(L.onlyFactoryPlaceholder([st]), true);
  assert.equal(L.onlyFactoryPlaceholder([{ ...st, connected: true }]), false);
  assert.equal(L.onlyFactoryPlaceholder([st, { id: 'readsb', type: 'beast', addr: '127.0.0.1:30005', enabled: true, connected: false }]), false);
  assert.equal(L.onlyFactoryPlaceholder([{ ...st, addr: '10.1.1.1:10003' }]), false);
  assert.equal(L.onlyFactoryPlaceholder([]), false);
});

test('feed presets: RTL-SDR / readsb / dump1090 first, Radarcape after, all valid types', () => {
  const types = new Set(M.FEED_TYPES.map((t) => t.value));
  const firstRc = M.FEED_PRESETS.findIndex((p) => p.radarcape);
  assert.ok(firstRc > 0);
  assert.ok(M.FEED_PRESETS.slice(0, firstRc).every((p) => !/radarcape/i.test(p.label + p.feed.name + (p.feed.host || '') + (p.feed.url || ''))));
  assert.ok(M.FEED_PRESETS.slice(firstRc).filter((p) => /radarcape/i.test(p.label)).every((p) => p.radarcape));
  for (const p of M.FEED_PRESETS) assert.ok(types.has(p.feed.type), p.label);
  assert.equal(M.FEED_PRESETS[0].feed.port, 30005);
  assert.equal(L.LOCAL_DECODER_FEED.host, '127.0.0.1');
  assert.equal(L.LOCAL_DECODER_FEED.port, 30005);
});

test('isFactoryDefaultFeed matches the new and the legacy factory feed only', () => {
  assert.equal(L.isFactoryDefaultFeed({ id: 'beast', type: 'beast', host: 'localhost', port: 30005 }), true);
  assert.equal(L.isFactoryDefaultFeed({ id: 'radarcape', type: 'beast', host: 'radarcape', port: 10003 }), true);
  assert.equal(L.isFactoryDefaultFeed({ id: 'beast', type: 'beast', host: '192.0.2.5', port: 30005 }), false);
  assert.equal(L.isFactoryDefaultFeed({ id: 'beast', type: 'sbs', host: 'localhost', port: 30005 }), false);
  assert.equal(L.isFactoryDefaultFeed(null), false);
});
