// Network (aggregator) mode for the UI mock server: accounts with sessions, CSRF and TOTP, the public /
// owner / admin routes the network UI calls (web/static/js/net/endpoints.js) and the per-viewer projection
// of the live picture. Fixture data only - no real station, address or person.
//
// Enabled by `node web/dev/mock-server.mjs --aggregator [--setup]`:
//   accounts   admin / admin-password-1 (authenticator: any code "123456"; recovery code AAAAA-BBBBB)
//              alice / alice-password (owner of two stations, no authenticator)
//   --setup    start without an administrator: GET /api/auth/setup says required, the console token is MOCKTOKEN
//   claim      code ABCD2345 (native) or UUID 0123456789abcdef0123456789abcdef from 127.0.0.1 (compat)

import { randomBytes, createHash } from 'node:crypto';

const COOKIE = 'airview_session';
const TOTP_OK = '123456';
const RECOVERY = 'AAAAA-BBBBB';
const FORBIDDEN_PUBLIC = ['distKm', 'bearing', 'elevation', 'slantKm', 'rssi', 'rssiAvg', 'antennas', 'feeds', 'cpaKm', 'cpaInS', 'terrainVisible', 'losMarginDeg', 'seenBy', 'stations', 'mine'];

export function createNetworkMock({ center, fleetIcaos, readBody, json, setupRequired = false }) {
  const [clat, clon] = center();
  const now0 = Date.now();
  const accounts = new Map([
    ['alice', { id: 2, username: 'alice', password: 'alice-password', scope: 'owner', totp: false }],
  ]);
  if (!setupRequired) accounts.set('admin', { id: 1, username: 'admin', password: 'admin-password-1', scope: 'admin', totp: true });
  const sessions = new Map(); // token -> { id, user, scope, csrf, mfa, created, lastSeen, expires }
  let pendingTotp = null;

  // ---- stations (fixture) ----
  const mk = (i, name, owner, extra = {}) => {
    const ang = (i * 47) % 360;
    const km = 25 + ((i * 37) % 160);
    const lat = clat + (km / 111) * Math.cos((ang * Math.PI) / 180);
    const lon = clon + (km / (111 * Math.cos((clat * Math.PI) / 180))) * Math.sin((ang * Math.PI) / 180);
    const sid = 'st' + createHash('sha256').update('mock' + i).digest('hex').slice(0, 14);
    return {
      sid, name, owner, kind: i % 3 === 0 ? 'beast' : 'avf', admission: 'approved', online: i % 5 !== 4, lastSeen: Date.now() - (i % 5 === 4 ? 7200e3 : 2000),
      clockClass: i % 3 === 0 ? 'sdr12' : i === 1 ? 'gps_utc' : 'hw12', feederId: i % 3 === 0 ? '' : 'fdr' + sid.slice(2, 10),
      lat: +lat.toFixed(5), lon: +lon.toFixed(5), alt: 40 + i * 3, locStatus: i === 2 ? 'estimated' : 'declared', locSource: 'manual', sigmaM: i === 2 ? 38 : 5,
      pin: 'hidden', publicStats: false, showName: false,
      stats: { msgRate: 300 + i * 41, aircraft: 20 + i, uptimePct: 90 + (i % 9), aircraftDay: 600 + i * 13, positions: 200000 + i * 9000, mlatContrib: 4000 + i * 100, onlyMe24h: i % 4, posRate: 50 + i },
      ...extra,
    };
  };
  const stations = [
    mk(1, 'Host EVO 2CH', 'admin', { kind: 'host', pin: 'region', publicStats: true, showName: true }),
    mk(2, 'Alice Roof', 'alice', { pin: 'cell01', publicStats: true, showName: true }),
    mk(3, 'Alice Garage', 'alice', { admission: 'pending' }),
    mk(4, 'Club North', null, { pin: 'region', publicStats: true, showName: false }),
    mk(5, 'Unclaimed Pi', null, {}),
    mk(6, 'Hilltop', null, { publicStats: true, showName: true }),
    mk(7, 'Coast', null, { pin: 'cell01' }),
    mk(8, 'Valley', null, { admission: 'denied' }),
  ];
  // stored public point: cell origin + keyed offset (never the station position)
  const cellPoint = (s) => {
    if (s.pin === 'hidden') return null;
    const cell = s.pin === 'cell01' ? 0.1 : 0.5;
    const h = createHash('sha256').update('pin' + s.sid).digest();
    const oLat = Math.floor(s.lat / cell) * cell;
    const oLon = Math.floor(s.lon / cell) * cell;
    return { lat: +(oLat + (h[0] / 255) * cell).toFixed(4), lon: +(oLon + (h[1] / 255) * cell).toFixed(4), precision: s.pin };
  };
  const tokens = [{ id: 't1', label: 'Club member Smith', uses: 0, maxUses: 1, created: now0 - 86400e3, expires: now0 + 6 * 86400e3, revoked: false }];
  const audit = [{ t: now0 - 3600e3, actor: 'admin', action: 'login', target: '', detail: '' }];
  const addAudit = (actor, action, target, detail = '') => audit.unshift({ t: Date.now(), actor, action, target, detail });

  // ---- sessions ----
  function cookieOf(req) {
    const m = new RegExp(`(?:^|;\\s*)${COOKIE}=([^;]+)`).exec(req.headers.cookie || '');
    return m ? m[1] : '';
  }
  function viewer(req) {
    const s = sessions.get(cookieOf(req));
    if (!s || s.expires < Date.now()) return { scope: 'public', user: null };
    s.lastSeen = Date.now();
    return { scope: s.scope, user: s.user, session: s };
  }
  function info(v) {
    if (v.scope === 'public') return { ok: true, admin: false, loopback: false, passwordSet: true, loggedIn: false, aggregator: true, scope: 'public', setupRequired: !accounts.has('admin') };
    const acct = accounts.get(v.user);
    return { ok: true, admin: v.scope === 'admin', loopback: false, passwordSet: true, loggedIn: true, aggregator: true, scope: v.scope, username: v.user, totp: !!acct?.totp, csrfToken: v.session.csrf, setupRequired: !accounts.has('admin') };
  }
  function newSession(res, acct) {
    const token = randomBytes(24).toString('hex');
    const s = { id: createHash('sha256').update(token).digest('hex'), user: acct.username, scope: acct.scope, csrf: randomBytes(16).toString('hex'), created: Date.now(), lastSeen: Date.now(), expires: Date.now() + 30 * 86400e3 };
    sessions.set(token, s);
    res.setHeader('Set-Cookie', `${COOKIE}=${token}; Path=/; HttpOnly; SameSite=Strict; Max-Age=${30 * 86400}`);
    return { scope: s.scope, user: s.user, session: s };
  }
  const body = async (req) => { try { return JSON.parse((await readBody(req)) || '{}'); } catch { return {}; } };

  // ---- DTOs (shapes of internal/server api_me.go / api_admin.go / privacy/publicdto) ----
  const owners = { admin: 1, alice: 2 };
  const stationDTO = (s, admin = false) => ({
    sid: s.sid, name: s.name, kind: s.kind, feederId: s.feederId || undefined, rxNo: 0, clockClass: s.clockClass, admission: s.admission,
    located: true, locStatus: s.locStatus, effective: { lat: s.lat, lon: s.lon, hae: s.alt, sigmaM: s.sigmaM, source: s.locSource },
    declared: { lat: s.lat, lon: s.lon, alt: s.alt, altRef: 'msl' }, gnss: s.clockClass === 'gps_utc' ? { lat: s.lat, lon: s.lon, alt: s.alt } : undefined,
    pin: s.pin, showName: s.showName, publicStats: s.publicStats, publicPin: cellPoint(s), anchor: s.clockClass === 'gps_utc',
    ...(admin ? { ownerId: owners[s.owner] || undefined, trust: 3 } : {}),
    created: now0 - 30 * 86400e3, lastSeen: s.lastSeen, online: s.online,
  });
  const liveStats = (s) => ({ online: s.online, msgRate: s.stats.msgRate, aircraft: s.stats.aircraft, positions: s.stats.positions, uptimePct: s.stats.uptimePct });
  const lane = (s) => ({ frames: s.stats.positions * 9, positions: s.stats.positions, lateFrames: 12, aircraft: s.stats.aircraft, firstMs: now0 - 5 * 3600e3, lastMs: Date.now() });
  const linksOf = (s) => (s.online ? [{ proto: s.kind === 'host' ? 'avf' : s.kind, uuidTail: s.sid.slice(-6), since: Date.now() - 5 * 3600e3, msgRate: s.stats.msgRate, posRate: s.stats.posRate, kbps: 5.2, rttMs: 38, clockClass: s.clockClass, mlatSynced: s.kind !== 'beast', software: s.kind === 'beast' ? 'readsb' : 'airview-feeder 0.9.0' }] : []);
  const detail = (s, admin = false) => ({
    station: stationDTO(s, admin),
    estimates: [{ t: Date.now() - 4 * 3600e3, method: 'survey', lat: s.lat + 0.0002, lon: s.lon - 0.0003, hae: s.alt + 4, sigmaE: 18, sigmaN: 25, sigmaU: 30, n: 412, status: 'stable' },
      { t: Date.now() - 86400e3, method: 'locateme', lat: s.lat + 0.03, lon: s.lon + 0.02, hae: 0, sigmaE: 4000, sigmaN: 4000, sigmaU: 0, n: 900, status: 'coarse' }],
    links: linksOf(s), stats: liveStats(s), lane: lane(s),
    ...(admin ? { liveLinks: linksOf(s).map((l) => ({ ...l, ip: '192.0.2.10', sid: s.sid })) } : {}),
  });
  const connections = (s) => Array.from({ length: 6 }, (_, i) => ({ on: Date.now() - (6 - i) * 26 * 3600e3, off: i === 5 ? 0 : Date.now() - (6 - i) * 26 * 3600e3 + 20 * 3600e3, ipHash: createHash('sha256').update('ip' + i).digest('hex').slice(0, 16), proto: s.kind === 'host' ? 'avf' : s.kind, bytes: 1e8 + i * 1e7 }));
  const publicEntry = (s) => ({
    ...(s.pin !== 'hidden' ? { sid: s.sid } : {}), ...(s.showName ? { name: s.name } : {}), ...(cellPoint(s) ? { pin: cellPoint(s) } : {}),
    ...(s.publicStats ? { stats: { online: s.online, msgRate: s.stats.msgRate, aircraft: s.stats.aircraft, positions: s.stats.positions, uptimePct: s.stats.uptimePct } } : {}),
  });

  // ---- projection of the live picture ----
  const mineSet = new Set(fleetIcaos().filter((_, i) => i % 3 === 0));
  function projectSnapshot(snap, v) {
    if (!snap) return snap;
    const owned = stations.filter((s) => s.owner === v.user).map((s) => s.sid);
    const aircraft = snap.aircraft.map((a) => {
      const o = { ...a };
      if (o.source === 'mlat') { o.mlatGrade = ['A', 'B', 'C'][parseInt(o.icao, 16) % 3]; o.mlatSigmaM = [40, 120, 420][parseInt(o.icao, 16) % 3]; }
      if (v.scope === 'public') { for (const k of FORBIDDEN_PUBLIC) delete o[k]; return o; }
      delete o.feeds; delete o.antennas;
      if (v.scope === 'admin') { o.stations = stations.filter((_, i) => (parseInt(o.icao, 16) + i) % 3 === 0).map((s) => s.sid); }
      if (mineSet.has(o.icao) && owned.length) { o.mine = true; o.stations = v.scope === 'admin' ? o.stations : owned.slice(0, 1); }
      return o;
    });
    const counts = { ...snap.counts };
    if (v.scope === 'public') delete counts.maxRangeKm;
    return { ...snap, aircraft, counts };
  }
  function projectStatus(st, v) {
    if (v.scope === 'admin') return { ...st, mode: 'aggregator' };
    return { app: st.app, version: st.version, platform: st.platform, gui: false, now: st.now, started: st.started, mode: 'aggregator', counts: { ...st.counts, maxRangeKm: undefined }, demo: false, tileCache: st.tileCache, located: false };
  }

  // ---- routes ----
  async function handle(req, res, u) {
    const p = u.pathname;
    const m = req.method;
    let v = viewer(req);
    const isWrite = m !== 'GET' && m !== 'HEAD';
    const csrfFree = ['/api/login', '/api/logout', '/api/auth/register', '/api/auth/setup/begin', '/api/auth/setup/finish'];
    if (isWrite && p.startsWith('/api/') && !csrfFree.includes(p) && v.scope !== 'public' && req.headers['x-csrf-token'] !== v.session.csrf) {
      return json(res, 403, { error: 'CSRF token missing or wrong' }), true;
    }
    const need = (scope) => {
      if (v.scope === 'public') { json(res, 401, { error: 'login required' }); return false; }
      if (scope === 'admin' && v.scope !== 'admin') { json(res, 403, { error: 'not allowed for this account' }); return false; }
      return true;
    };
    const st = (sid) => stations.find((s) => s.sid === sid);
    const own = (sid) => { const s = st(sid); return s && (s.owner === v.user || v.scope === 'admin') ? s : null; };

    if (p === '/api/session' && m === 'GET') return json(res, 200, info(v)), true;
    if (p === '/api/login' && m === 'POST') {
      const b = await body(req);
      const acct = accounts.get(String(b.username || '').toLowerCase());
      if (!acct || acct.password !== b.password) return json(res, 401, { error: 'wrong user name or password' }), true;
      if (acct.totp && !b.code) return json(res, 401, { error: 'authenticator code required', ...info({ scope: 'public' }), codeRequired: true }), true;
      let recoveryLeft;
      if (acct.totp && b.code !== TOTP_OK) {
        if (b.code === RECOVERY) recoveryLeft = 9; else return json(res, 401, { error: 'wrong code' }), true;
      }
      v = newSession(res, acct);
      addAudit(acct.username, 'login', '');
      return json(res, 200, { ...info(v), ...(recoveryLeft !== undefined ? { recoveryLeft } : {}) }), true;
    }
    if (p === '/api/logout' && m === 'POST') {
      sessions.delete(cookieOf(req));
      res.setHeader('Set-Cookie', `${COOKIE}=; Path=/; Max-Age=0`);
      return json(res, 200, info({ scope: 'public' })), true;
    }
    if (p === '/api/config' && (m === 'GET' || m === 'PUT')) {
      if (!need('admin')) return true;
      return false; // the base mock answers; the shell only asks as admin
    }
    // ---- accounts ----
    if (p === '/api/auth/setup' && m === 'GET') return json(res, 200, { required: !accounts.has('admin'), tokenPending: !accounts.has('admin') }), true;
    if (p === '/api/auth/setup/begin' && m === 'POST') {
      const b = await body(req);
      if (accounts.has('admin')) return json(res, 409, { error: 'setup is already complete' }), true;
      if (b.token !== 'MOCKTOKEN') return json(res, 401, { error: 'wrong setup token' }), true;
      if (String(b.password || '').length < 12) return json(res, 400, { error: 'the password needs at least 12 characters' }), true;
      pendingTotp = { username: b.username, password: b.password };
      return json(res, 200, { secret: 'JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP', uri: `otpauth://totp/AirView%20Mock:${encodeURIComponent(b.username)}?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=AirView%20Mock&algorithm=SHA1&digits=6&period=30` }), true;
    }
    if (p === '/api/auth/setup/finish' && m === 'POST') {
      const b = await body(req);
      if (!pendingTotp || b.token !== 'MOCKTOKEN') return json(res, 401, { error: 'wrong setup token' }), true;
      if (b.code !== TOTP_OK) return json(res, 401, { error: 'wrong code' }), true;
      accounts.set(pendingTotp.username, { id: 1, username: pendingTotp.username, password: pendingTotp.password, scope: 'admin', totp: true });
      pendingTotp = null;
      return json(res, 200, { recoveryCodes: Array.from({ length: 10 }, (_, i) => `R${i}AAA-BBBB${i}`) }), true;
    }
    if (p === '/api/auth/register' && m === 'POST') {
      const b = await body(req);
      const name = String(b.username || '').toLowerCase();
      if (!/^[a-z0-9._-]{3,32}$/.test(name)) return json(res, 400, { error: 'invalid user name' }), true;
      if (accounts.has(name)) return json(res, 409, { error: 'user name taken' }), true;
      if (String(b.password || '').length < 8) return json(res, 400, { error: 'password too short' }), true;
      const acct = { id: accounts.size + 10, username: name, password: b.password, scope: 'owner', totp: false };
      accounts.set(name, acct);
      v = newSession(res, acct);
      return json(res, 200, info(v)), true;
    }
    if (p.startsWith('/api/auth/') && p !== '/api/auth/setup') {
      if (!need('owner')) return true;
      const acct = accounts.get(v.user);
      const b = isWrite ? await body(req) : {};
      if (p === '/api/auth/password') { if (b.current !== acct.password) return json(res, 401, { error: 'wrong password' }), true; acct.password = b.new; return json(res, 200, { ok: true }), true; }
      if (p === '/api/auth/totp/begin') { if (b.password !== acct.password) return json(res, 401, { error: 'wrong password' }), true; return json(res, 200, { secret: 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ', uri: `otpauth://totp/AirView%20Mock:${acct.username}?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=AirView%20Mock` }), true; }
      if (p === '/api/auth/totp/confirm') { if (b.code !== TOTP_OK) return json(res, 401, { error: 'wrong code' }), true; acct.totp = true; return json(res, 200, { recoveryCodes: Array.from({ length: 10 }, (_, i) => `Q${i}ZZZ-YYYY${i}`) }), true; }
      if (p === '/api/auth/totp/disable') { if (acct.scope === 'admin') return json(res, 403, { error: 'the administrator needs the authenticator' }), true; if (b.password !== acct.password || ![TOTP_OK, RECOVERY].includes(b.code)) return json(res, 401, { error: 'wrong password or code' }), true; acct.totp = false; return json(res, 200, { ok: true }), true; }
      if (p === '/api/auth/recovery-codes') { if (b.password !== acct.password || b.code !== TOTP_OK) return json(res, 401, { error: 'wrong password or code' }), true; return json(res, 200, { recoveryCodes: Array.from({ length: 10 }, (_, i) => `N${i}NNN-MMMM${i}`) }), true; }
      if (p === '/api/auth/sessions') {
        const list = [...sessions.values()].filter((s) => s.user === v.user).map((s) => ({ id: s.id.slice(0, 12), scope: s.scope, created: s.created, lastSeen: s.lastSeen, expires: s.expires, current: s === v.session }));
        return json(res, 200, { sessions: list }), true;
      }
      if (p === '/api/auth/logout-others') { let n = 0; for (const [k, s] of sessions) if (s.user === v.user && s !== v.session) { sessions.delete(k); n++; } return json(res, 200, { ended: n }), true; }
      return json(res, 404, { error: 'unknown account route' }), true;
    }
    // ---- public ----
    const approved = () => stations.filter((s) => s.admission === 'approved');
    if (p === '/api/public/stats' && m === 'GET') {
      return json(res, 200, { now: Date.now(), name: 'AirView Mock Network', approved: approved().length, online: approved().filter((s) => s.online).length, counts: { aircraft: fleetIcaos().length, withPos: fleetIcaos().length - 5, adsb: 60, mlat: 9, tisb: 2, modes: 5, msgRate: 5230, posRate: 300, predicted: 1 } }), true;
    }
    if (p === '/api/public/stations' && m === 'GET') return json(res, 200, { entries: approved().filter((s) => s.publicStats || s.pin !== 'hidden').map(publicEntry), approved: approved().length, online: approved().filter((s) => s.online).length }), true;
    if (p === '/api/me/check' && m === 'GET') {
      const st5 = stations[4];
      return json(res, 200, { now: Date.now(), matched: true, links: [
        { proto: 'avf', uuidTail: st5.feederId.slice(-6), since: Date.now() - 3 * 3600e3, msgRate: 412.5, posRate: 61.2, kbps: 5.1, rttMs: 42, clockClass: 'hw12', mlatSynced: true, software: 'airview-feeder 0.9.0', warnings: [],
          claim: { claimed: !!st5.owner, mine: st5.owner === v.user, claimable: st5.owner ? undefined : 'code', admission: st5.admission, ...(st5.owner === v.user ? { sid: st5.sid } : {}) } },
        { proto: 'beast', since: Date.now() - 600e3, msgRate: 380.1, posRate: 55, kbps: 4.1, clockClass: 'sdr12', warnings: ['no-uuid'], claim: { claimed: false, claimable: 'admin', admission: 'pending' } },
      ] }), true;
    }
    // ---- owner ----
    if (p.startsWith('/api/me/')) {
      if (!need('owner')) return true;
      if (p === '/api/me/stations' && m === 'GET') return json(res, 200, { stations: stations.filter((s) => s.owner === v.user).map((s) => stationDTO(s)) }), true;
      if (p === '/api/me/claim' && m === 'POST') {
        const b = await body(req);
        const code = String(b.code || '').toUpperCase().replace(/[^A-Z2-7]/g, '');
        const target = code === 'ABCD2345' ? stations[4] : String(b.uuid || '') === '0123456789abcdef0123456789abcdef' ? stations[6] : null;
        if (!target) return json(res, 400, { error: 'unknown or expired claim code' }), true;
        if (target.owner && target.owner !== v.user) return json(res, 409, { error: 'this station already belongs to another account' }), true;
        target.owner = v.user;
        addAudit(v.user, 'station.claim', target.sid);
        return json(res, 200, stationDTO(target)), true;
      }
      if (p === '/api/me/export' && m === 'GET') { res.writeHead(200, { 'Content-Type': 'application/zip', 'Content-Disposition': 'attachment; filename="airview-export.zip"' }); res.end(Buffer.from('PK\x05\x06' + '\0'.repeat(18), 'binary')); return true; }
      if (p === '/api/me/account' && m === 'DELETE') {
        const b = await body(req);
        if (b.confirm !== 'DELETE') return json(res, 400, { error: 'send {"confirm":"DELETE"}' }), true;
        accounts.delete(v.user); for (const s of stations) if (s.owner === v.user) s.owner = null; sessions.delete(cookieOf(req)); return json(res, 200, { ok: true }), true;
      }
      const mm = /^\/api\/me\/stations\/([^/]+)(\/.*)?$/.exec(p);
      if (mm) {
        const s = stations.find((x) => x.sid === decodeURIComponent(mm[1]) && x.owner === v.user);
        if (!s) return json(res, 404, { error: 'unknown station' }), true;
        const sub = mm[2] || '';
        if (!sub && m === 'GET') return json(res, 200, detail(s)), true;
        if (!sub && m === 'POST') {
          const b = await body(req);
          if (b.pin !== undefined) { if (!['hidden', 'region', 'cell01'].includes(b.pin)) return json(res, 400, { error: 'pin: hidden | region | cell01' }), true; s.pin = b.pin; }
          if (b.showName !== undefined) s.showName = !!b.showName;
          if (b.publicStats !== undefined) s.publicStats = !!b.publicStats;
          if (b.name !== undefined) { if (String(b.name).length > 64) return json(res, 400, { error: 'name: at most 64 characters' }), true; s.name = String(b.name) || s.name; }
          if (b.declared) { s.lat = b.declared.lat; s.lon = b.declared.lon; s.alt = b.declared.alt; s.locStatus = 'declared'; s.locSource = 'manual'; }
          return json(res, 200, stationDTO(s)), true;
        }
        if (sub === '/stats' && m === 'GET') return json(res, 200, { sid: s.sid, now: Date.now(), stats: liveStats(s), lane: lane(s), links: linksOf(s), lastSeen: s.lastSeen }), true;
        if (sub === '/connections' && m === 'GET') return json(res, 200, { connections: connections(s) }), true;
        if (sub === '/unclaim' && m === 'POST') { s.owner = null; return json(res, 200, { ok: true }), true; }
      }
      return json(res, 404, { error: 'unknown route' }), true;
    }
    // ---- admin ----
    if (p.startsWith('/api/admin/')) {
      if (!need('admin')) return true;
      if (p === '/api/admin/stations' && m === 'GET') {
        const q = (u.searchParams.get('q') || '').toLowerCase();
        const adm = u.searchParams.get('admission') || '';
        const kind = u.searchParams.get('kind') || '';
        const unclaimed = u.searchParams.get('unclaimed') === '1';
        const rows = stations.filter((s) => (!q || `${s.name} ${s.sid} ${s.feederId}`.toLowerCase().includes(q)) && (!adm || s.admission === adm) && (!kind || s.kind === kind) && (!unclaimed || !s.owner));
        const counts = {};
        for (const s of stations) counts[s.admission] = (counts[s.admission] || 0) + 1;
        // registry rows carry no coordinates (the detail does, audited)
        return json(res, 200, { stations: rows.map((s) => { const d = stationDTO(s, true); delete d.effective; delete d.declared; delete d.gnss; delete d.publicPin; return d; }), counts }), true;
      }
      if (p === '/api/admin/tokens' && m === 'GET') return json(res, 200, { tokens }), true;
      if (p === '/api/admin/tokens' && m === 'POST') {
        const b = await body(req);
        const t = { id: 't' + (tokens.length + 1), note: b.note || '', uses: 0, maxUses: b.maxUses || 1, created: Date.now(), expires: Date.now() + (b.ttlHours || 168) * 3600e3, revoked: false, usable: true, createdBy: 1, lastUsed: 0 };
        tokens.unshift(t);
        addAudit(v.user, 'token.mint', '', t.note);
        return json(res, 200, { secret: 'avf-' + randomBytes(12).toString('hex'), token: t }), true;
      }
      const tm = /^\/api\/admin\/tokens\/([^/]+)$/.exec(p);
      if (tm && m === 'DELETE') { const t = tokens.find((x) => x.id === tm[1]); if (t) { t.revoked = true; t.usable = false; } addAudit(v.user, 'token.revoke', '', tm[1]); return json(res, 200, { ok: true }), true; }
      if (p === '/api/admin/audit' && m === 'GET') return json(res, 200, { entries: audit.slice(0, Number(u.searchParams.get('limit')) || 200).map((a, k) => ({ id: audit.length - k, t: a.t, actorId: owners[a.actor] || 0, actor: a.actor, action: a.action, sid: a.target || undefined, detail: a.detail || undefined })) }), true;
      if (p === '/api/admin/retention' && m === 'POST') return json(res, 200, { ran: 3 }), true;
      const fm = /^\/api\/admin\/feeders\/([^/]+)\/revoke$/.exec(p);
      if (fm && m === 'POST') { for (const s of stations) if (s.feederId === fm[1]) s.admission = 'revoked'; addAudit(v.user, 'feeder.revoke', '', fm[1]); return json(res, 200, { ok: true }), true; }
      const am = /^\/api\/admin\/stations\/([^/]+)$/.exec(p);
      if (am) {
        const s = st(decodeURIComponent(am[1]));
        if (!s) return json(res, 404, { error: 'unknown station' }), true;
        if (m === 'GET') { addAudit(v.user, 'station.reveal', s.sid); return json(res, 200, detail(s, true)), true; }
        const b = await body(req);
        if (m === 'POST') {
          if (b.admission) { s.admission = b.admission; addAudit(v.user, 'station.admission', s.sid, b.admission); }
          if (b.name) { s.name = b.name; addAudit(v.user, 'station.rename', s.sid, s.name); }
          return json(res, 200, stationDTO(s, true)), true;
        }
        if (m === 'DELETE') { stations.splice(stations.indexOf(s), 1); addAudit(v.user, 'station.delete', s.sid, b.reason || ''); return json(res, 200, { ok: true }), true; }
      }
      return json(res, 404, { error: 'unknown admin route' }), true;
    }
    // ---- legacy single-station routes: the host station's data, admin only (internal/server WIRING.md) ----
    if (v.scope !== 'admin' && p.startsWith('/api/') && !['/api/status', '/api/aircraft', '/api/events', '/api/trails'].includes(p) && !/^\/api\/aircraft\/[0-9A-F]{6}$/i.test(p)) {
      return v.scope === 'public' ? (json(res, 401, { error: 'login required' }), true) : (json(res, 403, { error: 'not allowed for this account' }), true);
    }
    return false;
  }

  return { handle, viewer, projectSnapshot, projectStatus };
}
