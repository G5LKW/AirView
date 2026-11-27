// rf/detection.js - RF Lab "Detection": probability of detection (Pd) versus distance, elevation, bearing and
// signal level, per squitter type, position update intervals per range band and per-aircraft Pd.

import { ok, axisCentres, anyNum } from './common.js';

const SERIES = [['pos', 'Position'], ['vel', 'Velocity'], ['ident', 'Identification']];

export default {
  id: 'detection',
  label: 'Detection',
  icon: 'target',
  endpoints: {
    detection: { path: '/api/rf/detection', every: 10000 },
  },

  build(env) {
    const { h, ui, units, charts, C, f, panel, show, notEnough, pctAxis } = env;
    const colors = { pos: C.cyan, vel: C.blue, ident: C.violet };

    const P = {};
    const typeTiles = {
      position: ui.stat({ label: 'Position squitters', icon: 'pin', accent: 'cyan', tip: 'Airborne position messages: nominally 2 per second per aircraft' }),
      velocity: ui.stat({ label: 'Velocity squitters', icon: 'route', tip: 'Airborne velocity messages: nominally 2 per second' }),
      ident: ui.stat({ label: 'Identification', icon: 'plane', accent: 'violet', tip: 'Callsign / category messages: nominally one every 5 seconds' }),
      range: ui.stat({ label: 'Detection range', icon: 'radar', accent: 'amber', tip: 'Distance at which position reception falls below 90 % / 50 %' }),
    };
    P.types = panel({
      title: 'Reception probability', icon: 'target', class: 'span-2', content: h('div.grid.cols-4.rf-minis', typeTiles.position, typeTiles.velocity, typeTiles.ident, typeTiles.range),
      caption: 'Probability of detection (Pd) = squitters decoded ÷ squitters the aircraft actually transmitted, counted per aircraft over 10-second windows. 100 % would be every single message; 70–90 % close in is typical, because replies overlap and aircraft turn their antennas away.',
    });
    P.dist = panel({
      title: 'Pd versus distance', icon: 'radar', class: 'span-2', height: 300,
      caption: 'How reliably messages arrive as aircraft get farther away. The dashed horizontal lines mark 90 % and 50 %; the vertical markers show where position reception crosses them - your practical "solid" and "usable" range. Grey bars show how many messages were expected in each distance bin (few = less certain).',
    });
    P.elev = panel({
      title: 'Pd versus elevation', icon: 'forecast', height: 260,
      caption: 'Reception by the angle at which you see the aircraft above the horizon. Near 0° the signal grazes the ground and obstacles; a station with a clear horizon keeps a high Pd right down to the lowest band.',
    });
    P.bear = panel({
      title: 'Pd versus bearing', icon: 'compass', height: 300,
      caption: 'Position reception per 10° direction (outer ring = 100 %). Even all round is ideal; a notch that stays in one direction is an obstruction or the null of a directional antenna.',
    });
    P.rssi = panel({
      title: 'Pd versus signal level', icon: 'signal', height: 240,
      caption: 'Reception by the average signal level of the aircraft. The curve rising from 0 to near 100 % is your receiver\'s sensitivity: the steeper and the further left, the better.',
    });
    const gapTable = h('div.rf-tablewrap');
    P.gaps = panel({
      title: 'Position update intervals', icon: 'timer', height: 240, class: 'span-2',
      caption: 'Time between two consecutive positions of the same aircraft, per distance band. Aircraft send a position every 0.5 s, so p50 ≈ 0.5 s means almost nothing is lost; the p90 / p99 tails show the dropouts. "≤ 5 s" = share of time the latest position is no older than 5 s - what a map viewer experiences.',
    });
    P.gaps.extra.append(gapTable);
    const acTable = h('div.rf-tablewrap');
    let acT = null;
    let gapT = null;
    P.aircraft = panel({
      title: 'Aircraft now', icon: 'plane', content: acTable,
      caption: 'Reception probability of every active ADS-B aircraft since it was first heard. Click a row to show it on the map.',
    });

    // ------------------------------------------------------------------------------------------
    function paintTypes(d) {
      const types = d.types || [];
      if (!types.some((t) => (t.expected ?? 0) > 0)) return { empty: notEnough('Pd is counted from ADS-B aircraft with known positions; the first numbers appear after about a minute.') };
      for (const t of types) {
        const tile = typeTiles[t.type];
        if (!tile) continue;
        tile.set(ok(t.pd) ? units.num(t.pd * 100, 1) : null, ok(t.pd) ? '%' : '');
        tile.setSub(`${units.si(t.decoded)} of ${units.si(t.expected)} expected · ${f.rate(t.nominalRate)} nominal`);
      }
      typeTiles.range.set(ok(d.range90Km) ? units.format('dist', d.range90Km, { unit: false, digits: 0 }) : null, ok(d.range90Km) ? units.label('dist') : '');
      typeTiles.range.setSub(`90 % reliable · 50 % to ${ok(d.range50Km) ? units.dist(d.range50Km) : 'beyond the data'}`);
      return null;
    }

    function axisChart(ax, o) {
      const pos = ax?.pos || [];
      if (!anyNum(pos) && !anyNum(ax?.vel) && !anyNum(ax?.ident)) return null;
      const xs = o.x(ax);
      const series = SERIES.map(([k, name]) => ({
        type: 'line', name, color: colors[k], connectNulls: false, showSymbol: true, symbolSize: 4, smooth: 0.15,
        lineStyle: { width: k === 'pos' ? 2.2 : 1.4 }, z: 3,
        data: (ax[k] || []).map((v, i) => (ok(xs[i]) ? [o.cat ? i : xs[i], ok(v) ? v * 100 : null] : null)).filter(Boolean),
      }));
      series[0].markLine = {
        symbol: 'none', silent: true, label: { color: C.text3, fontSize: 10, formatter: '{b}', position: 'insideStartTop' },
        lineStyle: { color: C.text3, type: 'dashed' },
        data: [{ yAxis: 90, name: '90 %' }, { yAxis: 50, name: '50 %' }, ...(o.marks || [])],
      };
      if (o.expected) {
        series.push({ type: 'bar', name: 'expected', yAxisIndex: 1, color: 'rgba(132,148,179,0.22)', barWidth: '70%', z: 1, data: (ax.expected || []).map((v, i) => (ok(xs[i]) ? [o.cat ? i : xs[i], v || 0] : null)).filter(Boolean), tooltip: { valueFormatter: (v) => units.int(v) } });
      }
      return {
        animation: false,
        grid: { left: 8, right: o.expected ? 8 : 14, top: 34, bottom: 8, containLabel: true },
        legend: { show: true, top: 0, left: 0, data: SERIES.map((s) => s[1]) },
        tooltip: { trigger: 'axis', confine: true, formatter: (ps) => {
          const i = ps[0] && ps[0].dataIndex;
          const head = o.label(i, ps[0]);
          return head + '<br>' + ps.map((p) => `${p.marker}${p.seriesName}: <b>${p.seriesName === 'expected' ? units.int(p.value[1]) : f.pct(p.value[1], 0)}</b>`).join('<br>');
        } },
        xAxis: o.cat
          ? { type: 'category', data: o.catLabels, axisLabel: { rotate: o.catLabels.length > 10 ? 35 : 0 } }
          : { type: 'value', name: o.xName, nameLocation: 'middle', nameGap: 24, min: o.xMin, max: o.xMax, axisLabel: { formatter: o.xFormat } },
        yAxis: o.expected ? [pctAxis(), { type: 'value', name: 'expected', splitLine: { show: false }, axisLabel: { formatter: (v) => units.si(v) } }] : pctAxis(),
        series,
      };
    }

    function paintDist(d) {
      const ax = d.vsDistance || {};
      const centres = axisCentres(ax);
      const marks = [];
      if (ok(d.range90Km)) marks.push({ xAxis: units.value('dist', d.range90Km), name: '90 % range', lineStyle: { color: C.green, type: 'solid' } });
      if (ok(d.range50Km)) marks.push({ xAxis: units.value('dist', d.range50Km), name: '50 % range', lineStyle: { color: C.amber, type: 'solid' } });
      const lastIdx = (ax.expected || []).reduce((m, v, i) => (v > 0 ? i : m), -1);
      const opt = axisChart(ax, {
        x: () => centres.map((c) => units.value('dist', c)),
        xName: units.label('dist'), xFormat: (v) => units.num(v, 0), xMin: 0,
        xMax: lastIdx >= 0 && ok(ax.hi?.[lastIdx]) ? Math.ceil(units.value('dist', ax.hi[lastIdx])) : undefined,
        expected: true, marks,
        label: (i, p) => units.dist(units.toBase('dist', p.value[0])),
      });
      if (!opt) return { empty: notEnough('Needs aircraft positions and a receiver location; each distance bin needs enough expected squitters before it is shown.') };
      P.dist.chart.set(opt, { replace: true });
      P.dist.card.setSub(ok(d.range90Km) ? `90 % to ${units.dist(d.range90Km)} · 50 % to ${ok(d.range50Km) ? units.dist(d.range50Km) : units.DASH}` : '');
      return null;
    }

    function catAxis(ax, fmt) {
      const lo = ax?.lo || [];
      const hi = ax?.hi || [];
      return lo.map((l, i) => {
        if (!ok(l)) return `< ${fmt(hi[i])}`;
        if (!ok(hi[i])) return `> ${fmt(l)}`;
        return `${fmt(l)}`;
      });
    }

    function paintElev(d) {
      const ax = d.vsElev || {};
      const labels = catAxis(ax, (v) => units.deg(v, Math.abs(v) < 1 && v !== 0 ? 2 : 0));
      const opt = axisChart(ax, { cat: true, catLabels: labels, x: () => labels.map((_, i) => i), expected: true, label: (i) => {
        const lo = ax.lo[i];
        const hi = ax.hi[i];
        return !ok(lo) ? `below ${units.deg(hi, 1)}` : !ok(hi) ? `above ${units.deg(lo, 1)}` : `${units.num(lo, 2)}–${units.deg(hi, 2)}`;
      } });
      if (!opt) return { empty: notEnough('Needs aircraft positions, altitudes and a receiver location.') };
      P.elev.chart.set(opt, { replace: true });
      return null;
    }

    function paintBear(d) {
      const ax = d.vsBearing || {};
      const c = axisCentres(ax);
      const series = SERIES.slice(0, 2).map(([k, name]) => ({ name, color: colors[k], area: k === 'pos' ? 0.16 : false, width: k === 'pos' ? 2 : 1.2, data: (ax[k] || []).map((v, i) => (ok(v) && ok(c[i]) ? [v * 100, c[i]] : null)).filter(Boolean) }));
      if (series[0].data.length < 3) return { empty: notEnough('Needs traffic in at least a few directions around the station.') };
      const opt = charts.polar({ series, maxR: 100, minR: 0, rFormat: (v) => units.pct(v, 0), legend: true, tooltip: (p) => `${p.seriesName}<br>${units.bearing(p.value[1])}: <b>${f.pct(p.value[0], 0)}</b>` });
      opt.radiusAxis.interval = 25;
      opt.series.forEach((s) => { s.showSymbol = true; s.symbolSize = 3; });
      P.bear.chart.set(opt, { replace: true });
      return null;
    }

    function paintRssi(d) {
      const ax = d.vsRssi || {};
      const centres = axisCentres(ax);
      const opt = axisChart(ax, { x: () => centres, xName: 'dBFS', xFormat: (v) => units.num(v, 0), expected: false, label: (i, p) => `around ${f.dbfs(p.value[0], 0)}` });
      if (!opt) return { empty: notEnough('Needs ADS-B aircraft with a signal level.') };
      opt.xAxis.scale = true;
      P.rssi.chart.set(opt, { replace: true });
      return null;
    }

    function paintGaps(d) {
      const gaps = (d.gaps || []).filter((g) => g.n > 0);
      if (!gaps.length) return { empty: notEnough('Update intervals are measured between consecutive positions of the same aircraft.') };
      const x = gaps.map((g) => f.band(g.loKm, g.hiKm));
      const s = (name, k, color) => ({ type: 'bar', name, color, data: gaps.map((g) => (ok(g[k]) ? g[k] : null)) });
      P.gaps.chart.set({
        animation: false,
        grid: { left: 8, right: 14, top: 34, bottom: 8, containLabel: true },
        legend: { show: true, top: 0, left: 0 },
        tooltip: { trigger: 'axis', confine: true, axisPointer: { type: 'shadow' }, valueFormatter: (v) => f.sec(v) },
        xAxis: { type: 'category', data: x },
        yAxis: { type: 'value', name: 's', min: 0, axisLabel: { formatter: (v) => units.num(v, v < 10 ? 1 : 0) } },
        series: [s('median', 'p50', C.cyan), s('p90', 'p90', C.blue), s('p95', 'p95', C.violet), s('p99', 'p99', C.amber)],
      }, { replace: true });
      const tsec = (gaps[0].tSec || []);
      const key = tsec.join(',');
      if (gapT && gapT.key === key) { gapT.setRows(gaps); return null; }
      gapT = ui.table({
        columns: [
          { key: 'band', label: 'Distance', render: (g) => f.band(g.loKm, g.hiKm) },
          { key: 'n', label: 'Intervals', align: 'right', num: true, render: (g) => units.int(g.n) },
          { key: 'p50', label: 'Median', align: 'right', num: true, render: (g) => f.sec(g.p50) },
          { key: 'p90', label: 'p90', align: 'right', num: true, render: (g) => f.sec(g.p90) },
          { key: 'p99', label: 'p99', align: 'right', num: true, render: (g) => f.sec(g.p99) },
          ...tsec.map((t, j) => ({
            key: 't' + j, label: `≤ ${f.sec(t, 0)}`, align: 'right', num: true, tip: `Share of time the newest position is at most ${f.sec(t, 0)} old`,
            render: (g) => {
              const v = g.pUpdate?.[j];
              return h('span.rf-pcell' + (ok(v) && v < 0.9 ? '.low' : ''), f.frac(v, 1));
            },
          })),
        ],
        rows: gaps,
      });
      gapT.key = key;
      ui.mount(gapTable, gapT);
      return null;
    }

    function paintAircraft(d) {
      const rows = d.aircraft || [];
      if (!rows.length) return { empty: ['No aircraft right now', 'No ADS-B aircraft with enough messages at the moment.', 'plane'] };
      const pd = (v) => h('span.rf-pcell' + (ok(v) && v < 0.5 ? '.low' : ''), f.frac(v, 0));
      P.aircraft.card.setSub(`${units.int(rows.length)} aircraft`);
      if (acT) { acT.setRows(rows.slice(0, 200)); return null; }
      acT = ui.table({
        columns: [
          { key: 'icao', label: 'ICAO', sort: true, render: (r) => h('span.num', r.icao) },
          { key: 'distKm', label: 'Distance', align: 'right', num: true, sort: true, render: (r) => units.dist(r.distKm) },
          { key: 'pdPos', label: 'Position', align: 'right', num: true, sort: true, render: (r) => pd(r.pdPos) },
          { key: 'pdVel', label: 'Velocity', align: 'right', num: true, sort: true, render: (r) => pd(r.pdVel) },
          { key: 'pdIdent', label: 'Ident', align: 'right', num: true, sort: true, render: (r) => pd(r.pdIdent) },
          { key: 'rssi', label: 'Signal', align: 'right', num: true, sort: true, render: (r) => f.dbfs(r.rssi) },
        ],
        rows: rows.slice(0, 200), sort: { key: 'distKm', dir: 'asc' },
        onRowClick: (r) => env.ctx.navigate('map/' + r.icao),
      });
      ui.mount(acTable, acT);
      return null;
    }

    return {
      el: h('div.grid.cols-2.rf-grid', P.types.card, P.dist.card, P.elev.card, P.bear.card, P.gaps.card, P.rssi.card, P.aircraft.card),
      render(state) {
        const s = state.detection;
        show(P.types, s, paintTypes);
        show(P.dist, s, paintDist);
        show(P.elev, s, paintElev);
        show(P.bear, s, paintBear);
        show(P.rssi, s, paintRssi);
        show(P.gaps, s, paintGaps);
        show(P.aircraft, s, paintAircraft);
      },
    };
  },
};
