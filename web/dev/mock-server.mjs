#!/usr/bin/env node
// AirView UI development server: serves web/static and implements the core v1 API from
// docs/ARCHITECTURE.md with moving fake aircraft, so the front-end can be developed and checked
// without the Go backend. No dependencies.
//
//   node web/dev/mock-server.mjs [--port=8099] [--n=80] [--unlocated] [--demo] [--no-tilecache]
//                                [--no-emergency] [--no-sse] [--lat=52.2 --lon=0.12]
//
// Implemented: GET /api/status, GET/PUT /api/config, GET /api/aircraft, GET /api/aircraft/{icao},
// GET /api/trails?minutes=N, GET /api/events (SSE, incl. a fake `event: alert` every 20 s),
// GET /api/alerts/recent, POST /api/timing/ntp/sample (?server= probe; "fail" in the host = no answer),
// GET /api/timing/latency[/aircraft[/{icao}]|/feeds/{id}], GET /api/timing/ntpcheck,
// GET /api/stats/daily (?days=N, synthetic days with busiestHour / farthest), GET /api/reports (+ .csv;
// the current fleet as closed flights; limit / offset / sort / desc / dep / arr / callsign filters),
// POST /api/factory-reset, GET /tilecache/{host}/{path}.
// Everything else under /api/ answers 404 {"error": ...} like the real server would for an unknown route.
//
// Network (aggregator) mode: --aggregator [--setup] adds accounts, sessions, CSRF, the /api/public, /api/me,
// /api/admin and /api/auth routes and the per-viewer projection of the live picture (web/dev/mock-network.mjs).

import http from 'node:http';
import { readFile, stat } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createNetworkMock } from './mock-network.mjs';

const args = Object.fromEntries(process.argv.slice(2).map((a) => {
  const m = /^--([^=]+)(?:=(.*))?$/.exec(a);
  return m ? [m[1], m[2] ?? true] : [a, true];
}));
const PORT = Number(args.port || process.env.PORT || 8099);
const N = Math.max(1, Number(args.n || 80));
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../static');
const STARTED = Date.now();

const MIME = {
  '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8', '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8', '.json': 'application/json; charset=utf-8', '.svg': 'image/svg+xml',
  '.png': 'image/png', '.woff2': 'font/woff2', '.md': 'text/markdown; charset=utf-8', '.txt': 'text/plain; charset=utf-8',
};

// ---------------------------------------------------------------------------------------------
// config (mirrors model.DefaultConfig)
// ---------------------------------------------------------------------------------------------
function defaultConfig() {
  return {
    version: 1,
    mode: 'standalone',
    http: { port: 80, bindAddr: '', adminPasswordHash: '', tileCache: !args['no-tilecache'] },
    receiver: { name: 'My Station', model: 'Radarcape EVO 2CH', located: false, lat: 0, lon: 0, groundElevM: 0 },
    antennas: [
      { id: 'vertical', name: 'Vertical', type: 'vertical', channel: 0, gainDbi: 5, heightAglM: 8, azimuthDeg: 0, tiltDeg: 0, hBeamwidthDeg: 0, vBeamwidthDeg: 30, frontToBackDb: 0, elements: 0, polarization: 'vertical', cableLossDb: 0, lnaGainDb: 0, lnaNoiseFigDb: 0, filterLossDb: 0, notes: '', color: '#38bdf8' },
      { id: 'yagi', name: 'Yagi', type: 'yagi', channel: 1, gainDbi: 12, heightAglM: 8, azimuthDeg: 0, tiltDeg: 0, hBeamwidthDeg: 40, vBeamwidthDeg: 40, frontToBackDb: 18, elements: 9, polarization: 'vertical', cableLossDb: 0, lnaGainDb: 0, lnaNoiseFigDb: 0, filterLossDb: 0, notes: '', color: '#f59e0b' },
    ],
    feeds: [{ id: 'radarcape', name: 'Radarcape EVO (Beast 10003)', enabled: true, type: 'beast', host: 'radarcape', port: 10003, url: '', antennaId: '', mlatOnly: false, notes: '' }],
    ntp: { enabled: false, server: '', intervalSec: 64 },
    weather: { enabled: true, intervalMin: 15, radarOverlay: false },
    hub: { enabled: false, port: 30154, password: '' },
    remote: { host: '', dataPort: 30154, httpPort: 80, password: '', useHostReceiver: true },
    storage: { trailIntervalSec: 5, trailRetentionDays: 30, flightRetentionDays: 730, minuteRetentionDays: 90 },
    ui: { units: 'aviation', trailMinutes: 15, rangeRingsKm: [100, 200, 300, 400], aircraftTtlS: 60, mapStyle: 'airview-dark', colorTrailsBy: 'altitude' },
    demo: { enabled: false, aircraft: 60 },
  };
}

function mockConfig() {
  const c = defaultConfig();
  if (!args.unlocated) {
    c.receiver = { ...c.receiver, name: 'Mock Station', located: true, lat: Number(args.lat ?? 52.2), lon: Number(args.lon ?? 0.12), groundElevM: 18 };
    c.antennas[1].azimuthDeg = 245;
    c.ntp = { enabled: true, server: '192.168.1.10', intervalSec: 64 };
  }
  c.demo.enabled = !!args.demo;
  return c;
}
let config = mockConfig();
// network mode: the aggregator block of model.Config (subset the admin console edits)
let netAggregator = { name: 'AirView Mock Network', admission: 'invite', publicMap: true, publicStats: true, tarJson: true, defaultPin: 'hidden', islandGuard: false, mlat: { autoUseSurvey: true, autoUseMaxSigmaM: 50 }, auth: { registration: 'open' } };

// ---------------------------------------------------------------------------------------------
// geodesy
// ---------------------------------------------------------------------------------------------
const R = 6371.0088;
const D2R = Math.PI / 180;
const R2D = 180 / Math.PI;
const normLon = (l) => ((((l + 180) % 360) + 360) % 360) - 180;
const norm360 = (d) => ((d % 360) + 360) % 360;
function dest(lat, lon, brg, km) {
  const a = km / R;
  const p1 = lat * D2R;
  const b = brg * D2R;
  const s = Math.sin(p1) * Math.cos(a) + Math.cos(p1) * Math.sin(a) * Math.cos(b);
  const p2 = Math.asin(Math.max(-1, Math.min(1, s)));
  const l2 = lon * D2R + Math.atan2(Math.sin(b) * Math.sin(a) * Math.cos(p1), Math.cos(a) - Math.sin(p1) * s);
  return [p2 * R2D, normLon(l2 * R2D)];
}
function distKm(lat1, lon1, lat2, lon2) {
  const dp = (lat2 - lat1) * D2R;
  const dl = (lon2 - lon1) * D2R;
  const a = Math.sin(dp / 2) ** 2 + Math.cos(lat1 * D2R) * Math.cos(lat2 * D2R) * Math.sin(dl / 2) ** 2;
  return 2 * R * Math.asin(Math.min(1, Math.sqrt(a)));
}
function bearing(lat1, lon1, lat2, lon2) {
  const dl = (lon2 - lon1) * D2R;
  const y = Math.sin(dl) * Math.cos(lat2 * D2R);
  const x = Math.cos(lat1 * D2R) * Math.sin(lat2 * D2R) - Math.sin(lat1 * D2R) * Math.cos(lat2 * D2R) * Math.cos(dl);
  return norm360(Math.atan2(y, x) * R2D);
}

// ---------------------------------------------------------------------------------------------
// traffic simulation
// ---------------------------------------------------------------------------------------------
let seed = 20261111;
const rnd = () => { seed = (seed * 1664525 + 1013904223) >>> 0; return seed / 4294967296; };
const pick = (arr) => arr[Math.floor(rnd() * arr.length)];
const between = (a, b) => a + rnd() * (b - a);

const AIRLINES = [
  ['BAW', 'British Airways', 'United Kingdom', 'GB', 0x400000], ['EZY', 'easyJet', 'United Kingdom', 'GB', 0x406000],
  ['RYR', 'Ryanair', 'Ireland', 'IE', 0x4ca000], ['DLH', 'Lufthansa', 'Germany', 'DE', 0x3c4000],
  ['AFR', 'Air France', 'France', 'FR', 0x394000], ['KLM', 'KLM', 'Netherlands', 'NL', 0x484000],
  ['UAE', 'Emirates', 'United Arab Emirates', 'AE', 0x896000], ['QTR', 'Qatar Airways', 'Qatar', 'QA', 0x06a000],
  ['AAL', 'American Airlines', 'United States', 'US', 0xa00000], ['UAL', 'United Airlines', 'United States', 'US', 0xa80000],
  ['SAS', 'SAS', 'Sweden', 'SE', 0x4a8000], ['THY', 'Turkish Airlines', 'Türkiye', 'TR', 0x4b8000],
  ['WZZ', 'Wizz Air', 'Hungary', 'HU', 0x471000], ['VLG', 'Vueling', 'Spain', 'ES', 0x344000],
];
const TYPES = [
  ['A320', 'Airbus A320', 'A3', 450], ['A20N', 'Airbus A320neo', 'A3', 450], ['B738', 'Boeing 737-800', 'A3', 455], ['A21N', 'Airbus A321neo', 'A3', 455],
  ['B38M', 'Boeing 737 MAX 8', 'A3', 455], ['E190', 'Embraer 190', 'A3', 430], ['A333', 'Airbus A330-300', 'A5', 480], ['B77W', 'Boeing 777-300ER', 'A5', 490],
  ['B789', 'Boeing 787-9', 'A5', 490], ['A359', 'Airbus A350-900', 'A5', 488], ['A388', 'Airbus A380-800', 'A5', 490], ['DH8D', 'Dash 8 Q400', 'A2', 340],
];
const GA = [['C172', 'Cessna 172', 'A1', 105], ['PA28', 'Piper PA-28', 'A1', 115], ['SR22', 'Cirrus SR22', 'A1', 170], ['EC35', 'Airbus H135', 'A7', 120], ['R44', 'Robinson R44', 'A7', 100], ['ASK21', 'Schleicher ASK 21', 'B1', 55], ['PC12', 'Pilatus PC-12', 'A1', 260]];

const center = () => (config.receiver.located ? [config.receiver.lat, config.receiver.lon] : [Number(args.lat ?? 52.2), Number(args.lon ?? 0.12)]);
const fleet = [];
const trails = new Map(); // icao -> [[tMs,lat,lon,alt,gs,rssi,src],...]
const TRAIL_KEEP_MS = 2 * 3600 * 1000;

function spawn(i, now, fresh) {
  const [clat, clon] = center();
  const r = rnd();
  let kind = 'airliner';
  if (r > 0.86) kind = 'ga';
  if (r > 0.97) kind = 'military';
  const src = (() => { const s = rnd(); return s < 0.78 ? 'adsb' : s < 0.89 ? 'mlat' : s < 0.97 ? 'modes' : 'tisb'; })();
  const al = pick(AIRLINES);
  const t = kind === 'ga' ? pick(GA) : pick(TYPES);
  // ICAO addresses must be unique within the fleet (the real tracker keys everything on them)
  let icao;
  do {
    const icaoNum = kind === 'military' ? 0x43c000 + Math.floor(rnd() * 0xfff) : al[4] + Math.floor(rnd() * 0xfff);
    icao = icaoNum.toString(16).toUpperCase().padStart(6, '0');
  } while (fleet.some((x, j) => j !== i && x && x.icao === icao));
  const dist = fresh ? between(380, 430) : Math.sqrt(rnd()) * 400;
  const brgFrom = rnd() * 360;
  const [lat, lon] = dest(clat, clon, brgFrom, dist);
  const inbound = norm360(brgFrom + 180 + between(-55, 55));
  const cruise = kind === 'ga' ? between(1200, 6500) : kind === 'military' ? between(8000, 28000) : Math.round(between(28000, 41000) / 1000) * 1000;
  const phase = kind === 'airliner' && rnd() < 0.3 ? (rnd() < 0.5 ? 'climb' : 'descend') : 'cruise';
  const a = {
    icao, kind, src,
    callsign: kind === 'ga' ? 'G-' + String.fromCharCode(65 + Math.floor(rnd() * 26), 65 + Math.floor(rnd() * 26), 65 + Math.floor(rnd() * 26), 65 + Math.floor(rnd() * 26))
      : kind === 'military' ? pick(['RRR', 'ASCOT', 'TARTN', 'NATO']) + Math.floor(between(10, 99)) : al[0] + Math.floor(between(1, 9999)),
    squawk: String(Math.floor(between(0, 7) + 0) * 1000 + Math.floor(rnd() * 8) * 100 + Math.floor(rnd() * 8) * 10 + Math.floor(rnd() * 8)).padStart(4, '0'),
    category: kind === 'military' ? pick(['A6', 'A5', 'A3']) : t[2],
    type: t[0], typeDesc: t[1], operator: kind === 'airliner' ? al[1] : kind === 'military' ? 'Royal Air Force' : '',
    country: kind === 'airliner' ? al[2] : 'United Kingdom', countryCode: kind === 'airliner' ? al[3] : 'GB',
    reg: kind === 'ga' ? null : (al[3] === 'GB' ? 'G-' : al[3] === 'DE' ? 'D-A' : al[3] === 'US' ? 'N' : al[3] + '-') + icao.slice(2),
    military: kind === 'military',
    lat, lon, track: fresh ? inbound : rnd() * 360,
    alt: phase === 'climb' ? between(4000, cruise - 4000) : phase === 'descend' ? between(6000, cruise) : cruise,
    cruise, phase, speed: t[3] * between(0.92, 1.06), turn: 0, turnUntil: 0,
    msgs: Math.floor(between(50, 4000)), firstSeen: now - Math.floor(between(20, 1500)) * 1000,
    lastTrail: 0, rssiBias: between(-3, 3), hasGeom: rnd() < 0.9, v2: rnd() < 0.85,
    // sane values until the first simulation step has run for this aircraft
    gs: t[3], vr: 0, dist, rssi: -22, rssiAvg: -22, msgRate: 1,
  };
  if (kind === 'ga') { a.alt = cruise; a.phase = 'cruise'; }
  fleet[i] = a;
  trails.delete(icao);
  return a;
}

function step(now, dt) {
  const [clat, clon] = center();
  for (let i = 0; i < fleet.length; i++) {
    let a = fleet[i];
    // occasional heading change
    if (now > a.turnUntil) {
      if (rnd() < 0.012 * dt) { a.turn = between(-2.2, 2.2); a.turnUntil = now + between(8, 40) * 1000; } else a.turn = 0;
    }
    a.track = norm360(a.track + a.turn * dt);
    // vertical profile
    let vr = 0;
    if (a.phase === 'climb') { vr = a.alt < 10000 ? 2400 : 1600; if (a.alt >= a.cruise) { a.phase = 'cruise'; a.alt = a.cruise; vr = 0; } }
    else if (a.phase === 'descend') { vr = -1700; if (a.alt <= 3000) { a.phase = 'climb'; } }
    else if (a.kind === 'airliner' && rnd() < 0.0004 * dt) a.phase = 'descend';
    a.alt = Math.max(0, a.alt + (vr * dt) / 60);
    a.vr = vr + (vr ? between(-80, 80) : 0);
    const gs = a.speed * (a.alt < 10000 && a.kind !== 'ga' ? 0.62 : 1) + Math.sin(now / 60000 + i) * 6;
    a.gs = gs;
    const [lat, lon] = dest(a.lat, a.lon, a.track, (gs * 1.852 * dt) / 3600);
    a.lat = lat;
    a.lon = lon;
    const d = distKm(clat, clon, lat, lon);
    if (d > 440) { a = spawn(i, now, true); continue; }
    a.dist = d;
    const slant = Math.hypot(d, (a.alt * 0.3048) / 1000);
    a.rssi = Math.max(-38, Math.min(-1.5, -4 - 20 * Math.log10(Math.max(4, slant) / 12) + a.rssiBias + between(-1.2, 1.2)));
    a.rssiAvg = a.rssiAvg === undefined ? a.rssi : a.rssiAvg * 0.9 + a.rssi * 0.1;
    a.msgRate = Math.max(0.2, (a.src === 'modes' ? 1.2 : 5.5) * (1 - d / 600) + between(-0.5, 0.5));
    a.msgs += Math.round(a.msgRate * dt);
    if (a.src !== 'modes' && now - a.lastTrail >= 4000) {
      a.lastTrail = now;
      let t = trails.get(a.icao);
      if (!t) { t = []; trails.set(a.icao, t); }
      t.push([now, +lat.toFixed(5), +lon.toFixed(5), Math.round(a.alt), Math.round(gs), +a.rssi.toFixed(1), a.src === 'adsb' ? 0 : a.src === 'mlat' ? 1 : 2]);
      while (t.length && now - t[0][0] > TRAIL_KEEP_MS) t.shift();
    }
  }
}

const round = (v, d = 0) => { const k = 10 ** d; return Math.round(v * k) / k; };

function dto(a, now) {
  const located = config.receiver.located;
  const hasPos = a.src !== 'modes';
  const o = {
    icao: a.icao, addrType: a.src === 'tisb' ? 'tisb-track' : 'icao', callsign: a.callsign, squawk: a.emergency ? '7700' : a.squawk, category: a.category,
    reg: a.reg || undefined, type: a.type, typeDesc: a.typeDesc, operator: a.operator || undefined, country: a.country, countryCode: a.countryCode,
    military: a.military || undefined,
    source: a.src,
    altBaro: round(a.alt / 25) * 25, vertRate: round(a.vr / 64) * 64,
    gs: round(a.gs, 1), track: round(a.track, 1),
    rssi: round(a.rssi, 1), rssiAvg: round(a.rssiAvg, 1), msgs: a.msgs, msgRate: round(a.msgRate, 1),
    antennas: a.dist < 150 ? ['vertical', 'yagi'] : ['vertical'], feeds: ['radarcape'],
    firstSeen: a.firstSeen, lastSeen: now - Math.floor(rnd() * 900),
    flightId: parseInt(a.icao, 16) % 100000,
  };
  if (hasPos) {
    o.lat = round(a.lat, 5);
    o.lon = round(a.lon, 5);
    o.posAge = a.src === 'mlat' ? round(between(0.5, 6), 1) : a.stale ? round(between(22, 50), 1) : round(between(0, 1.4), 1);
    o.onGround = false;
    if (a.hasGeom) o.altGeom = o.altBaro + 175;
    if (located) {
      const { lat, lon, groundElevM } = config.receiver;
      o.distKm = round(a.dist, 2);
      o.bearing = round(bearing(lat, lon, a.lat, a.lon), 1);
      const hKm = (a.alt * 0.3048 - groundElevM - 8) / 1000;
      o.slantKm = round(Math.hypot(a.dist, hKm), 2);
      o.elevation = round(Math.atan2(hKm - (a.dist * a.dist) / (2 * R * (4 / 3)), a.dist) * R2D, 2);
    }
  }
  if (a.src === 'adsb') {
    o.adsbVersion = a.v2 ? 2 : 0; o.nic = 8; o.nacp = 9; o.nacv = 2; o.sil = 3; o.rcM = 185.2;
    if (a.kind === 'airliner') {
      o.selAlt = a.phase === 'cruise' ? a.cruise : a.phase === 'climb' ? a.cruise : 3000;
      o.qnh = 1013.2; o.selHeading = round(a.track, 0);
      o.navModes = a.phase === 'cruise' ? ['autopilot', 'althold', 'lnav', 'tcas'] : ['autopilot', 'vnav', 'lnav', 'tcas'];
      o.ias = round(a.gs * (a.alt > 25000 ? 0.58 : 0.8)); o.tas = round(a.gs - 12); o.mach = a.alt > 25000 ? round(a.gs / 590, 3) : undefined;
      o.magHeading = round(norm360(a.track - 1.5), 1); o.roll = round(a.turn * 9, 1); o.trackRate = round(a.turn, 2);
      if (a.alt > 15000) { o.windDir = round(norm360(255 + Math.sin(a.lat) * 20)); o.windSpd = round(48 + Math.cos(a.lon) * 20); o.oat = round(15 - (a.alt / 1000) * 1.98, 1); }
    }
  }
  if (a.emergency) { o.emergency = 'general'; o.alert = true; }
  return o;
}

function snapshot() {
  const now = Date.now();
  const list = fleet.map((a) => dto(a, now));
  const counts = { aircraft: list.length, withPos: 0, adsb: 0, mlat: 0, tisb: 0, modes: 0, msgRate: 0, posRate: 0, maxRangeKm: 0 };
  for (const a of list) {
    if (a.lat !== undefined) counts.withPos++;
    if (a.source === 'adsb') counts.adsb++; else if (a.source === 'mlat') counts.mlat++; else if (a.source === 'tisb') counts.tisb++; else counts.modes++;
    counts.msgRate += a.msgRate;
    if (a.distKm > counts.maxRangeKm) counts.maxRangeKm = a.distKm;
  }
  counts.msgRate = round(counts.msgRate, 1);
  counts.posRate = round(counts.withPos * 1.6, 1);
  counts.maxRangeKm = round(counts.maxRangeKm, 1);
  return { now, aircraft: list, counts };
}

function status() {
  const snap = snapshot();
  return {
    app: 'AirView', version: '0.1.0-mock', platform: `${process.platform}/${process.arch}`, gui: false,
    now: Date.now(), started: STARTED, mode: config.mode, httpPort: PORT,
    located: config.receiver.located, demo: !!config.demo.enabled, tileCache: !!config.http.tileCache,
    feeds: config.feeds.map((f, i) => ({
      id: f.id, name: f.name, type: f.type, addr: f.type === 'json' ? f.url : `${f.host}:${f.port}`, enabled: f.enabled, connected: f.enabled && i === 0,
      since: STARTED, frames: Math.floor((Date.now() - STARTED) / 1000 * snap.counts.msgRate), bytes: Math.floor((Date.now() - STARTED) / 1000 * snap.counts.msgRate * 20),
      msgRate: i === 0 ? snap.counts.msgRate : 0, lastFrame: Date.now(), gpsTime: i === 0, info: i === 0 ? 'GPS locked, 11 sats' : '',
      ...(f.enabled && i !== 0 ? { lastError: 'dial tcp: connection refused' } : {}),
    })),
    counts: snap.counts, dbBytes: 48_300_032, dataDir: '/mock/data',
    ntp: config.ntp.enabled
      ? { enabled: true, server: config.ntp.server, synced: true, offsetMs: round(0.42 + Math.sin(Date.now() / 30000) * 0.3, 3), delayMs: 0.61, jitterMs: 0.08, stratum: 1, refId: 'GPS', lastOk: Date.now() - 12000 }
      : { enabled: false, server: '', synced: false, offsetMs: 0, delayMs: 0, jitterMs: 0, stratum: 0, lastOk: 0 },
    hub: { enabled: config.hub.enabled, port: config.hub.port, clients: 0 },
    adminLocked: false,
    station: config.receiver.located ? { lat: config.receiver.lat, lon: config.receiver.lon, source: 'config' } : { lat: null, lon: null, source: 'none' },
  };
}

// ---------------------------------------------------------------------------------------------
// alerts (in memory) + the newer timing endpoints, with plausible shapes (see docs/API.md)
// ---------------------------------------------------------------------------------------------
const alertLog = [];
let alertSeq = 0;
function fireAlert() {
  const a = pick(fleet.filter((x) => x.src !== 'modes')) || fleet[0];
  if (!a) return null;
  const sev = pick(['info', 'info', 'warning', 'critical']);
  const al = {
    id: ++alertSeq, t: Date.now(), ruleId: 'mock-low', ruleName: 'Low overhead (mock)', severity: sev, kind: 'aircraft',
    icao: a.icao, callsign: a.callsign, reg: a.reg || undefined, type: a.type, operator: a.operator || undefined,
    altFt: Math.round(a.alt), distKm: round(a.dist || 0, 1), squawk: a.squawk,
    text: `${a.callsign} at ${Math.round(a.alt)} ft, ${round(a.dist || 0, 1)} km`, notify: true, sound: sev !== 'info', logged: true,
  };
  alertLog.push(al);
  while (alertLog.length > 500) alertLog.shift();
  for (const c of sseClients) sseSend(c, 'alert', al);
  return al;
}
setInterval(fireAlert, 20000);

const nul = (v) => (Number.isFinite(v) ? v : null);
function latencyChain() {
  const st = (key, label, p50, spread) => ({ key, label, description: label, count: 1000 + Math.floor(rnd() * 500), minMs: round(p50 * 0.4, 3), p50Ms: round(p50, 3), p95Ms: round(p50 + spread, 3), p99Ms: round(p50 + spread * 1.6, 3), maxMs: round(p50 + spread * 3, 3), meanMs: round(p50 * 1.1, 3), uncertaintyMs: 0.5, exact: key === 'propagation' });
  const stages = [st('propagation', 'Propagation', 0.96, 0.6), st('receiver', 'Receiver', 0.12, 0.05), st('processing', 'Processing', 1.8, 2), st('transponder', 'Transponder', 0.4, 0.2), st('ageDelta', 'Age delta', 220, 150), st('airToSystem', 'Air to system', 3.1, 2), st('endToEnd', 'End to end', 230, 160)];
  const histo = (key, label) => ({ key, label, unit: 'ms', scale: 'log', count: 400, bins: Array.from({ length: 12 }, (_, i) => ({ lo: 0.1 * 2 ** i, hi: 0.1 * 2 ** (i + 1), n: Math.round(80 * Math.exp(-((i - 4) ** 2) / 6)) })) });
  return {
    chain: { now: Date.now(), windowSec: 600, receiverSet: !!config.receiver.located, clockSynced: !!config.ntp.enabled, clockOffsetMs: 0.4, aircraft: fleet.length, aircraftT1: Math.floor(fleet.length / 3), shareT1: 0.33, msgShareT1: 0.4, stages, counts: {}, feeds: [], findings: [] },
    histograms: [...stages.map((s) => histo(s.key, s.label)), histo('aircraftTransponder', 'Transponder per aircraft'), histo('aircraftAgeJitter', 'Age jitter per aircraft')],
  };
}
function latencyRow(a) {
  return { icao: a.icao, firstSeen: a.firstSeen, lastSeen: Date.now(), n: a.msgs, positions: Math.floor(a.msgs / 4), t1: a.v2, t0: !a.v2, tFlag: a.v2 ? 1 : 0, lastTc: 11, xpdrN: 40, xpdrMeanMs: 0.4, xpdrStdMs: 0.1, xpdrMinMs: 0.2, xpdrMaxMs: 0.8, agePairs: 30, ageJitterMs: 12, ageModelMs: 200, ageCheckMs: 205, positionAgeMs: 210, ageExact: a.v2, propMeanUs: round((a.dist || 100) * 3.336, 1), propLastUs: round((a.dist || 100) * 3.336, 1), slantKm: round(a.dist || 0, 1), flags: [] };
}

// boot the fleet and pre-roll 20 minutes of history so trails exist immediately
{
  const now = Date.now();
  const preroll = 20 * 60;
  for (let i = 0; i < N; i++) spawn(i, now - preroll * 1000, false);
  for (let t = preroll; t > 0; t -= 4) step(now - t * 1000, 4);
  if (!args['no-emergency'] && fleet.length > 3) {
    const e = fleet.find((a) => a.src === 'adsb' && a.kind === 'airliner');
    if (e) e.emergency = true;
    const s = fleet.filter((a) => a.src === 'adsb' && !a.emergency)[5];
    if (s) s.stale = true;
  }
}
let lastStep = Date.now();
setInterval(() => { const now = Date.now(); step(now, Math.min(5, (now - lastStep) / 1000)); lastStep = now; }, 1000);

// ---------------------------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------------------------
function json(res, code, body) {
  const data = JSON.stringify(body);
  res.writeHead(code, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store', 'Content-Length': Buffer.byteLength(data) });
  res.end(data);
}

function readBody(req, limit = 1 << 20) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (c) => { size += c.length; if (size > limit) { reject(new Error('body too large')); req.destroy(); } else chunks.push(c); });
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

const sseClients = new Set();
function sseSend(res, event, data) { res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`); }
// network mode: every stream gets the picture projected for its own viewer (public / owner / admin)
const viewOf = (c) => (NET ? NET.viewer(c.__req) : null);
const forViewer = (kind, data, v) => (!NET || !v ? data : kind === 'aircraft' ? NET.projectSnapshot(data, v) : NET.projectStatus(data, v));
setInterval(() => { if (sseClients.size) { const s = snapshot(); for (const c of sseClients) sseSend(c, 'aircraft', forViewer('aircraft', s, viewOf(c))); } }, 1000);
setInterval(() => { if (sseClients.size) { const s = status(); for (const c of sseClients) sseSend(c, 'status', forViewer('status', s, viewOf(c))); } }, 5000);

const TILE_HOSTS = new Set(['tiles.openfreemap.org', 'tile.openstreetmap.org', 'tilecache.rainviewer.com', 'api.rainviewer.com']);
const tileMem = new Map(); // url -> { type, body }
let tileMemBytes = 0;
async function tileProxy(res, host, rest, search) {
  if (!TILE_HOSTS.has(host)) return json(res, 403, { error: `host ${host} is not whitelisted` });
  const url = `https://${host}/${rest}${search}`;
  let hit = tileMem.get(url);
  if (!hit) {
    const up = await fetch(url, { headers: { 'User-Agent': 'AirView-dev-mock/0.1 (local UI development)' } });
    if (!up.ok) { res.writeHead(up.status, { 'Content-Type': 'text/plain' }); res.end(`upstream ${up.status}`); return; }
    hit = { type: up.headers.get('content-type') || 'application/octet-stream', body: Buffer.from(await up.arrayBuffer()) };
    if (host !== 'api.rainviewer.com' && tileMemBytes < 128 << 20) { tileMem.set(url, hit); tileMemBytes += hit.body.length; }
  }
  res.writeHead(200, { 'Content-Type': hit.type, 'Cache-Control': 'public, max-age=86400', 'Content-Length': hit.body.length, 'X-Mock-Tilecache': 'yes' });
  res.end(hit.body);
}

async function serveStatic(res, pathname, head) {
  let rel = decodeURIComponent(pathname);
  if (rel.endsWith('/')) rel += 'index.html';
  const file = path.normalize(path.join(ROOT, rel));
  if (!file.startsWith(ROOT)) { res.writeHead(403); res.end(); return; }
  try {
    const st = await stat(file);
    if (st.isDirectory()) throw new Error('dir');
    const data = await readFile(file);
    res.writeHead(200, { 'Content-Type': MIME[path.extname(file).toLowerCase()] || 'application/octet-stream', 'Cache-Control': 'no-cache', 'Content-Length': data.length, 'X-Content-Type-Options': 'nosniff' });
    res.end(head ? undefined : data);
  } catch {
    res.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' });
    res.end('404 not found');
  }
}

// ---- reports / daily (synthetic, from the current fleet) ----
const AIRPORTS = ['EGLL', 'EGKK', 'EGSS', 'EHAM', 'EDDF', 'LFPG', 'EIDW', ''];
function mockReport(q) {
  const now = Date.now();
  let rows = fleet.filter(Boolean).map((a, i) => {
    const start = Math.floor(a.firstSeen || now - 3600e3);
    return {
      flightId: 1000 + i, icao: a.icao, callsign: a.callsign || undefined, reg: a.reg || undefined, type: a.type, operator: a.operator || undefined,
      country: a.country, category: a.category, military: a.kind === 'military' || undefined,
      start, end: now, durationS: Math.round((now - start) / 1000), msgs: a.msgs || 0, positions: Math.round((a.msgs || 0) / 4), srcMask: 1,
      minDistKm: round((a.dist || 0) * 0.6, 1), maxDistKm: round(a.dist || 0, 1), maxDistBrg: null, minAlt: 0, maxAlt: a.alt ?? null, maxGs: a.gs ?? null,
      rssiMax: null, rssiAvg: null, firstLat: null, firstLon: null, lastLat: a.lat ?? null, lastLon: a.lon ?? null,
      dep: AIRPORTS[i % AIRPORTS.length], arr: AIRPORTS[(i * 3 + 1) % AIRPORTS.length],
    };
  });
  for (const k of ['dep', 'arr']) {
    const v = (q.get(k) || '').toUpperCase();
    if (v) rows = rows.filter((r) => r[k] === v);
  }
  const cs = (q.get('callsign') || '').toUpperCase();
  if (cs) rows = rows.filter((r) => (r.callsign || '').toUpperCase().includes(cs));
  const sort = q.get('sort') || 'start';
  const desc = q.get('desc') !== '0';
  rows.sort((a, b) => {
    const x = a[sort] ?? '', y = b[sort] ?? '';
    return (x < y ? -1 : x > y ? 1 : 0) * (desc ? -1 : 1);
  });
  const limit = Math.min(Number(q.get('limit')) || 100, 1000), offset = Number(q.get('offset')) || 0;
  return { from: now - 86400e3, to: now, total: rows.length, limit, offset, sort, desc, rows: rows.slice(offset, offset + limit) };
}

function mockDaily(days) {
  days = Math.max(1, Math.min(days, 365));
  const now = new Date();
  const mid = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const out = [];
  for (let d = days - 1; d >= 0; d--) {
    const t = mid - d * 86400e3;
    const k = 0.7 + 0.3 * Math.sin(d * 1.7);
    const flights = Math.round(900 * k);
    const a = fleet[d % Math.max(1, fleet.length)];
    out.push({
      t, date: new Date(t).toISOString().slice(0, 10), frames: Math.round(9e6 * k), positions: Math.round(1.2e6 * k),
      peakAircraft: Math.round(80 * k), uniqueAircraft: Math.round(700 * k), flights, newAirframes: Math.round(40 * k),
      maxKm: round(330 + 60 * k, 1), uptimeMin: d === 0 ? Math.round((now - mid) / 60000) : 1440,
      busiestHour: { t: t + (14 + (d % 5)) * 3600e3, aircraft: Math.round(80 * k), frames: Math.round(6e5 * k) },
      farthest: a ? { flightId: 1000 + (d % fleet.length), icao: a.icao, callsign: a.callsign || '', km: round(330 + 60 * k, 1), t: t + 9 * 3600e3 } : null,
    });
  }
  const busiestDay = out.reduce((b, x) => (!b || x.uniqueAircraft > b.uniqueAircraft ? x : b), null);
  const busiestHour = out.reduce((b, x) => (!b || x.busiestHour.aircraft > b.aircraft ? x.busiestHour : b), null);
  return { from: out[0].t, to: mid + 86400e3, ant: '', utcOffsetSec: -now.getTimezoneOffset() * 60, days: out, busiestDay, busiestHour };
}

const NET = args.aggregator ? createNetworkMock({ center, fleetIcaos: () => fleet.map((a) => a.icao), readBody, json, setupRequired: !!args.setup }) : null;

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://localhost');
  const p = u.pathname;
  try {
    if (NET && (await NET.handle(req, res, u))) return;
    const viewer = NET ? NET.viewer(req) : null;
    if (p === '/api/status' && req.method === 'GET') return json(res, 200, forViewer('status', status(), viewer));
    if (p === '/api/config' && req.method === 'GET') return json(res, 200, NET ? { ...config, mode: 'aggregator', aggregator: netAggregator, locked: [{ path: 'aggregator.tarJson', origin: 'env AIRVIEW_AGGREGATOR_TAR_JSON' }] } : config);
    if (p === '/api/config' && req.method === 'PUT') {
      let next;
      try { next = JSON.parse(await readBody(req)); } catch { return json(res, 400, { error: 'invalid JSON body' }); }
      if (!next || typeof next !== 'object' || !next.receiver || !Array.isArray(next.antennas) || !Array.isArray(next.feeds)) return json(res, 400, { error: 'config: receiver, antennas and feeds are required' });
      if (next.receiver.located && (Math.abs(next.receiver.lat) > 90 || Math.abs(next.receiver.lon) > 180)) return json(res, 400, { error: 'receiver: latitude/longitude out of range' });
      const moved = next.receiver.lat !== config.receiver.lat || next.receiver.lon !== config.receiver.lon || next.receiver.located !== config.receiver.located;
      if (NET && next.aggregator) netAggregator = { ...netAggregator, ...next.aggregator, tarJson: netAggregator.tarJson };
      delete next.locked;
      delete next.aggregator;
      config = { ...defaultConfig(), ...next, version: 1 };
      if (moved) { const now = Date.now(); for (let i = 0; i < fleet.length; i++) spawn(i, now, false); for (let t = 600; t > 0; t -= 4) step(now - t * 1000, 4); }
      for (const c of sseClients) sseSend(c, 'status', forViewer('status', status(), viewOf(c)));
      return json(res, 200, NET ? { ...config, mode: 'aggregator', aggregator: netAggregator } : config);
    }
    if (p === '/api/factory-reset' && req.method === 'POST') {
      let body = {};
      try { body = JSON.parse(await readBody(req)); } catch { /* fallthrough */ }
      if (body.confirm !== 'RESET') return json(res, 400, { error: 'confirmation text must be RESET' });
      config = defaultConfig();
      for (const c of sseClients) { sseSend(c, 'notice', { level: 'warn', text: 'Factory reset complete', t: Date.now() }); sseSend(c, 'status', status()); }
      return json(res, 200, { ok: true });
    }
    if (p === '/api/aircraft' && req.method === 'GET') return json(res, 200, forViewer('aircraft', snapshot(), viewer));
    if (p.startsWith('/api/aircraft/') && req.method === 'GET') {
      const icao = decodeURIComponent(p.slice('/api/aircraft/'.length)).toUpperCase();
      if (!/^[0-9A-F]{6}$/.test(icao)) return json(res, 400, { error: 'invalid ICAO address' });
      const a = fleet.find((x) => x.icao === icao);
      const now = Date.now();
      return json(res, 200, {
        aircraft: a ? (viewer ? NET.projectSnapshot({ aircraft: [dto(a, now)], counts: {} }, viewer).aircraft[0] : dto(a, now)) : null,
        trail: trails.get(icao) || [],
        info: a ? { icao, reg: a.reg || '', type: a.type, typeDesc: a.typeDesc, operator: a.operator, country: a.country, countryCode: a.countryCode, military: !!a.military, categoryDesc: a.category } : {},
        flights: a ? [{ id: parseInt(icao, 16) % 100000, callsign: a.callsign, start: Math.floor(a.firstSeen / 1000), end: Math.floor(now / 1000), msgs: a.msgs, maxDistKm: round(a.dist || 0, 1) }] : [],
      });
    }
    if (p === '/api/trails' && req.method === 'GET') {
      const minutes = Math.max(1, Math.min(240, Number(u.searchParams.get('minutes')) || 15));
      const now = Date.now();
      const cut = now - minutes * 60000;
      const out = {};
      for (const a of fleet) {
        const t = trails.get(a.icao);
        if (!t) continue;
        const sub = t.filter((pt) => pt[0] >= cut);
        if (sub.length) out[a.icao] = sub;
      }
      return json(res, 200, { now, trails: out });
    }
    if (p === '/api/events' && req.method === 'GET') {
      if (args['no-sse']) return json(res, 503, { error: 'SSE disabled in mock (--no-sse)' });
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store', Connection: 'keep-alive', 'X-Accel-Buffering': 'no' });
      res.write('retry: 2000\n\n');
      res.__req = req;
      sseSend(res, 'status', forViewer('status', status(), viewer));
      sseSend(res, 'aircraft', forViewer('aircraft', snapshot(), viewer));
      sseClients.add(res);
      req.on('close', () => sseClients.delete(res));
      return;
    }
    if (p.startsWith('/tilecache/') && req.method === 'GET') {
      if (!config.http.tileCache) return json(res, 404, { error: 'tile cache disabled' });
      const rest = p.slice('/tilecache/'.length);
      const i = rest.indexOf('/');
      if (i < 0) return json(res, 400, { error: 'bad tilecache path' });
      return await tileProxy(res, rest.slice(0, i), rest.slice(i + 1), u.search);
    }
    if (p === '/api/alerts/recent' && req.method === 'GET') {
      const after = Number(u.searchParams.get('after') || 0);
      const limit = Math.max(1, Math.min(500, Number(u.searchParams.get('limit')) || 100));
      return json(res, 200, alertLog.filter((a) => a.id > after).slice(-limit).reverse());
    }
    if (p === '/api/timing/ntp/sample' && req.method === 'POST') {
      let body = {};
      try { body = JSON.parse((await readBody(req)) || '{}'); } catch { /* none */ }
      const server = String(u.searchParams.get('server') || body.server || '').trim();
      const now = Date.now();
      if (!server) {
        if (!config.ntp.enabled || !config.ntp.server) return json(res, 502, { error: 'no NTP server configured' });
        return json(res, 200, { server: config.ntp.server, addr: config.ntp.server + ':123', t: now, stratum: 1, refId: 'GPS', leap: 0, offsetMs: 0.41, delayMs: 0.62, rootDelayMs: 0, rootDispMs: 0.12 });
      }
      if (!/^(\[[0-9a-fA-F:]+\]|[A-Za-z0-9_.-]+)(:\d{1,5})?$/.test(server)) return json(res, 400, { error: 'invalid server address' });
      const norm = /:\d+$/.test(server) ? server : server + ':123';
      if (/fail|invalid|^10\.99\./.test(server)) return json(res, 200, { ok: false, server: norm, t: now, stratum: null, refId: '', leap: null, offsetMs: null, delayMs: null, rootDelayMs: null, rootDispMs: null, error: 'i/o timeout', saved: false });
      const pool = /pool|time\./.test(server);
      return json(res, 200, { ok: true, server: norm, addr: pool ? '162.159.200.1:123' : norm, t: now, stratum: pool ? 2 : 1, refId: pool ? '10.12.4.1' : 'GPS', leap: 0, offsetMs: pool ? 2.3 : 0.38, delayMs: pool ? 18.4 : 0.57, rootDelayMs: pool ? 4.1 : 0, rootDispMs: pool ? 9.6 : 0.11, saved: false });
    }
    if (p === '/api/timing/latency' && req.method === 'GET') {
      return json(res, 200, { now: Date.now(), clockOffsetMs: 0.4, clockSynced: !!config.ntp.enabled, feeds: [], ...latencyChain() });
    }
    if (p === '/api/timing/latency/aircraft' && req.method === 'GET') return json(res, 200, { aircraft: fleet.map(latencyRow).sort((a, b) => b.positions - a.positions) });
    if (p.startsWith('/api/timing/latency/aircraft/') && req.method === 'GET') {
      const icao = decodeURIComponent(p.split('/').pop()).toUpperCase();
      if (!/^[0-9A-F]{6}$/.test(icao)) return json(res, 400, { error: 'invalid ICAO address' });
      const a = fleet.find((x) => x.icao === icao);
      if (!a) return json(res, 404, { error: 'unknown aircraft' });
      const prop = (a.dist || 100) * 3.336e-3;
      return json(res, 200, { row: latencyRow(a), waterfall: { icao, t: Date.now(), df: 17, tc: 11, tFlag: a.v2 ? 1 : 0, cprOdd: false, feed: 'radarcape', exact: a.v2, slantKm: round(a.dist || 0, 1), elevDeg: 2.1, events: [], stages: [
        { key: 'positionAge', label: 'Position age', ns: 2e8, ms: 200, uncMs: 50, method: 'model' }, { key: 'propagation', label: 'Propagation', ns: prop * 1e6, ms: round(prop, 3), uncMs: 0.01, method: 'exact' },
        { key: 'receiver', label: 'Receiver', ns: 1.2e5, ms: 0.12, uncMs: 0.05, method: 'gps' }, { key: 'processing', label: 'Processing', ns: 1.8e6, ms: 1.8, uncMs: 0.5, method: 'ntp' }], totalMs: round(202 + prop, 3), totalUncMs: 50, notes: [] } });
    }
    if (p.startsWith('/api/timing/latency/feeds/') && req.method === 'GET') {
      const id = decodeURIComponent(p.split('/').pop());
      if (!config.feeds.some((f) => f.id === id)) return json(res, 404, { error: 'unknown feed' });
      const now = Date.now();
      const series = Array.from({ length: 60 }, (_, i) => ({ t: now - (60 - i) * 60000, n: 900, minMs: 1.1, p50Ms: 1.8, p95Ms: 3.9, maxMs: 9, meanMs: 2.1, floorMs: 1.1, jitterMs: 0.4, neg: 0, absurd: 0 }));
      return json(res, 200, { feed: id, feeds: config.feeds.map((f) => f.id), series });
    }
    if (p === '/api/timing/ntpcheck' && req.method === 'GET') {
      const on = !!config.ntp.enabled;
      return json(res, 200, {
        ntp: { enabled: on, synced: on, server: config.ntp.server, stratum: on ? 1 : 0, refId: on ? 'GPS' : '', leap: 0, offsetMs: on ? 0.4 : null, delayMs: on ? 0.6 : null, jitterMs: on ? 0.08 : null, rootDelayMs: 0, rootDispMs: on ? 0.12 : null, lastOk: on ? Date.now() - 12000 : 0, lastError: '' },
        quality: on ? 'good' : 'unknown', offsetUncertaintyMs: on ? 0.3 : nul(NaN), issues: on ? [] : ['NTP sampling is off'],
        feeds: config.feeds.map((f) => ({ feedId: f.id, samples: 600, negative: 0, absurd: 0, lastMs: 1.9, floorMs: 1.1, jitterMs: 0.4, driftUsPerMin: 0.2, intervals: 10, status: 'ok', issues: [], minima: [] })),
        explanation: 'Mock data.',
      });
    }
    if (p === '/api/stats/daily' && req.method === 'GET') return json(res, 200, mockDaily(Number(u.searchParams.get('days')) || 30));
    if ((p === '/api/reports' || p === '/api/reports.csv') && req.method === 'GET') {
      const rep = mockReport(u.searchParams);
      if (p === '/api/reports') return json(res, 200, rep);
      const cols = ['flightId', 'icao', 'callsign', 'reg', 'type', 'operator', 'start', 'end', 'msgs', 'maxDistKm', 'maxAlt', 'dep', 'arr'];
      const csv = [cols.join(',')].concat(rep.rows.map((r) => cols.map((c) => String(r[c] ?? '').replace(/[,\n"]/g, ' ')).join(','))).join('\n') + '\n';
      res.writeHead(200, { 'Content-Type': 'text/csv; charset=utf-8', 'Content-Disposition': 'attachment; filename="airview-report.csv"' });
      res.end(csv);
      return;
    }
    if (p.startsWith('/api/')) return json(res, 404, { error: `not implemented in the mock server: ${req.method} ${p}` });
    if (req.method !== 'GET' && req.method !== 'HEAD') { res.writeHead(405); res.end(); return; }
    return await serveStatic(res, p, req.method === 'HEAD');
  } catch (e) {
    console.error(req.method, req.url, e);
    if (!res.headersSent) json(res, 500, { error: String(e.message || e) });
    else res.end();
  }
});

server.listen(PORT, '127.0.0.1', () => {
  console.log(`AirView mock server: http://127.0.0.1:${PORT}/   (${N} aircraft, receiver ${config.receiver.located ? 'located' : 'NOT located'}, tile cache ${config.http.tileCache ? 'on' : 'off'}${NET ? ', NETWORK MODE' + (args.setup ? ' (setup pending, token MOCKTOKEN)' : '') : ''})`);
});
process.on('SIGINT', () => { for (const c of sseClients) c.end(); server.close(() => process.exit(0)); setTimeout(() => process.exit(0), 300); });
