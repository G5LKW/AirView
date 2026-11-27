// mlat/owner.js - the "Timing & MLAT" section of one station (pages/me.js), DESIGN §11 and §8.2:
//   own clock quality (class, anchor state, σθ, drift, β short, jitter, outliers, steps, events),
//   MLAT participation (contributions per hour, used / excluded / outliers, residual bias, σ scale, timed
//   frames, result eligibility, results received), the station's last 2 hours, and the sync peers.
// Owners see peers only as a weekly tag unique to their station plus a distance band - never a name, a
// position, a bearing or a station ID (the server sends nothing more; this view adds nothing). The admin
// (scope admin) gets the same view with peers by station ID from /api/admin/stations/{sid}/mlat.
//
//   const el = timingSection(ctx, station, { admin, every, signal });   // refreshes every 15 s via every()

import { EP } from '../net/endpoints.js';
import { clockLabel } from '../net/model.js';
import { v } from '../net/view.js';
import { BANDS, normStationMLAT, series } from './model.js';

const REFRESH_MS = 15000;
const POS_SOURCE = { gnss: 'GNSS (receiver)', confirmed: 'Confirmed by you', survey: 'Network survey', declared: 'Declared', none: 'Not located' };
const ANCHOR = { anchor: 'Anchor (GPS time reference)', probation: 'On probation', free: 'Free-running clock', demoted: 'Demoted', 'self-asserted': 'Anchor (self-asserted)' };
const EVENT = { step: 'Clock step', freq_reset: 'Frequency reset', promoted: 'Promoted to anchor', demoted: 'Demoted', leap: 'Leap / whole-second offset', anchor_lost: 'Anchor lost', validated: 'Anchor validated', epoch: 'Epoch', restart: 'Receiver restart' };

export function timingSection(ctx, station, opts = {}) {
  const { ui, units, api, charts } = ctx;
  const { h } = ui;
  const sid = station.sid;
  const url = opts.admin ? EP.adminStationMLAT(sid) : EP.myStationMLAT(sid);
  const root = h('div.col.gap-3.mlat-owner');
  const kvNum = (x, d, unit) => v(x, (n) => units.num(n, d) + (unit ? ' ' + unit : ''));

  const tiles = {
    sync: ui.stat({ label: 'Clock', icon: 'timer', accent: 'cyan' }),
    peers: ui.stat({ label: 'Synced peers', icon: 'link', accent: 'violet', tip: 'Stations whose clocks this station is synchronised with (no identities)' }),
    contrib: ui.stat({ label: 'MLAT fixes / h', icon: 'target', accent: 'amber', tip: 'Positions this station helped to compute in the last hour' }),
    results: ui.stat({ label: 'Results received', icon: 'download', accent: 'green', tip: 'MLAT positions sent back to this feeder' }),
  };
  const statusRow = h('div.grid.cols-4.mlat-tiles', Object.values(tiles));
  const body = h('div.col.gap-3');
  const chartEl = h('div.chart', { style: '--h:220px' });
  let chart = null;
  // days: one point per hour from the stored minute rows (own station only)
  const ltEl = h('div.chart', { style: '--h:220px' });
  const ltNote = h('span');
  const ltCard = ui.card({ title: 'Last 7 days', icon: 'chart', sub: ltNote, body: ltEl });
  let ltChart = null;
  root.append(statusRow, body, ltCard);

  function render(raw) {
    const m = normStationMLAT(raw);
    const c = m.clock;
    const q = m.mlat;
    const sm = m.summary;
    if (!m.running) {
      for (const t of Object.values(tiles)) t.set(null);
      ui.mount(body, ui.empty({ icon: 'target', title: 'MLAT is off on this network', text: 'The network does not run its own multilateration engine right now, so there are no clock or MLAT figures.' }));
      return;
    }
    tiles.sync.set(c ? (c.synced ? 'synced' : 'not synced') : '—');
    tiles.sync.setSub(c ? clockLabel(c.clockClass) : 'no timed frames yet');
    tiles.peers.set(units.int(sm.syncedPeers));
    tiles.peers.setSub(sm.peers ? `${units.int(sm.peers)} with common aircraft` : 'none yet');
    tiles.contrib.set(v(sm.contribPerHour, units.int));
    tiles.contrib.setSub(sm.timedPerMin !== undefined ? `${units.int(sm.timedPerMin)} timed frames / min` : '');
    tiles.results.set(sm.resultsReceived !== undefined ? units.si(sm.resultsReceived) : sm.resultsEligible ? 'eligible' : '—');
    tiles.results.setSub(sm.resultsEligible ? 'this feeder gets MLAT results' : 'needs ≥ 10 timed frames / min while synced');

    const clockCard = ui.card({ title: 'Clock', icon: 'timer', sub: c?.relativeTime ? 'Relative network time (no GPS anchor in this part of the network)' : undefined, body: c ? ui.kv([
      ['Clock class', clockLabel(c.clockClass) + (c.declaredClass && c.declaredClass !== c.clockClass ? ` (declared ${clockLabel(c.declaredClass)})` : '')],
      ['Synchronised', c.synced ? 'yes' : 'no'],
      ['Anchor', (ANCHOR[c.anchorState] || c.anchorState || '—') + (c.anchorReason ? ` – ${c.anchorReason}` : '')],
      ['σθ (offset uncertainty)', kvNum(c.sigmaThetaNs, 0, 'ns')],
      ['Drift', kvNum(c.driftPpm, 3, 'ppm')],
      ['Short-frame offset β', kvNum(c.betaShortNs, 0, 'ns')],
      ['Jitter', kvNum(c.jitterNs, 0, 'ns')],
      ['Sync outliers', kvNum(c.outlierPct, 1, '%')],
      ['Clock steps', units.int(c.steps)],
    ]) : ui.empty({ icon: 'timer', title: 'No clock state yet', text: 'The network starts tracking this clock once the station sends timestamped frames of aircraft other stations hear too.', compact: true }) });

    const mlatCard = ui.card({ title: 'MLAT contribution', icon: 'target', body: h('div.col.gap-2',
      q?.biasFlag ? h('div.net-warn', ui.icon('alert'), h('span', 'This station\'s arrival times are consistently off. Check its position (Location tab): an antenna position error of 100 m shifts the times by up to 330 ns.')) : null,
      ui.kv([
        ['Position used for MLAT', POS_SOURCE[m.positionSource] || m.positionSource || '—'],
        ['Fixes per hour', v(q?.contribPerHour, units.int)],
        ['Rows used', kvNum(q?.usedPct, 1, '%')],
        ['Excluded (integrity check)', kvNum(q?.excludedPct, 1, '%')],
        ['Outliers', kvNum(q?.outlierPct, 1, '%')],
        ['Residual bias', kvNum(q?.residualBiasNs, 0, 'ns')],
        ['σ scale', q?.wScale !== undefined ? units.num(q.wScale, 2) + (q.wScale > 1.5 ? ' (noisier than assumed)' : q.wScale < 0.67 ? ' (cleaner than assumed)' : '') : '—'],
        ['Learned jitter', q?.learnedJitterNs ? units.num(q.learnedJitterNs, 0) + ' ns' : '—'],
        ['Timed frames / min (synced)', v(sm.timedPerMin, units.int)],
        ['MLAT results to this feeder', sm.resultsEligible ? 'yes' : 'not yet'],
      ])) });

    const hist = m.history;
    const histCard = ui.card({ title: 'Last 2 hours', icon: 'history', class: 'span-2', sub: 'One sample per minute', body: hist && hist.t.length > 1 ? chartEl
      : ui.empty({ icon: 'history', title: 'Collecting', text: 'The server records one sample per minute; the chart appears after two.', compact: true }) });

    const evCard = c && c.events.length ? ui.card({ title: 'Clock events', icon: 'history', body: ui.table({ columns: [
      { key: 't', label: 'Time', render: (e) => units.dateTime(e.t) },
      { key: 'reason', label: 'Event', render: (e) => EVENT[e.reason] || e.reason },
      { key: 'detail', label: 'Detail' },
    ], rows: c.events.slice(0, 30) }) }) : null;

    const peerCols = [
      opts.admin ? { key: 'sid', label: 'Station', render: (p) => h('code', p.sid || '?') } : { key: 'tag', label: 'Peer', tip: 'A tag that changes every week and differs for every station: it cannot be linked to a station', render: (p) => h('code', p.tag || '—') },
      { key: 'band', label: 'Distance', render: (p) => BANDS[p.band] || '—' },
      { key: 'count', label: 'Common frames (30 s)', align: 'right', num: true, sort: true, render: (p) => units.int(p.count) },
      { key: 'rmsNs', label: 'RMS', align: 'right', num: true, sort: true, render: (p) => kvNum(p.rmsNs, 0, 'ns') },
      // relative frequency: admin only (the server leaves it out of owner views)
      ...(opts.admin ? [{ key: 'relPpb', label: 'Relative frequency', align: 'right', num: true, render: (p) => kvNum(p.relPpb, 1, 'ppb') }] : []),
    ];
    const peerCard = ui.card({ title: 'Sync peers', icon: 'link', class: evCard ? '' : 'span-2',
      sub: opts.admin ? 'Admin view: peers by station ID' : 'Shown with a weekly changing tag and a distance band only – never their name or position.',
      body: m.peers.length ? ui.table({ columns: peerCols, rows: m.peers, sort: { key: 'count', dir: 'desc' } })
        : ui.empty({ icon: 'link', title: 'No sync peers yet', text: 'Peers appear when this station and another hear the same ADS-B aircraft and both send timestamps.', compact: true }) });

    ui.mount(body, h('div.grid.cols-2.net-grid', clockCard, mlatCard, histCard, evCard, peerCard));
    if (hist && hist.t.length > 1) {
      const opt = charts.line({
        series: [
          { name: 'σθ (ns)', data: series(hist, 'sigmaThetaNs'), color: charts.colors.cyan },
          { name: 'Residual bias (ns)', data: series(hist, 'residualBiasNs'), color: charts.colors.amber },
          { name: 'Fixes / h', data: series(hist, 'contribPerHour'), color: charts.colors.violet, y2: true, dashed: true },
        ],
        legend: true, yName: 'ns', y2Name: 'fixes / h', valueFormat: (x) => (x === null || x === undefined ? '—' : units.num(x, 1)),
      });
      if (!chart) chart = charts.create(chartEl, opt); else chart.set(opt, { replace: true });
    }
  }

  async function loadLongterm() {
    try {
      const r = await api.get(EP.myStationMLATLongterm(sid), { params: { days: 7 }, signal: opts.signal, timeout: 20000 });
      if (!r || r.running === false || !r.t?.length) {
        ui.setText(ltNote, 'No stored history yet: the server writes one row per minute while the MLAT engine runs.');
        ltEl.classList.add('hidden');
        return;
      }
      ltEl.classList.remove('hidden');
      ui.setText(ltNote, 'One point per hour');
      const opt = charts.line({
        series: [
          { name: 'Fixes / min', data: series(r, 'fixesPerMin'), color: charts.colors.violet },
          { name: 'Outliers / min', data: series(r, 'outliersPerMin'), color: charts.colors.red },
          { name: 'σθ (ns)', data: series(r, 'sigmaNs'), color: charts.colors.cyan, y2: true, dashed: true },
          { name: 'Synced %', data: series(r, 'syncedShare', 100), color: charts.colors.green, y2: true, width: 1.2 },
        ],
        legend: true, yName: '/ min', y2Name: 'ns · %', valueFormat: (x) => (x === null || x === undefined ? '—' : units.num(x, 1)),
      });
      if (!ltChart) ltChart = charts.create(ltEl, opt); else ltChart.set(opt, { replace: true });
    } catch (e) {
      if (!e.aborted) ui.setText(ltNote, e.status === 404 ? 'Not available on this server' : 'Could not be loaded');
    }
  }

  let seq = 0;
  let ticks = 0;
  async function load(first) {
    const my = ++seq;
    try {
      const r = await api.get(url, { signal: opts.signal, timeout: 15000 });
      if (my === seq) render(r);
    } catch (e) {
      if (e.aborted || my !== seq) return;
      if (first) ui.mount(body, ui.error(e, { title: 'Timing figures could not be loaded', retry: () => load(true) }));
    }
  }
  ui.mount(body, ui.skeleton({ lines: 6, title: true }));
  load(true);
  loadLongterm();
  opts.every?.(REFRESH_MS, () => {
    if (!root.isConnected) return;
    load(false);
    if (++ticks % 20 === 0) loadLongterm(); // every 5 minutes
  }); // a re-rendered station page leaves this one detached
  return root;
}
