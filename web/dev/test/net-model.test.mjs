// Rules of the network (aggregator) UI: modes, scopes, views, page access, station= parameter, the public
// request guard and the public payload scrubber. Run: node --test web/dev/test
import test from 'node:test';
import assert from 'node:assert/strict';
import * as M from '../../static/js/net/model.js';
import { EP, PUBLIC_UI_REQUESTS, normPublicSummary, normPublicStations, normCheck, normStation, normStationDetail, uptimeSegments } from '../../static/js/net/endpoints.js';
import { firstDataTime } from '../../static/js/reports/common.js';

const agg = (scope, station) => ({ mode: M.MODE_AGGREGATOR, scope, station: station || null });

test('mode detection', () => {
  assert.equal(M.detectMode(null, null), 'standalone');
  assert.equal(M.detectMode({ ok: true, admin: true }, null), 'standalone');
  assert.equal(M.detectMode({ ok: true, aggregator: true }, null), 'aggregator');
  assert.equal(M.detectMode(null, { mode: 'aggregator' }), 'aggregator');
  assert.equal(M.detectMode(null, { mode: 'remote-web' }), 'standalone');
});

test('scope of a session', () => {
  assert.equal(M.scopeOf(null), 'public');
  assert.equal(M.scopeOf({ loggedIn: false, scope: 'admin' }), 'public');
  assert.equal(M.scopeOf({ loggedIn: true, scope: 'owner' }), 'owner');
  assert.equal(M.scopeOf({ loggedIn: true, scope: 'admin', admin: true }), 'admin');
  // an admin session without the second factor is not admin
  assert.equal(M.scopeOf({ loggedIn: true, scope: 'admin', admin: false }), 'owner');
  assert.equal(M.scopeOf({ loggedIn: true, scope: 'weird' }), 'public');
});

test('views per scope', () => {
  assert.deepEqual(M.viewsFor('public'), ['network']);
  assert.deepEqual(M.viewsFor('owner'), ['network', 'mine', 'station']);
  assert.deepEqual(M.viewsFor('admin'), ['network', 'mine', 'station', 'admin']);
  assert.equal(M.normalizeView('admin', 'owner', true), 'network');
  assert.equal(M.normalizeView('mine', 'public', false), 'network');
  assert.equal(M.normalizeView('station', 'owner', false), 'mine');
  assert.equal(M.normalizeView('station', 'owner', true), 'station');
  assert.equal(M.normalizeView('bogus', 'admin', true), 'network');
  for (const v of ['network', 'mine', 'admin']) assert.equal(M.viewHidesReceiver(v), true);
  assert.equal(M.viewHidesReceiver('station'), false);
});

test('page access', () => {
  assert.deepEqual(M.pageAccess('settings', { mode: 'standalone' }), { ok: true });
  assert.equal(M.pageAccess('map', agg('public')).ok, true);
  assert.equal(M.pageAccess('check', agg('public')).ok, true);
  assert.equal(M.pageAccess('stats', agg('public')).need, 'login');
  assert.equal(M.pageAccess('settings', agg('owner')).need, 'admin');
  // single-station pages show the host station: admin only (the server answers them to the admin only)
  assert.equal(M.pageAccess('stats', agg('owner', { sid: 'x' })).need, 'admin');
  assert.equal(M.pageAccess('me', agg('owner')).ok, true);
  assert.equal(M.pageAccess('settings', agg('admin')).ok, true);
  assert.equal(M.pageAccess('unknown-page', agg('owner')).ok, false, 'unknown pages are admin-only');
  // every station-scoped page is hidden from the public
  for (const [id, rule] of Object.entries(M.PAGE_ACCESS)) if (rule !== 'public') assert.equal(M.pageAccess(id, agg('public')).ok, false, id);
});

test('navigation per scope', () => {
  assert.deepEqual(M.navFor('public'), ['map', 'network', 'feeder', 'check']);
  assert.deepEqual(M.navFor('owner'), ['map', 'network', 'feeder', 'check', 'me']);
  assert.ok(M.navFor('admin').includes('settings') && M.navFor('admin').includes('admin') && M.navFor('admin').includes('stats'));
  for (const id of M.navFor('admin')) if (id !== 'sep') assert.equal(M.pageAccess(id, agg('admin')).ok, true, id);
  for (const id of M.navFor('owner')) assert.equal(M.pageAccess(id, agg('owner')).ok, true, id);
});

test('station context', () => {
  const st = M.localStation({ receiver: { name: 'Home', located: true, lat: 51.5, lon: -0.1 }, antennas: [{ id: 'a' }] });
  assert.deepEqual(st, { sid: '', local: true, scope: 'local', name: 'Home', located: true, lat: 51.5, lon: -0.1, antennas: [{ id: 'a' }] });
  assert.equal(M.localStation(null).located, false);
  assert.equal(M.networkStation({}, 'owner'), null);
  const n = M.networkStation({ sid: 'abc', name: 'Roof', location: { lat: 1, lon: 2 } }, 'owner');
  assert.equal(n.located, true);
  assert.equal(n.lat, 1);
  assert.equal(M.networkStation({ sid: 'abc' }).located, false);
});

test('overlay config never carries a receiver outside the station view', () => {
  const cfg = { receiver: { name: 'Host', located: true, lat: 50, lon: 8 }, antennas: [{ id: 'x' }], ui: { units: 'metric' } };
  assert.equal(M.overlayConfig(cfg, { mode: 'standalone' }), cfg, 'standalone: unchanged object');
  for (const view of ['network', 'mine', 'admin']) {
    const o = M.overlayConfig(cfg, { mode: 'aggregator', view, station: { sid: 's', located: true, lat: 1, lon: 2 } });
    assert.equal(o.receiver.located, false, view);
    assert.deepEqual(o.antennas, []);
    assert.equal(o.ui.units, 'metric');
  }
  const o = M.overlayConfig(cfg, { mode: 'aggregator', view: 'station', station: { sid: 's', name: 'Roof', located: true, lat: 1, lon: 2, antennas: [] } });
  assert.deepEqual([o.receiver.lat, o.receiver.lon, o.receiver.name], [1, 2, 'Roof']);
  assert.equal(M.overlayConfig(null, { mode: 'aggregator', view: 'station', station: null }).receiver.located, false);
});

test('station= parameter only on station-scoped paths', () => {
  assert.equal(M.stationParam('/api/events', ''), '');
  for (const p of ['/api/events', '/api/aircraft', '/api/trails', '/api/aircraft/4CA1FA', '/api/me/aircraft']) assert.equal(M.stationParam(p, 'abc'), 'abc', p);
  for (const p of ['/api/stats/rates', '/api/me/stations', '/api/public/stats', '/api/admin/stations', '/api/auth/sessions', '/api/session', '/api/login', '/api/config', '/api/status', '/tilecache/x/y', 'relative']) {
    assert.equal(M.stationParam(p, 'abc'), '', p);
  }
  assert.equal(M.stationParam('/api/aircraft?station=zzz', 'abc'), '');
});

test('public guard', () => {
  for (const p of PUBLIC_UI_REQUESTS) assert.ok(M.publicAllowed(p), p);
  for (const p of ['/api/aircraft/4CA1FA', '/api/login', '/api/auth/register', '/api/navdata/route/BAW1']) assert.ok(M.publicAllowed(p), p);
  for (const p of ['/api/config', '/api/stats/range', '/api/terrain/coverage', '/api/weather/current', '/api/me/stations', '/api/admin/stations',
    '/api/timing/receiver', '/api/stats/heat', '/api/aircraft/notanicao', '/api/export/flight/1.kml']) {
    assert.equal(M.publicAllowed(p), false, p);
  }
});

test('public scrubber removes receiver data at any depth', () => {
  const dirty = { name: 'n', distKm: 4, nested: [{ bearing: 1, ok: 2, stations: ['a'], deeper: { ip: '192.0.2.1', feeds: ['x'], keep: true } }], uuid: 'u' };
  assert.deepEqual(M.scrubPublic(dirty), { name: 'n', nested: [{ ok: 2, deeper: { keep: true } }] });
  assert.equal(M.scrubPublic(null), null);
  assert.equal(M.scrubPublic(5), 5);
});

test('public pins', () => {
  assert.equal(M.publicPin({ pin: null }), null);
  assert.equal(M.publicPin({ pin: { precision: 'hidden', lat: 1, lon: 2 } }), null);
  assert.equal(M.publicPin({ pin: { precision: 'region' } }), null);
  assert.deepEqual(M.publicPin({ pin: { precision: 'cell01', lat: 1, lon: 2, extra: 9 } }), { lat: 1, lon: 2, precision: 'cell01' });
  assert.equal(M.publicPin({ pin: { precision: 'weird', lat: 1, lon: 2 } }).precision, 'region');
});

test('input normalisation', () => {
  assert.equal(M.normalizeClaimCode(' abcd-2345 '), 'ABCD2345');
  assert.equal(M.normalizeClaimCode('0I8x'), 'OIBX');
  assert.ok(M.validClaimCode('abcd 2345'));
  assert.ok(!M.validClaimCode('ABCD234'));
  assert.ok(!M.validClaimCode('ABCD2349'));
  assert.ok(M.validUUID('01234567-89ab-cdef-0123-456789abcdef'));
  assert.ok(!M.validUUID('1234'));
  assert.ok(M.validUsername('Alice.radio'));
  assert.ok(!M.validUsername('al'));
  assert.ok(!M.validUsername('a b c'));
  assert.equal(M.normalizeCode(' 123 456 '), '123456');
  assert.equal(M.normalizeCode('abcde-fghij'), 'ABCDE-FGHIJ');
  assert.equal(M.idTail('0123-4567-89ab-cdef'), '…abcdef');
  assert.equal(M.idTail(''), '');
  assert.equal(M.clockLabel('gps_utc'), 'GPS (UTC)');
  assert.equal(M.clockLabel(5), 'SDR 12 MHz');
  assert.equal(M.clockLabel(undefined), 'Unknown');
  assert.equal(M.groupSecret('ABCDEFGHIJ'), 'ABCD EFGH IJ');
  assert.equal(M.shortSid('abcdefghijklmnop'), 'abcd…nop');
});

test('otpauth parsing', () => {
  const o = M.parseOtpauth('otpauth://totp/AirView%20Net:alice?secret=JBSW&issuer=AirView%20Net&digits=6&period=30');
  assert.deepEqual(o, { label: 'AirView Net:alice', issuer: 'AirView Net', secret: 'JBSW', digits: 6, period: 30 });
  assert.equal(M.parseOtpauth('https://x'), null);
  assert.equal(M.parseOtpauth('otpauth://totp/Iss:bob?secret=X').issuer, 'Iss');
});

test('endpoint paths are encoded', () => {
  assert.equal(EP.myStation('a/b'), '/api/me/stations/a%2Fb');
  assert.equal(EP.adminTokenRevoke('t 1'), '/api/admin/tokens/t%201');
});

test('replay starts at the first recorded data', () => {
  const from = 1_000_000;
  const to = from + 3600e3;
  assert.equal(firstDataTime([{ first: from + 600e3 }, { first: from + 900e3 }], from, to, 10), from + 600e3 - 10e3);
  assert.equal(firstDataTime([{ first: Infinity }], from, to, 10), from, 'no data: window start');
  assert.equal(firstDataTime([], from, to, 10), from);
  assert.equal(firstDataTime([{ first: from + 5e3 }], from, to, 10), from, 'never before the window');
  assert.equal(firstDataTime([{ first: to + 1 }], from, to, 10), from);
});

test('normalisers accept the server shapes', () => {
  const sum = normPublicSummary({ now: 1, name: 'Net', approved: 10, online: 7, counts: { aircraft: 80, mlat: 9, msgRate: 5000 } });
  assert.deepEqual(sum, { name: 'Net', stationCounts: { total: 10, online: 7 }, aircraft: 80, mlatAircraft: 9, messagesPerSec: 5000, mlat: null });
  const st = normPublicStations({ entries: [{ sid: 'a', pin: { lat: 1, lon: 2, precision: 'region' }, stats: { online: true, msgRate: 3 } }, { name: 'x' }] });
  assert.equal(st.length, 2);
  assert.equal(st[0].online, true);
  assert.equal(st[1].online, false);
  assert.equal(st[1].stats, null);
  const links = normCheck({ links: [{ proto: 'beast', uuidTail: 'abcdef', warnings: ['no-uuid', 'uuid-mismatch'], kbps: 4, claim: { claimed: false, claimable: 'uuid', admission: 'pending' } }, { proto: 'avf', claim: { claimed: true, mine: true, sid: 's1' } }] });
  assert.equal(links[0].idTail, '…abcdef');
  assert.ok(links[0].uuidMissing && links[0].uuidMismatch && links[0].claimable);
  assert.equal(links[0].kBps, 4);
  assert.equal(links[0].admission, 'pending');
  assert.ok(links[1].yours && links[1].claimed && !links[1].claimable);
  assert.equal(links[1].sid, 's1');
  const n = normStation({ sid: 's', effective: { lat: 1, lon: 2, hae: 30, sigmaM: 5, source: 'manual' }, publicPin: { lat: 1.2, lon: 2.3, precision: 'region' }, ownerId: 4 });
  assert.deepEqual([n.lat, n.lon, n.alt, n.sigmaM, n.locSource, n.publicPoint.precision, n.owner], [1, 2, 30, 5, 'manual', 'region', 'account #4']);
  assert.equal(normStation({}), null);
  const d = normStationDetail({ station: { sid: 's', kind: 'avf', effective: { lat: 1, lon: 2, hae: 3, sigmaM: 4, source: 'gnss' } }, estimates: [{ method: 'survey', lat: 1, lon: 2, hae: 3, sigmaE: 3, sigmaN: 4 }], links: [{ proto: 'avf', software: 'f 1', kbps: 2, rttMs: 30 }], stats: { msgRate: 9, online: true }, lane: { lateFrames: 5, aircraft: 3 } });
  assert.equal(d.connection.version, 'f 1');
  assert.equal(d.connection.lateFrames, 5);
  assert.equal(d.stats.aircraft, 3);
  assert.equal(d.location.effective.source, 'gnss');
  assert.ok(Math.abs(d.location.estimates[0].sigmaM - Math.hypot(3, 4) / Math.SQRT2) < 1e-9);
});

test('uptime segments from the connection log', () => {
  const now = 10 * 86400e3;
  const day = 86400e3;
  const segs = uptimeSegments([{ on: now - 2 * day, off: now - day }, { on: now - 1.5 * day, off: now - 0.5 * day }, { on: now - 0.25 * day, off: 0 }], now, 7);
  assert.deepEqual(segs.map((x) => x.online), [false, true, false, true]);
  assert.equal(segs[1].from, now - 2 * day);
  assert.equal(segs[1].to, now - 0.5 * day);
  assert.equal(segs[3].to, now);
  assert.deepEqual(uptimeSegments([], now, 1), [{ from: now - day, to: now, online: false }]);
});

test('feeder page snippets', () => {
  const join = { network: true, host: '', avfPort: 30160, beastPort: 30004, mlatPort: 31090, admission: 'invite' };
  assert.equal(M.snippetHost(join, { hostname: 'feed.example.net' }), 'feed.example.net');
  assert.equal(M.snippetHost({ host: 'agg.example' }, { hostname: 'x' }), 'agg.example');
  assert.equal(M.snippetHost({ host: '2001:db8::1' }, {}), '[2001:db8::1]');
  assert.equal(M.snippetHost({ host: 'bad host"<x>' }, { hostname: 'ok.example' }), 'ok.example');
  const s = M.feederSnippets(join, 'agg.example');
  assert.equal(s.avf[0], 'airview-feeder setup --server agg.example:30160 --enrol <token from the operator>');
  assert.equal(s.readsb, '--net-connector agg.example,30004,beast_reduce_plus_out,uuid=<your-uuid>');
  assert.match(s.mlatclient, /--server agg\.example:31090 /);
  assert.equal(s.ultrafeederMlat, 'mlat,agg.example,31090,uuid=<your-uuid>');
  const closed = M.feederSnippets({ avfPort: 30160, beastPort: 0, mlatPort: 0, admission: 'open' }, 'h');
  assert.deepEqual(Object.keys(closed), ['avf']);
  assert.ok(!closed.avf[0].includes('--enrol'));
});

test('C6 notice text', () => {
  const T = "We measured your station's position to about {σ} m and now use it for MLAT. It differs from the position you entered by {Δ} m.";
  const dist = () => 0.4123;
  const eff = { source: 'estimated', sigmaM: 12.4, lat: 50, lon: 8 };
  assert.equal(M.surveyNoticeText(eff, { lat: 50.001, lon: 8 }, T, dist), "We measured your station's position to about 12 m and now use it for MLAT. It differs from the position you entered by 412 m.");
  assert.equal(M.surveyNoticeText(eff, null, T, dist), "We measured your station's position to about 12 m and now use it for MLAT.");
  assert.equal(M.surveyNoticeText({ ...eff, source: 'manual' }, null, T, dist), '');
  assert.equal(M.surveyNoticeText({ ...eff, sigmaM: null }, null, T, dist), '');
});