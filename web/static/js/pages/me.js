// pages/me.js - "My stations" (network / aggregator mode, owners and the admin):
//   #/me, #/me/stations          the viewer's stations as cards
//   #/me/stations/<sid>[/<tab>]  one station: Connection, Reception, Timing & MLAT, Location, Privacy & data
// (DESIGN §11). The station's own statistics come from /api/me/stations/{sid}/stats (live, refreshed every
// 10 s) and its connection log from /api/me/stations/{sid}/connections; the admin additionally gets links to
// the single-station pages (Statistics, Antenna Lab, ...), which show the host station.
//
// API (net/endpoints.js): GET /api/me/stations, GET /api/me/stations/{sid}, POST /api/me/stations/{sid}
// (name, pin, showName, publicStats, declared), GET .../stats, GET .../connections, POST .../unclaim,
// GET /api/me/export. Every field is optional: what the server does not send shows as a dash.

import { EP, normStation, normStationDetail, uptimeSegments } from '../net/endpoints.js';
import { clockLabel, KIND_LABELS, LOC_LABELS, PIN_OPTIONS, PROTO_LABELS, publicPin, shortSid, surveyNoticeText } from '../net/model.js';
import { pageHead, loadInto, onlineBadge, admissionBadge, v } from '../net/view.js';
import { timingSection } from '../mlat/owner.js';
import { consentBlock, joinInfo, C6_NOTICE, C6_NOTICE_LINK } from '../net/consent.js';
import { geo } from '../geo.js';
import { comparisonCard, confirmSurveyFlow, locateCard, noticesCard, surveyCard, surveyPoint } from '../net/locationview.js';
import { locBadge } from '../net/location.js';
import { renderMyShares, openCreateShare } from '../net/shares.js';

// MLAT position sources (internal/mlat PosSource names).
const MLAT_SOURCE_LABELS = { none: 'None (not located)', gnss: 'Receiver GNSS', confirmed: 'Confirmed by you', survey: 'Network survey (automatic)', declared: 'Declared position' };

let P = null; // page state while mounted

const TABS = [
  { id: 'connection', label: 'Connection', icon: 'link' },
  { id: 'reception', label: 'Reception', icon: 'radar' },
  { id: 'timing', label: 'Timing & MLAT', icon: 'timer' },
  { id: 'location', label: 'Location', icon: 'pin' },
  { id: 'privacy', label: 'Privacy & data', icon: 'shield' },
];
const STATS_REFRESH_MS = 10000;

function render(route) {
  const { ctx } = P;
  P.scopeAbort?.abort();
  P.scopeAbort = new AbortController();
  for (const t of P.timers.splice(0)) clearInterval(t);
  const sub = route.args[0] || 'stations';
  const sid = sub === 'stations' ? route.args[1] : null;
  P.lastSid = sid || null;
  P.tabsEl = null;
  ctx.ui.clear(P.root);
  if (sub === 'shares') renderMyShares(P.root, { ...ctx, signal: P.scopeAbort.signal });
  else if (sid) renderStation(sid, route.args[2]);
  else renderList();
}

const every = (ms, fn) => { const t = setInterval(() => { if (!document.hidden) fn(); }, ms); P.timers.push(t); };

// -----------------------------------------------------------------------------------------------
// list
// -----------------------------------------------------------------------------------------------
function renderList() {
  const { ctx } = P;
  const { ui, units, api } = ctx;
  const { h } = ui;
  const body = h('div');
  const noticeHost = h('div');
  P.root.append(h('div.page-inner',
    pageHead('My stations', ctx.net.username ? `Stations of ${ctx.net.username}` : '',
      ui.button({ label: 'Claim a feeder', icon: 'claim', variant: 'primary', href: '#/me/claim' }),
      ui.button({ label: 'Share links', icon: 'share', href: '#/me/shares' }),
      ui.button({ label: 'Feed Check', icon: 'signal', href: '#/me/check' })),
    noticeHost, body));
  const signal = P.scopeAbort.signal;
  // Owner notification (DESIGN §19.3): recent location notices of the own stations (7 days).
  api.get(EP.myNotices(), { signal }).then((r) => {
    const week = Date.now() - 7 * 86400e3;
    const list = (r?.notices || []).filter((n) => n.t >= week && (n.level === 'warn' || n.level === 'ok')).slice(0, 5);
    if (!list.length || signal.aborted) return;
    ui.mount(noticeHost, h('div.net-note.net-notice-banner', ui.icon('bell'), h('div.col.gap-1',
      h('b', 'Location news for your stations'),
      list.map((n) => h('span', h('a', { href: `#/me/stations/${encodeURIComponent(n.sid)}/location` }, n.name || shortSid(n.sid)), ': ', n.text, h('span.muted', ` (${units.ago(n.t)})`))))));
  }).catch(() => { /* notices are optional */ });
  const reload = loadInto(body, () => api.get(EP.myStations(), { signal }), (r) => {
    const list = (r?.stations || []).map(normStation).filter(Boolean);
    if (!list.length) {
      return ui.empty({ icon: 'antenna', title: 'No stations yet', text: 'Claim your feeder with the code from airview-feeder, or with its UUID for readsb / mlat-client.',
        action: ui.button({ label: 'Claim a feeder', icon: 'claim', variant: 'primary', href: '#/me/claim' }) });
    }
    return h('div.grid.grid-auto.net-cards', list.map((s) => {
      const open = () => ctx.navigate('me/stations/' + encodeURIComponent(s.sid));
      return ui.card({
        title: s.name || shortSid(s.sid),
        sub: [KIND_LABELS[s.kind] || s.kind, clockLabel(s.clockClass)].filter(Boolean).join(' · '),
        icon: 'antenna',
        actions: [onlineBadge(s.online, s.lastSeen), admissionBadge(s.admission && s.admission !== 'approved' ? s.admission : '')].filter(Boolean),
        body: h('div.col.gap-3',
          ui.kv([
            ['Last data', s.online ? 'now' : s.lastSeen ? units.ago(s.lastSeen) : 'never'],
            ['Location', LOC_LABELS[s.locStatus] || s.locStatus || units.DASH],
            ['Public pin', (PIN_OPTIONS.find((o) => o.value === (s.pin || 'hidden')) || PIN_OPTIONS[0]).label],
            ['Statistics', s.publicStats ? 'public' : 'private'],
          ], { columns: 2 }),
          h('div.row.gap-2', { style: 'flex-wrap:wrap' },
            ui.button({ label: 'Open', icon: 'chevron-right', size: 'sm', variant: 'primary', onClick: open }),
            ui.button({ label: 'Station map', icon: 'map', size: 'sm', disabled: !Number.isFinite(s.lat), onClick: () => { ctx.net.setStation(s.sid); ctx.net.setView('station'); ctx.navigate('map'); } }))),
      });
    }));
  });
  P.reload = () => reload(true);
}

// -----------------------------------------------------------------------------------------------
// one station
// -----------------------------------------------------------------------------------------------
function renderStation(sid, tabId) {
  const { ctx } = P;
  const { ui, api } = ctx;
  const { h } = ui;
  const head = pageHead('Station', '', ui.button({ label: 'All stations', icon: 'chevron-left', variant: 'ghost', href: '#/me/stations' }));
  const host = h('div');
  P.root.append(h('div.page-inner', head, host));
  const signal = P.scopeAbort.signal;
  const reload = loadInto(host, () => api.get(EP.myStation(sid), { signal }), (raw) => {
    const s = normStationDetail(raw);
    P.station = s;
    head.querySelector('h2').textContent = s.name || shortSid(s.sid);
    head.setSub([KIND_LABELS[s.kind] || s.kind, s.online ? 'online' : s.lastSeen ? `offline since ${ctx.units.ago(s.lastSeen)}` : 'offline', `ID ${s.sid}`, s.feederId ? `feeder ${s.feederId}` : null].filter(Boolean).join(' · '));
    ctx.net.updateStation(raw?.station || raw);
    const active = P.tabsEl?.active || (TABS.some((t) => t.id === tabId) ? tabId : 'connection');
    P.tabsEl = ui.tabs({
      active,
      onChange: (id) => history.replaceState(null, '', `#/me/stations/${encodeURIComponent(sid)}/${id}`),
      tabs: TABS.map((t) => ({ ...t, render: (panel) => panel.append(SECTIONS[t.id](s)) })),
    });
    return h('div.net-station', s.admission && s.admission !== 'approved' ? h('div.net-note', ui.icon('info'), h('span', admissionText(s.admission))) : null, P.tabsEl);
  }, { lines: 8 });
  P.reload = () => reload(true);
}

function admissionText(a) {
  return {
    pending: 'This station waits for the network administrator. Its data stays in its own statistics and is not used for the public map or MLAT until it is approved.',
    denied: 'This station was not admitted to the network.',
    revoked: 'The network administrator revoked this station.',
  }[a] || `Admission: ${a}`;
}

const kvNum = (x, d, unit) => v(x, (n) => P.ctx.units.num(n, d) + (unit ? ' ' + unit : ''));

const SECTIONS = {
  connection(s) {
    const { ui, units, api } = P.ctx;
    const { h } = ui;
    const c = s.connection || {};
    const links = c.links || [];
    const logHost = h('div');
    const timelineHost = h('div', ui.skeleton({ lines: 1 }));
    const signal = P.scopeAbort.signal;
    loadInto(logHost, () => api.get(EP.myConnections(s.sid), { params: { days: 7, limit: 200 }, signal }), (r) => {
      const rows = r?.connections || [];
      ui.mount(timelineHost, timeline(uptimeSegments(rows, Date.now(), 7, s.created)));
      return rows.length ? ui.table({
        columns: [
          { key: 'on', label: 'Connected', render: (x) => units.dateTime(x.on) },
          { key: 'off', label: 'Disconnected', render: (x) => (x.off ? units.dateTime(x.off) : ui.badge('open', 'green')) },
          { key: 'dur', label: 'Duration', align: 'right', render: (x) => units.duration((x.off || Date.now()) - x.on) },
          { key: 'proto', label: 'Protocol', render: (x) => PROTO_LABELS[x.proto] || x.proto || units.DASH },
          { key: 'bytes', label: 'Data', align: 'right', render: (x) => v(x.bytes, units.bytes) },
          { key: 'ipHash', label: 'Address', tip: 'Addresses are kept only as a daily-salted hash', render: (x) => (x.ipHash ? h('code.muted', String(x.ipHash).slice(0, 8)) : units.DASH) },
        ], rows }) : ui.empty({ icon: 'history', title: 'No connections in the last 7 days', compact: true });
    }, { lines: 3 });
    return h('div.grid.cols-2.net-grid',
      ui.card({ title: 'Link', icon: 'link', body: ui.kv([
        ['Protocol', PROTO_LABELS[c.proto] || c.proto || KIND_LABELS[s.kind] || units.DASH],
        ['Software', v(c.version)],
        ['Connected since', c.since ? `${units.dateTime(c.since)} (${units.ago(c.since)})` : units.DASH],
        ['Round trip', kvNum(c.rttP50Ms, 0, 'ms')],
        ['Data rate', kvNum(c.kBps, 1, 'kB/s')],
        ['Late frames', v(c.lateFrames, units.int)],
        ['Clock', clockLabel(s.clockClass)],
      ]) }),
      ui.card({ title: 'Live links', icon: 'antenna', body: links.length
        ? ui.table({ columns: [
          { key: 'proto', label: 'Protocol', render: (l) => PROTO_LABELS[l.proto] || l.proto },
          { key: 'since', label: 'Since', render: (l) => (l.since ? units.ago(l.since) : units.DASH) },
          { key: 'rate', label: 'msg/s', align: 'right', num: true, render: (l) => kvNum(l.msgRate, 0) },
          { key: 'sync', label: 'MLAT', render: (l) => (l.mlatSynced === true ? 'synced' : l.mlatSynced === false ? 'not synced' : units.DASH) },
        ], rows: links })
        : ui.empty({ icon: 'antenna', title: 'Not connected right now', compact: true }) }),
      ui.card({ title: 'Uptime (7 days)', icon: 'history', class: 'span-2', body: timelineHost }),
      ui.card({ title: 'Connection log', icon: 'list', class: 'span-2', body: logHost }));
  },

  reception(s) {
    const { ui, units, api, net } = P.ctx;
    const { h } = ui;
    const tiles = {
      rate: ui.stat({ label: 'Messages', icon: 'signal', accent: 'cyan', unit: '/s' }),
      ac: ui.stat({ label: 'Aircraft this minute', icon: 'plane', accent: 'blue' }),
      pos: ui.stat({ label: 'Positions', icon: 'pin', accent: 'teal' }),
      late: ui.stat({ label: 'Late frames', icon: 'history', accent: 'amber', tip: 'Frames that arrived after their group was closed (backfill after an outage)' }),
    };
    const draw = (st, lane) => {
      tiles.rate.set(v(st?.msgRate, (x) => units.num(x, x < 100 ? 1 : 0)), '/s');
      tiles.ac.set(v(st?.aircraft ?? lane?.aircraft, units.int));
      tiles.pos.set(v(st?.positions ?? lane?.positions, units.si));
      tiles.late.set(v(lane?.lateFrames, units.int));
      tiles.rate.setSub(typeof st?.uptimePct === 'number' ? `${units.num(st.uptimePct, 1)} % uptime` : '');
    };
    draw(s.stats, s.lane);
    every(STATS_REFRESH_MS, async () => {
      try {
        const r = await api.get(EP.myStationStats(s.sid), { signal: P.scopeAbort.signal, timeout: 8000 });
        draw(r?.stats, r?.lane);
      } catch { /* keep the last values */ }
    });
    const go = (page, view) => () => { net.setStation(s.sid); if (view) net.setView(view); P.ctx.navigate(page); };
    return h('div.col.gap-4',
      h('div.grid.cols-4', Object.values(tiles)),
      ui.card({ title: 'On the map', icon: 'map', body: h('div.col.gap-3',
        h('p', 'The Station view centres the map on this station with range rings and its antenna beams; My stations highlights the aircraft your stations hear.'),
        h('div.row.gap-2', { style: 'flex-wrap:wrap' },
          ui.button({ label: 'Station map', icon: 'map', variant: 'primary', disabled: !Number.isFinite(s.lat), onClick: go('map', 'station') }),
          ui.button({ label: 'Heard by my stations', icon: 'antenna', onClick: go('map', 'mine') }))) }),
      net.scope === 'admin' && s.kind === 'host' ? ui.card({ title: 'Host station pages', icon: 'radar', sub: 'The single-station pages show the host station', body: h('div.net-linkgrid',
        ui.button({ label: 'Statistics', icon: 'chart', href: '#/stats' }),
        ui.button({ label: 'Antenna Lab', icon: 'antenna', href: '#/antenna' }),
        ui.button({ label: 'RF Lab', icon: 'wave', href: '#/rf' }),
        ui.button({ label: 'Timing', icon: 'timer', href: '#/timing' }),
        ui.button({ label: 'Aircraft', icon: 'plane', href: '#/aircraft' }),
        ui.button({ label: 'Reports & Replay', icon: 'replay', href: '#/reports' })) }) : null);
  },

  timing(s) {
    // clock quality, MLAT participation, history and sync peers (mlat/owner.js; peers as weekly tags only)
    return timingSection(P.ctx, s, { admin: P.ctx.net.scope === 'admin', every, signal: P.scopeAbort.signal });
  },

  location(s) {
    const { ui, units, api } = P.ctx;
    const { h } = ui;
    const L = s.location || {};
    const est = Array.isArray(L.estimates) ? L.estimates : [];
    const eff = L.effective;
    const admin = P.ctx.net.scope === 'admin';
    const pos = (p) => (p && Number.isFinite(p.lat) ? `${units.latlon(p.lat, p.lon)}${Number.isFinite(p.alt ?? p.hae) ? ' · ' + units.height(p.alt ?? p.hae) : ''}` : units.DASH);
    const pin = publicPin({ pin: s.publicPoint ? { ...s.publicPoint, precision: s.publicPoint.precision || s.pin } : null });
    const update = (body, okText) => api.post(EP.myStationUpdate(s.sid), body).then((r) => { ui.toast.success(okText); if (r?.sid) P.ctx.net.updateStation(r); P.reload?.(); });

    const lat = ui.input({ type: 'number', step: 'any', value: L.declared?.lat ?? '', placeholder: 'Latitude' });
    const lon = ui.input({ type: 'number', step: 'any', value: L.declared?.lon ?? '', placeholder: 'Longitude' });
    const alt = ui.input({ type: 'number', step: 'any', value: L.declared?.alt ?? '', placeholder: 'm', suffix: 'm' });
    const altRef = ui.segmented({ value: L.declared?.altRef || 'msl', options: [{ value: 'msl', label: 'above sea level' }, { value: 'hae', label: 'above ellipsoid' }] });
    const fDecl = ui.field({ label: 'Antenna position', control: h('div.net-latlon', lat, lon, alt), hint: 'The antenna, not the house: decimal degrees (WGS-84) and the height of the antenna. The network checks a declared position against what it measures.' });
    const saveDecl = ui.button({ label: 'Save declared position', icon: 'save', variant: 'primary', onClick: async () => {
      fDecl.setError(null);
      if (lat.value === null || lon.value === null || !(Math.abs(lat.value) <= 90) || !(Math.abs(lon.value) <= 180)) { fDecl.setError('Enter a latitude (-90..90) and a longitude (-180..180).'); return; }
      saveDecl.setLoading(true);
      try { await update({ declared: { lat: lat.value, lon: lon.value, alt: alt.value ?? 0, altRef: altRef.value } }, 'Position saved'); } catch (e) { fDecl.setError(e.message); } finally { saveDecl.setLoading(false); }
    } });
    const clearDecl = L.declared ? ui.button({ label: 'Remove', icon: 'x', variant: 'ghost', onClick: async () => {
      if (!(await ui.confirm({ title: 'Remove the declared position?', message: 'The station then relies on its receiver GNSS or the network survey.', confirmLabel: 'Remove' }))) return;
      try { await update({ declared: null }, 'Declared position removed'); } catch (e) { ui.toast.error(e.message); }
    } }) : null;

    // A position the feeder announced and the network holds (a large move waits for the survey or the
    // owner; small ones are applied at most once a day).
    const pend = s.pendingPosition;
    const pendBlock = pend ? h('div.net-note', ui.icon('info'), h('div.col.gap-2',
      h('span', `Your feeder announced a new position (${pos(pend)}). `
        + (pend.reason === 'held' ? 'It is far from the current one, so it is applied when the network survey confirms it, or when you confirm it here.' : 'Announced changes are applied at most once a day.')),
      ui.button({ label: 'Use the announced position', size: 'sm', onClick: async () => {
        try { const r = await api.post(EP.myPendingConfirm(s.sid), {}); ui.toast.success('Announced position in use'); if (r?.sid) P.ctx.net.updateStation(r); P.reload?.(); } catch (e) { ui.toast.error(e.message); }
      } }))) : null;
    // The MLAT position source (DESIGN §19.3); the survey card below holds the owner override.
    const mp = s.mlatPosition;
    const timing = s.timing && typeof s.timing === 'object' ? s.timing : null;
    const timingBlock = timing && timing.state !== 'trusted' ? h('div.net-note', ui.icon('timer'), h('span', timing.state === 'excluded'
      ? 'The network operator excluded this station\'s timing from MLAT.'
      : `New station: its timing is compared with other stations before it joins MLAT${timing.progress ? ` (${timing.progress.good} good windows, ${timing.progress.refs} reference stations${timing.progress.reason ? ': ' + timing.progress.reason : ''})` : ''}. Its data is used for the map meanwhile.`)) : null;
    const badge = locBadge(L.status);

    // Phase-3 cards: survey, declared versus measured, approximate location, notices (net/locationview.js).
    const unlocated = !eff || L.status === 'estimating' || L.status === 'unknown' || !L.status;
    const p3 = h('div.grid.cols-2.net-grid.span-2');
    const signal = P.scopeAbort.signal;
    const loadP3 = async () => {
      const [sv, loc, nt] = await Promise.allSettled([
        api.get(admin ? EP.adminSurvey(s.sid) : EP.mySurvey(s.sid), { signal }),
        unlocated ? api.get(EP.myLocate(s.sid), { signal, timeout: 30000 }) : Promise.resolve(null),
        api.get(EP.myStationNotices(s.sid), { signal }),
      ]);
      if (signal.aborted) return;
      const status = sv.status === 'fulfilled' ? sv.value : null;
      const locate = loc.status === 'fulfilled' ? loc.value : null;
      const survey = surveyPoint(status, mp);
      ui.mount(p3,
        surveyCard({ ctx: P.ctx, s, status: status || { enabled: false }, estimates: est, mp, admin, reload: () => P.reload?.() }),
        comparisonCard({ declared: L.declared, survey, gnss: L.gnss, locate }),
        unlocated ? locateCard(locate, loc.status === 'rejected' ? loc.reason : null) : null,
        noticesCard(nt.status === 'fulfilled' ? nt.value?.notices : []));
    };
    ui.mount(p3, ui.skeleton({ lines: 4, title: true }));
    loadP3();

    return h('div.grid.cols-2.net-grid',
      ui.card({ title: 'Position in use', icon: 'pin', actions: [ui.badge(badge.label, badge.kind)], body: h('div.col.gap-3',
        ui.kv([
          ['Status', LOC_LABELS[L.status] || L.status || units.DASH],
          ['Position', pos(eff)],
          ['Source', eff?.source || units.DASH],
          ['Uncertainty (1σ)', v(eff?.sigmaM, (x) => units.height(x))],
          mp ? ['Used for MLAT', MLAT_SOURCE_LABELS[mp.source] || mp.source] : null,
        ]),
        L.status === 'suspect' ? h('div.net-warn', ui.icon('alert'), h('span', 'The declared position disagrees with what the network measures. Check it: until it is fixed this station is not used for MLAT geometry.')) : null,
        surveyNotice(eff, L.declared),
        pendBlock, timingBlock,
        L.status === 'estimated' ? h('div.net-note', ui.icon('info'), h('span', 'This position was measured by the network. Precise measurements (σ ≤ 50 m) are used for MLAT automatically; confirm it, reject it, or declare another position below.')) : null,
        L.status === 'estimating' ? h('div.net-note', ui.icon('radar'), h('span', 'The network is measuring this station\'s position now. This takes a few minutes of traffic shared with located stations.')) : null,
        h('p.muted', 'Only you and the network operator see this position.')) }),
      ui.card({ title: 'What the public map shows', icon: 'eye', body: h('div.col.gap-2',
        pin ? ui.kv([['Pin', (PIN_OPTIONS.find((o) => o.value === pin.precision) || PIN_OPTIONS[1]).label], ['Shown at', units.latlon(pin.lat, pin.lon)]]) : h('p', 'Nothing: the pin is hidden.'),
        h('p.muted', pin ? 'A fixed point somewhere in the cell around your station - never the station itself. It is computed once and changes only when the station moves to another cell.' : 'Change it under Privacy & data.'),
        pin ? ui.button({ label: 'Show on the network map', icon: 'map', size: 'sm', onClick: () => { P.ctx.net.setView('network'); P.ctx.navigate(`map?lat=${pin.lat.toFixed(3)}&lon=${pin.lon.toFixed(3)}&zoom=8`); } }) : null) }),
      p3,
      ui.card({ title: 'Measured and reported positions', icon: 'target', class: 'span-2 net-measured', body: h('div.col.gap-3',
        ui.kv([['Declared', pos(L.declared)], ['Receiver GNSS', pos(L.gnss)]]),
        est.length ? ui.table({
          columns: [
            { key: 'method', label: 'Method', render: (e) => ({ survey: 'Network survey (reverse MLAT)', bundle: 'Network adjustment (all stations jointly)', locateme: 'Locate Me', gnss: 'GNSS' }[e.method] || e.method) },
            { key: 't', label: 'When', render: (e) => (e.t ? units.ago(e.t) : units.DASH) },
            { key: 'pos', label: 'Position', render: (e) => pos(e) },
            { key: 'sigma', label: '1σ', render: (e) => v(e.sigmaM, (x) => '±' + units.height(x)) },
            { key: 'status', label: 'Result', render: (e) => e.status || units.DASH },
            { key: 'use', label: '', render: (e) => (e.method !== 'locateme' && e.status !== 'suspect' && Number.isFinite(e.lat) ? ui.button({ label: 'Use this location', size: 'sm',
              onClick: () => confirmSurveyFlow(P.ctx, s, e, () => P.reload?.()) }) : '') },
          ], rows: est }) : null,
        est.some((e) => e.method === 'bundle') ? h('p.muted', 'Network adjustment: once an hour the network solves the positions and clocks of many stations together from the aircraft they share. It also checks declared positions; a contradicted declaration is marked suspect.') : null,
        est.length ? null
          : ui.empty({ icon: 'target', title: 'No measurements yet', text: 'A feeder that sends timestamps is surveyed automatically once it shares aircraft with located stations.', compact: true })) }),
      ui.card({ title: 'Declare the position', icon: 'edit', class: 'span-2', sub: 'Overrides what the network measured; the network keeps checking it', body: h('div.col.gap-3', fDecl, altRef, h('div.row.gap-2', saveDecl, clearDecl), consentBlock('C1', { compact: true }), consentBlock('C6', { compact: true })) }));
  },

  privacy(s) {
    const { ui, api } = P.ctx;
    const { h } = ui;
    const name = ui.input({ value: s.name || '', maxlength: 64, placeholder: 'Station name' });
    const fName = ui.field({ label: 'Name', control: name, hint: 'Shown to you; shown publicly only when "Show the name" is on.' });
    const hintOf = (val) => (PIN_OPTIONS.find((o) => o.value === val) || PIN_OPTIONS[0]).hint;
    const pinHint = h('div.field-hint', hintOf(s.pin || 'hidden'));
    const pin = ui.segmented({ block: true, value: s.pin || 'hidden', options: PIN_OPTIONS.map((o) => ({ value: o.value, label: o.label, tip: o.hint })), onChange: (val) => ui.setText(pinHint, hintOf(val)) });
    const stats = ui.toggle({ label: 'Publish statistics (message rate, uptime, aircraft, positions)', checked: !!s.publicStats });
    const showName = ui.toggle({ label: 'Show the name on the public pages', checked: !!s.showName });
    const c8 = h('div', consentBlock('C8', { compact: true }));
    joinInfo(api).then((j) => { if (Number.isFinite(j.backupDays) && c8.isConnected !== false) ui.mount(c8, consentBlock('C8', { compact: true, backupDays: j.backupDays })); });
    const save = ui.button({ label: 'Save', icon: 'save', variant: 'primary', onClick: async () => {
      fName.setError(null);
      save.setLoading(true);
      try {
        const body = { pin: pin.value, publicStats: stats.checked, showName: showName.checked };
        if ((name.value || '').trim() !== (s.name || '')) body.name = name.value.trim();
        const r = await api.post(EP.myStationUpdate(s.sid), body);
        ui.toast.success('Privacy settings saved');
        if (r && r.sid) P.ctx.net.updateStation(r);
        P.reload?.();
      } catch (e) { fName.setError(e.message); } finally { save.setLoading(false); }
    } });
    const exportBtn = ui.button({ label: 'Export my data', icon: 'download', onClick: () => { location.href = EP.myExport(); } });
    const unclaim = ui.button({ label: 'Unclaim this station', icon: 'x', variant: 'danger', onClick: async () => {
      if (!(await ui.confirm({ title: 'Unclaim this station?', message: 'It stays on the network but no longer belongs to your account. You can claim it again later.', confirmLabel: 'Unclaim', danger: true }))) return;
      try {
        await api.post(EP.myUnclaim(s.sid), {});
        ui.toast.success('Station unclaimed');
        await P.ctx.net.loadStations();
        P.ctx.navigate('me/stations');
      } catch (e) { ui.toast.error(e.message); }
    } });
    return h('div.grid.cols-2.net-grid',
      ui.card({ title: 'Public appearance', icon: 'eye', body: h('div.col.gap-3',
        fName,
        consentBlock('C2', { compact: true }),
        ui.field({ label: 'Map pin', control: h('div.col.gap-1', pin, pinHint) }),
        consentBlock('C4', { compact: true }),
        stats,
        consentBlock('C3', { compact: true }),
        showName, h('div.row', save)) }),
      ui.card({ title: 'Your data', icon: 'database', body: h('div.col.gap-3',
        c8,
        h('p', 'Download everything stored about your account and stations (JSON in a zip file), or give the station up.'),
        h('div.row.gap-2', { style: 'flex-wrap:wrap' }, exportBtn, unclaim),
        h('p.muted', 'To delete your account and all data of your stations, go to ', h('a', { href: '#/me/account' }, 'Account'), '.')) }),
      ui.card({ title: 'Share links', icon: 'share', class: 'span-2', body: h('div.col.gap-3',
        h('p', 'Let someone without an account follow what this station receives live: a read-only link you can revoke at any time. It never shows the station\'s position, but the aircraft it hears outline roughly where it is.'),
        h('div.row.gap-2', { style: 'flex-wrap:wrap' },
          ui.button({ label: 'Share this station…', icon: 'share', variant: 'primary', onClick: () => openCreateShare(P.ctx, { sids: [s.sid] }) }),
          ui.button({ label: 'All my links', icon: 'list', href: '#/me/shares' }))) }));
  },
};

/**
 * Consent text C6, notice form: shown when the position in use is a network survey (reverse MLAT) that was
 * used automatically (DESIGN §19.3). {Δ} is the distance to the declared position; without one the second
 * sentence is left out.
 */
function surveyNotice(eff, declared) {
  const { ui } = P.ctx;
  const { h } = ui;
  const text = surveyNoticeText(eff, declared, C6_NOTICE, geo.distanceKm);
  if (!text) return null;
  const review = h('a', { href: '#', onclick: (e) => { e.preventDefault(); P.root.querySelector('.net-measured')?.scrollIntoView({ behavior: 'smooth', block: 'start' }); } }, C6_NOTICE_LINK);
  return h('div.net-note', { 'data-consent': 'C6-notice' }, ui.icon('info'), h('span', text, ' ', review));
}

/** Uptime bar from [{from, to, online}] (Unix ms). */
function timeline(segs) {
  const { ui, units } = P.ctx;
  const { h } = ui;
  if (!Array.isArray(segs) || !segs.length) return ui.empty({ icon: 'history', title: 'No history yet', compact: true });
  const start = Math.min(...segs.map((x) => x.from));
  const end = Math.max(...segs.map((x) => x.to));
  const bar = h('div.net-timeline');
  let up = 0;
  for (const x of segs) {
    if (x.to <= x.from) continue;
    if (x.online) up += x.to - x.from;
    bar.append(h('i' + (x.online ? '.on' : ''), { style: { left: ((x.from - start) / (end - start)) * 100 + '%', width: ((x.to - x.from) / (end - start)) * 100 + '%' }, tip: `${x.online ? 'online' : 'offline'} ${units.dateTime(x.from)} – ${units.dateTime(x.to)}` }));
  }
  return h('div.col.gap-2', bar, h('div.row.muted', h('span', units.date(start)), h('span.grow'), h('span', `${units.num((up / (end - start)) * 100, 1)} % connected`), h('span.grow'), h('span', units.date(end))));
}

export default {
  id: 'me',
  title: 'My stations',

  mount(container, ctx) {
    P = { ctx, root: container, scopeAbort: null, reload: null, station: null, timers: [], tabsEl: null, lastSid: null };
    ctx.onCleanup(() => { if (P) { P.scopeAbort?.abort(); for (const t of P.timers) clearInterval(t); } });
    render(ctx.route);
  },

  onRoute(route) {
    if (!P) return;
    // a tab switch inside one station only rewrites the URL (replaceState); a deep link selects the tab
    const sid = route.args[0] === 'stations' ? route.args[1] || null : null;
    if (sid && sid === P.lastSid) {
      if (route.args[2] && P.tabsEl && TABS.some((t) => t.id === route.args[2])) P.tabsEl.select(route.args[2]);
      return;
    }
    render(route);
  },

  onNet(reason) {
    if (!P) return;
    if (reason === 'session') render(P.ctx.route);
  },

  unmount() { P = null; },
};
