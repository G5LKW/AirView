// mlat/phase45.js - phase 4-5 cards of the admin MLAT console (mlat/console.js; DESIGN §9.6, §9.9, §15):
//
//   verificationCard(ctx)   ADS-B position verification: counters, the aircraft whose broadcast positions are
//                           suspect now (possible spoofing or a bad transponder position), the last transitions,
//                           and a banner on the console while any aircraft is suspect
//                           (GET /api/admin/mlat/verification)
//   improveCard(ctx, map)   where one more station would improve MLAT most: candidate sites ranked by the CRLB
//                           gain at a chosen height, with the cells that become covered or reach grade A / B;
//                           "On the map" opens the Live Map's MLAT quality layer in mode 'improve'
//                           (GET /api/admin/mlat/crlb/improve)
//   modeACCard(ctx)         Mode A/C multilateration (aircraft without a Mode S address): counters and the shown
//                           targets (GET /api/admin/mlat/modeac)
//
// Each returns { card, load() } (verification also { banner }). Admin only: these routes answer 401/403 for
// everyone else, and nothing here is shown on public or owner pages.

import { ui, h } from '../ui.js';
import { units } from '../units.js';
import { EP } from '../net/endpoints.js';
import { v } from '../net/view.js';
import { pagedTable } from '../net/pager.js';

const HEIGHTS = [{ value: 914.4, label: '3000 ft' }, { value: 3048, label: 'FL100' }, { value: 9144, label: 'FL300' }];

const pos = (lat, lon) => (Number.isFinite(lat) && Number.isFinite(lon) ? units.latlon(lat, lon) : units.DASH);
const showOnMap = (ctx, icao) => { ctx.net?.setView?.('admin'); ctx.navigate('map/' + encodeURIComponent(icao)); };

export function verificationCard(ctx) {
  const { api } = ctx;
  const counters = h('div');
  const host = h('div');
  const evHost = h('div');
  const banner = h('div');
  const card = ui.card({ title: 'ADS-B verification', icon: 'shield', class: 'span-2',
    sub: 'Broadcast positions checked against the arrival times at synced stations; contradicted aircraft are multilaterated for 10 minutes',
    body: h('div.col.gap-3', counters, host, h('div.mlat-label', 'Recent changes'), evHost) });
  let table = null;
  let evTable = null;
  async function load() {
    let r;
    try {
      r = await api.get(EP.adminVerification(), { timeout: 15000 });
    } catch (e) {
      if (!e.aborted) ui.mount(host, ui.error(e, { retry: load }));
      return;
    }
    if (!r || r.running === false) { ui.mount(host, ui.empty({ icon: 'shield', title: 'The MLAT engine does not run', compact: true })); return; }
    const c = r.counters || {};
    ui.mount(counters, ui.kv([
      ['Positions confirmed', units.si(c.confirmed || 0)],
      ['Contradicted', `${units.si(c.contradicted || 0)} (${units.num((c.contradictedShare || 0) * 100, 2)} %)`],
      ['Aircraft made suspect', units.si(c.suspects || 0)],
      ['Charged to a station instead', `${units.si(c.blamed || 0)} declared-position · ${units.si(c.culprit || 0)} single-row`],
      ['Muted (network-wide breaker)', units.si(c.muted || 0)],
      ['Recovered panics', `${units.si(c.clockPanicResets || 0)} clock filter · ${units.si(c.advancePanics || 0)} engine`],
    ], { columns: 2 }));
    const cur = r.current || [];
    ui.mount(banner, cur.length ? h('div.net-warn.mlat-verify-banner', ui.icon('alert'), h('span',
      `${cur.length === 1 ? 'One aircraft broadcasts' : `${units.int(cur.length)} aircraft broadcast`} positions the network's timing contradicts (possible spoofing or a faulty transponder): `,
      ...cur.slice(0, 5).flatMap((a, i) => [i ? ', ' : '', h('a', { href: '#', onclick: (e) => { e.preventDefault(); showOnMap(ctx, a.icao); } }, a.callsign || a.icao)]),
      cur.length > 5 ? ' …' : '')) : null);
    if (!table) {
      table = pagedTable({ rows: cur, pageSize: 25, search: (a) => `${a.icao} ${a.callsign || ''}`, placeholder: 'ICAO or callsign',
        empty: 'No aircraft is suspect: every checked position agrees with the network.', sort: { key: 'since', dir: 'desc' },
        onRowClick: (a) => showOnMap(ctx, a.icao),
        columns: [
          { key: 'icao', label: 'Aircraft', sort: true, render: (a) => h('span.col', h('b', a.callsign || a.icao), h('code.muted', a.icao)) },
          { key: 'since', label: 'Suspect since', sort: true, sortDir: 'desc', render: (a) => units.ago(a.since) },
          { key: 'pos', label: 'Broadcast position', render: (a) => pos(a.lat, a.lon) },
          { key: 'altFt', label: 'Altitude', align: 'right', num: true, sort: true, render: (a) => v(a.altFt, (x) => units.alt(x)) },
          { key: 'nacp', label: 'NACp', align: 'right', num: true, sort: true, render: (a) => v(a.nacp) },
          { key: 'mlat', label: 'Shown from', sort: (a) => (a.mlat ? 1 : 0), render: (a) => (a.mlat ? ui.badge('MLAT', 'mlat') : ui.badge(a.source || 'ADS-B', 'other')) },
        ] });
      ui.mount(host, table);
    } else table.setRows(cur);
    const ev = r.events || [];
    if (!evTable) {
      evTable = pagedTable({ rows: ev, pageSize: 10, empty: 'No changes since the server started.',
        columns: [
          { key: 't', label: 'When', render: (e) => units.dateTime(e.t) },
          { key: 'kind', label: 'Change', render: (e) => (e.kind === 'suspect' ? ui.badge('became suspect', 'amber') : ui.badge(`cleared after ${units.age(e.durationS || 0)}`, 'green')) },
          { key: 'icao', label: 'Aircraft', render: (e) => `${e.callsign ? e.callsign + ' · ' : ''}${e.icao}` },
          { key: 'pos', label: 'Position then', render: (e) => pos(e.lat, e.lon) },
          { key: 'altFt', label: 'Altitude', align: 'right', render: (e) => v(e.altFt, (x) => units.alt(x)) },
        ] });
      ui.mount(evHost, evTable);
    } else evTable.setRows(ev);
  }
  return { card, banner, load };
}

export function improveCard(ctx, openMap) {
  const { api } = ctx;
  const prefs = ui.prefs('airview.mlat.improve', { alt: 3048, sigmaNs: 30 });
  const host = h('div');
  const alt = ui.segmented({ value: prefs.get().alt, options: HEIGHTS, onChange: (x) => { prefs.set({ alt: x }); load(false); } });
  const kind = ui.select({ size: 'sm', value: String(prefs.get().sigmaNs), options: [
    { value: '30', label: 'GPS receiver (30 ns)' }, { value: '83', label: 'Beast / 12 MHz (83 ns)' }, { value: '400', label: 'RTL-SDR (400 ns)' }],
  onChange: (x) => { prefs.set({ sigmaNs: Number(x) }); load(false); } });
  const card = ui.card({ title: 'Where to add a station', icon: 'pin', class: 'span-2',
    sub: 'Sites where one more receiver would improve the expected MLAT accuracy (CRLB) most; admin only',
    actions: [ui.button({ label: 'On the map', icon: 'map', size: 'sm', variant: 'ghost', tip: 'Live Map, Admin view: improvement layer', onClick: () => { ui.prefs('airview.map.net', {}).set({ crlbAlt: prefs.get().alt }); openMap?.(); } }),
      ui.button({ label: 'Recompute', icon: 'refresh', size: 'sm', variant: 'ghost', onClick: () => load(true) })],
    body: h('div.col.gap-3', h('div.row.gap-2.net-filters', alt, kind), host) });
  let seq = 0;
  async function load(refresh) {
    const my = ++seq;
    ui.mount(host, ui.loading(refresh ? 'Computing…' : 'Loading…'));
    try {
      const p = prefs.get();
      const r = await api.get(EP.adminCRLBImprove(), { params: { alt: p.alt, sigmaNs: p.sigmaNs, refresh: refresh ? 1 : undefined }, timeout: 120000 });
      if (my !== seq) return;
      if (!r || r.running === false) { ui.mount(host, ui.empty({ icon: 'pin', title: 'The MLAT engine does not run', compact: true })); return; }
      const sites = r.sites || [];
      const b = r.baseline || {};
      ui.mount(host, h('div.col.gap-2',
        ui.kv([['Stations in the geometry', units.int(r.stations || 0)], ['Covered grid cells now', `${units.int(b.cells || 0)} · median σ ${v(b.p50M, (x) => units.height(x))}`]], { columns: 2 }),
        sites.length ? ui.table({ rows: sites.map((s, i) => ({ ...s, rank: i + 1 })), columns: [
          { key: 'rank', label: '#', align: 'right', num: true },
          { key: 'pos', label: 'Area (cell centre)', render: (s) => pos(s.lat, s.lon) },
          { key: 'score', label: 'Benefit', render: (s) => h('span.mlat-score', h('i', { style: { width: Math.round((s.score || 0) * 100) + '%' } }), h('span', units.num((s.score || 0) * 100, 0) + ' %')) },
          { key: 'newCells', label: 'New coverage', align: 'right', num: true, tip: 'Grid cells without MLAT geometry today', render: (s) => units.int(s.newCells) },
          { key: 'newA', label: '→ A', align: 'right', num: true, tip: 'Cells that reach σ ≤ 50 m', render: (s) => units.int(s.newA) },
          { key: 'newB', label: '→ B', align: 'right', num: true, tip: 'Cells that reach σ ≤ 150 m', render: (s) => units.int(s.newB) },
          { key: 'med', label: 'Median σ nearby', align: 'right', tip: 'Covered cells within 400 km: now → with the station', render: (s) => `${s.medianBeforeM ? units.height(s.medianBeforeM) : '—'} → ${s.medianAfterM ? units.height(s.medianAfterM) : '—'}` },
        ] }) : ui.empty({ icon: 'pin', title: 'No suggestion', text: r.reason || 'No site improves the expected accuracy.', compact: true }),
        h('div.muted.small', `${units.int(r.candidates || 0)} candidate cells on a ${r.stepDeg || 0.5}° grid at ${(HEIGHTS.find((x) => x.value === r.altM) || {}).label || units.height(r.altM)} · computed ${r.computed ? units.ago(r.computed) : '—'} in ${units.num((r.elapsedMs || 0) / 1000, 1)} s · cached 10 minutes. A cell is a ${r.stepDeg || 0.5}° area, not an address: any site in it with a clear horizon will do.`)));
    } catch (e) {
      if (my === seq && !e.aborted) ui.mount(host, ui.error(e, { retry: () => load(false) }));
    }
  }
  return { card, load };
}

export function modeACCard(ctx) {
  const { api } = ctx;
  const host = h('div');
  const card = ui.card({ title: 'Mode A/C targets', icon: 'radar', class: 'span-2',
    sub: 'Aircraft without a Mode S address, located from their Mode A/C replies (aggregator.mlat.modeAc)', body: host });
  let table = null;
  async function load() {
    let r;
    try { r = await api.get(EP.adminModeAC(), { timeout: 15000 }); } catch (e) { if (!e.aborted) ui.mount(host, ui.error(e, { retry: load })); return; }
    if (!r || !r.enabled) {
      table = null;
      ui.mount(host, ui.empty({ icon: 'radar', title: 'Mode A/C MLAT is off', text: 'Switch on aggregator.mlat.modeAc in the operator configuration. Only GPS-anchored stations within 150 km of each other take part.', compact: true }));
      return;
    }
    const st = r.stats || {};
    const rows = r.targets || [];
    const kv = ui.kv([
      ['Replies used / unsynced', `${units.si(st.replies || 0)} / ${units.si(st.unsynced || 0)}`],
      ['Clusters / solves (ok)', `${units.si(st.clusters || 0)} / ${units.si(st.solves || 0)} (${units.si(st.solveOk || 0)})`],
      ['Stations taking part', units.int(st.stations || 0)],
      ['Targets shown', units.int(st.shown ?? rows.length)],
    ], { columns: 2 });
    if (!table) {
      table = pagedTable({ rows, pageSize: 25, search: (t) => `${t.id} ${t.squawk ?? ''}`, placeholder: 'Squawk or id', empty: 'No Mode A/C target right now.',
        onRowClick: (t) => showOnMap(ctx, t.id),
        columns: [
          { key: 'id', label: 'Target', sort: true, render: (t) => h('code', t.id) },
          { key: 'squawk', label: 'Squawk', sort: true, render: (t) => (t.squawk ? h('b', String(t.squawk)) : units.DASH) },
          { key: 'altFt', label: 'Mode C', align: 'right', num: true, sort: true, render: (t) => v(t.altFt, (x) => units.alt(x)) },
          { key: 'grade', label: 'Grade', sort: true, render: (t) => (t.grade ? ui.badge(t.grade, t.grade === 'A' ? 'green' : t.grade === 'B' ? 'cyan' : 'amber') : units.DASH) },
          { key: 'sigmaHM', label: 'σ', align: 'right', num: true, sort: true, render: (t) => v(t.sigmaHM, (x) => units.height(x)) },
          { key: 'fixes', label: 'Fixes', align: 'right', num: true, sort: true },
          { key: 'ageS', label: 'Age', align: 'right', num: true, sort: true, render: (t) => v(t.ageS, (x) => units.num(x, 0) + ' s') },
        ] });
    } else table.setRows(rows);
    ui.mount(host, h('div.col.gap-3', kv, table));
  }
  return { card, load };
}
