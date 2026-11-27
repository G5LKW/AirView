// weather/profile.js - vertical profile section of the Weather page: model (Open-Meteo pressure levels)
// against the aircraft-derived profile (Mode S EHS / MRAR), difference statistics, winds aloft and the
// time-height history.   Endpoints: /api/weather/profile, /api/met/profile, /api/met/compare, /api/met/history.

import {
  FT_PER_M, HOUR, ok, esc, hm, dayHm, dayTime, fmtDelta, signed, uv, round, dewPoint, caption, panel, source, soft, colors,
} from './common.js';

const MODEL = colors.blue;
const OBS = colors.amber;

export function mountProfiles(host, ctx, { reg, refreshers }) {
  const { api, units, ui, charts } = ctx;
  const { h } = ui;

  // ------------------------------------------------------------------ profile charts
  let range = 'full';
  const rangeSeg = ui.segmented({
    options: [{ value: 'full', label: 'Full' }, { value: 'low', label: 'Below 10,000 ft', tip: 'Zoom on the boundary layer, where inversions and ducts form' }],
    value: range, onChange: (v) => { range = v; profPanel.rerender(); },
  });
  const profCard = ui.card({ title: 'Vertical profile', icon: 'layers', sub: 'Weather model versus what aircraft measure', actions: rangeSeg });
  const profPanel = reg(panel(profCard.body, {
    skeleton: 320,
    empty: (d) => (!modelLevels(d).length && !(d.met?.levels || []).length ? {
      icon: 'layers', title: 'No profile data yet',
      text: 'The model profile arrives with the first weather download. The aircraft-derived profile needs Mode S EHS (BDS 5,0 / 6,0) or MRAR replies from aircraft in range; it fills within minutes once such aircraft are tracked.',
    } : null),
    build(el) {
      const hosts = ['temp', 'rh', 'speed', 'dir'].map(() => h('div.chart', { style: { '--h': '360px' } }));
      const titles = ['Temperature and dew point', 'Relative humidity', 'Wind speed', 'Wind direction (from)'];
      const statsEl = h('div.wx-inline-stats');
      const noteEl = h('div');
      el.append(
        h('div.wx-profile-grid', hosts.map((c, i) => h('div.wx-profile-cell', h('div.wx-profile-title', titles[i]), c))),
        ui.legend([{ label: 'Open-Meteo model', color: MODEL }, { label: 'Model dew point', color: colors.teal }, { label: 'Aircraft-derived', color: OBS }]),
        statsEl, noteEl,
        caption('Height runs up the vertical axis. Where the amber aircraft points sit on the blue model line, the model describes the air above you well. A temperature line that leans right with height is an inversion - the classic cause of extended ADS-B range. Arrows on the wind chart point where the measured wind blows to (north up).'));
      const cs = hosts.map((c) => charts.create(c));
      return (d) => {
        const o = profileOptions(d, range, units);
        cs.forEach((c, i) => c.set(o[i], { replace: true }));
        ui.mount(statsEl, compareStats(d, ctx));
        const m = d.met;
        ui.mount(noteEl, h('p.wx-note', ui.icon('plane'),
          m && (m.nWind || m.nTemp)
            ? ` Aircraft profile: ${units.int(m.nWind)} wind and ${units.int(m.nTemp)} temperature observations in the last ${units.num(m.windowMin, 0)} min (${(m.levels || []).length} layers).`
            : ' No aircraft-derived observations in the current window.',
          d.model && d.model.t ? ` Model profile valid ${dayTime(d.model.t)} (${d.model.source || 'model'}).` : ' No model profile for this hour.'));
      };
    },
  }));

  // ------------------------------------------------------------------ winds aloft
  const windsCard = ui.card({ title: 'Winds aloft', icon: 'wind', sub: 'Aircraft-derived, at standard flight levels' });
  const windsPanel = reg(panel(windsCard.body, {
    lines: 6,
    empty: (d) => (!(d.met?.windsAloft || []).length && !(d.compare?.comparison?.levels || []).length ? {
      icon: 'wind', title: 'No winds aloft yet',
      text: 'Winds are computed from the difference between ground track / ground speed and heading / true airspeed that aircraft report via Mode S EHS. A few equipped aircraft in range for some minutes are enough.',
    } : null),
    build(el) {
      const analysis = h('div.wx-analysis');
      const flHost = h('div');
      const cmpHost = h('div');
      const tabs = ui.tabs({
        tabs: [{ id: 'fl', label: 'Flight levels', content: flHost }, { id: 'cmp', label: 'Aircraft vs model', content: cmpHost }],
      });
      el.append(analysis, tabs, caption('“Coded” is the classic winds-aloft group DDSS±TT (direction in tens of degrees, speed in knots, temperature). ISA dev. is the temperature difference to the standard atmosphere at that level. In the comparison, differences are aircraft minus model.'));
      return (d) => {
        const m = d.met || {};
        const tp = m.tropopause;
        const mw = m.maxWind;
        const sh = m.maxShear;
        ui.mount(analysis, ui.kv([
          ['Freezing level', ok(m.freezingLevelFt) ? units.alt(m.freezingLevelFt) : units.DASH],
          ['Tropopause', tp ? `${units.alt(tp.altFt, { fl: true })} · ${units.temp(tp.tempC)}${tp.confirmed ? '' : ' (unconfirmed)'}` : units.DASH],
          ['Strongest wind', mw ? `${units.speed(mw.windKt)} from ${units.deg(mw.windDir)} at ${units.alt(mw.altFt, { fl: true })}${mw.jet ? ' · jet stream' : ''}` : units.DASH],
          ['Strongest shear', sh ? `${units.speed(sh.ktPer1000Ft, { digits: 1 })} per ${units.alt(1000)} at ${units.alt((sh.baseFt + sh.topFt) / 2, { fl: true })}` : units.DASH],
          ['Inversions aloft', (m.inversions || []).length ? (m.inversions || []).map((v) => `${units.alt(v.baseFt)} – ${units.alt(v.topFt)} (${fmtDelta('temp', v.strengthC)})`).join(', ') : 'none measured', { text: true }],
        ]));
        ui.mount(flHost, ui.table({
          empty: 'No flight level has enough observations yet',
          columns: [
            { key: 'fl', label: 'Level', render: (r) => 'FL' + String(r.fl).padStart(3, '0') },
            { key: 'altFt', label: 'Altitude', align: 'right', num: true, render: (r) => units.alt(r.altFt) },
            { key: 'windDir', label: 'From', align: 'right', num: true, render: (r) => (ok(r.windDir) ? units.bearing(r.windDir) : units.DASH) },
            { key: 'windKt', label: 'Speed', align: 'right', num: true, render: (r) => units.speed(r.windKt) },
            { key: 'tempC', label: 'Temp', align: 'right', num: true, render: (r) => units.temp(r.tempC) },
            { key: 'isaDev', label: 'ISA dev.', align: 'right', num: true, render: (r) => fmtDelta('temp', r.isaDev) },
            { key: 'coded', label: 'Coded', num: true, render: (r) => r.coded || units.DASH },
          ],
          rows: m.windsAloft || [],
        }));
        const cmp = d.compare;
        ui.mount(cmpHost, cmp && !cmp.modelAvailable
          ? ui.empty({ icon: 'cloud', compact: true, title: 'No model profile to compare with', text: 'The comparison needs the Open-Meteo pressure-level profile of the current hour.' })
          : ui.table({
            empty: 'No model level inside the altitude range aircraft have sampled yet',
            columns: [
              { key: 'hpa', label: 'Level', render: (r) => units.num(r.hpa, 0) + ' hPa' },
              { key: 'altFt', label: 'Altitude', align: 'right', num: true, render: (r) => units.alt(r.altFt, { fl: true }) },
              { key: 'obsTempC', label: 'T aircraft', align: 'right', num: true, render: (r) => units.temp(r.obsTempC) },
              { key: 'modelTempC', label: 'T model', align: 'right', num: true, render: (r) => units.temp(r.modelTempC) },
              { key: 'dTempC', label: 'ΔT', align: 'right', num: true, render: (r) => fmtDelta('temp', r.dTempC) },
              { key: 'obsWindKt', label: 'Wind aircraft', align: 'right', num: true, render: (r) => (ok(r.obsWindKt) ? `${units.deg(r.obsWindDir)} / ${units.speed(r.obsWindKt)}` : units.DASH) },
              { key: 'modelWindKt', label: 'Wind model', align: 'right', num: true, render: (r) => (ok(r.modelWindKt) ? `${units.deg(r.modelWindDir)} / ${units.speed(r.modelWindKt)}` : units.DASH) },
              { key: 'vecDiffKt', label: 'Vector diff.', align: 'right', num: true, render: (r) => units.speed(r.vecDiffKt, { digits: 1 }) },
            ],
            rows: cmp?.comparison?.levels || [],
          }));
      };
    },
  }));

  refreshers.push(source(ctx, {
    every: 60000, panels: [profPanel, windsPanel],
    load: async (signal) => {
      const [model, met, compare] = await Promise.all([
        soft(api.get('/api/weather/profile', { signal })),
        api.get('/api/met/profile', { signal }),
        soft(api.get('/api/met/compare', { signal })),
      ]);
      return { model, met, compare };
    },
  }));

  // ------------------------------------------------------------------ time-height history
  let thVar = 'wind';
  let thHours = 24;
  const thVarSeg = ui.segmented({
    options: [{ value: 'wind', label: 'Wind' }, { value: 'temp', label: 'Temperature' }, { value: 'isa', label: 'ISA dev.' }, { value: 'n', label: 'Samples' }],
    value: thVar, onChange: (v) => { thVar = v; thPanel.rerender(); },
  });
  const thSpanSeg = ui.segmented({ options: [{ value: 24, label: '24 h' }, { value: 72, label: '3 d' }, { value: 168, label: '7 d' }], value: thHours, onChange: (v) => { thHours = v; thReload(); } });
  const thCard = ui.card({ title: 'Time-height history', icon: 'history', sub: 'Aircraft-derived wind and temperature per hour and 1,000 ft layer', actions: [thVarSeg, thSpanSeg] });
  const thPanel = reg(panel(thCard.body, {
    skeleton: 320,
    empty: (d) => (!(d.cells || []).length ? {
      icon: 'history', title: 'Not enough data yet',
      text: 'One column is stored per hour in which aircraft delivered wind or temperature observations. The first column appears within the first hour of reception with Mode S EHS traffic.',
    } : null),
    build(el) {
      const chartEl = h('div.chart', { style: { '--h': '380px' } });
      const cap = h('div');
      el.append(chartEl, cap);
      const chart = charts.create(chartEl);
      return (d) => {
        chart.set(timeHeightOption(d, thVar, units, charts), { replace: true });
        ui.mount(cap, caption({
          wind: 'Wind speed by time (left to right) and altitude (bottom to top). A bright band near the top is the jet stream; watch it move up, down or through as weather systems pass.',
          temp: 'Air temperature by time and altitude. Horizontal stripes are normal (colder with height); a warm tongue pushing down or a sudden shift of all colours marks a change of air mass (front).',
          isa: 'Temperature difference to the standard atmosphere. Blue = colder than standard, cyan = warmer. Aircraft climb better and fly higher flight levels in cold air; a sign change over time shows a front passing.',
          n: 'Number of observations behind each cell. Dark gaps are hours or layers without Mode S EHS traffic - values there are simply unknown, not zero.',
        }[thVar]));
      };
    },
  }));
  const thReload = source(ctx, {
    every: 300000, panels: [thPanel],
    load: (signal) => api.get('/api/met/history', { params: { hours: thHours }, signal }),
  });
  refreshers.push(thReload);

  host.append(h('div.wx-grid.wx-grid--profile', profCard, windsCard), thCard);

  // ---------------------------------------------------------------------------------------------

  function compareStats(d) {
    const c = d.compare && d.compare.comparison;
    if (!d.compare || !d.compare.modelAvailable || !c || (!c.nTemp && !c.nWind)) {
      return h('span.muted', 'Difference statistics appear once aircraft have sampled at least one model pressure level.');
    }
    const item = (label, value, tip) => h('span', { tip }, label + ' ', h('b.num', value));
    return [
      h('span.wx-inline-head', 'Aircraft − model:'),
      c.nTemp ? item('temperature bias', fmtDelta('temp', c.tempBiasC), `Mean difference over ${c.nTemp} levels; positive = the air is warmer than the model says`) : null,
      c.nTemp ? item('RMS', fmtDelta('temp', c.tempRmsC).replace('+', ''), 'Root-mean-square temperature difference: below about 1.5 °C is a good match') : null,
      c.nWind ? item('wind speed bias', (c.speedBiasKt > 0 ? '+' : '') + units.speed(c.speedBiasKt, { digits: 1 }), `Mean speed difference over ${c.nWind} levels`) : null,
      c.nWind && ok(c.dirBiasDeg) ? item('direction', signed(c.dirBiasDeg, 0) + '°', 'Mean signed direction difference (levels with at least 10 kt)') : null,
      c.nWind ? item('RMS vector diff.', units.speed(c.rmsvdKt, { digits: 1 }), 'RMS vector difference - the standard wind verification score. Under about 10 kt is typical for a good model analysis; a fresh station with few observations will be noisier.') : null,
    ];
  }
}

const modelLevels = (d) => ((d.model && d.model.levels) || []).filter((l) => ok(l.z));

/** Four options (temperature, humidity, wind speed, wind direction) against altitude. */
function profileOptions(d, range, units) {
  const topFt = range === 'low' ? 10000 : null;
  const alt = (ft) => uv('alt', ft, 0);
  const inRange = (ft) => topFt === null || ft <= topFt * 1.05;
  const mdl = modelLevels(d).map((l) => ({ ...l, ft: l.z * FT_PER_M })).filter((l) => inRange(l.ft)).sort((a, b) => a.ft - b.ft);
  const obs = ((d.met && d.met.levels) || []).filter((l) => inRange(l.altFt));
  const al = units.label('alt');
  const allFt = [...mdl.map((l) => l.ft), ...obs.map((l) => l.altFt)];
  const yMax = topFt !== null ? alt(topFt) : alt(Math.min(50000, Math.ceil((Math.max(20000, ...allFt) + 500) / 5000) * 5000));
  const yMin = Math.min(0, alt(Math.min(0, ...allFt)));
  const marks = [];
  if (d.met && ok(d.met.freezingLevelFt) && inRange(d.met.freezingLevelFt)) marks.push({ yAxis: alt(d.met.freezingLevelFt), name: '0 °C level' });
  if (d.met && d.met.tropopause && inRange(d.met.tropopause.altFt)) marks.push({ yAxis: alt(d.met.tropopause.altFt), name: 'tropopause' });
  const base = (xName, tipFmt, extra = {}) => ({
    animationDuration: 250,
    grid: { left: 6, right: 14, top: 10, bottom: 26, containLabel: true },
    tooltip: { trigger: 'item', confine: true, formatter: (p) => `${p.marker}${esc(p.seriesName)}<br><b>${esc(tipFmt(p.value[0], p))}</b> at ${units.num(p.value[1], 0)} ${al}` },
    xAxis: { type: 'value', scale: true, name: xName, nameLocation: 'middle', nameGap: 22, splitNumber: 4, splitLine: { show: true, lineStyle: { color: colors.line, type: [2, 4] } }, ...extra },
    yAxis: { type: 'value', min: yMin, max: yMax, name: al, nameTextStyle: { align: 'left' }, axisLabel: { formatter: (v) => units.num(v, 0) } },
  });
  const ln = (name, color, data, o = {}) => ({ name, type: 'line', color, data, showSymbol: true, symbolSize: 4, lineStyle: { width: 1.8, ...(o.dashed ? { type: 'dashed' } : {}) }, connectNulls: true, ...o.extra });
  const markLine = marks.length ? { silent: true, symbol: 'none', lineStyle: { color: colors.text4, type: 'dashed' }, label: { formatter: '{b}', position: 'insideEndTop', color: colors.text3, fontSize: 9.5 }, data: marks } : undefined;

  const tl = units.label('temp');
  const temp = base(tl, (v) => units.num(v, 1) + ' ' + tl);
  temp.series = [
    ln('Model temperature', MODEL, mdl.filter((l) => ok(l.t)).map((l) => [uv('temp', l.t), alt(l.ft)]), { extra: { markLine } }),
    ln('Model dew point', colors.teal, mdl.filter((l) => ok(dewPoint(l.t, l.rh))).map((l) => [uv('temp', dewPoint(l.t, l.rh)), alt(l.ft)]), { dashed: true }),
    ln('Aircraft temperature', OBS, obs.filter((l) => ok(l.tempC)).map((l) => [uv('temp', l.tempC), alt(l.altFt)]), { extra: { symbolSize: 6, lineStyle: { width: 1.4 } } }),
  ];

  const rh = base('%', (v) => units.num(v, 0) + ' %', { min: 0, max: 100, scale: false });
  rh.series = [{ ...ln('Model relative humidity', colors.cyan, mdl.filter((l) => ok(l.rh)).map((l) => [round(l.rh, 0), alt(l.ft)])), areaStyle: undefined }];

  const sl = units.label('wind');
  const speed = base(sl, (v, p) => units.num(v, 0) + ' ' + sl + (ok(p.value[2]) ? ` from ${Math.round(p.value[2])}°` : ''), { min: 0, scale: false });
  speed.series = [
    ln('Model wind', MODEL, mdl.filter((l) => ok(l.ws)).map((l) => [uv('wind', l.ws, 0), alt(l.ft), l.wd])),
    ln('Aircraft wind', OBS, obs.filter((l) => ok(l.windKt)).map((l) => [uv('speed', l.windKt, 0), alt(l.altFt), l.windDir]), {
      extra: { symbol: 'arrow', symbolSize: 11, symbolRotate: (v) => (ok(v[2]) ? -(v[2] + 180) : 0), lineStyle: { width: 1.2, opacity: 0.7 } },
    }),
  ];

  const dir = base('° true', (v) => Math.round(v) + '° ' + units.cardinal(v), {
    min: 0, max: 360, interval: 90, scale: false,
    axisLabel: { formatter: (v) => ({ 0: 'N', 90: 'E', 180: 'S', 270: 'W', 360: 'N' }[v] ?? v) },
  });
  const sc = (name, color, data, size) => ({ name, type: 'scatter', color, data, symbolSize: size, itemStyle: { opacity: 0.9 } });
  dir.series = [
    sc('Model wind direction', MODEL, mdl.filter((l) => ok(l.wd)).map((l) => [round(l.wd, 0), alt(l.ft)]), 6),
    sc('Aircraft wind direction', OBS, obs.filter((l) => ok(l.windDir)).map((l) => [round(l.windDir, 0), alt(l.altFt)]), 7),
  ];
  return [temp, rh, speed, dir];
}

/** Heat map of the hourly aircraft-derived history. */
function timeHeightOption(d, v, units, charts) {
  const cells = d.cells || [];
  const t0 = Math.floor(Math.min(...cells.map((c) => c.t)) / HOUR) * HOUR;
  const t1 = Math.floor(Math.max(...cells.map((c) => c.t)) / HOUR) * HOUR;
  const nx = Math.round((t1 - t0) / HOUR) + 1;
  const lv0 = Math.min(...cells.map((c) => c.level));
  const lv1 = Math.max(...cells.map((c) => c.level));
  const x = [];
  for (let i = 0; i < nx; i++) x.push(nx > 30 ? dayHm(t0 + i * HOUR) : hm(t0 + i * HOUR));
  const y = [];
  // label each 1,000 ft layer by its base (FL350 = 35,000-35,999 ft); the tooltip shows the mean altitude
  for (let l = lv0; l <= lv1; l++) y.push(units.alt(l * 1000, { fl: true }));
  const pick = {
    wind: (c) => uv('speed', c.windKt, 0),
    temp: (c) => uv('temp', c.tempC, 1),
    isa: (c) => (ok(c.isaDev) ? round(units.value('temp', c.isaDev) - units.value('temp', 0), 1) : null),
    n: (c) => (c.nWind || 0) + (c.nTemp || 0),
  }[v];
  const unit = { wind: units.label('speed'), temp: units.label('temp'), isa: units.label('temp'), n: 'obs' }[v];
  const byKey = new Map();
  const data = [];
  for (const c of cells) {
    const val = pick(c);
    if (!ok(val)) continue;
    const xi = Math.round((c.t - t0) / HOUR);
    const yi = c.level - lv0;
    byKey.set(xi + ':' + yi, c);
    data.push([xi, yi, val]);
  }
  const vals = data.map((r) => r[2]);
  let min = vals.length ? Math.min(...vals) : 0;
  let max = vals.length ? Math.max(...vals) : 1;
  let ramp = charts.ramps.heat;
  if (v === 'isa') { const m = Math.max(Math.abs(min), Math.abs(max), 1); min = -m; max = m; ramp = ['#3b8bff', '#12305f', '#0d1c3c', '#0e5a6e', '#22d3ee']; }
  if (v === 'wind' || v === 'n') min = 0;
  if (v === 'n') ramp = charts.ramps.blue;
  const opt = charts.heatmap({
    x, y, data, min, max, ramp,
    valueFormat: (val) => units.num(val, v === 'temp' || v === 'isa' ? 1 : 0),
    tooltip: (p) => {
      const c = byKey.get(p.value[0] + ':' + p.value[1]);
      if (!c) return '';
      return `<div class="wx-tt"><div class="wx-tt-head">${esc(dayTime(c.t))} · ${esc(units.alt(c.altFt, { fl: true }))}</div>`
        + `<div class="wx-tt-row"><span>Wind</span><b>${ok(c.windKt) ? esc(units.deg(c.windDir) + ' / ' + units.speed(c.windKt)) : '—'}</b></div>`
        + `<div class="wx-tt-row"><span>Temperature</span><b>${esc(units.temp(c.tempC))}</b></div>`
        + `<div class="wx-tt-row"><span>ISA deviation</span><b>${esc(fmtDelta('temp', c.isaDev))}</b></div>`
        + `<div class="wx-tt-row"><span>Observations</span><b>${c.nWind || 0} wind · ${c.nTemp || 0} temp</b></div></div>`;
    },
  });
  opt.series[0].itemStyle = { borderWidth: 0 };
  opt.visualMap.text = [unit, ''];
  opt.xAxis.axisLabel = { hideOverlap: true };
  opt.yAxis.axisLabel = { interval: (i) => (lv0 + i) % 5 === 0 };
  opt.grid.top = 22;
  return opt;
}
