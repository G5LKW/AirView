// weather/impact.js - bottom half of the Weather page: weather history aligned with the hourly range
// metrics, and the weather-impact analysis (correlations, response curves, regression, ducting episodes).
// Endpoints: /api/weather/hourly, /api/stats/range, /api/weather/impact.

import {
  HOUR, ok, esc, dayTime, fmtDelta, signed, pText, uv, round, caption, panel, source, soft, progress, notesList,
  stackedGrids, yIndex, timeTooltip, bandSeries, colors,
} from './common.js';

const METRICS = {
  p95_km: { label: 'p95 range', kind: 'dist', tip: '95th percentile of the position distances in an hour: the robust “how far do I reach” figure' },
  max_km: { label: 'Max range', kind: 'dist', tip: 'Furthest position of the hour' },
  far_count: { label: 'Far positions', unit: '', tip: 'Positions beyond the far-reception threshold per hour' },
  resid_db: { label: 'Signal residual', unit: 'dB', tip: 'Mean signal level relative to what the distance alone predicts' },
};
const metricLabel = (m) => (METRICS[m] ? METRICS[m].label : m);

/** How to show a weather variable of the analysis in the user's units. */
function varConv(key, units) {
  const id = (v) => v;
  switch (key) {
    case 'temp_c': case 'dewpoint_c': return { conv: (v) => units.value('temp', v), unit: units.label('temp') };
    case 'pressure_msl': return { conv: (v) => units.value('pressure', v), unit: units.label('pressure'), digits: units.system === 'imperial' ? 2 : 0 };
    case 'pressure_tend_3h': return { conv: (v) => units.value('pressure', v) - units.value('pressure', 0), unit: units.label('pressure') + '/3h', digits: units.system === 'imperial' ? 2 : 1 };
    case 'wind_kmh': case 'gust_kmh': return { conv: (v) => units.value('wind', v), unit: units.label('wind'), digits: 0 };
    case 'precip_mm': return { conv: (v) => units.value('precip', v), unit: units.label('precip'), digits: 2 };
    case 'visibility_km': return { conv: (v) => units.value('visibility', v * 1000), unit: units.label('visibility') };
    default: return { conv: id, unit: null };
  }
}

export function mountImpact(host, ctx, { reg, refreshers }) {
  const { api, units, ui, charts } = ctx;
  const { h } = ui;

  // ------------------------------------------------------------------ history aligned with range
  let span = 48;
  const spanSeg = ui.segmented({ options: [{ value: 48, label: '48 h' }, { value: 168, label: '7 d' }, { value: 720, label: '30 d' }], value: span, onChange: (v) => { span = v; histReload(); } });
  const histCard = ui.card({ title: 'Weather history and range', icon: 'chart', sub: 'Hourly weather aligned with what the receiver achieved', actions: spanSeg });
  const HIST_H = 660;
  const histPanel = reg(panel(histCard.body, {
    skeleton: 360,
    empty: (d) => (!(d.wx.obs || []).length && !(d.range?.hours || []).length ? {
      icon: 'history', title: 'Not enough data yet',
      text: 'Needs at least one stored weather hour (first download) or one complete hour of reception. Both lines grow hour by hour from now on.',
    } : null),
    build(el) {
      const chartEl = h('div.chart', { style: { '--h': HIST_H + 'px' } });
      const note = h('div');
      el.append(chartEl, note, caption('All panes share the time axis: move the pointer to read one hour across range, temperature, pressure, wind and refractivity. Look for range peaks (top) that line up with a steep negative dN/dh (bottom), calm wind and a small temperature - dew point gap: that is tropospheric enhancement.'));
      const chart = charts.create(chartEl);
      return (d) => {
        chart.set(historyOption(d, units, HIST_H), { replace: true });
        const nr = (d.range?.hours || []).length;
        ui.mount(note, nr ? null : h('p.wx-note', ui.icon('info'), ' No range statistics in this window yet: the range pane fills after the first complete hour of reception.'));
      };
    },
  }));
  const histReload = source(ctx, {
    every: 300000, panels: [histPanel],
    load: async (signal) => {
      const [wx, range] = await Promise.all([
        api.get('/api/weather/hourly', { params: { hours: span, ahead: 0 }, signal }),
        soft(api.get('/api/stats/range', { params: { hours: span }, signal })),
      ]);
      return { wx, range };
    },
  });
  refreshers.push(histReload);

  // ------------------------------------------------------------------ impact analysis
  const impCard = ui.card({ title: 'Weather impact on reception', icon: 'target', sub: 'Does the weather explain your range? Statistics over the station’s own record' });
  let corrKind = 'partial';
  let metric = 'p95_km';
  let respVar = null;
  const impPanel = reg(panel(impCard.body, {
    skeleton: 260,
    build(el) {
      const suffEl = h('div.wx-suff');
      const conclEl = h('div');
      const corrSeg = ui.segmented({
        options: [
          { value: 'partial', label: 'Partial r', tip: 'Correlation with the traffic level held constant - the fairest single number' },
          { value: 'pearson', label: 'Pearson r', tip: 'Linear correlation' },
          { value: 'spearman', label: 'Spearman ρ', tip: 'Rank correlation: robust against outliers and curved relations' },
          { value: 'bestLagR', label: 'Best-lag r', tip: 'Strongest correlation when the weather series is shifted by up to ±6 h' },
        ],
        value: corrKind, onChange: (v) => { corrKind = v; draw(); },
      });
      const metricSeg = ui.segmented({ options: Object.keys(METRICS).map((m) => ({ value: m, label: METRICS[m].label, tip: METRICS[m].tip })), value: metric, onChange: (v) => { metric = v; draw(); } });
      const matrixEl = h('div.chart', { style: { '--h': '470px' } });
      const matrixWrap = h('div');
      const tableWrap = h('div');
      const respSel = h('div');
      const respEl = h('div.chart', { style: { '--h': '300px' } });
      const respWrap = h('div');
      const modelWrap = h('div');
      const modelEl = h('div.chart', { style: { '--h': '300px' } });
      const epiWrap = h('div');
      el.append(
        h('div.wx-grid.wx-grid--2',
          h('div', ui.section('Data sufficiency', 'database'), suffEl),
          h('div', ui.section('Conclusions', 'check'), conclEl)),
        h('div.wx-grid.wx-grid--2.wx-gap-top',
          h('div', h('div.wx-subhead', ui.section('Correlation matrix', 'chart'), corrSeg), matrixWrap),
          h('div', h('div.wx-subhead', ui.section('Correlation table', 'list'), metricSeg), tableWrap)),
        h('div.wx-grid.wx-grid--2.wx-gap-top',
          h('div', h('div.wx-subhead', ui.section('Response curve', 'chart'), respSel), respWrap),
          h('div', ui.section('Regression: what explains the p95 range', 'ruler'), modelWrap)),
        h('div.wx-gap-top', ui.section('Ducting and long-range episodes', 'bolt'), epiWrap));
      let matrix = null;
      let resp = null;
      let model = null;
      let data = null;

      const emptyBox = (title, text) => ui.empty({ icon: 'history', compact: true, title, text });

      function draw() {
        const d = data;
        if (!d) return;
        const need = d.minHours || 48;
        // --- sufficiency
        const mdl = d.model || {};
        const mNeed = modelNeed(mdl);
        ui.mount(suffEl,
          progress('Hours with range and weather data', d.hours, need, `${units.int(d.hours)} of ${units.int(need)} h`),
          progress('Range hours logged', d.rangeHours, need, `${units.int(d.rangeHours)} h`, d.rangeHours >= need),
          progress('Weather hours stored', d.weatherHours, need, `${units.int(d.weatherHours)} h`, d.weatherHours >= need),
          mNeed ? progress('Complete hours for the regression model', mdl.n, mNeed, `${units.int(mdl.n)} of ${units.int(mNeed)} h`) : null,
          h('p.wx-note', ui.icon('info'), ` Analysis window ${dayTime(d.from)} – ${dayTime(d.to)}${d.ant ? ' · antenna ' + d.ant : ''}. Recomputed by the server every 5 minutes.`));
        // --- conclusions
        ui.mount(conclEl, (d.conclusions || []).length ? notesList(d.conclusions, d.enough ? '' : '.wx-notes--warn') : emptyBox('No conclusions yet', `At least ${need} hours with both range and weather data are needed.`));
        // --- matrix
        const corr = d.correlations || [];
        if (!corr.length) {
          if (matrix) { matrix.dispose(); matrix = null; }
          corrSeg.classList.add('hidden');
          metricSeg.classList.add('hidden');
          ui.mount(matrixWrap, emptyBox('Not enough data yet', `Correlations are computed once ${need} hours with both range and weather data exist (${units.int(d.hours)} so far).`));
          ui.mount(tableWrap, emptyBox('Not enough data yet', 'The table lists r, p-value, sample size and best lag per weather variable.'));
        } else {
          corrSeg.classList.remove('hidden');
          metricSeg.classList.remove('hidden');
          if (!matrix) {
            ui.mount(matrixWrap, matrixEl, caption('Each cell is the correlation between a weather variable (rows) and a reception metric (columns): blue to cyan = the metric rises with the variable, red = it falls, dark = no relation. A dot marks cells that stay significant after correcting for the number of variables tested. Values beyond about ±0.3 with a dot are worth attention.'));
            matrix = charts.create(matrixEl);
          }
          matrix.set(matrixOption(d, corrKind, units, charts), { replace: true });
          const vars = new Map((d.vars || []).map((v) => [v.key, v]));
          ui.mount(tableWrap, ui.table({
            sort: { key: 'partial', dir: 'desc' },
            columns: [
              { key: 'var', label: 'Variable', render: (r) => vars.get(r.var)?.label || r.var, sort: (r) => vars.get(r.var)?.label || r.var },
              { key: 'pearson', label: 'r', align: 'right', num: true, render: (r) => signed(r.pearson), sort: (r) => (ok(r.pearson) ? Math.abs(r.pearson) : null), sortDir: 'desc', tip: 'Pearson correlation' },
              { key: 'pearsonP', label: 'p', align: 'right', num: true, render: (r) => pText(r.pearsonP), sort: true, tip: 'Probability of seeing such a correlation by chance (small = unlikely to be chance). Hourly values are correlated in time, so read p as optimistic.' },
              { key: 'spearman', label: 'ρ', align: 'right', num: true, render: (r) => signed(r.spearman), sort: (r) => (ok(r.spearman) ? Math.abs(r.spearman) : null), sortDir: 'desc', tip: 'Spearman rank correlation' },
              { key: 'partial', label: 'partial r', align: 'right', num: true, render: (r) => signed(r.partial), sort: (r) => (ok(r.partial) ? Math.abs(r.partial) : null), sortDir: 'desc', tip: 'Correlation at constant traffic level' },
              { key: 'partialP', label: 'p', align: 'right', num: true, render: (r) => pText(r.partialP), sort: true },
              { key: 'n', label: 'n', align: 'right', num: true, render: (r) => units.int(r.n), sort: true, tip: 'Hours used' },
              { key: 'bestLagH', label: 'Best lag', align: 'right', num: true, render: (r) => (ok(r.bestLagR) ? `${r.bestLagH > 0 ? '+' : ''}${r.bestLagH} h (${signed(r.bestLagR)})` : units.DASH), sort: true, tip: 'Shift with the strongest correlation; positive = range follows the weather by that many hours' },
              { key: 'significant', label: '', render: (r) => (r.significant ? ui.badge('significant', 'green') : ''), sort: (r) => (r.significant ? 1 : 0), sortDir: 'desc' },
            ],
            rows: corr.filter((c) => c.metric === metric),
            empty: 'No correlations for this metric',
          }), caption(`${metricLabel(metric)} against every weather variable, strongest partial correlation first. Click a column to sort.`));
        }
        // --- response curves
        const responses = (d.responses || []).filter((r) => (r.bins || []).length >= 2);
        if (!responses.length) {
          if (resp) { resp.dispose(); resp = null; }
          ui.clear(respSel);
          ui.mount(respWrap, emptyBox('Not enough data yet', 'Response curves show the typical range in bins of a weather variable (for example range at 0-2, 2-4, 4-6 kt of wind). They need the same 48 h as the correlations.'));
        } else {
          const vars = new Map((d.vars || []).map((v) => [v.key, v]));
          if (!respVar || !responses.some((r) => r.var === respVar)) {
            const best = corr.filter((c) => c.metric === responses[0].metric && ok(c.partial)).sort((a, b) => Math.abs(b.partial) - Math.abs(a.partial))[0];
            respVar = best && responses.some((r) => r.var === best.var) ? best.var : responses[0].var;
          }
          ui.mount(respSel, ui.select({ options: responses.map((r) => ({ value: r.var, label: vars.get(r.var)?.label || r.var })), value: respVar, size: 'sm', onChange: (v) => { respVar = v; draw(); } }));
          if (!resp) {
            ui.mount(respWrap, respEl, caption('Mean p95 range in each bin of the chosen weather variable, with the band between the lower and upper quantile of the hours in that bin. A flat line means the variable does not matter; a clear slope beyond the band width is a real effect.'));
            resp = charts.create(respEl);
          }
          const r = responses.find((x) => x.var === respVar);
          resp.set(responseOption(r, vars.get(respVar), units, charts), { replace: true });
        }
        // --- regression
        if (!mdl.ok || !(mdl.factors || []).length) {
          if (model) { model.dispose(); model = null; }
          ui.mount(modelWrap, emptyBox('Regression model not fitted yet', mdl.reason ? capFirst(mdl.reason) + '.' : 'The ridge regression needs about 80 complete hours.'));
        } else {
          const kvs = ui.kv([
            ['Hours used', units.int(mdl.n)],
            ['R² (in sample)', units.num(mdl.r2, 2, 2), { tip: 'Share of the range variation the model reproduces on the data it was fitted to' }],
            ['R² (cross-validated)', units.num(mdl.cvR2, 2, 2), { tip: 'Blocked 5-fold cross-validation: the honest figure. Near zero or negative = no predictive power.' }],
            ['Traffic + time of day only', units.num(mdl.baseR2, 2, 2)],
            ['Added by weather', signed(mdl.weatherR2), { tip: 'R² minus the traffic/time-only R²: what the weather variables add' }],
            ['Residual spread', units.dist(mdl.residStdKm)],
          ], { columns: 2 });
          if (!model) {
            ui.mount(modelWrap, h('div.wx-model-kv'), modelEl, caption('Standardised coefficients of a ridge regression of the hourly p95 range: bar length = how much the range moves (in standard deviations) when the factor rises by one standard deviation, everything else equal. Right = more range, left = less. Compare the weather bars with the traffic and time-of-day bars to see what really drives your numbers.'),
              ui.legend([{ label: 'weather', color: colors.cyan }, { label: 'traffic', color: colors.amber }, { label: 'time of day', color: colors.violet }]));
            model = charts.create(modelEl);
          }
          ui.mount(modelWrap.querySelector('.wx-model-kv'), kvs);
          model.set(factorOption(mdl, units, charts), { replace: true });
        }
        // --- episodes
        const ep = d.episodes || [];
        ui.mount(epiWrap, ep.length ? ui.table({
          columns: [
            { key: 'from', label: 'From', render: (r) => dayTime(r.from), sort: true, sortDir: 'desc' },
            { key: 'to', label: 'To', render: (r) => dayTime(r.to) },
            { key: 'hours', label: 'Hours', align: 'right', num: true, sort: true, sortDir: 'desc' },
            { key: 'peakP95Km', label: 'Peak p95', align: 'right', num: true, render: (r) => units.dist(r.peakP95Km), sort: true, sortDir: 'desc' },
            { key: 'peakMaxKm', label: 'Peak max', align: 'right', num: true, render: (r) => units.dist(r.peakMaxKm), sort: true, sortDir: 'desc' },
            { key: 'peakZ', label: 'Anomaly z', align: 'right', num: true, render: (r) => signed(r.peakZ, 1), sort: true, sortDir: 'desc', tip: 'Robust z-score of the range anomaly: above about 3 is clearly unusual' },
            { key: 'minDnDh', label: 'Min dN/dh', align: 'right', num: true, render: (r) => (ok(r.minDnDh) ? signed(r.minDnDh, 0) + ' N/km' : units.DASH), sort: true },
            { key: 'flags', label: 'Evidence', render: (r) => h('span.row.gap-1', r.ductFlag ? ui.badge('duct in profile', 'violet') : null, r.anomalous ? ui.badge('range anomaly', 'cyan') : null) },
            { key: 'reason', label: 'Why', render: (r) => h('span.wx-wrap', r.reason || '') },
          ],
          rows: ep, sort: { key: 'from', dir: 'desc' },
        }) : emptyBox(d.enough ? 'No episodes detected' : 'Not enough data yet',
          d.enough ? `No run of hours with a duct in the weather profile or an anomalously long range in the ${units.int(d.hours)} hours analysed. Episodes are rare - a handful per month is typical in settled high-pressure weather.`
            : 'Episodes are runs of hours with a duct flag from the weather analysis or a range far above the station’s norm. The norm needs 48 h of data first.'),
        ep.length ? caption('An episode is most convincing when both badges are present: the weather profile shows a duct and the receiver really saw further than usual.') : null);
      }

      return (d) => { data = d; draw(); };
    },
  }));
  refreshers.push(source(ctx, {
    every: 120000, panels: [impPanel],
    load: (signal) => api.get('/api/weather/impact', { signal, timeout: 30000 }),
  }));

  host.append(histCard, impCard);
}

const capFirst = (s) => (s ? s[0].toUpperCase() + s.slice(1) : s);

/** "... 59 complete hours, 80 needed ..." -> 80 (null when the reason has no such figure). */
function modelNeed(mdl) {
  if (!mdl || mdl.ok || !mdl.reason) return null;
  const m = /(\d+)\s+needed/.exec(mdl.reason);
  return m ? +m[1] : null;
}

function historyOption(d, units, H) {
  const obs = d.wx.obs || [];
  const rng = (d.range && d.range.hours) || [];
  const dl = units.label('dist');
  const tl = units.label('temp');
  const pl = units.label('pressure');
  const wl = units.label('wind');
  const rows = [
    { name: `range, ${dl}`, weight: 1.25, y2: { name: 'aircraft', min: 0 } },
    { name: tl },
    { name: pl, y2: { name: units.label('precip'), min: 0, scale: false } },
    { name: wl, min: 0, scale: false, y2: { name: 'cloud %', min: 0, max: 100, scale: false } },
    { name: 'dN/dh, N/km', y2: { name: 'RH %', min: 0, max: 100, scale: false } },
  ];
  const now = d.wx.now || Date.now();
  const tMin = Math.min(d.wx.from || Infinity, obs.length ? obs[0].t : Infinity, rng.length ? rng[0].t : Infinity);
  const g = stackedGrids(rows, { height: H, min: tMin, max: now, top: 44 });
  const S = (pane, name, color, data, o = {}) => ({
    name, type: o.type || 'line', xAxisIndex: pane, yAxisIndex: yIndex(rows, pane, !!o.y2), color, data,
    showSymbol: data.filter((p) => p[1] !== null && p[1] !== undefined).length < 3, symbolSize: 6, connectNulls: false, lineStyle: { width: o.width ?? 1.7, type: o.dashed ? 'dashed' : 'solid' },
    areaStyle: o.area ? { color, opacity: 0.1 } : undefined, barMaxWidth: 6, itemStyle: o.type === 'bar' ? { opacity: 0.75 } : undefined, z: o.z,
    step: o.step,
  });
  const pt = (list, f) => list.map((r) => [r.t, f(r)]);
  const pd = units.system === 'imperial' ? 2 : 1;
  const series = [
    S(0, 'Max range', colors.blue, pt(rng, (r) => uv('dist', r.maxKm, 0)), { width: 1.2 }),
    S(0, 'p95 range', colors.cyan, pt(rng, (r) => uv('dist', r.p95Km, 0)), { width: 2.2, area: true }),
    S(0, 'Aircraft', colors.grey, pt(rng, (r) => r.aircraft), { y2: true, width: 1, dashed: true }),
    S(1, 'Temperature', colors.amber, pt(obs, (r) => uv('temp', r.tempC))),
    S(1, 'Dew point', colors.teal, pt(obs, (r) => uv('temp', r.dewpointC)), { dashed: true }),
    S(2, 'Pressure', colors.violet, pt(obs, (r) => uv('pressure', r.pressureMsl, pd))),
    S(2, 'Precipitation', colors.blue, pt(obs, (r) => (r.precipMm > 0 ? uv('precip', r.precipMm, 2) : null)), { y2: true, type: 'bar' }),
    S(3, 'Wind', colors.green, pt(obs, (r) => uv('wind', r.windKmh, 0))),
    S(3, 'Gusts', colors.green, pt(obs, (r) => uv('wind', r.gustKmh, 0)), { dashed: true, width: 1 }),
    S(3, 'Cloud', colors.grey, pt(obs, (r) => round(r.cloud, 0)), { y2: true, width: 1, area: true }),
    S(4, 'dN/dh', colors.cyan, pt(obs, (r) => round(r.dnDh, 1)), { width: 2 }),
    S(4, 'Humidity', colors.blue, pt(obs, (r) => round(r.rh, 0)), { y2: true, width: 1, dashed: true }),
    S(4, 'Duct', colors.violet, obs.filter((r) => r.duct > 0).map((r) => [r.t, round(r.dnDh, 1)]), { type: 'scatter' }),
  ];
  series[10].markLine = {
    silent: true, symbol: 'none', lineStyle: { type: 'dashed', color: colors.text4 }, label: { position: 'insideStartTop', fontSize: 9.5, color: colors.text3 },
    data: [{ yAxis: -40, name: 'standard', label: { formatter: 'standard −40' } }, { yAxis: -79, label: { formatter: 'super-refraction −79' } }],
  };
  series[12].symbol = 'diamond';
  series[12].symbolSize = 9;
  const fmt = {
    'Max range': dl, 'p95 range': dl, Aircraft: (v) => units.int(v), Temperature: (v) => units.num(v, 1) + ' ' + tl, 'Dew point': (v) => units.num(v, 1) + ' ' + tl,
    Pressure: (v) => units.num(v, pd) + ' ' + pl, Precipitation: (v) => units.num(v, 2) + ' ' + units.label('precip'), Wind: wl, Gusts: wl, Cloud: '%',
    'dN/dh': (v) => signed(v, 0) + ' N/km', Humidity: '%', Duct: () => 'in profile',
  };
  return {
    animation: false,
    legend: { show: true, top: 0, left: 0, type: 'scroll' },
    tooltip: { trigger: 'axis', confine: true, formatter: timeTooltip(fmt), axisPointer: { type: 'line' } },
    axisPointer: { link: [{ xAxisIndex: 'all' }] },
    dataZoom: [{ type: 'inside', xAxisIndex: rows.map((_, i) => i), throttle: 50 }],
    ...g,
    series,
  };
}

function matrixOption(d, kind, units, charts) {
  const vars = d.vars || [];
  const metrics = d.metrics || [];
  const x = metrics.map(metricLabel);
  const y = vars.map((v) => v.label).reverse();
  const by = new Map();
  const data = [];
  for (const c of d.correlations || []) {
    const xi = metrics.indexOf(c.metric);
    const vi = vars.findIndex((v) => v.key === c.var);
    if (xi < 0 || vi < 0) continue;
    const yi = vars.length - 1 - vi;
    by.set(xi + ':' + yi, c);
    const val = c[kind];
    data.push({ value: [xi, yi, ok(val) ? round(val, 2) : '-'], label: { show: true, formatter: () => (ok(val) ? signed(val) + (c.significant ? ' ●' : '') : '') } });
  }
  const opt = charts.heatmap({
    x, y, data: [], min: -1, max: 1, ramp: charts.ramps.diverging,
    valueFormat: (v) => signed(v, 1),
    tooltip: (p) => {
      const c = by.get(p.value[0] + ':' + p.value[1]);
      if (!c) return '';
      const row = (a, b) => `<div class="wx-tt-row"><span>${a}</span><b>${esc(b)}</b></div>`;
      return `<div class="wx-tt"><div class="wx-tt-head">${esc(y[p.value[1]])} → ${esc(x[p.value[0]])}</div>`
        + row('Pearson r', `${signed(c.pearson)} (p ${pText(c.pearsonP)})`)
        + row('Spearman ρ', `${signed(c.spearman)} (p ${pText(c.spearmanP)})`)
        + row('Partial r (traffic fixed)', `${signed(c.partial)} (p ${pText(c.partialP)})`)
        + row('Best lag', ok(c.bestLagR) ? `${c.bestLagH > 0 ? '+' : ''}${c.bestLagH} h, r ${signed(c.bestLagR)}` : '—')
        + row('Hours (n)', units.int(c.n))
        + row('Significant', c.significant ? 'yes (Bonferroni-corrected)' : 'no') + '</div>';
    },
  });
  opt.series[0].data = data;
  opt.series[0].label = { show: true, fontSize: 10, fontFamily: "'JetBrains Mono Variable',Consolas,monospace", color: '#e8f0ff' };
  opt.visualMap.dimension = 2;
  opt.grid.top = 26;
  opt.xAxis.position = 'top';
  opt.xAxis.axisLabel = { interval: 0, fontSize: 10 };
  opt.yAxis.axisLabel = { interval: 0, fontSize: 10.5, width: 130, overflow: 'truncate' };
  return opt;
}

function responseOption(r, info, units, charts) {
  const cv = varConv(r.var, units);
  const m = METRICS[r.metric] || { label: r.metric };
  const yc = (v) => (m.kind ? uv(m.kind, v, 0) : round(v, 1));
  const yl = m.kind ? units.label(m.kind) : m.unit || '';
  const xu = cv.unit ?? info?.unit ?? '';
  const bins = r.bins.filter((b) => ok(b.xMean) && ok(b.yMean));
  const X = (b) => round(cv.conv(b.xMean), 2);
  const band = bins.map((b) => {
    const q = (b.yq || []).filter(ok);
    return [X(b), q.length >= 2 ? yc(q[0]) : null, q.length >= 2 ? yc(q[q.length - 1]) : null];
  });
  const opt = charts.line({
    xType: 'value', xName: (info?.label || r.var) + (xu ? `, ${xu}` : ''), yName: `${m.label}${yl ? ', ' + yl : ''}`, yScale: true, legend: false,
    series: [{ name: 'Mean ' + m.label, data: bins.map((b) => [X(b), yc(b.yMean), b.n]), color: colors.cyan, width: 2.2 }],
    tooltip: (ps) => {
      const p = (Array.isArray(ps) ? ps : [ps]).find((q) => q.seriesName && !q.seriesName.startsWith('band-'));
      if (!p) return '';
      const b = bins[p.dataIndex];
      return `<div class="wx-tt"><div class="wx-tt-head">${esc(info?.label || r.var)} ${units.num(cv.conv(b.lo), cv.digits ?? 1)} – ${units.num(cv.conv(b.hi), cv.digits ?? 1)} ${esc(xu)}</div>`
        + `<div class="wx-tt-row"><span>Mean ${esc(m.label)}</span><b>${units.num(p.value[1], 0)} ${esc(yl)}</b></div>`
        + `<div class="wx-tt-row"><span>Hours in bin</span><b>${units.int(b.n)}</b></div></div>`;
    },
  });
  opt.xAxis.scale = true;
  opt.xAxis.splitLine = { show: true, lineStyle: { color: colors.line, type: [2, 4] } };
  opt.grid.bottom = 30;
  opt.series[0].showSymbol = true;
  opt.series[0].symbolSize = 7;
  opt.series[0].sampling = undefined;
  opt.series.push(...bandSeries('iqr', band, colors.cyan, { connectNulls: true }));
  return opt;
}

function factorOption(mdl, units, charts) {
  const GROUP = { weather: colors.cyan, traffic: colors.amber, time: colors.violet };
  const fs = [...mdl.factors].filter((f) => ok(f.beta)).sort((a, b) => Math.abs(a.beta) - Math.abs(b.beta)).slice(-14);
  const dk = units.value('dist', 1);
  return {
    animationDuration: 250,
    grid: { left: 8, right: 18, top: 8, bottom: 8, containLabel: true },
    tooltip: {
      trigger: 'item', confine: true,
      formatter: (p) => {
        const f = fs[p.dataIndex];
        return `<div class="wx-tt"><div class="wx-tt-head">${esc(f.label || f.name)} · ${esc(f.group)}</div>`
          + `<div class="wx-tt-row"><span>Standardised effect β</span><b>${signed(f.beta)}</b></div>`
          + `<div class="wx-tt-row"><span>Per unit of the factor</span><b>${ok(f.perUnit) ? signed(f.perUnit * dk, 2) + ' ' + units.label('dist') : '—'}</b></div></div>`;
      },
    },
    xAxis: { type: 'value', name: 'β (std. dev. of range per std. dev. of factor)', nameLocation: 'middle', nameGap: 24, splitLine: { show: true, lineStyle: { color: colors.line, type: [2, 4] } } },
    yAxis: { type: 'category', data: fs.map((f) => f.label || f.name), axisLabel: { fontSize: 10.5, width: 150, overflow: 'truncate' }, axisTick: { show: false } },
    series: [{
      type: 'bar', barMaxWidth: 14,
      data: fs.map((f) => ({ value: round(f.beta, 3), itemStyle: { color: GROUP[f.group] || colors.grey, borderRadius: f.beta >= 0 ? [0, 3, 3, 0] : [3, 0, 0, 3] } })),
      markLine: { silent: true, symbol: 'none', lineStyle: { color: colors.line2, type: 'solid' }, label: { show: false }, data: [{ xAxis: 0 }] },
    }],
  };
}
