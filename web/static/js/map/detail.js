// map/detail.js - the aircraft detail panel of the Live Map.
//
// Besides the live state it shows the route (offline standing data, plausibility-checked against the
// position), an optional photo (config.navData.photos), prediction / terrain visibility / closest
// approach, and offers the full stored trace of today and flight exports.

import { ui, h, setText } from '../ui.js';
import { units } from '../units.js';
import { shapeFor, shapeSvg } from './aircraft-icons.js';
import { isEmergency, hasPosition } from './list.js';
import { net } from '../net/store.js';
import { shortSid } from '../net/model.js';

/** Name of a station SID for the "Received by" row (from the picker's list). */
const stationName = (sid) => { const st = net.stations.find((x) => x.sid === sid); return st?.name || shortSid(sid); };

/** Prediction 1-sigma radius (m): metres / feet when small, distance units from 1 km. */
const sigmaText = (m) => (m >= 1000 ? units.dist(m / 1000) : units.height(m));

const D = '—';
const CATEGORY = {
  A0: 'No category information', A1: 'Light (< 15,500 lb)', A2: 'Small (15,500–75,000 lb)', A3: 'Large (75,000–300,000 lb)',
  A4: 'High-vortex large (B757)', A5: 'Heavy (> 300,000 lb)', A6: 'High performance (> 5 g, > 400 kt)', A7: 'Rotorcraft',
  B0: 'No category information', B1: 'Glider / sailplane', B2: 'Lighter-than-air', B3: 'Parachutist / skydiver', B4: 'Ultralight / hang-glider',
  B6: 'Unmanned aerial vehicle', B7: 'Space / trans-atmospheric vehicle',
  C0: 'No category information', C1: 'Surface emergency vehicle', C2: 'Surface service vehicle', C3: 'Point obstacle', C4: 'Cluster obstacle', C5: 'Line obstacle',
};
const SQUAWK = { 7500: 'Unlawful interference', 7600: 'Radio failure', 7700: 'General emergency', 7000: 'VFR conspicuity (EU)', 1200: 'VFR (US)', 2000: 'Entering SSR area' };
const EMERGENCY = { general: 'General emergency', medical: 'Medical emergency', minfuel: 'Minimum fuel', nordo: 'No communications', unlawful: 'Unlawful interference', downed: 'Downed aircraft' };
const ADDR = { icao: 'ICAO 24-bit', 'non-icao': 'Non-ICAO', 'tisb-track': 'TIS-B track file', anon: 'Anonymous', modeac: 'Mode A/C, no address (MLAT)' };
/** Mode A/C MLAT targets have a synthetic "#xxxxxx" id: no reference data, photo, route or intent lookups. */
const synthetic = (id) => typeof id === 'string' && id.startsWith('#');
const NAV = { autopilot: 'AP', vnav: 'VNAV', althold: 'ALT HOLD', approach: 'APP', lnav: 'LNAV', tcas: 'TCAS' };
const PHASE = { ground: 'On the ground', climb: 'Climbing', cruise: 'Cruise', descent: 'Descending', approach: 'Approach', holding: 'Holding', turning: 'Turning', unknown: 'Unknown' };
const SIL_TEXT = ['unknown', '≤ 1e-3 /h', '≤ 1e-5 /h', '≤ 1e-7 /h'];
const NACP_TEXT = ['unknown', '< 10 nm', '< 4 nm', '< 2 nm', '< 1 nm', '< 0.5 nm', '< 0.3 nm', '< 0.1 nm', '< 0.05 nm', '< 30 m', '< 10 m', '< 3 m'];
const INTENT_REFRESH_MS = 15000;

const n = (v) => typeof v === 'number' && Number.isFinite(v);
const yesNo = (v) => (v === true ? 'Yes' : v === false ? 'No' : D);

/** External lookup links for an aircraft (opened in a new window; nothing is fetched by AirView). */
function lookups(a, icao) {
  const hex = icao.toLowerCase();
  const out = [
    { label: 'ADS-B Exchange', href: `https://globe.adsbexchange.com/?icao=${hex}` },
    { label: 'Planespotters', href: `https://www.planespotters.net/hex/${icao}` },
    { label: 'OpenSky Network', href: `https://opensky-network.org/aircraft-profile?icao24=${hex}` },
  ];
  const cs = (a?.callsign || '').trim();
  if (/^[A-Z]{3}\d/.test(cs)) out.push({ label: `FlightAware ${cs}`, href: `https://www.flightaware.com/live/flight/${encodeURIComponent(cs)}` });
  return out;
}

/** Start a download of a same-origin URL (the server sends Content-Disposition: attachment). */
export function download(href) {
  const a = h('a', { href, download: '' });
  document.body.appendChild(a);
  a.click();
  a.remove();
}

/**
 * @param {object} ctx page context
 * @param {{ onClose(): void, onFollow(on: boolean): void, onCenter(): void, colorOf(a): string,
 *           onTrace(on: boolean): Promise<{points: number, flights: number}|null>, onRouteInfo(info|null): void }} hooks
 */
export function createDetail(ctx, hooks) {
  let icao = null;
  let last = null; // last live DTO seen for this aircraft
  let info = null;
  let following = false;
  let seq = 0;
  let sparks = null; // { alt, gs, rssi: { chart, data } }
  let rows = null; // label -> setter
  let lastSparkPush = 0;
  let traceOn = false;
  let intent = null;
  let intentTimer = 0;
  let path = null;
  let routeInfo = null; // /api/navdata/route reply
  let routeFor = ''; // callsign the route was fetched for
  let routeState = 'idle';
  let detailFlights = [];

  const el = h('section.mapv-detail.hidden', { 'aria-label': 'Aircraft details' });

  const antennaName = (id) => {
    const ant = (ctx.config.current?.antennas || []).find((x) => x.id === id);
    return ant ? ant.name || id : id;
  };

  function build(a) {
    const head = {
      shape: h('span.det-shape'),
      title: h('h2.det-title'),
      sub: h('div.det-sub'),
      badges: h('div.det-badges'),
    };
    const followBtn = ui.button({ label: 'Follow', icon: 'follow', size: 'sm', pressed: following, tip: 'Keep the map centred on this aircraft (F)', onClick: () => { following = !following; followBtn.setPressed(following); hooks.onFollow(following); } });
    const centreBtn = ui.button({ label: 'Centre', icon: 'locate', size: 'sm', tip: 'Centre the map on this aircraft', onClick: () => hooks.onCenter() });
    const linkBtn = ui.button({ label: 'Look up', icon: 'external', size: 'sm', tip: 'Open this aircraft on an external tracking site', onClick: () => {
      ui.menu(linkBtn, lookups(last, icao).map((l) => ({ label: l.label, icon: 'external', onClick: () => window.open(l.href, '_blank', 'noopener,noreferrer') })), { placement: 'bottom-start' });
    } });
    const traceBtn = ui.button({ label: 'Full trace', icon: 'history', size: 'sm', pressed: traceOn, tip: 'Show every stored position of today (from the history database)', onClick: () => setTrace(!traceOn) });
    const exportBtn = ui.button({ label: 'Export', icon: 'download', size: 'sm', tip: 'Download this flight', onClick: () => openExport(exportBtn) });
    const copyBtn = ui.button({ icon: 'link', size: 'sm', variant: 'ghost', tip: 'Copy a link to this aircraft', onClick: async () => { (await ui.copy(location.href)) ? ui.toast.success('Link copied') : ui.toast.warn('Could not copy the link'); } });
    const closeBtn = ui.button({ icon: 'x', variant: 'ghost', size: 'sm', tip: 'Close (Esc)', onClick: () => hooks.onClose() });

    const tiles = {
      alt: tile('Altitude'), spd: tile('Speed'), dist: tile('Distance'), sig: tile('Signal'),
    };
    const sparkBox = (label) => { const c = h('div.det-spark-chart'); const v = h('span.num'); return { el: h('div.det-spark', h('div.det-spark-head', h('span.label', label), v), c), chart: c, value: v }; };
    const sAlt = sparkBox('Altitude');
    const sGs = sparkBox('Ground speed');
    const sRssi = sparkBox('Signal');

    rows = new Map();
    const group = (title, iconName, defs) => {
      const kv = ui.kv(defs.map(([label, , o]) => [label, D, o]));
      defs.forEach(([label, fn], i) => rows.set(title + '|' + label, { fn, set: (v) => kv.set(i, v) }));
      return h('div.det-group', ui.section(title, iconName), kv);
    };
    const navEl = h('div.det-nav');
    const predText = h('span');
    const predBanner = h('div.det-pred.hidden', { role: 'note' }, h('span.det-pred-dot'), h('div.grow', h('b', 'Predicted position'), predText));
    const routeEl = h('div.det-route.hidden');
    const photoEl = h('div.det-photo.hidden');

    const body = h('div.det-body',
      predBanner,
      routeEl,
      h('div.det-tiles', tiles.alt.el, tiles.spd.el, tiles.dist.el, tiles.sig.el),
      photoEl,
      h('div.det-sparks', sAlt.el, sGs.el, sRssi.el),
      // network mode: MLAT grade and the receivers - own stations for owners, every station for the admin,
      // nothing for the public (the server sends no `stations` field to them, DESIGN §8.2)
      net.aggregator ? group('Network', 'globe', [
        ['MLAT fix grade', (x) => (x.mlatGrade ? `${x.mlatGrade}${n(x.mlatSigmaM) ? ` · ±${x.mlatSigmaM >= 1000 ? units.dist(x.mlatSigmaM / 1000) : units.num(x.mlatSigmaM, 0) + ' m'} (1σ)` : ''}` : D), { text: true }],
        [net.scope === 'admin' ? 'Received by' : 'Your stations', (x) => (net.scope === 'public' ? D : x.stations?.length ? x.stations.map(stationName).join(', ') : x.mine ? 'yes' : net.scope === 'admin' ? D : 'none'), { text: true }],
      ]) : null,
      group('Prediction & visibility', 'target', [
        ['Position', (x) => (x.predicted ? `Predicted · ${units.age(x.coastS)} without a position` : hasPosition(x) ? 'Received' : 'No position'), { text: true }],
        ['Uncertainty (1σ, may have turned)', (x) => (x.predicted && n(x.predSigmaM) ? sigmaText(x.predSigmaM) : D)],
        ['Flight phase', () => (intent && intent.phase ? PHASE[intent.phase] || intent.phase : D), { text: true }],
        ['Closest approach', (x) => (n(x.cpaKm) ? `${units.dist(x.cpaKm)} ${n(x.cpaInS) && x.cpaInS > 0 ? 'in ' + units.age(x.cpaInS) : '(now, moving away)'}` : D)],
        ['Terrain line of sight', (x) => (typeof x.terrainVisible !== 'boolean' ? D : `${x.terrainVisible ? 'Visible' : 'Hidden by terrain'}${n(x.losMarginDeg) ? ` (${x.losMarginDeg > 0 ? '+' : ''}${units.num(x.losMarginDeg, 2, 2)}°)` : ''}`), { text: true }],
        ['Terrain forecast', () => terrainForecast(), { text: true }],
      ]),
      group('Identity', 'plane', [
        ['ICAO address', () => icao],
        ['Callsign', (x) => x.callsign || D],
        ['Registration', (x) => x.reg || info?.reg || D],
        ['Type', (x) => [x.type || info?.type, x.typeDesc || info?.typeDesc].filter(Boolean).join(' · ') || D, { text: true }],
        ['Operator', (x) => x.operator || info?.operator || D, { text: true }],
        ['Country', (x) => x.country || info?.country || D, { text: true }],
        ['Category', (x) => (x.category ? `${x.category} · ${CATEGORY[x.category] || 'Reserved'}` : D), { text: true }],
        ['Squawk', (x) => (x.squawk ? x.squawk + (SQUAWK[x.squawk] ? ` · ${SQUAWK[x.squawk]}` : '') : D)],
        ['Address type', (x) => ADDR[x.addrType] || x.addrType || D, { text: true }],
        ['Alert rule', (x) => (x.highlight ? `${x.highlight.label || x.highlight.ruleId} (${x.highlight.severity || 'info'})` : 'None'), { text: true }],
      ]),
      group('Position & geometry', 'pin', [
        ['Position', (x) => (hasPosition(x) ? units.latlon(x.lat, x.lon) : 'No position')],
        ['Distance', (x) => units.dist(x.distKm)],
        ['Bearing from receiver', (x) => units.bearing(x.bearing)],
        ['Elevation angle', (x) => (n(x.elevation) ? units.num(x.elevation, 1, 1) + '°' : D)],
        ['Slant range', (x) => units.dist(x.slantKm)],
        ['Position age', (x) => (n(x.posAge) ? units.num(x.posAge, 1, 1) + ' s' : D)],
        ['On ground', (x) => yesNo(x.onGround)],
        ['Source', (x) => ui.sourceLabel(x.source), { text: true }],
      ]),
      group('Altitude & speed', 'chart', [
        ['Barometric altitude', (x) => units.alt(x.altBaro)],
        ['Geometric altitude', (x) => units.alt(x.altGeom)],
        ['Vertical rate', (x) => units.vrate(x.vertRate)],
        ['Ground speed', (x) => units.speed(x.gs)],
        ['Track', (x) => units.bearing(x.track)],
        ['Indicated airspeed', (x) => units.speed(x.ias)],
        ['True airspeed', (x) => units.speed(x.tas)],
        ['Mach', (x) => (n(x.mach) ? x.mach.toFixed(3) : D)],
        ['Magnetic heading', (x) => units.deg(x.magHeading)],
        ['Roll', (x) => (n(x.roll) ? units.num(Math.abs(x.roll), 1, 1) + '° ' + (x.roll > 0.05 ? 'R' : x.roll < -0.05 ? 'L' : '') : D)],
        ['Turn rate', (x) => (n(x.trackRate) ? units.num(x.trackRate, 2, 2) + ' °/s' : D)],
      ]),
      h('div.det-group', ui.section('Autopilot / selected', 'target'),
        (() => {
          const kv = ui.kv([['Selected altitude', D], ['Selected heading', D], ['QNH', D]]);
          rows.set('ap|alt', { fn: (x) => units.alt(x.selAlt), set: (v) => kv.set(0, v) });
          rows.set('ap|hdg', { fn: (x) => units.deg(x.selHeading), set: (v) => kv.set(1, v) });
          rows.set('ap|qnh', { fn: (x) => units.pressure(x.qnh), set: (v) => kv.set(2, v) });
          return kv;
        })(), navEl),
      group('Integrity', 'shield', [
        ['ADS-B version', (x) => (n(x.adsbVersion) ? `v${x.adsbVersion}${x.adsbVersion === 2 ? ' (DO-260B)' : x.adsbVersion === 1 ? ' (DO-260A)' : x.adsbVersion === 0 ? ' (DO-260)' : ''}` : D)],
        ['NIC', (x) => (n(x.nic) ? String(x.nic) : D)],
        ['Containment radius', (x) => (n(x.rcM) ? (x.rcM >= 1852 ? units.dist(x.rcM / 1000) : units.num(x.rcM, 0) + ' m') : D)],
        ['NACp', (x) => (n(x.nacp) ? `${x.nacp} · ${NACP_TEXT[x.nacp] || ''}` : D)],
        ['NACv', (x) => (n(x.nacv) ? String(x.nacv) : D)],
        ['SIL', (x) => (n(x.sil) ? `${x.sil} · ${SIL_TEXT[x.sil] || ''}` : D)],
        ['Alert / SPI', (x) => `${x.alert ? 'Alert' : 'No'} / ${x.spi ? 'Ident' : 'No'}`],
        ['Emergency', (x) => (x.emergency ? EMERGENCY[x.emergency] || x.emergency : 'None'), { text: true }],
      ]),
      group('Reception', 'antenna', [
        ['RSSI now', (x) => units.dbfs(x.rssi)],
        ['RSSI average', (x) => units.dbfs(x.rssiAvg)],
        ['Message rate', (x) => (n(x.msgRate) ? units.num(x.msgRate, 1, 1) + ' /s' : D)],
        ['Messages', (x) => units.int(x.msgs)],
        ['Heard by', (x) => (x.antennas?.length ? x.antennas.map(antennaName).join(', ') : D), { text: true }],
        ['Feeds', (x) => (x.feeds?.length ? x.feeds.join(', ') : D), { text: true }],
        ['First seen', (x) => (x.firstSeen ? `${units.clock(x.firstSeen)} · ${units.age((ctx.live.serverNow() - x.firstSeen) / 1000)}` : D)],
        ['Last seen', (x) => (x.lastSeen ? units.age(Math.max(0, (ctx.live.serverNow() - x.lastSeen) / 1000)) + ' ago' : D)],
      ]),
      group('Derived weather aloft', 'wind', [
        ['Wind', (x) => (n(x.windDir) && n(x.windSpd) ? `${units.deg(x.windDir)} / ${units.speed(x.windSpd)}` : D)],
        ['Outside air temp', (x) => units.temp(x.oat)],
      ]));

    ui.mount(el,
      h('header.det-head',
        h('div.det-head-main', head.shape, h('div.grow', head.title, head.sub), copyBtn, closeBtn),
        head.badges,
        h('div.det-actions', followBtn, centreBtn, linkBtn),
        h('div.det-actions', traceBtn, exportBtn)),
      body);

    el.__parts = { head, tiles, sAlt, sGs, sRssi, navEl, followBtn, traceBtn, predBanner, predText, routeEl, photoEl };
    return a;
  }

  function terrainForecast() {
    const t = path && path.terrain;
    if (!t || !t.known) return D;
    const now = ctx.live.serverNow();
    if (t.lostAtMs > 0) return `Hidden by terrain in ${units.age(Math.max(0, (t.lostAtMs - now) / 1000))}`;
    if (t.acquiredAtMs > 0) return `Visible again in ${units.age(Math.max(0, (t.acquiredAtMs - now) / 1000))}`;
    return t.visibleNow ? 'Stays visible along the predicted path' : 'Stays hidden along the predicted path';
  }

  function tile(label) {
    const v = h('span.num.det-tile-v', D);
    const u = h('span.unit');
    const s = h('span.det-tile-s');
    return { el: h('div.det-tile', h('span.label', label), h('div', v, u), s), v, u, s };
  }

  function splitUnit(str) {
    const i = str.lastIndexOf(' ');
    return i < 0 ? [str, ''] : [str.slice(0, i), str.slice(i + 1)];
  }

  // ---- route ----
  function routeView(a) {
    const r = a.route;
    const ri = routeInfo && routeInfo.found && routeFor === (a.callsign || '').trim() ? routeInfo : null;
    if (!r && !ri) {
      if (routeState === 'loading') return h('div.det-route-empty', h('span.spinner'), 'Looking up the route…');
      return null;
    }
    const aps = ri ? ri.airports || [] : [];
    const first = aps[0];
    const lastAp = aps[aps.length - 1];
    const code = (ap, fallback, iata) => (ap?.airport?.iata || iata || ap?.code || fallback || '?');
    const name = (ap, fallback) => ap?.airport?.municipality || ap?.airport?.name || fallback || '';
    const from = { code: code(first, r?.from, r?.fromIata), name: name(first, r?.fromName), icao: first?.airport?.icao || r?.from };
    const to = { code: code(lastAp, r?.to, r?.toIata), name: name(lastAp, r?.toName), icao: lastAp?.airport?.icao || r?.to };
    const via = r ? r.via || 0 : Math.max(0, aps.length - 2);
    // The live route (internal/routes) is the fused, scored active leg: it wins over the lazily
    // fetched /api/navdata/route answer, which only adds the distance.
    if (r && r.from) {
      from.code = r.fromIata || r.from; from.name = r.fromName || from.name; from.icao = r.from;
      to.code = r.toIata || r.to; to.name = r.toName || to.name; to.icao = r.to;
    }
    const states = {
      fits: ['Route fits', 'green'], uncertain: ['Route uncertain', 'other'], doubtful: ['Doubtful route', 'amber'],
      reversed: ['Reversed?', 'amber'], unchecked: ['Unchecked', 'other'],
    };
    let badge;
    const check = ri?.check;
    if (r && r.state && states[r.state]) badge = ui.badge(states[r.state][0], states[r.state][1], { tip: r.reason || '' });
    else if (check && (check.plausible || check.doubtful)) badge = check.plausible ? ui.badge('Route fits', 'green', { tip: check.reason }) : ui.badge('Doubtful route', 'amber', { tip: check.reason || 'The position does not fit this route' });
    else if (r && r.checked) badge = r.plausible ? ui.badge('Route fits', 'green', { tip: 'The current position lies inside the route corridor' }) : ui.badge('Doubtful route', 'amber', { tip: 'The current position does not fit the published route for this callsign' });
    else badge = ui.badge('Unchecked', 'other', { tip: 'Plausibility needs a position and both airports' });
    const srcNames = { override: 'operator', learned: 'learned', 'standing-data': 'standing data', adsbdb: 'adsbdb.com' };
    const srcKey = r?.source || ri?.source;
    let src = srcNames[srcKey] || 'standing data';
    if (r && n(r.conf)) src += ` · ${Math.round(r.conf * 100)} %`;
    if (r && (r.flags || []).includes('variant')) src += ' · similar callsign';
    const srcTip = [r?.reason, srcKey === 'adsbdb' ? 'Route data: adsbdb.com (shown to this station\'s users only)' : srcKey === 'learned' ? 'Learned by AirView from observed take-offs and landings' : ''].filter(Boolean).join(' - ');
    let progress = null;
    if (r && n(r.along) && r.state !== 'unchecked') {
      const eta = n(r.eta) && r.eta > 0 ? new Date(r.eta * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '';
      const sig = n(r.etaSigma) && r.etaSigma > 0 ? ` ±${Math.max(1, Math.round(r.etaSigma / 60))} min` : '';
      const bar = h('div.det-route-prog', h('i', { style: { width: Math.max(0, Math.min(100, r.along)) + '%' } }));
      progress = h('div.det-route-progwrap', bar,
        h('div.det-route-progtxt',
          r.legs > 1 ? h('span.muted', `leg ${r.leg}/${r.legs}`) : null,
          n(r.distRem) ? h('span.muted', `${units.dist(r.distRem)} to go`) : null,
          eta ? h('span', { tip: 'Estimated arrival (station clock, your time zone); a guess from distance, speed and a standard descent profile' }, `ETA ${eta}${sig}`) : null));
    }
    return h('div.det-route-box',
      h('div.det-route-line',
        h('div.det-route-ap', h('b.num', { tip: from.icao }, from.code), h('span', from.name)),
        h('div.det-route-mid', h('i'), via ? h('span.muted', `${via} stop${via > 1 ? 's' : ''}`) : null),
        h('div.det-route-ap.end', h('b.num', { tip: to.icao }, to.code), h('span', to.name))),
      progress,
      h('div.det-route-foot', badge, n(ri?.distanceKm) ? h('span.muted', units.dist(ri.distanceKm)) : null, h('span.muted', { tip: srcTip }, src)));
  }

  function renderRoute(a) {
    const p = el.__parts;
    if (!p) return;
    const ri = routeInfo && routeFor === (a.callsign || '').trim() ? routeInfo : null;
    const sig = `${JSON.stringify(a.route || null)}|${ri ? ri.callsign + (ri.check?.plausible ? 1 : 0) + (ri.check?.doubtful ? 1 : 0) : ''}|${routeState}|${units.system}`;
    if (p.routeEl.__s === sig) return;
    p.routeEl.__s = sig;
    const v = routeView(a);
    p.routeEl.classList.toggle('hidden', !v);
    if (v) ui.mount(p.routeEl, v); else ui.clear(p.routeEl);
  }

  async function loadRoute(a) {
    const cs = (a.callsign || '').trim();
    if (!cs || cs === routeFor) return;
    const my = seq;
    routeFor = cs;
    routeInfo = null;
    routeState = 'loading';
    hooks.onRouteInfo(null);
    try {
      const r = await ctx.api.get('/api/navdata/route/' + encodeURIComponent(cs), { params: hasPosition(a) && !a.predicted ? { lat: a.lat.toFixed(4), lon: a.lon.toFixed(4) } : undefined, signal: ctx.signal, timeout: 15000 });
      if (my !== seq || routeFor !== cs) return;
      routeInfo = r && r.found ? r : null;
      routeState = routeInfo ? 'ok' : 'empty';
      hooks.onRouteInfo(routeInfo);
    } catch (e) {
      if (e.aborted || my !== seq || routeFor !== cs) return;
      routeState = 'error';
    }
    if (last) renderRoute(last);
  }

  // ---- photo ----
  async function loadPhoto(a) {
    const p = el.__parts;
    if (!p) return;
    const cfg = ctx.config.current?.navData;
    if (!cfg || !cfg.photos || synthetic(a?.icao)) { p.photoEl.classList.add('hidden'); return; }
    const my = seq;
    p.photoEl.classList.remove('hidden');
    ui.mount(p.photoEl, ui.skeleton({ lines: 0, block: 150 }));
    try {
      const r = await ctx.api.get('/api/navdata/photo/' + encodeURIComponent(icao), { params: { reg: a.reg || info?.reg || undefined }, signal: ctx.signal, timeout: 20000 });
      if (my !== seq || !el.__parts) return;
      const img = r && r.found && (r.thumbnail?.src || r.large?.src);
      if (!img) { ui.mount(p.photoEl, h('div.det-photo-none', 'No photo of this airframe found')); return; }
      const link = typeof r.link === 'string' && /^https:\/\//.test(r.link) ? r.link : null;
      const pic = h('img', { src: img, alt: `Photo of ${a.reg || icao}`, loading: 'lazy', referrerpolicy: 'no-referrer' });
      ui.mount(p.photoEl,
        link ? h('a', { href: link, target: '_blank', rel: 'noopener noreferrer' }, pic) : pic,
        h('div.det-photo-cap', r.attribution || (r.photographer ? `© ${r.photographer}` : '')));
    } catch (e) {
      if (e.aborted || my !== seq || !el.__parts) return;
      if (e.status === 409) { p.photoEl.classList.add('hidden'); return; }
      ui.mount(p.photoEl, h('div.det-photo-none', 'Photo unavailable (' + e.message + ')'));
    }
  }

  // ---- intent (flight phase) ----
  async function loadIntent() {
    clearTimeout(intentTimer);
    if (!icao || synthetic(icao) || ctx.config.current?.prediction?.enabled === false) return;
    const my = seq;
    try {
      const r = await ctx.api.get('/api/prediction/intent/' + encodeURIComponent(icao), { signal: ctx.signal, timeout: 10000 });
      if (my !== seq) return;
      intent = r;
    } catch (e) {
      if (e.aborted || my !== seq) return;
      intent = null;
    }
    if (last && my === seq) render(last, !ctx.live.get(icao));
    if (my === seq) intentTimer = setTimeout(loadIntent, INTENT_REFRESH_MS);
  }

  // ---- trace / export ----
  async function setTrace(on) {
    const p = el.__parts;
    traceOn = on;
    if (p) { p.traceBtn.setPressed(on); p.traceBtn.setLoading(on); }
    let r = null;
    try { r = await hooks.onTrace(on); } finally { if (el.__parts) el.__parts.traceBtn.setLoading(false); }
    if (!on || !el.__parts) return;
    if (!r) { traceOn = false; el.__parts.traceBtn.setPressed(false); return; }
    if (!r.points) ui.toast('No stored positions of this aircraft today yet.');
  }

  function openExport(anchor) {
    const fid = last?.flightId || detailFlights[0]?.id;
    if (!fid) { ui.toast('This aircraft has no stored flight to export yet.'); return; }
    const base = `/api/export/flight/${encodeURIComponent(fid)}`;
    ui.menu(anchor, [
      { label: 'Google Earth (KML)', icon: 'download', onClick: () => download(base + '.kml') },
      { label: 'GeoJSON', icon: 'download', onClick: () => download(base + '.geojson') },
      { label: 'CSV', icon: 'download', onClick: () => download(base + '.csv') },
      'sep',
      { label: 'Open in Reports', icon: 'report', onClick: () => ctx.navigate('reports/flight/' + fid) },
    ], { placement: 'bottom-start' });
  }

  function render(a, lost) {
    const p = el.__parts;
    if (!p) return;
    const color = lost ? '#8fa3c4' : hooks.colorOf(a);
    const shape = shapeFor(info && info.typeInfo && !a.typeInfo ? { ...a, typeInfo: info.typeInfo } : a);
    const sig = `${shape}|${color}|${Math.round((a.track || 0) / 5)}`;
    if (p.head.shape.__s !== sig) { p.head.shape.__s = sig; p.head.shape.innerHTML = shapeSvg(shape, color, 34, a.track || 0); }
    p.head.shape.classList.toggle('pred', !!a.predicted && !lost);
    p.head.shape.style.setProperty('--c', color);
    setText(p.head.title, a.callsign || icao);
    setText(p.head.sub, [a.reg || info?.reg, a.typeDesc || info?.typeDesc || a.type, a.operator || info?.operator].filter(Boolean).join(' · ') || (a.callsign ? icao : 'No identification received yet'));
    const hl = a.highlight;
    const bsig = `${a.source}|${a.military}|${isEmergency(a)}|${a.squawk}|${a.countryCode}|${lost}|${a.emergency}|${a.predicted}|${hl ? hl.ruleId + hl.color : ''}|${a.terrainVisible}`;
    if (p.head.badges.__s !== bsig) {
      p.head.badges.__s = bsig;
      ui.mount(p.head.badges,
        ui.flag(a.countryCode || info?.countryCode, a.country || info?.country),
        ui.badge(icao, 'blue', { tip: 'ICAO 24-bit address' }),
        a.predicted && !lost ? ui.badge('Predicted', 'violet', { tip: 'No position received for a few seconds: shown where the tracking filter predicts it' }) : ui.sourceBadge(a.source),
        a.military && ui.badge('Military', 'green'),
        isEmergency(a) && ui.badge(`${a.squawk || ''} ${EMERGENCY[a.emergency] || SQUAWK[a.squawk] || 'Emergency'}`.trim(), 'solid-red'),
        hl && h('span.badge.det-hl', { style: { '--hl': /^#[0-9a-f]{3,8}$/i.test(hl.color || '') ? hl.color : '#ffd60a' }, tip: `Alert rule: ${hl.label || hl.ruleId}` }, hl.label || 'Alert'),
        a.terrainVisible === false && ui.badge('Behind terrain', 'amber', { tip: 'Below the terrain radio horizon of the antennas' }),
        lost && ui.badge('Signal lost', 'red'));
    }
    // prediction banner
    p.predBanner.classList.toggle('hidden', !a.predicted || lost);
    if (a.predicted && !lost) {
      p.predBanner.style.setProperty('--c', color);
      setText(p.predText, `No position for ${units.age(a.coastS)}${n(a.predSigmaM) ? ` · 1σ ±${sigmaText(a.predSigmaM)}, assuming it may have turned` : ''}`);
    }
    renderRoute(a);
    // headline tiles
    const altV = a.onGround ? null : a.altBaro ?? a.altGeom;
    const [av, au] = a.onGround ? ['GND', ''] : splitUnit(units.alt(altV));
    setText(p.tiles.alt.v, av); setText(p.tiles.alt.u, au);
    setText(p.tiles.alt.s, n(a.vertRate) && Math.abs(a.vertRate) > 100 ? `${a.vertRate > 0 ? '↑' : '↓'} ${units.vrate(Math.abs(a.vertRate), { sign: false })}` : n(altV) ? 'level' : '');
    p.tiles.alt.s.className = 'det-tile-s ' + (n(a.vertRate) && a.vertRate > 100 ? 'up' : n(a.vertRate) && a.vertRate < -100 ? 'down' : '');
    const [sv, su] = splitUnit(units.speed(a.gs));
    setText(p.tiles.spd.v, sv); setText(p.tiles.spd.u, su);
    setText(p.tiles.spd.s, n(a.track) ? `track ${units.deg(a.track)}` : '');
    const [dv, du] = splitUnit(units.dist(a.distKm));
    setText(p.tiles.dist.v, dv); setText(p.tiles.dist.u, du);
    setText(p.tiles.dist.s, n(a.bearing) ? `${units.bearing(a.bearing)}` : hasPosition(a) ? 'receiver not located' : 'no position');
    const rs = a.rssiAvg ?? a.rssi;
    setText(p.tiles.sig.v, n(rs) ? units.num(rs, 1, 1) : D); setText(p.tiles.sig.u, n(rs) ? 'dBFS' : '');
    setText(p.tiles.sig.s, n(a.msgRate) ? `${units.num(a.msgRate, 1, 1)} msg/s` : '');
    // rows
    for (const r of rows.values()) {
      let v;
      try { v = r.fn(a); } catch { v = D; }
      r.set(v);
    }
    const nav = a.navModes || [];
    const nsig = nav.join(',');
    if (p.navEl.__s !== nsig) { p.navEl.__s = nsig; ui.mount(p.navEl, nav.length ? nav.map((m) => ui.badge(NAV[m] || m, 'blue')) : h('span.muted', 'No autopilot modes reported')); }
    setText(p.sAlt.value, a.onGround ? 'GND' : units.alt(altV));
    setText(p.sGs.value, units.speed(a.gs));
    setText(p.sRssi.value, units.dbfs(rs));
  }

  function initSparks(trail) {
    const p = el.__parts;
    const series = { alt: [], gs: [], rssi: [] };
    for (const pt of trail || []) {
      if (n(pt[3])) series.alt.push([pt[0], pt[3]]);
      if (n(pt[4])) series.gs.push([pt[0], pt[4]]);
      if (n(pt[5])) series.rssi.push([pt[0], pt[5]]);
    }
    const tipTime = (t) => units.clock(t, { z: false });
    sparks = {
      alt: { data: series.alt, color: '#6aa8ff', fmt: (v, t) => `${tipTime(t)}  ${units.alt(v)}`, chart: null, host: p.sAlt.chart },
      gs: { data: series.gs, color: '#22d3ee', fmt: (v, t) => `${tipTime(t)}  ${units.speed(v)}`, chart: null, host: p.sGs.chart },
      rssi: { data: series.rssi, color: '#34d399', fmt: (v, t) => `${tipTime(t)}  ${units.dbfs(v)}`, chart: null, host: p.sRssi.chart },
    };
    for (const s of Object.values(sparks)) {
      s.chart = ctx.charts.sparkline(s.host, s.data, { color: s.color, tooltip: s.fmt });
    }
  }

  function pushSparks(a, now) {
    if (!sparks || now - lastSparkPush < 2000 || a.predicted) return;
    lastSparkPush = now;
    const add = (s, v) => {
      if (!n(v)) return;
      s.data.push([now, v]);
      if (s.data.length > 1500) s.data.splice(0, s.data.length - 1500);
      s.chart.set({ series: [{ data: s.data }] });
    };
    add(sparks.alt, a.onGround ? 0 : a.altBaro ?? a.altGeom);
    add(sparks.gs, a.gs);
    add(sparks.rssi, a.rssiAvg ?? a.rssi);
  }

  function disposeSparks() {
    if (!sparks) return;
    for (const s of Object.values(sparks)) if (s.chart) s.chart.dispose();
    sparks = null;
  }

  function resetExtras() {
    clearTimeout(intentTimer);
    intent = null;
    path = null;
    routeInfo = null;
    routeFor = '';
    routeState = 'idle';
    detailFlights = [];
    if (traceOn) { traceOn = false; hooks.onTrace(false); }
  }

  async function show(id, opts = {}) {
    const my = ++seq;
    resetExtras();
    icao = id;
    info = null;
    following = !!opts.follow;
    disposeSparks();
    last = ctx.live.get(id) || { icao: id, source: 'other', msgs: 0, msgRate: 0, firstSeen: 0, lastSeen: 0 };
    build(last);
    el.classList.remove('hidden');
    el.scrollTop = 0;
    render(last, !ctx.live.get(id));
    loadRoute(last);
    loadIntent();
    let detail = null;
    try {
      detail = await ctx.api.aircraftDetail(id, { signal: ctx.signal });
    } catch (e) {
      if (my !== seq || e.aborted) return null;
      detail = { aircraft: null, trail: [], info: {}, flights: [] };
    }
    if (my !== seq) return null;
    info = detail.info || {};
    detailFlights = Array.isArray(detail.flights) ? detail.flights : [];
    if (!ctx.live.get(id) && detail.aircraft) last = detail.aircraft;
    render(last, !ctx.live.get(id));
    if (!last.callsign && detail.aircraft?.callsign) loadRoute(detail.aircraft);
    initSparks(detail.trail);
    loadPhoto(last);
    return detail;
  }

  return {
    el,
    get icao() { return icao; },
    get following() { return following; },
    get flights() { return detailFlights; },
    show,
    /** Called once per snapshot while open. */
    update(now) {
      if (!icao) return;
      const a = ctx.live.get(icao);
      if (a) {
        last = a;
        render(a, false);
        pushSparks(a, now);
        if ((a.callsign || '').trim() && (a.callsign || '').trim() !== routeFor) loadRoute(a);
      } else if (last) render(last, true);
    },
    /** Predicted path of the selected aircraft (for the terrain forecast row), or null. */
    setPath(p) { path = p; },
    setFollowing(on) {
      following = !!on;
      if (el.__parts) el.__parts.followBtn.setPressed(following);
    },
    /** Toggle the full-trace button from outside (keyboard). */
    setTraceState(on) { traceOn = !!on; if (el.__parts) el.__parts.traceBtn.setPressed(traceOn); },
    hide() {
      seq++;
      resetExtras();
      icao = null;
      last = null;
      following = false;
      disposeSparks();
      el.classList.add('hidden');
      el.__parts = null;
      ui.clear(el);
    },
    /** Re-render after a unit change. */
    refresh() { if (icao && last) { if (el.__parts) el.__parts.routeEl.__s = ''; render(last, !ctx.live.get(icao)); } },
    destroy() { disposeSparks(); clearTimeout(intentTimer); },
  };
}
