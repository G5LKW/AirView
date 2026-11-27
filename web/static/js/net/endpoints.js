// net/endpoints.js - every HTTP route the network (aggregator) UI calls, in one place, plus the normalisers
// that turn the server's replies (internal/server routes_net.go, api_public.go, api_me.go, api_admin.go,
// privacy/publicdto) into the shapes the pages render. When a route or a field changes on the server, only
// this file changes. The normalisers also accept the draft names (stations / stationCounts / idTail ...)
// used by web/dev/mock-network.mjs, so the mock and the server both work.

const enc = encodeURIComponent;
const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : undefined);

export const EP = {
  // ---- public (no session) ----
  /** GET -> publicdto.MLATCoverage { quality:{p50M, p95M, p99M, fixesPerMin}|null, samples, cells:[{lat, lon, band, n, p50M, p95M}] } */
  publicMLAT: () => '/api/public/mlat',
  /** GET -> { network, name?, host, avfPort, beastPort, mlatPort, mlat, admission, registration, backupDays, privacyNotice } (feeder page) */
  publicJoin: () => '/api/public/join',
  /** GET -> publicdto.NetworkStats { now, name, approved, online, counts } */
  publicSummary: () => '/api/public/stats',
  /** GET -> publicdto.StationList { entries:[{ sid?, name?, pin?, stats? }], approved, online } */
  publicStations: () => '/api/public/stations',
  /** GET -> { now, links:[linkDTO + warnings + claim], matched } (IP match, no login) */
  feedCheck: () => '/api/me/check',

  // ---- owner ----
  /** GET -> { stations:[stationDTO] } */
  myStations: () => '/api/me/stations',
  /** GET -> { station, estimates, links, stats, lane } */
  myStation: (sid) => `/api/me/stations/${enc(sid)}`,
  /** POST { name?, pin?, showName?, publicStats?, declared?:{lat,lon,alt,altRef}|null } -> stationDTO */
  myStationUpdate: (sid) => `/api/me/stations/${enc(sid)}`,
  /** GET -> { sid, now, stats, lane, links, lastSeen } */
  myStationStats: (sid) => `/api/me/stations/${enc(sid)}/stats`,
  /** GET ?days=&limit= -> { connections:[{ on, off, ipHash, proto, bytes }] } */
  myConnections: (sid) => `/api/me/stations/${enc(sid)}/connections`,
  /** POST {} -> { ok } */
  myUnclaim: (sid) => `/api/me/stations/${enc(sid)}/unclaim`,
  /** POST { code } | { uuid } -> stationDTO ; 429 with Retry-After */
  claim: () => '/api/me/claim',
  /** GET -> application/zip */
  myExport: () => '/api/me/export',
  /** GET -> { running, known, positionSource, clock{..., peers:[{tag, band, count, rmsNs}] (admin: sid, relPpb too), events:[{t, reason, detail}]}, mlat, summary, history } */
  myStationMLAT: (sid) => `/api/me/stations/${enc(sid)}/mlat`,
  /** GET ?days= -> hourly columns of the own station { t, fixesPerMin, framesPerMin, outliersPerMin, peers, syncedShare, sigmaNs, residRmsNs } */
  myStationMLATLongterm: (sid) => `/api/me/stations/${enc(sid)}/mlat/longterm`,
  /** POST {} -> stationDTO (apply the feeder-announced position the aggregator holds); 404 when none */
  myPendingConfirm: (sid) => `/api/me/stations/${enc(sid)}/location/pending/confirm`,
  /** PUT { reject } -> { positionSource, surveyRejected } (automatic use of the survey position for MLAT) */
  mySurveyUse: (sid) => `/api/me/stations/${enc(sid)}/mlat/survey`,
  /** GET -> { enabled, running, job, reason, started, observed, last, history[], nextCheck } (exact positions: owner/admin);
   *  POST {} -> { job, queued } (202 queued, 409 + consent C1 without timestamps, 429 within the hour) */
  mySurvey: (sid) => `/api/me/stations/${enc(sid)}/survey`,
  /** GET -> Locate2Result { ok, lat, lon, r95Km, sigmaKm, samples, notes[], errorKm, refSource } (approximate position) */
  myLocate: (sid) => `/api/me/stations/${enc(sid)}/locate`,
  /** POST { estimate: id } -> stationDTO: the owner confirms a measured position (status "confirmed") */
  myLocationConfirm: (sid) => `/api/me/stations/${enc(sid)}/location/confirm`,
  /** GET -> { notices:[{ t, kind, level, text }] } (location notices of the station, 30 days) */
  myStationNotices: (sid) => `/api/me/stations/${enc(sid)}/notices`,
  /** GET -> { notices:[{ sid, name, t, kind, level, text }] } (all own stations, 14 days) */
  myNotices: () => '/api/me/notices',
  /** DELETE { confirm: 'DELETE' } -> { ok } */
  myAccount: () => '/api/me/account',

  // ---- admin ----
  /** GET ?q=&admission=&kind=&unclaimed=&limit= -> { stations:[adminStationRow], counts } (no coordinates) */
  adminStations: () => '/api/admin/stations',
  /** GET -> station detail + liveLinks with addresses (audited "station.reveal") */
  adminStation: (sid) => `/api/admin/stations/${enc(sid)}`,
  /** POST { admission?, ownerId?, name?, trustDelta?, anchor? } */
  adminStationUpdate: (sid) => `/api/admin/stations/${enc(sid)}`,
  /** POST { mode: trusted|excluded|quarantine|auto } -> { timing, override } (timing quarantine override) */
  adminTiming: (sid) => `/api/admin/stations/${enc(sid)}/timing`,
  /** GET -> { enabled, stats, stations:[{ sid, name, kind, created, report }] } */
  adminQuarantine: () => '/api/admin/mlat/quarantine',
  /** DELETE { reason? } -> { ok } (queued) */
  adminDelete: (sid) => `/api/admin/stations/${enc(sid)}`,
  /** POST {} -> { ok } (revoke a native feeder key: "ban") */
  adminRevokeFeeder: (feederId) => `/api/admin/feeders/${enc(feederId)}/revoke`,
  /** GET -> { tokens:[tokenDTO] } */
  adminTokens: () => '/api/admin/tokens',
  /** POST { note, maxUses, ttlHours } -> { secret, token } (secret shown once) */
  adminTokenCreate: () => '/api/admin/tokens',
  /** DELETE -> { ok } */
  adminTokenRevoke: (id) => `/api/admin/tokens/${enc(id)}`,
  /** GET ?sid=&action=&limit= -> { entries:[{ id, t, actorId, actor, action, sid, detail }] } */
  adminAudit: () => '/api/admin/audit',
  /** POST {} -> retention report */
  adminRetention: () => '/api/admin/retention',
  // MLAT console (all answer { running:false } when the engine does not run)
  /** GET -> mlat.Stats (every stage, clock filter, reference store, tracker, planner, k_atm) */
  adminMLAT: () => '/api/admin/mlat',
  /** GET ?window=&cells=1 -> hold-out quality report */
  adminMLATQuality: () => '/api/admin/mlat/quality',
  /** GET ?minutes= -> columns { t, fixP50M, fixP95M, trackP50M, trackP95M, ..., solvesPerMin, ... } */
  adminMLATHistory: () => '/api/admin/mlat/history',
  /** GET ?days= -> hourly columns { t, minutes, groupsPerMin, fixesPerMin, outliersPerMin, peers, holdN, holdP50M, holdP95M } */
  adminMLATLongterm: () => '/api/admin/mlat/longterm',
  /** GET -> { stations:[row] } clock state and residuals per station */
  adminMLATStations: () => '/api/admin/mlat/stations',
  /** GET -> { stations:[{sid, name, clockClass, anchorState, synced}], edges:[{a, b, count, rmsNs, relPpb, sameDomain}] } */
  adminMLATSync: () => '/api/admin/mlat/sync',
  /** GET ?refresh=1 -> { computed, stepDeg, summary:[...], cells:[{lat, lon, altM, sigmaHM, n}] } */
  adminMLATCRLB: () => '/api/admin/mlat/crlb',
  /** GET -> the owner view of any station, peers by SID */
  adminStationMLAT: (sid) => `/api/admin/stations/${enc(sid)}/mlat`,
  /** GET status / POST start (no rate limit) / DELETE cancel a station's survey */
  adminSurvey: (sid) => `/api/admin/stations/${enc(sid)}/survey`,
  /** GET -> { enabled, autoUse, autoUseMaxSigmaM, stats, quality, stations:[...] } (no coordinates) */
  adminSurveys: () => '/api/admin/mlat/surveys',
  /** GET -> { enabled, stats, bundle:{ stats, last:{ stations:[...] } } } (network bundle adjustment: admin, exact) */
  adminSurveyStats: () => '/api/admin/mlat/survey',
  /** GET -> { running, counters:{confirmed, contradicted, suspects, blamed, culprit, contradictedShare}, current:[...], events:[...] } */
  adminVerification: () => '/api/admin/mlat/verification',
  /** GET ?alt=&step=&sigmaNs=&refresh=1 -> { altM, stepDeg, baseline, sites:[{lat, lon, gain, score, newCells, newA, newB, ...}], cells:[{lat, lon, score}] } */
  adminCRLBImprove: () => '/api/admin/mlat/crlb/improve',
  /** GET -> { enabled, stats, targets:[ModeACTarget] } */
  adminModeAC: () => '/api/admin/mlat/modeac',
  /** GET -> { enabled, running, error?, status:{ nodeId, name, listen, peers[], received, outFeeds[] } } */
  adminFederation: () => '/api/admin/federation',
  /** GET ?q=&state=&offset=&limit= -> { links:[shareLink + owner], total, counts } ; DELETE /{id} -> revoke */
  adminShares: () => '/api/admin/shares',
  adminShare: (id) => `/api/admin/shares/${enc(id)}`,

  // ---- share links (WP-H2) ----
  /** GET -> { links:[shareLink], max, maxDays, defaultDays, coarseLowDefault, warning } ; POST { stations, label, scope, days, coarseLow, acknowledge } -> { link, token, path } */
  myShares: () => '/api/me/shares',
  /** DELETE -> { ok, link } */
  myShare: (id) => `/api/me/shares/${enc(id)}`,
  /** GET (anyone with the link) -> { label, scope, stationCount, expires, coarseLow, network } */
  share: (token) => `/api/share/${enc(token)}`,
  /** GET -> publicdto.Snapshot of the aircraft the link's stations heard (no receiver data) */
  shareAircraft: (token) => `/api/share/${enc(token)}/aircraft`,
  /** GET -> { entries:[{ name, online, stats }] } (scope map+stats; never a stations member in a public payload) */
  shareStats: (token) => `/api/share/${enc(token)}/stats`,

  // ---- accounts (internal/server/auth_routes.go) ----
  setupState: () => '/api/auth/setup',
  setupBegin: () => '/api/auth/setup/begin',
  setupFinish: () => '/api/auth/setup/finish',
  register: () => '/api/auth/register',
  password: () => '/api/auth/password',
  totpBegin: () => '/api/auth/totp/begin',
  totpConfirm: () => '/api/auth/totp/confirm',
  totpDisable: () => '/api/auth/totp/disable',
  recoveryCodes: () => '/api/auth/recovery-codes',
  sessions: () => '/api/auth/sessions',
  logoutOthers: () => '/api/auth/logout-others',
};

/** Requests the public network UI makes (used by web/dev/privacy-crawl.mjs). */
export const PUBLIC_UI_REQUESTS = ['/api/session', '/api/status', '/api/aircraft', '/api/trails?minutes=5', EP.publicSummary(), EP.publicStations(), EP.publicMLAT(), EP.publicJoin(), EP.feedCheck(), EP.setupState()];

// ---------------------------------------------------------------------------------------------
// normalisers: server reply -> page model
// ---------------------------------------------------------------------------------------------

/** GET /api/public/stats -> { name, stationCounts:{total, online}, aircraft, mlatAircraft, messagesPerSec, mlat } */
export function normPublicSummary(r) {
  r = r || {};
  const c = r.counts || {};
  return {
    name: r.name || '',
    stationCounts: r.stationCounts || (num(r.approved) !== undefined ? { total: r.approved, online: r.online } : null),
    aircraft: num(r.aircraft) ?? num(c.aircraft),
    mlatAircraft: num(r.mlatAircraft) ?? num(c.mlat),
    messagesPerSec: num(r.messagesPerSec) ?? num(c.msgRate),
    mlat: r.mlat || null,
  };
}

/** GET /api/public/stations -> [{ sid?, name?, online, pin, stats }] */
export function normPublicStations(r) {
  const list = (r && (r.entries || r.stations)) || [];
  return list.map((e) => ({
    sid: e.sid, name: e.name, pin: e.pin || null,
    online: typeof e.online === 'boolean' ? e.online : !!e.stats?.online,
    stats: e.stats ? { msgRate: num(e.stats.msgRate), uptimePct: num(e.stats.uptimePct), aircraft: num(e.stats.aircraft), aircraftDay: num(e.stats.aircraftDay), positions: num(e.stats.positions), mlatContrib: num(e.stats.mlatContrib) } : null,
  }));
}

/** GET /api/me/check -> the links of the Feed Check page. */
export function normCheck(r) {
  return ((r && r.links) || []).map((l) => {
    const c = l.claim || {};
    const warn = Array.isArray(l.warnings) ? l.warnings : [];
    const claimKind = typeof c.claimable === 'string' ? c.claimable : l.claimMethod || (l.claimable ? (l.proto === 'avf' ? 'code' : 'uuid') : '');
    return {
      proto: l.proto, software: l.software,
      idTail: l.idTail || (l.uuidTail ? '…' + l.uuidTail : ''),
      uuidMissing: !!l.uuidMissing || warn.includes('no-uuid'),
      uuidMismatch: !!l.uuidMismatch || warn.includes('uuid-mismatch'),
      since: l.since, msgRate: l.msgRate, posRate: l.posRate, kBps: num(l.kBps) ?? num(l.kbps), rttMs: l.rttMs, clockClass: l.clockClass,
      mlatSynced: l.mlatSynced,
      sid: c.sid || l.sid || '', stationName: l.stationName || '',
      claimed: !!(c.claimed ?? l.claimed), yours: !!(c.mine ?? l.yours),
      claimKind, claimable: claimKind === 'code' || claimKind === 'uuid',
      admission: c.admission || l.admission || (l.pending ? 'pending' : ''),
      details: l.details !== false,
    };
  });
}

/** stationDTO (owner / admin) -> the station model of the pages, picker and map (exact position from `effective`). */
export function normStation(d) {
  if (!d || !d.sid) return null;
  const eff = d.effective || null;
  return {
    ...d,
    lat: num(d.lat) ?? num(eff?.lat),
    lon: num(d.lon) ?? num(eff?.lon),
    alt: num(d.alt) ?? num(eff?.hae),
    sigmaM: num(d.sigmaM) ?? num(eff?.sigmaM),
    locSource: d.locSource || eff?.source || '',
    publicPoint: d.publicPoint || d.publicPin || null,
    owner: d.owner || (d.ownerId ? `account #${d.ownerId}` : ''),
  };
}

/** GET /api/me/stations/{sid} (and the admin detail) -> one object the station page renders. */
export function normStationDetail(r) {
  if (!r) return null;
  if (!r.station) { // server shape: the station fields at top level, connection {proto, software, kbps, rttMs, links}
    const d = normStation(r);
    if (d && r.connection) {
      const c = r.connection;
      const l0 = (c.links || [])[0] || {};
      d.connection = { ...c, version: c.software || l0.software, rttP50Ms: num(c.rttMs) ?? num(l0.rttMs), kBps: num(c.kbps) ?? num(l0.kbps) };
    }
    return d;
  }
  const s = normStation(r.station);
  const links = Array.isArray(r.links) ? r.links : [];
  const live = r.stats || {};
  const lane = r.lane || {};
  const l0 = links[0] || {};
  const est = (r.estimates || []).map((e, i) => ({
    id: e.id || String(i), method: e.method, t: e.t, lat: e.lat, lon: e.lon, alt: num(e.alt) ?? num(e.hae),
    sigmaM: num(e.sigmaM) ?? (num(e.sigmaE) !== undefined ? Math.hypot(e.sigmaE, e.sigmaN || 0) / Math.SQRT2 : undefined),
    status: e.status, n: e.n,
  }));
  return {
    ...s,
    online: s.online ?? live.online,
    stats: { msgRate: num(live.msgRate), aircraft: num(live.aircraft) ?? num(lane.aircraft), positions: num(live.positions) ?? num(lane.positions), uptimePct: num(live.uptimePct) },
    lane,
    connection: {
      proto: l0.proto || s.kind, version: l0.software, since: l0.since, rttP50Ms: num(l0.rttMs), kBps: num(l0.kbps),
      lateFrames: num(lane.lateFrames), links, liveLinks: r.liveLinks || [],
    },
    location: {
      status: s.locStatus,
      effective: s.lat !== undefined ? { lat: s.lat, lon: s.lon, alt: s.alt, source: s.locSource, sigmaM: s.sigmaM } : null,
      declared: s.declared || null, gnss: s.gnss || null, estimates: est,
    },
  };
}

/** Connection log -> uptime segments [{from, to, online}] over the last `days`, from `notBefore` at the earliest (online where a link was open). */
export function uptimeSegments(conns, now, days = 7, notBefore = 0) {
  // the window starts no earlier than the station itself (UX-01)
  const start = Math.max(now - days * 86400e3, Number(notBefore) || 0);
  const open = (conns || []).map((c) => [Math.max(start, c.on), c.off ? Math.min(now, c.off) : now]).filter(([a, b]) => b > a).sort((a, b) => a[0] - b[0]);
  const merged = [];
  for (const [a, b] of open) {
    const last = merged[merged.length - 1];
    if (last && a <= last[1]) last[1] = Math.max(last[1], b); else merged.push([a, b]);
  }
  const segs = [];
  let t = start;
  for (const [a, b] of merged) {
    if (a > t) segs.push({ from: t, to: a, online: false });
    segs.push({ from: a, to: b, online: true });
    t = b;
  }
  if (t < now) segs.push({ from: t, to: now, online: false });
  return segs;
}

export default EP;
