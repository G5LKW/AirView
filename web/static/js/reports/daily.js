// reports/daily.js - daily summary report: per station-local day the aircraft, flights, new airframes, peak,
// farthest flight and busiest hour over a chosen range. Source: /api/stats/daily, whose days carry
// `busiestHour` and `farthest` - one request. Older servers without those fields: /api/stats/rates (hourly
// aircraft counts -> busiest hour) plus one /api/reports query per day for the farthest flight (most recent
// 31 days, fetched lazily, three at a time).

import { DAY, HOUR, fin } from './common.js';
import { mountBriefing } from './briefing.js';

const RANGES = [{ value: 7, label: '7 days' }, { value: 14, label: '14 days' }, { value: 30, label: '30 days' }, { value: 90, label: '90 days' }];
const FARTHEST_MAX_DAYS = 31;

export function mountDaily(host, ctx, scope) {
  const { ui, units, api } = ctx;
  const { h } = ui;
  const prefs = ui.prefs('airview.reports.daily', { days: 14 });
  let days = RANGES.some((r) => r.value === prefs.get().days) ? prefs.get().days : 14;
  let model = null;
  let error = null;
  let seq = 0;
  const far = new Map();       // day t -> { row } | { none: true } | { error }

  const tiles = {
    flights: ui.stat({ label: 'Flights', icon: 'plane', accent: 'cyan' }),
    day: ui.stat({ label: 'Busiest day', icon: 'chart', accent: 'amber' }),
    hour: ui.stat({ label: 'Busiest hour', icon: 'timer', accent: 'violet' }),
    far: ui.stat({ label: 'Farthest', icon: 'radar', accent: 'teal' }),
  };
  for (const t of Object.values(tiles)) t.set(units.DASH);
  const rangeSeg = ui.segmented({ options: RANGES, value: days, onChange: (v) => { days = v; prefs.set({ days: v }); load(); } });
  const csvBtn = ui.button({ label: 'CSV', icon: 'download', size: 'sm', onClick: downloadCsv, tip: 'Download this summary as CSV' });
  const chartHost = h('div.rpd-chart-host');
  const tableHost = h('div.rpd-table-host');
  const chartCard = ui.card({ title: 'Traffic per day', icon: 'chart', sub: '', body: [chartHost, h('p.muted', { style: 'margin:8px 0 0;font-size:var(--fs-sm)' }, 'Bars: different aircraft and flights logged each day (left scale). Line: the farthest position received that day (right scale). A low day with a normal range usually means less traffic, not a receiver problem.')] });
  const tableCard = ui.card({ title: 'Daily summary', icon: 'report', sub: 'Click a day to list its flights', actions: csvBtn, body: tableHost, flush: true });
  host.append(h('div.rpd',
    mountBriefing(ctx, scope),
    h('div.rpd-bar', rangeSeg, h('span.muted.rpd-note', 'Days are station-local days.')),
    h('div.rpd-tiles', Object.values(tiles)),
    chartCard, tableCard));
  let chart = null;
  scope.onCleanup(() => { if (chart) chart.dispose(); });

  async function load() {
    const my = ++seq;
    model = null; error = null;
    far.clear();
    render();
    let legacy = false;
    try {
      const daily = await api.get('/api/stats/daily', { params: { days }, signal: scope.signal, timeout: 30000 });
      if (my !== seq) return;
      const list = Array.isArray(daily?.days) ? daily.days : [];
      // feature detection: newer servers put the busiest hour and the farthest flight into every day
      legacy = list.length > 0 && !list.every((d) => d && 'busiestHour' in d && 'farthest' in d);
      let rates = null;
      if (legacy) {
        rates = await api.get('/api/stats/rates', { params: { days }, signal: scope.signal, timeout: 30000 }).catch((e) => { if (e.aborted) throw e; return null; });
        if (my !== seq) return;
      }
      model = build(daily, rates, !legacy);
    } catch (e) {
      if (e.aborted || my !== seq) return;
      error = e;
    }
    render();
    if (model && legacy) fetchFarthest(my);
  }

  /** Join day totals with the busiest hour of each day (native: taken from the day itself). */
  function build(daily, rates, native) {
    const off = (daily?.utcOffsetSec || 0) * 1000;
    if (native) {
      const rows = (daily?.days || []).map((d) => ({ ...d, hour: d.busiestHour && fin(d.busiestHour.t) ? { t: d.busiestHour.t, aircraft: d.busiestHour.aircraft } : null }))
        .sort((a, b) => b.t - a.t);
      for (const r of rows) far.set(r.t, r.farthest && r.farthest.icao ? { row: r.farthest } : { none: true });
      return { rows, off, busiestDay: daily?.busiestDay || null, busiestHour: daily?.busiestHour || null, from: daily?.from, to: daily?.to };
    }
    const dayKey = (t) => { const d = new Date(t + off); return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}-${String(d.getUTCDate()).padStart(2, '0')}`; };
    const best = new Map();
    const s = rates?.series;
    if (s && Array.isArray(s.t) && Array.isArray(s.aircraft)) {
      const hourly = rates.resolution === 'hour';
      const buckets = new Map();      // local hour start -> max aircraft
      for (let i = 0; i < s.t.length; i++) {
        const v = s.aircraft[i];
        if (!fin(v)) continue;
        const t = hourly ? s.t[i] : Math.floor((s.t[i] + off) / HOUR) * HOUR - off;
        buckets.set(t, Math.max(buckets.get(t) ?? 0, v));
      }
      for (const [t, v] of buckets) {
        const k = dayKey(t);
        const b = best.get(k);
        if (!b || v > b.aircraft) best.set(k, { t, aircraft: v });
      }
    }
    const rows = (daily?.days || []).map((d) => ({ ...d, hour: best.get(d.date) || null })).sort((a, b) => b.t - a.t);
    return { rows, off, busiestDay: daily?.busiestDay || null, busiestHour: daily?.busiestHour || null, from: daily?.from, to: daily?.to };
  }

  async function fetchFarthest(my) {
    const queue = model.rows.filter((r) => fin(r.maxKm) && r.flights > 0).slice(0, FARTHEST_MAX_DAYS);
    const worker = async () => {
      while (queue.length && my === seq && scope.alive) {
        const r = queue.shift();
        try {
          const res = await api.get('/api/reports', { params: { from: r.t, to: r.t + DAY, sort: 'maxDistKm', desc: 1, limit: 1 }, signal: scope.signal, timeout: 20000 });
          far.set(r.t, res?.rows?.[0] ? { row: res.rows[0] } : { none: true });
        } catch (e) {
          if (e.aborted) return;
          far.set(r.t, { error: e });
        }
        if (my === seq) paintFar(r.t);
      }
    };
    await Promise.all([worker(), worker(), worker()]);
  }

  const farCells = new Map();
  function farCell(r) {
    const f = far.get(r.t);
    if (!fin(r.maxKm)) return units.DASH;
    const dist = h('span.num', units.dist(r.maxKm));
    if (!f) return h('span.rpd-far', dist, h('span.muted.rpd-far-id', '…'));
    if (!f.row) return h('span.rpd-far', dist);
    const x = f.row;
    // a /api/reports row has a flightId; the day's own `farthest` may not: list that day's flights by distance
    const href = x.flightId ? '#/reports/flight/' + x.flightId : `#/reports?from=${r.t}&to=${r.t + DAY}&sort=maxDistKm&desc=1`;
    const tip = [x.operator, x.type].filter(Boolean).join(' ') || (fin(x.km) ? `${x.icao} · ${units.dist(x.km)}` : null);
    return h('span.rpd-far', dist, h('a.rpd-far-id.num', { href, onclick: (e) => e.stopPropagation(), tip }, x.callsign || x.reg || x.icao));
  }
  function paintFar(t) {
    const td = farCells.get(t);
    const r = model?.rows.find((x) => x.t === t);
    if (td && r) ui.mount(td, farCell(r));
  }

  function hourText(hr, off) {
    if (!hr) return units.DASH;
    const d = new Date(hr.t + off);
    const hh = String(d.getUTCHours()).padStart(2, '0');
    return `${hh}:00 · ${units.int(hr.aircraft)}`;
  }

  function render() {
    farCells.clear();
    if (error) {
      const e = ui.error(error, { title: 'The summary could not be loaded', retry: load });
      ui.mount(chartHost, e);
      ui.clear(tableHost);
      return;
    }
    if (!model) {
      ui.mount(chartHost, ui.skeleton({ block: 220, lines: 0 }));
      ui.mount(tableHost, h('div.rpd-pad', ui.skeleton({ lines: 6 })));
      chartCard.setSub('Loading…');
      return;
    }
    const rows = model.rows;
    const withData = rows.filter((r) => r.flights > 0 || r.uniqueAircraft > 0);
    // tiles
    const sum = (k) => rows.reduce((a, r) => a + (fin(r[k]) ? r[k] : 0), 0);
    tiles.flights.set(units.int(sum('flights')));
    tiles.flights.setSub(`${units.int(sum('newAirframes'))} new airframes · ${units.int(withData.length)} ${withData.length === 1 ? 'day' : 'days'} with data`);
    const bd = model.busiestDay;
    tiles.day.set(bd ? bd.date : units.DASH);
    tiles.day.setSub(bd ? `${units.int(bd.uniqueAircraft)} aircraft · ${units.int(bd.flights)} flights` : 'not enough data yet');
    const bh = model.busiestHour;
    if (bh && fin(bh.t)) {
      const d = new Date(bh.t + model.off);
      tiles.hour.set(`${String(d.getUTCHours()).padStart(2, '0')}:00`);
      tiles.hour.setSub(`${units.int(bh.aircraft)} aircraft · ${units.date(bh.t + model.off)}`);
    } else { tiles.hour.set(units.DASH); tiles.hour.setSub('not enough data yet'); }
    const fr = rows.reduce((m, r) => (fin(r.maxKm) && (!m || r.maxKm > m.maxKm) ? r : m), null);
    tiles.far.set(fr ? units.format('dist', fr.maxKm, { unit: false, digits: 0 }) : units.DASH, units.label('dist'));
    tiles.far.setSub(fr ? `on ${fr.date}` : 'not enough data yet');

    if (!withData.length) {
      const empty = () => ui.empty({ icon: 'chart', title: 'Not enough data yet', text: 'Day totals appear after the first statistics roll-up (a few minutes after start). Come back later.' });
      ui.mount(chartHost, empty());
      ui.clear(tableHost);
      chartCard.setSub('');
      return;
    }
    chartCard.setSub(`${units.int(rows.length)} ${rows.length === 1 ? 'day' : 'days'} · aircraft, flights and farthest position`);

    // chart (oldest left)
    const asc = rows.slice().reverse();
    const el = h('div.chart.rpd-chart');
    ui.mount(chartHost, el);
    if (chart) chart.dispose();
    chart = ctx.charts.create(el);
    const C = ctx.charts.colors;
    const opt = ctx.charts.bar({
      x: asc.map((r) => r.date.slice(5)),
      series: [
        { name: 'Aircraft', data: asc.map((r) => r.uniqueAircraft ?? null), color: C.cyan },
        { name: 'Flights', data: asc.map((r) => r.flights ?? null), color: C.blue },
      ],
      legend: true, y2Name: units.label('dist'),
    });
    opt.series.push({ type: 'line', name: 'Farthest', yAxisIndex: 1, data: asc.map((r) => (fin(r.maxKm) ? Math.round(units.value('dist', r.maxKm)) : null)), color: C.amber, symbol: 'circle', symbolSize: 5, showSymbol: true, lineStyle: { width: 1.8 } });
    opt.tooltip = { ...opt.tooltip, valueFormatter: undefined, formatter: (ps) => {
      const list = Array.isArray(ps) ? ps : [ps];
      const r = asc[list[0]?.dataIndex];
      if (!r) return '';
      return `<b>${r.date}</b><br>${units.int(r.uniqueAircraft)} aircraft · ${units.int(r.flights)} flights<br>farthest ${units.dist(r.maxKm)}<br>busiest hour ${hourText(r.hour, model.off)}`;
    } };
    chart.set(opt);

    // table
    const COLS = [
      ['Date', (r) => h('span.num', r.date)],
      ['Aircraft', (r) => units.int(r.uniqueAircraft), 'right'],
      ['Flights', (r) => units.int(r.flights), 'right'],
      ['New', (r) => units.int(r.newAirframes), 'right', 'Airframes never seen before'],
      ['Peak', (r) => units.int(r.peakAircraft), 'right', 'Most aircraft at once'],
      ['Farthest', null, '', 'Farthest position of the day and the flight that reached it'],
      ['Busiest hour', (r) => hourText(r.hour, model.off), 'right', 'Station-local hour with the most aircraft'],
      ['Positions', (r) => units.si(r.positions), 'right'],
      ['Uptime', (r) => (fin(r.uptimeMin) ? units.pct(Math.min(100, (r.uptimeMin / 1440) * 100)) : units.DASH), 'right', 'Minutes with data / 24 h'],
    ];
    const tbody = h('tbody', rows.map((r) => {
      const tr = h('tr.rpb-row', { tabIndex: 0 }, COLS.map(([label, fn, align]) => {
        if (label === 'Farthest') { const td = h('td'); farCells.set(r.t, td); td.append(farCell(r)); return td; }
        return h('td.num' + (align ? '.' + align : ''), fn(r));
      }));
      const open = () => ctx.navigate(`reports?from=${r.t}&to=${r.t + DAY}`);
      tr.addEventListener('click', open);
      tr.addEventListener('keydown', (e) => { if (e.key === 'Enter') open(); });
      if (!(r.flights > 0)) tr.classList.add('is-empty');
      return tr;
    }));
    ui.mount(tableHost, h('div.table-wrap.rpb-table', h('table.table', h('thead', h('tr', COLS.map(([label, , align, tip]) => h('th' + (align ? '.' + align : ''), { tip: tip || null }, label)))), tbody)));
  }

  function downloadCsv() {
    if (!model?.rows?.length) { ui.toast.info('Nothing to download yet.'); return; }
    const esc = (v) => (v === null || v === undefined ? '' : /[",\n]/.test(String(v)) ? `"${String(v).replace(/"/g, '""')}"` : String(v));
    const lines = ['date,aircraft,flights,new_airframes,peak_aircraft,max_dist_km,farthest_flight_id,farthest_callsign,farthest_icao,busiest_hour_local,busiest_hour_aircraft,positions,uptime_min'];
    for (const r of model.rows.slice().reverse()) {
      const f = far.get(r.t)?.row;
      const hr = r.hour ? `${String(new Date(r.hour.t + model.off).getUTCHours()).padStart(2, '0')}:00` : '';
      lines.push([r.date, r.uniqueAircraft, r.flights, r.newAirframes, r.peakAircraft, fin(r.maxKm) ? r.maxKm.toFixed(1) : '', f?.flightId, f?.callsign, f?.icao, hr, r.hour?.aircraft, r.positions, r.uptimeMin].map(esc).join(','));
    }
    const blob = new Blob([lines.join('\r\n') + '\r\n'], { type: 'text/csv' });
    const a = h('a', { href: URL.createObjectURL(blob), download: `airview-daily-${model.rows[model.rows.length - 1].date}-${model.rows[0].date}.csv` });
    document.body.append(a);
    a.click();
    setTimeout(() => { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
  }

  scope.onCleanup(units.onChange(() => { if (model) render(); }));
  load();
}
