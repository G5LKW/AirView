// rf/channel.js - RF Lab "Channel": 1090 MHz occupancy over time, frames/s per downlink format, CRC / garble
// statistics, the Poisson overlap model against the observed failure rate, and burst seconds.

import { ok } from './common.js';

const CRC = [
  ['ok', 'CRC OK', '#34d399', 'Parity check passed (DF11 / DF17 / DF18)'],
  ['fixed', 'Corrected', '#22d3ee', 'A 1-bit error was repaired'],
  ['addrAccepted', 'Address/parity, known aircraft', '#3b8bff', 'DF0/4/5/16/20/21: the address hidden in the parity matches an aircraft already heard'],
  ['addrRejected', 'Address/parity, rejected', '#fbbf24', 'The parity gives an address nobody has transmitted: most likely a corrupted frame'],
  ['bad', 'CRC failed', '#ff4d5e', 'Parity check failed and could not be repaired'],
  ['unknown', 'Other', '#8494b3', 'Formats without a usable check'],
];

export default {
  id: 'channel',
  label: 'Channel',
  icon: 'chart',
  endpoints: {
    channel: { path: '/api/rf/channel', every: 5000 },
    history: { path: '/api/rf/history', every: 60000, params: { days: 7 } },
  },

  build(env) {
    const { h, ui, units, charts, C, f, panel, show, notEnough } = env;
    const prefs = ui.prefs('airview.rf.channel', { span: 'recent' });
    let span = prefs.get().span === 'hours' ? 'hours' : 'recent';
    let last = null;

    const P = {};
    const spanSeg = ui.segmented({
      options: [{ value: 'recent', label: '5 min', tip: 'Last five minutes, one point per second' }, { value: 'hours', label: '7 days', tip: 'Hourly averages' }],
      value: span, onChange: (v) => { span = v; prefs.set({ span: v }); paintOcc(); },
    });
    P.occ = panel({
      title: '1090 MHz channel occupancy', icon: 'chart', class: 'span-2', actions: spanSeg, height: 280,
      caption: 'Share of the time the frequency carries a reply you decoded (each reply occupies 64 or 120 µs), with the frame rate on the right axis and failed frames in red. Busy airspace near large airports reaches a few percent; the real load is higher, because replies too weak to decode also occupy the channel.',
    });
    P.df = panel({
      title: 'Frames per second by format', icon: 'layers', height: 280,
      caption: 'DF17 = ADS-B extended squitter (aircraft broadcasting on their own). DF11 = all-call replies and acquisition squitters. DF0/16 = ACAS (collision avoidance) chatter between aircraft. DF4/5/20/21 = answers to secondary radar interrogations - their share shows how much radar is around you.',
    });
    const crcKv = h('div');
    P.crc = panel({
      title: 'Decode quality', icon: 'check-circle', height: 220,
      caption: 'How frames passed the parity check. A few percent rejected or failed is normal (overlapping replies); much more points at interference, a too-high gain or a noisy power supply.',
    });
    P.crc.extra.append(crcKv);
    const modelKv = h('div');
    const burstHost = h('div');
    P.model = panel({
      title: 'Garble model versus observed', icon: 'bolt', height: 240, class: 'span-2',
      caption: 'Replies are 64–120 µs long and arrive at random, so some overlap and are lost - the Poisson model predicts how many at your frame rate (dashed). If the observed failure rate (solid) is far above the model, the extra errors come from something else: interference, noise or replies too weak to decode (FRUIT). "Implied rate" is the channel rate that would explain all failures by overlap alone.',
    });
    P.model.extra.append(modelKv, burstHost);

    // ------------------------------------------------------------------------------------------
    function paintOcc() {
      if (!last) return;
      if (span === 'hours') {
        show(P.occ, last.history, (d) => {
          const hours = (d && d.hours) || [];
          if (!hours.length) return { empty: ['Not enough data yet', 'Hourly values appear after the first full clock hour.', 'history'] };
          P.occ.chart.set(charts.line({
            series: [
              { name: 'Occupancy', data: hours.map((r) => [r.t, r.occupancyPct]), color: C.cyan, area: true },
              { name: 'Frames/s', data: hours.map((r) => [r.t, r.fps]), color: C.blue, y2: true },
            ],
            yName: '%', yFormat: (v) => units.num(v, 1), y2Name: 'frames/s', y2Format: (v) => units.num(v, 0), valueFormat: (v) => units.num(v, 2), zoom: true,
          }), { replace: true });
          return null;
        });
        return;
      }
      show(P.occ, last.channel, (c) => {
        const r = c.recent || {};
        const fr = r.frames || [];
        if (!fr.length || !fr.some((v) => v > 0)) return { empty: notEnough('The per-second series fills as frames arrive.') };
        let start = fr.findIndex((v) => v > 0);
        if (start < 0) start = 0;
        const t = (i) => r.t0 + i * 1000;
        const occ = [];
        const fps = [];
        const bad = [];
        for (let i = start; i < fr.length; i++) {
          occ.push([t(i), ok(r.occPct?.[i]) ? r.occPct[i] : null]);
          fps.push([t(i), fr[i]]);
          bad.push([t(i), r.bad?.[i] || 0]);
        }
        const opt = charts.line({
          series: [
            { name: 'Occupancy', data: occ, color: C.cyan, area: true },
            { name: 'Frames/s', data: fps, color: C.blue, y2: true, width: 1.2 },
          ],
          yName: '%', yMin: 0, yFormat: (v) => units.num(v, 1), y2Name: 'frames/s', y2Min: 0, y2Format: (v) => units.num(v, 0),
        });
        opt.series.push({ type: 'bar', name: 'Failed', data: bad, yAxisIndex: 1, color: C.red, barMaxWidth: 3 });
        opt.tooltip.formatter = (ps) => `${units.clock(ps[0].value[0])}<br>` + ps.map((p) => `${p.marker}${p.seriesName}: <b>${p.seriesName === 'Occupancy' ? f.pct(p.value[1], 2) : units.int(p.value[1])}</b>`).join('<br>');
        opt.animation = false;
        P.occ.chart.set(opt, { replace: true });
        return null;
      });
      const c = last.channel && last.channel.data;
      if (c) P.occ.card.setSub(`now ${f.pct(c.occupancyPct, 2)} · peak (10 min) ${f.pct(c.peakOccupancyPct, 2)} · mean reply ${f.us(c.meanReplyUs)}`);
    }

    function paintDf(c) {
      const list = (c.byDf || []).filter((d) => d.count > 0).sort((a, b) => (a.rate ?? 0) - (b.rate ?? 0));
      if (!list.length) return { empty: notEnough('No frames yet.') };
      const color = (df) => (df === 17 || df === 18 ? C.cyan : df === 11 ? C.teal : df === 0 || df === 16 ? C.violet : df >= 20 ? C.amber : C.blue);
      P.df.chart.set({
        animation: false,
        grid: { left: 8, right: 54, top: 8, bottom: 8, containLabel: true },
        tooltip: { trigger: 'item', confine: true, formatter: (p) => { const d = list[p.dataIndex]; return `${d.label}<br><b>${f.rate(d.rate)}</b> · ${units.int(d.count)} frames · ${f.frac(d.count / (c.frames || 1), 1)}`; } },
        xAxis: { type: 'value', name: 'frames/s', axisLabel: { formatter: (v) => units.num(v, 0) } },
        yAxis: { type: 'category', data: list.map((d) => 'DF' + d.df), axisLabel: { fontFamily: charts.theme.gauge.detail.fontFamily } },
        series: [{
          type: 'bar', data: list.map((d) => ({ value: d.rate ?? 0, itemStyle: { color: color(d.df) } })), barMaxWidth: 16, itemStyle: { borderRadius: [0, 2, 2, 0] },
          label: { show: true, position: 'right', color: C.text2, fontSize: 10.5, formatter: (p) => f.rate(p.value) },
        }],
      }, { replace: true });
      P.df.card.setSub(`${f.rate(c.fps)} total · ${units.int(c.frames)} frames in ${units.age(c.seconds)}`);
      return null;
    }

    function paintCrc(c) {
      const by = c.byCrc || {};
      const data = CRC.map(([k, name, color]) => ({ name, value: by[k] || 0, color })).filter((d) => d.value > 0);
      if (!data.length) return { empty: notEnough('No frames yet.') };
      const opt = charts.donut({ data, valueFormat: (v) => units.int(v), centerText: f.frac(c.failFrac, 1) });
      opt.legend.formatter = (name) => { const d = data.find((x) => x.name === name); return `${name}  ${f.frac(d.value / (c.frames || 1), 1)}`; };
      opt.legend.textStyle = { fontSize: 11 };
      P.crc.chart.set(opt, { replace: true });
      ui.mount(crcKv, ui.kv([
        ['Failed (bad + rejected)', f.frac(c.failFrac, 2), { tip: 'Shown in the middle of the ring' }],
        ['CRC failed', f.frac(c.crcBadFrac, 2)],
        ['Corrected', f.frac(c.fixedFrac, 2)],
        ['Address rejected', f.frac(c.rejectedFrac, 2), { tip: 'Share of address/parity frames whose address was never heard' }],
      ], { columns: 2 }));
      return null;
    }

    function paintModel(c) {
      const bins = (c.vsLoad || []).filter((b) => b.seconds > 0 && ok(b.meanFps));
      if (!c.frames) return { empty: notEnough('No frames yet.') };
      const pts = (k) => bins.map((b) => [b.meanFps, ok(b[k]) ? b[k] * 100 : null]);
      const opt = charts.line({
        xType: 'value', xName: 'frames/s', xFormat: (v) => units.num(v, 0),
        series: [
          { name: 'Observed', data: pts('failFrac'), color: C.red },
          { name: 'Overlap model', data: pts('modelFailFrac'), color: C.text2, dashed: true },
        ],
        yName: '% failed', yMin: 0, yFormat: (v) => units.num(v, 1), valueFormat: (v) => f.pct(v, 2),
      });
      opt.series.forEach((s) => { s.showSymbol = true; s.symbolSize = 6; });
      if (bins.length === 1) opt.xAxis.scale = true;
      P.model.chart.set(opt, { replace: true });
      const ex = c.excessFail;
      const verdict = !ok(ex) ? null : ex > 0.15 ? ['bad', 'Far more failures than overlapping replies explain: look for a local interferer (LTE, DME, switching power supplies) and consider a 1090 MHz band-pass filter.']
        : ex > 0.05 ? ['warn', 'Somewhat more failures than overlap explains: possible interference or many undecodable weak replies.']
          : ['ok', 'Failures match what overlapping replies explain - nothing beyond normal garble.'];
      ui.mount(modelKv, ui.kv([
        ['Observed failures', f.frac(c.failFrac, 2)],
        ['Overlap model', f.frac(c.overlapProb, 2), { tip: '1 − exp(−2 λ τ): chance that another reply overlaps a given one at the measured rate' }],
        ['Excess', ok(ex) ? (ex > 0 ? '+' : '') + f.frac(ex, 2) : units.DASH, { tip: 'Observed minus model: positive = errors from something other than overlap' }],
        ['Implied channel rate', f.rate(c.impliedRate), { tip: `Compared with ${f.rate(c.fps)} decoded: a much higher value means many replies you never decode (FRUIT)` }],
      ], { columns: 2 }), verdict ? env.finding(verdict[0], verdict[1]) : null);
      const bursts = c.bursts || [];
      ui.mount(burstHost, c.burstCount > 0 ? [ui.section(`Burst seconds (${units.int(c.burstCount)})`, 'alert'), ui.table({
        columns: [
          { key: 't', label: 'Time', render: (b) => units.clock(b.t) },
          { key: 'frames', label: 'Frames', align: 'right', num: true, render: (b) => units.int(b.frames) },
          { key: 'expected', label: 'Expected', align: 'right', num: true, render: (b) => units.num(b.expected, 0) },
          { key: 'badFrac', label: 'Failed', align: 'right', num: true, render: (b) => f.frac(b.badFrac, 1) },
        ],
        rows: bursts.slice(-10).reverse(),
      })] : null);
      return null;
    }

    return {
      el: h('div.grid.cols-2.rf-grid', P.occ.card, P.df.card, P.crc.card, P.model.card),
      render(state) {
        last = state;
        paintOcc();
        show(P.df, state.channel, paintDf);
        show(P.crc, state.channel, paintCrc);
        show(P.model, state.channel, paintModel);
      },
    };
  },
};
