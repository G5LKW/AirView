// rf/timing.js - RF Lab "Squitter timing": GPS-timestamped interval histograms against the 0.4-0.6 s spec window,
// even/odd alternation and the list of transponders whose timing looks non-compliant.

import { ok, histPoints, histTotal } from './common.js';

const TYPE_LABEL = { position: 'Position', velocity: 'Velocity', ident: 'Identification' };

export default {
  id: 'timing',
  label: 'Squitter timing',
  icon: 'timer',
  endpoints: {
    timing: { path: '/api/rf/timing', every: 15000 },
  },

  build(env) {
    const { h, ui, units, C, f, panel, show, notEnough } = env;
    const prefs = ui.prefs('airview.rf.timing', { type: 'position' });
    let type = prefs.get().type;
    let last = null;
    let ncT = null;

    const P = {};
    const typeSeg = ui.segmented({
      options: Object.entries(TYPE_LABEL).map(([value, label]) => ({ value, label })),
      value: type, onChange: (v) => { type = v; prefs.set({ type: v }); paintHist(); },
    });
    const histKv = h('div');
    P.hist = panel({
      title: 'Squitter interval histogram', icon: 'timer', class: 'span-2', actions: typeSeg, height: 300,
      caption: 'Time between two consecutive squitters of the same type from the same aircraft, measured with the receiver\'s GPS clock. The standard makes transponders jitter randomly inside the shaded window (0.4–0.6 s for position and velocity, 4.8–5.2 s for identification), so the first hump should be flat and fill the window. Further humps at twice and three times the interval are lost squitters, not transponder faults.',
    });
    P.hist.extra.append(histKv);
    const altKv = h('div');
    P.alt = panel({
      title: 'Even / odd alternation', icon: 'refresh', content: altKv,
      caption: 'Position squitters alternate between "even" and "odd" encodings, which a decoder needs as a pair. When one squitter is lost in between, the next two have the same parity. Both shares should be close to 100 %.',
    });
    const ncHost = h('div.rf-tablewrap');
    P.nc = panel({
      title: 'Non-compliant transponders', icon: 'alert', content: ncHost,
      caption: 'Aircraft whose squitter intervals fall outside what a compliant transmitter can produce, or whose jitter is not random. Usually an old or mis-configured transponder; occasionally a sign of a spoofed or simulated target.',
    });
    const noteEl = h('p.rf-note');

    // ------------------------------------------------------------------------------------------
    function unavailable(d) {
      return !d || !d.available;
    }
    const noGps = ['Needs GPS timestamps', 'Squitter timing is measured with the receiver\'s GPS clock (Radarcape / Beast with GPS time). The feed does not deliver GPS timestamps, so this analysis is not possible.', 'timer'];

    function paintHist() {
      if (!last) return;
      show(P.hist, last.timing, (d) => {
        if (unavailable(d)) return { empty: noGps };
        const t = (d.types || []).find((x) => x.type === type) || (d.types || [])[0];
        if (!t || !histTotal(t.hist)) return { empty: notEnough('Intervals are collected from aircraft heard continuously; give it a few minutes.') };
        const pts = histPoints(t.hist);
        let lastNz = pts.length - 1;
        while (lastNz > 0 && !pts[lastNz][1]) lastNz--;
        const nomHi = ok(t.nominalHi) ? t.nominalHi : pts[pts.length - 1][0];
        const xMax = Math.min(pts[lastNz][0] + t.hist.step, nomHi * 3.2);
        const xMin = Math.max(0, (ok(t.nominalLo) ? t.nominalLo : t.hist.lo) - (nomHi - (t.nominalLo || 0)) * 0.5);
        P.hist.chart.set({
          animation: false,
          grid: { left: 8, right: 14, top: 18, bottom: 8, containLabel: true },
          tooltip: { trigger: 'axis', confine: true, axisPointer: { type: 'shadow' }, formatter: (ps) => `${f.sec(ps[0].value[0] - t.hist.step / 2, 3)} … ${f.sec(ps[0].value[0] + t.hist.step / 2, 3)}<br><b>${units.int(ps[0].value[1])}</b> intervals` },
          dataZoom: [{ type: 'inside', throttle: 50 }],
          xAxis: { type: 'value', name: 'interval (s)', nameLocation: 'middle', nameGap: 24, min: xMin, max: xMax, axisLabel: { formatter: (v) => units.num(v, 2) } },
          yAxis: { type: 'value', name: 'intervals', minInterval: 1, axisLabel: { formatter: (v) => units.si(v) } },
          series: [{
            type: 'bar', data: pts, color: C.blue, barWidth: '92%',
            markArea: ok(t.nominalLo) && ok(t.nominalHi) ? { silent: true, itemStyle: { color: 'rgba(52,211,153,0.10)', borderColor: 'rgba(52,211,153,0.4)', borderWidth: 1 }, label: { color: C.green, fontSize: 10, position: 'insideTop' }, data: [[{ xAxis: t.nominalLo, name: 'spec window' }, { xAxis: t.nominalHi }]] } : undefined,
            markLine: ok(t.mean) ? { symbol: 'none', silent: true, label: { show: false }, lineStyle: { color: C.cyan }, data: [{ xAxis: t.mean }] } : undefined,
          }],
        }, { replace: true });
        ui.mount(histKv, h('div.rf-mp',
          ui.badge(t.uniformOk ? 'jitter uniform' : (ok(t.chi2) ? 'jitter not uniform' : 'too few intervals'), t.uniformOk ? 'green' : ok(t.chi2) ? 'amber' : 'other', { tip: 'χ² test of the single intervals against a flat distribution (10 sub-bins, 9 degrees of freedom)' }),
          ui.kv([
            ['Intervals', units.int(t.intervals)],
            ['Impossible intervals', f.frac(t.badFrac, 2), { tip: 'Shorter than a compliant transmitter can produce', class: (t.badFrac ?? 0) > 0.01 ? 'c-red' : '' }],
            ['Mean (in window)', f.sec(t.mean, 3)],
            ['Std. deviation', f.sec(t.std, 3), { tip: `A flat 0.4–0.6 s window gives ${f.sec(0.2 / Math.sqrt(12), 3)}` }],
            ['χ² (df 9)', units.num(t.chi2, 1)],
            ['Spec window', ok(t.nominalLo) ? `${units.num(t.nominalLo, 1)}–${f.sec(t.nominalHi, 1)}` : units.DASH],
          ], { columns: 2 })));
        return null;
      });
    }

    function paintAlt(d) {
      if (unavailable(d)) return { empty: noGps };
      if (!d.altPairs && !d.alt2Pairs) return { empty: notEnough('Needs consecutive position squitters.') };
      ui.mount(altKv, h('div',
        ui.kv([
          ['Consecutive pairs', units.int(d.altPairs)],
          ['…that alternate', f.frac(d.altRegularity, 1), { class: ok(d.altRegularity) && d.altRegularity < 0.95 ? 'c-red' : '' }],
          ['Pairs with one lost', units.int(d.alt2Pairs)],
          ['…with equal parity', f.frac(d.alt2Same, 1)],
        ], { columns: 2 })));
      return null;
    }

    function paintNc(d) {
      if (unavailable(d)) return { empty: noGps };
      const rows = d.nonCompliant || [];
      if (!rows.length) {
        ui.mount(ncHost, env.finding('ok', 'No transponder with suspicious squitter timing in the window.'));
        return null;
      }
      ncT = ui.table({
        columns: [
          { key: 'icao', label: 'ICAO', render: (r) => h('span.num', r.icao) },
          { key: 'intervals', label: 'Intervals', align: 'right', num: true, sort: true, render: (r) => units.int(r.intervals) },
          { key: 'badFrac', label: 'Bad', align: 'right', num: true, sort: true, sortDir: 'desc', render: (r) => f.frac(r.badFrac, 1) },
          { key: 'mean', label: 'Mean', align: 'right', num: true, render: (r) => f.sec(r.mean, 3) },
          { key: 'std', label: 'Std', align: 'right', num: true, render: (r) => f.sec(r.std, 3) },
          { key: 'reason', label: 'Why', render: (r) => h('span.rf-wrap-text', r.reason || '') },
        ],
        rows, sort: { key: 'badFrac', dir: 'desc' }, onRowClick: (r) => env.ctx.navigate('map/' + r.icao),
      });
      ui.mount(ncHost, ncT);
      return null;
    }

    return {
      el: h('div', h('div.grid.cols-2.rf-grid', P.hist.card, P.alt.card, P.nc.card), noteEl),
      render(state) {
        last = state;
        paintHist();
        show(P.alt, state.timing, paintAlt);
        show(P.nc, state.timing, paintNc);
        const d = state.timing && state.timing.data;
        ui.setText(noteEl, d && d.note ? d.note : '');
      },
    };
  },
};
