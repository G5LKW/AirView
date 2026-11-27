// rf/prediction.js - RF Lab "Prediction skill": self-scored error of the tracking filter's 5/15/30/60 s position
// predictions against constant velocity and dead reckoning (/api/prediction/stats).

import { ok } from './common.js';

const PHASES = [
  { value: 'all', label: 'All', tip: 'Every scored prediction' },
  { value: 'straight', label: 'Straight', tip: 'Aircraft flying straight and level' },
  { value: 'turn', label: 'Turning', tip: 'Aircraft in a turn: where a smart filter should beat dead reckoning' },
  { value: 'vertical', label: 'Climb / descent', tip: 'Aircraft climbing or descending' },
];
const PREDICTORS = [
  ['imm', 'AirView filter (IMM)', '#22d3ee'],
  ['cv', 'Constant velocity', '#a78bfa'],
  ['dr', 'Dead reckoning', '#8494b3'],
];

export default {
  id: 'prediction',
  label: 'Prediction skill',
  icon: 'route',
  endpoints: {
    prediction: { path: '/api/prediction/stats', every: 15000 },
  },

  build(env) {
    const { h, ui, units, C, f, panel, show } = env;
    const prefs = ui.prefs('airview.rf.prediction', { phase: 'all' });
    let phase = PHASES.some((p) => p.value === prefs.get().phase) ? prefs.get().phase : 'all';
    let last = null;

    const P = {};
    const phaseSeg = ui.segmented({ options: PHASES, value: phase, onChange: (v) => { phase = v; prefs.set({ phase: v }); paint(); } });
    const tableHost = h('div.rf-tablewrap');
    P.skill = panel({
      title: 'Prediction error by horizon', icon: 'route', class: 'span-2', actions: phaseSeg, height: 300,
      caption: 'While an aircraft is heard, AirView keeps predicting where it will be 5, 15, 30 and 60 s later and scores each prediction when the real position arrives. Bars = median error of the tracking filter and two simple baselines; the line = skill, how much smaller the filter\'s error is than dead reckoning (straight-line extrapolation of the last report). Positive skill means the coasting positions on the map are better than a naive guess.',
    });
    P.skill.extra.append(tableHost);
    const kvHost = h('div');
    P.tracks = panel({
      title: 'Tracking filter', icon: 'target', content: kvHost,
      caption: 'Counters of the trajectory filter that drives coasting on the Live Map. Rejected positions are outliers (bad CPR decodes, jumps) the filter refused to accept.',
    });

    // ------------------------------------------------------------------------------------------
    function paintSkill(d) {
      const rows = (d.horizons || []).filter((r) => r.phase === phase && r.n > 0);
      if (!rows.length) return { empty: ['Not enough data yet', phase === 'all' ? 'Predictions are scored once aircraft have been tracked for at least the prediction horizon; the first numbers appear after a minute or two.' : 'No scored predictions in this flight phase yet - try "All".', 'route'] };
      const horizons = [...new Set(rows.map((r) => r.horizon))].sort((a, b) => a - b);
      const get = (hz, pred) => rows.find((r) => r.horizon === hz && r.predictor === pred);
      const skill = (d.skill || []).filter((s) => s.phase === phase);
      const errV = (km) => (ok(km) ? units.value('height', km * 1000) : null);
      const series = PREDICTORS.map(([k, name, color]) => ({
        type: 'bar', name, color, barMaxWidth: 26,
        data: horizons.map((hz) => errV(get(hz, k)?.medianKm)),
      }));
      series.push({
        type: 'line', name: 'Skill vs dead reckoning', yAxisIndex: 1, color: C.amber, symbol: 'circle', symbolSize: 7, showSymbol: true, lineStyle: { width: 2 },
        data: horizons.map((hz) => { const s = skill.find((x) => x.horizon === hz); return s && ok(s.skillPct) ? s.skillPct : null; }),
        markLine: { symbol: 'none', silent: true, label: { show: false }, lineStyle: { color: C.text4, type: 'dashed' }, data: [{ yAxis: 0 }] },
      });
      const hu = units.label('height');
      P.skill.chart.set({
        animation: false,
        grid: { left: 8, right: 8, top: 34, bottom: 8, containLabel: true },
        legend: { show: true, top: 0, left: 0, type: 'scroll' },
        tooltip: { trigger: 'axis', confine: true, axisPointer: { type: 'shadow' }, formatter: (ps) => {
          const hz = horizons[ps[0].dataIndex];
          return `${units.int(hz)} s ahead<br>` + ps.map((p) => `${p.marker}${p.seriesName}: <b>${p.seriesType === 'line' ? f.pct(p.value, 0) : f.err(units.toBase('height', p.value) / 1000)}</b>`).join('<br>');
        } },
        xAxis: { type: 'category', data: horizons.map((hz) => `${units.int(hz)} s`) },
        yAxis: [
          { type: 'value', name: `median error (${hu})`, axisLabel: { formatter: (v) => units.num(v, 0) } },
          { type: 'value', name: 'skill', splitLine: { show: false }, axisLabel: { formatter: (v) => units.pct(v, 0) } },
        ],
        series,
      }, { replace: true });
      ui.mount(tableHost, ui.table({
        columns: [
          { key: 'h', label: 'Horizon', render: (hz) => `${units.int(hz)} s` },
          { key: 'n', label: 'Scored', align: 'right', num: true, render: (hz) => units.int(get(hz, 'imm')?.n) },
          { key: 'med', label: 'Median', align: 'right', num: true, render: (hz) => f.err(get(hz, 'imm')?.medianKm) },
          { key: 'p90', label: 'p90', align: 'right', num: true, render: (hz) => f.err(get(hz, 'imm')?.p90Km) },
          { key: 'p95', label: 'p95', align: 'right', num: true, render: (hz) => f.err(get(hz, 'imm')?.p95Km) },
          { key: 'cross', label: 'Cross-track', align: 'right', num: true, tip: 'Median absolute error sideways to the flight path', render: (hz) => f.err(get(hz, 'imm')?.crossMedAbsKm) },
          { key: 'vert', label: 'Vertical', align: 'right', num: true, render: (hz) => units.alt(get(hz, 'imm')?.vertMedAbsFt) },
          { key: 'cal', label: '95 % calib.', align: 'right', num: true, tip: 'Share of real positions inside the predicted 95 % ellipse - ideally 95 %', render: (hz) => f.frac(get(hz, 'imm')?.calib95, 0) },
          { key: 'dr', label: 'Dead reck.', align: 'right', num: true, render: (hz) => f.err(get(hz, 'dr')?.medianKm) },
          { key: 'skill', label: 'Skill', align: 'right', num: true, render: (hz) => { const s = skill.find((x) => x.horizon === hz); const v = s?.skillPct; return h('span.rf-pcell' + (ok(v) && v < 0 ? '.low' : ''), ok(v) ? (v > 0 ? '+' : '') + f.pct(v, 0) : units.DASH); } },
        ],
        rows: horizons,
      }));
      return null;
    }

    function paintTracks(d) {
      const c = d.counters || {};
      ui.mount(kvHost, ui.kv([
        ['Tracks', units.int(d.tracks)],
        ['Ready to predict', units.int(d.ready)],
        ['Coasting now', units.int(d.coasting)],
        ['Positions accepted', units.int(c.positions)],
        ['Observations', units.int(c.observations)],
        ['Rejected positions', units.int(c.rejected)],
        ['Rejected velocities', units.int(c.velRejected)],
        ['Rejected altitudes', units.int(c.altRejected)],
        ['Out of order', units.int(c.outOfOrder)],
        ['Duplicates', units.int(c.duplicates)],
        ['Filter resets', units.int(c.resets)],
      ], { columns: 2 }));
      return null;
    }

    function paint() {
      if (!last) return;
      show(P.skill, last.prediction, paintSkill);
      show(P.tracks, last.prediction, paintTracks);
    }

    return {
      el: h('div.grid.cols-2.rf-grid', P.skill.card, P.tracks.card),
      render(state) { last = state; paint(); },
    };
  },
};
