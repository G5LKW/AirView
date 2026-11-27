// net/locadmin.js - the admin's "Locations" tab (pages/admin.js; DESIGN §10, §11 admin console, §19.3;
// WP-W3-wire): how well the network's stations are located (location quality), the survey counters and every
// station's survey with start / cancel. GET /api/admin/mlat/surveys carries no coordinates; the station's own
// Location tab (#/me/stations/<sid>/location) shows the exact positions.

import { ui } from '../ui.js';
import { units } from '../units.js';
import { EP } from './endpoints.js';
import { failState } from './view.js';
import { KIND_LABELS, LOC_LABELS, clockLabel, shortSid } from './model.js';
import * as L from './location.js';
import { pagedTable } from './pager.js';

const { h } = ui;

const SOURCE_LABELS = { none: 'none', gnss: 'GNSS', confirmed: 'confirmed', survey: 'survey (auto)', declared: 'declared' };

/** Render the tab into panel. act(fn, okText) runs an admin write (re-login on 401, toasts). */
export function renderLocationsAdmin(panel, ctx, act) {
  const { api } = ctx;
  const host = h('div');
  const count = h('span.muted');
  panel.append(h('div.col.gap-3', h('div.row.gap-2', h('span.grow'), count,
    ui.button({ icon: 'refresh', size: 'sm', variant: 'ghost', tip: 'Reload', onClick: () => load() })), host));
  let seq = 0;
  let table = null;
  let bundle = null;
  async function load() {
    const my = ++seq;
    if (!host.firstChild) ui.mount(host, ui.skeleton({ lines: 8 }));
    try {
      const [r, b] = await Promise.all([api.get(EP.adminSurveys(), { signal: ctx.signal }), api.get(EP.adminSurveyStats(), { signal: ctx.signal }).catch(() => null)]);
      if (my !== seq) return;
      bundle = b?.bundle || null;
      const n = r?.stations?.length || 0;
      ui.setText(count, `${units.int(n)} ${n === 1 ? 'station' : 'stations'} · updated ${units.clock(r?.at || Date.now())}`);
      ui.mount(host, view(r));
    } catch (e) {
      if (my !== seq || e.aborted) return;
      ui.mount(host, failState(e, { retry: load }));
    }
  }

  function view(r) {
    const q = r?.quality || {};
    const st = r?.stats || {};
    const tiles = h('div.grid.cols-4',
      ui.stat({ label: 'Stations', icon: 'antenna', value: units.int(q.stations || 0), sub: `${units.int(q.unlocated || 0)} without a position` }),
      ui.stat({ label: 'Located by the survey', icon: 'radar', accent: 'cyan', value: units.int(q.autoUsed || 0), sub: r?.autoUse ? `auto-use on (σ ≤ ${r.autoUseMaxSigmaM || 50} m)` : 'auto-use off' }),
      ui.stat({ label: 'Declared checks', icon: 'check', accent: q.suspect ? 'amber' : 'green', value: `${units.int(q.agreeing || 0)} / ${units.int(q.checked || 0)}`, sub: q.suspect ? `${units.int(q.suspect)} suspect` : 'all agree' }),
      ui.stat({ label: 'Median discrepancy', icon: 'ruler', value: Number.isFinite(q.medianDistM) ? L.fmtDistance(q.medianDistM) : '—', sub: Number.isFinite(q.maxDistM) ? `max ${L.fmtDistance(q.maxDistM)}` : 'no declared checks yet' }));
    const maxBin = Math.max(1, ...(q.sigmaBins || [0]));
    const hist = h('div.net-hist', (q.sigmaBins || [0, 0, 0, 0, 0]).flatMap((n, i) => [
      h('span', L.SIGMA_BINS[i]), h('span', h('div.bar', { style: { width: `${(n / maxBin) * 100}%` } })), h('span.num', units.int(n)),
    ]));
    const byStatus = Object.entries(q.byStatus || {}).sort((a, b) => b[1] - a[1]).map(([k, n]) => `${LOC_LABELS[k] || k} ${n}`).join(' · ');
    const bySource = Object.entries(q.bySource || {}).sort((a, b) => b[1] - a[1]).map(([k, n]) => `${SOURCE_LABELS[k] || k} ${n}`).join(' · ');
    const quality = ui.card({ title: 'Location quality', icon: 'target', body: h('div.col.gap-3',
      ui.kv([
        ['Location status', byStatus || '—'],
        ['MLAT position source', bySource || '—'],
        ['Stable surveys', `${units.int(q.surveyedStable || 0)} (σ of the last stable survey below)`],
        ['Owner rejections', units.int(q.rejected || 0)],
        ['Without timestamps', `${units.int(q.noTimestamps || 0)} (never surveyed)`],
      ]),
      hist) });
    const counters = ui.card({ title: 'Survey engine', icon: 'radar', sub: r?.enabled ? 'aggregator.mlat.survey' : 'not running (aggregator.mlat.enabled / survey.disabled)', body: r?.enabled ? ui.kv([
      ['Running / queued', `${units.int(st.Running || 0)} / ${units.int(st.Queued || 0)}`],
      ['Finished', `${units.int(st.Done || 0)} stable, ${units.int(st.Unstable || 0)} unstable, ${units.int(st.NoData || 0)} no data, ${units.int(st.Cancelled || 0)} cancelled`],
      ['Groups seen / matched', `${units.si(st.Groups || 0)} / ${units.si(st.Matched || 0)}`],
      ['Observations accepted', units.si(st.Accepted || 0)],
      ['Rejected', `few peers ${units.si(st.FewPeers || 0)}, no reference ${units.si(st.NoRef || 0)}, inconsistent ${units.si(st.Inconsistent || 0)}, below the mask ${units.si(st.Masked || 0)}`],
    ]) : ui.empty({ icon: 'radar', title: 'The survey does not run', compact: true }) });
    const rows = r?.stations || [];
    const columns = [
        { key: 'name', label: 'Station', sort: (x) => (x.name || '').toLowerCase(), render: (x) => h('span.col', h('a', { href: `#/me/stations/${encodeURIComponent(x.sid)}/location` }, x.name || shortSid(x.sid)), h('code.muted', `${KIND_LABELS[x.kind] || x.kind} · ${clockLabel(x.clockClass)}`)) },
        { key: 'locStatus', label: 'Location', sort: true, render: (x) => { const b = L.locBadge(x.locStatus); return ui.badge(b.label, b.kind); } },
        { key: 'positionSource', label: 'MLAT uses', sort: true, render: (x) => SOURCE_LABELS[x.positionSource] || x.positionSource },
        { key: 'survey', label: 'Survey', sort: (x) => (x.running ? 2 : x.last ? 1 : 0), render: (x) => (x.running
          ? h('span', ui.badge('running', 'violet'), ` ${units.int(x.observed)} obs · ${L.reasonText(x.reason)}`)
          : x.last ? h('span', L.lastResultText(x.last), x.last.finished ? h('span.muted', ` ${units.ago(x.last.finished)}`) : '') : h('span.muted', x.timestamps ? 'not yet' : 'no timestamps')) },
        { key: 'sigma', label: 'σ', align: 'right', num: true, sort: (x) => x.last?.sigmaHM ?? Infinity, render: (x) => (Number.isFinite(x.last?.sigmaHM) ? L.fmtDistance(x.last.sigmaHM) : '—') },
        { key: 'decl', label: 'Declared off by', align: 'right', sort: (x) => x.last?.declaredDistM ?? -1, render: (x) => (x.last?.declaredChecked ? (x.last.declaredSuspect ? ui.badge(L.fmtDistance(x.last.declaredDistM), 'amber') : L.fmtDistance(x.last.declaredDistM)) : '—') },
        { key: 'next', label: 'Next check', render: (x) => (x.nextCheck ? units.dateTime(x.nextCheck) : '—') },
        { key: 'act', label: '', render: (x) => h('span.row.gap-1',
          x.running
            ? ui.button({ label: 'Cancel', size: 'sm', variant: 'ghost', onClick: async () => { if (await act(() => api.del(EP.adminSurvey(x.sid)), 'Survey cancelled')) load(); } })
            : ui.button({ label: 'Survey', size: 'sm', disabled: !x.timestamps || !r?.enabled, tip: x.timestamps ? 'Start a survey now (no rate limit for the admin)' : 'The station sends no timestamps', onClick: async () => { if (await act(() => api.post(EP.adminSurvey(x.sid), {}), 'Survey started')) load(); } })) },
    ];
    // One page in the DOM (1,000+ stations); search, sort and page survive the reload.
    if (!table) table = pagedTable({ columns, rows, sort: { key: 'survey', dir: 'desc' }, pageSize: 50, empty: 'No approved stations',
      search: (x) => `${x.name || ''} ${x.sid} ${x.kind || ''}`, placeholder: 'Name or ID' });
    else table.setRows(rows);
    return h('div.col.gap-4', tiles, h('div.grid.cols-2.net-grid', quality, counters), bundleCard(bundle, !!r?.enabled), ui.card({ title: 'Stations', icon: 'list', body: table }));
  }
  // The hourly network bundle adjustment (DESIGN §10.3 phase 4; GET /api/admin/mlat/survey "bundle"): joint
  // positions and clocks of up to 64 stations per run. Admin only: the result holds exact positions.
  function bundleCard(b, surveyOn) {
    const sub = 'Hourly joint solution of station positions and clocks from shared aircraft (aggregator.mlat.survey.bundle)';
    if (!b) {
      return ui.card({ title: 'Network bundle adjustment', icon: 'layers', sub, body: ui.empty({ icon: 'layers', compact: true,
        title: surveyOn ? 'The bundle adjustment is off' : 'The survey does not run',
        text: surveyOn ? 'aggregator.mlat.survey.bundle.disabled is set in the operator configuration.' : 'It runs beside the survey (aggregator.mlat.enabled, survey not disabled).' }) });
    }
    const s = b.stats || {};
    const last = b.last || null;
    const kv = ui.kv([
      ['Members / subjects', `${units.int(s.Members || 0)} / ${units.int(s.Subjects || 0)}`],
      ['Frames collected', `${units.si(s.Frames || 0)} (${units.si(s.Recs || 0)} receptions)`],
      ['Runs / failures', `${units.int(s.Runs || 0)} / ${units.int(s.Failures || 0)}`],
      ['Last run', s.LastRun && !String(s.LastRun).startsWith('0001') ? `${units.dateTime(Date.parse(s.LastRun))} · ${units.int(s.LastStations || 0)} stations, ${units.int(s.LastReleased || 0)} declarations released, ${units.int(s.LastOutliers || 0)} outliers` : 'not yet (first run after collecting for an hour)'],
      s.LastError ? ['Last error', s.LastError] : null,
    ], { columns: 2 });
    if (!last) return ui.card({ title: 'Network bundle adjustment', icon: 'layers', sub, body: kv });
    const rows = (last.stations || []).map((x) => ({ ...x, flag: x.outlier ? 2 : x.priorReleased ? 1 : 0 }));
    const why = (x) => [x.priorReleased ? 'declared position contradicted' : '', ...(x.outlierWhy || [])].filter(Boolean).join(', ');
    const table = pagedTable({ rows, pageSize: 25, search: (x) => `${x.name || ''} ${x.sid || ''} ${x.station}`, placeholder: 'Name or ID',
      sort: { key: 'flag', dir: 'desc' }, empty: 'No station in the last adjustment',
      rowClass: (x) => (x.flag ? 'mlat-row-warn' : ''),
      columns: [
        { key: 'name', label: 'Station', sort: (x) => (x.name || x.sid || '').toLowerCase(), render: (x) => (x.sid ? h('span.col', h('a', { href: `#/me/stations/${encodeURIComponent(x.sid)}/location` }, x.name || shortSid(x.sid)), h('code.muted', x.sid)) : `#${x.station}`) },
        { key: 'prior', label: 'Prior', sort: true, render: (x) => x.prior || '—' },
        { key: 'determined', label: 'Result', sort: (x) => (x.determined ? 1 : 0), render: (x) => (!x.used ? h('span.muted', x.why || 'not used') : x.determined ? ui.badge('determined', 'green') : ui.badge('anchor / held', 'other')) },
        { key: 'sigmaHM', label: 'σ h / v', align: 'right', num: true, sort: true, render: (x) => (Number.isFinite(x.sigmaHM) ? `${L.fmtDistance(x.sigmaHM)} / ${Number.isFinite(x.sigmaVM) ? L.fmtDistance(x.sigmaVM) : '—'}` : '—') },
        { key: 'shiftHM', label: 'Moved', align: 'right', num: true, sort: true, tip: 'Horizontal distance from the prior position', render: (x) => (Number.isFinite(x.shiftHM) ? L.fmtDistance(x.shiftHM) : '—') },
        { key: 'priorDistHM', label: 'Declared off by', align: 'right', num: true, sort: true, render: (x) => (x.priorTested && Number.isFinite(x.priorDistHM) ? (x.priorReleased ? ui.badge(L.fmtDistance(x.priorDistHM), 'amber') : L.fmtDistance(x.priorDistHM)) : '—') },
        { key: 'jitterNs', label: 'Jitter', align: 'right', num: true, sort: true, render: (x) => (Number.isFinite(x.jitterNs) ? units.num(x.jitterNs, 0) + ' ns' : '—') },
        { key: 'receptions', label: 'Rx / rejected', align: 'right', num: true, sort: true, render: (x) => `${units.si(x.receptions || 0)} / ${units.si(x.rejected || 0)}` },
        { key: 'flag', label: 'Flags', sort: true, render: (x) => (x.flag ? ui.badge(why(x) || 'outlier', 'amber') : '') },
      ] });
    return ui.card({ title: 'Network bundle adjustment', icon: 'layers', class: 'span-2', sub, body: h('div.col.gap-3', kv,
      h('div.muted.small', `Last result: ${units.si(last.framesUsed || 0)} of ${units.si(last.frames || 0)} frames, variance factor ${Number.isFinite(last.varianceFactor) ? units.num(last.varianceFactor, 2) : '—'}, ${units.num(last.durationS || 0, 0)} s of data, computed in ${units.num((last.elapsedMs || 0) / 1000, 1)} s. Determined positions of unlocated stations become "bundle" estimates on the station's Location tab; contradicted declarations are marked suspect.`),
      table) });
  }

  load();
}
