// rf/health.js - RF Lab overview: health score with scored items, advice, blocked sectors and the hourly history.

import { ok, statusOf, scoreStatus } from './common.js';
import { itemText } from './advice.js';

const METRICS = [
  { value: 'traffic', label: 'Traffic', tip: 'Frame rate and channel occupancy per hour' },
  { value: 'errors', label: 'Errors', tip: 'CRC failures and the overlap (garble) model per hour' },
  { value: 'detection', label: 'Detection', tip: 'Position reception probability and 90 % detection range per hour' },
  { value: 'health', label: 'Health', tip: 'Health score and peak GNSS interference index per hour' },
];

export default {
  id: 'health',
  label: 'Overview',
  icon: 'bolt',
  endpoints: {
    health: { path: '/api/rf/health', every: 10000 },
    history: { path: '/api/rf/history', every: 60000, params: { days: 7 } },
  },

  build(env) {
    const { h, ui, units, charts, C, f, panel, show } = env;
    const prefs = ui.prefs('airview.rf.history', { metric: 'traffic' });
    let metric = METRICS.some((m) => m.value === prefs.get().metric) ? prefs.get().metric : 'traffic';
    let last = null;

    const dialEl = h('div.chart.rf-dial');
    const itemsEl = h('div.rf-items');
    const P = {};
    P.health = panel({
      title: 'RF health', icon: 'bolt', accent: true, class: 'span-2',
      content: h('div.rf-score', dialEl, itemsEl),
      caption: 'One number for the radio side of the station: receiver gain, interference, how many squitters are decoded, gaps around the compass, position update rate, GNSS integrity of the fleet and transponder timing. Each line explains its score in plain words; grey lines are still collecting data.',
    });
    P.advice = panel({
      title: 'What to do', icon: 'info', content: h('div.rf-findings'),
      caption: 'Concrete suggestions derived from the scores. Change one thing at a time and give the statistics an hour to settle before you judge the result.',
    });
    P.blocked = panel({
      title: 'Weak sectors', icon: 'compass', content: h('div.rf-findings'),
      caption: 'Bearings where far fewer position squitters arrive than in the typical direction - usually a building, tree, mast or the yagi\'s back side.',
    });
    const metricSeg = ui.segmented({ options: METRICS, value: metric, onChange: (v) => { metric = v; prefs.set({ metric: v }); paintHistory(); } });
    P.history = panel({
      title: 'Hourly history', icon: 'history', class: 'span-2', actions: metricSeg, height: 280, sub: 'last 7 days',
      caption: 'One point per closed clock hour. A sudden step in the error rate or a drop in reception probability that does not follow the traffic usually means something changed in the receive chain (cable, connector, gain, a new interferer nearby).',
    });

    let dial = null;

    function paintHealth(hl) {
      const score = hl.score;
      const has = ok(score) && score >= 0;
      const st = statusOf(has ? scoreStatus(score) : 'unknown');
      if (!dial) dial = charts.create(dialEl);
      dial.set({
        series: [{
          type: 'gauge', startAngle: 220, endAngle: -40, min: 0, max: 100, radius: '84%', center: ['50%', '57%'],
          progress: { show: has, width: 14, roundCap: true, itemStyle: { color: { type: 'linear', x: 0, y: 1, x2: 1, y2: 0, colorStops: [{ offset: 0, color: '#1a4fd0' }, { offset: 1, color: st.color }] }, shadowColor: st.color, shadowBlur: 14 } },
          axisLine: { roundCap: true, lineStyle: { width: 14, color: [[1, 'rgba(120,165,255,0.12)']] } },
          axisTick: { show: false }, pointer: { show: false }, anchor: { show: false },
          splitLine: { distance: -22, length: 5, lineStyle: { color: C.line2, width: 1 } },
          axisLabel: { distance: -14, color: C.text4, fontSize: 9, formatter: (v) => (v % 50 === 0 ? units.int(v) : '') },
          title: { offsetCenter: [0, '36%'], color: st.color, fontSize: 12, fontWeight: 650 },
          detail: { valueAnimation: has, offsetCenter: [0, '-2%'], fontSize: 46, fontWeight: 500, color: '#fff', fontFamily: charts.theme.gauge.detail.fontFamily, formatter: (v) => (has ? units.int(v) : units.DASH) },
          data: [{ value: has ? score : 0, name: has ? String(hl.status || st.label).toUpperCase() : 'COLLECTING' }],
        }],
      }, { replace: true });
      ui.mount(itemsEl, (hl.items || []).map((it) => {
        const s = statusOf(it.status);
        const known = ok(it.score) && it.score >= 0;
        return h('div.rf-item' + (known ? '' : '.off'),
          h('div.rf-item-head', h('span.rf-item-label', it.label), ui.badge(known ? s.label : 'collecting', s.badge), h('span.num.rf-item-score', known ? units.int(it.score) : units.DASH)),
          env.scoreMeter(it.score), h('div.rf-item-text', itemText(it, units)));
      }));
    }

    function paintAdvice(hl) {
      // advice entries are sentences, or { text, code, data } on newer servers; numbers follow the display units
      const src = Array.isArray(hl.adviceItems) && hl.adviceItems.length ? hl.adviceItems : (hl.advice || []);
      const list = src.filter((a) => a && (typeof a === 'string' || a.text)).map((a) => itemText(a, units));
      if (!list.length) return { empty: ['No advice yet', 'Suggestions appear once enough frames have been analysed to judge the receive chain.', 'info'] };
      const healthy = (hl.score ?? -1) >= 80;
      ui.mount(P.advice.main, list.map((t) => env.finding(healthy ? 'ok' : (hl.status === 'bad' ? 'bad' : 'warn'), t)));
      return null;
    }

    function paintBlocked(hl) {
      const sec = hl.blockedSectors || [];
      const sectorsItem = (hl.items || []).find((i) => i.key === 'sectors');
      if (!sec.length) {
        if (sectorsItem && sectorsItem.score < 0) return { empty: ['Not enough data yet', 'Needs traffic from at least 12 bearing sectors to compare them.', 'compass'] };
        ui.mount(P.blocked.main, env.finding('ok', 'No bearing sector stands out: reception is even around the compass (relative to the traffic there).'));
        return null;
      }
      ui.mount(P.blocked.main, sec.map((s) => env.finding('warn',
        `${units.deg(s.fromDeg)}–${units.deg(s.toDeg)} (${units.cardinal((s.fromDeg + s.toDeg) / 2)}): position reception ${f.frac(s.pd, 0)} against a typical ${f.frac(s.medianPd, 0)}.`)));
      return null;
    }

    function historyOption(hours) {
      const T = (r) => r.t;
      const line = (name, get, o = {}) => ({ name, data: hours.map((r) => [T(r), get(r)]), ...o });
      const pct100 = (v) => (ok(v) ? v * 100 : null);
      switch (metric) {
        case 'errors':
          return charts.line({
            series: [
              line('CRC failed', (r) => pct100(r.crcBadFrac), { color: C.red }),
              line('Address rejected', (r) => pct100(r.rejectedFrac), { color: C.amber }),
              line('Overlap model', (r) => pct100(r.overlapProb), { color: C.text3, dashed: true }),
            ],
            yName: '%', yFormat: (v) => units.num(v, 1), valueFormat: (v) => f.pct(v, 2), zoom: true,
          });
        case 'detection':
          return charts.line({
            series: [
              line('Position Pd', (r) => pct100(r.pdPos), { color: C.cyan, area: true }),
              line('90 % range', (r) => units.value('dist', r.range90Km), { color: C.amber, y2: true }),
            ],
            yName: '%', yMin: 0, yMax: 100, yFormat: (v) => units.num(v, 0), y2Name: units.label('dist'), y2Format: (v) => units.num(v, 0), zoom: true,
            valueFormat: (v) => units.num(v, 1),
          });
        case 'health':
          return charts.line({
            series: [
              line('Health', (r) => (ok(r.health) && r.health >= 0 ? r.health : null), { color: C.green, area: true, step: 'middle' }),
              line('GNSS index (max)', (r) => r.gnssIndexMax, { color: C.violet }),
            ],
            yMin: 0, yMax: 100, yFormat: (v) => units.num(v, 0), valueFormat: (v) => units.num(v, 0), zoom: true,
          });
        default:
          return charts.line({
            series: [
              line('Frames/s', (r) => r.fps, { color: C.blue, area: true }),
              line('Occupancy %', (r) => r.occupancyPct, { color: C.cyan, y2: true }),
            ],
            yName: 'frames/s', yFormat: (v) => units.num(v, 0), y2Name: '%', y2Format: (v) => units.num(v, 1), valueFormat: (v) => units.num(v, 2), zoom: true,
          });
      }
    }

    function paintHistory() {
      if (!last) return;
      show(P.history, last.history, (d) => {
        const hours = (d && d.hours) || [];
        if (!hours.length) return { empty: ['Not enough data yet', 'The first point appears when the first full clock hour after start-up is over.', 'history'] };
        P.history.chart.set(historyOption(hours), { replace: true });
        return null;
      });
    }

    return {
      el: h('div.grid.cols-2.rf-grid', P.health.card, P.advice.card, P.blocked.card, P.history.card),
      render(state) {
        last = state;
        show(P.health, state.health, (hl) => { paintHealth(hl); return null; });
        show(P.advice, state.health, paintAdvice);
        show(P.blocked, state.health, paintBlocked);
        paintHistory();
      },
    };
  },
};
