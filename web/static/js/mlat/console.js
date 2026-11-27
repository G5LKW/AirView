// mlat/console.js - the network administrator's MLAT console (tab "MLAT" of pages/admin.js; DESIGN §11, §9.6):
//   tiles       engine state, synced stations, hold-out accuracy (track P50 / P95, last hour), solve and
//               publish rates, clock epoch cost, the week-on-week P95 alarm
//   history     hold-out P50 / P95 over time (fix and track), stage rates per minute, clock filter
//               (GET /api/admin/mlat/history: one server sample per minute, kept 24 h in memory)
//   quality     σ calibration (within 1σ / 2σ, NEES), grades, rejections, ADS-B verification, k_atm
//   stations    clock quality and residuals per station (GET /api/admin/mlat/stations); a row opens the
//               station's full Timing & MLAT view (peers by SID)
//   sync        the clock-sync matrix as a heat map (RMS residual, co-observations or relative frequency),
//               stations grouped by connected component (GET /api/admin/mlat/sync)
//   CRLB        expected accuracy of the geometry: area per grade at 3000 ft / FL100 / FL300 (admin only)
//   phase 4-5   (mlat/phase45.js) ADS-B verification alerts (suspect / spoofed positions), where one more
//               station would help most (CRLB gain, also a map layer), Mode A/C targets
//
//   renderMLATConsole(panel, ctx)   // mounts into a tab panel; refreshes every 30 s while visible

import { EP } from '../net/endpoints.js';
import { clockLabel } from '../net/model.js';
import { v } from '../net/view.js';
import { GRADES, crlbRows, last, series, seriesMinN, syncMatrix } from './model.js';
import { timingSection } from './owner.js';
import { pagedTable } from '../net/pager.js';
import { verificationCard, improveCard, modeACCard } from './phase45.js';

const REFRESH_MS = 30000;
const MIN_N = 5; // hold-out points with fewer samples are not drawn
const REJECT_LABELS = { sigma: 'σ too large', altitude: 'altitude mismatch', horizon: 'beyond radio horizon', speed: 'implausible speed', hosttime: 'arrival-time mismatch', implausible: 'implausible', notconverged: 'not converged', raim: 'integrity (RAIM)' };

export function renderMLATConsole(panel, ctx) {
  const { ui, units, api, charts } = ctx;
  const { h } = ui;
  const timers = [];
  const alive = () => panel.isConnected;
  const kvNum = (x, d, unit) => v(x, (n) => units.num(n, d) + (unit ? ' ' + unit : ''));
  const metres = (x) => v(x, (n) => (n >= 1000 ? units.num(n / 1000, 2) + ' km' : units.num(n, 0) + ' m'));

  const state = { minutes: 360, metric: 'rmsNs', problems: false, stats: null, rows: [] };

  // ---- head ----
  const range = ui.segmented({ value: state.minutes, options: [{ value: 60, label: '1 h' }, { value: 360, label: '6 h' }, { value: 1440, label: '24 h' }, { value: 7 * 1440, label: '7 d', tip: 'Hourly points from the stored minute tables' }, { value: 30 * 1440, label: '30 d' }], onChange: (x) => { state.minutes = x; loadHistory(); } });
  const updated = h('span.muted');
  const head = h('div.row.gap-2.net-filters', range, h('span.grow'), updated, ui.button({ icon: 'refresh', size: 'sm', variant: 'ghost', tip: 'Reload', onClick: () => loadAll() }));

  // ---- tiles ----
  const T = {
    engine: ui.stat({ label: 'Engine', icon: 'bolt', accent: 'cyan' }),
    synced: ui.stat({ label: 'Synced stations', icon: 'timer', accent: 'violet' }),
    acc: ui.stat({ label: 'Hold-out track P50 / P95', icon: 'target', accent: 'green', tip: 'Error of MLAT tracks of aircraft whose ADS-B position is held back, last hour' }),
    solves: ui.stat({ label: 'Solves / min', icon: 'chart', accent: 'amber' }),
    pub: ui.stat({ label: 'Map updates / min', icon: 'map', accent: 'teal' }),
    epoch: ui.stat({ label: 'Clock epoch', icon: 'timer', accent: 'cyan', tip: 'Mean wall time of recent clock-filter epochs (budget 80 ms at 200 stations)' }),
  };
  const tiles = h('div.grid.cols-3.mlat-tiles', Object.values(T));
  const alarm = h('div');

  // ---- charts ----
  const accEl = h('div.chart', { style: '--h:240px' });
  const rateEl = h('div.chart', { style: '--h:220px' });
  const clockEl = h('div.chart', { style: '--h:200px' });
  const histNote = h('span');
  const accCard = ui.card({ title: 'Hold-out accuracy', icon: 'target', sub: histNote, body: accEl, class: 'span-2',
    actions: ui.button({ label: 'Error map', icon: 'map', size: 'sm', variant: 'ghost', tip: 'Live Map, Admin view: measured error per 1° cell', onClick: () => openQualityMap('measured') }) });
  const rateCard = ui.card({ title: 'Solve rates', icon: 'chart', sub: 'Per minute', body: rateEl });
  const clockCard = ui.card({ title: 'Clock filter', icon: 'timer', sub: 'Stations, synced stations and epoch cost', body: clockEl });
  let accChart = null; let rateChart = null; let clockChart = null;

  // ---- quality ----
  const qualHost = h('div');
  const qualCard = ui.card({ title: 'Self-validation', icon: 'check-circle', sub: 'Hold-out aircraft, last hour', body: qualHost, class: 'span-2' });

  // ---- stations ----
  const stHost = h('div');
  const stProblems = ui.toggle({ label: 'Problems only', checked: false, onChange: (x) => { state.problems = x; drawStations(); } });
  const stCard = ui.card({ title: 'Stations', icon: 'antenna', sub: 'Clock quality and residuals · click a row for its details', class: 'span-2', body: stHost });
  let stTable = null;
  // ---- sync matrix ----
  const syncEl = h('div.chart', { style: '--h:420px' });
  const syncNote = h('span');
  const syncMetric = ui.segmented({ value: state.metric, options: [{ value: 'rmsNs', label: 'RMS' }, { value: 'count', label: 'Frames' }, { value: 'relPpb', label: 'Freq.' }], onChange: (x) => { state.metric = x; loadSync(); } });
  const syncCard = ui.card({ title: 'Sync matrix', icon: 'link', sub: syncNote, actions: syncMetric, body: syncEl, class: 'span-2' });
  let syncChart = null;

  // ---- CRLB ----
  const crlbHost = h('div');
  const crlbCard = ui.card({ title: 'Expected accuracy (CRLB)', icon: 'radar', sub: 'Cramér-Rao bound of the station geometry and timing; admin only', class: 'span-2',
    actions: [ui.button({ label: 'On the map', icon: 'map', size: 'sm', variant: 'ghost', tip: 'Live Map, Admin view, MLAT quality layer', onClick: () => openQualityMap('expected') }),
      ui.button({ label: 'Recompute', icon: 'refresh', size: 'sm', variant: 'ghost', onClick: () => loadCRLB(true) })], body: crlbHost });
  // the Live Map's MLAT quality layer (map/network.js) in the Admin view: CRLB or the measured hold-out cells
  function openQualityMap(mode) {
    ui.prefs('airview.map.net', {}).set({ mlatq: mode, mlatqBand: -1 });
    ctx.net.setView('admin');
    ctx.navigate('map');
  }

  // Phase 4-5 cards (mlat/phase45.js): ADS-B verification alerts, where to add a station, Mode A/C targets.
  const verify = verificationCard(ctx);
  const improve = improveCard(ctx, () => openQualityMap('improve'));
  const modeac = modeACCard(ctx);
  panel.append(h('div.col.gap-3.mlat-console', head, alarm, verify.banner, tiles,
    h('div.grid.cols-2.net-grid', accCard, rateCard, clockCard, qualCard, verify.card, stCard, syncCard, crlbCard, improve.card, modeac.card)));

  const off = () => {
    ui.mount(panel, ui.empty({ icon: 'target', title: 'The MLAT engine does not run', text: 'Enable aggregator.mlat.enabled in the configuration (it needs a restart) to compute MLAT positions on this network.' }));
  };

  // ---- loaders ----
  async function get(path, params) { return api.get(path, { params, timeout: 20000 }); }

  async function loadStats() {
    const [s, q] = await Promise.all([get(EP.adminMLAT()), get(EP.adminMLATQuality(), { window: 60 })]);
    if (s && s.running === false) { off(); return false; }
    state.stats = s;
    const tr = q?.track || {};
    T.engine.set(units.int(s.workers), 'workers');
    T.engine.setSub(`${units.num(s.procUsPerGroup || 0, 1)} µs / group · ${units.si(s.dropped || 0)} dropped`);
    // Headline: the track error estimated from the cross-track errors (free of
    // the ADS-B reference's airframe latency); the vs-reference figures below it.
    const te = q?.trackEst || {};
    const est = tr.n >= 20 && te.p50M > 0;
    T.acc.set(tr.n >= 20 ? `${units.num(est ? te.p50M : tr.p50M, 0)} / ${units.num(est ? te.p95M : tr.p95M, 0)}` : '—', tr.n >= 20 ? 'm' : '');
    const accSub = est ? `estimated · vs ADS-B ${units.num(tr.p50M, 0)} / ${units.num(tr.p95M, 0)} m` : `cross-track P95 ${units.num(tr.crossP95M || 0, 0)} m`;
    T.acc.setSub(tr.n ? `${units.int(tr.n)} samples · ${accSub}` : 'no hold-out samples yet');
    T.epoch.set(units.num((s.clock?.MeanEpochNs || s.clock?.LastEpochNs || 0) / 1e6, 1), 'ms');
    T.epoch.setSub(`${units.int(s.clock?.Components || 0)} components · ${units.si(s.clock?.Steps || 0)} steps`);
    ui.mount(alarm, q?.alarm ? h('div.net-warn', ui.icon('alert'), h('span', `Hold-out P95 regressed week on week: ${units.num(q.alarmThisP95M, 0)} m against ${units.num(q.alarmLastP95M, 0)} m (more than 25 %).`)) : null);
    renderQuality(s, q);
    return true;
  }

  function renderQuality(s, q) {
    const f = q?.fix || {};
    const t = q?.track || {};
    const pct = (x) => (typeof x === 'number' && Number.isFinite(x) ? units.num(x * 100, 1) + ' %' : '—');
    const grades = s.fixGrades || {};
    const gTotal = Object.values(grades).reduce((a, b) => a + b, 0);
    const rejects = Object.entries(s.rejects || {}).sort((a, b) => b[1] - a[1]);
    const calib = ui.table({ columns: [
      { key: 'k', label: '' },
      { key: 'n', label: 'Samples', align: 'right', num: true },
      { key: 'p50', label: 'P50', align: 'right', num: true },
      { key: 'p95', label: 'P95', align: 'right', num: true },
      { key: 'x95', label: 'Cross-track P95', align: 'right', num: true },
      { key: 'est', label: 'Estimated P50 / P95', align: 'right', num: true, tip: 'Horizontal error estimated from the cross-track errors alone (free of the ADS-B reference latency)' },
      { key: 'w1', label: 'Within 1σ', align: 'right', num: true, tip: 'Ideal 63 % for a consistent σ' },
      { key: 'w2', label: 'Within 2σ', align: 'right', num: true, tip: 'Ideal 98 %' },
      { key: 'nees', label: 'NEES 95', align: 'right', num: true, tip: 'Share of normalised errors below the 95 % χ² bound (ideal 95 %)' },
    ], rows: [['Fixes', f, q?.fixEst], ['Tracks', t, q?.trackEst]].map(([k, x, e]) => ({ k, n: units.int(x.n || 0),
      est: x.n && e && e.p50M > 0 ? `${units.num(e.p50M, 0)} / ${units.num(e.p95M, 0)} m` : '—', p50: metres(x.n ? x.p50M : undefined), p95: metres(x.n ? x.p95M : undefined), x95: metres(x.n ? x.crossP95M : undefined), w1: pct(x.n ? x.within1Sigma : undefined), w2: pct(x.n ? x.within2Sigma : undefined), nees: pct(x.n ? x.nees95 : undefined) })) });
    const gradeBar = h('div.mlat-gradebar', gTotal ? GRADES.concat([{ id: 'D', color: '#64748b', text: 'track maintenance only' }]).map((g) => {
      const n = grades[g.id] || 0;
      return n ? h('span', { style: { flex: String(n), background: g.color }, tip: `Grade ${g.id}: ${units.int(n)} fixes (${units.num((100 * n) / gTotal, 1)} %) · ${g.text}` }, n / gTotal > 0.06 ? g.id : '') : null;
    }) : h('span.muted', 'no fixes yet'));
    ui.mount(qualHost, h('div.col.gap-3', calib, h('div.grid.cols-2.gap-3',
      h('div.col.gap-2', h('div.mlat-label', 'Fix grades'), gradeBar, ui.kv([
        ['Solves', `${units.si(s.solves || 0)} (${units.si(s.solveOk || 0)} ok, ${units.si(s.solveErr || 0)} failed)`],
        ['Track restarts / gate solves', `${units.si(s.restarts || 0)} / ${units.si(s.gateSolves || 0)}`],
        ['Rejected', units.si(s.rejected || 0)],
        ...rejects.slice(0, 5).map(([k, n]) => ['  ' + (REJECT_LABELS[k] || k), units.si(n)]),
      ])),
      ui.kv([
        ['Hold-out fraction', units.pct((q?.fraction || 0) * 100)],
        ['ADS-B positions confirmed', units.si(s.adsbConfirmed || 0)],
        ['ADS-B contradicted / suspects', `${units.si(s.adsbContradicted || 0)} / ${units.si(s.adsbSuspects || 0)}`],
        ['Results sent / refused', `${units.si(s.resultsSent || 0)} / ${units.si(s.resultsRefused || 0)}`],
        ['k_atm (refractivity scale)', s.kAtm ? `${units.num(s.kAtm.model, 3)}${s.kAtm.hours ? ` · estimate ${units.num(s.kAtm.estimate, 3)} ± ${units.num(s.kAtm.sigmaB, 3)}` : ''}${s.kAtm.applied ? '' : ' (advisory)'}` : '—'],
        ['Aircraft blacklisted for sync', units.int(s.clock?.BlacklistedAc || 0)],
      ]))));
  }

  async function loadHistory() {
    if (state.minutes > 1440) return loadLongterm(Math.round(state.minutes / 1440));
    const r = await get(EP.adminMLATHistory(), { minutes: state.minutes });
    if (!r || r.running === false) return;
    const n = r.t?.length || 0;
    ui.setText(histNote, n ? `P50 and P95 of the error over the preceding ${r.windowMin} min, one point per minute · null below ${MIN_N} samples` : 'The server records one sample per minute: the charts fill in after two minutes.');
    T.solves.set(v(last(r, 'solvesPerMin'), (x) => units.num(x, 0)));
    T.solves.setSub(last(r, 'solveOkPerMin') !== undefined ? `${units.num(last(r, 'solveOkPerMin'), 0)} accepted · ${units.num(last(r, 'rejectedPerMin') || 0, 0)} rejected` : '');
    T.pub.set(v(last(r, 'publishedPerMin'), (x) => units.num(x, 0)));
    T.pub.setSub(last(r, 'resultsPerMin') !== undefined ? `${units.num(last(r, 'resultsPerMin'), 0)} results to feeders` : '');
    const syncedNow = last(r, 'synced');
    const stationsNow = last(r, 'stations');
    T.synced.set(syncedNow !== undefined ? `${units.int(syncedNow)} / ${units.int(stationsNow || 0)}` : '—');
    T.synced.setSub(last(r, 'components') !== undefined ? `${units.int(last(r, 'components'))} clock components` : '');
    const m = (x) => (x === null || x === undefined ? '—' : x >= 1000 ? units.num(x / 1000, 2) + ' km' : units.num(x, 0) + ' m');
    const acc = charts.line({
      series: [
        { name: 'Track P50', data: seriesMinN(r, 'trackP50M', 'trackN', MIN_N), color: charts.colors.green },
        { name: 'Track P95', data: seriesMinN(r, 'trackP95M', 'trackN', MIN_N), color: charts.colors.amber },
        { name: 'Fix P50', data: seriesMinN(r, 'fixP50M', 'fixN', MIN_N), color: charts.colors.cyan, dashed: true, width: 1.2 },
        { name: 'Fix P95', data: seriesMinN(r, 'fixP95M', 'fixN', MIN_N), color: charts.colors.red, dashed: true, width: 1.2 },
      ],
      legend: true, yName: 'm', yScale: true, valueFormat: m,
    });
    const rates = charts.line({
      series: [
        { name: 'Solves', data: series(r, 'solvesPerMin'), color: charts.colors.blue },
        { name: 'Accepted', data: series(r, 'solveOkPerMin'), color: charts.colors.green },
        { name: 'Rejected', data: series(r, 'rejectedPerMin'), color: charts.colors.red },
        { name: 'Map updates', data: series(r, 'publishedPerMin'), color: charts.colors.amber },
        { name: 'Results', data: series(r, 'resultsPerMin'), color: charts.colors.violet, dashed: true },
      ],
      legend: true, yName: '/ min', valueFormat: (x) => (x === null || x === undefined ? '—' : units.num(x, 0)),
    });
    const clk = charts.line({
      series: [
        { name: 'Stations', data: series(r, 'stations'), color: charts.colors.grey, step: 'end' },
        { name: 'Synced', data: series(r, 'synced'), color: charts.colors.violet, step: 'end' },
        { name: 'Epoch (ms)', data: series(r, 'epochMs'), color: charts.colors.cyan, y2: true, width: 1.2 },
      ],
      legend: true, yName: 'stations', y2Name: 'ms', valueFormat: (x) => (x === null || x === undefined ? '—' : units.num(x, 1)),
    });
    if (!accChart) accChart = charts.create(accEl, acc); else accChart.set(acc, { replace: true });
    if (!rateChart) rateChart = charts.create(rateEl, rates); else rateChart.set(rates, { replace: true });
    if (!clockChart) clockChart = charts.create(clockEl, clk); else clockChart.set(clk, { replace: true });
  }

  // days to weeks: one point per hour from the persisted tables (GET /api/admin/mlat/longterm)
  async function loadLongterm(days) {
    const r = await get(EP.adminMLATLongterm(), { days });
    if (!r || r.running === false) return;
    const n = r.t?.length || 0;
    ui.setText(histNote, n ? `Hold-out error per hour (summed histograms of every cell, ±12 % bins), ${days} days` : 'No stored history yet: the server writes one row per minute while the MLAT engine runs.');
    const m = (x) => (x === null || x === undefined ? '—' : x >= 1000 ? units.num(x / 1000, 2) + ' km' : units.num(x, 0) + ' m');
    const acc = charts.line({
      series: [
        { name: 'Hold-out P50', data: seriesMinN(r, 'holdP50M', 'holdN', MIN_N), color: charts.colors.green },
        { name: 'Hold-out P95', data: seriesMinN(r, 'holdP95M', 'holdN', MIN_N), color: charts.colors.amber },
      ], legend: true, yName: 'm', yScale: true, valueFormat: m,
    });
    const rates = charts.line({
      series: [
        { name: 'Groups', data: series(r, 'groupsPerMin'), color: charts.colors.blue },
        { name: 'Map updates', data: series(r, 'fixesPerMin'), color: charts.colors.amber },
        { name: 'Rejected', data: series(r, 'outliersPerMin'), color: charts.colors.red },
      ], legend: true, yName: '/ min', valueFormat: (x) => (x === null || x === undefined ? '—' : units.num(x, 0)),
    });
    const clk = charts.line({
      series: [{ name: 'Synced stations (max / h)', data: series(r, 'peers'), color: charts.colors.violet, step: 'end' }],
      legend: true, yName: 'stations', valueFormat: (x) => (x === null || x === undefined ? '—' : units.num(x, 0)),
    });
    if (!accChart) accChart = charts.create(accEl, acc); else accChart.set(acc, { replace: true });
    if (!rateChart) rateChart = charts.create(rateEl, rates); else rateChart.set(rates, { replace: true });
    if (!clockChart) clockChart = charts.create(clockEl, clk); else clockChart.set(clk, { replace: true });
  }

  const problem = (r) => (r.hasClock && !r.synced) || r.biasFlag || (r.outlierPct || 0) > 10 || (r.clockOutlierPct || 0) > 10 || (r.steps || 0) > 0 || (r.wScale || 0) > 2;
  // Search, sort and paging over every station (1,000+ stations: one page in the DOM, net/pager.js).
  function drawStations() {
    const rows = state.problems ? state.rows.filter(problem) : state.rows;
    if (!stTable) {
      stTable = pagedTable({
        rows, sort: { key: 'contribPerHour', dir: 'desc' }, pageSize: 50, filters: [stProblems],
        search: (r) => `${r.name || ''} ${r.sid}`, placeholder: 'Name or ID',
        empty: 'No station has timed data yet',
        rowClass: (r) => (problem(r) ? 'mlat-row-warn' : ''),
        onRowClick: (r) => openStation(r),
        columns: [
          { key: 'name', label: 'Station', sort: (r) => (r.name || r.sid).toLowerCase(), render: (r) => h('span.col', h('span', r.name || '—'), h('code.muted', r.sid)) },
          { key: 'clockClass', label: 'Clock', sort: true, render: (r) => (r.hasClock ? clockLabel(r.clockClass) : '—') },
          { key: 'anchorState', label: 'Anchor', sort: true, render: (r) => r.anchorState || '—' },
          { key: 'synced', label: 'Sync', sort: (r) => (r.synced ? 1 : 0), render: (r) => (r.hasClock ? ui.badge(r.synced ? 'synced' : 'no', r.synced ? 'green' : 'amber') : '—') },
          { key: 'sigmaThetaNs', label: 'σθ', align: 'right', num: true, sort: true, tip: 'Clock offset uncertainty (ns)', render: (r) => (r.hasClock ? units.num(r.sigmaThetaNs, 0) : '—') },
          { key: 'driftPpm', label: 'Drift ppm', align: 'right', num: true, sort: true, render: (r) => (r.hasClock ? units.num(r.driftPpm, 3) : '—') },
          { key: 'jitterNs', label: 'Jitter', align: 'right', num: true, sort: true, tip: 'ns', render: (r) => (r.hasClock ? units.num(r.jitterNs, 0) : '—') },
          { key: 'syncedPeers', label: 'Peers', align: 'right', num: true, sort: true, tip: 'Synced peers / peers with co-observations', render: (r) => `${r.syncedPeers}/${r.peers}` },
          { key: 'steps', label: 'Steps', align: 'right', num: true, sort: true },
          { key: 'contribPerHour', label: 'Fixes/h', align: 'right', num: true, sort: true, sortDir: 'desc', render: (r) => (r.hasMlat ? units.int(r.contribPerHour) : '—') },
          { key: 'excludedPct', label: 'Excl. %', align: 'right', num: true, sort: true, render: (r) => (r.hasMlat ? units.num(r.excludedPct, 1) : '—') },
          { key: 'residualBiasNs', label: 'Bias ns', align: 'right', num: true, sort: (r) => Math.abs(r.residualBiasNs || 0), render: (r) => (r.hasMlat ? h('span', { class: r.biasFlag ? 'mlat-bad' : '' }, units.num(r.residualBiasNs, 0)) : '—') },
          { key: 'wScale', label: 'σ scale', align: 'right', num: true, sort: true, render: (r) => (r.hasMlat && r.wScale ? units.num(r.wScale, 2) : '—') },
        ],
      });
      ui.mount(stHost, stTable);
      return;
    }
    stTable.setRows(rows);
  }

  async function loadStations() {
    const r = await get(EP.adminMLATStations());
    if (!r || r.running === false) return;
    state.rows = r.stations || [];
    drawStations();
  }

  function openStation(r) {
    const local = [];
    const d = ui.drawer({ title: r.name || r.sid, icon: 'timer', width: 760, onClose: () => { for (const t of local) clearInterval(t); } });
    const every = (ms, fn) => local.push(setInterval(() => { if (!document.hidden) fn(); }, ms));
    d.body.append(timingSection(ctx, { sid: r.sid }, { admin: true, every }));
  }

  async function loadSync() {
    const r = await get(EP.adminMLATSync());
    if (!r || r.running === false) return;
    const mx = syncMatrix(r, { max: 60, metric: state.metric });
    const unit = state.metric === 'count' ? 'frames (30 s)' : state.metric === 'relPpb' ? 'ppb' : 'ns';
    ui.setText(syncNote, mx.ids.length
      ? `${mx.ids.length} stations in ${mx.components} component${mx.components === 1 ? '' : 's'}${mx.dropped ? ` · ${mx.dropped} with the fewest co-observations not shown` : ''} · row = station, column = reference peer, value in ${unit}`
      : 'No co-observations yet: the matrix fills when synced stations hear the same ADS-B aircraft.');
    syncEl.style.setProperty('--h', Math.max(220, Math.min(900, mx.ids.length * 14 + 110)) + 'px');
    const opt = charts.heatmap({ x: mx.labels, y: mx.labels, data: mx.cells.map(([x, y, val]) => [y, x, val]), min: 0,
      max: state.metric === 'rmsNs' ? 1000 : undefined, ramp: state.metric === 'count' ? charts.ramps.blue : charts.ramps.heat,
      valueFormat: (x) => units.num(x, state.metric === 'relPpb' ? 1 : 0) + ' ' + (state.metric === 'count' ? '' : unit) });
    opt.xAxis.axisLabel = { rotate: 60, fontSize: 9, interval: 0 };
    opt.yAxis.axisLabel = { fontSize: 9, interval: 0 };
    if (!syncChart) syncChart = charts.create(syncEl, opt); else syncChart.set(opt, { replace: true });
  }

  async function loadCRLB(refresh) {
    ui.mount(crlbHost, ui.loading(refresh ? 'Computing…' : 'Loading…'));
    try {
      const r = await get(EP.adminMLATCRLB(), refresh ? { refresh: 1 } : undefined);
      if (!r || r.running === false) return;
      const rows = crlbRows(r);
      ui.mount(crlbHost, h('div.col.gap-2', rows.length ? ui.table({ rows, columns: [
        { key: 'altFt', label: 'Height', render: (x) => (x.altFt >= 10000 ? units.fl(x.altFt) : units.alt(x.altFt)) },
        { key: 'cells', label: 'Grid points', align: 'right', num: true, render: (x) => units.int(x.cells) },
        { key: 'minSigmaM', label: 'Best σ', align: 'right', num: true, render: (x) => metres(x.minSigmaM) },
        { key: 'p50SigmaM', label: 'Median σ', align: 'right', num: true, render: (x) => metres(x.p50SigmaM) },
        { key: 'p90SigmaM', label: 'P90 σ', align: 'right', num: true, render: (x) => metres(x.p90SigmaM) },
        ...GRADES.map((g) => ({ key: 'areaKm2' + g.id, label: h('span', h('i.mlat-dot', { style: { background: g.color } }), ` ${g.id} area`), align: 'right', num: true, tip: `Area where σ ≤ ${g.max} m`, render: (x) => units.area(x['areaKm2' + g.id]) })),
      ] }) : ui.empty({ icon: 'radar', title: 'No located stations with timing', text: 'The bound needs at least 3 located stations with a clock state.', compact: true }),
      h('div.muted.small', `Computed ${r.computed ? units.ago(r.computed) : '—'} on a ${r.stepDeg || 0.25}° grid · cached 10 minutes`)));
    } catch (e) {
      if (!e.aborted) ui.mount(crlbHost, ui.error(e, { retry: () => loadCRLB(false) }));
    }
  }

  let busy = false;
  async function loadAll() {
    if (busy || !alive()) return;
    busy = true;
    try {
      if (!(await loadStats())) return;
      await Promise.all([loadHistory(), loadStations(), loadSync(), verify.load(), modeac.load()]);
      ui.setText(updated, 'updated ' + units.clock(Date.now()));
    } catch (e) {
      if (!e.aborted) ui.toast.error(e.message || 'MLAT console: loading failed');
    } finally {
      busy = false;
    }
  }

  // the tab panel is attached to the page after this function returns (ui.tabs renders the first tab
  // while it builds itself): start on the next task
  setTimeout(() => { loadAll().then(() => { if (alive() && state.stats) { loadCRLB(false); improve.load(false); } }); }, 0);
  timers.push(setInterval(() => { if (!alive()) { for (const t of timers) clearInterval(t); return; } if (!document.hidden && panel.offsetParent !== null) loadAll(); }, REFRESH_MS)); // only while the tab is shown
}
